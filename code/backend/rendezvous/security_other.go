//go:build !windows

package rendezvous

import (
	"fmt"
	"os"
	"syscall"
)

func validateOwnerOnly(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("rendezvous state is not a regular file")
	}
	if info.Mode().Perm() != 0o600 {
		return fmt.Errorf("rendezvous state permissions are not owner-only")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(stat.Uid) != os.Geteuid() {
		return fmt.Errorf("rendezvous state owner is invalid")
	}
	return nil
}

func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
