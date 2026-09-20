package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"sort"

	"github.com/local/replicaro/models"
)

// Header counts are optional native observations, not a traversal estimate.
// In particular, an unavailable count must not prevent indexing that snapshot.
func RequiredMetadataBucketCount(snapshots []models.Snapshot, current int64) int64 {
	var largest int64
	available := false
	for _, snapshot := range snapshots {
		if snapshot.TotalFileCount != nil && *snapshot.TotalFileCount >= 0 {
			largest = max(largest, *snapshot.TotalFileCount)
			available = true
		}
	}
	if !available && current >= 100 {
		return current
	}
	if largest <= 10000 {
		return 100
	}
	return largest / 100
}

// Division avoids overflow at either multiplication boundary, including native
// counts near MaxInt64. Equality intentionally triggers a resize.
func MetadataBucketResizeRequired(current, required int64) bool {
	return current >= 100 && required >= 100 && (required/2 >= current || required <= current/2)
}

func MetadataBucketCount(ctx context.Context, db *sql.DB, repositoryID string) (int64, error) {
	handle, _, release, err := metadataCacheReader(ctx, db, repositoryID)
	if err != nil {
		return 0, err
	}
	defer release()
	var count int64
	err = handle.readers.QueryRowContext(ctx, `SELECT bucket_count FROM metadata_cache_state WHERE repository_database_id=1`).Scan(&count)
	return count, err
}

// InitializeMetadataBucketCount selects the fresh cache layout under the existing
// metadata admission. An interrupted first pass may already contain complete sets;
// those require repartitioning even when last_reconciled is still empty.
func InitializeMetadataBucketCount(ctx context.Context, db *sql.DB, repositoryID string, count int64) (bool, error) {
	if count < 100 {
		return false, fmt.Errorf("invalid metadata bucket count")
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
	// Test eligibility and change the count in the same writer transaction, so
	// no content writer can publish membership using the previous partition.
	result, err := tx.ExecContext(ctx, `UPDATE metadata_cache_state SET bucket_count=?
		WHERE repository_database_id=1 AND last_reconciled=''
		AND NOT EXISTS (SELECT 1 FROM metadata_cache_entry_sets)
		AND NOT EXISTS (SELECT 1 FROM metadata_cache_block_list_items)
		AND NOT EXISTS (SELECT 1 FROM metadata_cache_block_entries)`, count)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed == 1, tx.Commit()
}

type metadataBlockMember struct{ file, version int64 }

// Resolve file/version identities first. Bucket membership is stable for a
// vault's current count, so a changed version replaces only its bucket block.
// Digest matches are confirmed against the complete ordered member pairs: a
// collision can never merge unrelated native addresses or versions. The pairs
// remain stored only once, rather than duplicated in a fingerprint blob/index.
func internMetadataBlocksTx(ctx context.Context, tx *sql.Tx, entrySetID int64) error {
	var count int64
	if err := tx.QueryRowContext(ctx, `SELECT bucket_count FROM metadata_cache_state WHERE repository_database_id=1`).Scan(&count); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT file_id,file_version_id FROM replicaro_metadata_resolved ORDER BY file_id`)
	if err != nil {
		return err
	}
	buckets := map[int64][]metadataBlockMember{}
	for rows.Next() {
		var member metadataBlockMember
		if err := rows.Scan(&member.file, &member.version); err != nil {
			rows.Close()
			return err
		}
		buckets[member.file%count] = append(buckets[member.file%count], member)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	ordinals := make([]int64, 0, len(buckets))
	for bucket := range buckets {
		ordinals = append(ordinals, bucket)
	}
	sort.Slice(ordinals, func(i, j int) bool { return ordinals[i] < ordinals[j] })
	list := make([]byte, 0, len(ordinals)*16)
	listRows := make([][]any, 0, len(ordinals))
	for _, bucket := range ordinals {
		if err := ctx.Err(); err != nil {
			return err
		}
		members := buckets[bucket]
		content := make([]byte, 0, len(members)*16)
		for _, member := range members {
			content = binary.BigEndian.AppendUint64(content, uint64(member.file))
			content = binary.BigEndian.AppendUint64(content, uint64(member.version))
		}
		digest := sha256.Sum256(content)
		var blockID int64
		blockID, err = findExactMetadataBlockTx(ctx, tx, digest[:], members)
		if err == sql.ErrNoRows {
			result, err := tx.ExecContext(ctx, `INSERT INTO metadata_cache_blocks(digest) VALUES(?)`, digest[:])
			if err != nil {
				return err
			}
			blockID, err = result.LastInsertId()
			if err != nil {
				return err
			}
			entries := make([][]any, 0, len(members))
			for _, member := range members {
				entries = append(entries, []any{blockID, member.file, member.version})
			}
			if err := insertMetadataTempRows(ctx, tx, `INSERT INTO metadata_cache_block_entries(block_id,file_id,file_version_id) VALUES `, `(?,?,?)`, 3, entries); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		list = binary.BigEndian.AppendUint64(list, uint64(bucket))
		list = binary.BigEndian.AppendUint64(list, uint64(blockID))
		listRows = append(listRows, []any{bucket, blockID})
	}
	digest := sha256.Sum256(list)
	var listID int64
	err = tx.QueryRowContext(ctx, `SELECT block_list_id FROM metadata_cache_block_lists WHERE digest=? AND content=?`, digest[:], list).Scan(&listID)
	if err == sql.ErrNoRows {
		result, err := tx.ExecContext(ctx, `INSERT INTO metadata_cache_block_lists(digest,content) VALUES(?,?)`, digest[:], list)
		if err != nil {
			return err
		}
		listID, err = result.LastInsertId()
		if err != nil {
			return err
		}
		for i := range listRows {
			listRows[i] = append([]any{listID}, listRows[i]...)
		}
		if err := insertMetadataTempRows(ctx, tx, `INSERT INTO metadata_cache_block_list_items(block_list_id,bucket,block_id) VALUES `, `(?,?,?)`, 3, listRows); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE metadata_cache_entry_sets SET block_list_id=? WHERE entry_set_id=?`, listID, entrySetID)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("metadata entry set disappeared during block publication")
	}
	return nil
}

func findExactMetadataBlockTx(ctx context.Context, tx *sql.Tx, digest []byte, members []metadataBlockMember) (int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT block_id FROM metadata_cache_blocks WHERE digest=?`, digest)
	if err != nil {
		return 0, err
	}
	candidates := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		candidates = append(candidates, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return 0, err
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	for _, id := range candidates {
		rows, err := tx.QueryContext(ctx, `SELECT file_id,file_version_id FROM metadata_cache_block_entries WHERE block_id=? ORDER BY file_id`, id)
		if err != nil {
			return 0, err
		}
		index := 0
		equal := true
		for rows.Next() {
			var member metadataBlockMember
			if err := rows.Scan(&member.file, &member.version); err != nil {
				rows.Close()
				return 0, err
			}
			if index >= len(members) || member != members[index] {
				equal = false
				break
			}
			index++
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, err
		}
		if err := rows.Close(); err != nil {
			return 0, err
		}
		if equal && index == len(members) {
			return id, nil
		}
	}
	return 0, sql.ErrNoRows
}
