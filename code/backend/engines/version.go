package engines

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

type semanticVersion struct {
	major, minor, patch int
	pre, build          string
}

func (version semanticVersion) String() string {
	return fmt.Sprintf("%d.%d.%d%s", version.major, version.minor, version.patch, version.pre)
}

var semanticVersionPattern = regexp.MustCompile(`(?i)(^|[^0-9A-Za-z])v?([0-9]+)\.([0-9]+)\.([0-9]+)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?($|[^0-9A-Za-z])`)
var resticVersionPattern = regexp.MustCompile(`(?i)^\s*restic\s+v?([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)(?:\s+compiled with go[0-9.]+(?:\s+on\s+\S+)?)?\s*$`)
var kopiaVersionPattern = regexp.MustCompile(`(?i)^\s*v?([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)\s+build:\s+[^\s]+\s+from:\s+kopia/kopia\s*$`)
var kopiaLegacyVersionPattern = regexp.MustCompile(`(?i)^\s*kopia\s+v?([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?(?:\+[0-9A-Za-z.-]+)?)\s*$`)

func parseSemanticVersion(output string) (semanticVersion, error) {
	matches := semanticVersionPattern.FindAllStringSubmatch(output, -1)
	if len(matches) != 1 {
		if len(matches) == 0 {
			return semanticVersion{}, fmt.Errorf("no semantic version found")
		}
		return semanticVersion{}, fmt.Errorf("ambiguous semantic version output")
	}
	match := matches[0]
	major, _ := strconv.Atoi(match[2])
	minor, _ := strconv.Atoi(match[3])
	patch, _ := strconv.Atoi(match[4])
	return semanticVersion{major: major, minor: minor, patch: patch, pre: match[5], build: match[6]}, nil
}

func semanticVersionEqual(left, right string) bool {
	a, errA := parseSemanticVersion(left)
	b, errB := parseSemanticVersion(right)
	return errA == nil && errB == nil && a.major == b.major && a.minor == b.minor && a.patch == b.patch && a.pre == b.pre
}

func parseEngineVersion(engine, output string) (semanticVersion, error) {
	var pattern *regexp.Regexp
	switch engine {
	case ResticID:
		pattern = resticVersionPattern
	case KopiaID:
		pattern = kopiaVersionPattern
	default:
		return semanticVersion{}, fmt.Errorf("version output is for the wrong engine")
	}
	trimmed := strings.TrimSpace(output)
	match := pattern.FindStringSubmatch(trimmed)
	if engine == KopiaID && len(match) != 2 {
		match = kopiaLegacyVersionPattern.FindStringSubmatch(trimmed)
	}
	if len(match) != 2 {
		return semanticVersion{}, fmt.Errorf("version output is not a verified %s form", engine)
	}
	return parseSemanticVersion(match[1])
}
