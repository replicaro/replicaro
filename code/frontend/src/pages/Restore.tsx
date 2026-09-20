import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate, useParams } from "react-router-dom";

import { resticArchiveDisplayPath, resticNativeAddress } from "../nativePathDisplay";
import { DirectoryField } from "../components/DirectoryPicker";
import { PaginationControls } from "../components/ListControls";
import { MetadataIndexNotice } from "../components/MetadataIndexNotice";
import { EmptyState, Icon, Loading, Modal, parseTime, Tooltip, useToast } from "../components/ui";
import {
	APIError,
	getEngines,
	getMetadataStatus,
	getOperation,
	getRepositories,
	deleteSnapshot,
	listCachedSnapshots,
	listSnapshotFiles,
	prepareMetadata,
	refreshRestoreSnapshots,
	observeSubmittedRestoreOperation,
	isOperationNotFound,
	requireExactOperation,
	restoreSnapshot,
} from "../services/api";
import type { MetadataPreparationStatus } from "../services/api";
import { defaultConflictMode, restoreCapability, restoreConflictModeLabel } from "../restoreCapabilities";
import type { EngineDescriptor, Repository, Snapshot, SnapshotEntry } from "../types";
import type { MetadataIndexState } from "../types";

const PAGE_SIZE = 6;
const PAGE_SIZE_OPTIONS = [PAGE_SIZE, 9, 18, 36] as const;
const PAGE_SIZE_STORAGE_KEY = "replicaro.restore.snapshots.pageSize";
const SNAPSHOT_HISTORY_TOOLTIP = "Replicaro periodically refreshes snapshot metadata for this vault. You can force a refresh immediately. Your backed up files are not changed. Forced refreshes read from the vault's destination and may take time to complete.";

type SnapshotRoot = NonNullable<Snapshot["sourceRoots"]>[number];
type RestoreChoice = { kind: "snapshot"; repositoryId: string; snapshot: Snapshot; path: string; nativeRootId?: string };

function readPageSize() {
	try {
		const stored = Number(window.localStorage.getItem(PAGE_SIZE_STORAGE_KEY));
		return PAGE_SIZE_OPTIONS.includes(stored as typeof PAGE_SIZE_OPTIONS[number]) ? stored : PAGE_SIZE;
	} catch {
		return PAGE_SIZE;
	}
}

function displayPath(value: string) { return /^[A-Za-z]:[\\/]/.test(value) ? value.replace(/\//g, "\\") : value; }

function isWindowsPath(value: string) {
	return /^[A-Za-z]:[\\/]/.test(value) || value.startsWith("\\\\");
}

function dateKey(value: string | Date) {
    const date = typeof value === "string" ? parseTime(value) : value;
    if (!date) return "";
    return [date.getFullYear(), String(date.getMonth() + 1).padStart(2, "0"), String(date.getDate()).padStart(2, "0")].join("-");
}

function friendlyDate(value: string) {
    const date = parseTime(value);
    if (!date) return "Unknown";
    const today = new Date();
    const current = new Date(today.getFullYear(), today.getMonth(), today.getDate()).getTime();
    const then = new Date(date.getFullYear(), date.getMonth(), date.getDate()).getTime();
    const days = Math.round((current - then) / 86400000);
    if (days === 0) return "Today";
    if (days === 1) return "Yesterday";
    if (days > 1 && days < 7) return date.toLocaleDateString(undefined, { weekday: "long" });
    return date.toLocaleDateString(undefined, { month: "long", day: "numeric" });
}

function fullDate(value: string) {
    const date = parseTime(value);
    return date?.toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" }) ?? "—";
}

function snapshotTime(value: string) {
    const date = parseTime(value);
    return date?.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", hour12: false }) ?? "—";
}

function snapshotMeta(snapshot: Snapshot) {
	const sources = snapshotSourceLabel(snapshot);
    return `Taken ${fullDate(snapshot.timestamp)} at ${snapshotTime(snapshot.timestamp)} · ${snapshot.size || "size unknown"} · ${sources}`;
}

function snapshotSourceLabel(snapshot: Snapshot) {
	return snapshot.sourceRoots?.length
		? snapshot.sourceRoots.map(snapshotRootLabel).join("; ")
		: displayPath(snapshot.source);
}

function snapshotRoots(snapshot: Snapshot): SnapshotRoot[] {
	return snapshot.sourceRoots ?? [];
}

function snapshotRootLabel(root: SnapshotRoot) {
	return displayPath(root.path);
}

export default function Restore() {
    return <RestoreRoute />;
}

function RestoreRoute() {
    const { repoId = "" } = useParams();
    const navigate = useNavigate();
    const toast = useToast();
    const [repos, setRepos] = useState<Repository[] | null>(null);
    const [engines, setEngines] = useState<EngineDescriptor[]>([]);
    const [selected, setSelected] = useState(repoId);
    const selectedRef = useRef(repoId);
    const [snapshots, setSnapshots] = useState<Snapshot[] | null>(null);
    const [snapshotsFor, setSnapshotsFor] = useState("");
    const [error, setError] = useState("");
    const [dateFilter, setDateFilter] = useState("");
    const [calendarOpen, setCalendarOpen] = useState(false);
    const [calendarMonth, setCalendarMonth] = useState(() => new Date());
    const [managedPage, setManagedPage] = useState(0);
    const [unmanagedPage, setUnmanagedPage] = useState(0);
	const [pageSize, setPageSize] = useState(readPageSize);

    const [browsing, setBrowsing] = useState<{ snapshot: Snapshot; root: SnapshotRoot } | null>(null);
    const [browsePath, setBrowsePath] = useState("");
    const [entries, setEntries] = useState<SnapshotEntry[] | null>(null);
    const [browseRaw, setBrowseRaw] = useState("");
	const browseRequestRef = useRef<{ controller: AbortController; generation: number } | null>(null);
	const browseGenerationRef = useRef(0);
	const repositoryGenerationRef = useRef(0);
	const snapshotRequestGenerationRef = useRef(0);
	const refreshingVaultsRef = useRef(new Map<string, AbortController>());
	const snapshotSessionCacheRef = useRef(new Map<string, Snapshot[]>());
	const snapshotRequestControllerRef = useRef<{ repositoryID: string; controller: AbortController } | null>(null);
	const directlySelectedRouteRef = useRef("");
    const [restoring, setRestoring] = useState<RestoreChoice | null>(null);
    const [restoreTarget, setRestoreTarget] = useState("");
    const [conflictMode, setConflictMode] = useState("always");
    const [restoreBusy, setRestoreBusy] = useState(false);
	const [deleting, setDeleting] = useState<{ readonly repositoryId: string; readonly snapshot: Snapshot; readonly session: number } | null>(null);
	const deleteReviewOwner = useRef<{ review: NonNullable<typeof deleting>; pending: boolean } | null>(null);
	const [deleteBusy, setDeleteBusy] = useState(false);
	const [metadataState, setMetadataState] = useState<MetadataIndexState | null>(null);
	const [readySnapshotIDs, setReadySnapshotIDs] = useState<Set<string>>(() => new Set());
	const [metadataBusy, setMetadataBusy] = useState(false);
	const [metadataPaused, setMetadataPaused] = useState(false);
	const [metadataStage, setMetadataStage] = useState<MetadataPreparationStatus["stage"]>("");
	const [completeHeaderListing, setCompleteHeaderListing] = useState(false);
	const [forcingMetadata, setForcingMetadata] = useState(false);
	const restoreReviewSession = useRef(0);
	const restoreSubmissionGeneration = useRef(0);
	const restoreSubmissionOwner = useRef<{ session: number; generation: number; requestPending: boolean; handedOff: boolean; observationController: AbortController; controller?: AbortController } | null>(null);

	const cancelBrowse = useCallback(() => {
		browseGenerationRef.current += 1;
		browseRequestRef.current?.controller.abort();
		browseRequestRef.current = null;
	}, []);
	const cancelSnapshotRequest = useCallback(() => {
		const active = snapshotRequestControllerRef.current;
		if (!active) return;
		active.controller.abort();
		if (refreshingVaultsRef.current.get(active.repositoryID) === active.controller) {
			refreshingVaultsRef.current.delete(active.repositoryID);
		}
		snapshotRequestControllerRef.current = null;
	}, []);

	useEffect(() => cancelBrowse, [cancelBrowse]);

	useEffect(() => () => {
		cancelSnapshotRequest();
		snapshotRequestGenerationRef.current++;
		restoreReviewSession.current++;
		deleteReviewOwner.current = null;
		restoreSubmissionGeneration.current++;
		restoreSubmissionOwner.current?.observationController.abort();
		restoreSubmissionOwner.current = null;
	}, [cancelSnapshotRequest]);

	useEffect(() => {
		cancelBrowse();
	}, [cancelBrowse, selected]);

    useEffect(() => {
		const controller = new AbortController();
		const generation = ++repositoryGenerationRef.current;
		Promise.all([getRepositories(controller.signal), getEngines()])
            .then(([nextRepos, engineResponse]) => {
				if (controller.signal.aborted || repositoryGenerationRef.current !== generation) return;
                setRepos(nextRepos);
                setEngines(engineResponse.engines);
            })
            .catch((reason: Error) => {
				if (controller.signal.aborted || reason.name === "AbortError" || repositoryGenerationRef.current !== generation) return;
                setRepos([]);
                setError(reason.message);
            });
		return () => controller.abort();
    }, []);

    useEffect(() => {
        if (repos === null) return;
        const routeRepo = repos.find((repo) => repo.id === repoId);
        const nextSelected = routeRepo?.id ?? "";
        if (nextSelected === selectedRef.current) return;
		cancelSnapshotRequest();
		snapshotRequestGenerationRef.current++;
        cancelBrowse();
        restoreReviewSession.current++;
        restoreSubmissionGeneration.current++;
		restoreSubmissionOwner.current?.observationController.abort();
        restoreSubmissionOwner.current = null;
        setRestoreBusy(false);
        setRestoring(null);
		deleteReviewOwner.current = null;
		setDeleting(null);
		setDeleteBusy(false);
        setBrowsing(null);
        selectedRef.current = nextSelected;
        setSelected(nextSelected);
        setSnapshotsFor("");
        setDateFilter("");
        setCalendarOpen(false);
        setManagedPage(0);
        setUnmanagedPage(0);
		setError("");
		setMetadataState(null);
		setReadySnapshotIDs(new Set());
		setMetadataBusy(false);
		setMetadataPaused(false);
		setMetadataStage("");
		setCompleteHeaderListing(false);
		setForcingMetadata(false);
		const cached = snapshotSessionCacheRef.current.get(nextSelected);
		if (cached) {
			setSnapshots(cached);
			setSnapshotsFor(nextSelected);
		}
    }, [cancelBrowse, cancelSnapshotRequest, repoId, repos]);

	const commitSnapshots = useCallback((repositoryID: string, nextSnapshots: Snapshot[]) => {
		const sorted = [...nextSnapshots].sort((a, b) =>
			(parseTime(b.timestamp)?.getTime() ?? 0) - (parseTime(a.timestamp)?.getTime() ?? 0));
		snapshotSessionCacheRef.current.set(repositoryID, sorted);
		setSnapshots(sorted);
		setSnapshotsFor(repositoryID);
		const newest = parseTime(sorted[0]?.timestamp ?? "");
		if (newest) setCalendarMonth(new Date(newest.getFullYear(), newest.getMonth(), 1));
	}, []);

	const loadSelectedVault = useCallback(async (repositoryID: string, action: "access" | "retry" | "force" = "access") => {
		if (!repositoryID) return;
		const activeController = refreshingVaultsRef.current.get(repositoryID);
		if (activeController) {
			if (action !== "access") {
				if (action === "force") setForcingMetadata(true);
				try {
					let status = await prepareMetadata(repositoryID, action);
					while (!activeController.signal.aborted && (status.running || status.pending)) {
						setMetadataState(status.index);
						setReadySnapshotIDs(new Set(status.readySnapshotIds ?? []));
						setMetadataBusy(status.running || status.pending);
						setMetadataPaused(Boolean(status.paused));
						setMetadataStage(status.stage);
						setCompleteHeaderListing(status.completeHeaderListing);
						await new Promise((resolve) => window.setTimeout(resolve, 250));
						status = await getMetadataStatus(repositoryID, activeController.signal);
					}
					if (!activeController.signal.aborted && selectedRef.current === repositoryID) {
						const finalSnapshots = await refreshRestoreSnapshots(repositoryID, activeController.signal);
						if (activeController.signal.aborted || selectedRef.current !== repositoryID) return;
						commitSnapshots(repositoryID, finalSnapshots);
						setMetadataState(status.index);
						setReadySnapshotIDs(new Set(status.readySnapshotIds ?? []));
						setMetadataBusy(status.running || status.pending);
						setMetadataPaused(Boolean(status.paused));
						setMetadataStage(status.stage);
						setCompleteHeaderListing(status.completeHeaderListing);
						if (status.index.repositoryFailed) setError("Backup data could not be refreshed. Existing backup data is still available.");
						else setError("");
						if (action === "force") {
							const succeeded = !status.index.repositoryFailed && status.index.complete;
							toast(succeeded ? "ok" : "error", succeeded ? "Snapshot history refreshed." : "Backup data refresh failed.");
						}
					}
				} catch (reason) {
					if (!activeController.signal.aborted && selectedRef.current === repositoryID && (reason as Error).name !== "AbortError") {
						setError((reason as Error).message);
					}
				} finally {
					if (action === "force" && selectedRef.current === repositoryID) setForcingMetadata(false);
				}
			}
			return;
		}
		cancelSnapshotRequest();
		const controller = new AbortController();
		refreshingVaultsRef.current.set(repositoryID, controller);
		snapshotRequestControllerRef.current = { repositoryID, controller };
		const generation = ++snapshotRequestGenerationRef.current;
		const current = () => !controller.signal.aborted &&
			snapshotRequestGenerationRef.current === generation &&
			selectedRef.current === repositoryID;
		let cached: Snapshot[] = [];
		if (action === "force") setForcingMetadata(true);
		try {
			cached = await listCachedSnapshots(repositoryID, controller.signal);
			if (!current()) return;
			if (cached.length > 0) commitSnapshots(repositoryID, cached);
		} catch (reason) {
			if (!current() || (reason as Error).name === "AbortError") return;
		}
		try {
			let status = await prepareMetadata(repositoryID, action);
			if (!current()) return;
			setMetadataState(status.index);
			setReadySnapshotIDs(new Set(status.readySnapshotIds ?? []));
			setMetadataBusy(status.running || status.pending);
			setMetadataPaused(Boolean(status.paused));
			setMetadataStage(status.stage);
			setCompleteHeaderListing(status.completeHeaderListing);
			while (status.running || status.pending) {
				await new Promise((resolve) => window.setTimeout(resolve, 250));
				if (!current()) return;
				status = await getMetadataStatus(repositoryID, controller.signal);
				if (!current()) return;
				setMetadataState(status.index);
				setReadySnapshotIDs(new Set(status.readySnapshotIds ?? []));
				setMetadataBusy(status.running || status.pending);
				setMetadataPaused(Boolean(status.paused));
				setMetadataStage(status.stage);
				setCompleteHeaderListing(status.completeHeaderListing);
				cached = await refreshRestoreSnapshots(repositoryID, controller.signal);
				if (!current()) return;
				commitSnapshots(repositoryID, cached);
			}
			cached = await refreshRestoreSnapshots(repositoryID, controller.signal);
			if (!current()) return;
			commitSnapshots(repositoryID, cached);
			if (status.index.repositoryFailed) setError("Backup data could not be refreshed. Existing backup data is still available.");
			else setError("");
			if (action === "force") {
				const succeeded = !status.index.repositoryFailed && status.index.complete;
				toast(succeeded ? "ok" : "error", succeeded ? "Snapshot history refreshed." : "Backup data refresh failed.");
			}
		} catch (reason) {
			if (!current() || (reason as Error).name === "AbortError") return;
			if (cached.length === 0) commitSnapshots(repositoryID, []);
			setError((reason as Error).message);
		} finally {
			if (current()) {
				setMetadataBusy(false);
				setForcingMetadata(false);
			}
			if (refreshingVaultsRef.current.get(repositoryID) === controller) {
				refreshingVaultsRef.current.delete(repositoryID);
			}
			if (snapshotRequestControllerRef.current?.controller === controller) {
				snapshotRequestControllerRef.current = null;
			}
		}
	}, [cancelSnapshotRequest, commitSnapshots, toast]);

	useEffect(() => {
		if (repos === null) return;
		const routeRepository = repos.find((repository) => repository.id === repoId);
		if (!routeRepository || selected !== routeRepository.id) return;
		if (directlySelectedRouteRef.current === routeRepository.id) {
			directlySelectedRouteRef.current = "";
			return;
		}
		void loadSelectedVault(routeRepository.id);
	}, [loadSelectedVault, repoId, repos, selected]);

    const chooseVault = (id: string) => {
		if (!id) return;
		if (id === selectedRef.current) {
			void loadSelectedVault(id);
			return;
		}
		cancelSnapshotRequest();
		snapshotRequestGenerationRef.current++;
		cancelBrowse();
		restoreReviewSession.current++;
		restoreSubmissionGeneration.current++;
		restoreSubmissionOwner.current?.observationController.abort();
		restoreSubmissionOwner.current = null;
		setRestoreBusy(false);
		setRestoring(null);
		deleteReviewOwner.current = null;
		setDeleting(null);
		setDeleteBusy(false);
		setBrowsing(null);
        selectedRef.current = id;
        setSelected(id);
        setSnapshotsFor("");
        setDateFilter("");
        setManagedPage(0);
        setUnmanagedPage(0);
        setError("");
		directlySelectedRouteRef.current = id;
        navigate(`/restore/${encodeURIComponent(id)}`, { replace: true });
		void loadSelectedVault(id);
    };

	const forceRefresh = () => {
		if (selected) void loadSelectedVault(selected, "force");
	};

	const retryEntries = () => {
		if (selected) void loadSelectedVault(selected, "retry");
	};

    const displayedSnapshots = snapshotsFor === selected ? snapshots : null;
	const selectedRepository = repos?.find((repository) => repository.id === selected);
	const selectedEngineLabel = selectedRepository?.engine === "kopia" ? "Kopia" : selectedRepository?.engine === "restic" ? "Restic" : "native";
	const unmanagedSnapshotsTooltip = `Replicaro does not currently manage these ${selectedEngineLabel} snapshots. They may have been created outside Replicaro or belong to a job Replicaro no longer recognizes. You can browse, restore, and manually delete them, but Replicaro does not apply retention policies to them.`;
    const vaultBusy = error.trim().replace(/\s+/g, " ").toLowerCase() === "vault is busy with another operation";
    const availableDays = useMemo(() => new Set((displayedSnapshots ?? []).map((snapshot) => dateKey(snapshot.timestamp))), [displayedSnapshots]);
	const dateFiltered = (displayedSnapshots ?? []).filter((snapshot) => !dateFilter || dateKey(snapshot.timestamp) === dateFilter);
	const managedSnapshots = dateFiltered.filter((snapshot) => snapshot.presentation === "managed");
	const unmanagedSnapshots = dateFiltered.filter((snapshot) => snapshot.presentation !== "managed");
	const managedPageCount = Math.max(1, Math.ceil(managedSnapshots.length / pageSize));
	const unmanagedPageCount = Math.max(1, Math.ceil(unmanagedSnapshots.length / pageSize));
	const safeManagedPage = Math.min(managedPage, managedPageCount - 1);
	const safeUnmanagedPage = Math.min(unmanagedPage, unmanagedPageCount - 1);
	const managedPageItems = managedSnapshots.slice(safeManagedPage * pageSize, (safeManagedPage + 1) * pageSize);
	const unmanagedPageItems = unmanagedSnapshots.slice(safeUnmanagedPage * pageSize, (safeUnmanagedPage + 1) * pageSize);

	const changePageSize = (value: number) => {
		if (!PAGE_SIZE_OPTIONS.includes(value as typeof PAGE_SIZE_OPTIONS[number])) return;
		setPageSize(value);
		setManagedPage(0);
		setUnmanagedPage(0);
		try {
			window.localStorage.setItem(PAGE_SIZE_STORAGE_KEY, String(value));
		} catch {
			// Browser-local persistence is optional when storage is unavailable.
		}
	};

    const snapshotGroups = (items: Snapshot[], presentation: "managed" | "unmanaged") => {
		const result: Array<{ key: string; snapshot: Snapshot; items: Snapshot[] }> = [];
        for (const snapshot of items) {
			const key = `${presentation}:${dateKey(snapshot.timestamp)}`;
            const current = result[result.length - 1];
            if (current?.key === key) current.items.push(snapshot);
			else result.push({ key, snapshot, items: [snapshot] });
        }
        return result;
	};
	const managedGroups = snapshotGroups(managedPageItems, "managed");
	const unmanagedGroups = snapshotGroups(unmanagedPageItems, "unmanaged");

    const firstDay = new Date(calendarMonth.getFullYear(), calendarMonth.getMonth(), 1);
    const daysInMonth = new Date(calendarMonth.getFullYear(), calendarMonth.getMonth() + 1, 0).getDate();
    const leading = (firstDay.getDay() + 6) % 7;
    const calendarCells = [
        ...Array.from({ length: leading }, () => null),
        ...Array.from({ length: daysInMonth }, (_, index) => index + 1),
    ];

    const pickDate = (day: number) => {
        const next = dateKey(new Date(calendarMonth.getFullYear(), calendarMonth.getMonth(), day));
        setDateFilter((current) => current === next ? "" : next);
        setManagedPage(0);
        setUnmanagedPage(0);
    };

    const clearDate = () => {
        setDateFilter("");
        setManagedPage(0);
        setUnmanagedPage(0);
    };

	const openBrowse = (snapshot: Snapshot, root: SnapshotRoot, path = "") => {
		cancelBrowse();
		const controller = new AbortController();
		const generation = ++browseGenerationRef.current;
		browseRequestRef.current = { controller, generation };
		setBrowsing({ snapshot, root });
        setBrowsePath(path);
        setEntries(null);
        setBrowseRaw("");
		listSnapshotFiles(selected, snapshot.id, root.nativeRootId, path, controller.signal)
            .then((result) => {
				if (browseGenerationRef.current !== generation || controller.signal.aborted) return;
                setEntries(result.entries ?? []);
                setBrowseRaw(result.raw ?? "");
            })
			.catch((reason: Error) => {
				if (browseGenerationRef.current !== generation || controller.signal.aborted || reason.name === "AbortError") return;
                setEntries([]);
                toast("error", reason.message);
			})
			.finally(() => {
				if (browseRequestRef.current?.generation === generation) browseRequestRef.current = null;
			});
    };

	const closeBrowse = () => {
		cancelBrowse();
		setBrowsing(null);
	};

	const openRestore = (snapshot: Snapshot, path = "", nativeRootId?: string) => {
        if (!selected) return;
		restoreReviewSession.current++;
		restoreSubmissionOwner.current?.observationController.abort();
		restoreSubmissionOwner.current = null;
		setRestoreBusy(false);
		setRestoring({ kind: "snapshot", repositoryId: selected, snapshot, path, nativeRootId });
        setRestoreTarget("");
        setConflictMode(defaultConflictMode(selectedRepository, engines));
    };

	const dismissRestore = () => {
		if (restoreSubmissionOwner.current?.session === restoreReviewSession.current) return false;
		restoreReviewSession.current++;
		restoreSubmissionOwner.current?.observationController.abort();
		restoreSubmissionOwner.current = null;
		setRestoreBusy(false);
		setRestoring(null);
		return true;
	};

	const cancelColdRestore = () => {
		const owner = restoreSubmissionOwner.current;
		if (!owner?.controller) return;
		owner.controller.abort();
	};

    const doRestore = async () => {
        if (!restoring) return;
		if (restoreSubmissionOwner.current) return;
        // Whitespace belongs to the chosen filesystem route. Trimming here would
        // submit a different destination from the one reviewed in the dialog.
        const targetPath = restoreTarget;
        if (!targetPath) {
            toast("error", "Choose a restore destination.");
            return;
        }
		const payload = {
			operationId: window.crypto.randomUUID(),
			repositoryId: restoring.repositoryId,
			snapshotId: restoring.snapshot.id,
			path: restoring.path,
			nativeRootId: restoring.nativeRootId,
			targetPath,
			conflictMode,
			originalLocation: false,
		};
		const coldStorage = Boolean(repos?.find((repo) => repo.id === restoring.repositoryId)?.coldStorage);
		const owner = {
			session: restoreReviewSession.current,
			generation: ++restoreSubmissionGeneration.current,
			requestPending: false,
			handedOff: false,
			observationController: new AbortController(),
			controller: coldStorage ? new AbortController() : undefined,
		};
		restoreSubmissionOwner.current = owner;
		const ownsSubmission = () => restoreReviewSession.current === owner.session &&
			restoreSubmissionGeneration.current === owner.generation && restoreSubmissionOwner.current === owner;
        setRestoreBusy(true);
        try {
			const preflightTimeout = window.setTimeout(() => owner.observationController.abort(), 5000);
			let operationAbsent = false;
			const existing = await getOperation(payload.operationId, owner.observationController.signal)
				.catch((reason) => {
					// Exact lookup reports ordinary absence as HTTP 404. Only that
					// response proves this freshly generated identity is available.
					if (isOperationNotFound(reason)) {
						operationAbsent = true;
						return [];
					}
					throw reason;
				})
				.finally(() => window.clearTimeout(preflightTimeout));
			if (!ownsSubmission()) return;
			if (!operationAbsent) {
				requireExactOperation(payload.operationId, existing);
				throw new Error("operation id already exists");
			}
			owner.requestPending = true;
			const request = owner.controller
				? restoreSnapshot(payload, owner.controller.signal)
				: restoreSnapshot(payload);
			const handoff = observeSubmittedRestoreOperation(payload.operationId, () =>
				ownsSubmission() && owner.requestPending && !owner.handedOff,
				owner.observationController.signal,
			).then((operation) => {
				if (!operation || !ownsSubmission() || owner.handedOff) return false;
				owner.handedOff = true;
				owner.requestPending = false;
				owner.observationController.abort();
				setRestoring(null);
				navigate(`/?operation=${encodeURIComponent(payload.operationId)}`);
				return true;
			});
			let requestError: unknown;
			try {
				await request;
			} catch (reason) {
				requestError = reason;
			}
			// Native execution can finish and make the durable row terminal before
			// the POST reports its HTTP error. Keep exact handoff ownership until the
			// separately bounded observer settles so that result truth is not lost.
			// An HTTP error means the backend handler has returned: a durable row
			// either already exists or this was a pre-row rejection. AbortError and
			// transport failures are different because server work may still continue.
			if (!requestError || requestError instanceof APIError) owner.observationController.abort();
			const handedOff = await handoff;
			owner.observationController.abort();
			if (!handedOff && ownsSubmission()) {
				const finalController = new AbortController();
				owner.observationController = finalController;
				const finalTimeout = window.setTimeout(() => finalController.abort(), 5000);
				let finalAbsent = false;
				const finalMatches = await getOperation(payload.operationId, finalController.signal)
					.catch((reason) => {
						if (isOperationNotFound(reason)) {
							finalAbsent = true;
							return [];
						}
						throw reason;
					})
					.finally(() => window.clearTimeout(finalTimeout));
				if (!ownsSubmission()) return;
				if (!finalAbsent) {
					requireExactOperation(payload.operationId, finalMatches);
					owner.handedOff = true;
					finalController.abort();
					setRestoring(null);
					navigate(`/?operation=${encodeURIComponent(payload.operationId)}`);
					return;
				}
				if (requestError) throw requestError;
				owner.requestPending = false;
				toast("ok", `Restore completed into ${targetPath}`);
				setRestoring(null);
			}
        } catch (reason) {
			const requestWasPending = owner.requestPending;
			owner.requestPending = false;
			owner.observationController.abort();
			if (ownsSubmission() && !owner.handedOff) {
				if (requestWasPending && coldStorage && (reason as Error).name === "AbortError") toast("info", "Cold storage restore was interrupted locally. Provider restore requests may continue.");
				else toast("error", (reason as Error).message);
			}
        } finally {
			if (ownsSubmission()) {
				restoreSubmissionOwner.current = null;
				setRestoreBusy(false);
			}
        }
    };

	const openDelete = (snapshot: Snapshot) => {
		if (!selected) return;
		const review = { repositoryId: selected, snapshot, session: restoreReviewSession.current };
		deleteReviewOwner.current = { review, pending: false };
		setDeleting(review);
		setDeleteBusy(false);
	};
	const dismissDelete = () => {
		if (deleteReviewOwner.current?.pending) return;
		deleteReviewOwner.current = null;
		setDeleting(null);
	};
	const confirmDelete = async () => {
		const owner = deleteReviewOwner.current;
		if (!owner || owner.pending) return;
		const review = owner.review;
		// A copied vault can share snapshot IDs. Bind confirmation and completion
		// to this reviewed vault and view session, including browser-history moves
		// away and back. Navigation cannot undo a native deletion already started.
		const ownsReview = () => deleteReviewOwner.current === owner &&
			restoreReviewSession.current === review.session && selectedRef.current === review.repositoryId;
		if (!ownsReview()) return;
		owner.pending = true;
		setDeleteBusy(true);
		try {
			await deleteSnapshot(review.repositoryId, review.snapshot.id);
			if (!ownsReview()) return;
			setSnapshots((current) => current?.filter((snapshot) => snapshot.id !== review.snapshot.id) ?? current);
			snapshotSessionCacheRef.current.set(review.repositoryId,
				(snapshotSessionCacheRef.current.get(review.repositoryId) ?? []).filter((snapshot) => snapshot.id !== review.snapshot.id));
			setDeleting(null);
			toast("ok", "Snapshot deleted. Physical space is reclaimed only by later vault maintenance.");
		} catch (reason) {
			if (ownsReview()) toast("error", (reason as Error).message);
		} finally {
			if (ownsReview()) {
				owner.pending = false;
				setDeleteBusy(false);
			}
		}
	};
	// Whole file-root Kopia restore addresses the target itself. The native root
	// header is a UI hint here; backend fresh discovery remains authoritative.
	const restoringFileRoot = restoring?.path === "" && restoring.snapshot.nativeRootType === "f" &&
		repos?.find((repo) => repo.id === restoring.repositoryId)?.engine === "kopia";

	const crumbs = browsePath.split("/").filter(Boolean);
	// Only the native source association can make an archive breadcrumb Windows-like.
	// Keep crumbs and click arguments untouched: display conversion is not navigation.
	const breadcrumbNativePath = browsing ? resticNativeAddress(browsing.root.path, browsePath) : "";
	const breadcrumbDisplayPath = browsing?.root.path === "/" && repos?.find((repo) => repo.id === selected)?.engine === "restic"
		? resticArchiveDisplayPath(breadcrumbNativePath, browsing.snapshot.source) : breadcrumbNativePath;
	const associatedWindowsBreadcrumb = breadcrumbDisplayPath !== breadcrumbNativePath;
	const browseSeparator = associatedWindowsBreadcrumb || browsing && isWindowsPath(browsing.root.path) ? "\\" : "/";
    const selectedDate = dateFilter ? new Date(`${dateFilter}T12:00:00`) : null;

    return (
        <div className="page restore-page">
            <header className="page-header simple">
                <h1 className="page-title">Restore</h1>
                <p className="page-desc">Find the snapshot you need, then browse or restore it.</p>
            </header>

            {repos === null && <div className="inline-notice" role="status"><span className="spinner" /> Loading vaults…</div>}
            {repos !== null && repos.length === 0 && (
                <EmptyState icon="camera" title="No vaults yet">
                    <p><Link to="/protect">Add a vault</Link> before browsing snapshots.</p>
                </EmptyState>
            )}

            {(repos?.length ?? 0) > 0 && (
                <>
                    <div className="restore-controls">
                        <div className="vault-tabs" role="tablist" aria-label="Vaults">
                            {(repos ?? []).map((repo) => (
                                <button key={repo.id} role="tab" aria-selected={selected === repo.id} className={selected === repo.id ? "active" : ""} onClick={() => chooseVault(repo.id)}>{repo.name}</button>
                            ))}
                        </div>
                        {selected && <>
                            <span className="control-divider" />
	                            <button className={`btn date-filter${dateFilter ? " active" : ""}`} onClick={() => setCalendarOpen((open) => !open)}>
                                <Icon name="calendar" size={13} />
                                {selectedDate ? selectedDate.toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" }) : "Filter by date"}
                            </button>
	                            {dateFilter && <button className="text-button clear-filter" onClick={clearDate}>Clear ✕</button>}
							<Tooltip content={SNAPSHOT_HISTORY_TOOLTIP}><button className="btn" aria-label="Refresh snapshot history" disabled={forcingMetadata} onClick={forceRefresh}>
								{forcingMetadata && <span className="spinner" />}Refresh snapshot history
							</button></Tooltip>
	                        </>}
                    </div>

                    {!selected && <EmptyState icon="vault" title="Choose a vault"><p>Select a vault above to browse its snapshots.</p></EmptyState>}

                    {selected && calendarOpen && (
                        <div className="calendar-popover">
                            <div className="calendar-header">
                                <button onClick={() => setCalendarMonth(new Date(calendarMonth.getFullYear(), calendarMonth.getMonth() - 1, 1))} aria-label="Previous month">←</button>
                                <strong>{calendarMonth.toLocaleDateString(undefined, { month: "long", year: "numeric" })}</strong>
                                <button onClick={() => setCalendarMonth(new Date(calendarMonth.getFullYear(), calendarMonth.getMonth() + 1, 1))} aria-label="Next month">→</button>
                            </div>
                            <div className="calendar-weekdays">{["Mo", "Tu", "We", "Th", "Fr", "Sa", "Su"].map((day) => <span key={day}>{day}</span>)}</div>
                            <div className="calendar-grid">
                                {calendarCells.map((day, index) => {
                                    if (day === null) return <span key={`empty-${index}`} />;
                                    const key = dateKey(new Date(calendarMonth.getFullYear(), calendarMonth.getMonth(), day));
                                    const hasSnapshot = availableDays.has(key);
                                    const isSelected = key === dateFilter;
                                    return (
                                        <button key={key} disabled={!hasSnapshot} className={isSelected ? "selected" : ""} onClick={() => pickDate(day)}>
                                            {day}{hasSnapshot && <span />}
                                        </button>
                                    );
                                })}
                            </div>
                            <div className="calendar-legend"><span />days with snapshots</div>
                        </div>
                    )}

			                    {selected && <>
			                        {displayedSnapshots === null && <div className="inline-notice" role="status"><span className="spinner" /> Preparing backup data…</div>}
							{displayedSnapshots !== null && <MetadataIndexNotice active={metadataBusy} paused={metadataPaused} stage={metadataStage} completeHeaderListing={completeHeaderListing} state={metadataState} />}
							{metadataState?.repositoryFailed && <div className="inline-notice" role="status">Backup data could not be refreshed. Existing backup data is still available. <Tooltip content={SNAPSHOT_HISTORY_TOOLTIP}><button className="btn" aria-label="Refresh snapshot history" disabled={forcingMetadata} onClick={forceRefresh}>Refresh snapshot history</button></Tooltip></div>}
							{metadataState?.headerValid && (metadataState.failedSnapshots ?? 0) > 0 && <div className="inline-notice" role="status">Some backup contents could not be indexed. <button className="btn" disabled={metadataBusy} onClick={retryEntries}>Retry</button></div>}
	                        {displayedSnapshots !== null && displayedSnapshots.length > 0 && error && (
	                            <div className="inline-notice" role="status">{error}</div>
	                        )}
	                        {displayedSnapshots !== null && displayedSnapshots.length === 0 && (
                            <EmptyState icon="camera" title={vaultBusy ? "Try again in a minute or two" : "No snapshots here"}>
                                <p>{vaultBusy
                                    ? "Vault is busy with another operation and currently cannot view your snapshots (do not worry, your data is still there)"
                                    : error || "Run a backup job to create the first point in time."}</p>
                            </EmptyState>
                        )}
                        {displayedSnapshots !== null && displayedSnapshots.length > 0 && dateFiltered.length === 0 && (
                            <EmptyState icon="calendar" title="No snapshots on this date"><button className="btn" onClick={clearDate}>Clear date filter</button></EmptyState>
                        )}

                        {displayedSnapshots !== null && <div className="snapshot-groups">
							{managedGroups.map((group) => (<div key={`${group.key}-${safeManagedPage}`}>
                                <section className="snapshot-day">
                                    <header><strong>{friendlyDate(group.snapshot.timestamp)}</strong><span>{fullDate(group.snapshot.timestamp)}</span></header>
                                    <div>
                                        <div className="snapshot-table-header">
											<span>Computer</span>
                                            <span>Time</span>
                                            <span>Source</span>
                                            <span>Size</span>
                                            <span className="snapshot-header-spacer" />
                                        </div>
                                        {group.items.map((snapshot) => (
											<article key={snapshot.id} className="snapshot-row" data-snapshot-id={snapshot.id}>
												<span className="snapshot-machine" title={snapshot.machineLabel || undefined}>{snapshot.machineLabel || "Unknown"}</span>
                                                <time>{snapshotTime(snapshot.timestamp)}</time>
												<span className="snapshot-source">{snapshotSourceLabel(snapshot)}</span>
                                                <span className="snapshot-size">{snapshot.size || "—"}</span>
											<span className="snapshot-actions">{snapshotRoots(snapshot).map((root) => snapshot.nativeRootType === "f"
                                                        ? <Tooltip key={root.nativeRootId} content="This snapshot does not support browse; it only supports restore."><button className="btn sm snapshot-browse-unavailable" aria-label="Browse" aria-disabled="true">Browse</button></Tooltip>
                                                        : <button key={root.nativeRootId} className="btn sm" disabled={!readySnapshotIDs.has(snapshot.id)} onClick={() => openBrowse(snapshot, root)}>Browse{snapshotRoots(snapshot).length > 1 ? ` ${snapshotRootLabel(root)}` : ""}</button>)}<button className="btn sm restore-button" onClick={() => openRestore(snapshot)}>Restore</button><Tooltip content="Delete snapshot"><button className="btn ghost-icon danger-hover" aria-label="Delete" onClick={() => openDelete(snapshot)}><Icon name="trash" size={15} /></button></Tooltip></span>
                                            </article>
                                        ))}
                                    </div>
                                </section>
							</div>))}
							{managedSnapshots.length > 0 && <PaginationControls
								label="managed snapshots"
								total={managedSnapshots.length}
								defaultPageSize={PAGE_SIZE}
								page={safeManagedPage}
								pageSize={pageSize}
								pageSizeOptions={PAGE_SIZE_OPTIONS}
								onPage={setManagedPage}
								onPageSize={changePageSize}
							/>}
							{unmanagedSnapshots.length > 0 && <section className="restore-unmanaged-section" aria-labelledby="restore-unmanaged-heading">
								<div className="find-presentation-heading" id="restore-unmanaged-heading">
									<h2>Snapshots not managed by Replicaro</h2>
									<Tooltip content={unmanagedSnapshotsTooltip}><button type="button" className="icon-btn" aria-label="About snapshots not managed by Replicaro"><Icon name="info" size={16} /></button></Tooltip>
								</div>
								{unmanagedGroups.map((group) => (<div key={`${group.key}-${safeUnmanagedPage}`}>
									<section className="snapshot-day">
										<header><strong>{friendlyDate(group.snapshot.timestamp)}</strong><span>{fullDate(group.snapshot.timestamp)}</span></header>
										<div>
											<div className="snapshot-table-header"><span>Computer</span><span>Time</span><span>Source</span><span>Size</span><span className="snapshot-header-spacer" /></div>
											{group.items.map((snapshot) => <article key={snapshot.id} className="snapshot-row" data-snapshot-id={snapshot.id}>
												<span className="snapshot-machine" title={snapshot.machineLabel || undefined}>{snapshot.machineLabel || "Unknown"}</span><time>{snapshotTime(snapshot.timestamp)}</time><span className="snapshot-source">{snapshotSourceLabel(snapshot)}</span><span className="snapshot-size">{snapshot.size || "—"}</span>
												<span className="snapshot-actions">{snapshotRoots(snapshot).map((root) => snapshot.nativeRootType === "f"
                                                        ? <Tooltip key={root.nativeRootId} content="This snapshot does not support browse; it only supports restore."><button className="btn sm snapshot-browse-unavailable" aria-label="Browse" aria-disabled="true">Browse</button></Tooltip>
                                                        : <button key={root.nativeRootId} className="btn sm" disabled={!readySnapshotIDs.has(snapshot.id)} onClick={() => openBrowse(snapshot, root)}>Browse{snapshotRoots(snapshot).length > 1 ? ` ${snapshotRootLabel(root)}` : ""}</button>)}<button className="btn sm restore-button" onClick={() => openRestore(snapshot)}>Restore</button><Tooltip content="Delete snapshot"><button className="btn ghost-icon danger-hover" aria-label="Delete" onClick={() => openDelete(snapshot)}><Icon name="trash" size={15} /></button></Tooltip></span>
											</article>)}
										</div>
									</section>
								</div>))}
								<PaginationControls label="snapshots not managed by Replicaro" total={unmanagedSnapshots.length} defaultPageSize={PAGE_SIZE} page={safeUnmanagedPage} pageSize={pageSize} pageSizeOptions={PAGE_SIZE_OPTIONS} onPage={setUnmanagedPage} onPageSize={changePageSize} />
							</section>}
                        </div>}
                    </>}
                </>
            )}
			{deleting && <Modal title="Delete snapshot" onClose={dismissDelete}>
				<p>Delete this snapshot from the vault? Deletion cannot be undone.</p>
				<p>Physical space is reclaimed only by later vault maintenance.</p>
				<div className="modal-footer"><button className="btn" disabled={deleteBusy} onClick={dismissDelete}>Cancel</button><button className="btn danger" disabled={deleteBusy} onClick={() => void confirmDelete()}>{deleteBusy && <span className="spinner" />}Delete</button></div>
			</Modal>}

            {browsing && (
                <Modal title="Snapshot" wide onClose={closeBrowse}>
					<div className="snapshot-modal-meta mono">{snapshotMeta(browsing.snapshot)} · {browsing.snapshot.id.slice(0, 12)}</div>
                    <div className="browse-crumbs">
						<button onClick={() => openBrowse(browsing.snapshot, browsing.root, "")}>{displayPath(browsing.root.path) || "/"}</button>
						{crumbs.map((crumb, index) => <span key={`${crumb}-${index}`}>{associatedWindowsBreadcrumb && index === 0 ? "" : browseSeparator}<button onClick={() => openBrowse(browsing.snapshot, browsing.root, crumbs.slice(0, index + 1).join("/"))}>{associatedWindowsBreadcrumb && index === 0 ? `${crumb}:` : crumb}</button></span>)}
                    </div>
                    <div className="file-list">
						{browsePath && <button className="file-up" onClick={() => openBrowse(browsing.snapshot, browsing.root, crumbs.slice(0, -1).join("/"))}><Icon name="arrowUp" size={16} /><span>Up one directory</span></button>}
                        {entries === null && <Loading />}
                        {entries !== null && entries.length === 0 && <p className="muted file-empty">This directory is empty.</p>}
                        {(entries ?? []).map((entry) => {
                            const childPath = browsePath ? `${browsePath}/${entry.name}` : entry.name;
							const nativePath = repos?.find((repo) => repo.id === selected)?.engine === "restic" ? resticNativeAddress(browsing.root.path, childPath) : childPath;
							const displayNative = browsing.root.path === "/" && repos?.find((repo) => repo.id === selected)?.engine === "restic"
								? resticArchiveDisplayPath(nativePath, browsing.snapshot.source) : nativePath;
							const displayName = displayNative !== nativePath && !childPath.includes("/") ? displayNative : entry.name;
                            return (
                                <div key={childPath} title={`${displayNative}\nNative source: ${browsing.root.path}\nNative path: ${nativePath}`} className={`file-row ${entry.isDir ? "folder" : "file"}`}>
                                    <Icon name={entry.isDir ? "folder" : "file"} size={15} />
								{entry.isDir ? <button onClick={() => openBrowse(browsing.snapshot, browsing.root, childPath)}>{displayName}</button> : <span>{displayName}</span>}
								{!entry.isDir && <span>{entry.size}</span>}
								{!entry.isDir && <button className="restore-file" onClick={() => { openRestore(browsing.snapshot, childPath, browsing.root.nativeRootId); closeBrowse(); }}>Restore file</button>}
                                </div>
                            );
                        })}
                    </div>
                    <div className="modal-footer browse-footer">
                        <span className="mono">{entries?.length ?? 0} items{browseRaw ? " · listing loaded" : ""}</span>
						<button className="btn primary" onClick={() => { openRestore(browsing.snapshot); closeBrowse(); }}>Restore entire snapshot</button>
                    </div>
                </Modal>
            )}

            {restoring && (
                <Modal title="Restore snapshot" onClose={dismissRestore}>
					<fieldset className="modal-workflow-fields" disabled={restoreBusy}>
                    <><div className="snapshot-modal-id">{restoring.snapshot.id.slice(0, 12)}</div><div className="snapshot-modal-meta mono">{snapshotMeta(restoring.snapshot)}</div></>
                    <div className="modal-section-label">Restore to</div>
					<label className="field">
						<span>{restoringFileRoot ? "Destination file" : "Destination folder"}</span>
						{restoringFileRoot
							? <input value={restoreTarget} onChange={(event) => setRestoreTarget(event.target.value)} aria-label="Destination file" />
							: <DirectoryField value={restoreTarget} onChange={setRestoreTarget} ariaLabel="Destination folder" />}
						{restoringFileRoot && <small>Enter the full destination path, including the filename.</small>}
                    </label>
                    {(restoreCapability(repos?.find((repo) => repo.id === restoring.repositoryId), engines)?.conflictModes.length ?? 0) > 1 &&
						<label className="field restore-conflict-field"><span>Should existing files in destination be overwritten?</span><select value={conflictMode} onChange={(event) => setConflictMode(event.target.value)}>{restoreCapability(repos?.find((repo) => repo.id === restoring.repositoryId), engines)?.conflictModes.map((mode) => <option key={mode.id} value={mode.id}>{restoreConflictModeLabel(mode)}</option>)}</select></label>}
					{restoreBusy && repos?.find((repo) => repo.id === restoring.repositoryId)?.coldStorage && <p className="recovery-warning">Cold storage retrieval can take hours or days. Replicaro is waiting for native Restic. Cancellation or shutdown stops the local process, but provider restore requests may continue.</p>}
					</fieldset>
					<div className="modal-footer"><button className="btn" disabled={restoreBusy && !repos?.find((repo) => repo.id === restoring.repositoryId)?.coldStorage} onClick={restoreBusy && repos?.find((repo) => repo.id === restoring.repositoryId)?.coldStorage ? cancelColdRestore : dismissRestore}>{restoreBusy && repos?.find((repo) => repo.id === restoring.repositoryId)?.coldStorage ? "Cancel restore" : "Cancel"}</button><button className="btn primary" disabled={restoreBusy || !restoreTarget} onClick={() => void doRestore()}>{restoreBusy && <span className="spinner" />}<Icon name="restore" size={14} />Restore</button></div>
                </Modal>
            )}
        </div>
    );
}
