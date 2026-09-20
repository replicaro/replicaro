package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
)

// RcloneVaultConfigOwnerIDs returns existing repository IDs and exact active
// workflow-owned repository IDs. It derives only from existing durable state.
func RcloneVaultConfigOwnerIDs(db *sql.DB) (map[string]bool, error) {
	result := map[string]bool{}
	rows, err := db.Query(`SELECT id FROM repositories
		UNION SELECT id FROM repository_creation_intents`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		result[id] = true
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	rows, err = db.Query(`SELECT payload_json FROM repository_connection_intents`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			_ = rows.Close()
			return nil, err
		}
		var value struct {
			Repository struct {
				ID string `json:"id"`
			} `json:"repository"`
		}
		if err := json.Unmarshal([]byte(payload), &value); err != nil || value.Repository.ID == "" {
			_ = rows.Close()
			return nil, fmt.Errorf("decode active repository connection config ownership")
		}
		result[value.Repository.ID] = true
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return result, nil
}
