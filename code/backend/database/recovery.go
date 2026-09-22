package database

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/schedulevalue"
	"github.com/local/replicaro/vaultidentity"
	"github.com/local/replicaro/vaultprofile"
)

type RecoveredLocalJobAdmission struct {
	Reviewed                    models.BackupJob `json:"reviewed"`
	ReviewedActiveRepositoryIDs []string         `json:"reviewedActiveRepositoryIds"`
	ReviewedPortableTargetIDs   []string         `json:"reviewedPortableTargetIds"`
	WasEnabled                  bool             `json:"wasEnabled"`
	PriorNextRun                string           `json:"priorNextRun"`
}

func BuildRecoveredLocalJobAdmission(job models.BackupJob) RecoveredLocalJobAdmission {
	active := make([]string, 0, len(job.Targets))
	for _, target := range job.Targets {
		active = append(active, target.RepositoryID)
	}
	return RecoveredLocalJobAdmission{
		ReviewedActiveRepositoryIDs: canonicalRepositoryIDSet(active),
		ReviewedPortableTargetIDs:   canonicalRepositoryIDSet(job.PortableTargetIDs),
		WasEnabled:                  job.Enabled,
		PriorNextRun:                job.NextRun,
	}
}

func canonicalRepositoryIDSet(ids []string) []string {
	result := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		result = append(result, id)
	}
	sort.Strings(result)
	return result
}

type recoveredJobQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

type reservedConnectionRepository struct {
	ID, Name, Engine, Description      string
	ColdStorage                        bool
	ArchiveWriteClass                  string
	StorageIdentityVersion             string
	StorageIdentityKey                 string
	StorageIdentityJSON                string
	CheckSchedule, MaintenanceSchedule string
	ConcurrencyMode                    string
	AutoUnlock                         bool
	ObjectLock                         models.ObjectLockSettings
	ProfileUUID                        string
	AttachmentGeneration               int64
	NativeRepositoryID                 string
	CreatedAt                          time.Time
}

type reservedConnectionPayload struct {
	Repository            reservedConnectionRepository
	PhysicalVaultIdentity string
	Jobs                  []models.BackupJob
	LocalJobAdmissions    []RecoveredLocalJobAdmission
	Dormant               []DormantRecoveryJob
	Profile               []byte
	Root                  []byte
	UpdateExistingVault   bool
}

// StageRecoveredLocalJobAdmissions atomically closes local job admission before
// a connection may publish recovery metadata or activate native configuration.
// The reserved reviewed definition remains authoritative across retries.
func StageRecoveredLocalJobAdmissions(
	db *sql.DB,
	connectionID string,
	admissions []RecoveredLocalJobAdmission,
) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var payloadJSON string
	if err := tx.QueryRow(`SELECT payload_json FROM repository_connection_intents
		WHERE id=?`, connectionID).Scan(&payloadJSON); err != nil {
		return err
	}
	var reserved reservedConnectionPayload
	if err := json.Unmarshal([]byte(payloadJSON), &reserved); err != nil ||
		!sameCanonicalJSON(reserved.LocalJobAdmissions, admissions) {
		return fmt.Errorf("reviewed local job admissions differ from the reserved connection")
	}
	reservedJobIDs := make(map[string]bool, len(reserved.Jobs))
	localCollisions := make(map[string]bool, len(admissions))
	for _, job := range reserved.Jobs {
		if job.ID == "" || reservedJobIDs[job.ID] {
			return fmt.Errorf("reserved recovered job set is invalid")
		}
		reservedJobIDs[job.ID] = true
		var existing int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM backup_jobs WHERE id=?`, job.ID).Scan(&existing); err != nil {
			return err
		}
		if existing != 0 {
			localCollisions[job.ID] = true
		}
	}
	seen := map[string]bool{}
	for _, admission := range admissions {
		reviewed := admission.Reviewed
		if reviewed.ID == "" || seen[reviewed.ID] || !reservedJobIDs[reviewed.ID] ||
			!localCollisions[reviewed.ID] {
			return fmt.Errorf("reviewed local job admission is invalid")
		}
		seen[reviewed.ID] = true
		var reserved int
		if err := tx.QueryRow(`SELECT COUNT(*)
			FROM repository_connection_job_name_reservations r
			JOIN repository_connection_intents c ON c.id=r.connection_id
			WHERE r.connection_id=? AND r.job_id=?`,
			connectionID, reviewed.ID).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 1 {
			return fmt.Errorf("reviewed local job admission has no live connection reservation")
		}
		var name, source, sourceStorageVersion, sourceStorageKey, sourceStorageJSON, sourceBindingState string
		var schedule, excludes, tag, settingsJSON, portableJSON, nextRun string
		var beforeScriptPath, afterScriptPath string
		var beforeScriptMustSucceed, afterScriptMustSucceed bool
		var retention int
		var retentionHourly, retentionDaily, retentionWeekly, retentionMonthly, retentionYearly sql.NullInt64
		var enabled bool
		if err := tx.QueryRow(`SELECT name,source,source_storage_version,source_storage_key,
				source_storage_json,source_binding_state,schedule,retention,retention_hourly,retention_daily,
				retention_weekly,retention_monthly,retention_yearly,excludes,tag,
				before_script_path,before_script_must_succeed,after_script_path,after_script_must_succeed,
				engine_settings,portable_target_ids,enabled,COALESCE(next_run,'')
				FROM backup_jobs WHERE id=?`, reviewed.ID).Scan(
			&name, &source, &sourceStorageVersion, &sourceStorageKey, &sourceStorageJSON,
			&sourceBindingState, &schedule, &retention, &retentionHourly, &retentionDaily, &retentionWeekly,
			&retentionMonthly, &retentionYearly, &excludes, &tag, &beforeScriptPath, &beforeScriptMustSucceed,
			&afterScriptPath, &afterScriptMustSucceed,
			&settingsJSON, &portableJSON, &enabled, &nextRun,
		); err != nil {
			return fmt.Errorf("reload reviewed local job %s: %w", reviewed.ID, err)
		}
		portableTargetIDs := []string{}
		if err := json.Unmarshal([]byte(portableJSON), &portableTargetIDs); err != nil {
			return fmt.Errorf("reload reviewed local job portable targets %s: %w", reviewed.ID, err)
		}
		activeRows, err := tx.Query(`SELECT repository_id FROM backup_job_targets
			WHERE job_id=? ORDER BY repository_id`, reviewed.ID)
		if err != nil {
			return err
		}
		activeTargetIDs := []string{}
		for activeRows.Next() {
			var repositoryID string
			if err := activeRows.Scan(&repositoryID); err != nil {
				_ = activeRows.Close()
				return err
			}
			activeTargetIDs = append(activeTargetIDs, repositoryID)
		}
		if err := activeRows.Close(); err != nil {
			return err
		}
		reviewedSettings, err := encodeEngineSettings(reviewed.EngineSettings)
		if err != nil {
			return err
		}
		loadedRetention := models.BackupJob{Retention: retention,
			RetentionHourly: nullableRetentionValue(retentionHourly), RetentionDaily: nullableRetentionValue(retentionDaily),
			RetentionWeekly: nullableRetentionValue(retentionWeekly), RetentionMonthly: nullableRetentionValue(retentionMonthly),
			RetentionYearly: nullableRetentionValue(retentionYearly)}
		if name != reviewed.Name || source != reviewed.Source || schedule != reviewed.Schedule ||
			sourceStorageVersion != reviewed.SourceStorageVersion ||
			sourceStorageKey != reviewed.SourceStorageKey ||
			sourceStorageJSON != reviewed.SourceStorageDescriptorJSON ||
			!models.RetentionPolicyEqual(loadedRetention, reviewed) || excludes != reviewed.Excludes || tag != reviewed.Tag ||
			beforeScriptPath != reviewed.BeforeScriptPath ||
			beforeScriptMustSucceed != reviewed.BeforeScriptMustSucceed ||
			afterScriptPath != reviewed.AfterScriptPath ||
			afterScriptMustSucceed != reviewed.AfterScriptMustSucceed ||
			settingsJSON != reviewedSettings ||
			!reflect.DeepEqual(canonicalRepositoryIDSet(activeTargetIDs), admission.ReviewedActiveRepositoryIDs) ||
			!reflect.DeepEqual(canonicalRepositoryIDSet(portableTargetIDs), admission.ReviewedPortableTargetIDs) ||
			enabled != admission.WasEnabled || nextRun != admission.PriorNextRun {
			return fmt.Errorf("the reviewed local job changed before connection admission")
		}
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM operations
			WHERE job_id=? AND status IN ('queued','running')`, reviewed.ID).Scan(&active); err != nil {
			return err
		}
		if active != 0 {
			return ErrJobRunActive
		}
	}
	if len(seen) != len(localCollisions) {
		return fmt.Errorf("an unreviewed local job appeared before connection admission")
	}
	return tx.Commit()
}

func sameCanonicalJSON(left, right any) bool {
	leftJSON, leftErr := json.Marshal(left)
	rightJSON, rightErr := json.Marshal(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	var leftValue, rightValue any
	if json.Unmarshal(leftJSON, &leftValue) != nil || json.Unmarshal(rightJSON, &rightValue) != nil {
		return false
	}
	return reflect.DeepEqual(leftValue, rightValue)
}

func verifyReservedRecoveryAttachment(tx *sql.Tx, parent RepositoryConnectionIntent, repo models.Repository,
	jobs []models.BackupJob, dormant []DormantRecoveryJob) error {
	if !json.Valid([]byte(parent.PayloadJSON)) {
		return fmt.Errorf("repository connection reserved payload is invalid")
	}
	var reserved reservedConnectionPayload
	if err := json.Unmarshal([]byte(parent.PayloadJSON), &reserved); err != nil {
		return fmt.Errorf("decode repository connection reserved payload: %w", err)
	}
	actualRepository := reservedConnectionRepository{
		ID: repo.ID, Name: repo.Name, Engine: repo.Engine, Description: repo.Description,
		ColdStorage: repo.ColdStorage, ArchiveWriteClass: repo.ArchiveWriteClass,
		StorageIdentityVersion: repo.StorageIdentityVersion,
		StorageIdentityKey:     repo.StorageIdentityKey,
		StorageIdentityJSON:    repo.StorageIdentityJSON,
		CheckSchedule:          repo.CheckSchedule, MaintenanceSchedule: repo.MaintenanceSchedule,
		ConcurrencyMode: repo.ConcurrencyMode,
		AutoUnlock:      repo.AutoUnlock, ObjectLock: repo.ObjectLock,
		ProfileUUID: repo.ProfileUUID, AttachmentGeneration: repo.AttachmentGeneration,
		NativeRepositoryID: repo.NativeRepositoryID, CreatedAt: repo.CreatedAt,
	}
	if !sameCanonicalJSON(reserved.Repository, actualRepository) {
		return fmt.Errorf("repository connection repository differs from its reserved payload")
	}
	if reserved.Repository.ID != parent.CanonicalIdentity {
		return fmt.Errorf("repository connection vault UUID differs from its reserved payload")
	}
	if !sameCanonicalJSON(reserved.Jobs, jobs) {
		return fmt.Errorf("repository connection jobs differ from its reserved payload")
	}
	if !sameCanonicalJSON(reserved.Dormant, dormant) {
		return fmt.Errorf("repository connection attachment differs from its reserved payload")
	}
	profileSum := sha256.Sum256(reserved.Profile)
	if hex.EncodeToString(profileSum[:]) != parent.ProfileSHA256 {
		return fmt.Errorf("repository connection reserved profile checksum is invalid")
	}
	return nil
}

// NormalizeRecoveredJobTargets merges the newly connected vault into an
// existing portable job when its reviewed definition is unchanged. Target
// membership is intentionally excluded from the definition comparison because
// reconnecting another vault is precisely what this operation is adding.
func NormalizeRecoveredJobTargets(db *sql.DB, repositoryID string, jobs []models.BackupJob) ([]models.BackupJob, error) {
	return normalizeRecoveredJobTargets(db, repositoryID, jobs)
}

func normalizeRecoveredJobTargets(queryer recoveredJobQueryer, repositoryID string, jobs []models.BackupJob) ([]models.BackupJob, error) {
	if strings.TrimSpace(repositoryID) == "" {
		return nil, fmt.Errorf("recovered vault ID is required")
	}
	normalized := append([]models.BackupJob(nil), jobs...)
	seen := map[string]bool{}
	for index := range normalized {
		job := &normalized[index]
		if job.ID == "" || seen[job.ID] {
			return nil, fmt.Errorf("recovered jobs contain a duplicate or empty portable job ID")
		}
		seen[job.ID] = true
		if _, err := encodeEngineSettings(job.EngineSettings); err != nil {
			return nil, err
		}
		if err := validateJobSourceBinding(*job); err != nil {
			return nil, fmt.Errorf("portable job %s has an invalid source storage binding: %w", job.ID, err)
		}
		requestedTargets := mergePortableTargetIDs(job.PortableTargetIDs, []string{repositoryID})
		importedSettings := make(models.EngineSettings, len(job.EngineSettings))
		for engineID, settings := range job.EngineSettings {
			importedSettings[engineID] = settings
		}
		newEngine := ""
		for _, target := range job.Targets {
			if target.RepositoryID == repositoryID && target.Engine != "" {
				newEngine = target.Engine
				break
			}
		}

		var name, source, sourceStorageVersion, sourceStorageKey, sourceStorageJSON, sourceBindingState string
		var schedule, excludes, tag, existingSettings, existingTargets string
		var beforeScriptPath, afterScriptPath string
		var beforeScriptMustSucceed, afterScriptMustSucceed bool
		var retention, enabled int
		var retentionHourly, retentionDaily, retentionWeekly, retentionMonthly, retentionYearly sql.NullInt64
		err := queryer.QueryRow(`SELECT name, source, source_storage_version, source_storage_key,
			source_storage_json, source_binding_state, schedule, retention, retention_hourly, retention_daily,
			retention_weekly, retention_monthly, retention_yearly, excludes, tag,
			before_script_path,before_script_must_succeed,after_script_path,after_script_must_succeed,
			engine_settings, portable_target_ids, enabled FROM backup_jobs WHERE id = ?`, job.ID).Scan(
			&name, &source, &sourceStorageVersion, &sourceStorageKey, &sourceStorageJSON,
			&sourceBindingState, &schedule, &retention, &retentionHourly, &retentionDaily, &retentionWeekly,
			&retentionMonthly, &retentionYearly, &excludes, &tag, &beforeScriptPath, &beforeScriptMustSucceed,
			&afterScriptPath, &afterScriptMustSucceed, &existingSettings, &existingTargets, &enabled)
		if err == sql.ErrNoRows {
			job.PortableTargetIDs = requestedTargets
			continue
		}
		if err != nil {
			return nil, err
		}
		localSettings, err := decodeEngineSettings(existingSettings)
		if err != nil {
			return nil, fmt.Errorf("portable job %s has invalid local engine settings: %w", job.ID, err)
		}
		// A portable ID already present locally is authoritative. Importing a
		// vault attaches the new target to that definition and discards stale
		// or unsupported profile differences instead of asking the user to
		// choose between two definitions of the same ID.
		job.Name, job.Source = name, source
		job.SourceStorageVersion, job.SourceStorageKey, job.SourceStorageDescriptorJSON =
			sourceStorageVersion, sourceStorageKey, sourceStorageJSON
		job.SourceBindingState = sourceBindingState
		job.Schedule, job.Retention, job.Excludes, job.Tag = schedule, retention, excludes, tag
		job.RetentionHourly, job.RetentionDaily = nullableRetentionValue(retentionHourly), nullableRetentionValue(retentionDaily)
		job.RetentionWeekly, job.RetentionMonthly = nullableRetentionValue(retentionWeekly), nullableRetentionValue(retentionMonthly)
		job.RetentionYearly = nullableRetentionValue(retentionYearly)
		job.BeforeScriptPath, job.BeforeScriptMustSucceed = beforeScriptPath, beforeScriptMustSucceed
		job.AfterScriptPath, job.AfterScriptMustSucceed = afterScriptPath, afterScriptMustSucceed
		job.EngineSettings = localSettings
		if newEngine != "" {
			if _, represented := job.EngineSettings[newEngine]; !represented {
				settings := models.EngineJobSettings{}
				if imported, ok := importedSettings[newEngine]; ok {
					settings = imported
				}
				addition := models.EngineSettings{newEngine: settings}
				if err := engines.ValidatePortableJobSettings(
					addition, map[string]bool{newEngine: true},
				); err != nil {
					return nil, fmt.Errorf("portable job %s has invalid imported %s settings: %w", job.ID, newEngine, err)
				}
				job.EngineSettings[newEngine] = settings
			}
		}
		var existingTargetIDs []string
		if err := json.Unmarshal([]byte(existingTargets), &existingTargetIDs); err != nil {
			return nil, fmt.Errorf("portable job %s has an invalid existing target list", job.ID)
		}
		job.PortableTargetIDs = mergePortableTargetIDs(existingTargetIDs, requestedTargets)
	}
	return normalized, nil
}

// RenameRecoveredJobNameConflicts gives newly reconstructed jobs deterministic
// local names while preserving an existing local definition with the same ID.
func RenameRecoveredJobNameConflicts(db *sql.DB, jobs []models.BackupJob) ([]models.BackupJob, error) {
	rows, err := db.Query(`SELECT id,name FROM backup_jobs`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	existingByID := map[string]string{}
	used := map[string]bool{}
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		existingByID[id] = name
		used[strings.ToLower(strings.TrimSpace(name))] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	reservedRows, err := db.Query(`SELECT job_id,name FROM repository_connection_job_name_reservations`)
	if err != nil {
		return nil, err
	}
	for reservedRows.Next() {
		var id, name string
		if err := reservedRows.Scan(&id, &name); err != nil {
			_ = reservedRows.Close()
			return nil, err
		}
		if _, local := existingByID[id]; !local {
			used[strings.ToLower(strings.TrimSpace(name))] = true
		}
	}
	if err := reservedRows.Close(); err != nil {
		return nil, err
	}
	renamed := append([]models.BackupJob(nil), jobs...)
	for index := range renamed {
		if localName, ok := existingByID[renamed[index].ID]; ok {
			renamed[index].Name = localName
			continue
		}
		base := strings.TrimSpace(renamed[index].Name)
		candidate := base
		for suffix := 1; used[strings.ToLower(candidate)]; suffix++ {
			candidate = fmt.Sprintf("%s %d", base, suffix)
		}
		renamed[index].Name = candidate
		used[strings.ToLower(candidate)] = true
	}
	return renamed, nil
}

func RenameRecoveredRepositoryNameConflict(db *sql.DB, repo models.Repository) (models.Repository, error) {
	base := strings.TrimSpace(repo.Name)
	if base == "" {
		return repo, fmt.Errorf("recovered vault name is required")
	}
	rows, err := db.Query(`SELECT name FROM repositories WHERE id<>?`, repo.ID)
	if err != nil {
		return repo, err
	}
	defer rows.Close()
	used := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return repo, err
		}
		used[strings.ToLower(strings.TrimSpace(name))] = true
	}
	if err := rows.Err(); err != nil {
		return repo, err
	}
	reservedRows, err := db.Query(`SELECT repository_id,name
		FROM repository_connection_name_reservations WHERE repository_id<>?`, repo.ID)
	if err != nil {
		return repo, err
	}
	for reservedRows.Next() {
		var repositoryID, name string
		if err := reservedRows.Scan(&repositoryID, &name); err != nil {
			_ = reservedRows.Close()
			return repo, err
		}
		used[strings.ToLower(strings.TrimSpace(name))] = true
	}
	if err := reservedRows.Close(); err != nil {
		return repo, err
	}
	candidate := base
	for suffix := 1; used[strings.ToLower(candidate)]; suffix++ {
		candidate = fmt.Sprintf("%s %d", base, suffix)
	}
	repo.Name = candidate
	return repo, nil
}

func mergePortableTargetIDs(groups ...[]string) []string {
	merged := []string{}
	seen := map[string]bool{}
	for _, group := range groups {
		for _, id := range group {
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			merged = append(merged, id)
		}
	}
	return merged
}

// ValidateRecoveredJobEngineSettings ensures that a recovered definition has
// settings for every engine represented by its existing and new vault targets.
func ValidateRecoveredJobEngineSettings(db *sql.DB, repo models.Repository, jobs []models.BackupJob) error {
	return validateRecoveredJobEngineSettings(db, repo.ID, repo.Engine, jobs)
}

func validateRecoveredJobEngineSettings(queryer recoveredJobQueryer, repositoryID, repositoryEngine string, jobs []models.BackupJob) error {
	for _, job := range jobs {
		represented := map[string]bool{repositoryEngine: true}
		for _, target := range job.Targets {
			if target.Engine != "" {
				represented[target.Engine] = true
			}
		}
		rows, err := queryer.Query(`SELECT r.engine FROM backup_job_targets t JOIN repositories r ON r.id = t.repository_id WHERE t.job_id = ?`, job.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var engine string
			if err := rows.Scan(&engine); err != nil {
				_ = rows.Close()
				return err
			}
			represented[engine] = true
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if err := engines.ValidatePortableJobSettings(job.EngineSettings, represented); err != nil {
			return fmt.Errorf("portable job %s cannot be attached: engine settings are incompatible with the recovered vault targets: %w", job.ID, err)
		}
	}
	return nil
}

func AttachRecoveredRepository(
	db *sql.DB,
	repo models.Repository,
	jobs []models.BackupJob,
	dormant []DormantRecoveryJob,
	connectionIntentID string,
) (string, []string, error) {
	if strings.TrimSpace(repo.ID) == "" || strings.TrimSpace(repo.Name) == "" || !models.ValidEngine(repo.Engine) {
		return "", nil, fmt.Errorf("recovered vault settings are invalid")
	}
	for _, job := range jobs {
		normalizedSchedule, validSchedule := schedulevalue.NormalizeJob(job.Schedule)
		if !validSchedule || normalizedSchedule != job.Schedule {
			return "", nil, fmt.Errorf("recovered job %s schedule is invalid or noncanonical", job.ID)
		}
	}
	if err := models.ValidateVaultPassword(repo.Passphrase); err != nil {
		return "", nil, err
	}
	archiveWriteClass, err := models.NormalizeColdStorage(repo.Engine, repo.Connector, repo.ColdStorage, repo.ArchiveWriteClass)
	if err != nil {
		return "", nil, err
	}
	repo.ArchiveWriteClass = archiveWriteClass
	if repo.ColdStorage && repo.CheckSchedule != "" && repo.CheckSchedule != "manual" {
		return "", nil, fmt.Errorf("%s", models.ColdStorageIntegrityHelp)
	}
	identity, storageVersion, storageKey, storageJSON, err := repositoryPersistenceIdentity(repo)
	if err != nil {
		return "", nil, err
	}
	if connectionIntentID == "" {
		return "", nil, fmt.Errorf("repository connection intent is required before attachment")
	}
	if strings.TrimSpace(identity) == "" {
		return "", nil, fmt.Errorf("recovered vault canonical identity is empty")
	}
	repo.CanonicalIdentity = identity
	repo.StorageIdentityVersion = storageVersion
	repo.StorageIdentityKey = storageKey
	repo.StorageIdentityJSON = storageJSON
	encodedPassphrase, optionsJSON, err := encodeRepositorySecrets(repo.Connector, repo.Passphrase, repo.ConnectorOptions)
	if err != nil {
		return "", nil, err
	}
	if strings.TrimSpace(repo.ProfileUUID) == "" || repo.AttachmentGeneration < 1 ||
		strings.TrimSpace(repo.NativeRepositoryID) == "" {
		return "", nil, fmt.Errorf("recovered vault profile binding or native identity is incomplete")
	}
	tx, err := db.Begin()
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback() }()
	parent, err := scanConnectionIntent(tx.QueryRow(`SELECT `+connectionIntentColumns+
		` FROM repository_connection_intents WHERE id=?`, connectionIntentID).Scan)
	if err != nil {
		return "", nil, fmt.Errorf("reload repository connection intent: %w", err)
	}
	if parent.CanonicalIdentity != repo.ID || parent.State != "prepared" && parent.State != "attachment_pending" {
		return "", nil, fmt.Errorf("repository connection parent does not authorize attachment")
	}
	if err := AssertVaultUUIDAvailableTx(tx, repo.ID); err != nil {
		return "", nil, err
	}
	if err := verifyReservedRecoveryAttachment(tx, parent, repo, jobs, dormant); err != nil {
		return "", nil, err
	}
	var reservedPayload reservedConnectionPayload
	if err := json.Unmarshal([]byte(parent.PayloadJSON), &reservedPayload); err != nil {
		return "", nil, fmt.Errorf("decode repository connection reserved payload: %w", err)
	}
	repo.Name = strings.TrimSpace(repo.Name)
	if err := ensureUniqueRepositoryName(tx, repo.Name, repo.ID); err != nil {
		return "", nil, err
	}
	check := repo.CheckSchedule
	if check == "" {
		check = "manual"
	}
	maintenance := repo.MaintenanceSchedule
	if maintenance == "" {
		maintenance = "daily"
	}
	concurrencyMode, err := models.NormalizeConcurrencyModeForConnector(repo.Connector, repo.ConcurrencyMode)
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC()
	createdAt := repo.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	objectLockJSON, err := encodeRepositoryObjectLock(repo)
	if err != nil {
		return "", nil, err
	}
	_, err = tx.Exec(`INSERT INTO repositories
		(id, name, engine, connector, cold_storage, archive_write_class, location, canonical_identity,
		 storage_identity_version, storage_identity_key, storage_identity_json, description, passphrase,
		 connector_options, native_repository_id, profile_uuid, attachment_generation, metadata_cache_binding, check_schedule, next_check, maintenance_schedule, object_lock_json, next_maintenance, concurrency_mode, auto_unlock, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, repo.ID, strings.TrimSpace(repo.Name),
		repo.Engine, repo.Connector, repo.ColdStorage, repo.ArchiveWriteClass, repo.Location, identity,
		storageVersion, storageKey, storageJSON, repo.Description, encodedPassphrase,
		optionsJSON, repo.NativeRepositoryID, repo.ProfileUUID, repo.AttachmentGeneration, uuid.NewString(), check, NextRunFrom(check, now), maintenance, objectLockJSON, NextRunFrom(maintenance, now), concurrencyMode, repo.AutoUnlock, createdAt)
	if err != nil {
		return "", nil, err
	}
	normalizedJobs, err := normalizeRecoveredJobTargets(tx, repo.ID, jobs)
	if err != nil {
		return "", nil, err
	}
	if err := validateRecoveredJobEngineSettings(tx, repo.ID, repo.Engine, normalizedJobs); err != nil {
		return "", nil, err
	}
	localAdmissions := map[string]RecoveredLocalJobAdmission{}
	for _, admission := range reservedPayload.LocalJobAdmissions {
		if admission.Reviewed.ID == "" || localAdmissions[admission.Reviewed.ID].Reviewed.ID != "" {
			return "", nil, fmt.Errorf("repository connection local job admissions are invalid")
		}
		localAdmissions[admission.Reviewed.ID] = admission
	}
	seenJobNames := map[string]bool{}
	affectedProfileIDs := []string{repo.ID}
	for _, job := range normalizedJobs {
		job.Name = strings.TrimSpace(job.Name)
		// Historical source spelling is metadata, not display-label padding.
		// First local binding performs configured-route eligibility checks.
		if job.ID == "" || strings.TrimSpace(job.Name) == "" || job.Source == "" || job.Enabled {
			return "", nil, fmt.Errorf("recovered jobs must have stable IDs and be disabled")
		}
		if err := validateJobSourceBinding(job); err != nil {
			return "", nil, fmt.Errorf("recovered job source storage binding is invalid: %w", err)
		}
		nameKey := strings.ToLower(job.Name)
		if seenJobNames[nameKey] {
			return "", nil, ErrJobNameExists
		}
		seenJobNames[nameKey] = true
		if err := ensureUniqueJobName(tx, job.Name, job.ID); err != nil {
			return "", nil, err
		}
		settings, err := encodeEngineSettings(job.EngineSettings)
		if err != nil {
			return "", nil, err
		}
		portableTargetIDs := job.PortableTargetIDs
		if len(portableTargetIDs) == 0 {
			portableTargetIDs = []string{repo.ID}
		}
		portableJSON, err := json.Marshal(portableTargetIDs)
		if err != nil {
			return "", nil, err
		}
		var existing int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM backup_jobs WHERE id = ?`, job.ID).Scan(&existing); err != nil {
			return "", nil, err
		}
		if existing != 0 {
			var name, source, schedule, excludes, tag, encoded, existingTargets string
			var beforeScriptPath, afterScriptPath string
			var beforeScriptMustSucceed, afterScriptMustSucceed bool
			var retention int
			var retentionHourly, retentionDaily, retentionWeekly, retentionMonthly, retentionYearly sql.NullInt64
			var enabled bool
			var sourceStorageVersion, sourceStorageKey, sourceStorageJSON, sourceBindingState string
			if err := tx.QueryRow(`SELECT name, source, source_storage_version, source_storage_key, source_storage_json,
				source_binding_state, schedule, retention, retention_hourly, retention_daily, retention_weekly,
				retention_monthly, retention_yearly, excludes, tag,
				before_script_path,before_script_must_succeed,after_script_path,after_script_must_succeed,
				engine_settings, portable_target_ids, enabled
				FROM backup_jobs WHERE id = ?`, job.ID).Scan(&name, &source,
				&sourceStorageVersion, &sourceStorageKey, &sourceStorageJSON,
				&sourceBindingState, &schedule, &retention,
				&retentionHourly, &retentionDaily, &retentionWeekly, &retentionMonthly, &retentionYearly,
				&excludes, &tag, &beforeScriptPath, &beforeScriptMustSucceed,
				&afterScriptPath, &afterScriptMustSucceed, &encoded, &existingTargets, &enabled); err != nil {
				return "", nil, err
			}
			if _, ok := localAdmissions[job.ID]; !ok {
				return "", nil, fmt.Errorf("portable job %s is not reserved for this connection", job.ID)
			}
			localSettings, err := decodeEngineSettings(encoded)
			if err != nil {
				return "", nil, err
			}
			for engineID, localSection := range localSettings {
				if importedSection, ok := job.EngineSettings[engineID]; !ok ||
					!reflect.DeepEqual(localSection, importedSection) {
					return "", nil, fmt.Errorf("portable job %s local engine settings changed after review", job.ID)
				}
			}
			loadedRetention := models.BackupJob{Retention: retention,
				RetentionHourly: nullableRetentionValue(retentionHourly), RetentionDaily: nullableRetentionValue(retentionDaily),
				RetentionWeekly: nullableRetentionValue(retentionWeekly), RetentionMonthly: nullableRetentionValue(retentionMonthly),
				RetentionYearly: nullableRetentionValue(retentionYearly)}
			if name != job.Name || source != job.Source ||
				sourceStorageVersion != job.SourceStorageVersion ||
				sourceStorageKey != job.SourceStorageKey ||
				sourceStorageJSON != job.SourceStorageDescriptorJSON ||
				sourceBindingState != normalizedJobSourceBindingState(job.SourceBindingState) ||
				schedule != job.Schedule || !models.RetentionPolicyEqual(loadedRetention, job) || excludes != job.Excludes ||
				tag != job.Tag || beforeScriptPath != job.BeforeScriptPath ||
				beforeScriptMustSucceed != job.BeforeScriptMustSucceed ||
				afterScriptPath != job.AfterScriptPath ||
				afterScriptMustSucceed != job.AfterScriptMustSucceed {
				return "", nil, fmt.Errorf("portable job %s cannot be attached: its definition or settings differ from the existing local job; use the existing job definition or choose a different job ID", job.ID)
			}
			// Recovered jobs remain disabled by product design.
			updated, err := tx.Exec(`UPDATE backup_jobs
				SET portable_target_ids=?,engine_settings=?,enabled=0,next_run=''
				WHERE id=?`,
				string(portableJSON), settings, job.ID)
			if err != nil {
				return "", nil, err
			}
			if count, _ := updated.RowsAffected(); count != 1 {
				return "", nil, fmt.Errorf("portable job %s admission changed before attachment", job.ID)
			}
			rows, err := tx.Query(`SELECT repository_id FROM backup_job_targets WHERE job_id = ?`, job.ID)
			if err != nil {
				return "", nil, err
			}
			for rows.Next() {
				var repositoryID string
				if err := rows.Scan(&repositoryID); err != nil {
					_ = rows.Close()
					return "", nil, err
				}
				affectedProfileIDs = append(affectedProfileIDs, repositoryID)
			}
			if err := rows.Close(); err != nil {
				return "", nil, err
			}
		} else {
			_, err = tx.Exec(`INSERT INTO backup_jobs
				(id, name, source, source_storage_version, source_storage_key, source_storage_json,
				 source_binding_state, schedule, enabled, next_run, last_run, retention, retention_hourly, retention_daily,
				 retention_weekly, retention_monthly, retention_yearly, excludes, tag,
				 before_script_path,before_script_must_succeed,after_script_path,after_script_must_succeed,
				 engine_settings, portable_target_ids)
				VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0, '', '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
				job.ID, job.Name, job.Source, job.SourceStorageVersion, job.SourceStorageKey,
				job.SourceStorageDescriptorJSON, normalizedJobSourceBindingState(job.SourceBindingState), job.Schedule,
				job.Retention, job.RetentionHourly,
				job.RetentionDaily, job.RetentionWeekly, job.RetentionMonthly, job.RetentionYearly, job.Excludes,
				job.Tag, job.BeforeScriptPath, job.BeforeScriptMustSucceed,
				job.AfterScriptPath, job.AfterScriptMustSucceed, settings, string(portableJSON))
			if err != nil {
				return "", nil, err
			}
		}
		if _, err := tx.Exec(`INSERT INTO backup_job_targets (job_id, repository_id) VALUES (?, ?)`, job.ID, repo.ID); err != nil {
			return "", nil, err
		}
	}
	dormantActiveAffected, err := reconcileDormantWithActiveJobs(tx, normalizedJobs)
	if err != nil {
		return "", nil, err
	}
	affectedProfileIDs = append(affectedProfileIDs, dormantActiveAffected...)
	dormantAffected, err := insertDormantRecoveryJobs(tx, repo.ID, dormant)
	if err != nil {
		return "", nil, err
	}
	affectedProfileIDs = append(affectedProfileIDs, dormantAffected...)
	if repo.Engine == engines.KopiaID {
		desired, desiredErr := DeriveKopiaPolicyDesired(tx, repo.ID)
		if desiredErr != nil {
			return "", nil, desiredErr
		}
		derivedDigest, digestErr := engines.KopiaManagedPolicyDigest(desired)
		if digestErr != nil {
			return "", nil, digestErr
		}
		// Connection attaches recovered jobs disabled and leaves the one existing
		// reconciler responsible for native desired-policy convergence.
		if err := InitializeKopiaPolicyStateTx(tx, repo.ID, derivedDigest, false); err != nil {
			return "", nil, err
		}
	}
	if err := markVaultProfilesDirty(tx, affectedProfileIDs); err != nil {
		return "", nil, err
	}
	if _, err := tx.Exec(`INSERT INTO activity_log (timestamp, level, message) VALUES (datetime('now'), 'INFO', ?)`, "Repository connected: "+repo.Name); err != nil {
		return "", nil, err
	}
	if connectionIntentID != "" {
		if _, err := tx.Exec(`DELETE FROM repository_connection_intents WHERE id = ?`, connectionIntentID); err != nil {
			return "", nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return repo.ID, mergePortableTargetIDs(affectedProfileIDs), nil
}

// ReconnectRecoveredRepository completes the existing-row variant of the
// durable connection workflow. The reviewed native/profile mutations have
// already completed; this transaction changes only the exact saved vault row
// and attachment state for jobs that already exist locally.
func ReconnectRecoveredRepository(db *sql.DB, repo models.Repository, jobs []models.BackupJob, dormant []DormantRecoveryJob,
	connectionIntentID string) (string, []string, error) {
	if strings.TrimSpace(repo.ID) == "" || strings.TrimSpace(repo.ProfileUUID) == "" ||
		repo.AttachmentGeneration < 1 || strings.TrimSpace(repo.NativeRepositoryID) == "" {
		return "", nil, fmt.Errorf("reconnect vault identity is incomplete")
	}
	_, _, _, _, err := repositoryPersistenceIdentity(repo)
	if err != nil {
		return "", nil, err
	}
	encodedPassphrase, optionsJSON, err := encodeRepositorySecrets(repo.Connector, repo.Passphrase, repo.ConnectorOptions)
	if err != nil {
		return "", nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return "", nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if connectionIntentID != "" {
		parent, loadErr := scanConnectionIntent(tx.QueryRow(`SELECT `+connectionIntentColumns+
			` FROM repository_connection_intents WHERE id=?`, connectionIntentID).Scan)
		if loadErr != nil || parent.CanonicalIdentity != repo.ID || parent.Connector != repo.Connector ||
			parent.Location != repo.Location || parent.NativeFingerprint != repo.NativeRepositoryID ||
			parent.State != "prepared" && parent.State != "attachment_pending" {
			return "", nil, fmt.Errorf("repository connection parent does not authorize exact reconnect")
		}
		if parent.ColdStorage != repo.ColdStorage || parent.ArchiveWriteClass != repo.ArchiveWriteClass {
			return "", nil, fmt.Errorf("repository connection parent does not authorize the reviewed cold-storage selection")
		}
		var reviewed reservedConnectionPayload
		if json.Unmarshal([]byte(parent.PayloadJSON), &reviewed) != nil || len(reviewed.Root) == 0 {
			return "", nil, fmt.Errorf("repository connection parent has no reviewed vault root state")
		}
		reviewedRoot, parseErr := vaultprofile.ParseRoot(reviewed.Root, repo.Connector)
		if parseErr != nil || reviewedRoot.VaultUUID != repo.ID || reviewedRoot.Repository.Engine != repo.Engine ||
			reviewedRoot.Repository.NativeRepositoryID != repo.NativeRepositoryID {
			return "", nil, fmt.Errorf("repository connection parent has invalid reviewed vault root state")
		}
		if repo.ColdStorage && (reviewedRoot.Repository.S3Storage == nil || reviewedRoot.Repository.S3Storage.Mode != "cold" ||
			reviewedRoot.Repository.S3Storage.DataStorageClass != repo.ArchiveWriteClass) {
			return "", nil, fmt.Errorf("reviewed vault root does not contain the selected cold-storage class")
		}
		if reviewed.UpdateExistingVault {
			if err := verifyReservedRecoveryAttachment(tx, parent, repo, jobs, dormant); err != nil {
				return "", nil, err
			}
		}
	}
	var storedEngine, storedConnector, storedLocation, storedIdentity, storedVersion, storedKey, storedJSON, storedNative, storedProfile string
	if err := tx.QueryRow(`SELECT engine,connector,location,canonical_identity,storage_identity_version,
		storage_identity_key,storage_identity_json,native_repository_id,profile_uuid FROM repositories WHERE id=?`, repo.ID).
		Scan(&storedEngine, &storedConnector, &storedLocation, &storedIdentity, &storedVersion, &storedKey,
			&storedJSON, &storedNative, &storedProfile); err != nil || storedNative != repo.NativeRepositoryID ||
		storedEngine != repo.Engine || storedConnector != repo.Connector {
		return "", nil, fmt.Errorf("saved vault changed before exact reconnect")
	}
	var reviewedUpdate bool
	if connectionIntentID != "" {
		var payloadJSON string
		if err := tx.QueryRow(`SELECT payload_json FROM repository_connection_intents WHERE id=?`, connectionIntentID).Scan(&payloadJSON); err != nil {
			return "", nil, err
		}
		var reviewed reservedConnectionPayload
		if err := json.Unmarshal([]byte(payloadJSON), &reviewed); err != nil {
			return "", nil, fmt.Errorf("repository connection parent payload is invalid")
		}
		reviewedUpdate = reviewed.UpdateExistingVault
	}
	if !reviewedUpdate && (storedLocation != repo.Location || storedIdentity != repo.CanonicalIdentity ||
		storedVersion != repo.StorageIdentityVersion || storedKey != repo.StorageIdentityKey || storedJSON != repo.StorageIdentityJSON) {
		return "", nil, fmt.Errorf("saved vault location can only change through confirmed Update existing vault")
	}
	for _, job := range jobs {
		var name, source string
		if err := tx.QueryRow(`SELECT name,source FROM backup_jobs WHERE id=?`, job.ID).Scan(&name, &source); err != nil ||
			name != job.Name || !vaultidentity.EquivalentSource(source, job.Source) {
			return "", nil, fmt.Errorf("profile job %s is not the exact existing local job", job.ID)
		}
		var targetCount int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM backup_job_targets WHERE job_id=? AND repository_id=?`, job.ID, repo.ID).
			Scan(&targetCount); err != nil || targetCount != 1 {
			return "", nil, fmt.Errorf("profile job %s is not attached to the expected vault", job.ID)
		}
	}
	if err := validateRecoveredJobEngineSettings(tx, repo.ID, repo.Engine, jobs); err != nil {
		return "", nil, err
	}
	check, maintenance := repo.CheckSchedule, repo.MaintenanceSchedule
	if check == "" {
		check = "manual"
	}
	if maintenance == "" {
		maintenance = "daily"
	}
	concurrencyMode, err := models.NormalizeConcurrencyModeForConnector(repo.Connector, repo.ConcurrencyMode)
	if err != nil {
		return "", nil, err
	}
	now := time.Now().UTC()
	var result sql.Result
	if reviewedUpdate {
		// A confirmed same-UUID update is the only path that replaces configured
		// address facts. Clear the observational alias and invalidate derived
		// presentation in the same commit; aliases alone never rewrite Location.
		result, err = tx.Exec(`UPDATE repositories SET name=?,description=?,passphrase=?,connector_options=?,
			cold_storage=?,archive_write_class=?,location=?,canonical_identity=?,storage_identity_version=?,storage_identity_key=?,storage_identity_json=?,
			resolved_repository_path='',resolved_repository_observed_at='',vault_size_dirty=1,
			profile_uuid=?,attachment_generation=?,check_schedule=?,next_check=?,maintenance_schedule=?,next_maintenance=?,concurrency_mode=?,auto_unlock=?
			WHERE id=? AND native_repository_id=?`, strings.TrimSpace(repo.Name), repo.Description,
			encodedPassphrase, optionsJSON, repo.ColdStorage, repo.ArchiveWriteClass,
			repo.Location, repo.CanonicalIdentity, repo.StorageIdentityVersion, repo.StorageIdentityKey, repo.StorageIdentityJSON,
			repo.ProfileUUID, repo.AttachmentGeneration, check, NextRunFrom(check, now),
			maintenance, NextRunFrom(maintenance, now), concurrencyMode, repo.AutoUnlock, repo.ID, repo.NativeRepositoryID)
	} else {
		result, err = tx.Exec(`UPDATE repositories SET name=?,description=?,passphrase=?,connector_options=?,
			cold_storage=?,archive_write_class=?,profile_uuid=?,attachment_generation=?,check_schedule=?,next_check=?,maintenance_schedule=?,next_maintenance=?,concurrency_mode=?,auto_unlock=?
			WHERE id=? AND native_repository_id=?`, strings.TrimSpace(repo.Name), repo.Description,
			encodedPassphrase, optionsJSON, repo.ColdStorage, repo.ArchiveWriteClass,
			repo.ProfileUUID, repo.AttachmentGeneration, check, NextRunFrom(check, now),
			maintenance, NextRunFrom(maintenance, now), concurrencyMode, repo.AutoUnlock, repo.ID, repo.NativeRepositoryID)
	}
	if err != nil {
		return "", nil, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return "", nil, fmt.Errorf("saved vault changed before reconnect commit")
	}
	if reviewedUpdate || storedProfile != repo.ProfileUUID {
		// The authoritative profile switch dirties the independently stored cache.
		// A later forced complete refresh rebuilds presentation for the new profile;
		// cache failure cannot roll back the already verified attachment.
		if _, err := tx.Exec(`UPDATE repositories SET required_generation=required_generation+1 WHERE id=?`, repo.ID); err != nil {
			return "", nil, err
		}
	}
	if _, err := insertDormantRecoveryJobs(tx, repo.ID, dormant); err != nil {
		return "", nil, err
	}
	if repo.Engine == engines.KopiaID {
		desired, desiredErr := DeriveKopiaPolicyDesired(tx, repo.ID)
		if desiredErr != nil {
			return "", nil, desiredErr
		}
		digest, digestErr := engines.KopiaManagedPolicyDigest(desired)
		if digestErr != nil {
			return "", nil, digestErr
		}
		if err := InitializeKopiaPolicyStateTx(tx, repo.ID, digest, false); err != nil {
			return "", nil, err
		}
	}
	if connectionIntentID != "" {
		if err := markVaultProfileDirty(tx, repo.ID); err != nil {
			return "", nil, err
		}
	}
	if reviewedUpdate && repo.Engine == engines.KopiaID {
		result, err := tx.Exec(`UPDATE kopia_filesystem_reconnect_intents SET state='cleanup',error='',updated_at=?
			WHERE repository_id=? AND candidate_path=? AND state='committed' AND staged_config_sha256<>''`,
			time.Now().UTC().Format(time.RFC3339Nano), repo.ID, repo.Location)
		if err != nil {
			return "", nil, err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return "", nil, fmt.Errorf("confirmed Kopia connection update is not durably activated")
		}
	}
	if connectionIntentID != "" {
		if _, err := tx.Exec(`DELETE FROM repository_connection_intents WHERE id=?`, connectionIntentID); err != nil {
			return "", nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return "", nil, err
	}
	return repo.ID, []string{repo.ID}, nil
}

// ValidateRecoveredRepositoryAttachment rejects deterministic local conflicts
// before fallback publication changes the remote vault.
func ValidateRecoveredRepositoryAttachment(db *sql.DB, repo models.Repository, jobs []models.BackupJob) error {
	if err := ensureUniqueRepositoryName(db, repo.Name, repo.ID); err != nil {
		return err
	}
	_, _, _, _, err := repositoryPersistenceIdentity(repo)
	if err != nil {
		return err
	}
	if err := AssertVaultUUIDAvailable(db, repo.ID); err != nil {
		return err
	}
	normalizedJobs, err := normalizeRecoveredJobTargets(db, repo.ID, jobs)
	if err != nil {
		return err
	}
	if err := validateRecoveredJobEngineSettings(db, repo.ID, repo.Engine, normalizedJobs); err != nil {
		return err
	}
	jobNames := map[string]bool{}
	for _, job := range normalizedJobs {
		if err := validateJobSourceBinding(job); err != nil {
			return fmt.Errorf("recovered job source storage binding is invalid: %w", err)
		}
		name := strings.TrimSpace(job.Name)
		nameKey := strings.ToLower(name)
		if jobNames[nameKey] {
			return ErrJobNameExists
		}
		jobNames[nameKey] = true
		if err := ensureUniqueJobName(db, name, job.ID); err != nil {
			return err
		}
	}
	return nil
}
