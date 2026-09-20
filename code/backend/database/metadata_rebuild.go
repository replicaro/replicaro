package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/replicaro/models"
)

type MetadataRebuildSnapshot struct {
	Snapshot models.Snapshot
	Identity MetadataEntrySetIdentity
}

var publishMetadataReplacement = replaceMetadataCacheFile

// RebuildMetadataCache runs only under the existing low-priority vault admission
// and coordinator serialization. The original remains the recovery copy until
// one same-directory atomic replacement publishes both contents and bucket count.
// Nothing here writes a native repository or adds another remote locking scheme.
func RebuildMetadataCache(ctx context.Context, db *sql.DB, repositoryID string, count int64,
	snapshots []MetadataRebuildSnapshot, listMissing func(context.Context, models.Snapshot) ([]models.SnapshotEntry, error),
	finishNative func() error) (resultErr error) {
	if count < 100 {
		return fmt.Errorf("invalid metadata bucket count")
	}
	source, authority, err := metadataCacheWriter(ctx, db, repositoryID)
	if err != nil {
		return err
	}
	metadataCaches.Lock()
	if metadataCaches.closing[source.path] || metadataCaches.rebuilding[source.path] {
		metadataCaches.Unlock()
		return ErrMetadataIndexingUnavailable
	}
	metadataCaches.rebuilding[source.path] = true
	var readsDrained <-chan struct{}
	if source.readersInUse > 0 {
		if source.readsDrained == nil {
			source.readsDrained = make(chan struct{})
		}
		readsDrained = source.readsDrained
	}
	metadataCaches.Unlock()
	// Stop admitting cache work before draining the writer. A transaction already
	// using this pool finishes against the original; a later one cannot start.
	// The vault admission keeps native/local mutation writers out of this interval.
	writerClosed := false
	defer func() {
		metadataCaches.Lock()
		if writerClosed {
			delete(metadataCaches.handles, source.path)
		}
		delete(metadataCaches.rebuilding, source.path)
		metadataCaches.Unlock()
	}()
	// A query may have acquired its handle but not begun a SQLite transaction.
	// Draining admitted queries as well as SQL transactions prevents that caller
	// from reopening an old pool across replacement. Waiting remains cancellable.
	if readsDrained != nil {
		select {
		case <-readsDrained:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	writerLease, err := source.writer.Conn(ctx)
	if err != nil {
		return err
	}
	defer writerLease.Close()
	var applied int64
	if err := source.readers.QueryRowContext(ctx, `SELECT applied_generation FROM metadata_cache_state WHERE repository_database_id=1`).Scan(&applied); err != nil {
		return err
	}
	if applied == authority.RequiredGeneration {
		result, err := db.ExecContext(ctx, `UPDATE repositories SET required_generation=required_generation+1 WHERE id=? AND metadata_cache_binding=? AND required_generation=?`, repositoryID, authority.Binding, authority.RequiredGeneration)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if changed != 1 {
			return ErrMetadataIndexingUnavailable
		}
		authority.RequiredGeneration++
	}
	// A query-only read pool cannot checkpoint the last writer's WAL on close.
	// Flush and truncate it before retiring the writer, otherwise immutable
	// admission could see an empty main file, or old WAL pages could shadow the
	// newly renamed replacement. The leased sole writer also drains prior writes.
	var busy, logFrames, checkpointed int
	if err := writerLease.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("metadata readers prevented cache checkpoint")
	}
	writerClosed = true
	defer func() {
		if writerClosed {
			resultErr = errors.Join(resultErr, closeMetadataCacheDatabase(source.readers))
		}
	}()
	if err := closeMetadataCacheDatabase(source.writer); err != nil {
		return err
	}
	if err := writerLease.Close(); err != nil {
		return err
	}

	file, err := os.CreateTemp(filepath.Dir(source.path), metadataReplacementPrefix(source.path)+"*.db")
	if err != nil {
		return err
	}
	temporary := file.Name()
	defer func() {
		for _, path := range []string{temporary, temporary + "-wal", temporary + "-shm"} {
			if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
				resultErr = errors.Join(resultErr, err)
			}
		}
	}()
	if err := file.Close(); err != nil {
		return err
	}
	target, err := openMetadataCache(temporary, repositoryID, authority.Binding)
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, closeMetadataCacheHandle(target)) }()
	tx, err := target.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE metadata_cache_state SET bucket_count=? WHERE repository_database_id=1`, count); err != nil {
		return err
	}
	var originalRevision int64
	if err := source.readers.QueryRowContext(ctx, `SELECT COALESCE((SELECT revision FROM metadata_cache_revisions WHERE repository_database_id=1),0)`).Scan(&originalRevision); err != nil {
		return err
	}
	for _, item := range snapshots {
		if err := ctx.Err(); err != nil {
			return err
		}
		snapshot := item.Snapshot
		identity := item.Identity
		if identity.NativeIdentity == "" {
			identity.NativeIdentity = snapshot.ID
		}
		if err := validateMetadataEntrySetIdentity(identity); err != nil {
			return err
		}
		scope, err := metadataRootScope(snapshot)
		if err != nil {
			return err
		}
		if err := upsertMetadataSnapshotHeaderTxContext(ctx, tx, repositoryID, snapshot, "pending", "", ""); err != nil {
			return err
		}
		var setID int64
		err = tx.QueryRowContext(ctx, `SELECT entry_set_id FROM metadata_cache_entry_sets WHERE engine=? AND native_identity=? AND root_scope=?`, identity.Engine, identity.NativeIdentity, scope).Scan(&setID)
		if errors.Is(err, sql.ErrNoRows) {
			entries, ready, err := rebuildCachedEntries(ctx, source.readers, snapshot, identity, scope)
			if err != nil {
				return err
			}
			if !ready {
				entries, err = listMissing(ctx, snapshot)
				if err != nil {
					return err
				}
			}
			result, err := tx.ExecContext(ctx, `INSERT INTO metadata_cache_entry_sets(repository_database_id,engine,native_identity,root_scope) VALUES(1,?,?,?)`, identity.Engine, identity.NativeIdentity, scope)
			if err != nil {
				return err
			}
			setID, err = result.LastInsertId()
			if err != nil {
				return err
			}
			if err := insertMetadataEntriesBatchedTxContext(ctx, tx, repositoryID, setID, snapshot, entries); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE metadata_cache_snapshots SET entry_set_id=?,status='ready',indexed_at=? WHERE snapshot_id=?`, setID, time.Now().UTC().Format(time.RFC3339), snapshot.ID); err != nil {
			return err
		}
	}
	if err := finishNative(); err != nil {
		return err
	}
	// This is the existing one final authoritative binding/generation read. A
	// newer local reservation leaves the original intact and the cache dirty.
	latest, err := LoadMetadataReadAuthority(ctx, db, repositoryID)
	if err != nil {
		return err
	}
	if latest.Binding != authority.Binding {
		return ErrMetadataCacheBinding
	}
	if latest.RequiredGeneration != authority.RequiredGeneration {
		return ErrMetadataIndexingUnavailable
	}
	if _, err := tx.ExecContext(ctx, `UPDATE metadata_cache_state SET applied_generation=?,last_reconciled=? WHERE repository_database_id=1`, authority.RequiredGeneration, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO metadata_cache_revisions(repository_database_id,revision) VALUES(1,?) ON CONFLICT(repository_database_id) DO UPDATE SET revision=excluded.revision`, originalRevision+1); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if err := target.writer.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return err
	}
	if busy != 0 {
		return fmt.Errorf("metadata replacement checkpoint was incomplete")
	}
	if err := closeMetadataCacheHandle(target); err != nil {
		return err
	}
	target = nil
	if err := closeMetadataCacheDatabase(source.readers); err != nil {
		return err
	}
	for _, path := range []string{temporary + "-wal", temporary + "-shm"} {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	// Pools and WAL files are closed before replacement, which is required on
	// Windows and prevents an old pool from opening a connection to the new file.
	if err := publishMetadataReplacement(temporary, source.path); err != nil {
		return err
	}
	return nil
}

func rebuildCachedEntries(ctx context.Context, db *sql.DB, snapshot models.Snapshot, identity MetadataEntrySetIdentity, scope string) ([]models.SnapshotEntry, bool, error) {
	var setID int64
	err := db.QueryRowContext(ctx, `SELECT e.entry_set_id FROM metadata_cache_entry_sets e
 WHERE engine=? AND native_identity=? AND root_scope=?
 AND EXISTS(SELECT 1 FROM metadata_cache_snapshots s WHERE s.entry_set_id=e.entry_set_id AND s.status='ready')`, identity.Engine, identity.NativeIdentity, scope).Scan(&setID)
	if errors.Is(err, sql.ErrNoRows) {
		// Entry-only caching may have used a snapshot identity before a native index
		// session supplied its immutable object identity. Exact snapshot + ordered
		// roots is still a complete local observation, never a guessed object match.
		err = db.QueryRowContext(ctx, `SELECT e.entry_set_id FROM metadata_cache_snapshots s JOIN metadata_cache_entry_sets e ON e.entry_set_id=s.entry_set_id
   WHERE s.snapshot_id=? AND s.status='ready' AND e.root_scope=? AND e.engine=? AND e.native_identity=?`, snapshot.ID, scope, identity.Engine, snapshot.ID).Scan(&setID)
	}
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	rows, err := db.QueryContext(ctx, `SELECT f.root,f.parent_path,f.name,v.mode,v.size,v.is_dir FROM metadata_cache_entries e
 JOIN metadata_cache_files f ON f.file_id=e.file_id JOIN metadata_cache_file_versions v ON v.file_id=e.file_id AND v.file_version_id=e.file_version_id
 WHERE e.entry_set_id=? ORDER BY f.file_id`, setID)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	entries := []models.SnapshotEntry{}
	for rows.Next() {
		var entry models.SnapshotEntry
		var parent string
		if err := rows.Scan(&entry.SourceRoot, &parent, &entry.Name, &entry.Mode, &entry.Size, &entry.IsDir); err != nil {
			return nil, false, err
		}
		entry.Path = joinMetadataRelative(parent, entry.Name)
		entries = append(entries, entry)
	}
	return entries, true, rows.Err()
}

func metadataReplacementPrefix(path string) string {
	return "." + filepath.Base(path) + ".replacement-"
}

// Called with cache admission held, after the original has passed binding and
// schema validation and before a handle is published. A live rebuild closes
// that admission, so only abandoned replacements for this exact vault qualify.
func removeAbandonedMetadataReplacements(path string) error {
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return err
	}
	prefix := metadataReplacementPrefix(path)
	var result error
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !entry.Type().IsRegular() {
			continue
		}
		suffix := strings.TrimPrefix(name, prefix)
		if strings.HasSuffix(suffix, "-wal") || strings.HasSuffix(suffix, "-shm") {
			suffix = suffix[:len(suffix)-4]
		}
		if !strings.HasSuffix(suffix, ".db") {
			continue
		}
		// os.CreateTemp uses decimal random suffixes. Restrict cleanup to that
		// exact shape, including orphaned WAL/SHM files left after a crash.
		number := strings.TrimSuffix(suffix, ".db")
		if number == "" || strings.Trim(number, "0123456789") != "" {
			continue
		}
		if err := removeMetadataCacheFile(filepath.Join(filepath.Dir(path), name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			result = errors.Join(result, err)
		}
	}
	return result
}
