//go:build windows && amd64

package storagehelper

import (
	_ "embed"
)

//go:embed assets/windows_amd64/storage-helper.exe
var embeddedBinary []byte

//go:embed assets/windows_amd64/storage-helper.exe.sha256
var embeddedChecksum []byte

func readEmbeddedComponent() ([]byte, []byte, error) {
	return embeddedBinary, embeddedChecksum, nil
}

func executableName() string { return "storage-helper.exe" }
