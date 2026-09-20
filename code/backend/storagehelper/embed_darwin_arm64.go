//go:build darwin && arm64

package storagehelper

import (
	_ "embed"
)

//go:embed assets/darwin_arm64/storage-helper
var embeddedBinary []byte

//go:embed assets/darwin_arm64/storage-helper.sha256
var embeddedChecksum []byte

func readEmbeddedComponent() ([]byte, []byte, error) {
	return embeddedBinary, embeddedChecksum, nil
}

func executableName() string { return "storage-helper" }
