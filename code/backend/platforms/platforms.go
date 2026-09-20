// Package platforms defines the release targets supported by Replicaro.
package platforms

import "runtime"

const (
	WindowsAMD64 = "windows_amd64"
	LinuxAMD64   = "linux_amd64"
	LinuxARM64   = "linux_arm64"
	DarwinAMD64  = "darwin_amd64"
	DarwinARM64  = "darwin_arm64"
)

var Supported = []string{WindowsAMD64, LinuxAMD64, LinuxARM64, DarwinAMD64, DarwinARM64}

func Current() string { return runtime.GOOS + "_" + runtime.GOARCH }

func IsSupported(target string) bool {
	for _, supported := range Supported {
		if target == supported {
			return true
		}
	}
	return false
}

func ExecutableName(name, target string) string {
	if target == WindowsAMD64 {
		return name + ".exe"
	}
	return name
}
