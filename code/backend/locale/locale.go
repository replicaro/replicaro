// Package locale reads the same catalog files served to the web UI. It owns no
// translated copy of notification text or native-engine output.
package locale

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/local/replicaro/models"
)

type catalog struct {
	Locale   string                     `json:"locale"`
	Messages map[string]json.RawMessage `json:"messages"`
}

var state struct {
	sync.RWMutex
	catalogs map[string]catalog
}

func init() {
	// Source-tree execution has no built UI. Packaged execution replaces this
	// source with the exact embedded or installed UI filesystem at API setup.
	_, file, _, ok := runtime.Caller(0)
	if ok {
		_ = LoadFS(os.DirFS(filepath.Join(filepath.Dir(file), "..", "..", "frontend", "public")))
	}
}

// LoadFS accepts only catalog files from the UI filesystem. A failed load
// leaves the previous complete set intact, so a transient UI lookup cannot
// silently create a partial language selection.
func LoadFS(fsys fs.FS) error {
	paths, err := fs.Glob(fsys, "locales/*.json")
	if err != nil {
		return err
	}
	loaded := make(map[string]catalog)
	for _, path := range paths {
		name := strings.TrimSuffix(filepath.Base(path), ".json")
		// Activation is explicit in the UI, backend, and saved-setting
		// validation. A catalog file alone must not select a language.
		if name == models.LanguageSystem || !models.ValidLanguage(name) {
			continue
		}
		data, err := fs.ReadFile(fsys, path)
		if err != nil {
			return err
		}
		var item catalog
		if err := json.Unmarshal(data, &item); err != nil {
			return err
		}
		if item.Locale != name || len(item.Messages) == 0 {
			continue
		}
		loaded[name] = item
	}
	if _, ok := loaded[models.LanguageEnglish]; !ok {
		return fs.ErrNotExist
	}
	state.Lock()
	state.catalogs = loaded
	state.Unlock()
	return nil
}

func Supported() []string {
	state.RLock()
	defer state.RUnlock()
	result := make([]string, 0, len(state.catalogs))
	for key := range state.catalogs {
		result = append(result, key)
	}
	sort.Strings(result)
	return result
}

// MatchPreferred respects OS preference order. A regional catalog takes
// precedence over its base language; unsupported languages fall through.
// en-GB and en-US share en, and Spanish regions share es. pt-PT and pt-BR
// retain their own catalogs. This chooses a catalog, not a separate formatting
// region: the UI formats values using the selected catalog's locale.
func MatchPreferred(preferred []string, supported []string) string {
	available := make(map[string]string, len(supported))
	for _, value := range supported {
		available[strings.ToLower(value)] = value
	}
	for _, value := range preferred {
		value = normalizeTag(value)
		if value == "" {
			continue
		}
		for candidate := value; candidate != ""; {
			if match, ok := available[strings.ToLower(candidate)]; ok {
				return match
			}
			index := strings.LastIndexByte(candidate, '-')
			if index < 0 {
				break
			}
			candidate = candidate[:index]
		}
		// A regional translation of the same language is still closer than
		// an unrelated later OS preference when no neutral catalog exists.
		// Supported sorts the catalog names, so this fallback is deterministic
		// (currently pt or pt-AO selects pt-BR). It does not infer dialects;
		// Cantonese requires yue, while zh selects the Mandarin catalog.
		base := strings.SplitN(value, "-", 2)[0]
		for _, supported := range supported {
			if strings.HasPrefix(strings.ToLower(supported), strings.ToLower(base)+"-") {
				return supported
			}
		}
	}
	return models.LanguageEnglish
}

func normalizeTag(value string) string {
	value = strings.TrimSpace(strings.SplitN(value, ".", 2)[0])
	value = strings.SplitN(value, "@", 2)[0]
	return strings.ReplaceAll(value, "_", "-")
}

func Effective(selection string) string {
	supported := Supported()
	if selection != models.LanguageSystem {
		for _, candidate := range supported {
			if candidate == selection {
				return candidate
			}
		}
		return models.LanguageEnglish
	}
	if len(supported) == 1 && supported[0] == models.LanguageEnglish {
		return models.LanguageEnglish
	}
	return MatchPreferred(systemPreferredLanguages(), supported)
}

// Text substitutes only named placeholders explicitly present in a catalog
// key. Callers pass user names and paths as values, never as translation keys.
func Text(effective, key string, values map[string]string) string {
	state.RLock()
	item, ok := state.catalogs[effective]
	if !ok {
		item = state.catalogs[models.LanguageEnglish]
	}
	raw := item.Messages[key]
	if len(raw) == 0 {
		raw = state.catalogs[models.LanguageEnglish].Messages[key]
	}
	state.RUnlock()
	var result string
	if json.Unmarshal(raw, &result) != nil {
		return key
	}
	if len(values) > 0 {
		replacements := make([]string, 0, 2*len(values))
		for name, value := range values {
			replacements = append(replacements, "{"+name+"}", value)
		}
		result = strings.NewReplacer(replacements...).Replace(result)
	}
	return result
}
