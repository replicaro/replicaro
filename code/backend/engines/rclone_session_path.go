//go:build !windows

package engines

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func prepareRcloneSessionRoot(target string) error {
	absolute, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	current := string(filepath.Separator)
	volume := filepath.VolumeName(absolute)
	if volume != "" {
		current = volume + string(filepath.Separator)
	}
	relative := strings.TrimPrefix(absolute[len(volume):], string(filepath.Separator))
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		if err := rejectRcloneCaseAlias(current, component); err != nil {
			return err
		}
		current = filepath.Join(current, component)
		info, statErr := os.Lstat(current)
		created := false
		if os.IsNotExist(statErr) {
			if err := os.Mkdir(current, 0o700); err != nil &&
				!errors.Is(err, os.ErrExist) {
				return fmt.Errorf("create private rclone session directory: %w", err)
			}
			created = true
			info, statErr = os.Lstat(current)
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("private rclone session ancestor is not a real directory: %s", current)
		}
		if (created || current == target || current == filepath.Dir(target)) &&
			func() error { return os.Chmod(current, 0o700) }() != nil {
			return fmt.Errorf("protect private rclone session directory")
		}
		if current == target {
			targetInfo, err := os.Lstat(current)
			if err != nil || targetInfo.Mode().Perm() != 0o700 {
				return fmt.Errorf("private rclone session root is not owner-only")
			}
		}
	}
	return nil
}
