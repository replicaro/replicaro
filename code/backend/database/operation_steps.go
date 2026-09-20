package database

import (
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/operationlog"
)

var ErrOperationStepFinalization = errors.New("operation step finalization failed")

func StartOperationStep(db *sql.DB, operationID, domain, kind string, started time.Time) error {
	if operationID == "" || kind == "" ||
		(domain != "native" && domain != "orchestration" && domain != "application") {
		return fmt.Errorf("operation step is incomplete")
	}
	_, err := db.Exec(`INSERT INTO operation_steps
		(id,operation_id,domain,kind,status,started_at)
		VALUES (?,?,?,?, 'running',?)
		ON CONFLICT(operation_id,kind) DO NOTHING`,
		uuid.NewString(), operationID, domain, kind, started.UTC().Format(time.RFC3339Nano))
	return err
}

// StartMetadataMutationStep atomically records the native step start and
// reserves the repository's next metadata-cache generation. The required side
// stays in the authoritative database while the applied side stays in the
// rebuildable vault cache, so cache loss can never erase evidence that native
// work may have changed the repository. Native work must not launch when this
// transaction fails.
func StartMetadataMutationStep(db *sql.DB, operationID, repositoryID, kind string, started time.Time) (int64, error) {
	if operationID == "" || repositoryID == "" || kind == "" {
		return 0, fmt.Errorf("metadata mutation step is incomplete")
	}
	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`INSERT INTO operation_steps
		(id,operation_id,domain,kind,status,started_at)
		VALUES (?,?, 'native',?, 'running',?)
		ON CONFLICT(operation_id,kind) DO NOTHING`,
		uuid.NewString(), operationID, kind, started.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return 0, err
	}
	if count, countErr := result.RowsAffected(); countErr != nil || count != 1 {
		if countErr != nil {
			return 0, countErr
		}
		return 0, fmt.Errorf("metadata mutation step was already started")
	}
	var generation int64
	if err := tx.QueryRow(`UPDATE repositories
		SET required_generation=required_generation+1
		WHERE id=?
		RETURNING required_generation`, repositoryID).Scan(&generation); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return generation, nil
}

func FinishOperationStep(db *sql.DB, operationID, kind, status, output string, finished time.Time) error {
	if status != "succeeded" && status != "failed" && status != "skipped" && status != "warning" && status != "interrupted" {
		return fmt.Errorf("operation step status is invalid")
	}
	var domain, engine string
	if err := db.QueryRow(`SELECT s.domain,o.engine FROM operation_steps s
		JOIN operations o ON o.id=s.operation_id
		WHERE s.operation_id=? AND s.kind=? AND s.status='running'`, operationID, kind).Scan(&domain, &engine); err != nil {
		return err
	}
	if status == "interrupted" {
		if domain != "native" {
			return fmt.Errorf("interrupted operation step status is valid only for the native domain")
		}
	}
	section := operationlog.Section{Engine: engine, Domain: domain, Kind: kind, Status: status, Body: output}
	return operationlog.FinalizeSection(operationID, section, func() error {
		result, err := db.Exec(`UPDATE operation_steps
			SET status=?,finished_at=?
			WHERE operation_id=? AND kind=? AND status='running'`,
			status, finished.UTC().Format(time.RFC3339Nano), operationID, kind)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func SkipOperationStep(db *sql.DB, operationID, domain, kind, reason string, at time.Time) error {
	if err := StartOperationStep(db, operationID, domain, kind, at); err != nil {
		return err
	}
	if err := FinishOperationStep(db, operationID, kind, "skipped", reason, at); err != nil {
		return fmt.Errorf("%w: %v", ErrOperationStepFinalization, err)
	}
	return nil
}

func ListOperationSteps(db *sql.DB, operationID string) ([]models.OperationStep, error) {
	rows, err := db.Query(`SELECT id,operation_id,domain,kind,status,started_at,finished_at
		FROM operation_steps WHERE operation_id=?`, operationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type timedStep struct {
		step    models.OperationStep
		started time.Time
	}
	var steps []timedStep
	for rows.Next() {
		var value models.OperationStep
		if err := rows.Scan(&value.ID, &value.OperationID, &value.Domain, &value.Kind,
			&value.Status, &value.StartedAt, &value.FinishedAt); err != nil {
			return nil, err
		}
		started, err := time.Parse(time.RFC3339Nano, value.StartedAt)
		if err != nil {
			return nil, errors.New("operation step has invalid start time")
		}
		steps = append(steps, timedStep{step: value, started: started})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// RFC3339Nano omits trailing fractional zeros, so its text order can
	// reverse adjacent instants. Parse existing rows without rewriting them.
	slices.SortFunc(steps, func(a, b timedStep) int {
		if order := a.started.Compare(b.started); order != 0 {
			return order
		}
		return strings.Compare(a.step.ID, b.step.ID)
	})
	result := make([]models.OperationStep, len(steps))
	for index, value := range steps {
		result[index] = value.step
	}
	return result, nil
}
