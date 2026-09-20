//go:build darwin && amd64

package engines

import "embed"

const targetAssetPath = "darwin_amd64"

//go:embed assets/darwin_amd64/restic assets/darwin_amd64/kopia
var darwinAMD64Assets embed.FS

func init() { embeddedUnixBinaries = darwinAMD64Assets }
