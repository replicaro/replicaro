package api

import (
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strconv"
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
		db, db, endpoint, nil, record, activate, mode, newRcloneAuthStore(), appupdate.New(db), false, false,
	)
}

func applicationHandlerAtWithSecurityModeAndRcloneAuth(
	db, readDB *sql.DB,
	endpoint runtimeendpoint.Endpoint,
	lanOrigins runtimeendpoint.LANOrigins,
	record rendezvous.Record,
	activate func() error,
	mode SecurityMode,
	rcloneAuth *rcloneAuthStore,
	updater *appupdate.Service,
	keepStartAtLogin bool,
	containerPackage bool,
	runtimeManagers ...*operationruntime.Manager,
) http.Handler {
	runtimeManager := operationruntime.New()
	if len(runtimeManagers) > 0 && runtimeManagers[0] != nil {
		runtimeManager = runtimeManagers[0]
	}
	// Page loads are checked with this copy. The API handler below builds its
	// own copy from the same lanOrigins, which replaces this one for /api/.
	security := requestSecurity{
		endpoint: endpoint, lanOrigins: lanOrigins, mode: normalizedSecurityMode(mode), clientUUID: installationUUID(db),
	}
	apiHandler := handlerAtWithSecurityModeRcloneAuthAndRuntime(
		db, readDB, endpoint, lanOrigins, record, activate, mode, rcloneAuth, updater, keepStartAtLogin, containerPackage, runtimeManager,
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
	// The .gz files next to the assets are an alternative encoding of those
	// assets, chosen by Accept-Encoding in serveWebUIFile. They are not URLs
	// of their own, so a request naming one gets the app shell like any other
	// unknown path instead of a raw gzip body.
	if requested != "" && requested != "." && fs.ValidPath(requested) && !strings.Contains(requested, `\`) &&
		!strings.HasSuffix(requested, gzipSuffix) {
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
		// Never compressed: the shell carries the installation UUID, and
		// compressing a secret in the same response as request-influenced
		// content is what BREACH-style attacks measure. It is tiny anyway.
		http.ServeContent(w, r, "index.html", time.Time{}, bytes.NewReader(data))
		return
	}
	if strings.HasPrefix(filename, "locales/") {
		// Catalogs keep the same names in every release (the UI fetches
		// /locales/<locale>.json), so an immutable year-long cache would keep
		// last version's text after an upgrade. no-cache makes the browser
		// check back each time; the embedded files carry no modification time
		// or ETag, so that means a fresh download of one small catalog per
		// page load.
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		// Everything else is a Vite output whose name changes with its
		// content, or a fixed image that is always referenced with a ?cb=
		// content token.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	if info, err := fs.Stat(webUI, filename+gzipSuffix); err == nil && info.Mode().IsRegular() {
		// Both encodings are cacheable under one URL, so shared caches must
		// key on Accept-Encoding for the plain response as well.
		w.Header().Add("Vary", "Accept-Encoding")
		if r.Header.Get("Range") == "" && acceptsGzip(r.Header.Values("Accept-Encoding")) && serveGzipCopy(webUI, w, r, filename) {
			return
		}
	}
	http.ServeFileFS(w, r, webUI, filename)
}

// gzipSuffix names the build-time compressed copy that tools/webuiembed writes
// next to each JavaScript, CSS, and locale catalog file. The copy is made from
// the exact bytes embedded as the plain file, in the same build step, so the
// two can't drift apart; the plain file stays embedded for clients that don't
// accept gzip and for locale.LoadFS, which reads the plain catalogs.
const gzipSuffix = ".gz"

// serveGzipCopy sends the precompressed copy of filename. It reports false,
// having written nothing, when the copy can't be used, so the caller falls
// back to the plain file. Range requests never reach it: they get the plain
// file, which keeps byte ranges meaningful and avoids ranges over gzip data.
func serveGzipCopy(webUI fs.FS, w http.ResponseWriter, r *http.Request, filename string) bool {
	// Same type the plain file is served with. Without a known type we'd
	// have to sniff, which needs the plain bytes, so just serve those.
	contentType := mime.TypeByExtension(path.Ext(filename))
	if contentType == "" {
		return false
	}
	file, err := webUI.Open(filename + gzipSuffix)
	if err != nil {
		return false
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	header := w.Header()
	header.Set("Content-Type", contentType)
	header.Set("Content-Encoding", "gzip")
	header.Set("Content-Length", strconv.FormatInt(info.Size(), 10))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		_, _ = io.Copy(w, file)
	}
	return true
}

// acceptsGzip applies the Accept-Encoding rules that matter here: an explicit
// gzip entry decides, otherwise a * entry does, and a q value of 0 means "not
// acceptable". A missing header or a malformed q value means no gzip.
func acceptsGzip(values []string) bool {
	gzipQuality, wildcardQuality := -1.0, -1.0
	for _, value := range values {
		for _, entry := range strings.Split(value, ",") {
			parts := strings.Split(entry, ";")
			coding := strings.ToLower(strings.TrimSpace(parts[0]))
			if coding != "gzip" && coding != "*" {
				continue
			}
			quality := 1.0
			for _, parameter := range parts[1:] {
				name, number, found := strings.Cut(strings.TrimSpace(parameter), "=")
				if !found || !strings.EqualFold(strings.TrimSpace(name), "q") {
					continue
				}
				parsed, err := strconv.ParseFloat(strings.TrimSpace(number), 64)
				if err != nil || parsed < 0 || parsed > 1 {
					parsed = 0
				}
				quality = parsed
			}
			if coding == "gzip" {
				gzipQuality = max(gzipQuality, quality)
			} else {
				wildcardQuality = max(wildcardQuality, quality)
			}
		}
	}
	if gzipQuality >= 0 {
		return gzipQuality > 0
	}
	return wildcardQuality > 0
}
