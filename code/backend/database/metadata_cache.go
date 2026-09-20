package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/models"
)

// The metadata cache deliberately resolves the public repository UUID once at
// each database boundary. The compact integer never becomes repository,
// native-engine, lock, recovery, or API identity.
type metadataRepositoryResolver interface {
	QueryRow(query string, args ...any) *sql.Row
}

func metadataRepositoryDatabaseID(q metadataRepositoryResolver, repositoryID string) (int64, error) {
	var id int64
	err := q.QueryRow(`SELECT database_id FROM repositories WHERE id=?`, repositoryID).Scan(&id)
	return id, err
}

type MetadataIndexState struct {
	ReadySnapshots     int    `json:"readySnapshots"`
	PendingSnapshots   int    `json:"pendingSnapshots"`
	FailedSnapshots    int    `json:"failedSnapshots"`
	KnownSnapshots     int    `json:"knownSnapshots"`
	RepositoryFailed   bool   `json:"repositoryFailed"`
	RetryAfter         string `json:"retryAfter"`
	HeaderRetryAfter   string `json:"headerRetryAfter"`
	EntryRetryAfter    string `json:"entryRetryAfter"`
	HeaderValid        bool   `json:"headerValid"`
	HeaderStale        bool   `json:"headerStale"`
	LastReconciled     string `json:"lastReconciled"`
	RequiredGeneration int64  `json:"requiredGeneration"`
	AppliedGeneration  int64  `json:"appliedGeneration"`
	Complete           bool   `json:"complete"`
}

const MetadataFailureRetryCooldown = 15 * time.Minute

var (
	ErrMetadataRevisionChanged     = errors.New("metadata revision changed")
	ErrMetadataIndexingUnavailable = errors.New("file history is unavailable while indexing")
)

const metadataTimestampLayout = "2006-01-02T15:04:05.000000000Z"

type MetadataEntrySetIdentity struct {
	Engine         string
	NativeIdentity string
}

func validateMetadataEntrySetIdentity(identity MetadataEntrySetIdentity) error {
	if strings.TrimSpace(identity.Engine) == "" || strings.TrimSpace(identity.NativeIdentity) == "" {
		return fmt.Errorf("metadata entry-set identity is incomplete")
	}
	if strings.ContainsAny(identity.Engine, "\x00\r\n") || strings.ContainsAny(identity.NativeIdentity, "\x00\r\n") {
		return fmt.Errorf("metadata entry-set identity contains invalid control characters")
	}
	return nil
}

func canonicalMetadataTimestamp(value string) string {
	parsed, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(value))
	if err != nil {
		return ""
	}
	return parsed.UTC().Format(metadataTimestampLayout)
}

// Fresh cache schema 2 stores canonical timestamps at insertion. This hook remains
// in startup initialization so schema creation has one stable call site.
func metadataRevisionTx(tx *sql.Tx, repositoryID string) (int64, error) {
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return 0, err
	}
	var revision int64
	err = tx.QueryRow(`SELECT revision FROM metadata_cache_revisions WHERE repository_database_id=?`, repositoryDatabaseID).Scan(&revision)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return revision, err
}

func bumpMetadataRevisionTx(tx *sql.Tx, repositoryID string) error {
	return bumpMetadataRevisionTxContext(context.Background(), tx, repositoryID)
}

func bumpMetadataRevisionTxContext(ctx context.Context, tx *sql.Tx, repositoryID string) error {
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO metadata_cache_revisions(repository_database_id,revision) VALUES (?,1)
		ON CONFLICT(repository_database_id) DO UPDATE SET revision=revision+1`, repositoryDatabaseID)
	return err
}

func bumpMetadataRevisionIfChanged(tx *sql.Tx, repositoryID string, result sql.Result) error {
	return bumpMetadataRevisionIfChangedContext(context.Background(), tx, repositoryID, result)
}

func bumpMetadataRevisionIfChangedContext(ctx context.Context, tx *sql.Tx, repositoryID string, result sql.Result) error {
	changed, err := result.RowsAffected()
	if err != nil || changed == 0 {
		return err
	}
	return bumpMetadataRevisionTxContext(ctx, tx, repositoryID)
}

type metadataIndexStateReader interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func metadataRepositoryDatabaseIDForAuthority(ctx context.Context, reader metadataIndexStateReader, authority MetadataReadAuthority) (int64, error) {
	if err := validateMetadataAuthority(authority); err != nil {
		return 0, err
	}
	var repositoryDatabaseID int64
	err := reader.QueryRowContext(ctx, `SELECT database_id FROM repositories
		WHERE id=? AND metadata_cache_binding=?`, authority.RepositoryID, authority.Binding).Scan(&repositoryDatabaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrMetadataCacheBinding
	}
	return repositoryDatabaseID, err
}

func repositoryMetadataIndexState(ctx context.Context, reader metadataIndexStateReader, authority MetadataReadAuthority) (MetadataIndexState, error) {
	var state MetadataIndexState
	repositoryDatabaseID, err := metadataRepositoryDatabaseIDForAuthority(ctx, reader, authority)
	if err != nil {
		return state, err
	}
	err = reader.QueryRowContext(ctx, `SELECT
		COALESCE(SUM(CASE WHEN s.status='ready' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN s.status='pending' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN s.status='failed' THEN 1 ELSE 0 END),0),
		COUNT(s.snapshot_id),COALESCE(MAX(CASE WHEN s.status='failed' THEN s.indexed_at ELSE '' END),''),
		COALESCE(st.last_reconciled,''),COALESCE(st.applied_generation,0)
	FROM repositories r
	LEFT JOIN metadata_cache_snapshots s ON s.repository_database_id=r.database_id
	LEFT JOIN metadata_cache_state st ON st.repository_database_id=r.database_id
	WHERE r.database_id=? GROUP BY r.database_id`, repositoryDatabaseID).Scan(
		&state.ReadySnapshots, &state.PendingSnapshots, &state.FailedSnapshots, &state.KnownSnapshots, &state.EntryRetryAfter,
		&state.LastReconciled, &state.AppliedGeneration)
	if err != nil {
		return state, err
	}
	state.RequiredGeneration = authority.RequiredGeneration
	var attemptedAt, failureText string
	err = reader.QueryRowContext(ctx, `SELECT COALESCE(f.attempted_at,''),COALESCE(f.error,'')
		FROM repositories r LEFT JOIN metadata_cache_failures f ON f.repository_database_id=r.database_id WHERE r.database_id=?`, repositoryDatabaseID).Scan(&attemptedAt, &failureText)
	if err != nil {
		return state, err
	}
	state.RepositoryFailed = failureText != ""
	state.HeaderValid = state.LastReconciled != ""
	if reconciledAt, parseErr := time.Parse(time.RFC3339, state.LastReconciled); parseErr == nil {
		state.HeaderStale = time.Since(reconciledAt) >= 10*24*time.Hour
	} else if state.LastReconciled != "" {
		state.HeaderStale = true
	}
	if failedAt, parseErr := time.Parse(time.RFC3339, state.EntryRetryAfter); parseErr == nil {
		state.EntryRetryAfter = failedAt.Add(MetadataFailureRetryCooldown).UTC().Format(time.RFC3339)
	} else {
		state.EntryRetryAfter = ""
	}
	if state.RepositoryFailed {
		if parsed, parseErr := time.Parse(time.RFC3339, attemptedAt); parseErr == nil {
			state.HeaderRetryAfter = parsed.Add(MetadataFailureRetryCooldown).UTC().Format(time.RFC3339)
		}
	}
	state.RetryAfter = state.EntryRetryAfter
	if state.HeaderRetryAfter > state.RetryAfter {
		state.RetryAfter = state.HeaderRetryAfter
	}
	state.Complete = state.HeaderValid && !state.HeaderStale && state.RequiredGeneration == state.AppliedGeneration &&
		!state.RepositoryFailed && state.PendingSnapshots == 0 && state.FailedSnapshots == 0
	return state, nil
}

func RepositoryMetadataIndexState(ctx context.Context, db *sql.DB, repositoryID string) (MetadataIndexState, error) {
	handle, authority, release, err := metadataCacheReader(ctx, db, repositoryID)
	if err != nil {
		return MetadataIndexState{}, err
	}
	defer release()
	tx, err := handle.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MetadataIndexState{}, err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := repositoryMetadataIndexState(ctx, tx, authority)
	if err != nil {
		return state, err
	}
	return state, tx.Commit()
}

func RepositoryMetadataStatusSnapshot(ctx context.Context, db *sql.DB, repositoryID string) (MetadataIndexState, []string, error) {
	handle, authority, release, err := metadataCacheReader(ctx, db, repositoryID)
	if err != nil {
		return MetadataIndexState{}, nil, err
	}
	defer release()
	tx, err := handle.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return MetadataIndexState{}, nil, err
	}
	defer func() { _ = tx.Rollback() }()
	state, err := repositoryMetadataIndexState(ctx, tx, authority)
	if err != nil {
		return state, nil, err
	}
	if repositoryMetadataStatusSnapshotReadBarrier != nil {
		repositoryMetadataStatusSnapshotReadBarrier()
	}
	rows, err := tx.QueryContext(ctx, `SELECT s.snapshot_id FROM metadata_cache_snapshots s
		JOIN repositories r ON r.database_id=s.repository_database_id
		WHERE r.id=? AND s.status='ready' ORDER BY s.snapshot_id`, repositoryID)
	if err != nil {
		return state, nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return state, nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return state, nil, err
	}
	if err := rows.Close(); err != nil {
		return state, nil, err
	}
	return state, ids, tx.Commit()
}

var repositoryMetadataStatusSnapshotReadBarrier func()

func MarkMetadataRepositoryFailed(db *sql.DB, repositoryID, safeError string, attemptedAt time.Time) error {
	handle, _, err := metadataCacheWriter(context.Background(), db, repositoryID)
	if err != nil {
		return err
	}
	_, err = handle.writer.Exec(`INSERT INTO metadata_cache_failures(repository_database_id,attempted_at,error)
		SELECT database_id,?,? FROM repositories WHERE id=?
		ON CONFLICT(repository_database_id) DO UPDATE SET attempted_at=excluded.attempted_at,error=excluded.error`,
		attemptedAt.UTC().Format(time.RFC3339), safeError, repositoryID)
	return err
}

func ClearMetadataRepositoryFailure(db *sql.DB, repositoryID string) error {
	handle, _, err := metadataCacheWriter(context.Background(), db, repositoryID)
	if err != nil {
		return err
	}
	_, err = handle.writer.Exec(`DELETE FROM metadata_cache_failures WHERE repository_database_id=(SELECT database_id FROM repositories WHERE id=?)`, repositoryID)
	return err
}

func metadataReadRevisionAndGate(ctx context.Context, tx *sql.Tx, authority MetadataReadAuthority, expectedRevision *int64) (int64, int64, error) {
	repositoryDatabaseID, err := metadataRepositoryDatabaseIDForAuthority(ctx, tx, authority)
	if err != nil {
		return 0, 0, err
	}
	revision, err := metadataRevisionTx(tx, authority.RepositoryID)
	if err != nil {
		return 0, 0, err
	}
	var applied int64
	err = tx.QueryRowContext(ctx, `SELECT COALESCE(applied_generation,0)
		FROM metadata_cache_state WHERE repository_database_id=?`, repositoryDatabaseID).Scan(&applied)
	if err != nil {
		return revision, repositoryDatabaseID, err
	}
	// _files is the union catalog itself. Gating every request here prevents a
	// partially rebuilt union from leaking, including through an old UI session.
	if authority.RequiredGeneration != applied {
		return revision, repositoryDatabaseID, ErrMetadataIndexingUnavailable
	}
	var unavailable int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata_cache_snapshots
		WHERE repository_database_id=? AND status<>'ready') OR
		EXISTS(SELECT 1 FROM metadata_cache_failures WHERE repository_database_id=? AND error<>'')`,
		repositoryDatabaseID, repositoryDatabaseID).Scan(&unavailable); err != nil {
		return revision, repositoryDatabaseID, err
	}
	if unavailable != 0 {
		return revision, repositoryDatabaseID, ErrMetadataIndexingUnavailable
	}
	if expectedRevision != nil && *expectedRevision != revision {
		return revision, repositoryDatabaseID, ErrMetadataRevisionChanged
	}
	return revision, repositoryDatabaseID, nil
}

func SearchReadyMetadataFiles(ctx context.Context, db *sql.DB, repositoryID, query string, limit, offset int) ([]models.FileSearchResult, error) {
	result, _, err := SearchReadyMetadataFilesPage(ctx, db, repositoryID, query, limit, offset, nil)
	return result, err
}

const metadataSearchCatalogSQL = `SELECT f.root,f.parent_path,f.name,
	EXISTS(SELECT 1 FROM metadata_cache_files child WHERE child.repository_database_id=f.repository_database_id
		AND child.root=f.root AND child.parent_path=CASE WHEN f.parent_path='' THEN f.name ELSE f.parent_path||'/'||f.name END AND child.name<>''),
	EXISTS(SELECT 1 FROM metadata_cache_file_versions v WHERE v.file_id=f.file_id AND v.is_dir=1)
FROM metadata_cache_files f WHERE f.repository_database_id=? AND f.name<>''
	AND (f.name LIKE ? ESCAPE char(92) OR (CASE WHEN f.parent_path='' THEN f.name ELSE f.parent_path||'/'||f.name END) LIKE ? ESCAPE char(92))
ORDER BY f.root,f.parent_path,f.name LIMIT ? OFFSET ?`

const metadataBrowseChildrenSQL = `SELECT parent_path,name,has_child,has_directory_version FROM (
	SELECT f.parent_path,f.name,
		EXISTS(SELECT 1 FROM metadata_cache_files child WHERE child.repository_database_id=f.repository_database_id
			AND child.root=f.root AND child.parent_path=CASE WHEN f.parent_path='' THEN f.name ELSE f.parent_path||'/'||f.name END AND child.name<>'') AS has_child,
		EXISTS(SELECT 1 FROM metadata_cache_file_versions v WHERE v.file_id=f.file_id AND v.is_dir=1) AS has_directory_version
	FROM metadata_cache_files f WHERE f.repository_database_id=? AND f.root=? AND f.parent_path=? AND f.name<>''
) ORDER BY (has_child OR has_directory_version) DESC,name COLLATE BINARY LIMIT ? OFFSET ?`

func SearchReadyMetadataFilesPage(ctx context.Context, db *sql.DB, repositoryID, query string, limit, offset int, expectedRevision *int64) ([]models.FileSearchResult, int64, error) {
	authority, err := LoadMetadataReadAuthority(ctx, db, repositoryID)
	if err != nil {
		return nil, 0, err
	}
	return searchReadyMetadataFilesPageForAuthority(ctx, db, authority, query, limit, offset, expectedRevision, nil)
}

func SearchReadyMetadataFilesPageForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority, query string, limit, offset int, expectedRevision *int64) ([]models.FileSearchResult, int64, MetadataIndexState, error) {
	var state MetadataIndexState
	items, revision, err := searchReadyMetadataFilesPageForAuthority(ctx, db, authority, query, limit, offset, expectedRevision, &state)
	return items, revision, state, err
}

func searchReadyMetadataFilesPageForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority, query string, limit, offset int, expectedRevision *int64, stateOut *MetadataIndexState) ([]models.FileSearchResult, int64, error) {
	if limit <= 0 || limit > 201 {
		limit = 201
	}
	if offset < 0 {
		offset = 0
	}
	handle, _, release, err := metadataCacheReaderForAuthority(ctx, db, authority)
	if err != nil {
		return nil, 0, err
	}
	defer release()
	tx, err := handle.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if stateOut != nil {
		state, stateErr := repositoryMetadataIndexState(ctx, tx, authority)
		if stateErr != nil {
			return nil, 0, stateErr
		}
		*stateOut = state
	}
	revision, repositoryDatabaseID, err := metadataReadRevisionAndGate(ctx, tx, authority, expectedRevision)
	if err != nil {
		return nil, revision, err
	}
	// Search and browse intentionally touch only _files/_file_versions and child
	// existence. The millions-row membership table is lazy until path selection.
	rows, err := tx.QueryContext(ctx, metadataSearchCatalogSQL, repositoryDatabaseID, searchLikePattern(query), searchLikePattern(query), limit, offset)
	if err != nil {
		return nil, revision, err
	}
	defer rows.Close()
	result := []models.FileSearchResult{}
	for rows.Next() {
		var item models.FileSearchResult
		var parent string
		var child, directory int
		if err := rows.Scan(&item.Source, &parent, &item.Name, &child, &directory); err != nil {
			return nil, revision, err
		}
		item.Path = joinMetadataRelative(parent, item.Name)
		item.IsDir = child != 0 || directory != 0
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, revision, err
	}
	if err := tx.Commit(); err != nil {
		return nil, revision, err
	}
	return result, revision, nil
}

func BrowseReadyMetadataEntriesPage(ctx context.Context, db *sql.DB, repositoryID, source, parent string, limit, offset int, expectedRevision *int64) ([]models.FileBrowseEntry, int64, error) {
	authority, err := LoadMetadataReadAuthority(ctx, db, repositoryID)
	if err != nil {
		return nil, 0, err
	}
	return browseReadyMetadataEntriesPageForAuthority(ctx, db, authority, source, parent, limit, offset, expectedRevision, nil)
}

func BrowseReadyMetadataEntriesPageForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority, source, parent string, limit, offset int, expectedRevision *int64) ([]models.FileBrowseEntry, int64, MetadataIndexState, error) {
	var state MetadataIndexState
	items, revision, err := browseReadyMetadataEntriesPageForAuthority(ctx, db, authority, source, parent, limit, offset, expectedRevision, &state)
	return items, revision, state, err
}

func browseReadyMetadataEntriesPageForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority, source, parent string, limit, offset int, expectedRevision *int64, stateOut *MetadataIndexState) ([]models.FileBrowseEntry, int64, error) {
	if limit <= 0 || limit > 201 {
		limit = 201
	}
	if offset < 0 {
		offset = 0
	}
	handle, _, release, err := metadataCacheReaderForAuthority(ctx, db, authority)
	if err != nil {
		return nil, 0, err
	}
	defer release()
	tx, err := handle.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if stateOut != nil {
		state, stateErr := repositoryMetadataIndexState(ctx, tx, authority)
		if stateErr != nil {
			return nil, 0, stateErr
		}
		*stateOut = state
	}
	revision, repositoryDatabaseID, err := metadataReadRevisionAndGate(ctx, tx, authority, expectedRevision)
	if err != nil {
		return nil, revision, err
	}
	result := []models.FileBrowseEntry{}
	if source == "" {
		rows, queryErr := tx.QueryContext(ctx, `SELECT f.root,
			EXISTS(SELECT 1 FROM metadata_cache_files child WHERE child.repository_database_id=f.repository_database_id
				AND child.root=f.root AND child.parent_path='' AND child.name<>'')
			FROM metadata_cache_files f WHERE f.repository_database_id=? AND f.parent_path='' AND f.name=''
			ORDER BY f.root COLLATE BINARY LIMIT ? OFFSET ?`, repositoryDatabaseID, limit, offset)
		if queryErr != nil {
			return nil, revision, queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var item models.FileBrowseEntry
			var children int
			if err := rows.Scan(&item.Source, &children); err != nil {
				return nil, revision, err
			}
			item.Name, item.IsDir, item.IsSource = item.Source, true, true
			item.CanExpand = children != 0
			result = append(result, item)
		}
		if err := rows.Err(); err != nil {
			return nil, revision, err
		}
	} else {
		source = normalizeMetadataRoot(source)
		parent = normalizeMetadataPath(parent)
		rows, queryErr := tx.QueryContext(ctx, metadataBrowseChildrenSQL, repositoryDatabaseID, source, parent, limit, offset)
		if queryErr != nil {
			return nil, revision, queryErr
		}
		defer rows.Close()
		for rows.Next() {
			var item models.FileBrowseEntry
			var itemParent string
			var child, directory int
			if err := rows.Scan(&itemParent, &item.Name, &child, &directory); err != nil {
				return nil, revision, err
			}
			item.Source, item.Path = source, joinMetadataRelative(itemParent, item.Name)
			item.CanExpand, item.IsDir = child != 0, child != 0 || directory != 0
			result = append(result, item)
		}
		if err := rows.Err(); err != nil {
			return nil, revision, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, revision, err
	}
	return result, revision, nil
}

func ReadyMetadataFileHistory(ctx context.Context, db *sql.DB, repositoryID, path, source string, limit, offset int) ([]models.FileVersion, error) {
	result, _, err := ReadyMetadataFileHistoryPage(ctx, db, repositoryID, path, source, limit, offset, nil)
	return result, err
}

type metadataVersionRow struct {
	version   models.FileVersion
	engine    string
	client    string
	machine   string
	rootUser  string
	rootHost  string
	groupKey  string
	baseLabel string
}

const metadataHistoryFileLookupSQL = `SELECT f.file_id,
	EXISTS(SELECT 1 FROM metadata_cache_files child
		WHERE child.repository_database_id=f.repository_database_id AND child.root=f.root
		AND child.parent_path=CASE WHEN f.parent_path='' THEN f.name ELSE f.parent_path||'/'||f.name END
		AND child.name<>'')
	FROM metadata_cache_files f
	WHERE f.repository_database_id=? AND f.root=? AND f.parent_path=? AND f.name=?`

// Start from the selected file's blocks before expanding lists and snapshots.
// Starting from all ready snapshots can multiply a frequently changed file's
// blocks by its snapshot count rather than following each actual reference.
const metadataDirectHistorySQL = `SELECT s.snapshot_id,s.timestamp,v.size,v.is_dir,r.path,r.native_user,r.native_host,
		setrow.engine,s.presentation_client_uuid,s.presentation_machine_label
	FROM metadata_cache_block_entries e INDEXED BY metadata_cache_block_entries_file_version
 CROSS JOIN metadata_cache_block_list_items l ON l.block_id=e.block_id
 CROSS JOIN metadata_cache_entry_sets setrow ON setrow.block_list_id=l.block_list_id
	CROSS JOIN metadata_cache_snapshots s ON s.entry_set_id=setrow.entry_set_id AND s.repository_database_id=? AND s.status='ready'
	JOIN metadata_cache_files f ON f.file_id=e.file_id
	JOIN metadata_cache_file_versions v ON v.file_version_id=e.file_version_id AND v.file_id=e.file_id
	JOIN metadata_cache_snapshot_roots r ON r.repository_database_id=s.repository_database_id AND r.snapshot_id=s.snapshot_id AND r.normalized_root=f.root
	WHERE e.file_id=?
	ORDER BY s.timestamp DESC,s.snapshot_id DESC,r.path,r.native_user,r.native_host`

// Reduce descendant matches to shared blocks before expanding their lists and
// entry sets. Otherwise each file repeats every retained snapshot-list join.
const metadataDescendantHistorySQL = `WITH candidate_children(file_id,root) AS (
	SELECT child.file_id,child.root FROM metadata_cache_files child
	WHERE child.repository_database_id=? AND child.root=? AND child.parent_path=? COLLATE BINARY AND child.name<>''
	UNION ALL
	SELECT child.file_id,child.root FROM metadata_cache_files child
	WHERE child.repository_database_id=? AND child.root=?
		AND child.parent_path>=? COLLATE BINARY AND child.parent_path<? COLLATE BINARY AND child.name<>''
), candidate_blocks(block_id,root) AS MATERIALIZED (
 SELECT DISTINCT e.block_id,child.root FROM candidate_children child
 CROSS JOIN metadata_cache_block_entries e INDEXED BY metadata_cache_block_entries_file_version ON e.file_id=child.file_id
), candidate_sets(entry_set_id,root) AS MATERIALIZED (
 SELECT DISTINCT setrow.entry_set_id,candidate.root FROM candidate_blocks candidate
 JOIN metadata_cache_block_list_items l ON l.block_id=candidate.block_id
 JOIN metadata_cache_entry_sets setrow ON setrow.block_list_id=l.block_list_id
)
SELECT s.snapshot_id,s.timestamp,r.path,r.native_user,r.native_host,
		setrow.engine,s.presentation_client_uuid,s.presentation_machine_label
	FROM candidate_sets candidate
	JOIN metadata_cache_entry_sets setrow ON setrow.entry_set_id=candidate.entry_set_id
	JOIN metadata_cache_snapshots s ON s.entry_set_id=candidate.entry_set_id AND s.repository_database_id=? AND s.status='ready'
	JOIN metadata_cache_snapshot_roots r ON r.repository_database_id=s.repository_database_id AND r.snapshot_id=s.snapshot_id AND r.normalized_root=candidate.root
	ORDER BY s.timestamp DESC,s.snapshot_id DESC,r.path,r.native_user,r.native_host`

func ReadyMetadataFileHistoryPage(ctx context.Context, db *sql.DB, repositoryID, path, source string, limit, offset int, expectedRevision *int64) ([]models.FileVersion, int64, error) {
	authority, err := LoadMetadataReadAuthority(ctx, db, repositoryID)
	if err != nil {
		return nil, 0, err
	}
	return readyMetadataFileHistoryPageForAuthority(ctx, db, authority, path, source, limit, offset, expectedRevision, nil)
}

func ReadyMetadataFileHistoryPageForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority, path, source string, limit, offset int, expectedRevision *int64) ([]models.FileVersion, int64, MetadataIndexState, error) {
	var state MetadataIndexState
	items, revision, err := readyMetadataFileHistoryPageForAuthority(ctx, db, authority, path, source, limit, offset, expectedRevision, &state)
	return items, revision, state, err
}

func readyMetadataFileHistoryPageForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority, path, source string, limit, offset int, expectedRevision *int64, stateOut *MetadataIndexState) ([]models.FileVersion, int64, error) {
	if limit <= 0 || limit > 201 {
		limit = 201
	}
	if offset < 0 {
		offset = 0
	}
	path = normalizeMetadataPath(path)
	source = normalizeMetadataRoot(source)
	parent, name := splitMetadataRelative(path)
	handle, _, release, err := metadataCacheReaderForAuthority(ctx, db, authority)
	if err != nil {
		return nil, 0, err
	}
	defer release()
	tx, err := handle.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = tx.Rollback() }()
	if stateOut != nil {
		state, stateErr := repositoryMetadataIndexState(ctx, tx, authority)
		if stateErr != nil {
			return nil, 0, stateErr
		}
		*stateOut = state
	}
	revision, repositoryDatabaseID, err := metadataReadRevisionAndGate(ctx, tx, authority, expectedRevision)
	if err != nil {
		return nil, revision, err
	}
	var fileID int64
	var hasChild int
	if err := tx.QueryRowContext(ctx, metadataHistoryFileLookupSQL,
		repositoryDatabaseID, source, parent, name).Scan(&fileID, &hasChild); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return []models.FileVersion{}, revision, tx.Commit()
		}
		return nil, revision, err
	}
	rows, err := tx.QueryContext(ctx, metadataDirectHistorySQL, repositoryDatabaseID, fileID)
	if err != nil {
		return nil, revision, err
	}
	all := []metadataVersionRow{}
	for rows.Next() {
		var row metadataVersionRow
		var isDir int
		if err := rows.Scan(&row.version.SnapshotID, &row.version.Timestamp, &row.version.Size, &isDir,
			&row.version.Source, &row.rootUser, &row.rootHost, &row.engine, &row.client, &row.machine); err != nil {
			_ = rows.Close()
			return nil, revision, err
		}
		row.version.IsDir, row.version.Present = isDir != 0, true
		row.version.NativeRootID = models.SnapshotNativeRootIdentity(models.SnapshotSourceRoot{Path: row.version.Source, User: row.rootUser, Host: row.rootHost})
		all = append(all, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, revision, err
	}
	if err := rows.Close(); err != nil {
		return nil, revision, err
	}
	// Implied ancestors have no synthetic membership/version. Derive one
	// truthful directory occurrence per snapshot from surviving descendants,
	// including snapshots where another version stored an explicit directory.
	// Collapse the potentially large descendant membership join to exact
	// entry-set/root pairs before expanding shared sets to snapshot headers.
	occurrences := make(map[string]int, len(all))
	for index := range all {
		occurrences[all[index].version.SnapshotID+"\x00"+all[index].version.NativeRootID] = index
	}
	// Ingestion materializes every ancestor, so direct-child existence is both
	// necessary and sufficient for any descendant occurrence. Leaf files and
	// explicit empty directories skip the potentially large membership join.
	if hasChild == 0 {
		applyMetadataMachineLabels(all)
		if offset > len(all) {
			offset = len(all)
		}
		end := min(offset+limit, len(all))
		result := make([]models.FileVersion, 0, end-offset)
		for _, row := range all[offset:end] {
			result = append(result, row.version)
		}
		if err := tx.Commit(); err != nil {
			return nil, revision, err
		}
		return result, revision, nil
	}
	// The prefix range is exact BINARY identity. LIKE is ASCII-case-insensitive
	// in SQLite and would make Folder history absorb folder descendants. The
	// upper bound is safe because the prefix always ends in '/' and '0' is its
	// immediate ASCII successor.
	descendantPrefix := path + "/"
	descendantUpperBound := path + "0"
	rows, err = tx.QueryContext(ctx, metadataDescendantHistorySQL,
		repositoryDatabaseID, source, path,
		repositoryDatabaseID, source, descendantPrefix, descendantUpperBound,
		repositoryDatabaseID)
	if err != nil {
		return nil, revision, err
	}
	for rows.Next() {
		var row metadataVersionRow
		if err := rows.Scan(&row.version.SnapshotID, &row.version.Timestamp, &row.version.Source,
			&row.rootUser, &row.rootHost, &row.engine, &row.client, &row.machine); err != nil {
			_ = rows.Close()
			return nil, revision, err
		}
		row.version.IsDir, row.version.Present = true, true
		row.version.NativeRootID = models.SnapshotNativeRootIdentity(models.SnapshotSourceRoot{Path: row.version.Source, User: row.rootUser, Host: row.rootHost})
		key := row.version.SnapshotID + "\x00" + row.version.NativeRootID
		if _, exists := occurrences[key]; exists {
			// Exact native occurrences outrank the implied-ancestor view. A child
			// proves navigation but must not rewrite that occurrence's cached type
			// or size; only descendant-only snapshots receive a synthetic directory.
			continue
		}
		occurrences[key] = len(all)
		all = append(all, row)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, revision, err
	}
	if err := rows.Close(); err != nil {
		return nil, revision, err
	}
	applyMetadataMachineLabels(all)
	if offset > len(all) {
		offset = len(all)
	}
	end := offset + limit
	if end > len(all) {
		end = len(all)
	}
	result := make([]models.FileVersion, 0, end-offset)
	for _, row := range all[offset:end] {
		result = append(result, row.version)
	}
	if err := tx.Commit(); err != nil {
		return nil, revision, err
	}
	return result, revision, nil
}

func applyMetadataMachineLabels(rows []metadataVersionRow) {
	type group struct {
		key, label, earliest string
		latestMachineTime    string
		latestHostTime       string
	}
	groups := map[string]*group{}
	for index := range rows {
		row := &rows[index]
		switch {
		case row.client != "":
			row.groupKey = "client:" + row.client
		case row.engine == "restic" && row.rootHost != "":
			row.groupKey = "restic-host:" + row.rootHost
		case row.engine == "kopia" && validMetadataClientUUID(row.rootUser):
			row.groupKey = "kopia-user:" + row.rootUser
		default:
			row.groupKey = "previous"
		}
		g := groups[row.groupKey]
		if g == nil {
			g = &group{key: row.groupKey, earliest: row.version.Timestamp}
			groups[row.groupKey] = g
		}
		if row.version.Timestamp < g.earliest {
			g.earliest = row.version.Timestamp
		}
		if row.machine != "" && row.version.Timestamp >= g.latestMachineTime {
			g.latestMachineTime, g.label = row.version.Timestamp, row.machine
		}
		if g.latestMachineTime == "" && row.engine == "restic" && row.rootHost != "" && row.version.Timestamp >= g.latestHostTime {
			g.latestHostTime, g.label = row.version.Timestamp, row.rootHost
		}
	}
	ordered := make([]*group, 0, len(groups))
	for _, g := range groups {
		if g.label == "" {
			g.label = "Previous computer"
		}
		ordered = append(ordered, g)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].earliest != ordered[j].earliest {
			return ordered[i].earliest < ordered[j].earliest
		}
		return ordered[i].key < ordered[j].key
	})
	byLabel := map[string][]*group{}
	groupOrder := map[string]int{}
	for index, g := range ordered {
		groupOrder[g.key] = index
		byLabel[g.label] = append(byLabel[g.label], g)
	}
	final := map[string]string{}
	for label, labelGroups := range byLabel {
		for index, g := range labelGroups {
			final[g.key] = label
			if len(labelGroups) > 1 {
				final[g.key] = fmt.Sprintf("%s #%d", label, index+1)
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if groupOrder[rows[i].groupKey] != groupOrder[rows[j].groupKey] {
			return groupOrder[rows[i].groupKey] < groupOrder[rows[j].groupKey]
		}
		if rows[i].version.Timestamp != rows[j].version.Timestamp {
			return rows[i].version.Timestamp > rows[j].version.Timestamp
		}
		if rows[i].version.SnapshotID != rows[j].version.SnapshotID {
			return rows[i].version.SnapshotID > rows[j].version.SnapshotID
		}
		return rows[i].version.NativeRootID < rows[j].version.NativeRootID
	})
	for index := range rows {
		rows[index].version.MachineLabel = final[rows[index].groupKey]
	}
}

func validMetadataClientUUID(value string) bool {
	parsed, err := uuid.Parse(value)
	return err == nil && parsed != uuid.Nil && parsed.String() == value
}

func MetadataGenerations(db *sql.DB, repositoryID string) (required, applied int64, err error) {
	handle, authority, release, err := metadataCacheReader(context.Background(), db, repositoryID)
	if err != nil {
		return 0, 0, err
	}
	defer release()
	err = handle.readers.QueryRow(`SELECT applied_generation FROM metadata_cache_state WHERE repository_database_id=1`).Scan(&applied)
	return authority.RequiredGeneration, applied, err
}

func advanceAppliedGenerationTx(tx *sql.Tx, repositoryID string, generation int64) (bool, error) {
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return false, err
	}
	result, err := tx.Exec(`UPDATE metadata_cache_state SET applied_generation=?
		WHERE repository_database_id=? AND applied_generation=?
		AND NOT EXISTS(SELECT 1 FROM metadata_cache_snapshots
			WHERE repository_database_id=? AND status<>'ready')`, generation, repositoryDatabaseID, generation-1, repositoryDatabaseID)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	return count == 1, err
}

func AcknowledgeMetadataGeneration(db *sql.DB, repositoryID string, generation int64) (bool, error) {
	handle, authority, err := metadataCacheWriter(context.Background(), db, repositoryID)
	if err != nil {
		return false, err
	}
	if authority.RequiredGeneration < generation {
		return false, fmt.Errorf("metadata generation was not reserved")
	}
	tx, err := handle.writer.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	advanced, err := advanceAppliedGenerationTx(tx, repositoryID, generation)
	if err != nil {
		return false, err
	}
	if !advanced {
		var applied int64
		repositoryDatabaseID, resolveErr := metadataRepositoryDatabaseID(tx, repositoryID)
		if resolveErr != nil {
			return false, resolveErr
		}
		if err := tx.QueryRow(`SELECT applied_generation FROM metadata_cache_state WHERE repository_database_id=?`, repositoryDatabaseID).Scan(&applied); err != nil {
			return false, err
		}
		advanced = applied >= generation
	}
	return advanced, tx.Commit()
}

func ApplyMetadataBackupSnapshot(db *sql.DB, repositoryID string, generation int64, snapshot models.Snapshot) error {
	handle, authority, err := metadataCacheWriter(context.Background(), db, repositoryID)
	if err != nil {
		return err
	}
	if authority.RequiredGeneration < generation {
		return fmt.Errorf("metadata generation was not reserved")
	}
	tx, err := handle.writer.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := upsertMetadataSnapshotHeaderTx(tx, repositoryID, snapshot, "pending", "", ""); err != nil {
		return err
	}
	// The automatic workflow keeps its reserved generation dirty until the
	// complete inventory and every included entry set are ready.
	_ = generation
	return tx.Commit()
}

func ApplyMetadataDeletedSnapshots(db *sql.DB, repositoryID string, generation int64, snapshotIDs []string, acknowledge bool) error {
	handle, authority, err := metadataCacheWriter(context.Background(), db, repositoryID)
	if err != nil {
		return err
	}
	if authority.RequiredGeneration < generation {
		return fmt.Errorf("metadata generation was not reserved")
	}
	tx, err := handle.writer.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := detachMetadataSnapshotsTx(context.Background(), tx, repositoryID, snapshotIDs); err != nil {
		return err
	}
	if acknowledge {
		if _, err := advanceAppliedGenerationTx(tx, repositoryID, generation); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

func metadataSnapshotMarker(snapshot models.Snapshot) (string, string, string) {
	marker := snapshot.OwnershipMarker
	status := string(marker.Status)
	if status == "" {
		status = string(models.SnapshotOwnershipMissing)
	}
	if marker.Status != models.SnapshotOwnershipValid {
		return status, "", ""
	}
	return status, marker.ProfileID, marker.JobID
}

func metadataSnapshotRoots(snapshot models.Snapshot) []models.SnapshotSourceRoot {
	if len(snapshot.SourceRoots) != 0 {
		return snapshot.SourceRoots
	}
	if snapshot.Source == "" {
		return nil
	}
	return []models.SnapshotSourceRoot{{Path: snapshot.Source, User: snapshot.NativeSourceUser, Host: snapshot.NativeSourceHost}}
}

func normalizedMetadataSnapshotRoots(snapshot models.Snapshot) ([]models.SnapshotSourceRoot, []string, error) {
	roots := metadataSnapshotRoots(snapshot)
	normalized := make([]string, len(roots))
	seen := make(map[string]struct{}, len(roots))
	for index, root := range roots {
		normalized[index] = normalizeMetadataRoot(root.Path)
		if normalized[index] == "" {
			return nil, nil, fmt.Errorf("snapshot %q has an empty native source root", snapshot.ID)
		}
		if _, duplicate := seen[normalized[index]]; duplicate {
			// Membership stores the normalized root through file_id instead of
			// repeating root_ordinal. Two roots with the same normalized spelling
			// would therefore make exact native-root presentation ambiguous.
			return nil, nil, fmt.Errorf("snapshot %q has duplicate normalized native source root %q", snapshot.ID, normalized[index])
		}
		seen[normalized[index]] = struct{}{}
	}
	return roots, normalized, nil
}

func metadataRootScope(snapshot models.Snapshot) (string, error) {
	_, normalized, err := normalizedMetadataSnapshotRoots(snapshot)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(normalized)
	return string(encoded), err
}

// MetadataEntrySetRootScope exposes the exact normalized scope used by entry-set
// lookup so coordinator batching cannot invent a second path normalization.
func MetadataEntrySetRootScope(snapshot models.Snapshot) (string, error) {
	return metadataRootScope(snapshot)
}

func replaceMetadataSnapshotRootsTx(tx *sql.Tx, repositoryID string, snapshot models.Snapshot) error {
	return replaceMetadataSnapshotRootsTxContext(context.Background(), tx, repositoryID, snapshot)
}

func replaceMetadataSnapshotRootsTxContext(ctx context.Context, tx *sql.Tx, repositoryID string, snapshot models.Snapshot) error {
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metadata_cache_snapshot_roots WHERE repository_database_id=? AND snapshot_id=?`, repositoryDatabaseID, snapshot.ID); err != nil {
		return err
	}
	roots, normalized, err := normalizedMetadataSnapshotRoots(snapshot)
	if err != nil {
		return err
	}
	for ordinal, root := range roots {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata_cache_snapshot_roots(repository_database_id,snapshot_id,root_ordinal,path,normalized_root,native_user,native_host)
			VALUES (?,?,?,?,?,?,?)`, repositoryDatabaseID, snapshot.ID, ordinal, root.Path, normalized[ordinal], root.User, root.Host); err != nil {
			return err
		}
	}
	return nil
}

func metadataSnapshotRootsEqualTx(ctx context.Context, tx *sql.Tx, repositoryDatabaseID int64, snapshot models.Snapshot) (bool, error) {
	want, normalized, err := normalizedMetadataSnapshotRoots(snapshot)
	if err != nil {
		return false, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT root_ordinal,path,normalized_root,native_user,native_host FROM metadata_cache_snapshot_roots
		WHERE repository_database_id=? AND snapshot_id=? ORDER BY root_ordinal`, repositoryDatabaseID, snapshot.ID)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	index := 0
	equal := true
	for rows.Next() {
		var ordinal int
		var path, normalizedRoot, user, host string
		if err := rows.Scan(&ordinal, &path, &normalizedRoot, &user, &host); err != nil {
			return false, err
		}
		if index >= len(want) || ordinal != index || path != want[index].Path || normalizedRoot != normalized[index] || user != want[index].User || host != want[index].Host {
			equal = false
		}
		index++
	}
	return equal && index == len(want), rows.Err()
}

func upsertMetadataSnapshotHeaderTx(tx *sql.Tx, repositoryID string, snapshot models.Snapshot, status, indexedAt, safeError string) error {
	return upsertMetadataSnapshotHeaderTxContext(context.Background(), tx, repositoryID, snapshot, status, indexedAt, safeError)
}

func upsertMetadataSnapshotHeaderTxContext(ctx context.Context, tx *sql.Tx, repositoryID string, snapshot models.Snapshot, status, indexedAt, safeError string) error {
	if strings.TrimSpace(snapshot.ID) == "" {
		return fmt.Errorf("snapshot ID is required")
	}
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return err
	}
	markerStatus, markerProfile, markerJob := metadataSnapshotMarker(snapshot)
	client, machine := models.ParseSnapshotPresentationTags(snapshot.Tags)
	// Entry-only retry snapshots are reconstructed from this cache and have no
	// raw native tag slice. Retain only their previously validated, nonserialized
	// hints. Authoritative native headers carry no such values, so absent or
	// malformed native tags still clear/fall back during normal reconciliation.
	if len(snapshot.Tags) == 0 {
		if snapshot.PresentationClientUUID != "" {
			client = snapshot.PresentationClientUUID
		}
		if snapshot.PresentationMachineLabel != "" {
			machine = snapshot.PresentationMachineLabel
		}
	}
	result, err := tx.ExecContext(ctx, `INSERT INTO metadata_cache_snapshots
		(repository_database_id,snapshot_id,timestamp,size,duration,source,marker_status,marker_profile_uuid,marker_job_uuid,
		 presentation_client_uuid,presentation_machine_label,status,indexed_at,error,total_file_count,native_root_type)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(repository_database_id,snapshot_id) DO UPDATE SET
		timestamp=excluded.timestamp,size=excluded.size,duration=excluded.duration,source=excluded.source,
		marker_status=excluded.marker_status,marker_profile_uuid=excluded.marker_profile_uuid,marker_job_uuid=excluded.marker_job_uuid,
		presentation_client_uuid=excluded.presentation_client_uuid,presentation_machine_label=excluded.presentation_machine_label,
		status=excluded.status,indexed_at=excluded.indexed_at,error=excluded.error,total_file_count=excluded.total_file_count,native_root_type=excluded.native_root_type`,
		repositoryDatabaseID, snapshot.ID, canonicalMetadataTimestamp(snapshot.Timestamp), snapshot.Size, snapshot.Duration, snapshot.Source,
		markerStatus, markerProfile, markerJob, client, machine, status, indexedAt, safeError, snapshot.TotalFileCount, snapshot.NativeRootType)
	if err != nil {
		return err
	}
	if err := replaceMetadataSnapshotRootsTxContext(ctx, tx, repositoryID, snapshot); err != nil {
		return err
	}
	return bumpMetadataRevisionIfChangedContext(ctx, tx, repositoryID, result)
}

type metadataSnapshotReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func loadMetadataSnapshotRootsContext(ctx context.Context, reader metadataSnapshotReader, repositoryDatabaseID int64, snapshots []models.Snapshot) error {
	byID := make(map[string]*models.Snapshot, len(snapshots))
	for index := range snapshots {
		byID[snapshots[index].ID] = &snapshots[index]
	}
	rows, err := reader.QueryContext(ctx, `SELECT roots.snapshot_id,roots.path,roots.native_user,roots.native_host
		FROM metadata_cache_snapshot_roots roots
		WHERE roots.repository_database_id=? ORDER BY roots.snapshot_id,roots.root_ordinal`, repositoryDatabaseID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var root models.SnapshotSourceRoot
		if err := rows.Scan(&id, &root.Path, &root.User, &root.Host); err != nil {
			return err
		}
		if snapshot := byID[id]; snapshot != nil {
			snapshot.SourceRoots = append(snapshot.SourceRoots, models.WithSnapshotNativeRootIdentity(root))
		}
	}
	return rows.Err()
}

func MetadataSnapshotReady(db *sql.DB, repositoryID, snapshotID string) (bool, error) {
	authority, err := LoadMetadataReadAuthority(context.Background(), db, repositoryID)
	if err != nil {
		return false, err
	}
	ready, err := ReadyMetadataSnapshotIDsForAuthority(context.Background(), db, authority, []string{snapshotID})
	return ready[snapshotID], err
}

func ReadyMetadataSnapshotIDsForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority, snapshotIDs []string) (map[string]bool, error) {
	ready := map[string]bool{}
	if len(snapshotIDs) == 0 {
		return ready, nil
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
	args := []any{repositoryDatabaseID}
	for _, id := range snapshotIDs {
		args = append(args, id)
	}
	rows, err := tx.QueryContext(ctx, `SELECT snapshot_id FROM metadata_cache_snapshots
		WHERE repository_database_id=? AND status='ready' AND snapshot_id IN (`+makePlaceholders(len(snapshotIDs))+`)`, args...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ready[id] = true
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return ready, tx.Commit()
}

func metadataSnapshotReadyInTx(ctx context.Context, tx *sql.Tx, repositoryDatabaseID int64, snapshotID string) (bool, error) {
	var ready int
	err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata_cache_snapshots
		WHERE repository_database_id=? AND snapshot_id=? AND status='ready')`, repositoryDatabaseID, snapshotID).Scan(&ready)
	return ready != 0, err
}

func EnsureMetadataSnapshotsKnown(db *sql.DB, repositoryID string, snapshots []models.Snapshot) error {
	handle, _, err := metadataCacheWriter(context.Background(), db, repositoryID)
	if err != nil {
		return err
	}
	tx, err := handle.writer.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, snapshot := range snapshots {
		repositoryDatabaseID, resolveErr := metadataRepositoryDatabaseID(tx, repositoryID)
		if resolveErr != nil {
			return resolveErr
		}
		var exists int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM metadata_cache_snapshots WHERE repository_database_id=? AND snapshot_id=?)`, repositoryDatabaseID, snapshot.ID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			if err := upsertMetadataSnapshotHeaderTx(tx, repositoryID, snapshot, "pending", "", ""); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func MarkMetadataSnapshotPending(db *sql.DB, repositoryID, snapshotID string) error {
	handle, _, err := metadataCacheWriter(context.Background(), db, repositoryID)
	if err != nil {
		return err
	}
	tx, err := handle.writer.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return err
	}
	var entrySetID sql.NullInt64
	if err := tx.QueryRow(`SELECT entry_set_id FROM metadata_cache_snapshots
		WHERE repository_database_id=? AND snapshot_id=?`, repositoryDatabaseID, snapshotID).Scan(&entrySetID); errors.Is(err, sql.ErrNoRows) {
		return tx.Commit()
	} else if err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE metadata_cache_snapshots SET entry_set_id=NULL,status='pending',indexed_at='',error=''
		WHERE repository_database_id=? AND snapshot_id=?`, repositoryDatabaseID, snapshotID)
	if err != nil {
		return err
	}
	if entrySetID.Valid {
		var shared int
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM metadata_cache_snapshots
			WHERE repository_database_id=? AND entry_set_id=?)`, repositoryDatabaseID, entrySetID.Int64).Scan(&shared); err != nil {
			return err
		}
		// Shared sets keep their catalog rows. A final detach uses the same
		// candidate-scoped cleanup as deletion, avoiding repository-wide scans.
		if shared == 0 {
			if err := cleanupFinalMetadataEntrySetsTx(context.Background(), tx, repositoryDatabaseID, []int64{entrySetID.Int64}); err != nil {
				return err
			}
		}
	}
	if err := bumpMetadataRevisionIfChanged(tx, repositoryID, result); err != nil {
		return err
	}
	return tx.Commit()
}

func ReadyMetadataSnapshotIDs(db *sql.DB, repositoryID string, snapshotIDs []string) (map[string]bool, error) {
	authority, err := LoadMetadataReadAuthority(context.Background(), db, repositoryID)
	if err != nil {
		return nil, err
	}
	return ReadyMetadataSnapshotIDsForAuthority(context.Background(), db, authority, snapshotIDs)
}

func ListMetadataEntries(db *sql.DB, repositoryID, snapshotID, parentPath string) ([]models.SnapshotEntry, error) {
	authority, err := LoadMetadataReadAuthority(context.Background(), db, repositoryID)
	if err != nil {
		return nil, err
	}
	_, entries, ready, err := ReadyMetadataSnapshotEntriesForAuthority(context.Background(), db, authority, snapshotID, parentPath)
	if err != nil {
		return nil, err
	}
	if !ready {
		return []models.SnapshotEntry{}, nil
	}
	return entries, nil
}

// Snapshot browsing starts with just the selected roots' immediate children,
// then computes each file's one bucket in the selected snapshot's block list.
// Expanding every block first would read an entire large snapshot to browse a
// small folder. Source-root joins preserve the exact native restore address.
const metadataSnapshotChildrenSQL = `SELECT
 CASE WHEN f.parent_path='' THEN f.name ELSE f.parent_path||'/'||f.name END AS path,
 r.path,v.mode,v.size,v.is_dir
 FROM metadata_cache_snapshots s
 JOIN metadata_cache_entry_sets setrow ON setrow.entry_set_id=s.entry_set_id
 JOIN metadata_cache_snapshot_roots r ON r.repository_database_id=s.repository_database_id AND r.snapshot_id=s.snapshot_id
 JOIN metadata_cache_state state ON state.repository_database_id=s.repository_database_id
 CROSS JOIN metadata_cache_files f ON f.repository_database_id=s.repository_database_id AND f.root=r.normalized_root AND f.parent_path=?
 CROSS JOIN metadata_cache_block_list_items l ON l.block_list_id=setrow.block_list_id AND l.bucket=f.file_id % state.bucket_count
 CROSS JOIN metadata_cache_block_entries e ON e.block_id=l.block_id AND e.file_id=f.file_id
 JOIN metadata_cache_file_versions v ON v.file_id=f.file_id AND v.file_version_id=e.file_version_id
 WHERE s.repository_database_id=? AND s.snapshot_id=? ORDER BY v.is_dir DESC,path`

func ReadyMetadataSnapshotEntriesForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority, snapshotID, parentPath string) (models.Snapshot, []models.SnapshotEntry, bool, error) {
	parentPath = normalizeMetadataPath(parentPath)
	handle, _, release, err := metadataCacheReaderForAuthority(ctx, db, authority)
	if err != nil {
		return models.Snapshot{}, nil, false, err
	}
	defer release()
	tx, err := handle.readers.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return models.Snapshot{}, nil, false, err
	}
	defer func() { _ = tx.Rollback() }()
	repositoryDatabaseID, err := metadataRepositoryDatabaseIDForAuthority(ctx, tx, authority)
	if err != nil {
		return models.Snapshot{}, nil, false, err
	}
	snapshots, err := listMetadataSnapshotsTx(ctx, tx, repositoryDatabaseID, []string{snapshotID}, false)
	if err != nil {
		return models.Snapshot{}, nil, false, err
	}
	if len(snapshots) == 0 {
		return models.Snapshot{}, []models.SnapshotEntry{}, false, tx.Commit()
	}
	ready, err := metadataSnapshotReadyInTx(ctx, tx, repositoryDatabaseID, snapshotID)
	if err != nil || !ready {
		return snapshots[0], []models.SnapshotEntry{}, ready, err
	}
	rows, err := tx.QueryContext(ctx, metadataSnapshotChildrenSQL, parentPath, repositoryDatabaseID, snapshotID)
	if err != nil {
		return models.Snapshot{}, nil, false, err
	}
	result := []models.SnapshotEntry{}
	for rows.Next() {
		var entry models.SnapshotEntry
		var isDir int
		if err := rows.Scan(&entry.Path, &entry.SourceRoot, &entry.Mode, &entry.Size, &isDir); err != nil {
			_ = rows.Close()
			return models.Snapshot{}, nil, false, err
		}
		entry.Name, entry.IsDir = metadataBase(entry.Path), isDir != 0
		result = append(result, entry)
	}
	if err := rows.Close(); err != nil {
		return models.Snapshot{}, nil, false, err
	}
	if err := rows.Err(); err != nil {
		return models.Snapshot{}, nil, false, err
	}
	return snapshots[0], result, true, tx.Commit()
}

func ReplaceMetadataSnapshot(db *sql.DB, repositoryID string, snapshot models.Snapshot, entries []models.SnapshotEntry) error {
	return ReplaceMetadataSnapshotForRefreshContext(context.Background(), db, repositoryID, snapshot, entries)
}

func ReplaceMetadataSnapshotForRefresh(db *sql.DB, repositoryID string, snapshot models.Snapshot, entries []models.SnapshotEntry) error {
	return ReplaceMetadataSnapshotForRefreshContext(context.Background(), db, repositoryID, snapshot, entries)
}

func ReplaceMetadataSnapshotForRefreshContext(ctx context.Context, db *sql.DB, repositoryID string, snapshot models.Snapshot, entries []models.SnapshotEntry) error {
	var engine string
	if err := db.QueryRowContext(ctx, `SELECT engine FROM repositories WHERE id=?`, repositoryID).Scan(&engine); err != nil {
		return err
	}
	return ReplaceMetadataSnapshotWithIdentityForRefreshContext(ctx, db, repositoryID, snapshot, entries,
		MetadataEntrySetIdentity{Engine: engine, NativeIdentity: snapshot.ID})
}

func ReplaceMetadataSnapshotWithIdentity(db *sql.DB, repositoryID string, snapshot models.Snapshot, entries []models.SnapshotEntry, identity MetadataEntrySetIdentity) error {
	return replaceMetadataSnapshotWithIdentity(context.Background(), db, repositoryID, snapshot, entries, identity)
}

func ReplaceMetadataSnapshotWithIdentityForRefresh(db *sql.DB, repositoryID string, snapshot models.Snapshot, entries []models.SnapshotEntry, identity MetadataEntrySetIdentity) error {
	return ReplaceMetadataSnapshotWithIdentityForRefreshContext(context.Background(), db, repositoryID, snapshot, entries, identity)
}

func ReplaceMetadataSnapshotWithIdentityForRefreshContext(ctx context.Context, db *sql.DB, repositoryID string, snapshot models.Snapshot, entries []models.SnapshotEntry, identity MetadataEntrySetIdentity) error {
	return replaceMetadataSnapshotWithIdentity(ctx, db, repositoryID, snapshot, entries, identity)
}

func replaceMetadataSnapshotWithIdentity(ctx context.Context, db *sql.DB, repositoryID string, snapshot models.Snapshot, entries []models.SnapshotEntry, identity MetadataEntrySetIdentity) error {
	if err := validateMetadataEntrySetIdentity(identity); err != nil {
		return err
	}
	handle, _, err := metadataCacheWriter(ctx, db, repositoryID)
	if err != nil {
		return err
	}
	tx, err := handle.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := detachMetadataSnapshotsTx(ctx, tx, repositoryID, []string{snapshot.ID}); err != nil {
		return err
	}
	if err := upsertMetadataSnapshotHeaderTxContext(ctx, tx, repositoryID, snapshot, "pending", "", ""); err != nil {
		return err
	}
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return err
	}
	rootScope, err := metadataRootScope(snapshot)
	if err != nil {
		return err
	}
	var entrySetID int64
	err = tx.QueryRowContext(ctx, `SELECT entry_set_id FROM metadata_cache_entry_sets
		WHERE repository_database_id=? AND engine=? AND native_identity=? AND root_scope=?`, repositoryDatabaseID, identity.Engine, identity.NativeIdentity, rootScope).Scan(&entrySetID)
	if errors.Is(err, sql.ErrNoRows) {
		result, insertErr := tx.ExecContext(ctx, `INSERT INTO metadata_cache_entry_sets(repository_database_id,engine,native_identity,root_scope)
			VALUES (?,?,?,?)`, repositoryDatabaseID, identity.Engine, identity.NativeIdentity, rootScope)
		if insertErr != nil {
			return insertErr
		}
		entrySetID, err = result.LastInsertId()
		if err != nil {
			return err
		}
		if err := insertMetadataEntriesBatchedTxContext(ctx, tx, repositoryID, entrySetID, snapshot, entries); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metadata_cache_snapshots SET entry_set_id=?,status='ready',indexed_at=?,error=''
		WHERE repository_database_id=? AND snapshot_id=?`, entrySetID, time.Now().UTC().Format(time.RFC3339), repositoryDatabaseID, snapshot.ID); err != nil {
		return err
	}
	if err := bumpMetadataRevisionTxContext(ctx, tx, repositoryID); err != nil {
		return err
	}
	return tx.Commit()
}

type stagedMetadataMembership struct {
	root, parent, name, mode, size string
	isDir                          bool
}

// SQLite builds in supported packages admit far more parameters, but measured
// larger statements were slower. A 900-parameter ceiling also works with
// conservative builds and bounds SQL text, bind work, and cancellation latency
// without introducing an ingestion framework.
const metadataInsertParameterLimit = 900

func insertMetadataTempRows(ctx context.Context, tx *sql.Tx, prefix, tuple string, parametersPerRow int, rows [][]any) error {
	if parametersPerRow <= 0 || len(rows) == 0 {
		return nil
	}
	batchSize := metadataInsertParameterLimit / parametersPerRow
	var statement *sql.Stmt
	preparedRows := 0
	defer func() {
		if statement != nil {
			_ = statement.Close()
		}
	}()
	for start := 0; start < len(rows); start += batchSize {
		end := min(start+batchSize, len(rows))
		rowCount := end - start
		arguments := make([]any, 0, (end-start)*parametersPerRow)
		for _, row := range rows[start:end] {
			if len(row) != parametersPerRow {
				return fmt.Errorf("metadata staging row has %d parameters, want %d", len(row), parametersPerRow)
			}
			arguments = append(arguments, row...)
		}
		if preparedRows != rowCount {
			if statement != nil {
				_ = statement.Close()
			}
			values := make([]string, rowCount)
			for index := range values {
				values[index] = tuple
			}
			var err error
			statement, err = tx.PrepareContext(ctx, prefix+strings.Join(values, ","))
			if err != nil {
				return err
			}
			preparedRows = rowCount
		}
		// Most snapshots execute the full batch shape hundreds of times. Reuse
		// one compiled SQLite statement for those batches and prepare at most one
		// smaller tail; rebuilding the same large placeholder SQL on every batch
		// added measurable parse/bind time without reducing peak memory.
		if _, err := statement.ExecContext(ctx, arguments...); err != nil {
			return err
		}
	}
	return nil
}

func insertMetadataEntriesBatchedTx(tx *sql.Tx, repositoryID string, entrySetID int64, snapshot models.Snapshot, entries []models.SnapshotEntry) error {
	return insertMetadataEntriesBatchedTxContext(context.Background(), tx, repositoryID, entrySetID, snapshot, entries)
}

func insertMetadataEntriesBatchedTxContext(ctx context.Context, tx *sql.Tx, repositoryID string, entrySetID int64, snapshot models.Snapshot, entries []models.SnapshotEntry) error {
	if snapshot.NativeRootType == "f" {
		if len(entries) != 0 {
			return fmt.Errorf("Kopia file-root snapshot cannot contain indexed entries")
		}
		return nil
	}

	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return err
	}
	_, normalizedRoots, err := normalizedMetadataSnapshotRoots(snapshot)
	if err != nil {
		return err
	}
	memberships := map[string]stagedMetadataMembership{}
	// Explicit paths already live in memberships with their version facts. Keep
	// this second map limited to versionless roots and implied ancestors so a
	// large snapshot is not duplicated in Go and in both SQLite staging tables.
	// Reintroducing explicit rows here roughly doubles ingestion allocations.
	supportRows := map[string][3]string{}
	for _, entry := range entries {
		foundRoot := false
		entryRoot := normalizeMetadataRoot(entry.SourceRoot)
		if entryRoot == "" && len(normalizedRoots) == 1 {
			foundRoot, entryRoot = true, normalizedRoots[0]
		} else {
			for _, root := range normalizedRoots {
				if root == entryRoot {
					foundRoot = true
					break
				}
			}
		}
		if !foundRoot {
			return fmt.Errorf("metadata entry root does not belong to snapshot")
		}
		path := normalizeMetadataPath(entry.Path)
		if path == "" {
			continue
		}
		parent, name := splitMetadataRelative(path)
		membership := stagedMetadataMembership{root: entryRoot, parent: parent, name: name, mode: entry.Mode, size: entry.Size, isDir: entry.IsDir}
		key := entryRoot + "\x00" + parent + "\x00" + name
		if prior, ok := memberships[key]; ok && (prior.mode != membership.mode || prior.size != membership.size || prior.isDir != membership.isDir) {
			return fmt.Errorf("metadata entry has conflicting cached facts")
		}
		memberships[key] = membership
		current := parent
		for current != "" {
			ancestorParent, ancestorName := splitMetadataRelative(current)
			ancestorKey := entryRoot + "\x00" + ancestorParent + "\x00" + ancestorName
			// supportRows is also the run-local ancestor-closure set. A prior
			// insertion of this exact root/case-sensitive ancestor completed its
			// chain toward the root, so another walk can stop without a second
			// seen-parent map or an input-order dependency.
			if _, expanded := supportRows[ancestorKey]; expanded {
				break
			}
			supportRows[ancestorKey] = [3]string{entryRoot, ancestorParent, ancestorName}
			current = ancestorParent
		}
	}
	for _, root := range normalizedRoots {
		supportRows[root+"\x00\x00"] = [3]string{root, "", ""}
	}
	statements := []string{
		`CREATE TEMP TABLE IF NOT EXISTS replicaro_metadata_staged_files(root TEXT,parent_path TEXT,name TEXT,PRIMARY KEY(root,parent_path,name)) WITHOUT ROWID`,
		`CREATE TEMP TABLE IF NOT EXISTS replicaro_metadata_staged_memberships(root TEXT,parent_path TEXT,name TEXT,mode TEXT,size TEXT,is_dir INTEGER,PRIMARY KEY(root,parent_path,name)) WITHOUT ROWID`,
		`DELETE FROM replicaro_metadata_staged_files`, `DELETE FROM replicaro_metadata_staged_memberships`,
	}
	for _, statement := range statements {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	stagedFiles := make([][]any, 0, len(supportRows))
	for _, row := range supportRows {
		stagedFiles = append(stagedFiles, []any{row[0], row[1], row[2]})
	}
	if err := insertMetadataTempRows(ctx, tx,
		`INSERT INTO replicaro_metadata_staged_files(root,parent_path,name) VALUES `, `(?,?,?)`, 3, stagedFiles); err != nil {
		return err
	}
	stagedMemberships := make([][]any, 0, len(memberships))
	for _, row := range memberships {
		stagedMemberships = append(stagedMemberships, []any{row.root, row.parent, row.name, row.mode, row.size, boolInt(row.isDir)})
	}
	if err := insertMetadataTempRows(ctx, tx,
		`INSERT INTO replicaro_metadata_staged_memberships(root,parent_path,name,mode,size,is_dir) VALUES `,
		`(?,?,?,?,?,?)`, 6, stagedMemberships); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO metadata_cache_files(repository_database_id,root,parent_path,name)
		SELECT ?,root,parent_path,name FROM replicaro_metadata_staged_files`, repositoryDatabaseID); err != nil {
		return err
	}
	// Materialize explicit catalog rows from the membership staging table rather
	// than staging the same 250k-scale path set twice. This must precede version
	// and membership resolution; source roots and implied ancestors remain the
	// deliberately versionless rows inserted above.
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO metadata_cache_files(repository_database_id,root,parent_path,name)
		SELECT ?,root,parent_path,name FROM replicaro_metadata_staged_memberships`, repositoryDatabaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO metadata_cache_file_versions(file_id,mode,size,is_dir)
		SELECT f.file_id,m.mode,m.size,m.is_dir FROM replicaro_metadata_staged_memberships m
		JOIN metadata_cache_files f ON f.repository_database_id=? AND f.root=m.root AND f.parent_path=m.parent_path AND f.name=m.name`, repositoryDatabaseID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS replicaro_metadata_resolved`); err != nil {
		return err
	}
	// Resolve every normalized ID through one repository/root-scoped set join.
	// One total count check validates the complete set without per-row triggers
	// or a repeated repository integer in compact memberships.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE replicaro_metadata_resolved AS
		SELECT f.file_id,v.file_version_id
		FROM replicaro_metadata_staged_memberships m
		JOIN metadata_cache_entry_sets setrow ON setrow.entry_set_id=? AND setrow.repository_database_id=?
		JOIN metadata_cache_files f ON f.repository_database_id=setrow.repository_database_id
			AND f.root=m.root AND f.parent_path=m.parent_path AND f.name=m.name
		JOIN metadata_cache_file_versions v ON v.file_id=f.file_id AND v.mode=m.mode AND v.size=m.size AND v.is_dir=m.is_dir`, entrySetID, repositoryDatabaseID); err != nil {
		return err
	}
	var stagedCount, resolvedCount int
	if err := tx.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM replicaro_metadata_staged_memberships),(SELECT COUNT(*) FROM replicaro_metadata_resolved)`).Scan(&stagedCount, &resolvedCount); err != nil {
		return err
	}
	if stagedCount != resolvedCount {
		return fmt.Errorf("metadata entry-set resolution count mismatch: staged %d, resolved %d", stagedCount, resolvedCount)
	}
	return internMetadataBlocksTx(ctx, tx, entrySetID)
}

const metadataDeleteCandidateVersionsSQL = `DELETE FROM metadata_cache_file_versions AS version
	WHERE (version.file_id,version.file_version_id) IN
		(SELECT candidate.file_id,candidate.file_version_id FROM replicaro_metadata_candidate_versions candidate)
	AND NOT EXISTS(SELECT 1 FROM metadata_cache_entries e
		WHERE e.file_id=version.file_id AND e.file_version_id=version.file_version_id)`

func cleanupFinalMetadataEntrySetsTx(ctx context.Context, tx *sql.Tx, repositoryDatabaseID int64, entrySetIDs []int64) error {
	if len(entrySetIDs) == 0 {
		return nil
	}
	for _, statement := range []string{
		`CREATE TEMP TABLE IF NOT EXISTS replicaro_metadata_final_sets(entry_set_id INTEGER PRIMARY KEY) WITHOUT ROWID`,
		`CREATE TEMP TABLE IF NOT EXISTS replicaro_metadata_candidate_files(file_id INTEGER PRIMARY KEY,depth INTEGER NOT NULL) WITHOUT ROWID`,
		`CREATE TEMP TABLE IF NOT EXISTS replicaro_metadata_candidate_parent_paths(root TEXT NOT NULL,path TEXT NOT NULL,PRIMARY KEY(root,path)) WITHOUT ROWID`,
		`CREATE TEMP TABLE IF NOT EXISTS replicaro_metadata_candidate_versions(file_id INTEGER NOT NULL,file_version_id INTEGER NOT NULL,PRIMARY KEY(file_id,file_version_id)) WITHOUT ROWID`,
		`DELETE FROM replicaro_metadata_final_sets`, `DELETE FROM replicaro_metadata_candidate_files`,
		`DELETE FROM replicaro_metadata_candidate_parent_paths`, `DELETE FROM replicaro_metadata_candidate_versions`,
	} {
		if _, err := tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	finalSetRows := make([][]any, 0, len(entrySetIDs))
	for _, id := range entrySetIDs {
		finalSetRows = append(finalSetRows, []any{id})
	}
	if err := insertMetadataTempRows(ctx, tx,
		`INSERT OR IGNORE INTO replicaro_metadata_final_sets(entry_set_id) VALUES `, `(?)`, 1, finalSetRows); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replicaro_metadata_candidate_versions(file_id,file_version_id)
		SELECT e.file_id,e.file_version_id FROM metadata_cache_entries e JOIN replicaro_metadata_final_sets sets ON sets.entry_set_id=e.entry_set_id`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replicaro_metadata_candidate_files(file_id,depth)
		SELECT DISTINCT f.file_id,CASE WHEN f.parent_path='' THEN CASE WHEN f.name='' THEN 0 ELSE 1 END
			ELSE 2+length(f.parent_path)-length(replace(f.parent_path,'/','')) END
		FROM metadata_cache_entries e JOIN replicaro_metadata_final_sets sets ON sets.entry_set_id=e.entry_set_id
		JOIN metadata_cache_files f ON f.file_id=e.file_id`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replicaro_metadata_candidate_parent_paths(root,path)
		SELECT DISTINCT f.root,f.parent_path FROM replicaro_metadata_candidate_files candidate
		JOIN metadata_cache_files f ON f.file_id=candidate.file_id WHERE f.parent_path<>''`); err != nil {
		return err
	}
	// Expand ancestors one level per statement. Besides avoiding a large recursive
	// CTE, these bounded writes give local writer preemption a prompt context
	// boundary while retaining the same all-or-nothing transaction.
	for {
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replicaro_metadata_candidate_files(file_id,depth)
			SELECT DISTINCT parent.file_id,
				CASE WHEN parent.parent_path='' THEN CASE WHEN parent.name='' THEN 0 ELSE 1 END
				ELSE 2+length(parent.parent_path)-length(replace(parent.parent_path,'/','')) END
			FROM replicaro_metadata_candidate_parent_paths candidate
			JOIN metadata_cache_files parent ON parent.repository_database_id=? AND parent.root=candidate.root
				AND candidate.path=CASE WHEN parent.parent_path='' THEN parent.name ELSE parent.parent_path||'/'||parent.name END`, repositoryDatabaseID)
		if err != nil {
			return err
		}
		inserted, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if inserted == 0 {
			break
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replicaro_metadata_candidate_parent_paths(root,path)
			SELECT DISTINCT f.root,f.parent_path FROM replicaro_metadata_candidate_files candidate
			JOIN metadata_cache_files f ON f.file_id=candidate.file_id WHERE f.parent_path<>''`); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO replicaro_metadata_candidate_files(file_id,depth)
		SELECT f.file_id,0 FROM metadata_cache_entry_sets setrow
		JOIN replicaro_metadata_final_sets sets ON sets.entry_set_id=setrow.entry_set_id
		JOIN json_each(setrow.root_scope) scope
		JOIN metadata_cache_files f ON f.repository_database_id=? AND f.root=CAST(scope.value AS TEXT) COLLATE BINARY
			AND f.parent_path='' AND f.name=''`, repositoryDatabaseID); err != nil {
		return err
	}
	// Targeted cleanup stages only final sets. Root scope contributes candidates
	// even for an empty tree, while shared sets take the header-only path and
	// never scan membership/file/version rows.
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS replicaro_metadata_final_lists(block_list_id INTEGER PRIMARY KEY);
 DELETE FROM replicaro_metadata_final_lists;
 INSERT OR IGNORE INTO replicaro_metadata_final_lists SELECT block_list_id FROM metadata_cache_entry_sets
 WHERE entry_set_id IN (SELECT entry_set_id FROM replicaro_metadata_final_sets);
 CREATE TEMP TABLE IF NOT EXISTS replicaro_metadata_final_blocks(block_id INTEGER PRIMARY KEY);
 DELETE FROM replicaro_metadata_final_blocks;
 INSERT OR IGNORE INTO replicaro_metadata_final_blocks SELECT block_id FROM metadata_cache_block_list_items
 WHERE block_list_id IN (SELECT block_list_id FROM replicaro_metadata_final_lists)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metadata_cache_entry_sets WHERE entry_set_id IN (SELECT entry_set_id FROM replicaro_metadata_final_sets)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metadata_cache_block_lists WHERE block_list_id IN
 (SELECT block_list_id FROM replicaro_metadata_final_lists) AND NOT EXISTS
 (SELECT 1 FROM metadata_cache_entry_sets s WHERE s.block_list_id=metadata_cache_block_lists.block_list_id);
 DELETE FROM metadata_cache_blocks WHERE block_id IN (SELECT block_id FROM replicaro_metadata_final_blocks)
 AND NOT EXISTS (SELECT 1 FROM metadata_cache_block_list_items l WHERE l.block_id=metadata_cache_blocks.block_id)`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, metadataDeleteCandidateVersionsSQL); err != nil {
		return err
	}
	var maxDepth int
	if err := tx.QueryRowContext(ctx, `SELECT COALESCE(MAX(depth),0) FROM replicaro_metadata_candidate_files`).Scan(&maxDepth); err != nil {
		return err
	}
	for depth := maxDepth; depth >= 0; depth-- {
		if _, err := tx.ExecContext(ctx, `DELETE FROM metadata_cache_files AS f
			WHERE f.file_id IN (SELECT file_id FROM replicaro_metadata_candidate_files WHERE depth=?)
			AND NOT EXISTS(SELECT 1 FROM metadata_cache_entries e WHERE e.file_id=f.file_id)
			AND NOT EXISTS(SELECT 1 FROM metadata_cache_file_versions v WHERE v.file_id=f.file_id)
			AND NOT EXISTS(SELECT 1 FROM metadata_cache_files child WHERE child.repository_database_id=f.repository_database_id
				AND child.root=f.root AND child.name<>''
				AND child.parent_path=CASE WHEN f.name='' THEN '' WHEN f.parent_path='' THEN f.name ELSE f.parent_path||'/'||f.name END)
			AND (f.name<>'' OR NOT EXISTS(SELECT 1 FROM metadata_cache_entry_sets surviving
				JOIN json_each(surviving.root_scope) scope
				WHERE surviving.repository_database_id=f.repository_database_id
					AND CAST(scope.value AS TEXT)=f.root COLLATE BINARY))`, depth); err != nil {
			return err
		}
	}
	return nil
}

func detachMetadataSnapshotsTx(ctx context.Context, tx *sql.Tx, repositoryID string, snapshotIDs []string) error {
	if len(snapshotIDs) == 0 {
		return nil
	}
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return err
	}
	if err := stageMetadataSnapshotIDs(ctx, tx, snapshotIDs); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT DISTINCT s.entry_set_id FROM metadata_cache_snapshots s
		JOIN replicaro_snapshot_ids target ON target.snapshot_id=s.snapshot_id
		WHERE s.repository_database_id=? AND s.entry_set_id IS NOT NULL
		AND NOT EXISTS(SELECT 1 FROM metadata_cache_snapshots survivor
			WHERE survivor.repository_database_id=s.repository_database_id AND survivor.entry_set_id=s.entry_set_id
			AND NOT EXISTS(SELECT 1 FROM replicaro_snapshot_ids all_targets WHERE all_targets.snapshot_id=survivor.snapshot_id))`, repositoryDatabaseID)
	if err != nil {
		return err
	}
	finalSets := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		finalSets = append(finalSets, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `DELETE FROM metadata_cache_snapshots WHERE repository_database_id=?
		AND EXISTS(SELECT 1 FROM replicaro_snapshot_ids target WHERE target.snapshot_id=metadata_cache_snapshots.snapshot_id)`, repositoryDatabaseID)
	if err != nil {
		return err
	}
	if err := cleanupFinalMetadataEntrySetsTx(ctx, tx, repositoryDatabaseID, finalSets); err != nil {
		return err
	}
	return bumpMetadataRevisionIfChangedContext(ctx, tx, repositoryID, result)
}

func ReplaceMetadataSnapshots(db *sql.DB, repositoryID string, snapshots []models.Snapshot, entries map[string][]models.SnapshotEntry) error {
	for _, snapshot := range snapshots {
		if err := ReplaceMetadataSnapshot(db, repositoryID, snapshot, entries[snapshot.ID]); err != nil {
			return err
		}
	}
	return RemoveMetadataSnapshotsNotIn(db, repositoryID, snapshotIDs(snapshots))
}

func replaceMetadataSnapshotTx(tx *sql.Tx, repositoryID string, snapshot models.Snapshot, entries []models.SnapshotEntry, identity MetadataEntrySetIdentity) error {
	if err := validateMetadataEntrySetIdentity(identity); err != nil {
		return err
	}
	if err := detachMetadataSnapshotsTx(context.Background(), tx, repositoryID, []string{snapshot.ID}); err != nil {
		return err
	}
	if err := upsertMetadataSnapshotHeaderTx(tx, repositoryID, snapshot, "pending", "", ""); err != nil {
		return err
	}
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return err
	}
	rootScope, err := metadataRootScope(snapshot)
	if err != nil {
		return err
	}
	result, err := tx.Exec(`INSERT INTO metadata_cache_entry_sets(repository_database_id,engine,native_identity,root_scope) VALUES (?,?,?,?)`,
		repositoryDatabaseID, identity.Engine, identity.NativeIdentity, rootScope)
	if err != nil {
		return err
	}
	entrySetID, err := result.LastInsertId()
	if err != nil {
		return err
	}
	if err := insertMetadataEntriesBatchedTx(tx, repositoryID, entrySetID, snapshot, entries); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE metadata_cache_snapshots SET entry_set_id=?,status='ready',indexed_at=?,error=''
		WHERE repository_database_id=? AND snapshot_id=?`, entrySetID, time.Now().UTC().Format(time.RFC3339), repositoryDatabaseID, snapshot.ID)
	return err
}

// The cache has no catalog overlay; _files is populated with the entry set.
func ReuseMetadataSnapshotEntrySet(db *sql.DB, repositoryID string, snapshot models.Snapshot, identity MetadataEntrySetIdentity) (bool, error) {
	return reuseMetadataSnapshotEntrySet(context.Background(), db, repositoryID, snapshot, identity)
}

func ReuseMetadataSnapshotEntrySetForRefresh(db *sql.DB, repositoryID string, snapshot models.Snapshot, identity MetadataEntrySetIdentity) (bool, error) {
	return ReuseMetadataSnapshotEntrySetForRefreshContext(context.Background(), db, repositoryID, snapshot, identity)
}

func ReuseMetadataSnapshotEntrySetForRefreshContext(ctx context.Context, db *sql.DB, repositoryID string, snapshot models.Snapshot, identity MetadataEntrySetIdentity) (bool, error) {
	return reuseMetadataSnapshotEntrySet(ctx, db, repositoryID, snapshot, identity)
}

func reuseMetadataSnapshotEntrySet(ctx context.Context, db *sql.DB, repositoryID string, snapshot models.Snapshot, identity MetadataEntrySetIdentity) (bool, error) {
	if err := validateMetadataEntrySetIdentity(identity); err != nil {
		return false, err
	}
	handle, _, err := metadataCacheWriter(ctx, db, repositoryID)
	if err != nil {
		return false, err
	}
	tx, err := handle.writer.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return false, err
	}
	rootScope, err := metadataRootScope(snapshot)
	if err != nil {
		return false, err
	}
	var entrySetID int64
	err = tx.QueryRowContext(ctx, `SELECT entry_set_id FROM metadata_cache_entry_sets
		WHERE repository_database_id=? AND engine=? AND native_identity=? AND root_scope=?`, repositoryDatabaseID, identity.Engine, identity.NativeIdentity, rootScope).Scan(&entrySetID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err := detachMetadataSnapshotsTx(ctx, tx, repositoryID, []string{snapshot.ID}); err != nil {
		return false, err
	}
	if err := upsertMetadataSnapshotHeaderTxContext(ctx, tx, repositoryID, snapshot, "ready", time.Now().UTC().Format(time.RFC3339), ""); err != nil {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metadata_cache_snapshots SET entry_set_id=?
		WHERE repository_database_id=? AND snapshot_id=?`, entrySetID, repositoryDatabaseID, snapshot.ID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func MetadataEntrySetExistsForSnapshot(db *sql.DB, repositoryID string, snapshot models.Snapshot, identity MetadataEntrySetIdentity) (bool, error) {
	if err := validateMetadataEntrySetIdentity(identity); err != nil {
		return false, err
	}
	rootScope, err := metadataRootScope(snapshot)
	if err != nil {
		return false, err
	}
	handle, _, release, err := metadataCacheReader(context.Background(), db, repositoryID)
	if err != nil {
		return false, err
	}
	defer release()
	var exists int
	err = handle.readers.QueryRow(`SELECT EXISTS(SELECT 1 FROM metadata_cache_entry_sets setrow
		JOIN repositories r ON r.database_id=setrow.repository_database_id
		WHERE r.id=? AND setrow.engine=? AND setrow.native_identity=? AND setrow.root_scope=?)`,
		repositoryID, identity.Engine, identity.NativeIdentity, rootScope).Scan(&exists)
	return exists != 0, err
}

func MarkMetadataSnapshotFailed(db *sql.DB, repositoryID string, snapshot models.Snapshot, indexErr error) error {
	return MarkMetadataSnapshotFailedAt(db, repositoryID, snapshot, indexErr, time.Now())
}

func MarkMetadataSnapshotFailedAt(db *sql.DB, repositoryID string, snapshot models.Snapshot, indexErr error, attemptedAt time.Time) error {
	return markMetadataSnapshotFailedAt(db, repositoryID, snapshot, indexErr, attemptedAt)
}

func MarkMetadataSnapshotFailedForRefresh(db *sql.DB, repositoryID string, snapshot models.Snapshot, indexErr error) error {
	return markMetadataSnapshotFailedAt(db, repositoryID, snapshot, indexErr, time.Now())
}

func markMetadataSnapshotFailedAt(db *sql.DB, repositoryID string, snapshot models.Snapshot, indexErr error, attemptedAt time.Time) error {
	handle, _, err := metadataCacheWriter(context.Background(), db, repositoryID)
	if err != nil {
		return err
	}
	tx, err := handle.writer.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := detachMetadataSnapshotsTx(context.Background(), tx, repositoryID, []string{snapshot.ID}); err != nil {
		return err
	}
	safeError := "metadata indexing failed"
	if indexErr != nil && strings.TrimSpace(indexErr.Error()) != "" {
		safeError = indexErr.Error()
	}
	if err := upsertMetadataSnapshotHeaderTx(tx, repositoryID, snapshot, "failed", attemptedAt.UTC().Format(time.RFC3339), safeError); err != nil {
		return err
	}
	return tx.Commit()
}

func RemoveMetadataSnapshotsNotIn(db *sql.DB, repositoryID string, snapshotIDs []string) error {
	handle, _, err := metadataCacheWriter(context.Background(), db, repositoryID)
	if err != nil {
		return err
	}
	tx, err := handle.writer.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return err
	}
	if err := stageMetadataSnapshotIDs(context.Background(), tx, snapshotIDs); err != nil {
		return err
	}
	rows, err := tx.Query(`SELECT snapshot_id FROM metadata_cache_snapshots s WHERE repository_database_id=?
		AND NOT EXISTS(SELECT 1 FROM replicaro_snapshot_ids keep WHERE keep.snapshot_id=s.snapshot_id)`, repositoryDatabaseID)
	if err != nil {
		return err
	}
	remove := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return err
		}
		remove = append(remove, id)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := detachMetadataSnapshotsTx(context.Background(), tx, repositoryID, remove); err != nil {
		return err
	}
	return tx.Commit()
}

func MetadataRevision(db *sql.DB, repositoryID string) (int64, error) {
	handle, _, release, err := metadataCacheReader(context.Background(), db, repositoryID)
	if err != nil {
		return 0, err
	}
	defer release()
	tx, err := handle.readers.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	revision, err := metadataRevisionTx(tx, repositoryID)
	if err != nil {
		return 0, err
	}
	return revision, tx.Commit()
}

func stageMetadataSnapshotIDs(ctx context.Context, tx *sql.Tx, snapshotIDs []string) error {
	if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS replicaro_snapshot_ids(snapshot_id TEXT PRIMARY KEY) WITHOUT ROWID`); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM replicaro_snapshot_ids`); err != nil {
		return err
	}
	rows := make([][]any, 0, len(snapshotIDs))
	for _, id := range snapshotIDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("snapshot ID is required")
		}
		rows = append(rows, []any{id})
	}
	return insertMetadataTempRows(ctx, tx, `INSERT OR IGNORE INTO replicaro_snapshot_ids(snapshot_id) VALUES `, `(?)`, 1, rows)
}

func MetadataLastReconciled(db *sql.DB, repositoryID string) (string, error) {
	handle, _, release, err := metadataCacheReader(context.Background(), db, repositoryID)
	if err != nil {
		return "", err
	}
	defer release()
	var value string
	err = handle.readers.QueryRow(`SELECT COALESCE(st.last_reconciled,'') FROM repositories r
		LEFT JOIN metadata_cache_state st ON st.repository_database_id=r.database_id WHERE r.id=?`, repositoryID).Scan(&value)
	return value, err
}

func ListMetadataSnapshots(db *sql.DB, repositoryID string) ([]models.Snapshot, error) {
	authority, err := LoadMetadataReadAuthority(context.Background(), db, repositoryID)
	if err != nil {
		return nil, err
	}
	return ListMetadataSnapshotsForAuthority(context.Background(), db, authority)
}

func ListMetadataSnapshotsForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority) ([]models.Snapshot, error) {
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
	result, err := listMetadataSnapshotsTx(ctx, tx, repositoryDatabaseID, nil, false)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

func ReadyMetadataSnapshotsForAuthority(ctx context.Context, db *sql.DB, authority MetadataReadAuthority, snapshotIDs []string) ([]models.Snapshot, error) {
	if len(snapshotIDs) == 0 {
		return []models.Snapshot{}, nil
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
	result, err := listMetadataSnapshotsTx(ctx, tx, repositoryDatabaseID, snapshotIDs, true)
	if err != nil {
		return nil, err
	}
	return result, tx.Commit()
}

func listMetadataSnapshotsTx(ctx context.Context, tx *sql.Tx, repositoryDatabaseID int64, snapshotIDs []string, readyOnly bool) ([]models.Snapshot, error) {
	query := `SELECT snapshot_id,timestamp,size,duration,source,marker_status,marker_profile_uuid,marker_job_uuid,
		presentation_client_uuid,presentation_machine_label,total_file_count,native_root_type
		FROM metadata_cache_snapshots WHERE repository_database_id=?`
	args := []any{repositoryDatabaseID}
	if readyOnly {
		query += ` AND status='ready'`
	}
	if len(snapshotIDs) != 0 {
		query += ` AND snapshot_id IN (` + makePlaceholders(len(snapshotIDs)) + `)`
		for _, snapshotID := range snapshotIDs {
			args = append(args, snapshotID)
		}
	}
	query += ` ORDER BY timestamp DESC,snapshot_id`
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	result := []models.Snapshot{}
	for rows.Next() {
		var snapshot models.Snapshot
		var markerStatus, client, machine string
		if err := rows.Scan(&snapshot.ID, &snapshot.Timestamp, &snapshot.Size, &snapshot.Duration, &snapshot.Source,
			&markerStatus, &snapshot.OwnershipMarker.ProfileID, &snapshot.OwnershipMarker.JobID, &client, &machine, &snapshot.TotalFileCount, &snapshot.NativeRootType); err != nil {
			return nil, err
		}
		snapshot.OwnershipMarker.Status = models.SnapshotOwnershipMarkerStatus(markerStatus)
		snapshot.PresentationClientUUID, snapshot.PresentationMachineLabel = client, machine
		result = append(result, snapshot)
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := loadMetadataSnapshotRootsContext(ctx, tx, repositoryDatabaseID, result); err != nil {
		return nil, err
	}
	return result, nil
}

func MetadataSnapshotsNeedingIndex(db *sql.DB, repositoryID string, now time.Time, bypassCooldown bool) ([]models.Snapshot, error) {
	cutoff := now.UTC().Add(-MetadataFailureRetryCooldown).Format(time.RFC3339)
	handle, _, release, err := metadataCacheReader(context.Background(), db, repositoryID)
	if err != nil {
		return nil, err
	}
	defer release()
	db = handle.readers
	rows, err := db.Query(`SELECT s.snapshot_id,s.timestamp,s.size,s.duration,s.source,s.marker_status,s.marker_profile_uuid,s.marker_job_uuid,
			s.presentation_client_uuid,s.presentation_machine_label,s.total_file_count,s.native_root_type
		FROM metadata_cache_snapshots s JOIN repositories r ON r.database_id=s.repository_database_id
		WHERE r.id=? AND (s.status='pending' OR (s.status='failed' AND (? OR s.indexed_at='' OR s.indexed_at<=?)))
		ORDER BY s.timestamp DESC,s.snapshot_id`, repositoryID, bypassCooldown, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []models.Snapshot{}
	for rows.Next() {
		var snapshot models.Snapshot
		var markerStatus string
		if err := rows.Scan(&snapshot.ID, &snapshot.Timestamp, &snapshot.Size, &snapshot.Duration, &snapshot.Source,
			&markerStatus, &snapshot.OwnershipMarker.ProfileID, &snapshot.OwnershipMarker.JobID,
			&snapshot.PresentationClientUUID, &snapshot.PresentationMachineLabel, &snapshot.TotalFileCount, &snapshot.NativeRootType); err != nil {
			return nil, err
		}
		snapshot.OwnershipMarker.Status = models.SnapshotOwnershipMarkerStatus(markerStatus)
		result = append(result, snapshot)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := loadMetadataSnapshotRootsContext(context.Background(), db, 1, result); err != nil {
		return nil, err
	}
	return result, nil
}

func ReconcileMetadataSnapshotHeaders(db *sql.DB, repositoryID string, snapshots []models.Snapshot) error {
	ctx := context.Background()
	handle, _, err := metadataCacheWriter(ctx, db, repositoryID)
	if err != nil {
		return err
	}
	tx, err := handle.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := reconcileMetadataSnapshotHeadersTx(ctx, tx, repositoryID, snapshots); err != nil {
		return err
	}
	return tx.Commit()
}

type metadataHeaderDelta struct{ changed bool }

func reconcileMetadataSnapshotHeadersTx(ctx context.Context, tx *sql.Tx, repositoryID string, snapshots []models.Snapshot) (metadataHeaderDelta, error) {
	delta := metadataHeaderDelta{}
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return delta, err
	}
	keep := snapshotIDs(snapshots)
	if err := stageMetadataSnapshotIDs(ctx, tx, keep); err != nil {
		return delta, err
	}
	rows, err := tx.QueryContext(ctx, `SELECT snapshot_id FROM metadata_cache_snapshots s WHERE repository_database_id=?
		AND NOT EXISTS(SELECT 1 FROM replicaro_snapshot_ids keep WHERE keep.snapshot_id=s.snapshot_id)`, repositoryDatabaseID)
	if err != nil {
		return delta, err
	}
	remove := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return delta, err
		}
		remove = append(remove, id)
	}
	if err := rows.Close(); err != nil {
		return delta, err
	}
	if len(remove) != 0 {
		delta.changed = true
		if err := detachMetadataSnapshotsTx(ctx, tx, repositoryID, remove); err != nil {
			return delta, err
		}
	}
	for _, snapshot := range snapshots {
		var existing models.Snapshot
		var markerStatus, existingClient, existingMachine string
		err := tx.QueryRowContext(ctx, `SELECT timestamp,size,duration,source,marker_status,marker_profile_uuid,marker_job_uuid
			,presentation_client_uuid,presentation_machine_label,native_root_type
			FROM metadata_cache_snapshots WHERE repository_database_id=? AND snapshot_id=?`, repositoryDatabaseID, snapshot.ID).Scan(
			&existing.Timestamp, &existing.Size, &existing.Duration, &existing.Source, &markerStatus,
			&existing.OwnershipMarker.ProfileID, &existing.OwnershipMarker.JobID, &existingClient, &existingMachine, &existing.NativeRootType)
		if errors.Is(err, sql.ErrNoRows) {
			delta.changed = true
			if err := upsertMetadataSnapshotHeaderTxContext(ctx, tx, repositoryID, snapshot, "pending", "", ""); err != nil {
				return delta, err
			}
			continue
		}
		if err != nil {
			return delta, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE metadata_cache_snapshots SET total_file_count=? WHERE repository_database_id=? AND snapshot_id=?`, snapshot.TotalFileCount, repositoryDatabaseID, snapshot.ID); err != nil {
			return delta, err
		}
		existing.OwnershipMarker.Status = models.SnapshotOwnershipMarkerStatus(markerStatus)
		wantStatus, wantProfile, wantJob := metadataSnapshotMarker(snapshot)
		wantClient, wantMachine := models.ParseSnapshotPresentationTags(snapshot.Tags)
		rootsEqual, rootsErr := metadataSnapshotRootsEqualTx(ctx, tx, repositoryDatabaseID, snapshot)
		if rootsErr != nil {
			return delta, rootsErr
		}
		if existing.Timestamp != canonicalMetadataTimestamp(snapshot.Timestamp) || existing.Size != snapshot.Size || existing.Duration != snapshot.Duration ||
			existing.Source != snapshot.Source || markerStatus != wantStatus || existing.OwnershipMarker.ProfileID != wantProfile || existing.OwnershipMarker.JobID != wantJob ||
			existingClient != wantClient || existingMachine != wantMachine || existing.NativeRootType != snapshot.NativeRootType || !rootsEqual {
			delta.changed = true
			if err := detachMetadataSnapshotsTx(ctx, tx, repositoryID, []string{snapshot.ID}); err != nil {
				return delta, err
			}
			if err := upsertMetadataSnapshotHeaderTxContext(ctx, tx, repositoryID, snapshot, "pending", "", ""); err != nil {
				return delta, err
			}
		}
	}
	return delta, nil
}

func ReconcileMetadataSnapshotHeadersComplete(db *sql.DB, repositoryID string, snapshots []models.Snapshot, at time.Time, observedGeneration int64) error {
	_, _, err := reconcileMetadataSnapshotHeadersComplete(context.Background(), db, repositoryID, snapshots, at, observedGeneration, false)
	return err
}

func ReconcileMetadataSnapshotHeadersCompleteForRefresh(db *sql.DB, repositoryID string, snapshots []models.Snapshot, at time.Time, observedGeneration int64) (bool, error) {
	_, required, err := reconcileMetadataSnapshotHeadersComplete(context.Background(), db, repositoryID, snapshots, at, observedGeneration, true)
	return required, err
}

func ReconcileMetadataSnapshotHeadersCompleteForRefreshGeneration(db *sql.DB, repositoryID string, snapshots []models.Snapshot, at time.Time, observedGeneration int64) (int64, bool, error) {
	return reconcileMetadataSnapshotHeadersComplete(context.Background(), db, repositoryID, snapshots, at, observedGeneration, true)
}

func ReconcileMetadataSnapshotHeadersCompleteForRefreshGenerationContext(ctx context.Context, db *sql.DB, repositoryID string, snapshots []models.Snapshot, at time.Time, observedGeneration int64) (int64, bool, error) {
	return reconcileMetadataSnapshotHeadersComplete(ctx, db, repositoryID, snapshots, at, observedGeneration, true)
}

func reconcileMetadataSnapshotHeadersComplete(ctx context.Context, db *sql.DB, repositoryID string, snapshots []models.Snapshot, at time.Time, observedGeneration int64, reserveDetectedChange bool) (int64, bool, error) {
	handle, authority, err := metadataCacheWriter(ctx, db, repositoryID)
	if err != nil {
		return observedGeneration, false, err
	}
	tx, err := handle.writer.BeginTx(ctx, nil)
	if err != nil {
		return observedGeneration, false, err
	}
	defer func() { _ = tx.Rollback() }()
	delta, err := reconcileMetadataSnapshotHeadersTx(ctx, tx, repositoryID, snapshots)
	if err != nil {
		return observedGeneration, false, err
	}
	repositoryDatabaseID, err := metadataRepositoryDatabaseIDForAuthority(ctx, tx, authority)
	if err != nil {
		return observedGeneration, false, err
	}
	if authority.RequiredGeneration != observedGeneration {
		return observedGeneration, false, nil
	}
	var incomplete int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata_cache_snapshots WHERE repository_database_id=? AND status<>'ready')`, repositoryDatabaseID).Scan(&incomplete); err != nil {
		return observedGeneration, false, err
	}
	needsWork := delta.changed || incomplete != 0
	targetGeneration := observedGeneration
	if reserveDetectedChange && needsWork {
		var appliedGeneration int64
		if err := tx.QueryRowContext(ctx, `SELECT applied_generation FROM metadata_cache_state
			WHERE repository_database_id=?`, repositoryDatabaseID).Scan(&appliedGeneration); err != nil {
			return observedGeneration, false, err
		}
		if appliedGeneration == observedGeneration {
			result, reserveErr := db.ExecContext(ctx, `UPDATE repositories
				SET required_generation=required_generation+1
				WHERE id=? AND metadata_cache_binding=? AND required_generation=?`,
				authority.RepositoryID, authority.Binding, observedGeneration)
			if reserveErr != nil {
				return observedGeneration, false, reserveErr
			}
			reserved, reserveErr := result.RowsAffected()
			if reserveErr != nil {
				return observedGeneration, false, reserveErr
			}
			if reserved != 1 {
				return observedGeneration, false, nil
			}
			targetGeneration = observedGeneration + 1
		} else if appliedGeneration > observedGeneration {
			return observedGeneration, false, fmt.Errorf("metadata cache generation exceeds authoritative generation")
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metadata_cache_state SET last_reconciled=? WHERE repository_database_id=?`,
		at.UTC().Format(time.RFC3339), repositoryDatabaseID); err != nil {
		return targetGeneration, false, err
	}
	return targetGeneration, needsWork, tx.Commit()
}

func CompleteMetadataRefresh(db *sql.DB, repositoryID string, observedGeneration int64) (bool, error) {
	return CompleteMetadataRefreshContext(context.Background(), db, repositoryID, observedGeneration)
}

func CompleteMetadataRefreshContext(ctx context.Context, db *sql.DB, repositoryID string, observedGeneration int64) (bool, error) {
	// The one main read happens immediately before final cache publication. Main
	// first can transiently report incomplete after a cache failure, but it cannot
	// report a stale cache complete. A later main reservation is observed by the
	// next request, so neither a second main read nor an acknowledgement write-back
	// is needed after this cache transaction.
	handle, authority, err := metadataCacheWriter(ctx, db, repositoryID)
	if err != nil {
		return false, err
	}
	if authority.RequiredGeneration != observedGeneration {
		return false, nil
	}
	tx, err := handle.writer.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	repositoryDatabaseID, err := metadataRepositoryDatabaseIDForAuthority(ctx, tx, authority)
	if err != nil {
		return false, err
	}
	var incomplete int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM metadata_cache_snapshots
		WHERE repository_database_id=? AND status<>'ready')`, repositoryDatabaseID).Scan(&incomplete); err != nil {
		return false, err
	}
	if incomplete != 0 {
		return false, fmt.Errorf("metadata refresh still has snapshots without ready entries")
	}
	var appliedGeneration int64
	if err := tx.QueryRowContext(ctx, `SELECT applied_generation FROM metadata_cache_state
		WHERE repository_database_id=?`, repositoryDatabaseID).Scan(&appliedGeneration); err != nil {
		return false, err
	}
	// This is the single final publication transaction. It cannot clear a newer
	// reserved generation and it runs only after every included header is ready.
	result, err := tx.ExecContext(ctx, `UPDATE metadata_cache_state SET applied_generation=?
		WHERE repository_database_id=? AND applied_generation<>?`,
		observedGeneration, repositoryDatabaseID, observedGeneration)
	if err != nil {
		return false, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	if count == 1 {
		if err := bumpMetadataRevisionTxContext(ctx, tx, repositoryID); err != nil {
			return false, err
		}
	}
	// A successful header listing is not complete until the native session has
	// closed and this final transaction commits. Clearing an earlier failure here
	// keeps an unchanged ready cache gated throughout that interval as well.
	if _, err := tx.ExecContext(ctx, `DELETE FROM metadata_cache_failures WHERE repository_database_id=?`, repositoryDatabaseID); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

func MarkMetadataReconciled(db *sql.DB, repositoryID string, at time.Time) error {
	handle, _, err := metadataCacheWriter(context.Background(), db, repositoryID)
	if err != nil {
		return err
	}
	_, err = handle.writer.Exec(`INSERT INTO metadata_cache_state(repository_database_id,last_reconciled,applied_generation)
		SELECT database_id,?,0 FROM repositories WHERE id=?
		ON CONFLICT(repository_database_id) DO UPDATE SET last_reconciled=excluded.last_reconciled`, at.UTC().Format(time.RFC3339), repositoryID)
	return err
}

func snapshotIDs(snapshots []models.Snapshot) []string {
	ids := make([]string, 0, len(snapshots))
	for _, snapshot := range snapshots {
		ids = append(ids, snapshot.ID)
	}
	return ids
}

func makePlaceholders(count int) string { return strings.TrimRight(strings.Repeat("?,", count), ",") }

func escapeLike(value string) string {
	value = strings.ReplaceAll(value, "\\", "\\\\")
	value = strings.ReplaceAll(value, "%", "\\%")
	return strings.ReplaceAll(value, "_", "\\_")
}

func searchLikePattern(value string) string {
	var pattern strings.Builder
	pattern.WriteByte('%')
	for _, character := range value {
		switch character {
		case '*':
			pattern.WriteByte('%')
		case '?':
			pattern.WriteByte('_')
		default:
			pattern.WriteString(escapeLike(string(character)))
		}
	}
	pattern.WriteByte('%')
	return pattern.String()
}

// Native roots and descendants are archive addresses. The viewing OS has no
// authority over their spelling: backslashes and whitespace can be literal.
func normalizeMetadataRoot(value string) string { return value }

func normalizeMetadataPath(value string) string {
	return strings.Trim(value, "/")
}

func splitMetadataRelative(value string) (string, string) {
	value = normalizeMetadataPath(value)
	index := strings.LastIndexByte(value, '/')
	if index < 0 {
		return "", value
	}
	return value[:index], value[index+1:]
}

func joinMetadataRelative(parent, name string) string {
	if parent == "" {
		return name
	}
	if name == "" {
		return parent
	}
	return parent + "/" + name
}

func metadataBase(path string) string {
	_, name := splitMetadataRelative(path)
	return name
}
