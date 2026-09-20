// Package command is the single process boundary for all backup engines.
// Arguments are always passed as an argv array; no shell is involved.
package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type Error struct {
	Engine string
	Output string
	Err    error
}

type PrivateFailureKind uint8

const (
	PrivateFailureUnknown PrivateFailureKind = iota
	PrivateFailureMissingObject
	PrivateFailureObjectIsDirectory
	PrivateFailureDecryptAuthentication
)

type PrivateOutputError struct {
	Kind  PrivateFailureKind
	Cause error
}

func (e *PrivateOutputError) Error() string { return "private native command failed" }
func (e *PrivateOutputError) Unwrap() error { return e.Cause }

// PrivateOutputFailureKind returns only the narrow output-free classification
// needed by private rclone workflows. Native text is inspected at the process
// boundary and is not retained in the returned error graph.
func PrivateOutputFailureKind(err error) PrivateFailureKind {
	if _, followup := FailureCauses(err); followup != nil {
		return PrivateFailureUnknown
	}
	var private *PrivateOutputError
	if errors.As(err, &private) {
		return private.Kind
	}
	return PrivateFailureUnknown
}

const maxDiagnosticBytes = 512 << 20
const maxCapturedDiagnosticBytes = 128 << 10
const maxPrivateDiagnosticBytes = 32 << 20
const maxPrivateObservedStderrBytes = 1 << 20

// NoTotalDeadline preserves the parent context without adding a command-wide
// timer. Cancellation and shutdown still flow through that parent context.
const NoTotalDeadline time.Duration = -1

var maxCommandOutputBytes = 512 << 20
var createProcessTree = newProcessTree

var ErrOutputLimitExceeded = errors.New("engine command output exceeded the 512 MiB safety limit")
var ErrPreProcessAdmission = errors.New("native process admission failed before process start")

type NativeProcessFailure struct{ Err error }

func (e *NativeProcessFailure) Error() string { return e.Err.Error() }
func (e *NativeProcessFailure) Unwrap() error { return e.Err }

type OutputProcessingFailure struct{ Err error }

func (e *OutputProcessingFailure) Error() string { return e.Err.Error() }
func (e *OutputProcessingFailure) Unwrap() error { return e.Err }

func splitFailures(err error) (nativeErr, outputErr error) {
	var native *NativeProcessFailure
	if errors.As(err, &native) {
		nativeErr = native
	}
	var output *OutputProcessingFailure
	if errors.As(err, &output) {
		outputErr = output
	}
	return nativeErr, outputErr
}

// FailureCauses returns separately typed native-process and output-processing
// causes without inferring either one from presentation text.
func FailureCauses(err error) (nativeErr, outputErr error) { return splitFailures(err) }

func commandError(engine, output string, nativeErr, outputErr error) error {
	if nativeErr == nil && outputErr == nil {
		return nil
	}
	var causes []error
	if nativeErr != nil {
		causes = append(causes, &NativeProcessFailure{Err: nativeErr})
	}
	if outputErr != nil {
		causes = append(causes, &OutputProcessingFailure{Err: outputErr})
	}
	return &Error{Engine: engine, Output: output, Err: errors.Join(causes...)}
}

type PreProcessAdmissionError struct {
	Err error
}

func (e *PreProcessAdmissionError) Error() string {
	return ErrPreProcessAdmission.Error() + ": " + e.Err.Error()
}

func (e *PreProcessAdmissionError) Unwrap() []error {
	return []error{ErrPreProcessAdmission, e.Err}
}

func IsPreProcessAdmission(err error) bool {
	return errors.Is(err, ErrPreProcessAdmission)
}

type beforeProcessKey struct{}
type beforeProcessFunc func(context.Context) error
type processStartTrackerKey struct{}
type liveOutputKey struct{}
type liveOutputEnabledKey struct{}
type successfulStderrKey struct{}

type LiveOutputObserver func(stream, text string)

// ContextWithLiveOutput carries a dormant operation observer. Engine adapters
// must separately enable it only around an eligible requested native command.
func ContextWithLiveOutput(ctx context.Context, observer LiveOutputObserver) context.Context {
	if observer == nil {
		return ctx
	}
	return context.WithValue(ctx, liveOutputKey{}, observer)
}

// ContextWithLiveOutputEnabled opts one exact command into live observation.
func ContextWithLiveOutputEnabled(ctx context.Context) context.Context {
	return context.WithValue(ctx, liveOutputEnabledKey{}, true)
}

// ContextWithoutLiveOutput prevents inherited operation observation at a
// private or otherwise ineligible native command boundary.
func ContextWithoutLiveOutput(ctx context.Context) context.Context {
	return context.WithValue(ctx, liveOutputEnabledKey{}, false)
}

type successfulStderrCapture struct {
	mu    sync.Mutex
	value string
}

// ContextWithSuccessfulStderrCapture lets an eligible user-facing adapter keep
// successful stderr as diagnostics without mixing it into strict stdout parser
// input. Private and internal commands do not install this capture.
func ContextWithSuccessfulStderrCapture(ctx context.Context) (context.Context, func() string) {
	capture := &successfulStderrCapture{}
	return context.WithValue(ctx, successfulStderrKey{}, capture), func() string {
		capture.mu.Lock()
		defer capture.mu.Unlock()
		return capture.value
	}
}

func recordSuccessfulStderr(ctx context.Context, value string) {
	capture, _ := ctx.Value(successfulStderrKey{}).(*successfulStderrCapture)
	if capture == nil || value == "" {
		return
	}
	capture.mu.Lock()
	capture.value = value
	capture.mu.Unlock()
}

type processStartTracker struct {
	starts atomic.Uint64
}

// ContextWithProcessStartTracking returns an operation-scoped, in-memory
// process-start observation. It distinguishes admission failures before any
// child starts from failures after an earlier child in the same invocation.
func ContextWithProcessStartTracking(ctx context.Context) (context.Context, func() bool) {
	tracker := &processStartTracker{}
	return context.WithValue(ctx, processStartTrackerKey{}, tracker), func() bool {
		return tracker.starts.Load() > 0
	}
}

func recordProcessStart(ctx context.Context) {
	if tracker, _ := ctx.Value(processStartTrackerKey{}).(*processStartTracker); tracker != nil {
		tracker.starts.Add(1)
	}
}

// ContextWithBeforeProcess installs a fail-closed admission callback at the
// process boundary. The callback runs immediately before every process launch
// made with the returned context.
func ContextWithBeforeProcess(ctx context.Context, before func(context.Context) error) context.Context {
	if before == nil {
		return ctx
	}
	previous, _ := ctx.Value(beforeProcessKey{}).(beforeProcessFunc)
	return context.WithValue(ctx, beforeProcessKey{}, beforeProcessFunc(func(checkCtx context.Context) error {
		if err := before(checkCtx); err != nil {
			return err
		}
		if previous != nil {
			return previous(checkCtx)
		}
		return nil
	}))
}

// ContextWithFinalCancellationAdmission preserves any existing process
// admission callback and adds a final cancellation check immediately before
// process start. It is used by job hooks, whose contract forbids launching a
// child after cancellation has already won the launch race.
func ContextWithFinalCancellationAdmission(ctx context.Context) context.Context {
	before, _ := ctx.Value(beforeProcessKey{}).(beforeProcessFunc)
	return context.WithValue(ctx, beforeProcessKey{}, beforeProcessFunc(func(checkCtx context.Context) error {
		if before != nil {
			if err := before(checkCtx); err != nil {
				return err
			}
		}
		return checkCtx.Err()
	}))
}

// HasBeforeProcess reports whether the context already carries a process
// admission callback. Callers use it to preserve a narrower explicit check.
func HasBeforeProcess(ctx context.Context) bool {
	before, _ := ctx.Value(beforeProcessKey{}).(beforeProcessFunc)
	return before != nil
}

func admitProcess(ctx context.Context) error {
	before, _ := ctx.Value(beforeProcessKey{}).(beforeProcessFunc)
	if before == nil {
		return nil
	}
	// Suppress the callback while it runs so a bounded observation helper may
	// launch its own process without recursively invoking itself.
	err := before(context.WithValue(ctx, beforeProcessKey{}, beforeProcessFunc(nil)))
	if err == nil || IsPreProcessAdmission(err) {
		return err
	}
	return &PreProcessAdmissionError{Err: err}
}

func (e *Error) Error() string {
	if strings.TrimSpace(e.Output) == "" {
		return e.Engine + " engine command failed: " + e.Err.Error()
	}
	if errors.Is(e.Err, context.Canceled) || errors.Is(e.Err, context.DeadlineExceeded) {
		nativeErr, _ := splitFailures(e.Err)
		if nativeErr == nil {
			nativeErr = e.Err
		}
		return e.Engine + " engine command failed: " + strings.TrimSpace(e.Output) + ": " + nativeErr.Error()
	}
	return e.Engine + " engine command failed: " + strings.TrimSpace(e.Output)
}

func (e *Error) Unwrap() error { return e.Err }

func Run(ctx context.Context, path string, args, env []string, timeout time.Duration, engine string) (string, error) {
	return runWithInput(ctx, path, args, env, "", timeout, engine, false, false, nil, nil)
}

// RunCaptured drains exact ordinary stdout and stderr into owner-local files.
// Only a bounded diagnostic excerpt remains in memory; total output size does
// not alter the native process result. The caller must Close the returned
// capture after parsing it.
func RunCaptured(ctx context.Context, path string, args, env []string, timeout time.Duration, engine string) (*CapturedOutput, string, error) {
	capture, err := newCapturedOutput()
	if err != nil {
		return nil, "", &Error{Engine: engine, Err: err}
	}
	diagnostic, runErr := runWithInput(ctx, path, args, env, "", timeout, engine, false, false, nil, capture)
	status := "succeeded"
	if runErr != nil {
		status = "failed"
		if errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			status = "interrupted"
		}
	}
	_ = capture.publish(ctx, engine, status, diagnostic)
	return capture, diagnostic, runErr
}

// RunWithInputSecrets is the process boundary for commands that require a
// credential on standard input. The input is never placed in argv or output.
func RunWithInputSecrets(ctx context.Context, path string, args, env []string, input string, timeout time.Duration, engine string) (string, error) {
	return runWithInput(ctx, path, args, env, input, timeout, engine, false, false, nil, nil)
}

// RunWithInputSecretsPrivateOutput is for commands whose standard output is
// sensitive data. On failure, neither output stream is returned or included in
// the public error.
func RunWithInputSecretsPrivateOutput(ctx context.Context, path string, args, env []string, input string, timeout time.Duration, engine string) (string, error) {
	return runWithInput(ctx, path, args, env, input, timeout, engine, true, false, nil, nil)
}

// RunWithInputSecretsPrivateOutputObservedStderr is the one live-output seam
// used by native rclone authorization. The observer receives complete bounded
// stderr lines, while all process output remains private. An observer error
// cancels and reaps the process tree.
func RunWithInputSecretsPrivateOutputObservedStderr(
	ctx context.Context,
	path string,
	args, env []string,
	input string,
	timeout time.Duration,
	engine string,
	observe func([]byte) error,
) (string, error) {
	if observe == nil {
		return "", &Error{Engine: engine, Err: fmt.Errorf("private stderr observer is required")}
	}
	return runWithInput(ctx, path, args, env, input, timeout, engine, true, false, observe, nil)
}

// RunDiscardOutput drains stdout and stderr without retaining either stream.
// It is for current-user hooks whose arbitrary output is private and is never
// presented. The ordinary process-tree and cancellation lifecycle
// remains unchanged.
func RunDiscardOutput(ctx context.Context, path string, args, env []string, engine string) error {
	_, err := runWithInput(ctx, path, args, env, "", NoTotalDeadline, engine, true, true, nil, nil)
	return err
}

func runWithInput(ctx context.Context, path string, args, env []string, input string, timeout time.Duration, engine string, privateOutput, discardOutput bool, observeStderr func([]byte) error, captured *CapturedOutput) (string, error) {
	commandContext := ctx
	cancel := func() {}
	if timeout != NoTotalDeadline {
		commandContext, cancel = context.WithTimeout(ctx, timeout)
	}
	defer cancel()
	tree, treeErr := createProcessTree()
	if treeErr != nil {
		return "", &Error{Engine: engine, Err: treeErr}
	}
	defer tree.close()
	process := exec.Command(path, args...)
	process.Env = minimalEnvironment(env)
	if fence := nativeProcessFenceFromContext(ctx); fence != nil && fence.file != nil {
		process.ExtraFiles = append(process.ExtraFiles, fence.file)
	}
	if input != "" {
		process.Stdin = strings.NewReader(input)
	}
	configureWindowsCommand(process)
	stdoutLimit := maxCommandOutputBytes
	if captured != nil {
		stdoutLimit = maxCapturedDiagnosticBytes
	}
	stdout := newCappedDiagnosticBuffer(stdoutLimit)
	stderrLimit := maxDiagnosticBytes
	if privateOutput {
		stderrLimit = maxPrivateDiagnosticBytes
	}
	stderr := newHeadTailDiagnosticBuffer(stderrLimit)
	var stdoutDestination io.Writer = stdout
	var stderrDestination io.Writer = stderr
	if captured != nil {
		stderr = newHeadTailDiagnosticBuffer(maxCapturedDiagnosticBytes)
		stdoutDestination = io.MultiWriter(captured.stdout, stdout)
		stderrDestination = io.MultiWriter(captured.stderr, stderr)
	}
	if discardOutput {
		stdoutDestination = io.Discard
		stderrDestination = io.Discard
	}
	var observedStderr *privateStderrObserver
	if observeStderr != nil {
		observedStderr = &privateStderrObserver{
			observe: observeStderr,
			cancel:  cancel,
		}
		stderrDestination = observedStderr
	}
	var live LiveOutputObserver
	var liveStdout, liveStderr *liveRecordWriter
	if !privateOutput && !discardOutput {
		live = liveOutputObserver(commandContext)
		if live != nil {
			// Raw buffers stay first in each tee. The selected engines own ordinary
			// output content; Replicaro only assembles bounded complete live records.
			liveStdout = newLiveRecordWriter("stdout", live)
			liveStderr = newLiveRecordWriter("stderr", live)
			stdoutDestination = io.MultiWriter(stdoutDestination, liveStdout)
			stderrDestination = io.MultiWriter(stderrDestination, liveStderr)
		}
	}
	// Own both ends of the output pipes. Cmd.StdoutPipe/Cmd.StderrPipe are
	// closed by Cmd.Wait, which races concurrent readers and can discard output
	// from short-lived commands. Supplying *os.File writers keeps the read ends
	// under our control while still allowing process-group cleanup before EOF.
	stdoutPipe, stdoutWriter, pipeErr := os.Pipe()
	if pipeErr != nil {
		return "", &Error{Engine: engine, Err: pipeErr}
	}
	stderrPipe, stderrWriter, pipeErr := os.Pipe()
	if pipeErr != nil {
		_ = stdoutPipe.Close()
		_ = stdoutWriter.Close()
		return "", &Error{Engine: engine, Err: pipeErr}
	}
	process.Stdout = stdoutWriter
	process.Stderr = stderrWriter
	if err := admitProcess(commandContext); err != nil {
		_ = stdoutPipe.Close()
		_ = stdoutWriter.Close()
		_ = stderrPipe.Close()
		_ = stderrWriter.Close()
		return "", err
	}
	if err := process.Start(); err != nil {
		_ = stdoutPipe.Close()
		_ = stdoutWriter.Close()
		_ = stderrPipe.Close()
		_ = stderrWriter.Close()
		return "", &Error{Engine: engine, Err: err}
	}
	if !processStartsSuspended {
		recordProcessStart(commandContext)
	}
	// Start duplicates/inherits the write handles for the child. The parent must
	// close its copies so readers observe EOF after the process group exits.
	_ = stdoutWriter.Close()
	_ = stderrWriter.Close()
	readDone := make(chan struct{})
	stdoutReader, stderrReader := newCommandOutputReader(stdoutPipe), newCommandOutputReader(stderrPipe)
	var stdoutReadErr, stderrReadErr error
	go func() {
		defer close(readDone)
		var copies sync.WaitGroup
		copies.Add(2)
		go func() {
			defer copies.Done()
			_, stdoutReadErr = io.Copy(stdoutDestination, stdoutReader)
			if liveStdout != nil {
				liveStdout.flush()
			}
		}()
		go func() {
			defer copies.Done()
			_, stderrReadErr = io.Copy(stderrDestination, stderrReader)
			if liveStderr != nil {
				liveStderr.flush()
			}
			if observedStderr != nil {
				observedStderr.flush()
			}
		}()
		copies.Wait()
	}()
	if err := tree.attach(process); err != nil {
		_ = tree.terminate(process)
		_ = process.Process.Kill()
		_ = process.Wait()
		_ = stdoutPipe.Close()
		_ = stderrPipe.Close()
		<-readDone
		attachErr := fmt.Errorf("attach command process tree: %w", err)
		if processStartsSuspended {
			return "", &PreProcessAdmissionError{Err: attachErr}
		}
		return "", &Error{Engine: engine, Err: attachErr}
	}
	if processStartsSuspended {
		recordProcessStart(commandContext)
	}
	// Wait separately so cancellation can terminate the whole process tree on
	// Windows instead of relying on os.Process.Kill for only the direct child.
	waitResult := make(chan processWaitResult, 1)
	go func() { waitResult <- tree.wait(process) }()
	var err error
	var cleanupErr error
	var descendantCollectionErr error
	var waitReaped bool
	select {
	case result := <-waitResult:
		err = result.err
		waitReaped = result.reaped
		// A process can exit at the same instant its context is canceled. Treat
		// that race as cancellation so callers never mistake a canceled engine
		// operation for a successful/ordinary process failure.
		if contextErr := commandContext.Err(); contextErr != nil {
			err = contextErr
		}
	case <-commandContext.Done():
		select {
		case result := <-waitResult:
			err = result.err
			waitReaped = result.reaped
		default:
			if terminateErr := tree.terminate(process); terminateErr != nil {
				if handled, fallbackErr := tree.handleTerminateFailure(process); handled {
					cleanupErr = errors.Join(cleanupErr, terminateErr, fallbackErr)
					if fallbackErr != nil {
						err = commandContext.Err()
						break
					}
				}
			}
			result := <-waitResult
			err = result.err
			waitReaped = result.reaped
		}
	}
	if contextErr := commandContext.Err(); contextErr != nil {
		err = contextErr
	}
	// Always reap an isolated Unix process group. A command may exit while
	// descendants that inherited its stdio remain alive; waiting for the direct
	// process alone would otherwise leak those descendants indefinitely.  The
	// process-tree implementation validates the group identity and tolerates an
	// already-reaped group, so this is safe for normal success and failure too.
	// waitid/kqueue deliberately leaves the leader unreaped. In that state the
	// leader still anchors the validated PID/PGID, so terminate directly. An
	// additional kill(group, 0) probe is both unnecessary and racy on Darwin:
	// it can miss a live descendant that still owns an inherited output pipe.
	if !waitReaped || tree.active(process) {
		terminateErr := tree.terminate(process)
		cleanupErr = errors.Join(cleanupErr, terminateErr)
		if terminateErr != nil {
			// A failed group termination can leave inherited writers open forever.
			if runtime.GOOS == "linux" && os.Getpid() == 1 {
				stdoutReader.interrupt()
				stderrReader.interrupt()
			} else {
				_ = stdoutPipe.Close()
				_ = stderrPipe.Close()
			}
		}
	}
	// On Unix waitid intentionally left the leader unreaped while the process
	// group was cleaned. Reap only afterward so the top-level PID/PGID remains
	// anchored throughout cleanup. Darwin's documented accepted same-user
	// secondary-PGID reuse limitation is explained at terminatePlatform.
	if !waitReaped {
		canReap := true
		if cleanupErr != nil {
			if handled, platformCanReap, fallbackErr := tree.prepareReapAfterCleanupFailure(process); handled {
				cleanupErr = errors.Join(cleanupErr, fallbackErr)
				canReap = platformCanReap
			}
		}
		if canReap {
			descendantCollectionErr = tree.collectAdoptedDescendants(process)
			if descendantCollectionErr != nil {
				// An uncollected live helper may still hold an output writer.
				stdoutReader.interrupt()
				stderrReader.interrupt()
			}
			err = errors.Join(err, tree.reap(process))
		}
	}
	// The read handles are parent-owned, so Wait cannot close them before all
	// buffered output is consumed. Descendant cleanup above guarantees EOF even
	// when a grandchild inherited the write descriptors.
	// Capture writes are part of pipe drainage, so a fixed post-exit timer here
	// would truncate a healthy command merely because its owner-local filesystem
	// was slow. Process-tree cleanup above closes leaked descendant writers; once
	// that boundary is complete, wait for both exact streams without a byte- or
	// elapsed-time cap.
	<-readDone
	_ = stdoutPipe.Close()
	_ = stderrPipe.Close()
	if runtime.GOOS == "linux" && os.Getpid() == 1 {
		// PID 1 collection/drainage failures are follow-up causes, not native results.
		descendantCollectionErr = errors.Join(descendantCollectionErr, cleanupErr, stdoutReadErr, stderrReadErr)
		cleanupErr = nil
	}
	err = tree.joinCleanupResult(err, cleanupErr, commandContext.Err())
	outputErr := descendantCollectionErr
	if captured != nil {
		outputErr = errors.Join(outputErr, captured.finish())
	}
	if observedStderr != nil {
		outputErr = errors.Join(outputErr, observedStderr.errValue())
	}
	if captured == nil && stdout.truncated {
		outputErr = errors.Join(outputErr, ErrOutputLimitExceeded)
	}
	raw := stdout.buffer.String()
	diagnosticStdout := ""
	if !privateOutput {
		diagnosticStdout = raw
	}
	diagnosticStderr, stderrOmitted := stderr.String(), stderr.truncated
	combined := diagnosticStdout
	if diagnosticStderr != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += diagnosticStderr
	}
	diagnostic := boundedDiagnosticWithOmission(combined, stdout.truncated || stderrOmitted)
	if err != nil || outputErr != nil {
		if privateOutput {
			cause := commandError(engine, "", err, outputErr)
			return "", &PrivateOutputError{Kind: classifyPrivateFailure(stderr.String()), Cause: cause}
		}
		return diagnostic, commandError(engine, diagnostic, err, outputErr)
	}
	// Successful stderr capture feeds ordinary user-facing diagnostics only.
	// Private and discarded output must not cross that presentation boundary.
	if !privateOutput && !discardOutput {
		recordSuccessfulStderr(commandContext, boundedDiagnosticWithOmission(diagnosticStderr, stderrOmitted))
	}
	// Investigation of the pinned ordinary Restic and Kopia commands found no
	// evidence that they print passwords or keys. Their output therefore remains
	// native and engine-owned; Replicaro redacts only the explicitly public
	// support-report copy.
	if captured != nil {
		return diagnostic, nil
	}
	return raw, nil
}

func classifyPrivateFailure(stderr string) PrivateFailureKind {
	lower := strings.ToLower(stderr)
	if strings.Contains(lower, "failed to authenticate decrypted block - bad password?") {
		return PrivateFailureDecryptAuthentication
	}
	for _, marker := range []string{"credential", "authentication", "authorization", "access denied", "permission denied", "unauthorized", "forbidden", "private key", "certificate"} {
		if strings.Contains(lower, marker) {
			return PrivateFailureUnknown
		}
	}
	for _, marker := range []string{"is a directory", "not a regular file", "can't open directory", "cannot open directory"} {
		if strings.Contains(lower, marker) {
			return PrivateFailureObjectIsDirectory
		}
	}
	for _, marker := range []string{"directory not found", "object not found", "file not found", "path not found", "doesn't exist", "does not exist"} {
		if strings.Contains(lower, marker) {
			return PrivateFailureMissingObject
		}
	}
	return PrivateFailureUnknown
}

const maximumObservedStderrLineBytes = 16 << 10
const maximumLiveRecordBytes = 16 << 10
const omittedLiveRecord = "[Overlong live output omitted]"

func liveOutputObserver(ctx context.Context) LiveOutputObserver {
	enabled, _ := ctx.Value(liveOutputEnabledKey{}).(bool)
	observer, _ := ctx.Value(liveOutputKey{}).(LiveOutputObserver)
	if !enabled || observer == nil {
		return nil
	}
	return observer
}

type liveRecordWriter struct {
	stream     string
	observer   LiveOutputObserver
	record     []byte
	discarding bool
	afterCR    bool
}

func newLiveRecordWriter(stream string, observer LiveOutputObserver) *liveRecordWriter {
	return &liveRecordWriter{stream: stream, observer: observer}
}

func (writer *liveRecordWriter) Write(value []byte) (int, error) {
	written := len(value)
	for _, current := range value {
		if writer.afterCR {
			writer.afterCR = false
			if current == '\n' {
				continue
			}
		}
		switch current {
		case '\r':
			writer.emit()
			writer.afterCR = true
		case '\n':
			writer.emit()
		default:
			if writer.discarding {
				continue
			}
			if len(writer.record) >= maximumLiveRecordBytes {
				writer.record = nil
				writer.discarding = true
				continue
			}
			writer.record = append(writer.record, current)
		}
	}
	return written, nil
}

func (writer *liveRecordWriter) emit() {
	if writer.discarding {
		writer.observe(omittedLiveRecord)
		writer.discarding = false
		writer.record = nil
		return
	}
	if len(writer.record) == 0 {
		return
	}
	text := string(writer.record)
	writer.record = nil
	writer.observe(text)
}

func (writer *liveRecordWriter) observe(text string) {
	// The operation runtime manager is the only production observer and performs
	// one bounded in-memory append. Calling it directly avoids a second queue
	// whose saturation could silently discard an otherwise complete record.
	// Presentation failures remain isolated from native parser bytes and result
	// truth.
	defer func() { _ = recover() }()
	writer.observer(writer.stream, text)
}

func (writer *liveRecordWriter) flush() {
	writer.afterCR = false
	writer.emit()
}

type privateStderrObserver struct {
	observe  func([]byte) error
	cancel   context.CancelFunc
	line     []byte
	observed int
	mu       sync.Mutex
	err      error
}

func (observer *privateStderrObserver) Write(value []byte) (int, error) {
	written := len(value)
	for len(value) > 0 {
		newline := bytes.IndexByte(value, '\n')
		if newline < 0 {
			observer.append(value)
			break
		}
		observer.append(value[:newline+1])
		observer.emit()
		value = value[newline+1:]
	}
	return written, nil
}

func (observer *privateStderrObserver) append(value []byte) {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	if observer.err != nil {
		return
	}
	if len(value) > maxPrivateObservedStderrBytes-observer.observed {
		observer.err = fmt.Errorf("observed private stderr exceeded %d bytes", maxPrivateObservedStderrBytes)
		observer.cancel()
		return
	}
	observer.observed += len(value)
	if len(observer.line)+len(value) > maximumObservedStderrLineBytes {
		observer.err = fmt.Errorf("observed private stderr line exceeded %d bytes", maximumObservedStderrLineBytes)
		observer.cancel()
		return
	}
	observer.line = append(observer.line, value...)
}

func (observer *privateStderrObserver) emit() {
	observer.mu.Lock()
	if observer.err != nil || len(observer.line) == 0 {
		observer.line = nil
		observer.mu.Unlock()
		return
	}
	line := append([]byte(nil), observer.line...)
	observer.line = nil
	observer.mu.Unlock()

	err := observer.observe(line)
	if err != nil {
		observer.mu.Lock()
		if observer.err == nil {
			observer.err = err
			observer.cancel()
		}
		observer.mu.Unlock()
		return
	}
}

func (observer *privateStderrObserver) flush() { observer.emit() }

func (observer *privateStderrObserver) errValue() error {
	observer.mu.Lock()
	defer observer.mu.Unlock()
	return observer.err
}

type cappedDiagnosticBuffer struct {
	buffer    bytes.Buffer
	limit     int
	truncated bool
}

type headTailDiagnosticBuffer struct {
	limit     int
	head      []byte
	tail      []byte
	total     int64
	truncated bool
}

func newHeadTailDiagnosticBuffer(limit int) *headTailDiagnosticBuffer {
	return &headTailDiagnosticBuffer{limit: limit}
}

func (buffer *headTailDiagnosticBuffer) Write(value []byte) (int, error) {
	written := len(value)
	buffer.total += int64(written)
	if buffer.limit <= 0 {
		buffer.truncated = buffer.truncated || written > 0
		return written, nil
	}
	headLimit := buffer.limit / 2
	tailLimit := buffer.limit - headLimit
	if len(buffer.head) < headLimit {
		count := min(headLimit-len(buffer.head), len(value))
		buffer.head = append(buffer.head, value[:count]...)
		value = value[count:]
	}
	if len(value) > 0 {
		if len(value) >= tailLimit {
			buffer.tail = append(buffer.tail[:0], value[len(value)-tailLimit:]...)
		} else {
			needed := len(buffer.tail) + len(value) - tailLimit
			if needed > 0 {
				buffer.tail = append(buffer.tail[:0], buffer.tail[needed:]...)
			}
			buffer.tail = append(buffer.tail, value...)
		}
	}
	buffer.truncated = buffer.total > int64(buffer.limit)
	return written, nil
}

func (buffer *headTailDiagnosticBuffer) Len() int {
	if buffer.total > int64(buffer.limit) {
		return buffer.limit
	}
	return int(buffer.total)
}

func (buffer *headTailDiagnosticBuffer) String() string {
	return string(buffer.head) + string(buffer.tail)
}

func (buffer *headTailDiagnosticBuffer) Parts() (head, tail string) {
	return string(buffer.head), string(buffer.tail)
}

func newCappedDiagnosticBuffer(limit int) *cappedDiagnosticBuffer {
	return &cappedDiagnosticBuffer{limit: limit}
}

func (buffer *cappedDiagnosticBuffer) Write(value []byte) (int, error) {
	written := len(value)
	remaining := buffer.limit - buffer.buffer.Len()
	if remaining <= 0 {
		buffer.truncated = true
		return written, nil
	}
	if len(value) > remaining {
		value = value[:remaining]
		buffer.truncated = true
	}
	_, _ = buffer.buffer.Write(value)
	return written, nil
}

func (buffer *cappedDiagnosticBuffer) Len() int { return buffer.buffer.Len() }

func (buffer *cappedDiagnosticBuffer) String() string {
	value := buffer.buffer.String()
	if buffer.truncated {
		value += "\n[diagnostic output truncated]"
	}
	return value
}

func boundedDiagnostic(value string) string {
	return boundedDiagnosticWithOmission(value, false)
}

const diagnosticTruncationLabel = "[diagnostic output truncated]"
const diagnosticTruncationMarker = "\n" + diagnosticTruncationLabel + "\n"

func boundedDiagnosticWithOmission(value string, omitted bool) string {
	return boundedDiagnosticWithOmissionLimit(value, omitted, maxDiagnosticBytes)
}

func boundedDiagnosticWithOmissionLimit(value string, omitted bool, limit int) string {
	if !omitted && len(value) <= limit {
		return value
	}
	// Native output may contain marker-like text. Once Replicaro has omitted
	// diagnostic input, retain exactly one unambiguous wrapper marker.
	value = strings.ReplaceAll(value, diagnosticTruncationLabel, "[diagnostic truncation text]")
	remaining := limit - len(diagnosticTruncationMarker)
	if len(value) > remaining {
		head := remaining / 2
		tail := remaining - head
		return value[:head] + diagnosticTruncationMarker + value[len(value)-tail:]
	}
	middle := len(value) / 2
	return value[:middle] + diagnosticTruncationMarker + value[middle:]
}

func minimalEnvironment(overrides []string) []string {
	allowed := map[string]bool{}
	for _, name := range []string{"PATH", "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "TEMP", "TMP", "SystemRoot", "WINDIR", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "OS", "LANG", "LC_ALL", "TERM"} {
		allowed[strings.ToUpper(name)] = true
	}
	values := map[string]string{}
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok && allowed[strings.ToUpper(key)] {
			values[key] = value
		}
	}
	for _, item := range overrides {
		key, value, ok := strings.Cut(item, "=")
		if ok && key != "" {
			values[key] = value
		}
	}
	result := make([]string, 0, len(values))
	for key, value := range values {
		result = append(result, key+"="+value)
	}
	return result
}
