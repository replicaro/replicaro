package artifactrecovery

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
)

// Reconcile resolves crash-interrupted Restic/Kopia artifact stages before any
// worker or API endpoint can use repository credentials.
func Reconcile(db *sql.DB) error {
	stages, legacy, scanErr := engines.PendingRepositoryArtifactStages()
	var errs []error
	if scanErr != nil {
		errs = append(errs, scanErr)
	}
	for _, item := range legacy {
		message := "Legacy local engine artifact quarantine requires manual cleanup: " + bounded(item, 160)
		if err := database.LogError(db, message); err != nil {
			errs = append(errs, fmt.Errorf("record legacy artifact quarantine: %w", err))
		}
	}
	for _, stage := range stages {
		if err := reconcileStage(db, stage); err != nil {
			manifest := stage.Manifest()
			message := fmt.Sprintf("Local %s artifact recovery for repository %s requires attention", manifest.Engine, bounded(manifest.RepositoryID, 80))
			_ = database.LogError(db, message)
			errs = append(errs, fmt.Errorf("%s: %w", message, err))
		}
	}
	return errors.Join(errs...)
}

func reconcileStage(db *sql.DB, stage *engines.RepositoryArtifactStage) error {
	manifest := stage.Manifest()
	repo, err := database.GetRepository(db, manifest.RepositoryID)
	present := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if present && repo.Engine != manifest.Engine {
		return fmt.Errorf("repository engine does not match recovery manifest")
	}

	committed := !present
	if present && manifest.Action == engines.RepositoryArtifactCredentialRotation {
		digest, digestErr := engines.RepositoryOptionsDigest(repo)
		if digestErr != nil {
			return digestErr
		}
		committed = digest != manifest.OldOptionsSHA256
	}

	// Stage methods inspect original/quarantine collision states and refuse an
	// unsafe overwrite. A committed mutation always discards only quarantine.
	if committed {
		return stage.Finalize()
	}
	return stage.Restore()
}

func bounded(value string, limit int) string {
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, value)
	if len(value) > limit {
		return value[:limit]
	}
	return value
}
