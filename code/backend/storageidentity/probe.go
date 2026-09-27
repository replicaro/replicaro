package storageidentity

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"unicode"
)

const HelperVersion = "storage_probe_v5"

// Probe kinds. A source probe also reads one directory entry, because a
// backup needs a readable folder and an empty folder is still readable. A
// vault_create probe walks up to the deepest existing parent and reports the
// missing tail; the vault folder itself is created later by the native engine.
const (
	ProbeSource      = "source"
	ProbeVault       = "vault"
	ProbeVaultCreate = "vault_create"
)

// Access results.
const (
	AccessOK      = "ok"
	AccessMissing = "missing" // not found, or not a directory
	AccessDenied  = "denied"
	AccessFailed  = "failed" // any other OS error; the storage is treated as temporarily unreachable
)

// Recorded mount point states reported when the folder itself is missing.
const (
	MountPresent = "present"
	MountAbsent  = "absent"
	MountDenied  = "denied"
	MountFailed  = "failed"
)

// Step labels are fixed support-diagnostic vocabulary. They are exported with
// numeric OS error codes only; paths, share names, and OS messages are never
// part of a diagnostic.
const (
	StepOpenFolder      = "open_folder"
	StepReadEntry       = "read_entry"
	StepMountPoint      = "mount_point"
	StepFilesystemType  = "filesystem_type"
	StepMountPointCheck = "mount_point_check"
)

type HelperRequest struct {
	Version   string `json:"version"`
	Operation string `json:"operation"`
	Kind      string `json:"kind"`
	Path      string `json:"path"`
	// MountPoint is the recorded mount point. When the folder turns out to be
	// missing, the same request checks whether this mount point is still
	// mounted, so the caller can tell "drive unplugged" from "folder deleted".
	MountPoint string `json:"mount_point,omitempty"`
}

// FactFailure records why a fact could not be determined for an opened
// folder. It is diagnostic only.
type FactFailure struct {
	Step string `json:"step"`
	Code int64  `json:"code"`
}

// MountCheck is the result of probing the recorded mount point.
type MountCheck struct {
	State string `json:"state"`
	Step  string `json:"step,omitempty"`
	Code  int64  `json:"code,omitempty"`
}

type HelperResponse struct {
	Version       string        `json:"version"`
	Access        string        `json:"access"`
	Step          string        `json:"step,omitempty"`
	Code          int64         `json:"code,omitempty"`
	MountPoint    string        `json:"mount_point,omitempty"`
	Filesystem    string        `json:"filesystem,omitempty"`
	FactFailures  []FactFailure `json:"fact_failures,omitempty"`
	MissingTail   string        `json:"missing_tail,omitempty"`
	RecordedMount *MountCheck   `json:"recorded_mount,omitempty"`
}

// Facts returns the facts observed for the opened folder.
func (response HelperResponse) Facts() Facts {
	return Facts{MountPoint: response.MountPoint, Filesystem: response.Filesystem}
}

// FactFailure returns the diagnostic for one missing fact, if any.
func (response HelperResponse) FactFailure(step string) (FactFailure, bool) {
	for _, failure := range response.FactFailures {
		if failure.Step == step {
			return failure, true
		}
	}
	return FactFailure{}, false
}

// ExecuteRequest performs one probe in the current process. The helper binary
// calls it; tests may call it directly. A returned error means the request
// itself was invalid, not that the storage could not be observed.
func ExecuteRequest(request HelperRequest) (HelperResponse, error) {
	if err := validateHelperRequest(request); err != nil {
		return HelperResponse{}, err
	}
	var response HelperResponse
	if request.Kind == ProbeVaultCreate {
		response = probeCreationParent(request.Path)
	} else {
		response = probeFolder(request.Path, request.Kind == ProbeSource)
		if response.Access == AccessMissing && request.MountPoint != "" {
			check := checkRecordedMount(request.MountPoint)
			response.RecordedMount = &check
		}
	}
	response.Version = HelperVersion
	return response, nil
}

func probeFolder(path string, readEntry bool) HelperResponse {
	file, err := openDirectory(path)
	if err != nil {
		return failedAccess(StepOpenFolder, err)
	}
	defer file.Close()
	if readEntry {
		// Proves the folder can actually be listed. The entry itself is
		// discarded immediately; entry names are never returned or logged.
		if _, err := file.ReadDir(1); err != nil && !errors.Is(err, io.EOF) {
			response := failedAccess(StepReadEntry, err)
			if response.Access == AccessMissing {
				// The folder was opened a moment ago. A not-found error while
				// listing it is an unreachable-storage symptom (for example a
				// share that dropped), not proof that the folder is gone.
				response.Access = AccessFailed
			}
			return response
		}
	}
	return openedResponse(file, path)
}

func probeCreationParent(path string) HelperResponse {
	candidate := path
	tail := []string{}
	for {
		file, err := openDirectory(candidate)
		if err == nil {
			defer file.Close()
			response := openedResponse(file, path)
			for left, right := 0, len(tail)-1; left < right; left, right = left+1, right-1 {
				tail[left], tail[right] = tail[right], tail[left]
			}
			response.MissingTail = strings.Join(tail, "/")
			return response
		}
		access, _ := classifyOpenError(err)
		if access != AccessMissing || errors.Is(err, errNotDirectory) {
			// Only "does not exist" walks further up. A file in the way, a
			// denied parent, or an unreachable share ends the walk here.
			return failedAccess(StepOpenFolder, err)
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return failedAccess(StepOpenFolder, err)
		}
		tail = append(tail, filepath.Base(candidate))
		candidate = parent
	}
}

func openedResponse(file *os.File, path string) HelperResponse {
	facts, failures := observeFacts(file, path)
	facts, failures = sanitizeFacts(facts, failures)
	return HelperResponse{Access: AccessOK, MountPoint: facts.MountPoint,
		Filesystem: facts.Filesystem, FactFailures: failures}
}

// sanitizeFacts drops a fact that cannot be stored safely (for example a mount
// point containing a control character) and records it as undetermined, so
// the main process never has to reject an otherwise successful probe.
func sanitizeFacts(facts Facts, failures []FactFailure) (Facts, []FactFailure) {
	facts.Filesystem = NormalizeFilesystem(facts.Filesystem)
	if facts.MountPoint != "" && (Facts{MountPoint: facts.MountPoint}).Validate() != nil {
		facts.MountPoint = ""
		failures = append(failures, FactFailure{Step: StepMountPoint})
	}
	if facts.Filesystem != "" && (Facts{Filesystem: facts.Filesystem}).Validate() != nil {
		facts.Filesystem = ""
		failures = append(failures, FactFailure{Step: StepFilesystemType})
	}
	return facts, failures
}

// checkRecordedMount decides whether the recorded mount point is still
// mounted: it must open, and its own mount point must be itself. On Linux and
// macOS an unmounted share usually leaves an empty directory behind at the
// mount point; that directory opens fine but belongs to the parent mount, so
// it correctly reports "absent".
func checkRecordedMount(mountPoint string) MountCheck {
	file, err := openDirectory(mountPoint)
	if err != nil {
		access, code := classifyOpenError(err)
		switch access {
		case AccessMissing:
			return MountCheck{State: MountAbsent, Step: StepMountPointCheck, Code: code}
		case AccessDenied:
			return MountCheck{State: MountDenied, Step: StepMountPointCheck, Code: code}
		default:
			return MountCheck{State: MountFailed, Step: StepMountPointCheck, Code: code}
		}
	}
	defer file.Close()
	facts, failures := observeFacts(file, mountPoint)
	if facts.MountPoint == "" {
		code := int64(0)
		for _, failure := range failures {
			if failure.Step == StepMountPoint {
				code = failure.Code
			}
		}
		return MountCheck{State: MountFailed, Step: StepMountPointCheck, Code: code}
	}
	if SameMountPoint(facts.MountPoint, mountPoint) {
		return MountCheck{State: MountPresent}
	}
	return MountCheck{State: MountAbsent}
}

var errNotDirectory = errors.New("storage path is not a directory")

func openDirectory(path string) (*os.File, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	if !info.IsDir() {
		_ = file.Close()
		return nil, errNotDirectory
	}
	return file, nil
}

func failedAccess(step string, err error) HelperResponse {
	access, code := classifyOpenError(err)
	return HelperResponse{Access: access, Step: step, Code: code}
}

// classifyOpenError maps an OS error to the probe vocabulary. Only
// not-found/not-a-directory is "missing" and only access denied is "denied";
// every other OS error (ESTALE, ENOTCONN, EIO, EHOSTDOWN, Windows
// ERROR_NOT_READY, ERROR_BAD_NETPATH, ERROR_NETNAME_DELETED, ...) is "failed",
// which the caller treats as temporarily unreachable storage and pauses on.
// The platform files decide which numeric codes belong to which group; Go's
// errors.Is(err, fs.ErrNotExist) is deliberately not used because on Windows
// it also matches ERROR_BAD_NETPATH, an offline server rather than a missing
// folder.
func classifyOpenError(err error) (string, int64) {
	if errors.Is(err, errNotDirectory) {
		return AccessMissing, notDirectoryCode
	}
	code, ok := osErrorCode(err)
	if !ok {
		return AccessFailed, 0
	}
	return classifyErrorCode(code), code
}

// IsAccessDenied reports whether err is an OS error the probe classifies as
// access denied (EACCES/EPERM, or on Windows ERROR_ACCESS_DENIED and the
// share credential failures). Vault admission uses it for the reads that
// follow a successful probe, so a denied read there is an error, as it is in
// the probe itself, rather than a pause.
func IsAccessDenied(err error) bool {
	access, _ := classifyOpenError(err)
	return access == AccessDenied
}

func validateHelperRequest(request HelperRequest) error {
	if request.Version != HelperVersion || request.Operation != "probe" {
		return fmt.Errorf("unsupported storage helper request")
	}
	switch request.Kind {
	case ProbeSource, ProbeVault, ProbeVaultCreate:
	default:
		return fmt.Errorf("unsupported storage probe kind")
	}
	if err := ValidateBindingPath(request.Path); err != nil {
		return fmt.Errorf("storage probe path is invalid: %w", err)
	}
	if request.MountPoint != "" {
		if request.Kind == ProbeVaultCreate {
			return fmt.Errorf("a creation probe has no recorded mount point")
		}
		if len(request.MountPoint) > maxFactBytes || strings.IndexFunc(request.MountPoint, unicode.IsControl) >= 0 ||
			strings.TrimSpace(request.MountPoint) == "" {
			return fmt.Errorf("recorded mount point is invalid")
		}
	}
	return nil
}

// ValidateHelperResponse is the parent-side integrity check. Anything the
// helper returns outside this exact shape is a protocol failure, which the
// caller reports as an error rather than as missing or unreachable storage.
func ValidateHelperResponse(request HelperRequest, response HelperResponse) error {
	if response.Version != HelperVersion {
		return fmt.Errorf("unsupported storage helper response")
	}
	facts := response.Facts()
	if err := facts.Validate(); err != nil {
		return fmt.Errorf("storage helper facts are invalid: %w", err)
	}
	for _, failure := range response.FactFailures {
		if failure.Step != StepMountPoint && failure.Step != StepFilesystemType {
			return fmt.Errorf("storage helper fact failure is invalid")
		}
	}
	if response.MissingTail != "" && (request.Kind != ProbeVaultCreate || response.Access != AccessOK) {
		return fmt.Errorf("storage helper returned an unexpected missing tail")
	}
	switch response.Access {
	case AccessOK:
		if response.Step != "" || response.Code != 0 || response.RecordedMount != nil {
			return fmt.Errorf("storage helper success response is inconsistent")
		}
	case AccessMissing, AccessDenied, AccessFailed:
		if response.Step != StepOpenFolder && response.Step != StepReadEntry {
			return fmt.Errorf("storage helper failure step is invalid")
		}
		if !facts.Empty() || len(response.FactFailures) != 0 {
			return fmt.Errorf("storage helper failure response contains facts")
		}
		wantCheck := response.Access == AccessMissing && request.MountPoint != ""
		if wantCheck != (response.RecordedMount != nil) {
			return fmt.Errorf("storage helper mount point check is inconsistent")
		}
		if check := response.RecordedMount; check != nil {
			switch check.State {
			case MountPresent, MountAbsent:
				if check.State == MountPresent && (check.Step != "" || check.Code != 0) {
					return fmt.Errorf("storage helper mount point check is inconsistent")
				}
			case MountDenied, MountFailed:
				if check.Step != StepMountPointCheck {
					return fmt.Errorf("storage helper mount point check is inconsistent")
				}
			default:
				return fmt.Errorf("storage helper mount point state is invalid")
			}
		}
	default:
		return fmt.Errorf("storage helper access result is invalid")
	}
	return nil
}
