package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/integrations"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/vaultidentity"
)

var ErrRepositoryIdentityExists = fmt.Errorf("vault identity already exists")
var ErrStorageIdentityRequired = fmt.Errorf("filesystem storage binding is required")
var ErrVaultProfilePending = fmt.Errorf("vault recovery profile synchronization changed during removal")

// AssertVaultUUIDAvailable enforces duplicate-registration rejection. Native
// repository IDs may legitimately be shared by copied repositories, while the
// protected vault UUID remains the local registration and locking authority;
// the separately confirmed update path intentionally does not call this gate.
func AssertVaultUUIDAvailable(db *sql.DB, vaultUUID string) error {
	vaultUUID = strings.TrimSpace(vaultUUID)
	if vaultUUID == "" {
		return fmt.Errorf("vault UUID admission is incomplete")
	}
	var name string
	err := db.QueryRow(`SELECT name FROM repositories WHERE id=?`, vaultUUID).Scan(&name)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("This vault is already registered in Replicaro as %s. Review and confirm Update existing vault instead of adding a duplicate.", name)
}

func AssertVaultUUIDAvailableTx(tx *sql.Tx, vaultUUID string) error {
	vaultUUID = strings.TrimSpace(vaultUUID)
	if vaultUUID == "" {
		return fmt.Errorf("vault UUID admission is incomplete")
	}
	var name string
	err := tx.QueryRow(`SELECT name FROM repositories WHERE id=?`, vaultUUID).Scan(&name)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	return fmt.Errorf("This vault is already registered in Replicaro as %s. Review and confirm Update existing vault instead of adding a duplicate.", name)
}

const repoColumns = `
	id, name, engine, connector, cold_storage, archive_write_class, location, canonical_identity, description,
	storage_identity_version, storage_identity_key, storage_identity_json,
	resolved_repository_path, resolved_repository_observed_at,
	passphrase, pending_passphrase, connector_options,
	native_repository_id, profile_uuid, attachment_generation,
	COALESCE((SELECT value FROM settings WHERE key='installationId'), ''),
	vault_size_bytes, vault_size_measured_at, vault_size_dirty, vault_size_last_attempt_at,
	check_schedule, next_check, last_check, last_check_status,
	maintenance_schedule, object_lock_json, next_maintenance, last_maintenance,
	last_maintenance_status, concurrency_mode, auto_unlock, created_at`

func scanRepo(scan func(dest ...any) error) (models.Repository, error) {

	var r models.Repository
	var storedPassphrase string
	var storedPendingPassphrase string
	var optionsJSON string
	var objectLockJSON string

	err := scan(
		&r.ID,
		&r.Name,
		&r.Engine,
		&r.Connector,
		&r.ColdStorage,
		&r.ArchiveWriteClass,
		&r.Location,
		&r.CanonicalIdentity,
		&r.Description,
		&r.StorageIdentityVersion,
		&r.StorageIdentityKey,
		&r.StorageIdentityJSON,
		&r.ResolvedRepositoryPath,
		&r.ResolvedRepositoryObservedAt,
		&storedPassphrase,
		&storedPendingPassphrase,
		&optionsJSON,
		&r.NativeRepositoryID,
		&r.ProfileUUID,
		&r.AttachmentGeneration,
		&r.ClientUUID,
		&r.VaultSizeBytes,
		&r.VaultSizeMeasuredAt,
		&r.VaultSizeDirty,
		&r.VaultSizeLastAttemptAt,
		&r.CheckSchedule,
		&r.NextCheck,
		&r.LastCheck,
		&r.LastCheckStatus,
		&r.MaintenanceSchedule,
		&objectLockJSON,
		&r.NextMaintenance,
		&r.LastMaintenance,
		&r.LastMaintenanceStatus,
		&r.ConcurrencyMode,
		&r.AutoUnlock,
		&r.CreatedAt,
	)
	if err != nil {
		return r, err
	}

	r.Passphrase, r.ConnectorOptions, err = decodeRepositorySecrets(r.Connector, storedPassphrase, optionsJSON)
	if err != nil {
		return r, err
	}
	if storedPendingPassphrase != "" {
		decoded, decodeErr := decodeSecret(storedPendingPassphrase)
		if decodeErr != nil {
			return r, fmt.Errorf("decode pending vault password: %w", decodeErr)
		}
		r.PendingPassphrase = string(decoded)
	}
	r.HasPassword = r.Passphrase != ""
	if err := json.Unmarshal([]byte(objectLockJSON), &r.ObjectLock); err != nil {
		return r, fmt.Errorf("decode repository object lock settings: %w", err)
	}
	r.ObjectLock, err = models.NormalizeObjectLock(r.Engine, r.Connector, r.ObjectLock)
	if err != nil {
		return r, fmt.Errorf("invalid stored object lock settings: %w", err)
	}
	if !models.ValidEngine(r.Engine) {
		return r, fmt.Errorf("unsupported repository engine: %q", r.Engine)
	}
	if !models.ValidConcurrencyMode(r.ConcurrencyMode) {
		return r, fmt.Errorf("invalid stored concurrency mode")
	}
	// Old local rows may retain a formerly accepted high mode. Expose the
	// connector-compatible effective value without a startup rewrite campaign.
	r.ConcurrencyMode, err = models.NormalizeConcurrencyModeForConnector(r.Connector, r.ConcurrencyMode)
	if err != nil {
		return r, fmt.Errorf("invalid stored concurrency mode: %w", err)
	}
	if normalized, normalizeErr := models.NormalizeColdStorage(r.Engine, r.Connector, r.ColdStorage, r.ArchiveWriteClass); normalizeErr != nil || normalized != r.ArchiveWriteClass {
		if normalizeErr != nil {
			return r, fmt.Errorf("invalid stored cold-storage settings: %w", normalizeErr)
		}
		return r, fmt.Errorf("invalid stored cold-storage archive write class")
	}
	if err := validateRepositoryStoredBinding(r); err != nil {
		return r, err
	}
	r.HasCredentials = integrations.HasCredentials(r.Connector, r.ConnectorOptions)
	if engines.IsResticRcloneConnector(r.Connector) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
		r.HasCredentials = engines.RcloneVaultCredentialsReady(ctx, r)
		cancel()
	}
	r.IsNetwork = isNetworkFilesystemLocation(r.Connector, r.Location)
	r.ConnectorLabel = vaultidentity.ConnectorLabel(r.Connector, r.Location, r.ConnectorOptions)
	if r.Connector == "sftp" {
		if effective, resolveErr := vaultidentity.ResolveEffectiveAddress(r.Connector, r.Location, r.ConnectorOptions); resolveErr == nil {
			r.SFTPPathMode = effective.PathMode
		}
	}

	return r, err
}

func encodeRepositoryObjectLock(repo models.Repository) (string, error) {
	normalized, err := models.NormalizeObjectLock(repo.Engine, repo.Connector, repo.ObjectLock)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(normalized)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func SetResolvedRepositoryPath(db *sql.DB, repositoryID, configuredPath, resolvedPath string, observedAt time.Time) error {
	return setResolvedRepositoryPath(db.Exec, repositoryID, configuredPath, resolvedPath, observedAt)
}

func setResolvedRepositoryPath(exec func(string, ...any) (sql.Result, error), repositoryID, configuredPath, resolvedPath string, observedAt time.Time) error {
	stored := resolvedPath
	if stored == configuredPath {
		stored = ""
	}
	result, err := exec(`UPDATE repositories SET resolved_repository_path=?, resolved_repository_observed_at=?
		WHERE id=? AND connector='fs'`, stored, observedAt.UTC().Format(time.RFC3339Nano), repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func ListRepositories(db *sql.DB) ([]models.Repository, error) {

	rows, err := db.Query(
		`SELECT ` + repoColumns + ` FROM repositories ORDER BY name`,
	)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	repos := []models.Repository{}

	for rows.Next() {

		r, err := scanRepo(rows.Scan)

		if err != nil {
			return nil, err
		}

		repos = append(repos, r)
	}

	return repos, nil
}

// TakeStartupStuckResticRepositories returns the exact Restic vaults whose
// in-progress work was failed by Migrate on this application start. The
// handoff is connection-local and non-durable; Restic remains solely
// responsible for deciding whether its own unlock command removes anything.
func TakeStartupStuckResticRepositories(db *sql.DB) ([]models.Repository, error) {
	rows, err := db.Query(`SELECT repository_id FROM startup_stuck_restic_repositories ORDER BY repository_id`)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`DROP TABLE startup_stuck_restic_repositories`); err != nil {
		return nil, err
	}
	repositories := make([]models.Repository, 0, len(ids))
	for _, id := range ids {
		repo, err := GetRepository(db, id)
		if err != nil {
			return nil, err
		}
		repositories = append(repositories, repo)
	}
	return repositories, nil
}

func UpdateRepositoryCredentials(db *sql.DB, repositoryID string, options map[string]string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireRepositoryMutationUnreserved(tx, repositoryID); err != nil {
		return err
	}
	var connector string
	if err := tx.QueryRow(`SELECT connector FROM repositories WHERE id=?`, repositoryID).Scan(&connector); err != nil {
		return err
	}
	encoded, err := transformClassifiedOptions(connector, options, true)
	if err != nil {
		return err
	}
	optionsJSON, err := json.Marshal(encoded)
	if err != nil {
		return fmt.Errorf("encode connector credentials")
	}
	result, err := tx.Exec(`UPDATE repositories SET connector_options = ? WHERE id = ?`, string(optionsJSON), repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func UpdateRepositoryPassphrase(db *sql.DB, repositoryID, passphrase string) error {
	if err := models.ValidateVaultPassword(passphrase); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireRepositoryMutationUnreserved(tx, repositoryID); err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE repositories SET passphrase=? WHERE id=?`,
		encodeSecret([]byte(passphrase)), repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func UpdateRepositorySecrets(db *sql.DB, repositoryID, passphrase string, options map[string]string) error {
	if err := models.ValidateVaultPassword(passphrase); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireRepositoryMutationUnreserved(tx, repositoryID); err != nil {
		return err
	}
	var connector string
	if err := tx.QueryRow(`SELECT connector FROM repositories WHERE id=?`, repositoryID).Scan(&connector); err != nil {
		return err
	}
	encodedPassphrase, optionsJSON, err := encodeRepositorySecrets(connector, passphrase, options)
	if err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE repositories SET passphrase=?,connector_options=? WHERE id=?`,
		encodedPassphrase, optionsJSON, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

// RepairMissingRepositoryProfileBinding restores only an absent local pointer
// after remote authority has independently proven one exact attachment.
func RepairMissingRepositoryProfileBinding(db *sql.DB, repositoryID, clientUUID, profileUUID string, generation int64) error {
	if strings.TrimSpace(repositoryID) == "" || strings.TrimSpace(clientUUID) == "" ||
		strings.TrimSpace(profileUUID) == "" || generation < 1 {
		return fmt.Errorf("repository profile repair identity is incomplete")
	}
	result, err := db.Exec(`UPDATE repositories SET profile_uuid=?, attachment_generation=?
		WHERE id=? AND (trim(profile_uuid)='' OR attachment_generation<1)
		AND (SELECT value FROM settings WHERE key='installationId')=?`,
		profileUUID, generation, repositoryID, clientUUID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("repository profile binding changed before repair")
	}
	return nil
}

func GetRepository(
	db *sql.DB,
	id string,
) (models.Repository, error) {

	row := db.QueryRow(
		`SELECT `+repoColumns+` FROM repositories WHERE id = ?`,
		id,
	)

	return scanRepo(row.Scan)
}

func DeleteRepository(db *sql.DB, id string) error {
	return deleteRepository(db, id, false)
}

// DeleteRepositoryDiscardingPendingProfile performs the same local-only
// removal while explicitly accepting that the latest recovery profile could
// not be published. Every other deletion safeguard remains unchanged.
func DeleteRepositoryDiscardingPendingProfile(db *sql.DB, id string) error {
	return deleteRepository(db, id, true)
}

func deleteRepository(db *sql.DB, id string, discardPendingProfile bool) error {
	cacheRemoval, reopenCache, err := closeMetadataCacheForRemoval(db, id)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			reopenCache()
		}
	}()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireRepositoryConnectionUnreserved(tx, id); err != nil {
		return err
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE repository_id = ? AND status IN ('queued', 'running')`, id).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrJobRunActive
	}
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM repositories WHERE id = ?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return sql.ErrNoRows
	}
	var profilePending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM vault_profile_sync WHERE repository_id = ?`, id).Scan(&profilePending); err != nil {
		return err
	}
	if profilePending != 0 && !discardPendingProfile {
		return ErrVaultProfilePending
	}
	rows, err := tx.Query(`SELECT DISTINCT repository_id FROM backup_job_targets
		WHERE job_id IN (SELECT job_id FROM backup_job_targets WHERE repository_id = ?)
		AND repository_id <> ?`, id, id)
	if err != nil {
		return err
	}
	remainingTargets := []string{}
	for rows.Next() {
		var repositoryID string
		if err := rows.Scan(&repositoryID); err != nil {
			_ = rows.Close()
			return err
		}
		remainingTargets = append(remainingTargets, repositoryID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	jobRows, err := tx.Query(`SELECT id, portable_target_ids FROM backup_jobs`)
	if err != nil {
		return err
	}
	type portableJobUpdate struct{ id, targets string }
	jobUpdates := []portableJobUpdate{}
	for jobRows.Next() {
		var jobID, encoded string
		if err := jobRows.Scan(&jobID, &encoded); err != nil {
			_ = jobRows.Close()
			return err
		}
		var targets []string
		if err := json.Unmarshal([]byte(encoded), &targets); err != nil {
			_ = jobRows.Close()
			return fmt.Errorf("decode portable targets for job %s: %w", jobID, err)
		}
		updatedTargets := removePortableTarget(targets, id)
		updated, err := json.Marshal(updatedTargets)
		if err != nil {
			_ = jobRows.Close()
			return err
		}
		if len(updatedTargets) != len(targets) {
			jobUpdates = append(jobUpdates, portableJobUpdate{jobID, string(updated)})
		}
	}
	if err := jobRows.Close(); err != nil {
		return err
	}
	for _, update := range jobUpdates {
		if _, err := tx.Exec(`UPDATE backup_jobs SET portable_target_ids = ? WHERE id = ?`, update.targets, update.id); err != nil {
			return err
		}
		activeRows, err := tx.Query(`SELECT repository_id FROM backup_job_targets WHERE job_id = ? AND repository_id <> ?`, update.id, id)
		if err != nil {
			return err
		}
		for activeRows.Next() {
			var repositoryID string
			if err := activeRows.Scan(&repositoryID); err != nil {
				_ = activeRows.Close()
				return err
			}
			remainingTargets = append(remainingTargets, repositoryID)
		}
		if err := activeRows.Close(); err != nil {
			return err
		}
	}
	dormantRows, err := tx.Query(`SELECT repository_id, job_id, definition_json FROM dormant_recovery_jobs`)
	if err != nil {
		return err
	}
	type dormantUpdate struct{ repositoryID, jobID, definition string }
	dormantUpdates := []dormantUpdate{}
	for dormantRows.Next() {
		var ownerID, jobID, encoded string
		if err := dormantRows.Scan(&ownerID, &jobID, &encoded); err != nil {
			_ = dormantRows.Close()
			return err
		}
		definition, err := decodePortableDefinition(encoded)
		if err != nil {
			_ = dormantRows.Close()
			return fmt.Errorf("decode dormant portable job %s: %w", jobID, err)
		}
		updatedTargets := removePortableTarget(definition.TargetVaultUUIDs, id)
		if len(updatedTargets) == len(definition.TargetVaultUUIDs) {
			continue
		}
		definition.TargetVaultUUIDs = updatedTargets
		updated, err := json.Marshal(definition)
		if err != nil {
			_ = dormantRows.Close()
			return err
		}
		dormantUpdates = append(dormantUpdates, dormantUpdate{ownerID, jobID, string(updated)})
		if ownerID != id {
			remainingTargets = append(remainingTargets, ownerID)
		}
	}
	if err := dormantRows.Close(); err != nil {
		return err
	}
	for _, update := range dormantUpdates {
		if _, err := tx.Exec(`UPDATE dormant_recovery_jobs SET definition_json = ? WHERE repository_id = ? AND job_id = ?`,
			update.definition, update.repositoryID, update.jobID); err != nil {
			return err
		}
	}
	// Remove only this membership. A logical job is deleted only when its
	// final target disappears; operations intentionally remain untouched.
	if _, err := tx.Exec(`DELETE FROM backup_job_targets WHERE repository_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM backup_jobs WHERE NOT EXISTS
		(SELECT 1 FROM backup_job_targets t WHERE t.job_id = backup_jobs.id)`); err != nil {
		return err
	}
	// Repository deletion uses the repository FK cascade. It intentionally
	// bypasses targeted snapshot orphan analysis because the complete cache is
	// being discarded.
	if _, err := tx.Exec(`DELETE FROM repositories WHERE id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO activity_log (timestamp, level, message) VALUES (datetime('now'), 'INFO', 'Repository deleted')`); err != nil {
		return err
	}
	if err := markVaultProfilesDirty(tx, remainingTargets); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	committed = true
	retireMetadataCacheRemoval(cacheRemoval)
	if err := removeClosedMetadataCache(cacheRemoval); err != nil {
		return &MetadataCacheCleanupError{Err: err}
	}
	return nil
}

// CheckRepositoryDeletionEligibility rejects known failures before callers
// stage local artifacts. DeleteRepository repeats these checks transactionally.
func CheckRepositoryDeletionEligibility(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireRepositoryConnectionUnreserved(tx, id); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM repositories WHERE id = ?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return sql.ErrNoRows
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE repository_id = ? AND status IN ('queued', 'running')`, id).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrJobRunActive
	}
	return nil
}

func RepositoryVaultSizeFresh(repo models.Repository, now time.Time) bool {
	if repo.VaultSizeBytes == nil || repo.VaultSizeMeasuredAt == "" || repo.VaultSizeDirty {
		return false
	}
	updatedAt, err := time.Parse(time.RFC3339Nano, repo.VaultSizeMeasuredAt)
	if err != nil || updatedAt.After(now) {
		return false
	}
	return now.Sub(updatedAt) < 10*24*time.Hour
}

func RecordVaultSizeAttempt(db *sql.DB, id string, attemptedAt time.Time) error {
	result, err := db.Exec(`UPDATE repositories SET vault_size_last_attempt_at=? WHERE id=?`,
		attemptedAt.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func PublishVaultSize(db *sql.DB, id string, sizeBytes int64, measuredAt time.Time) error {
	if sizeBytes < 0 {
		return fmt.Errorf("vault size cannot be negative")
	}
	result, err := db.Exec(`UPDATE repositories SET vault_size_bytes=?, vault_size_measured_at=?, vault_size_dirty=0 WHERE id=?`,
		sizeBytes, measuredAt.UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func MarkVaultSizeDirty(db *sql.DB, id string) error {
	result, err := db.Exec(`UPDATE repositories SET vault_size_dirty=1 WHERE id=?`, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func UpdateRepositorySchedules(db *sql.DB, id, checkSchedule, maintenanceSchedule string) error {
	repo, err := GetRepository(db, id)
	if err != nil {
		return err
	}
	return UpdateRepositorySettings(db, id, checkSchedule, maintenanceSchedule, repo.AutoUnlock)
}

func DisableRepositoryMaintenance(db *sql.DB, id, reason string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var schedule, objectLockJSON string
	if err := tx.QueryRow(`SELECT maintenance_schedule,object_lock_json FROM repositories WHERE id=?`, id).
		Scan(&schedule, &objectLockJSON); err != nil {
		return err
	}
	var objectLock models.ObjectLockSettings
	if err := json.Unmarshal([]byte(objectLockJSON), &objectLock); err != nil {
		return fmt.Errorf("decode stored object lock settings: %w", err)
	}
	status := "blocked: " + strings.TrimSpace(reason)
	var result sql.Result
	if objectLock.Enrolled {
		if !models.ObjectLockMaintenanceEligible(objectLock, schedule) {
			return fmt.Errorf("space reclamation schedule is incompatible with object lock state")
		}
		next := NextRunFrom(schedule, time.Now())
		if !objectLock.Paused {
			// A transient owner/storage revalidation failure must not silently turn
			// off the maintenance that renews provider locks. The shortest allowed
			// retention/schedule pair has only a 24-hour extension margin, so retry
			// hourly instead of waiting another complete schedule interval.
			next = objectLockMaintenanceRetryAt(time.Now())
		}
		result, err = tx.Exec(`UPDATE repositories
			SET next_maintenance=?,last_maintenance_status=? WHERE id=?`,
			next, status, id)
	} else {
		result, err = tx.Exec(`UPDATE repositories
			SET maintenance_schedule='manual',next_maintenance='',last_maintenance_status=? WHERE id=?`, status, id)
	}
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func DisableRepositoryIntegrity(db *sql.DB, id, reason string) error {
	status := "blocked: " + strings.TrimSpace(reason)
	result, err := db.Exec(`UPDATE repositories
		SET check_schedule='manual',next_check='',last_check_status=? WHERE id=?`, status, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

const objectLockMaintenanceRetryDelay = time.Hour

func objectLockMaintenanceRetryAt(after time.Time) string {
	return after.UTC().Add(objectLockMaintenanceRetryDelay).Format(time.RFC3339)
}

func UpdateRepositorySettings(db *sql.DB, id, checkSchedule, maintenanceSchedule string, autoUnlock bool) error {
	repo, err := GetRepository(db, id)
	if err != nil {
		return err
	}
	return updateRepositorySettings(db, id, checkSchedule, maintenanceSchedule, repo.ConcurrencyMode, autoUnlock, nil)
}

func UpdateRepositorySettingsWithObjectLock(db *sql.DB, id, checkSchedule, maintenanceSchedule string, autoUnlock bool, objectLock models.ObjectLockSettings) error {
	repo, err := GetRepository(db, id)
	if err != nil {
		return err
	}
	return updateRepositorySettings(db, id, checkSchedule, maintenanceSchedule, repo.ConcurrencyMode, autoUnlock, &objectLock)
}

func UpdateRepositorySettingsWithObjectLockAndConcurrency(db *sql.DB, id, checkSchedule, maintenanceSchedule, concurrencyMode string, autoUnlock bool, objectLock models.ObjectLockSettings) error {
	return updateRepositorySettings(db, id, checkSchedule, maintenanceSchedule, concurrencyMode, autoUnlock, &objectLock)
}

// UpdateRepositoryLocalPreferences changes only profile-owned settings. In
// particular, it must not echo schedules from an earlier API read: an owner
// loss may have disabled integrity between that read and this write.
func UpdateRepositoryLocalPreferences(db *sql.DB, id, concurrencyMode string, autoUnlock bool) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireRepositoryMutationUnreserved(tx, id); err != nil {
		return err
	}
	var connector string
	if err := tx.QueryRow(`SELECT connector FROM repositories WHERE id=?`, id).Scan(&connector); err != nil {
		return err
	}
	concurrencyMode, err = models.NormalizeConcurrencyModeForConnector(connector, concurrencyMode)
	if err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE repositories SET concurrency_mode=?,auto_unlock=? WHERE id=?`, concurrencyMode, autoUnlock, id)
	if err != nil {
		return err
	}
	if count, countErr := result.RowsAffected(); countErr != nil {
		return countErr
	} else if count != 1 {
		return sql.ErrNoRows
	}
	if err := markVaultProfileDirty(tx, id); err != nil {
		return err
	}
	return tx.Commit()
}

// AdoptRepositoryVaultCare copies authority already validated from the
// protected root into the installation-local scheduler state. This is not an
// object-lock transition requested by the local profile, so it deliberately
// does not apply the forward-only transition rules used by settings edits.
// Concurrency and auto-unlock remain local profile preferences.
func AdoptRepositoryVaultCare(db *sql.DB, id, checkSchedule, maintenanceSchedule string, objectLock models.ObjectLockSettings) error {
	if !ValidSchedule(checkSchedule) || !ValidSchedule(maintenanceSchedule) {
		return fmt.Errorf("invalid repository task schedule")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireRepositoryMutationUnreserved(tx, id); err != nil {
		return err
	}
	var engine, connector string
	var coldStorage bool
	if err := tx.QueryRow(`SELECT engine,connector,cold_storage FROM repositories WHERE id=?`, id).Scan(&engine, &connector, &coldStorage); err != nil {
		return err
	}
	if coldStorage && checkSchedule != "manual" {
		return fmt.Errorf("%s", models.ColdStorageIntegrityHelp)
	}
	objectLock, err = models.NormalizeObjectLock(engine, connector, objectLock)
	if err != nil {
		return err
	}
	if !models.ObjectLockMaintenanceEligible(objectLock, maintenanceSchedule) {
		return fmt.Errorf("space reclamation schedule is incompatible with object lock duration")
	}
	objectLockJSON, err := json.Marshal(objectLock)
	if err != nil {
		return err
	}
	now := time.Now()
	result, err := tx.Exec(`UPDATE repositories SET check_schedule=?,next_check=?,maintenance_schedule=?,object_lock_json=?,next_maintenance=? WHERE id=?`,
		checkSchedule, NextRunFrom(checkSchedule, now), maintenanceSchedule, string(objectLockJSON), NextRunFrom(maintenanceSchedule, now), id)
	if err != nil {
		return err
	}
	if count, countErr := result.RowsAffected(); countErr != nil {
		return countErr
	} else if count != 1 {
		return sql.ErrNoRows
	}
	if engine == engines.KopiaID {
		if _, err := RefreshKopiaPolicyStatesTx(tx, []string{id}); err != nil {
			return err
		}
		// A former non-owner may have an equal-digest terminal error from its
		// deliberately rejected reconciliation attempt. Takeover must reopen the
		// exact policy state, but it must not displace an active durable fence.
		result, err := tx.Exec(`UPDATE kopia_policy_state SET state='dirty',applied_digest='',last_error=''
			WHERE repository_id=? AND state<>'applying' AND active_fence_path=''`, id)
		if err != nil {
			return err
		}
		if count, countErr := result.RowsAffected(); countErr != nil {
			return countErr
		} else if count != 1 {
			return fmt.Errorf("%w: policy verification cannot be reopened while reconciliation is active", ErrKopiaPolicyNotReady)
		}
	}
	return tx.Commit()
}

func updateRepositorySettings(db *sql.DB, id, checkSchedule, maintenanceSchedule, concurrencyMode string, autoUnlock bool, requestedObjectLock *models.ObjectLockSettings) error {
	// Vault-care schedules are independent from backup-target execution, so
	// editing them remains safe while a backup for this vault is active.
	if !ValidSchedule(checkSchedule) || !ValidSchedule(maintenanceSchedule) {
		return fmt.Errorf("invalid repository task schedule")
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireRepositoryMutationUnreserved(tx, id); err != nil {
		return err
	}
	var coldStorage bool
	var engine, connector, storedObjectLockJSON string
	if err := tx.QueryRow(`SELECT cold_storage,engine,connector,object_lock_json FROM repositories WHERE id=?`, id).
		Scan(&coldStorage, &engine, &connector, &storedObjectLockJSON); err != nil {
		return err
	}
	concurrencyMode, err = models.NormalizeConcurrencyModeForConnector(connector, concurrencyMode)
	if err != nil {
		return err
	}
	if coldStorage && checkSchedule != "manual" {
		return fmt.Errorf("%s", models.ColdStorageIntegrityHelp)
	}
	var storedObjectLock models.ObjectLockSettings
	if err := json.Unmarshal([]byte(storedObjectLockJSON), &storedObjectLock); err != nil {
		return fmt.Errorf("decode stored object lock settings: %w", err)
	}
	storedObjectLock, err = models.NormalizeObjectLock(engine, connector, storedObjectLock)
	if err != nil {
		return err
	}
	objectLock := storedObjectLock
	if requestedObjectLock != nil {
		objectLock = *requestedObjectLock
	}
	objectLock, err = models.NormalizeObjectLock(engine, connector, objectLock)
	if err != nil {
		return err
	}
	if err := models.ValidateObjectLockTransition(storedObjectLock, objectLock); err != nil {
		return err
	}
	if !models.ObjectLockMaintenanceEligible(objectLock, maintenanceSchedule) {
		return fmt.Errorf("space reclamation schedule is incompatible with object lock duration")
	}
	objectLockJSON, err := json.Marshal(objectLock)
	if err != nil {
		return err
	}
	now := time.Now()
	result, err := tx.Exec(
		`UPDATE repositories SET check_schedule = ?, next_check = ?,
		 maintenance_schedule = ?, object_lock_json = ?, next_maintenance = ?, concurrency_mode = ?, auto_unlock = ? WHERE id = ?`,
		checkSchedule, NextRunFrom(checkSchedule, now),
		maintenanceSchedule, string(objectLockJSON), NextRunFrom(maintenanceSchedule, now), concurrencyMode, autoUnlock, id,
	)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err == nil && rows == 0 {
		return sql.ErrNoRows
	}
	if err != nil {
		return err
	}
	if err := markVaultProfileDirty(tx, id); err != nil {
		return err
	}
	if engine == engines.KopiaID {
		if _, err := RefreshKopiaPolicyStatesTx(tx, []string{id}); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func DueRepositoryChecks(db *sql.DB) ([]models.Repository, error) {
	return dueRepositories(db, "check")
}

func DueRepositoryMaintenance(db *sql.DB) ([]models.Repository, error) {
	return dueRepositories(db, "maintenance")
}

func dueRepositories(db *sql.DB, operation string) ([]models.Repository, error) {
	repos, err := ListRepositories(db)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	due := []models.Repository{}
	for _, repo := range repos {
		if operation == "check" && repo.ColdStorage {
			continue
		}
		schedule, next := repo.CheckSchedule, repo.NextCheck
		if operation == "maintenance" {
			schedule, next = repo.MaintenanceSchedule, repo.NextMaintenance
		}
		if schedule == "" || schedule == "manual" || next == "" {
			continue
		}
		when, err := time.Parse(time.RFC3339, next)
		if err == nil && !when.After(now) {
			due = append(due, repo)
		}
	}
	return due, nil
}

func SetRepositoryOperationStatus(db *sql.DB, id, operation, status string) error {
	column := "last_check_status"
	if operation == "maintenance" {
		column = "last_maintenance_status"
	}
	_, err := db.Exec(`UPDATE repositories SET `+column+` = ? WHERE id = ?`, status, id)
	return err
}

func MarkRepositoryOperation(db *sql.DB, id, operation, schedule, status string) error {
	now := time.Now()
	lastColumn, nextColumn, statusColumn := "last_check", "next_check", "last_check_status"
	if operation == "maintenance" {
		lastColumn, nextColumn, statusColumn = "last_maintenance", "next_maintenance", "last_maintenance_status"
	}
	_, err := db.Exec(
		`UPDATE repositories SET `+lastColumn+` = ?, `+nextColumn+` = ?, `+statusColumn+` = ? WHERE id = ?`,
		now.Format(time.RFC3339), NextRunFrom(schedule, now), status, id,
	)
	return err
}
