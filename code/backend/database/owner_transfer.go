package database

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

type OwnerTransferOperation struct {
	OperationUUID, RepositoryID, FromProfileUUID, ToProfileUUID, State string
}

func validIdentityUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func BeginOwnerTransfer(db *sql.DB, operationUUID, repositoryID, fromProfileUUID, toProfileUUID string) (OwnerTransferOperation, error) {
	if !validIdentityUUID(operationUUID) || !validIdentityUUID(repositoryID) ||
		!validIdentityUUID(fromProfileUUID) || !validIdentityUUID(toProfileUUID) ||
		fromProfileUUID == toProfileUUID {
		return OwnerTransferOperation{}, fmt.Errorf("owner transfer identity is invalid")
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := db.Exec(`INSERT INTO owner_transfer_operations
		(operation_uuid,repository_id,from_profile_uuid,to_profile_uuid,state,created_at,updated_at)
		VALUES (?,?,?,?, 'reviewed',?,?)`, operationUUID, repositoryID, fromProfileUUID, toProfileUUID, now, now)
	if err != nil {
		return OwnerTransferOperation{}, err
	}
	return OwnerTransferOperation{OperationUUID: operationUUID, RepositoryID: repositoryID,
		FromProfileUUID: fromProfileUUID, ToProfileUUID: toProfileUUID, State: "reviewed"}, nil
}

func ActiveOwnerTransfer(db *sql.DB, repositoryID string) (OwnerTransferOperation, error) {
	var value OwnerTransferOperation
	err := db.QueryRow(`SELECT operation_uuid,repository_id,from_profile_uuid,to_profile_uuid,state
		FROM owner_transfer_operations WHERE repository_id=? AND state<>'completed' AND state<>'failed'
		ORDER BY created_at DESC LIMIT 1`, repositoryID).Scan(&value.OperationUUID, &value.RepositoryID,
		&value.FromProfileUUID, &value.ToProfileUUID, &value.State)
	return value, err
}

func AdvanceOwnerTransfer(db *sql.DB, operationUUID, fromState, toState string) error {
	valid := map[string]map[string]bool{
		"reviewed":             {"native_owner_applied": true},
		"native_owner_applied": {"root_published": true},
		"root_published":       {"completed": true},
	}
	if !valid[fromState][toState] {
		return fmt.Errorf("owner transfer state transition is invalid")
	}
	result, err := db.Exec(`UPDATE owner_transfer_operations SET state=?,updated_at=?
		WHERE operation_uuid=? AND state=?`, toState, time.Now().UTC().Format(time.RFC3339Nano), operationUUID, fromState)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
