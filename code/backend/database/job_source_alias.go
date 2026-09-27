package database

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/local/replicaro/models"
	"github.com/local/replicaro/storageidentity"
)

// UpdateJobSourceAlias saves the location chosen with "Update job source" as
// the job's alias (resolved_source_path) together with the storage facts
// observed at that location. It is the only path that may change a bound
// job's binding facts after creation (apart from the one-time legacy
// conversion); validateJobSourceTransition keeps rejecting every other
// attempt.
//
// Why an alias and not a new source: the immutable source is native retention
// and File History scope (Kopia's SourceInfo via --override-source, the File
// History grouping root, Kopia policy derivation). Rewriting it would split
// the job's history and restart native retention, so the source never changes
// here; backups simply read from the alias. source_storage_key stays the
// normalized immutable source path, and source_storage_json describes the
// location in use.
//
// expected is the job as read before the new location was probed. The update
// is a compare-and-swap on its source and alias, so two concurrent updates (or
// an update racing first binding) cannot silently overwrite each other; the
// loser gets ErrJobSourceChanged and can retry. The update is refused while
// any target of the job is queued or running (ErrJobRunActive) so an admitted
// run never changes location underneath itself, and while a vault connection
// reservation holds the job (ErrJobConnectionReserved).
//
// Pending catch-ups are kept. The per-target source observation is reset to
// "not checked" because it described the previous location; that also lets a
// paused catch-up be retried on the next scheduler tick instead of waiting out
// the five-minute unavailable recheck, and the next check probes the alias.
//
// When the alias actually changes (set, moved to another folder, or cleared
// back to the source) every target row of this job also gets
// full_source_read_pending. Kopia keeps the job's history under the immutable
// source through --override-source, so the first snapshot from the new folder
// finds the previous folder's manifests and reuses a file's stored content
// whenever name, size, mtime, mode and owner match. A folder whose files match
// the old folder's metadata but not its content would be recorded with the
// old bytes, silently, and every later snapshot would inherit them. While the
// flag is set, each backup of the (job, vault) pair hashes every file (the
// runner freezes it at activation and Kopia turns it into --force-hash=100);
// it stays set until one snapshot of that pair commits. No backup is started
// here: each pair picks the flag up on its next normal run. Other jobs on the
// same vault are not affected.
func UpdateJobSourceAlias(db *sql.DB, expected, updated models.BackupJob, now time.Time) error {
	if expected.ID == "" || expected.ID != updated.ID || expected.Source != updated.Source {
		return ErrJobSourceImmutable
	}
	releaseDefinition, err := acquireJobDefinitionUse(db, expected.ID)
	if err != nil {
		return err
	}
	defer releaseDefinition()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireJobConnectionUnreserved(tx, expected.ID); err != nil {
		return err
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE job_id = ? AND status IN ('queued', 'running')`, expected.ID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrJobRunActive
	}
	var name, source, state, alias string
	if err := tx.QueryRow(`SELECT name,source,source_binding_state,resolved_source_path FROM backup_jobs WHERE id=?`,
		expected.ID).Scan(&name, &source, &state, &alias); err != nil {
		return err
	}
	if normalizedJobSourceBindingState(state) != "bound" {
		// Imported jobs that were never bound here take the first-binding path,
		// which sets the source itself.
		return ErrJobSourceUnbound
	}
	if source != expected.Source || alias != expected.ResolvedSourcePath {
		return ErrJobSourceChanged
	}
	if updated.ResolvedSourcePath == source {
		return fmt.Errorf("source alias must differ from the immutable source")
	}
	if updated.ResolvedSourcePath != "" {
		// Same entry-point rule as a bound source: normalized, no control
		// characters. The probe already enforced it; this keeps the database
		// from accepting an alias no probe could have produced.
		if err := storageidentity.ValidateBindingPath(updated.ResolvedSourcePath); err != nil {
			return err
		}
	}
	if err := validateStorageBinding(updated.SourceStorageVersion, updated.SourceStorageKey,
		updated.SourceStorageDescriptorJSON, source); err != nil {
		return err
	}
	if legacyStorageBinding(updated.SourceStorageVersion) {
		return fmt.Errorf("source alias binding must use the current format")
	}
	updatedAt := now.UTC().Format(time.RFC3339Nano)
	observedAt := updatedAt
	if updated.ResolvedSourcePath == "" {
		observedAt = ""
	}
	result, err := tx.Exec(`UPDATE backup_jobs SET resolved_source_path=?,resolved_source_observed_at=?,
		source_storage_version=?,source_storage_key=?,source_storage_json=?
		WHERE id=? AND source=? AND source_binding_state='bound' AND resolved_source_path=?`,
		updated.ResolvedSourcePath, observedAt,
		updated.SourceStorageVersion, updated.SourceStorageKey, updated.SourceStorageDescriptorJSON,
		expected.ID, source, alias)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrJobSourceChanged
	}
	if _, err := tx.Exec(`UPDATE job_target_schedule_state
		SET source_availability=?,source_reason_code=?,source_checked_at='',updated_at=?
		WHERE job_id=?`, StorageUnknown, AvailabilityReasonNotChecked, updatedAt, expected.ID); err != nil {
		return err
	}
	if updated.ResolvedSourcePath != alias {
		// Re-saving the same alias (for example to record fresh facts) reads the
		// same folder, so Kopia's metadata match is still valid and no full
		// re-read is needed.
		if _, err := tx.Exec(`UPDATE job_target_schedule_state SET full_source_read_pending=1
			WHERE job_id=?`, expected.ID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO activity_log (timestamp, level, message) VALUES (datetime('now'), 'INFO', ?)`,
		"Backup job updated: "+name); err != nil {
		return err
	}
	return tx.Commit()
}

// markJobTargetFullSourceRead flags one newly inserted (job, vault) pair,
// inside the caller's transaction, so its backups re-read every source file.
// The pair's Kopia history lives under <jobID>@replicaro:<source>, and Kopia
// reuses a parent snapshot's stored content for any file whose name, size,
// mtime, mode and owner match. A new pair can already have such history, read
// from a different folder than the one its next backup reads:
//   - a vault added to an existing job (UpdateJob) may be one the job used
//     before, still holding snapshots taken from an older folder or alias;
//   - a recovered job attached to a vault (AttachRecoveredRepository) brings
//     that vault's snapshots of the job, taken on another computer;
//   - a job created with a restored or supplied ID (CreateJob) may have
//     snapshots in its vaults under that ID.
//
// Together with alias changes (UpdateJobSourceAlias) these are the only
// places the flag is set. A job created with a freshly generated ID starts at
// 0 on every pair: nothing can hold history under an ID that did not exist.
//
// The flag is not free, which is why it stays off that common path. While it
// is set, every backup attempt of the pair passes --force-hash=100 and hashes
// the whole source, and an attempt that follows an interrupted flagged run
// cannot reuse the checkpoint snapshots Kopia saved along the way; it is only
// cleared once a snapshot of the pair commits. For a new job's large first
// backup that would turn every retry into a full re-read.
func markJobTargetFullSourceRead(tx *sql.Tx, jobID, repositoryID string) error {
	_, err := tx.Exec(`UPDATE job_target_schedule_state SET full_source_read_pending=1
		WHERE job_id=? AND repository_id=?`, jobID, repositoryID)
	return err
}

// JobTargetFullSourceReadPending reports whether the next backup of this
// (job, vault) pair must re-read every source file: the job's source alias
// changed or the pair was created where the vault may already hold the job's
// history (see markJobTargetFullSourceRead), and the pair has not committed a
// snapshot since.
func JobTargetFullSourceReadPending(db *sql.DB, jobID, repositoryID string) (bool, error) {
	var pending int
	err := db.QueryRow(`SELECT full_source_read_pending FROM job_target_schedule_state
		WHERE job_id=? AND repository_id=?`, jobID, repositoryID).Scan(&pending)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return pending == 1, err
}

// ClearJobTargetFullSourceRead drops the pending full re-read once the engine
// has committed a snapshot for the pair. Every file in that snapshot was
// re-read from the current folder, so later snapshots can safely reuse its
// content again. A partial snapshot counts: files it could not read are not
// in it, so there is no stored content for them to reuse and the next run
// reads them in full anyway. Anything short of a committed snapshot (a
// failed, cancelled, interrupted or never-started run) must leave the flag
// set, otherwise the next run would go back to trusting metadata carried
// over from the previous folder.
func ClearJobTargetFullSourceRead(db *sql.DB, jobID, repositoryID string) error {
	_, err := db.Exec(`UPDATE job_target_schedule_state SET full_source_read_pending=0
		WHERE job_id=? AND repository_id=?`, jobID, repositoryID)
	return err
}
