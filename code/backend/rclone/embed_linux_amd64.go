//go:build linux && amd64

package rclone

import "embed"

const embeddedTarget = "linux_amd64"

//go:embed assets/linux_amd64/rclone
var targetAssets embed.FS

func init() { embeddedUnixAssets = targetAssets }
func embeddedBinarySHA256() string {
	return "f66d8c1d552ad90296a11bc8b46d56a7fa5da1a7fa05e7ca522d95df92c4a4c0"
}
