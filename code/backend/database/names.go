package database

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"
)

var (
	ErrRepositoryNameExists = errors.New("vault name already exists")
	ErrJobNameExists        = errors.New("backup job name already exists")
)

const MaxRepositoryNameCodePoints = 50

type nameQueryer interface {
	QueryRow(query string, args ...any) *sql.Row
}

func ValidateRepositoryName(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("vault name is required")
	}
	if utf8.RuneCountInString(name) > MaxRepositoryNameCodePoints {
		return fmt.Errorf("vault name must be %d Unicode code points or fewer", MaxRepositoryNameCodePoints)
	}
	return nil
}

func ensureUniqueRepositoryName(queryer nameQueryer, name, excludeID string) error {
	name = strings.TrimSpace(name)
	if err := ValidateRepositoryName(name); err != nil {
		return err
	}
	var count int
	if err := queryer.QueryRow(`SELECT
		(SELECT COUNT(*) FROM repositories WHERE name = ? COLLATE NOCASE AND id <> ?) +
		(SELECT COUNT(*) FROM repository_creation_intents
			WHERE name = ? COLLATE NOCASE AND id <> ?) +
		(SELECT COUNT(*) FROM repository_connection_name_reservations
			WHERE name = ? COLLATE NOCASE AND repository_id <> ?)`,
		name, excludeID, name, excludeID, name, excludeID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrRepositoryNameExists
	}
	return nil
}

func ensureUniqueJobName(queryer nameQueryer, name, excludeID string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return fmt.Errorf("backup job name is required")
	}
	var count int
	if err := queryer.QueryRow(`SELECT
		(SELECT COUNT(*) FROM backup_jobs WHERE name = ? COLLATE NOCASE AND id <> ?) +
		(SELECT COUNT(*) FROM repository_connection_job_name_reservations
			WHERE name = ? COLLATE NOCASE AND job_id <> ?)`,
		name, excludeID, name, excludeID).Scan(&count); err != nil {
		return err
	}
	if count != 0 {
		return ErrJobNameExists
	}
	return nil
}
