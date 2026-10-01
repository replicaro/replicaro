package profilesync

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/repositoryadmission"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultprofile"
	"github.com/local/replicaro/vaultreconnect"
)

func publishProfileDefault(ctx context.Context, repo models.Repository, data []byte, options vaultprofile.PublishOptions) error {
	return (vaultprofile.Store{Repository: repo}).ForProfile(repo.ProfileUUID).PublishUnderLock(ctx, data, options)
}

func readProfileDefault(ctx context.Context, repo models.Repository) (vaultprofile.ReadResult, error) {
	return (vaultprofile.Store{Repository: repo}).ForProfile(repo.ProfileUUID).ReadDetailed(ctx)
}

func publishSyncedProfileDefault(ctx context.Context, repo models.Repository, data []byte, options vaultprofile.PublishOptions) error {
	return (vaultprofile.Store{Repository: repo}).
		WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).
		ForProfile(repo.ProfileUUID).
		PublishUnderLock(ctx, data, options)
}

func readSyncedProfileDefault(ctx context.Context, repo models.Repository) (vaultprofile.ReadResult, error) {
	return (vaultprofile.Store{Repository: repo}).
		WithRepositoryAvailabilityCheck(storageavailability.RequireRepositoryAvailable).
		ForProfile(repo.ProfileUUID).
		ReadDetailed(ctx)
}

var publishProfile = publishProfileDefault
var readProfile = readProfileDefault
var publishSyncedProfile = publishSyncedProfileDefault
var readSyncedProfile = readSyncedProfileDefault
var admitRepository = repositoryadmission.AdmitUnderLock

func SetRepositoryAdmissionForTests(next func(context.Context, *sql.DB, models.Repository) (models.Repository, error)) func() {
	previous := admitRepository
	admitRepository = next
	return func() { admitRepository = previous }
}

var repositorySyncLocks sync.Map
var workerWakeups sync.Map

const maxConcurrentProfileSyncs = 4

func repositorySyncLock(id string) *sync.Mutex {
	value, _ := repositorySyncLocks.LoadOrStore(id, &sync.Mutex{})
	return value.(*sync.Mutex)
}

func SetReplaceForTests(next func(context.Context, models.Repository, []byte) error) func() {
	previousPublish, previousRead := publishProfile, readProfile
	previousSyncedPublish, previousSyncedRead := publishSyncedProfile, readSyncedProfile
	if next == nil {
		publishProfile, readProfile = publishProfileDefault, readProfileDefault
		publishSyncedProfile, readSyncedProfile =
			publishSyncedProfileDefault, readSyncedProfileDefault
	} else {
		publishProfile = func(ctx context.Context, repo models.Repository, data []byte, _ vaultprofile.PublishOptions) error {
			return next(ctx, repo, data)
		}
		publishSyncedProfile = publishProfile
		readProfile = func(context.Context, models.Repository) (vaultprofile.ReadResult, error) {
			return vaultprofile.ReadResult{}, os.ErrNotExist
		}
		readSyncedProfile = readProfile
	}
	return func() {
		publishProfile, readProfile = previousPublish, previousRead
		publishSyncedProfile, readSyncedProfile = previousSyncedPublish, previousSyncedRead
	}
}

func profileBytes(repo models.Repository, jobs []models.BackupJob, dormant []vaultprofile.BackupJob, revision int64, previousUpdatedAt time.Time) ([]byte, error) {
	return profileBytesPreserving(repo, jobs, dormant, revision, previousUpdatedAt, nil)
}

func buildProfilePreserving(repo models.Repository, jobs []models.BackupJob, dormant []vaultprofile.BackupJob, revision int64, previousUpdatedAt time.Time, current *vaultprofile.Profile) (vaultprofile.Profile, error) {
	now := time.Now().UTC()
	if repo.CreatedAt.IsZero() {
		repo.CreatedAt = now
	}
	if revision == 1 && previousUpdatedAt.IsZero() {
		now = repo.CreatedAt.UTC()
	}
	computerName, _ := os.Hostname()
	profile, err := vaultprofile.BuildProfile(repo, jobs, dormant, repo.ProfileUUID, repo.ClientUUID,
		repo.AttachmentGeneration, revision, vaultprofile.AttachmentDisplay{
			ComputerName: computerName, OperatingSystem: runtime.GOOS,
		}, previousUpdatedAt, now)
	if err != nil {
		return vaultprofile.Profile{}, err
	}
	if current != nil {
		// Ordinary synchronization updates definitions/preferences only. Stable
		// creation and attachment facts change exclusively during an actual
		// attachment transition.
		profile.CreatedAt = current.CreatedAt
		profile.Attachment.AttachedAt = current.Attachment.AttachedAt
		profile.Attachment.Display = current.Attachment.Display
		if err := profile.Validate(); err != nil {
			return vaultprofile.Profile{}, err
		}
	}
	return profile, nil
}

func profileBytesPreserving(repo models.Repository, jobs []models.BackupJob, dormant []vaultprofile.BackupJob, revision int64, previousUpdatedAt time.Time, current *vaultprofile.Profile) ([]byte, error) {
	profile, err := buildProfilePreserving(repo, jobs, dormant, revision, previousUpdatedAt, current)
	if err != nil {
		return nil, err
	}
	return vaultprofile.Marshal(profile)
}

// ProjectRepositoryProfileSize builds the next canonical profile entirely
// from local authoritative definitions and the best stable attachment facts
// already available locally. It performs no provider, rclone, or publication
// work and returns at most the profile limit plus one byte.
func ProjectRepositoryProfileSize(db *sql.DB, repositoryID string) (int, error) {
	repo, err := database.GetRepository(db, repositoryID)
	if err != nil {
		return 0, err
	}
	jobs, err := database.ListJobsForRepository(db, repositoryID)
	if err != nil {
		return 0, err
	}
	dormantRows, err := database.ListDormantRecoveryJobs(db, repositoryID)
	if err != nil {
		return 0, err
	}
	dormant := make([]vaultprofile.BackupJob, 0, len(dormantRows))
	for _, row := range dormantRows {
		var definition vaultprofile.BackupJob
		if err := json.Unmarshal([]byte(row.DefinitionJSON), &definition); err != nil || definition.JobUUID != row.JobID {
			return 0, fmt.Errorf("decode dormant recovery job %s", row.JobID)
		}
		dormant = append(dormant, definition)
	}
	revision := int64(1)
	previousUpdatedAt := time.Time{}
	var current *vaultprofile.Profile
	if state, stateErr := database.VaultProfileSyncState(db, repositoryID); stateErr == nil && state.ProfileJSON != "" {
		if profile, parseErr := vaultprofile.Parse([]byte(state.ProfileJSON)); parseErr == nil &&
			profile.ProfileUUID == repo.ProfileUUID && profile.Attachment.ClientUUID == repo.ClientUUID &&
			profile.Attachment.Generation == repo.AttachmentGeneration {
			current = &profile
			revision = profile.Revision + 1
			previousUpdatedAt = profile.UpdatedAt
		}
	} else if stateErr != nil && !errors.Is(stateErr, sql.ErrNoRows) {
		return 0, stateErr
	}
	profile, err := buildProfilePreserving(repo, jobs, dormant, revision, previousUpdatedAt, current)
	if err != nil {
		return 0, err
	}
	return vaultprofile.EncodedProfileSize(profile, vaultprofile.MaximumProfileDecryptedSize)
}

// PublishInitial writes and verifies a profile before a newly-created vault is
// reported as attached to the local database.
func PublishInitial(ctx context.Context, repo models.Repository) error {
	return Publish(ctx, repo, []models.BackupJob{})
}

func Publish(ctx context.Context, repo models.Repository, jobs []models.BackupJob) error {
	data, err := profileBytes(repo, jobs, nil, 1, time.Time{})
	if err != nil {
		return err
	}
	return (vaultprofile.Store{Repository: repo}).ForProfile(repo.ProfileUUID).Publish(ctx, data, vaultprofile.PublishOptions{OperationID: uuid.NewString()})
}

func PublishNew(ctx context.Context, repo models.Repository, jobs []models.BackupJob) error {
	return PublishNewWithDormant(ctx, repo, jobs, nil)
}

func PublishNewWithDormant(ctx context.Context, repo models.Repository, jobs []models.BackupJob, dormant []vaultprofile.BackupJob) error {
	return PublishNewOperation(ctx, repo, jobs, dormant, uuid.NewString())
}

func PublishNewOperation(ctx context.Context, repo models.Repository, jobs []models.BackupJob, dormant []vaultprofile.BackupJob, operationID string) error {
	data, err := BuildInitialProfile(repo, jobs, dormant)
	if err != nil {
		return err
	}
	return (vaultprofile.Store{Repository: repo}).ForProfile(repo.ProfileUUID).Publish(ctx, data, vaultprofile.PublishOptions{OperationID: operationID, CreateOnly: true})
}

func BuildInitialRoot(repo models.Repository) ([]byte, error) {
	root, err := vaultprofile.BuildRoot(repo, repo.NativeRepositoryID, repo.ProfileUUID, 1, time.Time{}, repo.CreatedAt)
	if err != nil {
		return nil, err
	}
	return vaultprofile.MarshalRoot(root, repo.Connector)
}

func BuildInitialProfile(repo models.Repository, jobs []models.BackupJob, dormant []vaultprofile.BackupJob) ([]byte, error) {
	return profileBytes(repo, jobs, dormant, 1, time.Time{})
}

// SyncRepository is one profile sync attempt for a vault that takes the vault
// lock itself: the queue worker's, and the same sync run right after another
// vault's connection. Its result also drives the vault's saved reconnect
// state (package vaultreconnect), recorded while the vault lock is still held
// so that a reconnect cannot commit between this attempt and its
// bookkeeping. Callers that publish under a lock they already hold
// (creation, connection, removal, password change) use
// SyncRepositoryUnderLock; they report their own failures, which do not count
// toward the reconnect clock, but their successes are recorded like the
// worker's own (see SyncRepositoryUnderLock).
func SyncRepository(ctx context.Context, db *sql.DB, repositoryID string) error {
	return syncRepositoryAttempt(ctx, db, repositoryID, vaultreconnect.Now())
}

// syncRepositoryAttempt is SyncRepository for an attempt that started at
// startedAt. The start is taken before waiting for the vault lock (and, in a
// queue pass, before waiting for a free slot): the reconnect clock measures
// the gap between failures to it, and a long backup holding the lock must not
// look like the app not retrying.
func syncRepositoryAttempt(ctx context.Context, db *sql.DB, repositoryID string, startedAt time.Time) error {
	unlock, err := vaultlock.AcquireExclusiveContext(ctx, repositoryID)
	if err != nil {
		return fmt.Errorf("wait for vault profile publication lock: %w", err)
	}
	defer unlock()
	// Only an Any Rclone Remote vault gets a note from this check: the result
	// of checking its rclone settings once after the failure. When the attempt
	// saves its failure, the note is saved with it, in the same write, so it
	// replaces the previous attempt's note and never lands on a newer
	// revision. A failure that returns before anything is saved (admission,
	// for example) only gets the note in its returned error.
	checked := false
	checkAfterFailure := func() string {
		checked = true
		return vaultreconnect.CheckRcloneRemoteAfterFailure(db, repositoryID, vaultreconnect.WorkerProfileSync)
	}
	err = syncRepositoryUnderLock(ctx, db, repositoryID, checkAfterFailure)
	switch {
	case err == nil:
		vaultreconnect.RecordWorkerSuccess(db, repositoryID, vaultreconnect.WorkerProfileSync)
	case errors.Is(err, database.ErrVaultProfilePending):
		// A local change overtook this attempt before anything was published.
		// That is neither a success nor a failure of the vault; the queue
		// simply picks up the newer revision.
	case errors.Is(err, context.Canceled):
		// Shutdown or the worker being stopped, not a failure of the vault.
		// RecordWorkerFailure ignores it as well.
	default:
		if !checked {
			if note := checkAfterFailure(); note != "" {
				err = fmt.Errorf("%w\n%s", err, note)
			}
		}
		vaultreconnect.RecordWorkerFailure(db, repositoryID, vaultreconnect.WorkerProfileSync, startedAt, err)
	}
	return err
}

// SyncRepositoryUnderLock publishes while the caller holds this repository's
// exclusive managed vault UUID lock. Creation and connection transactions use it
// to avoid re-entering the process-wide vault lock.
//
// A successful publication here is the same fact as a successful queue
// attempt: the pending profile reached the vault. It is recorded as a profile
// sync success so the worker's failure clock and any reconnect state the
// worker set are cleared. Otherwise a vault password change that publishes
// the pending profile would leave the queue with nothing to retry, the clock
// would keep its old failing-since time, and the next run of failures would be
// measured from it and could escalate early. The caller still holds the vault
// lock, so this is recorded before a reconnect could commit. Failures are not
// recorded: the caller reports them as its own result.
func SyncRepositoryUnderLock(ctx context.Context, db *sql.DB, repositoryID string) error {
	err := syncRepositoryUnderLock(ctx, db, repositoryID, nil)
	if err == nil {
		vaultreconnect.RecordWorkerSuccess(db, repositoryID, vaultreconnect.WorkerProfileSync)
	}
	return err
}

// syncRepositoryUnderLock publishes the pending profile. When it saves a
// failure, the line checkAfterFailure returns, if any, is saved with it and
// added to the returned error. Callers that report their own failures pass
// nil.
func syncRepositoryUnderLock(ctx context.Context, db *sql.DB, repositoryID string, checkAfterFailure func() string) error {
	ctx = vaultprofile.WithTimingReporter(ctx, func(label string, elapsed time.Duration) {
		log.Printf("vault profile sync %s: %s: %s", repositoryID, label, elapsed.Round(time.Microsecond))
	})
	lock := repositorySyncLock(repositoryID)
	lock.Lock()
	defer lock.Unlock()
	state, stateErr := database.VaultProfileSyncState(db, repositoryID)
	if stateErr != nil && !errors.Is(stateErr, sql.ErrNoRows) {
		return stateErr
	}
	repo, err := database.GetRepository(db, repositoryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) && stateErr == nil {
			return database.CompleteVaultProfileSync(db, repositoryID, state.Revision)
		}
		return err
	}
	repo, err = admitRepository(ctx, db, repo)
	if err != nil {
		return fmt.Errorf("admit persisted vault profile publication: %w", err)
	}
	jobs, err := database.ListJobsForRepository(db, repositoryID)
	if err != nil {
		return err
	}
	dormantRows, err := database.ListDormantRecoveryJobs(db, repositoryID)
	if err != nil {
		return err
	}
	dormant := make([]vaultprofile.BackupJob, 0, len(dormantRows))
	for _, row := range dormantRows {
		var definition vaultprofile.BackupJob
		if err := json.Unmarshal([]byte(row.DefinitionJSON), &definition); err != nil || definition.JobUUID != row.JobID {
			return fmt.Errorf("decode dormant recovery job %s", row.JobID)
		}
		dormant = append(dormant, definition)
	}
	var data []byte
	options := vaultprofile.PublishOptions{}
	if stateErr == nil && state.ProfileJSON != "" {
		data = []byte(state.ProfileJSON)
		hash := sha256.Sum256(data)
		if fmt.Sprintf("%x", hash[:]) != state.ProfileSHA256 {
			err = fmt.Errorf("pending vault profile checksum is invalid")
		}
		if profile, parseErr := vaultprofile.Parse(data); parseErr != nil {
			err = fmt.Errorf("pending vault profile is invalid")
		} else if profile.ProfileUUID != repo.ProfileUUID ||
			profile.Attachment.ClientUUID != repo.ClientUUID || profile.Attachment.Generation != repo.AttachmentGeneration {
			// The saved vault now belongs to a different attachment than the
			// profile prepared for it. A reconnect moves the vault to a new
			// attachment and resets the pending profile in the same commit, so
			// this is not something a retry can fix: it is conclusive, like the
			// profile store's own attachment checks.
			err = vaultprofile.MarkReconnectRequired(fmt.Errorf("pending vault profile is invalid"))
		}
		options = vaultprofile.PublishOptions{OperationID: state.OperationID, CreateOnly: state.CreateOnly,
			ExpectedCurrentSHA256: state.ExpectedCurrentSHA256}
	} else {
		if err == nil {
			read, readErr := readSyncedProfile(ctx, repo)
			revision := int64(1)
			previousUpdatedAt := time.Time{}
			expectedSHA := ""
			createOnly := false
			var currentProfile *vaultprofile.Profile
			if errors.Is(readErr, os.ErrNotExist) {
				createOnly = true
			} else if readErr != nil {
				err = readErr
			} else {
				current, parseErr := vaultprofile.Parse(read.Data)
				if parseErr != nil {
					err = parseErr
				} else if current.ProfileUUID != repo.ProfileUUID || current.Attachment.ClientUUID != repo.ClientUUID ||
					current.Attachment.Generation != repo.AttachmentGeneration {
					// Another installation, or another attachment of this one, now
					// owns the profile. Conclusive, like the profile store's own
					// attachment check (vaultprofile.ValidateAttachmentResult).
					err = vaultprofile.MarkReconnectRequired(
						fmt.Errorf("this installation is no longer attached to the vault profile; reconnect to review it"))
				} else {
					currentProfile = &current
					revision = current.Revision + 1
					previousUpdatedAt = current.UpdatedAt
					hash := sha256.Sum256(read.Data)
					expectedSHA = fmt.Sprintf("%x", hash[:])
				}
			}
			if err == nil {
				data, err = profileBytesPreserving(repo, jobs, dormant, revision, previousUpdatedAt, currentProfile)
			}
			if err == nil {
				hash := sha256.Sum256(data)
				options = vaultprofile.PublishOptions{OperationID: uuid.NewString(), CreateOnly: createOnly,
					ExpectedCurrentSHA256: expectedSHA}
				if stateErr == nil {
					err = database.PrepareVaultProfileSync(db, repositoryID, state.Revision, options.OperationID,
						fmt.Sprintf("%x", hash[:]), string(data), expectedSHA, createOnly)
					if errors.Is(err, sql.ErrNoRows) {
						// No row matched the revision read above: a profile-relevant
						// change committed meanwhile and bumped it (another vault's
						// removal rewriting a shared job, a job edit). The profile
						// built here is already stale, so nothing is published. Report
						// it as ErrVaultProfilePending, the same signal the removal's
						// delete transaction gives for a change that lands after the
						// publication, so a removal retries it once instead of failing
						// at once. The queue worker just picks up the new revision.
						err = fmt.Errorf("prepare vault recovery profile revision %d: %w", state.Revision, database.ErrVaultProfilePending)
					}
				}
			}
		}
	}
	if err == nil {
		options.PersistedJobSources = make(map[string]string, len(jobs)+len(dormant))
		for _, job := range dormant {
			options.PersistedJobSources[job.JobUUID] = job.Source
		}
		for _, job := range jobs {
			options.PersistedJobSources[job.ID] = job.Source
		}
		err = publishSyncedProfile(ctx, repo, data, options)
	}
	if err != nil {
		if stateErr == nil {
			saved := err.Error()
			if checkAfterFailure != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, database.ErrVaultProfilePending) {
				if note := checkAfterFailure(); note != "" {
					saved += "\n" + note
					err = fmt.Errorf("%w\n%s", err, note)
				}
			}
			_ = database.FailVaultProfileSync(db, repositoryID, state.Revision, saved)
		}
		return fmt.Errorf("vault settings were saved locally; vault recovery profile update is pending: %w", err)
	}
	if stateErr == nil {
		return database.CompleteVaultProfileSync(db, repositoryID, state.Revision)
	}
	return nil
}

func SyncPending(ctx context.Context, db *sql.DB) {
	startedAt := vaultreconnect.Now()
	states, err := database.ListPendingVaultProfiles(db)
	if err != nil {
		log.Printf("vault profile queue: %v", err)
		return
	}
	semaphore := make(chan struct{}, maxConcurrentProfileSyncs)
	var pending sync.WaitGroup
	for _, state := range states {
		select {
		case semaphore <- struct{}{}:
		case <-ctx.Done():
			pending.Wait()
			return
		}
		pending.Add(1)
		go func(repositoryID string) {
			defer pending.Done()
			defer func() { <-semaphore }()
			if err := syncRepositoryAttempt(ctx, db, repositoryID, startedAt); err != nil {
				log.Printf("vault profile sync %s: %v", repositoryID, err)
			}
		}(state.RepositoryID)
	}
	pending.Wait()
}

// Wake requests an immediate pass over the durable profile queue. Mutations
// remain safe if no worker is currently registered because the queue is also
// processed at startup and on the periodic retry interval.
func Wake(db *sql.DB) {
	value, ok := workerWakeups.Load(db)
	if !ok {
		return
	}
	select {
	case value.(chan struct{}) <- struct{}{}:
	default:
	}
}

func StartContext(db *sql.DB) func(context.Context) error {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	wake := make(chan struct{}, 1)
	workerWakeups.Store(db, wake)
	go func() {
		defer close(done)
		defer workerWakeups.CompareAndDelete(db, wake)
		SyncPending(ctx, db)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-wake:
				SyncPending(ctx, db)
			case <-ticker.C:
				SyncPending(ctx, db)
			}
		}
	}()
	var once sync.Once
	return func(stopCtx context.Context) error {
		once.Do(cancel)
		select {
		case <-done:
			return nil
		case <-stopCtx.Done():
			return stopCtx.Err()
		}
	}
}
