package storagehelper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/local/replicaro/storageidentity"
)

// Runner materializes and invokes the embedded helper without falling back to
// the GUI executable or any PATH-resolved program.
//
// The full materialization runs on first use and whenever the cached helper
// can no longer be trusted. Between those, a probe only checks that the cached
// executable is still a safe regular file with the embedded checksum, because
// the full procedure (directory checks and permission repair under a global
// lock) is far too slow to repeat for every probe when many backups start at
// once.
type Runner struct {
	timeout       time.Duration
	maxConcurrent int

	// mu serializes full preparation and guards process and processPath.
	mu          sync.Mutex
	process     *storageidentity.ProcessRunner
	processPath string

	// current is the last successful full preparation, or nil when the next
	// probe must run the full procedure. Every successful preparation stores a
	// new entry, so a probe can tell whether the entry it saw fail has since
	// been replaced.
	current atomic.Pointer[preparedHelper]

	materialize func() (string, error)
	runProcess  func(*storageidentity.ProcessRunner, context.Context, storageidentity.HelperRequest) (storageidentity.HelperResponse, error)
}

type preparedHelper struct {
	path    string
	process *storageidentity.ProcessRunner
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
	helper, err := runner.prepare()
	if err != nil {
		return storageidentity.HelperResponse{}, err
	}
	response, err := runner.runProcess(helper.process, ctx, request)
	if !isDisappearance(err) {
		if errors.Is(err, storageidentity.ErrHelperStart) {
			runner.forget(helper)
			return response, componentError(err)
		}
		return response, err
	}

	// A helper that disappears after verified materialization is recreated and
	// launched exactly once more. Every other failure remains fail-closed.
	helper, rematerializeErr := runner.prepareFull(helper)
	if rematerializeErr != nil {
		return storageidentity.HelperResponse{}, rematerializeErr
	}
	response, err = runner.runProcess(helper.process, ctx, request)
	if err != nil {
		if errors.Is(err, storageidentity.ErrHelperStart) {
			runner.forget(helper)
		}
		return response, componentError(fmt.Errorf("launch failed after one recreation: %w", err))
	}
	return response, nil
}

func (runner *Runner) prepare() (*preparedHelper, error) {
	if runner == nil || runner.materialize == nil || runner.runProcess == nil {
		return nil, componentError(fmt.Errorf("runner is not initialized"))
	}
	helper := runner.current.Load()
	if helper != nil && installedHelperMatches(helper.path) {
		return helper, nil
	}
	return runner.prepareFull(helper)
}

// prepareFull runs the complete materialization. stale is the cached entry the
// caller found unusable, or nil when the cache was empty. If another probe
// stored a different entry while this one waited for the lock, that entry is
// used after the same installed-file check as the fast path, so a burst of
// probes prepares the helper once. A stale entry never satisfies its own
// refresh, and a newer entry whose file has changed again is prepared afresh.
func (runner *Runner) prepareFull(stale *preparedHelper) (*preparedHelper, error) {
	runner.mu.Lock()
	defer runner.mu.Unlock()
	if current := runner.current.Load(); current != nil && current != stale && installedHelperMatches(current.path) {
		return current, nil
	}
	// Clear the entry first so a failed preparation leaves nothing cached.
	runner.current.Store(nil)
	path, err := runner.materialize()
	if err != nil {
		return nil, err
	}
	// Keep the same process runner while the path is unchanged: helpers that
	// are still being killed hold slots in it and must keep counting against
	// the concurrency limit.
	if runner.process == nil || runner.processPath != path {
		process, err := storageidentity.NewProcessRunner(path, nil, runner.timeout, runner.maxConcurrent)
		if err != nil {
			return nil, componentError(err)
		}
		runner.process = process
		runner.processPath = path
	}
	helper := &preparedHelper{path: path, process: runner.process}
	runner.current.Store(helper)
	return helper, nil
}

// forget drops helper from the cache after a start failure so the next probe
// runs the full preparation, which also repairs problems the checksum cannot
// see, such as a lost execute permission. A newer entry is left alone.
func (runner *Runner) forget(helper *preparedHelper) {
	runner.current.CompareAndSwap(helper, nil)
}

// installedHelperMatches is the per-launch check for an already prepared
// helper: the path must still be a safe regular file with the embedded bytes.
// It takes no lock and writes nothing.
func installedHelperMatches(path string) bool {
	_, expected, err := embeddedComponent()
	return err == nil && verifyExecutable(path, expected) == nil
}

func isDisappearance(err error) bool {
	return errors.Is(err, storageidentity.ErrHelperStart) &&
		errors.Is(err, os.ErrNotExist)
}
