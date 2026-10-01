//go:build windows

package engines

import (
	"errors"
	"io/fs"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

type rcloneRemoteWindowsLocation struct {
	path                        string
	volume, indexHigh, indexLow uint32
}

var rcloneRemoteNamespaceWindowsLocation = rcloneRemoteOpenedWindowsLocation

func checkRcloneRemoteNamespaceAliases(configFile, realFile string, roots []string) error {
	var managed []rcloneRemoteWindowsLocation
	for _, root := range roots {
		location, err := rcloneRemoteNamespaceWindowsLocation(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		managed = append(managed, location)
	}
	for _, route := range []string{configFile, realFile} {
		for current := route; ; current = filepath.Dir(current) {
			location, err := rcloneRemoteNamespaceWindowsLocation(current)
			if err != nil {
				return err
			}
			for _, root := range managed {
				if location.volume == root.volume && location.indexHigh == root.indexHigh && location.indexLow == root.indexLow ||
					rcloneRemotePathWithin(strings.ToUpper(location.path), strings.ToUpper(root.path)) {
					return rcloneRemoteConfigManagedFolder()
				}
			}
			if filepath.Dir(current) == current {
				break
			}
		}
	}
	return nil
}

func rcloneRemoteOpenedWindowsLocation(path string) (rcloneRemoteWindowsLocation, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return rcloneRemoteWindowsLocation{}, err
	}
	// Shared metadata handles work while another reader holds the directory.
	// Unlike os.SameFile's lazy lookup, every failed identity read is returned.
	handle, err := windows.CreateFile(name, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return rcloneRemoteWindowsLocation{}, err
	}
	defer windows.CloseHandle(handle)
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		return rcloneRemoteWindowsLocation{}, err
	}
	buffer := make([]uint16, 32768)
	// VOLUME_NAME_NT avoids drive-letter and subst spellings of the same path.
	length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), 0x2)
	if err != nil {
		return rcloneRemoteWindowsLocation{}, err
	}
	if length == 0 || length >= uint32(len(buffer)) {
		return rcloneRemoteWindowsLocation{}, windows.ERROR_INSUFFICIENT_BUFFER
	}
	final := filepath.Clean(windows.UTF16ToString(buffer[:length]))
	if !strings.HasPrefix(final, `\Device\`) {
		return rcloneRemoteWindowsLocation{}, windows.ERROR_INVALID_DATA
	}
	return rcloneRemoteWindowsLocation{path: final, volume: info.VolumeSerialNumber,
		indexHigh: info.FileIndexHigh, indexLow: info.FileIndexLow}, nil
}
