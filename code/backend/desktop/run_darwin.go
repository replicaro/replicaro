//go:build darwin && cgo

package desktop

/*
#cgo LDFLAGS: -framework AppKit -framework Foundation -framework UserNotifications
#include <stdlib.h>
#include "native_darwin.h"
*/
import "C"

import (
	"context"
	"errors"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

var (
	activeDarwinMu sync.RWMutex
	activeDarwin   *darwinDesktop
)

type darwinDesktop struct {
	quit     chan struct{}
	quitOnce sync.Once
	// stop is the application's staged shutdown; see
	// replicaroTerminateRequested.
	stop func() error
}

// AppKit must run on the process's primordial thread. Package initialization
// and main run on the initial Go goroutine, so pin it before startup work can
// move that goroutine to another OS thread.
func init() { runtime.LockOSThread() }

// Run runs AppKit on the main thread and serves until the service stops or
// the process gets SIGINT or SIGTERM; then it returns, and the caller runs the
// staged shutdown. Quit in the menu bar menu, logging out, restarting and
// shutting down go through AppKit's terminate request instead: Run calls
// stop itself for those, and AppKit exits the process once stop has returned
// or after at most 15 seconds, so Run doesn't return then. stop must be safe
// to call twice.
func Run(service func() error, stop func() error) error {
	if C.ReplicaroIsMainThread() == 0 {
		return errors.New("macOS desktop must run on the process main thread")
	}

	desktop := &darwinDesktop{quit: make(chan struct{}), stop: stop}
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

	// The menu-bar item is created once AppKit has finished launching; the
	// state shown before then is kept and applied to it (see
	// ReplicaroSetStatusState in native_darwin.m).
	defer startTrayState(showDarwinTrayState)()

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

// openWebUI opens the web interface for the menu-bar Open item, a reopen of
// the running app, and the launch. It's a variable so tests can check which
// of them open it without a browser.
var openWebUI = func() error { return openWebUIURL(currentWebUIURL()) }

// replicaroOpenRequested is called on the main thread for the menu-bar Open
// item and when the running app is opened again from Finder, Launchpad or
// Spotlight. Both are the user asking for the web interface, so they open it
// even with --no-startup-browser.
//
//export replicaroOpenRequested
func replicaroOpenRequested() {
	_ = openWebUI()
}

// replicaroLaunchOpenRequested is called on the main thread once the app has
// finished launching. --no-startup-browser skips only this open, which is why
// it has its own entry point: gating replicaroOpenRequested instead would also
// stop the menu-bar Open item and a reopen from working.
//
//export replicaroLaunchOpenRequested
func replicaroLaunchOpenRequested() {
	if !NoStartupBrowser() {
		_ = openWebUI()
	}
}

// replicaroTerminateRequested is called on the main thread when AppKit asks
// to terminate: Quit in the menu bar menu, logging out, restarting, shutting
// down, or a quit Apple event. applicationShouldTerminate: in native_darwin.m
// has answered NSTerminateLater, so AppKit keeps running on the main thread
// and waits for ReplicaroReplyToTerminate. The staged shutdown runs on its
// own goroutine so the main thread can go straight back to AppKit; blocking
// it would freeze the menu bar and keep queued main-thread work from running.
// Most of the shutdown has a 10-second deadline, but not every step takes a
// context (the rclone sign-in cleanup doesn't), so the reply is sent after at
// most terminateReplyLimit even if the shutdown is still running. Without a
// reply AppKit would wait forever and hold up the logout, restart or shutdown.
// AppKit exits the process after the reply without returning from Run, so
// this is where a shutdown error is logged.
//
//export replicaroTerminateRequested
func replicaroTerminateRequested() {
	activeDarwinMu.RLock()
	desktop := activeDarwin
	activeDarwinMu.RUnlock()
	go func() {
		if desktop != nil && desktop.stop != nil {
			finished := finishWithin(func() error {
				err := desktop.stop()
				if err != nil {
					log.Print(err)
				}
				return err
			}, terminateReplyLimit)
			if !finished {
				log.Printf("Replicaro is exiting before its shutdown finished, after waiting %s", terminateReplyLimit)
			}
		}
		C.ReplicaroReplyToTerminate()
	}()
}

// replicaroConfirmQuit runs on the main thread when Quit is chosen in the menu
// bar menu, and reports whether the user confirmed it. The prompt reads the
// saved language each time it opens, once for all three texts (see SetText).
//
//export replicaroConfirmQuit
func replicaroConfirmQuit() C.int {
	texts := desktopTexts(TextQuitPrompt, TextQuitButton, TextCancelButton)
	text := func(value string) *C.char {
		return C.CString(strings.ToValidUTF8(strings.ReplaceAll(value, "\x00", ""), "\uFFFD"))
	}
	question, quit, cancel := text(texts[0]), text(texts[1]), text(texts[2])
	defer C.free(unsafe.Pointer(question))
	defer C.free(unsafe.Pointer(quit))
	defer C.free(unsafe.Pointer(cancel))
	return C.ReplicaroConfirmQuit(question, quit, cancel)
}

// replicaroTrayLabel returns the Open (0) or Quit (1) menu label as a C string
// the caller frees.
//
//export replicaroTrayLabel
func replicaroTrayLabel(quit C.int) *C.char {
	open, quitLabel := trayLabels()
	label := open
	if quit != 0 {
		label = quitLabel
	}
	return C.CString(strings.ToValidUTF8(strings.ReplaceAll(label, "\x00", ""), "\uFFFD"))
}

// replicaroTrayStateText returns the tooltip of a tray state (see trayState)
// as a C string the caller frees.
//
//export replicaroTrayStateText
func replicaroTrayStateText(state C.int) *C.char {
	text := trayStateText(trayState(state))
	return C.CString(strings.ToValidUTF8(strings.ReplaceAll(text, "\x00", ""), "\uFFFD"))
}

// showDarwinTrayState hands state and, unless it is idle, the recoloured icon
// to the main thread, which changes the menu-bar item. It returns at once.
func showDarwinTrayState(state trayState) {
	var image unsafe.Pointer
	var length C.int
	if tint, ok := trayStateTint(state); ok {
		tinted, err := tintPNG(notificationSourceIcon, tint)
		if err != nil {
			log.Printf("Replicaro could not make its menu-bar icon for the new state and keeps the current one: %v", err)
			return
		}
		image = C.CBytes(tinted)
		defer C.free(image)
		length = C.int(len(tinted))
	}
	C.ReplicaroSetStatusState(C.int(state), image, length)
}

// terminateReplyLimit is a little longer than the staged shutdown's
// 10-second deadline, so a shutdown that keeps to it always finishes before
// the reply.
const terminateReplyLimit = 15 * time.Second

// OpenWebUI opens a running instance's web interface from a second launch.
// openWebUIURL below queues NSWorkspace on the main dispatch queue, which
// only drains while Run has the AppKit loop going, and a second launch exits
// without ever starting it. /usr/bin/open asks Launch Services to open the
// URL in the default browser, which is what NSWorkspace does too, and returns
// once the request is handed over.
//
// Double-clicking the .app while Replicaro runs doesn't get here: Launch
// Services sends the running app a reopen event instead of starting a second
// process, and the running app opens the web interface itself (see
// applicationShouldHandleReopen: in native_darwin.m). This path is for a
// second process started some other way, such as from a terminal, a login
// item or open -n.
//
// open normally returns within a second. The timeout only keeps a wedged
// Launch Services from leaving this process hanging around; the second launch
// then reports the error and exits.
func OpenWebUI(target string) error {
	if _, err := url.ParseRequestURI(target); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return exec.CommandContext(ctx, "/usr/bin/open", target).Run()
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

// ShowStartupFailure shows why Replicaro couldn't start in an alert and
// returns once the user dismisses it. launchd discards the stderr of an app
// started from Finder or as a login item, so the alert is the only place a
// user sees the reason. AppKit must run on the main thread; main calls this
// from there before desktop.Run has started the app, so the alert gets a
// fresh NSApplication.
func ShowStartupFailure(message string) {
	if C.ReplicaroIsMainThread() == 0 {
		return
	}
	// A C string ends at the first NUL, and NSString rejects invalid UTF-8.
	message = strings.ToValidUTF8(strings.ReplaceAll(message, "\x00", ""), "\uFFFD")
	value := C.CString(message)
	defer C.free(unsafe.Pointer(value))
	C.ReplicaroShowStartupFailure(value)
}

// ConfirmStartupRepair asks question in an alert before Replicaro starts, with
// the buttons "Yes, repair Replicaro" and "No, don't repair". It reports
// whether the alert was answered and whether the answer was to repair. Like
// ShowStartupFailure it needs the main thread, before desktop.Run.
func ConfirmStartupRepair(question string) (answered, repair bool) {
	if C.ReplicaroIsMainThread() == 0 {
		return false, false
	}
	question = strings.ToValidUTF8(strings.ReplaceAll(question, "\x00", ""), "\uFFFD")
	cQuestion := C.CString(question)
	defer C.free(unsafe.Pointer(cQuestion))
	cRepair, cDecline := C.CString("Yes, repair Replicaro"), C.CString("No, don't repair")
	defer C.free(unsafe.Pointer(cRepair))
	defer C.free(unsafe.Pointer(cDecline))
	switch C.ReplicaroConfirmStartupRepair(cQuestion, cRepair, cDecline) {
	case 1:
		return true, true
	case 0:
		return true, false
	}
	return false, false
}

// macOS notification persistence follows the user's Banner or Alert setting.
func ShowPersistentNotification(title, message, operationID string, successful bool) error {
	return ShowNotification(title, message, operationID, notificationStatusFromSuccess(successful))
}

func Capabilities() map[string]bool {
	_, homeErr := os.UserHomeDir()
	return map[string]bool{"startAtLogin": homeErr == nil, "nativeNotifications": C.ReplicaroNotificationCapability() != 0, "tray": false, "menuBar": true}
}
