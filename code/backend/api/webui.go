package api

import (
	"bytes"
	"database/sql"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/local/replicaro/appupdate"
	"github.com/local/replicaro/locale"
	"github.com/local/replicaro/operationruntime"
	"github.com/local/replicaro/rendezvous"
	"github.com/local/replicaro/runtimeendpoint"
)

func ApplicationHandler(db *sql.DB) http.Handler {
	return ApplicationHandlerAt(db, runtimeendpoint.Preferred(), rendezvous.Record{}, nil)
}

func ApplicationHandlerAt(db *sql.DB, endpoint runtimeendpoint.Endpoint, record rendezvous.Record, activate func() error) http.Handler {
	return ApplicationHandlerAtWithSecurityMode(db, endpoint, record, activate, SecurityModeProduction)
}

func ApplicationHandlerAtWithSecurityMode(db *sql.DB, endpoint runtimeendpoint.Endpoint, record rendezvous.Record, activate func() error, mode SecurityMode) http.Handler {
	return applicationHandlerAtWithSecurityModeAndRcloneAuth(
		db, db, endpoint, record, activate, mode, newRcloneAuthStore(), appupdate.New(db),
	)
}

func applicationHandlerAtWithSecurityModeAndRcloneAuth(
	db, readDB *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	record rendezvous.Record,
	activate func() error,
	mode SecurityMode,
	rcloneAuth *rcloneAuthStore,
	updater *appupdate.Service,
	runtimeManagers ...*operationruntime.Manager,
) http.Handler {
	runtimeManager := operationruntime.New()
	if len(runtimeManagers) > 0 && runtimeManagers[0] != nil {
		runtimeManager = runtimeManagers[0]
	}
	security := requestSecurity{endpoint: endpoint, mode: normalizedSecurityMode(mode), clientUUID: installationUUID(db)}
	apiHandler := handlerAtWithSecurityModeRcloneAuthAndRuntime(
		db, readDB, endpoint, record, activate, mode, rcloneAuth, updater, runtimeManager,
	)
	webUI, err := webUIFileSystem()
	if err == nil {
		if _, catalogErr := fs.Stat(webUI, "locales/en.json"); catalogErr == nil {
			err = locale.LoadFS(webUI)
		}
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r = withSecurity(r, security)
		setSecurityHeaders(w.Header(), security)
		if strings.HasPrefix(r.URL.Path, "/api/") {
			apiHandler.ServeHTTP(w, r)
			return
		}
		if !approvedHost(r.Host, security) || !approvedFetchMetadata(r, security) {
			writeSecurityError(w, http.StatusForbidden, "request is not permitted")
			return
		}
		if origin := strings.TrimSpace(r.Header.Get("Origin")); origin != "" && !approvedOrigin(origin, security) {
			writeSecurityError(w, http.StatusForbidden, "request origin is not permitted")
			return
		}

		if err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}

		serveWebUI(webUI, w, r)
	})
}

func webUIFileSystem() (fs.FS, error) {
	var candidates []string
	if configured := os.Getenv("REPLICARO_WEB_UI_DIR"); configured != "" {
		candidates = append(candidates, configured)
	}
	if directory := firstWebUIDirectory(candidates); directory != "" {
		return os.DirFS(directory), nil
	}

	if embedded, available, err := packagedWebUI(); available || err != nil {
		return embedded, err
	}

	candidates = candidates[:0]

	if executable, err := os.Executable(); err == nil {
		base := filepath.Dir(executable)
		candidates = append(candidates,
			filepath.Join(base, "frontend", "dist"),
			filepath.Join(base, "frontend"),
			filepath.Join(base, "dist"),
			filepath.Join(base, "..", "frontend", "dist"),
			filepath.Join(base, "..", "frontend"),
			// macOS .app bundles keep the executable under Contents/MacOS and
			// the UI under Contents/Resources.
			filepath.Join(base, "..", "Resources", "frontend", "dist"),
			filepath.Join(base, "..", "Resources", "frontend"),
		)
	}

	if workingDirectory, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(workingDirectory, "frontend", "dist"),
			filepath.Join(workingDirectory, "..", "frontend", "dist"),
		)
	}

	if directory := firstWebUIDirectory(candidates); directory != "" {
		return os.DirFS(directory), nil
	}

	return nil, fmt.Errorf("web interface is not built; run npm run build in the frontend directory")
}

func firstWebUIDirectory(candidates []string) string {
	for _, candidate := range candidates {
		index := filepath.Join(candidate, "index.html")
		if info, statErr := os.Stat(index); statErr == nil && !info.IsDir() {
			return filepath.Clean(candidate)
		}
	}
	return ""
}

func serveWebUI(webUI fs.FS, w http.ResponseWriter, r *http.Request) {
	requested := strings.TrimPrefix(path.Clean("/"+r.URL.Path), "/")
	if requested != "" && requested != "." && fs.ValidPath(requested) && !strings.Contains(requested, `\`) {
		if info, err := fs.Stat(webUI, requested); err == nil && !info.IsDir() {
			serveWebUIFile(webUI, w, r, requested)
			return
		}
	}

	// React Router owns client-side routes, so unknown paths load the app shell.
	serveWebUIFile(webUI, w, r, "index.html")
}

func serveWebUIFile(webUI fs.FS, w http.ResponseWriter, r *http.Request, filename string) {
	if path.Base(filename) == "index.html" {
		id := securityFromRequest(r).clientUUID
		if id == "" {
			http.Error(w, "installation UUID is unavailable", http.StatusServiceUnavailable)
			return
		}
		data, err := fs.ReadFile(webUI, filename)
		if err != nil {
			http.Error(w, "web interface is unavailable", http.StatusServiceUnavailable)
			return
		}
		// A validated canonical UUID requires no HTML escaping. Do not inject
		// executable script or modify the packaged asset on disk.
		metadata := []byte(`<meta name="replicaro-client-uuid" content="` + id + `">`)
		if !bytes.Contains(data, []byte("</head>")) {
			http.Error(w, "web interface has no document head", http.StatusServiceUnavailable)
			return
		}
		data = bytes.Replace(data, []byte("</head>"), append(metadata, []byte("</head>")...), 1)
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(data))
		return
	} else {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	http.ServeFileFS(w, r, webUI, filename)
}
