package desktop

import (
	"fmt"
	"net/url"
	"runtime"
	"sync"

	"github.com/local/replicaro/models"
	"github.com/local/replicaro/runtimeendpoint"
)

var endpointState = struct {
	sync.RWMutex
	endpoint   runtimeendpoint.Endpoint
	configured bool
}{endpoint: runtimeendpoint.Preferred()}

const replicaroUpdateURL = "https://replicaro.com/update"

var openDefaultBrowser = openWebUIURL

func ConfigureEndpoint(endpoint runtimeendpoint.Endpoint) error {
	if _, err := runtimeendpoint.ParseExact(endpoint.String()); err != nil {
		return err
	}
	endpointState.Lock()
	defer endpointState.Unlock()
	if endpointState.configured && endpointState.endpoint.String() != endpoint.String() {
		return fmt.Errorf("desktop runtime endpoint is already configured")
	}
	endpointState.endpoint = endpoint
	endpointState.configured = true
	return nil
}

func currentWebUIURL() string {
	endpointState.RLock()
	defer endpointState.RUnlock()
	return endpointState.endpoint.WebURL()
}

func TimelineURL(operationID string) string {
	target := currentWebUIURL()
	if operationID != "" {
		target += "?timelineOperation=" + url.QueryEscape(operationID)
	}
	return target
}

func ReplicaroReleaseURL(version string) (string, error) {
	return releaseURLForPlatform(version, runtime.GOOS, runtime.GOARCH)
}

func releaseURLForPlatform(version, operatingSystem, architecture string) (string, error) {
	if _, err := models.ParseSemanticVersion(version); err != nil {
		return "", err
	}
	if operatingSystem == "darwin" {
		operatingSystem = "macos"
	}
	return replicaroUpdateURL + "?os=" + url.QueryEscape(operatingSystem) + "&arch=" + url.QueryEscape(architecture), nil
}

func OpenReplicaroRelease(version string) error {
	target, err := ReplicaroReleaseURL(version)
	if err != nil {
		return err
	}
	return openDefaultBrowser(target)
}

func Initialize() error {
	return nil
}
