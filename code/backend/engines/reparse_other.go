//go:build !windows

package engines

import "os"

func isReparsePoint(os.FileInfo) bool { return false }

func runtimeDestinationIsDriveRelative(string) bool { return false }
