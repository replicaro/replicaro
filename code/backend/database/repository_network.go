package database

import "strings"

func isUNCFilesystemLocation(location string) bool {
	normalized := strings.TrimSpace(strings.ReplaceAll(location, "/", "\\"))
	return strings.HasPrefix(normalized, "\\\\")
}
