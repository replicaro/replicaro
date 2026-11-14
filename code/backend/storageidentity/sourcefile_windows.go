//go:build windows

package storageidentity

import (
	"io/fs"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

// windowsReparseTagNameSurrogate marks reparse points that stand for another
// name: symbolic links, junctions and WSL links. Placeholders such as
// OneDrive's cloud files and deduplicated files don't carry it; they are the
// file itself as far as the user is concerned.
const windowsReparseTagNameSurrogate = 0x20000000

// sourceFileCandidate reports whether an Lstat result may be a single-file
// source. Go reports reparse points other than links as ModeIrregular, so an
// irregular entry is allowed through here and settled by its reparse tag on
// the opened handle. Links, folders, sockets, pipes and devices are refused.
func sourceFileCandidate(mode fs.FileMode) bool {
	return mode&(fs.ModeSymlink|fs.ModeDir|fs.ModeSocket|fs.ModeNamedPipe|fs.ModeDevice|fs.ModeCharDevice) == 0
}

// observeSourceFile opens a single-file source for metadata only.
//
// The handle asks for nothing but FILE_READ_ATTRIBUTES and shares read, write
// and delete. An open like that doesn't take part in sharing checks, so a file
// another program holds exclusively (an Outlook .pst, a database that is
// running, a document open in Office) still opens. Asking for read access
// would fail there with ERROR_SHARING_VIOLATION (32), which the probe would
// classify as "failed", unreachable storage, and pause a job whose storage is
// perfectly present; whether the engine can read the contents is for the
// engine to report in its backup result. FILE_FLAG_OPEN_REPARSE_POINT opens a
// link as the link itself, so the tag check below refuses it instead of
// following it, and FILE_FLAG_BACKUP_SEMANTICS lets a folder that replaced the
// file open so that it can be recognised.
//
// The facts come from the unchanged observeFacts, which works from the same
// handle a folder's open would give it.
func observeSourceFile(path string) (sourceFileObservation, error) {
	pointer, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return sourceFileObservation{}, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(pointer, windows.FILE_READ_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING,
		windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		return sourceFileObservation{}, &os.PathError{Op: "open", Path: path, Err: err}
	}
	file := os.NewFile(uintptr(handle), path)
	defer file.Close()
	var info struct {
		FileAttributes uint32
		ReparseTag     uint32
	}
	if err := windows.GetFileInformationByHandleEx(handle, windows.FileAttributeTagInfo,
		(*byte)(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		return sourceFileObservation{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	nameSurrogate := info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 &&
		info.ReparseTag&windowsReparseTagNameSurrogate != 0
	directory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
	fileType, err := windows.GetFileType(handle)
	if err != nil {
		return sourceFileObservation{}, &os.PathError{Op: "stat", Path: path, Err: err}
	}
	observed := sourceFileObservation{
		regular:   !directory && !nameSurrogate && fileType == windows.FILE_TYPE_DISK,
		directory: directory && !nameSurrogate,
	}
	if !observed.regular {
		return observed, nil
	}
	observed.facts, observed.failures = observeFacts(file, path)
	return observed, nil
}
