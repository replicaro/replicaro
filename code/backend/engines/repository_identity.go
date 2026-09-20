package engines

import (
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
	"strings"

	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/models"
)

// DetectRepositorySignatures inspects only exact-root, engine-owned marker
// structures and does not authenticate or execute an engine.
func DetectRepositorySignatures(repo models.Repository) ([]string, error) {
	repo = repo.RuntimeView()
	if repo.Connector != "fs" {
		return nil, fmt.Errorf("remote repository signatures require connector-backed root inspection")
	}
	entries, err := os.ReadDir(repo.Location)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, entry := range entries {
		names[strings.ToLower(entry.Name())] = true
	}
	candidates := []string{}
	if names["kopia.repository.f"] || names["kopia.blobcfg.f"] || names["kopia.maintenance.f"] {
		candidates = append(candidates, KopiaID)
	}
	resticStructure := names["config"] && names["data"] && names["index"] && names["keys"] && names["snapshots"]
	if resticStructure {
		candidates = append(candidates, ResticID)
	}
	sort.Strings(candidates)
	return candidates, nil
}

// RepositoryFingerprint binds a recovery preview to native repository material.
// Exact-root configuration bytes are preferred; normalized validation output
// is the remote-repository fallback.
func RepositoryFingerprint(repo models.Repository, validationOutput string) (string, error) {
	repo = repo.RuntimeView()
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
			hash := sha256.New()
			_, _ = hash.Write(data)
			hash.Write([]byte("\x00" + repo.Engine))
			return fmt.Sprintf("%x", hash.Sum(nil)), nil
		}
	}
	if repo.Engine == KopiaID {
		candidates := []string{strings.TrimSpace(validationOutput)}
		lines := strings.Split(validationOutput, "\n")
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
				return uniqueID, nil
			}
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

func (s *RepositoryArtifactStage) Finalize() error {
	if s == nil || s.parent == "" {
		return nil
	}
	_, quarantine := s.paths()
	if err := os.RemoveAll(quarantine); err != nil {
		return fmt.Errorf("remove quarantined local engine artifacts: %w", err)
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
	if err := appdata.SecurePath(path, false); err != nil {
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

func CleanupPreviewArtifacts(repo models.Repository) {
	_ = CleanupRepositoryArtifacts(repo)
}
