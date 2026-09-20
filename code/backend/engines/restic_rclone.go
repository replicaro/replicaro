package engines

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
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
			// Adapter-owned native classifications wrapped around the command
			// carrier must survive separation from a simultaneous capture failure.
			// Otherwise callers lose the established safe guidance even though the
			// requested native failure itself remains conclusive.
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
	// Rclone exclusively owns its opaque persisted config, including token
	// refresh. Closing reopens the exact local path after native access and removes
	// request-local resources. These failures are follow-up cleanup truth; neither
	// changes Restic's result nor requires another provider/account probe.
	return errors.Join(
		session.binding.Revalidate(context.WithoutCancel(session.ctx)),
		session.cleanup(), session.binding.Close(),
	)
}
