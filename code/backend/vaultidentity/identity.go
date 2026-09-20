// Package vaultidentity canonicalizes connector namespaces and consumes the
// filesystem binding selected by storage identity admission.
package vaultidentity

import (
	"fmt"
	"net"
	"net/url"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/local/replicaro/storageidentity"
	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// EffectiveAddress is the single, secret-free interpretation of a connector
// destination used for native translation and remote duplicate admission.
// Managed local operation serialization is keyed separately by vault UUID.
type EffectiveAddress struct {
	Connector   string
	Location    string
	Host        string
	Port        string
	Username    string
	Bucket      string
	Prefix      string
	PathMode    string
	Endpoint    string
	AccountName string
	UseTLS      string
}

func IsFilesystem(connector string) bool {
	return strings.EqualFold(strings.TrimSpace(connector), "fs")
}

// ConnectorLabel returns a safe user-facing connector label derived from the
// same effective address used by storage engines. It never includes credentials.
func ConnectorLabel(connector, location string, options map[string]string) string {
	connector = strings.ToLower(strings.TrimSpace(connector))
	if connector != "s3" {
		return connector
	}
	effective, err := ResolveEffectiveAddress(connector, location, options)
	if err != nil {
		return connector
	}
	endpoint, err := url.Parse(effective.Endpoint)
	if err != nil || endpoint.Hostname() == "" {
		return connector
	}
	host := strings.ToLower(endpoint.Hostname())
	if host == "wasabisys.com" || strings.HasSuffix(host, ".wasabisys.com") {
		host = "wasabisys.com"
	}
	return "s3: " + host
}

// CanonicalLocation returns a safe comparison key. The original location is
// intentionally not returned or stored here: it remains the user-facing value.
func CanonicalLocation(connector, location string) (string, error) {
	if !IsFilesystem(connector) && !strings.EqualFold(connector, "s3") {
		location = strings.TrimSpace(location)
	}
	if location == "" {
		return "", fmt.Errorf("vault location is required")
	}
	if !IsFilesystem(connector) {
		return canonicalConnectorLocation(location), nil
	}
	if err := storageidentity.ValidateConfiguredPath(location); err != nil {
		return "", err
	}
	if runtime.GOOS == "windows" && looksLikeWindowsPath(location) {
		return canonicalWindowsLocation(location)
	}
	abs, err := filepath.Abs(filepath.Clean(location))
	if err != nil {
		return "", fmt.Errorf("canonicalize vault location: %w", err)
	}
	if resolved, resolveErr := resolveExistingPOSIXPrefix(abs); resolveErr == nil {
		abs = resolved
	} else if !os.IsNotExist(resolveErr) {
		return "", fmt.Errorf("resolve vault location: %w", resolveErr)
	}
	abs = filepath.Clean(abs)
	abs = filepath.ToSlash(abs)
	return abs, nil
}

// resolveExistingPOSIXPrefix resolves every existing component of a path and
// appends a nonexistent tail without allowing a symlink in an ancestor to
// change the identity. This mirrors the Windows junction handling above.
func resolveExistingPOSIXPrefix(value string) (string, error) {
	candidate := filepath.Clean(value)
	tail := []string{}
	for {
		if _, err := os.Lstat(candidate); err == nil {
			resolved, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				return "", err
			}
			caseInsensitive := posixCaseSensitivityProbe(resolved)
			if runtime.GOOS == "darwin" || caseInsensitive {
				resolved = darwinFilesystemSpelling(resolved)
			}
			for index := len(tail) - 1; index >= 0; index-- {
				// No filesystem entry exists to prove alternate spelling here.
				// Parent volume behavior cannot establish a future directory's case.
				resolved = filepath.Join(resolved, tail[index])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return value, nil
		}
		tail = append(tail, filepath.Base(candidate))
		candidate = parent
	}
}

// posixCaseSensitivityProbe is deliberately read-only: it compares an
// existing directory entry with a case-toggled alias and never creates files.
// Tests can inject a deterministic answer for synthetic case-insensitive
// volumes.
var posixCaseSensitivityProbe = detectPOSIXCaseInsensitive

func detectPOSIXCaseInsensitive(directory string) bool {
	current := filepath.Clean(directory)
	for {
		name := filepath.Base(current)
		var toggled []rune
		changed := false
		for _, r := range name {
			switch {
			case !changed && r >= 'a' && r <= 'z':
				toggled = append(toggled, unicode.ToUpper(r))
				changed = true
			case !changed && r >= 'A' && r <= 'Z':
				toggled = append(toggled, unicode.ToLower(r))
				changed = true
			default:
				toggled = append(toggled, r)
			}
		}
		if changed {
			alias := filepath.Join(filepath.Dir(current), string(toggled))
			originalInfo, originalErr := os.Stat(current)
			aliasInfo, aliasErr := os.Stat(alias)
			if originalErr == nil && aliasErr == nil {
				return os.SameFile(originalInfo, aliasInfo)
			}
			if originalErr == nil && os.IsNotExist(aliasErr) {
				return false
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			return false
		}
		current = parent
	}
}

// APFS can be case-insensitive while preserving the spelling supplied by the
// caller. Walk the resolved path and use the directory entry spelling so case
// aliases have one stable identity without folding case on case-sensitive
// POSIX systems.
func darwinFilesystemSpelling(value string) string {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return value
	}
	volume := filepath.VolumeName(absolute)
	root := volume + string(filepath.Separator)
	remainder := strings.TrimPrefix(absolute, root)
	current := root
	for _, part := range strings.Split(remainder, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		actual := part
		if entries, readErr := os.ReadDir(current); readErr == nil {
			// Prefer an exact entry on case-sensitive APFS/HFS volumes. Only use
			// case folding when the requested spelling does not exist, which is
			// how a case-insensitive volume exposes an alias spelling.
			exact := false
			for _, entry := range entries {
				if entry.Name() == part {
					actual = entry.Name()
					exact = true
					break
				}
			}
			if !exact {
				for _, entry := range entries {
					if foldFilesystemComponent(entry.Name()) == foldFilesystemComponent(part) {
						requested, requestedErr := os.Lstat(filepath.Join(current, part))
						candidate, candidateErr := os.Lstat(filepath.Join(current, entry.Name()))
						if requestedErr == nil && candidateErr == nil && os.SameFile(requested, candidate) {
							actual = entry.Name()
							break
						}
					}
				}
			}
		}
		current = filepath.Join(current, actual)
	}
	return current
}

// Unicode folding only filters spelling candidates. It cannot establish the
// filesystem's comparison rules; Lstat/SameFile must prove the selected entry.
func foldFilesystemComponent(value string) string {
	return cases.Fold().String(norm.NFD.String(value))
}

func Identity(engine, connector, location string) (string, error) {
	return PhysicalIdentity(connector, location)
}

func IdentityWithOptions(engine, connector, location string, options map[string]string) (string, error) {
	return PhysicalIdentityWithOptions(connector, location, options)
}

// PhysicalIdentity is shared by every engine that addresses the same local
// storage. Engine identity is intentionally kept separate from storage
// identity so changing engines cannot bypass uniqueness or operation locks.
func PhysicalIdentity(connector, location string) (string, error) {
	return PhysicalIdentityWithOptions(connector, location, nil)
}

// PhysicalIdentityWithOptions includes only normalized, non-secret values that
// can change the addressed storage namespace. Credentials and encryption
// material are deliberately excluded.
func PhysicalIdentityWithOptions(connector, location string, options map[string]string) (string, error) {
	effective, err := ResolveEffectiveAddress(connector, location, options)
	if err != nil {
		return "", err
	}
	if effective.Connector == "fs" {
		// Filesystem comparison keys may resolve aliases or fold case; the
		// effective address used for I/O must retain the configured route.
		effective.Location, err = CanonicalLocation("fs", location)
		if err != nil {
			return "", err
		}
	}
	return strings.Join([]string{effective.Connector, effective.Location, effective.Host,
		effective.Port, effective.Username, effective.Bucket, effective.Prefix, effective.PathMode,
		effective.Endpoint, effective.AccountName}, "\x00"), nil
}

// PhysicalIdentityWithStorageKey is the narrow storage-aware entry point for
// filesystem vaults. The validated storage key becomes the existing single
// physical-vault identity; remote connector identity behavior is unchanged.
func PhysicalIdentityWithStorageKey(connector, location, storageKey string, options map[string]string) (string, error) {
	if !IsFilesystem(connector) {
		if strings.TrimSpace(storageKey) != "" {
			return "", fmt.Errorf("storage keys apply only to filesystem vaults")
		}
		return PhysicalIdentityWithOptions(connector, location, options)
	}
	if strings.TrimSpace(storageKey) == "" {
		return "", fmt.Errorf("filesystem storage identity key is required")
	}
	if _, err := storageidentity.ParseCanonicalKey(storageKey); err != nil {
		return "", fmt.Errorf("invalid filesystem storage identity key: %w", err)
	}
	return storageKey, nil
}

func EngineIdentity(engine, connector, location string) (string, error) {
	physical, err := PhysicalIdentity(connector, location)
	if err != nil {
		return "", err
	}
	return strings.ToLower(strings.TrimSpace(engine)) + "\x00" + physical, nil
}

// Bound sources and native historical scopes require exact spelling. Path
// resemblance cannot prove filesystem entry identity, especially on Windows.
func EquivalentSource(left, right string) bool { return left == right }

func looksLikeWindowsPath(value string) bool {
	if len(value) >= 2 && ((value[1] == ':' && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z'))) || strings.HasPrefix(value, `\\`) || strings.HasPrefix(value, "//")) {
		return true
	}
	return false
}

func canonicalWindowsPath(value string) (string, error) {
	value = strings.ReplaceAll(value, "/", `\`)
	if len(value) > 2 && value[1] == ':' && value[2] != '\\' {
		return "", fmt.Errorf("drive-relative Windows paths are not supported")
	}
	if strings.HasPrefix(strings.ToLower(value), `\\?\unc\`) {
		value = `\\` + value[len(`\\?\UNC\`):]
	} else if strings.HasPrefix(value, `\\?\`) {
		value = value[len(`\\?\`):]
	}
	value = strings.TrimRight(value, `\`)
	if len(value) == 2 && value[1] == ':' {
		value += `\`
	}
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == '\\' })
	prefix := ""
	if len(value) >= 2 && value[1] == ':' {
		prefix = value[:2]
	} else if strings.HasPrefix(value, `\\`) {
		prefix = `\\`
	}
	if prefix != "" && prefix != `\\` && len(parts) > 0 && strings.EqualFold(parts[0], value[:2]) {
		parts = parts[1:]
	}
	stack := make([]string, 0, len(parts))
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			return "", fmt.Errorf("path cannot contain '..' components; enter the direct intended route")
		}
		stack = append(stack, part)
	}
	if prefix == `\\` {
		return prefix + strings.Join(stack, `\`), nil
	}
	if prefix != "" {
		if len(stack) == 0 {
			return prefix + `\`, nil
		}
		return prefix + `\` + strings.Join(stack, `\`), nil
	}
	return strings.Join(stack, `\`), nil
}

func canonicalWindowsLocation(value string) (string, error) {
	canonical, err := canonicalWindowsPath(value)
	if err != nil || runtime.GOOS != "windows" {
		return canonical, err
	}
	resolved, resolveErr := resolveExistingWindowsPrefix(strings.ReplaceAll(value, "/", `\`))
	if resolveErr != nil {
		return "", resolveErr
	}
	return canonicalWindowsPath(resolved)
}

// resolveExistingWindowsPrefix resolves junctions and symlinks in the
// existing part of a path, then appends a normalized nonexistent tail.
func resolveExistingWindowsPrefix(value string) (string, error) {
	candidate := filepath.Clean(value)
	tail := []string{}
	for {
		if _, err := os.Lstat(candidate); err == nil {
			resolved, err := filepath.EvalSymlinks(candidate)
			if err != nil {
				return "", err
			}
			for index := len(tail) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, tail[index])
			}
			return resolved, nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			return value, nil
		}
		tail = append(tail, filepath.Base(candidate))
		candidate = parent
	}
}

func canonicalConnectorLocation(value string) string {
	if parsed, err := url.Parse(value); err == nil && parsed.Scheme != "" {
		parsed.Scheme = strings.ToLower(parsed.Scheme)
		parsed.Host = strings.ToLower(parsed.Host)
		parsed.Path = pathpkg.Clean("/" + parsed.Path)
		if parsed.User != nil {
			parsed.User = url.User(parsed.User.Username())
		}
		parsed.RawQuery = ""
		parsed.ForceQuery = false
		parsed.Fragment = ""
		return parsed.String()
	}
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

// ValidateStorageLocation distinguishes a local filesystem name from URL syntax.
// Characters such as #, ? and % have no credential meaning in a local name.
func ValidateStorageLocation(connector, location string) error {
	if connector == "fs" {
		return storageidentity.ValidateConfiguredPath(location)
	}
	return ValidateSafeLocation(location)
}

// ValidateSafeLocation prevents credentials from being persisted in intent
// rows or echoed through their status APIs. Connector credentials belong only
// in catalog fields marked secret.
func ValidateSafeLocation(location string) error {
	parsed, err := url.Parse(strings.TrimSpace(location))
	if err != nil {
		return fmt.Errorf("invalid vault location: %w", err)
	}
	if parsed.User != nil {
		if _, hasPassword := parsed.User.Password(); hasPassword {
			return fmt.Errorf("vault locations cannot contain a password")
		}
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || strings.Contains(location, "#") {
		return fmt.Errorf("vault locations cannot contain query credentials or fragments")
	}
	return nil
}

func cleanRemotePath(parts ...string) string {
	clean := make([]string, 0, len(parts))
	for _, value := range parts {
		value = strings.Trim(strings.ReplaceAll(strings.TrimSpace(value), "\\", "/"), "/")
		if value != "" && value != "." {
			clean = append(clean, value)
		}
	}
	return strings.Join(clean, "/")
}

func validateRemoteNamespacePath(value string) error {
	if strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return fmt.Errorf("remote paths cannot contain control characters")
	}
	if strings.Contains(value, "\\") || strings.Contains(value, "//") {
		return fmt.Errorf("remote paths cannot contain backslashes or repeated separators")
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "." || segment == ".." {
			return fmt.Errorf("remote paths cannot contain dot segments")
		}
	}
	return nil
}

func normalizedEndpoint(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	value = strings.TrimSuffix(canonicalConnectorLocation(value), "/")
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return value
	}
	if (parsed.Scheme == "https" && parsed.Port() == "443") || (parsed.Scheme == "http" && parsed.Port() == "80") {
		if strings.Contains(parsed.Hostname(), ":") {
			parsed.Host = "[" + parsed.Hostname() + "]"
		} else {
			parsed.Host = parsed.Hostname()
		}
		value = strings.TrimSuffix(parsed.String(), "/")
	}
	return value
}

func validateHTTPEndpoint(value string, allowPath bool) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Hostname() == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		(!allowPath && parsed.Path != "" && parsed.Path != "/") {
		return "", fmt.Errorf("endpoint must be an HTTP(S) URL without credentials, query parameters, or fragments")
	}
	return normalizedEndpoint(parsed.String()), nil
}

func endpointWithPort(value, port string) (string, error) {
	port = strings.TrimSpace(port)
	if value == "" || port == "" {
		return value, nil
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Hostname() == "" {
		return "", fmt.Errorf("invalid endpoint")
	}
	if parsed.Port() == "" {
		parsed.Host = net.JoinHostPort(parsed.Hostname(), port)
	}
	return normalizedEndpoint(parsed.String()), nil
}

func normalizedHostPort(hostname, port, useTLS string) string {
	port = normalizedPort(port, useTLS)
	hostname = strings.ToLower(hostname)
	if port != "" {
		return net.JoinHostPort(hostname, port)
	}
	return hostname
}

func normalizedPort(port, useTLS string) string {
	if (useTLS == "true" && port == "443") || (useTLS == "false" && port == "80") {
		return ""
	}
	return port
}

// ResolveEffectiveAddress applies connector precedence rules exactly once.
func ResolveEffectiveAddress(connector, location string, options map[string]string) (EffectiveAddress, error) {
	connector = strings.ToLower(strings.TrimSpace(connector))
	if connector == "" || location == "" {
		return EffectiveAddress{}, fmt.Errorf("vault storage type and location are required")
	}
	if identityFields, rcloneProvider := rcloneProviderIdentityFields[connector]; rcloneProvider &&
		strings.HasPrefix(norm.NFC.String(strings.TrimSpace(location)), "Replicaro/") {
		return resolveRcloneProviderAddress(connector, location, options, identityFields)
	}
	result := EffectiveAddress{Connector: connector}
	if connector == "fs" {
		// Address translation is not identity canonicalization. Resolving a link
		// or lowercasing here would undo the spelling saved at initial binding.
		canonical, err := storageidentity.NormalizeConfiguredPath(location)
		if err != nil {
			return EffectiveAddress{}, err
		}
		result.Location = canonical
		return result, nil
	}
	if connector != "s3" {
		location = strings.TrimSpace(location)
	}
	if location == "" {
		return EffectiveAddress{}, fmt.Errorf("vault storage type and location are required")
	}
	if err := ValidateSafeLocation(location); err != nil {
		return EffectiveAddress{}, err
	}
	parsed, err := url.Parse(location)
	if err != nil {
		return EffectiveAddress{}, fmt.Errorf("invalid vault location: %w", err)
	}
	if connector != "sftp" && parsed.User != nil {
		return EffectiveAddress{}, fmt.Errorf("only SFTP vault locations may contain a username")
	}
	boolValue := func(key string) string { return strings.ToLower(strings.TrimSpace(options[key])) }
	boolDefault := func(key, fallback string) string {
		value := boolValue(key)
		if value == "" {
			return fallback
		}
		return value
	}
	root := options["root"]
	if connector == "s3" || connector == "sftp" || connector == "azblob" || connector == "gcs" {
		if err := validateRemoteNamespacePath(parsed.Path); err != nil {
			return EffectiveAddress{}, err
		}
		validatedRoot := strings.TrimSpace(root)
		if connector == "s3" {
			validatedRoot = root
		}
		if validatedRoot != "" {
			if err := validateRemoteNamespacePath(validatedRoot); err != nil {
				return EffectiveAddress{}, err
			}
		}
	}
	switch connector {
	case "s3":
		if _, configured := options["virtual_host"]; configured {
			return EffectiveAddress{}, fmt.Errorf("S3 virtual_host addressing is no longer supported")
		}
		if !strings.EqualFold(parsed.Scheme, "s3") || parsed.Hostname() == "" {
			return EffectiveAddress{}, fmt.Errorf("S3 vault location must use s3://endpoint/bucket/path")
		}
		result.UseTLS = boolDefault("use_tls", "true")
		rawEndpoint := strings.TrimSpace(options["endpoint"])
		result.Endpoint = normalizedEndpoint(rawEndpoint)
		scheme := "https"
		if result.UseTLS == "false" {
			scheme = "http"
		}
		if result.Endpoint != "" {
			endpoint, endpointErr := url.Parse(rawEndpoint)
			if endpointErr == nil && endpoint.Host == "" {
				endpoint, endpointErr = url.Parse(scheme + "://" + rawEndpoint)
			}
			if endpointErr != nil || endpoint.Hostname() == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") ||
				endpoint.User != nil || (endpoint.Path != "" && endpoint.Path != "/") || endpoint.RawQuery != "" || endpoint.Fragment != "" {
				return EffectiveAddress{}, fmt.Errorf("invalid S3 endpoint")
			}
			if (endpoint.Scheme == "https") != (result.UseTLS == "true") {
				return EffectiveAddress{}, fmt.Errorf("S3 endpoint scheme conflicts with the TLS setting")
			}
			endpoint.Path = ""
			result.Endpoint = normalizedEndpoint(endpoint.String())
			if optionPort := strings.TrimSpace(options["port"]); endpoint.Port() != "" && optionPort != "" && endpoint.Port() != optionPort {
				return EffectiveAddress{}, fmt.Errorf("S3 endpoint port conflicts with the port option")
			}
			result.Endpoint, endpointErr = endpointWithPort(result.Endpoint, options["port"])
			if endpointErr != nil {
				return EffectiveAddress{}, endpointErr
			}
		}
		// The URL has already been decoded exactly once. Only slash boundaries
		// are structural in this admitted S3 namespace; spaces, including Unicode
		// spaces at a prefix edge, and literal percent sequences name objects.
		// Keep the other providers' existing public path grammars independent.
		source := strings.Trim(parsed.Path, "/")
		if result.Endpoint != "" {
			endpoint, endpointErr := url.Parse(result.Endpoint)
			if endpointErr != nil || endpoint.Hostname() == "" {
				return EffectiveAddress{}, fmt.Errorf("invalid S3 endpoint")
			}
			locationPort := parsed.Port()
			if locationPort == "" && strings.EqualFold(parsed.Hostname(), endpoint.Hostname()) {
				locationPort = strings.TrimSpace(options["port"])
			}
			if normalizedHostPort(parsed.Hostname(), locationPort, result.UseTLS) !=
				normalizedHostPort(endpoint.Hostname(), endpoint.Port(), result.UseTLS) {
				source = strings.ToLower(parsed.Host) + "/" + source
			}
		} else if source == "" {
			// The host-only shorthand addresses an AWS bucket.
			result.Endpoint = scheme + "://s3.amazonaws.com"
			source = strings.ToLower(parsed.Hostname())
		} else {
			result.Endpoint = scheme + "://" + strings.ToLower(parsed.Host)
		}
		if root != "" {
			if !strings.HasPrefix(root, "/") {
				return EffectiveAddress{}, fmt.Errorf("S3 root must be an absolute bucket path")
			}
			source = strings.Trim(root, "/")
		}
		parts := strings.Split(source, "/")
		if len(parts) == 0 || parts[0] == "" {
			return EffectiveAddress{}, fmt.Errorf("S3 vault location must identify a bucket")
		}
		result.Bucket = parts[0]
		result.Prefix = strings.Join(parts[1:], "/")
		if result.Endpoint, err = endpointWithPort(result.Endpoint, options["port"]); err != nil {
			return EffectiveAddress{}, err
		}
	case "sftp":
		if !strings.EqualFold(parsed.Scheme, "sftp") || parsed.Hostname() == "" {
			return EffectiveAddress{}, fmt.Errorf("SFTP vault location must use sftp://host/path")
		}
		result.Host = strings.ToLower(parsed.Hostname())
		result.Port = parsed.Port()
		if result.Port == "" {
			result.Port = strings.TrimSpace(options["port"])
		}
		if result.Port == "" {
			result.Port = "22"
		}
		portNumber, portErr := strconv.Atoi(result.Port)
		if portErr != nil || portNumber < 1 || portNumber > 65535 {
			return EffectiveAddress{}, fmt.Errorf("SFTP port must be between 1 and 65535")
		}
		result.Username = strings.TrimSpace(options["username"])
		if result.Username != "" && parsed.User != nil && parsed.User.Username() != "" {
			return EffectiveAddress{}, fmt.Errorf("SFTP username must be specified in either the location or the username option, not both")
		}
		if result.Username == "" && parsed.User != nil {
			result.Username = parsed.User.Username()
		}
		if strings.TrimSpace(root) != "" {
			return EffectiveAddress{}, fmt.Errorf("SFTP root overrides are unsupported; use the vault path and path mode")
		}
		// RawPath is optional: Go omits it for canonical escapes such as %23.
		// Keep the existing unescaped-input grammar, including its rejection of
		// spaces/Unicode, instead of silently decoding a different native root.
		if parsed.RawPath != "" || strings.Contains(parsed.EscapedPath(), "%") {
			return EffectiveAddress{}, fmt.Errorf("SFTP vault paths cannot contain percent escapes")
		}
		remoteRoot := cleanRemotePath(parsed.Path)
		if remoteRoot == "" {
			return EffectiveAddress{}, fmt.Errorf("SFTP vault path relative to the selected base is required")
		}
		for _, segment := range strings.Split(remoteRoot, "/") {
			if strings.HasPrefix(segment, "~") {
				return EffectiveAddress{}, fmt.Errorf("SFTP vault paths cannot contain ~ segments")
			}
		}
		result.PathMode = strings.ToLower(strings.TrimSpace(options["path_mode"]))
		if result.PathMode == "" {
			result.PathMode = "home"
		}
		switch result.PathMode {
		case "home":
			result.Prefix = remoteRoot
		case "absolute":
			result.Prefix = "/" + remoteRoot
		default:
			return EffectiveAddress{}, fmt.Errorf("SFTP path mode must be home or absolute")
		}
	case "azblob":
		if !strings.EqualFold(parsed.Scheme, "azblob") || parsed.Host == "" {
			return EffectiveAddress{}, fmt.Errorf("Azure vault location must identify a container")
		}
		if parsed.RawPath != "" || strings.Contains(parsed.EscapedPath(), "%") {
			return EffectiveAddress{}, fmt.Errorf("Azure vault paths cannot contain percent escapes")
		}
		result.Bucket = strings.ToLower(parsed.Host)
		result.Prefix = cleanRemotePath(parsed.Path, root)
		result.AccountName = strings.TrimSpace(options["account_name"])
		if endpoint := strings.TrimSpace(options["endpoint"]); endpoint != "" {
			result.Endpoint, err = validateHTTPEndpoint(endpoint, true)
			if err != nil {
				return EffectiveAddress{}, fmt.Errorf("invalid Azure endpoint: %w", err)
			}
		}
		protocol, endpointSuffix := "https", ""
		for _, item := range strings.Split(options["connection_string"], ";") {
			key, value, ok := strings.Cut(item, "=")
			if !ok {
				continue
			}
			switch strings.ToLower(strings.TrimSpace(key)) {
			case "usedevelopmentstorage", "developmentstorageproxyuri":
				return EffectiveAddress{}, fmt.Errorf("Azure development storage connection strings are unsupported; use an explicit endpoint")
			case "accountname":
				result.AccountName = strings.TrimSpace(value)
			case "blobendpoint":
				result.Endpoint, err = validateHTTPEndpoint(value, true)
				if err != nil {
					return EffectiveAddress{}, fmt.Errorf("invalid Azure connection string endpoint: %w", err)
				}
			case "defaultendpointsprotocol":
				protocol = strings.ToLower(strings.TrimSpace(value))
			case "endpointsuffix":
				endpointSuffix = strings.TrimSpace(value)
			}
		}
		if result.Endpoint == "" && result.AccountName != "" && endpointSuffix != "" {
			result.Endpoint, err = validateHTTPEndpoint(protocol+"://"+result.AccountName+".blob."+endpointSuffix, false)
			if err != nil {
				return EffectiveAddress{}, fmt.Errorf("invalid Azure connection string endpoint suffix: %w", err)
			}
		}
		if result.Endpoint == "" && result.AccountName != "" {
			result.Endpoint = normalizedEndpoint("https://" + result.AccountName + ".blob.core.windows.net")
		}
	case "gcs":
		if parsed.Scheme != "gs" || parsed.Host == "" {
			return EffectiveAddress{}, fmt.Errorf("GCS vault location must use gs://bucket/path")
		}
		if parsed.RawPath != "" || strings.Contains(parsed.EscapedPath(), "%") {
			return EffectiveAddress{}, fmt.Errorf("GCS vault paths cannot contain percent escapes")
		}
		result.Bucket = strings.ToLower(parsed.Host)
		result.Prefix = cleanRemotePath(parsed.Path, root)
		if endpoint := strings.TrimSpace(options["endpoint"]); endpoint != "" {
			result.Endpoint, err = validateHTTPEndpoint(endpoint, false)
			if err != nil {
				return EffectiveAddress{}, fmt.Errorf("invalid GCS endpoint: %w", err)
			}
		}
	default:
		return EffectiveAddress{}, fmt.Errorf("unsupported connector %q", connector)
	}
	return result, nil
}

var rcloneProviderIdentityFields = map[string][]string{
	"google_drive": {"root_folder_id", "team_drive"},
	"dropbox":      {"root_namespace"},
	"onedrive":     {"region", "drive_id", "drive_type", "root_folder_id", "tenant"},
}

func resolveRcloneProviderAddress(
	connector, location string,
	options map[string]string,
	identityFields []string,
) (EffectiveAddress, error) {
	root := norm.NFC.String(strings.TrimSpace(location))
	const prefix = "Replicaro/"
	name := strings.TrimPrefix(root, prefix)
	normalizedName := norm.NFC.String(strings.TrimSpace(name))
	if !strings.HasPrefix(root, prefix) || name != normalizedName ||
		normalizedName == "" || normalizedName == "." || normalizedName == ".." ||
		strings.ContainsAny(name, `/\`) || utf8.RuneCountInString(name) > 50 ||
		len([]byte(root)) > 210 || strings.HasSuffix(name, " ") || strings.HasSuffix(name, ".") {
		return EffectiveAddress{}, fmt.Errorf("Restic rclone location must be one exact Replicaro/<name> vault folder")
	}
	for _, character := range root {
		if unicode.IsControl(character) {
			return EffectiveAddress{}, fmt.Errorf("Restic rclone location contains a control character")
		}
	}
	if len(identityFields) == 0 {
		return EffectiveAddress{}, fmt.Errorf("rclone provider %q has no credential-independent namespace identity", connector)
	}
	identity := make([]string, 0, len(identityFields)*2)
	hasIdentity := false
	for _, field := range identityFields {
		value := norm.NFC.String(strings.TrimSpace(options[field]))
		if strings.ContainsRune(value, '\x00') {
			return EffectiveAddress{}, fmt.Errorf("rclone provider identity field %q is invalid", field)
		}
		hasIdentity = hasIdentity || value != ""
		identity = append(identity, field, value)
	}
	if !hasIdentity {
		return EffectiveAddress{}, fmt.Errorf("rclone provider %q namespace identity is ambiguous", connector)
	}
	return EffectiveAddress{
		Connector: connector,
		Location:  root,
		Endpoint:  strings.Join(identity, "\x1f"),
	}, nil
}
