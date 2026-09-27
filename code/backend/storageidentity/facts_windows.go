//go:build windows

package storageidentity

import (
	"os"
	"strings"

	"golang.org/x/sys/windows"
)

const (
	windowsLongPathUTF16 = 32768
	windowsVolumeNameDOS = 0x0 // VOLUME_NAME_DOS with FILE_NAME_NORMALIZED
)

// observeFacts splits Windows paths into two groups.
//
// Network paths (UNC paths and mapped drive letters) take their mount point
// from the path text and record no filesystem type. Asking Windows for the
// share's volume root, universal name, provider, or filesystem name contacts
// the server; those calls failed on real NAS devices and blocked vault
// creation. An unmounted share or disconnected mapped drive makes the path
// disappear rather than leaving an empty folder behind, so the type check adds
// nothing for network paths. The only local call is GetDriveType on the drive
// root, which reads the local drive table and never talks to the server.
//
// Local paths use the opened handle: GetFinalPathNameByHandleW gives the real
// path (so a volume mounted into a folder such as C:\Mounts\Disk2 is seen as
// its own mount), GetVolumePathNameW turns that into the volume root, and
// GetVolumeInformationByHandleW reads the filesystem name. If the volume root
// lookup fails we fall back to the drive letter from the path text rather than
// recording nothing.
func observeFacts(file *os.File, path string) (Facts, []FactFailure) {
	if mount, network := windowsNetworkMount(path); network {
		return Facts{MountPoint: mount}, nil
	}
	handle := windows.Handle(file.Fd())
	facts := Facts{}
	failures := []FactFailure{}
	if mount, err := windowsLocalVolumeRoot(handle); err == nil {
		facts.MountPoint = mount
	} else if drive, ok := WindowsDriveFromText(path); ok {
		facts.MountPoint = drive
	} else {
		code, _ := osErrorCode(err)
		failures = append(failures, FactFailure{Step: StepMountPoint, Code: code})
	}
	if filesystem, err := windowsFilesystemByHandle(handle); err == nil {
		facts.Filesystem = filesystem
	} else {
		code, _ := osErrorCode(err)
		failures = append(failures, FactFailure{Step: StepFilesystemType, Code: code})
	}
	return facts, failures
}

// windowsNetworkMount reports the text-derived mount point of a network path.
func windowsNetworkMount(path string) (string, bool) {
	if share, ok := WindowsNetworkMountFromText(path); ok {
		return share, true
	}
	drive, ok := WindowsDriveFromText(path)
	if !ok {
		return "", false
	}
	pointer, err := windows.UTF16PtrFromString(drive)
	if err != nil {
		return "", false
	}
	if windows.GetDriveType(pointer) == windows.DRIVE_REMOTE {
		return drive, true
	}
	return "", false
}

func windowsLocalVolumeRoot(handle windows.Handle) (string, error) {
	buffer := make([]uint16, windowsLongPathUTF16)
	length, err := windows.GetFinalPathNameByHandle(handle, &buffer[0], uint32(len(buffer)), windowsVolumeNameDOS)
	if err != nil {
		return "", err
	}
	if length == 0 || length >= uint32(len(buffer)) {
		return "", windows.ERROR_INSUFFICIENT_BUFFER
	}
	final := windows.UTF16ToString(buffer[:length])
	switch {
	case strings.HasPrefix(strings.ToUpper(final), `\\?\UNC\`):
		final = `\\` + final[len(`\\?\UNC\`):]
	case strings.HasPrefix(final, `\\?\`):
		final = final[len(`\\?\`):]
	}
	pointer, err := windows.UTF16PtrFromString(final)
	if err != nil {
		return "", err
	}
	root := make([]uint16, windowsLongPathUTF16)
	if err := windows.GetVolumePathName(pointer, &root[0], uint32(len(root))); err != nil {
		return "", err
	}
	value := windows.UTF16ToString(root)
	if value == "" {
		return "", windows.ERROR_INVALID_DATA
	}
	return value, nil
}

func windowsFilesystemByHandle(handle windows.Handle) (string, error) {
	var name [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, nil, &name[0], uint32(len(name))); err != nil {
		return "", err
	}
	filesystem := NormalizeFilesystem(windows.UTF16ToString(name[:]))
	if filesystem == "" {
		return "", windows.ERROR_INVALID_DATA
	}
	return filesystem, nil
}
