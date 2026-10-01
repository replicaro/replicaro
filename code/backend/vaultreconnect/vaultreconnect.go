// Package vaultreconnect decides when a vault is saved as needing a reconnect
// and when that saved state is cleared. The state itself is one row per vault
// in the database (database/vault_reconnect.go); the vault card's Reconnect
// button and message are driven only by it.
//
// How the state is set:
//
//   - At once, by a conclusive reconnect error from a job (backup, restore,
//     integrity check, maintenance, snapshot deletion) or from a background
//     worker (profile sync, Kopia policy reconciler). "Conclusive" is exactly
//     what the profile store and the engines already mark as reconnect
//     required; this package adds no classification of its own.
//   - After a time bound of continuous failure of a background worker for any
//     other reason: 24 hours for profile sync, 6 hours for the Kopia policy
//     reconciler. Each worker keeps one failing-since time per vault in the
//     database, because the profile sync attempt count is reset by every local
//     edit and Kopia policy errors are reset to dirty at startup; an in-memory
//     or attempt-based clock would never reach the bound. Next to it the
//     worker keeps its last failure time, so that time with the app closed or
//     the computer asleep is not counted as failure time (see maxFailureGap).
//
// Some failures never count toward the bound, and each one resets the clock:
// a paused filesystem vault (pause takes priority, the pause rules already
// report it their own way), an unfinished vault password change (it has its
// own dashboard issue and Retry) and an unfinished ownership transfer (it is
// finished by repeating the takeover, not by reconnecting). These exclusions
// apply to the clock only; a conclusive error still sets the state. The pause
// exclusion follows the pause rules exactly, and those pause filesystem vaults
// only: the same typed "storage unavailable" error from an SFTP or cloud vault
// is an outage like any other and counts.
//
// An Any Rclone Remote vault is also checked after a failed job or profile
// sync (CheckRcloneRemoteAfterFailure): the vault's rclone settings are
// checked once, and if that check fails the state is set with its own source,
// RcloneRemoteCheckSource.
//
// How it is cleared: by the next success of the same job kind or worker that
// set it, whichever path that success came through (a worker's success is
// also recorded when its work is done on its behalf, for example a Kopia
// policy made ready by scheduled maintenance, or a profile published by a
// vault password change), or by a successful reconnect (which deletes the row in the same
// transaction as the reconnect). A success of some other job or worker does
// not clear it: a backup succeeding says nothing about whether the recovery
// profile can be published. The Any Rclone Remote source is the exception:
// the saved state doesn't record which kind failed before the check, so any
// later success of a job or worker on that vault clears it, and so does a
// later check after a failure that passes. Clearing never stops a worker's
// retries, and the state never blocks anything; it only tells the user.
//
// When the state goes from clear to set, and only then, one ERROR activity
// entry (a dashboard issue) and one error notification are sent. A backup that
// ends reconnect_required and sets it sends no second notification: the
// backup's own reconnect_required notification, which it sends on every run,
// already carries the same message. A failed Any Rclone Remote settings check
// does send one, also after a backup, because that backup ends failed and its
// own notification doesn't carry the reconnect message.
package vaultreconnect

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/locale"
	"github.com/local/replicaro/notifications"
	"github.com/local/replicaro/storageavailability"
	"github.com/local/replicaro/vaultprofile"
)

// MessageKey is the one reconnect message, used on the vault card, in the
// dashboard issue, in the state's notification and in a backup's own
// reconnect_required notification.
const MessageKey = "notifications.message.vaultReconnectRequired"

// Workers that keep a failure clock.
const (
	WorkerProfileSync = database.ReconnectWorkerProfileSync
	WorkerKopiaPolicy = database.ReconnectWorkerKopiaPolicy
)

var workerBounds = map[string]time.Duration{
	WorkerProfileSync: 24 * time.Hour,
	WorkerKopiaPolicy: 6 * time.Hour,
}

// maxFailureGap is the longest time between two failures of a worker that
// still counts as one continuous run. The failing-since time is saved, so it
// survives restarts; without this, time with the app closed or the laptop
// asleep would count as failure time, and the first failure after a long gap
// (a failure at 23:00, lid closed overnight, then "network not up yet" at
// boot) could escalate at once although the vault was only tried twice.
//
// Both workers retry a failing vault at least every 30 minutes while the app
// runs (their backoff caps at 30 minutes; the profile queue is also polled
// every minute), so more than an hour between one failure and the start of
// the next attempt means nobody was retrying. The clock then restarts at the
// new failure instead of counting the gap. One hour leaves room for an attempt
// that starts a little late on top of the 30-minute delay.
//
// The gap ends when the next attempt starts, before it waits for the vault
// lock, not when it fails. Backups hold the vault lock for their whole run, so
// a retry that falls due during a long backup only fails once the backup is
// done; measured to its failure, a daily backup of an hour or more would
// restart the clock every day and a 24-hour bound could never be reached. How
// long an attempt then takes is time the app spent retrying and counts.
// The profile queue runs one pass at a time and a pass waits for all its
// attempts, so its attempts start when the pass does; a pass held open for
// more than an hour by one vault's lock wait still restarts the clock of
// another failing vault whose next attempt is in the following pass.
//
// Don't raise this towards the bounds above, or a gap would again be counted
// as failure; don't lower it to 30 minutes or less, or a normal retry at the
// backoff cap could restart the clock.
const maxFailureGap = time.Hour

// RcloneRemoteCheckSource is the saved source of a state set by
// CheckRcloneRemoteAfterFailure. The source doesn't say which job kind or
// worker failed before the check, so the next success of any job or worker on
// the vault clears it (RecordOperationResult, RecordWorkerSuccess), as does a
// later check after a failure that passes. A successful reconnect clears it
// like any other state.
const RcloneRemoteCheckSource = "rclone_remote_check"

// preflightRcloneRemote is a variable only so tests can replace the check.
var preflightRcloneRemote = engines.PreflightRcloneRemote

// SetRcloneRemoteCheckForTests replaces the Any Rclone Remote settings check.
func SetRcloneRemoteCheckForTests(next func(context.Context, map[string]string) error) func() {
	previous := preflightRcloneRemote
	preflightRcloneRemote = next
	return func() { preflightRcloneRemote = previous }
}

// CheckRcloneRemoteAfterFailure runs the Any Rclone Remote settings check once
// after a failed backup, restore, integrity check, maintenance, snapshot
// deletion, or profile sync (kind is the operation kind or WorkerProfileSync)
// and returns the line to add to the failure message. For any other vault or
// kind it does nothing and returns "".
//
// A failed check sets the vault's saved reconnect state and returns the
// check's own message. The one exception is a config file that didn't respond
// in time: that says nothing about the settings, so it only returns the
// message. A passing check clears a state an earlier check set, and returns
// the line that points the user at rclone: Replicaro can't tell an expired
// sign-in from an outage in the words of each rclone backend, so those
// failures are left to rclone's own output. Callers run this only for a failed
// result, before it is saved and while they still hold the vault lock. A
// successful operation never runs it.
func CheckRcloneRemoteAfterFailure(db *sql.DB, repositoryID, kind string) string {
	if db == nil || repositoryID == "" || !operationKinds[kind] && kind != WorkerProfileSync {
		return ""
	}
	repo, err := database.GetRepository(db, repositoryID)
	if err != nil || repo.Engine != engines.ResticID || repo.Connector != engines.RcloneRemoteConnector {
		return ""
	}
	// The check bounds itself. A fresh context keeps its rclone child apart
	// from the operation's cancel gate and live output.
	checkErr := preflightRcloneRemote(context.Background(), repo.ConnectorOptions)
	if checkErr == nil {
		clearRcloneRemoteCheck(db, repositoryID)
		return engines.RcloneRemoteUseFailureHint(repo.ConnectorOptions[engines.RcloneRemoteNameOption])
	}
	var refused *engines.RcloneRemoteError
	if !errors.As(checkErr, &refused) {
		// For example the bundled rclone could not be prepared. That says
		// nothing about the vault's settings, so nothing is set.
		log.Printf("check rclone settings of vault %s after a failed %s: %v", repositoryID, kind, checkErr)
		return ""
	}
	if refused.Code != engines.RcloneRemoteConfigNotRespondingCode {
		// A config file on a share or drive that didn't answer in time may
		// answer on the next run, so only the other findings set the state.
		markRequired(db, repositoryID, RcloneRemoteCheckSource, true)
	}
	return refused.Error()
}

// clearRcloneRemoteCheck clears a state set by CheckRcloneRemoteAfterFailure.
// Other vaults never have that source, so for them it changes nothing.
func clearRcloneRemoteCheck(db *sql.DB, repositoryID string) {
	if _, err := database.ClearVaultReconnectRequired(db, repositoryID, RcloneRemoteCheckSource); err != nil {
		log.Printf("clear reconnect state for vault %s: %v", repositoryID, err)
	}
}

// Job kinds (operation kinds) whose conclusive reconnect errors set the state.
// Vault administration jobs and job deletion are left out: they either fix
// the vault themselves or do not act on one vault.
var operationKinds = map[string]bool{
	"backup": true, "restore": true, "check": true, "maintenance": true, "delete": true,
}

var now = time.Now
var dispatchNotification = notifications.Dispatch

// Now is the clock failure times are taken from. Workers take an attempt's
// start time from it, so a test that moves the clock moves both.
func Now() time.Time { return now() }

// SetClockForTests replaces the clock used for failing-since times.
func SetClockForTests(clock func() time.Time) func() {
	previous := now
	now = clock
	return func() { now = previous }
}

// SetNotificationDispatchForTests replaces notification delivery.
func SetNotificationDispatchForTests(next func(string, bool, bool, notifications.Event, func(error))) func() {
	previous := dispatchNotification
	dispatchNotification = next
	return func() { dispatchNotification = previous }
}

// Conclusive reports whether err is a reconnect-required error from the
// profile store or an engine, raised before any requested native work
// started. A cancel, shutdown or deadline is never conclusive, and neither is
// an error that came after the native child already ran: the child's own
// result is the truth then.
func Conclusive(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if !vaultprofile.IsReconnectRequired(err) && !engines.IsReconnectRequired(err) {
		return false
	}
	if status, _, _, _, _, known := engines.RequestedOperationOutcome(err); known &&
		status != engines.RequestedOperationNotStarted {
		return false
	}
	return true
}

// RecordOperationResult applies one finished job's result to its vault's
// saved state. Callers pass the saved terminal status and the error that
// produced it, and call it after the status is durable and before the vault
// lock is released, so a reconnect cannot commit in between and then be
// overwritten by this older result.
func RecordOperationResult(db *sql.DB, repositoryID, kind, status string, cause error) {
	if db == nil || repositoryID == "" || !operationKinds[kind] {
		return
	}
	switch status {
	case "success", "completed_with_issues":
		// The main work reached the vault, so whatever this kind found before
		// is gone.
		if _, err := database.ClearVaultReconnectRequired(db, repositoryID, kind); err != nil {
			log.Printf("clear reconnect state for vault %s: %v", repositoryID, err)
		}
		clearRcloneRemoteCheck(db, repositoryID)
	case "failed", "reconnect_required":
		if status == "reconnect_required" || Conclusive(cause) {
			markRequired(db, repositoryID, kind, kind != "backup")
		}
	}
}

// RecordWorkerSuccess clears the worker's clock, and the saved state when this
// worker set it.
func RecordWorkerSuccess(db *sql.DB, repositoryID, worker string) {
	if db == nil || repositoryID == "" {
		return
	}
	if err := database.ResetVaultWorkerFailureClock(db, repositoryID, worker); err != nil {
		log.Printf("reset %s failure clock for vault %s: %v", worker, repositoryID, err)
	}
	if _, err := database.ClearVaultReconnectRequired(db, repositoryID, worker); err != nil {
		log.Printf("clear reconnect state for vault %s: %v", repositoryID, err)
	}
	clearRcloneRemoteCheck(db, repositoryID)
}

// RecordWorkerFailure applies one failed worker attempt. startedAt is when
// the attempt started, taken from Now before it waited for the vault lock;
// see maxFailureGap for why the gap is measured to it. See the package
// comment for the order: cancel, pause, conclusive, excluded, clock.
func RecordWorkerFailure(db *sql.DB, repositoryID, worker string, startedAt time.Time, cause error) {
	bound, known := workerBounds[worker]
	if db == nil || repositoryID == "" || !known || cause == nil || errors.Is(cause, context.Canceled) {
		// A cancel is shutdown or the worker being stopped, not a failure.
		return
	}
	var unavailable *storageavailability.RepositoryStorageUnavailableError
	if errors.As(cause, &unavailable) && filesystemVault(db, repositoryID) {
		// Admission returns this typed error for every connector (a timeout or
		// a missing repository reported by native validation), but only a
		// filesystem vault is paused by it: the pause rules are filesystem-only
		// (runner/runner.go and scheduler/repository_tasks.go make the same
		// connector check). An SFTP or cloud vault with this error is having
		// an outage, which counts toward the bound like any other failure.
		// Keep the connector check; without it a cloud vault that stays
		// unreachable would never be reported.
		resetClock(db, repositoryID, worker)
		return
	}
	if Conclusive(cause) {
		markRequired(db, repositoryID, worker, true)
		return
	}
	if excludedFromClock(db, repositoryID, cause) {
		resetClock(db, repositoryID, worker)
		return
	}
	at := now()
	if startedAt.IsZero() || startedAt.After(at) {
		startedAt = at
	}
	since, err := database.RecordVaultWorkerFailure(db, repositoryID, worker, at, startedAt.Add(-maxFailureGap))
	if err != nil {
		log.Printf("record %s failure clock for vault %s: %v", worker, repositoryID, err)
		return
	}
	if at.Sub(since) >= bound {
		markRequired(db, repositoryID, worker, true)
	}
}

// filesystemVault reports whether the vault is a filesystem vault. A vault
// that cannot be read (removed meanwhile) is not; its failure is then not
// saved anyway, because the state row needs the vault.
func filesystemVault(db *sql.DB, repositoryID string) bool {
	repo, err := database.GetRepository(db, repositoryID)
	return err == nil && repo.Connector == "fs"
}

func excludedFromClock(db *sql.DB, repositoryID string, cause error) bool {
	if errors.Is(cause, database.ErrVaultPasswordChangeRecoveryRequired) ||
		errors.Is(cause, vaultprofile.ErrOwnerTransferUnfinished) {
		return true
	}
	// The saved records are checked too, not only the error: while this
	// computer's password change or ownership transfer is unfinished, a failure
	// can surface as some other error first.
	if err := database.RequireNoPrecommitVaultPasswordChange(db, repositoryID); errors.Is(err, database.ErrVaultPasswordChangeRecoveryRequired) {
		return true
	}
	if _, err := database.ActiveOwnerTransfer(db, repositoryID); err == nil {
		return true
	}
	return false
}

func resetClock(db *sql.DB, repositoryID, worker string) {
	if err := database.ResetVaultWorkerFailureClock(db, repositoryID, worker); err != nil {
		log.Printf("reset %s failure clock for vault %s: %v", worker, repositoryID, err)
	}
}

// markRequired sets the state and, only when this call set it, writes the
// dashboard issue and (unless notify is false) sends the notification. The
// set is one conditional upsert, so two callers racing to set it send one
// message between them.
func markRequired(db *sql.DB, repositoryID, source string, notify bool) {
	changed, err := database.MarkVaultReconnectRequired(db, repositoryID, source, now())
	if err != nil {
		// For example the vault was removed meanwhile (foreign key). Nothing to
		// tell the user about then.
		log.Printf("save reconnect state for vault %s: %v", repositoryID, err)
		return
	}
	if !changed {
		return
	}
	settings, settingsErr := database.GetSettings(db)
	language := ""
	if settingsErr == nil {
		language = settings.Language
	}
	effective := locale.Effective(language)
	if err := database.LogError(db, locale.Text(effective, MessageKey, nil)); err != nil {
		log.Printf("record reconnect issue for vault %s: %v", repositoryID, err)
	}
	if !notify || settingsErr != nil {
		return
	}
	// Same channels as any failed operation. The status is the one a backup's
	// reconnect notification already uses, so webhook receivers see one value
	// for "reconnect needed".
	const status = "reconnect_required"
	native, webhook := settings.NotificationChannels(status)
	if !native && !webhook {
		return
	}
	event := notifications.Event{
		Event: "reconnect", Status: status, Locale: effective, MessageKey: MessageKey,
	}
	dispatchNotification(settings.WebhookURL, native, webhook, event, func(err error) {
		if err != nil {
			_ = database.LogError(db, "Notification failed: "+err.Error())
		}
	})
}

// Message returns the reconnect message in the saved UI language, for a
// backup's own operation result.
func Message(db *sql.DB) string {
	language := ""
	if settings, err := database.GetSettings(db); err == nil {
		language = settings.Language
	}
	return locale.Text(locale.Effective(language), MessageKey, nil)
}
