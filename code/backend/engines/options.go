package engines

import (
	"fmt"
	"strings"
	"unicode"

	"github.com/local/replicaro/models"
)

var allowedBackupFlags = map[string]map[string]bool{
	ResticID: {"--verbose": false, "--no-scan": false, "--exclude-caches": false, "--one-file-system": false, "--ignore-ctime": false, "--ignore-inode": false, "--with-atime": false, "--skip-if-unchanged": false},
	KopiaID:  {"--fail-fast": false},
}

var disabledBackupFlags = map[string]bool{
	"--use-fs-snapshot": true,
}

var alwaysProtectedFlags = map[string]bool{
	"--repo": true, "-r": true, "--repository": true, "--password": true, "-p": true, "--password-file": true, "--password-command": true,
	"--config-file": true, "--cache-directory": true, "--source": true, "--target": true, "--to": true, "--tag": true, "--tags": true,
	"--exclude": true, "--exclude-file": true, "--include": true, "--include-file": true, "--path": true, "--json": true, "--output": true,
	"--compression": true, "--no-compression": true, "--encryption": true, "--repository-version": true, "--read-data": true, "--check": true,
	"--stdin-from-command": true, "--stdin-filename": true, "--files-from": true, "--files-from-raw": true, "--files-from-verbatim": true,
	"--time": true, "--override-source": true, "--pin": true, "--action": true, "--policy": true, "--enable-actions": true,
	"--log-file": true, "--log-dir": true, "--output-format": true, "--command": true, "--stdin-command": true,
}

func validateOptions(engine string, options []string) error {
	allowed, known := allowedBackupFlags[engine]
	if !known {
		return fmt.Errorf("unsupported engine %q", engine)
	}
	tokens, err := optionTokens(options)
	if err != nil {
		return fmt.Errorf("%s advanced options: %w", engine, err)
	}
	seen := map[string]bool{}
	for index := 0; index < len(tokens); index++ {
		token := tokens[index]
		if !strings.HasPrefix(token, "-") || token == "-" || strings.HasPrefix(token, "@") {
			return fmt.Errorf("%s advanced options must contain supported flags only", engine)
		}
		flag, hasValue := token, false
		if equal := strings.IndexByte(token, '='); equal >= 0 {
			flag, hasValue = token[:equal], true
		}
		flag = strings.ToLower(flag)
		if disabledBackupFlags[flag] {
			return fmt.Errorf("%s advanced option %q is disabled because VSS filesystem snapshots are not supported", engine, flag)
		}
		// Protect snapshot timestamp override via exact --time above. A substring
		// ban on "time" also rejects the explicitly supported --ignore-ctime and
		// --with-atime flags; the closed allowlist below still rejects unknowns.
		if alwaysProtectedFlags[flag] || strings.Contains(flag, "pass") || strings.Contains(flag, "secret") || strings.Contains(flag, "token") || strings.Contains(flag, "credential") || strings.Contains(flag, "repository") || strings.Contains(flag, "repo") || strings.Contains(flag, "config") || strings.Contains(flag, "limit") || strings.Contains(flag, "concurr") {
			return fmt.Errorf("%s advanced option %q is controlled by Replicaro", engine, flag)
		}
		valueExpected, ok := allowed[flag]
		if !ok || (len(flag) > 2 && strings.HasPrefix(flag, "-") && strings.HasPrefix(flag[1:], "-") == false && len(flag) > 2) {
			return fmt.Errorf("%s advanced option %q is not allowed for backup", engine, flag)
		}
		if valueExpected && !hasValue {
			if index+1 >= len(tokens) {
				return fmt.Errorf("%s advanced option %q is missing its value", engine, flag)
			}
			index++
		} else if !valueExpected && hasValue {
			if !(engine == ResticID && flag == "--verbose" && validResticVerbosity(token[strings.IndexByte(token, '=')+1:])) {
				return fmt.Errorf("%s advanced option %q does not accept a value", engine, flag)
			}
		}
		duplicateAllowed := engine == ResticID && flag == "--verbose"
		if seen[flag] && !duplicateAllowed {
			return fmt.Errorf("%s advanced option %q is duplicated", engine, flag)
		}
		seen[flag] = true
	}
	return nil
}

func validResticVerbosity(value string) bool {
	return value == "0" || value == "1" || value == "2"
}

func optionTokens(lines []string) ([]string, error) {
	var result []string
	for _, line := range lines {
		var token strings.Builder
		quoted := rune(0)
		escaped := false
		flush := func() {
			if token.Len() > 0 {
				result = append(result, token.String())
				token.Reset()
			}
		}
		for _, r := range line {
			if unicode.IsControl(r) {
				return nil, fmt.Errorf("control characters are not allowed")
			}
			if escaped {
				token.WriteRune(r)
				escaped = false
				continue
			}
			if r == '\\' && quoted != '\'' {
				escaped = true
				continue
			}
			if quoted != 0 {
				if r == quoted {
					quoted = 0
				} else {
					token.WriteRune(r)
				}
				continue
			}
			switch r {
			case '\'', '"':
				quoted = r
			case ' ', '\t':
				flush()
			default:
				token.WriteRune(r)
			}
		}
		if escaped || quoted != 0 {
			return nil, fmt.Errorf("malformed quoting")
		}
		flush()
	}
	return result, nil
}

func normalizedOptions(settings models.EngineJobSettings) ([]string, error) {
	options, err := optionTokens(settings.AdditionalOptions)
	if err != nil {
		return nil, err
	}
	return options, nil
}

func validateEngineSettingsSection(id string, value models.EngineJobSettings) error {
	if !models.ValidEngine(id) {
		return fmt.Errorf("unknown engine settings key %q", id)
	}
	return validateOptions(id, value.AdditionalOptions)
}

// ValidatePortableJobSettings requires settings for every represented engine,
// while retaining valid settings for known engines whose vaults are not
// currently connected.
func ValidatePortableJobSettings(settings models.EngineSettings, represented map[string]bool) error {
	for id := range represented {
		if !models.ValidEngine(id) {
			return fmt.Errorf("unknown represented engine %q", id)
		}
		if _, ok := settings[id]; !ok {
			return fmt.Errorf("engine settings for %s are required", id)
		}
	}
	for id, value := range settings {
		if err := validateEngineSettingsSection(id, value); err != nil {
			return err
		}
	}
	return nil
}

func ValidateJobSettings(settings models.EngineSettings, represented map[string]bool) error {
	if err := ValidatePortableJobSettings(settings, represented); err != nil {
		return err
	}
	for id := range settings {
		if !represented[id] {
			return fmt.Errorf("engine settings for %s are not represented by this job's vaults", id)
		}
	}
	if len(settings) != len(represented) {
		return fmt.Errorf("engine settings must contain exactly one section for each represented engine")
	}
	return nil
}
