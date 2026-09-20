//go:build !windows

package storageidentity

import "os/exec"

func configureStorageHelperCommand(*exec.Cmd) {}
