package database

import (
	"database/sql"
	"strconv"
	"time"

	"github.com/local/replicaro/models"
)

// RecentActivity returns the most recent activity-log entries, newest
// first, for display on the dashboard.
func RecentActivity(db *sql.DB) ([]models.ActivityEntry, error) {
	return queryActivity(db, `
		SELECT id, timestamp, level, message
		FROM activity_log
		WHERE level IS NULL OR level <> 'SUPPORT'
		ORDER BY id DESC
		LIMIT 10`)
}

// ListActivity returns the full activity log, newest first.
func ListActivity(db *sql.DB) ([]models.ActivityEntry, error) {
	return queryActivity(db, `
		SELECT id, timestamp, level, message
		FROM activity_log
		WHERE level IS NULL OR level <> 'SUPPORT'
		ORDER BY id DESC`)
}

func queryActivity(db *sql.DB, query string, args ...any) ([]models.ActivityEntry, error) {

	rows, err := db.Query(query, args...)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	entries := []models.ActivityEntry{}

	for rows.Next() {
		var e models.ActivityEntry

		if err := rows.Scan(&e.ID, &e.Timestamp, &e.Level, &e.Message); err != nil {
			return nil, err
		}

		entries = append(entries, e)
	}

	return entries, rows.Err()
}

func LogActivity(
	db *sql.DB,
	message string,
) error {

	_, err := db.Exec(
		`INSERT INTO activity_log (timestamp, level, message) VALUES (?, 'INFO', ?)`,
		formatSortableTimestamp(time.Now()), message,
	)

	return err
}

func LogError(
	db *sql.DB,
	message string,
) error {

	_, err := db.Exec(
		`INSERT INTO activity_log (timestamp, level, message) VALUES (?, 'ERROR', ?)`,
		formatSortableTimestamp(time.Now()), message,
	)
	if err != nil {
		return err
	}
	// An error entry is a dashboard issue.
	dashboardChanged()
	return nil
}

// LogSupport retains bounded diagnostics for export without presenting them as
// ordinary activity or dashboard issues. The failure response remains the
// operation's user-visible result.
func LogSupport(db *sql.DB, message string) error {
	_, err := db.Exec(
		`INSERT INTO activity_log (timestamp, level, message) VALUES (?, 'SUPPORT', ?)`,
		formatSortableTimestamp(time.Now()), message,
	)
	return err
}

func LogWarning(db *sql.DB, message string) error {
	_, err := db.Exec(
		`INSERT INTO activity_log (timestamp, level, message) VALUES (?, 'WARN', ?)`,
		formatSortableTimestamp(time.Now()), message,
	)
	if err != nil {
		return err
	}
	// A warning entry is a dashboard issue.
	dashboardChanged()
	return nil
}

// ClearActivity deletes the whole activity log, its warning and error entries
// included, so the dashboard's Issues count can drop.
func ClearActivity(db *sql.DB) error {
	if _, err := db.Exec(`DELETE FROM activity_log`); err != nil {
		return err
	}
	dashboardChanged()
	return nil
}

// PruneActivity deletes log entries older than the given retention.
func PruneActivity(db *sql.DB, days int) error {

	if days <= 0 {
		return nil
	}

	result, err := db.Exec(
		`DELETE FROM activity_log
		 WHERE timestamp < datetime('now', ?)`,
		"-"+strconv.Itoa(days)+" days",
	)
	if err != nil {
		return err
	}
	// This runs on every scheduler tick; only a deletion can change the
	// dashboard's Issues count.
	if count, _ := result.RowsAffected(); count > 0 {
		dashboardChanged()
	}
	return PruneOperations(db, days)
}
