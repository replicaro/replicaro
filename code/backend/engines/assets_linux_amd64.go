//go:build linux && amd64

package engines

import "embed"

const targetAssetPath = "linux_amd64"

//go:embed assets/linux_amd64/restic assets/linux_amd64/kopia
var linuxAMD64Assets embed.FS

func init() { embeddedUnixBinaries = linuxAMD64Assets }
