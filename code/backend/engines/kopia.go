package engines

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/vaultidentity"
)

type kopiaEngine struct {
	path, statusError string
	custom            bool
}

func kopiaPreparationFailure(repo models.Repository, err error) error {
	if err == nil {
		return nil
	}
	return requestedOperationPreparationFailure(KopiaID, err)
}

func kopiaFollowupFailure(repo models.Repository, err error) error {
	if err == nil {
		return nil
	}
	return requestedOperationFollowupFailure(KopiaID, err)
}

func kopiaOutputProcessingFailure(repo models.Repository, err error) error {
	if err == nil {
		return nil
	}
	return kopiaFollowupFailure(repo, &command.OutputProcessingFailure{Err: err})
}

type kopiaInitializationGate struct {
	locked chan struct{}
}

var kopiaInitializationGates sync.Map

func acquireKopiaInitialization(ctx context.Context, repositoryID string) (func(), error) {
	value, _ := kopiaInitializationGates.LoadOrStore(repositoryID, &kopiaInitializationGate{locked: make(chan struct{}, 1)})
	gate := value.(*kopiaInitializationGate)
	select {
	case gate.locked <- struct{}{}:
		return func() { <-gate.locked }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newKopia(custom string) Engine {
	path, statusError := detectBinary(KopiaID, custom, binaryName("kopia"))
	return &kopiaEngine{path: path, statusError: statusError, custom: custom != ""}
}
func (e *kopiaEngine) ID() string { return KopiaID }
func (e *kopiaEngine) version(ctx context.Context) (string, error) {
	out, err := e.run(ctx, nil, []string{"--version"}, 2*time.Minute)
	return strings.TrimSpace(out), err
}
func (e *kopiaEngine) Descriptor() Descriptor {
	return e.descriptor(context.Background())
}
func (e *kopiaEngine) descriptor(ctx context.Context) Descriptor {
	d := Descriptor{ID: KopiaID, Name: "Kopia", Compression: "Zstandard", Encryption: "AES-256", Installed: e.path != "", Path: e.path, Operations: operationNames(), JobSettings: []string{"additionalOptions"}, Providers: storageProviderDescriptors(KopiaID), Capabilities: capabilitiesFor(KopiaID)}
	if e.statusError != "" {
		d.Error = e.statusError
	}
	if e.path != "" {
		if version, err := e.version(ctx); err == nil {
			parsedVersion, parseErr := parseEngineVersion(KopiaID, version)
			if parseErr != nil {
				d.Installed = false
				d.Error = "Kopia version output is not parseable"
			} else {
				d.Version = parsedVersion.String()
				if e.custom && !semanticVersionEqual(version, "0.23.1") {
					d.CompatibilityWarning = "Custom Kopia version is outside the tested 0.23.1 range."
				}
			}
		} else {
			d.Installed = false
			d.Error = "Kopia executable is not working: " + err.Error()
		}
	}
	return d
}
func (e *kopiaEngine) config(repo models.Repository) (string, string, error) {
	if strings.TrimSpace(repo.ID) == "" {
		return "", "", fmt.Errorf("kopia vault ID is required for isolated configuration")
	}
	_, err := appdata.Directory(filepath.Join("engines", KopiaID, repo.ID))
	if err != nil {
		return "", "", err
	}
	configDir, err := appdata.Directory(filepath.Join("engines", KopiaID, repo.ID, "config"))
	if err != nil {
		return "", "", err
	}
	cacheDir, err := appdata.CacheDirectory(filepath.Join("engines", KopiaID, repo.ID, "cache"))
	if err != nil {
		return "", "", err
	}
	logDir, err := appdata.CacheDirectory(filepath.Join("engines", KopiaID, repo.ID, "logs"))
	if err != nil {
		return "", "", err
	}
	for _, directory := range []string{configDir, cacheDir, logDir} {
		if err := os.Chmod(directory, 0o700); err != nil {
			return "", "", err
		}
	}
	return filepath.Join(configDir, "repository.config"), cacheDir + "\x00" + logDir, nil
}
func (e *kopiaEngine) baseArgs(repo models.Repository) ([]string, []string, error) {
	config, cacheAndLog, err := e.config(repo)
	if err != nil {
		return nil, nil, err
	}
	parts := strings.SplitN(cacheAndLog, "\x00", 2)
	env := []string{"KOPIA_PASSWORD=" + repo.Passphrase, "KOPIA_CACHE_DIRECTORY=" + parts[0], "KOPIA_CHECK_FOR_UPDATES=false", "KOPIA_USE_KEYRING=false", "KOPIA_PERSIST_CREDENTIALS_ON_CONNECT=false", "KOPIA_LOG_DIR=" + parts[1]}
	switch repo.Connector {
	case "s3":
		env = append(env, "AWS_ACCESS_KEY_ID="+repo.ConnectorOptions["access_key"], "AWS_SECRET_ACCESS_KEY="+repo.ConnectorOptions["secret_access_key"])
	case "azblob":
		account, key := azureCredentials(repo.ConnectorOptions)
		env = append(env, "AZURE_STORAGE_ACCOUNT="+account, "AZURE_STORAGE_KEY="+key)
		if sas := azureSASToken(repo.ConnectorOptions); sas != "" {
			env = append(env, "AZURE_STORAGE_SAS_TOKEN="+sas)
		}
	}
	if socket := strings.TrimSpace(repo.ConnectorOptions["ssh_auth_sock"]); socket != "" {
		env = append(env, "SSH_AUTH_SOCK="+socket)
	}
	return []string{"--config-file", config, "--disable-file-logging", "--disable-content-log", "--no-persist-credentials", "--no-progress", "--log-dir", parts[1]}, env, nil
}

type kopiaRepositorySession struct {
	engine *kopiaEngine
	repo   models.Repository
	base   []string
	env    []string
}

type kopiaOperationSessionScopeKey struct{}

type kopiaOperationSessionScope struct {
	mu       sync.Mutex
	sessions map[string]*kopiaRepositorySession
	values   map[string]any
	closers  []func() error
	closed   bool
}

// WithOperationSessionScope allows related synchronous commands in one backup
// operation to reuse a repository binding validation. The scope must not be
// shared between unrelated operations.
func WithOperationSessionScope(ctx context.Context) context.Context {
	return context.WithValue(ctx, kopiaOperationSessionScopeKey{}, &kopiaOperationSessionScope{
		sessions: map[string]*kopiaRepositorySession{},
		values:   map[string]any{},
	})
}

func operationSessionScopeFromContext(ctx context.Context) *kopiaOperationSessionScope {
	scope, _ := ctx.Value(kopiaOperationSessionScopeKey{}).(*kopiaOperationSessionScope)
	return scope
}

func scopedOperationValue(ctx context.Context, key string) any {
	scope := operationSessionScopeFromContext(ctx)
	if scope == nil {
		return nil
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	return scope.values[key]
}

func rememberOperationValue(ctx context.Context, key string, value any, closer func() error) bool {
	scope := operationSessionScopeFromContext(ctx)
	if scope == nil {
		return false
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	if scope.closed {
		return false
	}
	scope.values[key] = value
	if closer != nil {
		scope.closers = append(scope.closers, closer)
	}
	return true
}

// CloseOperationSessionScope releases operation-scoped engine resources before
// terminal state is persisted. It is safe to call more than once.
func CloseOperationSessionScope(ctx context.Context) error {
	scope := operationSessionScopeFromContext(ctx)
	if scope == nil {
		return nil
	}
	scope.mu.Lock()
	if scope.closed {
		scope.mu.Unlock()
		return nil
	}
	closers := append([]func() error(nil), scope.closers...)
	scope.mu.Unlock()
	var result error
	failed := make([]func() error, 0, len(closers))
	for index := len(closers) - 1; index >= 0; index-- {
		if err := closers[index](); err != nil {
			result = errors.Join(result, err)
			failed = append(failed, closers[index])
		}
	}
	scope.mu.Lock()
	scope.closers = failed
	scope.closed = len(failed) == 0
	scope.mu.Unlock()
	return result
}

func scopedKopiaSession(ctx context.Context, repo models.Repository) *kopiaRepositorySession {
	scope, _ := ctx.Value(kopiaOperationSessionScopeKey{}).(*kopiaOperationSessionScope)
	if scope == nil {
		return nil
	}
	scope.mu.Lock()
	defer scope.mu.Unlock()
	session := scope.sessions[repo.ID]
	if session == nil || !sameOperationRepository(session.repo, normalizedStorageRepository(repo, KopiaID)) {
		return nil
	}
	return session
}

func rememberKopiaSession(ctx context.Context, session *kopiaRepositorySession) {
	scope, _ := ctx.Value(kopiaOperationSessionScopeKey{}).(*kopiaOperationSessionScope)
	if scope == nil || session == nil {
		return
	}
	scope.mu.Lock()
	scope.sessions[session.repo.ID] = session
	scope.mu.Unlock()
}

func sameOperationRepository(left, right models.Repository) bool {
	return left.ID == right.ID &&
		left.Connector == right.Connector && left.Location == right.Location &&
		left.Passphrase == right.Passphrase && mapsEqual(left.ConnectorOptions, right.ConnectorOptions)
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func (e *kopiaEngine) openRepositorySession(ctx context.Context, repo models.Repository, probeMissing bool) (*kopiaRepositorySession, string, error) {
	repo = normalizedStorageRepository(repo, KopiaID)
	if session := scopedKopiaSession(ctx, repo); session != nil {
		return session, "", nil
	}
	base, env, err := e.baseArgs(repo)
	if err != nil {
		return nil, "", err
	}
	output, err := e.ensureKopiaRepository(ctx, repo, base, env, probeMissing)
	if err != nil {
		return nil, output, err
	}
	session := &kopiaRepositorySession{engine: e, repo: repo, base: base, env: env}
	rememberKopiaSession(ctx, session)
	return session, output, nil
}

func (s *kopiaRepositorySession) run(ctx context.Context, args []string, timeout time.Duration) (string, error) {
	if kopiaFileBackedOutput(args) {
		capture, output, err := s.runCaptured(ctx, args, timeout)
		if capture != nil {
			defer capture.Close()
		}
		return output, err
	}
	if err := validateKopiaRepositoryInvocation(s.repo); err != nil {
		return "", kopiaPreparationFailure(s.repo, err)
	}
	commandContext, processStarted := command.ContextWithProcessStartTracking(ctx)
	if eligibleKopiaLiveCommand(args) {
		commandContext = command.ContextWithLiveOutputEnabled(commandContext)
	}
	readSuccessfulStderr := func() string { return "" }
	if eligibleKopiaLiveCommand(args) {
		commandContext, readSuccessfulStderr = command.ContextWithSuccessfulStderrCapture(commandContext)
	}
	output, err := s.engine.run(commandContext, s.env, kopiaCommandArgs(s.base, args), timeout)
	if err == nil {
		output = combineSuccessfulNativeStreams(output, readSuccessfulStderr())
	}
	started := processStarted() || err == nil
	return output, requestedOperationCommandFailure(KopiaID, err, nil, started)
}

func (s *kopiaRepositorySession) runCaptured(ctx context.Context, args []string, timeout time.Duration) (*command.CapturedOutput, string, error) {
	if err := validateKopiaRepositoryInvocation(s.repo); err != nil {
		return nil, "", kopiaPreparationFailure(s.repo, err)
	}
	commandContext, processStarted := command.ContextWithProcessStartTracking(ctx)
	if eligibleKopiaLiveCommand(args) {
		commandContext = command.ContextWithLiveOutputEnabled(commandContext)
	}
	commandContext = command.ContextWithCapturedOutputKind(commandContext, kopiaOutputKind(args))
	capture, output, err := s.engine.runCaptured(commandContext, s.env, kopiaCommandArgs(s.base, args), timeout)
	started := processStarted() || err == nil
	return capture, output, requestedOperationCommandFailure(KopiaID, err, nil, started)
}

func kopiaFileBackedOutput(args []string) bool {
	if len(args) == 0 {
		return false
	}
	if eligibleKopiaLiveCommand(args) {
		return true
	}
	return args[0] == "list" || args[0] == "show" || args[0] == "policy"
}

func eligibleKopiaLiveCommand(args []string) bool {
	if len(args) < 2 {
		return false
	}
	return args[0] == "snapshot" && (args[1] == "create" || args[1] == "restore" || args[1] == "verify" || args[1] == "delete") ||
		args[0] == "maintenance" && args[1] == "run"
}

func kopiaOutputKind(args []string) string {
	if len(args) < 2 {
		return "native"
	}
	if args[0] == "maintenance" && args[1] == "run" {
		return "maintenance"
	}
	if args[0] != "snapshot" {
		return args[0]
	}
	switch args[1] {
	case "create":
		return "backup"
	case "restore":
		return "restore"
	case "verify":
		return "check"
	case "delete":
		return "snapshot_deletion"
	default:
		return args[1]
	}
}

func kopiaCommandArgs(base, args []string) []string {
	result := append([]string(nil), base...)
	if eligibleKopiaLiveCommand(args) {
		// Pinned Kopia has a native progress toggle but no interval control. Only
		// requested user-facing commands replace the default suppression; probes,
		// bootstrap, listing, policy, and configuration commands retain it.
		for index := range result {
			if result[index] == "--no-progress" {
				result[index] = "--progress"
			}
		}
	}
	return append(result, args...)
}
func (e *kopiaEngine) Info(ctx context.Context, repo models.Repository) (output string, err error) {
	repo = normalizedStorageRepository(repo, KopiaID)
	base, env, err := e.baseArgs(repo)
	if err != nil {
		return "", err
	}
	output, err = e.ensureKopiaRepository(ctx, repo, base, env, true)
	if err != nil {
		return output, err
	}
	if err := validateKopiaRepositoryInvocation(repo); err != nil {
		return output, err
	}
	status, statusErr := e.run(ctx, env, append(base, "repository", "status", "--json"), command.NoTotalDeadline)
	if statusErr != nil {
		return status, statusErr
	}
	if err := validateKopiaBinding(repo, status); err != nil {
		return status, err
	}
	return status, nil
}

// validateRepository connects only to an isolated local Kopia configuration
// and reads repository status. Unlike Info, it deliberately leaves repository
// maintenance settings unchanged so connect previews remain non-mutating.
func (e *kopiaEngine) validateRepository(ctx context.Context, repo models.Repository) (string, error) {
	repo = normalizedStorageRepository(repo, KopiaID)
	base, env, err := e.baseArgs(repo)
	if err != nil {
		return "", err
	}
	output, err := e.ensureKopiaRepository(ctx, repo, base, env, true)
	return output, err
}
func (e *kopiaEngine) Create(ctx context.Context, repo models.Repository) (output string, err error) {
	// The explicit storage-create boundary is safe for ordinary native output.
	// Later client and policy control commands are separate private work.
	repo = normalizedStorageRepository(repo, KopiaID)
	args, input, err := kopiaStorageInvocation(repo, "create")
	if err != nil {
		return "", err
	}
	out, err := e.repoRunWithInput(command.ContextWithLiveOutputEnabled(ctx), repo, args, input, command.NoTotalDeadline)
	if err != nil {
		return out, err
	}
	_, status, statusErr := e.openRepositorySession(ctx, repo, true)
	if statusErr != nil {
		return out + "\n" + status, statusErr
	}
	return out + "\n" + status, nil
}

func (e *kopiaEngine) MaintenanceInfo(ctx context.Context, repo models.Repository) (string, error) {
	out, err := e.repoRun(ctx, repo, []string{"maintenance", "info"}, command.NoTotalDeadline)
	return out, err
}

type kopiaExportedPolicy struct {
	Retention struct {
		KeepLatest  *int `json:"keepLatest"`
		KeepHourly  *int `json:"keepHourly"`
		KeepDaily   *int `json:"keepDaily"`
		KeepWeekly  *int `json:"keepWeekly"`
		KeepMonthly *int `json:"keepMonthly"`
		KeepAnnual  *int `json:"keepAnnual"`
	} `json:"retention"`
	Files struct {
		Ignore []string `json:"ignore"`
	} `json:"files"`
	Compression struct {
		CompressorName string `json:"compressorName"`
	} `json:"compression"`
	Scheduling struct {
		Manual bool `json:"manual"`
	} `json:"scheduling"`
}

func normalizedKopiaExcludes(excludes []string) []string {
	result := make([]string, 0, len(excludes))
	for _, exclude := range excludes {
		if exclude = strings.TrimSpace(exclude); exclude != "" {
			result = append(result, exclude)
		}
	}
	return result
}

func (e *kopiaEngine) Backup(ctx context.Context, repo models.Repository, source string, options BackupOptions) (snapshot models.Snapshot, output string, err error) {
	extra, err := normalizedOptions(options.Settings)
	if err != nil {
		return models.Snapshot{}, "", kopiaPreparationFailure(repo, err)
	}
	if err := validateOptions(KopiaID, extra); err != nil {
		return models.Snapshot{}, "", kopiaPreparationFailure(repo, err)
	}
	concurrency, err := kopiaConcurrency(repo)
	if err != nil {
		return models.Snapshot{}, "", kopiaPreparationFailure(repo, err)
	}
	session, sessionOutput, err := e.openRepositorySession(ctx, repo, false)
	if err != nil {
		return models.Snapshot{}, sessionOutput, kopiaPreparationFailure(repo, err)
	}
	tag := options.Tag
	if tag != "" && !strings.Contains(tag, ":") {
		tag = "replicaro:" + tag
	}
	args := []string{"snapshot", "create", source, "--json", "--parallel", strconv.Itoa(concurrency.backup)}
	if tag != "" {
		args = append(args, "--tags", tag)
	}
	if options.OwnerProfileID != "" {
		args = append(args, "--tags", "replicaro-profile:"+options.OwnerProfileID)
	}
	if options.OwnerJobID != "" {
		args = append(args, "--tags", "replicaro-job:"+options.OwnerJobID)
		logicalSource := options.LogicalSource
		if logicalSource == "" {
			logicalSource = source
		}
		args = append(args, "--override-source", options.OwnerJobID+"@replicaro:"+logicalSource)
	}
	if options.OwnerProfileID != "" && options.OwnerJobID != "" {
		for _, tag := range backupPresentationTags(repo) {
			args = append(args, "--tags", tag)
		}
	}
	args = append(args, extra...)
	if admissionErr := admitSourceBackup(ctx); admissionErr != nil {
		return models.Snapshot{}, sessionOutput, kopiaPreparationFailure(repo, admissionErr)
	}
	capture, out, runErr := session.runCaptured(ctx, args, command.NoTotalDeadline)
	if capture != nil {
		defer capture.Close()
	}
	if runErr != nil {
		if partial, confirmed := kopiaPartialSnapshot(capture, runErr); confirmed {
			return partial, out, &BackupSourceReadFailure{SnapshotID: partial.ID, Err: runErr}
		}
		return models.Snapshot{}, out, runErr
	}
	// Kopia progress and warnings are stderr diagnostics. Only the exact stdout
	// stream is eligible for strict snapshot-result parsing.
	stdout, openErr := capture.OpenStdout()
	if openErr != nil {
		return models.Snapshot{}, out, kopiaFollowupFailure(repo, &command.OutputProcessingFailure{Err: openErr})
	}
	snapshot, parseErr := parseKopiaSnapshotReader(stdout)
	_ = stdout.Close()
	if parseErr != nil {
		return models.Snapshot{}, out, kopiaFollowupFailure(repo, &command.OutputProcessingFailure{Err: parseErr})
	}
	return snapshot, out, nil
}
func (e *kopiaEngine) ListSnapshots(ctx context.Context, repo models.Repository) (snapshots []models.Snapshot, output string, err error) {
	rows, out, err := e.snapshotRows(ctx, repo)
	if err != nil {
		return nil, out, err
	}
	result := make([]models.Snapshot, 0, len(rows))
	for _, row := range rows {
		result = append(result, models.SnapshotWithTags(models.Snapshot{NativeRootType: row.RootEntry.Type, ID: row.ID, Timestamp: row.StartTime,
			Source: row.Source.Path, TotalFileCount: kopiaNativeFileCount(row.RootEntry.Summary), NativeSourceUser: row.Source.UserName, NativeSourceHost: row.Source.Host,
			SourceRoots: []models.SnapshotSourceRoot{models.WithSnapshotNativeRootIdentity(models.SnapshotSourceRoot{Path: row.Source.Path, User: row.Source.UserName, Host: row.Source.Host})},
			Size:        formatBytes(row.Stats.TotalSize)}, kopiaSnapshotTags(row.Tags)))
	}
	return result, out, nil
}

type kopiaSnapshotRow struct {
	ID        string `json:"id"`
	StartTime string `json:"startTime"`
	Source    struct {
		Path     string `json:"path"`
		Host     string `json:"host"`
		UserName string `json:"userName"`
	} `json:"source"`
	RootEntry struct {
		Obj     string          `json:"obj"`
		Type    string          `json:"type"`
		Summary json.RawMessage `json:"summ"`
	} `json:"rootEntry"`
	Tags  map[string]string `json:"tags"`
	Stats struct {
		TotalSize int64 `json:"totalSize"`
	} `json:"stats"`
}

func kopiaSnapshotTags(tags map[string]string) []string {
	result := make([]string, 0, len(tags))
	for key, value := range tags {
		key = strings.TrimPrefix(key, "tag:")
		result = append(result, key+":"+value)
	}
	sort.Strings(result)
	return result
}

func (e *kopiaEngine) snapshotRows(ctx context.Context, repo models.Repository) ([]kopiaSnapshotRow, string, error) {
	session, sessionOutput, err := e.openRepositorySession(ctx, repo, false)
	if err != nil {
		return nil, sessionOutput, err
	}
	capture, out, err := session.runCaptured(ctx, []string{"snapshot", "list", "--all", "--show-identical", "--json"}, command.NoTotalDeadline)
	if capture != nil {
		defer capture.Close()
	}
	if err != nil {
		return nil, out, err
	}
	stdout, openErr := capture.OpenStdout()
	if openErr != nil {
		return nil, out, kopiaOutputProcessingFailure(repo, openErr)
	}
	rows, parseErr := parseKopiaSnapshotRowsReader(stdout)
	_ = stdout.Close()
	return rows, sessionOutput + out, kopiaOutputProcessingFailure(repo, parseErr)
}

func parseKopiaSnapshotRows(out string) ([]kopiaSnapshotRow, error) {
	return parseKopiaSnapshotRowsReader(strings.NewReader(out))
}

func parseKopiaSnapshotRowsReader(reader io.Reader) ([]kopiaSnapshotRow, error) {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, fmt.Errorf("parse kopia snapshots: snapshot inventory is not a JSON array")
	}
	var rows []kopiaSnapshotRow
	for decoder.More() {
		unique, ambiguousCount, uniqueErr := readUniqueJSONValueAtPath(decoder, nil, []string{"rootEntry", "summ", "files"})
		if uniqueErr != nil {
			return nil, fmt.Errorf("parse kopia snapshots: %w", uniqueErr)
		}
		encoded, marshalErr := json.Marshal(unique)
		if marshalErr != nil {
			return nil, fmt.Errorf("parse kopia snapshots: %w", marshalErr)
		}
		var row kopiaSnapshotRow
		if err := json.Unmarshal(encoded, &row); err != nil {
			return nil, fmt.Errorf("parse kopia snapshots: %w", err)
		}
		// An ambiguous optional file count is unusable for sizing or long-list
		// framing. Keep the header, and let the existing directory fallback
		// establish exact contents when the summary is unavailable.
		if ambiguousCount {
			row.RootEntry.Summary = nil
		}
		rows = append(rows, row)
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim(']') {
		return nil, fmt.Errorf("parse kopia snapshots: snapshot inventory has an incomplete JSON array")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("parse kopia snapshots: snapshot inventory has trailing JSON")
	}
	seen := map[string]bool{}
	for i := range rows {
		row := &rows[i]
		if strings.TrimSpace(row.ID) == "" || strings.TrimSpace(row.StartTime) == "" || row.Source.Path == "" || strings.TrimSpace(row.RootEntry.Obj) == "" {
			return nil, fmt.Errorf("parse kopia snapshots: snapshot is missing id, time, source, or root object")
		}
		if err := ValidateSnapshotIDArgument(row.ID); err != nil {
			return nil, fmt.Errorf("parse kopia snapshots: snapshot id %q is not canonical", row.ID)
		}
		parsedTime, err := time.Parse(time.RFC3339Nano, row.StartTime)
		if err != nil {
			return nil, fmt.Errorf("parse kopia snapshots: snapshot %q has malformed startTime", row.ID)
		}
		row.StartTime = parsedTime.UTC().Format(time.RFC3339Nano)
		if seen[row.ID] {
			return nil, fmt.Errorf("parse kopia snapshots: duplicate snapshot id %q", row.ID)
		}
		seen[row.ID] = true
	}
	return rows, nil
}
func (e *kopiaEngine) ListPath(ctx context.Context, repo models.Repository, id, path string) ([]models.SnapshotEntry, string, error) {
	return e.listPath(ctx, repo, id, path, false)
}
func (e *kopiaEngine) ListPathRecursive(ctx context.Context, repo models.Repository, id, path string) ([]models.SnapshotEntry, string, error) {
	return e.listPath(ctx, repo, id, path, true)
}
func (e *kopiaEngine) listPath(ctx context.Context, repo models.Repository, id, path string, recursive bool) (entries []models.SnapshotEntry, output string, err error) {
	session, statusOutput, err := e.openRepositorySession(ctx, repo, false)
	if err != nil {
		return nil, statusOutput, kopiaPreparationFailure(repo, err)
	}
	snapshotCapture, snapshotOutput, err := session.runCaptured(ctx, []string{"snapshot", "list", "--all", "--show-identical", "--json"}, command.NoTotalDeadline)
	if snapshotCapture != nil {
		defer snapshotCapture.Close()
	}
	if err != nil {
		return nil, snapshotOutput, kopiaPreparationFailure(repo, err)
	}
	snapshotStdout, openErr := snapshotCapture.OpenStdout()
	if openErr != nil {
		return nil, snapshotOutput, kopiaPreparationFailure(repo, &command.OutputProcessingFailure{Err: openErr})
	}
	rows, err := parseKopiaSnapshotRowsReader(snapshotStdout)
	_ = snapshotStdout.Close()
	if err != nil {
		return nil, snapshotOutput, kopiaPreparationFailure(repo, &command.OutputProcessingFailure{Err: err})
	}
	objectPath := ""
	var summary json.RawMessage
	for _, row := range rows {
		if row.ID == id {
			objectPath = row.RootEntry.Obj
			if strings.TrimSpace(objectPath) == "" || strings.TrimSpace(objectPath) != objectPath {
				return nil, snapshotOutput, kopiaPreparationFailure(repo, fmt.Errorf("snapshot %q has an invalid root object identity", row.ID))
			}
			if recursive && path == "" && row.RootEntry.Type == "f" {
				// Entry-only resume/retry has no index session. Validate the
				// fresh native header but never issue list/show for file payload.
				return []models.SnapshotEntry{}, snapshotOutput, nil
			}
			if row.RootEntry.Type != "d" {
				return nil, snapshotOutput, kopiaPreparationFailure(repo, fmt.Errorf("Kopia snapshot root is not a directory"))
			}
			summary = row.RootEntry.Summary
			break
		}
	}
	if objectPath == "" {
		return nil, snapshotOutput, kopiaPreparationFailure(repo,
			fmt.Errorf("Kopia snapshot %s was not found", id))
	}
	run := func(ctx context.Context, _ models.Repository, args []string, timeout time.Duration) (*command.CapturedOutput, string, error) {
		return session.runCaptured(ctx, args, timeout)
	}
	entries, output, err = e.listPathFromObject(ctx, repo, objectPath, path, recursive, run, summary)
	return entries, output, err
}

type kopiaCapturedCommandRunner func(context.Context, models.Repository, []string, time.Duration) (*command.CapturedOutput, string, error)

// Bound the returned context across a traversal; native capture/publication is unchanged.
func appendKopiaIndexDiagnostic(previous, next string) string {
	const limit = 256 << 10
	const omitted = "\n[earlier indexing diagnostics omitted]\n"
	if len(previous)+len(next) <= limit {
		return previous + next
	}
	previous = strings.TrimPrefix(previous, omitted)
	keep := limit - len(omitted)
	if len(next) >= keep {
		return omitted + next[len(next)-keep:]
	}
	tail := keep - len(next)
	if len(previous) > tail {
		previous = previous[len(previous)-tail:]
	}
	return omitted + previous + next
}

func (e *kopiaEngine) listPathFromObject(ctx context.Context, repo models.Repository, objectPath, path string, recursive bool, run kopiaCapturedCommandRunner, summaries ...json.RawMessage) ([]models.SnapshotEntry, string, error) {
	relative, err := normalizeSnapshotRelativePath(path)
	if err != nil {
		return nil, "", kopiaPreparationFailure(repo, err)
	}
	var summary json.RawMessage
	if len(summaries) > 0 {
		summary = summaries[0]
	}
	var output string
	// Resolve only directory entries before using show. Unlike list, show also
	// accepts file objects; probing an unchecked path would read backup payload.
	for _, component := range strings.Split(relative, "/") {
		if component == "" {
			continue
		}
		manifest, out, err := readKopiaDirectory(ctx, repo, objectPath, run)
		output = appendKopiaIndexDiagnostic(output, out)
		if err != nil {
			return nil, output, err
		}
		found := false
		for _, entry := range manifest.Entries {
			if entry.Name == component && entry.Type == "d" {
				objectPath = entry.Object
				summary = entry.Summary
				found = true
				break
			}
		}
		if !found {
			return nil, output, kopiaOutputProcessingFailure(repo, fmt.Errorf("Kopia path component %q is not a directory", component))
		}
	}
	if recursive {
		capture, out, err := run(ctx, repo, []string{"list", objectPath, "--long", "--recursive"}, command.NoTotalDeadline)
		output = appendKopiaIndexDiagnostic(output, out)
		if capture != nil {
			defer capture.Close()
		}
		// A failed native command is still a failure. JSON traversal only replaces
		// an unusable successful text representation, never authentication or I/O.
		if err != nil {
			return nil, output, err
		}
		stdout, err := capture.OpenStdout()
		if err != nil {
			return nil, output, kopiaOutputProcessingFailure(repo, err)
		}
		entries, parseErr := parseCheckedKopiaEntries(stdout, summary)
		_ = stdout.Close()
		if parseErr == nil {
			return entries, output, nil
		}
	}
	// Raw LF names can imitate another valid long-list record. Do not try to
	// repair individual lines or guess the damaged directory: discard the entire
	// candidate and traverse the requested tree from its known directory object.
	type pendingDirectory struct{ object, prefix string }
	pending := []pendingDirectory{{objectPath, ""}}
	var entries []models.SnapshotEntry
	for len(pending) > 0 {
		if err := ctx.Err(); err != nil {
			return nil, output, err
		}
		next := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		manifest, out, err := readKopiaDirectory(ctx, repo, next.object, run)
		output = appendKopiaIndexDiagnostic(output, out)
		if err != nil {
			return nil, output, err
		}
		for _, entry := range manifest.Entries {
			converted, err := entry.snapshotEntry(next.prefix)
			if err != nil {
				return nil, output, kopiaOutputProcessingFailure(repo, err)
			}
			entries = append(entries, converted)
			if recursive && entry.Type == "d" {
				pending = append(pending, pendingDirectory{entry.Object, converted.Path + "/"})
			}
		}
	}
	return entries, output, nil
}

type kopiaSnapshotIndexSession struct {
	repository *kopiaRepositorySession
	snapshots  []SnapshotIndexItem
	summaries  map[string]json.RawMessage
}

func (s *kopiaSnapshotIndexSession) Snapshots() []SnapshotIndexItem { return s.snapshots }
func (s *kopiaSnapshotIndexSession) Close() error                   { return nil }
func (s *kopiaSnapshotIndexSession) ListPathRecursive(ctx context.Context, snapshot SnapshotIndexItem, path string) ([]models.SnapshotEntry, string, error) {
	object, ok := snapshot.NativeReference.(string)
	if !ok || strings.TrimSpace(object) == "" {
		return nil, "", kopiaPreparationFailure(s.repository.repo,
			fmt.Errorf("kopia snapshot %q has no reusable root object", snapshot.Snapshot.ID))
	}
	// File roots have no browsable directory content. They participate in the
	// ordinary ready/complete cache state and remain whole-snapshot restorable.
	if snapshot.Snapshot.NativeRootType == "f" {
		return []models.SnapshotEntry{}, "", nil
	}
	if snapshot.Snapshot.NativeRootType != "d" {
		return nil, "", kopiaPreparationFailure(s.repository.repo, fmt.Errorf("Kopia snapshot has an unsupported root type"))
	}
	run := func(ctx context.Context, _ models.Repository, args []string, timeout time.Duration) (*command.CapturedOutput, string, error) {
		return s.repository.runCaptured(ctx, args, timeout)
	}
	entries, output, err := s.repository.engine.listPathFromObject(ctx, s.repository.repo, object, path, true, run, s.summaries[snapshot.Snapshot.ID])
	for index := range entries {
		entries[index].SourceRoot = snapshot.Snapshot.Source
	}
	return entries, output, err
}

func (e *kopiaEngine) BeginSnapshotIndex(ctx context.Context, repo models.Repository) (SnapshotIndexSession, string, error) {
	session, statusOutput, err := e.openRepositorySession(ctx, repo, false)
	if err != nil {
		return nil, statusOutput, kopiaPreparationFailure(repo, err)
	}
	capture, out, err := session.runCaptured(ctx, []string{"snapshot", "list", "--all", "--show-identical", "--json"}, command.NoTotalDeadline)
	if capture != nil {
		defer capture.Close()
	}
	if err != nil {
		return nil, out, err
	}
	stdout, openErr := capture.OpenStdout()
	if openErr != nil {
		return nil, out, kopiaOutputProcessingFailure(repo, openErr)
	}
	rows, err := parseKopiaSnapshotRowsReader(stdout)
	_ = stdout.Close()
	if err != nil {
		return nil, out, kopiaOutputProcessingFailure(repo, err)
	}
	items := make([]SnapshotIndexItem, 0, len(rows))
	summaries := make(map[string]json.RawMessage, len(rows))
	for _, row := range rows {
		snapshot := models.SnapshotWithTags(models.Snapshot{NativeRootType: row.RootEntry.Type, ID: row.ID, Timestamp: row.StartTime, Source: row.Source.Path,
			SourceRoots:    []models.SnapshotSourceRoot{{Path: row.Source.Path, User: row.Source.UserName, Host: row.Source.Host}},
			TotalFileCount: kopiaNativeFileCount(row.RootEntry.Summary), NativeSourceUser: row.Source.UserName, NativeSourceHost: row.Source.Host, Size: formatBytes(row.Stats.TotalSize)}, kopiaSnapshotTags(row.Tags))
		identity := row.RootEntry.Obj
		if row.RootEntry.Type != "d" && row.RootEntry.Type != "f" {
			return nil, out, kopiaOutputProcessingFailure(repo, fmt.Errorf("Kopia snapshot has an unsupported root type"))
		}
		summaries[row.ID] = row.RootEntry.Summary
		if strings.TrimSpace(identity) == "" || strings.TrimSpace(identity) != identity {
			return nil, out, kopiaOutputProcessingFailure(repo, fmt.Errorf("snapshot %q has an invalid root object identity", row.ID))
		}
		items = append(items, SnapshotIndexItem{Snapshot: snapshot, NativeContentIdentity: row.RootEntry.Type + ":" + identity, NativeReference: identity})
	}
	return &kopiaSnapshotIndexSession{repository: session, snapshots: items, summaries: summaries}, statusOutput + "\n" + out, nil
}
func (e *kopiaEngine) Restore(ctx context.Context, repo models.Repository, id string, options RestoreOptions) (output string, err error) {
	concurrency, err := kopiaConcurrency(repo)
	if err != nil {
		return "", kopiaPreparationFailure(repo, err)
	}
	selection, err := validateRestoreOptions(options)
	if err != nil {
		return "", kopiaPreparationFailure(repo, err)
	}
	rows, _, err := e.snapshotRows(ctx, repo)
	if err != nil {
		return "", kopiaPreparationFailure(repo, err)
	}
	source := ""
	for _, row := range rows {
		if row.ID == id {
			source = row.RootEntry.Obj
			if selection != "" && row.RootEntry.Type != "d" {
				return "", kopiaPreparationFailure(repo, fmt.Errorf("Kopia snapshot root is not a directory"))
			}
			break
		}
	}
	if source == "" {
		return "", kopiaPreparationFailure(repo, fmt.Errorf("kopia snapshot %q was not found", id))
	}
	if options.OriginalLocation {
		return "", kopiaPreparationFailure(repo, fmt.Errorf("kopia original-location restore is unsupported"))
	}
	// A content object can be shared by manifests with different root attributes.
	// Pinned Kopia resolves an ambiguous object to the newest manifest; the exact
	// selected manifest ID is required to restore its historical root metadata.
	// Selected-entry metadata traversal below still needs the content object.
	restoreSource := id
	destination := options.Destination
	if selection != "" {
		// Pass the archived spelling unchanged. Source separator interpretation
		// belongs to Kopia, including its Windows literal-backslash limitation.
		restoreSource = source + "/" + selection
		parent := pathpkg.Dir(selection)
		if parent == "." {
			parent = ""
		}
		session, sessionOutput, metadataErr := e.openRepositorySession(ctx, repo, false)
		if metadataErr != nil {
			return sessionOutput, kopiaPreparationFailure(repo, metadataErr)
		}
		run := func(ctx context.Context, _ models.Repository, args []string, timeout time.Duration) (*command.CapturedOutput, string, error) {
			return session.runCaptured(ctx, args, timeout)
		}
		parentObject := source
		metadata := sessionOutput
		var manifest *kopiaDirectoryManifest
		for _, component := range append(strings.Split(parent, "/"), "") {
			if manifest != nil && component == "" {
				break
			}
			var out string
			manifest, out, metadataErr = readKopiaDirectory(ctx, repo, parentObject, run)
			metadata += out
			if metadataErr != nil {
				return metadata, kopiaPreparationFailure(repo, metadataErr)
			}
			if component == "" {
				break
			}
			found := false
			for _, entry := range manifest.Entries {
				if entry.Name == component && entry.Type == "d" {
					parentObject = entry.Object
					found = true
					break
				}
			}
			if !found {
				return metadata, kopiaPreparationFailure(repo, fmt.Errorf("Kopia selected parent %q is not a directory", component))
			}
			manifest = nil
		}
		entryType, metadataErr := manifest.selectedType(pathpkg.Base(selection))
		if metadataErr != nil {
			return metadata, kopiaPreparationFailure(repo, &command.OutputProcessingFailure{Err: metadataErr})
		}

		switch entryType {
		case kopiaSelectedRegularFile:
			basename := kopiaDestinationBasename(pathpkg.Base(selection), runtime.GOOS)
			if options.KopiaFileNames != nil {
				basename = options.KopiaFileNames.assign(basename)
			}
			destination, metadataErr = kopiaSelectedFileDestination(options.Destination, basename)
			if metadataErr == nil && basename != pathpkg.Base(selection) && options.ReportDestination != nil {
				options.ReportDestination(fmt.Sprintf("Replicaro destination naming: selected file %q will use %q. Native overwrite policy remains %q.", selection, destination, options.ConflictMode))
			}
			if metadataErr != nil {
				return sessionOutput + metadata, kopiaPreparationFailure(repo, metadataErr)
			}
		case kopiaSelectedDirectory:
		default:
			return sessionOutput + metadata, kopiaPreparationFailure(repo,
				fmt.Errorf("kopia selected path %q has unsupported native object type %q", selection, entryType))
		}
	}
	// Kopia's default auto mode treats archive suffixes as output-format requests
	// and uses os.Create, bypassing filesystem no-overwrite flags. This feature
	// always requests native filesystem output, including filenames ending .zip.
	args := []string{"snapshot", "restore", "--mode=local", "--parallel", strconv.Itoa(concurrency.restore)}
	switch options.ConflictMode {
	case "overwrite":
	case "no-overwrite":
		args = append(args, "--no-overwrite-files", "--no-overwrite-directories", "--no-overwrite-symlinks")
	default:
		return "", kopiaPreparationFailure(repo,
			fmt.Errorf("unsupported Kopia conflict mode %q", options.ConflictMode))
	}
	args = append(args, restoreSource, destination)
	out, err := e.repoRun(ctx, repo, args, command.NoTotalDeadline)
	return out, err
}

type kopiaSelectedEntryType string

const (
	kopiaSelectedRegularFile kopiaSelectedEntryType = "regular_file"
	kopiaSelectedDirectory   kopiaSelectedEntryType = "directory"
	kopiaSelectedSymlink     kopiaSelectedEntryType = "symlink"
	kopiaSelectedUnsupported kopiaSelectedEntryType = "unsupported"
)

func parseKopiaLongEntry(line string) (mode, name string, err error) {
	position := 0
	var fields [6]string
	for index := range fields {
		for position < len(line) && (line[position] == ' ' || line[position] == '\t') {
			position++
		}
		start := position
		for position < len(line) && line[position] != ' ' && line[position] != '\t' {
			position++
		}
		if start == position {
			return "", "", fmt.Errorf("parse kopia listing: malformed long-list record")
		}
		fields[index] = line[start:position]
	}
	// Kopia renders the object ID in a minimum-width 34-byte column followed
	// by one mandatory separator. A 34-byte indirect ID (the native "Ix..."
	// form) consumes all column padding and therefore has exactly one space
	// before the name. Keep that separator distinct from leading name spaces.
	separatorWidth := 1
	if len(fields[5]) < 34 {
		separatorWidth += 34 - len(fields[5])
	}
	if position+separatorWidth > len(line) {
		return "", "", fmt.Errorf("parse kopia listing: malformed name separator")
	}
	for index := 0; index < separatorWidth; index++ {
		if line[position+index] != ' ' {
			return "", "", fmt.Errorf("parse kopia listing: malformed name separator")
		}
	}
	name = line[position+separatorWidth:]
	// Kopia appends its error summary after a directory's mandatory slash.
	// That framing distinguishes diagnostics from a real name ending in
	// " (N errors)" (which still has its slash after the closing parenthesis).
	// Raw stdout/stderr remain unchanged for native result presentation.
	if strings.HasPrefix(fields[0], "d") && strings.HasSuffix(name, " errors)") {
		if suffix := strings.LastIndex(name, "/ ("); suffix >= 0 {
			count := name[suffix+3 : len(name)-len(" errors)")]
			if _, err := strconv.ParseUint(count, 10, 64); err == nil {
				name = name[:suffix+1]
			}
		}
	}
	if fields[0] == "" || name == "" {
		return "", "", fmt.Errorf("parse kopia listing: missing mode or name")
	}
	return fields[0], name, nil
}

func kopiaSelectedFileDestination(directory, basename string) (string, error) {
	if basename == "" || basename == "." || basename == ".." || filepath.Base(basename) != basename || filepath.IsAbs(basename) {
		return "", fmt.Errorf("kopia selected file has an unsafe destination basename")
	}
	base, err := filepath.Abs(directory)
	if err != nil {
		return "", fmt.Errorf("resolve kopia restore destination: %w", err)
	}
	target := filepath.Join(base, basename)
	relative, err := filepath.Rel(base, target)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.IsAbs(relative) {
		return "", fmt.Errorf("kopia selected file destination escapes the chosen directory")
	}
	return filepath.Join(directory, basename), nil
}
func (e *kopiaEngine) DeleteSnapshot(ctx context.Context, repo models.Repository, id string) (output string, err error) {
	return e.DeleteSnapshots(ctx, repo, []string{id})
}
func (e *kopiaEngine) DeleteSnapshots(ctx context.Context, repo models.Repository, ids []string) (output string, err error) {
	for _, id := range ids {
		if err := ValidateSnapshotIDArgument(id); err != nil {
			return "", kopiaPreparationFailure(repo, err)
		}
	}
	args := append([]string{"snapshot", "delete"}, ids...)
	args = append(args, "--delete")
	out, err := e.repoRun(ctx, repo, args, command.NoTotalDeadline)
	return out, err
}

func (e *kopiaEngine) prepareIntegrityCheckCache(ctx context.Context, repo models.Repository) (output string, err error) {
	operationConfig := integrityCheckKopiaConfig(ctx)
	if operationConfig == "" {
		return "", fmt.Errorf("operation-owned Kopia integrity configuration is unavailable")
	}
	base, verifyEnv, err := e.kopiaIntegrityBase(repo, operationConfig)
	if err != nil {
		return "", err
	}
	if err := validateKopiaRepositoryInvocation(repo); err != nil {
		return "", err
	}
	operationRoot := filepath.Dir(operationConfig)
	preparationCache := filepath.Join(operationRoot, "cache")
	if err := os.Mkdir(preparationCache, 0o700); err != nil {
		return "", fmt.Errorf("create operation-owned Kopia preparation cache: %w", err)
	}
	if err := appdata.SecurePath(preparationCache, true); err != nil {
		return "", fmt.Errorf("secure operation-owned Kopia preparation cache: %w", err)
	}
	if _, err := secureIntegrityArtifactPath(operationRoot, preparationCache); err != nil {
		return "", err
	}

	// `cache set` opens the repository before its action changes the copied
	// configuration. Give that setup process a private cache inside the same
	// cleanup-owned directory; otherwise a relative cache path from the copied
	// config can resolve beside the copy and leave unbounded residue. This
	// override is deliberately limited to setup and is absent from verification.
	preparationEnv := replaceEnvironment(verifyEnv, "KOPIA_CACHE_DIRECTORY", preparationCache)

	// In pinned Kopia 0.23.1, a zero content soft limit is the switch that
	// clears the complete caching configuration. A zero hard limit alone means
	// "no hard limit", while a zero metadata soft limit can inherit the content
	// size. Keep all four flags together, with the content soft limit at zero,
	// so the native command records the intended content and metadata policy.
	cacheArgs := []string{
		"cache", "set",
		"--content-cache-size-mb=0",
		"--content-cache-size-limit-mb=0",
		"--metadata-cache-size-mb=0",
		"--metadata-cache-size-limit-mb=0",
	}
	output, err = e.run(ctx, preparationEnv, append(base, cacheArgs...), 2*time.Minute)
	if err != nil {
		return output, err
	}
	if err := secureIntegrityKopiaConfig(operationConfig); err != nil {
		return output, err
	}
	return output, nil
}

func (e *kopiaEngine) Check(ctx context.Context, repo models.Repository, id string) (output string, err error) {
	concurrency, err := kopiaConcurrency(repo)
	if err != nil {
		return "", kopiaPreparationFailure(repo, err)
	}
	args := []string{"snapshot", "verify", "--verify-files-percent=100",
		"--parallel", strconv.Itoa(concurrency.verify),
		"--file-parallelism", strconv.Itoa(concurrency.verifyFiles)}
	if id != "" {
		if err := ValidateSnapshotIDArgument(id); err != nil {
			return "", kopiaPreparationFailure(repo, err)
		}
		args = append(args, id)
		out, runErr := e.repoRun(ctx, repo, args, command.NoTotalDeadline)
		return out, runErr
	}
	operationConfig := integrityCheckKopiaConfig(ctx)
	if operationConfig == "" {
		return "", kopiaPreparationFailure(repo, fmt.Errorf("operation-owned Kopia integrity configuration is unavailable"))
	}
	base, env, err := e.kopiaIntegrityBase(repo, operationConfig)
	if err != nil {
		return "", kopiaPreparationFailure(repo, err)
	}
	if err := validateKopiaRepositoryInvocation(repo); err != nil {
		return "", kopiaPreparationFailure(repo, err)
	}

	// The canonical configuration must not be resized and restored around the
	// check: a crash between those operations could leave ordinary Kopia work
	// with a changed cache policy. The operation copy is disposable, so the
	// native cache mutation cannot escape this integrity check.
	if readyOutput, readyErr := e.ensureKopiaRepository(ctx, repo, base, env, false); readyErr != nil {
		return readyOutput, kopiaPreparationFailure(repo, readyErr)
	}
	if admissionErr := admitIntegrityCheck(ctx); admissionErr != nil {
		return "", kopiaPreparationFailure(repo, admissionErr)
	}
	commandContext, processStarted := command.ContextWithProcessStartTracking(ctx)
	commandContext = command.ContextWithLiveOutputEnabled(commandContext)
	commandContext = command.ContextWithCapturedOutputKind(commandContext, "check")
	capture, output, runErr := e.runCaptured(commandContext, env, kopiaCommandArgs(base, args), command.NoTotalDeadline)
	if capture != nil {
		defer capture.Close()
	}
	started := processStarted() || runErr == nil
	return output, requestedOperationCommandFailure(
		KopiaID, runErr, nil, started,
	)
}

func (e *kopiaEngine) kopiaIntegrityBase(repo models.Repository, operationConfig string) ([]string, []string, error) {
	base, env, err := e.baseArgs(repo)
	if err != nil {
		return nil, nil, err
	}
	if len(base) < 2 || base[0] != "--config-file" || strings.TrimSpace(operationConfig) == "" {
		return nil, nil, fmt.Errorf("Kopia integrity configuration arguments are invalid")
	}
	base = append([]string(nil), base...)
	base[1] = operationConfig

	// Do not leave KOPIA_CACHE_DIRECTORY in this environment. Kopia applies the
	// environment override after loading the now-empty caching configuration;
	// pinned 0.23.1 then constructs an invalid persistent-cache path and panics.
	// With no override, content reads use Kopia's pass-through implementation
	// and committed indexes use its in-memory cache.
	return base, removeEnvironment(env, "KOPIA_CACHE_DIRECTORY"), nil
}
func (e *kopiaEngine) Maintenance(ctx context.Context, repo models.Repository) (output string, err error) {
	out, err := e.repoRun(ctx, repo, []string{"maintenance", "run", "--full"}, command.NoTotalDeadline)
	return out, err
}

func (e *kopiaEngine) changeRepositoryPassword(ctx context.Context, repo models.Repository, newPassword, _ string, nativeInputPath string) (output string, err error) {
	if err := models.ValidateVaultPassword(newPassword); err != nil {
		return "", requestedOperationPreparationFailure(KopiaID, err)
	}
	if nativeInputPath != "" {
		return "", requestedOperationPreparationFailure(KopiaID, fmt.Errorf("Kopia does not accept a password file"))
	}
	output, err = e.repoRunWithEnvironment(ctx, repo, []string{"repository", "change-password"},
		command.NoTotalDeadline, "KOPIA_NEW_PASSWORD", newPassword)
	return output, err
}
func (e *kopiaEngine) repoRun(ctx context.Context, repo models.Repository, args []string, timeout time.Duration) (string, error) {
	return e.repoRunWithInput(ctx, repo, args, "", timeout)
}

func (e *kopiaEngine) repoRunWithEnvironment(ctx context.Context, repo models.Repository, args []string, timeout time.Duration, key, value string) (string, error) {
	repo = normalizedStorageRepository(repo, KopiaID)
	base, env, err := e.baseArgs(repo)
	if err != nil {
		return "", kopiaPreparationFailure(repo, err)
	}
	env = replaceEnvironment(env, key, value)
	if output, readyErr := e.ensureKopiaRepository(ctx, repo, base, env, false); readyErr != nil {
		return output, kopiaPreparationFailure(repo, readyErr)
	}
	if err := validateKopiaRepositoryInvocation(repo); err != nil {
		return "", kopiaPreparationFailure(repo, err)
	}
	commandContext, processStarted := command.ContextWithProcessStartTracking(ctx)
	output, runErr := e.run(commandContext, env, append(base, args...), timeout)
	started := processStarted() || runErr == nil
	return output, requestedOperationCommandFailure(KopiaID, runErr, nil, started)
}

func replaceEnvironment(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return append(result, prefix+value)
}

func removeEnvironment(env []string, key string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env))
	for _, entry := range env {
		if !strings.HasPrefix(entry, prefix) {
			result = append(result, entry)
		}
	}
	return result
}

func (e *kopiaEngine) repoRunWithInput(ctx context.Context, repo models.Repository, args []string, input string, timeout time.Duration) (string, error) {
	repo = normalizedStorageRepository(repo, KopiaID)
	if !kopiaBootstrapCommand(args) {
		if session := scopedKopiaSession(ctx, repo); session != nil {
			if len(args) >= 2 && args[0] == "snapshot" && args[1] == "delete" {
				if admissionErr := admitNativeDeletion(ctx); admissionErr != nil {
					return "", kopiaPreparationFailure(repo, admissionErr)
				}
			}
			if len(args) >= 2 && args[0] == "snapshot" && args[1] == "verify" {
				if admissionErr := admitIntegrityCheck(ctx); admissionErr != nil {
					return "", kopiaPreparationFailure(repo, admissionErr)
				}
			}
			return session.run(ctx, args, timeout)
		}
	}
	base, env, err := e.baseArgs(repo)
	if err != nil {
		if kopiaBootstrapCommand(args) {
			return "", err
		}
		return "", kopiaPreparationFailure(repo, err)
	}
	if !kopiaBootstrapCommand(args) {
		if output, readyErr := e.ensureKopiaRepository(ctx, repo, base, env, false); readyErr != nil {
			return output, kopiaPreparationFailure(repo, readyErr)
		}
	}
	var output string
	var runErr error
	if err := validateKopiaRepositoryInvocation(repo); err != nil {
		if kopiaBootstrapCommand(args) {
			return "", err
		}
		return "", kopiaPreparationFailure(repo, err)
	}
	if len(args) >= 2 && args[0] == "snapshot" && args[1] == "delete" {
		if admissionErr := admitNativeDeletion(ctx); admissionErr != nil {
			return "", kopiaPreparationFailure(repo, admissionErr)
		}
	}
	if len(args) >= 2 && args[0] == "snapshot" && args[1] == "verify" {
		if admissionErr := admitIntegrityCheck(ctx); admissionErr != nil {
			return "", kopiaPreparationFailure(repo, admissionErr)
		}
	}
	commandContext := ctx
	processStarted := func() bool { return false }
	if !kopiaBootstrapCommand(args) {
		commandContext, processStarted = command.ContextWithProcessStartTracking(ctx)
		if eligibleKopiaLiveCommand(args) {
			commandContext = command.ContextWithLiveOutputEnabled(commandContext)
		}
	}
	var capture *command.CapturedOutput
	if input == "" && eligibleKopiaLiveCommand(args) {
		commandContext = command.ContextWithCapturedOutputKind(commandContext, kopiaOutputKind(args))
		capture, output, runErr = e.runCaptured(commandContext, env, kopiaCommandArgs(base, args), timeout)
		if capture != nil {
			defer capture.Close()
		}
	} else if input != "" {
		output, runErr = e.runWithInput(commandContext, env, kopiaCommandArgs(base, args), input, timeout)
	} else {
		output, runErr = e.run(commandContext, env, kopiaCommandArgs(base, args), timeout)
	}
	if kopiaBootstrapCommand(args) {
		return output, runErr
	}
	started := processStarted() || runErr == nil
	return output, requestedOperationCommandFailure(KopiaID, runErr, nil, started)
}

func (e *kopiaEngine) ensureKopiaRepository(ctx context.Context, repo models.Repository, base, env []string, probeMissing bool) (string, error) {
	release, err := acquireKopiaInitialization(ctx, repo.ID)
	if err != nil {
		return "", err
	}
	defer release()

	config, _, err := e.config(repo)
	if err != nil {
		return "", err
	}
	_, statErr := os.Stat(config)
	if statErr != nil && !os.IsNotExist(statErr) {
		return "", statErr
	}
	markerExists := repo.Connector != "fs" || kopiaRepositoryExists(repo.Location)
	output := ""
	if os.IsNotExist(statErr) && markerExists {
		output, err = e.connect(ctx, repo)
		if err != nil {
			return output, err
		}
		statErr = nil
	}
	if statErr != nil && !probeMissing {
		return output, nil
	}
	if err := validateKopiaRepositoryInvocation(repo); err != nil {
		return output, err
	}
	status, err := e.run(ctx, env, append(base, "repository", "status", "--json"), command.NoTotalDeadline)
	if err != nil {
		return output + status, err
	}
	if err := validateKopiaBinding(repo, status); err != nil {
		return output + status, err
	}
	if output != "" {
		output += "\n"
	}
	return output + status, nil
}

func validateKopiaRepositoryInvocation(repo models.Repository) error {
	repo = normalizedStorageRepository(repo, KopiaID)
	if repo.Connector != "sftp" || strings.TrimSpace(repo.ConnectorOptions["password"]) == "" {
		return nil
	}
	_, err := ValidateSFTPKnownHosts(repo.ConnectorOptions)
	return err
}

func kopiaBootstrapCommand(args []string) bool {
	if len(args) < 2 || args[0] != "repository" {
		return false
	}
	switch args[1] {
	case "create", "connect", "status":
		return true
	default:
		return false
	}
}

func validateKopiaBinding(repo models.Repository, output string) error {
	repo = normalizedStorageRepository(repo, KopiaID)
	value, err := decodeUniqueJSON([]byte(output))
	if err != nil {
		return fmt.Errorf("kopia repository status is malformed")
	}
	document, ok := value.(map[string]any)
	if !ok {
		return fmt.Errorf("kopia repository status has an invalid root")
	}
	storage, ok := document["storage"].(map[string]any)
	if !ok {
		return fmt.Errorf("kopia repository status has no storage configuration")
	}
	config, ok := storage["config"].(map[string]any)
	if !ok {
		return fmt.Errorf("kopia repository status has no storage configuration")
	}
	expected, err := vaultidentity.ResolveEffectiveAddress(repo.Connector, repo.Location, repo.ConnectorOptions)
	if err != nil {
		return err
	}
	// Kopia object backends concatenate prefix + blob ID literally. Match the
	// exact CLI value produced by kopiaStorageArgs: a nonempty root ends in one
	// slash, while an empty root stays empty. Trimming native readback would
	// incorrectly admit both vault/blob and vaultblob (or /blob and blob).
	switch expected.Connector {
	case "s3", "azblob", "gcs":
		if expected.Prefix != "" {
			expected.Prefix += "/"
		}
	}
	actual, err := kopiaStatusAddress(storage, config)
	if err != nil {
		return err
	}
	if expected.Connector == "fs" && actual.Connector == "fs" {
		expected.Location, err = vaultidentity.CanonicalLocation("fs", expected.Location)
		if err != nil {
			return err
		}
		actual.Location, err = vaultidentity.CanonicalLocation("fs", actual.Location)
	}
	if err != nil || actual != expected {
		return fmt.Errorf("kopia configuration is bound to a different repository")
	}
	return nil
}

func kopiaStatusAddress(storage, config map[string]any) (vaultidentity.EffectiveAddress, error) {
	stringValue := func(key string) string {
		value, _ := config[key].(string)
		return value
	}
	// These status fields occupy an authority or a single bucket component in
	// the temporary location below. Reject syntax injection before resolving
	// them; the independent literal prefix must never hide an injected path.
	for _, key := range []string{"host", "endpoint", "bucket", "container"} {
		if strings.ContainsAny(stringValue(key), "/\\?#@%") {
			return vaultidentity.EffectiveAddress{}, fmt.Errorf("kopia repository status has an invalid storage address")
		}
	}
	storageType, _ := storage["type"].(string)
	// Status paths/prefixes are native literal strings, not public location URLs.
	// Resolve independent authority facts with a fixed path, then compare the
	// actual literal path separately. Escaping status into a URL and sending it
	// through input validation either rejects valid native roots or aliases a%2Fb
	// with a/b. No expected configuration may supply missing readback facts.
	actual := models.Repository{Engine: KopiaID, ConnectorOptions: map[string]string{}}
	var literalPrefix string
	switch storageType {
	case "", "filesystem":
		path := stringValue("path")
		if path == "" {
			return vaultidentity.EffectiveAddress{}, fmt.Errorf("kopia repository status did not report a storage path")
		}
		actual.Connector, actual.Location = "fs", path
	case "sftp":
		actual.Connector = "sftp"
		storagePath := stringValue("path")
		host := strings.Trim(stringValue("host"), "[]")
		if strings.Contains(host, ":") {
			host = "[" + host + "]"
		}
		actual.Location = "sftp://" + host + "/status-path"
		literalPrefix = storagePath
		actual.ConnectorOptions["username"] = stringValue("username")
		actual.ConnectorOptions["port"] = fmt.Sprint(config["port"])
		if strings.HasPrefix(storagePath, "/") {
			actual.ConnectorOptions["path_mode"] = "absolute"
		} else {
			actual.ConnectorOptions["path_mode"] = "home"
		}
	case "s3":
		literalPrefix = stringValue("prefix")
		actual.Connector = "s3"
		actual.Location = "s3://" + stringValue("endpoint") + "/" + stringValue("bucket") + "/status-path"
		actual.ConnectorOptions["endpoint"] = schemeForKopiaS3(config) + "://" + stringValue("endpoint")
		actual.ConnectorOptions["use_tls"] = fmt.Sprint(!boolValue(config["doNotUseTLS"]))
	case "azureBlob":
		literalPrefix = stringValue("prefix")
		actual.Connector = "azblob"
		actual.Location = "azblob://" + stringValue("container") + "/status-path"
		actual.ConnectorOptions["account_name"] = stringValue("storageAccount")
		if domain := stringValue("storageDomain"); domain != "" {
			actual.ConnectorOptions["endpoint"] = "https://" + stringValue("storageAccount") + "." + domain
		}
	case "gcs":
		literalPrefix = stringValue("prefix")
		actual.Connector = "gcs"
		actual.Location = "gs://" + stringValue("bucket") + "/status-path"
	default:
		return vaultidentity.EffectiveAddress{}, fmt.Errorf("kopia repository status reported unsupported storage type %q", storageType)
	}
	address, err := vaultidentity.ResolveEffectiveAddress(actual.Connector, actual.Location, actual.ConnectorOptions)
	if err != nil {
		return vaultidentity.EffectiveAddress{}, err
	}
	if actual.Connector != "fs" {
		address.Prefix = literalPrefix
	}
	return address, nil
}

func boolValue(value any) bool {
	result, _ := value.(bool)
	return result
}

func schemeForKopiaS3(config map[string]any) string {
	if boolValue(config["doNotUseTLS"]) {
		return "http"
	}
	return "https"
}

func findKopiaStoragePath(value any) string {
	document, ok := value.(map[string]any)
	if !ok {
		return ""
	}
	storage, ok := document["storage"].(map[string]any)
	if !ok {
		return ""
	}
	config, ok := storage["config"].(map[string]any)
	if !ok {
		return ""
	}
	path, _ := config["path"].(string)
	return path
}
func (e *kopiaEngine) connect(ctx context.Context, repo models.Repository) (string, error) {
	args, input, err := kopiaStorageInvocation(repo, "connect")
	if err != nil {
		return "", err
	}
	return e.repoRunWithInput(ctx, repo, args, input, command.NoTotalDeadline)
}

type kopiaStorageToken struct {
	Version string                  `json:"version"`
	Storage kopiaStorageTokenConfig `json:"storage"`
}

type kopiaStorageTokenConfig struct {
	Type   string                   `json:"type"`
	Config kopiaSFTPTokenConfigData `json:"config"`
}

type kopiaSFTPTokenConfigData struct {
	Path           string `json:"path"`
	Host           string `json:"host"`
	Port           int    `json:"port"`
	Username       string `json:"username"`
	Password       string `json:"password,omitempty"`
	KnownHostsFile string `json:"knownHostsFile,omitempty"`
}

func kopiaStorageInvocation(repo models.Repository, operation string) ([]string, string, error) {
	repo = normalizedStorageRepository(repo, KopiaID)
	args, err := kopiaStorageArgs(repo, operation)
	if err != nil {
		return nil, "", err
	}
	if repo.Connector != "sftp" || strings.TrimSpace(repo.ConnectorOptions["password"]) == "" {
		return args, "", nil
	}
	address, err := effectiveAddress(repo)
	if err != nil {
		return nil, "", err
	}
	if boolOption(repo.ConnectorOptions, "insecure_ignore_host_key") {
		return nil, "", fmt.Errorf("Kopia SFTP password authentication requires host-key verification")
	}
	// Kopia's from-config token path accepts storage credentials on stdin;
	// keeping the password out of argv preserves Replicaro's process boundary.
	port, err := strconv.Atoi(address.Port)
	if err != nil {
		return nil, "", fmt.Errorf("Kopia SFTP port: %w", err)
	}
	knownHosts, err := ValidateSFTPKnownHosts(repo.ConnectorOptions)
	if err != nil {
		return nil, "", err
	}
	token := kopiaStorageToken{
		Version: "1",
		Storage: kopiaStorageTokenConfig{
			Type: "sftp",
			Config: kopiaSFTPTokenConfigData{
				Path: address.Prefix, Host: address.Host, Port: port, Username: address.Username,
				Password: repo.ConnectorOptions["password"], KnownHostsFile: knownHosts,
			},
		},
	}
	data, err := json.Marshal(token)
	if err != nil {
		return nil, "", fmt.Errorf("encode Kopia SFTP credentials: %w", err)
	}
	return args, base64.RawURLEncoding.EncodeToString(data) + "\n", nil
}

func kopiaStorageArgs(repo models.Repository, operation string) ([]string, error) {
	repo = normalizedStorageRepository(repo, KopiaID)
	address, err := effectiveAddress(repo)
	if err != nil {
		return nil, err
	}
	common := []string{"--no-check-for-updates", "--no-enable-actions"}
	if operation == "create" {
		common = append(common, "--encryption", "AES256-GCM-HMAC-SHA256")
		if repo.ObjectLock.Enrolled {
			if repo.ObjectLock.Paused {
				return nil, fmt.Errorf("object lock cannot start paused during vault creation")
			}
			period, err := repo.ObjectLock.NativeRetentionPeriod()
			if err != nil {
				return nil, err
			}
			common = append(common, "--retention-mode", strings.ToUpper(repo.ObjectLock.Mode),
				"--retention-period", period)
		}
	}
	options := repo.ConnectorOptions
	args := []string{"repository", operation}
	switch address.Connector {
	case "fs":
		args = append(args, "filesystem", "--path", repo.Location, "--dir-mode=0700", "--file-mode=0600")
	case "sftp":
		if address.Username == "" {
			return nil, fmt.Errorf("SFTP username is required for Kopia")
		}
		if strings.TrimSpace(options["password"]) != "" {
			if boolOption(options, "insecure_ignore_host_key") {
				return nil, fmt.Errorf("Kopia SFTP password authentication requires host-key verification")
			}
			args = append(args, "from-config", "--token-stdin")
			return append(args, common...), nil
		}
		sshArgs := []string{"-p", address.Port}
		identity, err := materializeSSHPrivateKey(repo)
		if err != nil {
			return nil, err
		}
		insecure := boolOption(options, "insecure_ignore_host_key")
		if identity != "" && !insecure {
			home, err := os.UserHomeDir()
			if err != nil {
				return nil, fmt.Errorf("resolve SSH known_hosts: %w", err)
			}
			args = append(args, "sftp", "--host", address.Host, "--port", address.Port,
				"--username", address.Username, "--path", address.Prefix, "--keyfile", identity,
				"--known-hosts", filepath.Join(home, ".ssh", "known_hosts"))
			break
		}
		if identity != "" {
			if err := validateKopiaSFTPIdentityPath(identity); err != nil {
				return nil, err
			}
			sshArgs = append(sshArgs, "-i", identity)
		}
		if insecure {
			sshArgs = append(sshArgs, "-o", "StrictHostKeyChecking=no", "-o", "UserKnownHostsFile="+os.DevNull)
		}
		args = append(args, "sftp", "--host", address.Host, "--port", address.Port,
			"--username", address.Username, "--path", address.Prefix, "--external", "--ssh-args="+strings.Join(sshArgs, " "))
	case "s3":
		endpoint := endpointHost(address.Endpoint)
		if endpoint == "" {
			endpoint = "s3.amazonaws.com"
		}
		args = append(args, "s3", "--bucket", address.Bucket, "--endpoint", endpoint)
		if region := strings.TrimSpace(options["region"]); region != "" {
			args = append(args, "--region", region)
		}
		if prefix := remotePath(address.Prefix); prefix != "" {
			args = append(args, "--prefix", prefix+"/")
		}
		if address.UseTLS == "false" {
			args = append(args, "--disable-tls")
		}
		if boolOption(options, "tls_insecure_no_verify") {
			args = append(args, "--disable-tls-verification")
		}
	case "azblob":
		account, _ := azureCredentials(options)
		if account == "" {
			return nil, fmt.Errorf("Azure account name is required for Kopia")
		}
		args = append(args, "azure", "--container", address.Bucket, "--storage-account", account)
		if prefix := remotePath(address.Prefix); prefix != "" {
			args = append(args, "--prefix", prefix+"/")
		}
		if domain := azureStorageDomain(address.Endpoint, account); domain != "" {
			args = append(args, "--storage-domain", domain)
		}
	case "gcs":
		if address.Endpoint != "" {
			return nil, fmt.Errorf("Kopia GCS does not support custom endpoints")
		}
		args = append(args, "gcs", "--bucket", address.Bucket)
		if prefix := remotePath(address.Prefix); prefix != "" {
			args = append(args, "--prefix", prefix+"/")
		}
		credentials, err := materializeGCSCredentials(repo)
		if err != nil {
			return nil, err
		}
		if credentials != "" {
			args = append(args, "--credentials-file", credentials)
		}
	default:
		return nil, fmt.Errorf("unsupported Kopia storage connector %q", address.Connector)
	}
	return append(args, common...), nil
}
func (e *kopiaEngine) run(ctx context.Context, env, args []string, timeout time.Duration) (string, error) {
	if e.path == "" {
		return "", fmt.Errorf("bundled kopia engine is unavailable")
	}
	return commandRunner(ctx, e.path, args, env, timeout, KopiaID)
}

func (e *kopiaEngine) runCaptured(ctx context.Context, env, args []string, timeout time.Duration) (*command.CapturedOutput, string, error) {
	if e.path == "" {
		return nil, "", fmt.Errorf("bundled kopia engine is unavailable")
	}
	return capturedCommandRunner(ctx, e.path, args, env, timeout, KopiaID)
}

func (e *kopiaEngine) runWithInput(ctx context.Context, env, args []string, input string, timeout time.Duration) (string, error) {
	if e.path == "" {
		return "", fmt.Errorf("bundled kopia engine is unavailable")
	}
	return commandRunnerWithInput(ctx, e.path, args, env, input, timeout, KopiaID)
}
func parseKopiaSnapshot(output string) (models.Snapshot, error) {
	return parseKopiaSnapshotReader(strings.NewReader(output))
}

func parseKopiaSnapshotReader(output io.Reader) (models.Snapshot, error) {
	var found models.Snapshot
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		value, ambiguousSize, err := decodeUniqueJSONAllowDuplicatePath(
			[]byte(line), []string{"rootEntry", "summ", "size"})
		if err != nil {
			return models.Snapshot{}, fmt.Errorf("kopia snapshot output is malformed")
		}
		data, err := json.Marshal(value)
		if err != nil {
			return models.Snapshot{}, fmt.Errorf("kopia snapshot output is malformed")
		}
		var row struct {
			ID        string `json:"id"`
			StartTime string `json:"startTime"`
			Source    struct {
				Path     string `json:"path"`
				Host     string `json:"host"`
				UserName string `json:"userName"`
			} `json:"source"`
			Tags      map[string]string `json:"tags"`
			RootEntry struct {
				Type    string `json:"type"`
				Summary struct {
					TotalFileSize json.RawMessage `json:"size"`
				} `json:"summ"`
			} `json:"rootEntry"`
		}
		if err := json.Unmarshal(data, &row); err != nil || row.ID == "" || row.StartTime == "" ||
			row.Source.Path == "" {
			return models.Snapshot{}, fmt.Errorf("kopia snapshot output is missing required fields")
		}
		current := models.SnapshotWithTags(models.Snapshot{NativeRootType: row.RootEntry.Type, ID: row.ID, Timestamp: row.StartTime,
			Source: row.Source.Path, NativeSourceUser: row.Source.UserName,
			NativeSourceHost: row.Source.Host}, kopiaSnapshotTags(row.Tags))
		if value, valid := exactNonnegativeJSONInteger(row.RootEntry.Summary.TotalFileSize); !ambiguousSize && valid {
			current.Size = formatBytes(value)
			current.LogicalSizeBytes = &value
		}
		if found.ID != "" && (found.ID != current.ID || found.Timestamp != current.Timestamp ||
			found.Source != current.Source || found.NativeSourceUser != current.NativeSourceUser ||
			found.NativeSourceHost != current.NativeSourceHost || !slices.Equal(found.Tags, current.Tags)) {
			return models.Snapshot{}, fmt.Errorf("kopia snapshot output contains conflicting records")
		}
		if found.ID != "" {
			current.LogicalSizeBytes = nil
		}
		found = current
	}
	if err := scanner.Err(); err != nil {
		return models.Snapshot{}, fmt.Errorf("kopia snapshot output is malformed")
	}
	if found.ID == "" {
		return models.Snapshot{}, fmt.Errorf("kopia snapshot output did not contain a snapshot ID")
	}
	return found, nil
}

func kopiaRepositoryExists(location string) bool {
	for _, name := range []string{"kopia.repository.f", "kopia.blobcfg.f", "kopia.maintenance.f"} {
		if _, err := os.Stat(filepath.Join(location, name)); err == nil {
			return true
		}
	}
	return false
}

func KopiaRepositoryExists(location string) bool { return kopiaRepositoryExists(location) }
func decodeUniqueJSON(data []byte) (any, error) {
	value, _, err := decodeUniqueJSONAllowDuplicatePath(data, nil)
	return value, err
}

func decodeUniqueJSONAllowDuplicatePath(data []byte, optionalPath []string) (any, bool, error) {
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	value, ambiguousOptional, err := readUniqueJSONValueAtPath(decoder, nil, optionalPath)
	if err != nil {
		return nil, false, err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, false, fmt.Errorf("trailing JSON")
	}
	return value, ambiguousOptional, nil
}

func exactNonnegativeJSONInteger(raw json.RawMessage) (int64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	value, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
	return value, err == nil && value >= 0
}

func readUniqueJSONValueAtPath(decoder *json.Decoder, path, optionalPath []string) (any, bool, error) {
	token, err := decoder.Token()
	if err != nil {
		return nil, false, err
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			result := map[string]any{}
			ambiguousOptional := false
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return nil, false, err
				}
				key, ok := keyToken.(string)
				if !ok {
					return nil, false, fmt.Errorf("object key is not a string")
				}
				childPath := append(append([]string(nil), path...), key)
				child, childAmbiguous, err := readUniqueJSONValueAtPath(decoder, childPath, optionalPath)
				if err != nil {
					return nil, false, err
				}
				ambiguousOptional = ambiguousOptional || childAmbiguous
				if _, exists := result[key]; exists {
					if !slices.Equal(childPath, optionalPath) {
						return nil, false, fmt.Errorf("duplicate JSON field %q", key)
					}
					ambiguousOptional = true
					continue
				}
				result[key] = child
			}
			_, err = decoder.Token()
			return result, ambiguousOptional, err
		case '[':
			result := []any{}
			ambiguousOptional := false
			for decoder.More() {
				child, childAmbiguous, err := readUniqueJSONValueAtPath(decoder, path, optionalPath)
				if err != nil {
					return nil, false, err
				}
				result = append(result, child)
				ambiguousOptional = ambiguousOptional || childAmbiguous
			}
			_, err = decoder.Token()
			return result, ambiguousOptional, err
		}
	}
	return token, false, nil
}

// This is a recursive directory total. Kopia upload stats.fileCount omits cached
// files, so it cannot describe the amount of metadata a snapshot contains.
func kopiaNativeFileCount(summary json.RawMessage) *int64 {
	var row struct {
		Files json.RawMessage `json:"files"`
	}
	if err := json.Unmarshal(summary, &row); err != nil {
		return nil
	}
	return optionalNativeFileCount(row.Files)
}

func optionalNativeFileCount(raw json.RawMessage) *int64 {
	value, valid := exactNonnegativeJSONInteger(raw)
	if !valid {
		return nil
	}
	return &value
}
