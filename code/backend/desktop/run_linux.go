//go:build linux

package desktop

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"image/png"
	"log"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/godbus/dbus/v5"
	"github.com/godbus/dbus/v5/introspect"
	"github.com/godbus/dbus/v5/prop"
)

const (
	statusItemInterface  = "org.kde.StatusNotifierItem"
	statusItemPath       = dbus.ObjectPath("/StatusNotifierItem")
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
	statusProps            *prop.Properties
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
	// The pending Quit prompt, if any (see requestQuitFromMenu). quitPromptID
	// is 0 when none is pending.
	quitPromptMu    sync.Mutex
	quitPromptID    uint32
	quitPromptTimer *time.Timer
}

// quitPromptLimit is how long the Quit prompt waits for an answer. A prompt
// that isn't answered in time counts as Cancel. Tests shorten it.
var quitPromptLimit = 30 * time.Second

// startLinuxDesktop and openWithoutDesktop are variables so tests can run Run
// without a session bus or a browser.
var (
	startLinuxDesktop  = newLinuxDesktop
	openWithoutDesktop = openURLFallback
)

// Run serves until the service stops, the tray's Quit is chosen, or the
// process gets SIGINT or SIGTERM. A Linux session that ends either stops
// Replicaro with SIGTERM, which reaches the same return as any other stop, or
// leaves it running without its session bus (see receiveDesktopSignals). So
// the caller runs the staged shutdown after Run returns, and Run never calls
// stop itself.
func Run(service func() error, _ func() error) error {
	desktop, desktopErr := startLinuxDesktop()
	openOnStart := !NoStartupBrowser()
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
		// The item is exported even before a tray host registers it, so its
		// state is kept up to date from the start; a host that appears later
		// reads the current icon. This defer runs before the one above, so the
		// state stops changing before the connection closes.
		if desktop.exportedStatusProperties() != nil {
			defer startTrayState(desktop.showTrayState)()
		}
		if openOnStart {
			_ = desktop.open(currentWebUIURL())
		}
	} else if openOnStart && graphicalLinuxSession() {
		_ = openWithoutDesktop(currentWebUIURL())
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
	// A Quit prompt can still be showing when Replicaro stops some other way
	// (SIGTERM, systemctl stop, the session ending) or through a second Quit
	// click. Close it before the bus connection goes, because some servers
	// (GNOME Shell) ignore the expiry and would keep a "Quit Replicaro?"
	// notification for a process that is gone. Only the close request is
	// sent, bounded by closeNotification's limit; shutdown never waits for
	// an answer.
	desktop.quitPromptMu.Lock()
	promptID := desktop.quitPromptID
	desktop.clearQuitPromptLocked()
	desktop.quitPromptMu.Unlock()
	if promptID != 0 && desktop.conn != nil {
		desktop.closeNotification(promptID)
	}
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
	if err := desktop.conn.Export(item, statusItemPath, statusItemInterface); err != nil {
		return err
	}
	itemProps, err := prop.Export(desktop.conn, statusItemPath, statusItemProperties())
	if err != nil {
		return err
	}
	itemNode := &introspect.Node{Name: string(statusItemPath), Interfaces: []introspect.Interface{
		prop.IntrospectData,
		{
			Name: statusItemInterface, Methods: introspect.Methods(item), Properties: itemProps.Introspection(statusItemInterface),
			Signals: []introspect.Signal{{Name: "NewIcon"}, {Name: "NewToolTip"}},
		},
	}}
	if err := desktop.conn.Export(introspect.NewIntrospectable(itemNode), statusItemPath, "org.freedesktop.DBus.Introspectable"); err != nil {
		return err
	}
	desktop.stateMu.Lock()
	desktop.statusProps = itemProps
	desktop.stateMu.Unlock()

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

// statusItemProperties builds a new property map with the values the item is
// exported with, in the idle state. Nothing changes this map once it is
// exported; the tray state changes the live exported properties, which
// exportedStatusProperties returns. IconName, IconPixmap and ToolTip change
// with the tray state (see showTrayState); StatusNotifierItem announces such
// changes with its own NewIcon and NewToolTip signals rather than
// PropertiesChanged, so they are EmitFalse. The rest never change.
func statusItemProperties() prop.Map {
	iconName, pixmaps, tooltip := statusItemIcon(trayIdle)
	iconThemePath := appImageIconThemePath()
	return prop.Map{statusItemInterface: {
		"Category":      {Value: "ApplicationStatus", Emit: prop.EmitConst},
		"Id":            {Value: "replicaro", Emit: prop.EmitConst},
		"Title":         {Value: "Replicaro", Emit: prop.EmitConst},
		"Status":        {Value: "Active", Emit: prop.EmitConst},
		"WindowId":      {Value: uint32(0), Emit: prop.EmitConst},
		"IconName":      {Value: iconName, Emit: prop.EmitFalse},
		"IconPixmap":    {Value: pixmaps, Emit: prop.EmitFalse},
		"ToolTip":       {Value: tooltip, Emit: prop.EmitFalse},
		"ItemIsMenu":    {Value: false, Emit: prop.EmitConst},
		"Menu":          {Value: menuPath, Emit: prop.EmitConst},
		"IconThemePath": {Value: iconThemePath, Emit: prop.EmitConst},
	}}
}

// statusItemIcon returns the IconName, IconPixmap and ToolTip values for
// state. Idle names the installed replicaro theme icon and serves the
// embedded teal icon as the pixmap. Tray hosts prefer the icon name over the
// pixmap, and the theme icon is always teal, so every other state leaves the
// name empty and serves the recoloured pixmap, both for the icon and in the
// tooltip. The tooltip's title carries the state
// text; its body is the description (trayDescription).
func statusItemIcon(state trayState) (string, []statusPixmap, statusTooltip) {
	iconName, source := "replicaro", linuxDesktopIcon
	if tint, ok := trayStateTint(state); ok {
		if tinted, err := tintPNG(linuxDesktopIcon, tint); err == nil {
			iconName, source = "", tinted
		}
	}
	pixmaps := statusIconPixmaps(source)
	return iconName, pixmaps, statusTooltip{iconName, pixmaps, trayStateText(state), trayDescription()}
}

var trayDescriptionState struct {
	once sync.Once
	text string
}

// trayDescription returns the tooltip's second line, which says what
// Replicaro is. Like the state text above it (trayStateText), it is read once,
// when the tray is set up, in the language saved then.
func trayDescription() string {
	trayDescriptionState.once.Do(func() {
		trayDescriptionState.text = desktopText(TextTrayDescription)
	})
	return trayDescriptionState.text
}

// exportedStatusProperties returns the live properties of the exported item,
// the ones showTrayState changes, or nil when the item couldn't be exported.
// statusItemProperties only builds the idle values they start from.
func (desktop *linuxDesktop) exportedStatusProperties() *prop.Properties {
	desktop.stateMu.RLock()
	defer desktop.stateMu.RUnlock()
	return desktop.statusProps
}

// showTrayState changes the item's icon and tooltip to state and announces
// the change. Hosts that ignore the signals keep showing whatever they read
// first, which is the teal icon only if they read it while the state was
// idle.
func (desktop *linuxDesktop) showTrayState(state trayState) {
	props := desktop.exportedStatusProperties()
	if props == nil {
		return
	}
	iconName, pixmaps, tooltip := statusItemIcon(state)
	props.SetMust(statusItemInterface, "IconName", iconName)
	props.SetMust(statusItemInterface, "IconPixmap", pixmaps)
	props.SetMust(statusItemInterface, "ToolTip", tooltip)
	if err := desktop.conn.Emit(statusItemPath, statusItemInterface+".NewIcon"); err != nil {
		log.Printf("Replicaro could not announce its new tray icon: %v", err)
	}
	if err := desktop.conn.Emit(statusItemPath, statusItemInterface+".NewToolTip"); err != nil {
		log.Printf("Replicaro could not announce its new tray tooltip: %v", err)
	}
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

func statusIconPixmaps(source []byte) []statusPixmap {
	image, err := png.Decode(bytes.NewReader(source))
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

// maxActivationTokenBytes bounds an XDG activation token before it's held or
// forwarded to the portal. Real tokens are short; anything this large is
// garbage, whether it came from the tray host or a launcher's environment.
const maxActivationTokenBytes = 4096

func (item *linuxStatusItem) Activate(_, _ int32) *dbus.Error {
	if err := item.desktop.open(currentWebUIURL()); err != nil {
		return dbus.MakeFailedError(err)
	}
	return nil
}
func (item *linuxStatusItem) ProvideXdgActivationToken(token string) *dbus.Error {
	if len(token) > maxActivationTokenBytes {
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
		menu.desktop.requestQuitFromMenu()
	}
	return nil
}

// requestQuitFromMenu asks before Quit stops Replicaro: the tray icon is the
// same process as the service, so Quit stops every scheduled and running
// backup until Replicaro is started again, and users can read it as "hide
// the icon". Only this menu click asks. Sign-out, SIGTERM, systemctl stop and
// the service ending on its own still quit at once through Run, and so does
// Quit itself once confirmed.
//
// The prompt is a notification with Quit and Cancel buttons, because the
// tray has no window of its own. A notification can't take an answer
// reliably (Do Not Disturb or hidden banners mean it is never seen), so:
//   - a second Quit click while the prompt is pending quits at once;
//   - the prompt expires after 30 seconds, and closing or ignoring it
//     counts as Cancel;
//   - without a notification server, or with one that can't show buttons,
//     Quit quits at once.
func (desktop *linuxDesktop) requestQuitFromMenu() {
	// The lock is held while the prompt is sent, so a second click that
	// arrives meanwhile finds it pending instead of sending a second one.
	desktop.quitPromptMu.Lock()
	defer desktop.quitPromptMu.Unlock()
	if desktop.quitPromptID != 0 {
		// The prompt stays pending here; close, which Run always reaches
		// after a quit, closes it before the bus connection goes. Closing it
		// from a separate goroutine instead could lose the request to the
		// connection closing first.
		desktop.requestQuit()
		return
	}
	id, err := desktop.showQuitPrompt()
	if err != nil || id == 0 {
		desktop.requestQuit()
		return
	}
	desktop.quitPromptID = id
	desktop.quitPromptTimer = time.AfterFunc(quitPromptLimit, func() {
		// Some servers ignore a notification's expiry, so the prompt is
		// closed here as well; closing it counts as Cancel.
		if desktop.takeQuitPrompt(id) {
			desktop.closeNotification(id)
		}
	})
}

func (desktop *linuxDesktop) clearQuitPromptLocked() {
	desktop.quitPromptID = 0
	if desktop.quitPromptTimer != nil {
		desktop.quitPromptTimer.Stop()
		desktop.quitPromptTimer = nil
	}
}

// takeQuitPrompt clears the pending prompt if it is id, and reports whether
// it was.
func (desktop *linuxDesktop) takeQuitPrompt(id uint32) bool {
	desktop.quitPromptMu.Lock()
	defer desktop.quitPromptMu.Unlock()
	if id == 0 || desktop.quitPromptID != id {
		return false
	}
	desktop.clearQuitPromptLocked()
	return true
}

// showQuitPrompt sends the Quit prompt notification and returns its ID. It
// returns an error when notifications are unavailable or the server can't
// show buttons, so the caller quits without asking.
func (desktop *linuxDesktop) showQuitPrompt() (uint32, error) {
	if _, notifications := desktop.capabilities(); !notifications {
		return 0, errors.New("desktop notifications are unavailable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	server := desktop.conn.Object(notificationService, notificationPath)
	var capabilities []string
	if call := server.CallWithContext(ctx, notificationService+".GetCapabilities", 0); call.Err != nil || call.Store(&capabilities) != nil {
		return 0, errors.New("notification server capabilities are unavailable")
	}
	if !slices.Contains(capabilities, "actions") {
		return 0, errors.New("notification server can't show buttons")
	}
	texts := desktopTexts(TextQuitButton, TextCancelButton, TextTrayQuit, TextQuitPrompt)
	actions := []string{"quit", texts[0], "cancel", texts[1]}
	hints := map[string]dbus.Variant{"desktop-entry": dbus.MakeVariant("replicaro")}
	if image, ok := desktopNotificationImage("completed_with_issues"); ok {
		hints["image-data"] = dbus.MakeVariant(image)
	}
	var id uint32
	call := server.CallWithContext(ctx, notificationService+".Notify", 0,
		"Replicaro", uint32(0), "replicaro", texts[2],
		freedesktopBodyText(texts[3], slices.Contains(capabilities, "body-markup")),
		actions, hints, int32(quitPromptLimit/time.Millisecond))
	if call.Err != nil {
		return 0, call.Err
	}
	if err := call.Store(&id); err != nil {
		return 0, err
	}
	return id, nil
}

// closeNotification asks the server to close a notification; errors are
// ignored, since the notification may already be gone.
func (desktop *linuxDesktop) closeNotification(id uint32) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = desktop.conn.Object(notificationService, notificationPath).
		CallWithContext(ctx, notificationService+".CloseNotification", 0, id).Err
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
	open, quit := trayLabels()
	switch id {
	case 1:
		return menuLayout{ID: 1, Properties: label(open), Children: []dbus.Variant{}}, true
	case 2:
		return menuLayout{ID: 2, Properties: label(quit), Children: []dbus.Variant{}}, true
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
	go desktop.receiveDesktopSignals()
}

// receiveDesktopSignals handles the D-Bus signals Replicaro subscribes to
// until close stops it or the connection to the session bus is lost.
func (desktop *linuxDesktop) receiveDesktopSignals() {
	for {
		select {
		case signal, ok := <-desktop.signals:
			if !ok {
				// godbus closes the channel when the connection is lost, for
				// example when the session bus exits at the end of a
				// session. The connection doesn't come back, so the tray and
				// notifications are reported unavailable and Replicaro keeps
				// serving without them. Receiving from the closed channel
				// again would return at once, forever, and spin a core.
				desktop.setTray(false)
				desktop.setNotifications(false)
				return
			}
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
				// A Quit prompt closed without its Quit button (dismissed or
				// expired) is a Cancel.
				desktop.takeQuitPrompt(id)
				continue
			}
			if signal.Name != notificationService+".ActionInvoked" {
				continue
			}
			action, actionOK := signal.Body[1].(string)
			if idOK && actionOK && (action == "quit" || action == "cancel") {
				if desktop.takeQuitPrompt(id) && action == "quit" {
					desktop.requestQuit()
				}
				continue
			}
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
	notifications := desktop.conn.Object(notificationService, notificationPath)
	call := notifications.CallWithContext(
		ctx, notificationService+".Notify", 0,
		"Replicaro", uint32(0), appIcon, title, notificationBody(ctx, notifications, message), actions,
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

// ShowStartupFailure shows why Replicaro couldn't start as a desktop
// notification. A desktop launch sends stderr to the journal or to
// ~/.xsession-errors, where a user doesn't look, so the notification is the
// place they see the reason. Only a graphical session with a session bus gets
// one; headless installs keep the error on stderr and in the startup log, and
// the container on stderr alone, where systemd or Docker already capture it.
// Errors are ignored, because the caller exits next either way.
func ShowStartupFailure(message string) {
	if !graphicalLinuxSession() || os.Getenv("DBUS_SESSION_BUS_ADDRESS") == "" {
		return
	}
	// One deadline covers connecting and notifying, so a session bus that
	// doesn't answer can't keep the failed process from exiting.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, err := dbus.ConnectSessionBus(dbus.WithContext(ctx))
	if err != nil {
		return
	}
	defer conn.Close()
	_ = (&linuxDesktop{conn: conn}).notifyStartupFailure(ctx, message)
}

// ConfirmStartupRepair never asks on Linux. A desktop notification can't take
// an answer reliably, and many installs are headless services, so startup
// fails with a message that says how to start Replicaro to repair it.
func ConfirmStartupRepair(string) (answered, repair bool) { return false, false }

// notifyStartupFailure sends the notification without actions, since there's
// no running Replicaro to open. It uses the server's default expiry (-1)
// rather than 0 (never expire): the systemd user unit restarts Replicaro on
// failure, so a lasting problem sends one notice per attempt, and those should
// not pile up until the user dismisses each one. The icon comes from the
// embedded image only: the file-based icon is written under the data folder,
// which may be the reason startup failed.
func (desktop *linuxDesktop) notifyStartupFailure(ctx context.Context, message string) error {
	notifications := desktop.conn.Object(notificationService, notificationPath)
	// Startup errors quote option syntax such as --loopback-port=<port>.
	message = notificationBody(ctx, notifications, message)
	hints := map[string]dbus.Variant{"desktop-entry": dbus.MakeVariant("replicaro")}
	if image, ok := desktopNotificationImage("failed"); ok {
		hints["image-data"] = dbus.MakeVariant(image)
	}
	return notifications.CallWithContext(
		ctx, notificationService+".Notify", 0,
		"Replicaro", uint32(0), "replicaro", "Replicaro", message, []string{},
		hints, int32(-1),
	).Err
}

// notificationBody prepares a message for the notification server. A server
// with the "body-markup" capability reads the body as markup, so a vault, job
// or file name with "&" or "<", or an option such as --port=<port>, would be
// read as an entity or tag and shown wrongly or dropped. The server is asked
// on every notification rather than once, because another server can take
// over the name while Replicaro runs. The summary is never markup.
func notificationBody(ctx context.Context, notifications dbus.BusObject, message string) string {
	var capabilities []string
	if call := notifications.CallWithContext(ctx, notificationService+".GetCapabilities", 0); call.Err == nil {
		_ = call.Store(&capabilities)
	}
	return freedesktopBodyText(message, slices.Contains(capabilities, "body-markup"))
}

// freedesktopBodyText escapes the three characters the notification markup
// gives meaning to when the server reads markup, and leaves the text alone
// otherwise, since a plain-text server shows the body as it is.
func freedesktopBodyText(message string, markup bool) string {
	if !markup {
		return message
	}
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(message)
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
	command := exec.Command("xdg-open", target)
	// xdg-open can start a new browser rather than hand the URL to a running
	// one. In its own session, a browser started here isn't in Replicaro's
	// process group, so Ctrl+C on Replicaro in a terminal doesn't reach it.
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	return startAndReap(command)
}

func startAndReap(command *exec.Cmd) error {
	if err := command.Start(); err != nil {
		return err
	}
	go func() { _ = command.Wait() }()
	return nil
}

// OpenWebUI opens a running instance's web interface from a second launch,
// which has no tray or D-Bus connection of its own yet. With a session bus it
// asks the desktop portal, like Run does on a first launch. When the launcher
// provided an XDG_ACTIVATION_TOKEN, it's passed through to the portal (and
// xdg-open, which the portal call falls back to, inherits the variable);
// whether the browser actually gets focus is up to the compositor. A token
// over maxActivationTokenBytes isn't sent to the portal, the same limit the
// tray path applies.
//
// Without a session bus it uses xdg-open, but only in a graphical session,
// the same check Run makes on a first launch. Over SSH or on a headless
// install there's no browser to show, and xdg-open would start and fail in
// the background while this launch reported success; returning an error
// makes the second launch exit non-zero after it has logged the URL. A
// missing xdg-open is reported the same way.
func OpenWebUI(target string) error {
	if _, err := url.ParseRequestURI(target); err != nil {
		return err
	}
	if os.Getenv("DBUS_SESSION_BUS_ADDRESS") != "" {
		if conn, err := dbus.ConnectSessionBus(); err == nil {
			defer conn.Close()
			token := os.Getenv("XDG_ACTIVATION_TOKEN")
			if len(token) > maxActivationTokenBytes {
				token = ""
			}
			opener := &linuxDesktop{conn: conn, activationToken: token}
			return opener.openURLWithFallback(target, openURLFallback)
		}
	}
	if !graphicalLinuxSession() {
		return errors.New("no graphical session to open the web interface in")
	}
	return openURLFallback(target)
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
