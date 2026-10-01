//go:build linux

package engines

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

type rcloneRemoteNamespaceMount struct {
	id               uint64
	device           string
	root, mountPoint string
}

type rcloneRemoteNamespaceLocation struct {
	device, path, openedPath string
	mountID                  uint64
}

var rcloneRemoteNamespaceMountID = rcloneRemoteOpenedMountID

func checkRcloneRemoteNamespaceAliases(configFile, realFile string, roots []string) error {
	mounts, err := rcloneRemoteNamespaceMounts()
	if err != nil {
		return err
	}
	var managed []rcloneRemoteNamespaceLocation
	for _, root := range roots {
		location, err := rcloneRemoteLinuxLocation(root, mounts)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		managed = append(managed, location)
		// A managed child may be a separate filesystem. Include its visible
		// mount, but ignore records hidden by another mount on the same route.
		for _, mount := range mounts {
			if !rcloneRemotePathWithin(mount.mountPoint, location.openedPath) || mount.id == location.mountID {
				continue
			}
			child, err := rcloneRemoteLinuxLocation(mount.mountPoint, mounts)
			if errors.Is(err, fs.ErrNotExist) {
				continue
			}
			if err != nil {
				return err
			}
			if child.mountID == mount.id {
				managed = append(managed, child)
			}
		}
	}
	for _, route := range []string{configFile, realFile} {
		for current := route; ; current = filepath.Dir(current) {
			location, err := rcloneRemoteLinuxLocation(current, mounts)
			if err != nil {
				return err
			}
			for _, root := range managed {
				if location.device == root.device && rcloneRemotePathWithin(location.path, root.path) {
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

func rcloneRemoteLinuxLocation(path string, mounts []rcloneRemoteNamespaceMount) (rcloneRemoteNamespaceLocation, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return rcloneRemoteNamespaceLocation{}, err
	}
	defer unix.Close(fd)
	id, err := rcloneRemoteNamespaceMountID(fd)
	if err != nil {
		return rcloneRemoteNamespaceLocation{}, err
	}
	opened, err := rcloneRemoteLinuxOpenedPath(fd, path)
	if err != nil {
		return rcloneRemoteNamespaceLocation{}, err
	}
	return rcloneRemoteLinuxLocationOnMount(opened, id, mounts)
}

func rcloneRemoteLinuxOpenedPath(fd int, route string) (string, error) {
	opened, err := os.Readlink("/proc/self/fd/" + strconv.Itoa(fd))
	if err != nil {
		return "", err
	}
	// proc appends this suffix to unlinked dentries, but a real filename can
	// also end with it. Require both names to still identify the opened file.
	if strings.HasSuffix(opened, " (deleted)") {
		var descriptor, selected, named unix.Stat_t
		if err := unix.Fstat(fd, &descriptor); err != nil {
			return "", err
		}
		if descriptor.Nlink == 0 {
			return "", fmt.Errorf("opened config route was removed")
		}
		if err := unix.Stat(route, &selected); err != nil {
			return "", err
		}
		if err := unix.Stat(opened, &named); err != nil {
			return "", err
		}
		if descriptor.Dev != selected.Dev || descriptor.Ino != selected.Ino || descriptor.Dev != named.Dev || descriptor.Ino != named.Ino {
			return "", fmt.Errorf("opened config route changed")
		}
	}
	return opened, nil
}

func rcloneRemoteLinuxLocationOnMount(opened string, id uint64, mounts []rcloneRemoteNamespaceMount) (rcloneRemoteNamespaceLocation, error) {
	if !filepath.IsAbs(opened) {
		return rcloneRemoteNamespaceLocation{}, fmt.Errorf("opened config route has no comparable path")
	}
	var selected *rcloneRemoteNamespaceMount
	for index := range mounts {
		if mounts[index].id == id {
			if selected != nil {
				return rcloneRemoteNamespaceLocation{}, fmt.Errorf("opened config mount is ambiguous")
			}
			selected = &mounts[index]
		}
	}
	if selected == nil || !filepath.IsAbs(selected.root) || filepath.Clean(selected.root) != selected.root || !rcloneRemotePathWithin(opened, selected.mountPoint) {
		return rcloneRemoteNamespaceLocation{}, fmt.Errorf("opened config mount is unavailable")
	}
	relative, err := filepath.Rel(selected.mountPoint, opened)
	if err != nil {
		return rcloneRemoteNamespaceLocation{}, err
	}
	return rcloneRemoteNamespaceLocation{device: selected.device, path: filepath.Join(selected.root, relative),
		openedPath: filepath.Clean(opened), mountID: id}, nil
}

func rcloneRemoteOpenedMountID(fd int) (uint64, error) {
	var stat unix.Statx_t
	if err := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_NO_AUTOMOUNT, unix.STATX_MNT_ID, &stat); err == nil && stat.Mask&unix.STATX_MNT_ID != 0 && stat.Mnt_id != 0 {
		return stat.Mnt_id, nil
	}
	// Older kernels expose the opened handle's mount ID through fdinfo.
	file, err := os.Open("/proc/self/fdinfo/" + strconv.Itoa(fd))
	if err != nil {
		return 0, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(io.LimitReader(file, 16<<10))
	for scanner.Scan() {
		name, value, ok := strings.Cut(scanner.Text(), ":")
		if ok && name == "mnt_id" {
			id, err := strconv.ParseUint(strings.TrimSpace(value), 10, 64)
			if err == nil && id != 0 {
				return id, nil
			}
			return 0, fmt.Errorf("opened config mount ID is invalid")
		}
	}
	return 0, errors.Join(fmt.Errorf("opened config mount ID is unavailable"), scanner.Err())
}

func rcloneRemoteNamespaceMounts() ([]rcloneRemoteNamespaceMount, error) {
	file, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer file.Close()
	const maxTableBytes = 4 << 20
	data, err := io.ReadAll(io.LimitReader(file, maxTableBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxTableBytes {
		return nil, fmt.Errorf("config namespace mount table is too large")
	}
	return parseRcloneRemoteNamespaceMounts(string(data))
}

func parseRcloneRemoteNamespaceMounts(data string) ([]rcloneRemoteNamespaceMount, error) {
	var mounts []rcloneRemoteNamespaceMount
	for _, line := range strings.Split(strings.TrimSuffix(data, "\n"), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 10 || !strings.Contains(line, " - ") {
			return nil, fmt.Errorf("config namespace mount record is invalid")
		}
		id, err := strconv.ParseUint(fields[0], 10, 64)
		major, minor, validDevice := strings.Cut(fields[2], ":")
		_, majorErr := strconv.ParseUint(major, 10, 32)
		_, minorErr := strconv.ParseUint(minor, 10, 32)
		root, point := decodeMountInfoPath(fields[3]), decodeMountInfoPath(fields[4])
		if err != nil || id == 0 || !validDevice || majorErr != nil || minorErr != nil || !filepath.IsAbs(point) || filepath.Clean(point) != point {
			return nil, fmt.Errorf("config namespace mount identity is invalid")
		}
		mounts = append(mounts, rcloneRemoteNamespaceMount{id: id, device: fields[2], root: root, mountPoint: point})
	}
	return mounts, nil
}
