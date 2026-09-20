package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/local/replicaro/command"
)

const vaultProgressHeader = "X-Replicaro-Vault-Progress"

type vaultProgressKey struct{}
type vaultProgressSink func(kind, stream, message string)

// Progress is request-scoped presentation. The existing synchronous handlers
// still own admission, durable intents, locks, native results, and failure
// recovery; disconnecting this stream never creates a second job authority.
func reportVaultProgress(ctx context.Context, message string) {
	if sink, ok := ctx.Value(vaultProgressKey{}).(vaultProgressSink); ok {
		sink("stage", "", message)
	}
}

type vaultProgressResponse struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (response *vaultProgressResponse) Header() http.Header { return response.header }
func (response *vaultProgressResponse) WriteHeader(status int) {
	if response.status == 0 {
		response.status = status
	}
}
func (response *vaultProgressResponse) Write(data []byte) (int, error) {
	if response.status == 0 {
		response.status = http.StatusOK
	}
	return response.body.Write(data)
}

func vaultProgressStart(path string) string {
	switch path {
	case "/api/repositories":
		return "Checking the new vault details..."
	case "/api/vaults/connect/preview", "/api/vaults/connect/profile":
		return "Checking the existing vault and its native repository..."
	case "/api/vaults/connect/retry":
		return "Continuing the previous connection..."
	default:
		return "Verifying vault identity and finishing this computer's attachment..."
	}
}

func vaultProgressRoute(path string) bool {
	switch path {
	case "/api/repositories", "/api/vaults/connect/preview", "/api/vaults/connect/profile", "/api/vaults/connect", "/api/vaults/connect/retry":
		return true
	default:
		return false
	}
}

// The stream wraps the normal JSON response only when explicitly requested by
// the UI. The inner support recorder still sees the real status and body.
func withVaultProgress(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !vaultProgressRoute(r.URL.Path) || r.Header.Get(vaultProgressHeader) != "1" {
			next.ServeHTTP(w, r)
			return
		}
		_, ok := w.(http.Flusher)
		if !ok {
			next.ServeHTTP(w, r)
			return
		}
		// The first stage is flushed before the JSON handler reads the POST body.
		// HTTP/1 closes unread request bodies on an early response unless full
		// duplex is enabled. Fall back to ordinary JSON if the writer cannot do it.
		if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
			next.ServeHTTP(w, r)
			return
		}
		// Admission also supplies CORS and security headers on the outer stream.
		// The normal route retains its own check and remains the request authority.
		if !secureAPIRequest(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(http.StatusOK)
		controller := http.NewResponseController(w)
		streamFailed := false
		writeRecord := func(record any) {
			if streamFailed || r.Context().Err() != nil {
				return
			}
			if err := json.NewEncoder(w).Encode(record); err != nil {
				streamFailed = true
				return
			}
			if err := controller.Flush(); err != nil {
				streamFailed = true
			}
		}
		writeRecord(map[string]string{"type": "stage", "stream": "", "text": vaultProgressStart(r.URL.Path)})
		// Command pipe readers invoke the native observer synchronously. Keep
		// network backpressure off those readers; presentation may drop records
		// when its bounded queue is full, while native output/result stay intact.
		events := make(chan any, 128)
		completed := make(chan struct{})
		writerDone := make(chan struct{})
		var terminal any
		go func() {
			defer close(writerDone)
			for {
				select {
				case <-completed:
					// Preserve queued stage/native order for responsive clients;
					// the final write deadline bounds a stalled peer.
					for draining := true; draining; {
						select {
						case record := <-events:
							writeRecord(record)
						default:
							draining = false
						}
					}
					writeRecord(terminal)
					return
				case record := <-events:
					writeRecord(record)
				case <-r.Context().Done():
					return
				}
			}
		}()
		writeEvent := func(kind, stream, message string) {
			select {
			case events <- map[string]string{"type": kind, "stream": stream, "text": message}:
			default:
			}
		}
		ctx := context.WithValue(r.Context(), vaultProgressKey{}, vaultProgressSink(writeEvent))
		ctx = command.ContextWithLiveOutput(ctx, func(stream, output string) {
			// Native adapters enable this observer only at admitted ordinary
			// command boundaries; private probes and authorization stay opaque.
			writeEvent("native", stream, output)
		})
		response := &vaultProgressResponse{header: make(http.Header)}
		next.ServeHTTP(response, r.WithContext(ctx))
		status := response.status
		if status == 0 {
			status = http.StatusOK
		}
		body := bytes.TrimSpace(response.body.Bytes())
		if !json.Valid(body) {
			body = []byte(`{"error":"The vault request returned an invalid response."}`)
		}
		// RawMessage preserves the original success or coded-error envelope.
		terminal = struct {
			Type   string          `json:"type"`
			Status int             `json:"status"`
			Body   json.RawMessage `json:"body"`
		}{"result", status, json.RawMessage(body)}
		// Only after native work has returned may a stalled client bound the
		// final presentation write. It cannot impose a deadline on that work.
		_ = controller.SetWriteDeadline(time.Now().Add(2 * time.Second))
		close(completed)
		<-writerDone
	})
}
