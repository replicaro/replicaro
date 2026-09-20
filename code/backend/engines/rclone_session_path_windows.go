//go:build windows

package engines

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/local/replicaro/appdata"
	"golang.org/x/sys/windows"
)

func prepareRcloneSessionRoot(target string) error {
	absolute, err := filepath.Abs(target)
	if err != nil {
		return err
	}
	// target is always <Replicaro app root>\rclone\sessions. Ancestors above
	// the Replicaro-owned root must be inspected for reparse points, but their
	// ACLs belong to Windows/the user profile and must never be rewritten.
	secureStart := filepath.Clean(filepath.Dir(filepath.Dir(absolute)))
	volume := filepath.VolumeName(absolute)
	if volume == "" {
		return fmt.Errorf("private rclone session root has no volume")
	}
	current := volume + string(filepath.Separator)
	relative := strings.TrimPrefix(absolute[len(volume):], string(filepath.Separator))
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		if err := rejectRcloneCaseAlias(current, component); err != nil {
			return err
		}
		current = filepath.Join(current, component)
		relativeToOwnedRoot, relativeErr := filepath.Rel(secureStart, current)
		owned := relativeErr == nil &&
			(relativeToOwnedRoot == "." ||
				relativeToOwnedRoot != ".." &&
					!strings.HasPrefix(relativeToOwnedRoot, ".."+string(filepath.Separator)))
		created := false
		info, statErr := os.Lstat(current)
		if os.IsNotExist(statErr) {
			if !owned {
				return fmt.Errorf("private rclone session ancestor above the Replicaro-owned root is missing: %s", current)
			}
			mkdirErr := os.Mkdir(current, 0o700)
			if mkdirErr != nil && !errors.Is(mkdirErr, os.ErrExist) {
				return fmt.Errorf("create private rclone session directory: %w", mkdirErr)
			}
			created = mkdirErr == nil
			info, statErr = os.Lstat(current)
		}
		if statErr != nil {
			return statErr
		}
		if !info.IsDir() || isReparsePoint(info) {
			return fmt.Errorf("private rclone session ancestor is reparse, linked, or special: %s", current)
		}
		if owned {
			if created {
				if err := appdata.SecurePath(current, true); err != nil {
					return err
				}
			} else {
				directory, openErr := openWindowsLocked(
					current, windows.GENERIC_READ|windows.READ_CONTROL, true,
				)
				if openErr != nil {
					return openErr
				}
				inspectErr := inspectWindowsRcloneObject(directory, true)
				if err := errors.Join(inspectErr, directory.Close()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
