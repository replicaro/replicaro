package engines

import (
	"fmt"
	"runtime"
	"strings"
	"unicode"
)

func validateRestoreOptions(options RestoreOptions) (string, error) {
	if options.Destination == "" && !options.OriginalLocation {
		return "", fmt.Errorf("restore destination is required")
	}
	if strings.ContainsRune(options.Destination, '\x00') {
		return "", fmt.Errorf("restore destination contains an invalid character")
	}
	// Restore destinations may contain valid POSIX control characters. Reject
	// NUL at the process boundary and traversal before any local path cleanup.
	if !options.OriginalLocation {
		grammar := options.Destination
		if runtime.GOOS == "windows" {
			grammar = strings.ReplaceAll(grammar, `\`, "/")
		}
		for _, component := range strings.Split(grammar, "/") {
			if component == ".." {
				return "", fmt.Errorf("path cannot contain '..' components; enter the direct intended route")
			}
		}
	}

	return normalizeSnapshotRelativePath(options.Selection)
}

func isDriveAbsolutePath(value string) bool {
	return len(value) >= 3 && isASCIILetter(value[0]) && value[1] == ':' && (value[2] == '/' || value[2] == '\\')
}

func isDriveRelativePath(value string) bool {
	return len(value) >= 2 && isASCIILetter(value[0]) && value[1] == ':' && !isDriveAbsolutePath(value)
}

func isASCIILetter(value byte) bool {
	return (value >= 'A' && value <= 'Z') || (value >= 'a' && value <= 'z')
}

// Archive descendants always use slash hierarchy. A Windows viewer must not
// reinterpret literal backslashes, drive-shaped names, or case from Unix data.
func normalizeSnapshotRelativePath(value string) (string, error) {
	if value == "" || value == "." {
		return "", nil
	}
	if strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("snapshot path contains an invalid character")
	}
	for _, component := range strings.Split(value, "/") {
		if component == "" || component == "." || component == ".." {
			return "", fmt.Errorf("snapshot path must use exact relative native path components")
		}
	}
	return value, nil
}

// validateOriginalLocationSourceForOS verifies that an engine-recorded source
// can safely be used as a native destination. Snapshot metadata may contain a
// path from another operating system; treating that spelling as a local path
// could redirect an original-location restore before the engine is invoked.
func validateOriginalLocationSourceForOS(source, operatingSystem string) error {
	if source == "" {
		return fmt.Errorf("snapshot has no recorded source path")
	}
	if strings.ContainsRune(source, '\x00') || containsControl(source) {
		return fmt.Errorf("snapshot source path contains an invalid character")
	}
	if operatingSystem == "windows" {
		normalized := strings.ReplaceAll(source, `\`, "/")
		if strings.HasPrefix(normalized, "//") {
			parts := make([]string, 0, 2)
			for _, part := range strings.Split(strings.TrimLeft(normalized, "/"), "/") {
				if part != "" {
					parts = append(parts, part)
				}
			}
			if len(parts) >= 2 {
				return nil
			}
		}
		if len(normalized) >= 3 && ((normalized[0] >= 'A' && normalized[0] <= 'Z') || (normalized[0] >= 'a' && normalized[0] <= 'z')) && normalized[1] == ':' && normalized[2] == '/' {
			return nil
		}
		return fmt.Errorf("original-location restore requires a native Windows absolute source path")
	}
	// filepath.IsAbs on Unix intentionally accepts //server as a POSIX path;
	// reject that spelling and all Windows drive/UNC forms explicitly.
	if strings.HasPrefix(source, "//") || resticWindowsStylePath(source) || !strings.HasPrefix(source, "/") {
		return fmt.Errorf("original-location restore requires a native absolute source path")
	}
	return nil
}

func validateOriginalLocationSource(source string) error {
	return validateOriginalLocationSourceForOS(source, runtime.GOOS)
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
