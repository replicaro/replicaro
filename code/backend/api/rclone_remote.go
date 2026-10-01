package api

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/local/replicaro/engines"
)

// Any Rclone Remote vaults have no sign-in session and no private rclone
// config. Instead, every create, connect, and reconnect request checks the
// user's own config file and remote first (engines.PreflightRcloneRemote) and
// refuses with the check's coded error before anything is saved. The check
// bounds its own file access, so a config file on a hung share ends the
// request with its own code instead of keeping it open.

const rcloneRemoteListPath = "/api/vaults/rclone-remote/remotes"

// Variables only so tests can replace the native checks.
var preflightRcloneRemote = engines.PreflightRcloneRemote
var listRcloneRemotes = engines.ListRcloneRemotes

// checkRcloneRemoteSettings runs the preflight for an Any Rclone Remote vault
// and does nothing for any other storage type. The connector is compared the
// way engines.RcloneProvider compares it, because this runs before the
// request's own normalization: a padded or differently cased ID must not skip
// the check.
func checkRcloneRemoteSettings(ctx context.Context, connector string, options map[string]string) error {
	if strings.ToLower(strings.TrimSpace(connector)) != engines.RcloneRemoteConnector {
		return nil
	}
	return preflightRcloneRemote(ctx, options)
}

// writeRcloneRemoteError answers a refused Any Rclone Remote setting or a
// failed check with its code and the values its message shows, so the UI can
// show a translated message. It reports false for any other error.
func writeRcloneRemoteError(w http.ResponseWriter, err error) bool {
	var refused *engines.RcloneRemoteError
	if !errors.As(err, &refused) {
		return false
	}
	status := http.StatusBadRequest
	if refused.Code == engines.RcloneRemoteConfigNotRespondingCode {
		// Like a folder that doesn't respond in the folder picker.
		status = http.StatusGatewayTimeout
	} else {
		// The user's settings were refused; this is not a Replicaro fault.
		markSupportResponseError(w, err, true)
	}
	response := map[string]any{"error": refused.Message, "code": refused.Code}
	if refused.ConfigFile != "" {
		response["configFile"] = refused.ConfigFile
	}
	if refused.Remote != "" {
		response["remote"] = refused.Remote
	}
	if len(refused.Names) != 0 {
		response["names"] = refused.Names
	}
	if refused.Detail != "" {
		response["detail"] = refused.Detail
	}
	writeJSONStatus(w, status, response)
	return true
}

// rcloneRemoteListRequest carries the form's connector options. Only the
// settings that decide which remotes rclone sees are used; the remote name
// and path in remote, which are chosen from the answer, are ignored.
type rcloneRemoteListRequest struct {
	Options map[string]string `json:"options"`
}

type rcloneRemoteListEntry struct {
	Name string `json:"name"`
	Type string `json:"type"`
}

// handleRcloneRemoteList lists the remotes of the user's rclone config file
// for the form's remote picker. The rclone config password and the
// environment variables travel only in the request body.
func handleRcloneRemoteList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		badRequest(w, "invalid method")
		return
	}
	var request rcloneRemoteListRequest
	if err := decodeRequest(r, &request); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	options := map[string]string{}
	for _, key := range []string{
		engines.RcloneRemoteConfigFileOption, engines.RcloneRemoteConfigEncryptedOption,
		engines.RcloneRemoteConfigPasswordOption, engines.RcloneRemoteEnvironmentOption,
	} {
		if value, ok := request.Options[key]; ok {
			options[key] = value
		}
	}
	entries, err := listRcloneRemotes(r.Context(), options)
	if err != nil {
		if !writeRcloneRemoteError(w, err) {
			writeError(w, http.StatusBadRequest, err)
		}
		return
	}
	remotes := make([]rcloneRemoteListEntry, 0, len(entries))
	for _, entry := range entries {
		remotes = append(remotes, rcloneRemoteListEntry{Name: entry.Name, Type: entry.Type})
	}
	writeJSON(w, map[string]any{"remotes": remotes})
}
