//go:build linux

package command

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

var readDescendantDirectory = os.ReadDir

// A container PID 1 adopts orphaned native helpers. Killing their process
// group does not collect their exit records. Keep the unreaped command leader
// as the session anchor while collecting only this session's adopted children;
// wait(-1) would race os/exec's waiters for unrelated commands.
// Restic SSH helpers can exit in a different process group within this session.
// Collect their zombies with exact-child wait4; do not signal live secondary
// groups. pidfd was rejected here because live-process signaling is unnecessary
// for zombie collection and adds kernel/seccomp compatibility complexity.
func (tree *processTree) collectAdoptedDescendants(process *exec.Cmd) error {
	if os.Getpid() != 1 || process.Process == nil || process.ProcessState != nil ||
		tree.sid <= 1 || process.Process.Pid != tree.sid || !tree.leaderExited.Load() {
		return nil
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		entries, err := readDescendantDirectory("/proc")
		if err != nil {
			return fmt.Errorf("inspect adopted command descendants: %w", err)
		}
		pending := false
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 1 || pid == tree.pgid {
				continue
			}
			session, err := unix.Getsid(pid)
			if errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect adopted descendant session: %w", err)
			}
			if session != tree.sid {
				continue
			}
			stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, unix.ESRCH) {
				continue
			}
			if err != nil {
				return fmt.Errorf("inspect command descendant: %w", err)
			}
			// comm can contain spaces and parentheses. The final closing
			// parenthesis precedes state, PPID, and process group in /proc/stat.
			end := strings.LastIndex(string(stat), ") ")
			if end < 0 {
				return errors.New("invalid command descendant process record")
			}
			fields := strings.Fields(string(stat[end+2:]))
			if len(fields) < 4 {
				return errors.New("incomplete command descendant process record")
			}
			sid, err := strconv.Atoi(fields[3])
			if err != nil {
				return errors.New("invalid command descendant process session")
			}
			if sid != tree.sid {
				continue
			}
			// Once adopted by us, only this exact session's collector owns the
			// child. Never race its original parent's wait or a foreign waiter.
			if fields[1] != "1" || fields[0] != "Z" {
				pending = true
				continue
			}
			var status unix.WaitStatus
			reaped, err := unix.Wait4(pid, &status, unix.WNOHANG, nil)
			if err != nil && !errors.Is(err, unix.ECHILD) && !errors.Is(err, unix.EINTR) {
				return fmt.Errorf("collect adopted command descendant: %w", err)
			}
			// A killed descendant can still be exiting or waiting for its
			// parent to exit and reparent it to PID 1. Reinspect the session.
			if reaped != pid {
				pending = true
			}
		}
		if !pending {
			return nil
		}
		if !time.Now().Before(deadline) {
			return errors.New("command descendants could not be collected after termination")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
