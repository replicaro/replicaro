package notifications

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// MaxURLs is how many notification URLs the settings accept.
const MaxURLs = 10

// deliveryTimeout is the budget for one notification across all of its URLs.
// The URLs are sent in parallel under one context, so a notification takes
// about this long at most however many URLs there are. It's the only time
// limit on delivery; see webhookHTTPClient.
const deliveryTimeout = 10 * time.Second

// Error texts are fixed on purpose. Notification URLs carry tokens and
// passwords in their user info, path and query, and net/http and url errors
// quote the whole request URL, so no error from those packages is ever passed
// on. Every error names the URL by its position and type only, plus an HTTP
// status or the step that failed. These errors end up in the operation log,
// the app log and toasts.
var (
	errRedirect       = errors.New("redirect is not permitted")
	errTimedOut       = errors.New("timed out")
	errDeliveryFailed = errors.New("delivery failed")
	errStopped        = errors.New("delivery stopped")
)

// sender is one parsed notification URL. send gets the prepared event (see
// prepare) and returns an error built only from the fixed texts above,
// statusError and short step names.
type sender interface {
	send(ctx context.Context, event Event) error
}

// CleanURLs trims each entry and drops the empty ones, the way the settings
// box treats blank lines. The result is never nil, so an empty list stores and
// encodes as [] rather than null.
func CleanURLs(lines []string) []string {
	cleaned := make([]string, 0, len(lines))
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			cleaned = append(cleaned, line)
		}
	}
	return cleaned
}

// CheckURLs cleans the list and checks that every entry is a supported
// notification URL. It returns the cleaned list, which is what gets stored.
func CheckURLs(lines []string) ([]string, error) {
	urls := CleanURLs(lines)
	if len(urls) > MaxURLs {
		return nil, fmt.Errorf("No more than %d notification URLs", MaxURLs)
	}
	for i, raw := range urls {
		if _, _, err := parseTarget(i+1, raw); err != nil {
			return nil, err
		}
	}
	return urls, nil
}

// unsupportedTypes are the other shoutrrr services. Their names are safe to
// repeat in an error. Any other scheme is reported without its name, because
// a URL typed without "scheme://" in front can make a token look like the
// scheme.
var unsupportedTypes = map[string]bool{
	"bark": true, "hangouts": true, "ifttt": true, "join": true, "logger": true,
	"opsgenie": true, "smtp": true, "signal": true, "wecom": true, "lark": true,
}

// parseTarget parses one URL and returns its type name ("webhook" for
// http(s)) and its sender. The error is already in its final form.
func parseTarget(index int, raw string) (string, sender, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || !strings.Contains(raw, "://") {
		return "", nil, fmt.Errorf("Notification URL %d: not a valid URL", index)
	}
	kind := u.Scheme
	var s sender
	switch u.Scheme {
	case "http", "https":
		kind = "webhook"
		s, err = parseWebhook(u, raw)
	case "ntfy":
		s, err = parseNtfy(u)
	case "gotify":
		s, err = parseGotify(u)
	case "discord":
		s, err = parseDiscord(u)
	case "slack":
		s, err = parseSlack(u)
	case "telegram":
		s, err = parseTelegram(u)
	case "pushover":
		s, err = parsePushover(u)
	case "pushbullet":
		s, err = parsePushbullet(u)
	case "matrix":
		s, err = parseMatrix(u)
	case "mattermost":
		s, err = parseMattermost(u)
	case "googlechat":
		s, err = parseGoogleChat(u)
	case "rocketchat":
		s, err = parseRocketChat(u)
	case "zulip":
		s, err = parseZulip(u)
	case "teams":
		s, err = parseTeams(u)
	case "generic", "generic+http", "generic+https":
		kind = "generic"
		s, err = parseGeneric(u)
	default:
		if unsupportedTypes[u.Scheme] {
			return "", nil, fmt.Errorf("Notification URL %d: unsupported type %q", index, u.Scheme)
		}
		return "", nil, fmt.Errorf("Notification URL %d: unsupported type", index)
	}
	if err != nil {
		return kind, nil, labelled(index, kind, err)
	}
	return kind, s, nil
}

// delivery is the outcome for one URL of a notification.
type delivery struct {
	kind string
	err  error
}

// deliver sends event to every URL in parallel and returns one outcome per
// URL, in list order. A URL that fails, or no longer parses, never stops the
// others. There are no retries: a notification is either delivered within the
// budget or reported as failed.
//
// The budget is applied here, because every send passes through: the URLs get
// deliveryTimeout, or less if ctx ends sooner.
func deliver(ctx context.Context, urls []string, event Event) []delivery {
	ctx, cancel := context.WithTimeout(ctx, deliveryTimeout)
	defer cancel()
	results := make([]delivery, len(urls))
	var wg sync.WaitGroup
	for i, raw := range urls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			kind, s, err := parseTarget(i+1, raw)
			if err == nil {
				if sendErr := s.send(ctx, event); sendErr != nil {
					err = labelled(i+1, kind, sendErr)
				}
			}
			results[i] = delivery{kind: kind, err: err}
		}()
	}
	wg.Wait()
	// net/http keeps dialing (connect, proxy CONNECT, TLS handshake) after
	// the request that started the dial has given up, so the connection can
	// be reused. With no per-step timeouts on the transport, a dial to a host
	// that never answers would then hang until the peer or the OS gives up,
	// holding a socket and goroutines. Nothing waits for those dials once the
	// notification is done, and this cancels them (and drops idle
	// connections). It is safe while other notifications are still sending:
	// net/http only cancels dials no request is waiting for and never touches
	// a connection in use. The cost is that an overlapping notification may
	// have to reconnect for its next request instead of reusing an idle one.
	webhookHTTPClient.CloseIdleConnections()
	return results
}

// labelled puts "Notification URL n (type): " in front of err. A sender that
// reaches several chats or rooms returns errors.Join of one error per failed
// destination, and each of those gets its own prefixed line.
func labelled(index int, kind string, err error) error {
	parts := []error{err}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		parts = joined.Unwrap()
	}
	lines := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != nil {
			lines = append(lines, fmt.Sprintf("Notification URL %d (%s): %s", index, kind, part))
		}
	}
	return errors.New(strings.Join(lines, "\n"))
}

// TestResult is one URL's outcome of a test notification.
type TestResult struct {
	Index int    `json:"index"`
	Type  string `json:"type"`
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// SendTest sends the test notification to each URL of the unsaved list. It
// ignores the success/failure switches and creates no operation or log entry;
// the caller gets every URL's outcome. The returned error is a validation
// error meant for the user.
func SendTest(ctx context.Context, lines []string, locale string) ([]TestResult, error) {
	if len(CleanURLs(lines)) == 0 {
		return nil, errors.New("Add at least one notification URL")
	}
	urls, err := CheckURLs(lines)
	if err != nil {
		return nil, err
	}
	event := Event{Event: testEvent, Status: "success", Success: true, Locale: locale, MessageKey: "notifications.test.message"}
	prepare(&event)
	results := make([]TestResult, 0, len(urls))
	for i, outcome := range deliver(ctx, urls, event) {
		result := TestResult{Index: i + 1, Type: outcome.kind, OK: outcome.err == nil}
		if outcome.err != nil {
			result.Error = outcome.err.Error()
		}
		results = append(results, result)
	}
	return results, nil
}

// request is one HTTP call of a sender.
type request struct {
	method string
	url    string
	header http.Header
	body   []byte
	user   *url.Userinfo // sent as basic auth when set
}

// maxRedirects is how many redirects one request follows. The time budget
// alone isn't enough: a server that redirects to itself answers in
// microseconds, so a loop would make many thousands of requests before the
// deadline.
const maxRedirects = 10

// do sends req through the shared notification client (proxy from the
// environment, no time limit but ctx) and returns the start of the response
// body. A non-2xx answer is an error.
//
// Redirects that redirectTarget allows are followed here by sending the
// identical request again (same method, headers, body and credentials) to the
// new address, at most maxRedirects times.
func do(ctx context.Context, req request) ([]byte, error) {
	method := req.method
	if method == "" {
		method = http.MethodPost
	}
	target := req.url
	for redirects := 0; ; redirects++ {
		httpRequest, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(req.body))
		if err != nil {
			return nil, errDeliveryFailed
		}
		for name, values := range req.header {
			httpRequest.Header[name] = values
		}
		if req.user != nil {
			password, _ := req.user.Password()
			httpRequest.SetBasicAuth(req.user.Username(), password)
		}
		response, err := webhookHTTPClient.Do(httpRequest)
		if err != nil {
			return nil, deliveryError(ctx, err)
		}
		body, _ := io.ReadAll(io.LimitReader(response.Body, maxWebhookResponseBytes))
		response.Body.Close()
		switch response.StatusCode {
		case http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
			http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
			next, ok := redirectTarget(response, httpRequest.URL)
			if !ok || redirects == maxRedirects {
				return nil, errRedirect
			}
			target = next.String()
			continue
		}
		if response.StatusCode < 200 || response.StatusCode >= 300 {
			return body, statusError(response.StatusCode)
		}
		return body, nil
	}
}

// redirectTarget returns the address a redirect from "from" may be followed
// to. Only two cases are followed, both on the same host name:
//
//   - the plain upgrade from http:// to https:// with the same path and query,
//     for any redirect status (the port may change with it); and
//   - a 307 or 308 to any path on the same origin (same scheme and port, or the
//     same http-to-https upgrade).
//
// Keep it this narrow. The request carries the URL's tokens in its headers,
// body and basic auth, so following to another host would hand them to
// whoever the redirect names, and a service on the internet could bounce the
// request onto an address in the local network. A 301, 302 or 303 to a new
// address usually points at a different resource that expects a GET
// (post/redirect/get), so posting the notification there again would be a
// guess. The exact https upgrade is clearly the same resource, which is why
// any status is followed for it, and 307 and 308 promise that the same method
// and body are wanted at the new address. A Location with its own user info
// is refused, so a redirect can't add or change credentials.
func redirectTarget(response *http.Response, from *url.URL) (*url.URL, bool) {
	location, err := url.Parse(response.Header.Get("Location"))
	if err != nil || location.String() == "" || location.User != nil {
		return nil, false
	}
	to := from.ResolveReference(location)
	if !sameHostName(to.Hostname(), from.Hostname()) {
		return nil, false
	}
	upgrade := from.Scheme == "http" && to.Scheme == "https"
	samePath := requestPath(to) == requestPath(from) && to.RawQuery == from.RawQuery
	temporaryOrPermanent := response.StatusCode == http.StatusTemporaryRedirect ||
		response.StatusCode == http.StatusPermanentRedirect
	switch {
	case upgrade && samePath:
	case temporaryOrPermanent && upgrade:
	case temporaryOrPermanent && to.Scheme == from.Scheme && port(to) == port(from):
	default:
		return nil, false
	}
	// The next hop sends the same basic auth as this one, whether it came
	// from the URL's user info or not. An absolute Location would otherwise
	// drop the user info of the configured URL; one with user info of its own
	// was refused above.
	to.User = from.User
	return to, true
}

// sameHostName compares host names the way DNS does: ASCII letters ignore
// case, everything else must match exactly. Don't use strings.EqualFold here.
// Its Unicode folding treats names such as "straße" and "straẞe", or "ς" and
// "σ", as equal, but IDNA turns them into different domains, so the request
// and its credentials would go to another host.
func sameHostName(a, b string) bool {
	return a == b || isASCII(a) && isASCII(b) && strings.EqualFold(a, b)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// requestPath is the path as sent on the request line, where an empty path
// is "/".
func requestPath(u *url.URL) string {
	if path := u.EscapedPath(); path != "" {
		return path
	}
	return "/"
}

// port is the URL's port, or the scheme's default one.
func port(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

// deliveryError maps a client error to one of the fixed texts.
func deliveryError(ctx context.Context, err error) error {
	var netErr net.Error
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return errStopped
	case ctx.Err() != nil, errors.As(err, &netErr) && netErr.Timeout():
		return errTimedOut
	default:
		return errDeliveryFailed
	}
}

// statusError is built from the status code alone; the server's own reason
// text isn't repeated.
func statusError(code int) error {
	return errors.New(strings.TrimSpace(fmt.Sprintf("HTTP %d %s", code, http.StatusText(code))))
}

// stepError names the step of a multi-step sender that failed, keeping the
// HTTP status or the delivery error in brackets: "login failed (HTTP 403
// Forbidden)".
func stepError(step string, err error) error {
	return fmt.Errorf("%s failed (%s)", step, err)
}

// queryValues parses a URL's query. url.Query() silently drops any part it
// can't parse (a raw ";" or a bad %-escape), so an option could vanish
// without an error; for example ntfy's actions=view,Open,https://a;http,...
// would save fine and send no actions. Like shoutrrr, escape ";" first so it
// stays part of the value, and refuse a query that still doesn't parse. The
// error doesn't quote the query, which can hold tokens.
func queryValues(u *url.URL) (url.Values, error) {
	values, err := url.ParseQuery(strings.ReplaceAll(u.RawQuery, ";", "%3B"))
	if err != nil {
		return nil, errors.New("invalid query")
	}
	return values, nil
}

// queryOptions is options over the URL's parsed query.
func queryOptions(u *url.URL, names ...string) (map[string]string, error) {
	query, err := queryValues(u)
	if err != nil {
		return nil, err
	}
	return options(query, names...)
}

// optionName is what an option name can look like: every real one is a few
// letters, sometimes with "_". Unknown names are only quoted back when they
// match, because a key that doesn't look like a name is often a pasted token
// or a piece of the address in the wrong place. Keep it this narrow; digits,
// "." or "-" would let real token formats through (ntfy's tk_ tokens,
// Pushover keys). The length limit matters too: the longest real name is 12
// characters, and Gotify app tokens are exactly 15 and can be all letters and
// "_", so don't raise it past 14.
var optionName = regexp.MustCompile(`^[A-Za-z_]{1,14}$`)

// options reads a URL's query as named options. Each entry of names is an
// option name, optionally followed by "|"-separated aliases; the first name
// is the one the option is stored under. Names match without regard to case,
// as in shoutrrr. An unknown option is refused rather than ignored, so a typo
// never looks like it took effect. Empty values count as not set.
func options(query url.Values, names ...string) (map[string]string, error) {
	canonical := make(map[string]string)
	for _, name := range names {
		parts := strings.Split(name, "|")
		for _, part := range parts {
			canonical[part] = parts[0]
		}
	}
	keys := make([]string, 0, len(query))
	for key := range query {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values := make(map[string]string)
	for _, key := range keys {
		name, ok := canonical[strings.ToLower(key)]
		if !ok {
			if optionName.MatchString(key) {
				return nil, fmt.Errorf("unknown option %q", key)
			}
			return nil, errors.New("unknown option")
		}
		if _, seen := values[name]; !seen && query[key][0] != "" {
			values[name] = query[key][0]
		}
	}
	return values, nil
}

// boolOption reads a yes/no option the way shoutrrr does; fallback is used
// when the option isn't set.
func boolOption(values map[string]string, name string, fallback bool) (bool, error) {
	value, ok := values[name]
	if !ok {
		return fallback, nil
	}
	switch strings.ToLower(value) {
	case "true", "1", "yes", "y":
		return true, nil
	case "false", "0", "no", "n":
		return false, nil
	}
	return false, invalidOption(name)
}

func invalidOption(name string) error {
	return fmt.Errorf("invalid value for option %q", name)
}

// splitList splits a comma-separated option and drops empty entries.
func splitList(value string) []string {
	var items []string
	for _, item := range strings.Split(value, ",") {
		if item = strings.TrimSpace(item); item != "" {
			items = append(items, item)
		}
	}
	return items
}

// cutRunes shortens text to at most limit characters, ending with "…" when it
// was cut. Don't split long messages into several: cutting keeps one
// notification per event on every service.
func cutRunes(text string, limit int) string {
	if utf8.RuneCountInString(text) <= limit {
		return text
	}
	runes := []rune(text)
	return string(runes[:limit-1]) + "…"
}

// cutBytes is cutRunes for services that limit bytes instead of characters.
// It never cuts inside a character.
func cutBytes(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	cut := limit - len("…")
	for cut > 0 && !utf8.RuneStart(text[cut]) {
		cut--
	}
	return text[:cut] + "…"
}

// titled is the plain-text form used by services without a separate title
// field: the title on the first line, then the message.
func titled(title, message string) string {
	return title + "\n" + message
}
