package models

import (
	"fmt"
	"strings"
)

const (
	ConcurrencyReduced   = "reduced"
	ConcurrencyNative    = "native"
	ConcurrencyIncreased = "increased"
	ConcurrencyMaximum   = "maximum"
)

func ValidConcurrencyMode(value string) bool {
	switch value {
	case ConcurrencyReduced, ConcurrencyNative, ConcurrencyIncreased, ConcurrencyMaximum:
		return true
	default:
		return false
	}
}

// NormalizeConcurrencyMode supplies the stable default only when an API or
// fresh-database caller omitted the new preference. Persisted sidecar records
// use ValidConcurrencyMode directly so malformed remote state fails closed.
func NormalizeConcurrencyMode(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return ConcurrencyNative, nil
	}
	if !ValidConcurrencyMode(value) {
		return "", fmt.Errorf("concurrency mode must be reduced, native, increased, or maximum")
	}
	return value, nil
}

// NormalizeConcurrencyModeForConnector preserves compatibility with saved or
// recovered high-speed preferences that these native transports cannot use.
// Validation happens first so an unknown value is never hidden by fallback.
func NormalizeConcurrencyModeForConnector(connector, value string) (string, error) {
	mode, err := NormalizeConcurrencyMode(value)
	if err != nil {
		return "", err
	}
	switch connector {
	case "dropbox", "google_drive", "onedrive":
		if mode == ConcurrencyIncreased || mode == ConcurrencyMaximum {
			return ConcurrencyNative, nil
		}
	}
	return mode, nil
}
