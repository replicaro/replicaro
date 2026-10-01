package vaultidentity

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// rcloneRemoteConnector is the "Any Rclone Remote" storage type. The engines
// package owns the connector; this package only needs its ID, because engines
// imports vaultidentity and not the other way round.
const rcloneRemoteConnector = "rclone_remote"

// The option keys of the Any Rclone Remote connector that name the storage.
const (
	rcloneRemoteNameOption = "remote"
	rcloneRemotePathOption = "path"
)

// Errors from NormalizeRcloneRemotePath. The messages are shown to the user.
var (
	ErrRcloneRemotePathRequired = errors.New("Enter the path in remote.")
	ErrRcloneRemotePathInvalid  = errors.New("The path in remote can't contain control characters, empty folder names, or '.' or '..' folders.")
	// ErrRcloneRemotePathIncludesRemote is wrapped with the remote name; see
	// NormalizeRcloneRemotePath.
	ErrRcloneRemotePathIncludesRemote = errors.New("path in remote starts with the remote name")
)

// RcloneRemotePathIncludesRemoteMessage is the message for a path in remote
// that starts with "<remote name>:".
func RcloneRemotePathIncludesRemoteMessage(remote string) string {
	return fmt.Sprintf("Leave the remote name out of the path in remote. Enter only what comes after '%s:'.", remote)
}

type rcloneRemotePathIncludesRemoteError struct{ remote string }

func (e *rcloneRemotePathIncludesRemoteError) Error() string {
	return RcloneRemotePathIncludesRemoteMessage(e.remote)
}
func (e *rcloneRemotePathIncludesRemoteError) Unwrap() error {
	return ErrRcloneRemotePathIncludesRemote
}

// NormalizeRcloneRemotePath applies the rules for the bucket, folder, or path
// in the user's remote under which Replicaro keeps Replicaro/<vault name>.
// Only the edges are trimmed and one trailing "/" is dropped. A leading "/" is
// kept because some backends (local, SFTP) read "/path" as absolute and "path"
// as relative to the home folder. Case, inner spaces, and non-ASCII characters
// are kept, and nothing is Unicode-normalized, because the path names storage
// the user already has.
func NormalizeRcloneRemotePath(remote, value string) (string, error) {
	path := strings.TrimSpace(value)
	if path == "" {
		return "", ErrRcloneRemotePathRequired
	}
	if strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return "", ErrRcloneRemotePathInvalid
	}
	if remote != "" && len(path) > len(remote) &&
		strings.EqualFold(path[:len(remote)+1], remote+":") {
		return "", &rcloneRemotePathIncludesRemoteError{remote: remote}
	}
	if path == "/" {
		return path, nil
	}
	path = strings.TrimSuffix(path, "/")
	for index, segment := range strings.Split(path, "/") {
		if segment == "" && index == 0 {
			// The leading "/" of an absolute path.
			continue
		}
		if segment == "" {
			return "", ErrRcloneRemotePathInvalid
		}
		// rclone's local backend on Windows also splits on "\", so dot
		// segments are refused there too.
		for _, part := range strings.Split(segment, `\`) {
			if part == "." || part == ".." {
				return "", ErrRcloneRemotePathInvalid
			}
		}
	}
	return path, nil
}

// RcloneSidecarRemoteName is the name of the rclone crypt remote that carries
// the vault sidecar; engines.RcloneSidecarRemoteName explains how it is used.
// It lives here so the remote-name check below can refuse it.
const RcloneSidecarRemoteName = "replicaro_sidecar"

// IsRcloneSidecarRemoteName reports whether rclone would treat a remote name
// as the sidecar's. rclone matches RCLONE_CONFIG_<NAME>_* variables against
// strings.ToUpper of the name, so this compares the same way. EqualFold would
// miss names such as "replıcaro_sidecar" (dotless ı), which rclone upper-cases
// to the same variable prefix.
func IsRcloneSidecarRemoteName(name string) bool {
	return strings.ToUpper(name) == strings.ToUpper(RcloneSidecarRemoteName)
}

// Errors from ValidateRcloneRemoteName. The messages are shown to the user.
var (
	ErrRcloneRemoteNameRequired = errors.New("Enter the remote name.")
	ErrRcloneRemoteNameInvalid  = errors.New("The remote name can't contain ':' or control characters.")
	// rclone reads "a/b:path" or "a\b:path" as a local path, not a remote.
	ErrRcloneRemoteNameSeparator = errors.New("The remote name can't contain '/' or '\\', because rclone would read it as a folder on this computer.")
	// rclone on Windows reads "c:path" as drive C:, not a remote named c.
	ErrRcloneRemoteNameDriveLetter = errors.New("The remote name can't be a single letter, because rclone on Windows reads it as a drive letter.")
	ErrRcloneRemoteNameReserved    = errors.New("Replicaro reserves the remote name '" + RcloneSidecarRemoteName + "'. Use a remote with another name.")
)

// ValidateRcloneRemoteName checks only what would make the rclone root
// "<remote>:<path>" point somewhere other than the user's remote; whether the
// remote exists is up to the preflight. The rules are the same on every
// platform, so a vault set up on one computer works on another.
func ValidateRcloneRemoteName(remote string) error {
	switch {
	case remote == "":
		return ErrRcloneRemoteNameRequired
	case strings.IndexFunc(remote, unicode.IsControl) >= 0 || strings.Contains(remote, ":"):
		return ErrRcloneRemoteNameInvalid
	case strings.ContainsAny(remote, `/\`):
		return ErrRcloneRemoteNameSeparator
	case len(remote) == 1 && ('a' <= remote[0] && remote[0] <= 'z' || 'A' <= remote[0] && remote[0] <= 'Z'):
		return ErrRcloneRemoteNameDriveLetter
	case IsRcloneSidecarRemoteName(remote):
		return ErrRcloneRemoteNameReserved
	}
	return nil
}

// resolveRcloneRemoteAddress builds the address of an Any Rclone Remote vault
// from the remote name (kept in Host), the path in remote (kept in Prefix),
// and the stored Replicaro/<vault name> location. This is a local key, not a
// physical identity: the same remote name can point at different storage on
// another computer or after the user edits their rclone.conf, and Replicaro
// can't learn the remote's account without reading that file. The native
// Restic repository ID and the vault UUID decide which vault it is. The
// config file path is deliberately not part of the key.
func resolveRcloneRemoteAddress(location string, options map[string]string) (EffectiveAddress, error) {
	root, err := rcloneVaultRoot(location)
	if err != nil {
		return EffectiveAddress{}, err
	}
	remote := strings.TrimSpace(options[rcloneRemoteNameOption])
	if err := ValidateRcloneRemoteName(remote); err != nil {
		return EffectiveAddress{}, err
	}
	path, err := NormalizeRcloneRemotePath(remote, options[rcloneRemotePathOption])
	if err != nil {
		return EffectiveAddress{}, err
	}
	return EffectiveAddress{
		Connector: rcloneRemoteConnector,
		Location:  root,
		Host:      remote,
		Prefix:    path,
	}, nil
}

// RcloneRemoteRoot composes the full rclone root
// "<remote>:<path>/Replicaro/<vault name>" of an Any Rclone Remote vault.
// Restic, the sidecar, and Vault Size all take the root from here.
func RcloneRemoteRoot(address EffectiveAddress) (string, error) {
	if address.Connector != rcloneRemoteConnector || address.Host == "" ||
		address.Prefix == "" || address.Location == "" {
		return "", fmt.Errorf("the rclone remote address is incomplete")
	}
	return address.Host + ":" + strings.TrimSuffix(address.Prefix, "/") + "/" + address.Location, nil
}
