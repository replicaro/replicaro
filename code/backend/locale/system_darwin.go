//go:build darwin

package locale

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"time"
)

func systemPreferredLanguages() []string {
	// AppleLanguages is the per-user ordered UI-language list. Defaults may be
	// unavailable in a headless session, where process locale is the fallback.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(ctx, "/usr/bin/defaults", "read", "-g", "AppleLanguages")
	command.WaitDelay = 100 * time.Millisecond
	if output, err := command.Output(); err == nil {
		text := strings.Trim(string(output), "() \t\r\n")
		var result []string
		for _, field := range strings.Split(text, ",") {
			field = strings.Trim(strings.TrimSpace(field), `"`)
			if field != "" {
				result = append(result, field)
			}
		}
		if len(result) > 0 {
			return result
		}
	}
	if ctx.Err() != nil {
		return nil // A stalled lookup cannot delay completion or select a stale fallback.
	}
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if value := os.Getenv(key); value != "" {
			return []string{value}
		}
	}
	return nil
}
