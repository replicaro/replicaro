package api

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/local/replicaro/database"
	"github.com/local/replicaro/operationruntime"
	"github.com/local/replicaro/vaultlock"
	"github.com/local/replicaro/vaultreconnect"
)

// Restore, restore of selected items, manual integrity check and
// maintenance, snapshot deletion, job deletion, and the vault administration
// jobs (password change, settings save, removal; see
// vault_admin_operations.go) are tracked operations. They
// deliberately do not run inside their HTTP request: closing the browser,
// sleep, a lost connection or a reverse-proxy timeout would cancel them, and a
// busy vault would be an immediate error. They share this one path:
//
//  1. The request runs only the checks that need no vault lock (request shape,
//     capability, an unfinished password change, duplicates) and refuses with
//     an HTTP error and no record when one fails.
//  2. It queues the operation record with a server-generated ID, registers
//     the record's runtime (live output and cancel) and answers 202 with that
//     ID. The UI takes it from there (dashboard live log, or close and toast).
//  3. The worker runs on the runtime manager's application-owned lifetime, not
//     on the request. It waits for a busy vault instead of failing; the record
//     stays queued meanwhile and can be cancelled.
//  4. With the lock held it moves the record to running, reloads what it acts
//     on and repeats the admission that needs the lock. A failure there is a
//     failed operation with a dashboard issue and a notification, not an HTTP
//     error: the request has already been answered.
//
// Everything the worker takes (vault lock, restore metadata-sync deferral, the
// job deletion gate) is released by the worker after the final status is
// saved. Do not move this work back onto the request, and do not keep the
// request open until the job finishes: a proxy timeout would still end the
// request and show a false error while the job carried on.
type trackedOperation struct {
	db      *sql.DB
	runtime *operationruntime.Manager
	id      string
	// kind and repositoryID come from the queued record. finish uses them to
	// keep the vault's saved reconnect state in step with this result.
	kind         string
	repositoryID string
	// lifetime ends only at application shutdown; ctx additionally ends on an
	// accepted cancel and carries the live-output and captured-output hooks.
	lifetime  context.Context
	ctx       context.Context
	closeGate func()
	// notify prepares this operation's notification for a terminal status; nil
	// sends none. It is set by the request before the worker starts.
	notify    func(status string) func()
	persisted bool
}

// afterTrackedOperationForTests runs once a worker has released everything it
// held, so tests can wait for exactly that operation.
var afterTrackedOperationForTests func(operationID string)

// startTrackedOperation queues the record and starts work on the background
// lifetime. It returns only after the record exists and its runtime (cancel,
// live output) is registered, so the ID it returns can be opened and cancelled
// at once. work must end the operation through finish (or leave it running on
// purpose, as the repository task does when a step result could not be saved).
func startTrackedOperation(
	db *sql.DB,
	runtime *operationruntime.Manager,
	record database.QueuedOperation,
	notify func(operationID, status string) func(),
	work func(*trackedOperation),
) (string, error) {
	lifetime, done, err := runtime.BeginBackground()
	if err != nil {
		return "", err
	}
	operationID, err := database.QueueOperation(db, record, time.Now())
	if err != nil {
		done()
		return "", err
	}
	ctx, closeGate, release, err := beginOperationRuntime(lifetime, runtime, operationID)
	if err != nil {
		_ = finishOperationDurably(db, operationID, "failed", err.Error(), time.Now())
		done()
		return "", err
	}
	op := &trackedOperation{db: db, runtime: runtime, id: operationID, kind: record.Kind, repositoryID: record.RepositoryID,
		lifetime: lifetime, ctx: ctx, closeGate: closeGate}
	if notify != nil {
		op.notify = func(status string) func() { return notify(operationID, status) }
	}
	go func() {
		defer func() {
			// A closed runtime stays attached when the terminal write failed, so
			// the live endpoint keeps matching the still-running row that startup
			// reconciliation owns.
			release(op.persisted)
			if afterTrackedOperationForTests != nil {
				afterTrackedOperationForTests(operationID)
			}
			done()
		}()
		work(op)
	}()
	return operationID, nil
}

// waitForVault waits for the vault's exclusive lock while the record stays
// queued. It uses the same public exclusive entry point as every other vault
// writer: low-priority readers (metadata sync, Vault Size) yield, and a
// waiting writer holds back new readers so it is not starved. Among several
// waiting writers the order is Go's mutex order, roughly first come first
// served, not a strict queue. It returns early only when the operation is
// cancelled or the application stops.
func (op *trackedOperation) waitForVault(repositoryID string) (func(), error) {
	return vaultlock.AcquireExclusiveContext(op.ctx, repositoryID)
}

// activate moves the record from queued to running once the worker holds
// what it waited for.
func (op *trackedOperation) activate() error {
	return database.ActivateQueuedOperation(op.db, op.id)
}

// startWithoutCancel closes the cancel gate for work that cannot be stopped
// once it has started (snapshot and job deletion). It is called after the
// locks are held and before anything changes. It reports false when a cancel
// was accepted first; the caller then ends the operation interrupted without
// starting. Shutdown still ends ctx; that is an interruption, not a cancel.
func (op *trackedOperation) startWithoutCancel() bool {
	return op.runtime.CloseCancel(op.id)
}

// finish closes the cancel gate, saves the final status and output, and only
// then sends the notification. Interrupted operations (cancel, shutdown) never
// notify; that matches every other operation kind.
func (op *trackedOperation) finish(status, output string) bool {
	return op.finishWithCause(status, output, nil)
}

// finishWithCause is finish for a result that has an error behind it. A
// conclusive reconnect error sets the vault's saved reconnect state and a
// success clears one this kind set (vaultreconnect.RecordOperationResult).
// Workers call it while they still hold the vault lock, which is released
// only after the final status is saved.
func (op *trackedOperation) finishWithCause(status, output string, cause error) bool {
	op.closeGate()
	if status == "failed" {
		// Only an Any Rclone Remote vault gets a line here: the result of
		// checking its rclone settings once after the failure.
		if note := vaultreconnect.CheckRcloneRemoteAfterFailure(op.db, op.repositoryID, op.kind); note != "" {
			output = strings.TrimSpace(output + "\n" + note)
		}
	}
	var dispatch func()
	if status != "interrupted" && op.notify != nil {
		dispatch = op.notify(status)
	}
	if err := finishOperationDurably(op.db, op.id, status, output, time.Now()); err != nil {
		_ = database.LogError(op.db, "Operation "+op.id+" "+err.Error())
		return false
	}
	op.persisted = true
	vaultreconnect.RecordOperationResult(op.db, op.repositoryID, op.kind, status, cause)
	if dispatch != nil {
		dispatch()
	}
	return true
}

// fail ends the operation as failed with err's text, or interrupted when err
// comes from a cancel or shutdown.
func (op *trackedOperation) fail(err error) bool {
	return op.finishWithCause(terminalOperationStatus(op.ctx, err), err.Error(), err)
}

// notStarted ends an operation that never got past waiting: a cancel while it
// waited, or shutdown. Nothing ran, so it is interrupted with a plain reason
// in its log. Any other error before start is a failure.
func (op *trackedOperation) notStarted(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		reason := "Cancelled before the operation started."
		if op.lifetime.Err() != nil {
			reason = "Application stopped before the operation started."
		}
		return op.finish("interrupted", reason)
	}
	return op.finish("failed", err.Error())
}

// writeTrackedOperationStarted is the 202 every tracked request returns.
func writeTrackedOperationStarted(w http.ResponseWriter, operationID string) {
	writeJSONStatus(w, http.StatusAccepted, map[string]string{"operationId": operationID})
}

// writeTrackedStartError maps a failure to start a tracked operation. A vault
// whose removal is pending, or that already has a password change, settings
// save or removal queued or running, is a coded refusal.
func writeTrackedStartError(w http.ResponseWriter, err error) {
	if writeVaultWorkRefusal(w, err) {
		return
	}
	if errors.Is(err, operationruntime.ErrStopping) {
		writeError(w, http.StatusServiceUnavailable, err)
		return
	}
	writeError(w, http.StatusInternalServerError, err)
}
