package engines

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
)

// resticNodeModified turns the mtime of a Restic `ls --json` node into whole
// Unix seconds. Restic prints it in RFC 3339 with nanoseconds and the zone of
// the computer that made the backup; the instant is what matters, so the
// seconds are taken in UTC and the fraction is dropped (floor, also before
// 1970). A node without mtime gives 0, a malformed one fails the listing.
func resticNodeModified(value string) (int64, error) {
	if value == "" {
		return 0, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return 0, fmt.Errorf("parse restic listing: node has a malformed modification time")
	}
	return parsed.Unix(), nil
}

// ResticDiffChange is one path that `restic diff --json --metadata` reported.
// Path is the native archive path as Restic printed it, without the trailing
// slash it adds to folders (IsDir records that slash). Modifier is Restic's
// own mark, unchanged: "+" added, "-" removed, "M" content, "U" metadata only,
// "T" type, "?" bitrot, alone or combined.
type ResticDiffChange struct {
	Path     string
	Modifier string
	IsDir    bool
}

// Removed reports whether Restic marked the path as gone from the target.
func (change ResticDiffChange) Removed() bool { return change.Modifier == "-" }

// ResticSnapshotDiff runs `restic diff --json --metadata <base> <target>` and
// returns every change line. File History uses it to tell a file's versions
// apart in Restic vaults: Restic's ls has no content ID, and diff is the only
// native command that says whether a file changed between two snapshots,
// including content changes that kept the size and modified time.
//
// The command reads only folder metadata of the two snapshots (no file data)
// and takes Restic's usual non-exclusive lock. Its output is parsed strictly:
// every line must be a change or the final statistics record, and the
// statistics must name exactly the two snapshots asked for. Anything else is
// a failure rather than a partial result, because a missing line would hide
// a version.
func ResticSnapshotDiff(ctx context.Context, engine Engine, repo models.Repository, base, target string) ([]ResticDiffChange, string, error) {
	inner := engine
	if public, ok := engine.(*publicEngine); ok {
		inner = public.Engine
	}
	restic, ok := inner.(*resticEngine)
	if !ok {
		return nil, "", fmt.Errorf("snapshot diff is only used for Restic vaults")
	}
	for _, id := range []string{base, target} {
		if err := ValidateSnapshotIDArgument(id); err != nil {
			return nil, "", resticPreparationFailure(err)
		}
	}
	capture, out, err := restic.runRepoCommandCaptured(ctx, repo, nil, []string{"diff", "--json", "--metadata", base, target}, command.NoTotalDeadline)
	if capture != nil {
		defer capture.Close()
	}
	if err != nil {
		return nil, out, err
	}
	stdout, err := capture.OpenStdout()
	if err != nil {
		return nil, out, resticCommandFailure(&command.OutputProcessingFailure{Err: fmt.Errorf("parse restic diff: %w", err)}, true)
	}
	changes, err := parseResticDiff(stdout, base, target)
	_ = stdout.Close()
	if err != nil {
		return nil, out, resticCommandFailure(&command.OutputProcessingFailure{Err: err}, true)
	}
	return changes, out, nil
}

func parseResticDiff(output io.Reader, base, target string) ([]ResticDiffChange, error) {
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	var changes []ResticDiffChange
	statistics := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if statistics {
			return nil, fmt.Errorf("parse restic diff: output continues after the statistics")
		}
		var row struct {
			MessageType string `json:"message_type"`
			Path        string `json:"path"`
			Modifier    string `json:"modifier"`
			Source      string `json:"source_snapshot"`
			Target      string `json:"target_snapshot"`
		}
		if err := json.Unmarshal([]byte(line), &row); err != nil {
			return nil, fmt.Errorf("parse restic diff: malformed JSON: %w", err)
		}
		switch row.MessageType {
		case "change":
			if !strings.HasPrefix(row.Path, "/") || !validResticDiffModifier(row.Modifier) {
				return nil, fmt.Errorf("parse restic diff: malformed change record")
			}
			change := ResticDiffChange{Path: row.Path, Modifier: row.Modifier}
			if len(change.Path) > 1 && strings.HasSuffix(change.Path, "/") {
				change.Path, change.IsDir = strings.TrimSuffix(change.Path, "/"), true
			}
			changes = append(changes, change)
		case "statistics":
			if !strings.EqualFold(row.Source, base) || !strings.EqualFold(row.Target, target) {
				return nil, fmt.Errorf("parse restic diff: statistics name other snapshots than the ones compared")
			}
			statistics = true
		default:
			return nil, fmt.Errorf("parse restic diff: unexpected record type %q", row.MessageType)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("parse restic diff: %w", err)
	}
	if !statistics {
		return nil, fmt.Errorf("parse restic diff: statistics record is missing")
	}
	return changes, nil
}

// ResticRelativePath maps an archive path from `restic diff` to the path
// relative to one of the snapshot's native source roots, the same way the
// content listing maps its nodes, so a change lines up with the file the
// index already holds. It returns false for a path outside that root
// (including the folders above it, which diff also reports).
func ResticRelativePath(root, archivePath string) (string, bool) {
	return normalizeResticEntryPath(normalizeResticPath(root), archivePath)
}

// validResticDiffModifier accepts the marks Restic's diff documents: a lone
// "-" for a removal, or any combination of "+", "M", "U", "T" and "?".
func validResticDiffModifier(modifier string) bool {
	if modifier == "-" {
		return true
	}
	if modifier == "" {
		return false
	}
	for _, mark := range modifier {
		if !strings.ContainsRune("+MUT?", mark) {
			return false
		}
	}
	return true
}
