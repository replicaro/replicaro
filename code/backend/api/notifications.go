package api

import (
	"database/sql"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/notifications"
)

var getNotificationSettings = database.GetSettings

func prepareOperationNotification(db *sql.DB, operationID, kind, title, status string) func() {
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
