//go:build linux && arm64

package rclone

import "embed"

const embeddedTarget = "linux_arm64"

//go:embed assets/linux_arm64/rclone
var targetAssets embed.FS

func init() { embeddedUnixAssets = targetAssets }
func embeddedBinarySHA256() string {
	return "d7ecfc17726b34f95c5f7ea9470862c2aef53dce1bb0677b83142ca38503f489"
}
