//go:build !linux

package command

import "os/exec"

func (tree *processTree) collectAdoptedDescendants(_ *exec.Cmd) error { return nil }
