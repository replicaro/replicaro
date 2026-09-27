package database

import (
	"context"
	"database/sql"
	"errors"
	"os"

	"github.com/local/replicaro/models"
)

// File History grouping roots
//
// The metadata cache keys files by (root, parent_path, name). The root a
// snapshot's files are cataloged under is its "grouping root"
// (metadata_cache_snapshot_roots.normalized_root); the native restore address
// is kept separately (metadata_cache_snapshot_roots.path). Until jobs could
// read from a source alias the two were always equal.
//
// A job whose source was moved with "Update job source" reads from the alias
// path. Kopia keeps the job's history together on its own, because the backup
// passes the immutable source through --override-source, so its recorded root
// never changes. Restic has no equivalent: it records the path it actually
// read, so without regrouping File History would show the job as two sources
// and split every file's history at the move. Restic snapshots of a job known
// on this computer are therefore grouped by the job's immutable source.
//
// Details that are easy to "simplify" by mistake:
//
//   - Grouping applies to every single-root managed Restic snapshot, not only
//     snapshots taken from an alias, so a difference in how Restic spells the
//     path never splits a job's history either.
//   - Only the grouping root changes. Restore addressing, Restore-page browse,
//     whole-snapshot restore, and every engine command keep using the native
//     root (SourceRoots / the "path" column). Restoring an alias snapshot must
//     make Restic read the alias path it recorded; using the grouping root
//     there would address a path that does not exist in the snapshot.
//   - Kopia is deliberately left alone. Dropping --override-source to make
//     Kopia record the alias would restart its per-source retention and leave
//     the old source's snapshots unpruned forever.
//   - Entry-set sharing stays safe because the entry-set lookup scope is the
//     snapshot's grouping roots (metadataRootScope, built from
//     normalizedMetadataSnapshotRoots). A cached entry set references file
//     rows keyed by grouping root, so it is reused only by a snapshot cataloged
//     under the same grouping root. Keep the grouping root in that scope.
//   - Unmanaged snapshots (no valid marker, another profile, or a job that is
//     no longer on this computer) keep their native root.

// MetadataGroupingRoot decides the File History grouping root of one snapshot.
// It returns the job's immutable source for a single-root Restic snapshot that
// carries the exact marker of a job known on this computer for this vault
// profile, and "" (group by the native root) for everything else. jobSources
// maps each job targeting the vault to its immutable source, as returned by
// JobSourcesForRepository. The result is applied through
// models.Snapshot.MetadataGroupingRoot and consumed only by
// normalizedMetadataSnapshotRoots.
func MetadataGroupingRoot(engine, profileUUID string, snapshot models.Snapshot, jobSources map[string]string) string {
	if engine != "restic" || len(metadataSnapshotRoots(snapshot)) != 1 {
		return ""
	}
	marker := snapshot.OwnershipMarker
	if marker.Status != models.SnapshotOwnershipValid || marker.ProfileID == "" || marker.ProfileID != profileUUID {
		return ""
	}
	return jobSources[marker.JobID]
}

// JobSourcesForRepository maps every job targeting the vault to its immutable
// source. It is the "known locally" set for File History grouping and returns
// the same job IDs as JobIDsForRepository.
func JobSourcesForRepository(db *sql.DB, repositoryID string) (map[string]string, error) {
	rows, err := db.Query(`SELECT j.id,j.source FROM backup_job_targets t
		JOIN backup_jobs j ON j.id=t.job_id WHERE t.repository_id=?`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]string{}
	for rows.Next() {
		var id, source string
		if err := rows.Scan(&id, &source); err != nil {
			return nil, err
		}
		result[id] = source
	}
	return result, rows.Err()
}

// RefreshStaleMetadataGrouping is called after a committed local job change
// that can move a vault's Restic snapshots between grouping roots: a job
// becoming known or unknown to a vault (creation with an existing job UUID,
// deletion, target changes, dormant-definition import) or an unbound imported
// job's source being set at first binding.
//
// It compares every cached single-root snapshot's stored grouping root with
// what MetadataGroupingRoot decides under the current jobs, and only when one
// differs bumps the vault's required metadata generation. The existing
// complete header reconciliation then re-derives roots and re-indexes only
// those snapshots, while File History stays gated so it never shows the stale
// grouping. The common case, a job whose snapshots were recorded at its
// source path, needs no refresh and leaves File History untouched; bumping
// unconditionally would make File History unavailable after every job edit
// until the vault could be listed again, which for an offline vault can be a
// long time.
//
// This is best effort and runs after the job transaction on purpose: opening
// a metadata cache can take a while and must not hold the main writer. If it
// fails, or a refresh that read the old jobs publishes afterwards, the result
// is only a stale presentation that the next header refresh corrects; restore
// addressing never depends on grouping roots.
func RefreshStaleMetadataGrouping(db *sql.DB, repositoryIDs []string) {
	seen := make(map[string]bool, len(repositoryIDs))
	for _, repositoryID := range repositoryIDs {
		if repositoryID == "" || seen[repositoryID] {
			continue
		}
		seen[repositoryID] = true
		stale, err := metadataGroupingStale(context.Background(), db, repositoryID)
		if err != nil || !stale {
			continue
		}
		_, _ = db.Exec(`UPDATE repositories SET required_generation=required_generation+1 WHERE id=?`, repositoryID)
	}
}

func metadataGroupingStale(ctx context.Context, db *sql.DB, repositoryID string) (bool, error) {
	var engine, profileUUID string
	if err := db.QueryRowContext(ctx, `SELECT engine,profile_uuid FROM repositories WHERE id=?`, repositoryID).
		Scan(&engine, &profileUUID); err != nil {
		return false, err
	}
	if engine != "restic" {
		// Only Restic snapshots are regrouped; other engines always use the
		// native root, which no job change can alter.
		return false, nil
	}
	cachePath, err := MetadataCachePath(db, repositoryID)
	if err != nil {
		return false, err
	}
	if _, err := os.Lstat(cachePath); errors.Is(err, os.ErrNotExist) {
		// Never indexed: the first refresh derives roots from current jobs.
		return false, nil
	}
	jobSources, err := JobSourcesForRepository(db, repositoryID)
	if err != nil {
		return false, err
	}
	handle, _, release, err := metadataCacheReader(ctx, db, repositoryID)
	if err != nil {
		return false, err
	}
	defer release()
	rows, err := handle.readers.QueryContext(ctx, `SELECT s.snapshot_id,s.marker_status,s.marker_profile_uuid,s.marker_job_uuid,
			r.path,r.normalized_root,r.native_user,r.native_host
		FROM metadata_cache_snapshots s
		JOIN metadata_cache_snapshot_roots r ON r.repository_database_id=s.repository_database_id AND r.snapshot_id=s.snapshot_id
		WHERE (SELECT COUNT(*) FROM metadata_cache_snapshot_roots c
			WHERE c.repository_database_id=s.repository_database_id AND c.snapshot_id=s.snapshot_id)=1`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var snapshot models.Snapshot
		var markerStatus, stored string
		var root models.SnapshotSourceRoot
		if err := rows.Scan(&snapshot.ID, &markerStatus, &snapshot.OwnershipMarker.ProfileID, &snapshot.OwnershipMarker.JobID,
			&root.Path, &stored, &root.User, &root.Host); err != nil {
			return false, err
		}
		snapshot.OwnershipMarker.Status = models.SnapshotOwnershipMarkerStatus(markerStatus)
		snapshot.SourceRoots = []models.SnapshotSourceRoot{root}
		want := MetadataGroupingRoot(engine, profileUUID, snapshot, jobSources)
		if want == "" {
			want = root.Path
		}
		if normalizeMetadataRoot(want) != stored {
			return true, nil
		}
	}
	return false, rows.Err()
}

func jobTargetRepositoryIDs(db *sql.DB, jobID string) []string {
	rows, err := db.Query(`SELECT repository_id FROM backup_job_targets WHERE job_id=?`, jobID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var repositoryID string
		if rows.Scan(&repositoryID) == nil {
			result = append(result, repositoryID)
		}
	}
	return result
}

// metadataGroupingRootsSettingKey records that the one-time grouping-root
// refresh ran after upgrading to the version that introduced grouping roots.
// It is a plain settings row so no schema change is needed; SaveSettings
// preserves it.
const metadataGroupingRootsSettingKey = "metadataGroupingRootsV1"

// refreshMetadataGroupingRootsOnce bumps every Restic vault's required
// metadata generation exactly once after upgrade. Existing caches were built
// when the grouping root always equaled the native root; the bump makes the
// next File History or Restore access run the ordinary complete header
// reconciliation, which re-derives grouping roots and re-indexes only
// snapshots whose root changed. This replaces a cache schema change or a
// forced full rebuild.
//
// Only Restic vaults are bumped: MetadataGroupingRoot never regroups another
// engine's snapshots, so for Kopia (and any other engine) the bump would only
// make File History wait for a full vault listing, which for an offline
// vault can be a long time, and change nothing.
func refreshMetadataGroupingRootsOnce(tx *sql.Tx) error {
	result, err := tx.Exec(`INSERT INTO settings (key,value) VALUES (?, '1') ON CONFLICT(key) DO NOTHING`,
		metadataGroupingRootsSettingKey)
	if err != nil {
		return err
	}
	inserted, err := result.RowsAffected()
	if err != nil || inserted == 0 {
		return err
	}
	_, err = tx.Exec(`UPDATE repositories SET required_generation=required_generation+1 WHERE engine='restic'`)
	return err
}

// JobSnapshotTopLevelNames are the names directly inside the source root of
// one cached snapshot, used only by the "Update job source" folder check.
type JobSnapshotTopLevelNames struct {
	RepositoryID string
	SnapshotID   string
	Timestamp    string
	Names        []string
}

// LatestReadyJobSnapshotTopLevelNames returns the top-level names of the most
// recent snapshot of the job, across all of its vaults, whose metadata cache
// entry is ready. found is false when no vault has such a snapshot; the folder
// check then falls back to its name-only checks. Only Replicaro's own cache is
// read: no engine command runs and nothing is written except what opening a
// cache handle does anyway. A vault whose cache cannot be read right now (being
// rebuilt, closed, or incompatible) is skipped, because the check is an
// advisory warning and must not block the source update.
func LatestReadyJobSnapshotTopLevelNames(ctx context.Context, db *sql.DB, jobID string) (JobSnapshotTopLevelNames, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT r.id,r.profile_uuid FROM backup_job_targets t
		JOIN repositories r ON r.id=t.repository_id WHERE t.job_id=? ORDER BY r.id`, jobID)
	if err != nil {
		return JobSnapshotTopLevelNames{}, false, err
	}
	type vault struct{ id, profile string }
	vaults := []vault{}
	for rows.Next() {
		var item vault
		if err := rows.Scan(&item.id, &item.profile); err != nil {
			_ = rows.Close()
			return JobSnapshotTopLevelNames{}, false, err
		}
		vaults = append(vaults, item)
	}
	if err := rows.Close(); err != nil {
		return JobSnapshotTopLevelNames{}, false, err
	}
	var best JobSnapshotTopLevelNames
	found := false
	for _, item := range vaults {
		if err := ctx.Err(); err != nil {
			return JobSnapshotTopLevelNames{}, false, err
		}
		candidate, ok, err := latestReadyJobSnapshotInVault(ctx, db, item.id, item.profile, jobID)
		if err != nil {
			if ctx.Err() != nil {
				return JobSnapshotTopLevelNames{}, false, ctx.Err()
			}
			continue
		}
		if ok && (!found || candidate.Timestamp > best.Timestamp ||
			candidate.Timestamp == best.Timestamp && candidate.SnapshotID > best.SnapshotID) {
			best, found = candidate, true
		}
	}
	return best, found, nil
}

func latestReadyJobSnapshotInVault(ctx context.Context, db *sql.DB, repositoryID, profileUUID, jobID string) (JobSnapshotTopLevelNames, bool, error) {
	if profileUUID == "" {
		return JobSnapshotTopLevelNames{}, false, nil
	}
	authority, err := LoadMetadataReadAuthority(ctx, db, repositoryID)
	if err != nil {
		return JobSnapshotTopLevelNames{}, false, err
	}
	handle, _, release, err := metadataCacheReaderForAuthority(ctx, db, authority)
	if err != nil {
		return JobSnapshotTopLevelNames{}, false, err
	}
	defer release()
	tx, err := handle.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return JobSnapshotTopLevelNames{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	repositoryDatabaseID, err := metadataRepositoryDatabaseIDForAuthority(ctx, tx, authority)
	if err != nil {
		return JobSnapshotTopLevelNames{}, false, err
	}
	// Timestamps are stored in one canonical fixed-width UTC layout, so the
	// text order is the time order.
	result := JobSnapshotTopLevelNames{RepositoryID: repositoryID}
	err = tx.QueryRowContext(ctx, `SELECT snapshot_id,timestamp FROM metadata_cache_snapshots
		WHERE repository_database_id=? AND status='ready' AND marker_status='valid'
		  AND marker_job_uuid=? AND marker_profile_uuid=?
		ORDER BY timestamp DESC,snapshot_id DESC LIMIT 1`,
		repositoryDatabaseID, jobID, profileUUID).Scan(&result.SnapshotID, &result.Timestamp)
	if errors.Is(err, sql.ErrNoRows) {
		return JobSnapshotTopLevelNames{}, false, tx.Commit()
	}
	if err != nil {
		return JobSnapshotTopLevelNames{}, false, err
	}
	// Same membership walk as Restore browse at the snapshot root: the names
	// the user would see when opening this snapshot.
	names, err := tx.QueryContext(ctx, `SELECT DISTINCT f.name
		FROM metadata_cache_snapshots s
		JOIN metadata_cache_entry_sets setrow ON setrow.entry_set_id=s.entry_set_id
		JOIN metadata_cache_snapshot_roots r ON r.repository_database_id=s.repository_database_id AND r.snapshot_id=s.snapshot_id
		JOIN metadata_cache_state state ON state.repository_database_id=s.repository_database_id
		CROSS JOIN metadata_cache_files f ON f.repository_database_id=s.repository_database_id AND f.root=r.normalized_root AND f.parent_path=''
		CROSS JOIN metadata_cache_block_list_items l ON l.block_list_id=setrow.block_list_id AND l.bucket=f.file_id % state.bucket_count
		CROSS JOIN metadata_cache_block_entries e ON e.block_id=l.block_id AND e.file_id=f.file_id
		WHERE s.repository_database_id=? AND s.snapshot_id=? AND f.name<>''`, repositoryDatabaseID, result.SnapshotID)
	if err != nil {
		return JobSnapshotTopLevelNames{}, false, err
	}
	for names.Next() {
		var name string
		if err := names.Scan(&name); err != nil {
			_ = names.Close()
			return JobSnapshotTopLevelNames{}, false, err
		}
		result.Names = append(result.Names, name)
	}
	if err := names.Close(); err != nil {
		return JobSnapshotTopLevelNames{}, false, err
	}
	if err := names.Err(); err != nil {
		return JobSnapshotTopLevelNames{}, false, err
	}
	return result, true, tx.Commit()
}
