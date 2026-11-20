package metadata

import (
	"database/sql"
	"log"

	"github.com/local/replicaro/database"
)

// ScheduleOutdatedCacheRebuilds starts indexing each vault whose metadata
// cache was written by an older version, once, when Replicaro starts. Such a
// cache can't be read (it lacks the facts File History versions need), and
// waiting for someone to open File History would leave the whole re-index for
// that moment. It is the one case where startup begins remote indexing on its
// own; a vault with a current cache, or with none at all, is left alone, and
// it uses the ordinary queue, locking, deferral and global slots like any
// other refresh.
//
// The old cache is removed here, from its header alone, before the refresh is
// queued (see database.DiscardOutdatedMetadataCache), whatever vault it was
// bound to. So the next start finds nothing outdated even when this refresh
// never opened the cache: a vault that can't be reached right now, or whose
// rebuild is interrupted, continues on its next use like any vault whose cache
// is missing.
//
// Call it after StartContext and before the API starts taking requests, so no
// cache is open while the old files are removed.
func ScheduleOutdatedCacheRebuilds(db *sql.DB) {
	repos, err := database.ListRepositories(db)
	if err != nil {
		log.Printf("check for File History caches of an older version: %v", err)
		return
	}
	for _, repo := range repos {
		discarded, err := database.DiscardOutdatedMetadataCache(db, repo.ID)
		if err != nil {
			log.Printf("check the File History cache of vault %s (%s): %v", repo.Name, repo.ID, err)
			continue
		}
		if !discarded {
			continue
		}
		log.Printf("vault %s (%s): the File History cache was written by an older version and is being rebuilt from the vault", repo.Name, repo.ID)
		scheduleRepositorySync(db, syncRequest{repository: repo, intent: intentHeaders}, false)
	}
}
