package api

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/local/replicaro/storageidentity"
)

type directoryEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

type directoryListing struct {
	Current     string           `json:"current"`
	Parent      string           `json:"parent"`
	Roots       []directoryEntry `json:"roots"`
	Directories []directoryEntry `json:"directories"`
	// Files is filled only in the job-source picker's file mode and is left
	// out of the JSON otherwise, so the folders-only listing every other
	// picker reads has no "files" key. It is also left out when a folder in
	// file mode has no files.
	Files []directoryEntry `json:"files,omitempty"`
}

// Picker modes. The default lists folders only, for every picker. The job
// source picker opts in to pickerModeFiles, which also lists the files a job
// can back up, and the new-job form opts in to pickerModeKind for a typed
// path, which answers only whether it is a file, a folder or neither.
const (
	pickerModeFolders = ""
	pickerModeFiles   = "files"
	pickerModeKind    = "kind"
)

// Answers of the kind lookup. They are a hint for the form; whether a path can
// really be a job source is decided by the binding probe when it is saved.
const (
	pickerKindFile    = "file"
	pickerKindFolder  = "folder"
	pickerKindNeither = "neither"
)

// pickerKindAnswer is the kind lookup's whole response.
type pickerKindAnswer struct {
	Kind string `json:"kind"`
}

// directoryListingTimeout bounds how long one picker request waits for a
// folder listing. A dead network share or a disconnected mapped drive can
// block Stat/ReadDir for minutes, and without this bound the picker would
// keep spinning. The OS call cannot be cancelled from Go: when the bound
// expires the request ends with errDirectoryNotResponding, and the call keeps
// running in its goroutine until the OS gives up. That goroutine is
// deliberately left alone rather than treated as a leak; directoryListingCalls
// keeps it to one per path. The job source folder check has its own bound of
// the same length (jobSourceFolderReadTimeout) and no per-path deduplication.
// TODO: move the picker into the storage helper process, which can be killed.
// A variable only so tests can shorten it.
var directoryListingTimeout = 60 * time.Second

// errDirectoryNotResponding is returned as the "directory_not_responding"
// API code; the picker shows its own localized title and body for it.
var errDirectoryNotResponding = errors.New("this folder did not respond within 60 seconds")

type directoryListingCall struct {
	done    chan struct{}
	listing directoryListing
	kind    string
	err     error
}

// directoryListingCalls holds at most one listing per resolved path and
// picker mode. A request for a path whose earlier listing is still stuck in
// the OS joins that call (with its own time limit) instead of starting another
// blocked call, so repeated clicks on a dead share cannot pile up threads, and
// a slow folder is never reported against a different folder's request. The
// key is the exact resolved path; spellings that differ only in case on
// Windows are separate calls, which keeps each response's Current exactly as
// requested. See directoryCallKey for how the mode is part of the key.
var directoryListingCalls = struct {
	sync.Mutex
	inFlight map[string]*directoryListingCall
}{inFlight: map[string]*directoryListingCall{}}

// directoryCallKey keys the default folders-only listing by the bare path and
// the opt-in modes by mode, NUL and path. The modes answer different
// questions about the same path, so a file-mode request must never join a
// folders-only call (it would get a listing without files) or the reverse. NUL
// can't occur in a resolved path, so the keys never collide.
func directoryCallKey(mode, abs string) string {
	if mode == pickerModeFolders {
		return abs
	}
	return mode + "\x00" + abs
}

// listPickerDirectory is a variable only so tests can simulate a folder that
// never answers.
var listPickerDirectory = listDirectory

// listPickerFiles and lookupPickerKind are the file-mode and kind-lookup
// counterparts of listPickerDirectory, variables for the same reason.
var listPickerFiles = listDirectoryWithFiles
var lookupPickerKind = pickerPathKind

// browseDirectoriesBounded is the picker endpoint's entry point: the path is
// resolved lexically first, and only the filesystem work runs in the shared
// per-path call under directoryListingTimeout. mode is pickerModeFolders or
// pickerModeFiles.
func browseDirectoriesBounded(ctx context.Context, requested string, containerPackage bool, mode string) (directoryListing, error) {
	abs, err := resolveBrowsePath(requested, containerPackage)
	if err != nil {
		return directoryListing{}, err
	}
	call := startPickerCall(mode, abs, func() (directoryListing, string, error) {
		if mode == pickerModeFiles {
			listing, err := listPickerFiles(abs)
			return listing, "", err
		}
		listing, err := listPickerDirectory(abs)
		return listing, "", err
	})
	if err := waitPickerCall(ctx, call); err != nil {
		return directoryListing{}, err
	}
	return call.listing, call.err
}

// lookupPickerKindBounded answers the new-job form's kind lookup for a typed
// path under the same bound and per-path sharing as a listing. An empty or
// lexically invalid path is "neither": the lookup is only a hint, and the save
// reports the real problem with its own message.
func lookupPickerKindBounded(ctx context.Context, requested string, containerPackage bool) (string, error) {
	if requested == "" {
		return pickerKindNeither, nil
	}
	abs, err := resolveBrowsePath(requested, containerPackage)
	if err != nil {
		return pickerKindNeither, nil
	}
	call := startPickerCall(pickerModeKind, abs, func() (directoryListing, string, error) {
		return directoryListing{}, lookupPickerKind(abs), nil
	})
	if err := waitPickerCall(ctx, call); err != nil {
		return "", err
	}
	return call.kind, nil
}

// startPickerCall joins the in-flight call for this mode and path, or starts
// one that runs work in its own goroutine.
func startPickerCall(mode, abs string, work func() (directoryListing, string, error)) *directoryListingCall {
	key := directoryCallKey(mode, abs)
	directoryListingCalls.Lock()
	defer directoryListingCalls.Unlock()
	call := directoryListingCalls.inFlight[key]
	if call == nil {
		call = &directoryListingCall{done: make(chan struct{})}
		directoryListingCalls.inFlight[key] = call
		go func() {
			listing, kind, err := work()
			directoryListingCalls.Lock()
			call.listing, call.kind, call.err = listing, kind, err
			// Remove before closing so a request that arrives after this
			// result starts a fresh listing instead of reading a stale one.
			delete(directoryListingCalls.inFlight, key)
			directoryListingCalls.Unlock()
			close(call.done)
		}()
	}
	return call
}

// waitPickerCall waits for call under directoryListingTimeout. A nil result
// means the call finished and its fields can be read.
func waitPickerCall(ctx context.Context, call *directoryListingCall) error {
	timer := time.NewTimer(directoryListingTimeout)
	defer timer.Stop()
	select {
	case <-call.done:
		return nil
	case <-timer.C:
		return errDirectoryNotResponding
	case <-ctx.Done():
		return ctx.Err()
	}
}

// resolveBrowsePath is lexical only (plus the home directory lookup), so it
// never blocks on the storage being browsed.
//
// An empty path opens the user's home folder, except in the Docker package.
// There HOME is Replicaro's own folder inside its state volume, which no one
// should pick or mount over, and the user's folders are bind mounts at paths
// they chose, so the picker starts at / where those mounts are visible.
func resolveBrowsePath(requested string, containerPackage bool) (string, error) {
	// Keep whitespace in the chosen route; normalization below rejects `..`
	// before cleanup so browsing cannot silently choose another directory.
	path := requested
	if path == "" && containerPackage {
		path = "/"
	}
	if path == "" {
		var err error
		path, err = os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("find home directory: %w", err)
		}
	}
	if runtime.GOOS == "windows" && len(path) >= 4 && path[0] == '/' &&
		((path[1] >= 'A' && path[1] <= 'Z') || (path[1] >= 'a' && path[1] <= 'z')) &&
		path[2] == ':' && (path[3] == '/' || path[3] == '\\') {
		path = path[1:]
	}
	abs, err := storageidentity.NormalizeConfiguredPath(path)
	if err != nil {
		return "", fmt.Errorf("resolve directory: %w", err)
	}
	return abs, nil
}

// listDirectory is the folders-only listing every picker uses by default.
func listDirectory(abs string) (directoryListing, error) {
	return listPickerEntries(abs, false)
}

// listDirectoryWithFiles is the job-source picker's file mode: the same folder
// listing plus the files a job can back up. A path that is not a folder (a
// file path typed in the source field) opens the folder that holds it, so the
// picker starts next to that file; the UI finds the file in Files by its
// path. Any other failure is the folders-only listing's error.
func listDirectoryWithFiles(abs string) (directoryListing, error) {
	if info, err := os.Stat(abs); err == nil && !info.IsDir() {
		if parent := filepath.Dir(abs); parent != abs {
			abs = parent
		}
	}
	return listPickerEntries(abs, true)
}

// pickerPathKind is the kind lookup's answer for one resolved path, using the
// probe's own classification (storageidentity.SourcePathKind).
func pickerPathKind(abs string) string {
	switch storageidentity.SourcePathKind(abs) {
	case storageidentity.PathKindFile:
		return pickerKindFile
	case storageidentity.PathKindFolder:
		return pickerKindFolder
	default:
		return pickerKindNeither
	}
}

// listPickerEntries lists abs for the picker. Folders follow the same rules
// with and without files, so a file-mode listing shows exactly the folders a
// folders-only listing would.
//
// With withFiles, an entry that the directory read reports as a regular file
// is offered as a file. A symbolic link to a file, a device, a pipe or a
// socket is left out, because a job can't back it up (the binding probe
// refuses it). On Windows an entry Go reports as irregular is a reparse point
// that isn't a link or junction; it is settled the way the probe settles it,
// so a OneDrive placeholder or a deduplicated file is offered and anything
// else is left out. Do not follow links to find more files here: the probe
// would refuse the link when the job is saved.
func listPickerEntries(abs string, withFiles bool) (directoryListing, error) {
	info, err := os.Stat(abs)
	if err != nil {
		return directoryListing{}, fmt.Errorf("open directory: %w", err)
	}
	if !info.IsDir() {
		return directoryListing{}, fmt.Errorf("%s is not a directory", abs)
	}
	entries, err := os.ReadDir(abs)
	if err != nil {
		return directoryListing{}, fmt.Errorf("read directory: %w", err)
	}
	directories := make([]directoryEntry, 0)
	var files []directoryEntry
	for _, entry := range entries {
		isDirectory := entry.IsDir()
		entryType := entry.Type()
		if entryType&os.ModeSymlink != 0 ||
			(runtime.GOOS == "windows" && entryType&os.ModeIrregular != 0) {
			// Follow only to establish directory eligibility. Keep the selected
			// route in the response: resolving the target here would change later
			// source/vault binding. Windows junctions can report ModeIrregular.
			// Broken, looping and non-directory links vanish.
			info, err := os.Stat(filepath.Join(abs, entry.Name()))
			isDirectory = err == nil && info.IsDir()
			if isDirectory && runtime.GOOS == "windows" && directoryListingDenied(filepath.Join(abs, entry.Name())) {
				// Windows compatibility junctions can allow the metadata read
				// above while denying directory listing. Do not offer a route
				// that immediately fails when opened in the picker. Check access,
				// not names or reparse tags: usable junctions remain selectable.
				isDirectory = false
			}
		}
		if isDirectory {
			directories = append(directories, directoryEntry{
				Name: entry.Name(), Path: filepath.Join(abs, entry.Name()),
			})
			continue
		}
		if !withFiles {
			continue
		}
		isFile := entryType.IsRegular()
		if runtime.GOOS == "windows" && entryType&os.ModeIrregular != 0 {
			isFile = storageidentity.SourcePathKind(filepath.Join(abs, entry.Name())) == storageidentity.PathKindFile
		}
		if isFile {
			files = append(files, directoryEntry{Name: entry.Name(), Path: filepath.Join(abs, entry.Name())})
		}
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.ToLower(directories[i].Name) < strings.ToLower(directories[j].Name)
	})
	sort.Slice(files, func(i, j int) bool {
		return strings.ToLower(files[i].Name) < strings.ToLower(files[j].Name)
	})
	parent := filepath.Dir(abs)
	if parent == abs {
		parent = ""
	}
	return directoryListing{
		Current: abs, Parent: parent, Roots: directoryRoots(), Directories: directories, Files: files,
	}, nil
}

func directoryListingDenied(path string) bool {
	// Probe only Windows link candidates, requesting one name to check that the
	// directory can be listed. EOF means an accessible empty directory.
	// Only permission denial hides an entry; transient or unrelated errors keep
	// their normal direct-open error path. This does not replace the checks made
	// later when the folder is opened or chosen as a source or destination, and
	// it never resolves the chosen route into a different path.
	directory, err := os.Open(path)
	if err != nil {
		return os.IsPermission(err)
	}
	defer directory.Close()
	_, err = directory.Readdirnames(1)
	return os.IsPermission(err)
}
