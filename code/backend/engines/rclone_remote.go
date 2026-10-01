package engines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/models"
	"github.com/local/replicaro/vaultidentity"
)

// RcloneRemoteConnector is the "Any Rclone Remote" storage type: a Restic
// vault on a remote that the user set up in their own rclone.conf. Replicaro
// reads that file in place for every rclone run and never writes, copies, or
// parses it. There is no private per-vault config and no sign-in flow.
const RcloneRemoteConnector = "rclone_remote"

// Option keys of the Any Rclone Remote connector.
const (
	RcloneRemoteConfigFileOption      = "config_file"
	RcloneRemoteEnvironmentOption     = "environment"
	RcloneRemoteConfigEncryptedOption = "config_encrypted"
	RcloneRemoteConfigPasswordOption  = "config_password"
	RcloneRemoteNameOption            = "remote"
	RcloneRemotePathOption            = "path"
)

// RcloneSidecarRemoteName is the rclone crypt remote that carries the vault
// sidecar. It is defined only by environment variables for each command and
// never saved. rclone still ties it to the config file in two ways: any option
// the environment leaves unset is filled in from a file section with exactly
// this name, and the RCLONE_CONFIG_REPLICARO_SIDECAR_* variables apply to a
// file remote with this name in any letter case. The distinct name keeps the
// sidecar apart from a user's own "crypt" remote; it makes a clash unlikely,
// not impossible. An Any Rclone Remote vault refuses a user file that defines
// a remote rclone would match with this name (see
// vaultidentity.IsRcloneSidecarRemoteName).
const RcloneSidecarRemoteName = vaultidentity.RcloneSidecarRemoteName

// Error codes of the Any Rclone Remote checks. Each code has exactly one
// English message, so the UI can show a translated one from the code and the
// fields of RcloneRemoteError alone.
const (
	RcloneRemoteConfigFileRequiredCode      = "rclone_remote_config_file_required"
	RcloneRemoteConfigFileNotAbsoluteCode   = "rclone_remote_config_file_not_absolute"
	RcloneRemoteConfigEncryptedInvalidCode  = "rclone_remote_config_encrypted_invalid"
	RcloneRemoteConfigPasswordRequiredCode  = "rclone_remote_config_password_required"
	RcloneRemoteNameRequiredCode            = "rclone_remote_name_required"
	RcloneRemoteNameInvalidCode             = "rclone_remote_name_invalid"
	RcloneRemoteNameSeparatorCode           = "rclone_remote_name_separator"
	RcloneRemoteNameDriveLetterCode         = "rclone_remote_name_drive_letter"
	RcloneRemoteNameReservedCode            = "rclone_remote_name_reserved"
	RcloneRemotePathRequiredCode            = "rclone_remote_path_required"
	RcloneRemotePathInvalidCode             = "rclone_remote_path_invalid"
	RcloneRemotePathIncludesRemoteCode      = "rclone_remote_path_includes_remote"
	RcloneRemoteEnvironmentUnreadableCode   = "rclone_remote_environment_unreadable"
	RcloneRemoteEnvironmentNameRequiredCode = "rclone_remote_environment_name_required"
	RcloneRemoteEnvironmentNameEqualsCode   = "rclone_remote_environment_name_equals"
	RcloneRemoteEnvironmentNULCode          = "rclone_remote_environment_nul"
	RcloneRemoteEnvironmentDuplicateCode    = "rclone_remote_environment_duplicate"
	RcloneRemoteEnvironmentRefusedCode      = "rclone_remote_environment_refused"
	RcloneRemoteConfigNotFoundCode          = "rclone_remote_config_not_found"
	RcloneRemoteConfigAccessCode            = "rclone_remote_config_access"
	RcloneRemoteConfigFolderCode            = "rclone_remote_config_folder"
	RcloneRemoteConfigManagedFolderCode     = "rclone_remote_config_managed_folder"
	RcloneRemoteConfigNotRespondingCode     = "rclone_remote_config_not_responding"
	RcloneRemoteListFailedCode              = "rclone_remote_list_failed"
	RcloneRemoteListUnreadableCode          = "rclone_remote_list_unreadable"
	RcloneRemoteConfigReservedNameCode      = "rclone_remote_config_reserved_name"
	RcloneRemoteNotFoundCode                = "rclone_remote_not_found"
	RcloneRemoteMemoryCode                  = "rclone_remote_memory"
	RcloneRemoteCompressCode                = "rclone_remote_compress"
	RcloneRemoteCryptCode                   = "rclone_remote_crypt"
)

// rcloneRemoteErrorCodes lists every code above; the tests check that each
// one has its own message.
var rcloneRemoteErrorCodes = []string{
	RcloneRemoteConfigFileRequiredCode, RcloneRemoteConfigFileNotAbsoluteCode,
	RcloneRemoteConfigEncryptedInvalidCode, RcloneRemoteConfigPasswordRequiredCode,
	RcloneRemoteNameRequiredCode, RcloneRemoteNameInvalidCode, RcloneRemoteNameSeparatorCode,
	RcloneRemoteNameDriveLetterCode, RcloneRemoteNameReservedCode,
	RcloneRemotePathRequiredCode, RcloneRemotePathInvalidCode, RcloneRemotePathIncludesRemoteCode,
	RcloneRemoteEnvironmentUnreadableCode, RcloneRemoteEnvironmentNameRequiredCode,
	RcloneRemoteEnvironmentNameEqualsCode, RcloneRemoteEnvironmentNULCode,
	RcloneRemoteEnvironmentDuplicateCode, RcloneRemoteEnvironmentRefusedCode,
	RcloneRemoteConfigNotFoundCode, RcloneRemoteConfigAccessCode, RcloneRemoteConfigFolderCode,
	RcloneRemoteConfigManagedFolderCode,
	RcloneRemoteConfigNotRespondingCode,
	RcloneRemoteListFailedCode, RcloneRemoteListUnreadableCode, RcloneRemoteConfigReservedNameCode,
	RcloneRemoteNotFoundCode, RcloneRemoteMemoryCode, RcloneRemoteCompressCode, RcloneRemoteCryptCode,
}

// The remote-name and path rules live in vaultidentity, which can't import
// this package, so their errors are matched to codes here.
var rcloneRemoteIdentityErrorCodes = []struct {
	err  error
	code string
}{
	{vaultidentity.ErrRcloneRemoteNameRequired, RcloneRemoteNameRequiredCode},
	{vaultidentity.ErrRcloneRemoteNameInvalid, RcloneRemoteNameInvalidCode},
	{vaultidentity.ErrRcloneRemoteNameSeparator, RcloneRemoteNameSeparatorCode},
	{vaultidentity.ErrRcloneRemoteNameDriveLetter, RcloneRemoteNameDriveLetterCode},
	{vaultidentity.ErrRcloneRemoteNameReserved, RcloneRemoteNameReservedCode},
	{vaultidentity.ErrRcloneRemotePathRequired, RcloneRemotePathRequiredCode},
	{vaultidentity.ErrRcloneRemotePathInvalid, RcloneRemotePathInvalidCode},
	{vaultidentity.ErrRcloneRemotePathIncludesRemote, RcloneRemotePathIncludesRemoteCode},
}

func rcloneRemoteIdentityError(err error, remote string) error {
	for _, known := range rcloneRemoteIdentityErrorCodes {
		if errors.Is(err, known.err) {
			return &RcloneRemoteError{Code: known.code, Message: err.Error(), Remote: remote, cause: err}
		}
	}
	return err
}

// These two messages are intentionally simplified. Restic, not Replicaro,
// does the compression and encryption, and "may cause data loss" is more
// cautious than the known effect of layering rclone compress or crypt under
// Restic. They are phrased this way so users don't have to understand the
// difference between Replicaro and its backup engine. Keep the wording as it
// is.
const (
	rcloneRemoteCompressMessage = "Replicaro handles compression and does not support Rclone compress. Enabling Rclone compress with Replicaro's compression may cause data loss. Please disable Rclone compress."
	rcloneRemoteCryptMessage    = "Replicaro handles encryption and does not support Rclone crypt. Enabling Rclone crypt with Replicaro's encryption may cause data loss. Please disable Rclone crypt."
)

const rcloneRemoteListFailedHint = "Check the rclone config file and the rclone config password."

// RcloneRemoteError is a refused Any Rclone Remote setting or a failed
// preflight step. Message is the English text for the API. Every value a
// message shows is also in a field, so a translated message can be built from
// Code and the fields. It never holds an environment variable value or the
// rclone config password.
type RcloneRemoteError struct {
	Code    string
	Message string
	// ConfigFile is the rclone config file path the message is about, if any.
	ConfigFile string
	// Remote is the remote name the message is about, if any.
	Remote string
	// Names lists the refused or repeated environment variable names.
	Names []string
	// Detail is rclone's own output when listing the remotes failed.
	Detail string
	cause  error
}

func (e *RcloneRemoteError) Error() string { return e.Message }
func (e *RcloneRemoteError) Unwrap() error { return e.cause }

// RcloneRemoteUseFailureHint is the line added to a failed vault operation of
// this connector. rclone has many backends that report errors in their own
// words, so Replicaro doesn't try to tell an expired sign-in from an outage;
// it points the user at rclone instead.
func RcloneRemoteUseFailureHint(remote string) string {
	return fmt.Sprintf("Check this remote with rclone, for example `rclone lsd %[1]s`, or `rclone config reconnect %[1]s` if its sign-in expired.",
		rcloneRemoteShellArgument(remote+":"))
}

// rcloneRemoteShellArgument quotes a "<remote>:" argument so the hint can be
// pasted into a terminal. Names made only of ASCII letters, digits, "_", "-",
// and "." are left bare; anything else, such as a space, gets double quotes,
// which cmd, PowerShell, and POSIX shells all read the same way for the
// characters rclone allows in a remote name.
func rcloneRemoteShellArgument(argument string) string {
	for _, character := range strings.TrimSuffix(argument, ":") {
		if !('a' <= character && character <= 'z' || 'A' <= character && character <= 'Z' ||
			'0' <= character && character <= '9' || strings.ContainsRune("_-.", character)) {
			return `"` + argument + `"`
		}
	}
	return argument
}

// RcloneRemoteVariable is one row of the environment variables field. The
// whole list is saved as one secret option, as JSON, in the order entered.
// Names and values are passed to rclone exactly as entered.
type RcloneRemoteVariable struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// ParseRcloneRemoteEnvironment decodes the saved environment variables option.
// An empty option means no variables.
func ParseRcloneRemoteEnvironment(raw string) ([]RcloneRemoteVariable, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var variables []RcloneRemoteVariable
	if err := decoder.Decode(&variables); err != nil {
		return nil, rcloneRemoteEnvironmentFormatError()
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, rcloneRemoteEnvironmentFormatError()
	}
	return variables, nil
}

func rcloneRemoteEnvironmentFormatError() error {
	return &RcloneRemoteError{
		Code:    RcloneRemoteEnvironmentUnreadableCode,
		Message: "The environment variables could not be read. Enter them again.",
	}
}

// EncodeRcloneRemoteEnvironment is the saved form of the variables; no
// variables is an empty option.
func EncodeRcloneRemoteEnvironment(variables []RcloneRemoteVariable) (string, error) {
	if len(variables) == 0 {
		return "", nil
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(variables); err != nil {
		return "", err
	}
	return strings.TrimSuffix(encoded.String(), "\n"), nil
}

// Environment variables the user may not pass to rclone. A variable is
// refused when it can make rclone silently skip, redirect, truncate, or
// unencrypt a write; change what a read or listing returns; change the output
// Replicaro reads; leak secrets into logs; open a listener; write files
// elsewhere; load code; or change how the config file is found or decrypted.
// Names Replicaro sets itself are refused too. This is a list of refusals, not
// of allowed names, so any backend option (RCLONE_<backend>_<option>) other
// than crypt's, and any remote option (RCLONE_CONFIG_<remote>_<option>) other
// than the sidecar's, stays available to set up the user's remote.
//
// A family refuses its own name and every name that starts with it plus "_".
// None of them may cover a backend prefix; the tests check that against the
// pinned rclone.
var refusedRcloneRemoteVariableFamilies = []string{
	"RESTIC",
	"RCLONE_CONFIG_" + strings.ToUpper(RcloneSidecarRemoteName),
	"RCLONE_CRYPT",
	"_RCLONE",
	"RCLONE_RC",
	"RCLONE_METRICS",
	"RCLONE_DUMP",
	"RCLONE_LOG",
	"RCLONE_SYSLOG",
	"RCLONE_PROGRESS",
	"RCLONE_METADATA",
	"RCLONE_INCLUDE",
	"RCLONE_EXCLUDE",
	"RCLONE_FILTER",
	"RCLONE_FILES_FROM",
	"RCLONE_SUFFIX",
	"RCLONE_MAX_DELETE",
	// macOS loads extra libraries into rclone and Restic through these.
	"DYLD",
}

// refusedRcloneFlagVariables are refused as RCLONE_<name>, exactly.
var refusedRcloneFlagVariables = []string{
	// Replicaro sets these itself.
	"CONFIG", "CONFIG_PASS", "PASSWORD_COMMAND", "ASK_PASSWORD", "CACHE_DIR", "TEMP_DIR",
	// Writes that can be skipped, redirected, or cut short without an error.
	"DRY_RUN", "INTERACTIVE", "IGNORE_EXISTING", "UPDATE", "NO_CHECK_DEST", "COMPARE_DEST",
	"COPY_DEST", "BACKUP_DIR", "NAME_TRANSFORM", "INPLACE", "IGNORE_CHECKSUM", "IGNORE_SIZE",
	"MAX_TRANSFER", "MAX_DURATION", "ERROR_ON_NO_TRANSFER",
	// copyto's report files, which rclone creates or overwrites at any path.
	"COMBINED", "DEST_AFTER", "DIFFER", "ERROR", "MATCH", "MISSING_ON_DST", "MISSING_ON_SRC",
	// Filters that shrink listings and reads.
	"FILES_FROM0", "MIN_AGE", "MAX_AGE", "MIN_SIZE", "MAX_SIZE", "MAX_DEPTH", "HASH_FILTER",
	// Flags of cat, lsjson, and listremotes that change their output.
	"DISCARD", "HEAD", "TAIL", "OFFSET", "COUNT", "SEPARATOR", "DIRS_ONLY", "FILES_ONLY",
	"STAT", "RECURSIVE", "NAME", "TYPE", "SOURCE", "DESCRIPTION",
	// Flags of "serve restic", which carries Restic's traffic.
	"APPEND_ONLY", "PRIVATE_REPOS", "BASEURL", "USER", "PASS", "HTPASSWD", "USER_FROM_HEADER",
	// Logging, profiling, plugins, and rclone's own config folder.
	"VERBOSE", "QUIET", "USE_JSON_LOG", "CPUPROFILE", "MEMPROFILE", "PLUGIN_PATH", "CONFIG_DIR",
	// A hidden flag that sends rclone's log to the Windows event log.
	"WINDOWS_EVENT_LOG_LEVEL",
}

// refusedOtherVariables are refused exactly, like the RCLONE_ names above.
var refusedOtherVariables = []string{
	// GODEBUG=http2debug makes Go print HTTP/2 request headers, including
	// Authorization, into the output of Restic's rclone child.
	"GODEBUG",
	// The dynamic loader loads extra libraries through these.
	"LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT",
}

var refusedRcloneRemoteVariableNames = func() map[string]bool {
	names := map[string]bool{}
	for _, name := range refusedRcloneFlagVariables {
		names["RCLONE_"+name] = true
	}
	for _, name := range refusedOtherVariables {
		names[name] = true
	}
	for _, name := range command.MinimalEnvironmentNames() {
		names[strings.ToUpper(name)] = true
	}
	return names
}()

// rcloneRemoteVariableKey is how names are compared: trimmed and upper-cased,
// because Windows ignores the case of environment names.
func rcloneRemoteVariableKey(name string) string {
	return strings.ToUpper(strings.TrimSpace(name))
}

// RcloneRemoteVariableRefused reports whether a name may not be passed to
// rclone, whatever its value.
func RcloneRemoteVariableRefused(name string) bool {
	key := rcloneRemoteVariableKey(name)
	if refusedRcloneRemoteVariableNames[key] {
		return true
	}
	for _, family := range refusedRcloneRemoteVariableFamilies {
		if key == family || strings.HasPrefix(key, family+"_") {
			return true
		}
	}
	return false
}

// ValidateRcloneRemoteEnvironment is the one check of the user's environment
// variables. It runs when they are saved and again before every run, so a
// saved variable refused by a newer rule fails visibly instead of being passed
// on. Nothing is trimmed or changed; trimming is only for comparing names.
func ValidateRcloneRemoteEnvironment(variables []RcloneRemoteVariable) error {
	for _, variable := range variables {
		switch {
		case strings.TrimSpace(variable.Name) == "":
			return &RcloneRemoteError{
				Code:    RcloneRemoteEnvironmentNameRequiredCode,
				Message: "Every environment variable needs a name.",
			}
		case strings.Contains(variable.Name, "="):
			return &RcloneRemoteError{
				Code:    RcloneRemoteEnvironmentNameEqualsCode,
				Message: "An environment variable name can't contain '='.",
			}
		case strings.ContainsRune(variable.Name, 0) || strings.ContainsRune(variable.Value, 0):
			return &RcloneRemoteError{
				Code:    RcloneRemoteEnvironmentNULCode,
				Message: "An environment variable name or value can't contain a NUL character.",
			}
		}
	}
	seen := make(map[string]bool, len(variables))
	var repeated, refused []string
	for _, variable := range variables {
		key := rcloneRemoteVariableKey(variable.Name)
		if seen[key] {
			repeated = append(repeated, strings.TrimSpace(variable.Name))
			continue
		}
		seen[key] = true
		if RcloneRemoteVariableRefused(variable.Name) {
			refused = append(refused, strings.TrimSpace(variable.Name))
		}
	}
	if len(repeated) > 0 {
		return &RcloneRemoteError{
			Code:    RcloneRemoteEnvironmentDuplicateCode,
			Message: "Each environment variable can be entered only once. Entered more than once: " + strings.Join(repeated, ", ") + ".",
			Names:   repeated,
		}
	}
	if len(refused) > 0 {
		return &RcloneRemoteError{
			Code: RcloneRemoteEnvironmentRefusedCode,
			Message: "Replicaro can't pass these environment variables to rclone: " + strings.Join(refused, ", ") +
				". Replicaro sets them itself, or they change how rclone saves, reads, lists, or logs files, or make it start a server, write extra files, or run a program. Remove them and try again. Variables that set up your remote, such as AWS_ACCESS_KEY_ID or RCLONE_S3_ACCESS_KEY_ID, are fine.",
			Names: refused,
		}
	}
	return nil
}

// normalizeRcloneRemoteOptions checks and normalizes the connector's options.
// The remote name and the path in remote are checked only when requireRemote
// is set, so the remote list can be fetched before a remote is chosen.
func normalizeRcloneRemoteOptions(values map[string]string, requireRemote bool) (map[string]string, error) {
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	configFile := strings.TrimSpace(result[RcloneRemoteConfigFileOption])
	if configFile == "" {
		return nil, &RcloneRemoteError{
			Code:    RcloneRemoteConfigFileRequiredCode,
			Message: "Enter the path of the rclone config file.",
		}
	}
	if !filepath.IsAbs(configFile) {
		return nil, &RcloneRemoteError{
			Code:    RcloneRemoteConfigFileNotAbsoluteCode,
			Message: "Enter the full path of the rclone config file.", ConfigFile: configFile,
		}
	}
	result[RcloneRemoteConfigFileOption] = configFile
	switch strings.TrimSpace(result[RcloneRemoteConfigEncryptedOption]) {
	case "true":
		result[RcloneRemoteConfigEncryptedOption] = "true"
		if result[RcloneRemoteConfigPasswordOption] == "" {
			return nil, &RcloneRemoteError{
				Code:    RcloneRemoteConfigPasswordRequiredCode,
				Message: "Enter the rclone config password.",
			}
		}
	case "", "false":
		// Without an encrypted file there is no rclone config password to keep.
		result[RcloneRemoteConfigEncryptedOption] = "false"
		delete(result, RcloneRemoteConfigPasswordOption)
	default:
		return nil, &RcloneRemoteError{
			Code:    RcloneRemoteConfigEncryptedInvalidCode,
			Message: "Choose whether the rclone config file is encrypted.",
		}
	}
	variables, err := ParseRcloneRemoteEnvironment(result[RcloneRemoteEnvironmentOption])
	if err != nil {
		return nil, err
	}
	if err := ValidateRcloneRemoteEnvironment(variables); err != nil {
		return nil, err
	}
	encoded, err := EncodeRcloneRemoteEnvironment(variables)
	if err != nil {
		return nil, err
	}
	if encoded == "" {
		delete(result, RcloneRemoteEnvironmentOption)
	} else {
		result[RcloneRemoteEnvironmentOption] = encoded
	}
	remote := strings.TrimSpace(result[RcloneRemoteNameOption])
	if remote != "" || requireRemote {
		if err := vaultidentity.ValidateRcloneRemoteName(remote); err != nil {
			return nil, rcloneRemoteIdentityError(err, remote)
		}
		result[RcloneRemoteNameOption] = remote
	}
	if result[RcloneRemotePathOption] != "" || requireRemote {
		path, err := vaultidentity.NormalizeRcloneRemotePath(remote, result[RcloneRemotePathOption])
		if err != nil {
			return nil, rcloneRemoteIdentityError(err, remote)
		}
		result[RcloneRemotePathOption] = path
	}
	return result, nil
}

// rcloneRemoteEnvironment is the environment every rclone run of this
// connector gets on top of the minimal one: no password prompts, the rclone
// config password only when the user said the file is encrypted, and the
// user's variables as entered. Restic's rclone child inherits it from Restic.
// None of these values ever goes on a command line.
func rcloneRemoteEnvironment(options map[string]string) ([]string, error) {
	variables, err := ParseRcloneRemoteEnvironment(options[RcloneRemoteEnvironmentOption])
	if err != nil {
		return nil, err
	}
	env := []string{"RCLONE_ASK_PASSWORD=false"}
	if options[RcloneRemoteConfigEncryptedOption] == "true" {
		env = append(env, "RCLONE_CONFIG_PASS="+options[RcloneRemoteConfigPasswordOption])
	}
	for _, variable := range variables {
		env = append(env, variable.Name+"="+variable.Value)
	}
	return env, nil
}

// RcloneRemoteRun is what every rclone run of an Any Rclone Remote vault
// uses: the user's config file (passed with --config, or as RCLONE_CONFIG to
// Restic), the vault's full rclone root, and the environment from
// rcloneRemoteEnvironment. It holds secrets in Env and must not be logged.
type RcloneRemoteRun struct {
	ConfigFile string
	Root       string
	Env        []string
}

// RcloneRemoteRunForRepository checks the saved options again, including the
// environment variables, and returns the settings for one run.
func RcloneRemoteRunForRepository(repo models.Repository) (RcloneRemoteRun, error) {
	if repo.Engine != ResticID || repo.Connector != RcloneRemoteConnector {
		return RcloneRemoteRun{}, fmt.Errorf("%w: repository is not an Any Rclone Remote vault", ErrUnsupported)
	}
	options, err := normalizeRcloneRemoteOptions(repo.ConnectorOptions, true)
	if err != nil {
		return RcloneRemoteRun{}, err
	}
	address, err := vaultidentity.ResolveEffectiveAddress(repo.Connector, repo.Location, options)
	if err != nil {
		return RcloneRemoteRun{}, err
	}
	root, err := vaultidentity.RcloneRemoteRoot(address)
	if err != nil {
		return RcloneRemoteRun{}, err
	}
	env, err := rcloneRemoteEnvironment(options)
	if err != nil {
		return RcloneRemoteRun{}, err
	}
	return RcloneRemoteRun{
		ConfigFile: options[RcloneRemoteConfigFileOption], Root: root, Env: env,
	}, nil
}

// RcloneRemoteEntry is one remote from "rclone listremotes --json".
type RcloneRemoteEntry struct {
	Name   string `json:"name"`
	Type   string `json:"type"`
	Source string `json:"source"`
}

var runRcloneRemoteList = command.Run

// ListRcloneRemotes runs the file checks and lists the remotes of the user's
// config file, for the remote picker. It refuses a file that defines the
// sidecar's reserved remote name. The options need the config file, the
// encryption answer and rclone config password, and the environment
// variables; the remote name and path in remote are not used.
func ListRcloneRemotes(ctx context.Context, options map[string]string) ([]RcloneRemoteEntry, error) {
	normalized, err := normalizeRcloneRemoteOptions(options, false)
	if err != nil {
		return nil, err
	}
	return listRcloneRemotes(ctx, normalized)
}

// PreflightRcloneRemote checks an Any Rclone Remote vault's settings before
// they are used: the config file can be opened for reading and writing, its
// folder can take a new file (rclone saves the file by writing a temporary one
// next to it, for example after refreshing a sign-in), the chosen remote is in
// the file or the environment variables, and it is not a memory, compress, or
// crypt remote. It returns an *RcloneRemoteError for a refused setting or
// failed check, or the context's error. It never changes the user's file.
func PreflightRcloneRemote(ctx context.Context, options map[string]string) error {
	normalized, err := normalizeRcloneRemoteOptions(options, true)
	if err != nil {
		return err
	}
	entries, err := listRcloneRemotes(ctx, normalized)
	if err != nil {
		return err
	}
	remote := normalized[RcloneRemoteNameOption]
	// rclone applies RCLONE_CONFIG_<NAME>_* variables to a file remote whose
	// upper-cased name matches, but lists the variables' remote separately
	// under its own lower-case name. File sections, on the other hand, only
	// match their exact name. So the type check covers the entry listed under
	// exactly the chosen name plus any environment entry whose name matches it
	// in another letter case, and the chosen name itself must be listed as
	// entered.
	found := false
	for _, entry := range entries {
		if entry.Name != remote &&
			(entry.Source != "environment" || strings.ToUpper(entry.Name) != strings.ToUpper(remote)) {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(entry.Type)) {
		case "memory":
			return &RcloneRemoteError{
				Code: RcloneRemoteMemoryCode, Remote: remote,
				Message: fmt.Sprintf("The remote '%s' is an rclone memory remote, which keeps nothing after rclone exits. Choose another remote.", remote),
			}
		case "compress":
			return &RcloneRemoteError{Code: RcloneRemoteCompressCode, Remote: remote, Message: rcloneRemoteCompressMessage}
		case "crypt":
			return &RcloneRemoteError{Code: RcloneRemoteCryptCode, Remote: remote, Message: rcloneRemoteCryptMessage}
		}
		if entry.Name == remote {
			found = true
		}
	}
	if found {
		return nil
	}
	return &RcloneRemoteError{
		Code: RcloneRemoteNotFoundCode, Remote: remote,
		Message: fmt.Sprintf("The remote '%s' is not in the rclone config file.", remote),
	}
}

// rcloneRemoteCheckTimeout bounds the file checks and the remote listing of
// one preflight or picker request. The file checks call the operating system
// directly, and on a hung network share (a mapped drive, or a home or AppData
// folder redirected to a server) they can block for minutes and can't be
// cancelled; the call is then left to finish in its goroutine, as the folder
// picker does. The bound is longer than the minute rclone listremotes gets on
// its own, so a slow rclone still reports its own failure first. A variable
// only so tests can shorten it.
var rcloneRemoteCheckTimeout = 90 * time.Second

// rcloneRemoteConfigNotResponding is the error for a config file that didn't
// answer in time, whether the file checks hit rcloneRemoteCheckTimeout or
// rclone's own read of the file hit its one-minute limit. The message names no
// time because it covers both.
func rcloneRemoteConfigNotResponding(configFile string) *RcloneRemoteError {
	return &RcloneRemoteError{
		Code:       RcloneRemoteConfigNotRespondingCode,
		Message:    "The rclone config file did not respond in time. Check that the drive or network share that holds it is available.",
		ConfigFile: configFile,
	}
}

func listRcloneRemotes(ctx context.Context, options map[string]string) ([]RcloneRemoteEntry, error) {
	type result struct {
		entries []RcloneRemoteEntry
		err     error
	}
	// Cancelling this context stops the rclone child when the bound expires.
	checkCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	// Buffered so an abandoned check can still deliver and exit.
	done := make(chan result, 1)
	go func() {
		entries, err := listRcloneRemotesUnbounded(checkCtx, options)
		done <- result{entries, err}
	}()
	timer := time.NewTimer(rcloneRemoteCheckTimeout)
	defer timer.Stop()
	select {
	case finished := <-done:
		return finished.entries, finished.err
	case <-timer.C:
		return nil, rcloneRemoteConfigNotResponding(options[RcloneRemoteConfigFileOption])
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func listRcloneRemotesUnbounded(ctx context.Context, options map[string]string) ([]RcloneRemoteEntry, error) {
	configFile := options[RcloneRemoteConfigFileOption]
	if err := checkRcloneRemoteConfigFile(configFile); err != nil {
		return nil, err
	}
	env, err := rcloneRemoteEnvironment(options)
	if err != nil {
		return nil, err
	}
	binary, err := rcloneBinary(pathFor(RcloneComponentID))
	if err != nil {
		return nil, err
	}
	// Only the user's variables: with the sidecar's variables set, a file
	// remote named like the sidecar would be listed once, as the sidecar.
	output, err := runRcloneRemoteList(ctx, binary,
		[]string{"listremotes", "--json", "--config", configFile, "--log-level", "ERROR"},
		env, time.Minute, RcloneComponentID)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		detail := ""
		var commandErr *command.Error
		if errors.As(err, &commandErr) {
			detail = strings.TrimSpace(commandErr.Output)
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// rclone's own limit ran out while the caller still waits. Listing
			// remotes is local work, so the config file didn't answer, as when
			// the file checks hit the bound in listRcloneRemotes. Whatever
			// rclone printed before it was stopped stays in Detail. The
			// deadline is not kept as the cause: callers read a deadline in the
			// chain as their own request or operation running out.
			notResponding := rcloneRemoteConfigNotResponding(configFile)
			notResponding.Detail = detail
			return nil, notResponding
		}
		if detail == "" {
			detail = err.Error()
		}
		return nil, &RcloneRemoteError{
			Code: RcloneRemoteListFailedCode, Detail: detail,
			Message: detail + "\n" + rcloneRemoteListFailedHint, cause: err,
		}
	}
	var entries []RcloneRemoteEntry
	if err := json.Unmarshal([]byte(output), &entries); err != nil {
		return nil, &RcloneRemoteError{
			Code:    RcloneRemoteListUnreadableCode,
			Message: "rclone returned an unexpected list of remotes.\n" + rcloneRemoteListFailedHint,
			cause:   err,
		}
	}
	for _, entry := range entries {
		if vaultidentity.IsRcloneSidecarRemoteName(entry.Name) {
			return nil, &RcloneRemoteError{
				Code: RcloneRemoteConfigReservedNameCode, Remote: entry.Name,
				Message: "The rclone config file defines a remote named '" + RcloneSidecarRemoteName +
					"', which Replicaro reserves. Rename that remote.",
			}
		}
	}
	return entries, nil
}

// rcloneRemoteProbeFolder is the folder checkRcloneRemoteConfigFile writes its
// probe to; tests replace it to make that write fail on any platform.
var rcloneRemoteProbeFolder = filepath.Dir

// checkRcloneRemoteConfigFile opens the user's config file for reading and
// writing without creating or changing it, and checks that its folder takes a
// new file. rclone saves the config by writing a temporary file next to the
// real file (after following a link) and exits 0 even when that fails, so an
// unwritable folder would silently lose a refreshed sign-in. No rclone command
// can check this without rewriting the file.
func checkRcloneRemoteConfigFile(configFile string) error {
	notFound := &RcloneRemoteError{
		Code:    RcloneRemoteConfigNotFoundCode,
		Message: "The rclone config file was not found at " + configFile + ".", ConfigFile: configFile,
	}
	access := func(cause error) error {
		return &RcloneRemoteError{
			Code:    RcloneRemoteConfigAccessCode,
			Message: "Replicaro cannot read or write the rclone config file.",
			cause:   cause,
		}
	}
	realFile, err := resolveRcloneRemoteConfigOutsideManagedFolders(configFile)
	var refused *RcloneRemoteError
	if errors.As(err, &refused) {
		return refused
	}
	if errors.Is(err, fs.ErrNotExist) {
		notFound.cause = err
		return notFound
	}
	if err != nil {
		return access(err)
	}
	file, err := os.OpenFile(realFile, os.O_RDWR, 0)
	if errors.Is(err, fs.ErrNotExist) {
		notFound.cause = err
		return notFound
	}
	if err != nil {
		return access(err)
	}
	if err := file.Close(); err != nil {
		return access(err)
	}
	folderErr := func(cause error) error {
		return &RcloneRemoteError{
			Code:    RcloneRemoteConfigFolderCode,
			Message: "Replicaro cannot write to the folder that holds the rclone config file.",
			cause:   cause,
		}
	}
	probe, err := os.CreateTemp(rcloneRemoteProbeFolder(realFile), ".replicaro-write-check-*")
	if err != nil {
		return folderErr(err)
	}
	// Removing this probe is part of the check: it is the empty file this
	// function just created under a unique name.
	closeErr := probe.Close()
	removeErr := os.Remove(probe.Name())
	if err := errors.Join(closeErr, removeErr); err != nil {
		return folderErr(err)
	}
	return nil
}
