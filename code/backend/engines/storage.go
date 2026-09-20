package engines

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/integrations"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/vaultidentity"
)

var supportedStorageConnectors = map[string]bool{
	"fs": true, "sftp": true, "s3": true, "azblob": true, "gcs": true,
}

// userSafeConnectorError carries only a fixed connector option name/value and
// static recovery direction. It is safe to preserve through an engine's
// preparation boundary; all other private preparation failures use a generic
// output-free public error.
type userSafeConnectorError struct {
	message string
}

func (err *userSafeConnectorError) Error() string { return err.message }

func SupportsConnector(engine, connector string) bool {
	if !models.ValidEngine(engine) {
		return false
	}
	connector = strings.ToLower(strings.TrimSpace(connector))
	if IsResticRcloneConnector(connector) {
		// Runner coverage is diagnostic evidence. It cannot downgrade an
		// implemented integration; native engine capability checks still apply.
		return engine == ResticID
	}
	return supportedStorageConnectors[connector]
}

// NormalizeConnectorOptions applies only defaults supported by the selected
// native engine and rejects an explicitly supplied option that engine cannot
// consume. This keeps the shared UI catalog from leaking another engine's
// settings into native connector validation.
func NormalizeConnectorOptions(engine string, integration integrations.Integration, provided map[string]string) (map[string]string, error) {
	normalized, err := NormalizeStorageOptions(integration, provided)
	if err != nil {
		return nil, err
	}
	if IsResticRcloneConnector(integration.ID) {
		if engine != ResticID {
			return nil, fmt.Errorf("%s does not support %s vault storage", engine, integration.ID)
		}
		return NormalizeRcloneProviderOptions(integration.ID, normalized)
	}
	for key := range provided {
		if !storageOptionSupported(engine, integration.ID, key) {
			return nil, fmt.Errorf("%s does not support %s option %q", engine, integration.ID, key)
		}
	}
	for _, option := range integration.Options {
		if !storageOptionSupported(engine, integration.ID, option.Key) {
			delete(normalized, option.Key)
		}
	}
	return normalized, nil
}

// NormalizeStorageOptions applies engine-neutral storage defaults while
// preserving the distinction between an omitted host-key option and an
// explicitly unsafe choice. Password authentication defaults to verification;
// an explicit opt-out remains available to validation so it can fail closed.
func NormalizeStorageOptions(integration integrations.Integration, provided map[string]string) (map[string]string, error) {
	normalized, err := integrations.NormalizeOptions(integration, provided)
	if err != nil {
		return nil, err
	}
	_, hostKeyOptionProvided := provided["insecure_ignore_host_key"]
	if integration.ID == "sftp" &&
		strings.TrimSpace(normalized["password"]) != "" &&
		!hostKeyOptionProvided {
		normalized["insecure_ignore_host_key"] = "false"
	}
	return normalized, nil
}

// ValidateStorageOptions rejects ambiguous authentication combinations before
// any connector is used to inspect or mutate a remote vault.
func ValidateStorageOptions(connector string, options map[string]string) error {
	switch strings.ToLower(strings.TrimSpace(connector)) {
	case "s3":
		if err := validateStoragePort(options["port"]); err != nil {
			return fmt.Errorf("S3 port: %w", err)
		}
		if root := options["root"]; root != "" && !strings.HasPrefix(root, "/") {
			return fmt.Errorf("S3 root must be an absolute bucket path")
		}
	case "sftp":
		if err := validateStoragePort(options["port"]); err != nil {
			return fmt.Errorf("SFTP port: %w", err)
		}
		if err := validateSFTPPrivateKeyFormat(options); err != nil {
			return err
		}
		if strings.TrimSpace(options["password"]) != "" && boolOption(options, "insecure_ignore_host_key") {
			return fmt.Errorf("SFTP password authentication requires host-key verification")
		}
		if strings.TrimSpace(options["root"]) != "" {
			return fmt.Errorf("SFTP root overrides are unsupported; use the vault path and path mode")
		}
		mode := strings.ToLower(strings.TrimSpace(options["path_mode"]))
		if mode != "" && mode != "home" && mode != "absolute" {
			return fmt.Errorf("SFTP path mode must be home or absolute")
		}
	case "azblob":
		connection := strings.TrimSpace(options["connection_string"])
		connectionParts := splitConnectionString(connection)
		if connectionParts["usedevelopmentstorage"] != "" || connectionParts["developmentstorageproxyuri"] != "" {
			return fmt.Errorf("Azure development storage connection strings are unsupported; use an explicit endpoint")
		}
		account := strings.TrimSpace(options["account_name"])
		key := strings.TrimSpace(options["account_key"])
		if connection != "" && (account != "" || key != "" || strings.TrimSpace(options["endpoint"]) != "") {
			return fmt.Errorf("Azure connection strings cannot be combined with account credentials or a separate endpoint")
		}
		if connection == "" && (account == "" || key == "") {
			return fmt.Errorf("Azure requires a connection string or an account name and key")
		}
	case "gcs":
		file := strings.TrimSpace(options["credentials_file"])
		data := strings.TrimSpace(options["credentials_json"])
		if file != "" && data != "" {
			return fmt.Errorf("Google Cloud Storage accepts either a credentials file or credentials JSON, not both")
		}
	}
	return nil
}

func validateSFTPPrivateKeyFormat(options map[string]string) error {
	const guidance = "PuTTY .ppk keys are not supported by the selected native SFTP tools; export the key in unencrypted OpenSSH private-key format"
	// Test blankness separately: trimming an admitted filename can validate one
	// key and then deliver another to the native engine or sidecar reader.
	if identity := options["identity"]; strings.TrimSpace(identity) != "" {
		if strings.EqualFold(filepath.Ext(identity), ".ppk") {
			return fmt.Errorf("%s", guidance)
		}
		if isPuTTYPrivateKey(readPrivateKeyPrefix(identity)) {
			return fmt.Errorf("%s", guidance)
		}
	}
	if isPuTTYPrivateKey([]byte(strings.TrimSpace(options["ssh_private_key"]))) {
		return fmt.Errorf("%s", guidance)
	}
	return nil
}

func isPuTTYPrivateKey(data []byte) bool {
	if len(data) > 1024 {
		data = data[:1024]
	}
	firstLine := strings.TrimSpace(strings.SplitN(string(data), "\n", 2)[0])
	return strings.HasPrefix(firstLine, "PuTTY-User-Key-File-")
}

func readPrivateKeyPrefix(path string) []byte {
	file, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer file.Close()
	data := make([]byte, 1024)
	n, _ := file.Read(data)
	return data[:n]
}

// ValidateSFTPKnownHosts ensures password-bearing Kopia invocations cannot
// outlive an earlier preview's host-key trust prerequisite.
func ValidateSFTPKnownHosts(options map[string]string) (string, error) {
	if strings.TrimSpace(options["password"]) == "" {
		return "", nil
	}
	if boolOption(options, "insecure_ignore_host_key") {
		return "", fmt.Errorf("Kopia SFTP password authentication requires host-key verification")
	}
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		if err == nil {
			err = fmt.Errorf("user home is empty")
		}
		return "", fmt.Errorf("resolve SSH known_hosts: %w", err)
	}
	knownHosts := filepath.Join(home, ".ssh", "known_hosts")
	info, err := os.Stat(knownHosts)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("SSH known_hosts file %q is required for Kopia SFTP password authentication; add this server's host key before retrying", knownHosts)
		}
		return "", fmt.Errorf("inspect SSH known_hosts file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("SSH known_hosts path %q is not a regular file", knownHosts)
	}
	return knownHosts, nil
}

func ValidateConnectorOptions(engine, connector string, options map[string]string) error {
	connector = strings.ToLower(strings.TrimSpace(connector))
	if !SupportsConnector(engine, connector) {
		return fmt.Errorf("%s does not support %s vault storage", engine, connector)
	}
	if IsResticRcloneConnector(connector) {
		_, err := NormalizeRcloneProviderOptions(connector, options)
		return err
	}
	if err := ValidateStorageOptions(connector, options); err != nil {
		return err
	}
	switch connector {
	case "sftp":
		if identity := options["identity"]; strings.EqualFold(filepath.Ext(identity), ".ppk") {
			return fmt.Errorf("PuTTY .ppk keys are not supported by the selected native SFTP tools; export the key in unencrypted OpenSSH private-key format")
		}
		if engine == KopiaID && strings.TrimSpace(options["password"]) != "" && sftpKeyAuthenticationConfigured(options) {
			return fmt.Errorf("%s SFTP accepts either a password or an SSH key, not both", engine)
		}
		if engine == KopiaID && strings.TrimSpace(options["password"]) != "" && boolOption(options, "insecure_ignore_host_key") {
			return fmt.Errorf("Kopia SFTP password authentication requires host-key verification")
		}
		if engine == KopiaID && boolOption(options, "insecure_ignore_host_key") {
			if err := validateKopiaSFTPIdentityPath(options["identity"]); err != nil {
				return err
			}
		}
		if engine != KopiaID && strings.TrimSpace(options["password"]) != "" {
			return fmt.Errorf("%s SFTP does not support password authentication; use an SSH identity/key file or agent", engine)
		}
	case "azblob":
		account, key := azureCredentials(options)
		if account == "" {
			return fmt.Errorf("Azure account name is required for %s", engine)
		}
		if key == "" && azureSASToken(options) == "" {
			return fmt.Errorf("Azure account key or SAS token is required for %s", engine)
		}
		if endpoint := strings.TrimSpace(options["endpoint"]); endpoint != "" {
			if err := validateAzureEngineEndpoint(engine, endpoint, account); err != nil {
				return fmt.Errorf("%s Azure endpoint: %w", engine, err)
			}
		}
		connection := splitConnectionString(options["connection_string"])
		if protocol := strings.ToLower(strings.TrimSpace(connection["defaultendpointsprotocol"])); protocol != "" && protocol != "https" {
			return fmt.Errorf("%s Azure connection strings require HTTPS endpoints", engine)
		}
		if endpoint := strings.TrimSpace(connection["blobendpoint"]); endpoint != "" {
			if err := validateAzureEngineEndpoint(engine, endpoint, account); err != nil {
				return fmt.Errorf("%s Azure connection string endpoint: %w", engine, err)
			}
		}
		if suffix := strings.TrimSpace(connection["endpointsuffix"]); suffix != "" {
			if err := validateAzureEngineEndpoint(engine, "https://"+account+".blob."+suffix, account); err != nil {
				return fmt.Errorf("%s Azure connection string endpoint suffix: %w", engine, err)
			}
		}
	case "gcs":
		if engine == KopiaID && strings.TrimSpace(options["endpoint"]) != "" {
			return fmt.Errorf("Kopia GCS does not support custom endpoints")
		}
		if engine == ResticID && strings.TrimSpace(options["endpoint"]) != "" {
			return fmt.Errorf("Restic GCS does not support custom endpoints")
		}
	case "s3":
		storageClass := strings.ToUpper(strings.TrimSpace(options["storage_class"]))
		if engine == ResticID && (storageClass == models.ArchiveWriteClassGlacier || storageClass == models.ArchiveWriteClassDeepArchive) {
			return &userSafeConnectorError{message: fmt.Sprintf("Storage class %s requires the cold storage option; change your storage type to cold storage from the top of the window", storageClass)}
		}
		if strings.TrimSpace(options["sse_customer_key"]) != "" {
			return fmt.Errorf("%s S3 does not support customer-provided SSE keys", engine)
		}
	}
	return nil
}

func sftpKeyAuthenticationConfigured(options map[string]string) bool {
	for _, key := range []string{"identity", "ssh_private_key", "ssh_auth_sock"} {
		if strings.TrimSpace(options[key]) != "" {
			return true
		}
	}
	return false
}

func validateKopiaSFTPIdentityPath(identity string) error {
	// Keep the existing control-character and quote restrictions. Spaces pass
	// unchanged: external SSH argument parsing belongs to Kopia. Its pinned
	// parser splits ASCII spaces without shell quote handling, so adding quotes
	// or staging a differently named key would not preserve the supplied path.
	if !strings.ContainsAny(identity, "\x00\r\n\"") {
		return nil
	}
	return fmt.Errorf("Kopia engine cannot use an SSH identity path containing a quote, newline, or NUL when host-key verification is disabled. You can either enable host-key verification, use an SSH agent, choose another key path, or switch the engine to Restic.")
}

func validateStoragePort(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	port, err := strconv.Atoi(value)
	if err != nil || port < 1 || port > 65535 {
		return fmt.Errorf("must be a number from 1 to 65535")
	}
	return nil
}

func normalizedStorageRepository(repo models.Repository, engine string) models.Repository {
	repo = repo.RuntimeView()
	if strings.TrimSpace(repo.Engine) == "" {
		repo.Engine = engine
	}
	if strings.TrimSpace(repo.Connector) == "" {
		repo.Connector = "fs"
	}
	if repo.ConnectorOptions == nil {
		repo.ConnectorOptions = map[string]string{}
	}
	return repo
}

func storageProviderDescriptors(engine string) []ProviderDescriptor {
	catalog, err := integrations.Load()
	if err != nil {
		return nil
	}
	providers := make([]ProviderDescriptor, 0, len(catalog.Integrations))
	for _, provider := range catalog.Integrations {
		fields := make([]string, 0, len(provider.Options))
		for _, option := range provider.Options {
			if storageOptionSupported(engine, provider.ID, option.Key) {
				fields = append(fields, option.Key)
			}
		}
		providers = append(providers, ProviderDescriptor{
			ID: provider.ID, Label: provider.Label,
			Description: provider.Description,
			Supported:   SupportsConnector(engine, provider.ID),
			Fields:      fields,
		})
	}
	return providers
}

func storageOptionSupported(engine, connector, key string) bool {
	if IsResticRcloneConnector(connector) {
		if engine != ResticID {
			return false
		}
		provider, _ := RcloneProvider(connector)
		for _, field := range provider.Fields {
			if key == field {
				return true
			}
		}
		return false
	}
	unsupported := map[string]bool{}
	switch connector {
	case "s3":
		unsupported["sse_customer_key"] = true
		if engine == KopiaID {
			unsupported["storage_class"] = true
		}
	case "sftp":
		unsupported["ssh_private_key_ttl"] = true
		if engine != KopiaID {
			unsupported["password"] = true
		}
	case "gcs":
		if engine == KopiaID || engine == ResticID {
			unsupported["endpoint"] = true
		}
	}
	return !unsupported[key]
}

func effectiveAddress(repo models.Repository) (vaultidentity.EffectiveAddress, error) {
	return validatedConnectorAddress(repo.Engine, repo.Connector, repo.Location, repo.ConnectorOptions)
}

func ValidateConnectorAddress(engine, connector, location string, options map[string]string) error {
	_, err := validatedConnectorAddress(engine, connector, location, options)
	return err
}

func validatedConnectorAddress(engine, connector, location string, options map[string]string) (vaultidentity.EffectiveAddress, error) {
	if err := ValidateConnectorOptions(engine, connector, options); err != nil {
		return vaultidentity.EffectiveAddress{}, err
	}
	address, err := vaultidentity.ResolveEffectiveAddress(connector, location, options)
	if err != nil {
		return vaultidentity.EffectiveAddress{}, err
	}
	if engine == KopiaID && connector == "sftp" && strings.TrimSpace(address.Username) == "" {
		return vaultidentity.EffectiveAddress{}, fmt.Errorf("SFTP username is required for Kopia")
	}
	return address, nil
}

func splitConnectionString(value string) map[string]string {
	result := map[string]string{}
	for _, item := range strings.Split(value, ";") {
		key, part, ok := strings.Cut(item, "=")
		if ok {
			result[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(part)
		}
	}
	return result
}

func azureCredentials(options map[string]string) (account, key string) {
	account, key = strings.TrimSpace(options["account_name"]), strings.TrimSpace(options["account_key"])
	connection := splitConnectionString(options["connection_string"])
	if connection["accountname"] != "" {
		account = connection["accountname"]
	}
	if connection["accountkey"] != "" {
		key = connection["accountkey"]
	}
	return account, key
}

func azureSASToken(options map[string]string) string {
	connection := splitConnectionString(options["connection_string"])
	return strings.TrimPrefix(strings.TrimSpace(connection["sharedaccesssignature"]), "?")
}

func endpointHost(value string) string {
	value = strings.TrimSpace(value)
	if parsed, err := url.Parse(value); err == nil && parsed.Host != "" {
		return parsed.Host
	}
	return strings.TrimSuffix(strings.TrimPrefix(strings.TrimPrefix(value, "https://"), "http://"), "/")
}

func azureStorageDomain(endpoint, account string) string {
	host := endpointHost(endpoint)
	if host == "" {
		return ""
	}
	host = trimPrefixFold(host, account+".")
	return strings.TrimPrefix(host, ".")
}

func azureResticEndpointSuffix(endpoint, account string) string {
	host := endpointHost(endpoint)
	if host == "" {
		return ""
	}
	host = trimPrefixFold(host, account+".blob.")
	return strings.TrimPrefix(host, ".")
}

func trimPrefixFold(value, prefix string) string {
	if len(value) >= len(prefix) && strings.EqualFold(value[:len(prefix)], prefix) {
		return value[len(prefix):]
	}
	return value
}

func validateAzureEngineEndpoint(engine, endpoint, account string) error {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("must be an HTTPS account endpoint without credentials, a path, query, or fragment")
	}
	prefix := strings.ToLower(account) + "."
	if engine == ResticID {
		prefix += "blob."
	}
	if !strings.HasPrefix(strings.ToLower(parsed.Hostname()), prefix) || len(parsed.Hostname()) <= len(prefix) {
		if engine == ResticID {
			return fmt.Errorf("must begin with the configured account name and .blob")
		}
		return fmt.Errorf("must begin with the configured account name")
	}
	return nil
}

func remotePath(prefix string) string {
	if prefix = strings.Trim(strings.ReplaceAll(prefix, "\\", "/"), "/"); prefix == "" {
		return ""
	}
	return prefix
}

func sftpHostPort(address vaultidentity.EffectiveAddress) string {
	host := address.Host
	if strings.Contains(host, ":") {
		host = "[" + strings.Trim(host, "[]") + "]"
	}
	return net.JoinHostPort(strings.Trim(host, "[]"), address.Port)
}

func materializeGCSCredentials(repo models.Repository) (string, error) {
	// This is the native reader's filename, not JSON loaded by Replicaro.
	// Preserve its exact spelling; blankness must not become path normalization.
	if path := repo.ConnectorOptions["credentials_file"]; strings.TrimSpace(path) != "" {
		return path, nil
	}
	data := strings.TrimSpace(repo.ConnectorOptions["credentials_json"])
	if data == "" {
		return "", nil
	}
	return materializeCredentialFile(repo, "gcs-credentials.json", data)
}

func materializeSSHPrivateKey(repo models.Repository) (string, error) {
	if path := repo.ConnectorOptions["identity"]; strings.TrimSpace(path) != "" {
		return path, nil
	}
	data := strings.TrimSpace(repo.ConnectorOptions["ssh_private_key"])
	if data == "" {
		return "", nil
	}
	return materializeCredentialFile(repo, "sftp-identity", data+"\n")
}

func materializeCredentialFile(repo models.Repository, name, data string) (string, error) {
	directory, err := appdata.Directory(filepath.Join("engines", repo.Engine, repo.ID, "credentials"))
	if err != nil {
		return "", err
	}
	temporary, err := os.CreateTemp(directory, name+"-*")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if _, err := temporary.WriteString(data); err != nil {
		_ = temporary.Close()
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	destination := filepath.Join(directory, name)
	if err := appdata.AtomicReplace(temporaryPath, destination); err != nil {
		return "", err
	}
	return destination, nil
}

func boolOption(options map[string]string, key string) bool {
	value, _ := strconv.ParseBool(strings.TrimSpace(options[key]))
	return value
}
