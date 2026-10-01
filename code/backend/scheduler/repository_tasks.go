package scheduler

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/kopiapolicy"
	"github.com/local/replicaro/locale"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/notifications"
	"github.com/local/replicaro/operationlog"
	"github.com/local/replicaro/operationruntime"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
	"github.com/local/replicaro/vaultreconnect"
)

var ErrRepositoryTaskRunning = errors.New("repository task is already running")
var prepareIntegrityCheck = engines.PrepareIntegrityCheck
var admitRepository = repositoryadmission.AdmitUnderLock
var beforeKopiaPolicyReassurance = func() {}
var beforeIntegrityPreparation = func() {}
var finishRepositoryOperation = database.FinishRepositoryOperation
var finishRepositoryStorageAdmissionStep = database.FinishOperationStep
var finishRepositoryOwnerAdmissionStep = database.FinishOperationStep
var finishRepositoryTaskStep = database.FinishOperationStep
var reassureKopiaRepositoryUnderLock = kopiapolicy.ReassureRepositoryUnderLock
var ensureKopiaMaintenanceOwner = engines.EnsureKopiaMaintenanceOwner

// ErrRepositoryTaskPersistence marks a failure to persist task state, which
// may be joined with a vault-owner or conflict error. API callers must not
// report a definitive 403/409 when the task's admission or terminal state
// was not persisted.
var ErrRepositoryTaskPersistence = errors.New("repository task persistence failed")
var errRepositoryTaskStepFinalization = errors.New("repository task step finalization failed")

func finishStartedRepositoryTaskStep(db *sql.DB, operationID, kind, status, output string, at time.Time) error {
	if err := finishRepositoryTaskStep(db, operationID, kind, status, output, at); err != nil {
		return errors.Join(ErrRepositoryTaskPersistence,
			fmt.Errorf("%w: persist %s result: %v", errRepositoryTaskStepFinalization, kind, err))
	}
	return nil
}

func skipRepositoryTaskStep(db *sql.DB, operationID, domain, kind, reason string, at time.Time) error {
	err := database.SkipOperationStep(db, operationID, domain, kind, reason, at)
	if errors.Is(err, database.ErrOperationStepFinalization) {
		return errors.Join(ErrRepositoryTaskPersistence,
			fmt.Errorf("%w: persist skipped %s result: %v", errRepositoryTaskStepFinalization, kind, err))
	}
	if err != nil {
		return fmt.Errorf("%w: persist skipped %s step: %v", ErrRepositoryTaskPersistence, kind, err)
	}
	return nil
}

func SetRepositoryAdmissionForTests(next func(context.Context, *sql.DB, models.Repository) (models.Repository, error)) func() {
	previous := admitRepository
	admitRepository = next
	return func() { admitRepository = previous }
}

var assertRepositoryOwner = func(ctx context.Context, repo models.Repository) error {
	return (vaultprofile.Store{Repository: repo}).
		WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).
		AssertRootOwner(ctx, repo.ClientUUID, repo.ProfileUUID, repo.AttachmentGeneration)
}

var refreshRepositoryOwnerVaultCare = func(ctx context.Context, repo models.Repository) (models.Repository, error) {
	authority, err := (vaultprofile.Store{Repository: repo}).
		WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).
		ReadRootCareAuthority(ctx)
	if err != nil {
		return models.Repository{}, err
	}
	if err := authority.AssertRepositoryOwner(repo); err != nil {
		return models.Repository{}, err
	}
	repo.CheckSchedule = authority.IntegritySchedule
	repo.MaintenanceSchedule = authority.MaintenanceSchedule
	repo.ObjectLock = authority.ObjectLock
	return repo, nil
}

var assertRepositoryWriter = func(ctx context.Context, repo models.Repository) error {
	if err := assertRepositoryOwner(ctx, repo); err != nil {
		return err
	}
	if repo.Engine == engines.KopiaID {
		engine, err := engines.ResolveWithRepositoryAvailabilityCheck(repo, storageavailability.RequireRepositoryAvailable)
		if err != nil {
			return err
		}
		return engines.VerifyKopiaMaintenanceOwner(ctx, engine, repo)
	}
	return nil
}

var observeScheduledRepository = storageavailability.ObserveRepository

// deferUnavailableRepositoryTask applies the before-run pause rule to a
// scheduled vault check or maintenance before any operation is created, so
// an unplugged drive or offline NAS does not produce a failed operation and
// a notification on every scheduled run. When the vault's storage is
// temporarily unavailable the task is simply moved unavailableCatchUpInterval
// into the future (the same recheck cadence as paused backups) and nothing
// else happens. A storage error that is not a pause still runs the task, so
// its admission fails visibly. Manual checks and maintenance do not come
// through here and report unavailability immediately.
func deferUnavailableRepositoryTask(ctx context.Context, db *sql.DB, repo models.Repository, operation string) (bool, error) {
	if repo.Connector != "fs" {
		return false, nil
	}
	observation := observeScheduledRepository(ctx, repo, time.Now().UTC())
	if ctx.Err() != nil {
		return true, nil
	}
	if observation.State != database.StorageUnavailable {
		return false, nil
	}
	due := repo.NextCheck
	if operation == "maintenance" {
		due = repo.NextMaintenance
	}
	return true, database.DeferRepositoryTask(db, repo.ID, operation, due, time.Now().Add(unavailableCatchUpInterval))
}

// ValidateRepositoryTaskRequest holds the checks a check or maintenance
// request makes before any operation record exists: a supported engine, and
// no integrity check on a cold storage vault (cold storage has none).
func ValidateRepositoryTaskRequest(repo models.Repository, operation string) error {
	if !models.ValidEngine(repo.Engine) {
		return errors.New("legacy plaintext or uncompressed Klosets are disabled")
	}
	if normalizedRepositoryTask(operation) == "check" && repo.ColdStorage {
		return fmt.Errorf("%s", models.ColdStorageIntegrityHelp)
	}
	return nil
}

func normalizedRepositoryTask(operation string) string {
	if operation != "maintenance" {
		return "check"
	}
	return operation
}

// runScheduledRepositoryTask is the scheduler coordinator's entry. A scheduled
// task never waits for a busy vault: it tries the lock once, and a vault that
// is busy or already has the same task queued or running (for example a manual
// check waiting for the lock) reports ErrRepositoryTaskRunning and is tried on
// a later tick. Manual checks and maintenance come in through
// RunQueuedRepositoryTask instead.
func runScheduledRepositoryTask(ctx context.Context, db *sql.DB, repo models.Repository, operation string, runtimeManager *operationruntime.Manager) (string, error) {
	operation = normalizedRepositoryTask(operation)
	if err := ValidateRepositoryTaskRequest(repo, operation); err != nil {
		return "", err
	}
	unlock, ok, lockErr := vaultlock.YieldLowPriorityAndTryExclusiveContext(ctx, repo.ID)
	if lockErr != nil {
		return "", lockErr
	}
	if !ok {
		return "", ErrRepositoryTaskRunning
	}
	defer unlock()

	operationID, err := retryRepositoryPersistence(ctx, func() (string, error) {
		return database.StartRepositoryOperation(db, repo, operation, time.Now())
	})
	if errors.Is(err, database.ErrRepositoryTaskActive) || errors.Is(err, database.ErrVaultBeingRemoved) {
		// A vault whose removal is pending is skipped like a busy one: no record,
		// no issue, and a later tick tries again (by then the vault is usually
		// gone).
		return "", ErrRepositoryTaskRunning
	}
	if err != nil {
		return "", fmt.Errorf("start repository %s: %w", operation, err)
	}
	operationCtx, cancelOperation := context.WithCancel(ctx)
	if runtimeManager == nil {
		runtimeManager = operationruntime.New()
	}
	if registerErr := runtimeManager.Register(operationID, func() error { cancelOperation(); return nil }); registerErr != nil {
		cancelOperation()
		_ = database.FinishRepositoryOperation(db, operationID, repo, operation, "failed", registerErr.Error(), time.Now())
		return "", registerErr
	}
	operationCtx = command.ContextWithLiveOutput(operationCtx, func(stream, text string) {
		runtimeManager.Append(operationID, stream, text)
	})
	operationCtx = command.ContextWithCapturedOutputPublisher(operationCtx, func(engine, kind, status, diagnostic string, stdout, stderr io.Reader) bool {
		return operationlog.StageNativeOutput(operationID, engine, kind, status, diagnostic, stdout, stderr) == nil
	})
	output, terminalPersisted, err := runRepositoryTaskWithRuntime(operationCtx, db, repo, operation, operationID, runtimeManager, true)
	cancelOperation()
	// Failed bounded terminal persistence leaves a closed runtime aligned with
	// the active row that startup reconciliation still owns.
	if terminalPersisted {
		runtimeManager.Remove(operationID)
	}
	return output, err
}

// RunQueuedRepositoryTask runs a manual check or maintenance on the API's
// tracked operation path. The request queued its record before the worker
// waited for the vault; by the time this is called the worker holds the vault
// lock, has moved the record to running, and ctx is the operation's context
// (cancel, live output and captured native output are already attached). The
// caller keeps the lock until this returns, which is after the final status
// was saved. terminalPersisted reports whether that final write happened; when
// it did not, the record stays running for startup reconciliation.
func RunQueuedRepositoryTask(ctx context.Context, db *sql.DB, repo models.Repository, operation, operationID string, runtimeManager *operationruntime.Manager) (output string, terminalPersisted bool, err error) {
	return runRepositoryTaskWithRuntime(ctx, db, repo, normalizedRepositoryTask(operation), operationID, runtimeManager, false)
}

// runRepositoryTaskWithRuntime is the one repository-task body, run with the
// vault lock held and a running operation record. scheduled is true only for
// the scheduler's coordinator; manual checks and maintenance pass false and
// keep reporting (and notifying) every failure.
func runRepositoryTaskWithRuntime(ctx context.Context, db *sql.DB, repo models.Repository, operation, operationID string, runtimeManager *operationruntime.Manager, scheduled bool) (string, bool, error) {
	var err error
	terminalPersisted := false
	admitted, admissionErr := admitRepository(ctx, db, repo)
	// A scheduled check or maintenance on a filesystem vault can pass
	// deferUnavailableRepositoryTask's probe and still find the vault
	// unavailable here, typically when a share hangs and the admission
	// deadline expires. That is the same pause the probe would have found, so
	// it follows the backup runner's rule for a pause found at admission: the
	// attempt keeps its one failed operation row and no failure notification
	// is sent. Unlike a pause found by the probe, the task is not moved to the
	// five-minute recheck; it keeps the next regular due time that terminal
	// persistence just set. Rechecking here would cost a failed row and up to
	// a minute of vault-lock hold every five minutes for the whole outage, so
	// a hanging share gets one silent failed row per scheduled run instead.
	// The vault task itself never escalates: only the enabled scheduled
	// backup jobs on the vault reach the 30-day unavailable issue, so a vault
	// with none never raises one. Cloud vaults are intentionally excluded and
	// keep notifying: cloud outages are not part of the storage-observation
	// pause rules.
	// Only admission's typed unavailable result qualifies; a conclusive
	// storage failure or any later error is reported and notified as usual.
	storagePaused := false
	if admissionErr != nil {
		err = fmt.Errorf("admit persisted vault for %s: %w", operation, admissionErr)
		var unavailable *storageavailability.RepositoryStorageUnavailableError
		storagePaused = scheduled && repo.Connector == "fs" && errors.As(admissionErr, &unavailable)
	} else {
		repo = admitted
	}

	var output string
	// nativeSucceeded is set only when the requested native check or
	// maintenance child itself succeeded. It is what lets a later Replicaro
	// failure (output processing, cleanup, result persistence) end as
	// completed_with_issues instead of failed.
	nativeSucceeded := false
	closeCancelGate := func() { runtimeManager.CloseCancel(operationID) }
	if operation == "maintenance" && err == nil {
		err = persistRepositoryWriterAdmission(ctx, db, operationID, repo)
		if err != nil {
			err = errors.Join(err, skipRepositoryTaskStep(db, operationID, "native", "maintenance",
				"repository writer and storage validation failed", time.Now()))
		}
	} else if operation == "maintenance" {
		err = errors.Join(err, skipRepositoryTaskStep(db, operationID, "native", "maintenance",
			"complete repository admission failed", time.Now()))
	}
	if operation == "check" && err == nil {
		err = persistRepositoryIntegrityOwnerAdmission(ctx, db, operationID, repo, "integrity_owner_admission")
		if err != nil {
			err = errors.Join(err, skipRepositoryTaskStep(db, operationID, "native", "repository_integrity_check",
				"vault owner validation failed", time.Now()))
		}
	}
	var manager engines.Engine
	if err == nil {
		manager, err = engines.ResolveWithRepositoryAvailabilityCheck(
			repo, storageavailability.RequireRepositoryAvailable,
		)
		if err != nil && operation == "maintenance" {
			err = errors.Join(err, skipRepositoryTaskStep(db, operationID, "native", "maintenance",
				"engine resolution failed", time.Now()))
		}
	}
	if err == nil {
		if operation == "maintenance" {
			if repo.Engine == engines.KopiaID {
				const reconcileKind = "kopia_policy_reconciliation"
				// Cancellation is checked before durable preparation and is also the
				// final process admission for the prerequisite native policy commands.
				// An accepted request therefore cannot launch reassurance work later.
				beforeKopiaPolicyReassurance()
				if contextErr := ctx.Err(); contextErr != nil {
					closeCancelGate()
					err = contextErr
					err = errors.Join(err,
						skipRepositoryTaskStep(db, operationID, "orchestration", reconcileKind,
							"operation was canceled before Kopia policy reconciliation", time.Now()),
						skipRepositoryTaskStep(db, operationID, "native", "maintenance",
							"operation was canceled before native maintenance", time.Now()))
				} else if stepErr := database.StartOperationStep(db, operationID, "orchestration", reconcileKind, time.Now()); stepErr != nil {
					err = fmt.Errorf("persist Kopia policy reconciliation step: %w", stepErr)
				} else {
					var reconcileOutput string
					reassureContext := command.ContextWithFinalCancellationAdmission(ctx)
					reconcileOutput, err = reassureKopiaRepositoryUnderLock(reassureContext, db, repo)
					if err == nil {
						// Reassurance may perform repository-wide policy work. Re-read the
						// canonical owner and root care immediately afterward, before the
						// separate maintenance-set mutation that applies this profile's list
						// parallelism. A takeover during reassurance must stop at this boundary.
						var refreshed models.Repository
						refreshed, err = refreshRepositoryOwnerVaultCare(reassureContext, repo)
						if err == nil {
							repo = refreshed
						}
					}
					if err == nil {
						// list-parallelism is stored in Kopia's repository-wide maintenance
						// policy, while the selected value is local profile intent. Align and
						// read it back only inside this owner-authorized maintenance path.
						var concurrencyOutput string
						mutationContext := engines.ContextWithKopiaMaintenanceMutationAdmission(
							reassureContext,
							func(admissionContext context.Context) error {
								fresh, admissionErr := refreshRepositoryOwnerVaultCare(admissionContext, repo)
								if admissionErr != nil {
									return admissionErr
								}
								if fresh.CheckSchedule != repo.CheckSchedule || fresh.MaintenanceSchedule != repo.MaintenanceSchedule ||
									fresh.ObjectLock != repo.ObjectLock {
									return fmt.Errorf("protected vault care changed before Kopia maintenance mutation")
								}
								return nil
							},
						)
						concurrencyOutput, err = ensureKopiaMaintenanceOwner(mutationContext, manager, repo)
						if strings.TrimSpace(concurrencyOutput) != "" {
							if strings.TrimSpace(reconcileOutput) != "" {
								reconcileOutput += "\n"
							}
							reconcileOutput += concurrencyOutput
						}
					}
					stepStatus := "succeeded"
					if err != nil {
						closeCancelGate()
						stepStatus = "failed"
					}
					err = errors.Join(err, finishStartedRepositoryTaskStep(
						db, operationID, reconcileKind, stepStatus,
						combinedTaskOutput(reconcileOutput, err), time.Now()))
					if err != nil {
						err = errors.Join(err, skipRepositoryTaskStep(db, operationID, "native", "maintenance",
							"Kopia policy reconciliation failed", time.Now()))
					}
				}
				// Reassurance can span native policy work, so ownership is read again
				// afterward. A conclusive takeover at that boundary supersedes this
				// machine's recovered schedule just like the ordinary owner admission;
				// ambiguous provider or sidecar failures leave the schedule intact.
				if errors.Is(err, vaultprofile.ErrNotVaultOwner) || errors.Is(err, vaultprofile.ErrVaultProfileAttachmentLost) {
					if disableErr := database.DisableRepositoryMaintenance(db, repo.ID, err.Error()); disableErr != nil {
						err = errors.Join(err, fmt.Errorf("%w: disable superseded maintenance schedule: %v", ErrRepositoryTaskPersistence, disableErr))
					}
				}
				if err == nil {
					err = persistRepositoryOwnerRevalidation(ctx, db, operationID, repo)
					if err != nil {
						closeCancelGate()
						err = errors.Join(err, skipRepositoryTaskStep(db, operationID, "native", "maintenance",
							"vault owner changed before native maintenance", time.Now()))
					}
				}
				if err == nil {
					if stepErr := database.StartOperationStep(db, operationID, "native", "maintenance", time.Now()); stepErr != nil {
						closeCancelGate()
						err = fmt.Errorf("persist native maintenance step: %w", stepErr)
					} else {
						nativeContext, nativeProcessStarted := command.ContextWithProcessStartTracking(ctx)
						nativeContext = command.ContextWithFinalCancellationAdmission(nativeContext)
						var nativeErr error
						output, nativeErr = manager.Maintenance(nativeContext, repo)
						closeCancelGate()
						nativeSucceeded = requestedNativeSucceeded(nativeErr)
						if requestedMutationStarted(nativeErr, nativeProcessStarted) {
							if dirtyErr := database.MarkVaultSizeDirty(db, repo.ID); dirtyErr != nil {
								_ = database.LogWarning(db, "Vault Size cache could not be marked dirty after native maintenance started")
							}
						}
						err = finishTrackedNativeOperation(db, operationID, "maintenance", output, nativeErr, nativeProcessStarted)
					}
				}
			} else if stepErr := database.StartOperationStep(db, operationID, "native", "maintenance", time.Now()); stepErr != nil {
				closeCancelGate()
				err = fmt.Errorf("persist native maintenance step: %w", stepErr)
			} else {
				nativeContext, nativeProcessStarted := command.ContextWithProcessStartTracking(ctx)
				// Restic's optional plain unlock is an engine-owned prerequisite.
				// Reauthorize the current root owner only after it succeeds and at
				// the narrow boundary immediately before the requested prune child.
				nativeContext = engines.ContextWithResticMaintenancePruneAdmission(
					nativeContext,
					func(admissionContext context.Context) error {
						return persistRepositoryOwnerRevalidation(
							admissionContext, db, operationID, repo,
						)
					},
				)
				nativeContext = command.ContextWithFinalCancellationAdmission(nativeContext)
				var nativeErr error
				output, nativeErr = manager.Maintenance(nativeContext, repo)
				closeCancelGate()
				nativeSucceeded = requestedNativeSucceeded(nativeErr)
				if requestedMutationStarted(nativeErr, nativeProcessStarted) {
					if dirtyErr := database.MarkVaultSizeDirty(db, repo.ID); dirtyErr != nil {
						_ = database.LogWarning(db, "Vault Size cache could not be marked dirty after native maintenance started")
					}
				}
				err = finishTrackedNativeOperation(db, operationID, "maintenance", output, nativeErr, nativeProcessStarted)
			}
		} else {
			output, nativeSucceeded, err = runIntegrityCheck(ctx, db, operationID, repo, manager, closeCancelGate)
		}
	}
	// Admission or prerequisite failure can leave no requested child to run.
	// Close before aggregate classification and terminal persistence in that case.
	closeCancelGate()
	status := repositoryTaskStatus(err, nativeSucceeded)
	output = combinedTaskOutput(output, err)
	if errors.Is(err, errRepositoryTaskStepFinalization) {
		// A started orchestration step is still running durably. Keep its parent
		// active for startup reconciliation; terminalizing only the parent would
		// strand an unreachable running child in operation history.
		return output, false, err
	}
	// No cancelable child remains. Keep the vault lock until the bounded,
	// cancel-independent terminal write has completed.
	runtimeManager.CloseCancel(operationID)
	storagePaused = storagePaused && status == "failed"
	if status == "failed" {
		// Only an Any Rclone Remote vault gets a line here: the result of
		// checking its rclone settings once after the failure.
		if note := vaultreconnect.CheckRcloneRemoteAfterFailure(db, repo.ID, operation); note != "" {
			output = strings.TrimSpace(output + "\n" + note)
		}
	}
	var dispatchNotification func()
	if status != "interrupted" && !storagePaused {
		dispatchNotification = prepareRepositoryTaskNotification(
			db, operationID, repo, operation, status,
		)
	}
	persistErr := retryRepositoryFinish(context.Background(), func() error {
		return finishRepositoryOperation(db, operationID, repo, operation, status, output, time.Now())
	})
	if persistErr != nil {
		message := fmt.Sprintf("repository %s completed but final state requires startup reconciliation: %v", operation, persistErr)
		_ = database.LogError(db, message)
		err = errors.Join(err, fmt.Errorf("%w: %s", ErrRepositoryTaskPersistence, message))
	} else {
		terminalPersisted = true
		// Both scheduled and manual tasks hold the vault lock until this body
		// returns, so the saved reconnect state follows this result before a
		// reconnect can commit.
		vaultreconnect.RecordOperationResult(db, repo.ID, operation, status, err)
		if dispatchNotification != nil {
			dispatchNotification()
		}
	}
	return output, terminalPersisted, err
}

// repositoryTaskStatus is the outcome rule for checks and maintenance. A clean
// native child followed by a failed Replicaro step (output processing,
// cleanup, result persistence) is completed_with_issues. A check that finds
// damage is a failed native child, so it is always failed; admission, owner
// and Kopia policy failures never reach the native child and stay failed.
func repositoryTaskStatus(err error, nativeSucceeded bool) string {
	switch {
	case err == nil:
		return "success"
	case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
		return "interrupted"
	case nativeSucceeded:
		return "completed_with_issues"
	default:
		return "failed"
	}
}

// requestedNativeSucceeded reports whether the requested native child itself
// succeeded. An adapter can return success together with a follow-up error
// (for example output processing or local cleanup); that still counts here.
func requestedNativeSucceeded(nativeErr error) bool {
	status, started, _, _, _, known := engines.RequestedOperationOutcome(nativeErr)
	return known && started && status == engines.RequestedOperationSucceeded
}

func requestedMutationStarted(nativeErr error, observed func() bool) bool {
	_, stageStarted, _, _, _, stageKnown := engines.RequestedOperationOutcome(nativeErr)
	if stageKnown {
		return stageStarted
	}
	return observed() || nativeErr == nil
}

func persistRepositoryWriterAdmission(ctx context.Context, db *sql.DB, operationID string, repo models.Repository) error {
	return persistRepositoryOwnerAdmission(ctx, db, operationID, repo, "repository_writer_storage_validation")
}

func persistRepositoryOwnerRevalidation(ctx context.Context, db *sql.DB, operationID string, repo models.Repository) error {
	return persistRepositoryOwnerAdmission(ctx, db, operationID, repo, "maintenance_owner_revalidation")
}

func persistRepositoryOwnerAdmission(ctx context.Context, db *sql.DB, operationID string, repo models.Repository, kind string) error {
	if err := database.StartOperationStep(db, operationID, "orchestration", kind, time.Now()); err != nil {
		return fmt.Errorf("%w: persist repository writer and storage validation start: %v", ErrRepositoryTaskPersistence, err)
	}
	admissionErr := assertRepositoryWriter(ctx, repo)
	status, output := "succeeded", "repository storage and vault ownership are valid"
	if admissionErr != nil {
		status, output = "failed", admissionErr.Error()
		// Only a definite loss of ownership disables this profile's recovered
		// schedule. A missing or corrupt canonical owner record is ambiguous: fail
		// the operation without rewriting configuration.
		if errors.Is(admissionErr, vaultprofile.ErrNotVaultOwner) || errors.Is(admissionErr, vaultprofile.ErrVaultProfileAttachmentLost) {
			if disableErr := database.DisableRepositoryMaintenance(db, repo.ID, output); disableErr != nil {
				admissionErr = errors.Join(admissionErr, fmt.Errorf("%w: disable superseded maintenance schedule: %v", ErrRepositoryTaskPersistence, disableErr))
			}
		}
	}
	if err := finishRepositoryOwnerAdmissionStep(db, operationID, kind, status, output, time.Now()); err != nil {
		return errors.Join(admissionErr, ErrRepositoryTaskPersistence,
			fmt.Errorf("%w: persist repository writer and storage validation result: %v", errRepositoryTaskStepFinalization, err))
	}
	return admissionErr
}

func persistRepositoryIntegrityOwnerAdmission(ctx context.Context, db *sql.DB, operationID string, repo models.Repository, kind string) error {
	if err := database.StartOperationStep(db, operationID, "orchestration", kind, time.Now()); err != nil {
		return fmt.Errorf("%w: persist integrity owner validation start: %v", ErrRepositoryTaskPersistence, err)
	}
	admissionErr := assertRepositoryOwner(ctx, repo)
	status, output := "succeeded", "vault ownership is current"
	if admissionErr != nil {
		status, output = "failed", admissionErr.Error()
		// A schedule recovered from the protected root must stop locally once this
		// profile is definitely no longer the owner. Ambiguous read, cancellation,
		// credential, and transitional failures fail the operation but keep the
		// configuration; a later attempt reads the owner record again.
		if errors.Is(admissionErr, vaultprofile.ErrNotVaultOwner) || errors.Is(admissionErr, vaultprofile.ErrVaultProfileAttachmentLost) {
			if disableErr := database.DisableRepositoryIntegrity(db, repo.ID, output); disableErr != nil {
				admissionErr = errors.Join(admissionErr, fmt.Errorf("%w: disable superseded integrity schedule: %v", ErrRepositoryTaskPersistence, disableErr))
			}
		}
	}
	if err := finishRepositoryOwnerAdmissionStep(db, operationID, kind, status, output, time.Now()); err != nil {
		return errors.Join(admissionErr, ErrRepositoryTaskPersistence,
			fmt.Errorf("%w: persist integrity owner validation result: %v", errRepositoryTaskStepFinalization, err))
	}
	return admissionErr
}

// runIntegrityCheck also reports whether the requested native check child
// succeeded, so the caller can tell a clean check followed by a failed cleanup
// (completed_with_issues) from a failed or damaged check (failed).
func runIntegrityCheck(ctx context.Context, db *sql.DB, operationID string, repo models.Repository, manager engines.Engine, closeCancelGate ...func()) (string, bool, error) {
	// These persisted step kinds predate Kopia's operation-config isolation and
	// remain stable for operation-history compatibility. "Cache" here describes
	// preparation of the native cache policy, not creation of a cache directory.
	const prepareKind = "integrity_cache_prepare"
	const nativeKind = "repository_integrity_check"
	const cleanupKind = "integrity_cache_cleanup"
	gateClosed := false
	closeGate := func() {
		if !gateClosed && len(closeCancelGate) > 0 && closeCancelGate[0] != nil {
			closeCancelGate[0]()
		}
		gateClosed = true
	}
	runCleanup := func(cleanup func() error) error {
		closeGate()
		cleanupStartErr := database.StartOperationStep(db, operationID, "orchestration", cleanupKind, time.Now())
		cleanupErr := cleanup()
		if cleanupStartErr != nil {
			return errors.Join(cleanupErr, fmt.Errorf("persist integrity cleanup step: %w", cleanupStartErr))
		}
		cleanupStatus := "succeeded"
		cleanupOutput := "operation-owned integrity resources removed"
		if cleanupErr != nil {
			cleanupStatus, cleanupOutput = "failed", cleanupErr.Error()
		}
		return errors.Join(cleanupErr, finishStartedRepositoryTaskStep(
			db, operationID, cleanupKind, cleanupStatus, cleanupOutput, time.Now(),
		))
	}
	skip := func(domain, kind, reason string) error {
		return skipRepositoryTaskStep(db, operationID, domain, kind, reason, time.Now())
	}

	// Kopia admission must precede its native `cache set`, which opens the
	// repository even though it mutates only the operation config. Restic has no
	// preparatory native command; keep its existing prepare-before-admission step
	// order and recorded failure steps unchanged so its operation history stays
	// consistent.
	if repo.Engine == engines.KopiaID {
		if admissionErr := persistRepositoryStorageAdmission(ctx, db, operationID, repo); admissionErr != nil {
			return "", false, errors.Join(admissionErr,
				skip("orchestration", prepareKind, "repository storage admission failed before integrity preparation"),
				skip("native", nativeKind, "repository storage admission failed"),
				skip("orchestration", cleanupKind, "integrity resources were not prepared"))
		}
	}
	beforeIntegrityPreparation()
	if contextErr := ctx.Err(); contextErr != nil {
		closeGate()
		return "", false, errors.Join(contextErr,
			skip("orchestration", prepareKind, "operation was canceled before integrity preparation"),
			skip("native", nativeKind, "operation was canceled before repository integrity check"),
			skip("orchestration", cleanupKind, "integrity resources were not prepared"))
	}
	if err := database.StartOperationStep(db, operationID, "orchestration", prepareKind, time.Now()); err != nil {
		return "", false, errors.Join(fmt.Errorf("persist integrity cache preparation step: %w", err),
			skip("native", nativeKind, "integrity preparation was not recorded"),
			skip("orchestration", cleanupKind, "integrity resources were not prepared"))
	}
	// Kopia's cache preparation may launch native work. Wrap it in the same
	// final cancellation check used for the requested full check, without
	// making cleanup cancelable.
	prepareContext := command.ContextWithFinalCancellationAdmission(ctx)
	checkContext, prepareOutput, cleanup, prepareErr := prepareIntegrityCheck(prepareContext, manager, repo)
	if prepareErr != nil {
		preparePersistErr := finishStartedRepositoryTaskStep(
			db, operationID, prepareKind, "failed", combinedTaskOutput(prepareOutput, prepareErr), time.Now(),
		)
		skipNativeErr := skip("native", nativeKind, "integrity preparation failed")
		if cleanup == nil {
			closeGate()
			return prepareOutput, false, errors.Join(prepareErr, preparePersistErr, skipNativeErr,
				skip("orchestration", cleanupKind, "integrity resources were not created"))
		}
		return prepareOutput, false, errors.Join(prepareErr, preparePersistErr, skipNativeErr, runCleanup(cleanup))
	}
	prepareResult := strings.TrimSpace(prepareOutput)
	if prepareResult == "" {
		prepareResult = "operation-scoped native cache policy prepared"
	}
	if err := finishStartedRepositoryTaskStep(db, operationID, prepareKind, "succeeded", prepareResult, time.Now()); err != nil {
		return "", false, errors.Join(fmt.Errorf("persist integrity cache preparation outcome: %w", err),
			skip("native", nativeKind, "cache preparation outcome was not persisted"), runCleanup(cleanup))
	}
	if repo.Engine != engines.KopiaID {
		if admissionErr := persistRepositoryStorageAdmission(checkContext, db, operationID, repo); admissionErr != nil {
			return "", false, errors.Join(admissionErr,
				skip("native", nativeKind, "repository storage admission failed"), runCleanup(cleanup))
		}
	}
	if admissionErr := persistRepositoryIntegrityOwnerAdmission(checkContext, db, operationID, repo, "integrity_owner_revalidation"); admissionErr != nil {
		return "", false, errors.Join(admissionErr,
			skip("native", nativeKind, "vault owner changed before native integrity check"), runCleanup(cleanup))
	}
	if err := database.StartOperationStep(db, operationID, "native", nativeKind, time.Now()); err != nil {
		return "", false, errors.Join(fmt.Errorf("persist native integrity-check step: %w", err), runCleanup(cleanup))
	}
	nativeContext, nativeProcessStarted := command.ContextWithProcessStartTracking(checkContext)
	// Restic may unlock and Kopia may connect or validate its operation config
	// after the persisted revalidation above. Re-read owner authority at the
	// adapter's final boundary so it cannot change during those prerequisites.
	nativeContext = engines.ContextWithIntegrityCheckAdmission(
		nativeContext,
		func(admissionContext context.Context) error {
			return persistRepositoryIntegrityOwnerAdmission(
				admissionContext, db, operationID, repo, "integrity_owner_final_admission",
			)
		},
	)
	nativeContext = command.ContextWithFinalCancellationAdmission(nativeContext)
	nativeContext = command.ContextWithCapturedOutputKind(nativeContext, nativeKind)
	output, nativeErr := manager.Check(nativeContext, repo)
	closeGate()
	nativePersistErr := finishTrackedNativeOperation(
		db, operationID, nativeKind, output, nativeErr, nativeProcessStarted,
	)

	return output, requestedNativeSucceeded(nativeErr), errors.Join(nativePersistErr, runCleanup(cleanup))
}

func finishTrackedNativeOperation(
	db *sql.DB,
	operationID, kind, output string,
	nativeErr error,
	processStarted func() bool,
) error {
	operationStatus, stageStarted, preparationErr, nativeStageErr, outputProcessingErr, stageKnown :=
		engines.RequestedOperationOutcome(nativeErr)
	classifiedNativeErr := nativeErr
	started := processStarted()
	if stageKnown {
		classifiedNativeErr = nativeStageErr
		started = stageStarted
	}
	status := "succeeded"
	result := output
	var persistenceErr error
	admissionRejected := stageKnown && !stageStarted && preparationErr != nil &&
		command.IsPreProcessAdmission(preparationErr)
	if stageKnown && operationStatus == engines.RequestedOperationNotStarted {
		status = "skipped"
		result = "native engine preparation failed; requested native operation was not started"
	} else if stageKnown && operationStatus == engines.RequestedOperationInterrupted && !stageStarted {
		status = "skipped"
		result = "native engine operation was interrupted before native process start"
	} else if stageKnown && operationStatus == engines.RequestedOperationInterrupted {
		status = "interrupted"
		result = combinedTaskOutput(output, classifiedNativeErr)
	} else if stageKnown && operationStatus != engines.RequestedOperationSucceeded {
		status = "failed"
		result = combinedTaskOutput(output, classifiedNativeErr)
	} else if !stageKnown && classifiedNativeErr != nil && command.IsPreProcessAdmission(classifiedNativeErr) && !started {
		admissionRejected = true
		status = "skipped"
		result = "native process skipped because admission failed before process start"
	} else if !stageKnown && classifiedNativeErr != nil {
		status = "failed"
		result = combinedTaskOutput(output, classifiedNativeErr)
	}
	if admissionRejected {
		result = "native process skipped because admission failed before process start"
		if err := database.StartOperationStep(db, operationID, "orchestration", "native_process_admission", time.Now()); err != nil {
			persistenceErr = fmt.Errorf("persist native-process admission start: %w", err)
		} else if err := finishStartedRepositoryTaskStep(
			db, operationID, "native_process_admission", "failed",
			"native process admission failed before process start", time.Now(),
		); err != nil {
			persistenceErr = fmt.Errorf("persist native-process admission failure: %w", err)
		}
	}
	persistenceErr = errors.Join(persistenceErr,
		finishStartedRepositoryTaskStep(db, operationID, kind, status, result, time.Now()))
	var outputFailure *command.OutputProcessingFailure
	if errors.As(errors.Join(outputProcessingErr, nativeErr), &outputFailure) {
		if err := database.StartOperationStep(db, operationID, "orchestration", "output_processing", time.Now()); err != nil {
			persistenceErr = errors.Join(persistenceErr, fmt.Errorf("persist output-processing start: %w", err))
		} else if err := finishStartedRepositoryTaskStep(
			db, operationID, "output_processing", "failed", "native output could not be processed completely", time.Now(),
		); err != nil {
			persistenceErr = errors.Join(persistenceErr, fmt.Errorf("persist output-processing failure: %w", err))
		}
	}
	return errors.Join(nativeErr, persistenceErr)
}

func persistRepositoryStorageAdmission(ctx context.Context, db *sql.DB, operationID string, repo models.Repository) error {
	const kind = "repository_storage_admission"
	if err := database.StartOperationStep(db, operationID, "orchestration", kind, time.Now()); err != nil {
		return fmt.Errorf("%w: persist repository storage admission start: %v", ErrRepositoryTaskPersistence, err)
	}
	admissionErr := storageavailability.RequireRepositoryAvailable(ctx, repo)
	status, output := "succeeded", "exact repository storage is available"
	if admissionErr != nil {
		status, output = "failed", admissionErr.Error()
	}
	if err := finishRepositoryStorageAdmissionStep(db, operationID, kind, status, output, time.Now()); err != nil {
		return errors.Join(admissionErr, ErrRepositoryTaskPersistence,
			fmt.Errorf("%w: persist repository storage admission result: %v", errRepositoryTaskStepFinalization, err))
	}
	return admissionErr
}

func combinedTaskOutput(output string, err error) string {
	if err == nil {
		return output
	}
	if strings.TrimSpace(output) == "" {
		return err.Error()
	}
	return output + "\n" + err.Error()
}

func retryRepositoryPersistence(ctx context.Context, fn func() (string, error)) (string, error) {
	var id string
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		id, err = fn()
		if err == nil || errors.Is(err, database.ErrRepositoryTaskActive) || errors.Is(err, database.ErrVaultBeingRemoved) || errors.Is(err, sql.ErrNoRows) {
			return id, err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Duration(attempt) * 25 * time.Millisecond):
		}
	}
	return id, err
}

func retryRepositoryFinish(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		err = fn()
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Duration(attempt) * 25 * time.Millisecond):
		}
	}
	return err
}

func prepareRepositoryTaskNotification(
	db *sql.DB,
	operationID string,
	repo models.Repository,
	operation, status string,
) func() {
	settings, err := database.GetSettings(db)
	if err != nil {
		_ = database.SkipOperationStep(db, operationID, "application", "notification",
			"notification settings unavailable: "+err.Error(), time.Now())
		return nil
	}
	success := status == "success"
	nativeEnabled, webhookEnabled := settings.NotificationChannels(status)
	event := notifications.Event{
		Event: operation, Status: status,
		Success: success, Title: repo.Name, OperationID: operationID,
	}
	if !notifications.ShouldNotify(event) || !nativeEnabled && !webhookEnabled {
		return nil
	}
	event.Locale = locale.Effective(settings.Language)
	if err := database.StartOperationStep(db, operationID, "application", "notification", time.Now()); err != nil {
		_ = database.LogError(db, "Notification step could not be registered: "+err.Error())
		return nil
	}
	// Register before terminal visibility, then let the caller dispatch only
	// after the terminal transaction succeeds. The existing running step is the
	// complete handoff barrier; no additional durable state is needed.
	return func() {
		notifications.Dispatch(settings.WebhookURL, nativeEnabled, webhookEnabled, event, func(notifyErr error) {
			stepStatus, result := "succeeded", "notification delivery completed"
			if notifyErr != nil {
				stepStatus, result = "warning", notifyErr.Error()
				_ = database.LogError(db, "Notification failed: "+notifyErr.Error())
			}
			_ = database.FinishOperationStep(db, operationID, "notification", stepStatus, result, time.Now())
		})
	}
}
