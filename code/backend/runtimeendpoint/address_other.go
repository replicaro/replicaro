//go:build !windows

package runtimeendpoint

import (
	"errors"
	"syscall"
)

func addressInUse(err error) bool {
	return errors.Is(err, syscall.EADDRINUSE)
}
