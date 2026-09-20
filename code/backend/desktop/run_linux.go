//go:build linux

package desktop

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image/png"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const (
	statusItemInterface  = "org.kde.StatusNotifierItem"
	statusWatcherService = "org.kde.StatusNotifierWatcher"
	statusWatcherPath    = dbus.ObjectPath("/StatusNotifierWatcher")
	menuInterface        = "com.canonical.dbusmenu"
	menuPath             = dbus.ObjectPath("/Menu")
	notificationService  = "org.freedesktop.Notifications"
	notificationPath     = dbus.ObjectPath("/org/freedesktop/Notifications")
	portalService        = "org.freedesktop.portal.Desktop"
	portalPath           = dbus.ObjectPath("/org/freedesktop/portal/desktop")
	portalOpenURI        = "org.freedesktop.portal.OpenURI"
)

var (
	activeLinuxMu sync.RWMutex
	activeLinux   *linuxDesktop
)

//go:embed assets/replicaro-favicon.png
var linuxDesktopIcon []byte

type statusPixmap struct {
	Width, Height int32
	Data          []byte
}

type statusTooltip struct {
	IconName string
	Pixmaps  []statusPixmap
	Title    string
	Text     string
}

type notificationImage struct {
	Width, Height, Rowstride int32
	HasAlpha                 bool
	BitsPerSample, Channels  int32
	Data                     []byte
}

type menuLayout struct {
	ID         int32
	Properties map[string]dbus.Variant
	Children   []dbus.Variant
}

type menuPropertyGroup struct {
	ID         int32
	Properties map[string]dbus.Variant
}

type linuxDesktop struct {
	conn                   *dbus.Conn
	serviceName            string
	stateMu                sync.RWMutex
	statusPrepared         bool
	tray                   bool
	notifications          bool
	quit                   chan struct{}
	quitOnce               sync.Once
	signals                chan *dbus.Signal
	stopSignals            chan struct{}
	notificationOperations map[uint32]string
	notificationMu         sync.Mutex
	activationToken        string
	activationTokenMu      sync.Mutex
	open                   func(string) error
}

func Run(service func() error) error {
	desktop, desktopErr := newLinuxDesktop()
	if desktopErr == nil {
		activeLinuxMu.Lock()
		activeLinux = desktop
		activeLinuxMu.Unlock()
		defer func() {
			activeLinuxMu.Lock()
			if activeLinux == desktop {
				activeLinux = nil
			}
			activeLinuxMu.Unlock()
			desktop.close()
		}()
		_ = desktop.open(currentWebUIURL())
	} else if graphicalLinuxSession() {
		_ = openURLFallback(currentWebUIURL())
	}

	serviceDone := make(chan error, 1)
	go func() { serviceDone <- service() }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)

	if desktop == nil {
		select {
		case err := <-serviceDone:
			return err
		case <-signals:
			return nil
		}
	}
	select {
	case err := <-serviceDone:
		return err
	case <-desktop.quit:
		return nil
	case <-signals:
		return nil
	}
}

func newLinuxDesktop() (*linuxDesktop, error) {
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return nil, errors.New("D-Bus desktop session is unavailable")
	}
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return nil, err
	}
	desktop := &linuxDesktop{
		conn:                   conn,
		quit:                   make(chan struct{}),
		signals:                make(chan *dbus.Signal, 16),
		stopSignals:            make(chan struct{}),
		notificationOperations: make(map[uint32]string),
	}
	desktop.open = desktop.openURL
	desktop.startDesktopSignals()
	desktop.setNotifications(desktop.probeNotifications())
	if err := desktop.exportStatusNotifier(); err == nil {
		desktop.stateMu.Lock()
		desktop.statusPrepared = true
		desktop.stateMu.Unlock()
		if desktop.nameHasOwner(statusWatcherService) {
			desktop.setTray(desktop.registerStatusNotifier() == nil)
		}
	}
	return desktop, nil
}

func graphicalLinuxSession() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}

func (desktop *linuxDesktop) requestQuit() {
	desktop.quitOnce.Do(func() { close(desktop.quit) })
}

func (desktop *linuxDesktop) close() {
	desktop.quitOnce.Do(func() { close(desktop.quit) })
	select {
	case <-desktop.stopSignals:
	default:
		close(desktop.stopSignals)
	}
	if desktop.conn != nil {
		desktop.conn.RemoveSignal(desktop.signals)
		_ = desktop.conn.Close()
	}
}

func (desktop *linuxDesktop) nameHasOwner(name string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var owner bool
	call := desktop.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.NameHasOwner", 0, name)
	return call.Err == nil && call.Store(&owner) == nil && owner
}

func (desktop *linuxDesktop) exportStatusNotifier() error {
	desktop.serviceName = "org.kde.StatusNotifierItem-" + strconv.Itoa(os.Getpid()) + "-1"
	reply, err := desktop.conn.RequestName(desktop.serviceName, dbus.NameFlagDoNotQueue)
	if err != nil {
		return err
	}
	if reply != dbus.RequestNameReplyPrimaryOwner && reply != dbus.RequestNameReplyAlreadyOwner {
		return fmt.Errorf("status notifier D-Bus name is unavailable: %s", reply)
	}

	item := &linuxStatusItem{desktop: desktop}
	if err := desktop.conn.Export(item, "/StatusNotifierItem", statusItemInterface); err != nil {
		return err
	}
	itemProps, err := prop.Export(desktop.conn, "/StatusNotifierItem", statusItemProperties())
	if err != nil {
		return err
	}
	itemNode := &introspect.Node{Name: "/StatusNotifierItem", Interfaces: []introspect.Interface{
		prop.IntrospectData,
		{Name: statusItemInterface, Methods: introspect.Methods(item), Properties: itemProps.Introspection(statusItemInterface)},
	}}
	if err := desktop.conn.Export(introspect.NewIntrospectable(itemNode), "/StatusNotifierItem", "org.freedesktop.DBus.Introspectable"); err != nil {
		return err
	}

	menu := &linuxStatusMenu{desktop: desktop}
	if err := desktop.conn.Export(menu, menuPath, menuInterface); err != nil {
		return err
	}
	menuProps, err := prop.Export(desktop.conn, menuPath, statusMenuProperties())
	if err != nil {
		return err
	}
	menuNode := &introspect.Node{Name: string(menuPath), Interfaces: []introspect.Interface{
		prop.IntrospectData,
		{Name: menuInterface, Methods: introspect.Methods(menu), Properties: menuProps.Introspection(menuInterface)},
	}}
	if err := desktop.conn.Export(introspect.NewIntrospectable(menuNode), menuPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return err
	}

	return nil
}

func (desktop *linuxDesktop) registerStatusNotifier() error {
	desktop.stateMu.RLock()
	prepared := desktop.statusPrepared
	desktop.stateMu.RUnlock()
	if !prepared {
		return errors.New("status notifier exports are unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	return desktop.conn.Object(statusWatcherService, statusWatcherPath).CallWithContext(
		ctx, statusWatcherService+".RegisterStatusNotifierItem", 0, desktop.serviceName,
	).Err
}

func (desktop *linuxDesktop) setTray(value bool) {
	desktop.stateMu.Lock()
	desktop.tray = value
	desktop.stateMu.Unlock()
}

func (desktop *linuxDesktop) setNotifications(value bool) {
	desktop.stateMu.Lock()
	desktop.notifications = value
	desktop.stateMu.Unlock()
}

func (desktop *linuxDesktop) capabilities() (bool, bool) {
	desktop.stateMu.RLock()
	defer desktop.stateMu.RUnlock()
	return desktop.tray, desktop.notifications
}

func (desktop *linuxDesktop) probeNotifications() bool {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var capabilities []string
	call := desktop.conn.Object(notificationService, notificationPath).CallWithContext(
		ctx, notificationService+".GetCapabilities", 0,
	)
	return call.Err == nil && call.Store(&capabilities) == nil
}

func statusItemProperties() prop.Map {
	pixmaps := statusIconPixmaps()
	iconThemePath := appImageIconThemePath()
	return prop.Map{statusItemInterface: {
		"Category":      {Value: "ApplicationStatus", Emit: prop.EmitConst},
		"Id":            {Value: "replicaro", Emit: prop.EmitConst},
		"Title":         {Value: "Replicaro", Emit: prop.EmitConst},
		"Status":        {Value: "Active", Emit: prop.EmitConst},
		"WindowId":      {Value: uint32(0), Emit: prop.EmitConst},
		"IconName":      {Value: "replicaro", Emit: prop.EmitConst},
		"IconPixmap":    {Value: pixmaps, Emit: prop.EmitConst},
		"ToolTip":       {Value: statusTooltip{"replicaro", pixmaps, "Replicaro", "Backup companion"}, Emit: prop.EmitConst},
		"ItemIsMenu":    {Value: false, Emit: prop.EmitConst},
		"Menu":          {Value: menuPath, Emit: prop.EmitConst},
		"IconThemePath": {Value: iconThemePath, Emit: prop.EmitConst},
	}}
}

func appImageIconThemePath() string {
	themePath := os.Getenv("REPLICARO_APPIMAGE_ICON_THEME_PATH")
	if themePath == "" || !filepath.IsAbs(themePath) {
		return ""
	}
	info, err := os.Stat(themePath)
	if err != nil || !info.IsDir() {
		return ""
	}
	return filepath.Clean(themePath)
}

func statusMenuProperties() prop.Map {
	return prop.Map{menuInterface: {
		"Version":       {Value: uint32(3), Emit: prop.EmitConst},
		"TextDirection": {Value: "ltr", Emit: prop.EmitConst},
		"Status":        {Value: "normal", Emit: prop.EmitConst},
		"IconThemePath": {Value: []string{}, Emit: prop.EmitConst},
	}}
}

func statusIconPixmaps() []statusPixmap {
	image, err := png.Decode(bytes.NewReader(linuxDesktopIcon))
	if err != nil {
		return []statusPixmap{}
	}
	bounds := image.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	data := make([]byte, 0, width*height*4)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := image.At(x, y).RGBA()
			data = append(data, byte(a>>8), byte(r>>8), byte(g>>8), byte(b>>8))
		}
	}
	return []statusPixmap{{Width: int32(width), Height: int32(height), Data: data}}
}

type linuxStatusItem struct{ desktop *linuxDesktop }

func (item *linuxStatusItem) Activate(_, _ int32) *dbus.Error {
	if err := item.desktop.open(currentWebUIURL()); err != nil {
		return dbus.MakeFailedError(err)
	}
	return nil
}
func (item *linuxStatusItem) ProvideXdgActivationToken(token string) *dbus.Error {
	if len(token) > 4096 {
		return dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"activation token is too large"})
	}
	item.desktop.activationTokenMu.Lock()
	item.desktop.activationToken = token
	item.desktop.activationTokenMu.Unlock()
	return nil
}
func (item *linuxStatusItem) SecondaryActivate(x, y int32) *dbus.Error { return item.Activate(x, y) }
func (*linuxStatusItem) ContextMenu(_, _ int32) *dbus.Error            { return nil }
func (*linuxStatusItem) Scroll(_ int32, _ string) *dbus.Error          { return nil }

type linuxStatusMenu struct{ desktop *linuxDesktop }

func (*linuxStatusMenu) GetLayout(parentID, _ int32, _ []string) (uint32, menuLayout, *dbus.Error) {
	return 1, statusMenuLayout(parentID), nil
}

func (*linuxStatusMenu) GetGroupProperties(ids []int32, _ []string) ([]menuPropertyGroup, *dbus.Error) {
	groups := make([]menuPropertyGroup, 0, len(ids))
	for _, id := range ids {
		layout := statusMenuLayout(id)
		groups = append(groups, menuPropertyGroup{ID: id, Properties: layout.Properties})
	}
	return groups, nil
}

func (*linuxStatusMenu) GetProperty(id int32, name string) (dbus.Variant, *dbus.Error) {
	layout, ok := statusMenuLayoutForID(id)
	if !ok {
		return dbus.Variant{}, dbus.NewError("com.canonical.dbusmenu.Error.InvalidMenuItem", []any{"unknown menu item"})
	}
	value, ok := layout.Properties[name]
	if !ok {
		return dbus.Variant{}, dbus.NewError("com.canonical.dbusmenu.Error.InvalidProperty", []any{"unknown menu property"})
	}
	return value, nil
}

func (menu *linuxStatusMenu) Event(id int32, eventID string, _ dbus.Variant, _ uint32) *dbus.Error {
	if eventID != "clicked" {
		return nil
	}
	switch id {
	case 1:
		if err := menu.desktop.open(currentWebUIURL()); err != nil {
			return dbus.MakeFailedError(err)
		}
	case 2:
		menu.desktop.requestQuit()
	}
	return nil
}

func (menu *linuxStatusMenu) EventGroup(events []struct {
	ID        int32
	EventID   string
	Data      dbus.Variant
	Timestamp uint32
}) ([]int32, *dbus.Error) {
	failed := []int32{}
	for _, event := range events {
		if err := menu.Event(event.ID, event.EventID, event.Data, event.Timestamp); err != nil {
			failed = append(failed, event.ID)
		}
	}
	return failed, nil
}

func (*linuxStatusMenu) AboutToShow(_ int32) (bool, *dbus.Error) { return false, nil }
func (*linuxStatusMenu) AboutToShowGroup(_ []int32) ([]int32, []int32, *dbus.Error) {
	return []int32{}, []int32{}, nil
}

func statusMenuLayout(id int32) menuLayout {
	layout, _ := statusMenuLayoutForID(id)
	return layout
}

func statusMenuLayoutForID(id int32) (menuLayout, bool) {
	label := func(value string) map[string]dbus.Variant {
		return map[string]dbus.Variant{"label": dbus.MakeVariant(value), "enabled": dbus.MakeVariant(true), "visible": dbus.MakeVariant(true)}
	}
	switch id {
	case 1:
		return menuLayout{ID: 1, Properties: label("Open Replicaro"), Children: []dbus.Variant{}}, true
	case 2:
		return menuLayout{ID: 2, Properties: label("Quit Replicaro"), Children: []dbus.Variant{}}, true
	case 3:
		return menuLayout{ID: 3, Properties: map[string]dbus.Variant{"type": dbus.MakeVariant("separator"), "visible": dbus.MakeVariant(true)}, Children: []dbus.Variant{}}, true
	case 0:
		return menuLayout{ID: 0, Properties: map[string]dbus.Variant{"children-display": dbus.MakeVariant("submenu")}, Children: []dbus.Variant{
			dbus.MakeVariant(statusMenuLayout(1)), dbus.MakeVariant(statusMenuLayout(3)), dbus.MakeVariant(statusMenuLayout(2)),
		}}, true
	default:
		return menuLayout{}, false
	}
}

func (desktop *linuxDesktop) startDesktopSignals() {
	desktop.conn.Signal(desktop.signals)
	_ = desktop.conn.AddMatchSignal(
		dbus.WithMatchObjectPath(notificationPath),
		dbus.WithMatchInterface(notificationService),
		dbus.WithMatchMember("ActionInvoked"),
	)
	_ = desktop.conn.AddMatchSignal(
		dbus.WithMatchObjectPath(notificationPath),
		dbus.WithMatchInterface(notificationService),
		dbus.WithMatchMember("NotificationClosed"),
	)
	_ = desktop.conn.AddMatchSignal(
		dbus.WithMatchObjectPath("/org/freedesktop/DBus"),
		dbus.WithMatchInterface("org.freedesktop.DBus"),
		dbus.WithMatchMember("NameOwnerChanged"),
	)
	go func() {
		for {
			select {
			case signal := <-desktop.signals:
				if signal == nil {
					continue
				}
				if signal.Name == "org.freedesktop.DBus.NameOwnerChanged" && len(signal.Body) >= 3 {
					name, nameOK := signal.Body[0].(string)
					newOwner, ownerOK := signal.Body[2].(string)
					if !nameOK || !ownerOK {
						continue
					}
					switch name {
					case statusWatcherService:
						desktop.setTray(newOwner != "" && desktop.registerStatusNotifier() == nil)
					case notificationService:
						desktop.setNotifications(newOwner != "" && desktop.probeNotifications())
					}
					continue
				}
				if len(signal.Body) < 2 {
					continue
				}
				id, idOK := signal.Body[0].(uint32)
				if signal.Name == notificationService+".NotificationClosed" && idOK {
					desktop.notificationMu.Lock()
					delete(desktop.notificationOperations, id)
					desktop.notificationMu.Unlock()
					continue
				}
				if signal.Name != notificationService+".ActionInvoked" {
					continue
				}
				action, actionOK := signal.Body[1].(string)
				if !idOK || !actionOK || (action != "default" && action != "open") {
					continue
				}
				desktop.notificationMu.Lock()
				operationID, known := desktop.notificationOperations[id]
				if known {
					delete(desktop.notificationOperations, id)
				}
				desktop.notificationMu.Unlock()
				if !known {
					continue
				}
				_ = desktop.open(operationURL(operationID))
			case <-desktop.stopSignals:
				return
			}
		}
	}()
}

func operationURL(operationID string) string {
	return TimelineURL(operationID)
}

func (desktop *linuxDesktop) notify(title, message, operationID string, status string) error {
	return desktop.notifyWithPersistence(title, message, operationID, status, false)
}

func (desktop *linuxDesktop) notifyPersistent(title, message, operationID string, status string) error {
	return desktop.notifyWithPersistence(title, message, operationID, status, true)
}

func (desktop *linuxDesktop) notifyWithPersistence(title, message, operationID string, status string, persistent bool) error {
	actions := []string{"default", "Open Replicaro"}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var id uint32
	hints := map[string]dbus.Variant{"desktop-entry": dbus.MakeVariant("replicaro")}
	expireTimeout := int32(0)
	if !persistent {
		hints["transient"] = dbus.MakeVariant(true)
		expireTimeout = 10000
	}
	if image, ok := desktopNotificationImage(status); ok {
		hints["image-data"] = dbus.MakeVariant(image)
	}
	// The notification server may choose an app icon, image-data, both, or
	// neither. Supply the same outcome artwork through both native slots.
	appIcon := "replicaro"
	if iconPath, err := notificationIconPath(status); err == nil {
		appIcon = (&url.URL{Scheme: "file", Path: iconPath}).String()
	}
	call := desktop.conn.Object(notificationService, notificationPath).CallWithContext(
		ctx, notificationService+".Notify", 0,
		"Replicaro", uint32(0), appIcon, title, message, actions,
		hints, expireTimeout,
	)
	if call.Err != nil {
		return call.Err
	}
	if err := call.Store(&id); err != nil {
		return err
	}
	desktop.notificationMu.Lock()
	desktop.notificationOperations[id] = operationID
	desktop.notificationMu.Unlock()
	return nil
}

func desktopNotificationImage(status string) (notificationImage, bool) {
	icon, err := tintedNotificationPNG(linuxDesktopIcon, status)
	if err != nil {
		return notificationImage{}, false
	}
	image, err := png.Decode(bytes.NewReader(icon))
	if err != nil {
		return notificationImage{}, false
	}
	bounds := image.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	data := make([]byte, 0, width*height*4)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, a := image.At(x, y).RGBA()
			data = append(data, byte(r>>8), byte(g>>8), byte(b>>8), byte(a>>8))
		}
	}
	return notificationImage{int32(width), int32(height), int32(width * 4), true, 8, 4, data}, true
}

func (desktop *linuxDesktop) openURL(target string) error {
	return desktop.openURLWithFallback(target, openURLFallback)
}

func (desktop *linuxDesktop) openURLWithFallback(target string, fallback func(string) error) error {
	if _, err := url.ParseRequestURI(target); err != nil {
		return err
	}
	desktop.activationTokenMu.Lock()
	activationToken := desktop.activationToken
	desktop.activationToken = ""
	desktop.activationTokenMu.Unlock()
	options := map[string]dbus.Variant{}
	if activationToken != "" {
		options["activation_token"] = dbus.MakeVariant(activationToken)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	portal := desktop.conn.Object(portalService, portalPath)
	call := portal.CallWithContext(ctx, portalOpenURI+".OpenURI", 0, "", target, options)
	if call.Err == nil {
		return nil
	}
	return fallback(target)
}

func openURLFallback(target string) error {
	return startAndReap(exec.Command("xdg-open", target))
}

func startAndReap(command *exec.Cmd) error {
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

func openWebUIURL(target string) error {
	activeLinuxMu.RLock()
	desktop := activeLinux
	activeLinuxMu.RUnlock()
	if desktop != nil {
		return desktop.open(target)
	}
	if _, err := url.ParseRequestURI(target); err != nil {
		return err
	}
	return openURLFallback(target)
}

func ShowNotification(title, message, operationID string, status string) error {
	activeLinuxMu.RLock()
	desktop := activeLinux
	activeLinuxMu.RUnlock()
	if desktop == nil {
		return nil
	}
	_, notifications := desktop.capabilities()
	if !notifications {
		return nil
	}
	return desktop.notify(title, message, operationID, status)
}

func ShowPersistentNotification(title, message, operationID string, successful bool) error {
	status := notificationStatusFromSuccess(successful)
	activeLinuxMu.RLock()
	desktop := activeLinux
	activeLinuxMu.RUnlock()
	if desktop == nil {
		return nil
	}
	_, notifications := desktop.capabilities()
	if !notifications {
		return nil
	}
	return desktop.notifyPersistent(title, message, operationID, status)
}

func MinimizeToTray() error {
	activeLinuxMu.RLock()
	desktop := activeLinux
	activeLinuxMu.RUnlock()
	if desktop == nil {
		return errors.New("StatusNotifier tray host is unavailable")
	}
	tray, _ := desktop.capabilities()
	if !tray {
		return errors.New("StatusNotifier tray host is unavailable")
	}
	return nil
}

func RestoreWindow() error { return openWebUIURL(currentWebUIURL()) }

func Capabilities() map[string]bool {
	activeLinuxMu.RLock()
	desktop := activeLinux
	activeLinuxMu.RUnlock()
	tray, notifications := false, false
	if desktop != nil {
		tray, notifications = desktop.capabilities()
	} else if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		if conn, err := dbus.ConnectSessionBus(); err == nil {
			probe := &linuxDesktop{conn: conn}
			tray = probe.nameHasOwner(statusWatcherService)
			notifications = probe.probeNotifications()
			_ = conn.Close()
		}
	}
	_, configErr := os.UserConfigDir()
	return map[string]bool{"startAtLogin": configErr == nil, "nativeNotifications": notifications, "tray": tray, "menuBar": false}
}
