package database

import (
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The saved "needs reconnect" state of a vault and the two background worker
// clocks that can raise it live in vault_reconnect_state. The rules for when
// the state is set or cleared belong to the vaultreconnect package; this file
// only stores them. The row is deleted with its vault (foreign key cascade)
// and by a successful reconnect (ReconnectRecoveredRepository).

// Background workers that keep a failing-since time. Each maps to fixed
// columns below; the names never reach SQL any other way.
const (
	ReconnectWorkerProfileSync = "profile_sync"
	ReconnectWorkerKopiaPolicy = "kopia_policy"
)

// reconnectWorkerColumns returns the worker's failing-since column and its
// last-failure column.
func reconnectWorkerColumns(worker string) (since, last string, err error) {
	switch worker {
	case ReconnectWorkerProfileSync:
		return "profile_sync_failing_since", "profile_sync_last_failure_at", nil
	case ReconnectWorkerKopiaPolicy:
		return "kopia_policy_failing_since", "kopia_policy_last_failure_at", nil
	}
	return "", "", fmt.Errorf("unknown reconnect worker %q", worker)
}

// MarkVaultReconnectRequired sets the vault's reconnect state and records
// which job kind or worker set it. It reports true only for the call that
// actually changed the state from clear to set, so concurrent setters (a
// backup and a worker failing together) produce one set of messages. An
// already set state keeps its original source: that source's next success is
// what clears it.
func MarkVaultReconnectRequired(db *sql.DB, repositoryID, source string, at time.Time) (bool, error) {
	if repositoryID == "" || source == "" {
		return false, fmt.Errorf("reconnect state needs a vault and a source")
	}
	result, err := db.Exec(`INSERT INTO vault_reconnect_state
		(repository_id, reconnect_required, reconnect_source, reconnect_set_at)
		VALUES (?, 1, ?, ?)
		ON CONFLICT(repository_id) DO UPDATE SET
			reconnect_required = 1,
			reconnect_source = excluded.reconnect_source,
			reconnect_set_at = excluded.reconnect_set_at
		WHERE vault_reconnect_state.reconnect_required = 0`,
		repositoryID, source, at.UTC().Format(time.RFC3339Nano))
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return count == 1, nil
}

// ClearVaultReconnectRequired clears the state only when source is the job
// kind or worker that set it. It reports whether it cleared anything.
func ClearVaultReconnectRequired(db *sql.DB, repositoryID, source string) (bool, error) {
	result, err := db.Exec(`UPDATE vault_reconnect_state
		SET reconnect_required = 0, reconnect_source = '', reconnect_set_at = ''
		WHERE repository_id = ? AND reconnect_required = 1 AND reconnect_source = ?`,
		repositoryID, source)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return count == 1, nil
}

// VaultReconnectRequired reports the saved state of one vault.
func VaultReconnectRequired(db *sql.DB, repositoryID string) (required bool, source string, err error) {
	err = db.QueryRow(`SELECT reconnect_required, reconnect_source FROM vault_reconnect_state
		WHERE repository_id = ?`, repositoryID).Scan(&required, &source)
	if errors.Is(err, sql.ErrNoRows) {
		return false, "", nil
	}
	return required, source, err
}

// RecordVaultWorkerFailure saves one failure of the worker at time at and
// returns the failing-since time it belongs to. The first failure of a run
// starts the clock and later failures keep it, except that a previously saved
// failure earlier than cutoff ends the old run and this failure starts a new
// one at at: the time in between is not treated as failure time (see
// vaultreconnect for how the cutoff is chosen). A saved failing-since time
// with no saved last failure (a row written before the last-failure column
// existed) also starts a new run, since the gap cannot be known.
//
// All times are written in the fixed-width sortable layout so the gap test
// can compare the stored text directly, inside the one upsert. SQLite
// evaluates every SET expression against the row as it was, so the CASE sees
// the previous last failure, and two concurrent calls cannot interleave
// between a read and a write.
func RecordVaultWorkerFailure(db *sql.DB, repositoryID, worker string, at, cutoff time.Time) (time.Time, error) {
	since, last, err := reconnectWorkerColumns(worker)
	if err != nil {
		return time.Time{}, err
	}
	now := formatSortableTimestamp(at)
	if _, err := db.Exec(`INSERT INTO vault_reconnect_state (repository_id, `+since+`, `+last+`) VALUES (?, ?, ?)
		ON CONFLICT(repository_id) DO UPDATE SET
			`+since+` = CASE
				WHEN vault_reconnect_state.`+since+` = '' OR vault_reconnect_state.`+last+` = ''
					OR vault_reconnect_state.`+last+` < ? THEN excluded.`+since+`
				ELSE vault_reconnect_state.`+since+` END,
			`+last+` = excluded.`+last,
		repositoryID, now, now, formatSortableTimestamp(cutoff)); err != nil {
		return time.Time{}, err
	}
	var saved string
	if err := db.QueryRow(`SELECT `+since+` FROM vault_reconnect_state WHERE repository_id = ?`,
		repositoryID).Scan(&saved); err != nil {
		return time.Time{}, err
	}
	parsed, err := time.Parse(time.RFC3339Nano, saved)
	if err != nil {
		return time.Time{}, fmt.Errorf("saved %s failing-since time is invalid: %w", worker, err)
	}
	return parsed, nil
}

// ResetVaultWorkerFailureClock forgets the worker's failing-since and last
// failure times. It is used on the worker's success and when a failure is one
// the clock does not count, so the next counted failure starts a new run.
func ResetVaultWorkerFailureClock(db *sql.DB, repositoryID, worker string) error {
	since, last, err := reconnectWorkerColumns(worker)
	if err != nil {
		return err
	}
	_, err = db.Exec(`UPDATE vault_reconnect_state SET `+since+` = '', `+last+` = '' WHERE repository_id = ?`, repositoryID)
	return err
}

// VaultWorkerFailingSince returns the worker's saved failing-since time, or
// the zero time when the clock is not running.
func VaultWorkerFailingSince(db *sql.DB, repositoryID, worker string) (time.Time, error) {
	column, _, err := reconnectWorkerColumns(worker)
	if err != nil {
		return time.Time{}, err
	}
	var since string
	err = db.QueryRow(`SELECT `+column+` FROM vault_reconnect_state WHERE repository_id = ?`,
		repositoryID).Scan(&since)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && since == "") {
		return time.Time{}, nil
	}
	if err != nil {
		return time.Time{}, err
	}
	return time.Parse(time.RFC3339Nano, since)
}

func clearVaultReconnectStateTx(tx *sql.Tx, repositoryID string) error {
	_, err := tx.Exec(`DELETE FROM vault_reconnect_state WHERE repository_id = ?`, repositoryID)
	return err
}
