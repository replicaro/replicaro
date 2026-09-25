import { renderMessage, t } from "../i18n";
import { useEffect, useLayoutEffect, useRef, useState } from "react";

import {
	closeRcloneAuthorization,
	continueRcloneAuthorization,
	statusRcloneAuthorization,
	startRcloneAuthorization,
} from "../services/api";
import type { RcloneAuthStatus } from "../services/api";

interface Props {
	provider: string;
	label: string;
	readyAction: "create vault" | "check existing vault" | "validate and save login";
	value: RcloneAuthStatus | null;
	disabled?: boolean;
	closeOnUnmount?: boolean;
	showDescription?: boolean;
	onChange: (status: RcloneAuthStatus | null) => void;
	onError: (message: string) => void;
}

export function RcloneAuthorization(props: Props) {
	// A provider change must discard the previous provider's local choice and
	// pending request owner even if a caller keeps this component mounted.
	return <RcloneAuthorizationForProvider key={props.provider} {...props} />;
}

function RcloneAuthorizationForProvider({
	provider,
	label,
	readyAction,
	value,
	disabled = false,
	closeOnUnmount = true,
	showDescription = true,
	onChange,
	onError,
}: Props) {
	const [busy, setBusy] = useState(false);
	const [answer, setAnswer] = useState("");
	const [authorizationUrl, setAuthorizationUrl] = useState("");
	const authorizationSession = useRef("");
	const sessionOnUnmount = useRef("");
	const authorizationGeneration = useRef(0);

	useLayoutEffect(() => {
		return () => {
			// A late native authorization response used to restore an abandoned
			// OneDrive session after Back, replacing a newer location choice.
			// Invalidate pending start/continue work during unmount or provider change.
			authorizationGeneration.current++;
		};
	}, [provider]);

	useEffect(() => {
		sessionOnUnmount.current = value?.sessionId ?? "";
	});

	useEffect(() => () => {
		if (closeOnUnmount && sessionOnUnmount.current) {
			void closeRcloneAuthorization(sessionOnUnmount.current);
		}
	}, [closeOnUnmount]);

	useEffect(() => {
		if (value?.status !== "waiting_browser" || value.sessionId !== authorizationSession.current) {
			setAuthorizationUrl("");
			authorizationSession.current = "";
		}
	}, [value?.sessionId, value?.status]);

	useEffect(() => {
		if (value?.status !== "waiting_browser" || !value.sessionId) return;
		let active = true;
		let inFlight = false;
		let timer = 0;
		const stop = () => {
			active = false;
			window.clearInterval(timer);
		};
		const poll = async () => {
			if (!active || inFlight) return;
			inFlight = true;
			try {
				const next = await statusRcloneAuthorization(value.sessionId);
				if (!active || next.status === "waiting_browser") return;
				stop();
				setAuthorizationUrl("");
				onChange(next);
				// A surveyed OneDrive account can have several working locations.
				// Keep the choice empty so Continue never silently picks the first.
				setAnswer(next.question?.id === "config_driveid" ? "" :
					(next.question?.defaultValue ?? next.question?.choices[0]?.value ?? ""));
			} catch (reason) {
				if (!active) return;
				stop();
				setAuthorizationUrl("");
				onChange(null);
				setAnswer("");
				onError((reason as Error).message);
			} finally {
				inFlight = false;
			}
		};
		timer = window.setInterval(() => void poll(), 1500);
		return stop;
	}, [value?.sessionId, value?.status, onChange, onError]);

	const acceptStatus = (next: RcloneAuthStatus) => {
		setAuthorizationUrl(next.authorizationUrl ?? "");
		authorizationSession.current = next.authorizationUrl ? next.sessionId : "";
		if (next.authorizationUrl) {
			const publicStatus = { ...next };
			delete publicStatus.authorizationUrl;
			onChange(publicStatus);
		} else {
			onChange(next);
		}
		setAnswer(next.question?.id === "config_driveid" ? "" :
			(next.question?.defaultValue ?? next.question?.choices[0]?.value ?? ""));
	};

	const closeStaleSession = (next: RcloneAuthStatus) => {
		// The parent could not close a session it had never received. A stale
		// response must not become the next create/connect authorization.
		if (next.sessionId) void closeRcloneAuthorization(next.sessionId).catch(() => undefined);
	};

	const start = async () => {
		if (busy || disabled) return;
		const generation = ++authorizationGeneration.current;
		setBusy(true);
		try {
			if (value?.sessionId) await closeRcloneAuthorization(value.sessionId);
			if (generation !== authorizationGeneration.current) return;
			setAuthorizationUrl("");
			onChange(null);
			const next = await startRcloneAuthorization(provider);
			if (generation !== authorizationGeneration.current) {
				closeStaleSession(next);
				return;
			}
			acceptStatus(next);
		} catch (reason) {
			if (generation !== authorizationGeneration.current) return;
			setAuthorizationUrl("");
			onChange(null);
			setAnswer("");
			onError((reason as Error).message);
		} finally {
			if (generation === authorizationGeneration.current) setBusy(false);
		}
	};

	const continueFlow = async () => {
		if (!value?.question || busy || disabled) return;
		const selected = value.question.id === "config_driveid" ? answer :
			(answer || value.question.defaultValue || value.question.choices[0]?.value);
		if (!selected) {
			onError(t("ui.rclone.chooseAccountOption"));
			return;
		}
		const generation = ++authorizationGeneration.current;
		setBusy(true);
		try {
			const next = await continueRcloneAuthorization(value.sessionId, selected);
			if (generation !== authorizationGeneration.current) {
				closeStaleSession(next);
				return;
			}
			acceptStatus(next);
		} catch (reason) {
			if (generation !== authorizationGeneration.current) return;
			setAuthorizationUrl("");
			onChange(null);
			setAnswer("");
			onError((reason as Error).message);
		} finally {
			if (generation === authorizationGeneration.current) setBusy(false);
		}
	};

	return (
		<section className="advanced-panel rclone-authorization">
			<div className="modal-section-label">{t("ui.rclone.accountTitle", { provider: label })}</div>
			{showDescription && <p>{renderMessage("ui.rclone.authorizationDescription", { provider: label, rcloneLink: <a href="https://github.com/rclone/rclone" target="_blank" rel="noreferrer">{t("ui.components.rcloneauthorization.rclone")}</a> })}</p>}
			{busy && <div className="inline-notice" role="status">
				<span className="spinner" /> {t("ui.rclone.finishInBrowser")}
			</div>}
			{value?.status === "waiting_browser" && authorizationUrl && <div className="inline-notice" role="status">
				<a href={authorizationUrl} target="_blank" rel="noreferrer">
					{t("ui.rclone.openAuthorizationPage", { provider: label })}
				</a>
				<div>{t("ui.components.rcloneauthorization.finish.authorization.in.the.browser.then.return.to.replicaro")}</div>
			</div>}
			{value?.status === "ready" && <div className="inline-notice" role="status">
				{readyAction === "create vault" ? t("ui.rclone.readyCreateVault") : readyAction === "check existing vault" ? t("ui.rclone.readyCheckVault") : t("ui.rclone.readySaveLogin")}
			</div>}
			{value?.status === "question" && value.question?.notice &&
				<div className="inline-error" role="alert">{value.question.notice}</div>}
			{value?.status === "question" && value.question && <label className="field">
				<span>{value.question.prompt || t("ui.rclone.chooseNativeOption")}</span>
				<select
					aria-label={value.question.id === "config_driveid" ? t("ui.rclone.oneDriveLocation") : t("ui.rclone.accountOption")}
					value={answer}
					disabled={busy || disabled}
					onChange={(event) => setAnswer(event.target.value)}
				>
					{value.question.id === "config_driveid" &&
						<option value="" disabled>{t("ui.components.rcloneauthorization.select.a.onedrive.location")}</option>}
					{value.question.choices.map((choice) =>
						<option key={choice.value} value={choice.value}>
							{choice.label || choice.value}
						</option>
					)}
				</select>
			</label>}
			<div className="tool-buttons">
				{value?.status === "question"
					? <button className="btn" disabled={busy || disabled ||
						(value.question?.id === "config_driveid" && !answer)}
						onClick={() => void continueFlow()}>
						{busy && <span className="spinner" />}{t("ui.rclone.continue")}
					</button>
					: value?.status !== "ready" && value?.status !== "waiting_browser" && <button className="btn" disabled={busy || disabled} onClick={() => void start()}>
						{busy && <span className="spinner" />}{t("ui.rclone.connectProvider", { provider: label })}
					</button>}
			</div>
		</section>
	);
}
