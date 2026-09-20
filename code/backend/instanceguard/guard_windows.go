//go:build windows

package instanceguard

import (
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ErrAlreadyRunning = fmt.Errorf("another Replicaro instance is already running")

type Guard struct {
	handle windows.Handle
	once   sync.Once
	err    error
}

func Acquire(_ string) (*Guard, error) {
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, fmt.Errorf("open current-user token: %w", err)
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, fmt.Errorf("resolve current-user SID: %w", err)
	}
	sid := user.User.Sid.String()
	name, err := windows.UTF16PtrFromString("Global\\Replicaro-0.12-" + sid)
	if err != nil {
		return nil, err
	}
	descriptor, err := windows.SecurityDescriptorFromString(
		"D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;" + sid + ")",
	)
	if err != nil {
		return nil, fmt.Errorf("build current-user mutex security: %w", err)
	}
	attributes := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: descriptor,
	}
	handle, err := windows.CreateMutex(attributes, false, name)
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		_ = windows.CloseHandle(handle)
		return nil, ErrAlreadyRunning
	}
	if err != nil {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, err
	}
	return &Guard{handle: handle}, nil
}
func (g *Guard) Close() error {
	if g == nil {
		return nil
	}
	g.once.Do(func() { g.err = windows.CloseHandle(g.handle); g.handle = 0 })
	return g.err
}
