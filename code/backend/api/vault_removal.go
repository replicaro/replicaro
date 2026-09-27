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
	"github.com/local/replicaro/profilesync"
	"github.com/local/replicaro/vaultlock"
)

// Vault removal and pending recovery-profile updates
//
// Removing a vault rewrites the portable job definitions of every other vault
// that shared a job with it and marks their recovery profiles for publication
// (database.deleteRepository -> markVaultProfilesDirty). The profile queue
// (profilesync) then publishes each of those profiles under that vault's lock.
// Removing one of those vaults straight afterwards can hit one of two
// temporary conflicts:
//
//   - the queue held the vault lock, so removal reported the vault as busy; or
//   - removal published the profile itself, but another removal committed in
//     between and marked this vault's profile pending again, so the final
//     transactional check returned database.ErrVaultProfilePending.
//
// Removal reports exactly those two cases as vault_profile_update_pending.
// The client says the vault information is being updated and repeats the
// request with awaitProfileUpdate=true. That request wakes the queue, waits up
// to vaultRemovalProfileUpdateWait until no profile update for the vault is
// running or due, and then makes one normal removal attempt that rechecks
// every safeguard: the lock, eligibility, its own profile publication,
// artifact staging, and the transactional pending-profile check. If the vault
// is still blocked by a profile update after that (or the wait runs out), the
// answer is vault_profile_update_still_pending and the user can retry later.
// A busy lock held by anything other than a profile update, and every other
// failure, get their usual responses.
//
// The wait deliberately never skips, cancels, or discards the queued update.
// Those updates carry the other vaults' changed job definitions; dropping one
// would leave a remote recovery profile describing jobs as they were before
// the earlier removal. Only the explicit "Remove anyway" confirmation
// (discardRecoveryProfile) may leave this vault's own profile unpublished, and
// it still waits for a profile update that holds the lock. When the update
// fails rather than finishes, the retry's own publication fails the same way
// and the client offers "Remove anyway" as before. Nothing is kept between
// the two requests: no worker, queue, or record beyond the existing
// vault_profile_sync row.

// vaultRemovalProfileUpdateWait bounds the awaitProfileUpdate wait. A variable
// only so tests can shorten it.
var vaultRemovalProfileUpdateWait = 2 * time.Minute

const (
	vaultProfileUpdatePendingCode      = "vault_profile_update_pending"
	vaultProfileUpdateStillPendingCode = "vault_profile_update_still_pending"
)

var errVaultProfileUpdateHoldsLock = errors.New("vault recovery profile update is in progress")

func handleRepositoryRemoval(db *sql.DB, w http.ResponseWriter, r *http.Request) {
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
	awaitProfileUpdate := false
	switch value := r.URL.Query().Get("awaitProfileUpdate"); value {
	case "":
	case "true":
		awaitProfileUpdate = true
	default:
		badRequest(w, "awaitProfileUpdate must be true when provided")
		return
	}
	if awaitProfileUpdate {
		profilesync.Wake(db)
		settled, err := profilesync.WaitForPendingUpdate(r.Context(), db, id, vaultRemovalProfileUpdateWait)
		if err != nil {
			// Only the request's own context ending is a timeout. Any other
			// error (reading the profile queue state) is a server failure and
			// must not be presented as "try again later".
			status := http.StatusInternalServerError
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				status = http.StatusRequestTimeout
			}
			writeError(w, status, err)
			return
		}
		if !settled {
			writeCodedError(w, http.StatusConflict, vaultProfileUpdateStillPendingCode,
				"vault removal stopped because its recovery profile update is still in progress")
			return
		}
	}
	blocked := removeRepositoryOnce(db, w, r, id, discardRecoveryProfile)
	if blocked == nil {
		return
	}
	code := vaultProfileUpdatePendingCode
	if awaitProfileUpdate {
		code = vaultProfileUpdateStillPendingCode
	}
	writeCodedError(w, http.StatusConflict, code, blocked.Error())
}

// removeRepositoryOnce makes one removal attempt. It writes the response for
// every outcome except removal blocked only by a recovery-profile update; that
// error is returned unwritten so the caller can answer with the right code.
// Its local artifacts and database state are unchanged in that case.
func removeRepositoryOnce(db *sql.DB, w http.ResponseWriter, r *http.Request, id string, discardRecoveryProfile bool) error {
	deletedRepo, repoErr := database.GetRepository(db, id)
	if repoErr == nil {
		unlock, lockOK, lockErr := vaultlock.YieldLowPriorityAndTryExclusiveContext(r.Context(), deletedRepo.ID)
		if lockErr != nil {
			writeError(w, http.StatusRequestTimeout, lockErr)
			return nil
		}
		if !lockOK {
			if profilesync.PublishingUnderLock(deletedRepo.ID) {
				return errVaultProfileUpdateHoldsLock
			}
			writeError(w, http.StatusConflict, errors.New(vaultRemovalBusyMessage))
			return nil
		}
		defer unlock()
	} else if !errors.Is(repoErr, sql.ErrNoRows) {
		writeError(w, http.StatusInternalServerError, repoErr)
		return nil
	}

	if err := database.CheckRepositoryDeletionEligibility(db, id); err != nil {
		if errors.Is(err, database.ErrJobRunActive) || errors.Is(err, database.ErrRepositoryConnectionReserved) {
			writeError(w, http.StatusConflict, err)
		} else if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
		} else {
			writeError(w, http.StatusInternalServerError, err)
		}
		return nil
	}
	if !discardRecoveryProfile {
		if err := profilesync.SyncRepositoryUnderLock(r.Context(), db, id); err != nil {
			writeCodedError(w, http.StatusConflict, "vault_profile_sync_required",
				fmt.Sprintf("vault removal stopped because its recovery profile could not be synchronized: %v", err))
			return nil
		}
	}
	stage, err := stageRepositoryArtifacts(deletedRepo, engines.RepositoryArtifactDelete)
	if err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("local engine credential staging failed: %w", err))
		return nil
	}
	deleteLocalState := deleteRepository
	if discardRecoveryProfile {
		deleteLocalState = deleteRepositoryDiscardingPendingProfile
	}
	deleteErr := deleteLocalState(db, id)
	var cacheCleanupErr *database.MetadataCacheCleanupError
	if deleteErr != nil && !errors.As(deleteErr, &cacheCleanupErr) {
		if restoreErr := restoreRepositoryArtifacts(stage); restoreErr != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("vault deletion failed and local engine artifacts could not be restored: %v; restore error: %w", deleteErr, restoreErr))
			return nil
		}
		if errors.Is(deleteErr, database.ErrVaultProfilePending) {
			// A profile-relevant change committed after this attempt's own
			// publication. The newer profile must be published first.
			return deleteErr
		}
		if errors.Is(deleteErr, database.ErrJobRunActive) || errors.Is(deleteErr, database.ErrRepositoryConnectionReserved) {
			writeError(w, http.StatusConflict, deleteErr)
		} else if errors.Is(deleteErr, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, deleteErr)
		} else {
			writeError(w, http.StatusInternalServerError, deleteErr)
		}
		return nil
	}
	profilesync.Wake(db)
	warnings := []string{}
	if discardRecoveryProfile {
		warnings = append(warnings, fmt.Sprintf("Vault %q removed from Replicaro without updating its recovery profile.", deletedRepo.Name))
	}
	if cacheCleanupErr != nil {
		warnings = append(warnings, "The vault was removed, but its local metadata cache requires manual cleanup.")
	}
	var cleanupErrors []error
	if err := removeDeletedRcloneConfig(id); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("vault was deleted but its local rclone config requires cleanup: %w", err))
	}
	if err := finalizeRepositoryArtifacts(stage); err != nil {
		cleanupErrors = append(cleanupErrors, fmt.Errorf("vault was deleted but quarantined local engine artifacts require manual cleanup: %w", err))
	}
	if len(cleanupErrors) != 0 {
		allErrors := make([]error, 0, len(warnings)+len(cleanupErrors))
		for _, warning := range warnings {
			allErrors = append(allErrors, errors.New(warning))
		}
		allErrors = append(allErrors, cleanupErrors...)
		// Database deletion is already committed here. Preserve that truth in
		// the API so a client cannot offer to keep or retry an absent vault.
		writeCodedError(w, http.StatusInternalServerError, "vault_removal_cleanup_required", errors.Join(allErrors...).Error())
		return nil
	}

	if len(warnings) != 0 {
		writeJSON(w, map[string]any{
			"warning": strings.Join(warnings, " "),
		})
		return nil
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
