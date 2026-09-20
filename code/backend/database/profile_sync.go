package database

import (
	"database/sql"
	"fmt"
	"math"
	"time"
)

type VaultProfileSync struct {
	RepositoryID          string `json:"repositoryId"`
	Revision              int64  `json:"revision"`
	LastError             string `json:"lastError,omitempty"`
	AttemptCount          int    `json:"attemptCount"`
	NextAttemptAt         string `json:"nextAttemptAt,omitempty"`
	UpdatedAt             string `json:"updatedAt"`
	OperationID           string `json:"-"`
	ProfileSHA256         string `json:"-"`
	ProfileJSON           string `json:"-"`
	ExpectedCurrentSHA256 string `json:"-"`
	CreateOnly            bool   `json:"-"`
}

const profileSyncColumns = `repository_id, revision, last_error, attempt_count, next_attempt_at,
	operation_id, profile_sha256, profile_json, expected_current_sha256, create_only, updated_at`

func scanProfileSync(scan func(...any) error) (VaultProfileSync, error) {
	var value VaultProfileSync
	err := scan(&value.RepositoryID, &value.Revision, &value.LastError, &value.AttemptCount,
		&value.NextAttemptAt, &value.OperationID, &value.ProfileSHA256, &value.ProfileJSON,
		&value.ExpectedCurrentSHA256, &value.CreateOnly, &value.UpdatedAt)
	if err != nil {
		return value, err
	}
	if value.ProfileJSON != "" {
		decoded, err := decodeSecret(value.ProfileJSON)
		if err != nil {
			return value, fmt.Errorf("decode pending profile sync: %w", err)
		}
		value.ProfileJSON = string(decoded)
	}
	return value, nil
}

func markVaultProfileDirty(exec interface {
	Exec(string, ...any) (sql.Result, error)
}, repositoryID string) error {
	if repositoryID == "" {
		return nil
	}
	_, err := exec.Exec(`INSERT INTO vault_profile_sync (repository_id, revision, last_error, attempt_count, next_attempt_at, updated_at)
		VALUES (?, 1, '', 0, '', ?)
		ON CONFLICT(repository_id) DO UPDATE SET
			revision = revision + 1, last_error = '', attempt_count = 0, next_attempt_at = '',
			operation_id = '', profile_sha256 = '', profile_json = '', expected_current_sha256 = '',
			create_only = 0, updated_at = excluded.updated_at`,
		repositoryID, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func MarkVaultProfileDirty(db *sql.DB, repositoryID string) error {
	return markVaultProfileDirty(db, repositoryID)
}

func markVaultProfilesDirty(exec interface {
	Exec(string, ...any) (sql.Result, error)
}, repositoryIDs []string) error {
	seen := map[string]bool{}
	for _, id := range repositoryIDs {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if err := markVaultProfileDirty(exec, id); err != nil {
			return err
		}
	}
	return nil
}

func ListPendingVaultProfiles(db *sql.DB) ([]VaultProfileSync, error) {
	rows, err := db.Query(`SELECT `+profileSyncColumns+`
		FROM vault_profile_sync WHERE next_attempt_at = '' OR next_attempt_at <= ? ORDER BY updated_at, repository_id`,
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := []VaultProfileSync{}
	for rows.Next() {
		value, err := scanProfileSync(rows.Scan)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func CompleteVaultProfileSync(db *sql.DB, repositoryID string, revision int64) error {
	_, err := db.Exec(`DELETE FROM vault_profile_sync WHERE repository_id = ? AND revision = ?`, repositoryID, revision)
	return err
}

func FailVaultProfileSync(db *sql.DB, repositoryID string, revision int64, safeError string) error {
	var attempts int
	if err := db.QueryRow(`SELECT attempt_count FROM vault_profile_sync WHERE repository_id = ? AND revision = ?`, repositoryID, revision).Scan(&attempts); err != nil {
		return err
	}
	attempts++
	delayMinutes := math.Min(math.Pow(2, float64(attempts-1)), 30)
	now := time.Now().UTC()
	_, err := db.Exec(`UPDATE vault_profile_sync SET last_error = ?, attempt_count = ?, next_attempt_at = ?, updated_at = ?
		WHERE repository_id = ? AND revision = ?`, safeError,
		attempts, now.Add(time.Duration(delayMinutes)*time.Minute).Format(time.RFC3339Nano),
		now.Format(time.RFC3339Nano), repositoryID, revision)
	return err
}

func VaultProfileSyncState(db *sql.DB, repositoryID string) (VaultProfileSync, error) {
	return scanProfileSync(db.QueryRow(`SELECT `+profileSyncColumns+` FROM vault_profile_sync WHERE repository_id = ?`, repositoryID).Scan)
}

func ListVaultProfileSyncStatuses(db *sql.DB) ([]VaultProfileSync, error) {
	rows, err := db.Query(`SELECT ` + profileSyncColumns + `
		FROM vault_profile_sync ORDER BY updated_at, repository_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []VaultProfileSync{}
	for rows.Next() {
		value, err := scanProfileSync(rows.Scan)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func PrepareVaultProfileSync(db *sql.DB, repositoryID string, revision int64, operationID, profileSHA256, profileJSON, expectedSHA string, createOnly bool) error {
	if profileJSON == "" {
		return fmt.Errorf("profile sync payload is empty")
	}
	protectedProfile := encodeSecret([]byte(profileJSON))
	result, err := db.Exec(`UPDATE vault_profile_sync SET operation_id = ?, profile_sha256 = ?, profile_json = ?,
		expected_current_sha256 = ?, create_only = ?, updated_at = ?
		WHERE repository_id = ? AND revision = ? AND operation_id = ''`, operationID, profileSHA256,
		protectedProfile, expectedSHA, createOnly, time.Now().UTC().Format(time.RFC3339Nano), repositoryID, revision)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func RetryVaultProfileSync(db *sql.DB, repositoryID string) error {
	result, err := db.Exec(`UPDATE vault_profile_sync SET next_attempt_at = '', updated_at = ? WHERE repository_id = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
