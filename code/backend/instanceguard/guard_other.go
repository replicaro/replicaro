//go:build !windows

package instanceguard

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var ErrAlreadyRunning = fmt.Errorf("another Replicaro instance is already running")

type Guard struct {
	file *os.File
	once sync.Once
	err  error
}

func Acquire(path string) (*Guard, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, ErrAlreadyRunning
		}
		return nil, err
	}
	return &Guard{file: file}, nil
}
func (g *Guard) Close() error {
	if g == nil || g.file == nil {
		return nil
	}
	g.once.Do(func() { _ = syscall.Flock(int(g.file.Fd()), syscall.LOCK_UN); g.err = g.file.Close() })
	return g.err
}
