//go:build darwin && arm64

package engines

import "embed"

const targetAssetPath = "darwin_arm64"

//go:embed assets/darwin_arm64/restic assets/darwin_arm64/kopia
var darwinARM64Assets embed.FS

func init() { embeddedUnixBinaries = darwinARM64Assets }
