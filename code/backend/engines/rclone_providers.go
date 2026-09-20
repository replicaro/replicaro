package engines

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

const (
	RcloneProviderRoot = "Replicaro"
	RcloneRootPrefix   = RcloneProviderRoot + "/"
)

type RcloneProviderDefinition struct {
	StorageType    string
	NativeType     string
	Label          string
	Fields         []string
	SecretFields   map[string]bool
	ObscuredFields map[string]bool
	IdentityFields []string
}

// These fields are bound to pinned rclone 1.75.1's native configuration.
// Custom OAuth client fields are intentionally absent.
var rcloneProviders = []RcloneProviderDefinition{
	{
		StorageType: "google_drive", NativeType: "drive", Label: "Google Drive",
		Fields:         []string{"token", "scope", "root_folder_id", "team_drive"},
		SecretFields:   secretSet("token"),
		IdentityFields: []string{"root_folder_id", "team_drive"},
	},
	{
		StorageType: "dropbox", NativeType: "dropbox", Label: "Dropbox",
		Fields:         []string{"token", "root_namespace"},
		SecretFields:   secretSet("token"),
		IdentityFields: []string{"root_namespace"},
	},
	{
		StorageType: "onedrive", NativeType: "onedrive", Label: "OneDrive",
		Fields:         []string{"token", "region", "drive_id", "drive_type", "root_folder_id", "tenant"},
		SecretFields:   secretSet("token"),
		IdentityFields: []string{"region", "drive_id", "drive_type", "root_folder_id", "tenant"},
	},
}

func secretSet(values ...string) map[string]bool {
	result := make(map[string]bool, len(values))
	for _, value := range values {
		result[value] = true
	}
	return result
}

func RcloneProviders() []RcloneProviderDefinition {
	result := make([]RcloneProviderDefinition, len(rcloneProviders))
	copy(result, rcloneProviders)
	return result
}

func RcloneProvider(storageType string) (RcloneProviderDefinition, bool) {
	storageType = strings.ToLower(strings.TrimSpace(storageType))
	for _, provider := range rcloneProviders {
		if provider.StorageType == storageType {
			return provider, true
		}
	}
	return RcloneProviderDefinition{}, false
}

func IsResticRcloneConnector(storageType string) bool {
	_, ok := RcloneProvider(storageType)
	return ok
}

func ValidateRcloneFolderName(value string) (normalized, root string, err error) {
	normalized = norm.NFC.String(strings.TrimSpace(value))
	if normalized == "" {
		return "", "", fmt.Errorf("vault folder name is required")
	}
	if normalized == "." || normalized == ".." {
		return "", "", fmt.Errorf("vault folder name must be one folder component")
	}
	if utf8.RuneCountInString(normalized) > 50 {
		return "", "", fmt.Errorf("vault folder name exceeds 50 Unicode code points")
	}
	if strings.ContainsAny(normalized, `/\`) {
		return "", "", fmt.Errorf("vault folder name must not contain a path separator")
	}
	for _, character := range normalized {
		if unicode.IsControl(character) || character == 0x7f {
			return "", "", fmt.Errorf("vault folder name contains a control character")
		}
	}
	if strings.HasSuffix(normalized, " ") || strings.HasSuffix(normalized, ".") {
		return "", "", fmt.Errorf("vault folder name has a provider-unsafe ending")
	}
	root = RcloneRootPrefix + normalized
	if utf8.RuneCountInString(root) > 61 || len([]byte(root)) > 210 {
		return "", "", fmt.Errorf("generated vault root exceeds its fixed safety bound")
	}
	return normalized, root, nil
}

func ParseRcloneGeneratedRoot(value string) (normalized, root string, err error) {
	root = norm.NFC.String(strings.TrimSpace(value))
	if !strings.HasPrefix(root, RcloneRootPrefix) {
		return "", "", fmt.Errorf("Restic rclone location must be Replicaro/<vault folder name>")
	}
	normalized, expected, err := ValidateRcloneFolderName(strings.TrimPrefix(root, RcloneRootPrefix))
	if err != nil {
		return "", "", err
	}
	if root != expected {
		return "", "", fmt.Errorf("Restic rclone location must be the exact normalized generated root")
	}
	return normalized, root, nil
}

func validateRcloneProviderOptions(provider RcloneProviderDefinition, values map[string]string) error {
	allowed := make(map[string]bool, len(provider.Fields))
	for _, field := range provider.Fields {
		allowed[field] = true
	}
	for key, value := range values {
		if !allowed[key] {
			return fmt.Errorf("unsupported pinned %s field %q", provider.Label, key)
		}
		if len(value) > rcloneMaxProviderValueBytes {
			return fmt.Errorf("pinned %s field %q exceeds its size bound", provider.Label, key)
		}
		if strings.ContainsAny(key, "\r\n=[]") || strings.ContainsAny(value, "\x00\r\n") {
			return fmt.Errorf("invalid pinned %s field %q", provider.Label, key)
		}
	}
	return nil
}

func requireRcloneProviderIdentityOptions(provider RcloneProviderDefinition, values map[string]string) error {
	switch provider.StorageType {
	case "dropbox":
		if strings.TrimSpace(values["root_namespace"]) == "" {
			return fmt.Errorf("native Dropbox namespace identity is required")
		}
	case "google_drive":
		if strings.TrimSpace(values["team_drive"]) == "" &&
			strings.TrimSpace(values["root_folder_id"]) == "" {
			return fmt.Errorf("native Google Drive root identity is required")
		}
	case "onedrive":
		if strings.TrimSpace(values["drive_id"]) == "" {
			return fmt.Errorf("native OneDrive drive identity is required")
		}
	}
	return nil
}

func NormalizeRcloneProviderOptions(storageType string, values map[string]string) (map[string]string, error) {
	provider, ok := RcloneProvider(storageType)
	if !ok {
		return nil, fmt.Errorf("%w: Restic rclone provider %q", ErrUnsupported, storageType)
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		if provider.SecretFields[key] {
			result[key] = value
		} else {
			result[key] = strings.TrimSpace(value)
		}
	}
	if err := validateRcloneProviderOptions(provider, result); err != nil {
		return nil, err
	}
	if err := requireRcloneProviderIdentityOptions(provider, result); err != nil {
		return nil, err
	}
	return result, nil
}
