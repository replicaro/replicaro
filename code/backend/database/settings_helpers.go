package database

import "database/sql"

func GetSetting(
	db *sql.DB,
	key string,
) (string, error) {

	var value string

	err := db.QueryRow(
		`SELECT value
		FROM settings
		WHERE key = ?`,
		key,
	).Scan(&value)

	if err != nil {
		return "", err
	}

	return value, nil
}
