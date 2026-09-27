import { renderMessage, t } from "../i18n";
import { useEffect, useRef, useState } from "react";

import { updateJobSource } from "../services/api";
import type { BackupJob, JobSourceFolderCheck } from "../types";
import { DirectoryField } from "./DirectoryPicker";
import { Modal } from "./ui";

// "Update job source": the user picks the folder where the job's data now
// lives. The backend saves it as the job's alias and never replaces the
// immutable source (native retention and File History scope depend on it).
//
// The folder check is a warning with an override, not a gate. Picking the
// wrong folder (typically the empty folder left behind by an unmounted share)
// is the mistake it guards against, because backing that up under the job
// lets native retention expire the real snapshots; but the user may really
// have reorganized their data, and only they can tell. Cancel is the default
// action of the warning.
export function UpdateJobSourceDialog({
	job,
	onClose,
	onSaved,
	onError,
}: {
	job: BackupJob;
	onClose: () => void;
	onSaved: (job: BackupJob) => void;
	onError: (message: string) => void;
}) {
	// Starts empty on purpose. This dialog is offered because the current
	// source cannot be used, and the picker opens at the field's value: on a
	// dead share the first Browse would wait out the 60-second listing bound
	// before showing anything. An empty field opens the picker at its normal
	// default location instead.
	const [path, setPath] = useState("");
	const [busy, setBusy] = useState(false);
	const [check, setCheck] = useState<JobSourceFolderCheck | null>(null);
	// A response that arrives after the dialog closed must not reopen it or
	// apply a result the user walked away from.
	const open = useRef(true);
	useEffect(() => {
		open.current = true;
		return () => { open.current = false; };
	}, []);
	// Cancel is the warning's default action. Modal focuses its first control
	// when it mounts; this parent effect runs after that and moves focus.
	const cancelWarning = useRef<HTMLButtonElement>(null);
	useEffect(() => {
		if (check) cancelWarning.current?.focus();
	}, [check]);

	const submit = async (confirmed: boolean) => {
		if (busy || !path) return;
		setBusy(true);
		try {
			const result = await updateJobSource(job.id, path, confirmed);
			if (!open.current) return;
			if (result.saved && result.job) {
				onSaved(result.job);
				return;
			}
			if (result.check) setCheck(result.check);
		} catch (reason) {
			if (open.current) onError((reason as Error).message);
		} finally {
			if (open.current) setBusy(false);
		}
	};

	const close = () => {
		if (!busy) onClose();
	};

	if (check) {
		const reason = check.reason === "empty"
			? t("ui.protect.sourceCheck.empty")
			: check.reason === "name"
				? renderMessage("ui.protect.sourceCheck.nameDiffers", { chosenName: <strong key="chosen">{check.chosenName}</strong>, sourceName: <strong key="source">{check.sourceName}</strong> })
				: t("ui.protect.sourceCheck.items", { matched: check.matched, total: check.total });
		return (
			<Modal title={t("ui.protect.sourceCheck.title")} onClose={close}>
				<p className="modal-intro">{renderMessage("ui.protect.sourceCheck.selected", {
					chosen: <strong key="chosen" className="mono">{check.chosen}</strong>,
					current: <strong key="current" className="mono">{check.current}</strong>,
				})}</p>
				<p className="modal-intro">{reason}</p>
				<p className="muted">{t("ui.protect.sourceCheck.guidance")}</p>
				<div className="modal-footer">
					<button ref={cancelWarning} className="btn" disabled={busy} onClick={close}>{t("ui.pages.protect.cancel")}</button>
					<button className="btn primary" disabled={busy} onClick={() => void submit(true)}>{busy && <span className="spinner" />}{t("ui.protect.sourceCheck.continue")}</button>
				</div>
			</Modal>
		);
	}

	return (
		<Modal title={t("ui.protect.updateJobSource")} onClose={close}>
			<fieldset className="modal-workflow-fields" disabled={busy}>
				<label className="field"><span>{t("ui.pages.protect.source.data")}</span><DirectoryField ariaLabel={t("ui.pages.protect.source.data")} value={path} onChange={setPath} /></label>
			</fieldset>
			<div className="modal-footer">
				<button className="btn" disabled={busy} onClick={close}>{t("ui.pages.protect.cancel")}</button>
				<button className="btn primary" disabled={busy || !path} onClick={() => void submit(false)}>{busy && <span className="spinner" />}{t("ui.pages.protect.save")}</button>
			</div>
		</Modal>
	);
}
