//go:build !linux && !windows

package engines

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

func checkRcloneRemoteNamespaceAliases(configFile, realFile string, roots []string) error {
	managed := make([]os.FileInfo, 0, len(roots))
	for _, root := range roots {
		info, err := os.Stat(root)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		managed = append(managed, info)
	}
	for _, route := range []string{configFile, realFile} {
		for current := route; ; current = filepath.Dir(current) {
			info, err := os.Stat(current)
			if err != nil {
				return err
			}
			for _, root := range managed {
				if os.SameFile(info, root) {
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
