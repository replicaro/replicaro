// Package rclone materializes Replicaro's pinned rclone sidecar.
package rclone

import (
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/local/replicaro/appdata"
)

var materializeOnce sync.Once
var materializedPath string
var materializedErr error

const (
	Version = "1.75.1"
)

var (
	BinarySHA256 = embeddedBinarySHA256()
	binaryName   = embeddedBinaryName()
)

// Materialize verifies and installs the embedded rclone executable in
// Replicaro's secured per-user application-data directory.
func Materialize() (string, error) {
	materializeOnce.Do(func() {
		data, err := embeddedBinary()
		if err != nil {
			materializedErr = err
			return
		}
		directory, err := appdata.Directory(filepath.Join("components", "rclone"))
		if err != nil {
			materializedErr = fmt.Errorf("prepare rclone component directory: %w", err)
			return
		}
		materializedPath, materializedErr = materializeBinary(data, BinarySHA256, directory, binaryName)
	})
	return materializedPath, materializedErr
}

func materializeBinary(data []byte, expectedHash, directory, name string) (string, error) {
	if actual := sha256Bytes(data); !strings.EqualFold(actual, expectedHash) {
		return "", fmt.Errorf("embedded rclone checksum mismatch")
	}

	destination := filepath.Join(directory, name)
	if err := verifyFile(destination, expectedHash); err == nil {
		if err := os.Chmod(destination, 0o700); err != nil {
			return "", fmt.Errorf("secure installed rclone: %w", err)
		}
		return destination, nil
	}

	temporary, err := os.CreateTemp(directory, ".rclone.updated-*")
	if err != nil {
		return "", fmt.Errorf("create temporary rclone executable: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("secure temporary rclone executable: %w", err)
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("write temporary rclone executable: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return "", fmt.Errorf("flush temporary rclone executable: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", fmt.Errorf("close temporary rclone executable: %w", err)
	}
	if err := verifyFile(temporaryPath, expectedHash); err != nil {
		return "", fmt.Errorf("verify temporary rclone executable: %w", err)
	}
	if err := appdata.AtomicReplace(temporaryPath, destination); err != nil {
		return "", fmt.Errorf("install embedded rclone: %w", err)
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return "", fmt.Errorf("secure installed rclone: %w", err)
	}
	if err := verifyFile(destination, expectedHash); err != nil {
		return "", fmt.Errorf("verify installed rclone: %w", err)
	}
	return destination, nil
}

func verifyFile(path, expectedHash string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("rclone executable is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	actual := fmt.Sprintf("%x", hash.Sum(nil))
	if !strings.EqualFold(actual, expectedHash) {
		return fmt.Errorf("rclone executable checksum mismatch")
	}
	return nil
}

func sha256Bytes(data []byte) string {
	hash := sha256.Sum256(data)
	return fmt.Sprintf("%x", hash[:])
}
