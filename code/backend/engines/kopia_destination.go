package engines

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"runtime"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Kopia restores a selected file to an explicit filename. Translate only that
// destination component for the target host; the archive selection stays exact.
func kopiaDestinationBasename(name, operatingSystem string) string {
	if operatingSystem != "windows" {
		return name
	}
	name = strings.Map(func(r rune) rune {
		if r < 32 || strings.ContainsRune(`<>:"/\|?*`, r) {
			return '_'
		}
		return r
	}, name)
	end := len(name)
	for end > 0 && (name[end-1] == '.' || name[end-1] == ' ') {
		end--
	}
	name = name[:end] + strings.Repeat("_", len(name)-end)
	stem := strings.ToUpper(strings.TrimRight(strings.SplitN(name, ".", 2)[0], " "))
	reserved := stem == "CON" || stem == "PRN" || stem == "AUX" || stem == "NUL" || stem == "CONIN$" || stem == "CONOUT$"
	if strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT") {
		suffix := stem[3:]
		reserved = len(suffix) == 1 && suffix[0] >= '1' && suffix[0] <= '9' || suffix == "¹" || suffix == "²" || suffix == "³"
	}
	if reserved {
		name = "_" + name
	}
	return name
}

// ErrKopiaSelectionIncludesFolder refuses a Kopia restore of several selected
// items when any of them is a folder.
//
// Kopia restores a selected folder's contents straight into the destination,
// not into a folder of that name. With several selected items in one
// destination, two folders would merge there, and a same-named file in one
// would silently overwrite the other's (or a separately selected file), while
// every item still reports restored. Restic keeps each item's archived path,
// so it is not affected. Giving each folder its own subfolder would mean
// Replicaro laying out a restore Kopia did not make; selected-file naming is
// the only destination translation Replicaro does for Kopia. So the request
// is refused instead. A single folder and several regular files stay allowed.
//
// Don't relax this, or replace it with per-folder subfolders, without a
// product decision: it is what keeps a Kopia multi-selection from merging.
var ErrKopiaSelectionIncludesFolder = errors.New("Replicaro can restore multiple files at the same time for Kopia vaults. Replicaro can also restore one folder at a time for Kopia vaults. But Replicaro cannot restore multiple folders or files with folders at the same time for Kopia vaults. This is a Kopia limitation. Please rerun your selective restore accordingly.")

// ErrKopiaSelectedFileDestinationMissing refuses a Kopia restore that
// includes selected files when the destination folder doesn't exist.
//
// Pinned Kopia creates a missing destination folder for a whole snapshot or a
// selected folder, but writes a selected file straight to <folder>/<name>, and
// that fails with "no such file or directory" while nothing is restored.
// Replicaro doesn't create the folder for Kopia, because that would be a
// destination layout Kopia didn't make, so the restore is refused before
// anything runs. Whole-snapshot and folder restores, and every Restic restore,
// keep Kopia's and Restic's own behaviour.
var ErrKopiaSelectedFileDestinationMissing = errors.New("The destination folder doesn't exist. Replicaro can restore selected files only into a folder that already exists. Create the folder or choose an existing one, then try again.")

// RestoreDestinationMissing reports whether nothing exists at a restore
// destination. Any other answer, including an error reading it, is left to
// the engine.
func RestoreDestinationMissing(destination string) bool {
	_, err := os.Stat(destination)
	return errors.Is(err, fs.ErrNotExist)
}

// KopiaRestoreFileNames belongs to one sequential selection request. It never
// inspects disk and never changes overwrite policy. Reserving selected names
// first prevents a generated file_1.txt from consuming an explicit selection.
type KopiaRestoreFileNames struct {
	reserved, assigned []string
	operatingSystem    string
}

func NewKopiaRestoreFileNames(selections []string) *KopiaRestoreFileNames {
	names := &KopiaRestoreFileNames{operatingSystem: runtime.GOOS}
	for _, selection := range selections {
		// An empty selection is a single-file source's version. Its name is
		// only known from the fresh root entry when that item restores, so it
		// reserves nothing here (path.Base would make it "."); it still counts
		// as one of the request's items for the folder check.
		reserved := ""
		if selection != "" {
			reserved = kopiaDestinationBasename(path.Base(selection), runtime.GOOS)
		}
		names.reserved = append(names.reserved, reserved)
	}
	return names
}

// selections is how many items the request selected. It is the only part of
// the request a single item's restore can see, so the folder check in Restore
// uses it.
func (names *KopiaRestoreFileNames) selections() int {
	if names == nil {
		return 0
	}
	return len(names.reserved)
}

// Comparison is local to destination allocation, never source/cache identity.
// Darwin also compares canonical Unicode equivalents: ordinary APFS lookup
// aliases NFC/NFD names, so EqualFold alone can let a later selection overwrite
// an earlier one. Normalize operands only; retain each emitted name's spelling.
// This remains conservative on case-sensitive Windows/macOS volumes and does
// not model every volume's equivalence rules or Linux casefold mounts.
func (names *KopiaRestoreFileNames) contains(values []string, candidate string) bool {
	if names.operatingSystem == "darwin" {
		candidate = norm.NFC.String(candidate)
	}
	for _, value := range values {
		if names.operatingSystem == "darwin" {
			value = norm.NFC.String(value)
		}
		if value == candidate || (names.operatingSystem == "windows" || names.operatingSystem == "darwin") && strings.EqualFold(value, candidate) {
			return true
		}
	}
	return false
}
func (names *KopiaRestoreFileNames) assign(basename string) string {
	candidate := basename
	extension := path.Ext(basename)
	stem := strings.TrimSuffix(basename, extension)
	for suffix := 1; names.contains(names.assigned, candidate); suffix++ {
		candidate = fmt.Sprintf("%s_%d%s", stem, suffix, extension)
		for names.contains(names.reserved, candidate) {
			suffix++
			candidate = fmt.Sprintf("%s_%d%s", stem, suffix, extension)
		}
	}
	names.assigned = append(names.assigned, candidate)
	return candidate
}
