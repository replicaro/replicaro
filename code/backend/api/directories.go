package api

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

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

func browseDirectories(requested string) (directoryListing, error) {
	// Keep whitespace in the chosen route; normalization below rejects `..`
	// before cleanup so browsing cannot silently choose another directory.
	path := requested
	if path == "" {
		var err error
		path, err = os.UserHomeDir()
		if err != nil {
			return directoryListing{}, fmt.Errorf("find home directory: %w", err)
		}
	}
	if runtime.GOOS == "windows" && len(path) >= 4 && path[0] == '/' &&
		((path[1] >= 'A' && path[1] <= 'Z') || (path[1] >= 'a' && path[1] <= 'z')) &&
		path[2] == ':' && (path[3] == '/' || path[3] == '\\') {
		path = path[1:]
	}
	abs, err := storageidentity.NormalizeConfiguredPath(path)
	if err != nil {
		return directoryListing{}, fmt.Errorf("resolve directory: %w", err)
	}
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
	// Probe only Windows link candidates, requesting one name to establish
	// listing access. EOF means an accessible empty directory.
	// Only permission denial hides an entry; transient or unrelated errors keep
	// their normal direct-open error path. This observation does not replace
	// later navigation or source/destination admission, and never resolves the
	// chosen route into a different path.
	directory, err := os.Open(path)
	if err != nil {
		return os.IsPermission(err)
	}
	defer directory.Close()
	_, err = directory.Readdirnames(1)
	return os.IsPermission(err)
}
