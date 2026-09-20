package engines

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
)

// These are native DirManifest fields, not a replacement repository format.
// Keep summaries raw: missing or unusable counters disable the text fast path,
// while native error details remain available in the unchanged captured output.
type kopiaDirectoryManifest struct {
	Stream  string                `json:"stream"`
	Entries []kopiaDirectoryEntry `json:"entries"`
	Summary json.RawMessage       `json:"summary"`
}
type kopiaDirectoryEntry struct {
	Name    string          `json:"name"`
	Type    string          `json:"type"`
	Mode    string          `json:"mode"`
	Size    int64           `json:"size"`
	Mtime   string          `json:"mtime"`
	Object  string          `json:"obj"`
	Summary json.RawMessage `json:"summ"`
}

func readKopiaDirectory(ctx context.Context, repo models.Repository, object string, run kopiaCapturedCommandRunner) (*kopiaDirectoryManifest, string, error) {
	capture, out, err := run(ctx, repo, []string{"show", object}, command.NoTotalDeadline)
	if capture != nil {
		defer capture.Close()
	}
	if err != nil {
		return nil, out, err
	}
	stdout, err := capture.OpenStdout()
	if err != nil {
		return nil, out, kopiaOutputProcessingFailure(repo, err)
	}
	manifest, err := parseKopiaDirectory(stdout)
	_ = stdout.Close()
	return manifest, out, kopiaOutputProcessingFailure(repo, err)
}

func parseKopiaDirectory(reader io.Reader) (*kopiaDirectoryManifest, error) {
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	unique, _, err := readUniqueJSONValueAtPath(decoder, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("parse Kopia directory: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("parse Kopia directory: trailing data")
	}
	fields, ok := unique.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("parse Kopia directory: manifest is not an object")
	}
	if _, present := fields["entries"]; !present {
		return nil, fmt.Errorf("parse Kopia directory: entries are missing")
	}
	data, err := json.Marshal(unique)
	if err != nil {
		return nil, err
	}
	var manifest kopiaDirectoryManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("parse Kopia directory: %w", err)
	}
	if manifest.Stream != "kopia:directory" {
		return nil, fmt.Errorf("parse Kopia directory: unexpected native stream")
	}
	seen := make(map[string]bool, len(manifest.Entries))
	for _, entry := range manifest.Entries {
		if entry.Name == "" || entry.Name == "." || entry.Name == ".." || strings.ContainsAny(entry.Name, "/\x00") || seen[entry.Name] {
			return nil, fmt.Errorf("parse Kopia directory: invalid or duplicate native name %q", entry.Name)
		}
		seen[entry.Name] = true
		if entry.Object == "" && entry.Type != "" || strings.TrimSpace(entry.Object) != entry.Object || strings.ContainsAny(entry.Object, "/\x00\r\n") || strings.HasPrefix(entry.Object, "-") {
			return nil, fmt.Errorf("parse Kopia directory: invalid native object identity")
		}
		if _, err := entry.snapshotEntry(""); err != nil {
			return nil, err
		}
	}
	return &manifest, nil
}

func (entry kopiaDirectoryEntry) snapshotEntry(prefix string) (models.SnapshotEntry, error) {
	var mode os.FileMode
	if entry.Mode != "" {
		parsed, err := strconv.ParseUint(entry.Mode, 8, 32)
		if err != nil {
			return models.SnapshotEntry{}, fmt.Errorf("parse Kopia directory: invalid native mode")
		}
		mode = os.FileMode(parsed)
	}
	switch entry.Type {
	case "f":
	case "d":
		mode |= os.ModeDir
	case "s":
		mode |= os.ModeSymlink
	case "":
		mode = 0 // Native snapshotfs reports zero mode; selected restore still rejects this type.
	default:
		return models.SnapshotEntry{}, fmt.Errorf("parse Kopia directory: unsupported native entry type %q", entry.Type)
	}
	if entry.Size < 0 {
		return models.SnapshotEntry{}, fmt.Errorf("parse Kopia directory: negative size")
	}
	if entry.Mtime != "" {
		if _, err := time.Parse(time.RFC3339Nano, entry.Mtime); err != nil {
			return models.SnapshotEntry{}, fmt.Errorf("parse Kopia directory: invalid mtime")
		}
	}

	// Kopia's snapshotfs loader replaces a directory's stored FileSize with
	// DirSummary.TotalFileSize before list prints it. Match that native view.
	size := entry.Size
	if entry.Type == "d" && len(entry.Summary) > 0 && string(entry.Summary) != "null" {
		var summary struct {
			Size int64 `json:"size"`
		}
		if err := json.Unmarshal(entry.Summary, &summary); err != nil || summary.Size < 0 {
			return models.SnapshotEntry{}, fmt.Errorf("parse Kopia directory: invalid native directory size")
		}
		size = summary.Size
	}
	return models.SnapshotEntry{Name: entry.Name, Path: prefix + entry.Name, Mode: mode.String(), Size: strconv.FormatInt(size, 10), IsDir: entry.Type == "d"}, nil
}

func (manifest *kopiaDirectoryManifest) selectedType(basename string) (kopiaSelectedEntryType, error) {
	for _, entry := range manifest.Entries {
		if entry.Name != basename {
			continue
		}
		switch entry.Type {
		case "f":
			return kopiaSelectedRegularFile, nil
		case "d":
			return kopiaSelectedDirectory, nil
		case "s":
			return kopiaSelectedSymlink, nil
		default:
			return kopiaSelectedUnsupported, nil
		}
	}
	return "", fmt.Errorf("kopia selected path %q was not found in fresh snapshot metadata", basename)
}

func kopiaSummaryEntryCount(raw json.RawMessage) (int64, bool) {
	var summary map[string]json.RawMessage
	if json.Unmarshal(raw, &summary) != nil {
		return 0, false
	}
	var total int64
	for _, key := range []string{"files", "dirs", "symlinks"} {
		count, ok := exactNonnegativeJSONInteger(summary[key])
		if !ok || key == "dirs" && count == 0 || count > math.MaxInt64-total {
			return 0, false
		}
		total += count
	}
	return total - 1, true // Native directories include the requested root.
}

// Count raw LF records before any deduplication. A filename containing LF can
// manufacture another perfectly valid record; syntax alone cannot detect that.
// The native summary is a framing check, not a new integrity guarantee.
func parseCheckedKopiaEntries(reader io.Reader, summary json.RawMessage) ([]models.SnapshotEntry, error) {
	expected, usable := kopiaSummaryEntryCount(summary)
	if !usable {
		return nil, fmt.Errorf("Kopia native summary is unavailable or unusable")
	}
	buffered := bufio.NewReader(reader)
	var entries []models.SnapshotEntry
	seen := map[string]bool{}
	var records int64
	for {
		line, err := buffered.ReadString('\n')
		if err == io.EOF && line == "" {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("Kopia long listing is not LF terminated: %w", err)
		}
		records++
		if records > expected {
			return nil, fmt.Errorf("Kopia long listing record count differs from native summary")
		}
		line = strings.TrimSuffix(line, "\n")
		mode, name, err := parseKopiaLongEntry(line)
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(line)
		if len(fields) < 7 || len(mode) != 10 || !strings.ContainsRune("-dL", rune(mode[0])) {
			return nil, fmt.Errorf("Kopia long listing has an unsupported mode")
		}
		for _, character := range mode[1:] {
			if !strings.ContainsRune("rwx-", character) {
				return nil, fmt.Errorf("Kopia long listing has a nonstandard mode")
			}
		}
		size, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil || size < 0 {
			return nil, fmt.Errorf("Kopia long listing has invalid size")
		}
		if _, err := time.Parse("2006-01-02 15:04:05", fields[2]+" "+fields[3]); err != nil {
			return nil, fmt.Errorf("Kopia long listing has invalid time")
		}
		if mode[0] == 'd' {
			if !strings.HasSuffix(name, "/") {
				return nil, fmt.Errorf("Kopia directory record has no slash")
			}
			name = strings.TrimSuffix(name, "/")
		}
		if normalized, err := normalizeSnapshotRelativePath(name); err != nil || normalized == "" || seen[name] {
			return nil, fmt.Errorf("Kopia long listing has an invalid or duplicate path")
		}
		seen[name] = true
		parts := strings.Split(name, "/")
		entries = append(entries, models.SnapshotEntry{Name: parts[len(parts)-1], Path: name, Mode: mode, Size: fields[1], IsDir: mode[0] == 'd'})
	}
	if records != expected {
		return nil, fmt.Errorf("Kopia long listing record count differs from native summary")
	}
	return entries, nil
}
