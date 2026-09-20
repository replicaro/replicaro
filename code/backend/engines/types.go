package engines

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/local/replicaro/models"
)

func SizeBytes(value string) int64 {
	fields := strings.Fields(value)
	if len(fields) == 0 {
		return 0
	}
	number, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || number < 0 {
		return 0
	}
	multiplier := float64(1)
	if len(fields) > 1 {
		switch strings.ToLower(fields[1]) {
		case "kb":
			multiplier = 1e3
		case "kib":
			multiplier = 1024
		case "mb":
			multiplier = 1e6
		case "mib":
			multiplier = 1024 * 1024
		case "gb":
			multiplier = 1e9
		case "gib":
			multiplier = 1024 * 1024 * 1024
		case "tb":
			multiplier = 1e12
		case "tib":
			multiplier = 1024 * 1024 * 1024 * 1024
		}
	}
	return int64(number * multiplier)
}

const (
	ResticID = "restic"
	KopiaID  = "kopia"
)

type StorageModel string

const (
	StorageModelSnapshotRepository StorageModel = "snapshot_repository"
)

var ErrUnsupported = errors.New("unsupported backup engine")

type ProviderDescriptor struct {
	ID          string   `json:"id"`
	Label       string   `json:"label"`
	Description string   `json:"description"`
	Supported   bool     `json:"supported"`
	Fields      []string `json:"fields,omitempty"`
}

type Descriptor struct {
	ID                   string               `json:"id"`
	Name                 string               `json:"name"`
	Compression          string               `json:"compression"`
	Encryption           string               `json:"encryption"`
	Providers            []ProviderDescriptor `json:"providers"`
	Installed            bool                 `json:"installed"`
	Path                 string               `json:"path"`
	Version              string               `json:"version"`
	Error                string               `json:"error,omitempty"`
	CompatibilityWarning string               `json:"compatibilityWarning,omitempty"`
	Operations           []string             `json:"operations"`
	JobSettings          []string             `json:"jobSettings"`
	Capabilities         Capabilities         `json:"capabilities"`
}

type DeletionInvocation string
type ResultGranularity string

const (
	DeletionSingle DeletionInvocation = "single"
	DeletionBatch  DeletionInvocation = "batch"

	ResultPerID     ResultGranularity = "per_id"
	ResultBatchOnly ResultGranularity = "batch_only"
)

type NativeOperationCapability struct {
	Supported bool   `json:"supported"`
	Label     string `json:"label"`
	Scope     string `json:"scope"`
}

type RestoreCapability struct {
	FullSnapshot     bool                  `json:"fullSnapshot"`
	SinglePath       bool                  `json:"singlePath"`
	MultiPath        bool                  `json:"multiPath"`
	OriginalLocation bool                  `json:"originalLocation"`
	ConflictModes    []RestoreConflictMode `json:"conflictModes"`
	DestinationScope string                `json:"destinationScope"`
}

type RestoreConflictMode struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Default     bool   `json:"default"`
}

type DeletionCapability struct {
	Supported         bool               `json:"supported"`
	Invocation        DeletionInvocation `json:"invocation"`
	ResultGranularity ResultGranularity  `json:"resultGranularity"`
}

type Capabilities struct {
	StorageModel        StorageModel              `json:"storageModel"`
	Backup              NativeOperationCapability `json:"backup"`
	Restore             RestoreCapability         `json:"restore"`
	Verification        NativeOperationCapability `json:"verification"`
	IntegrityCheck      NativeOperationCapability `json:"integrityCheck"`
	Retention           NativeOperationCapability `json:"retention"`
	Deletion            DeletionCapability        `json:"deletion"`
	Maintenance         NativeOperationCapability `json:"maintenance"`
	NativeEncryption    bool                      `json:"nativeEncryption"`
	NativeCompression   bool                      `json:"nativeCompression"`
	NativeDeduplication bool                      `json:"nativeDeduplication"`
}

func capabilitiesFor(id string) Capabilities {
	base := Capabilities{
		StorageModel:     StorageModelSnapshotRepository,
		Backup:           NativeOperationCapability{Supported: true, Label: "Native backup", Scope: "The selected engine owns traversal, capture, metadata, and the native result."},
		IntegrityCheck:   NativeOperationCapability{Supported: true, Label: "Repository integrity check", Scope: "Native repository check; scope and guarantees remain engine-specific."},
		Retention:        NativeOperationCapability{Supported: true, Label: "Native retention policy", Scope: "Replicaro passes the saved job intent to the selected engine's native retention scope."},
		Deletion:         DeletionCapability{Supported: true, Invocation: DeletionBatch, ResultGranularity: ResultBatchOnly},
		Maintenance:      NativeOperationCapability{Supported: true, Label: "Space reclamation", Scope: "Native maintenance operation."},
		NativeEncryption: true, NativeCompression: true, NativeDeduplication: true,
	}
	switch id {
	case ResticID:
		base.Restore = RestoreCapability{FullSnapshot: true, SinglePath: true, ConflictModes: []RestoreConflictMode{
			{ID: "always", Label: "Always overwrite", Description: "Restic passes its native --overwrite always mode.", Default: true},
			{ID: "never", Label: "Never overwrite", Description: "Restic passes its native --overwrite never mode.", Default: false},
		}, DestinationScope: "Restic writes directly below the selected native target. Replicaro exposes Restic's native always and never overwrite modes."}
		base.Verification = NativeOperationCapability{Supported: true, Label: "Restic repository verification", Scope: "Native check --read-data content verification, optionally filtered to one snapshot."}
	case KopiaID:
		base.Restore = RestoreCapability{FullSnapshot: true, SinglePath: true, ConflictModes: []RestoreConflictMode{
			{ID: "overwrite", Label: "Overwrite", Description: "Kopia applies its native overwrite behavior.", Default: true},
			{ID: "no-overwrite", Label: "Do not overwrite", Description: "Kopia passes its native no-overwrite flags.", Default: false},
		}, DestinationScope: "Kopia writes the selected snapshot object directly to the selected destination using its native conflict flags."}
		base.Verification = NativeOperationCapability{Supported: true, Label: "Kopia snapshot verification", Scope: "Native snapshot verify with full file-content verification."}
	}
	return base
}

type RepositoryAvailabilityCheck func(context.Context, models.Repository) error

func ValidateRestoreCapability(descriptor Descriptor, options RestoreOptions) error {
	capability := descriptor.Capabilities.Restore
	if !capability.FullSnapshot {
		return fmt.Errorf("%w: native full-snapshot restore is unavailable", ErrUnsupported)
	}
	if options.Selection != "" && !capability.SinglePath {
		return fmt.Errorf("%w: native selected-path restore is unavailable", ErrUnsupported)
	}
	if options.OriginalLocation && !capability.OriginalLocation {
		return fmt.Errorf("%w: native original-location restore is unavailable", ErrUnsupported)
	}
	if options.ConflictMode == "" {
		return fmt.Errorf("%w: restore conflict mode is required", ErrUnsupported)
	}
	matches := 0
	for _, mode := range capability.ConflictModes {
		if mode.ID == options.ConflictMode {
			matches++
		}
	}
	if matches != 1 {
		return fmt.Errorf("%w: restore conflict mode %q is not declared by the selected engine", ErrUnsupported, options.ConflictMode)
	}
	return nil
}

type BackupOptions struct {
	Tag            string
	OwnerProfileID string
	OwnerJobID     string
	// LogicalSource preserves Kopia's immutable job history identity when the
	// actual filesystem input is a freshly verified local alias.
	LogicalSource string
	Excludes      []string
	Verify        bool
	Settings      models.EngineJobSettings
}

// RetentionPolicy is the compact job intent translated by each engine. A zero
// Latest means keep all; optional tiers remain dormant in that mode.
type RetentionPolicy struct {
	Latest  int
	Hourly  *int
	Daily   *int
	Weekly  *int
	Monthly *int
	Yearly  *int
}

type nativeRetentionEngine interface {
	ApplyRetention(context.Context, models.Repository, string, RetentionPolicy) (string, error)
}

// ApplyNativeRetention invokes the selected engine's native policy boundary.
// Kopia applies retention inside snapshot create and therefore intentionally
// does not implement this separate operation.
func ApplyNativeRetention(ctx context.Context, engine Engine, repo models.Repository, jobUUID string, policy RetentionPolicy) (string, error) {
	target := engine
	if public, ok := engine.(*publicEngine); ok {
		if repo.Engine != ResticID {
			return "", fmt.Errorf("%w: separate native retention is unavailable for %s", ErrUnsupported, repo.Engine)
		}
		ctx = repositoryProcessContext(ctx, repo, public.availabilityCheck)
		if err := public.autoUnlockRestic(ctx, repo); err != nil {
			return "", resticPreparationFailure(err)
		}
		target = public.Engine
	}
	retention, ok := target.(nativeRetentionEngine)
	if !ok {
		return "", fmt.Errorf("%w: separate native retention is unavailable for %s", ErrUnsupported, repo.Engine)
	}
	output, err := retention.ApplyRetention(ctx, repo, jobUUID, policy)
	if repo.Engine == ResticID {
		err = resticRequestedOperationError(err)
	}
	return output, err
}

type RestoreOptions struct {
	// Kopia-only request-local naming and presentation. Neither changes native output.
	KopiaFileNames    *KopiaRestoreFileNames
	ReportDestination func(string)
	Destination       string
	Selection         string
	NativeRoot        models.SnapshotSourceRoot
	// ExactSourceRoot is set only after the restore coordinator has established
	// that a whole restore is the one authoritative source of a managed
	// snapshot. NativeRoot alone is rebuildable addressing identity and must
	// never be allowed to narrow whole-snapshot scope.
	ExactSourceRoot  bool
	OriginalLocation bool
	ConflictMode     string
}

type Engine interface {
	ID() string
	Descriptor() Descriptor
	Info(context.Context, models.Repository) (string, error)
	Create(context.Context, models.Repository) (string, error)
	Backup(context.Context, models.Repository, string, BackupOptions) (models.Snapshot, string, error)
	ListSnapshots(context.Context, models.Repository) ([]models.Snapshot, string, error)
	ListPath(context.Context, models.Repository, string, string) ([]models.SnapshotEntry, string, error)
	ListPathRecursive(context.Context, models.Repository, string, string) ([]models.SnapshotEntry, string, error)
	Restore(context.Context, models.Repository, string, RestoreOptions) (string, error)
	DeleteSnapshot(context.Context, models.Repository, string) (string, error)
	Check(context.Context, models.Repository, string) (string, error)
	Maintenance(context.Context, models.Repository) (string, error)
}

type NativeDeletionResult struct {
	Invocation        DeletionInvocation       `json:"invocation"`
	ResultGranularity ResultGranularity        `json:"resultGranularity"`
	RequestedIDs      []string                 `json:"requestedIds"`
	Output            string                   `json:"output"`
	PerIDResults      []NativeDeletionIDResult `json:"perIdResults,omitempty"`
}

type NativeDeletionIDResult struct {
	SnapshotID string `json:"snapshotId"`
	Status     string `json:"status"`
	Output     string `json:"output,omitempty"`
}

func ValidateDeletionCapability(engine Engine) (DeletionCapability, error) {
	capability := engine.Descriptor().Capabilities.Deletion
	if !capability.Supported {
		return capability, fmt.Errorf("%w: native snapshot deletion is not declared supported", ErrUnsupported)
	}
	switch capability.Invocation {
	case DeletionSingle:
		if capability.ResultGranularity != ResultPerID && capability.ResultGranularity != ResultBatchOnly {
			return capability, fmt.Errorf("native deletion result granularity is missing or invalid")
		}
	case DeletionBatch:
		switch capability.ResultGranularity {
		case ResultBatchOnly:
			if _, ok := engine.(batchDeletionEngine); !ok {
				return capability, fmt.Errorf("%w: descriptor declares native batch deletion but adapter does not implement it", ErrUnsupported)
			}
		case ResultPerID:
			if _, ok := engine.(batchDeletionPerIDEngine); !ok {
				return capability, fmt.Errorf("%w: descriptor declares native batch deletion with per-ID results but adapter does not implement it", ErrUnsupported)
			}
		default:
			return capability, fmt.Errorf("native deletion result granularity is missing or invalid")
		}
		if _, batchOnly := engine.(batchDeletionEngine); !batchOnly {
			if _, perID := engine.(batchDeletionPerIDEngine); !perID {
				return capability, fmt.Errorf("%w: descriptor declares native batch deletion but adapter does not implement it", ErrUnsupported)
			}
		}
	default:
		return capability, fmt.Errorf("native deletion invocation is missing or invalid")
	}
	return capability, nil
}

type batchDeletionEngine interface {
	DeleteSnapshots(context.Context, models.Repository, []string) (string, error)
}

// batchDeletionPerIDEngine is separate from batchDeletionEngine so an adapter
// cannot accidentally manufacture per-ID outcomes from batch-only native
// output. Implementations return the engine's native per-ID results.
type batchDeletionPerIDEngine interface {
	DeleteSnapshotsWithResults(context.Context, models.Repository, []string) (string, []NativeDeletionIDResult, error)
}

type freshSnapshotLister interface {
	ListSnapshotsFresh(context.Context, models.Repository) ([]models.Snapshot, string, error)
}

// ListSnapshotsFresh requires an engine-native listing that bypasses any
// wrapper operation cache. Engines without such a cache use their normal
// native listing.
func ListSnapshotsFresh(ctx context.Context, engine Engine, repo models.Repository) ([]models.Snapshot, string, error) {
	if fresh, ok := engine.(freshSnapshotLister); ok {
		return fresh.ListSnapshotsFresh(ctx, repo)
	}
	return engine.ListSnapshots(ctx, repo)
}

func DeleteSnapshots(ctx context.Context, engine Engine, repo models.Repository, ids []string) (NativeDeletionResult, error) {
	capability, capabilityErr := ValidateDeletionCapability(engine)
	result := NativeDeletionResult{
		Invocation: capability.Invocation, ResultGranularity: capability.ResultGranularity,
		RequestedIDs: append([]string(nil), ids...),
	}
	if capabilityErr != nil {
		return result, capabilityErr
	}
	if len(ids) == 0 {
		return result, fmt.Errorf("native deletion requires at least one snapshot ID")
	}
	for _, id := range ids {
		if err := ValidateSnapshotIDArgument(id); err != nil {
			return result, err
		}
	}
	if capability.Invocation == DeletionBatch {
		if capability.ResultGranularity == ResultPerID {
			batch := engine.(batchDeletionPerIDEngine)
			output, perID, err := batch.DeleteSnapshotsWithResults(ctx, repo, ids)
			result.Output = output
			result.PerIDResults = append([]NativeDeletionIDResult(nil), perID...)
			if validationErr := validateNativeDeletionIDResults(ids, perID); validationErr != nil {
				return result, errors.Join(err, validationErr)
			}
			for _, nativeResult := range perID {
				if nativeResult.Status != "succeeded" {
					return result, errors.Join(err, fmt.Errorf("native batch deletion reported failed snapshot %s", nativeResult.SnapshotID))
				}
			}
			return result, err
		}
		batch := engine.(batchDeletionEngine)
		result.Output, capabilityErr = batch.DeleteSnapshots(ctx, repo, ids)
		return result, capabilityErr
	}
	var outputs []string
	for index, id := range ids {
		output, err := engine.DeleteSnapshot(ctx, repo, id)
		outputs = append(outputs, output)
		if err != nil {
			status, _, _, _, _, known := RequestedOperationOutcome(err)
			if known && status == RequestedOperationSucceeded {
				if capability.ResultGranularity == ResultPerID {
					result.PerIDResults = append(result.PerIDResults, NativeDeletionIDResult{SnapshotID: id, Status: "succeeded", Output: output})
					for _, skipped := range ids[index+1:] {
						result.PerIDResults = append(result.PerIDResults, NativeDeletionIDResult{SnapshotID: skipped, Status: "skipped"})
					}
				}
				result.Output = strings.Join(outputs, "\n")
				return result, err
			}
			if capability.ResultGranularity == ResultPerID {
				result.PerIDResults = append(result.PerIDResults, NativeDeletionIDResult{SnapshotID: id, Status: "failed", Output: output})
				for _, skipped := range ids[index+1:] {
					result.PerIDResults = append(result.PerIDResults, NativeDeletionIDResult{SnapshotID: skipped, Status: "skipped"})
				}
			}
			result.Output = strings.Join(outputs, "\n")
			return result, err
		}
		if capability.ResultGranularity == ResultPerID {
			result.PerIDResults = append(result.PerIDResults, NativeDeletionIDResult{SnapshotID: id, Status: "succeeded", Output: output})
		}
	}
	result.Output = strings.Join(outputs, "\n")
	return result, nil
}

func SuccessfulNativeDeletionIDs(result NativeDeletionResult) ([]string, error) {
	if result.ResultGranularity == ResultPerID {
		if err := validateNativeDeletionIDResults(result.RequestedIDs, result.PerIDResults); err != nil {
			return nil, err
		}
		successful := make([]string, 0, len(result.PerIDResults))
		for _, nativeResult := range result.PerIDResults {
			if nativeResult.Status == "succeeded" {
				successful = append(successful, nativeResult.SnapshotID)
			}
		}
		return successful, nil
	}
	return nil, fmt.Errorf("native deletion result does not expose trustworthy per-ID success")
}

func validateNativeDeletionIDResults(requested []string, results []NativeDeletionIDResult) error {
	if len(results) != len(requested) {
		return fmt.Errorf("native batch per-ID result count does not match requested snapshot IDs")
	}
	for index, result := range results {
		if result.SnapshotID != requested[index] {
			return fmt.Errorf("native batch per-ID result identity does not match requested snapshot IDs")
		}
		switch result.Status {
		case "succeeded", "failed":
		default:
			return fmt.Errorf("native batch per-ID result status is missing or invalid")
		}
	}
	return nil
}

// ValidateSnapshotIDArgument rejects values that a native CLI could interpret
// as options or multiple arguments. Exact canonical membership is separately
// established from a native listing by the locked orchestration layer.
func ValidateSnapshotIDArgument(id string) error {
	if id == "" || strings.TrimSpace(id) != id || strings.HasPrefix(id, "-") ||
		strings.ContainsAny(id, "\x00\r\n\t ") {
		return fmt.Errorf("invalid canonical snapshot ID")
	}
	return nil
}

// SnapshotIndexItem carries one public snapshot plus an operation-scoped,
// engine-native reference. NativeReference is never serialized or persisted.
type SnapshotIndexItem struct {
	Snapshot              models.Snapshot
	NativeContentIdentity string `json:"-"`
	NativeReference       any    `json:"-"`
}

// SnapshotIndexSession lets metadata reconciliation reuse one repository
// listing and any validated command context while it indexes missing snapshots.
// ListPathRecursive must support up to four concurrent read-only calls for
// distinct items. Close runs only after every call has returned. Engines may
// implement SnapshotIndexEngine without expanding the public Engine contract
// used by integrations and tests.
type SnapshotIndexSession interface {
	Snapshots() []SnapshotIndexItem
	ListPathRecursive(context.Context, SnapshotIndexItem, string) ([]models.SnapshotEntry, string, error)
	Close() error
}

type SnapshotIndexEngine interface {
	BeginSnapshotIndex(context.Context, models.Repository) (SnapshotIndexSession, string, error)
}

type CommandError struct {
	Engine string
	Output string
	Err    error
}

func (e *CommandError) Error() string {
	if strings.TrimSpace(e.Output) == "" {
		return fmt.Sprintf("%s engine command failed: %v", e.Engine, e.Err)
	}
	return fmt.Sprintf("%s engine command failed: %s", e.Engine, strings.TrimSpace(e.Output))
}

func (e *CommandError) Unwrap() error { return e.Err }

func operationNames() []string {
	return []string{"status", "version", "create", "connect", "info", "backup", "list", "restore", "delete", "verify", "maintenance"}
}
