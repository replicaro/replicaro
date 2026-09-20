package storageidentity

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// filesystemSpelling runs only inside the bounded storage helper, after path
// validation. Directory-entry names preserve the route the caller chose; a
// final-handle path or EvalSymlinks would replace mapped drives and links with
// their targets. Spelling is best effort, never a second eligibility gate.
func filesystemSpelling(configured string, creation bool) string {
	return filesystemSpellingWith(configured, creation, entrySpelling)
}

func filesystemSpellingWith(configured string, creation bool, lookup func(string, string, os.FileInfo) (string, error)) string {
	volume := filepath.VolumeName(configured)
	root := volume + string(filepath.Separator)
	parts := strings.Split(strings.TrimPrefix(configured, root), string(filepath.Separator))
	original, selected := root, root
	for index, part := range parts {
		if part == "" {
			continue
		}
		original = filepath.Join(original, part)
		info, err := os.Lstat(original)
		if err != nil {
			if creation && os.IsNotExist(err) {
				// A new destination has no directory-entry spelling for its missing
				// tail. Keep those names exactly as requested; this does not create it.
				return filepath.Join(append([]string{selected}, parts[index:]...)...)
			}
			return configured
		}
		name, err := lookup(selected, part, info)
		if err != nil || !sameEntrySpelling(part, name) || filepath.Base(name) != name {
			return configured
		}
		candidate := filepath.Join(selected, name)
		candidateInfo, err := os.Lstat(candidate)
		if err != nil || !os.SameFile(info, candidateInfo) {
			return configured
		}
		selected = candidate
	}
	return selected
}

func sameEntrySpelling(a, b string) bool {
	// This only filters candidates. The filesystem must also prove SameFile
	// using Lstat, so case folding cannot select another file or another link.
	return strings.EqualFold(norm.NFC.String(a), norm.NFC.String(b))
}

func entrySpelling(parent, requested string, info os.FileInfo) (string, error) {
	directory, err := os.Open(parent)
	if err != nil {
		return "", err
	}
	defer directory.Close()
	match := ""
	for {
		names, readErr := directory.Readdirnames(128)
		for _, name := range names {
			if name == requested {
				return name, nil
			}
			if !sameEntrySpelling(requested, name) {
				continue
			}
			candidate, err := os.Lstat(filepath.Join(parent, name))
			if err == nil && os.SameFile(info, candidate) {
				if match != "" {
					return "", fmt.Errorf("directory-entry spelling is ambiguous")
				}
				match = name
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return "", readErr
		}
	}
	if match == "" {
		return "", fmt.Errorf("directory-entry spelling is unavailable")
	}
	return match, nil
}

// BindingPath validates the helper's optional spelling fact without filesystem
// I/O in the parent. Same-object proof belongs to the bounded helper; this check
// rejects malformed facts and route substitution before descriptor admission.
func (response HelperResponse) BindingPath(request HelperRequest) (string, error) {
	if response.ConfiguredPath == "" {
		return request.Path, nil
	}
	if !request.SelectSpelling || request.Operation == "enumerate" {
		return "", fmt.Errorf("unexpected configured spelling")
	}
	selected := response.ConfiguredPath
	normalized, err := NormalizeConfiguredPath(selected)
	if err != nil || normalized != selected || filepath.VolumeName(selected) != filepath.VolumeName(request.Path) {
		return "", fmt.Errorf("invalid configured spelling")
	}
	before := strings.Split(request.Path, string(filepath.Separator))
	after := strings.Split(selected, string(filepath.Separator))
	if len(before) != len(after) {
		return "", fmt.Errorf("configured spelling changed route")
	}
	for index := range before {
		if !sameEntrySpelling(before[index], after[index]) {
			return "", fmt.Errorf("configured spelling changed route")
		}
	}
	return selected, nil
}
