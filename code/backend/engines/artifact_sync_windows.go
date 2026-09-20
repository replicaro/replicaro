//go:build windows

package engines

// Artifact renames stay on one filesystem. Windows MoveFileEx durability is
// handled by the file activation path; directory handles cannot be fsynced.
func syncDirectory(string) error { return nil }
