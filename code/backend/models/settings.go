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
	case LanguageSystem, LanguageEnglish, "de", "fr", "ar", "ur", "hi", "es", "it", "zh-Hans", "yue-Hant",
		"ja", "ga", "ko", "ms", "id", "tr", "he", "pt-PT", "pt-BR", "ru", "pl", "nl", "bn", "el", "fa", "vi", "pcm":
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
// language is saved: System > Appearance previews an unsaved Language choice
// before Save, and only the backend can read the OS language, so the page
// needs it to preview "system" while another language is saved.
type Settings struct {
	DefaultEngine                string `json:"defaultEngine"`
	AutoStart                    bool   `json:"autoStart"`
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

	StartWithWindows             bool `json:"startWithWindows"`
	StartAtLogin                 bool `json:"startAtLogin"`
	MinimizeToTray               bool `json:"minimizeToTray"`
	MaxConcurrentJobRuns         int  `json:"maxConcurrentJobRuns"`
	DisableAutomaticUpdateChecks bool `json:"disableAutomaticUpdateChecks"`
}

func (settings Settings) NotificationChannels(success bool) (native, webhook bool) {
	if success {
		return settings.NotifyWindowsOnSuccess || settings.NativeNotificationsOnSuccess, settings.NotifyWebhookOnSuccess
	}
	return settings.NotifyWindowsOnFailure || settings.NativeNotificationsOnFailure, settings.NotifyWebhookOnFailure
}
