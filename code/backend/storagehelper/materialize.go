// Package storagehelper securely materializes and launches Replicaro's
// target-native, embedded filesystem storage helper.
package storagehelper

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

const componentDirectory = "storage-helper"

var materializeMu sync.Mutex

// Materialize verifies and, when necessary, atomically recreates the embedded
// storage helper in Replicaro's private per-user application-data directory.
func Materialize() (string, error) {
	binary, expected, err := embeddedComponent()
	if err != nil {
		return "", componentError(err)
	}
	root, err := appdata.Directory("")
	if err != nil {
		return "", componentError(fmt.Errorf("resolve application-data root: %w", err))
	}
	return materializeBinary(binary, expected, root, executableName())
}

func materializeBinary(binary []byte, expected, root, name string) (string, error) {
	materializeMu.Lock()
	defer materializeMu.Unlock()

	if checksum(binary) != expected {
		return "", componentError(fmt.Errorf("embedded bytes do not match their build checksum"))
	}
	if err := validateManagedDirectory(root, false); err != nil {
		return "", componentError(fmt.Errorf("application-data root is unsafe: %w", err))
	}
	if err := rejectCaseAmbiguity(root, "components"); err != nil {
		return "", componentError(err)
	}
	components := filepath.Join(root, "components")
	if err := ensurePrivateDirectory(components); err != nil {
		return "", componentError(fmt.Errorf("prepare component root: %w", err))
	}
	if err := rejectCaseAmbiguity(components, componentDirectory); err != nil {
		return "", componentError(err)
	}
	directory := filepath.Join(components, componentDirectory)
	if err := ensurePrivateDirectory(directory); err != nil {
		return "", componentError(fmt.Errorf("prepare component directory: %w", err))
	}
	if err := rejectCaseAmbiguity(directory, name); err != nil {
		return "", componentError(err)
	}
	destination := filepath.Join(directory, name)
	if err := verifyExecutable(destination, expected); err == nil {
		if err := secureExecutable(destination); err != nil {
			return "", componentError(fmt.Errorf("secure installed executable: %w", err))
		}
		return destination, nil
	} else if !os.IsNotExist(err) {
		info, statErr := inspectRegularFile(destination)
		if statErr != nil && !os.IsNotExist(statErr) {
			return "", componentError(fmt.Errorf("refuse unsafe installed executable: %w", statErr))
		}
		if statErr == nil && info == nil {
			return "", componentError(fmt.Errorf("refuse unsafe installed executable"))
		}
	}

	temporary, err := os.CreateTemp(directory, "."+name+".updated-*")
	if err != nil {
		return "", componentError(fmt.Errorf("create temporary executable: %w", err))
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	fail := func(cause error) (string, error) {
		_ = temporary.Close()
		return "", componentError(cause)
	}
	if err := temporary.Chmod(0o700); err != nil {
		return fail(fmt.Errorf("secure temporary executable: %w", err))
	}
	if err := protectExecutable(temporaryPath); err != nil {
		return fail(fmt.Errorf("protect temporary executable: %w", err))
	}
	if _, err := temporary.Write(binary); err != nil {
		return fail(fmt.Errorf("write temporary executable: %w", err))
	}
	if err := temporary.Sync(); err != nil {
		return fail(fmt.Errorf("flush temporary executable: %w", err))
	}
	if err := temporary.Close(); err != nil {
		return "", componentError(fmt.Errorf("close temporary executable: %w", err))
	}
	if err := verifyExecutable(temporaryPath, expected); err != nil {
		return "", componentError(fmt.Errorf("verify temporary executable: %w", err))
	}
	if err := appdata.AtomicReplace(temporaryPath, destination); err != nil {
		return "", componentError(fmt.Errorf("atomically install executable: %w", err))
	}
	if err := secureExecutable(destination); err != nil {
		return "", componentError(fmt.Errorf("secure installed executable: %w", err))
	}
	if err := verifyExecutable(destination, expected); err != nil {
		return "", componentError(fmt.Errorf("verify installed executable: %w", err))
	}
	return destination, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.Mkdir(path, 0o700); err != nil && !os.IsExist(err) {
		return err
	}
	if err := validateManagedDirectory(path, true); err != nil {
		return err
	}
	if err := os.Chmod(path, 0o700); err != nil {
		return err
	}
	return appdata.SecurePath(path, true)
}

func secureExecutable(path string) error {
	return protectExecutable(path)
}

func verifyExecutable(path, expected string) error {
	if _, err := inspectRegularFile(path); err != nil {
		return err
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
	if fmt.Sprintf("%x", hash.Sum(nil)) != expected {
		return fmt.Errorf("executable checksum mismatch")
	}
	return nil
}

func checksum(value []byte) string {
	sum := sha256.Sum256(value)
	return fmt.Sprintf("%x", sum[:])
}

func rejectCaseAmbiguity(directory, exact string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	matches := 0
	exactMatches := 0
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), exact) {
			matches++
			if entry.Name() == exact {
				exactMatches++
			}
		}
	}
	if matches > 1 || matches == 1 && exactMatches != 1 {
		return fmt.Errorf("component path has ambiguous casing for %q", exact)
	}
	return nil
}

func componentError(err error) error {
	return fmt.Errorf("storage helper component unavailable: %w", err)
}
