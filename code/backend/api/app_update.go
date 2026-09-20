package api

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/local/replicaro/appupdate"
	"github.com/local/replicaro/desktop"
	"github.com/local/replicaro/models"
)

var openReplicaroRelease = desktop.OpenReplicaroRelease
var errMethodNotAllowed = errors.New("method not allowed")

func registerAppUpdateHandlers(mux *http.ServeMux, updater *appupdate.Service, readDB *sql.DB) {
	handle(mux, "/api/app-update/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeError(w, http.StatusMethodNotAllowed, errMethodNotAllowed)
			return
		}
		status, err := updater.StatusWithReader(readDB)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, status)
	})

	handle(mux, "/api/app-update/check", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, errMethodNotAllowed)
			return
		}
		status, err := updater.Check(r.Context(), false)
		if err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, status)
	})

	handle(mux, "/api/app-update/skip", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, errMethodNotAllowed)
			return
		}
		var request struct {
			Version string `json:"version"`
		}
		if err := decodeRequest(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		status, stored, err := updater.Skip(request.Version)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, struct {
			appupdate.Status
			Stored bool `json:"stored"`
		}{Status: status, Stored: stored})
	})

	handle(mux, "/api/app-update/open", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed, errMethodNotAllowed)
			return
		}
		var request struct {
			Version string `json:"version"`
		}
		if err := decodeRequest(r, &request); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if _, err := models.ParseSemanticVersion(request.Version); err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		if err := openReplicaroRelease(request.Version); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}
