package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"
)

const rcloneAuthCallbackPort = 53682

type rcloneAuthRelayFactory func(string) (io.Closer, error)

type rcloneAuthRelay struct {
	server    *http.Server
	listener  net.Listener
	closeOnce sync.Once
	closeErr  error
}

// startRcloneAuthRelay opens the container-side half of one temporary Docker
// published-port relay. The browser-visible URL is unchanged: pinned rclone
// still owns the callback listener, state validation, exchange, token and
// private config on exact internal loopback 127.0.0.1:53682.
func startRcloneAuthRelay(relayPort int, authorizationURL string) (io.Closer, error) {
	if relayPort < 1 || relayPort > 65535 || relayPort == rcloneAuthCallbackPort {
		return nil, fmt.Errorf("rclone authorization relay port is invalid")
	}
	parsed, err := url.Parse(authorizationURL)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil ||
		parsed.Hostname() != "127.0.0.1" || parsed.Port() != strconv.Itoa(rcloneAuthCallbackPort) ||
		parsed.Path != "/auth" || parsed.RawPath != "" || parsed.Fragment != "" {
		return nil, fmt.Errorf("rclone authorization relay URL is invalid")
	}
	query := parsed.Query()
	states, ok := query["state"]
	if !ok || len(query) != 1 || len(states) != 1 || states[0] == "" {
		return nil, fmt.Errorf("rclone authorization relay state is invalid")
	}
	expectedState := states[0]

	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + strconv.Itoa(rcloneAuthCallbackPort)}
	proxy := &httputil.ReverseProxy{
		Rewrite: func(request *httputil.ProxyRequest) {
			request.SetURL(target)
			// ReverseProxy sanitizes malformed query fields before Rewrite. Pinned
			// rclone owns ParseForm failure semantics, so restore the exact admitted
			// provider query rather than silently turning malformed input valid.
			request.Out.URL.RawQuery = request.In.URL.RawQuery
			request.Out.Host = target.Host
			request.Out.Header.Del("Forwarded")
			request.Out.Header.Del("X-Forwarded-For")
			request.Out.Header.Del("X-Forwarded-Host")
			request.Out.Header.Del("X-Forwarded-Proto")
		},
		ErrorLog: log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, _ error) {
			http.Error(w, "native rclone authorization callback is unavailable", http.StatusBadGateway)
		},
	}

	handler := http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		query := request.URL.Query()
		state := query["state"]
		validRoute := request.URL.Path == "/" || (request.URL.Path == "/auth" && len(query) == 1)
		if request.Method != http.MethodGet || !validRoute || request.URL.RawPath != "" ||
			!callbackHostAllowed(request.Host) || len(state) != 1 || state[0] != expectedState {
			http.Error(w, "rclone authorization callback is not permitted", http.StatusForbidden)
			return
		}
		proxy.ServeHTTP(w, request)
	})
	listener, err := net.Listen("tcp4", "0.0.0.0:"+strconv.Itoa(relayPort))
	if err != nil {
		return nil, fmt.Errorf("listen for rclone authorization relay: %w", err)
	}
	relay := &rcloneAuthRelay{
		listener: listener,
		server: &http.Server{
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       15 * time.Second,
			MaxHeaderBytes:    64 << 10,
		},
	}
	go func() {
		if serveErr := relay.server.Serve(listener); serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			_ = listener.Close()
		}
	}()
	return relay, nil
}

func callbackHostAllowed(host string) bool {
	name, port, err := net.SplitHostPort(host)
	return err == nil && port == strconv.Itoa(rcloneAuthCallbackPort) &&
		(name == "127.0.0.1" || name == "localhost")
}

func (relay *rcloneAuthRelay) Close() error {
	if relay == nil {
		return nil
	}
	relay.closeOnce.Do(func() {
		// Stop new callback admission immediately, but give an accepted native
		// response a short bounded interval to reach the browser before forcing
		// active connections closed.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		shutdownErr := relay.server.Shutdown(ctx)
		cancel()
		if shutdownErr != nil {
			shutdownErr = errors.Join(shutdownErr, relay.server.Close())
		}
		relay.closeErr = shutdownErr
		if err := relay.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			relay.closeErr = errors.Join(relay.closeErr, err)
		}
	})
	return relay.closeErr
}
