//go:build !windows

package engines

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

func activateRcloneConfigCandidate(
	staged string,
	destination *os.File,
	target string,
) (RcloneConfigDisposition, error) {
	if err := recognizedRcloneConfigSessionPath(filepath.Dir(staged)); err != nil ||
		filepath.Base(staged) != "rclone.conf" {
		return RcloneConfigRetained, fmt.Errorf("rclone config candidate is outside the private config-session root")
	}
	sourceDirectory, err := openUnixDirectory(filepath.Dir(staged))
	if err != nil {
		return RcloneConfigRetained, err
	}
	defer sourceDirectory.Close()
	source, mode, err := openUnixChild(sourceDirectory, "rclone.conf")
	if err != nil {
		return RcloneConfigRetained, err
	}
	defer source.Close()
	if !mode.IsRegular() || inspectUnixRcloneConfigFile(source) != nil {
		return RcloneConfigRetained, fmt.Errorf("rclone config candidate is unsafe")
	}
	if err := inspectBoundedRcloneVaultConfig(source); err != nil {
		return RcloneConfigRetained, err
	}
	if same, sameErr := unixSameEntry(sourceDirectory, "rclone.conf", source); sameErr != nil || !same {
		return RcloneConfigRetained, fmt.Errorf("rclone config candidate identity changed")
	}
	var sourceStat, destinationStat unix.Stat_t
	if err := unix.Fstat(int(sourceDirectory.Fd()), &sourceStat); err != nil {
		return RcloneConfigRetained, err
	}
	if err := unix.Fstat(int(destination.Fd()), &destinationStat); err != nil {
		return RcloneConfigRetained, err
	}
	if sourceStat.Dev != destinationStat.Dev {
		return RcloneConfigRetained, fmt.Errorf("rclone config candidate and canonical target are on different filesystems")
	}
	if err := unix.Renameat(
		int(sourceDirectory.Fd()), "rclone.conf",
		int(destination.Fd()), filepath.Base(target),
	); err != nil {
		return RcloneConfigRetained, err
	}
	return RcloneConfigActivated, errors.Join(destination.Sync())
}
