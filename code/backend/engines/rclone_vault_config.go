package engines

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
)

var runRcloneVaultConfigCommand = command.RunWithInputSecretsPrivateOutput
var syncRcloneVaultPublicationDirectory func(string) error
var rcloneVaultPublicationAfterOpenForTest func(string) error
var rcloneVaultPublicationBeforeCreateForTest func(string) error
var rcloneVaultPublicationAfterRenameForTest func(string) error
var rcloneVaultRemovalBeforeMutationForTest func() error

const maximumRcloneVaultConfigBytes = 8 << 20

type rcloneVaultConfigGuard struct {
	path    string
	inspect func() error
	close   func() error
}

// RcloneVaultConfigBinding selects one ordinary canonical private config path.
// Revalidate reopens that path; it never retains a file or directory across a
// native child lifetime.
type RcloneVaultConfigBinding struct {
	path       string
	revalidate func(context.Context) error
}

func (binding *RcloneVaultConfigBinding) Path() string {
	if binding == nil {
		return ""
	}
	return binding.path
}

func (binding *RcloneVaultConfigBinding) NativePath() string {
	return binding.Path()
}

func (binding *RcloneVaultConfigBinding) Close() error {
	return nil
}

func (binding *RcloneVaultConfigBinding) Revalidate(ctx context.Context) error {
	if binding == nil || binding.revalidate == nil {
		return nil
	}
	return binding.revalidate(ctx)
}

type rcloneVaultPublication struct {
	target        string
	syncDirectory func() error
	inspectTarget func() error
	close         func() error
	activate      func(string) (RcloneConfigDisposition, error)
}

func SetRcloneVaultConfigRunnerForTests(next func(
	context.Context, string, []string, []string, string, time.Duration, string,
) (string, error)) func() {
	previous := runRcloneVaultConfigCommand
	runRcloneVaultConfigCommand = next
	return func() {
		runRcloneVaultConfigCommand = previous
	}
}

func validRepositoryUUID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed.String() == id
}

// RcloneVaultConfigPath resolves the only durable config path for a repository.
func RcloneVaultConfigPath(repositoryID string) (string, error) {
	if !validRepositoryUUID(repositoryID) {
		return "", fmt.Errorf("rclone vault repository ID must be a canonical UUID")
	}
	root, err := appdata.RcloneVaultConfigRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, repositoryID, "rclone.conf"), nil
}

func rcloneConfigPathForRepository(repo models.Repository) (string, error) {
	if repo.RcloneConfigPath != "" {
		absolute, err := filepath.Abs(repo.RcloneConfigPath)
		if err != nil {
			return "", err
		}
		if absolute != filepath.Clean(repo.RcloneConfigPath) {
			return "", invalidRcloneLocalBinding(fmt.Errorf("staged rclone config path is not canonical"))
		}
		return absolute, nil
	}
	return RcloneVaultConfigPath(repo.ID)
}

func prepareRcloneVaultDirectory(repositoryID string) (string, error) {
	return prepareRcloneVaultDirectorySecure(repositoryID)
}

func inspectBoundedRcloneVaultConfig(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return invalidRcloneLocalBinding(fmt.Errorf("rclone vault config has the wrong type"))
	}
	if info.Size() < 0 || info.Size() > maximumRcloneVaultConfigBytes {
		return fmt.Errorf("rclone vault config is oversized")
	}
	return nil
}

func localRcloneBindingFailure(message string, err error) error {
	result := fmt.Errorf("%s: %w", message, err)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, errInvalidRcloneLocalBinding) {
		return reconnectRequired(result)
	}
	return result
}

func validateRcloneVaultFile(filename string) error {
	file, err := openRcloneConfigFileReadOnly(filename)
	if err != nil {
		return err
	}
	return errors.Join(inspectBoundedRcloneVaultConfig(file), file.Close())
}

func privateRcloneOperationDirectories() (root, cache, temporary string, cleanup func() error, err error) {
	parent, err := appdata.RcloneSessionRoot()
	if err != nil {
		return "", "", "", nil, err
	}
	if err := prepareRcloneSessionRoot(parent); err != nil {
		return "", "", "", nil, err
	}
	root, err = os.MkdirTemp(parent, "op-")
	if err != nil {
		return "", "", "", nil, err
	}
	cleanup = func() error { return removeRcloneSession(root) }
	cache, temporary = filepath.Join(root, "cache"), filepath.Join(root, "tmp")
	for _, directory := range []string{root, cache, temporary} {
		if directory != root {
			if err := os.Mkdir(directory, 0o700); err != nil {
				return "", "", "", cleanup, errors.Join(err, cleanup())
			}
		}
		if err := appdata.SecurePath(directory, true); err != nil {
			return "", "", "", cleanup, errors.Join(err, cleanup())
		}
	}
	return root, cache, temporary, cleanup, nil
}

// ValidateRcloneVaultConfig verifies the selected private file and its local
// path safeguards. OAuth authorization discovers the account, drive, or
// namespace needed for the canonical physical address once. Pinned rclone then
// owns this opaque file and its token refresh; Replicaro does not reparse its
// credential fields. Every attached vault is still admitted by the selected
// engine's native repository identity and the protected sidecar. Repeating
// provider probes on each command would chiefly police a same-OS-user manual
// config replacement or reauthorization outside this workflow, rather than
// strengthen those vault proofs. Ordinary engine orchestration and exact
// private-path safety are the intended boundary for attached operations.
func ValidateRcloneVaultConfig(ctx context.Context, repo models.Repository) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	binding, err := openRcloneVaultConfigBindingLocal(repo)
	if err != nil {
		return err
	}
	return binding.Close()
}

func openRcloneVaultConfigBindingLocal(repo models.Repository) (*RcloneVaultConfigBinding, error) {
	if repo.Engine != ResticID || !IsResticRcloneConnector(repo.Connector) {
		return nil, fmt.Errorf("%w: repository is not a Restic rclone connector", ErrUnsupported)
	}
	if _, ok := RcloneProvider(repo.Connector); !ok {
		return nil, fmt.Errorf("%w: repository is not a Restic rclone connector", ErrUnsupported)
	}
	if _, err := NormalizeRcloneProviderOptions(repo.Connector, repo.ConnectorOptions); err != nil {
		return nil, reconnectRequired(fmt.Errorf("rclone vault identity is invalid: %w", err))
	}
	config, err := rcloneConfigPathForRepository(repo)
	if err != nil {
		return nil, localRcloneBindingFailure("rclone vault requires native reconnect", err)
	}
	if repo.RcloneConfigPath == "" {
		expected, pathErr := RcloneVaultConfigPath(repo.ID)
		if pathErr != nil || config != expected {
			return nil, reconnectRequired(fmt.Errorf("rclone vault config binding is invalid"))
		}
	}
	var guard *rcloneVaultConfigGuard
	if repo.RcloneConfigPath == "" {
		guard, err = openRcloneVaultConfigGuard(repo.ID)
	} else {
		guard, err = openStagedRcloneConfigGuard(config)
	}
	if err != nil {
		return nil, localRcloneBindingFailure("rclone vault requires native reconnect", err)
	}
	defer guard.close()
	if err := guard.inspect(); err != nil {
		return nil, localRcloneBindingFailure("rclone vault requires native reconnect", err)
	}
	return &RcloneVaultConfigBinding{
		path: guard.path,
		revalidate: func(context.Context) error {
			// Reopen after native access: rclone may atomically replace the
			// file during token refresh, so no inode or handle is retained.
			var nextGuard *rcloneVaultConfigGuard
			var err error
			if repo.RcloneConfigPath == "" {
				nextGuard, err = openRcloneVaultConfigGuard(repo.ID)
			} else {
				nextGuard, err = openStagedRcloneConfigGuard(config)
			}
			if err != nil {
				return localRcloneBindingFailure("rclone vault requires native reconnect", err)
			}
			return errors.Join(nextGuard.inspect(), nextGuard.close())
		},
	}, nil
}

// OpenRcloneVaultConfigBinding locally admits a Restic-rclone config and selects
// its ordinary canonical path. Revalidate checks local path safety immediately
// before or after use. Native repository and sidecar proofs establish the vault
// identity; a provider listing here would not prove those objects. The binding
// retains no file or directory.
func OpenRcloneVaultConfigBinding(
	ctx context.Context,
	repo models.Repository,
) (*RcloneVaultConfigBinding, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return openRcloneVaultConfigBindingLocal(repo)
}

// RcloneVaultCredentialsReady reports the narrow attached-vault readiness
// contract: complete non-secret address identity plus one secure private native
// config file. Provider reachability remains operation-authoritative.
func RcloneVaultCredentialsReady(ctx context.Context, repo models.Repository) bool {
	if repo.Engine != ResticID || !IsResticRcloneConnector(repo.Connector) {
		return false
	}
	normalized, err := NormalizeRcloneProviderOptions(
		repo.Connector, repo.ConnectorOptions,
	)
	if err != nil {
		return false
	}
	provider, _ := RcloneProvider(repo.Connector)
	for field := range provider.SecretFields {
		if normalized[field] != "" {
			return false
		}
	}
	_ = ctx
	guard, err := openRcloneVaultConfigGuard(repo.ID)
	if err != nil {
		return false
	}
	defer guard.close()
	return guard.inspect() == nil
}

func promoteRcloneVaultConfig(
	ctx context.Context,
	binary, stagedConfig, repositoryID string,
) (result RcloneConfigActivation, err error) {
	_ = ctx
	_ = binary
	target, pathErr := RcloneVaultConfigPath(repositoryID)
	result = retainedRcloneConfig(target)
	if pathErr != nil {
		return result, pathErr
	}
	if err := validateRcloneVaultFile(stagedConfig); err != nil {
		return result, fmt.Errorf("staged rclone config is unsafe: %w", err)
	}
	publication, err := newRcloneVaultPublication(repositoryID)
	if err != nil {
		return result, fmt.Errorf("prepare rclone config publication: %w", err)
	}
	defer func() { err = errors.Join(err, publication.close()) }()
	disposition, activateErr := publication.activate(stagedConfig)
	result = RcloneConfigActivation{Path: publication.target, Disposition: disposition}
	if activateErr != nil {
		return result, fmt.Errorf("activate rclone vault config: %w", activateErr)
	}
	if hook := rcloneVaultPublicationAfterRenameForTest; hook != nil {
		if err := hook(filepath.Dir(publication.target)); err != nil {
			return result, err
		}
	}
	if syncRcloneVaultPublicationDirectory != nil {
		err = syncRcloneVaultPublicationDirectory(filepath.Dir(publication.target))
	} else {
		err = publication.syncDirectory()
	}
	if err != nil {
		return result, fmt.Errorf("sync rclone vault directory: %w", err)
	}
	if err := publication.inspectTarget(); err != nil {
		return result, err
	}
	return result, nil
}

// EnsureRcloneSidecarConfig returns the vault-owned opaque native config.
// Restic-rclone sidecars reuse their exact private repository config.
func EnsureRcloneSidecarConfig(ctx context.Context, repo models.Repository, binary string) (string, error) {
	if IsResticRcloneConnector(repo.Connector) {
		if err := ValidateRcloneVaultConfig(ctx, repo); err != nil {
			return "", err
		}
		return rcloneConfigPathForRepository(repo)
	}
	if repo.RcloneConfigPath != "" {
		if err := validateRcloneVaultFile(repo.RcloneConfigPath); err != nil {
			return "", err
		}
		return repo.RcloneConfigPath, nil
	}
	binding, err := openNonResticRcloneSidecarConfigBinding(ctx, repo, binary)
	if err != nil {
		return "", err
	}
	path := binding.Path()
	if err := binding.Close(); err != nil {
		return "", err
	}
	return path, nil
}

func openNonResticRcloneSidecarConfigBinding(
	ctx context.Context,
	repo models.Repository,
	binary string,
) (*RcloneVaultConfigBinding, error) {
	config, err := RcloneVaultConfigPath(repo.ID)
	if err != nil {
		return nil, err
	}
	guard, openErr := openRcloneVaultConfigGuard(repo.ID)
	if openErr == nil {
		inspectErr := guard.inspect()
		closeErr := guard.close()
		if err := errors.Join(inspectErr, closeErr); err != nil {
			return nil, err
		}
		return nonResticRcloneSidecarBinding(repo.ID, config), nil
	} else if !errors.Is(openErr, os.ErrNotExist) {
		return nil, openErr
	}
	config, err = prepareRcloneVaultDirectory(repo.ID)
	if err != nil {
		return nil, err
	}
	_, cache, temporary, cleanup, err := privateRcloneOperationDirectories()
	if err != nil {
		return nil, err
	}
	defer func() { _ = cleanup() }()
	binding, err := touchRcloneVaultSidecarConfig(
		ctx, binary, repo.ID, config, cache, temporary,
	)
	if err != nil {
		return nil, fmt.Errorf("native rclone could not establish the vault sidecar config")
	}
	return binding, nil
}

func nonResticRcloneSidecarBinding(
	repositoryID, config string,
) *RcloneVaultConfigBinding {
	return &RcloneVaultConfigBinding{
		path: config,
		revalidate: func(context.Context) error {
			guard, err := openRcloneVaultConfigGuard(repositoryID)
			if err != nil {
				return err
			}
			return errors.Join(guard.inspect(), guard.close())
		},
	}
}

// OpenRcloneSidecarConfigBinding establishes the existing vault-specific
// sidecar config at its ordinary canonical path without retaining an opened
// file or directory across a native command.
func OpenRcloneSidecarConfigBinding(
	ctx context.Context,
	repo models.Repository,
	binary string,
) (*RcloneVaultConfigBinding, error) {
	if IsResticRcloneConnector(repo.Connector) {
		return OpenRcloneVaultConfigBinding(ctx, repo)
	}
	if repo.RcloneConfigPath != "" {
		return nil, fmt.Errorf("staged non-Restic sidecar config cannot select an attached binding")
	}
	return openNonResticRcloneSidecarConfigBinding(ctx, repo, binary)
}

// OpenExistingRcloneStatisticsConfigBinding admits only an already-existing
// attached vault config. Unlike the sidecar binding it never creates a vault
// directory, touches a config, or initializes any remote. OAuth repositories
// retain their exact private config path and local safety checks.
func OpenExistingRcloneStatisticsConfigBinding(
	ctx context.Context,
	repo models.Repository,
) (*RcloneVaultConfigBinding, error) {
	if IsResticRcloneConnector(repo.Connector) {
		return OpenRcloneVaultConfigBinding(ctx, repo)
	}
	if repo.RcloneConfigPath != "" {
		return nil, fmt.Errorf("staged non-Restic config cannot select an attached statistics binding")
	}
	config, err := RcloneVaultConfigPath(repo.ID)
	if err != nil {
		return nil, err
	}
	guard, err := openRcloneVaultConfigGuard(repo.ID)
	if err != nil {
		return nil, err
	}
	if err := errors.Join(guard.inspect(), guard.close()); err != nil {
		return nil, err
	}
	return nonResticRcloneSidecarBinding(repo.ID, config), nil
}

// NewRclonePreviewSidecarConfig creates a native-touched operation-owned opaque
// config for pre-attachment inspection. Random preview IDs never select a
// durable vault path.
func NewRclonePreviewSidecarConfig(
	ctx context.Context,
) (string, func() error, error) {
	binary, err := rcloneBinary(pathFor(RcloneComponentID))
	if err != nil {
		return "", nil, err
	}
	root, cache, temporary, cleanup, err := privateRcloneOperationDirectories()
	if err != nil {
		return "", nil, err
	}
	config := filepath.Join(root, "rclone.conf")
	if _, err := runRcloneVaultConfigCommand(
		ctx, binary,
		[]string{"config", "touch", "--config", config, "--cache-dir", cache,
			"--temp-dir", temporary, "--log-level", "ERROR"},
		nil, "", time.Minute, RcloneComponentID,
	); err != nil {
		return "", cleanup, errors.Join(
			fmt.Errorf("native rclone could not establish preview config"),
			cleanup(),
		)
	}
	if err := hardenNewRcloneConfigFile(config); err != nil {
		return "", cleanup, errors.Join(err, cleanup())
	}
	if err := validateRcloneVaultFile(config); err != nil {
		return "", cleanup, errors.Join(err, cleanup())
	}
	return config, cleanup, nil
}

// RemoveRcloneVaultConfig removes only one validated repository-ID directory.
func RemoveRcloneVaultConfig(repositoryID string) error {
	if !validRepositoryUUID(repositoryID) {
		return fmt.Errorf("rclone vault repository ID must be a canonical UUID")
	}
	return removeRcloneVaultConfigSecure(repositoryID)
}

// ReconcileRcloneVaultConfigs removes only exact, otherwise-empty UUID config
// directories with no attached or workflow owner. Unknown or ambiguous
// entries are deliberately untouched.
func ReconcileRcloneVaultConfigs(ownerIDs map[string]bool) error {
	root, err := appdata.RcloneVaultConfigRoot()
	if err != nil {
		return err
	}
	if _, err := os.Lstat(root); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	return reconcileRcloneVaultConfigsSecure(ownerIDs)
}
