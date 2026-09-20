package engines

import (
	"bufio"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
)

// BackupSourceReadFailure qualifies aggregate presentation only. The wrapped
// requested operation remains failed, including its original native exit cause.
// Keeping those facts separate prevents a saved, incomplete snapshot from
// admitting retention or advancing successful-backup health and statistics.
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

func backupExit(err error, code int) bool {
	status, started, _, nativeErr, followupErr, known := RequestedOperationOutcome(err)
	var exit *exec.ExitError
	return known && started && status == RequestedOperationFailed && followupErr == nil &&
		errors.As(nativeErr, &exit) && exit.ExitCode() == code
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

func kopiaPartialSnapshot(capture *command.CapturedOutput, runErr error) (models.Snapshot, bool) {
	if capture == nil || !backupExit(runErr, 1) {
		return models.Snapshot{}, false
	}
	reader, err := capture.OpenStdout()
	if err != nil {
		return models.Snapshot{}, false
	}
	// This is one bounded result record, never an inventory or a substitute for
	// native repository verification. Oversized/ambiguous evidence stays failed.
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
		if item.Path == "" || !kopiaSourceReadError(item.Error) {
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

func canonicalKopiaPartialID(id string) bool {
	if len(id) != 32 || id != strings.ToLower(id) {
		return false
	}
	_, err := hex.DecodeString(id)
	return err == nil
}
