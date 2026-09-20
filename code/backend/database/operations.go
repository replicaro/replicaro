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
var ErrInvalidOperationID = errors.New("operation id must be a canonical lowercase UUID v4")
var ErrOperationIDExists = errors.New("operation id already exists")

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
	return StartOperationWithID(db, kind, title, jobID, repositoryID, nil, startedAt)
}

// StartOperationWithID lets a restore submission carry one exact UI-selected
// identity into durable state. The canonical UUID v4 check prevents alternate
// spellings from weakening exact operation lookup, while the database primary
// key remains the final collision boundary.
func StartOperationWithID(
	db *sql.DB,
	kind, title, jobID, repositoryID string,
	requestedID *string,
	startedAt time.Time,
) (string, error) {
	id := ""
	if requestedID == nil {
		id = uuid.New().String()
	} else {
		id = *requestedID
		if err := ValidateOperationID(id); err != nil {
			return "", ErrInvalidOperationID
		}
	}
	result, err := db.Exec(
		`INSERT INTO operations
			(id, kind, status, title, job_id, repository_id, engine, started_at)
			VALUES (?, ?, 'running', ?, ?, ?, COALESCE((SELECT engine FROM repositories WHERE id = ?), ''), ?)
			ON CONFLICT(id) DO NOTHING`,
		id, kind, title, jobID, repositoryID, repositoryID, formatSortableTimestamp(startedAt),
	)
	if err != nil {
		return "", err
	}
	if count, err := result.RowsAffected(); err != nil {
		return "", err
	} else if count != 1 {
		return "", ErrOperationIDExists
	}
	return id, nil
}

func ValidateOperationID(id string) error {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil || parsed.Version() != 4 ||
		parsed.Variant() != uuid.RFC4122 || parsed.String() != id {
		return ErrInvalidOperationID
	}
	return nil
}

func OperationIDExists(db *sql.DB, id string) (bool, error) {
	var exists int
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM operations WHERE id = ?)`, id).Scan(&exists); err != nil {
		return false, err
	}
	return exists != 0, nil
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
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE repository_id = ? AND kind = ? AND status IN ('queued','running')`, repo.ID, kind).Scan(&active); err != nil {
		return "", err
	}
	if active != 0 {
		return "", ErrRepositoryTaskActive
	}
	id := uuid.New().String()
	if _, err := tx.Exec(`INSERT INTO operations (id, kind, engine, status, title, repository_id, started_at)
		VALUES (?, ?, ?, 'running', ?, ?, ?)`, id, kind, repo.Engine, strings.Title(kind)+": "+repo.Name, repo.ID, formatSortableTimestamp(startedAt)); err != nil {
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
			if kind == "maintenance" && status != "success" {
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
		current.SourceStorageVersion != expected.SourceStorageVersion ||
		current.SourceStorageKey != expected.SourceStorageKey ||
		current.SourceStorageDescriptorJSON != expected.SourceStorageDescriptorJSON ||
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
	// Process activation is not requested-backup start. Keep the scheduled
	// occurrence restorable through lock-level admission and supporting native
	// commands; the runner resolves it only after a conclusive pre-native
	// availability result or the requested backup boundary has been reached.
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
