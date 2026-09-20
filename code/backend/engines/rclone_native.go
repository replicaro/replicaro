package engines

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"sync"

	"github.com/local/replicaro/appdata"
	replicarorclone "github.com/local/replicaro/rclone"
)

// RcloneComponentID identifies the pinned native helper. Rclone is not a
// Replicaro backup engine.
const RcloneComponentID = "rclone"

const (
	rcloneMaxProviderValueBytes      = 1 << 20
	rcloneMaxProviderConfigLineBytes = maximumRcloneVaultConfigBytes
)

type rcloneRootEntry struct {
	ID string `json:"ID"`
}

func rcloneBinary(customPath string) (string, error) {
	if strings.TrimSpace(customPath) != "" {
		if !filepath.IsAbs(customPath) {
			return "", fmt.Errorf("rclone helper binary path must be absolute")
		}
		return customPath, nil
	}
	return replicarorclone.Materialize()
}

var (
	rcloneCleanupHooksMu sync.RWMutex
	rcloneSessionRemover = removeRcloneSessionTree
	rcloneConfigWiper    = wipeRcloneConfig
	rcloneCleanupOnce    sync.Once
	rcloneCleanupErr     error
)

func SetRcloneSessionRemoverForTests(next func(string) error) func() {
	rcloneCleanupHooksMu.Lock()
	previous := rcloneSessionRemover
	rcloneSessionRemover = next
	rcloneCleanupHooksMu.Unlock()
	return func() {
		rcloneCleanupHooksMu.Lock()
		rcloneSessionRemover = previous
		rcloneCleanupHooksMu.Unlock()
	}
}

func setRcloneConfigWiperForTests(next func(string) error) func() {
	rcloneCleanupHooksMu.Lock()
	previous := rcloneConfigWiper
	rcloneConfigWiper = next
	rcloneCleanupHooksMu.Unlock()
	return func() {
		rcloneCleanupHooksMu.Lock()
		rcloneConfigWiper = previous
		rcloneCleanupHooksMu.Unlock()
	}
}

func removeRcloneSession(path string) error {
	rcloneCleanupHooksMu.RLock()
	remove := rcloneSessionRemover
	rcloneCleanupHooksMu.RUnlock()
	return remove(path)
}

func wipeRcloneSessionConfig(path string) error {
	rcloneCleanupHooksMu.RLock()
	wipe := rcloneConfigWiper
	rcloneCleanupHooksMu.RUnlock()
	return wipe(path)
}

// CleanupRcloneSessions removes only recognized private sessions left by an
// earlier process. Authorization and operation sessions cannot be resumed.
func CleanupRcloneSessions() error {
	operationParent, err := appdata.RcloneSessionRoot()
	if err != nil {
		return err
	}
	if err := prepareRcloneSessionRoot(operationParent); err != nil {
		return err
	}
	configParent, err := appdata.RcloneConfigSessionRoot()
	if err != nil {
		return err
	}
	if err := prepareRcloneSessionRoot(configParent); err != nil {
		return err
	}
	return errors.Join(
		cleanupRcloneSessionsSecure(operationParent),
		cleanupRcloneSessionsSecure(configParent),
	)
}

func ensureRcloneSessionsCleaned() error {
	rcloneCleanupOnce.Do(func() { rcloneCleanupErr = CleanupRcloneSessions() })
	return rcloneCleanupErr
}

func validRcloneOperationDirectoryName(name string) bool {
	return validRcloneSessionDirectoryName(name, "op-")
}

func validRcloneAuthorizationDirectoryName(name string) bool {
	return validRcloneSessionDirectoryName(name, "auth-")
}

func validRcloneConfigDirectoryName(name string) bool {
	return validRcloneSessionDirectoryName(name, "config-")
}

func validRcloneSessionDirectoryName(name, prefix string) bool {
	if !strings.HasPrefix(name, prefix) || len(name) <= len(prefix) {
		return false
	}
	for _, character := range strings.TrimPrefix(name, prefix) {
		if (character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') {
			return false
		}
	}
	return true
}

func readRcloneProviderConfigReader(
	reader io.Reader,
	remoteName string,
	provider RcloneProviderDefinition,
) (map[string]string, bool, error) {
	values := map[string]string{}
	section := ""
	providerSections := 0
	sawType := false
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(
		make([]byte, 64*1024),
		rcloneMaxProviderConfigLineBytes+1,
	)
	for scanner.Scan() {
		if len(scanner.Bytes()) > rcloneMaxProviderConfigLineBytes {
			return nil, false, fmt.Errorf("rclone provider configuration line is oversized")
		}
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(line, "["), "]"))
			if section != remoteName {
				return nil, false, fmt.Errorf("rclone added an unexpected private configuration section")
			}
			providerSections++
			if providerSections > 1 {
				return nil, false, fmt.Errorf("rclone duplicated the provider configuration")
			}
			continue
		}
		if section != remoteName {
			return nil, false, fmt.Errorf("rclone added content outside the provider configuration")
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, false, fmt.Errorf("malformed rclone provider configuration")
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if key == "type" {
			if sawType || value != provider.NativeType {
				return nil, false, fmt.Errorf("rclone changed the provider type")
			}
			sawType = true
			continue
		}
		known := false
		for _, field := range provider.Fields {
			known = known || field == key
		}
		if !known {
			// Rclone owns native additions. Replicaro observes only the identity
			// and credential fields it already needs and ignores every other
			// field without admitting, repairing, or rewriting it.
			continue
		}
		if _, duplicate := values[key]; duplicate {
			return nil, false, fmt.Errorf("rclone duplicated a provider field")
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return nil, false, err
	}
	if providerSections != 1 || !sawType {
		return nil, false, fmt.Errorf("rclone provider configuration is incomplete")
	}
	return values, true, nil
}
