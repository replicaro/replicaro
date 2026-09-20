//go:build windows && amd64

package rclone

import (
	"embed"
)

//go:embed assets/rclone.exe
var embeddedAssets embed.FS

func embeddedBinary() ([]byte, error) {
	return embeddedAssets.ReadFile("assets/rclone.exe")
}

func embeddedBinaryName() string { return "rclone.exe" }
func embeddedBinarySHA256() string {
	return "033eee51c9ad47c2de2624b6674d355274bcd6cf0027a5f85db4437ba24ae81c"
}
