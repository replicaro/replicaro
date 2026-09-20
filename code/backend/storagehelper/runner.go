package storagehelper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/local/replicaro/storageidentity"
)

// Runner materializes and invokes the embedded helper without falling back to
// the GUI executable or any PATH-resolved program.
type Runner struct {
	timeout       time.Duration
	maxConcurrent int

	mu          sync.Mutex
	process     *storageidentity.ProcessRunner
	processPath string

	materialize func() (string, error)
	runProcess  func(*storageidentity.ProcessRunner, context.Context, storageidentity.HelperRequest) (storageidentity.HelperResponse, error)
}

func NewRunner(timeout time.Duration, maxConcurrent int) (*Runner, error) {
	if timeout <= 0 || maxConcurrent <= 0 {
		return nil, fmt.Errorf("storage helper timeout and concurrency are required")
	}
	return &Runner{
		timeout:       timeout,
		maxConcurrent: maxConcurrent,
		materialize:   Materialize,
		runProcess: func(process *storageidentity.ProcessRunner, ctx context.Context, request storageidentity.HelperRequest) (storageidentity.HelperResponse, error) {
			return process.Run(ctx, request)
		},
	}, nil
}

// Observe implements storageavailability.Observer without coupling this
// component package to admission or orchestration state.
func (runner *Runner) Observe(ctx context.Context, request storageidentity.HelperRequest) (storageidentity.HelperResponse, error) {
	return runner.Run(ctx, request)
}

func (runner *Runner) Run(ctx context.Context, request storageidentity.HelperRequest) (storageidentity.HelperResponse, error) {
	process, err := runner.prepare()
	if err != nil {
		return storageidentity.HelperResponse{}, err
	}
	response, err := runner.runProcess(process, ctx, request)
	if !isDisappearance(err) {
		if errors.Is(err, storageidentity.ErrHelperStart) {
			return response, componentError(err)
		}
		return response, err
	}

	// A helper that disappears after verified materialization is recreated and
	// launched exactly once more. Every other failure remains fail-closed.
	process, rematerializeErr := runner.prepare()
	if rematerializeErr != nil {
		return storageidentity.HelperResponse{}, rematerializeErr
	}
	response, err = runner.runProcess(process, ctx, request)
	if err != nil {
		return response, componentError(fmt.Errorf("launch failed after one recreation: %w", err))
	}
	return response, nil
}

func (runner *Runner) prepare() (*storageidentity.ProcessRunner, error) {
	if runner == nil || runner.materialize == nil || runner.runProcess == nil {
		return nil, componentError(fmt.Errorf("runner is not initialized"))
	}
	path, err := runner.materialize()
	if err != nil {
		return nil, err
	}
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if runner.process == nil || runner.processPath != path {
		process, err := storageidentity.NewProcessRunner(path, nil, runner.timeout, runner.maxConcurrent)
		if err != nil {
			return nil, componentError(err)
		}
		runner.process = process
		runner.processPath = path
	}
	return runner.process, nil
}

func isDisappearance(err error) bool {
	return errors.Is(err, storageidentity.ErrHelperStart) &&
		errors.Is(err, os.ErrNotExist)
}
