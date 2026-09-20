package database

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type KopiaFilesystemReconnectIntent struct {
	RepositoryID    string
	IntentID        string
	PriorPath       string
	CandidatePath   string
	PriorConfigSHA  string
	StagedConfigSHA string
	State           string
	Error           string
	CreatedAt       string
	UpdatedAt       string
}

const kopiaFilesystemReconnectColumns = `repository_id,intent_id,prior_resolved_path,candidate_path,
	prior_config_sha256,staged_config_sha256,state,error,created_at,updated_at`

func scanKopiaFilesystemReconnect(scan func(...any) error) (KopiaFilesystemReconnectIntent, error) {
	var value KopiaFilesystemReconnectIntent
	err := scan(&value.RepositoryID, &value.IntentID, &value.PriorPath, &value.CandidatePath,
		&value.PriorConfigSHA, &value.StagedConfigSHA, &value.State, &value.Error,
		&value.CreatedAt, &value.UpdatedAt)
	return value, err
}

func ReserveKopiaFilesystemReconnect(db *sql.DB, repositoryID, priorPath, candidatePath, priorConfigSHA string) (KopiaFilesystemReconnectIntent, error) {
	if repositoryID == "" || priorPath == "" || candidatePath == "" || len(priorConfigSHA) != 64 {
		return KopiaFilesystemReconnectIntent{}, fmt.Errorf("Kopia filesystem reconnect intent is incomplete")
	}
	tx, err := db.Begin()
	if err != nil {
		return KopiaFilesystemReconnectIntent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if existing, err := scanKopiaFilesystemReconnect(tx.QueryRow(`SELECT `+kopiaFilesystemReconnectColumns+
		` FROM kopia_filesystem_reconnect_intents WHERE repository_id=?`, repositoryID).Scan); err == nil {
		if existing.PriorPath != priorPath || existing.CandidatePath != candidatePath || existing.PriorConfigSHA != priorConfigSHA {
			return KopiaFilesystemReconnectIntent{}, fmt.Errorf("a different Kopia filesystem reconnect is already pending")
		}
		if err := tx.Commit(); err != nil {
			return KopiaFilesystemReconnectIntent{}, err
		}
		return existing, nil
	} else if err != sql.ErrNoRows {
		return KopiaFilesystemReconnectIntent{}, err
	}
	var engine, connector string
	if err := tx.QueryRow(`SELECT engine,connector FROM repositories WHERE id=?`, repositoryID).Scan(&engine, &connector); err != nil {
		return KopiaFilesystemReconnectIntent{}, err
	}
	if engine != "kopia" || connector == "" {
		return KopiaFilesystemReconnectIntent{}, fmt.Errorf("Kopia reconnect requires a saved Kopia vault")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	value := KopiaFilesystemReconnectIntent{RepositoryID: repositoryID, IntentID: uuid.NewString(),
		PriorPath: priorPath, CandidatePath: candidatePath, PriorConfigSHA: priorConfigSHA,
		State: "prepared", CreatedAt: now, UpdatedAt: now}
	if _, err := tx.Exec(`INSERT INTO kopia_filesystem_reconnect_intents
		(repository_id,intent_id,prior_resolved_path,candidate_path,prior_config_sha256,
		 staged_config_sha256,state,error,created_at,updated_at)
		VALUES (?,?,?,?,?,'','prepared','',?,?)`, repositoryID, value.IntentID, priorPath,
		candidatePath, priorConfigSHA, now, now); err != nil {
		return KopiaFilesystemReconnectIntent{}, err
	}
	if err := tx.Commit(); err != nil {
		return KopiaFilesystemReconnectIntent{}, err
	}
	return value, nil
}

// CommitKopiaConnectionUpdate records that exact staged bytes are active while
// leaving configured address publication to the repository connection's final
// transaction. The filesystem alias workflow continues to use its own commit.
func CommitKopiaConnectionUpdate(db *sql.DB, intent KopiaFilesystemReconnectIntent) error {
	return updateKopiaFilesystemReconnect(db, intent.RepositoryID, intent.IntentID, "activated", "committed", "", "")
}

func SetKopiaFilesystemReconnectStaged(db *sql.DB, repositoryID, intentID, stagedSHA string) error {
	if len(stagedSHA) != 64 {
		return fmt.Errorf("staged Kopia configuration fingerprint is invalid")
	}
	return updateKopiaFilesystemReconnect(db, repositoryID, intentID, "prepared", "prepared", stagedSHA, "")
}

func MarkKopiaFilesystemReconnectActivated(db *sql.DB, repositoryID, intentID string) error {
	return updateKopiaFilesystemReconnect(db, repositoryID, intentID, "prepared", "activated", "", "")
}

func CommitKopiaFilesystemReconnect(db *sql.DB, intent KopiaFilesystemReconnectIntent, observedAt time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var configuredPath string
	if err := tx.QueryRow(`SELECT location FROM repositories WHERE id=? AND engine='kopia' AND connector='fs'`, intent.RepositoryID).Scan(&configuredPath); err != nil {
		return err
	}
	if err := setResolvedRepositoryPath(tx.Exec, intent.RepositoryID, configuredPath, intent.CandidatePath, observedAt); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.Exec(`UPDATE kopia_filesystem_reconnect_intents SET state='committed',error='',updated_at=?
		WHERE repository_id=? AND intent_id=? AND state='activated' AND staged_config_sha256<>''`,
		now, intent.RepositoryID, intent.IntentID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("Kopia filesystem reconnect activation changed before local commit")
	}
	return tx.Commit()
}

func MarkKopiaFilesystemReconnectCleanup(db *sql.DB, repositoryID, intentID string) error {
	return updateKopiaFilesystemReconnect(db, repositoryID, intentID, "committed", "cleanup", "", "")
}

func RecordKopiaFilesystemReconnectError(db *sql.DB, repositoryID, intentID string, reconnectErr error) error {
	message := ""
	if reconnectErr != nil {
		message = reconnectErr.Error()
	}
	result, err := db.Exec(`UPDATE kopia_filesystem_reconnect_intents SET error=?,updated_at=?
		WHERE repository_id=? AND intent_id=?`, message, time.Now().UTC().Format(time.RFC3339Nano), repositoryID, intentID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("Kopia filesystem reconnect intent changed while recording its error")
	}
	return nil
}

func DeleteKopiaFilesystemReconnect(db *sql.DB, repositoryID, intentID string) error {
	result, err := db.Exec(`DELETE FROM kopia_filesystem_reconnect_intents
		WHERE repository_id=? AND intent_id=? AND state='cleanup'`, repositoryID, intentID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("Kopia filesystem reconnect cleanup state changed")
	}
	return nil
}

func updateKopiaFilesystemReconnect(db *sql.DB, repositoryID, intentID, from, to, stagedSHA, message string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	query := `UPDATE kopia_filesystem_reconnect_intents SET state=?,error=?,updated_at=?`
	args := []any{to, message, now}
	if stagedSHA != "" {
		query += `,staged_config_sha256=?`
		args = append(args, stagedSHA)
	}
	query += ` WHERE repository_id=? AND intent_id=? AND state=?`
	args = append(args, repositoryID, intentID, from)
	result, err := db.Exec(query, args...)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("Kopia filesystem reconnect state changed")
	}
	return nil
}

func ListKopiaFilesystemReconnects(db *sql.DB) ([]KopiaFilesystemReconnectIntent, error) {
	rows, err := db.Query(`SELECT ` + kopiaFilesystemReconnectColumns + ` FROM kopia_filesystem_reconnect_intents ORDER BY repository_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []KopiaFilesystemReconnectIntent{}
	for rows.Next() {
		value, err := scanKopiaFilesystemReconnect(rows.Scan)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func FindKopiaFilesystemReconnect(db *sql.DB, repositoryID string) (KopiaFilesystemReconnectIntent, error) {
	return scanKopiaFilesystemReconnect(db.QueryRow(`SELECT `+kopiaFilesystemReconnectColumns+
		` FROM kopia_filesystem_reconnect_intents WHERE repository_id=?`, repositoryID).Scan)
}
