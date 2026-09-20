//go:build darwin

package command

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	darwinSessionTerminationTimeout = 500 * time.Millisecond
	darwinSessionQuiescence         = 60 * time.Millisecond
	darwinSessionPollInterval       = 20 * time.Millisecond
	darwinEPERMRescanAttempts       = int(darwinSessionQuiescence/darwinSessionPollInterval) + 1
	// XNU exposes p_stat in KinfoProc and defines SZOMB as 5. Exclude zombies
	// before group liveness accounting because killpg deliberately does not
	// signal them.
	darwinZombieProcessStatus = 5
)

type darwinSessionProcessGroup struct {
	pgid       int
	liveMember bool
}

func initializeProcessTreePlatform(tree *processTree) {
	tree.sessionProcesses = func() ([]processIdentity, error) {
		processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
		if err != nil {
			return nil, err
		}
		result := make([]processIdentity, 0, len(processes))
		for _, process := range processes {
			result = append(result, processIdentity{
				pid:    int(process.Proc.P_pid),
				status: process.Proc.P_stat,
				euid:   process.Eproc.Ucred.Uid,
			})
		}
		return result, nil
	}
}

// terminatePlatform terminates every signalable process group that still
// belongs to this invocation's exact Darwin session. The intentionally
// unreaped session leader anchors the session ID until cleanup is complete.
//
// Accepted limitation: XNU identifies secondary process groups numerically, so
// a deliberate same-user process can race reuse of a secondary PGID between
// session enumeration and killpg. Replicaro narrows that interval by rescanning
// and requiring continuous session quiescence, but does not claim protection
// from a local same-user adversary. The leader/top-level PGID remains anchored
// and is not subject to this accepted limitation.
func (tree *processTree) terminatePlatform(process *exec.Cmd) (bool, error) {
	if tree.sid <= 1 || process.Process == nil || process.Process.Pid != tree.sid {
		return true, fmt.Errorf("invalid engine process session")
	}

	if tree.leaderExited.Load() {
		return true, tree.signalDarwinSession(syscall.SIGKILL, darwinSessionTerminationTimeout)
	}
	if err := tree.signalDarwinSessionOnce(syscall.SIGTERM); err != nil {
		return true, err
	}
	deadline := time.Now().Add(darwinSessionTerminationTimeout)
	var inactiveSince time.Time
	for time.Now().Before(deadline) {
		if tree.leaderExited.Load() {
			return true, tree.signalDarwinSession(syscall.SIGKILL, darwinSessionTerminationTimeout)
		}
		active, err := tree.darwinSessionActive()
		if err != nil {
			return true, err
		}
		if active {
			inactiveSince = time.Time{}
		} else if inactiveSince.IsZero() {
			inactiveSince = time.Now()
		} else if time.Since(inactiveSince) >= darwinSessionQuiescence {
			return true, nil
		}
		time.Sleep(darwinSessionPollInterval)
	}
	return true, tree.signalDarwinSession(syscall.SIGKILL, darwinSessionTerminationTimeout)
}

func (tree *processTree) signalDarwinSession(signal syscall.Signal, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var inactiveSince time.Time
	for {
		if err := tree.signalDarwinSessionOnce(signal); err != nil {
			return err
		}
		active, err := tree.darwinSessionActive()
		if err != nil {
			return err
		}
		if active {
			inactiveSince = time.Time{}
		} else if inactiveSince.IsZero() {
			inactiveSince = time.Now()
		} else if time.Since(inactiveSince) >= darwinSessionQuiescence {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("engine process session remained active after termination")
		}
		time.Sleep(darwinSessionPollInterval)
	}
}

func (tree *processTree) signalDarwinSessionOnce(signal syscall.Signal) error {
	groups, err := tree.darwinSessionProcessGroups()
	if err != nil {
		return err
	}
	for _, group := range groups {
		if err := syscall.Kill(-group.pgid, signal); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				continue
			}
			gone, inspectErr := tree.darwinGroupGoneAfterSignalError(group, err)
			if inspectErr != nil {
				return inspectErr
			}
			if !gone {
				return fmt.Errorf("signal engine process group %d: %w", group.pgid, err)
			}
		}
	}
	return nil
}

func (tree *processTree) darwinSessionActive() (bool, error) {
	return tree.darwinSessionActiveWithInspect(unix.Getsid, unix.Getpgid, syscall.Kill)
}

func (tree *processTree) darwinSessionActiveWithInspect(
	sessionID func(int) (int, error),
	processGroupID func(int) (int, error),
	signalGroup func(int, syscall.Signal) error,
) (bool, error) {
	groups, err := tree.darwinSessionProcessGroupsWithInspect(sessionID, processGroupID)
	if err != nil {
		return false, err
	}
	for _, group := range groups {
		// NOTE_EXIT is authoritative for the exact leader even while XNU still
		// exposes that process in a pre-zombie exit state. Keep the leader
		// unreaped as the session identity anchor, but do not let that
		// transition state make an otherwise empty session look live.
		if !group.liveMember && tree.leaderExited.Load() && group.pgid == tree.pgid {
			continue
		}
		if err := signalGroup(-group.pgid, 0); err == nil {
			return true, nil
		} else if !errors.Is(err, syscall.ESRCH) {
			gone, inspectErr := tree.darwinGroupGoneAfterSignalError(group, err)
			if inspectErr != nil {
				return false, inspectErr
			}
			if !gone {
				return false, fmt.Errorf("inspect engine process group %d: %w", group.pgid, err)
			}
		}
	}
	return false, nil
}

// XNU's killpg path excludes zombies and returns EPERM when it finds no
// signalable member. Re-enumerate for one bounded quiescence interval after a
// signal error so a just-exited process that remains briefly visible in
// kern.proc.all is harmless, while a persistently live member makes cleanup
// fail closed. The top-level group is anchored by the intentionally unreaped
// leader, so it may also be accepted after a continuously empty rescan when
// EPERM arrives before the separate exit watcher observes that leader.
func (tree *processTree) darwinGroupGoneAfterSignalError(group darwinSessionProcessGroup, signalErr error) (bool, error) {
	return darwinGroupGoneAfterSignalErrorWithRescan(
		group,
		signalErr,
		tree.leaderExited.Load(),
		tree.pgid,
		tree.darwinSessionProcessGroups,
		time.Sleep,
	)
}

func darwinGroupGoneAfterSignalErrorWithRescan(
	group darwinSessionProcessGroup,
	signalErr error,
	leaderExited bool,
	leaderPGID int,
	rescan func() ([]darwinSessionProcessGroup, error),
	pause func(time.Duration),
) (bool, error) {
	if !errors.Is(signalErr, syscall.EPERM) {
		return false, nil
	}
	if !group.liveMember {
		if group.pgid != leaderPGID {
			return false, nil
		}
		if leaderExited {
			return true, nil
		}
	}
	requireEmptyQuiescence := !group.liveMember
	for attempt := 0; attempt < darwinEPERMRescanAttempts; attempt++ {
		if attempt > 0 {
			pause(darwinSessionPollInterval)
		}
		groups, err := rescan()
		if err != nil {
			return false, err
		}
		live := false
		for _, current := range groups {
			if current.pgid == group.pgid && current.liveMember {
				live = true
				break
			}
		}
		if !live && (!requireEmptyQuiescence || attempt == darwinEPERMRescanAttempts-1) {
			return true, nil
		}
		if live {
			requireEmptyQuiescence = false
		}
	}
	return false, nil
}

func (tree *processTree) darwinSessionProcessGroups() ([]darwinSessionProcessGroup, error) {
	return tree.darwinSessionProcessGroupsWithInspect(unix.Getsid, unix.Getpgid)
}

func (tree *processTree) darwinSessionProcessGroupsWithInspect(
	sessionID func(int) (int, error),
	processGroupID func(int) (int, error),
) ([]darwinSessionProcessGroup, error) {
	if tree.sid <= 1 || tree.pgid <= 1 || tree.sid != tree.pgid {
		return nil, fmt.Errorf("invalid engine process session identity")
	}
	processes, err := tree.sessionProcesses()
	if err != nil {
		return nil, fmt.Errorf("enumerate engine process session: %w", err)
	}
	groups := map[int]bool{tree.pgid: false}
	euid := uint32(os.Geteuid())
	for _, process := range processes {
		pid := process.pid
		if pid <= 1 {
			continue
		}
		if process.status == darwinZombieProcessStatus {
			if pid == tree.sid {
				tree.leaderExited.Store(true)
			}
			continue
		}
		// kqueue can report NOTE_EXIT before kern.proc.all changes p_stat to
		// SZOMB. Once that exact event has been observed, the leader cannot do
		// more work or create descendants and must not be counted as a live
		// session member. The unreaped PID still anchors the session identity.
		if pid == tree.sid && tree.leaderExited.Load() {
			continue
		}
		sid, err := sessionID(pid)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect process %d session: %w", pid, err)
		}
		if sid != tree.sid {
			continue
		}
		if process.euid != euid {
			return nil, fmt.Errorf("engine process session contains a different effective user")
		}
		pgid, err := processGroupID(pid)
		if errors.Is(err, unix.ESRCH) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect process %d group: %w", pid, err)
		}
		if pgid <= 1 {
			return nil, fmt.Errorf("engine process session contains an invalid process group")
		}
		groups[pgid] = true
	}
	result := make([]darwinSessionProcessGroup, 0, len(groups))
	for pgid, liveMember := range groups {
		result = append(result, darwinSessionProcessGroup{pgid: pgid, liveMember: liveMember})
	}
	sort.Slice(result, func(left, right int) bool { return result[left].pgid < result[right].pgid })
	return result, nil
}
