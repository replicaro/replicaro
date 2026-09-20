package models

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
)

const (
	SnapshotProfileMarkerPrefix   = "replicaro-profile:"
	SnapshotOwnershipMarkerPrefix = "replicaro-job:"
	SnapshotClientMarkerPrefix    = "replicaro-client:"
	SnapshotMachineMarkerPrefix   = "replicaro-machine:"
	ReservedSnapshotTagError      = "The `replicaro-profile`, `replicaro-job`, `replicaro-client`, and `replicaro-machine` tag namespaces are reserved for Replicaro. Choose a different tag."
)

type SnapshotOwnershipMarkerStatus string

const (
	SnapshotOwnershipMissing     SnapshotOwnershipMarkerStatus = "missing"
	SnapshotOwnershipValid       SnapshotOwnershipMarkerStatus = "valid"
	SnapshotOwnershipMalformed   SnapshotOwnershipMarkerStatus = "malformed"
	SnapshotOwnershipDuplicate   SnapshotOwnershipMarkerStatus = "duplicate"
	SnapshotOwnershipConflicting SnapshotOwnershipMarkerStatus = "conflicting"
)

type SnapshotOwnershipMarker struct {
	Status    SnapshotOwnershipMarkerStatus `json:"status"`
	ProfileID string                        `json:"profileId,omitempty"`
	JobID     string                        `json:"jobId,omitempty"`
	Markers   []string                      `json:"markers,omitempty"`
}

type SnapshotSourceRoot struct {
	Path     string `json:"path"`
	User     string `json:"user,omitempty"`
	Host     string `json:"host,omitempty"`
	Identity string `json:"nativeRootId"`
}

// SnapshotNativeRootIdentity is rebuildable presentation/cache identity only.
// It is never ownership evidence and the exact Path remains the engine input.
func SnapshotNativeRootIdentity(root SnapshotSourceRoot) string {
	digest := sha256.Sum256([]byte(root.User + "\x00" + root.Host + "\x00" + root.Path))
	return hex.EncodeToString(digest[:])
}

func WithSnapshotNativeRootIdentity(root SnapshotSourceRoot) SnapshotSourceRoot {
	root.Identity = SnapshotNativeRootIdentity(root)
	return root
}

type SnapshotPresentation string

const (
	SnapshotPresentationManaged   SnapshotPresentation = "managed"
	SnapshotPresentationUnmanaged SnapshotPresentation = "unmanaged"
	SnapshotPresentationHidden    SnapshotPresentation = "hidden"
)

type Snapshot struct {
	// NativeRootType is Kopia header metadata: d (directory) or f (regular file).
	NativeRootType  string                  `json:"nativeRootType,omitempty"`
	ID              string                  `json:"id"`
	Timestamp       string                  `json:"timestamp"`
	Size            string                  `json:"size"`
	Duration        string                  `json:"duration"`
	Source          string                  `json:"source"`
	Tags            []string                `json:"tags,omitempty"`
	OwnershipMarker SnapshotOwnershipMarker `json:"ownershipMarker"`
	SourceRoots     []SnapshotSourceRoot    `json:"sourceRoots,omitempty"`
	Presentation    SnapshotPresentation    `json:"presentation,omitempty"`
	ManagedJobID    string                  `json:"managedJobId,omitempty"`
	// Presentation tags are rebuildable, non-authoritative hints. They never
	// participate in ownership, visibility, retention, deletion, or restore.
	PresentationClientUUID   string `json:"-"`
	PresentationMachineLabel string `json:"-"`
	// NativeSourceUser and NativeSourceHost preserve engine-emitted source
	// identity needed for native addressing and classification. They are not
	// presentation or portable profile fields.
	NativeSourceUser string `json:"-"`
	NativeSourceHost string `json:"-"`
	// LogicalSizeBytes is optional application-cache input captured only from
	// already-produced native backup output.
	LogicalSizeBytes *int64 `json:"-"`
	// TotalFileCount is the optional recursive native header total, including unchanged files.
	TotalFileCount *int64 `json:"-"`
}

// ClassifySnapshotPresentation is the sole presentation classifier. A valid
// foreign profile is hidden; every nonexact marker state remains visible
// unmanaged. Managed presentation never substitutes for native retention scope.
func ClassifySnapshotPresentation(snapshot Snapshot, profileUUID string, knownJobs map[string]bool) SnapshotPresentation {
	marker := snapshot.OwnershipMarker
	if marker.Status != SnapshotOwnershipValid {
		return SnapshotPresentationUnmanaged
	}
	if marker.ProfileID != profileUUID {
		return SnapshotPresentationHidden
	}
	if !knownJobs[marker.JobID] {
		return SnapshotPresentationUnmanaged
	}
	return SnapshotPresentationManaged
}

func PresentSnapshot(snapshot Snapshot, profileUUID string, knownJobs map[string]bool) Snapshot {
	snapshot.Presentation = ClassifySnapshotPresentation(snapshot, profileUUID, knownJobs)
	if snapshot.Presentation == SnapshotPresentationManaged {
		snapshot.ManagedJobID = snapshot.OwnershipMarker.JobID
	} else {
		snapshot.ManagedJobID = ""
	}
	return snapshot
}

func ClassifySnapshotOwnership(tags []string) SnapshotOwnershipMarker {
	result := SnapshotOwnershipMarker{Status: SnapshotOwnershipMissing}
	validProfiles := map[string]int{}
	validJobs := map[string]int{}
	malformed := false
	for _, tag := range tags {
		profileMarker := tag == "replicaro-profile" || strings.HasPrefix(tag, SnapshotProfileMarkerPrefix)
		jobMarker := tag == "replicaro-job" || strings.HasPrefix(tag, SnapshotOwnershipMarkerPrefix)
		if !profileMarker && !jobMarker {
			continue
		}
		result.Markers = append(result.Markers, tag)
		prefix := SnapshotOwnershipMarkerPrefix
		if profileMarker {
			prefix = SnapshotProfileMarkerPrefix
		}
		value := strings.TrimPrefix(tag, prefix)
		parsed, err := uuid.Parse(value)
		if err != nil || parsed == uuid.Nil || parsed.String() != value {
			malformed = true
			continue
		}
		if profileMarker {
			validProfiles[value]++
		} else {
			validJobs[value]++
		}
	}
	if len(result.Markers) == 0 {
		return result
	}
	if malformed {
		result.Status = SnapshotOwnershipMalformed
		return result
	}
	if len(validProfiles) != 1 || len(validJobs) != 1 {
		result.Status = SnapshotOwnershipConflicting
		return result
	}
	for profileID, count := range validProfiles {
		if count != 1 {
			result.Status = SnapshotOwnershipDuplicate
			return result
		}
		result.ProfileID = profileID
	}
	for jobID, count := range validJobs {
		if count != 1 {
			result.Status = SnapshotOwnershipDuplicate
			return result
		}
		result.JobID = jobID
	}
	result.Status = SnapshotOwnershipValid
	return result
}

// ContainsReservedOwnershipTag keeps the internal ownership namespace
// separate from user-defined snapshot tags. Each comma-separated component is
// checked independently.
func ContainsReservedOwnershipTag(value string) bool {
	for _, tag := range strings.Split(value, ",") {
		tag = strings.TrimSpace(tag)
		if tag == "replicaro-profile" || strings.HasPrefix(tag, SnapshotProfileMarkerPrefix) ||
			tag == "replicaro-job" || strings.HasPrefix(tag, SnapshotOwnershipMarkerPrefix) ||
			tag == "replicaro-client" || strings.HasPrefix(tag, SnapshotClientMarkerPrefix) ||
			tag == "replicaro-machine" || strings.HasPrefix(tag, SnapshotMachineMarkerPrefix) {
			return true
		}
	}
	return false
}

func SnapshotWithTags(snapshot Snapshot, tags []string) Snapshot {
	snapshot.Tags = append([]string(nil), tags...)
	snapshot.OwnershipMarker = ClassifySnapshotOwnership(snapshot.Tags)
	return snapshot
}

type SnapshotEntry struct {
	Name           string `json:"name"`
	Path           string `json:"path,omitempty"`
	SourceRoot     string `json:"sourceRoot,omitempty"`
	NativeRootID   string `json:"nativeRootId,omitempty"`
	NativeRootUser string `json:"nativeRootUser,omitempty"`
	NativeRootHost string `json:"nativeRootHost,omitempty"`
	Mode           string `json:"mode"`
	Size           string `json:"size"`
	IsDir          bool   `json:"isDir"`
}

type FileSearchResult struct {
	Path   string `json:"path"`
	Name   string `json:"name"`
	Source string `json:"source"`
	IsDir  bool   `json:"isDir"`
}

// FileBrowseEntry is one source root or immediate child in the indexed-history
// browser. Source roots are navigation-only and have an empty Path.
type FileBrowseEntry struct {
	Path      string `json:"path"`
	Name      string `json:"name"`
	Source    string `json:"source"`
	IsDir     bool   `json:"isDir"`
	IsSource  bool   `json:"isSource"`
	CanExpand bool   `json:"canExpand"`
}

type FileVersion struct {
	SnapshotID   string `json:"snapshotId"`
	Timestamp    string `json:"timestamp"`
	Size         string `json:"size"`
	Source       string `json:"source"`
	IsDir        bool   `json:"isDir"`
	Present      bool   `json:"present"`
	NativeRootID string `json:"nativeRootId"`
	MachineLabel string `json:"machineLabel"`
}

// ParseSnapshotPresentationTags validates the two optional presentation
// namespaces independently. Exactly one valid value is required per namespace;
// malformed or duplicate values discard only that namespace.
func ParseSnapshotPresentationTags(tags []string) (clientUUID, machineLabel string) {
	clientValues, machineValues := []string{}, []string{}
	clientMalformed, machineMalformed := false, false
	for _, tag := range tags {
		switch {
		case tag == "replicaro-client" || strings.HasPrefix(tag, SnapshotClientMarkerPrefix):
			value := strings.TrimPrefix(tag, SnapshotClientMarkerPrefix)
			parsed, err := uuid.Parse(value)
			if err != nil || parsed == uuid.Nil || parsed.String() != value || len(tag) > 128 {
				clientMalformed = true
			} else {
				clientValues = append(clientValues, value)
			}
		case tag == "replicaro-machine" || strings.HasPrefix(tag, SnapshotMachineMarkerPrefix):
			value := strings.TrimPrefix(tag, SnapshotMachineMarkerPrefix)
			decoded, ok := parseSnapshotMachineValue(value)
			if !ok || len(tag) > 384 {
				machineMalformed = true
			} else {
				machineValues = append(machineValues, decoded)
			}
		}
	}
	if !clientMalformed && len(clientValues) == 1 {
		clientUUID = clientValues[0]
	}
	if !machineMalformed && len(machineValues) == 1 {
		machineLabel = machineValues[0]
	}
	return clientUUID, machineLabel
}

func parseSnapshotMachineValue(value string) (string, bool) {
	separator := strings.LastIndexByte(value, '@')
	if separator <= 0 || separator == len(value)-1 {
		return "", false
	}
	osName := value[separator+1:]
	if !validSnapshotMachineOS(osName) {
		return "", false
	}
	encoded := value[:separator]
	decoded := make([]byte, 0, len(encoded))
	for index := 0; index < len(encoded); index++ {
		character := encoded[index]
		if character == '%' {
			if index+2 >= len(encoded) {
				return "", false
			}
			high, low := hexValue(encoded[index+1]), hexValue(encoded[index+2])
			if high < 0 || low < 0 {
				return "", false
			}
			decodedByte := byte(high<<4 | low)
			if isPresentationUnreserved(decodedByte) {
				return "", false
			}
			decoded = append(decoded, decodedByte)
			index += 2
			continue
		}
		if !isPresentationUnreserved(character) {
			return "", false
		}
		decoded = append(decoded, character)
	}
	if len(decoded) == 0 || len(decoded) > 255 || !utf8.Valid(decoded) {
		return "", false
	}
	name := string(decoded)
	if !validSnapshotMachineName(name) {
		return "", false
	}
	return name + "@" + osName, true
}

// EncodeSnapshotMachineTag shares the parser's exact hostname and platform
// admission rule so a producer cannot create a presentation tag that the
// cache later rejects. Failure remains local to this optional tag.
func EncodeSnapshotMachineTag(name, osName string) (string, bool) {
	if !validSnapshotMachineName(name) || !validSnapshotMachineOS(osName) {
		return "", false
	}
	var encoded strings.Builder
	for _, value := range []byte(name) {
		if isPresentationUnreserved(value) {
			encoded.WriteByte(value)
		} else {
			_, _ = fmt.Fprintf(&encoded, "%%%02X", value)
		}
	}
	tag := SnapshotMachineMarkerPrefix + encoded.String() + "@" + osName
	return tag, len(tag) <= 384
}

func validSnapshotMachineName(name string) bool {
	return name != "" && len([]byte(name)) <= 255 && utf8.ValidString(name) &&
		strings.TrimSpace(name) == name && strings.IndexFunc(name, unicode.IsControl) < 0
}

func validSnapshotMachineOS(osName string) bool {
	return osName == "windows" || osName == "darwin" || osName == "linux"
}

func hexValue(value byte) int {
	switch {
	case value >= '0' && value <= '9':
		return int(value - '0')
	case value >= 'A' && value <= 'F':
		return int(value-'A') + 10
	default:
		return -1
	}
}

func isPresentationUnreserved(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' ||
		value >= '0' && value <= '9' || value == '.' || value == '_' || value == '-'
}
