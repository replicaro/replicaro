//go:build !windows

package engines

import (
	"fmt"
	"os"
	"path/filepath"
)

// destinationMountDetector is injectable so unprivileged tests can model a
// mount without creating one. Production checks inspect the complete tree.
var destinationMountDetector = detectDestinationMounts

func validateDestinationMounts(path string) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return destinationMountDetector(path)
}

func detectDestinationMounts(path string) error {
	root, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	root = filepath.Clean(root)
	rootDev, err := destinationDevice(root)
	if err != nil {
		return err
	}
	if err := destinationMountSpecific(root); err != nil {
		return err
	}
	if parent := filepath.Dir(root); parent != root {
		parentDev, parentErr := destinationDevice(parent)
		if parentErr != nil {
			return parentErr
		}
		if parentDev != rootDev {
			return fmt.Errorf("restore destination is a filesystem mountpoint: %q", root)
		}
	}
	return filepath.WalkDir(root, func(current string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if current != root {
			if err := destinationMountPoint(current, rootDev); err != nil {
				return err
			}
		}
		if entry.IsDir() && entry.Type()&os.ModeSymlink != 0 {
			return filepath.SkipDir
		}
		return nil
	})
}

func destinationMountPoint(path string, rootDevice uint64) error {
	dev, err := destinationDevice(path)
	if err != nil {
		return err
	}
	if dev != rootDevice {
		return fmt.Errorf("restore destination crosses a filesystem boundary at %q", path)
	}
	return destinationMountSpecific(path)
}
