//go:build windows

package storageidentity

import (
	"os/exec"
	"syscall"

	"golang.org/x/sys/windows"
)

func configureStorageHelperCommand(command *exec.Cmd) {
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
}
