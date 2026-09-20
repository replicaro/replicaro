package engines

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/local/replicaro/appdata"
	"github.com/local/replicaro/command"
	"github.com/local/replicaro/vaultidentity"
)

const (
	rcloneAuthorizationTimeout          = 15 * time.Minute
	oneDriveLocationSurveyTimeout       = 5 * time.Minute
	rcloneDropboxIdentitySourceFile     = "dropbox/dropbox.go"
	rcloneGoogleDriveIdentitySourceFile = "drive/drive.go"
)

var runRcloneAuthorizationCommand = func(
	ctx context.Context,
	path string,
	args, env []string,
	timeout time.Duration,
) (string, error) {
	return command.RunWithInputSecretsPrivateOutput(
		ctx, path, args, env, "", timeout, RcloneComponentID,
	)
}

var runObservedRcloneAuthorizationCommand = func(
	ctx context.Context,
	path string,
	args, env []string,
	timeout time.Duration,
	observe func([]byte) error,
) (string, error) {
	return command.RunWithInputSecretsPrivateOutputObservedStderr(
		ctx, path, args, env, "", timeout, RcloneComponentID, observe,
	)
}

func SetRcloneAuthorizationRunnerForTests(next func(
	context.Context,
	string,
	[]string,
	[]string,
	time.Duration,
) (string, error)) func() {
	previous := runRcloneAuthorizationCommand
	runRcloneAuthorizationCommand = func(
		ctx context.Context, path string, args, env []string, timeout time.Duration,
	) (string, error) {
		if len(args) >= 3 && args[0] == "config" && args[1] == "show" && args[2] == "provider" {
			config := ""
			for index := range args {
				if args[index] == "--config" && index+1 < len(args) {
					config = args[index+1]
					break
				}
			}
			if config == "" {
				return "", fmt.Errorf("test rclone config observation omitted --config")
			}
			data, err := os.ReadFile(config)
			return string(data), err
		}
		return next(ctx, path, args, env, timeout)
	}
	return func() { runRcloneAuthorizationCommand = previous }
}

func SetObservedRcloneAuthorizationRunnerForTests(next func(
	context.Context,
	string,
	[]string,
	[]string,
	time.Duration,
	func([]byte) error,
) (string, error)) func() {
	previous := runObservedRcloneAuthorizationCommand
	runObservedRcloneAuthorizationCommand = next
	return func() { runObservedRcloneAuthorizationCommand = previous }
}

type RcloneAuthChoice struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

type RcloneAuthQuestion struct {
	ID           string             `json:"id"`
	Prompt       string             `json:"prompt"`
	DefaultValue string             `json:"defaultValue"`
	Choices      []RcloneAuthChoice `json:"choices"`
	Notice       string             `json:"notice,omitempty"`
}

type rcloneConfigResponse struct {
	State  string                `json:"State"`
	Option *rcloneConfigQuestion `json:"Option"`
	Error  string                `json:"Error"`
	Result string                `json:"Result"`
}

type rcloneConfigQuestion struct {
	Name       string `json:"Name"`
	Help       string `json:"Help"`
	DefaultStr string `json:"DefaultStr"`
	ValueStr   string `json:"ValueStr"`
	Type       string `json:"Type"`
	IsPassword bool   `json:"IsPassword"`
	Sensitive  bool   `json:"Sensitive"`
	Exclusive  bool   `json:"Exclusive"`
	Required   bool   `json:"Required"`
	Examples   []struct {
		Value string `json:"Value"`
		Help  string `json:"Help"`
	} `json:"Examples"`
}

// RcloneAuthorizationFlow owns one short-lived, private native rclone
// authorization session. Provider credentials never leave this object through
// its public status methods.
type RcloneAuthorizationFlow struct {
	mu                  sync.Mutex
	binary              string
	provider            RcloneProviderDefinition
	root, operationRoot string
	config, cache, tmp  string
	browserEnvironment  func() ([]string, error)
	state               string
	question            *rcloneConfigQuestion
	oneDriveSurveyed    bool
	expectedOneDriveID  string
	options             map[string]string
	ready, closed       bool
	cleanupComplete     bool
	manualBrowser       bool
}

func rcloneNativeLoginProvider(storageType string) (RcloneProviderDefinition, error) {
	switch strings.ToLower(strings.TrimSpace(storageType)) {
	case "dropbox", "google_drive", "onedrive":
		provider, _ := RcloneProvider(storageType)
		return provider, nil
	default:
		return RcloneProviderDefinition{}, fmt.Errorf("%w: native rclone login is unavailable for %q", ErrUnsupported, storageType)
	}
}

func StartRcloneAuthorization(
	ctx context.Context,
	customBinary, storageType string,
) (*RcloneAuthorizationFlow, error) {
	return startRcloneAuthorization(
		ctx, customBinary, storageType, rcloneAuthorizationBrowserEnvironment,
	)
}

func StartRcloneAuthorizationManual(
	ctx context.Context,
	customBinary, storageType string,
	onAuthorizationURL func(string),
) (*RcloneAuthorizationFlow, error) {
	return startRcloneAuthorizationMode(
		ctx, customBinary, storageType, nil, true, onAuthorizationURL,
	)
}

func startRcloneAuthorization(
	ctx context.Context,
	customBinary, storageType string,
	browserEnvironment func() ([]string, error),
) (*RcloneAuthorizationFlow, error) {
	return startRcloneAuthorizationMode(
		ctx, customBinary, storageType, browserEnvironment, false, nil,
	)
}

func startRcloneAuthorizationMode(
	ctx context.Context,
	customBinary, storageType string,
	browserEnvironment func() ([]string, error),
	manualBrowser bool,
	onAuthorizationURL func(string),
) (*RcloneAuthorizationFlow, error) {
	if err := ensureRcloneSessionsCleaned(); err != nil {
		return nil, fmt.Errorf("clean stale rclone sessions: %w", err)
	}
	provider, err := rcloneNativeLoginProvider(storageType)
	if err != nil {
		return nil, err
	}
	binary, err := rcloneBinary(customBinary)
	if err != nil {
		return nil, err
	}
	parent, err := appdata.RcloneConfigSessionRoot()
	if err != nil {
		return nil, err
	}
	if err := prepareRcloneSessionRoot(parent); err != nil {
		return nil, fmt.Errorf("validate private rclone authorization root: %w", err)
	}
	root, err := os.MkdirTemp(parent, "config-")
	if err != nil {
		return nil, err
	}
	cleanup := func(primary error) (*RcloneAuthorizationFlow, error) {
		cleanupErr := removeRcloneSession(root)
		return nil, errors.Join(primary, cleanupErr)
	}
	if err := os.Chmod(root, 0o700); err != nil {
		return cleanup(err)
	}
	if err := appdata.SecurePath(root, true); err != nil {
		return cleanup(err)
	}
	operationRoot, cache, temporary, _, err := privateRcloneOperationDirectories()
	if err != nil {
		return cleanup(err)
	}
	flow := &RcloneAuthorizationFlow{
		binary: binary, provider: provider, root: root, operationRoot: operationRoot,
		config:             filepath.Join(root, "rclone.conf"),
		cache:              cache,
		tmp:                temporary,
		options:            map[string]string{},
		browserEnvironment: browserEnvironment,
		manualBrowser:      manualBrowser,
	}
	if err := flow.advanceLocked(ctx, true, "", "", onAuthorizationURL); err != nil {
		return failRcloneAuthorization(flow, err)
	}
	return flow, nil
}

func failRcloneAuthorization(
	flow *RcloneAuthorizationFlow,
	primary error,
) (*RcloneAuthorizationFlow, error) {
	if flow == nil {
		return nil, primary
	}
	cleanupErr := flow.Close()
	if cleanupErr != nil {
		return flow, errors.Join(primary, cleanupErr)
	}
	return nil, primary
}

func (flow *RcloneAuthorizationFlow) args(values ...string) []string {
	return flow.argsWithLogLevel("ERROR", values...)
}

func (flow *RcloneAuthorizationFlow) argsWithLogLevel(
	level string,
	values ...string,
) []string {
	result := append([]string(nil), values...)
	return append(result,
		"--config", flow.config,
		"--cache-dir", flow.cache,
		"--temp-dir", flow.tmp,
		"--log-level", level,
	)
}

func (flow *RcloneAuthorizationFlow) advanceLocked(
	ctx context.Context,
	initial bool,
	state, result string,
	onAuthorizationURL func(string),
) error {
	browserAuthorization := false
	for {
		var args []string
		if initial {
			args = []string{
				"config", "create", "provider", flow.provider.NativeType,
				"--non-interactive",
			}
		} else {
			if state == "" {
				return fmt.Errorf("native rclone account configuration returned unsafe continuation data")
			}
			args = []string{
				"config", "update", "provider", "--continue",
				"--state", state, "--result", result, "--non-interactive",
			}
		}
		timeout := 2 * time.Minute
		var environment []string
		if browserAuthorization {
			timeout = rcloneAuthorizationTimeout
			if !flow.manualBrowser {
				if flow.browserEnvironment == nil {
					return fmt.Errorf("native rclone browser authorization environment is unavailable")
				}
				browserEnvironment, environmentErr := flow.browserEnvironment()
				if environmentErr != nil {
					return environmentErr
				}
				environment = browserEnvironment
			}
		}
		var output string
		var err error
		if browserAuthorization && flow.manualBrowser {
			if onAuthorizationURL == nil {
				return fmt.Errorf("native rclone manual authorization presentation is unavailable")
			}
			manualArgs := append([]string(nil), args[:3]...)
			manualArgs = append(manualArgs, "config_auth_no_browser", "true")
			manualArgs = append(manualArgs, args[3:]...)
			manualArgs = append(manualArgs, "--use-json-log")
			observer := newRcloneAuthorizationURLObserver(onAuthorizationURL)
			output, err = runObservedRcloneAuthorizationCommand(
				ctx, flow.binary, flow.argsWithLogLevel("NOTICE", manualArgs...), nil,
				timeout, observer.observe,
			)
			if finishErr := observer.finish(); finishErr != nil {
				err = errors.Join(err, finishErr)
			}
		} else {
			output, err = runRcloneAuthorizationCommand(
				ctx, flow.binary, flow.args(args...), environment,
				timeout,
			)
		}
		if err != nil {
			return fmt.Errorf("native rclone account configuration failed")
		}
		_, configPresent, err := flow.readProviderConfigIfPresent(ctx)
		if err != nil {
			return err
		}
		var response rcloneConfigResponse
		if err := decodeRcloneConfigResponse(output, &response); err != nil {
			return fmt.Errorf("native rclone account configuration returned invalid JSON")
		}
		if err := validateRcloneAuthState(response.State); err != nil {
			return err
		}
		if err := validateRcloneAuthResult(response.Result); err != nil {
			return err
		}
		if response.Error != "" {
			return fmt.Errorf("native rclone account configuration failed")
		}
		if response.Option == nil {
			if response.State != "" || response.Result != "" {
				return fmt.Errorf("native rclone account configuration returned incomplete state")
			}
			if !configPresent {
				return fmt.Errorf("native rclone provider configuration is unavailable")
			}
			return flow.finalizeLocked(ctx)
		}
		if response.State == "" {
			return fmt.Errorf("native rclone account configuration returned incomplete state")
		}
		if err := validateRcloneAuthQuestion(flow.provider, response.Option); err != nil {
			return err
		}
		switch response.Option.Name {
		case "config_is_local":
			state, result = response.State, "true"
			initial, browserAuthorization = false, true
			continue
		case "config_refresh_token":
			state, result = response.State, "false"
			initial, browserAuthorization = false, false
			continue
		case "config_shared_client_id":
			// Rclone's shared Google client retirement notice was previously
			// surfaced as a UI question even though Replicaro has no custom
			// client-ID input. Keep the notice internal in every login flow and
			// use the only supported native choice; rclone reports any failure.
			state, result = response.State, "true"
			initial, browserAuthorization = false, false
			continue
		case "config_change_team_drive":
			// Rclone used to ask users whether to use a Google Shared Drive
			// after login. This login path defaults to My Drive, so
			// answer No in every create, connect, and reauthorization flow.
			state, result = response.State, "false"
			initial, browserAuthorization = false, false
			continue
		case "config_type":
			// Rclone offers SharePoint and advanced connection types as well.
			// Replicaro supports only OneDrive Personal or Business, so the
			// connection-type question must stay internal in every flow.
			state, result = response.State, "onedrive"
			initial, browserAuthorization = false, false
			continue
		case "config_drive_ok":
			// Rclone already validated the selected location's root. Its
			// redundant "Use this drive?" confirmation used to reach the UI;
			// answer Yes consistently after the location was selected.
			state, result = response.State, "true"
			initial, browserAuthorization = false, false
			continue
		case "config_driveid":
			if flow.oneDriveSurveyed {
				return fmt.Errorf("native OneDrive location changed during selection")
			}
			return flow.surveyOneDriveLocationsLocked(
				ctx, response.State, response.Option, onAuthorizationURL,
			)
		}
		flow.state, flow.question = response.State, response.Option
		return nil
	}
}

// surveyOneDriveLocationsLocked fixes the old numbered chooser, which asked
// users to guess among untested OneDrive IDs. It runs the same native root and
// identity checks that previously ran only after a user picked one location.
// Probe every discovered ID before deciding whether a choice is necessary.
// The checks are read-only remotely; rclone updates the private local config.
func (flow *RcloneAuthorizationFlow) surveyOneDriveLocationsLocked(
	ctx context.Context,
	state string,
	question *rcloneConfigQuestion,
	onAuthorizationURL func(string),
) error {
	surveyCtx, cancel := context.WithTimeout(ctx, oneDriveLocationSurveyTimeout)
	defer cancel()
	filtered := *question
	filtered.Examples = nil
	filtered.DefaultStr = ""
	seen := make(map[string]bool, len(question.Examples))
	for _, example := range question.Examples {
		if err := surveyCtx.Err(); err != nil {
			return fmt.Errorf("native OneDrive location survey did not complete: %w", err)
		}
		if !rcloneAuthAnswerAllowed(flow.provider, question.Name, example.Value) ||
			seen[example.Value] {
			continue
		}
		seen[example.Value] = true
		works, err := flow.probeOneDriveLocationLocked(surveyCtx, state, example.Value)
		if err != nil {
			return err
		}
		if works {
			filtered.Examples = append(filtered.Examples, example)
		}
	}
	if err := surveyCtx.Err(); err != nil {
		return fmt.Errorf("native OneDrive location survey did not complete: %w", err)
	}
	if len(filtered.Examples) == 0 {
		return fmt.Errorf("native rclone found no usable OneDrive location")
	}
	flow.oneDriveSurveyed = true
	if len(filtered.Examples) == 1 {
		// Reapply the sole successful ID because a later probe may have
		// changed rclone's private config. The normal path revalidates it.
		flow.expectedOneDriveID = filtered.Examples[0].Value
		return flow.advanceLocked(ctx, false, state, flow.expectedOneDriveID, onAuthorizationURL)
	}
	flow.state, flow.question = state, &filtered
	return nil
}

func (flow *RcloneAuthorizationFlow) probeOneDriveLocationLocked(
	ctx context.Context,
	state, driveID string,
) (bool, error) {
	output, err := runRcloneAuthorizationCommand(
		ctx, flow.binary,
		flow.args("config", "update", "provider", "--continue", "--state", state,
			"--result", driveID, "--non-interactive"),
		nil, 2*time.Minute,
	)
	if err != nil {
		return false, fmt.Errorf("native rclone OneDrive location check failed")
	}
	_, present, err := flow.readProviderConfigIfPresent(ctx)
	if err != nil {
		return false, err
	}
	if !present {
		return false, fmt.Errorf("native rclone provider configuration is unavailable")
	}
	var response rcloneConfigResponse
	if err := decodeRcloneConfigResponse(output, &response); err != nil {
		return false, fmt.Errorf("native rclone OneDrive location check returned invalid JSON")
	}
	if err := validateRcloneAuthState(response.State); err != nil {
		return false, err
	}
	if err := validateRcloneAuthResult(response.Result); err != nil {
		return false, err
	}
	if response.Error != "" {
		if response.State == "choose_type" && response.Option == nil {
			// Pinned rclone reports an inaccessible root this way. Its raw
			// Graph error may contain private account details; never show it.
			return false, nil
		}
		return false, fmt.Errorf("native rclone OneDrive location check failed")
	}
	if response.State != "driveid_final_end" || response.Option == nil ||
		response.Option.Name != "config_drive_ok" {
		return false, fmt.Errorf("native rclone OneDrive location check returned an unexpected state")
	}
	if err := validateRcloneAuthQuestion(flow.provider, response.Option); err != nil {
		return false, err
	}
	options, err := flow.discoverIdentityLocked(ctx)
	if err != nil {
		return false, err
	}
	if options["drive_id"] != driveID {
		return false, fmt.Errorf("native OneDrive location identity changed during check")
	}
	return true, nil
}

func decodeRcloneConfigResponse(
	output string,
	response *rcloneConfigResponse,
) error {
	decoder := json.NewDecoder(strings.NewReader(output))
	if err := decoder.Decode(response); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("native rclone account configuration returned trailing JSON")
	}
	return nil
}

const rcloneAuthorizationLinkPrefix = "Please go to the following link: "
const rcloneAuthorizationReadyMessage = "Log in and authorize rclone for access\n"

type rcloneAuthorizationURLObserver struct {
	onURL     func(string)
	url       string
	delivered bool
	linkCount int
}

func newRcloneAuthorizationURLObserver(onURL func(string)) *rcloneAuthorizationURLObserver {
	return &rcloneAuthorizationURLObserver{onURL: onURL}
}

func (observer *rcloneAuthorizationURLObserver) observe(line []byte) error {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	var entry struct {
		Level   string `json:"level"`
		Message string `json:"msg"`
	}
	decoder := json.NewDecoder(bytes.NewReader(line))
	if err := decoder.Decode(&entry); err != nil {
		return fmt.Errorf("native rclone authorization returned malformed structured stderr")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("native rclone authorization returned trailing structured stderr")
	}
	if entry.Level == "notice" && strings.HasPrefix(entry.Message, rcloneAuthorizationLinkPrefix) {
		observer.linkCount++
		if observer.linkCount != 1 || observer.url != "" || observer.delivered {
			return fmt.Errorf("native rclone authorization returned duplicate links")
		}
		candidateWithNewline := strings.TrimPrefix(entry.Message, rcloneAuthorizationLinkPrefix)
		if !strings.HasSuffix(candidateWithNewline, "\n") || strings.Count(candidateWithNewline, "\n") != 1 {
			return fmt.Errorf("native rclone authorization returned an invalid link")
		}
		candidate := strings.TrimSuffix(candidateWithNewline, "\n")
		if !validRcloneAuthorizationURL(candidate) {
			return fmt.Errorf("native rclone authorization returned an invalid link")
		}
		observer.url = candidate
		return nil
	}
	if strings.Contains(entry.Message, "/auth?state=") ||
		strings.Contains(entry.Message, rcloneAuthorizationLinkPrefix) {
		return fmt.Errorf("native rclone authorization returned an unexpected link")
	}
	if entry.Level == "notice" && entry.Message == rcloneAuthorizationReadyMessage {
		if observer.linkCount != 1 || observer.url == "" || observer.delivered || observer.onURL == nil {
			return fmt.Errorf("native rclone authorization link was unavailable")
		}
		authorizationURL := observer.url
		observer.url = ""
		observer.delivered = true
		observer.onURL(authorizationURL)
	}
	return nil
}

func (observer *rcloneAuthorizationURLObserver) finish() error {
	if observer.linkCount != 1 || !observer.delivered {
		return fmt.Errorf("native rclone authorization did not provide one validated link")
	}
	return nil
}

func validRcloneAuthorizationURL(value string) bool {
	if len(value) == 0 || len(value) > 1024 || strings.ContainsAny(value, "\x00\r\n\t ") {
		return false
	}
	parsed, err := url.ParseRequestURI(value)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil ||
		parsed.Hostname() != "127.0.0.1" || parsed.Port() != "53682" ||
		parsed.Path != "/auth" || parsed.RawPath != "" || parsed.Fragment != "" {
		return false
	}
	values, err := url.ParseQuery(parsed.RawQuery)
	if err != nil || len(values) != 1 || len(values["state"]) != 1 {
		return false
	}
	state := values.Get("state")
	if len(state) == 0 || len(state) > 512 || parsed.RawQuery != "state="+state {
		return false
	}
	for _, character := range state {
		if !(character >= 'a' && character <= 'z') &&
			!(character >= 'A' && character <= 'Z') &&
			!(character >= '0' && character <= '9') &&
			character != '-' && character != '_' && character != '.' && character != '~' {
			return false
		}
	}
	return true
}

func (flow *RcloneAuthorizationFlow) readProviderConfigIfPresent(ctx context.Context) (
	map[string]string,
	bool,
	error,
) {
	root := filepath.Clean(flow.root)
	config := filepath.Clean(flow.config)
	relative, err := filepath.Rel(root, config)
	if err != nil || relative != "rclone.conf" {
		return nil, false, fmt.Errorf("native rclone provider configuration path is unsafe")
	}
	if err := prepareRcloneSessionRoot(root); err != nil {
		return nil, false, fmt.Errorf("native rclone authorization session is unsafe")
	}
	info, err := os.Lstat(config)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		removeErr := os.Remove(config)
		return nil, false, errors.Join(
			fmt.Errorf("native rclone provider configuration is linked or special"),
			removeErr,
		)
	}
	if err := os.Chmod(config, 0o600); err != nil {
		return nil, false, fmt.Errorf("protect native rclone provider configuration")
	}
	if err := appdata.SecurePath(config, false); err != nil {
		return nil, false, fmt.Errorf("protect native rclone provider configuration")
	}
	if info.Size() < 0 || info.Size() > maximumRcloneVaultConfigBytes {
		return nil, false, fmt.Errorf("native rclone provider configuration is oversized")
	}
	shown, err := runRcloneAuthorizationCommand(
		ctx, flow.binary, flow.args("config", "show", "provider"), nil,
		time.Minute,
	)
	if err != nil {
		return nil, false, fmt.Errorf("native rclone provider observation failed")
	}
	identityProvider := flow.provider
	identityProvider.Fields = append([]string(nil), flow.provider.IdentityFields...)
	values, _, err := readRcloneProviderConfigReader(
		strings.NewReader(shown), "provider", identityProvider,
	)
	if err != nil {
		return nil, false, fmt.Errorf("native rclone provider observation was invalid")
	}
	info, err = os.Lstat(config)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		removeErr := os.Remove(config)
		return nil, false, errors.Join(
			fmt.Errorf("native rclone provider configuration became unsafe"),
			removeErr,
		)
	}
	return values, true, nil
}

func validateRcloneAuthQuestion(
	provider RcloneProviderDefinition,
	question *rcloneConfigQuestion,
) error {
	if question == nil || question.Name == "" || question.IsPassword || question.Sensitive {
		return fmt.Errorf("native rclone requested an unsupported sensitive account answer")
	}
	if !validRcloneAuthControlValue(question.Name, 128, false) ||
		!validRcloneAuthDisplayText(question.Help, 4096) ||
		!validRcloneAuthControlValue(question.DefaultStr, 512, true) ||
		!validRcloneAuthControlValue(question.ValueStr, 512, true) {
		return fmt.Errorf("native rclone requested an invalid account option")
	}
	switch question.Name {
	case "config_is_local", "config_refresh_token",
		"config_change_team_drive",
		"config_type", "config_driveid", "config_drive_ok",
		"config_shared_client_id":
	default:
		return fmt.Errorf("native rclone requested unsupported account option %q", question.Name)
	}
	boundedChoice := question.Exclusive
	if provider.StorageType == "onedrive" && question.Name == "config_driveid" {
		// Pinned rclone presents discovered drives as required suggestions
		// because its CLI also permits a manually entered advanced drive ID.
		// Replicaro exposes only the discovered native values.
		boundedChoice = question.Required
	}
	if !boundedChoice || len(question.Examples) == 0 || len(question.Examples) > 100 {
		return fmt.Errorf("native rclone account option %q is not a bounded choice", question.Name)
	}
	for _, example := range question.Examples {
		if !validRcloneAuthControlValue(example.Value, 512, false) ||
			!validRcloneAuthDisplayText(example.Help, 4096) {
			return fmt.Errorf("native rclone account option %q has an invalid choice", question.Name)
		}
	}
	allowed := false
	for _, example := range question.Examples {
		allowed = allowed || rcloneAuthAnswerAllowed(provider, question.Name, example.Value)
	}
	if !allowed {
		return fmt.Errorf("native rclone account option %q has no supported choice", question.Name)
	}
	return nil
}

func validRcloneAuthControlValue(value string, maximum int, allowEmpty bool) bool {
	if (!allowEmpty && value == "") || len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) {
			return false
		}
	}
	return true
}

func validRcloneAuthDisplayText(value string, maximum int) bool {
	if len(value) > maximum || !utf8.ValidString(value) {
		return false
	}
	for _, character := range value {
		if unicode.IsControl(character) &&
			character != '\r' && character != '\n' && character != '\t' {
			return false
		}
	}
	return true
}

func validateRcloneAuthState(state string) error {
	if len(state) > 8192 || strings.ContainsAny(state, "\x00\r\n") {
		return fmt.Errorf("native rclone account configuration returned invalid state")
	}
	return nil
}

func validateRcloneAuthResult(result string) error {
	if !validRcloneAuthControlValue(result, 512, true) {
		return fmt.Errorf("native rclone account configuration returned invalid result")
	}
	return nil
}

func rcloneAuthAnswerAllowed(
	provider RcloneProviderDefinition,
	question, value string,
) bool {
	return validRcloneNativeChoice(provider, question, value)
}

func validRcloneNativeChoice(
	provider RcloneProviderDefinition,
	question, value string,
) bool {
	switch question {
	case "config_is_local":
		return value == "true" || value == "false"
	case "config_refresh_token":
		return value == "true" || value == "false"
	case "config_change_team_drive":
		return provider.StorageType == "google_drive" && value == "false"
	case "config_drive_ok":
		return provider.StorageType == "onedrive" && value == "true"
	case "config_shared_client_id":
		// Replicaro does not collect a custom Google OAuth client ID.
		return provider.StorageType == "google_drive" && value == "true"
	case "config_type":
		return provider.StorageType == "onedrive" && value == "onedrive"
	case "config_driveid":
		if provider.StorageType != "onedrive" || value == "" || len(value) > 512 {
			return false
		}
		for _, character := range value {
			if !unicode.IsLetter(character) && !unicode.IsDigit(character) &&
				character != '-' && character != '_' && character != '!' &&
				character != '.' && character != ':' {
				return false
			}
		}
		return true
	default:
		return false
	}
}

func rcloneAuthPublicChoiceID(index int) string {
	return fmt.Sprintf("choice-%d", index+1)
}

func rcloneAuthPublicPrompt(question string) string {
	switch question {
	case "config_driveid":
		return "Your OneDrive account has multiple locations where vaults can be stored. Pick one location."
	default:
		return "Choose the native rclone account option"
	}
}

func rcloneAuthOneDriveLocationLabel(nativeHelp string, index int) string {
	// Pinned rclone builds this help from the account's drive name and type.
	// Display only that bounded description; native IDs, raw Graph errors,
	// and private OAuth config values stay out of status. React escapes it.
	description := strings.Join(strings.Fields(nativeHelp), " ")
	if description == "" {
		return rcloneAuthPublicChoiceLabel("config_driveid", index)
	}
	return fmt.Sprintf("%s — location %d", description, index+1)
}

func rcloneAuthPublicChoiceLabel(question string, index int) string {
	switch question {
	case "config_driveid":
		return fmt.Sprintf("OneDrive location option %d", index+1)
	default:
		return fmt.Sprintf("Option %d", index+1)
	}
}

func (flow *RcloneAuthorizationFlow) Continue(
	ctx context.Context,
	answer string,
) error {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	return flow.continueLocked(ctx, answer, nil)
}

func (flow *RcloneAuthorizationFlow) ContinueManual(
	ctx context.Context,
	answer string,
	onAuthorizationURL func(string),
) error {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if !flow.manualBrowser {
		return fmt.Errorf("rclone authorization does not use manual browser presentation")
	}
	return flow.continueLocked(ctx, answer, onAuthorizationURL)
}

func (flow *RcloneAuthorizationFlow) continueLocked(
	ctx context.Context,
	answer string,
	onAuthorizationURL func(string),
) error {
	if flow.closed || flow.ready || flow.question == nil || flow.state == "" {
		return fmt.Errorf("rclone authorization is not awaiting an answer")
	}
	nativeAnswer := ""
	for index, example := range flow.question.Examples {
		if answer == rcloneAuthPublicChoiceID(index) || answer == example.Value {
			if rcloneAuthAnswerAllowed(flow.provider, flow.question.Name, example.Value) {
				nativeAnswer = example.Value
				break
			}
		}
	}
	if nativeAnswer == "" {
		return fmt.Errorf("rclone authorization answer is not one of the native choices")
	}
	state := flow.state
	if flow.question.Name == "config_driveid" {
		if !flow.oneDriveSurveyed {
			return fmt.Errorf("OneDrive locations have not been checked")
		}
		flow.expectedOneDriveID = nativeAnswer
	}
	flow.state, flow.question = "", nil
	return flow.advanceLocked(ctx, false, state, nativeAnswer, onAuthorizationURL)
}

func (flow *RcloneAuthorizationFlow) finalizeLocked(ctx context.Context) error {
	options, err := flow.discoverIdentityLocked(ctx)
	if err != nil {
		return err
	}
	if flow.provider.StorageType == "onedrive" && flow.expectedOneDriveID != "" &&
		options["drive_id"] != flow.expectedOneDriveID {
		return fmt.Errorf("native OneDrive location identity changed during selection")
	}
	flow.options = options
	flow.ready = true
	flow.state, flow.question = "", nil
	return nil
}

func (flow *RcloneAuthorizationFlow) discoverIdentityLocked(
	ctx context.Context,
) (map[string]string, error) {
	remote := "provider:"
	args := flow.args("lsjson", remote, "--stat")
	identityLog := ""
	switch flow.provider.StorageType {
	case "dropbox":
		// A leading slash makes the pinned Dropbox backend resolve the
		// account's actual root namespace through GetCurrentAccount. Rclone
		// exposes that value only in its structured debug log.
		remote = "provider:/"
		identityLog = filepath.Join(flow.tmp, "dropbox-identity.jsonl")
	case "google_drive":
		// The pinned Google Drive backend does not include the account root
		// ID in lsjson --stat. It emits the resolved native root only in its
		// structured debug log.
		identityLog = filepath.Join(flow.tmp, "google-drive-identity.jsonl")
	}
	if identityLog != "" {
		args = flow.argsWithLogLevel(
			"DEBUG", "lsjson", remote, "--stat", "--use-json-log",
			"--log-file", identityLog,
		)
	}
	output, err := runRcloneAuthorizationCommand(
		ctx, flow.binary, args,
		nil, 2*time.Minute,
	)
	if err != nil {
		return nil, fmt.Errorf("native rclone account identity discovery failed")
	}
	var root rcloneRootEntry
	if err := json.Unmarshal([]byte(output), &root); err != nil {
		return nil, fmt.Errorf("native rclone account identity discovery returned invalid JSON")
	}
	if identityLog != "" {
		if err := appdata.SecurePath(identityLog, false); err != nil {
			return nil, fmt.Errorf("secure native rclone identity output")
		}
	}
	values, configPresent, err := flow.readProviderConfigIfPresent(ctx)
	if err != nil {
		return nil, err
	}
	if !configPresent {
		return nil, fmt.Errorf("native rclone provider configuration is unavailable")
	}
	switch flow.provider.StorageType {
	case "google_drive":
		if values["team_drive"] == "" {
			rootID, err := readRcloneGoogleDriveRootIdentity(identityLog)
			if err != nil {
				return nil, err
			}
			values["root_folder_id"] = rootID
		}
	case "dropbox":
		namespace, err := readRcloneDropboxNamespaceIdentity(identityLog)
		if err != nil {
			return nil, err
		}
		values["root_namespace"] = namespace
	case "onedrive":
		if strings.TrimSpace(values["drive_id"]) == "" {
			return nil, fmt.Errorf("native OneDrive drive identity is unavailable")
		}
		if values["root_folder_id"] == "" && strings.TrimSpace(root.ID) != "" {
			values["root_folder_id"] = strings.TrimSpace(root.ID)
		}
	}
	if _, err := vaultidentity.IdentityWithOptions(
		ResticID, flow.provider.StorageType, "Replicaro/Authorization Probe", values,
	); err != nil {
		return nil, fmt.Errorf("native rclone account identity is unsafe: %w", err)
	}
	options := make(map[string]string, len(flow.provider.IdentityFields))
	for _, field := range flow.provider.IdentityFields {
		if value := strings.TrimSpace(values[field]); value != "" {
			options[field] = value
		}
	}
	return options, nil
}

type rcloneIdentityLogEntry struct {
	Level      string `json:"level"`
	Message    string `json:"msg"`
	ObjectType string `json:"objectType"`
	Source     string `json:"source"`
}

func rcloneIdentitySourceMatches(source, backendFile string) bool {
	separator := strings.LastIndexByte(source, ':')
	if separator <= 0 || separator == len(source)-1 {
		return false
	}
	line, err := strconv.Atoi(source[separator+1:])
	if err != nil || line <= 0 {
		return false
	}
	filename := source[:separator]
	return filename == backendFile || strings.HasSuffix(filename, "/"+backendFile)
}

func readRcloneDropboxNamespaceIdentity(filename string) (string, error) {
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", fmt.Errorf("native Dropbox identity output is unavailable")
	}
	file, err := os.Open(filename)
	if err != nil {
		return "", fmt.Errorf("native Dropbox identity output is unavailable")
	}
	defer file.Close()

	const prefix = `Using root namespace "`
	var namespace string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var entry rcloneIdentityLogEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			return "", fmt.Errorf("native Dropbox identity output returned invalid JSON")
		}
		if entry.Level != "debug" ||
			entry.ObjectType != "*dropbox.Fs" ||
			!rcloneIdentitySourceMatches(entry.Source, rcloneDropboxIdentitySourceFile) ||
			!strings.HasPrefix(entry.Message, prefix) ||
			!strings.HasSuffix(entry.Message, `"`) {
			continue
		}
		value := strings.TrimSuffix(strings.TrimPrefix(entry.Message, prefix), `"`)
		if !validRcloneDropboxNamespace(value) || namespace != "" {
			return "", fmt.Errorf("native Dropbox namespace identity is invalid")
		}
		namespace = value
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read native Dropbox identity output")
	}
	if namespace == "" {
		return "", fmt.Errorf("native Dropbox namespace identity is unavailable")
	}
	return namespace, nil
}

func readRcloneGoogleDriveRootIdentity(filename string) (string, error) {
	info, err := os.Lstat(filename)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return "", fmt.Errorf("native Google Drive identity output is unavailable")
	}
	file, err := os.Open(filename)
	if err != nil {
		return "", fmt.Errorf("native Google Drive identity output is unavailable")
	}
	defer file.Close()

	const (
		prefix = `'root_folder_id = `
		suffix = `' - save this in the config to speed up startup`
	)
	var rootID string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		var entry rcloneIdentityLogEntry
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
			return "", fmt.Errorf("native Google Drive identity output returned invalid JSON")
		}
		if entry.Level != "debug" ||
			entry.ObjectType != "*drive.Fs" ||
			!rcloneIdentitySourceMatches(entry.Source, rcloneGoogleDriveIdentitySourceFile) ||
			!strings.HasPrefix(entry.Message, prefix) ||
			!strings.HasSuffix(entry.Message, suffix) {
			continue
		}
		value := strings.TrimSuffix(strings.TrimPrefix(entry.Message, prefix), suffix)
		if !validRcloneGoogleDriveRootID(value) || rootID != "" {
			return "", fmt.Errorf("native Google Drive root identity is invalid")
		}
		rootID = value
	}
	if err := scanner.Err(); err != nil {
		return "", fmt.Errorf("read native Google Drive identity output")
	}
	if rootID == "" {
		return "", fmt.Errorf("native Google Drive root identity is unavailable")
	}
	return rootID, nil
}

func validRcloneGoogleDriveRootID(value string) bool {
	if value == "" || value == "root" || len(value) > 256 {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			character != '-' && character != '_' {
			return false
		}
	}
	return true
}

func validRcloneDropboxNamespace(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for index := range len(value) {
		character := value[index]
		if (character < '0' || character > '9') &&
			(character < 'a' || character > 'z') &&
			(character < 'A' || character > 'Z') &&
			character != '-' && character != '_' &&
			character != '.' && character != ':' {
			return false
		}
	}
	return true
}

func (flow *RcloneAuthorizationFlow) Status() (bool, *RcloneAuthQuestion) {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if flow.closed {
		return false, nil
	}
	if flow.question == nil {
		return flow.ready, nil
	}
	question := &RcloneAuthQuestion{
		ID:     flow.question.Name,
		Prompt: rcloneAuthPublicPrompt(flow.question.Name),
	}
	for index, example := range flow.question.Examples {
		if !rcloneAuthAnswerAllowed(flow.provider, flow.question.Name, example.Value) {
			continue
		}
		label := rcloneAuthPublicChoiceLabel(flow.question.Name, index)
		if flow.oneDriveSurveyed && flow.question.Name == "config_driveid" {
			label = rcloneAuthOneDriveLocationLabel(example.Help, index)
		}
		question.Choices = append(question.Choices, RcloneAuthChoice{
			Value: rcloneAuthPublicChoiceID(index),
			Label: label,
		})
		if example.Value == flow.question.DefaultStr {
			question.DefaultValue = rcloneAuthPublicChoiceID(index)
		}
	}
	if question.DefaultValue == "" && len(question.Choices) > 0 &&
		!(flow.oneDriveSurveyed && flow.question.Name == "config_driveid") {
		question.DefaultValue = question.Choices[0].Value
	}
	return false, question
}

// IdentityOptions returns only credential-independent provider identity.
func (flow *RcloneAuthorizationFlow) IdentityOptions() (map[string]string, error) {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if flow.closed || !flow.ready {
		return nil, fmt.Errorf("rclone authorization is not complete")
	}
	result := make(map[string]string, len(flow.provider.IdentityFields))
	for _, key := range flow.provider.IdentityFields {
		if value := strings.TrimSpace(flow.options[key]); value != "" {
			result[key] = value
		}
	}
	if err := requireRcloneProviderIdentityOptions(flow.provider, result); err != nil {
		return nil, err
	}
	return result, nil
}

// StagedConfigPath returns the still-session-owned native config path.
func (flow *RcloneAuthorizationFlow) StagedConfigPath() (string, error) {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if flow.closed || !flow.ready {
		return "", fmt.Errorf("rclone authorization is not complete")
	}
	if err := validateRcloneVaultFile(flow.config); err != nil {
		return "", fmt.Errorf("native rclone authorization config is unsafe")
	}
	return flow.config, nil
}

// PublishVaultConfig securely transfers the opaque native config to the
// authoritative repository-ID path. A retained pre-activation failure leaves
// the candidate in this flow; activation and indeterminate results consume it.
func (flow *RcloneAuthorizationFlow) PublishVaultConfig(
	ctx context.Context,
	repositoryID string,
) (RcloneConfigActivation, error) {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if flow.closed || !flow.ready {
		return RcloneConfigActivation{Disposition: RcloneConfigRetained}, fmt.Errorf("rclone authorization is not complete")
	}
	return promoteRcloneVaultConfig(ctx, flow.binary, flow.config, repositoryID)
}

func (flow *RcloneAuthorizationFlow) Provider() string {
	return flow.provider.StorageType
}

func (flow *RcloneAuthorizationFlow) Close() error {
	flow.mu.Lock()
	defer flow.mu.Unlock()
	if flow.cleanupComplete {
		return nil
	}
	if !flow.closed {
		flow.closed = true
		for key := range flow.options {
			flow.options[key] = ""
			delete(flow.options, key)
		}
	}
	wipeErr := wipeRcloneAuthorizationConfig(flow.config)
	removeErr := errors.Join(removeRcloneSession(flow.root), removeRcloneSession(flow.operationRoot))
	if wipeErr == nil && removeErr == nil {
		flow.cleanupComplete = true
	}
	return errors.Join(wipeErr, removeErr)
}

func wipeRcloneAuthorizationConfig(filename string) error {
	if err := wipeRcloneConfig(filename); err != nil {
		return fmt.Errorf("wipe private rclone authorization config: %w", err)
	}
	return nil
}

func IsRcloneNativeLoginProvider(storageType string) bool {
	_, err := rcloneNativeLoginProvider(storageType)
	return err == nil
}
