package command

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// CapturedOutputPublisher receives exact ordinary stdout and stderr after the
// process tree has been drained. The publisher is presentation-only: it must
// consume the readers synchronously and must not let a logging failure affect
// the native result.
type CapturedOutputPublisher func(engine, kind, status, diagnostic string, stdout, stderr io.Reader) bool

type capturedOutputPublisherKey struct{}
type capturedOutputKindKey struct{}

func ContextWithCapturedOutputPublisher(ctx context.Context, publisher CapturedOutputPublisher) context.Context {
	if publisher == nil {
		return ctx
	}
	return context.WithValue(ctx, capturedOutputPublisherKey{}, publisher)
}

// ContextWithCapturedOutputKind identifies the exact native operation-log
// section for one command. It does not enable capture by itself.
func ContextWithCapturedOutputKind(ctx context.Context, kind string) context.Context {
	if kind == "" {
		return ctx
	}
	if existing, _ := ctx.Value(capturedOutputKindKey{}).(string); existing != "" {
		return ctx
	}
	return context.WithValue(ctx, capturedOutputKindKey{}, kind)
}

func capturedOutputPublication(ctx context.Context) (CapturedOutputPublisher, string) {
	publisher, _ := ctx.Value(capturedOutputPublisherKey{}).(CapturedOutputPublisher)
	kind, _ := ctx.Value(capturedOutputKindKey{}).(string)
	if publisher == nil || kind == "" {
		return nil, ""
	}
	return publisher, kind
}

// CapturedOutput owns two temporary, owner-local stream files. Callers parse
// through OpenStdout/OpenStderr and must Close the value when finished.
type CapturedOutput struct {
	stdout *captureFile
	stderr *captureFile
	once   sync.Once
}

type captureFile struct {
	mu       sync.Mutex
	file     *os.File
	path     string
	writeErr error
	closed   bool
}

var beforeCaptureWrite func()

func newCapturedOutput() (*CapturedOutput, error) {
	directory, err := capturedOutputDirectory()
	if err != nil {
		return nil, err
	}
	stdout, err := newCaptureFile(directory, ".stdout-*.tmp")
	if err != nil {
		return nil, err
	}
	stderr, err := newCaptureFile(directory, ".stderr-*.tmp")
	if err != nil {
		stdout.remove()
		return nil, err
	}
	return &CapturedOutput{stdout: stdout, stderr: stderr}, nil
}

// CleanupCapturedOutput removes only abandoned files from the command-output
// directory after the application has acquired its single-instance guard. It
// does not inspect capacity, enforce quotas, rotate logs, or adapt behavior to
// available disk space.
func CleanupCapturedOutput() error {
	directory, err := capturedOutputDirectory()
	if err != nil {
		return err
	}
	return cleanupCapturedOutputDirectory(directory)
}

// CapturedOutputForTests constructs the same file-backed result without
// launching a process. It keeps engine test seams compatible while exercising
// the production reader and cleanup lifecycle.
func CapturedOutputForTests(stdoutValue, stderrValue []byte) (*CapturedOutput, error) {
	output, err := newCapturedOutput()
	if err != nil {
		return nil, err
	}
	_, _ = output.stdout.Write(stdoutValue)
	_, _ = output.stderr.Write(stderrValue)
	if err := output.finish(); err != nil {
		_ = output.Close()
		return nil, err
	}
	return output, nil
}

func newCaptureFile(directory, pattern string) (*captureFile, error) {
	file, err := createCapturedOutputFile(directory, pattern)
	if err != nil {
		return nil, err
	}
	return &captureFile{file: file, path: file.Name()}, nil
}

// Write deliberately reports complete consumption even after the local file
// fails. Pipe drainage must never stop because diagnostic capture failed; the
// recorded write error is returned separately as output-processing failure.
func (file *captureFile) Write(value []byte) (int, error) {
	file.mu.Lock()
	defer file.mu.Unlock()
	if beforeCaptureWrite != nil {
		beforeCaptureWrite()
	}
	if file.writeErr != nil || file.closed {
		return len(value), nil
	}
	if _, err := file.file.Write(value); err != nil {
		file.writeErr = err
	}
	return len(value), nil
}

func (file *captureFile) finish() error {
	file.mu.Lock()
	defer file.mu.Unlock()
	if file.closed {
		return file.writeErr
	}
	if file.writeErr == nil {
		file.writeErr = file.file.Sync()
	}
	if err := file.file.Close(); file.writeErr == nil {
		file.writeErr = err
	}
	file.closed = true
	return file.writeErr
}

func (file *captureFile) open() (*os.File, error) {
	file.mu.Lock()
	closed := file.closed
	path := file.path
	err := file.writeErr
	file.mu.Unlock()
	if !closed {
		return nil, fmt.Errorf("captured output is still being written")
	}
	if err != nil {
		return nil, err
	}
	return openCapturedOutput(filepath.Clean(path))
}

func (file *captureFile) remove() {
	file.mu.Lock()
	if !file.closed && file.file != nil {
		_ = file.file.Close()
		file.closed = true
	}
	path := file.path
	file.mu.Unlock()
	if path != "" {
		_ = removeCapturedOutput(path)
	}
}

func (output *CapturedOutput) finish() error {
	return errorsJoin(output.stdout.finish(), output.stderr.finish())
}

// OpenStdout returns a fresh reader positioned at the start of exact stdout.
func (output *CapturedOutput) OpenStdout() (*os.File, error) { return output.stdout.open() }

// OpenStderr returns a fresh reader positioned at the start of exact stderr.
func (output *CapturedOutput) OpenStderr() (*os.File, error) { return output.stderr.open() }

func (output *CapturedOutput) publish(ctx context.Context, engine, status, diagnostic string) bool {
	publisher, kind := capturedOutputPublication(ctx)
	if publisher == nil {
		return false
	}
	stdout, stdoutErr := output.OpenStdout()
	stderr, stderrErr := output.OpenStderr()
	published := false
	if stdoutErr == nil && stderrErr == nil {
		published = publisher(engine, kind, status, diagnostic, stdout, stderr)
	}
	if stdout != nil {
		_ = stdout.Close()
	}
	if stderr != nil {
		_ = stderr.Close()
	}
	return published
}

// PublishForTests exercises the production publication boundary for engine
// test seams that construct captured output without launching a child process.
func (output *CapturedOutput) PublishForTests(ctx context.Context, engine, status, diagnostic string) bool {
	return output.publish(ctx, engine, status, diagnostic)
}

// Close removes both run-local capture files.
func (output *CapturedOutput) Close() error {
	if output == nil {
		return nil
	}
	output.once.Do(func() {
		output.stdout.remove()
		output.stderr.remove()
	})
	return nil
}

func errorsJoin(values ...error) error {
	if err := errors.Join(values...); err != nil {
		return fmt.Errorf("captured command output: %w", err)
	}
	return nil
}
