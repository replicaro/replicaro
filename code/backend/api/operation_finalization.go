package api

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/local/replicaro/command"
	"github.com/local/replicaro/database"
	"github.com/local/replicaro/engines"
	"github.com/local/replicaro/operationlog"
	"github.com/local/replicaro/operationruntime"
)

var finishOperationWrite = database.FinishOperation
var finishNativeOperationStep = database.FinishOperationStep
var errOperationStepFinalization = errors.New("operation step finalization failed")

func beginOperationRuntime(parent context.Context, manager *operationruntime.Manager, operationID string) (context.Context, func(), func(bool), error) {
	operationCtx, cancel := context.WithCancel(parent)
	if err := manager.Register(operationID, func() error { cancel(); return nil }); err != nil {
		cancel()
		return nil, nil, nil, err
	}
	operationCtx = command.ContextWithLiveOutput(operationCtx, func(stream, text string) {
		manager.Append(operationID, stream, text)
	})
	operationCtx = command.ContextWithCapturedOutputPublisher(operationCtx, func(engine, kind, status, diagnostic string, stdout, stderr io.Reader) bool {
		return operationlog.StageNativeOutput(operationID, engine, kind, status, diagnostic, stdout, stderr) == nil
	})
	closeGate := func() { manager.CloseCancel(operationID) }
	release := func(terminalPersisted bool) {
		cancel()
		// A closed runtime remains attached when the terminal write exhausts its
		// bounded retries; startup reconciliation still owns that active row.
		if terminalPersisted {
			manager.Remove(operationID)
		}
	}
	return operationCtx, closeGate, release, nil
}

func finishTrackedNativeStep(
	db *sql.DB, operationID, kind, output string, operationErr error, processStarted bool,
) error {
	operationStatus, stageStarted, preparationErr, nativeStageErr, outputProcessingErr, stageKnown :=
		engines.RequestedOperationOutcome(operationErr)
	status := "succeeded"
	stepOutput := output
	admissionRejected := false
	if stageKnown {
		admissionRejected = !stageStarted && preparationErr != nil &&
			command.IsPreProcessAdmission(preparationErr)
		switch operationStatus {
		case engines.RequestedOperationNotStarted:
			status = "skipped"
			stepOutput = "native engine preparation failed; requested native operation was not started"
		case engines.RequestedOperationInterrupted:
			if !stageStarted {
				status = "skipped"
				stepOutput = "native engine operation was interrupted before native process start"
				break
			}
			status = "interrupted"
			stepOutput = combinedOperationOutput(output, nativeStageErr)
		case engines.RequestedOperationFailed:
			status = "failed"
			stepOutput = combinedOperationOutput(output, nativeStageErr)
		}
	} else {
		nativeStageErr = operationErr
		stageStarted = processStarted
		if nativeStageErr != nil {
			status = "failed"
			stepOutput = combinedOperationOutput(output, nativeStageErr)
		}
		if nativeStageErr != nil && command.IsPreProcessAdmission(nativeStageErr) && !stageStarted {
			admissionRejected = true
			status = "skipped"
			stepOutput = "native operation skipped because process admission failed before process start"
		}
	}
	if admissionRejected {
		stepOutput = "native operation skipped because process admission failed before process start"
	}
	var persistErr error
	if err := finishNativeOperationStep(db, operationID, kind, status, stepOutput, time.Now()); err != nil {
		persistErr = fmt.Errorf("%w: persist %s result: %v", errOperationStepFinalization, kind, err)
	}
	if admissionRejected {
		if err := database.StartOperationStep(db, operationID, "orchestration", "native_process_admission", time.Now()); err != nil {
			persistErr = errors.Join(persistErr, fmt.Errorf("persist native-process admission start: %w", err))
		} else if err := finishNativeOperationStep(
			db, operationID, "native_process_admission", "failed",
			"native process admission failed before process start", time.Now(),
		); err != nil {
			persistErr = errors.Join(persistErr, fmt.Errorf("%w: persist native-process admission failure: %v", errOperationStepFinalization, err))
		}
	}
	var outputFailure *command.OutputProcessingFailure
	if errors.As(errors.Join(outputProcessingErr, operationErr), &outputFailure) {
		if err := database.StartOperationStep(db, operationID, "orchestration", "output_processing", time.Now()); err != nil {
			persistErr = errors.Join(persistErr, fmt.Errorf("persist output-processing start: %w", err))
		} else if err := finishNativeOperationStep(
			db, operationID, "output_processing", "failed", "native output could not be processed completely", time.Now(),
		); err != nil {
			persistErr = errors.Join(persistErr, fmt.Errorf("%w: persist output-processing failure: %v", errOperationStepFinalization, err))
		}
	}
	return persistErr
}

func terminalOperationStatus(_ context.Context, err error) string {
	// A request can be cancelled after the native operation has already
	// completed successfully. The native/orchestration result remains
	// authoritative in that race; only a reported cancellation is interrupted.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "interrupted"
	}
	if err != nil {
		return "failed"
	}
	return "success"
}

// terminalStatusAfterNative applies the outcome rule to an operation with one
// requested native child. A failure is completed_with_issues, not failed, only
// when there is positive evidence that the main work was done:
//
//   - the engine itself reported the task completed but with errors (a restore
//     carrying engines.RestoreCompletedWithErrors), or
//   - the native child succeeded and only a later Replicaro step failed, such
//     as output processing, local cleanup, or persisting a step result.
//
// Everything else stays failed, and cancellation stays interrupted. The caller
// decides nativeCompleted; for restores that is restoreNativeCompleted, which
// deliberately leaves out a Kopia exit 0 whose summary could not be read even
// though its native child succeeded (see engines.RestoreOutcomeUnknown). Warning-only
// application failures (notification delivery, rebuildable cache refresh,
// dashboard refresh, statistics) never reach err, so they leave the result
// unchanged. Do not widen nativeCompleted to "the process started" or to
// anything read from counts: Replicaro never infers a partial success.
func terminalStatusAfterNative(ctx context.Context, err error, nativeCompleted bool) string {
	status := terminalOperationStatus(ctx, err)
	if status == "failed" && nativeCompleted {
		return "completed_with_issues"
	}
	return status
}

// restoreNativeCompleted reports whether a restore's native child finished its
// work: the engine reported completed with errors, or the requested command
// succeeded.
//
// The one exception is a restore whose outcome is unknown: Kopia exited 0 (so
// its native step is succeeded) but the summary that says what was restored
// could not be read. That output-processing failure would otherwise count as
// "native success, follow-up failed" and become completed_with_issues, which
// would claim the restore was done with nothing to show it. It stays failed.
func restoreNativeCompleted(err error) bool {
	if engines.RestoreOutcomeIsUnknown(err) {
		return false
	}
	if err == nil || engines.RestoreReportedErrors(err) {
		return true
	}
	status, started, _, _, _, known := engines.RequestedOperationOutcome(err)
	return known && started && status == engines.RequestedOperationSucceeded
}

// finishOperationDurably retries ordinary transient failures. If it still
// fails, the running marker is intentionally preserved for startup recovery.
func finishOperationDurably(db *sql.DB, operationID, status, output string, finishedAt time.Time) error {
	var err error
	for attempt := 1; attempt <= 4; attempt++ {
		err = finishOperationWrite(db, operationID, status, output, finishedAt)
		if err == nil || errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if attempt < 4 {
			time.Sleep(time.Duration(attempt) * 25 * time.Millisecond)
		}
	}
	return fmt.Errorf("operation completed but final state requires startup reconciliation: %w", err)
}
