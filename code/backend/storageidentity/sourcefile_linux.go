//go:build linux

package storageidentity

import (
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// observeSourceFile opens a single-file source for metadata only and reads
// its type, whether it is a mount root, and the same two facts a folder
// records.
//
// O_PATH gives a handle that can't be used to read the file, so the contents
// are never touched: a file the user can't read, or one another program holds
// open, still passes here and the engine reports the problem itself in its
// backup result. The probe only answers "is the storage there". O_PATH also
// never blocks, even on a FIFO that slipped in after the Lstat check, and
// O_NOFOLLOW makes a symlink that slipped in open as the link itself, which
// the type check below then refuses.
//
// The facts come from the unchanged observeFacts: its statx and
// /proc/self/fdinfo lookups work on an O_PATH handle, so a file records its
// mount exactly as its folder would.
func observeSourceFile(path string) (sourceFileObservation, error) {
	fd, err := unix.Open(path, unix.O_PATH|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if err != nil {
		return sourceFileObservation{}, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	mode, mountRoot, err := sourceFileTypeAndMountRoot(fd, path)
	if err != nil {
		return sourceFileObservation{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	observed := sourceFileObservation{
		regular:   mode&unix.S_IFMT == unix.S_IFREG,
		directory: mode&unix.S_IFMT == unix.S_IFDIR,
	}
	if !observed.regular {
		return observed, nil
	}
	observed.mountRoot = mountRoot
	observed.facts, observed.failures = observeFacts(file, path)
	return observed, nil
}

// sourceFileTypeAndMountRoot reads the opened file's type and, for a regular
// file, whether it is the root of its own mount, which is what a single-file
// bind mount (the Docker case) looks like. The type comes from statx, or from
// fstat if statx fails. path is the path the file was opened by.
func sourceFileTypeAndMountRoot(fd int, path string) (uint32, bool, error) {
	var stat unix.Statx_t
	statErr := unix.Statx(fd, "", unix.AT_EMPTY_PATH|unix.AT_SYMLINK_NOFOLLOW|unix.AT_NO_AUTOMOUNT, unix.STATX_TYPE, &stat)
	mode := uint32(stat.Mode)
	if statErr != nil || stat.Mask&unix.STATX_TYPE == 0 {
		var fallback unix.Stat_t
		if err := unix.Fstat(fd, &fallback); err != nil {
			return 0, false, err
		}
		mode = fallback.Mode
	}
	if mode&unix.S_IFMT != unix.S_IFREG {
		return mode, false, nil
	}
	mountRoot := sourceFileMountRoot(stat, statErr, func() (bool, error) {
		return sourceFileOnOwnMount(fd, path)
	})
	return mode, mountRoot, nil
}

// sourceFileMountRoot decides whether the file is a mount root from the statx
// result, or from compareWithFolder when the kernel gave no answer.
//
// Where the kernel reports STATX_ATTR_MOUNT_ROOT (Linux 5.8 and later) that
// answer is final and nothing else is looked at. Older kernels leave the
// attribute out of the attributes mask, but the folder facts still work there
// because they read the mount ID from /proc/self/fdinfo, so the same mount IDs
// decide it: a file whose mount differs from its folder's is a mount of its
// own. Don't drop this fallback because current kernels never need it; without
// it a single-file bind mount on an older kernel would be taken for an
// ordinary file and every backup would quietly store stale content.
//
// If the comparison can't be made, the file counts as a mount root. Its mount
// status is then unknown, and a file that can't be shown to be ordinary must
// be refused like a single-file mount, not accepted. Keep that answer true.
func sourceFileMountRoot(stat unix.Statx_t, statErr error, compareWithFolder func() (bool, error)) bool {
	if statErr == nil && stat.Attributes_mask&unix.STATX_ATTR_MOUNT_ROOT != 0 {
		return stat.Attributes&unix.STATX_ATTR_MOUNT_ROOT != 0
	}
	differs, err := compareWithFolder()
	return err != nil || differs
}

// sourceFileOnOwnMount reports whether the opened file is on a different
// mount than the folder that contains it. The folder is taken from the path
// the file was opened by, not from the recorded mount point: a bind-mounted
// file reached through a symlinked folder has to be compared with the folder
// it really sits in, and opening filepath.Dir(path) follows that link. The
// folder is opened with O_PATH, so nothing in it is read.
func sourceFileOnOwnMount(fd int, path string) (bool, error) {
	fileID, err := sourceFileMountID(fd)
	if err != nil {
		return false, err
	}
	folder, err := unix.Open(filepath.Dir(path), unix.O_PATH|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return false, err
	}
	defer unix.Close(folder)
	folderID, err := sourceFileMountID(folder)
	if err != nil {
		return false, err
	}
	return fileID != folderID, nil
}

// sourceFileMountID reads a mount ID the way observeFacts does: statx first,
// then /proc/self/fdinfo on kernels without STATX_MNT_ID.
func sourceFileMountID(fd int) (uint64, error) {
	if id, err := statxMountID(fd); err == nil {
		return id, nil
	}
	return fdinfoMountID(fd)
}
