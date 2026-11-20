//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"image/color"
	"log"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	trayCallbackMessage     = 0x0401 // WM_USER + 1
	trayNotificationMessage = 0x8001 // WM_APP + 1
	trayStateMessage        = 0x8002 // WM_APP + 2
	menuOpen                = 1001
	menuQuit                = 1002

	wmClose        = 0x0010
	wmDestroy      = 0x0002
	wmLButtonUp    = 0x0202
	wmRButtonUp    = 0x0205
	wmNull         = 0x0000
	nimAdd         = 0x00000000
	nimModify      = 0x00000001
	nimDelete      = 0x00000002
	nifMessage     = 0x00000001
	nifIcon        = 0x00000002
	nifTip         = 0x00000004
	nifGUID        = 0x00000020
	mfString       = 0x00000000
	mfSeparator    = 0x00000800
	tpmRightButton = 0x0002
	tpmReturnCmd   = 0x0100
	imageIcon      = 1
	lrLoadFromFile = 0x00000010
	msgfltAllow    = 1

	wmQueryEndSession = 0x0011
	wmEndSession      = 0x0016
)

var trayIconGUID = windows.GUID{
	Data1: 0x8b6e3d2c,
	Data2: 0x8b8e,
	Data3: 0x4d43,
	Data4: [8]byte{0x9a, 0xb6, 0x72, 0x0d, 0x4c, 0x3e, 0x1f, 0x58},
}

var (
	kernel32                  = windows.NewLazySystemDLL("kernel32.dll")
	user32                    = windows.NewLazySystemDLL("user32.dll")
	shell32                   = windows.NewLazySystemDLL("shell32.dll")
	procGetModuleHandle       = kernel32.NewProc("GetModuleHandleW")
	procRegisterClass         = user32.NewProc("RegisterClassExW")
	procUnregisterClass       = user32.NewProc("UnregisterClassW")
	procCreateWindow          = user32.NewProc("CreateWindowExW")
	procDestroyWindow         = user32.NewProc("DestroyWindow")
	procDefWindowProc         = user32.NewProc("DefWindowProcW")
	procRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	procPostMessage           = user32.NewProc("PostMessageW")
	procPostQuitMessage       = user32.NewProc("PostQuitMessage")
	procGetMessage            = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessage       = user32.NewProc("DispatchMessageW")
	procLoadImage             = user32.NewProc("LoadImageW")
	procDestroyIcon           = user32.NewProc("DestroyIcon")
	procCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	procAppendMenu            = user32.NewProc("AppendMenuW")
	procDestroyMenu           = user32.NewProc("DestroyMenu")
	procGetCursorPos          = user32.NewProc("GetCursorPos")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	procChangeMessageFilter   = user32.NewProc("ChangeWindowMessageFilterEx")
	procShellNotifyIcon       = shell32.NewProc("Shell_NotifyIconW")
)

type nativeTray struct {
	instance            uintptr
	window              uintptr
	icon                uintptr
	menu                uintptr
	className           *uint16
	taskbarCreated      uint32
	notifyData          notifyIconData
	iconAdded           bool
	classRegistered     bool
	windowDestroyed     bool
	notificationMu      sync.Mutex
	notifications       []nativeNotification
	notificationsClosed bool
	// sessionEnd is the application's staged shutdown, which Run passes in.
	// WM_ENDSESSION runs it; see endSession.
	sessionEnd func() error
	// quitPromptOpen is set while the Quit message box is open. Only the
	// tray's own thread reads and writes it.
	quitPromptOpen bool
	// wantedState is the tray state last asked for (see requestState).
	// shownState and stateIcon belong to the tray's own thread: the state the
	// icon shows, and the recoloured icon showing it, which is 0 while the
	// icon is the base icon (idle).
	stateMu     sync.Mutex
	wantedState trayState
	shownState  trayState
	stateIcon   uintptr
}

var errTrayNotificationsClosed = errors.New("notification tray is closed")

type nativeNotification struct {
	title       string
	message     string
	operationID string
	status      string
	persistent  bool
	result      chan error
}

type windowClass struct {
	size        uint32
	style       uint32
	windowProc  uintptr
	classExtra  int32
	windowExtra int32
	instance    uintptr
	icon        uintptr
	cursor      uintptr
	background  uintptr
	menuName    *uint16
	className   *uint16
	iconSmall   uintptr
}

type notifyIconData struct {
	size                       uint32
	window                     uintptr
	id, flags, callbackMessage uint32
	icon                       uintptr
	tip                        [128]uint16
	state, stateMask           uint32
	info                       [256]uint16
	timeoutOrVersion           uint32
	infoTitle                  [64]uint16
	infoFlags                  uint32
	guidItem                   windows.GUID
	balloonIcon                uintptr
}

type windowMessage struct {
	window  uintptr
	message uint32
	wParam  uintptr
	lParam  uintptr
	time    uint32
	point   trayPoint
	private uint32
}

type trayPoint struct {
	x int32
	y int32
}

func newNativeTray() (*nativeTray, error) {
	runtime.LockOSThread()

	tray := &nativeTray{}
	if err := tray.initialize(); err != nil {
		tray.close()
		runtime.UnlockOSThread()
		return nil, err
	}

	return tray, nil
}

func (tray *nativeTray) initialize() error {
	instance, _, callErr := procGetModuleHandle.Call(0)
	if instance == 0 {
		return win32Error("get module handle", callErr)
	}
	tray.instance = instance

	className, err := windows.UTF16PtrFromString(fmt.Sprintf("ReplicaroTray-%d", os.Getpid()))
	if err != nil {
		return err
	}
	tray.className = className

	class := windowClass{
		windowProc: windows.NewCallback(tray.windowProc),
		instance:   tray.instance,
		className:  tray.className,
	}
	class.size = uint32(unsafe.Sizeof(class))
	registered, _, callErr := procRegisterClass.Call(uintptr(unsafe.Pointer(&class)))
	if registered == 0 {
		return win32Error("register tray window class", callErr)
	}
	tray.classRegistered = true

	taskbarMessage, err := windows.UTF16PtrFromString("TaskbarCreated")
	if err != nil {
		return err
	}
	message, _, callErr := procRegisterWindowMessage.Call(uintptr(unsafe.Pointer(taskbarMessage)))
	if message == 0 {
		return win32Error("register taskbar restart message", callErr)
	}
	tray.taskbarCreated = uint32(message)

	windowName, err := windows.UTF16PtrFromString("Replicaro")
	if err != nil {
		return err
	}
	window, _, callErr := procCreateWindow.Call(
		0,
		uintptr(unsafe.Pointer(tray.className)),
		uintptr(unsafe.Pointer(windowName)),
		0, 0, 0, 0, 0, 0, 0, tray.instance, 0,
	)
	if window == 0 {
		return win32Error("create tray window", callErr)
	}
	tray.window = window
	// An elevated Replicaro doesn't receive TaskbarCreated from Explorer,
	// which runs unelevated, unless the window lets that message through.
	// Without it the icon wouldn't come back after Explorer restarts. A
	// failure here only costs that, so it's ignored.
	if procChangeMessageFilter.Find() == nil {
		procChangeMessageFilter.Call(window, uintptr(tray.taskbarCreated), msgfltAllow, 0)
	}

	tray.icon, err = loadTrayIcon()
	if err != nil {
		return err
	}
	tray.menu, err = createTrayMenu()
	if err != nil {
		return err
	}

	tray.notifyData = notifyIconData{
		window:          tray.window,
		id:              1,
		flags:           nifMessage | nifIcon | nifTip | nifGUID,
		callbackMessage: trayCallbackMessage,
		icon:            tray.icon,
		balloonIcon:     tray.icon,
		guidItem:        trayIconGUID,
	}
	tray.notifyData.size = uint32(unsafe.Sizeof(tray.notifyData))
	copyUTF16(tray.notifyData.tip[:], strings.ReplaceAll(trayStateText(trayIdle), "\x00", ""))

	// A missing icon isn't a reason to stop. Early in a login Explorer may not
	// be up yet, and it can refuse the icon for other reasons. The window
	// stays, so the icon is added when Explorer broadcasts TaskbarCreated and
	// toasts keep working in the meantime.
	if err := tray.addIcon(); err != nil {
		log.Printf("Replicaro could not add its notification-area icon and will try again when Explorer restarts: %v", err)
	}
	return nil
}

// shellNotifyIcon calls Shell_NotifyIconW. Tests replace it.
var shellNotifyIcon = func(message uintptr, data *notifyIconData) error {
	result, _, callErr := procShellNotifyIcon.Call(message, uintptr(unsafe.Pointer(data)))
	if result == 0 {
		return callErr
	}
	return nil
}

// addIcon adds the icon with its fixed GUID first, and without it if Windows
// refuses. Windows binds an icon GUID to the path of the executable that
// first used it, so an unsigned portable copy started next to an installed
// one is refused. Without the GUID the icon is identified by its window and
// ID, which is how removeIcon finds it too, since both use notifyData.flags.
func (tray *nativeTray) addIcon() error {
	tray.notifyData.flags = nifMessage | nifIcon | nifTip | nifGUID
	err := shellNotifyIcon(nimAdd, &tray.notifyData)
	if err != nil {
		tray.notifyData.flags &^= nifGUID
		if retryErr := shellNotifyIcon(nimAdd, &tray.notifyData); retryErr != nil {
			return errors.Join(
				win32Error("add notification-area icon", err),
				win32Error("add notification-area icon without its GUID", retryErr),
			)
		}
	}
	tray.iconAdded = true
	return nil
}

func (tray *nativeTray) removeIcon() {
	if !tray.iconAdded {
		return
	}
	_ = shellNotifyIcon(nimDelete, &tray.notifyData)
	tray.iconAdded = false
}

func (tray *nativeTray) windowProc(window uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case trayNotificationMessage:
		tray.showNextNotification()
		return 0

	case trayStateMessage:
		tray.applyState()
		return 0

	case trayCallbackMessage:
		switch uint32(lParam) {
		case wmLButtonUp:
			_ = openWebUI()
		case wmRButtonUp:
			tray.showMenu()
		}
		return 0

	case wmClose:
		procDestroyWindow.Call(window)
		return 0

	case wmDestroy:
		tray.removeIcon()
		tray.windowDestroyed = true
		tray.closeNotifications(errTrayNotificationsClosed)
		procPostQuitMessage.Call(0)
		return 0

	case wmQueryEndSession:
		// Replicaro never vetoes or holds up the end of a session. Another
		// app can still cancel it after this, so the shutdown waits for
		// WM_ENDSESSION.
		return 1

	case wmEndSession:
		if wParam != 0 {
			tray.endSession()
		}
		return 0
	}

	// Explorer broadcasts TaskbarCreated when it starts or restarts. Icons it
	// had are gone by then, and one that couldn't be added at startup gets its
	// next chance here.
	if message == tray.taskbarCreated {
		tray.iconAdded = false
		if err := tray.addIcon(); err != nil {
			log.Printf("Replicaro could not add its notification-area icon after Explorer restarted: %v", err)
			return 0
		}
		// A state change that failed while Explorer was gone is shown now.
		tray.applyState()
		return 0
	}

	result, _, _ := procDefWindowProc.Call(window, uintptr(message), wParam, lParam)
	return result
}

// sessionEndWait bounds how long WM_ENDSESSION waits for the staged
// shutdown. Windows gives an app about five seconds to answer before it
// treats it as not responding, so this stays below that. A shutdown that
// takes longer keeps going until Windows ends the process, and the next start
// finds whatever it left behind, as after any other abrupt stop. Tests
// shorten it.
var sessionEndWait = 4 * time.Second

// endSession runs the staged shutdown when Windows ends the session: the user
// signs out, Windows shuts down or restarts, or an installer closes Replicaro
// through the Restart Manager. Windows may end the process as soon as
// WM_ENDSESSION returns, so the shutdown has to finish inside the message,
// and it runs on its own goroutine because this thread runs the message loop.
// Once the HTTP server has stopped, Run's service goroutine posts WM_CLOSE,
// so the loop ends after this message and Run returns as after Quit.
func (tray *nativeTray) endSession() {
	// The message loop is blocked until the shutdown ends, so a toast queued
	// now would never be shown and its sender would wait for good. Fail
	// them at once, the same way they fail after Quit. The tray also stops
	// being the active one first, as it is after Quit by the time the
	// shutdown starts: operations the shutdown interrupts then send their
	// toasts to no tray, which isn't an error, instead of to a closed one,
	// which would show up as a failed notification on each of them.
	activeTrayMu.Lock()
	if activeTray == tray {
		activeTray = nil
	}
	activeTrayMu.Unlock()
	tray.closeNotifications(errTrayNotificationsClosed)
	if tray.sessionEnd == nil {
		return
	}
	if !finishWithin(tray.sessionEnd, sessionEndWait) {
		log.Printf("Replicaro is still shutting down after %s; Windows may end it before it finishes", sessionEndWait)
	}
}

// requestState asks the tray's thread to show state. It is called with the
// tray state lock held, from the tray state reader and from the
// /api/dashboard and /api/operations/active handlers (see
// UpdateTrayFromDashboard), so it only records the state and posts a
// message; the icon is made and changed on the tray's own thread,
// which owns the icon handles and notifyData. Messages that arrive in a burst
// all apply the latest state.
func (tray *nativeTray) requestState(state trayState) {
	tray.stateMu.Lock()
	tray.wantedState = state
	tray.stateMu.Unlock()
	if tray.window == 0 {
		return
	}
	if result, _, callErr := procPostMessage.Call(tray.window, trayStateMessage, 0, 0); result == 0 {
		log.Printf("Replicaro could not ask its tray to change the icon: %v", win32Error("post tray state", callErr))
	}
}

// applyState shows the wanted state on the tray's thread: the base icon for
// idle, otherwise the base icon recoloured, with the state's tooltip. The
// icon it replaces is destroyed once Windows shows the new one. When the
// icon can't be made or changed, the tray keeps what it shows and a later
// state change tries again.
func (tray *nativeTray) applyState() {
	tray.stateMu.Lock()
	state := tray.wantedState
	tray.stateMu.Unlock()
	if state == tray.shownState {
		return
	}
	icon := tray.icon
	if tint, ok := trayStateTint(state); ok {
		loaded, err := loadTrayStateIcon(tint)
		if err != nil {
			log.Printf("Replicaro could not make its tray icon for the new state and keeps the current one: %v", err)
			return
		}
		icon = loaded
	}
	previous := tray.notifyData
	tray.notifyData.icon = icon
	copyUTF16(tray.notifyData.tip[:], strings.ReplaceAll(trayStateText(state), "\x00", ""))
	// Without the icon in the notification area (Explorer not running yet),
	// notifyData is all there is to change: addIcon shows it later.
	if tray.iconAdded {
		if err := shellNotifyIcon(nimModify, &tray.notifyData); err != nil {
			log.Printf("Replicaro could not change its notification-area icon and keeps the current one: %v", win32Error("change notification-area icon", err))
			tray.notifyData = previous
			if icon != tray.icon {
				procDestroyIcon.Call(icon)
			}
			return
		}
	}
	if tray.stateIcon != 0 {
		procDestroyIcon.Call(tray.stateIcon)
	}
	tray.stateIcon = 0
	if icon != tray.icon {
		tray.stateIcon = icon
	}
	tray.shownState = state
}

func (tray *nativeTray) enqueueNotification(notification nativeNotification) error {
	tray.notificationMu.Lock()
	defer tray.notificationMu.Unlock()
	if tray.notificationsClosed || tray.window == 0 {
		return errTrayNotificationsClosed
	}
	tray.notifications = append(tray.notifications, notification)
	result, _, callErr := procPostMessage.Call(tray.window, trayNotificationMessage, 0, 0)
	if result == 0 {
		tray.notifications = tray.notifications[:len(tray.notifications)-1]
		return win32Error("queue tray notification", callErr)
	}
	return nil
}

func (tray *nativeTray) closeNotifications(err error) {
	tray.notificationMu.Lock()
	tray.notificationsClosed = true
	notifications := tray.notifications
	tray.notifications = nil
	tray.notificationMu.Unlock()

	for _, notification := range notifications {
		if notification.result != nil {
			notification.result <- err
		}
	}
}

func (tray *nativeTray) showNextNotification() {
	tray.notificationMu.Lock()
	if len(tray.notifications) == 0 {
		tray.notificationMu.Unlock()
		return
	}
	notification := tray.notifications[0]
	tray.notifications = tray.notifications[1:]
	tray.notificationMu.Unlock()

	if notification.result != nil {
		notification.result <- showToastNotification(notification)
	} else {
		_ = showToastNotification(notification)
	}

	tray.notificationMu.Lock()
	hasMore := len(tray.notifications) > 0
	tray.notificationMu.Unlock()
	if hasMore {
		procPostMessage.Call(tray.window, trayNotificationMessage, 0, 0)
	}
}

func copyUTF16(destination []uint16, value string) {
	encoded, err := windows.UTF16FromString(value)
	if err != nil {
		return
	}
	copy(destination, encoded)
	if len(destination) > 0 {
		destination[len(destination)-1] = 0
	}
}

func (tray *nativeTray) showMenu() {
	point := trayPoint{}
	result, _, _ := procGetCursorPos.Call(uintptr(unsafe.Pointer(&point)))
	if result == 0 {
		return
	}

	procSetForegroundWindow.Call(tray.window)
	command, _, _ := procTrackPopupMenu.Call(
		tray.menu,
		tpmRightButton|tpmReturnCmd,
		uintptr(point.x),
		uintptr(point.y),
		0,
		tray.window,
		0,
	)

	switch command {
	case menuOpen:
		_ = openWebUI()
	case menuQuit:
		if tray.confirmQuit() {
			tray.requestClose()
		}
	}
	procPostMessage.Call(tray.window, wmNull, 0, 0)
}

// confirmQuit asks before Quit stops Replicaro, and reports whether the
// answer was Yes. The tray icon is the same process as the service, so Quit
// stops every scheduled and running backup until Replicaro is started again,
// and users can read it as "hide the icon". Only this menu click asks:
// sign-out and the service ending on its own still close the tray at once.
//
// The message box runs its own message loop on this thread, which keeps
// dispatching to the tray window, so WM_ENDSESSION and a service stop are
// still handled while it is open, and the tray menu can be opened again. A
// second Quit from it does nothing while the box is open. Windows names the
// buttons Yes and No in its own language, which is why the prompt is a
// question (see ConfirmStartupRepair).
func (tray *nativeTray) confirmQuit() bool {
	if tray.quitPromptOpen {
		return false
	}
	tray.quitPromptOpen = true
	defer func() { tray.quitPromptOpen = false }()
	text, err := windows.UTF16PtrFromString(strings.ReplaceAll(desktopText(TextQuitPrompt), "\x00", ""))
	if err != nil {
		return true
	}
	caption, err := windows.UTF16PtrFromString("Replicaro")
	if err != nil {
		return true
	}
	choice, _ := windows.MessageBox(0, text, caption, windows.MB_YESNO|windows.MB_ICONQUESTION|windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
	return choice == messageBoxYes
}

func (tray *nativeTray) requestClose() {
	if tray.window != 0 {
		procPostMessage.Call(tray.window, wmClose, 0, 0)
	}
}

func (tray *nativeTray) runMessageLoop() error {
	message := windowMessage{}
	for {
		result, _, callErr := procGetMessage.Call(uintptr(unsafe.Pointer(&message)), 0, 0, 0)
		switch int32(result) {
		case -1:
			return win32Error("read tray message", callErr)
		case 0:
			return nil
		default:
			procTranslateMessage.Call(uintptr(unsafe.Pointer(&message)))
			procDispatchMessage.Call(uintptr(unsafe.Pointer(&message)))
		}
	}
}

func (tray *nativeTray) close() {
	tray.closeNotifications(errTrayNotificationsClosed)
	tray.removeIcon()
	if tray.window != 0 && !tray.windowDestroyed {
		procDestroyWindow.Call(tray.window)
	}
	if tray.menu != 0 {
		procDestroyMenu.Call(tray.menu)
		tray.menu = 0
	}
	if tray.stateIcon != 0 {
		procDestroyIcon.Call(tray.stateIcon)
		tray.stateIcon = 0
	}
	if tray.icon != 0 {
		procDestroyIcon.Call(tray.icon)
		tray.icon = 0
	}
	if tray.classRegistered {
		procUnregisterClass.Call(uintptr(unsafe.Pointer(tray.className)), tray.instance)
		tray.classRegistered = false
	}
}

func createTrayMenu() (uintptr, error) {
	menu, _, callErr := procCreatePopupMenu.Call()
	if menu == 0 {
		return 0, win32Error("create tray menu", callErr)
	}

	// The labels are read once, here, so a language change shows in them
	// after the next start.
	openText, quitText := trayLabels()
	openLabel, _ := windows.UTF16PtrFromString(strings.ReplaceAll(openText, "\x00", ""))
	quitLabel, _ := windows.UTF16PtrFromString(strings.ReplaceAll(quitText, "\x00", ""))
	items := []struct {
		flags uintptr
		id    uintptr
		label *uint16
	}{
		{mfString, menuOpen, openLabel},
		{mfSeparator, 0, nil},
		{mfString, menuQuit, quitLabel},
	}
	for _, item := range items {
		result, _, callErr := procAppendMenu.Call(
			menu,
			item.flags,
			item.id,
			uintptr(unsafe.Pointer(item.label)),
		)
		if result == 0 {
			procDestroyMenu.Call(menu)
			return 0, win32Error("add tray menu item", callErr)
		}
	}

	return menu, nil
}

func loadTrayIcon() (uintptr, error) {
	return loadTrayIconData(trayIcon())
}

// loadTrayStateIcon makes the tray icon recoloured in tint, from the same
// .ico the base icon is loaded from.
func loadTrayStateIcon(tint color.NRGBA) (uintptr, error) {
	data, err := tintedICO(trayIcon(), tint)
	if err != nil {
		return 0, err
	}
	return loadTrayIconData(data)
}

func loadTrayIconData(data []byte) (uintptr, error) {
	file, err := os.CreateTemp("", "replicaro-tray-*.ico")
	if err != nil {
		return 0, err
	}
	name := file.Name()
	defer os.Remove(name)

	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return 0, err
	}
	if err := file.Close(); err != nil {
		return 0, err
	}

	filename, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	icon, _, callErr := procLoadImage.Call(
		0,
		uintptr(unsafe.Pointer(filename)),
		imageIcon,
		trayIconSize,
		trayIconSize,
		lrLoadFromFile,
	)
	if icon == 0 {
		return 0, win32Error("load tray icon", callErr)
	}
	return icon, nil
}

func win32Error(operation string, err error) error {
	if err == nil {
		return fmt.Errorf("%s failed", operation)
	}
	if errno, ok := err.(syscall.Errno); ok && errno == 0 {
		return fmt.Errorf("%s failed", operation)
	}
	return fmt.Errorf("%s: %w", operation, err)
}
