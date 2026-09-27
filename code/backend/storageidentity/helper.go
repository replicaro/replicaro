package storageidentity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"time"
)

const (
	DefaultMaxInput  = 64 << 10
	DefaultMaxOutput = 256 << 10
)

var ErrHelperStart = errors.New("storage helper process could not start")

// ServeHelper reads one probe request, performs it, and writes one response.
// Storage that cannot be observed is a normal response (access "missing",
// "denied", or "failed" with a step label and OS code). Only an invalid request
// or an oversized response is an error, which makes the helper exit non-zero
// so the parent reports a protocol failure instead of guessing.
func ServeHelper(reader io.Reader, writer io.Writer, maxInput, maxOutput int64) error {
	if maxInput <= 0 {
		maxInput = DefaultMaxInput
	}
	if maxOutput <= 0 {
		maxOutput = DefaultMaxOutput
	}
	requestBytes, err := readBounded(reader, maxInput)
	if err != nil {
		return err
	}
	var request HelperRequest
	if err := decodeStrict(requestBytes, &request); err != nil {
		return fmt.Errorf("decode storage helper request: %w", err)
	}
	response, err := ExecuteRequest(request)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(response)
	if err != nil {
		return err
	}
	if int64(len(encoded)) > maxOutput {
		return fmt.Errorf("storage helper output exceeds limit")
	}
	if _, err := writer.Write(encoded); err != nil {
		return fmt.Errorf("write storage helper response: %w", err)
	}
	return nil
}

type ProcessRunner struct {
	Executable string
	Args       []string
	Timeout    time.Duration
	MaxInput   int64
	MaxOutput  int64
	live       chan struct{}
	newProcess func([]byte, io.Writer, io.Writer) helperProcess
}

func NewProcessRunner(executable string, args []string, timeout time.Duration, maxConcurrent int) (*ProcessRunner, error) {
	if executable == "" || timeout <= 0 || maxConcurrent <= 0 {
		return nil, fmt.Errorf("helper executable, timeout, and concurrency are required")
	}
	runner := &ProcessRunner{
		Executable: executable, Args: append([]string(nil), args...), Timeout: timeout,
		MaxInput: DefaultMaxInput, MaxOutput: DefaultMaxOutput,
		live: make(chan struct{}, maxConcurrent),
	}
	runner.newProcess = func(input []byte, stdout, stderr io.Writer) helperProcess {
		command := exec.Command(runner.Executable, runner.Args...)
		configureStorageHelperCommand(command)
		command.Stdin = bytes.NewReader(input)
		command.Stdout, command.Stderr = stdout, stderr
		return execHelperProcess{command: command}
	}
	return runner, nil
}

func (runner *ProcessRunner) Run(ctx context.Context, request HelperRequest) (HelperResponse, error) {
	if runner == nil || runner.live == nil || runner.newProcess == nil {
		return HelperResponse{}, fmt.Errorf("storage helper runner is not initialized")
	}
	if err := validateHelperRequest(request); err != nil {
		return HelperResponse{}, err
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return HelperResponse{}, err
	}
	maxInput := runner.MaxInput
	if maxInput <= 0 {
		maxInput = DefaultMaxInput
	}
	if int64(len(encoded)) > maxInput {
		return HelperResponse{}, fmt.Errorf("storage helper input exceeds limit")
	}
	deadlineContext, cancel := context.WithTimeout(ctx, runner.Timeout)
	defer cancel()
	if err := deadlineContext.Err(); err != nil {
		return HelperResponse{}, err
	}
	select {
	case runner.live <- struct{}{}:
	case <-deadlineContext.Done():
		return HelperResponse{}, deadlineContext.Err()
	}
	maxOutput := runner.MaxOutput
	if maxOutput <= 0 {
		maxOutput = DefaultMaxOutput
	}
	stdout := &boundedBuffer{limit: maxOutput}
	stderr := &boundedBuffer{limit: maxOutput}
	process := runner.newProcess(encoded, stdout, stderr)
	if process == nil {
		<-runner.live
		return HelperResponse{}, fmt.Errorf("create storage helper process")
	}
	if err := process.Start(); err != nil {
		<-runner.live
		return HelperResponse{}, fmt.Errorf("%w: %w", ErrHelperStart, err)
	}
	waited := make(chan error, 1)
	go func() {
		err := process.Wait()
		<-runner.live
		waited <- err
	}()
	select {
	case err := <-waited:
		if err != nil {
			if errors.Is(stdout.err, errLimitExceeded) || errors.Is(stderr.err, errLimitExceeded) {
				return HelperResponse{}, fmt.Errorf("storage helper output exceeds limit")
			}
			return HelperResponse{}, fmt.Errorf("storage helper failed")
		}
	case <-deadlineContext.Done():
		_ = process.Kill()
		// Wait continues in the bounded live-helper goroutine. The caller
		// returns at its deadline, while the live slot remains held until the
		// child is eventually reaped. This bounds both callers and unreaped
		// processes even if an OS Wait implementation wedges after Kill.
		return HelperResponse{}, deadlineContext.Err()
	}
	if stdout.err != nil || stderr.err != nil {
		return HelperResponse{}, fmt.Errorf("storage helper output exceeds limit")
	}
	var response HelperResponse
	if err := decodeStrict(stdout.Bytes(), &response); err != nil {
		return HelperResponse{}, fmt.Errorf("decode storage helper response: %w", err)
	}
	if err := ValidateHelperResponse(request, response); err != nil {
		return HelperResponse{}, err
	}
	return response, nil
}

type helperProcess interface {
	Start() error
	Wait() error
	Kill() error
}

type execHelperProcess struct {
	command *exec.Cmd
}

func (process execHelperProcess) Start() error { return process.command.Start() }
func (process execHelperProcess) Wait() error  { return process.command.Wait() }
func (process execHelperProcess) Kill() error  { return process.command.Process.Kill() }

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("storage helper input exceeds limit")
	}
	return data, nil
}

func decodeStrict(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

var errLimitExceeded = errors.New("bounded output exceeded")

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int64
	err    error
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	if buffer.err != nil {
		return 0, buffer.err
	}
	remaining := buffer.limit - int64(buffer.buffer.Len())
	if remaining <= 0 || int64(len(value)) > remaining {
		if remaining > 0 {
			_, _ = buffer.buffer.Write(value[:remaining])
		}
		buffer.err = errLimitExceeded
		if remaining < 0 {
			remaining = 0
		}
		return int(remaining), buffer.err
	}
	return buffer.buffer.Write(value)
}

func (buffer *boundedBuffer) Bytes() []byte { return buffer.buffer.Bytes() }
