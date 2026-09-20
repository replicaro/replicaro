package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

type overlayFile struct {
	Replace map[string]string
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(arguments []string) error {
	flags := flag.NewFlagSet("webuiembed", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	preflightRoot := flags.String("preflight-root", "", "absolute target-owned output root to validate before frontend cleanup")
	frontendOutput := flags.String("frontend-output", "", "exact frontend output path below the preflight root")
	prepareTarget := flags.String("prepare-helper-target", "", "compile a fresh selected-target helper for direct builds and tests")
	dist := flags.String("dist", "", "absolute target-owned Vite output directory")
	outputRoot := flags.String("output-root", "", "absolute target-owned build output root")
	virtualRoot := flags.String("virtual-root", "", "absolute package-local virtual embed directory")
	baseOverlay := flags.String("base-overlay", "", "existing storage-helper overlay")
	helperSource := flags.String("helper-source", "", "virtual package-local storage-helper binary mapped by the base overlay")
	helperBuilt := flags.String("helper-built", "", "target-built storage-helper binary mapped by the base overlay")
	output := flags.String("output", "", "combined product overlay output")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("positional arguments are not accepted")
	}
	if *prepareTarget != "" {
		if *outputRoot == "" || *dist != "" || *virtualRoot != "" || *baseOverlay != "" || *helperSource != "" || *helperBuilt != "" || *output != "" || *preflightRoot != "" || *frontendOutput != "" {
			return errors.New("helper preparation requires exactly -prepare-helper-target and -output-root")
		}
		return prepareHelper(*prepareTarget, *outputRoot)
	}
	if *preflightRoot != "" || *frontendOutput != "" {
		if *preflightRoot == "" || *frontendOutput == "" || *dist != "" || *outputRoot != "" ||
			*virtualRoot != "" || *baseOverlay != "" || *helperSource != "" || *helperBuilt != "" || *output != "" {
			return errors.New("preflight requires exactly -preflight-root and -frontend-output")
		}
		return validateFrontendOutputLocation(*preflightRoot, *frontendOutput, false)
	}
	if *dist == "" || *outputRoot == "" || *virtualRoot == "" || *baseOverlay == "" ||
		*helperSource == "" || *helperBuilt == "" || *output == "" {
		return errors.New("-dist, -output-root, -virtual-root, -base-overlay, -helper-source, -helper-built, and -output are required")
	}
	if err := validateFrontendOutputLocation(*outputRoot, *dist, true); err != nil {
		return err
	}

	distRoot, err := exactAbsoluteDirectory(*dist, "frontend output")
	if err != nil {
		return err
	}
	virtual, err := exactAbsoluteDirectory(*virtualRoot, "virtual embed root")
	if err != nil {
		return err
	}
	base, err := exactAbsoluteFile(*baseOverlay, "base overlay")
	if err != nil {
		return err
	}
	result, err := filepath.Abs(*output)
	if err != nil {
		return fmt.Errorf("resolve product overlay output: %w", err)
	}
	result = filepath.Clean(result)
	if result == base {
		return errors.New("product overlay output must be distinct from the base overlay")
	}
	if err := validateExistingPathComponents(result); err != nil {
		return err
	}
	if info, statErr := os.Lstat(result); statErr == nil {
		linked, linkErr := pathIsLinkLike(result, info)
		if linkErr != nil {
			return linkErr
		}
		if linked || !info.Mode().IsRegular() {
			return errors.New("product overlay output must be a regular file")
		}
	} else if !os.IsNotExist(statErr) {
		return fmt.Errorf("inspect product overlay output: %w", statErr)
	}

	overlay, err := readOverlay(base)
	if err != nil {
		return err
	}
	if err := validateHelperOverlay(overlay, *helperSource, *helperBuilt); err != nil {
		return err
	}
	frontend, err := validateFrontend(distRoot)
	if err != nil {
		return err
	}
	if err := addFrontendFiles(&overlay, distRoot, virtual, frontend); err != nil {
		return err
	}
	return writeOverlay(result, overlay)
}

func validateFrontendOutputLocation(rootPath, outputPath string, requireOutput bool) error {
	root, err := filepath.Abs(rootPath)
	if err != nil {
		return fmt.Errorf("resolve target output root: %w", err)
	}
	root = filepath.Clean(root)
	output, err := filepath.Abs(outputPath)
	if err != nil {
		return fmt.Errorf("resolve frontend output: %w", err)
	}
	output = filepath.Clean(output)
	volumeRoot := filepath.Clean(filepath.VolumeName(root) + string(filepath.Separator))
	if root == volumeRoot {
		return errors.New("target output root must not be a filesystem root")
	}
	if output != filepath.Join(root, "frontend", "dist") {
		return errors.New("frontend output must be the exact frontend/dist path below the target output root")
	}
	if err := validateExistingPathComponents(output); err != nil {
		return err
	}
	if requireOutput {
		info, err := os.Lstat(output)
		if err != nil {
			return fmt.Errorf("inspect frontend output: %w", err)
		}
		if linked, err := pathIsLinkLike(output, info); err != nil {
			return err
		} else if linked || !info.IsDir() {
			return errors.New("frontend output must be a real directory")
		}
	}
	return nil
}

func validateExistingPathComponents(path string) error {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	absolute = filepath.Clean(absolute)
	volume := filepath.VolumeName(absolute)
	root := filepath.Clean(volume + string(filepath.Separator))
	relative := strings.TrimPrefix(absolute, root)
	current := root
	for _, component := range strings.Split(relative, string(filepath.Separator)) {
		if component == "" {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return fmt.Errorf("inspect output path component %s: %w", current, err)
		}
		linked, err := pathIsLinkLike(current, info)
		if err != nil {
			return err
		}
		if linked {
			return fmt.Errorf("output path component must not be a link or reparse point: %s", current)
		}
		if current != absolute && !info.IsDir() {
			return fmt.Errorf("output path ancestor must be a directory: %s", current)
		}
	}
	return nil
}

func exactAbsoluteDirectory(path, label string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	absolute = filepath.Clean(absolute)
	if err := validateExistingPathComponents(absolute); err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", label, err)
	}
	linked, err := pathIsLinkLike(absolute, info)
	if err != nil {
		return "", err
	}
	if linked || !info.IsDir() {
		return "", fmt.Errorf("%s must be a real directory", label)
	}
	return absolute, nil
}

func exactAbsoluteFile(path, label string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve %s: %w", label, err)
	}
	absolute = filepath.Clean(absolute)
	if err := validateExistingPathComponents(absolute); err != nil {
		return "", err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return "", fmt.Errorf("inspect %s: %w", label, err)
	}
	linked, err := pathIsLinkLike(absolute, info)
	if err != nil {
		return "", err
	}
	if linked || !info.Mode().IsRegular() {
		return "", fmt.Errorf("%s must be a regular file", label)
	}
	return absolute, nil
}

func readOverlay(path string) (overlayFile, error) {
	overlay := overlayFile{Replace: make(map[string]string)}
	file, err := os.Open(path)
	if err != nil {
		return overlay, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	opening, err := decoder.Token()
	if err != nil || opening != json.Delim('{') {
		return overlay, errors.New("base overlay must be one JSON object")
	}
	foundReplace := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return overlay, fmt.Errorf("decode base overlay field: %w", err)
		}
		key, ok := keyToken.(string)
		if !ok || key != "Replace" || foundReplace {
			return overlay, errors.New("base overlay must contain exactly one Replace field")
		}
		foundReplace = true
		mapOpening, err := decoder.Token()
		if err != nil || mapOpening != json.Delim('{') {
			return overlay, errors.New("base overlay Replace field must be one object")
		}
		for decoder.More() {
			fromToken, err := decoder.Token()
			if err != nil {
				return overlay, fmt.Errorf("decode base overlay replacement: %w", err)
			}
			from, ok := fromToken.(string)
			if !ok || from == "" {
				return overlay, errors.New("base overlay replacement source must be a nonempty string")
			}
			if _, exists := overlay.Replace[from]; exists {
				return overlay, fmt.Errorf("base overlay contains a duplicate replacement source: %s", from)
			}
			var to string
			if err := decoder.Decode(&to); err != nil || to == "" {
				return overlay, errors.New("base overlay replacement target must be a nonempty string")
			}
			overlay.Replace[from] = to
		}
		if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') {
			return overlay, errors.New("base overlay Replace object is not closed")
		}
	}
	if closing, err := decoder.Token(); err != nil || closing != json.Delim('}') || !foundReplace {
		return overlay, errors.New("base overlay object is incomplete")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return overlay, errors.New("base overlay contains trailing data")
	}
	return overlay, nil
}

func validateHelperOverlay(overlay overlayFile, sourcePath, builtPath string) error {
	source, err := filepath.Abs(sourcePath)
	if err != nil {
		return err
	}
	source = filepath.Clean(source)
	for _, virtual := range []string{source, source + ".sha256"} {
		if err := validateExistingPathComponents(virtual); err != nil {
			return err
		}
		if _, err := os.Lstat(virtual); !os.IsNotExist(err) {
			return fmt.Errorf("storage-helper embed input must remain virtual: %s", virtual)
		}
	}
	built, err := exactAbsoluteFile(builtPath, "target-built storage helper")
	if err != nil {
		return err
	}
	if source == built {
		return errors.New("storage-helper source and target-built binary must be distinct")
	}
	sourceHash := source + ".sha256"
	builtHash, err := exactAbsoluteFile(built+".sha256", "target-built storage-helper checksum")
	if err != nil {
		return err
	}
	for _, path := range []string{built, builtHash} {
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Size() == 0 {
			return fmt.Errorf("storage-helper overlay input is empty: %s", path)
		}
	}
	expected := map[string]string{source: built, sourceHash: builtHash}
	if len(overlay.Replace) != len(expected) {
		return errors.New("base overlay must contain exactly the storage-helper binary and checksum replacements")
	}
	for from, to := range expected {
		if overlay.Replace[from] != to {
			return fmt.Errorf("base overlay does not contain the exact storage-helper replacement for %s", from)
		}
	}
	if err := verifySHA256File(built, builtHash); err != nil {
		return fmt.Errorf("target-built storage helper: %w", err)
	}
	return nil
}

func verifySHA256File(payloadPath, digestPath string) error {
	payload, err := os.ReadFile(payloadPath)
	if err != nil {
		return err
	}
	digest, err := os.ReadFile(digestPath)
	if err != nil {
		return err
	}
	want := strings.TrimSuffix(string(digest), "\n")
	if len(want) != 64 || (string(digest) != want && string(digest) != want+"\n") {
		return errors.New("checksum must be exactly 64 lowercase hexadecimal characters with at most one newline")
	}
	for _, character := range want {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return errors.New("checksum is not lowercase hexadecimal")
		}
	}
	got := fmt.Sprintf("%x", sha256.Sum256(payload))
	if got != want {
		return errors.New("checksum does not match payload")
	}
	return nil
}

type frontendInventory struct {
	files map[string][sha256.Size]byte
}

var localReference = regexp.MustCompile(`(?:src|href)="(/[^"]+)"`)

func validateFrontend(distRoot string) (frontendInventory, error) {
	inventory := frontendInventory{files: make(map[string][sha256.Size]byte)}
	err := filepath.WalkDir(distRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == distRoot {
			return nil
		}
		relative, err := filepath.Rel(distRoot, path)
		if err != nil {
			return err
		}
		slashRelative := filepath.ToSlash(relative)
		if !fs.ValidPath(slashRelative) || strings.Contains(slashRelative, `\`) || !embedEligiblePath(slashRelative) {
			return fmt.Errorf("frontend output path is not eligible for Go embedding: %s", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		linked, err := pathIsLinkLike(path, info)
		if err != nil {
			return err
		}
		if entry.IsDir() {
			if linked {
				return fmt.Errorf("frontend output contains a link or reparse point: %s", relative)
			}
			return nil
		}
		if linked || !info.Mode().IsRegular() {
			return fmt.Errorf("frontend output contains a non-regular file: %s", relative)
		}
		if info.Size() == 0 {
			return fmt.Errorf("frontend output contains an empty file: %s", relative)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		inventory.files[slashRelative] = sha256.Sum256(content)
		return nil
	})
	if err != nil {
		return frontendInventory{}, err
	}
	index, err := os.ReadFile(filepath.Join(distRoot, "index.html"))
	if err != nil {
		return frontendInventory{}, errors.New("frontend output must contain index.html")
	}
	document := string(index)
	if !strings.Contains(document, "<title>Replicaro</title>") || !strings.Contains(document, `<div id="root"></div>`) {
		return frontendInventory{}, errors.New("frontend index is not the Replicaro application shell")
	}
	return validateFrontendReferences(distRoot, document, inventory)
}

func embedEligiblePath(path string) bool {
	for _, component := range strings.Split(path, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasPrefix(component, "_") {
			return false
		}
	}
	return true
}

func validateFrontendReferences(distRoot, document string, inventory frontendInventory) (frontendInventory, error) {
	references := localReference.FindAllStringSubmatch(document, -1)
	seen := make(map[string]int)
	cacheBust := ""
	for _, match := range references {
		parsed, err := url.Parse(match[1])
		if err != nil || !strings.HasPrefix(parsed.Path, "/") || parsed.Path == "/" || parsed.Fragment != "" {
			return frontendInventory{}, fmt.Errorf("frontend index contains an invalid local reference: %s", match[1])
		}
		query := parsed.Query()
		if parsed.RawQuery == "" || query.Get("cb") == "" || len(query) != 1 || len(query["cb"]) != 1 {
			return frontendInventory{}, fmt.Errorf("frontend index local reference is not cache-busted: %s", match[1])
		}
		token := query.Get("cb")
		if cacheBust == "" {
			cacheBust = token
		} else if token != cacheBust {
			return frontendInventory{}, errors.New("frontend index uses inconsistent cache-bust tokens")
		}
		relative := strings.TrimPrefix(parsed.Path, "/")
		if !fs.ValidPath(relative) || !embedEligiblePath(relative) {
			return frontendInventory{}, fmt.Errorf("frontend index reference is not embed eligible: %s", parsed.Path)
		}
		if _, ok := inventory.files[relative]; !ok {
			return frontendInventory{}, fmt.Errorf("frontend index references a missing file: %s", parsed.Path)
		}
		seen[relative]++
	}
	if cacheBust == "" {
		return frontendInventory{}, errors.New("frontend index contains no cache-busted local references")
	}
	requireOne := func(pattern string) (string, error) {
		expression := regexp.MustCompile(pattern)
		matches := []string{}
		for name := range seen {
			if expression.MatchString(name) {
				matches = append(matches, name)
			}
		}
		if len(matches) != 1 || seen[matches[0]] != 1 {
			return "", fmt.Errorf("frontend index must reference exactly one %s", pattern)
		}
		return matches[0], nil
	}
	script, err := requireOne(`^assets/[A-Za-z0-9._-]+[.]js$`)
	if err != nil {
		return frontendInventory{}, err
	}
	if _, err := requireOne(`^assets/[A-Za-z0-9._-]+[.]css$`); err != nil {
		return frontendInventory{}, err
	}
	for _, fixed := range []string{"favicon-teal-64.png", "favicon.ico"} {
		if seen[fixed] != 1 {
			return frontendInventory{}, fmt.Errorf("frontend index must reference cache-busted %s exactly once", fixed)
		}
	}
	wordmark := "replicaro-wordmark-white.png"
	if _, ok := inventory.files[wordmark]; !ok {
		return frontendInventory{}, errors.New("frontend output is missing the fixed wordmark")
	}
	scriptBytes, err := os.ReadFile(filepath.Join(distRoot, filepath.FromSlash(script)))
	if err != nil {
		return frontendInventory{}, err
	}
	if !strings.Contains(string(scriptBytes), "/"+wordmark+"?cb="+cacheBust) {
		return frontendInventory{}, errors.New("frontend JavaScript does not reference the cache-busted fixed wordmark")
	}
	return inventory, nil
}

func addFrontendFiles(overlay *overlayFile, distRoot, virtualRoot string, inventory frontendInventory) error {
	remaining := make(map[string][sha256.Size]byte, len(inventory.files))
	for name, digest := range inventory.files {
		remaining[name] = digest
	}
	pending := make(map[string]string, len(inventory.files))
	err := filepath.WalkDir(distRoot, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == distRoot {
			return nil
		}
		relative, err := filepath.Rel(distRoot, path)
		if err != nil {
			return err
		}
		slashRelative := filepath.ToSlash(relative)
		if entry.IsDir() {
			if _, wasFile := inventory.files[slashRelative]; wasFile {
				return fmt.Errorf("frontend file type changed during overlay creation: %s", relative)
			}
			return nil
		}
		want, ok := inventory.files[slashRelative]
		if !ok {
			return fmt.Errorf("frontend inventory changed during overlay creation: %s", relative)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		linked, err := pathIsLinkLike(path, info)
		if err != nil {
			return err
		}
		if linked || !info.Mode().IsRegular() {
			return fmt.Errorf("frontend file type changed during overlay creation: %s", relative)
		}
		content, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(content) != want {
			return fmt.Errorf("frontend file changed during overlay creation: %s", relative)
		}
		delete(remaining, slashRelative)

		virtualPath := filepath.Clean(filepath.Join(virtualRoot, filepath.FromSlash(slashRelative)))
		inside, err := filepath.Rel(virtualRoot, virtualPath)
		if err != nil || inside == ".." || strings.HasPrefix(inside, ".."+string(filepath.Separator)) {
			return fmt.Errorf("frontend output escapes the virtual root: %s", relative)
		}
		if _, exists := overlay.Replace[virtualPath]; exists {
			return fmt.Errorf("frontend overlay path collides with an existing replacement: %s", relative)
		}
		if _, exists := pending[virtualPath]; exists {
			return fmt.Errorf("frontend output contains a duplicate virtual path: %s", relative)
		}
		if _, err := os.Lstat(virtualPath); err == nil {
			return fmt.Errorf("frontend embed target must remain virtual: %s", relative)
		} else if !os.IsNotExist(err) {
			return err
		}
		pending[virtualPath] = path
		return nil
	})
	if err != nil {
		return err
	}
	if len(remaining) != 0 {
		missing := make([]string, 0, len(remaining))
		for name := range remaining {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return fmt.Errorf("validated frontend inventory disappeared before overlay creation: %s", strings.Join(missing, ", "))
	}
	for virtualPath, physicalPath := range pending {
		overlay.Replace[virtualPath] = physicalPath
	}
	return nil
}

func writeOverlay(path string, overlay overlayFile) error {
	directory := filepath.Dir(path)
	if err := validateExistingPathComponents(directory); err != nil {
		return err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return errors.New("product overlay parent must be a real directory")
	}
	linked, err := pathIsLinkLike(directory, info)
	if err != nil {
		return err
	}
	if linked || !info.IsDir() {
		return errors.New("product overlay parent must be a real directory")
	}
	temporary, err := os.CreateTemp(directory, ".product-overlay-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return err
	}
	encoder := json.NewEncoder(temporary)
	if err := encoder.Encode(overlay); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return nil
}
