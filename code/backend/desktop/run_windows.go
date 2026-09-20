//go:build windows

package desktop

import (
	"os/exec"
	"runtime"
	"sync"
	"syscall"

	"golang.org/x/sys/windows"
)

var (
	activeTrayMu sync.RWMutex
	activeTray   *nativeTray
)

func Run(service func() error) error {
	tray, err := newNativeTray()
	if err != nil {
		return err
	}
	defer runtime.UnlockOSThread()
	defer uninitializeWindowsToast()
	defer tray.close()
	activeTrayMu.Lock()
	activeTray = tray
	activeTrayMu.Unlock()
	defer func() {
		activeTrayMu.Lock()
		if activeTray == tray {
			activeTray = nil
		}
		activeTrayMu.Unlock()
	}()

	serviceDone := make(chan error, 1)
	go func() {
		serviceDone <- service()
		tray.requestClose()
	}()
	_ = openWebUI()

	loopErr := tray.runMessageLoop()

	select {
	case err := <-serviceDone:
		return err
	default:
		return loopErr
	}
}

func openWebUI() error {
	return openWebUIURL(currentWebUIURL())
}

func openWebUIURL(target string) error {
	command := exec.Command("rundll32", "url.dll,FileProtocolHandler", target)
	command.SysProcAttr = &syscall.SysProcAttr{
		HideWindow:    true,
		CreationFlags: windows.CREATE_NO_WINDOW,
	}
	return command.Start()
}

func ShowNotification(title string, message string, operationID string, status string) error {
	return showNotification(title, message, operationID, status, false)
}

func ShowPersistentNotification(title string, message string, operationID string, successful bool) error {
	status := notificationStatusFromSuccess(successful)
	return showNotification(title, message, operationID, status, true)
}

func showNotification(title string, message string, operationID string, status string, persistent bool) error {
	activeTrayMu.RLock()
	tray := activeTray
	activeTrayMu.RUnlock()
	if tray == nil {
		return nil
	}
	result := make(chan error, 1)
	if err := tray.enqueueNotification(nativeNotification{
		title:       title,
		message:     message,
		operationID: operationID,
		status:      status,
		persistent:  persistent,
		result:      result,
	}); err != nil {
		return err
	}
	return <-result
}

func showToastNotification(notification nativeNotification) error {
	target := TimelineURL(notification.operationID)
	return showWindowsToast(notification.title, notification.message, target, notification.status, notification.persistent)
}

func MinimizeToTray() error {
	return nil
}

func RestoreWindow() error {
	return openWebUI()
}

func Capabilities() map[string]bool {
	return map[string]bool{"startAtLogin": true, "nativeNotifications": true, "tray": true, "menuBar": false}
}
