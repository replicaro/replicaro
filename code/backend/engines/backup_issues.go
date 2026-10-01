package engines

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
)

// BackupSourceReadFailure only changes how the result is presented. The wrapped
// requested operation is still a failure, with its original native exit cause.
// Keeping the two separate means a saved but incomplete snapshot cannot trigger
// retention or count toward successful-backup health and statistics.
type BackupSourceReadFailure struct {
	SnapshotID string
	Err        error
}

func (e *BackupSourceReadFailure) Error() string { return e.Err.Error() }
func (e *BackupSourceReadFailure) Unwrap() error { return e.Err }

func IsBackupSourceReadFailure(err error) bool {
	if partial, ok := err.(*BackupSourceReadFailure); ok {
		return partial.SnapshotID != "" && partial.Err != nil
	}
	// The executor joins stage errors. A source-read qualification cannot hide
	// a second failure from result persistence or occurrence bookkeeping.
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		causes := joined.Unwrap()
		if len(causes) == 0 {
			return false
		}
		for _, cause := range causes {
			if !IsBackupSourceReadFailure(cause) {
				return false
			}
		}
		return true
	}
	if cause := errors.Unwrap(err); cause != nil {
		return IsBackupSourceReadFailure(cause)
	}
	return false
}

// backupExit is the backup rule for trusting a native exit code: the requested
// child exited with exactly this code and no follow-up of any kind failed next
// to it. Backups are stricter than restores here on purpose; a saved-snapshot
// claim is only made from a completely clean exit record.
func backupExit(err error, code int) bool {
	followupErr, exited := requestedNativeExit(err, code)
	return exited && followupErr == nil
}

func resticPartialSnapshot(capture *command.CapturedOutput, runErr error) (models.Snapshot, bool) {
	if capture == nil || !backupExit(runErr, 3) {
		return models.Snapshot{}, false
	}
	reader, err := capture.OpenStdout()
	if err != nil {
		return models.Snapshot{}, false
	}
	defer reader.Close()
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var snapshot models.Snapshot
	summaries := 0
	for scanner.Scan() {
		value, err := decodeUniqueJSON(scanner.Bytes())
		if err != nil {
			return models.Snapshot{}, false
		}
		row, ok := value.(map[string]any)
		if !ok {
			return models.Snapshot{}, false
		}
		if row["message_type"] != "summary" {
			continue
		}
		summaries++
		id, _ := row["snapshot_id"].(string)
		if !resticSnapshotIDPattern.MatchString(id) || id != strings.ToLower(id) || (row["dry_run"] != nil && row["dry_run"] != false) {
			return models.Snapshot{}, false
		}
		snapshot = parseResticBackupOutput(scanner.Text())
	}
	return snapshot, scanner.Err() == nil && summaries == 1 && snapshot.ID != ""
}

// readPath is the exact folder passed to "kopia snapshot create". It differs
// from the recorded snapshot source when the job has an "Update job source"
// alias: Kopia then reads the alias while --override-source records the
// job's immutable source.
func kopiaPartialSnapshot(capture *command.CapturedOutput, runErr error, readPath string) (models.Snapshot, bool) {
	if capture == nil || !backupExit(runErr, 1) {
		return models.Snapshot{}, false
	}
	reader, err := capture.OpenStdout()
	if err != nil {
		return models.Snapshot{}, false
	}
	// Read at most 8 MiB of stdout as a single snapshot result record. This is not
	// a repository listing and never a substitute for native repository
	// verification. Output that is too large or ambiguous leaves the backup
	// reported as failed.
	data, err := io.ReadAll(io.LimitReader(reader, 8*1024*1024+1))
	_ = reader.Close()
	if err != nil || len(data) > 8*1024*1024 {
		return models.Snapshot{}, false
	}
	value, err := decodeUniqueJSON(data)
	if err != nil {
		return models.Snapshot{}, false
	}
	compact, err := json.Marshal(value)
	if err != nil {
		return models.Snapshot{}, false
	}
	snapshot, err := parseKopiaSnapshot(string(compact))
	if err != nil {
		return models.Snapshot{}, false
	}
	var row struct {
		ID               string `json:"id"`
		IncompleteReason string `json:"incomplete"`
		RootEntry        struct {
			Object  string `json:"obj"`
			Summary struct {
				Failed int64 `json:"numFailed"`
				Errors []struct {
					Path  string `json:"path"`
					Error string `json:"error"`
				} `json:"errors"`
			} `json:"summ"`
		} `json:"rootEntry"`
	}
	if json.Unmarshal(compact, &row) != nil || !canonicalKopiaPartialID(row.ID) || row.RootEntry.Object == "" || row.IncompleteReason != "" || row.RootEntry.Summary.Failed <= 0 || int64(len(row.RootEntry.Summary.Errors)) != row.RootEntry.Summary.Failed {
		return models.Snapshot{}, false
	}
	for _, item := range row.RootEntry.Summary.Errors {
		if item.Path == "" || !(kopiaSourceReadError(item.Error) || kopiaSourcePathError(item.Error, snapshot.Source, item.Path) ||
			(readPath != "" && readPath != snapshot.Source && kopiaSourcePathError(item.Error, readPath, item.Path))) {
			return models.Snapshot{}, false
		}
	}
	stderr, err := capture.OpenStderr()
	if err != nil {
		return models.Snapshot{}, false
	}
	defer stderr.Close()
	scanner := bufio.NewScanner(stderr)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var last string
	for scanner.Scan() {
		if line := strings.TrimSpace(scanner.Text()); line != "" {
			last = line
		}
	}
	// In pinned Kopia, manifest output precedes the final flush. Only the final
	// source-error return proves flush did not supersede it with a publication
	// failure. Earlier progress/errors or a JSON snapshot ID alone prove neither.
	expected := fmt.Sprintf("Found %d fatal error(s) while snapshotting %s@%s:%s.", row.RootEntry.Summary.Failed, snapshot.NativeSourceUser, snapshot.NativeSourceHost, snapshot.Source)
	return snapshot, scanner.Err() == nil && last == expected
}

// Only pinned source-side error prefixes qualify. Object writer and repository
// errors can also appear in the manifest's fatal list, so an arbitrary item
// error must never be interpreted as an incomplete source capture.
func kopiaSourceReadError(message string) bool {
	return strings.HasPrefix(message, "unable to open file: unable to open local file: ") ||
		strings.HasPrefix(message, "cannot create iterator: unable to read directory: ") ||
		strings.HasPrefix(message, "unable to read symlink: ")
}

// The prefixes above were observed from pinned Kopia on Linux, where they wrap
// file-open, directory-read, and symlink-read failures. Windows source reads
// (and Linux reads that fail after a file opened) surface instead as raw Go
// *os.PathError text, "<op> <absolute path>: <message>", for example "read
// \\server\share\a.mp4: ..." or "GetFileInformationByHandleEx
// \\server\share\dir: ...". That shape alone proves nothing, because
// repository and object-writer failures can carry path errors too. It
// qualifies only when the absolute path is exactly the snapshot's native
// source root joined with the manifest item's own relative path, which ties
// the failure to reading that source item rather than to writing the
// repository.
//
// For a job with an "Update job source" alias the recorded source (set by
// --override-source) is the immutable source, but Kopia actually opened
// files under the alias, so its path errors name the alias. The caller
// therefore also accepts the exact folder it passed to Kopia as the root.
// That is the same proof, just against the root Kopia really read; every
// other condition (exact join, one path per item, count, final line) is
// unchanged. Do not widen this to prefix or case-insensitive matching.
func kopiaSourcePathError(message, sourceRoot, itemPath string) bool {
	op, rest, found := strings.Cut(message, " ")
	if !found || op == "" || strings.ContainsAny(op, ": \t\r\n\\/") {
		return false
	}
	windows := kopiaWindowsSourceRoot(sourceRoot)
	separator := "/"
	root := sourceRoot
	if windows {
		separator = `\`
		root = kopiaNormalizeWindowsPath(root)
		itemPath = strings.ReplaceAll(itemPath, "/", separator)
		rest = kopiaNormalizeWindowsPath(rest)
	} else if !strings.HasPrefix(root, "/") {
		return false
	}
	for _, segment := range strings.Split(itemPath, separator) {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	base := strings.TrimRight(root, separator)
	if windows && base == "" {
		return false
	}
	prefix := base + separator + itemPath + ": "
	return strings.HasPrefix(rest, prefix) && strings.TrimSpace(rest[len(prefix):]) != ""
}

func kopiaWindowsSourceRoot(root string) bool {
	if strings.HasPrefix(root, `\\`) {
		return true
	}
	return len(root) >= 3 && root[1] == ':' && (root[2] == '\\' || root[2] == '/') &&
		((root[0] >= 'A' && root[0] <= 'Z') || (root[0] >= 'a' && root[0] <= 'z'))
}

// Windows may spell one path with either separator and with or without the
// extended-length prefix (\\?\UNC\server\share versus \\server\share).
func kopiaNormalizeWindowsPath(path string) string {
	path = strings.ReplaceAll(path, "/", `\`)
	if strings.HasPrefix(path, `\\?\UNC\`) {
		return `\\` + path[len(`\\?\UNC\`):]
	}
	return strings.TrimPrefix(path, `\\?\`)
}

func canonicalKopiaPartialID(id string) bool {
	if len(id) != 32 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
