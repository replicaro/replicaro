package database

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/local/replicaro/models"
	"strconv"
	"strings"
)

const installationIDKey = "installationId"
const languageKey = "language"
const notificationURLsKey = "notificationUrls"

// EnsureLanguageSetting only fills the absent key. It runs on the writable
// startup connection before read-only pools open, including on older installs.
func EnsureLanguageSetting(db *sql.DB) error {
	_, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO NOTHING`, languageKey, models.LanguageSystem)
	return err
}

// ErrInstallationIDDamaged means the stored installation ID is present but
// isn't a canonical UUID. Every vault attachment and the browser's client
// UUID check use this ID, so a damaged value is never replaced without asking:
// a new ID makes every attached vault need reconnecting, and that needs the
// user's go-ahead (see RepairInstallationID).
var ErrInstallationIDDamaged = errors.New("the stored installation ID is damaged")

// ValidInstallationID reports whether id is a canonical UUID: lowercase,
// hyphenated, without braces or a urn:uuid: prefix, and not the nil UUID. It's
// the one test of a stored installation ID. The nil UUID is refused because
// every vault check of a client UUID refuses it too, so an installation with
// that ID couldn't use any vault.
func ValidInstallationID(id string) bool {
	parsed, err := uuid.Parse(id)
	return err == nil && parsed != uuid.Nil && parsed.String() == id
}

// InstallationID returns the stored installation ID. A new one is created only
// when none is stored at all, as on a fresh install. A read error is returned
// as it is, and a damaged value returns ErrInstallationIDDamaged; neither
// creates a new ID.
func InstallationID(db *sql.DB) (string, error) {
	var id string
	err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, installationIDKey).Scan(&id)
	if err == nil {
		if ValidInstallationID(id) {
			return id, nil
		}
		return "", ErrInstallationIDDamaged
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	_, err = db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO NOTHING`, installationIDKey, uuid.NewString())
	if err != nil {
		return "", err
	}
	return InstallationID(db)
}

// RepairInstallationID replaces a damaged installation ID with a new one and
// returns the ID that is stored afterwards. An intact ID is returned
// unchanged, and a missing one is created as InstallationID does. Only the
// damaged value read here is replaced, so an ID stored in the meantime is
// never overwritten.
func RepairInstallationID(db *sql.DB) (string, error) {
	var current string
	err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, installationIDKey).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return InstallationID(db)
	}
	if err != nil {
		return "", err
	}
	if ValidInstallationID(current) {
		return current, nil
	}
	// The stored value can be a BLOB, which never equals the text read back
	// here, so it's compared as text.
	if _, err := db.Exec(`UPDATE settings SET value = ? WHERE key = ? AND CAST(value AS TEXT) = ?`,
		uuid.NewString(), installationIDKey, current); err != nil {
		return "", err
	}
	return InstallationID(db)
}

const (
	DefaultMaxConcurrentJobRuns = 2
	MinMaxConcurrentJobRuns     = 1
	MaxMaxConcurrentJobRuns     = 32
)

func ValidMaxConcurrentJobRuns(value int) bool {
	return value >= MinMaxConcurrentJobRuns && value <= MaxMaxConcurrentJobRuns
}

func GetSettings(
	db *sql.DB,
) (models.Settings, error) {

	settings := models.Settings{
		DefaultEngine: "restic",
		// autoStart and minimizeToTray rows from older versions are left in
		// place and ignored; nothing in Replicaro acts on either preference.
		LogRetentionDays:             30,
		Theme:                        models.ThemeDark,
		Language:                     models.LanguageSystem,
		NotifyWindowsOnFailure:       true,
		NativeNotificationsOnFailure: true,
		StartWithWindows:             true,
		StartAtLogin:                 true,
		MaxConcurrentJobRuns:         DefaultMaxConcurrentJobRuns,
	}

	rows, err := db.Query(
		`SELECT key, value
		FROM settings`,
	)

	if err != nil {
		return settings, err
	}

	defer rows.Close()
	legacyStartSeen := false
	neutralStartSeen := false
	var storedNotificationURLs *string

	for rows.Next() {

		var key string
		var value string

		err := rows.Scan(
			&key,
			&value,
		)

		if err != nil {
			continue
		}

		switch key {

		case "defaultEngine":
			if value == "restic" || value == "kopia" {
				settings.DefaultEngine = value
			}

		case "theme":
			if models.ValidTheme(value) {
				settings.Theme = value
			} else {
				// Historical invalid values never represented an applied theme.
				// Preserve the former visible behavior instead of guessing.
				settings.Theme = models.ThemeDark
			}
		case languageKey:
			if models.ValidLanguage(value) {
				settings.Language = value
			}

		case "logRetentionDays":

			if days, err := strconv.Atoi(value); err == nil {
				settings.LogRetentionDays = days
			}

		case "startWithWindows":
			settings.StartWithWindows =
				value == "true"
			legacyStartSeen = true
		case "startAtLogin":
			settings.StartAtLogin = value == "true"
			settings.StartWithWindows = settings.StartAtLogin
			neutralStartSeen = true

		case "webhookUrl":
			settings.WebhookURL = value
		case notificationURLsKey:
			storedNotificationURLs = &value

		case "notifyWindowsOnSuccess":
			settings.NotifyWindowsOnSuccess = value == "true"
			settings.NativeNotificationsOnSuccess = settings.NotifyWindowsOnSuccess

		case "notifyWindowsOnFailure":
			settings.NotifyWindowsOnFailure = value == "true"
			settings.NativeNotificationsOnFailure = settings.NotifyWindowsOnFailure

		case "nativeNotificationsOnSuccess":
			settings.NativeNotificationsOnSuccess = value == "true"
			settings.NotifyWindowsOnSuccess = settings.NativeNotificationsOnSuccess

		case "nativeNotificationsOnFailure":
			settings.NativeNotificationsOnFailure = value == "true"
			settings.NotifyWindowsOnFailure = settings.NativeNotificationsOnFailure

		case "notifyWebhookOnSuccess":
			settings.NotifyWebhookOnSuccess = value == "true"

		case "notifyWebhookOnFailure":
			settings.NotifyWebhookOnFailure = value == "true"

		case "maxConcurrentJobRuns":
			if limit, err := strconv.Atoi(value); err == nil && ValidMaxConcurrentJobRuns(limit) {
				settings.MaxConcurrentJobRuns = limit
			}
		case appUpdateDisableAutomaticKey:
			settings.DisableAutomaticUpdateChecks = value == "true"
		}
	}
	if err := rows.Err(); err != nil {
		return settings, err
	}
	if legacyStartSeen && !neutralStartSeen {
		// Legacy-only rows represented the user's complete startup preference;
		// mirror it into the neutral field so upgrades cannot silently re-enable
		// login startup.
		settings.StartAtLogin = settings.StartWithWindows
	}
	settings.NotificationURLs = notificationURLs(storedNotificationURLs, settings.WebhookURL)
	return settings, nil
}

// notificationURLs reads the stored list. Until the list is first saved there
// is no row, and an existing webhook URL becomes the list's only entry; that is
// the whole upgrade, with no migration. An unreadable row falls back the same
// way rather than dropping every notification.
func notificationURLs(stored *string, webhookURL string) []string {
	if stored != nil {
		var urls []string
		if json.Unmarshal([]byte(*stored), &urls) == nil {
			if urls == nil {
				urls = []string{}
			}
			return urls
		}
	}
	if webhookURL == "" {
		return []string{}
	}
	return []string{webhookURL}
}

// firstWebhookURL is the first http(s) entry of the list, or "".
func firstWebhookURL(urls []string) string {
	for _, entry := range urls {
		lower := strings.ToLower(entry)
		if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
			return entry
		}
	}
	return ""
}

func SaveSettings(
	db *sql.DB,
	settings models.Settings,
) error {
	if settings.MaxConcurrentJobRuns == 0 {
		settings.MaxConcurrentJobRuns = DefaultMaxConcurrentJobRuns
	}
	if settings.DefaultEngine == "" {
		settings.DefaultEngine = "restic"
	}
	if settings.DefaultEngine != "restic" && settings.DefaultEngine != "kopia" {
		return fmt.Errorf("unsupported default engine: %s", settings.DefaultEngine)
	}
	if settings.Theme == "" {
		settings.Theme = models.ThemeDark
	}
	if !models.ValidTheme(settings.Theme) {
		return fmt.Errorf("unsupported theme: %s", settings.Theme)
	}
	if settings.Language != "" && !models.ValidLanguage(settings.Language) {
		return fmt.Errorf("unsupported language: %s", settings.Language)
	}
	if !ValidMaxConcurrentJobRuns(settings.MaxConcurrentJobRuns) {
		return fmt.Errorf("max concurrent job runs must be between %d and %d", MinMaxConcurrentJobRuns, MaxMaxConcurrentJobRuns)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if settings.Language == "" {
		// A missing language field means an older writer supplied the prior
		// settings shape. Resolve it inside this save's transaction so that
		// writer cannot reset a newer client's explicit choice.
		err = tx.QueryRow(`SELECT value FROM settings WHERE key = ?`, languageKey).Scan(&settings.Language)
		if err == sql.ErrNoRows {
			settings.Language = models.LanguageSystem
		} else if err != nil {
			return err
		}
		if !models.ValidLanguage(settings.Language) {
			settings.Language = models.LanguageSystem
		}
	}

	// Only the user-facing keys below are written, each one upserted. Every
	// other row is internal state that the settings form doesn't carry (the
	// installation ID, the issue review watermark, update-check state,
	// one-time migration markers and anything added later), so a save leaves
	// it as it is rather than reading it back and writing it again.
	values := map[string]string{
		"defaultEngine": settings.DefaultEngine,
		"theme":         settings.Theme,
		languageKey:     settings.Language,
		// The notification and startup preferences each have an older key name
		// and a newer one. Both names are written, so a build that reads either
		// one sees the same choice.
		"notifyWindowsOnSuccess":       strconv.FormatBool(settings.NotifyWindowsOnSuccess),
		"notifyWindowsOnFailure":       strconv.FormatBool(settings.NotifyWindowsOnFailure),
		"nativeNotificationsOnSuccess": strconv.FormatBool(settings.NativeNotificationsOnSuccess || settings.NotifyWindowsOnSuccess),
		"nativeNotificationsOnFailure": strconv.FormatBool(settings.NativeNotificationsOnFailure || settings.NotifyWindowsOnFailure),
		"notifyWebhookOnSuccess":       strconv.FormatBool(settings.NotifyWebhookOnSuccess),
		"notifyWebhookOnFailure":       strconv.FormatBool(settings.NotifyWebhookOnFailure),
		"logRetentionDays":             strconv.Itoa(settings.LogRetentionDays),
		"startWithWindows":             strconv.FormatBool(settings.StartWithWindows),
		"startAtLogin":                 strconv.FormatBool(settings.StartAtLogin || settings.StartWithWindows),
		"maxConcurrentJobRuns":         strconv.Itoa(settings.MaxConcurrentJobRuns),
		appUpdateDisableAutomaticKey:   strconv.FormatBool(settings.DisableAutomaticUpdateChecks),
	}
	if settings.NotificationURLs != nil {
		encoded, err := json.Marshal(settings.NotificationURLs)
		if err != nil {
			return err
		}
		values[notificationURLsKey] = string(encoded)
		// Older builds only know webhookUrl. Keep writing it, as the first
		// http(s) entry of the list, so a downgrade keeps the JSON webhook
		// working. Nothing in this build sends to it.
		values["webhookUrl"] = firstWebhookURL(settings.NotificationURLs)
	}
	for key, value := range values {
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
			return err
		}
	}

	return tx.Commit()
}
