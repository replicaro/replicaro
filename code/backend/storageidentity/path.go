package storageidentity

import (
	"fmt"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

// NormalizeConfiguredPath performs lexical cleanup only. Configured paths are
// operational names, not comparison keys: folding case here can change the
// object opened on a case-sensitive filesystem (including Windows directories).
// Binding retains this spelling; recorded mount facts never become an
// operational pathname.
func NormalizeConfiguredPath(value string) (string, error) {
	if err := ValidateConfiguredPath(value); err != nil {
		return "", err
	}
	var absolute string
	var err error
	if runtime.GOOS == "windows" {
		absolute, err = absoluteWindowsConfiguredPath(value)
	} else {
		absolute, err = filepath.Abs(value)
	}
	if err != nil {
		return "", fmt.Errorf("make storage path absolute: %w", err)
	}
	if runtime.GOOS == "windows" {
		absolute = cleanWindowsConfiguredPath(absolute)
		if absolute == "" {
			return "", fmt.Errorf("storage path is not a canonical absolute path")
		}
	} else {
		absolute = filepath.ToSlash(filepath.Clean(absolute))
	}
	if runtime.GOOS != "windows" && !canonicalConfiguredPath(absolute) {
		return "", fmt.Errorf("storage path is not a canonical absolute path")
	}
	return absolute, nil
}

// GetFullPathName (used by filepath.Abs on Windows) can strip trailing
// whitespace from the final name. Resolve only the drive/current-directory
// prefix through the OS; join the admitted user components lexically.
func absoluteWindowsConfiguredPath(value string) (string, error) {
	if filepath.IsAbs(value) {
		return value, nil
	}
	volume := filepath.VolumeName(value)
	tail := value[len(volume):]
	base, err := filepath.Abs(volume + ".")
	if err != nil {
		return "", err
	}
	if volume == "" && strings.HasPrefix(strings.ReplaceAll(tail, `\`, "/"), "/") {
		return filepath.VolumeName(base) + tail, nil
	}
	return filepath.Join(base, tail), nil
}

// ValidateConfiguredPath runs before lexical cleanup. Collapsing link/../dir
// can choose a different directory from filesystem traversal, so require the
// direct intended route instead. POSIX backslashes are filename characters.
// The NUL/CR/LF exclusion here applies to configured paths only, not to native
// archive/restore selections, where LF, CR, and tab can be valid names.
// Binding and run admission apply the stricter ValidateBindingPath.
func ValidateConfiguredPath(value string) error {
	if value == "" {
		return fmt.Errorf("storage path is required")
	}
	if strings.ContainsAny(value, "\x00\r\n") {
		return fmt.Errorf("storage path contains an invalid character")
	}
	grammar := value
	if runtime.GOOS == "windows" {
		grammar = strings.ReplaceAll(grammar, `\`, "/")
	}
	for _, component := range strings.Split(grammar, "/") {
		if component == ".." {
			return fmt.Errorf("path cannot contain '..' components; enter the direct intended route")
		}
	}
	return nil
}

func canonicalConfiguredPath(value string) bool {
	if looksLikeCanonicalWindowsPath(value) {
		return cleanWindowsConfiguredPath(value) == value
	}
	return strings.HasPrefix(value, "/") && path.Clean(value) == value
}

func looksLikeCanonicalWindowsPath(value string) bool {
	return len(value) >= 3 && value[1] == ':' && value[2] == '\\' || isWindowsUNC(value)
}

func cleanWindowsConfiguredPath(value string) string {
	// Windows can enable case sensitivity per directory. Keep entered spelling
	// after lexical cleanup.
	value = strings.ReplaceAll(value, "/", `\`)
	prefix, remainder, protected := "", value, 0
	switch {
	case strings.HasPrefix(strings.ToLower(value), `\\?\unc\`):
		prefix, remainder, protected = value[:len(`\\?\unc\`)], value[len(`\\?\unc\`):], 2
	case strings.HasPrefix(value, `\\`):
		prefix, remainder, protected = `\\`, value[2:], 2
	case len(value) >= 3 && value[1] == ':' && value[2] == '\\':
		prefix, remainder, protected = value[:3], value[3:], 0
	default:
		return ""
	}
	parts := strings.Split(remainder, `\`)
	cleaned := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			if len(cleaned) > protected {
				cleaned = cleaned[:len(cleaned)-1]
			}
		default:
			cleaned = append(cleaned, part)
		}
	}
	if len(cleaned) < protected {
		return ""
	}
	joined := strings.Join(cleaned, `\`)
	if prefix == `\\` || strings.EqualFold(prefix, `\\?\unc\`) {
		if len(cleaned) < 2 {
			return ""
		}
		return prefix + joined
	}
	if joined == "" {
		return prefix
	}
	return prefix + joined
}

func isWindowsUNC(value string) bool {
	value = strings.ToLower(value)
	return strings.HasPrefix(value, `\\?\unc\`) ||
		(strings.HasPrefix(value, `\\`) && !strings.HasPrefix(value, `\\?\`))
}

// WindowsNetworkMountFromText derives a Windows network mount point from the
// configured path text alone: the share root of a UNC path. It returns false
// for anything that is not a UNC path. Mapped drive letters are recognized by
// the platform code, which needs the local drive-type check.
//
// Reading the path text instead of asking Windows (WNetGetUniversalName,
// WNetGetResourceInformation, GetVolumeInformation on the share) is deliberate:
// those calls contact the server and were the fragile part of vault creation
// on SMB. A text-derived share root also stays the same when a DFS namespace
// fails over to another server.
func WindowsNetworkMountFromText(value string) (string, bool) {
	value = strings.ReplaceAll(value, "/", `\`)
	lower := strings.ToLower(value)
	prefix := ""
	switch {
	case strings.HasPrefix(lower, `\\?\unc\`):
		prefix = value[:len(`\\?\UNC\`)]
	case strings.HasPrefix(value, `\\`) && !strings.HasPrefix(value, `\\?\`) && !strings.HasPrefix(value, `\\.\`):
		prefix = `\\`
	default:
		return "", false
	}
	parts := strings.SplitN(value[len(prefix):], `\`, 3)
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return "", false
	}
	return prefix + parts[0] + `\` + parts[1], true
}

// WindowsDriveFromText returns the drive letter root ("D:\") named by a drive
// path or its \\?\D:\ extended spelling.
func WindowsDriveFromText(value string) (string, bool) {
	value = strings.ReplaceAll(value, "/", `\`)
	if strings.HasPrefix(value, `\\?\`) {
		value = value[len(`\\?\`):]
	}
	if len(value) >= 2 && value[1] == ':' && isASCIILetter(value[0]) {
		return strings.ToUpper(value[:1]) + `:\`, true
	}
	return "", false
}
