package database

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/local/replicaro/models"
)

const (
	appUpdateDisableAutomaticKey     = "disableAutomaticUpdateChecks"
	appUpdateLastAutomaticAttemptKey = "appUpdateLastAutomaticAttemptAt"
	appUpdateLastResultKey           = "appUpdateLastResult"
	appUpdateLastAvailableVersionKey = "appUpdateLastAvailableVersion"
	appUpdateSkippedVersionKey       = "appUpdateSkippedVersion"
)

type AppUpdateState struct {
	DisableAutomaticChecks bool
	LastAutomaticAttemptAt string
	LastResult             string
	LastAvailableVersion   string
	SkippedVersion         string
}

func scanAppUpdateState(query interface {
	Query(string, ...any) (*sql.Rows, error)
}) (AppUpdateState, error) {
	state := AppUpdateState{}
	rows, err := query.Query(`SELECT key, value FROM settings WHERE key IN (?, ?, ?, ?, ?)`,
		appUpdateDisableAutomaticKey, appUpdateLastAutomaticAttemptKey,
		appUpdateLastResultKey, appUpdateLastAvailableVersionKey, appUpdateSkippedVersionKey)
	if err != nil {
		return state, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return state, err
		}
		switch key {
		case appUpdateDisableAutomaticKey:
			state.DisableAutomaticChecks = value == "true"
		case appUpdateLastAutomaticAttemptKey:
			state.LastAutomaticAttemptAt = value
		case appUpdateLastResultKey:
			state.LastResult = value
		case appUpdateLastAvailableVersionKey:
			state.LastAvailableVersion = value
		case appUpdateSkippedVersionKey:
			state.SkippedVersion = value
		}
	}
	return state, rows.Err()
}

func GetAppUpdateState(db *sql.DB) (AppUpdateState, error) {
	return scanAppUpdateState(db)
}

func putAppUpdateSetting(tx *sql.Tx, key, value string) error {
	_, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func NormalizeAppUpdateState(db *sql.DB, runningVersion string) error {
	running, err := models.ParseSemanticVersion(runningVersion)
	if err != nil {
		return fmt.Errorf("parse running Replicaro version: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := scanAppUpdateState(tx)
	if err != nil {
		return err
	}
	availableValid := false
	if available, parseErr := models.ParseSemanticVersion(state.LastAvailableVersion); parseErr == nil {
		availableValid = models.CompareSemanticVersions(available, running) > 0
	}
	if !availableValid {
		state.LastAvailableVersion = ""
		if state.LastResult == "update_available" {
			state.LastResult = ""
		}
	}
	if state.LastResult != "" && state.LastResult != "up_to_date" &&
		state.LastResult != "update_available" && state.LastResult != "unavailable" {
		state.LastResult = ""
	}
	if skipped, parseErr := models.ParseSemanticVersion(state.SkippedVersion); parseErr != nil ||
		models.CompareSemanticVersions(running, skipped) >= 0 {
		state.SkippedVersion = ""
	}
	for key, value := range map[string]string{
		appUpdateLastResultKey:           state.LastResult,
		appUpdateLastAvailableVersionKey: state.LastAvailableVersion,
		appUpdateSkippedVersionKey:       state.SkippedVersion,
	} {
		if err := putAppUpdateSetting(tx, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func RecordAppUpdateAutomaticAttempt(db *sql.DB, attemptedAt time.Time) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := putAppUpdateSetting(tx, appUpdateLastAutomaticAttemptKey, attemptedAt.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func CompleteAppUpdateCheck(db *sql.DB, result, availableVersion, runningVersion string) error {
	if result != "up_to_date" && result != "update_available" && result != "unavailable" {
		return fmt.Errorf("invalid application update result %q", result)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := scanAppUpdateState(tx)
	if err != nil {
		return err
	}
	if result == "update_available" {
		if _, err := models.ParseSemanticVersion(availableVersion); err != nil {
			return err
		}
		state.LastAvailableVersion = availableVersion
		if skipped, parseErr := models.ParseSemanticVersion(state.SkippedVersion); parseErr != nil {
			state.SkippedVersion = ""
		} else if available, _ := models.ParseSemanticVersion(availableVersion); models.CompareSemanticVersions(available, skipped) > 0 {
			state.SkippedVersion = ""
		}
	} else if result == "up_to_date" {
		state.LastAvailableVersion = ""
		running, parseErr := models.ParseSemanticVersion(runningVersion)
		if parseErr != nil {
			return parseErr
		}
		if skipped, parseErr := models.ParseSemanticVersion(state.SkippedVersion); parseErr != nil ||
			models.CompareSemanticVersions(running, skipped) >= 0 {
			state.SkippedVersion = ""
		}
	}
	if err := putAppUpdateSetting(tx, appUpdateLastResultKey, result); err != nil {
		return err
	}
	if result != "unavailable" {
		if err := putAppUpdateSetting(tx, appUpdateLastAvailableVersionKey, state.LastAvailableVersion); err != nil {
			return err
		}
		if err := putAppUpdateSetting(tx, appUpdateSkippedVersionKey, state.SkippedVersion); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func SkipCurrentAppUpdateVersion(db *sql.DB, displayedVersion, runningVersion string) (AppUpdateState, bool, error) {
	if _, err := models.ParseSemanticVersion(displayedVersion); err != nil {
		return AppUpdateState{}, false, err
	}
	running, err := models.ParseSemanticVersion(runningVersion)
	if err != nil {
		return AppUpdateState{}, false, err
	}
	tx, err := db.Begin()
	if err != nil {
		return AppUpdateState{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := scanAppUpdateState(tx)
	if err != nil {
		return AppUpdateState{}, false, err
	}
	available, parseErr := models.ParseSemanticVersion(state.LastAvailableVersion)
	stored := parseErr == nil && state.LastAvailableVersion == displayedVersion &&
		models.CompareSemanticVersions(available, running) > 0
	if stored {
		state.SkippedVersion = displayedVersion
		if err := putAppUpdateSetting(tx, appUpdateSkippedVersionKey, displayedVersion); err != nil {
			return AppUpdateState{}, false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return AppUpdateState{}, false, err
	}
	return state, stored, nil
}

func appUpdateAutomaticDue(state AppUpdateState, now time.Time) bool {
	if state.DisableAutomaticChecks {
		return false
	}
	last, err := time.Parse(time.RFC3339Nano, state.LastAutomaticAttemptAt)
	if err != nil || last.After(now) {
		return true
	}
	return now.Sub(last) >= 24*time.Hour
}

func AppUpdateAutomaticDue(db *sql.DB, now time.Time) (bool, error) {
	state, err := GetAppUpdateState(db)
	if err != nil {
		return false, err
	}
	return appUpdateAutomaticDue(state, now), nil
}

// AppUpdateAutomaticWait returns whether automatic checks are enabled and how
// long remains before the next attempt is due. A zero wait means it is due now.
func AppUpdateAutomaticWait(db *sql.DB, now time.Time) (time.Duration, bool, error) {
	state, err := GetAppUpdateState(db)
	if err != nil {
		return 0, false, err
	}
	if state.DisableAutomaticChecks {
		return 0, false, nil
	}
	last, err := time.Parse(time.RFC3339Nano, state.LastAutomaticAttemptAt)
	if err != nil || last.After(now) {
		return 0, true, nil
	}
	wait := 24*time.Hour - now.Sub(last)
	if wait < 0 {
		wait = 0
	}
	return wait, true, nil
}
