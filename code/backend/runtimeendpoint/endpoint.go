package runtimeendpoint

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
)

const (
	PreferredPort        = 9460
	PreferredBindAddress = "127.0.0.1:9460"
)

type Endpoint struct {
	port int
}

func Preferred() Endpoint { return Endpoint{port: PreferredPort} }

func FromPort(port int) (Endpoint, error) {
	if port < 1 || port > 65535 {
		return Endpoint{}, fmt.Errorf("loopback API port is invalid")
	}
	return Endpoint{port: port}, nil
}

func FromListener(listener net.Listener) (Endpoint, error) {
	if listener == nil {
		return Endpoint{}, fmt.Errorf("loopback API listener is required")
	}
	address, ok := listener.Addr().(*net.TCPAddr)
	if !ok || address.IP == nil || !address.IP.Equal(net.IPv4(127, 0, 0, 1)) {
		return Endpoint{}, fmt.Errorf("API listener is not exact IPv4 loopback")
	}
	return FromPort(address.Port)
}

func ParseExact(value string) (Endpoint, error) {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "http" || parsed.User != nil ||
		parsed.Hostname() != "127.0.0.1" || parsed.Port() == "" ||
		parsed.Path != "" || parsed.RawPath != "" || parsed.RawQuery != "" ||
		parsed.Fragment != "" {
		return Endpoint{}, fmt.Errorf("runtime endpoint is not exact IPv4 loopback")
	}
	port, err := strconv.Atoi(parsed.Port())
	if err != nil {
		return Endpoint{}, fmt.Errorf("runtime endpoint port is invalid")
	}
	endpoint, err := FromPort(port)
	if err != nil || endpoint.String() != value {
		return Endpoint{}, fmt.Errorf("runtime endpoint is not canonical")
	}
	return endpoint, nil
}

func Listen() (net.Listener, error) {
	return listenWith(net.Listen)
}

// ListenPort binds one exact IPv4 loopback port. Unlike Listen, it never
// falls back to another port when the requested endpoint is unavailable.
func ListenPort(port int) (net.Listener, error) {
	endpoint, err := FromPort(port)
	if err != nil {
		return nil, err
	}
	return net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(endpoint.Port()))
}

func listenWith(listen func(network, address string) (net.Listener, error)) (net.Listener, error) {
	listener, err := listen("tcp4", PreferredBindAddress)
	if err == nil {
		return listener, nil
	}
	if !addressInUse(err) {
		return nil, err
	}
	return listen("tcp4", "127.0.0.1:0")
}

func (endpoint Endpoint) Port() int { return endpoint.port }

func (endpoint Endpoint) String() string {
	return "http://127.0.0.1:" + strconv.Itoa(endpoint.port)
}

func (endpoint Endpoint) WebURL() string { return endpoint.String() + "/" }

func (endpoint Endpoint) HostAllowed(host string) bool {
	name, port, err := net.SplitHostPort(host)
	if err != nil || port != strconv.Itoa(endpoint.port) {
		return false
	}
	return name == "127.0.0.1" || name == "localhost"
}
