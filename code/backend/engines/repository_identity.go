package engines

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/models"
)

// DetectRepositorySignatures inspects only exact-root, engine-owned marker
// structures and does not authenticate or execute an engine. The listing of
// the vault folder is bounded by ctx (see ReadWithin).
func DetectRepositorySignatures(ctx context.Context, repo models.Repository) ([]string, error) {
	if repo.Connector != "fs" {
		return nil, fmt.Errorf("remote repository signatures require connector-backed root inspection")
	}
	entries, err := ReadWithin(ctx, func() ([]os.DirEntry, error) { return os.ReadDir(repo.Location) })
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return RepositorySignatures(names), nil
}

// RepositorySignatures returns the engines whose repository markers appear
// among the names listed at a vault root, sorted. Kopia is recognized by its
// format blob, which is "kopia.repository" on object storage and
// "kopia.repository.f" where Kopia stores blobs as files. Restic is recognized
// by "config" together with "keys": on object storage a Restic repository
// without snapshots has nothing else, because empty folders do not exist
// there. Both engines are returned when both are present.
func RepositorySignatures(names []string) []string {
	present := map[string]bool{}
	for _, name := range names {
		present[strings.ToLower(strings.TrimSpace(name))] = true
	}
	candidates := []string{}
	if present["kopia.repository"] || present["kopia.repository.f"] {
		candidates = append(candidates, KopiaID)
	}
	if present["config"] && present["keys"] {
		candidates = append(candidates, ResticID)
	}
	sort.Strings(candidates)
	return candidates
}

// RepositoryFingerprintContext is RepositoryFingerprint for callers that hold
// a vault lock or the Kopia initialization lock, or that are connecting a
// vault: the filesystem marker read is bounded by ctx (see ReadWithin). Other
// connectors read nothing locally and are computed directly.
func RepositoryFingerprintContext(ctx context.Context, repo models.Repository, validationOutput string) (string, error) {
	if repo.Connector != "fs" {
		return RepositoryFingerprint(repo, validationOutput)
	}
	return ReadWithin(ctx, func() (string, error) { return RepositoryFingerprint(repo, validationOutput) })
}

// RepositoryMissingContext is RepositoryMissing for the same callers. For a
// filesystem vault the check stats and lists the vault folder, so it is
// bounded by ctx; an error means ctx ended before the check finished and
// nothing is known about whether the repository is missing.
func RepositoryMissingContext(ctx context.Context, repo models.Repository, validationOutput string) (bool, error) {
	if repo.Connector != "fs" {
		return RepositoryMissing(repo, validationOutput), nil
	}
	return ReadWithin(ctx, func() (bool, error) { return RepositoryMissing(repo, validationOutput), nil })
}

// RepositoryFingerprint binds a recovery preview to native repository material.
// Exact-root configuration bytes are preferred; normalized validation output
// is the remote-repository fallback.
//
// For a filesystem vault it reads the vault path directly and can block
// indefinitely on a hung share, so code that holds a vault lock or serves a
// request should call RepositoryFingerprintContext instead.
func RepositoryFingerprint(repo models.Repository, validationOutput string) (string, error) {
	if repo.Connector == "fs" {
		markers := []string{}
		switch repo.Engine {
		case ResticID:
			markers = []string{"config", "CONFIG"}
		case KopiaID:
			markers = []string{"kopia.repository.f", "kopia.blobcfg.f"}
		}
		for _, name := range markers {
			path := filepath.Join(repo.Location, name)
			file, err := os.Open(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return "", err
			}
			data, copyErr := io.ReadAll(io.LimitReader(file, (8<<20)+1))
			closeErr := file.Close()
			if copyErr != nil {
				return "", copyErr
			}
			if closeErr != nil {
				return "", closeErr
			}
			if len(data) > 8<<20 {
				return "", fmt.Errorf("native repository identity marker exceeds the size limit")
			}
			if repo.Engine == KopiaID {
				return KopiaFormatUniqueID(data)
			}
			hash := sha256.New()
			_, _ = hash.Write(data)
			hash.Write([]byte("\x00" + repo.Engine))
			return fmt.Sprintf("%x", hash.Sum(nil)), nil
		}
	}
	if repo.Engine == KopiaID {
		if document, ok := kopiaStatusDocument(validationOutput); ok {
			return document["uniqueIDHex"].(string), nil
		}
		return "", fmt.Errorf("kopia native repository identity is unavailable")
	}
	normalized := strings.Join(strings.Fields(validationOutput), " ")
	if normalized == "" {
		return "", fmt.Errorf("native repository identity is unavailable")
	}
	hash := sha256.Sum256([]byte(repo.Engine + "\x00" + normalized))
	return fmt.Sprintf("%x", hash[:]), nil
}

// kopiaStatusDocument finds the `kopia repository status --json` document in
// a validation output. Validation returns the status JSON as one line, after
// any output of the connect that came before it, so the whole output is tried
// first and then each line that looks like a JSON object, last line first.
// The document must carry a well-formed native repository ID, so another
// JSON line can't be mistaken for the status.
func kopiaStatusDocument(output string) (map[string]any, bool) {
	candidates := []string{strings.TrimSpace(output)}
	lines := strings.Split(output, "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		line := strings.TrimSpace(lines[index])
		if strings.HasPrefix(line, "{") && strings.HasSuffix(line, "}") {
			candidates = append(candidates, line)
		}
	}
	for _, candidate := range candidates {
		value, err := decodeUniqueJSON([]byte(candidate))
		document, ok := value.(map[string]any)
		uniqueID, _ := document["uniqueIDHex"].(string)
		decoded, decodeErr := hex.DecodeString(uniqueID)
		if err == nil && ok && decodeErr == nil && len(decoded) == sha256.Size && uniqueID == strings.ToLower(uniqueID) {
			return document, true
		}
	}
	return nil, false
}

// KopiaReportedECCOverheadPercent returns the error correction overhead in
// the `kopia repository status --json` document of a validation output, 0
// when error correction is off. It reads the status the caller's validation
// already ran; it never runs Kopia.
//
// Pinned Kopia reports contentFormat.ecc ("REED-SOLOMON-CRC32") and
// contentFormat.eccOverheadPercent only for a repository created with error
// correction, and leaves both out otherwise. Kopia itself applies error
// correction only when the overhead is above 0, whatever ecc says, so only
// the overhead is read: absent or 0 is off. The value is kept as Kopia
// reported it, including percentages the new-vault form doesn't offer, which
// a repository created outside Replicaro can have.
func KopiaReportedECCOverheadPercent(output string) (int, error) {
	document, ok := kopiaStatusDocument(output)
	if !ok {
		return 0, fmt.Errorf("kopia repository status is unavailable")
	}
	format, ok := document["contentFormat"].(map[string]any)
	if !ok {
		return 0, fmt.Errorf("kopia repository status has no content format")
	}
	raw, present := format["eccOverheadPercent"]
	if !present {
		return 0, nil
	}
	number, ok := raw.(json.Number)
	if !ok {
		return 0, fmt.Errorf("kopia repository status has an invalid error correction overhead")
	}
	percent, err := strconv.ParseInt(number.String(), 10, 32)
	if err != nil {
		return 0, fmt.Errorf("kopia repository status has an invalid error correction overhead")
	}
	if percent < 0 {
		return 0, nil
	}
	return int(percent), nil
}

// KopiaFormatUniqueID returns the repository's stable ID, as lowercase hex,
// from kopia.repository bytes. It is the same value Kopia reports as
// uniqueIDHex and Replicaro stores as the native repository ID, and a
// password change leaves it unchanged.
func KopiaFormatUniqueID(data []byte) (string, error) {
	var format struct {
		UniqueID string `json:"uniqueID"`
	}
	if err := json.Unmarshal(data, &format); err != nil || strings.TrimSpace(format.UniqueID) == "" {
		return "", fmt.Errorf("kopia native repository identity is invalid")
	}
	decoded, err := base64.StdEncoding.DecodeString(format.UniqueID)
	if err != nil || len(decoded) != sha256.Size {
		return "", fmt.Errorf("kopia native repository identity is invalid")
	}
	return hex.EncodeToString(decoded), nil
}

func CleanupRepositoryArtifacts(repo models.Repository) error {
	if (repo.Engine != KopiaID && repo.Engine != ResticID) || strings.TrimSpace(repo.ID) == "" {
		return nil
	}
	root, err := appdata.Directory(filepath.Join("engines", repo.Engine, repo.ID))
	if err != nil {
		return err
	}
	return os.RemoveAll(root)
}

// RepositoryArtifactStage holds a same-filesystem quarantine for local engine
// configuration. A stage must be either restored or finalized by its caller.
type RepositoryArtifactStage struct {
	manifest RepositoryArtifactManifest
	parent   string
}

type RepositoryArtifactAction string

const (
	RepositoryArtifactDelete             RepositoryArtifactAction = "delete"
	RepositoryArtifactCredentialRotation RepositoryArtifactAction = "credential_rotation"
	repositoryArtifactManifestVersion                             = 1
	repositoryArtifactPrefix                                      = ".replicaro-quarantine-"
	repositoryArtifactManifestSuffix                              = ".manifest.json"
)

// RepositoryArtifactManifest deliberately contains no paths or credentials.
// Paths are reconstructed only after every identity-bearing field is validated.
type RepositoryArtifactManifest struct {
	Version          int                      `json:"version"`
	Engine           string                   `json:"engine"`
	RepositoryID     string                   `json:"repositoryId"`
	Action           RepositoryArtifactAction `json:"action"`
	Quarantine       string                   `json:"quarantine"`
	OldOptionsSHA256 string                   `json:"oldOptionsSha256"`
	// Committed is set by Finalize, which only runs once the vault change is
	// saved. A manifest kept because the old copy couldn't be removed outlives
	// the database state that showed the change was saved: the same vault can
	// be added back under the same ID, or its credentials changed back to the
	// old values, and reading the database again would then call the change
	// unsaved and move the old copy back over the current one. Manifests
	// written before this field existed lack it and are still classified from
	// the database; older versions ignore it.
	Committed bool `json:"committed,omitempty"`
}

// RepositoryOptionsDigest is the narrow state comparison used by startup
// recovery. encoding/json sorts map keys, producing a stable canonical form.
func RepositoryOptionsDigest(repo models.Repository) (string, error) {
	encoded, err := json.Marshal(struct {
		Engine           string            `json:"engine"`
		RepositoryID     string            `json:"repositoryId"`
		ConnectorOptions map[string]string `json:"connectorOptions"`
	}{repo.Engine, repo.ID, repo.ConnectorOptions})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}

func StageRepositoryArtifacts(repo models.Repository, action RepositoryArtifactAction) (*RepositoryArtifactStage, error) {
	stage := &RepositoryArtifactStage{}
	if (repo.Engine != KopiaID && repo.Engine != ResticID) || strings.TrimSpace(repo.ID) == "" {
		return stage, nil
	}
	if !validRepositoryArtifactID(repo.ID) {
		return nil, fmt.Errorf("prepare local engine artifact staging: invalid repository identity")
	}
	if action != RepositoryArtifactDelete && action != RepositoryArtifactCredentialRotation {
		return nil, fmt.Errorf("prepare local engine artifact staging: invalid action")
	}
	parent, err := appdata.Directory(filepath.Join("engines", repo.Engine))
	if err != nil {
		return nil, fmt.Errorf("prepare local engine artifact staging: %w", err)
	}
	original := filepath.Join(parent, repo.ID)
	if _, err := os.Stat(original); os.IsNotExist(err) {
		return stage, nil
	} else if err != nil {
		return nil, fmt.Errorf("inspect local engine artifacts: %w", err)
	}
	tokenFile, err := os.CreateTemp(parent, repositoryArtifactPrefix+"*")
	if err != nil {
		return nil, fmt.Errorf("reserve local engine artifact quarantine: %w", err)
	}
	quarantine := tokenFile.Name()
	if closeErr := tokenFile.Close(); closeErr != nil {
		_ = os.Remove(quarantine)
		return nil, fmt.Errorf("reserve local engine artifact quarantine: %w", closeErr)
	}
	if err := os.Remove(quarantine); err != nil {
		return nil, fmt.Errorf("prepare local engine artifact quarantine: %w", err)
	}
	digest, err := RepositoryOptionsDigest(repo)
	if err != nil {
		return nil, fmt.Errorf("digest local engine artifact state: %w", err)
	}
	manifest := RepositoryArtifactManifest{
		Version: repositoryArtifactManifestVersion, Engine: repo.Engine, RepositoryID: repo.ID,
		Action: action, Quarantine: filepath.Base(quarantine), OldOptionsSHA256: digest,
	}
	stage = &RepositoryArtifactStage{manifest: manifest, parent: parent}
	if err := writeRepositoryArtifactManifest(stage.manifestPath(), manifest); err != nil {
		return nil, fmt.Errorf("persist local engine artifact stage: %w", err)
	}
	if err := os.Rename(original, quarantine); err != nil {
		_ = removeAndSync(stage.manifestPath(), parent)
		return nil, fmt.Errorf("stage local engine artifacts: %w", err)
	}
	if err := syncDirectory(parent); err != nil {
		return nil, errors.Join(fmt.Errorf("sync local engine artifact stage: %w", err), stage.Restore())
	}
	return stage, nil
}

func (s *RepositoryArtifactStage) Restore() error {
	if s == nil || s.parent == "" {
		return nil
	}
	original, quarantine := s.paths()
	originalExists, err := pathExists(original)
	if err != nil {
		return fmt.Errorf("restore local engine artifacts: %w", err)
	}
	quarantineExists, err := pathExists(quarantine)
	if err != nil {
		return fmt.Errorf("restore local engine artifacts: %w", err)
	}
	if originalExists && quarantineExists {
		return fmt.Errorf("restore local engine artifacts: destination is no longer empty")
	}
	if !originalExists && !quarantineExists {
		return fmt.Errorf("restore local engine artifacts: staged artifacts are missing")
	}
	if !originalExists {
		if err := os.Rename(quarantine, original); err != nil {
			return fmt.Errorf("restore local engine artifacts: %w", err)
		}
		if err := syncDirectory(s.parent); err != nil {
			return fmt.Errorf("restore local engine artifacts: %w", err)
		}
	}
	if err := removeAndSync(s.manifestPath(), s.parent); err != nil {
		return fmt.Errorf("remove local engine artifact manifest: %w", err)
	}
	s.parent = ""
	return nil
}

// Finalize discards the quarantined old copy. Callers run it only after the
// change is saved, so it first records that in the manifest; if the removal
// then fails, the next start finishes it without consulting the database.
func (s *RepositoryArtifactStage) Finalize() error {
	if s == nil || s.parent == "" {
		return nil
	}
	var markErr error
	if !s.manifest.Committed {
		committed := s.manifest
		committed.Committed = true
		// The temporary file is only left behind by a crash during an
		// earlier write of this manifest, and the write refuses to reuse it.
		_ = os.Remove(s.manifestPath() + ".tmp")
		if err := writeRepositoryArtifactManifest(s.manifestPath(), committed); err != nil {
			// Still try the removal: if it succeeds the manifest goes too and
			// the flag no longer matters. Only if both fail does the next
			// start fall back to classifying the stage from the database.
			markErr = fmt.Errorf("record saved local engine artifact change: %w", err)
		} else {
			s.manifest = committed
		}
	}
	_, quarantine := s.paths()
	if err := os.RemoveAll(quarantine); err != nil {
		return errors.Join(fmt.Errorf("remove quarantined local engine artifacts: %w", err), markErr)
	}
	if err := syncDirectory(s.parent); err != nil {
		return fmt.Errorf("sync quarantined local engine artifacts: %w", err)
	}
	if err := removeAndSync(s.manifestPath(), s.parent); err != nil {
		return fmt.Errorf("remove local engine artifact manifest: %w", err)
	}
	s.parent = ""
	return nil
}

func (s *RepositoryArtifactStage) Manifest() RepositoryArtifactManifest { return s.manifest }

func (s *RepositoryArtifactStage) paths() (string, string) {
	return filepath.Join(s.parent, s.manifest.RepositoryID), filepath.Join(s.parent, s.manifest.Quarantine)
}

func (s *RepositoryArtifactStage) manifestPath() string {
	return filepath.Join(s.parent, s.manifest.Quarantine+repositoryArtifactManifestSuffix)
}

// PendingRepositoryArtifactStages validates all durable manifests. Anonymous
// legacy quarantines are reported separately and never removed automatically.
func PendingRepositoryArtifactStages() ([]*RepositoryArtifactStage, []string, error) {
	var stages []*RepositoryArtifactStage
	var legacy []string
	var errs []error
	for _, engine := range []string{ResticID, KopiaID} {
		parent, err := appdata.Directory(filepath.Join("engines", engine))
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect %s artifact recovery: %w", engine, err))
			continue
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			errs = append(errs, fmt.Errorf("inspect %s artifact recovery: %w", engine, err))
			continue
		}
		manifests := map[string]bool{}
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), repositoryArtifactPrefix) && strings.HasSuffix(entry.Name(), repositoryArtifactManifestSuffix) {
				base := strings.TrimSuffix(entry.Name(), repositoryArtifactManifestSuffix)
				manifests[base] = true
				stage, readErr := readRepositoryArtifactManifest(parent, entry.Name(), engine)
				if readErr != nil {
					errs = append(errs, readErr)
					continue
				}
				stages = append(stages, stage)
			}
		}
		for _, entry := range entries {
			if entry.IsDir() && strings.HasPrefix(entry.Name(), repositoryArtifactPrefix) && !manifests[entry.Name()] {
				legacy = append(legacy, engine+":"+entry.Name())
			}
		}
	}
	return stages, legacy, errors.Join(errs...)
}

func readRepositoryArtifactManifest(parent, name, expectedEngine string) (*RepositoryArtifactStage, error) {
	if filepath.Base(name) != name {
		return nil, fmt.Errorf("invalid local engine artifact recovery manifest name")
	}
	data, err := os.ReadFile(filepath.Join(parent, name))
	if err != nil {
		return nil, fmt.Errorf("read local engine artifact recovery manifest: %w", err)
	}
	var manifest RepositoryArtifactManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("malformed local engine artifact recovery manifest")
	}
	if manifest.Version != repositoryArtifactManifestVersion || manifest.Engine != expectedEngine ||
		(manifest.Action != RepositoryArtifactDelete && manifest.Action != RepositoryArtifactCredentialRotation) ||
		!validRepositoryArtifactID(manifest.RepositoryID) || !validQuarantineBasename(manifest.Quarantine) ||
		len(manifest.OldOptionsSHA256) != sha256.Size*2 || strings.TrimSuffix(name, repositoryArtifactManifestSuffix) != manifest.Quarantine {
		return nil, fmt.Errorf("invalid local engine artifact recovery manifest")
	}
	if _, err := hex.DecodeString(manifest.OldOptionsSHA256); err != nil {
		return nil, fmt.Errorf("invalid local engine artifact recovery manifest")
	}
	return &RepositoryArtifactStage{manifest: manifest, parent: parent}, nil
}

func validRepositoryArtifactID(id string) bool {
	return id != "" && id != "." && id != ".." && filepath.Base(id) == id && !strings.ContainsAny(id, `/\\`) && !strings.HasPrefix(id, ".")
}

func validQuarantineBasename(name string) bool {
	if filepath.Base(name) != name || !strings.HasPrefix(name, repositoryArtifactPrefix) {
		return false
	}
	token := strings.TrimPrefix(name, repositoryArtifactPrefix)
	if len(token) < 6 || strings.ContainsAny(token, `/\\.`) {
		return false
	}
	return true
}

func writeRepositoryArtifactManifest(path string, manifest RepositoryArtifactManifest) error {
	data, err := json.Marshal(manifest)
	if err != nil {
		return err
	}
	temporary := path + ".tmp"
	file, err := os.OpenFile(temporary, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	cleanup = false
	if err := os.Chmod(path, 0o600); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func pathExists(path string) (bool, error) {
	_, err := os.Lstat(path)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

func removeAndSync(path, parent string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return syncDirectory(parent)
}

// CleanupPreviewArtifacts removes everything a preview or credential check
// left under its throwaway vault ID: the local engine configuration and the
// engine cache. Callers only pass an ID they generated for that one check,
// never a saved vault's ID, because this also deletes the cache.
func CleanupPreviewArtifacts(repo models.Repository) {
	_ = CleanupRepositoryArtifacts(repo)
	_ = RemoveRepositoryCaches(repo)
}

// RemoveRepositoryCaches deletes the engine cache folder Replicaro gives one
// vault ID: cache/engines/restic/<id>, which Restic gets as RESTIC_CACHE_DIR,
// or cache/engines/kopia/<id>, which holds Kopia's cache and log folders
// (KOPIA_CACHE_DIRECTORY and KOPIA_LOG_DIR). On macOS that folder is under
// ~/Library/Caches/Replicaro instead (see appdata.CacheDirectory). The engines
// rebuild a cache on their own, so this only frees space; it's called once the
// ID belongs to no saved vault, and must not be called for a vault that is
// still in use. Vault and preview IDs are canonical UUIDs, so anything else is
// ignored, which also keeps the removal inside the engine's cache folder.
func RemoveRepositoryCaches(repo models.Repository) error {
	if (repo.Engine != KopiaID && repo.Engine != ResticID) || !validRepositoryUUID(repo.ID) {
		return nil
	}
	parent, err := appdata.CacheDirectory(filepath.Join("engines", repo.Engine))
	if err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(parent, repo.ID))
}

// ReconcileRepositoryCaches removes the engine cache folders that belong to
// no vault ID in ownerIDs: caches older versions left behind when a vault was
// removed or a creation cancelled, and a cache whose removal a crash cut
// short. It runs once at startup, before any worker or request can open a
// vault, so no engine is using a folder it removes. Only canonical-UUID
// folder names are considered, which leaves anything else in the cache
// folder alone. It returns how many folders it removed.
func ReconcileRepositoryCaches(ownerIDs map[string]bool) (int, error) {
	var result error
	removed := 0
	for _, engine := range []string{ResticID, KopiaID} {
		parent, err := appdata.CacheDirectory(filepath.Join("engines", engine))
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		entries, err := os.ReadDir(parent)
		if err != nil {
			result = errors.Join(result, err)
			continue
		}
		for _, entry := range entries {
			if id := entry.Name(); validRepositoryUUID(id) && !ownerIDs[id] {
				if err := RemoveRepositoryCaches(models.Repository{ID: id, Engine: engine}); err != nil {
					result = errors.Join(result, err)
				} else {
					removed++
				}
			}
		}
	}
	return removed, result
}
