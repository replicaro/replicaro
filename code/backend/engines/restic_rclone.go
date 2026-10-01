package engines

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/vaultidentity"
)

type resticRcloneSession struct {
	repository string
	args       []string
	env        []string
	root       string
	config     string
	binding    *RcloneVaultConfigBinding
	ctx        context.Context
	cleanup    func() error
}

type ResticOperationStatus string

const (
	ResticOperationNotStarted  ResticOperationStatus = "not_started"
	ResticOperationSucceeded   ResticOperationStatus = "succeeded"
	ResticOperationFailed      ResticOperationStatus = "failed"
	ResticOperationInterrupted ResticOperationStatus = "interrupted"
)

// ResticOperationFailure describes only the Restic operation requested at the
// public engine boundary. Unlock, snapshot lookup, and local configuration
// preparation stay internal; if one blocks the requested command, the outcome
// is simply not_started (or interrupted). A successful requested command may
// still carry separate output-processing and local cleanup errors. Provider
// identity checks are not part of an attached command's result.
type ResticOperationFailure struct {
	Status                ResticOperationStatus
	ProcessStarted        bool
	PreparationError      error
	NativeError           error
	OutputProcessingError error
	CleanupError          error
}

func (e *ResticOperationFailure) Error() string {
	var message string
	switch e.Status {
	case ResticOperationNotStarted:
		var userSafe *userSafeConnectorError
		if errors.As(e.PreparationError, &userSafe) {
			return userSafe.Error()
		}
		return "native Restic preparation failed; requested native operation was not started"
	case ResticOperationInterrupted:
		if e.ProcessStarted {
			message = "native Restic operation was interrupted; completion is unknown"
			break
		}
		return "native Restic operation was interrupted before native process start"
	case ResticOperationFailed:
		if e.NativeError != nil {
			message = e.NativeError.Error()
			break
		}
		message = "native Restic operation failed"
	case ResticOperationSucceeded:
		if e.OutputProcessingError != nil {
			message := "native Restic operation succeeded but output processing failed: " + e.OutputProcessingError.Error()
			if e.CleanupError != nil {
				// Some callers persist only this aggregate text. Keep both known
				// follow-up failures visible without exposing the private config path.
				return message + "; local rclone session cleanup failed"
			}
			return message
		}
		if e.CleanupError != nil {
			return "native Restic operation succeeded but local cleanup failed"
		}
	}
	if message != "" {
		// The native child keeps its own typed result and output. Aggregate
		// presentation also needs to disclose a failed local follow-up, without
		// exposing its path or credential-bearing cause.
		if e.CleanupError != nil {
			return message + "; local rclone session cleanup failed"
		}
		return message
	}
	return "native Restic operation outcome is incomplete"
}

func (e *ResticOperationFailure) Unwrap() []error {
	result := make([]error, 0, 4)
	if e.PreparationError != nil {
		result = append(result, e.PreparationError)
	}
	if e.NativeError != nil {
		result = append(result, e.NativeError)
	}
	if e.OutputProcessingError != nil {
		result = append(result, e.OutputProcessingError)
	}
	if e.CleanupError != nil {
		result = append(result, e.CleanupError)
	}
	return result
}

// ResticRequestedOperationOutcome returns the one requested-command result.
// Output remains the ordinary method return value and is never taken from an
// internal prerequisite command.
func ResticRequestedOperationOutcome(err error) (
	status ResticOperationStatus,
	processStarted bool,
	preparationErr, nativeErr error,
	known bool,
) {
	if err == nil {
		return ResticOperationSucceeded, true, nil, nil, true
	}
	var failure *ResticOperationFailure
	if !errors.As(err, &failure) {
		return "", false, nil, nil, false
	}
	return failure.Status, failure.ProcessStarted, failure.PreparationError,
		failure.NativeError, true
}

func resticPreparationFailure(err error) error {
	if err == nil {
		return nil
	}
	status := ResticOperationNotStarted
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = ResticOperationInterrupted
	}
	var userSafe *userSafeConnectorError
	if errors.As(err, &userSafe) {
		return &ResticOperationFailure{Status: status, PreparationError: userSafe}
	}
	return &ResticOperationFailure{
		Status: status, PreparationError: err,
	}
}

func resticRequestedOperationError(err error) error {
	if err == nil {
		return nil
	}
	var failure *ResticOperationFailure
	if errors.As(err, &failure) {
		return failure
	}
	return resticPreparationFailure(err)
}

func resticCommandFailure(nativeErr error, processStarted bool) error {
	var outputErr error
	if nativeCause, outputCause := command.FailureCauses(nativeErr); outputCause != nil {
		outputErr = outputCause
		if nativeCause == nil {
			nativeErr = nil
		} else {
			// Keep the adapter's classification (such as the cold-storage guidance)
			// when the native error is split from an output-capture failure that
			// happened at the same time. Otherwise callers lose the user guidance
			// even though the Restic command failure itself is still known.
			if errors.Is(nativeErr, ErrColdStorageArchivedObject) {
				nativeCause = errors.Join(ErrColdStorageArchivedObject, nativeCause)
			}
			nativeErr = separateNativeProcessError(nativeErr.Error(), nativeCause, outputCause)
		}
	}
	if nativeErr == nil && outputErr == nil {
		return nil
	}
	if !processStarted {
		return resticPreparationFailure(errors.Join(nativeErr, outputErr))
	}
	status := ResticOperationSucceeded
	if nativeErr != nil {
		status = ResticOperationFailed
		if errors.Is(nativeErr, context.Canceled) || errors.Is(nativeErr, context.DeadlineExceeded) {
			status = ResticOperationInterrupted
		}
	}
	return &ResticOperationFailure{
		Status: status, ProcessStarted: true, NativeError: nativeErr,
		OutputProcessingError: outputErr,
	}
}

func resticCommandCleanupFailure(nativeErr, cleanupErr error, processStarted bool) error {
	result := resticCommandFailure(nativeErr, processStarted)
	if cleanupErr == nil {
		return result
	}
	if result == nil {
		return &ResticOperationFailure{
			Status: ResticOperationSucceeded, ProcessStarted: processStarted,
			CleanupError: cleanupErr,
		}
	}
	var failure *ResticOperationFailure
	if errors.As(result, &failure) {
		copy := *failure
		copy.CleanupError = cleanupErr
		return &copy
	}
	return errors.Join(result, cleanupErr)
}

// privateOutputError publishes a fixed message for private configuration and
// authorization work while retaining error classification for control flow.
// Ordinary Restic and Kopia command failures do not use this boundary.
type privateOutputError struct {
	message string
	cause   error
}

func (e *privateOutputError) Error() string { return e.message }
func (e *privateOutputError) Is(target error) bool {
	return errors.Is(e.cause, target)
}
func (e *privateOutputError) As(target any) bool {
	return errors.As(e.cause, target)
}

func newResticRcloneSession(
	ctx context.Context,
	repo models.Repository,
) (*resticRcloneSession, error) {
	if !SupportsConnector(repo.Engine, repo.Connector) {
		return nil, fmt.Errorf("%w: repository is not a Restic rclone connector", ErrUnsupported)
	}
	_, root, err := ParseRcloneGeneratedRoot(repo.Location)
	if err != nil {
		return nil, err
	}
	if repo.Connector == RcloneRemoteConnector {
		return newResticRcloneRemoteSession(ctx, repo)
	}
	binding, err := OpenRcloneVaultConfigBinding(ctx, repo)
	if err != nil {
		return nil, err
	}
	closeBinding := true
	defer func() {
		if closeBinding {
			_ = binding.Close()
		}
	}()
	binary, err := rcloneBinary(pathFor(RcloneComponentID))
	if err != nil {
		return nil, err
	}
	sessionRoot, cache, temporary, cleanup, err := privateRcloneOperationDirectories()
	if err != nil {
		return nil, err
	}
	session := &resticRcloneSession{
		repository: "rclone:provider:" + root,
		root:       sessionRoot,
		config:     binding.NativePath(),
		binding:    binding,
		ctx:        ctx,
		cleanup:    cleanup,
	}
	programOption, err := encodeResticRcloneProgramOption(binary)
	if err != nil {
		return nil, errors.Join(
			fmt.Errorf("encode Restic rclone program option: %w", err),
			cleanup(),
		)
	}
	closeBinding = false
	session.args = []string{"-o", programOption}
	session.env = []string{
		"RCLONE_CONFIG=" + session.config,
		"RCLONE_CACHE_DIR=" + cache,
		"RCLONE_TEMP_DIR=" + temporary,
	}
	if err := binding.Revalidate(ctx); err != nil {
		return nil, errors.Join(err, cleanup())
	}
	return session, nil
}

// newResticRcloneRemoteSession points Restic's rclone child at the user's own
// config file through RCLONE_CONFIG. There is no private config to check
// before or after the run; the user owns the file, and the preflight checks it
// at create, connect, and Reconnect. rclone.args stays Restic's default, so
// RCLONE_ASK_PASSWORD=false in the environment is what stops the child from
// prompting for the rclone config password.
func newResticRcloneRemoteSession(
	ctx context.Context,
	repo models.Repository,
) (*resticRcloneSession, error) {
	run, err := RcloneRemoteRunForRepository(repo)
	if err != nil {
		return nil, err
	}
	binary, err := rcloneBinary(pathFor(RcloneComponentID))
	if err != nil {
		return nil, err
	}
	programOption, err := encodeResticRcloneProgramOption(binary)
	if err != nil {
		return nil, fmt.Errorf("encode Restic rclone program option: %w", err)
	}
	sessionRoot, cache, temporary, cleanup, err := privateRcloneOperationDirectories()
	if err != nil {
		return nil, err
	}
	env := append([]string{
		"RCLONE_CONFIG=" + run.ConfigFile,
		"RCLONE_CACHE_DIR=" + cache,
		"RCLONE_TEMP_DIR=" + temporary,
	}, run.Env...)
	return &resticRcloneSession{
		repository: "rclone:" + run.Root,
		args:       []string{"-o", programOption},
		env:        env,
		root:       sessionRoot,
		config:     run.ConfigFile,
		ctx:        ctx,
		cleanup:    cleanup,
	}, nil
}

// WebDAVRcloneRemote returns the one rclone remote, "base", that every rclone
// use of a WebDAV vault defines: Restic's transport, the sidecar, Vault Size,
// and the connect preview. It gives the fixed TYPE, URL, VENDOR, and USER
// settings, and the WebDAV account password separately because rclone needs
// it obscured: the caller passes it through "rclone obscure -" and sets the
// result as RCLONE_CONFIG_BASE_PASS before rclone runs. The vault URL already
// contains the path, so the remote's root is "base:" itself.
func WebDAVRcloneRemote(
	address vaultidentity.EffectiveAddress, options map[string]string,
) (settings []string, password string, err error) {
	vaultURL, err := vaultidentity.WebDAVURL(address)
	if err != nil {
		return nil, "", err
	}
	return []string{
		"RCLONE_CONFIG_BASE_TYPE=webdav",
		"RCLONE_CONFIG_BASE_URL=" + vaultURL,
		// Always the generic vendor: vendor-specific modes (Nextcloud chunking,
		// SharePoint sign-in) have no Kopia equivalent, and both engines must
		// reach the same server the same way.
		"RCLONE_CONFIG_BASE_VENDOR=other",
		"RCLONE_CONFIG_BASE_USER=" + options["username"],
	}, options["password"], nil
}

// prepareResticWebDAVStorage gives Restic a WebDAV vault through its native
// rclone backend, since Restic has no WebDAV backend of its own. It is not the
// OAuth Restic-rclone path above: there is no per-vault authorization, so
// nothing here reads or creates the vault's persistent rclone config. Every
// command instead gets a throwaway empty config in a private operation
// directory and one environment-defined remote, "base", whose URL already
// contains the vault path (hence the repository "rclone:base:"). Because no
// vault-ID-bound state is used, this also works for the connect Check and for
// resume validation, which run under random repository IDs.
//
// The WebDAV account password is obscured with "rclone obscure -" over
// standard input and passed as RCLONE_CONFIG_BASE_PASS. rclone takes a
// password only on the command line when writing a config, and its obscuring
// is reversible, so a config file would expose the password in the process
// list and on disk; the environment does neither. Every variable name is fixed
// here and never built from user input, so a long vault name, URL, or path
// only ever lands in values.
func prepareResticWebDAVStorage(
	ctx context.Context,
	repo models.Repository,
) (repository string, args, env []string, cleanup func() error, err error) {
	address, err := effectiveAddress(normalizedStorageRepository(repo, ResticID))
	if err != nil {
		return "", nil, nil, nil, err
	}
	remote, password, err := WebDAVRcloneRemote(address, repo.ConnectorOptions)
	if err != nil {
		return "", nil, nil, nil, err
	}
	binary, err := rcloneBinary(pathFor(RcloneComponentID))
	if err != nil {
		return "", nil, nil, nil, err
	}
	programOption, err := encodeResticRcloneProgramOption(binary)
	if err != nil {
		return "", nil, nil, nil, fmt.Errorf("encode Restic rclone program option: %w", err)
	}
	config, cache, temporary, cleanup, err := newTemporaryRcloneConfig(ctx, binary)
	if err != nil {
		return "", nil, nil, nil, err
	}
	output, err := runRcloneVaultConfigCommand(
		ctx, binary,
		[]string{"obscure", "-", "--config", config, "--cache-dir", cache,
			"--temp-dir", temporary, "--log-level", "ERROR"},
		nil, password+"\n", time.Minute, RcloneComponentID,
	)
	if err != nil {
		// The runner keeps rclone's output private, so the cause carries no
		// credential; it is kept so cancellation is still recognized upstream.
		return "", nil, nil, nil, errors.Join(
			fmt.Errorf("native rclone could not prepare the WebDAV account password: %w", err), cleanup(),
		)
	}
	obscured := strings.TrimSpace(output)
	if obscured == "" || strings.ContainsAny(obscured, "\r\n") {
		return "", nil, nil, nil, errors.Join(
			fmt.Errorf("native rclone returned an invalid obscured WebDAV account password"), cleanup(),
		)
	}
	env = append([]string{
		"RCLONE_CONFIG=" + config,
		"RCLONE_CACHE_DIR=" + cache,
		"RCLONE_TEMP_DIR=" + temporary,
	}, remote...)
	env = append(env, "RCLONE_CONFIG_BASE_PASS="+obscured)
	return "rclone:base:", []string{"-o", programOption}, env, cleanup, nil
}

func encodeResticRcloneProgramOption(program string) (string, error) {
	if program == "" {
		return "", fmt.Errorf("rclone program path is empty")
	}
	if strings.ContainsAny(program, "\"\r\n\x00") {
		return "", fmt.Errorf("rclone program path cannot be represented as one Restic shell word")
	}
	return encodeResticExtendedOption("rclone.program", `"`+program+`"`)
}

func (session *resticRcloneSession) close() error {
	// Rclone alone manages its persisted config, including token refresh.
	// Closing revalidates the local config path after Restic has used it and
	// removes per-request resources. Failures here are reported as follow-up
	// cleanup errors; they don't change Restic's result or need another provider probe.
	return errors.Join(
		session.binding.Revalidate(context.WithoutCancel(session.ctx)),
		session.cleanup(), session.binding.Close(),
	)
}
