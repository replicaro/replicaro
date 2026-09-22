package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/integrations"
	"github.com/local/replicaro/kopiapolicy"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/profilesync"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
)

func explainObjectLockProviderRequirement(connector string, err error) error {
	return engines.ExplainKopiaObjectLockProviderRequirement(connector, err)
}

func normalizeCreateRepositoryDefaults(req *CreateRepositoryRequest) error {
	if req.CheckSchedule == "" {
		req.CheckSchedule = "manual"
	}
	var err error
	req.ConcurrencyMode, err = models.NormalizeConcurrencyModeForConnector(req.Connector, req.ConcurrencyMode)
	if err != nil {
		return err
	}
	if req.MaintenanceSchedule == "" {
		req.MaintenanceSchedule = "daily"
	}
	return nil
}

func normalizeCreateColdStorage(req *CreateRepositoryRequest) error {
	archiveWriteClass, err := models.NormalizeColdStorage(req.Engine, req.Connector, req.ColdStorage, req.ArchiveWriteClass)
	if err != nil {
		return err
	}
	req.ArchiveWriteClass = archiveWriteClass
	if req.ColdStorage {
		req.CheckSchedule = "manual"
	}
	return nil
}

func integrationSupportsVaultStorage(integration integrations.Integration) bool {
	return integration.Supports("storage")
}

func normalizeResticRcloneCreateRequest(req *CreateRepositoryRequest) error {
	if _, ok := engines.RcloneProvider(req.Connector); !ok {
		return nil
	}
	if req.Engine != "" && req.Engine != engines.ResticID {
		return fmt.Errorf("the selected storage type requires engine restic")
	}
	req.Engine = engines.ResticID
	_, root, err := engines.ValidateRcloneFolderName(req.Location)
	if err != nil {
		return err
	}
	req.Location = root
	return nil
}

func pendingCreationValidationFailure(output string, validationErr error) (int, string) {
	if engines.RepositoryAccessRejected(output) {
		return http.StatusBadRequest, "The supplied keys/credentials are either wrong or lack the correct permissions at the storage provider. Correct them and retry this action."
	}
	if errors.Is(validationErr, storageavailability.ErrRepositoryStorageUnavailable) {
		return http.StatusConflict, "The pending native repository storage is unavailable. Restore access and retry this pending creation."
	}
	return http.StatusBadGateway, "The pending native repository could not be validated. Correct any connection or credential issue and retry this pending creation."
}

// Creation stores the same authenticated native identity that ordinary
// repository admission will later recompute. Existing-vault recovery keeps
// its separate root-marker fallback contract.
func creationNativeRepositoryIdentity(ctx context.Context, repo models.Repository, validationOutput string) (string, error) {
	if repo.Engine == engines.ResticID && engines.IsResticRcloneConnector(repo.Connector) {
		return engines.ResticRcloneRepositoryFingerprint(ctx, repo)
	}
	if repo.Connector == "fs" {
		if err := requirePersistedRepositoryStorageAvailable(ctx, repo); err != nil {
			return "", err
		}
	}
	return engines.RepositoryFingerprint(repo, validationOutput)
}

func creationMaintenanceMutationContext(
	ctx context.Context, db *sql.DB, intent database.RepositoryCreationIntent,
	boundRepository models.Repository, reviewed map[string]string,
) context.Context {
	return engines.ContextWithKopiaMaintenanceMutationAdmission(ctx, func(context.Context) error {
		fresh, admissionErr := database.LoadRepositoryCreationRetry(db, intent.ID, boundRepository, reviewed)
		if admissionErr != nil {
			return admissionErr
		}
		// No protected root exists yet. The immutable, native-ready creation
		// intent is the one authorization record for this initial profile's
		// repository-wide maintenance value; re-read it after Kopia connects.
		if fresh.Phase != database.RepositoryCreationNativeReady ||
			fresh.NativeFingerprint != intent.NativeFingerprint ||
			fresh.NativeOperationID != intent.NativeOperationID ||
			fresh.PublicationOperationID != intent.PublicationOperationID ||
			fresh.ProfileSHA256 != intent.ProfileSHA256 || fresh.ProfileJSON != intent.ProfileJSON {
			return fmt.Errorf("repository creation authority changed before Kopia maintenance mutation")
		}
		return nil
	})
}

func writeRepositoryCreationSuccess(
	db *sql.DB,
	w http.ResponseWriter,
	id string,
	created bool,
	repo models.Repository,
	fencePath string,
	rcloneOutcome rcloneApplicationOutcome,
	finishAuthorization func(engines.RcloneConfigDisposition) error,
) {
	if engines.IsResticRcloneConnector(repo.Connector) {
		rcloneOutcome = attachedUsableRcloneOutcome(rcloneOutcome)
	}
	cleanupWarnings := []string{}
	if cleanupErr := cleanupCreationFenceWithRetry(db, fencePath); cleanupErr != nil {
		cleanupWarnings = append(cleanupWarnings, "The vault is attached, but its inactive local creation fence still needs cleanup.")
	}
	if authCleanupErr := finishAuthorization(rcloneOutcome.Activation.Disposition); authCleanupErr != nil {
		cleanupWarnings = append(cleanupWarnings, "The vault is attached, but its temporary native authorization session still needs cleanup.")
	}
	response := map[string]any{"id": id, "created": created}
	if len(cleanupWarnings) > 0 {
		response["warning"] = strings.Join(cleanupWarnings, " ")
	}
	if engines.IsResticRcloneConnector(repo.Connector) {
		addRcloneOutcome(response, rcloneOutcome)
	}
	writeJSONStatus(w, http.StatusCreated, response)
}

func handleRepositoryCreate(db *sql.DB, rcloneAuth *rcloneAuthStore, w http.ResponseWriter, r *http.Request) {
	var req CreateRepositoryRequest
	if err := decodeRequest(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := models.ValidateVaultPassword(req.Password); err != nil {
		badRequest(w, err.Error())
		return
	}
	if req.Password != req.PasswordConfirmation {
		badRequest(w, "the passwords do not match")
		return
	}
	req.CreationIntentID = strings.TrimSpace(req.CreationIntentID)
	req.Name = strings.TrimSpace(req.Name)
	// Filesystem route and S3 prefix whitespace can name different destinations.
	// Their dedicated validation runs without silently trimming the address.
	if req.Connector != "fs" && req.Connector != "s3" {
		req.Location = strings.TrimSpace(req.Location)
	}
	if err := database.ValidateRepositoryName(req.Name); err != nil {
		badRequest(w, err.Error())
		return
	}
	if err := normalizeCreateRepositoryDefaults(&req); err != nil {
		badRequest(w, err.Error())
		return
	}
	if err := normalizeResticRcloneCreateRequest(&req); err != nil {
		badRequest(w, err.Error())
		return
	}
	if req.Engine == "" {
		if settings, err := database.GetSettings(db); err == nil {
			req.Engine = settings.DefaultEngine
		}
		if req.Engine == "" {
			req.Engine = engines.ResticID
		}
	}
	if err := normalizeCreateColdStorage(&req); err != nil {
		badRequest(w, err.Error())
		return
	}
	if req.Name == "" || req.Location == "" {
		badRequest(w, "name and location are required")
		return
	}
	if !engines.SupportsConnector(req.Engine, req.Connector) {
		badRequest(w, "the selected engine does not support this vault storage type")
		return
	}
	integration, ok := integrations.Find(req.Connector)
	if !ok || !integrationSupportsVaultStorage(integration) {
		badRequest(w, "unsupported vault storage")
		return
	}

	rcloneConfigPath := ""
	var finishRcloneAuth func(engines.RcloneConfigDisposition) error
	reusingPublishedRcloneConfig := false
	if engines.IsResticRcloneConnector(req.Connector) && req.RcloneAuthSessionID == "" {
		if req.CreationIntentID == "" {
			badRequest(w, "native rclone authorization is required for a new vault")
			return
		}
		retryOptions, retryErr := engines.NormalizeConnectorOptions(req.Engine, integration, req.Options)
		var retryIntent database.RepositoryCreationIntent
		if retryErr == nil {
			retryIntent, rcloneConfigPath, retryErr = reusablePersistentRcloneCreation(
				r.Context(), db, req.CreationIntentID, req.Engine, req.Connector,
				req.Location, retryOptions,
			)
		}
		if retryErr != nil {
			badRequest(w, "the pending vault requires native rclone reauthorization before retry")
			return
		}
		req.Options = retryIntent.ReviewedOptions
		finishRcloneAuth = func(engines.RcloneConfigDisposition) error { return nil }
		reusingPublishedRcloneConfig = true
	} else {
		var err error
		req.Options, finishRcloneAuth, err = rcloneAuth.mergeOptionsForFinalTransaction(
			req.RcloneAuthSessionID, req.Connector, req.Options,
		)
		if err != nil {
			if engines.IsResticRcloneConnector(req.Connector) {
				writeRcloneAuthorizationError(w, http.StatusBadRequest,
					"native rclone authorization session is unavailable or not ready")
			} else {
				badRequest(w, err.Error())
			}
			return
		}
		if engines.IsResticRcloneConnector(req.Connector) {
			rcloneConfigPath, err = rcloneAuth.configPath(req.RcloneAuthSessionID, req.Connector)
			if err != nil {
				_ = finishRcloneAuth(engines.RcloneConfigRetained)
				writeRcloneAuthorizationError(w, http.StatusBadRequest,
					"native rclone authorization session configuration is unavailable")
				return
			}
		}
	}
	rcloneAuthorizationFinished := false
	finishAuthorization := func(disposition engines.RcloneConfigDisposition) error {
		rcloneAuthorizationFinished = true
		return finishRcloneAuth(disposition)
	}
	defer func() {
		if !rcloneAuthorizationFinished {
			_ = finishRcloneAuth(engines.RcloneConfigRetained)
		}
	}()

	options, err := engines.NormalizeConnectorOptions(req.Engine, integration, req.Options)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if req.Connector == "azblob" && options["connection_string"] == "" &&
		(options["account_name"] == "" || options["account_key"] == "") {
		badRequest(w, "Azure requires a connection string or an account name and key")
		return
	}
	if req.Connector == "gcs" && options["credentials_file"] != "" && options["credentials_json"] != "" {
		badRequest(w, "Google Cloud Storage accepts either a credentials file or credentials JSON, not both")
		return
	}
	if err := engines.ValidateConnectorAddress(req.Engine, req.Connector, req.Location, options); err != nil {
		badRequest(w, err.Error())
		return
	}
	if err := engines.ValidateExternalSSHAvailable(models.Repository{
		Engine: req.Engine, Connector: req.Connector, ConnectorOptions: options,
	}); err != nil {
		badRequest(w, err.Error())
		return
	}
	if !database.ValidSchedule(req.CheckSchedule) || !database.ValidSchedule(req.MaintenanceSchedule) {
		badRequest(w, "invalid repository task schedule")
		return
	}
	req.ObjectLock, err = models.NormalizeObjectLock(req.Engine, req.Connector, req.ObjectLock)
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	if req.ObjectLock.Enrolled && req.ObjectLock.Paused {
		badRequest(w, "object lock cannot start paused during vault creation")
		return
	}
	if !models.ObjectLockMaintenanceEligible(req.ObjectLock, req.MaintenanceSchedule) {
		req.MaintenanceSchedule, err = models.LongestEligibleObjectLockMaintenance(req.ObjectLock)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
	}

	boundRepository, err := bindRepositoryStorage(r.Context(), models.Repository{
		Name: req.Name, Engine: req.Engine, Connector: req.Connector, Location: req.Location,
		ColdStorage: req.ColdStorage, ArchiveWriteClass: req.ArchiveWriteClass,
		Description: req.Description, CheckSchedule: req.CheckSchedule,
		MaintenanceSchedule: req.MaintenanceSchedule, ConcurrencyMode: req.ConcurrencyMode, ObjectLock: req.ObjectLock,
		AutoUnlock: true, ConnectorOptions: options,
	})
	if err != nil {
		badRequest(w, err.Error())
		return
	}
	reviewed := reviewedConnectorOptions(integration, options)
	freshRequest := req.CreationIntentID == ""
	var intent database.RepositoryCreationIntent
	if freshRequest {
		if pending, findErr := findCreationIntentForBoundRepository(db, boundRepository); findErr == nil {
			writeJSONStatus(w, http.StatusConflict, map[string]any{
				"error": "A pending creation already owns this destination. Use its pending card to retry, cancel, or forget it.",
				"code":  "creation_pending", "intentId": pending.ID,
			})
			return
		} else if !errors.Is(findErr, sql.ErrNoRows) {
			writeError(w, http.StatusInternalServerError, findErr)
			return
		}
		intent, err = database.ReserveRepositoryCreation(db, boundRepository, reviewed)
	} else {
		intent, err = database.LoadRepositoryCreationRetry(db, req.CreationIntentID, boundRepository, reviewed)
	}
	if err != nil {
		switch {
		case errors.Is(err, database.ErrRepositoryCreationPending):
			pending, findErr := findCreationIntentForBoundRepository(db, boundRepository)
			if findErr == nil {
				writeJSONStatus(w, http.StatusConflict, map[string]any{
					"error": "A pending creation already owns this destination. Use its pending card to retry, cancel, or forget it.",
					"code":  "creation_pending", "intentId": pending.ID,
				})
				return
			}
			writeError(w, http.StatusConflict, err)
		case errors.Is(err, database.ErrRepositoryIdentityExists),
			errors.Is(err, database.ErrRepositoryNameExists),
			errors.Is(err, database.ErrRepositoryCreationSettingsMismatch):
			writeError(w, http.StatusConflict, err)
		case errors.Is(err, sql.ErrNoRows):
			writeError(w, http.StatusNotFound, err)
		default:
			writeError(w, http.StatusInternalServerError, err)
		}
		return
	}
	// The reserved ID is the managed vault's serialization identity throughout
	// creation and retry. Storage identity still gates destination admission,
	// but must not become a second lock that can split after path changes.
	unlocked, locked, lockErr := vaultlock.YieldLowPriorityAndTryExclusiveContext(r.Context(), intent.ID)
	if lockErr != nil {
		writeError(w, http.StatusConflict, lockErr)
		return
	}
	if !locked {
		writeError(w, http.StatusConflict, errors.New("this vault creation is busy"))
		return
	}
	defer unlocked()
	cancelFreshPrepared := freshRequest
	defer func() {
		if cancelFreshPrepared {
			_ = cleanupPreparedRepositoryCreation(db, intent)
		}
	}()

	if intent.ProfileJSON == "" {
		createdAt, _ := time.Parse(time.RFC3339Nano, intent.CreatedAt)
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		clientUUID, clientErr := database.InstallationID(db)
		if clientErr != nil {
			writeError(w, http.StatusInternalServerError, clientErr)
			return
		}
		preparedRepo := models.Repository{ID: intent.ID, Name: intent.Name, Engine: intent.Engine,
			Connector: intent.Connector, Location: intent.Location, Description: intent.Description,
			Passphrase: req.Password, ConnectorOptions: options, CheckSchedule: intent.CheckSchedule,
			MaintenanceSchedule: intent.MaintenanceSchedule, ConcurrencyMode: intent.ConcurrencyMode, ObjectLock: intent.ObjectLock,
			AutoUnlock: true, CreatedAt: createdAt,
			ProfileUUID: uuid.NewString(), AttachmentGeneration: 1, ClientUUID: clientUUID,
			RcloneConfigPath: rcloneConfigPath}
		preparedRepo = applyIntentStorage(preparedRepo, intent)
		profileData, profileErr := profilesync.BuildInitialProfile(preparedRepo, nil, nil)
		if profileErr != nil {
			writeError(w, http.StatusInternalServerError, profileErr)
			return
		}
		hash := sha256.Sum256(profileData)
		intent, profileErr = database.PrepareRepositoryCreationProfile(db, intent.ID,
			hex.EncodeToString(hash[:]), string(profileData))
		if profileErr != nil {
			writeError(w, http.StatusConflict, profileErr)
			return
		}
	}

	clientUUID, err := database.InstallationID(db)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	repoModel := models.Repository{ID: intent.ID, Engine: intent.Engine, Connector: intent.Connector,
		Location: intent.Location, Passphrase: req.Password, ConnectorOptions: options,
		ClientUUID: clientUUID, CheckSchedule: intent.CheckSchedule, MaintenanceSchedule: intent.MaintenanceSchedule,
		ConcurrencyMode: intent.ConcurrencyMode,
		ObjectLock:      intent.ObjectLock, RcloneConfigPath: rcloneConfigPath}
	repoModel = applyIntentStorage(repoModel, intent)
	engine, err := resolveEngine(repoModel)
	if err != nil {
		badRequest(w, err.Error())
		return
	}

	createdStore := false
	if intent.Phase == database.RepositoryCreationPrepared {
		nativeOperationID := uuid.NewString()
		fencePath, fenceErr := database.RepositoryCreationFencePath(db, nativeOperationID)
		if fenceErr != nil {
			writeError(w, http.StatusInternalServerError, fenceErr)
			return
		}
		fence, fenceErr := command.AcquireNativeProcessFence(fencePath)
		if fenceErr != nil {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("acquire native creation fence: %w", fenceErr))
			return
		}
		if !fence.Supported() {
			_ = fence.Close()
			_ = command.CleanupClosedNativeProcessFence(fencePath)
			writeError(w, http.StatusConflict, fmt.Errorf("native process fencing is unsupported"))
			return
		}
		// This is the sole empty-destination check and occurs at the last safe
		// point before the native create mutation.
		if preflightErr := runNewVaultPreflight(r.Context(), repoModel); preflightErr != nil {
			_ = fence.Close()
			_ = command.CleanupClosedNativeProcessFence(fencePath)
			_ = database.MarkRepositoryCreationError(db, intent.ID, preflightErr.Error())
			writeError(w, http.StatusConflict, preflightErr)
			return
		}
		intent, err = database.BeginRepositoryCreationNative(db, intent.ID, nativeOperationID)
		if err != nil {
			_ = fence.Close()
			_ = command.CleanupClosedNativeProcessFence(fencePath)
			writeError(w, http.StatusConflict, err)
			return
		}
		cancelFreshPrepared = false
		reportVaultProgress(r.Context(), "Creating the native encrypted repository...")
		output, createErr := engine.Create(command.WithNativeProcessFence(r.Context(), fence), repoModel)
		closeErr := fence.Close()
		if createErr != nil || closeErr != nil {
			_ = database.MarkRepositoryCreationError(db, intent.ID,
				"Native creation did not complete cleanly. Retry validates the exact operation; create will not be replayed.")
			nativeErr := engineOutputError(output, errors.Join(createErr, closeErr))
			if repoModel.ObjectLock.Enrolled {
				nativeErr = explainObjectLockProviderRequirement(repoModel.Connector, nativeErr)
			}
			writeError(w, http.StatusInternalServerError, nativeErr)
			return
		}
		createdStore = true
	}

	// Native create is never automatically replayed after native_started. Retry
	// first proves the matching inherited fence inactive, then validates the
	// exact native repository or reports that no repository was found.
	if intent.NativeOperationID == "" {
		writeError(w, http.StatusConflict, fmt.Errorf("pending creation has no native operation identity"))
		return
	}
	fencePath, err := database.RepositoryCreationFencePath(db, intent.NativeOperationID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	inactive, proofErr := command.NativeProcessFenceInactive(fencePath)
	if proofErr != nil || !inactive {
		message := "The matching native creation process or a descendant may still be running; wait and retry."
		_ = database.MarkRepositoryCreationError(db, intent.ID, message)
		writeError(w, http.StatusConflict, errors.New(message))
		return
	}
	validationOutput, validationErr := engines.ValidateRepository(r.Context(), engine, repoModel)
	if validationErr != nil {
		if engines.RepositoryMissing(repoModel, validationOutput) {
			message := "The native creation process is inactive, but no native repository was found. Forget this pending creation before starting a new attempt."
			_ = database.MarkRepositoryCreationError(db, intent.ID, message)
			writeError(w, http.StatusConflict, errors.New(message))
			return
		}
		status, message := pendingCreationValidationFailure(validationOutput, validationErr)
		_ = database.MarkRepositoryCreationError(db, intent.ID, message)
		writeError(w, status, errors.New(message))
		return
	}
	reportVaultProgress(r.Context(), "Verifying the native repository identity...")
	nativeFingerprint, err := creationNativeRepositoryIdentity(r.Context(), repoModel, validationOutput)
	if err != nil {
		_ = database.MarkRepositoryCreationError(db, intent.ID, "The exact native repository identity could not be verified.")
		writeError(w, http.StatusBadGateway, fmt.Errorf("identify created vault: %w", err))
		return
	}
	if intent.NativeFingerprint != "" && nativeFingerprint != intent.NativeFingerprint {
		message := "The native repository identity does not match this pending creation."
		_ = database.MarkRepositoryCreationError(db, intent.ID, message)
		writeError(w, http.StatusConflict, errors.New(message))
		return
	}
	if err := database.MarkRepositoryCreationNativeReady(db, intent.ID, nativeFingerprint); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	// Every post-create Kopia session must stay bound to the exact native
	// repository whose fingerprint was just durably admitted. The storage path
	// alone is insufficient if native repository contents are replaced in place.
	repoModel.NativeRepositoryID = nativeFingerprint
	intent, err = database.FindRepositoryCreationIntentByID(db, intent.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	if repoModel.Engine == engines.KopiaID {
		if err := requirePersistedRepositoryStorageAvailable(r.Context(), repoModel); err != nil {
			_ = database.MarkRepositoryCreationError(db, intent.ID, err.Error())
			writeError(w, http.StatusConflict, err)
			return
		}
		identityOutput, identityErr := engines.EnsureKopiaClientIdentity(r.Context(), engine, repoModel)
		if identityErr != nil {
			_ = database.MarkRepositoryCreationError(db, intent.ID, "Kopia client identity alignment is pending.")
			writeError(w, http.StatusConflict, engineOutputError(identityOutput, identityErr))
			return
		}
		maintenanceContext := creationMaintenanceMutationContext(r.Context(), db, intent, boundRepository, reviewed)
		ownerOutput, ownerErr := engines.EnsureKopiaMaintenanceOwner(maintenanceContext, engine, repoModel)
		if ownerErr != nil {
			_ = database.MarkRepositoryCreationError(db, intent.ID, "Kopia maintenance-owner alignment is pending.")
			writeError(w, http.StatusConflict, engineOutputError(ownerOutput, ownerErr))
			return
		}
	}

	reportVaultProgress(r.Context(), "Publishing and verifying the recovery profile...")
	profileData, preparedProfile, profileErr := validatedCreationProfile(intent, clientUUID)
	initialRepo := models.Repository{ID: intent.ID, Name: intent.Name, Engine: intent.Engine,
		Connector: intent.Connector, Location: intent.Location, Description: intent.Description,
		Passphrase: req.Password, ConnectorOptions: options, CheckSchedule: intent.CheckSchedule,
		MaintenanceSchedule: intent.MaintenanceSchedule, ConcurrencyMode: intent.ConcurrencyMode, ObjectLock: intent.ObjectLock,
		AutoUnlock: true, CreatedAt: preparedProfile.CreatedAt,
		ProfileUUID: preparedProfile.ProfileUUID, AttachmentGeneration: preparedProfile.Attachment.Generation,
		ClientUUID: clientUUID, NativeRepositoryID: nativeFingerprint, RcloneConfigPath: rcloneConfigPath}
	initialRepo = applyIntentStorage(initialRepo, intent)
	if profileErr == nil {
		profileErr = requirePersistedRepositoryStorageAvailable(r.Context(), initialRepo)
	}
	if profileErr == nil {
		var rootData []byte
		rootData, profileErr = profilesync.BuildInitialRoot(initialRepo)
		if profileErr == nil {
			// Root and profile have separate create-only publication/readback
			// boundaries, but share this attempt's native sidecar setup. A retry
			// still verifies either generation already published by this intent.
			profileErr = (vaultprofile.Store{Repository: initialRepo}).WithSession(r.Context(), func(store vaultprofile.Store) error {
				if err := store.PublishUnderLock(r.Context(), rootData, vaultprofile.PublishOptions{
					OperationID: intent.PublicationOperationID, CreateOnly: true,
				}); err != nil {
					return err
				}
				profileOperationID := uuid.NewSHA1(uuid.MustParse(intent.PublicationOperationID), []byte("initial-profile")).String()
				return store.ForProfile(initialRepo.ProfileUUID).PublishUnderLock(r.Context(), profileData, vaultprofile.PublishOptions{
					OperationID: profileOperationID, CreateOnly: true,
				})
			})
		}
	}
	if profileErr != nil {
		_ = database.MarkRepositoryCreationError(db, intent.ID,
			"The native vault exists, but protected root/profile publication is pending. Retry this exact creation.")
		writeError(w, http.StatusBadGateway,
			fmt.Errorf("vault repository exists but mandatory recovery metadata could not be published: %w", profileErr))
		return
	}

	completedRepo := initialRepo
	rcloneOutcome := rcloneApplicationOutcome{
		Activation: engines.RcloneConfigActivation{Disposition: engines.RcloneConfigRetained},
	}
	if engines.IsResticRcloneConnector(completedRepo.Connector) {
		if reusingPublishedRcloneConfig {
			rcloneOutcome.Activation.Disposition = engines.RcloneConfigActivated
		} else {
			activation, activationErr := rcloneAuth.publishConfig(
				r.Context(), req.RcloneAuthSessionID, completedRepo.Connector, completedRepo.ID,
			)
			rcloneOutcome.Activation = activation
			if activationErr != nil {
				publishErr := errors.Join(activationErr, finishAuthorization(activation.Disposition))
				_ = database.MarkRepositoryCreationError(db, intent.ID,
					"The native vault exists, but its native rclone configuration could not be activated. Retry this exact creation.")
				writeRcloneApplicationError(w, http.StatusInternalServerError, rcloneOutcome,
					fmt.Errorf("publish native rclone vault config: %w", publishErr))
				return
			}
		}
	}

	reportVaultProgress(r.Context(), "Finishing this computer's vault attachment...")
	id, err := database.CompleteRepositoryCreation(db, intent, completedRepo)
	if err != nil {
		_ = database.MarkRepositoryCreationError(db, intent.ID,
			"The native vault and protected recovery metadata exist, but local attachment is pending. Retry this exact creation.")
		var authorizationCleanupErr error
		if engines.IsResticRcloneConnector(completedRepo.Connector) {
			authorizationCleanupErr = finishAuthorization(rcloneOutcome.Activation.Disposition)
		}
		if rcloneOutcome.Activation.ConsumesAuthorization() {
			writeRcloneApplicationError(w, http.StatusInternalServerError, rcloneOutcome,
				errors.Join(fmt.Errorf("native rclone config activated but vault attachment failed: %w", err), authorizationCleanupErr))
		} else if errors.Is(err, database.ErrRepositoryIdentityExists) {
			writeError(w, http.StatusConflict, err)
		} else {
			writeJSONStatus(w, http.StatusInternalServerError, map[string]any{
				"error":       "The vault exists but local attachment failed; retry the exact pending creation.",
				"recoverable": true, "intentId": intent.ID,
			})
		}
		return
	}
	if completedRepo.Engine == engines.KopiaID {
		// Creation leaves policy dirty/not-ready for the one existing
		// post-attachment reconciler; startup QueueAll covers a crash here.
		kopiapolicy.QueueDirty(db, id)
	}
	// Attachment is already committed and truthful success must not be rewritten
	// by best-effort local fence or authorization-session cleanup.
	writeRepositoryCreationSuccess(db, w, id, createdStore, completedRepo, fencePath, rcloneOutcome, finishAuthorization)
}
