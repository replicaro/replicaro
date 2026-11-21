package desktop

import (
	"bytes"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"image/color"
	"log"
	"sync"
	"sync/atomic"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/models"
)

// trayState is what the tray or menu-bar icon shows. It follows the
// dashboard: running while anything is under "Active now", otherwise errors
// or warnings while the Issues tile shows unreviewed issues, otherwise idle.
// The numbering is shared with native_darwin.m.
type trayState int

const (
	trayIdle trayState = iota
	trayRunning
	trayWarnings
	trayErrors
)

// trayStateFor decides the state from the dashboard's two answers: whether
// "Active now" lists anything, and the Issues count with its warning subset
// (models.DashboardStats.FailedRuns and WarningRuns). Running comes first and
// errors before warnings, the same order as the dashboard's hero ring, so the
// icon is blue while work runs and goes back to red or amber afterwards while
// issues stay unreviewed. Errors are "at least one unreviewed issue is not a
// warning", exactly when the Issues tile is red. Do not add a definition of
// running or of an issue here: the tray must agree with the dashboard.
func trayStateFor(running bool, failedRuns, warningRuns int) trayState {
	switch {
	case running:
		return trayRunning
	case failedRuns > warningRuns:
		return trayErrors
	case failedRuns > 0:
		return trayWarnings
	}
	return trayIdle
}

// trayRunningColor is the blue of the running state. Warnings and errors use
// the notification icons' amber and red.
var trayRunningColor = color.NRGBA{R: 0x0e, G: 0xa5, B: 0xe9, A: 0xff}

// trayStateTint returns the colour the icon is repainted in for state. Idle
// has none: it shows Replicaro's own icon, unchanged.
func trayStateTint(state trayState) (color.NRGBA, bool) {
	switch state {
	case trayRunning:
		return trayRunningColor, true
	case trayWarnings:
		return notificationWarningColor, true
	case trayErrors:
		return notificationFailureColor, true
	}
	return color.NRGBA{}, false
}

var trayStateTextState struct {
	once  sync.Once
	texts [4]string
}

// trayStateText returns the tooltip for state. Like the menu labels, the four
// texts are read once, when the tray is set up, in the language saved then.
func trayStateText(state trayState) string {
	trayStateTextState.once.Do(func() {
		trayStateTextState.texts = trayStateTexts()
	})
	if state < trayIdle || state > trayErrors {
		state = trayIdle
	}
	return trayStateTextState.texts[state]
}

func trayStateTexts() [4]string {
	var texts [4]string
	copy(texts[:], desktopTexts(TextTrayIdle, TextTrayRunning, TextTrayWarnings, TextTrayErrors))
	return texts
}

// tintedICO repaints every image of an .ico file in tint, as tintPNG does for
// one PNG, and keeps the file's directory (sizes, order) as it is. Replicaro's
// .ico holds only PNG images; an image in the older BMP form is refused, and
// the caller then keeps the icon it has.
func tintedICO(source []byte, tint color.NRGBA) ([]byte, error) {
	if len(source) < 6 || binary.LittleEndian.Uint16(source[0:]) != 0 || binary.LittleEndian.Uint16(source[2:]) != 1 {
		return nil, errors.New("not an icon file")
	}
	count := int(binary.LittleEndian.Uint16(source[4:]))
	directoryEnd := 6 + 16*count
	if count == 0 || len(source) < directoryEnd {
		return nil, errors.New("icon file has no complete directory")
	}
	images := make([][]byte, count)
	for index := range count {
		entry := source[6+16*index : 6+16*(index+1)]
		size := uint64(binary.LittleEndian.Uint32(entry[8:]))
		offset := uint64(binary.LittleEndian.Uint32(entry[12:]))
		if offset < uint64(directoryEnd) || offset+size > uint64(len(source)) {
			return nil, fmt.Errorf("icon image %d is outside the file", index)
		}
		tinted, err := tintPNG(source[offset:offset+size], tint)
		if err != nil {
			return nil, fmt.Errorf("icon image %d: %w", index, err)
		}
		images[index] = tinted
	}
	var output bytes.Buffer
	output.Write(source[:6])
	offset := directoryEnd
	for index, image := range images {
		entry := bytes.Clone(source[6+16*index : 6+16*(index+1)])
		binary.LittleEndian.PutUint32(entry[8:], uint32(len(image)))
		binary.LittleEndian.PutUint32(entry[12:], uint32(offset))
		output.Write(entry)
		offset += len(image)
	}
	for _, image := range images {
		output.Write(image)
	}
	return output.Bytes(), nil
}

// The tray reads its state on demand, never on a timer. The database calls
// DashboardChanged after every write that can change the dashboard's "Active
// now" list or Issues count (see database.NotifyDashboardChanges); the tray
// then reads the dashboard's own two queries once and sets the icon. Every
// answer of /api/dashboard and /api/operations/active also sets the tray (see
// UpdateTrayFromDashboard and UpdateTrayFromActiveOperations). The dashboard
// asks for both while it is open, so the two always agree then; the Protect
// and Restore pages also ask for /api/operations/active, and their answers
// set the running half the same way. Nothing here is saved.
//
// One Issues change has no notice: a Kopia policy process found stuck is an
// issue worked out from saved state and the clock when the dashboard query
// runs, so no write marks the moment it becomes one, and the writes of Kopia
// policy state are deliberately not hooked. No timer covers it either: it is
// rare, and the tray catches up at the next change of any kind or at once
// from a dashboard answer. Do not hook those writes or add a timer for it
// without a product decision.
//
// The two halves of the state are kept separately because the dashboard asks
// for them separately: the "Active now" answer only says whether anything is
// running, the dashboard answer only gives the issue counts. Each answer
// replaces its own half and the state is worked out again from both.
var trayStatus struct {
	sync.Mutex
	db *sql.DB
	// show sets the platform icon and tooltip. It is nil while no tray runs,
	// which makes every notice and answer do nothing. It is called with the
	// lock held, so states reach the platform in the order they were decided.
	// On Windows it records the state and posts a message to the tray's
	// thread. On macOS it recolours the icon, then hands the state and image
	// to the main thread. On Linux it recolours the icon, sets the item's
	// D-Bus properties and writes the NewIcon and NewToolTip signals to the
	// session bus before it returns; those writes wait only on the bus
	// daemon, never on the tray host. The lock is also taken by the
	// /api/dashboard and /api/operations/active handlers, so nothing slow or
	// blocking may be added to any platform's show.
	show                    func(trayState)
	running                 bool
	failedRuns, warningRuns int
	shown                   trayState
}

// trayNotices counts DashboardChanged calls. A request handler reads it before
// its query and the tray takes the answer only if no change was announced
// while the query ran (see UpdateTrayFromActiveOperations).
var trayNotices atomic.Uint64

// trayWake holds at most one pending notice. A notice that finds it full
// adds nothing: the pending one already makes the tray read after this
// change. A notice that arrives while the tray is reading fills it again,
// so the tray reads once more and the last change is always shown.
var trayWake = make(chan struct{}, 1)

// The two queries the dashboard uses. Tests replace them to make a read fail.
var (
	readActiveOperations = database.ListActiveOperations
	readDashboardStats   = database.GetDashboardStats
)

// WatchDashboard gives the tray the database it reads its state from. Call it
// before Run.
func WatchDashboard(db *sql.DB) {
	trayStatus.Lock()
	trayStatus.db = db
	trayStatus.Unlock()
}

// DashboardChanged tells the tray that the dashboard's "Active now" list or
// Issues count may have changed. Operation and activity-log writers call it
// (through database.NotifyDashboardChanges) right after their write, so it
// never blocks and never touches the database: the tray reads on its own
// goroutine. Without a tray it does nothing.
func DashboardChanged() {
	trayNotices.Add(1)
	select {
	case trayWake <- struct{}{}:
	default:
	}
}

// startTrayState is called by the platform's Run once its tray icon exists,
// with the function that shows a state on it. It reads the state once and
// then again after each notice, until the returned stop is called. The
// platform's icon starts as idle, Replicaro's own teal icon with the idle
// tooltip.
func startTrayState(show func(trayState)) (stop func()) {
	trayStatus.Lock()
	trayStatus.show = show
	trayStatus.shown = trayIdle
	trayStatus.Unlock()
	done := make(chan struct{})
	go func() {
		readTrayState()
		for {
			select {
			case <-trayWake:
				readTrayState()
			case <-done:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			trayStatus.Lock()
			trayStatus.show = nil
			trayStatus.Unlock()
			close(done)
		})
	}
}

// readTrayState reads the dashboard's two queries and shows the state. When
// either read fails the icon keeps the state it shows; a tray problem must
// never become an operation's or the database's problem.
func readTrayState() {
	trayStatus.Lock()
	db := trayStatus.db
	trayStatus.Unlock()
	if db == nil {
		return
	}
	active, err := readActiveOperations(db)
	if err != nil {
		log.Printf("Replicaro could not read its active operations for the tray icon and keeps the icon as it is: %v", err)
		return
	}
	stats, err := readDashboardStats(db)
	if err != nil {
		log.Printf("Replicaro could not read the dashboard issue count for the tray icon and keeps the icon as it is: %v", err)
		return
	}
	trayStatus.Lock()
	defer trayStatus.Unlock()
	trayStatus.running = len(active) > 0
	trayStatus.failedRuns, trayStatus.warningRuns = stats.FailedRuns, stats.WarningRuns
	showTrayStateLocked()
}

func showTrayStateLocked() {
	state := trayStateFor(trayStatus.running, trayStatus.failedRuns, trayStatus.warningRuns)
	if trayStatus.show == nil || state == trayStatus.shown {
		return
	}
	trayStatus.shown = state
	trayStatus.show(state)
}

// TrayNotices returns the number of dashboard change notices so far. The
// handlers of /api/operations/active and /api/dashboard take it before their
// query and pass it to UpdateTrayFromActiveOperations or
// UpdateTrayFromDashboard with the answer.
func TrayNotices() uint64 {
	return trayNotices.Load()
}

// UpdateTrayFromActiveOperations sets the running half of the tray state from
// the "Active now" answer the API is about to send, so the tray agrees with
// an open dashboard without reading anything itself. noticesBefore is
// TrayNotices from before the query. When a change was announced while the
// query ran, the answer may predate it and is not used: the tray's own read
// for that notice comes after the change. Without this check, an answer read
// just before a backup finished could land after the tray's read for the
// finish and leave the icon blue until the next change.
func UpdateTrayFromActiveOperations(noticesBefore uint64, active []models.Operation) {
	trayStatus.Lock()
	defer trayStatus.Unlock()
	if trayStatus.show == nil || trayNotices.Load() != noticesBefore {
		return
	}
	trayStatus.running = len(active) > 0
	showTrayStateLocked()
}

// UpdateTrayFromDashboard sets the issues half of the tray state from the
// dashboard answer the API is about to send. It also brings in what has no
// notice of its own, such as a Kopia policy process found stuck. noticesBefore
// works as for UpdateTrayFromActiveOperations.
func UpdateTrayFromDashboard(noticesBefore uint64, stats models.DashboardStats) {
	trayStatus.Lock()
	defer trayStatus.Unlock()
	if trayStatus.show == nil || trayNotices.Load() != noticesBefore {
		return
	}
	trayStatus.failedRuns, trayStatus.warningRuns = stats.FailedRuns, stats.WarningRuns
	showTrayStateLocked()
}
