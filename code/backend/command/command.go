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
	"regexp"
	"slices"
	"sort"
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
	PrivateFailureSSHHostKeyUnknown
	PrivateFailureSSHHostKeyChanged
	// PrivateFailureStorageFull is the vault reporting that it ran out of
	// space, by the markers the caller passed with
	// ContextWithStorageFullMarkers. It is only used to explain a failure;
	// it never changes how the failure is retried.
	PrivateFailureStorageFull
	// PrivateFailureSSHAuthentication is an SFTP server turning down key
	// sign-in: publickey was among the methods tried and none of them
	// worked. A wrong password on a password-only remote is not this kind.
	// Like the host-key kinds it only picks the help shown with the generic
	// error; the native text itself is still withheld.
	PrivateFailureSSHAuthentication
)

type PrivateOutputError struct {
	Kind  PrivateFailureKind
	Cause error
}

func (e *PrivateOutputError) Error() string { return "private native command failed" }
func (e *PrivateOutputError) Unwrap() error { return e.Cause }

// PrivateOutputFailureKind returns only the failure category that private
// rclone workflows need, with no output text. Native text is inspected inside
// this package and is never kept in the returned error chain.
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
type outlivesReplicaroKey struct{}
type privateFailureRedactionsKey struct{}
type storageFullMarkersKey struct{}

// ContextWithPrivateFailureRedactions names text that private-output commands
// started with the returned context remove from their stderr before it is
// classified (see classifyPrivateFailure). Callers pass the vault's own path:
// rclone quotes it in its errors, and a folder called "Permission denied" or
// "not found" would otherwise decide the category.
// The values are matched case-insensitively and never leave this package.
func ContextWithPrivateFailureRedactions(ctx context.Context, values []string) context.Context {
	return context.WithValue(ctx, privateFailureRedactionsKey{}, append([]string(nil), values...))
}

// ContextWithStorageFullMarkers names the out-of-space wording that the
// private-output commands started with the returned context look for, after
// the redactions above are taken out. The caller passes the markers for the
// vault's connector from the one shared list (engines.StorageFullMarkers), so
// this package never needs to know which connector says what. Without
// markers the storage-full category is never returned.
func ContextWithStorageFullMarkers(ctx context.Context, markers []string) context.Context {
	return context.WithValue(ctx, storageFullMarkersKey{}, append([]string(nil), markers...))
}

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

// ContextOutlivingReplicaro lets the native children started with the
// returned context keep running when Replicaro ends without stopping them (a
// crash or a SIGKILL), instead of getting the Linux parent-death signal.
// Cancellation and process-tree cleanup still apply while Replicaro runs. Use
// it only for a command that makes several writes the repository can't open
// between, where finishing on its own is safer than being cut off midway.
func ContextOutlivingReplicaro(ctx context.Context) context.Context {
	return context.WithValue(ctx, outlivesReplicaroKey{}, true)
}

// OutlivesReplicaro reports whether ctx came from ContextOutlivingReplicaro.
func OutlivesReplicaro(ctx context.Context) bool {
	outlives, _ := ctx.Value(outlivesReplicaroKey{}).(bool)
	return outlives
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
	return runWithInput(ctx, path, args, childProcess{env: nativeEnvironment(env)}, "", timeout, engine, false, false, nil, nil)
}

// RunCaptured drains the complete stdout and stderr into owner-local files.
// Only a bounded diagnostic excerpt stays in memory, and total output size
// never changes the native process result. The caller must Close the returned
// capture after parsing it.
func RunCaptured(ctx context.Context, path string, args, env []string, timeout time.Duration, engine string) (*CapturedOutput, string, error) {
	capture, err := newCapturedOutput()
	if err != nil {
		return nil, "", &Error{Engine: engine, Err: err}
	}
	diagnostic, runErr := runWithInput(ctx, path, args, childProcess{env: nativeEnvironment(env)}, "", timeout, engine, false, false, nil, capture)
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

// RunWithInputSecretsPrivateOutput is for commands whose standard output is
// sensitive data. On failure, neither output stream is returned or included in
// the public error.
func RunWithInputSecretsPrivateOutput(ctx context.Context, path string, args, env []string, input string, timeout time.Duration, engine string) (string, error) {
	return runWithInput(ctx, path, args, childProcess{env: nativeEnvironment(env)}, input, timeout, engine, true, false, nil, nil)
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
	return runWithInput(ctx, path, args, childProcess{env: nativeEnvironment(env)}, input, timeout, engine, true, false, observe, nil)
}

// RunDiscardOutput drains stdout and stderr without retaining either stream.
// It is for current-user hooks whose arbitrary output is private and is never
// presented. The hook runs in dir with all of Replicaro's own environment, so
// it sees what it would see when the user runs it (USER, ProgramFiles, XDG_*
// and so on). Replicaro never puts a vault password, key or provider
// credential in its own environment; those go only into the overrides of the
// engine and rclone children that need them.
//
// Process-tree cleanup and cancellation work as for any other command, so the
// hook's processes are ended on cancellation and as soon as the hook exits. A
// process that leaves the hook's process group (Linux) or session (macOS), for
// example with setsid, or one started through a service manager, isn't ended.
// It doesn't hold up the hook step either: once the hook exits, Replicaro
// closes its end of the hook's output pipes, so such a process's next write
// to them raises SIGPIPE and ends it unless it redirects its output.
func RunDiscardOutput(ctx context.Context, path string, args []string, dir string, engine string) error {
	// Leave env nil rather than passing os.Environ(): os/exec sets PWD to dir
	// only for an inherited environment, and a hook that isn't a shell script
	// (Python, Node, Perl) would otherwise read Replicaro's own PWD.
	_, err := runWithInput(ctx, path, args, childProcess{dir: dir}, "", NoTotalDeadline, engine, true, true, nil, nil)
	return err
}

// childProcess is what a child gets besides its arguments: its environment
// (nil inherits all of Replicaro's) and its working directory (empty keeps
// Replicaro's). Engine and rclone children always get a non-nil environment
// from nativeEnvironment.
type childProcess struct {
	env []string
	dir string
}

func runWithInput(ctx context.Context, path string, args []string, child childProcess, input string, timeout time.Duration, engine string, privateOutput, discardOutput bool, observeStderr func([]byte) error, captured *CapturedOutput) (string, error) {
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
	process.Env = child.env
	process.Dir = child.dir
	if fence := nativeProcessFenceFromContext(ctx); fence != nil && fence.file != nil {
		process.ExtraFiles = append(process.ExtraFiles, fence.file)
	}
	if input != "" {
		process.Stdin = strings.NewReader(input)
	}
	configureWindowsCommand(process)
	if !OutlivesReplicaro(ctx) {
		endWithReplicaro(process)
	}
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
			// Raw buffers stay first in each tee. The engine's output is kept as-is; the
			// live writers only split it into bounded, complete records for display.
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
	// Held until this function has waited for the child; see
	// lockStartingThread.
	defer lockStartingThread()()
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
	go func() {
		defer close(readDone)
		var copies sync.WaitGroup
		copies.Add(2)
		go func() {
			defer copies.Done()
			_, _ = io.Copy(stdoutDestination, stdoutPipe)
			if liveStdout != nil {
				liveStdout.flush()
			}
		}()
		go func() {
			defer copies.Done()
			_, _ = io.Copy(stderrDestination, stderrPipe)
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
			_ = stdoutPipe.Close()
			_ = stderrPipe.Close()
		}
	}
	// On Unix waitid intentionally left the leader unreaped while the process
	// group was cleaned. Reap only afterward so the top-level PID/PGID remains
	// anchored throughout cleanup. The Darwin same-user secondary-PGID reuse
	// limitation is a deliberately accepted one; see terminatePlatform.
	if !waitReaped {
		canReap := true
		if cleanupErr != nil {
			if handled, platformCanReap, fallbackErr := tree.prepareReapAfterCleanupFailure(process); handled {
				cleanupErr = errors.Join(cleanupErr, fallbackErr)
				canReap = platformCanReap
			}
		}
		if canReap {
			err = errors.Join(err, tree.reap(process))
		}
	}
	// The read handles are parent-owned, so Wait cannot close them before all
	// buffered output is consumed. Descendant cleanup above guarantees EOF even
	// when a grandchild inherited the write descriptors, as long as it stayed in
	// the process tree.
	// Capture writes are part of pipe drainage, so a fixed post-exit timer here
	// would truncate a healthy command merely because its owner-local filesystem
	// was slow. Process-tree cleanup above closes leaked descendant writers; once
	// that boundary is complete, wait for both exact streams without a byte- or
	// elapsed-time cap.
	if discardOutput {
		// A hook can start a process that leaves its process group or session
		// (setsid, a daemon) and still holds the hook's stdout or stderr. Cleanup
		// can't reach that process, so waiting for EOF would hold the hook step,
		// and with it Cancel and Quit, until it exits. Nothing is kept from this
		// output, so stop reading once the hook itself is done; a later write
		// from the escaped process gets a broken pipe.
		_ = stdoutPipe.Close()
		_ = stderrPipe.Close()
	}
	<-readDone
	_ = stdoutPipe.Close()
	_ = stderrPipe.Close()
	err = tree.joinCleanupResult(err, cleanupErr, commandContext.Err())
	var outputErr error
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
			redactions, _ := commandContext.Value(privateFailureRedactionsKey{}).([]string)
			storageFull, _ := commandContext.Value(storageFullMarkersKey{}).([]string)
			return "", &PrivateOutputError{Kind: classifyPrivateFailure(redactPrivateFailureText(stderr.String(), redactions), storageFull...), Cause: cause}
		}
		return diagnostic, commandError(engine, diagnostic, err, outputErr)
	}
	// Successful stderr capture feeds ordinary user-facing diagnostics only.
	// Private and discarded output must not cross that presentation boundary.
	if !privateOutput && !discardOutput {
		recordSuccessfulStderr(commandContext, boundedDiagnosticWithOmission(diagnosticStderr, stderrOmitted))
	}
	// We have found nothing in the normal (non-private) pinned Restic and Kopia
	// commands that prints passwords or keys, so their output is returned
	// unmodified. Only the public support-report copy is redacted.
	if captured != nil {
		return diagnostic, nil
	}
	return raw, nil
}

// redactPrivateFailureText lowercases stderr and replaces each redaction with
// NUL, longest first so a shorter value can't split a longer one. No marker
// contains NUL, so the replacement can neither join the text around it into a
// new marker nor hide a marker rclone wrote outside the redacted value.
func redactPrivateFailureText(stderr string, redactions []string) string {
	text := strings.ToLower(stderr)
	values := make([]string, 0, len(redactions))
	for _, value := range redactions {
		if value = strings.ToLower(value); value != "" {
			values = append(values, value)
		}
	}
	sort.Slice(values, func(i, j int) bool { return len(values[i]) > len(values[j]) })
	for _, value := range values {
		text = strings.ReplaceAll(text, value, "\x00")
	}
	return text
}

var (
	openSSHPermissionDenied = regexp.MustCompile(`permission denied \(([a-z0-9@.,_-]+)\)`)
	goSSHAttemptedMethods   = regexp.MustCompile(`ssh: unable to authenticate, attempted methods \[([^\]]*)\]`)
)

// SSHKeySignInRefused reports whether SSH client output says the server
// turned down key sign-in. OpenSSH prints the methods the server still allows,
// as in "Permission denied (password,publickey).", and Go's x/crypto/ssh
// (rclone and Kopia's own SFTP client) prints the methods it tried, as in
// "ssh: unable to authenticate, attempted methods [none publickey], no
// supported methods remain". Either only counts when publickey is in the
// list: an Any Rclone Remote SFTP remote with a wrong password fails with
// "[none password]", and pointing that user at their SSH key would be wrong.
func SSHKeySignInRefused(text string) bool {
	lower := strings.ToLower(text)
	for _, match := range openSSHPermissionDenied.FindAllStringSubmatch(lower, -1) {
		if slices.Contains(strings.Split(match[1], ","), "publickey") {
			return true
		}
	}
	for _, match := range goSSHAttemptedMethods.FindAllStringSubmatch(lower, -1) {
		if slices.Contains(strings.Fields(match[1]), "publickey") {
			return true
		}
	}
	return false
}

func classifyPrivateFailure(stderr string, storageFullMarkers ...string) PrivateFailureKind {
	lower := strings.ToLower(stderr)
	if strings.Contains(lower, "failed to authenticate decrypted block - bad password?") {
		return PrivateFailureDecryptAuthentication
	}
	// rclone's SFTP backend refuses a server whose host key isn't in, or
	// differs from, the known_hosts file with these Go knownhosts errors.
	if strings.Contains(lower, "knownhosts: key mismatch") {
		return PrivateFailureSSHHostKeyChanged
	}
	if strings.Contains(lower, "knownhosts: key is unknown") {
		return PrivateFailureSSHHostKeyUnknown
	}
	// Check a refused key before the generic credential markers below so a
	// wrong key or username can get its own help. It comes after the host-key
	// checks: a host-key refusal ends the handshake before any key is offered.
	if SSHKeySignInRefused(lower) {
		return PrivateFailureSSHAuthentication
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
	// Out of space is checked last on purpose: when rclone's text also has
	// credential, missing-object or directory wording, that category decides,
	// because it changes what the caller does next and this one only adds an
	// explanation.
	for _, marker := range storageFullMarkers {
		if marker = strings.ToLower(marker); marker != "" && strings.Contains(lower, marker) {
			return PrivateFailureStorageFull
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
	// whose saturation could silently discard an otherwise complete record. The
	// recover keeps a presentation failure from affecting parser input or the
	// command result.
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

// minimalEnvironmentNames are the variables every engine and rclone child
// inherits from Replicaro's own environment, together with
// networkEnvironmentNames.
var minimalEnvironmentNames = []string{"PATH", "HOME", "USERPROFILE", "HOMEDRIVE", "HOMEPATH", "TEMP", "TMP", "SystemRoot", "WINDIR", "APPDATA", "LOCALAPPDATA", "PROGRAMDATA", "SYSTEMDRIVE", "COMSPEC", "PATHEXT", "OS", "LANG", "LC_ALL", "TERM"}

// networkEnvironmentNames let Restic, Kopia and rclone use the proxy and
// certificate environment variables, the same way they do when the user runs
// them directly. Names match in any letter case, so the lowercase proxy forms
// pass too. The certificate variables take effect on Linux and in Docker;
// macOS and Windows builds use the system trust store. Only these variables
// are read, not the Windows or macOS system proxy settings.
//
// They are kept apart from minimalEnvironmentNames on purpose: callers refuse
// the minimal names in variables a user enters, but an Any Rclone Remote vault
// may set its own proxy. A vault's value replaces the inherited one in every
// letter case, because Go-based tools read HTTPS_PROXY before https_proxy and
// would otherwise keep Replicaro's value whenever the cases differ.
var networkEnvironmentNames = []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"}

// timeZoneEnvironmentNames are inherited only in the Docker package, after
// InheritTimeZone. There TZ is how the user sets the container's time zone,
// and Restic, Kopia and rclone should use the same zone as Replicaro's
// schedules, its hooks and the image's own tools. Desktop builds never
// inherit it: their engines follow the system's time zone, as before.
//
// TZ is kept out of minimalEnvironmentNames for the same reason as the
// network names: an Any Rclone Remote vault refuses the minimal names, and it
// checks that on every use, not only when saved, so a vault saved with its
// own TZ would stop working. Here a vault's TZ replaces the inherited one.
var timeZoneEnvironmentNames = []string{"TZ"}

var inheritTimeZone atomic.Bool

// InheritTimeZone makes every later engine and rclone child inherit TZ from
// Replicaro's environment. main calls it once at startup, only after it has
// confirmed that it is running in the Docker package. Don't call it for a
// desktop build.
func InheritTimeZone() {
	inheritTimeZone.Store(true)
}

// MinimalEnvironmentNames returns the names a native child inherits from
// Replicaro's environment. Callers that accept variables from the user refuse
// these so the inherited values can't be replaced.
func MinimalEnvironmentNames() []string {
	return append([]string(nil), minimalEnvironmentNames...)
}

// ProxyEnvironmentValues returns Replicaro's own proxy settings, in every
// letter case that is set. Proxy URLs can carry a user name and password, so
// the support report redacts these values like any other credential.
func ProxyEnvironmentValues() []string {
	var values []string
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if !ok || value == "" {
			continue
		}
		switch strings.ToUpper(key) {
		case "HTTP_PROXY", "HTTPS_PROXY":
			values = append(values, value)
		}
	}
	return values
}

// nativeEnvironment is the complete environment of an engine or rclone child:
// the minimal and network names from Replicaro's environment (and TZ in the
// Docker package), then overrides, which win.
func nativeEnvironment(overrides []string) []string {
	minimal := map[string]bool{}
	for _, name := range minimalEnvironmentNames {
		minimal[strings.ToUpper(name)] = true
	}
	// The network names, and TZ when inherited, give way to an override.
	overridable := map[string]bool{}
	for _, name := range networkEnvironmentNames {
		overridable[name] = true
	}
	if inheritTimeZone.Load() {
		for _, name := range timeZoneEnvironmentNames {
			overridable[name] = true
		}
	}
	overridden := map[string]bool{}
	for _, item := range overrides {
		if key, _, ok := strings.Cut(item, "="); ok && key != "" {
			overridden[strings.ToUpper(key)] = true
		}
	}
	values := map[string]string{}
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		name := strings.ToUpper(key)
		if minimal[name] || overridable[name] && !overridden[name] {
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
