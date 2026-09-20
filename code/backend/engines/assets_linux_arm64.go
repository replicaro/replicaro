//go:build linux && arm64

package engines

import "embed"

const targetAssetPath = "linux_arm64"

//go:embed assets/linux_arm64/restic assets/linux_arm64/kopia
var linuxARM64Assets embed.FS

func init() { embeddedUnixBinaries = linuxARM64Assets }
