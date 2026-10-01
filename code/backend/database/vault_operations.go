package database

import (
	"database/sql"
	"errors"
)

// Operation kinds of the vault administration jobs that run on the tracked
// background path: a vault password change (or its retry), a vault settings
// save and a vault removal. Their records carry the vault ID.
const (
	VaultPasswordChangeKind = "vault_password"
	VaultSettingsKind       = "vault_settings"
	VaultRemovalKind        = "remove_vault"
)

var (
	// ErrVaultBeingRemoved refuses new work for a vault while its removal is
	// queued or running. The API shows this text (with the vault_being_removed
	// code the UI translates).
	ErrVaultBeingRemoved = errors.New("This vault is being removed.")
	// ErrVaultChangeActive refuses a second password change, settings save or
	// removal for a vault while one of them is queued or running. The API shows
	// this text (with the vault_change_active code the UI translates).
	ErrVaultChangeActive = errors.New("Another change to this vault is already in progress. Try again when it finishes.")
)

// vaultBeingRemovedReason is the reason code of a manual backup target that
// was not started because its vault's removal is pending. It is the same code
// the API gives the ErrVaultBeingRemoved refusal.
const vaultBeingRemovedReason = "vault_being_removed"

type queryRower interface {
	QueryRow(query string, args ...any) *sql.Row
}

// vaultRemovalPending reports whether a removal of the vault is queued or
// running. This is how a pending removal holds back new work: every place
// that admits work for a vault asks it inside the same transaction that
// records the work, so a removal and new work cannot both pass. The database
// has one write connection, so those transactions run one after the other.
//
// The answer comes from the removal's own operation record, not from memory:
// a removal that was still queued or running when the app stopped is marked
// interrupted at startup, so a restart never leaves a vault held back.
func vaultRemovalPending(q queryRower, repositoryID string) (bool, error) {
	if repositoryID == "" {
		return false, nil
	}
	var pending int
	if err := q.QueryRow(`SELECT COUNT(*) FROM operations
		WHERE repository_id = ? AND kind = ? AND status IN ('queued','running')`,
		repositoryID, VaultRemovalKind).Scan(&pending); err != nil {
		return false, err
	}
	return pending != 0, nil
}

// RequireVaultNotBeingRemoved is the request-time form of the pending-removal
// check for work that has no operation record of its own to queue in the same
// transaction (for example a job deletion's target vaults).
func RequireVaultNotBeingRemoved(db *sql.DB, repositoryID string) error {
	pending, err := vaultRemovalPending(db, repositoryID)
	if err != nil {
		return err
	}
	if pending {
		return ErrVaultBeingRemoved
	}
	return nil
}

// requireNoVaultChange refuses a vault administration job while another one
// (password change, settings save or removal) is queued or running for the
// same vault. Each of them waits for a busy vault, so a second one would
// otherwise queue behind the first and run on whatever the first left behind
// (a settings save on a vault that is being removed, a second password change
// on top of an unfinished one).
func requireNoVaultChange(tx *sql.Tx, repositoryID string) error {
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations
		WHERE repository_id = ? AND kind IN (?, ?, ?) AND status IN ('queued','running')`,
		repositoryID, VaultPasswordChangeKind, VaultSettingsKind, VaultRemovalKind).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return ErrVaultChangeActive
	}
	return nil
}

// otherActiveVaultWorkQuery counts what a vault's removal must wait for (or,
// in deleteRepository, refuse on): every queued or running operation of the
// vault other than the removal itself, plus every queued or running job
// deletion of a job that targets the vault. Job deletion records carry only
// the job ID, never a vault, so without the second part a removal could run
// while a Kopia job deletion is still deleting that job's policy in the vault
// (it holds the vault lock only once it stops waiting) or while any job
// deletion is still rewriting the vault's job list. Its arguments are the
// vault ID twice.
const otherActiveVaultWorkQuery = `SELECT COUNT(*) FROM operations
	WHERE status IN ('queued','running') AND (
		(repository_id = ? AND kind <> '` + VaultRemovalKind + `') OR
		(kind = '` + JobDeletionKind + `' AND job_id IN (SELECT job_id FROM backup_job_targets WHERE repository_id = ?)))`

// RepositoryHasOtherActiveWork reports whether anything other than the
// vault's removal is queued or running for it, job deletions of the vault's
// jobs included. A pending removal polls this before it takes the vault lock:
// taking the lock first would deadlock with queued work that is itself
// waiting for that lock.
func RepositoryHasOtherActiveWork(db *sql.DB, repositoryID string) (bool, error) {
	var active int
	if err := db.QueryRow(otherActiveVaultWorkQuery, repositoryID, repositoryID).Scan(&active); err != nil {
		return false, err
	}
	return active != 0, nil
}

// CheckRepositoryRemovalRequest holds the checks a removal request makes
// before its operation is queued: the vault exists, no password change record
// or pending connection reserves it. Other work for the vault is not a
// refusal here; the removal waits for it. CheckRepositoryDeletionEligibility
// and the delete transaction repeat all of this under the vault lock.
func CheckRepositoryRemovalRequest(db *sql.DB, repositoryID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM repositories WHERE id = ?`, repositoryID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return sql.ErrNoRows
	}
	return requireRepositoryConnectionUnreserved(tx, repositoryID)
}

// RunningVaultPasswordChange is a password change operation that startup
// found still running.
type RunningVaultPasswordChange struct {
	OperationID, RepositoryID string
}

// RunningVaultPasswordChangeOperations returns the password change operations
// still marked running. Startup reconciliation in Migrate leaves these running
// on purpose: startup password recovery finishes each one with the outcome of
// the phase record it resumed.
func RunningVaultPasswordChangeOperations(db *sql.DB) ([]RunningVaultPasswordChange, error) {
	rows, err := db.Query(`SELECT id, repository_id FROM operations
		WHERE kind = ? AND status = 'running' ORDER BY started_at, id`, VaultPasswordChangeKind)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	operations := []RunningVaultPasswordChange{}
	for rows.Next() {
		var operation RunningVaultPasswordChange
		if err := rows.Scan(&operation.OperationID, &operation.RepositoryID); err != nil {
			return nil, err
		}
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}
