package storageidentity

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const (
	HelperVersion              = "storage_observer_v4"
	HelperPermissionDeniedCode = "permission_denied"
	DefaultMaxInput            = 64 << 10
	DefaultMaxOutput           = 256 << 10
)

var ErrHelperStart = errors.New("storage helper process could not start")

type HelperRequest struct {
	Version        string `json:"version"`
	Operation      string `json:"operation"`
	Connector      string `json:"connector"`
	Path           string `json:"path"`
	ObjectType     string `json:"object_type,omitempty"`
	SelectSpelling bool   `json:"select_spelling,omitempty"`
}

type HelperResponse struct {
	Version            string              `json:"version"`
	ConfiguredPath     string              `json:"configured_path,omitempty"`
	Descriptor         *Descriptor         `json:"descriptor,omitempty"`
	Key                string              `json:"key,omitempty"`
	ObservedFilesystem string              `json:"observed_filesystem,omitempty"`
	Filesystems        []MountedFilesystem `json:"filesystems,omitempty"`
	ErrorCode          string              `json:"error_code,omitempty"`
}

// MountedFilesystem is one currently mounted OS-authoritative filesystem
// root. It contains no credentials and is used only to construct bounded alias
// candidates which still require the caller's source identity check or complete
// native/protected destination proof.
type MountedFilesystem struct {
	Path       string     `json:"path"`
	Descriptor Descriptor `json:"descriptor"`
}

func ExecuteRequest(request HelperRequest) (HelperResponse, error) {
	if err := validateHelperRequest(request); err != nil {
		return HelperResponse{}, err
	}
	switch request.Operation {
	case "observe":
		observed, observedFilesystem, _, err := resolveExistingObservation(request.Path, request.ObjectType)
		if err != nil {
			return HelperResponse{}, err
		}
		response, err := observationResponse(observed, observedFilesystem)
		if err == nil && request.SelectSpelling {
			// Keep the v4 wire field for callers that still request it. Binding
			// preserves the validated route; directory enumeration could change a
			// UNC root and required a second, redundant observation.
			response.ConfiguredPath = request.Path
		}
		return response, err
	case "bind_parent":
		descriptor, err := ResolveRepositoryForCreation(request.Path)
		if err != nil {
			return HelperResponse{}, err
		}
		key, err := descriptor.CanonicalKey()
		if err != nil {
			return HelperResponse{}, err
		}
		response := HelperResponse{Version: HelperVersion, Descriptor: &descriptor, Key: key}
		if request.SelectSpelling {
			response.ConfiguredPath = request.Path
		}
		return response, nil
	case "enumerate":
		filesystems, err := EnumerateMountedFilesystems()
		if err != nil {
			return HelperResponse{}, err
		}
		return HelperResponse{Version: HelperVersion, Filesystems: filesystems}, nil
	default:
		return HelperResponse{}, fmt.Errorf("unsupported storage helper request")
	}
}

func observationResponse(observed Descriptor, observedFilesystem string) (HelperResponse, error) {
	descriptor := observed
	if strings.TrimSpace(observedFilesystem) == "" {
		observedFilesystem = observed.Filesystem
	}
	if normalized := strings.ToLower(strings.TrimSpace(observedFilesystem)); normalized == "" || normalized != observedFilesystem ||
		strings.IndexFunc(observedFilesystem, unicode.IsControl) >= 0 || len(observedFilesystem) > maxDescriptorFieldBytes {
		return HelperResponse{}, fmt.Errorf("observed filesystem type is invalid")
	}
	// The separate fact makes the current OS observation explicit. Path-only
	// bindings persist the same type for continuity, while stable Windows
	// network descriptors keep protocol-key semantics.
	response := HelperResponse{Version: HelperVersion, ObservedFilesystem: observedFilesystem}
	key, err := descriptor.CanonicalKey()
	if err != nil {
		return HelperResponse{}, err
	}
	response.Descriptor, response.Key = &descriptor, key
	return response, nil
}

// ObservationFilesystem returns the authoritative type for one exact helper
// observation. Ordinary descriptors persist that same type. Stable Windows
// network descriptors retain their protocol key and therefore require the
// explicit actual-type fact.
func (response HelperResponse) ObservationFilesystem() (string, error) {
	if response.Descriptor == nil {
		return "", fmt.Errorf("storage helper descriptor is missing")
	}
	value := response.ObservedFilesystem
	if value == "" {
		if (response.Descriptor.Kind == KindSMB || response.Descriptor.Kind == KindNFS) && response.Descriptor.Provider != "" {
			return "", fmt.Errorf("Windows network filesystem observation is missing")
		}
		value = response.Descriptor.Filesystem
	}
	normalized := strings.ToLower(strings.TrimSpace(value))
	if normalized == "" || normalized != value || strings.IndexFunc(value, unicode.IsControl) >= 0 || len(value) > maxDescriptorFieldBytes {
		return "", fmt.Errorf("observed filesystem type is invalid")
	}
	windowsNetwork := (response.Descriptor.Kind == KindSMB || response.Descriptor.Kind == KindNFS) && response.Descriptor.Provider != ""
	if windowsNetwork {
		kind, err := WindowsNetworkProviderKind(response.Descriptor.Provider)
		if err != nil || kind != response.Descriptor.Kind || response.Descriptor.Filesystem != string(response.Descriptor.Kind) {
			return "", fmt.Errorf("Windows network identity facts are inconsistent")
		}
	} else if value != response.Descriptor.Filesystem {
		// Two competing OS observations are unsafe at the operation boundary.
		return "", fmt.Errorf("filesystem observation contradicts descriptor")
	}
	return normalized, nil
}

func validateHelperRequest(request HelperRequest) error {
	if request.Version != HelperVersion || request.Operation != "observe" && request.Operation != "bind_parent" && request.Operation != "enumerate" {
		return fmt.Errorf("unsupported storage helper request")
	}
	if request.Connector != "fs" {
		return fmt.Errorf("only filesystem storage may be observed")
	}
	if (request.Operation == "observe" || request.Operation == "bind_parent") && request.Path == "" {
		return fmt.Errorf("storage path is required")
	}
	if request.Operation == "observe" && request.ObjectType != "" && request.ObjectType != "directory" && request.ObjectType != "file" {
		return fmt.Errorf("unsupported storage object type")
	}
	if request.Operation != "observe" && request.ObjectType != "" {
		return fmt.Errorf("storage object type is only valid for exact observation")
	}
	if request.SelectSpelling {
		configured, err := NormalizeConfiguredPath(request.Path)
		if err != nil || configured != request.Path {
			return fmt.Errorf("spelling selection requires a normalized configured path")
		}
	}
	if request.Operation == "enumerate" && (request.Path != "" || request.SelectSpelling) {
		return fmt.Errorf("enumeration does not accept a path")
	}
	return nil
}

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
		// Only actual OS inspection errors mean that this candidate could not
		// be inspected. Invalid or conflicting identity facts must survive the
		// process boundary as a conclusive failure, not apparent absence.
		code := helperErrorCode(err)
		response = HelperResponse{Version: HelperVersion, ErrorCode: code}
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

func helperErrorCode(err error) string {
	if !IsInspectionError(err) {
		return "observation_invalid"
	}
	var missing *MissingStorageError
	if errors.As(err, &missing) {
		return "storage_missing"
	}
	if errors.Is(err, syscall.EACCES) || errors.Is(err, syscall.EPERM) {
		return HelperPermissionDeniedCode
	}
	return "observation_failed"
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
	if response.Version != HelperVersion {
		return HelperResponse{}, fmt.Errorf("unsupported storage helper response")
	}
	if response.ErrorCode != "" {
		if response.Descriptor != nil || response.Key != "" || response.ConfiguredPath != "" ||
			response.ObservedFilesystem != "" || len(response.Filesystems) != 0 {
			return HelperResponse{}, fmt.Errorf("storage helper error response contains observation facts")
		}
		return response, fmt.Errorf("storage observation failed")
	}
	if _, err := response.BindingPath(request); err != nil {
		return HelperResponse{}, err
	}
	if request.Operation == "enumerate" {
		if response.Descriptor != nil || response.Key != "" || response.ObservedFilesystem != "" {
			return HelperResponse{}, fmt.Errorf("storage helper enumeration response is invalid")
		}
		for _, mounted := range response.Filesystems {
			if mounted.Path == "" {
				return HelperResponse{}, fmt.Errorf("storage helper enumeration path is empty")
			}
			if err := mounted.Descriptor.Validate(); err != nil {
				return HelperResponse{}, err
			}
		}
		return response, nil
	}
	if response.Descriptor == nil || response.Key == "" || len(response.Filesystems) != 0 {
		return HelperResponse{}, fmt.Errorf("storage helper response is incomplete")
	}
	if err := response.Descriptor.Validate(); err != nil {
		return HelperResponse{}, err
	}
	if request.Operation == "observe" {
		if _, err := response.ObservationFilesystem(); err != nil {
			return HelperResponse{}, err
		}
	} else if response.ObservedFilesystem != "" {
		return HelperResponse{}, fmt.Errorf("storage helper bind response contains observation-only facts")
	}
	key, err := response.Descriptor.CanonicalKey()
	if err != nil || key != response.Key {
		return HelperResponse{}, fmt.Errorf("storage helper identity key mismatch")
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
