//go:build replicaro_embedded_ui

package api

import (
	"embed"
	"io/fs"
)

// Canonical platform builds populate this target-owned virtual directory
// through the same Go overlay that supplies the embedded storage helper.
//
//go:embed webui_dist
var embeddedWebUI embed.FS

func packagedWebUI() (fs.FS, bool, error) {
	webUI, err := fs.Sub(embeddedWebUI, "webui_dist")
	return webUI, true, err
}
