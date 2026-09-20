//go:build windows

package appdata

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// File returns a durable per-user path. Existing files outside this directory
// are never inspected or migrated.
func File(name string) (string, error) {
	root, err := rootDirectory()
	if err != nil {
		return "", err
	}
	directory := root
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create application data directory: %w", err)
	}
	if err := SecurePath(directory, true); err != nil {
		return "", err
	}

	cleanName, err := safeRelativeName(name)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(directory, cleanName)
	if err := os.MkdirAll(filepath.Dir(destination), 0o700); err != nil {
		return "", fmt.Errorf("create application data directory: %w", err)
	}
	if err := SecurePath(filepath.Dir(destination), true); err != nil {
		return "", err
	}
	return destination, nil
}

func Directory(name string) (string, error) {
	root, err := rootDirectory()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(root, 0o700); err != nil {
		return "", fmt.Errorf("create application data root: %w", err)
	}
	if err := SecurePath(root, true); err != nil {
		return "", err
	}
	cleanName, err := safeRelativeName(name)
	if err != nil {
		return "", err
	}
	directory := filepath.Join(root, cleanName)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create application data directory: %w", err)
	}
	if err := SecurePath(directory, true); err != nil {
		return "", err
	}
	return directory, nil
}

func safeRelativeName(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	// filepath.IsAbs intentionally reports rooted drive-relative names such as
	// `\outside` as non-absolute on Windows. Joining one of those names still
	// discards the application-data tail, so reject every rooted spelling.
	if filepath.IsAbs(name) || filepath.IsLocal(name) == false {
		return "", fmt.Errorf("application data path must be relative")
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("application data path escapes the application root")
	}
	return clean, nil
}
