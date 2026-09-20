package engines

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/local/replicaro/storageidentity"
	"io"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
)

const KopiaManagedRetentionCeiling = 99_999_999
const maximumKopiaPolicyDocumentBytes = 8 << 20
const maximumKopiaPolicyDiagnosticBytes = 256 << 10

func appendKopiaPolicyDiagnostic(output *strings.Builder, omitted *bool, value string) {
	value = strings.TrimSpace(value)
	if value == "" || *omitted {
		return
	}
	separator := ""
	if output.Len() > 0 {
		separator = "\n"
	}
	remaining := maximumKopiaPolicyDiagnosticBytes - output.Len()
	if remaining <= len(separator) {
		*omitted = true
		return
	}
	output.WriteString(separator)
	remaining -= len(separator)
	if len(value) <= remaining {
		output.WriteString(value)
		return
	}
	marker := "\n[Additional native policy diagnostics omitted from this bounded result; exact streams remain in the operation log.]"
	if remaining > len(marker) {
		output.WriteString(value[:remaining-len(marker)])
		output.WriteString(marker)
	}
	*omitted = true
}

func runCapturedKopiaPolicyOutput(
	ctx context.Context,
	session *kopiaRepositorySession,
	args []string,
	parse func(io.Reader) error,
) (string, error) {
	capture, output, err := session.runCaptured(ctx, args, command.NoTotalDeadline)
	if capture != nil {
		defer capture.Close()
	}
	if err != nil {
		return output, err
	}
	stdout, openErr := capture.OpenStdout()
	if openErr != nil {
		return output, &command.OutputProcessingFailure{Err: openErr}
	}
	parseErr := parse(stdout)
	_ = stdout.Close()
	if parseErr != nil {
		return output, &command.OutputProcessingFailure{Err: parseErr}
	}
	return output, nil
}

func strictJSONReader(reader io.Reader, target any) error {
	decoder := json.NewDecoder(reader)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON")
		}
		return err
	}
	return nil
}

func readKopiaPolicyDocument(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximumKopiaPolicyDocumentBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maximumKopiaPolicyDocumentBytes {
		return nil, fmt.Errorf("Kopia policy document exceeds the 8 MiB protocol-record limit")
	}
	return data, nil
}

// KopiaManagedPolicySource is the only job-specific native policy Replicaro
// owns. Source is an absolute local path; Excludes are Kopia ignore patterns.
type KopiaManagedPolicySource struct {
	JobUUID     string   `json:"job_uuid"`
	Source      string   `json:"source"`
	Excludes    []string `json:"excludes"`
	KeepLatest  int      `json:"keep_latest"`
	KeepHourly  int      `json:"keep_hourly"`
	KeepDaily   int      `json:"keep_daily"`
	KeepWeekly  int      `json:"keep_weekly"`
	KeepMonthly int      `json:"keep_monthly"`
	KeepAnnual  int      `json:"keep_annual"`
}

// KopiaManagedPolicyDesiredState is intentionally Kopia-specific. It is not a
// generalized engine lifecycle contract.
type KopiaManagedPolicyDesiredState struct {
	Version             int                        `json:"version"`
	MaintenanceSchedule string                     `json:"maintenance_schedule"`
	ObjectLock          models.ObjectLockSettings  `json:"object_lock"`
	Sources             []KopiaManagedPolicySource `json:"sources"`
}

type KopiaManagedPolicyResult struct {
	DesiredDigest string `json:"desiredDigest"`
	Mutated       bool   `json:"mutated"`
	DriftObserved bool   `json:"driftObserved"`
}

type kopiaPolicyListEntry struct {
	ID     string `json:"id"`
	Target struct {
		Host     string `json:"host"`
		UserName string `json:"userName"`
		Path     string `json:"path"`
	} `json:"target"`
	Retention           json.RawMessage `json:"retention"`
	Files               json.RawMessage `json:"files"`
	ErrorHandling       json.RawMessage `json:"errorHandling"`
	Scheduling          json.RawMessage `json:"scheduling"`
	Compression         json.RawMessage `json:"compression"`
	MetadataCompression json.RawMessage `json:"metadataCompression"`
	Splitter            json.RawMessage `json:"splitter"`
	Actions             json.RawMessage `json:"actions"`
	OSSnapshots         json.RawMessage `json:"osSnapshots"`
	Logging             json.RawMessage `json:"logging"`
	Upload              json.RawMessage `json:"upload"`
}

type kopiaMaintenancePolicy struct {
	Owner string `json:"owner"`
	Quick *struct {
		Enabled  bool  `json:"enabled"`
		Interval int64 `json:"interval"`
	} `json:"quick"`
	Full *struct {
		Enabled  bool  `json:"enabled"`
		Interval int64 `json:"interval"`
	} `json:"full"`
	LogRetention      json.RawMessage `json:"logRetention"`
	ExtendObjectLocks bool            `json:"extendObjectLocks"`
	ListParallelism   int             `json:"listParallelism"`
	Schedule          json.RawMessage `json:"schedule"`
}

type kopiaRepositoryStatus struct {
	ConfigFile    string `json:"configFile"`
	UniqueIDHex   string `json:"uniqueIDHex"`
	ClientOptions struct {
		Hostname                string `json:"hostname"`
		Username                string `json:"username"`
		Description             string `json:"description"`
		EnableActions           bool   `json:"enableActions"`
		FormatBlobCacheDuration int64  `json:"formatBlobCacheDuration"`
	} `json:"clientOptions"`
	Storage       json.RawMessage `json:"storage"`
	Volume        json.RawMessage `json:"volume"`
	ContentFormat json.RawMessage `json:"contentFormat"`
	ObjectFormat  json.RawMessage `json:"objectFormat"`
	BlobRetention json.RawMessage `json:"blobRetention"`
}

func verifyKopiaRepositoryStatusIdentity(status string, repo models.Repository) error {
	if repo.NativeRepositoryID == "" {
		return nil
	}
	var repositoryStatus kopiaRepositoryStatus
	if err := strictJSON([]byte(status), &repositoryStatus); err != nil {
		return fmt.Errorf("decode Kopia repository status: %w", err)
	}
	decoded, err := hex.DecodeString(repositoryStatus.UniqueIDHex)
	if err != nil || len(decoded) != sha256.Size ||
		repositoryStatus.UniqueIDHex != strings.ToLower(repositoryStatus.UniqueIDHex) ||
		repositoryStatus.UniqueIDHex != repo.NativeRepositoryID {
		return fmt.Errorf("Kopia configuration is bound to a different native repository")
	}
	return nil
}

type kopiaPolicySnapshot struct {
	ClientUser  string
	ClientHost  string
	Retention   json.RawMessage
	Maintenance json.RawMessage
	Global      json.RawMessage
	Policies    map[string]json.RawMessage
}

func NormalizeKopiaManagedPolicyDesired(value KopiaManagedPolicyDesiredState) (KopiaManagedPolicyDesiredState, error) {
	result := KopiaManagedPolicyDesiredState{Version: 1, MaintenanceSchedule: value.MaintenanceSchedule,
		ObjectLock: value.ObjectLock}
	if value.ObjectLock.Enrolled {
		if _, err := value.ObjectLock.DurationHours(); err != nil {
			return result, err
		}
		if !models.ObjectLockMaintenanceEligible(value.ObjectLock, value.MaintenanceSchedule) {
			return result, fmt.Errorf("space reclamation schedule is incompatible with object lock duration")
		}
	} else {
		// Unenrolled repositories stay outside this feature entirely. In
		// particular, importing a native repository without enrolling must not
		// cause Replicaro to disable or reinterpret native-only retention state.
		result.MaintenanceSchedule = ""
	}
	byJob := map[string]KopiaManagedPolicySource{}
	for _, source := range value.Sources {
		jobUUID := strings.TrimSpace(source.JobUUID)
		if parsed, err := uuid.Parse(jobUUID); err != nil || parsed == uuid.Nil || parsed.String() != jobUUID {
			return result, fmt.Errorf("Kopia job_uuid is invalid")
		}
		// This exact configured source becomes native SourceInfo scope. TrimSpace
		// or early Clean would redirect both backup traversal and retention policy.
		absolute, err := storageidentity.NormalizeConfiguredPath(source.Source)
		if err != nil {
			return result, fmt.Errorf("canonicalize Kopia source: %w", err)
		}
		absolute = filepath.Clean(absolute)
		if absolute == "" {
			return result, fmt.Errorf("Kopia source path is required")
		}
		excludes := normalizedKopiaExcludes(source.Excludes)
		sort.Strings(excludes)
		excludes = compactStrings(excludes)
		values := []int{source.KeepLatest, source.KeepHourly, source.KeepDaily,
			source.KeepWeekly, source.KeepMonthly, source.KeepAnnual}
		for _, value := range values {
			if value < 0 {
				return result, fmt.Errorf("Kopia retention values cannot be negative")
			}
		}
		// Keep all is represented by six explicit zeros so native defaults can
		// never leak through an inherited field.
		if source.KeepLatest == 0 {
			source.KeepHourly, source.KeepDaily, source.KeepWeekly = 0, 0, 0
			source.KeepMonthly, source.KeepAnnual = 0, 0
		}
		normalizedSource := KopiaManagedPolicySource{
			JobUUID: jobUUID, Source: absolute, Excludes: excludes,
			KeepLatest: source.KeepLatest, KeepHourly: source.KeepHourly,
			KeepDaily: source.KeepDaily, KeepWeekly: source.KeepWeekly,
			KeepMonthly: source.KeepMonthly, KeepAnnual: source.KeepAnnual,
		}
		if existing, ok := byJob[jobUUID]; ok && !equalKopiaManagedSource(existing, normalizedSource) {
			return result, fmt.Errorf("conflicting Kopia policy for job %q", jobUUID)
		}
		byJob[jobUUID] = normalizedSource
	}
	jobs := make([]string, 0, len(byJob))
	for jobUUID := range byJob {
		jobs = append(jobs, jobUUID)
	}
	sort.Strings(jobs)
	for _, jobUUID := range jobs {
		entry := byJob[jobUUID]
		entry.Excludes = append([]string(nil), entry.Excludes...)
		result.Sources = append(result.Sources, entry)
	}
	return result, nil
}

func equalKopiaManagedSource(left, right KopiaManagedPolicySource) bool {
	return left.JobUUID == right.JobUUID && left.Source == right.Source &&
		equalStrings(left.Excludes, right.Excludes) && left.KeepLatest == right.KeepLatest &&
		left.KeepHourly == right.KeepHourly && left.KeepDaily == right.KeepDaily &&
		left.KeepWeekly == right.KeepWeekly && left.KeepMonthly == right.KeepMonthly &&
		left.KeepAnnual == right.KeepAnnual
}

func KopiaManagedPolicyDigest(value KopiaManagedPolicyDesiredState) (string, error) {
	normalized, err := NormalizeKopiaManagedPolicyDesired(value)
	if err != nil {
		return "", err
	}
	document := struct {
		Version             int                        `json:"version"`
		Manual              bool                       `json:"manual"`
		Compression         string                     `json:"compression"`
		Retention           int                        `json:"retention"`
		Quick               bool                       `json:"quick"`
		Full                bool                       `json:"full"`
		MaintenanceSchedule string                     `json:"maintenance_schedule"`
		ObjectLock          models.ObjectLockSettings  `json:"object_lock"`
		Sources             []KopiaManagedPolicySource `json:"sources"`
	}{
		Version: 1, Manual: true, Compression: "zstd",
		Retention: KopiaManagedRetentionCeiling, Quick: false, Full: false,
		MaintenanceSchedule: normalized.MaintenanceSchedule, ObjectLock: normalized.ObjectLock,
		Sources: normalized.Sources,
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

func ReconcileKopiaManagedPolicies(
	ctx context.Context,
	engine Engine,
	repo models.Repository,
	desired KopiaManagedPolicyDesiredState,
) (KopiaManagedPolicyResult, string, error) {
	target, check, err := concreteKopiaEngine(engine)
	if err != nil {
		return KopiaManagedPolicyResult{}, "", err
	}
	ctx = repositoryProcessContext(ctx, repo, check)
	result, output, err := target.reconcileManagedPolicies(ctx, repo, desired)
	return result, output, err
}

func concreteKopiaEngine(engine Engine) (*kopiaEngine, RepositoryAvailabilityCheck, error) {
	var check RepositoryAvailabilityCheck
	if public, ok := engine.(*publicEngine); ok {
		check = public.availabilityCheck
		engine = public.Engine
	}
	target, ok := engine.(*kopiaEngine)
	if !ok {
		return nil, nil, fmt.Errorf("Kopia managed-policy reconciliation requires the Kopia engine")
	}
	return target, check, nil
}

// EnsureKopiaClientIdentity assigns and reads back the stable Replicaro client
// identity in this vault's isolated Kopia configuration.
func EnsureKopiaClientIdentity(ctx context.Context, engine Engine, repo models.Repository) (string, error) {
	target, check, err := concreteKopiaEngine(engine)
	if err != nil {
		return "", err
	}
	if parsed, parseErr := uuid.Parse(repo.ClientUUID); parseErr != nil || parsed == uuid.Nil || parsed.String() != repo.ClientUUID {
		return "", fmt.Errorf("client_uuid is invalid")
	}
	ctx = repositoryProcessContext(ctx, repo, check)
	session, _, err := target.openRepositorySession(ctx, repo, false)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	status, err := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return status, err
	}
	user, host, err := kopiaClientIdentity(status)
	if err != nil {
		return status, err
	}
	var repositoryStatus kopiaRepositoryStatus
	if err := strictJSON([]byte(status), &repositoryStatus); err != nil {
		return status, fmt.Errorf("decode Kopia repository status: %w", err)
	}
	if err := verifyKopiaRepositoryStatusIdentity(status, repo); err != nil {
		return status, err
	}
	disableFormatCache := repo.ObjectLock.Enrolled && repositoryStatus.ClientOptions.FormatBlobCacheDuration != -1
	if user != repo.ClientUUID || host != "replicaro" || disableFormatCache {
		args := []string{"repository", "set-client", "--username", repo.ClientUUID, "--hostname", "replicaro"}
		if repo.ObjectLock.Enrolled {
			// The repository format blob contains provider-retention settings.
			// Every client of an enrolled vault must bypass stale local copies so
			// pause/resume and monotonic changes take effect at the native boundary.
			// Unenrolled vaults keep Kopia's normal cache because this safeguard has
			// no Object Lock correctness benefit there.
			args = append(args, "--disable-repository-format-cache")
		}
		value, setErr := session.run(ctx, args, command.NoTotalDeadline)
		output.WriteString(value)
		if setErr != nil {
			return output.String(), setErr
		}
	}
	verified, err := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if output.Len() > 0 && strings.TrimSpace(verified) != "" {
		output.WriteByte('\n')
	}
	output.WriteString(verified)
	if err != nil {
		return output.String(), err
	}
	// The closing status is an identity admission, not just confirmation that
	// set-client persisted. A repository can be replaced while that mutation is
	// running; accepting client fields from the replacement would attach the
	// reviewed vault metadata to different native repository bytes.
	if err := verifyKopiaRepositoryStatusIdentity(verified, repo); err != nil {
		return output.String(), err
	}
	user, host, err = kopiaClientIdentity(verified)
	if err != nil || user != repo.ClientUUID || host != "replicaro" {
		return output.String(), fmt.Errorf("Kopia client identity readback did not match %s@replicaro", repo.ClientUUID)
	}
	if repo.ObjectLock.Enrolled {
		if err := strictJSON([]byte(verified), &repositoryStatus); err != nil {
			return output.String(), fmt.Errorf("decode Kopia repository status: %w", err)
		}
		if repositoryStatus.ClientOptions.FormatBlobCacheDuration != -1 {
			return output.String(), fmt.Errorf("Kopia repository format cache remained enabled for an Object Lock vault")
		}
	}
	return output.String(), nil
}

// ExplainKopiaObjectLockProviderRequirement translates only recognizable
// provider prerequisite failures. Generic retention/readback errors stay
// native so an internal mismatch is never mislabeled as bucket configuration.
func ExplainKopiaObjectLockProviderRequirement(connector string, err error) error {
	if err == nil {
		return nil
	}
	lower := strings.ToLower(err.Error())
	patterns := []string{
		"objectlockconfigurationnotfound",
		"object lock configuration is not enabled",
		"object locking is not enabled",
		"object retention is not enabled",
		"versioning must be enabled",
		"unsupported put-blob option",
		"version-level immutability",
	}
	matched := false
	for _, pattern := range patterns {
		if strings.Contains(lower, pattern) {
			matched = true
			break
		}
	}
	if !matched {
		return err
	}
	var guidance string
	switch connector {
	case "s3":
		guidance = "Object Lock is not enabled for this bucket. Turn on S3 Object Lock and versioning for the bucket, then try again."
	case "azblob":
		guidance = "Object Lock is not enabled for this container. Turn on blob versioning and version-level immutability for the storage account, then try again."
	case "gcs":
		guidance = "Object Lock is not enabled for this bucket. Turn on Object Versioning and Object Retention for the bucket, then try again."
	default:
		return err
	}
	return fmt.Errorf("%s Native Kopia details: %w", guidance, err)
}

// EnsureKopiaMaintenanceOwner aligns native maintenance with the freshly
// authorized Replicaro owner and keeps Kopia automatic maintenance disabled.
func EnsureKopiaMaintenanceOwner(ctx context.Context, engine Engine, repo models.Repository) (string, error) {
	target, check, err := concreteKopiaEngine(engine)
	if err != nil {
		return "", err
	}
	ctx = repositoryProcessContext(ctx, repo, check)
	session, _, err := target.openRepositorySession(ctx, repo, false)
	if err != nil {
		return "", err
	}
	expected := expectedKopiaMaintenanceOwner(repo)
	concurrency, err := kopiaConcurrency(repo)
	if err != nil {
		return "", err
	}
	// List parallelism is repository-wide Kopia maintenance state. Only the
	// freshly authorized vault owner may align it to this computer's profile.
	args := []string{"maintenance", "set", "--owner", expected, "--enable-quick=false", "--enable-full=false",
		"--list-parallelism", fmt.Sprint(concurrency.maintenanceList)}
	if repo.ObjectLock.Enrolled {
		args, err = kopiaObjectLockMaintenanceArgs(repo, !repo.ObjectLock.Paused)
		if err != nil {
			return "", err
		}
		args = append(args, "--list-parallelism", fmt.Sprint(concurrency.maintenanceList))
	}
	// Native identity is engine preparation. The protected owner/intent callback
	// below must run after it and remain the final admission before maintenance
	// set, with no native child in between.
	status, err := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return status, err
	}
	if err := verifyKopiaRepositoryStatusIdentity(status, repo); err != nil {
		return status, err
	}
	if err := admitKopiaMaintenanceMutation(ctx); err != nil {
		return "", err
	}
	value, err := session.run(ctx, args, command.NoTotalDeadline)
	if err != nil {
		return value, err
	}
	verified, verifyErr := session.run(ctx, []string{"maintenance", "info", "--json"}, command.NoTotalDeadline)
	output := strings.TrimSpace(value + "\n" + verified)
	if verifyErr != nil {
		return output, verifyErr
	}
	if err := verifyKopiaMaintenanceConfiguration([]byte(verified), repo, expected, true); err != nil {
		return output, err
	}
	status, err = session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return output, err
	}
	if err := verifyKopiaRepositoryStatusIdentity(status, repo); err != nil {
		return output, err
	}
	return output, nil
}

// ConfigureKopiaObjectLockForEnrollment is limited to the first unmanaged
// import, where no protected Replicaro root exists yet to authorize through.
// The connection intent and managed vault UUID lock are the durable boundary;
// ordinary edits must go through the reconciler's fresh root-owner admission.
func ConfigureKopiaObjectLockForEnrollment(ctx context.Context, engine Engine, repo models.Repository) (string, error) {
	if !repo.ObjectLock.Enrolled || repo.ObjectLock.Paused {
		return "", fmt.Errorf("initial object lock enrollment must be active")
	}
	target, check, err := concreteKopiaEngine(engine)
	if err != nil {
		return "", err
	}
	ctx = repositoryProcessContext(ctx, repo, check)
	session, _, err := target.openRepositorySession(ctx, repo, false)
	if err != nil {
		return "", err
	}
	var output strings.Builder
	// Initial enrollment has no protected root, so its immutable connection
	// intent/profile callback is the Object Lock mutation authority. Let the
	// shared helper invoke it after its last native-identity preparation and
	// immediately before the first provider-wide command.
	ctx = ContextWithKopiaObjectLockMutationAdmission(ctx, func(admissionContext context.Context) error {
		return admitKopiaMaintenanceMutation(admissionContext)
	})
	if err := applyKopiaObjectLockSettings(ctx, session, repo, true, func(value string) {
		if output.Len() > 0 && strings.TrimSpace(value) != "" {
			output.WriteByte('\n')
		}
		output.WriteString(value)
	}); err != nil {
		return output.String(), err
	}
	maintenance, err := session.run(ctx, []string{"maintenance", "info", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return output.String(), err
	}
	// The enrollment readback spans repository and maintenance state. Close it
	// with status so replacement during maintenance info cannot authorize root
	// publication for different native repository bytes.
	status, err := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return output.String(), err
	}
	if err := verifyKopiaObjectLockEnrollmentReadback([]byte(status), []byte(maintenance), repo); err != nil {
		return output.String(), err
	}
	return output.String(), nil
}

func verifyKopiaObjectLockEnrollmentReadback(statusData, maintenanceData []byte, repo models.Repository) error {
	if err := verifyKopiaRepositoryStatusIdentity(string(statusData), repo); err != nil {
		return err
	}
	var repositoryStatus kopiaRepositoryStatus
	if err := strictJSON(statusData, &repositoryStatus); err != nil {
		return err
	}
	snapshot := kopiaPolicySnapshot{Retention: repositoryStatus.BlobRetention, Maintenance: json.RawMessage(maintenanceData)}
	desired := KopiaManagedPolicyDesiredState{Version: 1, MaintenanceSchedule: repo.MaintenanceSchedule, ObjectLock: repo.ObjectLock}
	expectedOwner := expectedKopiaMaintenanceOwner(repo)
	if !kopiaObjectLockMatches(snapshot, desired, expectedOwner) {
		return fmt.Errorf("Kopia object lock enrollment readback did not match")
	}
	// Enrollment also writes list-parallelism in the same maintenance set.
	// Require the exact profile-derived value before considering the unmanaged
	// import converted to Replicaro-owned repository-wide maintenance state.
	if err := verifyKopiaMaintenanceConfiguration(maintenanceData, repo, expectedOwner, true); err != nil {
		return fmt.Errorf("Kopia object lock enrollment readback did not match: %w", err)
	}
	return nil
}

func VerifyKopiaMaintenanceOwner(ctx context.Context, engine Engine, repo models.Repository) error {
	target, check, err := concreteKopiaEngine(engine)
	if err != nil {
		return err
	}
	ctx = repositoryProcessContext(ctx, repo, check)
	session, _, err := target.openRepositorySession(ctx, repo, false)
	if err != nil {
		return err
	}
	status, err := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return err
	}
	if err := verifyKopiaRepositoryStatusIdentity(status, repo); err != nil {
		return err
	}
	value, err := session.run(ctx, []string{"maintenance", "info", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return err
	}
	// Maintenance ownership is used as a final connection/scheduler admission.
	// Close the read-only sequence with the same native identity so a repository
	// replacement between status and maintenance info cannot authorize later
	// publication or owner-only work.
	status, err = session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return err
	}
	if err := verifyKopiaRepositoryStatusIdentity(status, repo); err != nil {
		return err
	}
	return verifyKopiaMaintenanceConfiguration([]byte(value), repo, expectedKopiaMaintenanceOwner(repo), false)
}

// DeleteKopiaManagedJobPolicy deletes only Replicaro's exact job user policy.
// Native path artifacts and every foreign namespace are read-only here.
func DeleteKopiaManagedJobPolicy(ctx context.Context, engine Engine, repo models.Repository, jobUUID, source string) (string, error) {
	desired, err := NormalizeKopiaManagedPolicyDesired(KopiaManagedPolicyDesiredState{Version: 1,
		Sources: []KopiaManagedPolicySource{{JobUUID: jobUUID, Source: source}}})
	if err != nil {
		return "", err
	}
	target, check, err := concreteKopiaEngine(engine)
	if err != nil {
		return "", err
	}
	ctx = repositoryProcessContext(ctx, repo, check)
	before, err := target.inspectManagedPoliciesScoped(ctx, repo, desired.Sources)
	if err != nil {
		return "", err
	}
	if before.ClientUser != repo.ClientUUID || before.ClientHost != "replicaro" {
		return "", fmt.Errorf("Kopia client identity is not %s@replicaro", repo.ClientUUID)
	}
	if !globalPolicyMatches(before.Global) {
		return "", fmt.Errorf("Kopia global policy is not the managed retention safety ceiling")
	}
	session, _, err := target.openRepositorySession(ctx, repo, false)
	if err != nil {
		return "", err
	}
	if err := validateKopiaPathArtifacts(before.Policies, desired.Sources); err != nil {
		return "", err
	}
	policyTarget := jobUUID + "@replicaro"
	managed := desired.Sources[0]
	raw, exists := before.Policies[policyTarget]
	if !exists {
		// A retry after a conclusive delete validates that SourceInfo now
		// inherits the managed global ceiling instead of recreating job policy.
		pathOutput, pathErr := validateEffectiveKopiaPathArtifacts(ctx, session, before.Policies, desired.Sources,
			map[string]KopiaManagedPolicySource{policyTarget: {
				KeepLatest: KopiaManagedRetentionCeiling, KeepHourly: KopiaManagedRetentionCeiling,
				KeepDaily: KopiaManagedRetentionCeiling, KeepWeekly: KopiaManagedRetentionCeiling,
				KeepMonthly: KopiaManagedRetentionCeiling, KeepAnnual: KopiaManagedRetentionCeiling,
			}})
		if pathErr != nil {
			return pathOutput, pathErr
		}
		status, statusErr := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
		if statusErr != nil {
			return pathOutput, statusErr
		}
		if statusErr := verifyKopiaRepositoryStatusIdentity(status, repo); statusErr != nil {
			return pathOutput, statusErr
		}
		return pathOutput, nil
	}
	{
		excludes, retention, ok := localSourcePolicyValues(raw)
		if !ok {
			return "", fmt.Errorf("Kopia job policy %s is not the exact managed user policy", policyTarget)
		}
		managed.Excludes = excludes
		managed.KeepLatest, managed.KeepHourly = retention.KeepLatest, retention.KeepHourly
		managed.KeepDaily, managed.KeepWeekly = retention.KeepDaily, retention.KeepWeekly
		managed.KeepMonthly, managed.KeepAnnual = retention.KeepMonthly, retention.KeepAnnual
	}
	pathOutput, err := validateEffectiveKopiaPathArtifacts(ctx, session, before.Policies, desired.Sources,
		map[string]KopiaManagedPolicySource{policyTarget: managed})
	if err != nil {
		return pathOutput, err
	}
	status, err := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return pathOutput, err
	}
	if err := verifyKopiaRepositoryStatusIdentity(status, repo); err != nil {
		return pathOutput, err
	}
	output, err := session.run(ctx, []string{"policy", "delete", policyTarget}, command.NoTotalDeadline)
	output = joinKopiaPolicyOutput(pathOutput, output)
	if err != nil {
		return output, err
	}
	after, err := target.inspectManagedPoliciesScoped(ctx, repo, desired.Sources)
	if err != nil {
		return output, err
	}
	if _, exists := after.Policies[policyTarget]; exists {
		return output, fmt.Errorf("Kopia job policy deletion readback still contains %s", policyTarget)
	}
	if !globalPolicyMatches(after.Global) {
		return output, fmt.Errorf("Kopia global policy changed before deletion readback completed")
	}
	if err := validateKopiaPathArtifacts(after.Policies, desired.Sources); err != nil {
		return output, err
	}
	pathOutput, err = validateEffectiveKopiaPathArtifacts(ctx, session, after.Policies, desired.Sources,
		map[string]KopiaManagedPolicySource{policyTarget: {
			KeepLatest: KopiaManagedRetentionCeiling, KeepHourly: KopiaManagedRetentionCeiling,
			KeepDaily: KopiaManagedRetentionCeiling, KeepWeekly: KopiaManagedRetentionCeiling,
			KeepMonthly: KopiaManagedRetentionCeiling, KeepAnnual: KopiaManagedRetentionCeiling,
		}})
	output = joinKopiaPolicyOutput(output, pathOutput)
	if err != nil {
		return output, err
	}
	status, err = session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return output, err
	}
	if err := verifyKopiaRepositoryStatusIdentity(status, repo); err != nil {
		return output, err
	}
	return output, nil
}

func joinKopiaPolicyOutput(left, right string) string {
	left, right = strings.TrimSpace(left), strings.TrimSpace(right)
	if left == "" {
		return right
	}
	if right == "" {
		return left
	}
	return left + "\n" + right
}

type kopiaObjectLockMutationAdmissionKey struct{}

// ContextWithKopiaObjectLockMutationAdmission keeps provider-wide mutation at
// the exact owner-authorized boundary without teaching the engine adapter how
// to read Replicaro's recovery sidecar. Ordinary job-policy reconciliation can
// still run on non-owner attachments when the protected repository settings
// already match.
func ContextWithKopiaObjectLockMutationAdmission(ctx context.Context, admit func(context.Context) error) context.Context {
	return context.WithValue(ctx, kopiaObjectLockMutationAdmissionKey{}, admit)
}

func admitKopiaObjectLockMutation(ctx context.Context) error {
	admit, _ := ctx.Value(kopiaObjectLockMutationAdmissionKey{}).(func(context.Context) error)
	if admit == nil {
		return fmt.Errorf("Kopia object lock mutation lacks vault-owner authorization")
	}
	return admit(ctx)
}

type kopiaBlobRetention struct {
	Mode   string        `json:"retentionMode,omitempty"`
	Period time.Duration `json:"retentionPeriod,omitempty"`
}

func kopiaObjectLockMode(settings models.ObjectLockSettings) string {
	return strings.ToUpper(settings.Mode)
}

func expectedKopiaMaintenanceOwner(repo models.Repository) string {
	clientUUID := repo.NativeMaintenanceOwnerClientUUID
	if clientUUID == "" {
		clientUUID = repo.ClientUUID
	}
	return clientUUID + "@replicaro"
}

const kopiaPausedManualFullInterval = time.Hour

func kopiaObjectLockMaintenanceInterval(settings models.ObjectLockSettings, schedule string) (time.Duration, bool) {
	if interval, valid := models.ObjectLockMaintenanceInterval(schedule); valid {
		return interval, true
	}
	if !settings.Enrolled || !settings.Paused || schedule != "manual" {
		return 0, false
	}
	// Kopia's complete maintenance command still requires --full-interval even
	// when both native schedulers and lock extension are disabled. One hour is
	// inert in that state and deliberately distinct from every supported
	// automatic schedule, so Manual<->automatic changes cannot disappear as
	// native-equivalent no-ops. Resume replaces it with the eligible interval.
	return kopiaPausedManualFullInterval, true
}

func kopiaObjectLockMaintenanceArgs(repo models.Repository, extend bool) ([]string, error) {
	interval, valid := kopiaObjectLockMaintenanceInterval(repo.ObjectLock, repo.MaintenanceSchedule)
	if !valid {
		return nil, fmt.Errorf("object lock requires a valid space reclamation interval")
	}
	// --full-interval does not enable Kopia's scheduler. Kopia uses the stored
	// interval to validate its 24-hour lock-extension margin, while Replicaro
	// continues to own every full-maintenance launch.
	return []string{"maintenance", "set", "--owner", expectedKopiaMaintenanceOwner(repo),
		"--enable-quick=false", "--enable-full=false", "--full-interval", interval.String(),
		"--extend-object-locks=" + fmt.Sprint(extend)}, nil
}

func kopiaObjectLockMatches(snapshot kopiaPolicySnapshot, desired KopiaManagedPolicyDesiredState, expectedOwner string) bool {
	if !desired.ObjectLock.Enrolled {
		return true
	}
	var retention kopiaBlobRetention
	if strictJSON(snapshot.Retention, &retention) != nil {
		return false
	}
	var maintenance kopiaMaintenancePolicy
	if strictJSON(snapshot.Maintenance, &maintenance) != nil || maintenance.Quick == nil || maintenance.Full == nil {
		return false
	}
	interval, valid := kopiaObjectLockMaintenanceInterval(desired.ObjectLock, desired.MaintenanceSchedule)
	if !valid || maintenance.Owner != expectedOwner || maintenance.Quick.Enabled || maintenance.Full.Enabled ||
		maintenance.Full.Interval != int64(interval) || maintenance.ExtendObjectLocks != !desired.ObjectLock.Paused {
		return false
	}
	if desired.ObjectLock.Paused {
		return retention.Mode == "" && retention.Period == 0
	}
	hours, err := desired.ObjectLock.DurationHours()
	return err == nil && retention.Mode == kopiaObjectLockMode(desired.ObjectLock) &&
		retention.Period == time.Duration(hours)*time.Hour
}

func applyKopiaObjectLockSettings(
	ctx context.Context,
	session *kopiaRepositorySession,
	repo models.Repository,
	includeListParallelism bool,
	output func(string),
) error {
	disableExtensionArgs, err := kopiaObjectLockMaintenanceArgs(repo, false)
	if err != nil {
		return err
	}
	var concurrency kopiaConcurrencySettings
	if includeListParallelism {
		concurrency, err = kopiaConcurrency(repo)
		if err != nil {
			return err
		}
		// Initial enrollment has no protected root yet. Its connection intent
		// authorizes this one profile-derived repository-wide maintenance value;
		// ordinary Object Lock reconciliation leaves it to owner-only maintenance
		// alignment so a nonowner's local profile setting cannot create drift.
		disableExtensionArgs = append(disableExtensionArgs, "--list-parallelism", fmt.Sprint(concurrency.maintenanceList))
	}
	retentionArgs := []string{"repository", "set-parameters"}
	if repo.ObjectLock.Paused {
		retentionArgs = append(retentionArgs, "--retention-mode", "none")
	} else {
		period, err := repo.ObjectLock.NativeRetentionPeriod()
		if err != nil {
			return err
		}
		retentionArgs = append(retentionArgs, "--retention-mode", kopiaObjectLockMode(repo.ObjectLock),
			"--retention-period", period)
	}
	finalMaintenanceArgs, err := kopiaObjectLockMaintenanceArgs(repo, !repo.ObjectLock.Paused)
	if err != nil {
		return err
	}
	if includeListParallelism {
		finalMaintenanceArgs = append(finalMaintenanceArgs, "--list-parallelism", fmt.Sprint(concurrency.maintenanceList))
	}
	// Native identity is preparation; protected owner/intent authorization must
	// follow it and remain the final admission before the first mutation.
	status, err := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return err
	}
	if err := verifyKopiaRepositoryStatusIdentity(status, repo); err != nil {
		return err
	}
	if err := admitKopiaObjectLockMutation(ctx); err != nil {
		return err
	}
	if !repo.ObjectLock.Paused {
		// Kopia validates the retention period against the currently stored full
		// interval whenever extension is enabled. Disable extension with the full
		// managed command first so changing either value works safely in both
		// directions; backup admission remains closed until final readback.
		value, runErr := session.run(ctx, disableExtensionArgs, command.NoTotalDeadline)
		output(value)
		if runErr != nil {
			return runErr
		}
	}
	value, err := session.run(ctx, retentionArgs, command.NoTotalDeadline)
	output(value)
	if err != nil {
		return err
	}
	// Pausing deliberately sends the complete managed command too. Depending on
	// omitted native fields would make pause subtly inherit externally changed
	// owner or scheduler settings instead of reasserting Replicaro's contract.
	value, err = session.run(ctx, finalMaintenanceArgs, command.NoTotalDeadline)
	output(value)
	return err
}

func verifyKopiaMaintenanceOwner(data []byte, expected string) error {
	return verifyKopiaMaintenanceConfiguration(data, models.Repository{}, expected, false)
}

func verifyKopiaMaintenanceConfiguration(data []byte, repo models.Repository, expected string, verifyListParallelism bool) error {
	var maintenance kopiaMaintenancePolicy
	if err := strictJSON(data, &maintenance); err != nil {
		return fmt.Errorf("decode Kopia maintenance policy: %w", err)
	}
	if maintenance.Owner != expected || maintenance.Quick == nil || maintenance.Full == nil ||
		maintenance.Quick.Enabled || maintenance.Full.Enabled {
		return fmt.Errorf("Kopia maintenance owner or automatic-maintenance state did not match")
	}
	if verifyListParallelism {
		concurrency, err := kopiaConcurrency(repo)
		if err != nil {
			return err
		}
		if maintenance.ListParallelism != concurrency.maintenanceList {
			return fmt.Errorf("Kopia maintenance list parallelism did not match")
		}
	}
	if repo.ObjectLock.Enrolled {
		interval, valid := kopiaObjectLockMaintenanceInterval(repo.ObjectLock, repo.MaintenanceSchedule)
		if !valid || maintenance.Full.Interval != int64(interval) ||
			maintenance.ExtendObjectLocks != !repo.ObjectLock.Paused {
			return fmt.Errorf("Kopia object lock maintenance settings did not match")
		}
	}
	return nil
}

func (e *kopiaEngine) inspectManagedPolicies(ctx context.Context, repo models.Repository) (kopiaPolicySnapshot, error) {
	return e.inspectManagedPoliciesScoped(ctx, repo, nil)
}

func (e *kopiaEngine) inspectManagedPoliciesScoped(ctx context.Context, repo models.Repository, sources []KopiaManagedPolicySource) (kopiaPolicySnapshot, error) {
	session, _, err := e.openRepositorySession(ctx, repo, false)
	if err != nil {
		return kopiaPolicySnapshot{}, err
	}
	statusOutput, err := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return kopiaPolicySnapshot{}, fmt.Errorf("read Kopia client identity: %w", err)
	}
	if err := verifyKopiaRepositoryStatusIdentity(statusOutput, repo); err != nil {
		return kopiaPolicySnapshot{}, err
	}
	user, host, err := kopiaClientIdentity(statusOutput)
	if err != nil {
		return kopiaPolicySnapshot{}, err
	}
	maintenanceOutput, err := session.run(ctx, []string{"maintenance", "info", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return kopiaPolicySnapshot{}, fmt.Errorf("read Kopia maintenance policy: %w", err)
	}
	var maintenance kopiaMaintenancePolicy
	if err := strictJSON([]byte(maintenanceOutput), &maintenance); err != nil {
		return kopiaPolicySnapshot{}, fmt.Errorf("decode Kopia maintenance policy: %w", err)
	}
	if maintenance.Quick == nil || maintenance.Full == nil {
		return kopiaPolicySnapshot{}, fmt.Errorf("Kopia maintenance policy is missing quick or full state")
	}
	var entries []kopiaPolicyListEntry
	listOutput, err := runCapturedKopiaPolicyOutput(ctx, session, []string{"policy", "list", "--json"}, func(reader io.Reader) error {
		return strictJSONReader(reader, &entries)
	})
	if err != nil {
		return kopiaPolicySnapshot{}, fmt.Errorf("list Kopia policies: %w", err)
	}
	_ = listOutput
	targetNames, err := uniqueKopiaPolicyTargets(entries)
	if err != nil {
		return kopiaPolicySnapshot{}, err
	}
	var global json.RawMessage
	_, err = runCapturedKopiaPolicyOutput(ctx, session, []string{"policy", "export", "--global"}, func(reader io.Reader) error {
		data, readErr := readKopiaPolicyDocument(reader)
		if readErr != nil {
			return readErr
		}
		global, readErr = singleExportedPolicy(string(data), "(global)")
		return readErr
	})
	if err != nil {
		return kopiaPolicySnapshot{}, fmt.Errorf("export Kopia global policy: %w", err)
	}
	selectedJobs := make(map[string]string, len(sources))
	for _, source := range sources {
		selectedJobs[source.JobUUID] = filepath.Clean(source.Source)
	}
	selectedTargets := make([]string, 0, len(targetNames))
	for _, target := range targetNames {
		userHost, targetPath := target, ""
		if index := strings.Index(target, ":"); index >= 0 {
			userHost, targetPath = target[:index], target[index+1:]
		}
		if !strings.HasSuffix(userHost, "@replicaro") {
			continue
		}
		jobUUID := strings.TrimSuffix(userHost, "@replicaro")
		expectedPath, selected := selectedJobs[jobUUID]
		if !selected {
			continue
		}
		if targetPath == "" || filepath.Clean(targetPath) == expectedPath {
			selectedTargets = append(selectedTargets, target)
			continue
		}
		// A different path in this selected job namespace is locally
		// authoritative ambiguity and must be retained so readiness rejects it.
		selectedTargets = append(selectedTargets, target)
	}
	policies := make(map[string]json.RawMessage, len(selectedTargets))
	for _, target := range selectedTargets {
		var raw json.RawMessage
		_, exportErr := runCapturedKopiaPolicyOutput(ctx, session, []string{"policy", "export", target}, func(reader io.Reader) error {
			data, readErr := readKopiaPolicyDocument(reader)
			if readErr != nil {
				return readErr
			}
			raw, readErr = singleExportedPolicy(string(data), target)
			return readErr
		})
		if exportErr != nil {
			return kopiaPolicySnapshot{}, fmt.Errorf("export Kopia source policy: %w", exportErr)
		}
		policies[target] = raw
	}
	// A policy snapshot is an authorization/readiness record, so close the
	// multi-command read on the same native repository identity it opened with.
	closingStatus, err := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if err != nil {
		return kopiaPolicySnapshot{}, fmt.Errorf("close Kopia policy snapshot: %w", err)
	}
	if err := verifyKopiaRepositoryStatusIdentity(closingStatus, repo); err != nil {
		return kopiaPolicySnapshot{}, err
	}
	var status kopiaRepositoryStatus
	if err := strictJSON([]byte(statusOutput), &status); err != nil {
		return kopiaPolicySnapshot{}, fmt.Errorf("decode Kopia repository status: %w", err)
	}
	return kopiaPolicySnapshot{
		ClientUser: user, ClientHost: host, Retention: status.BlobRetention,
		Maintenance: json.RawMessage(maintenanceOutput),
		Global:      global, Policies: policies,
	}, nil
}

func uniqueKopiaPolicyTargets(entries []kopiaPolicyListEntry) ([]string, error) {
	globalCount := 0
	policyIDs := map[string]struct{}{}
	targets := map[string]struct{}{}
	comparisonTargets := map[string]struct{}{}
	for _, entry := range entries {
		policyID := strings.TrimSpace(entry.ID)
		if policyID == "" {
			return nil, fmt.Errorf("Kopia policy list contains an empty policy ID")
		}
		if _, duplicate := policyIDs[policyID]; duplicate {
			return nil, fmt.Errorf("Kopia policy list contains duplicate policy ID %q", policyID)
		}
		policyIDs[policyID] = struct{}{}
		emptyTarget := entry.Target.Host == "" && entry.Target.UserName == "" && entry.Target.Path == ""
		if emptyTarget {
			globalCount++
			continue
		}
		user := entry.Target.UserName
		host := entry.Target.Host
		targetPath := entry.Target.Path
		if strings.TrimSpace(user) == "" || strings.TrimSpace(host) == "" {
			return nil, fmt.Errorf("Kopia policy list contains an ambiguous target")
		}
		target := user + "@" + host
		if targetPath != "" {
			target += ":" + targetPath
		}
		if _, duplicate := targets[target]; duplicate {
			return nil, fmt.Errorf("Kopia policy list contains duplicate target %q", target)
		}
		// Native policy paths are exact scope, including whitespace and separators;
		// local filesystem cleanup would merge policies that Kopia kept distinct.
		comparisonTarget := strings.TrimSpace(user) + "@" + strings.TrimSpace(host)
		if targetPath != "" {
			comparisonTarget += ":" + targetPath
		}
		if _, duplicate := comparisonTargets[comparisonTarget]; duplicate {
			return nil, fmt.Errorf("Kopia policy list contains ambiguous duplicate target %q", target)
		}
		targets[target] = struct{}{}
		comparisonTargets[comparisonTarget] = struct{}{}
	}
	if globalCount != 1 {
		return nil, fmt.Errorf("Kopia policy list must contain exactly one global policy")
	}
	targetNames := make([]string, 0, len(targets))
	for target := range targets {
		targetNames = append(targetNames, target)
	}
	sort.Strings(targetNames)
	return targetNames, nil
}

func (e *kopiaEngine) reconcileManagedPolicies(
	ctx context.Context,
	repo models.Repository,
	desired KopiaManagedPolicyDesiredState,
) (KopiaManagedPolicyResult, string, error) {
	normalized, err := NormalizeKopiaManagedPolicyDesired(desired)
	if err != nil {
		return KopiaManagedPolicyResult{}, "", err
	}
	digest, err := KopiaManagedPolicyDigest(normalized)
	if err != nil {
		return KopiaManagedPolicyResult{}, "", err
	}
	mutated := false
	driftObserved := false
	currentResult := func() KopiaManagedPolicyResult {
		return KopiaManagedPolicyResult{
			DesiredDigest: digest, Mutated: mutated, DriftObserved: driftObserved,
		}
	}
	before, err := e.inspectManagedPoliciesScoped(ctx, repo, normalized.Sources)
	if err != nil {
		return currentResult(), "", err
	}
	desiredPolicies := map[string]KopiaManagedPolicySource{}
	if before.ClientUser != repo.ClientUUID || before.ClientHost != "replicaro" {
		return currentResult(), "", fmt.Errorf("Kopia client identity is not %s@replicaro", repo.ClientUUID)
	}
	for _, source := range normalized.Sources {
		target := source.JobUUID + "@replicaro"
		if existing, ok := desiredPolicies[target]; ok && !equalKopiaManagedSource(existing, source) {
			return currentResult(), "", fmt.Errorf("conflicting Kopia source policy target %q", target)
		}
		desiredPolicies[target] = source
	}
	if err := validateKopiaPathArtifacts(before.Policies, normalized.Sources); err != nil {
		return currentResult(), "", err
	}
	session, _, err := e.openRepositorySession(ctx, repo, false)
	if err != nil {
		return currentResult(), "", err
	}
	var output strings.Builder
	outputOmitted := false
	appendOutput := func(value string) {
		appendKopiaPolicyDiagnostic(&output, &outputOmitted, value)
	}
	var maintenance kopiaMaintenancePolicy
	if err := strictJSON(before.Maintenance, &maintenance); err != nil {
		return currentResult(), output.String(), err
	}
	if maintenance.Quick == nil || maintenance.Full == nil {
		return currentResult(), output.String(), fmt.Errorf("Kopia maintenance policy is missing quick or full state")
	}
	maintenanceDrift := maintenance.Quick.Enabled || maintenance.Full.Enabled
	objectLockDrift := !kopiaObjectLockMatches(before, normalized, expectedKopiaMaintenanceOwner(repo))
	globalDrift := !globalPolicyMatches(before.Global)
	policyDrift := false
	for target, source := range desiredPolicies {
		raw, exists := before.Policies[target]
		if !exists || !localSourcePolicyMatches(raw, source) {
			policyDrift = true
			break
		}
	}
	driftObserved = maintenanceDrift || objectLockDrift || globalDrift || policyDrift
	if driftObserved {
		// The snapshot above validated its own session, but reconciliation mutates
		// through this separately prepared session. Bind its exact native repository
		// immediately before the first command that can change policy.
		status, statusErr := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
		if statusErr != nil {
			return currentResult(), output.String(), statusErr
		}
		if statusErr := verifyKopiaRepositoryStatusIdentity(status, repo); statusErr != nil {
			return currentResult(), output.String(), statusErr
		}
	}
	if objectLockDrift {
		mutated = true
		repo.ObjectLock = normalized.ObjectLock
		repo.MaintenanceSchedule = normalized.MaintenanceSchedule
		if runErr := applyKopiaObjectLockSettings(ctx, session, repo, false, appendOutput); runErr != nil {
			return currentResult(), output.String(), runErr
		}
	} else if maintenanceDrift {
		mutated = true
		value, runErr := session.run(ctx, []string{"maintenance", "set", "--enable-quick=false", "--enable-full=false"}, command.NoTotalDeadline)
		appendOutput(value)
		if runErr != nil {
			return currentResult(), output.String(), runErr
		}
	}
	if globalDrift {
		mutated = true
		value, runErr := session.run(ctx, []string{"policy", "set", "--global", "--no-manual",
			"--snapshot-interval=0", "--snapshot-time=inherit", "--snapshot-time-crontab=inherit",
			"--run-missed=inherit"}, command.NoTotalDeadline)
		appendOutput(value)
		if runErr != nil {
			return currentResult(), output.String(), runErr
		}
		value, runErr = session.run(ctx, []string{"policy", "set", "--global", "--manual",
			"--compression=zstd", "--keep-latest=99999999", "--keep-hourly=99999999",
			"--keep-daily=99999999", "--keep-weekly=99999999", "--keep-monthly=99999999",
			"--keep-annual=99999999"}, command.NoTotalDeadline)
		appendOutput(value)
		if runErr != nil {
			return currentResult(), output.String(), runErr
		}
	}
	targets := make([]string, 0, len(desiredPolicies))
	for target := range desiredPolicies {
		targets = append(targets, target)
	}
	sort.Strings(targets)
	for _, target := range targets {
		if raw, exists := before.Policies[target]; exists && localSourcePolicyMatches(raw, desiredPolicies[target]) {
			continue
		}
		mutated = true
		source := desiredPolicies[target]
		value, runErr := session.run(ctx, []string{"policy", "set", "--clear-ignore",
			"--keep-latest=" + fmt.Sprint(source.KeepLatest),
			"--keep-hourly=" + fmt.Sprint(source.KeepHourly),
			"--keep-daily=" + fmt.Sprint(source.KeepDaily),
			"--keep-weekly=" + fmt.Sprint(source.KeepWeekly),
			"--keep-monthly=" + fmt.Sprint(source.KeepMonthly),
			"--keep-annual=" + fmt.Sprint(source.KeepAnnual), target}, command.NoTotalDeadline)
		appendOutput(value)
		if runErr != nil {
			return currentResult(), output.String(), runErr
		}
		if excludes := source.Excludes; len(excludes) > 0 {
			args := []string{"policy", "set"}
			for _, exclude := range excludes {
				args = append(args, "--add-ignore", exclude)
			}
			args = append(args, target)
			value, runErr = session.run(ctx, args, command.NoTotalDeadline)
			appendOutput(value)
			if runErr != nil {
				return currentResult(), output.String(), runErr
			}
		}
	}
	after, err := e.inspectManagedPoliciesScoped(ctx, repo, normalized.Sources)
	if err != nil {
		return currentResult(), output.String(), fmt.Errorf("verify Kopia managed policies: %w", err)
	}
	if err := verifyManagedPolicySnapshot(
		after, normalized, desiredPolicies, expectedKopiaMaintenanceOwner(repo),
	); err != nil {
		return currentResult(), output.String(), err
	}
	if err := validateKopiaPathArtifacts(after.Policies, normalized.Sources); err != nil {
		return currentResult(), output.String(), err
	}
	for _, target := range targets {
		var effectiveDocument []byte
		effectiveOutput, showErr := runCapturedKopiaPolicyOutput(ctx, session, []string{"policy", "show", "--json", target}, func(reader io.Reader) error {
			var readErr error
			effectiveDocument, readErr = readKopiaPolicyDocument(reader)
			return readErr
		})
		appendOutput(effectiveOutput)
		if showErr != nil {
			return currentResult(), output.String(),
				fmt.Errorf("read effective Kopia source policy %q: %w", target, showErr)
		}
		if !effectiveSourcePolicyMatches(effectiveDocument, desiredPolicies[target]) {
			return currentResult(), output.String(),
				fmt.Errorf("effective Kopia source policy did not inherit the managed values for %q", target)
		}
	}
	pathOutput, pathErr := validateEffectiveKopiaPathArtifacts(
		ctx, session, after.Policies, normalized.Sources, desiredPolicies,
	)
	appendOutput(pathOutput)
	if pathErr != nil {
		return currentResult(), output.String(), pathErr
	}
	status, statusErr := session.run(ctx, []string{"repository", "status", "--json"}, command.NoTotalDeadline)
	if statusErr != nil {
		return currentResult(), output.String(), statusErr
	}
	if statusErr := verifyKopiaRepositoryStatusIdentity(status, repo); statusErr != nil {
		return currentResult(), output.String(), statusErr
	}
	return currentResult(), output.String(), nil
}

func verifyManagedPolicySnapshot(
	snapshot kopiaPolicySnapshot,
	state KopiaManagedPolicyDesiredState,
	desired map[string]KopiaManagedPolicySource,
	expectedOwner string,
) error {
	var maintenance kopiaMaintenancePolicy
	if err := strictJSON(snapshot.Maintenance, &maintenance); err != nil {
		return err
	}
	if maintenance.Quick == nil || maintenance.Full == nil {
		return fmt.Errorf("Kopia maintenance policy is missing quick or full state")
	}
	if maintenance.Quick.Enabled || maintenance.Full.Enabled {
		return fmt.Errorf("Kopia automatic maintenance remained enabled")
	}
	if !kopiaObjectLockMatches(snapshot, state, expectedOwner) {
		return fmt.Errorf("Kopia object lock or maintenance readback did not match the desired state")
	}
	if !globalPolicyMatches(snapshot.Global) {
		return fmt.Errorf("Kopia global policy readback did not match the desired state")
	}
	for target, source := range desired {
		raw, ok := snapshot.Policies[target]
		if !ok || !localSourcePolicyMatches(raw, source) {
			return fmt.Errorf("Kopia source policy readback did not match target %q", target)
		}
	}
	return nil
}

func globalPolicyMatches(raw json.RawMessage) bool {
	var policy kopiaExportedPolicy
	if !knownKopiaPolicyDocument(raw) || json.Unmarshal(raw, &policy) != nil {
		return false
	}
	retention := policy.Retention
	return policy.Scheduling.Manual && policy.Compression.CompressorName == "zstd" &&
		intValue(retention.KeepLatest) == KopiaManagedRetentionCeiling &&
		intValue(retention.KeepHourly) == KopiaManagedRetentionCeiling &&
		intValue(retention.KeepDaily) == KopiaManagedRetentionCeiling &&
		intValue(retention.KeepWeekly) == KopiaManagedRetentionCeiling &&
		intValue(retention.KeepMonthly) == KopiaManagedRetentionCeiling &&
		intValue(retention.KeepAnnual) == KopiaManagedRetentionCeiling
}

func effectiveSourcePolicyMatches(raw json.RawMessage, source KopiaManagedPolicySource) bool {
	if !knownKopiaPolicyDocument(raw) {
		return false
	}
	var policy kopiaExportedPolicy
	if json.Unmarshal(raw, &policy) != nil {
		return false
	}
	retention := policy.Retention
	if !policy.Scheduling.Manual || policy.Compression.CompressorName != "zstd" ||
		retention.KeepLatest == nil || *retention.KeepLatest != source.KeepLatest ||
		retention.KeepHourly == nil || *retention.KeepHourly != source.KeepHourly ||
		retention.KeepDaily == nil || *retention.KeepDaily != source.KeepDaily ||
		retention.KeepWeekly == nil || *retention.KeepWeekly != source.KeepWeekly ||
		retention.KeepMonthly == nil || *retention.KeepMonthly != source.KeepMonthly ||
		retention.KeepAnnual == nil || *retention.KeepAnnual != source.KeepAnnual {
		return false
	}
	actual := normalizedKopiaExcludes(policy.Files.Ignore)
	sort.Strings(actual)
	expected := append([]string(nil), source.Excludes...)
	sort.Strings(expected)
	return equalStrings(actual, expected)
}

func knownKopiaPolicyDocument(raw json.RawMessage) bool {
	var document map[string]json.RawMessage
	if strictJSON(raw, &document) != nil {
		return false
	}
	allowed := map[string]bool{
		"retention": true, "scheduling": true, "files": true,
		"errorHandling": true, "compression": true, "metadataCompression": true,
		"splitter": true, "actions": true, "osSnapshots": true,
		"logging": true, "upload": true,
	}
	for key := range document {
		if !allowed[key] {
			return false
		}
	}
	return true
}

func localSourcePolicyMatches(raw json.RawMessage, source KopiaManagedPolicySource) bool {
	actual, retention, ok := localSourcePolicyValues(raw)
	if !ok {
		return false
	}
	expected := normalizedKopiaExcludes(source.Excludes)
	sort.Strings(expected)
	return equalStrings(actual, expected) && retention.KeepLatest == source.KeepLatest &&
		retention.KeepHourly == source.KeepHourly && retention.KeepDaily == source.KeepDaily &&
		retention.KeepWeekly == source.KeepWeekly && retention.KeepMonthly == source.KeepMonthly &&
		retention.KeepAnnual == source.KeepAnnual
}

func localSourcePolicyValues(raw json.RawMessage) ([]string, KopiaManagedPolicySource, bool) {
	var document map[string]json.RawMessage
	if strictJSON(raw, &document) != nil {
		return nil, KopiaManagedPolicySource{}, false
	}
	requiredEmpty := []string{
		"errorHandling", "scheduling", "compression",
		"metadataCompression", "splitter", "actions", "upload",
	}
	if len(document) != len(requiredEmpty)+4 {
		return nil, KopiaManagedPolicySource{}, false
	}
	for _, key := range requiredEmpty {
		value, ok := document[key]
		if !ok || !exactEmptyJSONObject(value) {
			return nil, KopiaManagedPolicySource{}, false
		}
	}
	var retention struct {
		KeepLatest  *int `json:"keepLatest"`
		KeepHourly  *int `json:"keepHourly"`
		KeepDaily   *int `json:"keepDaily"`
		KeepWeekly  *int `json:"keepWeekly"`
		KeepMonthly *int `json:"keepMonthly"`
		KeepAnnual  *int `json:"keepAnnual"`
	}
	if strictJSON(document["retention"], &retention) != nil || retention.KeepLatest == nil ||
		retention.KeepHourly == nil || retention.KeepDaily == nil || retention.KeepWeekly == nil ||
		retention.KeepMonthly == nil || retention.KeepAnnual == nil {
		return nil, KopiaManagedPolicySource{}, false
	}
	values := KopiaManagedPolicySource{KeepLatest: *retention.KeepLatest, KeepHourly: *retention.KeepHourly,
		KeepDaily: *retention.KeepDaily, KeepWeekly: *retention.KeepWeekly,
		KeepMonthly: *retention.KeepMonthly, KeepAnnual: *retention.KeepAnnual}
	filesRaw, ok := document["files"]
	if !ok {
		return nil, KopiaManagedPolicySource{}, false
	}
	var files map[string]json.RawMessage
	if strictJSON(filesRaw, &files) != nil || files == nil || len(files) > 1 {
		return nil, KopiaManagedPolicySource{}, false
	}
	var actual []string
	if ignore, exists := files["ignore"]; exists {
		if strictJSON(ignore, &actual) != nil || actual == nil {
			return nil, KopiaManagedPolicySource{}, false
		}
	}
	osSnapshotsRaw, ok := document["osSnapshots"]
	if !ok || !exactJSONObjectOfEmptyObjects(osSnapshotsRaw, "volumeShadowCopy") {
		return nil, KopiaManagedPolicySource{}, false
	}
	loggingRaw, ok := document["logging"]
	if !ok || !exactJSONObjectOfEmptyObjects(loggingRaw, "directories", "entries") {
		return nil, KopiaManagedPolicySource{}, false
	}
	actual = normalizedKopiaExcludes(actual)
	sort.Strings(actual)
	actual = compactStrings(actual)
	return actual, values, true
}

func validateKopiaPathArtifacts(policies map[string]json.RawMessage, sources []KopiaManagedPolicySource) error {
	selected := make(map[string]string, len(sources))
	for _, source := range sources {
		selected[source.JobUUID] = filepath.Clean(source.Source)
	}
	for target, raw := range policies {
		index := strings.Index(target, ":")
		if index < 0 {
			continue
		}
		userHost, targetPath := target[:index], target[index+1:]
		if !strings.HasSuffix(userHost, "@replicaro") {
			continue
		}
		jobUUID := strings.TrimSuffix(userHost, "@replicaro")
		expectedPath, ok := selected[jobUUID]
		if !ok {
			continue
		}
		if filepath.Clean(targetPath) != expectedPath {
			return fmt.Errorf("Kopia job %s has an unexpected path policy", jobUUID)
		}
		if !manualOnlyPathPolicyMatches(raw) {
			return fmt.Errorf("Kopia job %s path policy is not the exact native manual-only artifact", jobUUID)
		}
	}
	return nil
}

func validateEffectiveKopiaPathArtifacts(
	ctx context.Context,
	session *kopiaRepositorySession,
	policies map[string]json.RawMessage,
	sources []KopiaManagedPolicySource,
	desiredPolicies map[string]KopiaManagedPolicySource,
) (string, error) {
	_ = policies // Defined path artifacts are validated separately above.
	targets := make([]string, 0, len(sources))
	for _, source := range sources {
		targets = append(targets, source.JobUUID+"@replicaro:"+filepath.Clean(source.Source))
	}
	sort.Strings(targets)
	var output strings.Builder
	outputOmitted := false
	for _, target := range targets {
		var document []byte
		value, err := runCapturedKopiaPolicyOutput(ctx, session, []string{"policy", "show", "--json", target}, func(reader io.Reader) error {
			var readErr error
			document, readErr = readKopiaPolicyDocument(reader)
			return readErr
		})
		appendKopiaPolicyDiagnostic(&output, &outputOmitted, value)
		if err != nil {
			return output.String(), fmt.Errorf("read effective Kopia path policy %q: %w", target, err)
		}
		userTarget := target[:strings.Index(target, ":")]
		if !effectiveSourcePolicyMatches(document, desiredPolicies[userTarget]) {
			return output.String(), fmt.Errorf("effective Kopia path policy did not inherit the managed values for %q", target)
		}
	}
	return output.String(), nil
}

func manualOnlyPathPolicyMatches(raw json.RawMessage) bool {
	var document map[string]json.RawMessage
	if strictJSON(raw, &document) != nil || len(document) != 11 {
		return false
	}
	for _, key := range []string{"retention", "files", "errorHandling", "compression",
		"metadataCompression", "splitter", "actions", "upload"} {
		if !exactEmptyJSONObject(document[key]) {
			return false
		}
	}
	var scheduling map[string]json.RawMessage
	if strictJSON(document["scheduling"], &scheduling) != nil || len(scheduling) != 1 {
		return false
	}
	var manual bool
	if strictJSON(scheduling["manual"], &manual) != nil || !manual {
		return false
	}
	return exactJSONObjectOfEmptyObjects(document["osSnapshots"], "volumeShadowCopy") &&
		exactJSONObjectOfEmptyObjects(document["logging"], "directories", "entries")
}

func singleExportedPolicy(output, expectedID string) (json.RawMessage, error) {
	var policies map[string]json.RawMessage
	if err := strictJSON([]byte(output), &policies); err != nil {
		return nil, fmt.Errorf("decode Kopia policy export: %w", err)
	}
	if len(policies) != 1 {
		return nil, fmt.Errorf("Kopia policy export is ambiguous")
	}
	raw, ok := policies[expectedID]
	if !ok {
		return nil, fmt.Errorf("Kopia policy export returned an unexpected target")
	}
	var document map[string]json.RawMessage
	if err := strictJSON(raw, &document); err != nil {
		return nil, fmt.Errorf("decode Kopia defined policy: %w", err)
	}
	normalized, err := json.Marshal(document)
	return normalized, err
}

func kopiaClientIdentity(output string) (string, string, error) {
	var status kopiaRepositoryStatus
	if err := strictJSON([]byte(output), &status); err != nil {
		return "", "", fmt.Errorf("decode Kopia repository status: %w", err)
	}
	user := strings.TrimSpace(status.ClientOptions.Username)
	host := strings.TrimSpace(status.ClientOptions.Hostname)
	if user == "" || host == "" {
		return "", "", fmt.Errorf("Kopia repository status has an incomplete client identity")
	}
	return user, host, nil
}

func kopiaSourceTarget(user, host, source string) (string, error) {
	absolute, err := filepath.Abs(source)
	if err != nil {
		return "", err
	}
	return user + "@" + host + ":" + filepath.Clean(absolute), nil
}

func kopiaTargetPath(target string) string {
	index := strings.Index(target, ":")
	if index < 1 || index == len(target)-1 {
		return ""
	}
	return target[index+1:]
}

func strictJSON(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON")
		}
		return err
	}
	return nil
}

func exactEmptyJSONObject(raw json.RawMessage) bool {
	var value map[string]json.RawMessage
	if strictJSON(raw, &value) != nil {
		return false
	}
	return value != nil && len(value) == 0
}

func exactJSONObjectOfEmptyObjects(raw json.RawMessage, keys ...string) bool {
	var value map[string]json.RawMessage
	if strictJSON(raw, &value) != nil || value == nil || len(value) != len(keys) {
		return false
	}
	for _, key := range keys {
		child, ok := value[key]
		if !ok || !exactEmptyJSONObject(child) {
			return false
		}
	}
	return true
}

func intValue(value *int) int {
	if value == nil {
		return 0
	}
	return *value
}

func compactStrings(values []string) []string {
	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}
	return result
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
