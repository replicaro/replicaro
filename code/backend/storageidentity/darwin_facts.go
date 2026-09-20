package storageidentity

import (
	"fmt"
	"path/filepath"
	"strings"
)

type DarwinMount struct {
	MountPoint string
	Source     string
	Filesystem string
	VolumeUUID string
}

func DescriptorFromDarwinMount(value string, mount DarwinMount) (Descriptor, error) {
	relative, err := filepath.Rel(mount.MountPoint, value)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return Descriptor{}, fmt.Errorf("storage path is outside selected mount")
	}
	relative = cleanRelative(filepath.ToSlash(relative))
	filesystem := strings.ToLower(strings.TrimSpace(mount.Filesystem))
	switch filesystem {
	case "smbfs", "cifs":
		endpoint, share, err := ParseSMBSource(mount.Source)
		if err != nil {
			return Descriptor{}, err
		}
		d := Descriptor{Version: DescriptorVersion, Kind: KindSMB, Filesystem: filesystem,
			Endpoint: endpoint, Share: share, MountRoot: "/", RelativePath: relative,
			StorageClass: StorageClassNetwork}
		return d, d.Validate()
	case "nfs":
		endpoint, export, err := ParseNFSSource(mount.Source)
		if err != nil {
			return Descriptor{}, err
		}
		d := Descriptor{Version: DescriptorVersion, Kind: KindNFS, Filesystem: filesystem,
			Endpoint: endpoint, Export: export, MountRoot: "/", RelativePath: relative,
			StorageClass: StorageClassNetwork}
		return d, d.Validate()
	}
	if strings.HasPrefix(mount.Source, "/dev/") {
		if strings.TrimSpace(mount.VolumeUUID) == "" {
			return pathOnlyDescriptorAtMount(value, filesystem, StorageClassLocal, mount.MountPoint)
		}
		d := Descriptor{Version: DescriptorVersion, Kind: KindVolume, Filesystem: filesystem,
			VolumeID: strings.ToLower(strings.TrimSpace(mount.VolumeUUID)), MountRoot: "/",
			RelativePath: relative, StorageClass: StorageClassLocal}
		return d, d.Validate()
	}
	return pathOnlyDescriptor(value, filesystem, StorageClassOther)
}

// Stable identities use volume-relative native geometry. Identityless bindings
// retain configured-path geometry, including the mount root used to derive a
// replacement-media candidate. Do not mix ordinary and no-firmlink namespaces.
func descriptorFromDarwinObservation(resolved, nativePath, nativeRoot string, mount DarwinMount) (Descriptor, error) {
	nativeMount := mount
	nativeMount.MountPoint = nativeRoot
	descriptor, err := DescriptorFromDarwinMount(nativePath, nativeMount)
	if err != nil || descriptor.Kind != KindPathOnly {
		return descriptor, err
	}
	routeRoot := mount.MountPoint
	relative, relativeErr := filepath.Rel(routeRoot, resolved)
	if relativeErr != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		routeRoot = nativeRoot
	}
	return pathOnlyDescriptorAtMount(resolved, descriptor.Filesystem, descriptor.StorageClass, routeRoot)
}
