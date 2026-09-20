package api

import (
	"encoding/json"
	"errors"

	"github.com/local/replicaro/models"
)

var errInvalidOperationIDWire = errors.New("operationId must be a string when supplied")

func requestedOperationID(raw json.RawMessage) (*string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var id *string
	if err := json.Unmarshal(raw, &id); err != nil || id == nil {
		return nil, errInvalidOperationIDWire
	}
	return id, nil
}

type CreateRepositoryRequest struct {
	CreationIntentID     string                    `json:"creationIntentId,omitempty"`
	Engine               string                    `json:"engine"`
	Name                 string                    `json:"name"`
	Connector            string                    `json:"connector"`
	ColdStorage          bool                      `json:"coldStorage"`
	ArchiveWriteClass    string                    `json:"archiveWriteClass,omitempty"`
	Location             string                    `json:"location"`
	Description          string                    `json:"description"`
	Password             string                    `json:"password"`
	PasswordConfirmation string                    `json:"passwordConfirmation"`
	Options              map[string]string         `json:"options"`
	RcloneAuthSessionID  string                    `json:"rcloneAuthSessionId,omitempty"`
	CheckSchedule        string                    `json:"checkSchedule"`
	MaintenanceSchedule  string                    `json:"maintenanceSchedule"`
	ConcurrencyMode      string                    `json:"concurrencyMode"`
	ObjectLock           models.ObjectLockSettings `json:"objectLock"`
}

type RepositoryScheduleRequest struct {
	RepositoryID           string                     `json:"repositoryId"`
	CheckSchedule          string                     `json:"checkSchedule"`
	MaintenanceSchedule    string                     `json:"maintenanceSchedule"`
	ConcurrencyMode        string                     `json:"concurrencyMode"`
	ObjectLock             *models.ObjectLockSettings `json:"objectLock,omitempty"`
	AutoUnlock             *bool                      `json:"autoUnlock,omitempty"`
	ProfilePreferencesOnly bool                       `json:"profilePreferencesOnly,omitempty"`
}

type RestoreRequest struct {
	OperationID      json.RawMessage          `json:"operationId,omitempty"`
	RepositoryID     string                   `json:"repositoryId"`
	SnapshotID       string                   `json:"snapshotId"`
	Content          *RestoreContentReference `json:"content,omitempty"`
	Path             string                   `json:"path"`
	NativeRootID     string                   `json:"nativeRootId,omitempty"`
	TargetPath       string                   `json:"targetPath"`
	ConflictMode     string                   `json:"conflictMode"`
	OriginalLocation bool                     `json:"originalLocation"`
}

type RestoreContentReference struct {
	Kind       string `json:"kind"`
	SnapshotID string `json:"snapshotId,omitempty"`
}

type RestoreSelectionItem struct {
	SnapshotID   string                   `json:"snapshotId"`
	Content      *RestoreContentReference `json:"content,omitempty"`
	Path         string                   `json:"path"`
	NativeRootID string                   `json:"nativeRootId,omitempty"`
}

type RestoreSelectionRequest struct {
	OperationID  json.RawMessage        `json:"operationId,omitempty"`
	RepositoryID string                 `json:"repositoryId"`
	TargetPath   string                 `json:"targetPath"`
	Items        []RestoreSelectionItem `json:"items"`
	ConflictMode string                 `json:"conflictMode"`
}

type RestoreSelectionItemResult struct {
	SnapshotID          string `json:"snapshotId"`
	Path                string `json:"path"`
	DestinationRelative string `json:"destinationRelative,omitempty"`
	Domain              string `json:"domain"`
	Status              string `json:"status"`
	Output              string `json:"output,omitempty"`
	Error               string `json:"error,omitempty"`
	NativeStatus        string `json:"nativeStatus,omitempty"`
	NativeOutput        string `json:"nativeOutput,omitempty"`
	NativeError         string `json:"nativeError,omitempty"`
	OrchestrationStatus string `json:"orchestrationStatus,omitempty"`
	OrchestrationError  string `json:"orchestrationError,omitempty"`
}

type AdHocBackupRequest struct {
	RepositoryID string `json:"repositoryId"`
	Source       string `json:"source"`
}
