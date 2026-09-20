package engines

import (
	"context"
	"errors"
	"strings"

	"github.com/local/replicaro/command"
)

// RequestedOperationStatus describes only the native command requested at an
// engine method boundary. Native setup and wrapper follow-up work are not the
// requested command and cannot supply or rewrite this result.
type RequestedOperationStatus string

const (
	RequestedOperationNotStarted  RequestedOperationStatus = "not_started"
	RequestedOperationSucceeded   RequestedOperationStatus = "succeeded"
	RequestedOperationFailed      RequestedOperationStatus = "failed"
	RequestedOperationInterrupted RequestedOperationStatus = "interrupted"
)

// RequestedOperationFailure carries one run-local requested-command result.
// FollowupError is reserved for wrapper work after a successful requested
// command, such as snapshot identity reconciliation or temporary config
// cleanup. It never changes Status from succeeded.
type RequestedOperationFailure struct {
	Engine           string
	Status           RequestedOperationStatus
	ProcessStarted   bool
	PreparationError error
	NativeError      error
	FollowupError    error
}

type separatedNativeProcessError struct {
	message string
	cause   error
}

func (e *separatedNativeProcessError) Error() string { return e.message }
func (e *separatedNativeProcessError) Unwrap() error { return e.cause }

func separateNativeProcessError(presentation string, nativeCause, outputCause error) error {
	presentation = strings.TrimSpace(strings.ReplaceAll(presentation, "\n"+outputCause.Error(), ""))
	if presentation == "" {
		presentation = nativeCause.Error()
	}
	return &separatedNativeProcessError{message: presentation, cause: nativeCause}
}

func (e *RequestedOperationFailure) Error() string {
	engine := e.Engine
	if engine == "" {
		engine = "engine"
	}
	switch e.Status {
	case RequestedOperationNotStarted:
		message := "native " + engine + " preparation failed; requested native operation was not started"
		if e.PreparationError != nil {
			return message + ": " + e.PreparationError.Error()
		}
		return message
	case RequestedOperationInterrupted:
		if e.ProcessStarted {
			message := "native " + engine + " operation was interrupted; completion is unknown"
			if e.NativeError != nil {
				return message + ": " + e.NativeError.Error()
			}
			return message
		}
		message := "native " + engine + " operation was interrupted before native process start"
		if e.PreparationError != nil {
			return message + ": " + e.PreparationError.Error()
		}
		return message
	case RequestedOperationFailed:
		if e.NativeError != nil {
			return e.NativeError.Error()
		}
		return "native " + engine + " operation failed"
	case RequestedOperationSucceeded:
		if e.FollowupError != nil {
			return "native " + engine + " operation succeeded but its orchestration follow-up failed: " + e.FollowupError.Error()
		}
	}
	return "native " + engine + " operation outcome is incomplete"
}

func (e *RequestedOperationFailure) Unwrap() []error {
	result := make([]error, 0, 3)
	if e.PreparationError != nil {
		result = append(result, e.PreparationError)
	}
	if e.NativeError != nil {
		result = append(result, e.NativeError)
	}
	if e.FollowupError != nil {
		result = append(result, e.FollowupError)
	}
	return result
}

func requestedOperationPreparationFailure(engine string, err error) error {
	if err == nil {
		return nil
	}
	status := RequestedOperationNotStarted
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		status = RequestedOperationInterrupted
	}
	return &RequestedOperationFailure{
		Engine: engine, Status: status, PreparationError: err,
	}
}

func requestedOperationCommandFailure(engine string, nativeErr, followupErr error, processStarted bool) error {
	if nativeCause, outputCause := command.FailureCauses(nativeErr); outputCause != nil {
		// Output capture is wrapper follow-up truth even when the native process
		// also failed or was interrupted. Preserve the native adapter message for
		// native truth while keeping the typed output cause separate.
		followupErr = errors.Join(followupErr, outputCause)
		if nativeCause == nil {
			nativeErr = nil
		} else {
			nativeErr = separateNativeProcessError(nativeErr.Error(), nativeCause, outputCause)
		}
	}
	if nativeErr == nil && followupErr == nil {
		return nil
	}
	if !processStarted {
		return requestedOperationPreparationFailure(engine, errors.Join(nativeErr, followupErr))
	}
	status := RequestedOperationSucceeded
	if nativeErr != nil {
		status = RequestedOperationFailed
		if errors.Is(nativeErr, context.Canceled) || errors.Is(nativeErr, context.DeadlineExceeded) {
			status = RequestedOperationInterrupted
		}
	}
	return &RequestedOperationFailure{
		Engine: engine, Status: status, ProcessStarted: true,
		NativeError: nativeErr, FollowupError: followupErr,
	}
}

func requestedOperationFollowupFailure(engine string, followupErr error) error {
	return requestedOperationCommandFailure(engine, nil, followupErr, true)
}

func addRequestedOperationFollowup(engine string, operationErr, followupErr error) error {
	if followupErr == nil {
		return operationErr
	}
	var failure *RequestedOperationFailure
	if errors.As(operationErr, &failure) {
		copy := *failure
		copy.FollowupError = errors.Join(copy.FollowupError, followupErr)
		return &copy
	}
	if operationErr == nil {
		return requestedOperationFollowupFailure(engine, followupErr)
	}
	return errors.Join(operationErr, followupErr)
}

// RequestedOperationOutcome returns the one requested-command result for the
// ordinary backup-engine boundary. The Restic-specific run-local carrier
// preserves separate cleanup truth without changing the requested native
// result.
func RequestedOperationOutcome(err error) (
	status RequestedOperationStatus,
	processStarted bool,
	preparationErr, nativeErr, followupErr error,
	known bool,
) {
	if err == nil {
		return RequestedOperationSucceeded, true, nil, nil, nil, true
	}
	var failure *RequestedOperationFailure
	if errors.As(err, &failure) {
		return failure.Status, failure.ProcessStarted, failure.PreparationError,
			failure.NativeError, failure.FollowupError, true
	}
	var resticFailure *ResticOperationFailure
	if errors.As(err, &resticFailure) {
		return RequestedOperationStatus(resticFailure.Status), resticFailure.ProcessStarted,
			resticFailure.PreparationError, resticFailure.NativeError,
			errors.Join(resticFailure.OutputProcessingError, resticFailure.CleanupError), true
	}
	return "", false, nil, nil, nil, false
}
