//go:build darwin

package command

import (
	"errors"
	"fmt"
	"os/exec"

	"golang.org/x/sys/unix"
)

type darwinKevent func(int, []unix.Kevent_t, []unix.Kevent_t, *unix.Timespec) (int, error)

// Darwin's kqueue NOTE_EXIT observes a child exit without reaping it.  The
// leader therefore remains in its original process group while descendants
// are terminated, preventing a recycled numeric PGID from being signalled.
func (tree *processTree) wait(process *exec.Cmd) processWaitResult {
	return tree.waitWithKevent(process, unix.Kevent)
}

func (tree *processTree) waitWithKevent(process *exec.Cmd, kevent darwinKevent) processWaitResult {
	if process.Process == nil {
		return processWaitResult{err: fmt.Errorf("invalid process")}
	}
	if tree.leaderExited.Load() {
		return processWaitResult{observed: true}
	}
	kq, err := unix.Kqueue()
	if err != nil {
		// Preserve the unreaped session leader so the caller can terminate the
		// exact session before reaping. Reaping here would discard the identity
		// anchor while separate-process-group descendants may still exist.
		return processWaitResult{err: fmt.Errorf("create engine exit watcher: %w", err)}
	}
	defer unix.Close(kq)
	change := unix.Kevent_t{Ident: uint64(process.Process.Pid), Filter: unix.EVFILT_PROC, Flags: unix.EV_ADD | unix.EV_ONESHOT, Fflags: unix.NOTE_EXIT}
	for {
		_, err = kevent(kq, []unix.Kevent_t{change}, nil, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		break
	}
	if err != nil {
		// A short-lived process can exit after attach validates its isolated
		// group but before this watcher is registered. Darwin reports ESRCH in
		// that case. Keep the leader unreaped so its PID/PGID cannot be reused;
		// the caller will terminate the validated group and reap afterward.
		if errors.Is(err, unix.ESRCH) {
			tree.leaderExited.Store(true)
			return processWaitResult{observed: true}
		}
		return processWaitResult{err: fmt.Errorf("register engine exit watcher: %w", err)}
	}
	events := make([]unix.Kevent_t, 1)
	for {
		_, err = kevent(kq, nil, events, nil)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		break
	}
	if err != nil {
		return processWaitResult{err: fmt.Errorf("observe engine process exit: %w", err)}
	}
	tree.leaderExited.Store(true)
	return processWaitResult{observed: true}
}

func (tree *processTree) reap(process *exec.Cmd) error { return process.Wait() }
