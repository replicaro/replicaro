package engines

import (
	"fmt"
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
		names.reserved = append(names.reserved, kopiaDestinationBasename(path.Base(selection), runtime.GOOS))
	}
	return names
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
