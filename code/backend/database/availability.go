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

	// AvailabilityReasonUnavailableTooLong marks the one failed operation
	// raised when an enabled job's storage has stayed unavailable for
	// UnavailableIssueAfter since the first missed scheduled run. It is an
	// admission result reason only, never a persisted availability reason.
	AvailabilityReasonUnavailableTooLong = "unavailable_30_days"
)

// UnavailableIssueAfter is how long a paused scheduled occurrence may wait
// before the user gets one failed operation (and the ordinary failure
// notification) about it. Pausing itself sends nothing, so without this a
// share deleted on the server would look paused forever.
//
// The trigger is a missed occurrence of an enabled scheduled backup job, so
// manual-only jobs and a vault with no enabled scheduled backup job never
// raise it: the vault's own scheduled checks and maintenance only pause (see
// scheduler/repository_tasks.go). That is intentional; there is no scheduled
// run whose absence the user needs to hear about.
const UnavailableIssueAfter = 30 * 24 * time.Hour

const (
	AdmissionAdmittedRegular    = "admitted_regular"
	AdmissionAdmittedCatchUp    = "admitted_catchup"
	AdmissionStorageUnavailable = "storage_unavailable"
	AdmissionBusy               = "busy"
	AdmissionPolicyNotReady     = "policy_not_ready"
	AdmissionPaused             = "paused"
	AdmissionNoPending          = "no_pending"
	// AdmissionUnavailableIssue queues the one failed operation for storage
	// that has been unavailable for UnavailableIssueAfter.
	AdmissionUnavailableIssue = "unavailable_issue"
)

type StorageAvailabilityObservation struct {
	State      string
	ReasonCode string
	CheckedAt  time.Time
	// ConclusiveFailure only steers this run and is never persisted. It lets a real
	// storage validation error show up as a failed attempt through the normal
	// executor, with no extra status or retry state.
	ConclusiveFailure bool
	// Conversion is set only for an available source whose binding is still
	// in the legacy format; the admission transaction applies it. Admission
	// never writes the alias (resolved_source_path): there is no automatic
	// relocation, and only the user sets the alias.
	Conversion *SourceBindingConversion
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
			value.ReasonCode != AvailabilityReasonIdentityMismatch &&
			value.ReasonCode != AvailabilityReasonStorageMissing {
			return fmt.Errorf("unknown storage availability reason is invalid")
		}
	default:
		return fmt.Errorf("storage availability state is invalid")
	}
	// A folder that is missing while its recorded mount point is still
	// present is a conclusive failure (deleted or moved), so storage_missing
	// is a valid conclusive reason alongside identity mismatch and observation failure.
	if value.ConclusiveFailure && (value.State != StorageUnknown ||
		value.ReasonCode != AvailabilityReasonIdentityMismatch &&
			value.ReasonCode != AvailabilityReasonObservationFailed &&
			value.ReasonCode != AvailabilityReasonStorageMissing) {
		return fmt.Errorf("conclusive storage failure classification is invalid")
	}
	if value.Conversion != nil && value.State != StorageAvailable {
		return fmt.Errorf("source binding conversion requires an available observation")
	}
	return nil
}

// applySourceObservation applies a source observation to the job row. The only
// change it can make is the one-time conversion of a legacy binding; it never
// writes the job's alias.
func applySourceObservation(tx *sql.Tx, jobID string, source StorageAvailabilityObservation) error {
	if source.Conversion == nil {
		return nil
	}
	return convertLegacySourceBindingTx(tx, jobID, *source.Conversion)
}

func unavailableIssueDue(firstDeferredDueAt string, now time.Time) bool {
	first, err := time.Parse(time.RFC3339Nano, firstDeferredDueAt)
	return err == nil && !now.Before(first.Add(UnavailableIssueAfter))
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

// RestorePreNativeUnavailable puts back a scheduled occurrence only when its
// full destination check definitely stopped it before the native backup
// started. It reuses the coalesced pair state; there is no retry counter,
// timer row, or remote coordination.
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

// ConsumeOverdueUnavailableOccurrence turns a pre-native filesystem pause
// into the 30-day issue when the paused occurrence has already waited
// UnavailableIssueAfter. It returns false, changing nothing, when the
// operation holds no eligible scheduled occurrence, the job is disabled, or
// the first missed run is newer than that; the caller then restores the
// occurrence as an ordinary pause.
//
// Why this exists: the scheduler raises the 30-day issue only from its own
// preliminary probe (CoordinateScheduledAdmission). A vault can pass that
// probe and then time out every time under the vault lock (marker read,
// protected root, native validation on a hung share), and a source can pass
// it and then fail the final check at the child boundary. Without this, such
// an occurrence would be restored silently on every tick and the user would
// never hear about it.
//
// On escalation the occurrence is consumed (same bookkeeping as
// ConsumeScheduledBackupOccurrence) and the pair's pending state is cleared
// so the 30-day count restarts. Each job/vault pair escalates on its own,
// even for a source outage. The scheduler path raises one issue per job for
// a source outage, but a source pause that shows up only here, on a job with
// several vaults, can raise one 30-day issue per vault. That needs a source
// that keeps passing the probe and failing the final check for 30 days, and
// a duplicate notice then is cheaper than coordinating sibling operations
// that are already queued or running, so it is accepted.
func ConsumeOverdueUnavailableOccurrence(db *sql.DB, operationID, reason string, now time.Time, source bool) (bool, error) {
	if operationID == "" || now.IsZero() {
		return false, ErrInvalidTargets
	}
	now = now.UTC()
	tx, err := db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()
	var groupID, jobID, repositoryID, firstDue, lastDue, restoreState string
	var missed int64
	err = tx.QueryRow(`SELECT group_id,job_id,repository_id,coalesced_missed_count,
		first_deferred_due_at,last_due_at,restore_state
		FROM scheduled_operation_restorations WHERE operation_id=?`, operationID).
		Scan(&groupID, &jobID, &repositoryID, &missed, &firstDue, &lastDue, &restoreState)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil || restoreState != "eligible" {
		return false, err
	}
	savedFirst, _, err := parseRestorationRange(missed, firstDue, lastDue)
	if err != nil {
		return false, err
	}
	var enabled bool
	if err := tx.QueryRow(`SELECT enabled FROM backup_jobs WHERE id=?`, jobID).Scan(&enabled); err != nil {
		return false, err
	}
	if !enabled || !unavailableIssueDue(savedFirst.Format(time.RFC3339Nano), now) {
		return false, nil
	}
	if _, err := tx.Exec(`UPDATE scheduled_operation_restorations SET restore_state='started'
		WHERE operation_id=? AND restore_state='eligible'`, operationID); err != nil {
		return false, err
	}
	if _, err := tx.Exec(`UPDATE scheduled_admission_groups SET any_started=1
		WHERE id=? AND job_id=?`, groupID, jobID); err != nil {
		return false, err
	}
	stamp := now.Format(time.RFC3339Nano)
	availabilityColumns := "target_availability=?,target_reason_code=?,target_checked_at=?"
	if source {
		availabilityColumns = "source_availability=?,source_reason_code=?,source_checked_at=?"
	}
	if _, err := tx.Exec(`UPDATE job_target_schedule_state SET
		pending_catchup=0,coalesced_missed_count=0,first_deferred_due_at='',`+
		availabilityColumns+`,updated_at=? WHERE job_id=? AND repository_id=?`,
		StorageUnavailable, reason, stamp, stamp, jobID, repositoryID); err != nil {
		return false, err
	}
	if err := reconcileScheduledAdmissionGroupsTx(tx, jobID); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// ScheduledOccurrenceEligible reports whether the operation still holds an
// eligible scheduled occurrence, i.e. a restore would put it back as a pending
// catch-up. The runner asks before restoring, because a successful restore
// can remove the restoration row together with its admission group. It uses
// the answer to skip the failure notification for a pause: pauses are shown
// as "source unavailable" / "vault unavailable" status, and the user is only
// notified once storage has been unavailable for UnavailableIssueAfter.
func ScheduledOccurrenceEligible(db *sql.DB, operationID string) (bool, error) {
	var state string
	err := db.QueryRow(`SELECT restore_state FROM scheduled_operation_restorations WHERE operation_id=?`,
		operationID).Scan(&state)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return state == "eligible", err
}

// ConsumeScheduledBackupOccurrence marks the occurrence's restoration row as
// started (no longer restorable) only after this process reached the point of
// launching the requested native backup, or the
// operation ended for any reason other than storage being confirmed unavailable
// before that backup. Manual operations have no restoration row.
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
	return queueOperationRow(tx, jobID, jobName, repositoryID, repositoryName, engine, now)
}

// queueOperationRow inserts one queued backup operation. The unavailable-too-
// long issue uses it directly because that operation never reaches native
// work, so Kopia policy readiness is irrelevant to it.
func queueOperationRow(
	tx *sql.Tx,
	jobID, jobName, repositoryID, repositoryName, engine string,
	now time.Time,
) (string, error) {
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
	var jobName, source, schedule, nextRun, lastRun string
	var enabled bool
	if err := tx.QueryRow(`SELECT name,source,schedule,
		COALESCE(next_run,''),COALESCE(last_run,''),enabled
		FROM backup_jobs WHERE id=?`, request.JobID).Scan(
		&jobName, &source, &schedule, &nextRun, &lastRun, &enabled); err != nil {
		return nil, err
	}
	if err := applySourceObservation(tx, request.JobID, request.Source); err != nil {
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
		// Preliminary target observations only record availability. A vault
		// always runs at its registered location; destination proof happens
		// later, once per operation, under the vault lock.
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
	// A vault whose removal is queued or running counts as busy: the pair keeps
	// its pending occurrence and nothing is queued, reported or notified, as for
	// any busy pair. Once the removal commits, the target is gone with it; if
	// the removal fails, the next tick admits the pending occurrence.
	pairBusy := func(repositoryID string) (bool, error) {
		var active int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM operations
			WHERE job_id=? AND repository_id=? AND status IN ('queued','running')`,
			request.JobID, repositoryID).Scan(&active); err != nil {
			return false, err
		}
		if active != 0 {
			return true, nil
		}
		return vaultRemovalPending(tx, repositoryID)
	}
	sourceOutage := request.Source.State != StorageAvailable && !request.Source.ConclusiveFailure
	// A source outage is reported once per job, carried by the first idle
	// pair whose pending occurrence has waited UnavailableIssueAfter. Every
	// overdue pending pair of the job is consumed with it so the count
	// restarts for all of them. If every overdue pair is busy, nothing is
	// raised or consumed on this tick.
	sourceIssueCarrier := ""
	if enabled && sourceOutage {
		for _, target := range resultTargets {
			if target.pending == 0 || !unavailableIssueDue(target.firstDeferredDueAt, now) {
				continue
			}
			busy, err := pairBusy(target.repositoryID)
			if err != nil {
				return nil, err
			}
			if !busy {
				sourceIssueCarrier = target.repositoryID
				break
			}
		}
	}
	queueRestorable := func(target scheduledTargetRow, requirePolicy bool) (string, error) {
		var operationID string
		var err error
		if requirePolicy {
			operationID, err = queueAdmittedPair(tx, request.JobID, jobName, source,
				target.repositoryID, target.repositoryName, target.engine, now)
		} else {
			operationID, err = queueOperationRow(tx, request.JobID, jobName,
				target.repositoryID, target.repositoryName, target.engine, now)
		}
		if err != nil {
			return "", err
		}
		if groupID == "" {
			groupID = uuid.NewString()
			if _, err := tx.Exec(`INSERT INTO scheduled_admission_groups
				(id,job_id,generation,admitted_at,previous_job_last_run)
				SELECT ?,?,COALESCE(MAX(generation),0)+1,?,?
				FROM scheduled_admission_groups WHERE job_id=?`,
				groupID, request.JobID, now.Format(time.RFC3339Nano), lastRun, request.JobID); err != nil {
				return "", err
			}
		}
		if target.missed <= 0 || target.firstDeferredDueAt == "" || target.lastDueAt == "" {
			return "", fmt.Errorf("scheduled admission restoration state is invalid")
		}
		if _, _, err := parseRestorationRange(
			target.missed, target.firstDeferredDueAt, target.lastDueAt,
		); err != nil {
			return "", err
		}
		if _, err := tx.Exec(`INSERT INTO scheduled_operation_restorations
			(operation_id,group_id,job_id,repository_id,coalesced_missed_count,
			 first_deferred_due_at,last_due_at)
			VALUES(?,?,?,?,?,?,?)`,
			operationID, groupID, request.JobID, target.repositoryID, target.missed,
			target.firstDeferredDueAt, target.lastDueAt); err != nil {
			return "", err
		}
		if err := clearPairPending(tx, request.JobID, target.repositoryID, now); err != nil {
			return "", err
		}
		admitted = true
		return operationID, nil
	}
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
		if sourceOutage {
			result.Status = AdmissionStorageUnavailable
			result.ReasonCode = request.Source.ReasonCode
			if sourceIssueCarrier != "" && unavailableIssueDue(target.firstDeferredDueAt, now) {
				if target.repositoryID == sourceIssueCarrier {
					// Queued through the ordinary operation path so the failure,
					// its log, Issues entry, and notification come from the same
					// code as any failed backup. The executor fails it before
					// admission, hooks, or native work.
					operationID, err := queueRestorable(target, false)
					if err != nil {
						return nil, err
					}
					result.Status = AdmissionUnavailableIssue
					result.OperationID = operationID
					result.RequiresSourceFailureCheck = true
					result.ReasonCode = AvailabilityReasonUnavailableTooLong
				} else if err := clearPairPending(tx, request.JobID, target.repositoryID, now); err != nil {
					return nil, err
				}
			}
			results = append(results, result)
			continue
		}
		if observation.State != StorageAvailable && !request.Source.ConclusiveFailure && !observation.ConclusiveFailure {
			result.Status = AdmissionStorageUnavailable
			result.ReasonCode = observation.ReasonCode
			if unavailableIssueDue(target.firstDeferredDueAt, now) {
				// A vault outage is reported per job and vault.
				busy, err := pairBusy(target.repositoryID)
				if err != nil {
					return nil, err
				}
				if !busy {
					operationID, err := queueRestorable(target, false)
					if err != nil {
						return nil, err
					}
					result.Status = AdmissionUnavailableIssue
					result.OperationID = operationID
					result.RequiresTargetFailureCheck = true
					result.ReasonCode = AvailabilityReasonUnavailableTooLong
				}
			}
			results = append(results, result)
			continue
		}
		busy, err := pairBusy(target.repositoryID)
		if err != nil {
			return nil, err
		}
		if busy {
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
		operationID, err := queueRestorable(target, true)
		if err != nil {
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
	var jobName, source, sourceBindingState string
	if err := tx.QueryRow(`SELECT name,source,source_binding_state
		FROM backup_jobs WHERE id=?`, request.JobID).Scan(
		&jobName, &source, &sourceBindingState); err != nil {
		return nil, err
	}
	if normalizedJobSourceBindingState(sourceBindingState) == "unbound_imported" {
		return nil, ErrJobSourceUnbound
	}
	if err := validateAvailabilityObservation(request.Source, now); err != nil {
		return nil, err
	}
	selected, err := observationMap(request.Targets)
	if err != nil {
		return nil, err
	}
	if err := applySourceObservation(tx, request.JobID, request.Source); err != nil {
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
	removingTargets := 0
	for _, repositoryID := range ids {
		target, ok := byID[repositoryID]
		if !ok {
			return nil, sql.ErrNoRows
		}
		observation := selected[repositoryID]
		if err := validateAvailabilityObservation(observation, now); err != nil {
			return nil, err
		}
		// Destination proof happens later, in full admission under the vault
		// lock; this only records the preliminary availability observation.
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
		// A vault whose removal is pending takes no new backup. In a run of
		// several vaults that target alone is reported busy (with the
		// vault_being_removed reason) and the other vaults still start, the way
		// scheduled admission holds back that pair; rolling the whole run back
		// would stop backups to vaults that have nothing to do with the
		// removal. Only a run in which every selected vault is being removed is
		// refused outright, below, so a single-vault run keeps its "This vault
		// is being removed." answer.
		if removing, err := vaultRemovalPending(tx, repositoryID); err != nil {
			return nil, err
		} else if removing {
			removingTargets++
			result.Status = AdmissionBusy
			result.ReasonCode = vaultBeingRemovedReason
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
	if removingTargets == len(ids) {
		return nil, ErrVaultBeingRemoved
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
