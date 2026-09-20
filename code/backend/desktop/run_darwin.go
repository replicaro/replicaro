//go:build darwin && cgo

package desktop

/*
#cgo LDFLAGS: -framework AppKit -framework Foundation -framework UserNotifications
#include <stdlib.h>
#include "native_darwin.h"
*/
import "C"

import (
	"errors"
	"net/url"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var (
	activeDarwinMu sync.RWMutex
	activeDarwin   *darwinDesktop
)

type darwinDesktop struct {
	quit     chan struct{}
	quitOnce sync.Once
}

// AppKit must run on the process's primordial thread. Package initialization
// and main run on the initial Go goroutine, so pin it before startup work can
// move that goroutine to another OS thread.
func init() { runtime.LockOSThread() }

func Run(service func() error) error {
	if C.ReplicaroIsMainThread() == 0 {
		return errors.New("macOS desktop must run on the process main thread")
	}

	desktop := &darwinDesktop{quit: make(chan struct{})}
	defer desktop.requestQuit()
	activeDarwinMu.Lock()
	activeDarwin = desktop
	activeDarwinMu.Unlock()
	defer func() {
		activeDarwinMu.Lock()
		if activeDarwin == desktop {
			activeDarwin = nil
		}
		activeDarwinMu.Unlock()
	}()

	serviceDone := make(chan error, 1)
	go func() {
		serviceDone <- service()
		C.ReplicaroStopApp()
	}()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		select {
		case <-signals:
			desktop.requestQuit()
			C.ReplicaroStopApp()
		case <-desktop.quit:
		}
	}()

	C.ReplicaroRunApp()
	select {
	case err := <-serviceDone:
		return err
	case <-desktop.quit:
		return nil
	default:
		return nil
	}
}

func (desktop *darwinDesktop) requestQuit() {
	desktop.quitOnce.Do(func() { close(desktop.quit) })
}

//export replicaroOpenRequested
func replicaroOpenRequested() {
	_ = openWebUIURL(currentWebUIURL())
}

//export replicaroQuitRequested
func replicaroQuitRequested() {
	activeDarwinMu.RLock()
	desktop := activeDarwin
	activeDarwinMu.RUnlock()
	if desktop != nil {
		desktop.requestQuit()
	}
}

func openWebUIURL(target string) error {
	if _, err := url.ParseRequestURI(target); err != nil {
		return err
	}
	value := C.CString(target)
	defer C.free(unsafe.Pointer(value))
	if C.ReplicaroOpenURL(value) == 0 {
		return errors.New("macOS could not open the Replicaro web interface")
	}
	return nil
}

func ShowNotification(title, message, operationID string, status string) error {
	target := TimelineURL(operationID)
	iconPath, err := notificationIconPath(status)
	if err != nil {
		return err
	}
	cIcon := C.CString(iconPath)
	defer C.free(unsafe.Pointer(cIcon))
	cTitle, cMessage, cTarget := C.CString(title), C.CString(message), C.CString(target)
	defer C.free(unsafe.Pointer(cTitle))
	defer C.free(unsafe.Pointer(cMessage))
	defer C.free(unsafe.Pointer(cTarget))
	if C.ReplicaroShowNotification(cTitle, cMessage, cTarget, cIcon) == 0 {
		return errors.New("macOS notification permission is denied or unavailable")
	}
	return nil
}

// macOS notification persistence follows the user's Banner or Alert setting.
func ShowPersistentNotification(title, message, operationID string, successful bool) error {
	return ShowNotification(title, message, operationID, notificationStatusFromSuccess(successful))
}

func MinimizeToTray() error { return nil }
func RestoreWindow() error  { return openWebUIURL(currentWebUIURL()) }

func Capabilities() map[string]bool {
	_, homeErr := os.UserHomeDir()
	return map[string]bool{"startAtLogin": homeErr == nil, "nativeNotifications": C.ReplicaroNotificationCapability() != 0, "tray": false, "menuBar": true}
}
