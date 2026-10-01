package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/operationruntime"
	"github.com/local/replicaro/profilesync"
)

// Vault removal runs on the tracked operation path (see
// vault_admin_operations.go). The request only checks what needs no lock and
// queues the operation; the worker does the rest, in this order:
//
//  1. Wait, holding no vault lock, until nothing else for the vault is queued
//     or running (its own record excluded). Taking the lock first would
//     deadlock: queued work for the vault is itself waiting for that lock.
//     While the removal is pending, no new work for the vault is admitted
//     (see database.ErrVaultBeingRemoved), so the wait ends.
//  2. Take the vault lock. No second idle check is made under it; see
//     removeVaultInBackground for why none is needed.
//  3. Publish the vault's recovery profile under the lock and verify it, then
//     stage the local engine artifacts and delete the local attachment in one
//     transaction. The publication reports, and that transaction refuses with,
//     database.ErrVaultProfilePending when a profile-relevant change committed
//     in between (typically another vault's removal that rewrote a job shared
//     with this vault). The worker then publishes once more and tries once
//     more, still under the lock. If the profile still cannot be updated, the
//     operation fails and records the vaultRemovalProfileStep step as failed;
//     the vault card reads that step to offer Retry removal and Remove anyway.
//     Nothing else is saved for it.
//
// The queued profile updates of other vaults are never skipped, cancelled or
// discarded: they carry those vaults' changed job definitions. Only the
// explicit Remove anyway (discardRecoveryProfile) leaves this vault's own
// profile unpublished; it still waits for other work, takes the lock and
// keeps every other safeguard.
//
// A removal can be cancelled while it waits in step 1 or 2 and not after.

// vaultRemovalProfileStep is the step a removal records when the recovery
// profile could not be published. It is the stable signal the UI uses to
// offer Remove anyway, so do not rename it.
const vaultRemovalProfileStep = "recovery_profile_publication"

// vaultRemovalIdlePoll is how often a pending removal looks again for the
// vault's other queued or running work. A variable only so tests can shorten
// it.
var vaultRemovalIdlePoll = time.Second

func handleRepositoryRemoval(db *sql.DB, runtimeManager *operationruntime.Manager, w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	if id == "" {
		badRequest(w, "missing repository id")
		return
	}
	discardRecoveryProfile := false
	switch value := r.URL.Query().Get("discardRecoveryProfile"); value {
	case "":
	case "true":
		discardRecoveryProfile = true
	default:
		badRequest(w, "discardRecoveryProfile must be true when provided")
		return
	}
	repo, err := database.GetRepository(db, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
		} else {
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	if err := database.CheckRepositoryRemovalRequest(db, id); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, http.StatusNotFound, err)
		case errors.Is(err, database.ErrRepositoryConnectionReserved), errors.Is(err, database.ErrVaultPasswordChangeRecoveryRequired):
			writeError(w, http.StatusConflict, err)
		default:
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	title := vaultRemovalTitle(repo.Name)
	operationID, err := startTrackedOperation(db, runtimeManager, database.QueuedOperation{
		Kind: database.VaultRemovalKind, Title: title, RepositoryID: repo.ID, VaultChange: true,
	}, func(operationID, status string) func() {
		return prepareOperationNotification(db, operationID, database.VaultRemovalKind, title, status)
	}, func(op *trackedOperation) {
		removeVaultInBackground(op, repo, discardRecoveryProfile)
	})
	if err != nil {
		writeTrackedStartError(w, err)
		return
	}
	writeTrackedOperationStarted(w, operationID)
}

func removeVaultInBackground(op *trackedOperation, repo models.Repository, discardRecoveryProfile bool) {
	db := op.db
	if err := waitForVaultIdle(op, repo.ID); err != nil {
		op.notStarted(err)
		return
	}
	// There is deliberately no second idle check under the lock, and no
	// releasing the lock to go back to waiting. Nothing new can appear: from
	// the moment this removal's record was queued, every place that records
	// work for the vault refuses it in the same transaction
	// (database.ErrVaultBeingRemoved), and job deletions of jobs that target
	// the vault are counted by the wait above. Anything that still got through
	// is caught by deleteRepository, which counts the same work in its own
	// transaction and refuses, so the removal fails rather than deleting a
	// vault that is in use.
	unlock, err := op.waitForVault(repo.ID)
	if err != nil {
		op.notStarted(err)
		return
	}
	// Held until the final status is saved (runs after op.finish).
	defer unlock()
	if !op.startWithoutCancel() {
		op.notStarted(context.Canceled)
		return
	}
	if err := op.activate(); err != nil {
		op.fail(fmt.Errorf("start vault removal: %w", err))
		return
	}
	if err := database.CheckRepositoryDeletionEligibility(db, repo.ID); err != nil {
		op.fail(err)
		return
	}
	deleteLocalState := deleteRepository
	if discardRecoveryProfile {
		deleteLocalState = deleteRepositoryDiscardingPendingProfile
	}
	var stage *engines.RepositoryArtifactStage
	var deleteErr error
	// A profile-relevant change can overtake this removal at two points: while
	// the profile is being prepared (the sync then reports
	// ErrVaultProfilePending and publishes nothing), or after the verified
	// publication and before the delete transaction (which then refuses with
	// ErrVaultProfilePending). Either way the removal publishes again and tries
	// exactly once more, still under the lock; a second overtake fails it with
	// the recovery-profile step. The one retry is shared by both points, so a
	// removal never loops.
	for attempt := 1; ; attempt++ {
		if !discardRecoveryProfile {
			if err := profilesync.SyncRepositoryUnderLock(op.ctx, db, repo.ID); err != nil {
				if errors.Is(err, database.ErrVaultProfilePending) && attempt == 1 {
					continue
				}
				if errors.Is(err, database.ErrVaultProfilePending) {
					op.failRemovalProfile(fmt.Errorf("vault removal stopped because its recovery profile changed again while it was being updated: %w", err))
					return
				}
				op.failRemovalProfile(fmt.Errorf("vault removal stopped because its recovery profile could not be synchronized: %w", err))
				return
			}
		}
		var err error
		stage, err = stageRepositoryArtifacts(repo, engines.RepositoryArtifactDelete)
		if err != nil {
			op.fail(fmt.Errorf("local engine credential staging failed: %w", err))
			return
		}
		deleteErr = deleteLocalState(db, repo.ID)
		var cacheCleanupErr *database.MetadataCacheCleanupError
		if deleteErr == nil || errors.As(deleteErr, &cacheCleanupErr) {
			break
		}
		if restoreErr := restoreRepositoryArtifacts(stage); restoreErr != nil {
			op.fail(fmt.Errorf("vault deletion failed and local engine artifacts could not be restored: %v; restore error: %w", deleteErr, restoreErr))
			return
		}
		if !errors.Is(deleteErr, database.ErrVaultProfilePending) {
			op.fail(deleteErr)
			return
		}
		if attempt == 2 {
			// A profile-relevant change overtook both attempts. The vault is
			// kept; the user can retry later or remove it anyway.
			op.failRemovalProfile(fmt.Errorf("vault removal stopped because its recovery profile changed again while it was being updated: %w", deleteErr))
			return
		}
	}
	// The local attachment is gone from here on; the rest is cleanup and
	// cannot undo it.
	profilesync.Wake(db)
	notes := []string{}
	if discardRecoveryProfile {
		notes = append(notes, fmt.Sprintf("Vault %q removed from Replicaro without updating its recovery profile.", repo.Name))
	} else {
		notes = append(notes, fmt.Sprintf("Vault %q removed from Replicaro.", repo.Name))
	}
	var cleanupErrors []string
	var cacheCleanupErr *database.MetadataCacheCleanupError
	if errors.As(deleteErr, &cacheCleanupErr) {
		cleanupErrors = append(cleanupErrors, "The vault was removed, but its local metadata cache requires manual cleanup.")
	}
	// Any Rclone Remote owns no private config; its user-selected file may
	// occupy this UUID's ordinary private-config path.
	if repo.Connector != engines.RcloneRemoteConnector {
		if err := removeDeletedRcloneConfig(repo.ID); err != nil {
			cleanupErrors = append(cleanupErrors, fmt.Sprintf("vault was deleted but its local rclone config requires cleanup: %v", err))
		}
	}
	if err := finalizeRepositoryArtifacts(stage); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Sprintf("vault was deleted but quarantined local engine artifacts require manual cleanup: %v", err))
	}
	if len(cleanupErrors) != 0 {
		// Removed, with local cleanup left for the user: the removal happened,
		// so this is not a failure, but it is not a clean success either.
		op.finish("completed_with_issues", strings.Join(append(notes, cleanupErrors...), "\n"))
		return
	}
	op.finish("success", strings.Join(notes, "\n"))
}

// waitForVaultIdle waits, holding no vault lock, until nothing other than the
// removal itself is queued or running for the vault, including a job deletion
// of a job that targets the vault (see database.RepositoryHasOtherActiveWork).
// It returns early only on cancel or shutdown.
func waitForVaultIdle(op *trackedOperation, repositoryID string) error {
	for {
		active, err := database.RepositoryHasOtherActiveWork(op.db, repositoryID)
		if err != nil {
			return fmt.Errorf("check the vault's queued or running work: %w", err)
		}
		if !active {
			return nil
		}
		timer := time.NewTimer(vaultRemovalIdlePoll)
		select {
		case <-op.ctx.Done():
			timer.Stop()
			return op.ctx.Err()
		case <-timer.C:
		}
	}
}

// failRemovalProfile ends a removal whose recovery profile could not be
// updated. The failed vaultRemovalProfileStep step is what lets the vault card
// offer Remove anyway for exactly this failure.
func (op *trackedOperation) failRemovalProfile(err error) {
	now := time.Now()
	if stepErr := startOperationStep(op.db, op.id, "orchestration", vaultRemovalProfileStep, now); stepErr == nil {
		_ = finishOperationStep(op.db, op.id, vaultRemovalProfileStep, "failed", err.Error(), now)
	}
	op.fail(err)
}
