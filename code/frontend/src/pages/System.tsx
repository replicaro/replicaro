import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation, useNavigate, useOutletContext } from "react-router-dom";

import { preventNumberInputWheel } from "../components/numberInput";
import { ConfirmDialog, Loading, Modal, SaveChangesDialog, useToast } from "../components/ui";
import { checkForAppUpdate, getEngines, getPlatform, getSettings, getSupportReport, openAppUpdateDownload, saveSettings } from "../services/api";
import type { AppUpdateStatus, EngineDescriptor, PlatformInfo, Settings as SettingsType } from "../types";
import type { AppUpdateOutletContext } from "../layouts/MainLayout";
import { applyThemePreference } from "../theme";
import { activateLocale, languageNames, t } from "../i18n";

const supportReportByteLimit = 256 * 1024;
type PendingSaveAction = "save" | "save-and-leave";
let supportClipboardTail: Promise<void> = Promise.resolve();

function writeSupportReportToClipboard(report: string): Promise<void> {
    const write = supportClipboardTail.then(() => navigator.clipboard.writeText(report));
    supportClipboardTail = write.catch(() => undefined);
    return write;
}

function capSupportReport(value: string): string {
    const encoder = new TextEncoder();
    if (encoder.encode(value).byteLength <= supportReportByteLimit) return value;
    const suffix = "\n- [report truncated at 256 KiB]\n";
    const budget = supportReportByteLimit - encoder.encode(suffix).byteLength;
    let used = 0;
    let result = "";
    for (const character of value) {
        const size = encoder.encode(character).byteLength;
        if (used + size > budget) break;
        result += character;
        used += size;
    }
    return result + suffix;
}

function composeSupportReport(report: string, engines: EngineDescriptor[]): string {
    const engineLines = engines.map((engine) => {
        const status = engine.installed && !engine.error ? "ready" : engine.path ? "unavailable" : "missing";
        return `- ${engine.name}: version=${engine.version || "unknown"} status=${status}`;
    });
    const section = `Engine status:\n${engineLines.length ? engineLines.join("\n") : "- No engine status was loaded."}\n`;
    const architectureLine = /(^Architecture:.*\n)/m;
    const combined = architectureLine.test(report)
        ? report.replace(architectureLine, (line) => line + section)
        : `${report.replace(/\s*$/, "")}\n${section}`;
    return capSupportReport(combined);
}

function sameSettings(left: SettingsType, right: SettingsType): boolean {
    return JSON.stringify(left) === JSON.stringify(right);
}

export default function System() {
    const toast = useToast();
    const location = useLocation();
    const navigate = useNavigate();
    const updateContext = useOutletContext<AppUpdateOutletContext | null>();
    const [engines, setEngines] = useState<EngineDescriptor[]>([]);
    const [settings, setSettings] = useState<SettingsType | null>(null);
    const [savedSettings, setSavedSettings] = useState<SettingsType | null>(null);
    const [platform, setPlatform] = useState<PlatformInfo | null>(null);
    const [version, setVersion] = useState("");
    const [saving, setSaving] = useState(false);
    const [pendingNavigation, setPendingNavigation] = useState<string | null>(null);
    const [pendingSaveAction, setPendingSaveAction] = useState<PendingSaveAction | null>(null);
    const [manualUpdateStatus, setManualUpdateStatus] = useState<AppUpdateStatus | null>(null);
    const [checkingForUpdate, setCheckingForUpdate] = useState(false);
    const [supportOpen, setSupportOpen] = useState(false);
    const [supportLoading, setSupportLoading] = useState(false);
    const [supportCopying, setSupportCopying] = useState(false);
    const [supportReport, setSupportReport] = useState("");
    const [supportError, setSupportError] = useState("");
    const [copyStatus, setCopyStatus] = useState("");
    const [copySucceeded, setCopySucceeded] = useState(false);
    const supportGeneration = useRef(0);
    const supportAbort = useRef<AbortController | null>(null);
    const supportCopyAttempt = useRef(0);
    const supportCopyPending = useRef(false);
    const historyIndex = useRef<number | null>(typeof window === "undefined" ? null : window.history.state?.idx ?? null);
    const restoringHistory = useRef(false);

    const load = useCallback(() => {
        void Promise.all([getEngines(), getSettings(), typeof getPlatform === "function" ? getPlatform() : Promise.resolve(null)]).then(([catalog, nextSettings, nextPlatform]) => {
            setEngines(catalog.engines);
            setVersion(catalog.replicaroVersion);
            setSettings(nextSettings);
            setSavedSettings(nextSettings);
            setPlatform(nextPlatform);
			applyThemePreference(nextSettings.theme);
            activateLocale(nextSettings.effectiveLocale);
        }).catch((error: Error) => toast("error", error.message));
    }, [toast]);

    useEffect(() => { void load(); }, [load]);
    useEffect(() => () => {
        supportGeneration.current++;
        supportCopyAttempt.current++;
        supportAbort.current?.abort();
    }, []);

    const save = async (resetEnglish = false) => {
        if (!settings || !savedSettings || saving) return false;
        // The recovery button saves only the language. Other edits stay in the
        // draft and still pass through the normal Save settings confirmations.
		const attemptedSettings: SettingsType = resetEnglish
            ? { ...savedSettings, language: "en", effectiveLocale: "en" }
            : settings;
		const previouslySavedSettings = savedSettings;
        setSaving(true);
        try {
            await saveSettings(attemptedSettings);
            // The backend owns OS-language resolution. Re-read only when the
            // language preference changed so the selected catalog follows its
            // authoritative effectiveLocale without consulting the browser.
            let confirmedSettings = attemptedSettings;
            if ((attemptedSettings.language ?? "system") !== (previouslySavedSettings?.language ?? "system")) {
                try { confirmedSettings = await getSettings(); }
                catch { /* The saved choice is durable; the next load retries locale resolution. */ }
            }
            setSavedSettings(confirmedSettings);
            setSettings((current) => {
                if (!current) return current;
                if (resetEnglish) return { ...current, language: confirmedSettings.language, effectiveLocale: confirmedSettings.effectiveLocale };
                return sameSettings(current, attemptedSettings) ? confirmedSettings : current;
            });
            activateLocale(confirmedSettings.effectiveLocale);
            toast("ok", t("ui.system.settingsSaved"));
            return true;
        } catch (error) {
            toast("error", (error as Error).message);
			let committed = false;
			try {
				const persistedSettings = await getSettings();
				if (!previouslySavedSettings || !sameSettings(persistedSettings, previouslySavedSettings)) {
					committed = true;
					setSavedSettings(persistedSettings);
                    if (resetEnglish) activateLocale(persistedSettings.effectiveLocale);
					setSettings((currentSettings) => {
                        if (resetEnglish && currentSettings) return { ...currentSettings, language: persistedSettings.language, effectiveLocale: persistedSettings.effectiveLocale };
						if (!currentSettings || !sameSettings(currentSettings, attemptedSettings)) return currentSettings;
						applyThemePreference(persistedSettings.theme);
                        activateLocale(persistedSettings.effectiveLocale);
						return persistedSettings;
					});
				}
			} catch {
				// Keep the draft when the authoritative state cannot be confirmed.
			}
			return committed;
        }
        finally { setSaving(false); }
    };

    const checkForUpdate = async () => {
        setCheckingForUpdate(true);
        try {
            const status = await checkForAppUpdate();
            setManualUpdateStatus(status);
            updateContext?.presentManualUpdateResult(status);
        } catch (error) {
            toast("error", (error as Error).message);
        } finally {
            setCheckingForUpdate(false);
        }
    };

    const openManualUpdate = async () => {
        if (!manualUpdateStatus?.availableVersion) return;
        updateContext?.dismissUpdateVersion(manualUpdateStatus.availableVersion);
        try {
            await openAppUpdateDownload(manualUpdateStatus.availableVersion);
        } catch (error) {
            toast("error", (error as Error).message);
        }
    };

    const openSupportReport = async () => {
        const generation = ++supportGeneration.current;
        supportCopyAttempt.current++;
        supportAbort.current?.abort();
        const controller = new AbortController();
        supportAbort.current = controller;
        setSupportOpen(true);
        setSupportLoading(true);
        setSupportReport("");
        setSupportError("");
        setCopyStatus("");
        try {
            const response = await getSupportReport(controller.signal);
            if (supportGeneration.current !== generation) return;
            setSupportReport(composeSupportReport(response.report, engines));
        } catch (error) {
            if (supportGeneration.current !== generation || (error instanceof DOMException && error.name === "AbortError")) return;
            setSupportError((error as Error).message);
        } finally {
            if (supportGeneration.current === generation) setSupportLoading(false);
        }
    };

    const closeSupportReport = () => {
        supportGeneration.current++;
        supportCopyAttempt.current++;
        supportAbort.current?.abort();
        supportAbort.current = null;
        setSupportOpen(false);
        setSupportLoading(false);
    };

    const copySupportReport = async () => {
        if (!supportReport || supportCopyPending.current) return;
        const generation = supportGeneration.current;
        const attempt = ++supportCopyAttempt.current;
        const report = supportReport;
        supportCopyPending.current = true;
        setSupportCopying(true);
        setCopyStatus("");
        let nextStatus: string;
        let nextSucceeded: boolean;
        try {
            await writeSupportReportToClipboard(report);
            nextStatus = t("ui.system.logCopied");
            nextSucceeded = true;
        } catch {
            nextStatus = t("ui.system.logCopyFailed");
            nextSucceeded = false;
        } finally {
            supportCopyPending.current = false;
            setSupportCopying(false);
        }
        if (supportGeneration.current === generation && supportCopyAttempt.current === attempt) {
            setCopySucceeded(nextSucceeded);
            setCopyStatus(nextStatus);
        }
    };

    const dirty = Boolean(settings && savedSettings && !sameSettings(settings, savedSettings));

    useEffect(() => {
        if (!dirty) return;
        const currentPath = `${location.pathname}${location.search}${location.hash}`;
        const onClick = (event: MouseEvent) => {
            if (event.defaultPrevented || event.button !== 0 || event.metaKey || event.ctrlKey || event.shiftKey || event.altKey) return;
            if (!(event.target instanceof Element)) return;
            const anchor = event.target.closest("a[href]");
            if (!(anchor instanceof HTMLAnchorElement) || (anchor.target && anchor.target !== "_self") || anchor.hasAttribute("download")) return;
            const target = new URL(anchor.href, window.location.href);
            if (target.origin !== window.location.origin) return;
            const targetPath = `${target.pathname}${target.search}${target.hash}`;
            if (targetPath === currentPath) return;
            event.preventDefault();
            event.stopPropagation();
            setPendingNavigation(targetPath);
        };
        const onPopState = (event: PopStateEvent) => {
            if (restoringHistory.current) {
                restoringHistory.current = false;
                return;
            }
            const targetPath = `${window.location.pathname}${window.location.search}${window.location.hash}`;
            if (targetPath === currentPath) return;
            event.stopImmediatePropagation();
            setPendingNavigation(targetPath);
            const targetIndex = event.state?.idx;
            if (typeof historyIndex.current === "number" && typeof targetIndex === "number") {
                restoringHistory.current = true;
                window.history.go(historyIndex.current - targetIndex);
            } else {
                window.history.pushState(window.history.state, "", currentPath);
            }
        };
        const onBeforeUnload = (event: BeforeUnloadEvent) => {
            event.preventDefault();
            event.returnValue = "";
        };
        window.addEventListener("click", onClick, true);
        window.addEventListener("popstate", onPopState, true);
        window.addEventListener("beforeunload", onBeforeUnload);
        return () => {
            window.removeEventListener("click", onClick, true);
            window.removeEventListener("popstate", onPopState, true);
            window.removeEventListener("beforeunload", onBeforeUnload);
        };
    }, [dirty, location.hash, location.pathname, location.search]);

    const discardAndLeave = () => {
        const target = pendingNavigation;
		if (savedSettings) {
			setSettings(savedSettings);
			applyThemePreference(savedSettings.theme);
		}
        setPendingNavigation(null);
        if (target) navigate(target);
    };

    const performSaveAction = async (action: PendingSaveAction) => {
        const target = pendingNavigation;
        if (await save()) {
            if (action === "save-and-leave") {
                setPendingNavigation(null);
                if (target) navigate(target);
            }
        }
    };

    const requestSave = (action: PendingSaveAction) => {
        if (!settings) return;
        // Confirmation is tied to a changed high value, not merely a high
        // persisted value. Keeping the pending action transient prevents a
        // cancel from committing settings or changing navigation state.
        if (settings.maxConcurrentJobRuns > 5 &&
                settings.maxConcurrentJobRuns !== savedSettings?.maxConcurrentJobRuns) {
            setPendingSaveAction(action);
            return;
        }
        void performSaveAction(action);
    };

    const confirmHighConcurrencySave = () => {
        const action = pendingSaveAction;
        setPendingSaveAction(null);
        if (action) void performSaveAction(action);
    };

    if (!settings) return <div className="page"><Loading /></div>;

    return <div className="page system-page">
        <header className="page-header simple"><h1 className="page-title">{t("ui.pages.system.system")}</h1><p className="page-desc">{t("ui.pages.system.settings.and.housekeeping")}</p></header>
        <div className="system-list">
            <div className="system-label">{t("ui.pages.system.version")}</div><section className="system-section"><div className="system-value mono">{version || t("ui.system.unknownVersion")}</div></section>
            <div className="system-label">{t("ui.pages.system.updates")}</div><section className="system-section app-update-settings">
                <button type="button" className="btn" disabled={checkingForUpdate} onClick={() => void checkForUpdate()}>{checkingForUpdate ? t("ui.pages.system.checking") : t("ui.pages.system.check.for.update.now")}</button>
				<div>
					<label className="check"><input type="checkbox" aria-describedby="automatic-update-help" checked={!(settings.disableAutomaticUpdateChecks ?? false)} onChange={(event) => setSettings({ ...settings, disableAutomaticUpdateChecks: !event.target.checked })} />{t("ui.pages.system.automatically.check.for.updates")}</label>
                    <div id="automatic-update-help" className="setting-hint">{t("ui.pages.system.replicaro.notifies.you.of.an.update.but.never.downloads.an.update.by.i")}</div>
				</div>
                {manualUpdateStatus?.result === "up_to_date" && <div className="inline-notice">{t("ui.system.updateCurrent", { version: manualUpdateStatus.runningVersion })}</div>}
                {manualUpdateStatus?.result === "unavailable" && <div className="inline-error">{t("ui.system.updateUnavailable")}</div>}
                {manualUpdateStatus?.result === "update_available" && manualUpdateStatus.availableVersion && <div className="inline-notice app-update-manual-result">
                    {manualUpdateStatus.availableVersionSkipped ? t("ui.system.updateAvailableSkipped", { version: manualUpdateStatus.availableVersion }) : t("ui.system.updateAvailable", { version: manualUpdateStatus.availableVersion })}
                    <button type="button" className="btn sm" onClick={() => void openManualUpdate()}>{t("ui.pages.system.open.download.page")}</button>
                </div>}
            </section>
            <div className="system-label">{t("ui.pages.system.support")}</div><section className="system-section support-section"><button type="button" className="btn" onClick={() => void openSupportReport()}>{t("ui.pages.system.report.bugs.or.errors")}</button><div className="setting-hint">{t("ui.pages.system.help.improve.replicaro.by.reporting.bugs.and.errors.your.private.infor")}</div></section>
			<div className="system-label">{t("ui.pages.system.appearance")}</div>
			<section className="system-section">
				<label className="field system-control-field"><span>{t("ui.pages.system.theme")}</span><select value={settings.theme} onChange={(event) => { const theme = event.target.value as SettingsType["theme"]; applyThemePreference(theme); setSettings({ ...settings, theme }); }}><option value="system">{t("ui.pages.system.system")}</option><option value="light">{t("ui.pages.system.light")}</option><option value="neutral">{t("ui.pages.system.neutral")}</option><option value="dark">{t("ui.pages.system.dark")}</option></select></label>
				<div className="setting-hint system-control-hint">{t("ui.pages.system.system.follows.your.operating.system.s.light.or.dark.theme.setting")}</div>
                <div className="system-language-controls">
                    <label className="field system-control-field system-language-field"><span>{t("system.language.label")}</span><select value={settings.language ?? "system"} disabled={saving} onChange={(event) => setSettings({ ...settings, language: event.target.value as SettingsType["language"] })}><option value="system">{t("system.language.system")}</option><option value="en">{t("system.language.english")}</option>{Object.entries(languageNames).map(([locale, name]) => <option key={locale} value={locale} lang={locale}>{name}</option>)}</select></label>
                    {/* Keep this recovery action in English so it remains recognizable in every UI language. */}
                    <button type="button" className="btn" lang="en" dir="ltr" translate="no" disabled={saving} onClick={() => void save(true)}>Reset to English</button>
                </div>
			</section>
            <div className="system-label">{t("ui.pages.system.default.engine.for.backups")}</div>
            <section className="system-section">
                <label className="field system-control-field"><span>{t("ui.pages.system.engine.used.for.new.vaults")}</span><select value={settings.defaultEngine} onChange={(event) => setSettings({ ...settings, defaultEngine: event.target.value as SettingsType["defaultEngine"] })}><option value="restic">{t("ui.pages.system.restic")}</option><option value="kopia">{t("ui.pages.system.kopia")}</option></select></label>
                <div className="setting-hint system-control-hint">{t("ui.pages.system.select.restic.if.you.are.unsure.which.engine.to.use.this.setting.does")}</div>
            </section>
            <div className="system-label">{t("ui.pages.system.backup.engine.status")}</div><section className="system-section"><div className="engine-cards">{engines.map((engine) => <div className={`engine-state${engine.installed ? " ready" : " missing"}`} key={engine.id}><span className="engine-status-dot" /><div className="engine-details"><strong>{engine.name}{engine.version ? ` · ${engine.version}` : ""}</strong><small>{t("ui.system.engineProtectionSummary", { compression: engine.compression, encryption: engine.encryption })}</small>{engine.compatibilityWarning && <div className="inline-error">{engine.compatibilityWarning}</div>}</div></div>)}</div></section>
            {(platform?.capabilities.startAtLogin ?? true) && <><div className="system-label">{t("ui.pages.system.startup")}</div><section className="system-section"><label className="check"><input type="checkbox" checked={settings.startAtLogin ?? settings.startWithWindows} onChange={(event) => setSettings({ ...settings, startAtLogin: event.target.checked, startWithWindows: event.target.checked })} />{t("ui.system.startAtLogin")}</label></section></>}
            <div className="system-label">{t("ui.pages.system.housekeeping")}</div><section className="system-section"><label className="field compact-field"><span>{t("ui.pages.system.keep.logs.for.days")}</span><input type="number" min={1} value={settings.logRetentionDays} onWheel={preventNumberInputWheel} onChange={(event) => setSettings({ ...settings, logRetentionDays: parseInt(event.target.value, 10) || 30 })} /></label></section>
            <div className="system-label">{t("ui.pages.system.backup.admission")}</div><section className="system-section"><label className="field system-control-field"><span>{t("ui.pages.system.maximum.simultaneous.backup.runs")}</span><input type="number" min={1} max={32} value={settings.maxConcurrentJobRuns} onWheel={preventNumberInputWheel} onChange={(event) => setSettings({ ...settings, maxConcurrentJobRuns: Math.min(32, Math.max(1, parseInt(event.target.value, 10) || 2)) })} /></label></section>
            <div className="system-label">{t("ui.pages.system.notifications")}</div><section className="system-section notifications-section">{(platform?.capabilities.nativeNotifications ?? true) && <div className="notification-channel"><div className="notification-channel-title">{t("ui.pages.system.desktop")}</div><label className="check"><input type="checkbox" checked={settings.nativeNotificationsOnFailure ?? settings.notifyWindowsOnFailure} onChange={(event) => setSettings({ ...settings, nativeNotificationsOnFailure: event.target.checked, notifyWindowsOnFailure: event.target.checked })} />{t("ui.system.notifyFailures")}</label><label className="check"><input type="checkbox" checked={settings.nativeNotificationsOnSuccess ?? settings.notifyWindowsOnSuccess} onChange={(event) => setSettings({ ...settings, nativeNotificationsOnSuccess: event.target.checked, notifyWindowsOnSuccess: event.target.checked })} />{t("ui.system.notifySuccess")}</label></div>}<div className="notification-channel"><div className="notification-channel-title">{t("ui.pages.system.webhooks")}</div><label className="field system-wide-field"><span>{t("ui.pages.system.webhook.url")}</span><input type="url" value={settings.webhookUrl} onChange={(event) => setSettings({ ...settings, webhookUrl: event.target.value })} /><small>{t("ui.pages.system.leave.blank.if.not.using.webhooks")}</small></label><label className="check"><input type="checkbox" checked={settings.notifyWebhookOnFailure} onChange={(event) => setSettings({ ...settings, notifyWebhookOnFailure: event.target.checked })} />{t("ui.system.notifyFailures")}</label><label className="check"><input type="checkbox" checked={settings.notifyWebhookOnSuccess} onChange={(event) => setSettings({ ...settings, notifyWebhookOnSuccess: event.target.checked })} />{t("ui.system.notifySuccess")}</label></div><button className="btn primary save-settings" disabled={saving} onClick={() => requestSave("save")}>{saving ? t("ui.system.saving") : t("ui.system.saveSettings")}</button></section>
        </div>
        {supportOpen && <Modal title={t("ui.pages.system.report.bugs.or.errors")} onClose={closeSupportReport} wide><div className="support-report-modal">
            <p className="modal-intro">{t("ui.pages.system.submit.an.issue.on.github.to.report.a.replicaro.bug.error.you.will.nee")}</p>
            <div className="inline-warning support-report-warning">{t("ui.pages.system.computer.names.and.recognizable.paths.and.filenames.are.removed.from.t")}</div>
            {supportLoading && !supportReport && <Loading />}
            {supportError && <div className="inline-error" role="alert">{t("ui.system.supportReportFailed", { error: supportError })}</div>}
            {supportReport && <textarea className="support-report-preview mono" aria-label={t("ui.pages.system.diagnostic.error.log.preview")} readOnly value={supportReport} />}
            {copyStatus && <div className={copySucceeded ? "inline-notice" : "inline-error"} role="status">{copyStatus}</div>}
            <div className="modal-footer">
                <button type="button" className="btn primary" disabled={!supportReport || supportLoading || supportCopying} onClick={() => void copySupportReport()}>{t("ui.pages.system.copy.error.log")}</button>
                <a className="btn" href={supportReport && !supportLoading ? "https://github.com/replicaro/replicaro/issues/new" : undefined} target="_blank" rel="noreferrer" role="link" aria-disabled={!supportReport || supportLoading} tabIndex={supportReport && !supportLoading ? 0 : -1} onClick={() => { if (supportReport && !supportLoading) void copySupportReport(); }}>{t("ui.pages.system.open.github.issue")}</a>
            </div>
        </div></Modal>}
        {pendingSaveAction && <ConfirmDialog title={t("ui.pages.system.confirm.backup.admission.limit")} message={t("ui.system.highBackupConfirmation")} confirmLabel={t("ui.system.saveLimit")} busy={saving} onConfirm={confirmHighConcurrencySave} onCancel={() => setPendingSaveAction(null)} />}
        {pendingNavigation && !pendingSaveAction && <SaveChangesDialog onSave={() => requestSave("save-and-leave")} onDiscard={discardAndLeave} onCancel={() => setPendingNavigation(null)} busy={saving} />}
    </div>;
}
