//go:build windows

package desktop

import (
	_ "embed"
	"net/url"
	"path/filepath"
)

const trayIconSize = 32

//go:embed assets/replicaro-favicon.ico
var replicaroFavicon []byte

func trayIcon() []byte { return replicaroFavicon }

func notificationIconURI(status string) (string, error) {
	path, err := notificationIconPath(status)
	if err != nil {
		return "", err
	}
	return (&url.URL{Scheme: "file", Path: "/" + filepath.ToSlash(path)}).String(), nil
}
