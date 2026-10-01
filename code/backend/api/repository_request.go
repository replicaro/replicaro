package api

import (
	"github.com/local/replicaro/models"
)

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

// RestoreRequest and RestoreSelectionRequest deliberately have no operationId
// field: the server generates the ID and returns it with 202. A client that
// sends one (an old page, for example) is refused as an unknown field rather
// than having it silently ignored and then looking up an operation that never
// existed. Do not add the field back.
type RestoreRequest struct {
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
