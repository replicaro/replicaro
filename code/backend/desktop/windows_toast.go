//go:build windows

package desktop

import (
	"fmt"
	"html"
	"runtime"
	"sync"
	"syscall"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	toastSuccessAppID        = "Replicaro"
	toastWarningAppID        = "Replicaro.Warning"
	toastWarningAppKey       = `Software\Classes\AppUserModelId\Replicaro.Warning`
	toastFailureAppID        = "Replicaro.Failure"
	toastSuccessAppKey       = `Software\Classes\AppUserModelId\Replicaro`
	toastFailureAppKey       = `Software\Classes\AppUserModelId\Replicaro.Failure`
	toastIconBackgroundColor = "FF0A0C10"
	roInitMultithread        = 1
)

var (
	combase = windows.NewLazySystemDLL("combase.dll")

	procRoInitialize       = combase.NewProc("RoInitialize")
	procRoUninitialize     = combase.NewProc("RoUninitialize")
	procRoActivateInstance = combase.NewProc("RoActivateInstance")
	procRoGetActivation    = combase.NewProc("RoGetActivationFactory")
	procCreateString       = combase.NewProc("WindowsCreateString")
	procDeleteString       = combase.NewProc("WindowsDeleteString")

	toastRuntimeMu          sync.Mutex
	toastRuntimeInitialized bool
	toastRegistrationDone   = map[string]bool{}
)

type toastHString uintptr

type toastInspectable struct {
	vtable *toastInspectableVtbl
}

type toastInspectableVtbl struct {
	queryInterface      uintptr
	addRef              uintptr
	release             uintptr
	getIIDs             uintptr
	getRuntimeClassName uintptr
	getTrustLevel       uintptr
}

type toastXMLDocumentIO struct {
	vtable *toastXMLDocumentIOVtbl
}

type toastXMLDocumentIOVtbl struct {
	toastInspectableVtbl
	loadXML             uintptr
	loadXMLWithSettings uintptr
	saveToFileAsync     uintptr
}

type toastManagerStatics struct {
	vtable *toastManagerStaticsVtbl
}

type toastManagerStaticsVtbl struct {
	toastInspectableVtbl
	getDefault uintptr
}

type toastManagerForUser struct {
	vtable *toastManagerForUserVtbl
}

type toastManagerForUserVtbl struct {
	toastInspectableVtbl
	createToastNotifier       uintptr
	createToastNotifierWithID uintptr
	getHistory                uintptr
	getUser                   uintptr
}

type toastNotificationFactory struct {
	vtable *toastNotificationFactoryVtbl
}

type toastNotificationFactoryVtbl struct {
	toastInspectableVtbl
	createToastNotification uintptr
}

type toastNotifier struct {
	vtable *toastNotifierVtbl
}

type toastNotifierVtbl struct {
	toastInspectableVtbl
	show                           uintptr
	hide                           uintptr
	getSetting                     uintptr
	addToSchedule                  uintptr
	removeFromSchedule             uintptr
	getScheduledToastNotifications uintptr
}

var (
	toastManagerStaticsIID      = mustToastGUID("d6f5f569-d40d-407c-8989-88cab42cfd14")
	toastManagerForUserIID      = mustToastGUID("79ab57f6-43fe-487b-8a7f-99567200ae94")
	toastNotificationFactoryIID = mustToastGUID("04124b20-82c6-4229-b109-fd9ed4662b53")
	toastNotifierIID            = mustToastGUID("75927b93-03f3-41ec-91d3-6e5bac1b38e7")
	toastXMLDocumentIOIID       = mustToastGUID("6cd0e74e-ee65-4489-9ebf-ca43e87ba637")
)

func mustToastGUID(value string) windows.GUID {
	guid, err := windows.GUIDFromString("{" + value + "}")
	if err != nil {
		panic(err)
	}
	return guid
}

func showWindowsToast(title, message, target string, status string, persistent bool) error {
	if err := initializeWindowsToast(); err != nil {
		return err
	}
	if err := registerToastApp(status); err != nil {
		return err
	}
	iconURI, err := notificationIconURI(status)
	if err != nil {
		return err
	}

	document, err := activateToastClass("Windows.Data.Xml.Dom.XmlDocument")
	if err != nil {
		return fmt.Errorf("create notification XML document: %w", err)
	}
	defer releaseToastObject(document)

	xmlDocumentIO, err := queryToastInterface(document, toastXMLDocumentIOIID)
	if err != nil {
		return fmt.Errorf("query notification XML interface: %w", err)
	}
	defer releaseToastObject(xmlDocumentIO)
	if err := loadToastXML(xmlDocumentIO, buildToastXML(title, message, target, iconURI, persistent)); err != nil {
		return err
	}

	managerFactory, err := getToastActivationFactory("Windows.UI.Notifications.ToastNotificationManager", toastManagerStaticsIID)
	if err != nil {
		return fmt.Errorf("get notification manager: %w", err)
	}
	defer releaseToastObject(managerFactory)

	manager := unsafe.Pointer(nil)
	managerHR, _, _ := syscall.SyscallN(
		(*toastManagerStatics)(managerFactory).vtable.getDefault,
		uintptr(managerFactory),
		uintptr(unsafe.Pointer(&manager)),
	)
	if managerHR != 0 {
		return toastHRESULTError("get notification manager for user", managerHR)
	}
	defer releaseToastObject(manager)

	managerForUser, err := queryToastInterface(manager, toastManagerForUserIID)
	if err != nil {
		return fmt.Errorf("query notification manager for user: %w", err)
	}
	defer releaseToastObject(managerForUser)

	appIDValue, _ := toastIdentity(status)
	appID, err := newToastHString(appIDValue)
	if err != nil {
		return fmt.Errorf("create notification app ID: %w", err)
	}
	defer deleteToastHString(appID)

	notifier := unsafe.Pointer(nil)
	notifierHR, _, _ := syscall.SyscallN(
		(*toastManagerForUser)(managerForUser).vtable.createToastNotifierWithID,
		uintptr(managerForUser),
		uintptr(appID),
		uintptr(unsafe.Pointer(&notifier)),
	)
	if notifierHR != 0 {
		return toastHRESULTError("create notification notifier", notifierHR)
	}
	defer releaseToastObject(notifier)

	factory, err := getToastActivationFactory("Windows.UI.Notifications.ToastNotification", toastNotificationFactoryIID)
	if err != nil {
		return fmt.Errorf("get notification factory: %w", err)
	}
	defer releaseToastObject(factory)

	notification := unsafe.Pointer(nil)
	notificationHR, _, _ := syscall.SyscallN(
		(*toastNotificationFactory)(factory).vtable.createToastNotification,
		uintptr(factory),
		uintptr(document),
		uintptr(unsafe.Pointer(&notification)),
	)
	if notificationHR != 0 {
		return toastHRESULTError("create notification", notificationHR)
	}
	defer releaseToastObject(notification)

	notifierInterface, err := queryToastInterface(notifier, toastNotifierIID)
	if err != nil {
		return fmt.Errorf("query notification notifier interface: %w", err)
	}
	defer releaseToastObject(notifierInterface)

	showHR, _, _ := syscall.SyscallN(
		(*toastNotifier)(notifierInterface).vtable.show,
		uintptr(notifierInterface),
		uintptr(notification),
	)
	if showHR != 0 {
		return toastHRESULTError("show notification", showHR)
	}
	return nil
}

func initializeWindowsToast() error {
	toastRuntimeMu.Lock()
	defer toastRuntimeMu.Unlock()
	if toastRuntimeInitialized {
		return nil
	}
	hr, _, _ := procRoInitialize.Call(roInitMultithread)
	if hr != 0 && hr != 1 { // S_OK and S_FALSE both indicate success.
		return toastHRESULTError("initialize Windows Runtime", hr)
	}
	toastRuntimeInitialized = true
	return nil
}

func uninitializeWindowsToast() {
	toastRuntimeMu.Lock()
	defer toastRuntimeMu.Unlock()
	if !toastRuntimeInitialized {
		return
	}
	procRoUninitialize.Call()
	toastRuntimeInitialized = false
}

// The toast sender and header-icon registration must use the same identity.
// Keep the three native app IDs paired with their registration keys.
func toastIdentity(status string) (appID, registryKey string) {
	switch status {
	case "success":
		return toastSuccessAppID, toastSuccessAppKey
	case "completed_with_issues":
		return toastWarningAppID, toastWarningAppKey
	default:
		return toastFailureAppID, toastFailureAppKey
	}
}

func registerToastApp(status string) error {
	toastRuntimeMu.Lock()
	defer toastRuntimeMu.Unlock()
	if toastRegistrationDone[status] {
		return nil
	}
	iconPath, err := notificationIconPath(status)
	if err != nil {
		return err
	}
	_, appKey := toastIdentity(status)

	key, _, err := registry.CreateKey(registry.CURRENT_USER, appKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	values := []struct {
		name  string
		value string
	}{
		{name: "DisplayName", value: "Replicaro"},
		{name: "IconUri", value: iconPath},
		{name: "IconBackgroundColor", value: toastIconBackgroundColor},
	}
	for _, value := range values {
		if err := key.SetStringValue(value.name, value.value); err != nil {
			_ = key.Close()
			return err
		}
	}
	if err := key.Close(); err != nil {
		return err
	}
	toastRegistrationDone[status] = true
	return nil
}

func activateToastClass(className string) (unsafe.Pointer, error) {
	classID, err := newToastHString(className)
	if err != nil {
		return nil, err
	}
	defer deleteToastHString(classID)

	instance := unsafe.Pointer(nil)
	hr, _, _ := procRoActivateInstance.Call(
		uintptr(classID),
		uintptr(unsafe.Pointer(&instance)),
	)
	if hr != 0 {
		return nil, toastHRESULTError("activate Windows Runtime class", hr)
	}
	return instance, nil
}

func getToastActivationFactory(className string, iid windows.GUID) (unsafe.Pointer, error) {
	classID, err := newToastHString(className)
	if err != nil {
		return nil, err
	}
	defer deleteToastHString(classID)

	factory := unsafe.Pointer(nil)
	hr, _, _ := procRoGetActivation.Call(
		uintptr(classID),
		uintptr(unsafe.Pointer(&iid)),
		uintptr(unsafe.Pointer(&factory)),
	)
	if hr != 0 {
		return nil, toastHRESULTError("get Windows Runtime activation factory", hr)
	}
	return factory, nil
}

func queryToastInterface(object unsafe.Pointer, iid windows.GUID) (unsafe.Pointer, error) {
	if object == nil {
		return nil, fmt.Errorf("cannot query interface on nil object")
	}
	result := unsafe.Pointer(nil)
	hr, _, _ := syscall.SyscallN(
		(*toastInspectable)(object).vtable.queryInterface,
		uintptr(object),
		uintptr(unsafe.Pointer(&iid)),
		uintptr(unsafe.Pointer(&result)),
	)
	if hr != 0 {
		return nil, toastHRESULTError("query Windows Runtime interface", hr)
	}
	return result, nil
}

func releaseToastObject(object unsafe.Pointer) {
	if object != nil {
		syscall.SyscallN(
			(*toastInspectable)(object).vtable.release,
			uintptr(object),
		)
	}
}

func loadToastXML(object unsafe.Pointer, value string) error {
	valueHString, err := newToastHString(value)
	if err != nil {
		return fmt.Errorf("create notification XML string: %w", err)
	}
	defer deleteToastHString(valueHString)

	hr, _, _ := syscall.SyscallN(
		(*toastXMLDocumentIO)(object).vtable.loadXML,
		uintptr(object),
		uintptr(valueHString),
	)
	if hr != 0 {
		return toastHRESULTError("load notification XML", hr)
	}
	return nil
}

func newToastHString(value string) (toastHString, error) {
	encoded := utf16.Encode([]rune(value))
	var pointer uintptr
	if len(encoded) > 0 {
		pointer = uintptr(unsafe.Pointer(&encoded[0]))
	}
	var result toastHString
	hr, _, _ := procCreateString.Call(
		pointer,
		uintptr(len(encoded)),
		uintptr(unsafe.Pointer(&result)),
	)
	runtime.KeepAlive(encoded)
	if hr != 0 {
		return 0, toastHRESULTError("create Windows Runtime string", hr)
	}
	return result, nil
}

func deleteToastHString(value toastHString) {
	if value != 0 {
		procDeleteString.Call(uintptr(value))
	}
}

func buildToastXML(title, message, target, iconURI string, persistent bool) string {
	if persistent {
		return fmt.Sprintf(
			`<toast activationType="protocol" launch="%s" duration="long" scenario="reminder"><visual><binding template="ToastGeneric"><image placement="appLogoOverride" src="%s" alt="Replicaro" /><text>%s</text><text>%s</text></binding></visual><audio src="ms-winsoundevent:Notification.Default" /><actions><action content="Open Replicaro" activationType="protocol" arguments="%s" /></actions></toast>`,
			html.EscapeString(target),
			html.EscapeString(iconURI),
			html.EscapeString(title),
			html.EscapeString(message),
			html.EscapeString(target),
		)
	}
	return fmt.Sprintf(
		`<toast activationType="protocol" launch="%s" duration="short"><visual><binding template="ToastGeneric"><image placement="appLogoOverride" src="%s" alt="Replicaro" /><text>%s</text><text>%s</text></binding></visual><audio src="ms-winsoundevent:Notification.Default" /></toast>`,
		html.EscapeString(target),
		html.EscapeString(iconURI),
		html.EscapeString(title),
		html.EscapeString(message),
	)
}

func toastHRESULTError(operation string, hr uintptr) error {
	return fmt.Errorf("%s failed with HRESULT 0x%08x", operation, uint32(hr))
}
