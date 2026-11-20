package desktop

import (
	"fmt"
	"net/url"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/local/replicaro/models"
	"github.com/local/replicaro/runtimeendpoint"
)

var endpointState = struct {
	sync.RWMutex
	endpoint   runtimeendpoint.Endpoint
	configured bool
}{endpoint: runtimeendpoint.Preferred()}

const replicaroUpdateURL = "https://replicaro.com/update"

// Catalog keys of the tray menu labels, the tray icon's tooltips and the Quit
// prompt. They live in the web UI's locale catalogs like the rest of the
// text; main supplies the lookup (SetText) so they follow the saved UI
// language.
const (
	TextTrayOpen     = "desktop.trayOpen"
	TextTrayQuit     = "desktop.trayQuit"
	TextQuitPrompt   = "desktop.quitPrompt"
	TextQuitButton   = "desktop.quitButton"
	TextCancelButton = "desktop.cancelButton"
	TextTrayIdle     = "desktop.trayIdle"
	TextTrayRunning  = "desktop.trayRunning"
	TextTrayWarnings = "desktop.trayWarnings"
	TextTrayErrors   = "desktop.trayErrors"
	// The second line of the Linux tray tooltip, under the state text.
	TextTrayDescription = "desktop.trayDescription"
)

// englishText is used until main sets a lookup, and whenever the lookup has
// no text for a key. It must match en.json; a test checks that.
var englishText = map[string]string{
	TextTrayOpen:        "Open Replicaro",
	TextTrayQuit:        "Quit Replicaro",
	TextQuitPrompt:      "Quit Replicaro? Backups and other work won't run until you start Replicaro again.",
	TextQuitButton:      "Quit",
	TextCancelButton:    "Cancel",
	TextTrayIdle:        "Replicaro · Idle",
	TextTrayRunning:     "Replicaro · Running jobs",
	TextTrayWarnings:    "Replicaro · Warnings to review",
	TextTrayErrors:      "Replicaro · Errors to review",
	TextTrayDescription: "Backup companion",
}

var textLookup struct {
	sync.RWMutex
	resolve func() func(key string) string
}

// SetText sets how the tray labels, the tray tooltips and the Quit prompt get
// their text: resolve reads the saved UI language and returns a lookup for it.
// Call it before Run: the tray labels and tooltips are read once, when the
// tray is set up, so a language change shows in them after the next start. The Quit prompt
// resolves the language each time it opens, once for all of its texts:
// resolving the "system" language can run an OS command (on macOS, `defaults
// read`), and the macOS prompt is built on the main thread.
func SetText(resolve func() func(key string) string) {
	textLookup.Lock()
	textLookup.resolve = resolve
	textLookup.Unlock()
}

// desktopTexts returns the texts for keys, all in the language resolved once
// for this call, with English for any key the lookup has no text for.
func desktopTexts(keys ...string) []string {
	textLookup.RLock()
	resolve := textLookup.resolve
	textLookup.RUnlock()
	var lookup func(key string) string
	if resolve != nil {
		lookup = resolve()
	}
	values := make([]string, len(keys))
	for index, key := range keys {
		values[index] = englishText[key]
		if lookup == nil {
			continue
		}
		// The catalog lookup returns the key itself for a missing text.
		if value := strings.TrimSpace(lookup(key)); value != "" && value != key {
			values[index] = value
		}
	}
	return values
}

func desktopText(key string) string {
	return desktopTexts(key)[0]
}

var trayLabelState struct {
	once       sync.Once
	open, quit string
}

// trayLabels returns the Open and Quit menu labels, read once at the first
// call (when the tray or menu bar item is set up) and kept for the run.
func trayLabels() (open, quit string) {
	trayLabelState.once.Do(func() {
		labels := desktopTexts(TextTrayOpen, TextTrayQuit)
		trayLabelState.open, trayLabelState.quit = labels[0], labels[1]
	})
	return trayLabelState.open, trayLabelState.quit
}

var openDefaultBrowser = openWebUIURL

// noStartupBrowserOption is the startup option that keeps Replicaro from
// opening the web interface by itself when it starts. Start at login passes
// it, so signing in shows only the tray or menu-bar icon, as with most tray
// apps, instead of a browser window every time; the icon stays the way in.
const noStartupBrowserOption = "--no-startup-browser"

var noStartupBrowser atomic.Bool

// SetNoStartupBrowser records whether this process was started with
// --no-startup-browser. Call it before Run. It only skips the open Run does
// by itself at startup: the tray icon, the tray and menu-bar Open item, and
// on macOS reopening the running app still open the web interface.
func SetNoStartupBrowser(skip bool) {
	noStartupBrowser.Store(skip)
}

// NoStartupBrowser reports what SetNoStartupBrowser recorded. A second launch
// reads it to decide whether to open the running instance's web interface.
func NoStartupBrowser() bool {
	return noStartupBrowser.Load()
}

func ConfigureEndpoint(endpoint runtimeendpoint.Endpoint) error {
	if _, err := runtimeendpoint.ParseExact(endpoint.String()); err != nil {
		return err
	}
	endpointState.Lock()
	defer endpointState.Unlock()
	if endpointState.configured && endpointState.endpoint.String() != endpoint.String() {
		return fmt.Errorf("desktop runtime endpoint is already configured")
	}
	endpointState.endpoint = endpoint
	endpointState.configured = true
	return nil
}

func currentWebUIURL() string {
	endpointState.RLock()
	defer endpointState.RUnlock()
	return endpointState.endpoint.WebURL()
}

func TimelineURL(operationID string) string {
	target := currentWebUIURL()
	if operationID != "" {
		target += "?timelineOperation=" + url.QueryEscape(operationID)
	}
	return target
}

func ReplicaroReleaseURL(version string) (string, error) {
	return releaseURLForPlatform(version, runtime.GOOS, runtime.GOARCH)
}

func releaseURLForPlatform(version, operatingSystem, architecture string) (string, error) {
	if _, err := models.ParseSemanticVersion(version); err != nil {
		return "", err
	}
	if operatingSystem == "darwin" {
		operatingSystem = "macos"
	}
	return replicaroUpdateURL + "?os=" + url.QueryEscape(operatingSystem) + "&arch=" + url.QueryEscape(architecture), nil
}

func OpenReplicaroRelease(version string) error {
	target, err := ReplicaroReleaseURL(version)
	if err != nil {
		return err
	}
	return openDefaultBrowser(target)
}

// OpenURL opens target in the default browser with the opener the running
// instance uses for its own web interface. The browser is started through the
// platform's URL handler on Replicaro's behalf, so it never belongs to an
// engine's or rclone's process tree and ending one of those never closes it.
//
// Callers pass a URL they have already validated. The platform handlers open
// whatever they're given; on Windows, FileProtocolHandler can also open
// local files.
func OpenURL(target string) error {
	return openDefaultBrowser(target)
}

// finishWithin runs stop on its own goroutine and waits at most limit for it
// to return. It reports whether stop finished in time; when it didn't, stop
// keeps running. stop's error is dropped here because stop is the
// application's staged shutdown, whose caller gets the same result once Run
// returns. Where Run doesn't return (macOS), the stop passed in logs it.
func finishWithin(stop func() error, limit time.Duration) bool {
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = stop()
	}()
	timer := time.NewTimer(limit)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}

// toastXMLText drops the characters XML 1.0 does not allow anywhere (the C0
// control characters other than tab, line feed and carriage return, and
// U+FFFE and U+FFFF). Escaping cannot write them, and a single one in a
// title or message, for example from a file name, makes Windows reject the
// whole toast document. Only the Windows toast uses it; it has no build tag so
// its test runs everywhere.
func toastXMLText(value string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' || r == '\n' || r == '\r' {
			return r
		}
		if r < 0x20 || r == 0xFFFE || r == 0xFFFF {
			return -1
		}
		return r
	}, value)
}
