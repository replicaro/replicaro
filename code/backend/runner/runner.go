package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/jobscript"
	"github.com/local/replicaro/locale"
	"github.com/local/replicaro/metadata"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/notifications"
	"github.com/local/replicaro/operationlog"
	"github.com/local/replicaro/operationruntime"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
)

var ErrAlreadyRunning = errors.New("this backup target already has a queued or running operation; wait for it to finish before running it again")
var ErrAdmissionClosed = errors.New("backup admission is closed")

type targetTask struct {
	db                  *sql.DB
	job                 models.BackupJob
	target              models.BackupJobTarget
	repository          models.Repository
	operationID         string
	vaultID             string
	unlock              func()
	resumeMetadata      func()
	sourcePath          string
	sourceFailureReason string
	targetFailureReason string
	ctx                 context.Context
	cancel              context.CancelFunc
	quarantined         bool
	cancelRequested     atomic.Bool
}

type Coordinator struct {
	db           *sql.DB
	mu           sync.Mutex
	limit        int
	queue        []*targetTask
	active       map[string]*targetTask
	vaultActive  map[string]bool
	vaultWaiting map[string]bool // present=false: initial handoff; present=true: known contention
	vaultReady   map[string]func()
	running      int
	admitting    bool
	admitMu      sync.Mutex
	wake         chan struct{}
	stop         chan struct{}
	dispatchDone chan struct{}
	queuedDone   chan struct{}
	workWG       sync.WaitGroup
	stopOnce     sync.Once
	shutdownOnce sync.Once
	closed       bool
	ctx          context.Context
	cancel       context.CancelFunc
	runtime      *operationruntime.Manager
}

var (
	coordinatorMu           sync.Mutex
	coordinatorTransitionMu sync.Mutex
	coordinator             *Coordinator
	coordinatorDisabled     = map[*sql.DB]bool{}
	executorMu              sync.RWMutex
	executor                contextTargetExecutor = defaultExecutorContext
	finisherMu              sync.RWMutex
	finisher                OperationFinisher = database.FinishBackupOperation
	resolveBackupEngine                       = func(repo models.Repository) (engines.Engine, error) {
		return engines.ResolveWithRepositoryAvailabilityCheck(
			repo, storageavailability.RequireRepositoryAvailable,
		)
	}
	assertVaultWriter = func(ctx context.Context, repo models.Repository) error {
		store := (vaultprofile.Store{Repository: repo}).
			WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
		if err := store.AssertRootIdentity(ctx); err != nil {
			return err
		}
		return store.ForProfile(repo.ProfileUUID).
			AssertAttachment(ctx, repo.ClientUUID, repo.AttachmentGeneration)
	}
	scheduleRepositorySync       = metadata.ScheduleRepositorySyncAfterBackup
	interruptQueuedOperations    = database.InterruptQueuedBackupOperations
	afterTaskRuntimeRegistration = func(*targetTask) {}
	afterTargetExecutorReturn    = func(*targetTask) {}
)

// TargetExecutor is the engine seam used by runner tests and by callers that
// provide a controlled engine implementation.
type TargetExecutor func(*sql.DB, models.BackupJob, models.BackupJobTarget) (string, error)
type contextTargetExecutor func(context.Context, *sql.DB, models.BackupJob, models.BackupJobTarget) (string, error)

type operationIDContextKey struct{}
type cancelGateCloserContextKey struct{}
type sourceFailureReasonContextKey struct{}
type targetFailureReasonContextKey struct{}

func contextWithCancelGateCloser(ctx context.Context, closeGate func()) context.Context {
	return context.WithValue(ctx, cancelGateCloserContextKey{}, closeGate)
}

func closeCancelGateFromContext(ctx context.Context) {
	if closeGate, _ := ctx.Value(cancelGateCloserContextKey{}).(func()); closeGate != nil {
		closeGate()
	}
}

type frozenSourcePathContextKey struct{}
type runtimeRepositoryViewContextKey struct{}

type runtimeRepositoryView struct {
	mu   sync.RWMutex
	repo models.Repository
}

func setRuntimeRepositoryView(ctx context.Context, repo models.Repository) {
	view, _ := ctx.Value(runtimeRepositoryViewContextKey{}).(*runtimeRepositoryView)
	if view == nil {
		return
	}
	view.mu.Lock()
	view.repo = repo
	view.mu.Unlock()
}

func getRuntimeRepositoryView(ctx context.Context) (models.Repository, bool) {
	view, _ := ctx.Value(runtimeRepositoryViewContextKey{}).(*runtimeRepositoryView)
	if view == nil {
		return models.Repository{}, false
	}
	view.mu.RLock()
	defer view.mu.RUnlock()
	return view.repo, view.repo.ID != ""
}

type followupFailure struct{ err error }
type applicationWarning struct{ err error }
type afterScriptFailure struct{ err error }
type afterScriptSuppressed struct{ err error }

func (value followupFailure) Error() string       { return value.err.Error() }
func (value followupFailure) Unwrap() error       { return value.err }
func (value applicationWarning) Error() string    { return value.err.Error() }
func (value applicationWarning) Unwrap() error    { return value.err }
func (value afterScriptFailure) Error() string    { return value.err.Error() }
func (value afterScriptFailure) Unwrap() error    { return value.err }
func (value afterScriptSuppressed) Error() string { return value.err.Error() }
func (value afterScriptSuppressed) Unwrap() error { return value.err }

func taskCanceledStage(taskErr, stageErr error) bool {
	return (errors.Is(taskErr, context.Canceled) || errors.Is(taskErr, context.DeadlineExceeded)) &&
		(errors.Is(stageErr, context.Canceled) || errors.Is(stageErr, context.DeadlineExceeded))
}

func classifyBackupRunStatus(runErr, taskErr error) string {
	if runErr == nil {
		// A cancellation accepted after every eligible child returned did not
		// interrupt or suppress work and cannot rewrite established success.
		return "success"
	}
	var suppressed afterScriptSuppressed
	if errors.As(runErr, &suppressed) {
		return "interrupted"
	}
	var afterFailure afterScriptFailure
	if errors.As(runErr, &afterFailure) {
		if taskErr != nil && (errors.Is(afterFailure.err, context.Canceled) || errors.Is(afterFailure.err, context.DeadlineExceeded)) {
			return "interrupted"
		}
		return "failed"
	}
	var followup followupFailure
	if errors.As(runErr, &followup) {
		followupStatus, _, followupPreparationErr, _, _, followupKnown := engines.RequestedOperationOutcome(followup.err)
		if followupKnown && followupStatus == engines.RequestedOperationInterrupted {
			return "interrupted"
		}
		if followupKnown && followupStatus == engines.RequestedOperationNotStarted &&
			taskCanceledStage(taskErr, followupPreparationErr) {
			return "interrupted"
		}
		if !followupKnown && taskErr != nil && (errors.Is(followup.err, context.Canceled) || errors.Is(followup.err, context.DeadlineExceeded)) {
			return "interrupted"
		}
		return "completed_with_issues"
	}
	requestedStatus, _, preparationErr, _, _, requestedKnown := engines.RequestedOperationOutcome(runErr)
	if requestedKnown {
		switch requestedStatus {
		case engines.RequestedOperationInterrupted:
			return "interrupted"
		case engines.RequestedOperationNotStarted:
			// Repository admission can preserve a cancellation cause while the
			// requested native child truth remains correctly not-started. Require
			// both causes so an unrelated preparation failure still wins a late
			// cancellation race.
			if taskCanceledStage(taskErr, preparationErr) {
				return "interrupted"
			}
			return "failed"
		case engines.RequestedOperationFailed:
			if engines.IsBackupSourceReadFailure(runErr) {
				return "completed_with_issues"
			}
			return "failed"
		}
	}
	if taskErr != nil && (errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded)) {
		return "interrupted"
	}
	return "failed"
}

func operationIDFromContext(ctx context.Context) string {
	value, _ := ctx.Value(operationIDContextKey{}).(string)
	return value
}

var startOperationStep = database.StartOperationStep
var startMetadataMutationStep = database.StartMetadataMutationStep
var finishOperationStep = database.FinishOperationStep
var skipOperationStep = database.SkipOperationStep
var closeOperationSessionScope = engines.CloseOperationSessionScope
var getNotificationSettings = database.GetSettings

func finishStep(ctx context.Context, db *sql.DB, kind, status, output string) error {
	if operationID := operationIDFromContext(ctx); operationID != "" {
		return finishOperationStep(db, operationID, kind, status, output, time.Now())
	}
	return nil
}

func startStep(ctx context.Context, db *sql.DB, domain, kind string) error {
	if operationID := operationIDFromContext(ctx); operationID != "" {
		return startOperationStep(db, operationID, domain, kind, time.Now())
	}
	return nil
}

func runJobScriptStep(ctx context.Context, db *sql.DB, kind, path string, required bool) error {
	if path == "" && !required {
		return nil
	}
	if err := startStep(ctx, db, "orchestration", kind); err != nil {
		return fmt.Errorf("persist %s start before launch: %w", kind, err)
	}
	err := jobscript.Run(ctx, path)
	status := "succeeded"
	result := "script launched and completed successfully; output was discarded"
	cancellationErr := ctx.Err()
	if cancellationErr != nil {
		status = "failed"
		result = "script was interrupted; output was discarded"
	} else if err != nil {
		status = "warning"
		result = err.Error() + "; output was discarded; failure is result-neutral"
		if required {
			status = "failed"
			result = err.Error() + "; output was discarded; this script is required"
		}
	}
	if persistErr := finishStep(ctx, db, kind, status, result); persistErr != nil {
		return errors.Join(cancellationErr, err, fmt.Errorf("persist %s completion: %w", kind, persistErr))
	}
	if cancellationErr != nil {
		return cancellationErr
	}
	if err != nil && required {
		return fmt.Errorf("required %s failed", strings.ReplaceAll(kind, "_", " "))
	}
	return nil
}

func combinedRunnerOutput(output string, err error) string {
	if err == nil {
		return output
	}
	if strings.TrimSpace(output) == "" {
		return err.Error()
	}
	return output + "\n" + err.Error()
}

type OperationFinisher func(*sql.DB, string, string, string, string, string, time.Time) error

func NewCoordinator(db *sql.DB, limit int) *Coordinator {
	return NewCoordinatorWithRuntime(db, limit, operationruntime.New())
}

func NewCoordinatorWithRuntime(db *sql.DB, limit int, runtimeManager *operationruntime.Manager) *Coordinator {
	if limit < database.MinMaxConcurrentJobRuns || limit > database.MaxMaxConcurrentJobRuns {
		limit = database.DefaultMaxConcurrentJobRuns
	}
	ctx, cancel := context.WithCancel(context.Background())
	if runtimeManager == nil {
		runtimeManager = operationruntime.New()
	}
	c := &Coordinator{db: db, limit: limit, active: map[string]*targetTask{}, vaultActive: map[string]bool{}, vaultWaiting: map[string]bool{}, vaultReady: map[string]func(){}, wake: make(chan struct{}, 1), stop: make(chan struct{}), dispatchDone: make(chan struct{}), queuedDone: make(chan struct{}), ctx: ctx, cancel: cancel, runtime: runtimeManager}
	go c.dispatch()
	return c
}

func Configure(db *sql.DB, limit int) {
	ConfigureWithRuntime(db, limit, operationruntime.New())
}

func ConfigureWithRuntime(db *sql.DB, limit int, runtimeManager *operationruntime.Manager) {
	coordinatorTransitionMu.Lock()
	defer coordinatorTransitionMu.Unlock()
	coordinatorMu.Lock()
	delete(coordinatorDisabled, db)
	coordinatorMu.Unlock()

	coordinatorMu.Lock()
	if coordinator != nil && coordinator.db == db {
		current := coordinator
		coordinatorMu.Unlock()
		if current.live() {
			current.SetLimit(limit)
			return
		}
		coordinatorMu.Lock()
		if coordinator == current {
			coordinator = nil
		}
		coordinatorMu.Unlock()
		_ = current.Shutdown(context.Background())
	} else {
		coordinatorMu.Unlock()
	}

	coordinatorMu.Lock()
	previous := coordinator
	coordinator = nil
	coordinatorMu.Unlock()
	if previous != nil {
		_ = previous.Shutdown(context.Background())
	}

	current := NewCoordinatorWithRuntime(db, limit, runtimeManager)
	coordinatorMu.Lock()
	coordinator = current
	coordinatorMu.Unlock()
}

// CurrentLimit reports the live process-wide admission limit.
func CurrentLimit(db *sql.DB) int {
	return ensureCoordinator(db).Limit()
}

// AcquireBackgroundNativeAdmission lets bounded UI-triggered native helper
// work share the existing process-wide backup admission authority. It creates
// no independent limit or queue and is released before runner shutdown.
func AcquireBackgroundNativeAdmission(ctx context.Context, db *sql.DB) (func(), error) {
	c, err := coordinatorForAdmission(db)
	if err != nil {
		return nil, err
	}
	for {
		c.mu.Lock()
		if c.closed {
			c.mu.Unlock()
			return nil, ErrAdmissionClosed
		}
		// Give every backup that can run now the first claim on available
		// capacity. A vault-blocked backup remains queued, so background work
		// holding that vault can finish instead of deadlocking with it.
		c.dispatchRunnableLocked()
		if !c.admitting && c.running < c.limit && !c.hasUnresolvedVaultLocked() {
			c.running++
			c.workWG.Add(1)
			released := false
			c.mu.Unlock()
			return func() {
				c.mu.Lock()
				if !released {
					released = true
					c.running--
					c.workWG.Done()
				}
				c.mu.Unlock()
				c.signal()
			}, nil
		}
		c.mu.Unlock()
		timer := time.NewTimer(25 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}

// Shutdown stops admission for the configured database and drains all owned
// runner and notification work before the database is closed.
func Shutdown(db *sql.DB) error {
	return ShutdownContext(context.Background(), db)
}

func ShutdownContext(ctx context.Context, db *sql.DB) error {
	coordinatorTransitionMu.Lock()
	defer coordinatorTransitionMu.Unlock()

	coordinatorMu.Lock()
	current := coordinator
	coordinatorDisabled[db] = true
	if current != nil && current.db == db {
		coordinator = nil
	}
	coordinatorMu.Unlock()
	if current == nil || current.db != db {
		return nil
	}
	return current.Shutdown(ctx)
}

func ensureCoordinator(db *sql.DB) *Coordinator {
	coordinatorTransitionMu.Lock()
	defer coordinatorTransitionMu.Unlock()
	return ensureCoordinatorLocked(db)
}

func ensureCoordinatorLocked(db *sql.DB) *Coordinator {
	coordinatorMu.Lock()
	if coordinator != nil && coordinator.db == db {
		current := coordinator
		coordinatorMu.Unlock()
		if current.live() {
			return current
		}
		coordinatorMu.Lock()
		if coordinator == current {
			coordinator = nil
		}
		coordinatorMu.Unlock()
		_ = current.Shutdown(context.Background())
	} else {
		coordinatorMu.Unlock()
	}

	coordinatorMu.Lock()
	previous := coordinator
	coordinator = nil
	coordinatorMu.Unlock()
	if previous != nil {
		_ = previous.Shutdown(context.Background())
	}

	limit := database.DefaultMaxConcurrentJobRuns
	if settings, err := database.GetSettings(db); err == nil {
		limit = settings.MaxConcurrentJobRuns
	}
	current := NewCoordinator(db, limit)
	coordinatorMu.Lock()
	coordinator = current
	coordinatorMu.Unlock()
	return current
}

func coordinatorForAdmission(db *sql.DB) (*Coordinator, error) {
	coordinatorTransitionMu.Lock()
	defer coordinatorTransitionMu.Unlock()
	coordinatorMu.Lock()
	disabled := coordinatorDisabled[db]
	coordinatorMu.Unlock()
	if disabled {
		return nil, ErrAdmissionClosed
	}
	return ensureCoordinatorLocked(db), nil
}

func (c *Coordinator) live() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.closed
}

func (c *Coordinator) SetLimit(limit int) {
	if !database.ValidMaxConcurrentJobRuns(limit) {
		return
	}
	c.mu.Lock()
	c.limit = limit
	c.mu.Unlock()
	c.signal()
}

func (c *Coordinator) Limit() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.limit
}

func (c *Coordinator) signal() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

func (c *Coordinator) dispatch() {
	defer close(c.dispatchDone)
	retry := time.NewTicker(250 * time.Millisecond)
	defer retry.Stop()
	for {
		c.mu.Lock()
		c.dispatchRunnableLocked()
		c.mu.Unlock()
		select {
		case <-c.wake:
		case <-retry.C:
		case <-c.stop:
			return
		}
	}
}

// dispatchRunnableLocked gives queued backup writers first use of every
// currently available process slot. Work blocked on its managed vault UUID stays
// queued and consumes no slot, allowing a current vault owner to complete.
func (c *Coordinator) dispatchRunnableLocked() {
	for !c.admitting && c.running < c.limit && len(c.queue) > 0 {
		task := c.takeRunnableLocked()
		if task == nil {
			return
		}
		c.running++
		c.workWG.Add(1)
		go c.run(task)
	}
}

// takeRunnableLocked preserves queue order while allowing work for other
// vaults to proceed. Vault contention keeps an operation visibly queued rather
// than rejecting it or consuming a global execution slot.
func (c *Coordinator) takeRunnableLocked() *targetTask {
	for index, task := range c.queue {
		if task.quarantined || task.cancelRequested.Load() {
			continue
		}
		if c.vaultActive[task.target.RepositoryID] {
			continue
		}
		if unlock := c.vaultReady[task.vaultID]; unlock != nil {
			delete(c.vaultReady, task.vaultID)
			task.unlock = unlock
			c.vaultActive[task.target.RepositoryID] = true
			c.queue = append(c.queue[:index], c.queue[index+1:]...)
			return task
		}
		if blocked, waiting := c.vaultWaiting[task.vaultID]; waiting {
			if !blocked {
				return nil // Preserve FIFO until the waiter reports acquired or blocked.
			}
			continue
		}
		// Admission always runs outside c.mu, including uncontended vaults.
		// The common exclusive path may need to cancel and drain indexing;
		// waiting here would block dispatch and completion for other vaults.
		c.vaultWaiting[task.vaultID] = false
		go c.reserveVault(task.vaultID)
		return nil
	}
	return nil
}

// An unresolved initial handoff is brief and owns no execution slot. Once
// contention is known, background work may use capacity to release that vault.
func (c *Coordinator) hasUnresolvedVaultLocked() bool {
	for _, task := range c.queue {
		if task.quarantined || task.cancelRequested.Load() {
			continue
		}
		if blocked, waiting := c.vaultWaiting[task.vaultID]; waiting && !blocked {
			return true
		}
	}
	return false
}

func (c *Coordinator) reserveVault(vaultID string) {
	unlock, err := vaultlock.AcquireExclusiveContext(c.ctx, vaultID, func() {
		c.mu.Lock()
		c.vaultWaiting[vaultID] = true
		c.mu.Unlock()
		c.signal()
	})
	release := unlock
	c.mu.Lock()
	delete(c.vaultWaiting, vaultID)
	// Acquisition can finish after the final queued task was canceled. Recheck
	// current demand before retaining a reservation that could strand the lock.
	if err == nil && !c.closed && c.queueNeedsVaultLocked(vaultID) {
		c.vaultReady[vaultID] = unlock
		release = nil
	}
	c.mu.Unlock()
	if release != nil {
		release()
	}
	c.signal()
}

func (c *Coordinator) queueNeedsVaultLocked(vaultID string) bool {
	for _, task := range c.queue {
		if task.vaultID == vaultID && !task.quarantined {
			return true
		}
	}
	return false
}

// Shutdown stops new admission, interrupts queued work, waits for the
// dispatcher and active tasks, and is safe to call repeatedly. Queued work is
// recorded as interrupted; active work is allowed to finish so its database
// ownership is released before cleanup.
func (c *Coordinator) Shutdown(ctx context.Context) error {
	c.shutdownOnce.Do(func() {
		c.admitMu.Lock()
		c.mu.Lock()
		c.closed = true
		c.admitting = true
		queued := append([]*targetTask(nil), c.queue...)
		c.queue = nil
		ready := make([]func(), 0, len(c.vaultReady))
		for vaultID, unlock := range c.vaultReady {
			ready = append(ready, unlock)
			delete(c.vaultReady, vaultID)
		}
		for _, task := range queued {
			delete(c.active, targetKey(task.job.ID, task.target.RepositoryID))
			delete(c.vaultActive, task.target.RepositoryID)
		}
		c.mu.Unlock()
		c.stopOnce.Do(func() { close(c.stop) })
		c.cancel()
		c.admitMu.Unlock()
		for _, unlock := range ready {
			unlock()
		}

		for _, task := range queued {
			if task.unlock != nil {
				task.unlock()
				task.unlock = nil
			}
			if task.resumeMetadata != nil {
				task.resumeMetadata()
				task.resumeMetadata = nil
			}
		}
		go c.finalizeQueued(queued)
	})

	select {
	case <-c.dispatchDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-c.queuedDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	workDone := make(chan struct{})
	go func() { c.workWG.Wait(); close(workDone) }()
	select {
	case <-workDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	return notifications.Wait(ctx)
}

func (c *Coordinator) finalizeQueued(queued []*targetTask) {
	defer close(c.queuedDone)
	if len(queued) == 0 {
		return
	}
	operationIDs := make([]string, 0, len(queued))
	for _, task := range queued {
		operationIDs = append(operationIDs, task.operationID)
	}
	if err := interruptQueuedOperations(
		c.db, operationIDs,
		"Application stopped before the operation started.", time.Now(),
	); err != nil {
		log.Printf("queued backups require startup reconciliation: %v", err)
		return
	}
	for _, task := range queued {
		task.cancel()
		c.runtime.CloseCancel(task.operationID)
		c.runtime.Remove(task.operationID)
	}
}

func targetKey(jobID, repositoryID string) string { return jobID + "\x00" + repositoryID }

func (c *Coordinator) prepareTaskRuntime(task *targetTask) error {
	task.ctx, task.cancel = context.WithCancel(c.ctx)
	return nil
}

func (c *Coordinator) registerTaskRuntimeLocked(task *targetTask) error {
	if err := c.runtime.Register(task.operationID, func() error {
		// Publish cancellation intent before waiting for the coordinator mutex.
		// Dispatch can therefore quarantine a request that arrived at the narrow
		// registration/publication release boundary even if it wins the mutex.
		task.cancelRequested.Store(true)
		return c.cancelTask(task)
	}); err != nil {
		task.cancel()
		return err
	}
	// Test synchronization at this exact release-order boundary is intentionally
	// narrow: the coordinator mutex is still held, so cancellation cannot
	// mistake an unpublished queued task for an activated one.
	afterTaskRuntimeRegistration(task)
	return nil
}

func (c *Coordinator) cancelTask(task *targetTask) error {
	c.mu.Lock()
	queuedIndex := -1
	for index, candidate := range c.queue {
		if candidate == task {
			queuedIndex = index
			break
		}
	}
	if queuedIndex < 0 {
		// Activation already owns the operation. The same pre-dispatch context
		// now closes final launch admission or terminates the running process tree.
		task.cancel()
		c.mu.Unlock()
		return nil
	}
	// Quarantine before the database transition so dispatch cannot select work
	// while queued cancellation and conditional activation arbitrate.
	task.quarantined = true
	c.mu.Unlock()

	if err := interruptQueuedOperations(
		task.db, []string{task.operationID}, "Operation canceled before it started.", time.Now(),
	); err != nil {
		// Failed persistence remains quarantined and explicitly retryable. It is
		// never returned to dispatch merely because the API attempt failed.
		return err
	}

	var readyUnlock func()
	c.mu.Lock()
	for index, candidate := range c.queue {
		if candidate == task {
			c.queue = append(c.queue[:index], c.queue[index+1:]...)
			break
		}
	}
	delete(c.active, targetKey(task.job.ID, task.target.RepositoryID))
	if !c.queueNeedsVaultLocked(task.vaultID) {
		readyUnlock = c.vaultReady[task.vaultID]
		delete(c.vaultReady, task.vaultID)
	}
	c.mu.Unlock()
	task.cancel()
	if readyUnlock != nil {
		readyUnlock()
	}
	if task.unlock != nil {
		task.unlock()
		task.unlock = nil
	}
	if task.resumeMetadata != nil {
		task.resumeMetadata()
		task.resumeMetadata = nil
	}
	// Terminal persistence and resource cleanup precede runtime removal.
	c.runtime.CloseCancel(task.operationID)
	c.runtime.Remove(task.operationID)
	c.signal()
	return nil
}

func (c *Coordinator) enqueue(db *sql.DB, job models.BackupJob, target models.BackupJobTarget) (string, error) {
	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	return c.enqueueLocked(db, job, target)
}

func (c *Coordinator) enqueueLocked(db *sql.DB, job models.BackupJob, target models.BackupJobTarget) (string, error) {
	key := targetKey(job.ID, target.RepositoryID)
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return "", ErrAdmissionClosed
	}
	if _, busy := c.active[key]; busy {
		c.mu.Unlock()
		return "", ErrAlreadyRunning
	}
	c.mu.Unlock()
	repo, repoErr := database.GetRepository(db, target.RepositoryID)
	if repoErr != nil {
		return "", repoErr
	}
	operationID, err := database.QueueBackupOperation(db,
		"Backup: "+job.Name+" → "+target.RepositoryName, job.ID, target.RepositoryID, time.Now())
	if err != nil {
		if errors.Is(err, database.ErrTargetRunActive) {
			return "", ErrAlreadyRunning
		}
		return "", err
	}
	// The managed vault UUID is the one process-local serialization identity.
	// Physical storage facts remain admission and availability evidence only;
	// using them here would split one vault after an explicit path reconnect.
	vaultID := repo.ID
	task := &targetTask{
		db: db, job: job, target: target, repository: repo,
		operationID: operationID, vaultID: vaultID,
	}
	if err := c.prepareTaskRuntime(task); err != nil {
		_ = persistTerminalState(task, "interrupted", "Backup runtime registration failed before dispatch.", time.Now())
		return "", err
	}
	task.resumeMetadata = metadata.DeferRepositorySyncForBackup(db, repo)
	c.mu.Lock()
	// Registration and queue publication share the coordinator mutex. A cancel
	// callback that becomes visible here cannot observe the task as neither
	// queued nor activated and incorrectly accept cancellation without the
	// durable queued-interruption transaction.
	if err := c.registerTaskRuntimeLocked(task); err != nil {
		c.mu.Unlock()
		task.resumeMetadata()
		task.resumeMetadata = nil
		_ = persistTerminalState(task, "interrupted", "Backup runtime registration failed before dispatch.", time.Now())
		return "", err
	}
	// The database transaction is the final duplicate guard. This second
	// check protects against a concurrent enqueue in this process.
	if _, busy := c.active[key]; busy {
		c.mu.Unlock()
		task.resumeMetadata()
		task.resumeMetadata = nil
		if finishErr := persistTerminalState(task, "interrupted", "Duplicate target admission was rejected.", time.Now()); finishErr != nil {
			log.Printf("duplicate backup marker requires startup reconciliation: %v", finishErr)
		}
		c.runtime.CloseCancel(task.operationID)
		c.runtime.Remove(task.operationID)
		return "", ErrAlreadyRunning
	}
	c.active[key] = task
	c.queue = append(c.queue, task)
	c.mu.Unlock()
	c.signal()
	return operationID, nil
}

// AdmitPersistedOperations runs one atomic database admission while runner
// admission is open, then activates the already-created queued operations in
// memory without inserting a second database operation.
func AdmitPersistedOperations(
	db *sql.DB,
	admit func() ([]database.TargetAdmissionResult, error),
) ([]database.TargetAdmissionResult, error) {
	if admit == nil {
		return nil, errors.New("backup admission callback is required")
	}
	c, err := coordinatorForAdmission(db)
	if err != nil {
		return nil, err
	}
	c.admitMu.Lock()
	defer c.admitMu.Unlock()
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil, ErrAdmissionClosed
	}
	c.admitting = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.admitting = false
		c.mu.Unlock()
		c.signal()
	}()

	results, err := admit()
	if err != nil {
		return nil, err
	}
	operationIDs := make([]string, 0, len(results))
	sourceFailures := make(map[string]string)
	targetFailures := make(map[string]string)
	for _, result := range results {
		if result.OperationID != "" {
			operationIDs = append(operationIDs, result.OperationID)
			if result.RequiresSourceFailureCheck {
				sourceFailures[result.OperationID] = result.ReasonCode
			} else if result.RequiresTargetFailureCheck {
				targetFailures[result.OperationID] = result.ReasonCode
			}
		}
	}
	if err := c.activatePersistedLocked(db, operationIDs, sourceFailures, targetFailures); err != nil {
		cleanupErr := database.InterruptQueuedBackupOperations(
			db, operationIDs,
			"Admission was persisted but could not be activated in this process; retry the backup.",
			time.Now(),
		)
		return results, errors.Join(err, cleanupErr)
	}
	return results, nil
}

func (c *Coordinator) activatePersistedLocked(
	db *sql.DB,
	operationIDs []string,
	sourceFailures, targetFailures map[string]string,
) error {
	if len(operationIDs) == 0 {
		return nil
	}
	tasks := make([]*targetTask, 0, len(operationIDs))
	frozenSources := map[string]string{}
	seenOperations := make(map[string]bool, len(operationIDs))
	seenTargets := make(map[string]bool, len(operationIDs))
	for _, operationID := range operationIDs {
		if operationID == "" || seenOperations[operationID] {
			return database.ErrInvalidTargets
		}
		seenOperations[operationID] = true
		operation, err := database.GetOperation(db, operationID)
		if err != nil {
			return err
		}
		if operation.Kind != "backup" || operation.Status != "queued" ||
			operation.JobID == "" || operation.RepositoryID == "" {
			return fmt.Errorf("persisted backup admission is not a queued target operation")
		}
		job, err := database.GetJob(db, operation.JobID)
		if err != nil {
			return err
		}
		sourcePath, frozen := frozenSources[job.ID]
		if !frozen {
			sourcePath = job.Source
			if job.ResolvedSourcePath != "" {
				sourcePath = job.ResolvedSourcePath
			}
			frozenSources[job.ID] = sourcePath
		}
		var target models.BackupJobTarget
		found := false
		for _, candidate := range job.Targets {
			if candidate.RepositoryID == operation.RepositoryID {
				target, found = candidate, true
				break
			}
		}
		if !found {
			return sql.ErrNoRows
		}
		key := targetKey(job.ID, target.RepositoryID)
		if seenTargets[key] {
			return database.ErrInvalidTargets
		}
		seenTargets[key] = true
		repo, err := database.GetRepository(db, target.RepositoryID)
		if err != nil {
			return err
		}
		vaultID := repo.ID
		tasks = append(tasks, &targetTask{
			db: db, job: job, target: target, repository: repo,
			operationID: operation.ID, vaultID: vaultID, sourcePath: sourcePath,
			sourceFailureReason: sourceFailures[operation.ID],
			targetFailureReason: targetFailures[operation.ID],
		})
	}
	for _, task := range tasks {
		if err := c.prepareTaskRuntime(task); err != nil {
			return err
		}
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		for _, task := range tasks {
			task.cancel()
		}
		return ErrAdmissionClosed
	}
	toActivate := make([]*targetTask, 0, len(tasks))
	for _, task := range tasks {
		key := targetKey(task.job.ID, task.target.RepositoryID)
		if active := c.active[key]; active != nil {
			if active.operationID == task.operationID {
				continue
			}
			c.mu.Unlock()
			for _, prepared := range tasks {
				prepared.cancel()
			}
			return ErrAlreadyRunning
		}
		toActivate = append(toActivate, task)
	}
	c.mu.Unlock()
	for _, task := range toActivate {
		task.resumeMetadata = metadata.DeferRepositorySyncForBackup(db, task.repository)
	}
	c.mu.Lock()
	registered := make([]*targetTask, 0, len(toActivate))
	for _, task := range toActivate {
		if err := c.registerTaskRuntimeLocked(task); err != nil {
			for _, prepared := range registered {
				prepared.cancel()
				c.runtime.CloseCancel(prepared.operationID)
				c.runtime.Remove(prepared.operationID)
			}
			c.mu.Unlock()
			for _, prepared := range toActivate {
				prepared.cancel()
				if prepared.resumeMetadata != nil {
					prepared.resumeMetadata()
					prepared.resumeMetadata = nil
				}
			}
			return err
		}
		registered = append(registered, task)
	}
	for _, task := range toActivate {
		key := targetKey(task.job.ID, task.target.RepositoryID)
		c.active[key] = task
		c.queue = append(c.queue, task)
	}
	c.mu.Unlock()
	return nil
}

func (c *Coordinator) run(task *targetTask) {
	defer c.workWG.Done()
	started := time.Now()
	if err := database.ActivateBackupOperation(task.db, task.operationID, task.job.ID, task.target.RepositoryID, started); err != nil {
		output := "Could not activate queued backup: " + err.Error()
		dispatchNotification := prepareBackupNotification(task, "failed")
		terminalPersisted := true
		if restoreErr := interruptQueuedOperations(
			task.db, []string{task.operationID}, output, time.Now(),
		); restoreErr != nil {
			terminalPersisted = false
			output += "\nQueued occurrence could not be restored; startup reconciliation is required."
			_ = database.LogError(task.db, "Backup queued-occurrence reconciliation required: "+restoreErr.Error())
		}
		if terminalPersisted && dispatchNotification != nil {
			dispatchNotification()
		}
		c.release(task)
		task.cancel()
		c.runtime.CloseCancel(task.operationID)
		if terminalPersisted {
			c.runtime.Remove(task.operationID)
		}
		return
	}
	operationCtx := engines.WithOperationSessionScope(task.ctx)
	operationCtx = context.WithValue(operationCtx, operationIDContextKey{}, task.operationID)
	operationCtx = contextWithCancelGateCloser(operationCtx, func() {
		c.runtime.CloseCancel(task.operationID)
	})
	operationCtx = command.ContextWithLiveOutput(operationCtx, func(stream, text string) {
		c.runtime.Append(task.operationID, stream, text)
	})
	operationCtx = command.ContextWithCapturedOutputPublisher(operationCtx, func(engine, kind, status, diagnostic string, stdout, stderr io.Reader) bool {
		return operationlog.StageNativeOutput(task.operationID, engine, kind, status, diagnostic, stdout, stderr) == nil
	})
	operationCtx = context.WithValue(operationCtx, runtimeRepositoryViewContextKey{}, &runtimeRepositoryView{})
	if task.sourcePath != "" {
		operationCtx = context.WithValue(operationCtx, frozenSourcePathContextKey{}, task.sourcePath)
	}
	if task.sourceFailureReason != "" {
		operationCtx = context.WithValue(operationCtx, sourceFailureReasonContextKey{}, task.sourceFailureReason)
	}
	if task.targetFailureReason != "" {
		operationCtx = context.WithValue(operationCtx, targetFailureReasonContextKey{}, task.targetFailureReason)
	}
	executorMu.RLock()
	currentExecutor := executor
	executorMu.RUnlock()
	var output string
	var runErr error
	output, runErr = currentExecutor(operationCtx, task.db, task.job, task.target)
	afterTargetExecutorReturn(task)
	requestedStatus, requestedStarted, _, _, _, requestedKnown := engines.RequestedOperationOutcome(runErr)
	preNative := !requestedKnown || requestedStatus == engines.RequestedOperationNotStarted || !requestedStarted
	var sourceUnavailable *storageavailability.SourceStorageUnavailableError
	var repositoryUnavailable *storageavailability.RepositoryStorageUnavailableError
	sourceMissing := preNative && errors.As(runErr, &sourceUnavailable)
	destinationMissing := preNative && errors.As(runErr, &repositoryUnavailable)
	if sourceMissing || destinationMissing {
		// Only typed, conclusively pre-native availability outcomes restore a
		// scheduled occurrence. Source and destination observations retain their
		// own persisted reason/timestamp columns; native failures never replay.
		reason := database.AvailabilityReasonStorageMissing
		if sourceMissing && sourceUnavailable.ReasonCode != "" {
			reason = sourceUnavailable.ReasonCode
		} else if destinationMissing && repositoryUnavailable.ReasonCode != "" {
			reason = repositoryUnavailable.ReasonCode
		}
		var restoreErr error
		if sourceMissing {
			restoreErr = database.RestorePreNativeSourceUnavailable(task.db, task.operationID, reason, time.Now().UTC())
		} else {
			restoreErr = database.RestorePreNativeUnavailable(task.db, task.operationID, reason, time.Now().UTC())
		}
		if restoreErr != nil {
			runErr = errors.Join(runErr, fmt.Errorf("restore unavailable catch-up occurrence: %w", restoreErr))
		}
	} else if consumeErr := database.ConsumeScheduledBackupOccurrence(task.db, task.operationID); consumeErr != nil {
		runErr = errors.Join(runErr, fmt.Errorf("close scheduled occurrence restoration: %w", consumeErr))
	}
	// No cancelable child may launch after the executor returns. Cleanup and
	// final persistence below intentionally do not use the user-canceled context.
	c.runtime.CloseCancel(task.operationID)
	// The durable terminal value records this operation's conclusive blocked
	// result; it is not a persisted vault-health flag.
	reconnectBlocked := (vaultprofile.IsReconnectRequired(runErr) || engines.IsReconnectRequired(runErr))
	if requestedStatus, _, _, _, _, known := engines.RequestedOperationOutcome(runErr); known &&
		requestedStatus != engines.RequestedOperationNotStarted {
		reconnectBlocked = false
	}
	reconnectBlocked = reconnectBlocked && !errors.Is(runErr, context.Canceled) && !errors.Is(runErr, context.DeadlineExceeded)
	status := "success"
	if c.ctx.Err() != nil {
		status = "interrupted"
		if runErr == nil {
			runErr = c.ctx.Err()
		}
	}
	if runErr != nil {
		if status != "interrupted" {
			status = classifyBackupRunStatus(runErr, task.ctx.Err())
		}
		if strings.TrimSpace(output) == "" {
			output = runErr.Error()
		} else {
			output += "\n" + runErr.Error()
		}
	}
	if closeErr := closeOperationSessionScope(operationCtx); closeErr != nil {
		// A failed engine cleanup can be transient (for example, a short-lived
		// Windows file handle). Retry once while preserving the first
		// error in the durable operation result.
		closeErr = errors.Join(closeErr, closeOperationSessionScope(operationCtx))
		if c.ctx.Err() != nil {
			status = "interrupted"
		} else if status == "success" {
			status = "completed_with_issues"
		} else if engines.IsBackupSourceReadFailure(runErr) {
			// Saved source-read failures qualify only in isolation. Cleanup is
			// independent orchestration failure, not native backup success.
			status = classifyBackupRunStatus(errors.Join(runErr, closeErr), task.ctx.Err())
		}
		runErr = errors.Join(runErr, closeErr)
		output = strings.TrimSpace(output + "\nEngine operation cleanup failed: " + closeErr.Error())
	}
	if status == "failed" && reconnectBlocked {
		status = "reconnect_required"
	}
	finished := time.Now()
	dispatchNotification := prepareBackupNotification(task, status)
	terminalPersisted := true
	if persistErr := persistTerminalState(task, status, output, finished); persistErr != nil {
		terminalPersisted = false
		status = "failed"
		warning := "Terminal state could not be persisted after bounded retries; startup reconciliation is required."
		output = strings.TrimSpace(output + "\n" + warning)
		log.Printf("backup terminal-state reconciliation required: %v", persistErr)
		_ = database.LogError(task.db, "Backup terminal-state reconciliation required: "+persistErr.Error())
	}
	switch status {
	case "success":
		log.Println("backup completed:", task.job.Name, task.target.RepositoryName)
	case "completed_with_issues":
		log.Println("backup completed with issues:", task.job.Name, task.target.RepositoryName, errors.Join(runErr))
	default:
		log.Println("backup failed:", task.job.Name, task.target.RepositoryName, errors.Join(runErr))
	}
	c.release(task)
	task.cancel()
	// Keep the closed runtime attached to an unreconciled active row; only a
	// durable terminal write permits removal from the shared operation view.
	if terminalPersisted {
		c.runtime.Remove(task.operationID)
	}
	if terminalPersisted && dispatchNotification != nil {
		dispatchNotification()
	}
}

// persistTerminalState retains ownership of the coordinator slot until the
// terminal operation and target status are durable. Fast retries absorb short
// SQLite contention; persistent storage failures retry at a slower cadence.
// A restart remains the final recovery path and marks the row interrupted.
func persistTerminalState(task *targetTask, status, output string, finished time.Time) error {
	return persistTerminalStateContext(context.Background(), task, status, output, finished)
}

func persistTerminalStateContext(ctx context.Context, task *targetTask, status, output string, finished time.Time) error {
	const maxAttempts = 6
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		finisherMu.RLock()
		currentFinisher := finisher
		finisherMu.RUnlock()
		err := currentFinisher(task.db, task.operationID, task.job.ID, task.target.RepositoryID, status, output, finished)
		if err == nil {
			return nil
		}
		log.Printf("could not persist terminal backup state (attempt %d): %v", attempt, err)
		if attempt == maxAttempts {
			return err
		}
		timer := time.NewTimer(time.Duration(attempt) * 50 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	return nil
}

func (c *Coordinator) release(task *targetTask) {
	if task.unlock != nil {
		task.unlock()
		task.unlock = nil
	}
	if task.resumeMetadata != nil {
		task.resumeMetadata()
		task.resumeMetadata = nil
	}
	c.mu.Lock()
	delete(c.active, targetKey(task.job.ID, task.target.RepositoryID))
	delete(c.vaultActive, task.target.RepositoryID)
	if c.running > 0 {
		c.running--
	}
	c.mu.Unlock()
	c.signal()
}

func prepareBackupNotification(task *targetTask, status string) func() {
	settings, err := getNotificationSettings(task.db)
	if err != nil {
		_ = database.SkipOperationStep(task.db, task.operationID, "application", "notification",
			"notification settings unavailable: "+err.Error(), time.Now())
		return nil
	}
	success := status == "success"
	native, webhook := settings.NotificationChannels(success)
	title := "Backup: " + task.job.Name + " → " + task.target.RepositoryName
	if !native && !webhook {
		return nil
	}
	nativeBackupSucceeded := status == "success"
	if steps, stepsErr := database.ListOperationSteps(task.db, task.operationID); stepsErr == nil {
		for _, step := range steps {
			if step.Domain == "native" && step.Kind == "backup" {
				nativeBackupSucceeded = step.Status == "succeeded"
				break
			}
		}
	}
	event := notifications.Event{
		Event: "backup", Status: status, Success: success,
		NativeBackupSucceeded: nativeBackupSucceeded,
		Title:                 title, OperationID: task.operationID,
		TaskKey: "notifications.task.backupTarget", TaskName: task.job.Name, TaskTarget: task.target.RepositoryName,
		Locale: locale.Effective(settings.Language),
	}
	if !notifications.ShouldNotify(event) {
		return nil
	}
	if err := database.StartOperationStep(task.db, task.operationID, "application", "notification", time.Now()); err != nil {
		_ = database.LogError(task.db, "Notification step could not be registered: "+err.Error())
		return nil
	}
	return func() {
		notifications.Dispatch(settings.WebhookURL, native, webhook, event, func(err error) {
			stepStatus, result := "succeeded", "notification delivery completed"
			if err != nil {
				stepStatus, result = "warning", err.Error()
				_ = database.LogError(task.db, "Notification failed: "+err.Error())
			}
			_ = database.FinishOperationStep(task.db, task.operationID, "notification", stepStatus, result, time.Now())
		})
	}
}

func WaitForNotifications() { _ = notifications.Wait(context.Background()) }

func EnqueueTarget(db *sql.DB, jobID, repositoryID string) (string, error) {
	job, err := database.GetJob(db, jobID)
	if err != nil {
		return "", err
	}
	c, err := coordinatorForAdmission(db)
	if err != nil {
		return "", err
	}
	for _, target := range job.Targets {
		if target.RepositoryID == repositoryID {
			return c.enqueue(db, job, target)
		}
	}
	return "", sql.ErrNoRows
}

// EnqueueAll admits every target from one settings snapshot. A duplicate in
// any pair returns ErrAlreadyRunning so the API can report a conflict.
func EnqueueAll(db *sql.DB, jobID string) (int, error) {
	job, err := database.GetJob(db, jobID)
	if err != nil {
		return 0, err
	}
	c, err := coordinatorForAdmission(db)
	if err != nil {
		return 0, err
	}
	return enqueueAllSnapshot(c, db, job, false)
}

type ScheduledResult struct {
	Accepted int
	Blocked  int
	Error    error
}

func EnqueueScheduled(db *sql.DB, job models.BackupJob) ScheduledResult {
	current, err := database.GetJob(db, job.ID)
	if err != nil {
		return ScheduledResult{Error: err}
	}
	c, err := coordinatorForAdmission(db)
	if err != nil {
		return ScheduledResult{Error: err}
	}
	accepted, err := enqueueAllSnapshot(c, db, current, true)
	result := ScheduledResult{Accepted: accepted}
	if errors.Is(err, ErrAlreadyRunning) {
		result.Blocked = len(current.Targets) - accepted
	} else {
		result.Error = err
	}
	return result
}

func enqueueAllSnapshot(c *Coordinator, db *sql.DB, job models.BackupJob, requireDue bool) (int, error) {
	if len(job.Targets) == 0 {
		return 0, database.ErrInvalidTargets
	}
	c.admitMu.Lock()
	c.mu.Lock()
	c.admitting = true
	defer func() {
		c.mu.Lock()
		c.admitting = false
		c.mu.Unlock()
		c.admitMu.Unlock()
		c.signal()
	}()
	for _, target := range job.Targets {
		if _, busy := c.active[targetKey(job.ID, target.RepositoryID)]; busy {
			c.mu.Unlock()
			return 0, ErrAlreadyRunning
		}
	}
	c.mu.Unlock()
	vaultIDs := make([]string, 0, len(job.Targets))
	repositories := make([]models.Repository, 0, len(job.Targets))
	for _, target := range job.Targets {
		repo, repoErr := database.GetRepository(db, target.RepositoryID)
		if repoErr != nil {
			return 0, repoErr
		}
		vaultID := repo.ID
		vaultIDs = append(vaultIDs, vaultID)
		repositories = append(repositories, repo)
	}
	queuedAt := time.Now()
	requests := make([]database.BackupOperationRequest, 0, len(job.Targets))
	for _, target := range job.Targets {
		requests = append(requests, database.BackupOperationRequest{
			Title: "Backup: " + job.Name + " → " + target.RepositoryName,
			JobID: job.ID, RepositoryID: target.RepositoryID, Engine: target.Engine,
		})
	}
	operationIDs, err := database.QueueBackupOperations(db, requests, queuedAt, job.Schedule, true,
		&database.BackupTriggerExpectation{Job: job, RequireDue: requireDue})
	if errors.Is(err, database.ErrTargetRunActive) {
		return 0, ErrAlreadyRunning
	}
	if err != nil {
		return 0, err
	}
	tasks := make([]*targetTask, 0, len(job.Targets))
	for index, target := range job.Targets {
		task := &targetTask{
			db: db, job: job, target: target, repository: repositories[index],
			operationID: operationIDs[index], vaultID: vaultIDs[index],
			resumeMetadata: metadata.DeferRepositorySyncForBackup(db, repositories[index]),
		}
		if err := c.prepareTaskRuntime(task); err != nil {
			for _, prepared := range tasks {
				prepared.cancel()
				if prepared.resumeMetadata != nil {
					prepared.resumeMetadata()
					prepared.resumeMetadata = nil
				}
				c.runtime.CloseCancel(prepared.operationID)
				c.runtime.Remove(prepared.operationID)
			}
			if task.resumeMetadata != nil {
				task.resumeMetadata()
				task.resumeMetadata = nil
			}
			return 0, err
		}
		tasks = append(tasks, task)
	}
	c.mu.Lock()
	registered := make([]*targetTask, 0, len(tasks))
	for _, task := range tasks {
		if err := c.registerTaskRuntimeLocked(task); err != nil {
			for _, prepared := range registered {
				prepared.cancel()
				c.runtime.CloseCancel(prepared.operationID)
				c.runtime.Remove(prepared.operationID)
			}
			c.mu.Unlock()
			for _, prepared := range tasks {
				prepared.cancel()
				if prepared.resumeMetadata != nil {
					prepared.resumeMetadata()
					prepared.resumeMetadata = nil
				}
			}
			return 0, err
		}
		registered = append(registered, task)
	}
	for _, task := range tasks {
		c.active[targetKey(job.ID, task.target.RepositoryID)] = task
		c.queue = append(c.queue, task)
	}
	c.mu.Unlock()
	return len(job.Targets), nil
}

// Start is retained as the API-facing name for Run All.
func Start(db *sql.DB, jobID string) error {
	_, err := EnqueueAll(db, jobID)
	return err
}

// RunSync now means scheduler submission. Scheduling never waits for a target
// to finish, which keeps the scheduler ticker responsive.
func RunSync(db *sql.DB, job models.BackupJob) {
	result := EnqueueScheduled(db, job)
	if result.Error != nil {
		log.Println("scheduled backup enqueue:", result.Error)
	}
}

func RunningJobs() map[string]string {
	coordinatorMu.Lock()
	c := coordinator
	coordinatorMu.Unlock()
	if c == nil {
		return map[string]string{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for key, task := range c.active {
		out[key] = task.operationID
	}
	return out
}

func IsRunning(jobID string) bool {
	for key := range RunningJobs() {
		if strings.HasPrefix(key, jobID+"\x00") {
			return true
		}
	}
	return false
}

func ActiveTargetStatuses(db *sql.DB) ([]models.BackupTargetStatus, error) {
	operations, err := database.ListActiveOperations(db)
	if err != nil {
		return nil, err
	}
	statuses := make([]models.BackupTargetStatus, 0, len(operations))
	for _, operation := range operations {
		if operation.Kind != "backup" || operation.JobID == "" || operation.RepositoryID == "" {
			continue
		}
		name := ""
		if repo, err := database.GetRepository(db, operation.RepositoryID); err == nil {
			name = repo.Name
		}
		statuses = append(statuses, models.BackupTargetStatus{
			JobID: operation.JobID, RepositoryID: operation.RepositoryID, RepositoryName: name,
			OperationID: operation.ID, Status: operation.Status,
		})
	}
	return statuses, nil
}

func SetExecutorForTests(next TargetExecutor) func() {
	executorMu.Lock()
	previous := executor
	if next == nil {
		executor = defaultExecutorContext
	} else {
		executor = func(_ context.Context, db *sql.DB, job models.BackupJob, target models.BackupJobTarget) (string, error) {
			return next(db, job, target)
		}
	}
	executorMu.Unlock()
	return func() {
		executorMu.Lock()
		executor = previous
		executorMu.Unlock()
	}
}

func SetOperationFinisherForTests(next OperationFinisher) func() {
	finisherMu.Lock()
	previous := finisher
	if next == nil {
		finisher = database.FinishBackupOperation
	} else {
		finisher = next
	}
	finisherMu.Unlock()
	return func() {
		finisherMu.Lock()
		finisher = previous
		finisherMu.Unlock()
	}
}

func defaultExecutor(db *sql.DB, job models.BackupJob, target models.BackupJobTarget) (string, error) {
	return defaultExecutorContext(context.Background(), db, job, target)
}

func defaultExecutorContext(ctx context.Context, db *sql.DB, job models.BackupJob, target models.BackupJobTarget) (string, error) {
	persistedJob, persistedJobErr := database.GetJob(db, job.ID)
	if persistedJobErr == nil {
		job = persistedJob
	} else if !errors.Is(persistedJobErr, sql.ErrNoRows) {
		return "", persistedJobErr
	}
	if err := models.ValidateRetentionPolicy(job); err != nil {
		return "", err
	}
	if operationIDFromContext(ctx) == "" {
		operationID, err := database.StartOperation(db, "backup", "Backup", job.ID, target.RepositoryID, time.Now())
		if err != nil {
			return "", err
		}
		ctx = context.WithValue(ctx, operationIDContextKey{}, operationID)
	}
	if err := startStep(ctx, db, "orchestration", "backup_admission"); err != nil {
		return "", fmt.Errorf("persist backup admission start: %w", err)
	}
	failAdmission := func(admissionErr error) (string, error) {
		if stepErr := finishStep(ctx, db, "backup_admission", "failed", admissionErr.Error()); stepErr != nil {
			admissionErr = errors.Join(admissionErr, fmt.Errorf("persist backup admission failure: %w", stepErr))
		}
		return "", admissionErr
	}
	repo, err := database.GetRepository(db, target.RepositoryID)
	if err != nil {
		return failAdmission(err)
	}
	if reason, _ := ctx.Value(sourceFailureReasonContextKey{}).(string); reason != "" {
		// The shared preliminary source resolution already conclusively failed.
		// Report it through the ordinary operation before destination admission,
		// hooks, or any native process can mask or follow that source failure.
		return failAdmission(&engines.RequestedOperationFailure{
			Engine: repo.Engine, Status: engines.RequestedOperationNotStarted,
			PreparationError: &storageavailability.SourceStorageFailureError{ReasonCode: reason},
		})
	}
	if reason, _ := ctx.Value(targetFailureReasonContextKey{}).(string); reason != "" {
		// As with source failures, inconsistent preliminary destination evidence is
		// conclusive for this attempt and must fail before scripts or native work.
		return failAdmission(&engines.RequestedOperationFailure{
			Engine: repo.Engine, Status: engines.RequestedOperationNotStarted,
			PreparationError: &storageavailability.RepositoryStorageFailureError{ReasonCode: reason},
		})
	}
	repo, err = repositoryadmission.AdmitUnderLockWithOptions(ctx, db, repo, repositoryadmission.Options{
		AssertControlPlane: assertVaultWriter,
		ResolveEngine:      resolveBackupEngine,
	})
	if err != nil {
		return failAdmission(&engines.RequestedOperationFailure{
			Engine: repo.Engine, Status: engines.RequestedOperationNotStarted, PreparationError: err,
		})
	}
	if err := database.RequireKopiaPolicyReady(db, repo.ID); err != nil {
		return failAdmission(err)
	}
	engine, err := resolveBackupEngine(repo)
	if err != nil {
		return failAdmission(err)
	}
	setRuntimeRepositoryView(ctx, repo)
	if err := finishStep(ctx, db, "backup_admission", "succeeded", "backup admitted to the native engine boundary"); err != nil {
		return "", fmt.Errorf("persist successful backup admission: %w", err)
	}

	activeResticRetention := repo.Engine == engines.ResticID && job.Retention > 0
	afterConfigured := job.AfterScriptPath != "" || job.AfterScriptMustSucceed
	if err := runJobScriptStep(ctx, db, "before_script", job.BeforeScriptPath, job.BeforeScriptMustSucceed); err != nil {
		closeCancelGateFromContext(ctx)
		reason := "before script prevented native backup launch"
		if operationID := operationIDFromContext(ctx); operationID != "" {
			err = errors.Join(err, skipOperationStep(db, operationID, "native", "backup", reason, time.Now()))
			if activeResticRetention {
				err = errors.Join(err, skipOperationStep(db, operationID, "native", "retention", reason, time.Now()))
			}
		}
		return "", err
	}

	operationID := operationIDFromContext(ctx)
	metadataGeneration, err := startMetadataMutationStep(db, operationID, repo.ID, "backup", time.Now())
	if err != nil {
		closeCancelGateFromContext(ctx)
		return "", fmt.Errorf("atomically reserve metadata generation with native backup start: %w", err)
	}
	acknowledgeNoStart := func(reason string) error {
		if err := startStep(ctx, db, "application", "metadata_cache"); err != nil {
			return err
		}
		covered, cacheErr := database.AcknowledgeMetadataGeneration(db, repo.ID, metadataGeneration)
		status, output := "succeeded", reason+"; reserved generation acknowledged"
		if cacheErr != nil {
			status, output = "warning", reason+"; metadata generation acknowledgement failed: "+cacheErr.Error()
		} else if !covered {
			status, output = "skipped", reason+"; generation remains behind an earlier cache gap"
		}
		return errors.Join(cacheErr, finishStep(ctx, db, "metadata_cache", status, output))
	}
	finishNoLaunch := func(reason string, cause error) error {
		stepErr := finishStep(ctx, db, "backup", "skipped", reason)
		if activeResticRetention {
			stepErr = errors.Join(stepErr, skipOperationStep(db, operationID, "native", "retention", reason, time.Now()))
		}
		return errors.Join(cause, stepErr, acknowledgeNoStart(reason))
	}

	sourcePath, _ := ctx.Value(frozenSourcePathContextKey{}).(string)
	if sourcePath == "" {
		resolved, resolveErr := storageavailability.ResolveSourcePath(ctx, job)
		if resolveErr != nil {
			_ = database.SetResolvedSourcePath(db, job.ID, job.Source, job.Source, time.Now().UTC())
			closeCancelGateFromContext(ctx)
			return "", finishNoLaunch("source resolution failed before native backup launch",
				storageavailability.ClassifySourceResolutionError(resolveErr))
		}
		sourcePath = resolved.Path
		if err := database.SetResolvedSourcePath(db, job.ID, job.Source, sourcePath, resolved.CheckedAt); err != nil {
			closeCancelGateFromContext(ctx)
			return "", finishNoLaunch("resolved source could not be persisted before native backup launch", err)
		}
	}
	// Writer identity is rechecked at the final child boundary, after native
	// preparation, so a local attachment change cannot launch a stale writer.
	ctx = engines.ContextWithSourceBackupAdmission(ctx, func(checkContext context.Context) error {
		if err := assertVaultWriter(checkContext, repo); err != nil {
			return err
		}
		return storageavailability.RequireSourcePathAvailable(checkContext, job, sourcePath)
	})
	// Storage and writer checks run first; cancellation is the final admission
	// decision so no requested child starts after a cancel request wins.
	ctx = command.ContextWithFinalCancellationAdmission(ctx)

	nativeContext, observedProcessStart := command.ContextWithProcessStartTracking(ctx)
	snapshot, backupOutput, backupErr := engine.Backup(nativeContext, repo, sourcePath, engines.BackupOptions{
		Tag: job.Tag, OwnerProfileID: repo.ProfileUUID, OwnerJobID: job.ID,
		Excludes: strings.Split(job.Excludes, "\n"), LogicalSource: job.Source,
		Settings: job.EngineSettings[repo.Engine],
	})
	if !activeResticRetention && !afterConfigured {
		// With no later script or Restic retention child, process return is the
		// last cancelable boundary. Result persistence and cache bookkeeping must
		// not remain able to accept a cancellation they cannot act upon.
		closeCancelGateFromContext(ctx)
	}
	backupStatus, backupStartedNative, preparationErr, nativeErr, followupErr, known :=
		engines.RequestedOperationOutcome(backupErr)
	if !known {
		backupStartedNative = observedProcessStart() || backupErr == nil
		if backupErr == nil {
			backupStatus = engines.RequestedOperationSucceeded
		} else if command.IsPreProcessAdmission(backupErr) && !backupStartedNative {
			backupStatus = engines.RequestedOperationNotStarted
			preparationErr = backupErr
		} else if errors.Is(backupErr, context.Canceled) || errors.Is(backupErr, context.DeadlineExceeded) {
			backupStatus = engines.RequestedOperationInterrupted
		} else {
			backupStatus = engines.RequestedOperationFailed
		}
		nativeErr = backupErr
	}
	if !backupStartedNative || (activeResticRetention && backupStatus != engines.RequestedOperationSucceeded && !afterConfigured) {
		// A not-started requested child makes both retention and the after hook
		// ineligible. Close immediately after parsing so a late request cannot
		// rewrite that already-established primary truth.
		closeCancelGateFromContext(ctx)
	}
	stepStatus := "succeeded"
	stepOutput := backupOutput
	switch backupStatus {
	case engines.RequestedOperationNotStarted:
		stepStatus = "skipped"
		stepOutput = combinedRunnerOutput(backupOutput, preparationErr)
	case engines.RequestedOperationInterrupted:
		if backupStartedNative {
			stepStatus = "interrupted"
			stepOutput = combinedRunnerOutput(backupOutput, nativeErr)
		} else {
			stepStatus = "skipped"
			stepOutput = combinedRunnerOutput(backupOutput, preparationErr)
		}
	case engines.RequestedOperationFailed:
		stepStatus = "failed"
		stepOutput = combinedRunnerOutput(backupOutput, nativeErr)
	}
	var backupResultPersistErr error
	if err := finishStep(ctx, db, "backup", stepStatus, stepOutput); err != nil {
		backupResultPersistErr = fmt.Errorf("persist native backup result: %w", err)
		backupErr = errors.Join(backupErr, backupResultPersistErr)
	}
	var backupOutputProcessingErr *command.OutputProcessingFailure
	if errors.As(followupErr, &backupOutputProcessingErr) {
		outputStepErr := startStep(ctx, db, "orchestration", "backup_output_processing")
		if outputStepErr == nil {
			outputStepErr = finishStep(ctx, db, "backup_output_processing", "failed",
				"native backup output could not be processed completely")
		}
		backupErr = errors.Join(backupErr, outputStepErr)
	}
	if !backupStartedNative {
		backupErr = errors.Join(backupErr, acknowledgeNoStart("native backup did not start"))
	}

	backupSucceeded := backupStatus == engines.RequestedOperationSucceeded
	var retentionErr error
	var retentionOutput string
	// Kopia may apply retention inside any started snapshot-create command.
	// Restic needs its separate requested command to have reached native work.
	retentionApplied := repo.Engine == engines.KopiaID && job.Retention > 0 && backupStartedNative
	if activeResticRetention {
		switch {
		case !backupSucceeded:
			retentionErr = skipOperationStep(db, operationID, "native", "retention",
				"native backup did not succeed; retention was not started", time.Now())
		case backupResultPersistErr != nil:
			// Retention must not mutate the repository until the successful backup
			// result is durable; startup recovery depends on that ordering.
			retentionErr = skipOperationStep(db, operationID, "native", "retention",
				"native backup result was not durable; retention was not started", time.Now())
		default:
			// This durable start intentionally shares the backup generation. A
			// second reservation could acknowledge only half of the compound mutation.
			if err := startStep(ctx, db, "native", "retention"); err != nil {
				startErr := fmt.Errorf("persist native retention start before launch: %w", err)
				retentionErr = errors.Join(startErr, skipOperationStep(db, operationID, "native", "retention",
					"native retention was not started because its durable start could not be recorded", time.Now()))
				break
			}
			retentionContext := engines.ContextWithNativeDeletionAdmission(ctx, func(checkContext context.Context) error {
				return assertVaultWriter(checkContext, repo)
			})
			retentionContext = command.ContextWithFinalCancellationAdmission(retentionContext)
			retentionOutput, retentionErr = engines.ApplyNativeRetention(retentionContext, engine, repo, job.ID,
				engines.RetentionPolicy{
					Latest: job.Retention, Hourly: job.RetentionHourly, Daily: job.RetentionDaily,
					Weekly: job.RetentionWeekly, Monthly: job.RetentionMonthly, Yearly: job.RetentionYearly,
				})
			status, retentionProcessStarted, retentionPreparationErr, retentionNativeErr, _, resultKnown :=
				engines.RequestedOperationOutcome(retentionErr)
			retentionApplied = retentionProcessStarted || !resultKnown
			retentionStepStatus := "succeeded"
			retentionStepOutput := retentionOutput
			if resultKnown {
				switch status {
				case engines.RequestedOperationNotStarted:
					retentionStepStatus = "skipped"
					retentionStepOutput = combinedRunnerOutput(retentionOutput, retentionPreparationErr)
				case engines.RequestedOperationInterrupted:
					if retentionProcessStarted {
						retentionStepStatus = "interrupted"
						retentionStepOutput = combinedRunnerOutput(retentionOutput, retentionNativeErr)
					} else {
						retentionStepStatus = "skipped"
						retentionStepOutput = combinedRunnerOutput(retentionOutput, retentionPreparationErr)
					}
				case engines.RequestedOperationFailed:
					retentionStepStatus = "failed"
					retentionStepOutput = combinedRunnerOutput(retentionOutput, retentionNativeErr)
				}
			} else if retentionErr != nil {
				retentionStepStatus = "failed"
			}
			if err := finishStep(ctx, db, "retention", retentionStepStatus, retentionStepOutput); err != nil {
				retentionErr = errors.Join(retentionErr, fmt.Errorf("persist native retention result: %w", err))
			}
			_, _, _, _, retentionFollowupErr, _ := engines.RequestedOperationOutcome(retentionErr)
			var retentionOutputProcessingErr *command.OutputProcessingFailure
			if errors.As(retentionFollowupErr, &retentionOutputProcessingErr) {
				outputStepErr := startStep(ctx, db, "orchestration", "retention_output_processing")
				if outputStepErr == nil {
					outputStepErr = finishStep(ctx, db, "retention_output_processing", "failed",
						"native retention output could not be processed completely")
				}
				retentionErr = errors.Join(retentionErr, outputStepErr)
			}
		}
	}
	if !afterConfigured {
		// Restic retention is the final possible child when configured. Closing
		// here precedes Vault Size, metadata, and logical-size bookkeeping.
		closeCancelGateFromContext(ctx)
	}
	if backupStartedNative {
		// Vault Size bookkeeping follows the complete native phase so no local
		// cache work is inserted between Restic backup and forget.
		if dirtyErr := database.MarkVaultSizeDirty(db, repo.ID); dirtyErr != nil {
			_ = database.LogWarning(db, "Vault Size cache could not be marked dirty after native backup started")
		}
	}

	// A complete header pass is intentional even when the backup returned one
	// exact snapshot ID: Restic forget and Kopia's create-time retention may also
	// have removed older snapshots. The low-priority coordinator normally rereads
	// newly written tree metadata from the engine cache and may use the repository
	// on a native cache miss. Failure leaves the pre-reserved generation dirty for
	// the next Restore/File History access; there is no retry loop or remote poller.
	if backupStartedNative {
		cacheStepErr := startStep(ctx, db, "application", "metadata_cache")
		// Reconciliation is required by native-start truth, not by whether its
		// diagnostic child row can be opened. Keep the submission independent so a
		// control-plane logging failure cannot silently suppress cache repair.
		queued := scheduleRepositorySync(db, repo, snapshot.ID, retentionApplied)
		if cacheStepErr == nil {
			if queued {
				cacheStepErr = finishStep(ctx, db, "metadata_cache", "succeeded",
					"post-backup metadata reconciliation queued; generation remains dirty until publication succeeds")
			} else {
				cacheStepErr = finishStep(ctx, db, "metadata_cache", "skipped",
					"post-backup metadata reconciliation was not queued; generation remains dirty for access-driven reconciliation")
			}
		}
		if cacheStepErr != nil {
			_ = database.LogWarning(db, "metadata cache pending-state result could not be recorded")
		}
	}
	if backupSucceeded && snapshot.LogicalSizeBytes != nil {
		if err := database.RecordJobTargetLogicalSize(db, job.ID, repo.ID, *snapshot.LogicalSizeBytes, time.Now()); err != nil {
			_ = database.LogWarning(db, "Job Size cache could not be updated after native backup")
		}
	} else if backupSucceeded && snapshot.LogicalSizeBytes == nil {
		// Missing/malformed summary facts are application warnings. Native
		// backup and job-tag-scoped retention truth remain conclusive.
		_ = database.LogWarning(db, "Native backup succeeded but Job Size or snapshot summary fields were unavailable")
	}

	var afterErr error
	if backupStartedNative && afterConfigured {
		if ctx.Err() != nil {
			afterErr = afterScriptSuppressed{errors.Join(ctx.Err(), skipOperationStep(db, operationID, "orchestration", "after_script",
				"after script was not launched because the operation is shutting down", time.Now()))}
		} else {
			afterErr = runJobScriptStep(ctx, db, "after_script", job.AfterScriptPath, job.AfterScriptMustSucceed)
			if afterErr != nil {
				afterErr = afterScriptFailure{afterErr}
			}
		}
		// The after hook is the last child when configured. Cancellation after
		// this point cannot suppress or terminate work and is rejected before
		// aggregate classification.
		closeCancelGateFromContext(ctx)
	} else if afterConfigured {
		// A hook is not eligible when native backup never started, so no later
		// cancelable child remains.
		closeCancelGateFromContext(ctx)
	}

	var resultErr error
	if !backupSucceeded {
		if backupErr != nil {
			resultErr = backupErr
		} else if preparationErr != nil {
			resultErr = preparationErr
		} else {
			resultErr = nativeErr
		}
		// Failure to record skipped retention is independent of the saved
		// partial snapshot and must prevent its amber-only qualification.
		resultErr = errors.Join(resultErr, retentionErr)
	} else {
		if backupResultPersistErr != nil {
			resultErr = errors.Join(resultErr, followupFailure{backupResultPersistErr})
		}
		if followupErr != nil {
			// A parser or cleanup failure after native success remains a separate
			// orchestration issue; it cannot rewrite the successful backup child.
			resultErr = errors.Join(resultErr, followupFailure{followupErr})
		}
		if retentionErr != nil {
			resultErr = errors.Join(resultErr, followupFailure{retentionErr})
		}
	}
	resultErr = errors.Join(resultErr, afterErr)
	return backupOutput, resultErr
}
