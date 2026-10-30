package models

const (
	LanguageSystem  = "system"
	LanguageEnglish = "en"
	ThemeSystem     = "system"
	ThemeLight      = "light"
	ThemeNeutral    = "neutral"
	ThemeDark       = "dark"
)

func ValidLanguage(value string) bool {
	switch value {
	case LanguageSystem, LanguageEnglish, "de", "fr", "ar", "ur", "hi", "es", "it",
		"zh-Hans", "zh-Hant", "yue-Hant", "yue-Hans", "ja", "ga", "ko", "ms", "id", "tr", "he", "pt-PT", "pt-BR", "ru", "pl", "nl", "bn", "el", "fa", "vi", "pcm":
		return true
	default:
		return false
	}
}

func ValidTheme(value string) bool {
	return value == ThemeSystem || value == ThemeLight || value == ThemeNeutral || value == ThemeDark
}

// Settings is both the stored preference set and the GET /api/settings body.
// EffectiveLocale and SystemLocale are read-only response values and are never
// stored; SaveSettingsRequest has no field for either, so a client that echoes
// them back is rejected by the strict decoder. SystemLocale is the catalog the
// "system" preference resolves to right now and is sent even when a specific
// language is saved, since only the backend can read the OS language. The web
// UI doesn't use it at the moment: a language change is saved first and takes
// effect when the page reloads, using EffectiveLocale.
type Settings struct {
	DefaultEngine                string `json:"defaultEngine"`
	LogRetentionDays             int    `json:"logRetentionDays"`
	Theme                        string `json:"theme"`
	Language                     string `json:"language"`
	EffectiveLocale              string `json:"effectiveLocale"`
	SystemLocale                 string `json:"systemLocale"`
	WebhookURL                   string `json:"webhookUrl"`
	NotifyWindowsOnSuccess       bool   `json:"notifyWindowsOnSuccess"`
	NotifyWindowsOnFailure       bool   `json:"notifyWindowsOnFailure"`
	NativeNotificationsOnSuccess bool   `json:"nativeNotificationsOnSuccess"`
	NativeNotificationsOnFailure bool   `json:"nativeNotificationsOnFailure"`
	NotifyWebhookOnSuccess       bool   `json:"notifyWebhookOnSuccess"`
	NotifyWebhookOnFailure       bool   `json:"notifyWebhookOnFailure"`

	// NotificationURLs is the System page's list of http(s) JSON webhooks and
	// shoutrrr-style URLs, sent when the notifyWebhook switches above say so.
	// GetSettings always returns a non-nil list. SaveSettings leaves the
	// stored list alone when it is nil, so a caller that didn't load the list
	// can't clear it by accident. WebhookURL is the older single-webhook
	// setting: nothing sends to it any more, and SaveSettings derives it from
	// this list so an older build keeps its webhook after a downgrade.
	NotificationURLs []string `json:"notificationUrls"`

	StartWithWindows             bool `json:"startWithWindows"`
	StartAtLogin                 bool `json:"startAtLogin"`
	MaxConcurrentJobRuns         int  `json:"maxConcurrentJobRuns"`
	DisableAutomaticUpdateChecks bool `json:"disableAutomaticUpdateChecks"`
}

// NotificationChannels picks the channels for one finished operation status:
// native desktop notifications and the notification URL list (whose switches
// keep their older notifyWebhook names).
// A completed_with_issues result is part success and part failure, so a
// channel sends it when either its success or its failure switch is on. The
// result is still one notification per channel: turning both switches on does
// not send it twice.
func (settings Settings) NotificationChannels(status string) (native, webhook bool) {
	nativeOnSuccess := settings.NotifyWindowsOnSuccess || settings.NativeNotificationsOnSuccess
	nativeOnFailure := settings.NotifyWindowsOnFailure || settings.NativeNotificationsOnFailure
	switch status {
	case "success":
		return nativeOnSuccess, settings.NotifyWebhookOnSuccess
	case "completed_with_issues":
		return nativeOnSuccess || nativeOnFailure, settings.NotifyWebhookOnSuccess || settings.NotifyWebhookOnFailure
	default:
		return nativeOnFailure, settings.NotifyWebhookOnFailure
	}
}
