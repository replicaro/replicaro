//go:build linux && amd64

package storagehelper

import (
	_ "embed"
)

//go:embed assets/linux_amd64/storage-helper
var embeddedBinary []byte

//go:embed assets/linux_amd64/storage-helper.sha256
var embeddedChecksum []byte

func readEmbeddedComponent() ([]byte, []byte, error) {
	return embeddedBinary, embeddedChecksum, nil
}

func executableName() string { return "storage-helper" }
