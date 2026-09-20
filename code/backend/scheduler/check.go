package scheduler

import (
	"context"
	"database/sql"
	"log"
	"sort"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/runner"
	"github.com/local/replicaro/storageavailability"
)

var dueJobs = database.DueJobs
var listPendingCatchUps = database.ListPendingCatchUps
var getScheduledJob = database.GetJob
var getScheduledRepository = database.GetRepository
var coordinateScheduledAdmission = database.CoordinateScheduledAdmission
var admitPersistedOperations = runner.AdmitPersistedOperations
var dueRepositoryChecks = database.DueRepositoryChecks
var dueRepositoryMaintenance = database.DueRepositoryMaintenance

const unavailableCatchUpInterval = 5 * time.Minute

func catchUpEligible(item database.PendingCatchUp, now time.Time) bool {
	if item.SourceAvailability != database.StorageUnavailable &&
		item.TargetAvailability != database.StorageUnavailable {
		// Busy and policy-not-ready pairs retain pending work but did not perform
		// an unavailable-storage attempt. Reconsider them on the next ordinary
		// scheduler tick instead of applying the storage recheck cadence.
		return true
	}
	latest := time.Time{}
	unavailableChecks := []string{}
	if item.SourceAvailability == database.StorageUnavailable {
		unavailableChecks = append(unavailableChecks, item.SourceCheckedAt)
	}
	if item.TargetAvailability == database.StorageUnavailable {
		unavailableChecks = append(unavailableChecks, item.TargetCheckedAt)
	}
	for _, value := range unavailableChecks {
		if parsed, err := time.Parse(time.RFC3339Nano, value); err == nil && parsed.After(latest) {
			latest = parsed
		}
	}
	return latest.IsZero() || !now.Before(latest.Add(unavailableCatchUpInterval))
}

func CheckDueJobs(db *sql.DB) {
	ctx := context.Background()
	coordinator := newRepositoryTaskCoordinator(db, ctx)
	CheckDueJobsContext(ctx, db, coordinator)
	_ = coordinator.stop(context.Background())
}

func CheckDueJobsContext(ctx context.Context, db *sql.DB, coordinator *repositoryTaskCoordinator) {
	if ctx.Err() != nil {
		return
	}
	jobs, err := dueJobs(db)

	if err != nil {
		log.Println("scheduler:", err)
		return
	}

	pending, err := listPendingCatchUps(db)
	if err != nil {
		log.Println("scheduler pending catch-up:", err)
		return
	}
	byID := make(map[string]models.BackupJob, len(jobs)+len(pending))
	dueAt := make(map[string]time.Time, len(jobs))
	pendingTargets := make(map[string]map[string]bool)
	selectionNow := time.Now().UTC()
	for _, job := range jobs {
		byID[job.ID] = job
		if parsed, parseErr := time.Parse(time.RFC3339Nano, job.NextRun); parseErr == nil {
			dueAt[job.ID] = parsed
		}
	}
	for _, item := range pending {
		if !catchUpEligible(item, selectionNow) {
			continue
		}
		if pendingTargets[item.JobID] == nil {
			pendingTargets[item.JobID] = map[string]bool{}
		}
		pendingTargets[item.JobID][item.RepositoryID] = true
		if _, exists := byID[item.JobID]; exists {
			continue
		}
		job, getErr := getScheduledJob(db, item.JobID)
		if getErr != nil {
			log.Println("scheduler pending job:", getErr)
			continue
		}
		byID[job.ID] = job
	}
	ids := make([]string, 0, len(byID))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		job := byID[id]
		repositories := make([]models.Repository, 0, len(job.Targets))
		observationFailed := false
		for _, target := range job.Targets {
			if dueAt[job.ID].IsZero() && !pendingTargets[job.ID][target.RepositoryID] {
				continue
			}
			repo, getErr := getScheduledRepository(db, target.RepositoryID)
			if getErr != nil {
				log.Println("scheduler repository observation:", getErr)
				observationFailed = true
				break
			}
			repositories = append(repositories, repo)
		}
		if observationFailed {
			continue
		}
		if len(repositories) == 0 {
			continue
		}
		source, targets := storageavailability.ObserveBackupSet(ctx, job, repositories)
		if ctx.Err() != nil {
			return
		}
		now := time.Now().UTC()
		request := database.ScheduledAdmissionRequest{
			JobID: job.ID, DueAt: dueAt[job.ID], Now: now, Source: source, Targets: targets,
		}
		results, admitErr := admitPersistedOperations(db, func() ([]database.TargetAdmissionResult, error) {
			return coordinateScheduledAdmission(db, request)
		})
		if admitErr != nil {
			log.Println("scheduler backup admission:", admitErr)
			continue
		}
		admitted := 0
		for _, result := range results {
			if result.OperationID != "" {
				admitted++
			}
		}
		if admitted != 0 {
			log.Printf("scheduler: admitted %d target(s) for %s", admitted, job.Name)
		}
	}

	checks, err := dueRepositoryChecks(db)
	if err != nil {
		log.Println("scheduler repository checks:", err)
	} else {
		for _, repo := range checks {
			if ctx.Err() != nil {
				return
			}
			log.Println("scheduler: checking repository:", repo.Name)
			coordinator.admit(repo, "check")
		}
	}

	maintenance, err := dueRepositoryMaintenance(db)
	if err != nil {
		log.Println("scheduler repository maintenance:", err)
	} else {
		for _, repo := range maintenance {
			if ctx.Err() != nil {
				return
			}
			log.Println("scheduler: maintaining repository:", repo.Name)
			coordinator.admit(repo, "maintenance")
		}
	}

	// Enforce log retention alongside the scheduling tick.
	settings, err := database.GetSettings(db)

	if err == nil {
		_ = database.PruneActivity(db, settings.LogRetentionDays)
	}
}
