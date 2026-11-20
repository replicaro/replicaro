//go:build windows

package desktop

import (
	"log"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"

	"golang.org/x/sys/windows"
)

var (
	activeTrayMu sync.RWMutex
	activeTray   *nativeTray
	// runningWithoutTray is set once Run has gone on without a tray. The API
	// already answers while Run is still setting the tray up, and the tray
	// usually comes up, so Capabilities only reports it missing after this.
	runningWithoutTray atomic.Bool
	// startNativeTray and openWebUI are variables so tests can run Run and
	// click the tray without a real tray or browser.
	startNativeTray = newNativeTray
	// openWebUI opens the web interface for the tray icon, its Open item, and
	// Run at startup.
	openWebUI = func() error { return openWebUIURL(currentWebUIURL()) }
)

// Run serves with the notification-area icon when Windows lets Replicaro set
// one up, and without it otherwise. The tray is a convenience: without it the
// web interface still works, and ending the app over it would leave a user
// with no console and no idea why Replicaro closed. Linux keeps serving
// without a tray the same way.
//
// stop is the application's staged shutdown, which the caller also runs
// after Run returns, so it must be safe to call twice. Run calls it itself
// only when Windows ends the session (see nativeTray.endSession), because
// Windows can end the process as soon as the tray window has answered
// WM_ENDSESSION.
func Run(service func() error, stop func() error) error {
	tray, err := startNativeTray()
	if err != nil {
		log.Printf("Replicaro is running without a notification-area icon: %v", err)
		runningWithoutTray.Store(true)
		return runWithoutTray(service)
	}
	tray.sessionEnd = stop
	// Windows ends a session's processes from the highest shutdown level to
	// the lowest, and every process starts at the same default level. The
	// engines run as console processes, which Windows terminates at sign-out,
	// so a backup could be killed before Replicaro hears of the session end
	// and would then be recorded as failed. A higher level gets Replicaro its
	// WM_ENDSESSION first, so it cancels its own work and records it as
	// interrupted before Windows gets to the engines.
	if err := windows.SetProcessShutdownParameters(replicaroShutdownLevel, 0); err != nil {
		log.Printf("Replicaro could not ask Windows to stop it before its engine processes at sign-out: %v", err)
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
	// Runs before tray.close (defers run last first), so the tray state stops
	// asking for changes before the window and icons are released. A change
	// can still arrive after WM_CLOSE has destroyed the window and before this
	// runs; posting it then fails and only logs a line, which is harmless
	// because the tray is going away anyway.
	defer startTrayState(tray.requestState)()

	serviceDone := make(chan error, 1)
	go func() {
		serviceDone <- service()
		tray.requestClose()
	}()
	if !NoStartupBrowser() {
		_ = openWebUI()
	}

	loopErr := tray.runMessageLoop()

	select {
	case err := <-serviceDone:
		return err
	default:
		return loopErr
	}
}

// replicaroShutdownLevel is above the default level of 0x280, in the range
// Windows reserves for applications that shut down first (0x300 to 0x3FF).
const replicaroShutdownLevel = 0x300

// runWithoutTray serves until the service stops or the process is ended, by
// sign-out, the installer or Task Manager, say. The GUI build has no console
// to deliver Ctrl+C or close events and, without the tray, no window either,
// so the signals below only arrive in a console build. Otherwise sign-out
// ends the process without a staged shutdown, since WM_ENDSESSION goes to
// windows. Without the tray window there are no toast notifications either,
// since they're delivered from the tray's message loop.
func runWithoutTray(service func() error) error {
	serviceDone := make(chan error, 1)
	go func() { serviceDone <- service() }()
	if !NoStartupBrowser() {
		_ = openWebUI()
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err := <-serviceDone:
		return err
	case <-signals:
		return nil
	}
}

// ShowStartupFailure shows why Replicaro couldn't start in a message box and
// returns once the user closes it. Replicaro is a GUI program with no
// console, so the message box is the only place a user sees the reason.
func ShowStartupFailure(message string) {
	text, err := windows.UTF16PtrFromString(strings.ReplaceAll(message, "\x00", ""))
	if err != nil {
		return
	}
	caption, err := windows.UTF16PtrFromString("Replicaro")
	if err != nil {
		return
	}
	// A process started at login or by Explorer may not be in the
	// foreground, and Windows can refuse MB_SETFOREGROUND then. MB_TOPMOST
	// keeps the box above other windows even so, rather than behind them
	// where nobody notices it.
	_, _ = windows.MessageBox(0, text, caption, windows.MB_OK|windows.MB_ICONERROR|windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
}

// MessageBox return values for the Yes and No buttons (IDYES, IDNO).
const (
	messageBoxYes = 6
	messageBoxNo  = 7
)

// ConfirmStartupRepair asks question in a Yes/No message box before Replicaro
// starts. It reports whether the box was answered and whether the answer was
// Yes. A plain message box can't name its own buttons. A task dialog could,
// but it needs version 6 of the common controls, which the executable's
// manifest doesn't ask for, so the question is worded for Windows' own Yes
// and No.
func ConfirmStartupRepair(question string) (answered, repair bool) {
	text, err := windows.UTF16PtrFromString(strings.ReplaceAll(question, "\x00", ""))
	if err != nil {
		return false, false
	}
	caption, err := windows.UTF16PtrFromString("Replicaro")
	if err != nil {
		return false, false
	}
	// The same flags as ShowStartupFailure keep the box in front at login.
	choice, _ := windows.MessageBox(0, text, caption, windows.MB_YESNO|windows.MB_ICONWARNING|windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
	switch choice {
	case messageBoxYes:
		return true, true
	case messageBoxNo:
		return true, false
	}
	return false, false
}

// OpenWebUI opens a running instance's web interface from a second launch.
// It's the same rundll32 hand-off the first launch uses. The second launch
// is the process the user just started, so Windows lets the browser it opens
// come to the foreground.
func OpenWebUI(target string) error {
	return openWebUIURL(target)
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
	// Toasts are delivered from the tray's message loop, so without a tray
	// there are none. That's reported through Capabilities, the same as on
	// the other platforms, and not as an error here: a notification that
	// can't be shown mustn't turn a finished operation into a failed one.
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

// Capabilities reports the tray and notifications unless Run had to go on
// without the tray.
func Capabilities() map[string]bool {
	trayAvailable := !runningWithoutTray.Load()
	return map[string]bool{"startAtLogin": true, "nativeNotifications": trayAvailable, "tray": trayAvailable, "menuBar": false}
}
