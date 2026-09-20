package database

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/operationlog"
	"github.com/local/replicaro/schedulevalue"
)

const (
	StorageAvailable   = "available"
	StorageUnavailable = "unavailable"
	StorageUnknown     = "unknown"

	AvailabilityReasonNone               = ""
	AvailabilityReasonNotChecked         = "not_checked"
	AvailabilityReasonStorageMissing     = "storage_missing"
	AvailabilityReasonIdentityMismatch   = "identity_mismatch"
	AvailabilityReasonObservationFailed  = "observation_failed"
	AvailabilityReasonObservationTimeout = "observation_timeout"
)

const (
	AdmissionAdmittedRegular    = "admitted_regular"
	AdmissionAdmittedCatchUp    = "admitted_catchup"
	AdmissionStorageUnavailable = "storage_unavailable"
	AdmissionBusy               = "busy"
	AdmissionPolicyNotReady     = "policy_not_ready"
	AdmissionPaused             = "paused"
	AdmissionNoPending          = "no_pending"
)

type StorageAvailabilityObservation struct {
	State        string
	ReasonCode   string
	CheckedAt    time.Time
	ObservedKey  string
	ResolvedPath string
	// ConclusiveFailure is run-local routing only. It lets a genuine storage
	// validation error create a visible failed attempt through the existing
	// executor without persisting a new status or retry mechanism.
	ConclusiveFailure bool
}

type TargetAvailabilityObservation struct {
	RepositoryID string
	Availability StorageAvailabilityObservation
}

type ScheduledAdmissionRequest struct {
	JobID   string
	DueAt   time.Time
	Now     time.Time
	Source  StorageAvailabilityObservation
	Targets []TargetAvailabilityObservation
}

type ManualAdmissionRequest struct {
	JobID   string
	Now     time.Time
	Source  StorageAvailabilityObservation
	Targets []TargetAvailabilityObservation
}

type TargetAdmissionResult struct {
	RepositoryID               string
	Status                     string
	ReasonCode                 string
	OperationID                string
	RequiresSourceFailureCheck bool
	RequiresTargetFailureCheck bool
}

type PendingCatchUp struct {
	JobID                string
	RepositoryID         string
	CoalescedMissedCount int64
	FirstDeferredDueAt   string
	LastDueAt            string
	SourceAvailability   string
	SourceReasonCode     string
	SourceCheckedAt      string
	TargetAvailability   string
	TargetReasonCode     string
	TargetCheckedAt      string
	JobEnabled           bool
	Schedule             string
}

type scheduledTargetRow struct {
	repositoryID, repositoryName, engine, connector, storageKey string
	firstDeferredDueAt, lastDueAt                               string
	pending, missed                                             int64
}

func validateAvailabilityObservation(value StorageAvailabilityObservation, now time.Time) error {
	if value.CheckedAt.IsZero() {
		return fmt.Errorf("storage availability check time is required")
	}
	if value.CheckedAt.After(now.Add(time.Second)) {
		return fmt.Errorf("storage availability check time is in the future")
	}
	switch value.State {
	case StorageAvailable:
		if value.ReasonCode != AvailabilityReasonNone {
			return fmt.Errorf("available storage must not contain an unavailable reason")
		}
	case StorageUnavailable:
		switch value.ReasonCode {
		case AvailabilityReasonStorageMissing, AvailabilityReasonIdentityMismatch,
			AvailabilityReasonObservationFailed, AvailabilityReasonObservationTimeout:
		default:
			return fmt.Errorf("storage availability reason is invalid")
		}
	case StorageUnknown:
		if value.ReasonCode != AvailabilityReasonNotChecked &&
			value.ReasonCode != AvailabilityReasonObservationFailed &&
			value.ReasonCode != AvailabilityReasonObservationTimeout &&
			value.ReasonCode != AvailabilityReasonIdentityMismatch {
			return fmt.Errorf("unknown storage availability reason is invalid")
		}
	default:
		return fmt.Errorf("storage availability state is invalid")
	}
	if value.ConclusiveFailure && (value.State != StorageUnknown ||
		value.ReasonCode != AvailabilityReasonIdentityMismatch &&
			value.ReasonCode != AvailabilityReasonObservationFailed) {
		return fmt.Errorf("conclusive storage failure classification is invalid")
	}
	return nil
}

func validateSourceObservedKey(observation StorageAvailabilityObservation, expected string) error {
	if observation.State == StorageAvailable && observation.ObservedKey != expected {
		return fmt.Errorf("available source identity does not match the persisted binding")
	}
	return nil
}

func advanceScheduleAfter(schedule string, dueAt, now time.Time) (string, error) {
	if schedule == "" || schedule == "manual" {
		return "", fmt.Errorf("manual schedules do not have regular occurrences")
	}
	nextText := NextRunFrom(schedule, dueAt)
	if nextText == "" {
		return "", fmt.Errorf("backup schedule is invalid")
	}
	next, err := time.Parse(time.RFC3339Nano, nextText)
	if err != nil {
		return "", fmt.Errorf("backup schedule produced an invalid occurrence")
	}
	if _, cronSchedule := schedulevalue.CronExpression(schedule); cronSchedule && !next.After(now) {
		nextText = NextRunFrom(schedule, now)
		if nextText == "" {
			return "", fmt.Errorf("backup schedule is invalid")
		}
		next, err = time.Parse(time.RFC3339Nano, nextText)
		if err != nil || !next.After(now) {
			return "", fmt.Errorf("backup schedule does not advance")
		}
		return next.Format(time.RFC3339Nano), nil
	}
	_, customMonths := schedulevalue.CustomMonths(schedule)
	if schedule != "monthly" && !customMonths && !next.After(now) {
		interval := next.Sub(dueAt)
		if interval <= 0 {
			return "", fmt.Errorf("backup schedule does not advance")
		}
		steps := now.Sub(next)/interval + 1
		next = next.Add(steps * interval)
		return next.Format(time.RFC3339Nano), nil
	}
	for !next.After(now) {
		nextText = NextRunFrom(schedule, next)
		if nextText == "" {
			return "", fmt.Errorf("backup schedule is invalid")
		}
		following, parseErr := time.Parse(time.RFC3339Nano, nextText)
		if parseErr != nil || !following.After(next) {
			return "", fmt.Errorf("backup schedule does not advance")
		}
		next = following
	}
	return next.Format(time.RFC3339Nano), nil
}

func loadScheduledTargets(tx *sql.Tx, jobID string) ([]scheduledTargetRow, error) {
	rows, err := tx.Query(`SELECT t.repository_id,r.name,r.engine,r.connector,r.storage_identity_key,
		s.pending_catchup,s.coalesced_missed_count,s.first_deferred_due_at,s.last_due_at
		FROM backup_job_targets t
		JOIN repositories r ON r.id=t.repository_id
		JOIN job_target_schedule_state s ON s.job_id=t.job_id AND s.repository_id=t.repository_id
		WHERE t.job_id=? ORDER BY t.repository_id`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []scheduledTargetRow{}
	for rows.Next() {
		var value scheduledTargetRow
		if err := rows.Scan(&value.repositoryID, &value.repositoryName, &value.engine,
			&value.connector, &value.storageKey, &value.pending, &value.missed,
			&value.firstDeferredDueAt, &value.lastDueAt); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func observationMap(targets []TargetAvailabilityObservation) (map[string]StorageAvailabilityObservation, error) {
	result := make(map[string]StorageAvailabilityObservation, len(targets))
	for _, target := range targets {
		if target.RepositoryID == "" {
			return nil, ErrInvalidTargets
		}
		if _, exists := result[target.RepositoryID]; exists {
			return nil, ErrInvalidTargets
		}
		result[target.RepositoryID] = target.Availability
	}
	return result, nil
}

func updatePairObservations(
	tx *sql.Tx,
	jobID, repositoryID string,
	source, target StorageAvailabilityObservation,
	now time.Time,
) error {
	_, err := tx.Exec(`UPDATE job_target_schedule_state
		SET source_availability=?,source_reason_code=?,source_checked_at=?,
		    target_availability=?,target_reason_code=?,target_checked_at=?,updated_at=?
		WHERE job_id=? AND repository_id=?`,
		source.State, source.ReasonCode, source.CheckedAt.UTC().Format(time.RFC3339Nano),
		target.State, target.ReasonCode, target.CheckedAt.UTC().Format(time.RFC3339Nano),
		now.UTC().Format(time.RFC3339Nano), jobID, repositoryID)
	return err
}

func markPairMissed(tx *sql.Tx, jobID, repositoryID string, dueAt, now time.Time) error {
	due := dueAt.UTC().Format(time.RFC3339Nano)
	_, err := tx.Exec(`UPDATE job_target_schedule_state
		SET pending_catchup=1,
		    coalesced_missed_count=coalesced_missed_count+1,
		    first_deferred_due_at=CASE WHEN pending_catchup=0 THEN ? ELSE first_deferred_due_at END,
		    last_due_at=?,updated_at=?
		WHERE job_id=? AND repository_id=?`,
		due, due, now.UTC().Format(time.RFC3339Nano), jobID, repositoryID)
	return err
}

func clearPairPending(tx *sql.Tx, jobID, repositoryID string, now time.Time) error {
	_, err := tx.Exec(`UPDATE job_target_schedule_state
		SET pending_catchup=0,coalesced_missed_count=0,first_deferred_due_at='',updated_at=?
		WHERE job_id=? AND repository_id=?`,
		now.UTC().Format(time.RFC3339Nano), jobID, repositoryID)
	return err
}

// RestorePreNativeUnavailable returns only a scheduled occurrence whose full
// destination admission conclusively stopped before native backup launch. It
// reuses the existing coalesced pair state; no retry counter, timer row, or
// remote coordination is introduced.
func RestorePreNativeUnavailable(db *sql.DB, operationID, reason string, checkedAt time.Time) error {
	return restorePreNativeUnavailable(db, operationID, reason, checkedAt, false)
}

func RestorePreNativeSourceUnavailable(db *sql.DB, operationID, reason string, checkedAt time.Time) error {
	return restorePreNativeUnavailable(db, operationID, reason, checkedAt, true)
}

func restorePreNativeUnavailable(db *sql.DB, operationID, reason string, checkedAt time.Time, source bool) error {
	if operationID == "" || checkedAt.IsZero() {
		return ErrInvalidTargets
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var groupID, jobID, repositoryID, firstDue, lastDue, restoreState string
	var missed int64
	err = tx.QueryRow(`SELECT group_id,job_id,repository_id,coalesced_missed_count,
		first_deferred_due_at,last_due_at,restore_state
		FROM scheduled_operation_restorations WHERE operation_id=?`, operationID).
		Scan(&groupID, &jobID, &repositoryID, &missed, &firstDue, &lastDue, &restoreState)
	if err == sql.ErrNoRows {
		return nil // Manual operations have no catch-up occurrence to restore.
	}
	if err != nil || restoreState != "eligible" {
		return err
	}
	savedFirst, savedLast, err := parseRestorationRange(missed, firstDue, lastDue)
	if err != nil {
		return err
	}
	var pending, currentCount int64
	var currentFirstText, currentLastText string
	if err := tx.QueryRow(`SELECT pending_catchup,coalesced_missed_count,
		first_deferred_due_at,last_due_at FROM job_target_schedule_state
		WHERE job_id=? AND repository_id=?`, jobID, repositoryID).Scan(
		&pending, &currentCount, &currentFirstText, &currentLastText); err != nil {
		return err
	}
	mergedCount, mergedFirst, mergedLast := missed, savedFirst, savedLast
	if pending == 1 {
		currentFirst, currentLast, parseErr := parseRestorationRange(currentCount, currentFirstText, currentLastText)
		if parseErr != nil {
			return parseErr
		}
		if currentCount > math.MaxInt64-missed {
			return fmt.Errorf("scheduled catch-up count overflow")
		}
		mergedCount += currentCount
		if currentFirst.Before(mergedFirst) {
			mergedFirst = currentFirst
		}
		if currentLast.After(mergedLast) {
			mergedLast = currentLast
		}
	} else if pending != 0 || currentCount != 0 || currentFirstText != "" {
		return fmt.Errorf("current scheduled catch-up state is invalid")
	}
	checked := checkedAt.UTC().Format(time.RFC3339Nano)
	availabilityColumns := "target_availability=?,target_reason_code=?,target_checked_at=?"
	if source {
		availabilityColumns = "source_availability=?,source_reason_code=?,source_checked_at=?"
	}
	if _, err := tx.Exec(`UPDATE job_target_schedule_state SET
		pending_catchup=1,coalesced_missed_count=?,first_deferred_due_at=?,last_due_at=?,`+
		availabilityColumns+`,updated_at=? WHERE job_id=? AND repository_id=?`,
		mergedCount, mergedFirst.Format(time.RFC3339Nano), mergedLast.Format(time.RFC3339Nano),
		StorageUnavailable, reason, checked, checked, jobID, repositoryID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE scheduled_operation_restorations SET restore_state='restored'
		WHERE operation_id=? AND restore_state='eligible'`, operationID); err != nil {
		return err
	}
	if err := reconcileScheduledAdmissionGroupsTx(tx, jobID); err != nil {
		return err
	}
	return tx.Commit()
}

// ConsumeScheduledBackupOccurrence closes restoration only after this process
// reached the requested native-backup boundary or produced a terminal outcome
// that is not conclusive pre-native unavailability. Manual operations have no
// restoration row.
func ConsumeScheduledBackupOccurrence(db *sql.DB, operationID string) error {
	if operationID == "" {
		return ErrInvalidTargets
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var groupID, jobID, state string
	err = tx.QueryRow(`SELECT group_id,job_id,restore_state
		FROM scheduled_operation_restorations WHERE operation_id=?`, operationID).
		Scan(&groupID, &jobID, &state)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if state != "eligible" {
		return nil
	}
	if _, err := tx.Exec(`UPDATE scheduled_operation_restorations SET restore_state='started'
		WHERE operation_id=? AND restore_state='eligible'`, operationID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE scheduled_admission_groups SET any_started=1
		WHERE id=? AND job_id=?`, groupID, jobID); err != nil {
		return err
	}
	if err := reconcileScheduledAdmissionGroupsTx(tx, jobID); err != nil {
		return err
	}
	return tx.Commit()
}

func queueAdmittedPair(
	tx *sql.Tx,
	jobID, jobName, source, repositoryID, repositoryName, engine string,
	now time.Time,
) (string, error) {
	if err := requireKopiaPolicyReady(tx, repositoryID); err != nil {
		return "", err
	}
	id := uuid.NewString()
	title := "Backup: " + jobName + " → " + repositoryName
	if _, err := tx.Exec(`INSERT INTO operations
		(id,kind,status,title,job_id,repository_id,engine,started_at)
		VALUES(?,'backup','queued',?,?,?,?,?)`,
		id, title, jobID, repositoryID, engine, formatSortableTimestamp(now)); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`UPDATE backup_job_targets SET last_status='queued'
		WHERE job_id=? AND repository_id=?`, jobID, repositoryID); err != nil {
		return "", err
	}
	return id, nil
}

// CoordinateScheduledAdmission records one regular occurrence when DueAt is
// nonzero, advances from that exact due time, coalesces unavailable/busy pairs,
// and admits each currently available pending pair at most once.
func CoordinateScheduledAdmission(db *sql.DB, request ScheduledAdmissionRequest) ([]TargetAdmissionResult, error) {
	if request.JobID == "" || len(request.Targets) == 0 {
		return nil, ErrInvalidTargets
	}
	releaseDefinition, err := acquireJobDefinitionUse(db, request.JobID)
	if err != nil {
		return nil, err
	}
	defer releaseDefinition()
	now := request.Now.UTC()
	if now.IsZero() {
		return nil, fmt.Errorf("scheduled admission time is required")
	}
	if err := validateAvailabilityObservation(request.Source, now); err != nil {
		return nil, err
	}
	targetObservations, err := observationMap(request.Targets)
	if err != nil {
		return nil, err
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireJobConnectionUnreserved(tx, request.JobID); err != nil {
		return nil, err
	}
	var jobName, source, sourceKey, schedule, nextRun, lastRun string
	var enabled bool
	if err := tx.QueryRow(`SELECT name,source,source_storage_key,schedule,
		COALESCE(next_run,''),COALESCE(last_run,''),enabled
		FROM backup_jobs WHERE id=?`, request.JobID).Scan(
		&jobName, &source, &sourceKey, &schedule, &nextRun, &lastRun, &enabled); err != nil {
		return nil, err
	}
	// Source aliases remain identity-bearing local state. Preserve the database
	// boundary check even though destination observations deliberately no longer
	// claim that an address-derived storage key proves a vault.
	if err := validateSourceObservedKey(request.Source, sourceKey); err != nil {
		return nil, err
	}
	resolvedSource := request.Source.ResolvedPath
	if request.Source.State != StorageAvailable {
		resolvedSource = source
	}
	if err := setResolvedSourcePathTx(tx, request.JobID, source, resolvedSource, request.Source.CheckedAt); err != nil {
		return nil, err
	}
	targets, err := loadScheduledTargets(tx, request.JobID)
	if err != nil {
		return nil, err
	}
	regularDue := !request.DueAt.IsZero()
	if len(targets) == 0 || len(targetObservations) == 0 ||
		(regularDue && len(targetObservations) != len(targets)) || len(targetObservations) > len(targets) {
		return nil, ErrInvalidTargets
	}
	targetByID := make(map[string]scheduledTargetRow, len(targets))
	for _, target := range targets {
		targetByID[target.repositoryID] = target
		observation, ok := targetObservations[target.repositoryID]
		if !ok {
			if regularDue {
				return nil, ErrInvalidTargets
			}
			continue
		}
		if err := validateAvailabilityObservation(observation, now); err != nil {
			return nil, err
		}
		// Preliminary target observations never publish aliases. The once-per-
		// operation repository proof commits only a fully verified runtime path.
		if err := updatePairObservations(tx, request.JobID, target.repositoryID, request.Source, observation, now); err != nil {
			return nil, err
		}
	}
	if regularDue {
		due := request.DueAt.UTC()
		persistedDue, parseErr := time.Parse(time.RFC3339Nano, nextRun)
		if parseErr != nil || !persistedDue.Equal(due) || due.After(now) {
			return nil, ErrBackupTriggerChanged
		}
		if !enabled {
			results := make([]TargetAdmissionResult, 0, len(targets))
			for _, target := range targets {
				results = append(results, TargetAdmissionResult{RepositoryID: target.repositoryID, Status: AdmissionPaused})
			}
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return results, nil
		}
		next, err := advanceScheduleAfter(schedule, due, now)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE backup_jobs SET next_run=? WHERE id=?`, next, request.JobID); err != nil {
			return nil, err
		}
		for index := range targets {
			target := &targets[index]
			if err := markPairMissed(tx, request.JobID, target.repositoryID, due, now); err != nil {
				return nil, err
			}
			if target.pending == 0 {
				target.firstDeferredDueAt = due.Format(time.RFC3339Nano)
			}
			target.lastDueAt = due.Format(time.RFC3339Nano)
			target.pending = 1
			target.missed++
		}
	}
	resultTargets := targets
	if !regularDue {
		resultTargets = make([]scheduledTargetRow, 0, len(targetObservations))
		for repositoryID := range targetObservations {
			target, ok := targetByID[repositoryID]
			if !ok {
				return nil, ErrInvalidTargets
			}
			resultTargets = append(resultTargets, target)
		}
		sort.Slice(resultTargets, func(i, j int) bool { return resultTargets[i].repositoryID < resultTargets[j].repositoryID })
	}
	results := make([]TargetAdmissionResult, 0, len(resultTargets))
	admitted := false
	groupID := ""
	for _, target := range resultTargets {
		observation := targetObservations[target.repositoryID]
		result := TargetAdmissionResult{RepositoryID: target.repositoryID}
		if !enabled {
			result.Status = AdmissionPaused
			results = append(results, result)
			continue
		}
		if target.pending == 0 {
			result.Status = AdmissionNoPending
			results = append(results, result)
			continue
		}
		if request.Source.State != StorageAvailable && !request.Source.ConclusiveFailure {
			result.Status = AdmissionStorageUnavailable
			result.ReasonCode = request.Source.ReasonCode
			results = append(results, result)
			continue
		}
		if observation.State != StorageAvailable && !request.Source.ConclusiveFailure && !observation.ConclusiveFailure {
			result.Status = AdmissionStorageUnavailable
			result.ReasonCode = observation.ReasonCode
			results = append(results, result)
			continue
		}
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM operations
			WHERE job_id=? AND repository_id=? AND status IN ('queued','running')`,
			request.JobID, target.repositoryID).Scan(&active); err != nil {
			return nil, err
		}
		if active != 0 {
			result.Status = AdmissionBusy
			results = append(results, result)
			continue
		}
		if err := requireKopiaPolicyReady(tx, target.repositoryID); err != nil {
			if errors.Is(err, ErrKopiaPolicyNotReady) {
				result.Status = AdmissionPolicyNotReady
				result.ReasonCode = "kopia_policy_not_ready"
				results = append(results, result)
				continue
			}
			return nil, err
		}
		operationID, err := queueAdmittedPair(tx, request.JobID, jobName, source,
			target.repositoryID, target.repositoryName, target.engine, now)
		if err != nil {
			return nil, err
		}
		if groupID == "" {
			groupID = uuid.NewString()
			if _, err := tx.Exec(`INSERT INTO scheduled_admission_groups
				(id,job_id,generation,admitted_at,previous_job_last_run)
				SELECT ?,?,COALESCE(MAX(generation),0)+1,?,?
				FROM scheduled_admission_groups WHERE job_id=?`,
				groupID, request.JobID, now.Format(time.RFC3339Nano), lastRun, request.JobID); err != nil {
				return nil, err
			}
		}
		if target.missed <= 0 || target.firstDeferredDueAt == "" || target.lastDueAt == "" {
			return nil, fmt.Errorf("scheduled admission restoration state is invalid")
		}
		if _, _, err := parseRestorationRange(
			target.missed, target.firstDeferredDueAt, target.lastDueAt,
		); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO scheduled_operation_restorations
			(operation_id,group_id,job_id,repository_id,coalesced_missed_count,
			 first_deferred_due_at,last_due_at)
			VALUES(?,?,?,?,?,?,?)`,
			operationID, groupID, request.JobID, target.repositoryID, target.missed,
			target.firstDeferredDueAt, target.lastDueAt); err != nil {
			return nil, err
		}
		if err := clearPairPending(tx, request.JobID, target.repositoryID, now); err != nil {
			return nil, err
		}
		result.OperationID = operationID
		result.RequiresSourceFailureCheck = request.Source.ConclusiveFailure
		if request.Source.ConclusiveFailure {
			result.ReasonCode = request.Source.ReasonCode
		} else if observation.ConclusiveFailure {
			result.RequiresTargetFailureCheck = true
			result.ReasonCode = observation.ReasonCode
		}
		if regularDue && target.missed == 1 {
			result.Status = AdmissionAdmittedRegular
		} else {
			result.Status = AdmissionAdmittedCatchUp
		}
		admitted = true
		results = append(results, result)
	}
	if admitted {
		if _, err := tx.Exec(`UPDATE backup_jobs SET last_run=? WHERE id=?`,
			now.Format(time.RFC3339Nano), request.JobID); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

// InterruptQueuedBackupOperations atomically interrupts queued backup
// operations. Scheduled operations restore their durable admitted occurrence;
// manual operations have no restoration metadata and remain otherwise
// unaffected. Already-restored interruptions are idempotent.
func InterruptQueuedBackupOperations(
	db *sql.DB,
	operationIDs []string,
	output string,
	finishedAt time.Time,
) error {
	if finishedAt.IsZero() {
		return fmt.Errorf("queued interruption time is required")
	}
	seen := make(map[string]bool, len(operationIDs))
	finals := make([]operationlog.Final, 0, len(operationIDs))
	for _, operationID := range operationIDs {
		if operationID == "" || seen[operationID] {
			return ErrInvalidTargets
		}
		seen[operationID] = true
		var engine, kind string
		// Engine and kind are immutable operation identity. Load them before the
		// local log barrier so no transaction can hold SQLite's sole connection
		// while waiting behind another operation's final append.
		if err := db.QueryRow(`SELECT engine,kind FROM operations WHERE id=?`, operationID).Scan(&engine, &kind); err != nil {
			return err
		}
		finals = append(finals, operationlog.Final{
			OperationID: operationID, Engine: engine, Kind: kind, Status: "interrupted", Output: output,
		})
	}
	// Every terminal path acquires the local log barrier before beginning its
	// database write. Keeping that single order avoids a shutdown deadlock
	// without adding a vault lock or any remote coordination.
	return operationlog.FinalizeOperations(finals, func() error {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		for _, final := range finals {
			if err := restoreQueuedBackupOperationTx(tx, final.OperationID, output, finishedAt.UTC()); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}

type scheduledRestorationRow struct {
	groupID, jobID, repositoryID                string
	firstDeferredDueAt, lastDueAt, restoreState string
	admittedAt                                  string
	coalescedMissedCount                        int64
}

func parseRestorationRange(count int64, firstText, lastText string) (time.Time, time.Time, error) {
	if count <= 0 || firstText == "" || lastText == "" {
		return time.Time{}, time.Time{}, fmt.Errorf("scheduled admission restoration state is invalid")
	}
	first, err := time.Parse(time.RFC3339Nano, firstText)
	if err != nil {
		return time.Time{}, time.Time{}, fmt.Errorf("scheduled admission first due time is invalid")
	}
	last, err := time.Parse(time.RFC3339Nano, lastText)
	if err != nil || last.Before(first) {
		return time.Time{}, time.Time{}, fmt.Errorf("scheduled admission last due time is invalid")
	}
	return first.UTC(), last.UTC(), nil
}

func restoreQueuedBackupOperationTx(
	tx *sql.Tx,
	operationID, output string,
	finishedAt time.Time,
) error {
	var jobID, repositoryID, status, queuedAt string
	if err := tx.QueryRow(`SELECT job_id,repository_id,status,started_at
		FROM operations WHERE id=? AND kind='backup'`, operationID).Scan(
		&jobID, &repositoryID, &status, &queuedAt); err != nil {
		return err
	}
	if jobID == "" || repositoryID == "" {
		return sql.ErrNoRows
	}
	var restoration scheduledRestorationRow
	restoreErr := tx.QueryRow(`SELECT r.group_id,r.job_id,r.repository_id,
		r.coalesced_missed_count,r.first_deferred_due_at,r.last_due_at,r.restore_state,
		g.admitted_at
		FROM scheduled_operation_restorations r
		JOIN scheduled_admission_groups g ON g.id=r.group_id AND g.job_id=r.job_id
		WHERE r.operation_id=?`, operationID).Scan(
		&restoration.groupID, &restoration.jobID, &restoration.repositoryID,
		&restoration.coalescedMissedCount, &restoration.firstDeferredDueAt,
		&restoration.lastDueAt, &restoration.restoreState, &restoration.admittedAt)
	if restoreErr != nil && restoreErr != sql.ErrNoRows {
		return restoreErr
	}
	if status == "interrupted" {
		if restoreErr == sql.ErrNoRows || restoration.restoreState == "restored" {
			return nil
		}
		return sql.ErrNoRows
	}
	if status != "queued" && status != "running" {
		return sql.ErrNoRows
	}
	if status == "running" {
		var nativeBackupSteps int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM operation_steps
			WHERE operation_id=? AND domain='native' AND kind='backup' AND status<>'skipped'`, operationID).
			Scan(&nativeBackupSteps); err != nil {
			return err
		}
		if nativeBackupSteps != 0 {
			return sql.ErrNoRows
		}
	}

	targetLastRun := queuedAt
	if restoreErr == nil {
		if restoration.jobID != jobID || restoration.repositoryID != repositoryID ||
			restoration.restoreState != "eligible" {
			return fmt.Errorf("scheduled admission restoration identity is invalid")
		}
		savedFirst, savedLast, err := parseRestorationRange(
			restoration.coalescedMissedCount,
			restoration.firstDeferredDueAt,
			restoration.lastDueAt,
		)
		if err != nil {
			return err
		}
		admittedAt, err := time.Parse(time.RFC3339Nano, restoration.admittedAt)
		if err != nil {
			return fmt.Errorf("scheduled admission time is invalid")
		}
		targetLastRun = admittedAt.UTC().Format(time.RFC3339Nano)

		var pending, currentCount int64
		var currentFirstText, currentLastText string
		if err := tx.QueryRow(`SELECT pending_catchup,coalesced_missed_count,
			first_deferred_due_at,last_due_at
			FROM job_target_schedule_state WHERE job_id=? AND repository_id=?`,
			jobID, repositoryID).Scan(
			&pending, &currentCount, &currentFirstText, &currentLastText); err != nil {
			return err
		}
		mergedCount := restoration.coalescedMissedCount
		mergedFirst, mergedLast := savedFirst, savedLast
		if pending == 1 {
			currentFirst, currentLast, err := parseRestorationRange(
				currentCount, currentFirstText, currentLastText,
			)
			if err != nil {
				return err
			}
			if currentCount > math.MaxInt64-restoration.coalescedMissedCount {
				return fmt.Errorf("scheduled catch-up count overflow")
			}
			mergedCount += currentCount
			if currentFirst.Before(mergedFirst) {
				mergedFirst = currentFirst
			}
			if currentLast.After(mergedLast) {
				mergedLast = currentLast
			}
		} else {
			if pending != 0 || currentCount != 0 || currentFirstText != "" {
				return fmt.Errorf("current scheduled catch-up state is invalid")
			}
			if currentLastText != "" {
				if _, err := time.Parse(time.RFC3339Nano, currentLastText); err != nil {
					return fmt.Errorf("current scheduled catch-up last due time is invalid")
				}
			}
		}
		if _, err := tx.Exec(`UPDATE job_target_schedule_state
			SET pending_catchup=1,coalesced_missed_count=?,first_deferred_due_at=?,
			    last_due_at=?,updated_at=?
			WHERE job_id=? AND repository_id=?`,
			mergedCount, mergedFirst.Format(time.RFC3339Nano),
			mergedLast.Format(time.RFC3339Nano), finishedAt.Format(time.RFC3339Nano),
			jobID, repositoryID); err != nil {
			return err
		}
	}

	operationResult, err := tx.Exec(`UPDATE operations
		SET status='interrupted',finished_at=?
		WHERE id=? AND status IN ('queued','running')`,
		formatSortableTimestamp(finishedAt), operationID)
	if err != nil {
		return err
	}
	if count, _ := operationResult.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	targetResult, err := tx.Exec(`UPDATE backup_job_targets
		SET last_status='interrupted',last_run=?
		WHERE job_id=? AND repository_id=?`,
		targetLastRun, jobID, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := targetResult.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	if restoreErr == nil {
		result, err := tx.Exec(`UPDATE scheduled_operation_restorations
			SET restore_state='restored'
			WHERE operation_id=? AND restore_state='eligible'`, operationID)
		if err != nil {
			return err
		}
		if count, _ := result.RowsAffected(); count != 1 {
			return sql.ErrNoRows
		}
		if err := reconcileScheduledAdmissionGroupsTx(tx, jobID); err != nil {
			return err
		}
	}
	return nil
}

type scheduledGroupState struct {
	id, admittedAt, previousJobLastRun string
	generation, eligible               int64
	anyStarted                         bool
}

func reconcileScheduledAdmissionGroupsTx(tx *sql.Tx, jobID string) error {
	rows, err := tx.Query(`SELECT g.id,g.generation,g.admitted_at,g.previous_job_last_run,
		COALESCE(SUM(CASE WHEN r.restore_state='eligible' THEN 1 ELSE 0 END),0),
		COUNT(r.operation_id),g.any_started
		FROM scheduled_admission_groups g
		LEFT JOIN scheduled_operation_restorations r ON r.group_id=g.id
		WHERE g.job_id=?
		GROUP BY g.id,g.generation,g.admitted_at,g.previous_job_last_run,g.any_started
		ORDER BY g.generation`, jobID)
	if err != nil {
		return err
	}
	defer rows.Close()
	groups := []scheduledGroupState{}
	for rows.Next() {
		var group scheduledGroupState
		var total, anyStarted int64
		if err := rows.Scan(&group.id, &group.generation, &group.admittedAt,
			&group.previousJobLastRun, &group.eligible, &total, &anyStarted); err != nil {
			return err
		}
		group.anyStarted = anyStarted == 1
		if total == 0 && !group.anyStarted {
			return fmt.Errorf("scheduled admission group state is invalid")
		}
		if _, err := time.Parse(time.RFC3339Nano, group.admittedAt); err != nil {
			return fmt.Errorf("scheduled admission group time is invalid")
		}
		if group.previousJobLastRun != "" {
			if _, err := time.Parse(time.RFC3339Nano, group.previousJobLastRun); err != nil {
				return fmt.Errorf("scheduled admission prior job time is invalid")
			}
		}
		if len(groups) > 0 && group.generation <= groups[len(groups)-1].generation {
			return fmt.Errorf("scheduled admission group generation is invalid")
		}
		groups = append(groups, group)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(groups) == 0 {
		return nil
	}
	lastRun := ""
	hasEligible := false
	for _, group := range groups {
		if group.eligible > 0 {
			hasEligible = true
		}
		if group.eligible > 0 || group.anyStarted {
			lastRun = group.admittedAt
		}
	}
	if !hasEligible {
		lastRun = groups[0].previousJobLastRun
		for _, group := range groups {
			if group.anyStarted {
				lastRun = group.admittedAt
			}
		}
	}
	result, err := tx.Exec(`UPDATE backup_jobs SET last_run=? WHERE id=?`, lastRun, jobID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	if !hasEligible {
		if _, err := tx.Exec(`DELETE FROM scheduled_admission_groups WHERE job_id=?`, jobID); err != nil {
			return err
		}
	}
	return nil
}

// AdmitManualBackupTargets admits each explicitly selected, available, idle
// pair in one transaction. It updates observations but never creates, clears,
// or coalesces scheduled catch-up state.
func AdmitManualBackupTargets(db *sql.DB, request ManualAdmissionRequest) ([]TargetAdmissionResult, error) {
	if request.JobID == "" || len(request.Targets) == 0 {
		return nil, ErrInvalidTargets
	}
	releaseDefinition, err := acquireJobDefinitionUse(db, request.JobID)
	if err != nil {
		return nil, err
	}
	defer releaseDefinition()
	now := request.Now.UTC()
	if now.IsZero() {
		return nil, fmt.Errorf("manual admission time is required")
	}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	if err := requireJobConnectionUnreserved(tx, request.JobID); err != nil {
		return nil, err
	}
	var jobName, source, sourceKey, sourceBindingState string
	if err := tx.QueryRow(`SELECT name,source,source_storage_key,source_binding_state
		FROM backup_jobs WHERE id=?`, request.JobID).Scan(
		&jobName, &source, &sourceKey, &sourceBindingState); err != nil {
		return nil, err
	}
	if normalizedJobSourceBindingState(sourceBindingState) == "unbound_imported" {
		return nil, ErrJobSourceUnbound
	}
	if err := validateAvailabilityObservation(request.Source, now); err != nil {
		return nil, err
	}
	if err := validateSourceObservedKey(request.Source, sourceKey); err != nil {
		return nil, err
	}
	selected, err := observationMap(request.Targets)
	if err != nil {
		return nil, err
	}
	resolvedSource := request.Source.ResolvedPath
	if request.Source.State != StorageAvailable {
		resolvedSource = source
	}
	if err := setResolvedSourcePathTx(tx, request.JobID, source, resolvedSource, request.Source.CheckedAt); err != nil {
		return nil, err
	}
	targets, err := loadScheduledTargets(tx, request.JobID)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]scheduledTargetRow, len(targets))
	for _, target := range targets {
		byID[target.repositoryID] = target
	}
	results := make([]TargetAdmissionResult, 0, len(selected))
	ids := make([]string, 0, len(selected))
	for repositoryID := range selected {
		ids = append(ids, repositoryID)
	}
	sort.Strings(ids)
	for _, repositoryID := range ids {
		target, ok := byID[repositoryID]
		if !ok {
			return nil, sql.ErrNoRows
		}
		observation := selected[repositoryID]
		if err := validateAvailabilityObservation(observation, now); err != nil {
			return nil, err
		}
		// Full admission owns destination proof and verified alias publication.
		if err := updatePairObservations(tx, request.JobID, repositoryID, request.Source, observation, now); err != nil {
			return nil, err
		}
		result := TargetAdmissionResult{RepositoryID: repositoryID}
		if request.Source.State != StorageAvailable && !request.Source.ConclusiveFailure {
			result.Status = AdmissionStorageUnavailable
			result.ReasonCode = request.Source.ReasonCode
			results = append(results, result)
			continue
		}
		if observation.State != StorageAvailable && !request.Source.ConclusiveFailure && !observation.ConclusiveFailure {
			result.Status = AdmissionStorageUnavailable
			result.ReasonCode = observation.ReasonCode
			results = append(results, result)
			continue
		}
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM operations
			WHERE job_id=? AND repository_id=? AND status IN ('queued','running')`,
			request.JobID, repositoryID).Scan(&active); err != nil {
			return nil, err
		}
		if active != 0 {
			result.Status = AdmissionBusy
			results = append(results, result)
			continue
		}
		if err := requireKopiaPolicyReady(tx, repositoryID); err != nil {
			if errors.Is(err, ErrKopiaPolicyNotReady) {
				result.Status = AdmissionPolicyNotReady
				result.ReasonCode = "kopia_policy_not_ready"
				results = append(results, result)
				continue
			}
			return nil, err
		}
		result.OperationID, err = queueAdmittedPair(tx, request.JobID, jobName, source,
			repositoryID, target.repositoryName, target.engine, now)
		if err != nil {
			return nil, err
		}
		result.Status = AdmissionAdmittedRegular
		result.RequiresSourceFailureCheck = request.Source.ConclusiveFailure
		if request.Source.ConclusiveFailure {
			result.ReasonCode = request.Source.ReasonCode
		} else if observation.ConclusiveFailure {
			result.RequiresTargetFailureCheck = true
			result.ReasonCode = observation.ReasonCode
		}
		results = append(results, result)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return results, nil
}

func ListPendingCatchUps(db *sql.DB) ([]PendingCatchUp, error) {
	rows, err := db.Query(`SELECT s.job_id,s.repository_id,s.coalesced_missed_count,
		s.first_deferred_due_at,s.last_due_at,s.source_availability,s.source_reason_code,
		s.source_checked_at,s.target_availability,s.target_reason_code,s.target_checked_at,
		j.enabled,j.schedule
		FROM job_target_schedule_state s
		JOIN backup_jobs j ON j.id=s.job_id
		WHERE s.pending_catchup=1
		ORDER BY s.job_id,s.repository_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []PendingCatchUp{}
	for rows.Next() {
		var value PendingCatchUp
		if err := rows.Scan(&value.JobID, &value.RepositoryID, &value.CoalescedMissedCount,
			&value.FirstDeferredDueAt, &value.LastDueAt, &value.SourceAvailability,
			&value.SourceReasonCode, &value.SourceCheckedAt, &value.TargetAvailability,
			&value.TargetReasonCode, &value.TargetCheckedAt, &value.JobEnabled,
			&value.Schedule); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}
