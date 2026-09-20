//go:build windows

package engines

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const fileAttributeReparsePoint = 0x400

func isReparsePoint(info os.FileInfo) bool {
	data, ok := info.Sys().(*syscall.Win32FileAttributeData)
	return ok && data.FileAttributes&fileAttributeReparsePoint != 0
}

func runtimeDestinationIsDriveRelative(path string) bool {
	return len(path) > 1 && path[1] == ':' && !filepath.IsAbs(path) && !strings.HasPrefix(path, `\\`)
}
