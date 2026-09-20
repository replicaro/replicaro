package engines

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/local/replicaro/command"
)

var commandRunner = runCommand
var commandRunnerWithInput = runCommandWithInput
var privateCommandRunner = runPrivateCommand
var capturedCommandRunner = runCapturedCommand

var errReconnectRequired = errors.New("vault reconnect required")
var errInvalidRcloneLocalBinding = errors.New("invalid local rclone vault binding")

func combineSuccessfulNativeStreams(stdout, stderr string) string {
	if stderr == "" {
		return stdout
	}
	if stdout == "" || strings.HasSuffix(stdout, "\n") {
		return stdout + stderr
	}
	return stdout + "\n" + stderr
}

type invalidRcloneLocalBindingError struct{ err error }

func (e *invalidRcloneLocalBindingError) Error() string { return e.err.Error() }
func (e *invalidRcloneLocalBindingError) Unwrap() error { return e.err }
func (e *invalidRcloneLocalBindingError) Is(target error) bool {
	return target == errInvalidRcloneLocalBinding || errors.Is(e.err, target)
}

type reconnectRequiredError struct{ err error }

func (e *reconnectRequiredError) Error() string { return e.err.Error() }
func (e *reconnectRequiredError) Unwrap() error { return e.err }
func (e *reconnectRequiredError) Is(target error) bool {
	return target == errReconnectRequired || errors.Is(e.err, target)
}

func reconnectRequired(err error) error {
	if err == nil || errors.Is(err, errReconnectRequired) {
		return err
	}
	return &reconnectRequiredError{err: err}
}

func invalidRcloneLocalBinding(err error) error {
	if err == nil || errors.Is(err, errInvalidRcloneLocalBinding) {
		return err
	}
	return &invalidRcloneLocalBindingError{err: err}
}

// IsReconnectRequired reports only conclusive local Restic-rclone binding or
// provider-identity facts. Transient native and provider failures are excluded.
func IsReconnectRequired(err error) bool {
	return errors.Is(err, errReconnectRequired)
}

func SetCommandRunnerForTests(next func(context.Context, string, []string, []string, time.Duration, string) (string, error)) func() {
	previous := commandRunner
	previousWithInput := commandRunnerWithInput
	previousPrivate := privateCommandRunner
	previousCaptured := capturedCommandRunner
	if next == nil {
		commandRunner = runCommand
		commandRunnerWithInput = runCommandWithInput
		privateCommandRunner = runPrivateCommand
		capturedCommandRunner = runCapturedCommand
	} else {
		commandRunner = next
		commandRunnerWithInput = func(ctx context.Context, path string, args, env []string, input string, timeout time.Duration, engine string) (string, error) {
			return next(ctx, path, args, env, timeout, engine)
		}
		privateCommandRunner = func(ctx context.Context, path string, args, env []string, timeout time.Duration, engine string) (string, error) {
			output, err := next(ctx, path, args, env, timeout, engine)
			if err != nil {
				return "", &privateOutputError{message: "private native command failed", cause: err}
			}
			return output, nil
		}
		capturedCommandRunner = func(ctx context.Context, path string, args, env []string, timeout time.Duration, engine string) (*command.CapturedOutput, string, error) {
			output, err := next(ctx, path, args, env, timeout, engine)
			capture, captureErr := command.CapturedOutputForTests([]byte(output), nil)
			if captureErr != nil {
				return nil, "", captureErr
			}
			status := "succeeded"
			if err != nil {
				status = "failed"
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					status = "interrupted"
				}
			}
			_ = capture.PublishForTests(ctx, engine, status, output)
			return capture, output, err
		}
	}
	return func() {
		commandRunner = previous
		commandRunnerWithInput = previousWithInput
		privateCommandRunner = previousPrivate
		capturedCommandRunner = previousCaptured
	}
}

func runCapturedCommand(ctx context.Context, path string, args []string, env []string, timeout time.Duration, engine string) (*command.CapturedOutput, string, error) {
	capture, output, err := command.RunCaptured(ctx, path, args, env, timeout, engine)
	if err != nil {
		return capture, output, &CommandError{Engine: engine, Output: output, Err: err}
	}
	return capture, output, nil
}

func runCommand(ctx context.Context, path string, args []string, env []string, timeout time.Duration, engine string) (string, error) {
	output, err := command.Run(ctx, path, args, env, timeout, engine)
	if err != nil {
		return output, &CommandError{Engine: engine, Output: output, Err: err}
	}
	return output, nil
}

func runCommandWithInput(ctx context.Context, path string, args []string, env []string, input string, timeout time.Duration, engine string) (string, error) {
	output, err := command.RunWithInputSecrets(ctx, path, args, env, input, timeout, engine)
	if err != nil {
		return output, &CommandError{Engine: engine, Output: output, Err: err}
	}
	return output, nil
}

func runPrivateCommand(ctx context.Context, path string, args []string, env []string, timeout time.Duration, engine string) (string, error) {
	output, err := command.RunWithInputSecretsPrivateOutput(ctx, path, args, env, "", timeout, engine)
	if err != nil {
		return "", &CommandError{Engine: engine, Err: err}
	}
	return output, nil
}
