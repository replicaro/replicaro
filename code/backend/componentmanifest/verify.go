// Package componentmanifest verifies the exact native component inputs for one
// supported product target from the public component metadata. It is offline,
// deterministic, and read-only.
package componentmanifest

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	maxComponentMetadataSize = 1 << 20
)

var supportedTargets = map[string]bool{
	"windows_amd64": true, "linux_amd64": true, "linux_arm64": true,
	"darwin_amd64": true, "darwin_arm64": true,
}

var componentIdentities = map[string]struct{ owner, repository string }{
	"restic": {"restic", "restic"},
	"kopia":  {"kopia", "kopia"},
	"rclone": {"rclone", "rclone"},
}

// Artifact is one exact public-metadata acquisition selection.
type Artifact struct {
	Component       string
	Target          string
	Version         string
	Repository      string
	Release         string
	ArchiveURL      string
	ArchiveSHA256   string
	ArchiveFilename string
	BinaryPath      string
	BinaryFilename  string
	BinarySHA256    string
	ChecksumURL     string
}

type artifactMetadata struct {
	Target           string `json:"target"`
	Archive          string `json:"archive"`
	ArchiveSHA256    string `json:"archiveSha256"`
	Binary           string `json:"binary"`
	BinarySHA256     string `json:"binarySha256"`
	UpstreamChecksum string `json:"upstreamChecksum"`
	SignatureURL     string `json:"signatureUrl"`
}

type componentMetadata struct {
	ID                   string                     `json:"id"`
	Version              string                     `json:"version"`
	ReviewedAt           string                     `json:"reviewedAt"`
	LatestStableObserved string                     `json:"latestStableObserved"`
	PinReason            string                     `json:"pinReason"`
	Source               string                     `json:"source"`
	Archive              string                     `json:"archive"`
	ArchiveSHA256        string                     `json:"archiveSha256"`
	Binary               string                     `json:"binary"`
	BinarySHA256         string                     `json:"binarySha256"`
	License              string                     `json:"license"`
	LicensePath          string                     `json:"licensePath"`
	LicenseURL           string                     `json:"licenseUrl"`
	Artifacts            map[string]json.RawMessage `json:"artifacts"`
}

type componentMetadataEnvelope struct {
	ID                   string          `json:"id"`
	Version              string          `json:"version"`
	ReviewedAt           string          `json:"reviewedAt"`
	LatestStableObserved string          `json:"latestStableObserved"`
	PinReason            string          `json:"pinReason"`
	Source               string          `json:"source"`
	Archive              string          `json:"archive"`
	ArchiveSHA256        string          `json:"archiveSha256"`
	Binary               string          `json:"binary"`
	BinarySHA256         string          `json:"binarySha256"`
	License              string          `json:"license"`
	LicensePath          string          `json:"licensePath"`
	LicenseURL           string          `json:"licenseUrl"`
	Artifacts            json.RawMessage `json:"artifacts"`
}

type enginesMetadata struct {
	Engines []componentMetadata `json:"engines"`
}

type layout struct {
	root, target string
	metadata     map[string]string
	binary       func(component string, record artifactMetadata) string
	license      func(component string, record componentMetadata) string
}

// VerifySourceTree verifies one target using the fixed managed backend layout.
func VerifySourceTree(backendRoot, target string) error {
	l := layout{root: backendRoot, target: target,
		metadata: map[string]string{
			"engines": filepath.Join("engines", "metadata.json"),
			"rclone":  filepath.Join("rclone", "assets", "metadata.json"),
		},
		binary: func(component string, record artifactMetadata) string {
			if component == "restic" || component == "kopia" {
				component = "engines"
			}
			return filepath.Join(backendRoot, component, filepath.FromSlash(record.Binary))
		},
		license: func(component string, record componentMetadata) string {
			if component == "restic" || component == "kopia" {
				component = "engines"
			}
			return filepath.Join(backendRoot, component, filepath.FromSlash(record.LicensePath))
		},
	}
	return verify(l, "")
}

// VerifySourceTreeForAcquisition verifies the selected target's unaffected
// state before one selected component binary and its acquisition provenance are
// replaced. The selected component's metadata and license remain in scope.
func VerifySourceTreeForAcquisition(backendRoot, target, selectedComponent string) error {
	if componentIdentities[selectedComponent].repository == "" {
		return fmt.Errorf("unsupported component")
	}
	l := layout{root: backendRoot, target: target,
		metadata: map[string]string{
			"engines": filepath.Join("engines", "metadata.json"),
			"rclone":  filepath.Join("rclone", "assets", "metadata.json"),
		},
		binary: func(component string, record artifactMetadata) string {
			if component == "restic" || component == "kopia" {
				component = "engines"
			}
			return filepath.Join(backendRoot, component, filepath.FromSlash(record.Binary))
		},
		license: func(component string, record componentMetadata) string {
			if component == "restic" || component == "kopia" {
				component = "engines"
			}
			return filepath.Join(backendRoot, component, filepath.FromSlash(record.LicensePath))
		},
	}
	return verify(l, selectedComponent)
}

// VerifyStagedKit verifies one target using the fixed credential-free kit layout.
func VerifyStagedKit(kitRoot, target string) error {
	l := layout{root: kitRoot, target: target,
		metadata: map[string]string{
			"engines": filepath.Join("provenance", "engines-metadata.json"),
			"rclone":  filepath.Join("provenance", "rclone-metadata.json"),
		},
		binary: func(_ string, record artifactMetadata) string {
			return filepath.Join(kitRoot, "components", filepath.Base(filepath.FromSlash(record.Binary)))
		},
		license: func(_ string, record componentMetadata) string {
			return filepath.Join(kitRoot, "licenses", "product", filepath.Base(filepath.FromSlash(record.LicensePath)))
		},
	}
	return verify(l, "")
}

// SelectArtifact selects one exact component and target from the fixed managed
// source-tree metadata without requiring the selected binary to exist.
func SelectArtifact(backendRoot, component, target string) (Artifact, error) {
	if !supportedTargets[target] || componentIdentities[component].repository == "" {
		return Artifact{}, fmt.Errorf("unsupported component or target")
	}
	record, err := readSelectedComponent(backendRoot, component)
	if err != nil {
		return Artifact{}, err
	}
	a, err := selectedArtifact(record, target)
	if err != nil {
		return Artifact{}, fmt.Errorf("component target is missing: %s %s", component, target)
	}
	if err := validateComponentRecord(component, record, target, a); err != nil {
		return Artifact{}, err
	}
	checksum, err := checksumURL(component, record.Version, a.Archive)
	if err != nil {
		return Artifact{}, err
	}
	archiveURL, _ := url.Parse(a.Archive)
	archiveFilename := strings.TrimPrefix(archiveURL.EscapedPath(), "/"+componentIdentities[component].owner+"/"+componentIdentities[component].repository+"/releases/download/v"+strings.TrimPrefix(record.Version, "v")+"/")
	return Artifact{Component: component, Target: target, Version: record.Version,
		Repository: repositoryURL(component), Release: record.Source, ArchiveURL: a.Archive,
		ArchiveSHA256: strings.ToLower(a.ArchiveSHA256), ArchiveFilename: archiveFilename,
		BinaryPath: filepath.FromSlash(a.Binary), BinaryFilename: filepath.Base(filepath.FromSlash(a.Binary)),
		BinarySHA256: strings.ToLower(a.BinarySHA256), ChecksumURL: checksum}, nil
}

func verify(l layout, skipComponentBinary string) error {
	if !supportedTargets[l.target] {
		return fmt.Errorf("unsupported target %q", l.target)
	}
	components, err := readMetadata(l.root, l.metadata, l.target)
	if err != nil {
		return err
	}
	for _, id := range []string{"restic", "kopia", "rclone"} {
		record, ok := components[id]
		if !ok {
			return fmt.Errorf("component metadata identity is missing: %s", id)
		}
		a, err := selectedArtifact(record, l.target)
		if err != nil {
			return fmt.Errorf("component %s target is missing: %s", id, l.target)
		}
		if err := validateComponentRecord(id, record, l.target, a); err != nil {
			return err
		}
		if id != skipComponentBinary {
			binaryPath := l.binary(id, a)
			if err := regularContained(l.root, binaryPath); err != nil {
				return fmt.Errorf("%s binary: %w", id, err)
			}
			if err := verifySHA256(binaryPath, a.BinarySHA256); err != nil {
				return fmt.Errorf("%s binary: %w", id, err)
			}
			if err := ValidateBinaryTarget(binaryPath, l.target); err != nil {
				return fmt.Errorf("%s binary target: %w", id, err)
			}
		}
		licensePath := l.license(id, record)
		if err := nonemptyRegularContained(l.root, licensePath); err != nil {
			return fmt.Errorf("%s license: %w", id, err)
		}
	}
	return nil
}

func readMetadata(root string, names map[string]string, target string) (map[string]componentMetadata, error) {
	read := func(name string) ([]byte, error) {
		path := filepath.Join(root, names[name])
		if err := regularContained(root, path); err != nil {
			return nil, fmt.Errorf("%s metadata: %w", name, err)
		}
		data, err := readBoundedFile(path, maxComponentMetadataSize)
		if err != nil {
			return nil, err
		}
		return data, nil
	}
	components := map[string]componentMetadata{}
	rcloneData, err := read("rclone")
	if err != nil {
		return nil, err
	}
	r, err := decodeComponentMetadata(rcloneData, "rclone", target)
	if err != nil {
		return nil, fmt.Errorf("rclone metadata: %w", err)
	}
	components[r.ID] = r
	enginesData, err := read("engines")
	if err != nil {
		return nil, err
	}
	engineRows, err := decodeNamedArrayDocument(enginesData, "engines")
	if err != nil {
		return nil, fmt.Errorf("engines metadata: %w", err)
	}
	if len(engineRows) != 2 {
		return nil, fmt.Errorf("engine metadata must contain exactly restic and kopia")
	}
	for _, raw := range engineRows {
		id, _, err := decodeRowIdentity(raw)
		if err != nil || id != "restic" && id != "kopia" || components[id].ID != "" {
			return nil, fmt.Errorf("unknown or duplicate engine identity %q: %v", id, err)
		}
		engine, err := decodeComponentMetadata(raw, id, target)
		if err != nil {
			return nil, fmt.Errorf("engine %s metadata: %w", id, err)
		}
		components[engine.ID] = engine
	}
	return components, nil
}

func readSelectedComponent(root, component string) (componentMetadata, error) {
	var empty componentMetadata
	var relative string
	switch component {
	case "rclone":
		relative = filepath.Join("rclone", "assets", "metadata.json")
	case "restic", "kopia":
		relative = filepath.Join("engines", "metadata.json")
	default:
		return empty, fmt.Errorf("unsupported component")
	}
	filename := filepath.Join(root, relative)
	if err := regularContained(root, filename); err != nil {
		return empty, err
	}
	data, err := readBoundedFile(filename, maxComponentMetadataSize)
	if err != nil {
		return empty, err
	}
	if component == "rclone" {
		return decodeComponentMetadata(data, component, "")
	}
	rows, err := decodeNamedArrayDocument(data, "engines")
	if err != nil {
		return empty, err
	}
	found := 0
	identities := map[string]bool{}
	for _, item := range rows {
		identity, _, identityErr := decodeRowIdentity(item)
		if identityErr != nil || identity != "restic" && identity != "kopia" || identities[identity] {
			return empty, fmt.Errorf("engine identity is invalid or duplicated")
		}
		identities[identity] = true
		if identity != component {
			continue
		}
		found++
		empty, err = decodeComponentMetadata(item, component, "")
		if err != nil {
			return componentMetadata{}, err
		}
	}
	if found != 1 || len(rows) != 2 || !identities["restic"] || !identities["kopia"] {
		return componentMetadata{}, fmt.Errorf("selected component identity is missing or duplicated")
	}
	return empty, nil
}

var componentRecordMembers = memberSet(
	"id", "version", "reviewedAt", "latestStableObserved", "pinReason", "source", "archive",
	"archiveSha256", "binary", "binarySha256", "license", "licensePath", "licenseUrl", "artifacts",
)

func memberSet(names ...string) map[string]bool {
	result := make(map[string]bool, len(names))
	for _, name := range names {
		result[name] = true
	}
	return result
}

// decodeObjectMembers validates only one JSON object frame. Values remain raw
// until the selected target or selected component consumes them, so semantic
// members inside unrelated bounded rows cannot gate selected-target work.
func decodeObjectMembers(data []byte, allowed map[string]bool, allowUnknown bool) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, fmt.Errorf("expected JSON object")
	}
	result := map[string]json.RawMessage{}
	seenFolded := map[string]string{}
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, fmt.Errorf("invalid JSON object key")
		}
		folded := strings.ToLower(key)
		if previous, exists := seenFolded[folded]; exists {
			return nil, fmt.Errorf("duplicate or case-alias JSON members %q and %q", previous, key)
		}
		if canonical, exists := canonicalJSONMembers[folded]; exists && canonical != key {
			return nil, fmt.Errorf("non-canonical JSON member %q (expected %q)", key, canonical)
		}
		if !allowUnknown && !allowed[key] {
			return nil, fmt.Errorf("unknown JSON member %q", key)
		}
		seenFolded[folded] = key
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return nil, err
		}
		result[key] = append(json.RawMessage(nil), raw...)
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return nil, fmt.Errorf("unterminated JSON object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON data")
	}
	return result, nil
}

func decodeRawArray(data []byte) ([]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var rows []json.RawMessage
	if err := decoder.Decode(&rows); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("trailing JSON data")
	}
	return rows, nil
}

func decodeNamedArrayDocument(data []byte, name string) ([]json.RawMessage, error) {
	members, err := decodeObjectMembers(data, memberSet(name), false)
	if err != nil {
		return nil, err
	}
	raw, ok := members[name]
	if !ok {
		return nil, fmt.Errorf("missing %s array", name)
	}
	return decodeRawArray(raw)
}

func decodeRowIdentity(data []byte) (string, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return "", false, fmt.Errorf("expected JSON row object")
	}
	var id string
	seenID := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return "", false, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return "", false, fmt.Errorf("invalid JSON row key")
		}
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			return "", false, err
		}
		switch strings.ToLower(key) {
		case "id":
			if key != "id" || seenID || json.Unmarshal(raw, &id) != nil || id == "" {
				return "", false, fmt.Errorf("row identity is invalid, duplicated, or non-canonical")
			}
			seenID = true
		}
	}
	if token, err = decoder.Token(); err != nil || token != json.Delim('}') {
		return "", false, fmt.Errorf("unterminated JSON row object")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return "", false, fmt.Errorf("trailing JSON row data")
	}
	if !seenID {
		return "", false, fmt.Errorf("row identity is missing")
	}
	return id, false, nil
}

func decodeTargetMap(data []byte, target string) (map[string]json.RawMessage, error) {
	rows, err := decodeObjectMembers(data, supportedTargets, false)
	if err != nil {
		return nil, err
	}
	if target != "" {
		selected, ok := rows[target]
		if !ok {
			return nil, fmt.Errorf("selected target row is missing")
		}
		return map[string]json.RawMessage{target: selected}, nil
	}
	return rows, nil
}

func decodeComponentMetadata(data []byte, expectedID, target string) (componentMetadata, error) {
	members, err := decodeObjectMembers(data, componentRecordMembers, false)
	if err != nil {
		return componentMetadata{}, err
	}
	rawID, hasID := members["id"]
	if hasID {
		var id string
		if err := json.Unmarshal(rawID, &id); err != nil || id != expectedID {
			return componentMetadata{}, fmt.Errorf("component identity is present but conflicts with %q", expectedID)
		}
	} else if expectedID == "restic" || expectedID == "kopia" {
		return componentMetadata{}, fmt.Errorf("engine identity is missing or non-canonical")
	}
	var envelope componentMetadataEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return componentMetadata{}, err
	}
	artifacts, err := decodeTargetMap(envelope.Artifacts, target)
	if err != nil {
		return componentMetadata{}, fmt.Errorf("component artifacts: %w", err)
	}
	return componentMetadata{
		ID: expectedID, Version: envelope.Version, ReviewedAt: envelope.ReviewedAt,
		LatestStableObserved: envelope.LatestStableObserved, PinReason: envelope.PinReason, Source: envelope.Source,
		Archive: envelope.Archive, ArchiveSHA256: envelope.ArchiveSHA256, Binary: envelope.Binary,
		BinarySHA256: envelope.BinarySHA256, License: envelope.License, LicensePath: envelope.LicensePath,
		LicenseURL: envelope.LicenseURL, Artifacts: artifacts,
	}, nil
}

func selectedArtifact(record componentMetadata, target string) (artifactMetadata, error) {
	raw, ok := record.Artifacts[target]
	if !ok {
		return artifactMetadata{}, fmt.Errorf("selected target row is missing")
	}
	var artifact artifactMetadata
	if err := strictJSON(raw, &artifact); err != nil {
		return artifactMetadata{}, err
	}
	if artifact.Target != target {
		return artifactMetadata{}, fmt.Errorf("selected target identity mismatch")
	}
	return artifact, nil
}

func strictJSON(data []byte, target any) error {
	if err := rejectDuplicateJSONMembers(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

func validateComponentRecord(id string, record componentMetadata, target string, a artifactMetadata) error {
	identity := componentIdentities[id]
	if record.ID != id || identity.repository == "" || record.Version == "" || record.LatestStableObserved == "" ||
		(record.Version != record.LatestStableObserved && strings.TrimSpace(record.PinReason) == "") ||
		record.Source == "" || record.Archive == "" || !validHash(record.ArchiveSHA256) ||
		record.Binary == "" || !validHash(record.BinarySHA256) || record.License == "" ||
		record.LicensePath == "" || record.LicenseURL == "" || a.Target != target ||
		!validHash(a.ArchiveSHA256) || !validHash(a.BinarySHA256) || a.Binary == "" || a.Archive == "" {
		return fmt.Errorf("component %s metadata is incomplete", id)
	}
	if err := safeRelativePath(record.Binary); err != nil {
		return fmt.Errorf("component %s compatibility binary path: %w", id, err)
	}
	if err := safeRelativePath(a.Binary); err != nil {
		return fmt.Errorf("component %s binary path: %w", id, err)
	}
	if err := safeRelativePath(record.LicensePath); err != nil {
		return fmt.Errorf("component %s license path: %w", id, err)
	}
	if err := validateOfficialGitHubURL(record.Source, id, record.Version, "source"); err != nil {
		return fmt.Errorf("component %s release identity is not canonical", id)
	}
	if err := validateOfficialGitHubURL(record.Archive, id, record.Version, "archive"); err != nil {
		return fmt.Errorf("component %s compatibility archive identity is not canonical", id)
	}
	if err := validateOfficialGitHubURL(record.LicenseURL, id, record.Version, "license"); err != nil {
		return fmt.Errorf("component %s license identity is not canonical", id)
	}
	if err := validateOfficialGitHubURL(a.Archive, id, record.Version, "archive"); err != nil {
		return fmt.Errorf("component %s archive identity is not canonical", id)
	}
	if a.SignatureURL != "" {
		if err := validateOfficialGitHubURL(a.SignatureURL, id, record.Version, "signature"); err != nil {
			return fmt.Errorf("component %s signature identity is not canonical", id)
		}
	}
	base := filepath.Base(filepath.FromSlash(a.Binary))
	if target == "windows_amd64" {
		if filepath.Ext(base) != ".exe" {
			return fmt.Errorf("component %s Windows binary filename is not canonical", id)
		}
		if record.Archive != a.Archive || !strings.EqualFold(record.ArchiveSHA256, a.ArchiveSHA256) ||
			record.Binary != a.Binary || !strings.EqualFold(record.BinarySHA256, a.BinarySHA256) {
			return fmt.Errorf("component %s Windows compatibility metadata does not match its target artifact", id)
		}
	} else if base != id || filepath.Ext(base) != "" {
		return fmt.Errorf("component %s Unix binary filename is not canonical", id)
	}
	return nil
}

func validateOfficialGitHubURL(value, id, version, kind string) error {
	identity := componentIdentities[id]
	if value == "" || value != strings.TrimSpace(value) || identity.repository == "" {
		return fmt.Errorf("official URL is missing or non-canonical")
	}
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil ||
		u.RawQuery != "" || u.Fragment != "" || u.RawPath != "" || u.Path == "" ||
		u.Path != "/"+strings.Trim(u.Path, "/") || strings.Contains(u.Path, "//") {
		return fmt.Errorf("official URL is not canonical")
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] != identity.owner || parts[1] != identity.repository {
		return fmt.Errorf("official repository identity mismatch")
	}
	tag := "v" + strings.TrimPrefix(version, "v")
	valid := false
	switch kind {
	case "source":
		valid = len(parts) == 5 && parts[2] == "releases" && parts[3] == "tag" && parts[4] == tag
	case "archive", "signature":
		valid = len(parts) == 6 && parts[2] == "releases" && parts[3] == "download" && parts[4] == tag &&
			parts[5] != "" && url.PathEscape(parts[5]) == parts[5]
	case "license":
		valid = len(parts) == 5 && parts[2] == "blob" && parts[3] == tag &&
			(parts[4] == "LICENSE" || parts[4] == "COPYING")
	}
	if !valid {
		return fmt.Errorf("official %s URL is not canonical", kind)
	}
	return nil
}

func checksumURL(component, version, archive string) (string, error) {
	u, err := url.Parse(archive)
	if err != nil {
		return "", err
	}
	base := strings.TrimSuffix(u.Path, filepath.Base(u.Path))
	var name string
	switch component {
	case "restic", "rclone":
		name = "SHA256SUMS"
	case "kopia":
		name = "checksums.txt"
	default:
		return "", fmt.Errorf("unsupported component")
	}
	u.Path = base + name
	u.RawPath = ""
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func repositoryURL(component string) string {
	i := componentIdentities[component]
	return fmt.Sprintf("https://github.com/%s/%s", i.owner, i.repository)
}

func regularContained(root, path string) error {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rootInfo, err := os.Lstat(rootAbs)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fmt.Errorf("controlled root is redirected or not a directory")
	}
	pathAbs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(rootAbs, pathAbs)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("path escapes controlled root")
	}
	current := rootAbs
	parts := strings.Split(rel, string(filepath.Separator))
	for index, part := range parts {
		current = filepath.Join(current, part)
		info, statErr := os.Lstat(current)
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path crosses a symbolic link")
		}
		if index != len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("path crosses a non-directory")
		}
		if index == len(parts)-1 && !info.Mode().IsRegular() {
			return fmt.Errorf("path is not a regular file")
		}
	}
	return nil
}

func nonemptyRegularContained(root, path string) error {
	if err := regularContained(root, path); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.Size() == 0 {
		return fmt.Errorf("path is empty")
	}
	return nil
}

func safeRelativePath(path string) error {
	if path == "" || strings.ContainsAny(path, `\:`) || filepath.IsAbs(path) || filepath.VolumeName(path) != "" || filepath.Clean(path) == "." || filepath.Clean(path) == ".." || strings.HasPrefix(filepath.Clean(path), ".."+string(filepath.Separator)) || filepath.ToSlash(filepath.Clean(path)) != path {
		return fmt.Errorf("unsafe relative path")
	}
	return nil
}

func verifySHA256(path, expected string) error {
	actual, err := fileSHA256(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(actual, expected) {
		return fmt.Errorf("SHA-256 mismatch")
	}
	return nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func readBoundedFile(path string, maximum int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("input is not a regular file")
	}
	if info.Size() > maximum {
		return nil, fmt.Errorf("input exceeds %d-byte limit", maximum)
	}
	data, err := io.ReadAll(io.LimitReader(f, maximum+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("input exceeds %d-byte limit", maximum)
	}
	return data, nil
}

func validHash(value string) bool {
	return len(value) == 64 && strings.Trim(strings.ToLower(value), "0123456789abcdef") == ""
}

func hashMapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for key, value := range a {
		if b[key] != value {
			return false
		}
	}
	return true
}

// ValidateBinaryTarget validates one PE, ELF, or Mach-O executable header.
func ValidateBinaryTarget(path, target string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return fmt.Errorf("executable is not a regular file")
	}
	data := make([]byte, min(int64(128<<10), info.Size()))
	n, readErr := f.Read(data)
	if readErr != nil && readErr != io.EOF {
		return readErr
	}
	return validateBinaryHeader(data[:n], target, info.Size())
}

func validateBinaryHeader(data []byte, target string, totalSize int64) error {
	if totalSize <= 0 {
		return fmt.Errorf("empty executable")
	}
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0x7f, 'E', 'L', 'F'}) {
		want := map[string]uint16{"linux_amd64": 0x3e, "linux_arm64": 0xb7}[target]
		if want == 0 || len(data) < 64 || data[4] != 2 || data[5] != 1 || data[6] != 1 ||
			(binary.LittleEndian.Uint16(data[16:18]) != 2 && binary.LittleEndian.Uint16(data[16:18]) != 3) ||
			binary.LittleEndian.Uint16(data[18:20]) != want || binary.LittleEndian.Uint32(data[20:24]) != 1 ||
			binary.LittleEndian.Uint16(data[52:54]) != 64 {
			return fmt.Errorf("malformed or wrong-target ELF64 header")
		}
		programOffset := binary.LittleEndian.Uint64(data[32:40])
		programEntrySize := uint64(binary.LittleEndian.Uint16(data[54:56]))
		programCount := uint64(binary.LittleEndian.Uint16(data[56:58]))
		sectionOffset := binary.LittleEndian.Uint64(data[40:48])
		sectionEntrySize := uint64(binary.LittleEndian.Uint16(data[58:60]))
		sectionCount := uint64(binary.LittleEndian.Uint16(data[60:62]))
		if programOffset < 64 || programEntrySize != 56 || programCount == 0 || programCount > 128 ||
			!tableWithinFile(programOffset, programEntrySize, programCount, uint64(totalSize)) ||
			(sectionCount != 0 && (sectionOffset < 64 || sectionEntrySize != 64 || sectionCount > 4096 || !tableWithinFile(sectionOffset, sectionEntrySize, sectionCount, uint64(totalSize)))) {
			return fmt.Errorf("invalid ELF64 table structure")
		}
		return nil
	}
	if len(data) >= 4 && bytes.Equal(data[:4], []byte{0xcf, 0xfa, 0xed, 0xfe}) {
		want := map[string]uint32{"darwin_amd64": 0x01000007, "darwin_arm64": 0x0100000c}[target]
		if want == 0 || len(data) < 32 || binary.LittleEndian.Uint32(data[4:8]) != want ||
			binary.LittleEndian.Uint32(data[12:16]) != 2 {
			return fmt.Errorf("malformed or wrong-target Mach-O 64 header")
		}
		commands := binary.LittleEndian.Uint32(data[16:20])
		commandsSize := binary.LittleEndian.Uint32(data[20:24])
		if commands == 0 || commands > 4096 || commandsSize < commands*8 || uint64(32)+uint64(commandsSize) > uint64(totalSize) || int64(32+commandsSize) > int64(len(data)) {
			return fmt.Errorf("invalid Mach-O load-command structure")
		}
		position, end := uint32(32), uint32(32)+commandsSize
		for index := uint32(0); index < commands; index++ {
			if position+8 > end {
				return fmt.Errorf("truncated Mach-O load command")
			}
			commandSize := binary.LittleEndian.Uint32(data[position+4 : position+8])
			if commandSize < 8 || commandSize%4 != 0 || position+commandSize > end {
				return fmt.Errorf("invalid Mach-O load command")
			}
			position += commandSize
		}
		if position != end {
			return fmt.Errorf("Mach-O load-command size mismatch")
		}
		return nil
	}
	if len(data) >= 2 && bytes.Equal(data[:2], []byte{'M', 'Z'}) {
		if target != "windows_amd64" || len(data) < 64 {
			return fmt.Errorf("malformed or wrong-target PE header")
		}
		pe := uint64(binary.LittleEndian.Uint32(data[0x3c:0x40]))
		if pe < 64 || pe+24 > uint64(len(data)) || !bytes.Equal(data[pe:pe+4], []byte{'P', 'E', 0, 0}) ||
			binary.LittleEndian.Uint16(data[pe+4:pe+6]) != 0x8664 {
			return fmt.Errorf("malformed or wrong-target PE32+ header")
		}
		sections := uint64(binary.LittleEndian.Uint16(data[pe+6 : pe+8]))
		optionalSize := uint64(binary.LittleEndian.Uint16(data[pe+20 : pe+22]))
		characteristics := binary.LittleEndian.Uint16(data[pe+22 : pe+24])
		optional := pe + 24
		if sections == 0 || sections > 96 || optionalSize != 0xf0 || optional+optionalSize > uint64(len(data)) ||
			binary.LittleEndian.Uint16(data[optional:optional+2]) != 0x20b || characteristics&0x0002 == 0 ||
			binary.LittleEndian.Uint32(data[optional+108:optional+112]) != 16 {
			return fmt.Errorf("invalid PE32+ COFF or optional header")
		}
		sectionTable := optional + optionalSize
		if sectionTable+sections*40 > uint64(len(data)) {
			return fmt.Errorf("truncated PE32+ section table")
		}
		for index := uint64(0); index < sections; index++ {
			section := sectionTable + index*40
			rawSize := uint64(binary.LittleEndian.Uint32(data[section+16 : section+20]))
			rawOffset := uint64(binary.LittleEndian.Uint32(data[section+20 : section+24]))
			if rawSize != 0 && (rawOffset == 0 || rawOffset+rawSize > uint64(totalSize)) {
				return fmt.Errorf("invalid PE32+ section range")
			}
		}
		return nil
	}
	return fmt.Errorf("unrecognized executable format")
}

func tableWithinFile(offset, entrySize, count, total uint64) bool {
	return offset <= total && entrySize != 0 && count <= (total-offset)/entrySize
}

func rejectDuplicateJSONMembers(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			seenFolded := map[string]string{}
			for decoder.More() {
				keyToken, err := decoder.Token()
				if err != nil {
					return err
				}
				key, ok := keyToken.(string)
				if !ok {
					return fmt.Errorf("invalid JSON object key")
				}
				if seen[key] {
					return fmt.Errorf("duplicate JSON member %q", key)
				}
				folded := strings.ToLower(key)
				if previous, exists := seenFolded[folded]; exists {
					return fmt.Errorf("case-alias JSON members %q and %q", previous, key)
				}
				if canonical, exists := canonicalJSONMembers[folded]; exists && canonical != key {
					return fmt.Errorf("non-canonical JSON member %q (expected %q)", key, canonical)
				}
				seen[key] = true
				seenFolded[folded] = key
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := walk(); err != nil {
					return err
				}
			}
			_, err = decoder.Token()
			return err
		default:
			return fmt.Errorf("unexpected JSON delimiter")
		}
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("trailing JSON data")
	}
	return nil
}

var canonicalJSONMembers = func() map[string]string {
	members := []string{
		"advanced", "archive", "archiveSha256", "artifacts", "binary", "binarySha256",
		"capabilities", "credential", "default", "description", "engines", "help", "id",
		"integrations", "key", "kind", "label", "latestStableObserved", "license",
		"licensePath", "licenseUrl", "localBrowser", "options", "pinReason", "placeholder", "required",
		"reviewedAt", "schema", "secret", "signatureUrl", "source", "target",
		"upstreamChecksum", "version",
	}
	result := make(map[string]string, len(members))
	for _, member := range members {
		result[strings.ToLower(member)] = member
	}
	return result
}()

// ComponentIDs and Targets return stable copies for narrow callers and tests.
func ComponentIDs() []string {
	result := []string{"restic", "kopia", "rclone"}
	return result
}
func Targets() []string {
	result := make([]string, 0, len(supportedTargets))
	for target := range supportedTargets {
		result = append(result, target)
	}
	sort.Strings(result)
	return result
}
