//go:build !windows && !plan9

package engines

import "syscall"

func destinationDevice(path string) (uint64, error) {
	var stat syscall.Stat_t
	if err := syscall.Lstat(path, &stat); err != nil {
		return 0, err
	}
	return uint64(stat.Dev), nil
}
