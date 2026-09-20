package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/models"
)

var (
	ErrRepositoryConnectionCannotCancel = errors.New("repository connection cannot be cancelled after external mutation may have begun")
	ErrRepositoryConnectionReserved     = errors.New("saved vault is reserved by a pending repository reconnect")
)

func requireRepositoryConnectionUnreserved(tx *sql.Tx, repositoryID string) error {
	var passwordChange int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM vault_password_change_operations WHERE repository_id=?`, repositoryID).Scan(&passwordChange); err != nil {
		return err
	}
	if passwordChange != 0 {
		return ErrVaultPasswordChangeRecoveryRequired
	}
	var reserved int
	if err := tx.QueryRow(`SELECT COUNT(*)
		FROM repository_connection_name_reservations r
		JOIN repository_connection_intents c ON c.id=r.connection_id
		WHERE r.repository_id=?`, repositoryID).Scan(&reserved); err != nil {
		return err
	}
	if reserved != 0 {
		return ErrRepositoryConnectionReserved
	}
	return nil
}

// requireRepositoryMutationUnreserved permits ordinary saved-row controls
// after the new credential is atomically committed. Destructive removal keeps
// using requireRepositoryConnectionUnreserved so cleanup truth cannot cascade
// away with the repository row.
func requireRepositoryMutationUnreserved(tx *sql.Tx, repositoryID string) error {
	var passwordChange int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM vault_password_change_operations
		WHERE repository_id=? AND phase IN ('preparing','native_started','publishing')`, repositoryID).Scan(&passwordChange); err != nil {
		return err
	}
	if passwordChange != 0 {
		return ErrVaultPasswordChangeRecoveryRequired
	}
	var reserved int
	if err := tx.QueryRow(`SELECT COUNT(*)
		FROM repository_connection_name_reservations r
		JOIN repository_connection_intents c ON c.id=r.connection_id
		WHERE r.repository_id=?`, repositoryID).Scan(&reserved); err != nil {
		return err
	}
	if reserved != 0 {
		return ErrRepositoryConnectionReserved
	}
	return nil
}

// ValidateRepositoryMutationAdmission rejects saved-row settings, credential,
// and removal mutations while the existing live connection reservation owns
// an in-progress reconnect for this exact repository row.
func ValidateRepositoryMutationAdmission(db *sql.DB, repositoryID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireRepositoryMutationUnreserved(tx, repositoryID); err != nil {
		return err
	}
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM repositories WHERE id=?`, repositoryID).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return sql.ErrNoRows
	}
	return nil
}

type RepositoryConnectionIntent struct {
	ID                     string            `json:"id"`
	CanonicalIdentity      string            `json:"-"`
	Connector              string            `json:"connector"`
	ColdStorage            bool              `json:"coldStorage"`
	ArchiveWriteClass      string            `json:"archiveWriteClass,omitempty"`
	Location               string            `json:"location"`
	ReviewedOptions        map[string]string `json:"reviewedOptions,omitempty"`
	ReviewedOptionsJSON    string            `json:"-"`
	Mode                   string            `json:"mode"`
	PreviewDigest          string            `json:"previewDigest"`
	PayloadJSON            string            `json:"-"`
	ProfileSHA256          string            `json:"-"`
	NativeFingerprint      string            `json:"-"`
	PublicationOperationID string            `json:"-"`
	State                  string            `json:"state"`
	Error                  string            `json:"error,omitempty"`
	CreatedAt              string            `json:"createdAt"`
	UpdatedAt              string            `json:"updatedAt"`
}

const connectionIntentColumns = `id, canonical_identity, connector, location, reviewed_options_json, mode, preview_digest,
	payload_json, profile_sha256, native_fingerprint, publication_operation_id,
	state, error, created_at, updated_at`

func scanConnectionIntent(scan func(...any) error) (RepositoryConnectionIntent, error) {
	var value RepositoryConnectionIntent
	err := scan(&value.ID, &value.CanonicalIdentity, &value.Connector, &value.Location,
		&value.ReviewedOptionsJSON, &value.Mode, &value.PreviewDigest,
		&value.PayloadJSON, &value.ProfileSHA256, &value.NativeFingerprint,
		&value.PublicationOperationID, &value.State, &value.Error,
		&value.CreatedAt, &value.UpdatedAt)
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal([]byte(value.ReviewedOptionsJSON), &value.ReviewedOptions); err != nil {
		return value, fmt.Errorf("decode reviewed connector options: %w", err)
	}
	var marker struct {
		Repository struct {
			ColdStorage       bool
			ArchiveWriteClass string
		}
	}
	if err := json.Unmarshal([]byte(value.PayloadJSON), &marker); err != nil {
		return value, fmt.Errorf("decode repository connection cold-storage settings: %w", err)
	}
	value.ColdStorage = marker.Repository.ColdStorage
	value.ArchiveWriteClass = marker.Repository.ArchiveWriteClass
	return value, err
}

func ReserveRepositoryConnection(db *sql.DB, canonicalIdentity, connector, location string, reviewedOptions map[string]string, mode, digest, payloadJSON, profileSHA256, nativeFingerprint string, publicationOperationID ...string) (RepositoryConnectionIntent, error) {
	if connector == "fs" {
		return RepositoryConnectionIntent{}, ErrStorageIdentityRequired
	}
	return reserveRepositoryConnection(db, canonicalIdentity, connector, location, reviewedOptions,
		mode, digest, payloadJSON, profileSHA256, nativeFingerprint, nil, publicationOperationID...)
}

func ReserveRepositoryUpdate(db *sql.DB, expected models.Repository, connector, location string, reviewedOptions map[string]string, mode, digest, payloadJSON, profileSHA256, nativeFingerprint string, publicationOperationID ...string) (RepositoryConnectionIntent, error) {
	if connector == "fs" {
		return RepositoryConnectionIntent{}, ErrStorageIdentityRequired
	}
	return reserveRepositoryConnection(db, expected.ID, connector, location, reviewedOptions,
		mode, digest, payloadJSON, profileSHA256, nativeFingerprint, &expected, publicationOperationID...)
}

func ReserveRepositoryConnectionWithStorage(
	db *sql.DB,
	version, key, descriptorJSON, location string,
	reviewedOptions map[string]string,
	mode, digest, payloadJSON, profileSHA256, nativeFingerprint string,
	publicationOperationID ...string,
) (RepositoryConnectionIntent, error) {
	canonicalJSON, err := validateStorageBinding(version, key, descriptorJSON)
	if err != nil {
		return RepositoryConnectionIntent{}, err
	}
	if err := validatePathOnlyBindingLocation(location, canonicalJSON); err != nil {
		return RepositoryConnectionIntent{}, err
	}
	return reserveRepositoryConnection(db, key, "fs", location, reviewedOptions,
		mode, digest, payloadJSON, profileSHA256, nativeFingerprint, nil, publicationOperationID...)
}

func ReserveRepositoryUpdateWithStorage(
	db *sql.DB,
	expected models.Repository,
	version, key, descriptorJSON, location string,
	reviewedOptions map[string]string,
	mode, digest, payloadJSON, profileSHA256, nativeFingerprint string,
	publicationOperationID ...string,
) (RepositoryConnectionIntent, error) {
	canonicalJSON, err := validateStorageBinding(version, key, descriptorJSON)
	if err != nil {
		return RepositoryConnectionIntent{}, err
	}
	if err := validatePathOnlyBindingLocation(location, canonicalJSON); err != nil {
		return RepositoryConnectionIntent{}, err
	}
	return reserveRepositoryConnection(db, expected.ID, "fs", location, reviewedOptions,
		mode, digest, payloadJSON, profileSHA256, nativeFingerprint, &expected, publicationOperationID...)
}

func reserveRepositoryConnection(db *sql.DB, canonicalIdentity, connector, location string, reviewedOptions map[string]string, mode, digest, payloadJSON, profileSHA256, nativeFingerprint string, expectedExisting *models.Repository, requestedPublicationID ...string) (RepositoryConnectionIntent, error) {
	if canonicalIdentity == "" || connector == "" || location == "" || mode == "" || digest == "" || payloadJSON == "" || profileSHA256 == "" || nativeFingerprint == "" {
		return RepositoryConnectionIntent{}, fmt.Errorf("repository connection intent is incomplete")
	}
	if reviewedOptions == nil {
		reviewedOptions = map[string]string{}
	}
	var canonicalPayload any
	if err := json.Unmarshal([]byte(payloadJSON), &canonicalPayload); err != nil {
		return RepositoryConnectionIntent{}, fmt.Errorf("repository connection payload is invalid")
	}
	canonicalJSON, err := json.Marshal(canonicalPayload)
	if err != nil {
		return RepositoryConnectionIntent{}, fmt.Errorf("canonicalize repository connection payload: %w", err)
	}
	payloadJSON = string(canonicalJSON)
	var reserved reservedConnectionPayload
	if err := json.Unmarshal(canonicalJSON, &reserved); err != nil ||
		strings.TrimSpace(reserved.Repository.ID) == "" ||
		strings.TrimSpace(reserved.Repository.Name) == "" {
		return RepositoryConnectionIntent{}, fmt.Errorf("repository connection payload has no reservable vault name")
	}
	// The connection intent's lookup key is the protected vault UUID. Address
	// facts stay in the immutable payload for exact retry validation but never
	// merge or reject a new import by physical location.
	canonicalIdentity = strings.TrimSpace(reserved.Repository.ID)
	reviewedJSON, err := json.Marshal(reviewedOptions)
	if err != nil {
		return RepositoryConnectionIntent{}, fmt.Errorf("encode reviewed connector options: %w", err)
	}
	tx, err := db.Begin()
	if err != nil {
		return RepositoryConnectionIntent{}, err
	}
	defer func() { _ = tx.Rollback() }()
	if expectedExisting != nil {
		if err := validateRepositoryUpdateReservation(tx, *expectedExisting); err != nil {
			return RepositoryConnectionIntent{}, err
		}
	}
	if existing, err := scanConnectionIntent(tx.QueryRow(`SELECT `+connectionIntentColumns+
		` FROM repository_connection_intents WHERE canonical_identity=?`, canonicalIdentity).Scan); err == nil {
		if existing.Connector != connector || existing.Location != location || existing.ReviewedOptionsJSON != string(reviewedJSON) || existing.Mode != mode || existing.PreviewDigest != digest || existing.PayloadJSON != payloadJSON || existing.ProfileSHA256 != profileSHA256 || existing.NativeFingerprint != nativeFingerprint {
			return RepositoryConnectionIntent{}, fmt.Errorf("a different connection attempt is already pending for this vault")
		}
		if err := tx.Commit(); err != nil {
			return RepositoryConnectionIntent{}, err
		}
		return existing, nil
	} else if err != sql.ErrNoRows {
		return RepositoryConnectionIntent{}, err
	}
	if err := ensureUniqueRepositoryName(tx, reserved.Repository.Name, reserved.Repository.ID); err != nil {
		return RepositoryConnectionIntent{}, err
	}
	seenJobIDs := map[string]bool{}
	for _, job := range reserved.Jobs {
		if strings.TrimSpace(job.ID) == "" || seenJobIDs[job.ID] {
			return RepositoryConnectionIntent{}, fmt.Errorf("repository connection payload has invalid job name reservations")
		}
		seenJobIDs[job.ID] = true
		if err := ensureUniqueJobName(tx, job.Name, job.ID); err != nil {
			return RepositoryConnectionIntent{}, err
		}
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	id := uuid.NewString()
	publicationID := ""
	if len(requestedPublicationID) > 0 {
		publicationID = strings.TrimSpace(requestedPublicationID[0])
	}
	if publicationID == "" {
		publicationID = uuid.NewString()
	} else if parsed, parseErr := uuid.Parse(publicationID); parseErr != nil || parsed.String() != publicationID {
		return RepositoryConnectionIntent{}, fmt.Errorf("repository connection publication operation ID is invalid")
	}
	if _, err := tx.Exec(`INSERT INTO repository_connection_intents
		(id, canonical_identity, connector, location, reviewed_options_json, mode, preview_digest, payload_json,
		 profile_sha256, native_fingerprint, publication_operation_id, state, error, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'prepared', '', ?, ?)`, id, canonicalIdentity,
		connector, location, string(reviewedJSON), mode, digest, payloadJSON,
		profileSHA256, nativeFingerprint, publicationID, now, now); err != nil {
		return RepositoryConnectionIntent{}, err
	}
	if _, err := tx.Exec(`INSERT INTO repository_connection_name_reservations
		(connection_id,repository_id,name) VALUES (?,?,?)`,
		id, reserved.Repository.ID, strings.TrimSpace(reserved.Repository.Name)); err != nil {
		return RepositoryConnectionIntent{}, err
	}
	for _, job := range reserved.Jobs {
		if _, err := tx.Exec(`INSERT INTO repository_connection_job_name_reservations
			(connection_id,job_id,name) VALUES (?,?,?)`,
			id, job.ID, strings.TrimSpace(job.Name)); err != nil {
			return RepositoryConnectionIntent{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return RepositoryConnectionIntent{}, err
	}
	return FindRepositoryConnectionIntent(db, canonicalIdentity)
}

func validateRepositoryUpdateReservation(tx *sql.Tx, expected models.Repository) error {
	if err := requireRepositoryConnectionUnreserved(tx, expected.ID); err != nil {
		return err
	}
	encodedPassword, encodedOptions, err := encodeRepositorySecrets(expected.Connector, expected.Passphrase, expected.ConnectorOptions)
	if err != nil {
		return err
	}
	objectLockJSON, err := encodeRepositoryObjectLock(expected)
	if err != nil {
		return err
	}
	var name, engine, connector, archiveWriteClass, location, identity, description string
	var storageVersion, storageKey, storageJSON, resolvedPath, resolvedAt string
	var password, options, nativeID, profileUUID, checkSchedule, maintenanceSchedule, concurrencyMode, storedObjectLock string
	var coldStorage, autoUnlock bool
	var attachmentGeneration int64
	var requiredGeneration int64
	if err := tx.QueryRow(`SELECT name,engine,connector,cold_storage,archive_write_class,location,canonical_identity,description,
		storage_identity_version,storage_identity_key,storage_identity_json,resolved_repository_path,resolved_repository_observed_at,
		passphrase,connector_options,native_repository_id,profile_uuid,attachment_generation,check_schedule,maintenance_schedule,
		concurrency_mode,auto_unlock,object_lock_json,required_generation FROM repositories WHERE id=?`, expected.ID).Scan(
		&name, &engine, &connector, &coldStorage, &archiveWriteClass, &location, &identity, &description,
		&storageVersion, &storageKey, &storageJSON, &resolvedPath, &resolvedAt,
		&password, &options, &nativeID, &profileUUID, &attachmentGeneration, &checkSchedule, &maintenanceSchedule,
		&concurrencyMode, &autoUnlock, &storedObjectLock, &requiredGeneration,
	); err != nil {
		return err
	}
	// The reservation is the last no-mutation boundary. Once it succeeds,
	// ordinary row and job mutations are fenced by the existing connection
	// reservation until exact retry completes or a prepared review is cancelled.
	if name != expected.Name || engine != expected.Engine || connector != expected.Connector || coldStorage != expected.ColdStorage ||
		archiveWriteClass != expected.ArchiveWriteClass ||
		location != expected.Location || identity != expected.CanonicalIdentity || description != expected.Description ||
		storageVersion != expected.StorageIdentityVersion || storageKey != expected.StorageIdentityKey || storageJSON != expected.StorageIdentityJSON ||
		resolvedPath != expected.ResolvedRepositoryPath || resolvedAt != expected.ResolvedRepositoryObservedAt ||
		password != encodedPassword || options != encodedOptions || nativeID != expected.NativeRepositoryID ||
		profileUUID != expected.ProfileUUID || attachmentGeneration != expected.AttachmentGeneration ||
		checkSchedule != expected.CheckSchedule || maintenanceSchedule != expected.MaintenanceSchedule ||
		concurrencyMode != expected.ConcurrencyMode || autoUnlock != expected.AutoUnlock || storedObjectLock != objectLockJSON {
		return fmt.Errorf("saved vault changed after Update existing vault review")
	}
	_ = requiredGeneration // Metadata catch-up may advance independently of the reviewed saved-row state.
	var active int
	if err := tx.QueryRow(`SELECT
		(SELECT COUNT(*) FROM operations WHERE repository_id=? AND status IN ('queued','running')) +
		(SELECT COUNT(*) FROM owner_transfer_operations WHERE repository_id=? AND state NOT IN ('completed','failed')) +
		(SELECT COUNT(*) FROM kopia_filesystem_reconnect_intents WHERE repository_id=?)`, expected.ID, expected.ID, expected.ID).Scan(&active); err != nil {
		return err
	}
	if active != 0 {
		return fmt.Errorf("saved vault has queued, running, or unresolved native work")
	}
	return nil
}

func ListRepositoryConnectionIntents(db *sql.DB) ([]RepositoryConnectionIntent, error) {
	rows, err := db.Query(`SELECT ` + connectionIntentColumns + ` FROM repository_connection_intents ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	result := []RepositoryConnectionIntent{}
	for rows.Next() {
		value, scanErr := scanConnectionIntent(rows.Scan)
		if scanErr != nil {
			_ = rows.Close()
			return nil, scanErr
		}
		result = append(result, value)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return result, nil
}

// CancelRepositoryConnectionIntent removes a purely local prepared review.
// Once publication or native configuration work starts, forward recovery is
// the only safe lifecycle and the immutable intent remains authoritative.
func CancelRepositoryConnectionIntent(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`DELETE FROM repository_connection_intents WHERE id = ? AND state='prepared'`, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		var exists int
		if scanErr := tx.QueryRow(`SELECT COUNT(*) FROM repository_connection_intents WHERE id=?`, id).Scan(&exists); scanErr != nil {
			return scanErr
		}
		if exists != 0 {
			return ErrRepositoryConnectionCannotCancel
		}
		return sql.ErrNoRows
	}
	return tx.Commit()
}

func FindRepositoryConnectionIntent(db *sql.DB, canonicalIdentity string) (RepositoryConnectionIntent, error) {
	value, err := scanConnectionIntent(db.QueryRow(`SELECT `+connectionIntentColumns+
		` FROM repository_connection_intents WHERE canonical_identity = ?`, canonicalIdentity).Scan)
	return value, err
}

func FindRepositoryConnectionIntentByID(db *sql.DB, id string) (RepositoryConnectionIntent, error) {
	value, err := scanConnectionIntent(db.QueryRow(`SELECT `+connectionIntentColumns+
		` FROM repository_connection_intents WHERE id = ?`, id).Scan)
	return value, err
}

func MarkRepositoryConnectionIntent(db *sql.DB, id, state, safeError string) error {
	if state != "publication_started" && state != "attachment_pending" {
		return fmt.Errorf("repository connection lifecycle state is invalid")
	}
	result, err := db.Exec(`UPDATE repository_connection_intents
		SET state = ?, error = ?, updated_at = ? WHERE id = ? AND (
			(state='prepared' AND ?='publication_started') OR
			(state='publication_started' AND ?='attachment_pending') OR state=?)`, state, safeError,
		time.Now().UTC().Format(time.RFC3339Nano), id, state, state, state)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}
