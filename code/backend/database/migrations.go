package database

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/operationlog"
)

// Schema 70 binds the local/network identity contract to descriptor/helper v4.
// Reject older databases explicitly; never reinterpret fixed/removable bindings
// or delete an installation as an automatic compatibility step.
const CurrentSchemaVersion = 70

var ErrDatabaseResetRequired = errors.New("database schema is incompatible; delete replicaro.db and restart")

// Migrate initializes only the current schema. Older application databases
// are rejected without migration, import, or metadata-cache backfill.
func Migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return err
	}
	if version == 0 {
		var existing int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_schema
			WHERE name NOT GLOB 'sqlite_*'`).Scan(&existing); err != nil {
			return err
		}
		if existing > 0 {
			return ErrDatabaseResetRequired
		}
	} else if version != CurrentSchemaVersion {
		return fmt.Errorf("%w (found version %d, expected %d)", ErrDatabaseResetRequired, version, CurrentSchemaVersion)
	}

	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	schema := `
	CREATE TABLE IF NOT EXISTS repositories (
		database_id INTEGER PRIMARY KEY AUTOINCREMENT,
		id TEXT NOT NULL UNIQUE,
		name TEXT NOT NULL COLLATE NOCASE UNIQUE,
		engine TEXT NOT NULL CHECK (engine IN ('restic', 'kopia')),
		connector TEXT NOT NULL,
		cold_storage INTEGER NOT NULL DEFAULT 0 CHECK (cold_storage IN (0,1)),
		archive_write_class TEXT NOT NULL DEFAULT '',
		location TEXT NOT NULL,
		canonical_identity TEXT NOT NULL,
		storage_identity_version TEXT NOT NULL DEFAULT '',
		storage_identity_key TEXT NOT NULL DEFAULT '',
		storage_identity_json TEXT NOT NULL DEFAULT '',
		resolved_repository_path TEXT NOT NULL DEFAULT '',
		resolved_repository_observed_at TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		passphrase TEXT NOT NULL DEFAULT '',
		pending_passphrase TEXT NOT NULL DEFAULT '',
		connector_options TEXT NOT NULL DEFAULT '{}',
		native_repository_id TEXT NOT NULL,
		profile_uuid TEXT NOT NULL,
		attachment_generation INTEGER NOT NULL CHECK (attachment_generation > 0),
		metadata_cache_binding TEXT NOT NULL UNIQUE,
		required_generation INTEGER NOT NULL DEFAULT 0 CHECK (required_generation >= 0),
		vault_size_bytes INTEGER CHECK (vault_size_bytes IS NULL OR vault_size_bytes >= 0),
		vault_size_measured_at TEXT NOT NULL DEFAULT '',
		vault_size_dirty INTEGER NOT NULL DEFAULT 0 CHECK (vault_size_dirty IN (0,1)),
		vault_size_last_attempt_at TEXT NOT NULL DEFAULT '',
		check_schedule TEXT NOT NULL DEFAULT 'manual',
		next_check TEXT NOT NULL DEFAULT '',
		last_check TEXT NOT NULL DEFAULT '',
		last_check_status TEXT NOT NULL DEFAULT '',
		maintenance_schedule TEXT NOT NULL DEFAULT 'daily',
		object_lock_json TEXT NOT NULL DEFAULT '{"enrolled":false,"paused":false}',
		next_maintenance TEXT NOT NULL DEFAULT '',
		last_maintenance TEXT NOT NULL DEFAULT '',
		last_maintenance_status TEXT NOT NULL DEFAULT '',
		concurrency_mode TEXT NOT NULL DEFAULT 'native'
			CHECK (concurrency_mode IN ('reduced','native','increased','maximum')),
		auto_unlock INTEGER NOT NULL DEFAULT 1 CHECK (auto_unlock IN (0,1)),
		created_at DATETIME,
		CHECK ((vault_size_bytes IS NULL AND vault_size_measured_at = '') OR
		       (vault_size_bytes IS NOT NULL AND vault_size_measured_at <> '')),
		CHECK ((cold_storage = 0 AND archive_write_class = '') OR
		       (cold_storage = 1 AND engine = 'restic' AND connector = 's3' AND archive_write_class IN ('DEEP_ARCHIVE','GLACIER')))
	);

	CREATE TABLE IF NOT EXISTS activity_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		timestamp DATETIME,
		level TEXT,
		message TEXT
	);

	CREATE TABLE IF NOT EXISTS backup_jobs (
		id TEXT PRIMARY KEY,
		name TEXT NOT NULL COLLATE NOCASE UNIQUE,
		source TEXT NOT NULL,
		source_storage_version TEXT NOT NULL,
		source_storage_key TEXT NOT NULL,
		source_storage_json TEXT NOT NULL,
		source_binding_state TEXT NOT NULL DEFAULT 'bound'
			CHECK (source_binding_state IN ('bound','unbound_imported')),
		resolved_source_path TEXT NOT NULL DEFAULT '',
		resolved_source_observed_at TEXT NOT NULL DEFAULT '',
		schedule TEXT NOT NULL,
		enabled INTEGER NOT NULL,
		next_run DATETIME,
		last_run DATETIME,
		retention INTEGER NOT NULL DEFAULT 100 CHECK (retention >= 0),
		retention_hourly INTEGER CHECK (retention_hourly BETWEEN 1 AND 2147483647),
		retention_daily INTEGER CHECK (retention_daily BETWEEN 1 AND 2147483647),
		retention_weekly INTEGER CHECK (retention_weekly BETWEEN 1 AND 2147483647),
		retention_monthly INTEGER CHECK (retention_monthly BETWEEN 1 AND 2147483647),
		retention_yearly INTEGER CHECK (retention_yearly BETWEEN 1 AND 2147483647),
		excludes TEXT NOT NULL DEFAULT '',
		tag TEXT NOT NULL DEFAULT '',
		before_script_path TEXT NOT NULL DEFAULT '',
		before_script_must_succeed INTEGER NOT NULL DEFAULT 0 CHECK (before_script_must_succeed IN (0,1)),
		after_script_path TEXT NOT NULL DEFAULT '',
		after_script_must_succeed INTEGER NOT NULL DEFAULT 0 CHECK (after_script_must_succeed IN (0,1)),
		engine_settings TEXT NOT NULL DEFAULT '{"version":1,"engines":{}}',
		portable_target_ids TEXT NOT NULL DEFAULT '[]'
	);

	CREATE TABLE IF NOT EXISTS backup_job_targets (
		job_id TEXT NOT NULL,
		repository_id TEXT NOT NULL,
		last_run DATETIME NOT NULL DEFAULT '',
		last_status TEXT NOT NULL DEFAULT '',
		logical_size_bytes INTEGER CHECK (logical_size_bytes IS NULL OR logical_size_bytes >= 0),
		logical_size_measured_at TEXT NOT NULL DEFAULT '',
		CHECK ((logical_size_bytes IS NULL AND logical_size_measured_at = '') OR
		       (logical_size_bytes IS NOT NULL AND logical_size_measured_at <> '')),
		PRIMARY KEY (job_id, repository_id),
		FOREIGN KEY (job_id) REFERENCES backup_jobs(id) ON DELETE CASCADE,
		FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS job_target_schedule_state (
		job_id TEXT NOT NULL,
		repository_id TEXT NOT NULL,
		pending_catchup INTEGER NOT NULL DEFAULT 0 CHECK (pending_catchup IN (0,1)),
		coalesced_missed_count INTEGER NOT NULL DEFAULT 0 CHECK (coalesced_missed_count >= 0),
		first_deferred_due_at TEXT NOT NULL DEFAULT '',
		last_due_at TEXT NOT NULL DEFAULT '',
		source_availability TEXT NOT NULL DEFAULT 'unknown'
			CHECK (source_availability IN ('available','unavailable','unknown')),
		source_reason_code TEXT NOT NULL DEFAULT 'not_checked',
		source_checked_at TEXT NOT NULL DEFAULT '',
		target_availability TEXT NOT NULL DEFAULT 'unknown'
			CHECK (target_availability IN ('available','unavailable','unknown')),
		target_reason_code TEXT NOT NULL DEFAULT 'not_checked',
		target_checked_at TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL DEFAULT '',
		PRIMARY KEY (job_id, repository_id),
		FOREIGN KEY (job_id, repository_id)
			REFERENCES backup_job_targets(job_id, repository_id) ON DELETE CASCADE,
		CHECK (
			(pending_catchup = 0 AND coalesced_missed_count = 0 AND first_deferred_due_at = '') OR
			(pending_catchup = 1 AND coalesced_missed_count > 0 AND length(trim(first_deferred_due_at)) > 0)
		)
	);

	CREATE TRIGGER IF NOT EXISTS backup_job_targets_schedule_state
	AFTER INSERT ON backup_job_targets
	BEGIN
		INSERT INTO job_target_schedule_state (job_id, repository_id)
		VALUES (NEW.job_id, NEW.repository_id);
	END;

	CREATE TABLE IF NOT EXISTS kopia_policy_state (
		repository_id TEXT PRIMARY KEY,
		desired_digest TEXT NOT NULL CHECK (length(desired_digest) = 64),
		applied_digest TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL CHECK (state IN ('dirty','applying','ready','error')),
		last_error TEXT NOT NULL DEFAULT '',
		last_reconciled_at TEXT NOT NULL DEFAULT '',
		active_fence_path TEXT NOT NULL DEFAULT '',
		proof_kind TEXT NOT NULL DEFAULT '',
		FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS operations (
		id TEXT PRIMARY KEY,
		kind TEXT NOT NULL,
		engine TEXT NOT NULL DEFAULT '',
		status TEXT NOT NULL,
		title TEXT NOT NULL,
		job_id TEXT NOT NULL DEFAULT '',
		repository_id TEXT NOT NULL DEFAULT '',
		started_at DATETIME NOT NULL,
		finished_at DATETIME NOT NULL DEFAULT ''
	);

	CREATE TABLE IF NOT EXISTS operation_steps (
		id TEXT PRIMARY KEY,
		operation_id TEXT NOT NULL,
		domain TEXT NOT NULL CHECK (domain IN ('native','orchestration','application')),
		kind TEXT NOT NULL,
		status TEXT NOT NULL CHECK (
			status IN ('running','succeeded','failed','skipped','warning','interrupted')
			AND (status <> 'interrupted' OR domain = 'native')
		),
		started_at TEXT NOT NULL,
		finished_at TEXT NOT NULL DEFAULT '',
		UNIQUE (operation_id, kind),
		FOREIGN KEY (operation_id) REFERENCES operations(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS scheduled_admission_groups (
		id TEXT PRIMARY KEY,
		job_id TEXT NOT NULL,
		generation INTEGER NOT NULL CHECK (generation > 0),
		admitted_at TEXT NOT NULL CHECK (length(trim(admitted_at)) > 0),
		previous_job_last_run TEXT NOT NULL,
		any_started INTEGER NOT NULL DEFAULT 0 CHECK (any_started IN (0,1)),
		UNIQUE (job_id, generation),
		FOREIGN KEY (job_id) REFERENCES backup_jobs(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS scheduled_operation_restorations (
		operation_id TEXT PRIMARY KEY,
		group_id TEXT NOT NULL,
		job_id TEXT NOT NULL,
		repository_id TEXT NOT NULL,
		coalesced_missed_count INTEGER NOT NULL CHECK (coalesced_missed_count > 0),
		first_deferred_due_at TEXT NOT NULL CHECK (length(trim(first_deferred_due_at)) > 0),
		last_due_at TEXT NOT NULL CHECK (length(trim(last_due_at)) > 0),
		restore_state TEXT NOT NULL DEFAULT 'eligible'
			CHECK (restore_state IN ('eligible','started','restored')),
		FOREIGN KEY (operation_id) REFERENCES operations(id) ON DELETE CASCADE,
		FOREIGN KEY (group_id) REFERENCES scheduled_admission_groups(id) ON DELETE CASCADE,
		FOREIGN KEY (job_id, repository_id)
			REFERENCES backup_job_targets(job_id, repository_id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS owner_transfer_operations (
		operation_uuid TEXT PRIMARY KEY,
		repository_id TEXT NOT NULL,
		from_profile_uuid TEXT NOT NULL,
		to_profile_uuid TEXT NOT NULL,
		state TEXT NOT NULL CHECK (state IN ('reviewed','native_owner_applied','root_published','completed','failed')),
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE,
		CHECK (from_profile_uuid <> to_profile_uuid)
	);

	CREATE TABLE IF NOT EXISTS vault_password_change_operations (
		repository_id TEXT PRIMARY KEY,
		operation_uuid TEXT NOT NULL UNIQUE,
		phase TEXT NOT NULL CHECK (phase IN ('preparing','native_started','publishing','cleanup_pending')),
		native_status TEXT NOT NULL DEFAULT ''
			CHECK (native_status = '' OR native_status IN ('not_started','succeeded','failed','interrupted')),
		native_mutation_disposition TEXT NOT NULL DEFAULT 'unknown'
			CHECK (native_mutation_disposition IN ('unknown','rejected_before_mutation')),
		native_output TEXT NOT NULL DEFAULT '',
		last_error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS repository_creation_intents (
		id TEXT PRIMARY KEY,
		canonical_identity TEXT NOT NULL UNIQUE,
		storage_identity_version TEXT NOT NULL DEFAULT '',
		storage_identity_key TEXT NOT NULL DEFAULT '',
		storage_identity_json TEXT NOT NULL DEFAULT '',
		engine TEXT NOT NULL,
		connector TEXT NOT NULL,
		cold_storage INTEGER NOT NULL DEFAULT 0 CHECK (cold_storage IN (0,1)),
		archive_write_class TEXT NOT NULL DEFAULT '',
		location TEXT NOT NULL,
		name TEXT NOT NULL COLLATE NOCASE UNIQUE,
		description TEXT NOT NULL DEFAULT '',
		check_schedule TEXT NOT NULL DEFAULT 'manual',
		maintenance_schedule TEXT NOT NULL DEFAULT 'daily',
		concurrency_mode TEXT NOT NULL DEFAULT 'native'
			CHECK (concurrency_mode IN ('reduced','native','increased','maximum')),
		object_lock_json TEXT NOT NULL DEFAULT '{"enrolled":false,"paused":false}',
		publication_operation_id TEXT NOT NULL,
		reviewed_options_json TEXT NOT NULL DEFAULT '{}',
		profile_sha256 TEXT NOT NULL DEFAULT '',
		profile_json TEXT NOT NULL DEFAULT '',
		native_fingerprint TEXT NOT NULL DEFAULT '',
		native_operation_id TEXT NOT NULL DEFAULT '',
		phase TEXT NOT NULL DEFAULT 'prepared' CHECK (phase IN ('prepared','native_started','native_ready')),
		last_error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		CHECK ((cold_storage = 0 AND archive_write_class = '') OR
		       (cold_storage = 1 AND engine = 'restic' AND connector = 's3' AND archive_write_class IN ('DEEP_ARCHIVE','GLACIER')))
	);

	CREATE TABLE IF NOT EXISTS vault_profile_sync (
		repository_id TEXT PRIMARY KEY,
		revision INTEGER NOT NULL DEFAULT 1,
		last_error TEXT NOT NULL DEFAULT '',
		attempt_count INTEGER NOT NULL DEFAULT 0,
		next_attempt_at TEXT NOT NULL DEFAULT '',
		operation_id TEXT NOT NULL DEFAULT '',
		profile_sha256 TEXT NOT NULL DEFAULT '',
		profile_json TEXT NOT NULL DEFAULT '',
		expected_current_sha256 TEXT NOT NULL DEFAULT '',
		create_only INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL
	);

	CREATE TABLE IF NOT EXISTS dormant_recovery_jobs (
		repository_id TEXT NOT NULL,
		job_id TEXT NOT NULL,
		definition_json TEXT NOT NULL,
		created_at TEXT NOT NULL,
		PRIMARY KEY (repository_id, job_id),
		FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS repository_connection_intents (
		id TEXT PRIMARY KEY,
		canonical_identity TEXT NOT NULL UNIQUE,
		connector TEXT NOT NULL,
		location TEXT NOT NULL,
		reviewed_options_json TEXT NOT NULL DEFAULT '{}',
		mode TEXT NOT NULL,
		preview_digest TEXT NOT NULL,
		payload_json TEXT NOT NULL CHECK (json_valid(payload_json)),
		profile_sha256 TEXT NOT NULL,
		native_fingerprint TEXT NOT NULL,
		publication_operation_id TEXT NOT NULL,
		state TEXT NOT NULL DEFAULT 'prepared'
			CHECK (state IN ('prepared','publication_started','attachment_pending')),
		error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	);
	CREATE TRIGGER IF NOT EXISTS repository_connection_intent_payload_immutable
	BEFORE UPDATE OF canonical_identity,connector,location,reviewed_options_json,mode,
		preview_digest,payload_json,profile_sha256,native_fingerprint,publication_operation_id
	ON repository_connection_intents
	BEGIN
		SELECT RAISE(ABORT, 'repository connection intent is immutable');
	END;
	CREATE TABLE IF NOT EXISTS repository_connection_name_reservations (
		connection_id TEXT PRIMARY KEY,
		repository_id TEXT NOT NULL,
		name TEXT NOT NULL COLLATE NOCASE UNIQUE,
		FOREIGN KEY (connection_id) REFERENCES repository_connection_intents(id) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS kopia_filesystem_reconnect_intents (
		repository_id TEXT PRIMARY KEY,
		intent_id TEXT NOT NULL UNIQUE,
		prior_resolved_path TEXT NOT NULL,
		candidate_path TEXT NOT NULL,
		prior_config_sha256 TEXT NOT NULL CHECK (length(prior_config_sha256) = 64),
		staged_config_sha256 TEXT NOT NULL DEFAULT '' CHECK (staged_config_sha256 = '' OR length(staged_config_sha256) = 64),
		state TEXT NOT NULL CHECK (state IN ('prepared','activated','committed','cleanup')),
		error TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		FOREIGN KEY (repository_id) REFERENCES repositories(id) ON DELETE CASCADE
	);
	CREATE TABLE IF NOT EXISTS repository_connection_job_name_reservations (
		connection_id TEXT NOT NULL,
		job_id TEXT NOT NULL,
		name TEXT NOT NULL COLLATE NOCASE UNIQUE,
		PRIMARY KEY (connection_id, job_id),
		FOREIGN KEY (connection_id) REFERENCES repository_connection_intents(id) ON DELETE CASCADE
	);
	CREATE INDEX IF NOT EXISTS backup_job_targets_repository
		ON backup_job_targets (repository_id);
	CREATE INDEX IF NOT EXISTS job_target_schedule_pending
		ON job_target_schedule_state (pending_catchup, job_id, repository_id);
	CREATE INDEX IF NOT EXISTS repositories_canonical_identity
		ON repositories (canonical_identity);
	CREATE INDEX IF NOT EXISTS operations_status_started
		ON operations (status, started_at);
	CREATE INDEX IF NOT EXISTS operations_job_target_status
		ON operations (job_id, repository_id, status);
	CREATE INDEX IF NOT EXISTS scheduled_admission_groups_job_generation
		ON scheduled_admission_groups (job_id, generation);
	CREATE INDEX IF NOT EXISTS scheduled_operation_restorations_group_state
		ON scheduled_operation_restorations (group_id, restore_state);
	CREATE INDEX IF NOT EXISTS dormant_recovery_jobs_job
		ON dormant_recovery_jobs (job_id, repository_id);
	`
	if _, err := tx.Exec(schema); err != nil {
		return err
	}
	// Schema 70 predates the optional password-mutation disposition. Adding the
	// conservative default in place keeps existing current-schema operations
	// fail-closed without admitting any older schema version.
	var dispositionColumns int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('vault_password_change_operations')
		WHERE name='native_mutation_disposition'`).Scan(&dispositionColumns); err != nil {
		return err
	}
	if dispositionColumns == 0 {
		if _, err := tx.Exec(`ALTER TABLE vault_password_change_operations
			ADD COLUMN native_mutation_disposition TEXT NOT NULL DEFAULT 'unknown'
			CHECK (native_mutation_disposition IN ('unknown','rejected_before_mutation'))`); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO settings (key,value) VALUES ('installationId',?)
		ON CONFLICT(key) DO NOTHING`, uuid.NewString()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DROP TABLE IF EXISTS temp.startup_stuck_restic_repositories`); err != nil {
		return err
	}
	if _, err := tx.Exec(`CREATE TEMP TABLE startup_stuck_restic_repositories (
		repository_id TEXT PRIMARY KEY
	)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO startup_stuck_restic_repositories (repository_id)
		SELECT r.id FROM repositories r
		WHERE r.engine = 'restic' AND r.auto_unlock = 1 AND (
			r.last_check_status = 'running' OR r.last_maintenance_status = 'running' OR
			EXISTS (SELECT 1 FROM operations o WHERE o.repository_id = r.id AND o.status = 'running')
		)`); err != nil {
		return err
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, CurrentSchemaVersion)); err != nil {
		return err
	}

	// Restore admitted scheduled occurrences that did not reach the requested
	// native backup boundary. Process activation alone is not consumption; a
	// non-skipped native backup step is the conservative crash-time start truth,
	// while a skipped step durably confirms that the requested child did not run.
	queuedRows, err := tx.Query(`SELECT o.id FROM operations o
		LEFT JOIN scheduled_operation_restorations r ON r.operation_id=o.id
		WHERE o.kind='backup' AND (
			o.status='queued' OR (
				o.status='running' AND r.restore_state='eligible' AND NOT EXISTS (
					SELECT 1 FROM operation_steps s WHERE s.operation_id=o.id
					AND s.domain='native' AND s.kind='backup' AND s.status<>'skipped'
				)
			)
		) ORDER BY o.started_at,o.id`)
	if err != nil {
		return err
	}
	queuedIDs := []string{}
	for queuedRows.Next() {
		var operationID string
		if err := queuedRows.Scan(&operationID); err != nil {
			_ = queuedRows.Close()
			return err
		}
		queuedIDs = append(queuedIDs, operationID)
	}
	if err := queuedRows.Close(); err != nil {
		return err
	}
	for _, operationID := range queuedIDs {
		if err := restoreQueuedBackupOperationTx(
			tx, operationID,
			"Application stopped before the operation started.",
			time.Now().UTC(),
		); err != nil {
			return err
		}
	}
	// Conversely, a durable non-skipped requested-backup step must never be
	// replayed after a crash. Consume its still-eligible restoration before
	// generic interruption.
	if _, err := tx.Exec(`UPDATE scheduled_operation_restorations SET restore_state='started'
		WHERE restore_state='eligible' AND operation_id IN (
			SELECT o.id FROM operations o WHERE o.status='running' AND EXISTS (
				SELECT 1 FROM operation_steps s WHERE s.operation_id=o.id
				AND s.domain='native' AND s.kind='backup' AND s.status<>'skipped'
			)
		)`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE scheduled_admission_groups SET any_started=1
		WHERE id IN (SELECT group_id FROM scheduled_operation_restorations WHERE restore_state='started')`); err != nil {
		return err
	}
	reconciliationRows, err := tx.Query(`SELECT DISTINCT job_id FROM scheduled_admission_groups ORDER BY job_id`)
	if err != nil {
		return err
	}
	reconciliationJobs := []string{}
	for reconciliationRows.Next() {
		var jobID string
		if err := reconciliationRows.Scan(&jobID); err != nil {
			_ = reconciliationRows.Close()
			return err
		}
		reconciliationJobs = append(reconciliationJobs, jobID)
	}
	if err := reconciliationRows.Close(); err != nil {
		return err
	}
	for _, jobID := range reconciliationJobs {
		if err := reconcileScheduledAdmissionGroupsTx(tx, jobID); err != nil {
			return err
		}
	}
	type startupStep struct{ operationID, engine, domain, kind string }
	startupSteps := []startupStep{}
	stepRows, err := tx.Query(`SELECT s.operation_id,o.engine,s.domain,s.kind
		FROM operation_steps s JOIN operations o ON o.id=s.operation_id
		WHERE s.status='running' ORDER BY s.started_at,s.id`)
	if err != nil {
		return err
	}
	for stepRows.Next() {
		var step startupStep
		if err := stepRows.Scan(&step.operationID, &step.engine, &step.domain, &step.kind); err != nil {
			_ = stepRows.Close()
			return err
		}
		startupSteps = append(startupSteps, step)
	}
	if err := stepRows.Close(); err != nil {
		return err
	}
	type startupOperation struct{ id, engine, kind string }
	startupOperations := []startupOperation{}
	operationRows, err := tx.Query(`SELECT id,engine,kind FROM operations WHERE status='running' ORDER BY started_at,id`)
	if err != nil {
		return err
	}
	for operationRows.Next() {
		var operation startupOperation
		if err := operationRows.Scan(&operation.id, &operation.engine, &operation.kind); err != nil {
			_ = operationRows.Close()
			return err
		}
		startupOperations = append(startupOperations, operation)
	}
	if err := operationRows.Close(); err != nil {
		return err
	}
	retentionOperations := []startupOperation{}
	retentionRows, err := tx.Query(`SELECT o.id,o.engine,o.kind
		FROM operations o
		JOIN repositories r ON r.id=o.repository_id AND r.engine='restic'
		JOIN backup_jobs j ON j.id=o.job_id AND j.retention>0
		JOIN operation_steps b ON b.operation_id=o.id AND b.kind='backup' AND b.domain='native' AND b.status='succeeded'
		WHERE o.status='running'
		  AND NOT EXISTS (SELECT 1 FROM operation_steps s WHERE s.operation_id=o.id AND s.kind='retention')`)
	if err != nil {
		return err
	}
	for retentionRows.Next() {
		var operation startupOperation
		if err := retentionRows.Scan(&operation.id, &operation.engine, &operation.kind); err != nil {
			_ = retentionRows.Close()
			return err
		}
		retentionOperations = append(retentionOperations, operation)
	}
	if err := retentionRows.Close(); err != nil {
		return err
	}
	interruptedAt := formatSortableTimestamp(time.Now())
	// No running operation survives a restart. A durable native start without a
	// durable result is conservatively interrupted and is never retried.
	if _, err := tx.Exec(`UPDATE backup_job_targets
		SET last_status = 'interrupted',
			last_run = COALESCE((
				SELECT o.started_at FROM operations o
				WHERE o.job_id = backup_job_targets.job_id
					AND o.repository_id = backup_job_targets.repository_id
					AND o.status = 'running'
				ORDER BY o.started_at DESC, o.id DESC LIMIT 1
			), last_run)
		WHERE last_status = 'running' OR EXISTS (
			SELECT 1 FROM operations o
			WHERE o.job_id = backup_job_targets.job_id
				AND o.repository_id = backup_job_targets.repository_id
				AND o.status = 'running'
		)`); err != nil {
		return err
	}
	// A conclusive Restic backup may have committed immediately before a crash
	// that preceded retention-step persistence. Record definite no-start truth;
	// do not infer a native result or launch the missed command on startup.
	if _, err := tx.Exec(`INSERT INTO operation_steps
		(id,operation_id,domain,kind,status,started_at,finished_at)
		SELECT lower(hex(randomblob(16))),o.id,'native','retention','skipped',
		       ?,?
		FROM operations o
		JOIN repositories r ON r.id=o.repository_id AND r.engine='restic'
		JOIN backup_jobs j ON j.id=o.job_id AND j.retention>0
		JOIN operation_steps b ON b.operation_id=o.id AND b.kind='backup' AND b.domain='native' AND b.status='succeeded'
		WHERE o.status='running'
		  AND NOT EXISTS (SELECT 1 FROM operation_steps s WHERE s.operation_id=o.id AND s.kind='retention')`,
		interruptedAt, interruptedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE operations
		SET status = 'interrupted',
			finished_at = ?
		WHERE status = 'running'`, interruptedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE operation_steps
		SET status = CASE WHEN domain='native' THEN 'interrupted' ELSE 'failed' END,
			finished_at = ?
		WHERE status = 'running'`, interruptedAt); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE repositories SET last_check_status = 'failed' WHERE last_check_status = 'running'`); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE repositories SET last_maintenance_status = 'failed' WHERE last_maintenance_status = 'running'`); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Startup status repair remains authoritative even when its corresponding
	// local diagnostic append fails; the file can be absent or incomplete.
	for _, operationID := range queuedIDs {
		var engine, kind string
		_ = db.QueryRow(`SELECT engine,kind FROM operations WHERE id=?`, operationID).Scan(&engine, &kind)
		_ = operationlog.AppendFinal(operationID, engine, kind, "interrupted", "Application stopped before the operation started.")
	}
	for _, step := range startupSteps {
		status := "failed"
		body := "Application stopped before this step completed."
		if step.domain == "native" {
			status = "interrupted"
			body = "Application stopped before native launch or completion could be durably resolved."
		} else if step.kind == "notification" {
			body = "desktop/webhook notification delivery did not complete because the application stopped"
		}
		_ = operationlog.AppendSection(step.operationID, operationlog.Section{
			Engine: step.engine, Domain: step.domain, Kind: step.kind, Status: status, Body: body,
		})
	}
	for _, operation := range retentionOperations {
		_ = operationlog.AppendSection(operation.id, operationlog.Section{
			Engine: operation.engine, Domain: "native", Kind: "retention", Status: "skipped",
			Body: "Application stopped before immediate native retention could be durably started; retention was not started.",
		})
	}
	for _, operation := range startupOperations {
		_ = operationlog.AppendFinal(operation.id, operation.engine, operation.kind, "interrupted",
			"Application stopped before the operation completed; the operation was marked interrupted.")
	}
	return nil
}
