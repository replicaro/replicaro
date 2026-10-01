package api

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/kopiapolicy"
	"github.com/local/replicaro/metadata"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/operationruntime"
	"github.com/local/replicaro/profilesync"
	"github.com/local/replicaro/scheduler"
)

// refuseWhilePasswordChangeUnfinished is the request-time form of the check
// admission repeats under the lock: a password change stopped before commit
// blocks every operation on the vault until it is retried. Refusing here
// avoids queueing work that could only fail.
func refuseWhilePasswordChangeUnfinished(w http.ResponseWriter, db *sql.DB, repositoryID string) bool {
	err := database.RequireNoPrecommitVaultPasswordChange(db, repositoryID)
	if err == nil {
		return false
	}
	if errors.Is(err, database.ErrVaultPasswordChangeRecoveryRequired) {
		writeError(w, http.StatusConflict, err)
	} else {
		writeError(w, http.StatusInternalServerError, err)
	}
	return true
}

// startManualRepositoryTask runs a manual integrity check or maintenance on
// the tracked path. It does not go through the scheduler's coordinator: the
// API cannot reach it, it refuses a second task per vault, shares a two-task
// limit and applies scheduled-only pause behavior. It runs the same body the
// scheduler runs, with scheduled=false, so every failure is reported and
// notified. The record is queued before the lock wait; a second manual or
// scheduled task of the same kind for the vault is refused while it is queued
// or running.
func startManualRepositoryTask(db *sql.DB, runtimeManager *operationruntime.Manager, w http.ResponseWriter, repo models.Repository, operation string) {
	if err := scheduler.ValidateRepositoryTaskRequest(repo, operation); err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}
	if refuseWhilePasswordChangeUnfinished(w, db, repo.ID) {
		return
	}
	title := database.RepositoryOperationTitle(operation, repo.Name)
	operationID, err := startTrackedOperation(db, runtimeManager, database.QueuedOperation{
		Kind: operation, Title: title, RepositoryID: repo.ID, Exclusive: true,
	}, func(operationID, status string) func() {
		// Used only when the worker fails before the shared body runs; the body
		// sends its own notification.
		return prepareOperationNotification(db, operationID, operation, title, status)
	}, func(op *trackedOperation) {
		unlock, err := op.waitForVault(repo.ID)
		if err != nil {
			op.notStarted(err)
			return
		}
		defer unlock()
		if err := op.activate(); err != nil {
			op.fail(fmt.Errorf("start repository %s: %w", operation, err))
			return
		}
		// The body reloads the vault through admission under the lock, saves the
		// final status (and the vault's last check or maintenance state) and
		// sends the notification. When a step result could not be saved it
		// leaves the record running on purpose for startup reconciliation.
		_, persisted, _ := runQueuedRepositoryTask(op.ctx, db, repo, operation, op.id, runtimeManager)
		op.persisted = persisted
	})
	if errors.Is(err, database.ErrOperationActive) {
		writeError(w, http.StatusConflict, scheduler.ErrRepositoryTaskRunning)
		return
	}
	if err != nil {
		writeTrackedStartError(w, err)
		return
	}
	writeTrackedOperationStarted(w, operationID)
}

// errSnapshotDeletionActive refuses a second deletion of a snapshot while the
// first is still queued or running. The UI hides the snapshot meanwhile, so
// this is only reached from another browser or a stale page. The refusal
// carries the snapshot_deletion_active code: the Restore page tells it apart
// from other 409s and keeps the snapshot hidden instead of showing it again.
var errSnapshotDeletionActive = errors.New("this snapshot is already queued or running for deletion")

func snapshotDeletionTitle(snapshotID string) string { return "Delete snapshot: " + snapshotID }

// startSnapshotDeletion queues one snapshot deletion. The UI closes its dialog
// at once, hides the snapshot while this operation is active, and reports the
// result from the operation. A busy vault is not an error: the deletion waits
// for it rather than creating a failed record and a red issue. It can be
// cancelled while it waits, but not once it has the lock: the native deletion
// is a batch Replicaro cannot stop midway.
func startSnapshotDeletion(db *sql.DB, runtimeManager *operationruntime.Manager, w http.ResponseWriter, repoModel models.Repository, snapshotID string) {
	manager, err := resolveEngine(repoModel)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if _, err := engines.ValidateDeletionCapability(manager); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	if refuseWhilePasswordChangeUnfinished(w, db, repoModel.ID) {
		return
	}
	title := snapshotDeletionTitle(snapshotID)
	operationID, err := startTrackedOperation(db, runtimeManager, database.QueuedOperation{
		Kind: "delete", Title: title, RepositoryID: repoModel.ID, Exclusive: true, MatchTitle: true,
	}, func(operationID, status string) func() {
		return prepareOperationNotification(db, operationID, "delete", title, status)
	}, func(op *trackedOperation) {
		deleteSnapshotInBackground(op, repoModel, snapshotID)
	})
	if errors.Is(err, database.ErrOperationActive) {
		writeCodedError(w, http.StatusConflict, "snapshot_deletion_active", errSnapshotDeletionActive.Error())
		return
	}
	if err != nil {
		writeTrackedStartError(w, err)
		return
	}
	writeTrackedOperationStarted(w, operationID)
}

func deleteSnapshotInBackground(op *trackedOperation, repoModel models.Repository, snapshotID string) {
	db := op.db
	operationID := op.id
	releaseMetadata, err := deferMetadataSyncForRestore(op.ctx, db, repoModel)
	if err != nil {
		op.notStarted(err)
		return
	}
	defer releaseMetadata()
	unlock, err := op.waitForVault(repoModel.ID)
	if err != nil {
		op.notStarted(err)
		return
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	if !op.startWithoutCancel() {
		op.notStarted(context.Canceled)
		return
	}
	if err := op.activate(); err != nil {
		op.fail(fmt.Errorf("start snapshot deletion: %w", err))
		return
	}
	// Everything from here reloads under the lock: the saved vault through
	// admission, attachment authority, and a fresh native listing. Failures are
	// failed operations with an issue and a notification.
	repoModel, err = admitPersistedRepository(op.ctx, db, repoModel)
	if err != nil {
		op.fail(err)
		return
	}
	manager, err := resolveEngine(repoModel)
	if err != nil {
		op.fail(err)
		return
	}
	if err := assertRepositoryWriter(op.ctx, repoModel); err != nil {
		op.fail(fmt.Errorf("snapshot deletion attachment authorization failed: %w", err))
		return
	}
	inventoryContext := command.ContextWithCapturedOutputKind(op.ctx, "snapshot_inventory")
	listed, listOutput, listErr := engines.ListSnapshotsFresh(inventoryContext, manager, repoModel)
	if listErr != nil {
		op.fail(&engineCommandError{output: listOutput, err: listErr})
		return
	}
	var exact *models.Snapshot
	for index := range listed {
		if listed[index].ID != snapshotID {
			continue
		}
		if exact != nil {
			op.fail(fmt.Errorf("snapshot identity is ambiguous in the fresh native listing"))
			return
		}
		exact = &listed[index]
	}
	if exact == nil {
		op.fail(fmt.Errorf("snapshot no longer exists in the fresh native listing"))
		return
	}
	knownJobs, err := database.JobIDsForRepository(db, repoModel.ID)
	if err != nil {
		op.fail(fmt.Errorf("snapshot visibility could not be freshly classified: %w", err))
		return
	}
	presentation := models.ClassifySnapshotPresentation(*exact, repoModel.ProfileUUID, knownJobs)
	if presentation == models.SnapshotPresentationHidden {
		op.fail(fmt.Errorf("snapshot belongs to a different vault profile"))
		return
	}
	if _, err := engines.ValidateDeletionCapability(manager); err != nil {
		op.fail(err)
		return
	}
	metadataGeneration, err := startMetadataMutationStep(db, operationID, repoModel.ID, "snapshot_deletion", time.Now())
	if err != nil {
		op.fail(fmt.Errorf("persist native deletion start before invocation: %w", err))
		return
	}
	deleteContext, processStarted := command.ContextWithProcessStartTracking(op.ctx)
	deleteContext = engines.ContextWithNativeDeletionAdmission(deleteContext, func(ctx context.Context) error {
		if err := assertRepositoryWriter(ctx, repoModel); err != nil {
			return fmt.Errorf("snapshot deletion attachment authorization changed: %w", err)
		}
		return nil
	})
	result, deleteErr := engines.DeleteSnapshots(deleteContext, manager, repoModel, []string{snapshotID})
	operationStatus, stageStarted, preparationErr, nativeStageErr, outputProcessingErr, stageKnown := engines.RequestedOperationOutcome(deleteErr)
	if !stageKnown {
		nativeStageErr = deleteErr
	} else if !stageStarted && preparationErr != nil {
		nativeStageErr = preparationErr
	}
	if processStarted() || stageStarted {
		if dirtyErr := database.MarkVaultSizeDirty(db, repoModel.ID); dirtyErr != nil {
			_ = database.LogWarning(db, "Vault Size cache could not be marked dirty after native snapshot deletion started")
		}
	}
	nativeSucceeded := deleteErr == nil
	if stageKnown {
		nativeSucceeded = operationStatus == engines.RequestedOperationSucceeded
	}
	if result.ResultGranularity == engines.ResultPerID {
		ids, resultErr := engines.SuccessfulNativeDeletionIDs(result)
		nativeSucceeded = resultErr == nil && len(ids) == 1 && ids[0] == snapshotID
		if resultErr != nil {
			deleteErr = errors.Join(deleteErr, resultErr)
		}
	}
	admissionRejected := command.IsPreProcessAdmission(nativeStageErr) && !stageStarted
	nativeStatus := "failed"
	if nativeSucceeded {
		nativeStatus = "succeeded"
	} else if admissionRejected || stageKnown && !stageStarted {
		nativeStatus = "skipped"
	}
	resultJSON, _ := json.Marshal(result)
	nativeStepErr := finishOperationStep(db, operationID, "snapshot_deletion", nativeStatus,
		combinedOperationOutput(string(resultJSON), deleteErr), time.Now())
	var cacheErr, stepPersistenceErr, admissionStepErr error
	var outputFailure *command.OutputProcessingFailure
	if errors.As(outputProcessingErr, &outputFailure) {
		if err := startOperationStep(db, operationID, "orchestration", "output_processing", time.Now()); err != nil {
			stepPersistenceErr = errors.Join(stepPersistenceErr, fmt.Errorf("persist output-processing start: %w", err))
		} else if err := finishOperationStep(
			db, operationID, "output_processing", "failed", "native output could not be processed completely", time.Now(),
		); err != nil {
			stepPersistenceErr = errors.Join(stepPersistenceErr, fmt.Errorf("persist output-processing failure: %w", err))
		}
	}
	if admissionRejected {
		if err := startOperationStep(db, operationID, "orchestration", "native_process_admission", time.Now()); err != nil {
			admissionStepErr = fmt.Errorf("persist native-process admission start: %w", err)
		} else if err := finishOperationStep(db, operationID, "native_process_admission", "failed", nativeStageErr.Error(), time.Now()); err != nil {
			admissionStepErr = fmt.Errorf("persist native-process admission failure: %w", err)
		}
	}
	cacheStepStarted := true
	if err := startOperationStep(db, operationID, "application", "metadata_cache_invalidation", time.Now()); err != nil {
		cacheStepStarted = false
		stepPersistenceErr = errors.Join(stepPersistenceErr, fmt.Errorf("persist metadata-cache application start: %w", err))
	}
	cacheStatus, cacheOutput := "skipped", "native deletion outcome was ambiguous; cache authority remains invalid"
	if nativeSucceeded {
		if err := database.ApplyMetadataDeletedSnapshots(db, repoModel.ID, metadataGeneration, []string{snapshotID}, true); err != nil {
			cacheErr = err
			cacheStatus, cacheOutput = "warning", "conclusive native deletion succeeded but rebuildable cache application failed: "+err.Error()
		} else {
			cacheStatus, cacheOutput = "succeeded", "conclusively deleted snapshot removed from rebuildable cache"
		}
	} else if stageKnown && !stageStarted || admissionRejected {
		if _, err := database.AcknowledgeMetadataGeneration(db, repoModel.ID, metadataGeneration); err != nil {
			cacheErr = err
			cacheStatus, cacheOutput = "warning", "no-start metadata generation acknowledgement failed: "+err.Error()
		} else {
			cacheStatus, cacheOutput = "succeeded", "native deletion did not start; metadata generation acknowledged"
		}
	}
	if cacheStepStarted {
		stepPersistenceErr = errors.Join(stepPersistenceErr, finishOperationStep(db, operationID, "metadata_cache_invalidation", cacheStatus, cacheOutput, time.Now()))
	}
	aggregateErr := errors.Join(deleteErr, nativeStepErr, admissionStepErr, stepPersistenceErr)
	// The snapshot is gone once native deletion succeeded. A later output
	// processing or result persistence failure makes the operation
	// completed_with_issues; a cache update failure (cacheErr) is
	// warning-only and is deliberately not part of aggregateErr.
	status := terminalStatusAfterNative(op.ctx, aggregateErr, nativeSucceeded)
	operationOutput := combinedOperationOutput(result.Output, errors.Join(aggregateErr, cacheErr))
	op.finishWithCause(status, operationOutput, aggregateErr)
	unlock()
	locked = false
	if nativeSucceeded && cacheErr == nil {
		metadata.ScheduleRepositoryBucketEvaluation(db, repoModel)
	}
}

func restoreSnapshotTitle(snapshotID string) string { return "Restore snapshot: " + snapshotID }

// startRestore queues a whole-snapshot or single-path restore. The UI opens the
// dashboard live log for the returned operation. Admission and the fresh
// snapshot visibility check run in the worker under the vault lock. Two
// restores of the same snapshot may be queued at once on purpose: each writes
// only where it was told to.
func startRestore(db *sql.DB, runtimeManager *operationruntime.Manager, w http.ResponseWriter, repoModel models.Repository, req RestoreRequest, restoreOptions engines.RestoreOptions) {
	title := restoreSnapshotTitle(req.SnapshotID)
	operationID, err := startTrackedOperation(db, runtimeManager, database.QueuedOperation{
		Kind: "restore", Title: title, RepositoryID: repoModel.ID,
	}, func(operationID, status string) func() {
		return prepareOperationNotification(db, operationID, "restore", title, status)
	}, func(op *trackedOperation) {
		restoreInBackground(op, repoModel, req, restoreOptions)
	})
	if err != nil {
		writeTrackedStartError(w, err)
		return
	}
	writeTrackedOperationStarted(w, operationID)
}

func restoreInBackground(op *trackedOperation, repoModel models.Repository, req RestoreRequest, restoreOptions engines.RestoreOptions) {
	db := op.db
	// The metadata-sync deferral and the vault lock are held until the final
	// status is saved; the deferred calls run after op.finish.
	releaseMetadataDeferral, err := deferMetadataSyncForRestore(op.ctx, db, repoModel)
	if err != nil {
		op.notStarted(err)
		return
	}
	defer releaseMetadataDeferral()
	unlock, err := op.waitForVault(repoModel.ID)
	if err != nil {
		op.notStarted(err)
		return
	}
	defer unlock()
	if err := op.activate(); err != nil {
		op.fail(fmt.Errorf("start restore: %w", err))
		return
	}
	repoModel, err = admitPersistedRepository(op.ctx, db, repoModel)
	if err != nil {
		op.fail(err)
		return
	}
	engine, err := resolveEngine(repoModel)
	if err != nil {
		op.fail(err)
		return
	}
	var visibleSnapshot models.Snapshot
	var visibilityErr error
	if req.Path == "" {
		visibleSnapshot, visibilityErr = visibleNativeRestoreSnapshot(op.ctx, db, repoModel, engine, req.SnapshotID)
	} else {
		authority, authorityErr := database.LoadMetadataReadAuthority(op.ctx, db, repoModel.ID)
		if authorityErr != nil {
			op.fail(authorityErr)
			return
		}
		var visibleSnapshots map[string]models.Snapshot
		visibleSnapshots, visibilityErr = visibleCachedRestoreSnapshots(op.ctx, db, repoModel, authority, []string{req.SnapshotID})
		visibleSnapshot = visibleSnapshots[req.SnapshotID]
	}
	if visibilityErr != nil {
		op.fail(visibilityErr)
		return
	}
	if req.Path != "" {
		nativeRoot, rootErr := exactRestoreNativeRoot(visibleSnapshot, req.NativeRootID, true)
		if rootErr != nil {
			op.fail(rootErr)
			return
		}
		restoreOptions.NativeRoot = nativeRoot
	} else if engine.ID() == engines.ResticID &&
		visibleSnapshot.Presentation == models.SnapshotPresentationManaged &&
		len(visibleSnapshot.SourceRoots) == 1 &&
		strings.HasPrefix(strings.TrimSpace(visibleSnapshot.SourceRoots[0].Path), `\\`) {
		// Restic archives a Windows UNC volume as a virtual tree component.
		// Narrowing a whole restore to that component is safe only when the
		// fresh native header proves this is the sole root of a managed snapshot;
		// nativeRootId is intentionally ignored for whole-snapshot requests.
		restoreOptions.NativeRoot = visibleSnapshot.SourceRoots[0]
		restoreOptions.ExactSourceRoot = true
	}
	if stepErr := startOperationStep(db, op.id, "native", "restore", time.Now()); stepErr != nil {
		op.finish("failed", "persist native restore start: "+stepErr.Error())
		return
	}
	nativeContext, nativeProcessStarted := command.ContextWithProcessStartTracking(op.ctx)
	nativeContext = command.ContextWithFinalCancellationAdmission(nativeContext)
	destinationNotice := ""
	restoreOptions.ReportDestination = func(notice string) { destinationNotice = notice }
	output, err := engine.Restore(nativeContext, repoModel, req.SnapshotID, restoreOptions)
	op.closeGate()
	// Decide from the engine's own result before later Replicaro errors are
	// joined onto it; a follow-up failure after a completed restore makes it
	// completed_with_issues, not failed.
	nativeCompleted := restoreNativeCompleted(err)
	if stepErr := finishTrackedNativeStep(db, op.id, "restore", output, err, nativeProcessStarted()); stepErr != nil {
		err = errors.Join(err, fmt.Errorf("persist native restore result: %w", stepErr))
	}
	if errors.Is(err, errOperationStepFinalization) {
		// The native step is still running durably. Leave the record running for
		// startup reconciliation rather than finishing only the parent.
		return
	}
	status := terminalStatusAfterNative(op.ctx, err, nativeCompleted)
	if destinationNotice != "" {
		// Keep the native prefix intact so operation-log finalization can
		// append only this wrapper notice without duplicating native output.
		output += "\n" + destinationNotice
	}
	op.finishWithCause(status, combinedOperationOutput(output, err), err)
}

func jobDeletionTitle(name string) string { return "Delete job: " + name }

// jobDeletionIdlePoll is how often a pending job deletion looks again for the
// job's other queued or running work.
var jobDeletionIdlePoll = time.Second

// writeJobBeingDeleted is the refusal for any user-started work or edit of a
// job whose deletion is queued or running. The code lets the UI show its
// translated text.
func writeJobBeingDeleted(w http.ResponseWriter) {
	writeCodedError(w, http.StatusConflict, "job_being_deleted", database.ErrJobDefinitionBusy.Error())
}

// startJobDeletion queues a job deletion. The UI closes its dialog at once,
// hides the job while this operation is active, and reports the result from
// the operation. Profile publication is queued (the deletion marks the vault
// profiles dirty and wakes the profile sync worker), as job create and edit
// already do, instead of being run under the request.
func startJobDeletion(db *sql.DB, runtimeManager *operationruntime.Manager, w http.ResponseWriter, jobID string) {
	job, err := database.GetJob(db, jobID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
		} else {
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	if err := database.ValidateJobDeletionRequest(db, jobID); err != nil {
		switch {
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, http.StatusNotFound, err)
		case errors.Is(err, database.ErrJobConnectionReserved):
			writeError(w, http.StatusConflict, err)
		default:
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	// A job deletion is refused while the removal of any of its target vaults
	// is pending, whatever the engine: a Restic job deletion runs no native
	// command, but it still rewrites the vault's job list and profile, which
	// the removal is publishing. (A removal queued after this check waits for
	// the job deletion instead; see database.RepositoryHasOtherActiveWork.)
	// Only Kopia job deletion runs native commands against the vault (the exact
	// job policy is deleted first), so only those vaults are also refused up
	// front while an unfinished password change blocks them, as the other
	// tracked jobs are.
	for _, target := range job.Targets {
		if err := database.RequireVaultNotBeingRemoved(db, target.RepositoryID); err != nil {
			if !writeVaultWorkRefusal(w, err) {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		repo, repoErr := database.GetRepository(db, target.RepositoryID)
		if repoErr != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("load job deletion vault: %w", repoErr))
			return
		}
		if repo.Engine == engines.KopiaID && refuseWhilePasswordChangeUnfinished(w, db, repo.ID) {
			return
		}
	}
	title := jobDeletionTitle(job.Name)
	operationID, err := startTrackedOperation(db, runtimeManager, database.QueuedOperation{
		Kind: database.JobDeletionKind, Title: title, JobID: jobID, Exclusive: true,
	}, func(operationID, status string) func() {
		return prepareOperationNotification(db, operationID, database.JobDeletionKind, title, status)
	}, func(op *trackedOperation) {
		deleteJobInBackground(op, jobID)
	})
	if errors.Is(err, database.ErrOperationActive) {
		writeJobBeingDeleted(w)
		return
	}
	if err != nil {
		writeTrackedStartError(w, err)
		return
	}
	writeTrackedOperationStarted(w, operationID)
}

// deleteJobInBackground deletes one job. The order matters:
//
//  1. Take the job's definition gate. From here every new admission or edit of
//     the job is refused (scheduled runs are skipped, user requests get "This
//     job is being deleted."), so the deletion cannot be starved.
//  2. Wait, holding no vault lock, until nothing else for the job is queued or
//     running. Taking a vault lock first would deadlock: a queued backup of
//     this job waits for that same lock.
//  3. Take the lock of each Kopia vault, in vault ID order, and check again.
//     A Restic job deletion takes no lock; it runs no native command.
//  4. Delete each exact Kopia job policy, then the job row.
//
// It can be cancelled while it waits in 2 or 3, and not after that. A Kopia
// deletion spanning several vaults holds each vault's lock while it waits for
// the next; without a browser bounding it, that wait can be long.
func deleteJobInBackground(op *trackedOperation, jobID string) {
	db := op.db
	releaseDefinition := database.HoldJobDefinitionForDeletion(db, jobID)
	defer releaseDefinition()
	for {
		active, err := database.JobHasOtherActiveWork(db, jobID)
		if err != nil {
			op.fail(fmt.Errorf("check the job's queued or running work: %w", err))
			return
		}
		if !active {
			break
		}
		timer := time.NewTimer(jobDeletionIdlePoll)
		select {
		case <-op.ctx.Done():
			timer.Stop()
			op.notStarted(op.ctx.Err())
			return
		case <-timer.C:
		}
	}
	deletedJob, err := database.GetJob(db, jobID)
	if err != nil {
		op.fail(fmt.Errorf("load job for deletion: %w", err))
		return
	}
	targetRepositories := make([]models.Repository, 0, len(deletedJob.Targets))
	kopiaRepositories := make([]models.Repository, 0, len(deletedJob.Targets))
	for _, target := range deletedJob.Targets {
		repo, repoErr := database.GetRepository(db, target.RepositoryID)
		if repoErr != nil {
			op.fail(fmt.Errorf("load job deletion vault: %w", repoErr))
			return
		}
		targetRepositories = append(targetRepositories, repo)
		if repo.Engine == engines.KopiaID {
			kopiaRepositories = append(kopiaRepositories, repo)
		}
	}
	sort.Slice(kopiaRepositories, func(i, j int) bool {
		return kopiaRepositories[i].ID < kopiaRepositories[j].ID
	})
	var vaultUnlocks []func()
	releaseVaultLocks := func() {
		for index := len(vaultUnlocks) - 1; index >= 0; index-- {
			vaultUnlocks[index]()
		}
		vaultUnlocks = nil
	}
	// Runs after op.finish below, so the locks are held until the final
	// status is saved.
	defer releaseVaultLocks()
	for _, repo := range kopiaRepositories {
		unlock, lockErr := op.waitForVault(repo.ID)
		if lockErr != nil {
			op.notStarted(lockErr)
			return
		}
		vaultUnlocks = append(vaultUnlocks, unlock)
	}
	if !op.startWithoutCancel() {
		op.notStarted(context.Canceled)
		return
	}
	if err := op.activate(); err != nil {
		op.fail(fmt.Errorf("start job deletion: %w", err))
		return
	}
	// The definition gate has refused new work for the job since step 1, so
	// this should pass; it is the same check DeleteJob repeats in its
	// transaction.
	if err := database.ValidateJobDeletionAdmission(db, jobID); err != nil {
		op.fail(err)
		return
	}
	kopiaRepairRequired := map[string]bool{}
	queueKopiaRepair := func() {
		for _, repo := range targetRepositories {
			if kopiaRepairRequired[repo.ID] {
				kopiapolicy.Queue(db, repo.ID)
			}
		}
	}
	// Preserve the authoritative definition until every exact native job
	// policy has been deleted and read back. Holding all affected vault locks
	// through row deletion prevents local reconciliation from deriving the
	// still-present job after cleanup.
	for _, repo := range kopiaRepositories {
		if dirtyErr := database.MarkKopiaPolicyVerificationRequired(db, repo.ID); dirtyErr != nil {
			queueKopiaRepair()
			op.fail(fmt.Errorf("close Kopia policy readiness before exact job-policy deletion: %w", dirtyErr))
			return
		}
		kopiaRepairRequired[repo.ID] = true
		admitted, admissionErr := admitPersistedRepository(op.ctx, db, repo)
		if admissionErr != nil {
			queueKopiaRepair()
			op.fail(fmt.Errorf("admit Kopia vault for exact job-policy deletion: %w", admissionErr))
			return
		}
		engine, resolveErr := resolveEngine(admitted)
		if resolveErr == nil {
			_, resolveErr = engines.DeleteKopiaManagedJobPolicy(op.ctx, engine, admitted, jobID, deletedJob.Source)
		}
		if resolveErr != nil {
			queueKopiaRepair()
			op.fail(fmt.Errorf("delete exact Kopia job policy: %w", resolveErr))
			return
		}
	}
	if err := deleteBackupJob(db, jobID); err != nil {
		queueKopiaRepair()
		op.fail(err)
		return
	}
	_ = database.LogActivity(db, "Backup job deleted")
	for _, target := range deletedJob.Targets {
		kopiapolicy.QueueDirty(db, target.RepositoryID)
	}
	// DeleteJob marked every affected vault profile dirty in its transaction;
	// the profile sync worker publishes it and retries on its own.
	profilesync.Wake(db)
	op.finish("success", "Backup job deleted")
}
