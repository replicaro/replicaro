// Package storageidentity resolves credential-free stable physical bindings
// where the OS supplies them and exact configured-path admission facts
// otherwise.
package storageidentity

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	DescriptorVersion       = "storage_identity_v4"
	maxDescriptorFieldBytes = 32 << 10
	maxCanonicalKeyBytes    = 256 << 10
)

type Kind string

const (
	KindPathOnly Kind = "path_only"
	KindVolume   Kind = "volume"
	KindSMB      Kind = "smb"
	KindNFS      Kind = "nfs"
)

// StorageClass describes routing semantics, not hardware attachment. A local
// filesystem may move regardless of the device removability flags. Descriptor v4
// intentionally rejects the former fixed/removable vocabulary; retaining those
// values would silently preserve two different discovery policies.
type StorageClass string

const (
	StorageClassLocal   StorageClass = "local"
	StorageClassNetwork StorageClass = "network"
	StorageClassOther   StorageClass = "other"
)

// Descriptor contains only credential-free storage facts. Stable kinds describe
// an OS-authoritative physical namespace; path_only describes only the exact
// normalized configured path and its observed filesystem type.
type Descriptor struct {
	Version    string `json:"version"`
	Kind       Kind   `json:"kind"`
	Filesystem string `json:"filesystem"`
	// ActualFilesystem is a local observation fact for stable bindings whose
	// identity grammar uses a protocol name (notably Windows SMB/NFS). It is
	// deliberately excluded from CanonicalKey: stable identity, rather than a
	// filesystem-type continuity policy, admits those sources at runtime.
	ActualFilesystem string `json:"actual_filesystem,omitempty"`
	VolumeID         string `json:"volume_id,omitempty"`
	Endpoint         string `json:"endpoint,omitempty"`
	Provider         string `json:"provider,omitempty"`
	Share            string `json:"share,omitempty"`
	Export           string `json:"export,omitempty"`
	MountRoot        string `json:"mount_root,omitempty"`
	RelativePath     string `json:"relative_path"`
	// LocalRelativePath is OS-observed mount-relative destination geometry,
	// independent of configured symlink spelling. It is never source identity.
	LocalRelativePath string       `json:"local_relative_path,omitempty"`
	StorageClass      StorageClass `json:"storage_class"`
}

func (d Descriptor) Validate() error {
	if d.Version != DescriptorVersion {
		return fmt.Errorf("unsupported storage identity version")
	}
	for name, value := range map[string]string{
		"filesystem": d.Filesystem, "actual filesystem": d.ActualFilesystem,
		"volume ID": d.VolumeID, "endpoint": d.Endpoint,
		"provider": d.Provider, "share": d.Share, "export": d.Export, "mount root": d.MountRoot,
		"relative path": d.RelativePath, "local relative path": d.LocalRelativePath,
	} {
		if strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return fmt.Errorf("%s contains control characters", name)
		}
		if len(value) > maxDescriptorFieldBytes {
			return fmt.Errorf("%s exceeds the storage identity limit", name)
		}
	}
	if d.Filesystem == "" || d.RelativePath == "" {
		return fmt.Errorf("filesystem and path are required")
	}
	if d.Endpoint != "" && strings.ContainsAny(d.Endpoint, `@/?#\ `) {
		return fmt.Errorf("endpoint is not a credential-free exact server name")
	}
	if d.Share != "" && strings.ContainsAny(d.Share, `/\`) {
		return fmt.Errorf("share is not canonical")
	}
	switch d.StorageClass {
	case StorageClassLocal, StorageClassNetwork, StorageClassOther:
	default:
		return fmt.Errorf("storage class is invalid")
	}
	if d.LocalRelativePath != "" {
		if d.Kind != KindPathOnly || d.StorageClass != StorageClassLocal || d.MountRoot == "" ||
			strings.HasPrefix(d.LocalRelativePath, "/") || path.Clean(d.LocalRelativePath) != d.LocalRelativePath ||
			d.LocalRelativePath != "." && hasDotSegment(d.LocalRelativePath) ||
			looksLikeCanonicalWindowsPath(d.MountRoot) && (strings.Contains(d.LocalRelativePath, `\`) || strings.Contains(d.LocalRelativePath, ":")) {
			return fmt.Errorf("local relative path is not canonical mount-relative geometry")
		}
	}
	switch d.Kind {
	case KindPathOnly:
		if d.ActualFilesystem != "" {
			return fmt.Errorf("configured-path binding duplicates its filesystem observation")
		}
		if d.Filesystem != strings.ToLower(strings.TrimSpace(d.Filesystem)) {
			return fmt.Errorf("path-only filesystem type is not normalized")
		}
		if d.VolumeID != "" || d.Endpoint != "" || d.Provider != "" || d.Share != "" || d.Export != "" {
			return fmt.Errorf("configured-path binding contains inapplicable fields")
		}
		if d.MountRoot != "" && d.StorageClass != StorageClassLocal {
			return fmt.Errorf("only local configured-path bindings may retain mount geometry")
		}
		if d.MountRoot != "" && !canonicalConfiguredPath(d.MountRoot) {
			return fmt.Errorf("configured-path mount root must be normalized and absolute")
		}
		if !canonicalConfiguredPath(d.RelativePath) {
			return fmt.Errorf("configured path must be normalized and absolute")
		}
	case KindVolume:
		if d.VolumeID == "" || d.Endpoint != "" || d.Provider != "" || d.Share != "" || d.Export != "" {
			return fmt.Errorf("volume identity fields are incomplete")
		}
		if d.StorageClass != StorageClassLocal {
			return fmt.Errorf("volume storage class is invalid")
		}
	case KindSMB:
		if d.Endpoint == "" || d.Share == "" || d.VolumeID != "" || d.Export != "" {
			return fmt.Errorf("SMB identity fields are incomplete")
		}
		if d.StorageClass != StorageClassNetwork {
			return fmt.Errorf("SMB storage class is invalid")
		}
	case KindNFS:
		if d.Endpoint == "" || d.Export == "" || d.VolumeID != "" || d.Share != "" {
			return fmt.Errorf("NFS identity fields are incomplete")
		}
		if !strings.HasPrefix(d.Export, "/") {
			return fmt.Errorf("NFS export is not canonical")
		}
		if d.StorageClass != StorageClassNetwork {
			return fmt.Errorf("NFS storage class is invalid")
		}
	default:
		return fmt.Errorf("unsupported storage kind")
	}
	if d.ActualFilesystem != "" &&
		d.ActualFilesystem != strings.ToLower(strings.TrimSpace(d.ActualFilesystem)) {
		return fmt.Errorf("actual filesystem type is not normalized")
	}
	if d.Kind != KindPathOnly {
		if strings.HasPrefix(d.RelativePath, "/") || d.RelativePath != "." && hasDotSegment(d.RelativePath) {
			return fmt.Errorf("relative path is not canonical")
		}
		if d.MountRoot != "" &&
			(!strings.HasPrefix(d.MountRoot, "/") && !looksLikeCanonicalWindowsPath(d.MountRoot) ||
				hasDotSegment(d.MountRoot)) {
			return fmt.Errorf("mount root is not canonical")
		}
	}
	return nil
}

func (d Descriptor) CanonicalKey() (string, error) {
	if err := d.Validate(); err != nil {
		return "", err
	}
	canonical := d.canonicalStorageNamespace()
	encoded, err := json.Marshal(canonical)
	if err != nil {
		return "", fmt.Errorf("serialize storage identity: %w", err)
	}
	key := DescriptorVersion + ":" + base64.RawURLEncoding.EncodeToString(encoded)
	if len(key) > maxCanonicalKeyBytes {
		return "", fmt.Errorf("storage identity key exceeds limit")
	}
	return key, nil
}

// canonicalStorageNamespace composes an OS-proven filesystem root with its
// mount-relative path before keying. The persisted descriptor retains the
// observed mount facts, while bind mounts and mounted subroots address the
// same stable namespace as their parent mount. The destination-only geometry
// hint is excluded, so a source key cannot turn it into physical identity.
func (d Descriptor) canonicalStorageNamespace() Descriptor {
	// The actual filesystem is required local recording evidence, but stable
	// source admission intentionally has no filesystem-type continuity gate.
	// Keeping it out of the key prevents that recording fact from becoming a
	// second identity binding.
	d.ActualFilesystem = ""
	d.LocalRelativePath = ""
	if d.Kind == KindPathOnly {
		return d
	}
	canonical := d
	if !looksLikeCanonicalWindowsPath(d.MountRoot) {
		canonical.RelativePath = cleanRelative(path.Join(cleanRoot(d.MountRoot), d.RelativePath))
	}
	// A Windows volume path is already relative to the exact volume root
	// returned by GetVolumePathName. Its host-side folder mount spelling is
	// diagnostic fact, not part of the volume namespace.
	canonical.MountRoot = "/"
	return canonical
}

// CandidatePath derives the exact path below one currently mounted root for
// an expected relocatable identity. It compares only OS-authoritative
// namespace facts; the returned object must still be observed normally.
func CandidatePath(expected Descriptor, mounted MountedFilesystem) (string, bool) {
	if expected.Kind == KindPathOnly || mounted.Descriptor.Kind == KindPathOnly ||
		expected.Kind != mounted.Descriptor.Kind || expected.Filesystem != mounted.Descriptor.Filesystem ||
		expected.VolumeID != mounted.Descriptor.VolumeID || expected.Endpoint != mounted.Descriptor.Endpoint ||
		expected.Provider != mounted.Descriptor.Provider ||
		expected.Share != mounted.Descriptor.Share || expected.Export != mounted.Descriptor.Export {
		return "", false
	}
	return candidatePathWithinMount(expected, mounted)
}

// CandidatePathOnLocal derives the same exact repository-relative path on
// mounted local storage. Volume identity is intentionally not compared:
// a candidate is only a location hint and the engine/native repository ID plus
// protected vault UUID must prove it before use.
func CandidatePathOnLocal(expected Descriptor, mounted MountedFilesystem) (string, bool) {
	if expected.StorageClass != StorageClassLocal ||
		mounted.Descriptor.StorageClass != StorageClassLocal {
		return "", false
	}
	if expected.Kind == KindPathOnly {
		if expected.LocalRelativePath == "" {
			return "", false
		}
		relative := expected.LocalRelativePath
		if relative == "." {
			return filepath.Clean(mounted.Path), true
		}
		return filepath.Join(mounted.Path, filepath.FromSlash(relative)), true
	}
	if mounted.Descriptor.Kind == KindPathOnly {
		// A local mount can be authoritatively classified even when the OS
		// supplies no stable volume ID. Its mount root is still a bounded location
		// hint; only native/protected repository proof may admit the candidate.
		relative := expected.canonicalStorageNamespace().RelativePath
		if relative == "." {
			return filepath.Clean(mounted.Path), true
		}
		return filepath.Join(mounted.Path, filepath.FromSlash(relative)), true
	}
	return candidatePathWithinMount(expected, mounted)
}

func configuredPathRelative(root, value string) (string, bool) {
	if looksLikeCanonicalWindowsPath(root) || looksLikeCanonicalWindowsPath(value) {
		return windowsPathRelative(cleanWindowsPath(root), cleanWindowsPath(value))
	}
	relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(value))
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false
	}
	return filepath.ToSlash(relative), true
}

func candidatePathWithinMount(expected Descriptor, mounted MountedFilesystem) (string, bool) {
	want := expected.canonicalStorageNamespace().RelativePath
	root := mounted.Descriptor.canonicalStorageNamespace().RelativePath
	want = cleanRelative(want)
	root = cleanRelative(root)
	var relative string
	switch {
	case root == ".":
		relative = want
	case want == root:
		relative = "."
	case strings.HasPrefix(want, root+"/"):
		relative = strings.TrimPrefix(want, root+"/")
	default:
		return "", false
	}
	if relative == "." {
		return filepath.Clean(mounted.Path), true
	}
	return filepath.Join(mounted.Path, filepath.FromSlash(relative)), true
}

func ParseCanonicalKey(key string) (Descriptor, error) {
	prefix := DescriptorVersion + ":"
	if len(key) > maxCanonicalKeyBytes {
		return Descriptor{}, fmt.Errorf("storage identity key exceeds limit")
	}
	if !strings.HasPrefix(key, prefix) {
		return Descriptor{}, fmt.Errorf("unsupported storage identity key")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, prefix))
	if err != nil {
		return Descriptor{}, fmt.Errorf("decode storage identity key: %w", err)
	}
	var d Descriptor
	if err := decodeStrict(raw, &d); err != nil {
		return Descriptor{}, fmt.Errorf("parse storage identity key: %w", err)
	}
	if err := d.Validate(); err != nil {
		return Descriptor{}, err
	}
	reencoded, _ := d.CanonicalKey()
	if reencoded != key {
		return Descriptor{}, fmt.Errorf("storage identity key is not canonical")
	}
	return d, nil
}

// DecodeBinding is the single strict decoder used at both persistence and
// runtime admission. Keeping both callers on this pure check prevents a saved
// tuple from being accepted by one boundary and repaired by the other.
func DecodeBinding(version, key, descriptorJSON string) (Descriptor, error) {
	if version != DescriptorVersion || key == "" || descriptorJSON == "" {
		return Descriptor{}, fmt.Errorf("storage identity binding is incomplete")
	}
	decoder := json.NewDecoder(bytes.NewBufferString(descriptorJSON))
	decoder.DisallowUnknownFields()
	var descriptor Descriptor
	if err := decoder.Decode(&descriptor); err != nil {
		return Descriptor{}, fmt.Errorf("storage identity descriptor is invalid")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Descriptor{}, fmt.Errorf("storage identity descriptor has trailing data")
	}
	actual, err := descriptor.CanonicalKey()
	canonicalJSON, marshalErr := json.Marshal(descriptor)
	if err != nil || marshalErr != nil || descriptor.Version != version || actual != key || string(canonicalJSON) != descriptorJSON {
		return Descriptor{}, fmt.Errorf("storage identity binding is inconsistent or noncanonical")
	}
	return descriptor, nil
}

func cleanRelative(value string) string {
	value = strings.TrimPrefix(path.Clean("/"+value), "/")
	if value == "." || value == "" {
		return "."
	}
	return value
}

func cleanRoot(value string) string {
	value = path.Clean("/" + strings.TrimPrefix(value, "/"))
	return value
}

func hasDotSegment(value string) bool {
	for _, part := range strings.Split(value, "/") {
		if part == "." || part == ".." {
			return true
		}
	}
	return false
}

func looksLikeCanonicalWindowsPath(value string) bool {
	return len(value) >= 3 && value[1] == ':' && value[2] == '\\' || isWindowsUNC(value)
}

func canonicalConfiguredPath(value string) bool {
	if looksLikeCanonicalWindowsPath(value) {
		return cleanWindowsPath(value) == value
	}
	return strings.HasPrefix(value, "/") && path.Clean(value) == value
}

// PathOnlyMatchesConfiguredPath proves that a path-only descriptor describes
// the exact normalized configured path supplied to the current operation.
func PathOnlyMatchesConfiguredPath(descriptor Descriptor, configured string) bool {
	if descriptor.Kind != KindPathOnly {
		return false
	}
	normalized, err := NormalizeConfiguredPath(configured)
	if err != nil || normalized != configured {
		return false
	}
	return descriptor.RelativePath == normalized
}
