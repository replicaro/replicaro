package database

import (
	"database/sql"
	"fmt"

	"github.com/local/replicaro/models"
	"github.com/local/replicaro/storageidentity"
	"github.com/local/replicaro/vaultidentity"
)

// validateStorageBinding checks one binding tuple against the immutable path
// it belongs to. Rows still carrying the retired descriptor version are
// accepted without decoding them (see storageidentity.LegacyBindingVersion).
func validateStorageBinding(version, key, bindingJSON, configuredPath string) error {
	_, _, err := storageidentity.DecodeBinding(version, key, bindingJSON, configuredPath)
	return err
}

func legacyStorageBinding(version string) bool {
	return version == storageidentity.LegacyBindingVersion
}

// SourceBindingRecordsNoFacts reports whether a bound job's source binding is
// in the current format but records neither a mount point nor a filesystem
// type, so the before-run check has nothing to compare. A retired-format
// binding is not included: its first successful probe converts it.
func SourceBindingRecordsNoFacts(job models.BackupJob) bool {
	if normalizedJobSourceBindingState(job.SourceBindingState) != "bound" ||
		job.SourceStorageVersion != storageidentity.BindingVersion {
		return false
	}
	facts, legacy, err := storageidentity.DecodeBinding(job.SourceStorageVersion, job.SourceStorageKey,
		job.SourceStorageDescriptorJSON, job.Source)
	return err == nil && !legacy && facts == (storageidentity.Facts{})
}

func repositoryPersistenceIdentity(repo models.Repository) (identity, version, key, bindingJSON string, err error) {
	if repo.Connector != "fs" {
		if repo.StorageIdentityVersion != "" || repo.StorageIdentityKey != "" || repo.StorageIdentityJSON != "" {
			err = fmt.Errorf("remote repository must not contain a filesystem storage identity")
			return
		}
		identity, err = vaultidentity.IdentityWithOptions(repo.Engine, repo.Connector, repo.Location, repo.ConnectorOptions)
		if err == nil && repo.CanonicalIdentity != "" && repo.CanonicalIdentity != identity {
			err = fmt.Errorf("repository canonical identity is inconsistent")
		}
		return
	}
	if err = validateStorageBinding(repo.StorageIdentityVersion, repo.StorageIdentityKey,
		repo.StorageIdentityJSON, repo.Location); err != nil {
		return
	}
	version, key, bindingJSON = repo.StorageIdentityVersion, repo.StorageIdentityKey, repo.StorageIdentityJSON
	// Current rows: the filesystem vault's canonical identity is its exact
	// normalized location (which is also the storage key). Legacy rows keep
	// their old descriptor key as canonical identity until the first
	// successful probe under the vault lock converts them.
	identity = repo.Location
	if legacyStorageBinding(version) {
		identity = key
	}
	if repo.CanonicalIdentity != "" && repo.CanonicalIdentity != identity {
		err = fmt.Errorf("filesystem repository canonical identity must equal its storage key")
	}
	return
}

func validateRepositoryStoredBinding(repo models.Repository) error {
	identity, version, key, bindingJSON, err := repositoryPersistenceIdentity(repo)
	if err != nil {
		return err
	}
	if identity != repo.CanonicalIdentity ||
		version != repo.StorageIdentityVersion ||
		key != repo.StorageIdentityKey ||
		bindingJSON != repo.StorageIdentityJSON {
		return fmt.Errorf("repository storage identity columns are inconsistent")
	}
	return nil
}

func validateJobSourceBinding(job models.BackupJob) error {
	state := normalizedJobSourceBindingState(job.SourceBindingState)
	if state == "unbound_imported" {
		if job.Enabled || job.SourceStorageVersion != "" || job.SourceStorageKey != "" || job.SourceStorageDescriptorJSON != "" {
			return fmt.Errorf("an imported unbound source must remain disabled and contain no local storage binding")
		}
		return nil
	}
	if state != "bound" {
		return fmt.Errorf("source binding state is invalid")
	}
	return validateStorageBinding(job.SourceStorageVersion, job.SourceStorageKey,
		job.SourceStorageDescriptorJSON, job.Source)
}

func normalizedJobSourceBindingState(state string) string {
	if state == "" {
		return "bound"
	}
	return state
}

// equivalentSourceBinding compares two stored source bindings of the same
// source. Besides exact equality it accepts one side being the legacy format
// and the other the current format: the only way that happens is that the
// row was converted by a probe between a caller reading it and writing it
// back, and failing the caller's request for that would be an unrelated
// conflict. Source equality itself is always checked separately.
func equivalentSourceBinding(leftVersion, leftKey, leftJSON, rightVersion, rightKey, rightJSON string) bool {
	if leftVersion == rightVersion && leftKey == rightKey && leftJSON == rightJSON {
		return true
	}
	return legacyStorageBinding(leftVersion) != legacyStorageBinding(rightVersion)
}

// SourceBindingConversion is the first successful probe of a job whose
// binding is still in the legacy format. The admission transaction applies it
// as a compare-and-swap on the exact old values, so a concurrent change (a
// definition update, first binding, or another conversion) wins and this one
// is simply skipped.
type SourceBindingConversion struct {
	Location                       string
	FromVersion, FromKey, FromJSON string
	ToVersion, ToKey, ToJSON       string
}

func convertLegacySourceBindingTx(tx *sql.Tx, jobID string, conversion SourceBindingConversion) error {
	if !legacyStorageBinding(conversion.FromVersion) || conversion.ToVersion != storageidentity.BindingVersion {
		return fmt.Errorf("source binding conversion is invalid")
	}
	var source string
	if err := tx.QueryRow(`SELECT source FROM backup_jobs WHERE id=?`, jobID).Scan(&source); err != nil {
		return err
	}
	if err := validateStorageBinding(conversion.ToVersion, conversion.ToKey, conversion.ToJSON, source); err != nil {
		return fmt.Errorf("converted source binding is invalid: %w", err)
	}
	// The location the facts were observed at must still be the location in
	// use (alias, or the source when there is no alias). The alias column is
	// matched as it is stored: empty means "use the source".
	alias := conversion.Location
	if alias == source {
		alias = ""
	}
	_, err := tx.Exec(`UPDATE backup_jobs SET source_storage_version=?,source_storage_key=?,source_storage_json=?
		WHERE id=? AND source=? AND source_binding_state='bound'
		  AND source_storage_version=? AND source_storage_key=? AND source_storage_json=?
		  AND resolved_source_path=?`,
		conversion.ToVersion, conversion.ToKey, conversion.ToJSON,
		jobID, source, conversion.FromVersion, conversion.FromKey, conversion.FromJSON, alias)
	return err
}

// ConvertLegacyRepositoryStorageBinding rewrites one legacy filesystem vault
// binding with the facts of its first successful probe. The caller holds the
// vault lock; the update is still a compare-and-swap on the old values so a
// row changed through another path (for example a confirmed location update)
// is never overwritten. It returns the stored row after the attempt.
func ConvertLegacyRepositoryStorageBinding(db *sql.DB, expected models.Repository, bindingJSON string) (models.Repository, error) {
	if expected.Connector != "fs" || !legacyStorageBinding(expected.StorageIdentityVersion) {
		return models.Repository{}, fmt.Errorf("repository storage binding is not a legacy filesystem binding")
	}
	if err := validateStorageBinding(storageidentity.BindingVersion, expected.Location, bindingJSON, expected.Location); err != nil {
		return models.Repository{}, fmt.Errorf("converted repository binding is invalid: %w", err)
	}
	if _, err := db.Exec(`UPDATE repositories SET canonical_identity=?,storage_identity_version=?,
		storage_identity_key=?,storage_identity_json=?
		WHERE id=? AND connector='fs' AND location=? AND canonical_identity=?
		  AND storage_identity_version=? AND storage_identity_key=? AND storage_identity_json=?`,
		expected.Location, storageidentity.BindingVersion, expected.Location, bindingJSON,
		expected.ID, expected.Location, expected.CanonicalIdentity,
		expected.StorageIdentityVersion, expected.StorageIdentityKey, expected.StorageIdentityJSON); err != nil {
		return models.Repository{}, err
	}
	return GetRepository(db, expected.ID)
}
