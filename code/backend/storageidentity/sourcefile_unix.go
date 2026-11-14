//go:build !windows

package storageidentity

import "io/fs"

// sourceFileCandidate reports whether an Lstat result may be a single-file
// source. Only a regular file qualifies: symlinks, devices, pipes, sockets and
// anything Go reports as irregular are refused without being opened.
func sourceFileCandidate(mode fs.FileMode) bool {
	return mode.IsRegular()
}
