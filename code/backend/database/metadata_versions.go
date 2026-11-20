package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/local/replicaro/models"
)

// File History versions
//
// A file's versions are runs of neighbouring backups in which the engine
// reported no change to the file. The engines decide what a change is, and
// they answer differently:
//
//   - Kopia: a file whose object ID differs from the one in the previous
//     backup starts a new version. Kopia keeps the object ID when only the
//     modified time or permissions change, so those don't split a version.
//   - Restic: `restic diff --json --metadata <previous> <this>` marks the
//     file with anything but a removal ("M", "U", "+", "T", "?", alone or
//     combined). Restic also marks metadata-only changes ("U").
//
// In both engines a backup that holds the file while the previous one doesn't
// starts a new version, and a backup without the file ends one. The first
// backup of a chain starts every file's first version, whatever marks it has.
//
// "The previous backup" is the previous cached backup in the same chain: the
// same computer (File History's computer grouping, see metadataComputerKey,
// the same grouping the computer label uses) and the same set of native source
// paths as the snapshot's own header lists them (native_source_paths, not the
// indexing roots: every untagged Restic snapshot is indexed beneath "/", and
// chaining on that would compare backups of different folders). Only
// neighbours are compared. Each backup run with the same content state of a
// file belongs to one version, but a change and its reversal, each held by a
// backup, are separate versions: nothing compares a backup with anything but
// its neighbour. Comparing across computers or source folders is deliberately
// not done: two computers backing up different files under one path would
// otherwise split every backup into its own version.
//
// The chain deliberately follows the computer grouping, not Restic's `parent`
// field, and a backup with a replicaro-client tag is grouped by that tag, not
// by its Restic host name. The host name changes whenever a Docker container
// is recreated, which would give every file a new version after each update,
// and Kopia has no per-computer host name at all; the tag covers both engines
// the same way. A Restic backup without the tag (one made outside Replicaro,
// for example) falls back to its host name, so for such backups a host-name
// change does start a new chain.
//
// Restic marks are stored per snapshot with the ID they were made against
// (marks_base). They are only valid while that ID is still the snapshot's
// previous backup in its chain; when a backup between two others is removed,
// or one lands between them, the later one gets a fresh diff against its new
// neighbour (see MetadataSnapshotsNeedingMarks). Until then the vault's File
// History waits, the same as for a backup whose contents aren't indexed yet.
// Marks are never moved to or merged into the next backup when one is
// removed. A merge of two backups' marks can't reproduce what Restic reports
// for the two remaining backups compared directly, so it could show other
// versions than a full rebuild; it would also need a merge rule for every pair
// of marks, couldn't be redone after a cache rebuild, and a backup that lands
// between two others would still need a diff. One new diff covers all of it.

// metadataComputerKey is the computer grouping File History uses for both the
// computer label and the chains that versions are computed in: the
// replicaro-client tag first, then Restic's host name, then a Kopia user name
// that is a client UUID, then one shared "previous computer" group.
func metadataComputerKey(client, engine, rootUser, rootHost string) string {
	// Do not replace the Restic host fallback with the host name of every
	// backup or with Restic's parent field: see the comment at the top of
	// this file.
	switch {
	case client != "":
		return "client:" + client
	case engine == "restic" && rootHost != "":
		return "restic-host:" + rootHost
	case engine == "kopia" && validMetadataClientUUID(rootUser):
		return "kopia-user:" + rootUser
	default:
		return "previous"
	}
}

type metadataChainMember struct {
	id, timestamp, status string
	marksBase             sql.NullString
	chain                 string
	position              int
}

type metadataChains struct {
	engine  string
	members map[string]*metadataChainMember
	chains  map[string][]*metadataChainMember
}

// predecessor returns the previous backup in the snapshot's chain, or nil for
// the first one (and for a snapshot that isn't cached).
func (chains metadataChains) predecessor(snapshotID string) *metadataChainMember {
	member := chains.members[snapshotID]
	if member == nil || member.position == 0 {
		return nil
	}
	return chains.chains[member.chain][member.position-1]
}

type metadataRowsReader interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

// loadMetadataChains reads every cached snapshot header (whatever its index
// status, since a diff needs no contents of the previous backup) and orders
// each chain by backup time, then snapshot ID so equal times stay stable.
func loadMetadataChains(ctx context.Context, reader metadataRowsReader, repositoryDatabaseID int64) (metadataChains, error) {
	chains := metadataChains{members: map[string]*metadataChainMember{}, chains: map[string][]*metadataChainMember{}}
	var err error
	if chains.engine, err = metadataCacheEngine(ctx, reader, repositoryDatabaseID); err != nil {
		return chains, err
	}
	// The computer group falls back to the first native root's user and host
	// (see metadataComputerKey); the paths half of the chain key is the
	// snapshot's native source paths, already stored in canonical form.
	rows, err := reader.QueryContext(ctx, `SELECT s.snapshot_id,s.timestamp,s.status,s.marks_base,s.presentation_client_uuid,
			s.native_source_paths,COALESCE(r.native_user,''),COALESCE(r.native_host,'')
		FROM metadata_cache_snapshots s
		LEFT JOIN metadata_cache_snapshot_roots r ON r.repository_database_id=s.repository_database_id
			AND r.snapshot_id=s.snapshot_id AND r.root_ordinal=0
		WHERE s.repository_database_id=? ORDER BY s.timestamp,s.snapshot_id`, repositoryDatabaseID)
	if err != nil {
		return chains, err
	}
	for rows.Next() {
		member := &metadataChainMember{}
		var client, paths, rootUser, rootHost string
		if err := rows.Scan(&member.id, &member.timestamp, &member.status, &member.marksBase, &client, &paths, &rootUser, &rootHost); err != nil {
			_ = rows.Close()
			return chains, err
		}
		member.chain = metadataComputerKey(client, chains.engine, rootUser, rootHost) + "\x00" + paths
		member.position = len(chains.chains[member.chain])
		chains.members[member.id] = member
		chains.chains[member.chain] = append(chains.chains[member.chain], member)
	}
	return chains, closeRows(rows)
}

// metadataNativeSourcePaths is the stored form of a snapshot's native source
// paths (metadata_cache_snapshots.native_source_paths): a JSON array, sorted
// and without repeats, so equal sets compare equal as text. Without
// NativeSourcePaths the snapshot's cached roots are its native source paths,
// as for Kopia, whose one root is its source path.
func metadataNativeSourcePaths(snapshot models.Snapshot) (string, error) {
	paths := append([]string{}, snapshot.NativeSourcePaths...)
	if len(paths) == 0 {
		for _, root := range metadataSnapshotRoots(snapshot) {
			paths = append(paths, root.Path)
		}
	}
	sort.Strings(paths)
	paths = slices.Compact(paths)
	encoded, err := json.Marshal(paths)
	return string(encoded), err
}

// metadataCacheEngine returns the engine of the vault's cached listings. All
// entry sets of a vault come from its one engine. A cache without any has no
// indexed snapshot, so nothing depends on the engine yet and it is "".
func metadataCacheEngine(ctx context.Context, reader metadataRowsReader, repositoryDatabaseID int64) (string, error) {
	var engine string
	err := reader.QueryRowContext(ctx, `SELECT COALESCE((SELECT engine FROM metadata_cache_entry_sets
		WHERE repository_database_id=? LIMIT 1),'')`, repositoryDatabaseID).Scan(&engine)
	return engine, err
}

// MetadataMarksNeed is one Restic snapshot whose diff marks are missing or
// were made against a backup that is no longer its previous one.
type MetadataMarksNeed struct {
	Snapshot models.Snapshot
	Base     string
	// runnable is false while the previous backup isn't indexed (pending, or
	// failed because its own listing failed). See metadataMarksNeeded.
	runnable bool
}

// metadataMarksNeeded lists the indexed Restic snapshots that need a diff
// against their previous backup. Only snapshots whose contents are indexed
// count: a pending or failed one is still waiting for its listing anyway,
// and its marks would need its file rows. Kopia vaults never need marks.
//
// A need is runnable only while the previous backup is ready too. When the
// previous backup's listing failed (for example an unreadable snapshot), a
// diff against it would fail the same way, and a failed diff marks this
// snapshot failed and drops its good listing, so Restore-page Browse and
// selected restore would lose it again after every cooldown. So no diff runs
// for it: its marks stay pending, File History shows the previous backup's
// failure and Retry, and once that backup is listed the diff runs in the same
// refresh. Do not let a blocked need schedule work: nothing could run, and
// access would start a refresh that does nothing each time File History is
// opened.
func metadataMarksNeeded(chains metadataChains) []MetadataMarksNeed {
	if chains.engine != "restic" {
		return nil
	}
	var needed []MetadataMarksNeed
	for _, chain := range chains.chains {
		for position, member := range chain {
			if position == 0 || member.status != "ready" {
				continue
			}
			previous := chain[position-1]
			if !member.marksBase.Valid || member.marksBase.String != previous.id {
				needed = append(needed, MetadataMarksNeed{Snapshot: models.Snapshot{ID: member.id, Timestamp: member.timestamp},
					Base: previous.id, runnable: previous.status == "ready"})
			}
		}
	}
	sort.Slice(needed, func(i, j int) bool {
		if needed[i].Snapshot.Timestamp != needed[j].Snapshot.Timestamp {
			return needed[i].Snapshot.Timestamp > needed[j].Snapshot.Timestamp
		}
		return needed[i].Snapshot.ID < needed[j].Snapshot.ID
	})
	return needed
}

func metadataMarksNeededTx(ctx context.Context, reader metadataRowsReader, repositoryDatabaseID int64) ([]MetadataMarksNeed, error) {
	chains, err := loadMetadataChains(ctx, reader, repositoryDatabaseID)
	if err != nil {
		return nil, err
	}
	return metadataMarksNeeded(chains), nil
}

// MetadataSnapshotsNeedingMarks returns the Restic snapshots that need a
// `restic diff` against their previous backup and can get one now (the
// previous backup is indexed; see metadataMarksNeeded), each with its cached
// native roots (the paths diff prints are mapped through them).
func MetadataSnapshotsNeedingMarks(ctx context.Context, db *sql.DB, repositoryID string) ([]MetadataMarksNeed, error) {
	handle, authority, release, err := metadataCacheReader(ctx, db, repositoryID)
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
	all, err := metadataMarksNeededTx(ctx, tx, repositoryDatabaseID)
	if err != nil {
		return nil, err
	}
	needed := []MetadataMarksNeed{}
	for _, need := range all {
		if need.runnable {
			needed = append(needed, need)
		}
	}
	if len(needed) == 0 {
		return needed, tx.Commit()
	}
	ids := make([]string, 0, len(needed))
	for _, need := range needed {
		ids = append(ids, need.Snapshot.ID)
	}
	snapshots, err := listMetadataSnapshotsTx(ctx, tx, repositoryDatabaseID, ids, true)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]models.Snapshot, len(snapshots))
	for _, snapshot := range snapshots {
		byID[snapshot.ID] = snapshot
	}
	result := needed[:0]
	for _, need := range needed {
		if snapshot, ok := byID[need.Snapshot.ID]; ok {
			need.Snapshot = snapshot
			result = append(result, need)
		}
	}
	return result, tx.Commit()
}

// MetadataMarkedFile names one file a diff marked, by the grouping root it is
// cached under and its path relative to that root ("" for a file root).
type MetadataMarkedFile struct {
	Root string
	Path string
}

// ErrMetadataMarksStale is returned when the snapshot to store marks for is no
// longer the indexed snapshot the diff was run for.
var ErrMetadataMarksStale = errors.New("metadata snapshot changed while its marks were computed")

// StoreMetadataSnapshotMarks replaces a Restic snapshot's marks with the
// files a `restic diff` against base reported, and records base. Paths that
// aren't cached are skipped: the index has no history row for them, so a
// mark there could never be shown.
func StoreMetadataSnapshotMarks(ctx context.Context, db *sql.DB, repositoryID, snapshotID, base string, files []MetadataMarkedFile) error {
	handle, _, err := metadataCacheWriter(ctx, db, repositoryID)
	if err != nil {
		return err
	}
	tx, err := handle.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	repositoryDatabaseID, err := metadataRepositoryDatabaseID(tx, repositoryID)
	if err != nil {
		return err
	}
	// The snapshot can have been re-indexed, failed or dropped while diff ran
	// (another refresh can't run, but a deletion of the vault's snapshot list
	// can land in between). Its marks are only stored on the same ready row.
	result, err := tx.ExecContext(ctx, `UPDATE metadata_cache_snapshots SET marks_base=?
		WHERE repository_database_id=? AND snapshot_id=? AND status='ready'`, base, repositoryDatabaseID, snapshotID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return ErrMetadataMarksStale
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metadata_cache_snapshot_marks WHERE repository_database_id=? AND snapshot_id=?`,
		repositoryDatabaseID, snapshotID); err != nil {
		return err
	}
	if len(files) > 0 {
		for _, statement := range []string{
			`CREATE TEMP TABLE IF NOT EXISTS replicaro_metadata_staged_marks(root TEXT,parent_path TEXT,name TEXT,PRIMARY KEY(root,parent_path,name)) WITHOUT ROWID`,
			`DELETE FROM replicaro_metadata_staged_marks`,
		} {
			if _, err := tx.ExecContext(ctx, statement); err != nil {
				return err
			}
		}
		rows := make([][]any, 0, len(files))
		seen := make(map[MetadataMarkedFile]bool, len(files))
		for _, file := range files {
			file.Root, file.Path = normalizeMetadataRoot(file.Root), normalizeMetadataPath(file.Path)
			if seen[file] {
				continue
			}
			seen[file] = true
			parent, name := splitMetadataRelative(file.Path)
			rows = append(rows, []any{file.Root, parent, name})
		}
		if err := insertMetadataTempRows(ctx, tx, `INSERT INTO replicaro_metadata_staged_marks(root,parent_path,name) VALUES `, `(?,?,?)`, 3, rows); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO metadata_cache_snapshot_marks(repository_database_id,snapshot_id,file_id)
			SELECT ?,?,f.file_id FROM replicaro_metadata_staged_marks m
			JOIN metadata_cache_files f ON f.repository_database_id=? AND f.root=m.root AND f.parent_path=m.parent_path AND f.name=m.name`,
			repositoryDatabaseID, snapshotID, repositoryDatabaseID); err != nil {
			return err
		}
	}
	if err := bumpMetadataRevisionTxContext(ctx, tx, repositoryID); err != nil {
		return err
	}
	return tx.Commit()
}

// metadataFileVersions groups a file's per-backup rows into versions (see
// the comment at the top of this file). rows are the file's history rows
// after applyMetadataMachineLabels, so they carry the computer label and are
// sorted by computer group, newest first; the versions keep that order.
// marked holds the snapshots whose Restic diff marked the file.
func metadataFileVersions(chains metadataChains, rows []metadataVersionRow, marked map[string]bool) []models.FileHistoryVersion {
	type run struct {
		first, last *metadataVersionRow
		order       int
	}
	rowOrder := make(map[*metadataVersionRow]int, len(rows))
	bySnapshot := make(map[string]*metadataVersionRow, len(rows))
	chainKeys := []string{}
	seenChains := map[string]bool{}
	for index := range rows {
		row := &rows[index]
		rowOrder[row] = index
		bySnapshot[row.version.SnapshotID] = row
		key := row.version.SnapshotID
		if member := chains.members[row.version.SnapshotID]; member != nil {
			key = member.chain
		}
		if !seenChains[key] {
			seenChains[key] = true
			chainKeys = append(chainKeys, key)
		}
	}
	var runs []*run
	for _, key := range chainKeys {
		members := chains.chains[key]
		if members == nil {
			// Not a cached chain (cannot happen inside one read transaction):
			// the row is a version of its own.
			members = []*metadataChainMember{{id: key}}
		}
		var current *run
		for _, member := range members {
			row := bySnapshot[member.id]
			if row == nil || row.version.IsDir {
				current = nil
				continue
			}
			changed := current == nil
			if !changed {
				if chains.engine == "restic" {
					changed = marked[member.id]
				} else {
					changed = row.objectID != current.last.objectID
				}
			}
			if changed {
				current = &run{first: row}
				runs = append(runs, current)
			}
			current.last = row
		}
	}
	for _, item := range runs {
		item.order = rowOrder[item.last]
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].order < runs[j].order })
	versions := make([]models.FileHistoryVersion, 0, len(runs))
	for _, item := range runs {
		versions = append(versions, models.FileHistoryVersion{
			SnapshotID:            item.last.version.SnapshotID,
			NativeRootID:          item.last.version.NativeRootID,
			Source:                item.last.version.Source,
			Modified:              models.NativeModifiedText(item.last.modified),
			Size:                  item.last.version.Size,
			RegularFile:           item.last.version.RegularFile,
			FirstBackup:           item.first.version.Timestamp,
			LastBackup:            item.last.version.Timestamp,
			MachineLabel:          item.last.version.MachineLabel,
			UnnamedComputer:       item.last.version.UnnamedComputer,
			UnnamedComputerNumber: item.last.version.UnnamedComputerNumber,
		})
	}
	return versions
}

// metadataMarkedSnapshotsTx returns the snapshots whose stored marks name
// the file.
func metadataMarkedSnapshotsTx(ctx context.Context, tx *sql.Tx, repositoryDatabaseID, fileID int64) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT snapshot_id FROM metadata_cache_snapshot_marks
		WHERE file_id=? AND repository_database_id=?`, fileID, repositoryDatabaseID)
	if err != nil {
		return nil, err
	}
	marked := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		marked[id] = true
	}
	return marked, closeRows(rows)
}

// metadataMarksPendingCount counts the indexed snapshots still waiting for
// their marks, for the index state and the read gate. That includes the ones
// whose diff can't run yet: they stay pending, not ready.
func metadataMarksPendingCount(ctx context.Context, reader metadataRowsReader, repositoryDatabaseID int64) (int, error) {
	engine, err := metadataCacheEngine(ctx, reader, repositoryDatabaseID)
	if err != nil {
		return 0, fmt.Errorf("check File History version marks: %w", err)
	}
	if engine != "restic" {
		return 0, nil
	}
	needed, err := metadataMarksNeededTx(ctx, reader, repositoryDatabaseID)
	if err != nil {
		return 0, fmt.Errorf("check File History version marks: %w", err)
	}
	return len(needed), nil
}
