package kopiapolicy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
)

var (
	managersMu sync.Mutex
	managers   = map[*sql.DB]*Manager{}
)

// Manager owns the complete background lifecycle for one database's
// Kopia-only policy workers.
type Manager struct {
	db       *sql.DB
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	accept   bool
	queued   map[string]bool
	wg       sync.WaitGroup
	stopOnce sync.Once
	stopped  chan struct{}
}

var backgroundReconcile = ReconcileRepository
var admitRepository = repositoryadmission.AdmitUnderLock
var finishKopiaPolicyApplication = database.FinishKopiaPolicyApplication
var readRootCareAuthority = func(
	ctx context.Context,
	store vaultprofile.Store,
	integritySchedule, maintenanceSchedule string,
	objectLock models.ObjectLockSettings,
) (vaultprofile.RootCareAuthority, error) {
	return store.AssertRootCare(ctx, integritySchedule, maintenanceSchedule, objectLock)
}

func SetRepositoryAdmissionForTests(next func(context.Context, *sql.DB, models.Repository) (models.Repository, error)) func() {
	previous := admitRepository
	admitRepository = next
	return func() { admitRepository = previous }
}

func SetRootCareAuthorityForTests(next func(context.Context, vaultprofile.Store, string, string, models.ObjectLockSettings) (vaultprofile.RootCareAuthority, error)) func() {
	previous := readRootCareAuthority
	readRootCareAuthority = next
	return func() { readRootCareAuthority = previous }
}

func assertRootOwnerCare(
	ctx context.Context,
	store vaultprofile.Store,
	repo models.Repository,
	integritySchedule, maintenanceSchedule string,
	objectLock models.ObjectLockSettings,
) error {
	authority, err := readRootCareAuthority(ctx, store, integritySchedule, maintenanceSchedule, objectLock)
	if err != nil {
		return err
	}
	return authority.AssertRepositoryOwner(repo)
}

func newManager(parent context.Context, db *sql.DB) *Manager {
	ctx, cancel := context.WithCancel(parent)
	return &Manager{
		db: db, ctx: ctx, cancel: cancel, accept: true,
		queued: map[string]bool{}, stopped: make(chan struct{}),
	}
}

func managerFor(db *sql.DB) *Manager {
	managersMu.Lock()
	defer managersMu.Unlock()
	if manager := managers[db]; manager != nil {
		return manager
	}
	// Direct callers in tests can queue without a production application
	// lifecycle. Production installs its app-owned context with StartContext
	// before any queue admission.
	manager := newManager(context.Background(), db)
	managers[db] = manager
	return manager
}

// StartContext binds the per-database manager to application shutdown. The
// returned stop function cancels and drains every admitted worker.
func StartContext(parent context.Context, db *sql.DB) func(context.Context) error {
	managersMu.Lock()
	manager := managers[db]
	if manager == nil {
		manager = newManager(parent, db)
		managers[db] = manager
	}
	managersMu.Unlock()
	return manager.Stop
}

func (manager *Manager) Stop(ctx context.Context) error {
	if manager == nil {
		return nil
	}
	manager.stopOnce.Do(func() {
		manager.mu.Lock()
		manager.accept = false
		manager.mu.Unlock()
		manager.cancel()
		go func() {
			manager.wg.Wait()
			close(manager.stopped)
		}()
	})
	select {
	case <-manager.stopped:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// ReconcileRepository converges one attached Kopia vault. It deliberately
// owns no policy abstraction for any other engine.
func ReconcileRepository(ctx context.Context, db *sql.DB, repo models.Repository) (string, error) {
	if repo.Engine != engines.KopiaID {
		return "", nil
	}
	unlock, err := vaultlock.AcquireExclusiveContext(ctx, repo.ID)
	if err != nil {
		return "", fmt.Errorf("wait for Kopia policy reconciliation lock: %w", err)
	}
	defer unlock()
	admitted, err := admitRepository(ctx, db, repo)
	if err != nil {
		return "", fmt.Errorf("admit persisted Kopia vault for policy reconciliation: %w", err)
	}
	return ReconcileRepositoryUnderLock(ctx, db, admitted)
}

// ReconcileRepositoryUnderLock is for callers such as repository maintenance
// that already hold the managed vault UUID lock.
func ReconcileRepositoryUnderLock(ctx context.Context, db *sql.DB, repo models.Repository) (string, error) {
	return reconcileRepositoryUnderLock(ctx, db, repo, false)
}

// ReassureRepositoryUnderLock is used only by scheduled maintenance. A
// pre-mutation read failure with no observed drift may retain the previously
// verified ready state; the maintenance operation still records the failure.
func ReassureRepositoryUnderLock(ctx context.Context, db *sql.DB, repo models.Repository) (string, error) {
	return reconcileRepositoryUnderLock(ctx, db, repo, true)
}

func reconcileRepositoryUnderLock(
	ctx context.Context,
	db *sql.DB,
	repo models.Repository,
	maintenanceReassurance bool,
) (string, error) {
	if repo.Engine != engines.KopiaID {
		return "", nil
	}
	desired, err := database.DeriveKopiaPolicyDesired(db, repo.ID)
	if err != nil {
		return "", err
	}
	digest, err := engines.KopiaManagedPolicyDigest(desired)
	if err != nil {
		return "", err
	}
	state, err := database.GetKopiaPolicyState(db, repo.ID)
	if errors.Is(err, sql.ErrNoRows) {
		tx, beginErr := db.Begin()
		if beginErr != nil {
			return "", beginErr
		}
		if beginErr = database.InitializeKopiaPolicyStateTx(tx, repo.ID, digest, false); beginErr == nil {
			beginErr = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if beginErr != nil {
			return "", beginErr
		}
		state, err = database.GetKopiaPolicyState(db, repo.ID)
	}
	if err != nil {
		return "", err
	}
	if state.DesiredDigest != digest {
		return "", fmt.Errorf("Kopia desired policy digest is stale")
	}
	wasVerifiedReady := state.State == "ready" && state.AppliedDigest == digest
	if state.ActiveFencePath != "" || state.State == "applying" {
		if state.ActiveFencePath == "" || state.ProofKind == "" {
			return "", fmt.Errorf("prior Kopia policy process has no durable fence proof")
		}
		inactive, proofErr := command.NativeProcessFenceInactive(state.ActiveFencePath)
		if proofErr != nil || !inactive {
			return "", fmt.Errorf("prior Kopia policy process is not proven inactive")
		}
		if err := database.ResetInterruptedKopiaPolicyApplication(
			db, repo.ID, digest, state.ActiveFencePath,
		); err != nil {
			return "", err
		}
		if cleanupErr := cleanupFencePath(db, state.ActiveFencePath); cleanupErr != nil {
			recordErr := database.MarkKopiaPolicyFenceCleanupError(
				db, repo.ID, digest, state.ActiveFencePath, state.ProofKind, cleanupErr,
			)
			return "", errors.Join(
				fmt.Errorf("clean recovered Kopia policy process fence: %w", cleanupErr),
				recordErr,
			)
		}
	}
	operationID := uuid.NewString()
	fencePath, err := database.NativeOperationFencePath(db, operationID)
	if err != nil {
		return "", err
	}
	fence, err := command.AcquireNativeProcessFence(fencePath)
	if err != nil {
		return "", err
	}
	if !fence.Supported() {
		_ = fence.Close()
		return "", fmt.Errorf("native process fencing is unsupported")
	}
	if err := database.BeginKopiaPolicyApplication(db, repo.ID, digest, fencePath, fence.ProofKind()); err != nil {
		_ = fence.Close()
		return "", err
	}
	proofKind := fence.ProofKind()
	finishWithoutNative := func(cause error) (string, error) {
		persistErr := finishKopiaPolicyApplication(
			db, repo.ID, digest, cause, false, false, false,
		)
		closeErr := fence.Close()
		if persistErr == nil && closeErr == nil {
			if cleanupErr := cleanupFencePath(db, fencePath); cleanupErr != nil {
				recordErr := database.MarkKopiaPolicyFenceCleanupError(
					db, repo.ID, digest, fencePath, proofKind, cleanupErr,
				)
				return "", errors.Join(cause,
					fmt.Errorf("clean completed Kopia policy process fence: %w", cleanupErr), recordErr)
			}
		}
		return "", errors.Join(cause, persistErr, closeErr)
	}
	var careStore vaultprofile.Store
	var authority vaultprofile.RootCareAuthority
	if repo.ObjectLock.Enrolled {
		careStore = (vaultprofile.Store{Repository: repo}).
			WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
		authority, err = readRootCareAuthority(ctx, careStore, repo.CheckSchedule, repo.MaintenanceSchedule, repo.ObjectLock)
		if err != nil {
			return finishWithoutNative(fmt.Errorf("verify protected vault care before Kopia reconciliation: %w", err))
		}
		// Object Lock is the only policy state whose native mutation depends on
		// protected vault-wide care. Keep ordinary unenrolled Kopia policy
		// reconciliation independent from recovery-sidecar availability.
		repo.NativeMaintenanceOwnerClientUUID = authority.OwnerClientUUID
	}
	engine, err := engines.ResolveWithRepositoryAvailabilityCheck(repo, storageavailability.RequireRepositoryAvailable)
	if err != nil {
		return finishWithoutNative(err)
	}
	nativeContext := command.WithNativeProcessFence(ctx, fence)
	if repo.ObjectLock.Enrolled {
		nativeContext = engines.ContextWithKopiaObjectLockMutationAdmission(nativeContext, func(admissionContext context.Context) error {
			return assertRootOwnerCare(admissionContext, careStore, repo,
				repo.CheckSchedule, repo.MaintenanceSchedule, repo.ObjectLock)
		})
	}
	result, output, reconcileErr := engines.ReconcileKopiaManagedPolicies(
		nativeContext, engine, repo, desired,
	)
	if reconcileErr == nil && result.DesiredDigest != digest {
		reconcileErr = fmt.Errorf("Kopia policy readback digest did not match the durable desired state")
	}
	careVerificationFailed := false
	if repo.ObjectLock.Enrolled && reconcileErr == nil {
		verifiedAuthority, careErr := readRootCareAuthority(ctx, careStore, repo.CheckSchedule, repo.MaintenanceSchedule, repo.ObjectLock)
		if careErr == nil && (verifiedAuthority.OwnerProfileUUID != authority.OwnerProfileUUID ||
			verifiedAuthority.OwnerClientUUID != authority.OwnerClientUUID ||
			verifiedAuthority.OwnerAttachmentGeneration != authority.OwnerAttachmentGeneration) {
			// A non-owner may reconcile its own job policy while the protected Object
			// Lock settings already match. The closing boundary therefore preserves
			// the exact owner observed on entry instead of requiring this attachment
			// to be that owner. When this attachment was the opening owner, keep the
			// typed takeover classification used by scheduler/UI recovery behavior.
			if authority.AssertRepositoryOwner(repo) == nil {
				careErr = verifiedAuthority.AssertRepositoryOwner(repo)
			}
			if careErr == nil {
				careErr = fmt.Errorf("protected vault owner changed during Kopia reconciliation")
			}
		}
		if careErr != nil {
			careVerificationFailed = true
			reconcileErr = fmt.Errorf("verify protected vault care after Kopia reconciliation: %w", careErr)
		}
	}
	if repo.ObjectLock.Enrolled {
		reconcileErr = engines.ExplainKopiaObjectLockProviderRequirement(repo.Connector, reconcileErr)
	}
	persistErr := finishKopiaPolicyApplication(
		db, repo.ID, digest, reconcileErr,
		maintenanceReassurance && wasVerifiedReady && !careVerificationFailed, result.Mutated, result.DriftObserved,
	)
	closeErr := fence.Close()
	if persistErr == nil && closeErr == nil {
		if cleanupErr := cleanupFencePath(db, fencePath); cleanupErr != nil {
			recordErr := database.MarkKopiaPolicyFenceCleanupError(
				db, repo.ID, digest, fencePath, proofKind, cleanupErr,
			)
			return output, errors.Join(
				fmt.Errorf("clean completed Kopia policy process fence: %w", cleanupErr),
				recordErr,
			)
		}
	}
	return output, errors.Join(reconcileErr, persistErr, closeErr)
}

func cleanupFencePath(db *sql.DB, fencePath string) error {
	referenced, err := database.NativeOperationFenceReferenced(db, fencePath)
	if err != nil {
		return err
	}
	if referenced {
		return fmt.Errorf("native operation fence remains durably referenced")
	}
	return command.CleanupClosedNativeProcessFence(fencePath)
}

func Queue(db *sql.DB, repositoryID string) bool {
	if db == nil || repositoryID == "" {
		return false
	}
	manager := managerFor(db)
	manager.mu.Lock()
	if !manager.accept || manager.ctx.Err() != nil {
		manager.mu.Unlock()
		return false
	}
	if _, exists := manager.queued[repositoryID]; exists {
		manager.queued[repositoryID] = true
		manager.mu.Unlock()
		return true
	}
	manager.queued[repositoryID] = false
	manager.wg.Add(1)
	manager.mu.Unlock()
	go manager.run(repositoryID)
	return true
}

func (manager *Manager) run(repositoryID string) {
	defer manager.wg.Done()
	for {
		manager.reconcile(repositoryID)
		manager.mu.Lock()
		if manager.ctx.Err() != nil || !manager.accept {
			delete(manager.queued, repositoryID)
			manager.mu.Unlock()
			return
		}
		if manager.queued[repositoryID] {
			manager.queued[repositoryID] = false
			manager.mu.Unlock()
			continue
		}
		delete(manager.queued, repositoryID)
		manager.mu.Unlock()
		return
	}
}

func (manager *Manager) reconcile(repositoryID string) {
	for {
		if manager.ctx.Err() != nil {
			return
		}
		repo, err := database.GetRepository(manager.db, repositoryID)
		if err != nil || repo.Engine != engines.KopiaID {
			return
		}
		before, beforeErr := database.GetKopiaPolicyState(manager.db, repositoryID)
		if beforeErr != nil && !errors.Is(beforeErr, sql.ErrNoRows) {
			return
		}
		// Terminal background errors require an explicit trigger to transition
		// the exact row back to dirty. A coalesced duplicate queue signal alone
		// must never retry a permanent failure.
		if beforeErr == nil && before.State == "error" {
			return
		}
		_, reconcileErr := backgroundReconcile(manager.ctx, manager.db, repo)
		if manager.ctx.Err() != nil {
			return
		}
		state, stateErr := database.GetKopiaPolicyState(manager.db, repositoryID)
		if stateErr != nil || state.State == "ready" || state.State == "error" {
			return
		}
		// A relevant job mutation can replace the desired digest while the
		// prior native process is fenced. Once that process returns and its
		// fence closes, consume the latest dirty/applying state here.
		if state.State == "applying" {
			timer := time.NewTimer(250 * time.Millisecond)
			select {
			case <-timer.C:
			case <-manager.ctx.Done():
				if !timer.Stop() {
					<-timer.C
				}
				return
			}
			continue
		}
		if beforeErr == nil && state.State == "dirty" && state.DesiredDigest != before.DesiredDigest {
			continue
		}
		if reconcileErr != nil && state.State == "dirty" {
			_ = database.MarkKopiaPolicyReconciliationError(
				manager.db, repositoryID, state.DesiredDigest, reconcileErr,
			)
		}
		return
	}
}

// QueueDirty avoids any native policy read or process for job edits that did
// not change the vault's derived Kopia policy.
func QueueDirty(db *sql.DB, repositoryID string) bool {
	state, err := database.GetKopiaPolicyState(db, repositoryID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && (state.State == "dirty" || state.State == "applying")) {
		return Queue(db, repositoryID)
	}
	return false
}

func QueueAll(db *sql.DB) error {
	ids, err := database.MarkAllKopiaPoliciesVerificationRequired(db)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if !Queue(db, id) {
			return fmt.Errorf("Kopia policy manager is stopping")
		}
	}
	return nil
}
