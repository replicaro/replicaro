//go:build windows

package engines

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/windows"
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
	sourceChain, err := openWindowsAncestorChain(filepath.Dir(staged))
	if err != nil {
		return RcloneConfigRetained, err
	}
	defer closeWindowsChain(sourceChain)
	sourceDirectory := sourceChain[len(sourceChain)-1]
	if err := inspectWindowsRcloneObject(sourceDirectory, true); err != nil {
		return RcloneConfigRetained, err
	}
	source, err := openWindowsLocked(
		staged,
		windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL|windows.DELETE,
		false,
	)
	if err != nil {
		return RcloneConfigRetained, err
	}
	defer source.Close()
	if err := inspectWindowsRcloneConfigFile(source); err != nil {
		return RcloneConfigRetained, err
	}
	if err := inspectBoundedRcloneVaultConfig(source); err != nil {
		return RcloneConfigRetained, err
	}
	if err := sameRcloneVaultOpenedPath(staged, source); err != nil {
		return RcloneConfigRetained, err
	}
	var sourceInfo, destinationInfo windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(windows.Handle(sourceDirectory.Fd()), &sourceInfo); err != nil {
		return RcloneConfigRetained, err
	}
	if err := windows.GetFileInformationByHandle(windows.Handle(destination.Fd()), &destinationInfo); err != nil {
		return RcloneConfigRetained, err
	}
	if sourceInfo.VolumeSerialNumber != destinationInfo.VolumeSerialNumber {
		return RcloneConfigRetained, fmt.Errorf("rclone config candidate and canonical target are on different filesystems")
	}
	if err := renameWindowsRcloneHandle(source, destination, filepath.Base(target)); err != nil {
		// NtSetInformationFile is the ownership-transfer boundary. A reported
		// failure cannot safely prove which path owns the candidate afterward.
		return RcloneConfigIndeterminate, err
	}
	return RcloneConfigActivated, nil
}
