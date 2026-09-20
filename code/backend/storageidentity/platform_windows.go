//go:build windows

package storageidentity

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	mprDLL                           = windows.NewLazySystemDLL("mpr.dll")
	procWNetGetUniversalName         = mprDLL.NewProc("WNetGetUniversalNameW")
	procWNetGetResourceInformation   = mprDLL.NewProc("WNetGetResourceInformationW")
	kernel32DLL                      = windows.NewLazySystemDLL("kernel32.dll")
	procGetVolumeInformationByHandle = kernel32DLL.NewProc("GetVolumeInformationByHandleW")
	procGetFinalPathNameByHandle     = kernel32DLL.NewProc("GetFinalPathNameByHandleW")
	procDeviceIoControl              = kernel32DLL.NewProc("DeviceIoControl")
	errWindowsWinFspNotApplicable    = errors.New("WinFsp compatibility observation is inapplicable")
)

const (
	windowsVolumeNameGUID    = 0x1
	windowsVolumeNameNT      = 0x2
	windowsVolumeNameNone    = 0x4
	windowsFileNameOpened    = 0x8
	windowsFinalPathUTF16Max = 32768
	winFspQueryControlCode   = 0x000920fc // FSP_FSCTL_QUERY_WINFSP from the pinned WinFsp SDK.
)

type windowsNetResource struct {
	Scope, Type, DisplayType, Usage          uint32
	LocalName, RemoteName, Comment, Provider *uint16
}

func resolvePlatform(resolved, existing string) (Descriptor, error) {
	descriptor, _, err := resolvePlatformObservation(resolved, existing)
	return descriptor, err
}

func resolvePlatformObservation(resolved, existing string) (Descriptor, string, error) {
	requested := filepath.Clean(resolved)
	root, err := windowsVolumeRoot(existing)
	if err != nil {
		if errors.Is(err, windows.ERROR_UNRECOGNIZED_VOLUME) {
			return resolveWindowsWinFspFallback(requested, existing, err)
		}
		return Descriptor{}, "", err
	}
	rootPointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return Descriptor{}, "", err
	}
	driveType := windows.GetDriveType(rootPointer)
	filesystem, err := windowsFilesystem(rootPointer)
	if err != nil {
		if errors.Is(err, windows.ERROR_DIR_NOT_ROOT) {
			return resolveWindowsWinFspFallback(requested, existing, err)
		}
		return Descriptor{}, "", err
	}
	if driveType == windows.DRIVE_REMOTE || isWindowsUNC(requested) {
		universal := requested
		if !isWindowsUNC(requested) {
			var universalErr error
			universal, universalErr = windowsUniversalName(requested)
			if universalErr != nil {
				if windowsUniversalNameIdentityUnavailable(universalErr) {
					descriptor, descriptorErr := pathOnlyDescriptor(requested, filesystem, StorageClassNetwork)
					return descriptor, filesystem, descriptorErr
				}
				return Descriptor{}, "", fmt.Errorf("resolve mapped network path: %w", universalErr)
			}
		}
		provider, kind, providerErr := windowsNetworkProvider(universal)
		if providerErr != nil {
			if errors.Is(providerErr, errWindowsNetworkProviderUnproven) || windowsProviderIdentityUnavailable(providerErr) {
				descriptor, descriptorErr := pathOnlyDescriptor(requested, filesystem, StorageClassNetwork)
				return descriptor, filesystem, descriptorErr
			}
			return Descriptor{}, "", providerErr
		}
		descriptor, descriptorErr := DescriptorFromWindowsNetworkFacts(requested, universal, root, "", filesystem, uint32(driveType), provider, kind)
		return descriptor, filesystem, descriptorErr
	}
	if driveType != windows.DRIVE_REMOVABLE && driveType != windows.DRIVE_FIXED {
		descriptor, descriptorErr := pathOnlyDescriptor(requested, filesystem, StorageClassOther)
		return descriptor, filesystem, descriptorErr
	}
	var volume [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeNameForVolumeMountPoint(rootPointer, &volume[0], uint32(len(volume))); err != nil {
		if errors.Is(err, windows.ERROR_NOT_SUPPORTED) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
			// These are bounded native abstention results. The one binding keeps
			// the authoritative storage class and filesystem type without inventing
			// a stable ID; every unexpected Windows failure below remains closed.
			descriptor, descriptorErr := descriptorFromWindowsVolumeGUIDAbsence(requested, root, filesystem, uint32(driveType))
			return descriptor, filesystem, descriptorErr
		}
		if errors.Is(err, windows.ERROR_UNRECOGNIZED_VOLUME) {
			return resolveWindowsWinFspFallback(requested, existing, err)
		}
		return Descriptor{}, "", fmt.Errorf("resolve volume GUID: %w", err)
	}
	descriptor, descriptorErr := DescriptorFromWindowsFacts(requested, "", root, windows.UTF16ToString(volume[:]), filesystem, uint32(driveType))
	return descriptor, filesystem, descriptorErr
}

func resolveWindowsWinFspFallback(requested, existing string, inspectionErr error) (Descriptor, string, error) {
	descriptor, filesystem, err := resolveWindowsWinFspWithoutMountManager(requested, existing)
	if errors.Is(err, errWindowsWinFspNotApplicable) {
		// The compatibility proof did not apply. Preserve the native inspection
		// failure so mounted enumeration can skip this unavailable candidate
		// without suppressing unrelated usable filesystems.
		return Descriptor{}, "", inspectionErr
	}
	return descriptor, filesystem, err
}

// Some WinFsp filesystems deliberately do not register with Mount Manager.
// Their drive root therefore has no DOS/GUID volume name, and on some versions
// GetVolumePathNameW returns the queried directory rather than the drive root.
// Admit only a positively identified WinFsp namespace whose opened object and
// standard drive root prove the same native device and exact mount geometry.
func resolveWindowsWinFspWithoutMountManager(requested, existing string) (Descriptor, string, error) {
	volume := filepath.VolumeName(existing)
	if len(volume) != 2 || volume[1] != ':' || !((volume[0] >= 'A' && volume[0] <= 'Z') || (volume[0] >= 'a' && volume[0] <= 'z')) {
		return Descriptor{}, "", fmt.Errorf("%w: path is not on a standard drive", errWindowsWinFspNotApplicable)
	}
	root := volume + `\`
	rootPointer, err := windows.UTF16PtrFromString(root)
	if err != nil {
		return Descriptor{}, "", err
	}
	driveType := windows.GetDriveType(rootPointer)
	if driveType != windows.DRIVE_FIXED && driveType != windows.DRIVE_REMOVABLE {
		return Descriptor{}, "", fmt.Errorf("%w: drive type %d is not an available local volume", errWindowsWinFspNotApplicable, driveType)
	}

	rootHandle, err := windowsOpenStorageHandle(root)
	if err != nil {
		return Descriptor{}, "", fmt.Errorf("open WinFsp mount root: %w", err)
	}
	defer windows.CloseHandle(rootHandle)
	existingHandle, err := windowsOpenStorageHandle(existing)
	if err != nil {
		return Descriptor{}, "", fmt.Errorf("open WinFsp storage path: %w", err)
	}
	defer windows.CloseHandle(existingHandle)

	rootFilesystem, err := windowsFilesystemByHandle(rootHandle)
	if err != nil {
		return Descriptor{}, "", fmt.Errorf("observe WinFsp root filesystem: %w", err)
	}
	existingFilesystem, err := windowsFilesystemByHandle(existingHandle)
	if err != nil {
		return Descriptor{}, "", fmt.Errorf("observe WinFsp path filesystem: %w", err)
	}
	if rootFilesystem != "winfsp" {
		return Descriptor{}, "", fmt.Errorf("%w: opened root filesystem is %q", errWindowsWinFspNotApplicable, rootFilesystem)
	}
	if existingFilesystem != rootFilesystem {
		return Descriptor{}, "", fmt.Errorf("resolve WinFsp mount root: opened objects are not one WinFsp filesystem")
	}
	if err := windowsQueryWinFsp(rootHandle); err != nil {
		return Descriptor{}, "", fmt.Errorf("prove WinFsp mount root: %w", err)
	}
	if err := windowsQueryWinFsp(existingHandle); err != nil {
		return Descriptor{}, "", fmt.Errorf("prove WinFsp storage path: %w", err)
	}

	rootNT, err := windowsFinalPathByHandle(rootHandle, windowsVolumeNameNT|windowsFileNameOpened)
	if err != nil {
		return Descriptor{}, "", fmt.Errorf("observe WinFsp root device path: %w", err)
	}
	existingNT, err := windowsFinalPathByHandle(existingHandle, windowsVolumeNameNT|windowsFileNameOpened)
	if err != nil {
		return Descriptor{}, "", fmt.Errorf("observe WinFsp storage device path: %w", err)
	}
	rootNone, err := windowsFinalPathByHandle(rootHandle, windowsVolumeNameNone|windowsFileNameOpened)
	if err != nil {
		return Descriptor{}, "", fmt.Errorf("observe WinFsp root-relative path: %w", err)
	}
	existingNone, err := windowsFinalPathByHandle(existingHandle, windowsVolumeNameNone|windowsFileNameOpened)
	if err != nil {
		return Descriptor{}, "", fmt.Errorf("observe WinFsp storage-relative path: %w", err)
	}
	localRelative, err := windowsCorrelatedWinFspRelative(rootNT, existingNT, rootNone, existingNone)
	if err != nil {
		return Descriptor{}, "", err
	}

	cleanExisting := cleanWindowsPath(existing)
	cleanRequested := cleanWindowsPath(requested)
	missingTail, ok := windowsPathRelative(cleanExisting, cleanRequested)
	if !ok {
		return Descriptor{}, "", fmt.Errorf("resolve WinFsp storage path: requested path is outside opened parent")
	}
	localRelative, err = joinWindowsMountRelative(localRelative, missingTail)
	if err != nil {
		return Descriptor{}, "", err
	}

	var volumeName [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumeNameForVolumeMountPoint(rootPointer, &volumeName[0], uint32(len(volumeName))); err == nil {
		handleVolumeName, handleErr := windowsFinalPathByHandle(rootHandle, windowsVolumeNameGUID|windowsFileNameOpened)
		if handleErr != nil {
			return Descriptor{}, "", fmt.Errorf("correlate WinFsp volume GUID with opened root: %w", handleErr)
		}
		correlatedVolumeName, correlateErr := windowsCorrelatedVolumeGUID(windows.UTF16ToString(volumeName[:]), handleVolumeName)
		if correlateErr != nil {
			return Descriptor{}, "", correlateErr
		}
		descriptor, descriptorErr := DescriptorFromWindowsFacts(requested, "", root, correlatedVolumeName, rootFilesystem, uint32(driveType))
		return descriptor, rootFilesystem, descriptorErr
	} else if !errors.Is(err, windows.ERROR_UNRECOGNIZED_VOLUME) {
		return Descriptor{}, "", fmt.Errorf("resolve WinFsp volume GUID: %w", err)
	}

	descriptor, err := pathOnlyDescriptorAtMount(requested, rootFilesystem, StorageClassLocal, root)
	if err != nil {
		return Descriptor{}, "", err
	}
	descriptor.LocalRelativePath = localRelative
	return descriptor, rootFilesystem, descriptor.Validate()
}

func windowsOpenStorageHandle(value string) (windows.Handle, error) {
	pointer, err := windows.UTF16PtrFromString(value)
	if err != nil {
		return windows.InvalidHandle, err
	}
	return windows.CreateFile(pointer, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
}

func windowsFilesystemByHandle(handle windows.Handle) (string, error) {
	var name [64]uint16
	result, _, callErr := procGetVolumeInformationByHandle.Call(
		uintptr(handle), 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&name[0])), uintptr(len(name)),
	)
	if result == 0 {
		return "", callErr
	}
	filesystem := strings.ToLower(strings.TrimSpace(windows.UTF16ToString(name[:])))
	if filesystem == "" {
		return "", fmt.Errorf("empty native result")
	}
	return filesystem, nil
}

func windowsQueryWinFsp(handle windows.Handle) error {
	var returned uint32
	result, _, callErr := procDeviceIoControl.Call(
		uintptr(handle), uintptr(winFspQueryControlCode), 0, 0, 0, 0,
		uintptr(unsafe.Pointer(&returned)), 0,
	)
	if result == 0 {
		return callErr
	}
	return nil
}

func windowsFinalPathByHandle(handle windows.Handle, flags uint32) (string, error) {
	buffer := make([]uint16, windowsFinalPathUTF16Max)
	length, _, callErr := procGetFinalPathNameByHandle.Call(
		uintptr(handle), uintptr(unsafe.Pointer(&buffer[0])), uintptr(len(buffer)), uintptr(flags),
	)
	if length == 0 {
		return "", callErr
	}
	if length >= uintptr(len(buffer)) {
		return "", fmt.Errorf("native path exceeds allocation limit")
	}
	return windows.UTF16ToString(buffer[:length]), nil
}

func enumeratePlatform() ([]MountedFilesystem, error) {
	volumes, err := windowsMountedVolumes()
	if err != nil {
		return nil, err
	}
	result := make([]MountedFilesystem, 0, len(volumes)+26)
	seen := map[string]bool{}
	for _, mounted := range volumes {
		key, keyErr := mounted.Descriptor.CanonicalKey()
		identity := strings.ToLower(filepath.Clean(mounted.Path)) + "\x00" + key
		if keyErr == nil && !seen[identity] {
			seen[identity] = true
			result = append(result, mounted)
		}
	}
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil, fmt.Errorf("enumerate Windows drives: %w", err)
	}
	for index := 0; index < 26; index++ {
		if mask&(1<<index) == 0 {
			continue
		}
		root := string(rune('A'+index)) + `:\`
		directory, inspectErr := mountedDirectory(root)
		if inspectErr != nil {
			if IsInspectionError(inspectErr) {
				continue
			}
			return nil, inspectErr
		}
		if !directory {
			continue
		}
		descriptor, descriptorErr := resolvePlatform(root, root)
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
		key, _ := descriptor.CanonicalKey()
		identity := strings.ToLower(filepath.Clean(root)) + "\x00" + key
		if !seen[identity] {
			seen[identity] = true
			result = append(result, MountedFilesystem{Path: root, Descriptor: descriptor})
		}
	}
	return result, nil
}

func windowsMountedVolumes() ([]MountedFilesystem, error) {
	var name [windows.MAX_PATH + 1]uint16
	handle, err := windows.FindFirstVolume(&name[0], uint32(len(name)))
	if err != nil {
		return nil, fmt.Errorf("enumerate Windows volumes: %w", err)
	}
	defer windows.FindVolumeClose(handle)
	result := []MountedFilesystem{}
	for {
		volume := windows.UTF16ToString(name[:])
		volumePointer, pointerErr := windows.UTF16PtrFromString(volume)
		if pointerErr != nil {
			return nil, pointerErr
		}
		mounted, inspectErr := windowsVolumeMounts(volume, volumePointer)
		if inspectErr != nil {
			if !IsInspectionError(inspectErr) {
				return nil, inspectErr
			}
		} else {
			result = append(result, mounted...)
		}
		if err := windows.FindNextVolume(handle, &name[0], uint32(len(name))); err != nil {
			if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
				break
			}
			return nil, fmt.Errorf("enumerate next Windows volume: %w", err)
		}
	}
	return result, nil
}

// A volume's mount-path query may fail when media disappears. Keep that failure
// local to the volume; inventory iteration errors and malformed facts still fail
// the complete request. No second query/retry or additional allocation is added.
func windowsVolumeMounts(volume string, volumePointer *uint16) ([]MountedFilesystem, error) {
	var needed uint32
	pathErr := windows.GetVolumePathNamesForVolumeName(volumePointer, nil, 0, &needed)
	if pathErr != nil && !errors.Is(pathErr, windows.ERROR_MORE_DATA) {
		return nil, pathErr
	}
	result, paths, err := prepareWindowsVolumePathNames(nil, needed)
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return result, nil
	}
	if err := windows.GetVolumePathNamesForVolumeName(volumePointer, &paths[0], needed, &needed); err != nil {
		return nil, err
	}
	for _, path := range splitWindowsMultiString(paths) {
		rootPointer, err := windows.UTF16PtrFromString(path)
		if err != nil {
			return nil, err
		}
		driveType := windows.GetDriveType(rootPointer)
		if driveType != windows.DRIVE_FIXED && driveType != windows.DRIVE_REMOVABLE {
			continue
		}
		directory, inspectErr := mountedDirectory(path)
		if inspectErr != nil {
			if IsInspectionError(inspectErr) {
				continue
			}
			return nil, inspectErr
		}
		if !directory {
			continue
		}
		filesystem, filesystemErr := windowsFilesystem(rootPointer)
		if filesystemErr != nil {
			if IsInspectionError(filesystemErr) {
				continue
			}
			return nil, filesystemErr
		}
		descriptor, descriptorErr := DescriptorFromWindowsFacts(path, "", path, volume, filesystem, uint32(driveType))
		if descriptorErr != nil {
			return nil, descriptorErr
		}
		result = append(result, MountedFilesystem{Path: path, Descriptor: descriptor})
	}
	return result, nil
}

func splitWindowsMultiString(values []uint16) []string {
	result := []string{}
	start := 0
	for index, value := range values {
		if value != 0 {
			continue
		}
		if index == start {
			break
		}
		result = append(result, windows.UTF16ToString(values[start:index]))
		start = index + 1
	}
	return result
}

func windowsNetworkProvider(remote string) (string, Kind, error) {
	pointer, err := windows.UTF16PtrFromString(remote)
	if err != nil {
		return "", "", err
	}
	resource := windowsNetResource{Type: 1, RemoteName: pointer}
	size := uint32(16 << 10)
	buffer := make([]byte, size)
	var system *uint16
	result, _, _ := procWNetGetResourceInformation.Call(uintptr(unsafe.Pointer(&resource)), uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&system)))
	if result != 0 {
		return "", "", fmt.Errorf("resolve mapped network provider: %w", syscall.Errno(result))
	}
	observed := (*windowsNetResource)(unsafe.Pointer(&buffer[0]))
	provider := strings.TrimSpace(windows.UTF16PtrToString(observed.Provider))
	kind, err := WindowsNetworkProviderKind(provider)
	if err != nil {
		return provider, kind, fmt.Errorf("%w: %v", errWindowsNetworkProviderUnproven, err)
	}
	return provider, kind, err
}

func windowsVolumeRoot(value string) (string, error) {
	pointer, err := windows.UTF16PtrFromString(value)
	if err != nil {
		return "", err
	}
	var root [windows.MAX_PATH + 1]uint16
	if err := windows.GetVolumePathName(pointer, &root[0], uint32(len(root))); err != nil {
		return "", fmt.Errorf("resolve volume root: %w", err)
	}
	return windows.UTF16ToString(root[:]), nil
}

func windowsFilesystem(root *uint16) (string, error) {
	var name [64]uint16
	if err := windows.GetVolumeInformation(root, nil, 0, nil, nil, nil, &name[0], uint32(len(name))); err != nil {
		return "", fmt.Errorf("observe filesystem type: %w", err)
	}
	filesystem := strings.ToLower(strings.TrimSpace(windows.UTF16ToString(name[:])))
	if filesystem == "" {
		return "", fmt.Errorf("observe filesystem type: empty native result")
	}
	return filesystem, nil
}

func windowsUniversalName(value string) (string, error) {
	pointer, err := windows.UTF16PtrFromString(value)
	if err != nil {
		return "", err
	}
	size := uint32(1024)
	for attempts := 0; attempts < 2; attempts++ {
		buffer := make([]byte, size)
		result, _, _ := procWNetGetUniversalName.Call(
			uintptr(unsafe.Pointer(pointer)), uintptr(1),
			uintptr(unsafe.Pointer(&buffer[0])), uintptr(unsafe.Pointer(&size)),
		)
		if result == 0 {
			namePointer := *(**uint16)(unsafe.Pointer(&buffer[0]))
			return windows.UTF16PtrToString(namePointer), nil
		}
		if syscall.Errno(result) != windows.ERROR_MORE_DATA {
			return "", syscall.Errno(result)
		}
	}
	return "", fmt.Errorf("universal name exceeds bounds")
}
