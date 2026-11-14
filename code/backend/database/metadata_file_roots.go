package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"strings"

	"github.com/local/replicaro/models"
	"modernc.org/sqlite"
)

// A single-file source is cached as a source row (root, '', '') that holds the
// file's own version (is_dir=0). A folder source row never holds a version,
// so "the source row has a non-directory version" is what tells the two apart.
// The lookups in this file are separate small queries, so the folder queries
// do not depend on file roots at all.

// MetadataFileRootName returns the name File History shows and searches for a
// single-file source: the last element of the root the engine recorded.
//
// The separator rule follows how the root was spelled. A root that starts with
// "/" is a POSIX path, where a backslash is an ordinary character in a name,
// so only "/" separates. Anything else is a Windows drive or UNC path
// (C:\Users\me\report.txt, \\server\share\report.txt), where either slash
// separates.
//
// This name is for matching and display only. Restores never use it to address
// anything: Restic splits its own archive tree path, and Kopia restores under
// the name its fresh root entry reports.
func MetadataFileRootName(root string) string {
	separators := `/\`
	if strings.HasPrefix(root, "/") {
		separators = "/"
	}
	return root[strings.LastIndexAny(root, separators)+1:]
}

func init() {
	sqlite.MustRegisterDeterministicScalarFunction("replicaro_file_root_name", 1,
		func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
			switch value := args[0].(type) {
			case string:
				return MetadataFileRootName(value), nil
			case []byte:
				return MetadataFileRootName(string(value)), nil
			default:
				return value, nil
			}
		})
}

// Matching single-file sources for search. It starts from the distinct
// recorded roots, which are few, instead of scanning every source row in the
// catalog. A root with an empty name never matches: the pattern always holds at
// least the two % around the query, and an empty query is refused earlier.
const metadataFileRootSearchSQL = `SELECT roots.root FROM (
		SELECT DISTINCT normalized_root AS root FROM metadata_cache_snapshot_roots WHERE repository_database_id=?
	) roots
	JOIN metadata_cache_files f ON f.repository_database_id=? AND f.root=roots.root AND f.parent_path='' AND f.name=''
	WHERE replicaro_file_root_name(roots.root)<>''
		AND replicaro_search_fold(replicaro_file_root_name(roots.root)) LIKE ? ESCAPE char(92)
		AND EXISTS(SELECT 1 FROM metadata_cache_file_versions v WHERE v.file_id=f.file_id AND v.is_dir=0)
	ORDER BY roots.root COLLATE BINARY`

func metadataFileRootSearchTx(ctx context.Context, tx *sql.Tx, repositoryDatabaseID int64, pattern string) ([]models.FileSearchResult, error) {
	rows, err := tx.QueryContext(ctx, metadataFileRootSearchSQL, repositoryDatabaseID, repositoryDatabaseID, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []models.FileSearchResult{}
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		result = append(result, models.FileSearchResult{Source: root, Name: MetadataFileRootName(root)})
	}
	return result, rows.Err()
}

// metadataFileRootSourcesTx returns which of the given source rows hold a file
// version.
func metadataFileRootSourcesTx(ctx context.Context, tx *sql.Tx, repositoryDatabaseID int64, roots []string) (map[string]bool, error) {
	result := map[string]bool{}
	if len(roots) == 0 {
		return result, nil
	}
	args := []any{repositoryDatabaseID}
	for _, root := range roots {
		args = append(args, root)
	}
	rows, err := tx.QueryContext(ctx, `SELECT f.root FROM metadata_cache_files f
		WHERE f.repository_database_id=? AND f.parent_path='' AND f.name='' AND f.root IN (`+makePlaceholders(len(roots))+`)
		AND EXISTS(SELECT 1 FROM metadata_cache_file_versions v WHERE v.file_id=f.file_id AND v.is_dir=0)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, err
		}
		result[root] = true
	}
	return result, rows.Err()
}

// markMetadataFileRootSourcesTx turns the single-file sources of one source
// list page into file rows named by their file. Only a source with nothing to
// expand can be one; a root that holds a file in some snapshots and a folder in
// others has children and stays a folder source, so a page of folder sources
// with contents costs no extra query at all.
func markMetadataFileRootSourcesTx(ctx context.Context, tx *sql.Tx, repositoryDatabaseID int64, items []models.FileBrowseEntry) error {
	candidates := []string{}
	for _, item := range items {
		if !item.CanExpand {
			candidates = append(candidates, item.Source)
		}
	}
	files, err := metadataFileRootSourcesTx(ctx, tx, repositoryDatabaseID, candidates)
	if err != nil {
		return err
	}
	for index := range items {
		if !items[index].CanExpand && files[items[index].Source] {
			items[index].Name, items[index].IsDir = MetadataFileRootName(items[index].Source), false
		}
	}
	return nil
}

// MetadataSourceHoldsFileVersionForAuthority reports whether a File History
// source holds a single file. It reads the cache without the readiness gate:
// the history request that follows applies the gate and checks this again in
// its own read.
func MetadataSourceHoldsFileVersionForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority, source string) (bool, error) {
	handle, _, release, err := metadataCacheReaderForAuthority(ctx, db, authority)
	if err != nil {
		return false, err
	}
	defer release()
	tx, err := handle.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	repositoryDatabaseID, err := metadataRepositoryDatabaseIDForAuthority(ctx, tx, authority)
	if err != nil {
		return false, err
	}
	source = normalizeMetadataRoot(source)
	files, err := metadataFileRootSourcesTx(ctx, tx, repositoryDatabaseID, []string{source})
	if err != nil {
		return false, err
	}
	return files[source], tx.Commit()
}

// Which native roots of ready snapshots are single files in that snapshot.
// The version check on the source row comes first, so folder roots stop there
// without reading any snapshot membership.
const metadataSnapshotFileRootsSQL = `SELECT s.snapshot_id,r.path,r.native_user,r.native_host
	FROM metadata_cache_snapshots s
	JOIN metadata_cache_entry_sets setrow ON setrow.entry_set_id=s.entry_set_id
	JOIN metadata_cache_snapshot_roots r ON r.repository_database_id=s.repository_database_id AND r.snapshot_id=s.snapshot_id
	JOIN metadata_cache_state state ON state.repository_database_id=s.repository_database_id
	CROSS JOIN metadata_cache_files f ON f.repository_database_id=s.repository_database_id AND f.root=r.normalized_root
		AND f.parent_path='' AND f.name=''
		AND EXISTS(SELECT 1 FROM metadata_cache_file_versions fv WHERE fv.file_id=f.file_id AND fv.is_dir=0)
	CROSS JOIN metadata_cache_block_list_items l ON l.block_list_id=setrow.block_list_id AND l.bucket=f.file_id % state.bucket_count
	CROSS JOIN metadata_cache_block_entries e ON e.block_id=l.block_id AND e.file_id=f.file_id
	JOIN metadata_cache_file_versions v ON v.file_id=f.file_id AND v.file_version_id=e.file_version_id AND v.is_dir=0
	WHERE s.repository_database_id=? AND s.status='ready'`

// MetadataSnapshotFileRootsForAuthority returns, for each ready snapshot, the
// native roots that the snapshot recorded as a single file, by root identity.
// snapshotIDs limits the lookup; nil reads every ready snapshot. Snapshots that
// aren't ready yet are absent, so callers treat "absent" as "not known to be a
// file" and keep any server-side refusal of their own.
func MetadataSnapshotFileRootsForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority, snapshotIDs []string) (map[string]map[string]models.SnapshotSourceRoot, error) {
	if snapshotIDs != nil && len(snapshotIDs) == 0 {
		return map[string]map[string]models.SnapshotSourceRoot{}, nil
	}
	handle, _, release, err := metadataCacheReaderForAuthority(ctx, db, authority)
	if err != nil {
		return nil, err
	}
	defer release()
	tx, err := handle.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	repositoryDatabaseID, err := metadataRepositoryDatabaseIDForAuthority(ctx, tx, authority)
	if err != nil {
		return nil, err
	}
	query, args := metadataSnapshotFileRootsSQL, []any{repositoryDatabaseID}
	if snapshotIDs != nil {
		query += ` AND s.snapshot_id IN (` + makePlaceholders(len(snapshotIDs)) + `)`
		for _, snapshotID := range snapshotIDs {
			args = append(args, snapshotID)
		}
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	result := map[string]map[string]models.SnapshotSourceRoot{}
	for rows.Next() {
		var snapshotID string
		var root models.SnapshotSourceRoot
		if err := rows.Scan(&snapshotID, &root.Path, &root.User, &root.Host); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if result[snapshotID] == nil {
			result[snapshotID] = map[string]models.SnapshotSourceRoot{}
		}
		root = models.WithSnapshotNativeRootIdentity(root)
		result[snapshotID][root.Identity] = root
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	return result, tx.Commit()
}
