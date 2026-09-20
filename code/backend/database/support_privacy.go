package database

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/local/replicaro/integrations"
)

// SupportPrivacyContext is the narrow configured-value input used only while
// producing the explicitly public support export. It deliberately excludes
// ordinary names and identifiers.
type SupportPrivacyContext struct {
	Paths   []string
	Hosts   []string
	Secrets []string
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
		if err := appendValues(&result.Secrets, append([]string{passphrase}, integrations.SecretValues(connector, options)...)...); err != nil {
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
