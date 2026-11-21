import { formatDisplayDate, formatDisplayNumber, formatDisplayTime, renderMessage, t } from "../i18n";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { ReactNode } from "react";
import { Link, useSearchParams } from "react-router-dom";

import {
	getDashboard,
	getDashboardIssues,
	getActiveOperations,
	getOperationLive,
	getOperationLockedFiles,
	getOperationLog,
	cancelOperation,
	getDashboardTimeline,
	getLatestVaultOperations,
	getOperation,
	isOperationNotFound,
	requireExactOperation,
	getJobs,
	markDashboardIssuesReviewed,
    getRepositories,
} from "../services/api";
import { EmptyState, Loading, Modal, Tooltip, duration, parseTime, timeAgo, useToast } from "../components/ui";
import type {
	ActivityEntry,
	BackupJob,
	DashboardStats,
	DashboardTimeline,
	DashboardTimelineFilter,
	DashboardTimelineItem,
	OperationEntry,
	OperationLogResponse,
	OperationLiveResponse,
    Repository,
} from "../types";
import { completedWithIssuesTag, dashboardIssueEvent, lockedFilesSkipped, logTag, logTagLabel } from "./dashboardIssue";
import type { TimelineEvent } from "./dashboardIssue";
import { formatReadableLog, noSummaryDiagnosticsLine } from "./nativeLogFormat";
import { NativeLogPager, retainReadableLog } from "./nativeLogPaging";
import { VAULT_PASSWORD_CHANGE_KIND, passwordChangeStillBlocksVault } from "../services/trackedOperations";
import { readableSize } from "../sizeDisplay";
import { ltrIsolate } from "../services/rcloneRemote";

type TimelineFilter = "all" | "backups" | "restores" | "checks" | "maintenance" | "other" | "issues";

const timelineFilters: Array<{ value: TimelineFilter; label: () => string }> = [
    { value: "all", label: () => t("ui.timeline.all") },
    { value: "backups", label: () => t("ui.timeline.backups") },
    { value: "restores", label: () => t("ui.timeline.restores") },
    { value: "checks", label: () => t("ui.timeline.integrityChecks") },
    { value: "maintenance", label: () => t("ui.timeline.spaceReclamation") },
    { value: "other", label: () => t("ui.timeline.other") },
    { value: "issues", label: () => t("ui.timeline.issues") },
];

const TIMELINE_PAGE_SIZE = 10;
const TIMELINE_LOAD_MORE_SIZE = 50;
const MAX_RETAINED_TERMINAL_OPERATIONS = 200;
const CANONICAL_UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

// Issues has a feed of its own, so no timeline feed is loaded while it is
// shown.
const timelineFeedFilter = (filter: TimelineFilter): DashboardTimelineFilter | null => filter === "issues" ? null : filter;

// The loaded part of one timeline tab: the server's batches of up to 200
// entries, newest first, and the cursor of the next older batch.
interface TimelineFeed {
	filter: DashboardTimelineFilter | null;
	items: DashboardTimelineItem[];
	cursor: string;
	hasMore: boolean;
	loading: boolean;
}

const emptyTimelineFeed = (filter: DashboardTimelineFilter | null): TimelineFeed =>
	({ filter, items: [], cursor: "", hasMore: false, loading: filter !== null });

// joinNewestTimelineBatch puts a refreshed newest batch in front of the older
// batches already loaded, joined at the oldest entry of the batch that was
// already loaded, so a refresh keeps what "Load older" brought in. When they
// share nothing (more than a batch of new entries since the last refresh, or
// nothing loaded yet) the tab starts over from the newest batch.
function joinNewestTimelineBatch(feed: TimelineFeed, batch: DashboardTimeline): TimelineFeed {
	const newest = { ...feed, items: batch.items, cursor: batch.nextCursor ?? "", hasMore: batch.hasMore, loading: false };
	if (!batch.hasMore) return newest;
	const loaded = new Map(feed.items.map((item, index) => [item.id, index]));
	for (let index = batch.items.length - 1; index >= 0; index--) {
		const overlap = loaded.get(batch.items[index].id);
		if (overlap === undefined) continue;
		const fresh = new Set(batch.items.map((item) => item.id));
		const older = feed.items.slice(overlap + 1).filter((item) => !fresh.has(item.id));
		return { ...feed, items: [...batch.items, ...older], loading: false };
	}
	return newest;
}

function appendOlderTimelineBatch(feed: TimelineFeed, batch: DashboardTimeline): TimelineFeed {
	const loaded = new Set(feed.items.map((item) => item.id));
	return {
		...feed, items: [...feed.items, ...batch.items.filter((item) => !loaded.has(item.id))],
		cursor: batch.nextCursor ?? "", hasMore: batch.hasMore,
	};
}

const logPresentation = (kind?: string, lockedFiles = false) => ({concise: true, operationKind: kind, maintenanceOnly: kind === "prune" || kind === "maintenance", lockedFilesSkipped: lockedFiles});

// Backup, restore, integrity check, and maintenance results get one fixed line
// of advice under the errors / warnings summary heading, but only when that
// summary actually lists at least one error or warning; a clean result gets
// no advice. Many native errors and warnings for these operations are
// transient (a share that briefly dropped, a file changing mid-read, a lock
// held by another client), so the advice is not guessed from the error text.
// It is not attached to live output or to other kinds such as snapshot
// deletion ("delete"). "prune" counts as maintenance, as in logPresentation.
// It is rendered as markup inside the <pre> (bold italic) and is never part
// of the formatted readable string, the diagnostics, or Raw, so log
// formatting and paging are unaffected.
const retryAdviceOperationKinds = new Set(["backup", "restore", "check", "maintenance", "prune"]);
// Text inside the log output is never translated, so the advice is English in
// every language, like the engine output around it.
const retryAdvice = "Retrying this backup/restore/integrity check/maintenance will often automatically fix errors and warnings.";

function withRetryAdvice(readable: string, kind?: string): ReactNode {
    if (!kind || !retryAdviceOperationKinds.has(kind)) return readable;
    const headingEnd = readable.indexOf("\n");
    const heading = headingEnd < 0 ? readable : readable.slice(0, headingEnd);
    const rest = headingEnd < 0 ? "" : readable.slice(headingEnd);
    const firstSummaryLine = rest.slice(1).split("\n", 1)[0];
    if (headingEnd < 0 || !firstSummaryLine || firstSummaryLine === noSummaryDiagnosticsLine) return readable;
    return <>{heading}{"\n"}<strong><em>{retryAdvice}</em></strong>{rest}</>;
}

// A response that is not a feed batch fails the request like any other error,
// so the page keeps what it has and says so.
function requireTimelineBatch(batch: DashboardTimeline) {
	if (!Array.isArray(batch?.items)) throw new Error(t("ui.app.pageErrorBody"));
	return batch;
}

async function loadDashboardSnapshot(timelineFilter: DashboardTimelineFilter | null, operationID: string, requiredOperationIDs: string[] = []) {
	const exactIDs = [...new Set([...requiredOperationIDs, ...(operationID ? [operationID] : [])])];
	const [stats, jobs, repos, timeline, passwordChanges, exactOperationGroups] = await Promise.all([
		getDashboard(),
		getJobs(),
		getRepositories(),
		timelineFilter === null ? null : getDashboardTimeline(timelineFilter).then(requireTimelineBatch),
		// Each vault's newest password change decides the blocked-vault link on
		// timeline and issue rows. Without it the links are hidden and the rest
		// of the page still loads.
		getLatestVaultOperations(VAULT_PASSWORD_CHANGE_KIND).then(
			(operations) => Array.isArray(operations) ? operations : null,
			() => null,
		),
		Promise.all(exactIDs.map(async (id) => {
			const required = requiredOperationIDs.includes(id);
			let absent = false;
			const matches = await getOperation(id).catch((reason) => {
				if (!required && isOperationNotFound(reason)) {
					absent = true;
					return [] as OperationEntry[];
				}
				throw reason;
			});
			if (absent) return [];
			const exact = requireExactOperation(id, matches);
			if (required && operationIsActive(exact)) {
				throw new Error(t("ui.overview.completedOperationLoadFailed"));
			}
			return [exact];
		})),
	]);
	return { stats, jobs, repos, timelineFilter, timeline, passwordChanges, exactOperations: exactOperationGroups.flat() };
}

async function loadRequiredTerminalOperations(ids: string[]) {
	return Promise.all(ids.map(async (id) => {
		const matches = await getOperation(id);
		const exact = matches.find((operation) => operation.id === id);
		if (!exact || operationIsActive(exact)) throw new Error(t("ui.overview.completedOperationLoadFailed"));
		return exact;
	}));
}

function operationIsActive(operation: OperationEntry) {
	return operation.status === "queued" || operation.status === "running" ||
		(operation.steps ?? []).some((step) => step.status === "running");
}

function operationLogHasMultiplePages(page: OperationLogResponse | null) {
	return Boolean(page?.available && Number.isSafeInteger(page.offset) && Number.isSafeInteger(page.nextOffset) &&
		Number.isSafeInteger(page.size) && typeof page.eof === "boolean" && (page.offset > 0 || !page.eof));
}

const logSizeUnits = ["bytes", "KB", "MB", "GB", "TB", "PB"];

function operationLogPageLabel(page: OperationLogResponse) {
	const unitIndex = page.size > 0
		? Math.min(Math.floor(Math.log(page.size) / Math.log(1000)), logSizeUnits.length - 1)
		: 0;
	const divisor = 1000 ** unitIndex;
	// Keep the log's three significant digits while letting the locale supply
	// separators; a fractional-digit default would hide tiny nonzero offsets.
	const format = (bytes: number) => formatDisplayNumber(bytes / divisor, { maximumSignificantDigits: 3 });
	const unit = logSizeUnits[unitIndex];
	if (unitIndex === 0) {
		return t("ui.overview.logByteRange", { count: page.nextOffset - page.offset, start: page.offset + 1, end: page.nextOffset, size: page.size });
	}
	// The range and the size are each one piece with their unit, isolated left
	// to right on a right-to-left page, so they read "1–256 KB" and "2.4 MB"
	// there too (sizes read number then unit on every page).
	return t("ui.overview.logUnitRange", {
		range: ltrIsolate(`${format(page.offset + 1)}–${format(page.nextOffset)} ${unit}`),
		size: ltrIsolate(`${format(page.size)} ${unit}`),
	});
}

// The "Vaults' Size" and "Protected Data" tiles add up the saved sizes the
// Protect page's vault and job cards show (Repository.vaultSizeBytes,
// BackupJob.sizeBytes) from the lists the dashboard already loads, and format
// the sum the same way (readableSize). They must never start a measurement or
// a request of their own. A vault or job that was never measured has no size
// and is left out, so the sum covers only part of them; the tooltip says how
// many were measured, so a partial sum isn't read as the whole. With nothing
// measured the tile shows a dash. On a right-to-left page the value keeps
// its number before its unit ("1.8 TB", not "TB 1.8").
function SizeTile({ to, sizes, label, measuredText }: { to: string; sizes: Array<number | null | undefined>; label: string; measuredText: (measured: number, count: number) => string }) {
	const measured = sizes.filter((size): size is number => typeof size === "number");
	const value = measured.length ? readableSize(measured.reduce((sum, size) => sum + size, 0)) : "—";
	const link = <Link to={to} aria-label={`${value} ${label}`}><strong><bdi dir="ltr">{value}</bdi></strong><span>{label}</span></Link>;
	return <div className="overview-size-stat">{measured.length < sizes.length
		? <Tooltip content={measuredText(measured.length, sizes.length)}>{link}</Tooltip>
		: link}</div>;
}

// A damaged record can have a time the page can't read, or no title, message
// or log level. The timeline still lists it and shows the missing value as "?",
// with what it stands for in the tooltip. The "?" is an image named by that
// label, so a screen reader reads "Unknown date" or "Unknown" rather than a
// question mark.
function UnknownValue({ label }: { label: string }) {
    return <Tooltip content={label}><span role="img">?</span></Tooltip>;
}

// The server places a dashboard time only when SQLite reads it and it starts
// with a full YYYY-MM-DD date, as every format Replicaro writes does. The page
// uses the same rule for showing "?" and for sorting, because the browser also
// reads values such as "2026", "2026-07" or "12345" as dates, which would give
// such a row a made-up date and move it out of the unknown group.
function dashboardTime(value: string): Date | null {
    return /^\d{4}-\d{2}-\d{2}(?:[T ]|$)/.test(value) ? parseTime(value) : null;
}

function eventTime(value: string): ReactNode {
    const date = dashboardTime(value);
    if (!date) return <UnknownValue label={t("ui.date.unknownDate")} />;

    const today = new Date();
    if (date.toDateString() === today.toDateString()) {
        return formatDisplayTime(date, { hour: "2-digit", minute: "2-digit" });
    }
    return formatDisplayDate(date, { month: "short", day: "numeric" });
}

function operationTag(entry: OperationEntry) {
	if (entry.status === "queued") return t("ui.operation.queued");
	if (entry.status === "running") return t("ui.operation.running");
    if (entry.status === "interrupted") return t("ui.operation.stopped");
    if (entry.status === "failed") return t("ui.operation.failed");
	if (entry.status === "reconnect_required") return t("ui.operation.reconnectRequired");
    if (entry.status === "completed_with_issues") return completedWithIssuesTag(entry.issueKind);
    if (entry.status === "partial") return t("ui.operation.partial");
    if (entry.status === "success" && (entry.kind === "backup" || entry.kind === "restore")) return t("ui.operation.success");
    return operationKindLabel(entry.kind);
}

// Unknown kinds (for example from a newer backend) keep their internal name.
function operationKindLabel(kind: string) {
    switch (kind) {
        case "backup": return t("ui.operation.kind.backup");
        case "restore": return t("ui.operation.kind.restore");
        case "check": return t("ui.operation.kind.check");
        case "maintenance": return t("ui.operation.kind.maintenance");
        case "delete": return t("ui.operation.kind.delete");
        case "delete_job": return t("ui.operation.kind.deleteJob");
        case "vault_password": return t("ui.operation.kind.vaultPassword");
        case "vault_settings": return t("ui.operation.kind.vaultSettings");
        case "remove_vault": return t("ui.operation.kind.removeVault");
        case "download": return t("ui.operation.kind.download");
        default: return kind;
    }
}

// The tone of a finished operation, shared by the dashboard timeline and the
// live log header. A skipped or unfamiliar persisted result is not proof of
// failure, so only known failure outcomes use red and the rest get the
// warning color.
function operationOutcomeTone(status: OperationEntry["status"]): "ok" | "danger" | "warn" {
    return status === "success" ? "ok" : ["failed", "interrupted", "partial", "reconnect_required"].includes(status) ? "danger" : "warn";
}

// The live log header is colored by the operation's status, not by its label:
// a successful check shows its own name, in green. Queued and running work
// has no outcome yet, so it keeps the normal text color.
function operationHeaderTone(status: OperationEntry["status"]) {
    return status === "queued" || status === "running" ? undefined : operationOutcomeTone(status);
}

type OperationStep = NonNullable<OperationEntry["steps"]>[number];

// Step kinds are internal names; show a plain name instead. An unknown kind
// keeps its internal name with spaces for underscores. The names are nouns
// ("Vault storage check", not "Checking the vault's storage") because each one
// sits next to its status, which may already be finished or failed.
function operationStepLabel(kind: string) {
    switch (kind) {
        case "backup": return t("ui.operation.kind.backup");
        case "restore": return t("ui.operation.kind.restore");
        case "repository_integrity_check": return t("ui.operation.kind.check");
        case "maintenance": return t("ui.operation.kind.maintenance");
        case "snapshot_deletion": return t("ui.operation.kind.delete");
        case "download": return t("ui.operation.kind.download");
        case "retention": return t("ui.operation.step.retention");
        case "integrity_cache_prepare": return t("ui.operation.step.prepareIntegrityCheck");
        case "integrity_cache_cleanup": return t("ui.operation.step.cleanUpIntegrityCheck");
        case "kopia_policy_reconciliation": return t("ui.operation.step.kopiaPolicies");
        // The writer validation confirms the vault's owner (and, for Kopia,
        // its maintenance owner) before anything writes; it checks ownership,
        // not write access.
        case "repository_writer_storage_validation":
        case "integrity_owner_admission": return t("ui.operation.step.vaultOwnership");
        case "repository_storage_admission": return t("ui.operation.step.vaultStorage");
        case "integrity_owner_revalidation":
        case "maintenance_owner_revalidation": return t("ui.operation.step.vaultOwnershipRecheck");
        case "integrity_owner_final_admission": return t("ui.operation.step.vaultOwnershipFinal");
        case "native_process_admission": return t("ui.operation.step.engineStartCheck");
        case "output_processing": return t("ui.operation.step.engineOutput");
        case "backup_output_processing": return t("ui.operation.step.backupOutput");
        case "retention_output_processing": return t("ui.operation.step.retentionOutput");
        case "backup_admission": return t("ui.operation.step.backupPreparation");
        case "before_script": return t("ui.operation.step.beforeScript");
        case "after_script": return t("ui.operation.step.afterScript");
        case "metadata_cache":
        case "metadata_cache_invalidation": return t("ui.operation.step.snapshotList");
        case "notification": return t("ui.operation.step.notification");
        case "vault_password_blocked": return t("ui.operation.step.passwordChangeBlocked");
        case "recovery_profile_publication": return t("ui.operation.step.recoveryProfile");
        default: return kind.replaceAll("_", " ");
    }
}

function operationStepStatus(status: OperationStep["status"]): { label: string; tone?: string } {
    switch (status) {
        case "running": return { label: t("ui.operation.running"), tone: "running" };
        case "succeeded": return { label: t("ui.operation.step.status.succeeded"), tone: "ok" };
        case "failed": return { label: t("ui.operation.failed"), tone: "danger" };
        case "interrupted": return { label: t("ui.operation.stopped"), tone: "danger" };
        case "warning": return { label: t("ui.operation.step.status.warning"), tone: "warn" };
        case "skipped": return { label: t("ui.operation.step.status.skipped") };
        default: return { label: status };
    }
}

function operationTitle(entry: OperationEntry) {
    const elapsed = duration(entry.startedAt, entry.finishedAt);
    return `${entry.title}${elapsed !== "—" ? ` · ${elapsed}` : ""}`;
}

// activeOperationSummary is the "kind · engine · progress" line of an active
// operation. A job deletion has no engine of its own (a job can target Restic
// and Kopia vaults at once), so empty parts are left out: it shows
// "Job deletion · Waiting for the vault" rather than
// "Job deletion ·  · Waiting for the vault".
function activeOperationSummary(entry: OperationEntry) {
    return [operationKindLabel(entry.kind), entry.engine, activeOperationProgress(entry)].filter(Boolean).join(" · ");
}

function activeOperationProgress(entry: OperationEntry) {
    if (entry.kind !== "backup") {
        // A restore, manual check or maintenance, snapshot or job deletion, or
        // vault password change, settings save or removal is queued only while
        // it waits for a busy vault (a job deletion or vault removal also waits
        // for the job's or vault's other work first). Backups keep their queued wording: they
        // also wait for a free run slot.
        return entry.status === "queued" ? t("ui.overview.waitingForVault") : t("ui.overview.operationStartedAt", { time: timeAgo(entry.startedAt) });
    }
    const backup = entry.steps?.find((step) =>
        step.domain === "native" && step.kind === "backup" && step.status === "succeeded"
    );
    if (!backup) {
        return entry.status === "queued" ? t("ui.overview.operationQueuedAt", { time: timeAgo(entry.startedAt) }) : t("ui.overview.operationStartedAt", { time: timeAgo(entry.startedAt) });
    }
    // Restic retention is a native child; Kopia retention remains part of its backup child.
    const retentionRunning = entry.steps?.some((step) =>
        step.domain === "native" && step.kind === "retention" && step.status === "running"
    );
    return retentionRunning ? t("ui.overview.snapshotFinishedRetentionRunning", { time: timeAgo(backup.finishedAt) }) : t("ui.overview.snapshotFinishedFinalizing", { time: timeAgo(backup.finishedAt) });
}

function backupCompletedWithIssues(operation?: OperationEntry, event?: TimelineEvent) {
    return (operation?.kind ?? event?.operationKind) === "backup" &&
        (operation?.status ?? event?.operationStatus) === "completed_with_issues";
}

function backupLockedFilesSkipped(operation?: OperationEntry, event?: TimelineEvent) {
    return backupCompletedWithIssues(operation, event) &&
        lockedFilesSkipped("completed_with_issues", operation ? operation.issueKind : event?.operationIssueKind);
}

// The files a "locked files skipped" backup left out, listed in its details.
// They are fetched each time the details are shown rather than kept with the
// run, since the backend reads them from the saved log. The paths are the
// engine's own and are shown as given. While loading, or when there are none,
// nothing is shown; a failed read gets one quiet line.
function SkippedLockedFiles({ operationID }: { operationID: string }) {
    const [result, setResult] = useState<{ operationID: string; files?: string[]; failed?: boolean } | null>(null);
    useEffect(() => {
        const controller = new AbortController();
        getOperationLockedFiles(operationID, controller.signal)
            .then((response) => setResult({ operationID, files: Array.isArray(response?.files) ? response.files : [] }))
            .catch((reason) => {
                if (!(reason instanceof DOMException && reason.name === "AbortError")) setResult({ operationID, failed: true });
            });
        return () => controller.abort();
    }, [operationID]);
    if (result?.operationID !== operationID) return null;
    if (result.failed) return <p className="muted">{t("ui.overview.skippedLockedFilesLoadFailed")}</p>;
    if (!result.files?.length) return null;
    return (
        <div className="skipped-locked-files">
            <div className="modal-section-label">{t("ui.overview.skippedLockedFiles")}</div>
            <ul aria-label={t("ui.overview.skippedLockedFiles")}>
                {result.files.map((file, index) => <li key={`${index}:${file}`}><bdi dir="ltr" translate="no">{file}</bdi></li>)}
            </ul>
        </div>
    );
}

export default function Overview() {
    const [stats, setStats] = useState<DashboardStats | null>(null);
    const [jobs, setJobs] = useState<BackupJob[]>([]);
    const [repos, setRepos] = useState<Repository[]>([]);
    const [timelineFeed, setTimelineFeed] = useState<TimelineFeed>(() => emptyTimelineFeed("all"));
    const [timelineOlderLoading, setTimelineOlderLoading] = useState(false);
    const [timelineReloadKey, setTimelineReloadKey] = useState(0);
    // Operations fetched by ID: runs that finished while the page was open and
    // the run a notification link names.
    const [pinnedOperations, setPinnedOperations] = useState<OperationEntry[]>([]);
    // Each vault's newest password change, of any status; null when unknown.
    const [passwordChanges, setPasswordChanges] = useState<OperationEntry[] | null>(null);
    const [activeOperations, setActiveOperations] = useState<OperationEntry[]>([]);
    const [activeOperationsLoaded, setActiveOperationsLoaded] = useState(false);
	const [selectedActiveOperation, setSelectedActiveOperation] = useState<OperationEntry | null>(null);
	const [operationDetail, setOperationDetail] = useState<OperationLiveResponse | null>(null);
	const [confirmCancel, setConfirmCancel] = useState(false);
	const [cancelAccepted, setCancelAccepted] = useState(false);
	const [cancelError, setCancelError] = useState("");
	const [cancelSubmitting, setCancelSubmitting] = useState(false);
	const [cancelNeedsFreshSnapshot, setCancelNeedsFreshSnapshot] = useState(false);
    const [issueEvents, setIssueEvents] = useState<TimelineEvent[]>([]);
    const [issuesHasMore, setIssuesHasMore] = useState(false);
    const [issuesCursor, setIssuesCursor] = useState("");
    const [issuesLoading, setIssuesLoading] = useState(false);
    const [issueRequestKey, setIssueRequestKey] = useState(0);
    const [error, setError] = useState("");
    const [filter, setFilter] = useState<TimelineFilter>("all");
    const [openOutput, setOpenOutput] = useState<string | null>(null);
	const [openOutputText, setOpenOutputText] = useState("");
    const [openOutputLoading, setOpenOutputLoading] = useState(false);
    const [showRawLog, setShowRawLog] = useState(false);
	const [openOutputPage, setOpenOutputPage] = useState<OperationLogResponse | null>(null);
	const [openOutputEngine, setOpenOutputEngine] = useState<OperationEntry["engine"]>();
	const [openOutputLoadFailed, setOpenOutputLoadFailed] = useState(false);
	const [completedOperationLog, setCompletedOperationLog] = useState("");
	const [completedOperationLogPage, setCompletedOperationLogPage] = useState<OperationLogResponse | null>(null);
	const [completedOperationLogLoadState, setCompletedOperationLogLoadState] = useState<"pending" | "loaded" | "failed">("pending");
    const [visible, setVisible] = useState(TIMELINE_PAGE_SIZE);
    const [reviewingIssues, setReviewingIssues] = useState(false);
    const [searchParams, setSearchParams] = useSearchParams();
	const [focusedOperationID, setFocusedOperationID] = useState("");
	const focusedOperationRef = useRef("");
	const activeDetailGeneration = useRef(0);
	const activeDetailRequestSequence = useRef(0);
	const cancelFreshAfterRequest = useRef(0);
    const issueReviewGeneration = useRef(0);
    const issueFeedGeneration = useRef(0);
    const issueResponseGeneration = useRef(0);
	const timelineOutputRequestSequence = useRef(0);
	const completedLogRequestSequence = useRef(0);
    const timelineLogPager = useRef(new NativeLogPager());
    const completedLogPager = useRef(new NativeLogPager());
	const dashboardRequestSequence = useRef(0);
	const timelineFilterRef = useRef<DashboardTimelineFilter | null>("all");
	const timelineGeneration = useRef(0);
	const timelineOlderRequestSequence = useRef(0);
	const priorActiveOperationIDs = useRef<Set<string> | null>(null);
	const pendingActiveCommit = useRef<OperationEntry[] | null>(null);
	const terminalRefreshInFlight = useRef(false);
	const terminalRefreshOwner = useRef(0);
	const pendingTerminalIDs = useRef(new Set<string>());
	const retainedTerminalOperations = useRef(new Map<string, OperationEntry>());
	const querySelectedOperationID = useRef("");
    const timelineOperationID = searchParams.get("timelineOperation") ?? "";
	const operationTargetID = searchParams.get("operation") ?? "";
	const validOperationTargetID = CANONICAL_UUID_V4.test(operationTargetID) ? operationTargetID : "";
	const dashboardOperationID = validOperationTargetID || timelineOperationID;
	const dashboardOperationIDRef = useRef(dashboardOperationID);
    const toast = useToast();

	// Runs fetched by ID are shown even when they are not in the loaded batches;
	// the tab filter below still decides where they appear.
	const operations = useMemo(() => {
		const operationByID = new Map<string, OperationEntry>();
		for (const item of timelineFeed.items) {
			if (item.kind === "operation") operationByID.set(item.operation.id, item.operation);
		}
		for (const operation of pinnedOperations) {
			if (!operationByID.has(operation.id)) operationByID.set(operation.id, operation);
		}
		return [...operationByID.values()];
	}, [pinnedOperations, timelineFeed.items]);
	const logs = useMemo<ActivityEntry[]>(
		() => timelineFeed.items.flatMap((item) => item.kind === "log" ? [item.log] : []),
		[timelineFeed.items],
	);

    useEffect(() => () => {
        completedLogRequestSequence.current++;
        timelineOutputRequestSequence.current++;
        completedLogPager.current.clear();
        timelineLogPager.current.clear();
    }, []);

	useEffect(() => {
		dashboardOperationIDRef.current = dashboardOperationID;
	}, [dashboardOperationID]);

	const resetOperationDetail = useCallback(() => {
        completedLogPager.current.clear();
		completedLogRequestSequence.current++;
		setSelectedActiveOperation(null);
		setOperationDetail(null);
		setCompletedOperationLog("");
		setCompletedOperationLogPage(null);
		setCompletedOperationLogLoadState("pending");
		setError("");
		setConfirmCancel(false);
		setCancelError("");
		setCancelNeedsFreshSnapshot(false);
	}, []);

	const openOperationDetail = useCallback((operation: OperationEntry) => {
        completedLogPager.current.clear();
		completedLogRequestSequence.current++;
		setSelectedActiveOperation(operation);
		setOperationDetail(null);
		setCompletedOperationLog("");
		setCompletedOperationLogPage(null);
		setCompletedOperationLogLoadState("pending");
		setError("");
		setConfirmCancel(false);
		setCancelAccepted(false);
		setCancelError("");
		cancelFreshAfterRequest.current = activeDetailRequestSequence.current;
		setCancelNeedsFreshSnapshot(false);
	}, []);

	const commitDashboardSnapshot = useCallback((snapshot: Awaited<ReturnType<typeof loadDashboardSnapshot>>, timelineRequestGeneration: number) => {
		setStats(snapshot.stats);
		setJobs(snapshot.jobs);
		setRepos(snapshot.repos);
		// Terminal rows that this run's disappearance tracking fetched by ID are
		// kept, as they may fall inside older batches loaded before they finished.
		const operationByID = new Map(retainedTerminalOperations.current);
		snapshot.exactOperations.forEach((operation) => operationByID.set(operation.id, operation));
		setPinnedOperations([...operationByID.values()]);
		setPasswordChanges(snapshot.passwordChanges);
		// A batch requested for a tab that has since been left is not used, nor
		// one requested for a tab that a refresh captured before a switch.
		const timeline = snapshot.timeline;
		if (timeline && timelineRequestGeneration === timelineGeneration.current && snapshot.timelineFilter === timelineFilterRef.current) {
			setTimelineFeed((current) => joinNewestTimelineBatch(current, timeline));
		}
		const pendingActive = pendingActiveCommit.current;
		if (pendingActive) {
			setActiveOperations(pendingActive);
			setActiveOperationsLoaded(true);
			priorActiveOperationIDs.current = new Set(pendingActive.map((operation) => operation.id));
			pendingActiveCommit.current = null;
		}
		setError("");
	}, []);

	// A failed request stops the spinner of a tab still waiting for its first
	// batch; the error banner says why.
	const stopTimelineLoading = useCallback((timelineRequestGeneration: number) => {
		if (timelineRequestGeneration !== timelineGeneration.current) return;
		setTimelineFeed((current) => current.loading ? { ...current, loading: false } : current);
	}, []);

	// A tab with a different feed starts again from its newest batch, and
	// responses still on their way for the previous one are ignored.
	const switchTimelineFeed = useCallback((nextFilter: TimelineFilter) => {
		timelineOlderRequestSequence.current++;
		setTimelineOlderLoading(false);
		const feedFilter = timelineFeedFilter(nextFilter);
		if (feedFilter === timelineFilterRef.current) return;
		timelineFilterRef.current = feedFilter;
		timelineGeneration.current++;
		setTimelineFeed(emptyTimelineFeed(feedFilter));
	}, []);

    useEffect(() => {
        let active = true;
        const timelineFilter = timelineFeedFilter(filter);
        const load = () => {
			if (terminalRefreshInFlight.current) return;
			const dashboardGeneration = ++dashboardRequestSequence.current;
			const reviewGeneration = issueReviewGeneration.current;
            const feedGeneration = issueFeedGeneration.current;
			const timelineRequestGeneration = timelineGeneration.current;
            if (filter === "issues") {
                const responseGeneration = ++issueResponseGeneration.current;
                getDashboardIssues()
                    .then((response) => {
                        if (!active ||
                            reviewGeneration !== issueReviewGeneration.current ||
                            feedGeneration !== issueFeedGeneration.current ||
                            responseGeneration !== issueResponseGeneration.current) return;
                        setIssueEvents(response.items.map(dashboardIssueEvent));
                        setIssuesHasMore(response.hasMore);
                        setIssuesCursor(response.nextCursor ?? "");
                        setIssuesLoading(false);
                    })
                    .catch((reason: Error) => {
                        if (active &&
                            reviewGeneration === issueReviewGeneration.current &&
                            feedGeneration === issueFeedGeneration.current &&
                            responseGeneration === issueResponseGeneration.current) {
                            setIssuesLoading(false);
                            setError(reason.message);
                        }
                    });
            }
            loadDashboardSnapshot(timelineFilter, dashboardOperationID)
                .then((snapshot) => {
                    if (!active ||
						dashboardGeneration !== dashboardRequestSequence.current ||
                        reviewGeneration !== issueReviewGeneration.current) return;
					commitDashboardSnapshot(snapshot, timelineRequestGeneration);
                })
                .catch((reason: Error) => {
                    if (active &&
						dashboardGeneration === dashboardRequestSequence.current &&
						reviewGeneration === issueReviewGeneration.current) {
						const pendingActive = pendingActiveCommit.current;
						if (pendingActive) {
							setActiveOperations(pendingActive);
							setActiveOperationsLoaded(true);
							priorActiveOperationIDs.current = new Set(pendingActive.map((operation) => operation.id));
							pendingActiveCommit.current = null;
						}
						stopTimelineLoading(timelineRequestGeneration);
						setError(reason.message);
					}
                });
        };

        load();
        const timer = window.setInterval(load, 15000);
        return () => {
            active = false;
            window.clearInterval(timer);
        };
    }, [commitDashboardSnapshot, dashboardOperationID, filter, issueRequestKey, stopTimelineLoading, timelineReloadKey]);

    useEffect(() => {
		let active = true;
		let requestGeneration = 0;
		const refreshTerminal = async () => {
			if (terminalRefreshInFlight.current) return;
			terminalRefreshInFlight.current = true;
			const refreshOwner = ++terminalRefreshOwner.current;
			dashboardRequestSequence.current++;
			const refreshIDs = new Set<string>();
			const timelineRequestGeneration = timelineGeneration.current;
			const timelineFilter = timelineFilterRef.current;
			try {
				const requiredIDs = [...pendingTerminalIDs.current];
				pendingTerminalIDs.current.clear();
				requiredIDs.forEach((id) => refreshIDs.add(id));
				let snapshot = await loadDashboardSnapshot(timelineFilter, dashboardOperationIDRef.current, requiredIDs);
				if (!active) return;

				// A later disappearance means the first full snapshot no longer lines up
				// with the newest jobs/logs/dashboard data. Coalesce all such IDs into
				// one full rerun; later arrivals only need their own rows.
				if (pendingTerminalIDs.current.size > 0) {
					const additionalIDs = [...pendingTerminalIDs.current];
					pendingTerminalIDs.current.clear();
					additionalIDs.forEach((id) => refreshIDs.add(id));
					snapshot = await loadDashboardSnapshot(timelineFilter, dashboardOperationIDRef.current, [...refreshIDs]);
					if (!active) return;
				}
				const operationByID = new Map([
					...(snapshot.timeline?.items ?? []).flatMap((item) => item.kind === "operation" ? [item.operation] : []),
					...snapshot.exactOperations,
				].map((operation) => [operation.id, operation]));
				// Active polls can discover more completions while an exact batch is
				// loading. Drain until a synchronous empty check so no UUID is stranded
				// after its prior-active membership has already advanced.
				while (pendingTerminalIDs.current.size > 0) {
					const finalExactIDs = [...pendingTerminalIDs.current];
					pendingTerminalIDs.current.clear();
					finalExactIDs.forEach((id) => refreshIDs.add(id));
					const additional = await loadRequiredTerminalOperations(finalExactIDs);
					additional.forEach((operation) => operationByID.set(operation.id, operation));
					if (!active) return;
				}
				if (!active) return;
				for (const id of refreshIDs) {
					const operation = operationByID.get(id);
					if (operation && !operationIsActive(operation)) retainedTerminalOperations.current.set(id, operation);
				}
				while (retainedTerminalOperations.current.size > MAX_RETAINED_TERMINAL_OPERATIONS) {
					const oldestID = retainedTerminalOperations.current.keys().next().value as string | undefined;
					if (!oldestID) break;
					retainedTerminalOperations.current.delete(oldestID);
				}
				// Every refreshed ID that ended up here is terminal and now retained, so
				// the commit shows it whatever batch it belongs to.
				commitDashboardSnapshot(snapshot, timelineRequestGeneration);
			} catch (reason) {
				if (!active) return;
				const pendingActive = pendingActiveCommit.current;
				if (pendingActive) {
					setActiveOperations(pendingActive);
					setActiveOperationsLoaded(true);
					pendingActiveCommit.current = null;
				}
				// Keep the failed IDs in the transition detector so the next active
				// poll can retry them without showing them as still active.
				const retryIDs = new Set((pendingActive ?? []).map((operation) => operation.id));
				refreshIDs.forEach((id) => retryIDs.add(id));
				pendingTerminalIDs.current.forEach((id) => retryIDs.add(id));
				priorActiveOperationIDs.current = retryIDs;
				stopTimelineLoading(timelineRequestGeneration);
				setError((reason as Error).message);
				pendingTerminalIDs.current.clear();
			} finally {
				if (terminalRefreshOwner.current === refreshOwner) {
					terminalRefreshInFlight.current = false;
					// A tab chosen while this refresh ran skipped its own first load.
					if (active && timelineRequestGeneration !== timelineGeneration.current) setTimelineReloadKey((key) => key + 1);
				}
			}
		};
        const loadActive = () => {
			const request = ++requestGeneration;
            getActiveOperations()
				.then((nextOperations) => {
					if (!active || request !== requestGeneration) return;
					const authoritative = nextOperations.filter(operationIsActive);
					const nextIDs = new Set(authoritative.map((operation) => operation.id));
					const previousIDs = priorActiveOperationIDs.current;
					const disappeared = previousIDs === null ? [] : [...previousIDs].filter((id) => !nextIDs.has(id));
					priorActiveOperationIDs.current = nextIDs;
					if (disappeared.length === 0) {
						if (terminalRefreshInFlight.current) {
							pendingActiveCommit.current = authoritative;
							return;
						}
						setActiveOperations(authoritative);
						setActiveOperationsLoaded(true);
						return;
					}

					// The exact prior UUID set covers every operation kind. Keep vanished
					// cards and their terminal timeline rows in one coordinated commit;
					// replacing this with a latest-backup inference reintroduces the race.
					pendingActiveCommit.current = authoritative;
					disappeared.forEach((id) => pendingTerminalIDs.current.add(id));
					void refreshTerminal();
                })
                .catch(() => {
					if (active && request === requestGeneration) setActiveOperationsLoaded(true);
				});
        };
        loadActive();
        const timer = window.setInterval(loadActive, 4000);
		return () => {
			active = false;
			requestGeneration++;
            window.clearInterval(timer);
		};
	}, [commitDashboardSnapshot, stopTimelineLoading]);

	useEffect(() => {
		if (!validOperationTargetID) {
			if (querySelectedOperationID.current) {
				querySelectedOperationID.current = "";
				resetOperationDetail();
			}
			return;
		}
		if (querySelectedOperationID.current && querySelectedOperationID.current !== validOperationTargetID) {
			querySelectedOperationID.current = "";
			resetOperationDetail();
		}
		if (querySelectedOperationID.current === validOperationTargetID) return;
		// Dashboard refreshes retry the exact query identity. Never substitute a
		// recent or similarly titled operation when its row is temporarily absent.
		const exact = operations.find((operation) => operation.id === validOperationTargetID);
		if (!exact) return;
		let active = true;
		Promise.resolve().then(() => {
			if (!active || querySelectedOperationID.current === validOperationTargetID) return;
			querySelectedOperationID.current = validOperationTargetID;
			openOperationDetail(exact);
		});
		return () => { active = false; };
	}, [openOperationDetail, operations, resetOperationDetail, validOperationTargetID]);

	useEffect(() => {
		if (!selectedActiveOperation) {
			return;
		}
		let active = true;
		let timer = 0;
		let controller: AbortController | null = null;
		const generation = ++activeDetailGeneration.current;
		const load = async () => {
			const requestSequence = ++activeDetailRequestSequence.current;
			controller = new AbortController();
			try {
				const response = await getOperationLive(selectedActiveOperation.id, controller.signal);
				if (!active || generation !== activeDetailGeneration.current) return;
				setOperationDetail(response);
				// A failed POST invalidates the displayed Cancel action. Only a later
				// authoritative snapshot that explicitly reopens the gate may restore it.
				if (response.live.cancelable && requestSequence > cancelFreshAfterRequest.current) {
					setCancelNeedsFreshSnapshot(false);
				}
				if (!operationIsActive(response.operation)) {
					try {
						const completed = await completedLogPager.current.read(
                            offset => getOperationLog(response.operation.id, controller!.signal, offset),
                            () => active && generation === activeDetailGeneration.current, response.operation.engine, backupCompletedWithIssues(response.operation), logPresentation(response.operation.kind, backupLockedFilesSkipped(response.operation)));
						if (active && generation === activeDetailGeneration.current) {
							setCompletedOperationLog(completed.available ? completed.output : "");
							setCompletedOperationLogPage(completed);
							setCompletedOperationLogLoadState("loaded");
							setError("");
						}
					} catch (reason) {
						if (active && generation === activeDetailGeneration.current &&
							!(reason instanceof DOMException && reason.name === "AbortError")) {
							setCompletedOperationLog("");
							setCompletedOperationLogPage(null);
							// A failed read is different from a successfully loaded empty file.
							// Keep that distinction local to the log surface so Overview does
							// not report absent output when output may still exist on disk.
							setCompletedOperationLogLoadState("failed");
						}
					}
					return;
				}
				timer = window.setTimeout(() => void load(), 2000);
			} catch (reason) {
				if (!active || generation !== activeDetailGeneration.current ||
					(reason instanceof DOMException && reason.name === "AbortError")) return;
				timer = window.setTimeout(() => void load(), 2000);
			}
		};
		void load();
		return () => {
			active = false;
			controller?.abort();
			window.clearTimeout(timer);
		};
	}, [selectedActiveOperation]);

    const readableLog = (text: string, operation?: OperationEntry, event?: TimelineEvent,
        page?: OperationLogResponse | null, live = false) => {
        return formatReadableLog(text, {
            operationStatus: operation?.status ?? event?.operationStatus,
            operationIssueKind: operation ? operation.issueKind : event?.operationIssueKind,
            operationKind: operation?.kind ?? event?.operationKind,
            engine: operation?.engine ?? event?.engine ?? openOutputEngine,
        }, live, page?.readableBody, page?.readableDiagnostics);
    };

    const completedReadableLog = (text: string, operation?: OperationEntry, event?: TimelineEvent,
        page?: OperationLogResponse | null) =>
        withRetryAdvice(readableLog(text, operation, event, page), operation?.kind ?? event?.operationKind);

    const timeline = useMemo<TimelineEvent[]>(() => {
        const repoById = new Map(repos.map((repo) => [repo.id, repo]));
        const jobById = new Map(jobs.map((job) => [job.id, job]));

        const operationEvents = operations
            .filter((operation) => operation.status !== "queued" && operation.status !== "running")
            .map((operation): TimelineEvent => {
            const job = jobById.get(operation.jobId);
            const target = job?.targets.find((item) => item.repositoryId === operation.repositoryId);
            const repo = repoById.get(operation.repositoryId || target?.repositoryId || "");
            const title = operation.repositoryId && repo && !operation.title.includes(repo.name)
                ? `${operation.title} → ${repo.name}`
                : operation.title;
            return {
                id: `operation-${operation.id}`,
                timestamp: operation.startedAt,
                kind: "operation",
                operationKind: operation.kind,
                operationID: operation.id,
                operationStatus: operation.status,
                operationIssueKind: operation.issueKind,
				engine: operation.engine,
                tone: operationOutcomeTone(operation.status),
                tag: operationTag(operation),
                // An empty title is shown as unknown, without the vault and
                // duration that would otherwise follow it.
                title: operation.title.trim() ? operationTitle({ ...operation, title }) : "",
                outputAvailable: true,
                issue: operation.status === "failed" || operation.status === "interrupted" || operation.status === "partial" || operation.status === "completed_with_issues" || operation.status === "reconnect_required",
            };
        });

        const logEvents = logs.map((entry): TimelineEvent => {
            const issue = entry.level === "ERROR" || entry.level === "WARN";
            const tag = logTag(entry);
            return {
                id: `log-${entry.id}`,
                timestamp: entry.timestamp,
                kind: "log",
                tone: entry.level === "ERROR" ? "danger" : entry.level === "WARN" ? "warn" : "faint",
                // An entry without a level has no level tag to fall back on and
                // shows it as unknown; a tag its message gives still applies.
                tag: !entry.level && tag === "info" ? "" : logTagLabel(tag),
                title: entry.message,
                outputAvailable: false,
                issue,
            };
        });

        // Newest first, with unknown times at the top as the server lists them.
        // The sort is stable, so entries with equal times keep the order they
        // were loaded in, operations before log entries.
        const time = (event: TimelineEvent) => dashboardTime(event.timestamp)?.getTime() ?? Infinity;
        return [...operationEvents, ...logEvents].sort((a, b) => {
            const aTime = time(a);
            const bTime = time(b);
            return aTime === bTime ? 0 : bTime > aTime ? 1 : -1;
        });
    }, [jobs, logs, operations, repos]);

    useEffect(() => {
        const operationID = searchParams.get("timelineOperation") ?? "";
        if (!operationID || focusedOperationRef.current === operationID) return;
        const index = timeline.findIndex((event) => event.operationID === operationID);
        if (index < 0) return;
        // The run is shown on the All tab, so its place has to come from that
        // tab's entries; another tab first switches over and waits for them.
        if (timelineFeed.filter !== "all") {
            const timer = window.setTimeout(() => {
                switchTimelineFeed("all");
                setFilter("all");
            }, 0);
            return () => window.clearTimeout(timer);
        }
        if (timelineFeed.loading) return;

        focusedOperationRef.current = operationID;
        const timer = window.setTimeout(() => {
            setFilter("all");
            setVisible(Math.max(TIMELINE_PAGE_SIZE, index + 1));
            setFocusedOperationID(operationID);
			const nextParams = new URLSearchParams(searchParams);
			nextParams.delete("timelineOperation");
			setSearchParams(nextParams, { replace: true });
            window.requestAnimationFrame(() => {
                window.requestAnimationFrame(() => {
                    document.getElementById(`operation-${operationID}`)?.scrollIntoView({
                        behavior: "smooth",
                        block: "center",
                    });
                });
            });
        }, 0);
        const clearFocusTimer = window.setTimeout(() => setFocusedOperationID(""), 4200);
        return () => {
            window.clearTimeout(timer);
            window.clearTimeout(clearFocusTimer);
        };
    }, [searchParams, setSearchParams, switchTimelineFeed, timeline, timelineFeed.filter, timelineFeed.loading]);

    const filtered = filter === "issues" ? issueEvents : timeline.filter((event) => {
        if (filter === "backups") return event.operationKind === "backup";
        if (filter === "restores") return event.operationKind === "restore";
        if (filter === "checks") return event.operationKind === "check";
        if (filter === "maintenance") return event.operationKind === "maintenance";
        if (filter === "other") return event.kind === "log" || !["backup", "restore", "check", "maintenance"].includes(event.operationKind ?? "");
        return true;
    });

    if ((!stats || !activeOperationsLoaded) && !error) {
        return <div className="page"><Loading /></div>;
    }

    const enabledJobs = jobs.filter((job) => job.enabled);
    const enabledTargets = enabledJobs.flatMap((job) => job.targets);
    const healthyJobs = enabledJobs.filter((job) => job.targets.length > 0 && job.targets.every((target) => target.lastStatus === "success"));
    const percent = enabledJobs.length
        ? Math.round((healthyJobs.length / enabledJobs.length) * 100)
        : 0;
    // This is intentionally a system-wide issue signal, not a failed-target
    // count. Restore/check/maintenance failures and warning/error logs should
    // override the target-health percentage and surface “Issues found.”
    const issueCount = stats?.failedRuns ?? 0;
    const hasIssues = issueCount > 0;
	const activeRepositoryByID = new Map(repos.map((repo) => [repo.id, repo]));
	// Anything under "Active now" counts as running, whatever its kind; the
	// tray icon uses the same rule.
	const jobsRunning = activeOperations.length > 0;
	// The two getting-started guides give way only to a running backup. A vault
	// check, maintenance or restore can run before the first job exists, and the
	// guide (with its button to create that job) should stay up while it does;
	// the operation is still listed under "Active now". Outside the
	// getting-started guides, the pacman, its ring and the count line follow
	// every active operation through jobsRunning.
	const backupRunning = activeOperations.some((operation) => operation.kind === "backup");
	const gettingStarted = stats !== null && !error && !hasIssues && !backupRunning && repos.length === 0 && jobs.length === 0;
	const needsFirstJob = stats !== null && !error && !hasIssues && !backupRunning && repos.length > 0 && jobs.length === 0;
    const issueTone = issueCount > (stats?.warningRuns ?? 0) ? "danger" : "warn";
    const ringTone = hasIssues ? issueTone : percent === 100 ? "ok" : percent === 0 ? "danger" : "warn";
    const failing = enabledTargets.filter((target) => target.lastStatus === "failed" || target.lastStatus === "interrupted" || target.lastStatus === "reconnect_required");
    const loadOlderBusy = filter === "issues" ? issuesLoading : timelineOlderLoading;
    const selectFilter = (nextFilter: TimelineFilter) => {
        timelineLogPager.current.clear();
        issueFeedGeneration.current++;
        issueResponseGeneration.current++;
        timelineOutputRequestSequence.current++;
		setOpenOutput(null);
        setOpenOutputLoading(false);
		setOpenOutputText("");
		setOpenOutputPage(null);
		setOpenOutputEngine(undefined);
		setOpenOutputLoadFailed(false);
		setError("");
        if (nextFilter === "issues") {
            setIssueEvents([]);
            setIssuesHasMore(false);
            setIssuesCursor("");
            setIssuesLoading(true);
            setIssueRequestKey((key) => key + 1);
        }
        switchTimelineFeed(nextFilter);
        setFilter(nextFilter);
        setVisible(TIMELINE_PAGE_SIZE);
    };
    const showIssues = () => {
        selectFilter("issues");
        window.requestAnimationFrame(() => {
            document.getElementById("timeline")?.scrollIntoView({ behavior: "smooth", block: "start" });
        });
    };
    const loadOlder = () => {
        if (filter !== "issues") {
            if (visible < filtered.length) {
                setVisible((count) => count + TIMELINE_LOAD_MORE_SIZE);
                return;
            }
            // Everything loaded is shown: fetch the next older batch and show the
            // first entries of it the same way.
            const { filter: feedFilter, cursor } = timelineFeed;
            if (timelineOlderLoading || timelineFeed.loading || !timelineFeed.hasMore || feedFilter === null) return;
            const timelineRequestGeneration = timelineGeneration.current;
            const requestSequence = ++timelineOlderRequestSequence.current;
            const shown = filtered.length;
            setTimelineOlderLoading(true);
            getDashboardTimeline(feedFilter, cursor)
                .then(requireTimelineBatch)
                .then((batch) => {
                    if (requestSequence !== timelineOlderRequestSequence.current ||
                        timelineRequestGeneration !== timelineGeneration.current) return;
                    // A refresh that started the tab over has moved the cursor on;
                    // this batch would then leave a gap.
                    setTimelineFeed((current) => current.cursor === cursor ? appendOlderTimelineBatch(current, batch) : current);
                    setVisible((count) => Math.min(count, shown) + TIMELINE_LOAD_MORE_SIZE);
                })
                .catch((reason: Error) => {
                    if (requestSequence === timelineOlderRequestSequence.current &&
                        timelineRequestGeneration === timelineGeneration.current) setError(reason.message);
                })
                .finally(() => {
                    if (requestSequence === timelineOlderRequestSequence.current) setTimelineOlderLoading(false);
                });
            return;
        }
		if (issuesLoading || !issuesHasMore) return;

        setIssuesLoading(true);
		const reviewGeneration = issueReviewGeneration.current;
        const feedGeneration = issueFeedGeneration.current;
        const responseGeneration = ++issueResponseGeneration.current;
        getDashboardIssues(TIMELINE_LOAD_MORE_SIZE, issuesCursor)
            .then((response) => {
				if (reviewGeneration !== issueReviewGeneration.current ||
                    feedGeneration !== issueFeedGeneration.current ||
                    responseGeneration !== issueResponseGeneration.current) return;
                setIssueEvents((current) => [...current, ...response.items.map(dashboardIssueEvent)]);
                setIssuesHasMore(response.hasMore);
                setIssuesCursor(response.nextCursor ?? "");
            })
            .catch((reason: Error) => {
				if (reviewGeneration === issueReviewGeneration.current &&
                    feedGeneration === issueFeedGeneration.current &&
                    responseGeneration === issueResponseGeneration.current) setError(reason.message);
			})
            .finally(() => {
				if (reviewGeneration === issueReviewGeneration.current &&
                    feedGeneration === issueFeedGeneration.current &&
                    responseGeneration === issueResponseGeneration.current) setIssuesLoading(false);
			});
    };
    const toggleTimelineOutput = async (event: TimelineEvent) => {
        timelineLogPager.current.clear();
        const requestSequence = ++timelineOutputRequestSequence.current;
        if (openOutput === event.id) {
            setOpenOutput(null);
            setOpenOutputLoading(false);
			setOpenOutputText("");
			setOpenOutputPage(null);
			setOpenOutputEngine(undefined);
			setOpenOutputLoadFailed(false);
			setError("");
            return;
        }
		setOpenOutput(event.id);
        setOpenOutputLoading(!!event.operationID);
		setOpenOutputText("");
		setOpenOutputPage(null);
		setOpenOutputEngine(event.engine);
		setOpenOutputLoadFailed(false);
		setError("");
		if (event.operationID) {
			try {
				const operationEngine = event.engine
					? Promise.resolve(event.engine)
					: getOperation(event.operationID).then((matches) => matches[0]?.engine).catch(() => undefined);
				const [log, engine] = await Promise.all([timelineLogPager.current.read(
                    offset => offset === 0 ? getOperationLog(event.operationID!) : getOperationLog(event.operationID!, undefined, offset),
                    () => requestSequence === timelineOutputRequestSequence.current, event.engine, backupCompletedWithIssues(operations.find(operation => operation.id === event.operationID), event), logPresentation(event.operationKind, backupLockedFilesSkipped(operations.find(operation => operation.id === event.operationID), event))), operationEngine]);
				if (requestSequence !== timelineOutputRequestSequence.current) return;
				setOpenOutputText(log.available ? log.output : "");
				setOpenOutputPage(log);
				setOpenOutputEngine(engine);
				setOpenOutputLoadFailed(false);
			} catch {
				if (requestSequence !== timelineOutputRequestSequence.current) return;
				setOpenOutputText("");
				setOpenOutputPage(null);
				// Do not collapse transport/read failures into the successful empty
				// log state; the file may exist even though this request failed.
				setOpenOutputLoadFailed(true);
			}
		}
		if (requestSequence === timelineOutputRequestSequence.current) setOpenOutputLoading(false);
    };
	const loadCompletedOperationLogPage = async (offset: number) => {
		if (!detailedOperation || offset < 0) return;
		const operationID = detailedOperation.id;
		const requestSequence = ++completedLogRequestSequence.current;
		setCompletedOperationLogLoadState("pending");
		try {
			const page = await getOperationLog(operationID, undefined, offset);
			if (requestSequence !== completedLogRequestSequence.current) return;
			const next = retainReadableLog(page, completedOperationLogPage, offset);
			setCompletedOperationLog(next.output);
			setCompletedOperationLogPage(next);
			setCompletedOperationLogLoadState("loaded");
		} catch {
			if (requestSequence !== completedLogRequestSequence.current) return;
			setCompletedOperationLogLoadState("failed");
		}
	};
	const loadTimelineOperationLogPage = async (operationID: string, offset: number) => {
		const requestSequence = ++timelineOutputRequestSequence.current;
		try {
			const page = await getOperationLog(operationID, undefined, offset);
			if (requestSequence !== timelineOutputRequestSequence.current) return;
			const next = retainReadableLog(page, openOutputPage, offset);
			setOpenOutputText(next.output);
			setOpenOutputPage(next);
			setOpenOutputLoadFailed(false);
		} catch {
			if (requestSequence !== timelineOutputRequestSequence.current) return;
			setOpenOutputLoadFailed(true);
		}
	};
	const subcopy = jobsRunning
		? t("ui.overview.runningOperations", { count: activeOperations.length })
		: hasIssues
        ? renderMessage("ui.overview.issuesNeedAttention", { count: issueCount, issueLink: <button type="button" className="text-button inline-issues-link" onClick={showIssues}>{t("ui.overview.issueCount", { count: issueCount })}</button> })
        : failing.length
        ? t("ui.overview.failedTargetRun", { vault: failing[0].repositoryName })
        : stats?.lastBackup
          ? t("ui.overview.latestBackupCompleted", { time: timeAgo(stats.lastBackup) })
          : t("ui.overview.beginProtecting");
    const reviewIssues = async () => {
        setReviewingIssues(true);
        try {
            const nextStats = await markDashboardIssuesReviewed();
			issueReviewGeneration.current++;
            issueResponseGeneration.current++;
            setStats(nextStats);
            setIssueEvents((current) => current.map((event) => ({ ...event, isNew: false })));
            setIssuesCursor("");
			setIssuesLoading(filter === "issues");
			setIssueRequestKey((key) => key + 1);
            toast("ok", t("ui.overview.issuesReviewed"));
        } catch (reason) {
            toast("error", (reason as Error).message);
        } finally {
            setReviewingIssues(false);
        }
    };
	const closeActiveOperation = () => {
		querySelectedOperationID.current = "";
		resetOperationDetail();
		if (searchParams.has("operation")) {
			const nextParams = new URLSearchParams(searchParams);
			nextParams.delete("operation");
			setSearchParams(nextParams, { replace: true });
		}
	};
	const submitCancellation = async () => {
		if (!selectedActiveOperation || cancelSubmitting || cancelAccepted) return;
		setCancelSubmitting(true);
		try {
			await cancelOperation(selectedActiveOperation.id);
			setCancelAccepted(true);
			setConfirmCancel(false);
			setCancelError("");
		} catch {
			setConfirmCancel(false);
			setCancelError(t("ui.overview.cancelOperationFailed"));
			cancelFreshAfterRequest.current = activeDetailRequestSequence.current;
			setCancelNeedsFreshSnapshot(true);
		} finally {
			setCancelSubmitting(false);
		}
	};
	const protectionRingContents = jobsRunning ? (
		<div className="running-indicator" role="img" aria-label={t("ui.overview.jobsRunning")}>
			<span className="pacman-motion" aria-hidden="true" />
			<span>{t("ui.pages.overview.running")}</span>
		</div>
	) : (
        <>
            <svg viewBox="0 0 190 190" aria-hidden="true">
                <circle className="ring-track" cx="95" cy="95" r="84" />
                <circle
                    className="ring-progress"
                    cx="95"
                    cy="95"
                    r="84"
                    pathLength="100"
                    strokeDasharray="100"
                    strokeDashoffset={hasIssues ? 0 : 100 - percent}
                />
            </svg>
            <div className="ring-copy">
                {hasIssues ? (
                    <>
                        <strong className="ring-alert-mark" aria-hidden="true">!</strong>
                        <span>{t("ui.pages.overview.issues.found")}</span>
                    </>
                ) : (
                    <>
                        <strong>{formatDisplayNumber(percent)}%</strong>
                        <span>{t("ui.pages.overview.protected")}</span>
                    </>
                )}
            </div>
        </>
	);
	const detailedOperation = operationDetail?.operation ?? selectedActiveOperation;
	const detailedOperationActive = detailedOperation ? operationIsActive(detailedOperation) : false;
	// A download's live log holds only the engine's error output, so it is
	// usually empty while the file transfers; its waiting line says so.
	const waitingForOutput = detailedOperation?.kind === "download"
		? t("ui.pages.overview.waiting.for.the.download")
		: t("ui.pages.overview.waiting.for.output");
	const canceling = cancelAccepted || Boolean(operationDetail?.live.cancelRequested);

    return (
        <div className="page overview-page">
            {error && <div className="inline-error overview-error">{error}</div>}

            {gettingStarted ? (
				<section className="getting-started" aria-labelledby="getting-started-title">
					<div className="getting-started-copy">
						<span className="getting-started-kicker">{t("ui.pages.overview.getting.started")}</span>
						<h1 id="getting-started-title">{t("ui.pages.overview.hi.there.ready.to.protect.your.data")}</h1>
						<Link className="btn primary" to="/protect#vaults">{t("ui.pages.overview.add.a.vault")}</Link>
					</div>
					<ol className="getting-started-steps">
						<li>
							<strong>{t("ui.pages.overview.step.1.add.a.vault")}</strong>
							<span>{t("ui.pages.overview.create.a.new.encrypted.vault.or.connect.an.existing.one.local.network")}</span>
						</li>
						<li>
							<strong>{t("ui.pages.overview.step.2.create.a.backup.job")}</strong>
							<span>{t("ui.pages.overview.choose.the.files.you.want.to.protect.and.vault.you.want.to.back.them.u")}</span>
						</li>
						<li>
							<strong>{t("ui.pages.overview.step.3.watch.replicaro.work")}</strong>
							<span>{t("ui.pages.overview.replicaro.automatically.backs.up.your.files.based.on.your.selected.sch")}</span>
						</li>
					</ol>
				</section>
			) : needsFirstJob ? (
				<section className="getting-started" aria-labelledby="first-job-title">
					<div className="getting-started-copy">
						<span className="getting-started-kicker">{t("ui.pages.overview.getting.started")}</span>
						<h1 id="first-job-title">{t("ui.pages.overview.create.your.first.backup.job")}</h1>
						<Link className="btn primary" to="/protect#jobs">{t("ui.pages.overview.create.a.backup.job")}</Link>
					</div>
					<ol className="getting-started-steps">
						<li>
							<strong>{t("ui.pages.overview.step.1.choose.what.to.protect")}</strong>
							<span>{t("ui.pages.overview.select.the.files.or.folders.you.want.replicaro.to.back.up")}</span>
						</li>
						<li>
							<strong>{t("ui.pages.overview.step.2.choose.a.vault")}</strong>
							<span>{t("ui.overview.chooseBackupDestinationHelp")}</span>
						</li>
						<li>
							<strong>{t("ui.pages.overview.step.3.pick.a.schedule")}</strong>
							<span>{t("ui.pages.overview.choose.when.the.job.should.run.then.let.replicaro.protect.your.data.au")}</span>
						</li>
					</ol>
				</section>
			) : (
			<section className="protection-hero" aria-label={jobsRunning ? t("ui.overview.jobsRunning") : hasIssues ? t("ui.overview.issueAttentionShort", { count: issueCount }) : t("ui.overview.percentProtected", { percent })}>
                {jobsRunning ? (
					<div className="protection-ring running">
						{protectionRingContents}
					</div>
				) : hasIssues ? (
                    <button type="button" className={`protection-ring ${ringTone}`} onClick={showIssues} aria-label={t("ui.pages.overview.view.dashboard.issues")}>
                        {protectionRingContents}
                    </button>
                ) : (
                    <div className={`protection-ring ${ringTone}`}>
                        {protectionRingContents}
                    </div>
                )}

                <div className="protection-copy">
                    <h1>
                        {jobsRunning
							? t("ui.overview.jobsRunningEllipsis")
							: hasIssues
                            ? t("ui.overview.thereIsProblem")
                            : percent === 100 && enabledJobs.length
                              ? t("ui.overview.everythingProtected")
                              : renderMessage("ui.overview.healthyJobs", { healthy: healthyJobs.length, total: enabledJobs.length, jobsLink: <Link to="/protect#jobs">{t("ui.pages.overview.jobs")}</Link> })}
                    </h1>
                    <p>{subcopy}</p>
                    {!jobsRunning && hasIssues && (
                        <button className={`btn ${issueTone}-outline issue-review-button`} disabled={reviewingIssues} onClick={() => void reviewIssues()}>
                            {reviewingIssues && <span className="spinner" />}
                            {t("ui.overview.markAllReviewed")}
                        </button>
                    )}
                    <div className="overview-stats">
                        <div><Link to="/protect#vaults"><strong>{formatDisplayNumber(stats?.repositoryCount ?? repos.length)}</strong><span>{t("ui.pages.overview.vaults")}</span></Link></div>
                        <SizeTile to="/protect#vaults" sizes={repos.map((repo) => repo.vaultSizeBytes)} label={t("ui.overview.vaultsSize")} measuredText={(measured, count) => t("ui.overview.vaultsMeasured", { measured, count })} />
                        <div><Link to="/protect#jobs"><strong>{formatDisplayNumber(stats?.jobCount ?? jobs.length)}</strong><span>{t("ui.pages.overview.jobs.2")}</span></Link></div>
                        <SizeTile to="/protect#jobs" sizes={jobs.map((job) => job.sizeBytes)} label={t("ui.overview.protectedData")} measuredText={(measured, count) => t("ui.overview.jobsMeasured", { measured, count })} />
                        <div className="ok"><strong>{formatDisplayNumber(stats?.successfulRuns ?? 0)}</strong><span>{t("ui.pages.overview.runs.ok")}</span></div>
                        <div className={hasIssues ? issueTone : ""}>
                            <button type="button" className="overview-stat-link" onClick={showIssues} aria-controls="timeline">
                                <strong>{formatDisplayNumber(stats?.failedRuns ?? 0)}</strong><span>{t("ui.pages.overview.issues")}</span>
                            </button>
                        </div>
                    </div>
                </div>
            </section>
			)}

            {activeOperations.length > 0 && (
                <section className="active-operations" aria-label={t("ui.pages.overview.active.operations")}>
                    <div className="section-heading">
                        <div>
                            <h2>{t("ui.pages.overview.active.now")}</h2>
                            <p className="section-copy">{t("ui.pages.overview.work.currently.running.in.the.background")}</p>
                        </div>
                        <span className="active-count">{formatDisplayNumber(activeOperations.length)}</span>
                    </div>
                    <div className="active-operation-list">
                        {activeOperations.map((operation) => (
							// Use the shared tooltip so the complete operation title and log action
							// have the same visual and keyboard treatment as other Replicaro hints.
							<Tooltip key={operation.id} content={t("ui.overview.viewLiveLogFor", { title: operation.title })}><button type="button" className="active-operation" onClick={() => openOperationDetail(operation)} aria-label={t("ui.overview.openOperation", { title: operation.title })}>
                                <span className="spinner" />
                                <div>
                                    {operation.kind === "backup" ? (
                                        <strong>
                                            <span className="active-operation-title-text">{operation.title}</span>
                                        </strong>
                                    ) : (
                                        <strong>{operation.title}</strong>
                                    )}
                                    <span className="active-operation-summary">{activeOperationSummary(operation)}</span>
									{activeRepositoryByID.get(operation.repositoryId || "")?.coldStorage && ["restore", "check", "maintenance"].includes(operation.kind) && <span>{t("ui.pages.overview.cold.storage.retrieval.can.take.hours.or.days.replicaro.is.waiting.for")}</span>}
                                </div>
							</button></Tooltip>
                        ))}
                    </div>
                </section>
            )}

			{selectedActiveOperation && detailedOperation && !confirmCancel && (
				<Modal
					title={detailedOperation.title}
					onClose={closeActiveOperation}
					wide
				>
					<div className="operation-detail">
						<div className="operation-detail-state">
							<strong className={operationHeaderTone(detailedOperation.status)}>{operationTag(detailedOperation)}</strong>
							<span>{activeOperationSummary(detailedOperation)}</span>
						</div>
						{(detailedOperation.steps ?? []).length > 0 && (
							<div className="operation-detail-steps" aria-label={t("ui.pages.overview.operation.steps")}>
								{detailedOperation.steps?.map((step) => {
									const status = operationStepStatus(step.status);
									return <div key={step.id}><strong>{operationStepLabel(step.kind)}</strong><span className={status.tone}>{status.label}</span></div>;
								})}
							</div>
						)}
						{backupLockedFilesSkipped(detailedOperation) && <SkippedLockedFiles operationID={detailedOperation.id} />}
						{/* The heading's spinner shows the operation is still running even
						    when its log has gone quiet. */}
						<div className="modal-section-label row">{t("ui.pages.overview.live.log")}{detailedOperationActive && <span className="spinner" aria-hidden="true" />}</div>
                        {!detailedOperationActive && <button type="button" className="text-button" aria-pressed={showRawLog} onClick={() => setShowRawLog((raw) => !raw)}>{showRawLog ? t("ui.pages.overview.show.readable.log") : t("ui.pages.overview.show.raw.log")}</button>}
						{detailedOperationActive ? !operationDetail ? (
							<p className="row muted"><span className="spinner" />{waitingForOutput}</p>
						) : !operationDetail.live.available ? (
							<p className="muted">{t("ui.pages.overview.live.output.is.unavailable.for.this.operation")}</p>
						) : operationDetail.live.entries.length === 0 ? (
							<p className="row muted"><span className="spinner" />{waitingForOutput}</p>
						) : (
							<pre className="output operation-live-log" aria-live="polite">{[
								// The live log is never translated, including this marker;
								// the engine output after it is shown exactly as written.
								...(operationDetail.live.truncated ? ["[Earlier live output omitted]"] : []),
								readableLog(operationDetail.live.entries.map((entry) => entry.text).join("\n"), detailedOperation, undefined, undefined, true),
							].join("\n")}</pre>
						) : (
							<>
								{/* Nothing inside the log output is translated, including these load states. */}
								<pre className="output operation-live-log">{completedOperationLogLoadState === "failed"
									? "Log could not be loaded."
									: completedOperationLogLoadState === "pending"
										? "Loading log…"
										: completedOperationLog ? showRawLog ? completedOperationLog : completedReadableLog(completedOperationLog, detailedOperation, undefined, completedOperationLogPage) : "(no output recorded)"}</pre>
								{showRawLog && operationLogHasMultiplePages(completedOperationLogPage) && completedOperationLogPage && (
									<div className="log-page-controls">
										<button type="button" className="btn" disabled={completedOperationLogPage.offset === 0 || completedOperationLogLoadState === "pending"} onClick={() => void loadCompletedOperationLogPage(completedOperationLogPage.previousOffset)}>{t("ui.pages.overview.previous.log.page")}</button>
										<span className="log-page-size">{operationLogPageLabel(completedOperationLogPage)}</span>
										<button type="button" className="btn" disabled={completedOperationLogPage.eof || completedOperationLogLoadState === "pending"} onClick={() => void loadCompletedOperationLogPage(completedOperationLogPage.nextOffset)}>{t("ui.pages.overview.next.log.page")}</button>
									</div>
								)}
							</>
						)}
						{cancelError && <div className="inline-error">{cancelError}</div>}
						{detailedOperationActive && (canceling || (operationDetail?.live.cancelable && !cancelNeedsFreshSnapshot)) && (
							<div className="modal-footer">
								{canceling ? <span className="muted">{t("ui.pages.overview.canceling")}</span> : (
									<button type="button" className="btn danger-outline" disabled={cancelSubmitting} onClick={() => setConfirmCancel(true)}>{t("ui.pages.overview.cancel.job")}</button>
								)}
							</div>
						)}
					</div>
				</Modal>
			)}

			{confirmCancel && selectedActiveOperation && (
				<Modal title={t("ui.pages.overview.cancel.operation")} onClose={() => { if (!cancelSubmitting) setConfirmCancel(false); }}>
					<p className="muted">{t("ui.pages.overview.this.stops.future.work.and.the.running.local.process.work.already.comp")}</p>
					<div className="modal-footer">
						<button type="button" className="btn" disabled={cancelSubmitting} onClick={() => setConfirmCancel(false)}>{t("ui.pages.overview.keep.running")}</button>
						<button type="button" className="btn danger" disabled={cancelSubmitting} onClick={() => void submitCancellation()}>{cancelSubmitting && <span className="spinner" />}{t("ui.pages.overview.cancel.operation.2")}</button>
					</div>
				</Modal>
			)}

            <section className="timeline-section" id="timeline">
                <div className="section-heading timeline-heading">
                    <h2>{t("ui.pages.overview.timeline")}</h2>
                    <div className="filter-pills" role="group" aria-label={t("ui.pages.overview.timeline.filters")}>
                        {timelineFilters.map(({ value, label }) => (
                            <button
                                key={value}
                                className={filter === value ? "active" : ""}
                                onClick={() => selectFilter(value)}
                            >
                                {label()}
                            </button>
                        ))}
                    </div>
                </div>

                {(filter === "issues" ? issuesLoading && filtered.length === 0 : timelineFeed.loading) ? (
                    <Loading />
                ) : filtered.length === 0 ? (
                    <EmptyState icon="history" title={t("ui.pages.overview.nothing.in.this.view")}>
                        <p>{t("ui.pages.overview.completed.operations.and.system.activity.will.appear.here")}</p>
                    </EmptyState>
                ) : (
                    <div className="timeline-list">
                        {filtered.slice(0, filter === "issues" ? filtered.length : visible).map((event) => (
                            <article
                                key={event.id}
                                id={event.id}
                                className={`timeline-event ${event.tone}${event.isNew ? " new-issue" : ""}${event.operationID === focusedOperationID ? " notification-target" : ""}`}
                            >
                                <span className="timeline-dot" />
                                <div className="timeline-line">
                                    <time>{eventTime(event.timestamp)}</time>
                                    <span className="timeline-tag">{event.tag || <UnknownValue label={t("ui.date.unknown")} />}</span>
                                    {event.isNew && <span className="new-issue-badge">{t("ui.pages.overview.new")}</span>}
                                    <span className="timeline-title">{event.title.trim() ? event.title : <UnknownValue label={t("ui.date.unknown")} />}</span>
                                    {event.outputAvailable && (
                                        <button
                                            className="text-button"
                                            onClick={() => void toggleTimelineOutput(event)}
                                        >
											{openOutput === event.id ? t("ui.pages.overview.hide.log") : t("ui.pages.overview.log")}
                                        </button>
                                    )}
                                </div>
								{/* A password change that stopped before commit blocks every
								    operation on the vault until it is retried; its issue says
								    so and leads to the vault card's Continue password change.
								    Only while it is the vault's newest password change of any
								    status: after a later change, or while a Retry is queued or
								    running, the old issue no longer describes the vault. */}
								{event.operationKind === VAULT_PASSWORD_CHANGE_KIND && passwordChanges && passwordChangeStillBlocksVault(passwordChanges.find((operation) => operation.id === event.operationID), passwordChanges) && (
									<Link className="text-button" to="/protect#vaults">{t("ui.protect.vaultPasswordChangeBlocking")}</Link>
								)}
								{openOutput === event.id && (
									<div>
										{event.operationID && backupLockedFilesSkipped(operations.find(operation => operation.id === event.operationID), event) && <SkippedLockedFiles operationID={event.operationID} />}
										<button type="button" className="text-button" aria-pressed={showRawLog} onClick={() => setShowRawLog((raw) => !raw)}>{showRawLog ? t("ui.pages.overview.show.readable.log") : t("ui.pages.overview.show.raw.log")}</button>
										{/* Nothing inside the log output is translated, including these load states. */}
                                        <pre className="output timeline-output">{openOutputLoadFailed
											? "Log could not be loaded."
											: openOutputLoading ? "Loading log…" : openOutputText ? showRawLog ? openOutputText : completedReadableLog(openOutputText, operations.find(operation => operation.id === event.operationID), event, openOutputPage) : "(no output recorded)"}</pre>
										{showRawLog && event.operationID && operationLogHasMultiplePages(openOutputPage) && openOutputPage && (
											<div className="log-page-controls">
												<button type="button" className="btn" disabled={openOutputPage.offset === 0} onClick={() => void loadTimelineOperationLogPage(event.operationID!, openOutputPage.previousOffset)}>{t("ui.pages.overview.previous.log.page")}</button>
												<span className="log-page-size">{operationLogPageLabel(openOutputPage)}</span>
												<button type="button" className="btn" disabled={openOutputPage.eof} onClick={() => void loadTimelineOperationLogPage(event.operationID!, openOutputPage.nextOffset)}>{t("ui.pages.overview.next.log.page")}</button>
											</div>
										)}
									</div>
								)}
                            </article>
                        ))}
                    </div>
                )}

                {(filter === "issues"
                    ? !issuesLoading && issuesHasMore
                    : !timelineFeed.loading && (visible < filtered.length || timelineFeed.hasMore)) && (
                    <button className="btn load-older" disabled={loadOlderBusy} onClick={loadOlder}>
                        {loadOlderBusy && <span className="spinner" />}
                        {t("ui.overview.loadOlderEvents")}
                    </button>
                )}
            </section>
        </div>
    );
}
