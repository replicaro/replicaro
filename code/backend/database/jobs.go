package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	ErrJobDefinitionBusy     = errors.New("backup job definition is being deleted")
	ErrInvalidTargets        = errors.New("at least one unique existing repository target is required")
	ErrJobSourceImmutable    = errors.New("backup job source cannot be changed after creation")
	ErrJobSourceChanged      = errors.New("backup job source changed during binding; retry the operation")
	ErrJobSourceUnbound      = errors.New("imported backup job source is not bound to this computer")
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

// AcquireJobDefinitionDeletion holds the one local per-job definition gate
// across deletion admission, exact native cleanup, ownership removal, and the
// final definition delete. It is intentionally process-local and run-scoped.
func AcquireJobDefinitionDeletion(db *sql.DB, jobID string) (func(), error) {
	gate := jobDefinitionGate(db, jobID)
	if !gate.TryLock() {
		return nil, ErrJobDefinitionBusy
	}
	return gate.Unlock, nil
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
	excludes, tag,
	before_script_path, before_script_must_succeed, after_script_path, after_script_must_succeed,
	engine_settings, portable_target_ids`

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
		&j.Excludes, &j.Tag, &j.BeforeScriptPath, &j.BeforeScriptMustSucceed,
		&j.AfterScriptPath, &j.AfterScriptMustSucceed, &settingsJSON, &targetIDsJSON)
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

func setResolvedSourcePathTx(tx *sql.Tx, jobID, configuredPath, resolvedPath string, observedAt time.Time) error {
	return setResolvedSourcePath(tx.Exec, jobID, configuredPath, resolvedPath, observedAt)
}

func SetResolvedSourcePath(db *sql.DB, jobID, configuredPath, resolvedPath string, observedAt time.Time) error {
	return setResolvedSourcePath(db.Exec, jobID, configuredPath, resolvedPath, observedAt)
}

func setResolvedSourcePath(exec func(string, ...any) (sql.Result, error), jobID, configuredPath, resolvedPath string, observedAt time.Time) error {
	stored := resolvedPath
	if stored == configuredPath {
		stored = ""
	}
	result, err := exec(`UPDATE backup_jobs SET resolved_source_path=?, resolved_source_observed_at=? WHERE id=?`,
		stored, observedAt.UTC().Format(time.RFC3339Nano), jobID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	return nil
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
			&target.PendingCatchUp, &target.CoalescedMissedCount,
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
	return job, populateJobTargets(db, &job)
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
	if err := validateJobSourceBinding(job); err != nil {
		return "", err
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
		excludes, tag, before_script_path, before_script_must_succeed,
		after_script_path, after_script_must_succeed, engine_settings, portable_target_ids)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, '', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, job.Name, job.Source, job.SourceStorageVersion, job.SourceStorageKey,
		job.SourceStorageDescriptorJSON, func() string {
			if job.SourceBindingState == "" {
				return "bound"
			}
			return job.SourceBindingState
		}(), job.Schedule, job.Enabled,
		NextRunFrom(job.Schedule, time.Now()), job.Retention,
		job.RetentionHourly, job.RetentionDaily, job.RetentionWeekly, job.RetentionMonthly, job.RetentionYearly,
		job.Excludes, job.Tag, job.BeforeScriptPath, job.BeforeScriptMustSucceed,
		job.AfterScriptPath, job.AfterScriptMustSucceed, string(settingsJSON), string(portableJSON))
	if err != nil {
		return "", err
	}
	for _, target := range job.Targets {
		if _, err := tx.Exec(`INSERT INTO backup_job_targets (job_id, repository_id) VALUES (?, ?)`, id, target.RepositoryID); err != nil {
			return "", err
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
	return id, nil
}

func UpdateJob(db *sql.DB, job models.BackupJob) error {
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
	if err := validateJobSourceTransition(existingJob, job, false); err != nil {
		return err
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
	if err := oldRows.Close(); err != nil {
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
		retention_monthly = ?, retention_yearly = ?, excludes = ?, tag = ?, enabled = ?, next_run = ?,
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
		job.Excludes, job.Tag,
		job.Enabled, nextRun, job.BeforeScriptPath, job.BeforeScriptMustSucceed,
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
	for _, target := range job.Targets {
		if _, err := tx.Exec(`INSERT INTO backup_job_targets (job_id, repository_id) VALUES (?, ?)
			ON CONFLICT(job_id, repository_id) DO NOTHING`, job.ID, target.RepositoryID); err != nil {
			return err
		}
	}
	if _, err := RefreshKopiaPolicyStatesTx(tx, affected); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO activity_log (timestamp, level, message) VALUES (datetime('now'), 'INFO', ?)`, "Backup job updated: "+job.Name); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
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
	if existingState != nextState ||
		existing.SourceStorageVersion != next.SourceStorageVersion ||
		existing.SourceStorageKey != next.SourceStorageKey ||
		existing.SourceStorageDescriptorJSON != next.SourceStorageDescriptorJSON {
		return ErrJobSourceImmutable
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
	if err := rows.Close(); err != nil {
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
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE job_id = ? AND status IN ('queued', 'running')`, id).Scan(&active); err != nil {
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
	if err := rows.Close(); err != nil {
		return err
	}
	result, err := tx.Exec(`DELETE FROM backup_jobs WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count == 0 {
		return sql.ErrNoRows
	}
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
	return tx.Commit()
}

// ValidateJobDeletionAdmission proves that deleting a job is currently
// admissible without changing its authoritative definition. Callers that must
// perform exact native cleanup first use this gate before crossing that native
// boundary; DeleteJob repeats the same checks transactionally afterward.
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
	if err := tx.QueryRow(`SELECT COUNT(*) FROM operations WHERE job_id = ? AND status IN ('queued', 'running')`, id).Scan(&active); err != nil {
		return err
	}
	if active > 0 {
		return ErrJobRunActive
	}
	return nil
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
	var sourceBindingState string
	var wasEnabled bool
	if err := tx.QueryRow(`SELECT schedule,enabled,COALESCE(next_run,''),source,
		source_storage_version,source_storage_key,source_storage_json,source_binding_state
		FROM backup_jobs WHERE id=?`, id).Scan(&schedule, &wasEnabled, &nextRun, &source,
		&sourceStorageVersion, &sourceStorageKey, &sourceStorageJSON, &sourceBindingState); err != nil {
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
			SourceBindingState: sourceBindingState,
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
		if err := validateJobSourceBinding(*sourceBinding); err != nil {
			return false, err
		}
	}
	if sourceBinding != nil && !bindSource {
		// Another first binding may have committed while the source was observed.
		// Do not enable a different source or replace the winning local identity.
		existing := models.BackupJob{Source: source, SourceBindingState: sourceBindingState,
			SourceStorageVersion: sourceStorageVersion, SourceStorageKey: sourceStorageKey,
			SourceStorageDescriptorJSON: sourceStorageJSON}
		if sourceBinding.ID != id {
			return false, ErrJobSourceImmutable
		}
		if err := validateJobSourceTransition(existing, *sourceBinding, true); err != nil {
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
	if err := rows.Close(); err != nil {
		return false, err
	}
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
	if bindSource && source != sourceBinding.Source {
		if _, err := RefreshKopiaPolicyStatesTx(tx, affected); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
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
		return validateJobSourceTransition(existing, bound, true)
	}
	if existing.Source != expectedSource {
		return ErrJobSourceChanged
	}
	if err := validateJobSourceTransition(existing, bound, true); err != nil {
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
	if existing.Source != saved.Source {
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
		if err := rows.Close(); err != nil {
			return err
		}
		if err := markVaultProfilesDirty(tx, affected); err != nil {
			return err
		}
		if _, err := RefreshKopiaPolicyStatesTx(tx, affected); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE backup_job_targets SET logical_size_bytes=NULL, logical_size_measured_at='' WHERE job_id=?`, bound.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
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
		return NextRunFrom(newSchedule, now)
	}
	if !existingEnabled && newEnabled && existingNextRun == "" {
		return NextRunFrom(newSchedule, now)
	}
	return existingNextRun
}

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
		if err == nil && !when.After(now) {
			due = append(due, job)
		}
	}
	return due, nil
}
