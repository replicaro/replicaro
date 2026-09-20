package storageidentity

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strconv"
	"strings"
)

type LinuxMount struct {
	// MountID is a run-local lookup fact, never a persistent storage identity.
	MountID    uint64
	MajorMinor string
	Root       string
	MountPoint string
	Filesystem string
	Source     string
}

func ParseLinuxMountInfo(reader io.Reader) ([]LinuxMount, error) {
	const maximumMountInfoBytes = 4 << 20
	data, err := io.ReadAll(io.LimitReader(reader, maximumMountInfoBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	if len(data) > maximumMountInfoBytes {
		return nil, fmt.Errorf("mountinfo exceeds 4 MiB limit")
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 256<<10)
	var mounts []LinuxMount
	for scanner.Scan() {
		line := scanner.Text()
		halves := strings.Split(line, " - ")
		if len(halves) != 2 {
			return nil, fmt.Errorf("malformed mountinfo separator")
		}
		left, right := strings.Fields(halves[0]), strings.Fields(halves[1])
		if len(left) < 6 || len(right) < 3 {
			return nil, fmt.Errorf("malformed mountinfo record")
		}
		mountID, err := strconv.ParseUint(left[0], 10, 64)
		if err != nil || mountID == 0 {
			return nil, fmt.Errorf("invalid mountinfo mount ID")
		}
		root, err := decodeMountInfo(left[3])
		if err != nil {
			return nil, err
		}
		point, err := decodeMountInfo(left[4])
		if err != nil {
			return nil, err
		}
		source, err := decodeMountInfo(right[1])
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, LinuxMount{
			MountID: mountID, MajorMinor: left[2], Root: path.Clean(root), MountPoint: path.Clean(point),
			Filesystem: strings.ToLower(right[0]), Source: source,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read mountinfo: %w", err)
	}
	return mounts, nil
}

func decodeMountInfo(value string) (string, error) {
	var result strings.Builder
	for index := 0; index < len(value); index++ {
		if value[index] != '\\' {
			result.WriteByte(value[index])
			continue
		}
		if index+3 >= len(value) {
			return "", fmt.Errorf("truncated mountinfo escape")
		}
		number, err := strconv.ParseUint(value[index+1:index+4], 8, 8)
		if err != nil {
			return "", fmt.Errorf("invalid mountinfo escape")
		}
		result.WriteByte(byte(number))
		index += 3
	}
	return result.String(), nil
}

func linuxMountByID(mounts []LinuxMount, id uint64) (LinuxMount, error) {
	var selected LinuxMount
	found := false
	for _, mount := range mounts {
		if mount.MountID != id {
			continue
		}
		if found {
			return LinuxMount{}, fmt.Errorf("duplicate mountinfo mount ID")
		}
		selected, found = mount, true
	}
	if !found {
		return LinuxMount{}, fmt.Errorf("observed mount is absent from mountinfo")
	}
	return selected, nil
}

type LinuxBlockFacts struct {
	VolumeUUID string
}

type LinuxBlockFactsLookup func(LinuxMount) (LinuxBlockFacts, error)

func DescriptorFromLinuxMount(value string, mount LinuxMount, blockFacts LinuxBlockFactsLookup) (Descriptor, error) {
	relative, err := filepath.Rel(mount.MountPoint, value)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Descriptor{}, fmt.Errorf("storage path is outside selected mount")
	}
	relative = cleanRelative(filepath.ToSlash(relative))
	filesystem := strings.ToLower(strings.TrimSpace(mount.Filesystem))
	switch filesystem {
	case "cifs", "smb3", "smbfs":
		endpoint, share, err := ParseSMBSource(mount.Source)
		if err != nil {
			return Descriptor{}, err
		}
		d := Descriptor{Version: DescriptorVersion, Kind: KindSMB, Filesystem: filesystem,
			Endpoint: endpoint, Share: share, MountRoot: cleanRoot(mount.Root), RelativePath: relative,
			StorageClass: StorageClassNetwork}
		return d, d.Validate()
	case "nfs", "nfs4":
		endpoint, export, err := ParseNFSSource(mount.Source)
		if err != nil {
			return Descriptor{}, err
		}
		d := Descriptor{Version: DescriptorVersion, Kind: KindNFS, Filesystem: filesystem,
			Endpoint: endpoint, Export: export, MountRoot: cleanRoot(mount.Root), RelativePath: relative,
			StorageClass: StorageClassNetwork}
		return d, d.Validate()
	}
	if isAmbiguousLinuxMountFilesystem(filesystem) {
		return pathOnlyDescriptor(value, filesystem, StorageClassOther)
	}
	if !isAuthoritativeLocalLinuxFilesystem(filesystem) &&
		!strings.HasPrefix(mount.Source, "/dev/") && filesystem != "btrfs" {
		return pathOnlyDescriptor(value, filesystem, StorageClassOther)
	}
	if strings.HasPrefix(mount.Source, "/dev/") || filesystem == "btrfs" {
		if blockFacts == nil {
			return Descriptor{}, fmt.Errorf("authoritative filesystem identity is unavailable")
		}
		facts, err := blockFacts(mount)
		if err != nil {
			return Descriptor{}, fmt.Errorf("authoritative filesystem identity is unavailable: %w", err)
		}
		uuid := strings.TrimSpace(facts.VolumeUUID)
		if uuid == "" {
			return pathOnlyDescriptorAtMount(value, filesystem, StorageClassLocal, mount.MountPoint)
		}
		d := Descriptor{Version: DescriptorVersion, Kind: KindVolume, Filesystem: filesystem,
			VolumeID: strings.ToLower(strings.TrimSpace(uuid)), MountRoot: cleanRoot(mount.Root),
			RelativePath: relative, StorageClass: StorageClassLocal}
		return d, d.Validate()
	}
	// These explicitly classified local filesystems retain their authoritative
	// local class and actual type in the one identityless source binding.
	// Unknown and ambiguous filesystems above remain classed as other.
	return pathOnlyDescriptorAtMount(value, filesystem, StorageClassLocal, mount.MountPoint)
}

func isAmbiguousLinuxMountFilesystem(filesystem string) bool {
	if strings.HasPrefix(filesystem, "fuse") {
		return true
	}
	switch filesystem {
	case "9p", "afs", "ceph", "coda", "curlftpfs", "davfs", "davfs2",
		"gcsfuse", "glusterfs", "sshfs":
		return true
	default:
		return false
	}
}

func isAuthoritativeLocalLinuxFilesystem(filesystem string) bool {
	switch filesystem {
	// These non-block-backed filesystems have local process/host storage
	// semantics. Unknown and FUSE-backed filesystems remain ambiguous: a
	// disconnected network mount can expose a different directory at the same
	// pathname and therefore must not inherit local identity.
	case "aufs", "overlay", "ramfs", "rootfs", "tmpfs", "zfs":
		return true
	default:
		return false
	}
}

func ParseSMBSource(source string) (endpoint, share string, err error) {
	source = strings.ReplaceAll(strings.TrimSpace(source), `\`, "/")
	if !strings.HasPrefix(source, "//") {
		return "", "", fmt.Errorf("SMB mount source must be an exact UNC share")
	}
	parts := strings.Split(strings.TrimPrefix(source, "//"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("SMB mount source is ambiguous")
	}
	// Darwin may expose a mounted SMB source as //user@server/share. The
	// authenticated user is not part of the physical share identity.
	if index := strings.LastIndex(parts[0], "@"); index >= 0 {
		parts[0] = parts[0][index+1:]
		if parts[0] == "" {
			return "", "", fmt.Errorf("SMB mount source is ambiguous")
		}
	}
	return strings.ToLower(parts[0]), strings.ToLower(parts[1]), nil
}

func ParseNFSSource(source string) (endpoint, export string, err error) {
	index := strings.Index(source, ":/")
	if index <= 0 {
		return "", "", fmt.Errorf("NFS mount source must identify server and export")
	}
	endpoint, export = source[:index], source[index+1:]
	if endpoint == "" || export == "" || hasDotSegment(export) {
		return "", "", fmt.Errorf("NFS mount source is ambiguous")
	}
	return strings.ToLower(endpoint), cleanRoot(export), nil
}
