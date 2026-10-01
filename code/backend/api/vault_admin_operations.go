package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/kopiapolicy"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/operationruntime"
	"github.com/local/replicaro/profilesync"
	"github.com/local/replicaro/runner"
	"github.com/local/replicaro/vaultprofile"
)

// Vault password change (and its retry), vault settings save and vault
// removal run on the tracked operation path (see trackedOperation), the same
// way restore and deletion do. The settings dialog closes at once and the
// vault card shows the operation's progress; the card reads that from the
// active operations, so a reload or another browser shows it too, and closing
// the browser does not stop the work. Each waits for a busy vault instead of
// failing and can be cancelled only while it waits.
//
// Only one of these three may be queued or running per vault (the
// VaultChange flag on the queued record). Because each waits for a busy vault,
// a second one would otherwise queue behind the first and run on whatever the
// first left behind.

func vaultPasswordChangeTitle(vaultName string) string { return "Change vault password: " + vaultName }
func vaultSettingsTitle(vaultName string) string       { return "Save vault settings: " + vaultName }
func vaultRemovalTitle(vaultName string) string        { return "Remove vault: " + vaultName }

// writeVaultWorkRefusal answers the two request-time refusals that are about
// the vault rather than the request: its removal is pending, or another vault
// change is queued or running. The codes let the UI show its translated text.
// It reports whether it wrote a response.
func writeVaultWorkRefusal(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, database.ErrVaultBeingRemoved):
		writeCodedError(w, http.StatusConflict, "vault_being_removed", database.ErrVaultBeingRemoved.Error())
	case errors.Is(err, database.ErrVaultChangeActive):
		writeCodedError(w, http.StatusConflict, "vault_change_active", database.ErrVaultChangeActive.Error())
	default:
		return false
	}
	return true
}

// startVaultPasswordChange queues a password change, or with retry a Retry of
// the vault's unfinished one. The saved phase record stays the recovery
// authority; this operation only reports it. The candidate password lives in
// the worker's memory until the change saves it as the pending credential,
// exactly as the request-bound change did; it never reaches the title, the
// output or the log.
func startVaultPasswordChange(db *sql.DB, runtimeManager *operationruntime.Manager, w http.ResponseWriter, repositoryID, candidate string, retry bool) {
	repo, err := database.GetRepository(db, repositoryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
		} else {
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	if retry {
		_, err = database.VaultPasswordChange(db, repositoryID)
	} else {
		err = runner.ValidateVaultPasswordChangeRequest(db, repositoryID, candidate)
	}
	if err != nil {
		status := http.StatusConflict
		if errors.Is(err, sql.ErrNoRows) {
			status = http.StatusNotFound
		}
		writeError(w, status, err)
		return
	}
	title := vaultPasswordChangeTitle(repo.Name)
	operationID, err := startTrackedOperation(db, runtimeManager, database.QueuedOperation{
		Kind: database.VaultPasswordChangeKind, Title: title, RepositoryID: repo.ID, VaultChange: true,
	}, func(operationID, status string) func() {
		return prepareOperationNotification(db, operationID, database.VaultPasswordChangeKind, title, status)
	}, func(op *trackedOperation) {
		changeVaultPasswordInBackground(op, repo.ID, candidate, retry)
	})
	if err != nil {
		writeTrackedStartError(w, err)
		return
	}
	writeTrackedOperationStarted(w, operationID)
}

func changeVaultPasswordInBackground(op *trackedOperation, repositoryID, candidate string, retry bool) {
	// The runner takes the vault lock through this function. It waits while
	// the record stays queued (and can be cancelled), then closes the cancel:
	// a password change cannot be stopped once it has the lock. The lock is
	// handed back to this worker rather than released by the runner, so it is
	// held until the final status below is saved.
	var releaseVault func()
	defer func() {
		if releaseVault != nil {
			releaseVault()
		}
	}()
	lock := func(_ context.Context, id string) (func(), error) {
		unlock, err := op.waitForVault(id)
		if err != nil {
			return nil, err
		}
		if !op.startWithoutCancel() {
			unlock()
			return nil, context.Canceled
		}
		if err := op.activate(); err != nil {
			unlock()
			return nil, fmt.Errorf("start vault password change: %w", err)
		}
		releaseVault = unlock
		return func() {}, nil
	}
	var result runner.VaultPasswordChangeResult
	var err error
	if retry {
		result, err = runner.RecoverVaultPasswordChange(op.ctx, op.db, repositoryID, lock)
	} else {
		result, err = runner.ChangeVaultPassword(op.ctx, op.db, repositoryID, candidate, lock)
	}
	if releaseVault == nil {
		// Cancelled or stopped while waiting, or refused before the lock (for
		// example a different change became pending meanwhile). Nothing ran.
		op.notStarted(err)
		return
	}
	status := runner.VaultPasswordChangeOutcome(result, err)
	if status == "failed" && op.lifetime.Err() != nil {
		if _, phaseErr := database.VaultPasswordChange(op.db, repositoryID); phaseErr == nil {
			// Shutdown stopped the change after it saved its phase. The record is
			// left running on purpose, with no final status and no notification:
			// startup password recovery resumes the phase and finishes this record
			// with the outcome (and notifies a failure or completed-with-issues
			// result), instead of it being reported interrupted and then completed
			// behind the user's back. The deferred release above still frees the
			// vault lock. Without a phase record nothing was changed, so the stop
			// is an ordinary interruption below.
			return
		}
		status = "interrupted"
	}
	if status == "failed" {
		runner.RecordVaultPasswordChangeBlocking(op.db, op.id, repositoryID)
	}
	if result.Phase == "completed" {
		_ = database.LogActivity(op.db, "Vault password changed")
	}
	op.finish(status, runner.VaultPasswordChangeSummary(result, err))
}

// vaultSettingsPlan is what a settings save does, decided from the saved vault
// and the request. The request decides it once so a save that publishes
// nothing does not wait for the vault lock; the worker decides it again under
// the lock from the vault as it is then.
type vaultSettingsPlan struct {
	autoUnlock  bool
	objectLock  models.ObjectLockSettings
	maintenance string
	careChanged bool
}

func planVaultSettings(db *sql.DB, repo models.Repository, req RepositoryScheduleRequest) (vaultSettingsPlan, error) {
	plan := vaultSettingsPlan{autoUnlock: repo.AutoUnlock, objectLock: repo.ObjectLock, maintenance: req.MaintenanceSchedule}
	if req.AutoUnlock != nil && repo.Engine == engines.ResticID {
		plan.autoUnlock = *req.AutoUnlock
	}
	if req.ProfilePreferencesOnly {
		return plan, nil
	}
	if req.ObjectLock != nil {
		objectLock, err := models.NormalizeObjectLock(repo.Engine, repo.Connector, *req.ObjectLock)
		if err != nil {
			return plan, err
		}
		plan.objectLock = objectLock
	}
	if err := models.ValidateObjectLockTransition(repo.ObjectLock, plan.objectLock); err != nil {
		return plan, err
	}
	if !models.ObjectLockMaintenanceEligible(plan.objectLock, plan.maintenance) {
		// Paused vaults may store Manual, but native protection must never be
		// resumed without a reclamation interval that satisfies the one-day
		// margin. Resolve it before publishing the protected owner intent.
		maintenance, err := models.LongestEligibleObjectLockMaintenance(plan.objectLock)
		if err != nil {
			return plan, err
		}
		plan.maintenance = maintenance
	}
	// An unchanged resubmission publishes nothing. A Kopia policy error is
	// retried by the reconciler's own timer, so saving the same settings again
	// is not a retry path (it no longer needs to be one for empty Object Lock
	// vaults either).
	plan.careChanged = req.CheckSchedule != repo.CheckSchedule ||
		plan.maintenance != repo.MaintenanceSchedule || plan.objectLock != repo.ObjectLock
	return plan, nil
}

func (plan vaultSettingsPlan) publishesRoot() bool { return plan.careChanged }

// startVaultSettingsSave queues a vault settings save. Every save is a
// tracked operation, including a save of this computer's own preferences
// only, so every save reports the same way on the vault card and the
// dashboard. A save that changes the vault's care settings (check and
// maintenance schedules, Object Lock) publishes the protected root first and
// commits locally only after that publication was verified; the worker holds
// the vault lock through both. Kopia policy reconciliation and the recovery
// profile publication that follow are queued work, as before.
func startVaultSettingsSave(db *sql.DB, runtimeManager *operationruntime.Manager, w http.ResponseWriter, req RepositoryScheduleRequest) {
	concurrencyMode, err := models.NormalizeConcurrencyMode(req.ConcurrencyMode)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	req.ConcurrencyMode = concurrencyMode
	if req.RepositoryID == "" || !models.ValidConcurrencyMode(req.ConcurrencyMode) ||
		(!req.ProfilePreferencesOnly && (!database.ValidSchedule(req.CheckSchedule) || !database.ValidSchedule(req.MaintenanceSchedule))) {
		badRequest(w, "repositoryId and valid schedules are required")
		return
	}
	repo, err := database.GetRepository(db, req.RepositoryID)
	if err != nil {
		writeError(w, http.StatusNotFound, err)
		return
	}
	req.ConcurrencyMode, err = models.NormalizeConcurrencyModeForConnector(repo.Connector, req.ConcurrencyMode)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if err := database.ValidateRepositoryMutationAdmission(db, repo.ID); err != nil {
		if errors.Is(err, database.ErrRepositoryConnectionReserved) || errors.Is(err, database.ErrVaultPasswordChangeRecoveryRequired) {
			writeError(w, http.StatusConflict, err)
		} else {
			writeError(w, http.StatusNotFound, err)
		}
		return
	}
	plan, err := planVaultSettings(db, repo, req)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	title := vaultSettingsTitle(repo.Name)
	operationID, err := startTrackedOperation(db, runtimeManager, database.QueuedOperation{
		Kind: database.VaultSettingsKind, Title: title, RepositoryID: repo.ID, VaultChange: true,
	}, func(operationID, status string) func() {
		return prepareOperationNotification(db, operationID, database.VaultSettingsKind, title, status)
	}, func(op *trackedOperation) {
		saveVaultSettingsInBackground(op, repo, req, plan.publishesRoot())
	})
	if err != nil {
		writeTrackedStartError(w, err)
		return
	}
	writeTrackedOperationStarted(w, operationID)
}

func saveVaultSettingsInBackground(op *trackedOperation, repo models.Repository, req RepositoryScheduleRequest, publishesRoot bool) {
	db := op.db
	if publishesRoot {
		unlock, err := op.waitForVault(repo.ID)
		if err != nil {
			op.notStarted(err)
			return
		}
		// Held until the final status is saved (deferred calls run after
		// op.finish below).
		defer unlock()
	}
	if !op.startWithoutCancel() {
		op.notStarted(context.Canceled)
		return
	}
	if err := op.activate(); err != nil {
		op.fail(fmt.Errorf("start vault settings save: %w", err))
		return
	}
	// Decide again from the vault as it is now; with the lock held nothing
	// else changes its care settings until this save is done.
	repo, err := database.GetRepository(db, repo.ID)
	if err != nil {
		op.fail(err)
		return
	}
	plan, err := planVaultSettings(db, repo, req)
	if err != nil {
		op.fail(err)
		return
	}
	if publishesRoot && plan.publishesRoot() {
		err = saveOwnerVaultCareUnderLock(op.ctx, db, repo, req, plan)
	} else {
		// Only this computer's profile-local preferences change. Schedules from
		// an earlier UI read are deliberately not replayed: owner-loss admission
		// may have disabled integrity concurrently, and root care is not
		// local-profile state to overwrite here.
		err = database.UpdateRepositoryLocalPreferences(db, repo.ID, req.ConcurrencyMode, plan.autoUnlock)
	}
	if err != nil {
		op.fail(err)
		return
	}
	message := "Repository schedules updated"
	if req.ProfilePreferencesOnly {
		message = "Repository profile preferences updated"
	}
	_ = database.LogActivity(db, message)
	profilesync.Wake(db)
	if repo.Engine == engines.KopiaID && !req.ProfilePreferencesOnly {
		kopiapolicy.QueueDirty(db, repo.ID)
	}
	op.finish("success", "Vault settings saved.")
}

// saveOwnerVaultCareUnderLock publishes the owner's care settings to the
// protected root and only then commits them locally. The caller holds the
// vault lock.
func saveOwnerVaultCareUnderLock(ctx context.Context, db *sql.DB, repo models.Repository, req RepositoryScheduleRequest, plan vaultSettingsPlan) error {
	var reviewedRoot vaultprofile.Root
	var reviewedRootData []byte
	repo, reviewedRoot, reviewedRootData, err := admitOwnerVaultCare(ctx, db, repo)
	if err != nil {
		return err
	}
	// Close backup admission before changing the protected root. If root
	// publication or the following DB write fails, reconciliation can only
	// restore readiness after a fresh exact root/native readback.
	if repo.Engine == engines.KopiaID && plan.careChanged {
		if err := database.MarkKopiaPolicyVerificationRequired(db, repo.ID); err != nil {
			return err
		}
	}
	if err := publishReviewedOwnerVaultCare(ctx, repo, reviewedRoot, reviewedRootData, req.CheckSchedule, plan.maintenance, plan.objectLock); err != nil {
		if repo.Engine == engines.KopiaID {
			kopiapolicy.QueueDirty(db, repo.ID)
		}
		return err
	}
	err = database.UpdateRepositorySettingsWithObjectLockAndConcurrency(db, repo.ID, req.CheckSchedule, plan.maintenance, req.ConcurrencyMode, plan.autoUnlock, plan.objectLock)
	if err != nil && repo.Engine == engines.KopiaID {
		kopiapolicy.QueueDirty(db, repo.ID)
	}
	return err
}
