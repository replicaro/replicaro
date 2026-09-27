//go:build linux

package storageidentity

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// The mount of the *opened* folder is identified by its kernel mount ID and
// then looked up in /proc/self/mountinfo. Resolving by path prefix instead
// would pick the wrong record for stacked mounts or a mount hidden under an
// overmounted parent. In a container this also sees a bind mount of a host
// folder with the host filesystem's type, which is what lets an unmounted
// host share (an empty folder on the host's root filesystem) show up as a
// different filesystem type and pause the job.
var (
	statxMountID        = statxOpenedMountID
	fdinfoMountID       = procFDInfoMountID
	readLinuxMountTable = readProcMountInfo
)

func observeFacts(file *os.File, _ string) (Facts, []FactFailure) {
	fd := int(file.Fd())
	id, err := statxMountID(fd)
	if err != nil {
		// STATX_MNT_ID arrived in Linux 5.8. Older kernels still expose the
		// same ID as mnt_id in /proc/self/fdinfo/<fd>.
		var fallbackErr error
		id, fallbackErr = fdinfoMountID(fd)
		if fallbackErr != nil {
			code, _ := osErrorCode(fallbackErr)
			if code == 0 {
				code, _ = osErrorCode(err)
			}
			return Facts{}, bothFactsFailed(code)
		}
	}
	mounts, err := readLinuxMountTable()
	if err != nil {
		code, _ := osErrorCode(err)
		return Facts{}, bothFactsFailed(code)
	}
	mount, err := linuxMountByID(mounts, id)
	if err != nil {
		return Facts{}, bothFactsFailed(0)
	}
	return Facts{MountPoint: mount.MountPoint, Filesystem: NormalizeFilesystem(mount.Filesystem)}, nil
}

func bothFactsFailed(code int64) []FactFailure {
	return []FactFailure{{Step: StepMountPoint, Code: code}, {Step: StepFilesystemType, Code: code}}
}

func statxOpenedMountID(fd int) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_NO_AUTOMOUNT, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, err
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id == 0 {
		return 0, fmt.Errorf("kernel did not report a mount ID")
	}
	return stat.Mnt_id, nil
}

func procFDInfoMountID(fd int) (uint64, error) {
	data, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(fd))
	if err != nil {
		return 0, err
	}
	return parseFDInfoMountID(string(data))
}

func parseFDInfoMountID(data string) (uint64, error) {
	for _, line := range strings.Split(data, "\n") {
		name, value, ok := strings.Cut(line, ":")
		if !ok || strings.TrimSpace(name) != "mnt_id" {
			continue
		}
		id, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
		if err != nil || id == 0 {
			return 0, fmt.Errorf("fdinfo mount ID is invalid")
		}
		return id, nil
	}
	return 0, fmt.Errorf("fdinfo has no mount ID")
}

func readProcMountInfo() ([]LinuxMount, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return ParseLinuxMountInfo(file)
}
