package engines

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/local/replicaro/appdata"
)

var (
	rcloneCleanupAfterOpenForTest func(string)
	rcloneCleanupChildOpenForTest func(string)
	rcloneWipeAfterOpenForTest    func(string)
)

func recognizedRcloneSessionPath(path string) (string, string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	operationParent, err := appdata.RcloneSessionRoot()
	if err != nil {
		return "", "", err
	}
	operationParent, err = filepath.Abs(operationParent)
	if err != nil {
		return "", "", err
	}
	configParent, err := appdata.RcloneConfigSessionRoot()
	if err != nil {
		return "", "", err
	}
	configParent, err = filepath.Abs(configParent)
	if err != nil {
		return "", "", err
	}
	absolute = filepath.Clean(absolute)
	operationParent = filepath.Clean(operationParent)
	configParent = filepath.Clean(configParent)
	name := filepath.Base(absolute)
	parent := filepath.Dir(absolute)
	operationSession := parent == operationParent &&
		(validRcloneAuthorizationDirectoryName(name) || validRcloneOperationDirectoryName(name))
	configSession := parent == configParent && validRcloneConfigDirectoryName(name)
	if !operationSession && !configSession {
		return "", "", fmt.Errorf("rclone cleanup target is not a recognized private session")
	}
	return parent, name, nil
}

func recognizedRcloneConfigSessionPath(path string) error {
	parent, name, err := recognizedRcloneSessionPath(path)
	if err != nil {
		return err
	}
	configParent, err := appdata.RcloneConfigSessionRoot()
	if err != nil {
		return err
	}
	configParent, err = filepath.Abs(configParent)
	if err != nil {
		return err
	}
	if filepath.Clean(parent) != filepath.Clean(configParent) || !validRcloneConfigDirectoryName(name) {
		return fmt.Errorf("rclone config candidate is outside the private config-session root")
	}
	return nil
}

func recognizedRcloneConfigPath(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	absolute = filepath.Clean(absolute)
	if filepath.Base(absolute) == "rclone.conf" {
		if _, _, err := recognizedRcloneSessionPath(filepath.Dir(absolute)); err == nil {
			return nil
		}
	}
	profileRoot, err := appdata.RcloneProfileSessionRoot()
	if err != nil {
		return err
	}
	profileRoot, err = filepath.Abs(profileRoot)
	if err != nil {
		return err
	}
	name := filepath.Base(absolute)
	if filepath.Dir(absolute) == filepath.Clean(profileRoot) &&
		strings.HasPrefix(name, "rclone-") &&
		strings.HasSuffix(name, ".conf") &&
		len(name) > len("rclone-.conf") {
		return nil
	}
	vaultRoot, err := appdata.RcloneVaultConfigRoot()
	if err != nil {
		return err
	}
	vaultRoot, err = filepath.Abs(vaultRoot)
	if err != nil {
		return err
	}
	vaultDirectory := filepath.Dir(absolute)
	if filepath.Base(absolute) == "rclone.conf" &&
		filepath.Dir(vaultDirectory) == filepath.Clean(vaultRoot) &&
		validRepositoryUUID(filepath.Base(vaultDirectory)) {
		return nil
	}
	return fmt.Errorf("private rclone config is outside a recognized session")
}

// removeRcloneSessionTree removes only an immediate recognized config,
// authorization, or operation session. Platform implementations bind traversal and deletion
// to opened directory identities and never follow replacement paths.
func removeRcloneSessionTree(root string) error {
	if _, _, err := recognizedRcloneSessionPath(root); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return removeRcloneSessionTreeSecure(root)
}

func wipeRcloneConfig(filename string) error {
	if err := recognizedRcloneConfigPath(filename); err != nil {
		return err
	}
	return wipeRcloneConfigSecure(filename)
}
