//go:build !darwin

package command

import (
	"errors"
	"os/exec"
)

func (tree *processTree) handleTerminateFailure(_ *exec.Cmd) (bool, error) {
	return false, nil
}

func (tree *processTree) prepareReapAfterCleanupFailure(_ *exec.Cmd) (bool, bool, error) {
	return false, true, nil
}

func (tree *processTree) joinCleanupResult(processErr, cleanupErr, contextErr error) error {
	if contextErr != nil {
		return contextErr
	}
	return errors.Join(processErr, cleanupErr)
}
