package database

import (
	"database/sql"
	"fmt"
	"github.com/google/uuid"
	"github.com/local/replicaro/models"
	"strconv"
)

const installationIDKey = "installationId"
const languageKey = "language"

// EnsureLanguageSetting only fills the absent key. It runs on the writable
// startup connection before read-only pools open, including on older installs.
func EnsureLanguageSetting(db *sql.DB) error {
	_, err := db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO NOTHING`, languageKey, models.LanguageSystem)
	return err
}

func InstallationID(db *sql.DB) (string, error) {
	var id string
	err := db.QueryRow(`SELECT value FROM settings WHERE key = ?`, installationIDKey).Scan(&id)
	if err == nil {
		parsed, parseErr := uuid.Parse(id)
		if parseErr == nil && parsed.String() == id {
			return id, nil
		}
		id = uuid.NewString()
		_, updateErr := db.Exec(`UPDATE settings SET value = ? WHERE key = ?`, id, installationIDKey)
		return id, updateErr
	}
	if err != sql.ErrNoRows {
		return "", err
	}
	id = uuid.NewString()
	_, err = db.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO NOTHING`, installationIDKey, id)
	if err != nil {
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
		DefaultEngine:                "restic",
		AutoStart:                    true,
		LogRetentionDays:             30,
		Theme:                        models.ThemeDark,
		Language:                     models.LanguageSystem,
		NotifyWindowsOnFailure:       true,
		NativeNotificationsOnFailure: true,
		StartWithWindows:             true,
		StartAtLogin:                 true,
		MinimizeToTray:               true,
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

		case "autoStart":

			settings.AutoStart =
				value == "true"

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

		case "minimizeToTray":

			settings.MinimizeToTray =
				value == "true"

		case "webhookUrl":
			settings.WebhookURL = value

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
	return settings, nil
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
		// settings shape. Resolve it inside this replacement transaction so
		// that writer cannot reset a newer client's explicit choice.
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

	var dashboardIssuesReviewedAt string
	_ = tx.QueryRow(
		`SELECT value FROM settings WHERE key = ?`,
		dashboardIssuesReviewedAtKey,
	).Scan(&dashboardIssuesReviewedAt)
	var installationID string
	_ = tx.QueryRow(`SELECT value FROM settings WHERE key = ?`, installationIDKey).Scan(&installationID)
	_, err = tx.Exec(
		`DELETE FROM settings WHERE key NOT IN (?, ?, ?, ?)`,
		appUpdateLastAutomaticAttemptKey, appUpdateLastResultKey,
		appUpdateLastAvailableVersionKey, appUpdateSkippedVersionKey,
	)

	if err != nil {
		return err
	}

	values := map[string]string{
		"defaultEngine":                settings.DefaultEngine,
		"theme":                        settings.Theme,
		languageKey:                    settings.Language,
		"webhookUrl":                   settings.WebhookURL,
		"notifyWindowsOnSuccess":       strconv.FormatBool(settings.NotifyWindowsOnSuccess),
		"notifyWindowsOnFailure":       strconv.FormatBool(settings.NotifyWindowsOnFailure),
		"nativeNotificationsOnSuccess": strconv.FormatBool(settings.NativeNotificationsOnSuccess || settings.NotifyWindowsOnSuccess),
		"nativeNotificationsOnFailure": strconv.FormatBool(settings.NativeNotificationsOnFailure || settings.NotifyWindowsOnFailure),
		"notifyWebhookOnSuccess":       strconv.FormatBool(settings.NotifyWebhookOnSuccess),
		"notifyWebhookOnFailure":       strconv.FormatBool(settings.NotifyWebhookOnFailure),
		"autoStart": strconv.FormatBool(
			settings.AutoStart,
		),
		"logRetentionDays": strconv.Itoa(
			settings.LogRetentionDays,
		),
		"startWithWindows": strconv.FormatBool(
			settings.StartWithWindows,
		),
		"startAtLogin": strconv.FormatBool(settings.StartAtLogin || settings.StartWithWindows),
		"minimizeToTray": strconv.FormatBool(
			settings.MinimizeToTray,
		),
		"maxConcurrentJobRuns":       strconv.Itoa(settings.MaxConcurrentJobRuns),
		dashboardIssuesReviewedAtKey: dashboardIssuesReviewedAt,
		installationIDKey:            installationID,
		appUpdateDisableAutomaticKey: strconv.FormatBool(settings.DisableAutomaticUpdateChecks),
	}
	for key, value := range values {

		_, err := tx.Exec(
			`INSERT INTO settings
			(key, value)
			VALUES (?, ?)`,
			key,
			value,
		)

		if err != nil {
			return err
		}
	}

	return tx.Commit()
}
