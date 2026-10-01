package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/integrations"
)

// SupportPrivacyContext is the narrow configured-value input used only while
// producing the explicitly public support export. It deliberately excludes
// ordinary names and identifiers.
type SupportPrivacyContext struct {
	Paths   []string
	Hosts   []string
	Secrets []string
	// SecretFragments are already encoded and must be matched literally.
	SecretFragments []string
}

type supportPrivacyQueryer interface {
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// LoadSupportPrivacyContext reads configured locations and decoded credentials
// through the same codec used by repository loading. Callers must still apply
// their own resource bounds before constructing report redaction patterns.
func LoadSupportPrivacyContext(query supportPrivacyQueryer, valueLimit, byteLimit, valueByteLimit int) (SupportPrivacyContext, error) {
	var result SupportPrivacyContext
	if valueLimit <= 0 || byteLimit <= 0 || valueByteLimit <= 0 {
		return result, fmt.Errorf("support privacy context has an invalid safe bound")
	}
	if err := preflightSupportPrivacyContext(query, byteLimit, valueByteLimit); err != nil {
		return result, err
	}
	valueCount, byteCount := 0, 0
	appendValues := func(destination *[]string, values ...string) error {
		for _, value := range values {
			if value == "" {
				continue
			}
			if len(value) > valueByteLimit || valueCount >= valueLimit || byteCount+len(value) > byteLimit {
				return fmt.Errorf("support privacy context exceeds its safe bound")
			}
			*destination = append(*destination, value)
			valueCount++
			byteCount += len(value)
		}
		return nil
	}
	seenFragments := map[string]bool{}
	appendFragments := func(fragments []string) error {
		for _, fragment := range fragments {
			if seenFragments[fragment] {
				continue
			}
			if err := appendValues(&result.SecretFragments, fragment); err != nil {
				return err
			}
			seenFragments[fragment] = true
		}
		return nil
	}
	repositories, err := query.Query(`SELECT connector,location,resolved_repository_path,passphrase,pending_passphrase,connector_options FROM repositories`)
	if err != nil {
		return result, fmt.Errorf("read support repository privacy context: %w", err)
	}
	for repositories.Next() {
		var connector, location, resolvedPath, storedPassphrase, storedPending, storedOptions string
		if err := repositories.Scan(&connector, &location, &resolvedPath, &storedPassphrase, &storedPending, &storedOptions); err != nil {
			_ = repositories.Close()
			return result, err
		}
		passphrase, options, err := decodeRepositorySecrets(connector, storedPassphrase, storedOptions)
		if err != nil {
			_ = repositories.Close()
			return result, fmt.Errorf("decode support repository privacy context: %w", err)
		}
		if err := appendValues(&result.Paths, location, resolvedPath); err != nil {
			_ = repositories.Close()
			return result, err
		}
		if err := appendValues(&result.Hosts, supportConfiguredHosts(location, options)...); err != nil {
			_ = repositories.Close()
			return result, err
		}
		if err := appendValues(&result.Secrets, append([]string{passphrase}, supportCatalogSecretValues(connector, options)...)...); err != nil {
			_ = repositories.Close()
			return result, err
		}
		rclonePaths, rcloneSecrets, rcloneFragments := rcloneRemotePrivacyValues(connector, options, valueByteLimit)
		if err := appendValues(&result.Paths, rclonePaths...); err != nil {
			_ = repositories.Close()
			return result, err
		}
		if err := appendValues(&result.Secrets, rcloneSecrets...); err != nil {
			_ = repositories.Close()
			return result, err
		}
		if err := appendFragments(rcloneFragments); err != nil {
			_ = repositories.Close()
			return result, err
		}
		if storedPending != "" {
			pending, err := decodeSecret(storedPending)
			if err != nil {
				_ = repositories.Close()
				return result, fmt.Errorf("decode pending support privacy context: %w", err)
			}
			if err := appendValues(&result.Secrets, string(pending)); err != nil {
				_ = repositories.Close()
				return result, err
			}
		}
	}
	if err := repositories.Err(); err != nil {
		_ = repositories.Close()
		return result, err
	}
	if err := repositories.Close(); err != nil {
		return result, err
	}

	for _, source := range []struct {
		name  string
		query string
	}{
		{"creation", `SELECT connector,location,reviewed_options_json FROM repository_creation_intents`},
		{"connection", `SELECT connector,location,reviewed_options_json FROM repository_connection_intents`},
	} {
		rows, err := query.Query(source.query)
		if err != nil {
			return result, fmt.Errorf("read support %s privacy context: %w", source.name, err)
		}
		for rows.Next() {
			var connector, location, encodedOptions string
			if err := rows.Scan(&connector, &location, &encodedOptions); err != nil {
				_ = rows.Close()
				return result, err
			}
			options := map[string]string{}
			if err := json.Unmarshal([]byte(encodedOptions), &options); err != nil {
				_ = rows.Close()
				return result, fmt.Errorf("decode support %s privacy context: %w", source.name, err)
			}
			if err := appendValues(&result.Paths, location); err != nil {
				_ = rows.Close()
				return result, err
			}
			if err := appendValues(&result.Hosts, supportConfiguredHosts(location, options)...); err != nil {
				_ = rows.Close()
				return result, err
			}
			if err := appendValues(&result.Secrets, integrations.SecretValues(connector, options)...); err != nil {
				_ = rows.Close()
				return result, err
			}
			// Intents keep only the reviewed settings, so this adds the paths.
			rclonePaths, _, _ := rcloneRemotePrivacyValues(connector, options, valueByteLimit)
			if err := appendValues(&result.Paths, rclonePaths...); err != nil {
				_ = rows.Close()
				return result, err
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return result, err
		}
		if err := rows.Close(); err != nil {
			return result, err
		}
	}

	jobs, err := query.Query(`SELECT source,resolved_source_path,before_script_path,after_script_path FROM backup_jobs`)
	if err != nil {
		return result, fmt.Errorf("read support job privacy context: %w", err)
	}
	for jobs.Next() {
		var source, resolvedPath, beforeScript, afterScript string
		if err := jobs.Scan(&source, &resolvedPath, &beforeScript, &afterScript); err != nil {
			_ = jobs.Close()
			return result, err
		}
		if err := appendValues(&result.Paths, source, resolvedPath, beforeScript, afterScript); err != nil {
			_ = jobs.Close()
			return result, err
		}
	}
	if err := jobs.Err(); err != nil {
		_ = jobs.Close()
		return result, err
	}
	if err := jobs.Close(); err != nil {
		return result, err
	}
	return result, nil
}

// supportCatalogSecretValues is integrations.SecretValues, except that an Any
// Rclone Remote vault's environment variables are left to
// rcloneRemotePrivacyValues, which redacts each value on its own. As one JSON
// list they could pass the per-value bound and fail the whole report.
func supportCatalogSecretValues(connector string, options map[string]string) []string {
	if connector != engines.RcloneRemoteConnector {
		return integrations.SecretValues(connector, options)
	}
	rest := make(map[string]string, len(options))
	for key, value := range options {
		if key != engines.RcloneRemoteEnvironmentOption {
			rest[key] = value
		}
	}
	return integrations.SecretValues(connector, rest)
}

// Environment variable values shorter than this are not redacted. They are
// settings such as RCLONE_TRANSFERS=1 or "true", not credentials, and as plain
// substrings they would blank every matching digit or word in the report.
const supportRcloneVariableMinimumLength = 6

// rcloneRemotePrivacyValues returns what an Any Rclone Remote vault adds to
// the privacy context: the config file path (which often holds the user
// name), the remote name, and the path in remote (which can hold a bucket
// name) as paths, and each environment variable value as a secret on its own,
// because rclone can echo a single value in its own error text. A path in
// remote made only of "/" names nothing private, and redacting it would blank
// every "/" in the report, so it is left out.
//
// A value whose longest representation exceeds the report's per-value bound
// contributes literal fragments of each complete representation instead. Encode
// before splitting: quoting individual raw pieces inserts delimiters that never
// occur inside a quoted native value. Fragments remain bounded and are matched
// against the original detail so overlapping matches cannot expose a remainder.
func rcloneRemotePrivacyValues(connector string, options map[string]string, valueByteLimit int) (paths, secrets, fragments []string) {
	if connector != engines.RcloneRemoteConnector {
		return nil, nil, nil
	}
	paths = []string{options[engines.RcloneRemoteConfigFileOption], options[engines.RcloneRemoteNameOption]}
	if remotePath := options[engines.RcloneRemotePathOption]; strings.Trim(strings.TrimSpace(remotePath), "/") != "" {
		paths = append(paths, remotePath)
	}
	raw := options[engines.RcloneRemoteEnvironmentOption]
	variables, err := engines.ParseRcloneRemoteEnvironment(raw)
	if err != nil {
		// A list that can't be read never reached rclone, because every run
		// checks it first. It is still redacted whole.
		return paths, []string{raw}, nil
	}
	for _, variable := range variables {
		trimmed := strings.TrimSpace(variable.Value)
		if utf8.RuneCountInString(trimmed) < supportRcloneVariableMinimumLength {
			continue
		}
		values := []string{variable.Value}
		if trimmed != variable.Value {
			// Response diagnostics trim their detail before storage. Include
			// that literal too, including for diagnostics already saved.
			values = append(values, trimmed)
		}
		for _, value := range values {
			representations := SupportSecretRepresentations(value)
			longest := 0
			for _, representation := range representations {
				longest = max(longest, len(representation))
			}
			if longest <= valueByteLimit {
				secrets = append(secrets, value)
			} else {
				for _, representation := range representations {
					fragments = append(fragments, splitSupportSecret(representation, max(1, valueByteLimit/8))...)
				}
			}
		}
	}
	return paths, secrets, fragments
}

// SupportSecretRepresentations returns a secret and the escaped forms of it
// that the support report also redacts, because native output can print a
// value URL-escaped, quoted, or JSON-escaped.
func SupportSecretRepresentations(secret string) []string {
	if secret == "" {
		return nil
	}
	encoded, _ := json.Marshal(secret)
	return []string{secret, url.QueryEscape(secret), url.PathEscape(secret), strconv.Quote(secret),
		string(encoded), strings.ReplaceAll(secret, `\`, `\\`)}
}

// splitSupportSecret cuts value into consecutive pieces of about the same
// length, each at most size bytes plus the rest of a character, cut only
// between characters.
func splitSupportSecret(value string, size int) []string {
	count := (len(value) + size - 1) / size
	target := (len(value) + count - 1) / count
	pieces := make([]string, 0, count)
	for len(value) > target {
		cut := target
		for cut < len(value) && !utf8.RuneStart(value[cut]) {
			cut++
		}
		pieces = append(pieces, value[:cut])
		value = value[cut:]
	}
	if value != "" {
		pieces = append(pieces, value)
	}
	return pieces
}

func preflightSupportPrivacyContext(query supportPrivacyQueryer, byteLimit, valueByteLimit int) error {
	var maximum int
	if err := query.QueryRow(`SELECT COALESCE(MAX(value_bytes),0) FROM (
		SELECT length(CAST(location AS BLOB)) AS value_bytes FROM repositories
		UNION ALL SELECT length(CAST(resolved_repository_path AS BLOB)) FROM repositories
		UNION ALL SELECT length(CAST(location AS BLOB)) FROM repository_creation_intents
		UNION ALL SELECT length(CAST(location AS BLOB)) FROM repository_connection_intents
		UNION ALL SELECT length(CAST(source AS BLOB)) FROM backup_jobs
		UNION ALL SELECT length(CAST(resolved_source_path AS BLOB)) FROM backup_jobs
		UNION ALL SELECT length(CAST(before_script_path AS BLOB)) FROM backup_jobs
		UNION ALL SELECT length(CAST(after_script_path AS BLOB)) FROM backup_jobs
	)`).Scan(&maximum); err != nil {
		return fmt.Errorf("measure support privacy path context: %w", err)
	}
	if maximum > valueByteLimit {
		return fmt.Errorf("support privacy context exceeds its safe bound")
	}

	encodedValueLimit := len(secretCodecPrefix) + 2*(valueByteLimit+12)
	if err := query.QueryRow(`SELECT COALESCE(MAX(value_bytes),0) FROM (
		SELECT length(CAST(passphrase AS BLOB)) AS value_bytes FROM repositories
		UNION ALL SELECT length(CAST(pending_passphrase AS BLOB)) FROM repositories
	)`).Scan(&maximum); err != nil {
		return fmt.Errorf("measure support privacy secret context: %w", err)
	}
	if maximum > encodedValueLimit {
		return fmt.Errorf("support privacy context exceeds its safe bound")
	}

	if err := query.QueryRow(`SELECT COALESCE(MAX(value_bytes),0) FROM (
		SELECT length(CAST(connector_options AS BLOB)) AS value_bytes FROM repositories
		UNION ALL SELECT length(CAST(reviewed_options_json AS BLOB)) FROM repository_creation_intents
		UNION ALL SELECT length(CAST(reviewed_options_json AS BLOB)) FROM repository_connection_intents
	)`).Scan(&maximum); err != nil {
		return fmt.Errorf("measure support privacy option context: %w", err)
	}
	if maximum > byteLimit {
		return fmt.Errorf("support privacy context exceeds its safe bound")
	}
	return nil
}

func supportConfiguredHosts(location string, options map[string]string) []string {
	values := []string{strings.TrimSpace(options["host"])}
	for _, candidate := range []string{location, options["endpoint"]} {
		parsed, err := url.Parse(strings.TrimSpace(candidate))
		if err == nil && parsed.Hostname() != "" {
			values = append(values, parsed.Hostname())
		}
	}
	return values
}
