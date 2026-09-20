package database

import (
	"encoding/json"
	"fmt"

	"github.com/local/replicaro/models"
	"github.com/local/replicaro/storageidentity"
	"github.com/local/replicaro/vaultidentity"
)

func validateStorageBinding(version, key, descriptorJSON string) (string, error) {
	descriptor, err := storageidentity.DecodeBinding(version, key, descriptorJSON)
	if err != nil {
		return "", err
	}
	canonicalJSON, err := json.Marshal(descriptor)
	if err != nil {
		return "", fmt.Errorf("serialize storage identity descriptor: %w", err)
	}
	if descriptorJSON != string(canonicalJSON) {
		return "", fmt.Errorf("storage identity descriptor is not canonical")
	}
	return string(canonicalJSON), nil
}

func validatePathOnlyBindingLocation(configuredPath, descriptorJSON string) error {
	canonical, err := storageidentity.NormalizeConfiguredPath(configuredPath)
	if err != nil || canonical != configuredPath {
		return fmt.Errorf("configured storage path is not canonical")
	}
	var descriptor storageidentity.Descriptor
	if err := json.Unmarshal([]byte(descriptorJSON), &descriptor); err != nil {
		return fmt.Errorf("storage identity descriptor is invalid")
	}
	if descriptor.Kind == storageidentity.KindPathOnly &&
		!storageidentity.PathOnlyMatchesConfiguredPath(descriptor, configuredPath) {
		return fmt.Errorf("path-only storage binding does not match its exact configured path")
	}
	return nil
}

func pathOnlyStorageBinding(version, key, descriptorJSON, configuredPath string) (bool, error) {
	canonicalJSON, err := validateStorageBinding(version, key, descriptorJSON)
	if err != nil {
		return false, err
	}
	if err := validatePathOnlyBindingLocation(configuredPath, canonicalJSON); err != nil {
		return false, err
	}
	var descriptor storageidentity.Descriptor
	if err := json.Unmarshal([]byte(canonicalJSON), &descriptor); err != nil {
		return false, err
	}
	return descriptor.Kind == storageidentity.KindPathOnly, nil
}

func repositoryPersistenceIdentity(repo models.Repository) (identity, version, key, descriptorJSON string, err error) {
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
	descriptorJSON, err = validateStorageBinding(
		repo.StorageIdentityVersion,
		repo.StorageIdentityKey,
		repo.StorageIdentityJSON,
	)
	if err != nil {
		return
	}
	if err = validatePathOnlyBindingLocation(repo.Location, descriptorJSON); err != nil {
		return
	}
	identity = repo.StorageIdentityKey
	version = repo.StorageIdentityVersion
	key = repo.StorageIdentityKey
	if repo.CanonicalIdentity != "" && repo.CanonicalIdentity != key {
		err = fmt.Errorf("filesystem repository canonical identity must equal its storage identity key")
	}
	return
}

func validateRepositoryStoredBinding(repo models.Repository) error {
	identity, version, key, descriptorJSON, err := repositoryPersistenceIdentity(repo)
	if err != nil {
		return err
	}
	if identity != repo.CanonicalIdentity ||
		version != repo.StorageIdentityVersion ||
		key != repo.StorageIdentityKey ||
		descriptorJSON != repo.StorageIdentityJSON {
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
	canonicalJSON, err := validateStorageBinding(
		job.SourceStorageVersion,
		job.SourceStorageKey,
		job.SourceStorageDescriptorJSON,
	)
	if err != nil {
		return err
	}
	if err := validatePathOnlyBindingLocation(job.Source, canonicalJSON); err != nil {
		return err
	}
	return nil
}

func normalizedJobSourceBindingState(state string) string {
	if state == "" {
		return "bound"
	}
	return state
}
