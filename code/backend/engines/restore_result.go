package engines

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/local/replicaro/command"
)

// RestoreCompletedWithErrors is how a restore adapter reports that the engine
// itself said the restore finished but some items failed. It is the engine's
// own "completed with errors" report, read from its structured output, and
// callers turn it into a completed_with_issues operation.
//
// NativeErr is the requested command's own result and is kept unchanged:
// Restic ends such a restore with exit code 1, so it holds that ordinary
// failed-command error; Kopia run with --ignore-errors exits 0, so it is nil.
// The native step therefore still records Restic as failed and Kopia as
// succeeded, exactly as each engine exited.
//
// Replicaro never builds this value from counts or guesses. Anything that is
// not the exact report described next to each reader stays an ordinary failed
// restore.
type RestoreCompletedWithErrors struct {
	Engine    string
	NativeErr error
	// Errors is the number of failed items the engine reported.
	Errors int
}

func (e *RestoreCompletedWithErrors) Error() string {
	message := fmt.Sprintf("native %s restore completed but reported %d %s", e.Engine, e.Errors, pluralErrors(e.Errors))
	if e.NativeErr != nil {
		return message + ": " + e.NativeErr.Error()
	}
	return message
}

func (e *RestoreCompletedWithErrors) Unwrap() error { return e.NativeErr }

func pluralErrors(count int) string {
	if count == 1 {
		return "error"
	}
	return "errors"
}

// RestoreOutcomeUnknown marks a restore whose requested child exited
// successfully but whose result Replicaro could not read from the output.
// Today only Kopia produces it: restores always run with --ignore-errors, so
// exit 0 alone does not show that anything was restored, and the result has to
// come from the final summary. When that summary is missing, repeated or not
// fully captured, the adapter keeps the native child as succeeded (that is
// what Kopia reported) and returns this value inside a typed
// command.OutputProcessingFailure follow-up.
//
// The marker exists because "native success plus a failed follow-up" is
// otherwise completed with issues. Here the follow-up that failed is the only
// evidence of what was restored, so callers check RestoreOutcomeIsUnknown and
// keep the operation failed. Don't drop the marker and let this fall back to
// the general follow-up rule, and don't turn it back into a native failure:
// the native step and the log header must keep showing Kopia's exit 0.
type RestoreOutcomeUnknown struct {
	Detail string
}

func (e *RestoreOutcomeUnknown) Error() string { return e.Detail }

// RestoreOutcomeIsUnknown reports whether err carries RestoreOutcomeUnknown.
func RestoreOutcomeIsUnknown(err error) bool {
	var unknown *RestoreOutcomeUnknown
	return errors.As(err, &unknown)
}

// RestoreReportedErrors reports whether err carries an engine "completed with
// errors" restore report. Restore requests run one native child, so any other
// error joined next to it is a later Replicaro follow-up, which leaves the
// outcome completed with issues rather than failed.
func RestoreReportedErrors(err error) bool {
	var reported *RestoreCompletedWithErrors
	return errors.As(err, &reported)
}

var resticRestoreErrorCount = regexp.MustCompile(`^Fatal: There were ([1-9][0-9]*) errors$`)

// resticRestoreCompletedWithErrors reads Restic's `restore --json` records and
// returns the engine's error count only when they are exactly Restic's
// "completed with errors" report:
//
//   - the requested command exited with code 1 and its output was captured
//     completely (a damaged capture cannot be trusted). A failed local
//     cleanup afterwards, such as closing the rclone session, does not touch
//     what Restic already reported, so it is left to the caller as an
//     ordinary follow-up failure and the restore stays completed with issues;
//   - stdout holds only status records and exactly one summary record;
//   - stderr holds one error record per failed item and ends with the exit
//     record "Fatal: There were N errors" (code 1), where N matches the
//     number of error records.
//
// Every fatal Restic failure also exits 1, but without the summary and the
// per-item records, so exit 1 alone is never enough. The summary counts are
// deliberately not used: Restic counts the files that failed as restored, so
// they cannot tell a complete restore from a partial one. Unreadable records,
// unknown record types, or a count that does not match leave the restore
// failed. Plain text lines on stderr are skipped rather than rejected: Restic
// itself still prints some diagnostics there as text in --json mode, and
// rclone-backed vaults forward rclone's own log lines there too. Neither is a
// Restic record, so neither can add to or contradict the report.
func resticRestoreCompletedWithErrors(capture *command.CapturedOutput, runErr error) (int, bool) {
	if capture == nil {
		return 0, false
	}
	followupErr, exited := requestedNativeExit(runErr, 1)
	var outputFailure *command.OutputProcessingFailure
	if !exited || errors.As(followupErr, &outputFailure) {
		return 0, false
	}
	stdout, err := capture.OpenStdout()
	if err != nil {
		return 0, false
	}
	summaries := 0
	stdoutOK := scanRestoreRecords(stdout, func(line string) bool {
		record, ok := resticRestoreRecord(line)
		if !ok {
			return false
		}
		switch record["message_type"] {
		case "status":
			return true
		case "summary":
			summaries++
			return true
		default:
			return false
		}
	})
	_ = stdout.Close()
	if !stdoutOK || summaries != 1 {
		return 0, false
	}
	stderr, err := capture.OpenStderr()
	if err != nil {
		return 0, false
	}
	defer stderr.Close()
	itemErrors, reported, exitRecords := 0, 0, 0
	stderrOK := scanRestoreRecords(stderr, func(line string) bool {
		if !strings.HasPrefix(line, "{") {
			return true
		}
		record, ok := resticRestoreRecord(line)
		if !ok || exitRecords > 0 {
			// Nothing may follow the exit record.
			return false
		}
		switch record["message_type"] {
		case "error":
			if !resticRestoreItemError(record) {
				return false
			}
			itemErrors++
			return true
		case "exit_error":
			exitRecords++
			code, _ := record["code"].(json.Number)
			message, _ := record["message"].(string)
			match := resticRestoreErrorCount.FindStringSubmatch(message)
			if code.String() != "1" || match == nil {
				return false
			}
			count, convErr := strconv.Atoi(match[1])
			if convErr != nil {
				return false
			}
			reported = count
			return true
		default:
			return false
		}
	})
	if !stderrOK || exitRecords != 1 || itemErrors == 0 || reported != itemErrors {
		return 0, false
	}
	return reported, true
}

func resticRestoreRecord(line string) (map[string]any, bool) {
	value, err := decodeUniqueJSON([]byte(line))
	if err != nil {
		return nil, false
	}
	record, ok := value.(map[string]any)
	if !ok {
		return nil, false
	}
	if _, ok := record["message_type"].(string); !ok {
		return nil, false
	}
	return record, true
}

func resticRestoreItemError(record map[string]any) bool {
	nested, _ := record["error"].(map[string]any)
	message, _ := nested["message"].(string)
	item, _ := record["item"].(string)
	during, _ := record["during"].(string)
	return message != "" && item != "" && during != ""
}

var kopiaRestoreSummary = regexp.MustCompile(
	`^Restored [0-9]+ files, [0-9]+ directories and [0-9]+ symbolic links \([^()]+\)` +
		`(?:, skipped [0-9]+ \([^()]+\))?(?:, ignored ([0-9]+) errors)?\.$`)

const kopiaIgnoredErrorPrefix = "ignored error "

// kopiaRestoreOutcome reads a Kopia restore that exited 0. Replicaro passes
// --ignore-errors to every Kopia restore so that one failed file does not stop
// the restore partway (by default Kopia gives up at the first failed file and
// leaves a partly restored destination). The price is that exit code 0 no
// longer proves anything: Kopia has already carried on past any errors, and
// it has no JSON output for restore. The result comes only from its text:
//
//   - success needs exactly one recognised final summary line ("Restored N
//     files, ... (size).") that reports no ignored errors, and no "ignored
//     error ..." line anywhere;
//   - completed with errors when the summary reports ignored errors, or when
//     any "ignored error ..." line appears;
//   - anything else, including a missing or repeated summary, is not
//     recognised and must never be reported as success. A repeated summary
//     stays unrecognised even when "ignored error" lines are present: two
//     summaries mean the output is not the single restore we ran, so its
//     lines are not trusted either.
//
// Kopia writes these lines to stderr; stdout is read too so a line is never
// missed, but it is empty for a real restore. Progress updates share lines
// with "\r", so both "\r" and "\n" end a line here.
func kopiaRestoreOutcome(capture *command.CapturedOutput) (ignored int, recognized bool) {
	if capture == nil {
		return 0, false
	}
	summaries, summaryIgnored, ignoredLines := 0, 0, 0
	for _, open := range []func() (*os.File, error){capture.OpenStdout, capture.OpenStderr} {
		reader, err := open()
		if err != nil {
			return 0, false
		}
		ok := scanRestoreRecords(reader, func(line string) bool {
			if strings.HasPrefix(line, kopiaIgnoredErrorPrefix) {
				ignoredLines++
				return true
			}
			if match := kopiaRestoreSummary.FindStringSubmatch(line); match != nil {
				summaries++
				if match[1] != "" {
					count, convErr := strconv.Atoi(match[1])
					if convErr != nil {
						return false
					}
					summaryIgnored = count
				}
			}
			return true
		})
		_ = reader.Close()
		if !ok {
			return 0, false
		}
	}
	if summaries > 1 {
		return 0, false
	}
	if summaries == 0 {
		// Without a summary, an "ignored error" line is still Kopia's own
		// report that it carried on past a failed item.
		return ignoredLines, ignoredLines > 0
	}
	return max(summaryIgnored, ignoredLines), true
}

// scanRestoreRecords calls visit with every non-empty, space-trimmed line,
// splitting on both "\n" and "\r". A line longer than 8 MiB, a read error, or
// visit returning false ends the scan with false.
func scanRestoreRecords(reader io.Reader, visit func(string) bool) bool {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8*1024*1024)
	scanner.Split(scanRestoreLines)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if !visit(line) {
			return false
		}
	}
	return scanner.Err() == nil
}

func scanRestoreLines(data []byte, atEOF bool) (int, []byte, error) {
	if atEOF && len(data) == 0 {
		return 0, nil, nil
	}
	if index := bytes.IndexAny(data, "\r\n"); index >= 0 {
		return index + 1, data[:index], nil
	}
	if atEOF {
		return len(data), data, nil
	}
	return 0, nil, nil
}
