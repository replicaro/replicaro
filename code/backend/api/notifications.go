package api

import (
	"database/sql"
	"strconv"
	"strings"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/locale"
	"github.com/local/replicaro/notifications"
)

var getNotificationSettings = database.GetSettings

func prepareOperationNotification(db *sql.DB, operationID, kind, title, status string, selectedItemsCount ...int) func() {
	settings, err := getNotificationSettings(db)
	if err != nil {
		_ = database.SkipOperationStep(db, operationID, "application", "notification",
			"notification settings unavailable: "+err.Error(), time.Now())
		return nil
	}

	success := status == "success"
	nativeEnabled, webhookEnabled := settings.NotificationChannels(success)
	event := notifications.Event{
		Event:       kind,
		Status:      status,
		Success:     success,
		Title:       title,
		OperationID: operationID,
	}
	if !notifications.ShouldNotify(event) || !nativeEnabled && !webhookEnabled {
		return nil
	}
	event.Locale = locale.Effective(settings.Language)
	switch kind {
	case "backup":
		if name, ok := strings.CutPrefix(title, "Backup: "); ok {
			event.TaskKey, event.TaskName = "notifications.task.backup", name
		}
	case "check":
		if name, ok := strings.CutPrefix(title, "Check: "); ok {
			event.TaskKey, event.TaskName = "notifications.task.check", name
		}
	case "delete":
		if name, ok := strings.CutPrefix(title, "Delete snapshot: "); ok {
			event.TaskKey, event.TaskName = "notifications.task.deleteSnapshot", name
		}
	case "restore":
		// File History supplies the count explicitly. The operation title and
		// native results stay unchanged; only notification prose is localized.
		if len(selectedItemsCount) == 1 {
			event.TaskKey, event.TaskName = "notifications.task.restoreSelectedItems", strconv.Itoa(selectedItemsCount[0])
		} else if name, ok := strings.CutPrefix(title, "Restore snapshot: "); ok {
			event.TaskKey, event.TaskName = "notifications.task.restoreSnapshot", name
		}
	}
	if err := database.StartOperationStep(db, operationID, "application", "notification", time.Now()); err != nil {
		_ = database.LogError(db, "Notification step could not be registered: "+err.Error())
		return nil
	}

	// Registration precedes terminal operation visibility; dispatch begins only
	// after the caller confirms terminal persistence.
	return func() {
		notifications.Dispatch(settings.WebhookURL, nativeEnabled, webhookEnabled, event, func(err error) {
			stepStatus, result := "succeeded", "notification delivery completed"
			if err != nil {
				stepStatus, result = "warning", err.Error()
				_ = database.LogError(db, "Notification failed: "+err.Error())
			}
			_ = database.FinishOperationStep(db, operationID, "notification", stepStatus, result, time.Now())
		})
	}
}
