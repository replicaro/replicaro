package api

type SaveSettingsRequest struct {
	DefaultEngine                string  `json:"defaultEngine"`
	AutoStart                    bool    `json:"autoStart"`
	LogRetentionDays             int     `json:"logRetentionDays"`
	Theme                        string  `json:"theme"`
	Language                     *string `json:"language"`
	WebhookURL                   string  `json:"webhookUrl"`
	NotifyWindowsOnSuccess       bool    `json:"notifyWindowsOnSuccess"`
	NotifyWindowsOnFailure       bool    `json:"notifyWindowsOnFailure"`
	NativeNotificationsOnSuccess bool    `json:"nativeNotificationsOnSuccess"`
	NativeNotificationsOnFailure bool    `json:"nativeNotificationsOnFailure"`
	NotifyWebhookOnSuccess       bool    `json:"notifyWebhookOnSuccess"`
	NotifyWebhookOnFailure       bool    `json:"notifyWebhookOnFailure"`

	StartWithWindows             bool `json:"startWithWindows"`
	StartAtLogin                 bool `json:"startAtLogin"`
	MinimizeToTray               bool `json:"minimizeToTray"`
	MaxConcurrentJobRuns         int  `json:"maxConcurrentJobRuns"`
	DisableAutomaticUpdateChecks bool `json:"disableAutomaticUpdateChecks"`
}
