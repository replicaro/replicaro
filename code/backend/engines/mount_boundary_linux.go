//go:build linux

package engines

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func destinationMountSpecific(path string) error {
	mounts, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("inspect Linux mount table: %w", err)
	}
	defer mounts.Close()
	path = filepath.Clean(path)
	scanner := bufio.NewScanner(mounts)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 6 {
			continue
		}
		mountPoint := decodeMountInfoPath(fields[4])
		if filepath.Clean(mountPoint) == path {
			return fmt.Errorf("restore destination is a mountpoint: %q", path)
		}
	}
	return scanner.Err()
}

func decodeMountInfoPath(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		if value[i] == '\\' && i+3 < len(value) {
			if n, err := strconv.ParseUint(value[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(value[i])
	}
	return b.String()
}
