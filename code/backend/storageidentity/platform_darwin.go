//go:build darwin

package storageidentity

import (
	"encoding/binary"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

var errDarwinStableVolumeUUIDUnavailable = errors.New("stable Darwin volume UUID is unavailable")

type darwinAttrList struct {
	BitmapCount   uint16
	Reserved      uint16
	CommonAttr    uint32
	VolumeAttr    uint32
	DirectoryAttr uint32
	FileAttr      uint32
	ForkAttr      uint32
}

func resolvePlatform(resolved, existing string) (Descriptor, error) {
	// Bind the containing filesystem and its non-firmlink namespace to the same
	// open object. A lexical /Users prefix can otherwise select the System volume.
	fd, err := unix.Open(existing, unix.O_EVTONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return Descriptor{}, fmt.Errorf("open Darwin storage observation: %w", err)
	}
	defer unix.Close(fd)
	var stats syscall.Statfs_t
	if err := syscall.Fstatfs(fd, &stats); err != nil {
		return Descriptor{}, fmt.Errorf("inspect Darwin filesystem: %w", err)
	}
	nativePath, err := darwinNoFirmlinkPath(fd)
	if err != nil {
		return Descriptor{}, err
	}
	tail, err := filepath.Rel(existing, resolved)
	if err != nil || tail == ".." || strings.HasPrefix(tail, "../") {
		return Descriptor{}, fmt.Errorf("storage path is outside observed parent")
	}
	mount, err := darwinMountFromStat(stats)
	if err != nil {
		return Descriptor{}, err
	}
	// Volume attributes are queried at the actual mount root. Confirm that root
	// still describes the open filesystem before combining UUID and geometry.
	rootFD, err := unix.Open(mount.MountPoint, unix.O_EVTONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return Descriptor{}, err
	}
	defer unix.Close(rootFD)
	var rootStats syscall.Statfs_t
	if err := syscall.Fstatfs(rootFD, &rootStats); err != nil {
		return Descriptor{}, err
	}
	if rootStats.Fsid != stats.Fsid {
		return Descriptor{}, fmt.Errorf("Darwin filesystem changed during observation")
	}
	// Mntonname uses ordinary firmlink spelling. A mount below /Users must
	// have its root converted to the same namespace as the opened object.
	nativeRoot, err := darwinNoFirmlinkPath(rootFD)
	if err != nil {
		return Descriptor{}, err
	}
	return descriptorFromDarwinObservation(resolved, filepath.Join(nativePath, tail), nativeRoot, mount)
}

func darwinNoFirmlinkPath(fd int) (string, error) {
	var physical [1024]byte // Darwin MAXPATHLEN required by F_GETPATH_NOFIRMLINK.
	_, _, errno := syscall.Syscall(syscall.SYS_FCNTL, uintptr(fd), unix.F_GETPATH_NOFIRMLINK, uintptr(unsafe.Pointer(&physical[0])))
	if errno != 0 {
		return "", fmt.Errorf("observe Darwin volume-relative path: %w", errno)
	}
	value := unix.ByteSliceToString(physical[:])
	if !filepath.IsAbs(value) {
		return "", fmt.Errorf("Darwin native volume path is not absolute")
	}
	return value, nil
}

func resolvePlatformObservation(resolved, existing string) (Descriptor, string, error) {
	descriptor, err := resolvePlatform(resolved, existing)
	return descriptor, descriptor.Filesystem, err
}

func darwinMounts() ([]DarwinMount, error) {
	count, err := syscall.Getfsstat(nil, unix.MNT_NOWAIT)
	if err != nil {
		return nil, fmt.Errorf("inspect Darwin mount count: %w", err)
	}
	stats := make([]syscall.Statfs_t, count)
	count, err = syscall.Getfsstat(stats, unix.MNT_NOWAIT)
	if err != nil {
		return nil, fmt.Errorf("inspect Darwin mounts: %w", err)
	}
	result := make([]DarwinMount, 0, count)
	for index := 0; index < count; index++ {
		// UUID access belongs to this mount, not the entire OS inventory.
		mount, err := darwinMountFromStat(stats[index])
		if err != nil {
			if IsInspectionError(err) {
				continue
			}
			return nil, err
		}
		result = append(result, mount)
	}
	return result, nil
}

func darwinMountFromStat(stats syscall.Statfs_t) (DarwinMount, error) {
	mount := DarwinMount{MountPoint: int8String(stats.Mntonname[:]), Source: int8String(stats.Mntfromname[:]), Filesystem: int8String(stats.Fstypename[:])}
	if strings.HasPrefix(mount.Source, "/dev/") {
		uuid, err := darwinVolumeUUID(mount.MountPoint)
		if err != nil && !errors.Is(err, errDarwinStableVolumeUUIDUnavailable) {
			return DarwinMount{}, err
		}
		mount.VolumeUUID = uuid
	}
	return mount, nil
}

func enumeratePlatform() ([]MountedFilesystem, error) {
	mounts, err := darwinMounts()
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
		descriptor, descriptorErr := DescriptorFromDarwinMount(mount.MountPoint, mount)
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

func int8String(value []int8) string {
	bytes := make([]byte, 0, len(value))
	for _, item := range value {
		if item == 0 {
			break
		}
		bytes = append(bytes, byte(item))
	}
	return string(bytes)
}

func darwinVolumeUUID(mountPoint string) (string, error) {
	pathPointer, err := syscall.BytePtrFromString(mountPoint)
	if err != nil {
		return "", err
	}
	attributes := darwinAttrList{BitmapCount: unix.ATTR_BIT_MAP_COUNT, VolumeAttr: unix.ATTR_VOL_INFO | unix.ATTR_VOL_UUID}
	var output [20]byte // uint32 length followed by 16 UUID bytes.
	_, _, errno := syscall.Syscall6(syscall.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(pathPointer)), uintptr(unsafe.Pointer(&attributes)),
		uintptr(unsafe.Pointer(&output[0])), uintptr(len(output)), 0, 0)
	if errno != 0 {
		if errno == unix.ENOTSUP || errno == unix.ENOATTR {
			return "", errDarwinStableVolumeUUIDUnavailable
		}
		return "", fmt.Errorf("read persistent volume UUID: %w", errno)
	}
	if binary.NativeEndian.Uint32(output[:4]) != uint32(len(output)) {
		return "", fmt.Errorf("invalid Darwin volume UUID response")
	}
	uuid := output[4:20]
	if [16]byte(uuid) == [16]byte{} {
		return "", errDarwinStableVolumeUUIDUnavailable
	}
	return fmt.Sprintf("%08x-%04x-%04x-%04x-%012x",
		uuid[0:4], uuid[4:6], uuid[6:8], uuid[8:10], uuid[10:16]), nil
}
