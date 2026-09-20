//go:build darwin && amd64

package rclone

import "embed"

const embeddedTarget = "darwin_amd64"

//go:embed assets/darwin_amd64/rclone
var targetAssets embed.FS

func init() { embeddedUnixAssets = targetAssets }
func embeddedBinarySHA256() string {
	return "67286994be6b150c8f33d70dbafb4256897b008714bc39f3727585a598ec0d31"
}
