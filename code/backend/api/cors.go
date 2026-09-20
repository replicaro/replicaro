package api

import "net/http"

func enableCORS(w http.ResponseWriter, r *http.Request) {

	origin := r.Header.Get("Origin")
	if approvedOrigin(origin, securityFromRequest(r)) {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Add("Vary", "Origin")
	}

	w.Header().Set(
		"Access-Control-Allow-Headers",
		"Content-Type",
	)

	w.Header().Set(
		"Access-Control-Allow-Methods",
		"GET, POST, PUT, DELETE, OPTIONS",
	)
}
