package storageidentity

import (
	"errors"
	"fmt"
	"strings"
	"syscall"
)

var errWindowsNetworkProviderUnproven = errors.New("Windows network provider does not prove SMB or NFS")

const maximumWindowsVolumePathNamesUTF16 = 1 << 20

// windowsVolumePathNamesBuffer is kept with the platform-fact code so the
// Windows API allocation boundary is testable without a Windows kernel call.
// An over-bound report fails the entire enumeration; it is never treated as a
// volume with zero mount paths.
func windowsVolumePathNamesBuffer(needed uint32) ([]uint16, error) {
	if needed > maximumWindowsVolumePathNamesUTF16 {
		return nil, fmt.Errorf("Windows volume mount paths exceed allocation limit")
	}
	if needed == 0 {
		return nil, nil
	}
	return make([]uint16, needed), nil
}

func prepareWindowsVolumePathNames(current []MountedFilesystem, needed uint32) ([]MountedFilesystem, []uint16, error) {
	paths, err := windowsVolumePathNamesBuffer(needed)
	if err != nil {
		// Returning nil here is deliberate: callers must not expose candidates
		// retained from earlier volumes after one volume exceeds the bound.
		return nil, nil, err
	}
	return current, paths, nil
}

func ParseUNC(value string) (endpoint, share, relative string, err error) {
	endpoint, share, relative, err = parseUNCComponents(value)
	return endpoint, strings.ToLower(share), relative, err
}

func parseUNCComponents(value string) (endpoint, share, relative string, err error) {
	value = strings.ReplaceAll(value, "/", `\`)
	for strings.HasPrefix(strings.ToLower(value), strings.ToLower(`\\?\UNC\`)) {
		value = `\\` + value[len(`\\?\UNC\`):]
	}
	if !strings.HasPrefix(value, `\\`) {
		return "", "", "", fmt.Errorf("path is not UNC")
	}
	parts := strings.FieldsFunc(strings.TrimPrefix(value, `\\`), func(r rune) bool { return r == '\\' })
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", "", "", fmt.Errorf("UNC path must include server and share")
	}
	for _, part := range parts {
		if part == "." || part == ".." {
			return "", "", "", fmt.Errorf("UNC path contains ambiguous traversal")
		}
	}
	return strings.ToLower(parts[0]), parts[1],
		cleanRelative(strings.Join(parts[2:], "/")), nil
}

func DescriptorFromWindowsFacts(requested, universal, volumeRoot, volumeGUID, filesystem string, driveType uint32) (Descriptor, error) {
	return DescriptorFromWindowsNetworkFacts(requested, universal, volumeRoot, volumeGUID, filesystem, driveType, "microsoft windows network", KindSMB)
}

func DescriptorFromWindowsNetworkFacts(requested, universal, volumeRoot, volumeGUID, filesystem string, driveType uint32, provider string, networkKind Kind) (Descriptor, error) {
	filesystem = strings.ToLower(strings.TrimSpace(filesystem))
	if universal != "" || isWindowsUNC(requested) {
		value := universal
		if value == "" {
			value = requested
		}
		endpoint, share, relative, err := parseUNCComponents(value)
		if err != nil {
			return Descriptor{}, err
		}
		provider = strings.ToLower(strings.TrimSpace(provider))
		classifiedKind, classifyErr := WindowsNetworkProviderKind(provider)
		if classifyErr != nil || classifiedKind != networkKind {
			return pathOnlyDescriptor(requested, filesystem, StorageClassNetwork)
		}
		if filesystem == "" {
			return Descriptor{}, fmt.Errorf("observed Windows filesystem type is unavailable")
		}
		// Stable Windows network key semantics use the proven protocol kind. The
		// actual filesystem type travels as explicit observation evidence instead
		// of hidden descriptor construction state.
		d := Descriptor{Version: DescriptorVersion, Kind: networkKind, Filesystem: string(networkKind),
			Endpoint: endpoint, Provider: provider, MountRoot: "/", RelativePath: relative,
			StorageClass: StorageClassNetwork}
		if networkKind == KindSMB {
			d.Share = strings.ToLower(share)
		} else {
			d.Export = "/" + share
		}
		return d, d.Validate()
	}
	if driveType == 4 { // DRIVE_REMOTE
		return pathOnlyDescriptor(requested, filesystem, StorageClassNetwork)
	}
	canonical := cleanWindowsPath(requested)
	root := cleanWindowsPath(volumeRoot)
	if (driveType == 2 || driveType == 3) && strings.TrimSpace(volumeGUID) == "" {
		return pathOnlyDescriptorAtMount(requested, filesystem, StorageClassLocal, volumeRoot)
	}
	if driveType == 2 || driveType == 3 { // device-backed local volume
		volumeGUID = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(volumeGUID, `\`)))
		if !strings.HasPrefix(volumeGUID, `\\?\volume{`) || !strings.HasSuffix(volumeGUID, "}") {
			return Descriptor{}, fmt.Errorf("stable Windows volume GUID is unavailable")
		}
		if len(canonical) < 3 || canonical[1] != ':' || len(root) < 3 {
			return Descriptor{}, fmt.Errorf("volume path is not absolute")
		}
		relative, ok := windowsPathRelative(root, canonical)
		if !ok {
			return Descriptor{}, fmt.Errorf("volume path is outside the OS-proven volume root")
		}
		d := Descriptor{Version: DescriptorVersion, Kind: KindVolume, Filesystem: filesystem,
			VolumeID: volumeGUID, MountRoot: root, RelativePath: relative, StorageClass: StorageClassLocal}
		return d, d.Validate()
	}
	return pathOnlyDescriptor(requested, filesystem, StorageClassOther)
}

func descriptorFromWindowsVolumeGUIDAbsence(requested, volumeRoot, filesystem string, driveType uint32) (Descriptor, error) {
	switch driveType {
	case 2, 3: // Both native local drive types have the same binding semantics.
		return pathOnlyDescriptorAtMount(requested, filesystem, StorageClassLocal, volumeRoot)
	default:
		return Descriptor{}, fmt.Errorf("volume GUID absence is inapplicable to drive type")
	}
}

// WindowsNetworkProviderKind accepts only redirectors whose native provider
// identity proves SMB or NFS. Same-spelling UNC paths from WebDAV or an unknown
// redirector deliberately do not inherit a supported network identity.
func WindowsNetworkProviderKind(provider string) (Kind, error) {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "microsoft windows network":
		return KindSMB, nil
	case "nfs network", "microsoft nfs network":
		return KindNFS, nil
	default:
		return "", fmt.Errorf("mapped network provider %q is not proven SMB or NFS", provider)
	}
}

func cleanWindowsPath(value string) string {
	// Windows can enable case sensitivity per directory. Only the filesystem
	// spelling proof may select another spelling; identity must keep descendants.
	return cleanWindowsConfiguredPath(value)
}

func cleanWindowsConfiguredPath(value string) string {
	value = strings.ReplaceAll(value, "/", `\`)
	prefix, remainder, protected := "", value, 0
	switch {
	case strings.HasPrefix(strings.ToLower(value), `\\?\unc\`):
		prefix, remainder, protected = value[:len(`\\?\unc\`)], value[len(`\\?\unc\`):], 2
	case strings.HasPrefix(value, `\\`):
		prefix, remainder, protected = `\\`, value[2:], 2
	case len(value) >= 3 && value[1] == ':' && value[2] == '\\':
		prefix, remainder, protected = value[:3], value[3:], 0
	default:
		return ""
	}
	parts := strings.Split(remainder, `\`)
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(cleaned) > protected {
				cleaned = cleaned[:len(cleaned)-1]
			}
		default:
			cleaned = append(cleaned, part)
		}
	}
	if len(cleaned) < protected {
		return ""
	}
	joined := strings.Join(cleaned, `\`)
	if prefix == `\\` || strings.EqualFold(prefix, `\\?\unc\`) {
		if len(cleaned) < 2 {
			return ""
		}
		return prefix + joined
	}
	if joined == "" {
		return prefix
	}
	return prefix + joined
}

// windowsUniversalNameIdentityUnavailable accepts only WNetGetUniversalName
// outcomes that conclusively say universal names are unsupported or the local
// device is not redirected. Invalid input, remembered-but-unavailable mappings,
// provider ambiguity, and network failures must remain observation failures.
func windowsUniversalNameIdentityUnavailable(err error) bool {
	for _, code := range []syscall.Errno{50, 2250} { // ERROR_NOT_SUPPORTED, ERROR_NOT_CONNECTED
		if errors.Is(err, code) {
			return true
		}
	}
	return false
}

// windowsProviderIdentityUnavailable is deliberately separate because
// WNetGetResourceInformation assigns different meaning to MPR result codes.
// ERROR_BAD_NET_NAME conclusively says no network provider owns the resource;
// all malformed, transient, and unavailable-network results fail closed.
func windowsProviderIdentityUnavailable(err error) bool {
	return errors.Is(err, syscall.Errno(67)) // ERROR_BAD_NET_NAME
}

func windowsPathRelative(root, value string) (string, bool) {
	// A drive letter names the same OS volume independent of its case. That
	// rule stops at the volume prefix: descendants can be case-sensitive.
	if len(root) >= 2 && len(value) >= 2 && root[1] == ':' && value[1] == ':' && strings.EqualFold(root[:2], value[:2]) {
		value = root[:2] + value[2:]
	}
	root = strings.TrimSuffix(root, `\`)
	value = strings.TrimSuffix(value, `\`)
	if value == root {
		return ".", true
	}
	prefix := root + `\`
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	return cleanRelative(strings.ReplaceAll(strings.TrimPrefix(value, prefix), `\`, "/")), true
}

func windowsCorrelatedWinFspRelative(rootNT, existingNT, rootNone, existingNone string) (string, error) {
	if rootNone != `\` {
		return "", fmt.Errorf("resolve WinFsp geometry: native root is not volume-relative root")
	}
	if !strings.HasSuffix(rootNT, `\`) {
		return "", fmt.Errorf("resolve WinFsp geometry: native root device is not a root")
	}
	rootDevice := strings.TrimSuffix(rootNT, `\`)
	if rootDevice == "" || !strings.HasPrefix(rootDevice, `\Device\`) {
		return "", fmt.Errorf("resolve WinFsp geometry: native root device is malformed")
	}
	if len(existingNT) < len(rootDevice) || !strings.EqualFold(existingNT[:len(rootDevice)], rootDevice) {
		return "", fmt.Errorf("resolve WinFsp geometry: storage path is on another native device")
	}
	nativeSuffix := existingNT[len(rootDevice):]
	if nativeSuffix != existingNone {
		return "", fmt.Errorf("resolve WinFsp geometry: native path views disagree")
	}
	return validateWindowsMountRelative(existingNone)
}

func windowsCorrelatedVolumeGUID(pathGUID, handleGUID string) (string, error) {
	normalize := func(value string) (string, bool) {
		value = strings.ToLower(strings.TrimSpace(strings.TrimSuffix(value, `\`)))
		return value, strings.HasPrefix(value, `\\?\volume{`) && strings.HasSuffix(value, "}")
	}
	pathValue, pathOK := normalize(pathGUID)
	handleValue, handleOK := normalize(handleGUID)
	if !pathOK || !handleOK || pathValue != handleValue {
		return "", fmt.Errorf("resolve WinFsp volume GUID: mount path and opened root disagree")
	}
	return pathValue, nil
}

func validateWindowsMountRelative(value string) (string, error) {
	if value == `\` {
		return ".", nil
	}
	if !strings.HasPrefix(value, `\`) || strings.HasPrefix(value, `\\`) {
		return "", fmt.Errorf("resolve WinFsp geometry: native relative path is malformed")
	}
	parts := strings.Split(strings.TrimPrefix(value, `\`), `\`)
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, `:/`) {
			return "", fmt.Errorf("resolve WinFsp geometry: native relative path is not canonical")
		}
	}
	return strings.Join(parts, "/"), nil
}

func joinWindowsMountRelative(base, tail string) (string, error) {
	if base == "" || tail == "" || pathHasWindowsMountTraversal(base) || pathHasWindowsMountTraversal(tail) {
		return "", fmt.Errorf("resolve WinFsp geometry: mount-relative path is not canonical")
	}
	if base == "." {
		return tail, nil
	}
	if tail == "." {
		return base, nil
	}
	return base + "/" + tail, nil
}

func pathHasWindowsMountTraversal(value string) bool {
	if value != cleanRelative(value) || strings.ContainsAny(value, `\:`) {
		return true
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." && value != "." || part == ".." {
			return true
		}
	}
	return false
}

func isWindowsUNC(value string) bool {
	value = strings.ToLower(value)
	return strings.HasPrefix(value, `\\?\unc\`) ||
		(strings.HasPrefix(value, `\\`) && !strings.HasPrefix(value, `\\?\`))
}
