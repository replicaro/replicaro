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
	err     error
}

// directoryListingCalls holds at most one listing per resolved path. A
// request for a path whose earlier listing is still stuck in the OS joins that
// call (with its own time limit) instead of starting another blocked call, so
// repeated clicks on a dead share cannot pile up threads, and a slow folder is
// never reported against a different folder's request. The key is the exact
// resolved path; spellings that differ only in case on Windows are separate
// calls, which keeps each response's Current exactly as requested.
var directoryListingCalls = struct {
	sync.Mutex
	inFlight map[string]*directoryListingCall
}{inFlight: map[string]*directoryListingCall{}}

// listPickerDirectory is a variable only so tests can simulate a folder that
// never answers.
var listPickerDirectory = listDirectory

// browseDirectoriesBounded is the picker endpoint's entry point: the path is
// resolved lexically first, and only the filesystem work runs in the shared
// per-path call under directoryListingTimeout.
func browseDirectoriesBounded(ctx context.Context, requested string) (directoryListing, error) {
	abs, err := resolveBrowsePath(requested)
	if err != nil {
		return directoryListing{}, err
	}
	directoryListingCalls.Lock()
	call := directoryListingCalls.inFlight[abs]
	if call == nil {
		call = &directoryListingCall{done: make(chan struct{})}
		directoryListingCalls.inFlight[abs] = call
		go func() {
			listing, err := listPickerDirectory(abs)
			directoryListingCalls.Lock()
			call.listing, call.err = listing, err
			// Remove before closing so a request that arrives after this
			// result starts a fresh listing instead of reading a stale one.
			delete(directoryListingCalls.inFlight, abs)
			directoryListingCalls.Unlock()
			close(call.done)
		}()
	}
	directoryListingCalls.Unlock()
	timer := time.NewTimer(directoryListingTimeout)
	defer timer.Stop()
	select {
	case <-call.done:
		return call.listing, call.err
	case <-timer.C:
		return directoryListing{}, errDirectoryNotResponding
	case <-ctx.Done():
		return directoryListing{}, ctx.Err()
	}
}

// resolveBrowsePath is lexical only (plus the home directory lookup), so it
// never blocks on the storage being browsed.
func resolveBrowsePath(requested string) (string, error) {
	// Keep whitespace in the chosen route; normalization below rejects `..`
	// before cleanup so browsing cannot silently choose another directory.
	path := requested
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

func listDirectory(abs string) (directoryListing, error) {
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
		}
	}
	sort.Slice(directories, func(i, j int) bool {
		return strings.ToLower(directories[i].Name) < strings.ToLower(directories[j].Name)
	})
	parent := filepath.Dir(abs)
	if parent == abs {
		parent = ""
	}
	return directoryListing{
		Current: abs, Parent: parent, Roots: directoryRoots(), Directories: directories,
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
