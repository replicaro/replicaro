//go:build linux

package storageidentity

import (
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"unsafe"
)

var errLinuxStableVolumeUUIDUnavailable = errors.New("stable Linux volume UUID is unavailable")

func resolvePlatform(resolved, existing string) (Descriptor, error) {
	mountID, err := linuxVisibleMountID(existing)
	if err != nil {
		return Descriptor{}, err
	}
	mounts, err := readLinuxMounts()
	if err != nil {
		return Descriptor{}, err
	}
	mount, err := linuxMountByID(mounts, mountID)
	if err != nil {
		return Descriptor{}, err
	}
	return DescriptorFromLinuxMount(resolved, mount, func(m LinuxMount) (LinuxBlockFacts, error) {
		return linuxFilesystemFacts(existing, m)
	})
}

func resolvePlatformObservation(resolved, existing string) (Descriptor, string, error) {
	descriptor, err := resolvePlatform(resolved, existing)
	return descriptor, descriptor.Filesystem, err
}

func readLinuxMounts() ([]LinuxMount, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, fmt.Errorf("open mountinfo: %w", err)
	}
	defer file.Close()
	mounts, err := ParseLinuxMountInfo(file)
	if err != nil {
		return nil, err
	}
	return mounts, nil
}

func enumeratePlatform() ([]MountedFilesystem, error) {
	mounts, err := readLinuxMounts()
	if err != nil {
		return nil, err
	}
	result := make([]MountedFilesystem, 0, len(mounts))
	for _, mount := range mounts {
		directory, inspectErr := mountedDirectory(mount.MountPoint)
		if inspectErr != nil {
			if IsInspectionError(inspectErr) {
				continue
			}
			return nil, inspectErr
		}
		if !directory {
			continue
		}
		visibleID, visibleErr := linuxVisibleMountID(mount.MountPoint)
		if visibleErr != nil {
			if IsInspectionError(visibleErr) {
				continue
			}
			return nil, visibleErr
		}
		if visibleID != mount.MountID {
			continue
		}
		descriptor, descriptorErr := DescriptorFromLinuxMount(mount.MountPoint, mount, func(m LinuxMount) (LinuxBlockFacts, error) {
			return linuxFilesystemFacts(mount.MountPoint, m)
		})
		if descriptorErr != nil {
			if IsInspectionError(descriptorErr) {
				continue
			}
			return nil, descriptorErr
		}
		if descriptor.Kind == KindPathOnly &&
			(descriptor.StorageClass != StorageClassLocal || descriptor.MountRoot == "") {
			continue
		}
		result = append(result, MountedFilesystem{Path: filepath.Clean(mount.MountPoint), Descriptor: descriptor})
	}
	return result, nil
}

// Filesystem identity does not depend on whether hardware is removable. Btrfs
// mountinfo device numbers identify virtual subvolume devices, not backing disks;
// walking /sys/dev/block both rejects valid mounts and asks the wrong question.
func linuxFilesystemFacts(existing string, mount LinuxMount) (LinuxBlockFacts, error) {
	if mount.Filesystem == "btrfs" {
		uuid, err := linuxBtrfsVolumeUUID(existing, mount.MountID)
		return LinuxBlockFacts{VolumeUUID: uuid}, err
	}
	uuid, err := stableLinuxVolumeUUID(mount.Source)
	if errors.Is(err, errLinuxStableVolumeUUIDUnavailable) {
		return LinuxBlockFacts{}, nil
	}
	return LinuxBlockFacts{VolumeUUID: uuid}, err
}

// BTRFS_IOC_FS_INFO is a read-only filesystem query available to ordinary
// users. Its full 16-byte fsid is the filesystem UUID, unlike statfs's folded
// fsid or a member-device UUID. The 1024-byte UAPI structure and ioctl number
// are identical on supported Linux amd64/arm64 targets (linux/btrfs.h).
// Query the opened observed object, retaining mountinfo subvolume/bind geometry.
// Checking its mount ID prevents combining a new mount's UUID with old geometry;
// a final path observation rejects a route that changed while the fd stayed open.
// This is bounded local observation, not a lock against external mount changes.
func linuxBtrfsVolumeUUID(existing string, mountID uint64) (string, error) {
	fd, err := unix.Open(existing, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		return "", fmt.Errorf("open Btrfs filesystem observation: %w", err)
	}
	defer unix.Close(fd)
	var stat unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_NO_AUTOMOUNT, unix.STATX_MNT_ID, &stat); err != nil {
		return "", fmt.Errorf("observe opened Btrfs mount: %w", err)
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 || mountID == 0 || stat.Mnt_id != mountID {
		return "", fmt.Errorf("Btrfs mount changed during observation")
	}
	var info [1024]byte
	const btrfsIOCFSInfo = 0x8400941f
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), btrfsIOCFSInfo, uintptr(unsafe.Pointer(&info[0])))
	if errno != 0 {
		return "", fmt.Errorf("read Btrfs filesystem UUID: %w", errno)
	}
	visible, err := linuxVisibleMountID(existing)
	if err != nil {
		return "", err
	}
	if visible != mountID {
		return "", fmt.Errorf("Btrfs mount changed during observation")
	}
	uuid := info[16:32] // max_id and num_devices precede fsid in the UAPI.
	if [16]byte(uuid) == [16]byte{} {
		return "", fmt.Errorf("Btrfs filesystem UUID is empty")
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x", uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16]), nil
}

func stableLinuxVolumeUUID(source string) (string, error) {
	return stableLinuxVolumeUUIDAt(source, "/dev/disk/by-uuid", filepath.EvalSymlinks, sameDevicePath)
}

func stableLinuxVolumeUUIDAt(source, directory string, eval func(string) (string, error), sameDevice func(string, string) (bool, error)) (string, error) {
	device, err := eval(source)
	if err != nil {
		return "", err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		if os.IsNotExist(err) {
			return "", errLinuxStableVolumeUUIDUnavailable
		}
		return "", err
	}
	var match string
	var incomplete error
	for _, entry := range entries {
		target, evalErr := eval(filepath.Join(directory, entry.Name()))
		if evalErr != nil {
			incomplete = errors.Join(incomplete, fmt.Errorf("inspect UUID candidate %q: %w", entry.Name(), evalErr))
			continue
		}
		same, sameErr := sameDevice(device, target)
		if sameErr != nil {
			incomplete = errors.Join(incomplete, fmt.Errorf("compare UUID candidate %q: %w", entry.Name(), sameErr))
			continue
		}
		if same {
			if match != "" && match != entry.Name() {
				return "", fmt.Errorf("conflicting stable UUIDs for block device")
			}
			match = entry.Name()
		}
	}
	if match == "" {
		if incomplete != nil {
			// A broken or unreadable candidate makes UUID absence inconclusive.
			// Do not turn an incomplete authoritative enumeration into path-only
			// admission.
			return "", incomplete
		}
		return "", errLinuxStableVolumeUUIDUnavailable
	}
	return match, nil
}

func sameDevicePath(left, right string) (bool, error) {
	leftInfo, err := os.Stat(left)
	if err != nil {
		return false, err
	}
	rightInfo, err := os.Stat(right)
	if err != nil {
		return false, err
	}
	return os.SameFile(leftInfo, rightInfo), nil
}

// Ask the kernel which mount is visible at this path. Prefixes cannot resolve
// stacked mounts or mounts hidden by an overmounted ancestor.
func linuxVisibleMountID(value string) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(unix.AT_FDCWD, value, unix.AT_NO_AUTOMOUNT, unix.STATX_MNT_ID, &stat); err != nil {
		return 0, fmt.Errorf("observe visible Linux mount: %w", err)
	}
	if stat.Mask&unix.STATX_MNT_ID == 0 || stat.Mnt_id == 0 {
		return 0, fmt.Errorf("native Linux mount ID is unavailable")
	}
	return stat.Mnt_id, nil
}
