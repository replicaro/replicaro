// Package storageavailability coordinates bounded filesystem identity
// observation for API and scheduler admission. It never probes direct cloud
// connectors or invokes a native backup engine.
package storageavailability

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/storageidentity"
	"github.com/local/replicaro/vaultidentity"
)

const DefaultObservationTimeout = 5 * time.Second
const maxConcurrentBatchObservations = 8

var ErrObserverUnavailable = errors.New("bounded storage observation is unavailable")
var ErrRepositoryStorageUnavailable = errors.New("repository storage is unavailable")
var ErrSourceStorageUnavailable = errors.New("source storage is unavailable")

type RepositoryStorageUnavailableError struct {
	ReasonCode string
}

func (err *RepositoryStorageUnavailableError) Error() string {
	if err == nil || err.ReasonCode == "" {
		return ErrRepositoryStorageUnavailable.Error()
	}
	return ErrRepositoryStorageUnavailable.Error() + ": " + err.ReasonCode
}

func (err *RepositoryStorageUnavailableError) Unwrap() error {
	return ErrRepositoryStorageUnavailable
}

type SourceStorageUnavailableError struct {
	ReasonCode string
	Detail     string
}

type SourceStorageFailureError struct {
	ReasonCode string
	Detail     string
}

type RepositoryStorageFailureError struct {
	ReasonCode string
}

func (err *RepositoryStorageFailureError) Error() string {
	if err == nil || err.ReasonCode == "" {
		return "repository storage validation failed"
	}
	return "repository storage validation failed: " + err.ReasonCode
}

func (err *SourceStorageFailureError) Error() string {
	if err != nil && err.Detail != "" {
		return "source storage validation failed: " + err.Detail
	}
	if err == nil || err.ReasonCode == "" {
		return "source storage validation failed"
	}
	return "source storage validation failed: " + err.ReasonCode
}

func (err *SourceStorageUnavailableError) Error() string {
	if err != nil && err.Detail != "" {
		return ErrSourceStorageUnavailable.Error() + ": " + err.Detail
	}
	if err == nil || err.ReasonCode == "" {
		return ErrSourceStorageUnavailable.Error()
	}
	return ErrSourceStorageUnavailable.Error() + ": " + err.ReasonCode
}

func (err *SourceStorageUnavailableError) Unwrap() error {
	return ErrSourceStorageUnavailable
}

type Observer interface {
	Observe(context.Context, storageidentity.HelperRequest) (storageidentity.HelperResponse, error)
}

type ObserverFunc func(context.Context, storageidentity.HelperRequest) (storageidentity.HelperResponse, error)

func (fn ObserverFunc) Observe(ctx context.Context, request storageidentity.HelperRequest) (storageidentity.HelperResponse, error) {
	return fn(ctx, request)
}

type ProcessObserver struct {
	Runner *storageidentity.ProcessRunner
}

func (observer ProcessObserver) Observe(ctx context.Context, request storageidentity.HelperRequest) (storageidentity.HelperResponse, error) {
	if observer.Runner == nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	return observer.Runner.Run(ctx, request)
}

type Binding struct {
	ConfiguredPath string
	Version        string
	Key            string
	DescriptorJSON string
}

type Resolution struct {
	Path        string
	CheckedAt   time.Time
	ObservedKey string
}

type resolutionUnavailableError struct {
	reason           string
	permissionDenied bool
}

func (err *resolutionUnavailableError) Error() string { return ErrObserverUnavailable.Error() }
func (err *resolutionUnavailableError) Unwrap() error { return ErrObserverUnavailable }

// IsPermissionDenied reports only a typed denial from a valid local storage
// helper response. It does not infer permission from generic observer failures.
func IsPermissionDenied(err error) bool {
	var unavailable *resolutionUnavailableError
	return errors.As(err, &unavailable) && unavailable.permissionDenied
}

type resolutionFailureError struct{ reason string }

func (err *resolutionFailureError) Error() string {
	if err == nil || err.reason == "" {
		return "storage identity validation failed"
	}
	return "storage identity validation failed: " + err.reason
}

func ResolutionReason(err error) string {
	var unavailable *resolutionUnavailableError
	if errors.As(err, &unavailable) && unavailable.reason != "" {
		return unavailable.reason
	}
	var failure *resolutionFailureError
	if errors.As(err, &failure) && failure.reason != "" {
		return failure.reason
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return database.AvailabilityReasonObservationTimeout
	}
	return database.AvailabilityReasonObservationFailed
}

// ClassifySourceResolutionError preserves the scheduling boundary between a
// temporarily absent source and conclusive local binding evidence. Only the
// former may restore an already-admitted scheduled occurrence.
func ClassifySourceResolutionError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return err
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &SourceStorageUnavailableError{ReasonCode: database.AvailabilityReasonObservationTimeout}
	}
	var unavailable *resolutionUnavailableError
	if errors.As(err, &unavailable) {
		return &SourceStorageUnavailableError{ReasonCode: ResolutionReason(err)}
	}
	return &SourceStorageFailureError{ReasonCode: ResolutionReason(err)}
}

var observerState struct {
	sync.RWMutex
	observer Observer
	timeout  time.Duration
}

func Configure(observer Observer, timeout time.Duration) {
	observerState.Lock()
	defer observerState.Unlock()
	observerState.observer = observer
	if timeout <= 0 {
		timeout = DefaultObservationTimeout
	}
	observerState.timeout = timeout
}

func SetObserverForTests(observer Observer, timeout time.Duration) func() {
	observerState.Lock()
	previousObserver, previousTimeout := observerState.observer, observerState.timeout
	observerState.observer = observer
	if timeout <= 0 {
		timeout = DefaultObservationTimeout
	}
	observerState.timeout = timeout
	observerState.Unlock()
	return func() {
		observerState.Lock()
		observerState.observer, observerState.timeout = previousObserver, previousTimeout
		observerState.Unlock()
	}
}

func observe(ctx context.Context, path, objectType string) (storageidentity.HelperResponse, error) {
	return observePath(ctx, path, objectType, false)
}

func observePath(ctx context.Context, path, objectType string, selectSpelling bool) (storageidentity.HelperResponse, error) {
	_, normalizeErr := storageidentity.NormalizeConfiguredPath(path)
	if normalizeErr != nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	observerState.RLock()
	current, timeout := observerState.observer, observerState.timeout
	observerState.RUnlock()
	if current == nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	if timeout <= 0 {
		timeout = DefaultObservationTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request := storageidentity.HelperRequest{
		Version: storageidentity.HelperVersion, Operation: "observe", Connector: "fs", Path: path,
		ObjectType: objectType, SelectSpelling: selectSpelling,
	}
	response, err := current.Observe(bounded, request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return response, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(bounded.Err(), context.DeadlineExceeded) {
			return response, context.DeadlineExceeded
		}
		// Error codes cannot suppress contradictory helper evidence. The
		// process runner enforces this too; alternate observers must preserve
		// the same boundary before a failed candidate can be skipped.
		if response.ErrorCode != "" && (response.Version != storageidentity.HelperVersion ||
			response.Descriptor != nil || response.Key != "" || response.ConfiguredPath != "" ||
			response.ObservedFilesystem != "" || len(response.Filesystems) != 0) {
			return storageidentity.HelperResponse{}, ErrObserverUnavailable
		}
		if response.ErrorCode == database.AvailabilityReasonStorageMissing {
			return response, &resolutionUnavailableError{reason: database.AvailabilityReasonStorageMissing}
		}
		if response.ErrorCode == database.AvailabilityReasonObservationFailed {
			// The helper completed a structurally valid request but could not observe
			// the path (for example, an inaccessible mounted share). Malformed or
			// inconsistent helper responses take the separate fail-closed path below.
			return response, &resolutionUnavailableError{reason: database.AvailabilityReasonObservationFailed}
		}
		if response.ErrorCode == storageidentity.HelperPermissionDeniedCode {
			return response, &resolutionUnavailableError{
				reason: database.AvailabilityReasonObservationFailed, permissionDenied: true,
			}
		}
		return response, ErrObserverUnavailable
	}
	if response.Version != storageidentity.HelperVersion || response.Descriptor == nil || response.Key == "" {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	if err := response.Descriptor.Validate(); err != nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	if _, err := response.ObservationFilesystem(); err != nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	bindingPath, spellingErr := response.BindingPath(request)
	if spellingErr != nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	if response.Descriptor.Kind == storageidentity.KindPathOnly &&
		!storageidentity.PathOnlyMatchesConfiguredPath(*response.Descriptor, bindingPath) {
		// Path-only carries no physical identity that could independently bind a
		// response to its request. Exact configured-path correlation is therefore
		// mandatory before the response can authorize admission or persistence.
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	key, err := response.Descriptor.CanonicalKey()
	if err != nil || key != response.Key {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	return response, nil
}

func bindParent(ctx context.Context, path string) (storageidentity.HelperResponse, error) {
	_, normalizeErr := storageidentity.NormalizeConfiguredPath(path)
	if normalizeErr != nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	observerState.RLock()
	current, timeout := observerState.observer, observerState.timeout
	observerState.RUnlock()
	if current == nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	if timeout <= 0 {
		timeout = DefaultObservationTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	request := storageidentity.HelperRequest{
		Version: storageidentity.HelperVersion, Operation: "bind_parent", Connector: "fs", Path: path,
		SelectSpelling: true,
	}
	response, err := current.Observe(bounded, request)
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
		bounded.Err() == nil && response.Version == storageidentity.HelperVersion &&
		response.ErrorCode == storageidentity.HelperPermissionDeniedCode &&
		response.Descriptor == nil && response.Key == "" && response.ConfiguredPath == "" &&
		response.ObservedFilesystem == "" && len(response.Filesystems) == 0 {
		return storageidentity.HelperResponse{}, &resolutionUnavailableError{
			reason: database.AvailabilityReasonObservationFailed, permissionDenied: true,
		}
	}
	if err != nil || response.Version != storageidentity.HelperVersion || response.Descriptor == nil || response.Key == "" {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	if validateErr := response.Descriptor.Validate(); validateErr != nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	if response.ObservedFilesystem != "" {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	bindingPath, spellingErr := response.BindingPath(request)
	if spellingErr != nil {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	if response.Descriptor.Kind == storageidentity.KindPathOnly &&
		!storageidentity.PathOnlyMatchesConfiguredPath(*response.Descriptor, bindingPath) {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	key, keyErr := response.Descriptor.CanonicalKey()
	if keyErr != nil || key != response.Key {
		return storageidentity.HelperResponse{}, ErrObserverUnavailable
	}
	return response, nil
}

func enumerate(ctx context.Context) ([]storageidentity.MountedFilesystem, error) {
	observerState.RLock()
	current, timeout := observerState.observer, observerState.timeout
	observerState.RUnlock()
	if current == nil {
		return nil, ErrObserverUnavailable
	}
	if timeout <= 0 {
		timeout = DefaultObservationTimeout
	}
	bounded, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	response, err := current.Observe(bounded, storageidentity.HelperRequest{
		Version: storageidentity.HelperVersion, Operation: "enumerate", Connector: "fs",
	})
	if err != nil || response.Version != storageidentity.HelperVersion || response.Descriptor != nil || response.Key != "" {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return nil, context.Canceled
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(bounded.Err(), context.DeadlineExceeded) {
			return nil, context.DeadlineExceeded
		}
		return nil, ErrObserverUnavailable
	}
	for _, mounted := range response.Filesystems {
		if mounted.Path == "" || mounted.Descriptor.Kind == storageidentity.KindPathOnly &&
			(mounted.Descriptor.StorageClass != storageidentity.StorageClassLocal || mounted.Descriptor.MountRoot == "") {
			return nil, ErrObserverUnavailable
		}
		if err := mounted.Descriptor.Validate(); err != nil {
			return nil, ErrObserverUnavailable
		}
	}
	return response.Filesystems, nil
}

func parseExpectedBinding(version, key, descriptorJSON string) (storageidentity.Descriptor, error) {
	expected, err := storageidentity.DecodeBinding(version, key, descriptorJSON)
	if err != nil {
		return storageidentity.Descriptor{}, ErrObserverUnavailable
	}
	return expected, nil
}

func parseExpectedBindingForPath(configured, version, key, descriptorJSON string) (storageidentity.Descriptor, error) {
	expected, err := parseExpectedBinding(version, key, descriptorJSON)
	if err != nil {
		return storageidentity.Descriptor{}, err
	}
	canonical, err := storageidentity.NormalizeConfiguredPath(configured)
	if err != nil || canonical != configured {
		// Fresh input is normalized before binding. A saved path is local
		// authority and must already be canonical; runtime admission never repairs
		// a tampered spelling, regardless of which binding kind was recorded.
		return storageidentity.Descriptor{}, ErrObserverUnavailable
	}
	if expected.Kind == storageidentity.KindPathOnly &&
		!storageidentity.PathOnlyMatchesConfiguredPath(expected, configured) {
		return storageidentity.Descriptor{}, ErrObserverUnavailable
	}
	return expected, nil
}

// ResolvePath follows the single configured, cached, bounded-discovery order.
// Every discovered candidate is admitted only by a normal exact observation;
// zero is unavailable and multiple exact matches fail closed.
func ResolvePath(ctx context.Context, configured, cached, version, key, descriptorJSON string) (Resolution, error) {
	return resolvePath(ctx, configured, cached, version, key, descriptorJSON)
}

func resolvePath(ctx context.Context, configured, cached, version, key, descriptorJSON string) (Resolution, error) {
	observerState.RLock()
	resolverTimeout := observerState.timeout
	observerState.RUnlock()
	if resolverTimeout <= 0 {
		resolverTimeout = DefaultObservationTimeout
	}
	resolverContext, cancel := context.WithTimeout(ctx, resolverTimeout)
	defer cancel()
	ctx = resolverContext
	expected, err := parseExpectedBindingForPath(configured, version, key, descriptorJSON)
	if err != nil {
		return Resolution{}, err
	}
	try := func(candidate string) (Resolution, bool, string, error) {
		if candidate == "" {
			return Resolution{}, false, "", nil
		}
		response, observeErr := observe(ctx, candidate, "directory")
		if observeErr != nil {
			if errors.Is(observeErr, context.DeadlineExceeded) {
				return Resolution{}, false, database.AvailabilityReasonObservationTimeout, nil
			}
			var unavailable *resolutionUnavailableError
			if errors.As(observeErr, &unavailable) {
				return Resolution{}, false, unavailable.reason, nil
			}
			return Resolution{}, false, database.AvailabilityReasonObservationFailed, observeErr
		}
		if response.Key != key {
			return Resolution{}, false, database.AvailabilityReasonIdentityMismatch, nil
		}
		return Resolution{Path: candidate, CheckedAt: time.Now().UTC(), ObservedKey: response.Key}, true, "", nil
	}
	if expected.Kind == storageidentity.KindPathOnly {
		// Path-only intentionally neither downgrades nor upgrades. Any valid
		// exact-path observation proves current availability, but never authorizes
		// a cached alias, discovery, or replacement binding. Stable configured-path
		// bindings continue below and must reproduce their exact stored key.
		response, observeErr := observe(ctx, configured, "directory")
		if observeErr != nil {
			if errors.Is(observeErr, context.Canceled) {
				return Resolution{}, context.Canceled
			}
			if errors.Is(observeErr, context.DeadlineExceeded) {
				return Resolution{}, &resolutionUnavailableError{reason: database.AvailabilityReasonObservationTimeout}
			}
			var unavailable *resolutionUnavailableError
			if errors.As(observeErr, &unavailable) {
				return Resolution{}, unavailable
			}
			return Resolution{}, ErrObserverUnavailable
		}
		actualFilesystem, typeErr := response.ObservationFilesystem()
		if typeErr != nil {
			return Resolution{}, ErrObserverUnavailable
		}
		if actualFilesystem != expected.Filesystem {
			return Resolution{}, &resolutionFailureError{reason: database.AvailabilityReasonIdentityMismatch}
		}
		return Resolution{Path: configured, CheckedAt: time.Now().UTC(), ObservedKey: key}, nil
	}
	result, ok, configuredReason, configuredErr := try(configured)
	if configuredErr != nil {
		return Resolution{}, configuredErr
	}
	if ok {
		return result, nil
	}
	// All validated stable kinds are local volumes or proven network namespaces.
	// Path-only bindings returned above and cannot reach alias discovery.
	if cached != "" && cached != configured {
		result, ok, _, cachedErr := try(cached)
		if cachedErr != nil {
			return Resolution{}, cachedErr
		}
		if ok {
			return result, nil
		}
	}
	mounted, err := enumerate(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return Resolution{}, &resolutionUnavailableError{reason: database.AvailabilityReasonObservationTimeout}
		}
		return Resolution{}, err
	}
	matches := map[string]Resolution{}
	for _, filesystem := range mounted {
		candidate, ok := storageidentity.CandidatePath(expected, filesystem)
		if !ok || candidate == configured || candidate == cached {
			continue
		}
		result, valid, _, candidateErr := try(candidate)
		if candidateErr != nil {
			return Resolution{}, candidateErr
		}
		if valid {
			matches[result.Path] = result
		}
	}
	if len(matches) == 0 {
		reason := configuredReason
		if reason == "" {
			reason = database.AvailabilityReasonStorageMissing
		}
		return Resolution{}, &resolutionUnavailableError{reason: reason}
	}
	if len(matches) > 1 {
		return Resolution{}, &resolutionFailureError{reason: database.AvailabilityReasonIdentityMismatch}
	}
	for _, match := range matches {
		return match, nil
	}
	return Resolution{}, ErrObserverUnavailable
}

// ResolveSourcePath evaluates the one local source binding. Stable identity is
// checked at the configured path first; stable local and network bindings may
// then use runtime aliases. Hardware removability is deliberately irrelevant.
// The saved source remains immutable and the final pre-native identity check
// still validates the frozen alias. This does not change native source scope.
func ResolveSourcePath(ctx context.Context, job models.BackupJob) (Resolution, error) {
	expected, err := parseExpectedBindingForPath(job.Source, job.SourceStorageVersion, job.SourceStorageKey, job.SourceStorageDescriptorJSON)
	if err != nil {
		return Resolution{}, err
	}
	if expected.Kind == storageidentity.KindPathOnly {
		return ResolvePath(ctx, job.Source, "", job.SourceStorageVersion, job.SourceStorageKey, job.SourceStorageDescriptorJSON)
	}
	return resolvePath(ctx, job.Source, job.ResolvedSourcePath, job.SourceStorageVersion,
		job.SourceStorageKey, job.SourceStorageDescriptorJSON)
}

// RepositoryFallbackCandidates returns only exact OS-mounted aliases for an
// eligible filesystem destination. It does not inspect repository contents or
// treat a storage descriptor as admission proof.
func RepositoryFallbackCandidates(ctx context.Context, repo models.Repository) ([]string, error) {
	if repo.Connector != "fs" {
		return nil, nil
	}
	expected, err := parseExpectedBinding(repo.StorageIdentityVersion, repo.StorageIdentityKey, repo.StorageIdentityJSON)
	if err != nil {
		return nil, err
	}
	if expected.StorageClass != storageidentity.StorageClassLocal &&
		expected.StorageClass != storageidentity.StorageClassNetwork {
		return nil, nil
	}
	mounted, err := enumerate(ctx)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{repo.Location: true, repo.ResolvedRepositoryPath: true}
	result := []string{}
	for _, filesystem := range mounted {
		var candidate string
		var ok bool
		if expected.StorageClass == storageidentity.StorageClassLocal {
			candidate, ok = storageidentity.CandidatePathOnLocal(expected, filesystem)
		} else {
			candidate, ok = storageidentity.CandidatePath(expected, filesystem)
		}
		if !ok || seen[candidate] {
			continue
		}
		seen[candidate] = true
		result = append(result, candidate)
	}
	return result, nil
}

func RepositoryAliasesEligible(repo models.Repository) bool {
	if repo.Connector != "fs" {
		return false
	}
	descriptor, err := parseExpectedBinding(repo.StorageIdentityVersion, repo.StorageIdentityKey, repo.StorageIdentityJSON)
	return err == nil && (descriptor.StorageClass == storageidentity.StorageClassLocal ||
		descriptor.StorageClass == storageidentity.StorageClassNetwork)
}

func resolveFilesystemBinding(ctx context.Context, path string, creation bool) (Binding, error) {
	var response storageidentity.HelperResponse
	var err error
	if creation {
		response, err = bindParent(ctx, path)
	} else {
		response, err = observePath(ctx, path, "directory", true)
	}
	if err != nil {
		return Binding{}, err
	}
	descriptor := *response.Descriptor
	if !creation && descriptor.Kind != storageidentity.KindPathOnly {
		actualFilesystem, actualErr := response.ObservationFilesystem()
		if actualErr != nil {
			return Binding{}, ErrObserverUnavailable
		}
		descriptor.ActualFilesystem = actualFilesystem
	}
	descriptorJSON, err := json.Marshal(descriptor)
	if err != nil {
		return Binding{}, ErrObserverUnavailable
	}
	configured := response.ConfiguredPath
	if configured == "" {
		configured = path
	}
	binding := Binding{
		ConfiguredPath: configured,
		Version:        storageidentity.DescriptorVersion, Key: response.Key,
		DescriptorJSON: string(descriptorJSON),
	}
	return binding, nil
}

func ResolveFilesystemBinding(ctx context.Context, path string) (Binding, error) {
	return resolveFilesystemBinding(ctx, path, false)
}

func ResolveFilesystemCreationBinding(ctx context.Context, path string) (Binding, error) {
	return resolveFilesystemBinding(ctx, path, true)
}

func BindRepository(ctx context.Context, repo models.Repository) (models.Repository, error) {
	return bindRepository(ctx, repo, true)
}

// BindExistingRepository is used only when the selected repository must
// already exist. Creation preview/reservation retains deepest-parent binding.
func BindExistingRepository(ctx context.Context, repo models.Repository) (models.Repository, error) {
	return bindRepository(ctx, repo, false)
}

func bindRepository(ctx context.Context, repo models.Repository, creation bool) (models.Repository, error) {
	if repo.Connector != "fs" {
		if repo.StorageIdentityVersion != "" || repo.StorageIdentityKey != "" || repo.StorageIdentityJSON != "" {
			return models.Repository{}, fmt.Errorf("direct connector contains a filesystem storage binding")
		}
		identity, err := vaultidentity.PhysicalIdentityWithOptions(repo.Connector, repo.Location, repo.ConnectorOptions)
		if err != nil {
			return models.Repository{}, err
		}
		repo.CanonicalIdentity = identity
		return repo, nil
	}
	configured, err := storageidentity.NormalizeConfiguredPath(repo.Location)
	if err != nil {
		return models.Repository{}, err
	}
	var binding Binding
	if creation {
		binding, err = ResolveFilesystemCreationBinding(ctx, configured)
	} else {
		binding, err = ResolveFilesystemBinding(ctx, configured)
	}
	if err != nil {
		return models.Repository{}, err
	}
	if descriptor, parseErr := parseExpectedBinding(binding.Version, binding.Key, binding.DescriptorJSON); parseErr != nil {
		return models.Repository{}, parseErr
	} else if descriptor.Kind == storageidentity.KindPathOnly {
		// Filesystem-type continuity is deliberately source-local. Path-only
		// vaults retain their configured-path behavior and identity representation.
		descriptor.Filesystem = "path-only"
		key, keyErr := descriptor.CanonicalKey()
		encoded, marshalErr := json.Marshal(descriptor)
		if keyErr != nil || marshalErr != nil {
			return models.Repository{}, ErrObserverUnavailable
		}
		binding.Key, binding.DescriptorJSON = key, string(encoded)
		repo.Location = descriptor.RelativePath
	}
	repo.Location = binding.ConfiguredPath
	repo.CanonicalIdentity = binding.Key
	repo.StorageIdentityVersion = binding.Version
	repo.StorageIdentityKey = binding.Key
	repo.StorageIdentityJSON = binding.DescriptorJSON
	return repo, nil
}

func BindJobSource(ctx context.Context, job models.BackupJob) (models.BackupJob, error) {
	configured, err := storageidentity.NormalizeConfiguredPath(job.Source)
	if err != nil {
		return models.BackupJob{}, err
	}
	// Select filesystem spelling only before binding. Once saved, this path is
	// also native source scope (notably Kopia SourceInfo); availability must not
	// rewrite it just because the filesystem now reports a different spelling.
	binding, err := ResolveFilesystemBinding(ctx, configured)
	if err != nil {
		return models.BackupJob{}, err
	}
	job.SourceStorageVersion, job.SourceStorageKey, job.SourceStorageDescriptorJSON =
		binding.Version, binding.Key, binding.DescriptorJSON
	// The complete computer-local binding, including any actual filesystem
	// observation, is omitted from portable profiles.
	job.Source = binding.ConfiguredPath
	return job, nil
}

func ObserveSource(ctx context.Context, job models.BackupJob, _ time.Time) database.StorageAvailabilityObservation {
	resolution, err := ResolveSourcePath(ctx, job)
	if err != nil {
		var unavailable *resolutionUnavailableError
		if !errors.As(err, &unavailable) {
			// This run-local flag routes a genuine source error through the existing
			// operation/executor result path. It adds no durable source state and lets
			// the executor reproduce the exact failure before hooks or native work.
			return database.StorageAvailabilityObservation{State: database.StorageUnknown,
				ReasonCode: ResolutionReason(err), CheckedAt: time.Now().UTC(), ConclusiveFailure: true}
		}
	}
	return resolutionObservation(resolution, err)
}

func ObserveRepository(ctx context.Context, repo models.Repository, _ time.Time) database.StorageAvailabilityObservation {
	if repo.Connector != "fs" {
		return database.StorageAvailabilityObservation{
			State: database.StorageAvailable, CheckedAt: time.Now().UTC(),
		}
	}
	// Preliminary filesystem availability is deliberately only an observation.
	// Repository/native/protected identity is proved once after the vault lock is
	// acquired; storage keys are location hints and no longer admit a vault.
	path := repo.Location
	_, err := observe(ctx, path, "directory")
	var configuredUnavailable *resolutionUnavailableError
	if err != nil && RepositoryAliasesEligible(repo) &&
		(errors.As(err, &configuredUnavailable) || errors.Is(err, context.DeadlineExceeded)) &&
		repo.ResolvedRepositoryPath != "" && repo.ResolvedRepositoryPath != repo.Location {
		path = repo.ResolvedRepositoryPath
		_, err = observe(ctx, path, "directory")
	}
	if err != nil {
		var unavailable *resolutionUnavailableError
		if RepositoryAliasesEligible(repo) && (errors.As(err, &unavailable) || errors.Is(err, context.DeadlineExceeded)) {
			// Eligible mounted storage gets one lock-level fallback opportunity even
			// when both saved hints are absent. This is only admission routing: it
			// publishes no alias and claims no native/protected repository proof.
			return database.StorageAvailabilityObservation{State: database.StorageAvailable,
				CheckedAt: time.Now().UTC()}
		}
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) &&
			!errors.As(err, &unavailable) {
			// A malformed or inconsistent observation is not evidence that storage is
			// merely absent. Route a visible failed attempt through the existing
			// executor so hooks and native work cannot run after bad helper evidence.
			return database.StorageAvailabilityObservation{State: database.StorageUnknown,
				ReasonCode: ResolutionReason(err), CheckedAt: time.Now().UTC(), ConclusiveFailure: true}
		}
		return resolutionObservation(Resolution{}, err)
	}
	return database.StorageAvailabilityObservation{State: database.StorageAvailable,
		CheckedAt: time.Now().UTC(), ResolvedPath: path}
}

func resolutionObservation(resolution Resolution, err error) database.StorageAvailabilityObservation {
	checkedAt := time.Now().UTC()
	if err != nil {
		reason := database.AvailabilityReasonObservationFailed
		var unavailable *resolutionUnavailableError
		if errors.As(err, &unavailable) && unavailable.reason != "" {
			reason = unavailable.reason
		}
		if errors.Is(err, context.DeadlineExceeded) {
			reason = database.AvailabilityReasonObservationTimeout
		}
		return database.StorageAvailabilityObservation{State: database.StorageUnavailable, ReasonCode: reason, CheckedAt: checkedAt}
	}
	return database.StorageAvailabilityObservation{State: database.StorageAvailable, CheckedAt: resolution.CheckedAt, ObservedKey: resolution.ObservedKey, ResolvedPath: resolution.Path}
}

// RequireSourceAvailable revalidates the complete persisted filesystem
// binding and freshly observes the exact source identity. Callers use it while
// holding the applicable vault lock immediately before admitting a native
// backup operation.
func RequireSourceAvailable(ctx context.Context, job models.BackupJob) error {
	path := job.Source
	if job.ResolvedSourcePath != "" {
		path = job.ResolvedSourcePath
	}
	return RequireSourcePathAvailable(ctx, job, path)
}

func RequireSourcePathAvailable(ctx context.Context, job models.BackupJob, path string) error {
	expected, err := parseExpectedBindingForPath(job.Source, job.SourceStorageVersion, job.SourceStorageKey, job.SourceStorageDescriptorJSON)
	if err != nil {
		return &SourceStorageFailureError{ReasonCode: database.AvailabilityReasonIdentityMismatch}
	}
	if expected.Kind == storageidentity.KindPathOnly {
		if path != job.Source {
			return &SourceStorageFailureError{ReasonCode: database.AvailabilityReasonIdentityMismatch}
		}
		response, observeErr := observe(ctx, path, "directory")
		if observeErr != nil {
			return ClassifySourceResolutionError(observeErr)
		}
		// This catches a different-type mount replacement, but it is not physical
		// identity: another filesystem reporting the same type remains admissible.
		observedFilesystem, typeErr := response.ObservationFilesystem()
		if typeErr != nil {
			return &SourceStorageFailureError{ReasonCode: database.AvailabilityReasonObservationFailed}
		}
		if observedFilesystem != expected.Filesystem {
			return &SourceStorageFailureError{
				ReasonCode: database.AvailabilityReasonIdentityMismatch,
				Detail:     "path-only source filesystem type changed since it was locally bound",
			}
		}
		return nil
	}
	response, observeErr := observe(ctx, path, "directory")
	if observeErr != nil {
		return ClassifySourceResolutionError(observeErr)
	}
	if response.Key != job.SourceStorageKey {
		// A stable local/network source that no longer matches at its frozen path
		// is absent for this occurrence; later resolution may find a valid alias.
		// Never switch paths during this final admission or silently downgrade it.
		return &SourceStorageUnavailableError{ReasonCode: database.AvailabilityReasonIdentityMismatch}
	}
	return nil
}

// RequireRepositoryAvailable remains an engine callback for established
// operation sessions. Full repository/native/protected identity is admitted
// once under the vault lock; repeating physical observations here would
// reintroduce address continuity as an accidental per-command gate.
func RequireRepositoryAvailable(ctx context.Context, repo models.Repository) error {
	_ = ctx
	_ = repo
	return nil
}

// ObserveBackupSet observes one source and its selected targets concurrently,
// with a fixed worker bound. This keeps the exact observations close to the
// admission transaction without creating unbounded helper processes.
func ObserveBackupSet(
	ctx context.Context,
	job models.BackupJob,
	repositories []models.Repository,
) (database.StorageAvailabilityObservation, []database.TargetAvailabilityObservation) {
	targets := make([]database.TargetAvailabilityObservation, len(repositories))
	type workItem struct {
		source bool
		index  int
	}
	work := make(chan workItem)
	workerCount := min(maxConcurrentBatchObservations, len(repositories)+1)
	var workers sync.WaitGroup
	workers.Add(workerCount)
	var source database.StorageAvailabilityObservation
	for range workerCount {
		go func() {
			defer workers.Done()
			for item := range work {
				if item.source {
					source = ObserveSource(ctx, job, time.Time{})
					continue
				}
				repo := repositories[item.index]
				targets[item.index] = database.TargetAvailabilityObservation{
					RepositoryID: repo.ID,
					Availability: ObserveRepository(ctx, repo, time.Time{}),
				}
			}
		}()
	}
	work <- workItem{source: true}
	for index := range repositories {
		work <- workItem{index: index}
	}
	close(work)
	workers.Wait()
	return source, targets
}

func observeExpectedFilesystem(ctx context.Context, path, expectedKey string) database.StorageAvailabilityObservation {
	response, err := observe(ctx, path, "directory")
	checkedAt := time.Now().UTC()
	if err != nil {
		reason := database.AvailabilityReasonObservationFailed
		if errors.Is(err, context.DeadlineExceeded) {
			reason = database.AvailabilityReasonObservationTimeout
		} else if response.ErrorCode == database.AvailabilityReasonStorageMissing {
			reason = database.AvailabilityReasonStorageMissing
		}
		return database.StorageAvailabilityObservation{
			State: database.StorageUnavailable, ReasonCode: reason, CheckedAt: checkedAt,
		}
	}
	observedKey := response.Key
	if observedKey != expectedKey {
		return database.StorageAvailabilityObservation{
			State: database.StorageUnavailable, ReasonCode: database.AvailabilityReasonIdentityMismatch,
			CheckedAt: checkedAt, ObservedKey: observedKey,
		}
	}
	return database.StorageAvailabilityObservation{
		State: database.StorageAvailable, CheckedAt: checkedAt, ObservedKey: observedKey,
	}
}
