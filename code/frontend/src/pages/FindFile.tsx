import { formatDisplayDateTime, renderMessage, t } from "../i18n";
import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import type { CSSProperties, ReactNode } from "react";
import { createPortal } from "react-dom";
import { useNavigate, useParams } from "react-router-dom";

import { DirectoryField } from "../components/DirectoryPicker";
import { MetadataIndexNotice } from "../components/MetadataIndexNotice";
import { EmptyState, Icon, Loading, Modal, parseTime, Tooltip, useToast } from "../components/ui";
import {
	APIError,
	browseFiles,
	getFileHistory,
	getMetadataStatus,
	getOperation,
    getEngines,
    getRepositories,
    listSnapshots,
	prepareMetadata,
	observeSubmittedRestoreOperation,
	isOperationNotFound,
	requireExactOperation,
	restoreSelection,
    searchFiles,
} from "../services/api";
import type { MetadataPreparationStatus } from "../services/api";
import { defaultConflictMode, restoreCapability, restoreConflictModeLabel } from "../restoreCapabilities";
import type { EngineDescriptor, FileBrowseEntry, FileSearchResult, FileVersion, MetadataIndexState, Repository, Snapshot } from "../types";

type RestoreMode = "single" | "individual";
type HistoryMap = Record<string, FileVersion[]>;
type VersionMap = Record<string, string>;
type PageState = { hasMore: boolean; nextOffset: number; loading: boolean; revision: number };
type BrowseItemsMap = Record<string, FileBrowseEntry[]>;
type BrowsePageMap = Record<string, PageState>;
type BrowseErrorMap = Record<string, string>;
type SearchCommand = { sequence: number; query: string };
type VaultViewStatus = { indexBlocked: boolean; searching: boolean; forcingMetadata: boolean };
type ReviewedRestoreItem = { snapshotId: string; path: string; nativeRootId: string };
type ReviewedRestore = {
	repositoryId: string;
	targetPath: string;
	items: ReviewedRestoreItem[];
	conflictMode: string;
	mode: RestoreMode;
	rows: Array<{ key: string; path: string; isDir: boolean; timestamp: string }>;
};

// File History is one current-profile catalog. Managed and visible unmanaged
// snapshots share native root/path identity; ownership remains a Restore concern.
const emptyPageState = (): PageState => ({ hasMore: false, nextOffset: 0, loading: false, revision: 0 });
const browseRootKey = "\u0000browse-root";
const allVaultsValue = "__all_vaults__";
const SNAPSHOT_HISTORY_TOOLTIP = () => t("ui.snapshotHistory.refreshHelp");

function resultKey(result: FileSearchResult) {
	return JSON.stringify([result.source, result.path]);
}

function resultIdentityKey(result: FileSearchResult) {
	return `${result.source}\u0000${result.path}`;
}

function browseNodeKey(result: FileBrowseEntry) {
	return result.isSource
		? `browse-source\u0000${result.source}`
		: `browse-item\u0000${resultKey(result)}`;
}

function canonicalSelectionPath(result: FileSearchResult) {
	return result.path.replace(/^\/+|\/+$/g, "");
}

function selectionSourceIdentity(result: FileSearchResult) {
	return result.source;
}

function isSelectionDescendant(result: FileSearchResult, ancestor: FileSearchResult) {
	if (selectionSourceIdentity(result) !== selectionSourceIdentity(ancestor)) return false;
	const resultPath = canonicalSelectionPath(result);
	const ancestorPath = canonicalSelectionPath(ancestor);
	return resultPath.startsWith(`${ancestorPath}/`);
}

function selectionConflict(results: FileSearchResult[]) {
	for (let left = 0; left < results.length; left++) {
		const leftPath = canonicalSelectionPath(results[left]);
		for (let right = left + 1; right < results.length; right++) {
			const rightPath = canonicalSelectionPath(results[right]);
			if (leftPath === rightPath) return t("ui.findFile.sameRestorePathConflict");
			if (leftPath.startsWith(`${rightPath}/`) || rightPath.startsWith(`${leftPath}/`)) {
				return t("ui.findFile.nestedRestorePathConflict");
			}
		}
	}
	return "";
}

function resultsOverlap(left: FileSearchResult, right: FileSearchResult) {
	if (selectionSourceIdentity(left) !== selectionSourceIdentity(right)) return false;
	const leftPath = canonicalSelectionPath(left);
	const rightPath = canonicalSelectionPath(right);
	return leftPath === rightPath || leftPath.startsWith(`${rightPath}/`) || rightPath.startsWith(`${leftPath}/`);
}

function appendUniqueResults(current: FileSearchResult[], next: FileSearchResult[]) {
	const keys = new Set(current.map(resultIdentityKey));
	return [...current, ...next.filter((item) => {
		const key = resultIdentityKey(item);
		if (keys.has(key)) return false;
		keys.add(key);
		return true;
	})];
}

function appendUniqueHistory(current: FileVersion[], next: FileVersion[]) {
	const ids = new Set(current.map(fileVersionKey));
	return [...current, ...next.filter((item) => {
		const key = fileVersionKey(item);
		if (ids.has(key)) return false;
		ids.add(key);
		return true;
	})];
}

function fileVersionKey(version: Pick<FileVersion, "snapshotId" | "nativeRootId">) {
	return JSON.stringify([version.snapshotId, version.nativeRootId]);
}

function fileVersionForKey(history: FileVersion[] | undefined, key: string) {
	return history?.find((version) => fileVersionKey(version) === key);
}

function exactSnapshotVersion(history: FileVersion[] | undefined, snapshotId: string) {
	const matches = presentVersions(history).filter((version) => version.snapshotId === snapshotId);
	return matches.length === 1 ? matches[0] : undefined;
}

// Filesystem destinations and native source headers retain their spelling.
// Relative archive names are slash-delimited regardless of the viewing OS.
function displayPath(value: string) { return value; }
function nativeRootLabel(path: string) { return /^[A-Za-z]:[\\/]/.test(path) ? path.replace(/\//g, "\\") : path; }
function displayFilePath(value: string, source = "") {
	const windowsSource = /^[A-Za-z]:[\\/]/.test(source) || source.startsWith("\\\\");
	return windowsSource && !value.includes("\\") ? `\\${value.replace(/\//g, "\\")}` : value ? `/${value}` : "/";
}

function displayFullFilePath(source: string, value: string) {
	if (!source) return displayFilePath(value);
	// A source header establishes Windows presentation only for descendants
	// without literal backslashes. A /C tree component alone proves nothing.
	const windowsSource = /^[A-Za-z]:[\\/]/.test(source) || source.startsWith("\\\\");
	if (windowsSource && !value.includes("\\")) {
		const root = source.replace(/\//g, "\\").replace(/\\$/, "");
		return value ? `${root}\\${value.replace(/\//g, "\\")}` : source;
	}
	return value ? `${source.replace(/\/$/, "")}/${value}` : source;
}

function splitDisplayPath(value: string, windows = false) {
	const separator = value.lastIndexOf(windows ? "\\" : "/");
	return separator < 0 ? { parent: "", name: value }
		: { parent: value.slice(0, separator + 1), name: value.slice(separator + 1) };
}

function FilePathLabel({ value, source, tooltipPath }: { value: string; source?: string; tooltipPath?: string }) {
    const path = displayFilePath(value, source);
    const parts = splitDisplayPath(path, path.startsWith("\\") && !value.includes("\\"));
    const id = useId();
    const triggerRef = useRef<HTMLSpanElement>(null);
    const tooltipRef = useRef<HTMLSpanElement>(null);
    const [tooltipOpen, setTooltipOpen] = useState(false);
    const [tooltipPosition, setTooltipPosition] = useState<{ left: number; top: number }>();

    useEffect(() => {
        if (!tooltipPath || !tooltipOpen) return;
        const positionTooltip = () => {
            const trigger = triggerRef.current;
            const tooltip = tooltipRef.current;
            if (!trigger || !tooltip) return;
            const triggerBox = trigger.getBoundingClientRect();
            const tooltipBox = tooltip.getBoundingClientRect();
            const left = Math.max(16, Math.min(triggerBox.left, window.innerWidth - tooltipBox.width - 16));
            const below = triggerBox.bottom + 8;
            const top = below + tooltipBox.height <= window.innerHeight - 16
                ? below
                : Math.max(16, triggerBox.top - tooltipBox.height - 8);
            setTooltipPosition({ left, top });
        };
        positionTooltip();
        window.addEventListener("resize", positionTooltip);
        document.addEventListener("scroll", positionTooltip, true);
        return () => {
            window.removeEventListener("resize", positionTooltip);
            document.removeEventListener("scroll", positionTooltip, true);
        };
    }, [tooltipOpen, tooltipPath]);

    const label = <span className="file-path-label"><span className="file-path-parent">{parts.parent}</span><span className="file-path-name">{parts.name}</span></span>;
    if (tooltipPath) return (
        <span
            ref={triggerRef}
            className="file-path-label tooltip-trigger"
            tabIndex={0}
            aria-label={tooltipPath}
            title={`Native source: ${source ?? ""}\nNative path: ${value}`}
            aria-describedby={id}
            onMouseEnter={() => { setTooltipPosition(undefined); setTooltipOpen(true); }}
            onMouseLeave={() => setTooltipOpen(false)}
            onFocus={() => { setTooltipPosition(undefined); setTooltipOpen(true); }}
            onBlur={() => setTooltipOpen(false)}
        >
            <span className="file-path-parent">{parts.parent}</span><span className="file-path-name">{parts.name}</span>
            {createPortal(
                <span
                    ref={tooltipRef}
                    id={id}
                    className={`tooltip-popup find-full-path-tooltip${tooltipOpen ? " open" : ""}`}
                    role="tooltip"
                    style={tooltipOpen && tooltipPosition
                        ? { left: tooltipPosition.left, top: tooltipPosition.top, visibility: "visible" }
                        : { visibility: "hidden" }}
                >{tooltipPath}</span>,
                document.body,
            )}
        </span>
    );
    return (
        <Tooltip content={path}>
            {label}
        </Tooltip>
    );
}

function SelectionCheckbox({
	checked,
	label,
	onChange,
	parentSelectionMessage,
}: {
	checked: boolean;
	label: string;
	onChange: () => void;
	parentSelectionMessage?: string;
}) {
	const checkbox = <input
		type="checkbox"
		checked={checked}
		disabled={Boolean(parentSelectionMessage)}
		aria-label={parentSelectionMessage ? `${label}. ${parentSelectionMessage}` : label}
		onChange={onChange}
		style={parentSelectionMessage ? { width: 16, height: 16, padding: 0, accentColor: "var(--accent)" } : undefined}
	/>;
	if (!parentSelectionMessage) return checkbox;
	return <Tooltip content={parentSelectionMessage}><span style={{ display: "inline-grid", width: 16, height: 16, placeItems: "center" }}>{checkbox}</span></Tooltip>;
}

function formatDate(value: string) {
    const date = parseTime(value);
    return date ? formatDisplayDateTime(date, {
        month: "short",
        day: "numeric",
        year: "numeric",
        hour: "2-digit",
        minute: "2-digit",
    }) : t("ui.date.unknownDate");
}

function sourceLabel(result: FileSearchResult) {
	return nativeRootLabel(result.source || t("ui.findFile.unknownSource"));
}

function presentVersions(history: FileVersion[] | undefined) {
    return (history ?? []).filter((version) => version.present);
}

function newestPresentVersion(history: FileVersion[] | undefined) {
	return presentVersions(history).reduce<FileVersion | undefined>((newest, version) => {
		if (!newest) return version;
		if (version.timestamp !== newest.timestamp) return version.timestamp > newest.timestamp ? version : newest;
		if (version.snapshotId !== newest.snapshotId) return version.snapshotId > newest.snapshotId ? version : newest;
		return version.nativeRootId < newest.nativeRootId ? version : newest;
	}, undefined);
}

// Keep restore choosers flat and newest-first. Show only the date and
// available machine label. Do not add computer grouping or snapshot ID,
// type, size, or root-path details to the chooser presentation; option
// values retain exact snapshot/root identities.
function newestVersionFirst(left: FileVersion, right: FileVersion) {
	if (left.timestamp !== right.timestamp) return left.timestamp > right.timestamp ? -1 : 1;
	if (left.snapshotId !== right.snapshotId) return left.snapshotId > right.snapshotId ? -1 : 1;
	return left.nativeRootId === right.nativeRootId ? 0 : left.nativeRootId < right.nativeRootId ? -1 : 1;
}

function newestSnapshotFirst(left: Snapshot, right: Snapshot) {
	if (left.timestamp !== right.timestamp) return left.timestamp > right.timestamp ? -1 : 1;
	return left.id === right.id ? 0 : left.id > right.id ? -1 : 1;
}

function versionChoiceLabel(version: FileVersion) {
	return `${formatDate(version.timestamp)}${version.machineLabel ? ` · ${version.machineLabel}` : ""}`;
}

function sharedSnapshotMachineLabel(snapshotId: string, results: FileSearchResult[], histories: HistoryMap) {
	const labels = new Set(results.flatMap((result) =>
		presentVersions(histories[resultKey(result)])
			.filter((version) => version.snapshotId === snapshotId && version.machineLabel)
			.map((version) => version.machineLabel as string)
	));
	return labels.size === 1 ? labels.values().next().value ?? "" : "";
}

function versionTypeLabel(version: FileVersion | undefined, fallbackIsDir: boolean) {
	return (version?.isDir ?? fallbackIsDir) ? t("ui.findFile.folderType") : t("ui.findFile.fileType");
}

function isMetadataRevisionError(reason: unknown) {
	return typeof reason === "object" && reason !== null && "code" in reason &&
		(reason as { code?: string }).code === "metadata_revision_changed";
}

export default function FindFile() {
	const { repoId = "" } = useParams();
	return <FindFilePage key={repoId} repoId={repoId} />;
}

function FindFilePage({ repoId }: { repoId: string }) {
	const toast = useToast();
	const [repos, setRepos] = useState<Repository[] | null>(null);
	const [engines, setEngines] = useState<EngineDescriptor[]>([]);
	const [selected, setSelected] = useState("");
	const [query, setQuery] = useState("");
	const [searchCommand, setSearchCommand] = useState<SearchCommand | null>(null);
	const [forceRefreshSequence, setForceRefreshSequence] = useState(0);
	const [vaultStatuses, setVaultStatuses] = useState<Record<string, VaultViewStatus>>({});
	const [error, setError] = useState("");
	const [singleSearchTarget, setSingleSearchTarget] = useState<HTMLDivElement | null>(null);
	const [allMatchesTarget, setAllMatchesTarget] = useState<HTMLDivElement | null>(null);
	const [allBrowseTarget, setAllBrowseTarget] = useState<HTMLDivElement | null>(null);
	const searchInput = useRef<HTMLInputElement | null>(null);

	useEffect(() => {
		let active = true;
		Promise.all([getRepositories(), getEngines()])
			.then(([nextRepos, engineResponse]) => {
				if (!active) return;
				setRepos(nextRepos);
				setEngines(engineResponse.engines);
				const routeRepo = nextRepos.find((repo) => repo.id === repoId);
				setSelected(routeRepo?.id ?? "");
			})
			.catch((reason: Error) => {
				if (!active) return;
				setRepos([]);
				setError(reason.message);
			});
		return () => { active = false; };
	}, [repoId]);

	const chooseVault = (id: string) => {
		setSelected(id);
		setSearchCommand(null);
		setForceRefreshSequence(0);
		setVaultStatuses({});
		setError("");
	};

	const submitSearch = () => {
		const nextQuery = query.trim();
		if (nextQuery.length < 2) {
			toast("error", t("ui.findFile.enterSearchTerm"));
			return;
		}
		setSearchCommand((current) => ({ sequence: (current?.sequence ?? 0) + 1, query: nextQuery }));
	};
	const reportVaultStatus = useCallback((repositoryId: string, status: VaultViewStatus) => {
		setVaultStatuses((current) => {
			const previous = current[repositoryId];
			if (previous?.indexBlocked === status.indexBlocked && previous.searching === status.searching && previous.forcingMetadata === status.forcingMetadata) return current;
			return { ...current, [repositoryId]: status };
		});
	}, []);

	const vaultSelect = repos ? <select aria-label={t("ui.pages.findfile.vault")} value={selected} onChange={(event) => chooseVault(event.target.value)}>
		<option value="">{t("ui.pages.findfile.choose.a.vault")}</option>
		<option value={allVaultsValue}>{t("ui.pages.findfile.all.vaults")}</option>
		{repos.map((repo) => <option key={repo.id} value={repo.id}>{repo.name}</option>)}
	</select> : null;
	const selectedStatus = selected && selected !== allVaultsValue ? vaultStatuses[selected] : undefined;
	const allSearching = selected === allVaultsValue && Object.values(vaultStatuses).some((status) => status.searching);
	const searchDisabled = !selected || selected !== allVaultsValue && (selectedStatus?.indexBlocked ?? true);
	const searchBusy = selected === allVaultsValue ? allSearching : Boolean(selectedStatus?.searching);

	return (
		<div className="page find-file-page">
			<header className="page-header simple">
				<h1 className="page-title">{t("ui.pages.findfile.file.history")}</h1>
				<p className="page-desc">{t("ui.pages.findfile.view.file.and.folder.history.across.a.whole.vault.then.restore.selecte")}</p>
			</header>

			{repos === null ? (
				<div className="inline-notice" role="status"><span className="spinner" /> {t("ui.pages.findfile.loading.vaults")}</div>
			) : repos.length === 0 ? (
				<EmptyState icon="vault" title={t("ui.pages.findfile.no.vaults.yet")}><p>{renderMessage("ui.fileHistory.addVaultBeforeSearch", { link: <a href="/protect">{t("ui.pages.findfile.add.a.vault")}</a> })}</p></EmptyState>
			) : (
				<>
					<section className="find-history-section find-search-section" aria-labelledby="search-vault-heading">
						<div className="section-heading find-search-heading"><div><h2 id="search-vault-heading">{t("ui.pages.findfile.search.vault")}</h2></div></div>
						<div className="find-file-toolbar">
							<div className="field find-vault"><span>{t("ui.pages.findfile.vault")}</span>{vaultSelect}{selected && selected !== allVaultsValue && <Tooltip content={SNAPSHOT_HISTORY_TOOLTIP()}><button className="metadata-force-link" aria-label={t("ui.pages.findfile.refresh.snapshot.history")} disabled={selectedStatus?.forcingMetadata} onClick={() => setForceRefreshSequence((value) => value + 1)}>{selectedStatus?.forcingMetadata && <span className="spinner" />}{t("ui.pages.findfile.refresh.snapshot.history")}</button></Tooltip>}</div>
							<label className="field find-search"><span>{t("ui.pages.findfile.file.or.folder.name")}</span><div className="find-search-input"><input ref={searchInput} value={query} placeholder={selected ? t("ui.fileHistory.searchPlaceholder") : t("ui.fileHistory.selectVaultPlaceholder")} disabled={searchDisabled} onChange={(event) => setQuery(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter") submitSearch(); }} /><button className="btn primary" onClick={submitSearch} disabled={searchDisabled || searchBusy}>{searchBusy ? <span className="spinner" /> : <Icon name="history" size={14} />}{t("ui.pages.findfile.search")}</button></div><small>{t("ui.pages.findfile.use.for.any.characters.or.for.one.character")}</small></label>
						</div>
						{error && <div className="inline-error">{error}</div>}
						{selected && selected !== allVaultsValue && <div className="find-single-vault-search-target" ref={setSingleSearchTarget} />}
					</section>
					{selected === allVaultsValue ? <div className="find-all-vaults-history">
						<div className="find-all-vault-matches-group" ref={setAllMatchesTarget} />
						<div className="find-all-vault-browse-group" ref={setAllBrowseTarget} />
						{allMatchesTarget && allBrowseTarget && repos.map((repository) => <VaultFileHistory
						key={repository.id}
						repository={repository}
						engines={engines}
						query={query}
						searchCommand={searchCommand}
						allVaults
						searchTarget={searchCommand ? allMatchesTarget : undefined}
						browseTarget={searchCommand ? allBrowseTarget : undefined}
						onFocusSearch={() => searchInput.current?.focus()}
						onStatusChange={reportVaultStatus}
					/>)}</div> : selected && singleSearchTarget ? <VaultFileHistory
						key={selected}
						repository={repos.find((repository) => repository.id === selected)!}
						engines={engines}
						query={query}
						searchCommand={searchCommand}
						forceRefreshSequence={forceRefreshSequence}
						searchTarget={singleSearchTarget}
						onFocusSearch={() => searchInput.current?.focus()}
						onStatusChange={reportVaultStatus}
					/> : !selected ? <section className="find-history-section find-browse-section" aria-labelledby="browse-vault-heading">
						<div className="section-heading"><div><h2 id="browse-vault-heading">{t("ui.pages.findfile.browse.vault")}</h2></div></div>
						<EmptyState icon="history" title={t("ui.pages.findfile.choose.a.vault")}><p>{t("ui.pages.findfile.select.a.vault.above.to.browse.file.and.folder.history")}</p></EmptyState>
					</section> : null}
				</>
			)}
		</div>
	);
}

function PortalSurface({ target, children }: { target?: Element; children: ReactNode }) {
	return target ? createPortal(children, target) : children;
}

function VaultFileHistory({
	repository,
	engines,
	query,
	searchCommand,
	forceRefreshSequence = 0,
	allVaults = false,
	searchTarget,
	browseTarget,
	onFocusSearch,
	onStatusChange,
}: {
	repository: Repository;
	engines: EngineDescriptor[];
	query: string;
	searchCommand?: SearchCommand | null;
	forceRefreshSequence?: number;
	allVaults?: boolean;
	searchTarget?: Element;
	browseTarget?: Element;
	onFocusSearch?: () => void;
	onStatusChange: (repositoryId: string, status: VaultViewStatus) => void;
}) {
    const toast = useToast();
	const navigate = useNavigate();
	const sectionId = useId();
	const selected = repository.id;
    const [snapshots, setSnapshots] = useState<Snapshot[]>([]);
    const [searchedQuery, setSearchedQuery] = useState("");
    const [results, setResults] = useState<FileSearchResult[] | null>(null);
    const [selectedItems, setSelectedItems] = useState<Record<string, FileSearchResult>>({});
    const [histories, setHistories] = useState<HistoryMap>({});
    const [chosenVersions, setChosenVersions] = useState<VersionMap>({});
    const [mode, setMode] = useState<RestoreMode>("single");
    const [singleSnapshot, setSingleSnapshot] = useState("");
    const [targetPath, setTargetPath] = useState("");
    const [searching, setSearching] = useState(false);
	const [restoring, setRestoring] = useState(false);
    const [reviewedRestore, setReviewedRestore] = useState<ReviewedRestore | null>(null);
    const [error, setError] = useState("");
    const [indexing, setIndexing] = useState(false);
	const [indexState, setIndexState] = useState<MetadataIndexState | null>(null);
	const [metadataPaused, setMetadataPaused] = useState(false);
	const [metadataStage, setMetadataStage] = useState<MetadataPreparationStatus["stage"]>("");
	const [completeHeaderListing, setCompleteHeaderListing] = useState(false);
	const [retryingIndex, setRetryingIndex] = useState(false);
	const [forcingMetadata, setForcingMetadata] = useState(false);
	const [preparationSequence, setPreparationSequence] = useState(0);
	const [searchPage, setSearchPage] = useState<PageState>(emptyPageState);
	const [matchesVisible, setMatchesVisible] = useState(false);
	const [browseItems, setBrowseItems] = useState<BrowseItemsMap>({});
	const [browsePages, setBrowsePages] = useState<BrowsePageMap>({});
	const [expandedBrowseKeys, setExpandedBrowseKeys] = useState<string[]>([]);
	const [browseErrors, setBrowseErrors] = useState<BrowseErrorMap>({});
    const generation = useRef(0);
    const searchGeneration = useRef(0);
    const snapshotRequest = useRef<AbortController | null>(null);
    const searchRequest = useRef<AbortController | null>(null);
    const historyRequests = useRef(new Map<string, AbortController>());
	const browseRequests = useRef(new Map<string, AbortController>());
	const handledSearchSequence = useRef(0);
	const handledForceRefreshSequence = useRef(0);
	const restoreReviewSession = useRef(0);
	const restoreSubmissionGeneration = useRef(0);
	const restoreSubmissionOwner = useRef<{ session: number; generation: number; requestPending: boolean; handedOff: boolean; observationController: AbortController; controller?: AbortController } | null>(null);
	const preparationAction = useRef<"access" | "retry" | "force">("access");
	const preparationRun = useRef(0);
	const preparationScheduledOrActive = useRef(false);
	const pendingDirtyGeneration = useRef(0);
	const scheduleDirtyGenerationRecovery = useCallback((requiredGeneration: number) => {
		pendingDirtyGeneration.current = Math.max(pendingDirtyGeneration.current, requiredGeneration);
		if (!selected || preparationScheduledOrActive.current) return;
		preparationScheduledOrActive.current = true;
		preparationAction.current = "access";
		setPreparationSequence((value) => value + 1);
	}, [selected]);
	const acceptCatalogResponse = useCallback((response: { index: MetadataIndexState; indexing: boolean }, recoverDirtyGeneration = false) => {
		setIndexState(response.index);
		if (response.index.complete) return true;
		// Generation equality is the publication boundary. A response from a
		// newly dirty generation invalidates already-rendered catalog data too.
		setIndexing(Boolean(response.indexing));
		setResults(null);
		setMatchesVisible(false);
		setBrowseItems({});
		setSelectedItems({});
		setHistories({});
		setChosenVersions({});
		const generationIsDirty = !response.index.repositoryFailed && (response.index.failedSnapshots ?? 0) === 0 &&
			typeof response.index.requiredGeneration === "number" &&
			typeof response.index.appliedGeneration === "number" &&
			response.index.requiredGeneration !== response.index.appliedGeneration;
		if (recoverDirtyGeneration && generationIsDirty) scheduleDirtyGenerationRecovery(response.index.requiredGeneration!);
		return false;
	}, [scheduleDirtyGenerationRecovery]);

	useEffect(() => {
		if (!selected) return;
		const controller = new AbortController();
		const run = ++preparationRun.current;
		preparationScheduledOrActive.current = true;
		const vaultGeneration = generation.current;
		const action = preparationAction.current;
		let dirtyGenerationCoveredByRun = pendingDirtyGeneration.current;
		preparationAction.current = "access";
		setIndexing(true);
		setMetadataPaused(false);
		setMetadataStage("queued");
		setCompleteHeaderListing(action === "force");
		setResults(null);
		setMatchesVisible(false);
		setSelectedItems({});
		setHistories({});
		setChosenVersions({});
		setBrowseItems({});
		setBrowsePages({ [browseRootKey]: emptyPageState() });
		void (async () => {
			try {
				let status = await prepareMetadata(selected, action);
				let catalogAccepted = false;
				let retryAccessPrepared = false;
				while (!controller.signal.aborted) {
					while (!controller.signal.aborted) {
						if (generation.current !== vaultGeneration) return;
						setIndexState(status.index);
						setIndexing(status.running || status.pending);
						setMetadataPaused(Boolean(status.paused));
						setMetadataStage(status.stage);
						setCompleteHeaderListing(status.completeHeaderListing);
						if (!status.running && !status.pending) break;
						await new Promise((resolve) => window.setTimeout(resolve, 250));
						status = await getMetadataStatus(selected, controller.signal);
					}
					const retryLeftDirtyGeneration = action === "retry" && !retryAccessPrepared &&
						!status.index.repositoryFailed && (status.index.failedSnapshots ?? 0) === 0 &&
						typeof status.index.requiredGeneration === "number" &&
						typeof status.index.appliedGeneration === "number" &&
						status.index.requiredGeneration !== status.index.appliedGeneration;
					if (!controller.signal.aborted && generation.current === vaultGeneration && retryLeftDirtyGeneration) {
						// Retry repairs failed entries; one ordinary access preparation then
						// gives the authoritative header path a chance to certify the still-
						// dirty generation. Access retains its normal persisted cooldown.
						retryAccessPrepared = true;
						status = await prepareMetadata(selected, "access");
						continue;
					}
					if (controller.signal.aborted || generation.current !== vaultGeneration) break;
					if (!status.index.complete) {
						const pendingGeneration = pendingDirtyGeneration.current;
						const canFollowDirtyGeneration = pendingGeneration > dirtyGenerationCoveredByRun &&
							!status.index.repositoryFailed && (status.index.failedSnapshots ?? 0) === 0;
						if (!canFollowDirtyGeneration) break;
						dirtyGenerationCoveredByRun = pendingGeneration;
						status = await prepareMetadata(selected, "access");
						continue;
					}
					const [nextSnapshots, catalogRoot] = await Promise.all([
						listSnapshots(selected, controller.signal),
						browseFiles(selected, "", "", 0, controller.signal),
					]);
					if (controller.signal.aborted || generation.current !== vaultGeneration) return;
					const pendingGeneration = pendingDirtyGeneration.current;
					const appliedGeneration = Math.max(
						status.index.appliedGeneration ?? 0,
						catalogRoot.index.appliedGeneration ?? 0,
					);
					const currentRunCoversPending = pendingGeneration === 0 ||
						pendingGeneration <= dirtyGenerationCoveredByRun || appliedGeneration >= pendingGeneration;
					if (catalogRoot.index.complete && !currentRunCoversPending) {
						// A catalog request can observe a newer dirty generation while an
						// older access, Retry, or Force preparation is still completing.
						// Retain that observation and let the same lifecycle perform one
						// ordinary access follow-up instead of dropping it at the active-run gate.
						dirtyGenerationCoveredByRun = pendingGeneration;
						setIndexing(true);
						setMetadataStage("queued");
						status = await prepareMetadata(selected, "access");
						continue;
					}
					if (!acceptCatalogResponse(catalogRoot)) {
						// Status can become complete before the root read observes a newer
						// dirty generation. Re-enter the existing access preparation so the
						// catalog recovers without requiring a vault reselection.
						dirtyGenerationCoveredByRun = Math.max(dirtyGenerationCoveredByRun, pendingDirtyGeneration.current);
						status = await prepareMetadata(selected, "access");
						if (!status.running && !status.pending && !status.index.complete) break;
						continue;
					}
					setSnapshots(nextSnapshots);
					setBrowseItems((current) => ({
						...current,
						[browseRootKey]: catalogRoot.items,
					}));
					setBrowsePages((current) => ({
						...current,
						[browseRootKey]: { hasMore: catalogRoot.hasMore, nextOffset: catalogRoot.nextOffset, loading: false, revision: catalogRoot.revision },
					}));
					if (currentRunCoversPending) pendingDirtyGeneration.current = 0;
					catalogAccepted = true;
					break;
				}
				if (action === "force") {
					const succeeded = !status.index.repositoryFailed && status.index.complete && catalogAccepted;
					toast(succeeded ? "ok" : "error", succeeded ? "Snapshot history refreshed." : "Backup data refresh failed.");
				}
			} catch (reason) {
				if (!controller.signal.aborted && generation.current === vaultGeneration) setError((reason as Error).message);
			} finally {
				if (preparationRun.current === run) preparationScheduledOrActive.current = false;
				if (!controller.signal.aborted && generation.current === vaultGeneration) {
					setIndexing(false);
					setMetadataPaused(false);
					setRetryingIndex(false);
					setForcingMetadata(false);
				}
			}
		})();
		return () => controller.abort();
	}, [acceptCatalogResponse, preparationSequence, selected, toast]);

	useEffect(() => () => {
		generation.current++;
		restoreReviewSession.current++;
		restoreSubmissionGeneration.current++;
		restoreSubmissionOwner.current?.observationController.abort();
		restoreSubmissionOwner.current = null;
        snapshotRequest.current?.abort();
		searchRequest.current?.abort();
        historyRequests.current.forEach((controller) => controller.abort());
		browseRequests.current.forEach((controller) => controller.abort());
    }, []);

    const selectedResults = useMemo(
		() => Object.values(selectedItems),
		[selectedItems]
	);
	const selectedRepository = repository;
	const indexBlocked = Boolean(selected) && (indexing || indexState === null || !indexState.complete);
	useEffect(() => {
		onStatusChange(repository.id, { indexBlocked, searching, forcingMetadata });
	}, [forcingMetadata, indexBlocked, onStatusChange, repository.id, searching]);

	const commonSnapshots = useMemo(() => {
		if (!selectedResults.length) return [];
		return snapshots.filter((snapshot) =>
			selectedResults.every((result) =>
					presentVersions(histories[resultKey(result)]).filter((version) => version.snapshotId === snapshot.id).length === 1
			)
		).sort(newestSnapshotFirst);
	}, [histories, selectedResults, snapshots]);

    const allHistoriesLoaded = selectedResults.every((result) => histories[resultKey(result)] !== undefined);
	const allIndividualVersionsChosen = selectedResults.every((result) =>
		Boolean(fileVersionForKey(histories[resultKey(result)], chosenVersions[resultKey(result)]))
	);
	const restoreSelectionConflict = selectionConflict(selectedResults);
    const effectiveSingleSnapshot = commonSnapshots.some((snapshot) => snapshot.id === singleSnapshot)
        ? singleSnapshot
        : commonSnapshots[0]?.id ?? "";
	// Keep the reviewed filesystem route through confirmation and submission;
	// trimming or host normalization would redirect restore writes.
	const effectiveTargetPath = targetPath;
	const restoreItems: ReviewedRestoreItem[] = mode === "single"
		? selectedResults.map((result) => {
			const version = exactSnapshotVersion(histories[resultKey(result)], effectiveSingleSnapshot);
			return { snapshotId: version?.snapshotId ?? "", path: result.path, nativeRootId: version?.nativeRootId ?? "" };
		})
		: selectedResults.map((result) => {
			const version = fileVersionForKey(histories[resultKey(result)], chosenVersions[resultKey(result)]);
			return { snapshotId: version?.snapshotId ?? "", path: result.path, nativeRootId: version?.nativeRootId ?? "" };
		});
	const canRestore = selectedResults.length > 0 && !restoreSelectionConflict && Boolean(effectiveTargetPath) && allHistoriesLoaded &&
		restoreItems.every((item) => Boolean(item.nativeRootId)) &&
		(mode === "single" ? Boolean(effectiveSingleSnapshot) : allIndividualVersionsChosen);
	const selectedVersion = (result: FileSearchResult) => {
		const key = resultKey(result);
		return mode === "single"
			? exactSnapshotVersion(histories[key], effectiveSingleSnapshot)
			: fileVersionForKey(histories[key], chosenVersions[key]);
	};
	const selectedFolderAncestor = (result: FileSearchResult) => selectedResults
		.filter((candidate) => (selectedVersion(candidate)?.isDir ?? candidate.isDir) && isSelectionDescendant(result, candidate))
		.sort((left, right) => canonicalSelectionPath(right).length - canonicalSelectionPath(left).length)[0];
	const parentSelectionMessage = (result: FileSearchResult) => selectedFolderAncestor(result)
		? result.isDir ? t("ui.findFile.childFolderRestoredWithParent") : t("ui.findFile.childFileRestoredWithParent")
		: "";

	const performSearch = useCallback(async (nextQuery: string, commandSequence = 0) => {
		searchRequest.current?.abort();
		const controller = new AbortController();
		searchRequest.current = controller;
		const requestGeneration = ++searchGeneration.current;
		const vaultGeneration = generation.current;
		setSearching(true);
		setError("");
		setSearchPage(emptyPageState());
		setMatchesVisible(true);
		try {
			const response = await searchFiles(selected, nextQuery, 0, controller.signal);
			if (generation.current !== vaultGeneration || searchGeneration.current !== requestGeneration) return;
				if (!acceptCatalogResponse(response, true)) {
					if (allVaults && commandSequence > 0 && handledSearchSequence.current === commandSequence) {
						handledSearchSequence.current = commandSequence - 1;
					}
					return;
				}
			setResults(response.items);
			setSearchedQuery(nextQuery);
			setSearchPage({ hasMore: response.hasMore, nextOffset: response.nextOffset, loading: false, revision: response.revision });
		} catch (reason) {
			if ((reason as Error).name === "AbortError" || generation.current !== vaultGeneration || searchGeneration.current !== requestGeneration) return;
			setResults([]);
			setError((reason as Error).message);
		} finally {
			if (searchRequest.current === controller) searchRequest.current = null;
			if (generation.current === vaultGeneration && searchGeneration.current === requestGeneration) setSearching(false);
		}
	}, [acceptCatalogResponse, allVaults, selected]);

		useEffect(() => {
			if (query.trim() === searchedQuery) return;
			searchGeneration.current++;
			searchRequest.current?.abort();
			searchRequest.current = null;
		setResults(null);
		setSearchedQuery("");
			setMatchesVisible(false);
			setSearchPage(emptyPageState());
			setSearching(false);
		}, [query, searchedQuery]);

		useEffect(() => {
			if (!searchCommand || indexBlocked || handledSearchSequence.current >= searchCommand.sequence) return;
			if (query.trim() !== searchCommand.query) return;
			handledSearchSequence.current = searchCommand.sequence;
			void performSearch(searchCommand.query, searchCommand.sequence);
		}, [indexBlocked, performSearch, query, searchCommand]);

	const loadMoreResults = async () => {
		const page = searchPage;
		if (!page.hasMore || page.loading || searchRequest.current) return;
		const controller = new AbortController();
		searchRequest.current = controller;
		const requestGeneration = searchGeneration.current;
		const vaultGeneration = generation.current;
		setSearchPage((current) => ({ ...current, loading: true }));
		try {
			const response = Number.isInteger(page.revision)
				? await searchFiles(selected, searchedQuery, page.nextOffset, controller.signal, page.revision)
				: await searchFiles(selected, searchedQuery, page.nextOffset, controller.signal);
			if (generation.current !== vaultGeneration || searchGeneration.current !== requestGeneration || searchRequest.current !== controller) return;
			if (!acceptCatalogResponse(response, true)) return;
			setResults((current) => appendUniqueResults(current ?? [], response.items));
			setSearchPage({ hasMore: response.hasMore, nextOffset: response.nextOffset, loading: false, revision: response.revision });
		} catch (reason) {
			if (isMetadataRevisionError(reason) && generation.current === vaultGeneration && searchGeneration.current === requestGeneration && searchRequest.current === controller) {
				searchRequest.current = null;
				await performSearch(searchedQuery);
			} else if ((reason as Error).name !== "AbortError" && generation.current === vaultGeneration && searchGeneration.current === requestGeneration) setError((reason as Error).message);
		} finally {
			if (searchRequest.current === controller) searchRequest.current = null;
			if (generation.current === vaultGeneration && searchGeneration.current === requestGeneration) {
				setSearchPage((current) => ({ ...current, loading: false }));
			}
		}
	};

    const toggleResult = (result: FileSearchResult) => {
        const key = resultKey(result);
		if (selectedItems[key]) {
            historyRequests.current.get(key)?.abort();
            historyRequests.current.delete(key);
			setSelectedItems((current) => { const next = { ...current }; delete next[key]; return next; });
			setHistories((current) => { const next = { ...current }; delete next[key]; return next; });
			setChosenVersions((current) => { const next = { ...current }; delete next[key]; return next; });
            return;
        }
		const replacedKeys = Object.entries(selectedItems)
			.filter(([, selectedResult]) => resultsOverlap(selectedResult, result))
			.map(([selectedKey]) => selectedKey);
		for (const replacedKey of replacedKeys) {
			historyRequests.current.get(replacedKey)?.abort();
			historyRequests.current.delete(replacedKey);
		}
		setSelectedItems((current) => Object.fromEntries([
			...Object.entries(current).filter(([selectedKey]) => !replacedKeys.includes(selectedKey)),
			[key, result],
		]));
		setHistories((current) => Object.fromEntries(Object.entries(current).filter(([selectedKey]) => !replacedKeys.includes(selectedKey))));
		setChosenVersions((current) => Object.fromEntries(Object.entries(current).filter(([selectedKey]) => !replacedKeys.includes(selectedKey))));
		if (histories[key] !== undefined) return;
		const controller = new AbortController();
		const vaultGeneration = generation.current;
		historyRequests.current.set(key, controller);
		void (async () => {
			let attempts = 0;
			while (attempts < 3) {
				attempts++;
				try {
					let response = await getFileHistory(selected, result.path, result.source, 0, controller.signal);
					if (generation.current !== vaultGeneration || historyRequests.current.get(key) !== controller) return;
					if (!acceptCatalogResponse(response, true)) return;
					let items = response.items;
					while (response.hasMore) {
						if (!Number.isSafeInteger(response.nextOffset) || response.nextOffset <= response.offset) {
							throw new Error(t("ui.findFile.invalidHistoryPageBoundary"));
						}
						response = await getFileHistory(selected, result.path, result.source, response.nextOffset, controller.signal, response.revision);
						if (generation.current !== vaultGeneration || historyRequests.current.get(key) !== controller) return;
						if (!acceptCatalogResponse(response, true)) return;
						items = appendUniqueHistory(items, response.items);
					}
					if (generation.current !== vaultGeneration || historyRequests.current.get(key) !== controller) return;
					setHistories((current) => ({ ...current, [key]: items }));
					const latest = newestPresentVersion(items);
					if (latest) setChosenVersions((current) => ({ ...current, [key]: fileVersionKey(latest) }));
					return;
				} catch (reason) {
					if (isMetadataRevisionError(reason) && attempts < 3) continue;
					if ((reason as Error).name !== "AbortError" && generation.current === vaultGeneration && historyRequests.current.get(key) === controller) {
						toast("error", (reason as Error).message);
					}
					return;
				}
			}
		})().finally(() => {
			if (historyRequests.current.get(key) === controller) historyRequests.current.delete(key);
		});
    };

	const loadBrowsePage = async (node: FileBrowseEntry | null, append: boolean) => {
		if (indexBlocked) return;
		const key = node ? browseNodeKey(node) : browseRootKey;
		const page = browsePages[key];
		if (browseRequests.current.has(key) || (append && (!page?.hasMore || page.loading))) return;
		const controller = new AbortController();
		const vaultGeneration = generation.current;
		const offset = append ? page.nextOffset : 0;
		browseRequests.current.set(key, controller);
		setBrowsePages((current) => ({ ...current, [key]: { ...(current[key] ?? { hasMore: false, nextOffset: 0, revision: 0 }), loading: true } }));
		setBrowseErrors((current) => { const next = { ...current }; delete next[key]; return next; });
		try {
			const response = append && Number.isInteger(page.revision)
				? node
					? await browseFiles(selected, node.path, node.source, offset, controller.signal, page.revision)
					: await browseFiles(selected, "", "", offset, controller.signal, page.revision)
				: node
					? await browseFiles(selected, node.path, node.source, offset, controller.signal)
					: await browseFiles(selected, "", "", offset, controller.signal);
			if (generation.current !== vaultGeneration || browseRequests.current.get(key) !== controller) return;
			if (!acceptCatalogResponse(response, true)) return;
			setBrowseItems((current) => ({ ...current, [key]: append ? appendUniqueResults(current[key] ?? [], response.items) as FileBrowseEntry[] : response.items }));
			setBrowsePages((current) => ({ ...current, [key]: { hasMore: response.hasMore, nextOffset: response.nextOffset, loading: false, revision: response.revision } }));
		} catch (reason) {
			if (append && isMetadataRevisionError(reason) && generation.current === vaultGeneration && browseRequests.current.get(key) === controller) {
				browseRequests.current.delete(key);
				const resetController = new AbortController();
				browseRequests.current.set(key, resetController);
				try {
					const response = node
						? await browseFiles(selected, node.path, node.source, 0, resetController.signal)
						: await browseFiles(selected, "", "", 0, resetController.signal);
					if (generation.current !== vaultGeneration || browseRequests.current.get(key) !== resetController) return;
					if (!acceptCatalogResponse(response, true)) return;
					setBrowseItems((current) => ({ ...current, [key]: response.items }));
					setBrowsePages((current) => ({ ...current, [key]: { hasMore: response.hasMore, nextOffset: response.nextOffset, loading: false, revision: response.revision } }));
				} catch (resetReason) {
					if ((resetReason as Error).name !== "AbortError" && generation.current === vaultGeneration) setBrowseErrors((current) => ({ ...current, [key]: (resetReason as Error).message }));
				} finally {
					if (browseRequests.current.get(key) === resetController) browseRequests.current.delete(key);
				}
			} else if ((reason as Error).name !== "AbortError" && generation.current === vaultGeneration) {
				setBrowseErrors((current) => ({ ...current, [key]: (reason as Error).message }));
			}
		} finally {
			if (browseRequests.current.get(key) === controller) browseRequests.current.delete(key);
			if (generation.current === vaultGeneration) setBrowsePages((current) => current[key] ? ({ ...current, [key]: { ...current[key], loading: false } }) : current);
		}
	};

	const toggleBrowseNode = (node: FileBrowseEntry) => {
		const key = browseNodeKey(node);
		if (expandedBrowseKeys.includes(key)) {
			setExpandedBrowseKeys((current) => current.filter((value) => value !== key));
			return;
		}
		setExpandedBrowseKeys((current) => [...current, key]);
		if (browseItems[key] === undefined) void loadBrowsePage(node, false);
	};

	const retryIndex = async () => {
		if (!selected || retryingIndex) return;
		preparationScheduledOrActive.current = true;
		setRetryingIndex(true);
		setIndexing(true);
		preparationAction.current = "retry";
		setPreparationSequence((value) => value + 1);
	};

	const forceRefresh = useCallback(() => {
		if (!selected || forcingMetadata) return;
		preparationScheduledOrActive.current = true;
		setForcingMetadata(true);
		setIndexing(true);
		setMetadataPaused(false);
		setMetadataStage("queued");
		setCompleteHeaderListing(true);
		preparationAction.current = "force";
		setPreparationSequence((value) => value + 1);
	}, [forcingMetadata, selected]);

	useEffect(() => {
		if (forceRefreshSequence <= handledForceRefreshSequence.current) return;
		handledForceRefreshSequence.current = forceRefreshSequence;
		forceRefresh();
	}, [forceRefresh, forceRefreshSequence]);

    const reviewRestore = () => {
		if (!canRestore) return;
		if (restoreSubmissionOwner.current) return;
		restoreReviewSession.current++;
		restoreSubmissionOwner.current = null;
		setRestoring(false);
		setReviewedRestore({
			repositoryId: selected,
			targetPath: effectiveTargetPath,
			items: restoreItems.map((item) => ({ ...item })),
			conflictMode: defaultConflictMode(selectedRepository, engines),
			mode,
			rows: selectedResults.map((result, index) => {
				const restoreItem = restoreItems[index];
				const version = fileVersionForKey(histories[resultKey(result)], fileVersionKey(restoreItem));
				return {
					key: resultKey(result),
					path: result.path,
					isDir: version?.isDir ?? result.isDir,
					timestamp: (mode === "single"
						? commonSnapshots.find((item) => item.id === restoreItem.snapshotId)?.timestamp
						: version?.timestamp) ?? "",
				};
			}),
		});
	};

	const dismissReviewedRestore = () => {
		if (restoreSubmissionOwner.current?.session === restoreReviewSession.current) return false;
		restoreReviewSession.current++;
		restoreSubmissionOwner.current?.observationController.abort();
		restoreSubmissionOwner.current = null;
		setRestoring(false);
		setReviewedRestore(null);
		return true;
	};

	const cancelColdRestore = () => {
		restoreSubmissionOwner.current?.controller?.abort();
	};

    const doRestore = async () => {
		if (!reviewedRestore) return;
		if (restoreSubmissionOwner.current) return;
		const reviewed = reviewedRestore;
		const payload = {
			operationId: window.crypto.randomUUID(),
			repositoryId: reviewed.repositoryId,
			targetPath: reviewed.targetPath,
			items: reviewed.items.map((item) => ({ ...item })),
			conflictMode: reviewed.conflictMode,
		};
		const coldStorage = Boolean(selectedRepository?.coldStorage);
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
        setRestoring(true);
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
				? restoreSelection(payload, owner.controller.signal)
				: restoreSelection(payload);
			const handoff = observeSubmittedRestoreOperation(payload.operationId, () =>
				ownsSubmission() && owner.requestPending && !owner.handedOff,
				owner.observationController.signal,
			).then((operation) => {
				if (!operation || !ownsSubmission() || owner.handedOff) return false;
				owner.handedOff = true;
				owner.requestPending = false;
				owner.observationController.abort();
				setReviewedRestore(null);
				navigate(`/?operation=${encodeURIComponent(payload.operationId)}`);
				return true;
			});
			let outcome: Awaited<ReturnType<typeof restoreSelection>> | undefined;
			let requestError: unknown;
			try {
				outcome = await request;
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
					setReviewedRestore(null);
					navigate(`/?operation=${encodeURIComponent(payload.operationId)}`);
					return;
				}
				if (requestError) throw requestError;
				if (!outcome) return;
				owner.requestPending = false;
				if (outcome.status === "success") toast("ok", `Restored ${outcome.restored} selected item${outcome.restored === 1 ? "" : "s"}.`);
				else toast("error", `Restore was ${outcome.status}: ${outcome.restored} restored, ${outcome.failed} failed, ${outcome.notAttempted} not attempted.`);
				setReviewedRestore(null);
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
				setRestoring(false);
			}
        }
    };

	const renderBrowseRows = (items: FileBrowseEntry[], depth: number): ReactNode => items.map((item) => {
		const key = browseNodeKey(item);
		const expanded = expandedBrowseKeys.includes(key);
		const expandable = item.isSource || Boolean(item.canExpand) || item.isDir;
		const checked = !item.isSource && Boolean(selectedItems[resultKey(item)]);
		const childPage = browsePages[key];
		const childError = browseErrors[key];
		const source = nativeRootLabel(item.source);
		const label = item.isSource ? source : item.name;
		const selectionLabel = checked
            ? t("ui.fileHistory.deselectFromSource", { item: item.isSource ? label : displayFilePath(item.path, item.source), source })
            : t("ui.fileHistory.selectFromSource", { item: item.isSource ? label : displayFilePath(item.path, item.source), source });
		const disabledMessage = item.isSource ? "" : parentSelectionMessage(item);
		const rowLabel = <strong>{label}</strong>;
		return <div className="browse-tree-node" key={key} role="listitem">
			<div className={`browse-tree-row${checked ? " selected" : ""}`} style={{ paddingLeft: 10 + depth * 22 } as CSSProperties}>
				{expandable ? <button type="button" className="browse-disclosure" aria-expanded={expanded} aria-label={expanded ? t("ui.fileHistory.collapseNamed", { name: label }) : t("ui.fileHistory.expandNamed", { name: label })} onClick={() => toggleBrowseNode(item)}>{expanded ? "▾" : "▸"}</button> : <span className="browse-disclosure-spacer" />}
				{item.isSource ? <span className="browse-checkbox-spacer" /> : <SelectionCheckbox checked={checked} label={selectionLabel} parentSelectionMessage={disabledMessage} onChange={() => toggleResult(item)} />}
				<Icon name={item.isDir ? "folder" : "file"} size={17} />
				{expandable
					? <button type="button" className="browse-tree-main browse-tree-label-button" aria-expanded={expanded} aria-label={t("ui.fileHistory.toggleNamedFromName", { name: label })} onClick={() => toggleBrowseNode(item)}>{rowLabel}</button>
					: <span className="browse-tree-main">{rowLabel}</span>}
			</div>
			{expanded && <div role="list" className="browse-tree-children">
				{childPage?.loading && browseItems[key] === undefined && <div role="listitem" className="browse-tree-status"><span className="spinner" /> {t("ui.pages.findfile.loading.folder")}</div>}
				{childError && !childPage?.loading && <div role="listitem" className="browse-tree-status"><span>{childError}</span><button type="button" className="btn" onClick={() => void loadBrowsePage(item, false)}>{t("ui.pages.findfile.retry.folder")}</button></div>}
				{browseItems[key] && browseItems[key].length === 0 && !childPage?.loading && <div role="listitem" className="browse-tree-status">{t("ui.pages.findfile.this.folder.has.no.indexed.items")}</div>}
				{renderBrowseRows(browseItems[key] ?? [], depth + 1)}
				{childPage?.hasMore && <div role="listitem"><button type="button" className="btn browse-load-more" disabled={childPage.loading} onClick={() => void loadBrowsePage(item, true)}>{childPage.loading && <span className="spinner" />}{t("ui.fileHistory.loadMoreIn", { folder: label })}</button></div>}
			</div>}
		</div>;
	});
		const restorePanel = selectedResults.length > 0 && <aside className="find-selection-panel">
		<div className="section-heading"><div><h2>{t("ui.pages.findfile.restore.selected")}</h2><p className="section-copy">{t("ui.pages.findfile.choose.one.point.in.time.or.a.version.per.item")}</p></div></div>
		<div className="find-mode-tabs">
			<button className={mode === "single" ? "active" : ""} onClick={() => setMode("single")}>{t("ui.pages.findfile.one.backup.date")}</button>
			<button className={mode === "individual" ? "active" : ""} onClick={() => setMode("individual")}>{t("ui.pages.findfile.each.item.s.version")}</button>
		</div>
		{mode === "single" ? <label className="field"><span>{t("ui.pages.findfile.backup.date.shared.by.all.selected.items")}</span><select value={effectiveSingleSnapshot} onChange={(event) => setSingleSnapshot(event.target.value)} disabled={!allHistoriesLoaded || commonSnapshots.length === 0}>{(!allHistoriesLoaded || commonSnapshots.length === 0) && <option value="">{allHistoriesLoaded ? t("ui.pages.findfile.no.unambiguous.common.backup.contains.every.item") : t("ui.pages.findfile.loading.item.history")}</option>}{commonSnapshots.map((snapshot) => { const machineLabel = sharedSnapshotMachineLabel(snapshot.id, selectedResults, histories); return <option key={snapshot.id} value={snapshot.id}>{formatDate(snapshot.timestamp)}{machineLabel ? ` · ${machineLabel}` : ""}</option>; })}</select></label> : <div className="find-version-list">
			{selectedResults.map((result) => {
				const key = resultKey(result);
				const versions = presentVersions(histories[key]).sort(newestVersionFirst);
					return <label className="field find-version-row" key={key}><FilePathLabel value={result.path} source={result.source} tooltipPath={displayFullFilePath(result.source, result.path)} /><select value={chosenVersions[key] ?? ""} onChange={(event) => setChosenVersions((current) => ({ ...current, [key]: event.target.value }))} disabled={!versions.length}><option value="">{versions.length ? t("ui.pages.findfile.choose.a.version") : t("ui.pages.findfile.loading.history")}</option>{versions.map((version) => <option key={fileVersionKey(version)} value={fileVersionKey(version)}>{versionChoiceLabel(version)}</option>)}</select></label>;
			})}
		</div>}
		<div className="find-selected-items"><span className="modal-section-label">{t("ui.pages.findfile.selected.items")}</span>{selectedResults.map((result) => { const key = resultKey(result); const version = selectedVersion(result); const isDir = version?.isDir ?? result.isDir; return <div className="find-selected-item" key={key}><Icon name={isDir ? "folder" : "file"} size={14} /><span style={{ display: "flex", minWidth: 0, flex: 1, flexDirection: "column", gap: 3 }}><FilePathLabel value={result.path} source={result.source} tooltipPath={displayFullFilePath(result.source, result.path)} />{isDir && <span className="mono faint">{t("ui.pages.findfile.all.files.and.folders.inside")}</span>}</span><span className="mono faint">{versionTypeLabel(version, result.isDir)}</span><button onClick={() => toggleResult(result)} aria-label={t("ui.fileHistory.removeNamed", { name: displayFilePath(result.path, result.source) })}>{t("ui.pages.findfile.remove")}</button></div>; })}</div>
		{restoreSelectionConflict && <div className="inline-error" role="alert">{restoreSelectionConflict}</div>}
		<label className="field"><span>{t("ui.pages.findfile.restore.destination")}</span><DirectoryField value={effectiveTargetPath} onChange={setTargetPath} placeholder={t("ui.pages.findfile.choose.a.new.or.existing.destination")} /></label>
			<button className="btn primary find-restore-button" disabled={!canRestore} onClick={reviewRestore}><Icon name="restore" size={14} />{t("ui.pages.findfile.review.restore")}</button>
		</aside>;
		const showSearchSurface = allVaults || Boolean(error || indexing || searching || matchesVisible || indexState?.repositoryFailed || (indexState?.failedSnapshots ?? 0) > 0);
		const SearchSurface = allVaults ? "section" : "div";

	    return (
		<>
			<PortalSurface target={searchTarget}>
			{showSearchSurface && <SearchSurface className="find-history-section find-search-section find-vault-matches" aria-labelledby={allVaults ? `${sectionId}-search-heading` : undefined}>
				{allVaults && <div className="section-heading find-all-vault-heading"><div><h2 id={`${sectionId}-search-heading`}>{searchCommand ? t("ui.fileHistory.matchesInVault", { vault: repository.name }) : repository.name}</h2></div><Tooltip content={SNAPSHOT_HISTORY_TOOLTIP()}><button className="metadata-force-link" aria-label={t("ui.fileHistory.refreshHistoryForVault", { vault: repository.name })} disabled={forcingMetadata} onClick={forceRefresh}>{forcingMetadata && <span className="spinner" />}{t("ui.pages.findfile.refresh.snapshot.history")}</button></Tooltip></div>}
				{error && <div className="inline-error">{error}</div>}
				{indexState?.repositoryFailed && <div className="inline-notice" role="status">{t("ui.pages.findfile.backup.history.could.not.be.refreshed.search.and.browsing.remain.disab")}</div>}
				{indexState?.headerValid && (indexState.failedSnapshots ?? 0) > 0 && <div className="inline-notice" role="status">{t("ui.fileHistory.indexingFailed", { count: indexState.failedSnapshots ?? 0 })} <button className="btn" onClick={() => void retryIndex()} disabled={retryingIndex}>{retryingIndex ? t("ui.fileHistory.retrying") : t("ui.fileHistory.retry")}</button></div>}
				<MetadataIndexNotice active={indexing} paused={metadataPaused} stage={metadataStage} completeHeaderListing={completeHeaderListing} state={indexState} blockSearchAndBrowse />
				{searching && <Loading />}
				{!searching && matchesVisible && results && results.length === 0 && <EmptyState icon="file" title={t("ui.pages.findfile.no.matches")}><p>{t("ui.fileHistory.noMatchesForQuery", { query: searchedQuery })}</p></EmptyState>}

				{!searching && matchesVisible && results && results.length > 0 && <div className="find-file-workspace">
					{restorePanel}
					<section className="find-results-panel">
						<div className="section-heading find-matches-heading"><div>{!allVaults && <h2>{t("ui.pages.findfile.matches")}</h2>}<p className="section-copy">{t("ui.fileHistory.resultCountHelp", { count: results.length })}</p></div><div className="find-matches-actions"><span className="mono faint">{t("ui.fileHistory.selectedCount", { count: selectedResults.length })}</span><button type="button" className="icon-btn" aria-label={t("ui.pages.findfile.close.matches")} onClick={() => { setMatchesVisible(false); onFocusSearch?.(); }}>×</button></div></div>
						<section className="find-presentation-group without-heading" aria-label={allVaults ? t("ui.fileHistory.namedVaultSearchResults", { vault: repository.name }) : t("ui.fileHistory.vaultSearchResults")}>
							<div className="find-results-list">{results.map((result) => {
								const key = resultKey(result);
								const checked = Boolean(selectedItems[key]);
								const path = displayFilePath(result.path, result.source);
								const selectionLabel = checked ? t("ui.fileHistory.deselectNamed", { name: path }) : t("ui.fileHistory.selectNamed", { name: path });
								return <label key={key} className={`find-result${checked ? " selected" : ""}`}><SelectionCheckbox checked={checked} label={selectionLabel} parentSelectionMessage={parentSelectionMessage(result)} onChange={() => toggleResult(result)} /><Icon name={result.isDir ? "folder" : "file"} size={18} /><span className="find-result-main"><strong><FilePathLabel value={result.path} source={result.source} /></strong><span className="mono">{t("ui.fileHistory.sourceLabel")} {sourceLabel(result)}</span></span></label>;
							})}</div>
							{searchPage.hasMore && <button className="btn find-presentation-load-more" onClick={() => void loadMoreResults()} disabled={searchPage.loading}>{searchPage.loading && <span className="spinner" />}{t("ui.pages.findfile.load.more.matches")}</button>}
						</section>
					</section>
				</div>}
			</SearchSurface>}
			</PortalSurface>

			<PortalSurface target={browseTarget}>
			<section className="find-history-section find-browse-section" aria-labelledby={`${sectionId}-browse-heading`}>
				<div className="section-heading"><div><h2 id={`${sectionId}-browse-heading`}>{allVaults ? t("ui.fileHistory.browseNamedVault", { vault: repository.name }) : t("ui.fileHistory.browseVault")}</h2></div></div>
				{indexBlocked ? <EmptyState icon="history" title={t("ui.pages.findfile.indexing.snapshot.metadata")}><p>{t("ui.pages.findfile.search.and.browsing.is.disabled.while.indexing.completes")}</p></EmptyState> : <div className="find-file-workspace browse-workspace">
								{restorePanel}
								<div className="browse-vault-panel">
									{(() => { const key = browseRootKey; const items = browseItems[key]; const page = browsePages[key]; return <section className="find-presentation-group without-heading" aria-label={allVaults ? t("ui.fileHistory.browseNamedVaultFiles", { vault: repository.name }) : t("ui.fileHistory.browseVaultFiles")}>
									{browseErrors[key] && <div className="inline-error" role="alert">{browseErrors[key]} <button type="button" className="btn" onClick={() => void loadBrowsePage(null, false)}>{t("ui.pages.findfile.retry.vault.browser")}</button></div>}
									{page?.loading && items === undefined && <div className="browse-tree" role="list" aria-label={t("ui.pages.findfile.vault.file.history")}><div role="listitem" className="browse-tree-status"><span className="spinner" /> {t("ui.pages.findfile.loading.vault.contents")}</div></div>}
									{items && items.length > 0 && <div className="browse-tree" role="list" aria-label={t("ui.pages.findfile.vault.file.history")}>{renderBrowseRows(items, 0)}</div>}
									{page?.hasMore && <button type="button" className="btn browse-load-more" disabled={page.loading} onClick={() => void loadBrowsePage(null, true)}>{page.loading && <span className="spinner" />}{t("ui.pages.findfile.load.more.sources")}</button>}
									{items?.length === 0 && !page?.loading && <div className="browse-tree" role="list" aria-label={t("ui.pages.findfile.vault.file.history")}><div role="listitem" className="browse-tree-status">{t("ui.pages.findfile.no.files.or.folders.found.yet")}</div></div>}
								</section>; })()}
							</div>
						</div>}
			</section>
			</PortalSurface>

			{reviewedRestore && <Modal title={t("ui.pages.findfile.review.restore")} onClose={dismissReviewedRestore} wide>
				<fieldset className="modal-workflow-fields" disabled={restoring}>
                <p className="muted modal-intro">{reviewedRestore.mode === "single" ? t("ui.fileHistory.singleDateHelp") : t("ui.fileHistory.perItemHelp")}</p>
				<div className="find-confirm-list">{reviewedRestore.rows.map((row) => <div className="find-confirm-row" key={row.key}><Icon name={row.isDir ? "folder" : "file"} size={15} /><span><strong><FilePathLabel value={row.path} /></strong><span className="mono">{formatDate(row.timestamp)} · {row.isDir ? t("ui.findFile.folderType") : t("ui.findFile.fileType")}</span></span></div>)}</div>
                <div className="find-confirm-destination"><span>{t("ui.pages.findfile.destination")}</span><strong className="mono">{displayPath(reviewedRestore.targetPath)}</strong></div>
				{(restoreCapability(selectedRepository, engines)?.conflictModes.length ?? 0) <= 1
					? <p className="muted">{restoreCapability(selectedRepository, engines)?.conflictModes[0]?.description}</p>
					: <label className="field"><span>{t("ui.pages.findfile.should.existing.files.be.overwritten")}</span><select value={reviewedRestore.conflictMode} onChange={(event) => setReviewedRestore((current) => current ? { ...current, conflictMode: event.target.value } : current)}>{restoreCapability(selectedRepository, engines)?.conflictModes.map((mode) => <option key={mode.id} value={mode.id}>{restoreConflictModeLabel(mode)}</option>)}</select></label>}
				{restoring && selectedRepository?.coldStorage && <p className="recovery-warning">{t("ui.pages.findfile.cold.storage.retrieval.can.take.hours.or.days.replicaro.is.waiting.for")}</p>}
				</fieldset>
				<div className="modal-footer"><button className="btn" disabled={restoring && !selectedRepository?.coldStorage} onClick={restoring && selectedRepository?.coldStorage ? cancelColdRestore : dismissReviewedRestore}>{restoring && selectedRepository?.coldStorage ? t("ui.pages.findfile.cancel.restore") : t("ui.findFile.back")}</button><button className="btn primary" disabled={restoring} onClick={() => void doRestore()}>{restoring && <span className="spinner" />}<Icon name="restore" size={14} />{t("ui.pages.findfile.restore.selected")}</button></div>
			</Modal>}

		</>
	    );
}
