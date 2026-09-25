package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/storageavailability"
)

const importedSourceMissingMessage = "The source path for this job is not present on this computer. Copy the job and choose a valid source. You can delete this job afterwards."

var admitPersistedRepository = repositoryadmission.AdmitUnderLock
var admitPersistedControlPlaneRead = repositoryadmission.AdmitControlPlaneReadUnderLock

type macOSAccessError struct {
	message string
	cause   error
}

func (err *macOSAccessError) Error() string { return err.message }
func (err *macOSAccessError) Unwrap() error { return err.cause }

type storageBindingFailure struct {
	message string
	cause   error
}

func (err *storageBindingFailure) Error() string { return err.message }
func (err *storageBindingFailure) Unwrap() error { return err.cause }

func storageBindingError(err error, subject, ability, fallback string) error {
	return storageBindingErrorForPlatform(runtime.GOOS, err, subject, ability, fallback)
}

func storageBindingErrorForPlatform(platform string, err error, subject, ability, fallback string) error {
	if platform != "darwin" || !storageavailability.IsPermissionDenied(err) {
		// The public message stays stable. The typed cause carries only local
		// observation classification to the narrow support recorder.
		return &storageBindingFailure{message: fallback, cause: err}
	}
	return &macOSAccessError{
		cause: err,
		message: "Replicaro can’t access the selected " + subject + " because macOS denied permission.\n\n" +
			"Make sure the " + subject + " is available and that Replicaro can " + ability + ". " +
			"For protected locations, you may also need to allow Replicaro access under " +
			"System Settings → Privacy & Security → Files & Folders or Full Disk Access.\n\n" +
			"After confirming access, try again.",
	}
}

func bindRepositoryStorage(ctx context.Context, repo models.Repository) (models.Repository, error) {
	bound, err := storageavailability.BindRepository(ctx, repo)
	if err != nil {
		return models.Repository{}, storageBindingError(err, "destination", "read from and write to it", "filesystem storage path could not be observed")
	}
	return bound, nil
}

func bindExistingRepositoryStorage(ctx context.Context, repo models.Repository) (models.Repository, error) {
	bound, err := storageavailability.BindExistingRepository(ctx, repo)
	if err != nil {
		if repo.Connector != "fs" {
			return models.Repository{}, err
		}
		return models.Repository{}, storageBindingError(err, "destination", "read from and write to it", "existing filesystem storage path could not be observed")
	}
	return bound, nil
}

func bindJobSourceStorage(ctx context.Context, job models.BackupJob) (models.BackupJob, error) {
	bound, err := storageavailability.BindJobSource(ctx, job)
	if err != nil {
		return models.BackupJob{}, storageBindingError(err, "source", "read it", "source path could not be observed")
	}
	bound.SourceBindingState = "bound"
	return bound, nil
}

func bindImportedJobSourceStorage(ctx context.Context, job models.BackupJob) (models.BackupJob, error) {
	bound, err := bindJobSourceStorage(ctx, job)
	if err != nil {
		var access *macOSAccessError
		if errors.As(err, &access) {
			return models.BackupJob{}, err
		}
		return models.BackupJob{}, &storageBindingFailure{message: importedSourceMissingMessage, cause: err}
	}
	return bound, nil
}

func requireRepositoryStorageAvailable(ctx context.Context, repo models.Repository) error {
	observation := storageavailability.ObserveRepository(ctx, repo, time.Now().UTC())
	if observation.State != database.StorageAvailable {
		return fmt.Errorf("the saved filesystem storage is unavailable")
	}
	return nil
}

func requirePersistedRepositoryStorageAvailable(ctx context.Context, repo models.Repository) error {
	if err := storageavailability.RequireRepositoryAvailable(ctx, repo); err != nil {
		return fmt.Errorf("the saved filesystem storage is unavailable: %w", err)
	}
	return nil
}

func requireJobSourceStorageAvailable(ctx context.Context, job models.BackupJob) error {
	observation := storageavailability.ObserveSource(ctx, job, time.Now().UTC())
	if observation.State != database.StorageAvailable {
		return fmt.Errorf("the saved source path is unavailable")
	}
	return nil
}

func applyIntentStorage(repo models.Repository, intent database.RepositoryCreationIntent) models.Repository {
	repo.ColdStorage = intent.ColdStorage
	repo.ArchiveWriteClass = intent.ArchiveWriteClass
	repo.CanonicalIdentity = intent.CanonicalIdentity
	repo.StorageIdentityVersion = intent.StorageIdentityVersion
	repo.StorageIdentityKey = intent.StorageIdentityKey
	repo.StorageIdentityJSON = intent.StorageIdentityJSON
	return repo
}

// Before a protected root exists, an in-flight creation still owns its exact
// configured destination. This lookup is only creation-lifecycle routing; it
// is not a managed-vault identity or import-duplicate rule.
func findCreationIntentForBoundRepository(db *sql.DB, repo models.Repository) (database.RepositoryCreationIntent, error) {
	if repo.Connector == "fs" {
		return database.FindRepositoryCreationIntentByStorage(db,
			repo.StorageIdentityVersion, repo.StorageIdentityKey, repo.StorageIdentityJSON)
	}
	return database.FindRepositoryCreationIntentWithOptions(db,
		repo.Engine, repo.Connector, repo.Location, repo.ConnectorOptions)
}
