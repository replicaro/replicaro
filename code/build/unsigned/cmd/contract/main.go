package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	inputsSchema   = "replicaro-unsigned-build-inputs-v1"
	sourceSchema   = "replicaro-public-source-v1"
	artifactSchema = "replicaro-unsigned-artifacts-v1"
	maxJSON        = 8 << 20
)

type sourceManifest struct {
	Schema string       `json:"schema"`
	Files  []sourceFile `json:"files"`
}

type sourceFile struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	LFS  bool   `json:"lfs,omitempty"`
}

type buildInputs struct {
	Schema     string                 `json:"schema"`
	Version    string                 `json:"version"`
	Toolchains map[string]string      `json:"toolchains"`
	Targets    map[string]targetInput `json:"targets"`
}

type targetInput struct {
	Artifacts []string `json:"artifacts"`
}

type artifactManifest struct {
	Schema         string         `json:"schema"`
	Target         string         `json:"target"`
	Version        string         `json:"version"`
	SourceIdentity string         `json:"sourceIdentity"`
	BuildTimestamp string         `json:"buildTimestamp"`
	InputsSHA256   string         `json:"inputsSha256"`
	Artifacts      []artifactFile `json:"artifacts"`
}

type artifactFile struct {
	Path   string `json:"path"`
	Mode   string `json:"mode"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "unsigned build contract:", err)
		os.Exit(1)
	}
}

func run(arguments []string, output io.Writer) error {
	if len(arguments) == 0 {
		return errors.New("command is required: source, inputs, timestamp, write, verify, or compare")
	}
	switch arguments[0] {
	case "source":
		flags := flag.NewFlagSet("source", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		root := flags.String("root", "", "source root")
		manifest := flags.String("manifest", "", "public source manifest")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("source requires --root and --manifest")
		}
		identity, err := sourceIdentity(*root, *manifest)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, identity)
		return err
	case "inputs":
		flags := flag.NewFlagSet("inputs", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		name := flags.String("file", "", "inputs file")
		field := flags.String("field", "", "version or a toolchain name")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("inputs requires --file and --field")
		}
		inputs, _, err := loadInputs(*name)
		if err != nil {
			return err
		}
		var value string
		switch *field {
		case "version":
			value = inputs.Version
		default:
			value = inputs.Toolchains[*field]
		}
		if value == "" {
			return fmt.Errorf("unknown or empty build input field %q", *field)
		}
		_, err = fmt.Fprintln(output, value)
		return err
	case "timestamp":
		flags := flag.NewFlagSet("timestamp", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		value := flags.String("value", "", "canonical UTC build timestamp")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("timestamp requires --value")
		}
		epoch, err := buildTimestampEpoch(*value)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, epoch)
		return err
	case "write":
		flags := flag.NewFlagSet("write", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		root := flags.String("root", "", "release artifact root")
		inputsName := flags.String("inputs", "", "inputs file")
		target := flags.String("target", "", "build target")
		identity := flags.String("source-identity", "", "canonical public source identity")
		buildTimestamp := flags.String("build-timestamp", "", "canonical UTC build timestamp")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("write requires --root, --inputs, --target, --source-identity, and --build-timestamp")
		}
		manifest, err := writeArtifacts(*root, *inputsName, *target, *identity, *buildTimestamp)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(output, manifest)
		return err
	case "compare":
		flags := flag.NewFlagSet("compare", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		left := flags.String("left", "", "first artifact manifest")
		right := flags.String("right", "", "second artifact manifest")
		allowMacOSDMGVariance := flags.Bool("allow-macos-dmg-container-variance", false, "permit only macOS DMG size and digest differences after separate payload verification")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("compare requires --left and --right")
		}
		if err := compareArtifacts(*left, *right, *allowMacOSDMGVariance); err != nil {
			return err
		}
		message := "unsigned artifact manifests are byte-for-byte identical"
		if *allowMacOSDMGVariance {
			message = "unsigned artifact manifests match except macOS DMG container size and digest"
		}
		_, err := fmt.Fprintln(output, message)
		return err
	case "verify":
		flags := flag.NewFlagSet("verify", flag.ContinueOnError)
		flags.SetOutput(io.Discard)
		root := flags.String("root", "", "downloaded artifact root")
		manifest := flags.String("manifest", "", "artifact manifest")
		if err := flags.Parse(arguments[1:]); err != nil || flags.NArg() != 0 {
			return errors.New("verify requires --root and --manifest")
		}
		if err := verifyArtifacts(*root, *manifest); err != nil {
			return err
		}
		_, err := fmt.Fprintln(output, "unsigned artifacts match their manifest")
		return err
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func sourceIdentity(root, manifestName string) (string, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(manifestName) {
		return "", errors.New("source root and manifest must be absolute")
	}
	manifest := sourceManifest{}
	if err := decodeJSON(manifestName, &manifest); err != nil {
		return "", fmt.Errorf("load source manifest: %w", err)
	}
	if manifest.Schema != sourceSchema || len(manifest.Files) == 0 {
		return "", errors.New("public source manifest has the wrong schema or is empty")
	}
	hash := sha256.New()
	previous := ""
	for _, item := range manifest.Files {
		if !safeRelative(item.Path) || (item.Mode != "100644" && item.Mode != "100755") || item.Path <= previous {
			return "", fmt.Errorf("public source manifest contains an invalid or unsorted path %q", item.Path)
		}
		previous = item.Path
		name := filepath.Join(root, filepath.FromSlash(item.Path))
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("public source input is missing or redirected: %s", item.Path)
		}
		fileHash, err := hashFile(name)
		if err != nil {
			return "", err
		}
		mode, _ := strconv.ParseUint(item.Mode[3:], 8, 32)
		fmt.Fprintf(hash, "%s\x00%06o\x00%d\x00%s\n", item.Path, mode, info.Size(), fileHash)
	}
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}

func loadInputs(name string) (buildInputs, string, error) {
	result := buildInputs{}
	if !filepath.IsAbs(name) {
		return result, "", errors.New("inputs path must be absolute")
	}
	if err := decodeJSON(name, &result); err != nil {
		return result, "", err
	}
	if result.Schema != inputsSchema || result.Version == "" || len(result.Targets) != 5 {
		return result, "", errors.New("unsigned build inputs are incomplete")
	}
	for target, input := range result.Targets {
		if target == "" || len(input.Artifacts) == 0 {
			return result, "", errors.New("unsigned build target is incomplete")
		}
		previous := ""
		for _, artifact := range input.Artifacts {
			expanded := strings.ReplaceAll(artifact, "{version}", result.Version)
			if !safeRelative(artifact) || strings.Contains(artifact, "/") || expanded <= previous {
				return result, "", fmt.Errorf("target %s has an invalid or unsorted artifact %q", target, artifact)
			}
			previous = expanded
		}
	}
	digest, err := hashFile(name)
	return result, "sha256:" + digest, err
}

func writeArtifacts(root, inputsName, target, identity, buildTimestamp string) (string, error) {
	if !filepath.IsAbs(root) || !filepath.IsAbs(inputsName) {
		return "", errors.New("artifact root and inputs must be absolute")
	}
	if !validSHA256(identity) {
		return "", errors.New("source identity must be canonical sha256")
	}
	if _, err := buildTimestampEpoch(buildTimestamp); err != nil {
		return "", err
	}
	inputs, inputsHash, err := loadInputs(inputsName)
	if err != nil {
		return "", err
	}
	targetInputs, ok := inputs.Targets[target]
	if !ok {
		return "", fmt.Errorf("unsupported target %q", target)
	}
	artifacts := make([]artifactFile, 0, len(targetInputs.Artifacts))
	wanted := make(map[string]struct{}, len(targetInputs.Artifacts))
	for _, template := range targetInputs.Artifacts {
		relative := strings.ReplaceAll(template, "{version}", inputs.Version)
		wanted[relative] = struct{}{}
		name := filepath.Join(root, relative)
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() == 0 {
			return "", fmt.Errorf("contracted artifact is missing, empty, or redirected: %s", relative)
		}
		digest, err := hashFile(name)
		if err != nil {
			return "", err
		}
		artifacts = append(artifacts, artifactFile{Path: relative, Mode: fmt.Sprintf("%04o", info.Mode().Perm()), Size: info.Size(), SHA256: "sha256:" + digest})
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if _, ok := wanted[entry.Name()]; !ok {
			return "", fmt.Errorf("unexpected file in release artifact root: %s", entry.Name())
		}
	}
	manifest := artifactManifest{Schema: artifactSchema, Target: target, Version: inputs.Version, SourceIdentity: identity, BuildTimestamp: buildTimestamp, InputsSHA256: inputsHash, Artifacts: artifacts}
	data, err := json.Marshal(manifest)
	if err != nil {
		return "", err
	}
	data = append(data, '\n')
	manifestName := filepath.Join(root, "artifact-manifest-v1.json")
	if err := writeExclusive(manifestName, data, 0o644); err != nil {
		return "", err
	}
	var sums bytes.Buffer
	for _, artifact := range artifacts {
		fmt.Fprintf(&sums, "%s  %s\n", strings.TrimPrefix(artifact.SHA256, "sha256:"), artifact.Path)
	}
	if err := writeExclusive(filepath.Join(root, "SHA256SUMS"), sums.Bytes(), 0o644); err != nil {
		_ = os.Remove(manifestName)
		return "", err
	}
	return manifestName, nil
}

func compareArtifacts(leftName, rightName string, allowMacOSDMGVariance bool) error {
	left, right := artifactManifest{}, artifactManifest{}
	if err := decodeJSON(leftName, &left); err != nil {
		return fmt.Errorf("load first manifest: %w", err)
	}
	if err := decodeJSON(rightName, &right); err != nil {
		return fmt.Errorf("load second manifest: %w", err)
	}
	if left.Schema != artifactSchema || right.Schema != artifactSchema {
		return errors.New("artifact manifest has the wrong schema")
	}
	if allowMacOSDMGVariance {
		if left.Target != right.Target || (left.Target != "darwin_amd64" && left.Target != "darwin_arm64") || left.Version != right.Version {
			return errors.New("macOS DMG variance requires matching macOS targets and versions")
		}
		dmg := "Replicaro-" + left.Version + "-" + left.Target + ".dmg"
		portable := "Replicaro-" + left.Version + "-" + left.Target + "-portable.zip"
		for _, manifest := range []*artifactManifest{&left, &right} {
			if len(manifest.Artifacts) != 2 || manifest.Artifacts[0].Path != portable || manifest.Artifacts[1].Path != dmg {
				return errors.New("macOS DMG variance requires the exact portable ZIP and DMG inventory")
			}
			if manifest.Artifacts[1].Size <= 0 || !validSHA256(manifest.Artifacts[1].SHA256) {
				return errors.New("macOS DMG variance requires a complete original DMG digest")
			}
			// Each caller verifies the original manifest against its files. Only the
			// DMG container fields may differ after a separate mounted-content check.
			manifest.Artifacts[1].Size = 0
			manifest.Artifacts[1].SHA256 = ""
		}
	}
	leftData, _ := json.Marshal(left)
	rightData, _ := json.Marshal(right)
	if !bytes.Equal(leftData, rightData) {
		return errors.New("unsigned artifact manifests differ")
	}
	return nil
}

func verifyArtifacts(root, manifestName string) error {
	if !filepath.IsAbs(root) || !filepath.IsAbs(manifestName) {
		return errors.New("artifact root and manifest must be absolute")
	}
	manifest := artifactManifest{}
	if err := decodeJSON(manifestName, &manifest); err != nil {
		return err
	}
	if manifest.Schema != artifactSchema || manifest.Target == "" || manifest.Version == "" ||
		!validSHA256(manifest.SourceIdentity) || !validSHA256(manifest.InputsSHA256) || len(manifest.Artifacts) == 0 {
		return errors.New("artifact manifest is incomplete")
	}
	if _, err := buildTimestampEpoch(manifest.BuildTimestamp); err != nil {
		return fmt.Errorf("artifact manifest build timestamp: %w", err)
	}
	wanted := map[string]struct{}{"artifact-manifest-v1.json": {}, "SHA256SUMS": {}}
	previous := ""
	var sums bytes.Buffer
	for _, artifact := range manifest.Artifacts {
		if !safeRelative(artifact.Path) || strings.Contains(artifact.Path, "/") || artifact.Path <= previous ||
			!validSHA256(artifact.SHA256) || artifact.Size <= 0 {
			return errors.New("artifact manifest contains an invalid or unsorted entry")
		}
		previous = artifact.Path
		wanted[artifact.Path] = struct{}{}
		name := filepath.Join(root, artifact.Path)
		info, err := os.Lstat(name)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() != artifact.Size {
			return fmt.Errorf("artifact size or identity differs: %s", artifact.Path)
		}
		digest, err := hashFile(name)
		if err != nil || "sha256:"+digest != artifact.SHA256 {
			return fmt.Errorf("artifact bytes differ: %s", artifact.Path)
		}
		fmt.Fprintf(&sums, "%s  %s\n", digest, artifact.Path)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if _, ok := wanted[entry.Name()]; !ok || entry.IsDir() {
			return fmt.Errorf("downloaded artifact root has an unexpected entry: %s", entry.Name())
		}
	}
	actualSums, err := os.ReadFile(filepath.Join(root, "SHA256SUMS"))
	if err != nil || !bytes.Equal(actualSums, sums.Bytes()) {
		return errors.New("SHA256SUMS differs from the artifact manifest")
	}
	return nil
}

func decodeJSON(name string, destination any) error {
	info, err := os.Lstat(name)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > maxJSON {
		return errors.New("JSON input is missing, redirected, or oversized")
	}
	file, err := os.Open(name)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxJSON+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return errors.New("JSON input has trailing data")
	}
	return nil
}

func hashFile(name string) (string, error) {
	file, err := os.Open(name)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func writeExclusive(name string, data []byte, mode fs.FileMode) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(name)
		return err
	}
	return file.Close()
}

func safeRelative(name string) bool {
	return name != "" && name == path.Clean(name) && !strings.HasPrefix(name, "/") && !strings.Contains(name, "\\") && name != "." && !strings.HasPrefix(name, "../")
}

func validSHA256(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

func buildTimestampEpoch(value string) (int64, error) {
	const layout = "2006-01-02T15:04:05Z"
	parsed, err := time.Parse(layout, value)
	if err != nil || parsed.Format(layout) != value || parsed.Unix() <= 0 {
		return 0, errors.New("build timestamp must be canonical UTC YYYY-MM-DDTHH:MM:SSZ")
	}
	return parsed.Unix(), nil
}
