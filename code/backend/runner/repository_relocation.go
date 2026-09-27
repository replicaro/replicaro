package runner

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultlock"
)

func continueKopiaFilesystemReconnect(ctx context.Context, db *sql.DB, engine engines.Engine, repo models.Repository, intent database.KopiaFilesystemReconnectIntent) error {
	return repositoryadmission.ContinueKopiaFilesystemReconnect(ctx, db, engine, repo, intent)
}

// RecoverKopiaFilesystemReconnects resumes confirmed Kopia connection
// updates (and finishes staged-file cleanup) without making one unavailable
// vault a process-wide startup dependency. The intent stays saved and fences
// only its repository until a later retry can finish it.
// Automatic-relocation intents are never resumed; database.Migrate
// discards them before this runs.
func RecoverKopiaFilesystemReconnects(ctx context.Context, db *sql.DB) error {
	intents, err := database.ListKopiaFilesystemReconnects(db)
	if err != nil {
		return err
	}
	for _, intent := range intents {
		if err := recoverKopiaFilesystemReconnect(ctx, db, intent); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			recoveryErr := fmt.Errorf("recover Kopia reconnect for %s: %w", intent.RepositoryID, err)
			if recordErr := database.RecordKopiaFilesystemReconnectError(db, intent.RepositoryID, intent.IntentID, recoveryErr); recordErr != nil && !errors.Is(recordErr, sql.ErrNoRows) {
				return errors.Join(recoveryErr, recordErr)
			}
			log.Printf("Kopia reconnect deferred for vault %s: %v", intent.RepositoryID, recoveryErr)
		}
	}
	return nil
}

func recoverKopiaFilesystemReconnect(ctx context.Context, db *sql.DB, listed database.KopiaFilesystemReconnectIntent) error {
	repo, err := database.GetRepository(db, listed.RepositoryID)
	if err != nil {
		return err
	}
	if repo.Engine != engines.KopiaID {
		return fmt.Errorf("saved Kopia reconnect no longer names a Kopia vault")
	}
	unlock, err := vaultlock.AcquireExclusiveContext(ctx, repo.ID)
	if err != nil {
		return err
	}
	defer unlock()
	// A normal operation may have finished this intent while startup recovery
	// waited for the repository lock. Re-read under the lock so a stale entry
	// from the earlier listing can't be replayed or overwrite newer state.
	intent, err := database.FindKopiaFilesystemReconnect(db, listed.RepositoryID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if intent.IntentID != listed.IntentID {
		return fmt.Errorf("Kopia reconnect intent changed while waiting for its vault lock")
	}
	if repo.Connector != "fs" && intent.State == "cleanup" && repo.Location != intent.CandidatePath {
		return fmt.Errorf("remote Kopia cleanup does not follow the committed configured location")
	}
	if repo.Connector != "fs" && intent.State != "cleanup" {
		connection, connectionErr := database.FindRepositoryConnectionIntent(db, repo.ID)
		if connectionErr != nil {
			return fmt.Errorf("remote Kopia reconnect has no pending confirmed connection update: %w", connectionErr)
		}
		if connection.Location != intent.CandidatePath {
			return fmt.Errorf("remote Kopia reconnect differs from its pending confirmed connection update")
		}
		if intent.State == "prepared" && intent.StagedConfigSHA == "" {
			// Remote credentials are intentionally absent from durable recovery state.
			// Startup may finish already staged bytes, but only the exact API retry can
			// supply credentials for a connection that never reached native staging.
			return fmt.Errorf("remote Kopia connection update requires exact credential retry before staging")
		}
	}
	if repo.Connector == "fs" && intent.State != "cleanup" {
		if _, connectionErr := database.FindRepositoryConnectionIntent(db, repo.ID); connectionErr != nil {
			return fmt.Errorf("filesystem Kopia reconnect has no pending confirmed connection update: %w", connectionErr)
		}
	}
	configured := repo.Location
	repo.Location = intent.CandidatePath
	engine, err := engines.ResolveWithRepositoryAvailabilityCheck(repo, storageavailability.RequireRepositoryAvailable)
	if err != nil {
		return err
	}
	if intent.State == "prepared" && intent.StagedConfigSHA == "" {
		paths, pathErr := engines.KopiaFilesystemReconnectArtifacts(engine, repo, intent.IntentID)
		if pathErr != nil {
			return pathErr
		}
		if activeSHA, activeErr := engines.KopiaReconnectConfigFingerprint(paths.Active); activeErr != nil || activeSHA != intent.PriorConfigSHA {
			return fmt.Errorf("prepared Kopia reconnect has no staged fingerprint and its active config changed")
		}
		_ = os.Remove(paths.Staged)
	}
	// Preserve the immutable configured location through the commit helper.
	repo.Location = configured
	candidateRepo := repo
	candidateRepo.Location = intent.CandidatePath
	return continueKopiaFilesystemReconnect(ctx, db, engine, candidateRepo, intent)
}
