package database

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/local/replicaro/models"
)

var ErrVaultPasswordChangeRecoveryRequired = errors.New("vault-password change recovery is required before this vault can be used")

type VaultPasswordChangeOperation struct {
	RepositoryID  string `json:"repositoryId"`
	OperationUUID string `json:"operationUUID"`
	Phase         string `json:"phase"`
	NativeStatus  string `json:"nativeStatus"`
	NativeOutput  string `json:"nativeOutput,omitempty"`
	LastError     string `json:"lastError,omitempty"`
	CreatedAt     string `json:"createdAt"`
	UpdatedAt     string `json:"updatedAt"`
}

const passwordChangeColumns = `repository_id, operation_uuid, phase,
	native_status, native_output, last_error, created_at, updated_at`

func scanVaultPasswordChange(scan func(...any) error) (VaultPasswordChangeOperation, error) {
	var value VaultPasswordChangeOperation
	err := scan(&value.RepositoryID, &value.OperationUUID, &value.Phase,
		&value.NativeStatus, &value.NativeOutput, &value.LastError, &value.CreatedAt, &value.UpdatedAt)
	return value, err
}

func BeginVaultPasswordChange(db *sql.DB, repositoryID, operationUUID, candidate string) (VaultPasswordChangeOperation, error) {
	if strings.TrimSpace(repositoryID) == "" || strings.TrimSpace(operationUUID) == "" {
		return VaultPasswordChangeOperation{}, fmt.Errorf("vault-password change identity is incomplete")
	}
	if err := models.ValidateVaultPassword(candidate); err != nil {
		return VaultPasswordChangeOperation{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return VaultPasswordChangeOperation{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var committed, pending string
	if err := tx.QueryRow(`SELECT passphrase,pending_passphrase FROM repositories WHERE id=?`, repositoryID).Scan(&committed, &pending); err != nil {
		return VaultPasswordChangeOperation{}, err
	}
	decoded, err := decodeSecret(committed)
	if err != nil {
		return VaultPasswordChangeOperation{}, fmt.Errorf("decode committed vault password: %w", err)
	}
	if candidate == string(decoded) {
		return VaultPasswordChangeOperation{}, fmt.Errorf("new vault password must differ from the current password")
	}
	if pending != "" {
		return VaultPasswordChangeOperation{}, ErrVaultPasswordChangeRecoveryRequired
	}
	var conflicts int
	if err := tx.QueryRow(`SELECT
		(SELECT COUNT(*) FROM vault_password_change_operations WHERE repository_id=?) +
		(SELECT COUNT(*) FROM owner_transfer_operations WHERE repository_id=? AND state NOT IN ('completed','failed'))`,
		repositoryID, repositoryID).Scan(&conflicts); err != nil {
		return VaultPasswordChangeOperation{}, err
	}
	if conflicts != 0 {
		return VaultPasswordChangeOperation{}, fmt.Errorf("another vault administration transition is pending")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.Exec(`UPDATE repositories SET pending_passphrase=? WHERE id=? AND pending_passphrase=''`,
		encodeSecret([]byte(candidate)), repositoryID)
	if err != nil {
		return VaultPasswordChangeOperation{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return VaultPasswordChangeOperation{}, ErrVaultPasswordChangeRecoveryRequired
	}
	if _, err := tx.Exec(`INSERT INTO vault_password_change_operations
		(repository_id,operation_uuid,phase,created_at,updated_at)
		VALUES (?,?, 'preparing', ?,?)`, repositoryID, operationUUID, now, now); err != nil {
		return VaultPasswordChangeOperation{}, err
	}
	if err := tx.Commit(); err != nil {
		return VaultPasswordChangeOperation{}, err
	}
	return VaultPasswordChange(db, repositoryID)
}

func VaultPasswordChange(db *sql.DB, repositoryID string) (VaultPasswordChangeOperation, error) {
	return scanVaultPasswordChange(db.QueryRow(`SELECT `+passwordChangeColumns+`
		FROM vault_password_change_operations WHERE repository_id=?`, repositoryID).Scan)
}

func ListVaultPasswordChanges(db *sql.DB) ([]VaultPasswordChangeOperation, error) {
	rows, err := db.Query(`SELECT ` + passwordChangeColumns + ` FROM vault_password_change_operations ORDER BY created_at,repository_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []VaultPasswordChangeOperation
	for rows.Next() {
		value, scanErr := scanVaultPasswordChange(rows.Scan)
		if scanErr != nil {
			return nil, scanErr
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func AdvanceVaultPasswordChange(db *sql.DB, repositoryID, from, to string) error {
	now := time.Now().UTC().Format(time.RFC3339Nano)
	query := `UPDATE vault_password_change_operations SET phase=?,updated_at=?
		WHERE repository_id=? AND phase=?`
	if from == "native_started" && to == "publishing" {
		query += ` AND native_status='succeeded'`
	}
	result, err := db.Exec(query, to, now, repositoryID, from)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("vault-password change phase changed unexpectedly")
	}
	return nil
}

// AdvanceVaultPasswordChangeAfterCandidateVerification records only the
// orchestration fact that the pending credential was verified against the
// exact native repository. It deliberately preserves the requested native
// command's status, output, and error truth.
func AdvanceVaultPasswordChangeAfterCandidateVerification(db *sql.DB, repositoryID, operationUUID string) error {
	result, err := db.Exec(`UPDATE vault_password_change_operations SET phase='publishing',updated_at=?
		WHERE repository_id=? AND operation_uuid=? AND phase='native_started' AND
		EXISTS (SELECT 1 FROM repositories WHERE id=? AND pending_passphrase<>'')`,
		time.Now().UTC().Format(time.RFC3339Nano), repositoryID, operationUUID, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("vault-password candidate-verification state changed unexpectedly")
	}
	return nil
}

func RecordVaultPasswordNativeResult(db *sql.DB, repositoryID, status, output, safeError string) error {
	if status != "not_started" && status != "succeeded" && status != "failed" && status != "interrupted" {
		return fmt.Errorf("invalid native password-change result")
	}
	result, err := db.Exec(`UPDATE vault_password_change_operations SET native_status=?,native_output=?,last_error=?,updated_at=?
		WHERE repository_id=? AND phase='native_started'`, status, output, safeError,
		time.Now().UTC().Format(time.RFC3339Nano), repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func RecordVaultPasswordChangeError(db *sql.DB, repositoryID, safeError string) error {
	_, err := db.Exec(`UPDATE vault_password_change_operations SET last_error=?,updated_at=? WHERE repository_id=?`,
		safeError, time.Now().UTC().Format(time.RFC3339Nano), repositoryID)
	return err
}

func CommitVaultPasswordChange(db *sql.DB, repositoryID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`UPDATE repositories SET passphrase=pending_passphrase,pending_passphrase=''
		WHERE id=? AND pending_passphrase<>'' AND EXISTS (
			SELECT 1 FROM vault_password_change_operations WHERE repository_id=? AND phase='publishing')`, repositoryID, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("vault-password candidate is unavailable for commit")
	}
	result, err = tx.Exec(`UPDATE vault_password_change_operations SET phase='cleanup_pending',last_error='',updated_at=?
		WHERE repository_id=? AND phase='publishing'`, time.Now().UTC().Format(time.RFC3339Nano), repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("vault-password change commit phase changed unexpectedly")
	}
	return tx.Commit()
}

func CompleteVaultPasswordChangeCleanup(db *sql.DB, repositoryID string) error {
	result, err := db.Exec(`DELETE FROM vault_password_change_operations
		WHERE repository_id=? AND phase='cleanup_pending' AND
		EXISTS (SELECT 1 FROM repositories WHERE id=? AND pending_passphrase='')`, repositoryID, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("vault-password cleanup state changed unexpectedly")
	}
	return nil
}

func AbandonPreparedVaultPasswordChange(db *sql.DB, repositoryID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`DELETE FROM vault_password_change_operations WHERE repository_id=? AND phase='preparing'`, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("vault-password change is no longer safely abandonable")
	}
	if _, err := tx.Exec(`UPDATE repositories SET pending_passphrase='' WHERE id=?`, repositoryID); err != nil {
		return err
	}
	return tx.Commit()
}

func AbandonConclusiveUnstartedVaultPasswordChange(db *sql.DB, repositoryID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`DELETE FROM vault_password_change_operations
		WHERE repository_id=? AND phase='native_started' AND native_status='not_started'`, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("native password change is not conclusively unstarted")
	}
	if _, err := tx.Exec(`UPDATE repositories SET pending_passphrase='' WHERE id=?`, repositoryID); err != nil {
		return err
	}
	return tx.Commit()
}

func RequireNoPrecommitVaultPasswordChange(db *sql.DB, repositoryID string) error {
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM vault_password_change_operations
		WHERE repository_id=? AND phase IN ('preparing','native_started','publishing')`, repositoryID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrVaultPasswordChangeRecoveryRequired
	}
	return nil
}
