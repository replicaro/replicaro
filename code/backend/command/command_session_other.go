//go:build !darwin && !windows

package command

import "os/exec"

func initializeProcessTreePlatform(_ *processTree) {}

func (tree *processTree) terminatePlatform(_ *exec.Cmd) (bool, error) {
	return false, nil
}
