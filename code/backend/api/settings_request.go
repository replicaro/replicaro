package api

import "github.com/local/replicaro/notifications"

type SaveSettingsRequest struct {
	DefaultEngine                string  `json:"defaultEngine"`
	LogRetentionDays             int     `json:"logRetentionDays"`
	Theme                        string  `json:"theme"`
	Language                     *string `json:"language"`
	WebhookURL                   string  `json:"webhookUrl"` // ignored, see NotificationURLs
	NotifyWindowsOnSuccess       bool    `json:"notifyWindowsOnSuccess"`
	NotifyWindowsOnFailure       bool    `json:"notifyWindowsOnFailure"`
	NativeNotificationsOnSuccess bool    `json:"nativeNotificationsOnSuccess"`
	NativeNotificationsOnFailure bool    `json:"nativeNotificationsOnFailure"`
	NotifyWebhookOnSuccess       bool    `json:"notifyWebhookOnSuccess"`
	NotifyWebhookOnFailure       bool    `json:"notifyWebhookOnFailure"`

	StartWithWindows             bool `json:"startWithWindows"`
	StartAtLogin                 bool `json:"startAtLogin"`
	MaxConcurrentJobRuns         int  `json:"maxConcurrentJobRuns"`
	DisableAutomaticUpdateChecks bool `json:"disableAutomaticUpdateChecks"`

	// NotificationURLs replaces the stored list when present. A page opened
	// before the upgrade doesn't send it, and then the stored list stays as it
	// is. Such a page still sends webhookUrl, which is accepted and ignored:
	// the stored webhookUrl is always derived from this list.
	NotificationURLs *[]string `json:"notificationUrls"`

	// The title prefixes replace the stored ones when present, after
	// notifications.CleanTitlePrefix. Like NotificationURLs they are pointers
	// because a page opened before the upgrade doesn't send them, and its save
	// must keep the stored prefixes rather than clear them.
	DesktopNotificationTitlePrefix *string `json:"desktopNotificationTitlePrefix"`
	NotificationURLTitlePrefix     *string `json:"notificationUrlTitlePrefix"`

	// Accepted and ignored. Nothing acts on these preferences, but an older
	// page or client still sends them with the rest of its settings document,
	// and the strict decoder would otherwise refuse the whole save.
	IgnoredAutoStart      bool `json:"autoStart"`
	IgnoredMinimizeToTray bool `json:"minimizeToTray"`
}

// cleanSentTitlePrefix cleans and checks a title prefix sent with a settings
// save. A prefix that wasn't sent stays nil, which keeps the stored one.
func cleanSentTitlePrefix(sent *string) (*string, error) {
	if sent == nil {
		return nil, nil
	}
	cleaned, err := notifications.CleanTitlePrefix(*sent)
	if err != nil {
		return nil, err
	}
	return &cleaned, nil
}
