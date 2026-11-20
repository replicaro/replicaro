import { formatDisplayDate, formatDisplayDateTime, getEffectiveLocale, isRightToLeft, renderMessage, t } from "../i18n";
import { useCallback, useEffect, useId, useMemo, useRef, useState } from "react";
import type { CSSProperties, KeyboardEvent, ReactNode } from "react";
import { createPortal } from "react-dom";
import { useNavigate, useParams } from "react-router-dom";

import { DirectoryField } from "../components/DirectoryPicker";
import { FileDownloadButton } from "../components/FileDownloadButton";
import { FloatingTooltip } from "../components/FloatingTooltip";
import { JobSourceLabel } from "../components/JobSourceLabel";
import { fileHistorySourceJob, jobSourceTooltip, pathWithinJob } from "../jobSourceDisplay";
import { displayPath } from "../nativePathDisplay";
import { ltrIsolate } from "../services/rcloneRemote";
import { MetadataIndexNotice } from "../components/MetadataIndexNotice";
import { MultilineSelect } from "../components/MultilineSelect";
import { EmptyState, Icon, Loading, Modal, parseTime, Tooltip, useToast } from "../components/ui";
import {
	APIError,
	browseFiles,
	getFileHistory,
	getJobs,
	getMetadataStatus,
    getEngines,
    getRepositories,
    listSnapshots,
	prepareMetadata,
	restoreSelection,
    searchFiles,
} from "../services/api";
import type { MetadataPreparationStatus } from "../services/api";
import { defaultConflictMode, restorableContainingFolder, RESTORE_SELECTION_NAME_INVALID, restoreCapability, restoreConflictModeLabel } from "../restoreCapabilities";
import type { BackupJob, EngineDescriptor, FileBrowseEntry, FileHistoryVersion, FileSearchResult, FileVersion, MetadataIndexState, Repository, Snapshot } from "../types";

// The three tabs of the "Restore selected" panel, in their order: a version of
// each file (folders keep a backup date), one backup date shared by every
// item, or a backup date per item.
//
// "File version" comes first and is the tab the panel opens on. That order,
// that default, and offering the modes as tabs rather than a toggle or a
// one-time prompt are product decisions; don't change them casually.
type RestoreMode = "version" | "single" | "individual";
const restoreModes: RestoreMode[] = ["version", "single", "individual"];
type HistoryMap = Record<string, FileVersion[]>;
type FileVersionListMap = Record<string, FileHistoryVersion[]>;
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

// refusedFileSourceVersion reports whether the refused item must be a version
// of a single-file source. Such an item is sent with the empty path, so its
// name is not in the request; when no item's path has a name the backend
// could refuse, the refused name is that of a single-file source's recorded
// root. The backend refuses U+FFFD everywhere and a backslash only on Windows;
// the page can't tell which system the backend runs on, so any backslash in an
// item's path counts and the folder text is shown. A single-file source has
// no containing folder to offer: only the whole snapshot, from the Restore
// page, restores it.
function refusedFileSourceVersion(items: ReviewedRestoreItem[]) {
	return items.some((item) => item.path === "") &&
		!items.some((item) => item.path.includes("\u{FFFD}") || item.path.includes("\\"));
}

// containingFolderRestoreItems replaces each item whose name the backend
// refused with its nearest clean folder from the same snapshot, and drops
// items that folder now contains. folders lists the replacement folders so the
// review can show what the action restores. It returns null when an item has
// no such folder, or when nothing would change. It also returns null when the
// folder would contain an item chosen from another snapshot: dropping that
// item would restore the folder's snapshot in place of the version the user
// chose for it, so no action is offered.
//
// Names with U+FFFD are refused on every backend. When the refused request has
// none, the refusal came from a Windows backend and a backslash, so those items
// are replaced instead, as the Restore page does for its single item.
function containingFolderRestoreItems(items: ReviewedRestoreItem[]): { items: ReviewedRestoreItem[]; folders: string[] } | null {
	const refusedChar = items.some((item) => item.path.includes("\uFFFD")) ? "\uFFFD" : "\\";
	const replaced = items.map((item) => item.path.includes(refusedChar) ? { ...item, path: restorableContainingFolder(item.path) } : item);
	if (replaced.some((item) => item.path === "") || replaced.every((item, index) => item === items[index])) return null;
	const kept: ReviewedRestoreItem[] = [];
	const folders: string[] = [];
	for (const [index, item] of replaced.entries()) {
		const covering = replaced.find((other, otherIndex) => otherIndex !== index &&
			(other.path === item.path ? otherIndex < index : item.path.startsWith(`${other.path}/`)));
		if (!covering) {
			kept.push(item);
			if (item !== items[index]) folders.push(item.path);
			continue;
		}
		if (covering.snapshotId !== item.snapshotId || covering.nativeRootId !== item.nativeRootId) return null;
	}
	return { items: kept, folders };
}

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

// A single-file source comes back as a file at the empty path: the source
// itself. It restores to <destination>/<name>, like a selected file at the top
// of a folder source, so its name stands in for its path wherever a path is
// shown or compared for the restore. The request still sends the empty path.
function isFileSourceResult(result: FileSearchResult) {
	return result.path === "" && !result.isDir;
}

function shownResultPath(result: FileSearchResult) {
	return isFileSourceResult(result) ? result.name : result.path;
}

// The name the restore request's duplicate check uses for a version of a
// single-file source: the last element of the root the engine recorded for
// that snapshot (FileVersion.source). With Restic that is the name the file
// restores under. A Kopia version backed up through "Update job source" from
// a differently named file records the job's source but restores under its
// root entry's name, which is not in the cache (Kopia reads it again at
// restore), so neither this page nor the check can use it; Kopia's numbering
// keeps two items from landing on one name. A root that starts with "/" is a
// POSIX path, where a backslash is an ordinary character in a name, so only
// "/" separates; any other root is a Windows drive or UNC path, where either
// slash does.
//
// This must give the same name as database.MetadataFileRootName in the
// backend, which the duplicate check uses. Change both together.
function recordedFileRootName(root: string) {
	const separator = root.startsWith("/") ? root.lastIndexOf("/") : Math.max(root.lastIndexOf("/"), root.lastIndexOf("\\"));
	return root.slice(separator + 1);
}

// The path an item is checked at for duplicates and nesting: where it lands
// under the destination, apart from the Kopia case below. Two single-file
// sources with the same name would land on the same file, and one named
// "docs" next to a selected docs/a.txt would be nested, exactly like folder
// items with those paths.
//
// A single-file source counts under the name recorded for the chosen version,
// which is what the backend compares (see recordedFileRootName for the Kopia
// version that lands under another name). It can differ from the search
// result's name when the job was backed up through "Update job source" with a
// file of another name (a rename, or only a change of case), so don't use
// result.name once a version is chosen. Before that the result's name stands
// in.
function restoreLandingPath(result: FileSearchResult, version: FileVersion | undefined) {
	if (!isFileSourceResult(result)) return canonicalSelectionPath(result);
	return version ? recordedFileRootName(version.source) : result.name;
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

function selectionConflict(results: FileSearchResult[], versionOf: (result: FileSearchResult) => FileVersion | undefined) {
	for (let left = 0; left < results.length; left++) {
		const leftPath = restoreLandingPath(results[left], versionOf(results[left]));
		for (let right = left + 1; right < results.length; right++) {
			const rightPath = restoreLandingPath(results[right], versionOf(results[right]));
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

// Relative archive names are slash-delimited regardless of the viewing OS.
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

// withinJob: the row belongs to a job-owned source, so its tooltip shows the
// item's path within the job (for example 2024\IMG_001.jpg) instead of a full
// native path. Unmanaged sources keep the native presentation.
//
// tooltipPath (the restore-selection panel) and floating (the search results
// list) render the tooltip with FloatingTooltip, because both boxes clip a
// popup rendered inside the label. The other callers are in a Modal and keep
// the shared Tooltip, which opens above the dialog.
//
// fileSource: the row is a single-file source, whose value is the file's name
// rather than a path inside the snapshot, so the hover title names only the
// backup source.
function FilePathLabel({ value, source, tooltipPath: nativeTooltipPath, withinJob = false, floating = false, fileSource = false }: { value: string; source?: string; tooltipPath?: string; withinJob?: boolean; floating?: boolean; fileSource?: boolean }) {
    const path = displayFilePath(value, source);
	const tooltipPath = withinJob && nativeTooltipPath !== undefined ? pathWithinJob(value, source ?? "") : nativeTooltipPath;
    const parts = splitDisplayPath(path, path.startsWith("\\") && !value.includes("\\"));
    const text = <><span className="file-path-parent">{parts.parent}</span><span className="file-path-name">{parts.name}</span></>;
	const sourceTitle = t("ui.restore.backupSourceTooltip", { source: ltrIsolate(source ?? "") });
    if (tooltipPath) return (
        <FloatingTooltip
            className="file-path-label"
            content={ltrIsolate(tooltipPath)}
            label={tooltipPath}
            title={withinJob ? undefined : fileSource ? sourceTitle : `${sourceTitle}\n${t("ui.restore.snapshotPathTooltip", { path: ltrIsolate(value) })}`}
        >{text}</FloatingTooltip>
    );
    const content = withinJob ? pathWithinJob(value, source ?? "") : path;
    if (floating) return <FloatingTooltip className="file-path-label" content={ltrIsolate(content)} label={content}>{text}</FloatingTooltip>;
    return (
        <Tooltip content={ltrIsolate(content)}>
            <span className="file-path-label">{text}</span>
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

function formatDate(value: string, locale?: string) {
    const date = parseTime(value);
    return date ? formatDisplayDateTime(date, {
        month: "short",
        day: "numeric",
        year: "numeric",
        hour: "2-digit",
        minute: "2-digit",
    }, locale) : t("ui.date.unknownDate");
}

function sourceLabel(result: FileSearchResult) {
	return displayPath(result.source || t("ui.findFile.unknownSource"));
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

// Keep restore choosers flat and newest-first. A backup-date entry for one item
// shows the backup date (with the size for a file), the item's modified time
// and the available machine label; one shared by several items shows only the
// backup date and the machine label. A file-version entry shows only its
// modified time and size, its backups and the machine label. Do not add
// computer grouping or snapshot ID, type, or root-path details to the chooser
// presentation; option values retain exact snapshot/root identities.
function newestFirst(leftTime: string, rightTime: string, left: { snapshotId: string; nativeRootId: string }, right: { snapshotId: string; nativeRootId: string }) {
	if (leftTime !== rightTime) return leftTime > rightTime ? -1 : 1;
	if (left.snapshotId !== right.snapshotId) return left.snapshotId > right.snapshotId ? -1 : 1;
	return left.nativeRootId === right.nativeRootId ? 0 : left.nativeRootId < right.nativeRootId ? -1 : 1;
}

function newestVersionFirst(left: FileVersion, right: FileVersion) {
	return newestFirst(left.timestamp, right.timestamp, left, right);
}

// File versions are ordered by the newest backup that holds them, the one
// each restores from, with the same tie-break as backup dates. The API
// groups a file's versions by computer, so its order can't be shown as is.
function newestFileVersionFirst(left: FileHistoryVersion, right: FileHistoryVersion) {
	return newestFirst(left.lastBackup, right.lastBackup, left, right);
}

function newestSnapshotFirst(left: Snapshot, right: Snapshot) {
	if (left.timestamp !== right.timestamp) return left.timestamp > right.timestamp ? -1 : 1;
	return left.id === right.id ? 0 : left.id > right.id ? -1 : 1;
}

// The lines of a dropdown entry. Dates go through the app's formatters, so
// they follow the language and its digits. Inside a sentence each date is
// isolated (<bdi>) so the sentence around it can't reorder its parts. The
// size and a computer's name (name@OS, a Restic host name) are shown left to
// right as written, so a right-to-left language doesn't move "@", "#2" or the
// unit to the other end. A computer the backend has no name for is page text
// instead: "Previous computer" in the page's language and direction, with the
// backend's number when several computers share that label. Its English
// machineLabel is never shown, but it stays what identifies the computer when
// labels are compared (see sharedSnapshotComputer).
type ComputerLabel = Pick<FileVersion, "machineLabel" | "unnamedComputer" | "unnamedComputerNumber">;

function computerLine(computer: ComputerLabel | undefined): ReactNode[] {
	if (computer?.unnamedComputer) {
		return [computer.unnamedComputerNumber
			? t("ui.findFile.previousComputerNumbered", { number: computer.unnamedComputerNumber })
			: t("ui.findFile.previousComputer")];
	}
	return computer?.machineLabel ? [<bdi dir="ltr">{computer.machineLabel}</bdi>] : [];
}

// A dropdown can show two dates of different kinds: when the backup was made
// and when the item was modified. Each date line is labelled ("Backed up
// on", "Modified", "From backups") so the two can't be mistaken for each
// other. A backup-date entry leads with the backup date because that is what
// is being chosen; a file-version entry leads with the modified time.
//
// A backup-date entry for one item (item given): the backup date, with the
// size for a file; the item's modified time in that backup; the computer.
// Without an item (one backup shared by several items) it is the backup date
// and the computer, as the items' sizes and times differ. A folder's line is
// the modified time the engine lists for it: Restic's is the folder's own
// time, Kopia's is the newest time within it. An empty or unreadable modified
// time leaves its line out (a folder held only through the items inside it
// has none).
function backupDateLines(timestamp: string, computer: ComputerLabel | undefined, locale?: string, item?: FileVersion): ReactNode[] {
	const date = <bdi key="date">{formatDate(timestamp, locale)}</bdi>;
	const first = item && !item.isDir && item.size
		? renderMessage("ui.findFile.backedUpOnWithSize", { date, size: <bdi key="size" dir="ltr" className="find-version-size">{item.size}</bdi> })
		: renderMessage("ui.findFile.backedUpOn", { date });
	return [first, ...(item ? modifiedLine(item.modified, locale) : []), ...computerLine(computer)];
}

function modifiedLine(modified: string, locale?: string): ReactNode[] {
	if (!modified || !parseTime(modified)) return [];
	return [renderMessage("ui.findFile.itemModified", { date: <bdi key="date">{formatDate(modified, locale)}</bdi> })];
}

// Backup ranges show whole days: a file version can span years, and the time
// of each end says nothing about the file.
function formatBackupDay(value: string, locale?: string) {
	const date = parseTime(value);
	return date ? formatDisplayDate(date, { month: "short", day: "numeric", year: "numeric" }, locale) : t("ui.date.unknownDate");
}

// A file version: when the file was modified and its size (both from the
// backup it restores from), the backups that hold it, and the computer. A
// version held by one backup, or only by backups of one day, names that day
// once instead of a range.
function fileVersionLines(version: FileHistoryVersion, locale: string): ReactNode[] {
	const first = formatBackupDay(version.firstBackup, locale);
	const last = formatBackupDay(version.lastBackup, locale);
	return [
		renderMessage("ui.findFile.versionModified", { date: <bdi key="date">{formatDate(version.modified, locale)}</bdi>, size: <bdi key="size" dir="ltr" className="find-version-size">{version.size}</bdi> }),
		first === last
			? renderMessage("ui.findFile.versionFromBackup", { date: <bdi key="date">{last}</bdi> })
			: renderMessage("ui.findFile.versionFromBackups", { first: <bdi key="first">{first}</bdi>, last: <bdi key="last">{last}</bdi> }),
		...computerLine(version),
	];
}

function restoreModeLabel(mode: RestoreMode) {
	if (mode === "version") return t("ui.findFile.fileVersionTab");
	if (mode === "single") return t("ui.pages.findfile.one.backup.date");
	return t("ui.findFile.eachItemBackupDateTab");
}

// The restore panel is rendered twice (beside the search results and beside
// the browse tree), so the tab and panel ids come from each copy's own useId.
function RestoreModeTabs({ mode, onChange, children }: { mode: RestoreMode; onChange: (mode: RestoreMode) => void; children: ReactNode }) {
	const id = useId();
	const tabs = useRef<Array<HTMLButtonElement | null>>([]);
	const keyDown = (event: KeyboardEvent<HTMLButtonElement>, index: number) => {
		// The tabs run right to left in a right-to-left language, so the arrow
		// keys swap to keep moving in the direction they point.
		const forward = isRightToLeft() ? "ArrowLeft" : "ArrowRight";
		const backward = isRightToLeft() ? "ArrowRight" : "ArrowLeft";
		const last = restoreModes.length - 1;
		const target = event.key === forward ? (index === last ? 0 : index + 1)
			: event.key === backward ? (index === 0 ? last : index - 1)
			: event.key === "Home" ? 0
			: event.key === "End" ? last
			: -1;
		if (target < 0) return;
		event.preventDefault();
		onChange(restoreModes[target]);
		tabs.current[target]?.focus();
	};
	return <>
		<div className="find-mode-tabs" role="tablist" aria-label={t("ui.findFile.restoreModeTabs")}>
			{restoreModes.map((item, index) => <button
				key={item}
				ref={(element) => { tabs.current[index] = element; }}
				type="button"
				role="tab"
				id={`${id}-${item}-tab`}
				aria-selected={mode === item}
				aria-controls={`${id}-panel`}
				tabIndex={mode === item ? 0 : -1}
				className={mode === item ? "active" : ""}
				onClick={() => onChange(item)}
				onKeyDown={(event) => keyDown(event, index)}
			>{restoreModeLabel(item)}</button>)}
		</div>
		<div role="tabpanel" id={`${id}-panel`} aria-labelledby={`${id}-${mode}-tab`} className="find-mode-panel">{children}</div>
	</>;
}

// The computer of one backup shared by several items, when every item's
// history gives it the same label. The labels are compared as the backend
// wrote them, together with the unnamed mark, so a translated "Previous
// computer" is never compared with an English one, and a computer really
// named "Previous computer" is not taken for an unnamed one.
function sharedSnapshotComputer(snapshotId: string, results: FileSearchResult[], histories: HistoryMap): ComputerLabel | undefined {
	const computers = new Map<string, ComputerLabel>();
	for (const version of results.flatMap((result) => presentVersions(histories[resultKey(result)]))) {
		if (version.snapshotId !== snapshotId || !version.machineLabel) continue;
		computers.set(JSON.stringify([version.machineLabel, Boolean(version.unnamedComputer)]), version);
	}
	return computers.size === 1 ? computers.values().next().value : undefined;
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
	// Job names and current sources label job-owned File History sources. A
	// failed load leaves every source shown by its recorded path.
	const [jobs, setJobs] = useState<BackupJob[] | null>(null);
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
		getJobs().then((nextJobs) => { if (active) setJobs(nextJobs); }).catch(() => undefined);
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
						jobs={jobs}
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
						jobs={jobs}
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
	jobs,
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
	jobs: BackupJob[] | null;
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
	// chosenVersions holds each item's backup date (the "Each item's backup
	// date" tab, and folders in the "File version" tab); chosenFileVersions
	// holds each file's version in the "File version" tab. Switching tabs keeps
	// every choice.
    const [chosenVersions, setChosenVersions] = useState<VersionMap>({});
	const [versionLists, setVersionLists] = useState<FileVersionListMap>({});
	const [chosenFileVersions, setChosenFileVersions] = useState<VersionMap>({});
    const [mode, setMode] = useState<RestoreMode>("version");
    const [singleSnapshot, setSingleSnapshot] = useState("");
    const [targetPath, setTargetPath] = useState("");
    const [searching, setSearching] = useState(false);
	const [restoring, setRestoring] = useState(false);
    const [reviewedRestore, setReviewedRestore] = useState<ReviewedRestore | null>(null);
	// Set when the backend refused a selected item's name. offer is the
	// request to send instead, or null when no containing folder can be offered.
	// fileSource is set when the refused item is a single-file source's version.
	const [nameRefusal, setNameRefusal] = useState<{ offer: ReturnType<typeof containingFolderRestoreItems>; fileSource: boolean } | null>(null);
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
	const restoreSubmissionOwner = useRef<{ session: number; generation: number } | null>(null);
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
		setVersionLists({});
		setChosenFileVersions({});
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
		setVersionLists({});
		setChosenFileVersions({});
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
						// Retry repairs failed entries; one normal access preparation then lets
						// the header-based path confirm the still-dirty generation. Access keeps
						// its normal persisted cooldown.
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
					toast(succeeded ? "ok" : "error", succeeded ? t("ui.restore.snapshotHistoryRefreshed") : t("ui.restore.backupDataRefreshFailed"));
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
	// A job-owned File History source is labelled by its job (see
	// jobSourceDisplay.ts). Paths, selections, and restore requests still use
	// the source and native roots returned by the backend.
	const sourceJob = (source: string) => fileHistorySourceJob(source, repository.id, jobs);
	const indexBlocked = Boolean(selected) && (indexing || indexState === null || !indexState.complete);
	useEffect(() => {
		onStatusChange(repository.id, { indexBlocked, searching, forcingMetadata });
	}, [forcingMetadata, indexBlocked, onStatusChange, repository.id, searching]);

	// The entries of each item's dropdowns, built only when the histories or
	// version lists change (or the language, which the lines are written in),
	// not on every render: the panel is rendered twice and re-renders on every
	// keystroke in the destination field, and an item can have hundreds of
	// backups.
	const locale = getEffectiveLocale();
	const backupDateOptions = useMemo(() => Object.fromEntries(Object.entries(histories).map(([key, history]) => [key,
		presentVersions(history).sort(newestVersionFirst)
			.map((version) => ({ value: fileVersionKey(version), lines: backupDateLines(version.timestamp, version, locale, version) }))])),
	[histories, locale]);
	const fileVersionOptions = useMemo(() => Object.fromEntries(Object.entries(versionLists).map(([key, versions]) => [key,
		versions.map((version) => ({ value: fileVersionKey(version), lines: fileVersionLines(version, locale) }))])),
	[versionLists, locale]);

	const commonSnapshots = useMemo(() => {
		if (!selectedResults.length) return [];
		return snapshots.filter((snapshot) =>
			selectedResults.every((result) =>
					presentVersions(histories[resultKey(result)]).filter((version) => version.snapshotId === snapshot.id).length === 1
			)
		).sort(newestSnapshotFirst);
	}, [histories, selectedResults, snapshots]);

    const allHistoriesLoaded = selectedResults.every((result) => histories[resultKey(result)] !== undefined);
    const effectiveSingleSnapshot = commonSnapshots.some((snapshot) => snapshot.id === singleSnapshot)
        ? singleSnapshot
        : commonSnapshots[0]?.id ?? "";
	// Keep the reviewed filesystem route through confirmation and submission;
	// trimming or host normalization would redirect restore writes.
	const effectiveTargetPath = targetPath;
	// An item has file versions once its history has loaded with a non-empty
	// list: the backend gives none for a folder (folders stay per backup
	// date), so this decides which dropdown an item gets in the "File version"
	// tab.
	const hasFileVersions = (key: string) => (versionLists[key]?.length ?? 0) > 0;
	// The backup each item restores from. A file version is restored from the
	// newest backup that holds it: its snapshotId and nativeRootId name one of
	// the item's per-backup entries, which supplies the rest (folder or file,
	// recorded source, backup date) exactly as a chosen backup date does, so
	// the request and every check below are the same for all three tabs.
	const selectedVersion = (result: FileSearchResult) => {
		const key = resultKey(result);
		if (mode === "single") return exactSnapshotVersion(histories[key], effectiveSingleSnapshot);
		return fileVersionForKey(histories[key], mode === "version" && hasFileVersions(key) ? chosenFileVersions[key] : chosenVersions[key]);
	};
	const restoreItems: ReviewedRestoreItem[] = selectedResults.map((result) => {
		const version = selectedVersion(result);
		return { snapshotId: version?.snapshotId ?? "", path: result.path, nativeRootId: version?.nativeRootId ?? "" };
	});
	const restoreSelectionConflict = selectionConflict(selectedResults, selectedVersion);
	// Kopia restores a selected folder's contents straight into the
	// destination, so several items that include a folder would merge there.
	// The backend refuses such a request; this shows the same text before
	// review. The chosen version decides whether an item is a folder.
	const kopiaFolderSelectionConflict = selectedRepository.engine === "kopia" && selectedResults.length > 1 &&
		selectedResults.some((result) => selectedVersion(result)?.isDir ?? result.isDir)
		? t("ui.restore.kopiaFolderSelection")
		: "";
	// Kopia can restore selected files only into a folder that already exists
	// (the backend refuses a missing one), so the destination hint says so
	// whenever the selection includes a file.
	const kopiaSelectionIncludesFile = selectedRepository.engine === "kopia" &&
		selectedResults.some((result) => !(selectedVersion(result)?.isDir ?? result.isDir));
	const canRestore = selectedResults.length > 0 && !restoreSelectionConflict && !kopiaFolderSelectionConflict && Boolean(effectiveTargetPath) && allHistoriesLoaded &&
		restoreItems.every((item) => Boolean(item.nativeRootId)) &&
		(mode === "single" ? Boolean(effectiveSingleSnapshot) : selectedResults.every((result) => Boolean(selectedVersion(result))));
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
			setVersionLists((current) => { const next = { ...current }; delete next[key]; return next; });
			setChosenFileVersions((current) => { const next = { ...current }; delete next[key]; return next; });
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
		setVersionLists((current) => Object.fromEntries(Object.entries(current).filter(([selectedKey]) => !replacedKeys.includes(selectedKey))));
		setChosenFileVersions((current) => Object.fromEntries(Object.entries(current).filter(([selectedKey]) => !replacedKeys.includes(selectedKey))));
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
					// Every page carries the file's complete list of versions,
					// so the last page's list is kept.
					let versions = response.versions;
					while (response.hasMore) {
						if (!Number.isSafeInteger(response.nextOffset) || response.nextOffset <= response.offset) {
							throw new Error(t("ui.findFile.invalidHistoryPageBoundary"));
						}
						response = await getFileHistory(selected, result.path, result.source, response.nextOffset, controller.signal, response.revision);
						if (generation.current !== vaultGeneration || historyRequests.current.get(key) !== controller) return;
						if (!acceptCatalogResponse(response, true)) return;
						items = appendUniqueHistory(items, response.items);
						versions = response.versions;
					}
					if (generation.current !== vaultGeneration || historyRequests.current.get(key) !== controller) return;
					setHistories((current) => ({ ...current, [key]: items }));
					// The API lists the versions grouped by computer, so they
					// are sorted here, once, newest first across computers.
					const fileVersions = [...(versions ?? [])].sort(newestFileVersionFirst);
					setVersionLists((current) => ({ ...current, [key]: fileVersions }));
					// Both choices start on the newest: the newest backup date
					// and, for a file, the version held by the newest backup.
					const latest = newestPresentVersion(items);
					if (latest) setChosenVersions((current) => ({ ...current, [key]: fileVersionKey(latest) }));
					const newestFileVersion = fileVersions[0];
					if (newestFileVersion) setChosenFileVersions((current) => ({ ...current, [key]: fileVersionKey(newestFileVersion) }));
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
		setNameRefusal(null);
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
					// A single-file source shows the name the duplicate check
					// compared (recordedFileRootName).
					path: isFileSourceResult(result) ? restoreLandingPath(result, version) : shownResultPath(result),
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
		restoreSubmissionOwner.current = null;
		setRestoring(false);
		setNameRefusal(null);
		setReviewedRestore(null);
		return true;
	};

	// The restore of the selected items runs in the background once the backend
	// has queued it. The review closes and the dashboard opens the live log for
	// the returned operation, where the per-item results appear when it ends
	// and where it can be cancelled (cold storage included).
    const doRestore = async (items = reviewedRestore?.items) => {
		if (!reviewedRestore || !items) return;
		if (restoreSubmissionOwner.current) return;
		const reviewed = reviewedRestore;
		const owner = { session: restoreReviewSession.current, generation: ++restoreSubmissionGeneration.current };
		restoreSubmissionOwner.current = owner;
		const ownsSubmission = () => restoreReviewSession.current === owner.session &&
			restoreSubmissionGeneration.current === owner.generation && restoreSubmissionOwner.current === owner;
        setRestoring(true);
		setNameRefusal(null);
        try {
			const { operationId } = await restoreSelection({
				repositoryId: reviewed.repositoryId,
				targetPath: reviewed.targetPath,
				items: items.map((item) => ({ ...item })),
				conflictMode: reviewed.conflictMode,
			});
			if (!ownsSubmission()) return;
			setReviewedRestore(null);
			navigate(`/?operation=${encodeURIComponent(operationId)}`);
        } catch (reason) {
			if (!ownsSubmission()) return;
			if (reason instanceof APIError && reason.code === RESTORE_SELECTION_NAME_INVALID) {
				const fileSource = refusedFileSourceVersion(items);
				setNameRefusal({ offer: fileSource ? null : containingFolderRestoreItems(items), fileSource });
				return;
			}
			toast("error", (reason as Error).message);
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
		// A single-file source is a source row that is a file: it has nothing
		// to expand and is selected like any file.
		const fileSource = item.isSource && isFileSourceResult(item);
		const selectable = !item.isSource || fileSource;
		const expandable = (item.isSource && !fileSource) || Boolean(item.canExpand) || item.isDir;
		const checked = selectable && Boolean(selectedItems[resultKey(item)]);
		const childPage = browsePages[key];
		const childError = browseErrors[key];
		const job = sourceJob(item.source);
		const source = job ? job.name : displayPath(item.source);
		const label = item.isSource ? source : item.name;
		// A single-file source row is its own source, so it is announced by
		// its label alone, like a search result.
		const fileSourceSelectionLabel = checked ? t("ui.fileHistory.deselectNamed", { name: label }) : t("ui.fileHistory.selectNamed", { name: label });
		const selectionLabel = fileSource ? fileSourceSelectionLabel : checked
            ? t("ui.fileHistory.deselectFromSource", { item: item.isSource ? label : displayFilePath(item.path, item.source), source })
            : t("ui.fileHistory.selectFromSource", { item: item.isSource ? label : displayFilePath(item.path, item.source), source });
		const disabledMessage = item.isSource ? "" : parentSelectionMessage(item);
		// Source rows of a job show the job name with the "Job source:" tooltip;
		// item rows under it show their path within the job. The browse tree
		// clips its content like the results list, so the tooltip is floating.
		const rowLabel = job
			? <FloatingTooltip content={item.isSource ? jobSourceTooltip(job) : ltrIsolate(pathWithinJob(item.path, item.source))} label={label} className="job-source-label"><strong className="job-source-name">{label}</strong></FloatingTooltip>
			: <strong><bdi dir="ltr">{label}</bdi></strong>;
		return <div className="browse-tree-node" key={key} role="listitem">
			<div className={`browse-tree-row${checked ? " selected" : ""}`} style={{ paddingLeft: 10 + depth * 22 } as CSSProperties}>
				{expandable ? <button type="button" className="browse-disclosure" aria-expanded={expanded} aria-label={expanded ? t("ui.fileHistory.collapseNamed", { name: label }) : t("ui.fileHistory.expandNamed", { name: label })} onClick={() => toggleBrowseNode(item)}>{expanded ? "▾" : "▸"}</button> : <span className="browse-disclosure-spacer" />}
				{!selectable ? <span className="browse-checkbox-spacer" /> : <SelectionCheckbox checked={checked} label={selectionLabel} parentSelectionMessage={disabledMessage} onChange={() => toggleResult(item)} />}
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
				{childPage?.hasMore && <div role="listitem"><button type="button" className="btn browse-load-more" disabled={childPage.loading} onClick={() => void loadBrowsePage(item, true)}>{childPage.loading && <span className="spinner" />}{t("ui.fileHistory.loadMoreIn", { folder: ltrIsolate(label) })}</button></div>}
			</div>}
		</div>;
	});
		// Each item's own dropdown is labelled by its path. A loading item shows
		// the same disabled "Loading history" dropdown in both per-item tabs, because
		// whether it has file versions isn't known until its history arrives.
		const itemPathLabel = (result: FileSearchResult) => <FilePathLabel value={shownResultPath(result)} source={result.source} tooltipPath={displayFullFilePath(result.source, result.path)} withinJob={Boolean(sourceJob(result.source))} fileSource={isFileSourceResult(result)} />;
		const backupDateChooser = (result: FileSearchResult) => {
			const key = resultKey(result);
			const options = backupDateOptions[key] ?? [];
			return <MultilineSelect key={key} className="find-version-row" label={itemPathLabel(result)} value={chosenVersions[key] ?? ""} options={options} onChange={(value) => setChosenVersions((current) => ({ ...current, [key]: value }))} disabled={!options.length} placeholder={options.length ? t("ui.pages.findfile.choose.a.version") : t("ui.pages.findfile.loading.history")} />;
		};
		const fileVersionChooser = (result: FileSearchResult) => {
			const key = resultKey(result);
			const options = fileVersionOptions[key] ?? [];
			return <MultilineSelect key={key} className="find-version-row" label={itemPathLabel(result)} value={chosenFileVersions[key] ?? ""} options={options} onChange={(value) => setChosenFileVersions((current) => ({ ...current, [key]: value }))} placeholder={t("ui.pages.findfile.choose.a.version")} />;
		};
		const restorePanel = selectedResults.length > 0 && <aside className="find-selection-panel">
		<div className="section-heading"><div><h2>{t("ui.pages.findfile.restore.selected")}</h2><p className="section-copy">{t("ui.findFile.restoreModesHelp")}</p></div></div>
		<RestoreModeTabs mode={mode} onChange={setMode}>
			{mode === "single"
				? <MultilineSelect
					label={t("ui.pages.findfile.backup.date.shared.by.all.selected.items")}
					value={effectiveSingleSnapshot}
					options={commonSnapshots.map((snapshot) => ({ value: snapshot.id, lines: backupDateLines(snapshot.timestamp, sharedSnapshotComputer(snapshot.id, selectedResults, histories), locale,
						selectedResults.length === 1 ? presentVersions(histories[resultKey(selectedResults[0])]).find((version) => version.snapshotId === snapshot.id) : undefined) }))}
					onChange={setSingleSnapshot}
					disabled={!allHistoriesLoaded || commonSnapshots.length === 0}
					placeholder={allHistoriesLoaded ? t("ui.pages.findfile.no.unambiguous.common.backup.contains.every.item") : t("ui.pages.findfile.loading.item.history")}
				/>
				: <div className="find-version-list">{selectedResults.map((result) =>
					mode === "version" && hasFileVersions(resultKey(result)) ? fileVersionChooser(result) : backupDateChooser(result))}</div>}
		</RestoreModeTabs>
		<div className="find-selected-items"><span className="modal-section-label">{t("ui.pages.findfile.selected.items")}</span>{selectedResults.map((result) => {
			const key = resultKey(result);
			const version = selectedVersion(result);
			const isDir = version?.isDir ?? result.isDir;
			const name = displayFilePath(shownResultPath(result), result.source);
			// Download fetches the backup this tab restores the item from
			// (selectedVersion), so it follows the tab on screen. It is offered
			// for a file only, never in a Cold Storage vault, and disappears once
			// the chosen backup holds the item as something other than a regular
			// file (a symlink, for example); SnapshotEntry in types.ts says why.
			// Until a backup is chosen, or while the item's history loads, it is
			// shown disabled.
			const downloadable = !repository.coldStorage && !isDir && (version === undefined || version.regularFile);
			const downloadTarget = version?.nativeRootId
				? { repositoryId: repository.id, snapshotId: version.snapshotId, nativeRootId: version.nativeRootId, path: result.path }
				: null;
			return <div className="find-selected-item" key={key}>
				<Icon name={isDir ? "folder" : "file"} size={14} />
				<span style={{ display: "flex", minWidth: 0, flex: "1 1 8em", flexDirection: "column", gap: 3 }}><FilePathLabel value={shownResultPath(result)} source={result.source} tooltipPath={displayFullFilePath(result.source, result.path)} withinJob={Boolean(sourceJob(result.source))} fileSource={isFileSourceResult(result)} />{isDir && <span className="mono faint">{t("ui.pages.findfile.all.files.and.folders.inside")}</span>}</span>
				<span className="mono faint">{versionTypeLabel(version, result.isDir)}</span>
				{downloadable && <FileDownloadButton target={downloadTarget} label={t("ui.download.actionNamed", { name })} />}
				<button onClick={() => toggleResult(result)} aria-label={t("ui.fileHistory.removeNamed", { name })}>{t("ui.pages.findfile.remove")}</button>
			</div>;
		})}</div>
		{restoreSelectionConflict && <div className="inline-error" role="alert">{restoreSelectionConflict}</div>}
		{kopiaFolderSelectionConflict && <div className="inline-error" role="alert">{kopiaFolderSelectionConflict}</div>}
		<label className="field"><span>{t("ui.pages.findfile.restore.destination")}</span><DirectoryField value={effectiveTargetPath} onChange={setTargetPath} placeholder={kopiaSelectionIncludesFile ? t("ui.restore.chooseExistingFolder") : t("ui.pages.findfile.choose.a.new.or.existing.destination")} /></label>
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
								const path = displayFilePath(shownResultPath(result), result.source);
								const selectionLabel = checked ? t("ui.fileHistory.deselectNamed", { name: path }) : t("ui.fileHistory.selectNamed", { name: path });
								return <label key={key} className={`find-result${checked ? " selected" : ""}`}><SelectionCheckbox checked={checked} label={selectionLabel} parentSelectionMessage={parentSelectionMessage(result)} onChange={() => toggleResult(result)} /><Icon name={result.isDir ? "folder" : "file"} size={18} /><span className="find-result-main"><strong><FilePathLabel value={shownResultPath(result)} source={result.source} withinJob={Boolean(sourceJob(result.source))} floating /></strong>{(() => { const job = sourceJob(result.source); return job
									? <span className="mono job-source-line">{t("ui.fileHistory.sourceLabel")} <JobSourceLabel job={job} /></span>
									: <span className="mono">{t("ui.fileHistory.sourceLabel")} <bdi dir="ltr">{sourceLabel(result)}</bdi></span>; })()}</span></label>;
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
                <p className="muted modal-intro">{reviewedRestore.mode === "single" ? t("ui.fileHistory.singleDateHelp") : reviewedRestore.mode === "version" ? t("ui.fileHistory.fileVersionHelp") : t("ui.fileHistory.perItemHelp")}</p>
				<div className="find-confirm-list">{reviewedRestore.rows.map((row) => <div className="find-confirm-row" key={row.key}><Icon name={row.isDir ? "folder" : "file"} size={15} /><span><strong><FilePathLabel value={row.path} /></strong><span className="mono">{formatDate(row.timestamp)} · {row.isDir ? t("ui.findFile.folderType") : t("ui.findFile.fileType")}</span></span></div>)}</div>
                <div className="find-confirm-destination"><span>{t("ui.pages.findfile.destination")}</span><strong className="mono"><bdi dir="ltr">{displayPath(reviewedRestore.targetPath)}</bdi></strong></div>
				{(restoreCapability(selectedRepository, engines)?.conflictModes.length ?? 0) <= 1
					? <p className="muted">{restoreCapability(selectedRepository, engines)?.conflictModes[0]?.description}</p>
					: <label className="field"><span>{t("ui.pages.findfile.should.existing.files.be.overwritten")}</span><select value={reviewedRestore.conflictMode} onChange={(event) => setReviewedRestore((current) => current ? { ...current, conflictMode: event.target.value } : current)}>{restoreCapability(selectedRepository, engines)?.conflictModes.map((mode) => <option key={mode.id} value={mode.id}>{restoreConflictModeLabel(mode)}</option>)}</select></label>}
				</fieldset>
				{nameRefusal && <div className="inline-error" role="alert">{nameRefusal.fileSource ? t("ui.restore.selectionNameInvalidSingleFile") : t("ui.restore.selectionNameInvalid")}{nameRefusal.offer && <> <button type="button" className="btn" disabled={restoring} onClick={() => void doRestore(nameRefusal.offer?.items)}>{t("ui.restore.restoreContainingFolder")}</button>{nameRefusal.offer.folders.map((folder) => <span key={folder}> <strong><FilePathLabel value={folder} /></strong></span>)}</>}</div>}
				<div className="modal-footer"><button className="btn" disabled={restoring} onClick={dismissReviewedRestore}>{t("ui.findFile.back")}</button><button className="btn primary" disabled={restoring} onClick={() => void doRestore()}>{restoring && <span className="spinner" />}<Icon name="restore" size={14} />{t("ui.pages.findfile.restore.selected")}</button></div>
			</Modal>}

		</>
	    );
}
