import { useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { renderMessage, t } from "../i18n";
import { APIError, getOperation, isOperationNotFound, requestDownloadTicket, startBrowserDownload } from "../services/api";
import type { DownloadTarget, DownloadTicket } from "../services/api";
import { firstStrongIsolate } from "../services/rcloneRemote";
import { operationIsActive } from "../services/trackedOperations";
import type { OperationEntry } from "../types";

// The text shown by the button when the backend refuses the download. The
// refusals a user can run into have their own text; anything else (a missing
// vault, an unfinished vault password change, a request that never reached
// Replicaro, and the Cold Storage refusal, which the pages don't normally
// reach because they offer no Download for a Cold Storage vault) gets the
// generic text with the reason the request gave. That reason is often English
// text from the backend or the engine, so it is isolated: on a right-to-left
// page it stays one piece instead of mixing into the sentence around it.
function downloadRefusalMessage(reason: unknown) {
	const code = reason instanceof APIError ? reason.code : undefined;
	switch (code) {
		case "vault_busy": return t("ui.download.vaultBusy");
		case "download_not_found": return t("ui.download.notFound");
		case "download_not_regular_file": return t("ui.download.notRegularFile");
		case "metadata_not_ready": return t("ui.restore.metadataNotReady");
		case "vault_being_removed": return t("ui.protect.vaultBeingRemoved");
		// Usually a reverse proxy that speaks HTTP/1.0 to Replicaro (nginx's
		// default); the text says what to change instead of showing the
		// backend's English reason.
		case "download_http_version": return t("ui.download.httpVersion");
	}
	return t("ui.download.failed", { reason: firstStrongIsolate((reason as Error)?.message ?? "") });
}

function targetKey(target: DownloadTarget | null) {
	return target ? JSON.stringify([target.repositoryId, target.snapshotId, target.nativeRootId, target.path]) : "";
}

// How often the download's operation is looked up: while waiting for the
// browser's request to record it, and then while it runs.
const START_LOOKUP_INTERVAL_MS = 500;
const RUNNING_LOOKUP_INTERVAL_MS = 2000;

type Outcome =
	| { kind: "refused"; text: string }
	| { kind: "notStarted" }
	| { kind: "failed"; operationId: string };

function wait(ms: number, signal: AbortSignal) {
	return new Promise<void>((resolve, reject) => {
		if (signal.aborted) {
			reject(signal.reason);
			return;
		}
		const timer = window.setTimeout(() => {
			signal.removeEventListener("abort", stop);
			resolve();
		}, ms);
		const stop = () => {
			window.clearTimeout(timer);
			reject(signal.reason);
		};
		signal.addEventListener("abort", stop, { once: true });
	});
}

// What one lookup of the download's operation answered: the operation, that
// nothing is recorded under the ID (the backend's 404), or nothing usable (the
// network, a restarting backend, any other error). Only the first two say
// anything about the download.
type Lookup = { operation: OperationEntry } | "notRecorded" | "unanswered";

async function lookUp(operationId: string, signal: AbortSignal): Promise<Lookup> {
	try {
		const operation = (await getOperation(operationId, signal)).find((entry) => entry.id === operationId);
		return operation ? { operation } : "unanswered";
	} catch (reason) {
		return isOperationNotFound(reason) ? "notRecorded" : "unanswered";
	}
}

// followDownload reads the download's operation until the download has ended
// and returns what the page shows about it: nothing for a success, a failure
// with a link to the operation's log for a failed or interrupted one, and
// "did not start" once the ticket's lifetime is over and the backend answers
// that nothing is recorded under the ID. A lookup that fails is never an
// outcome; the button keeps looking at the same pace.
//
// The browser's request is the only thing that records the download, and only
// with an unused, unexpired ticket. The record is written after that request
// has taken the vault lock, which can first wait for File History's indexing
// to give way, so a request that arrives near the end of the ticket's
// lifetime can be recorded shortly after the page has given up. The page then
// says the download did not start although it runs; it is listed on the
// Dashboard as usual.
//
// followDownload only reads: the download itself is never touched, and it
// runs for as long as the download does.
async function followDownload(ticket: DownloadTicket, signal: AbortSignal): Promise<Outcome | null> {
	const deadline = Date.now() + ticket.expiresInSeconds * 1000;
	let operation: OperationEntry | undefined;
	while (!operation) {
		await wait(START_LOOKUP_INTERVAL_MS, signal);
		const answer = await lookUp(ticket.operationId, signal);
		if (typeof answer === "object") operation = answer.operation;
		else if (answer === "notRecorded" && Date.now() >= deadline) return { kind: "notStarted" };
	}
	while (operationIsActive(operation)) {
		await wait(RUNNING_LOOKUP_INTERVAL_MS, signal);
		const answer = await lookUp(ticket.operationId, signal);
		if (typeof answer === "object") operation = answer.operation;
	}
	return operation.status === "success" ? null : { kind: "failed", operationId: ticket.operationId };
}

type Props = {
	// The file to download, or null while nothing is chosen yet (the button
	// is then shown disabled).
	target: DownloadTarget | null;
	// The button's accessible name. A list can hold several Download buttons,
	// so each one names its file.
	label: string;
	className?: string;
};

// FileDownloadButton asks the backend for a one-time ticket and hands its
// address to the browser, which then downloads the file itself; the browser
// shows the transfer. The ticket names the operation the download will be
// recorded under, and the button follows that operation: a spinner shows
// next to it from the click until the download has ended, or until the
// ticket's lifetime is over with nothing recorded. A refused ticket request,
// a download that did not start and a failed or interrupted download each
// show a message next to the button; the last links to the download's log on
// the Dashboard. A successful download only ends the spinner. The page never
// leaves by itself.
//
// While the button asks, waits or follows, it reports itself disabled
// (aria-disabled) and further clicks are ignored, so a double click asks once;
// it stays focusable.
//
// The message belongs to the file it was about. The control is keyed by its
// target, so choosing another backup or version for the item starts from a
// clean button, and removing the item drops the message with it. Either one
// also ends the following; the download carries on.
export function FileDownloadButton(props: Props) {
	return <DownloadControl key={targetKey(props.target)} {...props} />;
}

function DownloadControl({ target, label, className }: Props) {
	const [busy, setBusy] = useState(false);
	const [outcome, setOutcome] = useState<Outcome | null>(null);
	const busyRef = useRef(false);
	const following = useRef<AbortController | null>(null);
	useEffect(() => () => following.current?.abort(), []);

	const download = async () => {
		// The only guard against a second request: the button isn't disabled while busy, so it keeps keyboard focus.
		if (!target || busyRef.current) return;
		busyRef.current = true;
		setBusy(true);
		setOutcome(null);
		const controller = new AbortController();
		following.current = controller;
		try {
			let ticket: DownloadTicket;
			try {
				ticket = await requestDownloadTicket(target);
				startBrowserDownload(ticket.url);
			} catch (reason) {
				setOutcome({ kind: "refused", text: downloadRefusalMessage(reason) });
				return;
			}
			const result = await followDownload(ticket, controller.signal);
			if (!controller.signal.aborted) setOutcome(result);
		} catch {
			// Only an abort ends the following early: the control left the page.
		} finally {
			busyRef.current = false;
			setBusy(false);
		}
	};

	const message = outcome?.kind === "refused" ? outcome.text
		: outcome?.kind === "notStarted" ? t("ui.download.didNotStart")
			: outcome?.kind === "failed" ? renderMessage("ui.download.failedSeeLog", {
				logLink: <Link key="logLink" to={`/?operation=${encodeURIComponent(outcome.operationId)}`}>{t("ui.download.logLink")}</Link>,
			})
				: null;

	return <>
		<button type="button" className={className} disabled={!target} aria-disabled={busy || undefined} aria-label={label} onClick={() => void download()}>{t("ui.download.action")}</button>
		{busy && <span className="spinner" aria-hidden="true" />}
		{message && <span className="download-message" role="alert">{message}</span>}
	</>;
}
