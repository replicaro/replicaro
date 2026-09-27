package storageidentity

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"path"
	"strconv"
	"strings"
)

// LinuxMount is one parsed /proc/self/mountinfo record. The parser is kept
// platform-independent so it can be tested on any host.
type LinuxMount struct {
	// MountID is a run-local lookup key for the opened folder's mount. It is
	// never persisted.
	MountID    uint64
	MountPoint string
	Filesystem string
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
		// Only the mount ID, mount point, and filesystem type are recorded
		// facts; the other fields (major:minor, root, source) are not read.
		point, err := decodeMountInfo(left[4])
		if err != nil {
			return nil, err
		}
		mounts = append(mounts, LinuxMount{
			MountID: mountID, MountPoint: path.Clean(point), Filesystem: strings.ToLower(right[0]),
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
