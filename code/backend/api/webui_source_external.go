//go:build !replicaro_embedded_ui

package api

import "io/fs"

func packagedWebUI() (fs.FS, bool, error) {
	return nil, false, nil
}
