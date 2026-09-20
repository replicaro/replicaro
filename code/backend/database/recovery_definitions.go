package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/vaultprofile"
)

type DormantRecoveryJob struct {
	RepositoryID   string `json:"repositoryId"`
	JobID          string `json:"jobId"`
	DefinitionJSON string `json:"definitionJson"`
}

type portableJobDefinition struct {
	vaultprofile.BackupJob
	SourceStorageVersion string `json:"sourceStorageVersion"`
	SourceStorageKey     string `json:"sourceStorageKey"`
	SourceStorageJSON    string `json:"sourceStorageDescriptor"`
	SourceBindingState   string `json:"sourceBindingState,omitempty"`
}

func (definition portableJobDefinition) MarshalJSON() ([]byte, error) {
	jobData, err := json.Marshal(definition.BackupJob)
	if err != nil {
		return nil, err
	}
	fields := map[string]json.RawMessage{}
	if err := json.Unmarshal(jobData, &fields); err != nil {
		return nil, err
	}
	for key, value := range map[string]string{
		"sourceStorageVersion":    definition.SourceStorageVersion,
		"sourceStorageKey":        definition.SourceStorageKey,
		"sourceStorageDescriptor": definition.SourceStorageJSON,
		"sourceBindingState":      definition.SourceBindingState,
	} {
		encoded, encodeErr := json.Marshal(value)
		if encodeErr != nil {
			return nil, encodeErr
		}
		fields[key] = encoded
	}
	return json.Marshal(fields)
}

func (definition *portableJobDefinition) UnmarshalJSON(data []byte) error {
	var job vaultprofile.BackupJob
	if err := json.Unmarshal(data, &job); err != nil {
		return err
	}
	var storage struct {
		SourceStorageVersion string `json:"sourceStorageVersion"`
		SourceStorageKey     string `json:"sourceStorageKey"`
		SourceStorageJSON    string `json:"sourceStorageDescriptor"`
		SourceBindingState   string `json:"sourceBindingState"`
	}
	if err := json.Unmarshal(data, &storage); err != nil {
		return err
	}
	definition.BackupJob = job
	definition.SourceStorageVersion = storage.SourceStorageVersion
	definition.SourceStorageKey = storage.SourceStorageKey
	definition.SourceStorageJSON = storage.SourceStorageJSON
	definition.SourceBindingState = storage.SourceBindingState
	return nil
}

func portableDefinitionFromModel(job models.BackupJob) portableJobDefinition {
	return portableJobDefinition{
		BackupJob: vaultprofile.BackupJob{
			JobUUID: job.ID, Name: job.Name, Source: job.Source, Schedule: job.Schedule,
			Retention: job.Retention, RetentionHourly: job.RetentionHourly,
			RetentionDaily: job.RetentionDaily, RetentionWeekly: job.RetentionWeekly,
			RetentionMonthly: job.RetentionMonthly, RetentionYearly: job.RetentionYearly,
			Exclusions: job.Excludes, Tag: job.Tag,
			BeforeScriptPath:        job.BeforeScriptPath,
			BeforeScriptMustSucceed: job.BeforeScriptMustSucceed,
			AfterScriptPath:         job.AfterScriptPath,
			AfterScriptMustSucceed:  job.AfterScriptMustSucceed,
			EngineSettings:          job.EngineSettings, Enabled: job.Enabled,
			TargetVaultUUIDs: append([]string(nil), job.PortableTargetIDs...),
		},
		SourceStorageVersion: job.SourceStorageVersion,
		SourceStorageKey:     job.SourceStorageKey,
		SourceStorageJSON:    job.SourceStorageDescriptorJSON,
		SourceBindingState:   job.SourceBindingState,
	}
}

func decodePortableDefinition(value string) (portableJobDefinition, error) {
	var definition portableJobDefinition
	if err := json.Unmarshal([]byte(value), &definition); err != nil {
		return definition, fmt.Errorf("dormant recovery definition is malformed")
	}
	if err := validateJobSourceBinding(models.BackupJob{
		Source: definition.Source, Enabled: false,
		SourceStorageVersion:        definition.SourceStorageVersion,
		SourceStorageKey:            definition.SourceStorageKey,
		SourceStorageDescriptorJSON: definition.SourceStorageJSON,
		SourceBindingState:          definition.SourceBindingState,
	}); err != nil {
		return definition, fmt.Errorf("dormant recovery definition has an invalid source storage binding")
	}
	return definition, nil
}

func reconcileDormantWithActiveJobs(tx *sql.Tx, jobs []models.BackupJob) ([]string, error) {
	affected := []string{}
	for _, job := range jobs {
		active := portableDefinitionFromModel(job)
		rows, err := tx.Query(`SELECT repository_id, definition_json FROM dormant_recovery_jobs WHERE job_id = ?`, job.ID)
		if err != nil {
			return nil, err
		}
		type dormantOwner struct {
			repositoryID string
			enabled      bool
		}
		dormantOwners := []dormantOwner{}
		mergedTargets := append([]string(nil), active.TargetVaultUUIDs...)
		for rows.Next() {
			var repositoryID, definitionJSON string
			if err := rows.Scan(&repositoryID, &definitionJSON); err != nil {
				_ = rows.Close()
				return nil, err
			}
			dormant, err := decodePortableDefinition(definitionJSON)
			if err != nil || dormant.JobUUID != job.ID {
				_ = rows.Close()
				return nil, fmt.Errorf("dormant recovery definition is malformed")
			}
			if !dormantDefinitionsMatch(active, dormant) {
				_ = rows.Close()
				return nil, fmt.Errorf("portable job %s cannot be attached: its definition or settings differ from a dormant definition for the same job ID", job.ID)
			}
			mergedTargets = mergePortableTargetIDs(mergedTargets, dormant.TargetVaultUUIDs)
			dormantOwners = append(dormantOwners, dormantOwner{repositoryID: repositoryID, enabled: dormant.Enabled})
			affected = append(affected, repositoryID)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if len(dormantOwners) == 0 {
			continue
		}
		encodedTargets, err := json.Marshal(mergedTargets)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`UPDATE backup_jobs SET portable_target_ids = ? WHERE id = ?`, string(encodedTargets), job.ID); err != nil {
			return nil, err
		}
		for _, owner := range dormantOwners {
			updated := active
			updated.Enabled = owner.enabled
			updated.TargetVaultUUIDs = append([]string(nil), mergedTargets...)
			encodedDefinition, err := json.Marshal(updated)
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`UPDATE dormant_recovery_jobs SET definition_json = ? WHERE repository_id = ? AND job_id = ?`,
				string(encodedDefinition), owner.repositoryID, job.ID); err != nil {
				return nil, err
			}
		}
	}
	return mergePortableTargetIDs(affected), nil
}

func dormantDefinitionsMatch(left, right portableJobDefinition) bool {
	left.TargetVaultUUIDs = nil
	right.TargetVaultUUIDs = nil
	left.Enabled = false
	right.Enabled = false
	left.SourceBindingState = normalizedJobSourceBindingState(left.SourceBindingState)
	right.SourceBindingState = normalizedJobSourceBindingState(right.SourceBindingState)
	return reflect.DeepEqual(left, right)
}

func insertDormantRecoveryJobs(tx *sql.Tx, repositoryID string, definitions []DormantRecoveryJob) ([]string, error) {
	var repositoryEngine string
	if len(definitions) > 0 {
		if err := tx.QueryRow(`SELECT engine FROM repositories WHERE id = ?`, repositoryID).Scan(&repositoryEngine); err != nil {
			return nil, err
		}
	}
	seen := map[string]bool{}
	affected := []string{}
	for _, definition := range definitions {
		if definition.RepositoryID != "" && definition.RepositoryID != repositoryID {
			return nil, fmt.Errorf("dormant recovery definition targets a different vault")
		}
		if definition.JobID == "" || definition.DefinitionJSON == "" || seen[definition.JobID] {
			return nil, fmt.Errorf("dormant recovery definition is invalid or duplicated")
		}
		parsed, err := decodePortableDefinition(definition.DefinitionJSON)
		if err != nil || parsed.JobUUID != definition.JobID {
			return nil, fmt.Errorf("dormant recovery definition is malformed")
		}
		if err := engines.ValidatePortableJobSettings(parsed.EngineSettings, map[string]bool{repositoryEngine: true}); err != nil {
			return nil, fmt.Errorf("dormant portable job %s has incompatible engine settings: %w", definition.JobID, err)
		}
		parsed.TargetVaultUUIDs = mergePortableTargetIDs(parsed.TargetVaultUUIDs, []string{repositoryID})
		type existingDormant struct {
			repositoryID string
			enabled      bool
		}
		existingDormantRows := []existingDormant{}
		rows, err := tx.Query(`SELECT repository_id, definition_json FROM dormant_recovery_jobs WHERE job_id = ?`, definition.JobID)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ownerID, existingJSON string
			if err := rows.Scan(&ownerID, &existingJSON); err != nil {
				_ = rows.Close()
				return nil, err
			}
			existing, decodeErr := decodePortableDefinition(existingJSON)
			if decodeErr != nil || !dormantDefinitionsMatch(existing, parsed) {
				_ = rows.Close()
				return nil, fmt.Errorf("dormant portable job %s cannot be stored: its definition or settings differ from the same job ID already held by another vault", definition.JobID)
			}
			parsed.TargetVaultUUIDs = mergePortableTargetIDs(existing.TargetVaultUUIDs, parsed.TargetVaultUUIDs)
			existingDormantRows = append(existingDormantRows, existingDormant{repositoryID: ownerID, enabled: existing.Enabled})
			affected = append(affected, ownerID)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		var activeJSON, portableJSON string
		var name, source, sourceStorageVersion, sourceStorageKey, sourceStorageJSON, sourceBindingState string
		var schedule, excludes, tag, beforeScriptPath, afterScriptPath string
		var beforeScriptMustSucceed, afterScriptMustSucceed bool
		var retention, enabled int
		var retentionHourly, retentionDaily, retentionWeekly, retentionMonthly, retentionYearly sql.NullInt64
		err = tx.QueryRow(`SELECT name,source,source_storage_version,source_storage_key,
			source_storage_json,source_binding_state,schedule,retention,retention_hourly,retention_daily,
			retention_weekly,retention_monthly,retention_yearly,excludes,tag,
			before_script_path,before_script_must_succeed,after_script_path,after_script_must_succeed,
			engine_settings,
			portable_target_ids,enabled FROM backup_jobs WHERE id=?`, definition.JobID).Scan(
			&name, &source, &sourceStorageVersion, &sourceStorageKey, &sourceStorageJSON, &sourceBindingState,
			&schedule, &retention, &retentionHourly, &retentionDaily, &retentionWeekly,
			&retentionMonthly, &retentionYearly, &excludes, &tag, &beforeScriptPath, &beforeScriptMustSucceed,
			&afterScriptPath, &afterScriptMustSucceed, &activeJSON, &portableJSON, &enabled)
		if err != nil && err != sql.ErrNoRows {
			return nil, err
		}
		if err == nil {
			settings, decodeErr := decodeEngineSettings(activeJSON)
			var targets []string
			if decodeErr != nil || json.Unmarshal([]byte(portableJSON), &targets) != nil {
				return nil, fmt.Errorf("decode active portable job")
			}
			active := portableJobDefinition{
				BackupJob: vaultprofile.BackupJob{
					JobUUID: definition.JobID, Name: name, Source: source, Schedule: schedule,
					Retention: retention, RetentionHourly: nullableRetentionValue(retentionHourly),
					RetentionDaily: nullableRetentionValue(retentionDaily), RetentionWeekly: nullableRetentionValue(retentionWeekly),
					RetentionMonthly: nullableRetentionValue(retentionMonthly), RetentionYearly: nullableRetentionValue(retentionYearly),
					Exclusions: excludes, Tag: tag,
					BeforeScriptPath: beforeScriptPath, BeforeScriptMustSucceed: beforeScriptMustSucceed,
					AfterScriptPath: afterScriptPath, AfterScriptMustSucceed: afterScriptMustSucceed,
					EngineSettings: settings, Enabled: enabled != 0, TargetVaultUUIDs: targets,
				},
				SourceStorageVersion: sourceStorageVersion,
				SourceStorageKey:     sourceStorageKey,
				SourceStorageJSON:    sourceStorageJSON,
				SourceBindingState:   sourceBindingState,
			}
			if !dormantDefinitionsMatch(active, parsed) {
				return nil, fmt.Errorf("dormant portable job %s cannot be stored: its definition or settings differ from the active local job", definition.JobID)
			}
			mergedTargets := mergePortableTargetIDs(active.TargetVaultUUIDs, parsed.TargetVaultUUIDs)
			encodedTargets, marshalErr := json.Marshal(mergedTargets)
			if marshalErr != nil {
				return nil, marshalErr
			}
			if string(encodedTargets) != portableJSON {
				if _, err := tx.Exec(`UPDATE backup_jobs SET portable_target_ids = ? WHERE id = ?`, string(encodedTargets), definition.JobID); err != nil {
					return nil, err
				}
			}
			parsed.TargetVaultUUIDs = mergedTargets
			activeRows, queryErr := tx.Query(`SELECT repository_id FROM backup_job_targets WHERE job_id = ?`, definition.JobID)
			if queryErr != nil {
				return nil, queryErr
			}
			for activeRows.Next() {
				var ownerID string
				if scanErr := activeRows.Scan(&ownerID); scanErr != nil {
					_ = activeRows.Close()
					return nil, scanErr
				}
				affected = append(affected, ownerID)
			}
			if closeErr := activeRows.Close(); closeErr != nil {
				return nil, closeErr
			}
		}
		encoded, err := json.Marshal(parsed)
		if err != nil {
			return nil, err
		}
		for _, existing := range existingDormantRows {
			updated := parsed
			updated.Enabled = existing.enabled
			updated.TargetVaultUUIDs = append([]string(nil), parsed.TargetVaultUUIDs...)
			updatedJSON, err := json.Marshal(updated)
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(`UPDATE dormant_recovery_jobs SET definition_json = ? WHERE repository_id = ? AND job_id = ?`,
				string(updatedJSON), existing.repositoryID, definition.JobID); err != nil {
				return nil, err
			}
		}
		seen[definition.JobID] = true
		alreadyStored := false
		for _, existing := range existingDormantRows {
			alreadyStored = alreadyStored || existing.repositoryID == repositoryID
		}
		if !alreadyStored {
			if _, err := tx.Exec(`INSERT INTO dormant_recovery_jobs
				(repository_id, job_id, definition_json, created_at) VALUES (?, ?, ?, ?)`,
				repositoryID, definition.JobID, string(encoded), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return nil, err
			}
		}
		affected = append(affected, repositoryID)
	}
	return mergePortableTargetIDs(affected), nil
}

func DiscardDormantRecoveryJob(db *sql.DB, repositoryID, jobID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	result, err := tx.Exec(`DELETE FROM dormant_recovery_jobs WHERE repository_id = ? AND job_id = ?`, repositoryID, jobID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	affected := []string{repositoryID}
	rows, err := tx.Query(`SELECT repository_id, definition_json FROM dormant_recovery_jobs WHERE job_id = ?`, jobID)
	if err != nil {
		return err
	}
	type dormantUpdate struct{ repositoryID, definitionJSON string }
	updates := []dormantUpdate{}
	for rows.Next() {
		var ownerID, definitionJSON string
		if err := rows.Scan(&ownerID, &definitionJSON); err != nil {
			_ = rows.Close()
			return err
		}
		definition, err := decodePortableDefinition(definitionJSON)
		if err != nil {
			_ = rows.Close()
			return fmt.Errorf("dormant recovery definition is malformed")
		}
		definition.TargetVaultUUIDs = removePortableTarget(definition.TargetVaultUUIDs, repositoryID)
		encoded, err := json.Marshal(definition)
		if err != nil {
			_ = rows.Close()
			return err
		}
		updates = append(updates, dormantUpdate{ownerID, string(encoded)})
		affected = append(affected, ownerID)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := tx.Exec(`UPDATE dormant_recovery_jobs SET definition_json = ? WHERE repository_id = ? AND job_id = ?`,
			update.definitionJSON, update.repositoryID, jobID); err != nil {
			return err
		}
	}
	var portableJSON string
	if err := tx.QueryRow(`SELECT portable_target_ids FROM backup_jobs WHERE id = ?`, jobID).Scan(&portableJSON); err == nil {
		var targets []string
		if err := json.Unmarshal([]byte(portableJSON), &targets); err != nil {
			return fmt.Errorf("decode active portable targets: %w", err)
		}
		encoded, err := json.Marshal(removePortableTarget(targets, repositoryID))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE backup_jobs SET portable_target_ids = ? WHERE id = ?`, string(encoded), jobID); err != nil {
			return err
		}
		activeRows, err := tx.Query(`SELECT repository_id FROM backup_job_targets WHERE job_id = ?`, jobID)
		if err != nil {
			return err
		}
		for activeRows.Next() {
			var id string
			if err := activeRows.Scan(&id); err != nil {
				_ = activeRows.Close()
				return err
			}
			affected = append(affected, id)
		}
		if err := activeRows.Close(); err != nil {
			return err
		}
	} else if err != sql.ErrNoRows {
		return err
	}
	if err := markVaultProfilesDirty(tx, affected); err != nil {
		return err
	}
	return tx.Commit()
}

func removePortableTarget(values []string, remove string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != remove {
			result = append(result, value)
		}
	}
	return result
}

// PrepareDormantRecoveryJob returns the exact disabled job definition restored
// by an ordinary saved-job mutation.
func PrepareDormantRecoveryJob(db *sql.DB, repositoryID, jobID string) (models.BackupJob, error) {
	var definitionJSON, engine string
	if err := db.QueryRow(`SELECT definition_json FROM dormant_recovery_jobs
		WHERE repository_id=? AND job_id=?`, repositoryID, jobID).Scan(&definitionJSON); err != nil {
		return models.BackupJob{}, err
	}
	if err := db.QueryRow(`SELECT engine FROM repositories WHERE id=?`, repositoryID).Scan(&engine); err != nil {
		return models.BackupJob{}, err
	}
	saved, err := decodePortableDefinition(definitionJSON)
	if err != nil || saved.JobUUID != jobID {
		return models.BackupJob{}, fmt.Errorf("dormant recovery definition is invalid")
	}
	job := models.BackupJob{
		ID: saved.JobUUID, Name: saved.Name, Source: saved.Source, Schedule: saved.Schedule,
		Retention: saved.Retention, RetentionHourly: saved.RetentionHourly,
		RetentionDaily: saved.RetentionDaily, RetentionWeekly: saved.RetentionWeekly,
		RetentionMonthly: saved.RetentionMonthly, RetentionYearly: saved.RetentionYearly,
		Excludes: saved.Exclusions, Tag: saved.Tag, Enabled: false,
		BeforeScriptPath: saved.BeforeScriptPath, BeforeScriptMustSucceed: saved.BeforeScriptMustSucceed,
		AfterScriptPath: saved.AfterScriptPath, AfterScriptMustSucceed: saved.AfterScriptMustSucceed,
		SourceStorageVersion: saved.SourceStorageVersion, SourceStorageKey: saved.SourceStorageKey,
		SourceStorageDescriptorJSON: saved.SourceStorageJSON, SourceBindingState: saved.SourceBindingState,
		EngineSettings:    saved.EngineSettings,
		PortableTargetIDs: mergePortableTargetIDs(saved.TargetVaultUUIDs, []string{repositoryID}),
		Targets:           []models.BackupJobTarget{{RepositoryID: repositoryID, Engine: engine}},
	}
	existing, existingErr := GetJob(db, jobID)
	if existingErr == nil {
		if existing.Enabled {
			return models.BackupJob{}, fmt.Errorf("portable job %s is already enabled", jobID)
		}
		if !dormantDefinitionsMatch(portableDefinitionFromModel(existing), saved) {
			return models.BackupJob{}, fmt.Errorf("portable job %s cannot be restored: its definition or settings differ from the existing local job", jobID)
		}
		job = existing
		job.Enabled = false
		job.PortableTargetIDs = mergePortableTargetIDs(existing.PortableTargetIDs, saved.TargetVaultUUIDs)
		if !containsTarget(job.Targets, repositoryID) {
			job.Targets = append(job.Targets, models.BackupJobTarget{RepositoryID: repositoryID, Engine: engine})
		}
	} else if existingErr != sql.ErrNoRows {
		return models.BackupJob{}, existingErr
	} else {
		renamed, renameErr := RenameRecoveredJobNameConflicts(db, []models.BackupJob{job})
		if renameErr != nil {
			return models.BackupJob{}, renameErr
		}
		job = renamed[0]
	}
	represented := map[string]bool{}
	for _, target := range job.Targets {
		targetEngine := target.Engine
		if targetEngine == "" {
			if err := db.QueryRow(`SELECT engine FROM repositories WHERE id=?`, target.RepositoryID).Scan(&targetEngine); err != nil {
				return models.BackupJob{}, err
			}
		}
		represented[targetEngine] = true
	}
	if err := engines.ValidatePortableJobSettings(job.EngineSettings, represented); err != nil {
		return models.BackupJob{}, err
	}
	return job, nil
}

func containsTarget(targets []models.BackupJobTarget, repositoryID string) bool {
	for _, target := range targets {
		if target.RepositoryID == repositoryID {
			return true
		}
	}
	return false
}

// ConsumeDormantRecoveryJob removes the recovery definition only after the
// active target has been committed.
func ConsumeDormantRecoveryJob(db *sql.DB, repositoryID, jobID string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var portableJSON string
	if err := tx.QueryRow(`SELECT j.portable_target_ids FROM backup_jobs j
		JOIN backup_job_targets t ON t.job_id=j.id AND t.repository_id=?
		WHERE j.id=?`, repositoryID, jobID).Scan(&portableJSON); err != nil {
		return fmt.Errorf("restored job target is incomplete: %w", err)
	}
	var targetIDs []string
	if err := json.Unmarshal([]byte(portableJSON), &targetIDs); err != nil {
		return err
	}
	result, err := tx.Exec(`DELETE FROM dormant_recovery_jobs WHERE repository_id=? AND job_id=?`,
		repositoryID, jobID)
	if err != nil {
		return err
	}
	if count, _ := result.RowsAffected(); count != 1 {
		return sql.ErrNoRows
	}
	affected, err := updateDormantTargetIDs(tx, jobID, targetIDs)
	if err != nil {
		return err
	}
	affected = append(affected, targetIDs...)
	affected = append(affected, repositoryID)
	if err := markVaultProfilesDirty(tx, mergePortableTargetIDs(affected)); err != nil {
		return err
	}
	return tx.Commit()
}

func updateDormantTargetIDs(tx *sql.Tx, jobID string, targetIDs []string) ([]string, error) {
	rows, err := tx.Query(`SELECT repository_id, definition_json FROM dormant_recovery_jobs WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, err
	}
	type update struct {
		repositoryID string
		definition   string
	}
	updates := []update{}
	affected := []string{}
	for rows.Next() {
		var repositoryID, definitionJSON string
		if err := rows.Scan(&repositoryID, &definitionJSON); err != nil {
			_ = rows.Close()
			return nil, err
		}
		definition, err := decodePortableDefinition(definitionJSON)
		if err != nil || definition.JobUUID != jobID {
			_ = rows.Close()
			return nil, fmt.Errorf("dormant recovery definition is malformed")
		}
		definition.TargetVaultUUIDs = append([]string(nil), targetIDs...)
		encoded, err := json.Marshal(definition)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		updates = append(updates, update{repositoryID: repositoryID, definition: string(encoded)})
		affected = append(affected, repositoryID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	for _, item := range updates {
		if _, err := tx.Exec(`UPDATE dormant_recovery_jobs SET definition_json = ? WHERE repository_id = ? AND job_id = ?`,
			item.definition, item.repositoryID, jobID); err != nil {
			return nil, err
		}
	}
	return affected, nil
}

func updateDormantDefinitionsForJob(tx *sql.Tx, job models.BackupJob) ([]string, error) {
	saved := portableDefinitionFromModel(job)
	data, err := json.Marshal(saved)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT repository_id FROM dormant_recovery_jobs WHERE job_id = ?`, job.ID)
	if err != nil {
		return nil, err
	}
	repositories := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return nil, err
		}
		repositories = append(repositories, id)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE dormant_recovery_jobs SET definition_json = ? WHERE job_id = ?`, string(data), job.ID); err != nil {
		return nil, err
	}
	return repositories, nil
}

func ListDormantRecoveryJobs(db *sql.DB, repositoryID string) ([]DormantRecoveryJob, error) {
	rows, err := db.Query(`SELECT repository_id, job_id, definition_json
		FROM dormant_recovery_jobs WHERE repository_id = ? ORDER BY job_id`, repositoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []DormantRecoveryJob{}
	for rows.Next() {
		var value DormantRecoveryJob
		if err := rows.Scan(&value.RepositoryID, &value.JobID, &value.DefinitionJSON); err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, rows.Err()
}

func deleteDormantRecoveryJobEverywhere(tx *sql.Tx, jobID string) ([]string, error) {
	rows, err := tx.Query(`SELECT repository_id FROM dormant_recovery_jobs WHERE job_id = ?`, jobID)
	if err != nil {
		return nil, err
	}
	repositories := []string{}
	for rows.Next() {
		var repositoryID string
		if err := rows.Scan(&repositoryID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		repositories = append(repositories, repositoryID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`DELETE FROM dormant_recovery_jobs WHERE job_id = ?`, jobID); err != nil {
		return nil, err
	}
	return repositories, nil
}
