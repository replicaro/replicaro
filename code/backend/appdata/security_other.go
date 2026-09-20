//go:build !windows

package appdata

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// secureEnsureDirectory creates and validates each component without ever
// following a symlink. Existing managed components must remain directories
// owned by the current user; this prevents stale links or replaced ancestors
// from redirecting state, credentials, or embedded binaries elsewhere.
func secureEnsureDirectory(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	managedRoot, err := managedRootForPath(abs)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	current := volume + string(filepath.Separator)
	rel := abs
	if volume != "" {
		rel = abs[len(volume):]
	}
	for _, part := range splitPathComponents(rel) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		created := false
		if os.IsNotExist(statErr) {
			if err := os.Mkdir(current, 0o700); err != nil {
				return fmt.Errorf("create managed directory %s: %w", current, err)
			}
			created = true
			info, statErr = os.Lstat(current)
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("managed directory ancestor is a symlink: %s", current)
		}
		if !info.IsDir() {
			return fmt.Errorf("managed directory ancestor is not a directory: %s", current)
		}
		// System-owned ancestors (for example /home or /tmp) are expected;
		// managed components may be owned by the user or root, but never an
		// unrelated account.
		if uid := fileOwner(info); uid >= 0 && uid != os.Geteuid() && uid != 0 {
			return fmt.Errorf("managed directory %s has unexpected owner", current)
		}
		// Enforce the managed-directory mode on every reconciliation, including
		// directories that predate this process. System-owned ancestors are not
		// managed state and are left unchanged.
		owner := fileOwner(info)
		if pathWithin(managedRoot, current) && (created || owner == os.Geteuid()) && current != string(filepath.Separator) {
			if err := os.Chmod(current, 0o700); err != nil {
				return err
			}
		}
	}
	return nil
}

func managedRootForPath(path string) (string, error) {
	stateRoot, err := rootDirectory()
	if err != nil {
		return "", err
	}
	stateRoot, err = filepath.Abs(stateRoot)
	if err != nil {
		return "", err
	}
	if pathWithin(stateRoot, path) {
		return stateRoot, nil
	}
	// macOS cache data intentionally lives outside Application Support.
	if home, homeErr := os.UserHomeDir(); homeErr == nil {
		cacheRoot, cacheErr := filepath.Abs(filepath.Join(home, "Library", "Caches", "Replicaro"))
		if cacheErr == nil && pathWithin(cacheRoot, path) {
			return cacheRoot, nil
		}
	}
	// Isolated callers still own the exact directory they asked us to secure.
	return path, nil
}

func pathWithin(root, target string) bool {
	rel, err := filepath.Rel(root, target)
	return err == nil && rel != ".." && !filepath.IsAbs(rel) && (len(rel) < 3 || rel[:3] != ".."+string(filepath.Separator))
}

func validateManagedAncestors(path string) error {
	root, err := rootDirectory()
	if err != nil {
		return err
	}
	root, _ = filepath.Abs(root)
	target, _ := filepath.Abs(filepath.Dir(path))
	rel, err := filepath.Rel(root, target)
	if err != nil || rel == ".." || (len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator)) {
		return nil // AtomicReplace also serves isolated temporary test files.
	}
	current := root
	for _, part := range splitPathComponents(rel) {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("managed path ancestor is unsafe: %s", current)
		}
		if uid := fileOwner(info); uid >= 0 && uid != os.Geteuid() && uid != 0 {
			return fmt.Errorf("managed path ancestor has unexpected owner: %s", current)
		}
	}
	return nil
}

func splitPathComponents(path string) []string {
	parts := make([]string, 0)
	clean := filepath.Clean(path)
	for clean != "." && clean != string(filepath.Separator) {
		base := filepath.Base(clean)
		if base == string(filepath.Separator) || base == "." || base == "" {
			break
		}
		parts = append([]string{base}, parts...)
		parent := filepath.Dir(clean)
		if parent == clean {
			break
		}
		clean = parent
	}
	return parts
}

func fileOwner(info os.FileInfo) int {
	// Sys() is platform-specific; Unix stat implementations expose Uid.
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(stat.Uid)
	}
	return -1
}
