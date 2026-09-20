// Package repositoryadmission owns the narrow, lock-level admission proof for
// an already-persisted vault. It does not acquire coordinator or vault locks;
// callers retain their existing operation-family locking and pass the returned
// frozen view through the rest of that operation.
package repositoryadmission

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/profilebinding"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/storageidentity"
	"github.com/local/replicaro/vaultprofile"
)

// Options lets an existing operation family supply its already-established
// control-plane and engine seams. Production callers normally use zero values;
// the backup runner supplies its stricter writer proof through the same gate.
type Options struct {
	AssertControlPlane func(context.Context, models.Repository) error
	ResolveEngine      func(models.Repository) (engines.Engine, error)
}

// PasswordChangeRecoveryOptions is the exact, operation-bound exception that
// lets same-installation password recovery reuse normal filesystem resolution
// while ordinary precommit admission remains blocked.
type PasswordChangeRecoveryOptions struct {
	OperationUUID      string
	Phase              string
	AssertControlPlane func(context.Context, models.Repository) error
	SelectPassword     func(context.Context, models.Repository) (string, error)
	ResolveEngine      func(models.Repository) (engines.Engine, error)
}

// AdmitUnderLock reloads current persisted authority, resolves the filesystem
// location configured-first, proves the protected control-plane record and
// independent native repository identity, and returns one frozen runtime view.
// A later authority reload may refresh policy/ownership but must not replace
// the returned path.
func AdmitUnderLock(ctx context.Context, db *sql.DB, requested models.Repository) (models.Repository, error) {
	return admitUnderLock(ctx, db, requested, Options{}, nil, false)
}

func AdmitUnderLockWithOptions(ctx context.Context, db *sql.DB, requested models.Repository, options Options) (models.Repository, error) {
	return admitUnderLock(ctx, db, requested, options, nil, false)
}

// AdmitControlPlaneReadUnderLock reloads and freezes persisted vault authority
// for one bounded read of the protected sidecar. The supplied assertion is the
// complete control-plane proof for this read; unlike ordinary operation
// admission, this path deliberately performs no backup-engine validation or
// native configuration activation.
func AdmitControlPlaneReadUnderLock(
	ctx context.Context,
	db *sql.DB,
	requested models.Repository,
	assertControlPlane func(context.Context, models.Repository) error,
) (models.Repository, error) {
	if assertControlPlane == nil {
		return models.Repository{}, fmt.Errorf("control-plane read admission requires protected authority")
	}
	return admitUnderLock(ctx, db, requested, Options{AssertControlPlane: assertControlPlane}, nil, true)
}

// AdmitPasswordChangeRecoveryUnderLock admits only the exact locally durable
// precommit password-change operation. It returns one frozen repository path
// and restores the persisted committed/pending credential roles in memory.
func AdmitPasswordChangeRecoveryUnderLock(ctx context.Context, db *sql.DB, requested models.Repository, options PasswordChangeRecoveryOptions) (models.Repository, error) {
	if options.AssertControlPlane == nil || options.SelectPassword == nil {
		return models.Repository{}, fmt.Errorf("vault-password recovery admission is incomplete")
	}
	return admitUnderLock(ctx, db, requested, Options{
		AssertControlPlane: options.AssertControlPlane,
		ResolveEngine:      options.ResolveEngine,
	}, &options, false)
}

func admitUnderLock(
	ctx context.Context,
	db *sql.DB,
	requested models.Repository,
	options Options,
	passwordRecovery *PasswordChangeRecoveryOptions,
	controlPlaneRead bool,
) (models.Repository, error) {
	callerContext := ctx
	classifyKnownUnavailable := func(err error) error {
		if err == nil {
			return nil
		}
		if errors.Is(callerContext.Err(), context.Canceled) || errors.Is(err, context.Canceled) {
			return context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) {
			return &storageavailability.RepositoryStorageUnavailableError{
				ReasonCode: database.AvailabilityReasonObservationTimeout,
			}
		}
		return err
	}
	persisted, err := database.GetRepository(db, requested.ID)
	if err != nil {
		return models.Repository{}, err
	}
	if passwordRecovery == nil {
		if err := database.RequireNoPrecommitVaultPasswordChange(db, persisted.ID); err != nil {
			return models.Repository{}, err
		}
	} else {
		operation, operationErr := database.VaultPasswordChange(db, persisted.ID)
		if operationErr != nil || operation.OperationUUID != passwordRecovery.OperationUUID ||
			operation.Phase != passwordRecovery.Phase ||
			(operation.Phase != "preparing" && operation.Phase != "native_started" && operation.Phase != "publishing") {
			return models.Repository{}, fmt.Errorf("vault-password recovery authority changed before admission")
		}
	}
	if requested.ID != "" && (persisted.Engine != requested.Engine || persisted.Connector != requested.Connector) {
		// The saved UUID, engine, and connector define operation authority.
		// Address/volume facts are resolution hints and must not reintroduce the
		// removed physical-continuity gate through a stale caller snapshot.
		return models.Repository{}, fmt.Errorf("saved vault authority changed before admission")
	}
	repo := persisted
	configured := repo
	var resolution *storageavailability.Resolution
	controlPlaneProven := false
	if repo.Connector == "fs" {
		// One shared budget covers configured, cached, and mounted fallback
		// candidates. It is intentionally operation-local: backup/restore payload
		// execution gets its ordinary deadline after this admission returns.
		resolutionContext, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		ctx = resolutionContext
		aliasEligible := storageavailability.RepositoryAliasesEligible(repo)
		canProveAttachment := strings.TrimSpace(repo.ProfileUUID) != "" && repo.AttachmentGeneration > 0
		assertCandidateControlPlane := func(candidate models.Repository) error {
			if options.AssertControlPlane != nil {
				return options.AssertControlPlane(ctx, candidate)
			}
			store := (vaultprofile.Store{Repository: candidate}).
				WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
			if err := store.AssertRootIdentity(ctx); err != nil {
				return err
			}
			return store.ForProfile(candidate.ProfileUUID).
				AssertAttachment(ctx, candidate.ClientUUID, candidate.AttachmentGeneration)
		}
		proveCandidate := func(path string, proveControlPlane bool) (models.Repository, bool, error) {
			if err := ctx.Err(); err != nil {
				return models.Repository{}, false, err
			}
			candidate := repo
			candidate.Location = path
			candidate.ResolvedRepositoryPath = path
			present, markerErr := nativeIdentityMarkerPresent(candidate)
			if markerErr != nil {
				if storageidentity.IsInspectionError(markerErr) {
					return models.Repository{}, false, &storageavailability.RepositoryStorageUnavailableError{ReasonCode: database.AvailabilityReasonStorageMissing}
				}
				return models.Repository{}, false, markerErr
			}
			if !present {
				return models.Repository{}, false, nil
			}
			fingerprint, fingerprintErr := engines.RepositoryFingerprint(candidate, "")
			if fingerprintErr != nil {
				// Only raw local marker inspection errors are skippable here.
				// Native/protected parsing and authority failures below retain
				// their conclusive meaning even if another candidate matches.
				if storageidentity.IsInspectionError(fingerprintErr) {
					return models.Repository{}, false, &storageavailability.RepositoryStorageUnavailableError{ReasonCode: database.AvailabilityReasonStorageMissing}
				}
				return models.Repository{}, false, fingerprintErr
			}
			if fingerprint != repo.NativeRepositoryID {
				return models.Repository{}, false, nil
			}
			if proveControlPlane && canProveAttachment {
				if err := assertCandidateControlPlane(candidate); err != nil {
					if errors.Is(err, vaultprofile.ErrProtectedRootIdentityMismatch) {
						// A copied repository can legitimately share the native ID while
						// carrying a different protected vault UUID. It is a non-match,
						// not authority to stop checking other exact mounted candidates.
						return models.Repository{}, false, nil
					}
					return models.Repository{}, false, err
				}
			}
			return candidate, true, nil
		}
		paths := []string{repo.Location}
		if aliasEligible && repo.ResolvedRepositoryPath != "" && repo.ResolvedRepositoryPath != repo.Location {
			paths = append(paths, repo.ResolvedRepositoryPath)
		}
		var selected models.Repository
		unavailableReason := database.AvailabilityReasonStorageMissing
		candidateUnavailable := func(candidateErr error) (string, bool) {
			if candidateErr == nil {
				return "", false
			}
			if errors.Is(callerContext.Err(), context.Canceled) || errors.Is(candidateErr, context.Canceled) {
				return "", false
			}
			if errors.Is(candidateErr, context.DeadlineExceeded) {
				return database.AvailabilityReasonObservationTimeout, true
			}
			var inspection *storageavailability.RepositoryStorageUnavailableError
			if errors.As(candidateErr, &inspection) {
				return inspection.ReasonCode, true
			}
			if os.IsNotExist(candidateErr) || os.IsPermission(candidateErr) {
				return database.AvailabilityReasonStorageMissing, true
			}
			return "", false
		}
		for _, path := range paths {
			candidate, matches, candidateErr := proveCandidate(path, true)
			if candidateErr != nil {
				if reason, unavailable := candidateUnavailable(candidateErr); unavailable {
					unavailableReason = reason
					if aliasEligible {
						continue
					}
					return models.Repository{}, &storageavailability.RepositoryStorageUnavailableError{ReasonCode: reason}
				}
				return models.Repository{}, candidateErr
			}
			if matches {
				selected = candidate
				controlPlaneProven = canProveAttachment
				break
			}
			if !aliasEligible {
				if _, statErr := os.Stat(path); os.IsNotExist(statErr) {
					return models.Repository{}, &storageavailability.RepositoryStorageUnavailableError{ReasonCode: database.AvailabilityReasonStorageMissing}
				}
				return models.Repository{}, fmt.Errorf("the native repository identity does not match this saved vault")
			}
		}
		if selected.ID == "" {
			candidates, candidateErr := storageavailability.RepositoryFallbackCandidates(ctx, repo)
			if candidateErr != nil {
				if reason, unavailable := candidateUnavailable(candidateErr); unavailable {
					return models.Repository{}, &storageavailability.RepositoryStorageUnavailableError{ReasonCode: reason}
				}
				return models.Repository{}, candidateErr
			}
			matches := []models.Repository{}
			for _, path := range candidates {
				if err := ctx.Err(); err != nil {
					if errors.Is(callerContext.Err(), context.Canceled) {
						return models.Repository{}, callerContext.Err()
					}
					break
				}
				candidate, match, candidateErr := proveCandidate(path, true)
				if candidateErr != nil {
					if reason, unavailable := candidateUnavailable(candidateErr); unavailable {
						unavailableReason = reason
						continue
					}
					return models.Repository{}, candidateErr
				}
				if match {
					matches = append(matches, candidate)
				}
			}
			switch len(matches) {
			case 0:
				return models.Repository{}, &storageavailability.RepositoryStorageUnavailableError{ReasonCode: unavailableReason}
			case 1:
				selected, controlPlaneProven = matches[0], canProveAttachment
			default:
				return models.Repository{}, fmt.Errorf("multiple mounted locations identify this managed vault")
			}
		}
		checkedAt := time.Now().UTC()
		resolution = &storageavailability.Resolution{Path: selected.Location, CheckedAt: checkedAt}
		repo = selected
		repo.ResolvedRepositoryObservedAt = checkedAt.Format(time.RFC3339Nano)
	}
	if controlPlaneRead && (strings.TrimSpace(repo.ProfileUUID) == "" || repo.AttachmentGeneration < 1) {
		return models.Repository{}, fmt.Errorf("saved vault admission identity is incomplete")
	}
	if passwordRecovery == nil && (strings.TrimSpace(repo.ProfileUUID) == "" || repo.AttachmentGeneration < 1) {
		var repaired models.Repository
		var repairErr error
		if resolution != nil {
			repaired, _, repairErr = profilebinding.RepairMissingFilesystemAfterIdentityProof(ctx, db, repo)
		} else {
			repaired, _, repairErr = profilebinding.RepairMissing(ctx, db, repo)
		}
		if repairErr != nil {
			return models.Repository{}, repairErr
		}
		repo = repaired
		if resolution != nil {
			repo.Location = resolution.Path
			repo.ResolvedRepositoryPath = resolution.Path
			repo.ResolvedRepositoryObservedAt = resolution.CheckedAt.Format(time.RFC3339Nano)
		}
	}
	if !models.ValidEngine(repo.Engine) || strings.TrimSpace(repo.NativeRepositoryID) == "" ||
		strings.TrimSpace(repo.ProfileUUID) == "" || repo.AttachmentGeneration < 1 ||
		strings.TrimSpace(repo.ClientUUID) == "" {
		return models.Repository{}, fmt.Errorf("saved vault admission identity is incomplete")
	}
	if controlPlaneProven {
		// Candidate selection already established this exact root and attachment.
	} else if options.AssertControlPlane != nil {
		if err := options.AssertControlPlane(ctx, repo); err != nil {
			return models.Repository{}, classifyKnownUnavailable(err)
		}
	} else {
		store := (vaultprofile.Store{Repository: repo}).
			WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
		if err := store.AssertRootIdentity(ctx); err != nil {
			return models.Repository{}, classifyKnownUnavailable(err)
		}
		if err := store.ForProfile(repo.ProfileUUID).
			AssertAttachment(ctx, repo.ClientUUID, repo.AttachmentGeneration); err != nil {
			return models.Repository{}, classifyKnownUnavailable(err)
		}
	}
	if controlPlaneRead {
		return repo, nil
	}
	if passwordRecovery != nil {
		selected, selectErr := passwordRecovery.SelectPassword(ctx, repo)
		if selectErr != nil {
			return models.Repository{}, selectErr
		}
		if err := models.ValidateVaultPassword(selected); err != nil {
			return models.Repository{}, err
		}
		repo.Passphrase = selected
	}
	validateNative := func() error {
		resolve := options.ResolveEngine
		if resolve == nil {
			resolve = func(repo models.Repository) (engines.Engine, error) {
				return engines.ResolveWithRepositoryAvailabilityCheck(repo, storageavailability.RequireRepositoryAvailable)
			}
		}
		engine, resolveErr := resolve(repo)
		if resolveErr != nil {
			return resolveErr
		}
		// The exact native repository proof is the same admission boundary for
		// Restic-rclone as for other Restic vaults. OAuth authorization already
		// discovered the provider namespace for the canonical address, and pinned
		// rclone owns its private config and token refresh. A separate provider
		// account probe here would only detect an out-of-flow same-user config
		// replacement; it would not strengthen the native identity or protected
		// sidecar proof already established for this attachment.
		validationOutput, validationErr := engines.ValidateRepository(ctx, engine, repo)
		if validationErr != nil {
			if errors.Is(callerContext.Err(), context.Canceled) || errors.Is(validationErr, context.Canceled) {
				return context.Canceled
			}
			if errors.Is(validationErr, context.DeadlineExceeded) {
				return &storageavailability.RepositoryStorageUnavailableError{ReasonCode: database.AvailabilityReasonObservationTimeout}
			}
			if engines.RepositoryMissing(repo, validationOutput) {
				return &storageavailability.RepositoryStorageUnavailableError{ReasonCode: database.AvailabilityReasonStorageMissing}
			}
			return fmt.Errorf("validate exact native repository: %w", validationErr)
		}
		fingerprint, fingerprintErr := engines.RepositoryFingerprint(repo, validationOutput)
		if repo.Engine == engines.ResticID && engines.IsResticRcloneConnector(repo.Connector) {
			// Restic's public config result preserves its native missing/error
			// classification above. The private read hashes the exact raw config
			// bytes without exposing them; its digest is the stored repository ID.
			fingerprint, fingerprintErr = engines.ResticRcloneRepositoryFingerprint(ctx, repo)
		}
		if fingerprintErr != nil || fingerprint != repo.NativeRepositoryID {
			if errors.Is(callerContext.Err(), context.Canceled) || errors.Is(fingerprintErr, context.Canceled) {
				return context.Canceled
			}
			// Synchronous filesystem reads can finish after the outer deadline with
			// conclusive identity evidence. Only a timeout from the fingerprint itself
			// is unavailable; an expired clock must not hide a mismatch or corruption.
			if errors.Is(fingerprintErr, context.DeadlineExceeded) {
				return errors.Join(&storageavailability.RepositoryStorageUnavailableError{
					ReasonCode: database.AvailabilityReasonObservationTimeout,
				}, fingerprintErr, ctx.Err())
			}
			if repo.Connector == "fs" {
				// Fingerprinting can lose the marker's ENOENT when it falls back to
				// validation output. Check only the exact selected root so disappearance
				// remains retryable without treating marker corruption as unplugged media.
				if _, rootErr := os.Stat(repo.Location); os.IsNotExist(rootErr) {
					return errors.Join(&storageavailability.RepositoryStorageUnavailableError{
						ReasonCode: database.AvailabilityReasonStorageMissing,
					}, fingerprintErr, rootErr)
				} else if rootErr != nil {
					return fmt.Errorf("recheck native repository root after fingerprint failure: %w", rootErr)
				}
			}
		}
		if fingerprintErr != nil {
			return fmt.Errorf("fingerprint exact native repository: %w", fingerprintErr)
		}
		if fingerprint != repo.NativeRepositoryID {
			return fmt.Errorf("the native repository identity does not match this saved vault")
		}
		return nil
	}
	if resolution != nil {
		if repo.Engine == engines.KopiaID {
			// The reconnect path performs isolated native path/repository/client
			// readback before activation and commits the alias only afterward.
			if err := ReconnectKopiaFilesystem(ctx, db, configured, resolution.Path, resolution.CheckedAt); err != nil {
				return models.Repository{}, classifyKnownUnavailable(err)
			}
		} else {
			if err := validateNative(); err != nil {
				return models.Repository{}, err
			}
			if err := database.SetResolvedRepositoryPath(db, repo.ID, configured.Location,
				resolution.Path, resolution.CheckedAt); err != nil {
				return models.Repository{}, err
			}
		}
		// The resolved path is deliberately copied last so no helper above can
		// replace it by reloading mutable cached-alias state.
		repo.Location = resolution.Path
		repo.ResolvedRepositoryPath = resolution.Path
	} else if err := validateNative(); err != nil {
		return models.Repository{}, err
	}
	if passwordRecovery != nil {
		repo.Passphrase = persisted.Passphrase
		repo.PendingPassphrase = persisted.PendingPassphrase
	}
	return repo, nil
}

func nativeIdentityMarkerPresent(repo models.Repository) (bool, error) {
	markers := []string{"config", "CONFIG"}
	if repo.Engine == engines.KopiaID {
		markers = []string{"kopia.repository.f", "kopia.blobcfg.f"}
	}
	for _, marker := range markers {
		info, err := os.Stat(filepath.Join(repo.Location, marker))
		if err == nil {
			// An observed wrong-type identity marker is malformed repository
			// evidence, unlike an uninspectable candidate path. Do not turn
			// that evidence into a skippable read error (or open a FIFO).
			if !info.Mode().IsRegular() {
				return false, fmt.Errorf("native repository identity marker is not a regular file")
			}
			return true, nil
		}
		if !os.IsNotExist(err) {
			return false, err
		}
	}
	return false, nil
}

// IsUnavailable preserves the typed availability boundary for public API and
// durable orchestration mappings without exposing native errors.
func IsUnavailable(err error) bool {
	var unavailable *storageavailability.RepositoryStorageUnavailableError
	return errors.As(err, &unavailable)
}
