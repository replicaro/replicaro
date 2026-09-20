package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
)

var ErrKopiaPolicyNotReady = errors.New("Kopia policy reconciliation is not ready")
var ErrKopiaPolicyRetryUnavailable = errors.New("Kopia policy reconciliation is not in an error state")

type KopiaPolicyState struct {
	RepositoryID     string
	DesiredDigest    string
	AppliedDigest    string
	State            string
	LastError        string
	LastReconciledAt string
	ActiveFencePath  string
	ProofKind        string
}

type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

func DeriveKopiaPolicyDesired(db queryer, repositoryID string) (engines.KopiaManagedPolicyDesiredState, error) {
	var engine, connector, maintenanceSchedule, objectLockJSON string
	if err := db.QueryRow(`SELECT engine,connector,maintenance_schedule,object_lock_json FROM repositories WHERE id=?`, repositoryID).
		Scan(&engine, &connector, &maintenanceSchedule, &objectLockJSON); err != nil {
		return engines.KopiaManagedPolicyDesiredState{}, err
	}
	if engine != engines.KopiaID {
		return engines.KopiaManagedPolicyDesiredState{}, fmt.Errorf("repository is not a Kopia vault")
	}
	rows, err := db.Query(`
		SELECT j.id,j.source,j.excludes,j.retention,j.retention_hourly,
		       j.retention_daily,j.retention_weekly,j.retention_monthly,j.retention_yearly
		FROM backup_jobs j
		JOIN backup_job_targets t ON t.job_id=j.id
		WHERE t.repository_id=?
		ORDER BY j.id`, repositoryID)
	if err != nil {
		return engines.KopiaManagedPolicyDesiredState{}, err
	}
	defer rows.Close()
	var objectLock models.ObjectLockSettings
	if err := json.Unmarshal([]byte(objectLockJSON), &objectLock); err != nil {
		return engines.KopiaManagedPolicyDesiredState{}, fmt.Errorf("decode Kopia object lock settings: %w", err)
	}
	objectLock, err = models.NormalizeObjectLock(engine, connector, objectLock)
	if err != nil {
		return engines.KopiaManagedPolicyDesiredState{}, err
	}
	desired := engines.KopiaManagedPolicyDesiredState{Version: 1,
		MaintenanceSchedule: maintenanceSchedule, ObjectLock: objectLock}
	for rows.Next() {
		var jobUUID, source, exclusions string
		var latest int
		var hourly, daily, weekly, monthly, yearly sql.NullInt64
		if err := rows.Scan(&jobUUID, &source, &exclusions, &latest, &hourly,
			&daily, &weekly, &monthly, &yearly); err != nil {
			return engines.KopiaManagedPolicyDesiredState{}, err
		}
		value := func(field sql.NullInt64) int {
			if latest == 0 || !field.Valid {
				return 0
			}
			return int(field.Int64)
		}
		desired.Sources = append(desired.Sources, engines.KopiaManagedPolicySource{
			JobUUID: jobUUID, Source: source, Excludes: strings.Split(exclusions, "\n"),
			KeepLatest: latest, KeepHourly: value(hourly), KeepDaily: value(daily),
			KeepWeekly: value(weekly), KeepMonthly: value(monthly), KeepAnnual: value(yearly),
		})
	}
	if err := rows.Err(); err != nil {
		return engines.KopiaManagedPolicyDesiredState{}, err
	}
	return engines.NormalizeKopiaManagedPolicyDesired(desired)
}

func RefreshKopiaPolicyStatesTx(tx *sql.Tx, repositoryIDs []string) ([]string, error) {
	ids := uniqueSorted(repositoryIDs)
	changed := []string{}
	for _, repositoryID := range ids {
		var engine string
		err := tx.QueryRow(`SELECT engine FROM repositories WHERE id=?`, repositoryID).Scan(&engine)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if engine != engines.KopiaID {
			continue
		}
		desired, err := DeriveKopiaPolicyDesired(tx, repositoryID)
		if err != nil {
			return nil, err
		}
		digest, err := engines.KopiaManagedPolicyDigest(desired)
		if err != nil {
			return nil, err
		}
		var prior string
		err = tx.QueryRow(`SELECT desired_digest FROM kopia_policy_state WHERE repository_id=?`, repositoryID).Scan(&prior)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec(`INSERT INTO kopia_policy_state
				(repository_id,desired_digest,applied_digest,state)
				VALUES (?,?,'','dirty')`, repositoryID, digest); err != nil {
				return nil, err
			}
			changed = append(changed, repositoryID)
		case err != nil:
			return nil, err
		case prior != digest:
			if _, err := tx.Exec(`UPDATE kopia_policy_state
				SET desired_digest=?,
					state=CASE WHEN active_fence_path<>'' THEN 'applying' ELSE 'dirty' END,
					last_error=CASE WHEN active_fence_path<>'' THEN last_error ELSE '' END,
					active_fence_path=CASE WHEN active_fence_path<>'' THEN active_fence_path ELSE '' END,
					proof_kind=CASE WHEN active_fence_path<>'' THEN proof_kind ELSE '' END
				WHERE repository_id=?`, digest, repositoryID); err != nil {
				return nil, err
			}
			changed = append(changed, repositoryID)
		}
	}
	return changed, nil
}

// MarkAllKopiaPoliciesVerificationRequired synchronously removes startup
// readiness before schedulers or API handlers can admit Kopia backups.
// An in-progress row retains its durable native-process fence so recovery can
// prove the prior process inactive before any new mutation.
func MarkAllKopiaPoliciesVerificationRequired(db *sql.DB) ([]string, error) {
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT id FROM repositories WHERE engine='kopia' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, id := range ids {
		desired, err := DeriveKopiaPolicyDesired(tx, id)
		if err != nil {
			return nil, err
		}
		digest, err := engines.KopiaManagedPolicyDigest(desired)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`INSERT INTO kopia_policy_state
			(repository_id,desired_digest,applied_digest,state)
			VALUES (?,?,'','dirty')
			ON CONFLICT(repository_id) DO UPDATE SET
				desired_digest=excluded.desired_digest,
				state=CASE WHEN kopia_policy_state.active_fence_path<>'' THEN 'applying' ELSE 'dirty' END,
				last_error=CASE WHEN kopia_policy_state.active_fence_path<>''
					THEN kopia_policy_state.last_error ELSE '' END,
				active_fence_path=CASE WHEN kopia_policy_state.active_fence_path<>''
					THEN kopia_policy_state.active_fence_path ELSE '' END,
				proof_kind=CASE WHEN kopia_policy_state.active_fence_path<>''
					THEN kopia_policy_state.proof_kind ELSE '' END`,
			id, digest); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

func InitializeKopiaPolicyStateTx(tx *sql.Tx, repositoryID, digest string, ready bool) error {
	if len(digest) != 64 {
		return fmt.Errorf("Kopia desired digest is invalid")
	}
	state, applied := "dirty", ""
	if ready {
		state, applied = "ready", digest
	}
	_, err := tx.Exec(`INSERT INTO kopia_policy_state
		(repository_id,desired_digest,applied_digest,state,last_error,last_reconciled_at)
		VALUES (?,?,?,?,?,?)
		ON CONFLICT(repository_id) DO UPDATE SET
			desired_digest=excluded.desired_digest,
			applied_digest=excluded.applied_digest,
			state=excluded.state,
			last_error='',
			last_reconciled_at=excluded.last_reconciled_at,
			active_fence_path='',
			proof_kind=''`,
		repositoryID, digest, applied, state, "",
		time.Now().UTC().Format(time.RFC3339Nano))
	return err
}

func GetKopiaPolicyState(db *sql.DB, repositoryID string) (KopiaPolicyState, error) {
	return getKopiaPolicyState(db, repositoryID)
}

func getKopiaPolicyState(db queryer, repositoryID string) (KopiaPolicyState, error) {
	var value KopiaPolicyState
	err := db.QueryRow(`SELECT repository_id,desired_digest,applied_digest,state,
		last_error,last_reconciled_at,active_fence_path,proof_kind
		FROM kopia_policy_state WHERE repository_id=?`, repositoryID).Scan(
		&value.RepositoryID, &value.DesiredDigest, &value.AppliedDigest, &value.State,
		&value.LastError, &value.LastReconciledAt, &value.ActiveFencePath, &value.ProofKind)
	return value, err
}

func RequireKopiaPolicyReady(db *sql.DB, repositoryID string) error {
	return requireKopiaPolicyReady(db, repositoryID)
}

// MarkKopiaPolicyVerificationRequired closes backup admission before an exact
// managed-policy cleanup can expose snapshots to a different inheritance path.
// An active durable fence is never displaced by this local transition.
func MarkKopiaPolicyVerificationRequired(db *sql.DB, repositoryID string) error {
	result, err := db.Exec(`UPDATE kopia_policy_state
		SET state='dirty',applied_digest='',last_error=''
		WHERE repository_id=? AND state<>'applying' AND active_fence_path=''`, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("%w: policy verification cannot be closed while reconciliation is active", ErrKopiaPolicyNotReady)
	}
	return nil
}

func requireKopiaPolicyReady(db queryer, repositoryID string) error {
	var engine string
	if err := db.QueryRow(`SELECT engine FROM repositories WHERE id=?`, repositoryID).Scan(&engine); err != nil {
		return err
	}
	if engine != engines.KopiaID {
		return nil
	}
	state, err := getKopiaPolicyState(db, repositoryID)
	if err != nil {
		return fmt.Errorf("%w: state is missing", ErrKopiaPolicyNotReady)
	}
	if state.State != "ready" || state.DesiredDigest == "" || state.AppliedDigest != state.DesiredDigest {
		if state.LastError != "" {
			return fmt.Errorf("%w: %s", ErrKopiaPolicyNotReady, state.LastError)
		}
		return fmt.Errorf("%w: reconciliation is %s", ErrKopiaPolicyNotReady, state.State)
	}
	return nil
}

func BeginKopiaPolicyApplication(db *sql.DB, repositoryID, desiredDigest, fencePath, proofKind string) error {
	result, err := db.Exec(`UPDATE kopia_policy_state
		SET state='applying',last_error='',active_fence_path=?,proof_kind=?
		WHERE repository_id=? AND desired_digest=? AND state<>'applying'`,
		fencePath, proofKind, repositoryID, desiredDigest)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("%w: desired state changed or another reconciliation is active", ErrKopiaPolicyNotReady)
	}
	return nil
}

func ResetInterruptedKopiaPolicyApplication(
	db *sql.DB,
	repositoryID, desiredDigest, priorFencePath string,
) error {
	result, err := db.Exec(`UPDATE kopia_policy_state
		SET state='dirty',
			last_error='Application stopped before Kopia policy readback was durably recorded.',
			active_fence_path='',
			proof_kind=''
		WHERE repository_id=? AND desired_digest=? AND state IN ('applying','dirty','error')
			AND active_fence_path=?`,
		repositoryID, desiredDigest, priorFencePath)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("%w: interrupted application state changed during fence recovery", ErrKopiaPolicyNotReady)
	}
	return nil
}

func MarkKopiaPolicyFenceCleanupError(
	db *sql.DB,
	repositoryID, desiredDigest, fencePath, proofKind string,
	cleanupErr error,
) error {
	if cleanupErr == nil || fencePath == "" || proofKind == "" {
		return fmt.Errorf("Kopia fence cleanup failure is incomplete")
	}
	result, err := db.Exec(`UPDATE kopia_policy_state
		SET state='error',applied_digest='',last_error=?,
			active_fence_path=?,proof_kind=?
		WHERE repository_id=? AND desired_digest=? AND state IN ('ready','dirty','error')`,
		"Native process fence cleanup failed: "+cleanupErr.Error(),
		fencePath, proofKind, repositoryID, desiredDigest)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("Kopia policy state changed before fence cleanup failure was recorded")
	}
	return nil
}

func FinishKopiaPolicyApplication(
	db *sql.DB,
	repositoryID, desiredDigest string,
	reconcileErr error,
	preserveVerifiedReadiness bool,
	mutationStarted bool,
	driftObserved bool,
) error {
	state, applied, lastError := "ready", desiredDigest, ""
	if reconcileErr != nil {
		if preserveVerifiedReadiness && !mutationStarted && !driftObserved {
			state, applied = "ready", desiredDigest
		} else {
			state, applied, lastError = "error", "", reconcileErr.Error()
		}
	}
	result, err := db.Exec(`UPDATE kopia_policy_state
		SET state=?,applied_digest=?,last_error=?,last_reconciled_at=?,
			active_fence_path='',proof_kind=''
		WHERE repository_id=? AND desired_digest=?`,
		state, applied, lastError, time.Now().UTC().Format(time.RFC3339Nano),
		repositoryID, desiredDigest)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("Kopia desired state changed before reconciliation completed")
	}
	return nil
}

func MarkKopiaPolicyReconciliationError(
	db *sql.DB,
	repositoryID, desiredDigest string,
	reconcileErr error,
) error {
	if reconcileErr == nil {
		return fmt.Errorf("Kopia reconciliation error is required")
	}
	result, err := db.Exec(`UPDATE kopia_policy_state
		SET state='error',applied_digest='',last_error=?,last_reconciled_at=?,
			active_fence_path='',proof_kind=''
		WHERE repository_id=? AND desired_digest=? AND state='dirty'`,
		reconcileErr.Error(), time.Now().UTC().Format(time.RFC3339Nano),
		repositoryID, desiredDigest)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return fmt.Errorf("Kopia desired state changed before reconciliation error was recorded")
	}
	return nil
}

// RetryKopiaPolicyForTarget converts only the exact current terminal error for
// a Kopia job target back to dirty. It neither changes the desired digest nor
// touches an applying row's durable process fence.
func RetryKopiaPolicyForTarget(db *sql.DB, jobID, repositoryID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var engine string
	if err := tx.QueryRow(`
		SELECT r.engine
		FROM backup_job_targets t
		JOIN repositories r ON r.id=t.repository_id
		WHERE t.job_id=? AND t.repository_id=?`,
		jobID, repositoryID).Scan(&engine); err != nil {
		return err
	}
	if engine != engines.KopiaID {
		return fmt.Errorf("job target is not a Kopia vault")
	}
	if err := retryKopiaPolicyForRepositoryTx(tx, repositoryID); err != nil {
		return err
	}
	return tx.Commit()
}

// RetryKopiaPolicyForRepository reopens only the exact current terminal state.
// Vault-care resubmission uses this for empty enrolled vaults that have no job
// target through which the older retry endpoint could be reached.
func RetryKopiaPolicyForRepository(db *sql.DB, repositoryID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := retryKopiaPolicyForRepositoryTx(tx, repositoryID); err != nil {
		return err
	}
	return tx.Commit()
}

func retryKopiaPolicyForRepositoryTx(tx *sql.Tx, repositoryID string) error {
	var engine, state string
	if err := tx.QueryRow(`SELECT r.engine,k.state
		FROM repositories r JOIN kopia_policy_state k ON k.repository_id=r.id
		WHERE r.id=?`, repositoryID).Scan(&engine, &state); err != nil {
		return err
	}
	if engine != engines.KopiaID {
		return fmt.Errorf("repository is not a Kopia vault")
	}
	if state != "error" {
		return fmt.Errorf("%w: current state is %s", ErrKopiaPolicyRetryUnavailable, state)
	}
	// A fence-cleanup error deliberately retains its durable proof. Reopen that
	// exact error as applying, not ordinary dirty work, so the manager keeps it
	// on the interrupted-process recovery path until inactivity is proven.
	result, err := tx.Exec(`UPDATE kopia_policy_state
		SET state=CASE WHEN active_fence_path<>'' THEN 'applying' ELSE 'dirty' END,
			last_error=''
		WHERE repository_id=? AND state='error'`, repositoryID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return ErrKopiaPolicyRetryUnavailable
	}
	return nil
}

func ListKopiaRepositories(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`SELECT id FROM repositories WHERE engine='kopia' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		result = append(result, id)
	}
	return result, rows.Err()
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}
