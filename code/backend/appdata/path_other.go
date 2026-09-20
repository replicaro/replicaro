//go:build !windows

package appdata

import (
	"fmt"
	"path/filepath"
	"strings"
)

func File(name string) (string, error) {
	root, err := rootDirectory()
	if err != nil {
		return "", err
	}
	if err := ensureDirectory(root); err != nil {
		return "", err
	}
	cleanName, err := safeRelativeName(name)
	if err != nil {
		return "", err
	}
	destination := filepath.Join(root, cleanName)
	if err := ensureDirectory(filepath.Dir(destination)); err != nil {
		return "", err
	}
	return destination, nil
}

func Directory(name string) (string, error) {
	root, err := rootDirectory()
	if err != nil {
		return "", err
	}
	cleanName, err := safeRelativeName(name)
	if err != nil {
		return "", err
	}
	directory := filepath.Join(root, cleanName)
	if name == "" {
		directory = root
	}
	if err := ensureDirectory(directory); err != nil {
		return "", err
	}
	return directory, nil
}

func safeRelativeName(name string) (string, error) {
	if name == "" {
		return "", nil
	}
	if filepath.IsAbs(name) {
		return "", fmt.Errorf("application data path must be relative")
	}
	clean := filepath.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("application data path escapes the application root")
	}
	return clean, nil
}
