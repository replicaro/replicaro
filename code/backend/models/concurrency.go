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
