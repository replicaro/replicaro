package repositoryadmission

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/storageavailability"
)

// ReconnectKopiaFilesystem durably changes only Kopia's isolated native
// configuration. Candidate aliases are committed by the existing intent only
// after native path, repository ID, and client identity readback succeeds.
func ReconnectKopiaFilesystem(ctx context.Context, db *sql.DB, repo models.Repository, candidate string, observedAt time.Time) error {
	priorPath := repo.ResolvedRepositoryPath
	if priorPath == "" {
		priorPath = repo.Location
	}
	candidateRepo := repo
	candidateRepo.Location = candidate
	candidateRepo.ResolvedRepositoryPath = candidate
	engine, err := engines.ResolveWithRepositoryAvailabilityCheck(candidateRepo, storageavailability.RequireRepositoryAvailable)
	if err != nil {
		return err
	}
	if priorPath == candidate {
		validationOutput, validationErr := engines.ValidateRepository(ctx, engine, candidateRepo)
		if validationErr != nil {
			return fmt.Errorf("validate active Kopia filesystem configuration: %w", validationErr)
		}
		fingerprint, fingerprintErr := engines.RepositoryFingerprint(candidateRepo, validationOutput)
		if fingerprintErr != nil || fingerprint != candidateRepo.NativeRepositoryID {
			return fmt.Errorf("the active Kopia configuration identifies a different native repository")
		}
		return database.SetResolvedRepositoryPath(db, repo.ID, repo.Location, candidate, observedAt)
	}
	_, priorSHA, err := engines.KopiaActiveConfigFingerprint(engine, repo)
	if err != nil {
		return fmt.Errorf("fingerprint active Kopia filesystem configuration: %w", err)
	}
	intent, err := database.ReserveKopiaFilesystemReconnect(db, repo.ID, priorPath, candidate, priorSHA)
	if err != nil {
		return err
	}
	return ContinueKopiaFilesystemReconnect(ctx, db, engine, candidateRepo, intent, observedAt)
}

// StageKopiaConnectionUpdate reuses the bounded reconnect artifacts for a
// confirmed same-UUID update. Native configuration is prepared separately,
// atomically activated, and read back before the database may publish the new
// configured location. This also covers a same-location credential refresh.
func StageKopiaConnectionUpdate(ctx context.Context, db *sql.DB, engine engines.Engine, saved, candidate models.Repository) (database.KopiaFilesystemReconnectIntent, error) {
	intent, err := database.FindKopiaFilesystemReconnect(db, saved.ID)
	if errors.Is(err, sql.ErrNoRows) {
		_, priorSHA, fingerprintErr := engines.KopiaActiveConfigFingerprint(engine, saved)
		if fingerprintErr != nil {
			return intent, fmt.Errorf("fingerprint active Kopia configuration: %w", fingerprintErr)
		}
		priorPath := saved.ResolvedRepositoryPath
		if priorPath == "" {
			priorPath = saved.Location
		}
		intent, err = database.ReserveKopiaFilesystemReconnect(db, saved.ID, priorPath, candidate.Location, priorSHA)
	}
	if err != nil {
		return intent, err
	}
	if intent.CandidatePath != candidate.Location {
		return intent, fmt.Errorf("a different Kopia connection update is already pending")
	}
	paths, err := engines.KopiaFilesystemReconnectArtifacts(engine, candidate, intent.IntentID)
	if err != nil {
		return intent, err
	}
	if intent.State == "prepared" {
		if intent.StagedConfigSHA == "" {
			paths, _, err = engines.PrepareKopiaFilesystemReconnect(ctx, engine, candidate, intent.IntentID, intent.PriorConfigSHA)
			if err != nil {
				return intent, err
			}
			intent.StagedConfigSHA, err = engines.KopiaReconnectConfigFingerprint(paths.Staged)
			if err != nil {
				return intent, err
			}
			if err := database.SetKopiaFilesystemReconnectStaged(db, intent.RepositoryID, intent.IntentID, intent.StagedConfigSHA); err != nil {
				return intent, err
			}
		}
		activeSHA, activeErr := engines.KopiaReconnectConfigFingerprint(paths.Active)
		switch {
		case activeErr == nil && activeSHA == intent.PriorConfigSHA:
			// Preparation is reversible staging; activation replaces the live Kopia
			// config. Honor an already-won cancellation immediately before that first
			// consequential mutation so exact retries retain truthful pending state.
			if err := ctx.Err(); err != nil {
				return intent, err
			}
			if err := engines.ActivateKopiaFilesystemReconnect(paths, intent.PriorConfigSHA, intent.StagedConfigSHA); err != nil {
				return intent, err
			}
		case activeErr == nil && activeSHA == intent.StagedConfigSHA:
			// Activation completed before its database transition was durable.
		default:
			return intent, fmt.Errorf("active Kopia configuration matches neither side of the connection update")
		}
		if _, _, err := engines.VerifyActiveKopiaFilesystemReconnect(ctx, engine, candidate, intent.IntentID, intent.StagedConfigSHA); err != nil {
			return intent, err
		}
		if err := database.MarkKopiaFilesystemReconnectActivated(db, intent.RepositoryID, intent.IntentID); err != nil {
			return intent, err
		}
		intent.State = "activated"
	}
	if intent.State == "activated" {
		if _, _, err := engines.VerifyActiveKopiaFilesystemReconnect(ctx, engine, candidate, intent.IntentID, intent.StagedConfigSHA); err != nil {
			return intent, err
		}
		if err := database.CommitKopiaConnectionUpdate(db, intent); err != nil {
			return intent, err
		}
		intent.State = "committed"
	}
	if intent.State != "committed" && intent.State != "cleanup" {
		return intent, fmt.Errorf("Kopia connection update has invalid recovery state %q", intent.State)
	}
	return intent, nil
}

// CleanupKopiaConnectionUpdate runs only after the database transaction has
// published the reviewed row and advanced the intent to cleanup.
func CleanupKopiaConnectionUpdate(ctx context.Context, db *sql.DB, engine engines.Engine, repo models.Repository, intent database.KopiaFilesystemReconnectIntent) error {
	intent, err := database.FindKopiaFilesystemReconnect(db, repo.ID)
	if err != nil {
		return err
	}
	if intent.State != "cleanup" {
		return fmt.Errorf("Kopia connection update is not ready for cleanup")
	}
	paths, _, err := engines.VerifyActiveKopiaFilesystemReconnect(ctx, engine, repo, intent.IntentID, intent.StagedConfigSHA)
	if err != nil {
		return err
	}
	if err := engines.CleanupKopiaFilesystemReconnect(paths); err != nil {
		return err
	}
	return database.DeleteKopiaFilesystemReconnect(db, intent.RepositoryID, intent.IntentID)
}

// ContinueKopiaFilesystemReconnect is exported only so startup recovery can
// resume the same narrow durable intent; it is not a generalized workflow.
func ContinueKopiaFilesystemReconnect(ctx context.Context, db *sql.DB, engine engines.Engine, repo models.Repository, intent database.KopiaFilesystemReconnectIntent, observedAt time.Time) error {
	paths, err := engines.KopiaFilesystemReconnectArtifacts(engine, repo, intent.IntentID)
	if err != nil {
		return err
	}
	if intent.State == "prepared" {
		if intent.StagedConfigSHA == "" {
			paths, _, err = engines.PrepareKopiaFilesystemReconnect(ctx, engine, repo, intent.IntentID, intent.PriorConfigSHA)
			if err != nil {
				return err
			}
			intent.StagedConfigSHA, err = engines.KopiaReconnectConfigFingerprint(paths.Staged)
			if err != nil {
				return err
			}
			if err := database.SetKopiaFilesystemReconnectStaged(db, intent.RepositoryID, intent.IntentID, intent.StagedConfigSHA); err != nil {
				return err
			}
		}
		activeSHA, activeErr := engines.KopiaReconnectConfigFingerprint(paths.Active)
		switch {
		case activeErr == nil && activeSHA == intent.PriorConfigSHA:
			// Startup/operation continuation shares the same last cancellation gate;
			// neither path may activate an already-cancelled staged candidate.
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := engines.ActivateKopiaFilesystemReconnect(paths, intent.PriorConfigSHA, intent.StagedConfigSHA); err != nil {
				return err
			}
		case activeErr == nil && activeSHA == intent.StagedConfigSHA:
			// Activation completed before its database transition was durable.
		default:
			return fmt.Errorf("active Kopia configuration matches neither side of the reconnect intent")
		}
		if _, _, err := engines.VerifyActiveKopiaFilesystemReconnect(ctx, engine, repo, intent.IntentID, intent.StagedConfigSHA); err != nil {
			return err
		}
		if err := database.MarkKopiaFilesystemReconnectActivated(db, intent.RepositoryID, intent.IntentID); err != nil {
			return err
		}
		intent.State = "activated"
	}
	if intent.State == "activated" {
		if _, _, err := engines.VerifyActiveKopiaFilesystemReconnect(ctx, engine, repo, intent.IntentID, intent.StagedConfigSHA); err != nil {
			return err
		}
		if _, connectionErr := database.FindRepositoryConnectionIntent(db, intent.RepositoryID); connectionErr == nil {
			if err := database.CommitKopiaConnectionUpdate(db, intent); err != nil {
				return err
			}
		} else if errors.Is(connectionErr, sql.ErrNoRows) {
			if err := database.CommitKopiaFilesystemReconnect(db, intent, observedAt); err != nil {
				return err
			}
		} else {
			return connectionErr
		}
		intent.State = "committed"
	}
	if intent.State == "committed" {
		if _, _, err := engines.VerifyActiveKopiaFilesystemReconnect(ctx, engine, repo, intent.IntentID, intent.StagedConfigSHA); err != nil {
			return err
		}
		if _, connectionErr := database.FindRepositoryConnectionIntent(db, intent.RepositoryID); connectionErr == nil {
			// The confirmed connection transaction alone publishes the configured
			// location and advances cleanup; startup recovery must not substitute an
			// observational alias commit for that exact saved-row update.
			return nil
		} else if !errors.Is(connectionErr, sql.ErrNoRows) {
			return connectionErr
		}
		if err := database.MarkKopiaFilesystemReconnectCleanup(db, intent.RepositoryID, intent.IntentID); err != nil {
			return err
		}
		intent.State = "cleanup"
	}
	if intent.State == "cleanup" {
		if err := engines.CleanupKopiaFilesystemReconnect(paths); err != nil {
			return err
		}
		return database.DeleteKopiaFilesystemReconnect(db, intent.RepositoryID, intent.IntentID)
	}
	return nil
}
