//go:build windows && amd64

package engines

import (
	"bytes"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/local/replicaro/appdata"
)

//go:embed assets/restic.exe assets/kopia.exe
var embeddedBinaries embed.FS

func extractEmbeddedBinary(id, name string) (string, error) {
	data, err := embeddedBinaries.ReadFile("assets/" + id + ".exe")
	if err != nil {
		return "", err
	}
	expected, err := expectedEmbeddedHash(id, "windows_amd64")
	if err != nil {
		return "", err
	}
	actual := sha256.Sum256(data)
	if !strings.EqualFold(expected, hex.EncodeToString(actual[:])) {
		return "", fmt.Errorf("embedded %s hash mismatch", id)
	}
	directory, err := appdata.Directory(filepath.Join("engines", id))
	if err != nil {
		return "", err
	}
	path := filepath.Join(directory, name)
	if info, statErr := os.Lstat(path); statErr == nil && (info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular()) {
		return "", fmt.Errorf("refusing unsafe existing engine path")
	}
	if existing, err := os.ReadFile(path); err == nil && bytes.Equal(existing, data) {
		installed := sha256.Sum256(existing)
		if !strings.EqualFold(expected, hex.EncodeToString(installed[:])) {
			return "", fmt.Errorf("installed %s hash mismatch", id)
		}
		return path, nil
	}
	temporary, err := os.CreateTemp(directory, name+".updated-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o700); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(data); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	if err := appdata.AtomicReplace(temporaryPath, path); err != nil {
		return "", fmt.Errorf("install embedded %s: %w", id, err)
	}
	installed, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	installedHash := sha256.Sum256(installed)
	if !strings.EqualFold(expected, hex.EncodeToString(installedHash[:])) {
		return "", fmt.Errorf("installed %s hash mismatch", id)
	}
	return path, nil
}
