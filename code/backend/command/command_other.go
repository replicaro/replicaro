//go:build !windows

package command

import "os/exec"

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync/atomic"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const processStartsSuspended = false

type processTree struct {
	pgid             int
	sid              int
	sessionID        func(int) (int, error)
	sessionProcesses func() ([]processIdentity, error)
	leaderExited     atomic.Bool
}

type processIdentity struct {
	pid    int
	status int8
	euid   uint32
}

type processWaitResult struct {
	// observed means waitid observed the leader exit without reaping it.  Keeping
	// the zombie leader in its original process group prevents a recycled PGID
	// from being signalled during descendant cleanup.
	observed bool
	err      error
	reaped   bool
}

func (tree *processTree) active(process *exec.Cmd) bool {
	// Once os/exec has reaped the leader (the fallback used on platforms
	// without waitid(WNOWAIT)), the numeric PGID may already belong to an
	// unrelated process. Never signal it based on a recycled number.
	if process != nil && process.ProcessState != nil {
		return false
	}
	return tree.pgid > 1 && syscall.Kill(-tree.pgid, 0) == nil
}

func configureWindowsCommand(process *exec.Cmd) {
	if runtime.GOOS == "darwin" || (runtime.GOOS == "linux" && os.Getpid() == 1) {
		// A process group is not a complete child-process boundary:
		// pinned engines may create additional process groups for helpers. Give
		// each top-level native command its own session so those groups remain
		// attributable to this exact invocation.
		process.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		return
	}
	process.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func newProcessTree() (*processTree, error) {
	tree := &processTree{sessionID: unix.Getsid}
	initializeProcessTreePlatform(tree)
	return tree, nil
}

func (tree *processTree) attach(process *exec.Cmd) error {
	if process.Process == nil || process.Process.Pid <= 1 {
		return fmt.Errorf("invalid process id")
	}
	pgid, err := syscall.Getpgid(process.Process.Pid)
	if err != nil {
		// Darwin may stop exposing an exited-but-unreaped child through
		// getpgid. Start already succeeded with Setsid, so the child created the
		// exact PID session and process group before exec, and the unreaped
		// leader prevents that identity from being reused before cleanup.
		if runtime.GOOS == "darwin" && errors.Is(err, syscall.ESRCH) {
			tree.pgid = process.Process.Pid
			tree.sid = process.Process.Pid
			tree.leaderExited.Store(true)
			return nil
		}
		return err
	}
	if pgid != process.Process.Pid {
		return fmt.Errorf("engine process did not create an isolated process group")
	}
	tree.pgid = pgid
	if runtime.GOOS == "darwin" || (runtime.GOOS == "linux" && os.Getpid() == 1) {
		sid, err := tree.sessionID(process.Process.Pid)
		if errors.Is(err, unix.ESRCH) {
			// The session was successfully created before exec and Getpgid just
			// observed it. Preserve the unreaped leader's PID as the exact
			// session/group anchor when it exits before this second inspection.
			tree.pgid = process.Process.Pid
			tree.sid = process.Process.Pid
			tree.leaderExited.Store(true)
			return nil
		}
		if err != nil {
			return fmt.Errorf("inspect engine process session: %w", err)
		}
		if sid != process.Process.Pid {
			return fmt.Errorf("engine process did not create an isolated session")
		}
		tree.sid = sid
	}
	return nil
}

func (tree *processTree) terminate(process *exec.Cmd) error {
	if process.Process == nil || process.ProcessState != nil || tree.pgid <= 1 || process.Process.Pid != tree.pgid {
		return nil
	}
	if handled, err := tree.terminatePlatform(process); handled {
		return err
	}
	if tree.leaderExited.Load() {
		if err := syscall.Kill(-tree.pgid, syscall.SIGKILL); err != nil &&
			!errors.Is(err, syscall.ESRCH) &&
			!(runtime.GOOS == "darwin" && errors.Is(err, syscall.EPERM)) {
			return err
		}
		// Darwin can report EPERM when the exited, intentionally unreaped
		// leader is the only remaining process-group member. A same-principal
		// descendant is signalable; if one still holds an inherited output pipe,
		// the bounded drain below also prevents a false successful return.
		return nil
	}
	// Do not signal a recycled process-group identifier after a short-lived
	// command exits. kill(..., 0) succeeds only while the isolated group still
	// has a member (typically an inherited-stream descendant).
	if err := syscall.Kill(-tree.pgid, 0); errors.Is(err, syscall.ESRCH) {
		return nil
	}
	if err := syscall.Kill(-tree.pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if tree.leaderExited.Load() {
			if err := syscall.Kill(-tree.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
			return nil
		}
		if err := syscall.Kill(-tree.pgid, 0); errors.Is(err, syscall.ESRCH) {
			return nil
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(-tree.pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (tree *processTree) close() {}
