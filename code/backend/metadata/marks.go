package metadata

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
)

var resticSnapshotDiff = engines.ResticSnapshotDiff
var storeMetadataSnapshotMarks = database.StoreMetadataSnapshotMarks

// resticMarksPending reports whether a Restic vault has indexed snapshots
// still waiting for their diff marks (see indexResticMarks).
func resticMarksPending(ctx context.Context, db *sql.DB, repo models.Repository) (bool, error) {
	if repo.Engine != engines.ResticID {
		return false, nil
	}
	needed, err := database.MetadataSnapshotsNeedingMarks(ctx, db, repo.ID)
	return len(needed) > 0, err
}

type resticMarksResult struct {
	need    database.MetadataMarksNeed
	changes []engines.ResticDiffChange
	output  string
	err     error
}

// indexResticMarks runs `restic diff --json --metadata <previous> <this>`
// for every indexed snapshot whose marks are missing or were made against a
// backup that is no longer its previous one, and stores the files it marked.
// It is part of indexing: it runs inside the same refresh, under the same
// low-priority shared vault lock, after the content listings, and each diff
// is one more read-only native command per neighbouring pair (two backups
// removed in a row still cost one diff for the survivor after them).
//
// A diff that fails although both backups are listed is a failed indexing
// step for that snapshot: it is marked failed like a failed listing, with the
// diff's output, the same cooldown and Retry, and File History waits. That
// also covers another computer removing the previous backup between the
// header listing and the diff: Restic refuses the missing snapshot, and the
// next refresh, which lists the headers again, compares the snapshot with its
// new neighbour. Nothing is added to prevent
// that; it is rare and repairs itself.
//
// A diff is not skipped when two backups have the same tree ID. It would
// rarely save anything: a change in any folder above the source gives two
// backups of an unchanged source different tree IDs. And a diff costs about
// one listing and reads only folder metadata Restic keeps in its local cache.
//
// A diff runs only once the previous backup is listed too (see
// database.MetadataSnapshotsNeedingMarks): against a backup whose own listing
// failed it would fail as well, and failing this snapshot would drop its good
// listing.
func indexResticMarks(ctx context.Context, db *sql.DB, manager engines.Engine, repo models.Repository, readers int) error {
	if repo.Engine != engines.ResticID {
		return nil
	}
	needed, err := database.MetadataSnapshotsNeedingMarks(ctx, db, repo.ID)
	if err != nil || len(needed) == 0 {
		return err
	}
	readers = max(readers, 1)
	phase := beginIndexPhase("snapshot_version_marks")
	diagnostic := IndexPhaseDiagnostic{Snapshots: len(needed)}
	defer func() { phase.finish(diagnostic) }()
	var firstErr, persistenceErr error
	for start := 0; start < len(needed); start += readers {
		setCoordinatorStage(db, repo, "indexing_entries")
		window := needed[start:min(start+readers, len(needed))]
		results := make([]resticMarksResult, len(window))
		var wait sync.WaitGroup
		for index, need := range window {
			wait.Add(1)
			go func() {
				defer wait.Done()
				result := resticMarksResult{need: need}
				result.changes, result.output, result.err = resticSnapshotDiff(ctx, manager, repo, need.Base, need.Snapshot.ID)
				results[index] = result
			}()
		}
		wait.Wait()
		if err := ctx.Err(); err != nil {
			return err
		}
		diagnostic.ListedSnapshots += len(window)
		for _, result := range results {
			snapshot := result.need.Snapshot
			if result.err != nil {
				if markErr := markMetadataSnapshotFailed(db, repo.ID, snapshot, result.err); markErr != nil && persistenceErr == nil {
					persistenceErr = markErr
				}
				if firstErr == nil {
					firstErr = &engineError{output: result.output, err: result.err}
				}
				continue
			}
			files, err := resticMarkedFiles(snapshot, result.changes)
			if err == nil {
				err = storeMetadataSnapshotMarks(ctx, db, repo.ID, snapshot.ID, result.need.Base, files)
			}
			if errors.Is(err, database.ErrMetadataMarksStale) {
				// The snapshot changed while diff ran; whatever replaced it is
				// checked again on the next refresh.
				continue
			}
			if err != nil && persistenceErr == nil {
				persistenceErr = err
			}
			diagnostic.Entries += len(files)
		}
	}
	if persistenceErr != nil {
		return persistenceErr
	}
	return firstErr
}

// resticMarkedFiles keeps the changes that start a new version of a file:
// anything but a removal, on a path that isn't a folder. Each path is mapped
// to the cached file it names through the snapshot's native roots, the same
// way its content listing was mapped. Folders above the roots, which diff
// also reports, map to nothing and are dropped.
func resticMarkedFiles(snapshot models.Snapshot, changes []engines.ResticDiffChange) ([]database.MetadataMarkedFile, error) {
	roots, grouping, err := database.MetadataSnapshotRoots(snapshot)
	if err != nil {
		return nil, fmt.Errorf("map Restic diff paths: %w", err)
	}
	var files []database.MetadataMarkedFile
	for _, change := range changes {
		if change.Removed() || change.IsDir {
			continue
		}
		for index, root := range roots {
			if relative, ok := engines.ResticRelativePath(root.Path, change.Path); ok {
				files = append(files, database.MetadataMarkedFile{Root: grouping[index], Path: relative})
			}
		}
	}
	return files, nil
}
