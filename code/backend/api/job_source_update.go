package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/storageavailability"
)

// "Update job source"
//
// When a bound job's source can no longer be used (paused as "source
// unavailable", or failing because the data moved), the user picks the folder
// where the data now lives. That folder becomes the job's alias
// (resolved_source_path); the job's immutable source is never replaced,
// because it is the job's native retention and File History scope. See
// database.UpdateJobSourceAlias for the storage rules and
// database.MetadataGroupingRoot for how history stays together.
//
// Before saving, a folder check compares the chosen folder with the job's
// current location. It is a warning the user can override, not a gate: the
// user may really have reorganized their data, and only they can tell. It
// exists because the most likely mistake here is picking the wrong folder
// (for example the empty folder left behind by an unmounted share), and
// backing that up under the job would let native retention expire the real
// snapshots. Nothing in it calls an engine; the snapshot side comes from
// Replicaro's metadata cache.

// jobSourceFolderReadTimeout bounds the in-process read of the chosen folder's
// top-level names. It matches the directory picker's bound. The OS call itself
// cannot be cancelled; on a hung share it keeps running in its goroutine and
// finishes (or not) on its own, which is the same accepted trade-off as the
// picker until both move into the storage helper. A variable only so tests
// can shorten it.
var jobSourceFolderReadTimeout = 60 * time.Second

type jobSourceUpdateRequest struct {
	ID   string `json:"id"`
	Path string `json:"path"`
	// Confirmed skips the folder check after the user chose "Continue with the
	// selected folder" in the warning.
	Confirmed bool `json:"confirmed"`
}

// jobSourceFolderCheck is the one warning shown before saving. Reason is the
// first matching check in this fixed order: "empty", "name", "items".
type jobSourceFolderCheck struct {
	Reason     string `json:"reason"`
	Chosen     string `json:"chosen"`
	Current    string `json:"current"`
	ChosenName string `json:"chosenName"`
	SourceName string `json:"sourceName"`
	Matched    int    `json:"matched"`
	Total      int    `json:"total"`
}

var bindJobSourceAliasStorage = storageavailability.BindJobSourceAlias
var readJobSourceFolderNames = readFolderTopLevelNames
var latestJobSnapshotNames = database.LatestReadyJobSnapshotTopLevelNames

// osClutterNames are entries operating systems create on their own. They are
// ignored when deciding whether the chosen folder is empty and when comparing
// names with the latest backup, and nowhere else: the engines still back up
// whatever is in the folder. Matching is case-insensitive on every platform
// because these names arrive from shares formatted by other systems too.
var osClutterNames = map[string]bool{
	"desktop.ini":               true,
	"thumbs.db":                 true,
	"$recycle.bin":              true,
	"system volume information": true,
	".ds_store":                 true,
	".spotlight-v100":           true,
	".trashes":                  true,
	".fseventsd":                true,
	".temporaryitems":           true,
	"lost+found":                true,
}

func isOSClutterName(name string) bool {
	lower := strings.ToLower(name)
	return osClutterNames[lower] || strings.HasPrefix(lower, ".trash-")
}

// folderNameFold folds names the way the host's usual filesystems compare
// them: case-insensitively on Windows and macOS, exactly elsewhere.
func folderNameFold(goos string) func(string) string {
	if goos == "windows" || goos == "darwin" {
		return strings.ToLower
	}
	return func(value string) string { return value }
}

// folderDisplayName is the last component of a folder path, or the whole path
// for a root such as E:\, \\server\share, or /.
func folderDisplayName(path string) string {
	trimmed := strings.TrimRight(path, `\/`)
	if trimmed == "" {
		return path
	}
	name := filepath.Base(trimmed)
	if name == "." || name == "" || filepath.VolumeName(trimmed) == trimmed ||
		strings.HasSuffix(trimmed, ":") {
		return path
	}
	return name
}

// isDriveOrShareRoot reports whether path names a whole drive or share: a
// Windows drive root (E:\ or E:), a UNC share root (\\server\share, also in the
// \\?\ and \\?\UNC\ spellings), or the POSIX root. It is lexical only and
// is used only by the folder check.
func isDriveOrShareRoot(path string) bool {
	trimmed := strings.TrimRight(path, `\/`)
	if trimmed == "" {
		return path != ""
	}
	windows := strings.ReplaceAll(trimmed, "/", `\`)
	switch {
	case strings.HasPrefix(windows, `\\?\UNC\`):
		windows = `\\` + windows[len(`\\?\UNC\`):]
	case strings.HasPrefix(windows, `\\?\`):
		windows = windows[len(`\\?\`):]
	}
	if len(windows) == 2 && windows[1] == ':' &&
		(windows[0] >= 'A' && windows[0] <= 'Z' || windows[0] >= 'a' && windows[0] <= 'z') {
		return true
	}
	if strings.HasPrefix(windows, `\\`) {
		parts := 0
		for _, part := range strings.Split(windows[2:], `\`) {
			if part != "" {
				parts++
			}
		}
		return parts <= 2
	}
	return false
}

// readFolderTopLevelNames lists the chosen folder's direct entries in-process
// under jobSourceFolderReadTimeout. Entry names are only compared here and are
// never logged or returned to the client.
func readFolderTopLevelNames(ctx context.Context, path string) ([]string, error) {
	type result struct {
		names []string
		err   error
	}
	// Buffered so an abandoned read can still deliver and exit.
	done := make(chan result, 1)
	go func() {
		folder, err := os.Open(path)
		if err != nil {
			done <- result{err: err}
			return
		}
		names, err := folder.Readdirnames(-1)
		_ = folder.Close()
		done <- result{names: names, err: err}
	}()
	timer := time.NewTimer(jobSourceFolderReadTimeout)
	defer timer.Stop()
	select {
	case read := <-done:
		return read.names, read.err
	case <-timer.C:
		return nil, context.DeadlineExceeded
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// checkJobSourceFolder runs the three checks against the chosen folder and
// returns the first warning, or nil when none applies. current is the location
// the user sees as the job's source today (the alias when one is set).
func checkJobSourceFolder(ctx context.Context, db *sql.DB, job models.BackupJob, chosen string) (*jobSourceFolderCheck, error) {
	fold := folderNameFold(runtime.GOOS)
	current := storageavailability.SourceLocation(job)
	check := &jobSourceFolderCheck{
		Chosen: chosen, Current: current,
		ChosenName: folderDisplayName(chosen), SourceName: folderDisplayName(current),
	}
	entries, err := readJobSourceFolderNames(ctx, chosen)
	if err != nil {
		return nil, err
	}
	present := make(map[string]bool, len(entries))
	for _, name := range entries {
		if !isOSClutterName(name) {
			present[fold(name)] = true
		}
	}
	if len(present) == 0 {
		check.Reason = "empty"
		return check, nil
	}
	// The name check is skipped only when both sides are a whole drive or
	// share (for example E:\ moving to F:\): a root has no folder name of its
	// own, so its "name" is the drive letter or share, and comparing two of
	// those would warn on every drive-letter change, which is the most common
	// real move. A move between a folder and a root (E:\Photos to F:\) still
	// gets the name warning, since picking a whole drive for a folder job is
	// exactly the kind of mistake it is there to catch. The empty and item
	// checks still apply to roots.
	bothRoots := isDriveOrShareRoot(chosen) && isDriveOrShareRoot(current)
	if !bothRoots && fold(check.ChosenName) != fold(check.SourceName) {
		check.Reason = "name"
		return check, nil
	}
	// Without a snapshot whose cache entry is ready there is nothing to
	// compare, so only the first two checks apply.
	snapshot, found, err := latestJobSnapshotNames(ctx, db, job.ID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, nil
	}
	expected := map[string]bool{}
	for _, name := range snapshot.Names {
		if !isOSClutterName(name) {
			expected[fold(name)] = true
		}
	}
	for name := range expected {
		if present[name] {
			check.Matched++
		}
	}
	check.Total = len(expected)
	// "Fewer than half, rounded up": with 5 names at least 3 must be present.
	if check.Total > 0 && check.Matched < (check.Total+1)/2 {
		check.Reason = "items"
		return check, nil
	}
	return nil, nil
}

func handleJobSourceUpdate(db *sql.DB) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			badRequest(w, "invalid method")
			return
		}
		var req jobSourceUpdateRequest
		if err := decodeRequest(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		// The path is a filesystem route and keeps its exact spelling, including
		// surrounding whitespace; lexical cleanup happens in the binding step.
		req.ID = strings.TrimSpace(req.ID)
		if req.ID == "" || req.Path == "" {
			badRequest(w, "id and path are required")
			return
		}
		job, err := database.GetJob(db, req.ID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if job.SourceBindingState == "unbound_imported" {
			writeError(w, http.StatusConflict, database.ErrJobSourceUnbound)
			return
		}
		// Refuse early, before touching the folder, when the save would be
		// refused anyway. UpdateJobSourceAlias repeats this check in its
		// transaction, which is the one that counts.
		if err := database.ValidateJobDeletionAdmission(db, job.ID); err != nil {
			if errors.Is(err, database.ErrJobRunActive) || errors.Is(err, database.ErrJobConnectionReserved) {
				writeError(w, http.StatusConflict, err)
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		bound, err := bindJobSourceAliasStorage(r.Context(), job, req.Path)
		if err != nil {
			err = storageBindingError(err, "source", "read it", "source path could not be observed")
			markSupportStorageObservation(w, storageavailability.StageJobSourceUpdate, err)
			writeError(w, http.StatusConflict, err)
			return
		}
		if !req.Confirmed {
			check, checkErr := checkJobSourceFolder(r.Context(), db, job, storageavailability.SourceLocation(bound))
			if checkErr != nil {
				if errors.Is(checkErr, context.Canceled) {
					return
				}
				writeError(w, http.StatusConflict, &storageBindingFailure{
					message: "source path could not be observed", cause: checkErr,
				})
				return
			}
			if check != nil {
				writeJSON(w, map[string]any{"saved": false, "check": check})
				return
			}
		}
		if err := database.UpdateJobSourceAlias(db, job, bound, time.Now()); err != nil {
			if errors.Is(err, database.ErrJobRunActive) || errors.Is(err, database.ErrJobConnectionReserved) ||
				errors.Is(err, database.ErrJobDefinitionBusy) || errors.Is(err, database.ErrJobSourceChanged) ||
				errors.Is(err, database.ErrJobSourceUnbound) || errors.Is(err, database.ErrJobSourceImmutable) {
				writeError(w, http.StatusConflict, err)
			} else if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, err)
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		saved, err := database.GetJob(db, job.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, map[string]any{"saved": true, "job": saved})
	}
}
