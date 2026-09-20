package models

import "strings"

// ResticUNCArchivePath identifies Restic's literal virtual-volume component,
// e.g. `\\server\share/Photos/file.jpg`, in a source-relative archive path.
// Untagged snapshots are indexed beneath '/', so that component is now part
// of the relative path. Treating its backslashes as separators would silently
// address a different file. This is not a local filesystem destination.
func ResticUNCArchivePath(value string) bool {
	volume, remainder, _ := strings.Cut(value, "/")
	if !strings.HasPrefix(volume, `\\`) || strings.Contains(remainder, `\`) {
		return false
	}
	parts := strings.Split(volume[2:], `\`)
	if len(parts) != 2 {
		return false
	}
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.ContainsAny(part, ":?\x00") {
			return false
		}
	}
	return true
}
