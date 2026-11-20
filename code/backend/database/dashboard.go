package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/local/replicaro/models"
)

const dashboardIssuesReviewedAtKey = "dashboardIssuesReviewedAt"

var ErrInvalidDashboardIssueCursor = errors.New("invalid dashboard issue cursor")

func GetDashboardStats(db *sql.DB) (models.DashboardStats, error) {
	stats := models.DashboardStats{}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return stats, err
	}
	defer func() { _ = tx.Rollback() }()

	reviewedAt, err := dashboardIssuesReviewedAt(tx)
	if err != nil {
		return stats, err
	}
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM repositories`,
	).Scan(&stats.RepositoryCount); err != nil {
		return stats, err
	}

	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM backup_jobs`,
	).Scan(&stats.JobCount); err != nil {
		return stats, err
	}

	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM backup_jobs WHERE enabled = 1`,
	).Scan(&stats.EnabledJobs); err != nil {
		return stats, err
	}

	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM operations
		 WHERE kind = 'backup' AND job_id != '' AND status = 'success'`,
	).Scan(&stats.SuccessfulRuns); err != nil {
		return stats, err
	}

	// FailedRuns is intentionally the count of all unreviewed system issues,
	// including failed/interrupted/partial restores, checks, maintenance, and WARN/ERROR
	// activity entries. It powers the dashboard's global “Issues” state;
	// do not narrow it to backup targets. Target protection health is calculated
	// separately from each enabled job target's latest status in the frontend.
	// WarningRuns is the subset at the same review cutoff, so severity does not
	// require renaming the existing serialized issue-count field or another read.
	// The cutoff is the same condition the Issues list uses for its "new" marker.
	if err := tx.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(warning), 0) FROM (
			SELECT CASE WHEN status = 'completed_with_issues' THEN 1 ELSE 0 END AS warning FROM operations
			 WHERE status IN ('failed', 'interrupted', 'partial', 'completed_with_issues', 'reconnect_required')
			   AND `+dashboardNewSinceReview(issueOperationTime)+`
			 UNION ALL
			SELECT CASE WHEN level = 'WARN' THEN 1 ELSE 0 END AS warning FROM activity_log
			 WHERE level IN ('ERROR', 'WARN')
			   AND `+dashboardNewSinceReview("timestamp")+`
		)`,
		reviewedAt, reviewedAt, reviewedAt,
		reviewedAt, reviewedAt, reviewedAt,
	).Scan(&stats.FailedRuns, &stats.WarningRuns); err != nil {
		return stats, err
	}
	stuck, err := StuckKopiaPolicies(tx, time.Now())
	if err != nil {
		return stats, err
	}
	for _, policy := range stuck {
		if issueNewSince(policy.StuckAt, reviewedAt) {
			stats.FailedRuns++
		}
	}

	if err := tx.QueryRow(
		`SELECT COALESCE((SELECT operation_steps.finished_at
		 FROM operations
		 JOIN operation_steps
		   ON operation_steps.operation_id = operations.id
		  AND operation_steps.domain = 'native'
		  AND operation_steps.kind = 'backup'
		  AND operation_steps.status = 'succeeded'
		 WHERE operations.kind = 'backup'
		   AND operations.job_id != ''
		   AND operations.status IN ('success', 'completed_with_issues')
		   AND julianday(operation_steps.finished_at) IS NOT NULL
		 ORDER BY julianday(operation_steps.finished_at) DESC, operations.id DESC LIMIT 1), '')`,
	).Scan(&stats.LastBackup); err != nil {
		return stats, err
	}

	if err := tx.Commit(); err != nil {
		return models.DashboardStats{}, err
	}
	return stats, nil
}

// ListDashboardIssues returns issue events independently from the general
// operation history, so newer successful operations cannot displace failures
// from the dashboard's Issues view. Issues whose time is missing or unreadable
// come first (after any stuck Kopia policy), so they are never lost among the
// rest. A log entry's missing (NULL) timestamp or message is returned as an
// empty string.
func ListDashboardIssues(db *sql.DB, limit int, cursor string) (models.DashboardIssues, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 200 {
		limit = 200
	}
	beforeSeconds, beforeNanos, beforeID, err := decodeDashboardCursor(cursor, ErrInvalidDashboardIssueCursor)
	if err != nil {
		return models.DashboardIssues{}, err
	}
	var reviewedAt string
	err = db.QueryRow(
		`SELECT COALESCE((SELECT value FROM settings WHERE key = ?), '')`,
		dashboardIssuesReviewedAtKey,
	).Scan(&reviewedAt)
	if err != nil {
		return models.DashboardIssues{}, err
	}

	// A stuck Kopia policy process is a current condition, not an event, so it
	// leads the first page while it lasts and is gone once the process ends.
	// It is read before the main query, which holds its connection until the
	// rows are closed.
	var current []models.DashboardIssue
	if cursor == "" {
		stuck, err := StuckKopiaPolicies(db, time.Now())
		if err != nil {
			return models.DashboardIssues{}, err
		}
		for _, policy := range stuck {
			current = append(current, models.DashboardIssue{
				ID: "kopia-policy-stuck-" + policy.RepositoryID, Timestamp: formatSortableTimestamp(policy.StuckAt),
				Kind: models.DashboardIssueKopiaPolicyStuck, IsNew: issueNewSince(policy.StuckAt, reviewedAt),
				Severity: "error", Title: policy.Name,
			})
		}
	}

	rows, err := db.Query(`
		WITH issue_rows AS (
			SELECT
				'operation-' || id AS id,
				`+issueOperationTime+` AS timestamp,
				'operation' AS kind,
				CASE WHEN `+dashboardNewSinceReview(issueOperationTime)+` THEN 1 ELSE 0 END AS is_new,
				kind AS operation_kind,
				id AS operation_id,
				status,
				issue_kind,
				'' AS level,
				started_at,
				finished_at,
				title,
				1 AS output_available
			FROM operations
			WHERE status IN ('failed', 'interrupted', 'partial', 'completed_with_issues', 'reconnect_required')

			UNION ALL

			SELECT
				'log-' || id AS id,
				timestamp,
				'log' AS kind,
				CASE WHEN `+dashboardNewSinceReview("timestamp")+` THEN 1 ELSE 0 END AS is_new,
				'' AS operation_kind,
				'' AS operation_id,
				'' AS status,
				'' AS issue_kind,
				level,
				'' AS started_at,
				'' AS finished_at,
				message AS title,
				0 AS output_available
			FROM activity_log
			WHERE level IN ('ERROR', 'WARN')
		),
		ordered_issues AS (
			SELECT issue_rows.*, `+dashboardSortKeyColumns+`
			FROM issue_rows
		)
		SELECT id, timestamp, kind, is_new, operation_kind, operation_id, status, issue_kind, level,
		       started_at, finished_at, title, output_available,
		       sort_seconds, sort_nanos
		FROM ordered_issues
		WHERE (? = ''
			OR sort_seconds < ?
			OR (sort_seconds = ? AND (
				sort_nanos < ? OR (sort_nanos = ? AND id < ?)
			)))
		ORDER BY sort_seconds DESC, sort_nanos DESC, id DESC
		LIMIT ?`, reviewedAt, reviewedAt, reviewedAt,
		reviewedAt, reviewedAt, reviewedAt,
		beforeID, beforeSeconds, beforeSeconds, beforeNanos, beforeNanos, beforeID, limit+1)
	if err != nil {
		return models.DashboardIssues{}, err
	}
	defer rows.Close()

	issues := append(make([]models.DashboardIssue, 0, limit+len(current)), current...)
	firstPageExtra := len(current)
	var lastSortSeconds int64
	var lastSortNanos int
	for rows.Next() {
		if len(issues)-firstPageExtra == limit {
			last := issues[len(issues)-1]
			return models.DashboardIssues{
				Items: issues, HasMore: true,
				NextCursor: encodeDashboardCursor(lastSortSeconds, lastSortNanos, last.ID),
			}, nil
		}

		var issue models.DashboardIssue
		var isNew int
		// A log entry's time or message can be NULL. They are scanned rather
		// than wrapped in COALESCE, which would change the column's declared
		// type and so the form a time is returned in.
		var timestamp, title sql.NullString
		if err := rows.Scan(
			&issue.ID,
			&timestamp,
			&issue.Kind,
			&isNew,
			&issue.OperationKind,
			&issue.OperationID,
			&issue.Status,
			&issue.IssueKind,
			&issue.Level,
			&issue.StartedAt,
			&issue.FinishedAt,
			&title,
			&issue.OutputAvailable,
			&lastSortSeconds,
			&lastSortNanos,
		); err != nil {
			return models.DashboardIssues{}, err
		}
		issue.Timestamp, issue.Title = timestamp.String, title.String
		issue.IsNew = isNew == 1
		issue.Severity = "error"
		if issue.Status == "completed_with_issues" || issue.Level == "WARN" {
			issue.Severity = "warning"
		}
		issues = append(issues, issue)
	}
	if err := rows.Err(); err != nil {
		return models.DashboardIssues{}, err
	}

	return models.DashboardIssues{Items: issues, HasMore: false}, nil
}

// issueNewSince reports whether an issue that started at the given time is
// newer than the last review, the way the queries above compare timestamps.
func issueNewSince(at time.Time, reviewedAt string) bool {
	if reviewedAt == "" {
		return true
	}
	reviewed, err := time.Parse(time.RFC3339Nano, reviewedAt)
	return err != nil || at.After(reviewed)
}

// issueOperationTime is the time an operation's issue is listed and reviewed
// by: when it finished, or when it started if it never recorded an end.
const issueOperationTime = `COALESCE(NULLIF(finished_at, ''), started_at)`

// dashboardTimeKnown is the SQL condition that expr holds a time the dashboard
// can place: one SQLite reads that starts with its four-digit year, as every
// format Replicaro writes does. An empty, NULL or otherwise unreadable value
// (a bare number included) is an unknown time.
func dashboardTimeKnown(expr string) string {
	return `(` + expr + ` GLOB '[0-9][0-9][0-9][0-9]-*' AND unixepoch(` + expr + `) IS NOT NULL)`
}

// dashboardNewSinceReview is the SQL condition that an issue at expr came
// after the last "Mark all as reviewed". It takes the review time three times
// as parameters. Before the first review every issue is new; after it, an
// issue with an unknown time is not, because nothing places it after the
// review, and reviewing again could never clear it otherwise. The Issues list
// and the dashboard's issue count both use it, so they always agree.
func dashboardNewSinceReview(expr string) string {
	return `(? = '' OR (` + dashboardTimeKnown(expr) + ` AND (
		(length(` + expr + `) = 30 AND ` + expr + ` > ?)
		OR (length(` + expr + `) <> 30 AND julianday(` + expr + `) > julianday(?)))))`
}

// unknownDashboardTimeSeconds is the sort_seconds of an unknown time. No real
// time reaches it, so rows with an unknown time lead their feed, ordered among
// themselves by the tie-breaking ID, and a cursor inside that group still
// pages on into the rows with real times.
const unknownDashboardTimeSeconds = "9223372036854775807"

// dashboardSortKeyColumns orders dashboard events by the instant in their
// "timestamp" column, whichever format it was written in: the fixed-width
// sortable form, RFC 3339 with or without fractions or an offset, or SQLite's
// datetime('now'), which is UTC. Whole seconds come from unixepoch and the
// fraction is padded to nanoseconds, so text order never decides.
var dashboardSortKeyColumns = `CASE WHEN ` + dashboardTimeKnown("timestamp") + `
					THEN unixepoch(timestamp) ELSE ` + unknownDashboardTimeSeconds + ` END AS sort_seconds,
				CASE WHEN ` + dashboardTimeKnown("timestamp") + ` AND substr(timestamp, 20, 1) = '.' THEN
					CAST(substr(
						substr(timestamp, 21, CASE
							WHEN substr(timestamp, -1) = 'Z' THEN length(timestamp) - 21
							WHEN substr(timestamp, -6, 1) IN ('+', '-') THEN length(timestamp) - 26
							ELSE length(timestamp) - 20
						END) || '000000000',
						1, 9
					) AS INTEGER)
				ELSE 0 END AS sort_nanos`

// A dashboard cursor is the sort key of the last row on the previous page:
// seconds, nanoseconds and the tie-breaking ID.
func encodeDashboardCursor(seconds int64, nanos int, id string) string {
	value := strconv.FormatInt(seconds, 10) + "\x00" + strconv.Itoa(nanos) + "\x00" + id
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

// decodeDashboardCursor returns invalid for a malformed cursor, so each feed
// reports its own error.
func decodeDashboardCursor(cursor string, invalid error) (int64, int, string, error) {
	if cursor == "" {
		return 0, 0, "", nil
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, 0, "", invalid
	}
	parts := strings.Split(string(data), "\x00")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return 0, 0, "", invalid
	}
	seconds, secondsErr := strconv.ParseInt(parts[0], 10, 64)
	nanos, nanosErr := strconv.Atoi(parts[1])
	if secondsErr != nil || nanosErr != nil || nanos < 0 || nanos > 999999999 {
		return 0, 0, "", invalid
	}
	return seconds, nanos, parts[2], nil
}

func MarkDashboardIssuesReviewed(db *sql.DB, reviewedAt time.Time) error {
	_, err := db.Exec(
		`INSERT INTO settings (key, value)
		 VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value
		 WHERE CASE
			WHEN settings.value = '' THEN 1
			WHEN length(settings.value) = 30 THEN settings.value < excluded.value
			ELSE julianday(settings.value) < julianday(excluded.value)
		 END`,
		dashboardIssuesReviewedAtKey,
		formatSortableTimestamp(reviewedAt),
	)
	if err != nil {
		return err
	}
	dashboardChanged()
	return nil
}

func dashboardIssuesReviewedAt(tx *sql.Tx) (string, error) {
	var reviewedAt string
	err := tx.QueryRow(
		`SELECT value FROM settings WHERE key = ?`,
		dashboardIssuesReviewedAtKey,
	).Scan(&reviewedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return reviewedAt, err
}
