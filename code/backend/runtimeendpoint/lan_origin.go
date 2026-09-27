package runtimeendpoint

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

// LANOrigin is one browser origin, served by the user's own HTTPS reverse
// proxy, that may drive the local UI and API in addition to the loopback
// Endpoint. It is deliberately a separate type: the loopback Endpoint still
// owns the listener, rendezvous record, desktop links, and webhook links, and
// none of those ever point at a LAN origin.
//
// Replicaro itself still binds only loopback (or the private container bridge).
// A LAN origin changes nothing about what is reachable; it only lets requests
// that the proxy forwards pass the Host and Origin checks.
type LANOrigin struct {
	// origin is the canonical serialized origin, compared byte-for-byte with
	// the browser's Origin header, for example "https://backup.lan:8443".
	origin string
	// hostname is the lowercase host without brackets or port, compared with
	// the hostname part of the Host header, for example "backup.lan" or
	// "fd00::1".
	hostname string
}

// LANOrigins is the process-scoped list from repeated --lan-origin flags, in
// the order given. Duplicates are harmless and kept. The zero value (nil)
// matches nothing, so a process started without the flag behaves exactly as
// it did before LAN origins existed.
type LANOrigins []LANOrigin

// ParseLANOrigin validates one --lan-origin value and returns it in the form a
// browser serializes it, so later matching can be plain string comparison.
//
// HTTPS is mandatory. Browsers treat plain HTTP as a secure context only on
// loopback, and the UI depends on secure-context APIs (crypto.randomUUID for
// restore and Find File requests, navigator.clipboard for copy actions). Over
// http://<lan-host> those are undefined and the UI breaks in confusing ways,
// so an explicit non-HTTPS scheme is a startup error rather than a
// half-working origin.
//
// The https:// prefix may be omitted. The scheme is detected by looking for a
// literal "://" rather than by url.Parse, because url.Parse reads a bare
// "backup.lan:8443" as scheme "backup.lan" with opaque data "8443".
//
// Validation exists so that a value which could never equal what a browser
// sends (a Unicode hostname, a wildcard, a path) is reported at startup
// instead of silently never matching. Error messages never echo the value,
// because a mistyped value may contain user information such as a password.
func ParseLANOrigin(value string) (LANOrigin, error) {
	rest := value
	if scheme, afterScheme, found := strings.Cut(value, "://"); found {
		if !strings.EqualFold(scheme, "https") {
			return LANOrigin{}, errors.New("LAN origins must be HTTPS (https://); plain http:// and other schemes are not supported")
		}
		rest = afterScheme
	}
	if rest == "" {
		return LANOrigin{}, errors.New("LAN origin host is required")
	}
	if strings.ContainsAny(rest, "?#") {
		return LANOrigin{}, errors.New("LAN origin must not include a query or fragment")
	}
	parsed, err := url.Parse("https://" + rest)
	if err != nil {
		// url.Error quotes the whole input, so it is not wrapped here.
		return LANOrigin{}, errors.New("LAN origin must have the form [https://]<host>[:<port>], with an IPv6 address in [brackets]")
	}
	if parsed.User != nil {
		return LANOrigin{}, errors.New("LAN origin must not include a user name or password")
	}
	if (parsed.Path != "" && parsed.Path != "/") || parsed.RawPath != "" || parsed.Opaque != "" {
		return LANOrigin{}, errors.New("LAN origin must not include a path")
	}
	host, port := parsed.Hostname(), parsed.Port()
	if host == "" {
		return LANOrigin{}, errors.New("LAN origin host is required")
	}
	if strings.HasSuffix(parsed.Host, ":") {
		return LANOrigin{}, errors.New("LAN origin port must be a canonical decimal integer from 1 through 65535")
	}

	var hostname, serializedHost string
	if strings.HasPrefix(parsed.Host, "[") {
		// Browsers serialize IPv6 hosts in compressed lowercase hex, and netip
		// produces the same form for ordinary addresses, so [fd00:0::1] is
		// stored as [fd00::1]. Zones never appear in a browser Origin.
		address, addressErr := netip.ParseAddr(host)
		if addressErr != nil || !address.Is6() || address.Zone() != "" {
			return LANOrigin{}, errors.New("LAN origin IPv6 address is invalid")
		}
		// IPv4-mapped addresses are the exception: netip prints them dotted
		// (::ffff:192.168.1.20) while browsers print hex ([::ffff:c0a8:114]),
		// so the stored origin could never match. The plain IPv4 address
		// reaches the same machine and does match.
		if address.Is4In6() {
			return LANOrigin{}, errors.New("LAN origin must use the plain IPv4 address instead of an IPv4-mapped IPv6 address")
		}
		hostname = address.String()
		serializedHost = "[" + hostname + "]"
	} else if address, addressErr := netip.ParseAddr(host); addressErr == nil && address.Is4() {
		hostname = address.String()
		serializedHost = hostname
	} else {
		for index := 0; index < len(host); index++ {
			if host[index] >= 0x80 {
				return LANOrigin{}, errors.New("LAN origin hostname must be ASCII; use the punycode (xn--) form of an international name")
			}
		}
		if strings.Contains(host, "*") {
			return LANOrigin{}, errors.New("LAN origin hostname must not contain a wildcard (*); list each hostname exactly")
		}
		if strings.Contains(host, ":") {
			return LANOrigin{}, errors.New("LAN origin IPv6 addresses must be in [brackets]")
		}
		// A browser parses any host whose last label looks numeric as an IPv4
		// address and rewrites it (WHATWG URL host parsing): 192.168.010.6 is
		// read with octal as 192.168.8.6, 3232235777 becomes 192.168.1.1, and
		// 0x7f.0.0.1 becomes 127.0.0.1. Plain dotted-decimal was already taken
		// by the netip branch above, so anything numeric that reaches here
		// would be stored in a form the browser never sends. One trailing dot
		// is ignored first, as browsers do when making that decision.
		labels := strings.Split(strings.TrimSuffix(host, "."), ".")
		last := labels[len(labels)-1]
		if (last != "" && strings.Trim(last, "0123456789") == "") || strings.HasPrefix(last, "0x") || strings.HasPrefix(last, "0X") {
			return LANOrigin{}, errors.New("LAN origin IPv4 address must be in dotted-decimal form, for example 192.168.1.20")
		}
		hostname = strings.ToLower(host)
		serializedHost = hostname
	}

	if port != "" {
		number, portErr := strconv.Atoi(port)
		if portErr != nil || port != strconv.Itoa(number) || number < 1 || number > 65535 {
			return LANOrigin{}, errors.New("LAN origin port must be a canonical decimal integer from 1 through 65535")
		}
		if number == 443 {
			// Browsers omit the scheme's default port from Origin.
			port = ""
		}
	}
	origin := "https://" + serializedHost
	if port != "" {
		origin += ":" + port
	}
	return LANOrigin{origin: origin, hostname: hostname}, nil
}

func (origin LANOrigin) String() string { return origin.origin }

// HostAllowed reports whether the Host header names one of the configured LAN
// hostnames. Only the hostname is compared, case-insensitively; any port (or
// none) is accepted. Proxies differ in whether they forward the port: nginx's
// $host drops it, Caddy and Tailscale Serve keep the browser's. The port in the
// Host header also says nothing useful here, since Replicaro itself only
// listens on loopback or the private bridge. The Origin check stays exact
// (scheme, host, and port), and that is what catches a wrong port on writes.
//
// This matches only hostnames the user wrote out: no wildcard, suffix, or
// subdomain matching, and X-Forwarded-Host / Forwarded are never consulted
// because any client can set them.
func (origins LANOrigins) HostAllowed(host string) bool {
	if len(origins) == 0 {
		return false
	}
	name, ok := hostHeaderHostname(host)
	if !ok {
		return false
	}
	for _, origin := range origins {
		if strings.EqualFold(name, origin.hostname) {
			return true
		}
	}
	return false
}

// OriginAllowed reports whether origin is exactly one of the configured
// canonical LAN origins. Browsers serialize Origin canonically (lowercase
// host, compressed IPv6, default port omitted), so exact comparison is both
// the simplest and the strictest correct check.
func (origins LANOrigins) OriginAllowed(origin string) bool {
	for _, configured := range origins {
		if origin == configured.origin {
			return true
		}
	}
	return false
}

// hostHeaderHostname extracts the hostname from a Host header value of the
// form host, host:port, [ipv6], or [ipv6]:port. Brackets are required for an
// IPv6 literal and refused for anything else, so an unbracketed "fd00::1" or
// a bracketed name cannot be read two ways.
func hostHeaderHostname(host string) (string, bool) {
	name := host
	if splitName, port, err := net.SplitHostPort(host); err == nil {
		if port == "" || strings.Trim(port, "0123456789") != "" {
			return "", false
		}
		name = splitName
	} else if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		name = host[1 : len(host)-1]
	}
	if strings.HasPrefix(host, "[") != strings.Contains(name, ":") {
		return "", false
	}
	return name, name != ""
}
