//go:build !windows

package command

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

func acquireNativeProcessFence(path string) (*NativeProcessFence, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &NativeProcessFence{file: file, supported: true, proofKind: "inherited_file_lock_v1"}, nil
}

func nativeProcessFenceInactive(path string) (bool, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0600)
	if err != nil {
		return false, err
	}
	defer file.Close()
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	return true, nil
}

func cleanupClosedNativeProcessFence(path string) error {
	return removeInactiveNativeProcessFence(path, nativeProcessFenceInactive)
}

// The settled check and the removal use the same observation. Checking once
// more right before removing would reopen the window the settle loop exists
// to tolerate.
func cleanupNativeProcessFenceAfterClose(path string) error {
	return removeInactiveNativeProcessFence(path, awaitClosedNativeProcessFence)
}

func removeInactiveNativeProcessFence(path string, inactiveCheck func(string) (bool, error)) error {
	inactive, err := inactiveCheck(path)
	if err != nil {
		return err
	}
	if !inactive {
		return errors.New("native operation fence is still active")
	}
	return os.Remove(path)
}
