package vaultprofile

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/schedulevalue"
)

const (
	RootFormat                  = "replicaro-vault"
	RootSchemaVersion           = 9
	ProfileFormat               = "replicaro-profile"
	ProfileSchemaVersion        = 3
	MaximumRootDecryptedSize    = 4 << 20
	MaximumProfileDecryptedSize = 16 << 20
)

// Root is the vault-wide protected record at replicaro/vault.replicaro. It
// deliberately contains no jobs or client attachment facts.
type Root struct {
	Format         string                     `json:"format"`
	SchemaVersion  int                        `json:"schemaVersion"`
	Revision       int64                      `json:"revision"`
	VaultUUID      string                     `json:"vault_uuid"`
	Repository     RootRepository             `json:"repository"`
	VaultOwner     VaultOwner                 `json:"vault_owner"`
	Integrity      RootIntegrity              `json:"integrity"`
	Maintenance    RootMaintenance            `json:"maintenance"`
	ObjectLock     *models.ObjectLockSettings `json:"object_lock,omitempty"`
	OwnerTransfer  *OwnerTransfer             `json:"owner_transfer,omitempty"`
	PasswordChange *PasswordChange            `json:"password_change,omitempty"`
	CreatedAt      time.Time                  `json:"createdAt"`
	UpdatedAt      time.Time                  `json:"updatedAt"`
}

type RootRepository struct {
	Engine             string     `json:"engine"`
	NativeRepositoryID string     `json:"native_repository_id"`
	S3Storage          *S3Storage `json:"s3Storage,omitempty"`
}

type S3Storage struct {
	Mode             string `json:"mode"`
	StorageClass     string `json:"storageClass,omitempty"`
	DataStorageClass string `json:"dataStorageClass,omitempty"`
}

type VaultOwner struct {
	ProfileUUID string `json:"profile_uuid"`
}

type RootMaintenance struct {
	Schedule string `json:"schedule"`
}

type RootIntegrity struct {
	Schedule string `json:"schedule"`
}

type OwnerTransfer struct {
	OperationUUID   string `json:"operation_uuid"`
	FromProfileUUID string `json:"from_profile_uuid"`
	ToProfileUUID   string `json:"to_profile_uuid"`
	Phase           string `json:"phase"`
}

// PasswordChange is the complete remote publication fence. Recovery phase,
// credentials, owner facts, time and digests remain installation-local.
type PasswordChange struct {
	OperationUUID string `json:"operation_uuid"`
}

// Profile is the independently published stable attachment and job record at
// profiles/<profile_uuid>/profile.replicaro.
type Profile struct {
	Format           string           `json:"format"`
	SchemaVersion    int              `json:"schemaVersion"`
	Revision         int64            `json:"revision"`
	VaultUUID        string           `json:"vault_uuid"`
	ProfileUUID      string           `json:"profile_uuid"`
	Attachment       Attachment       `json:"attachment"`
	VaultPreferences VaultPreferences `json:"vaultPreferences"`
	Jobs             []BackupJob      `json:"jobs"`
	PasswordChange   *PasswordChange  `json:"password_change,omitempty"`
	CreatedAt        time.Time        `json:"createdAt"`
	UpdatedAt        time.Time        `json:"updatedAt"`
}

type Attachment struct {
	ClientUUID string            `json:"client_uuid"`
	Generation int64             `json:"generation"`
	AttachedAt time.Time         `json:"attachedAt"`
	Display    AttachmentDisplay `json:"display"`
}

type AttachmentDisplay struct {
	ComputerName    string `json:"computerName"`
	OperatingSystem string `json:"operatingSystem"`
}

type VaultPreferences struct {
	Name            string `json:"name"`
	Description     string `json:"description"`
	ConcurrencyMode string `json:"concurrencyMode"`
}

type BackupJob struct {
	JobUUID                 string                `json:"job_uuid"`
	Name                    string                `json:"name"`
	Source                  string                `json:"source"`
	Schedule                string                `json:"schedule"`
	Retention               int                   `json:"retention"`
	RetentionHourly         *int                  `json:"retentionHourly,omitempty"`
	RetentionDaily          *int                  `json:"retentionDaily,omitempty"`
	RetentionWeekly         *int                  `json:"retentionWeekly,omitempty"`
	RetentionMonthly        *int                  `json:"retentionMonthly,omitempty"`
	RetentionYearly         *int                  `json:"retentionYearly,omitempty"`
	Exclusions              string                `json:"exclusions"`
	Tag                     string                `json:"tag"`
	BeforeScriptPath        string                `json:"beforeScriptPath,omitempty"`
	BeforeScriptMustSucceed bool                  `json:"beforeScriptMustSucceed,omitempty"`
	AfterScriptPath         string                `json:"afterScriptPath,omitempty"`
	AfterScriptMustSucceed  bool                  `json:"afterScriptMustSucceed,omitempty"`
	EngineSettings          models.EngineSettings `json:"engineSettings"`
	Enabled                 bool                  `json:"enabled"`
	TargetVaultUUIDs        []string              `json:"target_vault_uuids"`
}

func exactUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func normalizedTimes(created, previous, now time.Time) (time.Time, time.Time) {
	if created.IsZero() {
		created = now
	}
	if now.Before(created) {
		now = created
	}
	if now.Before(previous) {
		now = previous
	}
	return created.UTC(), now.UTC()
}

func BuildRoot(repo models.Repository, nativeRepositoryID, ownerProfileUUID string, revision int64, previousUpdatedAt, now time.Time) (Root, error) {
	created, updated := normalizedTimes(repo.CreatedAt, previousUpdatedAt, now)
	root := Root{
		Format: RootFormat, SchemaVersion: RootSchemaVersion, Revision: revision,
		VaultUUID:   repo.ID,
		Repository:  RootRepository{Engine: repo.Engine, NativeRepositoryID: nativeRepositoryID},
		VaultOwner:  VaultOwner{ProfileUUID: ownerProfileUUID},
		Integrity:   RootIntegrity{Schedule: repo.CheckSchedule},
		Maintenance: RootMaintenance{Schedule: repo.MaintenanceSchedule},
		CreatedAt:   created, UpdatedAt: updated,
	}
	if repo.ObjectLock.Enrolled {
		settings := repo.ObjectLock
		root.ObjectLock = &settings
	}
	if repo.Connector == "s3" {
		if repo.ColdStorage {
			root.Repository.S3Storage = &S3Storage{Mode: "cold", DataStorageClass: repo.ArchiveWriteClass}
		} else if storageClass := strings.TrimSpace(repo.ConnectorOptions["storage_class"]); storageClass != "" {
			root.Repository.S3Storage = &S3Storage{Mode: "ordinary", StorageClass: storageClass}
		}
	}
	return root, root.Validate(repo.Connector)
}

func BuildProfile(repo models.Repository, jobs []models.BackupJob, dormant []BackupJob, profileUUID, clientUUID string, generation, revision int64, display AttachmentDisplay, previousUpdatedAt, now time.Time) (Profile, error) {
	concurrencyMode, err := models.NormalizeConcurrencyMode(repo.ConcurrencyMode)
	if err != nil {
		return Profile{}, err
	}
	created, updated := normalizedTimes(repo.CreatedAt, previousUpdatedAt, now)
	profile := Profile{
		Format: ProfileFormat, SchemaVersion: ProfileSchemaVersion, Revision: revision,
		VaultUUID: repo.ID, ProfileUUID: profileUUID,
		Attachment:       Attachment{ClientUUID: clientUUID, Generation: generation, AttachedAt: updated, Display: display},
		VaultPreferences: VaultPreferences{Name: repo.Name, Description: repo.Description, ConcurrencyMode: concurrencyMode},
		Jobs:             []BackupJob{}, CreatedAt: created, UpdatedAt: updated,
	}
	for _, job := range jobs {
		represented := map[string]bool{}
		targets := append([]string(nil), job.PortableTargetIDs...)
		for _, target := range job.Targets {
			represented[target.Engine] = true
			if len(job.PortableTargetIDs) == 0 {
				targets = append(targets, target.RepositoryID)
			}
		}
		for engineID := range job.EngineSettings {
			represented[engineID] = true
		}
		if err := engines.ValidateJobSettings(job.EngineSettings, represented); err != nil {
			return Profile{}, fmt.Errorf("job %q engine settings: %w", job.Name, err)
		}
		profile.Jobs = append(profile.Jobs, BackupJob{
			JobUUID: job.ID, Name: job.Name, Source: job.Source, Schedule: job.Schedule,
			Retention: job.Retention, RetentionHourly: job.RetentionHourly, RetentionDaily: job.RetentionDaily,
			RetentionWeekly: job.RetentionWeekly, RetentionMonthly: job.RetentionMonthly, RetentionYearly: job.RetentionYearly,
			Exclusions: job.Excludes, Tag: job.Tag, BeforeScriptPath: job.BeforeScriptPath,
			BeforeScriptMustSucceed: job.BeforeScriptMustSucceed, AfterScriptPath: job.AfterScriptPath,
			AfterScriptMustSucceed: job.AfterScriptMustSucceed, EngineSettings: job.EngineSettings,
			Enabled: job.Enabled, TargetVaultUUIDs: targets,
		})
	}
	active := map[string]bool{}
	for _, job := range profile.Jobs {
		active[job.JobUUID] = true
	}
	for _, job := range dormant {
		if !active[job.JobUUID] {
			profile.Jobs = append(profile.Jobs, job)
		}
	}
	return profile, profile.Validate()
}

func (r Root) Validate(connector string) error {
	if r.Format != RootFormat || r.SchemaVersion != RootSchemaVersion || r.Revision < 1 || !exactUUID(r.VaultUUID) ||
		!models.ValidEngine(r.Repository.Engine) || strings.TrimSpace(r.Repository.NativeRepositoryID) == "" ||
		!exactUUID(r.VaultOwner.ProfileUUID) || !schedulevalue.Valid(r.Integrity.Schedule) ||
		!schedulevalue.Valid(r.Maintenance.Schedule) ||
		r.CreatedAt.IsZero() || r.UpdatedAt.IsZero() || r.UpdatedAt.Before(r.CreatedAt) {
		return fmt.Errorf("vault root record is invalid")
	}
	if r.ObjectLock != nil {
		normalized, err := models.NormalizeObjectLock(r.Repository.Engine, connector, *r.ObjectLock)
		if err != nil || !normalized.Enrolled || normalized != *r.ObjectLock ||
			!models.ObjectLockMaintenanceEligible(normalized, r.Maintenance.Schedule) {
			return fmt.Errorf("vault root object lock settings are invalid")
		}
	}
	if r.OwnerTransfer != nil {
		if !exactUUID(r.OwnerTransfer.OperationUUID) || !exactUUID(r.OwnerTransfer.FromProfileUUID) ||
			!exactUUID(r.OwnerTransfer.ToProfileUUID) || r.OwnerTransfer.FromProfileUUID == r.OwnerTransfer.ToProfileUUID ||
			r.VaultOwner.ProfileUUID != r.OwnerTransfer.FromProfileUUID ||
			(r.OwnerTransfer.Phase != "reviewed" && r.OwnerTransfer.Phase != "native_owner_applied") {
			return fmt.Errorf("vault root owner transfer is invalid")
		}
	}
	if r.PasswordChange != nil && !exactUUID(r.PasswordChange.OperationUUID) {
		return fmt.Errorf("vault root password change fence is invalid")
	}
	if r.OwnerTransfer != nil && r.PasswordChange != nil {
		return fmt.Errorf("vault root cannot contain concurrent owner and password changes")
	}
	s3 := r.Repository.S3Storage
	if connector != "s3" {
		if s3 != nil {
			return fmt.Errorf("non-S3 vault root cannot declare s3Storage")
		}
		return nil
	}
	if s3 == nil {
		return nil
	}
	switch s3.Mode {
	case "ordinary":
		if strings.TrimSpace(s3.StorageClass) == "" || s3.DataStorageClass != "" {
			return fmt.Errorf("ordinary S3 vault root has invalid storage class")
		}
	case "cold":
		if r.Repository.Engine != engines.ResticID || s3.StorageClass != "" ||
			(s3.DataStorageClass != models.ArchiveWriteClassGlacier && s3.DataStorageClass != models.ArchiveWriteClassDeepArchive) {
			return fmt.Errorf("cold S3 vault root has invalid storage class")
		}
		if r.Integrity.Schedule != "manual" {
			return fmt.Errorf("cold S3 vault root cannot schedule full integrity checks")
		}
	default:
		return fmt.Errorf("vault root has invalid s3Storage mode")
	}
	return nil
}

func (p Profile) Validate() error {
	if p.Format != ProfileFormat || p.SchemaVersion != ProfileSchemaVersion || p.Revision < 1 ||
		!exactUUID(p.VaultUUID) || !exactUUID(p.ProfileUUID) || !exactUUID(p.Attachment.ClientUUID) ||
		p.Attachment.Generation < 1 || p.Attachment.AttachedAt.IsZero() ||
		p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() || p.UpdatedAt.Before(p.CreatedAt) ||
		strings.TrimSpace(p.VaultPreferences.Name) == "" || !models.ValidConcurrencyMode(p.VaultPreferences.ConcurrencyMode) {
		return fmt.Errorf("vault profile record is invalid")
	}
	if p.PasswordChange != nil && !exactUUID(p.PasswordChange.OperationUUID) {
		return fmt.Errorf("vault profile password change fence is invalid")
	}
	seen := map[string]bool{}
	for _, job := range p.Jobs {
		if !exactUUID(job.JobUUID) || seen[job.JobUUID] {
			return fmt.Errorf("vault profile has an invalid or duplicate job_uuid")
		}
		seen[job.JobUUID] = true
		normalizedSchedule, validSchedule := schedulevalue.NormalizeJob(job.Schedule)
		// Keep native historical source spelling across profile recovery;
		// configured-path eligibility is checked separately at local binding.
		if strings.TrimSpace(job.Name) == "" || job.Source == "" ||
			!validSchedule || normalizedSchedule != job.Schedule {
			return fmt.Errorf("vault profile job %q has invalid settings", job.JobUUID)
		}
		definition := models.BackupJob{Retention: job.Retention, RetentionHourly: job.RetentionHourly,
			RetentionDaily: job.RetentionDaily, RetentionWeekly: job.RetentionWeekly,
			RetentionMonthly: job.RetentionMonthly, RetentionYearly: job.RetentionYearly}
		if err := models.ValidateRetentionPolicy(definition); err != nil {
			return fmt.Errorf("vault profile job %q has invalid retention: %w", job.JobUUID, err)
		}
		if models.ContainsReservedOwnershipTag(job.Tag) {
			return fmt.Errorf("vault profile job %q uses a reserved Replicaro tag namespace", job.JobUUID)
		}
		if (job.BeforeScriptMustSucceed && job.BeforeScriptPath == "") || (job.AfterScriptMustSucceed && job.AfterScriptPath == "") {
			return fmt.Errorf("vault profile job %q requires an absent script", job.JobUUID)
		}
		if len(job.TargetVaultUUIDs) == 0 {
			return fmt.Errorf("vault profile job %q has no targets", job.JobUUID)
		}
		targets := map[string]bool{}
		containsVault := false
		for _, target := range job.TargetVaultUUIDs {
			if !exactUUID(target) || targets[target] {
				return fmt.Errorf("vault profile job %q has an invalid or duplicate target_vault_uuid", job.JobUUID)
			}
			targets[target] = true
			containsVault = containsVault || target == p.VaultUUID
		}
		if !containsVault {
			return fmt.Errorf("vault profile job %q does not target this vault", job.JobUUID)
		}
		represented := map[string]bool{}
		for engineID := range job.EngineSettings {
			represented[engineID] = true
		}
		if err := engines.ValidateJobSettings(job.EngineSettings, represented); err != nil {
			return fmt.Errorf("vault profile job %q engine settings: %w", job.JobUUID, err)
		}
	}
	return nil
}

func marshalBounded(value any, validate func() error, limit int) ([]byte, error) {
	if err := validate(); err != nil {
		return nil, err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	if len(data)+1 > limit {
		return nil, fmt.Errorf("protected recovery record exceeds the size limit")
	}
	return append(data, '\n'), nil
}

func MarshalRoot(root Root, connector string) ([]byte, error) {
	return marshalBounded(root, func() error { return root.Validate(connector) }, MaximumRootDecryptedSize)
}

func Marshal(profile Profile) ([]byte, error) {
	return marshalBounded(profile, profile.Validate, MaximumProfileDecryptedSize)
}

type boundedProfileSize struct {
	limit int
	size  int
}

func (size *boundedProfileSize) add(count int) bool {
	if count > size.limit-size.size {
		size.size = size.limit + 1
		return false
	}
	size.size += count
	return true
}

func addCanonicalProfileField(size *boundedProfileSize, name string, value any, trailingComma bool) (bool, error) {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return false, err
	}
	// Nested lines receive the outer object's two-space indentation. The first
	// line follows the field prefix and therefore needs no additional padding.
	count := len(`  "`+name+`": `) + len(encoded) + 2*bytes.Count(encoded, []byte{'\n'}) + 1
	if trailingComma {
		count++
	}
	return size.add(count), nil
}

// EncodedProfileSize follows the publication encoder's field order and
// indentation, but accounts for jobs individually and stops as soon as the
// requested boundary is conclusively exceeded. The returned size is capped at
// limit+1.
func EncodedProfileSize(profile Profile, limit int) (int, error) {
	if limit < 1 {
		return 0, fmt.Errorf("profile size limit is invalid")
	}
	if err := profile.Validate(); err != nil {
		return 0, err
	}
	size := &boundedProfileSize{limit: limit}
	if !size.add(2) { // opening brace and newline
		return size.size, nil
	}
	fields := []struct {
		name  string
		value any
	}{
		{"format", profile.Format},
		{"schemaVersion", profile.SchemaVersion},
		{"revision", profile.Revision},
		{"vault_uuid", profile.VaultUUID},
		{"profile_uuid", profile.ProfileUUID},
		{"attachment", profile.Attachment},
		{"vaultPreferences", profile.VaultPreferences},
	}
	for _, field := range fields {
		ok, err := addCanonicalProfileField(size, field.name, field.value, true)
		if err != nil || !ok {
			return size.size, err
		}
	}
	if len(profile.Jobs) == 0 {
		ok, err := addCanonicalProfileField(size, "jobs", profile.Jobs, true)
		if err != nil || !ok {
			return size.size, err
		}
	} else {
		if !size.add(len("  \"jobs\": [\n")) {
			return size.size, nil
		}
		for index, job := range profile.Jobs {
			encoded, err := json.MarshalIndent(job, "", "  ")
			if err != nil {
				return 0, err
			}
			count := len(encoded) + 4*(bytes.Count(encoded, []byte{'\n'})+1) + 1
			if index+1 < len(profile.Jobs) {
				count++
			}
			if !size.add(count) {
				return size.size, nil
			}
		}
		if !size.add(len("  ],\n")) {
			return size.size, nil
		}
	}
	if profile.PasswordChange != nil {
		ok, err := addCanonicalProfileField(size, "password_change", profile.PasswordChange, true)
		if err != nil || !ok {
			return size.size, err
		}
	}
	for index, field := range []struct {
		name  string
		value any
	}{{"createdAt", profile.CreatedAt}, {"updatedAt", profile.UpdatedAt}} {
		ok, err := addCanonicalProfileField(size, field.name, field.value, index == 0)
		if err != nil || !ok {
			return size.size, err
		}
	}
	size.add(2) // closing brace and canonical trailing newline
	return size.size, nil
}

func strictParse(data []byte, target any, limit int) error {
	if len(data) == 0 || len(data) > limit {
		return fmt.Errorf("protected recovery record size is invalid")
	}
	decoder := json.NewDecoder(io.LimitReader(bytes.NewReader(data), int64(limit)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return fmt.Errorf("protected recovery record contains trailing data")
	}
	return nil
}

func ParseRoot(data []byte, connector string) (Root, error) {
	var root Root
	if err := strictParse(data, &root, MaximumRootDecryptedSize); err != nil {
		return Root{}, fmt.Errorf("parse vault root record: %w", err)
	}
	if err := root.Validate(connector); err != nil {
		return Root{}, err
	}
	return root, nil
}

func Parse(data []byte) (Profile, error) {
	var profile Profile
	if err := strictParse(data, &profile, MaximumProfileDecryptedSize); err != nil {
		return Profile{}, fmt.Errorf("parse vault profile record: %w", err)
	}
	for i := range profile.Jobs {
		profile.Jobs[i].EngineSettings = filterImportedEngineSettings(profile.Jobs[i].EngineSettings)
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func filterImportedEngineSettings(settings models.EngineSettings) models.EngineSettings {
	filtered := models.EngineSettings{}
	for engineID, imported := range settings {
		if !models.ValidEngine(engineID) {
			continue
		}
		current := models.EngineJobSettings{}
		for _, line := range imported.AdditionalOptions {
			candidate := current
			candidate.AdditionalOptions = append(append([]string(nil), current.AdditionalOptions...), line)
			if engines.ValidatePortableJobSettings(models.EngineSettings{engineID: candidate}, map[string]bool{engineID: true}) == nil {
				current = candidate
			}
		}
		filtered[engineID] = current
	}
	return filtered
}
