import { useCallback, useEffect, useRef, useState } from "react";
import { useLocation, useNavigate, useOutletContext } from "react-router-dom";

import { preventNumberInputWheel } from "../components/numberInput";
import { Loading, Modal, SaveChangesDialog, useToast } from "../components/ui";
import { checkForAppUpdate, getEngines, getPlatform, getSettings, getSupportReport, openAppUpdateDownload, saveSettings } from "../services/api";
import type { AppUpdateStatus, EngineDescriptor, PlatformInfo, Settings as SettingsType } from "../types";
import type { AppUpdateOutletContext } from "../layouts/MainLayout";
import { applyThemePreference } from "../theme";

const supportReportByteLimit = 256 * 1024;
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
    const [manualUpdateStatus, setManualUpdateStatus] = useState<AppUpdateStatus | null>(null);
    const [checkingForUpdate, setCheckingForUpdate] = useState(false);
    const [supportOpen, setSupportOpen] = useState(false);
    const [supportLoading, setSupportLoading] = useState(false);
    const [supportCopying, setSupportCopying] = useState(false);
    const [supportReport, setSupportReport] = useState("");
    const [supportError, setSupportError] = useState("");
    const [copyStatus, setCopyStatus] = useState("");
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
        }).catch((error: Error) => toast("error", error.message));
    }, [toast]);

    useEffect(() => { void load(); }, [load]);
    useEffect(() => () => {
        supportGeneration.current++;
        supportCopyAttempt.current++;
        supportAbort.current?.abort();
    }, []);

    const save = async () => {
        if (!settings) return false;
		const attemptedSettings = settings;
		const previouslySavedSettings = savedSettings;
        setSaving(true);
        try {
            await saveSettings(attemptedSettings);
            setSavedSettings(attemptedSettings);
            toast("ok", "Settings saved and applied");
            return true;
        } catch (error) {
            toast("error", (error as Error).message);
			let committed = false;
			try {
				const persistedSettings = await getSettings();
				if (!previouslySavedSettings || !sameSettings(persistedSettings, previouslySavedSettings)) {
					committed = true;
					setSavedSettings(persistedSettings);
					setSettings((currentSettings) => {
						if (!currentSettings || !sameSettings(currentSettings, attemptedSettings)) return currentSettings;
						applyThemePreference(persistedSettings.theme);
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
        try {
            await writeSupportReportToClipboard(report);
            nextStatus = "Error log copied to the clipboard.";
        } catch {
            nextStatus = "Replicaro could not copy the error log. Select the preview and copy it manually.";
        } finally {
            supportCopyPending.current = false;
            setSupportCopying(false);
        }
        if (supportGeneration.current === generation && supportCopyAttempt.current === attempt) {
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

    const saveAndLeave = async () => {
        const target = pendingNavigation;
        if (await save()) {
            setPendingNavigation(null);
            if (target) navigate(target);
        }
    };

    if (!settings) return <div className="page"><Loading /></div>;

    return <div className="page system-page">
        <header className="page-header simple"><h1 className="page-title">System</h1><p className="page-desc">Settings and housekeeping</p></header>
        <div className="system-list">
            <div className="system-label">Version</div><section className="system-section"><div className="system-value mono">{version || "Unknown"}</div></section>
            <div className="system-label">Updates</div><section className="system-section app-update-settings">
                <button type="button" className="btn" disabled={checkingForUpdate} onClick={() => void checkForUpdate()}>{checkingForUpdate ? "Checking…" : "Check for update now"}</button>
				<div>
					<label className="check"><input type="checkbox" aria-describedby="automatic-update-help" checked={!(settings.disableAutomaticUpdateChecks ?? false)} onChange={(event) => setSettings({ ...settings, disableAutomaticUpdateChecks: !event.target.checked })} />Automatically check for updates</label>
                    <div id="automatic-update-help" className="setting-hint">Replicaro notifies you of an update but never downloads an update by itself.</div>
				</div>
                {manualUpdateStatus?.result === "up_to_date" && <div className="inline-notice">Replicaro {manualUpdateStatus.runningVersion} is up to date.</div>}
                {manualUpdateStatus?.result === "unavailable" && <div className="inline-error">Update status is unavailable. Replicaro could not check the update service.</div>}
                {manualUpdateStatus?.result === "update_available" && manualUpdateStatus.availableVersion && <div className="inline-notice app-update-manual-result">
                    Replicaro {manualUpdateStatus.availableVersion} is available.{manualUpdateStatus.availableVersionSkipped ? " You previously skipped this version; automatic notices remain suppressed." : ""}
                    <button type="button" className="btn sm" onClick={() => void openManualUpdate()}>Open download page</button>
                </div>}
            </section>
            <div className="system-label">Support</div><section className="system-section support-section"><button type="button" className="btn" onClick={() => void openSupportReport()}>Report bugs or errors</button><div className="setting-hint">Help improve Replicaro by reporting bugs and errors. Your private information (passwords, keys, files, backed up data, etc.) is never included in your report; only anonymized logs are shared.</div></section>
			<div className="system-label">Appearance</div>
			<section className="system-section">
				<label className="field system-control-field"><span>Theme</span><select value={settings.theme} onChange={(event) => { const theme = event.target.value as SettingsType["theme"]; applyThemePreference(theme); setSettings({ ...settings, theme }); }}><option value="system">System</option><option value="light">Light</option><option value="neutral">Neutral</option><option value="dark">Dark</option></select></label>
				<div className="setting-hint system-control-hint">System follows your operating system’s light or dark theme setting.</div>
			</section>
            <div className="system-label">Default Engine for Backups</div>
            <section className="system-section">
                <label className="field system-control-field"><span>Engine used for new vaults</span><select value={settings.defaultEngine} onChange={(event) => setSettings({ ...settings, defaultEngine: event.target.value as SettingsType["defaultEngine"] })}><option value="restic">Restic</option><option value="kopia">Kopia</option></select></label>
                <div className="setting-hint system-control-hint">Select Restic if you are unsure which engine to use. This setting does not change the engine for existing vaults.</div>
            </section>
            <div className="system-label">Backup Engine Status</div><section className="system-section"><div className="engine-cards">{engines.map((engine) => <div className={`engine-state${engine.installed ? " ready" : " missing"}`} key={engine.id}><span className="engine-status-dot" /><div className="engine-details"><strong>{engine.name}{engine.version ? ` · ${engine.version}` : ""}</strong><small>Compression: {engine.compression} · Encryption: {engine.encryption}</small>{engine.compatibilityWarning && <div className="inline-error">{engine.compatibilityWarning}</div>}</div></div>)}</div></section>
            {(platform?.capabilities.startAtLogin ?? true) && <><div className="system-label">Startup</div><section className="system-section"><label className="check"><input type="checkbox" checked={settings.startAtLogin ?? settings.startWithWindows} onChange={(event) => setSettings({ ...settings, startAtLogin: event.target.checked, startWithWindows: event.target.checked })} />Start Replicaro at login</label></section></>}
            <div className="system-label">Housekeeping</div><section className="system-section"><label className="field compact-field"><span>Keep logs for (days)</span><input type="number" min={1} value={settings.logRetentionDays} onWheel={preventNumberInputWheel} onChange={(event) => setSettings({ ...settings, logRetentionDays: parseInt(event.target.value, 10) || 30 })} /></label></section>
            <div className="system-label">Backup admission</div><section className="system-section"><label className="field system-control-field"><span>Maximum simultaneous backup runs</span><input type="number" min={1} max={32} value={settings.maxConcurrentJobRuns} onWheel={preventNumberInputWheel} onChange={(event) => setSettings({ ...settings, maxConcurrentJobRuns: Math.min(32, Math.max(1, parseInt(event.target.value, 10) || 2)) })} /></label></section>
            <div className="system-label">Notifications</div><section className="system-section notifications-section">{(platform?.capabilities.nativeNotifications ?? true) && <div className="notification-channel"><div className="notification-channel-title">Desktop</div><label className="check"><input type="checkbox" checked={settings.nativeNotificationsOnFailure ?? settings.notifyWindowsOnFailure} onChange={(event) => setSettings({ ...settings, nativeNotificationsOnFailure: event.target.checked, notifyWindowsOnFailure: event.target.checked })} />Notify on failures</label><label className="check"><input type="checkbox" checked={settings.nativeNotificationsOnSuccess ?? settings.notifyWindowsOnSuccess} onChange={(event) => setSettings({ ...settings, nativeNotificationsOnSuccess: event.target.checked, notifyWindowsOnSuccess: event.target.checked })} />Notify on success</label></div>}<div className="notification-channel"><div className="notification-channel-title">Webhooks</div><label className="field system-wide-field"><span>Webhook URL</span><input type="url" value={settings.webhookUrl} onChange={(event) => setSettings({ ...settings, webhookUrl: event.target.value })} /><small>Leave blank if not using webhooks</small></label><label className="check"><input type="checkbox" checked={settings.notifyWebhookOnFailure} onChange={(event) => setSettings({ ...settings, notifyWebhookOnFailure: event.target.checked })} />Notify on failures</label><label className="check"><input type="checkbox" checked={settings.notifyWebhookOnSuccess} onChange={(event) => setSettings({ ...settings, notifyWebhookOnSuccess: event.target.checked })} />Notify on success</label></div><button className="btn primary save-settings" disabled={saving} onClick={() => void save()}>{saving ? "Saving…" : "Save settings"}</button></section>
        </div>
        {supportOpen && <Modal title="Report bugs or errors" onClose={closeSupportReport} wide><div className="support-report-modal">
            <p className="modal-intro">Submit an issue on GitHub to report a Replicaro bug/error. You will need to copy/paste the error log below into the GitHub issue. Replicaro never uploads or submits this log automatically.</p>
            <div className="inline-warning support-report-warning">Computer names and recognizable paths and filenames are removed from this support report. Known and obvious credential forms are also redacted as a precaution. Review the report before posting it on GitHub.</div>
            {supportLoading && !supportReport && <Loading />}
            {supportError && <div className="inline-error" role="alert">Could not generate the error log: {supportError}</div>}
            {supportReport && <textarea className="support-report-preview mono" aria-label="Diagnostic error log preview" readOnly value={supportReport} />}
            {copyStatus && <div className={copyStatus.startsWith("Error log copied") ? "inline-notice" : "inline-error"} role="status">{copyStatus}</div>}
            <div className="modal-footer">
                <button type="button" className="btn primary" disabled={!supportReport || supportLoading || supportCopying} onClick={() => void copySupportReport()}>Copy error log</button>
                <a className="btn" href={supportReport && !supportLoading ? "https://github.com/replicaro/replicaro/issues/new" : undefined} target="_blank" rel="noreferrer" role="link" aria-disabled={!supportReport || supportLoading} tabIndex={supportReport && !supportLoading ? 0 : -1} onClick={() => { if (supportReport && !supportLoading) void copySupportReport(); }}>Open GitHub issue</a>
            </div>
        </div></Modal>}
        {pendingNavigation && <SaveChangesDialog onSave={() => void saveAndLeave()} onDiscard={discardAndLeave} onCancel={() => setPendingNavigation(null)} busy={saving} />}
    </div>;
}
