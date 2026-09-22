package database

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/vaultidentity"
	"github.com/local/replicaro/vaultprofile"
)

const (
	RepositoryCreationPrepared      = "prepared"
	RepositoryCreationNativeStarted = "native_started"
	RepositoryCreationNativeReady   = "native_ready"
)

type RepositoryCreationIntent struct {
	ID                     string                    `json:"id"`
	CanonicalIdentity      string                    `json:"canonicalIdentity"`
	StorageIdentityVersion string                    `json:"-"`
	StorageIdentityKey     string                    `json:"-"`
	StorageIdentityJSON    string                    `json:"-"`
	Engine                 string                    `json:"engine"`
	Connector              string                    `json:"connector"`
	ColdStorage            bool                      `json:"coldStorage"`
	ArchiveWriteClass      string                    `json:"archiveWriteClass,omitempty"`
	Location               string                    `json:"location"`
	Name                   string                    `json:"name"`
	Description            string                    `json:"description"`
	CheckSchedule          string                    `json:"checkSchedule"`
	MaintenanceSchedule    string                    `json:"maintenanceSchedule"`
	ConcurrencyMode        string                    `json:"concurrencyMode"`
	ObjectLock             models.ObjectLockSettings `json:"objectLock"`
	PublicationOperationID string                    `json:"-"`
	ReviewedOptions        map[string]string         `json:"reviewedOptions,omitempty"`
	ReviewedOptionsJSON    string                    `json:"-"`
	ProfileSHA256          string                    `json:"-"`
	ProfileJSON            string                    `json:"-"`
	NativeFingerprint      string                    `json:"-"`
	NativeOperationID      string                    `json:"-"`
	Phase                  string                    `json:"phase"`
	LastError              string                    `json:"lastError,omitempty"`
	CreatedAt              string                    `json:"createdAt"`
	UpdatedAt              string                    `json:"updatedAt"`
}

var ErrRepositoryCreationSettingsMismatch = fmt.Errorf("creation retry differs from the original reviewed vault settings")
var ErrRepositoryCreationPending = fmt.Errorf("a pending creation already owns this vault location")

func NativeOperationFencePath(db *sql.DB, operationID string) (string, error) {
	if !canonicalUUID(operationID) {
		return "", fmt.Errorf("native operation fence identity must be a canonical UUID")
	}
	var sequence int
	var name, databasePath string
	if err := db.QueryRow(`PRAGMA database_list`).Scan(&sequence, &name, &databasePath); err != nil {
		return "", fmt.Errorf("resolve database path for native operation fence: %w", err)
	}
	if databasePath == "" {
		return "", fmt.Errorf("native operation fence identity is incomplete")
	}
	return filepath.Join(filepath.Dir(databasePath), ".native-operations", operationID+".lock"), nil
}

func canonicalUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

// Repository creation keeps the local inherited process fence because a
// native child or descendant can outlive the request. Its path is derived from
// the one durable operation UUID rather than becoming another source of truth.
func RepositoryCreationFencePath(db *sql.DB, operationID string) (string, error) {
	return NativeOperationFencePath(db, operationID)
}

// NativeOperationFenceReferenced checks durable consumers before a closed
// native-operation proof is removed.
func NativeOperationFenceReferenced(db *sql.DB, fencePath string) (bool, error) {
	if fencePath == "" {
		return false, fmt.Errorf("native operation fence path is required")
	}
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM kopia_policy_state WHERE active_fence_path=?)`, fencePath).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return true, nil
	}
	rows, err := db.Query(`SELECT native_operation_id FROM repository_creation_intents
		WHERE phase IN ('native_started','native_ready') AND native_operation_id<>''`)
	if err != nil {
		return false, err
	}
	creationOperations := []string{}
	for rows.Next() {
		var operationID string
		if err := rows.Scan(&operationID); err != nil {
			_ = rows.Close()
			return false, err
		}
		creationOperations = append(creationOperations, operationID)
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	if err := rows.Err(); err != nil {
		return false, err
	}
	for _, operationID := range creationOperations {
		creationFence, err := RepositoryCreationFencePath(db, operationID)
		if err != nil {
			return false, fmt.Errorf("derive repository creation fence reference: %w", err)
		}
		if creationFence == fencePath {
			return true, nil
		}
	}
	rows, err = db.Query(`SELECT payload_json FROM repository_connection_intents`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return false, err
		}
		var value any
		if err := json.Unmarshal([]byte(payload), &value); err != nil {
			return false, fmt.Errorf("decode repository connection fence state: %w", err)
		}
		if jsonValueContainsExactString(value, fencePath) {
			return true, nil
		}
	}
	return false, rows.Err()
}

func jsonValueContainsExactString(value any, target string) bool {
	switch typed := value.(type) {
	case string:
		return typed == target
	case []any:
		for _, item := range typed {
			if jsonValueContainsExactString(item, target) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			if jsonValueContainsExactString(item, target) {
				return true
			}
		}
	}
	return false
}

const creationIntentColumns = `id, canonical_identity, storage_identity_version,
	storage_identity_key, storage_identity_json, engine, connector, cold_storage, archive_write_class, location, name,
	description, check_schedule, maintenance_schedule, concurrency_mode, object_lock_json, publication_operation_id,
	reviewed_options_json, profile_sha256, profile_json, native_fingerprint,
	native_operation_id, phase, last_error, created_at, updated_at`

func scanCreationIntent(scan func(...any) error) (RepositoryCreationIntent, error) {
	var value RepositoryCreationIntent
	var objectLockJSON string
	err := scan(&value.ID, &value.CanonicalIdentity, &value.StorageIdentityVersion,
		&value.StorageIdentityKey, &value.StorageIdentityJSON, &value.Engine, &value.Connector,
		&value.ColdStorage, &value.ArchiveWriteClass, &value.Location, &value.Name,
		&value.Description, &value.CheckSchedule, &value.MaintenanceSchedule, &value.ConcurrencyMode, &objectLockJSON,
		&value.PublicationOperationID, &value.ReviewedOptionsJSON, &value.ProfileSHA256,
		&value.ProfileJSON, &value.NativeFingerprint, &value.NativeOperationID,
		&value.Phase, &value.LastError, &value.CreatedAt, &value.UpdatedAt)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal([]byte(objectLockJSON), &value.ObjectLock); err != nil {
		return value, fmt.Errorf("decode pending creation object lock settings: %w", err)
	}
	value.ObjectLock, err = models.NormalizeObjectLock(value.Engine, value.Connector, value.ObjectLock)
	if err != nil {
		return value, fmt.Errorf("invalid pending creation object lock settings: %w", err)
	}
	if !models.ValidConcurrencyMode(value.ConcurrencyMode) {
		return value, fmt.Errorf("invalid pending creation concurrency mode")
	}
	value.ConcurrencyMode, err = models.NormalizeConcurrencyModeForConnector(value.Connector, value.ConcurrencyMode)
	if err != nil {
		return value, fmt.Errorf("invalid pending creation concurrency mode: %w", err)
	}
	if err := json.Unmarshal([]byte(value.ReviewedOptionsJSON), &value.ReviewedOptions); err != nil {
		return value, fmt.Errorf("decode reviewed connector options: %w", err)
	}
	if value.ProfileJSON != "" {
		decoded, err := decodeSecret(value.ProfileJSON)
		if err != nil {
			return value, fmt.Errorf("decode pending creation profile: %w", err)
		}
		value.ProfileJSON = string(decoded)
	}
	return value, nil
}

type normalizedCreation struct {
	repo                                 models.Repository
	identity, storageVersion, storageKey string
	storageJSON, check, maintenance      string
	objectLockJSON                       string
	reviewedJSON                         string
}

func normalizeCreation(repo models.Repository, reviewedOptions ...map[string]string) (normalizedCreation, error) {
	repo.Name = strings.TrimSpace(repo.Name)
	archiveWriteClass, err := models.NormalizeColdStorage(repo.Engine, repo.Connector, repo.ColdStorage, repo.ArchiveWriteClass)
	if err != nil {
		return normalizedCreation{}, err
	}
	repo.ArchiveWriteClass = archiveWriteClass
	identity, storageVersion, storageKey, storageJSON, err := repositoryPersistenceIdentity(repo)
	if err != nil {
		return normalizedCreation{}, err
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
		return normalizedCreation{}, err
	}
	repo.ConcurrencyMode = concurrencyMode
	repo.ObjectLock, err = models.NormalizeObjectLock(repo.Engine, repo.Connector, repo.ObjectLock)
	if err != nil {
		return normalizedCreation{}, err
	}
	if !models.ObjectLockMaintenanceEligible(repo.ObjectLock, maintenance) {
		return normalizedCreation{}, fmt.Errorf("space reclamation schedule is incompatible with object lock duration")
	}
	objectLockJSON, err := encodeRepositoryObjectLock(repo)
	if err != nil {
		return normalizedCreation{}, err
	}
	reviewed := map[string]string{}
	if len(reviewedOptions) > 0 && reviewedOptions[0] != nil {
		reviewed = reviewedOptions[0]
	}
	reviewedJSON, err := json.Marshal(reviewed)
	if err != nil {
		return normalizedCreation{}, fmt.Errorf("encode reviewed connector options: %w", err)
	}
	return normalizedCreation{repo: repo, identity: identity, storageVersion: storageVersion,
		storageKey: storageKey, storageJSON: storageJSON, check: check,
		maintenance: maintenance, objectLockJSON: objectLockJSON, reviewedJSON: string(reviewedJSON)}, nil
}

// ReserveRepositoryCreation creates the one recovery intent at the real
// non-atomic boundary between native creation, protected publication, and
// local attachment. The configured destination is not durable repository
// identity, but it remains the narrow reservation key until native identity
// and the protected vault UUID exist; otherwise concurrent creates could each
// start a different native repository at the same destination.
func ReserveRepositoryCreation(db *sql.DB, repo models.Repository, reviewedOptions ...map[string]string) (RepositoryCreationIntent, error) {
	normalized, err := normalizeCreation(repo, reviewedOptions...)
	if err != nil {
		return RepositoryCreationIntent{}, err
	}
	tx, err := db.Begin()
	if err != nil {
		return RepositoryCreationIntent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var pending int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM repository_creation_intents WHERE canonical_identity=?`, normalized.identity).Scan(&pending); err != nil {
		return RepositoryCreationIntent{}, err
	}
	if pending != 0 {
		return RepositoryCreationIntent{}, ErrRepositoryCreationPending
	}
	// Exact IDs retain retry idempotency independently of the destination
	// reservation above.
	if normalized.repo.ID != "" {
		var exact int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM repository_creation_intents WHERE id=?`, normalized.repo.ID).Scan(&exact); err != nil {
			return RepositoryCreationIntent{}, err
		}
		if exact != 0 {
			return RepositoryCreationIntent{}, ErrRepositoryCreationPending
		}
	}
	if err := ensureUniqueRepositoryName(tx, normalized.repo.Name, normalized.repo.ID); err != nil {
		return RepositoryCreationIntent{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := normalized.repo.ID
	if id == "" {
		id = uuid.NewString()
	}
	_, err = tx.Exec(`INSERT INTO repository_creation_intents
		(id,canonical_identity,storage_identity_version,storage_identity_key,storage_identity_json,
		 engine,connector,cold_storage,archive_write_class,location,name,description,
		 check_schedule,maintenance_schedule,concurrency_mode,object_lock_json,publication_operation_id,reviewed_options_json,
		 profile_sha256,profile_json,native_fingerprint,native_operation_id,phase,last_error,created_at,updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'','','','','prepared','',?,?)`,
		id, normalized.identity, normalized.storageVersion, normalized.storageKey,
		normalized.storageJSON, normalized.repo.Engine, normalized.repo.Connector,
		normalized.repo.ColdStorage, normalized.repo.ArchiveWriteClass,
		normalized.repo.Location, normalized.repo.Name, normalized.repo.Description,
		normalized.check, normalized.maintenance, normalized.repo.ConcurrencyMode, normalized.objectLockJSON, uuid.NewString(), normalized.reviewedJSON,
		now, now)
	if err != nil {
		return RepositoryCreationIntent{}, err
	}
	result, err := scanCreationIntent(tx.QueryRow(`SELECT `+creationIntentColumns+` FROM repository_creation_intents WHERE id=?`, id).Scan)
	if err != nil {
		return RepositoryCreationIntent{}, err
	}
	if err := tx.Commit(); err != nil {
		return RepositoryCreationIntent{}, err
	}
	return result, nil
}

func LoadRepositoryCreationRetry(db *sql.DB, id string, repo models.Repository, reviewedOptions ...map[string]string) (RepositoryCreationIntent, error) {
	normalized, err := normalizeCreation(repo, reviewedOptions...)
	if err != nil {
		return RepositoryCreationIntent{}, err
	}
	intent, err := FindRepositoryCreationIntentByID(db, strings.TrimSpace(id))
	if err != nil {
		return RepositoryCreationIntent{}, err
	}
	if intent.CanonicalIdentity != normalized.identity ||
		intent.StorageIdentityVersion != normalized.storageVersion ||
		intent.StorageIdentityKey != normalized.storageKey || intent.StorageIdentityJSON != normalized.storageJSON ||
		intent.Engine != normalized.repo.Engine || intent.Connector != normalized.repo.Connector ||
		intent.ColdStorage != normalized.repo.ColdStorage || intent.ArchiveWriteClass != normalized.repo.ArchiveWriteClass ||
		intent.Name != normalized.repo.Name || intent.Description != normalized.repo.Description ||
		intent.CheckSchedule != normalized.check || intent.MaintenanceSchedule != normalized.maintenance ||
		intent.ConcurrencyMode != normalized.repo.ConcurrencyMode ||
		intent.ObjectLock != normalized.repo.ObjectLock ||
		intent.ReviewedOptionsJSON != normalized.reviewedJSON {
		return RepositoryCreationIntent{}, ErrRepositoryCreationSettingsMismatch
	}
	return intent, nil
}

func CancelPreparedRepositoryCreation(db *sql.DB, id string) error {
	result, err := db.Exec(`DELETE FROM repository_creation_intents WHERE id=? AND phase='prepared'`, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("only a prepared repository creation can be cancelled")
	}
	return nil
}

// ForgetRepositoryCreationIntent removes no remote repository content and is
// called only after the API proves the one native operation is inactive and
// removes the exact local engine and rclone artifacts.
func ForgetRepositoryCreationIntent(db *sql.DB, id string) error {
	result, err := db.Exec(`DELETE FROM repository_creation_intents
		WHERE id=? AND phase IN ('native_started','native_ready')`, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("repository creation is not eligible to be forgotten")
	}
	return nil
}

func PrepareRepositoryCreationProfile(db *sql.DB, id, hash, profileJSON string) (RepositoryCreationIntent, error) {
	if hash == "" || profileJSON == "" {
		return RepositoryCreationIntent{}, fmt.Errorf("creation profile is incomplete")
	}
	protectedProfile := encodeSecret([]byte(profileJSON))
	result, err := db.Exec(`UPDATE repository_creation_intents SET profile_sha256=?,profile_json=?,updated_at=?
		WHERE id=? AND phase='prepared' AND ((profile_sha256='' AND profile_json='') OR (profile_sha256=? AND profile_json=?))`,
		hash, protectedProfile, time.Now().UTC().Format(time.RFC3339Nano), id, hash, protectedProfile)
	if err != nil {
		return RepositoryCreationIntent{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return RepositoryCreationIntent{}, fmt.Errorf("creation profile differs from the pending operation")
	}
	return FindRepositoryCreationIntentByID(db, id)
}

func BeginRepositoryCreationNative(db *sql.DB, id, operationID string) (RepositoryCreationIntent, error) {
	if !canonicalUUID(operationID) {
		return RepositoryCreationIntent{}, fmt.Errorf("native creation operation identity must be a canonical UUID")
	}
	result, err := db.Exec(`UPDATE repository_creation_intents
		SET native_operation_id=?,phase='native_started',last_error=?,updated_at=?
		WHERE id=? AND phase='prepared' AND native_operation_id='' AND profile_sha256<>'' AND profile_json<>''`,
		operationID, "Native creation started; retry validates this exact operation and never launches create again.",
		time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return RepositoryCreationIntent{}, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return RepositoryCreationIntent{}, fmt.Errorf("repository creation already started or is not fully prepared")
	}
	return FindRepositoryCreationIntentByID(db, id)
}

func MarkRepositoryCreationNativeReady(db *sql.DB, id, fingerprint string) error {
	if fingerprint == "" {
		return fmt.Errorf("native repository fingerprint is required")
	}
	result, err := db.Exec(`UPDATE repository_creation_intents
		SET native_fingerprint=?,phase='native_ready',last_error='',updated_at=?
		WHERE id=? AND native_operation_id<>'' AND phase IN ('native_started','native_ready')
		  AND (native_fingerprint='' OR native_fingerprint=?)`,
		fingerprint, time.Now().UTC().Format(time.RFC3339Nano), id, fingerprint)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("native repository differs from the pending creation operation")
	}
	return nil
}

func MarkRepositoryCreationError(db *sql.DB, id, safeError string) error {
	result, err := db.Exec(`UPDATE repository_creation_intents SET last_error=?,updated_at=? WHERE id=?`,
		safeError, time.Now().UTC().Format(time.RFC3339Nano), id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func ListRepositoryCreationIntents(db *sql.DB) ([]RepositoryCreationIntent, error) {
	rows, err := db.Query(`SELECT ` + creationIntentColumns + ` FROM repository_creation_intents ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []RepositoryCreationIntent{}
	for rows.Next() {
		value, err := scanCreationIntent(rows.Scan)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func FindRepositoryCreationIntent(db *sql.DB, engine, connector, location string) (RepositoryCreationIntent, error) {
	return FindRepositoryCreationIntentWithOptions(db, engine, connector, location, nil)
}

func FindRepositoryCreationIntentByID(db *sql.DB, id string) (RepositoryCreationIntent, error) {
	return scanCreationIntent(db.QueryRow(`SELECT `+creationIntentColumns+` FROM repository_creation_intents WHERE id=?`, id).Scan)
}

func FindRepositoryCreationIntentWithOptions(db *sql.DB, engine, connector, location string, options map[string]string) (RepositoryCreationIntent, error) {
	if connector == "fs" {
		return RepositoryCreationIntent{}, ErrStorageIdentityRequired
	}
	identity, err := vaultidentity.IdentityWithOptions(engine, connector, location, options)
	if err != nil {
		return RepositoryCreationIntent{}, err
	}
	return scanCreationIntent(db.QueryRow(`SELECT `+creationIntentColumns+` FROM repository_creation_intents WHERE canonical_identity=?`, identity).Scan)
}

func FindRepositoryCreationIntentByStorage(db *sql.DB, version, key, descriptorJSON string) (RepositoryCreationIntent, error) {
	if _, err := validateStorageBinding(version, key, descriptorJSON); err != nil {
		return RepositoryCreationIntent{}, err
	}
	return scanCreationIntent(db.QueryRow(`SELECT `+creationIntentColumns+
		` FROM repository_creation_intents WHERE canonical_identity=? AND storage_identity_key=?`, key, key).Scan)
}

// CompleteRepositoryCreation atomically attaches the exact native-ready vault
// and removes the now-unneeded intent. There is no completion archive because
// the attached repository is authoritative.
func CompleteRepositoryCreation(db *sql.DB, intent RepositoryCreationIntent, repo models.Repository) (string, error) {
	identity, storageVersion, storageKey, storageJSON, err := repositoryPersistenceIdentity(repo)
	if err != nil {
		return "", err
	}
	if repo.ID != intent.ID || repo.Engine != intent.Engine || repo.Connector != intent.Connector ||
		repo.ColdStorage != intent.ColdStorage || repo.ArchiveWriteClass != intent.ArchiveWriteClass ||
		identity != intent.CanonicalIdentity || storageVersion != intent.StorageIdentityVersion ||
		storageKey != intent.StorageIdentityKey || storageJSON != intent.StorageIdentityJSON {
		return "", fmt.Errorf("repository creation intent does not match the validated vault")
	}
	encodedPassphrase, optionsJSON, err := encodeRepositorySecrets(repo.Connector, repo.Passphrase, repo.ConnectorOptions)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(repo.ProfileUUID) == "" || strings.TrimSpace(repo.ClientUUID) == "" ||
		repo.AttachmentGeneration != 1 || strings.TrimSpace(repo.NativeRepositoryID) == "" {
		return "", fmt.Errorf("repository profile binding or native identity is incomplete")
	}
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	storedIntent, err := scanCreationIntent(tx.QueryRow(`SELECT `+creationIntentColumns+` FROM repository_creation_intents WHERE id=?`, intent.ID).Scan)
	if err != nil {
		return "", fmt.Errorf("reload repository creation intent: %w", err)
	}
	if storedIntent.CanonicalIdentity != intent.CanonicalIdentity ||
		storedIntent.StorageIdentityVersion != intent.StorageIdentityVersion ||
		storedIntent.StorageIdentityKey != intent.StorageIdentityKey || storedIntent.StorageIdentityJSON != intent.StorageIdentityJSON ||
		storedIntent.Engine != intent.Engine || storedIntent.Connector != intent.Connector ||
		storedIntent.ColdStorage != intent.ColdStorage || storedIntent.ArchiveWriteClass != intent.ArchiveWriteClass ||
		storedIntent.Name != intent.Name || storedIntent.ReviewedOptionsJSON != intent.ReviewedOptionsJSON ||
		storedIntent.ObjectLock != intent.ObjectLock ||
		storedIntent.Phase != RepositoryCreationNativeReady || strings.TrimSpace(storedIntent.NativeFingerprint) == "" {
		return "", fmt.Errorf("repository creation intent changed before attachment")
	}
	intent = storedIntent
	profileData := []byte(intent.ProfileJSON)
	profileHash := sha256.Sum256(profileData)
	profile, profileErr := vaultprofile.Parse(profileData)
	if intent.NativeFingerprint == "" || repo.NativeRepositoryID != intent.NativeFingerprint ||
		intent.ProfileSHA256 == "" || hex.EncodeToString(profileHash[:]) != intent.ProfileSHA256 ||
		profileErr != nil || profile.VaultUUID != intent.ID || profile.ProfileUUID != repo.ProfileUUID ||
		profile.Attachment.ClientUUID != repo.ClientUUID || profile.Attachment.Generation != 1 ||
		profile.Attachment.Generation != repo.AttachmentGeneration {
		return "", fmt.Errorf("repository creation attachment binding does not match the durable intent")
	}
	if err := ensureUniqueRepositoryName(tx, intent.Name, intent.ID); err != nil {
		return "", err
	}
	createdAt := repo.CreatedAt
	if createdAt.IsZero() {
		createdAt, _ = time.Parse(time.RFC3339Nano, intent.CreatedAt)
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	if err := AssertVaultUUIDAvailableTx(tx, intent.ID); err != nil {
		return "", err
	}
	objectLockJSON, err := encodeRepositoryObjectLock(models.Repository{
		Engine: intent.Engine, Connector: intent.Connector, ObjectLock: intent.ObjectLock,
	})
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO repositories
		(id,name,engine,connector,cold_storage,archive_write_class,location,canonical_identity,
		 storage_identity_version,storage_identity_key,storage_identity_json,description,
		 passphrase,connector_options,native_repository_id,profile_uuid,attachment_generation,metadata_cache_binding,check_schedule,next_check,
		 maintenance_schedule,object_lock_json,next_maintenance,concurrency_mode,auto_unlock,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		intent.ID, intent.Name, intent.Engine, intent.Connector, intent.ColdStorage,
		intent.ArchiveWriteClass, intent.Location, intent.CanonicalIdentity,
		intent.StorageIdentityVersion, intent.StorageIdentityKey, intent.StorageIdentityJSON,
		intent.Description, encodedPassphrase, optionsJSON, repo.NativeRepositoryID,
		repo.ProfileUUID, repo.AttachmentGeneration, uuid.NewString(), intent.CheckSchedule,
		NextRunFrom(intent.CheckSchedule, time.Now()), intent.MaintenanceSchedule,
		objectLockJSON,
		NextRunFrom(intent.MaintenanceSchedule, time.Now()), intent.ConcurrencyMode, repo.AutoUnlock, createdAt)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO activity_log (timestamp,level,message) VALUES (datetime('now'),'INFO',?)`, "Repository created: "+intent.Name); err != nil {
		return "", err
	}
	if intent.Engine == "kopia" {
		// One post-attachment reconciler owns Kopia policy mutation and exact
		// readback; dirty/not-ready admission remains fail-closed until it succeeds.
		if _, err := RefreshKopiaPolicyStatesTx(tx, []string{intent.ID}); err != nil {
			return "", err
		}
	}
	// The root and initial profile were already published and verified, so
	// creation does not dirty or republish that same empty profile.
	if _, err := tx.Exec(`DELETE FROM repository_creation_intents WHERE id=?`, intent.ID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return intent.ID, nil
}
