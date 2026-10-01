package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/operationlog"
)

var ErrTargetRunActive = errors.New("backup target already has a queued or running operation")
var removeOperationLog = operationlog.Remove
var ErrRepositoryTaskActive = errors.New("repository task already has a running operation")
var ErrBackupTriggerChanged = errors.New("backup job was disabled or changed before it could be queued")

// ErrOperationActive is returned by QueueOperation when an operation it must
// not duplicate is already queued or running.
var ErrOperationActive = errors.New("the same operation is already queued or running")

type BackupOperationRequest struct {
	Title        string
	JobID        string
	RepositoryID string
	Engine       string
}

type BackupTriggerExpectation struct {
	Job        models.BackupJob
	RequireDue bool
}

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
	return tx.Commit()
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
		return tx.Commit()
	})
}

// QueueBackupOperation atomically records visible queued work and marks the
// target queued. The coordinator performs admission only after this succeeds.
func QueueBackupOperation(db *sql.DB, title, jobID, repositoryID string, queuedAt time.Time) (string, error) {
	ids, err := QueueBackupOperations(db, []BackupOperationRequest{{
		Title: title, JobID: jobID, RepositoryID: repositoryID,
	}}, queuedAt, "", false, nil)
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

// QueueBackupOperations records an all-target trigger atomically. Either every
// target becomes visible as queued and the shared schedule advances once, or
// none of the targets are queued.
func QueueBackupOperations(
	db *sql.DB,
	requests []BackupOperationRequest,
	queuedAt time.Time,
	schedule string,
	markTrigger bool,
	expectation *BackupTriggerExpectation,
) ([]string, error) {
	if len(requests) == 0 {
		return nil, ErrInvalidTargets
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if expectation != nil {
		if err := validateBackupTrigger(tx, requests, queuedAt, *expectation); err != nil {
			return nil, err
		}
	}
	ids := make([]string, 0, len(requests))
	seen := make(map[string]bool, len(requests))
	for _, request := range requests {
		key := request.JobID + "\x00" + request.RepositoryID
		if request.JobID == "" || request.RepositoryID == "" || seen[key] {
			return nil, ErrInvalidTargets
		}
		seen[key] = true
		var targetCount, activeCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM backup_job_targets WHERE job_id = ? AND repository_id = ?`, request.JobID, request.RepositoryID).Scan(&targetCount); err != nil {
			return nil, err
		}
		if targetCount != 1 {
			return nil, sql.ErrNoRows
		}
		if err := requireKopiaPolicyReady(tx, request.RepositoryID); err != nil {
			return nil, err
		}
		if removing, err := vaultRemovalPending(tx, request.RepositoryID); err != nil {
			return nil, err
		} else if removing {
			return nil, ErrVaultBeingRemoved
		}
		if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE job_id = ? AND repository_id = ? AND status IN ('queued', 'running')`, request.JobID, request.RepositoryID).Scan(&activeCount); err != nil {
			return nil, err
		}
		if activeCount > 0 {
			return nil, ErrTargetRunActive
		}
		id := uuid.New().String()
		if _, err := tx.Exec(`INSERT INTO operations (id, kind, status, title, job_id, repository_id, engine, started_at)
			VALUES (?, 'backup', 'queued', ?, ?, ?, COALESCE(NULLIF(?, ''), (SELECT engine FROM repositories WHERE id = ?), ''), ?)`, id, request.Title, request.JobID, request.RepositoryID, request.Engine, request.RepositoryID, formatSortableTimestamp(queuedAt)); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE backup_job_targets SET last_status = 'queued' WHERE job_id = ? AND repository_id = ?`, request.JobID, request.RepositoryID); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if markTrigger {
		jobID := requests[0].JobID
		for _, request := range requests[1:] {
			if request.JobID != jobID {
				return nil, ErrInvalidTargets
			}
		}
		if _, err := tx.Exec(`UPDATE backup_jobs SET last_run = ?, next_run = ? WHERE id = ?`,
			queuedAt.Format(time.RFC3339), NextRunFrom(schedule, queuedAt), jobID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

func validateBackupTrigger(tx *sql.Tx, requests []BackupOperationRequest, queuedAt time.Time, expectation BackupTriggerExpectation) error {
	expected := expectation.Job
	current, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM backup_jobs WHERE id = ?`, expected.ID).Scan)
	if err != nil {
		return err
	}
	if current.Name != expected.Name || current.Source != expected.Source || current.Schedule != expected.Schedule ||
		!equivalentSourceBinding(current.SourceStorageVersion, current.SourceStorageKey, current.SourceStorageDescriptorJSON,
			expected.SourceStorageVersion, expected.SourceStorageKey, expected.SourceStorageDescriptorJSON) ||
		current.Enabled != expected.Enabled || current.NextRun != expected.NextRun || current.LastRun != expected.LastRun ||
		!models.RetentionPolicyEqual(current, expected) || current.Excludes != expected.Excludes || current.Tag != expected.Tag ||
		!reflect.DeepEqual(current.EngineSettings, expected.EngineSettings) ||
		!reflect.DeepEqual(current.PortableTargetIDs, expected.PortableTargetIDs) {
		return ErrBackupTriggerChanged
	}
	expectedTargets := make(map[string]string, len(expected.Targets))
	for _, target := range expected.Targets {
		expectedTargets[target.RepositoryID] = target.Engine
	}
	if len(expectedTargets) != len(expected.Targets) || len(requests) != len(expectedTargets) {
		return ErrBackupTriggerChanged
	}
	rows, err := tx.Query(`SELECT t.repository_id, r.engine FROM backup_job_targets t
		JOIN repositories r ON r.id = t.repository_id WHERE t.job_id = ?`, expected.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	currentTargets := 0
	for rows.Next() {
		var repositoryID, engine string
		if err := rows.Scan(&repositoryID, &engine); err != nil {
			return err
		}
		if expectedTargets[repositoryID] != engine {
			return ErrBackupTriggerChanged
		}
		currentTargets++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if currentTargets != len(expectedTargets) {
		return ErrBackupTriggerChanged
	}
	if expectation.RequireDue {
		if !current.Enabled || current.Schedule == "" || current.Schedule == "manual" || current.NextRun == "" {
			return ErrBackupTriggerChanged
		}
		dueAt, err := time.Parse(time.RFC3339, current.NextRun)
		if err != nil || dueAt.After(queuedAt) {
			return ErrBackupTriggerChanged
		}
	}
	return nil
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
	return tx.Commit()
}

func FinishBackupOperation(db *sql.DB, operationID, jobID, repositoryID, status, output string, finishedAt time.Time) error {
	var engine string
	_ = db.QueryRow(`SELECT engine FROM operations WHERE id=?`, operationID).Scan(&engine)
	return operationlog.FinalizeOperation(operationID, engine, "backup", status, output, func() error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		operationResult, err := tx.Exec(`UPDATE operations SET status = ?, finished_at = ?
		WHERE id = ? AND (
			status = 'running' OR (
				status = 'queued' AND NOT EXISTS (
					SELECT 1 FROM scheduled_operation_restorations r
					WHERE r.operation_id=operations.id AND r.restore_state='eligible'
				)
			)
		)`,
			status, formatSortableTimestamp(finishedAt), operationID)
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
		return tx.Commit()
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
		return nil
	})
}

const operationColumns = `
	id, kind, status, title, job_id, repository_id,
	engine, started_at, finished_at`

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
	if err := rows.Close(); err != nil {
		return err
	}
	for _, id := range ids {
		if err := removeOperationLog(id); err != nil {
			return err
		}
		// Retention deletes only the exact row whose canonical file removal just
		// succeeded. A concurrent cutoff match cannot be swept accidentally.
		if _, err := db.Exec(`DELETE FROM operations WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}
