package profilebinding

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultprofile"
)

// RepairMissing performs the sole automatic profile-binding repair: one
// bounded protected-profile scan may restore a missing local pointer only
// when exactly one authoritative attachment already names this client. It
// never publishes or changes a remote profile.
func RepairMissing(ctx context.Context, db *sql.DB, repo models.Repository) (models.Repository, bool, error) {
	return repairMissing(ctx, db, repo, false)
}

// RepairMissingFilesystemAfterIdentityProof is the filesystem-only repair
// path used after the caller has frozen the candidate and proved its exact
// native marker fingerprint. The function still rereads and validates the
// protected root before it changes the local profile pointer.
func RepairMissingFilesystemAfterIdentityProof(ctx context.Context, db *sql.DB, repo models.Repository) (models.Repository, bool, error) {
	if repo.Connector != "fs" {
		return repo, false, fmt.Errorf("verified filesystem profile repair requires a filesystem vault")
	}
	return repairMissing(ctx, db, repo, true)
}

func repairMissing(ctx context.Context, db *sql.DB, repo models.Repository, nativeIdentityProven bool) (models.Repository, bool, error) {
	if strings.TrimSpace(repo.ProfileUUID) != "" && repo.AttachmentGeneration > 0 {
		return repo, false, nil
	}
	if strings.TrimSpace(repo.ClientUUID) == "" {
		return repo, false, fmt.Errorf("local installation identity is unavailable; select Reconnect to review the vault")
	}
	if !nativeIdentityProven {
		engine, err := engines.ResolveWithRepositoryAvailabilityCheck(repo, storageavailability.RequireRepositoryAvailable)
		if err != nil {
			return repo, false, fmt.Errorf("saved vault connection is unavailable; select Reconnect to review it: %w", err)
		}
		output, err := engine.Info(ctx, repo)
		if err != nil {
			return repo, false, fmt.Errorf("saved vault connection or password was rejected; select Reconnect to review it: %w", err)
		}
		fingerprint, err := engines.RepositoryFingerprint(repo, output)
		if repo.Engine == engines.ResticID && engines.IsResticRcloneConnector(repo.Connector) {
			fingerprint, err = engines.ResticRcloneRepositoryFingerprint(ctx, repo)
		}
		if err != nil || fingerprint != repo.NativeRepositoryID {
			return repo, false, fmt.Errorf("saved connection does not identify the expected vault; select Reconnect to review it")
		}
	}
	store := (vaultprofile.Store{Repository: repo}).WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable)
	rootData, err := store.Read(ctx)
	if err != nil {
		return repo, false, fmt.Errorf("read expected vault identity before profile repair: %w", err)
	}
	root, err := vaultprofile.ParseRoot(rootData, repo.Connector)
	if err != nil || root.VaultUUID != repo.ID || root.Repository.Engine != repo.Engine ||
		root.Repository.NativeRepositoryID != repo.NativeRepositoryID || root.OwnerTransfer != nil {
		return repo, false, fmt.Errorf("protected root does not identify the expected stable vault; select Reconnect to review it")
	}
	matchUUID, matchGeneration, matches, profileCount, err := selectClientProfile(repo.ClientUUID, func(visit func(vaultprofile.Profile) error) error {
		return store.ScanProfiles(ctx, 256, func(profile vaultprofile.Profile, _ []byte) error {
			return visit(profile)
		})
	})
	if err != nil {
		return repo, false, fmt.Errorf("bounded vault profile repair failed; select Reconnect to review it: %w", err)
	}
	if matches == 0 {
		return repo, false, fmt.Errorf("no authoritative vault profile is attached to this computer; select Reconnect to review it")
	}
	if matches != 1 {
		return repo, false, fmt.Errorf("multiple authoritative vault profiles name this computer; select Reconnect to resolve the identity conflict")
	}
	if automaticSoleProfileConnector(repo.Connector) && profileCount != 1 {
		return repo, false, fmt.Errorf("this cloud vault must contain exactly one authoritative profile; select Reconnect to review it")
	}
	isOwner := root.VaultOwner.ProfileUUID == matchUUID
	if err := database.RepairMissingRepositoryProfileBinding(db, repo.ID, repo.ClientUUID,
		matchUUID, matchGeneration); err != nil {
		return repo, false, fmt.Errorf("restore exact local vault profile binding: %w", err)
	}
	repaired, err := database.GetRepository(db, repo.ID)
	repaired.IsVaultOwner = isOwner
	return repaired, err == nil, err
}

func automaticSoleProfileConnector(connector string) bool {
	switch connector {
	case "dropbox", "google_drive", "onedrive":
		return true
	default:
		return false
	}
}

func selectClientProfile(clientUUID string, scan func(func(vaultprofile.Profile) error) error) (string, int64, int, int, error) {
	matchUUID := ""
	var matchGeneration int64
	matches := 0
	profileCount := 0
	err := scan(func(profile vaultprofile.Profile) error {
		profileCount++
		if profile.Attachment.ClientUUID != clientUUID {
			return nil
		}
		matches++
		if matches == 1 {
			matchUUID = profile.ProfileUUID
			matchGeneration = profile.Attachment.Generation
		}
		return nil
	})
	return matchUUID, matchGeneration, matches, profileCount, err
}
