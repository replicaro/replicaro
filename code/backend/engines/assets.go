package engines

import (
	"os"
)

func detectBinary(id, custom, name string) (string, string) {
	if custom != "" {
		if info, err := os.Stat(custom); err == nil && !info.IsDir() {
			return custom, ""
		}
		return "", "custom path does not exist or is not a file"
	}
	if path, err := extractBinary(id, name); err == nil {
		return path, ""
	}
	return "", "included binary could not be installed"
}

func extractBinary(id, name string) (string, error) {
	return extractEmbeddedBinary(id, name)
}
