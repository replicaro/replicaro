package vaultidentity

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// A WebDAV vault is stored like an SFTP vault: Location holds
// "<scheme>://<host><path>" and a non-default port lives in the "port"
// connector option. The create and connect handlers store the canonical form
// returned by NormalizeWebDAVAddress, and ResolveEffectiveAddress runs the same
// parser, so every later reader (identity, Kopia, rclone, the vault card, the
// frontend's pending-connection matching) compares one spelling.
//
// The path is a plain folder name measured from the root of the server. It is
// deliberately never percent-decoded or encoded here: Kopia's WebDAV client
// encodes each segment itself (an already encoded "a%20b" fails there) and
// rclone accepts the raw form. Rejecting %, ? and # keeps that unencoded URL
// unambiguous, so do not "fix" this by escaping the path.

// webdavSchemes lists the accepted Server URL schemes. The dav/webdav spellings
// are common in client apps, so they are accepted and stored as the plain
// HTTP(S) scheme both engines and rclone understand.
var webdavSchemes = map[string]string{
	"https": "https", "http": "http",
	"webdavs": "https", "davs": "https",
	"webdav": "http", "dav": "http",
}

type webdavLocation struct {
	scheme string // "https" or "http"
	host   string // lowercase, without IPv6 brackets
	path   string // starts with "/"; the server root is "/"
}

// parseWebDAVLocation never includes the input in its errors: a user can paste
// a URL with a password in it, and these messages reach the UI and logs.
func parseWebDAVLocation(location string) (webdavLocation, error) {
	location = strings.TrimSpace(location)
	rawScheme, rest, found := strings.Cut(location, "://")
	scheme, known := webdavSchemes[strings.ToLower(rawScheme)]
	if !found || !known {
		return webdavLocation{}, fmt.Errorf("WebDAV server URL must start with https:// or http://")
	}
	authority, path := rest, ""
	if index := strings.IndexByte(rest, '/'); index >= 0 {
		authority, path = rest[:index], rest[index:]
	}
	host, err := webdavHost(strings.TrimSpace(authority))
	if err != nil {
		return webdavLocation{}, err
	}
	if path == "" {
		return webdavLocation{}, fmt.Errorf("WebDAV path is required; enter the folder that holds the vault, starting with /")
	}
	if strings.ContainsAny(path, "%?#") {
		return webdavLocation{}, fmt.Errorf("WebDAV paths cannot contain %%, ?, or #")
	}
	if err := validateRemoteNamespacePath(path); err != nil {
		return webdavLocation{}, err
	}
	// Drop the trailing slash, and any whitespace it was hiding (as in
	// "/dav /"), until neither is left. Parsing the saved value again must give
	// the same path; otherwise "/dav " would be saved while every later reader,
	// which trims again, used "/dav".
	for path != "/" {
		trimmed := strings.TrimRightFunc(strings.TrimSuffix(path, "/"), unicode.IsSpace)
		if trimmed == path {
			break
		}
		path = trimmed
	}
	// Trimming can turn a segment such as ".. " into "..", so check the result too.
	if err := validateRemoteNamespacePath(path); err != nil {
		return webdavLocation{}, err
	}
	return webdavLocation{scheme: scheme, host: host, path: path}, nil
}

func webdavHost(authority string) (string, error) {
	switch {
	case strings.Contains(authority, "@"):
		return "", fmt.Errorf("WebDAV server URL cannot contain a user name or password; enter them in the WebDAV username and WebDAV account password fields")
	case strings.ContainsAny(authority, "?#"):
		return "", fmt.Errorf("WebDAV server URL cannot contain a query or fragment")
	case authority == "":
		return "", fmt.Errorf("WebDAV server URL must include a host name")
	}
	invalid := fmt.Errorf("WebDAV server URL has an invalid host name")
	if strings.ContainsAny(authority, `%\`) || strings.IndexFunc(authority, unicode.IsControl) >= 0 {
		return "", invalid
	}
	if strings.HasPrefix(authority, "[") {
		end := strings.IndexByte(authority, ']')
		if end < 0 {
			return "", invalid
		}
		host, rest := authority[1:end], authority[end+1:]
		if strings.HasPrefix(rest, ":") {
			return "", fmt.Errorf("WebDAV server URL cannot contain a port; enter it in the Port field")
		}
		if rest != "" || !strings.Contains(host, ":") || net.ParseIP(host) == nil {
			return "", invalid
		}
		return strings.ToLower(host), nil
	}
	if strings.Contains(authority, ":") {
		// A bare IPv6 address is full of colons, so without this it would be
		// reported as a port.
		if net.ParseIP(authority) != nil {
			return "", fmt.Errorf("WebDAV server URL must put an IPv6 address in square brackets, for example https://[fd00::1]")
		}
		return "", fmt.Errorf("WebDAV server URL cannot contain a port; enter it in the Port field")
	}
	// Go's URL parser applies the usual host-character rules (no spaces and so
	// on). Its error text includes the input, so only its verdict is used.
	if parsed, err := url.Parse("http://" + authority); err != nil || parsed.Host != authority {
		return "", invalid
	}
	return strings.ToLower(authority), nil
}

// webdavPort returns the port to connect to and the value to store. The
// stored value is empty for the scheme's default, so an https vault entered
// with no port and one entered with port 443 are saved identically.
func webdavPort(scheme, port string) (effective, stored string, err error) {
	defaultPort := "443"
	if scheme == "http" {
		defaultPort = "80"
	}
	port = strings.TrimSpace(port)
	if port == "" {
		return defaultPort, "", nil
	}
	number, convErr := strconv.Atoi(port)
	if convErr != nil || number < 1 || number > 65535 {
		return "", "", fmt.Errorf("WebDAV port must be a number from 1 to 65535")
	}
	effective = strconv.Itoa(number)
	if effective == defaultPort {
		return effective, "", nil
	}
	return effective, effective, nil
}

func bracketedHost(host string) string {
	if strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// NormalizeWebDAVAddress returns the canonical Location and port option to
// store for a WebDAV vault. An empty port means the scheme's default, and the
// caller should then leave the option out.
func NormalizeWebDAVAddress(location, port string) (string, string, error) {
	parsed, err := parseWebDAVLocation(location)
	if err != nil {
		return "", "", err
	}
	_, stored, err := webdavPort(parsed.scheme, port)
	if err != nil {
		return "", "", err
	}
	return parsed.scheme + "://" + bracketedHost(parsed.host) + parsed.path, stored, nil
}

// resolveWebDAVAddress fills only the fields the identity key uses: host,
// port (default filled in, because two servers on one host can differ only by
// port) and path. The scheme goes in UseTLS, which is not part of the identity,
// and Username stays empty: two accounts can reach one shared folder, and
// changing the account must not make the vault a different one. Kopia and
// rclone take the WebDAV username from the connector options instead.
func resolveWebDAVAddress(location string, options map[string]string) (EffectiveAddress, error) {
	parsed, err := parseWebDAVLocation(location)
	if err != nil {
		return EffectiveAddress{}, err
	}
	port, _, err := webdavPort(parsed.scheme, options["port"])
	if err != nil {
		return EffectiveAddress{}, err
	}
	useTLS := "true"
	if parsed.scheme == "http" {
		useTLS = "false"
	}
	return EffectiveAddress{
		Connector: "webdav", Host: parsed.host, Port: port,
		Prefix: parsed.path, UseTLS: useTLS,
	}, nil
}

// WebDAVURL builds the one vault URL passed to Kopia (--url), to the rclone
// "base" remote, and compared with Kopia's status readback. The default port is
// left out and the path is written unencoded; see the note at the top of this
// file before changing either.
func WebDAVURL(address EffectiveAddress) (string, error) {
	scheme := ""
	switch address.UseTLS {
	case "true":
		scheme = "https"
	case "false":
		scheme = "http"
	}
	if address.Connector != "webdav" || scheme == "" || address.Host == "" ||
		address.Port == "" || !strings.HasPrefix(address.Prefix, "/") {
		return "", fmt.Errorf("WebDAV vault address is incomplete")
	}
	host := bracketedHost(address.Host)
	if port := normalizedPort(address.Port, address.UseTLS); port != "" {
		host += ":" + port
	}
	return scheme + "://" + host + address.Prefix, nil
}
