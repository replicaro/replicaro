package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultlock"
)

type syncIntent uint8

const (
	intentEntries syncIntent = iota + 1
	intentHeaders
	snapshotIndexReadersPerRepository = 4
)

type syncRequest struct {
	repository          models.Repository
	intent              syncIntent
	bypassEntryCooldown bool
	snapshotIDs         []string
	bucketCandidateOnly bool
	postBackup          bool
	backupSnapshotIDs   []string
	retainedSizing      bool
	accessRequest       bool
	accessNeededIDs     []string
	diagnosticQueuedAt  time.Time
}

type syncIntentContextKey struct{}

var repositoryLocks struct {
	sync.Mutex
	values map[string]*sync.Mutex
}

type backgroundCoordinator struct {
	mu            sync.Mutex
	running       map[string]bool
	activeCancel  map[string]context.CancelFunc
	activeDone    map[string]chan struct{}
	pending       map[string]syncRequest
	activeIntent  map[string]syncIntent
	activeRequest map[string]syncRequest
	activeStage   map[string]string
	stateRevision map[string]uint64
	paused        map[string]bool
	writers       map[string]int
	wg            sync.WaitGroup
	ctx           context.Context
	cancel        context.CancelFunc
	accept        bool
	started       bool
	done          chan struct{}
	sem           chan struct{}
	stopOnce      sync.Once
}

var backgroundSyncs struct {
	sync.Mutex
	byDB map[*sql.DB]*backgroundCoordinator
}

var diagnosticSink struct {
	sync.RWMutex
	fn func(string)
}

// IndexPhaseDiagnostic is deliberately limited to timing and aggregate counts.
// Native output, paths, repository identities, and credentials do not belong in
// this hook; callers can correlate an explicitly observed run themselves.
type IndexPhaseDiagnostic struct {
	Phase              string
	Duration           time.Duration
	Snapshots          int
	Entries            int
	ListedSnapshots    int
	ReusedEntrySets    int
	NativeOutputBytes  int
	PublicationSkipped bool
}

type indexPhaseDiagnosticObserver struct {
	mu      sync.Mutex
	observe func(IndexPhaseDiagnostic)
}

func (observer *indexPhaseDiagnosticObserver) deliver(diagnostic IndexPhaseDiagnostic) {
	// Two vault workers may finish phases concurrently. Serialize one observer's
	// callbacks so a simple collector does not need to reproduce coordinator
	// concurrency control merely to record diagnostics.
	observer.mu.Lock()
	defer observer.mu.Unlock()
	observer.observe(diagnostic)
}

type indexPhaseSpan struct {
	observer *indexPhaseDiagnosticObserver
	phase    string
	started  time.Time
	finished bool
}

var indexPhaseDiagnostics atomic.Pointer[indexPhaseDiagnosticObserver]

// SetIndexPhaseDiagnosticObserver installs an in-process observer. Replicaro
// never installs one during normal operation, keeping these permanent phase
// boundaries inert unless a benchmark, test, or explicit diagnostic session
// opts in. Restoring stops new spans when this registration is still current;
// a span that already captured the observer may finish afterward, so callers
// should drain the work they are observing before teardown.
func SetIndexPhaseDiagnosticObserver(observe func(IndexPhaseDiagnostic)) func() {
	previous := indexPhaseDiagnostics.Load()
	var next *indexPhaseDiagnosticObserver
	if observe != nil {
		next = &indexPhaseDiagnosticObserver{observe: observe}
	}
	indexPhaseDiagnostics.Store(next)
	return func() { indexPhaseDiagnostics.CompareAndSwap(next, previous) }
}

func beginIndexPhase(phase string) *indexPhaseSpan {
	return beginIndexPhaseAt(phase, time.Time{})
}

func beginIndexPhaseAt(phase string, started time.Time) *indexPhaseSpan {
	observer := indexPhaseDiagnostics.Load()
	if observer == nil {
		return &indexPhaseSpan{}
	}
	if started.IsZero() {
		started = time.Now()
	}
	return &indexPhaseSpan{observer: observer, phase: phase, started: started}
}

func (span *indexPhaseSpan) finish(diagnostic IndexPhaseDiagnostic) {
	if span == nil || span.observer == nil || span.finished {
		return
	}
	span.finished = true
	diagnostic.Phase = span.phase
	diagnostic.Duration = time.Since(span.started)
	span.observer.deliver(diagnostic)
}

var backgroundSync = SyncRepositoryContext
var admitRepository = repositoryadmission.AdmitUnderLock

func SetRepositoryAdmissionForTests(next func(context.Context, *sql.DB, models.Repository) (models.Repository, error)) func() {
	previous := admitRepository
	admitRepository = next
	return func() { admitRepository = previous }
}

var resolveMetadataEngine = func(repo models.Repository) (engines.Engine, error) {
	return engines.ResolveWithRepositoryAvailabilityCheck(
		repo, storageavailability.RequireRepositoryAvailable,
	)
}
var replaceMetadataSnapshot = database.ReplaceMetadataSnapshotForRefreshContext
var replaceMetadataSnapshotWithIdentity = database.ReplaceMetadataSnapshotWithIdentityForRefreshContext
var reuseMetadataSnapshotEntrySet = database.ReuseMetadataSnapshotEntrySetForRefreshContext
var metadataEntrySetExists = database.MetadataEntrySetExistsForSnapshot
var markMetadataSnapshotFailed = database.MarkMetadataSnapshotFailedForRefresh
var markMetadataRepositoryFailed = database.MarkMetadataRepositoryFailed
var reconcileMetadataHeadersComplete = database.ReconcileMetadataSnapshotHeadersCompleteForRefreshGenerationContext
var completeMetadataRefresh = database.CompleteMetadataRefreshContext
var metadataNow = time.Now

func init() {
	repositoryLocks.values = map[string]*sync.Mutex{}
	backgroundSyncs.byDB = map[*sql.DB]*backgroundCoordinator{}
}

func newBackgroundCoordinator() *backgroundCoordinator {
	return &backgroundCoordinator{
		running: map[string]bool{}, activeCancel: map[string]context.CancelFunc{},
		activeDone: map[string]chan struct{}{},
		pending:    map[string]syncRequest{}, activeIntent: map[string]syncIntent{},
		activeRequest: map[string]syncRequest{},
		activeStage:   map[string]string{}, stateRevision: map[string]uint64{}, paused: map[string]bool{}, writers: map[string]int{},
		accept: true, sem: make(chan struct{}, 2),
	}
}

func coordinatorFor(db *sql.DB) *backgroundCoordinator {
	backgroundSyncs.Lock()
	defer backgroundSyncs.Unlock()
	if value := backgroundSyncs.byDB[db]; value != nil {
		return value
	}
	value := newBackgroundCoordinator()
	value.ctx = context.Background()
	backgroundSyncs.byDB[db] = value
	return value
}

// SetDiagnosticSinkForTests captures one safe diagnostic per scheduled sync.
// Production continues to use the standard logger.
func SetDiagnosticSinkForTests(fn func(string)) func() {
	diagnosticSink.Lock()
	previous := diagnosticSink.fn
	diagnosticSink.fn = fn
	diagnosticSink.Unlock()
	return func() {
		diagnosticSink.Lock()
		diagnosticSink.fn = previous
		diagnosticSink.Unlock()
	}
}

func reportSyncError(repo models.Repository, err error) {
	err = withoutCooperativeCancellation(err)
	if err == nil {
		return
	}
	message := fmt.Sprintf("metadata synchronization failed for %s (%s): %v", repo.Name, repo.ID, err)
	diagnosticSink.RLock()
	sink := diagnosticSink.fn
	diagnosticSink.RUnlock()
	if sink != nil {
		sink(message)
		return
	}
	log.Print(message)
}

func withoutCooperativeCancellation(err error) error {
	if err == nil {
		return nil
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		remaining := make([]error, 0, len(joined.Unwrap()))
		for _, item := range joined.Unwrap() {
			if item = withoutCooperativeCancellation(item); item != nil {
				remaining = append(remaining, item)
			}
		}
		return errors.Join(remaining...)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, vaultlock.ErrLowPriorityYield) {
		return withoutCooperativeCancellation(errors.Unwrap(err))
	}
	return err
}

// ScheduleRepositorySync starts at most one synchronization for a repository.
// The caller must release any shared vault lock before invoking it.
func ScheduleRepositorySync(db *sql.DB, repo models.Repository) bool {
	return scheduleRepositorySync(db, syncRequest{repository: repo, intent: intentHeaders, bypassEntryCooldown: true}, false)
}

// ScheduleRepositorySyncAfterBackup preserves the complete mutation inventory
// pass. Only the preliminary sizing decision differs for Keep all: it uses the
// new backup header, while retention requires the retained maximum. Joining an
// already-pending complete request counts as success: each backup records that
// its reconciliation is retained even when writers coalesce into one pass.
func ScheduleRepositorySyncAfterBackup(db *sql.DB, repo models.Repository, snapshotID string, retention bool) bool {
	return scheduleRepositorySyncWithPendingJoin(db, syncRequest{repository: repo, intent: intentHeaders, bypassEntryCooldown: true,
		postBackup: true, backupSnapshotIDs: []string{snapshotID}, retainedSizing: retention}, false, true)
}

// ScheduleRepositoryAccess applies the shared Restore/File History freshness
// decision and respects the independent ordinary failure cooldowns.
func ScheduleRepositoryAccess(db *sql.DB, repo models.Repository) bool {
	admitted, _ := ScheduleRepositoryAccessWithReader(db, db, repo)
	return admitted
}

// ScheduleRepositoryAccessWithReader keeps coordinator ownership and all
// admitted work on db while allowing the access decision to use the API's
// query-only pool. This keeps Restore and File History able to join and observe
// an existing refresh while the metadata worker is using SQLite's sole writer.
func ScheduleRepositoryAccessWithReader(db, readDB *sql.DB, repo models.Repository) (bool, error) {
	state, err := database.RepositoryMetadataIndexState(context.Background(), readDB, repo.ID)
	if errors.Is(err, database.ErrMetadataIndexingUnavailable) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	request := syncRequest{repository: repo, intent: intentEntries, accessRequest: true}
	now := metadataNow().UTC()
	headerDue := !state.HeaderValid || state.HeaderStale
	headerCooling := retryCoolingDown(state.HeaderRetryAfter, now)
	if headerDue {
		if !headerCooling {
			request.intent = intentHeaders
		} else if !state.HeaderValid {
			return false, nil
		}
	}
	// A dirty generation can describe a native mutation whose local cache delta
	// rolled back. Ready cached headers do not explain that gap, so access must
	// first obtain the authoritative complete native header inventory. A prior
	// header failure still owns its persisted cooldown: repeatedly opening File
	// History must not turn a dirty generation into a native-listing hot loop.
	// Explicit Force remains the separate bypass.
	if state.RequiredGeneration != state.AppliedGeneration {
		if headerCooling {
			return false, nil
		}
		request.intent = intentHeaders
	}
	if request.intent == intentEntries {
		needed, err := database.MetadataSnapshotsNeedingIndex(readDB, repo.ID, now, false)
		if err != nil {
			return false, err
		}
		if len(needed) == 0 {
			return false, nil
		} else {
			request.accessNeededIDs = make([]string, 0, len(needed))
			for _, snapshot := range needed {
				request.accessNeededIDs = append(request.accessNeededIDs, snapshot.ID)
			}
		}
	}
	return scheduleRepositorySync(db, request, true), nil
}

func requestFollowup(active, next syncRequest) (syncRequest, bool) {
	if active.bucketCandidateOnly && !next.bucketCandidateOnly || active.postBackup && !next.postBackup && next.intent == intentHeaders {
		return next, true
	}
	if next.intent > active.intent {
		return next, true
	}
	if next.bypassEntryCooldown && !active.bypassEntryCooldown {
		// An active complete header listing already satisfies the Force header
		// requirement. Retain only the explicit entry catch-up so cooled failures
		// are attempted without duplicating that listing.
		if active.intent == intentHeaders && next.intent == intentHeaders {
			next.intent = intentEntries
		}
		return next, true
	}
	// A general Retry arriving during targeted mutation indexing still needs
	// one catch-up pass for every other eligible snapshot.
	if len(next.snapshotIDs) == 0 && len(active.snapshotIDs) > 0 {
		if next.accessRequest {
			activeIDs := make(map[string]bool, len(active.snapshotIDs))
			for _, snapshotID := range active.snapshotIDs {
				activeIDs[snapshotID] = true
			}
			allCovered := true
			for _, snapshotID := range next.accessNeededIDs {
				if !activeIDs[snapshotID] {
					allCovered = false
					break
				}
			}
			if allCovered {
				return syncRequest{}, false
			}
		}
		return next, true
	}
	// A targeted mutation delta may arrive after an equal-intent worker selected
	// its catch-up set, so preserve it in the one existing follow-up slot.
	if len(next.snapshotIDs) > 0 {
		return next, true
	}
	return syncRequest{}, false
}

// ForceRepositoryRefresh bypasses freshness and cooldowns. A stronger request
// arriving during entry-only work occupies the existing single pending-rerun
// slot; active header work is joined.
func ForceRepositoryRefresh(db *sql.DB, repo models.Repository) bool {
	return scheduleRepositorySync(db, syncRequest{
		repository: repo, intent: intentHeaders, bypassEntryCooldown: true,
	}, true)
}

func RetryRepositoryEntries(db *sql.DB, repo models.Repository) bool {
	return scheduleRepositorySync(db, syncRequest{
		repository: repo, intent: intentEntries, bypassEntryCooldown: true,
	}, true)
}

// ScheduleRepositoryEntryIndex queues only the entry index implied by one
// conclusive new backup header. It never requests a complete header listing.
func ScheduleRepositoryEntryIndex(db *sql.DB, repo models.Repository, snapshotID string) bool {
	if snapshotID == "" {
		return false
	}
	return scheduleRepositorySync(db, syncRequest{
		repository: repo, intent: intentEntries, snapshotIDs: []string{snapshotID},
	}, true)
}

func retryCoolingDown(value string, now time.Time) bool {
	retryAfter, err := time.Parse(time.RFC3339, value)
	return err == nil && now.Before(retryAfter)
}

func strongerRequest(current, next syncRequest) syncRequest {
	wasEmpty := current.intent == 0
	// A coalesced full refresh broadens sizing to retained headers, but any
	// pending backup still requires its new contents to be cached first.
	manualHeaders := current.intent == intentHeaders && !current.postBackup && !current.bucketCandidateOnly ||
		next.intent == intentHeaders && !next.postBackup && !next.bucketCandidateOnly
	if wasEmpty {
		current.bucketCandidateOnly = next.bucketCandidateOnly
	} else {
		current.bucketCandidateOnly = current.bucketCandidateOnly && next.bucketCandidateOnly
	}
	if current.diagnosticQueuedAt.IsZero() ||
		(!next.diagnosticQueuedAt.IsZero() && next.diagnosticQueuedAt.Before(current.diagnosticQueuedAt)) {
		current.diagnosticQueuedAt = next.diagnosticQueuedAt
	}
	currentWasGeneral := current.intent != 0 && len(current.snapshotIDs) == 0
	nextIsGeneral := next.intent != 0 && len(next.snapshotIDs) == 0
	if next.intent > current.intent {
		current.intent = next.intent
	}
	current.postBackup = current.postBackup || next.postBackup
	current.retainedSizing = current.retainedSizing || next.retainedSizing || manualHeaders
	current.backupSnapshotIDs = append(current.backupSnapshotIDs, next.backupSnapshotIDs...)
	current.repository = next.repository
	current.bypassEntryCooldown = current.bypassEntryCooldown || next.bypassEntryCooldown
	if currentWasGeneral || nextIsGeneral {
		current.snapshotIDs = nil
		return current
	}
	seen := make(map[string]bool, len(current.snapshotIDs)+len(next.snapshotIDs))
	merged := make([]string, 0, len(current.snapshotIDs)+len(next.snapshotIDs))
	for _, values := range [][]string{current.snapshotIDs, next.snapshotIDs} {
		for _, value := range values {
			if value != "" && !seen[value] {
				seen[value] = true
				merged = append(merged, value)
			}
		}
	}
	current.snapshotIDs = merged
	return current
}

func scheduleRepositorySync(db *sql.DB, request syncRequest, rerunIfActive bool) bool {
	return scheduleRepositorySyncWithPendingJoin(db, request, rerunIfActive, false)
}

func scheduleRepositorySyncWithPendingJoin(db *sql.DB, request syncRequest, rerunIfActive, acceptPendingJoin bool) bool {
	if request.diagnosticQueuedAt.IsZero() && indexPhaseDiagnostics.Load() != nil {
		request.diagnosticQueuedAt = time.Now()
	}
	repo := request.repository
	coordinator := coordinatorFor(db)
	key := backgroundRepositoryKey(repo)
	coordinator.mu.Lock()
	if !coordinator.accept {
		coordinator.mu.Unlock()
		return false
	}
	if coordinator.writers[key] > 0 {
		pending, alreadyPending := coordinator.pending[key]
		coordinator.pending[key] = strongerRequest(pending, request)
		setRepositoryPausedLocked(coordinator, key, true)
		coordinator.mu.Unlock()
		return acceptPendingJoin || rerunIfActive || !alreadyPending
	}
	if coordinator.running[key] {
		if coordinator.activeStage[key] == "rebuilding_entries" && (request.accessRequest || !request.postBackup) {
			coordinator.mu.Unlock()
			return rerunIfActive
		}
		if followup, retain := requestFollowup(coordinator.activeRequest[key], request); rerunIfActive && retain {
			coordinator.pending[key] = strongerRequest(coordinator.pending[key], followup)
			coordinator.mu.Unlock()
			return true
		}
		coordinator.mu.Unlock()
		// Explicit Force/Retry work joins an active request that already
		// satisfies it even when no additional rerun is needed.
		return rerunIfActive
	}
	ctx := coordinator.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	// Cancellation belongs to this vault from queue admission onward. Waiting
	// for a global slot is not native work and must not make a restore wait for
	// unrelated vaults. Active readers still close runDone only after draining.
	queueCtx, cancelQueued := context.WithCancel(ctx)
	coordinator.activeCancel[key] = cancelQueued
	setRepositoryRunningLocked(coordinator, key, true)
	coordinator.activeIntent[key] = request.intent
	coordinator.activeRequest[key] = request
	coordinator.activeStage[key] = "queued"
	runDone := make(chan struct{})
	coordinator.activeDone[key] = runDone
	coordinator.wg.Add(1)
	coordinator.mu.Unlock()
	go func() {
		defer coordinator.wg.Done()
		defer cancelQueued()
		defer func() {
			coordinator.mu.Lock()
			if coordinator.activeDone[key] == runDone {
				delete(coordinator.activeDone, key)
			}
			close(runDone)
			coordinator.mu.Unlock()
		}()
		finishQueued := func() {
			coordinator.mu.Lock()
			setRepositoryRunningLocked(coordinator, key, false)
			delete(coordinator.activeCancel, key)
			delete(coordinator.activeIntent, key)
			delete(coordinator.activeRequest, key)
			delete(coordinator.activeStage, key)
			// Deferral saved the active request together with any stronger pending
			// intent. Cancellation must not erase it. If the writer released before
			// this goroutine woke up, this cleanup owns the single resume instead.
			resume, pending := coordinator.pending[key]
			shouldResume := pending && coordinator.accept && ctx.Err() == nil && coordinator.writers[key] == 0
			if shouldResume || !coordinator.accept || ctx.Err() != nil {
				delete(coordinator.pending, key)
			}
			setRepositoryPausedLocked(coordinator, key, pending && !shouldResume && coordinator.accept && ctx.Err() == nil)
			coordinator.mu.Unlock()
			if shouldResume {
				scheduleRepositorySync(db, resume, true)
			}
		}
		select {
		case coordinator.sem <- struct{}{}:
			defer func() { <-coordinator.sem }()
		case <-queueCtx.Done():
			finishQueued()
			return
		}
		// The slot and cancellation may become ready together. Recheck under
		// the same mutex that transfers cancellation ownership to active work.
		current := request
		for {
			runCtx, cancelRun := context.WithCancel(ctx)
			coordinator.mu.Lock()
			if queueCtx.Err() != nil || coordinator.writers[key] > 0 {
				// A writer can arrive between coalesced iterations, before the next
				// active cancel is installed. Keep that selected next request too.
				coordinator.pending[key] = strongerRequest(coordinator.pending[key], current)
				coordinator.mu.Unlock()
				cancelRun()
				finishQueued()
				return
			}
			coordinator.activeIntent[key] = current.intent
			coordinator.activeRequest[key] = current
			coordinator.activeStage[key] = "preparing"
			coordinator.activeCancel[key] = cancelRun
			coordinator.mu.Unlock()
			runCtx = context.WithValue(runCtx, syncIntentContextKey{}, current)
			runErr := backgroundSync(runCtx, db, current.repository)
			cancelRun()
			reportSyncError(current.repository, runErr)
			coordinator.mu.Lock()
			delete(coordinator.activeCancel, key)
			delete(coordinator.activeIntent, key)
			delete(coordinator.activeRequest, key)
			delete(coordinator.activeStage, key)
			if errors.Is(runErr, vaultlock.ErrLowPriorityYield) && ctx.Err() == nil {
				coordinator.pending[key] = strongerRequest(coordinator.pending[key], current)
				setRepositoryPausedLocked(coordinator, key, true)
			}
			if coordinator.writers[key] > 0 {
				coordinator.pending[key] = strongerRequest(coordinator.pending[key], current)
				setRepositoryRunningLocked(coordinator, key, false)
				coordinator.mu.Unlock()
				return
			}
			next, rerun := coordinator.pending[key]
			if rerun && ctx.Err() == nil {
				delete(coordinator.pending, key)
				coordinator.mu.Unlock()
				current = next
				continue
			}
			setRepositoryRunningLocked(coordinator, key, false)
			setRepositoryPausedLocked(coordinator, key, false)
			delete(coordinator.pending, key)
			coordinator.mu.Unlock()
			return
		}
	}()
	return true
}

func setRepositoryRunningLocked(coordinator *backgroundCoordinator, key string, running bool) {
	if coordinator.running[key] == running {
		return
	}
	if running {
		coordinator.running[key] = true
	} else {
		delete(coordinator.running, key)
	}
	coordinator.stateRevision[key]++
}

func setRepositoryPausedLocked(coordinator *backgroundCoordinator, key string, paused bool) {
	if coordinator.paused[key] == paused {
		return
	}
	if paused {
		coordinator.paused[key] = true
	} else {
		delete(coordinator.paused, key)
	}
	coordinator.stateRevision[key]++
}

func reportRepositoryPause(db *sql.DB, repo models.Repository, paused bool) {
	coordinator := coordinatorFor(db)
	key := backgroundRepositoryKey(repo)
	coordinator.mu.Lock()
	setRepositoryPausedLocked(coordinator, key, paused)
	coordinator.mu.Unlock()
}

func backgroundRepositoryKey(repo models.Repository) string {
	if repo.ID != "" {
		return repo.ID
	}
	return repositoryLockKey(repo)
}

// DeferRepositorySyncForBackup cooperatively yields rebuildable metadata work
// while one or more admitted backup writers wait for or own the vault lock.
// The returned release must run after the backup writer releases that lock.
func DeferRepositorySyncForBackup(db *sql.DB, repo models.Repository) func() {
	release, _ := deferRepositorySync(db, repo)
	return release
}

// DeferRepositorySyncForRestore cancels and drains same-repository rebuildable
// metadata work before restore performs its one nonblocking exclusive lock try.
// The returned release must run after the restore releases that lock.
func DeferRepositorySyncForRestore(
	ctx context.Context,
	db *sql.DB,
	repo models.Repository,
) (func(), error) {
	release, drained := deferRepositorySync(db, repo)
	select {
	case <-drained:
		return release, nil
	case <-ctx.Done():
		release()
		return nil, ctx.Err()
	}
}

func deferRepositorySync(db *sql.DB, repo models.Repository) (func(), <-chan struct{}) {
	coordinator := coordinatorFor(db)
	key := backgroundRepositoryKey(repo)
	cancels := []context.CancelFunc{}
	coordinator.mu.Lock()
	coordinator.writers[key]++
	drained := coordinator.activeDone[key]
	if drained == nil {
		alreadyDrained := make(chan struct{})
		close(alreadyDrained)
		drained = alreadyDrained
	}
	if coordinator.running[key] && coordinator.activeCancel[key] != nil {
		active := coordinator.activeRequest[key]
		if active.repository.ID == "" {
			active = syncRequest{repository: repo, intent: coordinator.activeIntent[key]}
		}
		coordinator.pending[key] = strongerRequest(coordinator.pending[key], active)
	}
	if coordinator.running[key] || coordinator.pending[key].intent != 0 {
		setRepositoryPausedLocked(coordinator, key, true)
	}
	if cancel := coordinator.activeCancel[key]; cancel != nil {
		cancels = append(cancels, cancel)
	}
	// Publication writers are per-vault now. Preserve same-vault cooperative
	// yielding and coalescing, while an unrelated vault's cache transaction can
	// continue on its independent writer connection.
	coordinator.mu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			resumes := map[string]syncRequest{}
			coordinator.mu.Lock()
			if coordinator.writers[key] > 1 {
				coordinator.writers[key]--
			} else {
				delete(coordinator.writers, key)
				resume, shouldResume := coordinator.pending[key]
				if shouldResume && !coordinator.running[key] {
					resumes[key] = resume
					delete(coordinator.pending, key)
				}
				if !shouldResume {
					setRepositoryPausedLocked(coordinator, key, false)
				}
			}
			accept := coordinator.accept
			coordinator.mu.Unlock()
			if accept {
				for _, resume := range resumes {
					scheduleRepositorySync(db, resume, false)
				}
			}
		})
	}
	return release, drained
}

type CoordinatorState struct {
	Running               bool   `json:"running"`
	Pending               bool   `json:"pending"`
	Paused                bool   `json:"paused"`
	Stage                 string `json:"stage"`
	CompleteHeaderListing bool   `json:"completeHeaderListing"`
}

type coordinatorObservation struct {
	state    CoordinatorState
	revision uint64
}

func repositoryCoordinatorObservation(db *sql.DB, repo models.Repository) coordinatorObservation {
	coordinator := coordinatorFor(db)
	key := backgroundRepositoryKey(repo)
	coordinator.mu.Lock()
	defer coordinator.mu.Unlock()
	_, pending := coordinator.pending[key]
	stage := coordinator.activeStage[key]
	if stage == "publishing_entries" || stage == "rebuilding_entries" {
		stage = "indexing_entries"
	}
	return coordinatorObservation{state: CoordinatorState{
		Running: coordinator.running[key], Pending: pending,
		Paused: coordinator.paused[key] && (coordinator.running[key] || pending),
		Stage:  stage,
		CompleteHeaderListing: coordinator.activeIntent[key] == intentHeaders ||
			coordinator.pending[key].intent == intentHeaders,
	}, revision: coordinator.stateRevision[key]}
}

func RepositoryCoordinatorState(db *sql.DB, repo models.Repository) CoordinatorState {
	return repositoryCoordinatorObservation(db, repo).state
}

var repositoryStatusSnapshotBarrier func()

// RepositoryStatusSnapshot brackets one coherent SQLite status snapshot with
// coordinator observations. If work was active or crossed the database read,
// it returns a conservative nonterminal state so callers poll once more.
func RepositoryStatusSnapshot(ctx context.Context, db *sql.DB, repo models.Repository) (database.MetadataIndexState, []string, CoordinatorState, error) {
	return RepositoryStatusSnapshotWithReader(ctx, db, db, repo)
}

// RepositoryStatusSnapshotWithReader keeps coordinator observation attached to
// the writer identity while allowing the coherent SQLite snapshot to use the
// API's read-only pool.
func RepositoryStatusSnapshotWithReader(ctx context.Context, coordinatorDB, readDB *sql.DB, repo models.Repository) (database.MetadataIndexState, []string, CoordinatorState, error) {
	before := repositoryCoordinatorObservation(coordinatorDB, repo)
	state, readySnapshotIDs, err := database.RepositoryMetadataStatusSnapshot(ctx, readDB, repo.ID)
	if errors.Is(err, database.ErrMetadataIndexingUnavailable) {
		err = nil
		readySnapshotIDs = []string{}
	}
	if err != nil {
		return database.MetadataIndexState{}, nil, CoordinatorState{}, err
	}
	if repositoryStatusSnapshotBarrier != nil {
		repositoryStatusSnapshotBarrier()
	}
	after := repositoryCoordinatorObservation(coordinatorDB, repo)
	coordinatorState := after.state
	activeObserved := before.state.Running || before.state.Pending || before.state.Paused ||
		after.state.Running || after.state.Pending || after.state.Paused
	if activeObserved || before.revision != after.revision {
		coordinatorState.Running = before.state.Running || after.state.Running
		coordinatorState.Pending = before.state.Pending || after.state.Pending || !coordinatorState.Running
		coordinatorState.Paused = before.state.Paused || after.state.Paused
		if coordinatorState.Stage == "" {
			coordinatorState.Stage = before.state.Stage
		}
		coordinatorState.CompleteHeaderListing = before.state.CompleteHeaderListing || after.state.CompleteHeaderListing
	}
	return state, readySnapshotIDs, coordinatorState, nil
}

// WaitForBackgroundSyncs is intended for shutdown and deterministic tests.
func WaitForBackgroundSyncs() {
	_ = WaitForBackgroundSyncsContext(context.Background())
}

// WaitForBackgroundSyncsContext waits for admitted work without outliving ctx.
func WaitForBackgroundSyncsContext(ctx context.Context) error {
	backgroundSyncs.Lock()
	coordinators := make([]*backgroundCoordinator, 0, len(backgroundSyncs.byDB))
	for _, coordinator := range backgroundSyncs.byDB {
		coordinators = append(coordinators, coordinator)
	}
	backgroundSyncs.Unlock()
	for _, coordinator := range coordinators {
		done := make(chan struct{})
		go func(coordinator *backgroundCoordinator) {
			coordinator.wg.Wait()
			close(done)
		}(coordinator)
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func lockRepositoryContext(ctx context.Context, id string) (func(), error) {
	repositoryLocks.Lock()
	lock := repositoryLocks.values[id]
	if lock == nil {
		lock = &sync.Mutex{}
		repositoryLocks.values[id] = lock
	}
	repositoryLocks.Unlock()
	for {
		if lock.TryLock() {
			return lock.Unlock, nil
		}
		timer := time.NewTimer(5 * time.Millisecond)
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

func lockRepository(id string) func() {
	unlock, _ := lockRepositoryContext(context.Background(), id)
	return unlock
}

// Start owns the coordinator lifecycle. Metadata work is access-driven; start
// and shutdown never launch provider reads.
func Start(db *sql.DB) func() {
	stop := StartContext(db)
	return func() { _ = stop(context.Background()) }
}

// StartContext returns a stop function that cancels admission and joins
// background work without waiting beyond the caller's context.
func StartContext(db *sql.DB) func(context.Context) error {
	coordinator := coordinatorFor(db)
	coordinator.mu.Lock()
	if coordinator.started {
		coordinator.mu.Unlock()
		// A stopped coordinator is terminal. Repeated starts are intentionally
		// rejected by returning a no-op owner; only a fresh database identity can
		// obtain a new lifecycle.
		return func(context.Context) error { return nil }
	}
	ctx, cancel := context.WithCancel(context.Background())
	coordinator.started = true
	coordinator.ctx = ctx
	coordinator.cancel = cancel
	coordinator.accept = true
	done := make(chan struct{})
	coordinator.done = done
	coordinator.mu.Unlock()

	go func() {
		defer close(done)
		<-ctx.Done()
	}()
	stopDone := make(chan struct{})
	return func(stopCtx context.Context) error {
		coordinator.stopOnce.Do(func() {
			coordinator.mu.Lock()
			coordinator.accept = false
			stop := coordinator.cancel
			coordinator.cancel = nil
			coordinator.ctx = nil
			coordinator.mu.Unlock()
			if stop != nil {
				stop()
			}
			go func() {
				<-done
				coordinator.wg.Wait()
				close(stopDone)
			}()
		})
		select {
		case <-stopDone:
			return nil
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	}
}

// SyncRepository indexes missing or failed snapshots and removes metadata for
// snapshots that no longer exist in the source repository.
func SyncRepository(db *sql.DB, repo models.Repository) error {
	return SyncRepositoryContext(context.Background(), db, repo)
}

func SyncRepositoryContext(ctx context.Context, db *sql.DB, repo models.Repository) error {
	// Metadata reads and reconciliation share the vault read lock. This keeps a
	// slow engine listing from making ordinary search/history reads return 409.
	// User-visible mutations still take the exclusive lock and block this work.
	if repo.ID == "" {
		return fmt.Errorf("managed vault UUID is required for metadata synchronization")
	}
	lockKey := repositoryLockKey(repo)
	workContext, unlock, err := vaultlock.AcquireLowPrioritySharedContext(ctx, lockKey, func(paused bool) {
		reportRepositoryPause(db, repo, paused)
	})
	if err != nil {
		return err
	}
	defer unlock()
	current, err := database.GetRepository(db, repo.ID)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil
		}
		return err
	}
	if repositoryLockKey(current) != repositoryLockKey(repo) {
		return nil
	}
	repo = current
	err = syncRepositoryRecorded(workContext, db, repo)
	if errors.Is(context.Cause(workContext), vaultlock.ErrLowPriorityYield) {
		return errors.Join(vaultlock.ErrLowPriorityYield, withoutCooperativeCancellation(err))
	}
	return err
}

// SyncRepositoryUnderExclusive is used by the runner while it already owns
// the repository's exclusive mutation lock.
func SyncRepositoryUnderExclusive(db *sql.DB, repo models.Repository) error {
	return SyncRepositoryUnderExclusiveContext(context.Background(), db, repo)
}

func SyncRepositoryUnderExclusiveContext(ctx context.Context, db *sql.DB, repo models.Repository) error {
	return syncRepositoryRecorded(ctx, db, repo)
}

func syncRepositoryRecorded(ctx context.Context, db *sql.DB, repo models.Repository) error {
	admitted, admissionErr := admitRepository(ctx, db, repo)
	if admissionErr != nil {
		return admissionErr
	}
	repo = admitted
	err := syncRepository(ctx, db, repo)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	var refreshErr *repositoryRefreshError
	if errors.As(err, &refreshErr) {
		recordErr := markMetadataRepositoryFailed(db, repo.ID, err.Error(), metadataNow().UTC())
		return errors.Join(err, recordErr)
	}
	return err
}

func repositoryLockKey(repo models.Repository) string {
	return repo.ID
}

func visibleSnapshotsForProfile(db *sql.DB, repo models.Repository, snapshots []models.Snapshot) ([]models.Snapshot, error) {
	knownJobs, err := database.JobIDsForRepository(db, repo.ID)
	if err != nil {
		return nil, err
	}
	visible := make([]models.Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		snapshot = models.PresentSnapshot(snapshot, repo.ProfileUUID, knownJobs)
		if snapshot.Presentation != models.SnapshotPresentationHidden {
			visible = append(visible, snapshot)
		}
	}
	return visible, nil
}

func setCoordinatorStage(db *sql.DB, repo models.Repository, stage string) {
	coordinator := coordinatorFor(db)
	key := backgroundRepositoryKey(repo)
	coordinator.mu.Lock()
	if coordinator.running[key] {
		coordinator.activeStage[key] = stage
	}
	coordinator.mu.Unlock()
}

func syncRepository(ctx context.Context, db *sql.DB, repo models.Repository) error {
	unlock, err := lockRepositoryContext(ctx, repositoryLockKey(repo))
	if err != nil {
		return err
	}
	defer unlock()
	return syncRepositoryLocked(ctx, db, repo)
}

// The caller holds the coordinator's local repository serialization as well as
// the enclosing operation's vault admission. A fresh bucket-candidate pass keeps
// both admissions and reuses this body without reacquiring either lock.
func syncRepositoryLocked(ctx context.Context, db *sql.DB, repo models.Repository) (resultErr error) {
	request, ok := ctx.Value(syncIntentContextKey{}).(syncRequest)
	if !ok {
		request = syncRequest{repository: repo, intent: intentHeaders, bypassEntryCooldown: true}
	}
	totalPhase := beginIndexPhaseAt("metadata_refresh", request.diagnosticQueuedAt)
	defer totalPhase.finish(IndexPhaseDiagnostic{})
	uiPhaseName := "indexing_snapshot_contents"
	if request.intent == intentHeaders {
		uiPhaseName = "indexing_snapshots"
	}
	uiPhase := beginIndexPhaseAt(uiPhaseName, request.diagnosticQueuedAt)
	uiDiagnostic := IndexPhaseDiagnostic{}
	defer func() { uiPhase.finish(uiDiagnostic) }()

	state, err := database.RepositoryMetadataIndexState(ctx, db, repo.ID)
	if err != nil {
		return err
	}
	if request.intent == intentEntries && !state.HeaderValid {
		return nil
	}
	observedGeneration := state.RequiredGeneration
	var needed []models.Snapshot
	if request.intent == intentEntries {
		needed, err = database.MetadataSnapshotsNeedingIndex(db, repo.ID, metadataNow(), request.bypassEntryCooldown)
		if len(request.snapshotIDs) > 0 {
			selected := make(map[string]bool, len(request.snapshotIDs))
			for _, snapshotID := range request.snapshotIDs {
				selected[snapshotID] = true
			}
			filtered := needed[:0]
			for _, snapshot := range needed {
				if selected[snapshot.ID] {
					filtered = append(filtered, snapshot)
				}
			}
			needed = filtered
		}
		if err != nil {
			return err
		}
		if len(needed) == 0 {
			// Entry-only work can make known headers ready, but it cannot certify
			// that an unexplained generation gap contains no missing/deleted header.
			return nil
		}
	}
	manager, resolveErr := resolveMetadataEngine(repo)
	if resolveErr != nil {
		if request.intent == intentHeaders {
			return &repositoryRefreshError{err: resolveErr}
		}
		attemptedAt := metadataNow().UTC()
		var persistenceErr error
		for _, snapshot := range needed {
			persistenceErr = errors.Join(persistenceErr,
				database.MarkMetadataSnapshotFailedAt(db, repo.ID, snapshot, resolveErr, attemptedAt))
		}
		return errors.Join(resolveErr, persistenceErr)
	}
	var indexSession engines.SnapshotIndexSession
	indexSessionClosed := false
	closeIndexSession := func() error {
		if indexSession == nil || indexSessionClosed {
			return nil
		}
		indexSessionClosed = true
		if closeErr := indexSession.Close(); closeErr != nil {
			return fmt.Errorf("close snapshot index session: %w", closeErr)
		}
		return nil
	}
	defer func() {
		closeErr := closeIndexSession()
		if closeErr != nil {
			resultErr = errors.Join(resultErr, closeErr,
				database.MarkMetadataRepositoryFailed(db, repo.ID, closeErr.Error(), metadataNow().UTC()))
		}
	}()
	indexedSnapshots := []engines.SnapshotIndexItem{}
	completeHeaderRefresh := request.intent == intentHeaders
	contentIndexRequired := true
	if request.intent == intentHeaders {
		setCoordinatorStage(db, repo, "refreshing_headers")
		var generationErr error
		observedGeneration, _, generationErr = database.MetadataGenerations(db, repo.ID)
		if generationErr != nil {
			return &repositoryRefreshError{err: generationErr}
		}
		output := ""
		headerListingPhase := beginIndexPhase("snapshot_header_listing")
		if session, sessionOutput, supported, beginErr := engines.BeginSnapshotIndex(ctx, manager, repo); supported {
			indexSession, output, err = session, sessionOutput, beginErr
			if err == nil {
				indexedSnapshots = indexSession.Snapshots()
			}
		} else {
			var snapshots []models.Snapshot
			snapshots, output, err = manager.ListSnapshots(ctx, repo)
			for _, snapshot := range snapshots {
				indexedSnapshots = append(indexedSnapshots, engines.SnapshotIndexItem{Snapshot: snapshot})
			}
		}
		headerListingPhase.finish(IndexPhaseDiagnostic{
			Snapshots: len(indexedSnapshots), NativeOutputBytes: len(output),
		})
		if err != nil {
			return &repositoryRefreshError{err: &engineError{output: output, err: err}}
		}
		headerPublicationPhase := beginIndexPhase("snapshot_header_publication")
		headerPublicationDiagnostic := IndexPhaseDiagnostic{}
		defer func() { headerPublicationPhase.finish(headerPublicationDiagnostic) }()
		visible, classifyErr := visibleSnapshotsForProfile(db, repo, func() []models.Snapshot {
			values := make([]models.Snapshot, 0, len(indexedSnapshots))
			for _, item := range indexedSnapshots {
				values = append(values, item.Snapshot)
			}
			return values
		}())
		if classifyErr != nil {
			return &repositoryRefreshError{err: fmt.Errorf("classify snapshot presentation: %w", classifyErr)}
		}
		visibleByID := make(map[string]models.Snapshot, len(visible))
		for _, snapshot := range visible {
			visibleByID[snapshot.ID] = snapshot
		}
		filteredItems := indexedSnapshots[:0]
		for _, item := range indexedSnapshots {
			if snapshot, ok := visibleByID[item.Snapshot.ID]; ok {
				item.Snapshot = snapshot
				filteredItems = append(filteredItems, item)
			}
		}
		indexedSnapshots = filteredItems
		snapshots := make([]models.Snapshot, 0, len(indexedSnapshots))
		for _, item := range indexedSnapshots {
			snapshots = append(snapshots, item.Snapshot)
		}
		if !request.postBackup || !state.HeaderValid {
			rebuilt, rebuildErr := maybeRebuildMetadata(ctx, db, repo, indexedSnapshots, indexSession, manager, closeIndexSession)
			if rebuildErr != nil {
				if errors.Is(rebuildErr, database.ErrMetadataIndexingUnavailable) {
					return rebuildErr
				}
				// The unpublished replacement cannot retain per-snapshot failures.
				// Cleanup has finished; record the failure on the original cache so
				// ordinary access respects the existing repository retry cooldown.
				return &repositoryRefreshError{err: rebuildErr}
			}
			if rebuilt {
				return nil
			}
			if request.bucketCandidateOnly {
				// A no-resize decision still owns a native session. Its close
				// failure must use the same status/cooldown as a header refresh.
				if err := closeIndexSession(); err != nil {
					return &repositoryRefreshError{err: err}
				}
				return nil
			}
		}
		headerPublicationDiagnostic.Snapshots = len(snapshots)
		if err := ctx.Err(); err != nil {
			return err
		}
		setCoordinatorStage(db, repo, "publishing_entries")
		if err := ctx.Err(); err != nil {
			return err
		}
		observedGeneration, contentIndexRequired, err = reconcileMetadataHeadersComplete(
			ctx, db, repo.ID, snapshots, metadataNow().UTC(), observedGeneration,
		)
		setCoordinatorStage(db, repo, "indexing_entries")
		if err != nil {
			return &repositoryRefreshError{err: fmt.Errorf("publish complete metadata headers: %w", err)}
		}
		needed, err = database.MetadataSnapshotsNeedingIndex(db, repo.ID, metadataNow(), request.bypassEntryCooldown)
		if err != nil {
			headerPublicationPhase.finish(headerPublicationDiagnostic)
			return err
		}
		if !contentIndexRequired && len(needed) == 0 {
			// Complete unchanged native headers, ordered roots, and already-ready
			// attached entry sets need no content listing or high-cardinality write.
			// No content transaction is needed, but publication still waits for the
			// reusable native session to close successfully.
			headerPublicationDiagnostic.PublicationSkipped = true
			uiDiagnostic.Snapshots = len(snapshots)
			uiDiagnostic.PublicationSkipped = true
			headerPublicationPhase.finish(headerPublicationDiagnostic)
			if err := closeIndexSession(); err != nil {
				return &repositoryRefreshError{err: err}
			}
			cleared, completeErr := completeMetadataRefresh(ctx, db, repo.ID, observedGeneration)
			if completeErr != nil {
				return &repositoryRefreshError{err: fmt.Errorf("publish complete metadata cache: %w", completeErr)}
			}
			if !cleared {
				return nil
			}
			if request.postBackup {
				return evaluateBackupMetadataBuckets(ctx, db, repo, request, indexedSnapshots)
			}
			return nil
		}
		headerPublicationPhase.finish(headerPublicationDiagnostic)
		uiDiagnostic.Snapshots = len(snapshots)
		uiPhase.finish(uiDiagnostic)
		uiPhase = beginIndexPhase("indexing_snapshot_contents")
		uiDiagnostic = IndexPhaseDiagnostic{Snapshots: len(needed)}
	} else {
		for _, snapshot := range needed {
			indexedSnapshots = append(indexedSnapshots, engines.SnapshotIndexItem{Snapshot: snapshot})
		}
	}
	indexedByID := make(map[string]engines.SnapshotIndexItem, len(indexedSnapshots))
	for _, snapshot := range indexedSnapshots {
		indexedByID[snapshot.Snapshot.ID] = snapshot
	}
	orderedSnapshots := make([]engines.SnapshotIndexItem, 0, len(needed))
	for _, snapshot := range needed {
		if indexed, ok := indexedByID[snapshot.ID]; ok {
			orderedSnapshots = append(orderedSnapshots, indexed)
		} else {
			orderedSnapshots = append(orderedSnapshots, engines.SnapshotIndexItem{Snapshot: snapshot})
		}
	}
	var firstErr error
	var persistenceErr error
	setCoordinatorStage(db, repo, "indexing_entries")
	entryPhase := beginIndexPhase("snapshot_entry_index")
	entryDiagnostic := IndexPhaseDiagnostic{Snapshots: len(orderedSnapshots)}
	defer func() { entryPhase.finish(entryDiagnostic) }()
	readerLimit := 1
	if request.intent == intentHeaders && indexSession != nil {
		readerLimit = snapshotIndexReadersPerRepository
	}
	for start := 0; start < len(orderedSnapshots); start += readerLimit {
		setCoordinatorStage(db, repo, "indexing_entries")
		end := min(start+readerLimit, len(orderedSnapshots))
		window := orderedSnapshots[start:end]
		representatives := make([]engines.SnapshotIndexItem, 0, len(window))
		representativeIDs := make(map[string]bool, len(window))
		for _, item := range window {
			key, keyErr := snapshotEntrySetKey(repo, item)
			if keyErr != nil {
				return keyErr
			}
			if item.NativeContentIdentity != "" {
				identity := snapshotEntrySetIdentity(repo, item)
				exists, existsErr := metadataEntrySetExists(db, repo.ID, item.Snapshot, identity)
				if existsErr != nil {
					return existsErr
				}
				if exists {
					continue
				}
			}
			if !representativeIDs[key] {
				representativeIDs[key] = true
				representatives = append(representatives, item)
			}
		}
		results := listSnapshotIndexWindow(ctx, manager, indexSession, repo, representatives)
		entryDiagnostic.ListedSnapshots += len(representatives)
		resultsByID := make(map[string]snapshotIndexResult, len(results))
		for _, result := range results {
			resultsByID[result.snapshot.Snapshot.ID] = result
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		// Entry-set insertion materializes the direct files catalog in one selected-
		// vault transaction. Pass its context through to SQLite so same-vault
		// backup/restore work can cancel it, roll it back atomically, and let the
		// coalesced refresh resume without coupling another vault's cache writer.
		setCoordinatorStage(db, repo, "publishing_entries")
		if err := ctx.Err(); err != nil {
			return err
		}
		for _, item := range window {
			if item.NativeContentIdentity != "" {
				reused, reuseErr := reuseMetadataSnapshotEntrySet(ctx, db, repo.ID, item.Snapshot, snapshotEntrySetIdentity(repo, item))
				if reuseErr != nil {
					if persistenceErr == nil {
						persistenceErr = reuseErr
					}
					continue
				}
				if reused {
					entryDiagnostic.ReusedEntrySets++
					continue
				}
			}
			result, listed := resultsByID[item.Snapshot.ID]
			if !listed {
				setCoordinatorStage(db, repo, "indexing_entries")
				result = listSnapshotIndexWindow(ctx, manager, indexSession, repo, []engines.SnapshotIndexItem{item})[0]
				if err := ctx.Err(); err != nil {
					return err
				}
				setCoordinatorStage(db, repo, "publishing_entries")
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			snapshot := result.snapshot.Snapshot
			if result.err != nil {
				if markErr := markMetadataSnapshotFailed(db, repo.ID, snapshot, result.err); markErr != nil && persistenceErr == nil {
					persistenceErr = markErr
				}
				if firstErr == nil {
					firstErr = &engineError{output: result.output, err: result.err}
				}
				continue
			}
			entryDiagnostic.Entries += len(result.entries)
			var replaceErr error
			if item.NativeContentIdentity == "" {
				replaceErr = replaceMetadataSnapshot(ctx, db, repo.ID, snapshot, result.entries)
			} else {
				replaceErr = replaceMetadataSnapshotWithIdentity(ctx, db, repo.ID, snapshot, result.entries, snapshotEntrySetIdentity(repo, item))
			}
			if replaceErr != nil && persistenceErr == nil {
				persistenceErr = replaceErr
			}
		}
	}
	entryPhase.finish(entryDiagnostic)
	uiDiagnostic.Snapshots = entryDiagnostic.Snapshots
	uiDiagnostic.Entries = entryDiagnostic.Entries
	uiDiagnostic.ListedSnapshots = entryDiagnostic.ListedSnapshots
	uiDiagnostic.ReusedEntrySets = entryDiagnostic.ReusedEntrySets
	if persistenceErr != nil {
		return persistenceErr
	}
	if err := closeIndexSession(); err != nil {
		return errors.Join(err,
			database.MarkMetadataRepositoryFailed(db, repo.ID, err.Error(), metadataNow().UTC()))
	}
	if firstErr != nil {
		return firstErr
	}
	if completeHeaderRefresh {
		cleared, err := completeMetadataRefresh(ctx, db, repo.ID, observedGeneration)
		if err != nil {
			return &repositoryRefreshError{err: fmt.Errorf("publish complete metadata cache: %w", err)}
		}
		if !cleared {
			// A newer local mutation won the generation race. Only the reconciliation
			// for that reservation may make the direct union catalog available.
			return nil
		}
		if request.postBackup {
			return evaluateBackupMetadataBuckets(ctx, db, repo, request, indexedSnapshots)
		}
		return nil
	}
	return nil
}

func snapshotEntrySetIdentity(repo models.Repository, snapshot engines.SnapshotIndexItem) database.MetadataEntrySetIdentity {
	return database.MetadataEntrySetIdentity{Engine: repo.Engine, NativeIdentity: snapshot.NativeContentIdentity}
}

func snapshotEntrySetKey(repo models.Repository, snapshot engines.SnapshotIndexItem) (string, error) {
	if snapshot.NativeContentIdentity == "" {
		return "snapshot\x00" + snapshot.Snapshot.ID, nil
	}
	scope, err := database.MetadataEntrySetRootScope(snapshot.Snapshot)
	if err != nil {
		return "", err
	}
	// Native content identity alone is insufficient for Kopia: one immutable
	// root object can be presented under different source roots. Preserve exact
	// ordered normalized roots so listing windows follow the database's set identity.
	return repo.Engine + "\x00" + snapshot.NativeContentIdentity + "\x00" + scope, nil
}

type snapshotIndexResult struct {
	snapshot engines.SnapshotIndexItem
	entries  []models.SnapshotEntry
	output   string
	err      error
}

// listSnapshotIndexWindow overlaps only independent native read-only listings.
// Keep one exact-snapshot command per item: Restic's multi-snapshot find path
// was substantially slower and retained several times more parser memory than
// four concurrent ls calls. Kopia already has the same one-listing-per-object
// shape. Results stay in their input slots so publication remains newest-first.
func listSnapshotIndexWindow(
	ctx context.Context,
	manager engines.Engine,
	session engines.SnapshotIndexSession,
	repo models.Repository,
	snapshots []engines.SnapshotIndexItem,
) []snapshotIndexResult {
	results := make([]snapshotIndexResult, len(snapshots))
	if len(snapshots) == 0 {
		return results
	}
	list := func(snapshot engines.SnapshotIndexItem) snapshotIndexResult {
		result := snapshotIndexResult{snapshot: snapshot}
		if session != nil && snapshot.NativeReference != nil {
			result.entries, result.output, result.err = session.ListPathRecursive(ctx, snapshot, "")
		} else {
			result.entries, result.output, result.err = manager.ListPathRecursive(ctx, repo, snapshot.Snapshot.ID, "")
		}
		return result
	}
	if len(snapshots) == 1 {
		results[0] = list(snapshots[0])
		return results
	}
	var readers sync.WaitGroup
	for index, snapshot := range snapshots {
		index, snapshot := index, snapshot
		readers.Add(1)
		go func() {
			defer readers.Done()
			results[index] = list(snapshot)
		}()
	}
	readers.Wait()
	return results
}

// IndexSnapshot refreshes one snapshot. It is useful for API fallback paths,
// but normal backup completion uses SyncRepository to discover new snapshots.
func IndexSnapshot(db *sql.DB, repo models.Repository, snapshot models.Snapshot) error {
	unlockVault, lockErr := vaultlock.AcquireExclusiveContext(context.Background(), repositoryLockKey(repo))
	if lockErr != nil {
		return lockErr
	}
	defer unlockVault()
	key := repositoryLockKey(repo)
	unlock := lockRepository(key)
	defer unlock()
	manager, err := engines.ResolveWithRepositoryAvailabilityCheck(
		repo, storageavailability.RequireRepositoryAvailable,
	)
	if err != nil {
		return err
	}
	return indexSnapshot(db, repo, manager, snapshot)
}

func indexSnapshot(db *sql.DB, repo models.Repository, manager engines.Engine, snapshot models.Snapshot) error {
	entries, output, err := manager.ListPathRecursive(context.Background(), repo, snapshot.ID, "")
	if err != nil {
		_ = database.MarkMetadataSnapshotFailed(db, repo.ID, snapshot, err)
		return &engineError{output: output, err: err}
	}
	return database.ReplaceMetadataSnapshot(db, repo.ID, snapshot, entries)
}

type engineError struct {
	output string
	err    error
}

func (e *engineError) Error() string {
	if e.output == "" {
		return e.err.Error()
	}
	return e.output + ": " + e.err.Error()
}

func (e *engineError) Unwrap() error { return e.err }

type repositoryRefreshError struct{ err error }

func (e *repositoryRefreshError) Error() string { return e.err.Error() }
func (e *repositoryRefreshError) Unwrap() error { return e.err }

// The automatic first check is deliberately local and follows successful new
// header/content publication. A candidate re-enters a complete fresh header
// pass under this same admitted low-priority operation before choosing a count.
func evaluateBackupMetadataBuckets(ctx context.Context, db *sql.DB, repo models.Repository, request syncRequest, items []engines.SnapshotIndexItem) error {
	current, err := database.MetadataBucketCount(ctx, db, repo.ID)
	if err != nil {
		return err
	}
	selected := []models.Snapshot{}
	ids := map[string]bool{}
	for _, id := range request.backupSnapshotIDs {
		ids[id] = true
	}
	for _, item := range items {
		if request.retainedSizing || ids[item.Snapshot.ID] {
			selected = append(selected, item.Snapshot)
		}
	}
	required := database.RequiredMetadataBucketCount(selected, current)
	if !database.MetadataBucketResizeRequired(current, required) {
		return nil
	}
	// The caller retains the same local serialization and low-priority vault
	// admission throughout this follow-up. The nested pass uses a distinct
	// request and never recursively schedules work.
	next := syncRequest{repository: repo, intent: intentHeaders, bypassEntryCooldown: true, bucketCandidateOnly: true}
	return syncRepositoryLocked(context.WithValue(ctx, syncIntentContextKey{}, next), db, repo)
}

func maybeRebuildMetadata(ctx context.Context, db *sql.DB, repo models.Repository, items []engines.SnapshotIndexItem, session engines.SnapshotIndexSession, manager engines.Engine, closeSession func() error) (bool, error) {
	current, err := database.MetadataBucketCount(ctx, db, repo.ID)
	if err != nil {
		return false, err
	}
	snapshots := make([]models.Snapshot, 0, len(items))
	rebuild := make([]database.MetadataRebuildSnapshot, 0, len(items))
	byID := map[string]engines.SnapshotIndexItem{}
	for _, item := range items {
		snapshots = append(snapshots, item.Snapshot)
		rebuild = append(rebuild, database.MetadataRebuildSnapshot{Snapshot: item.Snapshot, Identity: snapshotEntrySetIdentity(repo, item)})
		byID[item.Snapshot.ID] = item
	}
	required := database.RequiredMetadataBucketCount(snapshots, current)
	lastReconciled, err := database.MetadataLastReconciled(db, repo.ID)
	if err != nil {
		return false, err
	}
	if lastReconciled == "" && required != current {
		initialized, err := database.InitializeMetadataBucketCount(ctx, db, repo.ID, required)
		if err != nil || initialized {
			// Empty caches use the ordinary bounded listing windows below. A
			// partially indexed cache must still regroup its existing membership.
			return false, err
		}
	}
	if !database.MetadataBucketResizeRequired(current, required) {
		return false, nil
	}
	setCoordinatorStage(db, repo, "rebuilding_entries")
	err = database.RebuildMetadataCache(ctx, db, repo.ID, required, rebuild, func(ctx context.Context, snapshot models.Snapshot) ([]models.SnapshotEntry, error) {
		result := listSnapshotIndexWindow(ctx, manager, session, repo, []engines.SnapshotIndexItem{byID[snapshot.ID]})[0]
		if result.err != nil {
			return nil, &engineError{output: result.output, err: result.err}
		}
		return result.entries, nil
	}, closeSession)
	return true, err
}

// Conclusive manual deletion has already applied its exact local delta. Only a
// retained-maximum threshold candidate needs a fresh native inventory afterward.
func ScheduleRepositoryBucketEvaluation(db *sql.DB, repo models.Repository) bool {
	snapshots, err := database.ListMetadataSnapshots(db, repo.ID)
	if err != nil {
		return false
	}
	current, err := database.MetadataBucketCount(context.Background(), db, repo.ID)
	if err != nil {
		return false
	}
	if !database.MetadataBucketResizeRequired(current, database.RequiredMetadataBucketCount(snapshots, current)) {
		return false
	}
	return scheduleRepositorySync(db, syncRequest{repository: repo, intent: intentHeaders, bypassEntryCooldown: true, bucketCandidateOnly: true}, true)
}
