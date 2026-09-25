package notifications

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/local/replicaro/desktop"
	"github.com/local/replicaro/locale"
)

type Event struct {
	Locale                string `json:"-"`
	TaskKey               string `json:"-"`
	TaskName              string `json:"-"`
	TaskTarget            string `json:"-"`
	Event                 string `json:"event"`
	Status                string `json:"status,omitempty"`
	Success               bool   `json:"success"`
	NativeBackupSucceeded bool   `json:"nativeBackupSucceeded,omitempty"`
	Title                 string `json:"title"`
	Message               string `json:"message"`
	OperationID           string `json:"operationId,omitempty"`
	TimelineURL           string `json:"timelineUrl,omitempty"`
	Severity              string `json:"severity"`
	Color                 string `json:"color"`
	Icon                  string `json:"icon"`
	Timestamp             string `json:"timestamp"`
}

var dispatchWG sync.WaitGroup
var showDesktopNotification = desktop.ShowNotification

const maxWebhookMessageBytes = 16 << 10
const maxWebhookResponseBytes = 64 << 10

var webhookHTTPClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 3 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: 5 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		MaxIdleConns:          10,
		MaxIdleConnsPerHost:   2,
	},
	Timeout: 10 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return errors.New("webhook redirect is not permitted")
	},
}

// Dispatch gives asynchronous notification delivery explicit ownership. Send
// itself has a bounded ten-second network context, and Wait lets application
// and test shutdown drain the owned work before closing the database.
func Dispatch(webhookURL string, nativeEnabled, webhookEnabled bool, event Event, onError func(error)) {
	dispatchWG.Add(1)
	go func() {
		defer dispatchWG.Done()
		err := SendAll(webhookURL, nativeEnabled, webhookEnabled, event)
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
func SendAll(webhookURL string, nativeEnabled, webhookEnabled bool, event Event) error {
	if !ShouldNotify(event) {
		return nil
	}
	prepare(&event)
	var nativeErr, webhookErr error
	if nativeEnabled {
		nativeErr = showDesktopNotification(event.Title, event.Message, event.OperationID, notificationStatus(event))
	}
	if webhookEnabled {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		webhookErr = sendContext(ctx, webhookURL, event)
	}
	return errors.Join(nativeErr, webhookErr)
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
	if len(event.Message) > maxWebhookMessageBytes {
		event.Message = event.Message[:maxWebhookMessageBytes] + "…"
	}
	event.Title = localizedNotificationTitle(*event)
}

func localizedNotificationTitle(event Event) string {
	key := "notifications.title.failed"
	switch notificationStatus(event) {
	case "success":
		key = "notifications.title.success"
	case "completed_with_issues":
		key = "notifications.title.completedWithIssues"
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

func Send(webhookURL string, event Event) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return SendContext(ctx, webhookURL, event)
}

// SendContext is the lifecycle-aware delivery seam used by shutdown tests and
// callers that own a stricter deadline.
func SendContext(ctx context.Context, webhookURL string, event Event) error {
	prepare(&event)
	return sendContext(ctx, webhookURL, event)
}

func sendContext(ctx context.Context, webhookURL string, event Event) error {
	if webhookURL == "" {
		return nil
	}
	body, err := json.Marshal(event)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return errors.New("invalid webhook configuration")
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := webhookHTTPClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return fmt.Errorf("webhook delivery stopped: %w", ctx.Err())
		}
		if strings.Contains(err.Error(), "redirect is not permitted") {
			return errors.New("webhook redirect is not permitted")
		}
		return errors.New("webhook delivery failed")
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxWebhookResponseBytes))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("webhook returned %s", response.Status)
	}
	return nil
}
