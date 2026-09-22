package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/profilesync"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
)

var vaultPasswordQuietPeriod = 30 * time.Second
var recordVaultPasswordNativeResult = database.RecordVaultPasswordNativeResult
var loadVaultPasswordChangeAfterNativeResult = database.VaultPasswordChange
var recoverVaultPasswordChange = RecoverVaultPasswordChange
var vaultPasswordChangeFault = func(string) error { return nil }
var prepareVaultPasswordChange = preparePasswordChange
var resolveVaultPasswordEngine = engines.ResolveWithRepositoryAvailabilityCheck
var runNativeVaultPasswordChange = engines.ChangeRepositoryPassword
var probeVaultPassword = engines.ProbeRepositoryPassword
var publishVaultPasswordTree = func(ctx context.Context, store vaultprofile.Store, inventory vaultprofile.PasswordRotationInventory) error {
	return store.PublishPasswordRotatedTree(ctx, inventory)
}
var cleanupVaultPasswordStage = vaultprofile.CleanupPasswordRotationStage
var loadVaultPasswordStage = vaultprofile.LoadPasswordRotationInventory
var captureVaultPasswordRollbackInventory = func(ctx context.Context, store vaultprofile.Store, operationUUID string) (vaultprofile.PasswordRotationInventory, error) {
	return store.CapturePasswordRotationInventory(ctx, operationUUID, false)
}
var deleteVaultPasswordRollbackPending = func(ctx context.Context, store vaultprofile.Store, inventory vaultprofile.PasswordRotationInventory) error {
	return store.DeletePasswordChangeOperationPending(ctx, inventory)
}
var removeVaultPasswordChangeFence = func(ctx context.Context, repo models.Repository, inventory vaultprofile.PasswordRotationInventory) error {
	store := (vaultprofile.Store{Repository: repo}).WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
	return store.SetPasswordChangeFence(ctx, inventory, false)
}
var assertVaultPasswordChangeAuthority = func(ctx context.Context, store vaultprofile.Store, repo models.Repository,
	operation database.VaultPasswordChangeOperation, allowUnfenced, requireProfileFence bool,
) error {
	return store.AssertPasswordChangeAuthority(ctx, repo.ClientUUID, repo.ProfileUUID, repo.AttachmentGeneration,
		operation.OperationUUID, allowUnfenced, requireProfileFence)
}
var admitPasswordChangeRecoveryUnderLock = repositoryadmission.AdmitPasswordChangeRecoveryUnderLock
var abandonConclusivePreMutationPasswordChange = func(db *sql.DB, repo models.Repository, operation database.VaultPasswordChangeOperation) error {
	if operation.NativeMutationDisposition == string(engines.PasswordMutationRejectedBeforeMutation) {
		return database.AbandonRejectedBeforeMutationVaultPasswordChange(db, repo.ID)
	}
	return database.AbandonConclusiveUnstartedVaultPasswordChange(db, repo.ID)
}

type VaultPasswordChangeResult struct {
	RepositoryID   string                       `json:"repositoryId"`
	OperationUUID  string                       `json:"operationUUID"`
	Phase          string                       `json:"phase"`
	Native         engines.PasswordChangeResult `json:"native"`
	SidecarStatus  string                       `json:"sidecarStatus"`
	CleanupPending bool                         `json:"cleanupPending"`
	Message        string                       `json:"message"`
	ResticKeyTruth string                       `json:"resticKeyTruth,omitempty"`
}

func nativePasswordResultTruth(operation database.VaultPasswordChangeOperation) string {
	if operation.NativeStatus == "" {
		return "unresolved"
	}
	return operation.NativeStatus
}

func verifiedCandidateNativeSummary(operation database.VaultPasswordChangeOperation) string {
	if operation.NativeMutationDisposition == string(engines.PasswordMutationRejectedBeforeMutation) {
		return "Restic rejected the selected-key password change before mutation; the pending credential verified separately"
	}
	return fmt.Sprintf("the pending credential verified while native password-mutation truth remains %s", nativePasswordResultTruth(operation))
}

func passwordChangeResult(repo models.Repository, operation database.VaultPasswordChangeOperation) VaultPasswordChangeResult {
	result := VaultPasswordChangeResult{RepositoryID: repo.ID, OperationUUID: operation.OperationUUID,
		Phase: operation.Phase, SidecarStatus: "pending",
		Native: engines.PasswordChangeResult{Engine: repo.Engine, Status: engines.RequestedOperationStatus(operation.NativeStatus),
			Output: operation.NativeOutput, MutationDisposition: engines.PasswordMutationDisposition(operation.NativeMutationDisposition)}}
	if operation.NativeStatus == "succeeded" || operation.NativeStatus == "failed" || operation.NativeStatus == "interrupted" {
		result.Native.ProcessStarted = true
	}
	nativeSucceeded := operation.NativeStatus == "succeeded"
	nativeTruth := nativePasswordResultTruth(operation)
	if operation.Phase == "cleanup_pending" && nativeSucceeded {
		result.SidecarStatus, result.CleanupPending = "succeeded", true
		result.Message = "The native vault password and protected recovery metadata changed successfully; local residue cleanup is pending."
	} else if operation.Phase == "cleanup_pending" {
		result.SidecarStatus, result.CleanupPending = "succeeded", true
		result.Message = fmt.Sprintf("The pending vault password verified and protected recovery metadata was updated; local residue cleanup is pending. The recorded native result remains %s.", nativeTruth)
	} else if operation.Phase == "publishing" && nativeSucceeded {
		result.Message = "The native vault password changed, but protected recovery metadata publication is incomplete and must recover forward."
	} else if operation.Phase == "publishing" {
		result.Message = fmt.Sprintf("The pending vault password verified, but protected recovery metadata publication is incomplete. The recorded native result remains %s.", nativeTruth)
	} else {
		result.Message = "Vault-password change recovery is in progress."
	}
	if repo.Engine == engines.ResticID {
		switch {
		case operation.NativeMutationDisposition == string(engines.PasswordMutationRejectedBeforeMutation):
			result.ResticKeyTruth = "Restic rejected the selected-key password change because the repository was locked before mutation. The selected key was not changed."
		case operation.NativeStatus == "succeeded":
			result.ResticKeyTruth = "Restic changed the selected key. Other Restic keys were not removed and may still accept an older password."
		default:
			result.ResticKeyTruth = "Whether Restic changed the selected key is unresolved. Other Restic keys may still accept an older password."
		}
	}
	return result
}

func runLocalNativePasswordChangeResult(repo models.Repository, operation database.VaultPasswordChangeOperation, native engines.PasswordChangeResult, disposition string) VaultPasswordChangeResult {
	runLocal := operation
	runLocal.NativeStatus = string(native.Status)
	runLocal.NativeMutationDisposition = disposition
	runLocal.NativeOutput = native.Output
	result := passwordChangeResult(repo, runLocal)
	result.Native = native
	return result
}

func persistRunLocalNativePasswordResult(db *sql.DB, repo models.Repository, operation database.VaultPasswordChangeOperation, native engines.PasswordChangeResult, nativeErr error) (database.VaultPasswordChangeOperation, VaultPasswordChangeResult, error) {
	status := string(native.Status)
	if status != "not_started" && status != "succeeded" && status != "failed" && status != "interrupted" {
		return operation, passwordChangeResult(repo, operation), fmt.Errorf("native vault-password result is unresolved; explicit retry is required")
	}
	safeError := ""
	if nativeErr != nil {
		safeError = nativeErr.Error()
	}
	disposition := string(native.MutationDisposition)
	if disposition == "" {
		disposition = string(engines.PasswordMutationUnknown)
	}
	if err := recordVaultPasswordNativeResult(db, repo.ID, status, disposition, native.Output, safeError); err != nil {
		return operation, runLocalNativePasswordChangeResult(repo, operation, native, disposition), fmt.Errorf("persist native vault-password result: %w", err)
	}
	updated, err := loadVaultPasswordChangeAfterNativeResult(db, repo.ID)
	if err != nil {
		return operation, runLocalNativePasswordChangeResult(repo, operation, native, disposition), err
	}
	return updated, passwordChangeResult(repo, updated), nil
}

// VaultPasswordChangeStatus reports only same-installation durable recovery
// state. It performs no repository access and discloses no remote owner facts.
func VaultPasswordChangeStatus(db *sql.DB, repositoryID string) (VaultPasswordChangeResult, error) {
	repo, err := database.GetRepository(db, repositoryID)
	if err != nil {
		return VaultPasswordChangeResult{}, err
	}
	operation, err := database.VaultPasswordChange(db, repositoryID)
	if err != nil {
		return VaultPasswordChangeResult{}, err
	}
	return passwordChangeResult(repo, operation), nil
}

func ChangeVaultPassword(ctx context.Context, db *sql.DB, repositoryID, candidate string) (VaultPasswordChangeResult, error) {
	if err := models.ValidateVaultPassword(candidate); err != nil {
		return VaultPasswordChangeResult{}, err
	}
	if operation, err := database.VaultPasswordChange(db, repositoryID); err == nil {
		repo, loadErr := database.GetRepository(db, repositoryID)
		if loadErr != nil {
			return VaultPasswordChangeResult{}, loadErr
		}
		if operation.Phase == "cleanup_pending" || repo.PendingPassphrase == "" || candidate != repo.PendingPassphrase {
			return passwordChangeResult(repo, operation), fmt.Errorf("a different vault-password change is already pending; use Retry to recover it")
		}
		return recoverVaultPasswordChange(ctx, db, repositoryID)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return VaultPasswordChangeResult{}, err
	}
	repo, err := database.GetRepository(db, repositoryID)
	if err != nil {
		return VaultPasswordChangeResult{}, err
	}
	if candidate == repo.Passphrase {
		return VaultPasswordChangeResult{}, fmt.Errorf("new vault password must differ from the current password")
	}
	if _, err := database.VaultProfileSyncState(db, repositoryID); err == nil {
		if err := profilesync.SyncRepository(ctx, db, repositoryID); err != nil {
			return VaultPasswordChangeResult{}, fmt.Errorf("resolve pending local recovery-profile publication before password change: %w", err)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		return VaultPasswordChangeResult{}, err
	}
	repo, err = database.GetRepository(db, repositoryID)
	if err != nil {
		return VaultPasswordChangeResult{}, err
	}
	if candidate == repo.Passphrase {
		return VaultPasswordChangeResult{}, fmt.Errorf("new vault password must differ from the current password")
	}
	if err := database.ValidateRepositoryMutationAdmission(db, repo.ID); err != nil {
		return VaultPasswordChangeResult{}, err
	}
	unlock, ok, lockErr := vaultlock.YieldLowPriorityAndTryExclusiveContext(ctx, repo.ID)
	if lockErr != nil {
		return VaultPasswordChangeResult{}, lockErr
	}
	if !ok {
		return VaultPasswordChangeResult{}, fmt.Errorf("vault is busy with another operation")
	}
	defer unlock()
	repo, err = repositoryadmission.AdmitUnderLock(ctx, db, repo)
	if err != nil {
		return VaultPasswordChangeResult{}, err
	}
	store := (vaultprofile.Store{Repository: repo}).WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
	if err := store.AssertRootOwner(ctx, repo.ClientUUID, repo.ProfileUUID, repo.AttachmentGeneration); err != nil {
		return VaultPasswordChangeResult{}, err
	}
	operationUUID := uuid.NewString()
	operation, err := database.BeginVaultPasswordChange(db, repo.ID, operationUUID, candidate)
	if err != nil {
		return VaultPasswordChangeResult{}, err
	}
	repo.PendingPassphrase = candidate
	if _, err := vaultprofile.PreparePasswordRotationStage(repo.ID, operationUUID); err != nil {
		_ = database.RecordVaultPasswordChangeError(db, repo.ID, err.Error())
		return passwordChangeResult(repo, operation), err
	}
	return resumeVaultPasswordChangeUnderLock(ctx, db, repo, operation)
}

func RecoverVaultPasswordChange(ctx context.Context, db *sql.DB, repositoryID string) (VaultPasswordChangeResult, error) {
	repo, err := database.GetRepository(db, repositoryID)
	if err != nil {
		return VaultPasswordChangeResult{}, err
	}
	operation, err := database.VaultPasswordChange(db, repositoryID)
	if err != nil {
		return VaultPasswordChangeResult{}, err
	}
	unlock, ok, lockErr := vaultlock.YieldLowPriorityAndTryExclusiveContext(ctx, repo.ID)
	if lockErr != nil {
		return passwordChangeResult(repo, operation), lockErr
	}
	if !ok {
		return passwordChangeResult(repo, operation), fmt.Errorf("vault is busy with another operation")
	}
	defer unlock()
	if operation.Phase != "cleanup_pending" {
		repo, err = admitVaultPasswordRecoveryUnderLock(ctx, db, repo, operation)
		if err != nil {
			return passwordChangeResult(repo, operation), err
		}
	}
	return resumeVaultPasswordChangeUnderLock(ctx, db, repo, operation)
}

func admitVaultPasswordRecoveryUnderLock(ctx context.Context, db *sql.DB, repo models.Repository, operation database.VaultPasswordChangeOperation) (models.Repository, error) {
	assertControlPlane := func(ctx context.Context, frozen models.Repository) error {
		oldStore := (vaultprofile.Store{Repository: frozen}).WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
		switch operation.Phase {
		case "preparing":
			return assertVaultPasswordChangeAuthority(ctx, oldStore, frozen, operation, true, false)
		case "native_started":
			if conclusivePreMutationRollback(repo.Engine, operation) {
				return assertVaultPasswordChangeAuthority(ctx, oldStore, frozen, operation, true, false)
			}
			return assertVaultPasswordChangeAuthority(ctx, oldStore, frozen, operation, false, true)
		case "publishing":
			if err := oldStore.AssertPasswordChangeRootState(ctx, frozen.ProfileUUID, operation.OperationUUID, true); err == nil {
				return nil
			}
			candidate := frozen
			candidate.Passphrase = frozen.PendingPassphrase
			newStore := (vaultprofile.Store{Repository: candidate}).WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
			return newStore.AssertPasswordChangeRootState(ctx, frozen.ProfileUUID, operation.OperationUUID, false)
		default:
			return fmt.Errorf("vault-password recovery phase is invalid")
		}
	}
	selectPassword := func(ctx context.Context, frozen models.Repository) (string, error) {
		switch operation.Phase {
		case "preparing":
			return frozen.Passphrase, nil
		case "native_started":
			if err := probeVaultPassword(ctx, frozen, frozen.PendingPassphrase); err == nil {
				return frozen.PendingPassphrase, nil
			}
			if err := probeVaultPassword(ctx, frozen, frozen.Passphrase); err == nil {
				return frozen.Passphrase, nil
			}
			return "", fmt.Errorf("neither saved vault-password candidate verifies the exact native repository")
		case "publishing":
			return frozen.PendingPassphrase, nil
		default:
			return "", fmt.Errorf("vault-password recovery phase is invalid")
		}
	}
	return admitPasswordChangeRecoveryUnderLock(ctx, db, repo,
		repositoryadmission.PasswordChangeRecoveryOptions{
			OperationUUID: operation.OperationUUID, Phase: operation.Phase,
			AssertControlPlane: assertControlPlane, SelectPassword: selectPassword,
		})
}

func preparePasswordChange(ctx context.Context, repo models.Repository, operation database.VaultPasswordChangeOperation) (vaultprofile.PasswordRotationInventory, error) {
	store := (vaultprofile.Store{Repository: repo}).WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
	assertAuthority := func(allowUnfenced, requireProfileFence bool) error {
		return assertVaultPasswordChangeAuthority(ctx, store, repo, operation, allowUnfenced, requireProfileFence)
	}
	restartable := func(prior error) bool {
		if prior == nil || errors.Is(prior, vaultprofile.ErrPasswordChangeAuthority) {
			return false
		}
		_, freshErr := store.CapturePasswordRotationInventory(ctx, operation.OperationUUID, false)
		return freshErr == nil
	}
	for {
		if err := assertAuthority(true, false); err != nil {
			return vaultprofile.PasswordRotationInventory{}, err
		}
		observed, err := store.CapturePasswordRotationInventory(ctx, operation.OperationUUID, false)
		if err != nil {
			return observed, err
		}
		if err := store.SetPasswordChangeFence(ctx, observed, true); err != nil {
			if restartable(err) {
				continue
			}
			return observed, err
		}
		if err := assertAuthority(false, true); err != nil {
			return observed, err
		}
		fenced, err := store.CapturePasswordRotationInventory(ctx, operation.OperationUUID, true)
		if err != nil {
			if restartable(err) {
				continue
			}
			return fenced, err
		}
		if err := store.DeletePasswordChangePending(ctx, fenced); err != nil {
			return fenced, err
		}
		stable, err := store.CapturePasswordRotationInventory(ctx, operation.OperationUUID, true)
		if err != nil {
			return stable, err
		}
		if len(stable.Pending) != 0 {
			continue
		}
		timer := time.NewTimer(vaultPasswordQuietPeriod)
		select {
		case <-ctx.Done():
			timer.Stop()
			return stable, ctx.Err()
		case <-timer.C:
		}
		rechecked, err := store.CapturePasswordRotationInventory(ctx, operation.OperationUUID, true)
		if err != nil {
			if restartable(err) {
				continue
			}
			return rechecked, err
		}
		if !vaultprofile.PasswordRotationInventoriesEqual(stable, rechecked) {
			continue
		}
		if err := vaultprofile.ResetPasswordRotationStage(repo.ID, operation.OperationUUID); err != nil {
			return rechecked, err
		}
		if err := vaultprofile.StagePasswordRotationInventory(repo.ID, operation.OperationUUID, rechecked); err != nil {
			return rechecked, err
		}
		staged, err := loadVaultPasswordStage(repo.ID, operation.OperationUUID)
		if err != nil {
			return rechecked, err
		}
		final, err := store.CapturePasswordRotationInventory(ctx, operation.OperationUUID, true)
		if err != nil {
			if restartable(err) {
				continue
			}
			return final, err
		}
		if !vaultprofile.PasswordRotationInventoriesEqual(staged, final) {
			continue
		}
		if err := assertAuthority(false, true); err != nil {
			return final, err
		}
		return staged, nil
	}
}

func rollbackPreparingPasswordChange(db *sql.DB, repo models.Repository, operation database.VaultPasswordChangeOperation) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	store := (vaultprofile.Store{Repository: repo}).WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
	if err := assertVaultPasswordChangeAuthority(ctx, store, repo, operation, true, false); err != nil {
		return err
	}
	inventory, err := store.CapturePasswordRotationInventory(ctx, operation.OperationUUID, false)
	if err == nil {
		err = store.DeletePasswordChangeOperationPending(ctx, inventory)
	}
	if err == nil {
		inventory, err = store.CapturePasswordRotationInventory(ctx, operation.OperationUUID, false)
	}
	if err == nil {
		err = assertVaultPasswordChangeAuthority(ctx, store, repo, operation, true, false)
	}
	if err == nil {
		err = store.SetPasswordChangeFence(ctx, inventory, false)
	}
	if err != nil {
		return err
	}
	if cleanupErr := vaultprofile.CleanupPasswordRotationStage(repo.ID, operation.OperationUUID); cleanupErr != nil {
		return cleanupErr
	}
	return database.AbandonPreparedVaultPasswordChange(db, repo.ID)
}

type nativePasswordRecoveryDecision int

const (
	nativePasswordRecoveryAmbiguous nativePasswordRecoveryDecision = iota
	nativePasswordRecoveryForward
	nativePasswordRecoveryAbandon
)

func decideNativePasswordRecovery(engine, nativeStatus, disposition string, candidateWorks, committedWorks bool) nativePasswordRecoveryDecision {
	if candidateWorks {
		return nativePasswordRecoveryForward
	}
	if committedWorks && (nativeStatus == "not_started" ||
		(engine == engines.ResticID && nativeStatus == "failed" &&
			disposition == string(engines.PasswordMutationRejectedBeforeMutation))) {
		return nativePasswordRecoveryAbandon
	}
	return nativePasswordRecoveryAmbiguous
}

func conclusivePreMutationRollback(engine string, operation database.VaultPasswordChangeOperation) bool {
	if operation.NativeStatus == "not_started" {
		return operation.NativeMutationDisposition == string(engines.PasswordMutationUnknown)
	}
	return engine == engines.ResticID && operation.NativeStatus == "failed" &&
		operation.NativeMutationDisposition == string(engines.PasswordMutationRejectedBeforeMutation)
}

// validatePreMutationRollbackInventory accepts the exact partial states that
// profile-first/root-last cleanup can leave behind. Capture already bounds the
// protected inventory; this check keeps a foreign fence or root-first residue
// from becoming valid cleanup input.
func validatePreMutationRollbackInventory(repo models.Repository, operation database.VaultPasswordChangeOperation, inventory vaultprofile.PasswordRotationInventory) (bool, error) {
	rootFound, rootFenced, profileFenced := false, false, false
	for _, object := range inventory.Objects {
		switch object.Role {
		case "root_canonical":
			rootFound = true
			root, err := vaultprofile.ParseRoot(object.Data, repo.Connector)
			if err != nil {
				return false, fmt.Errorf("validate password-change rollback root: %w", err)
			}
			if root.PasswordChange != nil {
				if root.PasswordChange.OperationUUID != operation.OperationUUID {
					return false, fmt.Errorf("protected vault root has a foreign password-change fence")
				}
				rootFenced = true
			}
		case "profile_canonical":
			profile, err := vaultprofile.Parse(object.Data)
			if err != nil {
				return false, fmt.Errorf("validate password-change rollback profile: %w", err)
			}
			if profile.PasswordChange != nil {
				if profile.PasswordChange.OperationUUID != operation.OperationUUID {
					return false, fmt.Errorf("protected profile has a foreign password-change fence")
				}
				profileFenced = true
			}
		}
	}
	if !rootFound {
		return false, fmt.Errorf("canonical protected vault root is required")
	}
	if !rootFenced && profileFenced {
		return false, fmt.Errorf("password-change rollback fence order is invalid")
	}
	return rootFenced || profileFenced, nil
}

func rollbackConclusivePreMutationPasswordChange(ctx context.Context, db *sql.DB, repo models.Repository, operation database.VaultPasswordChangeOperation) error {
	if !conclusivePreMutationRollback(repo.Engine, operation) {
		return fmt.Errorf("native password change lacks conclusive pre-mutation evidence")
	}
	store := (vaultprofile.Store{Repository: repo}).WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
	if err := assertVaultPasswordChangeAuthority(ctx, store, repo, operation, true, false); err != nil {
		return err
	}
	inventory, err := captureVaultPasswordRollbackInventory(ctx, store, operation.OperationUUID)
	if err != nil {
		return err
	}
	if _, err := validatePreMutationRollbackInventory(repo, operation, inventory); err != nil {
		return err
	}
	if err := deleteVaultPasswordRollbackPending(ctx, store, inventory); err != nil {
		return err
	}
	inventory, err = captureVaultPasswordRollbackInventory(ctx, store, operation.OperationUUID)
	if err != nil {
		return err
	}
	hasFence, err := validatePreMutationRollbackInventory(repo, operation, inventory)
	if err != nil {
		return err
	}
	if err := assertVaultPasswordChangeAuthority(ctx, store, repo, operation, true, false); err != nil {
		return err
	}
	if hasFence {
		if err := removeVaultPasswordChangeFence(ctx, repo, inventory); err != nil {
			return err
		}
	}
	final, err := captureVaultPasswordRollbackInventory(ctx, store, operation.OperationUUID)
	if err != nil {
		return err
	}
	finalFenced, err := validatePreMutationRollbackInventory(repo, operation, final)
	if err != nil {
		return err
	}
	if finalFenced || len(final.Pending) != 0 {
		return fmt.Errorf("password-change rollback remote cleanup is incomplete")
	}
	if err := assertVaultPasswordChangeAuthority(ctx, store, repo, operation, true, false); err != nil {
		return err
	}
	if err := cleanupVaultPasswordStage(repo.ID, operation.OperationUUID); err != nil {
		return err
	}
	return abandonConclusivePreMutationPasswordChange(db, repo, operation)
}

func recoverNativeTruth(ctx context.Context, db *sql.DB, repo models.Repository, operation database.VaultPasswordChangeOperation) (bool, error) {
	candidateWorks := probeVaultPassword(ctx, repo, repo.PendingPassphrase) == nil
	committedWorks := false
	if !candidateWorks {
		committedWorks = probeVaultPassword(ctx, repo, repo.Passphrase) == nil
	}
	switch decideNativePasswordRecovery(repo.Engine, operation.NativeStatus, operation.NativeMutationDisposition, candidateWorks, committedWorks) {
	case nativePasswordRecoveryForward:
		return true, nil
	case nativePasswordRecoveryAbandon:
		return false, rollbackConclusivePreMutationPasswordChange(ctx, db, repo, operation)
	default:
		return false, fmt.Errorf("native vault-password outcome is ambiguous; manual recovery is required")
	}
}

func resumeVaultPasswordChangeUnderLock(ctx context.Context, db *sql.DB, repo models.Repository, operation database.VaultPasswordChangeOperation) (VaultPasswordChangeResult, error) {
	if repo.PendingPassphrase == "" && operation.Phase != "cleanup_pending" {
		return passwordChangeResult(repo, operation), fmt.Errorf("pending vault-password candidate is unavailable; manual recovery is required")
	}
	if operation.Phase == "preparing" {
		_, err := prepareVaultPasswordChange(ctx, repo, operation)
		if err != nil {
			_ = database.RecordVaultPasswordChangeError(db, repo.ID, err.Error())
			if errors.Is(err, vaultprofile.ErrPasswordChangeAuthority) {
				return passwordChangeResult(repo, operation), err
			}
			if rollbackErr := rollbackPreparingPasswordChange(db, repo, operation); rollbackErr != nil {
				return passwordChangeResult(repo, operation), fmt.Errorf("prepare vault-password change: %v; fence cleanup remains pending: %w", err, rollbackErr)
			}
			return VaultPasswordChangeResult{}, err
		}
		store := (vaultprofile.Store{Repository: repo}).WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
		if err := assertVaultPasswordChangeAuthority(ctx, store, repo, operation, false, true); err != nil {
			_ = database.RecordVaultPasswordChangeError(db, repo.ID, err.Error())
			return passwordChangeResult(repo, operation), err
		}
		if err := vaultPasswordChangeFault("final_inventory_and_authority_verified"); err != nil {
			return passwordChangeResult(repo, operation), err
		}
		if err := database.AdvanceVaultPasswordChange(db, repo.ID, "preparing", "native_started"); err != nil {
			return passwordChangeResult(repo, operation), err
		}
		operation, _ = database.VaultPasswordChange(db, repo.ID)
		if err := vaultPasswordChangeFault("native_start_persisted"); err != nil {
			return passwordChangeResult(repo, operation), err
		}
		engine, err := resolveVaultPasswordEngine(repo, storageavailability.RequireRepositoryAvailable)
		if err != nil {
			if persistErr := database.RecordVaultPasswordNativeResult(db, repo.ID, "not_started", string(engines.PasswordMutationUnknown), "", err.Error()); persistErr != nil {
				return passwordChangeResult(repo, operation), fmt.Errorf("persist native not-started result: %w", persistErr)
			}
			operation, _ = database.VaultPasswordChange(db, repo.ID)
		} else {
			nativeInputPath := ""
			if repo.Engine == engines.ResticID {
				nativeInputPath, err = vaultprofile.WritePasswordRotationNativeInput(repo.ID, operation.OperationUUID, repo.PendingPassphrase)
				if err != nil {
					if persistErr := database.RecordVaultPasswordNativeResult(db, repo.ID, "not_started", string(engines.PasswordMutationUnknown), "", err.Error()); persistErr != nil {
						return passwordChangeResult(repo, operation), fmt.Errorf("prepare native password input: %v; persist native not-started result: %w", err, persistErr)
					}
					operation, _ = database.VaultPasswordChange(db, repo.ID)
					goto nativeRecorded
				}
			}
			native, nativeErr := runNativeVaultPasswordChange(ctx, engine, repo, repo.PendingPassphrase, operation.OperationUUID, nativeInputPath)
			if err := vaultPasswordChangeFault("native_launched"); err != nil {
				return passwordChangeResult(repo, operation), err
			}
			var persistResult VaultPasswordChangeResult
			operation, persistResult, err = persistRunLocalNativePasswordResult(db, repo, operation, native, nativeErr)
			if err != nil {
				return persistResult, err
			}
			if err := vaultPasswordChangeFault("native_result_persisted"); err != nil {
				return persistResult, err
			}
		}
	nativeRecorded:
	}
	if operation.Phase == "native_started" {
		forward, err := recoverNativeTruth(ctx, db, repo, operation)
		if err != nil {
			if operation.NativeMutationDisposition != string(engines.PasswordMutationRejectedBeforeMutation) {
				_ = database.RecordVaultPasswordChangeError(db, repo.ID, err.Error())
			}
			return passwordChangeResult(repo, operation), err
		}
		if !forward {
			result := passwordChangeResult(repo, operation)
			result.CleanupPending = false
			if operation.NativeMutationDisposition == string(engines.PasswordMutationRejectedBeforeMutation) {
				result.Phase = "failed"
				result.SidecarStatus = "not_changed"
				result.Message = "Restic rejected the password change because the repository was locked before mutation. The protected recovery metadata and committed password were left unchanged, and this operation's fence and staging were removed."
				if operation.LastError != "" {
					return result, errors.New(operation.LastError)
				}
				return result, fmt.Errorf("native Restic password change was rejected before mutation")
			}
			result.Phase = "not_started"
			result.SidecarStatus = "not_changed"
			result.Message = "Vault-password change did not start and its remote fence was removed."
			return result, nil
		}
		if err := database.AdvanceVaultPasswordChangeAfterCandidateVerification(db, repo.ID, operation.OperationUUID); err != nil {
			return passwordChangeResult(repo, operation), err
		}
		operation, _ = database.VaultPasswordChange(db, repo.ID)
	}
	if operation.Phase == "publishing" {
		if err := probeVaultPassword(ctx, repo, repo.PendingPassphrase); err != nil {
			return passwordChangeResult(repo, operation), fmt.Errorf("new native credential no longer verifies the exact repository; manual recovery is required")
		}
		staged, err := loadVaultPasswordStage(repo.ID, operation.OperationUUID)
		if err != nil {
			return passwordChangeResult(repo, operation), fmt.Errorf("durable password-change staging is unavailable; manual recovery is required: %w", err)
		}
		candidate := repo
		candidate.Passphrase = repo.PendingPassphrase
		store := (vaultprofile.Store{Repository: candidate}).WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
		if err := publishVaultPasswordTree(ctx, store, staged); err != nil {
			_ = database.RecordVaultPasswordChangeError(db, repo.ID, err.Error())
			if operation.NativeStatus == "succeeded" {
				return passwordChangeResult(repo, operation), fmt.Errorf("native vault password changed but protected recovery metadata publication is incomplete: %w", err)
			}
			return passwordChangeResult(repo, operation), fmt.Errorf("%s, but protected recovery metadata publication is incomplete: %w", verifiedCandidateNativeSummary(operation), err)
		}
		if err := vaultPasswordChangeFault("sidecar_tree_published"); err != nil {
			return passwordChangeResult(repo, operation), err
		}
		if err := database.CommitVaultPasswordChange(db, repo.ID); err != nil {
			return passwordChangeResult(repo, operation), err
		}
		repo.Passphrase, repo.PendingPassphrase = repo.PendingPassphrase, ""
		operation, _ = database.VaultPasswordChange(db, repo.ID)
		if err := vaultPasswordChangeFault("credential_committed"); err != nil {
			return passwordChangeResult(repo, operation), err
		}
	}
	if operation.Phase == "cleanup_pending" {
		if err := cleanupVaultPasswordStage(repo.ID, operation.OperationUUID); err != nil {
			_ = database.RecordVaultPasswordChangeError(db, repo.ID, err.Error())
			if operation.NativeStatus == "succeeded" {
				return passwordChangeResult(repo, operation), fmt.Errorf("vault password changed successfully but local staging cleanup is pending: %w", err)
			}
			return passwordChangeResult(repo, operation), fmt.Errorf("%s and protected recovery metadata was updated, but local staging cleanup is pending: %w", verifiedCandidateNativeSummary(operation), err)
		}
		if err := vaultPasswordChangeFault("local_cleanup_deleted"); err != nil {
			return passwordChangeResult(repo, operation), err
		}
		if err := database.CompleteVaultPasswordChangeCleanup(db, repo.ID); err != nil {
			return passwordChangeResult(repo, operation), err
		}
		result := passwordChangeResult(repo, operation)
		result.Phase, result.SidecarStatus, result.CleanupPending = "completed", "succeeded", false
		if operation.NativeStatus == "succeeded" {
			result.Message = "Vault password changed successfully. Other computers must reconnect and enter the current password."
		} else {
			result.Message = fmt.Sprintf("The pending vault password verified and protected recovery metadata was updated. The recorded native result remains %s. Other computers must reconnect and enter the current password.", nativePasswordResultTruth(operation))
		}
		return result, nil
	}
	return passwordChangeResult(repo, operation), fmt.Errorf("vault-password change phase is invalid")
}

// RecoverVaultPasswordChangesOnce performs the one startup attempt required
// before ordinary workers and HTTP admission begin. It never polls or starts a
// background retry service.
func RecoverVaultPasswordChangesOnce(ctx context.Context, db *sql.DB) error {
	operations, err := database.ListVaultPasswordChanges(db)
	if err != nil {
		return err
	}
	for _, operation := range operations {
		if _, err := RecoverVaultPasswordChange(ctx, db, operation.RepositoryID); err != nil {
			log.Printf("vault-password recovery %s remains blocked: %v", operation.RepositoryID, err)
		}
	}
	return nil
}
