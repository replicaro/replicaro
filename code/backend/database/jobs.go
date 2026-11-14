package database

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/schedulevalue"
)

var (
	ErrJobRunActive          = errors.New("backup job has queued or running target runs")
	ErrJobConnectionReserved = errors.New("backup job is reserved by a pending repository connection")
	// ErrJobDefinitionBusy is what every job admission and edit gets while a
	// deletion of the job holds its definition gate. The API shows this text to
	// the user (with the job_being_deleted code the UI translates).
	ErrJobDefinitionBusy  = errors.New("This job is being deleted.")
	ErrInvalidTargets     = errors.New("at least one unique existing repository target is required")
	ErrJobSourceImmutable = errors.New("backup job source cannot be changed after creation")
	ErrJobSourceChanged   = errors.New("backup job source changed during binding; retry the operation")
	ErrJobSourceUnbound   = errors.New("imported backup job source is not bound to this computer")
	// ErrFileSourceExclusions refuses saving a single-file job with folder
	// exclusion settings, which can never apply to one file.
	ErrFileSourceExclusions = errors.New("a backup job whose source is a single file can't have exclusions or skip-folder options")
	// ErrJobDefinitionChanged refuses an edit whose revision no longer matches
	// the stored definition. The API sends this text with the
	// job_definition_changed code, which the UI translates.
	ErrJobDefinitionChanged = errors.New("Job settings have changed since you opened the job. Your current changes were not saved. Please try again.")
)

type jobDefinitionGateKey struct {
	db    *sql.DB
	jobID string
}

var jobDefinitionGates sync.Map

func jobDefinitionGate(db *sql.DB, jobID string) *sync.RWMutex {
	key := jobDefinitionGateKey{db: db, jobID: jobID}
	gate, _ := jobDefinitionGates.LoadOrStore(key, &sync.RWMutex{})
	return gate.(*sync.RWMutex)
}

func acquireJobDefinitionUse(db *sql.DB, jobID string) (func(), error) {
	gate := jobDefinitionGate(db, jobID)
	if !gate.TryRLock() {
		return nil, ErrJobDefinitionBusy
	}
	return gate.RUnlock, nil
}

// JobDeletionKind is the operation kind of a job deletion. Its record carries
// the job ID, so the "anything else queued or running for this job" checks
// below leave it out; otherwise a deletion would wait for itself.
const JobDeletionKind = "delete_job"

// HoldJobDefinitionForDeletion takes the one local per-job definition gate for
// a job deletion and keeps it across waiting for the job's other work, exact
// native cleanup, ownership removal, and the final definition delete. It
// blocks until in-flight admissions (short read holds) finish. Once it is
// waiting or held, every new admission and edit of the job fails with
// ErrJobDefinitionBusy, which is how a pending deletion holds back new work:
// scheduled runs are skipped without an issue, user requests are refused. It
// is process-local; after a restart the interrupted deletion releases nothing
// because nothing is held.
func HoldJobDefinitionForDeletion(db *sql.DB, jobID string) func() {
	gate := jobDefinitionGate(db, jobID)
	gate.Lock()
	var once sync.Once
	return func() { once.Do(gate.Unlock) }
}

// JobHasOtherActiveWork reports whether anything other than a deletion of the
// job is queued or running for it. A job deletion polls this before taking any
// vault lock (see HoldJobDefinitionForDeletion).
func JobHasOtherActiveWork(db *sql.DB, jobID string) (bool, error) {
	var active int
	if err := db.QueryRow(`SELECT COUNT(*) FROM operations WHERE job_id = ? AND kind <> ? AND status IN ('queued', 'running')`,
		jobID, JobDeletionKind).Scan(&active); err != nil {
		return false, err
	}
	return active > 0, nil
}

func requireJobConnectionUnreserved(tx *sql.Tx, jobID string) error {
	var reserved int
	if err := tx.QueryRow(`SELECT COUNT(*)
		FROM repository_connection_job_name_reservations r
		JOIN repository_connection_intents c ON c.id=r.connection_id
		WHERE r.job_id=?`, jobID).Scan(&reserved); err != nil {
		return err
	}
	if reserved != 0 {
		return ErrJobConnectionReserved
	}
	return nil
}

func requireJobTargetConnectionsUnreserved(tx *sql.Tx, targets []models.BackupJobTarget) error {
	seen := map[string]bool{}
	for _, target := range targets {
		if target.RepositoryID == "" || seen[target.RepositoryID] {
			continue
		}
		seen[target.RepositoryID] = true
		var reserved int
		if err := tx.QueryRow(`SELECT COUNT(*)
			FROM repository_connection_name_reservations r
			JOIN repository_connection_intents c ON c.id=r.connection_id
			WHERE r.repository_id=?`, target.RepositoryID).Scan(&reserved); err != nil {
			return err
		}
		if reserved != 0 {
			return ErrRepositoryConnectionReserved
		}
	}
	return nil
}

func NextRunFrom(schedule string, from time.Time) string {
	switch schedule {
	case "manual", "":
		return ""
	case "hourly":
		return from.Add(time.Hour).Format(time.RFC3339)
	case "daily":
		return from.Add(24 * time.Hour).Format(time.RFC3339)
	case "weekly":
		return from.Add(7 * 24 * time.Hour).Format(time.RFC3339)
	case "monthly":
		return nextCalendarMonths(from, 1).Format(time.RFC3339Nano)
	}
	if minutes, valid := schedulevalue.CustomMinutes(schedule); valid {
		return from.Add(time.Duration(minutes) * time.Minute).Format(time.RFC3339)
	}
	if months, valid := schedulevalue.CustomMonths(schedule); valid {
		return nextCalendarMonths(from, months).Format(time.RFC3339Nano)
	}
	if next, valid := schedulevalue.NextCron(schedule, from); valid {
		return next.Format(time.RFC3339)
	}
	return ""
}

// nextJobRunFrom is NextRunFrom for backup job schedules. A job's daily,
// weekly, monthly and every-N-months schedules step on the local calendar, so
// they keep their local time of day (and a month step its day of the month)
// across daylight-saving changes. Hourly and custom-minute intervals stay
// fixed durations. Vault care keeps NextRunFrom's fixed intervals: Kopia
// Object Lock maintenance stores the interval as its native full interval,
// and its lock-extension margin assumes runs are never further apart than
// that.
func nextJobRunFrom(schedule string, from time.Time) string {
	days := func(count int) func(time.Time) time.Time {
		return func(reading time.Time) time.Time { return reading.AddDate(0, 0, count) }
	}
	months := func(count int) func(time.Time) time.Time {
		return func(reading time.Time) time.Time { return nextCalendarMonths(reading, count) }
	}
	switch schedule {
	case "daily":
		return schedulevalue.NextLocal(from, time.Local, days(1)).Format(time.RFC3339)
	case "weekly":
		return schedulevalue.NextLocal(from, time.Local, days(7)).Format(time.RFC3339)
	case "monthly":
		return schedulevalue.NextLocal(from, time.Local, months(1)).Format(time.RFC3339Nano)
	}
	if count, valid := schedulevalue.CustomMonths(schedule); valid {
		return schedulevalue.NextLocal(from, time.Local, months(count)).Format(time.RFC3339Nano)
	}
	return NextRunFrom(schedule, from)
}

// nextRunBeyondSchedule reports a saved next run that lies further ahead
// than one whole schedule period. Normal scheduling never produces one; it
// comes from a clock that jumped forward and back (or a time zone change), and
// left alone it would hold the job or vault task back until that date.
//
// Cron has no fixed period, so the saved run is flagged only when it lies
// after the second occurrence from now. Comparing with the first occurrence
// would treat a small backward clock step just after a run as a jump: the
// saved next run is then one occurrence ahead, and resetting it would bring
// back the slot that just ran and run it twice.
//
// Apart from cron, the saved run is measured against a whole period rather
// than against the next occurrence computed from now, because a run computed
// earlier can legitimately lie after that occurrence. A calendar job run
// computed in the first pass through a repeated autumn hour falls after one
// computed in the second pass, and month steps from the 30th and from the
// 31st both land on the last day of a 30-day month, so a step taken now can
// land almost a day earlier than one taken the day before.
func nextRunBeyondSchedule(schedule string, next, now time.Time) bool {
	if !next.After(now) {
		return false
	}
	if _, cronSchedule := schedulevalue.CronExpression(schedule); cronSchedule {
		first, valid := schedulevalue.NextCron(schedule, now)
		if !valid {
			return false
		}
		second, valid := schedulevalue.NextCron(schedule, first)
		return valid && next.After(second)
	}
	// Calendar schedules can run up to a daylight-saving shift longer than
	// their nominal period; the margin covers any shift in use. Fixed
	// intervals allow for small clock corrections and for the storage
	// recheck delay: a scheduled vault check or maintenance that finds its
	// storage unavailable is moved that far ahead, more than one period
	// for intervals under five minutes, and a smaller margin would pull it
	// straight back.
	const calendarMargin = 3 * time.Hour
	const fixedMargin = UnavailableStorageRecheckInterval
	var period, margin time.Duration
	switch schedule {
	case "hourly":
		period, margin = time.Hour, fixedMargin
	case "daily":
		period, margin = 24*time.Hour, calendarMargin
	case "weekly":
		period, margin = 7*24*time.Hour, calendarMargin
	case "monthly":
		period, margin = 31*24*time.Hour, calendarMargin
	default:
		if minutes, valid := schedulevalue.CustomMinutes(schedule); valid {
			period, margin = time.Duration(minutes)*time.Minute, fixedMargin
		} else if months, valid := schedulevalue.CustomMonths(schedule); valid {
			period, margin = time.Duration(months)*31*24*time.Hour, calendarMargin
		} else {
			return false
		}
	}
	// Compared this way so the longest custom intervals cannot overflow.
	ahead := next.Sub(now)
	return ahead > period && ahead-period > margin
}

func nextCalendarMonths(from time.Time, months int) time.Time {
	year, month, day := from.Date()
	nextMonthStart := time.Date(year, month+time.Month(months), 1, from.Hour(), from.Minute(), from.Second(), from.Nanosecond(), from.Location())
	lastDay := time.Date(nextMonthStart.Year(), nextMonthStart.Month()+1, 0, 0, 0, 0, 0, from.Location()).Day()
	if day > lastDay {
		day = lastDay
	}
	return time.Date(nextMonthStart.Year(), nextMonthStart.Month(), day, from.Hour(), from.Minute(), from.Second(), from.Nanosecond(), from.Location())
}

func ValidSchedule(schedule string) bool {
	return schedulevalue.Valid(schedule)
}

func ValidJobSchedule(schedule string) bool {
	return schedulevalue.ValidJob(schedule)
}

const jobColumns = `id, name, source, source_storage_version, source_storage_key,
		source_storage_json, source_binding_state, resolved_source_path, resolved_source_observed_at, schedule, enabled,
	COALESCE(next_run, ''), COALESCE(last_run, ''), retention,
	retention_hourly, retention_daily, retention_weekly, retention_monthly, retention_yearly,
	excludes, skip_nobackup_folders, skip_cachedir_folders, skip_marker_file_name, tag,
	before_script_path, before_script_must_succeed, after_script_path, after_script_must_succeed,
	engine_settings, portable_target_ids, source_kind`

type engineSettingsEnvelope struct {
	Version int                   `json:"version"`
	Engines models.EngineSettings `json:"engines"`
}

func encodeEngineSettings(settings models.EngineSettings) (string, error) {
	if settings == nil {
		settings = models.EngineSettings{}
	}
	data, err := json.Marshal(engineSettingsEnvelope{Version: 1, Engines: settings})
	if err != nil {
		return "", fmt.Errorf("encode engine settings: %w", err)
	}
	return string(data), nil
}

func decodeEngineSettings(value string) (models.EngineSettings, error) {
	if strings.TrimSpace(value) == "" {
		return nil, fmt.Errorf("engine settings envelope is empty")
	}
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	var envelope engineSettingsEnvelope
	if err := decoder.Decode(&envelope); err != nil {
		return nil, fmt.Errorf("malformed engine settings envelope: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("malformed engine settings envelope: trailing JSON")
		}
		return nil, fmt.Errorf("malformed engine settings envelope: trailing data: %w", err)
	}
	if envelope.Version != 1 {
		return nil, fmt.Errorf("unsupported engine settings version %d", envelope.Version)
	}
	if envelope.Engines == nil {
		return nil, fmt.Errorf("engine settings engines must be an object")
	}
	for engineID := range envelope.Engines {
		switch engineID {
		case "restic", "kopia":
		default:
			return nil, fmt.Errorf("unknown engine settings section %q", engineID)
		}
	}
	return envelope.Engines, nil
}

func scanJob(scan func(dest ...any) error) (models.BackupJob, error) {
	var j models.BackupJob
	var settingsJSON string
	var targetIDsJSON string
	var hourly, daily, weekly, monthly, yearly sql.NullInt64
	err := scan(&j.ID, &j.Name, &j.Source, &j.SourceStorageVersion, &j.SourceStorageKey,
		&j.SourceStorageDescriptorJSON, &j.SourceBindingState, &j.ResolvedSourcePath,
		&j.ResolvedSourceObservedAt, &j.Schedule, &j.Enabled, &j.NextRun, &j.LastRun,
		&j.Retention, &hourly, &daily, &weekly, &monthly, &yearly,
		&j.Excludes, &j.SkipNoBackupFolders, &j.SkipCacheDirFolders, &j.SkipMarkerFileName,
		&j.Tag, &j.BeforeScriptPath, &j.BeforeScriptMustSucceed,
		&j.AfterScriptPath, &j.AfterScriptMustSucceed, &settingsJSON, &targetIDsJSON, &j.SourceKind)
	if err == nil {
		j.RetentionHourly = nullableRetentionValue(hourly)
		j.RetentionDaily = nullableRetentionValue(daily)
		j.RetentionWeekly = nullableRetentionValue(weekly)
		j.RetentionMonthly = nullableRetentionValue(monthly)
		j.RetentionYearly = nullableRetentionValue(yearly)
		if bindingErr := validateJobSourceBinding(j); bindingErr != nil {
			return j, bindingErr
		}
		decoded, decodeErr := decodeEngineSettings(settingsJSON)
		if decodeErr != nil {
			return j, decodeErr
		}
		j.EngineSettings = decoded
		if decodeErr := json.Unmarshal([]byte(targetIDsJSON), &j.PortableTargetIDs); decodeErr != nil {
			return j, fmt.Errorf("malformed portable target IDs: %w", decodeErr)
		}
	}
	return j, err
}

func nullableRetentionValue(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}
	converted := int(value.Int64)
	return &converted
}

func populateJobTargets(db *sql.DB, job *models.BackupJob) error {
	rows, err := db.Query(`
		SELECT t.repository_id, r.name, r.engine, COALESCE(t.last_run, ''), t.last_status,
			t.logical_size_bytes, t.logical_size_measured_at,
			COALESCE((SELECT o.id FROM operations o
				WHERE o.job_id = t.job_id AND o.repository_id = t.repository_id
				  AND o.status IN ('queued', 'running')
				ORDER BY o.started_at DESC, o.id DESC LIMIT 1), ''),
			-- last_status has no qualifier of its own. FinishBackupOperationWithIssueKind
			-- sets it in the same transaction as the operation's status and issue_kind,
			-- so the qualifier is read from the target's newest finished backup
			-- operation by finished_at. If the clock moved backwards between two runs,
			-- the previous run's qualifier can show until the next backup finishes.
			-- An older build, or a pruned operation, leaves '' (plain result).
			CASE WHEN t.last_status = 'completed_with_issues' THEN COALESCE((SELECT o.issue_kind FROM operations o
				WHERE o.job_id = t.job_id AND o.repository_id = t.repository_id
				  AND o.kind = 'backup' AND o.status NOT IN ('queued', 'running')
				ORDER BY o.finished_at DESC, o.started_at DESC, o.id DESC LIMIT 1), '') ELSE '' END,
			s.pending_catchup, s.coalesced_missed_count, s.first_deferred_due_at, s.last_due_at,
			s.source_availability, s.source_reason_code, s.source_checked_at,
			s.target_availability, s.target_reason_code, s.target_checked_at,
			CASE
				WHEN r.engine <> 'kopia' THEN 'ready'
				WHEN k.state='ready' AND k.desired_digest=k.applied_digest THEN 'ready'
				WHEN k.state='error' THEN 'error'
				ELSE 'pending'
			END,
			CASE WHEN r.engine='kopia' THEN COALESCE(k.last_error,'') ELSE '' END
		FROM backup_job_targets t JOIN repositories r ON r.id = t.repository_id
		JOIN job_target_schedule_state s
			ON s.job_id=t.job_id AND s.repository_id=t.repository_id
		LEFT JOIN kopia_policy_state k ON k.repository_id=t.repository_id
		WHERE t.job_id = ? ORDER BY r.name, t.repository_id`, job.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	job.Targets = []models.BackupJobTarget{}
	for rows.Next() {
		var target models.BackupJobTarget
		if err := rows.Scan(&target.RepositoryID, &target.RepositoryName, &target.Engine,
			&target.LastRun, &target.LastStatus, &target.LogicalSizeBytes, &target.LogicalSizeMeasuredAt, &target.OperationID,
			&target.LastIssueKind, &target.PendingCatchUp, &target.CoalescedMissedCount,
			&target.FirstDeferredDueAt, &target.LastDueAt,
			&target.SourceAvailability, &target.SourceAvailabilityReason,
			&target.SourceAvailabilityCheck, &target.TargetAvailability,
			&target.TargetAvailabilityReason, &target.TargetAvailabilityCheck,
			&target.PolicyStatus, &target.PolicyError); err != nil {
			return err
		}
		job.Targets = append(job.Targets, target)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	job.UnattachedTargets = job.HasUnattachedTargets()
	selectPresentedJobSize(job)
	return nil
}

func JobIDsForRepository(db *sql.DB, repositoryID string) (map[string]bool, error) {
	rows, err := db.Query(`SELECT job_id FROM backup_job_targets WHERE repository_id=?`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result[id] = true
	}
	return result, rows.Err()
}

func selectPresentedJobSize(job *models.BackupJob) {
	job.SizeBytes, job.SizeMeasuredAt = nil, ""
	priority := map[string]int{"restic": 0, "kopia": 1}
	var selected *models.BackupJobTarget
	var selectedAt time.Time
	for index := range job.Targets {
		candidate := &job.Targets[index]
		if candidate.LogicalSizeBytes == nil || candidate.LogicalSizeMeasuredAt == "" {
			continue
		}
		candidateAt, err := time.Parse(time.RFC3339Nano, candidate.LogicalSizeMeasuredAt)
		if err != nil {
			continue
		}
		if selected == nil || candidateAt.After(selectedAt) ||
			(candidateAt.Equal(selectedAt) &&
				(priority[candidate.Engine] < priority[selected.Engine] ||
					(priority[candidate.Engine] == priority[selected.Engine] && candidate.RepositoryID < selected.RepositoryID))) {
			selected = candidate
			selectedAt = candidateAt
		}
	}
	if selected != nil {
		value := *selected.LogicalSizeBytes
		job.SizeBytes = &value
		job.SizeMeasuredAt = selected.LogicalSizeMeasuredAt
	}
}

func RecordJobTargetLogicalSize(db *sql.DB, jobID, repositoryID string, sizeBytes int64, measuredAt time.Time) error {
	if sizeBytes < 0 {
		return fmt.Errorf("job logical size cannot be negative")
	}
	result, err := db.Exec(`UPDATE backup_job_targets
		SET logical_size_bytes=?, logical_size_measured_at=? WHERE job_id=? AND repository_id=?`,
		sizeBytes, measuredAt.UTC().Format(time.RFC3339Nano), jobID, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
}

func ListJobs(db *sql.DB) ([]models.BackupJob, error) {
	rows, err := db.Query(`SELECT ` + jobColumns + ` FROM backup_jobs ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	jobs := []models.BackupJob{}
	for rows.Next() {
		job, err := scanJob(rows.Scan)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for index := range jobs {
		if err := populateJobTargets(db, &jobs[index]); err != nil {
			return nil, err
		}
		jobs[index].Revision = jobDefinitionRevision(jobs[index])
	}
	return jobs, nil
}

func ListJobsForRepository(db *sql.DB, repositoryID string) ([]models.BackupJob, error) {
	jobs, err := ListJobs(db)
	if err != nil {
		return nil, err
	}
	result := []models.BackupJob{}
	for _, job := range jobs {
		for _, target := range job.Targets {
			if target.RepositoryID == repositoryID {
				result = append(result, job)
				break
			}
		}
	}
	return result, nil
}

func GetJob(db *sql.DB, id string) (models.BackupJob, error) {
	job, err := scanJob(db.QueryRow(`SELECT `+jobColumns+` FROM backup_jobs WHERE id = ?`, id).Scan)
	if err != nil {
		return job, err
	}
	if err := populateJobTargets(db, &job); err != nil {
		return job, err
	}
	job.Revision = jobDefinitionRevision(job)
	return job, nil
}

// jobDefinitionRevision digests the parts of a stored job that a job edit
// rewrites, so an edit can show it started from the definition that is
// stored now. A job edit sends back every field, so an edit made from an
// outdated copy (a tab left open, or a copy read before profile sync applied
// another computer's change) would put back old values. A lower retention
// from such a copy makes the next native forget delete snapshots, and an old
// target list silently drops a vault. Comparing this digest refuses that edit
// instead.
//
// Included: name, source, schedule, the six retention values, excludes, the
// three marker-file choices, tag,
// both script paths and must-succeed flags, engine settings, the target vault
// IDs, and the portable target IDs (profile sync merges other computers'
// vaults into these, and connecting a vault adds a target and replaces engine
// settings).
//
// Left out on purpose, because they change without anyone editing the job and
// would refuse edits for no reason: enabled (it has its own endpoint, and an
// edit always keeps the stored value), next and last run, the local source
// binding facts and binding state (probes record and convert them, and an
// edit always keeps the stored binding), the source alias and its
// observation time, and everything per target other than its vault ID (run
// status, schedule state, availability, policy status, sizes, vault name and
// engine).
//
// The digest is computed when a job is loaded and is never stored, so it
// needs no schema change. Don't add a field here without checking that only
// an edit or profile sync changes it.
func jobDefinitionRevision(job models.BackupJob) string {
	targets := make([]string, 0, len(job.Targets))
	for _, target := range job.Targets {
		targets = append(targets, target.RepositoryID)
	}
	slices.Sort(targets)
	portable := append([]string(nil), job.PortableTargetIDs...)
	slices.Sort(portable)
	engineSettings := job.EngineSettings
	if engineSettings == nil {
		engineSettings = models.EngineSettings{}
	}
	// A struct keeps the field order fixed, and encoding/json writes map keys
	// in sorted order, so the same definition always encodes the same way.
	encoded, err := json.Marshal(struct {
		Name                    string                `json:"name"`
		Source                  string                `json:"source"`
		Schedule                string                `json:"schedule"`
		Retention               int                   `json:"retention"`
		RetentionHourly         *int                  `json:"retentionHourly"`
		RetentionDaily          *int                  `json:"retentionDaily"`
		RetentionWeekly         *int                  `json:"retentionWeekly"`
		RetentionMonthly        *int                  `json:"retentionMonthly"`
		RetentionYearly         *int                  `json:"retentionYearly"`
		Excludes                string                `json:"excludes"`
		SkipNoBackupFolders     bool                  `json:"skipNoBackupFolders"`
		SkipCacheDirFolders     bool                  `json:"skipCacheDirFolders"`
		SkipMarkerFileName      string                `json:"skipMarkerFileName"`
		Tag                     string                `json:"tag"`
		BeforeScriptPath        string                `json:"beforeScriptPath"`
		BeforeScriptMustSucceed bool                  `json:"beforeScriptMustSucceed"`
		AfterScriptPath         string                `json:"afterScriptPath"`
		AfterScriptMustSucceed  bool                  `json:"afterScriptMustSucceed"`
		EngineSettings          models.EngineSettings `json:"engineSettings"`
		Targets                 []string              `json:"targets"`
		PortableTargets         []string              `json:"portableTargets"`
	}{
		job.Name, job.Source, job.Schedule, job.Retention,
		job.RetentionHourly, job.RetentionDaily, job.RetentionWeekly, job.RetentionMonthly, job.RetentionYearly,
		job.Excludes, job.SkipNoBackupFolders, job.SkipCacheDirFolders, job.SkipMarkerFileName,
		job.Tag, job.BeforeScriptPath, job.BeforeScriptMustSucceed,
		job.AfterScriptPath, job.AfterScriptMustSucceed, engineSettings, targets, portable,
	})
	if err != nil {
		// Every field above always encodes; an empty revision never matches.
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func validateTargets(tx *sql.Tx, targets []models.BackupJobTarget) error {
	if len(targets) == 0 {
		return ErrInvalidTargets
	}
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		if target.RepositoryID == "" || seen[target.RepositoryID] {
			return ErrInvalidTargets
		}
		seen[target.RepositoryID] = true
		var count int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM repositories WHERE id = ?`, target.RepositoryID).Scan(&count); err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("repository target does not exist: %s", target.RepositoryID)
		}
	}
	return nil
}

func CreateJob(db *sql.DB, job models.BackupJob) (string, error) {
	job.Name = strings.TrimSpace(job.Name)
	var validSchedule bool
	job.Schedule, validSchedule = schedulevalue.NormalizeJob(job.Schedule)
	if !validSchedule {
		return "", fmt.Errorf("invalid backup schedule")
	}
	if err := models.ValidateRetentionPolicy(job); err != nil {
		return "", err
	}
	if err := models.ValidateSkipMarkerFileName(job.SkipMarkerFileName); err != nil {
		return "", err
	}
	if err := validateJobSourceBinding(job); err != nil {
		return "", err
	}
	if err := validateJobSourceKind(job); err != nil {
		return "", err
	}
	if err := validateFileSourceOptions(job); err != nil {
		return "", err
	}
	if job.IsFileSource() {
		job.Excludes = ""
	}
	tx, err := db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if err := ensureUniqueJobName(tx, job.Name, ""); err != nil {
		return "", err
	}
	id := job.ID
	// A job restored with its original UUID (a dormant recovery definition)
	// is already carried by snapshots in its vaults; a server-assigned UUID
	// cannot be, so only the former changes any File History grouping.
	restoredID := id != ""
	if id == "" {
		id = uuid.New().String()
	}
	job.ID = id
	if err := validateTargets(tx, job.Targets); err != nil {
		return "", err
	}
	if err := requireJobTargetConnectionsUnreserved(tx, job.Targets); err != nil {
		return "", err
	}
	settingsJSON, err := encodeEngineSettings(job.EngineSettings)
	if err != nil {
		return "", err
	}
	if err := requireJobConnectionUnreserved(tx, id); err != nil {
		return "", err
	}
	portableTargetIDs := make([]string, 0, len(job.Targets))
	for _, target := range job.Targets {
		portableTargetIDs = append(portableTargetIDs, target.RepositoryID)
	}
	portableJSON, err := json.Marshal(portableTargetIDs)
	if err != nil {
		return "", err
	}
	_, err = tx.Exec(`INSERT INTO backup_jobs
			(id, name, source, source_storage_version, source_storage_key, source_storage_json,
			source_binding_state, schedule, enabled, next_run, last_run, retention,
		retention_hourly, retention_daily, retention_weekly, retention_monthly, retention_yearly,
		excludes, skip_nobackup_folders, skip_cachedir_folders, skip_marker_file_name,
		tag, before_script_path, before_script_must_succeed,
		after_script_path, after_script_must_succeed, engine_settings, portable_target_ids, source_kind)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, job.Name, job.Source, job.SourceStorageVersion, job.SourceStorageKey,
		job.SourceStorageDescriptorJSON, func() string {
			if job.SourceBindingState == "" {
				return "bound"
			}
			return job.SourceBindingState
		}(), job.Schedule, job.Enabled,
		nextJobRunFrom(job.Schedule, time.Now()), job.Retention,
		job.RetentionHourly, job.RetentionDaily, job.RetentionWeekly, job.RetentionMonthly, job.RetentionYearly,
		job.Excludes, job.SkipNoBackupFolders, job.SkipCacheDirFolders, job.SkipMarkerFileName,
		job.Tag, job.BeforeScriptPath, job.BeforeScriptMustSucceed,
		job.AfterScriptPath, job.AfterScriptMustSucceed, string(settingsJSON), string(portableJSON), job.SourceKind)
	if err != nil {
		return "", err
	}
	for _, target := range job.Targets {
		if _, err := tx.Exec(`INSERT INTO backup_job_targets (job_id, repository_id) VALUES (?, ?)`, id, target.RepositoryID); err != nil {
			return "", err
		}
		// Only a restored or supplied ID can already have snapshots in this
		// vault; a freshly generated one keeps Kopia's normal first backup.
		if restoredID {
			if err := markJobTargetFullSourceRead(tx, id, target.RepositoryID); err != nil {
				return "", err
			}
		}
	}
	targetIDs := make([]string, 0, len(job.Targets))
	for _, target := range job.Targets {
		targetIDs = append(targetIDs, target.RepositoryID)
	}
	if _, err := RefreshKopiaPolicyStatesTx(tx, targetIDs); err != nil {
		return "", err
	}
	if err := markVaultProfilesDirty(tx, targetIDs); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO activity_log (timestamp, level, message) VALUES (datetime('now'), 'INFO', ?)`, "Backup job created: "+job.Name); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if restoredID {
		RefreshStaleMetadataGrouping(db, targetIDs)
	}
	return id, nil
}

// UpdateJob saves a job definition as given, including its enabled state,
// without checking which revision the caller started from. Job edits from
// the API use UpdateJobAtRevision.
func UpdateJob(db *sql.DB, job models.BackupJob) error {
	return updateJob(db, job, false, "")
}

// UpdateJobAtRevision saves a job edit only when the stored definition still
// has the revision the caller read; otherwise it changes nothing and returns
// ErrJobDefinitionChanged. An empty revision skips that check (see the job
// edit handler). The edit always keeps the stored enabled state, which only
// the enabled-state endpoint changes.
func UpdateJobAtRevision(db *sql.DB, job models.BackupJob, revision string) error {
	return updateJob(db, job, true, revision)
}

func updateJob(db *sql.DB, job models.BackupJob, definitionEdit bool, expectedRevision string) error {
	releaseDefinition, err := acquireJobDefinitionUse(db, job.ID)
	if err != nil {
		return err
	}
	defer releaseDefinition()
	job.Name = strings.TrimSpace(job.Name)
	var validSchedule bool
	job.Schedule, validSchedule = schedulevalue.NormalizeJob(job.Schedule)
	if !validSchedule {
		return fmt.Errorf("invalid backup schedule")
	}
	if err := models.ValidateRetentionPolicy(job); err != nil {
		return err
	}
	if err := models.ValidateSkipMarkerFileName(job.SkipMarkerFileName); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireJobConnectionUnreserved(tx, job.ID); err != nil {
		return err
	}
	if err := ensureUniqueJobName(tx, job.Name, job.ID); err != nil {
		return err
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE job_id = ? AND status IN ('queued', 'running')`, job.ID).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrJobRunActive
	}
	existingJob, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM backup_jobs WHERE id=?`, job.ID).Scan)
	if err != nil {
		return err
	}
	oldRows, err := tx.Query(`SELECT repository_id FROM backup_job_targets WHERE job_id = ?`, job.ID)
	if err != nil {
		return err
	}
	affected := []string{}
	oldActiveTargets := map[string]bool{}
	for oldRows.Next() {
		var repositoryID string
		if err := oldRows.Scan(&repositoryID); err != nil {
			_ = oldRows.Close()
			return err
		}
		affected = append(affected, repositoryID)
		oldActiveTargets[repositoryID] = true
		existingJob.Targets = append(existingJob.Targets, models.BackupJobTarget{RepositoryID: repositoryID})
	}
	if err := closeRows(oldRows); err != nil {
		return err
	}
	// The revision is compared here, inside the write transaction, rather
	// than only in the handler: the database has one writer connection, so
	// no other job edit, enable change or profile sync apply can land between
	// this read and the commit below. It comes before the target and
	// definition checks below so an outdated copy gets the reload message
	// rather than an error about a field that only differs because the copy
	// is old. The handler already compared the revision before the name and
	// running-operation checks above.
	if expectedRevision != "" && expectedRevision != jobDefinitionRevision(existingJob) {
		return ErrJobDefinitionChanged
	}
	if definitionEdit {
		// The caller's copy of enabled can be older than this transaction:
		// the enabled-state endpoint may have committed since it was read.
		job.Enabled = existingJob.Enabled
	}
	if err := validateJobSourceTransition(existingJob, job, false); err != nil {
		return err
	}
	// The kind is part of the source, which a definition edit never changes,
	// so the stored kind is what this edit is checked against (the UPDATE
	// below doesn't write it).
	job.SourceKind = existingJob.SourceKind
	if err := validateFileSourceOptions(job); err != nil {
		return err
	}
	if job.IsFileSource() {
		job.Excludes = ""
	}
	if normalizedJobSourceBindingState(existingJob.SourceBindingState) == "bound" {
		// A definition update never changes the local binding. Writing the
		// stored value (rather than the caller's copy) keeps a binding that
		// was converted after the caller read the job.
		job.SourceStorageVersion = existingJob.SourceStorageVersion
		job.SourceStorageKey = existingJob.SourceStorageKey
		job.SourceStorageDescriptorJSON = existingJob.SourceStorageDescriptorJSON
	}
	if err := validateJobSourceBinding(job); err != nil {
		return err
	}
	var existingPortableJSON, existingSchedule, existingNextRun string
	var existingEnabled bool
	if err := tx.QueryRow(`SELECT portable_target_ids,schedule,enabled,COALESCE(next_run,'')
		FROM backup_jobs WHERE id = ?`, job.ID).Scan(
		&existingPortableJSON, &existingSchedule, &existingEnabled, &existingNextRun); err != nil {
		return err
	}
	if err := validateTargets(tx, job.Targets); err != nil {
		return err
	}
	if err := requireJobTargetConnectionsUnreserved(tx, job.Targets); err != nil {
		return err
	}
	settingsJSON, err := encodeEngineSettings(job.EngineSettings)
	if err != nil {
		return err
	}
	portableTargetIDs := []string{}
	if err := json.Unmarshal([]byte(existingPortableJSON), &portableTargetIDs); err != nil {
		return fmt.Errorf("decode portable target identities: %w", err)
	}
	portableTargetIDs = reconcilePortableTargetIDs(portableTargetIDs, oldActiveTargets, job.Targets)
	portableJSON, err := json.Marshal(portableTargetIDs)
	if err != nil {
		return err
	}
	job.PortableTargetIDs = portableTargetIDs
	if sameSavedJobDefinition(existingJob, job) {
		return tx.Commit()
	}
	nextRun := schedulerTransition(existingSchedule, existingEnabled, existingNextRun,
		job.Schedule, job.Enabled, time.Now())
	result, err := tx.Exec(`UPDATE backup_jobs SET name = ?, source = ?,
			source_storage_version = ?, source_storage_key = ?, source_storage_json = ?,
			source_binding_state = ?, schedule = ?,
		retention = ?, retention_hourly = ?, retention_daily = ?, retention_weekly = ?,
		retention_monthly = ?, retention_yearly = ?, excludes = ?,
		skip_nobackup_folders = ?, skip_cachedir_folders = ?, skip_marker_file_name = ?,
		tag = ?, enabled = ?, next_run = ?,
		before_script_path = ?, before_script_must_succeed = ?,
		after_script_path = ?, after_script_must_succeed = ?,
			engine_settings = ?, portable_target_ids = ? WHERE id = ?`,
		job.Name, job.Source, job.SourceStorageVersion, job.SourceStorageKey,
		job.SourceStorageDescriptorJSON, func() string {
			if job.SourceBindingState == "" {
				return "bound"
			}
			return job.SourceBindingState
		}(), job.Schedule, job.Retention,
		job.RetentionHourly, job.RetentionDaily, job.RetentionWeekly, job.RetentionMonthly, job.RetentionYearly,
		job.Excludes, job.SkipNoBackupFolders, job.SkipCacheDirFolders, job.SkipMarkerFileName,
		job.Tag, job.Enabled, nextRun, job.BeforeScriptPath, job.BeforeScriptMustSucceed,
		job.AfterScriptPath, job.AfterScriptMustSucceed,
		string(settingsJSON), string(portableJSON),
		job.ID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return sql.ErrNoRows
	}
	dormantRepositories, err := updateDormantDefinitionsForJob(tx, job)
	if err != nil {
		return err
	}
	affected = append(affected, dormantRepositories...)
	ids := make([]string, 0, len(job.Targets))
	args := []any{job.ID}
	for _, target := range job.Targets {
		affected = append(affected, target.RepositoryID)
		ids = append(ids, "?")
		args = append(args, target.RepositoryID)
	}
	if _, err := tx.Exec(`UPDATE backup_job_targets
		SET logical_size_bytes=NULL, logical_size_measured_at='' WHERE job_id=?`, job.ID); err != nil {
		return err
	}
	if existingSchedule != "manual" && job.Schedule == "manual" {
		if _, err := tx.Exec(`UPDATE job_target_schedule_state
			SET pending_catchup=0,coalesced_missed_count=0,first_deferred_due_at='',
			    last_due_at='',updated_at=?
			WHERE job_id=?`, time.Now().UTC().Format(time.RFC3339Nano), job.ID); err != nil {
			return err
		}
	}
	if err := markVaultProfilesDirty(tx, affected); err != nil {
		return err
	}
	query := `DELETE FROM backup_job_targets WHERE job_id = ? AND repository_id NOT IN (` + strings.Join(ids, ",") + `)`
	if _, err := tx.Exec(query, args...); err != nil {
		return err
	}
	// Removed targets take their scheduled restorations with them (ON DELETE
	// CASCADE); drop any never-started admission group that leaves empty, as
	// vault removal does.
	if _, err := tx.Exec(deleteEmptyUnstartedAdmissionGroups+` AND job_id=?`, job.ID); err != nil {
		return err
	}
	for _, target := range job.Targets {
		inserted, err := tx.Exec(`INSERT INTO backup_job_targets (job_id, repository_id) VALUES (?, ?)
			ON CONFLICT(job_id, repository_id) DO NOTHING`, job.ID, target.RepositoryID)
		if err != nil {
			return err
		}
		// A vault newly added to this job may be one it used before, still
		// holding its snapshots from another folder. Targets the job already
		// had hit the conflict, insert nothing, and keep their flag.
		count, err := inserted.RowsAffected()
		if err != nil {
			return err
		}
		if count == 1 {
			if err := markJobTargetFullSourceRead(tx, job.ID, target.RepositoryID); err != nil {
				return err
			}
		}
	}
	if _, err := RefreshKopiaPolicyStatesTx(tx, affected); err != nil {
		return err
	}
	// File History groups a vault's Restic snapshots of a job under the job's
	// source only while the job targets that vault, so a vault joining or
	// leaving the job, or an unbound imported job's source changing, can move
	// snapshots between grouping roots in exactly those vaults.
	groupingChanged := []string{}
	newActiveTargets := make(map[string]bool, len(job.Targets))
	for _, target := range job.Targets {
		newActiveTargets[target.RepositoryID] = true
		if existingJob.Source != job.Source || !oldActiveTargets[target.RepositoryID] {
			groupingChanged = append(groupingChanged, target.RepositoryID)
		}
	}
	for repositoryID := range oldActiveTargets {
		if !newActiveTargets[repositoryID] {
			groupingChanged = append(groupingChanged, repositoryID)
		}
	}
	if _, err := tx.Exec(`INSERT INTO activity_log (timestamp, level, message) VALUES (datetime('now'), 'INFO', ?)`, "Backup job updated: "+job.Name); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	RefreshStaleMetadataGrouping(db, groupingChanged)
	return nil
}

// Imported definitions have no local source binding yet, so backend workflows
// can still update their source; normalization is one use case, not the limit.
// Binding freezes the source and identity, even if the job is later disabled.
// Protect deliberately keeps existing-job sources read-only: replacing a source
// under the same job ID can change native retention scope. That UI restriction
// must not turn into premature immutability in the backend.
func validateJobSourceTransition(existing, next models.BackupJob, allowFirstBinding bool) error {
	existingState := normalizedJobSourceBindingState(existing.SourceBindingState)
	nextState := normalizedJobSourceBindingState(next.SourceBindingState)
	if existingState == "unbound_imported" {
		if nextState == "unbound_imported" || nextState == "bound" && allowFirstBinding {
			return nil
		}
		return ErrJobSourceImmutable
	}
	if existing.Source != next.Source {
		return ErrJobSourceImmutable
	}
	// The binding is immutable after first binding with two exceptions, and
	// neither goes through this function: the one-time conversion of a legacy
	// binding by a successful probe (convertLegacySourceBindingTx) and a user's
	// "Update job source" (UpdateJobSourceAlias), which records the alias's
	// facts. A caller that read the row before a legacy conversion still
	// passes, and UpdateJob keeps the stored value. Do not widen this check to
	// accept other binding changes: definition edits must never move the
	// location a job reads from.
	if existingState != nextState || !equivalentSourceBinding(
		existing.SourceStorageVersion, existing.SourceStorageKey, existing.SourceStorageDescriptorJSON,
		next.SourceStorageVersion, next.SourceStorageKey, next.SourceStorageDescriptorJSON) {
		return ErrJobSourceImmutable
	}
	return nil
}

// validateJobSourceKind checks a kind about to be stored. "unknown" only
// describes a recovered or imported job that hasn't been bound here yet.
func validateJobSourceKind(job models.BackupJob) error {
	switch job.SourceKind {
	case "", models.SourceKindFile:
		return nil
	case models.SourceKindUnknown:
		if normalizedJobSourceBindingState(job.SourceBindingState) == "unbound_imported" {
			return nil
		}
		return fmt.Errorf("a bound backup job source must have a known kind")
	default:
		return fmt.Errorf("unsupported backup job source kind")
	}
}

// validateJobSourceKindTransition is the kind's counterpart of
// validateJobSourceTransition. Whether a source is a folder or a file is
// fixed with the source itself: it changes only once, when a recovered or
// imported job of unknown kind is first bound on this computer and the
// probe has seen what the path is.
func validateJobSourceKindTransition(existing, next models.BackupJob, allowFirstBinding bool) error {
	if err := validateJobSourceKind(next); err != nil {
		return err
	}
	if existing.SourceKind == next.SourceKind {
		return nil
	}
	if existing.SourceKind == models.SourceKindUnknown && allowFirstBinding &&
		normalizedJobSourceBindingState(existing.SourceBindingState) == "unbound_imported" &&
		normalizedJobSourceBindingState(next.SourceBindingState) == "bound" {
		return nil
	}
	return ErrJobSourceImmutable
}

// recordFirstBindingSourceKind stores the kind a first binding found, when it
// differs from the recorded one. Callers run validateJobSourceKindTransition
// first, so the only change that gets here is an adopted import of unknown
// kind settling as a folder ("") or a file, in the same transaction that
// saves its binding. It reports whether the kind changed.
//
// When an unknown import turns out to be a file, its exclusion and
// marker-file settings are cleared in the same statement. Adopted imports are
// created without any, so they can only be there if the user edited the job
// before enabling it; on one file they never had an effect, a file job is
// never saved with them, and the Kopia job policy would otherwise keep their
// ignore rules. The callers republish the vault profiles, rewrite the dormant
// copies from the saved row and refresh the Kopia policy states.
//
// This statement is separate from the binding's UPDATE on purpose: a job whose
// recorded kind is already known (every folder job) never runs it, so its
// first binding writes the binding and nothing else.
func recordFirstBindingSourceKind(tx *sql.Tx, jobID, recordedKind, foundKind string) (bool, error) {
	if recordedKind == foundKind {
		return false, nil
	}
	if recordedKind != models.SourceKindUnknown {
		return false, ErrJobSourceImmutable
	}
	query := `UPDATE backup_jobs SET source_kind=? WHERE id=? AND source_kind='unknown'`
	if foundKind == models.SourceKindFile {
		query = `UPDATE backup_jobs SET source_kind=?, excludes='', skip_nobackup_folders=0,
			skip_cachedir_folders=0, skip_marker_file_name='' WHERE id=? AND source_kind='unknown'`
	}
	result, err := tx.Exec(query, foundKind, jobID)
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return false, ErrJobSourceImmutable
	}
	return true, nil
}

// validateFileSourceOptions refuses folder exclusion settings on a file
// source. It runs where a definition is saved (job creation and edits), not
// when a row is read: a stored job must always load, so it can be shown,
// corrected or deleted.
//
// Recovery doesn't call it. A recovered file job that carries these settings
// can only come from a vault profile written by something other than these
// save paths, and refusing it would block recovering the whole vault, so
// attaching stores the job row, and any dormant copy, exactly as recovered.
// Every later save of that job comes through here: a save that keeps the
// settings is refused and one that clears them is accepted. Restoring a
// dormant copy saves the job with CreateJob when no local job exists and with
// UpdateJob when one does, so it is such a save too.
func validateFileSourceOptions(job models.BackupJob) error {
	if !job.IsFileSource() {
		return nil
	}
	if job.SkipNoBackupFolders || job.SkipCacheDirFolders || job.SkipMarkerFileName != "" {
		return ErrFileSourceExclusions
	}
	// Exclusions are one pattern per line. A line of only whitespace is
	// accepted here as no exclusion, but it is not harmless if stored: Restic
	// is given every non-empty line as an --exclude pattern. That's why the
	// callers store a file job's exclusions as "" once this check passes.
	for _, line := range strings.Split(job.Excludes, "\n") {
		if strings.TrimSpace(line) != "" {
			return ErrFileSourceExclusions
		}
	}
	return nil
}

func sameSavedJobDefinition(left, right models.BackupJob) bool {
	targetIDs := func(targets []models.BackupJobTarget) []string {
		values := make([]string, 0, len(targets))
		for _, target := range targets {
			values = append(values, target.RepositoryID)
		}
		slices.Sort(values)
		return values
	}
	leftPortable, rightPortable := append([]string(nil), left.PortableTargetIDs...), append([]string(nil), right.PortableTargetIDs...)
	slices.Sort(leftPortable)
	slices.Sort(rightPortable)
	return left.Name == right.Name && left.Source == right.Source &&
		left.SourceStorageVersion == right.SourceStorageVersion && left.SourceStorageKey == right.SourceStorageKey &&
		left.SourceStorageDescriptorJSON == right.SourceStorageDescriptorJSON &&
		(left.SourceBindingState == right.SourceBindingState || left.SourceBindingState == "" && right.SourceBindingState == "bound" || left.SourceBindingState == "bound" && right.SourceBindingState == "") && left.Schedule == right.Schedule &&
		left.Enabled == right.Enabled && models.RetentionPolicyEqual(left, right) && left.Excludes == right.Excludes &&
		left.SkipNoBackupFolders == right.SkipNoBackupFolders && left.SkipCacheDirFolders == right.SkipCacheDirFolders &&
		left.SkipMarkerFileName == right.SkipMarkerFileName &&
		left.Tag == right.Tag && left.BeforeScriptPath == right.BeforeScriptPath &&
		left.BeforeScriptMustSucceed == right.BeforeScriptMustSucceed && left.AfterScriptPath == right.AfterScriptPath &&
		left.AfterScriptMustSucceed == right.AfterScriptMustSucceed && semanticEngineSettingsEqual(left.EngineSettings, right.EngineSettings) &&
		slices.Equal(targetIDs(left.Targets), targetIDs(right.Targets)) && slices.Equal(leftPortable, rightPortable)
}

func SavedJobDefinitionEqual(left, right models.BackupJob) bool {
	return sameSavedJobDefinition(left, right)
}

// ProfileRepositoryIDsForJob returns every local profile whose bytes contain
// the active or dormant definition for this job.
func ProfileRepositoryIDsForJob(db *sql.DB, jobID string) ([]string, error) {
	rows, err := db.Query(`SELECT repository_id FROM backup_job_targets WHERE job_id=?
		UNION SELECT repository_id FROM dormant_recovery_jobs WHERE job_id=? ORDER BY repository_id`, jobID, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []string{}
	for rows.Next() {
		var repositoryID string
		if err := rows.Scan(&repositoryID); err != nil {
			return nil, err
		}
		result = append(result, repositoryID)
	}
	return result, rows.Err()
}

func semanticEngineSettingsEqual(left, right models.EngineSettings) bool {
	if len(left) != len(right) {
		return false
	}
	for engineID, leftSettings := range left {
		rightSettings, ok := right[engineID]
		if !ok || !slices.Equal(leftSettings.AdditionalOptions, rightSettings.AdditionalOptions) {
			return false
		}
	}
	return true
}

func reconcilePortableTargetIDs(existing []string, oldActive map[string]bool, newTargets []models.BackupJobTarget) []string {
	newActive := make(map[string]bool, len(newTargets))
	for _, target := range newTargets {
		newActive[target.RepositoryID] = true
	}
	result := make([]string, 0, len(existing)+len(newTargets))
	seen := make(map[string]bool, len(existing)+len(newTargets))
	for _, id := range existing {
		if id == "" || seen[id] || oldActive[id] && !newActive[id] {
			continue
		}
		result = append(result, id)
		seen[id] = true
	}
	for _, target := range newTargets {
		if !seen[target.RepositoryID] {
			result = append(result, target.RepositoryID)
			seen[target.RepositoryID] = true
		}
	}
	return result
}

// RetainedPortableTargetEngines classifies the exact portable target set an
// UpdateJob call with newTargets will persist. Targets absent from the local
// repository catalog are reported through unresolved.
func RetainedPortableTargetEngines(db *sql.DB, jobID string, newTargets []models.BackupJobTarget) (known map[string]bool, unresolved bool, err error) {
	var encoded string
	if err = db.QueryRow(`SELECT portable_target_ids FROM backup_jobs WHERE id = ?`, jobID).Scan(&encoded); err != nil {
		return nil, false, err
	}
	existing := []string{}
	if err = json.Unmarshal([]byte(encoded), &existing); err != nil {
		return nil, false, fmt.Errorf("decode portable target identities: %w", err)
	}
	rows, err := db.Query(`SELECT repository_id FROM backup_job_targets WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, false, err
	}
	oldActive := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, false, err
		}
		oldActive[id] = true
	}
	if err := closeRows(rows); err != nil {
		return nil, false, err
	}
	known = map[string]bool{}
	for _, id := range reconcilePortableTargetIDs(existing, oldActive, newTargets) {
		var engine string
		err := db.QueryRow(`SELECT engine FROM repositories WHERE id = ?`, id).Scan(&engine)
		if errors.Is(err, sql.ErrNoRows) {
			unresolved = true
			continue
		}
		if err != nil {
			return nil, false, err
		}
		known[engine] = true
	}
	return known, unresolved, nil
}

func DeleteJob(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireJobConnectionUnreserved(tx, id); err != nil {
		return err
	}
	var active int
	// The deletion's own operation record carries this job ID; see JobDeletionKind.
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE job_id = ? AND kind <> ? AND status IN ('queued', 'running')`, id, JobDeletionKind).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrJobRunActive
	}
	rows, err := tx.Query(`SELECT repository_id FROM backup_job_targets WHERE job_id = ?`, id)
	if err != nil {
		return err
	}
	affected := []string{}
	for rows.Next() {
		var repositoryID string
		if err := rows.Scan(&repositoryID); err != nil {
			_ = rows.Close()
			return err
		}
		affected = append(affected, repositoryID)
	}
	if err := closeRows(rows); err != nil {
		return err
	}
	result, err := tx.Exec(`DELETE FROM backup_jobs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return sql.ErrNoRows
	}
	// The job's Restic snapshots stay in its vaults as unmanaged snapshots and
	// return to their native grouping root once the grouping is refreshed.
	targetRepositoryIDs := append([]string(nil), affected...)
	dormantAffected, err := deleteDormantRecoveryJobEverywhere(tx, id)
	if err != nil {
		return err
	}
	affected = append(affected, dormantAffected...)
	if _, err := RefreshKopiaPolicyStatesTx(tx, affected); err != nil {
		return err
	}
	if err := markVaultProfilesDirty(tx, mergePortableTargetIDs(affected)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	RefreshStaleMetadataGrouping(db, targetRepositoryIDs)
	return nil
}

// ValidateJobDeletionAdmission checks that the job can be deleted right now,
// without changing its stored definition. Callers that must run native cleanup
// first call this before starting it; DeleteJob repeats the same checks in its
// transaction afterward.
func ValidateJobDeletionAdmission(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireJobConnectionUnreserved(tx, id); err != nil {
		return err
	}
	var active int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE job_id = ? AND kind <> ? AND status IN ('queued', 'running')`, id, JobDeletionKind).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrJobRunActive
	}
	return nil
}

// ValidateJobDeletionRequest is the part of deletion admission a request can
// check without waiting: the job exists and no pending repository connection
// has reserved it. Queued or running work for the job is not a refusal any
// more; the deletion waits for it.
func ValidateJobDeletionRequest(db *sql.DB, id string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM backup_jobs WHERE id = ?`, id).Scan(&exists); err != nil {
		return err
	}
	if exists != 1 {
		return sql.ErrNoRows
	}
	return requireJobConnectionUnreserved(tx, id)
}

func SetJobEnabledCommitted(db *sql.DB, id string, enabled bool) (bool, error) {
	return SetJobEnabledWithSourceBindingCommitted(db, id, enabled, "", nil)
}

// SetJobEnabledWithSourceBindingCommitted permits the one local transition an
// imported job needs before it can become runnable: unbound_imported to bound
// with its final source. The binding and enabled state commit
// together, so a failed enable cannot leave a partially enabled job.
func SetJobEnabledWithSourceBindingCommitted(db *sql.DB, id string, enabled bool, expectedSource string, sourceBinding *models.BackupJob) (bool, error) {
	releaseDefinition, err := acquireJobDefinitionUse(db, id)
	if err != nil {
		return false, err
	}
	defer releaseDefinition()
	// Deliberately allowed while a target is queued or running. This setting
	// controls only future scheduler eligibility and never cancels admitted work.
	value := 0
	if enabled {
		value = 1
	}
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var schedule, nextRun, source, sourceStorageVersion, sourceStorageKey, sourceStorageJSON string
	var sourceBindingState, sourceKind string
	var wasEnabled bool
	if err := tx.QueryRow(`SELECT schedule,enabled,COALESCE(next_run,''),source,
		source_storage_version,source_storage_key,source_storage_json,source_binding_state,source_kind
		FROM backup_jobs WHERE id=?`, id).Scan(&schedule, &wasEnabled, &nextRun, &source,
		&sourceStorageVersion, &sourceStorageKey, &sourceStorageJSON, &sourceBindingState, &sourceKind); err != nil {
		return false, err
	}
	bindSource := enabled && normalizedJobSourceBindingState(sourceBindingState) == "unbound_imported"
	if bindSource {
		if sourceBinding == nil {
			return false, ErrJobSourceUnbound
		}
		existing := models.BackupJob{
			ID: id, Source: source, SourceStorageVersion: sourceStorageVersion,
			SourceStorageKey: sourceStorageKey, SourceStorageDescriptorJSON: sourceStorageJSON,
			SourceBindingState: sourceBindingState, SourceKind: sourceKind,
		}
		if sourceBinding.ID != id {
			return false, ErrJobSourceImmutable
		}
		if source != expectedSource {
			return false, ErrJobSourceChanged
		}
		if err := validateJobSourceTransition(existing, *sourceBinding, true); err != nil {
			return false, err
		}
		if err := validateJobSourceKindTransition(existing, *sourceBinding, true); err != nil {
			return false, err
		}
		if err := validateJobSourceBinding(*sourceBinding); err != nil {
			return false, err
		}
	}
	if sourceBinding != nil && !bindSource {
		// Another first binding may have committed while the source was observed.
		// Do not enable a different source or replace the winning local identity.
		existing := models.BackupJob{Source: source, SourceBindingState: sourceBindingState,
			SourceStorageVersion: sourceStorageVersion, SourceStorageKey: sourceStorageKey,
			SourceStorageDescriptorJSON: sourceStorageJSON, SourceKind: sourceKind}
		if sourceBinding.ID != id {
			return false, ErrJobSourceImmutable
		}
		if err := validateJobSourceTransition(existing, *sourceBinding, true); err != nil {
			return false, err
		}
		if err := validateJobSourceKindTransition(existing, *sourceBinding, true); err != nil {
			return false, err
		}
	}
	if wasEnabled == enabled && !bindSource {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if err := requireJobConnectionUnreserved(tx, id); err != nil {
		return false, err
	}
	nextRun = schedulerTransition(schedule, wasEnabled, nextRun, schedule, enabled, time.Now())
	var result sql.Result
	if bindSource {
		result, err = tx.Exec(`UPDATE backup_jobs SET enabled = ?, next_run = ?, source = ?,
			source_storage_version = ?, source_storage_key = ?, source_storage_json = ?,
			source_binding_state = 'bound'
			WHERE id = ?`, value, nextRun, sourceBinding.Source, sourceBinding.SourceStorageVersion,
			sourceBinding.SourceStorageKey, sourceBinding.SourceStorageDescriptorJSON, id)
	} else {
		result, err = tx.Exec(`UPDATE backup_jobs SET enabled = ?, next_run = ? WHERE id = ?`,
			value, nextRun, id)
	}
	if err != nil {
		return false, err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return false, sql.ErrNoRows
	}
	kindResolved := false
	if bindSource {
		if kindResolved, err = recordFirstBindingSourceKind(tx, id, sourceKind, sourceBinding.SourceKind); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(`UPDATE backup_job_targets
		SET logical_size_bytes=NULL, logical_size_measured_at='' WHERE job_id=?`, id); err != nil {
		return false, err
	}
	rows, err := tx.Query(`SELECT repository_id FROM backup_job_targets WHERE job_id = ?`, id)
	if err != nil {
		return false, err
	}
	affected := []string{}
	for rows.Next() {
		var repositoryID string
		if err := rows.Scan(&repositoryID); err != nil {
			_ = rows.Close()
			return false, err
		}
		affected = append(affected, repositoryID)
	}
	if err := closeRows(rows); err != nil {
		return false, err
	}
	targetRepositoryIDs := append([]string(nil), affected...)
	job, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM backup_jobs WHERE id = ?`, id).Scan)
	if err != nil {
		return false, err
	}
	dormantAffected, err := updateDormantDefinitionsForJob(tx, job)
	if err != nil {
		return false, err
	}
	affected = append(affected, dormantAffected...)
	if err := markVaultProfilesDirty(tx, affected); err != nil {
		return false, err
	}
	// A resolved kind may have cleared exclusions and marker-file options, which
	// are part of the Kopia desired policy; the refresh is a no-op for a vault
	// whose desired policy is unchanged.
	if bindSource && (source != sourceBinding.Source || kindResolved) {
		if _, err := RefreshKopiaPolicyStatesTx(tx, affected); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	if bindSource {
		// First binding sets the source File History groups this job's Restic
		// snapshots under.
		RefreshStaleMetadataGrouping(db, targetRepositoryIDs)
	}
	return true, nil
}

// BindImportedJobSource commits the final source and first local binding together.
// expectedSource is the saved source observed before binding, so a concurrent
// source edit cannot be overwritten by a stale observation.
func BindImportedJobSource(db *sql.DB, expectedSource string, bound models.BackupJob) error {
	releaseDefinition, err := acquireJobDefinitionUse(db, bound.ID)
	if err != nil {
		return err
	}
	defer releaseDefinition()
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireJobConnectionUnreserved(tx, bound.ID); err != nil {
		return err
	}
	existing, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM backup_jobs WHERE id = ?`, bound.ID).Scan)
	if err != nil {
		return err
	}
	if normalizedJobSourceBindingState(existing.SourceBindingState) == "bound" {
		if err := validateJobSourceTransition(existing, bound, true); err != nil {
			return err
		}
		return validateJobSourceKindTransition(existing, bound, true)
	}
	if existing.Source != expectedSource {
		return ErrJobSourceChanged
	}
	if err := validateJobSourceTransition(existing, bound, true); err != nil {
		return err
	}
	if err := validateJobSourceKindTransition(existing, bound, true); err != nil {
		return err
	}
	if err := validateJobSourceBinding(bound); err != nil {
		return err
	}
	result, err := tx.Exec(`UPDATE backup_jobs SET source = ?, source_storage_version = ?, source_storage_key = ?,
		source_storage_json = ?, source_binding_state = 'bound'
		WHERE id = ? AND source = ? AND source_binding_state = 'unbound_imported'`,
		bound.Source, bound.SourceStorageVersion, bound.SourceStorageKey, bound.SourceStorageDescriptorJSON,
		bound.ID, expectedSource)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrJobSourceImmutable
	}
	kindResolved, err := recordFirstBindingSourceKind(tx, bound.ID, existing.SourceKind, bound.SourceKind)
	if err != nil {
		return err
	}
	// Read the committed definition inside this transaction; binding input does
	// not own unrelated settings or portable targets.
	saved, err := scanJob(tx.QueryRow(`SELECT `+jobColumns+` FROM backup_jobs WHERE id = ?`, bound.ID).Scan)
	if err != nil {
		return err
	}
	affected, err := updateDormantDefinitionsForJob(tx, saved)
	if err != nil {
		return err
	}
	// A resolved kind is part of the portable definition, so the vault profiles
	// must be republished even when the source is unchanged. The edit revision
	// leaves the kind out, so nothing else would notice that they are stale.
	if existing.Source != saved.Source || kindResolved {
		rows, err := tx.Query(`SELECT repository_id FROM backup_job_targets WHERE job_id=?`, bound.ID)
		if err != nil {
			return err
		}
		for rows.Next() {
			var repositoryID string
			if err := rows.Scan(&repositoryID); err != nil {
				_ = rows.Close()
				return err
			}
			affected = append(affected, repositoryID)
		}
		if err := closeRows(rows); err != nil {
			return err
		}
		if err := markVaultProfilesDirty(tx, affected); err != nil {
			return err
		}
		if _, err := RefreshKopiaPolicyStatesTx(tx, affected); err != nil {
			return err
		}
		if existing.Source != saved.Source {
			if _, err := tx.Exec(`UPDATE backup_job_targets SET logical_size_bytes=NULL, logical_size_measured_at='' WHERE job_id=?`, bound.ID); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// First binding sets the source File History groups this job's Restic
	// snapshots under.
	RefreshStaleMetadataGrouping(db, jobTargetRepositoryIDs(db, bound.ID))
	return nil
}

func SetJobEnabled(db *sql.DB, id string, enabled bool) error {
	_, err := SetJobEnabledCommitted(db, id, enabled)
	return err
}

func schedulerTransition(
	existingSchedule string,
	existingEnabled bool,
	existingNextRun string,
	newSchedule string,
	newEnabled bool,
	now time.Time,
) string {
	if newSchedule == "" || newSchedule == "manual" {
		return ""
	}
	if existingSchedule != newSchedule {
		return nextJobRunFrom(newSchedule, now)
	}
	if !existingEnabled && newEnabled {
		saved, err := time.Parse(time.RFC3339, existingNextRun)
		if existingNextRun == "" || err == nil && nextRunBeyondSchedule(newSchedule, saved, now) {
			return nextJobRunFrom(newSchedule, now)
		}
	}
	return existingNextRun
}

// DueJobs lists the enabled jobs whose next run has come. It also moves a
// next run that lies more than one schedule period ahead back to the next
// occurrence from now (see nextRunBeyondSchedule), so a clock that jumped
// forward and back does not stall the job.
func DueJobs(db *sql.DB) ([]models.BackupJob, error) {
	jobs, err := ListJobs(db)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	due := []models.BackupJob{}
	for _, job := range jobs {
		if !job.Enabled || job.Schedule == "manual" || job.Schedule == "" || job.NextRun == "" {
			continue
		}
		when, err := time.Parse(time.RFC3339, job.NextRun)
		if err != nil {
			continue
		}
		if !when.After(now) {
			due = append(due, job)
			continue
		}
		if nextRunBeyondSchedule(job.Schedule, when, now) {
			rescheduleNextRun(db, job, now)
		}
	}
	return due, nil
}

func rescheduleNextRun(db *sql.DB, job models.BackupJob, now time.Time) {
	next := nextJobRunFrom(job.Schedule, now)
	if next == "" {
		return
	}
	// Only the value read above is replaced: an edit or a scheduled run that
	// changed the job meanwhile wins. A job reserved by a vault connection in
	// progress is left alone, as every other job write does.
	result, err := db.Exec(`UPDATE backup_jobs SET next_run=?
		WHERE id=? AND enabled=1 AND schedule=? AND next_run=?
			AND NOT EXISTS (SELECT 1 FROM repository_connection_job_name_reservations r
				JOIN repository_connection_intents c ON c.id=r.connection_id
				WHERE r.job_id=backup_jobs.id)`,
		next, job.ID, job.Schedule, job.NextRun)
	if err != nil {
		log.Printf("reschedule job %s after a clock change: %v", job.ID, err)
		return
	}
	if count, _ := result.RowsAffected(); count == 1 {
		log.Printf("job %s: next run %s was more than one schedule period ahead; rescheduled to %s",
			job.ID, job.NextRun, next)
	}
}
