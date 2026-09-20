package engines

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func rejectRcloneCaseAlias(parent, exact string) error {
	entries, err := os.ReadDir(parent)
	if err != nil {
		return err
	}
	matches := 0
	for _, entry := range entries {
		if strings.EqualFold(entry.Name(), exact) {
			matches++
			if entry.Name() != exact {
				return invalidRcloneLocalBinding(fmt.Errorf("private rclone session path has a case-conflicting alias: %s", filepath.Join(parent, entry.Name())))
			}
		}
	}
	if matches > 1 {
		return invalidRcloneLocalBinding(fmt.Errorf("private rclone session path is case-ambiguous below %s", parent))
	}
	return nil
}
