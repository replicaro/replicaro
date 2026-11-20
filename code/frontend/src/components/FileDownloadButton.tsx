import { useRef, useState } from "react";

import { t } from "../i18n";
import { APIError, requestDownloadTicket, startBrowserDownload } from "../services/api";
import type { DownloadTarget } from "../services/api";
import { firstStrongIsolate } from "../services/rcloneRemote";

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
// address to the browser, which then downloads the file itself. While the
// ticket request runs the button reports itself disabled (aria-disabled) and
// further clicks are ignored, so a double click asks once. A refusal shows as
// a message next to the button and starts nothing. There is no progress
// display here: the browser shows the transfer, and the dashboard shows the
// download operation.
//
// The message belongs to the file it was about. The control is keyed by its
// target, so choosing another backup or version for the item starts from a
// clean button, and removing the item drops the message with it.
export function FileDownloadButton(props: Props) {
	return <DownloadControl key={targetKey(props.target)} {...props} />;
}

function DownloadControl({ target, label, className }: Props) {
	const [asking, setAsking] = useState(false);
	const [message, setMessage] = useState("");
	const askingRef = useRef(false);

	const download = async () => {
		// The only guard against a second request: the button isn't disabled while asking, so it keeps keyboard focus.
		if (!target || askingRef.current) return;
		askingRef.current = true;
		setAsking(true);
		setMessage("");
		try {
			const ticket = await requestDownloadTicket(target);
			startBrowserDownload(ticket.url);
		} catch (reason) {
			setMessage(downloadRefusalMessage(reason));
		} finally {
			askingRef.current = false;
			setAsking(false);
		}
	};

	return <>
		<button type="button" className={className} disabled={!target} aria-disabled={asking || undefined} aria-label={label} onClick={() => void download()}>{t("ui.download.action")}</button>
		{message && <span className="download-message" role="alert">{message}</span>}
	</>;
}
