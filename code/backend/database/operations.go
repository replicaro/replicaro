package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/operationlog"
)

var removeOperationLog = operationlog.Remove
var ErrRepositoryTaskActive = errors.New("repository task already has a running operation")
var ErrBackupTriggerChanged = errors.New("backup job was disabled or changed before it could be queued")

// ErrOperationActive is returned by QueueOperation when an operation it must
// not duplicate is already queued or running.
var ErrOperationActive = errors.New("the same operation is already queued or running")

// StartOperation records an operation before its external command begins so
// the dashboard can show work that is still in progress.
func StartOperation(
	db *sql.DB,
	kind, title, jobID, repositoryID string,
	startedAt time.Time,
) (string, error) {
	id := uuid.New().String()
	if _, err := db.Exec(
		`INSERT INTO operations
			(id, kind, status, title, job_id, repository_id, engine, started_at)
			VALUES (?, ?, 'running', ?, ?, ?, COALESCE((SELECT engine FROM repositories WHERE id = ?), ''), ?)`,
		id, kind, title, jobID, repositoryID, repositoryID, formatSortableTimestamp(startedAt),
	); err != nil {
		return "", err
	}
	dashboardChanged()
	return id, nil
}

// QueuedOperation describes a request-started operation (restore, manual
// check or maintenance, snapshot or job deletion, vault password change,
// settings save or removal) whose record is created before its worker waits
// for the vault.
type QueuedOperation struct {
	Kind, Title, JobID, RepositoryID string
	// Exclusive refuses the new record while another queued or running
	// operation has the same kind, repository and job. With MatchTitle the
	// title has to match as well; snapshot deletion uses that because the
	// snapshot ID is only carried in its title.
	Exclusive  bool
	MatchTitle bool
	// VaultChange marks a vault administration job (password change, settings
	// save, removal). Only one of those may be queued or running per vault;
	// another is refused with ErrVaultChangeActive.
	VaultChange bool
}

// QueueOperation creates the record as queued, with a server-generated ID. The
// checks and the insert share one transaction so two requests cannot both
// pass them. Every record for a vault is refused with ErrVaultBeingRemoved
// while that vault's removal is queued or running, so the removal is not
// starved by new work; a second removal gets the same answer.
func QueueOperation(db *sql.DB, operation QueuedOperation, queuedAt time.Time) (string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if removing, err := vaultRemovalPending(tx, operation.RepositoryID); err != nil {
		return "", err
	} else if removing {
		return "", ErrVaultBeingRemoved
	}
	if operation.VaultChange {
		if err := requireNoVaultChange(tx, operation.RepositoryID); err != nil {
			return "", err
		}
	}
	if operation.Exclusive {
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM operations
			WHERE kind = ? AND repository_id = ? AND job_id = ? AND (? = 0 OR title = ?)
			AND status IN ('queued','running')`,
			operation.Kind, operation.RepositoryID, operation.JobID, operation.MatchTitle, operation.Title).Scan(&active); err != nil {
			return "", err
		}
		if active != 0 {
			return "", ErrOperationActive
		}
	}
	id := uuid.New().String()
	if _, err := tx.Exec(`INSERT INTO operations
		(id, kind, status, title, job_id, repository_id, engine, started_at)
		VALUES (?, ?, 'queued', ?, ?, ?, COALESCE((SELECT engine FROM repositories WHERE id = ?), ''), ?)`,
		id, operation.Kind, operation.Title, operation.JobID, operation.RepositoryID,
		operation.RepositoryID, formatSortableTimestamp(queuedAt)); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	dashboardChanged()
	return id, nil
}

// ActivateQueuedOperation moves a queued record to running once its worker
// holds what it waited for. A manual check or maintenance also marks the
// vault's last check or maintenance status running here, not when it was
// queued: until the lock is taken nothing has run, and the startup Restic
// unlock selection reads that column.
func ActivateQueuedOperation(db *sql.DB, operationID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var kind, repositoryID string
	if err := tx.QueryRow(`SELECT kind, repository_id FROM operations WHERE id = ? AND status = 'queued'`,
		operationID).Scan(&kind, &repositoryID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE operations SET status = 'running' WHERE id = ? AND status = 'queued'`, operationID); err != nil {
		return err
	}
	if kind == "check" || kind == "maintenance" {
		column := "last_check_status"
		if kind == "maintenance" {
			column = "last_maintenance_status"
		}
		result, err := tx.Exec(`UPDATE repositories SET `+column+` = 'running' WHERE id = ?`, repositoryID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return sql.ErrNoRows
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	dashboardChanged()
	return nil
}

// RepositoryOperationTitle is the title of a check or maintenance record,
// shared by the scheduler and the manual request so both read the same.
func RepositoryOperationTitle(kind, repositoryName string) string {
	return strings.Title(kind) + ": " + repositoryName
}

// StartRepositoryOperation atomically creates the durable running marker and
// updates repository status before an external care command is admitted.
func StartRepositoryOperation(db *sql.DB, repo models.Repository, kind string, startedAt time.Time) (string, error) {
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	var exists, active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM repositories WHERE id = ?`, repo.ID).Scan(&exists); err != nil {
		return "", err
	}
	if exists != 1 {
		return "", sql.ErrNoRows
	}
	// A scheduled check or maintenance is skipped while the vault's removal is
	// pending, the same way a paused vault is: no record, no issue, and the
	// scheduler tries again on a later tick.
	if removing, err := vaultRemovalPending(tx, repo.ID); err != nil {
		return "", err
	} else if removing {
		return "", ErrVaultBeingRemoved
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE repository_id = ? AND kind = ? AND status IN ('queued','running')`, repo.ID, kind).Scan(&active); err != nil {
		return "", err
	}
	if active != 0 {
		return "", ErrRepositoryTaskActive
	}
	id := uuid.New().String()
	if _, err := tx.Exec(`INSERT INTO operations (id, kind, engine, status, title, repository_id, started_at)
		VALUES (?, ?, ?, 'running', ?, ?, ?)`, id, kind, repo.Engine, RepositoryOperationTitle(kind, repo.Name), repo.ID, formatSortableTimestamp(startedAt)); err != nil {
		return "", err
	}
	column := "last_check_status"
	if kind == "maintenance" {
		column = "last_maintenance_status"
	}
	result, err := tx.Exec(`UPDATE repositories SET `+column+` = 'running' WHERE id = ?`, repo.ID)
	if err != nil {
		return "", err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return "", sql.ErrNoRows
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	dashboardChanged()
	return id, nil
}

// FinishRepositoryOperation atomically records both the operation outcome and
// the repository's next due time.
func FinishRepositoryOperation(db *sql.DB, operationID string, repo models.Repository, kind, status, output string, finishedAt time.Time) error {
	scheduleColumn := "check_schedule"
	if kind == "maintenance" {
		scheduleColumn = "maintenance_schedule"
	} else if kind != "check" {
		return sql.ErrNoRows
	}
	lastColumn, nextColumn, statusColumn := "last_check", "next_check", "last_check_status"
	if kind == "maintenance" {
		lastColumn, nextColumn, statusColumn = "last_maintenance", "next_maintenance", "last_maintenance_status"
	}
	return operationlog.FinalizeOperation(operationID, repo.Engine, kind, status, output, func() error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		result, err := tx.Exec(`UPDATE operations SET status = ?, finished_at = ? WHERE id = ? AND repository_id = ? AND kind = ? AND status = 'running'`,
			status, formatSortableTimestamp(finishedAt), operationID, repo.ID, kind)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return sql.ErrNoRows
		}
		if status == "interrupted" {
			result, err = tx.Exec(`UPDATE repositories SET `+statusColumn+` = ? WHERE id = ?`, status, repo.ID)
		} else {
			var schedule string
			var objectLockJSON string
			if err := tx.QueryRow(`SELECT `+scheduleColumn+`,object_lock_json FROM repositories WHERE id = ?`, repo.ID).
				Scan(&schedule, &objectLockJSON); err != nil {
				return err
			}
			next := NextRunFrom(schedule, finishedAt)
			// completed_with_issues means native maintenance itself succeeded and
			// only a later Replicaro step failed, so the provider locks were
			// renewed. Counting it as done avoids re-running native maintenance an
			// hour later for nothing.
			if kind == "maintenance" && status != "success" && status != "completed_with_issues" {
				var objectLock models.ObjectLockSettings
				if err := json.Unmarshal([]byte(objectLockJSON), &objectLock); err != nil {
					return fmt.Errorf("decode stored object lock settings: %w", err)
				}
				if objectLock.Enrolled && !objectLock.Paused {
					// Do not let terminal bookkeeping overwrite the urgent retry
					// persisted by owner admission or required after native failure.
					next = objectLockMaintenanceRetryAt(finishedAt)
				}
			}
			result, err = tx.Exec(`UPDATE repositories SET `+lastColumn+` = ?, `+nextColumn+` = ?, `+statusColumn+` = ? WHERE id = ?`,
				finishedAt.UTC().Format(time.RFC3339), next, status, repo.ID)
		}
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return sql.ErrNoRows
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		dashboardChanged()
		return nil
	})
}

func ActivateBackupOperation(db *sql.DB, operationID, jobID, repositoryID string, _ time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE operations SET status = 'running'
		WHERE id = ? AND job_id = ? AND repository_id = ? AND status = 'queued'`,
		operationID, jobID, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return sql.ErrNoRows
	}
	targetResult, err := tx.Exec(`UPDATE backup_job_targets SET last_status = 'running' WHERE job_id = ? AND repository_id = ?`, jobID, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := targetResult.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	// Starting the operation is not the same as starting the requested backup. Keep
	// the scheduled occurrence restorable through the checks under the vault lock
	// and any supporting native commands; the runner resolves it only once storage
	// is confirmed unavailable before the native backup, or the requested backup
	// has been reached.
	if err := tx.Commit(); err != nil {
		return err
	}
	dashboardChanged()
	return nil
}

func FinishBackupOperation(db *sql.DB, operationID, jobID, repositoryID, status, output string, finishedAt time.Time) error {
	return FinishBackupOperationWithIssueKind(db, operationID, jobID, repositoryID, status, "", output, finishedAt)
}

// FinishBackupOperationWithIssueKind is FinishBackupOperation with the
// qualifier of a completed_with_issues result (models.OperationIssueLockedFiles
// or ""). It is written in the same transaction as the status, so the two can
// never disagree.
func FinishBackupOperationWithIssueKind(db *sql.DB, operationID, jobID, repositoryID, status, issueKind, output string, finishedAt time.Time) error {
	if issueKind != "" && (issueKind != models.OperationIssueLockedFiles || status != "completed_with_issues") {
		return fmt.Errorf("backup issue kind %q does not apply to status %q", issueKind, status)
	}
	var engine string
	_ = db.QueryRow(`SELECT engine FROM operations WHERE id=?`, operationID).Scan(&engine)
	return operationlog.FinalizeOperation(operationID, engine, "backup", status, output, func() error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		operationResult, err := tx.Exec(`UPDATE operations SET status = ?, issue_kind = ?, finished_at = ?
		WHERE id = ? AND (
			status = 'running' OR (
				status = 'queued' AND NOT EXISTS (
					SELECT 1 FROM scheduled_operation_restorations r
					WHERE r.operation_id=operations.id AND r.restore_state='eligible'
				)
			)
		)`,
			status, issueKind, formatSortableTimestamp(finishedAt), operationID)
		if err != nil {
			return err
		}
		if count, _ := operationResult.RowsAffected(); count != 1 {
			return sql.ErrNoRows
		}
		targetResult, err := tx.Exec(`UPDATE backup_job_targets SET last_status = ?, last_run = ? WHERE job_id = ? AND repository_id = ?`,
			status, finishedAt.Format(time.RFC3339Nano), jobID, repositoryID)
		if err != nil {
			return err
		}
		if count, _ := targetResult.RowsAffected(); count != 1 {
			return sql.ErrNoRows
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		dashboardChanged()
		return nil
	})
}

func FinishOperation(
	db *sql.DB,
	id, status, output string,
	finishedAt time.Time,
) error {
	var engine, kind string
	if err := db.QueryRow(`SELECT engine,kind FROM operations WHERE id=?`, id).Scan(&engine, &kind); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	return operationlog.FinalizeOperation(id, engine, kind, status, output, func() error {
		result, err := db.Exec(
			`UPDATE operations
			 SET status = ?, finished_at = ?
			 WHERE id = ?`,
			status, formatSortableTimestamp(finishedAt), id,
		)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count == 0 {
			return nil
		}
		dashboardChanged()
		return nil
	})
}

const operationColumns = `
	id, kind, status, title, job_id, repository_id,
	engine, started_at, finished_at, issue_kind`

func scanOperation(scan func(dest ...any) error) (models.Operation, error) {
	var operation models.Operation
	err := scan(
		&operation.ID,
		&operation.Kind,
		&operation.Status,
		&operation.Title,
		&operation.JobID,
		&operation.RepositoryID,
		&operation.Engine,
		&operation.StartedAt,
		&operation.FinishedAt,
		&operation.IssueKind,
	)
	return operation, err
}

func ListOperations(db *sql.DB, limit int) ([]models.Operation, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := db.Query(
		`SELECT `+operationColumns+` FROM operations
		 ORDER BY started_at DESC, id DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	operations := []models.Operation{}
	for rows.Next() {
		operation, err := scanOperation(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range operations {
		var err error
		operations[index].Steps, err = ListOperationSteps(db, operations[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return operations, nil
}

// latestVaultOperationOrder ranks operations by start time descending, then
// ID, as ListOperations does, except that a start time the dashboard can't
// place ranks after every readable one. Text order alone would put a value
// such as "garbage" or "zzzz" ahead of every real time and let a damaged row
// hide a vault's real newest operation.
var latestVaultOperationOrder = `CASE WHEN ` + dashboardTimeKnown("started_at") + ` THEN 0 ELSE 1 END,
	started_at DESC, id DESC`

// ListLatestVaultOperations returns the newest operation of each vault, of any
// status, ranked by latestVaultOperationOrder. An empty kind means any kind;
// any other kind returns each vault's newest operation of exactly that kind,
// and none for a kind that never ran. Unlike ListOperations it is not limited
// to the newest records overall, so a vault whose last operation is old is
// still included, and a vault whose start times are all unreadable still has
// one of them listed.
func ListLatestVaultOperations(db *sql.DB, kind string) ([]models.Operation, error) {
	rows, err := db.Query(
		`SELECT `+operationColumns+` FROM (
			SELECT operations.*, ROW_NUMBER() OVER (
				PARTITION BY repository_id ORDER BY `+latestVaultOperationOrder+`
			) AS vault_rank
			FROM operations
			WHERE repository_id <> '' AND (? = '' OR kind = ?)
		)
		WHERE vault_rank = 1
		ORDER BY `+latestVaultOperationOrder,
		kind, kind,
	)
	if err != nil {
		return nil, err
	}
	operations := []models.Operation{}
	for rows.Next() {
		operation, err := scanOperation(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range operations {
		var err error
		operations[index].Steps, err = ListOperationSteps(db, operations[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return operations, nil
}

func GetOperation(db *sql.DB, id string) (models.Operation, error) {
	var operation models.Operation
	err := db.QueryRow(
		`SELECT `+operationColumns+` FROM operations WHERE id = ?`, id,
	).Scan(
		&operation.ID,
		&operation.Kind,
		&operation.Status,
		&operation.Title,
		&operation.JobID,
		&operation.RepositoryID,
		&operation.Engine,
		&operation.StartedAt,
		&operation.FinishedAt,
		&operation.IssueKind,
	)
	if err == nil {
		operation.Steps, err = ListOperationSteps(db, operation.ID)
	}
	return operation, err
}

func ListActiveOperations(db *sql.DB) ([]models.Operation, error) {
	rows, err := db.Query(
		`SELECT ` + operationColumns + ` FROM operations
		 WHERE status IN ('queued', 'running')
		 ORDER BY started_at ASC, id ASC`,
	)
	if err != nil {
		return nil, err
	}
	operations := []models.Operation{}
	for rows.Next() {
		operation, err := scanOperation(rows.Scan)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		operations = append(operations, operation)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range operations {
		var err error
		operations[index].Steps, err = ListOperationSteps(db, operations[index].ID)
		if err != nil {
			return nil, err
		}
	}
	return operations, nil
}

// PruneOperations removes completed operation records older than retention.
func PruneOperations(db *sql.DB, days int) error {
	if days <= 0 {
		return nil
	}
	rows, err := db.Query(
		`SELECT id FROM operations
		 WHERE status != 'running'
		   AND julianday(finished_at) < julianday('now', ?)`,
		"-"+strconv.Itoa(days)+" days",
	)
	if err != nil {
		return err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	stuck, deleted := 0, 0
	var firstStuckID string
	var firstStuckErr error
	// The scheduler prunes on every tick, so the dashboard notice is sent only
	// when a row was actually deleted, not on every call.
	defer func() {
		if deleted > 0 {
			dashboardChanged()
		}
	}()
	for _, id := range ids {
		if err := removeOperationLog(id); err != nil {
			// Skip this one and keep going. A log that can't be deleted (marked
			// read-only on Windows, say) would otherwise stop retention for every
			// older log behind it, on every run. Its row stays, so the log is
			// still listed and the next run tries again; the error is still
			// returned once the rest are pruned.
			if stuck == 0 {
				firstStuckID, firstStuckErr = id, err
			}
			stuck++
			continue
		}
		// Retention deletes only the exact row whose canonical file removal just
		// succeeded. A concurrent cutoff match cannot be swept accidentally.
		if err := deleteExpiredOperation(db, id); err != nil {
			return errors.Join(stuckOperationLogsError(stuck, firstStuckID, firstStuckErr), err)
		}
		deleted++
	}
	return stuckOperationLogsError(stuck, firstStuckID, firstStuckErr)
}

// deleteExpiredOperation deletes one operation row. Its scheduled restoration
// row goes with it (ON DELETE CASCADE), so when that was the last row of a
// scheduled admission group whose backups never started, the group is
// deleted in the same transaction. Left behind, such a group fails the group
// check in reconcileScheduledAdmissionGroupsTx, which Migrate runs on every
// start. Normally a finished operation's restoration is no longer eligible
// and its group is already gone; this covers the one left eligible because
// an earlier database write failed.
func deleteExpiredOperation(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var groupID string
	err = tx.QueryRow(`SELECT group_id FROM scheduled_operation_restorations WHERE operation_id=?`, id).Scan(&groupID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM operations WHERE id=?`, id); err != nil {
		return err
	}
	if groupID != "" {
		if _, err := tx.Exec(deleteEmptyUnstartedAdmissionGroups+` AND id=?`, groupID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// stuckOperationLogsError reports every log pruning had to skip as one short
// line: the count plus the first failure, which stays reachable through
// errors.Is. The scheduler logs this on every one-minute tick, so a line per
// log would let a stuck logs folder (one marked read-only, say) flood the
// size-capped startup log within minutes.
func stuckOperationLogsError(count int, firstID string, firstErr error) error {
	if count == 0 {
		return nil
	}
	return fmt.Errorf("%d expired operation logs could not be removed; first (%s): %w", count, firstID, firstErr)
}
