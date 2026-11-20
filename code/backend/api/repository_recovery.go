package api

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/integrations"
	"github.com/local/replicaro/kopiapolicy"
	"github.com/local/replicaro/metadata"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/profilesync"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/vaultidentity"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
	"github.com/local/replicaro/vaultstatistics"
)

type ExistingVaultStorage struct {
	Connector            string            `json:"connector"`
	ColdStorage          bool              `json:"coldStorage"`
	ArchiveWriteClass    string            `json:"archiveWriteClass,omitempty"`
	Location             string            `json:"location"`
	Password             string            `json:"password"`
	Options              map[string]string `json:"options"`
	RcloneAuthSessionID  string            `json:"rcloneAuthSessionId,omitempty"`
	RcloneConfigPath     string            `json:"-"`
	ProfileUUID          string            `json:"profile_uuid,omitempty"`
	FinalAdmission       bool              `json:"-"`
	DiscoverIdentityOnly bool              `json:"-"`
	ReviewJoin           bool              `json:"-"`

	// Accepted and ignored. A Check or Connect sent by an older page or
	// client can still carry the saved vault's UUID, and the strict decoder
	// would otherwise refuse the whole request. Check and Connect identify
	// the vault from its protected root alone, the same way for a first
	// connection and a Reconnect.
	IgnoredExpectedVaultUUID string `json:"expectedVaultUUID,omitempty"`
}

// kopiaRcloneProviderUnsupportedCode marks the preview error for a Kopia
// repository found on a Restic-only rclone provider. The UI replaces the
// English text with its catalog message, passing the provider name.
const kopiaRcloneProviderUnsupportedCode = "kopia_rclone_provider_unsupported"

type kopiaRcloneProviderError struct {
	provider string
}

func newKopiaRcloneProviderError(connector string) *kopiaRcloneProviderError {
	provider, _ := engines.RcloneProvider(connector)
	return &kopiaRcloneProviderError{provider: provider.Label}
}

func (err *kopiaRcloneProviderError) Error() string {
	return fmt.Sprintf("Replicaro does not support %[1]s for the Kopia engine, because Kopia is no longer actively maintaining its rclone support. Replicaro supports %[1]s using the Restic engine.", err.provider)
}

// existingVaultAttachmentChangedCode marks a Check of a saved vault whose
// profile in the vault is no longer this computer's current attachment: this
// computer's installation ID changed (a repair), or another computer took over
// the profile. Update existing vault can't continue then. The response names
// the saved vault, so Reconnect for that vault can offer to remove it from
// this computer and continue with the usual Connect choices (join or take
// over, as the vault's storage allows), which is what the user would
// otherwise do by hand with Remove anyway and Connect existing vault.
const existingVaultAttachmentChangedCode = "existing_vault_attachment_changed"

type existingVaultAttachmentChangedError struct{ vaultID string }

func (*existingVaultAttachmentChangedError) Error() string {
	return "the selected copy does not contain the exact current vault attachment"
}

// writeExistingVaultPreviewError answers a failed connection check. Only the
// Kopia provider message, the changed attachment of a saved vault and the Any
// Rclone Remote checks carry a code; every other error keeps its text.
func writeExistingVaultPreviewError(w http.ResponseWriter, err error) {
	markSupportStorageObservation(w, "vault_connect_destination", err)
	if writeRcloneRemoteError(w, err) {
		return
	}
	var unsupported *kopiaRcloneProviderError
	if errors.As(err, &unsupported) {
		markSupportResponseError(w, err, false)
		writeCodedError(w, http.StatusBadRequest, kopiaRcloneProviderUnsupportedCode, err.Error())
		return
	}
	var changed *existingVaultAttachmentChangedError
	if errors.As(err, &changed) {
		markSupportResponseError(w, err, false)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": err.Error(), "code": existingVaultAttachmentChangedCode, "vaultId": changed.vaultID,
		})
		return
	}
	writeError(w, http.StatusBadRequest, err)
}

const refreshedCredentialStateFailure = "refreshed credential state could not be safely collected"
const existingVaultCredentialStateGuidance = "A Replicaro vault exists, but Replicaro could not safely decrypt or validate it. Check the vault encryption password and the storage connection settings, then try again."

func explainExistingVaultCredentialStateFailure(err error) error {
	if err == nil || !strings.Contains(err.Error(), refreshedCredentialStateFailure) {
		return nil
	}
	return errors.New(existingVaultCredentialStateGuidance)
}

var readRecoveryProfile = func(ctx context.Context, repo models.Repository) (vaultprofile.ReadResult, error) {
	return (vaultprofile.Store{Repository: repo}).ForProfile(repo.ProfileUUID).
		ReadDetailed(ctx)
}

var recoveryNameReservationMu sync.Mutex

var readRecoveryProfileDetailed = func(ctx context.Context, repo models.Repository) (vaultprofile.ReadResult, error) {
	store := (vaultprofile.Store{Repository: repo})
	if repo.NativeRepositoryID == "" {
		// Existing-vault discovery cannot match a tuple it has not learned yet.
		// This boundary still authenticates and validates the bounded root; the
		// same flow must immediately reread it after installing the discovered tuple.
		return store.ReadDiscoveryRootDetailed(ctx)
	}
	return store.ReadDetailed(ctx)
}

var scanRecoveryProfiles = func(ctx context.Context, repo models.Repository, selectedProfileUUID string) ([]VaultProfileChoice, *vaultprofile.Profile, []byte, error) {
	choices := make([]VaultProfileChoice, 0)
	var selected *vaultprofile.Profile
	var selectedData []byte
	localClientUUID, err := database.InstallationID(previewDB(ctx))
	if err != nil {
		return nil, nil, nil, err
	}
	localCount := 0
	err = (vaultprofile.Store{Repository: repo}).
		ScanProfiles(ctx, maximumExistingVaultProfiles, func(profile vaultprofile.Profile, data []byte) error {
			choice := vaultProfileChoice(profile)
			choice.SHA256 = recoverySHA256(data)
			choices = append(choices, choice)
			if profile.Attachment.ClientUUID == localClientUUID {
				localCount++
				if localCount > 1 {
					return fmt.Errorf("multiple vault profiles are attached to this computer")
				}
				copy := profile
				selected, selectedData = &copy, append([]byte(nil), data...)
				return nil
			}
			if localCount > 0 {
				return nil
			}
			if profile.ProfileUUID == selectedProfileUUID || selectedProfileUUID == "" && len(choices) == 1 {
				copy := profile
				selected = &copy
				selectedData = append([]byte(nil), data...)
			} else if selectedProfileUUID == "" {
				selected = nil
				selectedData = nil
			}
			return nil
		})
	if err != nil {
		return nil, nil, nil, err
	}
	if selectedProfileUUID != "" && selected == nil {
		return nil, nil, nil, fmt.Errorf("the selected profile is no longer present in this vault")
	}
	return choices, selected, selectedData, nil
}

var scanConnectionProfileAttachments = func(ctx context.Context, repo models.Repository) ([]VaultProfileChoice, error) {
	choices := make([]VaultProfileChoice, 0)
	err := (vaultprofile.Store{Repository: repo}).
		ScanProfiles(ctx, maximumExistingVaultProfiles, func(profile vaultprofile.Profile, _ []byte) error {
			choices = append(choices, VaultProfileChoice{ProfileUUID: profile.ProfileUUID, Attachment: profile.Attachment})
			return nil
		})
	return choices, err
}

var publishRecoveryProfile = func(ctx context.Context, repo models.Repository, data []byte, options vaultprofile.PublishOptions) error {
	return (vaultprofile.Store{Repository: repo}).ForProfile(repo.ProfileUUID).
		PublishUnderLock(ctx, data, options)
}

var publishRecoveryRoot = func(ctx context.Context, repo models.Repository, data []byte, options vaultprofile.PublishOptions) error {
	return (vaultprofile.Store{Repository: repo}).
		PublishUnderLock(ctx, data, options)
}

var readConnectionOwnerRoot = func(ctx context.Context, repo models.Repository) ([]byte, error) {
	return (vaultprofile.Store{Repository: repo}).Read(ctx)
}

var readConnectionOwnerProfileDetailed = func(ctx context.Context, repo models.Repository) (vaultprofile.ReadResult, error) {
	return (vaultprofile.Store{Repository: repo}).ForProfile(repo.ProfileUUID).ReadDetailed(ctx)
}

var readConnectionOwnerRootDetailed = func(ctx context.Context, repo models.Repository) (vaultprofile.ReadResult, error) {
	return (vaultprofile.Store{Repository: repo}).ReadDetailed(ctx)
}

var reviewExistingVaultForConnection = previewExistingVault

var fingerprintExistingRepository = repositoryFingerprint

var detectExistingRepositorySignatures = detectRepositorySignatures

var ensureConnectionKopiaClientIdentity = engines.EnsureKopiaClientIdentity
var stageKopiaConnectionUpdate = repositoryadmission.StageKopiaConnectionUpdate
var cleanupKopiaConnectionUpdate = repositoryadmission.CleanupKopiaConnectionUpdate

const maximumExistingVaultProfiles = 256
const existingVaultProfileCapacityMessage = "Replicaro supports up to 256 computers backing up to the same vault. You have reached this limit, so a new profile cannot be created. Either take over an existing profile or backup to a different vault."

var ensureConnectionKopiaMaintenanceOwner = engines.EnsureKopiaMaintenanceOwner

func connectionMaintenanceMutationContext(
	ctx context.Context, db *sql.DB, repo models.Repository,
	intent database.RepositoryConnectionIntent, payload connectionIntentPayload,
	ownerTransferComplete bool,
) context.Context {
	return engines.ContextWithKopiaMaintenanceMutationAdmission(ctx, func(admissionContext context.Context) error {
		fresh, err := database.FindRepositoryConnectionIntentByID(db, intent.ID)
		if err != nil || fresh.ID != intent.ID || fresh.CanonicalIdentity != intent.CanonicalIdentity ||
			fresh.Connector != intent.Connector || fresh.Location != intent.Location ||
			fresh.ColdStorage != intent.ColdStorage || fresh.ArchiveWriteClass != intent.ArchiveWriteClass ||
			fresh.ReviewedOptionsJSON != intent.ReviewedOptionsJSON || fresh.Mode != intent.Mode ||
			fresh.PreviewDigest != intent.PreviewDigest || fresh.PayloadJSON != intent.PayloadJSON ||
			fresh.ProfileSHA256 != intent.ProfileSHA256 || fresh.NativeFingerprint != intent.NativeFingerprint ||
			fresh.PublicationOperationID != intent.PublicationOperationID || fresh.State != intent.State {
			return fmt.Errorf("reviewed vault connection changed before Kopia maintenance mutation")
		}
		profileRead, profileErr := readConnectionOwnerProfileDetailed(admissionContext, repo)
		profileData := profileRead.Data
		profileHash := sha256.Sum256(profileData)
		expectedHash := sha256.Sum256(payload.Profile)
		profile, parseErr := vaultprofile.Parse(profileData)
		if profileErr != nil || profileRead.Degraded || profileRead.Generation != "canonical" || parseErr != nil || profileHash != expectedHash ||
			hex.EncodeToString(expectedHash[:]) != fresh.ProfileSHA256 ||
			profile.VaultUUID != repo.ID || profile.ProfileUUID != repo.ProfileUUID ||
			profile.Attachment.ClientUUID != repo.ClientUUID ||
			profile.Attachment.Generation != repo.AttachmentGeneration {
			return fmt.Errorf("authoritative vault profile attachment changed before Kopia maintenance mutation")
		}
		if payload.OwnerTransferFromProfileUUID != "" {
			rootRead, transitionErr := readConnectionOwnerRootDetailed(admissionContext, repo)
			if transitionErr != nil || rootRead.Degraded || rootRead.Generation != "canonical" {
				return fmt.Errorf("reviewed vault owner transition changed before Kopia maintenance mutation")
			}
			expectedRoot := payload.OwnerTransitionRoot
			if ownerTransferComplete {
				expectedRoot = payload.Root
			}
			actualHash, expectedHash := sha256.Sum256(rootRead.Data), sha256.Sum256(expectedRoot)
			root, parseRootErr := vaultprofile.ParseRoot(rootRead.Data, repo.Connector)
			if actualHash != expectedHash || parseRootErr != nil || !root.Identifies(repo) {
				return fmt.Errorf("reviewed vault owner transition changed before Kopia maintenance mutation")
			}
			if ownerTransferComplete {
				if root.VaultOwner.ProfileUUID != repo.ProfileUUID || root.OwnerTransfer != nil {
					return fmt.Errorf("reviewed vault owner transition changed before Kopia maintenance mutation")
				}
			} else if root.VaultOwner.ProfileUUID != payload.OwnerTransferFromProfileUUID || root.OwnerTransfer == nil ||
				root.OwnerTransfer.OperationUUID != fresh.PublicationOperationID ||
				root.OwnerTransfer.FromProfileUUID != payload.OwnerTransferFromProfileUUID ||
				root.OwnerTransfer.ToProfileUUID != repo.ProfileUUID || root.OwnerTransfer.Phase != "reviewed" {
				return fmt.Errorf("reviewed vault owner transition changed before Kopia maintenance mutation")
			}
		} else if payload.TakeoverFromClientUUID != "" && repo.IsVaultOwner {
			rootRead, rootErr := readConnectionOwnerRootDetailed(admissionContext, repo)
			rootData := rootRead.Data
			rootHash := sha256.Sum256(rootData)
			payloadRootHash := sha256.Sum256(payload.Root)
			rootDigest := hex.EncodeToString(rootHash[:])
			root, parseRootErr := vaultprofile.ParseRoot(rootData, repo.Connector)
			if rootErr != nil || rootRead.Degraded || rootRead.Generation != "canonical" || parseRootErr != nil || !root.Identifies(repo) ||
				root.VaultOwner.ProfileUUID != repo.ProfileUUID || root.OwnerTransfer != nil ||
				(rootDigest != payload.ExpectedRootSHA256 && rootHash != payloadRootHash) {
				return fmt.Errorf("authoritative vault owner changed before Kopia maintenance mutation")
			}
		}
		return nil
	})
}

var forceConnectedVaultMetadataRefresh = metadata.ForceRepositoryRefresh

var prepareConnectedVaultStatisticsRefresh = vaultstatistics.Prepare

func recoveryVisibleSnapshots(mode, profileUUID string, knownJobs map[string]bool, snapshots []models.Snapshot) []models.Snapshot {
	visible := make([]models.Snapshot, 0, len(snapshots))
	for _, snapshot := range snapshots {
		if mode == "fallback" {
			// A valid marker belongs to an existing Replicaro profile. Fallback
			// creates a new profile and must not disclose or adopt that history.
			if snapshot.OwnershipMarker.Status == models.SnapshotOwnershipValid {
				continue
			}
			snapshot.Presentation = models.SnapshotPresentationUnmanaged
			snapshot.ManagedJobID = ""
		} else {
			snapshot = models.PresentSnapshot(snapshot, profileUUID, knownJobs)
			if snapshot.Presentation == models.SnapshotPresentationHidden {
				continue
			}
		}
		visible = append(visible, snapshot)
	}
	return visible
}

func initiateConnectedVaultRefreshes(db *sql.DB, repositoryID string) {
	repo, err := database.GetRepository(db, repositoryID)
	if err != nil {
		_ = database.LogWarning(db, "Connected vault refresh could not load vault "+repositoryID)
		return
	}
	if !forceConnectedVaultMetadataRefresh(db, repo) {
		_ = database.LogWarning(db, "Metadata refresh could not be initiated for connected vault "+repositoryID)
	}
	if _, err := prepareConnectedVaultStatisticsRefresh(db, repositoryID, true); err != nil {
		_ = database.LogWarning(db, "Vault Size refresh could not be initiated for connected vault "+repositoryID)
	}
}

var recoveryProfileExists = func(ctx context.Context, repo models.Repository) (bool, error) {
	return (vaultprofile.Store{Repository: repo}).
		Exists(ctx)
}

type ExistingVaultReviewBaseline struct {
	Root            vaultprofile.Root                 `json:"root"`
	RootSHA256      string                            `json:"rootSHA256"`
	StorageIdentity string                            `json:"storageIdentity"`
	EngineVersion   string                            `json:"engineVersion"`
	Profiles        []ExistingVaultProfileFingerprint `json:"profiles"`
	ChoicesSHA256   string                            `json:"choicesSHA256"`
	Degraded        bool                              `json:"degraded"`
}

type ExistingVaultProfileFingerprint struct {
	ProfileUUID string `json:"profile_uuid"`
	SHA256      string `json:"sha256"`
}

func recoverySHA256(data []byte) string { return fmt.Sprintf("%x", sha256.Sum256(data)) }

type ExistingVaultPreview struct {
	Baseline                      *ExistingVaultReviewBaseline          `json:"baseline,omitempty"`
	VaultUUID                     string                                `json:"vaultUUID"`
	Mode                          string                                `json:"mode"`
	Digest                        string                                `json:"digest"`
	Engine                        string                                `json:"engine"`
	EngineVersion                 string                                `json:"engineVersion"`
	Profile                       *vaultprofile.Profile                 `json:"profile,omitempty"`
	Profiles                      []VaultProfileChoice                  `json:"profiles,omitempty"`
	VaultOwnerProfileUUID         string                                `json:"vault_owner_profile_uuid,omitempty"`
	RootIntegritySchedule         string                                `json:"rootIntegritySchedule,omitempty"`
	RootMaintenanceSchedule       string                                `json:"rootMaintenanceSchedule,omitempty"`
	Snapshots                     []models.Snapshot                     `json:"snapshots"`
	ImportedSources               []string                              `json:"importedSources,omitempty"`
	LocalJobSourceKeys            map[string]string                     `json:"-"`
	NativeFingerprint             string                                `json:"-"`
	ProfileDegraded               bool                                  `json:"profileDegraded,omitempty"`
	LocalJobConflicts             map[string]string                     `json:"localJobConflicts,omitempty"`
	ReviewedLocalJobs             map[string]models.BackupJob           `json:"-"`
	ProfileSHA256                 string                                `json:"-"`
	RootSHA256                    string                                `json:"-"`
	RootCreatedAt                 time.Time                             `json:"-"`
	Root                          *vaultprofile.Root                    `json:"-"`
	ColdStorage                   bool                                  `json:"coldStorage"`
	ArchiveWriteClass             string                                `json:"archiveWriteClass,omitempty"`
	StorageClass                  string                                `json:"storageClass"`
	ObjectLock                    models.ObjectLockSettings             `json:"objectLock"`
	ObjectLockEnrollmentAvailable bool                                  `json:"objectLockEnrollmentAvailable"`
	ExistingVault                 *ExistingVaultUpdateReview            `json:"existingVault,omitempty"`
	ExistingRepository            *models.Repository                    `json:"-"`
	ExistingJobAdmissions         []database.RecoveredLocalJobAdmission `json:"-"`
}

// ExistingVaultUpdateReview contains only non-secret saved-row values needed
// to distinguish a confirmed update from a second registration and to show the
// exact local preferences that would change. Connector credentials deliberately
// remain absent even though the final transaction can replace them.
type ExistingVaultUpdateReview struct {
	ID                  string `json:"id"`
	Name                string `json:"name"`
	Location            string `json:"location"`
	CandidateLocation   string `json:"candidateLocation"`
	Description         string `json:"description"`
	CheckSchedule       string `json:"checkSchedule"`
	MaintenanceSchedule string `json:"maintenanceSchedule"`
	ConcurrencyMode     string `json:"concurrencyMode"`
	IsVaultOwner        bool   `json:"isVaultOwner"`
}

func existingVaultUpdateReview(repo models.Repository) ExistingVaultUpdateReview {
	return ExistingVaultUpdateReview{
		ID: repo.ID, Name: repo.Name, Location: repo.Location, Description: repo.Description,
		CheckSchedule: repo.CheckSchedule, MaintenanceSchedule: repo.MaintenanceSchedule,
		ConcurrencyMode: repo.ConcurrencyMode, IsVaultOwner: repo.IsVaultOwner,
	}
}

func publicExistingVaultPreview(preview ExistingVaultPreview) ExistingVaultPreview {
	if preview.Mode != "fallback" || len(preview.Snapshots) == 0 {
		return preview
	}
	// Fallback snapshots remain authoritative for visibility, source import,
	// digesting, and final revalidation. Only the JSON response copy drops the
	// two internal presentation namespaces so installation and machine identity
	// do not cross the intended cache/command boundary.
	preview.Snapshots = append([]models.Snapshot(nil), preview.Snapshots...)
	for index := range preview.Snapshots {
		snapshot := &preview.Snapshots[index]
		if len(snapshot.Tags) == 0 {
			continue
		}
		tags := make([]string, 0, len(snapshot.Tags))
		for _, tag := range snapshot.Tags {
			if tag == "replicaro-client" || strings.HasPrefix(tag, models.SnapshotClientMarkerPrefix) ||
				tag == "replicaro-machine" || strings.HasPrefix(tag, models.SnapshotMachineMarkerPrefix) {
				continue
			}
			tags = append(tags, tag)
		}
		snapshot.Tags = tags
	}
	return preview
}

type VaultProfileChoice struct {
	SHA256           string                        `json:"sha256"`
	ProfileUUID      string                        `json:"profile_uuid"`
	Attachment       vaultprofile.Attachment       `json:"attachment"`
	VaultPreferences vaultprofile.VaultPreferences `json:"vaultPreferences"`
	JobCount         int                           `json:"jobCount"`
	CreatedAt        time.Time                     `json:"createdAt"`
	VaultOwner       bool                          `json:"vaultOwner"`
	LocalAttachment  bool                          `json:"localAttachment"`
}

func vaultProfileChoice(profile vaultprofile.Profile) VaultProfileChoice {
	return VaultProfileChoice{
		ProfileUUID: profile.ProfileUUID, Attachment: profile.Attachment,
		VaultPreferences: profile.VaultPreferences, JobCount: len(profile.Jobs), CreatedAt: profile.CreatedAt,
	}
}

func reviewedLocalJobDefinition(job models.BackupJob) models.BackupJob {
	return models.BackupJob{
		ID: job.ID, Name: job.Name, Source: job.Source, SourceKind: job.SourceKind, Schedule: job.Schedule, Enabled: job.Enabled,
		Retention: job.Retention, RetentionHourly: job.RetentionHourly,
		RetentionDaily: job.RetentionDaily, RetentionWeekly: job.RetentionWeekly,
		RetentionMonthly: job.RetentionMonthly, RetentionYearly: job.RetentionYearly,
		Excludes: job.Excludes, Tag: job.Tag,
		SkipNoBackupFolders: job.SkipNoBackupFolders, SkipCacheDirFolders: job.SkipCacheDirFolders,
		SkipMarkerFileName: job.SkipMarkerFileName,
		BeforeScriptPath:   job.BeforeScriptPath, BeforeScriptMustSucceed: job.BeforeScriptMustSucceed,
		AfterScriptPath: job.AfterScriptPath, AfterScriptMustSucceed: job.AfterScriptMustSucceed,
		SourceStorageVersion:        job.SourceStorageVersion,
		SourceStorageKey:            job.SourceStorageKey,
		SourceStorageDescriptorJSON: job.SourceStorageDescriptorJSON,
		SourceBindingState:          job.SourceBindingState,
		EngineSettings:              job.EngineSettings,
	}
}

func reviewedLocalDefinitionMatchesRecovered(
	reviewed models.BackupJob,
	recovered models.BackupJob,
	newEngine string,
) bool {
	recoveredDefinition := reviewedLocalJobDefinition(recovered)
	reviewedSettings := reviewed.EngineSettings
	recoveredSettings := recoveredDefinition.EngineSettings
	reviewed.EngineSettings = nil
	recoveredDefinition.EngineSettings = nil
	reviewed.Enabled = false
	recoveredDefinition.Enabled = false
	// Source-storage bindings and runtime aliases belong to this local
	// attachment and are intentionally absent from the portable vault profile.
	// They must not turn an otherwise exact portable definition into a
	// collision, and the separate local admission still protects them.
	reviewed.SourceStorageVersion, recoveredDefinition.SourceStorageVersion = "", ""
	reviewed.SourceStorageKey, recoveredDefinition.SourceStorageKey = "", ""
	reviewed.SourceStorageDescriptorJSON, recoveredDefinition.SourceStorageDescriptorJSON = "", ""
	reviewed.SourceBindingState, recoveredDefinition.SourceBindingState = "", ""
	if !reflect.DeepEqual(reviewed, recoveredDefinition) {
		return false
	}
	for engineID, settings := range reviewedSettings {
		if recoveredSettings == nil || !reflect.DeepEqual(recoveredSettings[engineID], settings) {
			return false
		}
	}
	for engineID := range recoveredSettings {
		if _, reviewedAlready := reviewedSettings[engineID]; !reviewedAlready && engineID != newEngine {
			return false
		}
	}
	return true
}

func recoveredLocalJobAdmissions(
	db *sql.DB,
	preview ExistingVaultPreview,
	jobs []models.BackupJob,
	newEngine string,
) ([]database.RecoveredLocalJobAdmission, error) {
	admissions := make([]database.RecoveredLocalJobAdmission, 0)
	seen := make(map[string]bool, len(jobs))
	for _, job := range jobs {
		if seen[job.ID] {
			return nil, fmt.Errorf("recovered local job admission set contains a duplicate job ID")
		}
		seen[job.ID] = true
		reviewed, wasReviewed := preview.ReviewedLocalJobs[job.ID]
		local, err := database.GetJob(db, job.ID)
		switch {
		case err == nil:
			if !wasReviewed {
				return nil, fmt.Errorf("a local job appeared after the recovery preview; check the vault again")
			}
			currentDefinition := reviewedLocalJobDefinition(local)
			if !reflect.DeepEqual(currentDefinition, reviewed) ||
				!reviewedLocalDefinitionMatchesRecovered(reviewed, job, newEngine) {
				return nil, fmt.Errorf("the reviewed local job changed before connection admission")
			}
			admission := database.BuildRecoveredLocalJobAdmission(local)
			admission.Reviewed = reviewed
			admissions = append(admissions, admission)
		case errors.Is(err, sql.ErrNoRows):
			if wasReviewed {
				return nil, fmt.Errorf("the reviewed local job is unavailable")
			}
		default:
			return nil, fmt.Errorf("reload recovered local job admission: %w", err)
		}
	}
	sort.Slice(admissions, func(i, j int) bool {
		return admissions[i].Reviewed.ID < admissions[j].Reviewed.ID
	})
	return admissions, nil
}

func recoveryPreviewDigest(mode, engine, version, storageIdentity, nativeIdentity string, coldStorage bool, archiveWriteClass string, profile []byte, snapshots []models.Snapshot, localJobs []models.BackupJob) (string, error) {
	// Snapshot inventories change normally and are rebuildable native evidence.
	// Unmanaged import binds only the distinct normalized source set reviewed by
	// the user; managed profile review does not journal snapshot inventory.
	reviewedSources := []string{}
	if mode == "fallback" {
		sources, err := unmanagedImportSources(snapshots)
		if err != nil {
			return "", err
		}
		for _, source := range sources {
			reviewedSources = append(reviewedSources, recoverySourceKey(source))
		}
		sort.Strings(reviewedSources)
	}
	fingerprintLocalJobs := append([]models.BackupJob(nil), localJobs...)
	sort.Slice(fingerprintLocalJobs, func(i, j int) bool { return fingerprintLocalJobs[i].ID < fingerprintLocalJobs[j].ID })
	fingerprint, err := json.Marshal(struct {
		Mode, Engine, Version, StorageIdentity, NativeIdentity string
		ColdStorage                                            bool
		ArchiveWriteClass                                      string
		Profile                                                []byte
		ReviewedSources                                        []string
		LocalJobs                                              []models.BackupJob
	}{mode, engine, version, storageIdentity, nativeIdentity, coldStorage, archiveWriteClass, profile, reviewedSources, fingerprintLocalJobs})
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(fingerprint)
	return hex.EncodeToString(digest[:]), nil
}

type ConnectExistingVaultRequest struct {
	ExistingVaultStorage
	ReviewedVaultUUID         string                           `json:"reviewedVaultUUID"`
	IntentID                  string                           `json:"intentId"`
	Mode                      string                           `json:"mode"`
	Digest                    string                           `json:"digest"`
	Name                      string                           `json:"name"`
	Description               string                           `json:"description"`
	CheckSchedule             string                           `json:"checkSchedule"`
	MaintenanceSchedule       string                           `json:"maintenanceSchedule"`
	ConcurrencyMode           string                           `json:"concurrencyMode"`
	ObjectLock                models.ObjectLockSettings        `json:"objectLock"`
	ProfileAction             string                           `json:"profileAction,omitempty"`
	OwnerAction               string                           `json:"ownerAction,omitempty"`
	UpdateExistingVault       bool                             `json:"updateExistingVault,omitempty"`
	UpdateExistingVaultReview *ExistingVaultUpdateConfirmation `json:"updateExistingVaultReview,omitempty"`
}

// ExistingVaultUpdateConfirmation is the non-secret state the user reviewed.
// Presence is the explicit confirmation, while exact comparison beneath the
// vault UUID lock prevents a stale checkbox or a newly constructed retry from
// authorizing different location or preference changes.
type ExistingVaultUpdateConfirmation struct {
	VaultUUID           string `json:"vaultUUID"`
	PreviewDigest       string `json:"previewDigest"`
	PreviousLocation    string `json:"previousLocation"`
	Location            string `json:"location"`
	Name                string `json:"name"`
	Description         string `json:"description"`
	CheckSchedule       string `json:"checkSchedule"`
	MaintenanceSchedule string `json:"maintenanceSchedule"`
	ConcurrencyMode     string `json:"concurrencyMode"`
}

func expectedExistingVaultUpdateConfirmation(preview ExistingVaultPreview, candidateLocation string, req ConnectExistingVaultRequest) (ExistingVaultUpdateConfirmation, error) {
	if preview.ExistingRepository == nil {
		return ExistingVaultUpdateConfirmation{}, fmt.Errorf("the reviewed vault is not an existing registration")
	}
	concurrencyMode, err := models.NormalizeConcurrencyModeForConnector(preview.ExistingRepository.Connector, req.ConcurrencyMode)
	if err != nil {
		return ExistingVaultUpdateConfirmation{}, err
	}
	return ExistingVaultUpdateConfirmation{
		VaultUUID: preview.ExistingRepository.ID, PreviewDigest: preview.Digest,
		PreviousLocation: preview.ExistingRepository.Location, Location: candidateLocation,
		Name: strings.TrimSpace(req.Name), Description: strings.TrimSpace(req.Description),
		CheckSchedule: req.CheckSchedule, MaintenanceSchedule: req.MaintenanceSchedule,
		ConcurrencyMode: concurrencyMode,
	}, nil
}

func unmanagedImportSources(snapshots []models.Snapshot) ([]string, error) {
	// Do not cap snapshot count, native root count, or discovered source path
	// count. Keep the byte, deadline, schema/path validity, deduplication, and
	// protected-profile-size safeguards.
	seen := map[string]bool{}
	result := []string{}
	for _, snapshot := range snapshots {
		roots := snapshot.SourceRoots
		if len(roots) == 0 && snapshot.Source != "" {
			roots = []models.SnapshotSourceRoot{{Path: snapshot.Source}}
		}
		for _, root := range roots {
			if root.Path == "" {
				return nil, fmt.Errorf("native snapshot discovery returned an empty source path")
			}
			if len(root.Path) > vaultprofile.MaximumProfileDecryptedSize {
				return nil, fmt.Errorf("native snapshot source path exceeds the protected profile limit")
			}
			key := recoverySourceKey(root.Path)
			if !seen[key] {
				seen[key] = true
				result = append(result, root.Path)
			}
		}
	}
	sort.SliceStable(result, func(i, j int) bool {
		left, right := recoverySourceKey(result[i]), recoverySourceKey(result[j])
		if left == right {
			return result[i] < result[j]
		}
		return left < right
	})
	return result, nil
}

func requireRecoveredJobSourcesAvailable(ctx context.Context, jobs []models.BackupJob) error {
	for _, job := range jobs {
		if job.SourceBindingState == "unbound_imported" {
			continue
		}
		if err := requireJobSourceStorageAvailable(ctx, job); err != nil {
			return err
		}
	}
	return nil
}

// importedSourceJobs adopts one job per native source root found in a vault
// that has no Replicaro profile. A native root can be a folder or a single
// file, and nothing on this computer has looked at it yet, so the kind is
// recorded as unknown; first binding probes the path and records what it
// finds, once. Do not default it to "" (folder): a root that is a file would
// then be refused as a missing source when the job is enabled, and the profile
// would publish it to other computers as a folder job.
func importedSourceJobs(repo models.Repository, sources []string) []models.BackupJob {
	jobs := make([]models.BackupJob, 0, len(sources))
	for index, source := range sources {
		jobs = append(jobs, models.BackupJob{
			ID: uuid.NewString(), Name: fmt.Sprintf("Imported source %d", index+1), Source: source,
			SourceKind:         models.SourceKindUnknown,
			SourceBindingState: "unbound_imported", Schedule: "manual", Enabled: false, Retention: 100,
			EngineSettings: models.EngineSettings{repo.Engine: {}}, PortableTargetIDs: []string{repo.ID},
			Targets: []models.BackupJobTarget{{RepositoryID: repo.ID, Engine: repo.Engine}},
		})
	}
	return jobs
}

type connectionProfileTransition struct {
	Profile            *vaultprofile.Profile
	Publish            bool
	CreateOnly         bool
	ExpectedSHA256     string
	TakeoverFromClient string
}

func deterministicImportVaultUUID(engine, nativeRepositoryID string) string {
	return uuid.NewSHA1(uuid.NameSpaceOID,
		[]byte("replicaro-vault\x00"+strings.TrimSpace(engine)+"\x00"+strings.TrimSpace(nativeRepositoryID))).String()
}

func finalNewJoinAtCapacity(preview ExistingVaultPreview, repo models.Repository, req ConnectExistingVaultRequest, clientUUID string) bool {
	if preview.Mode != "profile" || req.Mode != preview.Mode || strings.TrimSpace(req.Digest) == "" ||
		preview.Root == nil || models.SoleProfileConnector(repo.Connector) ||
		strings.TrimSpace(req.ProfileAction) != "join" || strings.TrimSpace(req.ProfileUUID) != "" ||
		len(preview.Profiles) != maximumExistingVaultProfiles {
		return false
	}
	for _, choice := range preview.Profiles {
		if choice.Attachment.ClientUUID == clientUUID {
			return false
		}
	}
	return true
}

func prepareConnectionProfile(preview ExistingVaultPreview, repo models.Repository, req ConnectExistingVaultRequest,
	clientUUID string, now time.Time) (connectionProfileTransition, error) {
	concurrencyMode, err := models.NormalizeConcurrencyModeForConnector(repo.Connector, repo.ConcurrencyMode)
	if err != nil {
		return connectionProfileTransition{}, err
	}
	repo.ConcurrencyMode = concurrencyMode
	submittedAction := strings.TrimSpace(req.ProfileAction)
	requestedProfileUUID := strings.TrimSpace(req.ProfileUUID)
	if preview.Mode == "fallback" {
		if requestedProfileUUID != "" || submittedAction != "" && submittedAction != "join" ||
			strings.TrimSpace(req.OwnerAction) != "" && strings.TrimSpace(req.OwnerAction) != "keep" {
			return connectionProfileTransition{}, fmt.Errorf("native vault import cannot select, reconnect, or take over an existing profile")
		}
		profileUUID := uuid.NewString()
		displayName, _ := os.Hostname()
		profile, err := vaultprofile.BuildProfile(repo, nil, nil, profileUUID, clientUUID, 1, 1,
			vaultprofile.AttachmentDisplay{ComputerName: displayName, OperatingSystem: runtime.GOOS}, time.Time{}, now)
		return connectionProfileTransition{Profile: &profile, Publish: true, CreateOnly: true}, err
	}
	if preview.Root == nil {
		return connectionProfileTransition{}, fmt.Errorf("the reviewed vault root is unavailable")
	}
	localProfiles := make([]VaultProfileChoice, 0, 1)
	for _, choice := range preview.Profiles {
		if choice.Attachment.ClientUUID == clientUUID {
			localProfiles = append(localProfiles, choice)
		}
	}
	// client_uuid is the installation identity within one physical vault. This
	// final admission preview contains the complete bounded profile set;
	// checking only the selected profile permits a Join request to create a
	// second local attachment. Keep this check at final admission rather than
	// relying on the UI or an earlier preview.
	//
	// Re-reading fresh state narrows, but cannot close, small races with another
	// computer using the vault. That risk is accepted: there is deliberately no
	// server/client coordinator, remote lease, heartbeat, or CAS protocol.
	if len(localProfiles) > 1 {
		return connectionProfileTransition{}, fmt.Errorf("multiple vault profiles are attached to this computer; connection is blocked until the identity conflict is resolved")
	}
	if submittedAction == "join" && requestedProfileUUID != "" {
		return connectionProfileTransition{}, fmt.Errorf("joining with a new profile cannot select an existing profile")
	}
	if (submittedAction == "reconnect" || submittedAction == "takeover") && requestedProfileUUID == "" {
		return connectionProfileTransition{}, fmt.Errorf("reconnect and takeover require the exact selected profile")
	}
	action := submittedAction
	if len(localProfiles) == 1 {
		localProfileUUID := localProfiles[0].ProfileUUID
		if preview.Profile == nil || preview.Profile.ProfileUUID != localProfileUUID {
			return connectionProfileTransition{}, fmt.Errorf("this computer is already attached to profile %s; reconnect that exact profile", localProfileUUID)
		}
		if action != "" && action != "reconnect" {
			return connectionProfileTransition{}, fmt.Errorf("this computer is already attached to the selected profile; reconnect it instead of joining or taking over another profile")
		}
		action = "reconnect"
	}
	if models.SoleProfileConnector(repo.Connector) {
		if len(preview.Profiles) != 1 || preview.Profile == nil {
			return connectionProfileTransition{}, fmt.Errorf("this cloud vault must contain exactly one profile")
		}
		if requestedProfileUUID == "" {
			requestedProfileUUID = preview.Profile.ProfileUUID
		}
		derivedAction := "takeover"
		if preview.Profile.Attachment.ClientUUID == clientUUID {
			derivedAction = "reconnect"
		}
		if submittedAction != "" && submittedAction != derivedAction {
			return connectionProfileTransition{}, fmt.Errorf("this cloud vault requires automatic %s of its sole profile", derivedAction)
		}
		action = derivedAction
	}
	if action == "join" && requestedProfileUUID != "" {
		return connectionProfileTransition{}, fmt.Errorf("joining with a new profile cannot select an existing profile")
	}
	if action != "join" && requestedProfileUUID == "" {
		return connectionProfileTransition{}, fmt.Errorf("reconnect and takeover require the exact selected profile")
	}
	displayName, _ := os.Hostname()
	display := vaultprofile.AttachmentDisplay{ComputerName: displayName, OperatingSystem: runtime.GOOS}
	switch action {
	case "join":
		if models.SoleProfileConnector(repo.Connector) {
			return connectionProfileTransition{}, fmt.Errorf("this cloud vault uses automatic sole-profile takeover")
		}
		// This is an observational admission check on the complete fresh scan,
		// not a reservation or remote computer-count coordinator. Concurrent
		// installations may both observe the final available slot.
		if len(preview.Profiles) >= maximumExistingVaultProfiles {
			return connectionProfileTransition{}, fmt.Errorf("%s", existingVaultProfileCapacityMessage)
		}
		profileUUID := uuid.NewString()
		profile, err := vaultprofile.BuildProfile(repo, nil, nil, profileUUID, clientUUID, 1, 1, display, time.Time{}, now)
		return connectionProfileTransition{Profile: &profile, Publish: true, CreateOnly: true}, err
	case "reconnect":
		if preview.Profile == nil || preview.Profile.Attachment.ClientUUID != clientUUID {
			return connectionProfileTransition{}, fmt.Errorf("reconnect requires this computer's selected profile")
		}
		if preview.Profile.VaultPreferences.ConcurrencyMode == repo.ConcurrencyMode {
			return connectionProfileTransition{Profile: preview.Profile}, nil
		}
		profile := *preview.Profile
		profile.Revision++
		profile.VaultPreferences.ConcurrencyMode = repo.ConcurrencyMode
		profile.UpdatedAt = now.UTC()
		if err := profile.Validate(); err != nil {
			return connectionProfileTransition{}, err
		}
		return connectionProfileTransition{Profile: &profile, Publish: true,
			ExpectedSHA256: preview.ProfileSHA256}, nil
	case "takeover":
		if preview.Profile == nil || preview.Profile.Attachment.ClientUUID == clientUUID {
			return connectionProfileTransition{}, fmt.Errorf("profile takeover requires another computer's selected profile")
		}
		profile := *preview.Profile
		priorClient := profile.Attachment.ClientUUID
		profile.Revision++
		profile.Attachment.ClientUUID = clientUUID
		profile.Attachment.Generation++
		profile.Attachment.AttachedAt = now.UTC()
		profile.Attachment.Display = display
		profile.VaultPreferences.ConcurrencyMode = repo.ConcurrencyMode
		profile.UpdatedAt = now.UTC()
		if err := profile.Validate(); err != nil {
			return connectionProfileTransition{}, err
		}
		return connectionProfileTransition{Profile: &profile, Publish: true,
			ExpectedSHA256: preview.ProfileSHA256, TakeoverFromClient: priorClient}, nil
	default:
		return connectionProfileTransition{}, fmt.Errorf("choose Join this vault, Reconnect this profile, or Force profile takeover")
	}
}

type RetryExistingVaultRequest struct {
	IntentID            string            `json:"intentId"`
	Password            string            `json:"password"`
	Options             map[string]string `json:"options"`
	RcloneAuthSessionID string            `json:"rcloneAuthSessionId,omitempty"`
}

func reusablePersistentRcloneCreation(
	ctx context.Context,
	db *sql.DB,
	intentID, engine, connector, location string,
	_ map[string]string,
) (database.RepositoryCreationIntent, string, error) {
	intent, err := database.FindRepositoryCreationIntentByID(db, strings.TrimSpace(intentID))
	if err != nil {
		return database.RepositoryCreationIntent{}, "", err
	}
	if intent.Engine != engine || intent.Connector != connector || intent.Location != location ||
		intent.Phase == database.RepositoryCreationPrepared {
		return database.RepositoryCreationIntent{}, "",
			fmt.Errorf("pending rclone vault creation is not retryable")
	}
	repo := models.Repository{
		ID: intent.ID, Engine: intent.Engine, Connector: intent.Connector,
		Location: intent.Location, ConnectorOptions: intent.ReviewedOptions,
	}
	if err := engines.ValidateRcloneVaultConfig(ctx, repo); err != nil {
		return database.RepositoryCreationIntent{}, "", err
	}
	path, err := engines.RcloneVaultConfigPath(intent.ID)
	if err != nil {
		return database.RepositoryCreationIntent{}, "", err
	}
	return intent, path, nil
}

func reusablePersistentRcloneConnection(
	ctx context.Context,
	db *sql.DB,
	intentID, connector, location string,
) (database.RepositoryConnectionIntent, connectionIntentPayload, string, error) {
	intent, err := database.FindRepositoryConnectionIntentByID(
		db, strings.TrimSpace(intentID),
	)
	if err != nil {
		return database.RepositoryConnectionIntent{}, connectionIntentPayload{}, "", err
	}
	var payload connectionIntentPayload
	if err := decodeConnectionIntentPayload(intent.PayloadJSON, &payload); err != nil {
		return database.RepositoryConnectionIntent{}, connectionIntentPayload{}, "", err
	}
	if intent.Connector != connector || intent.Location != location {
		return database.RepositoryConnectionIntent{}, connectionIntentPayload{}, "",
			fmt.Errorf("pending connection does not match this vault")
	}
	repo := models.Repository{
		ID: payload.Repository.ID, Engine: payload.Repository.Engine,
		Connector: intent.Connector, Location: intent.Location,
		ConnectorOptions: intent.ReviewedOptions,
	}
	if err := engines.ValidateRcloneVaultConfig(ctx, repo); err != nil {
		return database.RepositoryConnectionIntent{}, connectionIntentPayload{}, "", err
	}
	path, err := engines.RcloneVaultConfigPath(payload.Repository.ID)
	if err != nil {
		return database.RepositoryConnectionIntent{}, connectionIntentPayload{}, "", err
	}
	return intent, payload, path, nil
}

func applyReviewedConnectionVaultFields(repo *models.Repository, req ConnectExistingVaultRequest) {
	repo.Name = strings.TrimSpace(req.Name)
	repo.Description = req.Description
	repo.CheckSchedule = req.CheckSchedule
	repo.MaintenanceSchedule = req.MaintenanceSchedule
	repo.ConcurrencyMode = req.ConcurrencyMode
	repo.ColdStorage = req.ColdStorage
	repo.ArchiveWriteClass = req.ArchiveWriteClass
	if repo.ColdStorage {
		repo.CheckSchedule = "manual"
	}
}

type connectionIntentRepository struct {
	ID, Name, Engine, Description      string
	CheckSchedule, MaintenanceSchedule string
	ConcurrencyMode                    string
	ObjectLock                         models.ObjectLockSettings
	AutoUnlock                         bool
	ColdStorage                        bool
	ArchiveWriteClass                  string
	StorageIdentityVersion             string
	StorageIdentityKey                 string
	StorageIdentityJSON                string
	ProfileUUID                        string
	AttachmentGeneration               int64
	NativeRepositoryID                 string
	ClientUUID                         string
	IsVaultOwner                       bool
	CreatedAt                          time.Time
}

type connectionIntentExistingRepository struct {
	ID                   string
	ProfileUUID          string
	AttachmentGeneration int64
}

type connectionIntentPayload struct {
	Repository                   connectionIntentRepository
	PhysicalVaultIdentity        string
	StorageIdentityVersion       string
	StorageIdentityKey           string
	StorageIdentityJSON          string
	Jobs                         []models.BackupJob
	ReviewedSourceRoots          []string
	LocalJobAdmissions           []database.RecoveredLocalJobAdmission
	JobStorageBindings           map[string]connectionSourceStorageBinding
	Dormant                      []database.DormantRecoveryJob
	Profile                      []byte
	Root                         []byte
	RequestSelectionDigest       string
	ExpectedProfileSHA256        string
	ExpectedRootSHA256           string
	ExpectedFinalRootSHA256      string
	PublishProfile               bool
	CreateProfile                bool
	PublishRoot                  bool
	CreateRoot                   bool
	OwnerTransferFromProfileUUID string
	OwnerTransitionRoot          []byte
	TakeoverFromClientUUID       string
	NativeOwnerClientUUID        string
	UpdateExistingVault          bool
	ExistingRepository           connectionIntentExistingRepository

	// Decoded and ignored. Pending connections saved by earlier releases
	// carry this key, usually empty, and the strict decoder below would
	// otherwise refuse them. Nothing reads it: a retry identifies the vault
	// from Repository.ID and its protected root.
	IgnoredExpectedVaultUUID string `json:"ExpectedVaultUUID,omitempty"`
}

func decodeConnectionIntentPayload(data string, payload *connectionIntentPayload) error {
	// Stored connection state decides what a retry does next. Reject unknown
	// fields so state from a workflow branch this code does not know about
	// cannot trigger a publication.
	decoder := json.NewDecoder(strings.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(payload); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("pending connection payload contains trailing data")
	}
	return nil
}

func connectionIntentVaultUUID(intent database.RepositoryConnectionIntent) (string, error) {
	var payload connectionIntentPayload
	if err := decodeConnectionIntentPayload(intent.PayloadJSON, &payload); err != nil {
		return "", fmt.Errorf("pending connection state is invalid")
	}
	parsed, err := uuid.Parse(payload.Repository.ID)
	if err != nil || parsed.String() != payload.Repository.ID {
		return "", fmt.Errorf("pending connection has no managed vault UUID")
	}
	return payload.Repository.ID, nil
}

type connectionSourceStorageBinding struct {
	Version        string
	Key            string
	DescriptorJSON string
}

func validatePendingConnectionProfileAttachments(
	choices []VaultProfileChoice,
	repo models.Repository,
	payload connectionIntentPayload,
) error {
	if payload.CreateRoot {
		if len(choices) == 0 {
			return nil
		}
		if len(choices) == 1 && choices[0].ProfileUUID == repo.ProfileUUID &&
			choices[0].Attachment.ClientUUID == repo.ClientUUID {
			return nil
		}
		return fmt.Errorf("the pending native import found unexpected vault profiles")
	}
	automaticSoleProfile := models.SoleProfileConnector(repo.Connector)
	if automaticSoleProfile && payload.CreateProfile && len(choices) == 0 {
		return nil
	}
	if automaticSoleProfile && len(choices) != 1 {
		return fmt.Errorf("this cloud vault must contain exactly one profile")
	}
	localProfiles := make([]VaultProfileChoice, 0, 1)
	var selected *VaultProfileChoice
	for index := range choices {
		choice := choices[index]
		if choice.Attachment.ClientUUID == repo.ClientUUID {
			localProfiles = append(localProfiles, choice)
		}
		if choice.ProfileUUID == repo.ProfileUUID {
			selected = &choices[index]
		}
	}
	if len(localProfiles) > 1 {
		return fmt.Errorf("multiple vault profiles are attached to this computer; pending connection is blocked")
	}
	if len(localProfiles) == 1 && localProfiles[0].ProfileUUID != repo.ProfileUUID {
		return fmt.Errorf("this computer is already attached to profile %s; pending connection cannot attach profile %s", localProfiles[0].ProfileUUID, repo.ProfileUUID)
	}
	if selected == nil {
		if payload.CreateProfile && !automaticSoleProfile {
			if len(choices) >= maximumExistingVaultProfiles {
				return fmt.Errorf("%s", existingVaultProfileCapacityMessage)
			}
			return nil
		}
		return fmt.Errorf("the pending reconnect or takeover profile is no longer present")
	}
	if selected.Attachment.ClientUUID == repo.ClientUUID {
		return nil
	}
	if payload.TakeoverFromClientUUID != "" && selected.Attachment.ClientUUID == payload.TakeoverFromClientUUID {
		return nil
	}
	return fmt.Errorf("the pending connection's selected profile attachment changed")
}

func ensureConnectionProfilePublished(ctx context.Context, db *sql.DB, repo models.Repository, intent *database.RepositoryConnectionIntent, payload *connectionIntentPayload) error {
	expectedHash := sha256.Sum256(payload.Profile)
	readback, readErr := readRecoveryProfile(ctx, repo)
	if readErr == nil {
		actualHash := sha256.Sum256(readback.Data)
		if !readback.Degraded && readback.Generation == "canonical" && actualHash == expectedHash {
			return nil
		}
		if !payload.PublishProfile {
			return fmt.Errorf("the authoritative vault profile changed after review")
		}
	} else if !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read authoritative vault profile before publication: %w", readErr)
	}
	if !payload.PublishProfile {
		return fmt.Errorf("read authoritative vault profile: %w", readErr)
	}
	if intent.State == "prepared" {
		if err := database.MarkRepositoryConnectionIntent(db, intent.ID, "publication_started", "Recovery profile publication is in progress; retry this connection."); err != nil {
			return fmt.Errorf("persist profile publication admission: %w", err)
		}
		intent.State = "publication_started"
	}
	if err := publishRecoveryProfile(ctx, repo, payload.Profile, vaultprofile.PublishOptions{
		OperationID: intent.PublicationOperationID, CreateOnly: payload.CreateProfile,
		ExpectedCurrentSHA256: payload.ExpectedProfileSHA256,
	}); err != nil {
		return fmt.Errorf("publish authoritative vault profile attachment: %w", err)
	}
	readback, err := readRecoveryProfile(ctx, repo)
	if err != nil {
		return fmt.Errorf("read back authoritative vault profile attachment: %w", err)
	}
	return verifyCanonicalConnectionProfile(readback, repo, expectedHash)
}

func verifyCanonicalConnectionProfile(readback vaultprofile.ReadResult, repo models.Repository, expectedHash [sha256.Size]byte) error {
	// Previous generations can recover a profile for review, but only the exact
	// canonical generation can authorize a local attachment. Recheck this after
	// engine preparation as well as publication so takeover cannot race attach.
	if readback.Degraded || readback.Generation != "canonical" {
		return fmt.Errorf("the authoritative vault profile is not available as the canonical generation")
	}
	actualHash := sha256.Sum256(readback.Data)
	if actualHash != expectedHash {
		return fmt.Errorf("authoritative vault profile attachment readback differs")
	}
	profile, err := vaultprofile.Parse(readback.Data)
	if err != nil || profile.VaultUUID != repo.ID || profile.ProfileUUID != repo.ProfileUUID ||
		profile.Attachment.ClientUUID != repo.ClientUUID || profile.Attachment.Generation != repo.AttachmentGeneration {
		return fmt.Errorf("authoritative vault profile attachment identity changed")
	}
	return nil
}

func verifyConnectionProfileBeforeAttachment(ctx context.Context, repo models.Repository, expectedHash [sha256.Size]byte) error {
	readback, err := readRecoveryProfile(ctx, repo)
	if err != nil {
		return fmt.Errorf("read authoritative recovery profile before attachment: %w", err)
	}
	return verifyCanonicalConnectionProfile(readback, repo, expectedHash)
}

func verifyPendingConnectionRoot(ctx context.Context, repo models.Repository, payload connectionIntentPayload, finalRootExpected bool) error {
	if !engines.IsRcloneNativeLoginProvider(repo.Connector) {
		// This retry check is for the sign-in providers' connections, whose
		// private config may already be published when the retry runs. Other
		// connectors, Any Rclone Remote included, keep their existing retry
		// checks.
		return nil
	}
	rootMayBePublished := payload.PublishRoot || payload.OwnerTransferFromProfileUUID != ""
	// A pending retry is a fresh attachment admission. The earlier Check and
	// publication attempt cannot authorize attachment if the protected root was
	// removed, replaced, or password-fenced while the intent remained pending.
	readback, err := readConnectionOwnerRootDetailed(ctx, repo)
	if err != nil {
		if rootMayBePublished && !finalRootExpected && payload.CreateRoot && errors.Is(err, os.ErrNotExist) {
			// A first import may create its root later in this same retry. Any
			// existing root still has to match one exact admitted phase below.
			return nil
		}
		return fmt.Errorf("read authoritative vault root for pending connection: %w", err)
	}
	// An unregistered import may have reviewed a verified previous root when the
	// canonical root was unavailable but recoverable. That reviewed previous root
	// can still allow the work before publication, even if a changed care
	// schedule will be published; transition and final roots must be canonical.
	allowReviewedPrevious := !payload.UpdateExistingVault &&
		(!rootMayBePublished || !finalRootExpected)
	if readback.Generation != "canonical" &&
		!(allowReviewedPrevious && readback.Generation == "previous" && readback.Degraded) {
		return fmt.Errorf("the authoritative vault root is not available as an admitted generation")
	}
	if readback.Generation == "canonical" && readback.Degraded {
		return fmt.Errorf("the authoritative vault root is degraded")
	}
	root, err := vaultprofile.ParseRoot(readback.Data, repo.Connector)
	if err != nil || !root.Identifies(repo) {
		return fmt.Errorf("the authoritative vault root identity changed after review")
	}
	actual := sha256.Sum256(readback.Data)
	actualDigest := hex.EncodeToString(actual[:])
	finalDigest := ""
	if len(payload.Root) > 0 {
		expected := sha256.Sum256(payload.Root)
		finalDigest = hex.EncodeToString(expected[:])
	}
	if rootMayBePublished {
		if finalRootExpected {
			if finalDigest == "" || actualDigest != finalDigest {
				return fmt.Errorf("the published vault root changed before attachment")
			}
			return requireStableConnectionPreviewRoot(root)
		}
		if readback.Generation == "previous" {
			if payload.ExpectedRootSHA256 == "" || actualDigest != payload.ExpectedRootSHA256 {
				return fmt.Errorf("the reviewed vault root fallback changed before publication")
			}
			return requireStableConnectionPreviewRoot(root)
		}
		// Before publication, admit only the reviewed root, this intent's
		// exact owner-transition root, or its already-published final root.
		// Check now, before a pending profile can be republished.
		if len(payload.OwnerTransitionRoot) > 0 {
			transitionHash := sha256.Sum256(payload.OwnerTransitionRoot)
			if actualDigest == hex.EncodeToString(transitionHash[:]) {
				if root.PasswordChange != nil || root.OwnerTransfer == nil {
					return fmt.Errorf("the pending vault owner transition root is invalid")
				}
				return nil
			}
		}
		if (payload.ExpectedRootSHA256 != "" && actualDigest == payload.ExpectedRootSHA256) ||
			(finalDigest != "" && actualDigest == finalDigest) {
			return requireStableConnectionPreviewRoot(root)
		}
		return fmt.Errorf("the authoritative vault root changed after review")
	}
	if err := requireStableConnectionPreviewRoot(root); err != nil {
		return err
	}
	if payload.ExpectedRootSHA256 == "" || actualDigest != payload.ExpectedRootSHA256 {
		return fmt.Errorf("the authoritative vault root changed after review")
	}
	return nil
}

func connectionOwnerTransitionState(
	ctx context.Context,
	repo models.Repository,
	intent database.RepositoryConnectionIntent,
	payload connectionIntentPayload,
) (bool, error) {
	if payload.OwnerTransferFromProfileUUID == "" {
		return false, nil
	}
	if len(payload.OwnerTransitionRoot) == 0 || payload.ExpectedFinalRootSHA256 == "" ||
		payload.OwnerTransferFromProfileUUID == repo.ProfileUUID {
		return false, fmt.Errorf("pending vault owner transition is incomplete")
	}
	current, err := readConnectionOwnerRoot(ctx, repo)
	if err != nil {
		return false, err
	}
	currentHash := sha256.Sum256(current)
	currentDigest := hex.EncodeToString(currentHash[:])
	finalHash := sha256.Sum256(payload.Root)
	if currentDigest == hex.EncodeToString(finalHash[:]) {
		root, parseErr := vaultprofile.ParseRoot(current, repo.Connector)
		if parseErr != nil || !root.Identifies(repo) ||
			root.VaultOwner.ProfileUUID != repo.ProfileUUID || root.OwnerTransfer != nil {
			return false, fmt.Errorf("completed vault owner transition is invalid")
		}
		return true, nil
	}
	transitionHash := sha256.Sum256(payload.OwnerTransitionRoot)
	if hex.EncodeToString(transitionHash[:]) != payload.ExpectedFinalRootSHA256 {
		return false, fmt.Errorf("pending vault owner transition digest is invalid")
	}
	if currentDigest == hex.EncodeToString(transitionHash[:]) {
		root, parseErr := vaultprofile.ParseRoot(current, repo.Connector)
		if parseErr != nil || !root.Identifies(repo) ||
			root.VaultOwner.ProfileUUID != payload.OwnerTransferFromProfileUUID ||
			root.OwnerTransfer == nil || root.OwnerTransfer.OperationUUID != intent.PublicationOperationID ||
			root.OwnerTransfer.FromProfileUUID != payload.OwnerTransferFromProfileUUID ||
			root.OwnerTransfer.ToProfileUUID != repo.ProfileUUID || root.OwnerTransfer.Phase != "reviewed" {
			return false, fmt.Errorf("pending vault owner transition is invalid")
		}
		return false, nil
	}
	if currentDigest != payload.ExpectedRootSHA256 {
		return false, fmt.Errorf("the reviewed vault owner changed; review the takeover again")
	}
	// The final root is published under a different ID; see
	// connectionFinalRootPublishOptions.
	if err := publishRecoveryRoot(ctx, repo, payload.OwnerTransitionRoot, vaultprofile.PublishOptions{
		OperationID: intent.PublicationOperationID, ExpectedCurrentSHA256: payload.ExpectedRootSHA256,
	}); err != nil {
		return false, fmt.Errorf("publish reviewed vault owner transition: %w", err)
	}
	return false, nil
}

// connectionFinalRootPublishOptions returns the options for a connection's
// final root publish.
//
// The publication ID names the pending object the store uploads before it
// copies the root into place, and the store refuses different bytes under a
// pending name that already exists. A connection that takes over ownership
// publishes two roots: first the transition root (under the intent's
// publication ID, which is also the marker's OperationUUID), then the final
// root. If both used the same ID, a transition publish whose pending cleanup
// failed would leave the transition bytes under that name, and every retry of
// the final publish would be refused.
//
// The final root therefore gets its own ID, derived from the intent so a retry
// still finds and resumes its own pending object; both roots are fixed in the
// intent, so a retry always publishes the same bytes under the same name. The
// transition keeps the intent's ID so a transition upload left by an earlier
// attempt still resumes. A pending object left behind is harmless: reads and
// discovery ignore it, and the next vault password change removes it.
// Connections without an owner transfer publish only one root and keep the
// intent's ID.
//
// Don't give the two roots the same ID, and don't make either ID change between
// retries.
func connectionFinalRootPublishOptions(intent database.RepositoryConnectionIntent, payload connectionIntentPayload) vaultprofile.PublishOptions {
	options := vaultprofile.PublishOptions{OperationID: intent.PublicationOperationID,
		CreateOnly: payload.CreateRoot, ExpectedCurrentSHA256: payload.ExpectedRootSHA256}
	if payload.OwnerTransferFromProfileUUID != "" {
		options.OperationID = uuid.NewSHA1(uuid.MustParse(intent.PublicationOperationID), []byte("owner-transfer-final-root")).String()
		options.ExpectedCurrentSHA256 = payload.ExpectedFinalRootSHA256
	}
	return options
}

func restoreConnectionIntentSourceBindings(payload *connectionIntentPayload) error {
	restore := func(job *models.BackupJob) error {
		binding, ok := payload.JobStorageBindings[job.ID]
		if !ok {
			return fmt.Errorf("pending connection source binding is unavailable")
		}
		job.SourceStorageVersion = binding.Version
		job.SourceStorageKey = binding.Key
		job.SourceStorageDescriptorJSON = binding.DescriptorJSON
		return nil
	}
	for index := range payload.Jobs {
		if err := restore(&payload.Jobs[index]); err != nil {
			return err
		}
	}
	for index := range payload.LocalJobAdmissions {
		if err := restore(&payload.LocalJobAdmissions[index].Reviewed); err != nil {
			return err
		}
	}
	return nil
}

func connectRequestSelectionDigest(req ConnectExistingVaultRequest) string {
	req.Password = ""
	req.IntentID = ""
	req.IgnoredExpectedVaultUUID = ""
	req.Options = reviewedConnectorOptions(func() integrations.Integration {
		value, _ := integrations.Find(req.Connector)
		return value
	}(), req.Options)
	data, _ := json.Marshal(req)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// pendingConnectionResumesRequest fails unless req names this intent and makes
// the same selection the intent saved.
func pendingConnectionResumesRequest(intent database.RepositoryConnectionIntent, req ConnectExistingVaultRequest) error {
	var pendingPayload connectionIntentPayload
	matchesCurrentRequest := decodeConnectionIntentPayload(intent.PayloadJSON, &pendingPayload) == nil &&
		pendingPayload.RequestSelectionDigest == connectRequestSelectionDigest(req)
	if strings.TrimSpace(req.IntentID) != intent.ID || !matchesCurrentRequest {
		return fmt.Errorf("the pending connection does not match the complete current recovery selection; retry with the original Cold Storage marker, archive class, and reviewed choices")
	}
	return nil
}

func pendingConnectionRetryOptions(intent database.RepositoryConnectionIntent, submitted map[string]string) (map[string]string, error) {
	integration, ok := integrations.Find(intent.Connector)
	if !ok {
		return nil, fmt.Errorf("pending connection uses an unknown storage connector")
	}
	classified := make(map[string]integrations.Option, len(integration.Options))
	for _, option := range integration.Options {
		classified[option.Key] = option
	}
	merged := make(map[string]string, len(intent.ReviewedOptions)+len(submitted))
	for key, value := range intent.ReviewedOptions {
		merged[key] = value
	}
	for key, value := range submitted {
		option, exists := classified[key]
		if !exists {
			return nil, fmt.Errorf("pending connection retry contains an unsupported connector option")
		}
		if !option.Credential && !option.Secret {
			comparison := strings.TrimSpace(value)
			if intent.Connector == "s3" && key == "root" {
				// Retry must compare the same literal prefix admitted by the catalog;
				// trimming can reject the reviewed target or accept another one.
				comparison = value
			}
			if comparison != intent.ReviewedOptions[key] {
				return nil, fmt.Errorf("pending connection retry cannot change reviewed connector settings")
			}
			continue
		}
		merged[key] = value
	}
	normalized, err := integrations.NormalizeOptions(integration, merged)
	if err != nil {
		return nil, err
	}
	if !reflect.DeepEqual(reviewedConnectorOptions(integration, normalized), intent.ReviewedOptions) {
		return nil, fmt.Errorf("pending connection retry cannot change reviewed connector settings")
	}
	return normalized, nil
}

func handleExistingVaultRetry(db *sql.DB, auth *rcloneAuthStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			badRequest(w, "invalid method")
			return
		}
		var req RetryExistingVaultRequest
		if err := decodeRequest(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := models.ValidateVaultPassword(req.Password); err != nil {
			badRequest(w, err.Error())
			return
		}
		req.IntentID = strings.TrimSpace(req.IntentID)
		if req.IntentID == "" {
			badRequest(w, "intentId is required")
			return
		}
		intent, err := database.FindRepositoryConnectionIntentByID(db, req.IntentID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, fmt.Errorf("the selected pending connection no longer exists"))
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}

		finishRcloneAuth := func(engines.RcloneConfigDisposition) error { return nil }
		configPath := ""
		options := req.Options
		if engines.IsRcloneNativeLoginProvider(intent.Connector) && strings.TrimSpace(req.RcloneAuthSessionID) == "" {
			// This proves that the saved file is safely bound to the reviewed
			// vault. Only the later native repository validation can establish
			// whether its opaque provider credentials still work.
			_, _, configPath, err = reusablePersistentRcloneConnection(
				r.Context(), db, intent.ID, intent.Connector, intent.Location,
			)
			if err != nil {
				writeError(w, http.StatusBadRequest, fmt.Errorf("the pending vault requires native rclone reauthorization before retry"))
				return
			}
		} else {
			options, finishRcloneAuth, err = auth.mergeOptionsForFinalTransaction(
				req.RcloneAuthSessionID, intent.Connector, options,
			)
			if err != nil {
				if engines.IsRcloneNativeLoginProvider(intent.Connector) {
					writeRcloneAuthorizationError(w, http.StatusBadRequest,
						"native rclone authorization session is unavailable or not ready")
				} else {
					writeError(w, http.StatusBadRequest, err)
				}
				return
			}
			if engines.IsRcloneNativeLoginProvider(intent.Connector) {
				configPath, err = auth.configPath(req.RcloneAuthSessionID, intent.Connector)
				if err != nil {
					_ = finishRcloneAuth(engines.RcloneConfigRetained)
					writeRcloneAuthorizationError(w, http.StatusBadRequest,
						"native rclone authorization session configuration is unavailable")
					return
				}
			}
		}
		defer func() { _ = finishRcloneAuth(engines.RcloneConfigRetained) }()
		options, err = pendingConnectionRetryOptions(intent, options)
		if err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
		if err := checkRcloneRemoteSettings(r.Context(), intent.Connector, options); err != nil {
			if !writeRcloneRemoteError(w, err) {
				writeError(w, http.StatusBadRequest, err)
			}
			return
		}
		storage, err := normalizedExistingStorage(ExistingVaultStorage{
			Connector: intent.Connector, ColdStorage: intent.ColdStorage,
			ArchiveWriteClass: intent.ArchiveWriteClass, Location: intent.Location,
			Password: req.Password, Options: options,
			RcloneAuthSessionID: req.RcloneAuthSessionID, RcloneConfigPath: configPath,
		})
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		_, err = bindExistingRepositoryStorage(r.Context(), models.Repository{
			Connector: storage.Connector, Location: storage.Location, ConnectorOptions: storage.Options,
		})
		if err != nil {
			markSupportStorageObservation(w, "vault_connect_destination", err)
			writeError(w, http.StatusConflict, fmt.Errorf("pending connection storage is unavailable"))
			return
		}
		vaultUUID, keyErr := connectionIntentVaultUUID(intent)
		if keyErr != nil {
			writeError(w, http.StatusInternalServerError, keyErr)
			return
		}
		// Connecting is a foreground request, so a busy vault is waited for
		// rather than refused (the user would otherwise just press Retry until
		// the other work ends). Everything read before the wait is re-read
		// below: only the intent's state and error can change, and the
		// connection may have finished, or been cancelled, by whoever held the
		// lock. Cancelling the request (closing the page) ends the wait.
		unlock, lockErr := vaultlock.AcquireExclusiveContext(r.Context(), vaultUUID)
		if lockErr != nil {
			writeError(w, http.StatusConflict, lockErr)
			return
		}
		locked := true
		releaseVault := func() {
			if locked {
				unlock()
				locked = false
			}
		}
		defer releaseVault()
		intent, err = reloadConnectionIntentUnderLock(db, intent)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				writeError(w, http.StatusNotFound, fmt.Errorf("the selected pending connection no longer exists"))
			} else {
				writeError(w, http.StatusInternalServerError, err)
			}
			return
		}
		reportVaultProgress(r.Context(), "Verifying vault identity and finishing this computer's attachment. This may take a few minutes.")
		resumeRepositoryConnection(w, r, db, auth, intent, storage, finishRcloneAuth, releaseVault)
	}
}

// reloadConnectionIntentUnderLock re-reads a pending connection after its
// vault lock was waited for. The request that held the lock may have attached
// the vault (the intent is then gone, sql.ErrNoRows), cancelled it, or moved
// it to a later state, and resuming from the copy read before the wait would
// act on that stale state. The reviewed payload is immutable, so the lock key
// cannot change; it is checked anyway so a mismatch can never slip past the
// lock that was actually taken.
func reloadConnectionIntentUnderLock(
	db *sql.DB, before database.RepositoryConnectionIntent,
) (database.RepositoryConnectionIntent, error) {
	fresh, err := database.FindRepositoryConnectionIntentByID(db, before.ID)
	if err != nil {
		return database.RepositoryConnectionIntent{}, err
	}
	lockedKey, keyErr := connectionIntentVaultUUID(before)
	freshKey, freshKeyErr := connectionIntentVaultUUID(fresh)
	if keyErr != nil || freshKeyErr != nil || lockedKey != freshKey {
		return database.RepositoryConnectionIntent{}, fmt.Errorf("pending connection does not match the reviewed vault selection")
	}
	return fresh, nil
}

func safeConnectionRepository(repo models.Repository) connectionIntentRepository {
	return connectionIntentRepository{ID: repo.ID, Name: repo.Name, Engine: repo.Engine,
		Description: repo.Description, CheckSchedule: repo.CheckSchedule,
		MaintenanceSchedule: repo.MaintenanceSchedule, ConcurrencyMode: repo.ConcurrencyMode, AutoUnlock: repo.AutoUnlock,
		ObjectLock:  repo.ObjectLock,
		ColdStorage: repo.ColdStorage, ArchiveWriteClass: repo.ArchiveWriteClass, CreatedAt: repo.CreatedAt,
		StorageIdentityVersion: repo.StorageIdentityVersion,
		StorageIdentityKey:     repo.StorageIdentityKey, StorageIdentityJSON: repo.StorageIdentityJSON,
		ProfileUUID: repo.ProfileUUID, AttachmentGeneration: repo.AttachmentGeneration,
		NativeRepositoryID: repo.NativeRepositoryID, ClientUUID: repo.ClientUUID,
		IsVaultOwner: repo.IsVaultOwner}
}

func (value connectionIntentRepository) repository(storage ExistingVaultStorage) models.Repository {
	return models.Repository{ID: value.ID, Name: value.Name, Engine: value.Engine, Connector: storage.Connector,
		Location: storage.Location, Description: value.Description, CheckSchedule: value.CheckSchedule,
		MaintenanceSchedule: value.MaintenanceSchedule, ConcurrencyMode: value.ConcurrencyMode, AutoUnlock: value.AutoUnlock,
		ObjectLock:  value.ObjectLock,
		ColdStorage: value.ColdStorage, ArchiveWriteClass: value.ArchiveWriteClass, CreatedAt: value.CreatedAt,
		StorageIdentityVersion: value.StorageIdentityVersion,
		StorageIdentityKey:     value.StorageIdentityKey, StorageIdentityJSON: value.StorageIdentityJSON,
		CanonicalIdentity: value.StorageIdentityKey,
		Passphrase:        storage.Password, ConnectorOptions: storage.Options,
		ProfileUUID: value.ProfileUUID, AttachmentGeneration: value.AttachmentGeneration,
		NativeRepositoryID: value.NativeRepositoryID,
		ClientUUID:         value.ClientUUID, IsVaultOwner: value.IsVaultOwner,
		RcloneConfigPath: storage.RcloneConfigPath}
}

type destinationPreflightError struct{ kind string }

func (e *destinationPreflightError) Error() string {
	if e.kind == "existing" {
		return "An existing vault was found at this location. You must use the \"connect existing vault\" feature to connect to this vault."
	}
	return "Replicaro requires an empty location to create a new vault, but the selected location is not empty. Select an empty subfolder within this location or select a different location."
}

var runNewVaultPreflight = preflightNewVault

// newVaultLocationEntries lists the names directly inside a vault location
// with a read-only listing: os.ReadDir for a local folder, rclone's listing
// for every other storage type. A missing location lists as empty. Creation
// uses it to require an empty destination, and a retried creation uses it to
// see whether anything was written before the engine is run again.
//
// A local folder is read through engines.ReadWithin: the caller holds the new
// vault's lock, and a hung share must not keep it after the request ends.
func newVaultLocationEntries(ctx context.Context, repo models.Repository) ([]string, error) {
	if repo.Connector == "fs" {
		entries, err := engines.ReadWithin(ctx, func() ([]os.DirEntry, error) { return os.ReadDir(repo.Location) })
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(entries))
		for _, entry := range entries {
			names = append(names, entry.Name())
		}
		return names, nil
	}
	entries, err := (vaultprofile.Store{Repository: repo}).ListRoot(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return names, nil
}

// runPendingCreationListing is replaceable so tests can count the listing.
var runPendingCreationListing = newVaultLocationEntries

func preflightNewVault(ctx context.Context, repo models.Repository) error {
	names, err := newVaultLocationEntries(ctx, repo)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return nil
	}
	if repo.Connector == "fs" {
		// Creation holds the new vault's lock here, so this read is bounded
		// by the request context like the listing above.
		_, err = engines.ReadWithin(ctx, func() (os.FileInfo, error) {
			return os.Stat(filepath.Join(repo.Location, filepath.FromSlash(vaultprofile.PhysicalPath)))
		})
		if err == nil {
			return &destinationPreflightError{kind: "existing"}
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
			return err
		}
	} else if exists, err := (vaultprofile.Store{Repository: repo}).Exists(ctx); err == nil && exists {
		return &destinationPreflightError{kind: "existing"}
	}
	for _, entry := range names {
		name := strings.ToLower(strings.TrimSpace(entry))
		if name == "config" || strings.HasPrefix(name, "kopia.repository") || strings.HasPrefix(name, "kopia.blobcfg") {
			return &destinationPreflightError{kind: "existing"}
		}
	}
	return &destinationPreflightError{kind: "unknown"}
}

func normalizedExistingStorageBase(input ExistingVaultStorage, preview bool) (ExistingVaultStorage, error) {
	input.Connector = strings.TrimSpace(input.Connector)
	// Reconnect must observe the exact filesystem route or S3 prefix, including
	// spaces; trimming can validate and attach a different destination.
	if input.Connector != "fs" && input.Connector != "s3" {
		input.Location = strings.TrimSpace(input.Location)
	}
	input.ArchiveWriteClass = strings.TrimSpace(input.ArchiveWriteClass)
	if err := models.ValidateVaultPassword(input.Password); err != nil {
		return input, err
	}
	if input.Connector == "" || input.Location == "" {
		return input, fmt.Errorf("storage type and location are required")
	}
	if preview && input.ArchiveWriteClass != "" {
		return input, fmt.Errorf("archive write class is not accepted while checking an existing vault")
	}
	if !preview && input.ColdStorage && input.ArchiveWriteClass == "" {
		return input, fmt.Errorf("cold storage connection requires its protected or implicit GLACIER archive write class")
	}
	classForValidation := input.ArchiveWriteClass
	if preview && input.ColdStorage {
		// Initial discovery validates only cold-storage eligibility. Managed roots
		// supply their protected class later; native fallback uses implicit GLACIER.
		classForValidation = models.ArchiveWriteClassGlacier
	}
	archiveWriteClass, err := models.NormalizeColdStorage(engines.ResticID, input.Connector, input.ColdStorage, classForValidation)
	if err != nil {
		return input, err
	}
	if preview {
		input.ArchiveWriteClass = ""
	} else {
		input.ArchiveWriteClass = archiveWriteClass
	}
	// Provider folder names are literal native roots, not URLs. Their dedicated
	// validator below also runs on creation; URL parsing here would reject or
	// reinterpret legal #, ? and % characters before recovery can inspect them.
	if !engines.IsResticRcloneConnector(input.Connector) {
		if err := vaultidentity.ValidateStorageLocation(input.Connector, input.Location); err != nil {
			return input, err
		}
	}
	integration, ok := integrations.Find(input.Connector)
	if !ok {
		return input, fmt.Errorf("unsupported storage integration")
	}
	if !integrationSupportsVaultStorage(integration) {
		return input, fmt.Errorf("the selected integration is not supported as vault storage")
	}
	if engines.IsResticRcloneConnector(input.Connector) {
		userName := input.Location
		if strings.HasPrefix(userName, "Replicaro Backup ") {
			return input, fmt.Errorf("legacy Replicaro Backup rclone layouts are not migrated; select a vault created under Replicaro/<name>")
		}
		if strings.HasPrefix(userName, engines.RcloneRootPrefix) {
			userName = strings.TrimPrefix(userName, engines.RcloneRootPrefix)
		}
		_, generatedRoot, nameErr := engines.ValidateRcloneFolderName(userName)
		if nameErr != nil {
			return input, nameErr
		}
		input.Location = generatedRoot
	}
	options, err := engines.NormalizeStorageOptions(integration, input.Options)
	if err != nil {
		return input, err
	}
	if err := engines.ValidateStorageOptions(input.Connector, options); err != nil {
		return input, err
	}
	// Refuse this before the recovery metadata is read; the engine, and with
	// it the Restic check in the engine's own address validation, is not known
	// until then.
	if err := engines.ValidateSFTPHost(input.Connector, input.Location, options); err != nil {
		return input, err
	}
	if input.Connector == "webdav" {
		// Save the canonical address, exactly as creation does, so an
		// existing-vault connection and a later reconnect compare one spelling.
		input.Location, options, err = normalizedWebDAVAddress(input.Location, options)
		if err != nil {
			return input, err
		}
	}
	if integration.ID == engines.RcloneRemoteConnector {
		// Save the settings exactly as creation does (for example without a
		// trailing "/" in the path in remote), so a connection, a reconnect,
		// and the support report's redaction all see one spelling.
		options, err = engines.NormalizeConnectorOptions(engines.ResticID, integration, options)
		if err != nil {
			return input, err
		}
	}
	input.Options = options
	return input, nil
}

func normalizedExistingStorage(input ExistingVaultStorage) (ExistingVaultStorage, error) {
	return normalizedExistingStorageBase(input, false)
}

func normalizedExistingStorageForPreview(input ExistingVaultStorage) (ExistingVaultStorage, error) {
	return normalizedExistingStorageBase(input, true)
}

func recoveryEngineRepositoryWithProvidedOptions(repo models.Repository, connectorOptions map[string]string) (models.Repository, error) {
	integration, ok := integrations.Find(repo.Connector)
	if !ok {
		return models.Repository{}, fmt.Errorf("unsupported storage integration")
	}
	provided := make(map[string]string, len(connectorOptions))
	for key, value := range connectorOptions {
		provided[key] = value
	}
	options, err := engines.NormalizeConnectorOptions(repo.Engine, integration, provided)
	if err != nil {
		return models.Repository{}, err
	}
	native := repo
	native.ConnectorOptions = options
	if err := engines.ValidateConnectorAddress(native.Engine, native.Connector, native.Location, native.ConnectorOptions); err != nil {
		return models.Repository{}, err
	}
	// Import discovers the engine before admitting its native connection. Do
	// not require external SSH for Kopia's internal, verified-host-key mode.
	if err := engines.ValidateExternalSSHAvailable(native); err != nil {
		return models.Repository{}, err
	}
	return native, nil
}

func recoveryEngineRepository(repo models.Repository) (models.Repository, error) {
	return recoveryEngineRepositoryWithProvidedOptions(repo, repo.ConnectorOptions)
}

func importRecoveryRootStorage(input *ExistingVaultStorage, repository *models.Repository, root vaultprofile.Root) error {
	storage := root.Repository.S3Storage
	if storage == nil || storage.Mode == "ordinary" {
		if input.ColdStorage {
			// Cold mode is fixed when a managed vault root is created. Reconnecting
			// an ordinary managed vault through Cold Storage would silently change
			// future write behavior, so connection cannot be a conversion workflow.
			return fmt.Errorf("This Replicaro vault is configured for ordinary S3 storage. Connect it using Any S3-Compatible Object Storage.")
		}
		repository.ColdStorage = false
		repository.ArchiveWriteClass = ""
		expectedStorageClass := ""
		if storage != nil {
			expectedStorageClass = storage.StorageClass
		}
		if input.FinalAdmission && input.Connector == "s3" && strings.TrimSpace(input.Options["storage_class"]) != expectedStorageClass {
			return fmt.Errorf("the submitted S3 storage class does not match the protected vault root")
		}
		return nil
	}
	if storage.Mode != "cold" {
		return fmt.Errorf("the protected vault root has unsupported S3 storage settings")
	}
	if !input.ColdStorage {
		return fmt.Errorf("this vault is configured for cold storage; reconnect it using Cold Storage [Must Be S3 Compatible]")
	}
	input.ArchiveWriteClass = storage.DataStorageClass
	repository.ColdStorage = true
	repository.ArchiveWriteClass = storage.DataStorageClass
	return nil
}

func requireStableConnectionPreviewRoot(root vaultprofile.Root) error {
	if root.OwnerTransfer != nil {
		return fmt.Errorf("a vault owner transition is in progress; retry the pending connection or wait for it to finish")
	}
	if root.PasswordChange != nil {
		return fmt.Errorf("A vault-password change was in progress. Reconnect and enter the current vault password.")
	}
	return nil
}

func previewExistingVault(ctx context.Context, input ExistingVaultStorage) (ExistingVaultPreview, models.Repository, error) {
	providedOptions := make(map[string]string, len(input.Options))
	for key, value := range input.Options {
		providedOptions[key] = value
	}
	input, err := normalizedExistingStorageForPreview(input)
	if err != nil {
		return ExistingVaultPreview{}, models.Repository{}, err
	}
	var cleanupPreviewConfig func() error
	if !engines.IsResticRcloneConnector(input.Connector) {
		input.RcloneConfigPath, cleanupPreviewConfig, err =
			engines.NewRclonePreviewSidecarConfig(ctx)
		if err != nil {
			return ExistingVaultPreview{}, models.Repository{}, err
		}
		defer func() { _ = cleanupPreviewConfig() }()
	}
	temporary, err := bindExistingRepositoryStorage(ctx, models.Repository{
		ID: uuid.NewString(), Engine: engines.ResticID, Connector: input.Connector, Location: input.Location,
		ColdStorage: input.ColdStorage, ArchiveWriteClass: input.ArchiveWriteClass,
		Passphrase: input.Password, ConnectorOptions: input.Options,
		RcloneConfigPath: input.RcloneConfigPath,
	})
	if err != nil {
		return ExistingVaultPreview{}, models.Repository{}, err
	}
	// Address and volume facts are location hints only. Duplicate registration
	// is decided after the protected root supplies its vault UUID.
	reportVaultProgress(ctx, "Reading the vault's protected recovery metadata...")
	readResult, readErr := readRecoveryProfileDetailed(ctx, temporary)
	data := readResult.Data
	var profile *vaultprofile.Profile
	var root *vaultprofile.Root
	profileChoices := []VaultProfileChoice{}
	var selectedProfileData []byte
	var existingRepository *models.Repository
	localClientUUID := ""
	mode := "profile"
	engineID := ""
	engineVersion := ""
	if readErr == nil {
		parsed, err := vaultprofile.ParseRoot(data, input.Connector)
		if err != nil {
			return ExistingVaultPreview{}, models.Repository{}, err
		}
		if err := requireStableConnectionPreviewRoot(parsed); err != nil {
			return ExistingVaultPreview{}, models.Repository{}, err
		}
		if input.DiscoverIdentityOnly {
			// This authenticated root supplies only the local lock key. No
			// profile, inventory, registration, or owner decision escapes discovery.
			return ExistingVaultPreview{VaultUUID: parsed.VaultUUID}, temporary, nil
		}
		if saved, savedErr := database.GetRepository(previewDB(ctx), parsed.VaultUUID); savedErr == nil {
			// A unique registration does not by itself allow an update. Discovery may
			// return the registered row, but the final request must explicitly confirm
			// the update and repeat every check under this UUID's existing lock.
			if saved.Engine != parsed.Repository.Engine || saved.NativeRepositoryID != parsed.Repository.NativeRepositoryID {
				return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("the registered vault identity does not match the protected vault root")
			}
			if saved.Connector != input.Connector {
				return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("updating an existing vault cannot change its storage type")
			}
			if selected := strings.TrimSpace(input.ProfileUUID); selected != "" && selected != saved.ProfileUUID {
				return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("updating an existing vault requires its exact saved profile")
			}
			copy := saved
			existingRepository = &copy
			input.ProfileUUID = saved.ProfileUUID
		} else if !errors.Is(savedErr, sql.ErrNoRows) {
			return ExistingVaultPreview{}, models.Repository{}, savedErr
		}
		root = &parsed
		engineID = parsed.Repository.Engine
		engineVersion = engines.TestedVersion(engineID)
		temporary.ID = parsed.VaultUUID
		temporary.Engine = parsed.Repository.Engine
		temporary.NativeRepositoryID = parsed.Repository.NativeRepositoryID
		// The discovery read exists only to learn this tuple. Reread through the
		// ordinary Store boundary before using any root/profile facts so a future
		// caller cannot accidentally turn discovery into known-vault admission.
		readResult, err = readRecoveryProfileDetailed(ctx, temporary)
		if err != nil {
			return ExistingVaultPreview{}, models.Repository{}, err
		}
		data = readResult.Data
		parsed, err = vaultprofile.ParseRoot(data, input.Connector)
		if err != nil {
			return ExistingVaultPreview{}, models.Repository{}, err
		}
		if err := requireStableConnectionPreviewRoot(parsed); err != nil {
			return ExistingVaultPreview{}, models.Repository{}, err
		}
		root = &parsed
		if parsed.ObjectLock != nil {
			temporary.ObjectLock = *parsed.ObjectLock
		}
		if err := importRecoveryRootStorage(&input, &temporary, parsed); err != nil {
			return ExistingVaultPreview{}, models.Repository{}, err
		}
		reportVaultProgress(ctx, "Checking available vault profiles...")
		profileChoices, profile, selectedProfileData, err = scanRecoveryProfiles(ctx, temporary, strings.TrimSpace(input.ProfileUUID))
		if err != nil {
			if explained := explainExistingVaultCredentialStateFailure(err); explained != nil {
				return ExistingVaultPreview{}, models.Repository{}, explained
			}
			return ExistingVaultPreview{}, models.Repository{}, err
		}
		ownerPresent := false
		localClientUUID, _ = database.InstallationID(previewDB(ctx))
		for index := range profileChoices {
			profileChoices[index].VaultOwner = profileChoices[index].ProfileUUID == parsed.VaultOwner.ProfileUUID
			profileChoices[index].LocalAttachment = profileChoices[index].Attachment.ClientUUID == localClientUUID
			ownerPresent = ownerPresent || profileChoices[index].VaultOwner
		}
		if input.ReviewJoin {
			localAttachment := false
			for _, choice := range profileChoices {
				localAttachment = localAttachment || choice.LocalAttachment
			}
			if !localAttachment {
				profile, selectedProfileData = nil, nil
			}
		}
		if !ownerPresent {
			return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("the vault owner profile is missing or ambiguous")
		}
		if existingRepository != nil {
			if readResult.Degraded || profile == nil {
				return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("the selected copy does not contain the exact current vault attachment")
			}
			if profile.ProfileUUID != existingRepository.ProfileUUID ||
				profile.Attachment.ClientUUID != existingRepository.ClientUUID ||
				profile.Attachment.Generation != existingRepository.AttachmentGeneration {
				return ExistingVaultPreview{}, models.Repository{}, &existingVaultAttachmentChangedError{vaultID: existingRepository.ID}
			}
		}
	} else if errors.Is(readErr, os.ErrNotExist) {
		if exists, existsErr := recoveryProfileExists(ctx, temporary); existsErr != nil {
			if explained := explainExistingVaultCredentialStateFailure(existsErr); explained != nil {
				return ExistingVaultPreview{}, models.Repository{}, explained
			}
			return ExistingVaultPreview{}, models.Repository{}, existsErr
		} else if exists {
			return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("vault recovery profile publication is incomplete; retry the pending connection")
		}
		mode = "fallback"
		candidates, detectErr := detectExistingRepositorySignatures(ctx, temporary)
		if detectErr != nil {
			return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("inspect repository signatures: %w", detectErr)
		}
		if len(candidates) == 0 {
			// Reaching this branch means neither protected recovery metadata nor an
			// exact-root native signature was found. Keep this guidance about the
			// selected location; identity mismatch is a later, distinct safeguard
			// after a repository has been authenticated and identified.
			return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("Replicaro could not detect a vault in the selected location. Check that the location and path are correct.")
		}
		if len(candidates) != 1 {
			return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("more than one supported vault engine was detected; attachment was stopped")
		}
		engineID = candidates[0]
		// Replicaro never creates a Kopia vault on Google Drive, Dropbox,
		// OneDrive, or Any Rclone Remote, so a Kopia repository found there
		// was made outside it.
		// Kopia does not support these connectors in Replicaro, and the next
		// step would only fail with the internal connector error. Explain it
		// here instead, before any native Kopia command runs.
		if engineID == engines.KopiaID && engines.IsResticRcloneConnector(input.Connector) {
			return ExistingVaultPreview{}, models.Repository{}, newKopiaRcloneProviderError(input.Connector)
		}
	} else {
		if explained := explainExistingVaultCredentialStateFailure(readErr); explained != nil {
			return ExistingVaultPreview{}, models.Repository{}, explained
		}
		if vaultprofile.IsVaultPasswordDecryptionFailure(readErr) {
			return ExistingVaultPreview{}, models.Repository{}, errors.New("Replicaro vault exists but the vault encryption password you entered could not open the vault. Please check to make sure the vault encryption password was typed correctly. If you forgot your vault encryption password, there is no way to recover it and you will need to create a new vault instead.")
		}
		// Everything else, such as a rejected WebDAV sign-in, an untrusted
		// certificate, or an unreachable server, may have stopped before the file
		// was read at all, so don't claim it exists or blame the vault encryption
		// password.
		return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("vault.replicaro could not be read or validated. Check the location and the storage connection settings, such as the server address and port, the sign-in details for the storage account, and the certificate: %w", readErr)
	}
	temporary.Engine = engineID
	engineRepo, err := recoveryEngineRepositoryWithProvidedOptions(temporary, providedOptions)
	if err != nil {
		return ExistingVaultPreview{}, models.Repository{}, err
	}
	if input.ColdStorage && engineID != engines.ResticID {
		return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("cold storage requires an existing Restic vault")
	}
	if input.ColdStorage && input.ArchiveWriteClass == "" {
		// A native fallback import has no protected archive class yet. Read-only
		// native validation stays ordinary until final connection applies the
		// established GLACIER default.
		engineRepo.ColdStorage = false
		engineRepo.ArchiveWriteClass = ""
	}
	if engineID == engines.KopiaID || engineID == engines.ResticID {
		engineRepo.ID = uuid.NewString()
		defer engines.CleanupPreviewArtifacts(engineRepo)
	}
	engine, err := resolveEngine(engineRepo)
	var validationOutput string
	if err == nil {
		reportVaultProgress(ctx, "Validating the detected native repository...")
		validationOutput, err = engines.ValidateRepository(ctx, engine, engineRepo)
	}
	if err != nil {
		return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf(detectedVaultRejectedPasswordFormat, engineID)
	}
	if engineVersion == "" {
		if engine != nil {
			engineVersion = engine.Descriptor().Version
		}
		if engineVersion == "" {
			engineVersion = engines.TestedVersion(engineID)
		}
	}
	nativeIdentity, err := fingerprintExistingRepository(ctx, engineRepo, validationOutput)
	if err != nil {
		return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("identify native repository: %w", err)
	}
	if root != nil && nativeIdentity != root.Repository.NativeRepositoryID {
		return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("the native repository identity does not match the protected vault root")
	}
	if existingRepository != nil && (engineID != existingRepository.Engine ||
		nativeIdentity != existingRepository.NativeRepositoryID) {
		return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("the selected repository does not match the registered vault's native identity")
	}
	if input.DiscoverIdentityOnly {
		return ExistingVaultPreview{VaultUUID: deterministicImportVaultUUID(engineID, nativeIdentity)}, temporary, nil
	}
	if engineID == engines.KopiaID {
		// A Connect or Reconnect saves the error correction overhead Kopia
		// reports in the status this validation read; no extra Kopia command
		// runs for it. It comes from Kopia, not from the protected root, so it
		// is also right for a repository created outside Replicaro.
		temporary.ECCOverheadPercent, err = engines.KopiaReportedECCOverheadPercent(validationOutput)
		if err != nil {
			return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("read native repository error correction: %w", err)
		}
	}
	storageIdentity := temporary.CanonicalIdentity
	snapshots := []models.Snapshot{}
	if mode == "fallback" {
		snapshots, _, err = engine.ListSnapshots(ctx, engineRepo)
		if err != nil {
			return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("list existing snapshots: %w", err)
		}
	}
	rootSHA256 := ""
	if len(data) != 0 {
		rootSHA256 = recoverySHA256(data)
	}
	return finishExistingVaultPreview(ctx, input, temporary, engineVersion, nativeIdentity, storageIdentity,
		mode, rootSHA256, root, readResult, profile, selectedProfileData, profileChoices, existingRepository, snapshots, nil)
}

func finishExistingVaultPreview(ctx context.Context, input ExistingVaultStorage, temporary models.Repository,
	engineVersion, nativeIdentity, storageIdentity, mode, rootSHA256 string,
	root *vaultprofile.Root, readResult vaultprofile.ReadResult, profile *vaultprofile.Profile,
	selectedProfileData []byte, profileChoices []VaultProfileChoice, existingRepository *models.Repository,
	snapshots []models.Snapshot, baseline *ExistingVaultReviewBaseline,
) (ExistingVaultPreview, models.Repository, error) {
	engineID := temporary.Engine
	var err error
	knownProfileJobs := map[string]bool{}
	if profile != nil {
		for _, job := range profile.Jobs {
			knownProfileJobs[job.JobUUID] = true
		}
	}
	snapshots = recoveryVisibleSnapshots(mode, func() string {
		if profile == nil {
			return ""
		}
		return profile.ProfileUUID
	}(), knownProfileJobs, snapshots)
	importedSources := []string{}
	if mode == "fallback" {
		importedSources, err = unmanagedImportSources(snapshots)
		if err != nil {
			return ExistingVaultPreview{}, models.Repository{}, err
		}
		if encoded, encodeErr := json.Marshal(importedSources); encodeErr != nil {
			return ExistingVaultPreview{}, models.Repository{}, encodeErr
		} else if len(encoded) >= vaultprofile.MaximumProfileDecryptedSize {
			return ExistingVaultPreview{}, models.Repository{}, fmt.Errorf("discovered source paths exceed the protected profile size limit")
		}
	}
	localConflicts := map[string]string{}
	localJobSourceKeys := map[string]string{}
	reviewedLocalJobs := []models.BackupJob{}
	reviewedLocalJobsByID := map[string]models.BackupJob{}
	reviewLocalJob := func(jobID string) (models.BackupJob, bool, error) {
		if reviewed, ok := reviewedLocalJobsByID[jobID]; ok {
			return reviewed, true, nil
		}
		local, localErr := database.GetJob(previewDB(ctx), jobID)
		if errors.Is(localErr, sql.ErrNoRows) {
			return models.BackupJob{}, false, nil
		}
		if localErr != nil {
			return models.BackupJob{}, false, localErr
		}
		reviewed := reviewedLocalJobDefinition(local)
		localJobSourceKeys[local.ID] = recoverySourceKey(local.Source)
		reviewedLocalJobs = append(reviewedLocalJobs, reviewed)
		reviewedLocalJobsByID[local.ID] = reviewed
		return reviewed, true, nil
	}
	for _, snapshot := range snapshots {
		if snapshot.OwnershipMarker.Status != models.SnapshotOwnershipValid ||
			strings.TrimSpace(snapshot.OwnershipMarker.JobID) == "" {
			continue
		}
		if _, _, reviewErr := reviewLocalJob(snapshot.OwnershipMarker.JobID); reviewErr != nil {
			return ExistingVaultPreview{}, models.Repository{}, reviewErr
		}
	}
	if profile != nil {
		for _, saved := range profile.Jobs {
			reviewed, found, reviewErr := reviewLocalJob(saved.JobUUID)
			if reviewErr != nil {
				return ExistingVaultPreview{}, models.Repository{}, reviewErr
			}
			if !found {
				continue
			}
			if reviewed.Enabled {
				localConflicts[saved.JobUUID] = "enabled"
				continue
			}
			savedRetention := models.BackupJob{Retention: saved.Retention, RetentionHourly: saved.RetentionHourly,
				RetentionDaily: saved.RetentionDaily, RetentionWeekly: saved.RetentionWeekly,
				RetentionMonthly: saved.RetentionMonthly, RetentionYearly: saved.RetentionYearly}
			if reviewed.Name != saved.Name || reviewed.Source != saved.Source || reviewed.SourceKind != saved.SourceKind ||
				reviewed.Schedule != saved.Schedule ||
				!models.RetentionPolicyEqual(reviewed, savedRetention) || reviewed.Excludes != saved.Exclusions || reviewed.Tag != saved.Tag ||
				reviewed.SkipNoBackupFolders != saved.SkipNoBackupFolders || reviewed.SkipCacheDirFolders != saved.SkipCacheDirFolders ||
				reviewed.SkipMarkerFileName != saved.SkipMarkerFileName ||
				reviewed.BeforeScriptPath != saved.BeforeScriptPath ||
				reviewed.BeforeScriptMustSucceed != saved.BeforeScriptMustSucceed ||
				reviewed.AfterScriptPath != saved.AfterScriptPath ||
				reviewed.AfterScriptMustSucceed != saved.AfterScriptMustSucceed ||
				!reflect.DeepEqual(reviewed.EngineSettings, saved.EngineSettings) {
				localConflicts[saved.JobUUID] = "disabled-reviewed"
			}
		}
	}
	var existingJobAdmissions []database.RecoveredLocalJobAdmission
	if existingRepository != nil {
		localJobs, listErr := database.ListJobsForRepository(previewDB(ctx), existingRepository.ID)
		if listErr != nil {
			return ExistingVaultPreview{}, models.Repository{}, listErr
		}
		// A copied profile may be older than this installation's definitions.
		// Update review therefore binds every current local job and target set,
		// never the copy's portable job list, as the state that must survive.
		reviewedLocalJobs = reviewedLocalJobs[:0]
		reviewedLocalJobsByID = make(map[string]models.BackupJob, len(localJobs))
		existingJobAdmissions = make([]database.RecoveredLocalJobAdmission, 0, len(localJobs))
		for _, local := range localJobs {
			reviewed := reviewedLocalJobDefinition(local)
			reviewedLocalJobs = append(reviewedLocalJobs, reviewed)
			reviewedLocalJobsByID[local.ID] = reviewed
			admission := database.BuildRecoveredLocalJobAdmission(local)
			admission.Reviewed = reviewed
			existingJobAdmissions = append(existingJobAdmissions, admission)
		}
		sort.Slice(existingJobAdmissions, func(i, j int) bool {
			return existingJobAdmissions[i].Reviewed.ID < existingJobAdmissions[j].Reviewed.ID
		})
		localConflicts = map[string]string{}
	}
	// Kopia policy is deliberately not part of the connection review. The single
	// post-attachment reconciler applies it and reads it back, and until then the
	// dirty/not-ready state blocks backup and managed retention.
	protectedReview := []byte(rootSHA256 + recoverySHA256(selectedProfileData))
	// Hash the original attachment/preferences review independently of the
	// selected record. Refinement carries this comparison evidence and bounded
	// profile fingerprints, without posting every profile's preferences back.
	choicesSHA256 := ""
	if baseline != nil {
		choicesSHA256 = baseline.ChoicesSHA256
	} else {
		digestChoices := append([]VaultProfileChoice(nil), profileChoices...)
		for index := range digestChoices {
			digestChoices[index].SHA256 = ""
		}
		choicesJSON, marshalErr := json.Marshal(digestChoices)
		if marshalErr != nil {
			return ExistingVaultPreview{}, models.Repository{}, marshalErr
		}
		choicesSHA256 = recoverySHA256(choicesJSON)
	}
	protectedReview = append(protectedReview, choicesSHA256...)
	if existingRepository != nil {
		existingReview := existingVaultUpdateReview(*existingRepository)
		existingReview.CandidateLocation = temporary.Location
		existingReview.IsVaultOwner = root != nil && root.VaultOwner.ProfileUUID == existingRepository.ProfileUUID
		existingReviewJSON, marshalErr := json.Marshal(struct {
			Repository ExistingVaultUpdateReview
			Jobs       []database.RecoveredLocalJobAdmission
		}{existingReview, existingJobAdmissions})
		if marshalErr != nil {
			return ExistingVaultPreview{}, models.Repository{}, marshalErr
		}
		protectedReview = append(protectedReview, existingReviewJSON...)
	}
	digest, err := recoveryPreviewDigest(mode, engineID, engineVersion, storageIdentity, nativeIdentity,
		input.ColdStorage, input.ArchiveWriteClass, protectedReview, snapshots, reviewedLocalJobs)
	if err != nil {
		return ExistingVaultPreview{}, models.Repository{}, err
	}
	storageClass := ""
	if root != nil && root.Repository.S3Storage != nil && root.Repository.S3Storage.Mode == "ordinary" && !input.ColdStorage {
		storageClass = root.Repository.S3Storage.StorageClass
	}
	preview := ExistingVaultPreview{VaultUUID: deterministicImportVaultUUID(engineID, nativeIdentity), Mode: mode, Digest: digest, Engine: engineID,
		ColdStorage: input.ColdStorage, ArchiveWriteClass: input.ArchiveWriteClass,
		StorageClass:  storageClass,
		EngineVersion: engineVersion, Profile: profile, Profiles: profileChoices, Snapshots: snapshots, ImportedSources: importedSources,
		LocalJobSourceKeys: localJobSourceKeys,
		NativeFingerprint:  nativeIdentity, ProfileDegraded: readResult.Degraded,
		ObjectLock:                    temporary.ObjectLock,
		ObjectLockEnrollmentAvailable: mode == "fallback" && models.ObjectLockEligible(engineID, input.Connector),
		ReviewedLocalJobs:             reviewedLocalJobsByID}
	if existingRepository != nil {
		review := existingVaultUpdateReview(*existingRepository)
		review.CandidateLocation = temporary.Location
		review.IsVaultOwner = root != nil && root.VaultOwner.ProfileUUID == existingRepository.ProfileUUID
		preview.ExistingVault = &review
		preview.ExistingRepository = existingRepository
		preview.ExistingJobAdmissions = existingJobAdmissions
	}
	if root != nil {
		preview.VaultUUID = root.VaultUUID
		preview.VaultOwnerProfileUUID = root.VaultOwner.ProfileUUID
		preview.RootIntegritySchedule = root.Integrity.Schedule
		preview.RootMaintenanceSchedule = root.Maintenance.Schedule
		preview.RootCreatedAt = root.CreatedAt
		rootCopy := *root
		preview.Root = &rootCopy
		preview.Baseline = &ExistingVaultReviewBaseline{Root: rootCopy, RootSHA256: rootSHA256,
			StorageIdentity: storageIdentity, EngineVersion: engineVersion, ChoicesSHA256: choicesSHA256, Degraded: readResult.Degraded}
		for _, choice := range profileChoices {
			preview.Baseline.Profiles = append(preview.Baseline.Profiles, ExistingVaultProfileFingerprint{ProfileUUID: choice.ProfileUUID, SHA256: choice.SHA256})
		}
		if baseline != nil {
			preview.Baseline.Profiles = baseline.Profiles
		}
	}
	preview.RootSHA256 = rootSHA256
	if len(selectedProfileData) != 0 {
		profileHash := sha256.Sum256(selectedProfileData)
		preview.ProfileSHA256 = hex.EncodeToString(profileHash[:])
	}
	preview.LocalJobConflicts = localConflicts
	// The non-Restic-rclone path belongs only to this disposable preview.
	// Committed connection work must select the repository-ID-bound canonical
	// config instead of retaining a path that the deferred cleanup removes.
	if !engines.IsResticRcloneConnector(input.Connector) {
		temporary.RcloneConfigPath = ""
	}
	return preview, temporary, nil
}

// Historical source paths are native metadata, not local filesystem aliases.
// Folding Windows-looking names merges distinct case-sensitive source scopes.
func recoverySourceKey(source string) string { return source }

func repositoryFingerprint(ctx context.Context, repo models.Repository, validationOutput string) (string, error) {
	if repo.Engine == engines.ResticID && engines.IsResticRcloneConnector(repo.Connector) {
		return engines.ResticRcloneRepositoryFingerprint(ctx, repo)
	}
	// Keep direct-remote recovery aligned with creation and ordinary admission.
	// For s3, sftp, azblob, gcs, and webdav, Restic binds the authenticated
	// `cat config` result while Kopia exposes its stable repository ID in
	// native status. Raw marker bytes read through the sidecar transport are
	// not an interchangeable representation: hashing them here caused a false
	// mismatch after the exact repository had already authenticated.
	// Filesystem identity and Restic's rclone-login fingerprint remain the
	// separate branches above.
	return engines.RepositoryFingerprintContext(ctx, repo, validationOutput)
}

func detectRepositorySignatures(ctx context.Context, repo models.Repository) ([]string, error) {
	if repo.Connector == "fs" {
		return engines.DetectRepositorySignatures(ctx, repo)
	}
	entries, err := (vaultprofile.Store{Repository: repo}).ListRoot(ctx)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name)
	}
	return engines.RepositorySignatures(names), nil
}

// The preview helper is shared by handlers while retaining an explicit DB
// boundary for duplicate physical-vault checks.
type previewDBKey struct{}

func previewDB(ctx context.Context) *sql.DB { return ctx.Value(previewDBKey{}).(*sql.DB) }

// refineExistingVaultProfile consumes only comparison evidence from the modal.
// It deliberately does not refresh root or native facts. Final Add repeats the
// full locked review and rejects any changed root or selected-profile baseline.
func refineExistingVaultProfile(ctx context.Context, input ExistingVaultStorage, baseline ExistingVaultReviewBaseline) (ExistingVaultPreview, error) {
	if len(baseline.Profiles) == 0 || len(baseline.Profiles) > maximumExistingVaultProfiles || len(baseline.RootSHA256) != 64 || len(baseline.ChoicesSHA256) != 64 {
		return ExistingVaultPreview{}, fmt.Errorf("the vault review is invalid; check the vault again")
	}
	if _, err := hex.DecodeString(baseline.RootSHA256); err != nil {
		return ExistingVaultPreview{}, err
	}
	if _, err := hex.DecodeString(baseline.ChoicesSHA256); err != nil {
		return ExistingVaultPreview{}, err
	}
	if _, err := vaultprofile.MarshalRoot(baseline.Root, input.Connector); err != nil {
		return ExistingVaultPreview{}, err
	}
	if err := requireStableConnectionPreviewRoot(baseline.Root); err != nil {
		return ExistingVaultPreview{}, err
	}
	if !engines.IsResticRcloneConnector(input.Connector) {
		path, cleanup, err := engines.NewRclonePreviewSidecarConfig(ctx)
		if err != nil {
			return ExistingVaultPreview{}, err
		}
		defer func() { _ = cleanup() }()
		input.RcloneConfigPath = path
	}
	root := baseline.Root
	temporary, err := bindExistingRepositoryStorage(ctx, models.Repository{
		ID: root.VaultUUID, Engine: root.Repository.Engine, NativeRepositoryID: root.Repository.NativeRepositoryID,
		Connector: input.Connector, Location: input.Location, Passphrase: input.Password,
		ConnectorOptions: input.Options, RcloneConfigPath: input.RcloneConfigPath,
		ColdStorage: input.ColdStorage, ArchiveWriteClass: input.ArchiveWriteClass,
	})
	if err != nil {
		return ExistingVaultPreview{}, err
	}
	if temporary.CanonicalIdentity != baseline.StorageIdentity {
		return ExistingVaultPreview{}, fmt.Errorf("the selected storage changed; check the vault again")
	}
	if err := importRecoveryRootStorage(&input, &temporary, root); err != nil {
		return ExistingVaultPreview{}, err
	}
	if root.ObjectLock != nil {
		temporary.ObjectLock = *root.ObjectLock
	}
	var existing *models.Repository
	if saved, err := database.GetRepository(previewDB(ctx), root.VaultUUID); err == nil {
		existing = &saved
	} else if !errors.Is(err, sql.ErrNoRows) {
		return ExistingVaultPreview{}, err
	}
	var profile *vaultprofile.Profile
	var data []byte
	if input.ProfileUUID != "" {
		var expected string
		for _, choice := range baseline.Profiles {
			if choice.ProfileUUID == input.ProfileUUID {
				if expected != "" {
					return ExistingVaultPreview{}, fmt.Errorf("the selected profile is ambiguous; check the vault again")
				}
				expected = choice.SHA256
			}
		}
		if len(expected) != 64 {
			return ExistingVaultPreview{}, fmt.Errorf("the selected profile was not reviewed; check the vault again")
		}
		temporary.ProfileUUID = input.ProfileUUID
		read, err := readRecoveryProfile(ctx, temporary)
		if err != nil {
			return ExistingVaultPreview{}, err
		}
		if recoverySHA256(read.Data) != expected {
			return ExistingVaultPreview{}, fmt.Errorf("the reviewed vault profile changed; check the vault again")
		}
		parsed, err := vaultprofile.Parse(read.Data)
		if err != nil {
			return ExistingVaultPreview{}, err
		}
		if parsed.VaultUUID != root.VaultUUID || parsed.ProfileUUID != input.ProfileUUID || parsed.PasswordChange != nil {
			return ExistingVaultPreview{}, fmt.Errorf("the selected profile changed; check the vault again")
		}
		profile, data = &parsed, read.Data
	}
	preview, _, err := finishExistingVaultPreview(ctx, input, temporary, baseline.EngineVersion,
		root.Repository.NativeRepositoryID, baseline.StorageIdentity, "profile", baseline.RootSHA256,
		&root, vaultprofile.ReadResult{Degraded: baseline.Degraded}, profile, data, nil, existing, nil, &baseline)
	return preview, err
}

func handleExistingVaultPreview(db *sql.DB, auth *rcloneAuthStore) http.HandlerFunc {
	return handleExistingVaultReview(db, auth, false)
}

func handleExistingVaultProfileSelection(db *sql.DB, auth *rcloneAuthStore) http.HandlerFunc {
	return handleExistingVaultReview(db, auth, true)
}

func handleExistingVaultReview(db *sql.DB, auth *rcloneAuthStore, refine bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			badRequest(w, "invalid method")
			return
		}
		var request struct {
			ExistingVaultStorage
			Baseline *ExistingVaultReviewBaseline `json:"baseline,omitempty"`
		}
		if err := decodeRequest(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		input := request.ExistingVaultStorage
		if err := models.ValidateVaultPassword(input.Password); err != nil {
			badRequest(w, err.Error())
			return
		}
		if strings.TrimSpace(input.ArchiveWriteClass) != "" {
			badRequest(w, "archive write class is not accepted while checking an existing vault")
			return
		}
		// A cloud sign-in's Check reads the session's staged rclone.conf,
		// so it holds the session like create and connect do; otherwise
		// expiry could remove that config mid-Check. Releasing it as
		// retained keeps the sign-in for the next step.
		var release func(engines.RcloneConfigDisposition) error
		var err error
		input.Options, release, err = auth.mergeOptionsForFinalTransaction(
			input.RcloneAuthSessionID, input.Connector, input.Options,
		)
		if err == nil {
			defer func() { _ = release(engines.RcloneConfigRetained) }()
		}
		if err != nil {
			if engines.IsRcloneNativeLoginProvider(input.Connector) {
				writeRcloneAuthorizationError(w, http.StatusBadRequest,
					"native rclone authorization session is unavailable or not ready")
			} else {
				writeError(w, http.StatusBadRequest, err)
			}
			return
		}
		if engines.IsRcloneNativeLoginProvider(input.Connector) {
			input.RcloneConfigPath, err = auth.configPath(
				input.RcloneAuthSessionID, input.Connector,
			)
			if err != nil {
				writeRcloneAuthorizationError(w, http.StatusBadRequest,
					"native rclone authorization session configuration is unavailable")
				return
			}
		}
		// An Any Rclone Remote vault has no session: its sidecar and repository
		// are read through the user's own config file, which is checked first,
		// on a first connection and on a reconnect alike.
		if err := checkRcloneRemoteSettings(r.Context(), input.Connector, input.Options); err != nil {
			if !writeRcloneRemoteError(w, err) {
				writeError(w, http.StatusBadRequest, err)
			}
			return
		}
		normalized, err := normalizedExistingStorageForPreview(input)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		_, err = bindExistingRepositoryStorage(r.Context(), models.Repository{
			Connector: normalized.Connector, Location: normalized.Location, ConnectorOptions: normalized.Options,
		})
		if err != nil {
			markSupportStorageObservation(w, "vault_connect_destination", err)
			writeError(w, http.StatusBadRequest, err)
			return
		}
		reportVaultProgress(r.Context(), "Rechecking the reviewed native vault and recovery profile...")
		ctx := context.WithValue(r.Context(), previewDBKey{}, db)
		if refine {
			if request.Baseline == nil {
				badRequest(w, "check the vault before selecting a profile")
				return
			}
			preview, err := refineExistingVaultProfile(ctx, normalized, *request.Baseline)
			if err != nil {
				// A refined review can fail its second storage bind after the
				// initial check succeeded; retain only its fixed observation fact.
				markSupportStorageObservation(w, "vault_connect_destination", err)
				writeError(w, http.StatusConflict, err)
				return
			}
			writeJSON(w, publicExistingVaultPreview(preview))
			return
		}
		discoveryInput := normalized
		discoveryInput.DiscoverIdentityOnly = true
		discovery, _, err := previewExistingVault(ctx, discoveryInput)
		if err != nil {
			writeExistingVaultPreviewError(w, err)
			return
		}
		vaultUUID := discovery.VaultUUID
		unlock, err := vaultlock.AcquireExclusiveContext(r.Context(), vaultUUID)
		if err != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("wait to inspect the selected vault: %w", err))
			return
		}
		defer unlock()
		reportVaultProgress(r.Context(), "Verifying vault identity and recovery profiles under the vault lock...")
		lockedPreview, lockedRepository, lockedErr := previewExistingVault(ctx, normalized)
		if lockedErr != nil {
			writeExistingVaultPreviewError(w, lockedErr)
			return
		}
		lockedVaultUUID := deterministicImportVaultUUID(lockedRepository.Engine, lockedPreview.NativeFingerprint)
		if lockedPreview.Root != nil {
			lockedVaultUUID = lockedPreview.Root.VaultUUID
		}
		if lockedVaultUUID != vaultUUID {
			writeStaleVaultReview(w, "the selected vault changed while waiting for its operation lock; check the vault again")
			return
		}
		// Only the complete review repeated beneath the discovered UUID lock may
		// be returned; the first pass supplied the serialization key and nothing else.
		writeJSON(w, publicExistingVaultPreview(lockedPreview))
	}
}

func writeStaleVaultReview(w http.ResponseWriter, message string) {
	markSupportResponseError(w, errors.New(message), false)
	writeCodedError(w, http.StatusConflict, "vault_review_changed", message)
}

func handleExistingVaultConnect(db *sql.DB, auth *rcloneAuthStore) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			badRequest(w, "invalid method")
			return
		}
		var req ConnectExistingVaultRequest
		if err := decodeRequest(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := models.ValidateVaultPassword(req.Password); err != nil {
			badRequest(w, err.Error())
			return
		}
		req.Name = strings.TrimSpace(req.Name)
		if strings.TrimSpace(req.IntentID) == "" {
			if err := database.ValidateRepositoryName(req.Name); err != nil {
				badRequest(w, err.Error())
				return
			}
		}
		var err error
		var finishRcloneAuth func(engines.RcloneConfigDisposition) error
		if engines.IsRcloneNativeLoginProvider(req.Connector) &&
			strings.TrimSpace(req.RcloneAuthSessionID) == "" &&
			strings.TrimSpace(req.IntentID) != "" {
			pending, _, configPath, pendingErr := reusablePersistentRcloneConnection(
				r.Context(), db, req.IntentID, req.Connector, req.Location,
			)
			if pendingErr != nil {
				writeError(
					w, http.StatusBadRequest,
					fmt.Errorf("the pending vault requires native rclone reauthorization before retry"),
				)
				return
			}
			req.RcloneConfigPath = configPath
			req.Options = pending.ReviewedOptions
			finishRcloneAuth = func(engines.RcloneConfigDisposition) error { return nil }
		} else {
			req.Options, finishRcloneAuth, err = auth.mergeOptionsForFinalTransaction(
				req.RcloneAuthSessionID, req.Connector, req.Options,
			)
			if err != nil {
				if engines.IsRcloneNativeLoginProvider(req.Connector) {
					writeRcloneAuthorizationError(w, http.StatusBadRequest,
						"native rclone authorization session is unavailable or not ready")
				} else {
					writeError(w, http.StatusBadRequest, err)
				}
				return
			}
			if engines.IsRcloneNativeLoginProvider(req.Connector) {
				req.RcloneConfigPath, err = auth.configPath(
					req.RcloneAuthSessionID, req.Connector,
				)
				if err != nil {
					_ = finishRcloneAuth(engines.RcloneConfigRetained)
					writeRcloneAuthorizationError(w, http.StatusBadRequest,
						"native rclone authorization session configuration is unavailable")
					return
				}
			}
		}
		defer func() { _ = finishRcloneAuth(engines.RcloneConfigRetained) }()
		if err := checkRcloneRemoteSettings(r.Context(), req.Connector, req.Options); err != nil {
			if !writeRcloneRemoteError(w, err) {
				writeError(w, http.StatusBadRequest, err)
			}
			return
		}
		normalizedStorage, normalizeErr := normalizedExistingStorage(req.ExistingVaultStorage)
		if normalizeErr != nil {
			writeError(w, http.StatusBadRequest, normalizeErr)
			return
		}
		req.ExistingVaultStorage = normalizedStorage
		boundRepository, identityErr := bindExistingRepositoryStorage(r.Context(), models.Repository{
			Connector: req.Connector, Location: req.Location, ConnectorOptions: req.Options,
		})
		if identityErr != nil {
			markSupportStorageObservation(w, "vault_connect_destination", identityErr)
			badRequest(w, identityErr.Error())
			return
		}
		identity := boundRepository.CanonicalIdentity
		var unlock func()
		lockHeld := false
		releaseVault := func() {
			if lockHeld {
				unlock()
				lockHeld = false
			}
		}
		defer releaseVault()
		if intent, intentErr := database.FindRepositoryConnectionIntentByID(db, strings.TrimSpace(req.IntentID)); strings.TrimSpace(req.IntentID) != "" && intentErr == nil {
			if resumeErr := pendingConnectionResumesRequest(intent, req); resumeErr != nil {
				writeError(w, http.StatusConflict, resumeErr)
				return
			}
			vaultUUID, keyErr := connectionIntentVaultUUID(intent)
			if keyErr != nil {
				writeError(w, http.StatusInternalServerError, keyErr)
				return
			}
			// Wait for a busy vault, then resume from the intent as it is
			// now (see reloadConnectionIntentUnderLock).
			var lockErr error
			unlock, lockErr = vaultlock.AcquireExclusiveContext(r.Context(), vaultUUID)
			if lockErr != nil {
				writeError(w, http.StatusConflict, lockErr)
				return
			}
			lockHeld = true
			fresh, reloadErr := reloadConnectionIntentUnderLock(db, intent)
			if errors.Is(reloadErr, sql.ErrNoRows) {
				writeError(w, http.StatusConflict, fmt.Errorf("the selected pending connection no longer exists; check the vault again"))
				return
			}
			if reloadErr != nil {
				writeError(w, http.StatusInternalServerError, reloadErr)
				return
			}
			resumeRepositoryConnection(
				w, r, db, auth, fresh, req.ExistingVaultStorage,
				finishRcloneAuth, releaseVault,
			)
			return
		} else if strings.TrimSpace(req.IntentID) != "" && intentErr != sql.ErrNoRows {
			writeError(w, http.StatusInternalServerError, intentErr)
			return
		}
		if strings.TrimSpace(req.IntentID) != "" {
			writeError(w, http.StatusConflict, fmt.Errorf("the selected pending connection no longer exists; check the vault again"))
			return
		}
		reportVaultProgress(r.Context(), "Rechecking the reviewed native vault and recovery profile...")
		ctx := context.WithValue(r.Context(), previewDBKey{}, db)
		reviewStorage := req.ExistingVaultStorage
		reviewStorage.ProfileUUID = strings.TrimSpace(req.ProfileUUID)
		reviewStorage.ArchiveWriteClass = ""
		reviewStorage.FinalAdmission = true
		reviewStorage.ReviewJoin = req.ProfileAction == "join"
		initialVaultUUID := req.ReviewedVaultUUID
		if parsed, parseErr := uuid.Parse(initialVaultUUID); parseErr != nil || parsed.String() != initialVaultUUID {
			badRequest(w, "the reviewed vault UUID is invalid; check the vault again")
			return
		}
		// The submitted UUID selects serialization only. Wait for a busy vault;
		// the complete review below runs under the lock, must independently
		// confirm the UUID before any external effect, and rejects a vault that
		// changed while this waited.
		var lockErr error
		unlock, lockErr = vaultlock.AcquireExclusiveContext(r.Context(), initialVaultUUID)
		if lockErr != nil {
			writeError(w, http.StatusConflict, lockErr)
			return
		}
		lockHeld = true
		lockedPreview, lockedRepo, lockedReviewErr := reviewExistingVaultForConnection(ctx, reviewStorage)
		if lockedReviewErr != nil {
			// Final review can rebind storage after the earlier admission succeeded.
			// Preserve its observation fact before returning the existing error.
			markSupportStorageObservation(w, "vault_connect_destination", lockedReviewErr)
			writeError(w, http.StatusBadRequest, lockedReviewErr)
			return
		}
		lockedVaultUUID := deterministicImportVaultUUID(lockedRepo.Engine, lockedPreview.NativeFingerprint)
		if lockedPreview.Root != nil {
			lockedVaultUUID = lockedPreview.Root.VaultUUID
		}
		if lockedVaultUUID != initialVaultUUID {
			writeStaleVaultReview(w, "the selected vault changed while waiting for its operation lock; check the vault again")
			return
		}
		preview, repo := lockedPreview, lockedRepo
		if req.Mode != preview.Mode || req.Digest == "" || req.Digest != preview.Digest {
			clientUUID, clientErr := database.InstallationID(db)
			if clientErr != nil {
				writeError(w, http.StatusInternalServerError, clientErr)
				return
			}
			if finalNewJoinAtCapacity(preview, repo, req, clientUUID) {
				writeError(w, http.StatusConflict, fmt.Errorf("%s", existingVaultProfileCapacityMessage))
				return
			}
			writeStaleVaultReview(w, "the reviewed vault profile or visible source set changed; check the vault again")
			return
		}
		updatingExisting := preview.ExistingRepository != nil
		if updatingExisting != req.UpdateExistingVault {
			if updatingExisting {
				writeError(w, http.StatusConflict, fmt.Errorf("confirm Update existing vault after reviewing the exact saved-vault changes"))
			} else {
				writeError(w, http.StatusConflict, fmt.Errorf("the reviewed vault is not an existing registration"))
			}
			return
		}
		if updatingExisting {
			expectedConfirmation, confirmationErr := expectedExistingVaultUpdateConfirmation(preview, repo.Location, req)
			if confirmationErr != nil {
				writeError(w, http.StatusBadRequest, confirmationErr)
				return
			}
			var submittedConfirmation ExistingVaultUpdateConfirmation
			if req.UpdateExistingVaultReview != nil {
				submittedConfirmation = *req.UpdateExistingVaultReview
				submittedConfirmation.ConcurrencyMode, confirmationErr = models.NormalizeConcurrencyModeForConnector(
					preview.ExistingRepository.Connector, submittedConfirmation.ConcurrencyMode,
				)
				if confirmationErr != nil {
					writeError(w, http.StatusBadRequest, confirmationErr)
					return
				}
			}
			if req.UpdateExistingVaultReview == nil || submittedConfirmation != expectedConfirmation {
				writeError(w, http.StatusConflict, fmt.Errorf("confirm Update existing vault after reviewing the exact saved-vault changes"))
				return
			}
		}
		rootCold := preview.Root != nil && preview.Root.Repository.S3Storage != nil &&
			preview.Root.Repository.S3Storage.Mode == "cold"
		if rootCold && req.ArchiveWriteClass != preview.ArchiveWriteClass {
			writeError(w, http.StatusConflict, fmt.Errorf("the reviewed archive write class no longer matches the protected vault root; check the vault again"))
			return
		}
		if preview.Mode == "fallback" && preview.ColdStorage && req.ArchiveWriteClass != models.ArchiveWriteClassGlacier {
			// Native Cold Storage import has no protected class to recover and no
			// connection-time class choice. Its fixed admission default is GLACIER.
			writeError(w, http.StatusConflict, fmt.Errorf("native Cold Storage imports use the GLACIER storage class; check the vault again"))
			return
		}
		repo.ColdStorage = req.ColdStorage
		repo.ArchiveWriteClass = req.ArchiveWriteClass
		clientUUID, clientErr := database.InstallationID(db)
		if clientErr != nil {
			writeError(w, http.StatusInternalServerError, clientErr)
			return
		}
		repo.ClientUUID = clientUUID
		repo.AutoUnlock = true
		if preview.Root != nil {
			repo.ID = preview.Root.VaultUUID
			repo.CreatedAt = preview.Root.CreatedAt
			repo.NativeRepositoryID = preview.Root.Repository.NativeRepositoryID
		}
		if preview.Mode == "fallback" {
			repo.ID = deterministicImportVaultUUID(repo.Engine, preview.NativeFingerprint)
			repo.CreatedAt = time.Now().UTC()
			repo.NativeRepositoryID = preview.NativeFingerprint
			// A missing root sidecar does not prove that the profile namespace is
			// empty. Refuse first managed publication when any profile sidecar is
			// already present so one client UUID cannot be attached to a second
			// profile through an orphaned or externally copied profile record.
			profiles, scanErr := scanConnectionProfileAttachments(r.Context(), repo)
			if scanErr != nil {
				writeError(w, http.StatusConflict, fmt.Errorf("the existing vault profile namespace could not be verified; check the vault again: %w", scanErr))
				return
			}
			if len(profiles) != 0 {
				writeError(w, http.StatusConflict, fmt.Errorf("the existing vault contains managed profiles but no protected root; restore the protected root before connecting"))
				return
			}
		}
		if updatingExisting {
			saved := *preview.ExistingRepository
			// Keep the candidate's freshly proven address and credentials, but all
			// local attachment identity and unedited preferences come from the one
			// registered row. A copied profile is never local-definition authority.
			repo.Name = saved.Name
			repo.Description = saved.Description
			repo.ProfileUUID = saved.ProfileUUID
			repo.AttachmentGeneration = saved.AttachmentGeneration
			repo.ClientUUID = saved.ClientUUID
			repo.AutoUnlock = saved.AutoUnlock
			repo.CreatedAt = saved.CreatedAt
			repo.CheckSchedule = saved.CheckSchedule
			repo.MaintenanceSchedule = saved.MaintenanceSchedule
			repo.ConcurrencyMode = saved.ConcurrencyMode
			repo.ObjectLock = saved.ObjectLock
		}
		if repo.ID != initialVaultUUID {
			writeError(w, http.StatusConflict, fmt.Errorf("the selected vault changed during locked admission; check the vault again"))
			return
		}
		if !updatingExisting {
			if duplicateErr := database.AssertVaultUUIDAvailable(db, repo.ID); duplicateErr != nil {
				writeError(w, http.StatusConflict, duplicateErr)
				return
			}
		}
		// Imported profile values initialize the client form, but every editable
		// reviewed field is authoritative at initial local attachment.
		applyReviewedConnectionVaultFields(&repo, req)
		repo.ConcurrencyMode, err = models.NormalizeConcurrencyModeForConnector(repo.Connector, repo.ConcurrencyMode)
		if err != nil {
			badRequest(w, err.Error())
			return
		}
		if preview.Mode == "fallback" {
			repo.ObjectLock, err = models.NormalizeObjectLock(repo.Engine, repo.Connector, req.ObjectLock)
			if err != nil {
				badRequest(w, err.Error())
				return
			}
			if repo.ObjectLock.Enrolled && repo.ObjectLock.Paused {
				badRequest(w, "initial object lock enrollment must be active")
				return
			}
			if !models.ObjectLockMaintenanceEligible(repo.ObjectLock, repo.MaintenanceSchedule) {
				repo.MaintenanceSchedule, err = models.LongestEligibleObjectLockMaintenance(repo.ObjectLock)
				if err != nil {
					badRequest(w, err.Error())
					return
				}
			}
		} else if updatingExisting {
			if req.ObjectLock != preview.ExistingRepository.ObjectLock {
				badRequest(w, "object lock settings cannot be changed by Update existing vault")
				return
			}
			repo.ObjectLock = preview.ExistingRepository.ObjectLock
		} else {
			// Enrollment is a creation/first-import decision. Managed reconnects
			// recover it from the protected root and cannot reinterpret the form as
			// a second enrollment boundary.
			if req.ObjectLock != preview.ObjectLock {
				badRequest(w, "object lock settings do not match the protected vault root")
				return
			}
			repo.ObjectLock = preview.ObjectLock
		}
		if repo.Name == "" || !database.ValidSchedule(repo.CheckSchedule) || !database.ValidSchedule(repo.MaintenanceSchedule) {
			badRequest(w, "reviewed vault name and care schedules are required")
			return
		}
		var transition connectionProfileTransition
		if updatingExisting {
			if strings.TrimSpace(req.ProfileUUID) != "" && strings.TrimSpace(req.ProfileUUID) != repo.ProfileUUID ||
				strings.TrimSpace(req.ProfileAction) != "" && strings.TrimSpace(req.ProfileAction) != "reconnect" ||
				strings.TrimSpace(req.OwnerAction) != "" && strings.TrimSpace(req.OwnerAction) != "keep" {
				writeError(w, http.StatusConflict, fmt.Errorf("Update existing vault cannot join, take over, or change the saved attachment"))
				return
			}
			transition = connectionProfileTransition{Profile: preview.Profile}
		} else {
			var transitionErr error
			transition, transitionErr = prepareConnectionProfile(preview, repo, req, clientUUID, time.Now().UTC())
			if transitionErr != nil {
				writeError(w, http.StatusConflict, transitionErr)
				return
			}
		}
		preview.Profile = transition.Profile
		repo.ProfileUUID = transition.Profile.ProfileUUID
		repo.AttachmentGeneration = transition.Profile.Attachment.Generation
		repo.IsVaultOwner = preview.Mode == "fallback" || preview.VaultOwnerProfileUUID == repo.ProfileUUID
		recoveryNameReservationMu.Lock()
		nameReservationLocked := true
		defer func() {
			if nameReservationLocked {
				recoveryNameReservationMu.Unlock()
			}
		}()
		if !updatingExisting {
			repo, err = database.RenameRecoveredRepositoryNameConflict(db, repo)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
		}
		jobs := []models.BackupJob{}
		localJobAdmissions := []database.RecoveredLocalJobAdmission{}
		dormantRows := []database.DormantRecoveryJob{}
		if preview.Mode == "fallback" {
			jobs = importedSourceJobs(repo, preview.ImportedSources)
		}
		if preview.Profile != nil && !transition.CreateOnly && !updatingExisting {
			// Choosing a profile is what authorizes the recovery. Every job in that
			// profile is restored disabled, so a partial selection cannot silently
			// split the portable definition. A Join transition is create-only and must
			// not pick up the discovery scan's default of selecting the only profile.
			// Connectors that auto-select their only profile may omit the selection
			// fields, so the validated transition, not the raw request shape, decides
			// this. Do not reintroduce per-job request selections here.
			for _, saved := range preview.Profile.Jobs {
				job := models.BackupJob{ID: saved.JobUUID, Name: saved.Name, Source: saved.Source, SourceKind: saved.SourceKind, Schedule: saved.Schedule,
					Retention: saved.Retention, RetentionHourly: saved.RetentionHourly,
					RetentionDaily: saved.RetentionDaily, RetentionWeekly: saved.RetentionWeekly,
					RetentionMonthly: saved.RetentionMonthly, RetentionYearly: saved.RetentionYearly,
					Excludes: saved.Exclusions, Tag: saved.Tag,
					SkipNoBackupFolders: saved.SkipNoBackupFolders, SkipCacheDirFolders: saved.SkipCacheDirFolders,
					SkipMarkerFileName: saved.SkipMarkerFileName,
					BeforeScriptPath:   saved.BeforeScriptPath, BeforeScriptMustSucceed: saved.BeforeScriptMustSucceed,
					AfterScriptPath: saved.AfterScriptPath, AfterScriptMustSucceed: saved.AfterScriptMustSucceed,
					EngineSettings: saved.EngineSettings, Enabled: false,
					PortableTargetIDs: append([]string(nil), saved.TargetVaultUUIDs...),
					Targets:           []models.BackupJobTarget{{RepositoryID: repo.ID, Engine: repo.Engine}}}
				profileTargetIDs := append([]string(nil), job.PortableTargetIDs...)
				local, localErr := database.GetJob(db, saved.JobUUID)
				reviewedLocal, localWasReviewed := preview.ReviewedLocalJobs[saved.JobUUID]
				if localErr == nil {
					if !localWasReviewed {
						writeError(w, http.StatusConflict, fmt.Errorf("a local job appeared after the recovery preview; check the vault again"))
						return
					}
					// A same-UUID local row may preserve its attachment-specific targets,
					// but it cannot replace the selected profile's portable definition.
					if !reviewedLocalDefinitionMatchesRecovered(reviewedLocal, job, repo.Engine) {
						writeError(w, http.StatusConflict, fmt.Errorf("local job %s conflicts with the selected profile definition", saved.JobUUID))
						return
					}
					job = local
					// Portable target membership belongs to the profile definition even
					// when the local row supplies attachment-specific bindings. Preserve
					// both sets; normalization performs deterministic de-duplication.
					job.PortableTargetIDs = append(append([]string(nil), local.PortableTargetIDs...), profileTargetIDs...)
					job.Enabled = false
					job.NextRun = ""
					job.Targets = []models.BackupJobTarget{{RepositoryID: repo.ID, Engine: repo.Engine}}
				} else if !errors.Is(localErr, sql.ErrNoRows) {
					writeError(w, http.StatusConflict, fmt.Errorf("the reviewed local job is unavailable"))
					return
				} else {
					if localWasReviewed {
						writeError(w, http.StatusConflict, fmt.Errorf("the reviewed local job is unavailable"))
						return
					}
					job.SourceBindingState = "unbound_imported"
					job.SourceStorageVersion = ""
					job.SourceStorageKey = ""
					job.SourceStorageDescriptorJSON = ""
				}
				jobs = append(jobs, job)
			}
		}
		if updatingExisting {
			jobs, err = database.ListJobsForRepository(db, repo.ID)
			if err != nil {
				writeError(w, http.StatusInternalServerError, err)
				return
			}
			localJobAdmissions = append([]database.RecoveredLocalJobAdmission(nil), preview.ExistingJobAdmissions...)
		} else {
			normalizedJobs, normalizeErr := database.NormalizeRecoveredJobTargets(db, repo.ID, jobs)
			if normalizeErr != nil {
				writeError(w, http.StatusConflict, normalizeErr)
				return
			}
			jobs, normalizeErr = database.RenameRecoveredJobNameConflicts(db, normalizedJobs)
			if normalizeErr != nil {
				writeError(w, http.StatusInternalServerError, normalizeErr)
				return
			}
			localJobAdmissions, normalizeErr = recoveredLocalJobAdmissions(db, preview, jobs, repo.Engine)
			if normalizeErr != nil {
				writeError(w, http.StatusConflict, normalizeErr)
				return
			}
			if err := database.ValidateRecoveredRepositoryAttachment(db, repo, jobs); err != nil {
				writeError(w, http.StatusConflict, err)
				return
			}
		}
		var profileData []byte
		if transition.CreateOnly {
			profile, buildErr := vaultprofile.BuildProfile(repo, jobs, nil, repo.ProfileUUID,
				clientUUID, repo.AttachmentGeneration, 1, transition.Profile.Attachment.Display,
				time.Time{}, time.Now().UTC())
			if buildErr != nil {
				writeError(w, http.StatusBadRequest, buildErr)
				return
			}
			profileData, buildErr = vaultprofile.Marshal(profile)
			if buildErr != nil {
				writeError(w, http.StatusBadRequest, buildErr)
				return
			}
			transition.Profile = &profile
		} else {
			profileToPersist := *preview.Profile
			var marshalErr error
			profileData, marshalErr = vaultprofile.Marshal(profileToPersist)
			if marshalErr != nil {
				writeError(w, http.StatusBadRequest, marshalErr)
				return
			}
		}
		var rootData []byte
		publishRoot := preview.Mode == "fallback"
		createRoot := preview.Mode == "fallback"
		ownerAction := strings.TrimSpace(req.OwnerAction)
		if ownerAction != "" && ownerAction != "keep" && ownerAction != "takeover" {
			badRequest(w, "vault owner action is invalid")
			return
		}
		var rootToPersist vaultprofile.Root
		if preview.Mode == "fallback" {
			built, buildErr := vaultprofile.BuildRoot(repo, repo.NativeRepositoryID, repo.ProfileUUID, 1,
				time.Time{}, time.Now().UTC())
			if buildErr != nil {
				writeError(w, http.StatusBadRequest, buildErr)
				return
			}
			rootToPersist = built
			repo.IsVaultOwner = true
		} else {
			rootToPersist = *preview.Root
			if ownerAction == "takeover" && rootToPersist.VaultOwner.ProfileUUID != repo.ProfileUUID {
				rootToPersist.OwnerTransfer = nil
				rootToPersist.VaultOwner.ProfileUUID = repo.ProfileUUID
				publishRoot = true
				repo.IsVaultOwner = true
			}
			careChanged := rootToPersist.Integrity.Schedule != repo.CheckSchedule ||
				rootToPersist.Maintenance.Schedule != repo.MaintenanceSchedule
			if repo.IsVaultOwner && careChanged {
				rootToPersist.Integrity.Schedule = repo.CheckSchedule
				rootToPersist.Maintenance.Schedule = repo.MaintenanceSchedule
				publishRoot = true
			} else if !repo.IsVaultOwner && careChanged {
				writeError(w, http.StatusForbidden, fmt.Errorf("only the vault owner can change integrity-check or maintenance schedules"))
				return
			}
			if publishRoot {
				rootToPersist.Revision++
				if ownerAction == "takeover" && preview.Root.VaultOwner.ProfileUUID != repo.ProfileUUID {
					// The reviewed transition root consumes the next revision; the
					// completed owner root is the following revision.
					rootToPersist.Revision++
				}
				rootToPersist.UpdatedAt = time.Now().UTC()
			}
		}
		if publishRoot || updatingExisting {
			var marshalRootErr error
			rootData, marshalRootErr = vaultprofile.MarshalRoot(rootToPersist, repo.Connector)
			if marshalRootErr != nil {
				writeError(w, http.StatusBadRequest, marshalRootErr)
				return
			}
		}
		jobStorageBindings := make(map[string]connectionSourceStorageBinding, len(jobs))
		for _, job := range jobs {
			jobStorageBindings[job.ID] = connectionSourceStorageBinding{
				Version: job.SourceStorageVersion, Key: job.SourceStorageKey,
				DescriptorJSON: job.SourceStorageDescriptorJSON,
			}
		}
		nativeRepo, nativeRepoErr := recoveryEngineRepository(repo)
		if nativeRepoErr != nil {
			writeError(w, http.StatusBadRequest, nativeRepoErr)
			return
		}
		engine, engineErr := resolveEngine(nativeRepo)
		if engineErr != nil {
			writeError(w, http.StatusBadRequest, engineErr)
			return
		}
		nativeOwnerClientUUID := ""
		if repo.IsVaultOwner {
			nativeOwnerClientUUID = clientUUID
		} else {
			for _, choice := range preview.Profiles {
				if choice.ProfileUUID == preview.VaultOwnerProfileUUID {
					nativeOwnerClientUUID = choice.Attachment.ClientUUID
					break
				}
			}
		}

		publicationOperationID := uuid.NewString()
		// Publication readback against the expected hashes is the safeguard here. The
		// connection flow deliberately adds no remote lease, heartbeat, or
		// coordination protocol for the small race with another computer.
		payload := connectionIntentPayload{Repository: safeConnectionRepository(repo), Jobs: jobs,
			LocalJobAdmissions: localJobAdmissions,
			JobStorageBindings: jobStorageBindings,
			Dormant:            dormantRows, Profile: profileData, Root: rootData,
			PhysicalVaultIdentity:  identity,
			StorageIdentityVersion: repo.StorageIdentityVersion,
			StorageIdentityKey:     repo.StorageIdentityKey, StorageIdentityJSON: repo.StorageIdentityJSON,
			RequestSelectionDigest: connectRequestSelectionDigest(req),
			ExpectedProfileSHA256:  transition.ExpectedSHA256,
			ExpectedRootSHA256:     preview.RootSHA256,
			PublishProfile:         transition.Publish,
			CreateProfile:          transition.CreateOnly,
			PublishRoot:            publishRoot,
			CreateRoot:             createRoot,
			TakeoverFromClientUUID: transition.TakeoverFromClient,
			NativeOwnerClientUUID:  nativeOwnerClientUUID,
			UpdateExistingVault:    updatingExisting}
		if updatingExisting {
			payload.ExistingRepository = connectionIntentExistingRepository{
				ID: preview.ExistingRepository.ID, ProfileUUID: preview.ExistingRepository.ProfileUUID,
				AttachmentGeneration: preview.ExistingRepository.AttachmentGeneration,
			}
		}
		if preview.Mode == "fallback" {
			for _, source := range preview.ImportedSources {
				payload.ReviewedSourceRoots = append(payload.ReviewedSourceRoots, recoverySourceKey(source))
			}
			sort.Strings(payload.ReviewedSourceRoots)
		}
		if ownerAction == "takeover" && preview.Root != nil &&
			preview.Root.VaultOwner.ProfileUUID != repo.ProfileUUID {
			payload.OwnerTransferFromProfileUUID = preview.Root.VaultOwner.ProfileUUID
		}
		if payload.OwnerTransferFromProfileUUID != "" {
			transitionRoot := *preview.Root
			transitionRoot.OwnerTransfer = &vaultprofile.OwnerTransfer{
				OperationUUID: publicationOperationID, FromProfileUUID: payload.OwnerTransferFromProfileUUID,
				ToProfileUUID: repo.ProfileUUID, Phase: "reviewed",
			}
			transitionRoot.Revision++
			transitionRoot.UpdatedAt = rootToPersist.UpdatedAt
			transitionData, transitionErr := vaultprofile.MarshalRoot(transitionRoot, repo.Connector)
			if transitionErr != nil {
				writeError(w, http.StatusInternalServerError, transitionErr)
				return
			}
			transitionHash := sha256.Sum256(transitionData)
			payload.OwnerTransitionRoot = transitionData
			payload.ExpectedFinalRootSHA256 = hex.EncodeToString(transitionHash[:])
		}
		payloadJSON, marshalErr := json.Marshal(payload)
		if marshalErr != nil {
			writeError(w, http.StatusInternalServerError, marshalErr)
			return
		}
		profileHash := sha256.Sum256(profileData)
		integration, _ := integrations.Find(repo.Connector)
		var intent database.RepositoryConnectionIntent
		var reserveErr error
		if repo.Connector == "fs" && updatingExisting {
			intent, reserveErr = database.ReserveRepositoryUpdateWithStorage(db, *preview.ExistingRepository,
				repo.StorageIdentityVersion, repo.StorageIdentityKey, repo.StorageIdentityJSON, repo.Location,
				reviewedConnectorOptions(integration, repo.ConnectorOptions), preview.Mode, preview.Digest,
				string(payloadJSON), hex.EncodeToString(profileHash[:]), preview.NativeFingerprint, publicationOperationID)
		} else if repo.Connector == "fs" {
			intent, reserveErr = database.ReserveRepositoryConnectionWithStorage(db,
				repo.StorageIdentityVersion, repo.StorageIdentityKey, repo.StorageIdentityJSON, repo.Location,
				reviewedConnectorOptions(integration, repo.ConnectorOptions), preview.Mode, preview.Digest,
				string(payloadJSON), hex.EncodeToString(profileHash[:]), preview.NativeFingerprint, publicationOperationID)
		} else if updatingExisting {
			intent, reserveErr = database.ReserveRepositoryUpdate(db, *preview.ExistingRepository, repo.Connector, repo.Location,
				reviewedConnectorOptions(integration, repo.ConnectorOptions), preview.Mode, preview.Digest,
				string(payloadJSON), hex.EncodeToString(profileHash[:]), preview.NativeFingerprint, publicationOperationID)
		} else {
			intent, reserveErr = database.ReserveRepositoryConnection(db, repo.ID, repo.Connector, repo.Location,
				reviewedConnectorOptions(integration, repo.ConnectorOptions), preview.Mode, preview.Digest,
				string(payloadJSON), hex.EncodeToString(profileHash[:]), preview.NativeFingerprint, publicationOperationID)
		}
		recoveryNameReservationMu.Unlock()
		nameReservationLocked = false
		if reserveErr != nil {
			writeError(w, http.StatusConflict, reserveErr)
			return
		}
		finishConnection(w, r, db, auth, connectionAttachment{
			intent:              intent,
			payload:             payload,
			repo:                repo,
			nativeRepo:          nativeRepo,
			engine:              engine,
			profileHash:         profileHash,
			rcloneAuthSessionID: req.RcloneAuthSessionID,
			finishRcloneAuth:    finishRcloneAuth,
			releaseVault:        releaseVault,
		})
	}
}

// writeConnectionAttached answers a connection whose attachment transaction
// has committed. The vault is connected from here on, so a cleanup that did
// not finish (old Kopia config after an update, or the temporary cloud
// sign-in session) makes the connection completed with issues. It is still a
// 201 with the attached vault: an error status here would make the page treat
// a connected vault as a failed connection, and the intent it would retry is
// already gone.
func writeConnectionAttached(
	w http.ResponseWriter,
	response map[string]any,
	repo models.Repository,
	rcloneOutcome rcloneApplicationOutcome,
	kopiaCleanupErr, rcloneAuthCleanupErr error,
) {
	var outcome foregroundOutcome
	if kopiaCleanupErr != nil {
		outcome.addIssue("The vault update is active, but old Kopia configuration cleanup remains pending.")
	}
	if rcloneAuthCleanupErr != nil {
		outcome.addIssue("The vault is connected, but its temporary native authorization session still needs cleanup.")
	}
	outcome.addTo(response)
	if engines.IsRcloneNativeLoginProvider(repo.Connector) {
		addRcloneOutcome(response, rcloneOutcome)
	}
	writeJSONStatus(w, http.StatusCreated, response)
}

// detectedVaultRejectedPasswordFormat is the error for a detected vault that
// fails native validation, both at the connect Check and when a pending
// connection resumes. The connect page in frontend/src/pages/Protect.tsx
// matches the resulting text exactly to offer the rclone sign-in again, so
// don't change it without updating Protect.tsx.
const detectedVaultRejectedPasswordFormat = "the detected %s vault rejected the vault encryption password or could not be validated"

func resumeRepositoryConnection(
	w http.ResponseWriter,
	r *http.Request,
	db *sql.DB,
	auth *rcloneAuthStore,
	intent database.RepositoryConnectionIntent,
	storage ExistingVaultStorage,
	finishRcloneAuth func(engines.RcloneConfigDisposition) error,
	releaseVaultCallbacks ...func(),
) {
	releaseVault := func() {}
	if len(releaseVaultCallbacks) == 1 && releaseVaultCallbacks[0] != nil {
		releaseVault = releaseVaultCallbacks[0]
	}
	var payload connectionIntentPayload
	if err := decodeConnectionIntentPayload(intent.PayloadJSON, &payload); err != nil {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("pending connection state is invalid"))
		return
	}
	if err := restoreConnectionIntentSourceBindings(&payload); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	authoritativeStorage := ExistingVaultStorage{
		Connector: intent.Connector, Location: intent.Location, Password: storage.Password,
		Options: map[string]string{}, RcloneAuthSessionID: storage.RcloneAuthSessionID,
		RcloneConfigPath: storage.RcloneConfigPath,
	}
	for key, value := range storage.Options {
		authoritativeStorage.Options[key] = value
	}
	for key, value := range intent.ReviewedOptions {
		authoritativeStorage.Options[key] = value
	}
	storage = authoritativeStorage
	repo := payload.Repository.repository(storage)
	if engines.IsRcloneNativeLoginProvider(repo.Connector) && storage.RcloneAuthSessionID == "" {
		// The retry handler has already validated the saved vault config. Use
		// its repository-ID binding rather than treating that canonical file
		// as an authorization-session candidate.
		repo.RcloneConfigPath = ""
	}
	repo.CanonicalIdentity = payload.PhysicalVaultIdentity
	repo.StorageIdentityVersion = payload.StorageIdentityVersion
	repo.StorageIdentityKey = payload.StorageIdentityKey
	repo.StorageIdentityJSON = payload.StorageIdentityJSON
	_, err := bindExistingRepositoryStorage(r.Context(), repo)
	if err != nil || repo.ID != intent.CanonicalIdentity {
		// A retry can lose storage between its first bind and this locked bind.
		// The public mismatch response stays fixed; only the typed fact is exported.
		markSupportStorageObservation(w, "vault_connect_destination", err)
		writeError(w, http.StatusConflict, fmt.Errorf("pending connection does not match the reviewed vault selection"))
		return
	}
	// The durable intent fixes the reviewed location and repository tuple, not
	// the replaceable disk beneath that location. The fresh bind validates the
	// selected path; native/root/attachment proof remains repository authority.
	profile, err := vaultprofile.Parse(payload.Profile)
	if err != nil || profile.VaultUUID != repo.ID || profile.ProfileUUID != repo.ProfileUUID ||
		profile.Attachment.ClientUUID != repo.ClientUUID || profile.Attachment.Generation != repo.AttachmentGeneration {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("pending connection profile is invalid"))
		return
	}
	if payload.PublishRoot {
		root, rootErr := vaultprofile.ParseRoot(payload.Root, repo.Connector)
		if rootErr != nil || !root.Identifies(repo) {
			writeError(w, http.StatusInternalServerError, fmt.Errorf("pending connection root is invalid"))
			return
		}
	}
	profileHash := sha256.Sum256(payload.Profile)
	if hex.EncodeToString(profileHash[:]) != intent.ProfileSHA256 {
		writeError(w, http.StatusInternalServerError, fmt.Errorf("pending connection profile checksum is invalid"))
		return
	}
	nativeRepo, err := recoveryEngineRepository(repo)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	validationRepo := nativeRepo
	if repo.Engine == engines.KopiaID ||
		(repo.Engine == engines.ResticID && !(engines.IsRcloneNativeLoginProvider(repo.Connector) && repo.RcloneConfigPath == "")) {
		// Disposable validation IDs isolate preview engine artifacts. A saved
		// rclone config is bound to the real vault UUID, so changing that ID
		// would make native Restic read a nonexistent config. Never schedule
		// preview cleanup for the real vault's engine artifacts either.
		validationRepo.ID = uuid.NewString()
		defer engines.CleanupPreviewArtifacts(validationRepo)
	}
	engine, err := resolveEngine(validationRepo)
	var validationOutput string
	if err == nil {
		reportVaultProgress(r.Context(), "Validating the saved native repository...")
		validationOutput, err = engines.ValidateRepository(r.Context(), engine, validationRepo)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, fmt.Errorf(detectedVaultRejectedPasswordFormat, repo.Engine))
		return
	}
	nativeFingerprint, err := fingerprintExistingRepository(r.Context(), validationRepo, validationOutput)
	if err != nil || nativeFingerprint != intent.NativeFingerprint {
		writeError(w, http.StatusConflict, fmt.Errorf("the native vault changed after review; attachment was stopped"))
		return
	}
	if repo.Engine == engines.KopiaID {
		// The saved intent doesn't keep the error correction overhead, so a
		// retried connection reads it from the status this validation read,
		// as a first Connect does.
		repo.ECCOverheadPercent, err = engines.KopiaReportedECCOverheadPercent(validationOutput)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Errorf("read native repository error correction: %w", err))
			return
		}
	}
	if rootErr := verifyPendingConnectionRoot(r.Context(), repo, payload, intent.State == "attachment_pending"); rootErr != nil {
		writeError(w, http.StatusConflict, rootErr)
		return
	}
	// Retry is another attachment admission boundary. Rescan every bounded
	// profile before continuing so a copied or concurrently published local
	// attachment cannot bypass the final-request full-set check.
	if intent.Mode == "profile" || intent.Mode == "fallback" {
		profileChoices, profileScanErr := scanConnectionProfileAttachments(r.Context(), repo)
		if profileScanErr != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("rescan vault profiles for pending connection: %w", profileScanErr))
			return
		}
		if profileSetErr := validatePendingConnectionProfileAttachments(profileChoices, repo, payload); profileSetErr != nil {
			writeError(w, http.StatusConflict, profileSetErr)
			return
		}
	}
	snapshots := []models.Snapshot{}
	if intent.Mode == "fallback" {
		snapshots, _, err = engine.ListSnapshots(r.Context(), validationRepo)
		if err != nil {
			writeError(w, http.StatusBadRequest, fmt.Errorf("revalidate reviewed snapshots: %w", err))
			return
		}
	}
	if intent.Mode == "fallback" {
		// Use the same fallback classifier as preview so valid foreign profiles
		// cannot influence reviewed source roots, synthesized jobs, or cache state.
		snapshots = recoveryVisibleSnapshots("fallback", "", nil, snapshots)
		freshSources, sourceErr := unmanagedImportSources(snapshots)
		if sourceErr != nil {
			writeError(w, http.StatusConflict, sourceErr)
			return
		}
		freshRoots := make([]string, 0, len(freshSources))
		for _, source := range freshSources {
			freshRoots = append(freshRoots, recoverySourceKey(source))
		}
		sort.Strings(freshRoots)
		// A vault reviewed with no sources saves its roots as JSON null, which
		// decodes to nil, while freshRoots is empty but not nil. slices.Equal
		// treats the two as equal; reflect.DeepEqual doesn't, and would refuse
		// every retry of such a connection.
		if !slices.Equal(freshRoots, payload.ReviewedSourceRoots) {
			writeError(w, http.StatusConflict, fmt.Errorf("the native source set changed after review; check the vault again"))
			return
		}
	}
	if payload.UpdateExistingVault {
		if payload.ExistingRepository.ID != repo.ID || payload.ExistingRepository.ProfileUUID != repo.ProfileUUID ||
			payload.ExistingRepository.AttachmentGeneration != repo.AttachmentGeneration {
			writeError(w, http.StatusConflict, fmt.Errorf("pending Update existing vault state is invalid"))
			return
		}
	} else {
		if duplicateErr := database.AssertVaultUUIDAvailable(db, repo.ID); duplicateErr != nil {
			writeError(w, http.StatusConflict, duplicateErr)
			return
		}
		if err := database.ValidateRecoveredRepositoryAttachment(db, repo, payload.Jobs); err != nil {
			writeError(w, http.StatusConflict, err)
			return
		}
	}
	finishConnection(w, r, db, auth, connectionAttachment{
		resumed:             true,
		intent:              intent,
		payload:             payload,
		repo:                repo,
		nativeRepo:          nativeRepo,
		engine:              engine,
		profileHash:         profileHash,
		rcloneAuthSessionID: storage.RcloneAuthSessionID,
		finishRcloneAuth:    finishRcloneAuth,
		releaseVault:        releaseVault,
	})
}

// connectionAttachment is what finishConnection needs to publish and attach a
// connection whose intent is saved. A first Connect and a retry of a pending
// connection share that work; resumed marks the retry, which differs in the
// places finishConnection points out.
type connectionAttachment struct {
	resumed     bool
	intent      database.RepositoryConnectionIntent
	payload     connectionIntentPayload
	repo        models.Repository
	nativeRepo  models.Repository
	engine      engines.Engine
	profileHash [sha256.Size]byte
	// rcloneAuthSessionID is set when a sign-in provider's config still has to
	// move from that session to the vault. For a sign-in provider, an empty
	// value means the vault's config is already published: a first Connect of
	// a sign-in provider can't get here without a session, and a retry without
	// one reuses the saved config.
	rcloneAuthSessionID string
	finishRcloneAuth    func(engines.RcloneConfigDisposition) error
	releaseVault        func()
}

func finishConnection(w http.ResponseWriter, r *http.Request, db *sql.DB, auth *rcloneAuthStore, attachment connectionAttachment) {
	intent, payload, repo := attachment.intent, attachment.payload, attachment.repo
	nativeRepo, engine, profileHash := attachment.nativeRepo, attachment.engine, attachment.profileHash
	finishRcloneAuth := attachment.finishRcloneAuth
	// The pending-state texts are kept on the intent; a retry words them
	// slightly differently.
	retryAdvice := "retry this connection with the same credentials."
	if attachment.resumed {
		retryAdvice = "retry with the same credentials."
	}
	if err := database.StageRecoveredLocalJobAdmissions(
		db, intent.ID, payload.LocalJobAdmissions,
	); err != nil {
		writeError(w, http.StatusConflict, err)
		return
	}
	// Local compatibility has been fully checked before publication. A retry
	// finds the same saved intent and carries on from where it stopped.
	if profileErr := ensureConnectionProfilePublished(r.Context(), db, repo, &intent, &payload); profileErr != nil {
		_ = database.MarkRepositoryConnectionIntent(db, intent.ID, "publication_started", "Recovery profile publication is pending; "+retryAdvice)
		writeError(w, http.StatusBadGateway, profileErr)
		return
	}
	if payload.OwnerTransferFromProfileUUID != "" && intent.State == "prepared" {
		if stateErr := database.MarkRepositoryConnectionIntent(db, intent.ID, "publication_started", "Vault owner transition is in progress; retry this connection."); stateErr != nil {
			writeError(w, http.StatusInternalServerError, stateErr)
			return
		}
		intent.State = "publication_started"
	}
	ownerTransferComplete, ownerTransitionErr := connectionOwnerTransitionState(
		r.Context(), repo, intent, payload,
	)
	if ownerTransitionErr != nil {
		_ = database.MarkRepositoryConnectionIntent(db, intent.ID, "publication_started", "Vault owner transition is pending; "+retryAdvice)
		writeError(w, http.StatusConflict, ownerTransitionErr)
		return
	}
	var kopiaUpdateIntent database.KopiaFilesystemReconnectIntent
	if repo.Engine == engines.KopiaID {
		if intent.State == "prepared" {
			if stateErr := database.MarkRepositoryConnectionIntent(db, intent.ID, "publication_started", "Kopia client configuration is being activated; retry this connection."); stateErr != nil {
				writeError(w, http.StatusInternalServerError, stateErr)
				return
			}
			intent.State = "publication_started"
		}
		if payload.UpdateExistingVault {
			// The update reservation checked that the saved row still matches
			// the reviewed one and blocks edits to it until the connection ends,
			// so a first Connect and a retry both stage against the row as saved.
			saved, loadErr := database.GetRepository(db, repo.ID)
			if loadErr != nil {
				writeError(w, http.StatusConflict, fmt.Errorf("reload saved vault for Kopia connection update: %w", loadErr))
				return
			}
			var stageErr error
			kopiaUpdateIntent, stageErr = stageKopiaConnectionUpdate(r.Context(), db, engine, saved, nativeRepo)
			if stageErr != nil {
				writeError(w, http.StatusConflict, fmt.Errorf("stage Kopia connection update: %w", stageErr))
				return
			}
		} else if _, identityErr := ensureConnectionKopiaClientIdentity(r.Context(), engine, nativeRepo); identityErr != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("establish Kopia client identity: %w", identityErr))
			return
		}
		ownerRepo := nativeRepo
		ownerRepo.ClientUUID = payload.NativeOwnerClientUUID
		if ownerRepo.ClientUUID == "" {
			writeError(w, http.StatusConflict, fmt.Errorf("the Kopia maintenance owner profile is unavailable"))
			return
		}
		objectLockEnrolled := false
		if intent.Mode == "fallback" && ownerRepo.ObjectLock.Enrolled {
			mutationContext := connectionMaintenanceMutationContext(
				r.Context(), db, repo, intent, payload, ownerTransferComplete,
			)
			if _, enrollmentErr := engines.ConfigureKopiaObjectLockForEnrollment(mutationContext, engine, ownerRepo); enrollmentErr != nil {
				writeError(w, http.StatusConflict, explainObjectLockProviderRequirement(ownerRepo.Connector, enrollmentErr))
				return
			}
			objectLockEnrolled = true
		}
		alignNativeOwner := intent.Mode == "fallback" || payload.OwnerTransferFromProfileUUID != "" ||
			(payload.TakeoverFromClientUUID != "" && repo.IsVaultOwner)
		if alignNativeOwner && !objectLockEnrolled {
			mutationContext := connectionMaintenanceMutationContext(
				r.Context(), db, repo, intent, payload, ownerTransferComplete,
			)
			if _, ownerErr := ensureConnectionKopiaMaintenanceOwner(mutationContext, engine, ownerRepo); ownerErr != nil {
				writeError(w, http.StatusConflict, fmt.Errorf("align Kopia maintenance owner: %w", ownerErr))
				return
			}
		} else if ownerErr := engines.VerifyKopiaMaintenanceOwner(r.Context(), engine, ownerRepo); ownerErr != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("verify Kopia maintenance owner: %w", ownerErr))
			return
		}
	}
	// A retry whose intent already reached attachment_pending published its
	// root before, so it isn't published again. The root check before
	// attachment below re-reads it only for the sign-in providers (Dropbox,
	// Google Drive, OneDrive); other connectors rely on the intent's state.
	rootAlreadyPublished := attachment.resumed && intent.State == "attachment_pending"
	if payload.PublishRoot && !ownerTransferComplete && !rootAlreadyPublished {
		if intent.State == "prepared" {
			if stateErr := database.MarkRepositoryConnectionIntent(db, intent.ID, "publication_started", "Vault root publication is in progress; retry this connection."); stateErr != nil {
				writeError(w, http.StatusInternalServerError, stateErr)
				return
			}
			intent.State = "publication_started"
		}
		if err := publishRecoveryRoot(r.Context(), repo, payload.Root, connectionFinalRootPublishOptions(intent, payload)); err != nil {
			_ = database.MarkRepositoryConnectionIntent(db, intent.ID, "publication_started", "Vault root publication is pending; "+retryAdvice)
			writeError(w, http.StatusBadGateway, fmt.Errorf("could not publish the mandatory vault root: %w", err))
			return
		}
	}
	if attachment.resumed {
		// A retry checks the published profile once more here, before the
		// source check and the rclone config activation.
		remoteProfile, readErr := readRecoveryProfile(r.Context(), repo)
		if readErr != nil {
			writeError(w, http.StatusBadGateway, fmt.Errorf("read authoritative recovery profile: %w", readErr))
			return
		}
		if profileErr := verifyCanonicalConnectionProfile(remoteProfile, repo, profileHash); profileErr != nil {
			writeError(w, http.StatusConflict, fmt.Errorf("the vault recovery profile differs from the pending connection; attachment was stopped: %w", profileErr))
			return
		}
	}
	if availabilityErr := requireRecoveredJobSourcesAvailable(r.Context(), payload.Jobs); availabilityErr != nil {
		writeError(w, http.StatusConflict, availabilityErr)
		return
	}
	rcloneOutcome := rcloneApplicationOutcome{
		Activation: engines.RcloneConfigActivation{Disposition: engines.RcloneConfigRetained},
	}
	publishRcloneConfig := engines.IsRcloneNativeLoginProvider(repo.Connector) &&
		strings.TrimSpace(attachment.rcloneAuthSessionID) != ""
	if engines.IsRcloneNativeLoginProvider(repo.Connector) && !publishRcloneConfig {
		rcloneOutcome.Activation.Disposition = engines.RcloneConfigActivated
	}
	if publishRcloneConfig {
		if intent.State == "prepared" {
			if stateErr := database.MarkRepositoryConnectionIntent(db, intent.ID, "publication_started", "Native rclone configuration is being activated; retry this connection."); stateErr != nil {
				writeError(w, http.StatusInternalServerError, stateErr)
				return
			}
			intent.State = "publication_started"
		}
		activation, publishErr := auth.publishConfig(
			r.Context(), attachment.rcloneAuthSessionID, repo.Connector, repo.ID,
		)
		rcloneOutcome.Activation = activation
		publishErr = errors.Join(publishErr, finishRcloneAuth(activation.Disposition))
		if publishErr != nil {
			_ = database.MarkRepositoryConnectionIntent(
				db, intent.ID, "publication_started",
				"The native rclone configuration could not be published; retry this connection.",
			)
			writeRcloneApplicationError(w, http.StatusInternalServerError, rcloneOutcome,
				fmt.Errorf("publish native rclone vault config: %w", publishErr))
			return
		}
		// Publication moved the candidate out of its authorization session.
		// Every later read, including the profile readback just below, and the
		// attached repository use the canonical vault-owned config instead of
		// the now-absent staged path.
		repo.RcloneConfigPath = ""
	}
	if profileErr := verifyConnectionProfileBeforeAttachment(r.Context(), repo, profileHash); profileErr != nil {
		if rcloneOutcome.Activation.ConsumesAuthorization() {
			writeRcloneApplicationError(w, http.StatusConflict, rcloneOutcome,
				fmt.Errorf("native rclone config activated but vault profile admission failed: %w", profileErr))
			return
		}
		writeError(w, http.StatusConflict, profileErr)
		return
	}
	if attachment.resumed {
		// Recheck the reviewed or newly published root at the attachment
		// boundary. A retry may run on a sign-in config that an earlier
		// attempt published, and native validation, profile publication and
		// config activation may have taken time since the retry's first root
		// check. A first Connect skips this: it reviewed the root earlier in
		// this same request, through the config it has just published.
		if rootErr := verifyPendingConnectionRoot(r.Context(), repo, payload, true); rootErr != nil {
			if rcloneOutcome.Activation.ConsumesAuthorization() {
				writeRcloneApplicationError(w, http.StatusConflict, rcloneOutcome,
					fmt.Errorf("native rclone config activated but vault root admission failed: %w", rootErr))
				return
			}
			writeError(w, http.StatusConflict, rootErr)
			return
		}
	}
	if intent.State != "prepared" {
		if stateErr := database.MarkRepositoryConnectionIntent(db, intent.ID, "attachment_pending", "External publication and configuration are complete; local attachment is pending."); stateErr != nil {
			if rcloneOutcome.Activation.ConsumesAuthorization() {
				writeRcloneApplicationError(w, http.StatusInternalServerError, rcloneOutcome,
					fmt.Errorf("native rclone config activated but connection state could not be saved: %w", stateErr))
				return
			}
			writeError(w, http.StatusInternalServerError, stateErr)
			return
		}
	}
	if attachment.resumed {
		reportVaultProgress(r.Context(), "Finishing this computer's vault attachment...")
	}
	var id string
	var affectedRepositories []string
	var err error
	if payload.UpdateExistingVault {
		id, affectedRepositories, err = database.ReconnectRecoveredRepository(
			db, repo, payload.Jobs, payload.Dormant, intent.ID,
		)
	} else {
		id, affectedRepositories, err = database.AttachRecoveredRepository(
			db, repo, payload.Jobs, payload.Dormant, intent.ID,
		)
	}
	if err != nil {
		if rcloneOutcome.Activation.ConsumesAuthorization() {
			writeRcloneApplicationError(w, http.StatusInternalServerError, rcloneOutcome,
				fmt.Errorf("native rclone config activated but vault attachment failed: %w", err))
			return
		}
		// A retry reports every attachment refusal as a conflict with the
		// pending connection.
		if attachment.resumed || errors.Is(err, database.ErrRepositoryIdentityExists) {
			writeError(w, http.StatusConflict, err)
		} else {
			writeError(w, http.StatusBadRequest, err)
		}
		return
	}
	if engines.IsRcloneNativeLoginProvider(repo.Connector) {
		rcloneOutcome = attachedUsableRcloneOutcome(rcloneOutcome)
	}
	if repo.Engine == engines.KopiaID {
		// Policy convergence intentionally runs through the one post-attachment
		// reconciler; its dirty state keeps backup and retention fail-closed.
		kopiapolicy.QueueDirty(db, id)
	}
	var kopiaCleanupErr error
	if payload.UpdateExistingVault && repo.Engine == engines.KopiaID {
		kopiaCleanupErr = cleanupKopiaConnectionUpdate(r.Context(), db, engine, nativeRepo, kopiaUpdateIntent)
	}
	rcloneAuthCleanupErr := finishRcloneAuth(engines.RcloneConfigActivated)
	queueProfilesAfterConnectionAttachment(db, attachment.releaseVault)
	initiateConnectedVaultRefreshes(db, id)
	response := map[string]any{"id": id, "name": repo.Name, "profilePending": len(affectedRepositories) > 0}
	if attachment.resumed {
		response["resumed"] = true
	}
	if len(affectedRepositories) > 0 {
		response["warning"] = "Vault profiles will be synchronized in the background. You can start using Replicaro."
	}
	writeConnectionAttached(w, response, repo, rcloneOutcome, kopiaCleanupErr, rcloneAuthCleanupErr)
}

func queueProfilesAfterConnectionAttachment(db *sql.DB, releaseAttachedVault func()) {
	// Attachment already marks every affected profile dirty in its transaction.
	// Release the foreground vault before waking the existing durable worker;
	// that worker retains its normal exclusive-lock and priority contract.
	releaseAttachedVault()
	profilesync.Wake(db)
}
