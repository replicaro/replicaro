import { t } from "../i18n";
import { useCallback, useEffect, useLayoutEffect, useRef, useState } from "react";
import { Link, NavLink, Outlet, useLocation } from "react-router-dom";

import { getAppUpdateStatus, getEngineInfo, getJobs, openAppUpdateDownload, runJob, skipAppUpdateVersion } from "../services/api";
import { BackupRunPicker } from "../components/BackupRunPicker";
import { backupTargetIsActive } from "../services/backupJobs";
import { Icon, Modal, Tooltip, useToast } from "../components/ui";
import type { AppUpdateStatus, BackupJob, EngineInfo } from "../types";

const navItems = [
    { to: "/", label: () => t("ui.navigation.dashboard"), end: true },
    { to: "/protect", label: () => t("ui.navigation.protect") },
    { to: "/restore", label: () => t("ui.navigation.restore"), restore: true },
    { to: "/file-history", label: () => t("ui.navigation.fileHistory") },
];

type RunDialog = "backup" | null;

function displayPath(value: string) {
    if (!/^\/?[A-Za-z]:[\\/]/.test(value)) return value;
    return value.replace(/^\/+/, "").replace(/\//g, "\\");
}

export interface AppUpdateOutletContext {
    presentManualUpdateResult: (status: AppUpdateStatus) => void;
    dismissUpdateVersion: (version: string) => void;
}

export default function MainLayout() {
    const toast = useToast();
    const location = useLocation();
    const [engine, setEngine] = useState<EngineInfo | null>(null);
    const [jobs, setJobs] = useState<BackupJob[]>([]);
    const [runDialog, setRunDialog] = useState<RunDialog>(null);
    const [selectedJobID, setSelectedJobID] = useState("");
    const [runningSelection, setRunningSelection] = useState("");
    const [runPending, setRunPending] = useState(false);
    const [updateStatus, setUpdateStatus] = useState<AppUpdateStatus | null>(null);
    const [dismissedUpdateVersion, setDismissedUpdateVersion] = useState<string | null>(null);
    const [otherModalOpen, setOtherModalOpen] = useState(false);
    const runSession = useRef(0);
    const runGeneration = useRef(0);
    const runOwner = useRef<{ session: number; generation: number } | null>(null);

    useLayoutEffect(() => {
        // Explicit section links keep their page-owned anchor navigation.
        if (!location.hash) window.scrollTo({ top: 0, left: 0, behavior: "instant" });
    }, [location.pathname, location.hash]);

    useEffect(() => {
        let active = true;
        const loadInitial = () => {
            void Promise.all([getEngineInfo(), getJobs()])
            .then(([info, nextJobs]) => {
                if (!active) return;
                setEngine(info);
                setJobs(nextJobs);
            })
            .catch(() => active && setEngine(null));
            void getAppUpdateStatus().then((status) => {
                if (active) setUpdateStatus(status);
            }).catch(() => undefined);
        };
        const refreshJobs = () => {
            if (document.visibilityState === "hidden") return;
            void getJobs().then((nextJobs) => active && setJobs(nextJobs)).catch(() => undefined);
            void getAppUpdateStatus().then((status) => active && setUpdateStatus(status)).catch(() => undefined);
        };
        loadInitial();
        const timer = window.setInterval(refreshJobs, 30000);
        return () => { active = false; window.clearInterval(timer); };
    }, []);

    useEffect(() => () => {
        runSession.current++;
        runGeneration.current++;
        runOwner.current = null;
    }, []);

    const dismissRun = useCallback(() => {
        if (runOwner.current?.session === runSession.current) return false;
        runSession.current++;
        runOwner.current = null;
        setRunDialog(null);
        setSelectedJobID("");
        setRunningSelection("");
        setRunPending(false);
        return true;
    }, []);

    const startBackup = useCallback(async (job: BackupJob, repositoryId?: string) => {
        if (runOwner.current) return;
        const destination = repositoryId
            ? job.targets.find((target) => target.repositoryId === repositoryId)?.repositoryName ?? "vault"
            : job.targets.length === 1 ? job.targets[0].repositoryName : "all destination vaults";
        const owner = { session: runSession.current, generation: ++runGeneration.current };
        runOwner.current = owner;
        const owns = () => runSession.current === owner.session && runGeneration.current === owner.generation && runOwner.current === owner;
        setRunningSelection(repositoryId ?? "all");
        setRunPending(true);
        try {
            await runJob(job.id, repositoryId);
            toast("info", `"${job.name}" started for ${destination}`);
            if (owns()) { setRunDialog(null); setSelectedJobID(""); }
        } catch (error) {
            if (owns()) toast("error", (error as Error).message);
        } finally {
            if (owns()) { runOwner.current = null; setRunningSelection(""); setRunPending(false); }
        }
    }, [toast]);

    const selectedJob = jobs.find((job) => job.id === selectedJobID) ?? null;
    const presentManualUpdateResult = useCallback((status: AppUpdateStatus) => {
        setUpdateStatus(status);
        if (status.result === "update_available" && status.availableVersion) {
            setDismissedUpdateVersion((dismissed) => dismissed === status.availableVersion ? null : dismissed);
        }
    }, []);
    const noticeVersion = updateStatus?.availableVersion && !updateStatus.availableVersionSkipped &&
        dismissedUpdateVersion !== updateStatus.availableVersion ? updateStatus.availableVersion : null;
    const updateDialogTitle = noticeVersion ? t("ui.update.availableTitle", { version: noticeVersion }) : null;

    useEffect(() => {
        const refreshOtherModal = () => {
            const hasOtherModal = Array.from(document.querySelectorAll<HTMLElement>(".modal-overlay [role='dialog']"))
                .some((dialog) => dialog.querySelector(".app-update-copy") === null);
            setOtherModalOpen(hasOtherModal);
        };
        refreshOtherModal();
        const observer = new MutationObserver(refreshOtherModal);
        observer.observe(document.body, { childList: true, subtree: true });
        return () => observer.disconnect();
    }, []);

    const dismissUpdateVersion = useCallback((version: string) => setDismissedUpdateVersion(version), []);

    const dismissUpdate = () => {
        if (noticeVersion) setDismissedUpdateVersion(noticeVersion);
    };
    const openUpdate = async () => {
        if (!noticeVersion) return;
        const displayedVersion = noticeVersion;
        setDismissedUpdateVersion(displayedVersion);
        try {
            await openAppUpdateDownload(displayedVersion);
        } catch (error) {
            toast("error", (error as Error).message);
        }
    };
    const skipUpdate = async () => {
        if (!noticeVersion) return;
        const displayedVersion = noticeVersion;
        try {
            const status = await skipAppUpdateVersion(displayedVersion);
            setUpdateStatus(status);
            if (status.stored) setDismissedUpdateVersion(displayedVersion);
        } catch (error) {
            toast("error", (error as Error).message);
        }
    };
    const openLauncher = () => {
        if (runOwner.current) return;
        runSession.current++;
        setSelectedJobID("");
        setRunningSelection("");
        setRunPending(false);
        setRunDialog("backup");
    };

    return <div className="shell">
        <header className="topbar"><div className="topbar-inner">
            <Link to="/" className="brand" aria-label={t("ui.layouts.mainlayout.replicaro.dashboard")}>
                <img className="brand-wordmark brand-wordmark-dark" src="/replicaro-wordmark-white.png" alt="" />
                <img className="brand-wordmark brand-wordmark-light" src="/replicaro-wordmark-teal.png" alt="" />
            </Link>
            <nav className="topnav" aria-label={t("ui.layouts.mainlayout.primary.navigation")}>
                {navItems.map((item) => <NavLink key={item.to} to={item.to} end={item.end}
                    className={({ isActive }) => isActive || (item.restore && location.pathname.startsWith("/restore")) ? "active" : undefined}>
                    {item.label()}
                </NavLink>)}
            </nav>
            <div className="topbar-actions">
                <Tooltip content={t("ui.navigation.settings")}><NavLink to="/system" className={({ isActive }) => `btn system-link${isActive ? " active" : ""}`}><span className={`engine-dot${engine?.healthy ? " ok" : ""}`} />{t("ui.navigation.system")}</NavLink></Tooltip>
                <button className="btn primary topbar-backup" disabled={runPending} onClick={openLauncher}><Icon name="play" size={14} />{t("ui.backup.runJobsNow")}</button>
            </div>
        </div></header>
        <main className="content"><Outlet context={{ presentManualUpdateResult, dismissUpdateVersion } satisfies AppUpdateOutletContext} /></main>

        {noticeVersion && updateStatus && updateDialogTitle && !otherModalOpen && <Modal title={updateDialogTitle} onClose={dismissUpdate}>
            <div className="app-update-copy">
                <p>{t("ui.update.runningVersionAdvice", { version: updateStatus.runningVersion })}</p>
            </div>
            <div className="app-update-actions">
                <button type="button" className="btn primary" onClick={() => void openUpdate()}>{t("ui.layouts.mainlayout.open.download.page")}</button>
                <button type="button" className="btn" onClick={dismissUpdate}>{t("ui.layouts.mainlayout.not.now")}</button>
                <button type="button" className="btn" onClick={() => void skipUpdate()}>{t("ui.layouts.mainlayout.skip.this.version")}</button>
            </div>
        </Modal>}

        {runDialog === "backup" && <Modal title={selectedJob ? t("ui.backup.runNamedJob", { jobName: selectedJob.name }) : t("ui.backup.runBackupsNow")} onClose={dismissRun}>
            {selectedJob ? <>
                <button type="button" className="btn sm modal-back" disabled={Boolean(runningSelection)} onClick={() => setSelectedJobID("")}>{t("ui.layouts.mainlayout.back.to.jobs")}</button>
                <p className="muted modal-intro">{t("ui.layouts.mainlayout.choose.which.destination.vault.to.back.up.to")}</p>
                <BackupRunPicker job={selectedJob} busy={runningSelection} onRun={(repositoryId) => void startBackup(selectedJob, repositoryId)} />
            </> : <>
                <p className="muted modal-intro">{t("ui.layouts.mainlayout.choose.a.backup.job.to.run")}</p>
                {jobs.length === 0 ? <p className="muted">{t("ui.layouts.mainlayout.create.an.enabled.backup.job.first")}</p> : <div className="job-picker">{jobs.map((job) => {
                    const allActive = job.targets.length > 0 && job.targets.every(backupTargetIsActive);
                    const row = <button type="button" key={job.id} className="job-picker-row" aria-label={!job.enabled ? job.name : undefined} disabled={!job.enabled || Boolean(runningSelection) || allActive} onClick={() => {
                        if (!job.enabled || runOwner.current) return;
                        if (job.targets.length > 1) setSelectedJobID(job.id); else void startBackup(job);
                    }}><span><strong>{job.name}</strong><span className="mono faint">{displayPath(job.source)} · {t("ui.backup.destinationVaultCount", { count: job.targets.length })}</span></span><Icon name="play" size={14} /></button>;
                    return job.enabled ? row : <Tooltip key={job.id} content={t("ui.backup.disabledJobHelp")}><span className="job-picker-disabled-tooltip" tabIndex={0} aria-label={t("ui.backup.disabledNamedJobHelp", { jobName: job.name })}>{row}</span></Tooltip>;
                })}</div>}
            </>}
        </Modal>}
    </div>;
}
