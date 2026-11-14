package storageidentity

import "os"

// SourcePathKind tells the job-source picker and the new-job form whether a
// path is a folder (PathKindFolder), a file that can be a job source
// (PathKindFile), or neither (""), using the same rules as a source_any
// probe: os.Stat first so a link to a folder counts as the folder, then Lstat
// so a link to a file, a device, a pipe or a socket is never a file. On
// Windows a reparse point that isn't a link (a OneDrive placeholder, a
// deduplicated file) is settled by its reparse tag, exactly as the probe does.
//
// It is only a hint for the UI and runs in the server process, not in the
// killable helper, so callers must bound how long they wait for it. Whether a
// path can actually be saved is decided by the binding probe, which also
// checks that the folder can be listed and that a file isn't mounted on its
// own. Nothing here records facts or reads file contents.
func SourcePathKind(path string) string {
	info, err := os.Stat(path)
	if err != nil {
		return ""
	}
	if info.IsDir() {
		return PathKindFolder
	}
	linked, err := os.Lstat(path)
	if err != nil || !sourceFileCandidate(linked.Mode()) {
		return ""
	}
	if linked.Mode().IsRegular() {
		return PathKindFile
	}
	// Only Windows lets an irregular entry through sourceFileCandidate; its
	// reparse tag decides, on a handle opened for attributes only.
	observed, err := observeSourceFile(path)
	if err != nil || !observed.regular {
		return ""
	}
	return PathKindFile
}
