import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router-dom";

import {
	getDashboard,
	getDashboardIssues,
	getActiveOperations,
	getOperationLive,
	getOperationLog,
	cancelOperation,
	getOperations,
	getOperation,
	isOperationNotFound,
	requireExactOperation,
	getJobs,
	markDashboardIssuesReviewed,
    getLogs,
    getRepositories,
} from "../services/api";
import { EmptyState, Loading, Modal, Tooltip, duration, parseTime, timeAgo, useToast } from "../components/ui";
import type {
	ActivityEntry,
	BackupJob,
	DashboardStats,
	OperationEntry,
	OperationLogResponse,
	OperationLiveResponse,
    Repository,
} from "../types";
import { dashboardIssueEvent, logTag } from "./dashboardIssue";
import type { TimelineEvent } from "./dashboardIssue";
import { formatReadableLog } from "./nativeLogFormat";
import { NativeLogPager, retainReadableLog } from "./nativeLogPaging";

type TimelineFilter = "all" | "backups" | "restores" | "checks" | "maintenance" | "other" | "issues";

const timelineFilters: Array<{ value: TimelineFilter; label: string }> = [
    { value: "all", label: "All" },
    { value: "backups", label: "Backups" },
    { value: "restores", label: "Restores" },
    { value: "checks", label: "Integrity checks" },
    { value: "maintenance", label: "Space reclamation" },
    { value: "other", label: "Other" },
    { value: "issues", label: "Issues" },
];

const TIMELINE_PAGE_SIZE = 10;
const TIMELINE_LOAD_MORE_SIZE = 50;
const MAX_RETAINED_TERMINAL_OPERATIONS = 200;
const CANONICAL_UUID_V4 = /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/;

const logPresentation = (kind?: string) => ({concise: true, operationKind: kind, maintenanceOnly: kind === "prune" || kind === "maintenance"});

async function loadDashboardSnapshot(operationID: string, requiredOperationIDs: string[] = []) {
	const exactIDs = [...new Set([...requiredOperationIDs, ...(operationID ? [operationID] : [])])];
	const [stats, jobs, repos, operations, logs, exactOperationGroups] = await Promise.all([
		getDashboard(),
		getJobs(),
		getRepositories(),
		getOperations(),
		getLogs(),
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
				throw new Error("A completed operation could not be loaded.");
			}
			return [exact];
		})),
	]);
	const operationByID = new Map(
		[...operations, ...exactOperationGroups.flat()].map((operation) => [operation.id, operation]),
	);
	return { stats, jobs, repos, operations: [...operationByID.values()], logs };
}

async function loadRequiredTerminalOperations(ids: string[]) {
	return Promise.all(ids.map(async (id) => {
		const matches = await getOperation(id);
		const exact = matches.find((operation) => operation.id === id);
		if (!exact || operationIsActive(exact)) throw new Error("A completed operation could not be loaded.");
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
	const format = (bytes: number) => unitIndex === 0 ? String(bytes) : String(Number((bytes / divisor).toPrecision(3)));
	const unit = logSizeUnits[unitIndex];
	if (unitIndex === 0) {
		const rangeUnit = page.offset + 1 === page.nextOffset ? "byte" : "bytes";
		return `${page.offset + 1}–${page.nextOffset} ${rangeUnit} of ${page.size}-byte log`;
	}
	return `${format(page.offset + 1)}–${format(page.nextOffset)} ${unit} of ${format(page.size)} ${unit} log`;
}

function eventTime(value: string) {
    const date = parseTime(value);
    if (!date) return "—";

    const today = new Date();
    if (date.toDateString() === today.toDateString()) {
        return date.toLocaleTimeString(undefined, {
            hour: "2-digit",
            minute: "2-digit",
            hour12: true,
        });
    }
    return date.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

function operationTag(entry: OperationEntry) {
	if (entry.status === "queued") return "queued";
	if (entry.status === "running") return "running";
    if (entry.status === "interrupted") return "stopped";
    if (entry.status === "failed") return "failed";
	if (entry.status === "reconnect_required") return "Reconnect required";
    if (entry.status === "completed_with_issues") return "completed with issues";
    if (entry.status === "partial") return "partial";
    if (entry.status === "success" && (entry.kind === "backup" || entry.kind === "restore")) return "success";
    return entry.kind;
}

function operationTitle(entry: OperationEntry) {
    const elapsed = duration(entry.startedAt, entry.finishedAt);
    return `${entry.title}${elapsed !== "—" ? ` · ${elapsed}` : ""}`;
}

function activeOperationProgress(entry: OperationEntry) {
    if (entry.kind !== "backup") {
        return `${entry.status === "queued" ? "queued" : "started"} ${timeAgo(entry.startedAt)}`;
    }
    const backup = entry.steps?.find((step) =>
        step.domain === "native" && step.kind === "backup" && step.status === "succeeded"
    );
    if (!backup) {
        return `${entry.status === "queued" ? "queued" : "started"} ${timeAgo(entry.startedAt)}`;
    }
    // Restic retention is a native child; Kopia retention remains part of its backup child.
    const retentionRunning = entry.steps?.some((step) =>
        step.domain === "native" && step.kind === "retention" && step.status === "running"
    );
    return `Snapshot finished ${timeAgo(backup.finishedAt)} · ${retentionRunning ? "retention running" : "finalizing"}`;
}

function backupCompletedWithIssues(operation?: OperationEntry, event?: TimelineEvent) {
    return (operation?.kind ?? event?.operationKind) === "backup" &&
        (operation ? operation.status === "completed_with_issues" : event?.tag === "completed with issues");
}

export default function Overview() {
    const [stats, setStats] = useState<DashboardStats | null>(null);
    const [jobs, setJobs] = useState<BackupJob[]>([]);
    const [repos, setRepos] = useState<Repository[]>([]);
    const [operations, setOperations] = useState<OperationEntry[]>([]);
    const [activeOperations, setActiveOperations] = useState<OperationEntry[]>([]);
    const [activeOperationsLoaded, setActiveOperationsLoaded] = useState(false);
	const [selectedActiveOperation, setSelectedActiveOperation] = useState<OperationEntry | null>(null);
	const [operationDetail, setOperationDetail] = useState<OperationLiveResponse | null>(null);
	const [confirmCancel, setConfirmCancel] = useState(false);
	const [cancelAccepted, setCancelAccepted] = useState(false);
	const [cancelError, setCancelError] = useState("");
	const [cancelSubmitting, setCancelSubmitting] = useState(false);
	const [cancelNeedsFreshSnapshot, setCancelNeedsFreshSnapshot] = useState(false);
    const [logs, setLogs] = useState<ActivityEntry[]>([]);
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

	const commitDashboardSnapshot = useCallback((snapshot: Awaited<ReturnType<typeof loadDashboardSnapshot>>) => {
		setStats(snapshot.stats);
		setJobs(snapshot.jobs);
		setRepos(snapshot.repos);
		// Only exact terminal rows learned by this run-local disappearance
		// coordinator survive the server's ordinary newest-200 snapshot.
		const operationByID = new Map(retainedTerminalOperations.current);
		snapshot.operations.forEach((operation) => operationByID.set(operation.id, operation));
		setOperations([...operationByID.values()]);
		setLogs(snapshot.logs);
		const pendingActive = pendingActiveCommit.current;
		if (pendingActive) {
			setActiveOperations(pendingActive);
			setActiveOperationsLoaded(true);
			priorActiveOperationIDs.current = new Set(pendingActive.map((operation) => operation.id));
			pendingActiveCommit.current = null;
		}
		setError("");
	}, []);

    useEffect(() => {
        let active = true;
        const load = () => {
			if (terminalRefreshInFlight.current) return;
			const dashboardGeneration = ++dashboardRequestSequence.current;
			const reviewGeneration = issueReviewGeneration.current;
            const feedGeneration = issueFeedGeneration.current;
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
            loadDashboardSnapshot(dashboardOperationID)
                .then((snapshot) => {
                    if (!active ||
						dashboardGeneration !== dashboardRequestSequence.current ||
                        reviewGeneration !== issueReviewGeneration.current) return;
					commitDashboardSnapshot(snapshot);
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
    }, [commitDashboardSnapshot, dashboardOperationID, filter, issueRequestKey]);

    useEffect(() => {
		let active = true;
		let requestGeneration = 0;
		const refreshTerminal = async () => {
			if (terminalRefreshInFlight.current) return;
			terminalRefreshInFlight.current = true;
			const refreshOwner = ++terminalRefreshOwner.current;
			dashboardRequestSequence.current++;
			const refreshIDs = new Set<string>();
			try {
				const requiredIDs = [...pendingTerminalIDs.current];
				pendingTerminalIDs.current.clear();
				requiredIDs.forEach((id) => refreshIDs.add(id));
				let snapshot = await loadDashboardSnapshot(dashboardOperationIDRef.current, requiredIDs);
				if (!active) return;

				// A later disappearance means the first full snapshot can no longer be
				// paired with the newest jobs/logs/dashboard truth. Coalesce all such
				// IDs into one full rerun; later arrivals need only their exact rows.
				if (pendingTerminalIDs.current.size > 0) {
					const additionalIDs = [...pendingTerminalIDs.current];
					pendingTerminalIDs.current.clear();
					additionalIDs.forEach((id) => refreshIDs.add(id));
					snapshot = await loadDashboardSnapshot(dashboardOperationIDRef.current, [...refreshIDs]);
					if (!active) return;
				}
				const operationByID = new Map(snapshot.operations.map((operation) => [operation.id, operation]));
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
				commitDashboardSnapshot({ ...snapshot, operations: [...operationByID.values()] });
			} catch (reason) {
				if (!active) return;
				const pendingActive = pendingActiveCommit.current;
				if (pendingActive) {
					setActiveOperations(pendingActive);
					setActiveOperationsLoaded(true);
					pendingActiveCommit.current = null;
				}
				// Keep failed exact identities in the transition detector so the next
				// ordinary active poll can retry without showing them as still active.
				const retryIDs = new Set((pendingActive ?? []).map((operation) => operation.id));
				refreshIDs.forEach((id) => retryIDs.add(id));
				pendingTerminalIDs.current.forEach((id) => retryIDs.add(id));
				priorActiveOperationIDs.current = retryIDs;
				setError((reason as Error).message);
				pendingTerminalIDs.current.clear();
			} finally {
				if (terminalRefreshOwner.current === refreshOwner) terminalRefreshInFlight.current = false;
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
	}, [commitDashboardSnapshot]);

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
                            () => active && generation === activeDetailGeneration.current, response.operation.engine, backupCompletedWithIssues(response.operation), logPresentation(response.operation.kind));
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
            operationStatus: operation?.status ?? (event?.tag === "completed with issues" ? "completed_with_issues" : undefined),
            operationKind: operation?.kind ?? event?.operationKind,
            engine: operation?.engine ?? event?.engine ?? openOutputEngine,
        }, live, page?.readableBody, page?.readableDiagnostics);
    };

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
				engine: operation.engine,
                // A skipped or unfamiliar persisted result is not proof of failure.
                // Keep the warning fallback; only known failure outcomes use red.
                tone: operation.status === "success" ? "ok" : ["failed", "interrupted", "partial", "reconnect_required"].includes(operation.status) ? "danger" : "warn",
                tag: operationTag(operation),
                title: operationTitle({ ...operation, title }),
                outputAvailable: true,
                issue: operation.status === "failed" || operation.status === "interrupted" || operation.status === "partial" || operation.status === "completed_with_issues" || operation.status === "reconnect_required",
            };
        });

        const logEvents = logs.map((entry): TimelineEvent => {
            const issue = entry.level === "ERROR" || entry.level === "WARN";
            return {
                id: `log-${entry.id}`,
                timestamp: entry.timestamp,
                kind: "log",
                tone: entry.level === "ERROR" ? "danger" : entry.level === "WARN" ? "warn" : "faint",
                tag: logTag(entry),
                title: entry.message,
                outputAvailable: false,
                issue,
            };
        });

        return [...operationEvents, ...logEvents].sort((a, b) => {
            const aTime = parseTime(a.timestamp)?.getTime() ?? 0;
            const bTime = parseTime(b.timestamp)?.getTime() ?? 0;
            return bTime - aTime;
        });
    }, [jobs, logs, operations, repos]);

    useEffect(() => {
        const operationID = searchParams.get("timelineOperation") ?? "";
        if (!operationID || focusedOperationRef.current === operationID) return;
        const index = timeline.findIndex((event) => event.operationID === operationID);
        if (index < 0) return;

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
    }, [searchParams, setSearchParams, timeline]);

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
	const activeBackupOperations = activeOperations.filter((operation) => operation.kind === "backup");
	const jobsRunning = activeBackupOperations.length > 0;
	const gettingStarted = stats !== null && !error && !hasIssues && !jobsRunning && repos.length === 0 && jobs.length === 0;
	const needsFirstJob = stats !== null && !error && !hasIssues && !jobsRunning && repos.length > 0 && jobs.length === 0;
    const issueTone = issueCount > (stats?.warningRuns ?? 0) ? "danger" : "warn";
    const ringTone = hasIssues ? issueTone : percent === 100 ? "ok" : percent === 0 ? "danger" : "warn";
    const failing = enabledTargets.filter((target) => target.lastStatus === "failed" || target.lastStatus === "interrupted" || target.lastStatus === "reconnect_required");
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
            setVisible((count) => count + TIMELINE_LOAD_MORE_SIZE);
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
                    () => requestSequence === timelineOutputRequestSequence.current, event.engine, backupCompletedWithIssues(operations.find(operation => operation.id === event.operationID), event), logPresentation(event.operationKind)), operationEngine]);
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
		? `${activeBackupOperations.length} backup operation${activeBackupOperations.length === 1 ? " is" : "s are"} currently queued or running.`
		: hasIssues
        ? <><button type="button" className="text-button inline-issues-link" onClick={showIssues}>{issueCount} issue{issueCount === 1 ? "" : "s"}</button> need{issueCount === 1 ? "s" : ""} attention. Review the &quot;Issues&quot; timeline below.</>
        : failing.length
        ? `${failing[0].repositoryName} has a failed target run. Review its output in the timeline below.`
        : stats?.lastBackup
          ? `The latest backup completed ${timeAgo(stats.lastBackup)}.`
          : "Create and run a backup job to begin protecting your data.";
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
            toast("ok", "Issues marked as reviewed");
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
			setCancelError("Could not cancel this operation. Try again.");
			cancelFreshAfterRequest.current = activeDetailRequestSequence.current;
			setCancelNeedsFreshSnapshot(true);
		} finally {
			setCancelSubmitting(false);
		}
	};
	const protectionRingContents = jobsRunning ? (
		<div className="running-indicator" role="img" aria-label="Backup jobs running">
			<span className="pacman-motion" aria-hidden="true" />
			<span>Running</span>
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
                        <span>Issues found</span>
                    </>
                ) : (
                    <>
                        <strong>{percent}%</strong>
                        <span>Protected</span>
                    </>
                )}
            </div>
        </>
	);
	const detailedOperation = operationDetail?.operation ?? selectedActiveOperation;
	const detailedOperationActive = detailedOperation ? operationIsActive(detailedOperation) : false;
	const canceling = cancelAccepted || Boolean(operationDetail?.live.cancelRequested);

    return (
        <div className="page overview-page">
            {error && <div className="inline-error overview-error">{error}</div>}

            {gettingStarted ? (
				<section className="getting-started" aria-labelledby="getting-started-title">
					<div className="getting-started-copy">
						<span className="getting-started-kicker">Getting started</span>
						<h1 id="getting-started-title">Hi there. Ready to protect your data?</h1>
						<Link className="btn primary" to="/protect#vaults">Add a vault</Link>
					</div>
					<ol className="getting-started-steps">
						<li>
							<strong>Step 1: Add a vault.</strong>
							<span>Create a new encrypted vault or connect an existing one. Local, network, and cloud vaults are supported.</span>
						</li>
						<li>
							<strong>Step 2: Create a backup job.</strong>
							<span>Choose the files you want to protect and vault you want to back them up into. Also pick the schedule on which you want the backup to run.</span>
						</li>
						<li>
							<strong>Step 3: Watch Replicaro work.</strong>
							<span>Replicaro automatically backs up your files based on your selected schedule.</span>
						</li>
					</ol>
				</section>
			) : needsFirstJob ? (
				<section className="getting-started" aria-labelledby="first-job-title">
					<div className="getting-started-copy">
						<span className="getting-started-kicker">Getting started</span>
						<h1 id="first-job-title">Create your first backup job</h1>
						<Link className="btn primary" to="/protect#jobs">Create a backup job</Link>
					</div>
					<ol className="getting-started-steps">
						<li>
							<strong>Step 1: Choose what to protect.</strong>
							<span>Select the files or folders you want Replicaro to back up.</span>
						</li>
						<li>
							<strong>Step 2: Choose a vault.</strong>
							<span>Select where Replicaro should save this job&apos;s backups.</span>
						</li>
						<li>
							<strong>Step 3: Pick a schedule.</strong>
							<span>Choose when the job should run, then let Replicaro protect your data automatically.</span>
						</li>
					</ol>
				</section>
			) : (
			<section className="protection-hero" aria-label={jobsRunning ? "Jobs are running" : hasIssues ? `${issueCount} issue${issueCount === 1 ? "" : "s"} need attention` : `${percent}% protected`}>
                {jobsRunning ? (
					<div className="protection-ring running">
						{protectionRingContents}
					</div>
				) : hasIssues ? (
                    <button type="button" className={`protection-ring ${ringTone}`} onClick={showIssues} aria-label="View dashboard issues">
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
							? "Jobs are running…"
							: hasIssues
                            ? "There is a problem"
                            : percent === 100 && enabledJobs.length
                              ? "Everything is protected"
                              : <>{healthyJobs.length} of {enabledJobs.length} <Link to="/protect#jobs">jobs</Link> healthy</>}
                    </h1>
                    <p>{subcopy}</p>
                    {!jobsRunning && hasIssues && (
                        <button className={`btn ${issueTone}-outline issue-review-button`} disabled={reviewingIssues} onClick={() => void reviewIssues()}>
                            {reviewingIssues && <span className="spinner" />}
                            Mark all as reviewed
                        </button>
                    )}
                    <div className="overview-stats">
                        <div><Link to="/protect#vaults"><strong>{stats?.repositoryCount ?? repos.length}</strong><span>Vaults</span></Link></div>
                        <div><Link to="/protect#jobs"><strong>{stats?.jobCount ?? jobs.length}</strong><span>Jobs</span></Link></div>
                        <div className="ok"><strong>{stats?.successfulRuns ?? 0}</strong><span>Runs ok</span></div>
                        <div className={hasIssues ? issueTone : ""}>
                            <button type="button" className="overview-stat-link" onClick={showIssues} aria-controls="timeline">
                                <strong>{stats?.failedRuns ?? 0}</strong><span>Issues</span>
                            </button>
                        </div>
                    </div>
                </div>
            </section>
			)}

            {activeOperations.length > 0 && (
                <section className="active-operations" aria-label="Active operations">
                    <div className="section-heading">
                        <div>
                            <h2>Active now</h2>
                            <p className="section-copy">Work currently running in the background.</p>
                        </div>
                        <span className="active-count">{activeOperations.length}</span>
                    </div>
                    <div className="active-operation-list">
                        {activeOperations.map((operation) => (
							// Use the shared tooltip so the complete operation title and log action
							// have the same visual and keyboard treatment as other Replicaro hints.
							<Tooltip key={operation.id} content={`View live log for - ${operation.title}`}><button type="button" className="active-operation" onClick={() => openOperationDetail(operation)} aria-label={`Open ${operation.title}`}>
                                <span className="spinner" />
                                <div>
                                    {operation.kind === "backup" ? (
                                        <strong>
                                            <span className="active-operation-title-text">{operation.title}</span>
                                        </strong>
                                    ) : (
                                        <strong>{operation.title}</strong>
                                    )}
                                    <span>{operation.kind} · {operation.engine} · {activeOperationProgress(operation)}</span>
									{activeRepositoryByID.get(operation.repositoryId || "")?.coldStorage && ["restore", "check", "maintenance"].includes(operation.kind) && <span>Cold storage retrieval can take hours or days. Replicaro is waiting for native Restic. Native prune may thaw, download, repack, re-upload, and delete archived packs; provider latency, cost, minimum-duration, and temporary-copy rules vary. Cancellation or shutdown stops the local process, but provider restore requests may continue.</span>}
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
							<strong>{operationTag(detailedOperation)}</strong>
							<span>{detailedOperation.kind} · {detailedOperation.engine} · {activeOperationProgress(detailedOperation)}</span>
						</div>
						{(detailedOperation.steps ?? []).length > 0 && (
							<div className="operation-detail-steps" aria-label="Operation steps">
								{detailedOperation.steps?.map((step) => (
									<div key={step.id}><strong>{step.kind.replaceAll("_", " ")}</strong><span>{step.status}</span></div>
								))}
							</div>
						)}
						<div className="modal-section-label">Live log</div>
                        {!detailedOperationActive && <button type="button" className="text-button" aria-pressed={showRawLog} onClick={() => setShowRawLog((raw) => !raw)}>{showRawLog ? "Show readable log" : "Show raw log"}</button>}
						{detailedOperationActive ? !operationDetail ? (
							<p className="muted">Waiting for output…</p>
						) : !operationDetail.live.available ? (
							<p className="muted">Live output is unavailable for this operation.</p>
						) : operationDetail.live.entries.length === 0 ? (
							<p className="muted">Waiting for output…</p>
						) : (
							<pre className="output operation-live-log" aria-live="polite">{[
								...(operationDetail.live.truncated ? ["[Earlier live output omitted]"] : []),
								readableLog(operationDetail.live.entries.map((entry) => entry.text).join("\n"), detailedOperation, undefined, undefined, true),
							].join("\n")}</pre>
						) : (
							<>
								<pre className="output operation-live-log">{completedOperationLogLoadState === "failed"
									? "Log could not be loaded."
									: completedOperationLogLoadState === "pending"
										? "Loading log…"
										: completedOperationLog ? showRawLog ? completedOperationLog : readableLog(completedOperationLog, detailedOperation, undefined, completedOperationLogPage) : "(no output recorded)"}</pre>
								{showRawLog && operationLogHasMultiplePages(completedOperationLogPage) && completedOperationLogPage && (
									<div className="log-page-controls">
										<button type="button" className="btn" disabled={completedOperationLogPage.offset === 0 || completedOperationLogLoadState === "pending"} onClick={() => void loadCompletedOperationLogPage(completedOperationLogPage.previousOffset)}>Previous log page</button>
										<span className="log-page-size">{operationLogPageLabel(completedOperationLogPage)}</span>
										<button type="button" className="btn" disabled={completedOperationLogPage.eof || completedOperationLogLoadState === "pending"} onClick={() => void loadCompletedOperationLogPage(completedOperationLogPage.nextOffset)}>Next log page</button>
									</div>
								)}
							</>
						)}
						{cancelError && <div className="inline-error">{cancelError}</div>}
						{detailedOperationActive && (canceling || (operationDetail?.live.cancelable && !cancelNeedsFreshSnapshot)) && (
							<div className="modal-footer">
								{canceling ? <span className="muted">Canceling…</span> : (
									<button type="button" className="btn danger-outline" disabled={cancelSubmitting} onClick={() => setConfirmCancel(true)}>Cancel job</button>
								)}
							</div>
						)}
					</div>
				</Modal>
			)}

			{confirmCancel && selectedActiveOperation && (
				<Modal title="Cancel operation?" onClose={() => { if (!cancelSubmitting) setConfirmCancel(false); }}>
					<p className="muted">This stops future work and the running local process. Work already completed will not be undone.</p>
					<div className="modal-footer">
						<button type="button" className="btn" disabled={cancelSubmitting} onClick={() => setConfirmCancel(false)}>Keep running</button>
						<button type="button" className="btn danger" disabled={cancelSubmitting} onClick={() => void submitCancellation()}>{cancelSubmitting && <span className="spinner" />}Cancel operation</button>
					</div>
				</Modal>
			)}

            <section className="timeline-section" id="timeline">
                <div className="section-heading timeline-heading">
                    <h2>Timeline</h2>
                    <div className="filter-pills" role="group" aria-label="Timeline filters">
                        {timelineFilters.map(({ value, label }) => (
                            <button
                                key={value}
                                className={filter === value ? "active" : ""}
                                onClick={() => selectFilter(value)}
                            >
                                {label}
                            </button>
                        ))}
                    </div>
                </div>

                {issuesLoading && filtered.length === 0 ? (
                    <Loading />
                ) : filtered.length === 0 ? (
                    <EmptyState icon="history" title="Nothing in this view">
                        <p>Completed operations and system activity will appear here.</p>
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
                                    <span className="timeline-tag">{event.tag}</span>
                                    {event.isNew && <span className="new-issue-badge">New</span>}
                                    <span className="timeline-title">{event.title}</span>
                                    {event.outputAvailable && (
                                        <button
                                            className="text-button"
                                            onClick={() => void toggleTimelineOutput(event)}
                                        >
											{openOutput === event.id ? "hide log" : "log"}
                                        </button>
                                    )}
                                </div>
								{openOutput === event.id && (
									<div>
										<button type="button" className="text-button" aria-pressed={showRawLog} onClick={() => setShowRawLog((raw) => !raw)}>{showRawLog ? "Show readable log" : "Show raw log"}</button>
                                        <pre className="output timeline-output">{openOutputLoadFailed
											? "Log could not be loaded."
											: openOutputLoading ? "Loading log…" : openOutputText ? showRawLog ? openOutputText : readableLog(openOutputText, operations.find(operation => operation.id === event.operationID), event, openOutputPage) : "(no output recorded)"}</pre>
										{showRawLog && event.operationID && operationLogHasMultiplePages(openOutputPage) && openOutputPage && (
											<div className="log-page-controls">
												<button type="button" className="btn" disabled={openOutputPage.offset === 0} onClick={() => void loadTimelineOperationLogPage(event.operationID!, openOutputPage.previousOffset)}>Previous log page</button>
												<span className="log-page-size">{operationLogPageLabel(openOutputPage)}</span>
												<button type="button" className="btn" disabled={openOutputPage.eof} onClick={() => void loadTimelineOperationLogPage(event.operationID!, openOutputPage.nextOffset)}>Next log page</button>
											</div>
										)}
									</div>
								)}
                            </article>
                        ))}
                    </div>
                )}

                {!issuesLoading && ((filter === "issues" && issuesHasMore) || (filter !== "issues" && visible < filtered.length)) && (
                    <button className="btn load-older" disabled={issuesLoading} onClick={loadOlder}>
                        {issuesLoading && <span className="spinner" />}
                        Load older events
                    </button>
                )}
            </section>
        </div>
    );
}
