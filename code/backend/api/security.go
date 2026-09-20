package api

import (
	"context"
	"database/sql"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/runtimeendpoint"
)

const maxJSONBodyBytes int64 = 4 << 20

type securityContextKey struct{}

type SecurityMode uint8

const (
	SecurityModeProduction SecurityMode = iota
	SecurityModeDevelopment
)

type requestSecurity struct {
	endpoint   runtimeendpoint.Endpoint
	mode       SecurityMode
	clientUUID string
}

func installationUUID(db *sql.DB) string {
	if db == nil {
		return ""
	}
	id, err := database.InstallationID(db)
	if err != nil {
		return ""
	}
	return id
}

var endpointMethods = map[string][]string{
	"/api/health": {http.MethodGet}, "/api/integrations": {http.MethodGet},
	"/api/vaults/connect/preview": {http.MethodPost}, "/api/vaults/connect": {http.MethodPost},
	"/api/vaults/connect/retry":   {http.MethodPost},
	"/api/filesystem/directories": {http.MethodGet}, "/api/dashboard": {http.MethodGet},
	"/api/dashboard/issues": {http.MethodGet}, "/api/dashboard/issues/review": {http.MethodPost},
	"/api/dashboard/activity": {http.MethodGet}, "/api/operations/active": {http.MethodGet},
	"/api/operations/live": {http.MethodGet}, "/api/operations/cancel": {http.MethodPost},
	"/api/operations/output": {http.MethodGet},
	"/api/operations":        {http.MethodGet}, "/api/repositories": {http.MethodGet, http.MethodPost, http.MethodDelete},
	"/api/repository-creation-intents": {http.MethodGet, http.MethodDelete}, "/api/repository": {http.MethodGet},
	"/api/repository-connection-intents": {http.MethodGet, http.MethodDelete},
	"/api/dormant-recovery-jobs":         {http.MethodGet, http.MethodPost, http.MethodDelete},
	"/api/repository/schedules":          {http.MethodPut}, "/api/repository/info": {http.MethodGet},
	"/api/repository/ownership":      {http.MethodGet, http.MethodPost},
	"/api/repository/password":       {http.MethodGet, http.MethodPost},
	"/api/repository/password/retry": {http.MethodPost},
	"/api/repository/credentials":    {http.MethodPut},
	"/api/repository/check":          {http.MethodPost}, "/api/repository/maintenance": {http.MethodPost},
	"/api/repository/vault-size": {http.MethodGet}, "/api/repository/vault-size/prepare": {http.MethodPost}, "/api/repository/snapshots": {http.MethodGet},
	"/api/snapshot/files": {http.MethodGet}, "/api/files/search": {http.MethodGet}, "/api/files/browse": {http.MethodGet},
	"/api/files/history": {http.MethodGet}, "/api/files/index/retry": {http.MethodPost}, "/api/snapshot": {http.MethodDelete},
	"/api/metadata/status": {http.MethodGet}, "/api/metadata/prepare": {http.MethodPost},
	"/api/restore": {http.MethodPost}, "/api/restore-selection": {http.MethodPost},
	"/api/repository/snapshot": {http.MethodPost}, "/api/jobs": {http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete},
	"/api/rclone/auth/start": {http.MethodPost}, "/api/rclone/auth/continue": {http.MethodPost},
	"/api/rclone/auth/status":  {http.MethodPost},
	"/api/rclone/auth/session": {http.MethodDelete}, "/api/rclone/auth/apply": {http.MethodPost},
	"/api/jobs/run": {http.MethodPost}, "/api/jobs/status": {http.MethodGet},
	"/api/jobs/enabled": {http.MethodPut}, "/api/engines": {http.MethodGet},
	"/api/vault-profile-sync": {http.MethodGet, http.MethodPost},
	"/api/engine":             {http.MethodGet}, "/api/logs": {http.MethodGet, http.MethodDelete},
	"/api/settings": {http.MethodGet, http.MethodPost},
	"/api/activate": {http.MethodPost},
}

func secureAPIRequest(w http.ResponseWriter, r *http.Request) bool {
	security := securityFromRequest(r)
	setSecurityHeaders(w.Header(), security)
	if !approvedHost(r.Host, security) {
		writeSecurityError(w, http.StatusForbidden, "request host is not permitted")
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" {
		if !approvedOrigin(origin, security) {
			writeSecurityError(w, http.StatusForbidden, "request origin is not permitted")
			return false
		}
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	}
	if !approvedFetchMetadata(r, security) {
		writeSecurityError(w, http.StatusForbidden, "cross-site browser request is not permitted")
		return false
	}
	if r.Method == http.MethodOptions {
		if origin == "" {
			writeSecurityError(w, http.StatusForbidden, "preflight origin is required")
			return false
		}
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, X-Replicaro-Client-UUID, X-Replicaro-Vault-Progress")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return false
	}
	// Deliberately use the existing installation UUID, not a second token/session
	// system. Replicaro does not promise isolation from other users of this host;
	// the HTML shell publishes this value. This check requires installation-aware
	// API clients, while Host/Origin/Fetch Metadata still guard browser requests.
	// Do not treat the UUID as a secret or weaken those independent checks.
	// OPTIONS above runs no application handler and cannot carry the eventual
	// request's custom header.
	if security.clientUUID == "" || len(r.Header.Values("X-Replicaro-Client-UUID")) != 1 || r.Header.Get("X-Replicaro-Client-UUID") != security.clientUUID {
		writeSecurityError(w, http.StatusForbidden, "installation UUID is missing or does not match")
		return false
	}
	if allowed, ok := endpointMethods[r.URL.Path]; ok && !methodAllowed(r.Method, allowed) {
		w.Header().Set("Allow", strings.Join(append(append([]string{}, allowed...), http.MethodOptions), ", "))
		writeSecurityError(w, http.StatusMethodNotAllowed, "method is not allowed")
		return false
	}
	if requestMayHaveJSONBody(r) {
		if err := requireJSONContentType(r); err != nil {
			writeSecurityError(w, http.StatusUnsupportedMediaType, err.Error())
			return false
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	}
	return true
}

func methodAllowed(method string, allowed []string) bool {
	for _, candidate := range allowed {
		if method == candidate {
			return true
		}
	}
	return false
}

// Fetch Metadata is a browser signal, not authentication. Native clients that
// omit it remain supported; browsers explicitly reporting cross-site are denied.
func approvedFetchMetadata(r *http.Request, security requestSecurity) bool {
	site := strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site")))
	if site == "" || site == "same-origin" || site == "same-site" || site == "none" {
		return true
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	// An explicitly constructed development handler may use Vite on one exact
	// loopback origin. The already validated Origin keeps that usable.
	return approvedOrigin(strings.TrimSpace(r.Header.Get("Origin")), security)
}

func setSecurityHeaders(header http.Header, security requestSecurity) {
	header.Set("X-Content-Type-Options", "nosniff")
	header.Set("X-Frame-Options", "DENY")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Content-Security-Policy", contentSecurityPolicy(security))
}

func contentSecurityPolicy(security requestSecurity) string {
	port := strconv.Itoa(security.endpoint.Port())
	connectSources := []string{
		"'self'",
		"http://127.0.0.1:" + port,
		"http://localhost:" + port,
	}
	if security.mode == SecurityModeDevelopment {
		connectSources = append(connectSources,
			"http://127.0.0.1:5173",
			"http://localhost:5173",
		)
	}
	return "default-src 'self'; base-uri 'none'; frame-ancestors 'none'; object-src 'none'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; script-src 'self'; connect-src " + strings.Join(connectSources, " ")
}

func approvedHost(host string, security requestSecurity) bool {
	host = strings.TrimSpace(host)
	return host != "" && security.endpoint.HostAllowed(host)
}

func approvedOrigin(origin string, security requestSecurity) bool {
	parsed, err := url.Parse(origin)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Hostname() == "" {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "localhost" && host != "127.0.0.1" {
		return false
	}
	port := parsed.Port()
	if port == "" {
		return false
	}
	if port == strconv.Itoa(security.endpoint.Port()) {
		return true
	}
	return security.mode == SecurityModeDevelopment && port == "5173"
}

func securityFromRequest(r *http.Request) requestSecurity {
	if value, ok := r.Context().Value(securityContextKey{}).(requestSecurity); ok && value.endpoint.Port() > 0 {
		return value
	}
	return requestSecurity{endpoint: runtimeendpoint.Preferred(), mode: SecurityModeProduction}
}

func withSecurity(r *http.Request, security requestSecurity) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), securityContextKey{}, security))
}

func requestMayHaveJSONBody(r *http.Request) bool {
	return r.Method == http.MethodPost || r.Method == http.MethodPut || r.Method == http.MethodPatch
}

func requireJSONContentType(r *http.Request) error {
	if strings.TrimSpace(r.Header.Get("Content-Type")) == "" {
		return fmt.Errorf("Content-Type application/json is required")
	}
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !strings.EqualFold(mediaType, "application/json") {
		return fmt.Errorf("Content-Type application/json is required")
	}
	return nil
}

func writeSecurityError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"error":"` + message + `"}`))
}
