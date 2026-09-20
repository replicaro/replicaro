package integrations

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
)

// The API and repository wizard both consume this connector manifest.

//go:embed catalog.json
var catalogData []byte

type Option struct {
	Key         string `json:"key"`
	Label       string `json:"label"`
	Help        string `json:"help,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	Default     string `json:"default,omitempty"`
	Required    bool   `json:"required,omitempty"`
	Secret      bool   `json:"secret,omitempty"`
	Credential  bool   `json:"credential,omitempty"`
	Advanced    bool   `json:"advanced,omitempty"`
}

type Integration struct {
	ID           string   `json:"id"`
	ReviewedAt   string   `json:"reviewedAt"`
	Label        string   `json:"label"`
	Description  string   `json:"description"`
	Placeholder  string   `json:"placeholder"`
	Capabilities []string `json:"capabilities"`
	LocalBrowser bool     `json:"localBrowser,omitempty"`
	Options      []Option `json:"options"`
}

func (i Integration) Supports(capability string) bool {
	for _, supported := range i.Capabilities {
		if supported == capability {
			return true
		}
	}
	return false
}

type Catalog struct {
	ReviewedAt   string        `json:"reviewedAt"`
	Integrations []Integration `json:"integrations"`
}

var (
	catalogOnce sync.Once
	catalog     Catalog
	catalogErr  error
)

func Load() (Catalog, error) {
	catalogOnce.Do(func() {
		catalogErr = json.Unmarshal(catalogData, &catalog)
		if catalogErr == nil && len(catalog.Integrations) == 0 {
			catalogErr = fmt.Errorf("integration catalog is empty")
		}
	})
	return catalog, catalogErr
}

func Find(id string) (Integration, bool) {
	c, err := Load()
	if err != nil {
		return Integration{}, false
	}
	for _, integration := range c.Integrations {
		if integration.ID == id {
			return integration, true
		}
	}
	return Integration{}, false
}

func NormalizeOptions(integration Integration, provided map[string]string) (map[string]string, error) {
	allowed := make(map[string]Option, len(integration.Options))
	result := make(map[string]string, len(integration.Options))
	for _, option := range integration.Options {
		allowed[option.Key] = option
		if option.Default != "" {
			result[option.Key] = option.Default
		}
	}
	for key, value := range provided {
		option, ok := allowed[key]
		if !ok {
			return nil, fmt.Errorf("unsupported %s option %q", integration.Label, key)
		}
		trimmed := strings.TrimSpace(value)
		if option.Kind == "boolean" && trimmed != "" && trimmed != "true" && trimmed != "false" {
			return nil, fmt.Errorf("%s must be true or false", option.Label)
		}
		if trimmed != "" || integration.ID == "s3" && key == "root" && value != "" {
			// S3 root is a literal bucket/prefix address. Edge whitespace in an
			// admitted prefix names different objects and must survive normalization.
			if option.Credential || option.Secret || integration.ID == "s3" && key == "root" {
				result[key] = value
			} else {
				result[key] = trimmed
			}
		}
	}
	for _, option := range integration.Options {
		if option.Required && strings.TrimSpace(result[option.Key]) == "" {
			return nil, fmt.Errorf("%s is required", option.Label)
		}
	}
	return result, nil
}

func HasCredentials(id string, options map[string]string) bool {
	integration, ok := Find(id)
	if !ok {
		return false
	}
	for _, option := range integration.Options {
		if (option.Credential || option.Secret) && strings.TrimSpace(options[option.Key]) != "" {
			return true
		}
	}
	return false
}

// SecretValues returns option values explicitly classified as credential or
// secret material in the catalog. Option names are intentionally not
// interpreted heuristically.
func SecretValues(id string, options map[string]string) []string {
	integration, ok := Find(id)
	if !ok {
		return nil
	}
	values := make([]string, 0)
	for _, option := range integration.Options {
		if (option.Credential || option.Secret) && strings.TrimSpace(options[option.Key]) != "" {
			values = append(values, options[option.Key])
		}
	}
	return values
}
