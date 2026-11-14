//go:build darwin

package storageidentity

import (
	"os"

	"golang.org/x/sys/unix"
)

// observeSourceFile reads a single-file source's type and facts by path,
// without opening the file. macOS has no handle that can't read contents, and
// opening the file to read it is exactly what must not happen here: a file
// the user can't read, or one another program holds, has to reach the engine,
// which reports it in its backup result. lstat checks the type without
// following a link, and statfs on the path supplies the same mount point and
// filesystem type that fstatfs gives a folder. The path was checked with
// Lstat just before; a link swapped in between is caught by this lstat.
//
// Single-file bind mounts are a Docker on Linux case, so mountRoot is never
// set here.
func observeSourceFile(path string) (sourceFileObservation, error) {
	var stat unix.Stat_t
	if err := unix.Lstat(path, &stat); err != nil {
		return sourceFileObservation{}, &os.PathError{Op: "lstat", Path: path, Err: err}
	}
	observed := sourceFileObservation{
		regular:   stat.Mode&unix.S_IFMT == unix.S_IFREG,
		directory: stat.Mode&unix.S_IFMT == unix.S_IFDIR,
	}
	if !observed.regular {
		return observed, nil
	}
	var filesystem unix.Statfs_t
	if err := unix.Statfs(path, &filesystem); err != nil {
		code, _ := osErrorCode(err)
		observed.failures = []FactFailure{{Step: StepMountPoint, Code: code}, {Step: StepFilesystemType, Code: code}}
		return observed, nil
	}
	observed.facts, observed.failures = factsFromStatfs(filesystem)
	return observed, nil
}
