//go:build (linux || darwin) && (amd64 || arm64)

package rclone

import "embed"

var embeddedUnixAssets embed.FS

func embeddedBinary() ([]byte, error) {
	return embeddedUnixAssets.ReadFile("assets/" + embeddedTarget + "/rclone")
}
func embeddedBinaryName() string { return "rclone" }
