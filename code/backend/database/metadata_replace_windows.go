//go:build windows

package database

import "github.com/local/replicaro/appdata"

func replaceMetadataCacheFile(source, destination string) error {
	return appdata.AtomicReplace(source, destination)
}
