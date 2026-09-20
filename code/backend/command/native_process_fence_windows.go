//go:build windows

package command

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
)

var windowsNativeProcessFences = struct {
	sync.Mutex
	active map[string]bool
}{active: map[string]bool{}}

func windowsNativeProcessFenceKey(path string) (string, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return "", fmt.Errorf("canonicalize native operation fence: %w", err)
	}
	return strings.ToLower(filepath.Clean(absolute)), nil
}

func acquireNativeProcessFence(path string) (*NativeProcessFence, error) {
	// Every native command is assigned to a kill-on-close Job Object. Once the
	// single-instance guard admits a replacement Replicaro process, Windows has
	// closed the prior Job handle and terminated that command tree. Within the
	// current process, retain the exact operation identity until its command
	// scope closes so an API request cannot abandon still-running native work.
	key, err := windowsNativeProcessFenceKey(path)
	if err != nil {
		return nil, err
	}
	windowsNativeProcessFences.Lock()
	if windowsNativeProcessFences.active[key] {
		windowsNativeProcessFences.Unlock()
		return nil, fmt.Errorf("native operation fence is already active")
	}
	windowsNativeProcessFences.active[key] = true
	windowsNativeProcessFences.Unlock()
	var once sync.Once
	return &NativeProcessFence{
		supported: true, proofKind: "windows_job_kill_on_close_v1",
		release: func() error {
			once.Do(func() {
				windowsNativeProcessFences.Lock()
				delete(windowsNativeProcessFences.active, key)
				windowsNativeProcessFences.Unlock()
			})
			return nil
		},
	}, nil
}

func nativeProcessFenceInactive(path string) (bool, error) {
	key, err := windowsNativeProcessFenceKey(path)
	if err != nil {
		return false, err
	}
	windowsNativeProcessFences.Lock()
	active := windowsNativeProcessFences.active[key]
	windowsNativeProcessFences.Unlock()
	return !active, nil
}

func cleanupClosedNativeProcessFence(_ string) error { return nil }
