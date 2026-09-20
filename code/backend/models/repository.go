package models

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	ArchiveWriteClassDeepArchive = "DEEP_ARCHIVE"
	ArchiveWriteClassGlacier     = "GLACIER"
)

const ColdStorageIntegrityHelp = "Integrity checks are unavailable for cold storage because of how cold storage works. If you need integrity checks, use a regular hot storage vault."
const ColdStorageArchivedObjectHelp = "Replicaro detected cold storage files in this vault. Connect this vault using Cold Storage [Must Be S3 Compatible] instead of regular S3."

const (
	ObjectLockModeCompliance = "compliance"
	ObjectLockModeGovernance = "governance"
	ObjectLockUnitDays       = "days"
	ObjectLockUnitWeeks      = "weeks"
	ObjectLockUnitMonths     = "months"
	ObjectLockUnitYears      = "years"
)

// ObjectLockSettings is the one portable statement of owner intent shared by
// the local repository row and protected vault root. Enrollment is deliberately
// separate from Paused: provider locks cannot be undone, so pretending that a
// post-creation off switch "disables" enrollment would be both misleading and
// unsafe. Pausing only stops locks and extensions going forward.
type ObjectLockSettings struct {
	Enrolled      bool   `json:"enrolled"`
	Paused        bool   `json:"paused"`
	Mode          string `json:"mode,omitempty"`
	DurationValue int64  `json:"durationValue,omitempty"`
	DurationUnit  string `json:"durationUnit,omitempty"`
}

func ObjectLockEligible(engine, connector string) bool {
	if engine != "kopia" {
		return false
	}
	switch connector {
	case "s3", "azblob", "gcs":
		return true
	default:
		return false
	}
}

func objectLockUnitHours(unit string) (int64, bool) {
	// Kopia accepts a fixed duration, not calendar periods. Keep the UI-friendly
	// units deterministic across machines: a month is 30 days and a year is 365
	// days rather than depending on the creation date or local calendar.
	switch unit {
	case ObjectLockUnitDays:
		return 24, true
	case ObjectLockUnitWeeks:
		return 7 * 24, true
	case ObjectLockUnitMonths:
		return 30 * 24, true
	case ObjectLockUnitYears:
		return 365 * 24, true
	default:
		return 0, false
	}
}

func (settings ObjectLockSettings) DurationHours() (int64, error) {
	unitHours, valid := objectLockUnitHours(settings.DurationUnit)
	if !valid || settings.DurationValue <= 0 || settings.DurationValue > math.MaxInt64/unitHours {
		return 0, fmt.Errorf("object lock duration is invalid")
	}
	hours := settings.DurationValue * unitHours
	// time.Duration is the bound used by pinned Kopia for repository retention.
	// Validate before converting so an oversized UI number cannot wrap into a
	// shorter protection period.
	if hours > int64(math.MaxInt64/int64(time.Hour)) {
		return 0, fmt.Errorf("object lock duration exceeds Kopia's supported range")
	}
	if hours < 48 {
		return 0, fmt.Errorf("object lock duration must be at least 2 days")
	}
	return hours, nil
}

func (settings ObjectLockSettings) NativeRetentionPeriod() (string, error) {
	hours, err := settings.DurationHours()
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(hours, 10) + "h", nil
}

func NormalizeObjectLock(engine, connector string, settings ObjectLockSettings) (ObjectLockSettings, error) {
	if !settings.Enrolled {
		if settings.Paused || settings.Mode != "" || settings.DurationValue != 0 || settings.DurationUnit != "" {
			return ObjectLockSettings{}, fmt.Errorf("object lock settings require enrollment")
		}
		return ObjectLockSettings{}, nil
	}
	if !ObjectLockEligible(engine, connector) {
		return ObjectLockSettings{}, fmt.Errorf("object lock requires Kopia with S3, Azure Blob, or Google Cloud Storage")
	}
	settings.Mode = strings.ToLower(strings.TrimSpace(settings.Mode))
	settings.DurationUnit = strings.ToLower(strings.TrimSpace(settings.DurationUnit))
	if settings.Mode != ObjectLockModeCompliance && settings.Mode != ObjectLockModeGovernance {
		return ObjectLockSettings{}, fmt.Errorf("object lock mode must be compliance or governance")
	}
	if connector != "s3" && settings.Mode != ObjectLockModeCompliance {
		return ObjectLockSettings{}, fmt.Errorf("Azure Blob and Google Cloud Storage object lock require compliance mode")
	}
	if _, err := settings.DurationHours(); err != nil {
		return ObjectLockSettings{}, err
	}
	return settings, nil
}

func ObjectLockMaintenanceInterval(schedule string) (time.Duration, bool) {
	switch schedule {
	case "daily":
		return 24 * time.Hour, true
	case "weekly":
		return 7 * 24 * time.Hour, true
	case "monthly":
		// Monthly scheduling is calendar-based. Use 31 days here so the native
		// full-interval promise remains conservative in every month.
		return 31 * 24 * time.Hour, true
	default:
		return 0, false
	}
}

func ObjectLockMaintenanceEligible(settings ObjectLockSettings, schedule string) bool {
	if !settings.Enrolled {
		return true
	}
	if settings.Paused && schedule == "manual" {
		// Paused turns off new retention and lock extension, so there is no
		// protection deadline for Replicaro's scheduler to satisfy. Resume still
		// has to replace Manual with an eligible automatic interval first.
		return true
	}
	interval, valid := ObjectLockMaintenanceInterval(schedule)
	if !valid {
		return false
	}
	if settings.Paused {
		// A paused vault may also retain any mapped automatic schedule. Its
		// duration margin becomes mandatory again only when protection resumes.
		return true
	}
	hours, err := settings.DurationHours()
	return err == nil && time.Duration(hours)*time.Hour-interval >= 24*time.Hour
}

func LongestEligibleObjectLockMaintenance(settings ObjectLockSettings) (string, error) {
	if !settings.Enrolled || settings.Paused {
		return "monthly", nil
	}
	for _, schedule := range []string{"monthly", "weekly", "daily"} {
		if ObjectLockMaintenanceEligible(settings, schedule) {
			return schedule, nil
		}
	}
	return "", fmt.Errorf("object lock duration does not permit a space reclamation schedule")
}

// ValidateObjectLockTransition enforces the provider guarantees Replicaro can
// truthfully preserve after initial enrollment. Provider locks cannot be
// shortened, and S3 Compliance cannot safely be weakened to Governance.
func ValidateObjectLockTransition(current, next ObjectLockSettings) error {
	if current.Enrolled != next.Enrolled {
		return fmt.Errorf("object lock cannot be enabled or disabled after creation")
	}
	if !current.Enrolled {
		return nil
	}
	changedConfiguration := current.Mode != next.Mode || current.DurationValue != next.DurationValue ||
		current.DurationUnit != next.DurationUnit
	if next.Paused && changedConfiguration {
		return fmt.Errorf("resume object lock before changing its mode or duration")
	}
	currentHours, err := current.DurationHours()
	if err != nil {
		return err
	}
	nextHours, err := next.DurationHours()
	if err != nil {
		return err
	}
	if nextHours < currentHours {
		return fmt.Errorf("object lock duration cannot be reduced after enrollment")
	}
	if current.Mode == ObjectLockModeCompliance && next.Mode == ObjectLockModeGovernance {
		return fmt.Errorf("S3 Compliance object lock cannot be changed to Governance after enrollment")
	}
	return nil
}

type Repository struct {
	ID                           string             `json:"id"`
	Name                         string             `json:"name"`
	Engine                       string             `json:"engine"`
	Connector                    string             `json:"connector"`
	ConnectorLabel               string             `json:"connectorLabel,omitempty"`
	ColdStorage                  bool               `json:"coldStorage"`
	ArchiveWriteClass            string             `json:"archiveWriteClass,omitempty"`
	SFTPPathMode                 string             `json:"sftpPathMode,omitempty"`
	Location                     string             `json:"location"`
	IsNetwork                    bool               `json:"isNetwork,omitempty"`
	CanonicalIdentity            string             `json:"-"`
	StorageIdentityVersion       string             `json:"-"`
	StorageIdentityKey           string             `json:"-"`
	StorageIdentityJSON          string             `json:"-"`
	ResolvedRepositoryPath       string             `json:"resolvedRepositoryPath,omitempty"`
	ResolvedRepositoryObservedAt string             `json:"resolvedRepositoryObservedAt,omitempty"`
	Description                  string             `json:"description"`
	HasPassword                  bool               `json:"hasPassword"`
	HasCredentials               bool               `json:"hasCredentials"`
	VaultSizeBytes               *int64             `json:"vaultSizeBytes"`
	VaultSizeMeasuredAt          string             `json:"vaultSizeMeasuredAt"`
	VaultSizeDirty               bool               `json:"vaultSizeDirty"`
	VaultSizeLastAttemptAt       string             `json:"-"`
	CheckSchedule                string             `json:"checkSchedule"`
	NextCheck                    string             `json:"nextCheck"`
	LastCheck                    string             `json:"lastCheck"`
	LastCheckStatus              string             `json:"lastCheckStatus"`
	MaintenanceSchedule          string             `json:"maintenanceSchedule"`
	NextMaintenance              string             `json:"nextMaintenance"`
	LastMaintenance              string             `json:"lastMaintenance"`
	LastMaintenanceStatus        string             `json:"lastMaintenanceStatus"`
	ConcurrencyMode              string             `json:"concurrencyMode"`
	AutoUnlock                   bool               `json:"autoUnlock"`
	CreatedAt                    time.Time          `json:"createdAt"`
	ProfileUUID                  string             `json:"profile_uuid"`
	AttachmentGeneration         int64              `json:"attachment_generation"`
	NativeRepositoryID           string             `json:"native_repository_id"`
	IsVaultOwner                 bool               `json:"isVaultOwner"`
	ObjectLock                   ObjectLockSettings `json:"objectLock"`

	// Secrets and connector configuration stay server-side.
	Passphrase        string            `json:"-"`
	PendingPassphrase string            `json:"-"`
	ConnectorOptions  map[string]string `json:"-"`
	ClientUUID        string            `json:"-"`
	// NativeMaintenanceOwnerClientUUID is a transient protected-root readback,
	// never local configuration. Reconciliation uses it to reject a native
	// maintenance owner that is merely nonempty but is not the current owner.
	NativeMaintenanceOwnerClientUUID string `json:"-"`
	// RcloneConfigPath is an operation-only override for a still-owned native
	// authorization stage. Attached repositories always resolve by ID.
	RcloneConfigPath string `json:"-"`
}

// RuntimeView selects the local-only cached filesystem alias without changing
// the immutable configured Location exposed by the API or protected profiles.
// Admission code must resolve and freeze the path before constructing a view.
func (r Repository) RuntimeView() Repository {
	if r.Connector == "fs" && strings.TrimSpace(r.ResolvedRepositoryPath) != "" {
		r.Location = r.ResolvedRepositoryPath
	}
	return r
}

// ValidateVaultPassword rejects values that cannot be delivered unchanged to
// every backup engine and the mandatory rclone-encrypted recovery sidecar.
func ValidateVaultPassword(password string) error {
	if password == "" {
		return fmt.Errorf("vault password is required")
	}
	if strings.ContainsAny(password, "\r\n\x00") {
		return fmt.Errorf("vault password cannot contain carriage return, line feed, or NUL characters")
	}
	if password != strings.TrimSpace(password) {
		return fmt.Errorf("vault password cannot begin or end with whitespace")
	}
	return nil
}

// NormalizeColdStorage validates the explicit, non-identity cold-storage
// marker and returns its immutable write class. An omitted class defaults only
// when the marker is explicitly true.
func NormalizeColdStorage(engine, connector string, cold bool, archiveWriteClass string) (string, error) {
	archiveWriteClass = strings.TrimSpace(archiveWriteClass)
	if !cold {
		if archiveWriteClass != "" {
			return "", fmt.Errorf("archive write class requires Cold Storage [Must Be S3 Compatible]")
		}
		return "", nil
	}
	if engine != "restic" || connector != "s3" {
		return "", fmt.Errorf("cold storage requires Restic with S3-compatible storage")
	}
	if archiveWriteClass == "" {
		archiveWriteClass = ArchiveWriteClassGlacier
	}
	if archiveWriteClass != ArchiveWriteClassDeepArchive && archiveWriteClass != ArchiveWriteClassGlacier {
		return "", fmt.Errorf("cold storage archive write class must be DEEP_ARCHIVE or GLACIER")
	}
	return archiveWriteClass, nil
}

type VaultSizeStatus struct {
	VaultSizeBytes      *int64 `json:"vaultSizeBytes"`
	VaultSizeMeasuredAt string `json:"vaultSizeMeasuredAt"`
	VaultSizeDirty      bool   `json:"vaultSizeDirty"`
	Fresh               bool   `json:"fresh"`
	Running             bool   `json:"running"`
	Pending             bool   `json:"pending"`
	Paused              bool   `json:"paused"`
	Failure             string `json:"failure,omitempty"`
}

func ValidEngine(engine string) bool {
	return engine == "restic" || engine == "kopia"
}
