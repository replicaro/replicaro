//go:build darwin

package command

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
)

func (tree *processTree) handleTerminateFailure(process *exec.Cmd) (bool, error) {
	return true, killExactDarwinProcess(process)
}

func (tree *processTree) prepareReapAfterCleanupFailure(process *exec.Cmd) (bool, bool, error) {
	err := killExactDarwinProcess(process)
	return true, err == nil, err
}

func (tree *processTree) joinCleanupResult(processErr, cleanupErr, contextErr error) error {
	if contextErr != nil {
		return errors.Join(contextErr, cleanupErr)
	}
	return errors.Join(processErr, cleanupErr)
}

func killExactDarwinProcess(process *exec.Cmd) error {
	if process == nil || process.Process == nil || process.ProcessState != nil {
		return nil
	}
	if err := process.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("terminate exact engine process: %w", err)
	}
	return nil
}
