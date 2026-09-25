//go:build linux

package locale

import (
	"os"
	"strings"
)

func systemPreferredLanguages() []string {
	// LANGUAGE is the user's ordered gettext preference. LC_ALL and
	// LC_MESSAGES override LANG when LANGUAGE is absent.
	if value := os.Getenv("LANGUAGE"); value != "" {
		return strings.Split(value, ":")
	}
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if value := os.Getenv(key); value != "" {
			return []string{value}
		}
	}
	return nil
}
