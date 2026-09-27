//go:build darwin

package storageidentity

import (
	"os"

	"golang.org/x/sys/unix"
)

// One fstatfs call on the opened folder supplies both facts. For firmlinked
// locations such as /Users this reports the data volume's mount point
// (/System/Volumes/Data) consistently on every run, which is all the
// comparison needs.
func observeFacts(file *os.File, _ string) (Facts, []FactFailure) {
	var stat unix.Statfs_t
	if err := unix.Fstatfs(int(file.Fd()), &stat); err != nil {
		code, _ := osErrorCode(err)
		return Facts{}, []FactFailure{{Step: StepMountPoint, Code: code}, {Step: StepFilesystemType, Code: code}}
	}
	facts := Facts{
		MountPoint: unix.ByteSliceToString(stat.Mntonname[:]),
		Filesystem: NormalizeFilesystem(unix.ByteSliceToString(stat.Fstypename[:])),
	}
	failures := []FactFailure{}
	if facts.MountPoint == "" {
		failures = append(failures, FactFailure{Step: StepMountPoint})
	}
	if facts.Filesystem == "" {
		failures = append(failures, FactFailure{Step: StepFilesystemType})
	}
	return facts, failures
}
