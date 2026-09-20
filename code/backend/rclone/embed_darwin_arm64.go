//go:build darwin && arm64

package rclone

import "embed"

const embeddedTarget = "darwin_arm64"

//go:embed assets/darwin_arm64/rclone
var targetAssets embed.FS

func init() { embeddedUnixAssets = targetAssets }
func embeddedBinarySHA256() string {
	return "bfe4a146980f07de56e7dc0115cf562aaad5c81fd10ef9b0f36f777eba0ab031"
}
