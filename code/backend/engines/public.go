package engines

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
)

type PasswordMutationDisposition string

const (
	PasswordMutationUnknown                PasswordMutationDisposition = "unknown"
	PasswordMutationRejectedBeforeMutation PasswordMutationDisposition = "rejected_before_mutation"
)

type PasswordChangeResult struct {
	Engine              string                      `json:"engine"`
	Status              RequestedOperationStatus    `json:"status"`
	ProcessStarted      bool                        `json:"processStarted,omitempty"`
	Output              string                      `json:"output,omitempty"`
	MutationDisposition PasswordMutationDisposition `json:"mutationDisposition,omitempty"`
}

type repositoryPasswordChanger interface {
	changeRepositoryPassword(context.Context, models.Repository, string, string, string) (string, error)
}

type repositoryPasswordProber interface {
	probeRepositoryPassword(context.Context, models.Repository) (string, error)
}

func validateNativePasswordInputPath(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(absolute)
	current := string(filepath.Separator)
	if volume != "" {
		current = volume + string(filepath.Separator)
	}
	relative := strings.TrimPrefix(absolute[len(volume):], string(filepath.Separator))
	components := strings.Split(relative, string(filepath.Separator))
	for index, component := range components {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || isReparsePoint(info) {
			return fmt.Errorf("native password input path is linked or reparse")
		}
		if index == len(components)-1 {
			if !info.Mode().IsRegular() {
				return fmt.Errorf("native password input has the wrong type")
			}
		} else if !info.IsDir() {
			return fmt.Errorf("native password input ancestor has the wrong type")
		}
	}
	return nil
}

// ChangeRepositoryPassword invokes only the selected engine's pinned native
// repository password command. The native result remains separate from
// protected-sidecar publication and local cleanup.
func ChangeRepositoryPassword(ctx context.Context, engine Engine, repo models.Repository, newPassword, operationUUID, nativeInputPath string) (PasswordChangeResult, error) {
	if err := models.ValidateVaultPassword(newPassword); err != nil {
		return PasswordChangeResult{}, err
	}
	parsedOperation, err := uuid.Parse(operationUUID)
	if err != nil || parsedOperation.String() != operationUUID {
		return PasswordChangeResult{}, fmt.Errorf("vault-password operation identity is invalid")
	}
	if repo.Engine == ResticID {
		parsedRepository, parseErr := uuid.Parse(repo.ID)
		root, rootErr := appdata.PasswordRotationRoot()
		expected := ""
		if rootErr == nil {
			expected = filepath.Join(root, repo.ID, operationUUID, "restic-new-password.txt")
		}
		cleanInput := filepath.Clean(nativeInputPath)
		pathErr := validateNativePasswordInputPath(expected)
		resolvedInput, resolveErr := filepath.EvalSymlinks(cleanInput)
		resolvedExpected, resolveExpectedErr := filepath.EvalSymlinks(expected)
		if parseErr != nil || parsedRepository.String() != repo.ID || rootErr != nil || cleanInput != expected ||
			pathErr != nil || resolveErr != nil || resolveExpectedErr != nil || resolvedInput != resolvedExpected {
			return PasswordChangeResult{}, fmt.Errorf("native password input does not match the exact password-change operation")
		}
		info, inspectErr := os.Lstat(expected)
		if inspectErr != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() != int64(len(newPassword)+1) {
			return PasswordChangeResult{}, fmt.Errorf("native password input is unavailable or unsafe")
		}
		file, openErr := os.Open(expected)
		if openErr != nil {
			return PasswordChangeResult{}, fmt.Errorf("native password input is unavailable or unsafe")
		}
		data, readErr := io.ReadAll(io.LimitReader(file, int64(len(newPassword)+2)))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || string(data) != newPassword+"\n" {
			return PasswordChangeResult{}, fmt.Errorf("native password input does not contain the exact admitted candidate")
		}
	} else if nativeInputPath != "" {
		return PasswordChangeResult{}, fmt.Errorf("native password input is not supported by %s", repo.Engine)
	}
	target := engine
	if public, ok := engine.(*publicEngine); ok {
		target = public.Engine
		ctx = repositoryProcessContext(ctx, repo, public.availabilityCheck)
	}
	changer, ok := target.(repositoryPasswordChanger)
	if !ok {
		return PasswordChangeResult{}, fmt.Errorf("%w: native vault-password change is unavailable", ErrUnsupported)
	}
	output, err := changer.changeRepositoryPassword(ctx, repo, newPassword, operationUUID, nativeInputPath)
	status, started, _, nativeErr, _, known := RequestedOperationOutcome(err)
	if err == nil {
		status, started, known = RequestedOperationSucceeded, true, true
	}
	if !known {
		return PasswordChangeResult{Engine: repo.Engine, Output: output}, err
	}
	disposition := PasswordMutationUnknown
	if repo.Engine == ResticID && TestedVersion(ResticID) == "0.19.1" &&
		status == RequestedOperationFailed && started {
		var exit *exec.ExitError
		if errors.As(nativeErr, &exit) && exit.ExitCode() == 11 {
			// Pinned Restic 0.19.1 defines exit 11 for key passwd as a lock
			// rejection before its key mutation. Keep the native failure intact;
			// this structured exit status only narrows recovery cleanup authority.
			disposition = PasswordMutationRejectedBeforeMutation
		}
	}
	return PasswordChangeResult{Engine: repo.Engine, Status: status, ProcessStarted: started,
		Output: output, MutationDisposition: disposition}, err
}

// ProbeRepositoryPassword validates one candidate against the exact stored
// native repository identity. Kopia uses a fresh isolated config/cache identity
// so cached format metadata cannot authenticate the candidate.
func ProbeRepositoryPassword(ctx context.Context, repo models.Repository, password string) error {
	if err := models.ValidateVaultPassword(password); err != nil {
		return err
	}
	candidate := repo
	candidate.Passphrase = password
	if repo.Engine == KopiaID {
		candidate.ID = uuid.NewString()
		defer CleanupPreviewArtifacts(candidate)
	}
	if repo.Engine == ResticID && IsResticRcloneConnector(repo.Connector) {
		fingerprint, err := ResticRcloneRepositoryFingerprint(ctx, candidate)
		if err != nil {
			return err
		}
		if fingerprint != repo.NativeRepositoryID {
			return fmt.Errorf("credential probe reached a different native repository")
		}
		return nil
	}
	engine, err := Resolve(candidate)
	if err != nil {
		return err
	}
	target := engine
	if public, ok := engine.(*publicEngine); ok {
		target = public.Engine
		ctx = repositoryProcessContext(ctx, candidate, public.availabilityCheck)
	}
	if prober, ok := target.(repositoryPasswordProber); ok && repo.Engine == KopiaID {
		fingerprint, err := prober.probeRepositoryPassword(ctx, candidate)
		if err != nil {
			return err
		}
		if fingerprint != repo.NativeRepositoryID {
			return fmt.Errorf("credential probe reached a different native repository")
		}
		return nil
	}
	output, err := ValidateRepository(ctx, engine, candidate)
	if err != nil {
		return err
	}
	fingerprint, err := RepositoryFingerprint(candidate, output)
	if err != nil || fingerprint != repo.NativeRepositoryID {
		return fmt.Errorf("credential probe reached a different native repository")
	}
	return nil
}

// ResticRcloneRepositoryFingerprint authenticates the supplied repository
// password and returns only a digest of Restic's exact raw config response.
// The opaque native response is never returned, logged, or persisted.
func ResticRcloneRepositoryFingerprint(ctx context.Context, repo models.Repository) (string, error) {
	if repo.Engine != ResticID || !IsResticRcloneConnector(repo.Connector) {
		return "", fmt.Errorf("%w: repository is not a Restic rclone connector", ErrUnsupported)
	}
	if err := models.ValidateVaultPassword(repo.Passphrase); err != nil {
		return "", err
	}
	engine, err := Resolve(repo)
	if err != nil {
		return "", err
	}
	target := engine
	if public, ok := engine.(*publicEngine); ok {
		target = public.Engine
		ctx = repositoryProcessContext(ctx, repo, public.availabilityCheck)
	}
	prober, ok := target.(repositoryPasswordProber)
	if !ok {
		return "", fmt.Errorf("exact Restic rclone password probe is unavailable")
	}
	fingerprint, err := prober.probeRepositoryPassword(ctx, repo)
	if err != nil {
		return "", err
	}
	return fingerprint, nil
}

// TestedVersion is the pinned version bundled and qualified by Replicaro. It is
// used for recovery metadata when an unrelated live version probe is temporarily
// unavailable.
func TestedVersion(id string) string {
	switch id {
	case ResticID:
		return "0.19.1"
	case KopiaID:
		return "0.23.1"
	case RcloneComponentID:
		return "1.75.1"
	default:
		return ""
	}
}

// EmbeddedBinarySHA256 returns the public component-metadata identity for an
// exact engine and release target without materializing or executing it.
func EmbeddedBinarySHA256(id, target string) (string, error) {
	return expectedEmbeddedHash(id, target)
}

// publicEngine binds repository availability checks and requested-operation
// classification around a concrete adapter. Ordinary output remains the
// selected engine's native content through this application boundary.
type publicEngine struct {
	Engine
	repo              models.Repository
	availabilityCheck RepositoryAvailabilityCheck
}

type resticMaintenancePruneAdmissionKey struct{}
type integrityCheckAdmissionKey struct{}
type nativeDeletionAdmissionKey struct{}
type sourceBackupAdmissionKey struct{}

// ContextWithSourceBackupAdmission installs the source-specific final check
// used only immediately before the requested Restic backup or Kopia snapshot
// create child, after repository preparation has completed.
func ContextWithSourceBackupAdmission(ctx context.Context, admit func(context.Context) error) context.Context {
	if admit == nil {
		return ctx
	}
	return context.WithValue(ctx, sourceBackupAdmissionKey{}, admit)
}

func admitSourceBackup(ctx context.Context) error {
	admit, _ := ctx.Value(sourceBackupAdmissionKey{}).(func(context.Context) error)
	if admit == nil {
		return nil
	}
	err := admit(ctx)
	if err == nil || command.IsPreProcessAdmission(err) {
		return err
	}
	return &command.PreProcessAdmissionError{Err: err}
}

// ContextWithNativeDeletionAdmission installs the narrow final-writer check
// after repository preparation and directly before native snapshot deletion or
// Restic forget. Both commands can remove snapshot history.
func ContextWithNativeDeletionAdmission(ctx context.Context, admit func(context.Context) error) context.Context {
	if admit == nil {
		return ctx
	}
	return context.WithValue(ctx, nativeDeletionAdmissionKey{}, admit)
}

func admitNativeDeletion(ctx context.Context) error {
	admit, _ := ctx.Value(nativeDeletionAdmissionKey{}).(func(context.Context) error)
	if admit == nil {
		return nil
	}
	err := admit(ctx)
	if err == nil || command.IsPreProcessAdmission(err) {
		return err
	}
	return &command.PreProcessAdmissionError{Err: err}
}

// ContextWithResticMaintenancePruneAdmission installs the one narrow
// application authorization check that must run after a successful optional
// plain Restic unlock and immediately before the requested prune command.
// It is intentionally specific to owner-authorized maintenance and is not a
// generalized process coordinator.
func ContextWithResticMaintenancePruneAdmission(
	ctx context.Context,
	admit func(context.Context) error,
) context.Context {
	if admit == nil {
		return ctx
	}
	return context.WithValue(ctx, resticMaintenancePruneAdmissionKey{}, admit)
}

func admitResticMaintenancePrune(ctx context.Context) error {
	admit, _ := ctx.Value(resticMaintenancePruneAdmissionKey{}).(func(context.Context) error)
	if admit == nil {
		return nil
	}
	return admit(ctx)
}

// ContextWithIntegrityCheckAdmission installs the final owner check used after
// engine-owned preparation and immediately before a native integrity child.
// Restic may unlock first; Kopia may connect or validate an operation config.
// Either prerequisite can take long enough for protected-root ownership to
// change while it runs.
func ContextWithIntegrityCheckAdmission(
	ctx context.Context,
	admit func(context.Context) error,
) context.Context {
	if admit == nil {
		return ctx
	}
	return context.WithValue(ctx, integrityCheckAdmissionKey{}, admit)
}

func admitIntegrityCheck(ctx context.Context) error {
	admit, _ := ctx.Value(integrityCheckAdmissionKey{}).(func(context.Context) error)
	if admit == nil {
		return nil
	}
	return admit(ctx)
}

type kopiaMaintenanceMutationAdmissionKey struct{}

// ContextWithKopiaMaintenanceMutationAdmission installs the final authority
// check for repository-wide maintenance settings. Kopia may connect or
// validate its operation config before the mutation, so callers provide a
// fresh owner/intent check that runs after those prerequisites.
func ContextWithKopiaMaintenanceMutationAdmission(ctx context.Context, admit func(context.Context) error) context.Context {
	if admit == nil {
		return ctx
	}
	return context.WithValue(ctx, kopiaMaintenanceMutationAdmissionKey{}, admit)
}

func admitKopiaMaintenanceMutation(ctx context.Context) error {
	admit, _ := ctx.Value(kopiaMaintenanceMutationAdmissionKey{}).(func(context.Context) error)
	if admit == nil {
		return fmt.Errorf("Kopia maintenance mutation lacks fresh authorization")
	}
	return admit(ctx)
}

type readOnlyRepositoryValidator interface {
	validateRepository(context.Context, models.Repository) (string, error)
}

func unlockResticRepository(ctx context.Context, engine Engine, repo models.Repository) (string, error) {
	restic, ok := engine.(*resticEngine)
	if !ok || repo.Engine != ResticID {
		return "", fmt.Errorf("%w: native Restic unlock requires a Restic repository", ErrUnsupported)
	}
	return restic.unlock(ctx, repo)
}

// UnlockResticRepository invokes only Restic's native plain unlock command.
// Restic owns stale-lock classification and any resulting repository mutation.
func UnlockResticRepository(ctx context.Context, engine Engine, repo models.Repository) (string, error) {
	if public, ok := engine.(*publicEngine); ok {
		ctx = repositoryProcessContext(ctx, repo, public.availabilityCheck)
		output, err := unlockResticRepository(ctx, public.Engine, repo)
		return output, err
	}
	output, err := unlockResticRepository(ctx, engine, repo)
	return output, err
}

func (e *publicEngine) autoUnlockRestic(ctx context.Context, repo models.Repository) error {
	if repo.Engine != ResticID || !repo.AutoUnlock {
		return nil
	}
	_, err := unlockResticRepository(ctx, e.Engine, repo)
	if err != nil {
		return fmt.Errorf("native Restic auto unlock failed: %w", err)
	}
	return nil
}

// ContextWithRepositoryAvailabilityCheck binds an explicit persisted-storage
// check to every native process launched with the returned context.
func ContextWithRepositoryAvailabilityCheck(
	ctx context.Context,
	repo models.Repository,
	check RepositoryAvailabilityCheck,
) context.Context {
	if check == nil {
		return ctx
	}
	return command.ContextWithBeforeProcess(ctx, func(checkContext context.Context) error {
		return check(checkContext, repo)
	})
}

func repositoryProcessContext(
	ctx context.Context,
	repo models.Repository,
	check RepositoryAvailabilityCheck,
) context.Context {
	return ContextWithRepositoryAvailabilityCheck(ctx, repo, check)
}

// ValidateRepository checks an existing repository without changing its
// repository-level settings. Engines without a specialized validation path
// already have a non-mutating Info implementation.
func ValidateRepository(ctx context.Context, engine Engine, repo models.Repository) (string, error) {
	if public, ok := engine.(*publicEngine); ok {
		ctx = repositoryProcessContext(ctx, repo, public.availabilityCheck)
		if validator, ok := public.Engine.(readOnlyRepositoryValidator); ok {
			output, err := validator.validateRepository(ctx, repo)
			return output, err
		}
	}
	return engine.Info(ctx, repo)
}

func (e *publicEngine) Info(ctx context.Context, repo models.Repository) (string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	output, err := e.Engine.Info(ctx, repo)
	return output, err
}

func (e *publicEngine) Create(ctx context.Context, repo models.Repository) (string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	output, err := e.Engine.Create(ctx, repo)
	return output, err
}

func (e *publicEngine) Backup(ctx context.Context, repo models.Repository, source string, options BackupOptions) (models.Snapshot, string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	if err := e.autoUnlockRestic(ctx, repo); err != nil {
		return models.Snapshot{}, "", resticPreparationFailure(err)
	}
	snapshot, output, err := e.Engine.Backup(ctx, repo, source, options)
	if repo.Engine == ResticID && !IsBackupSourceReadFailure(err) {
		err = resticRequestedOperationError(err)
	}
	return snapshot, output, err
}

func (e *publicEngine) ListSnapshots(ctx context.Context, repo models.Repository) ([]models.Snapshot, string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	snapshots, output, err := e.Engine.ListSnapshots(ctx, repo)
	return snapshots, output, err
}

// ListSnapshotsFresh preserves the optional cache-bypassing listing contract
// through the public repository-checking boundary.
func (e *publicEngine) ListSnapshotsFresh(ctx context.Context, repo models.Repository) ([]models.Snapshot, string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	snapshots, output, err := ListSnapshotsFresh(ctx, e.Engine, repo)
	return snapshots, output, err
}

func (e *publicEngine) ListPath(ctx context.Context, repo models.Repository, snapshotID, path string) ([]models.SnapshotEntry, string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	entries, output, err := e.Engine.ListPath(ctx, repo, snapshotID, path)
	return entries, output, err
}

func (e *publicEngine) ListPathRecursive(ctx context.Context, repo models.Repository, snapshotID, path string) ([]models.SnapshotEntry, string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	entries, output, err := e.Engine.ListPathRecursive(ctx, repo, snapshotID, path)
	return entries, output, err
}

type publicSnapshotIndexSession struct {
	SnapshotIndexSession
	repo              models.Repository
	availabilityCheck RepositoryAvailabilityCheck
}

func (s *publicSnapshotIndexSession) ListPathRecursive(ctx context.Context, snapshot SnapshotIndexItem, path string) ([]models.SnapshotEntry, string, error) {
	ctx = repositoryProcessContext(ctx, s.repo, s.availabilityCheck)
	entries, output, err := s.SnapshotIndexSession.ListPathRecursive(ctx, snapshot, path)
	return entries, output, err
}

func (s *publicSnapshotIndexSession) Close() error {
	return s.SnapshotIndexSession.Close()
}

// BeginSnapshotIndex preserves the optional indexing contract across the
// public repository-checking wrapper without making unsupported engines
// advertise it.
func BeginSnapshotIndex(ctx context.Context, engine Engine, repo models.Repository) (SnapshotIndexSession, string, bool, error) {
	target := engine
	var availabilityCheck RepositoryAvailabilityCheck
	if public, ok := engine.(*publicEngine); ok {
		target = public.Engine
		availabilityCheck = public.availabilityCheck
		ctx = repositoryProcessContext(ctx, repo, public.availabilityCheck)
	}
	indexer, ok := target.(SnapshotIndexEngine)
	if !ok {
		return nil, "", false, nil
	}
	session, output, err := indexer.BeginSnapshotIndex(ctx, repo)
	if err != nil {
		return nil, output, true, err
	}
	if _, checked := engine.(*publicEngine); checked {
		session = &publicSnapshotIndexSession{
			SnapshotIndexSession: session,
			repo:                 repo,
			availabilityCheck:    availabilityCheck,
		}
	}
	return session, output, true, nil
}

func (e *publicEngine) Restore(ctx context.Context, repo models.Repository, snapshotID string, options RestoreOptions) (string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	if err := e.autoUnlockRestic(ctx, repo); err != nil {
		return "", resticPreparationFailure(err)
	}
	output, err := e.Engine.Restore(ctx, repo, snapshotID, options)
	if repo.Engine == ResticID {
		err = resticRequestedOperationError(err)
	}
	return output, err
}

func (e *publicEngine) DeleteSnapshot(ctx context.Context, repo models.Repository, snapshotID string) (string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	if err := e.autoUnlockRestic(ctx, repo); err != nil {
		return "", resticPreparationFailure(err)
	}
	output, err := e.Engine.DeleteSnapshot(ctx, repo, snapshotID)
	if repo.Engine == ResticID {
		err = resticRequestedOperationError(err)
	}
	return output, err
}

func (e *publicEngine) DeleteSnapshots(ctx context.Context, repo models.Repository, snapshotIDs []string) (string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	if err := e.autoUnlockRestic(ctx, repo); err != nil {
		return "", resticPreparationFailure(err)
	}
	if batch, ok := e.Engine.(batchDeletionEngine); ok {
		output, err := batch.DeleteSnapshots(ctx, repo, snapshotIDs)
		if repo.Engine == ResticID {
			err = resticRequestedOperationError(err)
		}
		return output, err
	}
	if len(snapshotIDs) != 1 {
		return "", fmt.Errorf("%w: adapter does not expose native batch deletion", ErrUnsupported)
	}
	return e.DeleteSnapshot(ctx, repo, snapshotIDs[0])
}

func (e *publicEngine) Check(ctx context.Context, repo models.Repository, snapshotID string) (string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	if err := e.autoUnlockRestic(ctx, repo); err != nil {
		return "", resticPreparationFailure(err)
	}
	output, err := e.Engine.Check(ctx, repo, snapshotID)
	if repo.Engine == ResticID {
		err = resticRequestedOperationError(err)
	}
	return output, err
}

func (e *publicEngine) Maintenance(ctx context.Context, repo models.Repository) (string, error) {
	ctx = repositoryProcessContext(ctx, repo, e.availabilityCheck)
	if err := e.autoUnlockRestic(ctx, repo); err != nil {
		return "", resticPreparationFailure(err)
	}
	output, err := e.Engine.Maintenance(ctx, repo)
	if repo.Engine == ResticID {
		err = resticRequestedOperationError(err)
	}
	return output, err
}
