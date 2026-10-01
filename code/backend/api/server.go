package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/appupdate"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/desktop"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/integrations"
	"github.com/local/replicaro/jobscript"
	"github.com/local/replicaro/kopiapolicy"
	"github.com/local/replicaro/locale"
	"github.com/local/replicaro/metadata"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/operationlog"
	"github.com/local/replicaro/operationruntime"
	"github.com/local/replicaro/platforms"
	"github.com/local/replicaro/profilesync"
	"github.com/local/replicaro/rendezvous"
	"github.com/local/replicaro/runner"
	"github.com/local/replicaro/runtimeendpoint"
	"github.com/local/replicaro/scheduler"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultidentity"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
	"github.com/local/replicaro/vaultstatistics"
)

var applyDesktopSettings = desktop.ApplySettings

// Credential activation checks native access here; its callers also require
// the same physical vault address and matching protected root/profile records
// before saving credentials. If a different repository readable with the saved
// password replaced the old one at that address while the old sidecars
// remained, activation could still report success. Backup admission checks the
// actual native repository identity before any backup work. Catching this case
// during activation would need another remote probe on every reconnect, so it
// is left to that check.
var validateRotatedCredentials = func(ctx context.Context, repo models.Repository) error {
	engine, err := resolveEngine(repo)
	if err != nil {
		return err
	}
	_, err = engines.ValidateRepository(ctx, engine, repo)
	return err
}
var readProfileWithRotatedCredentials = func(ctx context.Context, repo models.Repository) ([]byte, error) {
	return (vaultprofile.Store{Repository: repo}).ForProfile(repo.ProfileUUID).
		WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).
		Read(ctx)
}

const approachingProfileSizeWarning = "Job saved. You are approaching the limit on the number of jobs per vault that Replicaro can safely support. Report this bug on Github so we can fix it. In the meanwhile, please do not add any new jobs."
const exceededProfileSizeWarning = "Replicaro cannot safely support the number of jobs you have. Report this bug on Github so we can fix it. In the meanwhile, please decrease the number of jobs."

var projectRepositoryProfileSize = profilesync.ProjectRepositoryProfileSize
var readVaultSizeStatus = vaultstatistics.StatusWithReader

func projectedProfileSizeWarning(db *sql.DB, affected map[string]bool) string {
	ids := make([]string, 0, len(affected))
	for id := range affected {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	sizes := make([]int, 0, len(ids))
	for _, id := range ids {
		size, err := projectRepositoryProfileSize(db, id)
		if err != nil {
			continue
		}
		sizes = append(sizes, size)
	}
	return projectedProfileSizeWarningForSizes(sizes)
}

func projectedProfileSizeWarningForSizes(sizes []int) string {
	approaching := false
	for _, size := range sizes {
		if size > vaultprofile.MaximumProfileDecryptedSize {
			return exceededProfileSizeWarning
		}
		if size >= 15<<20 {
			approaching = true
		}
	}
	if approaching {
		return approachingProfileSizeWarning
	}
	return ""
}

var readRootWithRotatedCredentials = func(ctx context.Context, repo models.Repository) ([]byte, error) {
	return (vaultprofile.Store{Repository: repo}).
		WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).
		Read(ctx)
}
var stageRepositoryArtifacts = engines.StageRepositoryArtifacts
var restoreRepositoryArtifacts = func(stage *engines.RepositoryArtifactStage) error { return stage.Restore() }
var finalizeRepositoryArtifacts = func(stage *engines.RepositoryArtifactStage) error { return stage.Finalize() }
var cleanupCreationEngineArtifacts = engines.CleanupRepositoryArtifacts
var removeCreationRcloneConfig = engines.RemoveRcloneVaultConfig
var removeDeletedRcloneConfig = engines.RemoveRcloneVaultConfig
var cleanupCreationNativeFence = cleanupNativeOperationFence
var cancelPreparedRepositoryCreation = database.CancelPreparedRepositoryCreation
var forgetRepositoryCreationIntent = database.ForgetRepositoryCreationIntent
var deleteRepository = database.DeleteRepository
var deleteRepositoryDiscardingPendingProfile = database.DeleteRepositoryDiscardingPendingProfile
var cancelRepositoryConnectionIntent = database.CancelRepositoryConnectionIntent
var updateRepositoryCredentials = database.UpdateRepositoryCredentials
var deleteBackupJob = database.DeleteJob
var assertRepositoryWriter = func(ctx context.Context, repo models.Repository) error {
	return (vaultprofile.Store{Repository: repo}).ForProfile(repo.ProfileUUID).
		WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).
		AssertAttachment(ctx, repo.ClientUUID, repo.AttachmentGeneration)
}
var runQueuedRepositoryTask = scheduler.RunQueuedRepositoryTask
var startOperationStep = database.StartOperationStep
var startMetadataMutationStep = database.StartMetadataMutationStep
var finishOperationStep = database.FinishOperationStep
var skipOperationStep = database.SkipOperationStep

func cleanupNativeOperationFence(db *sql.DB, fencePath string) error {
	if fencePath == "" {
		return nil
	}
	referenced, err := database.NativeOperationFenceReferenced(db, fencePath)
	if err != nil {
		return err
	}
	if referenced {
		return fmt.Errorf("native operation fence remains durably referenced")
	}
	// Both callers (attaching a created vault and forgetting a pending
	// creation) reach this after the same request has already shown the fence
	// inactive, so the cleanup normally passes on its first look. An unrelated
	// process launch can still hold an inherited copy of the descriptor for a
	// few milliseconds, and the after-close cleanup re-checks for a short,
	// bounded window to absorb that. A fence that stays held is refused and
	// left in place.
	return command.CleanupNativeProcessFenceAfterClose(fencePath)
}

// Cancelling a prepared creation removes its local artifacts before deleting
// the creation intent, the only durable record of them. On failure the intent
// is kept, visible and retryable, where possible.
func cleanupPreparedRepositoryCreation(db *sql.DB, intent database.RepositoryCreationIntent) error {
	if intent.Phase != database.RepositoryCreationPrepared {
		return fmt.Errorf("only a prepared repository creation can be cancelled")
	}
	markRetained := func(message string) {
		_ = database.MarkRepositoryCreationError(db, intent.ID, message)
	}
	if err := cleanupCreationEngineArtifacts(models.Repository{ID: intent.ID, Engine: intent.Engine}); err != nil {
		markRetained("Prepared creation local engine cleanup is incomplete; retry cancellation.")
		return fmt.Errorf("cancel pending creation local engine cleanup: %w", err)
	}
	if intent.Connector != engines.RcloneRemoteConnector {
		if err := removeCreationRcloneConfig(intent.ID); err != nil {
			markRetained("Prepared creation local rclone cleanup is incomplete; retry cancellation.")
			return fmt.Errorf("cancel pending creation local rclone cleanup: %w", err)
		}
	}
	if err := cancelPreparedRepositoryCreation(db, intent.ID); err != nil {
		markRetained("Prepared creation cleanup completed, but cancellation is incomplete; retry cancellation.")
		return err
	}
	return nil
}

func reviewedConnectorOptions(integration integrations.Integration, options map[string]string) map[string]string {
	credential := map[string]bool{}
	for _, option := range integration.Options {
		if option.Credential || option.Secret {
			credential[option.Key] = true
		}
	}
	reviewed := map[string]string{}
	for key, value := range options {
		if !credential[key] {
			reviewed[key] = value
		}
	}
	return reviewed
}

func validatedCreationProfile(intent database.RepositoryCreationIntent, clientUUID string) ([]byte, vaultprofile.Profile, error) {
	data := []byte(intent.ProfileJSON)
	hash := sha256.Sum256(data)
	profile, err := vaultprofile.Parse(data)
	if err != nil || hex.EncodeToString(hash[:]) != intent.ProfileSHA256 || profile.VaultUUID != intent.ID ||
		profile.Attachment.ClientUUID != clientUUID || profile.Attachment.Generation != 1 {
		return nil, vaultprofile.Profile{}, fmt.Errorf("pending creation profile does not match its durable vault identity")
	}
	return data, profile, nil
}

// Keep the implementation available for a future UI-backed feature, but do not
// expose ad-hoc snapshots without saved-job context and engine-native scope.
const adHocBackupsEnabled = false

// handle wraps a handler with CORS and OPTIONS preflight handling.
func handle(
	mux *http.ServeMux,
	pattern string,
	fn http.HandlerFunc,
) {
	mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
		if !secureAPIRequest(w, r) {
			return
		}

		fn(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func writeJSONStatus(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, err error) {
	var expected *requestDecodeError
	markSupportResponseError(w, err, errors.As(err, &expected))
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": errorToastMessage(err),
	})
}

func writeCodedError(w http.ResponseWriter, status int, code, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message, "code": code})
}

func badRequest(w http.ResponseWriter, message string) {
	markSupportResponseError(w, errors.New(message), true)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(map[string]string{
		"error": message,
	})
}

func decodeRequest(r *http.Request, target any) error {
	if err := requireJSONContentType(r); err != nil {
		return &requestDecodeError{err: err}
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return &requestDecodeError{err: err}
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return &requestDecodeError{err: fmt.Errorf("request contains multiple JSON values")}
		}
		return &requestDecodeError{err: fmt.Errorf("invalid trailing JSON: %w", err)}
	}
	return nil
}

type requestDecodeError struct{ err error }

func (value *requestDecodeError) Error() string { return value.err.Error() }
func (value *requestDecodeError) Unwrap() error { return value.err }

func combinedOperationOutput(output string, err error) string {
	if err == nil {
		return output
	}
	if strings.TrimSpace(output) == "" {
		return err.Error()
	}
	return output + "\n" + err.Error()
}

func engineJobSettingsEqual(left, right models.EngineJobSettings) bool {
	return slices.Equal(left.AdditionalOptions, right.AdditionalOptions)
}

func normalizeUpdatedJobSettings(submitted models.EngineSettings, represented, retainedEngines map[string]bool, unresolved bool, stored models.BackupJob) (models.EngineSettings, error) {
	if err := engines.ValidatePortableJobSettings(submitted, represented); err != nil {
		return nil, err
	}
	for engineID, submittedSection := range submitted {
		if represented[engineID] {
			continue
		}
		storedSection, existed := stored.EngineSettings[engineID]
		if !existed || !engineJobSettingsEqual(submittedSection, storedSection) {
			return nil, fmt.Errorf("engine settings for disconnected %s may only preserve an unchanged trusted section", engineID)
		}
	}
	normalized := models.EngineSettings{}
	for engineID := range represented {
		normalized[engineID] = submitted[engineID]
	}
	needed := map[string]bool{}
	for engineID := range represented {
		needed[engineID] = true
	}
	for engineID := range retainedEngines {
		needed[engineID] = true
	}
	for engineID := range stored.EngineSettings {
		if unresolved {
			needed[engineID] = true
		}
	}
	for engineID := range needed {
		if represented[engineID] {
			continue
		}
		storedSection, exists := stored.EngineSettings[engineID]
		if !exists {
			return nil, fmt.Errorf("trusted engine settings for retained %s vaults are unavailable", engineID)
		}
		normalized[engineID] = storedSection
	}
	if err := engines.ValidatePortableJobSettings(normalized, needed); err != nil {
		return nil, err
	}
	return normalized, nil
}

// repoFromRequest resolves the repository row for an ?id= query param.
func repoFromRequest(
	db *sql.DB,
	r *http.Request,
) (models.Repository, error) {

	id := r.URL.Query().Get("id")
	return repositoryByID(db, id)
}

func repositoryByID(db *sql.DB, id string) (models.Repository, error) {
	repoModel, err := database.GetRepository(db, id)
	if err != nil {
		return repoModel, err
	}
	if !models.ValidEngine(repoModel.Engine) {
		return repoModel, fmt.Errorf("vault engine is unsupported")
	}
	return repoModel, nil
}

var resolveEngine = func(repo models.Repository) (engines.Engine, error) {
	return engines.ResolveWithRepositoryAvailabilityCheck(
		repo, storageavailability.RequireRepositoryAvailable,
	)
}

func engineOutputError(output string, err error) error {
	if err == nil {
		return nil
	}
	return &engineCommandError{output: output, err: err}
}

func Handler(db *sql.DB) http.Handler {
	return HandlerAt(db, runtimeendpoint.Preferred(), rendezvous.Record{}, nil)
}

func HandlerAt(db *sql.DB, endpoint runtimeendpoint.Endpoint, record rendezvous.Record, activate func() error) http.Handler {
	return HandlerAtWithSecurityMode(db, endpoint, record, activate, SecurityModeProduction)
}

func HandlerAtWithSecurityMode(db *sql.DB, endpoint runtimeendpoint.Endpoint, record rendezvous.Record, activate func() error, mode SecurityMode) http.Handler {
	return handlerAtWithSecurityModeAndRcloneAuth(
		db, endpoint, record, activate, mode, newRcloneAuthStore(), appupdate.New(db),
	)
}

func handlerAtWithSecurityModeAndRcloneAuth(
	db *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	record rendezvous.Record,
	activate func() error,
	mode SecurityMode,
	rcloneAuth *rcloneAuthStore,
	updaters ...*appupdate.Service,
) http.Handler {
	updater := appupdate.New(db)
	if len(updaters) > 0 && updaters[0] != nil {
		updater = updaters[0]
	}
	return handlerAtWithSecurityModeRcloneAuthAndRuntime(
		db, db, endpoint, nil, record, activate, mode, rcloneAuth, updater, false, operationruntime.New(),
	)
}

func handlerAtWithSecurityModeRcloneAuthAndRuntime(
	db, readDB *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	lanOrigins runtimeendpoint.LANOrigins,
	record rendezvous.Record,
	activate func() error,
	mode SecurityMode,
	rcloneAuth *rcloneAuthStore,
	updater *appupdate.Service,
	keepStartAtLogin bool,
	runtimeManager *operationruntime.Manager,
) http.Handler {
	// This copy replaces the web UI handler's for every /api/ request, so it
	// needs the same LAN origins or proxied API calls fail the Host check.
	security := requestSecurity{
		endpoint: endpoint, lanOrigins: lanOrigins, mode: normalizedSecurityMode(mode), clientUUID: installationUUID(db),
	}
	handler := handlerForExecutionInstanceWithReader(db, readDB, uuid.NewString(), rcloneAuth, updater, keepStartAtLogin, runtimeManager)
	if record.Version != "" {
		mux := http.NewServeMux()
		activationHandler := rendezvous.Handler(record, activate)
		mux.HandleFunc(rendezvous.ActivationPath, func(w http.ResponseWriter, r *http.Request) {
			if secureAPIRequest(w, r) {
				activationHandler.ServeHTTP(w, r)
			}
		})
		mux.Handle("/", handler)
		handler = mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, withSecurity(r, security))
	})
}

func handlerForExecutionInstance(
	db *sql.DB,
	executionInstanceID string,
	rcloneAuth *rcloneAuthStore,
	updater *appupdate.Service,
	runtimeManagers ...*operationruntime.Manager,
) http.Handler {
	return handlerForExecutionInstanceWithReader(db, db, executionInstanceID, rcloneAuth, updater, false, runtimeManagers...)
}

// keepStartAtLogin is true only in headless service mode (Linux with
// --lan-origin or --rclone-auth-no-open-browser, outside the container
// package). There the Start at login setting is what keeps replicaro.service
// enabled: with it off, every settings apply and every start runs
// `systemctl --user disable replicaro.service`, and a headless install would
// silently not come back after the next reboot. Nothing in the Settings UI
// tells a remote user that, so the backend keeps the setting on and reports
// the capability as unavailable, which hides the option in the existing
// Settings page.
func handlerForExecutionInstanceWithReader(
	db, readDB *sql.DB,
	executionInstanceID string,
	rcloneAuth *rcloneAuthStore,
	updater *appupdate.Service,
	keepStartAtLogin bool,
	runtimeManagers ...*operationruntime.Manager,
) http.Handler {
	runtimeManager := operationruntime.New()
	if len(runtimeManagers) > 0 && runtimeManagers[0] != nil {
		runtimeManager = runtimeManagers[0]
	}
	mux := http.NewServeMux()
	registerAppUpdateHandlers(mux, updater, readDB)
	registerRcloneAuthHandlers(mux, db, rcloneAuth)
	// New vault creation has no preview reservation. The single intent begins
	// only when the create request reaches its non-atomic native boundary.
	handle(mux, "/api/vaults/connect/preview", handleExistingVaultPreview(db, rcloneAuth))
	handle(mux, "/api/vaults/connect/profile", handleExistingVaultProfileSelection(db, rcloneAuth))
	handle(mux, "/api/vaults/connect", handleExistingVaultConnect(db, rcloneAuth))
	handle(mux, "/api/vaults/connect/retry", handleExistingVaultRetry(db, rcloneAuth))
	handle(mux, rcloneRemoteListPath, handleRcloneRemoteList)
	handle(mux, "/api/health", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{"status": "ok"})
	})

	handle(mux, "/api/platform", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		target := platforms.Current()
		capabilities := desktop.Capabilities()
		if keepStartAtLogin {
			capabilities["startAtLogin"] = false
		}
		writeJSON(w, map[string]any{
			"platform":     target,
			"os":           runtime.GOOS,
			"arch":         runtime.GOARCH,
			"supported":    platforms.IsSupported(target),
			"capabilities": capabilities,
		})
	})

	handle(mux, "/api/integrations", func(w http.ResponseWriter, r *http.Request) {
		catalog, err := integrations.Load()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, catalog)
	})

	handle(mux, "/api/filesystem/directories", func(w http.ResponseWriter, r *http.Request) {
		listing, err := browseDirectoriesBounded(r.Context(), r.URL.Query().Get("path"))
		if errors.Is(err, errDirectoryNotResponding) {
			writeCodedError(w, http.StatusGatewayTimeout, "directory_not_responding", err.Error())
			return
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, listing)
	})

	// ------------------------------------------------------------------
	// Dashboard
	// ------------------------------------------------------------------

	handle(mux, "/api/dashboard", func(w http.ResponseWriter, r *http.Request) {

		stats, err := database.GetDashboardStats(readDB)

		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		writeJSON(w, stats)
	})

	handle(mux, "/api/dashboard/issues", func(w http.ResponseWriter, r *http.Request) {
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		issues, err := database.ListDashboardIssues(readDB, limit, r.URL.Query().Get("cursor"))
		if err != nil {
			if errors.Is(err, database.ErrInvalidDashboardIssueCursor) {
				writeError(w, http.StatusBadRequest, err)
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		writeJSON(w, issues)
	})

	handle(mux, "/api/dashboard/issues/review", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			badRequest(w, "invalid method")
			return
		}
		if err := database.MarkDashboardIssuesReviewed(db, time.Now()); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		stats, err := database.GetDashboardStats(db)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, stats)
	})

	handle(mux, "/api/dashboard/activity", func(w http.ResponseWriter, r *http.Request) {

		activity, err := database.RecentActivity(readDB)

		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		writeJSON(w, activity)
	})

	handle(mux, "/api/operations/active", func(w http.ResponseWriter, r *http.Request) {
		operations, err := database.ListActiveOperations(readDB)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, operations)
	})

	handle(mux, "/api/operations/live", func(w http.ResponseWriter, r *http.Request) {
		operationID := r.URL.Query().Get("id")
		parsed, parseErr := uuid.Parse(operationID)
		if parseErr != nil || parsed == uuid.Nil || parsed.String() != operationID {
			writeError(w, http.StatusBadRequest, errors.New("operation id is invalid"))
			return
		}
		operation, err := database.GetOperation(readDB, operationID)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		live := runtimeManager.Snapshot(operationID)
		if operation.Status != "queued" && operation.Status != "running" {
			live = operationruntime.Snapshot{}
		}
		if live.Entries == nil {
			live.Entries = []operationruntime.Entry{}
		}
		writeJSON(w, map[string]any{"operation": operation, "live": live})
	})

	// Keep the route neutral: browser privacy filters commonly classify a path
	// ending in "/log" as telemetry and can block this local, user-requested
	// operation output before it reaches Replicaro.
	handle(mux, "/api/operations/output", func(w http.ResponseWriter, r *http.Request) {
		const operationLogChunkBytes = 256 << 10
		w.Header().Set("Cache-Control", "no-store")
		operationID := r.URL.Query().Get("id")
		parsed, parseErr := uuid.Parse(operationID)
		if parseErr != nil || parsed == uuid.Nil || parsed.String() != operationID {
			writeError(w, http.StatusBadRequest, errors.New("operation id is invalid"))
			return
		}
		operation, err := database.GetOperation(readDB, operationID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, err)
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		ownedStepRunning := false
		for _, step := range operation.Steps {
			ownedStepRunning = ownedStepRunning || step.Status == "running"
		}
		if operation.Status == "queued" || operation.Status == "running" || ownedStepRunning {
			writeCodedError(w, http.StatusConflict, "operation_active", "operation log is available after completion")
			return
		}
		offset := int64(0)
		if rawOffset := r.URL.Query().Get("offset"); rawOffset != "" {
			parsedOffset, parseErr := strconv.ParseInt(rawOffset, 10, 64)
			if parseErr != nil || parsedOffset < 0 {
				writeError(w, http.StatusBadRequest, errors.New("operation log offset is invalid"))
				return
			}
			offset = parsedOffset
		}
		chunk, err := operationlog.ReadChunk(operationID, offset, operationLogChunkBytes)
		if err != nil {
			if errors.Is(err, operationlog.ErrInvalidChunkOffset) {
				writeError(w, http.StatusBadRequest, err)
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		writeJSON(w, map[string]any{"available": chunk.Available,
			"outputBase64":    base64.StdEncoding.EncodeToString(chunk.DisplayData),
			"rawOutputBase64": base64.StdEncoding.EncodeToString(chunk.Data),
			"offset":          chunk.Offset, "previousOffset": chunk.PreviousOffset,
			"nextOffset": chunk.NextOffset, "size": chunk.Size, "eof": chunk.EOF})
	})

	handle(mux, "/api/operations/cancel", func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			OperationID string `json:"operationId"`
		}
		if err := decodeRequest(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		parsed, parseErr := uuid.Parse(request.OperationID)
		if parseErr != nil || parsed == uuid.Nil || parsed.String() != request.OperationID {
			writeError(w, http.StatusBadRequest, errors.New("operation id is invalid"))
			return
		}
		operation, err := database.GetOperation(db, request.OperationID)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if operation.Status != "queued" && operation.Status != "running" {
			writeCodedError(w, http.StatusConflict, "operation_terminal", "operation is already complete")
			return
		}
		result, cancelErr := runtimeManager.Cancel(request.OperationID)
		if errors.Is(cancelErr, operationruntime.ErrNotFound) || errors.Is(cancelErr, operationruntime.ErrNotCancelable) {
			writeCodedError(w, http.StatusConflict, "operation_not_cancelable", "operation is not cancelable")
			return
		}
		if cancelErr != nil {
			writeCodedError(w, http.StatusInternalServerError, "cancel_retryable", "Could not cancel this operation. Try again.")
			return
		}
		writeJSON(w, map[string]any{"accepted": result.Accepted})
	})

	handle(mux, "/api/operations", func(w http.ResponseWriter, r *http.Request) {
		if operationID := r.URL.Query().Get("id"); operationID != "" {
			operation, err := database.GetOperation(readDB, operationID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeError(w, http.StatusNotFound, err)
				} else {
					writeError(w, http.StatusInternalServerError, err)
				}
				return
			}
			writeJSON(w, []models.Operation{operation})
			return
		}
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		operations, err := database.ListOperations(readDB, limit)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, operations)
	})

	// ------------------------------------------------------------------
	// Repositories (Kloset stores)
	// ------------------------------------------------------------------

	handle(mux, "/api/repositories", func(w http.ResponseWriter, r *http.Request) {

		switch r.Method {

		case http.MethodGet:

			repos, err := database.ListRepositories(readDB)

			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}

			writeJSON(w, repos)

		case http.MethodPost:
			handleRepositoryCreate(db, rcloneAuth, w, r)

		case http.MethodDelete:
			handleRepositoryRemoval(db, runtimeManager, w, r)
		}
	})

	handle(mux, "/api/repository-creation-intents", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			intents, err := database.ListRepositoryCreationIntents(readDB)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, intents)
		case http.MethodDelete:
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				badRequest(w, "pending creation id is required")
				return
			}
			intent, err := database.FindRepositoryCreationIntentByID(db, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeError(w, http.StatusNotFound, err)
				} else {
					writeError(w, http.StatusInternalServerError, err)
				}
				return
			}
			unlock, ok, lockErr := vaultlock.YieldLowPriorityAndTryExclusiveContext(r.Context(), intent.ID)
			if lockErr != nil {
				writeError(w, http.StatusConflict, lockErr)
				return
			}
			if !ok {
				writeError(w, http.StatusConflict, fmt.Errorf("pending creation is active; wait for it to finish"))
				return
			}
			defer unlock()
			if intent.Phase == database.RepositoryCreationPrepared {
				if err := cleanupPreparedRepositoryCreation(db, intent); err != nil {
					writeError(w, http.StatusInternalServerError, err)
					return
				}
			} else {
				if intent.NativeOperationID == "" {
					writeError(w, http.StatusConflict, fmt.Errorf("native process inactivity cannot be proven; creation remains recoverable"))
					return
				}
				fencePath, pathErr := database.RepositoryCreationFencePath(db, intent.NativeOperationID)
				if pathErr != nil {
					writeError(w, http.StatusInternalServerError, pathErr)
					return
				}
				inactive, proofErr := command.NativeProcessFenceInactive(fencePath)
				if proofErr != nil || !inactive {
					writeError(w, http.StatusConflict, fmt.Errorf("native process inactivity cannot be proven; creation remains recoverable"))
					return
				}
				// Forget removes only the exact local recovery state and artifacts.
				// Native repository content is never deleted or rolled back.
				if err := cleanupCreationEngineArtifacts(models.Repository{ID: intent.ID, Engine: intent.Engine}); err != nil {
					writeError(w, http.StatusInternalServerError, fmt.Errorf("forget pending creation local engine cleanup: %w", err))
					return
				}
				if intent.Connector != engines.RcloneRemoteConnector {
					if err := removeCreationRcloneConfig(intent.ID); err != nil {
						writeError(w, http.StatusInternalServerError, fmt.Errorf("forget pending creation local rclone cleanup: %w", err))
						return
					}
				}
				if err := forgetRepositoryCreationIntent(db, id); err != nil {
					writeError(w, http.StatusConflict, err)
					return
				}
				// Inactivity was proven above, so this normally passes at once. The
				// cleanup absorbs a momentary inherited holder and refuses a fence
				// that stays held.
				if err := cleanupCreationNativeFence(db, fencePath); err != nil {
					writeJSONStatus(w, http.StatusOK, map[string]string{
						"warning": "Pending creation was forgotten, but its inactive local creation fence still needs cleanup.",
					})
					return
				}
			}
			// There is no abandonment tombstone or completion archive; remote CAS,
			// leases, and heartbeats are intentionally outside this local workflow.
			w.WriteHeader(http.StatusNoContent)
		default:
			badRequest(w, "invalid method")
		}
	})

	handle(mux, "/api/repository-connection-intents", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			intents, err := database.ListRepositoryConnectionIntents(readDB)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, intents)
		case http.MethodDelete:
			id := strings.TrimSpace(r.URL.Query().Get("id"))
			if id == "" {
				badRequest(w, "pending connection id is required")
				return
			}
			intent, err := database.FindRepositoryConnectionIntentByID(db, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeError(w, http.StatusNotFound, err)
				} else {
					writeError(w, http.StatusInternalServerError, err)
				}
				return
			}
			vaultUUID, keyErr := connectionIntentVaultUUID(intent)
			if keyErr != nil {
				writeError(w, http.StatusInternalServerError, keyErr)
				return
			}
			unlock, ok, lockErr := vaultlock.YieldLowPriorityAndTryExclusiveContext(r.Context(), vaultUUID)
			if lockErr != nil {
				writeError(w, http.StatusConflict, lockErr)
				return
			}
			if !ok {
				writeError(w, http.StatusConflict, fmt.Errorf("pending connection is active; wait for it to finish before cancelling it"))
				return
			}
			defer unlock()
			err = cancelRepositoryConnectionIntent(db, id)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeError(w, http.StatusNotFound, err)
				} else if errors.Is(err, database.ErrRepositoryConnectionCannotCancel) {
					writeError(w, http.StatusConflict, fmt.Errorf("finish or retry the pending connection; external mutation may already have begun"))
				} else {
					writeError(w, http.StatusInternalServerError, err)
				}
				return
			}
			w.WriteHeader(http.StatusNoContent)
		}
	})

	handle(mux, "/api/dormant-recovery-jobs", func(w http.ResponseWriter, r *http.Request) {
		repositoryID := strings.TrimSpace(r.URL.Query().Get("repositoryId"))
		if repositoryID == "" {
			badRequest(w, "repositoryId is required")
			return
		}
		switch r.Method {
		case http.MethodGet:
			rows, err := database.ListDormantRecoveryJobs(readDB, repositoryID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			type item struct {
				RepositoryID string          `json:"repositoryId"`
				JobID        string          `json:"jobId"`
				Definition   json.RawMessage `json:"definition"`
			}
			result := make([]item, 0, len(rows))
			for _, row := range rows {
				result = append(result, item{row.RepositoryID, row.JobID, json.RawMessage(row.DefinitionJSON)})
			}
			writeJSON(w, result)
		case http.MethodPost:
			var req struct {
				JobID string `json:"jobId"`
			}
			if err := decodeRequest(r, &req); err != nil || strings.TrimSpace(req.JobID) == "" {
				badRequest(w, "jobId is required")
				return
			}
			job, err := database.PrepareDormantRecoveryJob(db, repositoryID, req.JobID)
			if err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			_, existingErr := database.GetJob(db, job.ID)
			if errors.Is(existingErr, sql.ErrNoRows) {
				if database.SourceBindingRecordsNoFacts(job) {
					job = firstBindRestoredSource(r.Context(), job)
				}
				if _, err := database.CreateJob(db, job); err != nil {
					writeError(w, http.StatusConflict, err)
					return
				}
			} else if existingErr != nil {
				writeError(w, http.StatusInternalServerError, existingErr)
				return
			} else if err := database.UpdateJob(db, job); err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			if err := database.ConsumeDormantRecoveryJob(db, repositoryID, req.JobID); err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
			profilesync.Wake(db)
			kopiapolicy.QueueDirty(db, repositoryID)
			saved, err := database.GetJob(db, job.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, map[string]any{"job": saved, "profilePending": true})
		case http.MethodDelete:
			jobID := strings.TrimSpace(r.URL.Query().Get("jobId"))
			if jobID == "" {
				badRequest(w, "jobId is required")
				return
			}
			if err := database.DiscardDormantRecoveryJob(db, repositoryID, jobID); err != nil {
				writeError(w, http.StatusNotFound, err)
				return
			}
			profilesync.Wake(db)
			writeJSON(w, map[string]any{"profilePending": true})
		}
	})

	handle(mux, "/api/repository", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("reconnect") == "true" {
			repoModel, err := database.GetRepository(readDB, r.URL.Query().Get("id"))
			if err != nil {
				writeError(w, http.StatusNotFound, err)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			fields := map[string]any{
				"connector": repoModel.Connector, "coldStorage": repoModel.ColdStorage,
				"archiveWriteClass": repoModel.ArchiveWriteClass, "location": repoModel.Location,
				"password": repoModel.Passphrase, "passwordConfirmation": repoModel.Passphrase,
				"options": repoModel.ConnectorOptions, "profile_uuid": repoModel.ProfileUUID,
				"expectedVaultUUID": repoModel.ID, "name": repoModel.Name, "description": repoModel.Description,
				"checkSchedule": repoModel.CheckSchedule, "maintenanceSchedule": repoModel.MaintenanceSchedule,
				"concurrencyMode": repoModel.ConcurrencyMode,
			}
			if repoModel.RcloneRemote != nil {
				// Like every storage type, options holds all saved settings,
				// secrets included, so Reconnect can be prefilled. rcloneRemote
				// is the same summary the vault list shows.
				fields["rcloneRemote"] = repoModel.RcloneRemote
			}
			writeJSON(w, fields)
			return
		}

		repoModel, err := repoFromRequest(readDB, r)

		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}

		writeJSON(w, repoModel)
	})

	handle(mux, "/api/repository/ownership", handleVaultOwnership(db))

	handle(mux, "/api/repository/password", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			repositoryID := strings.TrimSpace(r.URL.Query().Get("id"))
			if repositoryID == "" {
				badRequest(w, "repository id is required")
				return
			}
			result, err := runner.VaultPasswordChangeStatus(readDB, repositoryID)
			if err != nil {
				status := http.StatusInternalServerError
				if errors.Is(err, sql.ErrNoRows) {
					status = http.StatusNotFound
				}
				writeJSONStatus(w, status, map[string]any{"error": err.Error()})
				return
			}
			writeJSON(w, result)
			return
		}
		if r.Method != http.MethodPost {
			badRequest(w, "invalid method")
			return
		}
		var req struct {
			RepositoryID         string `json:"repositoryId"`
			NewPassword          string `json:"newPassword"`
			PasswordConfirmation string `json:"passwordConfirmation"`
		}
		if err := decodeRequest(r, &req); err != nil || strings.TrimSpace(req.RepositoryID) == "" {
			badRequest(w, "repositoryId and new vault password are required")
			return
		}
		if err := models.ValidateVaultPassword(req.NewPassword); err != nil {
			badRequest(w, err.Error())
			return
		}
		if req.NewPassword != req.PasswordConfirmation {
			badRequest(w, "the vault passwords do not match")
			return
		}
		startVaultPasswordChange(db, runtimeManager, w, strings.TrimSpace(req.RepositoryID), req.NewPassword, false)
	})

	handle(mux, "/api/repository/password/retry", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			badRequest(w, "invalid method")
			return
		}
		var req struct {
			RepositoryID string `json:"repositoryId"`
		}
		if err := decodeRequest(r, &req); err != nil || strings.TrimSpace(req.RepositoryID) == "" {
			badRequest(w, "repositoryId is required")
			return
		}
		startVaultPasswordChange(db, runtimeManager, w, strings.TrimSpace(req.RepositoryID), "", true)
	})

	handle(mux, "/api/repository/schedules", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			badRequest(w, "invalid method")
			return
		}
		var req RepositoryScheduleRequest
		if err := decodeRequest(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		startVaultSettingsSave(db, runtimeManager, w, req)
	})

	handle(mux, "/api/repository/credentials", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			badRequest(w, "invalid method")
			return
		}
		var req struct {
			RepositoryID string            `json:"repositoryId"`
			Options      map[string]string `json:"options"`
		}
		if err := decodeRequest(r, &req); err != nil || strings.TrimSpace(req.RepositoryID) == "" {
			badRequest(w, "repositoryId and connector credentials are required")
			return
		}
		repo, err := database.GetRepository(db, req.RepositoryID)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeAdmissionError := func(err error) {
			if errors.Is(err, database.ErrRepositoryConnectionReserved) ||
				errors.Is(err, database.ErrVaultPasswordChangeRecoveryRequired) {
				writeError(w, http.StatusConflict, err)
			} else {
				writeError(w, http.StatusNotFound, err)
			}
		}
		if err := database.ValidateRepositoryMutationAdmission(db, repo.ID); err != nil {
			writeAdmissionError(err)
			return
		}
		integration, ok := integrations.Find(repo.Connector)
		if !ok || repo.Connector == "fs" {
			badRequest(w, "this vault has no rotatable connector credentials")
			return
		}
		if repo.Engine == engines.ResticID &&
			engines.IsRcloneNativeLoginProvider(repo.Connector) {
			badRequest(w, "use native rclone account authorization for this vault")
			return
		}
		if repo.Connector == engines.RcloneRemoteConnector {
			// The rclone config password and the environment variables are
			// changed together with the rest of this vault's rclone settings,
			// through Reconnect, which checks them all again first.
			badRequest(w, "change this vault's rclone settings through Reconnect")
			return
		}
		allowed := map[string]bool{}
		for _, option := range integration.Options {
			if option.Credential {
				allowed[option.Key] = true
			}
		}
		if len(req.Options) == 0 {
			badRequest(w, "at least one connector credential is required")
			return
		}
		for key := range req.Options {
			if !allowed[key] {
				badRequest(w, "only credential fields can be changed here")
				return
			}
		}
		// Rotation runs in the foreground, so a busy vault is waited for
		// instead of refused. The checks above depend only on the request and
		// the connector, which is compared again below. Everything that depends
		// on the saved vault is done after the wait, from a fresh read: the
		// mutation admission (a reconnect or vault password change may have
		// started meanwhile) and the merge of the new credentials into the
		// saved connector options. Merging into the copy read before the wait
		// could write back options another change replaced while this waited.
		unlock, lockErr := vaultlock.AcquireExclusiveContext(r.Context(), repo.ID)
		if lockErr != nil {
			writeError(w, http.StatusConflict, lockErr)
			return
		}
		defer unlock()
		if err := database.ValidateRepositoryMutationAdmission(db, repo.ID); err != nil {
			writeAdmissionError(err)
			return
		}
		current, err := database.GetRepository(db, repo.ID)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if current.Engine != repo.Engine || current.Connector != repo.Connector {
			writeError(w, http.StatusConflict, fmt.Errorf("vault changed while the credential update waited"))
			return
		}
		repo = current
		merged := map[string]string{}
		for key, value := range repo.ConnectorOptions {
			merged[key] = value
		}
		for key, value := range req.Options {
			if strings.TrimSpace(value) == "" {
				delete(merged, key)
			} else {
				merged[key] = value
			}
		}
		normalized, normalizeErr := engines.NormalizeConnectorOptions(repo.Engine, integration, merged)
		if normalizeErr != nil {
			badRequest(w, normalizeErr.Error())
			return
		}
		oldIdentity, oldErr := vaultidentity.PhysicalIdentityWithOptions(repo.Connector, repo.Location, repo.ConnectorOptions)
		newIdentity, newErr := vaultidentity.PhysicalIdentityWithOptions(repo.Connector, repo.Location, normalized)
		if oldErr != nil || newErr != nil || oldIdentity != newIdentity {
			writeError(w, http.StatusConflict, fmt.Errorf("credential rotation cannot change the physical vault address"))
			return
		}
		if err := engines.ValidateConnectorAddress(repo.Engine, repo.Connector, repo.Location, normalized); err != nil {
			badRequest(w, err.Error())
			return
		}
		candidate := repo
		candidate.ConnectorOptions = normalized
		nativeCandidate := candidate
		if repo.Engine == engines.KopiaID || repo.Engine == engines.ResticID {
			nativeCandidate.ID = uuid.NewString()
			defer engines.CleanupPreviewArtifacts(nativeCandidate)
		}
		if err := validateRotatedCredentials(r.Context(), nativeCandidate); err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("new credentials could not access the native vault"))
			return
		}
		rootData, err := readRootWithRotatedCredentials(r.Context(), candidate)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("new credentials could not access the vault root"))
			return
		}
		root, err := vaultprofile.ParseRoot(rootData, repo.Connector)
		if err != nil || root.VaultUUID != repo.ID || root.Repository.Engine != repo.Engine || root.Repository.NativeRepositoryID != repo.NativeRepositoryID {
			writeError(w, http.StatusConflict, fmt.Errorf("new credentials address a different vault"))
			return
		}
		profileData, err := readProfileWithRotatedCredentials(r.Context(), candidate)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("new credentials could not access the recovery profile"))
			return
		}
		profile, err := vaultprofile.Parse(profileData)
		if err != nil || profile.VaultUUID != repo.ID || profile.ProfileUUID != repo.ProfileUUID ||
			profile.Attachment.ClientUUID != repo.ClientUUID || profile.Attachment.Generation != repo.AttachmentGeneration {
			writeError(w, http.StatusConflict, fmt.Errorf("new credentials address a different vault"))
			return
		}
		stage, err := stageRepositoryArtifacts(repo, engines.RepositoryArtifactCredentialRotation)
		if err != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("stale local engine credentials could not be staged: %w", err))
			return
		}
		if err := updateRepositoryCredentials(db, repo.ID, normalized); err != nil {
			if restoreErr := restoreRepositoryArtifacts(stage); restoreErr != nil {
				writeError(w, http.StatusInternalServerError, fmt.Errorf("credential update failed and old local engine artifacts could not be restored: %v; restore error: %w", err, restoreErr))
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// The new credentials are saved from here on. A quarantined old engine
		// artifact that cannot be removed makes the rotation completed with
		// issues (200 with the issue), not a failure: the rotation must not be
		// repeated, only the leftover cleaned up.
		var outcome foregroundOutcome
		if err := finalizeRepositoryArtifacts(stage); err != nil {
			outcome.addIssue(fmt.Sprintf("credentials were updated but quarantined old engine artifacts require manual cleanup: %v", err))
		}
		_ = database.LogActivity(db, "Repository connector credentials updated: "+repo.Name)
		if outcome.Status != "" {
			writeJSON(w, outcome)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})

	// There is deliberately no GET /api/repository/info. It ran a native
	// repository info command under the request and nothing in the UI called
	// it. The engines' Info stays, because vault validation and profile repair
	// use it.

	handle(mux, "/api/repository/check", func(w http.ResponseWriter, r *http.Request) {
		// Only the whole-vault check exists. The snapshotId form (Restic
		// `check --read-data <id>`, Kopia `snapshot verify <id>`, run inside this
		// request) had no caller and was removed rather than moved onto the
		// background operation path, so the engines' Check no longer takes a
		// snapshot. A client that still sends snapshotId is refused instead of
		// having the parameter ignored: quietly widening its request to a
		// whole-vault check would read the entire repository, which is not what
		// it asked for. This also means cold storage vaults have no integrity
		// check at all, since the whole-vault check was never available to them.
		if r.URL.Query().Get("snapshotId") != "" {
			writeError(w, http.StatusNotFound, errors.New("snapshot integrity checks are not available; omit snapshotId to check the whole vault"))
			return
		}

		repoModel, err := repoFromRequest(db, r)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		startManualRepositoryTask(db, runtimeManager, w, repoModel, "check")
	})

	handle(mux, "/api/repository/maintenance", func(w http.ResponseWriter, r *http.Request) {
		repoModel, err := repoFromRequest(db, r)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		startManualRepositoryTask(db, runtimeManager, w, repoModel, "maintenance")
	})

	// ------------------------------------------------------------------
	// Snapshots
	// ------------------------------------------------------------------

	handle(mux, "/api/repository/vault-size", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			badRequest(w, "invalid method")
			return
		}
		repoModel, err := repoFromRequest(readDB, r)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		status, err := readVaultSizeStatus(db, readDB, repoModel.ID)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, status)
	})

	handle(mux, "/api/repository/vault-size/prepare", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			badRequest(w, "invalid method")
			return
		}
		repoModel, err := repoFromRequest(db, r)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		force := r.URL.Query().Get("force") == "true"
		status, err := vaultstatistics.Prepare(db, repoModel.ID, force)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, status)
	})

	handle(mux, "/api/repository/snapshots", func(w http.ResponseWriter, r *http.Request) {

		repoModel, err := repoFromRequest(readDB, r)

		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		authority, err := database.LoadMetadataReadAuthority(r.Context(), readDB, repoModel.ID)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		listVisibleCached := func() ([]models.Snapshot, error) {
			knownJobs, jobsErr := database.JobIDsForRepository(readDB, repoModel.ID)
			if jobsErr != nil {
				return nil, jobsErr
			}
			snapshots, listErr := database.ListMetadataSnapshotsForAuthority(r.Context(), readDB, authority)
			if listErr != nil {
				return nil, listErr
			}
			visible := make([]models.Snapshot, 0, len(snapshots))
			for _, snapshot := range snapshots {
				snapshot = models.PresentSnapshot(snapshot, repoModel.ProfileUUID, knownJobs)
				if snapshot.Presentation != models.SnapshotPresentationHidden {
					visible = append(visible, snapshot)
				}
			}
			return visible, nil
		}
		writeVisible := func(snapshots []models.Snapshot) {
			type responseSnapshot struct {
				models.Snapshot
				MachineLabel string `json:"machineLabel,omitempty"`
			}
			response := make([]responseSnapshot, 0, len(snapshots))
			for _, snapshot := range snapshots {
				response = append(response, responseSnapshot{Snapshot: snapshot, MachineLabel: snapshot.PresentationMachineLabel})
			}
			writeJSON(w, response)
		}
		switch r.URL.Query().Get("mode") {
		case "cached":
			snapshots, listErr := listVisibleCached()
			if listErr != nil {
				if errors.Is(listErr, database.ErrMetadataIndexingUnavailable) {
					// Restore polls headers while observing the separate busy status.
					writeVisible(nil)
					return
				}
				writeError(w, http.StatusInternalServerError, listErr)
				return
			}
			writeVisible(snapshots)
			return
		case "":
			// File History calls this endpoint without a mode; keep returning the listing below for it.
		default:
			badRequest(w, "invalid snapshot listing mode")
			return
		}

		snapshots, err := listVisibleCached()
		if err != nil {
			if errors.Is(err, database.ErrMetadataIndexingUnavailable) {
				// Restore polls headers while observing the separate busy status.
				writeVisible(nil)
				return
			}
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeVisible(snapshots)
	})

	handle(mux, "/api/snapshot/files", func(w http.ResponseWriter, r *http.Request) {

		repoModel, err := repoFromRequest(readDB, r)

		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		snapshotID := r.URL.Query().Get("snapshotId")

		if snapshotID == "" {
			badRequest(w, "missing snapshotId")
			return
		}

		browsePath := normalizeSnapshotPath(r.URL.Query().Get("path"))
		authority, cacheErr := database.LoadMetadataReadAuthority(r.Context(), readDB, repoModel.ID)
		if cacheErr != nil {
			writeError(w, http.StatusInternalServerError, cacheErr)
			return
		}
		knownJobs, jobsErr := database.JobIDsForRepository(readDB, repoModel.ID)
		if jobsErr != nil {
			writeError(w, http.StatusInternalServerError, jobsErr)
			return
		}
		cachedSnapshot, entries, ready, cacheErr := database.ReadyMetadataSnapshotEntriesForAuthority(
			r.Context(), readDB, authority, snapshotID, browsePath,
		)
		if cacheErr != nil {
			if errors.Is(cacheErr, database.ErrMetadataIndexingUnavailable) {
				writeCodedError(w, http.StatusConflict, "metadata_not_ready", "backup data is still being prepared")
				return
			}
			writeError(w, http.StatusInternalServerError, cacheErr)
			return
		}
		if !ready {
			writeCodedError(w, http.StatusConflict, "metadata_not_ready", "backup data is still being prepared")
			return
		}
		cachedSnapshot = models.PresentSnapshot(cachedSnapshot, repoModel.ProfileUUID, knownJobs)
		if cachedSnapshot.Presentation == models.SnapshotPresentationHidden {
			writeError(w, http.StatusForbidden, fmt.Errorf("snapshot %q is absent or not visible to this vault profile", snapshotID))
			return
		}
		nativeRoot, rootErr := exactRestoreNativeRoot(cachedSnapshot, r.URL.Query().Get("nativeRootId"), true)
		if rootErr != nil {
			writeError(w, http.StatusUnprocessableEntity, rootErr)
			return
		}
		rootEntries := entries[:0]
		for _, entry := range entries {
			if entry.SourceRoot != nativeRoot.Path {
				continue
			}
			entry.NativeRootID = nativeRoot.Identity
			entry.NativeRootUser = nativeRoot.User
			entry.NativeRootHost = nativeRoot.Host
			rootEntries = append(rootEntries, entry)
		}
		writeJSON(w, map[string]any{
			"entries": rootEntries,
			"raw":     "Loaded from the metadata cache.",
		})
	})

	handle(mux, "/api/files/search", func(w http.ResponseWriter, r *http.Request) {
		fileSearch(db, readDB, w, r)
	})

	handle(mux, "/api/files/browse", func(w http.ResponseWriter, r *http.Request) {
		fileBrowse(db, readDB, w, r)
	})

	handle(mux, "/api/files/history", func(w http.ResponseWriter, r *http.Request) {
		fileHistory(db, readDB, w, r)
	})

	handle(mux, "/api/files/index/retry", func(w http.ResponseWriter, r *http.Request) {
		retryFileIndex(db, w, r)
	})

	handle(mux, "/api/metadata/status", func(w http.ResponseWriter, r *http.Request) {
		metadataStatus(db, readDB, w, r)
	})

	handle(mux, "/api/metadata/prepare", func(w http.ResponseWriter, r *http.Request) {
		prepareMetadata(db, readDB, w, r)
	})

	handle(mux, "/api/snapshot", func(w http.ResponseWriter, r *http.Request) {

		if r.Method != http.MethodDelete {
			badRequest(w, "invalid method")
			return
		}
		repoModel, err := repoFromRequest(db, r)
		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		snapshotID := r.URL.Query().Get("snapshotId")
		if snapshotID == "" {
			badRequest(w, "missing snapshotId")
			return
		}
		if err := engines.ValidateSnapshotIDArgument(snapshotID); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		startSnapshotDeletion(db, runtimeManager, w, repoModel, snapshotID)
	})

	handle(mux, "/api/restore", func(w http.ResponseWriter, r *http.Request) {

		if r.Method != http.MethodPost {
			badRequest(w, "invalid method")
			return
		}

		var req RestoreRequest

		if err := decodeRequest(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}

		if req.RepositoryID == "" || req.TargetPath == "" {
			badRequest(w, "repositoryId and targetPath are required")
			return
		}

		repoModel, err := repositoryByID(db, req.RepositoryID)

		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if req.Content != nil && (req.Content.Kind != "snapshot" ||
			req.Content.SnapshotID == "" || req.SnapshotID != "" && req.SnapshotID != req.Content.SnapshotID) {
			writeCodedError(w, http.StatusUnprocessableEntity, "unsupported_content_reference",
				"snapshot restore requires content {kind: \"snapshot\", snapshotId: ...}")
			return
		}
		if req.Content != nil {
			req.SnapshotID = req.Content.SnapshotID
		}
		if req.SnapshotID == "" {
			badRequest(w, "snapshotId is required")
			return
		}
		engine, engineErr := resolveEngine(repoModel)
		if engineErr != nil {
			writeError(w, http.StatusBadRequest, engineErr)
			return
		}
		restoreOptions := engines.RestoreOptions{Destination: req.TargetPath, Selection: req.Path, OriginalLocation: req.OriginalLocation, ConflictMode: req.ConflictMode}
		descriptor := engine.Descriptor()
		if descriptor.ID == "" {
			descriptor.ID = engine.ID()
		}
		if err := engines.ValidateRestoreCapability(descriptor, restoreOptions); err != nil {
			writeError(w, http.StatusUnprocessableEntity, err)
			return
		}
		if refuseWhilePasswordChangeUnfinished(w, db, repoModel.ID) {
			return
		}
		startRestore(db, runtimeManager, w, repoModel, req, restoreOptions)
	})

	handle(mux, "/api/restore-selection", func(w http.ResponseWriter, r *http.Request) {
		restoreSelection(db, runtimeManager, w, r)
	})

	// Ad-hoc snapshot of an arbitrary path into a repository.
	handle(mux, "/api/repository/snapshot", func(w http.ResponseWriter, r *http.Request) {
		if !adHocBackupsEnabled {
			writeError(w, http.StatusNotFound, errors.New("ad-hoc backups are disabled"))
			return
		}

		if r.Method != http.MethodPost {
			badRequest(w, "invalid method")
			return
		}

		var req AdHocBackupRequest

		if err := decodeRequest(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}

		if req.RepositoryID == "" || req.Source == "" {
			badRequest(w, "repositoryId and source are required")
			return
		}

		repoModel, err := repositoryByID(db, req.RepositoryID)

		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		started := time.Now()
		operationID, operationErr := database.StartOperation(
			db, "backup", "Backup: "+req.Source, "", repoModel.ID, started,
		)
		if operationErr != nil {
			writeError(w, http.StatusInternalServerError, operationErr)
			return
		}
		engine, engineErr := resolveEngine(repoModel)
		if engineErr != nil {
			if finishErr := finishOperationDurably(db, operationID, "failed", engineErr.Error(), time.Now()); finishErr != nil {
				writeError(w, http.StatusInternalServerError, finishErr)
				return
			}
			writeError(w, http.StatusBadRequest, engineErr)
			return
		}
		unlock, ok, lockErr := vaultlock.YieldLowPriorityAndTryExclusiveContext(r.Context(), repoModel.ID)
		if lockErr != nil || !ok {
			if lockErr == nil {
				lockErr = errors.New("vault is busy with another operation")
			}
			if finishErr := finishOperationDurably(db, operationID, "failed", lockErr.Error(), time.Now()); finishErr != nil {
				writeError(w, http.StatusInternalServerError, finishErr)
				return
			}
			writeError(w, http.StatusConflict, errors.New("vault is busy with another operation"))
			return
		}
		defer unlock()
		metadataGeneration, stepErr := startMetadataMutationStep(db, operationID, repoModel.ID, "backup", time.Now())
		if stepErr != nil {
			_ = finishOperationDurably(db, operationID, "failed", "persist native backup start: "+stepErr.Error(), time.Now())
			writeError(w, http.StatusInternalServerError, stepErr)
			return
		}
		nativeContext, nativeProcessStarted := command.ContextWithProcessStartTracking(r.Context())
		snapshot, output, nativeErr := engine.Backup(nativeContext, repoModel, req.Source, engines.BackupOptions{})
		processStarted := nativeProcessStarted()
		operationStatus, stageStarted, _, _, _, stageKnown := engines.RequestedOperationOutcome(nativeErr)
		stageSucceeded := nativeErr == nil
		if stageKnown {
			processStarted = stageStarted
			stageSucceeded = operationStatus == engines.RequestedOperationSucceeded
		}
		err = nativeErr
		if stepErr := finishTrackedNativeStep(db, operationID, "backup", output, nativeErr, processStarted); stepErr != nil {
			err = errors.Join(err, fmt.Errorf("persist native backup result: %w", stepErr))
		}
		if stepErr := startOperationStep(db, operationID, "application", "metadata_cache", time.Now()); stepErr != nil {
			err = errors.Join(err, fmt.Errorf("persist metadata-cache application start: %w", stepErr))
		} else {
			cacheStatus, cacheOutput := "skipped", "native backup outcome was ambiguous; metadata remains invalid"
			var cacheErr error
			if stageSucceeded && snapshot.ID != "" {
				cacheErr = database.ApplyMetadataBackupSnapshot(db, repoModel.ID, metadataGeneration, snapshot)
				cacheStatus, cacheOutput = "succeeded", "conclusive backup header added to metadata cache"
				if cacheErr == nil {
					metadata.ScheduleRepositoryEntryIndex(db, repoModel, snapshot.ID)
				}
			} else if !processStarted {
				var covered bool
				covered, cacheErr = database.AcknowledgeMetadataGeneration(db, repoModel.ID, metadataGeneration)
				if covered && cacheErr == nil {
					cacheStatus, cacheOutput = "succeeded", "no native backup started; metadata generation acknowledged"
				} else if cacheErr == nil {
					cacheStatus, cacheOutput = "skipped", "no native backup started; metadata generation remains unapplied behind an earlier metadata cache gap"
				}
			}
			if cacheErr != nil {
				cacheStatus, cacheOutput = "warning", "metadata cache update failed after native backup: "+cacheErr.Error()
			}
			if finishErr := finishOperationStep(db, operationID, "metadata_cache", cacheStatus, cacheOutput, time.Now()); finishErr != nil {
				err = errors.Join(err, fmt.Errorf("persist metadata-cache application result: %w", finishErr))
			}
		}
		status := terminalOperationStatus(r.Context(), err)
		operationOutput := combinedOperationOutput(output, err)
		var dispatchNotification func()
		if status != "interrupted" {
			dispatchNotification = prepareOperationNotification(db, operationID, "backup", "Backup: "+req.Source, status)
		}
		if finishErr := finishOperationDurably(db, operationID, status, operationOutput, time.Now()); finishErr != nil {
			writeError(w, http.StatusInternalServerError, finishErr)
			return
		}
		if dispatchNotification != nil {
			dispatchNotification()
		}

		if err != nil {
			responseStatus := http.StatusInternalServerError
			if errors.Is(err, storageavailability.ErrRepositoryStorageUnavailable) {
				responseStatus = http.StatusConflict
			}
			writeError(
				w,
				responseStatus,
				&engineCommandError{output: output, err: err},
			)
			return
		}
		writeJSON(w, map[string]string{"output": output})
	})

	// ------------------------------------------------------------------
	// Backup jobs
	// ------------------------------------------------------------------

	handle(mux, "/api/jobs", func(w http.ResponseWriter, r *http.Request) {

		switch r.Method {

		case http.MethodGet:

			jobs, err := database.ListJobs(readDB)

			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}

			writeJSON(w, jobs)

		case http.MethodPost, http.MethodPut:

			var req JobRequest

			if err := decodeRequest(r, &req); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			if r.Method == http.MethodPost && strings.TrimSpace(req.ID) != "" {
				badRequest(w, "job id is assigned by the server")
				return
			}

			req.Name = strings.TrimSpace(req.Name)
			// Source is a filesystem route, unlike the display name. Trimming or
			// host-normalizing it here can redirect the job before source binding
			// rejects parent traversal and validates the exact chosen route.

			if req.Name == "" || req.Source == "" || len(req.RepositoryIDs) == 0 {
				badRequest(w, "name, source and repositoryIds are required")
				return
			}
			if !database.ValidJobSchedule(req.Schedule) {
				badRequest(w, "invalid backup schedule")
				return
			}
			retentionDefinition := models.BackupJob{
				Retention: req.Retention, RetentionHourly: req.RetentionHourly,
				RetentionDaily: req.RetentionDaily, RetentionWeekly: req.RetentionWeekly,
				RetentionMonthly: req.RetentionMonthly, RetentionYearly: req.RetentionYearly,
			}
			if err := models.ValidateRetentionPolicy(retentionDefinition); err != nil {
				badRequest(w, err.Error())
				return
			}
			if models.ContainsReservedOwnershipTag(req.Tag) {
				badRequest(w, models.ReservedSnapshotTagError)
				return
			}
			if err := jobscript.ValidateDefinition(req.BeforeScriptPath, req.BeforeScriptMustSucceed,
				req.AfterScriptPath, req.AfterScriptMustSucceed); err != nil {
				badRequest(w, err.Error())
				return
			}
			engineSettings := req.EngineSettings
			if engineSettings == nil {
				badRequest(w, "engineSettings is required")
				return
			}
			var stored models.BackupJob
			if r.Method == http.MethodPut {
				if req.ID == "" {
					badRequest(w, "missing job id")
					return
				}
				var storedErr error
				stored, storedErr = database.GetJob(db, req.ID)
				if storedErr != nil {
					if errors.Is(storedErr, sql.ErrNoRows) {
						writeError(w, http.StatusNotFound, storedErr)
					} else {
						writeError(w, http.StatusInternalServerError, storedErr)
					}
					return
				}
				if req.Source != stored.Source && stored.SourceBindingState != "unbound_imported" {
					writeError(w, http.StatusConflict, database.ErrJobSourceImmutable)
					return
				}
				if req.Enabled != nil && *req.Enabled != stored.Enabled {
					writeCodedError(w, http.StatusConflict, "job_enabled_state_conflict",
						"backup job enabled state can only be changed with the dedicated enabled-state control")
					return
				}
			}
			represented := map[string]bool{}
			for _, repositoryID := range req.RepositoryIDs {
				repository, repositoryErr := database.GetRepository(db, strings.TrimSpace(repositoryID))
				if repositoryErr != nil {
					badRequest(w, "repository target does not exist")
					return
				}
				represented[repository.Engine] = true
			}
			var validationErr error
			if r.Method == http.MethodPut {
				newTargets := make([]models.BackupJobTarget, 0, len(req.RepositoryIDs))
				for _, repositoryID := range req.RepositoryIDs {
					newTargets = append(newTargets, models.BackupJobTarget{RepositoryID: strings.TrimSpace(repositoryID)})
				}
				retainedEngines, unresolved, retainedErr := database.RetainedPortableTargetEngines(db, req.ID, newTargets)
				if retainedErr != nil {
					writeError(w, http.StatusInternalServerError, retainedErr)
					return
				}
				engineSettings, validationErr = normalizeUpdatedJobSettings(engineSettings, represented, retainedEngines, unresolved, stored)
			} else {
				validationErr = engines.ValidateJobSettings(engineSettings, represented)
			}
			if validationErr != nil {
				badRequest(w, validationErr.Error())
				return
			}

			enabled := true
			if r.Method == http.MethodPut {
				enabled = stored.Enabled
			}
			if r.Method == http.MethodPost && req.Enabled != nil {
				enabled = *req.Enabled
			}

			job := models.BackupJob{
				ID:                      req.ID,
				Name:                    req.Name,
				Source:                  req.Source,
				Targets:                 make([]models.BackupJobTarget, 0, len(req.RepositoryIDs)),
				Schedule:                req.Schedule,
				Retention:               req.Retention,
				RetentionHourly:         req.RetentionHourly,
				RetentionDaily:          req.RetentionDaily,
				RetentionWeekly:         req.RetentionWeekly,
				RetentionMonthly:        req.RetentionMonthly,
				RetentionYearly:         req.RetentionYearly,
				Excludes:                req.Excludes,
				Tag:                     req.Tag,
				BeforeScriptPath:        req.BeforeScriptPath,
				BeforeScriptMustSucceed: req.BeforeScriptMustSucceed,
				AfterScriptPath:         req.AfterScriptPath,
				AfterScriptMustSucceed:  req.AfterScriptMustSucceed,
				EngineSettings:          engineSettings,
				Enabled:                 enabled,
			}
			if r.Method == http.MethodPost {
				job, validationErr = bindJobSourceStorage(r.Context(), job)
			} else {
				job.SourceStorageVersion = stored.SourceStorageVersion
				job.SourceStorageKey = stored.SourceStorageKey
				job.SourceStorageDescriptorJSON = stored.SourceStorageDescriptorJSON
				job.SourceBindingState = stored.SourceBindingState
			}
			if validationErr != nil {
				if r.Method == http.MethodPost {
					markSupportStorageObservation(w, storageavailability.StageJobCreateSource, validationErr)
				}
				writeError(w, http.StatusConflict, validationErr)
				return
			}
			for _, repositoryID := range req.RepositoryIDs {
				job.Targets = append(job.Targets, models.BackupJobTarget{RepositoryID: strings.TrimSpace(repositoryID)})
			}
			affectedRepositories := map[string]bool{}
			for _, target := range job.Targets {
				affectedRepositories[target.RepositoryID] = true
			}
			if r.Method == http.MethodPut {
				for _, target := range stored.Targets {
					affectedRepositories[target.RepositoryID] = true
				}
			}
			var id string
			definitionChanged := true
			if r.Method == http.MethodPut {
				id = job.ID
				if err := database.UpdateJob(db, job); err != nil {
					if errors.Is(err, database.ErrJobDefinitionBusy) {
						writeJobBeingDeleted(w)
						return
					}
					if errors.Is(err, database.ErrJobRunActive) ||
						errors.Is(err, database.ErrJobConnectionReserved) ||
						errors.Is(err, database.ErrJobSourceChanged) || errors.Is(err, database.ErrJobSourceImmutable) ||
						errors.Is(err, database.ErrJobNameExists) {
						writeError(w, http.StatusConflict, err)
					} else if strings.Contains(err.Error(), "conflicting Kopia") {
						writeCodedError(w, http.StatusConflict, "kopia_policy_conflict", err.Error())
					} else {
						writeError(w, http.StatusInternalServerError, err)
					}
					return
				}
			} else {
				var err error
				id, err = database.CreateJob(db, job)
				if err != nil {
					if errors.Is(err, database.ErrJobNameExists) ||
						strings.Contains(err.Error(), "conflicting Kopia") {
						writeError(w, http.StatusConflict, err)
					} else {
						writeError(w, http.StatusInternalServerError, err)
					}
					return
				}
			}
			for repositoryID := range affectedRepositories {
				kopiapolicy.QueueDirty(db, repositoryID)
			}
			profilesync.Wake(db)
			saved, err := database.GetJob(db, id)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			if r.Method == http.MethodPut {
				definitionChanged = !database.SavedJobDefinitionEqual(stored, saved)
			}
			response := map[string]any{"id": id, "job": saved, "profilePending": true}
			if definitionChanged {
				profileRepositories, profileRepositoriesErr := database.ProfileRepositoryIDsForJob(db, id)
				if profileRepositoriesErr == nil {
					for _, repositoryID := range profileRepositories {
						affectedRepositories[repositoryID] = true
					}
					if warning := projectedProfileSizeWarning(db, affectedRepositories); warning != "" {
						response["warning"] = warning
					}
				}
			}
			if r.Method == http.MethodPost {
				writeJSONStatus(w, http.StatusCreated, response)
			} else {
				writeJSON(w, response)
			}

		case http.MethodDelete:
			jobID := r.URL.Query().Get("id")
			if jobID == "" {
				badRequest(w, "missing job id")
				return
			}
			startJobDeletion(db, runtimeManager, w, jobID)
		}
	})

	handle(mux, "/api/jobs/source", handleJobSourceUpdate(db))

	handle(mux, "/api/jobs/run", func(w http.ResponseWriter, r *http.Request) {

		jobID := r.URL.Query().Get("id")

		if jobID == "" {
			badRequest(w, "missing job id")
			return
		}

		job, err := database.GetJob(db, jobID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		if job.SourceBindingState == "unbound_imported" {
			expectedSource := job.Source
			job, err = bindImportedJobSourceStorage(r.Context(), job)
			if err != nil {
				markSupportStorageObservation(w, storageavailability.StageJobBindSource, err)
				writeError(w, http.StatusConflict, err)
				return
			}
			if err = database.BindImportedJobSource(db, expectedSource, job); err != nil {
				if errors.Is(err, database.ErrJobDefinitionBusy) {
					writeJobBeingDeleted(w)
					return
				}
				if errors.Is(err, database.ErrJobConnectionReserved) ||
					errors.Is(err, database.ErrJobSourceChanged) || errors.Is(err, database.ErrJobSourceImmutable) {
					writeError(w, http.StatusConflict, err)
				} else {
					writeError(w, http.StatusInternalServerError, err)
				}
				return
			}
		}
		repositoryID := strings.TrimSpace(r.URL.Query().Get("repositoryId"))
		selected := make([]models.BackupJobTarget, 0, len(job.Targets))
		for _, target := range job.Targets {
			if repositoryID == "" || target.RepositoryID == repositoryID {
				selected = append(selected, target)
			}
		}
		if len(selected) == 0 {
			writeError(w, http.StatusNotFound, sql.ErrNoRows)
			return
		}
		repositories := make([]models.Repository, 0, len(selected))
		for _, target := range selected {
			repo, repoErr := database.GetRepository(db, target.RepositoryID)
			if repoErr != nil {
				writeError(w, http.StatusInternalServerError, repoErr)
				return
			}
			repositories = append(repositories, repo)
		}
		sourceObservation, targetObservations := storageavailability.ObserveBackupSet(r.Context(), job, repositories)
		if err := r.Context().Err(); err != nil {
			writeError(w, http.StatusRequestTimeout, err)
			return
		}
		now := time.Now().UTC()
		results, err := runner.AdmitPersistedOperations(db, func() ([]database.TargetAdmissionResult, error) {
			return database.AdmitManualBackupTargets(db, database.ManualAdmissionRequest{
				JobID: job.ID, Now: now, Source: sourceObservation, Targets: targetObservations,
			})
		})
		if errors.Is(err, runner.ErrAdmissionClosed) {
			writeError(w, http.StatusServiceUnavailable, err)
			return
		}
		if errors.Is(err, database.ErrBackupTriggerChanged) {
			writeError(w, http.StatusConflict, err)
			return
		}
		if errors.Is(err, database.ErrJobDefinitionBusy) {
			writeJobBeingDeleted(w)
			return
		}
		if writeVaultWorkRefusal(w, err) {
			return
		}
		if errors.Is(err, database.ErrJobConnectionReserved) {
			writeError(w, http.StatusConflict, err)
			return
		}
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, err)
			return
		}

		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		type manualTargetResult struct {
			RepositoryID string `json:"repositoryId"`
			Status       string `json:"status"`
			ReasonCode   string `json:"reasonCode,omitempty"`
			OperationID  string `json:"operationId,omitempty"`
		}
		response := make([]manualTargetResult, 0, len(results))
		admitted := 0
		for _, result := range results {
			status := result.Status
			if status == database.AdmissionAdmittedRegular || status == database.AdmissionAdmittedCatchUp {
				status = "admitted"
				admitted++
			}
			response = append(response, manualTargetResult{
				RepositoryID: result.RepositoryID, Status: status,
				ReasonCode: result.ReasonCode, OperationID: result.OperationID,
			})
		}
		writeJSONStatus(w, http.StatusAccepted, map[string]any{
			"status": "admission_complete", "count": admitted, "results": response,
		})
	})

	handle(mux, "/api/jobs/status", func(w http.ResponseWriter, r *http.Request) {
		targets, err := runner.ActiveTargetStatuses(readDB)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, map[string]any{
			"running": runner.RunningJobs(),
			"targets": targets,
		})
	})

	handle(mux, "/api/jobs/enabled", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}

		jobID := strings.TrimSpace(r.URL.Query().Get("id"))

		if jobID == "" {
			badRequest(w, "missing job id")
			return
		}
		var req struct {
			Enabled *bool `json:"enabled"`
		}
		if err := decodeRequest(r, &req); err != nil || req.Enabled == nil {
			badRequest(w, "enabled is required")
			return
		}

		job, err := database.GetJob(db, jobID)

		if err != nil {
			writeError(w, http.StatusNotFound, err)
			return
		}
		if *req.Enabled && !job.Enabled {
			if err := jobscript.ValidateDefinition(job.BeforeScriptPath, job.BeforeScriptMustSucceed,
				job.AfterScriptPath, job.AfterScriptMustSucceed); err != nil {
				badRequest(w, err.Error())
				return
			}
		}

		var sourceBinding *models.BackupJob
		if *req.Enabled && job.SourceBindingState == "unbound_imported" {
			bound, bindErr := bindImportedJobSourceStorage(r.Context(), job)
			if bindErr != nil {
				markSupportStorageObservation(w, storageavailability.StageJobBindSource, bindErr)
				writeError(w, http.StatusConflict, bindErr)
				return
			}
			sourceBinding = &bound
		}
		changed, err := database.SetJobEnabledWithSourceBindingCommitted(db, jobID, *req.Enabled, job.Source, sourceBinding)
		if err != nil {
			if errors.Is(err, database.ErrJobDefinitionBusy) {
				writeJobBeingDeleted(w)
				return
			}
			if errors.Is(err, database.ErrJobConnectionReserved) ||
				errors.Is(err, database.ErrJobSourceChanged) || errors.Is(err, database.ErrJobSourceImmutable) || errors.Is(err, database.ErrJobSourceUnbound) {
				writeError(w, http.StatusConflict, err)
			} else if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, err)
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}

		if changed {
			status := "disabled"
			if *req.Enabled {
				status = "enabled"
			}
			_ = database.LogActivity(db, "Backup job "+status+": "+job.Name)
			profilesync.Wake(db)
		}
		writeJSON(w, map[string]any{
			"enabled": *req.Enabled, "changed": changed, "profilePending": changed,
		})
	})

	// ------------------------------------------------------------------
	// Engine, logs, settings
	// ------------------------------------------------------------------

	handle(mux, "/api/engines", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"engines": engines.AllDescriptors(r.Context()), "replicaroVersion": models.ReplicaroVersion})
	})
	handle(mux, supportReportPath, handleSupportReport(readDB))

	handle(mux, "/api/engine", func(w http.ResponseWriter, r *http.Request) {
		descriptors := engines.AllDescriptors(r.Context())
		settings, settingsErr := database.GetSettings(readDB)
		if settingsErr != nil {
			writeError(w, http.StatusInternalServerError, settingsErr)
			return
		}
		used := map[string]bool{settings.DefaultEngine: true}
		if repos, reposErr := database.ListRepositories(readDB); reposErr == nil {
			for _, repo := range repos {
				used[repo.Engine] = true
			}
		}
		byID := map[string]engines.Descriptor{}
		for _, descriptor := range descriptors {
			byID[descriptor.ID] = descriptor
		}
		primary := byID[settings.DefaultEngine]
		healthy := true
		for id := range used {
			if descriptor, ok := byID[id]; !ok || !descriptor.Installed || descriptor.Error != "" {
				healthy = false
			}
		}
		writeJSON(w, map[string]any{"installed": primary.Installed, "healthy": healthy, "path": primary.Path, "version": primary.Version, "replicaroVersion": models.ReplicaroVersion, "engines": descriptors})
	})

	handle(mux, "/api/logs", func(w http.ResponseWriter, r *http.Request) {

		if r.Method == http.MethodDelete {

			if err := database.ClearActivity(db); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}

			w.WriteHeader(http.StatusNoContent)
			return
		}

		logs, err := database.ListActivity(readDB)

		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}

		writeJSON(w, logs)
	})

	handle(mux, "/api/settings", func(w http.ResponseWriter, r *http.Request) {

		switch r.Method {

		case http.MethodGet:

			settings, err := database.GetSettings(readDB)

			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}

			settings.EffectiveLocale = locale.Effective(settings.Language)
			// Resolve the OS preference separately only when another language
			// is saved; on macOS each resolution runs a bounded `defaults read`.
			settings.SystemLocale = settings.EffectiveLocale
			if settings.Language != models.LanguageSystem {
				settings.SystemLocale = locale.Effective(models.LanguageSystem)
			}
			writeJSON(w, settings)

		case http.MethodPost:

			var req SaveSettingsRequest

			if err := decodeRequest(r, &req); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			if req.DefaultEngine != "" && !models.ValidEngine(req.DefaultEngine) {
				badRequest(w, "unsupported default engine: "+req.DefaultEngine)
				return
			}
			if !models.ValidTheme(req.Theme) {
				badRequest(w, "unsupported theme: "+req.Theme)
				return
			}
			language := ""
			if req.Language != nil {
				language = *req.Language
				if !models.ValidLanguage(language) {
					badRequest(w, "unsupported language: "+language)
					return
				}
			}
			if req.LogRetentionDays < 1 {
				badRequest(w, "log retention must be at least one day")
				return
			}
			if req.MaxConcurrentJobRuns == 0 {
				req.MaxConcurrentJobRuns = database.DefaultMaxConcurrentJobRuns
			}
			if !database.ValidMaxConcurrentJobRuns(req.MaxConcurrentJobRuns) {
				badRequest(w, "max concurrent job runs must be between 1 and 32")
				return
			}
			err := database.SaveSettings(db, models.Settings{
				DefaultEngine:                req.DefaultEngine,
				AutoStart:                    req.AutoStart,
				LogRetentionDays:             req.LogRetentionDays,
				Theme:                        req.Theme,
				Language:                     language,
				WebhookURL:                   req.WebhookURL,
				NotifyWindowsOnSuccess:       req.NotifyWindowsOnSuccess || req.NativeNotificationsOnSuccess,
				NotifyWindowsOnFailure:       req.NotifyWindowsOnFailure || req.NativeNotificationsOnFailure,
				NativeNotificationsOnSuccess: req.NativeNotificationsOnSuccess || req.NotifyWindowsOnSuccess,
				NativeNotificationsOnFailure: req.NativeNotificationsOnFailure || req.NotifyWindowsOnFailure,
				NotifyWebhookOnSuccess:       req.NotifyWebhookOnSuccess,
				NotifyWebhookOnFailure:       req.NotifyWebhookOnFailure,
				// In headless service mode a request cannot turn Start at login off;
				// see keepStartAtLogin above.
				StartWithWindows:             req.StartWithWindows || req.StartAtLogin || keepStartAtLogin,
				StartAtLogin:                 req.StartAtLogin || req.StartWithWindows || keepStartAtLogin,
				MinimizeToTray:               req.MinimizeToTray,
				MaxConcurrentJobRuns:         req.MaxConcurrentJobRuns,
				DisableAutomaticUpdateChecks: req.DisableAutomaticUpdateChecks,
			})

			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			updater.AutomaticSettingsChanged()

			// Reconfiguration must retain the one runtime shared with this API;
			// replacing it would orphan active log and cancellation handles.
			runner.ConfigureWithRuntime(db, req.MaxConcurrentJobRuns, runtimeManager)

			if err := applyDesktopSettings(db); err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}

			_ = database.LogActivity(db, "Settings updated")

			w.WriteHeader(http.StatusOK)
		}
	})

	handle(mux, "/api/vault-profile-sync", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			states, err := database.ListVaultProfileSyncStatuses(readDB)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			writeJSON(w, states)
		case http.MethodPost:
			var req struct {
				RepositoryID string `json:"repositoryId"`
			}
			if err := decodeRequest(r, &req); err != nil || strings.TrimSpace(req.RepositoryID) == "" {
				badRequest(w, "repositoryId is required")
				return
			}
			if err := database.RetryVaultProfileSync(db, req.RepositoryID); err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					writeError(w, http.StatusNotFound, err)
				} else {
					writeError(w, http.StatusInternalServerError, err)
				}
				return
			}
			// Retry only makes the pending update due now and wakes the profile
			// queue; it does not publish the profile itself. Running the sync here
			// used to tie the remote write (and a wait for the vault lock) to this
			// request, so a closed browser or proxy timeout could cut it off. The
			// queue already owns the lock, backoff and retries, and the vault card
			// shows the pending state until the queue clears it, so the sync should
			// not move back into this handler.
			profilesync.Wake(db)
			writeJSONStatus(w, http.StatusAccepted, map[string]any{"profilePending": true})
		}
	})

	handler := withVaultProgress(recordSupportResponseErrors(db, mux))
	clientUUID := installationUUID(db)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		security := securityFromRequest(r)
		security.clientUUID = clientUUID
		handler.ServeHTTP(w, withSecurity(r, security))
	})
}

func NewServer(db *sql.DB) *http.Server {
	return newServer(db, 5*time.Second)
}

func NewServerAt(db *sql.DB, endpoint runtimeendpoint.Endpoint, record rendezvous.Record, activate func() error) *http.Server {
	return newServerAt(db, endpoint, record, activate, 5*time.Second)
}

// NewServerAtWithRcloneAuthShutdown gives the application explicit ownership
// of the private rclone authorization lifecycle. The returned shutdown
// function must be called synchronously after HTTP requests drain and before
// later application teardown.
func NewServerAtWithRcloneAuthShutdown(
	db *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	record rendezvous.Record,
	activate func() error,
	updaters ...*appupdate.Service,
) (*http.Server, func() error) {
	updater := appupdate.New(db)
	if len(updaters) > 0 && updaters[0] != nil {
		updater = updaters[0]
	}
	return buildServerAtWithSecurityMode(
		db, db, endpoint, nil, record, activate, 5*time.Second, SecurityModeProduction, updater, nil, false,
	)
}

func NewServerAtWithRcloneAuthShutdownManualBrowser(
	db *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	record rendezvous.Record,
	activate func() error,
	rcloneAuthNoOpenBrowser bool,
	updaters ...*appupdate.Service,
) (*http.Server, func() error) {
	updater := appupdate.New(db)
	if len(updaters) > 0 && updaters[0] != nil {
		updater = updaters[0]
	}
	return buildServerAtWithSecurityMode(
		db, db, endpoint, nil, record, activate, 5*time.Second, SecurityModeProduction, updater, nil, false,
		rcloneAuthNoOpenBrowser,
	)
}

func NewServerAtWithRcloneAuthShutdownManualBrowserAndRuntime(
	db *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	record rendezvous.Record,
	activate func() error,
	rcloneAuthNoOpenBrowser bool,
	runtimeManager *operationruntime.Manager,
	updaters ...*appupdate.Service,
) (*http.Server, func() error) {
	updater := appupdate.New(db)
	if len(updaters) > 0 && updaters[0] != nil {
		updater = updaters[0]
	}
	return buildServerAtWithSecurityMode(
		db, db, endpoint, nil, record, activate, 5*time.Second, SecurityModeProduction, updater, runtimeManager, false,
		rcloneAuthNoOpenBrowser,
	)
}

// NewServerAtWithReaderAndRuntime keeps all mutations and coordinator identity
// on db while routing reviewed API queries through readDB. lanOrigins (from
// --lan-origin) widen only the Host/Origin checks; the listener, endpoint, and
// rendezvous identity stay loopback. keepStartAtLogin must be true exactly in
// headless service mode.
func NewServerAtWithReaderAndRuntime(
	db, readDB *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	lanOrigins runtimeendpoint.LANOrigins,
	record rendezvous.Record,
	activate func() error,
	rcloneAuthNoOpenBrowser bool,
	keepStartAtLogin bool,
	runtimeManager *operationruntime.Manager,
	updaters ...*appupdate.Service,
) (*http.Server, func() error) {
	updater := appupdate.New(db)
	if len(updaters) > 0 && updaters[0] != nil {
		updater = updaters[0]
	}
	return buildServerAtWithSecurityMode(
		db, readDB, endpoint, lanOrigins, record, activate, 5*time.Second, SecurityModeProduction, updater, runtimeManager,
		keepStartAtLogin, rcloneAuthNoOpenBrowser,
	)
}

// NewContainerServerAtWithReaderAndRuntime keeps the ordinary public
// loopback-origin security contract while enabling the package's temporary
// container-side relay for pinned rclone's exact loopback OAuth callback.
// lanOrigins are accepted as in the host build. There is no keepStartAtLogin
// here: the container package never manages a login service, so Start at
// login behaves as it always has even with --lan-origin or
// --rclone-auth-no-open-browser.
func NewContainerServerAtWithReaderAndRuntime(
	db, readDB *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	lanOrigins runtimeendpoint.LANOrigins,
	record rendezvous.Record,
	activate func() error,
	rcloneAuthRelayPort int,
	runtimeManager *operationruntime.Manager,
	updaters ...*appupdate.Service,
) (*http.Server, func() error) {
	updater := appupdate.New(db)
	if len(updaters) > 0 && updaters[0] != nil {
		updater = updaters[0]
	}
	return buildContainerServerAtWithSecurityMode(
		db, readDB, endpoint, lanOrigins, record, activate, 5*time.Second, SecurityModeProduction,
		updater, runtimeManager, rcloneAuthRelayPort,
	)
}

func newServer(db *sql.DB, readHeaderTimeout time.Duration) *http.Server {
	return newServerAt(db, runtimeendpoint.Preferred(), rendezvous.Record{}, nil, readHeaderTimeout)
}

func newServerAt(db *sql.DB, endpoint runtimeendpoint.Endpoint, record rendezvous.Record, activate func() error, readHeaderTimeout time.Duration) *http.Server {
	return newServerAtWithSecurityMode(db, endpoint, record, activate, readHeaderTimeout, SecurityModeProduction)
}

func NewServerAtWithSecurityMode(db *sql.DB, endpoint runtimeendpoint.Endpoint, record rendezvous.Record, activate func() error, mode SecurityMode) *http.Server {
	return newServerAtWithSecurityMode(db, endpoint, record, activate, 5*time.Second, mode)
}

func newServerAtWithSecurityMode(db *sql.DB, endpoint runtimeendpoint.Endpoint, record rendezvous.Record, activate func() error, readHeaderTimeout time.Duration, mode SecurityMode) *http.Server {
	server, closeRcloneAuth := buildServerAtWithSecurityMode(
		db, db, endpoint, nil, record, activate, readHeaderTimeout, mode, appupdate.New(db), nil, false,
	)
	server.RegisterOnShutdown(func() {
		if err := closeRcloneAuth(); err != nil {
			log.Print("Replicaro could not remove every closed rclone authorization session; private cleanup will be retried at next startup")
		}
	})
	return server
}

func buildServerAtWithSecurityMode(
	db, readDB *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	lanOrigins runtimeendpoint.LANOrigins,
	record rendezvous.Record,
	activate func() error,
	readHeaderTimeout time.Duration,
	mode SecurityMode,
	updater *appupdate.Service,
	runtimeManager *operationruntime.Manager,
	keepStartAtLogin bool,
	manualRcloneBrowser ...bool,
) (*http.Server, func() error) {
	rcloneAuth := newRcloneAuthStore(manualRcloneBrowser...)
	return buildServerWithRcloneAuth(
		db, readDB, endpoint, lanOrigins, record, activate, readHeaderTimeout, mode, updater, runtimeManager,
		rcloneAuth, keepStartAtLogin,
	)
}

func buildContainerServerAtWithSecurityMode(
	db, readDB *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	lanOrigins runtimeendpoint.LANOrigins,
	record rendezvous.Record,
	activate func() error,
	readHeaderTimeout time.Duration,
	mode SecurityMode,
	updater *appupdate.Service,
	runtimeManager *operationruntime.Manager,
	rcloneAuthRelayPort int,
) (*http.Server, func() error) {
	return buildServerWithRcloneAuth(
		db, readDB, endpoint, lanOrigins, record, activate, readHeaderTimeout, mode, updater, runtimeManager,
		newContainerRcloneAuthStore(rcloneAuthRelayPort), false,
	)
}

func buildServerWithRcloneAuth(
	db, readDB *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	lanOrigins runtimeendpoint.LANOrigins,
	record rendezvous.Record,
	activate func() error,
	readHeaderTimeout time.Duration,
	mode SecurityMode,
	updater *appupdate.Service,
	runtimeManager *operationruntime.Manager,
	rcloneAuth *rcloneAuthStore,
	keepStartAtLogin bool,
) (*http.Server, func() error) {
	server := &http.Server{
		Handler: applicationHandlerAtWithSecurityModeAndRcloneAuth(
			db, readDB, endpoint, lanOrigins, record, activate, mode, rcloneAuth, updater, keepStartAtLogin, runtimeManager,
		),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       15 * time.Second,
		// Engine operations may legitimately run for hours. Response deadlines
		// are governed by request cancellation and application shutdown instead.
		WriteTimeout: 0,
		IdleTimeout:  60 * time.Second,
	}
	return server, rcloneAuth.closeAll
}

func normalizedSecurityMode(mode SecurityMode) SecurityMode {
	if mode == SecurityModeDevelopment {
		return SecurityModeDevelopment
	}
	return SecurityModeProduction
}

// engineCommandError attaches native CLI output to a failed command so the UI
// can show the engine's actual message rather than only "exit status 1".
type engineCommandError struct {
	output string
	err    error
}

func (e *engineCommandError) Error() string {
	if errors.Is(e.err, engines.ErrColdStorageArchivedObject) {
		return models.ColdStorageArchivedObjectHelp
	}

	if e.output != "" {
		return e.output
	}
	return e.err.Error()
}

func (e *engineCommandError) Unwrap() error { return e.err }
