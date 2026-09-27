//go:build !windows

package storageidentity

import (
	"errors"
	"syscall"
)

const notDirectoryCode = int64(syscall.ENOTDIR)

func osErrorCode(err error) (int64, bool) {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return int64(errno), true
	}
	return 0, false
}

// classifyErrorCode keeps the missing and denied groups deliberately narrow.
// Stale NFS handles (ESTALE), dead FUSE/rclone/sshfs mounts (ENOTCONN), EIO,
// EHOSTDOWN and every other code are "failed", which pauses rather than
// erroring, because they describe storage that is there but unreachable.
func classifyErrorCode(code int64) string {
	switch syscall.Errno(code) {
	case syscall.ENOENT, syscall.ENOTDIR:
		return AccessMissing
	case syscall.EACCES, syscall.EPERM:
		return AccessDenied
	default:
		return AccessFailed
	}
}
