//go:build !windows

package database

import (
	"strings"

	"github.com/local/replicaro/storageidentity"
)

// isNetworkFilesystemLocation picks the "Network" or "Local" label shown on a
// filesystem vault card. It only reads stored data: the location text and the
// filesystem type recorded in the vault's storage binding.
//
// This used to call statfs on the vault folder (and, on Linux, scan
// /proc/self/mountinfo) every time a vault row was loaded. ListRepositories
// scans rows on the single SQLite writer connection, and the scheduler lists
// vaults every minute, so one hung NFS or SMB share could park that connection
// inside a kernel call that never returns and stall every database write in
// the process. A cosmetic label is not worth that. Please keep this function
// free of filesystem calls on the vault path; availability is decided by the
// out-of-process storage helper, which can be timed out and killed.
//
// Rows still carrying the legacy storage_identity_v4 binding have no decoded
// facts, so they fall back to the path text until their first successful
// probe rewrites the binding.
func isNetworkFilesystemLocation(connector, location string, facts storageidentity.Facts) bool {
	if !strings.EqualFold(strings.TrimSpace(connector), "fs") {
		return false
	}
	return isUNCFilesystemLocation(location) || networkFilesystemType(facts.Filesystem)
}

// networkFilesystemType lists the recorded filesystem types treated as
// network storage. It keeps the set the old statfs/mountinfo check used:
// NFS, SMB/CIFS (smb3 and nfs4 are already folded to cifs and nfs by
// storageidentity.NormalizeFilesystem), WebDAV, SSHFS, and every FUSE mount.
// FUSE is included because the old Linux check matched the FUSE superblock
// magic, which covers all fuse.* subtypes, and most FUSE mounts users pick as
// vault targets (sshfs, rclone, davfs) are remote.
func networkFilesystemType(filesystem string) bool {
	filesystem = storageidentity.NormalizeFilesystem(filesystem)
	switch filesystem {
	case "nfs", "cifs", "smbfs", "webdav", "davfs", "sshfs", "fuse", "fuseblk":
		return true
	}
	return strings.HasPrefix(filesystem, "fuse.")
}
