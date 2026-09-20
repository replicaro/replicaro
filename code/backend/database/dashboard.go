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
	if err := tx.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(warning), 0) FROM (
			SELECT CASE WHEN status = 'completed_with_issues' THEN 1 ELSE 0 END AS warning FROM operations
			 WHERE status IN ('failed', 'interrupted', 'partial', 'completed_with_issues', 'reconnect_required')
			   AND (? = ''
			        OR (length(COALESCE(NULLIF(finished_at, ''), started_at)) = 30
			            AND COALESCE(NULLIF(finished_at, ''), started_at) > ?)
			        OR (length(COALESCE(NULLIF(finished_at, ''), started_at)) <> 30
			            AND julianday(COALESCE(NULLIF(finished_at, ''), started_at)) > julianday(?)))
			 UNION ALL
			SELECT CASE WHEN level = 'WARN' THEN 1 ELSE 0 END AS warning FROM activity_log
			 WHERE level IN ('ERROR', 'WARN')
			   AND (? = ''
			        OR (length(timestamp) = 30 AND timestamp > ?)
			        OR (length(timestamp) <> 30 AND julianday(timestamp) > julianday(?)))
		)`,
		reviewedAt, reviewedAt, reviewedAt,
		reviewedAt, reviewedAt, reviewedAt,
	).Scan(&stats.FailedRuns, &stats.WarningRuns); err != nil {
		return stats, err
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

	if err := tx.QueryRow(
		`SELECT COALESCE(MIN(next_run), '') FROM backup_jobs
		 WHERE enabled = 1 AND next_run != ''
		   AND schedule != 'manual'`,
	).Scan(&stats.NextBackup); err != nil {
		return stats, err
	}

	if err := tx.Commit(); err != nil {
		return models.DashboardStats{}, err
	}
	return stats, nil
}

// ListDashboardIssues returns issue events independently from the general
// operation history, so newer successful operations cannot displace failures
// from the dashboard's Issues view.
func ListDashboardIssues(db *sql.DB, limit int, cursor string) (models.DashboardIssues, error) {
	if limit <= 0 {
		limit = 10
	}
	if limit > 200 {
		limit = 200
	}
	beforeSeconds, beforeNanos, beforeID, err := decodeDashboardIssueCursor(cursor)
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

	rows, err := db.Query(`
		WITH issue_rows AS (
			SELECT
				'operation-' || id AS id,
				COALESCE(NULLIF(finished_at, ''), started_at) AS timestamp,
				'operation' AS kind,
				CASE WHEN ? = ''
					OR (length(COALESCE(NULLIF(finished_at, ''), started_at)) = 30
						AND COALESCE(NULLIF(finished_at, ''), started_at) > ?)
					OR (length(COALESCE(NULLIF(finished_at, ''), started_at)) <> 30
						AND julianday(COALESCE(NULLIF(finished_at, ''), started_at)) > julianday(?))
					THEN 1 ELSE 0 END AS is_new,
				kind AS operation_kind,
				id AS operation_id,
				status,
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
				CASE WHEN ? = ''
					OR (length(timestamp) = 30 AND timestamp > ?)
					OR (length(timestamp) <> 30 AND julianday(timestamp) > julianday(?))
					THEN 1 ELSE 0 END AS is_new,
				'' AS operation_kind,
				'' AS operation_id,
				'' AS status,
				level,
				'' AS started_at,
				'' AS finished_at,
				message AS title,
				0 AS output_available
			FROM activity_log
			WHERE level IN ('ERROR', 'WARN')
		),
		ordered_issues AS (
			SELECT issue_rows.*,
				unixepoch(timestamp) AS sort_seconds,
				CASE WHEN substr(timestamp, 20, 1) = '.' THEN
					CAST(substr(
						substr(timestamp, 21, CASE
							WHEN substr(timestamp, -1) = 'Z' THEN length(timestamp) - 21
							WHEN substr(timestamp, -6, 1) IN ('+', '-') THEN length(timestamp) - 26
							ELSE length(timestamp) - 20
						END) || '000000000',
						1, 9
					) AS INTEGER)
				ELSE 0 END AS sort_nanos
			FROM issue_rows
		)
		SELECT id, timestamp, kind, is_new, operation_kind, operation_id, status, level,
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

	issues := make([]models.DashboardIssue, 0, limit)
	var lastSortSeconds int64
	var lastSortNanos int
	for rows.Next() {
		if len(issues) == limit {
			last := issues[len(issues)-1]
			return models.DashboardIssues{
				Items: issues, HasMore: true,
				NextCursor: encodeDashboardIssueCursor(lastSortSeconds, lastSortNanos, last.ID),
			}, nil
		}

		var issue models.DashboardIssue
		var isNew int
		if err := rows.Scan(
			&issue.ID,
			&issue.Timestamp,
			&issue.Kind,
			&isNew,
			&issue.OperationKind,
			&issue.OperationID,
			&issue.Status,
			&issue.Level,
			&issue.StartedAt,
			&issue.FinishedAt,
			&issue.Title,
			&issue.OutputAvailable,
			&lastSortSeconds,
			&lastSortNanos,
		); err != nil {
			return models.DashboardIssues{}, err
		}
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

func encodeDashboardIssueCursor(seconds int64, nanos int, id string) string {
	value := strconv.FormatInt(seconds, 10) + "\x00" + strconv.Itoa(nanos) + "\x00" + id
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeDashboardIssueCursor(cursor string) (int64, int, string, error) {
	if cursor == "" {
		return 0, 0, "", nil
	}
	data, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return 0, 0, "", ErrInvalidDashboardIssueCursor
	}
	parts := strings.Split(string(data), "\x00")
	if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
		return 0, 0, "", ErrInvalidDashboardIssueCursor
	}
	seconds, secondsErr := strconv.ParseInt(parts[0], 10, 64)
	nanos, nanosErr := strconv.Atoi(parts[1])
	if secondsErr != nil || nanosErr != nil || nanos < 0 || nanos > 999999999 {
		return 0, 0, "", ErrInvalidDashboardIssueCursor
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
	return err
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
