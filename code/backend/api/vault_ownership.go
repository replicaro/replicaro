package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/kopiapolicy"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
)

const ownerStatusMessage = "You are the vault owner and are responsible for running integrity checks and space reclamation. Only vault owners can run these two operations."
const ownerTakeoverExplanation = "This computer can take over vault ownership, if you want to. Taking over vault ownership changes which computer can run integrity checks and maintenance or change the vault password, but taking over vault ownership does not change or transfer backed up data, jobs, or snapshots."

type vaultOwnershipStatus struct {
	IsOwner             bool                           `json:"isOwner"`
	OwnerProfileUUID    string                         `json:"ownerProfileUUID"`
	LocalProfileUUID    string                         `json:"localProfileUUID"`
	OwnerDisplay        vaultprofile.AttachmentDisplay `json:"ownerDisplay"`
	LocalDisplay        vaultprofile.AttachmentDisplay `json:"localDisplay"`
	IntegritySchedule   string                         `json:"integritySchedule"`
	MaintenanceSchedule string                         `json:"maintenanceSchedule"`
	ObjectLock          models.ObjectLockSettings      `json:"objectLock"`
	Message             string                         `json:"message"`
	TakeoverExplanation string                         `json:"takeoverExplanation"`
}

func readFreshVaultOwnershipForTransfer(ctx context.Context, repo models.Repository, allowed *database.OwnerTransferOperation) (vaultprofile.Root, vaultprofile.Profile, []byte, error) {
	store := vaultprofile.Store{Repository: repo}.WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
	rootRead, err := store.ReadDetailed(ctx)
	if err != nil {
		return vaultprofile.Root{}, vaultprofile.Profile{}, nil, vaultprofile.ExplainSavedVaultPasswordFailure(err)
	}
	root, err := validateFreshVaultOwnershipRoot(repo, rootRead, allowed)
	if err != nil {
		return vaultprofile.Root{}, vaultprofile.Profile{}, nil, err
	}
	ownerRead, err := store.ForProfile(root.VaultOwner.ProfileUUID).ReadDetailed(ctx)
	if err != nil {
		return vaultprofile.Root{}, vaultprofile.Profile{}, nil, err
	}
	owner, err := validateFreshVaultOwnerProfile(repo, root, ownerRead)
	if err != nil {
		return vaultprofile.Root{}, vaultprofile.Profile{}, nil, err
	}
	return root, owner, rootRead.Data, nil
}

func validateFreshVaultOwnershipRoot(
	repo models.Repository,
	rootRead vaultprofile.ReadResult,
	allowed *database.OwnerTransferOperation,
) (vaultprofile.Root, error) {
	// Previous generations are recovery evidence, never takeover authority. A
	// missing canonical root may predate a completed transfer or care change.
	if rootRead.Degraded || rootRead.Generation != "canonical" {
		return vaultprofile.Root{}, fmt.Errorf("vault ownership requires the canonical protected root")
	}
	root, err := vaultprofile.ParseRoot(rootRead.Data, repo.Connector)
	if err != nil || root.VaultUUID != repo.ID || root.Repository.Engine != repo.Engine ||
		root.Repository.NativeRepositoryID != repo.NativeRepositoryID {
		return vaultprofile.Root{}, fmt.Errorf("vault ownership metadata is invalid or transitional")
	}
	if root.PasswordChange != nil {
		return vaultprofile.Root{}, fmt.Errorf("a vault-password change is in progress; owner takeover is blocked")
	}
	if root.OwnerTransfer != nil && (allowed == nil ||
		root.OwnerTransfer.OperationUUID != allowed.OperationUUID ||
		root.OwnerTransfer.FromProfileUUID != allowed.FromProfileUUID ||
		root.OwnerTransfer.ToProfileUUID != allowed.ToProfileUUID ||
		root.VaultOwner.ProfileUUID != allowed.FromProfileUUID ||
		(root.OwnerTransfer.Phase != allowed.State &&
			!(allowed.State == "reviewed" && root.OwnerTransfer.Phase == "native_owner_applied"))) {
		return vaultprofile.Root{}, fmt.Errorf("vault ownership metadata is invalid or transitional")
	}
	return root, nil
}

func validateFreshVaultOwnerProfile(
	repo models.Repository,
	root vaultprofile.Root,
	ownerRead vaultprofile.ReadResult,
) (vaultprofile.Profile, error) {
	// The attachment generation in a previous owner profile can authorize a
	// stale client after takeover, so it cannot participate in transfer review
	// or any retry that can publish the protected root.
	if ownerRead.Degraded || ownerRead.Generation != "canonical" {
		return vaultprofile.Profile{}, fmt.Errorf("vault ownership requires the canonical owner profile")
	}
	owner, err := vaultprofile.Parse(ownerRead.Data)
	if err != nil || owner.VaultUUID != repo.ID || owner.ProfileUUID != root.VaultOwner.ProfileUUID {
		return vaultprofile.Profile{}, fmt.Errorf("vault owner profile is invalid")
	}
	return owner, nil
}

func readFreshVaultOwnershipStatus(ctx context.Context, repo models.Repository) (vaultprofile.Root, vaultprofile.Profile, vaultprofile.Profile, error) {
	store := vaultprofile.Store{Repository: repo}.WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
	objects, err := store.ReadOwnershipObjects(ctx, repo.ProfileUUID)
	if err != nil {
		return vaultprofile.Root{}, vaultprofile.Profile{}, vaultprofile.Profile{}, explainOwnershipObjectsReadFailure(objects, err)
	}
	if _, err := vaultprofile.ValidateRootIdentityResult(objects.Root, repo); err != nil {
		return vaultprofile.Root{}, vaultprofile.Profile{}, vaultprofile.Profile{}, err
	}
	root, err := validateFreshVaultOwnershipRoot(repo, objects.Root, nil)
	if err != nil {
		return vaultprofile.Root{}, vaultprofile.Profile{}, vaultprofile.Profile{}, err
	}
	owner, err := validateFreshVaultOwnerProfile(repo, root, objects.Owner)
	if err != nil {
		return vaultprofile.Root{}, vaultprofile.Profile{}, vaultprofile.Profile{}, err
	}
	local, err := vaultprofile.ValidateAttachmentResult(
		objects.Local, repo.ID, repo.ProfileUUID, repo.ClientUUID, repo.AttachmentGeneration,
	)
	if err != nil {
		return vaultprofile.Root{}, vaultprofile.Profile{}, vaultprofile.Profile{}, err
	}
	return root, owner, local, nil
}

func explainOwnershipObjectsReadFailure(objects vaultprofile.OwnershipObjects, err error) error {
	if len(objects.Root.Data) == 0 {
		return fmt.Errorf("verify protected vault root: %w", vaultprofile.ExplainSavedVaultPasswordFailure(err))
	}
	return err
}

func ownershipStatus(repo models.Repository, root vaultprofile.Root, owner vaultprofile.Profile, localDisplay vaultprofile.AttachmentDisplay) vaultOwnershipStatus {
	isOwner := root.VaultOwner.ProfileUUID == repo.ProfileUUID
	message := ownerStatusMessage
	if !isOwner {
		ownerLabel := fmt.Sprintf("profile %s", owner.ProfileUUID)
		if strings.TrimSpace(owner.Attachment.Display.ComputerName) != "" &&
			strings.TrimSpace(owner.Attachment.Display.OperatingSystem) != "" {
			ownerLabel = fmt.Sprintf("%s@%s", owner.Attachment.Display.ComputerName, owner.Attachment.Display.OperatingSystem)
		}
		message = fmt.Sprintf("This computer is not the current vault owner and cannot run integrity checks, run maintenance, or change the password for this vault. `%s` is the current vault owner.", ownerLabel)
	}
	objectLock := models.ObjectLockSettings{}
	if root.ObjectLock != nil {
		objectLock = *root.ObjectLock
	}
	return vaultOwnershipStatus{IsOwner: isOwner, OwnerProfileUUID: root.VaultOwner.ProfileUUID,
		LocalProfileUUID: repo.ProfileUUID, OwnerDisplay: owner.Attachment.Display, LocalDisplay: localDisplay, Message: message,
		IntegritySchedule: root.Integrity.Schedule, MaintenanceSchedule: root.Maintenance.Schedule, ObjectLock: objectLock,
		TakeoverExplanation: ownerTakeoverExplanation}
}

func adoptFreshRootVaultCare(db *sql.DB, repo models.Repository, root vaultprofile.Root) (models.Repository, error) {
	objectLock := models.ObjectLockSettings{}
	if root.ObjectLock != nil {
		objectLock = *root.ObjectLock
	}
	// A takeover adopts the protected root's vault-wide care policy before any
	// native owner mutation. The incoming profile contributes only its local
	// concurrency preference; stale local schedules must never flow backward
	// into the root or into Kopia's repository-wide maintenance settings.
	if err := database.AdoptRepositoryVaultCare(db, repo.ID, root.Integrity.Schedule, root.Maintenance.Schedule, objectLock); err != nil {
		return models.Repository{}, err
	}
	repo.CheckSchedule = root.Integrity.Schedule
	repo.MaintenanceSchedule = root.Maintenance.Schedule
	repo.ObjectLock = objectLock
	return repo, nil
}

func adoptReviewedRootVaultCare(db *sql.DB, repo models.Repository, root vaultprofile.Root, reviewedOwnerProfileUUID string) (models.Repository, error) {
	// A stale takeover review is rejected before any local schedule, Object Lock,
	// or Kopia readiness state changes. A completed-transfer retry already named
	// as owner does not depend on the former reviewed-owner value.
	if root.VaultOwner.ProfileUUID != repo.ProfileUUID && root.VaultOwner.ProfileUUID != reviewedOwnerProfileUUID {
		return models.Repository{}, fmt.Errorf("the reviewed vault owner changed; review the takeover again")
	}
	return adoptFreshRootVaultCare(db, repo, root)
}

func readLocalOwnerCandidate(ctx context.Context, repo models.Repository) (vaultprofile.Profile, error) {
	data, err := (vaultprofile.Store{Repository: repo}).ForProfile(repo.ProfileUUID).
		WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).Read(ctx)
	if err != nil {
		return vaultprofile.Profile{}, err
	}
	profile, err := vaultprofile.Parse(data)
	if err != nil || profile.VaultUUID != repo.ID || profile.ProfileUUID != repo.ProfileUUID ||
		profile.Attachment.ClientUUID != repo.ClientUUID || profile.Attachment.Generation != repo.AttachmentGeneration {
		return vaultprofile.Profile{}, fmt.Errorf("local vault profile attachment is invalid")
	}
	return profile, nil
}

var readOwnershipForTransfer = readFreshVaultOwnershipForTransfer
var readOwnershipStatus = readFreshVaultOwnershipStatus
var readOwnershipLocalCandidate = readLocalOwnerCandidate
var assertOwnershipLocalAttachment = func(ctx context.Context, repo models.Repository) error {
	return (vaultprofile.Store{Repository: repo}).ForProfile(repo.ProfileUUID).
		WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).
		AssertAttachment(ctx, repo.ClientUUID, repo.AttachmentGeneration)
}
var ensureTakeoverKopiaMaintenanceOwner = engines.EnsureKopiaMaintenanceOwner
var queueTakeoverKopiaPolicy = kopiapolicy.QueueDirty

func currentRootOwnerMaintenanceMutationContext(ctx context.Context, repo models.Repository) context.Context {
	return engines.ContextWithKopiaMaintenanceMutationAdmission(ctx, func(admissionContext context.Context) error {
		authority, err := (vaultprofile.Store{Repository: repo}).
			WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).
			AssertRootCare(admissionContext, repo.CheckSchedule, repo.MaintenanceSchedule, repo.ObjectLock)
		if err != nil {
			return err
		}
		return authority.AssertRepositoryOwner(repo)
	})
}

func ownerTransferMaintenanceMutationContext(
	ctx context.Context, db *sql.DB, repo models.Repository, expected database.OwnerTransferOperation,
) context.Context {
	return engines.ContextWithKopiaMaintenanceMutationAdmission(ctx, func(admissionContext context.Context) error {
		fresh, err := database.ActiveOwnerTransfer(db, repo.ID)
		if err != nil || fresh != expected {
			return fmt.Errorf("reviewed vault owner transfer changed before Kopia maintenance mutation")
		}
		root, _, _, err := readOwnershipForTransfer(admissionContext, repo, &fresh)
		if err != nil {
			return err
		}
		if root.VaultOwner.ProfileUUID != fresh.FromProfileUUID || root.OwnerTransfer == nil ||
			root.OwnerTransfer.OperationUUID != fresh.OperationUUID ||
			root.OwnerTransfer.FromProfileUUID != fresh.FromProfileUUID ||
			root.OwnerTransfer.ToProfileUUID != fresh.ToProfileUUID ||
			root.OwnerTransfer.Phase != fresh.State {
			return fmt.Errorf("protected vault owner transfer changed before Kopia maintenance mutation")
		}
		return assertOwnershipLocalAttachment(admissionContext, repo)
	})
}

// admitOwnerVaultCare keeps the actual native identity proof in ordinary
// admission and supplies its complete owner/attachment proof once. Sidecar
// ownership alone is never enough to authorize native repository administration.
func admitOwnerVaultCare(ctx context.Context, db *sql.DB, repo models.Repository) (models.Repository, vaultprofile.Root, []byte, error) {
	var root vaultprofile.Root
	var rootData []byte
	admitted, err := repositoryadmission.AdmitUnderLockWithOptions(ctx, db, repo, repositoryadmission.Options{
		ResolveEngine: resolveEngine,
		AssertControlPlane: func(ctx context.Context, candidate models.Repository) error {
			var readErr error
			root, rootData, readErr = readOwnerVaultCare(ctx, candidate)
			return readErr
		},
	})
	if err == nil {
		// Native validation can outlast an attachment takeover that leaves the
		// root unchanged. Reauthorize the current canonical attachment before
		// the reviewed root can enter publication.
		err = (vaultprofile.Store{Repository: admitted}).
			WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).
			ForProfile(admitted.ProfileUUID).
			AssertAttachment(ctx, admitted.ClientUUID, admitted.AttachmentGeneration)
	}
	return admitted, root, rootData, err
}

func readOwnerVaultCare(ctx context.Context, repo models.Repository) (vaultprofile.Root, []byte, error) {
	objects, err := (vaultprofile.Store{Repository: repo}).
		WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).
		ReadOwnershipObjects(ctx, repo.ProfileUUID)
	if err != nil {
		return vaultprofile.Root{}, nil, explainOwnershipObjectsReadFailure(objects, err)
	}
	root, err := validateFreshVaultOwnershipRoot(repo, objects.Root, nil)
	if err != nil {
		return vaultprofile.Root{}, nil, err
	}
	if _, err := validateFreshVaultOwnerProfile(repo, root, objects.Owner); err != nil {
		return vaultprofile.Root{}, nil, err
	}
	if root.VaultOwner.ProfileUUID != repo.ProfileUUID {
		return vaultprofile.Root{}, nil, fmt.Errorf("only the vault owner can change integrity-check or maintenance schedules")
	}
	return root, objects.Root.Data, nil
}

func publishReviewedOwnerVaultCare(ctx context.Context, repo models.Repository, root vaultprofile.Root, rootData []byte, integritySchedule, maintenanceSchedule string, objectLock models.ObjectLockSettings) error {
	var err error
	current := models.ObjectLockSettings{}
	if root.ObjectLock != nil {
		current = *root.ObjectLock
	}
	objectLock, err = models.NormalizeObjectLock(repo.Engine, repo.Connector, objectLock)
	if err != nil {
		return err
	}
	if err := models.ValidateObjectLockTransition(current, objectLock); err != nil {
		return err
	}
	if !models.ObjectLockMaintenanceEligible(objectLock, maintenanceSchedule) {
		return fmt.Errorf("space reclamation schedule is incompatible with object lock duration")
	}
	if root.Integrity.Schedule == integritySchedule && root.Maintenance.Schedule == maintenanceSchedule && current == objectLock {
		return nil
	}
	root.Integrity.Schedule = integritySchedule
	root.Maintenance.Schedule = maintenanceSchedule
	if objectLock.Enrolled {
		settings := objectLock
		root.ObjectLock = &settings
	} else {
		root.ObjectLock = nil
	}
	root.Revision++
	root.UpdatedAt = time.Now().UTC()
	encoded, err := vaultprofile.MarshalRoot(root, repo.Connector)
	if err != nil {
		return err
	}
	hash := sha256.Sum256(rootData)
	return publishRecoveryRoot(ctx, repo, encoded, vaultprofile.PublishOptions{
		OperationID: uuid.NewString(), ExpectedCurrentSHA256: hex.EncodeToString(hash[:]),
	})
}

func handleVaultOwnership(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			w.Header().Set("Cache-Control", "no-store")
		}
		repo, err := repoFromRequest(db, r)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		switch r.Method {
		case http.MethodGet:
			readContext, unlock, lockErr := vaultlock.AcquireLowPrioritySharedContext(r.Context(), repo.ID, nil)
			if lockErr != nil {
				writeError(w, http.StatusConflict, lockErr)
				return
			}
			defer unlock()
			var root vaultprofile.Root
			var owner, local vaultprofile.Profile
			repo, err = admitPersistedControlPlaneRead(readContext, db, repo, func(assertContext context.Context, candidate models.Repository) error {
				var readErr error
				root, owner, local, readErr = readOwnershipStatus(assertContext, candidate)
				return readErr
			})
			if err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			writeJSON(w, ownershipStatus(repo, root, owner, local.Attachment.Display))
		case http.MethodPost:
			var req struct {
				ReviewedOwnerProfileUUID string `json:"reviewedOwnerProfileUUID"`
				Confirmed                bool   `json:"confirmed"`
			}
			if err := decodeRequest(r, &req); err != nil || !req.Confirmed || strings.TrimSpace(req.ReviewedOwnerProfileUUID) == "" {
				badRequest(w, "Force vault owner takeover requires review and confirmation")
				return
			}
			unlock, ok, lockErr := vaultlock.YieldLowPriorityAndTryExclusiveContext(r.Context(), repo.ID)
			if lockErr != nil {
				writeError(w, http.StatusConflict, lockErr)
				return
			}
			if !ok {
				writeError(w, http.StatusConflict, fmt.Errorf("the selected vault is busy with another operation"))
				return
			}
			defer unlock()
			repo, err = admitPersistedRepository(r.Context(), db, repo)
			if err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			op, opErr := database.ActiveOwnerTransfer(db, repo.ID)
			var allowed *database.OwnerTransferOperation
			if opErr == nil {
				allowed = &op
			} else if !errors.Is(opErr, sql.ErrNoRows) {
				writeError(w, http.StatusConflict, opErr)
				return
			}
			root, owner, rootData, err := readOwnershipForTransfer(r.Context(), repo, allowed)
			if err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			if err := assertOwnershipLocalAttachment(r.Context(), repo); err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			local, err := readOwnershipLocalCandidate(r.Context(), repo)
			if err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			// Reject a stale review before changing installation-local state. The
			// root-derived adoption below must still precede any Kopia owner mutation.
			repo, err = adoptReviewedRootVaultCare(db, repo, root, req.ReviewedOwnerProfileUUID)
			if err != nil {
				writeError(w, http.StatusConflict, fmt.Errorf("adopt authoritative vault-care settings: %w", err))
				return
			}
			if root.VaultOwner.ProfileUUID == repo.ProfileUUID {
				if active, activeErr := database.ActiveOwnerTransfer(db, repo.ID); activeErr == nil &&
					active.ToProfileUUID == repo.ProfileUUID {
					if active.FromProfileUUID == "" || active.ToProfileUUID != root.VaultOwner.ProfileUUID {
						writeError(w, http.StatusConflict, fmt.Errorf("completed vault owner transition does not match local recovery state"))
						return
					}
					if repo.Engine == engines.KopiaID {
						engine, resolveErr := resolveEngine(repo)
						if resolveErr != nil {
							writeError(w, http.StatusConflict, resolveErr)
							return
						}
						// A retry may observe a different profile-local concurrency mode
						// than the earlier native-owner phase. Root already names this
						// profile, so re-ensure the exact owner/list value before completion.
						mutationContext := currentRootOwnerMaintenanceMutationContext(r.Context(), repo)
						if _, ensureErr := ensureTakeoverKopiaMaintenanceOwner(mutationContext, engine, repo); ensureErr != nil {
							writeError(w, http.StatusConflict, fmt.Errorf("align completed Kopia vault owner: %w", ensureErr))
							return
						}
					}
					if active.State == "reviewed" {
						if err := database.AdvanceOwnerTransfer(db, active.OperationUUID, "reviewed", "native_owner_applied"); err != nil {
							writeError(w, http.StatusConflict, err)
							return
						}
						active.State = "native_owner_applied"
					}
					if active.State == "native_owner_applied" {
						if err := database.AdvanceOwnerTransfer(db, active.OperationUUID, "native_owner_applied", "root_published"); err != nil {
							writeError(w, http.StatusConflict, err)
							return
						}
						active.State = "root_published"
					}
					if active.State == "root_published" {
						if err := database.AdvanceOwnerTransfer(db, active.OperationUUID, "root_published", "completed"); err != nil {
							writeError(w, http.StatusConflict, err)
							return
						}
					}
				} else if activeErr == nil {
					writeError(w, http.StatusConflict, fmt.Errorf("active vault owner transition does not match the authoritative owner"))
					return
				} else if !errors.Is(activeErr, sql.ErrNoRows) {
					writeError(w, http.StatusConflict, activeErr)
					return
				}
				if repo.Engine == engines.KopiaID {
					// Root ownership is complete before the full managed-policy worker is
					// admitted. Ensure above aligns only native maintenance ownership.
					queueTakeoverKopiaPolicy(db, repo.ID)
				}
				writeJSON(w, ownershipStatus(repo, root, owner, local.Attachment.Display))
				return
			}
			if errors.Is(opErr, sql.ErrNoRows) {
				op, opErr = database.BeginOwnerTransfer(db, uuid.NewString(), repo.ID,
					root.VaultOwner.ProfileUUID, repo.ProfileUUID)
			}
			if opErr != nil || op.FromProfileUUID != root.VaultOwner.ProfileUUID || op.ToProfileUUID != repo.ProfileUUID {
				writeError(w, http.StatusConflict, fmt.Errorf("another vault owner transition is pending"))
				return
			}
			kopiaOwnerAligned := false
			if op.State == "reviewed" {
				if root.OwnerTransfer == nil {
					root.OwnerTransfer = &vaultprofile.OwnerTransfer{
						OperationUUID: op.OperationUUID, FromProfileUUID: op.FromProfileUUID,
						ToProfileUUID: op.ToProfileUUID, Phase: "reviewed",
					}
					root.Revision++
					root.UpdatedAt = time.Now().UTC()
					encoded, encodeErr := vaultprofile.MarshalRoot(root, repo.Connector)
					if encodeErr != nil {
						writeError(w, http.StatusInternalServerError, encodeErr)
						return
					}
					hash := sha256.Sum256(rootData)
					if err := publishRecoveryRoot(r.Context(), repo, encoded, vaultprofile.PublishOptions{
						OperationID: op.OperationUUID, ExpectedCurrentSHA256: hex.EncodeToString(hash[:]),
					}); err != nil {
						writeError(w, http.StatusConflict, fmt.Errorf("publish vault owner transition: %w", err))
						return
					}
					rootData = encoded
				}
				if repo.Engine == engines.KopiaID && root.OwnerTransfer.Phase == "reviewed" {
					engine, resolveErr := resolveEngine(repo)
					if resolveErr != nil {
						writeError(w, http.StatusConflict, resolveErr)
						return
					}
					mutationContext := ownerTransferMaintenanceMutationContext(r.Context(), db, repo, op)
					if _, nativeErr := ensureTakeoverKopiaMaintenanceOwner(mutationContext, engine, repo); nativeErr != nil {
						writeError(w, http.StatusConflict, fmt.Errorf("Kopia rejected vault owner takeover: %w", nativeErr))
						return
					}
					kopiaOwnerAligned = true
				}
				if root.OwnerTransfer.Phase == "reviewed" {
					root.OwnerTransfer.Phase = "native_owner_applied"
					root.Revision++
					root.UpdatedAt = time.Now().UTC()
					encoded, encodeErr := vaultprofile.MarshalRoot(root, repo.Connector)
					if encodeErr != nil {
						writeError(w, http.StatusInternalServerError, encodeErr)
						return
					}
					hash := sha256.Sum256(rootData)
					if err := publishRecoveryRoot(r.Context(), repo, encoded, vaultprofile.PublishOptions{
						OperationID: op.OperationUUID, ExpectedCurrentSHA256: hex.EncodeToString(hash[:]),
					}); err != nil {
						writeError(w, http.StatusConflict, fmt.Errorf("publish native-owner transfer phase: %w", err))
						return
					}
					rootData = encoded
				}
				if err := database.AdvanceOwnerTransfer(db, op.OperationUUID, "reviewed", "native_owner_applied"); err != nil {
					writeError(w, http.StatusConflict, err)
					return
				}
				op.State = "native_owner_applied"
			}
			if op.State == "native_owner_applied" {
				root, _, rootData, err = readOwnershipForTransfer(r.Context(), repo, &op)
				if err != nil {
					writeError(w, http.StatusConflict, err)
					return
				}
				if root.VaultOwner.ProfileUUID == repo.ProfileUUID && root.OwnerTransfer == nil {
					if err := database.AdvanceOwnerTransfer(db, op.OperationUUID, "native_owner_applied", "root_published"); err != nil {
						writeError(w, http.StatusConflict, err)
						return
					}
					op.State = "root_published"
				} else if root.OwnerTransfer == nil || root.VaultOwner.ProfileUUID != op.FromProfileUUID {
					writeError(w, http.StatusConflict, fmt.Errorf("vault owner transition no longer matches the reviewed owner"))
					return
				}
			}
			if op.State == "native_owner_applied" {
				if repo.Engine == engines.KopiaID && !kopiaOwnerAligned {
					engine, resolveErr := resolveEngine(repo)
					if resolveErr != nil {
						writeError(w, http.StatusConflict, resolveErr)
						return
					}
					// The transfer does not freeze profile preferences. Re-ensure on
					// every native-applied retry so root publication cannot complete
					// with list parallelism from an older local concurrency mode.
					mutationContext := ownerTransferMaintenanceMutationContext(r.Context(), db, repo, op)
					if _, ensureErr := ensureTakeoverKopiaMaintenanceOwner(mutationContext, engine, repo); ensureErr != nil {
						writeError(w, http.StatusConflict, fmt.Errorf("re-align Kopia vault owner takeover: %w", ensureErr))
						return
					}
				}
				root.VaultOwner.ProfileUUID = repo.ProfileUUID
				root.OwnerTransfer = nil
				root.Revision++
				root.UpdatedAt = time.Now().UTC()
				encoded, encodeErr := vaultprofile.MarshalRoot(root, repo.Connector)
				if encodeErr != nil {
					writeError(w, http.StatusInternalServerError, encodeErr)
					return
				}
				hash := sha256.Sum256(rootData)
				if err := publishRecoveryRoot(r.Context(), repo, encoded, vaultprofile.PublishOptions{
					OperationID: op.OperationUUID, ExpectedCurrentSHA256: hex.EncodeToString(hash[:]),
				}); err != nil {
					writeError(w, http.StatusConflict, fmt.Errorf("publish vault owner takeover: %w", err))
					return
				}
				if err := database.AdvanceOwnerTransfer(db, op.OperationUUID, "native_owner_applied", "root_published"); err != nil {
					writeError(w, http.StatusConflict, err)
					return
				}
				op.State = "root_published"
			}
			if op.State == "root_published" {
				if err := database.AdvanceOwnerTransfer(db, op.OperationUUID, "root_published", "completed"); err != nil {
					writeError(w, http.StatusConflict, err)
					return
				}
			}
			root.VaultOwner.ProfileUUID = repo.ProfileUUID
			if repo.Engine == engines.KopiaID {
				// Never queue full policy reconciliation until the canonical root has
				// durably named this profile as owner and the transfer is completed.
				queueTakeoverKopiaPolicy(db, repo.ID)
			}
			writeJSON(w, ownershipStatus(repo, root, local, local.Attachment.Display))
		default:
			badRequest(w, "invalid method")
		}
	}
}
