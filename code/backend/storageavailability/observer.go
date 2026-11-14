// Package storageavailability turns one storage-helper probe into the
// run/pause/error decision that precedes every filesystem backup, scheduled
// vault check, and scheduled vault maintenance, and records the two storage
// facts (mount point and filesystem type) when a source or vault is saved. It
// never probes direct cloud connectors or invokes a native backup engine.
//
// The rules in one place, because several of them look like bugs out of
// context:
//
//   - A run needs a positive observation: the folder opened, and every
//     recorded fact matches. A recorded fact that cannot be read now pauses.
//   - Different mount point or filesystem type pauses. That is the "share was
//     unmounted and the empty mount-point folder is left behind" case, which
//     both engines would otherwise back up as a success and then let
//     retention delete the real snapshots.
//   - Folder missing pauses only when the recorded mount point is also gone
//     (drive unplugged, NAS off). With the mount point present the folder
//     was deleted or moved, which is an error the user must see.
//   - Timeouts and unexpected OS errors pause; access denied errors; helper
//     launch/protocol/integrity failures error because they prove nothing.
//   - A probe that never got a helper slot, because stuck checks of other
//     storage hold them all, pauses under its own reason: this storage may be
//     fine, and the pause ends once those checks finish.
//   - Nothing is ever relocated automatically, and no path is rewritten.
//   - There is no retry loop here. Pausing reuses the scheduler's existing
//     catch-up (recheck at most every five minutes).
package storageavailability

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/storageidentity"
	"github.com/local/replicaro/vaultidentity"
)

// DefaultObservationTimeout is the deadline for one helper probe. It starts
// before the helper is prepared, and preparation is not interrupted when it
// expires, so it is not a hard limit on how long a probe takes to return. A
// probe that hits it is treated as temporarily unreachable storage (pause),
// never as a missing folder.
const DefaultObservationTimeout = 10 * time.Second
const maxConcurrentBatchObservations = 8

var ErrObserverUnavailable = errors.New("bounded storage observation is unavailable")
var ErrRepositoryStorageUnavailable = errors.New("repository storage is unavailable")
var ErrSourceStorageUnavailable = errors.New("source storage is unavailable")

// Run results.
const (
	ResultRun   = "run"
	ResultPause = "pause"
	ResultError = "error"
)

// Fixed support labels for failures that happen before any OS step.
const (
	stepHelper  = "helper"  // helper launch, protocol, integrity, or timeout
	stepBinding = "binding" // the stored binding could not be decoded or does not describe this path
	stepPath    = "path"    // the configured path failed validation; the helper was never asked
)

// The storage error types below carry only the fixed availability reason.
// The failing step label and numeric OS code are deliberately not part of
// their text: that text becomes the operation result users read. Step and
// code go to support records only (recordDecision before a run,
// ResolutionStep for API failures, the fact_unavailable diagnostic when a
// folder is saved without a fact).

type RepositoryStorageUnavailableError struct {
	ReasonCode string
}

// slotsExhaustedExplanation follows the reason code in the result of an
// operation paused by it. The code alone would leave the user guessing why
// healthy storage wasn't checked and whether they have to do anything.
const slotsExhaustedExplanation = "Replicaro couldn't check this storage because earlier checks of other drives " +
	"or network shares stopped responding and are still running. Backups resume automatically when those " +
	"checks finish. If a network share stopped responding, reconnect it; restarting Replicaro also clears this."

func unavailableText(base, reason string) string {
	if reason == database.AvailabilityReasonObservationSlotsExhausted {
		return base + ": " + reason + ". " + slotsExhaustedExplanation
	}
	return base + ": " + reason
}

func (err *RepositoryStorageUnavailableError) Error() string {
	if err == nil || err.ReasonCode == "" {
		return ErrRepositoryStorageUnavailable.Error()
	}
	return unavailableText(ErrRepositoryStorageUnavailable.Error(), err.ReasonCode)
}

func (err *RepositoryStorageUnavailableError) Unwrap() error {
	return ErrRepositoryStorageUnavailable
}

type SourceStorageUnavailableError struct {
	ReasonCode string
}

type SourceStorageFailureError struct {
	ReasonCode string
}

type RepositoryStorageFailureError struct {
	ReasonCode string
}

func (err *RepositoryStorageFailureError) Error() string {
	if err == nil || err.ReasonCode == "" {
		return "repository storage validation failed"
	}
	return "repository storage validation failed: " + err.ReasonCode
}

func (err *SourceStorageFailureError) Error() string {
	if err == nil || err.ReasonCode == "" {
		return "source storage validation failed"
	}
	return "source storage validation failed: " + err.ReasonCode
}

func (err *SourceStorageUnavailableError) Error() string {
	if err == nil || err.ReasonCode == "" {
		return ErrSourceStorageUnavailable.Error()
	}
	return unavailableText(ErrSourceStorageUnavailable.Error(), err.ReasonCode)
}

func (err *SourceStorageUnavailableError) Unwrap() error {
	return ErrSourceStorageUnavailable
}

type Observer interface {
	Observe(context.Context, storageidentity.HelperRequest) (storageidentity.HelperResponse, error)
}

type ObserverFunc func(context.Context, storageidentity.HelperRequest) (storageidentity.HelperResponse, error)

func (fn ObserverFunc) Observe(ctx context.Context, request storageidentity.HelperRequest) (storageidentity.HelperResponse, error) {
	return fn(ctx, request)
}

type ProcessObserver struct {
	Runner *storageidentity.ProcessRunner
}

func (observer ProcessObserver) Observe(ctx context.Context, request storageidentity.HelperRequest) (storageidentity.HelperResponse, error) {
	if observer.Runner == nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	return observer.Runner.Run(ctx, request)
}

// InProcessObserver runs the probe in the calling process. It exists for
// tests only; production always goes through the separate helper process so a
// hanging OS call can be killed.
func InProcessObserver() Observer {
	return ObserverFunc(func(_ context.Context, request storageidentity.HelperRequest) (storageidentity.HelperResponse, error) {
		return storageidentity.ExecuteRequest(request)
	})
}

type Binding struct {
	Version        string
	Key            string
	DescriptorJSON string
}

var observerState struct {
	sync.RWMutex
	observer Observer
	timeout  time.Duration
	support  func(string)
}

func Configure(observer Observer, timeout time.Duration) {
	observerState.Lock()
	defer observerState.Unlock()
	observerState.observer = observer
	if timeout <= 0 {
		timeout = DefaultObservationTimeout
	}
	observerState.timeout = timeout
}

// ConfigureSupportLog sets where fixed-label support diagnostics go (the
// support-only activity level in production). Messages never contain paths,
// share names, credentials, or OS message text.
func ConfigureSupportLog(record func(string)) {
	observerState.Lock()
	defer observerState.Unlock()
	observerState.support = record
}

func SetObserverForTests(observer Observer, timeout time.Duration) func() {
	observerState.Lock()
	previousObserver, previousTimeout := observerState.observer, observerState.timeout
	observerState.observer = observer
	if timeout <= 0 {
		timeout = DefaultObservationTimeout
	}
	observerState.timeout = timeout
	observerState.Unlock()
	return func() {
		observerState.Lock()
		observerState.observer, observerState.timeout = previousObserver, previousTimeout
		observerState.Unlock()
	}
}

func SetSupportLogForTests(record func(string)) func() {
	observerState.Lock()
	previous := observerState.support
	observerState.support = record
	observerState.Unlock()
	lastDecision = sync.Map{}
	return func() {
		observerState.Lock()
		observerState.support = previous
		observerState.Unlock()
	}
}

func recordSupport(message string) {
	observerState.RLock()
	record := observerState.support
	observerState.RUnlock()
	if record != nil {
		record(message)
	}
}

// lastDecision keeps the last non-run diagnostic per source or vault so a
// paused job rechecked every five minutes for weeks writes one support line
// per change instead of one per recheck. It is process-local on purpose; a
// restart simply logs the current state once more.
var lastDecision sync.Map

func recordDecision(key, stage string, check Check) {
	if check.Result == ResultRun {
		lastDecision.Delete(key)
		return
	}
	message := "Support diagnostic: stage=" + stage + " result=" + check.Result +
		" reason=" + check.ReasonCode + " step=" + check.Step + " code=" + strconv.FormatInt(check.Code, 10)
	if previous, ok := lastDecision.Load(key); ok && previous == message {
		return
	}
	lastDecision.Store(key, message)
	recordSupport(message)
}

// probe runs one bounded helper request. The returned error is only a context
// error, storageidentity.ErrHelperSlotsExhausted, or a helper
// launch/protocol/integrity failure; unobservable storage is reported inside
// the response.
func probe(ctx context.Context, path, kind, mountPoint string) (storageidentity.HelperResponse, error) {
	if err := storageidentity.ValidateBindingPath(path); err != nil {
		return storageidentity.HelperResponse{}, err
	}
	observerState.RLock()
	current, timeout := observerState.observer, observerState.timeout
	observerState.RUnlock()
	if current == nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	if timeout <= 0 {
		timeout = DefaultObservationTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request := storageidentity.HelperRequest{
		Version: storageidentity.HelperVersion, Operation: "probe", Kind: kind, Path: path, MountPoint: mountPoint,
	}
	response, err := current.Observe(bounded, request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return storageidentity.HelperResponse{}, context.Canceled
		}
		// Checked before the deadline: running out of slots also ends at the
		// deadline, but it is about other storage, not this one.
		if errors.Is(err, storageidentity.ErrHelperSlotsExhausted) {
			return storageidentity.HelperResponse{}, storageidentity.ErrHelperSlotsExhausted
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(bounded.Err(), context.DeadlineExceeded) {
			return storageidentity.HelperResponse{}, context.DeadlineExceeded
		}
		return storageidentity.HelperResponse{}, fmt.Errorf("%w: %v", ErrObserverUnavailable, err)
	}
	// The process runner validates responses; test and alternate observers go
	// through the same shape check so no observer can smuggle facts past it.
	if err := storageidentity.ValidateHelperResponse(request, response); err != nil {
		return storageidentity.HelperResponse{}, fmt.Errorf("%w: %v", ErrObserverUnavailable, err)
	}
	return response, nil
}

// Check is one before-run decision for a source or filesystem vault.
type Check struct {
	Result     string
	ReasonCode string
	Step       string
	Code       int64
	// Observed holds the facts of the opened folder when the folder opened.
	Observed storageidentity.Facts
	// Legacy is set when the stored binding still uses the retired format.
	// The caller that owns the row's lock converts it with Observed.
	Legacy bool
	// canceled marks a probe stopped by cancellation. It is reported as a
	// pause to preliminary callers (which stop anyway) and as
	// context.Canceled to the final checks, so a cancellation is never
	// mistaken for unavailable storage that restores a scheduled occurrence.
	canceled bool
}

func canceledCheck() Check {
	return Check{Result: ResultPause, ReasonCode: database.AvailabilityReasonObservationTimeout, Step: stepHelper, canceled: true}
}

func errorCheck(reason, step string, code int64) Check {
	return Check{Result: ResultError, ReasonCode: reason, Step: step, Code: code}
}

func pauseCheck(reason, step string, code int64) Check {
	return Check{Result: ResultPause, ReasonCode: reason, Step: step, Code: code}
}

// decide applies the before-run table to one probe. recorded holds the facts
// saved with the binding; legacy marks a row still in the retired format.
func decide(recorded storageidentity.Facts, legacy bool, response storageidentity.HelperResponse, err error) Check {
	if err != nil {
		if errors.Is(err, storageidentity.ErrHelperSlotsExhausted) {
			// This storage was never probed, so the share that stopped
			// answering keeps "timed out" and this one gets its own reason.
			return pauseCheck(database.AvailabilityReasonObservationSlotsExhausted, stepHelper, 0)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// Includes a hung SMB/NFS/FUSE call that the helper could not
			// finish in time: storage is there but not answering, so pause.
			return pauseCheck(database.AvailabilityReasonObservationTimeout, stepHelper, 0)
		}
		// Launch, protocol, or integrity failures say nothing about the
		// storage either way, so they must be visible errors rather than a
		// silent pause that could go on for weeks.
		return errorCheck(database.AvailabilityReasonObservationFailed, stepHelper, 0)
	}
	switch response.Access {
	case storageidentity.AccessOK:
		check := Check{Result: ResultRun, Observed: response.Facts(), Legacy: legacy}
		if legacy {
			// First successful probe after upgrade: record whatever is observed
			// now. The retired descriptor is deliberately not compared, so a share
			// that happens to be unmounted during this first probe goes unnoticed.
			// That is a known, documented limitation.
			return check
		}
		if recorded.MountPoint != "" {
			if response.MountPoint == "" {
				failure, _ := response.FactFailure(storageidentity.StepMountPoint)
				return pauseCheck(database.AvailabilityReasonObservationFailed, storageidentity.StepMountPoint, failure.Code)
			}
			if !storageidentity.SameMountPoint(recorded.MountPoint, response.MountPoint) {
				return pauseCheck(database.AvailabilityReasonIdentityMismatch, storageidentity.StepMountPoint, 0)
			}
		}
		if recorded.Filesystem != "" {
			// The type is compared only where it was recorded (never for
			// Windows network paths). A different type on the same mount point
			// is typically a Docker bind mount whose host share was unmounted
			// (when the container sees the host's leftover folder), or another
			// kind of drive now using the letter.
			if response.Filesystem == "" {
				failure, _ := response.FactFailure(storageidentity.StepFilesystemType)
				return pauseCheck(database.AvailabilityReasonObservationFailed, storageidentity.StepFilesystemType, failure.Code)
			}
			if storageidentity.NormalizeFilesystem(recorded.Filesystem) != response.Filesystem {
				return pauseCheck(database.AvailabilityReasonIdentityMismatch, storageidentity.StepFilesystemType, 0)
			}
		}
		return check
	case storageidentity.AccessMissing:
		if legacy {
			// Retired rows have no usable mount point; a missing folder keeps
			// its pre-upgrade behavior and pauses.
			return pauseCheck(database.AvailabilityReasonStorageMissing, response.Step, response.Code)
		}
		if recorded.MountPoint == "" || response.RecordedMount == nil {
			// Saved without facts: there is nothing to tell "unplugged" from
			// "deleted", so the missing folder is reported as an error.
			return errorCheck(database.AvailabilityReasonStorageMissing, response.Step, response.Code)
		}
		switch response.RecordedMount.State {
		case storageidentity.MountPresent:
			return errorCheck(database.AvailabilityReasonStorageMissing, response.Step, response.Code)
		case storageidentity.MountAbsent:
			return pauseCheck(database.AvailabilityReasonStorageMissing, response.Step, response.Code)
		case storageidentity.MountDenied:
			return errorCheck(database.AvailabilityReasonObservationFailed, response.RecordedMount.Step, response.RecordedMount.Code)
		default:
			return pauseCheck(database.AvailabilityReasonObservationFailed, response.RecordedMount.Step, response.RecordedMount.Code)
		}
	case storageidentity.AccessDenied:
		// The storage is there; the user has to fix access (for example lost
		// share credentials). Pausing would hide that for 30 days.
		return errorCheck(database.AvailabilityReasonObservationFailed, response.Step, response.Code)
	default:
		return pauseCheck(database.AvailabilityReasonObservationFailed, response.Step, response.Code)
	}
}

// SourceLocation is the folder a backup reads: the job's alias when one is
// set, otherwise the immutable source. The alias is user-owned state; nothing
// in this package ever writes it.
func SourceLocation(job models.BackupJob) string {
	if job.ResolvedSourcePath != "" {
		return job.ResolvedSourcePath
	}
	return job.Source
}

// CheckSource probes the job's location in use and applies the before-run
// table. The binding's facts describe that location.
func CheckSource(ctx context.Context, job models.BackupJob) Check {
	return checkSourcePath(ctx, job, SourceLocation(job))
}

func checkSourcePath(ctx context.Context, job models.BackupJob, path string) Check {
	var check Check
	// Path validation first, so a path that never reaches the helper is
	// labelled "path" in support records (DecodeBinding would report the same
	// failure as an undecodable binding).
	pathErr := errors.Join(storageidentity.ValidateBindingPath(path), storageidentity.ValidateBindingPath(job.Source))
	recorded, legacy, err := storageidentity.DecodeBinding(job.SourceStorageVersion, job.SourceStorageKey,
		job.SourceStorageDescriptorJSON, job.Source)
	switch {
	case pathErr != nil:
		check = errorCheck(database.AvailabilityReasonIdentityMismatch, stepPath, 0)
	case err != nil || path != SourceLocation(job):
		check = errorCheck(database.AvailabilityReasonIdentityMismatch, stepBinding, 0)
	default:
		response, probeErr := probe(ctx, path, sourceProbeKind(job), recorded.MountPoint)
		if errors.Is(probeErr, context.Canceled) {
			return canceledCheck()
		}
		check = decide(recorded, legacy, response, probeErr)
	}
	if ctx.Err() == nil {
		recordDecision("source:"+job.ID, "source_before_run", check)
	}
	return check
}

// sourceProbeKind picks the before-run probe for a bound job. A file job's
// probe answers in the folder probe's vocabulary (a path that is no longer a
// regular file is "missing"), so decide applies the same table to both. A
// file that has become a single-file mount since it was bound isn't refused
// here: its mount point then differs from the recorded one, which pauses the
// job as a changed storage identity, exactly as a remounted folder does.
func sourceProbeKind(job models.BackupJob) string {
	if job.IsFileSource() {
		return storageidentity.ProbeSourceFile
	}
	return storageidentity.ProbeSource
}

// CheckRepository probes a filesystem vault's registered location.
func CheckRepository(ctx context.Context, repo models.Repository) Check {
	if repo.Connector != "fs" {
		return Check{Result: ResultRun}
	}
	var check Check
	recorded, legacy, err := storageidentity.DecodeBinding(repo.StorageIdentityVersion, repo.StorageIdentityKey,
		repo.StorageIdentityJSON, repo.Location)
	switch {
	case storageidentity.ValidateBindingPath(repo.Location) != nil:
		// Checked before the binding so the support record names the real
		// step ("path") instead of an undecodable binding or the helper.
		check = errorCheck(database.AvailabilityReasonIdentityMismatch, stepPath, 0)
	case err != nil:
		check = errorCheck(database.AvailabilityReasonIdentityMismatch, stepBinding, 0)
	default:
		response, probeErr := probe(ctx, repo.Location, storageidentity.ProbeVault, recorded.MountPoint)
		if errors.Is(probeErr, context.Canceled) {
			return canceledCheck()
		}
		check = decide(recorded, legacy, response, probeErr)
	}
	if ctx.Err() == nil {
		recordDecision("vault:"+repo.ID, "vault_before_run", check)
	}
	return check
}

func checkObservation(check Check) database.StorageAvailabilityObservation {
	now := time.Now().UTC()
	switch check.Result {
	case ResultRun:
		return database.StorageAvailabilityObservation{State: database.StorageAvailable, CheckedAt: now}
	case ResultPause:
		return database.StorageAvailabilityObservation{State: database.StorageUnavailable,
			ReasonCode: check.ReasonCode, CheckedAt: now}
	default:
		// Used only to route this run: the executor turns the failure into a
		// normal failed operation before hooks or native work.
		return database.StorageAvailabilityObservation{State: database.StorageUnknown,
			ReasonCode: check.ReasonCode, CheckedAt: now, ConclusiveFailure: true}
	}
}

// ObserveSource is the preliminary source decision made before a scheduled
// or manual backup is queued. For a legacy binding it also carries the
// conversion that the admission transaction applies (compare-and-swap on the
// old values) when the probe succeeds.
func ObserveSource(ctx context.Context, job models.BackupJob, _ time.Time) database.StorageAvailabilityObservation {
	check := CheckSource(ctx, job)
	observation := checkObservation(check)
	if check.Result == ResultRun && check.Legacy {
		if encoded, err := storageidentity.EncodeBinding(check.Observed); err == nil {
			observation.Conversion = &database.SourceBindingConversion{
				Location:    SourceLocation(job),
				FromVersion: job.SourceStorageVersion, FromKey: job.SourceStorageKey, FromJSON: job.SourceStorageDescriptorJSON,
				ToVersion: storageidentity.BindingVersion, ToKey: job.Source, ToJSON: encoded,
			}
		}
	}
	return observation
}

// ObserveRepository is the preliminary vault decision made before a backup
// or scheduled vault task is queued. Conversion of a legacy vault binding
// happens later, under the vault lock, in repository admission.
func ObserveRepository(ctx context.Context, repo models.Repository, _ time.Time) database.StorageAvailabilityObservation {
	if repo.Connector != "fs" {
		return database.StorageAvailabilityObservation{State: database.StorageAvailable, CheckedAt: time.Now().UTC()}
	}
	return checkObservation(CheckRepository(ctx, repo))
}

// RequireSourcePathAvailable is the final source check at the native child
// boundary, after hooks and immediately before the requested backup starts.
// path is the location frozen when the run was dispatched.
func RequireSourcePathAvailable(ctx context.Context, job models.BackupJob, path string) error {
	check := checkSourcePath(ctx, job, path)
	if check.canceled || errors.Is(ctx.Err(), context.Canceled) {
		return context.Canceled
	}
	switch check.Result {
	case ResultRun:
		return nil
	case ResultPause:
		return &SourceStorageUnavailableError{ReasonCode: check.ReasonCode}
	default:
		return &SourceStorageFailureError{ReasonCode: check.ReasonCode}
	}
}

// AdmitRepositoryStorage is the vault decision made under the vault lock,
// before the native repository and protected-root identity checks. A pause
// or error is returned as the typed error the orchestration layer expects.
func AdmitRepositoryStorage(ctx context.Context, repo models.Repository) (Check, error) {
	check := CheckRepository(ctx, repo)
	if check.canceled || errors.Is(ctx.Err(), context.Canceled) {
		return check, context.Canceled
	}
	switch check.Result {
	case ResultRun:
		return check, nil
	case ResultPause:
		return check, &RepositoryStorageUnavailableError{ReasonCode: check.ReasonCode}
	default:
		return check, &RepositoryStorageFailureError{ReasonCode: check.ReasonCode}
	}
}

// ObserveBackupSet observes one source and its selected targets concurrently,
// with a fixed worker bound. This keeps the observations close to the
// admission transaction without creating unbounded helper processes.
func ObserveBackupSet(
	ctx context.Context,
	job models.BackupJob,
	repositories []models.Repository,
) (database.StorageAvailabilityObservation, []database.TargetAvailabilityObservation) {
	targets := make([]database.TargetAvailabilityObservation, len(repositories))
	type workItem struct {
		source bool
		index  int
	}
	work := make(chan workItem)
	workerCount := min(maxConcurrentBatchObservations, len(repositories)+1)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	var source database.StorageAvailabilityObservation
	for range workerCount {
		go func() {
			defer workers.Done()
			for item := range work {
				if item.source {
					source = ObserveSource(ctx, job, time.Time{})
					continue
				}
				repo := repositories[item.index]
				targets[item.index] = database.TargetAvailabilityObservation{
					RepositoryID: repo.ID,
					Availability: ObserveRepository(ctx, repo, time.Time{}),
				}
			}
		}()
	}
	work <- workItem{source: true}
	for index := range repositories {
		work <- workItem{index: index}
	}
	close(work)
	workers.Wait()
	return source, targets
}

// bindingError reports why a folder could not be bound at creation or
// connection time. It never contains a path or OS message.
type bindingError struct {
	reason           string
	step             string
	code             int64
	permissionDenied bool
	timeout          bool
	cause            error // only for a path that failed validation
}

// Error keeps step and code out of the text for the same reason as the
// storage error types above; ResolutionStep hands them to support records.
func (err *bindingError) Error() string {
	return ErrObserverUnavailable.Error() + ": " + err.reason
}

func (err *bindingError) Unwrap() []error {
	if err.timeout {
		return []error{ErrObserverUnavailable, context.DeadlineExceeded}
	}
	if err.cause != nil {
		return []error{ErrObserverUnavailable, err.cause}
	}
	return []error{ErrObserverUnavailable}
}

// IsPermissionDenied reports only a typed denial from a valid helper response.
// It does not infer permission from generic observer failures.
func IsPermissionDenied(err error) bool {
	var failure *bindingError
	return errors.As(err, &failure) && failure.permissionDenied
}

// ResolutionReason returns the fixed availability reason for a binding error.
func ResolutionReason(err error) string {
	var failure *bindingError
	if errors.As(err, &failure) && failure.reason != "" {
		return failure.reason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return database.AvailabilityReasonObservationTimeout
	}
	return database.AvailabilityReasonObservationFailed
}

// ResolutionStep returns the fixed step label and OS code for a binding error.
func ResolutionStep(err error) (string, int64) {
	var failure *bindingError
	if errors.As(err, &failure) {
		return failure.step, failure.code
	}
	return stepHelper, 0
}

// The two ways a job source can be refused even though the path is there.
// Their text is shown to the user as it is (the API passes exactly these two
// through instead of its fixed "could not be observed" text), so it must stay
// free of paths and OS messages. The UI and its documentation quote the
// wording, so don't reword it casually.
var (
	// ErrSourceNotFileOrFolder: the path is a symbolic link to a file, a
	// device, a pipe or a socket. A link isn't followed because the engines
	// disagree on links (Restic stores the link, Kopia the target) and
	// because it would back up whatever the link points at later; a device or
	// pipe can't be backed up as a file at all.
	ErrSourceNotFileOrFolder = errors.New("Replicaro can back up a file or a folder, but not a symbolic link, device, pipe, or socket. Choose a different path.")
	// ErrSourceFileMountedAlone: the file is itself the root of a mount,
	// which is what a single-file Docker bind mount looks like inside the
	// container. When an editor on the host saves by replacing the file, the
	// mount keeps the old one, so backups would silently store stale data.
	ErrSourceFileMountedAlone = errors.New("This file is mounted on its own (for example, as a single-file Docker bind mount). After the file is saved on the host, the mount can keep showing the old version, so Replicaro could back up outdated data. Mount the folder that holds the file instead, then choose the file inside that folder.")
)

// SourceRefusal returns ErrSourceNotFileOrFolder or ErrSourceFileMountedAlone
// when err is one of them, and nil for every other error.
func SourceRefusal(err error) error {
	for _, refusal := range []error{ErrSourceNotFileOrFolder, ErrSourceFileMountedAlone} {
		if errors.Is(err, refusal) {
			return refusal
		}
	}
	return nil
}

// bindFilesystem records the facts of one folder being saved. It never
// refuses a readable folder because a fact is missing: the folder is saved
// without that fact and a support diagnostic records why. Only a folder that
// cannot be opened at all (for a new vault: no existing parent can be opened)
// fails.
func bindFilesystem(ctx context.Context, path, kind, stage string) (Binding, error) {
	binding, _, err := bindObserved(ctx, path, kind, stage)
	return binding, err
}

// bindObserved is bindFilesystem that also returns what the probe found, for
// the source probes that report a kind. The "unsupported" case and the
// mount-root check can only trigger for a source_any probe: the helper
// response check refuses "unsupported" and mount_root for every other probe
// kind, so folder and vault bindings never reach them.
func bindObserved(ctx context.Context, path, kind, stage string) (Binding, string, error) {
	if err := storageidentity.ValidateBindingPath(path); err != nil {
		// Never reached the helper, so the support label is "path", not
		// "helper". The validation text itself stays in the wrapped cause.
		return Binding{}, "", &bindingError{reason: database.AvailabilityReasonObservationFailed, step: stepPath, cause: err}
	}
	response, err := probe(ctx, path, kind, "")
	if err != nil {
		if errors.Is(err, context.Canceled) {
			return Binding{}, "", context.Canceled
		}
		if errors.Is(err, storageidentity.ErrHelperSlotsExhausted) {
			return Binding{}, "", &bindingError{reason: database.AvailabilityReasonObservationSlotsExhausted, step: stepHelper}
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return Binding{}, "", &bindingError{reason: database.AvailabilityReasonObservationTimeout, step: stepHelper, timeout: true}
		}
		return Binding{}, "", &bindingError{reason: database.AvailabilityReasonObservationFailed, step: stepHelper}
	}
	switch response.Access {
	case storageidentity.AccessOK:
	case storageidentity.AccessMissing:
		return Binding{}, "", &bindingError{reason: database.AvailabilityReasonStorageMissing, step: response.Step, code: response.Code}
	case storageidentity.AccessDenied:
		return Binding{}, "", &bindingError{reason: database.AvailabilityReasonObservationFailed,
			step: response.Step, code: response.Code, permissionDenied: true}
	case storageidentity.AccessUnsupported:
		return Binding{}, "", ErrSourceNotFileOrFolder
	default:
		return Binding{}, "", &bindingError{reason: database.AvailabilityReasonObservationFailed, step: response.Step, code: response.Code}
	}
	if response.MountRoot {
		// Only a file is reported as a mount root; a folder that is a mount
		// point (a mounted drive, a Docker folder bind mount) is an ordinary
		// source and never gets here.
		return Binding{}, "", ErrSourceFileMountedAlone
	}
	for _, failure := range response.FactFailures {
		recordSupport("Support diagnostic: stage=" + stage + " reason=fact_unavailable step=" + failure.Step +
			" code=" + strconv.FormatInt(failure.Code, 10))
	}
	encoded, err := storageidentity.EncodeBinding(response.Facts())
	if err != nil {
		return Binding{}, "", &bindingError{reason: database.AvailabilityReasonObservationFailed, step: stepHelper}
	}
	return Binding{Version: storageidentity.BindingVersion, Key: path, DescriptorJSON: encoded}, response.Kind, nil
}

// bindSourcePath binds a job source with the probe its recorded kind calls
// for and returns the kind to store with the binding ("" for a folder).
//
//   - "" is a folder job: the folder probe, the same one its before-run
//     checks use. That includes every job recovered from a profile or dormant
//     definition that names no kind, so a recovered folder job whose path has
//     become a file is refused as a missing source, never quietly turned into
//     a file job.
//   - SourceKindUnknown means "find out": a new job, or an adopted import
//     binding for the first time. source_any sends a folder to the same
//     folder probe and anything else to the file rules.
//   - SourceKindFile is a recovered file job: source_any, and it has to find
//     a file. A folder there is refused as a missing source, the mirror image
//     of a folder job whose path is now a file.
//
// Do not let a recorded kind other than unknown change here: the kind decides
// which snapshots share the job's native retention group, so a folder job
// that silently became a file job (or the reverse) would let retention expire
// the other kind's snapshots.
func bindSourcePath(ctx context.Context, path, recordedKind, stage string) (Binding, string, error) {
	switch recordedKind {
	case "":
		binding, err := bindFilesystem(ctx, path, storageidentity.ProbeSource, stage)
		return binding, "", err
	case models.SourceKindUnknown, models.SourceKindFile:
	default:
		return Binding{}, "", fmt.Errorf("unsupported backup job source kind")
	}
	binding, found, err := bindObserved(ctx, path, storageidentity.ProbeSourceAny, stage)
	if err != nil {
		return Binding{}, "", err
	}
	switch found {
	case storageidentity.PathKindFile:
		return binding, models.SourceKindFile, nil
	case storageidentity.PathKindFolder:
		if recordedKind == models.SourceKindFile {
			// The same answer a bound file job's before-run probe gives for a
			// folder: missing, at the file step, with no OS error behind it.
			return Binding{}, "", &bindingError{reason: database.AvailabilityReasonStorageMissing, step: storageidentity.StepOpenFile}
		}
		return binding, "", nil
	default:
		// ValidateHelperResponse already requires a kind on a source_any
		// success; this only keeps a malformed answer from binding anything.
		return Binding{}, "", &bindingError{reason: database.AvailabilityReasonObservationFailed, step: stepHelper}
	}
}

// Support stages used for binding diagnostics. The API's failure records
// (markSupportStorageObservation) use the same labels, so a "saved without a
// fact" line and a failed save of the same action share one stage name.
const (
	StageJobCreateSource = "job_create_source"
	StageJobBindSource   = "job_bind_source"
	StageJobSourceUpdate = "job_source_update"
	StageVaultCreate     = "vault_create_destination"
	StageVaultConnect    = "vault_connect_destination"
)

func BindRepository(ctx context.Context, repo models.Repository) (models.Repository, error) {
	return bindRepository(ctx, repo, true)
}

// BindExistingRepository is used when the selected vault folder must already
// exist (connect, update, retries). Creation binds the deepest existing parent.
func BindExistingRepository(ctx context.Context, repo models.Repository) (models.Repository, error) {
	return bindRepository(ctx, repo, false)
}

func bindRepository(ctx context.Context, repo models.Repository, creation bool) (models.Repository, error) {
	if repo.Connector != "fs" {
		if repo.StorageIdentityVersion != "" || repo.StorageIdentityKey != "" || repo.StorageIdentityJSON != "" {
			return models.Repository{}, fmt.Errorf("direct connector contains a filesystem storage binding")
		}
		identity, err := vaultidentity.PhysicalIdentityWithOptions(repo.Connector, repo.Location, repo.ConnectorOptions)
		if err != nil {
			return models.Repository{}, err
		}
		repo.CanonicalIdentity = identity
		return repo, nil
	}
	configured, err := storageidentity.NormalizeConfiguredPath(repo.Location)
	if err != nil {
		return models.Repository{}, err
	}
	kind, stage := storageidentity.ProbeVault, StageVaultConnect
	if creation {
		kind, stage = storageidentity.ProbeVaultCreate, StageVaultCreate
	}
	binding, err := bindFilesystem(ctx, configured, kind, stage)
	if err != nil {
		return models.Repository{}, err
	}
	// A filesystem vault's canonical identity is its exact normalized
	// location, independent of engine. Pending creations are therefore found
	// and reserved by path, and a Restic and a Kopia creation at the same
	// folder conflict. Two spellings of one location (Z:\vault versus
	// \\server\share\vault, or different drive-letter case) are different
	// keys; the destination preflight and native creation remain the
	// protection there. Vault identity itself is still the native repository
	// ID plus the protected vault UUID.
	repo.Location = configured
	repo.CanonicalIdentity = configured
	repo.StorageIdentityVersion = binding.Version
	repo.StorageIdentityKey = binding.Key
	repo.StorageIdentityJSON = binding.DescriptorJSON
	return repo, nil
}

// BindJobSource records the source facts when a job is created
// (StageJobCreateSource) or when an imported job is first bound here
// (StageJobBindSource); stage only labels support diagnostics.
//
// The probe follows job.SourceKind (see bindSourcePath): a caller creating a
// new job sets it to SourceKindUnknown so the path decides between a folder
// and a file, while a recovered job keeps the kind it was saved with. The
// returned job carries the kind to store, "" for a folder or SourceKindFile.
func BindJobSource(ctx context.Context, job models.BackupJob, stage string) (models.BackupJob, error) {
	configured, err := storageidentity.NormalizeConfiguredPath(job.Source)
	if err != nil {
		return models.BackupJob{}, err
	}
	// Keep the entered spelling after lexical validation. Once saved, this path
	// is also native source scope (notably Kopia SourceInfo); recorded facts and
	// later availability checks never rewrite it.
	binding, kind, err := bindSourcePath(ctx, configured, job.SourceKind, stage)
	if err != nil {
		return models.BackupJob{}, err
	}
	job.Source = configured
	job.SourceKind = kind
	job.SourceStorageVersion, job.SourceStorageKey, job.SourceStorageDescriptorJSON =
		binding.Version, binding.Key, binding.DescriptorJSON
	return job, nil
}

// BindJobSourceAlias records the facts of the folder chosen with "Update job
// source" and returns the job with that folder as its alias. It uses the same
// probe and the same never-refuse-for-a-missing-fact rule as job creation.
//
// The returned binding keeps the immutable source as its key (the key is only
// the normalized configured path the columns require) while the facts describe
// the alias, because the facts must describe the folder backups actually read.
// Choosing the immutable source itself clears the alias. The source is never
// replaced: it is native retention and File History scope, see
// database.UpdateJobSourceAlias.
//
// The alias must be the same kind of source as the job: a folder job's alias
// goes through the folder probe, and a file job's alias must be a file
// (bindSourcePath). The returned job carries the kind the probe found, so
// database.UpdateJobSourceAlias compares it with the stored kind. An unbound
// job of unknown kind takes first binding instead: the handler refuses it
// before calling this, and the guard below is the backstop.
func BindJobSourceAlias(ctx context.Context, job models.BackupJob, location string) (models.BackupJob, error) {
	configured, err := storageidentity.NormalizeConfiguredPath(location)
	if err != nil {
		return models.BackupJob{}, err
	}
	if err := storageidentity.ValidateBindingPath(job.Source); err != nil {
		return models.BackupJob{}, err
	}
	if job.SourceKind == models.SourceKindUnknown {
		return models.BackupJob{}, database.ErrJobSourceUnbound
	}
	binding, kind, err := bindSourcePath(ctx, configured, job.SourceKind, StageJobSourceUpdate)
	if err != nil {
		return models.BackupJob{}, err
	}
	job.SourceKind = kind
	job.ResolvedSourcePath = configured
	if configured == job.Source {
		job.ResolvedSourcePath = ""
	}
	job.SourceStorageVersion, job.SourceStorageKey, job.SourceStorageDescriptorJSON =
		binding.Version, job.Source, binding.DescriptorJSON
	return job, nil
}
