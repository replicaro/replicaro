// Package repositoryadmission checks an already-saved vault, under its lock,
// before an operation uses it. It does not acquire coordinator or vault locks;
// callers keep their existing operation-family locking and pass the returned
// frozen view through the rest of that operation.
package repositoryadmission

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// PasswordChangeRecoveryOptions lets recovery of a password change started on
// this installation reuse normal filesystem resolution. It is the one
// exception, for that operation only, to the rule that a vault with an
// uncommitted password change can't be admitted.
type PasswordChangeRecoveryOptions struct {
	OperationUUID      string
	Phase              string
	AssertControlPlane func(context.Context, models.Repository) error
	SelectPassword     func(context.Context, models.Repository) (string, error)
	ResolveEngine      func(models.Repository) (engines.Engine, error)
}

// AdmitUnderLock reloads the saved vault row, resolves its configured
// filesystem location, verifies the protected control-plane record and,
// separately, the native repository ID, and returns one frozen runtime view.
// A later reload of the row may refresh policy/ownership but must not replace
// the returned path.
func AdmitUnderLock(ctx context.Context, db *sql.DB, requested models.Repository) (models.Repository, error) {
	return admitUnderLock(ctx, db, requested, Options{}, nil, false)
}

func AdmitUnderLockWithOptions(ctx context.Context, db *sql.DB, requested models.Repository, options Options) (models.Repository, error) {
	return admitUnderLock(ctx, db, requested, options, nil, false)
}

// AdmitControlPlaneReadUnderLock reloads and freezes the saved vault row for
// one bounded read of the protected sidecar. The supplied assertion is the
// only control-plane check for this read; unlike normal admission, this path
// deliberately skips backup-engine validation and native config activation.
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

// AdmitPasswordChangeRecoveryUnderLock admits only the uncommitted
// password-change operation saved in the local database, matched by UUID and
// phase. It returns one frozen repository path and restores the saved
// committed and pending passwords in memory.
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
		// A caller's copy of the location or binding may be stale; the
		// persisted row loaded above is what admission uses, so those fields
		// are deliberately not compared here.
		return models.Repository{}, fmt.Errorf("saved vault authority changed before admission")
	}
	repo := persisted
	var legacyFacts *storageidentity.Facts
	filesystemIdentityProven := false
	controlPlaneProven := false
	if repo.Connector == "fs" {
		// One deadline covers the work below that runs in another process and
		// honors the context: the storage probe (storage helper), the
		// protected root and attachment reads, and native validation. It is
		// kept on purpose so a share that answers the probe and then hangs in
		// one of those cannot hold the vault lock indefinitely. Expiry maps to
		// observation_timeout, which pauses a scheduled backup rather than
		// failing it. Backup/restore payload work gets its ordinary deadline
		// after admission returns.
		//
		// Known limitation, not a bug to "fix" by assuming the deadline
		// applies: the in-process reads here (the marker os.Stat calls in
		// nativeIdentityMarkerPresent, the marker read in
		// engines.RepositoryFingerprint, and the os.Stat rechecks of the
		// vault root) are plain filesystem calls that no context can
		// interrupt. A share that hangs inside one of them still blocks this
		// admission until the OS call returns. Moving them behind the storage
		// helper is separate work.
		resolutionContext, cancel := context.WithTimeout(ctx, time.Minute)
		defer cancel()
		ctx = resolutionContext
		// The vault runs only at its registered location; there is no cached
		// alias or mounted-candidate fallback. A vault that moved is reconnected
		// by the user through the connect/update flow.
		check, storageErr := admitRepositoryStorage(ctx, repo)
		if storageErr != nil {
			return models.Repository{}, classifyKnownUnavailable(storageErr)
		}
		if check.Legacy {
			// A vault saved by the descriptor-based code. Its binding is
			// converted only after the native repository ID, protected vault
			// UUID, and attachment proofs below succeed: a folder that merely
			// opens (for example an empty folder left behind by an unmounted
			// share, or a different repository) must not have its facts
			// recorded as this vault's. See convertLegacyBinding.
			legacyFacts = &check.Observed
		}
		canProveAttachment := strings.TrimSpace(repo.ProfileUUID) != "" && repo.AttachmentGeneration > 0
		present, markerErr := nativeIdentityMarkerPresent(repo)
		if markerErr != nil {
			if markerInspectionFailed(markerErr) {
				return models.Repository{}, &storageavailability.RepositoryStorageUnavailableError{ReasonCode: database.AvailabilityReasonStorageMissing}
			}
			return models.Repository{}, markerErr
		}
		identityMismatch := func() error {
			if _, statErr := os.Stat(repo.Location); os.IsNotExist(statErr) {
				return &storageavailability.RepositoryStorageUnavailableError{ReasonCode: database.AvailabilityReasonStorageMissing}
			}
			return fmt.Errorf("the native repository identity does not match this saved vault")
		}
		if !present {
			return models.Repository{}, identityMismatch()
		}
		fingerprint, fingerprintErr := engines.RepositoryFingerprint(repo, "")
		if fingerprintErr != nil {
			// Only raw local marker read errors mean "unavailable". Parse and
			// authority failures from the native or protected records are still
			// reported as real errors.
			if markerInspectionFailed(fingerprintErr) {
				return models.Repository{}, &storageavailability.RepositoryStorageUnavailableError{ReasonCode: database.AvailabilityReasonStorageMissing}
			}
			return models.Repository{}, fingerprintErr
		}
		if fingerprint != repo.NativeRepositoryID {
			return models.Repository{}, identityMismatch()
		}
		if canProveAttachment {
			if err := assertFilesystemControlPlane(ctx, repo, options); err != nil {
				if errors.Is(err, vaultprofile.ErrProtectedRootIdentityMismatch) {
					return models.Repository{}, identityMismatch()
				}
				return models.Repository{}, classifyKnownUnavailable(err)
			}
			controlPlaneProven = true
		}
		filesystemIdentityProven = true
		repo.ResolvedRepositoryPath = ""
		repo.ResolvedRepositoryObservedAt = ""
	}
	if controlPlaneRead && (strings.TrimSpace(repo.ProfileUUID) == "" || repo.AttachmentGeneration < 1) {
		return models.Repository{}, fmt.Errorf("saved vault admission identity is incomplete")
	}
	if passwordRecovery == nil && (strings.TrimSpace(repo.ProfileUUID) == "" || repo.AttachmentGeneration < 1) {
		var repaired models.Repository
		var repairErr error
		if filesystemIdentityProven {
			repaired, _, repairErr = profilebinding.RepairMissingFilesystemAfterIdentityProof(ctx, db, repo)
		} else {
			repaired, _, repairErr = profilebinding.RepairMissing(ctx, db, repo)
		}
		if repairErr != nil {
			return models.Repository{}, repairErr
		}
		repo = repaired
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
	if legacyFacts != nil {
		// Still under the caller's vault lock, and only now that the native
		// identity and the protected root and attachment are proven.
		converted, convertErr := convertLegacyBinding(db, repo, *legacyFacts)
		if convertErr != nil {
			return models.Repository{}, convertErr
		}
		repo = converted
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
		// Restic-rclone vaults get the same repository ID check as other Restic
		// vaults. OAuth authorization already found the provider namespace for the
		// canonical address, and pinned rclone manages its private config and token
		// refresh. A separate provider account probe here would only catch the same
		// user replacing the config outside Replicaro; it would add nothing to the
		// repository ID and sidecar checks already done for this attachment.
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
			// Synchronous filesystem reads can finish after the outer deadline with a
			// valid identity result. Only a timeout from the fingerprint itself counts
			// as unavailable; an expired deadline must not hide a mismatch or corruption.
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
	// Kopia filesystem vaults validate their native configuration here like
	// every other vault. Nothing reconnects Kopia to a relocated path
	// automatically; a native config still pointing at an old alias reports
	// its normal configuration error.
	if err := validateNative(); err != nil {
		return models.Repository{}, err
	}
	if passwordRecovery != nil {
		repo.Passphrase = persisted.Passphrase
		repo.PendingPassphrase = persisted.PendingPassphrase
	}
	return repo, nil
}

// convertLegacyBinding records the observed facts for a legacy vault
// binding once, after identity proof. The compare-and-swap refuses to
// overwrite a row changed some other way. Only the storage binding columns
// are taken from the rewritten row so the rest of the admitted view (for
// example a repaired attachment or recovery credentials) is kept.
func convertLegacyBinding(db *sql.DB, repo models.Repository, observed storageidentity.Facts) (models.Repository, error) {
	encoded, err := storageidentity.EncodeBinding(observed)
	if err != nil {
		return models.Repository{}, err
	}
	converted, err := database.ConvertLegacyRepositoryStorageBinding(db, repo, encoded)
	if err != nil {
		return models.Repository{}, err
	}
	if converted.Engine != repo.Engine || converted.Connector != repo.Connector ||
		converted.Location != repo.Location || converted.StorageIdentityVersion != storageidentity.BindingVersion {
		return models.Repository{}, fmt.Errorf("saved vault changed before admission")
	}
	repo.CanonicalIdentity = converted.CanonicalIdentity
	repo.StorageIdentityVersion = converted.StorageIdentityVersion
	repo.StorageIdentityKey = converted.StorageIdentityKey
	repo.StorageIdentityJSON = converted.StorageIdentityJSON
	return repo, nil
}

var admitRepositoryStorage = storageavailability.AdmitRepositoryStorage

// SetRepositoryStorageAdmissionForTests replaces the helper-backed storage
// decision in admission tests.
func SetRepositoryStorageAdmissionForTests(next func(context.Context, models.Repository) (storageavailability.Check, error)) func() {
	previous := admitRepositoryStorage
	admitRepositoryStorage = next
	return func() { admitRepositoryStorage = previous }
}

func assertFilesystemControlPlane(ctx context.Context, repo models.Repository, options Options) error {
	if options.AssertControlPlane != nil {
		return options.AssertControlPlane(ctx, repo)
	}
	store := (vaultprofile.Store{Repository: repo}).
		WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
	if err := store.AssertRootIdentity(ctx); err != nil {
		return err
	}
	return store.ForProfile(repo.ProfileUUID).AssertAttachment(ctx, repo.ClientUUID, repo.AttachmentGeneration)
}

// markerInspectionFailed reports whether a local read of the native identity
// marker failed after the storage probe succeeded. That means the vault became
// unreachable in between, so admission reports it as unavailable. Malformed or
// mismatching marker content is not an inspection failure.
//
// Access denied is deliberately excluded and stays an error: the storage is
// there and the user has to fix its permissions or share credentials, which
// is the same rule the probe applies to the folder itself. Counting it as
// unavailable would pause silently instead.
func markerInspectionFailed(err error) bool {
	if storageidentity.IsAccessDenied(err) {
		return false
	}
	var errno syscall.Errno
	return errors.Is(err, fs.ErrNotExist) || errors.As(err, &errno)
}

func nativeIdentityMarkerPresent(repo models.Repository) (bool, error) {
	markers := []string{"config", "CONFIG"}
	if repo.Engine == engines.KopiaID {
		markers = []string{"kopia.repository.f", "kopia.blobcfg.f"}
	}
	for _, marker := range markers {
		info, err := os.Stat(filepath.Join(repo.Location, marker))
		if err == nil {
			// An identity marker that exists but has the wrong file type means
			// the repository is malformed, unlike a path we couldn't inspect.
			// Don't turn it into a skippable read error (or open a FIFO).
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
