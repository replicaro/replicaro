//go:build linux

package command

import (
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

func (tree *processTree) wait(process *exec.Cmd) processWaitResult {
	if process.Process == nil {
		return processWaitResult{err: fmt.Errorf("invalid process")}
	}
	var info unix.Siginfo
	if err := unix.Waitid(unix.P_PID, process.Process.Pid, &info, unix.WEXITED|unix.WNOWAIT, nil); err != nil {
		return processWaitResult{err: process.Wait(), reaped: true}
	}
	tree.leaderExited.Store(true)
	return processWaitResult{observed: true}
}

func (tree *processTree) reap(process *exec.Cmd) error { return process.Wait() }
