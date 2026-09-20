package vaultprofile

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/rclone"
	"github.com/local/replicaro/vaultidentity"
	"github.com/local/replicaro/vaultlock"
)

var ErrNotVaultOwner = errors.New("only the current vault owner may administer vault-wide recovery metadata")
var ErrVaultProfileAttachmentLost = errors.New("another Replicaro installation took over this vault profile")
var ErrProtectedRootIdentityMismatch = errors.New("the protected vault root does not match this saved vault")

const PhysicalPath = "replicaro/vault.replicaro"

const canonicalRootObject = "vault.replicaro"
const previousRootObject = "vault.replicaro.previous"
const pendingRootPrefix = "vault.replicaro.pending."

// Internal aliases keep the root safe-publication implementation readable in
// existing helper and provider-proof code. They are not schema aliases.
const canonicalProfileObject = canonicalRootObject
const previousProfileObject = previousRootObject
const pendingProfilePrefix = pendingRootPrefix

var runRclone = command.RunWithInputSecretsPrivateOutput
var materializeRclone = rclone.Materialize
var newRclonePreviewSidecarConfig = engines.NewRclonePreviewSidecarConfig

var ErrPasswordChangeAuthority = errors.New("vault-password change authority changed")

var errReconnectRequired = errors.New("vault reconnect required")
var errSavedVaultPasswordDecrypt = errors.New("saved vault password could not decrypt protected sidecar")

type reconnectRequiredError struct{ err error }

func (e *reconnectRequiredError) Error() string { return e.err.Error() }
func (e *reconnectRequiredError) Unwrap() error { return e.err }
func (e *reconnectRequiredError) Is(target error) bool {
	return target == errReconnectRequired || errors.Is(e.err, target)
}

func reconnectRequired(err error) error {
	if err == nil || errors.Is(err, errReconnectRequired) {
		return err
	}
	return &reconnectRequiredError{err: err}
}

// IsReconnectRequired identifies only conclusive protected-control-plane
// attachment, password, or identity facts. Transport and ambiguous read
// failures deliberately remain outside this classification.
func IsReconnectRequired(err error) bool {
	return errors.Is(err, errReconnectRequired)
}

type Store struct {
	Repository models.Repository
	// ProfileUUID selects an authoritative profile object. Empty selects the
	// vault-wide root object.
	ProfileUUID           string
	requireAvailableFunc  func(context.Context, models.Repository) error
	supportsConnectorFunc func(string, string) bool
	operationSession      **storeSession
}

// WithSession reuses native sidecar setup during one sequential operation. It
// caches no objects or authority: every read and publication still validates
// its own record, and every command reopens and validates the native config.
// The callback must not retain the Store or change its repository binding.
func (s Store) WithSession(ctx context.Context, operation func(Store) error) (err error) {
	if s.operationSession != nil {
		return operation(s)
	}
	var session *storeSession
	s.operationSession = &session
	defer func() {
		if session != nil {
			finishStoreSession(ctx, session, &err)
		}
	}()
	return operation(s)
}

func (s Store) finishSession(ctx context.Context, session *storeSession, err *error) {
	if s.operationSession == nil {
		finishStoreSession(ctx, session, err)
	}
}

func (s Store) ForProfile(profileUUID string) Store {
	s.ProfileUUID = profileUUID
	return s
}

func (s Store) objectNames() (canonical, previous, pendingPrefix, directory string, err error) {
	if s.ProfileUUID == "" {
		return canonicalRootObject, previousRootObject, pendingRootPrefix, "", nil
	}
	if !exactUUID(s.ProfileUUID) {
		return "", "", "", "", fmt.Errorf("profile_uuid is invalid")
	}
	directory = "profiles/" + s.ProfileUUID
	canonical = directory + "/profile.replicaro"
	previous = canonical + ".previous"
	pendingPrefix = canonical + ".pending."
	return canonical, previous, pendingPrefix, directory, nil
}

func (s Store) validateRecord(data []byte) error {
	if s.ProfileUUID == "" {
		root, err := ParseRoot(data, s.Repository.Connector)
		if err != nil {
			return err
		}
		if root.VaultUUID != s.Repository.ID || root.Repository.Engine != s.Repository.Engine ||
			root.Repository.NativeRepositoryID != s.Repository.NativeRepositoryID {
			return reconnectRequired(fmt.Errorf("the protected vault root does not match this saved vault"))
		}
		return nil
	}
	profile, err := Parse(data)
	if err != nil {
		return err
	}
	if profile.VaultUUID != s.Repository.ID || profile.ProfileUUID != s.ProfileUUID {
		return reconnectRequired(fmt.Errorf("vault profile identity does not match its protected path"))
	}
	return nil
}

func (s Store) validateDiscoveryRootRecord(data []byte) error {
	if s.ProfileUUID != "" {
		return fmt.Errorf("vault-root discovery cannot read a profile object")
	}
	_, err := ParseRoot(data, s.Repository.Connector)
	return err
}

func (s Store) decryptedSizeLimit(name string) (int, error) {
	canonical, previous, pendingPrefix, _, err := s.objectNames()
	if err != nil {
		return 0, err
	}
	if name != canonical && name != previous && !strings.HasPrefix(name, pendingPrefix) {
		return 0, fmt.Errorf("protected recovery object role is invalid")
	}
	if s.ProfileUUID == "" {
		return MaximumRootDecryptedSize, nil
	}
	return MaximumProfileDecryptedSize, nil
}

func validateImmutableProfileJobSources(currentData, nextData []byte, persistedSources map[string]string) error {
	current, err := Parse(currentData)
	if err != nil {
		return err
	}
	next, err := Parse(nextData)
	if err != nil {
		return err
	}
	currentSources := make(map[string]string, len(current.Jobs))
	for _, job := range current.Jobs {
		currentSources[job.JobUUID] = job.Source
	}
	for _, job := range next.Jobs {
		if source, exists := currentSources[job.JobUUID]; exists && source != job.Source && persistedSources[job.JobUUID] != job.Source {
			return fmt.Errorf("backup job source cannot be changed after creation")
		}
	}
	return nil
}

func (s Store) WithRepositoryAvailabilityCheck(
	check func(context.Context, models.Repository) error,
) Store {
	s.requireAvailableFunc = check
	return s
}

type remoteConfig struct {
	root string
	env  []string
}

// BaseRemoteConfiguration exposes only the already-established canonical
// base-root translation needed by the read-only Vault Size helper role.
// It deliberately contains no crypt remote or vault password.
type BaseRemoteConfiguration struct {
	Root string
	Env  []string
}

func BaseRemoteForVault(repo models.Repository) (BaseRemoteConfiguration, error) {
	remote, err := translateRemote(repo)
	if err != nil {
		return BaseRemoteConfiguration{}, err
	}
	baseEnv := make([]string, 0, len(remote.env))
	for _, value := range remote.env {
		if value != "RCLONE_CONFIG_BASE_NO_CHECK_BUCKET=true" {
			baseEnv = append(baseEnv, value)
		}
	}
	return BaseRemoteConfiguration{
		Root: remote.root, Env: baseEnv,
	}, nil
}

type objectStat struct {
	Name  string `json:"Name"`
	Path  string `json:"Path"`
	Size  int64  `json:"Size"`
	IsDir bool   `json:"IsDir"`
}

type recoverableProfileReadError struct{ err error }

func (e *recoverableProfileReadError) Error() string { return e.err.Error() }
func (e *recoverableProfileReadError) Unwrap() error { return e.err }

func recoverableProfileRead(err error) error {
	if err == nil {
		return nil
	}
	return &recoverableProfileReadError{err: err}
}

func isRecoverableProfileRead(err error) bool {
	var target *recoverableProfileReadError
	return errors.As(err, &target)
}

type ReadResult struct {
	Data       []byte
	Generation string
	Degraded   bool
}

// OwnershipObjects contains the exact protected records needed to present a
// fresh ownership status. All records are read through one native sidecar
// session; Owner and Local share the same result when they name one profile.
type OwnershipObjects struct {
	Root  ReadResult
	Owner ReadResult
	Local ReadResult
}

// RootCareAuthority is a fresh protected-control-plane readback. It is kept
// transient so Kopia policy reconciliation cannot substitute stale local
// owner, integrity, maintenance, or Object Lock state for the vault's current authority.
type RootCareAuthority struct {
	IntegritySchedule         string
	MaintenanceSchedule       string
	ObjectLock                models.ObjectLockSettings
	OwnerProfileUUID          string
	OwnerClientUUID           string
	OwnerAttachmentGeneration int64
}

// AssertRepositoryOwner classifies a fresh protected-root owner read against
// the local attachment. Keep the profile and attachment cases distinct: a
// different profile is not authorized, while a changed client or generation
// means this same profile was taken over by another Replicaro installation.
func (a RootCareAuthority) AssertRepositoryOwner(repo models.Repository) error {
	if a.OwnerProfileUUID != repo.ProfileUUID {
		return ErrNotVaultOwner
	}
	if a.OwnerClientUUID != repo.ClientUUID || a.OwnerAttachmentGeneration != repo.AttachmentGeneration {
		return fmt.Errorf("%w: protected vault owner attachment changed", ErrVaultProfileAttachmentLost)
	}
	return nil
}

const savedVaultPasswordFailureMessage = "The saved vault password no longer unlocks this vault. It may have been changed. Select Reconnect and enter the current password."

// IsVaultPasswordDecryptionFailure recognizes only the secret-free marker from
// a protected crypt read, without hiding a separate post-command failure.
func IsVaultPasswordDecryptionFailure(err error) bool {
	return !hasStorePostCommandFailure(err) && errors.Is(err, errSavedVaultPasswordDecrypt)
}

// ExplainSavedVaultPasswordFailure consumes only the safe marker emitted at
// the protected rclone-crypt read boundary.
func ExplainSavedVaultPasswordFailure(err error) error {
	if err == nil {
		return nil
	}
	if IsVaultPasswordDecryptionFailure(err) {
		return reconnectRequired(errors.New(savedVaultPasswordFailureMessage))
	}
	return err
}

type PublishOptions struct {
	OperationID           string
	CreateOnly            bool
	ExpectedCurrentSHA256 string
	// PersistedJobSources is supplied only by local definition synchronization.
	// Binding state is computer-local and absent from profiles: a changed source
	// must match a database-validated definition, not merely the requested bytes.
	PersistedJobSources map[string]string
}

func (s Store) session(ctx context.Context) (session *storeSession, err error) {
	if s.operationSession != nil && *s.operationSession != nil {
		if !reflect.DeepEqual(s.Repository.RuntimeView(), (*s.operationSession).repository) {
			return nil, fmt.Errorf("sidecar operation repository binding changed")
		}
		return *s.operationSession, nil
	}
	s.Repository = s.Repository.RuntimeView()
	started := time.Now()
	defer func() { reportTiming(ctx, "profile session setup", time.Since(started)) }()
	if err := models.ValidateVaultPassword(s.Repository.Passphrase); err != nil {
		return nil, err
	}
	binary, err := materializeRclone()
	if err != nil {
		return nil, fmt.Errorf("prepare rclone: %w", err)
	}
	remote, err := translateRemote(s.Repository)
	if err != nil {
		return nil, err
	}
	var cleanupConfig func() error
	var configBinding *engines.RcloneVaultConfigBinding
	configPath, pathErr := engines.RcloneVaultConfigPath(s.Repository.ID)
	if pathErr != nil && !engines.IsResticRcloneConnector(s.Repository.Connector) &&
		s.Repository.RcloneConfigPath == "" {
		configPath, cleanupConfig, err = newRclonePreviewSidecarConfig(ctx)
	} else if pathErr == nil &&
		(s.Repository.RcloneConfigPath == "" ||
			engines.IsResticRcloneConnector(s.Repository.Connector)) {
		configBinding, err = engines.OpenRcloneSidecarConfigBinding(
			ctx, s.Repository, binary,
		)
		if err == nil {
			configPath = configBinding.NativePath()
		}
	} else {
		configPath, err = engines.EnsureRcloneSidecarConfig(ctx, s.Repository, binary)
	}
	if err != nil {
		return nil, err
	}
	setupComplete := false
	defer func() {
		if !setupComplete {
			if cleanupConfig != nil {
				err = errors.Join(err, cleanupConfig())
			}
			err = errors.Join(err, configBinding.Close())
		}
	}()
	if engines.IsResticRcloneConnector(s.Repository.Connector) {
		supports := s.supportsConnectorFunc
		if supports == nil {
			supports = engines.SupportsConnector
		}
		if !supports(s.Repository.Engine, s.Repository.Connector) {
			return nil, fmt.Errorf("Restic rclone connector is unavailable on this target")
		}
	}
	baseEnv := append([]string{}, remote.env...)
	output, err := runStoreRclone(
		ctx, configBinding, binary,
		[]string{"obscure", "-", "--config", configPath}, nil,
		s.Repository.Passphrase+"\n", time.Minute, "rclone",
	)
	if err != nil {
		return nil, fmt.Errorf("prepare encrypted vault profile access: %w", err)
	}
	obscured := strings.TrimSpace(output)
	if obscured == "" || strings.ContainsAny(obscured, "\r\n") {
		return nil, fmt.Errorf("rclone returned an invalid obscured password")
	}
	obscuredKeys := map[string]bool{"RCLONE_CONFIG_BASE_PASS": true}
	for index, item := range baseEnv {
		key, raw, ok := strings.Cut(item, "=")
		if !ok || !obscuredKeys[key] || raw == "" {
			continue
		}
		value, obscureErr := runStoreRclone(
			ctx, configBinding, binary,
			[]string{"obscure", "-", "--config", configPath}, nil,
			raw+"\n", time.Minute, "rclone",
		)
		if obscureErr != nil {
			return nil, fmt.Errorf("prepare connector credential: %w", obscureErr)
		}
		obscuredValue := strings.TrimSpace(value)
		baseEnv[index] = key + "=" + obscuredValue
	}
	baseEnv = append(baseEnv,
		"RCLONE_CONFIG_CRYPT_TYPE=crypt",
		"RCLONE_CONFIG_CRYPT_REMOTE="+strings.TrimSuffix(remote.root, "/")+"/replicaro",
		"RCLONE_CONFIG_CRYPT_FILENAME_ENCRYPTION=off",
		"RCLONE_CONFIG_CRYPT_DIRECTORY_NAME_ENCRYPTION=false",
		"RCLONE_CONFIG_CRYPT_SUFFIX=none",
		"RCLONE_CONFIG_CRYPT_PASSWORD="+obscured,
		"RCLONE_CONFIG_CRYPT_PASSWORD2=",
		"RCLONE_CONFIG_CRYPT_SERVER_SIDE_ACROSS_CONFIGS=false",
		"RCLONE_CONFIG_CRYPT_SHOW_MAPPING=false",
		"RCLONE_CONFIG_CRYPT_NO_DATA_ENCRYPTION=false",
		"RCLONE_CONFIG_CRYPT_PASS_BAD_BLOCKS=false",
		"RCLONE_CONFIG_CRYPT_STRICT_NAMES=false",
		"RCLONE_CONFIG_CRYPT_FILENAME_ENCODING=base32",
	)
	session = &storeSession{
		binary: binary, config: configPath, root: remote.root, env: baseEnv,
		repository:           s.Repository,
		requireAvailableFunc: s.requireAvailableFunc,
		cleanupConfig:        cleanupConfig,
		configBinding:        configBinding,
	}
	setupComplete = true
	if s.operationSession != nil {
		*s.operationSession = session
	}
	return session, nil
}

type storeSession struct {
	binary, config, root string
	env                  []string
	repository           models.Repository
	requireAvailableFunc func(context.Context, models.Repository) error
	cleanupConfig        func() error
	configBinding        *engines.RcloneVaultConfigBinding
}

func (s *storeSession) close(_ context.Context) error {
	if s.cleanupConfig == nil {
		return s.configBinding.Close()
	}
	return errors.Join(s.cleanupConfig(), s.configBinding.Close())
}

type privateStoreSessionError struct {
	message string
	cause   error
}

func (e *privateStoreSessionError) Error() string        { return e.message }
func (e *privateStoreSessionError) Is(target error) bool { return errors.Is(e.cause, target) }
func (e *privateStoreSessionError) As(target any) bool   { return errors.As(e.cause, target) }

type storeRcloneCommandFailure struct {
	NativeStageError        error
	PostCommandCleanupError error
	ProcessStarted          bool
}

func (e *storeRcloneCommandFailure) Error() string {
	if err := errors.Join(e.NativeStageError, e.PostCommandCleanupError); err != nil {
		return err.Error()
	}
	return "native sidecar command outcome is incomplete"
}

func (e *storeRcloneCommandFailure) Unwrap() []error {
	result := make([]error, 0, 2)
	if e.NativeStageError != nil {
		result = append(result, e.NativeStageError)
	}
	if e.PostCommandCleanupError != nil {
		result = append(result, e.PostCommandCleanupError)
	}
	return result
}

func finishStoreSession(ctx context.Context, session *storeSession, operationErr *error) {
	closeErr := session.close(ctx)
	if closeErr != nil {
		closeErr = &privateStoreSessionError{message: "sidecar cleanup failed", cause: closeErr}
	}
	*operationErr = errors.Join(*operationErr, closeErr)
}

func (s *storeSession) run(ctx context.Context, args ...string) (string, error) {
	return s.runWithInput(ctx, profileTimingLabel(args), "", args...)
}

func (s *storeSession) runWithInput(ctx context.Context, timingLabel, input string, args ...string) (output string, err error) {
	started := time.Now()
	defer func() { reportTiming(ctx, timingLabel, time.Since(started)) }()
	if s.requireAvailableFunc != nil {
		if err := s.requireAvailableFunc(ctx, s.repository); err != nil {
			return "", err
		}
	}
	args = append(args, "--config", s.config, "--log-level", "ERROR")
	return runStoreRclone(
		ctx, s.configBinding, s.binary, args, s.env, input,
		10*time.Minute, "rclone",
	)
}

func runStoreRclone(
	ctx context.Context,
	binding *engines.RcloneVaultConfigBinding,
	path string,
	args, env []string,
	input string,
	timeout time.Duration,
	engine string,
) (string, error) {
	if binding != nil {
		if err := binding.Revalidate(ctx); err != nil {
			return "", &storeRcloneCommandFailure{
				NativeStageError: fmt.Errorf("rclone config admission failed before launch: %w", err),
				ProcessStarted:   false,
			}
		}
	}
	trackedContext, processStarted := command.ContextWithProcessStartTracking(ctx)
	output, nativeErr := runRclone(
		trackedContext, path, args, env, input, timeout, engine,
	)
	started := processStarted() || nativeErr == nil
	if rcloneCryptReadDecryptionFailure(args, nativeErr) {
		// Discard native text here, before persistent-session output withholding,
		// so the durable classification carries no secret-bearing error chain.
		nativeErr = errSavedVaultPasswordDecrypt
	}
	if binding == nil {
		if nativeErr != nil {
			return "", nativeErr
		}
		return output, nil
	}
	postcheckErr := binding.Revalidate(context.WithoutCancel(ctx))
	if postcheckErr != nil {
		postcheckErr = &privateStoreSessionError{
			message: "rclone config post-command validation failed",
			cause:   postcheckErr,
		}
	}
	if nativeErr == nil && postcheckErr == nil {
		return output, nil
	}
	if nativeErr != nil {
		message := "native sidecar command failed"
		if postcheckErr != nil {
			message = "native rclone output was withheld because refreshed credential state could not be safely collected"
		}
		nativeErr = &privateStoreSessionError{
			message: message,
			cause:   errors.Join(ctx.Err(), nativeErr),
		}
	}
	return "", &storeRcloneCommandFailure{
		NativeStageError: nativeErr, PostCommandCleanupError: postcheckErr,
		ProcessStarted: started,
	}
}

func rcloneCryptReadDecryptionFailure(args []string, err error) bool {
	if len(args) < 2 || args[0] != "cat" || args[1] != "crypt:" || err == nil {
		return false
	}
	return command.PrivateOutputFailureKind(err) == command.PrivateFailureDecryptAuthentication
}

func (s Store) Exists(ctx context.Context) (exists bool, err error) {
	canonical, previous, _, _, err := s.objectNames()
	if err != nil {
		return false, err
	}
	session, err := s.session(ctx)
	if err != nil {
		return false, err
	}
	defer s.finishSession(ctx, session, &err)
	objects, err := profileObjects(ctx, session)
	if err != nil {
		return false, err
	}
	_, canonicalExists := objects[canonical]
	_, previousExists := objects[previous]
	// Operation-owned pending objects are deliberately ignored here. They are
	// resumable only through their matching durable local intent and must not
	// make an abandoned operation poison future discovery.
	return canonicalExists || previousExists, nil
}

func (s Store) Read(ctx context.Context) ([]byte, error) {
	result, err := s.ReadDetailed(ctx)
	return result.Data, err
}

func (s Store) ReadDetailed(ctx context.Context) (result ReadResult, err error) {
	session, err := s.session(ctx)
	if err != nil {
		return ReadResult{}, err
	}
	defer s.finishSession(ctx, session, &err)
	return s.readDetailed(ctx, session)
}

// ReadDiscoveryRootDetailed is the one root-only bootstrap boundary used when
// connecting storage whose protected vault tuple is not known yet. It retains
// the ordinary bounded decrypt, schema, canonical/fallback, and root-structure
// checks, but leaves tuple matching to the immediate known-root reread. All
// ordinary reads and every publication continue through exact tuple validation.
func (s Store) ReadDiscoveryRootDetailed(ctx context.Context) (result ReadResult, err error) {
	if s.ProfileUUID != "" {
		return ReadResult{}, fmt.Errorf("vault-root discovery cannot read a profile object")
	}
	session, err := s.session(ctx)
	if err != nil {
		return ReadResult{}, err
	}
	defer s.finishSession(ctx, session, &err)
	return s.readDetailedValidated(ctx, session, s.validateDiscoveryRootRecord)
}

func (s Store) readDetailed(ctx context.Context, session *storeSession) (ReadResult, error) {
	return s.readDetailedValidated(ctx, session, s.validateRecord)
}

func (s Store) readDetailedValidated(
	ctx context.Context,
	session *storeSession,
	validate func([]byte) error,
) (ReadResult, error) {
	canonical, previous, _, _, err := s.objectNames()
	if err != nil {
		return ReadResult{}, err
	}
	data, canonicalErr := s.readObjectBoundedValidated(ctx, session, canonical, validate)
	if canonicalErr == nil {
		return ReadResult{Data: data, Generation: "canonical"}, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ReadResult{}, ctxErr
	}
	if !isRecoverableProfileRead(canonicalErr) {
		return ReadResult{}, canonicalErr
	}
	// The canonical profile has a fixed name, so the healthy path reads it
	// directly. Enumerate the recovery directory only to distinguish a failed
	// canonical generation from a missing one and to discover the fallback.
	objects, err := profileObjects(ctx, session)
	if err != nil {
		return ReadResult{}, err
	}
	if _, exists := objects[canonical]; exists {
		if previousStat, hasPrevious := objects[previous]; hasPrevious {
			if recovered, previousErr := s.readObjectValidated(ctx, session, previous, previousStat, validate); previousErr == nil {
				return ReadResult{Data: recovered, Generation: "previous", Degraded: true}, nil
			}
		}
		return ReadResult{}, canonicalErr
	}
	if previousStat, exists := objects[previous]; exists {
		data, readErr := s.readObjectValidated(ctx, session, previous, previousStat, validate)
		return ReadResult{Data: data, Generation: "previous", Degraded: readErr == nil}, readErr
	}
	return ReadResult{}, os.ErrNotExist
}

// ReadOwnershipObjects reads the root, current owner profile, and local
// profile without repeatedly preparing and closing the same rclone sidecar
// session. It retains ReadDetailed's bounded canonical/fallback behavior so
// callers can continue to reject previous generations as authorization.
func (s Store) ReadOwnershipObjects(ctx context.Context, localProfileUUID string) (result OwnershipObjects, err error) {
	if s.ProfileUUID != "" || !exactUUID(localProfileUUID) {
		return OwnershipObjects{}, fmt.Errorf("ownership object request is invalid")
	}
	session, err := s.session(ctx)
	if err != nil {
		return OwnershipObjects{}, err
	}
	defer s.finishSession(ctx, session, &err)

	result.Root, err = s.readDetailed(ctx, session)
	if err != nil {
		return result, err
	}
	root, err := ValidateRootIdentityResult(result.Root, s.Repository)
	if err != nil {
		return result, err
	}
	if result.Root.Degraded || result.Root.Generation != "canonical" {
		return result, fmt.Errorf("vault ownership requires the canonical protected root")
	}
	result.Local, err = s.ForProfile(localProfileUUID).readDetailed(ctx, session)
	if err != nil {
		return result, fmt.Errorf("verify vault profile attachment: %w", err)
	}
	if _, err := ValidateAttachmentResult(
		result.Local, s.Repository.ID, localProfileUUID,
		s.Repository.ClientUUID, s.Repository.AttachmentGeneration,
	); err != nil {
		return result, err
	}
	if localProfileUUID == root.VaultOwner.ProfileUUID {
		result.Owner = result.Local
		return result, nil
	}
	result.Owner, err = s.ForProfile(root.VaultOwner.ProfileUUID).readDetailed(ctx, session)
	if err != nil {
		return result, err
	}
	return result, nil
}

// ScanProfiles visits at most max authoritative canonical profile objects.
// Each protected object is bounded and decoded independently, so discovery
// never retains an unbounded remote profile set in memory.
func (s Store) ScanProfiles(ctx context.Context, max int, visit func(Profile, []byte) error) (err error) {
	if s.ProfileUUID != "" || max < 1 || visit == nil {
		return fmt.Errorf("bounded vault profile scan is invalid")
	}
	session, err := s.session(ctx)
	if err != nil {
		return err
	}
	defer s.finishSession(ctx, session, &err)
	objects, err := boundedProfileObjects(ctx, session, max)
	if err != nil {
		return err
	}
	profileUUIDs := make([]string, 0, len(objects))
	for profileUUID := range objects {
		profileUUIDs = append(profileUUIDs, profileUUID)
	}
	sort.Strings(profileUUIDs)
	for _, profileUUID := range profileUUIDs {
		candidate := objects[profileUUID]
		// Keep the one bounded native session, but validate each object with the
		// profile-scoped Store selected by its protected path. The root-scoped
		// Store would otherwise parse profile bytes as a vault root before the
		// explicit path/profile identity check below.
		profileStore := s.ForProfile(profileUUID)
		data, readErr := profileStore.readObject(ctx, session, candidate.canonicalName, candidate.canonical)
		if readErr != nil && candidate.hasPrevious && isRecoverableProfileRead(readErr) {
			data, readErr = profileStore.readObject(ctx, session, candidate.previousName, candidate.previous)
		}
		if readErr != nil {
			return fmt.Errorf("read authoritative vault profile: %w", readErr)
		}
		profile, parseErr := Parse(data)
		if parseErr != nil {
			return parseErr
		}
		if profile.ProfileUUID != profileUUID || profile.VaultUUID != s.Repository.ID {
			return reconnectRequired(fmt.Errorf("vault profile identity does not match its protected path"))
		}
		if visitErr := visit(profile, data); visitErr != nil {
			return visitErr
		}
	}
	return nil
}

type boundedProfileObject struct {
	canonicalName, previousName string
	canonical, previous         objectStat
	hasCanonical, hasPrevious   bool
}

func boundedProfileObjects(ctx context.Context, session *storeSession, max int) (map[string]boundedProfileObject, error) {
	// List only the protected profiles subtree. Decode one listing entry at a
	// time and stop retaining identities at max+1, so an oversized protected
	// tree cannot first become an unbounded in-memory object set.
	output, err := session.run(ctx, "lsjson", "crypt:profiles", "--recursive", "--files-only", "--max-depth", "3",
		"--include", "*/profile.replicaro", "--include", "*/profile.replicaro.previous")
	if err != nil {
		if rcloneObjectMissing(err) {
			return map[string]boundedProfileObject{}, nil
		}
		return nil, fmt.Errorf("list protected vault profiles: %w", err)
	}
	decoder := json.NewDecoder(strings.NewReader(output))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('[') {
		return nil, fmt.Errorf("parse protected vault profile listing")
	}
	result := make(map[string]boundedProfileObject)
	for decoder.More() {
		var entry objectStat
		if err := decoder.Decode(&entry); err != nil {
			return nil, fmt.Errorf("parse protected vault profile listing: %w", err)
		}
		name := strings.TrimPrefix(strings.ReplaceAll(entry.Path, "\\", "/"), "/")
		if name == "" {
			name = entry.Name
		}
		name = strings.TrimPrefix(name, "profiles/")
		parts := strings.Split(name, "/")
		if len(parts) != 2 || !exactUUID(parts[0]) ||
			(parts[1] != "profile.replicaro" && parts[1] != "profile.replicaro.previous") {
			continue
		}
		profileUUID := parts[0]
		candidate, exists := result[profileUUID]
		if !exists && len(result) == max {
			return nil, fmt.Errorf("vault profile scan exceeds the %d-profile limit", max)
		}
		fullName := "profiles/" + name
		if parts[1] == "profile.replicaro" {
			if candidate.hasCanonical {
				return nil, fmt.Errorf("vault profile scan contains duplicate canonical profile %s", profileUUID)
			}
			candidate.canonicalName, candidate.canonical, candidate.hasCanonical = fullName, entry, true
		} else {
			if candidate.hasPrevious {
				return nil, fmt.Errorf("vault profile scan contains duplicate previous profile %s", profileUUID)
			}
			candidate.previousName, candidate.previous, candidate.hasPrevious = fullName, entry, true
		}
		result[profileUUID] = candidate
	}
	if _, err := decoder.Token(); err != nil {
		return nil, fmt.Errorf("parse protected vault profile listing: %w", err)
	}
	for profileUUID, candidate := range result {
		if !candidate.hasCanonical {
			// Missing canonical is recoverable only through the verified previous
			// generation and is still one bounded authoritative candidate.
			candidate.canonicalName, candidate.canonical = candidate.previousName, candidate.previous
			candidate.hasPrevious = false
			result[profileUUID] = candidate
		}
	}
	return result, nil
}

func (s Store) AssertAttachment(ctx context.Context, clientUUID string, generation int64) error {
	if s.ProfileUUID == "" {
		return fmt.Errorf("profile_uuid is required for attachment authorization")
	}
	result, err := s.ReadDetailed(ctx)
	if err != nil {
		return fmt.Errorf("verify vault profile attachment: %w", err)
	}
	_, err = ValidateAttachmentResult(result, s.Repository.ID, s.ProfileUUID, clientUUID, generation)
	return err
}

// ValidateAttachmentResult verifies one already-read profile without issuing
// another native sidecar command.
func ValidateAttachmentResult(
	result ReadResult,
	repositoryID, profileUUID, clientUUID string,
	generation int64,
) (Profile, error) {
	if result.Degraded || result.Generation != "canonical" {
		// Previous generations are recovery evidence, never current attachment
		// authority for repository mutations or owner-only work.
		return Profile{}, fmt.Errorf("verify vault profile attachment: canonical profile is unavailable")
	}
	profile, err := Parse(result.Data)
	if err != nil {
		return Profile{}, fmt.Errorf("verify vault profile attachment: %w", err)
	}
	if profile.VaultUUID != repositoryID || profile.ProfileUUID != profileUUID {
		return Profile{}, reconnectRequired(fmt.Errorf("the saved vault profile does not match this vault; select Reconnect to review it"))
	}
	if profile.Attachment.ClientUUID != clientUUID || profile.Attachment.Generation != generation {
		display := strings.TrimSpace(profile.Attachment.Display.ComputerName)
		if operatingSystem := strings.TrimSpace(profile.Attachment.Display.OperatingSystem); display != "" && operatingSystem != "" {
			display += " on " + operatingSystem
		}
		if display != "" {
			return Profile{}, reconnectRequired(fmt.Errorf("%w: %s took over this vault profile; select Reconnect to review the current attachment", ErrVaultProfileAttachmentLost, display))
		}
		return Profile{}, reconnectRequired(fmt.Errorf("%w; select Reconnect to review the current attachment", ErrVaultProfileAttachmentLost))
	}
	return profile, nil
}

// AssertRootIdentity decrypts and validates the protected root and binds it to
// the saved vault, engine, and native repository identity. It grants no owner
// or profile-attachment authority.
func (s Store) AssertRootIdentity(ctx context.Context) error {
	result, err := s.ReadDetailed(ctx)
	if err != nil {
		return fmt.Errorf("verify protected vault root: %w", ExplainSavedVaultPasswordFailure(err))
	}
	_, err = ValidateRootIdentityResult(result, s.Repository)
	return err
}

// ValidateRootIdentityResult applies the saved-vault identity and password
// fence checks to an already-read protected root without another native read.
func ValidateRootIdentityResult(result ReadResult, repository models.Repository) (Root, error) {
	root, err := ParseRoot(result.Data, repository.Connector)
	if err != nil {
		return Root{}, fmt.Errorf("verify protected vault root: %w", err)
	}
	if root.VaultUUID != repository.ID || root.Repository.Engine != repository.Engine ||
		root.Repository.NativeRepositoryID != repository.NativeRepositoryID {
		return Root{}, reconnectRequired(ErrProtectedRootIdentityMismatch)
	}
	if root.PasswordChange != nil {
		return Root{}, reconnectRequired(fmt.Errorf("A vault-password change was in progress. Reconnect and enter the current vault password."))
	}
	return root, nil
}

func (s Store) AssertRootOwner(ctx context.Context, clientUUID, profileUUID string, generation int64) error {
	rootResult, err := s.ReadDetailed(ctx)
	if err != nil {
		return fmt.Errorf("verify vault owner: %w", ExplainSavedVaultPasswordFailure(err))
	}
	if rootResult.Degraded || rootResult.Generation != "canonical" {
		// A previous root can support recovery, but it cannot prove current vault
		// ownership after a takeover or attachment publication.
		return fmt.Errorf("verify vault owner: canonical root is unavailable")
	}
	root, err := ParseRoot(rootResult.Data, s.Repository.Connector)
	if err != nil {
		return fmt.Errorf("verify vault owner: %w", err)
	}
	if root.VaultUUID != s.Repository.ID || root.Repository.Engine != s.Repository.Engine ||
		root.Repository.NativeRepositoryID != s.Repository.NativeRepositoryID {
		return fmt.Errorf("the protected vault root does not match this saved vault")
	}
	if root.PasswordChange != nil {
		return fmt.Errorf("A vault-password change was in progress. Reconnect and enter the current vault password.")
	}
	if root.VaultOwner.ProfileUUID != profileUUID {
		return ErrNotVaultOwner
	}
	if root.OwnerTransfer != nil {
		return fmt.Errorf("protected vault ownership is transitional")
	}
	return s.ForProfile(profileUUID).AssertAttachment(ctx, clientUUID, generation)
}

func (s Store) ReadRootCareAuthority(ctx context.Context) (RootCareAuthority, error) {
	result, err := s.ReadDetailed(ctx)
	if err != nil {
		return RootCareAuthority{}, fmt.Errorf("verify protected vault care: %w", ExplainSavedVaultPasswordFailure(err))
	}
	if result.Degraded {
		return RootCareAuthority{}, fmt.Errorf("protected vault care requires the canonical root")
	}
	root, err := ParseRoot(result.Data, s.Repository.Connector)
	if err != nil {
		return RootCareAuthority{}, fmt.Errorf("verify protected vault care: %w", err)
	}
	if root.VaultUUID != s.Repository.ID || root.Repository.Engine != s.Repository.Engine ||
		root.Repository.NativeRepositoryID != s.Repository.NativeRepositoryID {
		return RootCareAuthority{}, reconnectRequired(fmt.Errorf("the protected vault root does not match this saved vault"))
	}
	if root.PasswordChange != nil || root.OwnerTransfer != nil {
		return RootCareAuthority{}, fmt.Errorf("protected vault care is transitional")
	}
	ownerResult, err := s.ForProfile(root.VaultOwner.ProfileUUID).ReadDetailed(ctx)
	if err != nil {
		return RootCareAuthority{}, fmt.Errorf("verify protected vault owner profile: %w", err)
	}
	if ownerResult.Degraded {
		return RootCareAuthority{}, fmt.Errorf("protected vault care requires the canonical owner profile")
	}
	owner, err := Parse(ownerResult.Data)
	if err != nil || owner.VaultUUID != s.Repository.ID || owner.ProfileUUID != root.VaultOwner.ProfileUUID {
		return RootCareAuthority{}, fmt.Errorf("protected vault owner profile is invalid")
	}
	objectLock := models.ObjectLockSettings{}
	if root.ObjectLock != nil {
		objectLock = *root.ObjectLock
	}
	return RootCareAuthority{
		IntegritySchedule:         root.Integrity.Schedule,
		MaintenanceSchedule:       root.Maintenance.Schedule,
		ObjectLock:                objectLock,
		OwnerProfileUUID:          root.VaultOwner.ProfileUUID,
		OwnerClientUUID:           owner.Attachment.ClientUUID,
		OwnerAttachmentGeneration: owner.Attachment.Generation,
	}, nil
}

func (s Store) AssertRootCare(ctx context.Context, integritySchedule, maintenanceSchedule string, objectLock models.ObjectLockSettings) (RootCareAuthority, error) {
	authority, err := s.ReadRootCareAuthority(ctx)
	if err != nil {
		return RootCareAuthority{}, err
	}
	if authority.IntegritySchedule != integritySchedule || authority.MaintenanceSchedule != maintenanceSchedule || authority.ObjectLock != objectLock {
		return RootCareAuthority{}, fmt.Errorf("protected vault care does not match the durable desired state")
	}
	return authority, nil
}

// AssertPasswordChangeAuthority is the rotation-only owner/attachment proof.
// Unlike AssertRootOwner it admits only this operation's exact fence and never
// makes fenced roots valid for ordinary administration.
func (s Store) AssertPasswordChangeAuthority(ctx context.Context, clientUUID, profileUUID string, generation int64,
	operationUUID string, allowUnfenced, requireProfileFence bool,
) error {
	fail := func(message string) error {
		return fmt.Errorf("%w: %s", ErrPasswordChangeAuthority, message)
	}
	if !exactUUID(operationUUID) || !exactUUID(clientUUID) || !exactUUID(profileUUID) || generation < 1 {
		return fail("password-change owner attachment identity is invalid")
	}
	rootResult, err := s.ReadDetailed(ctx)
	if err != nil {
		return fail("the current protected vault root could not be verified")
	}
	root, err := ParseRoot(rootResult.Data, s.Repository.Connector)
	if err != nil || root.VaultUUID != s.Repository.ID || root.Repository.Engine != s.Repository.Engine ||
		root.Repository.NativeRepositoryID != s.Repository.NativeRepositoryID ||
		root.VaultOwner.ProfileUUID != profileUUID || root.OwnerTransfer != nil {
		return fail("only the unchanged current vault owner may continue this password change")
	}
	rootFenced := root.PasswordChange != nil
	if rootFenced {
		if root.PasswordChange.OperationUUID != operationUUID || rootResult.Degraded {
			return fail("the canonical vault root has a different or unverifiable password-change fence")
		}
	} else if !allowUnfenced {
		return fail("the canonical vault root no longer has this password-change fence")
	}

	profileResult, err := s.ForProfile(profileUUID).ReadDetailed(ctx)
	if err != nil {
		return fail("the current owner profile could not be verified")
	}
	profile, err := Parse(profileResult.Data)
	if err != nil || profile.VaultUUID != s.Repository.ID || profile.ProfileUUID != profileUUID ||
		profile.Attachment.ClientUUID != clientUUID || profile.Attachment.Generation != generation {
		return fail("the current owner profile attachment changed")
	}
	if rootFenced && profileResult.Degraded {
		return fail("the canonical owner profile could not be verified through this password-change fence")
	}
	if profile.PasswordChange != nil && profile.PasswordChange.OperationUUID != operationUUID {
		return fail("the owner profile has a different password-change fence")
	}
	if !rootFenced && profile.PasswordChange != nil {
		return fail("the owner profile fence no longer matches the canonical root")
	}
	if requireProfileFence && (profile.PasswordChange == nil || !rootFenced) {
		return fail("the canonical owner profile does not have this password-change fence")
	}
	return nil
}

// AssertPasswordChangeRootState is the publishing-recovery root proof. Profile
// objects may already be re-encrypted at this phase, so it verifies only the
// unchanged vault identity/owner and either this exact old-password fence or
// the final unfenced new-password root.
func (s Store) AssertPasswordChangeRootState(ctx context.Context, profileUUID, operationUUID string, fenced bool) error {
	fail := func() error {
		return fmt.Errorf("%w: the protected vault root is not in the expected password-change state", ErrPasswordChangeAuthority)
	}
	if !exactUUID(profileUUID) || !exactUUID(operationUUID) {
		return fail()
	}
	result, err := s.ReadDetailed(ctx)
	if err != nil || result.Degraded {
		return fail()
	}
	root, err := ParseRoot(result.Data, s.Repository.Connector)
	if err != nil || root.VaultUUID != s.Repository.ID || root.Repository.Engine != s.Repository.Engine ||
		root.Repository.NativeRepositoryID != s.Repository.NativeRepositoryID ||
		root.VaultOwner.ProfileUUID != profileUUID || root.OwnerTransfer != nil {
		return fail()
	}
	if fenced {
		if root.PasswordChange == nil || root.PasswordChange.OperationUUID != operationUUID {
			return fail()
		}
	} else if root.PasswordChange != nil {
		return fail()
	}
	return nil
}

func (s Store) Replace(ctx context.Context, plaintext []byte) error {
	return s.Publish(ctx, plaintext, PublishOptions{OperationID: uuid.NewString()})
}

// Create publishes a profile only if the exact object is still absent. It is
// used by the missing-profile connect fallback so a profile created by another
// actor after preview is never overwritten.
func (s Store) Create(ctx context.Context, plaintext []byte) error {
	return s.Publish(ctx, plaintext, PublishOptions{OperationID: uuid.NewString(), CreateOnly: true})
}

// Publish is the single crash-recoverable state machine used for profile
// creation, connection, and synchronization.
func (s Store) Publish(ctx context.Context, plaintext []byte, options PublishOptions) error {
	s.Repository = s.Repository.RuntimeView()
	if !exactUUID(s.Repository.ID) {
		return fmt.Errorf("repository vault UUID is invalid")
	}
	// Sidecar commands are outside native repository locks, so they share the
	// same UUID key as every other local operation on this managed vault.
	unlock, err := vaultlock.AcquireExclusiveContext(ctx, s.Repository.ID)
	if err != nil {
		return fmt.Errorf("wait for vault profile publication lock: %w", err)
	}
	defer unlock()
	return s.publishUnderLock(ctx, plaintext, options)
}

// PublishUnderLock publishes a profile while the caller already holds the
// managed vault UUID lock. It is used by profile synchronization and creation
// paths that must keep their database and remote publication sections under
// one admission gate.
func (s Store) PublishUnderLock(ctx context.Context, plaintext []byte, options PublishOptions) error {
	return s.publishUnderLock(ctx, plaintext, options)
}

func (s Store) publishUnderLock(ctx context.Context, plaintext []byte, options PublishOptions) (err error) {
	if !exactUUID(s.Repository.ID) {
		return fmt.Errorf("repository vault UUID is invalid")
	}
	// Root publication must prove the same UUID/engine/native-repository tuple
	// that selected the caller's one UUID lock before any sidecar command runs.
	if err := s.validateRecord(plaintext); err != nil {
		return err
	}
	if parsed, err := uuid.Parse(options.OperationID); err != nil || parsed.String() != options.OperationID {
		return fmt.Errorf("profile publication operation ID is invalid")
	}
	session, err := s.session(ctx)
	if err != nil {
		return err
	}
	defer s.finishSession(ctx, session, &err)
	canonical, previous, pendingPrefix, _, err := s.objectNames()
	if err != nil {
		return err
	}
	objects, err := profileObjects(ctx, session)
	if err != nil {
		return err
	}
	pendingObject := pendingPrefix + options.OperationID
	currentData, currentValid, canonicalInvalid := []byte(nil), false, false
	if current, exists := objects[canonical]; exists {
		currentData, err = s.readObject(ctx, session, canonical, current)
		if err != nil && !isRecoverableProfileRead(err) {
			return fmt.Errorf("read current vault recovery profile: %w", err)
		}
		currentValid = err == nil
		canonicalInvalid = err != nil
		if currentValid {
			fenced := false
			if s.ProfileUUID == "" {
				root, parseErr := ParseRoot(currentData, s.Repository.Connector)
				fenced = parseErr == nil && root.PasswordChange != nil
			} else {
				profile, parseErr := Parse(currentData)
				fenced = parseErr == nil && profile.PasswordChange != nil
			}
			if fenced {
				return fmt.Errorf("vault-password change is in progress; ordinary recovery-profile publication is blocked")
			}
		}
		if currentValid && string(currentData) == string(plaintext) {
			cleanupProfileObject(ctx, session, pendingObject)
			return nil
		}
		if options.CreateOnly {
			return fmt.Errorf("vault recovery profile appeared after preview; check the vault again")
		}
	}
	previousData, previousValid := []byte(nil), false
	if previousStat, exists := objects[previous]; exists {
		previousData, err = s.readObject(ctx, session, previous, previousStat)
		if err != nil && !isRecoverableProfileRead(err) {
			return fmt.Errorf("read previous vault recovery profile: %w", err)
		}
		previousValid = err == nil
	}
	if !currentValid && previousValid {
		fenced := false
		if s.ProfileUUID == "" {
			root, parseErr := ParseRoot(previousData, s.Repository.Connector)
			fenced = parseErr == nil && root.PasswordChange != nil
		} else {
			profile, parseErr := Parse(previousData)
			fenced = parseErr == nil && profile.PasswordChange != nil
		}
		if fenced {
			return fmt.Errorf("vault-password change is in progress; ordinary recovery-profile fallback publication is blocked")
		}
	}
	if options.CreateOnly && !canonicalInvalid && previousValid {
		return fmt.Errorf("vault recovery profile appeared after preview; check the vault again")
	}
	if s.ProfileUUID != "" {
		basis := currentData
		if !currentValid {
			basis = previousData
		}
		if len(basis) > 0 {
			if err := validateImmutableProfileJobSources(basis, plaintext, options.PersistedJobSources); err != nil {
				return err
			}
		}
	}
	expectedCurrentSHA256 := options.ExpectedCurrentSHA256
	if expectedCurrentSHA256 == "" && !options.CreateOnly {
		basis := currentData
		if !currentValid {
			basis = previousData
		}
		if len(basis) > 0 {
			expectedCurrentSHA256 = fmt.Sprintf("%x", sha256.Sum256(basis))
		}
	}
	if expectedCurrentSHA256 != "" {
		if err := s.ensureExpectedCurrent(ctx, session, expectedCurrentSHA256); err != nil {
			return err
		}
	}
	localPath, err := stageProfile(plaintext, "vault-profile-new-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(localPath)
	if pending, exists := objects[pendingObject]; exists {
		pendingData, pendingErr := s.readObject(ctx, session, pendingObject, pending)
		if pendingErr != nil {
			return fmt.Errorf("read pending vault recovery profile: %w", pendingErr)
		}
		if string(pendingData) != string(plaintext) {
			return fmt.Errorf("profile publication operation has conflicting pending data")
		}
	} else if _, err := session.run(ctx, "copyto", localPath, "crypt:"+pendingObject, "--immutable"); err != nil {
		return fmt.Errorf("upload pending vault recovery profile: %w", err)
	}
	pending, pendingErr := s.readObjectBounded(ctx, session, pendingObject)
	if pendingErr != nil {
		return fmt.Errorf("verify pending vault recovery profile: %w", pendingErr)
	}
	if string(pending) != string(plaintext) {
		return fmt.Errorf("verify pending vault recovery profile: uploaded bytes differ")
	}
	if currentValid {
		// The pending upload can take long enough for an owner fence to appear.
		// Revalidate immediately before mutating the retained fallback.
		if expectedCurrentSHA256 != "" {
			if err := s.ensureExpectedCurrent(ctx, session, expectedCurrentSHA256); err != nil {
				return err
			}
		}
		if _, err := session.run(ctx, "copyto", "crypt:"+canonical, "crypt:"+previous, "--ignore-times"); err != nil {
			return fmt.Errorf("preserve previous vault recovery profile: %w", err)
		}
		preserved, preserveErr := s.readObjectBounded(ctx, session, previous)
		if preserveErr != nil {
			return fmt.Errorf("verify previous vault recovery profile: %w", preserveErr)
		}
		if string(preserved) != string(currentData) {
			return fmt.Errorf("verify previous vault recovery profile: preserved bytes differ")
		}
	}
	if expectedCurrentSHA256 != "" {
		if err := s.ensureExpectedCurrent(ctx, session, expectedCurrentSHA256); err != nil {
			return err
		}
	}
	publishArgs := []string{"copyto", "crypt:" + pendingObject, "crypt:" + canonical}
	if options.CreateOnly {
		publishArgs = append(publishArgs, "--immutable")
	} else {
		publishArgs = append(publishArgs, "--ignore-times")
	}
	if _, err := session.run(ctx, publishArgs...); err != nil {
		return fmt.Errorf("publish vault recovery profile: %w", err)
	}
	published, publishErr := s.readObjectBounded(ctx, session, canonical)
	if publishErr != nil {
		return fmt.Errorf("verify published vault recovery profile: %w", publishErr)
	}
	if string(published) != string(plaintext) {
		// Do not restore the previous generation here. A different writer may
		// have published a newer canonical profile after this operation started;
		// restoring would turn a verification failure into a lost update.
		return fmt.Errorf("verify published vault recovery profile: published bytes differ")
	}
	cleanupProfileObject(ctx, session, pendingObject)
	return nil
}

func (s Store) ensureExpectedCurrent(ctx context.Context, session *storeSession, expected string) error {
	canonical, previous, _, _, err := s.objectNames()
	if err != nil {
		return err
	}
	// Keep all three publication checkpoints, but read the fixed canonical
	// address directly. Listing is recovery discovery, not additional authority.
	data, readErr := s.readObjectBounded(ctx, session, canonical)
	if readErr == nil {
		if fmt.Sprintf("%x", sha256.Sum256(data)) != expected {
			return fmt.Errorf("vault recovery profile changed before publication")
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !isRecoverableProfileRead(readErr) {
		return fmt.Errorf("read current vault recovery profile: %w", readErr)
	}
	objects, err := profileObjects(ctx, session)
	if err != nil {
		return err
	}
	var basis []byte
	if len(basis) == 0 {
		if previousStat, exists := objects[previous]; exists {
			if data, readErr := s.readObject(ctx, session, previous, previousStat); readErr == nil {
				basis = data
			} else if !isRecoverableProfileRead(readErr) {
				return fmt.Errorf("read previous vault recovery profile: %w", readErr)
			}
		}
	}
	if len(basis) == 0 || fmt.Sprintf("%x", sha256.Sum256(basis)) != expected {
		return fmt.Errorf("vault recovery profile changed before publication")
	}
	return nil
}

func cleanupProfileObject(parent context.Context, session *storeSession, name string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 20*time.Second)
	defer cancel()
	_, _ = session.run(ctx, "deletefile", "crypt:"+name)
}

func stageProfile(plaintext []byte, pattern string) (string, error) {
	directory, err := appdata.Directory(filepath.Join("profile", "staging"))
	if err != nil {
		return "", err
	}
	local, err := os.CreateTemp(directory, pattern)
	if err != nil {
		return "", err
	}
	path := local.Name()
	ok := false
	defer func() {
		if !ok {
			_ = local.Close()
			_ = os.Remove(path)
		}
	}()
	if err := local.Chmod(0o600); err != nil {
		return "", err
	}
	if _, err := local.Write(plaintext); err != nil {
		return "", err
	}
	if err := local.Sync(); err != nil {
		return "", err
	}
	if err := local.Close(); err != nil {
		return "", err
	}
	if err := appdata.SecurePath(path, false); err != nil {
		return "", err
	}
	ok = true
	return path, nil
}

// CleanupStaging removes plaintext profile files left by an unclean process
// exit. Startup holds the single-instance guard before calling this function.
func CleanupStaging() error {
	directory, err := appdata.Directory(filepath.Join("profile", "staging"))
	if err != nil {
		return err
	}
	for _, pattern := range []string{"vault-profile-*.json", "vault-password-object-*.json"} {
		matches, err := filepath.Glob(filepath.Join(directory, pattern))
		if err != nil {
			return err
		}
		for _, match := range matches {
			if err := os.Remove(match); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}

func (s Store) readObject(ctx context.Context, session *storeSession, name string, stat objectStat) ([]byte, error) {
	return s.readObjectValidated(ctx, session, name, stat, s.validateRecord)
}

func (s Store) readObjectValidated(
	ctx context.Context,
	session *storeSession,
	name string,
	stat objectStat,
	validate func([]byte) error,
) ([]byte, error) {
	limit, err := s.decryptedSizeLimit(name)
	if err != nil {
		return nil, recoverableProfileRead(err)
	}
	if stat.IsDir || stat.Size <= 0 || stat.Size > int64(limit) {
		return nil, recoverableProfileRead(fmt.Errorf("vault recovery profile size is invalid"))
	}
	return s.readObjectBoundedValidated(ctx, session, name, validate)
}

func (s Store) readObjectBounded(ctx context.Context, session *storeSession, name string) ([]byte, error) {
	return s.readObjectBoundedValidated(ctx, session, name, s.validateRecord)
}

func (s Store) readObjectBoundedValidated(
	ctx context.Context,
	session *storeSession,
	name string,
	validate func([]byte) error,
) ([]byte, error) {
	limit, limitErr := s.decryptedSizeLimit(name)
	if limitErr != nil {
		return nil, recoverableProfileRead(limitErr)
	}
	args := []string{"cat", "crypt:", "--files-from-raw", "-", "--no-traverse", "--count", strconv.Itoa(limit + 1)}
	output, err := session.runWithInput(ctx, profileTimingLabel([]string{"cat", "crypt:" + name}), name+"\n", args...)
	if err != nil {
		if rcloneObjectMissing(err) || rcloneObjectIsDirectory(err) {
			return nil, recoverableProfileRead(err)
		}
		return nil, err
	}
	data := []byte(output)
	if len(data) > limit {
		return nil, recoverableProfileRead(fmt.Errorf("vault recovery profile exceeds the size limit"))
	}
	if err := validate(data); err != nil {
		if s.ProfileUUID == "" && s.Repository.Connector == "s3" {
			var document struct {
				Repository map[string]json.RawMessage `json:"repository"`
			}
			if json.Unmarshal(data, &document) == nil && document.Repository["s3Storage"] != nil {
				// A canonical S3 storage declaration is authoritative. Falling
				// back to an older generation could silently downgrade a cold
				// vault to ordinary storage after a damaged declaration.
				return nil, err
			}
		}
		if IsReconnectRequired(err) {
			return nil, err
		}
		return nil, recoverableProfileRead(err)
	}
	return data, nil
}

func readProfileObjectBounded(ctx context.Context, session *storeSession, name string) ([]byte, error) {
	scoped, err := rotationStoreForObject(Store{Repository: session.repository}, name)
	if err != nil {
		return nil, recoverableProfileRead(err)
	}
	return scoped.readObjectBounded(ctx, session, name)
}

func profileObjects(ctx context.Context, session *storeSession) (map[string]objectStat, error) {
	// The crypt remote is already rooted at <vault>/replicaro. Listing it
	// directly avoids enumerating a potentially huge bucket or prefix merely to
	// discover whether that one directory exists.
	output, err := session.run(ctx, "lsjson", "crypt:", "--recursive", "--files-only", "--max-depth", "4")
	if err != nil {
		if rcloneObjectMissing(err) {
			return map[string]objectStat{}, nil
		}
		return nil, fmt.Errorf("list Replicaro recovery metadata: %w", err)
	}
	var entries []objectStat
	if err := json.Unmarshal([]byte(output), &entries); err != nil {
		return nil, fmt.Errorf("parse Replicaro recovery metadata listing: %w", err)
	}
	result := map[string]objectStat{}
	for _, entry := range entries {
		name := strings.TrimPrefix(strings.ReplaceAll(entry.Path, "\\", "/"), "/")
		if name == "" {
			name = entry.Name
		}
		if validProtectedObjectName(name) {
			result[name] = entry
		}
	}
	return result, nil
}

func validProtectedObjectName(name string) bool {
	if name == canonicalRootObject || name == previousRootObject || strings.HasPrefix(name, pendingRootPrefix) {
		return true
	}
	parts := strings.Split(name, "/")
	if len(parts) != 3 || parts[0] != "profiles" || !exactUUID(parts[1]) {
		return false
	}
	return parts[2] == "profile.replicaro" || parts[2] == "profile.replicaro.previous" ||
		strings.HasPrefix(parts[2], "profile.replicaro.pending.")
}

func statObject(ctx context.Context, session *storeSession, remote string) (objectStat, bool, error) {
	output, err := session.run(ctx, "lsjson", remote, "--stat")
	if err != nil {
		if rcloneObjectMissing(err) {
			return objectStat{}, false, nil
		}
		return objectStat{}, false, err
	}
	var stat objectStat
	if err := json.Unmarshal([]byte(output), &stat); err != nil {
		return objectStat{}, false, fmt.Errorf("parse rclone object metadata: %w", err)
	}
	return stat, true, nil
}

func rcloneObjectMissing(err error) bool {
	if err == nil {
		return false
	}
	if hasStorePostCommandFailure(err) {
		return false
	}
	if command.PrivateOutputFailureKind(err) == command.PrivateFailureMissingObject {
		return true
	}
	lower := strings.ToLower(err.Error())
	for _, unsafe := range []string{"credential", "authentication", "authorization", "access denied", "permission denied", "unauthorized", "forbidden", "private key", "certificate"} {
		if strings.Contains(lower, unsafe) {
			return false
		}
	}
	for _, missing := range []string{"directory not found", "object not found", "file not found", "path not found", "doesn't exist", "does not exist"} {
		if strings.Contains(lower, missing) {
			return true
		}
	}
	return false
}

func rcloneObjectIsDirectory(err error) bool {
	if err == nil {
		return false
	}
	if hasStorePostCommandFailure(err) {
		return false
	}
	if command.PrivateOutputFailureKind(err) == command.PrivateFailureObjectIsDirectory {
		return true
	}
	lower := strings.ToLower(err.Error())
	for _, marker := range []string{"is a directory", "not a regular file", "can't open directory", "cannot open directory"} {
		if strings.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func hasStorePostCommandFailure(err error) bool {
	if err == nil {
		return false
	}
	if commandFailure, ok := err.(*storeRcloneCommandFailure); ok && commandFailure.PostCommandCleanupError != nil {
		return true
	}
	if sessionFailure, ok := err.(*privateStoreSessionError); ok && (sessionFailure.message == "sidecar cleanup failed" ||
		sessionFailure.message == "rclone config post-command validation failed") {
		return true
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, child := range joined.Unwrap() {
			if hasStorePostCommandFailure(child) {
				return true
			}
		}
		return false
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return hasStorePostCommandFailure(wrapped.Unwrap())
	}
	return false
}

func translateRemote(repo models.Repository) (remoteConfig, error) {
	effective, err := vaultidentity.ResolveEffectiveAddress(repo.Connector, repo.Location, repo.ConnectorOptions)
	if err != nil {
		return remoteConfig{}, err
	}
	env := []string{}
	root := ""
	switch repo.Connector {
	case "fs":
		env = append(env, "RCLONE_CONFIG_BASE_TYPE=local")
		// filepath.ToSlash is a native conversion: on POSIX a literal
		// backslash is part of the filename and must not be rewritten.
		root = "base:" + filepath.ToSlash(filepath.Clean(effective.Location))
	case "s3":
		archiveWriteClass, coldErr := models.NormalizeColdStorage(repo.Engine, repo.Connector, repo.ColdStorage, repo.ArchiveWriteClass)
		if coldErr != nil {
			return remoteConfig{}, coldErr
		}
		_ = archiveWriteClass
		storageClass := ""
		if repo.Engine == "restic" {
			storageClass = strings.TrimSpace(repo.ConnectorOptions["storage_class"])
		}
		ordinaryStorageClass := strings.ToUpper(storageClass)
		if !repo.ColdStorage && (ordinaryStorageClass == models.ArchiveWriteClassGlacier || ordinaryStorageClass == models.ArchiveWriteClassDeepArchive) {
			return remoteConfig{}, fmt.Errorf("Storage class %s requires the cold storage option; change your storage type to cold storage from the top of the window", ordinaryStorageClass)
		}
		if repo.ColdStorage {
			storageClass = "STANDARD"
		}
		env = append(env, "RCLONE_CONFIG_BASE_TYPE=s3", "RCLONE_CONFIG_BASE_PROVIDER=Other",
			"RCLONE_CONFIG_BASE_NO_CHECK_BUCKET=true",
			"RCLONE_CONFIG_BASE_ENV_AUTH=false",
			"RCLONE_CONFIG_BASE_ACCESS_KEY_ID="+repo.ConnectorOptions["access_key"],
			"RCLONE_CONFIG_BASE_SECRET_ACCESS_KEY="+repo.ConnectorOptions["secret_access_key"],
			"RCLONE_CONFIG_BASE_SESSION_TOKEN=", "RCLONE_CONFIG_BASE_SHARED_CREDENTIALS_FILE=",
			"RCLONE_CONFIG_BASE_PROFILE=", "RCLONE_CONFIG_BASE_ROLE_ARN=",
			"RCLONE_CONFIG_BASE_ROLE_SESSION_NAME=", "RCLONE_CONFIG_BASE_ROLE_EXTERNAL_ID=")
		if effective.Endpoint != "" {
			env = append(env, "RCLONE_CONFIG_BASE_ENDPOINT="+effective.Endpoint)
		}
		env = append(env,
			"RCLONE_CONFIG_BASE_FORCE_PATH_STYLE=true",
			"RCLONE_CONFIG_BASE_SSE_CUSTOMER_KEY="+repo.ConnectorOptions["sse_customer_key"])
		if storageClass != "" {
			env = append(env, "RCLONE_CONFIG_BASE_STORAGE_CLASS="+storageClass)
		}
		if region := strings.TrimSpace(repo.ConnectorOptions["region"]); region != "" {
			env = append(env, "RCLONE_CONFIG_BASE_REGION="+region)
		}
		if repo.ConnectorOptions["tls_insecure_no_verify"] == "true" {
			env = append(env, "RCLONE_NO_CHECK_CERTIFICATE=true")
		}
		root = remotePath(effective.Bucket, effective.Prefix)
	case "sftp":
		env = append(env, "RCLONE_CONFIG_BASE_TYPE=sftp", "RCLONE_CONFIG_BASE_HOST="+effective.Host,
			"RCLONE_CONFIG_BASE_USER="+effective.Username, "RCLONE_CONFIG_BASE_PORT="+effective.Port,
			"RCLONE_CONFIG_BASE_PASS="+repo.ConnectorOptions["password"],
			"RCLONE_CONFIG_BASE_KEY_FILE="+repo.ConnectorOptions["identity"],
			"RCLONE_CONFIG_BASE_KEY_PEM="+repo.ConnectorOptions["ssh_private_key"],
			"RCLONE_CONFIG_BASE_KEY_FILE_PASS=", "RCLONE_CONFIG_BASE_PUBKEY=",
			"RCLONE_CONFIG_BASE_PUBKEY_FILE=", "RCLONE_CONFIG_BASE_SSH=",
			"RCLONE_CONFIG_BASE_ASK_PASSWORD=false",
			"RCLONE_CONFIG_BASE_KEY_USE_AGENT="+strconv.FormatBool(repo.ConnectorOptions["ssh_auth_sock"] != ""),
			"RCLONE_CONFIG_BASE_KNOWN_HOSTS_FILE=", "RCLONE_CONFIG_BASE_HOST_KEYS=",
			"RCLONE_CONFIG_BASE_PIN_HOST_KEY=false", "RCLONE_CONFIG_BASE_HOST_KEY_ALGORITHMS=")
		if repo.ConnectorOptions["insecure_ignore_host_key"] != "true" {
			home, homeErr := os.UserHomeDir()
			if homeErr != nil || strings.TrimSpace(home) == "" {
				if homeErr == nil {
					homeErr = fmt.Errorf("user home is empty")
				}
				return remoteConfig{}, fmt.Errorf("resolve SSH known_hosts: %w", homeErr)
			}
			knownHosts := filepath.Join(home, ".ssh", "known_hosts")
			info, statErr := os.Stat(knownHosts)
			if statErr != nil {
				if errors.Is(statErr, os.ErrNotExist) {
					if repo.Engine == "kopia" && strings.TrimSpace(repo.ConnectorOptions["password"]) != "" {
						return remoteConfig{}, fmt.Errorf("SSH known_hosts file %q is required for Kopia SFTP password authentication; add this server's host key before retrying", knownHosts)
					}
					return remoteConfig{}, fmt.Errorf("SSH known_hosts file %q does not exist; add this server's host key or enable \"Disable host-key/known_hosts verification\"", knownHosts)
				}
				return remoteConfig{}, fmt.Errorf("inspect SSH known_hosts file: %w", statErr)
			}
			if info.IsDir() {
				return remoteConfig{}, fmt.Errorf("SSH known_hosts path %q is a directory", knownHosts)
			}
			for index := range env {
				if strings.HasPrefix(env[index], "RCLONE_CONFIG_BASE_KNOWN_HOSTS_FILE=") {
					env[index] = "RCLONE_CONFIG_BASE_KNOWN_HOSTS_FILE=" + knownHosts
					break
				}
			}
		}
		if socket := repo.ConnectorOptions["ssh_auth_sock"]; socket != "" {
			env = append(env, "SSH_AUTH_SOCK="+socket)
		}
		root = "base:" + effective.Prefix
	case "azblob":
		account := effective.AccountName
		env = append(env, "RCLONE_CONFIG_BASE_TYPE=azureblob",
			"RCLONE_CONFIG_BASE_ACCOUNT="+account,
			"RCLONE_CONFIG_BASE_KEY="+repo.ConnectorOptions["account_key"],
			"RCLONE_CONFIG_BASE_ENDPOINT="+effective.Endpoint,
			"RCLONE_CONFIG_BASE_CONNECTION_STRING="+repo.ConnectorOptions["connection_string"],
			"RCLONE_CONFIG_BASE_ENV_AUTH=false", "RCLONE_CONFIG_BASE_SAS_URL=",
			"RCLONE_CONFIG_BASE_TENANT=", "RCLONE_CONFIG_BASE_CLIENT_ID=",
			"RCLONE_CONFIG_BASE_CLIENT_SECRET=", "RCLONE_CONFIG_BASE_CLIENT_CERTIFICATE_PATH=",
			"RCLONE_CONFIG_BASE_CLIENT_CERTIFICATE_PASSWORD=", "RCLONE_CONFIG_BASE_USERNAME=",
			"RCLONE_CONFIG_BASE_PASSWORD=", "RCLONE_CONFIG_BASE_SERVICE_PRINCIPAL_FILE=",
			"RCLONE_CONFIG_BASE_USE_MSI=false", "RCLONE_CONFIG_BASE_MSI_OBJECT_ID=",
			"RCLONE_CONFIG_BASE_MSI_CLIENT_ID=", "RCLONE_CONFIG_BASE_MSI_MI_RES_ID=",
			"RCLONE_CONFIG_BASE_USE_EMULATOR=false", "RCLONE_CONFIG_BASE_USE_AZ=false")
		root = remotePath(effective.Bucket, effective.Prefix)
	case "gcs":
		env = append(env, "RCLONE_CONFIG_BASE_TYPE=googlecloudstorage",
			"RCLONE_CONFIG_BASE_SERVICE_ACCOUNT_FILE="+repo.ConnectorOptions["credentials_file"],
			"RCLONE_CONFIG_BASE_SERVICE_ACCOUNT_CREDENTIALS="+repo.ConnectorOptions["credentials_json"],
			"RCLONE_CONFIG_BASE_ENDPOINT="+repo.ConnectorOptions["endpoint"],
			"RCLONE_CONFIG_BASE_BUCKET_POLICY_ONLY=true", "RCLONE_CONFIG_BASE_ENV_AUTH=false",
			"RCLONE_CONFIG_BASE_ANONYMOUS=false",
			"RCLONE_CONFIG_BASE_CLIENT_ID=", "RCLONE_CONFIG_BASE_CLIENT_SECRET=",
			"RCLONE_CONFIG_BASE_TOKEN=", "RCLONE_CONFIG_BASE_ACCESS_TOKEN=",
			"RCLONE_CONFIG_BASE_CLIENT_CREDENTIALS=false", "RCLONE_CONFIG_BASE_AUTH_URL=",
			"RCLONE_CONFIG_BASE_TOKEN_URL=")
		root = remotePath(effective.Bucket, effective.Prefix)
	case "google_drive", "dropbox", "onedrive":
		provider, ok := engines.RcloneProvider(repo.Connector)
		if !ok || repo.Engine != engines.ResticID {
			return remoteConfig{}, fmt.Errorf("unsupported connector-to-rclone translation: %s", repo.Connector)
		}
		_ = provider
		root = "provider:" + effective.Location
	default:
		return remoteConfig{}, fmt.Errorf("unsupported connector-to-rclone translation: %s", repo.Connector)
	}
	return remoteConfig{root: strings.TrimSuffix(root, "/"), env: env}, nil
}

func remotePath(parts ...string) string {
	clean := []string{}
	for _, part := range parts {
		part = strings.Trim(strings.ReplaceAll(part, "\\", "/"), "/")
		if part != "" && part != "." {
			clean = append(clean, part)
		}
	}
	return "base:" + strings.Join(clean, "/")
}

func (s Store) ListRoot(ctx context.Context) (values []objectStat, err error) {
	s.Repository = s.Repository.RuntimeView()
	if s.Repository.Connector == "fs" {
		info, err := os.Stat(s.Repository.Location)
		if errors.Is(err, os.ErrNotExist) {
			return []objectStat{}, nil
		}
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("selected vault root is not a directory")
		}
	}
	session, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	defer s.finishSession(ctx, session, &err)
	remote, err := translateRemote(s.Repository)
	if err != nil {
		return nil, err
	}
	output, err := session.run(ctx, "lsjson", remote.root, "--max-depth", "1")
	if err != nil {
		if rcloneObjectMissing(err) {
			return []objectStat{}, nil
		}
		return nil, err
	}
	values = nil
	if err := json.Unmarshal([]byte(output), &values); err != nil {
		return nil, fmt.Errorf("parse vault root listing: %w", err)
	}
	return values, nil
}

func (s Store) ReadRootObject(ctx context.Context, name string, maximum int64) (data []byte, err error) {
	name = strings.Trim(strings.ReplaceAll(name, "\\", "/"), "/")
	if name == "" || strings.Contains(name, "/") || maximum <= 0 {
		return nil, fmt.Errorf("invalid exact-root object request")
	}
	session, err := s.session(ctx)
	if err != nil {
		return nil, err
	}
	defer s.finishSession(ctx, session, &err)
	output, err := session.run(ctx, "cat", strings.TrimSuffix(session.root, "/")+"/"+name)
	if err != nil {
		return nil, err
	}
	if int64(len(output)) > maximum {
		return nil, fmt.Errorf("native repository identity object is too large")
	}
	return []byte(output), nil
}

func (s Store) RemoveProfileForTests(ctx context.Context) (err error) {
	session, err := s.session(ctx)
	if err != nil {
		return err
	}
	defer s.finishSession(ctx, session, &err)
	objects, err := profileObjects(ctx, session)
	if err != nil {
		return fmt.Errorf("list vault recovery profiles for test cleanup: %w", err)
	}
	names := make([]string, 0, len(objects))
	for name := range objects {
		if exactTestRecoveryProfileObject(name) {
			names = append(names, name)
		}
	}
	if len(names) == 0 {
		return nil
	}
	sort.Strings(names)
	var cleanupErr error
	for _, name := range names {
		if _, deleteErr := session.run(ctx, "deletefile", "crypt:"+name); deleteErr != nil {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("delete vault recovery profile %q for test cleanup: %w", name, deleteErr))
		}
	}
	verifyCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	remaining, listErr := profileObjects(verifyCtx, session)
	cancel()
	if listErr != nil {
		cleanupErr = errors.Join(cleanupErr, fmt.Errorf("verify vault recovery profile test cleanup: %w", listErr))
		return cleanupErr
	}
	for _, name := range names {
		if _, exists := remaining[name]; exists {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("verify vault recovery profile test cleanup: %q remains", name))
		}
	}
	return cleanupErr
}

func exactTestRecoveryProfileObject(name string) bool {
	if name == canonicalProfileObject || name == previousProfileObject {
		return true
	}
	operationID := strings.TrimPrefix(name, pendingProfilePrefix)
	if operationID == name {
		return false
	}
	parsed, err := uuid.Parse(operationID)
	return err == nil && parsed.String() == operationID
}
