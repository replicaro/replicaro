// Package storageidentity records and observes the two storage facts Replicaro
// keeps for a filesystem source or vault: the mount point the folder lives on
// and that mount's filesystem type. It also implements the one-operation
// storage helper protocol used to observe them in a separate, killable process.
//
// Background for future changes: this package used to build "physical
// identity" descriptors (volume GUIDs, filesystem UUIDs, SMB provider and
// share identities, NFS exports, WinFsp proofs, mounted-candidate
// enumeration). On Windows network paths roughly seven lookups could fail
// closed during vault creation, which is how SMB users ended up unable to
// create a vault at all. That machinery was replaced on purpose by the two
// cheap facts below. Do not reintroduce volume IDs, share identity lookups,
// or mount enumeration here without an explicit product decision: every added
// OS lookup is another way to refuse a perfectly readable path.
package storageidentity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

const (
	// BindingVersion is the current value of the *_storage_version columns.
	// The *_storage_key column holds the normalized configured path (the
	// immutable job source or the vault location) and the JSON column holds
	// only the recorded facts. The key is kept because existing columns and
	// constraints require it; runtime checks never compare keys, only facts.
	BindingVersion = "storage_facts_v5"

	// LegacyBindingVersion marks rows written by the descriptor-based code.
	// Loaders accept these rows without decoding the old descriptor. The first
	// successful probe of the folder (under the job admission transaction or
	// the vault lock) rewrites the row in the current format with whatever
	// facts it observes. The old descriptor is intentionally never compared
	// with the new observation; that exposure is an accepted, documented
	// trade-off, so please don't add a comparison back as a "fix".
	LegacyBindingVersion = "storage_identity_v4"

	maxFactBytes          = 32 << 10
	maxLegacyBindingBytes = 256 << 10
)

// Facts are the only storage facts recorded for a source or vault. Either
// value may be empty: an empty value means the fact could not be determined
// when the binding was saved (or, for Windows network paths, that the
// filesystem type is deliberately not recorded) and is therefore never
// compared before a run.
type Facts struct {
	MountPoint string `json:"mount_point"`
	Filesystem string `json:"filesystem"`
}

type bindingDocument struct {
	Version    string `json:"version"`
	MountPoint string `json:"mount_point"`
	Filesystem string `json:"filesystem"`
}

// Empty reports whether no fact was recorded.
func (facts Facts) Empty() bool { return facts.MountPoint == "" && facts.Filesystem == "" }

// Validate checks that facts are safe to persist and compare. It does not
// check that the facts describe any particular folder.
func (facts Facts) Validate() error {
	for name, value := range map[string]string{"mount point": facts.MountPoint, "filesystem type": facts.Filesystem} {
		if len(value) > maxFactBytes {
			return fmt.Errorf("%s exceeds the storage fact limit", name)
		}
		if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s contains control characters", name)
		}
	}
	if facts.Filesystem != NormalizeFilesystem(facts.Filesystem) {
		return fmt.Errorf("filesystem type is not normalized")
	}
	if facts.MountPoint != "" && strings.TrimSpace(facts.MountPoint) == "" {
		return fmt.Errorf("mount point is blank")
	}
	return nil
}

// EncodeBinding returns the canonical JSON stored in the binding column.
func EncodeBinding(facts Facts) (string, error) {
	if err := facts.Validate(); err != nil {
		return "", err
	}
	encoded, err := json.Marshal(bindingDocument{
		Version: BindingVersion, MountPoint: facts.MountPoint, Filesystem: facts.Filesystem,
	})
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

// DecodeBinding validates one stored binding tuple. configuredPath is the
// immutable path the key must equal: the job source or the vault location.
// Legacy rows are accepted (legacy=true) without being decoded, so the loader
// never has to understand the retired descriptor format.
func DecodeBinding(version, key, bindingJSON, configuredPath string) (facts Facts, legacy bool, err error) {
	if version == LegacyBindingVersion {
		if key == "" || bindingJSON == "" || len(key) > maxLegacyBindingBytes || len(bindingJSON) > maxLegacyBindingBytes {
			return Facts{}, false, fmt.Errorf("legacy storage binding is incomplete")
		}
		if err := ValidateBindingPath(configuredPath); err != nil {
			return Facts{}, false, err
		}
		return Facts{}, true, nil
	}
	if version != BindingVersion {
		return Facts{}, false, fmt.Errorf("unsupported storage binding version")
	}
	if err := ValidateBindingPath(configuredPath); err != nil {
		return Facts{}, false, err
	}
	if key != configuredPath {
		return Facts{}, false, fmt.Errorf("storage binding key does not match the configured path")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(bindingJSON))
	decoder.DisallowUnknownFields()
	var document bindingDocument
	if err := decoder.Decode(&document); err != nil {
		return Facts{}, false, fmt.Errorf("storage binding facts are invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Facts{}, false, fmt.Errorf("storage binding facts have trailing data")
	}
	if document.Version != BindingVersion {
		return Facts{}, false, fmt.Errorf("storage binding facts have the wrong version")
	}
	facts = Facts{MountPoint: document.MountPoint, Filesystem: document.Filesystem}
	canonical, err := EncodeBinding(facts)
	if err != nil {
		return Facts{}, false, err
	}
	if canonical != bindingJSON {
		return Facts{}, false, fmt.Errorf("storage binding facts are not canonical")
	}
	return facts, false, nil
}

// ValidateBindingPath is the entry-point rule for a path that is about to be
// bound or probed: it must already be lexically normalized and must not
// contain any control character. The broader ValidateConfiguredPath only
// rejects NUL, CR, and LF because it is also used by the directory picker,
// Kopia policy derivation for historical imported jobs, and effective-address
// resolution.
func ValidateBindingPath(path string) error {
	normalized, err := NormalizeConfiguredPath(path)
	if err != nil {
		return err
	}
	if normalized != path {
		return fmt.Errorf("storage path is not normalized")
	}
	if strings.IndexFunc(path, unicode.IsControl) >= 0 {
		return fmt.Errorf("storage path contains control characters")
	}
	return nil
}

// NormalizeFilesystem lowercases a filesystem type and folds the Linux SMB and
// NFS module aliases into one family name each. The same share can show up as
// "cifs" or "smb3" (and "nfs" or "nfs4") depending on how it was mounted, and
// that difference must not look like "a different filesystem is now mounted
// here" and pause a job.
func NormalizeFilesystem(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	switch value {
	case "smb3":
		return "cifs"
	case "nfs4":
		return "nfs"
	}
	return value
}

// SameMountPoint compares two recorded mount points. Windows-shaped values
// (drive letters, UNC shares, and their \\?\ extended spellings) compare
// case-insensitively and ignore the \\?\ or \\?\UNC\ prefix and a trailing
// separator, because GetVolumePathNameW, GetFinalPathNameByHandleW, and the
// user's own spelling can each produce a different but equivalent form. POSIX
// mount points compare exactly.
func SameMountPoint(left, right string) bool {
	if left == "" || right == "" {
		return false
	}
	if windowsShapedMount(left) || windowsShapedMount(right) {
		return windowsMountKey(left) == windowsMountKey(right)
	}
	return left == right
}

func windowsShapedMount(value string) bool {
	value = strings.ReplaceAll(value, "/", `\`)
	if strings.HasPrefix(value, `\\`) {
		return true
	}
	return len(value) >= 2 && value[1] == ':' && isASCIILetter(value[0])
}

func windowsMountKey(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, "/", `\`))
	switch {
	case strings.HasPrefix(value, `\\?\unc\`):
		value = `\\` + value[len(`\\?\unc\`):]
	case strings.HasPrefix(value, `\\?\`):
		value = value[len(`\\?\`):]
	}
	return strings.TrimRight(value, `\`)
}

func isASCIILetter(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}
