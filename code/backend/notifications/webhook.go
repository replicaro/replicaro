package notifications

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/desktop"
	"github.com/local/replicaro/locale"
	"github.com/local/replicaro/models"
)

type Event struct {
	Locale     string `json:"-"`
	TaskKey    string `json:"-"`
	TaskName   string `json:"-"`
	TaskTarget string `json:"-"`
	// MessageKey optionally replaces the generic status message with one
	// specific catalog message (the 30-day unavailable-storage notices, the
	// vault reconnect message, the Kopia out-of-space notice, and the locked
	// files message). MessageValues fills its placeholders; {taskName} is
	// filled with the localized task name unless MessageValues sets it.
	MessageKey            string            `json:"-"`
	MessageValues         map[string]string `json:"-"`
	Event                 string            `json:"event"`
	Status                string            `json:"status,omitempty"`
	IssueKind             string            `json:"issueKind,omitempty"` // only with status completed_with_issues; see models.OperationIssueLockedFiles
	Success               bool              `json:"success"`
	NativeBackupSucceeded bool              `json:"nativeBackupSucceeded,omitempty"`
	Title                 string            `json:"title"`
	Message               string            `json:"message"`
	OperationID           string            `json:"operationId,omitempty"`
	TimelineURL           string            `json:"timelineUrl,omitempty"`
	Severity              string            `json:"severity"`
	Color                 string            `json:"color"`
	Icon                  string            `json:"icon"`
	Timestamp             string            `json:"timestamp"`
}

var dispatchWG sync.WaitGroup
var showDesktopNotification = desktop.ShowNotification

const maxWebhookMessageBytes = 16 << 10
const maxWebhookResponseBytes = 64 << 10

// webhookHTTPClient is the one client every notification request goes through.
//
// The only time limit on a notification is deliveryTimeout, one overall
// deadline that deliver puts on the context of every request. The client has
// no dial, TLS handshake or response header timeouts: a limit on one step
// would cut a slow but healthy server short (a slow proxy, a slow handshake, a
// receiver that does its work before it answers) while the notification still
// had time left. Client.Timeout stays unset as well; it restarts for every
// redirect hop that do() follows and adds nothing to the context deadline.
//
// Without step timeouts a dial that nothing waits for any more can hang, so
// deliver calls CloseIdleConnections once a notification is done.
//
// Redirects are never followed by net/http; do() decides (see redirectTarget).
var webhookHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{KeepAlive: 30 * time.Second}).DialContext,
		IdleConnTimeout:     30 * time.Second,
		MaxIdleConns:        10,
		MaxIdleConnsPerHost: 2,
	},
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

// Dispatch gives asynchronous notification delivery explicit ownership. Every
// delivery is bounded by the ten-second deliveryTimeout, and Wait lets
// application and test shutdown drain the owned work before closing the
// database.
func Dispatch(urls []string, nativeEnabled, urlsEnabled bool, event Event, onError func(error)) {
	dispatchWG.Add(1)
	go func() {
		defer dispatchWG.Done()
		err := SendAll(urls, nativeEnabled, urlsEnabled, event)
		if onError != nil {
			onError(err)
		}
	}()
}

func Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		dispatchWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// PrepareOperation registers the notification step of a finished operation
// and returns the function that sends its notification, or nil when there is
// nothing to send. The step is registered before the operation is shown as
// finished, and the caller runs the returned function only after it has saved
// the operation's final state, so the running step is the whole handoff and no
// other durable state is needed.
//
// event builds the notification from the saved settings. It runs only when a
// channel is on for status, so any lookups it does are skipped otherwise.
func PrepareOperation(
	db *sql.DB,
	operationID, status string,
	loadSettings func(*sql.DB) (models.Settings, error),
	event func(settings models.Settings) Event,
) func() {
	settings, err := loadSettings(db)
	if err != nil {
		_ = database.SkipOperationStep(db, operationID, "application", "notification",
			"notification settings unavailable: "+err.Error(), time.Now())
		return nil
	}
	nativeEnabled, urlsEnabled := settings.NotificationChannels(status)
	if !nativeEnabled && !urlsEnabled {
		return nil
	}
	notification := event(settings)
	if !ShouldNotify(notification) {
		return nil
	}
	if err := database.StartOperationStep(db, operationID, "application", "notification", time.Now()); err != nil {
		_ = database.LogError(db, "Notification step could not be registered: "+err.Error())
		return nil
	}
	return func() {
		Dispatch(settings.NotificationURLs, nativeEnabled, urlsEnabled, notification, func(err error) {
			stepStatus, result := "succeeded", "notification delivery completed"
			if err != nil {
				stepStatus, result = "warning", err.Error()
				_ = database.LogError(db, "Notification failed: "+err.Error())
			}
			_ = database.FinishOperationStep(db, operationID, "notification", stepStatus, result, time.Now())
		})
	}
}

// ShouldNotify keeps successful notifications focused on the user-visible
// operation types while still reporting every failed operation shown as an
// Issue on the dashboard.
func ShouldNotify(event Event) bool {
	if notificationStatus(event) != "success" {
		return true
	}
	switch event.Event {
	case "backup", "restore", "check", "maintenance":
		return true
	default:
		return false
	}
}

// SendAll delivers an operation event to the selected notification channels.
// The notification URLs are sent in parallel under one ten-second budget, and
// the error has one line per failed URL (and one for the desktop
// notification), in list order.
func SendAll(urls []string, nativeEnabled, urlsEnabled bool, event Event) error {
	if !ShouldNotify(event) {
		return nil
	}
	prepare(&event)
	var failures []error
	if nativeEnabled {
		failures = append(failures, showDesktopNotification(event.Title, event.Message, event.OperationID, notificationStatus(event)))
	}
	if urlsEnabled {
		for _, outcome := range deliver(context.Background(), CleanURLs(urls), event) {
			failures = append(failures, outcome.err)
		}
	}
	return errors.Join(failures...)
}

func prepare(event *Event) {
	status := notificationStatus(*event)
	if event.Status == "" {
		event.Status = status
	}
	event.Success = status == "success"
	event.Severity, event.Color, event.Icon = "error", "#f0776f", "replicaro-failure"
	if status == "success" {
		event.Severity, event.Color, event.Icon = "success", "#1a8a9e", "replicaro-success"
	}
	if status == "completed_with_issues" {
		event.Severity, event.Color, event.Icon = "warning", "#f2c063", "replicaro-warning"
	} else {
		// The qualifier only narrows completed_with_issues. Webhook consumers
		// never see it next to any other status.
		event.IssueKind = ""
	}
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339)
	}
	if event.OperationID != "" {
		event.TimelineURL = desktop.TimelineURL(event.OperationID)
	} else {
		event.TimelineURL = ""
	}
	taskName := strings.TrimSpace(event.Title)
	if event.TaskKey != "" {
		taskName = locale.Text(event.Locale, event.TaskKey, map[string]string{
			"name": event.TaskName, "target": event.TaskTarget,
		})
	}
	event.Message = localizedNotificationMessage(taskName, *event)
	if event.MessageKey != "" {
		values := map[string]string{"taskName": taskName}
		for name, value := range event.MessageValues {
			values[name] = value
		}
		event.Message = locale.Text(event.Locale, event.MessageKey, values)
	}
	if len(event.Message) > maxWebhookMessageBytes {
		// Cut at the start of a character so a multi-byte character is never
		// split into invalid UTF-8.
		cut := maxWebhookMessageBytes
		for cut > 0 && !utf8.RuneStart(event.Message[cut]) {
			cut--
		}
		event.Message = event.Message[:cut] + "…"
	}
	event.Title = localizedNotificationTitle(*event)
}

// testEvent is the event name of the settings page's "Send test"
// notification. Its JSON webhook body has event "test" and status "success".
const testEvent = "test"

func localizedNotificationTitle(event Event) string {
	if event.Event == testEvent {
		return locale.Text(event.Locale, "notifications.test.title", nil)
	}
	key := "notifications.title.failed"
	switch notificationStatus(event) {
	case "success":
		key = "notifications.title.success"
	case "completed_with_issues":
		key = "notifications.title.completedWithIssues"
		if event.IssueKind == models.OperationIssueLockedFiles {
			key = "notifications.title.completedLockedFilesSkipped"
		}
	}
	return locale.Text(event.Locale, key, nil)
}

func localizedNotificationMessage(taskName string, event Event) string {
	key := "notifications.message.failed"
	switch notificationStatus(event) {
	case "success":
		key = "notifications.message.success"
	case "completed_with_issues":
		key = "notifications.message.completedWithIssues"
		if event.Event == "backup" && event.NativeBackupSucceeded {
			key = "notifications.message.backupNativeSucceededWithIssues"
		}
	}
	return locale.Text(event.Locale, key, map[string]string{"taskName": taskName})
}

func notificationTitle(event Event) string {
	event.Locale = "en"
	return localizedNotificationTitle(event)
}

func notificationMessage(taskName string, event Event) string {
	event.Locale = "en"
	return localizedNotificationMessage(taskName, event)
}

func notificationStatus(event Event) string {
	switch event.Status {
	case "success", "completed_with_issues", "failed", "partial", "interrupted", "skipped", "reconnect_required":
		return event.Status
	}
	if event.Success {
		return "success"
	}
	return "failed"
}

// Send delivers event to one notification URL with the usual ten-second
// budget. An empty URL sends nothing.
func Send(notificationURL string, event Event) error {
	return SendContext(context.Background(), notificationURL, event)
}

// SendContext is the lifecycle-aware delivery seam used by shutdown tests and
// callers that own a stricter deadline. The ten-second budget still applies
// when ctx has a later deadline or none.
func SendContext(ctx context.Context, notificationURL string, event Event) error {
	if notificationURL == "" {
		return nil
	}
	prepare(&event)
	return deliver(ctx, []string{notificationURL}, event)[0].err
}

// The JSON webhook. http:// and https:// URLs get the Event JSON as it is:
// existing receivers parse it, so don't switch these URLs to another format
// or move them onto the shoutrrr-style senders.

type webhookSender struct{ url string }

func parseWebhook(u *url.URL, raw string) (sender, error) {
	if u.Host == "" {
		return nil, errors.New("missing host")
	}
	// The URL is used as typed; user info in it still becomes basic auth.
	return webhookSender{url: raw}, nil
}

func (s webhookSender) send(ctx context.Context, event Event) error {
	body, err := json.Marshal(event)
	if err != nil {
		return errDeliveryFailed
	}
	_, err = do(ctx, request{url: s.url, header: jsonHeader(), body: body})
	return err
}
