//go:build windows

package desktop

import (
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	trayCallbackMessage     = 0x0401 // WM_USER + 1
	trayNotificationMessage = 0x8001 // WM_APP + 1
	menuOpen                = 1001
	menuQuit                = 1002

	wmClose        = 0x0010
	wmDestroy      = 0x0002
	wmLButtonUp    = 0x0202
	wmRButtonUp    = 0x0205
	wmNull         = 0x0000
	nimAdd         = 0x00000000
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
	tooltip, err := windows.UTF16FromString("Replicaro")
	if err != nil {
		return err
	}
	copy(tray.notifyData.tip[:], tooltip)

	return tray.addIcon()
}

func (tray *nativeTray) addIcon() error {
	tray.notifyData.flags = nifMessage | nifIcon | nifTip | nifGUID
	result, _, callErr := procShellNotifyIcon.Call(
		nimAdd,
		uintptr(unsafe.Pointer(&tray.notifyData)),
	)
	if result == 0 {
		return win32Error("add notification-area icon", callErr)
	}
	tray.iconAdded = true
	return nil
}

func (tray *nativeTray) removeIcon() {
	if !tray.iconAdded {
		return
	}
	procShellNotifyIcon.Call(nimDelete, uintptr(unsafe.Pointer(&tray.notifyData)))
	tray.iconAdded = false
}

func (tray *nativeTray) windowProc(window uintptr, message uint32, wParam, lParam uintptr) uintptr {
	switch message {
	case trayNotificationMessage:
		tray.showNextNotification()
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
	}

	if message == tray.taskbarCreated {
		tray.iconAdded = false
		_ = tray.addIcon()
		return 0
	}

	result, _, _ := procDefWindowProc.Call(window, uintptr(message), wParam, lParam)
	return result
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
		tray.requestClose()
	}
	procPostMessage.Call(tray.window, wmNull, 0, 0)
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

	openLabel, _ := windows.UTF16PtrFromString("Open Replicaro")
	quitLabel, _ := windows.UTF16PtrFromString("Quit Replicaro")
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
	file, err := os.CreateTemp("", "replicaro-tray-*.ico")
	if err != nil {
		return 0, err
	}
	name := file.Name()
	defer os.Remove(name)

	if _, err := file.Write(trayIcon()); err != nil {
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
