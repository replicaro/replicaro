package engines

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
)

// ErrColdStorageArchivedObject identifies the exact pinned-Restic ordinary-S3
// failure that has a Replicaro connection remedy. Native output remains
// separate and available to callers for operation diagnostics.
var ErrColdStorageArchivedObject = errors.New(models.ColdStorageArchivedObjectHelp)

type resticEngine struct {
	path, statusError string
	custom            bool
}

func newRestic(custom string) Engine {
	path, statusError := detectBinary(ResticID, custom, binaryName("restic"))
	return &resticEngine{path: path, statusError: statusError, custom: custom != ""}
}
func (e *resticEngine) ID() string { return ResticID }
func (e *resticEngine) version(ctx context.Context) (string, error) {
	out, err := e.run(ctx, nil, []string{"version"}, 2*time.Minute)
	return strings.TrimSpace(out), err
}
func (e *resticEngine) Descriptor() Descriptor {
	return e.descriptor(context.Background())
}
func (e *resticEngine) descriptor(ctx context.Context) Descriptor {
	d := Descriptor{ID: ResticID, Name: "Restic", Compression: "Zstandard", Encryption: "AES-256", Installed: e.path != "", Path: e.path, Operations: operationNames(), JobSettings: []string{"additionalOptions"}, Providers: storageProviderDescriptors(ResticID), Capabilities: capabilitiesFor(ResticID)}
	if e.statusError != "" {
		d.Error = e.statusError
	}
	if e.path != "" {
		if version, err := e.version(ctx); err == nil {
			parsedVersion, parseErr := parseEngineVersion(ResticID, version)
			if parseErr != nil {
				d.Installed = false
				d.Error = "Restic version output is not parseable"
			} else {
				d.Version = parsedVersion.String()
				if e.custom && !semanticVersionEqual(version, "0.19.1") {
					d.CompatibilityWarning = "Custom Restic version is outside the tested 0.19.1 range."
				}
			}
		} else {
			d.Installed = false
			d.Error = "Restic executable is not working: " + err.Error()
		}
	}
	return d
}

func resticLiveOutputEnabled(_ models.Repository, args []string) bool {
	// Investigation of the pinned engine commands found no evidence that ordinary
	// output exposes stored passwords or keys. Restic therefore owns that output
	// for every connector; Replicaro redacts only the public support export.
	return eligibleResticLiveCommand(args)
}

func (e *resticEngine) Info(ctx context.Context, repo models.Repository) (output string, err error) {
	out, err := e.runRepo(ctx, repo, []string{"cat", "config"}, command.NoTotalDeadline)
	if err != nil {
		return out, err
	}
	var config struct {
		Version     int    `json:"version"`
		Compression string `json:"compression"`
	}
	if err := json.Unmarshal([]byte(out), &config); err != nil {
		return out, fmt.Errorf("parse restic repository config: %w", err)
	}
	if config.Version != 2 {
		return out, fmt.Errorf("restic repository format v2 with compression is required")
	}
	if strings.EqualFold(config.Compression, "off") {
		return out, fmt.Errorf("restic repository compression is disabled")
	}
	return out, nil
}
func (e *resticEngine) Create(ctx context.Context, repo models.Repository) (output string, err error) {
	// Vault creation is a requested ordinary native command. Its init output
	// can reassure the user while the HTTP response is still pending; private
	// config and identity probes retain their separate suppressed boundaries.
	ctx = command.ContextWithLiveOutputEnabled(ctx)
	out, err := e.runRepo(ctx, repo, []string{"init", "--repository-version", "2", "--compression", "auto"}, command.NoTotalDeadline)
	return out, err
}

func (e *resticEngine) unlock(ctx context.Context, repo models.Repository) (output string, err error) {
	// Deliberately delegate the complete lock decision to Restic. In particular,
	// do not add --remove-all or inspect, classify, or remove lock objects here.
	out, err := e.runRepo(ctx, repo, []string{"unlock"}, command.NoTotalDeadline)
	return out, err
}
func (e *resticEngine) Backup(ctx context.Context, repo models.Repository, source string, options BackupOptions) (snapshot models.Snapshot, output string, err error) {
	extra, err := normalizedOptions(options.Settings)
	if err != nil {
		return models.Snapshot{}, "", err
	}
	if err := validateOptions(ResticID, extra); err != nil {
		return models.Snapshot{}, "", err
	}
	_, _, readConcurrency, err := resticConcurrency(repo)
	if err != nil {
		return models.Snapshot{}, "", resticPreparationFailure(err)
	}
	args := []string{"backup", "--json", "--compression", "auto", "--read-concurrency", strconv.Itoa(readConcurrency)}
	if options.Tag != "" {
		args = append(args, "--tag", options.Tag)
	}
	if options.OwnerProfileID != "" {
		args = append(args, "--tag", "replicaro-profile:"+options.OwnerProfileID)
	}
	if options.OwnerJobID != "" {
		args = append(args, "--tag", "replicaro-job:"+options.OwnerJobID)
	}
	if options.OwnerProfileID != "" && options.OwnerJobID != "" {
		for _, tag := range backupPresentationTags(repo) {
			args = append(args, "--tag", tag)
		}
	}
	for _, exclude := range options.Excludes {
		if exclude != "" {
			args = append(args, "--exclude", exclude)
		}
	}
	args = append(args, extra...)
	args = append(args, source)
	capture, out, runErr := e.runRepoCommandCaptured(ctx, repo, nil, args, command.NoTotalDeadline)
	if capture != nil {
		defer capture.Close()
	}
	if runErr != nil {
		if partial, confirmed := resticPartialSnapshot(capture, runErr); confirmed {
			return partial, out, &BackupSourceReadFailure{SnapshotID: partial.ID, Err: runErr}
		}
		return models.Snapshot{}, out, runErr
	}
	// Stdout is the exact native machine-readable stream. Successful stderr is
	// carried only in the returned diagnostic body and never enters this parser.
	stdout, openErr := capture.OpenStdout()
	if openErr != nil {
		return models.Snapshot{}, out, resticCommandFailure(&command.OutputProcessingFailure{Err: openErr}, true)
	}
	snapshot, parseErr := parseResticBackupReader(stdout)
	_ = stdout.Close()
	if parseErr != nil {
		return models.Snapshot{}, out, resticCommandFailure(&command.OutputProcessingFailure{Err: parseErr}, true)
	}
	// The backup summary is sufficient for bounded Job Size accounting but not
	// for marker/source presentation. The pre-reserved cache generation remains
	// dirty until the access-driven native listing rebuilds authoritative headers.
	return snapshot, out, nil
}

func (e *resticEngine) ApplyRetention(ctx context.Context, repo models.Repository, jobUUID string, policy RetentionPolicy) (output string, err error) {
	if policy.Latest <= 0 {
		return "", fmt.Errorf("Restic retention requires an active keep-latest policy")
	}
	if parsed, parseErr := uuid.Parse(jobUUID); parseErr != nil || parsed == uuid.Nil || parsed.String() != jobUUID {
		return "", fmt.Errorf("Restic retention job identity is invalid")
	}
	args := []string{"forget", "--json", "--tag", "replicaro-job:" + jobUUID,
		"--group-by=", "--keep-last", strconv.Itoa(policy.Latest)}
	for _, tier := range []struct {
		flag  string
		value *int
	}{
		{"--keep-hourly", policy.Hourly}, {"--keep-daily", policy.Daily},
		{"--keep-weekly", policy.Weekly}, {"--keep-monthly", policy.Monthly},
		{"--keep-yearly", policy.Yearly},
	} {
		if tier.value != nil {
			if *tier.value <= 0 {
				return "", fmt.Errorf("Restic retention tier %s must be positive when enabled", tier.flag)
			}
			args = append(args, tier.flag, strconv.Itoa(*tier.value))
		}
	}
	// Empty grouping is deliberate: the stable job tag is one native cohort
	// across profile takeover, host changes, and source relocation.
	out, err := e.runRepo(ctx, repo, args, command.NoTotalDeadline)
	return out, err
}
func (e *resticEngine) ListSnapshots(ctx context.Context, repo models.Repository) (snapshots []models.Snapshot, output string, err error) {
	return e.listSnapshots(ctx, repo)
}

type resticSnapshotRow struct {
	ID       string   `json:"id"`
	Tree     string   `json:"tree"`
	Time     string   `json:"time"`
	Hostname string   `json:"hostname"`
	Paths    []string `json:"paths"`
	Tags     []string `json:"tags"`
	Size     struct {
		TotalBytesProcessed int64           `json:"total_bytes_processed"`
		TotalFilesProcessed json.RawMessage `json:"total_files_processed"`
	} `json:"summary"`
}

func decodeResticSnapshotRows(data []byte) ([]resticSnapshotRow, error) {
	return decodeResticSnapshotRowsReader(bytes.NewReader(data))
}

func decodeResticSnapshotRowsReader(reader io.Reader) ([]resticSnapshotRow, error) {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, fmt.Errorf("snapshot inventory is not a JSON array")
	}
	var rows []resticSnapshotRow
	for decoder.More() {
		value, ambiguousCount, err := readUniqueJSONValueAtPath(decoder, nil, []string{"summary", "total_files_processed"})
		if err != nil {
			return nil, err
		}
		encoded, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		var row resticSnapshotRow
		if err := json.Unmarshal(encoded, &row); err != nil {
			return nil, err
		}
		if ambiguousCount {
			row.Size.TotalFilesProcessed = nil
		}
		rows = append(rows, row)
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim(']') {
		return nil, fmt.Errorf("snapshot inventory has an incomplete JSON array")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("snapshot inventory has trailing JSON")
	}
	return rows, nil
}

func (e *resticEngine) listSnapshots(ctx context.Context, repo models.Repository, ids ...string) ([]models.Snapshot, string, error) {
	result, _, output, err := e.listSnapshotRows(ctx, repo, ids...)
	return result, output, err
}

func (e *resticEngine) listSnapshotRows(ctx context.Context, repo models.Repository, ids ...string) ([]models.Snapshot, []resticSnapshotRow, string, error) {
	args := []string{"snapshots", "--json"}
	args = append(args, ids...)
	capture, out, err := e.runRepoCommandCaptured(ctx, repo, nil, args, command.NoTotalDeadline)
	if capture != nil {
		defer capture.Close()
	}
	if err != nil {
		return nil, nil, out, err
	}
	stdout, openErr := capture.OpenStdout()
	if openErr != nil {
		failure := &command.OutputProcessingFailure{Err: fmt.Errorf("parse restic snapshots: %w", openErr)}
		return nil, nil, out, resticCommandFailure(failure, true)
	}
	rows, err := decodeResticSnapshotRowsReader(stdout)
	_ = stdout.Close()
	if err != nil {
		failure := &command.OutputProcessingFailure{Err: fmt.Errorf("parse restic snapshots: %w", err)}
		return nil, nil, out, resticCommandFailure(failure, true)
	}
	outputFailure := func(err error) error {
		return resticCommandFailure(&command.OutputProcessingFailure{Err: err}, true)
	}
	result := make([]models.Snapshot, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		if strings.TrimSpace(row.ID) == "" || strings.TrimSpace(row.Time) == "" || len(row.Paths) == 0 {
			return nil, nil, out, outputFailure(fmt.Errorf("parse restic snapshots: snapshot is missing id, time, or source"))
		}
		if !resticSnapshotIDPattern.MatchString(row.ID) {
			return nil, nil, out, outputFailure(fmt.Errorf("parse restic snapshots: snapshot id %q is not a full canonical ID", row.ID))
		}
		parsedTime, parseErr := time.Parse(time.RFC3339Nano, row.Time)
		if parseErr != nil {
			return nil, nil, out, outputFailure(fmt.Errorf("parse restic snapshots: snapshot %q has malformed time", row.ID))
		}
		roots := make([]models.SnapshotSourceRoot, 0, len(row.Paths))
		rootSeen := map[string]bool{}
		for _, path := range row.Paths {
			if path == "" {
				return nil, nil, out, outputFailure(fmt.Errorf("parse restic snapshots: snapshot %q has an empty source path", row.ID))
			}
			if !rootSeen[path] {
				rootSeen[path] = true
				roots = append(roots, models.WithSnapshotNativeRootIdentity(models.SnapshotSourceRoot{Path: path, Host: row.Hostname}))
			}
		}
		source := row.Paths[0]
		if seen[row.ID] {
			return nil, nil, out, outputFailure(fmt.Errorf("parse restic snapshots: duplicate snapshot id %q", row.ID))
		}
		seen[row.ID] = true
		result = append(result, models.SnapshotWithTags(models.Snapshot{ID: row.ID, Timestamp: parsedTime.UTC().Format(time.RFC3339Nano), Source: source, SourceRoots: roots, NativeSourceHost: row.Hostname, TotalFileCount: optionalNativeFileCount(row.Size.TotalFilesProcessed), Size: formatBytes(row.Size.TotalBytesProcessed)}, row.Tags))
	}
	return result, rows, out, nil
}

func (e *resticEngine) ListPath(ctx context.Context, repo models.Repository, id, path string) ([]models.SnapshotEntry, string, error) {
	return e.listPath(ctx, repo, id, path, false)
}

// This is an indexing convention, not profile visibility or retention authority.
// A deleted job keeps its native source layout. Profile markers are evaluated
// separately by the ordinary presentation classifier.
func resticHasJobTag(tags []string) bool {
	count := 0
	for _, tag := range tags {
		if tag != "replicaro-job" && !strings.HasPrefix(tag, models.SnapshotOwnershipMarkerPrefix) {
			continue
		}
		value := strings.TrimPrefix(tag, models.SnapshotOwnershipMarkerPrefix)
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return false
		}
		count++
	}
	return count == 1
}
func (e *resticEngine) ListPathRecursive(ctx context.Context, repo models.Repository, id, path string) ([]models.SnapshotEntry, string, error) {
	return e.listPath(ctx, repo, id, path, true)
}
func (e *resticEngine) listPath(ctx context.Context, repo models.Repository, id, path string, recursive bool) (entries []models.SnapshotEntry, output string, err error) {
	source, sourceOutput, sourceErr := e.snapshotSource(ctx, repo, id)
	if sourceErr != nil {
		return nil, sourceOutput, sourceErr
	}
	return e.listPathFromSource(ctx, repo, id, source, path, recursive)
}

func (e *resticEngine) listPathFromSource(ctx context.Context, repo models.Repository, id, source, path string, recursive bool) (entries []models.SnapshotEntry, output string, err error) {
	commandSource, commandSourceErr := resticSnapshotTreePath(source)
	if commandSourceErr != nil {
		return nil, "", commandSourceErr
	}
	storedPath := commandSource
	requestedPath := ""
	if path != "" {
		var ok bool
		storedPath, ok = resticStoredPath(commandSource, path)
		if !ok {
			return nil, "", fmt.Errorf("restic path %q is outside snapshot source", path)
		}
		requestedPath, ok = normalizeResticEntryPath(normalizeResticPath(source), storedPath)
		if !ok {
			return nil, "", fmt.Errorf("restic path %q is outside snapshot source", path)
		}
	}
	args := []string{"ls", "--json", id}
	if source != "/" || path != "" {
		args = append(args, resticPathFilter(storedPath))
	}
	if recursive {
		args = append(args, "--recursive")
	}
	capture, out, err := e.runRepoCommandCaptured(ctx, repo, nil, args, command.NoTotalDeadline)
	if capture != nil {
		defer capture.Close()
	}
	if err != nil {
		return nil, out, err
	}
	stdout, openErr := capture.OpenStdout()
	if openErr != nil {
		failure := &command.OutputProcessingFailure{Err: fmt.Errorf("parse restic listing: %w", openErr)}
		return nil, out, resticCommandFailure(failure, true)
	}
	entries, parseErr := parseResticDirectoryEntriesReader(stdout, id, source, requestedPath, recursive)
	_ = stdout.Close()
	if parseErr != nil {
		parseErr = resticCommandFailure(&command.OutputProcessingFailure{Err: parseErr}, true)
	}
	return entries, out, parseErr
}

type resticSnapshotIndexSession struct {
	engine    *resticEngine
	repo      models.Repository
	snapshots []SnapshotIndexItem
}

type resticSnapshotIndexReference struct {
	Sources []string
	Tree    string
}

func (s *resticSnapshotIndexSession) Snapshots() []SnapshotIndexItem { return s.snapshots }
func (s *resticSnapshotIndexSession) Close() error                   { return nil }
func (s *resticSnapshotIndexSession) ListPathRecursive(ctx context.Context, snapshot SnapshotIndexItem, path string) ([]models.SnapshotEntry, string, error) {
	reference, ok := snapshot.NativeReference.(resticSnapshotIndexReference)
	if !ok || len(reference.Sources) == 0 {
		return nil, "", fmt.Errorf("restic snapshot %q has no reusable source reference", snapshot.Snapshot.ID)
	}
	var entries []models.SnapshotEntry
	var outputs []string
	for _, source := range reference.Sources {
		if source == "" {
			return nil, strings.Join(outputs, "\n"), fmt.Errorf("restic snapshot %q has an empty reusable source reference", snapshot.Snapshot.ID)
		}
		rootEntries, output, err := s.engine.listPathFromSource(ctx, s.repo, snapshot.Snapshot.ID, source, path, true)
		outputs = append(outputs, output)
		if err != nil {
			return nil, strings.Join(outputs, "\n"), err
		}
		for index := range rootEntries {
			rootEntries[index].SourceRoot = source
		}
		entries = append(entries, rootEntries...)
	}
	return entries, strings.Join(outputs, "\n"), nil
}

func (e *resticEngine) BeginSnapshotIndex(ctx context.Context, repo models.Repository) (SnapshotIndexSession, string, error) {
	snapshots, rows, output, err := e.listSnapshotRows(ctx, repo)
	if err != nil {
		return nil, output, err
	}
	items := make([]SnapshotIndexItem, 0, len(snapshots))
	for index, snapshot := range snapshots {
		if !resticHasJobTag(snapshot.Tags) {
			// Index native imports beneath their actual archive root. Keep the
			// native header roots returned by ListSnapshots intact for adoption.
			snapshot.SourceRoots = []models.SnapshotSourceRoot{models.WithSnapshotNativeRootIdentity(models.SnapshotSourceRoot{Path: "/", Host: snapshot.NativeSourceHost})}
		}
		sources := make([]string, 0, len(snapshot.SourceRoots))
		for _, root := range snapshot.SourceRoots {
			sources = append(sources, root.Path)
		}
		if len(sources) == 0 {
			sources = append(sources, snapshot.Source)
		}
		tree := rows[index].Tree
		if tree != "" && !resticSnapshotIDPattern.MatchString(tree) {
			return nil, output, fmt.Errorf("parse restic snapshots: snapshot %q has invalid tree identity", snapshot.ID)
		}
		identity := ""
		if tree != "" {
			identityBytes, marshalErr := json.Marshal(struct {
				Tree    string   `json:"tree"`
				Sources []string `json:"sources"`
			}{Tree: tree, Sources: append([]string(nil), rows[index].Paths...)})
			if marshalErr != nil {
				return nil, output, fmt.Errorf("encode restic content identity: %w", marshalErr)
			}
			identity = string(identityBytes)
		}
		items = append(items, SnapshotIndexItem{
			Snapshot: snapshot, NativeContentIdentity: identity,
			NativeReference: resticSnapshotIndexReference{Sources: sources, Tree: tree},
		})
	}
	return &resticSnapshotIndexSession{engine: e, repo: repo, snapshots: items}, output, nil
}
func (e *resticEngine) Restore(ctx context.Context, repo models.Repository, id string, options RestoreOptions) (output string, err error) {
	if err := ValidateSnapshotIDArgument(id); err != nil {
		return "", resticPreparationFailure(err)
	}
	selection, err := validateRestoreOptions(options)
	if err != nil {
		return "", err
	}
	if options.OriginalLocation {
		return "", fmt.Errorf("restic original-location restore is unsupported")
	}
	restoreID := id
	args := []string{"restore"}
	if selection != "" {
		source := options.NativeRoot.Path
		if source == "" {
			return "", resticPreparationFailure(fmt.Errorf("restic selected restore requires an exact native source root"))
		}
		var sourceErr error
		restoreID, sourceErr = resticRestoreSnapshotID(id, source)
		if sourceErr != nil {
			return "", resticPreparationFailure(sourceErr)
		}
		args = append(args, restoreID, "--target", options.Destination, "--include", resticLiteralIncludePath(selection))
	} else if options.ExactSourceRoot {
		if options.NativeRoot.Path == "" {
			return "", resticPreparationFailure(fmt.Errorf("restic exact-source restore requires an authoritative native source root"))
		}
		if !strings.HasPrefix(options.NativeRoot.Path, `\\`) {
			return "", resticPreparationFailure(fmt.Errorf("restic exact-source restore requires a Windows UNC source root"))
		}
		restoreID, err = resticRestoreSnapshotID(id, options.NativeRoot.Path)
		if err != nil {
			return "", resticPreparationFailure(err)
		}
		args = append(args, restoreID, "--target", options.Destination)
	} else {
		args = append(args, restoreID, "--target", options.Destination)
	}
	switch options.ConflictMode {
	case "always":
		args = append(args, "--overwrite", "always")
	case "never":
		args = append(args, "--overwrite", "never")
	default:
		return "", fmt.Errorf("unsupported Restic conflict mode %q", options.ConflictMode)
	}
	out, err := e.runRepo(ctx, repo, args, command.NoTotalDeadline)
	return out, err
}

func (e *resticEngine) snapshotSource(ctx context.Context, repo models.Repository, id string) (string, string, error) {
	capture, out, err := e.runRepoCommandCaptured(ctx, repo, nil, []string{"snapshots", "--json", id}, command.NoTotalDeadline)
	if capture != nil {
		defer capture.Close()
	}
	if err != nil {
		return "", out, resticPreparationFailure(err)
	}
	stdout, openErr := capture.OpenStdout()
	if openErr != nil {
		failure := &command.OutputProcessingFailure{Err: fmt.Errorf("parse restic snapshot sources: %w", openErr)}
		return "", out, resticCommandFailure(failure, true)
	}
	source, err := parseResticSnapshotSourceReader(stdout, id)
	_ = stdout.Close()
	if err != nil {
		return "", out, resticCommandFailure(&command.OutputProcessingFailure{Err: err}, true)
	}
	return source, out, nil
}

func parseResticSnapshotSource(out, id string) (string, error) {
	return parseResticSnapshotSourceReader(strings.NewReader(out), id)
}

func parseResticSnapshotSourceReader(reader io.Reader, id string) (string, error) {
	var rows []struct {
		ID    string   `json:"id"`
		Paths []string `json:"paths"`
		Tags  []string `json:"tags"`
	}
	decoder := json.NewDecoder(reader)
	if err := decoder.Decode(&rows); err != nil {
		return "", fmt.Errorf("parse restic snapshot sources: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return "", fmt.Errorf("parse restic snapshot sources: trailing JSON")
		}
		return "", fmt.Errorf("parse restic snapshot sources: trailing data: %w", err)
	}
	seen := map[string]bool{}
	for _, row := range rows {
		if strings.TrimSpace(row.ID) == "" || seen[row.ID] {
			return "", fmt.Errorf("restic snapshot source lookup returned a missing or duplicate ID")
		}
		seen[row.ID] = true
	}
	if len(rows) == 1 && rows[0].ID == id && len(rows[0].Paths) > 0 && !resticHasJobTag(rows[0].Tags) {
		for _, source := range rows[0].Paths {
			if source == "" {
				return "", fmt.Errorf("restic snapshot %q has an empty source path", id)
			}
		}
		return "/", nil
	}
	if len(rows) == 1 && rows[0].ID == id && len(rows[0].Paths) == 1 && rows[0].Paths[0] != "" {
		return rows[0].Paths[0], nil
	}
	if len(rows) == 1 && rows[0].ID == id && len(rows[0].Paths) != 1 {
		return "", fmt.Errorf("restic snapshot %q is an unsupported multi-root snapshot (expected exactly one source path)", id)
	}
	return "", fmt.Errorf("restic snapshot %q has no recorded source path", id)
}

func resticRestoreSnapshotID(id, source string) (string, error) {
	// Restic subfolder selectors address its archive tree, not the spelling in
	// the snapshot header. This distinction matters for Windows volumes: drives
	// are virtual /C/... roots and UNC volumes are one /\\server\share component.
	// Keep the conversion here so listing and restore cannot drift apart.
	commandSource, err := resticSnapshotTreePath(source)
	if err != nil {
		return "", err
	}
	return id + ":" + resticPathFilter(commandSource), nil
}

func (e *resticEngine) DeleteSnapshot(ctx context.Context, repo models.Repository, id string) (output string, err error) {
	return e.DeleteSnapshots(ctx, repo, []string{id})
}
func (e *resticEngine) DeleteSnapshots(ctx context.Context, repo models.Repository, ids []string) (output string, err error) {
	for _, id := range ids {
		if err := ValidateSnapshotIDArgument(id); err != nil {
			return "", err
		}
	}
	args := append([]string{"forget"}, ids...)
	out, err := e.runRepo(ctx, repo, args, command.NoTotalDeadline)
	return out, err
}
func (e *resticEngine) Check(ctx context.Context, repo models.Repository, id string) (output string, err error) {
	if repo.ColdStorage && id == "" {
		return "", fmt.Errorf("%s", models.ColdStorageIntegrityHelp)
	}
	args := []string{"check", "--read-data"}
	if id != "" {
		if !resticSnapshotIDPattern.MatchString(id) || id != strings.ToLower(id) {
			return "", fmt.Errorf("invalid canonical Restic snapshot ID")
		}
		// Restic 0.19.1 accepts snapshot IDs after --read-data, so a selected
		// integrity check remains a native check instead of staging a restore.
		args = append(args, id)
	}
	// Replicaro admits integrity checks only for the current vault owner and
	// serializes all Replicaro work with the managed vault UUID lock. For the
	// supported external-backup overlap, Restic's publication order keeps this
	// safe: an unindexed pack is a non-critical additional file, an
	// indexed pack is verified even if its snapshot was not captured, and a pack
	// published after enumeration is deferred to the next check. Success covers
	// the repository view captured by this run, not every concurrent upload.
	// Independently launched native mutations remain outside Replicaro's safety
	// boundary and must not be overlapped with this no-lock check.
	out, err := e.runRepoWithGlobal(ctx, repo, []string{"--no-cache", "--no-lock"}, args, command.NoTotalDeadline)
	return out, err
}
func (e *resticEngine) Maintenance(ctx context.Context, repo models.Repository) (output string, err error) {
	out, err := e.runRepo(ctx, repo, []string{"prune"}, command.NoTotalDeadline)
	return out, err
}

func (e *resticEngine) changeRepositoryPassword(ctx context.Context, repo models.Repository, newPassword, _ string, nativeInputPath string) (output string, err error) {
	if err := models.ValidateVaultPassword(newPassword); err != nil {
		return "", requestedOperationPreparationFailure(ResticID, err)
	}
	if filepath.Base(nativeInputPath) != "restic-new-password.txt" {
		return "", requestedOperationPreparationFailure(ResticID, fmt.Errorf("native password input path is invalid"))
	}
	return e.runRepo(ctx, repo, []string{"key", "passwd", "--new-password-file", nativeInputPath}, command.NoTotalDeadline)
}

func (e *resticEngine) probeRepositoryPassword(ctx context.Context, repo models.Repository) (string, error) {
	var fingerprint string
	ctx = context.WithValue(ctx, resticRawConfigObserverKey{}, func(output string) error {
		var config struct {
			Version     int    `json:"version"`
			Compression string `json:"compression"`
		}
		if err := json.Unmarshal([]byte(output), &config); err != nil || config.Version != 2 || strings.EqualFold(config.Compression, "off") {
			return fmt.Errorf("native Restic repository config is invalid")
		}
		digest := sha256.Sum256(append([]byte(output), []byte("\x00"+ResticID)...))
		fingerprint = hex.EncodeToString(digest[:])
		return nil
	})
	_, err := e.runRepo(ctx, repo, []string{"cat", "config"}, command.NoTotalDeadline)
	if err != nil {
		return "", err
	}
	if fingerprint == "" {
		return "", fmt.Errorf("native Restic repository config fingerprint is unavailable")
	}
	return fingerprint, nil
}

type resticRawConfigObserverKey struct{}

func (e *resticEngine) runRepo(ctx context.Context, repo models.Repository, args []string, timeout time.Duration) (string, error) {
	if resticFileBackedOutput(args) {
		capture, output, err := e.runRepoCommandCaptured(ctx, repo, nil, args, timeout)
		if capture != nil {
			defer capture.Close()
		}
		return output, err
	}
	return e.runRepoCommand(ctx, repo, nil, args, timeout, nil)
}

func (e *resticEngine) runRepoWithGlobal(ctx context.Context, repo models.Repository, global, args []string, timeout time.Duration) (string, error) {
	if resticFileBackedOutput(args) {
		capture, output, err := e.runRepoCommandCaptured(ctx, repo, global, args, timeout)
		if capture != nil {
			defer capture.Close()
		}
		return output, err
	}
	return e.runRepoCommand(ctx, repo, global, args, timeout, nil)
}

func (e *resticEngine) runRepoCommandCaptured(ctx context.Context, repo models.Repository, global, args []string, timeout time.Duration) (*command.CapturedOutput, string, error) {
	var capture *command.CapturedOutput
	output, err := e.runRepoCommand(ctx, repo, global, args, timeout, func(commandCtx context.Context, env, commandArgs []string, timeout time.Duration) (string, error, bool) {
		kind := resticOutputKind(args)
		if kind != "" {
			commandCtx = command.ContextWithCapturedOutputKind(commandCtx, kind)
		}
		var runErr error
		var invokeOutput string
		capture, invokeOutput, runErr = e.runCaptured(commandCtx, env, commandArgs, timeout)
		archivedObject := false
		if resticCapturedNativeFailure(runErr) && !repo.ColdStorage && repo.Engine == ResticID && repo.Connector == "s3" && capture != nil {
			archivedObject = capturedResticArchivedObject(capture)
		}
		return invokeOutput, runErr, archivedObject
	})
	return capture, output, err
}

type resticCommandInvoker func(context.Context, []string, []string, time.Duration) (string, error, bool)

func (e *resticEngine) runRepoCommand(ctx context.Context, repo models.Repository, global, args []string, timeout time.Duration, invoke resticCommandInvoker) (string, error) {
	archiveWriteClass, err := models.NormalizeColdStorage(repo.Engine, repo.Connector, repo.ColdStorage, repo.ArchiveWriteClass)
	if err != nil {
		return "", err
	}
	cache, err := resticCacheDir(repo)
	if err != nil {
		return "", err
	}
	repository, connectorArgs, connectorEnv, closeStorage, err :=
		prepareResticStorage(ctx, repo)
	if err != nil {
		return "", resticPreparationFailure(err)
	}
	backend, connections, _, err := resticConcurrency(repo)
	if err != nil {
		if closeStorage != nil {
			err = errors.Join(err, closeStorage())
		}
		return "", resticPreparationFailure(err)
	}
	// This is a Restic backend option, including for the rclone backend. It
	// controls how many backend operations Restic submits; Replicaro does not
	// alter rclone's own transfer or checker concurrency.
	connectorArgs = append(connectorArgs, "-o", backend+".connections="+strconv.Itoa(connections))
	if repo.ColdStorage && len(args) > 0 && args[0] != "unlock" {
		connectorArgs = append(connectorArgs,
			"-o", "s3.storage-class="+archiveWriteClass,
			"-o", "s3.enable-restore=true",
			"-o", "s3.restore-days=7",
			"-o", "s3.restore-tier=Standard",
			"-o", "s3.restore-timeout=72h",
		)
		connectorEnv = append(connectorEnv, "RESTIC_FEATURES=s3-restore")
	}
	env := append([]string{"RESTIC_PASSWORD=" + repo.Passphrase, "RESTIC_CACHE_DIR=" + cache}, connectorEnv...)
	if eligibleResticLiveCommand(args) {
		// Restic's native progress cadence is scoped only to the same requested
		// user-facing commands that own live presentation. Internal probes and
		// setup commands must not inherit it, and no user option can override the
		// adapter-owned environment value.
		env = append(env, "RESTIC_PROGRESS_FPS=0.5")
	}
	commandArgs := append(append([]string{}, global...), "--repo", repository)
	commandArgs = append(commandArgs, connectorArgs...)
	commandArgs = append(commandArgs, args...)
	if len(args) > 0 && args[0] == "backup" {
		if admissionErr := admitSourceBackup(ctx); admissionErr != nil {
			if closeStorage != nil {
				admissionErr = errors.Join(admissionErr, closeStorage())
				closeStorage = nil
			}
			return "", resticPreparationFailure(admissionErr)
		}
	}
	if len(args) > 0 && args[0] == "forget" {
		if admissionErr := admitNativeDeletion(ctx); admissionErr != nil {
			if closeStorage != nil {
				admissionErr = errors.Join(admissionErr, closeStorage())
				closeStorage = nil
			}
			return "", resticPreparationFailure(admissionErr)
		}
	}
	if len(args) > 0 && args[0] == "prune" {
		if admissionErr := admitResticMaintenancePrune(ctx); admissionErr != nil {
			if closeStorage != nil {
				admissionErr = errors.Join(admissionErr, closeStorage())
				closeStorage = nil
			}
			return "", resticPreparationFailure(admissionErr)
		}
	}
	if len(args) > 0 && args[0] == "check" {
		// Storage preparation, including the private rclone-config path check,
		// precedes this final protected-root owner read. Keep it immediately
		// before the no-lock Restic child so a takeover during preparation cannot
		// launch the check.
		if admissionErr := admitIntegrityCheck(ctx); admissionErr != nil {
			if closeStorage != nil {
				admissionErr = errors.Join(admissionErr, closeStorage())
				closeStorage = nil
			}
			return "", resticPreparationFailure(admissionErr)
		}
	}
	trackedContext, processStarted := command.ContextWithProcessStartTracking(ctx)
	if resticLiveOutputEnabled(repo, args) {
		trackedContext = command.ContextWithLiveOutputEnabled(trackedContext)
	}
	readSuccessfulStderr := func() string { return "" }
	if eligibleResticLiveCommand(args) && invoke == nil {
		trackedContext, readSuccessfulStderr = command.ContextWithSuccessfulStderrCapture(trackedContext)
	}
	privateConfigObservation := false
	run := e.run
	if _, ok := ctx.Value(resticRawConfigObserverKey{}).(func(string) error); ok {
		privateConfigObservation = true
		run = e.runPrivate
		trackedContext = command.ContextWithoutLiveOutput(trackedContext)
	}
	output := ""
	var runErr error
	archivedObjectObserved := false
	if invoke == nil {
		output, runErr = run(trackedContext, env, commandArgs, timeout)
	} else {
		output, runErr, archivedObjectObserved = invoke(trackedContext, env, commandArgs, timeout)
	}
	nativeProcessFailed := runErr != nil
	if invoke != nil {
		nativeProcessFailed = resticCapturedNativeFailure(runErr)
	}
	if runErr == nil {
		output = combineSuccessfulNativeStreams(output, readSuccessfulStderr())
	}
	if runErr == nil {
		if observer, ok := ctx.Value(resticRawConfigObserverKey{}).(func(string) error); ok {
			if observerErr := observer(output); observerErr != nil {
				runErr = observerErr
			}
		}
	}
	if privateConfigObservation {
		// The exact successful bytes are consumed only by the fingerprint observer.
		// They never become a method result, diagnostic, or postcheck failure body.
		output = ""
	}
	started := processStarted() || runErr == nil
	if closeStorage != nil {
		closeErr := closeStorage()
		// The selected local config is reopened after rclone can refresh it. A
		// local cleanup failure is follow-up truth, not a provider postcheck or
		// an alteration of the requested Restic command's native result.
		if closeErr != nil {
			return output, resticCommandCleanupFailure(runErr, &privateOutputError{
				message: "local rclone session cleanup failed", cause: closeErr,
			}, started)
		}
		if runErr != nil {
			return output, resticCommandFailure(
				runErr, started,
			)
		}
		return output, nil
	}
	if nativeProcessFailed && !repo.ColdStorage && repo.Engine == ResticID && repo.Connector == "s3" &&
		(archivedObjectObserved || resticArchivedObjectDiagnostic(output)) {
		return output, resticCommandFailure(
			errors.Join(ErrColdStorageArchivedObject, runErr), started,
		)
	}
	if runErr != nil {
		return output, resticCommandFailure(runErr, started)
	}
	return output, nil
}

const (
	resticInvalidObjectState  = "InvalidObjectState"
	resticInvalidStorageClass = "not valid for the object's storage class"
)

func resticArchivedObjectDiagnostic(output string) bool {
	return strings.Contains(output, resticInvalidObjectState) &&
		strings.Contains(strings.ToLower(output), resticInvalidStorageClass)
}

func resticCapturedNativeFailure(err error) bool {
	if err == nil {
		return false
	}
	nativeErr, outputErr := command.FailureCauses(err)
	if nativeErr != nil {
		return true
	}
	// Test adapters may return a plain process error rather than the production
	// command carrier. A typed output-only failure is the one case that must not
	// be promoted to native failure truth.
	return outputErr == nil
}

type resticArchivedObjectMatcher struct {
	invalidObjectState  bool
	invalidStorageClass bool
	tail                []byte
}

func (matcher *resticArchivedObjectMatcher) streamBoundary() { matcher.tail = nil }

func (matcher *resticArchivedObjectMatcher) Write(value []byte) (int, error) {
	written := len(value)
	window := make([]byte, 0, len(matcher.tail)+len(value))
	window = append(window, matcher.tail...)
	window = append(window, value...)
	if !matcher.invalidObjectState {
		matcher.invalidObjectState = bytes.Contains(window, []byte(resticInvalidObjectState))
	}
	if !matcher.invalidStorageClass {
		matcher.invalidStorageClass = bytes.Contains(bytes.ToLower(window), []byte(resticInvalidStorageClass))
	}
	overlap := max(len(resticInvalidObjectState), len(resticInvalidStorageClass)) - 1
	if overlap > len(window) {
		overlap = len(window)
	}
	matcher.tail = append(matcher.tail[:0], window[len(window)-overlap:]...)
	return written, nil
}

func capturedResticArchivedObject(capture *command.CapturedOutput) bool {
	matcher := &resticArchivedObjectMatcher{}
	for index, open := range []func() (*os.File, error){capture.OpenStdout, capture.OpenStderr} {
		if index > 0 {
			matcher.streamBoundary()
		}
		reader, err := open()
		if err != nil {
			continue
		}
		_, _ = io.Copy(matcher, reader)
		_ = reader.Close()
	}
	return matcher.invalidObjectState && matcher.invalidStorageClass
}

func resticFileBackedOutput(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "backup", "restore", "check", "prune", "forget", "snapshots", "ls":
		return true
	default:
		// init, unlock, key passwd, and cat config return bounded control outcomes
		// or one fixed repository-config protocol record. They do not enumerate
		// repository content and retain the explicit legacy inline boundary.
		return false
	}
}

func resticOutputKind(args []string) string {
	if len(args) == 0 {
		return "native"
	}
	switch args[0] {
	case "backup":
		return "backup"
	case "restore":
		return "restore"
	case "check":
		return "check"
	case "prune":
		return "maintenance"
	case "forget":
		for index, arg := range args[:len(args)-1] {
			if arg == "--tag" && strings.HasPrefix(args[index+1], "replicaro-job:") {
				return "retention"
			}
		}
		return "snapshot_deletion"
	default:
		return ""
	}
}

func eligibleResticLiveCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	switch args[0] {
	case "backup", "restore", "check", "prune", "forget":
		return true
	default:
		return false
	}
}

func resticStorage(repo models.Repository) (string, []string, []string, error) {
	repo = normalizedStorageRepository(repo, ResticID)
	address, err := effectiveAddress(repo)
	if err != nil {
		return "", nil, nil, err
	}
	options := repo.ConnectorOptions
	switch address.Connector {
	case "fs":
		return repo.Location, nil, nil, nil
	case "sftp":
		path := remotePath(address.Prefix)
		user := ""
		if address.Username != "" {
			user = url.User(address.Username).String() + "@"
		}
		separator := "/"
		if address.PathMode == "absolute" {
			separator = "//"
		}
		// Restic parses this argument as a URL. The effective path is already
		// literal, so escape once here; Azure/GCS colon grammars below are not URLs.
		repository := "sftp://" + user + sftpHostPort(address) + (&url.URL{Path: separator + path}).EscapedPath()
		sshArgs := []string{"-p", address.Port}
		identity, err := materializeSSHPrivateKey(repo)
		if err != nil {
			return "", nil, nil, err
		}
		if identity != "" {
			if strings.ContainsAny(identity, "\r\n\"") {
				return "", nil, nil, fmt.Errorf("Restic SFTP identity paths cannot contain quotes or newlines")
			}
			sshArgs = append(sshArgs, "-i", `"`+identity+`"`)
		}
		if boolOption(options, "insecure_ignore_host_key") {
			sshArgs = append(sshArgs, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile="+os.DevNull)
		}
		sftpOption, err := encodeResticExtendedOption("sftp.args", strings.Join(sshArgs, " "))
		if err != nil {
			return "", nil, nil, fmt.Errorf("encode Restic SFTP option: %w", err)
		}
		args := []string{"-o", sftpOption}
		env := []string{}
		if socket := strings.TrimSpace(options["ssh_auth_sock"]); socket != "" {
			env = append(env, "SSH_AUTH_SOCK="+socket)
		}
		return repository, args, env, nil
	case "s3":
		endpoint := address.Endpoint
		if endpoint == "" {
			endpoint = "https://s3.amazonaws.com"
		}
		// S3 root options are literal paths; concatenating them into the URL
		// would turn #/? into syntax and decode percent-lookalike names twice.
		storagePath := "/" + address.Bucket
		if prefix := remotePath(address.Prefix); prefix != "" {
			storagePath += "/" + prefix
		}
		repository := "s3:" + strings.TrimSuffix(endpoint, "/") + (&url.URL{Path: storagePath}).EscapedPath()
		args := []string{"-o", "s3.bucket-lookup=path"}
		if storageClass := strings.TrimSpace(options["storage_class"]); storageClass != "" && !repo.ColdStorage {
			args = append(args, "-o", "s3.storage-class="+storageClass)
		}
		if boolOption(options, "tls_insecure_no_verify") {
			args = append(args, "--insecure-tls")
		}
		env := []string{"AWS_ACCESS_KEY_ID=" + options["access_key"], "AWS_SECRET_ACCESS_KEY=" + options["secret_access_key"]}
		if region := strings.TrimSpace(options["region"]); region != "" {
			env = append(env, "AWS_REGION="+region, "AWS_DEFAULT_REGION="+region)
		}
		return repository, args, env, nil
	case "azblob":
		repository := "azure:" + address.Bucket + ":/" + remotePath(address.Prefix)
		account, key := azureCredentials(options)
		env := []string{"AZURE_ACCOUNT_NAME=" + account, "AZURE_ACCOUNT_KEY=" + key}
		if sas := azureSASToken(options); sas != "" {
			env = append(env, "AZURE_ACCOUNT_SAS="+sas)
		}
		if domain := azureResticEndpointSuffix(address.Endpoint, account); domain != "" {
			env = append(env, "AZURE_ENDPOINT_SUFFIX="+domain)
		}
		return repository, nil, env, nil
	case "gcs":
		repository := "gs:" + address.Bucket + ":/" + remotePath(address.Prefix)
		credentials, err := materializeGCSCredentials(repo)
		if err != nil {
			return "", nil, nil, err
		}
		env := []string{}
		if credentials != "" {
			env = append(env, "GOOGLE_APPLICATION_CREDENTIALS="+credentials)
		}
		if address.Endpoint != "" {
			env = append(env, "STORAGE_EMULATOR_HOST="+address.Endpoint)
		}
		return repository, nil, env, nil
	default:
		return "", nil, nil, fmt.Errorf("unsupported Restic storage connector %q", address.Connector)
	}
}

func prepareResticStorage(
	ctx context.Context,
	repo models.Repository,
) (
	string,
	[]string,
	[]string,
	func() error,
	error,
) {
	if !IsResticRcloneConnector(repo.Connector) {
		repository, args, env, err := resticStorage(repo)
		return repository, args, env, nil, err
	}
	session, err := newResticRcloneSession(ctx, repo)
	if err != nil {
		return "", nil, nil, nil, &privateOutputError{
			message: "native Restic preparation failed",
			cause:   err,
		}
	}
	return session.repository, session.args, session.env, session.close, nil
}

func encodeResticExtendedOption(key, value string) (string, error) {
	var encoded bytes.Buffer
	writer := csv.NewWriter(&encoded)
	if err := writer.Write([]string{key + "=" + value}); err != nil {
		return "", err
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return "", err
	}
	return strings.TrimSuffix(encoded.String(), "\n"), nil
}

func resticCacheDir(repo models.Repository) (string, error) {
	if strings.TrimSpace(repo.ID) == "" {
		return "", fmt.Errorf("restic vault ID is required for isolated cache")
	}
	directory, err := appdata.CacheDirectory(filepath.Join("engines", ResticID, repo.ID))
	if err != nil {
		return "", err
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", err
	}
	return directory, nil
}
func (e *resticEngine) run(ctx context.Context, env, args []string, timeout time.Duration) (string, error) {
	if e.path == "" {
		return "", fmt.Errorf("bundled restic engine is unavailable")
	}
	return commandRunner(ctx, e.path, args, env, timeout, ResticID)
}

func (e *resticEngine) runCaptured(ctx context.Context, env, args []string, timeout time.Duration) (*command.CapturedOutput, string, error) {
	if e.path == "" {
		return nil, "", fmt.Errorf("bundled restic engine is unavailable")
	}
	return capturedCommandRunner(ctx, e.path, args, env, timeout, ResticID)
}

func (e *resticEngine) runPrivate(ctx context.Context, env, args []string, timeout time.Duration) (string, error) {
	if e.path == "" {
		return "", fmt.Errorf("bundled restic engine is unavailable")
	}
	return privateCommandRunner(ctx, e.path, args, env, timeout, ResticID)
}

func parseResticBackupOutput(output string) models.Snapshot {
	snapshot, _ := parseResticBackupReader(strings.NewReader(output))
	return snapshot
}

func parseResticBackupReader(output io.Reader) (models.Snapshot, error) {
	var snapshot models.Snapshot
	var tags []string
	summaryCount := 0
	usableSummary := false
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		var row struct {
			MessageType string          `json:"message_type"`
			ID          string          `json:"snapshot_id"`
			Time        string          `json:"backup_start"`
			TotalBytes  json.RawMessage `json:"total_bytes_processed"`
		}
		_, uniqueErr := decodeUniqueJSON([]byte(line))
		ambiguousSize := false
		if uniqueErr != nil && strings.Contains(uniqueErr.Error(), `duplicate JSON field "total_bytes_processed"`) {
			// Preserve independently conclusive identity from the native summary,
			// but never publish an ambiguous optional statistics value.
			var ignored any
			uniqueErr = json.Unmarshal([]byte(line), &ignored)
			ambiguousSize = true
		}
		if uniqueErr == nil {
			if json.Unmarshal([]byte(line), &row) == nil && row.MessageType == "summary" {
				summaryCount++
				snapshot.ID, snapshot.Timestamp = row.ID, row.Time
				canonicalID := resticSnapshotIDPattern.MatchString(row.ID)
				if value, valid := exactNonnegativeJSONInteger(row.TotalBytes); canonicalID && !ambiguousSize && valid {
					snapshot.Size = formatBytes(value)
					snapshot.LogicalSizeBytes = &value
					usableSummary = true
				} else {
					snapshot.LogicalSizeBytes = nil
				}
			}
		}
		var event struct {
			MessageType string   `json:"message_type"`
			ID          string   `json:"id"`
			Tags        []string `json:"tags"`
		}
		if json.Unmarshal([]byte(line), &event) == nil && event.MessageType == "snapshot" {
			if snapshot.ID == "" {
				snapshot.ID = event.ID
			}
			tags = event.Tags
		}
	}
	if summaryCount != 1 || !usableSummary {
		snapshot.LogicalSizeBytes = nil
	}
	if err := scanner.Err(); err != nil {
		return models.Snapshot{}, fmt.Errorf("parse restic backup output: %w", err)
	}
	return models.SnapshotWithTags(snapshot, tags), nil
}
func parseResticEntries(output string, source ...string) ([]models.SnapshotEntry, error) {
	sourceRoot := ""
	if len(source) > 0 {
		sourceRoot = source[0]
	}
	return parseResticEntriesForSnapshot(output, "", sourceRoot)
}

func parseResticEntriesForSnapshot(output, expectedSnapshotID, source string) ([]models.SnapshotEntry, error) {
	return parseResticEntriesForSnapshotReader(strings.NewReader(output), expectedSnapshotID, source)
}

func parseResticEntriesForSnapshotReader(output io.Reader, expectedSnapshotID, source string) ([]models.SnapshotEntry, error) {
	var result []models.SnapshotEntry
	seen := map[string]models.SnapshotEntry{}
	sourceRoot := normalizeResticPath(source)
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	headerSeen := false
	nodesSeen := false
	matchedNodes := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var row struct {
			MessageType string   `json:"message_type"`
			ID          string   `json:"id"`
			Paths       []string `json:"paths"`
			Name        string   `json:"name"`
			Path        string   `json:"path"`
			Type        string   `json:"type"`
			Size        int64    `json:"size"`
			Permissions string   `json:"permissions"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse restic listing: malformed JSON: %w", err)
		}
		switch row.MessageType {
		case "snapshot":
			if headerSeen || nodesSeen || !resticSnapshotIDPattern.MatchString(row.ID) ||
				(resticSnapshotIDPattern.MatchString(expectedSnapshotID) && row.ID != expectedSnapshotID) || len(row.Paths) == 0 {
				return nil, fmt.Errorf("parse restic listing: malformed or duplicate snapshot header")
			}
			matchedSource := sourceRoot == "" || sourceRoot == "/"
			for _, root := range row.Paths {
				if root == "" {
					return nil, fmt.Errorf("parse restic listing: snapshot header has an empty source path")
				}
				matchedSource = matchedSource || resticPathsEqual(sourceRoot, root)
			}
			headerSeen = true
			if !matchedSource {
				return nil, fmt.Errorf("parse restic listing: snapshot source does not match requested source")
			}
			continue
		case "node":
			if !headerSeen {
				return nil, fmt.Errorf("parse restic listing: node appeared before snapshot header")
			}
			nodesSeen = true
		default:
			return nil, fmt.Errorf("parse restic listing: unexpected record type %q", row.MessageType)
		}
		if row.Type == "" || (row.Path == "" && row.Name == "") {
			return nil, fmt.Errorf("parse restic listing: node is missing required path or type")
		}
		storedPath := row.Path
		if storedPath == "" {
			storedPath = row.Name
		}
		path, ok := normalizeResticEntryPath(sourceRoot, storedPath)
		if !ok {
			if sourceRoot != "" && resticPathIsAncestor(storedPath, sourceRoot) {
				continue
			}
			return nil, fmt.Errorf("parse restic listing: path %q is outside the snapshot source", storedPath)
		}
		matchedNodes = true
		if path == "" {
			continue
		}
		name := pathpkg.Base(path)
		if name == "" || name == "." {
			return nil, fmt.Errorf("parse restic listing: invalid entry name")
		}
		entry := models.SnapshotEntry{Name: name, Path: path, Mode: row.Permissions, Size: formatBytes(row.Size), IsDir: row.Type == "dir" || row.Type == "directory"}
		if previous, exists := seen[path]; exists && previous != entry {
			return nil, fmt.Errorf("parse restic listing: conflicting duplicate path %q", path)
		}
		if _, exists := seen[path]; !exists {
			seen[path] = entry
			result = append(result, entry)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse restic listing: %w", err)
	}
	if !headerSeen {
		return nil, fmt.Errorf("parse restic listing: snapshot header is missing")
	}
	// An exact Restic filter that does not match still exits successfully after
	// emitting only the snapshot header. A genuinely empty saved directory emits
	// its directory node, which is counted here before the selected root is
	// omitted from presentation. Requiring that positive native evidence keeps
	// an unmatched filter from becoming a falsely ready empty cache.
	if !matchedNodes {
		return nil, fmt.Errorf("parse restic listing: exact snapshot filter matched no nodes")
	}
	return result, nil
}

func parseResticDirectoryEntries(output, expectedSnapshotID, source, requestedPath string, recursive bool) ([]models.SnapshotEntry, error) {
	return parseResticDirectoryEntriesReader(strings.NewReader(output), expectedSnapshotID, source, requestedPath, recursive)
}

func parseResticDirectoryEntriesReader(output io.Reader, expectedSnapshotID, source, requestedPath string, recursive bool) ([]models.SnapshotEntry, error) {
	entries, err := parseResticEntriesForSnapshotReader(output, expectedSnapshotID, source)
	if err != nil {
		return nil, err
	}
	requestedPath = strings.Trim(normalizeResticPath(requestedPath), "/")
	prefix := ""
	if requestedPath != "" {
		prefix = requestedPath + "/"
	}
	result := make([]models.SnapshotEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.Path == "" || entry.Name == "" || entry.Name == "." || entry.Path == requestedPath {
			continue
		}
		remainder := entry.Path
		if prefix != "" {
			if !resticPathPrefixMatch(prefix, entry.Path, source) {
				continue
			}
			remainder = strings.TrimPrefix(entry.Path, prefix)
		}
		if remainder == "" || (!recursive && strings.Contains(remainder, "/")) {
			continue
		}
		result = append(result, entry)
	}
	return result, nil
}

func resticPathPrefixMatch(prefix, value, source string) bool {
	return strings.HasPrefix(value, prefix)
}

// Native tree paths are slash-addressed on every viewer. Do not interpret
// Windows-looking node names or trim whitespace returned by Restic.
func normalizeResticPath(value string) string { return value }

func resticWindowsStylePath(value string) bool {
	normalized := strings.ReplaceAll(value, "\\", "/")
	return resticWindowsDrivePath(normalized) || resticWindowsUNCPath(value)
}

func resticWindowsUNCPath(value string) bool {
	return strings.HasPrefix(value, `\\`)
}

// resticWindowsUNCTreePath preserves Restic's Windows volume node verbatim.
// Converting the leading backslashes to ordinary slashes would address a
// different archive path and Restic would return a successful header-only ls.
func resticWindowsUNCTreePath(value string) (string, error) {
	var remainder string
	switch {
	case strings.HasPrefix(value, `/\\`):
		remainder = strings.TrimPrefix(value, `/\\`)
	case strings.HasPrefix(value, `\\`):
		remainder = strings.TrimPrefix(value, `\\`)
	default:
		return "", fmt.Errorf("restic snapshot source is not a backslash UNC path")
	}
	// A trailing separator is a valid spelling of a share or directory root.
	// Remove only that suffix; leading and interior empty components below must
	// still fail rather than being collapsed into a different native address.
	remainder = strings.TrimRight(remainder, `/\`)
	// Preserve component boundaries while validating. A separator-collapsing
	// split can turn malformed roots such as \\\server\share or
	// \\server\\share into a different, valid native Restic address.
	parts := make([]string, 0, 4)
	for remainder != "" {
		separator := strings.IndexAny(remainder, `/\\`)
		if separator < 0 {
			parts = append(parts, remainder)
			remainder = ""
			break
		}
		if separator == 0 {
			return "", fmt.Errorf("restic UNC snapshot source contains an empty path component")
		}
		parts = append(parts, remainder[:separator])
		remainder = remainder[separator+1:]
	}
	if len(parts) < 2 {
		return "", fmt.Errorf("restic UNC snapshot source requires server and share components")
	}
	if parts[0] == "?" || parts[0] == "." {
		return "", fmt.Errorf("restic device namespace snapshot sources are unsupported")
	}
	for _, part := range parts {
		if part == "." || part == ".." {
			return "", fmt.Errorf("restic UNC snapshot source contains an invalid path component")
		}
	}
	result := `/\\` + parts[0] + `\` + parts[1]
	if len(parts) > 2 {
		result += "/" + strings.Join(parts[2:], "/")
	}
	return result, nil
}

// Only source-header translation may map a Windows volume. Actual tree nodes
// and restore selections never pass through this conversion. The ls parser
// additionally requires a node within that native source tree, so an unmatched source
// cannot publish a falsely empty index. Relative historical sources cannot be
// resolved against this computer's working directory.
func resticSnapshotTreePath(value string) (string, error) {
	if strings.HasPrefix(value, "/") {
		return value, nil
	}
	if resticWindowsUNCPath(value) {
		return resticWindowsUNCTreePath(value)
	}
	if isDriveAbsolutePath(value) {
		return "/" + value[:1] + "/" + strings.ReplaceAll(value[3:], `\`, "/"), nil
	}
	return "", fmt.Errorf("restic source has no unambiguous absolute native tree address")
}

func resticPathsEqual(left, right string) bool { return left == right }

func normalizeResticEntryPath(source, value string) (string, bool) {
	if source == "" {
		return strings.TrimPrefix(value, "/"), value != ""
	}
	root, err := resticSnapshotTreePath(source)
	if err != nil {
		return "", false
	}
	if value == root {
		return "", true
	}
	prefix := strings.TrimSuffix(root, "/") + "/"
	if strings.HasPrefix(value, prefix) {
		return strings.TrimPrefix(value, prefix), true
	}
	return "", false
}

func resticPathDescendant(root, candidate string) bool {
	root = normalizeResticPath(root)
	candidate = normalizeResticPath(candidate)
	if root == "/" {
		return strings.HasPrefix(candidate, "/") && candidate != "/"
	}
	prefix := strings.TrimRight(root, "/") + "/"
	return strings.HasPrefix(candidate, prefix)
}

func resticPathIsAncestor(ancestor, descendant string) bool {
	root, err := resticSnapshotTreePath(descendant)
	return err == nil && (ancestor == root || resticPathDescendant(ancestor, root))
}

func resticStoredPath(source, value string) (string, bool) {
	if source == "" || value == "" {
		return "", false
	}
	if strings.HasPrefix(value, "/") {
		_, ok := normalizeResticEntryPath(source, value)
		return value, ok
	}
	relative, err := normalizeSnapshotRelativePath(value)
	if err != nil {
		return "", false
	}
	return strings.TrimSuffix(source, "/") + "/" + relative, true
}

func resticLiteralIncludePath(relative string) string {
	relative = strings.TrimLeft(normalizeResticPath(relative), "/")
	escaped := strings.NewReplacer(
		`\`, `\\`,
		`*`, `\*`,
		`?`, `\?`,
		`[`, `\[`,
	).Replace(relative)
	return "/" + escaped
}

// Restic's ls positional filters and restore subfolder selectors use
// slash-rooted snapshot paths. Windows drive paths such as C:/Users/... are
// represented in the snapshot tree as the virtual path /C/Users/.... UNC
// sources are normalized earlier because their volume node must retain its
// leading backslashes.
func resticPathFilter(value string) string {
	if value == "" || strings.HasPrefix(value, "/") {
		return value
	}
	return "/" + value
}

func resticWindowsDrivePath(value string) bool {
	return len(value) > 2 &&
		((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) &&
		value[1] == ':' && value[2] == '/'
}

var resticSnapshotIDPattern = regexp.MustCompile(`(?i)^[0-9a-f]{64}$`)

func formatBytes(value int64) string {
	if value < 0 {
		value = 0
	}
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	size := float64(value)
	unit := 0
	for size >= 1024 && unit < len(units)-1 {
		size /= 1024
		unit++
	}
	if unit == 0 {
		return strconv.FormatInt(value, 10) + " B"
	}
	if size >= 10 {
		return fmt.Sprintf("%.0f %s", size, units[unit])
	}
	return fmt.Sprintf("%.1f %s", size, units[unit])
}
func binaryName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}
