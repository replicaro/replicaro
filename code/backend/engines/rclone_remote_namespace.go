package engines

import (
	"path/filepath"
	"strings"

	"github.com/local/replicaro/appdata"
)

func rcloneRemoteConfigManagedFolder() error {
	return &RcloneRemoteError{
		Code:    RcloneRemoteConfigManagedFolderCode,
		Message: "Choose an rclone config file outside Replicaro's managed folders.",
	}
}

func rcloneRemotePathWithin(path, root string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func rcloneRemoteManagedRoots() ([]string, error) {
	logs, err := appdata.OperationLogDirectoryPath()
	if err != nil {
		return nil, err
	}
	vaults, err := appdata.RcloneVaultConfigRoot()
	if err != nil {
		return nil, err
	}
	return []string{filepath.Dir(logs), filepath.Dir(filepath.Dir(vaults))}, nil
}

// Managed cleanup owns these folders. Check both the chosen route and the
// resolved file, because a link inside a managed folder can point outward.
func resolveRcloneRemoteConfigOutsideManagedFolders(configFile string) (string, error) {
	roots, err := rcloneRemoteManagedRoots()
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		if rcloneRemotePathWithin(configFile, root) {
			return "", rcloneRemoteConfigManagedFolder()
		}
	}
	realFile, err := filepath.EvalSymlinks(configFile)
	if err != nil {
		return "", err
	}
	for _, root := range roots {
		if rcloneRemotePathWithin(realFile, root) {
			return "", rcloneRemoteConfigManagedFolder()
		}
	}
	if err := rcloneRemoteNamespaceAliasCheck(configFile, realFile, roots); err != nil {
		return "", err
	}
	return realFile, nil
}

var rcloneRemoteNamespaceAliasCheck = checkRcloneRemoteNamespaceAliases
