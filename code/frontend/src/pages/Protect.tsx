import { useCallback, useEffect, useRef, useState } from "react";
import { Link } from "react-router-dom";

import { DirectoryField } from "../components/DirectoryPicker";
import { BackupRunPicker } from "../components/BackupRunPicker";
import { ListControls } from "../components/ListControls";
import { RcloneAuthorization } from "../components/RcloneAuthorization";
import { JobScriptFields } from "../components/JobScriptFields";
import { preventNumberInputWheel } from "../components/numberInput";
import {
    ConfirmDialog,
    EmptyState,
    Icon,
    Loading,
    Modal,
    SaveChangesDialog,
    scheduleLabel,
    timeAgo,
    Tooltip,
    useToast,
} from "../components/ui";
import {
		checkRepository,
		changeVaultPassword,
		getVaultPasswordChangeStatus,
		retryVaultPasswordChange,
		closeRcloneAuthorization,
		createJob,
		createRepository,
		connectExistingVault,
		deleteRepositoryCreationIntent,
		cancelRepositoryConnectionIntent,
	    deleteJob,
    deleteRepository,
    getIntegrations,
    getEngines,
    getSettings,
    getJobStatus,
    getJobs,
	getOperation,
	getActiveOperations,
		getRepository,
	getRepositoryReconnectFields,
	getVaultOwnership,
	getRepositories,
	getVaultSizeStatus,
	prepareVaultSize,
	getVaultProfileSyncStatuses,
	forceVaultOwnershipTakeover,
	getRepositoryConnectionIntents,
	getRepositoryCreationIntents,
	getDormantRecoveryJobs,
    runJob,
    runMaintenance,
		previewExistingVault,
		selectExistingVaultProfile,
	retryVaultProfileSync,
	retryExistingVaultConnection,
		restoreDormantRecoveryJob,
	discardDormantRecoveryJob,
	setJobEnabled,
		updateJob,
		updateRepositorySchedules,
	} from "../services/api";
import { APIError } from "../services/api";
import type { ExistingVaultStorageInput, JobInput, ManualTargetAdmissionResult, RcloneAuthStatus, RepositoryConnectionIntent, RepositoryCreationIntent, VaultOwnershipStatus, VaultPasswordChangeResult, VaultProfileSyncStatus, VaultProgressRecord } from "../services/api";
import { backupTargetIsActive } from "../services/backupJobs";
import { validVaultPassword } from "../services/vaultPassword";
import type {
    BackupJob,
    IntegrationOption,
    Repository,
    StorageIntegration,
    EngineDescriptor,
	ExistingVaultPreview,
	DormantRecoveryJob,
	VaultSizeStatus,
	ObjectLockSettings,
} from "../types";

import { formatNativeLogText } from "./nativeLogFormat";

// Restic password changes use `key passwd`, which updates the selected key in
// place without creating another key that retains the prior password. Keys
// created independently by other software are outside this operation.
const vaultPasswordChangeNotice = (result: VaultPasswordChangeResult) => [
	result.message,
	`Native ${result.native.engine} result: ${result.native.status || "unresolved"}${result.native.output ? ` — ${formatNativeLogText(result.native.engine, "password_change", result.native.output)}` : ""}`,
].filter(Boolean).join(" ");

const vaultPasswordChangeNoticeKind = (result: VaultPasswordChangeResult) =>
	result.native.status === "succeeded" && !result.cleanupPending ? "ok" as const : "info" as const;

type RunSubmissionOwner = {
	session: number;
	generation: number;
	jobId: string;
	phase: "run";
};

type VaultOwnershipPresentation = "checking" | "owner" | "nonowner" | "unverified";
type VaultWorkState = "checking" | "idle" | "running" | "unavailable";
type ActiveBackupTarget = { jobId: string; repositoryId: string; operationId: string; status: string };
type ObservedBackupOperation = { jobId: string; repositoryId: string };
type ObservedOperationReadOwner = { generation: number; repositoryId: string; repositoryEpoch: number };

const JOB_PAGE_SIZE = 5;
const VAULT_PAGE_SIZE = 6;
const JOB_PAGE_SIZE_OPTIONS = [JOB_PAGE_SIZE, 10, 20, 50] as const;
const VAULT_PAGE_SIZE_OPTIONS = [VAULT_PAGE_SIZE, 9, 18, 36] as const;
const JOB_PAGE_SIZE_STORAGE_KEY = "replicaro.protect.jobs.pageSize";
const VAULT_PAGE_SIZE_STORAGE_KEY = "replicaro.protect.vaults.pageSize";
const JOB_SORT_STORAGE_KEY = "replicaro.protect.jobs.sort";
const VAULT_SORT_STORAGE_KEY = "replicaro.protect.vaults.sort";
const MAX_VAULT_NAME_CODE_POINTS = 50;

function manualRunResultText(result: ManualTargetAdmissionResult) {
	switch (result.status) {
		case "admitted":
			return result.operationId ? `Admitted · operation ${result.operationId}` : "Admitted";
		case "storage_unavailable":
			return `Storage unavailable${result.reasonCode ? ` · ${result.reasonCode}` : ""}`;
		case "policy_not_ready":
			return `Policy not ready${result.reasonCode ? ` · ${result.reasonCode}` : ""}`;
		case "busy":
			return "Busy";
	}
}

function truncateCodePoints(value: string, maximum: number) {
	return Array.from(value).slice(0, maximum).join("");
}

function validVaultName(value: string) {
	const name = value.trim();
	return name !== "" && unicodeCodePointCount(name) <= MAX_VAULT_NAME_CODE_POINTS;
}

function rcloneLifecycleFailure(error: unknown, pendingConnection = false) {
	const failure = error as Error & {
		activation?: { disposition?: string };
		attached?: boolean;
		usable?: boolean;
	};
	const disposition = failure.activation?.disposition;
	if (disposition !== "activated" && disposition !== "indeterminate") {
		return { consumed: false, completed: false, message: failure.message };
	}
	if (failure.attached === true && failure.usable === true) {
		return {
			consumed: true,
			completed: true,
			message: `${failure.message} The vault is saved and usable; only a local follow-up or temporary authorization cleanup failed. Refresh to review its current state.`,
		};
	}
	return {
		consumed: true,
		completed: false,
		// Activation consumes the login session but keeps the vault-owned config.
		// A pending connection can retry that config before another login.
		message: `${failure.message} Native authorization was ${disposition}; attached=${Boolean(failure.attached)}, usable=${failure.usable === undefined ? "not assessed" : String(failure.usable)}. ${pendingConnection ? "Retry the pending connection. Replicaro will request rclone authorization if the saved configuration is unavailable." : "Reauthorize before retrying."}`,
	};
}

function availableImportedVaultName(baseName: string, vaultID: string, repositories: Repository[] | null) {
	const base = truncateCodePoints(baseName.trim(), MAX_VAULT_NAME_CODE_POINTS);
	const used = new Set((repositories ?? [])
		.filter((repository) => repository.id !== vaultID)
		.map((repository) => repository.name.trim().toLowerCase()));
	let candidate = base;
	for (let suffix = 1; used.has(candidate.toLowerCase()); suffix++) {
		const ending = ` ${suffix}`;
		candidate = `${truncateCodePoints(base, MAX_VAULT_NAME_CODE_POINTS - unicodeCodePointCount(ending))}${ending}`;
	}
	return candidate;
}
const MAX_CUSTOM_SCHEDULE_MINUTES = 153_722_867;
const EXAMPLE_PREFIX = "example: ";
const integrityCheckHelp = "Checks the integrity of the snapshots stored in this vault. The whole vault is downloaded and tested for corruption.";
const coldStorageIntegrityHelp = "Replicaro disables integrity checks for all cold storage vaults to avoid you incurring excessive costs. Use hot storage if you need integrity checks.";
const connectAccessReminder = "Remember to disable other software from accessing this vault. Only Replicaro should now be using this vault.";
const profileDateLabel = (value: string) => {
	const date = new Date(value);
	return Number.isNaN(date.getTime()) ? "Unknown date" : date.toLocaleString(undefined, {
		month: "short",
		day: "numeric",
		year: "numeric",
		hour: "numeric",
		minute: "2-digit",
	});
};
const nextSnapshotTooltip = (job: BackupJob) => {
	if (!job.enabled) return "This job is disabled, so no snapshot is scheduled.";
	if (job.schedule === "manual") return "This job runs manually, so no snapshot is scheduled.";
	const relative = timeAgo(job.nextRun);
	if (relative === "—") return "The next snapshot time is unavailable.";
	return relative.startsWith("in ") ? `Next snapshot ${relative}.` : "The next snapshot is due now.";
};
const maintenanceHelp = "Runs maintenance to compact the vault and reclaim space from snapshots that have been marked for deletion.";
const objectLockMaintenanceHelp = "Replicaro requires the space reclamation schedule to run at least one day sooner than the object lock duration. Maintenance options that do not meet this requirement are unavailable, and Replicaro automatically selects the longest eligible schedule when necessary.";
const pausedObjectLockMaintenanceHelp = "Manual is available while object lock is paused. When object lock is resumed, Replicaro automatically selects the longest eligible schedule if the current choice is unavailable.";
const objectLockForwardHelp = "These settings apply to new snapshots. Existing locks are never shortened or removed. 2 days is the minimum allowed duration.";
const objectLockTransitionHelp = "After enrollment, the duration can only be increased. S3 Governance can be changed to Compliance, but Compliance cannot be changed to Governance.";

const emptyObjectLock = (): ObjectLockSettings => ({
	enrolled: false, paused: false, mode: "", durationValue: 0, durationUnit: "",
});

function objectLockEligible(engine: string, connector: string) {
	return engine === "kopia" && ["s3", "azblob", "gcs"].includes(connector);
}

function objectLockForSelection(current: ObjectLockSettings, engine: string, connector: string) {
	if (!objectLockEligible(engine, connector)) return emptyObjectLock();
	if (!current.enrolled) return emptyObjectLock();
	return { ...current, mode: connector === "s3" && current.mode === "governance" ? "governance" : "compliance" } as ObjectLockSettings;
}

function objectLockDurationHours(settings: ObjectLockSettings) {
	const hours = { days: 24, weeks: 7 * 24, months: 30 * 24, years: 365 * 24 }[settings.durationUnit || "days"];
	return settings.durationValue * hours;
}

function objectLockScheduleEligible(settings: ObjectLockSettings, schedule: string) {
	if (!settings.enrolled) return true;
	if (settings.paused && schedule === "manual") return true;
	const interval = { daily: 24, weekly: 7 * 24, monthly: 31 * 24 }[schedule as "daily" | "weekly" | "monthly"];
	if (!interval) return false;
	return settings.paused || objectLockDurationHours(settings) - interval >= 24;
}

function longestEligibleObjectLockSchedule(settings: ObjectLockSettings) {
	return ["monthly", "weekly", "daily"].find((schedule) => objectLockScheduleEligible(settings, schedule)) ?? "daily";
}

function validObjectLockSettings(settings: ObjectLockSettings, schedule: string, allowPaused = false, original?: ObjectLockSettings) {
	if (!settings.enrolled) return true;
	if (settings.paused && !allowPaused) return false;
	if (!settings.mode || !Number.isInteger(settings.durationValue) ||
		objectLockDurationHours(settings) < 48 || !objectLockScheduleEligible(settings, schedule)) return false;
	if (original?.enrolled) {
		const configurationChanged = settings.mode !== original.mode || settings.durationValue !== original.durationValue ||
			settings.durationUnit !== original.durationUnit;
		if (settings.paused && configurationChanged) return false;
		if (objectLockDurationHours(settings) < objectLockDurationHours(original)) return false;
		if (original.mode === "compliance" && settings.mode === "governance") return false;
	}
	return true;
}
const coldStorageProviderGuidance = [
	"Cold storage means retrieval is not instant; restores can take hours or days or longer. Replicaro places metadata (including the vault.replicaro object) in hot storage. The size of metadata is usually very small, so the cost of using hot storage for metadata is usually minimal. Metadata must remain in hot storage to ensure backup jobs complete successfully. Data packs, which use the bulk of storage space, are put into cold storage.",
	"If you have bucket lifecycle rules set up at your provider, it is highly recommended to disable those rules so that they do not accidentally move metadata to cold storage. Backups will break if metadata is put in cold storage.",
	`Each cold storage provider has its own costs related to access, retrieval, and deletion of data in cold storage. ${coldStorageIntegrityHelp} Space reclamation remains set to the default "daily"; if you want to disable space reclamation, set it to "manual" from Advanced Settings below.`,
];
const coldStorageArchiveClassHelp = "This choice cannot be changed once the vault is created. It is recommended to leave this as GLACIER instead of DEEP_ARCHIVE because most non-AWS S3-compatible providers support GLACIER but not DEEP_ARCHIVE. If you use DEEP_ARCHIVE and your provider does not support it, backups will fail on the first run. If that happens, remove the vault from Replicaro, manually delete the Replicaro vault data from the remote location (Replicaro does not delete remote data when a vault is removed, so you need to do it manually), and then create a new cold storage vault that leaves it at the GLACIER storage class.";
const coldStorageCancellationGuidance = "Cancellation or shutdown stops the local Restic process, but provider restore requests may continue.";
const vaultPasswordHelp = "Your password is never stored in the vault, and the password never leaves your computer. Only you know your password. Do not forget your password because it cannot be recovered or reset if you forget.";

function normalizedRcloneFolderName(value: string) {
	return value.trim().normalize("NFC");
}

function usesRcloneNativeLogin(provider: string) {
	// These account-backed rclone connectors use backend-enforced sole-profile
	// admission. Connect must reuse or transfer that one attachment instead of
	// offering Join or another profile that would imply unsupported coordination.
	return provider === "dropbox" || provider === "google_drive" || provider === "onedrive";
}

const vaultStorageOrder = ["fs", "s3", "azblob", "gcs", "sftp", "dropbox", "google_drive", "onedrive"];
const hiddenVaultOptionKeys = new Set([
	"sse_customer_key",
	"ssh_private_key",
	"ssh_private_key_ttl",
	"credentials_json",
]);

function orderVaultStorageIntegrations(integrations: StorageIntegration[]) {
	return [...integrations].sort((left, right) => {
		const leftIndex = vaultStorageOrder.indexOf(left.id);
		const rightIndex = vaultStorageOrder.indexOf(right.id);
		return (leftIndex < 0 ? vaultStorageOrder.length : leftIndex) -
			(rightIndex < 0 ? vaultStorageOrder.length : rightIndex);
	});
}

function visibleVaultOptions(options: IntegrationOption[], connector = "") {
	// Vault setup intentionally supports key-based SFTP authentication only.
	// Keep the backend's Kopia password capability available for compatibility,
	// but never present or submit that credential through create or connect.
	return options.filter((option) =>
		!hiddenVaultOptionKeys.has(option.key) &&
		!(connector === "sftp" && option.key === "password"));
}

function providerPresentationDescription(
	engineCatalog: EngineDescriptor[],
	connector: string,
	fallback: string,
	engine?: string,
) {
	if (!engine) return fallback;
	return engineCatalog
		.filter((descriptor) => descriptor.id === engine)
		.flatMap((descriptor) => descriptor.providers)
		.find((provider) => provider.id === connector && provider.supported)
		?.description || fallback;
}

// Certification status must never be shown in the end-user UI. Runtime
// admission remains authoritative for whether an engine/provider is available.

function unicodeCodePointCount(value: string) {
	return Array.from(value).length;
}

function examplePlaceholder(value?: string) {
	if (!value || value.startsWith(EXAMPLE_PREFIX)) return value;
	return `${EXAMPLE_PREFIX}${value}`;
}

type JobSort = "newest" | "oldest" | "name-asc" | "name-desc";
type VaultSort = "newest" | "oldest" | "size" | "name-asc" | "name-desc";

const JOB_SORT_OPTIONS: ReadonlyArray<{ value: JobSort; label: string }> = [
    { value: "newest", label: "Newest (last run)" },
    { value: "oldest", label: "Oldest (last run)" },
    { value: "name-asc", label: "Name A–Z" },
    { value: "name-desc", label: "Name Z–A" },
];

const VAULT_SORT_OPTIONS: ReadonlyArray<{ value: VaultSort; label: string }> = [
    { value: "newest", label: "Newest" },
    { value: "oldest", label: "Oldest" },
    { value: "size", label: "Size (largest)" },
    { value: "name-asc", label: "Name A–Z" },
    { value: "name-desc", label: "Name Z–A" },
];

function readStoredPageSize<T extends number>(key: string, options: readonly T[], fallback: T): T {
    if (typeof window === "undefined") return fallback;
    try {
        const stored = Number(window.localStorage.getItem(key));
        return options.includes(stored as T) ? stored as T : fallback;
    } catch {
        return fallback;
    }
}

function readStoredOption<T extends string>(key: string, options: readonly T[], fallback: T): T {
    if (typeof window === "undefined") return fallback;
    try {
        const stored = window.localStorage.getItem(key);
        return stored && options.includes(stored as T) ? stored as T : fallback;
    } catch {
        return fallback;
    }
}

function writeStoredPreference(key: string, value: string | number) {
    try {
        window.localStorage.setItem(key, String(value));
    } catch {
        // Storage can be unavailable in private browsing or restricted webviews.
    }
}

function compareDates(left: string, right: string, descending: boolean) {
    const leftTime = Date.parse(left);
    const rightTime = Date.parse(right);
    if (!Number.isFinite(leftTime)) return Number.isFinite(rightTime) ? 1 : 0;
    if (!Number.isFinite(rightTime)) return -1;
    return descending ? rightTime - leftTime : leftTime - rightTime;
}

function compareNames(left: string, right: string) {
    return left.localeCompare(right, undefined, { numeric: true, sensitivity: "base" });
}

function compareVaultSize(left: Repository, right: Repository) {
	if (left.vaultSizeBytes == null) return right.vaultSizeBytes == null ? 0 : 1;
	if (right.vaultSizeBytes == null) return -1;
	return right.vaultSizeBytes - left.vaultSizeBytes;
}

function sortJobs(jobs: BackupJob[], sort: JobSort) {
    return jobs
        .map((job, index) => ({ job, index }))
        .sort((left, right) => {
            const result = sort === "newest"
                ? compareDates(left.job.lastRun, right.job.lastRun, true)
                : sort === "oldest"
                    ? compareDates(left.job.lastRun, right.job.lastRun, false)
                    : sort === "name-asc"
                        ? compareNames(left.job.name, right.job.name)
                        : compareNames(right.job.name, left.job.name);
            return result || left.index - right.index;
        })
        .map(({ job }) => job);
}

function sortVaults(repositories: Repository[], sort: VaultSort) {
    return repositories
        .map((repository, index) => ({ repository, index }))
        .sort((left, right) => {
            let result: number;
            if (sort === "newest") result = compareDates(left.repository.createdAt, right.repository.createdAt, true);
            else if (sort === "oldest") result = compareDates(left.repository.createdAt, right.repository.createdAt, false);
            else if (sort === "size") result = compareVaultSize(left.repository, right.repository);
            else if (sort === "name-asc") result = compareNames(left.repository.name, right.repository.name);
            else result = compareNames(right.repository.name, left.repository.name);
            return result || left.index - right.index;
        })
        .map(({ repository }) => repository);
}

const customScheduleUnits: Record<string, { label: string; minutes: number; max: number } | undefined> = {
	custom: { label: "Minutes", minutes: 1, max: MAX_CUSTOM_SCHEDULE_MINUTES },
	"custom-hours": { label: "Hours", minutes: 60, max: Math.floor(MAX_CUSTOM_SCHEDULE_MINUTES / 60) },
	"custom-days": { label: "Days", minutes: 1440, max: Math.floor(MAX_CUSTOM_SCHEDULE_MINUTES / 1440) },
	"custom-weeks": { label: "Weeks", minutes: 10080, max: Math.floor(MAX_CUSTOM_SCHEDULE_MINUTES / 10080) },
	"custom-months": { label: "Months", minutes: 0, max: Math.floor(MAX_CUSTOM_SCHEDULE_MINUTES / (31 * 1440)) },
};

function customScheduleError(form: { schedule: string; customInterval: string }) {
	const unit = customScheduleUnits[form.schedule];
	if (!unit) return null;
	const count = Number(form.customInterval);
	if (/^\d+$/.test(form.customInterval) && Number.isSafeInteger(count) && count >= 1 && count <= unit.max) return null;
	return `Custom schedule must be a whole number from 1 to ${unit.max.toLocaleString("en-US")} ${unit.label.toLowerCase()}.`;
}

const MAX_CRON_EXPRESSION_LENGTH = 256;
const cronScheduleHelp = "This is cron-style scheduling. If you do not know what cron is, you should not use this schedule option. Cron schedules use this computer’s local time.";

function normalizedCronExpression(value: string) {
	const expression = value.trim().split(/\s+/).filter(Boolean).join(" ");
	return expression.length > 0 && expression.length <= MAX_CRON_EXPRESSION_LENGTH && expression.split(" ").length === 5
		? expression
		: null;
}

function scheduleValue(form: { schedule: string; customInterval: string; cronExpression: string }) {
	if (form.schedule === "custom") return `every:${form.customInterval}`;
	if (form.schedule === "custom-months") return `every-months:${form.customInterval}`;
	const unit = customScheduleUnits[form.schedule];
	if (unit) return `every:${Number(form.customInterval) * unit.minutes}`;
	if (form.schedule !== "cron") return form.schedule;
	const expression = normalizedCronExpression(form.cronExpression);
	return expression == null ? null : `cron:${expression}`;
}

function scheduleHelp(schedule: string) {
	return schedule === "manual"
		? "Replicaro will only run this job when you start it manually."
		: "Replicaro will automatically run this job based on your selected schedule.";
}

const schedulePresets = [
    ["manual", "Manual only"],
    ["hourly", "Every hour"],
    ["daily", "Every day"],
    ["weekly", "Every week"],
    ["monthly", "Every month"],
    ["custom", "Every N minutes…"],
	["custom-hours", "Every N hours…"],
	["custom-days", "Every N days…"],
	["custom-weeks", "Every N weeks…"],
	["custom-months", "Every N months…"],
	["cron", "Cron-style Scheduling"],
] as const;

const retentionPresets = [
	["0", "Keep all snapshots"],
	["10", "Keep 10 latest snapshots"],
	["50", "Keep 50 latest snapshots"],
	["100", "Keep 100 latest snapshots"],
	["1000", "Keep 1,000 latest snapshots"],
	["custom", "Keep N latest snapshots..."],
] as const;

const fixedRetentionValues = new Set<string>(retentionPresets.slice(0, -1).map(([value]) => value));
const MAX_MAIN_RETENTION_COUNT = Number.MAX_SAFE_INTEGER;
const MAX_RETENTION_COUNT = 2_147_483_647;

function retentionPreset(value: string) {
	return fixedRetentionValues.has(value) ? value : "custom";
}

function parsedRetention(value: string, optional = false) {
	if (optional && value.trim() === "") return undefined;
	const count = Number(value);
	const maximum = optional ? MAX_RETENTION_COUNT : MAX_MAIN_RETENTION_COUNT;
	return Number.isSafeInteger(count) && count >= (optional ? 1 : 0) && count <= maximum ? count : null;
}

const careSchedules = [
    ["manual", "Manual only"],
    ["daily", "Daily"],
    ["weekly", "Weekly"],
    ["monthly", "Monthly"],
] as const;

interface JobForm {
    name: string;
    source: string;
    repositoryIds: string[];
    schedule: string;
    customInterval: string;
	cronExpression: string;
    retention: string;
	retentionHourly: string;
	retentionDaily: string;
	retentionWeekly: string;
	retentionMonthly: string;
	retentionYearly: string;
    excludes: string;
    tag: string;
    engineOptions: Record<string, string>;
    enabled: boolean;
	beforeScriptPath: string;
	beforeScriptMustSucceed: boolean;
	afterScriptPath: string;
	afterScriptMustSucceed: boolean;
}

type JobMutationResultStatus = "created" | "updated" | "unchanged" | "warning" | "failed";

interface JobMutationResult {
	jobId?: string;
	name: string;
	source?: string;
	status: JobMutationResultStatus;
	message: string;
	payload?: JobInput;
	enabled?: boolean;
}

interface BulkEditForm extends JobForm {
	changeSchedule: boolean;
	changeRetention: boolean;
	changeExcludes: boolean;
	changeTag: boolean;
	changeScripts: boolean;
	replaceDestinations: boolean;
}

type BulkResultAction = "edit" | "enable" | "disable";

interface BulkResultView {
	action: BulkResultAction;
	items: JobMutationResult[];
}

interface VaultForm {
    engine: "restic" | "kopia";
    name: string;
    connector: string;
	coldStorage: boolean;
	archiveWriteClass: "DEEP_ARCHIVE" | "GLACIER";
    location: string;
    pendingLocation: string;
    bucket: string;
    container: string;
    prefix: string;
    host: string;
    description: string;
    password: string;
    passwordConfirmation: string;
    options: Record<string, string>;
    checkSchedule: string;
    maintenanceSchedule: string;
	concurrencyMode: "reduced" | "native" | "increased" | "maximum";
	objectLock: ObjectLockSettings;
}

const emptyJob: JobForm = {
    name: "",
    source: "",
    repositoryIds: [],
    schedule: "daily",
    customInterval: "30",
	cronExpression: "0 2 * * *",
    retention: "100",
	retentionHourly: "",
	retentionDaily: "",
	retentionWeekly: "",
	retentionMonthly: "",
	retentionYearly: "",
    excludes: "",
    tag: "",
    engineOptions: {},
    enabled: true,
	beforeScriptPath: "",
	beforeScriptMustSucceed: false,
	afterScriptPath: "",
	afterScriptMustSucceed: false,
};

const emptyBulkEdit: BulkEditForm = {
	...emptyJob,
	changeSchedule: false,
	changeRetention: false,
	changeExcludes: false,
	changeTag: false,
	changeScripts: false,
	replaceDestinations: false,
};

function jobToForm(job: BackupJob): JobForm {
    const custom = job.schedule.startsWith("every:");
	const cron = job.schedule.startsWith("cron:");
	const months = job.schedule.startsWith("every-months:");
    return {
        name: job.name,
        source: job.source,
        repositoryIds: job.targets.map((target) => target.repositoryId),
        schedule: custom ? "custom" : months ? "custom-months" : cron ? "cron" : job.schedule,
        customInterval: custom ? job.schedule.slice(6) : months ? job.schedule.slice(13) : "30",
		cronExpression: cron ? job.schedule.slice(5) : "0 2 * * *",
        retention: String(job.retention ?? 100),
		retentionHourly: job.retentionHourly == null ? "" : String(job.retentionHourly),
		retentionDaily: job.retentionDaily == null ? "" : String(job.retentionDaily),
		retentionWeekly: job.retentionWeekly == null ? "" : String(job.retentionWeekly),
		retentionMonthly: job.retentionMonthly == null ? "" : String(job.retentionMonthly),
		retentionYearly: job.retentionYearly == null ? "" : String(job.retentionYearly),
        excludes: job.excludes ?? "",
        tag: job.tag ?? "",
        engineOptions: Object.fromEntries(Object.entries(job.engineSettings ?? {}).map(([id, value]) => [id, (value.additionalOptions ?? []).join("\n")])),
        enabled: job.enabled,
		beforeScriptPath: job.beforeScriptPath ?? "",
		beforeScriptMustSucceed: job.beforeScriptMustSucceed ?? false,
		afterScriptPath: job.afterScriptPath ?? "",
		afterScriptMustSucceed: job.afterScriptMustSucceed ?? false,
    };
}

function asciiNoCase(value: string) {
	return value.replace(/[A-Z]/g, (letter) => letter.toLowerCase());
}

function alphabeticSuffix(index: number) {
	let value = index + 1;
	let suffix = "";
	while (value > 0) {
		value--;
		suffix = String.fromCharCode(97 + (value % 26)) + suffix;
		value = Math.floor(value / 26);
	}
	return suffix;
}

function generatedJobSourceDrafts(name: string, sources: string[], jobs: BackupJob[]) {
	const baseName = name.trim();
	const usedNames = new Set(jobs.map((job) => asciiNoCase(job.name.trim())));
	usedNames.add(asciiNoCase(baseName));
	return sources.map((source, index) => {
		if (index === 0) return { name: baseName, source };
		const numberedName = `${baseName} #${index}`;
		let candidate = numberedName;
		for (let suffix = 0; usedNames.has(asciiNoCase(candidate)); suffix++) {
			candidate = `${numberedName}${alphabeticSuffix(suffix)}`;
		}
		usedNames.add(asciiNoCase(candidate));
		return { name: candidate, source };
	});
}

function parsedEngineOptions(value: string) {
	return value.split("\n").map((option) => option.trim()).filter(Boolean);
}

function retentionReviewSummary(form: Pick<JobForm, "retention" | "retentionHourly" | "retentionDaily" | "retentionWeekly" | "retentionMonthly" | "retentionYearly">) {
	const tiers = [
		["hourly", form.retentionHourly],
		["daily", form.retentionDaily],
		["weekly", form.retentionWeekly],
		["monthly", form.retentionMonthly],
		["yearly", form.retentionYearly],
	].filter(([, value]) => Boolean(value));
	if (Number(form.retention) === 0) {
		return tiers.length === 0
			? "Keep all snapshots. No tier values are configured."
			: `Keep all snapshots. Configured tier values are inactive while Keep all snapshots is selected: ${tiers.map(([tier, value]) => `${tier} ${value}`).join(", ")}.`;
	}
	return `Keep ${form.retention} latest snapshots; ${["hourly", "daily", "weekly", "monthly", "yearly"].map((tier, index) => `${tier} ${[form.retentionHourly, form.retentionDaily, form.retentionWeekly, form.retentionMonthly, form.retentionYearly][index] || "off"}`).join(", ")}.`;
}

function clonedEngineSettings(job: BackupJob) {
	return Object.fromEntries(Object.entries(job.engineSettings ?? {}).map(([engine, settings]) => [engine, {
		...settings,
		additionalOptions: [...(settings.additionalOptions ?? [])],
	}]));
}

function jobInputFromStored(job: BackupJob): JobInput {
	return {
		id: job.id,
		name: job.name,
		source: job.source,
		repositoryIds: job.targets.map((target) => target.repositoryId),
		schedule: job.schedule,
		retention: job.retention,
		retentionHourly: job.retentionHourly,
		retentionDaily: job.retentionDaily,
		retentionWeekly: job.retentionWeekly,
		retentionMonthly: job.retentionMonthly,
		retentionYearly: job.retentionYearly,
		excludes: job.excludes,
		tag: job.tag,
		beforeScriptPath: job.beforeScriptPath,
		beforeScriptMustSucceed: job.beforeScriptMustSucceed,
		afterScriptPath: job.afterScriptPath,
		afterScriptMustSucceed: job.afterScriptMustSucceed,
		engineSettings: clonedEngineSettings(job),
	};
}

function sameStringSet(left: string[], right: string[]) {
	const sortedLeft = [...left].sort();
	const sortedRight = [...right].sort();
	return sortedLeft.length === sortedRight.length && sortedLeft.every((value, index) => value === sortedRight[index]);
}

function suggestedCopyJobName(job: BackupJob, jobs: BackupJob[]) {
	// SQLite NOCASE folds only ASCII letters. Match the database's uniqueness
	// authority deterministically instead of depending on the browser locale.
	const used = new Set(jobs.map((candidate) => asciiNoCase(candidate.name.trim())));
	const base = `${job.name.trim()} copy`;
	if (!used.has(asciiNoCase(base))) return base;
	for (let suffix = 2; ; suffix++) {
		const candidate = `${base} ${suffix}`;
		if (!used.has(asciiNoCase(candidate))) return candidate;
	}
}

const integrationDefaults = (integration?: StorageIntegration) =>
    Object.fromEntries(
        (integration?.options ?? [])
            .filter((option) => option.default !== undefined)
            .map((option) => [option.key, option.default ?? ""])
    );

function integrationOptionsForEngine(
	integration: StorageIntegration | undefined,
	engine: VaultForm["engine"],
	descriptors: EngineDescriptor[],
	current: Record<string, string> = {},
) {
	const provider = descriptors.find((descriptor) => descriptor.id === engine)?.providers
		.find((candidate) => candidate.id === integration?.id && candidate.supported);
	const fields = new Set(provider?.fields ?? []);
	const supported = visibleVaultOptions((integration?.options ?? []).filter((option) => fields.has(option.key)), integration?.id ?? "");
	const result = integrationDefaults(integration ? { ...integration, options: supported } : undefined);
	for (const option of supported) {
		if (current[option.key] !== undefined) result[option.key] = current[option.key];
	}
	return result;
}

function connectorOptionsWithoutUnchangedDefaults(integration: StorageIntegration, options: Record<string, string>) {
	const defaults = new Map(integration.options
		.filter((option) => option.default !== undefined)
		.map((option) => [option.key, option.default ?? ""]));
	return Object.fromEntries(Object.entries(options).filter(([key, value]) => defaults.get(key) !== value));
}

function connectorOptionsForSubmission(form: VaultForm, integration: StorageIntegration | undefined) {
	const presentedKeys = new Set(visibleVaultOptions(integration?.options ?? [], form.connector).map((option) => option.key));
	// Options removed from the UI must not silently reach a backend adapter or
	// native engine, including stale values restored from a pending intent.
	return Object.fromEntries(Object.entries(form.options)
		.filter(([key]) => presentedKeys.has(key)));
}

function supportedIntegrations(engine: VaultForm["engine"], descriptors: EngineDescriptor[], integrations: StorageIntegration[]) {
    const descriptor = descriptors.find((item) => item.id === engine);
    const providers = new Map((descriptor?.providers ?? []).filter((provider) => provider.supported).map((provider) => [provider.id, provider]));
    return integrations.filter((integration) => providers.has(integration.id)).map((integration) => {
        const fields = new Set(providers.get(integration.id)?.fields ?? []);
        return { ...integration, options: visibleVaultOptions(integration.options.filter((option) => fields.has(option.key)), integration.id) };
    });
}

function orderedConnectorOptions(integration: StorageIntegration | undefined, connector: string) {
	const priorities: Record<string, string[]> = {
		s3: ["endpoint", "access_key", "secret_access_key"],
		sftp: ["port", "username", "identity", "ssh_private_key", "ssh_auth_sock"],
	};
	const order = priorities[connector] ?? [];
	return [...(integration?.options ?? [])].sort((left, right) => {
		const leftIndex = order.indexOf(left.key);
		const rightIndex = order.indexOf(right.key);
		if (leftIndex < 0 && rightIndex < 0) return 0;
		if (leftIndex < 0) return 1;
		if (rightIndex < 0) return -1;
		return leftIndex - rightIndex;
	});
}

function missingRequiredConnectorOptions(integration: StorageIntegration | undefined, options: Record<string, string>) {
	return (integration?.options ?? []).filter((option) => option.required && !String(options[option.key] ?? "").trim());
}

function repositoryConnectorLabel(repository: Pick<Repository, "connector" | "connectorLabel" | "location" | "isNetwork" | "coldStorage">) {
	if (repository.coldStorage) return "Cold Storage";
    if (repository.connector === "s3") return repository.connectorLabel || "s3";
    if (repository.connector !== "fs") return repository.connector;
    const location = repository.location.trim().replace(/\//g, "\\");
    return repository.isNetwork || location.startsWith("\\\\") ? "Network" : "Local";
}

function filesystemVaultFolder(location: string) {
	const windows = /^[A-Za-z]:[\\/]/.test(location) || location.startsWith("\\\\");
	const parts = windows ? location.replace(/[\\/]+$/, "").split(/[\\/]/) : location.replace(/\/+$/, "").split("/");
	return parts.at(-1) || location;
}

function remotePathLabel(pathname: string) {
    return pathname.replace(/^\/+|\/+$/g, "");
}

function s3LocationURL(location: string) {
    // WHATWG URL parsing strips trailing ASCII spaces even without trim().
    // Protect literal spaces before parsing saved S3 addresses, while leaving
    // existing escapes intact so %20 and %2520 remain different native names.
    return new URL(location.replaceAll(" ", "%20"));
}

function repositoryLocationLabel(repository: Pick<Repository, "connector" | "connectorLabel" | "sftpPathMode" | "location" | "isNetwork">) {
    const location = ["fs", "s3"].includes(repository.connector) ? repository.location : repository.location.trim();
	const withLeadingSlash = (value: string) => value.startsWith("/") || value.startsWith("~/") ? value : `/${value}`;
    if (repository.connector === "fs") return withLeadingSlash(filesystemVaultFolder(location));
    try {
        const parsed = repository.connector === "s3" ? s3LocationURL(location) : new URL(location);
        const path = remotePathLabel(parsed.pathname);
        switch (repository.connector) {
            case "s3": {
                const pathParts = path ? path.split("/").map(decodeURIComponent) : [];
                const bucket = pathParts.shift() || parsed.hostname;
				return withLeadingSlash([bucket, ...pathParts].filter(Boolean).join("/") || location);
            }
            case "azblob":
            case "gcs":
				return withLeadingSlash([parsed.hostname, path].filter(Boolean).join("/") || location);
            case "sftp":
				return path ? `${repository.sftpPathMode === "absolute" ? "/" : "~/"}${path}` : repository.sftpPathMode === "absolute" ? "/" : "~/";
            default:
				return withLeadingSlash(location);
        }
    } catch {
		return withLeadingSlash(location);
    }
}

function ownedDormantJobs(repositoryId: string, jobs: DormantRecoveryJob[]) {
	return jobs.filter((job) => job.repositoryId === repositoryId);
}

const emptyVault = (integration?: StorageIntegration, engine: VaultForm["engine"] = "restic"): VaultForm => ({
    engine,
    name: "",
    connector: integration?.id ?? "fs",
	coldStorage: false,
	archiveWriteClass: "GLACIER",
    location: "",
    pendingLocation: "",
    bucket: "",
    container: "",
    prefix: "",
    host: "",
    description: "",
    password: "",
    passwordConfirmation: "",
    options: integrationDefaults(integration),
    checkSchedule: "manual",
    maintenanceSchedule: "daily",
	concurrencyMode: "native",
	objectLock: emptyObjectLock(),
});

function IntegrationField({
    option,
    value,
    onChange,
    explanation,
    disabled,
}: {
    option: IntegrationOption;
    value: string;
    onChange: (value: string) => void;
    explanation?: string;
    disabled?: boolean;
}) {
    if (option.kind === "boolean") {
        return (
            <div className="advanced-setting">
                <label className="check">
                    <input type="checkbox" checked={value === "true"} disabled={disabled} onChange={(event) => onChange(String(event.target.checked))} />
                    {option.label}
                </label>
				{option.help && <small>{option.help}</small>}
                {explanation && <small>{explanation}</small>}
            </div>
        );
    }
    return (
		<label className="field">
			<span>{option.label}{option.required && option.key !== "access_key" && option.key !== "secret_access_key" ? " *" : ""}</span>
			{option.kind === "textarea" ? (
				<textarea rows={3} value={value} disabled={disabled} placeholder={examplePlaceholder(option.placeholder)} onChange={(event) => onChange(event.target.value)} />
			) : (
				<input type={option.secret ? "password" : "text"} value={value} disabled={disabled} placeholder={examplePlaceholder(option.placeholder)} onChange={(event) => onChange(event.target.value)} />
			)}
			{option.help && <small>{option.help}</small>}
            {explanation && <small>{explanation}</small>}
        </label>
    );
}

function remotePathSuffix(prefix: string, connector: string) {
    // S3 key-prefix whitespace is significant, including at either edge.
    // Encode the literal form input once; other providers keep their grammar.
    const value = (connector === "s3" ? prefix : prefix.trim()).replace(/^\/+/, "");
    // Form fields contain literal components. Only S3 admits URL escapes;
    // other connectors retain their unescaped public location grammar. Never
    // let a typed percent-lookalike select a different native directory.
    const path = connector === "s3" ? value.split("/").map(encodeURIComponent).join("/") : value;
    return path ? `/${path}` : "";
}

function endpointHost(value: string) {
    const raw = value.trim();
    if (!raw) return "";
    try {
        const parsed = new URL(/^[a-z][a-z\d+.-]*:\/\//i.test(raw) ? raw : `https://${raw}`);
        return parsed.host;
    } catch {
        return "";
    }
}

function endpointHostname(value: string) {
    const raw = value.trim();
    if (!raw) return "";
    try {
        return new URL(/^[a-z][a-z\d+.-]*:\/\//i.test(raw) ? raw : `https://${raw}`).hostname.toLowerCase();
    } catch {
        return "";
    }
}

function awsRegionFromEndpoint(value: string) {
	const host = endpointHostname(value);
	const suffix = host.endsWith(".amazonaws.com.cn") ? ".amazonaws.com.cn" : ".amazonaws.com";
	if (!host || !host.endsWith(suffix)) return "";
	const labels = host.slice(0, -suffix.length).split(".");
	for (let index = 0; index < labels.length; index++) {
		if (labels[index] !== "s3" && labels[index] !== "s3-fips") continue;
		let regionIndex = index + 1;
		if (labels[regionIndex] === "dualstack") regionIndex++;
		const region = labels[regionIndex] ?? "";
		if (/^[a-z0-9]+(?:-[a-z0-9]+)+-\d+$/.test(region)) return region;
	}
	return "";
}

function sftpHost(value: string) {
    const host = value.trim();
    return host.includes(":") && !host.startsWith("[") ? `[${host}]` : host;
}

function vaultLocation(form: VaultForm, useVaultNameForRclone = false) {
    if (form.pendingLocation) return form.pendingLocation;
	if (usesRcloneNativeLogin(form.connector)) {
		const normalized = normalizedRcloneFolderName(useVaultNameForRclone ? form.name : form.location);
		return normalized && unicodeCodePointCount(normalized) <= 50 ? normalized : "";
	}
    const prefix = remotePathSuffix(form.prefix, form.connector);
    if (["sftp", "azblob", "gcs"].includes(form.connector) && /[%?#]/.test(form.prefix)) return "";
    switch (form.connector) {
        case "s3": {
            const endpoint = endpointHost(form.options.endpoint ?? "");
            const bucket = form.bucket.trim();
            if (!endpoint || !bucket) return "";
            return `s3://${endpoint}/${bucket}${prefix}`;
        }
        case "sftp": {
            const host = sftpHost(form.host);
			const rawPath = form.prefix.trim();
			const absolutePath = (form.options.path_mode || "home") === "absolute";
			if ((absolutePath && (!rawPath.startsWith("/") || rawPath.startsWith("//"))) || (!absolutePath && rawPath.startsWith("/"))) return "";
            return host && prefix ? `sftp://${host}${prefix}` : "";
        }
        case "azblob": {
            const container = form.container.trim();
            return container ? `azblob://${container}${prefix}` : "";
        }
        case "gcs": {
            const bucket = form.bucket.trim();
            return bucket ? `gs://${bucket}${prefix}` : "";
        }
        default:
            // Filesystem whitespace is part of the chosen route, not UI padding.
            return form.connector === "fs" ? form.location : form.location.trim();
    }
}

function effectiveS3Endpoint(endpointValue: string, useTLS: string, port: string, location: string) {
	try {
		const scheme = useTLS === "false" ? "http" : "https";
		const locationURL = s3LocationURL(location);
		if (locationURL.protocol !== "s3:" || !locationURL.hostname) return "";
		const raw = endpointValue.trim() || `${scheme}://${locationURL.pathname.replace(/\/+$/, "") ? locationURL.host : "s3.amazonaws.com"}`;
		const spelled = /^[a-z][a-z\d+.-]*:\/\//i.test(raw) ? raw : `${scheme}://${raw}`;
		const endpoint = new URL(spelled);
		if (endpoint.protocol !== `${scheme}:` || !endpoint.hostname || endpoint.username || endpoint.password || endpoint.pathname !== "/" || endpoint.search || endpoint.hash) return "";
		const authority = spelled.replace(/^[a-z][a-z\d+.-]*:\/\//i, "").split("/")[0];
		const explicitPort = authority.match(/:(\d+)$/)?.[1] ?? "";
		const optionPort = port.trim();
		if (explicitPort && optionPort && explicitPort !== optionPort) return "";
		return `${scheme}://${endpoint.hostname.toLowerCase()}:${explicitPort || optionPort || (scheme === "https" ? "443" : "80")}`;
	} catch { return ""; }
}

function resolvedS3Source(location: string, effectiveEndpoint: string, portOption: string) {
	try {
		const saved = s3LocationURL(location);
		const endpoint = new URL(effectiveEndpoint);
		if (saved.protocol !== "s3:" || !saved.hostname) return "";
		const nativePath = decodeURIComponent(saved.pathname).replace(/^\/+|\/+$/g, "");
		const endpointPort = effectiveEndpoint.match(/:(\d+)$/)?.[1] ?? (endpoint.protocol === "https:" ? "443" : "80");
		const sameHost = saved.hostname.toLowerCase() === endpoint.hostname.toLowerCase();
		const savedPort = saved.port || (sameHost ? portOption.trim() : "") || (endpoint.protocol === "https:" ? "443" : "80");
		// Native S3 treats a different location host as the bucket shorthand.
		// URL-port and option-port spellings of the endpoint resolve alike.
		return sameHost && savedPort === endpointPort
			? nativePath : `${saved.host.toLowerCase()}/${nativePath}`.replace(/\/+$/, "");
	} catch { return ""; }
}

function normalizedRemoteEndpoint(value: string, allowPath: boolean): string | null {
	if (!value.trim()) return "";
	try {
		const endpoint = new URL(value.trim());
		if (!["http:", "https:"].includes(endpoint.protocol) || !endpoint.hostname || endpoint.username || endpoint.password || endpoint.search || endpoint.hash ||
			(!allowPath && endpoint.pathname !== "/")) return null;
		// URL folds default HTTP(S) ports and lowercases the host, as native
		// address resolution does. Azure may retain a custom endpoint path.
		return `${endpoint.protocol}//${endpoint.host}${allowPath ? endpoint.pathname.replace(/\/+$/, "") : ""}`;
	} catch { return null; }
}

function connectionIntentMatchesDestination(intent: RepositoryConnectionIntent, form: VaultForm, entered: string) {
	if (intent.connector !== form.connector || Boolean(intent.coldStorage) !== form.coldStorage) return false;
	// A location URL omits some native destination fields. Do not offer an
	// unrelated saved attempt just because its visible path is the same.
	const savedOptions = intent.reviewedOptions ?? {};
	const sameOption = (key: string, fallback = "") => (savedOptions[key] ?? fallback).trim() === (form.options[key] ?? fallback).trim();
	if (form.connector === "s3") {
		// Native S3 resolution derives an omitted endpoint from the location and
		// folds default ports. Keep scheme and nondefault ports distinct.
		const savedEndpoint = effectiveS3Endpoint(savedOptions.endpoint ?? "", savedOptions.use_tls ?? "true", savedOptions.port ?? "", intent.location);
		const enteredEndpoint = effectiveS3Endpoint(form.options.endpoint ?? "", form.options.use_tls ?? "true", form.options.port ?? "", entered);
		if (!savedEndpoint || savedEndpoint !== enteredEndpoint) return false;
		// A native root override replaces the URL source. Strip only structural
		// slashes; edge whitespace and literal percent bytes remain significant.
		const sourceFromRoot = (root: string) => root.startsWith("/") ? root.replace(/^\/+|\/+$/g, "") : "";
		const savedSource = savedOptions.root ? sourceFromRoot(savedOptions.root) : resolvedS3Source(intent.location, savedEndpoint, savedOptions.port ?? "");
		const enteredPrefix = form.prefix.replace(/^\/+|\/+$/g, "");
		const enteredSource = form.options.root ? sourceFromRoot(form.options.root) : `${form.bucket.trim()}${enteredPrefix ? `/${enteredPrefix}` : ""}`;
		// The UI's bucket/prefix are the native source after URL decoding.
		// Preserve significant whitespace and literal percent sequences.
		return Boolean(savedSource) && savedSource === enteredSource;
	}
	if (form.connector === "sftp") {
		try {
			const saved = new URL(intent.location);
			const current = new URL(entered);
			const currentUsername = current.username ? decodeURIComponent(current.username) : form.options.username ?? "";
			const currentPort = current.port || form.options.port || "22";
			if ((saved.username ? decodeURIComponent(saved.username) : savedOptions.username ?? "").trim() !== currentUsername.trim() ||
				(saved.port || savedOptions.port || "22").trim() !== currentPort.trim() ||
				!sameOption("path_mode", "home")) return false;
		} catch { return false; }
	}
	if (form.connector === "azblob") {
		if (!sameOption("account_name") || !sameOption("root")) return false;
		const account = savedOptions.account_name?.trim() ?? "";
		const defaultEndpoint = account ? `https://${account}.blob.core.windows.net` : "";
		const savedEndpoint = normalizedRemoteEndpoint(savedOptions.endpoint?.trim() || defaultEndpoint, true);
		const enteredEndpoint = normalizedRemoteEndpoint(form.options.endpoint?.trim() || defaultEndpoint, true);
		if (savedEndpoint === null || savedEndpoint !== enteredEndpoint) return false;
	}
	if (form.connector === "gcs") {
		const savedEndpoint = normalizedRemoteEndpoint(savedOptions.endpoint ?? "", false);
		const enteredEndpoint = normalizedRemoteEndpoint(form.options.endpoint ?? "", false);
		if (!sameOption("root") || savedEndpoint === null || savedEndpoint !== enteredEndpoint) return false;
	}
	if (intent.location === entered) return true;
	if (usesRcloneNativeLogin(form.connector)) return intent.location === `Replicaro/${entered}`;
	if (form.connector !== "sftp") return intent.location === entered;
	try {
		// SFTP's visible form keeps username and port in connector options,
		// while saved intent locations can carry them in the URL. Compare the
		// same entered destination fields without discarding either identity.
		const saved = new URL(intent.location);
		const current = new URL(entered);
		return saved.protocol === "sftp:" && saved.hostname.toLowerCase() === current.hostname.toLowerCase() && saved.pathname === current.pathname;
	} catch { return false; }
}

function restoreVaultDestinationFields(form: VaultForm) {
    if (!form.location) return form;
	if (form.connector === "fs") return { ...form, pendingLocation: form.location };
	if (usesRcloneNativeLogin(form.connector)) {
		const canonical = form.location.trim();
		const prefix = "Replicaro/";
		if (!canonical.startsWith(prefix)) return form;
		const folder = canonical.slice(prefix.length);
		if (!folder || folder.includes("/") || folder.includes("\\")) return form;
		return { ...form, location: folder, pendingLocation: canonical };
	}
    try {
        const parsed = form.connector === "s3" ? s3LocationURL(form.location) : new URL(form.location.trim());
        const path = parsed.pathname.replace(/^\/+|\/+$/g, "");
        const parts = path ? path.split("/").map((part) => form.connector === "s3" ? decodeURIComponent(part) : part) : [];
        const restored = { ...form, pendingLocation: form.location };
        switch (form.connector) {
            case "s3": {
                const locationHost = parsed.hostname.toLowerCase();
                const configuredEndpoint = endpointHostname(form.options.endpoint ?? "");
                if (configuredEndpoint && locationHost !== configuredEndpoint) {
                    return { ...restored, bucket: parsed.hostname, prefix: parts.join("/") };
                }
                if (parts.length > 0) {
                    return { ...restored, bucket: parts.shift() ?? "", prefix: parts.join("/") };
                }
                return { ...restored, bucket: parsed.hostname, prefix: "" };
            }
            case "azblob":
                return { ...restored, container: parsed.hostname, prefix: parts.join("/") };
            case "gcs":
                return { ...restored, bucket: parsed.hostname, prefix: parts.join("/") };
            case "sftp":
				return { ...restored, host: parsed.hostname, prefix: `${form.options.path_mode === "absolute" ? "/" : ""}${parts.join("/")}` };
            default:
                return restored;
        }
    } catch {
        return form;
    }
}

function creationOptionIsCustom(connector: string, key: string) {
	if (key === "root") return true;
	if (connector === "s3" && key === "endpoint") return true;
	return connector === "sftp" && ["path_mode", "port", "username", "identity"].includes(key);
}

function connectionOptionIsCustom(connector: string, key: string) {
	return creationOptionIsCustom(connector, key);
}

function advancedCreationOptionExplanation(connector: string, option: IntegrationOption) {
	const helpRemoved = connector === "s3" && ["use_tls", "tls_insecure_no_verify", "port", "storage_class"].includes(option.key) ||
		connector === "sftp" && option.key === "ssh_auth_sock" ||
		["azblob", "gcs"].includes(connector) && option.key === "endpoint";
	return helpRemoved ? undefined : `Optional connector-specific setting for ${option.label.toLowerCase()}.`;
}

function vaultEngineHelp(connector: string, coldStorage: boolean) {
	if (coldStorage) return "Cold storage vaults must use Restic as the engine.";
	if (connector === "dropbox") return "Dropbox vaults must use Restic as the engine.";
	if (connector === "google_drive") return "Google Drive vaults must use Restic as the engine.";
	if (connector === "onedrive") return "OneDrive vaults must use Restic as the engine.";
	return "The engine for this vault cannot be changed after vault creation. Choose wisely.";
}

function creationOptionLockedForPending(form: VaultForm, option: IntegrationOption) {
	if (!form.pendingLocation || option.credential || option.secret) return false;
	return !(form.connector === "sftp" && option.key === "insecure_ignore_host_key");
}

function updateConnectorOption(form: VaultForm, key: string, value: string, inferS3Region = true): VaultForm {
	const options = { ...form.options, [key]: value };
	if (inferS3Region && form.connector === "s3" && key === "endpoint") options.region = awsRegionFromEndpoint(value);
	return { ...form, options };
}

function RemoteVaultFields({
	form,
	integration,
	onChange,
}: {
    form: VaultForm;
    integration: StorageIntegration;
    onChange: (next: VaultForm) => void;
}) {
    const option = (key: string) => integration.options.find((candidate) => candidate.key === key);
    const updateOption = (key: string, value: string) => onChange(updateConnectorOption(form, key, value, Boolean(option("region"))));
    const renderOption = (key: string, displayValue?: string) => {
        const selected = option(key);
		return selected ? <IntegrationField key={key} option={selected} value={displayValue ?? form.options[key] ?? ""} disabled={Boolean(form.pendingLocation) && !selected.credential} onChange={(value) => updateOption(key, value)} /> : null;
    };

	if (usesRcloneNativeLogin(integration.id)) {
		return <label className="field"><span>Vault folder name on {integration.label}</span><input aria-label={`Vault folder name on ${integration.label}`} value={form.location} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, location: event.target.value })} /><small>This is the name of the vault&apos;s folder in the Replicaro folder on {integration.label}. Make sure to type it in exactly as you see it.</small></label>;
	}

    switch (integration.id) {
        case "s3":
            return <>
                {renderOption("endpoint")}
                <label className="field"><span>Bucket</span><input value={form.bucket} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, bucket: event.target.value })} /></label>
				<label className="field"><span>Path/prefix (optional)</span><input value={form.prefix} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, prefix: event.target.value })} /></label>
            </>;
        case "azblob":
            return <>
                <label className="field"><span>Container</span><input value={form.container} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, container: event.target.value })} /></label>
				<label className="field"><span>Path/prefix (optional)</span><input value={form.prefix} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, prefix: event.target.value })} /></label>
            </>;
        case "gcs":
            return <>
                <label className="field"><span>Bucket</span><input value={form.bucket} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, bucket: event.target.value })} /></label>
                <label className="field"><span>Path/prefix (optional)</span><input value={form.prefix} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, prefix: event.target.value })} /></label>
            </>;
        case "sftp": {
            const pendingURL = (() => {
                try {
                    return form.pendingLocation ? new URL(form.pendingLocation) : null;
                } catch {
                    return null;
                }
            })();
			const pathMode = form.options.path_mode || "home";
			const absolutePath = pathMode === "absolute";
			const supportsPathMode = Boolean(option("path_mode"));
            return <>
                <label className="field"><span>Host</span><input value={form.host} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, host: event.target.value })} /></label>
                {renderOption("port", pendingURL?.port || undefined)}
                {renderOption("username", pendingURL?.username || undefined)}
				{supportsPathMode && <label className="field"><span>Vault location on server</span><select value={pathMode} disabled={Boolean(form.pendingLocation)} onChange={(event) => updateOption("path_mode", event.target.value)}><option value="home">SFTP home directory (recommended)</option><option value="absolute">Server filesystem root</option></select></label>}
				<label className="field"><span>{absolutePath ? "Vault path relative to server filesystem root" : "Vault path relative to SFTP home"}</span><input aria-label={absolutePath ? "Vault path relative to server filesystem root" : "Vault path relative to SFTP home"} value={form.prefix} placeholder={absolutePath ? "example: /srv/backups/vault" : "example: backups/vault"} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, prefix: event.target.value })} /><small>{absolutePath ? "Start with one /; the path begins at the server filesystem root." : "Do not start with /; the path begins in the authenticated user's SFTP home directory."}</small></label>
                {renderOption("identity")}
            </>;
        }
        default:
            return null;
    }
}

function VaultCareFields({
	form,
	disabled = false,
	integrityDisabled = false,
	maintenanceDisabled = false,
	integrityLockedHelp = "",
	maintenanceLockedHelp = "",
	onChange,
}: {
	form: VaultForm;
	disabled?: boolean;
	integrityDisabled?: boolean;
	maintenanceDisabled?: boolean;
	integrityLockedHelp?: string;
	maintenanceLockedHelp?: string;
	onChange: (next: VaultForm) => void;
}) {
	return (
		<div className="form-grid two">
			<label className="field">
				<span>Integrity check</span>
				<select disabled={disabled || integrityDisabled || form.coldStorage} value={form.coldStorage ? "manual" : form.checkSchedule} onChange={(event) => onChange({ ...form, checkSchedule: event.target.value })}>{(form.coldStorage ? [["manual", "Disabled"]] as const : careSchedules).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select>
				<small>{form.coldStorage ? coldStorageIntegrityHelp : integrityCheckHelp}</small>
				{integrityDisabled && integrityLockedHelp && <small>{integrityLockedHelp}</small>}
			</label>
			<label className="field">
				<span>Space reclamation</span>
				<select disabled={disabled || maintenanceDisabled} value={form.maintenanceSchedule} onChange={(event) => onChange({ ...form, maintenanceSchedule: event.target.value })}>{careSchedules.map(([value, label]) => <option key={value} value={value} disabled={!objectLockScheduleEligible(form.objectLock, value)}>{label}</option>)}</select>
				<small>{form.objectLock.enrolled ? (form.objectLock.paused ? pausedObjectLockMaintenanceHelp : objectLockMaintenanceHelp) : maintenanceHelp}</small>
				{maintenanceDisabled && maintenanceLockedHelp && <small>{maintenanceLockedHelp}</small>}
			</label>
			<label className="field vault-job-speed-field">
				<span>Job speed</span>
				<select disabled={disabled} value={form.concurrencyMode} onChange={(event) => onChange({ ...form, concurrencyMode: event.target.value as VaultForm["concurrencyMode"] })}><option value="reduced">Slower</option><option value="native">Normal</option><option value="increased">Faster</option><option value="maximum">Maximum</option></select>
				<small>Controls how quickly Replicaro finishes backup, restore, integrity check, and space reclamation jobs. Higher speed means higher CPU, memory, disk, and network use. Set higher speeds with caution.</small>
			</label>
		</div>
	);
}

function ObjectLockFields({
	form,
	onChange,
	disabled = false,
	creation = false,
	creationAvailable = false,
	originalObjectLock,
}: {
	form: VaultForm;
	onChange: (next: VaultForm) => void;
	disabled?: boolean;
	creation?: boolean;
	creationAvailable?: boolean;
	originalObjectLock?: ObjectLockSettings;
}) {
	const eligible = objectLockEligible(form.engine, form.connector) || (creation && creationAvailable);
	if (!eligible) return null;
	const enrollmentLocked = !creation || !eligible || disabled;
	const settingsDisabled = disabled || !form.objectLock.enrolled || form.objectLock.paused;
	const updateSettings = (next: ObjectLockSettings) => {
		if (next.enrolled) next = {
			...next,
			mode: next.mode || "compliance",
			durationValue: next.durationValue || 2,
			durationUnit: next.durationUnit || "days",
		};
		if (!next.enrolled) next = emptyObjectLock();
		const maintenanceSchedule = objectLockScheduleEligible(next, form.maintenanceSchedule)
			? form.maintenanceSchedule : longestEligibleObjectLockSchedule(next);
		onChange({ ...form, objectLock: next, maintenanceSchedule });
	};
	return <section className="advanced-setting">
		{creation
			? <label className="check"><input type="checkbox" checked={form.objectLock.enrolled} disabled={enrollmentLocked} onChange={(event) => updateSettings({ ...form.objectLock, enrolled: event.target.checked, paused: false })} />Enable Object Lock</label>
			: <div className="object-lock-enrollment-status">Object Lock Status: <strong className={form.objectLock.enrolled ? "enabled" : "disabled"}>{form.objectLock.enrolled ? "Enabled" : "Disabled"}</strong></div>}
		<small className="object-lock-help">{creation && eligible ? "Protect snapshots from deletion or overwrite for a selected duration." : "Object lock cannot be enabled or disabled after creation."}</small>
		{form.objectLock.enrolled && <>
			{!creation && <label className="field"><span>Object Lock activity</span><select value={form.objectLock.paused ? "paused" : "active"} disabled={disabled} onChange={(event) => updateSettings({ ...form.objectLock, paused: event.target.value === "paused" })}><option value="active">Active</option><option value="paused">Paused</option></select></label>}
			<div className="form-grid two object-lock-settings-grid">
				<label className="field"><span>Mode</span><select value={form.objectLock.mode || "compliance"} disabled={settingsDisabled} onChange={(event) => updateSettings({ ...form.objectLock, mode: event.target.value as ObjectLockSettings["mode"] })}><option value="compliance">Compliance{form.connector === "s3" ? " (default)" : ""}</option>{form.connector === "s3" && (creation || originalObjectLock?.mode === "governance") && <option value="governance">Governance</option>}</select></label>
				<label className="field"><span>Duration</span><div className="form-grid two"><input type="number" min={form.objectLock.durationUnit === "days" ? 2 : 1} step={1} value={form.objectLock.durationValue} disabled={settingsDisabled} onWheel={preventNumberInputWheel} onChange={(event) => updateSettings({ ...form.objectLock, durationValue: Math.max(0, Math.trunc(Number(event.target.value))) })} /><select aria-label="Object Lock duration unit" value={form.objectLock.durationUnit || "days"} disabled={settingsDisabled} onChange={(event) => updateSettings({ ...form.objectLock, durationUnit: event.target.value as ObjectLockSettings["durationUnit"] })}><option value="days">Days</option><option value="weeks">Weeks</option><option value="months">Months</option><option value="years">Years</option></select></div></label>
			</div>
			{form.objectLock.paused && <small>Resume object lock to change its mode or duration.</small>}
			{!creation && <small>{objectLockTransitionHelp}</small>}
			<small className="object-lock-help">{objectLockForwardHelp}</small>
		</>}
	</section>;
}

function DestinationVaultPicker({
    repositories,
    selectedIds,
    onChange,
}: {
    repositories: Repository[];
    selectedIds: string[];
    onChange: (ids: string[]) => void;
}) {
    const [query, setQuery] = useState("");
    const selected = selectedIds
        .map((id) => repositories.find((repository) => repository.id === id))
        .filter((repository): repository is Repository => Boolean(repository));
    const normalizedQuery = query.trim().toLowerCase();
    const available = repositories.filter((repository) =>
        !selectedIds.includes(repository.id) &&
        (!normalizedQuery || `${repository.name} ${repository.location}`.toLowerCase().includes(normalizedQuery))
    );

    return (
        <div className="destination-picker">
            <div className="destination-picker-selected">
                <div className="destination-picker-summary">
                    <span>{selected.length ? `${selected.length} selected` : "No vaults selected"}</span>
                    {selected.length > 0 && <button type="button" className="text-button" onClick={() => onChange([])}>Clear all</button>}
                </div>
                {selected.length > 0 && (
                    <div className="destination-chips">
                        {selected.map((repository) => (
                            <Tooltip key={repository.id} content={`Remove ${repository.name}`}>
                                <button
                                    type="button"
                                    className="destination-chip"
                                    onClick={() => onChange(selectedIds.filter((id) => id !== repository.id))}
                                >
                                    {repository.name}<Icon name="x" size={11} />
                                </button>
                            </Tooltip>
                        ))}
                    </div>
                )}
            </div>
            <input
                type="search"
                value={query}
                placeholder="Search vaults by name or location"
                aria-label="Search destination vaults"
                onChange={(event) => setQuery(event.target.value)}
            />
            <div className="destination-results" role="listbox" aria-label="Available destination vaults">
                {available.map((repository) => (
                    <button
                        type="button"
                        className="destination-result"
                        key={repository.id}
                        onClick={() => onChange([...selectedIds, repository.id])}
                    >
                        <span><strong>{repository.name}</strong><small className="mono">{repository.location}</small>{repository.resolvedRepositoryPath && <small className="mono">Last verified at {repository.resolvedRepositoryPath}</small>}</span>
                        <span className="destination-add"><Icon name="plus" size={12} /> Add</span>
                    </button>
                ))}
                {available.length === 0 && (
                    <div className="destination-results-empty">
                        {normalizedQuery ? "No matching vaults" : "All available vaults are selected"}
                    </div>
                )}
            </div>
        </div>
    );
}

function readableSize(bytes: number) {
    const units = ["B", "KB", "MB", "GB", "TB"];
    let value = bytes;
    let unit = 0;
    while (value >= 1000 && unit < units.length - 1) {
        value /= 1000;
        unit += 1;
    }
    return `${value >= 10 || unit === 0 ? value.toFixed(0) : value.toFixed(1)} ${units[unit]}`;
}

function vaultSizePresentation(bytes: number) {
	const decimalUnits = ["B", "KB", "MB", "GB", "TB"];
	const binaryUnits = ["B", "KiB", "MiB", "GiB", "TiB"];
	let decimalValue = bytes;
	let unit = 0;
	while (decimalValue >= 1000 && unit < decimalUnits.length - 1) {
		decimalValue /= 1000;
		unit += 1;
	}
	const display = `${decimalValue >= 10 || unit === 0 ? decimalValue.toFixed(0) : decimalValue.toFixed(1)} ${decimalUnits[unit]}`;
	if (unit === 0) {
		return { display, tooltip: `${display}. Bytes are the same in both unit systems.` };
	}
	const binaryValue = bytes / (1024 ** unit);
	const binary = `${binaryValue >= 10 ? binaryValue.toFixed(0) : binaryValue.toFixed(1)} ${binaryUnits[unit]}`;
	return {
		display,
		tooltip: `${binary}. ${decimalUnits[unit]} is smaller than ${binaryUnits[unit]}. Some providers show disk usage in ${decimalUnits[unit]} while others show in ${binaryUnits[unit]}.`,
	};
}

function displayPath(value: string) {
	if (!/^\/?[A-Za-z]:[\\/]/.test(value)) return value;
	return value.replace(/^\/+/, "").replace(/\//g, "\\");
}

export default function Protect() {
    const toast = useToast();
    const [jobs, setJobs] = useState<BackupJob[] | null>(null);
    const [repos, setRepos] = useState<Repository[] | null>(null);
	const [vaultStats, setVaultStats] = useState<Record<string, VaultSizeStatus>>({});
    const [integrations, setIntegrations] = useState<StorageIntegration[]>([]);
    const [engineCatalog, setEngineCatalog] = useState<EngineDescriptor[]>([]);
    const [defaultEngine, setDefaultEngine] = useState<"restic" | "kopia">("restic");
    const [running, setRunning] = useState<Record<string, boolean>>({});
    const [profileSync, setProfileSync] = useState<Record<string, VaultProfileSyncStatus>>({});
    const [jobPage, setJobPage] = useState(0);
    const [vaultPage, setVaultPage] = useState(0);
    const [jobPageSize, setJobPageSize] = useState<(typeof JOB_PAGE_SIZE_OPTIONS)[number]>(() => readStoredPageSize(JOB_PAGE_SIZE_STORAGE_KEY, JOB_PAGE_SIZE_OPTIONS, JOB_PAGE_SIZE));
    const [vaultPageSize, setVaultPageSize] = useState<(typeof VAULT_PAGE_SIZE_OPTIONS)[number]>(() => readStoredPageSize(VAULT_PAGE_SIZE_STORAGE_KEY, VAULT_PAGE_SIZE_OPTIONS, VAULT_PAGE_SIZE));
    const [jobSort, setJobSort] = useState<JobSort>(() => readStoredOption(JOB_SORT_STORAGE_KEY, JOB_SORT_OPTIONS.map((option) => option.value), "newest"));
    const [vaultSort, setVaultSort] = useState<VaultSort>(() => readStoredOption(VAULT_SORT_STORAGE_KEY, VAULT_SORT_OPTIONS.map((option) => option.value), "newest"));

    const [jobModal, setJobModal] = useState<BackupJob | "new" | null>(null);
	const [jobCopyDraft, setJobCopyDraft] = useState(false);
	const [jobForm, setJobForm] = useState<JobForm>({ ...emptyJob });
	const [jobAdditionalSources, setJobAdditionalSources] = useState<string[]>([]);
	const [jobCreationResults, setJobCreationResults] = useState<JobMutationResult[] | null>(null);
	const [jobCreationRetrying, setJobCreationRetrying] = useState(false);
    const [jobInitialForm, setJobInitialForm] = useState<JobForm | null>(null);
    const [jobAdvanced, setJobAdvanced] = useState(false);
    const [jobSaving, setJobSaving] = useState(false);
    const [jobDelete, setJobDelete] = useState<BackupJob | null>(null);
    const [jobDeleting, setJobDeleting] = useState(false);
	const [jobToggleBusy, setJobToggleBusy] = useState("");
	const [selectedJobIDs, setSelectedJobIDs] = useState<string[]>([]);
	const [bulkEditOpen, setBulkEditOpen] = useState(false);
	const [bulkEditStep, setBulkEditStep] = useState<"edit" | "review">("edit");
	const [bulkForm, setBulkForm] = useState<BulkEditForm>({ ...emptyBulkEdit });
	const [bulkSaving, setBulkSaving] = useState(false);
	const [bulkResults, setBulkResults] = useState<BulkResultView | null>(null);
	const [bulkRetrying, setBulkRetrying] = useState(false);
    const [runJobID, setRunJobID] = useState("");
    const [runBusy, setRunBusy] = useState("");
    const [runActiveJobID, setRunActiveJobID] = useState("");
	const [runResults, setRunResults] = useState<ManualTargetAdmissionResult[] | null>(null);

    const [showVaultCreate, setShowVaultCreate] = useState(false);
	const [showVaultChoice, setShowVaultChoice] = useState(false);
	const [showVaultConnect, setShowVaultConnect] = useState(false);
	const [showCreateRcloneAuthorization, setShowCreateRcloneAuthorization] = useState(false);
	const [connectRcloneAuthorizationAction, setConnectRcloneAuthorizationAction] = useState<"check" | "retry" | null>(null);
    const [vaultForm, setVaultFormState] = useState<VaultForm>(emptyVault());
	const [createRcloneAuth, setCreateRcloneAuth] = useState<RcloneAuthStatus | null>(null);
    const [vaultAdvanced, setVaultAdvanced] = useState(false);
		const [vaultSaving, setVaultSaving] = useState(false);
	const [retryCreationIntentId, setRetryCreationIntentId] = useState("");
	const setVaultForm = (next: VaultForm) => setVaultFormState(next);
	const [vaultCreateError, setVaultCreateError] = useState("");
	const [connectForm, setConnectForm] = useState<VaultForm>(emptyVault());
	const [connectRcloneAuth, setConnectRcloneAuth] = useState<RcloneAuthStatus | null>(null);
	const [connectAdvanced, setConnectAdvanced] = useState(false);
	const [connectPreview, setConnectPreview] = useState<ExistingVaultPreview | null>(null);
	const [connectProfileUUID, setConnectProfileUUID] = useState("");
	const [connectProfileAction, setConnectProfileAction] = useState<"join" | "reconnect" | "takeover">("join");
	const [connectProfileChoiceMade, setConnectProfileChoiceMade] = useState(false);
	const [connectPendingProfileChoice, setConnectPendingProfileChoice] = useState<{ profileUUID: string; join: boolean } | null>(null);
	const [connectOwnerAction, setConnectOwnerAction] = useState<"keep" | "takeover">("keep");
	const [connectOwnerChoiceMade, setConnectOwnerChoiceMade] = useState(false);
	const [connectUpdateConfirmedDigest, setConnectUpdateConfirmedDigest] = useState("");
	const [connectReviewedLocation, setConnectReviewedLocation] = useState("");
	const [connectDetectedEngine, setConnectDetectedEngine] = useState<"restic" | "kopia" | "">("");
	const [connectNameConflictNotice, setConnectNameConflictNotice] = useState("");
	const [connectRcloneNameConflict, setConnectRcloneNameConflict] = useState(false);
	const [connectChecking, setConnectChecking] = useState(false);
	const [connectSaving, setConnectSaving] = useState(false);
	const [vaultProgress, setVaultProgress] = useState<VaultProgressRecord[]>([]);
	const vaultProgressView = useRef<HTMLDivElement | null>(null);
	useEffect(() => {
		if (vaultProgressView.current) vaultProgressView.current.scrollTop = vaultProgressView.current.scrollHeight;
	}, [vaultProgress]);
	// A small bounded presentation buffer lets users see actual stages and native
	// output while the existing request remains authoritative. No progress is
	// invented, persisted, or used to decide whether creation/connection worked.
	const appendVaultProgress = (record: VaultProgressRecord) => setVaultProgress((current) => [...current.slice(-199), record]);
	const [connectionIntents, setConnectionIntents] = useState<RepositoryConnectionIntent[]>([]);
	const [retryConnectionError, setRetryConnectionError] = useState("");
	const [creationIntents, setCreationIntents] = useState<RepositoryCreationIntent[]>([]);
	const [creationErrorView, setCreationErrorView] = useState<RepositoryCreationIntent | null>(null);
	const [creationRemovalBusy, setCreationRemovalBusy] = useState("");
		const [retryConnectionIntentId, setRetryConnectionIntentId] = useState("");
	const [vaultSettings, setVaultSettings] = useState<Repository | null>(null);
	const [vaultWorkState, setVaultWorkState] = useState<VaultWorkState>("idle");
	const [vaultSettingsAdvanced, setVaultSettingsAdvanced] = useState(false);
	const [vaultOwnership, setVaultOwnership] = useState<VaultOwnershipStatus | null>(null);
	const [vaultOwnershipPresentation, setVaultOwnershipPresentation] = useState<VaultOwnershipPresentation>("unverified");
	const [vaultOwnershipBusy, setVaultOwnershipBusy] = useState(false);
	const [vaultPasswordForm, setVaultPasswordForm] = useState({ password: "", confirmation: "" });
	const [vaultPasswordRecovery, setVaultPasswordRecovery] = useState<VaultPasswordChangeResult | null>(null);
	const [vaultPasswordChangeBusy, setVaultPasswordChangeBusy] = useState(false);
	const rcloneAuthSessionsOnUnmount = useRef<string[]>([]);
	const [dormantJobs, setDormantJobs] = useState<DormantRecoveryJob[]>([]);
	const [dormantBusy, setDormantBusy] = useState("");
	const [vaultInitialSchedules, setVaultInitialSchedules] = useState<{ checkSchedule: string; maintenanceSchedule: string; concurrencyMode: VaultForm["concurrencyMode"]; autoUnlock: boolean; objectLock: ObjectLockSettings } | null>(null);
    const [checkSchedule, setCheckSchedule] = useState("manual");
    const [maintenanceSchedule, setMaintenanceSchedule] = useState("weekly");
	const [concurrencyMode, setConcurrencyMode] = useState<VaultForm["concurrencyMode"]>("native");
	const [autoUnlock, setAutoUnlock] = useState(true);
	const [objectLock, setObjectLock] = useState<ObjectLockSettings>(emptyObjectLock());
    const [careSaving, setCareSaving] = useState(false);
    const [toolBusy, setToolBusy] = useState("");
	const [toolOutput, setToolOutput] = useState("");
    const [showRawToolLog, setShowRawToolLog] = useState(false);
	const toolRequestController = useRef<AbortController | null>(null);
	const [vaultDelete, setVaultDelete] = useState<Repository | null>(null);
	const [vaultRemoval, setVaultRemoval] = useState<Record<string, { phase: "removing" | "profile_error" | "error"; message?: string }>>({});
	const [closePrompt, setClosePrompt] = useState<"job" | "vault" | null>(null);
	const [vaultReconnectAfterClose, setVaultReconnectAfterClose] = useState<Repository | null>(null);
	const jobModalSession = useRef(0);
	const jobSubmissionGeneration = useRef(0);
	const jobSubmissionOwner = useRef<{ session: number; generation: number } | null>(null);
	const jobCreationResultSession = useRef(0);
	const jobCreationRetryGeneration = useRef(0);
	const jobCreationRetryOwner = useRef<{ session: number; generation: number } | null>(null);
	const bulkSession = useRef(0);
	const bulkSubmissionGeneration = useRef(0);
	const bulkSubmissionOwner = useRef<{ session: number; generation: number } | null>(null);
	const bulkResultSession = useRef(0);
	const bulkRetryGeneration = useRef(0);
	const bulkRetryOwner = useRef<{ session: number; generation: number } | null>(null);
	const vaultCreateSession = useRef(0);
	const vaultCreateSubmissionGeneration = useRef(0);
	const vaultCreateSubmissionOwner = useRef<{ session: number; generation: number } | null>(null);
	const connectPreviewSession = useRef(0);
	const connectInitializationGeneration = useRef(0);
	const connectSubmissionGeneration = useRef(0);
	const connectPreviewController = useRef<AbortController | null>(null);
	const connectReviewedStorage = useRef<ExistingVaultStorageInput | null>(null);
	const connectPreviewBaseFields = useRef<Pick<VaultForm, "name" | "description" | "checkSchedule" | "maintenanceSchedule" | "concurrencyMode"> | null>(null);
	const vaultSettingsSession = useRef(0);
	const vaultSettingsOwner = useRef("");
	const vaultOwnershipRequest = useRef(0);
	// Reconnect presentation is derived only from operations observed in this
	// frontend session; it never probes or recreates persistent vault health.
	const observedBackupOperations = useRef<Record<string, ObservedBackupOperation>>({});
	const observedOperationSession = useRef(1);
	const observedOperationReadGeneration = useRef(0);
	const observedOperationReadOwners = useRef<Record<string, ObservedOperationReadOwner>>({});
	const observedRepositoryEpochs = useRef<Record<string, number>>({});
	const [reconnectRequiredVaults, setReconnectRequiredVaults] = useState<Record<string, boolean>>({});
	const runReviewSession = useRef(0);
	const runSubmissionGeneration = useRef(0);
	const runSubmissionOwner = useRef<RunSubmissionOwner | null>(null);
	const jobDeleteSession = useRef(0);
	const jobDeleteGeneration = useRef(0);
	const jobDeleteOwner = useRef<{ session: number; generation: number } | null>(null);
	const vaultRemovalInFlight = useRef(new Set<string>());
	const refreshGenerations = useRef({ jobs: 0, repositories: 0, profileSync: 0, running: 0, creations: 0 });
	const runningState = useRef<Record<string, boolean>>({});

	const commitRunning = useCallback((next: Record<string, boolean>) => {
		runningState.current = next;
		setRunning(next);
	}, []);

	const clearReconnectObservation = useCallback((repositoryId: string) => {
		if (!repositoryId) return;
		observedRepositoryEpochs.current[repositoryId] = (observedRepositoryEpochs.current[repositoryId] ?? 0) + 1;
		for (const [operationId, observed] of Object.entries(observedBackupOperations.current)) {
			if (observed.repositoryId !== repositoryId) continue;
			delete observedBackupOperations.current[operationId];
		}
		for (const [operationId, owner] of Object.entries(observedOperationReadOwners.current)) {
			if (owner.repositoryId !== repositoryId) continue;
			delete observedOperationReadOwners.current[operationId];
		}
		setReconnectRequiredVaults((current) => {
			const updated = { ...current };
			delete updated[repositoryId];
			return updated;
		});
	}, []);

	const commitActiveTargets = useCallback((activeTargets: ActiveBackupTarget[]) => {
		const activeOperationIDs = new Set(activeTargets.map((target) => target.operationId).filter(Boolean));
		for (const [operationId, observed] of Object.entries(observedBackupOperations.current)) {
			if (activeOperationIDs.has(operationId)) continue;
			delete observedBackupOperations.current[operationId];
			const session = observedOperationSession.current;
			const generation = ++observedOperationReadGeneration.current;
			const repositoryEpoch = observedRepositoryEpochs.current[observed.repositoryId] ?? 0;
			observedOperationReadOwners.current[operationId] = {
				generation, repositoryId: observed.repositoryId, repositoryEpoch,
			};
			void getOperation(operationId).then((operations) => {
				const owner = observedOperationReadOwners.current[operationId];
				if (observedOperationSession.current !== session ||
					owner?.generation !== generation || owner.repositoryId !== observed.repositoryId ||
					owner.repositoryEpoch !== repositoryEpoch ||
					(observedRepositoryEpochs.current[observed.repositoryId] ?? 0) !== repositoryEpoch) return;
				delete observedOperationReadOwners.current[operationId];
				const operation = operations.length === 1 ? operations[0] : undefined;
				if (!operation || operation.kind !== "backup" || operation.id !== operationId || operation.jobId !== observed.jobId ||
					operation.repositoryId !== observed.repositoryId || operation.status !== "reconnect_required") return;
				setReconnectRequiredVaults((current) => ({ ...current, [observed.repositoryId]: true }));
			}).catch(() => {
				if (observedOperationReadOwners.current[operationId]?.generation === generation) {
					delete observedOperationReadOwners.current[operationId];
				}
			});
		}
		for (const target of activeTargets) {
			if (!target.operationId) continue;
			delete observedOperationReadOwners.current[target.operationId];
			observedBackupOperations.current[target.operationId] = {
				jobId: target.jobId, repositoryId: target.repositoryId,
			};
		}
	}, []);

	const commitJobs = useCallback((nextJobs: BackupJob[]) => {
		setSelectedJobIDs((current) => {
			if (nextJobs.length <= 1) return current.length === 0 ? current : [];
			const available = new Set(nextJobs.map((job) => job.id));
			const retained = current.filter((id) => available.has(id));
			return retained.length === current.length ? current : retained;
		});
		setJobs(nextJobs);
	}, []);

	const invalidateConnectPreview = useCallback((preserveDetectedEngine = false) => {
		connectPreviewSession.current++;
		connectInitializationGeneration.current++;
		connectSubmissionGeneration.current++;
		connectPreviewController.current?.abort();
		connectPreviewController.current = null;
		connectReviewedStorage.current = null;
		const baseFields = connectPreviewBaseFields.current;
		connectPreviewBaseFields.current = null;
		if (baseFields) setConnectForm((current) => ({ ...current, ...baseFields }));
		setConnectChecking(false);
		setConnectSaving(false);
		setConnectPreview(null);
		if (!preserveDetectedEngine) setConnectDetectedEngine("");
		setConnectNameConflictNotice("");
		setConnectRcloneNameConflict(false);
		setConnectProfileUUID("");
		setConnectProfileAction("join");
		setConnectProfileChoiceMade(false);
		setConnectPendingProfileChoice(null);
		setConnectOwnerAction("keep");
		setConnectOwnerChoiceMade(false);
		setConnectUpdateConfirmedDigest("");
		setConnectReviewedLocation("");
	}, []);

	useEffect(() => () => {
		for (const sessionID of new Set(rcloneAuthSessionsOnUnmount.current)) {
			if (sessionID) void closeRcloneAuthorization(sessionID);
		}
		jobModalSession.current++;
		jobSubmissionGeneration.current++;
		jobSubmissionOwner.current = null;
		jobCreationResultSession.current++;
		jobCreationRetryGeneration.current++;
		jobCreationRetryOwner.current = null;
		bulkSession.current++;
		bulkSubmissionGeneration.current++;
		bulkSubmissionOwner.current = null;
		bulkResultSession.current++;
		bulkRetryGeneration.current++;
		bulkRetryOwner.current = null;
		vaultCreateSession.current++;
		vaultCreateSubmissionGeneration.current++;
		vaultCreateSubmissionOwner.current = null;
		connectPreviewSession.current++;
		connectInitializationGeneration.current++;
		connectSubmissionGeneration.current++;
		connectPreviewController.current?.abort();
		connectPreviewController.current = null;
		observedOperationSession.current++;
		observedBackupOperations.current = {};
		observedOperationReadOwners.current = {};
		observedRepositoryEpochs.current = {};
		runReviewSession.current++;
		runSubmissionGeneration.current++;
		runSubmissionOwner.current = null;
		jobDeleteSession.current++;
		jobDeleteGeneration.current++;
		jobDeleteOwner.current = null;
	}, []);

	useEffect(() => {
		rcloneAuthSessionsOnUnmount.current = [
			createRcloneAuth?.sessionId ?? "",
			connectRcloneAuth?.sessionId ?? "",
		];
	}, [createRcloneAuth?.sessionId, connectRcloneAuth?.sessionId]);

	const applySavedJob = (saved: BackupJob) => {
		// Invalidate older list requests before committing the authoritative save
		// response, so a slow pre-save refresh cannot replace the saved definition.
		++refreshGenerations.current.jobs;
		setJobs((current) => (current ?? []).some((job) => job.id === saved.id)
			? (current ?? []).map((job) => job.id === saved.id ? saved : job)
			: [...(current ?? []), saved]);
	};

	const load = useCallback(() => {
		const request = {
			jobs: ++refreshGenerations.current.jobs,
			repositories: ++refreshGenerations.current.repositories,
			profileSync: ++refreshGenerations.current.profileSync,
			running: ++refreshGenerations.current.running,
			creations: ++refreshGenerations.current.creations,
		};
		// Lists settle independently: a slow vault observation must not hold
		// saved jobs or synchronization status behind one aggregate response.
		void getJobs().then((nextJobs) => {
			if (refreshGenerations.current.jobs !== request.jobs) return;
			commitJobs(nextJobs);
			if (refreshGenerations.current.running !== request.running) return;
			const nextRunning = Object.fromEntries(nextJobs
				.filter((job) => job.targets.some(backupTargetIsActive))
				.map((job) => [job.id, true]));
			for (const observed of Object.values(observedBackupOperations.current)) nextRunning[observed.jobId] = true;
			commitRunning(nextRunning);
		}).catch((error: Error) => {
			if (refreshGenerations.current.jobs === request.jobs) toast("error", error.message);
		});
		void getVaultProfileSyncStatuses().then((statuses) => {
			if (refreshGenerations.current.profileSync === request.profileSync)
				setProfileSync(Object.fromEntries(statuses.map((status) => [status.repositoryId, status])));
		}).catch((error: Error) => {
			if (refreshGenerations.current.profileSync === request.profileSync) toast("error", error.message);
		});
		void getRepositoryCreationIntents().then((intents) => {
			if (refreshGenerations.current.creations === request.creations) setCreationIntents(intents);
		}).catch((error: Error) => {
			if (refreshGenerations.current.creations === request.creations) toast("error", error.message);
		});
		void getRepositories().then((nextRepos) => {
			if (refreshGenerations.current.repositories !== request.repositories) return;
			setRepos(nextRepos);
			setVaultStats((current) => Object.fromEntries(nextRepos.map((repository) => {
				const active = current[repository.id];
				return [repository.id, active?.running || active?.pending ? active : {
					vaultSizeBytes: repository.vaultSizeBytes,
					vaultSizeMeasuredAt: repository.vaultSizeMeasuredAt,
					vaultSizeDirty: repository.vaultSizeDirty,
					fresh: false, running: false, pending: false, paused: false,
				}];
			})));
		}).catch((error: Error) => {
			if (refreshGenerations.current.repositories === request.repositories) toast("error", error.message);
		});
	}, [commitJobs, commitRunning, toast]);

	const retryProfile = async (repositoryId: string) => {
		try {
			const result = await retryVaultProfileSync(repositoryId);
			toast(result.profilePending ? "info" : "ok", result.warning ?? "Vault recovery profile synchronized");
			load();
		} catch (error) {
			toast("error", (error as Error).message);
		}
	};

    useEffect(() => {
        load();
        Promise.all([getIntegrations(), getEngines(), getSettings()])
            .then(([catalog, engineInfo, settings]) => {
                setIntegrations(catalog.integrations);
                setEngineCatalog(engineInfo.engines);
                setDefaultEngine(settings.defaultEngine);
                const descriptor = engineInfo.engines.find((item) => item.id === settings.defaultEngine);
                const provider = catalog.integrations.find((item) => descriptor?.providers.some((candidate) => candidate.id === item.id && candidate.supported));
                setVaultFormState(emptyVault(provider, settings.defaultEngine));
				setConnectForm(emptyVault(catalog.integrations.find((item) => item.id === "fs"), settings.defaultEngine));
            })
            .catch((error: Error) => toast("error", error.message));
	}, [load, toast]);

	useEffect(() => {
		const target = window.location.hash.slice(1);
        if (target) {
            window.requestAnimationFrame(() => document.getElementById(target)?.scrollIntoView());
        }
	}, []);

	useEffect(() => {
		const policyPending = (jobs ?? []).some((job) =>
			job.targets.some((target) => target.engine === "kopia" && target.policyStatus === "pending")
		);
		if (!Object.values(running).some(Boolean) && Object.keys(profileSync).length === 0 && !policyPending) return;
		const timer = window.setInterval(() => {
			const request = {
				jobs: ++refreshGenerations.current.jobs,
				profileSync: ++refreshGenerations.current.profileSync,
				running: ++refreshGenerations.current.running,
			};
			void getJobs().then((nextJobs) => {
				if (refreshGenerations.current.jobs === request.jobs) commitJobs(nextJobs);
			}).catch(() => undefined);
			void getVaultProfileSyncStatuses().then((statuses) => {
				if (refreshGenerations.current.profileSync === request.profileSync)
					setProfileSync(Object.fromEntries(statuses.map((status) => [status.repositoryId, status])));
			}).catch(() => undefined);
			void getJobStatus().then(({ targets }) => {
				if (refreshGenerations.current.running !== request.running) return;
				commitActiveTargets(targets ?? []);
				const next = Object.fromEntries((targets ?? []).map((target) => [target.jobId, true]));
				const finished = Object.values(runningState.current).some(Boolean) && !Object.values(next).some(Boolean);
				commitRunning(next);
				if (finished) load();
			}).catch(() => undefined);
        }, 4000);
        return () => window.clearInterval(timer);
	}, [commitActiveTargets, commitJobs, commitRunning, jobs, load, profileSync, running]);

	useEffect(() => {
		const repositoryId = vaultSettings?.id;
		if (!repositoryId) return;
		const session = vaultSettingsSession.current;
		let active = true;
		let timer = 0;
		const ownsObservation = () => active && vaultSettingsSession.current === session && vaultSettingsOwner.current === repositoryId;
		const inspect = async () => {
			try {
				const operations = await getActiveOperations();
				if (!ownsObservation()) return;
				setVaultWorkState(operations.some((operation) => operation.repositoryId === repositoryId &&
					(operation.status === "queued" || operation.status === "running" ||
						(operation.steps ?? []).some((step) => step.status === "running"))) ? "running" : "idle");
			} catch {
				if (ownsObservation()) setVaultWorkState("unavailable");
			} finally {
				if (ownsObservation()) timer = window.setTimeout(() => void inspect(), 2000);
			}
		};
		void inspect();
		return () => {
			active = false;
			window.clearTimeout(timer);
		};
	}, [vaultSettings?.id]);

    const availableIntegrations = supportedIntegrations(vaultForm.engine, engineCatalog, integrations);
	const vaultStorageIntegrations = orderVaultStorageIntegrations(integrations.filter((candidate) =>
		candidate.capabilities.includes("storage") &&
		engineCatalog.some((descriptor) =>
			descriptor.installed &&
			descriptor.providers.some((provider) => provider.id === candidate.id && provider.supported)
		)
	));
	const integration = availableIntegrations.find((item) => item.id === vaultForm.connector);
	const storageIntegrations = integrations.filter((item) => item.capabilities.includes("storage"));
	const supportedConnectionProviderIDs = new Set(engineCatalog.filter((engine) => engine.installed).flatMap((engine) => engine.providers.filter((provider) => provider.supported).map((provider) => provider.id)));
	const connectionOptionProviders = engineCatalog.filter((engine) => engine.installed);
	const connectionIntegrations = orderVaultStorageIntegrations(
		storageIntegrations
			.filter((item) => supportedConnectionProviderIDs.has(item.id))
			.map((item) => ({
				...item,
				options: visibleVaultOptions(item.options.filter((option) =>
					connectionOptionProviders.some((engine) => engine.providers.some((provider) =>
						provider.id === item.id && provider.supported && provider.fields?.includes(option.key)))), item.id),
			})),
	);
	const connectIntegrations = connectionIntegrations;
	const connectIntegration = connectIntegrations.find((item) => item.id === connectForm.connector);
	const connectOrderedOptions = orderedConnectorOptions(connectIntegration, connectForm.connector).filter((option) =>
		connectForm.connector !== "s3" || option.key !== "storage_class" ||
		(!connectForm.coldStorage && connectDetectedEngine === "restic"));
	const connectMissingRequiredOptions = missingRequiredConnectorOptions(connectIntegration, connectForm.options);
	const createOrderedOptions = orderedConnectorOptions(integration, vaultForm.connector).filter((option) =>
		!vaultForm.coldStorage || vaultForm.connector !== "s3" || option.key !== "storage_class");
	const createMissingRequiredOptions = missingRequiredConnectorOptions(integration, vaultForm.options);
	const createUsesRcloneLogin = usesRcloneNativeLogin(vaultForm.connector);
	const createObjectLockAvailable = !vaultForm.coldStorage && ["s3", "azblob", "gcs"].includes(vaultForm.connector) &&
		engineCatalog.some((descriptor) => descriptor.id === "kopia" && descriptor.installed &&
			descriptor.providers.some((provider) => provider.id === vaultForm.connector && provider.supported));
	const connectUsesRcloneLogin = usesRcloneNativeLogin(connectForm.connector);
	const createIntegrationDescription = integration
		? providerPresentationDescription(
			engineCatalog, integration.id, integration.description, vaultForm.engine,
		)
		: "";
	const connectIntegrationDescription = connectIntegration
		? providerPresentationDescription(
			engineCatalog, connectIntegration.id, connectIntegration.description, connectDetectedEngine || undefined,
		)
		: "";
	const vaultSettingsIntegration = vaultSettings ? supportedIntegrations(vaultSettings.engine, engineCatalog, integrations).find((item) => item.id === vaultSettings.connector) : undefined;
	const currentVaultOwner = vaultOwnership
		? vaultOwnership.ownerDisplay.computerName && vaultOwnership.ownerDisplay.operatingSystem
			? `${vaultOwnership.ownerDisplay.computerName}@${vaultOwnership.ownerDisplay.operatingSystem}`
			: `profile ${vaultOwnership.ownerProfileUUID}`
		: "";
	const vaultSettingsRcloneSupported = Boolean(vaultSettings && engineCatalog.some((descriptor) =>
		descriptor.id === vaultSettings.engine &&
		descriptor.installed &&
		descriptor.providers.some((provider) =>
			provider.id === vaultSettings.connector && provider.supported
		)
	));
	const updateCreateObjectLock = (next: VaultForm) => {
		if (!next.objectLock.enrolled || next.engine === "kopia") {
			setVaultForm(next);
			return;
		}
		const selected = vaultStorageIntegrations.find((item) => item.id === next.connector);
		setVaultForm({
			...next,
			engine: "kopia",
			options: integrationOptionsForEngine(selected, "kopia", engineCatalog, next.options),
		});
	};
	    const selectedEngines = Array.from(new Set((jobForm.repositoryIds ?? []).map((id) => repos?.find((repo) => repo.id === id)?.engine).filter(Boolean))) as string[];
	const selectedJobs = (jobs ?? []).filter((job) => selectedJobIDs.includes(job.id));
	const bulkSelectedEngines = Array.from(new Set((bulkForm.repositoryIds ?? []).map((id) => repos?.find((repo) => repo.id === id)?.engine).filter(Boolean))) as string[];

	const resetConnectWorkflow = () => {
		connectInitializationGeneration.current++;
		if (connectRcloneAuth?.sessionId) void closeRcloneAuthorization(connectRcloneAuth.sessionId);
		setConnectRcloneAuth(null);
		setConnectRcloneAuthorizationAction(null);
		invalidateConnectPreview();
		const initialIntegration = connectionIntegrations[0] ?? storageIntegrations[0];
		setConnectForm(emptyVault(initialIntegration, defaultEngine));
		setConnectAdvanced(false);
		setRetryConnectionIntentId("");
		setRetryConnectionError("");
	};

	const resetConnectForNewAttempt = () => {
		connectInitializationGeneration.current++;
		if (connectRcloneAuth?.sessionId) void closeRcloneAuthorization(connectRcloneAuth.sessionId);
		setConnectRcloneAuth(null);
		setConnectRcloneAuthorizationAction(null);
		invalidateConnectPreview();
		setRetryConnectionIntentId("");
		setRetryConnectionError("");
		setConnectAdvanced(false);
		setConnectForm((current) => {
			const selectedIntegration = connectionIntegrations.find((item) => item.id === current.connector);
			return {
				...current,
				name: "",
				description: "",
				location: "",
				pendingLocation: "",
				bucket: "",
				container: "",
				prefix: "",
				host: "",
				password: "",
				passwordConfirmation: "",
				archiveWriteClass: "GLACIER",
				checkSchedule: "manual",
				maintenanceSchedule: "daily",
				objectLock: emptyObjectLock(),
				options: integrationDefaults(selectedIntegration),
			};
		});
	};

	const closeCreateRcloneAuthorization = () => {
		if (vaultSaving) return;
		if (createRcloneAuth?.sessionId) void closeRcloneAuthorization(createRcloneAuth.sessionId);
		setCreateRcloneAuth(null);
		setShowCreateRcloneAuthorization(false);
	};

	const closeConnectRcloneAuthorization = () => {
		if (connectChecking || connectSaving) return;
		if (connectRcloneAuth?.sessionId) void closeRcloneAuthorization(connectRcloneAuth.sessionId);
		setConnectRcloneAuth(null);
		setConnectRcloneAuthorizationAction(null);
	};

	    const openJob = (job?: BackupJob) => {
		jobModalSession.current++;
		jobSubmissionOwner.current = null;
		setJobSaving(false);
		setClosePrompt(null);
		setJobCopyDraft(false);
		setJobAdditionalSources([]);
	        setJobAdvanced(false);
		const initial = job ? jobToForm(job) : emptyJob;
		const form = { ...initial, repositoryIds: [...initial.repositoryIds], engineOptions: { ...initial.engineOptions } };
        setJobForm(form);
        setJobInitialForm(job ? form : null);
        setJobModal(job ?? "new");
    };

	const copyJob = (job: BackupJob) => {
		jobModalSession.current++;
		jobSubmissionOwner.current = null;
		setJobSaving(false);
		setClosePrompt(null);
		setJobAdvanced(false);
		setJobAdditionalSources([]);
		const initial = jobToForm(job);
		setJobForm({
			...initial,
			name: suggestedCopyJobName(job, jobs ?? []),
			source: "",
			repositoryIds: [...initial.repositoryIds],
			engineOptions: { ...initial.engineOptions },
		});
		setJobInitialForm(null);
		setJobCopyDraft(true);
		setJobModal("new");
	};

	const dismissJob = () => {
		const owner = jobSubmissionOwner.current;
		if (owner?.session === jobModalSession.current) return false;
		jobModalSession.current++;
		jobSubmissionOwner.current = null;
		setJobSaving(false);
		setJobCopyDraft(false);
		setJobAdditionalSources([]);
		setJobModal(null);
		return true;
	};

    const closeJob = () => {
		if (jobSubmissionOwner.current?.session === jobModalSession.current) return;
        const dirty = jobModal && jobModal !== "new" && jobInitialForm && JSON.stringify(jobForm) !== JSON.stringify(jobInitialForm);
        if (dirty) {
            setClosePrompt("job");
            return;
        }
		dismissJob();
    };

	// Multi-source creation is presentation-only fan-out. Each source remains an
	// independent saved job with its own backend identity and results. Keep this
	// grouping run-local; do not add a durable job group, batch API, or shared
	// backend schema for this UI convenience.
	const currentJobSourceDrafts = () => {
		const existingJob = jobModal && jobModal !== "new" ? jobModal : null;
		if (existingJob || jobCopyDraft) {
			return [{ name: jobForm.name, source: existingJob ? existingJob.source : jobForm.source }];
		}
		return generatedJobSourceDrafts(jobForm.name, [jobForm.source, ...jobAdditionalSources], jobs ?? []);
	};

	// Trim job labels only. Source whitespace belongs to the chosen route;
	// backend binding rejects `..` before cleanup. Host normalization here
	// could redirect the source before that validation sees the entered path.
	const validateJobSubmission = (sourceDrafts: Array<{ name: string; source: string }>) => {
	        if (sourceDrafts.some((draft) => !draft.name.trim() || !draft.source) || jobForm.repositoryIds.length === 0) {
	            toast("error", "A job name and source folder are required for every job, along with at least one vault.");
	            return null;
	        }
		const draftNames = sourceDrafts.map((draft) => asciiNoCase(draft.name.trim()));
		if (new Set(draftNames).size !== draftNames.length) {
			toast("error", "Every new backup job must have a unique name.");
			return null;
		}
		const intervalError = customScheduleError(jobForm);
		if (intervalError) {
			toast("error", intervalError);
			return null;
		}
		const schedule = scheduleValue(jobForm);
		if (schedule == null) {
			toast("error", "Cron schedule must be a valid five-field expression no longer than 256 characters.");
			return null;
		}
		const retention = parsedRetention(jobForm.retention);
		const retentionHourly = parsedRetention(jobForm.retentionHourly, true);
		const retentionDaily = parsedRetention(jobForm.retentionDaily, true);
		const retentionWeekly = parsedRetention(jobForm.retentionWeekly, true);
		const retentionMonthly = parsedRetention(jobForm.retentionMonthly, true);
		const retentionYearly = parsedRetention(jobForm.retentionYearly, true);
		if ([retention, retentionHourly, retentionDaily, retentionWeekly, retentionMonthly, retentionYearly].some((value) => value === null)) {
			toast("error", `Retention values must be whole numbers; optional values must be between 1 and ${MAX_RETENTION_COUNT.toLocaleString()}.`);
			return null;
		}
		return {
			schedule,
			retention: retention as number,
			retentionHourly: retentionHourly as number | undefined,
			retentionDaily: retentionDaily as number | undefined,
			retentionWeekly: retentionWeekly as number | undefined,
			retentionMonthly: retentionMonthly as number | undefined,
			retentionYearly: retentionYearly as number | undefined,
		};
	};

	    const saveJob = async () => {
		if (jobSubmissionOwner.current?.session === jobModalSession.current) return;
		const existingJob = jobModal && jobModal !== "new" ? jobModal : null;
		const sourceDrafts = currentJobSourceDrafts();
		const validated = validateJobSubmission(sourceDrafts);
		if (!validated) return;
		const { schedule, retention, retentionHourly, retentionDaily, retentionWeekly, retentionMonthly, retentionYearly } = validated;
			const selectedSettings = Object.fromEntries(selectedEngines.map((engine) => [engine, { additionalOptions: parsedEngineOptions(jobForm.engineOptions[engine] ?? "") }]));
		const originalTargetEngines = new Set(existingJob?.targets.map((target) => target.engine) ?? []);
		const disconnectedSettings = Object.fromEntries(Object.entries(existingJob?.engineSettings ?? {})
			.filter(([engine]) => !originalTargetEngines.has(engine as Repository["engine"]))
			.map(([engine, settings]) => [engine, { ...settings, additionalOptions: [...(settings.additionalOptions ?? [])] }]));
	        const sharedPayload = {
				repositoryIds: [...jobForm.repositoryIds],
            schedule,
			retention,
			retentionHourly,
			retentionDaily,
			retentionWeekly,
			retentionMonthly,
			retentionYearly,
            excludes: jobForm.excludes,
            tag: jobForm.tag.trim(),
			beforeScriptPath: jobForm.beforeScriptPath,
			beforeScriptMustSucceed: jobForm.beforeScriptMustSucceed,
			afterScriptPath: jobForm.afterScriptPath,
			afterScriptMustSucceed: jobForm.afterScriptMustSucceed,
				engineSettings: { ...disconnectedSettings, ...selectedSettings },
				...(existingJob ? {} : { enabled: jobForm.enabled }),
	        };
		const owner = { session: jobModalSession.current, generation: ++jobSubmissionGeneration.current };
		jobSubmissionOwner.current = owner;
		const ownsSubmission = () => jobModalSession.current === owner.session &&
			jobSubmissionGeneration.current === owner.generation && jobSubmissionOwner.current === owner;
		const existingJobId = existingJob?.id;

		setJobSaving(true);
		try {
			if (existingJobId) {
				const payload: JobInput = {
					id: existingJobId,
					name: sourceDrafts[0].name.trim(),
					source: existingJob.source,
					...sharedPayload,
				};
				const result = await updateJob(payload);
				if (result?.job) applySavedJob(result.job);
				toast(result?.warning ? "info" : "ok", result?.warning ?? `Job "${payload.name}" updated`);
				if (ownsSubmission()) setJobModal(null);
				load();
			} else if (sourceDrafts.length === 1) {
				const payload: JobInput = {
					name: sourceDrafts[0].name.trim(),
					source: sourceDrafts[0].source,
					...sharedPayload,
				};
				const result = await createJob(payload);
				if (result?.job) applySavedJob(result.job);
				toast(result?.warning ? "info" : "ok", result?.warning ?? `Job "${payload.name}" created`);
				if (ownsSubmission()) setJobModal(null);
				load();
			} else {
				const results: JobMutationResult[] = [];
				for (const draft of sourceDrafts) {
					if (!ownsSubmission()) break;
					const payload: JobInput = {
						name: draft.name.trim(),
						source: draft.source,
						...sharedPayload,
					};
					try {
						const result = await createJob(payload);
				if (result?.job) applySavedJob(result.job);
						results.push({
							name: payload.name,
							source: payload.source,
							status: result?.warning ? "warning" : "created",
							message: result?.warning ?? "Created",
						});
					} catch (error) {
						results.push({ name: payload.name, source: payload.source, status: "failed", message: (error as Error).message, payload });
					}
				}
				if (ownsSubmission()) {
					jobCreationResultSession.current++;
					jobCreationRetryOwner.current = null;
					setJobCreationResults(results);
					setJobModal(null);
					load();
				}
			}
		} catch (error) {
			if (ownsSubmission()) toast("error", (error as Error).message);
		} finally {
			if (ownsSubmission()) {
				jobSubmissionOwner.current = null;
				setJobSaving(false);
			}
		}
	};

	const retryFailedJobCreations = async () => {
		if (!jobCreationResults || jobCreationRetryOwner.current) return;
		const owner = {
			session: jobCreationResultSession.current,
			generation: ++jobCreationRetryGeneration.current,
		};
		jobCreationRetryOwner.current = owner;
		const ownsRetry = () => jobCreationResultSession.current === owner.session &&
			jobCreationRetryGeneration.current === owner.generation && jobCreationRetryOwner.current === owner;
		setJobCreationRetrying(true);
		const next = [...jobCreationResults];
		try {
			for (let index = 0; index < next.length; index++) {
				if (!ownsRetry()) break;
				const item = next[index];
				if (item.status !== "failed" || !item.payload) continue;
				try {
					const result = await createJob(item.payload);
				if (result?.job) applySavedJob(result.job);
					if (!ownsRetry()) break;
					next[index] = {
						name: item.name,
						source: item.source,
						status: result?.warning ? "warning" : "created",
						message: result?.warning ?? "Created",
					};
				} catch (error) {
					if (!ownsRetry()) break;
					next[index] = { ...item, message: (error as Error).message };
				}
				setJobCreationResults([...next]);
			}
			if (ownsRetry()) load();
		} finally {
			if (ownsRetry()) {
				jobCreationRetryOwner.current = null;
				setJobCreationRetrying(false);
			}
		}
	};

	const dismissJobCreationResults = () => {
		if (jobCreationRetryOwner.current) return false;
		jobCreationResultSession.current++;
		jobCreationRetryGeneration.current++;
		jobCreationRetryOwner.current = null;
		setJobCreationRetrying(false);
		setJobCreationResults(null);
		return true;
	};

	// Bulk edit is run-local frontend orchestration over the existing per-job
	// endpoints. Jobs stay independent, including partial failures and retries;
	// do not turn the selection into a durable group or backend bulk operation.
	const selectedJobIsActive = (job: BackupJob) => Boolean(running[job.id]) || job.targets.some(backupTargetIsActive);

	const toggleJobSelection = (jobID: string) => {
		if (bulkSaving) return;
		setSelectedJobIDs((current) => current.includes(jobID)
			? current.filter((id) => id !== jobID)
			: [...current, jobID]);
	};

	const selectAllJobs = () => {
		if (bulkSaving) return;
		setSelectedJobIDs((jobs ?? []).map((job) => job.id));
	};

	const openBulkEdit = () => {
		if (selectedJobs.length === 0) {
			toast("error", "Select at least one backup job.");
			return;
		}
		const active = selectedJobs.filter(selectedJobIsActive);
		if (active.length > 0) {
			toast("error", `Wait for these jobs to finish before bulk editing: ${active.map((job) => job.name).join(", ")}.`);
			return;
		}
		bulkSession.current++;
		bulkSubmissionOwner.current = null;
		const initial = jobToForm(selectedJobs[0]);
		setBulkForm({
			...emptyBulkEdit,
			...initial,
			repositoryIds: [],
			engineOptions: {},
			changeSchedule: false,
			changeRetention: false,
			changeExcludes: false,
			changeTag: false,
			changeScripts: false,
			replaceDestinations: false,
		});
		setBulkEditStep("edit");
		setBulkSaving(false);
		setBulkEditOpen(true);
	};

	const dismissBulkEdit = () => {
		if (bulkSubmissionOwner.current?.session === bulkSession.current) return false;
		bulkSession.current++;
		bulkSubmissionOwner.current = null;
		setBulkSaving(false);
		setBulkEditOpen(false);
		return true;
	};

	const bulkHasChanges = () => bulkForm.changeSchedule || bulkForm.changeRetention ||
		bulkForm.changeExcludes || bulkForm.changeTag || bulkForm.changeScripts || bulkForm.replaceDestinations;

	const reviewBulkEdit = () => {
		if (!bulkHasChanges()) {
			toast("error", "Choose at least one setting to change.");
			return;
		}
		const intervalError = bulkForm.changeSchedule ? customScheduleError(bulkForm) : null;
		if (intervalError) {
			toast("error", intervalError);
			return;
		}
		if (bulkForm.changeSchedule && scheduleValue(bulkForm) == null) {
			toast("error", "Cron schedule must be a valid five-field expression no longer than 256 characters.");
			return;
		}
		if (bulkForm.changeRetention) {
			const values = [
				parsedRetention(bulkForm.retention),
				parsedRetention(bulkForm.retentionHourly, true),
				parsedRetention(bulkForm.retentionDaily, true),
				parsedRetention(bulkForm.retentionWeekly, true),
				parsedRetention(bulkForm.retentionMonthly, true),
				parsedRetention(bulkForm.retentionYearly, true),
			];
			if (values.some((value) => value === null)) {
				toast("error", `Retention values must be whole numbers; optional values must be between 1 and ${MAX_RETENTION_COUNT.toLocaleString()}.`);
				return;
			}
		}
		if (bulkForm.replaceDestinations && bulkForm.repositoryIds.length === 0) {
			toast("error", "Select at least one destination vault.");
			return;
		}
		setBulkEditStep("review");
	};

	const buildBulkPayload = (job: BackupJob): JobInput => {
		const payload = jobInputFromStored(job);
		if (bulkForm.changeSchedule) {
			payload.schedule = scheduleValue(bulkForm) as string;
		}
		if (bulkForm.changeRetention) {
			payload.retention = parsedRetention(bulkForm.retention) as number;
			payload.retentionHourly = parsedRetention(bulkForm.retentionHourly, true) as number | undefined;
			payload.retentionDaily = parsedRetention(bulkForm.retentionDaily, true) as number | undefined;
			payload.retentionWeekly = parsedRetention(bulkForm.retentionWeekly, true) as number | undefined;
			payload.retentionMonthly = parsedRetention(bulkForm.retentionMonthly, true) as number | undefined;
			payload.retentionYearly = parsedRetention(bulkForm.retentionYearly, true) as number | undefined;
		}
		if (bulkForm.changeExcludes) payload.excludes = bulkForm.excludes;
		if (bulkForm.changeTag) payload.tag = bulkForm.tag.trim();
		if (bulkForm.changeScripts) {
			payload.beforeScriptPath = bulkForm.beforeScriptPath;
			payload.beforeScriptMustSucceed = bulkForm.beforeScriptMustSucceed;
			payload.afterScriptPath = bulkForm.afterScriptPath;
			payload.afterScriptMustSucceed = bulkForm.afterScriptMustSucceed;
		}
		if (bulkForm.replaceDestinations) {
			payload.repositoryIds = [...bulkForm.repositoryIds];
			const representedSettings = Object.fromEntries(bulkSelectedEngines.map((engine) => [engine, {
				additionalOptions: parsedEngineOptions(bulkForm.engineOptions[engine] ?? ""),
			}]));
			const originalEngines = new Set(job.targets.map((target) => target.engine));
			const disconnectedSettings = Object.fromEntries(Object.entries(job.engineSettings ?? {})
				.filter(([engine]) => !originalEngines.has(engine as Repository["engine"]))
				.map(([engine, settings]) => [engine, { ...settings, additionalOptions: [...(settings.additionalOptions ?? [])] }]));
			payload.engineSettings = { ...disconnectedSettings, ...representedSettings };
		}
		return payload;
	};

	const applyBulkEdit = async () => {
		if (bulkSubmissionOwner.current || selectedJobs.length === 0) return;
		const active = selectedJobs.filter(selectedJobIsActive);
		if (active.length > 0) {
			toast("error", `Bulk edit did not start because these jobs are now active: ${active.map((job) => job.name).join(", ")}.`);
			setBulkEditStep("edit");
			return;
		}
		const owner = { session: bulkSession.current, generation: ++bulkSubmissionGeneration.current };
		bulkSubmissionOwner.current = owner;
		const ownsSubmission = () => bulkSession.current === owner.session &&
			bulkSubmissionGeneration.current === owner.generation && bulkSubmissionOwner.current === owner;
		setBulkSaving(true);
		const results: JobMutationResult[] = [];
		try {
			for (const job of selectedJobs) {
				if (!ownsSubmission()) break;
				const payload = buildBulkPayload(job);
				try {
					const result = await updateJob(payload);
				if (result?.job) applySavedJob(result.job);
					results.push({
						jobId: job.id,
						name: job.name,
						status: result?.warning ? "warning" : "updated",
						message: result?.warning ?? "Updated",
					});
				} catch (error) {
					results.push({ jobId: job.id, name: job.name, status: "failed", message: (error as Error).message, payload });
				}
			}
			if (ownsSubmission()) {
				bulkResultSession.current++;
				bulkRetryOwner.current = null;
				setBulkResults({ action: "edit", items: results });
				setBulkEditOpen(false);
				load();
			}
		} finally {
			if (ownsSubmission()) {
				bulkSubmissionOwner.current = null;
				setBulkSaving(false);
			}
		}
	};

	const applyBulkEnabled = async (enabled: boolean) => {
		if (bulkSubmissionOwner.current || selectedJobs.length === 0) return;
		const owner = { session: bulkSession.current, generation: ++bulkSubmissionGeneration.current };
		bulkSubmissionOwner.current = owner;
		const ownsSubmission = () => bulkSession.current === owner.session &&
			bulkSubmissionGeneration.current === owner.generation && bulkSubmissionOwner.current === owner;
		setBulkSaving(true);
		const results: JobMutationResult[] = [];
		try {
			for (const job of selectedJobs) {
				if (!ownsSubmission()) break;
				try {
					const result = await setJobEnabled(job.id, enabled);
					results.push({
						jobId: job.id,
						name: job.name,
						status: result?.warning ? "warning" : result.changed ? "updated" : "unchanged",
						message: result?.warning ?? (result.changed ? "Updated" : "Unchanged"),
						enabled,
					});
				} catch (error) {
					results.push({ jobId: job.id, name: job.name, status: "failed", message: (error as Error).message, enabled });
				}
			}
			if (ownsSubmission()) {
				bulkResultSession.current++;
				bulkRetryOwner.current = null;
				setBulkResults({ action: enabled ? "enable" : "disable", items: results });
				load();
			}
		} finally {
			if (ownsSubmission()) {
				bulkSubmissionOwner.current = null;
				setBulkSaving(false);
			}
		}
	};

	const retryFailedBulkJobs = async () => {
		if (!bulkResults || bulkRetryOwner.current) return;
		const owner = {
			session: bulkResultSession.current,
			generation: ++bulkRetryGeneration.current,
		};
		bulkRetryOwner.current = owner;
		const ownsRetry = () => bulkResultSession.current === owner.session &&
			bulkRetryGeneration.current === owner.generation && bulkRetryOwner.current === owner;
		setBulkRetrying(true);
		const next = [...bulkResults.items];
		try {
			for (let index = 0; index < next.length; index++) {
				if (!ownsRetry()) break;
				const item = next[index];
				if (item.status !== "failed" || !item.jobId) continue;
				try {
					if (bulkResults.action === "edit") {
						if (!item.payload) continue;
						const currentJob = (jobs ?? []).find((job) => job.id === item.jobId);
						if (currentJob && selectedJobIsActive(currentJob)) {
							next[index] = { ...item, message: "Wait for this job to finish before retrying its definition update." };
							setBulkResults({ ...bulkResults, items: [...next] });
							continue;
						}
						const result = await updateJob(item.payload);
						if (result?.job) applySavedJob(result.job);
						if (!ownsRetry()) break;
						next[index] = { jobId: item.jobId, name: item.name, status: result?.warning ? "warning" : "updated", message: result?.warning ?? "Updated" };
					} else {
						const enabled = bulkResults.action === "enable";
						const result = await setJobEnabled(item.jobId, enabled);
						if (!ownsRetry()) break;
						next[index] = { jobId: item.jobId, name: item.name, status: result?.warning ? "warning" : result.changed ? "updated" : "unchanged", message: result?.warning ?? (result.changed ? "Updated" : "Unchanged"), enabled };
					}
				} catch (error) {
					if (!ownsRetry()) break;
					next[index] = { ...item, message: (error as Error).message };
				}
				setBulkResults({ ...bulkResults, items: [...next] });
			}
			if (ownsRetry()) load();
		} finally {
			if (ownsRetry()) {
				bulkRetryOwner.current = null;
				setBulkRetrying(false);
			}
		}
	};

	const dismissBulkResults = () => {
		if (bulkRetryOwner.current) return false;
		bulkResultSession.current++;
		bulkRetryGeneration.current++;
		bulkRetryOwner.current = null;
		setBulkRetrying(false);
		setBulkResults(null);
		return true;
	};

    const openRunReview = (jobId: string) => {
		if (runSubmissionOwner.current) return;
		runReviewSession.current++;
		runSubmissionOwner.current = null;
		setRunBusy("");
		setRunActiveJobID("");
		setRunResults(null);
		setRunJobID(jobId);
	};

	const dismissRunReview = () => {
		if (runSubmissionOwner.current?.session === runReviewSession.current && runSubmissionOwner.current.phase === "run") return false;
		runReviewSession.current++;
		runSubmissionOwner.current = null;
		setRunJobID("");
		setRunBusy("");
		setRunActiveJobID("");
		setRunResults(null);
		return true;
	};

    const startRun = async (job: BackupJob, repositoryId?: string) => {
		if (runSubmissionOwner.current) return;
		const reviewingMultipleTargets = runJobID === job.id;
		if (!reviewingMultipleTargets) runReviewSession.current++;
		const jobId = job.id;
		const jobName = job.name;
		const owner: RunSubmissionOwner = {
			session: runReviewSession.current,
			generation: ++runSubmissionGeneration.current,
			jobId,
			phase: "run",
		};
		runSubmissionOwner.current = owner;
		const ownsSubmission = () => runReviewSession.current === owner.session &&
			runSubmissionGeneration.current === owner.generation && runSubmissionOwner.current === owner;
        setRunBusy(repositoryId ?? "all");
		setRunActiveJobID(jobId);
        try {
			if (!ownsSubmission()) return;
            const result = await runJob(jobId, repositoryId);
			for (const admission of result.results) {
				if (admission.status === "admitted" && admission.operationId) {
					observedBackupOperations.current[admission.operationId] = {
						jobId, repositoryId: admission.repositoryId,
					};
				}
			}
            if (result.count > 0) commitRunning({ ...runningState.current, [jobId]: true });
			if (ownsSubmission() && reviewingMultipleTargets) setRunResults(result.results);
			const admittedVaultNames = result.results
				.filter((admission) => admission.status === "admitted")
				.map((admission) => job.targets.find((target) => target.repositoryId === admission.repositoryId)?.repositoryName ?? admission.repositoryId);
			if (admittedVaultNames.length === 1) {
				toast("info", `"${jobName}" started for "${admittedVaultNames[0]}".`);
			} else if (admittedVaultNames.length > 1) {
				toast("info", `"${jobName}" started for ${admittedVaultNames.length} vaults: ${admittedVaultNames.map((name) => `"${name}"`).join(", ")}.`);
			} else {
				toast("info", `No backup started for "${jobName}". Review the admission result.`);
			}
            load();
			if (ownsSubmission() && !reviewingMultipleTargets) setRunJobID("");
        } catch (error) {
			if (ownsSubmission()) toast("error", (error as Error).message);
        } finally {
			if (ownsSubmission()) {
				runSubmissionOwner.current = null;
				setRunBusy("");
				setRunActiveJobID("");
			}
        }
    };

    const toggle = async (job: BackupJob) => {
		if (jobToggleBusy) return;
		setJobToggleBusy(job.id);
        try {
			const result = await setJobEnabled(job.id, !job.enabled);
			// The PUT response is authoritative because it is written only after the
			// durable enabled-state transaction commits. Apply it before the broad
			// refresh so unrelated vault/profile reads cannot make a committed toggle
			// look pending. Keep load() to reconcile the rest of the server-owned state.
			setJobs((current) => current?.map((item) => item.id === job.id
				? { ...item, enabled: result.enabled }
				: item) ?? current);
			if (result?.warning) toast("info", result.warning);
            load();
        } catch (error) {
            toast("error", (error as Error).message);
		} finally {
			setJobToggleBusy("");
        }
    };

    const openJobDelete = (job: BackupJob) => {
		if (jobDeleteOwner.current) return;
		jobDeleteSession.current++;
		jobDeleteOwner.current = null;
		setJobDeleting(false);
		setJobDelete(job);
	};

	const dismissJobDelete = () => {
		if (jobDeleteOwner.current?.session === jobDeleteSession.current) return false;
		jobDeleteSession.current++;
		jobDeleteOwner.current = null;
		setJobDeleting(false);
		setJobDelete(null);
		return true;
	};

    const removeJob = async () => {
        if (!jobDelete) return;
		if (jobDeleteOwner.current) return;
		const jobId = jobDelete.id;
		const jobName = jobDelete.name;
		const owner = { session: jobDeleteSession.current, generation: ++jobDeleteGeneration.current };
		jobDeleteOwner.current = owner;
		const ownsSubmission = () => jobDeleteSession.current === owner.session &&
			jobDeleteGeneration.current === owner.generation && jobDeleteOwner.current === owner;
        setJobDeleting(true);
        try {
			const result = await deleteJob(jobId);
			toast(result?.warning ? "info" : "ok", result?.warning ?? `Job "${jobName}" deleted`);
            load();
			if (ownsSubmission()) setJobDelete(null);
        } catch (error) {
			if (ownsSubmission()) toast("error", (error as Error).message);
        } finally {
			if (ownsSubmission()) {
				jobDeleteOwner.current = null;
				setJobDeleting(false);
			}
        }
    };

	const openVaultCreate = (form?: VaultForm) => {
		if (createRcloneAuth?.sessionId) void closeRcloneAuthorization(createRcloneAuth.sessionId);
		setCreateRcloneAuth(null);
		setShowCreateRcloneAuthorization(false);
		vaultCreateSession.current++;
		vaultCreateSubmissionOwner.current = null;
		setVaultSaving(false);
		setVaultCreateError("");
			setVaultAdvanced(false);
			setRetryCreationIntentId("");
		const initial = form ?? emptyVault(supportedIntegrations(defaultEngine, engineCatalog, integrations)[0], defaultEngine);
		setVaultForm({ ...initial, options: { ...initial.options } });
		setShowVaultChoice(false);
		setShowVaultCreate(true);
		refreshCreationIntents();
	};

	const closeVaultCreate = () => {
		const owner = vaultCreateSubmissionOwner.current;
		if (owner?.session === vaultCreateSession.current) return false;
		vaultCreateSession.current++;
		vaultCreateSubmissionOwner.current = null;
		setVaultSaving(false);
			if (createRcloneAuth?.sessionId) void closeRcloneAuthorization(createRcloneAuth.sessionId);
			setCreateRcloneAuth(null);
			setShowCreateRcloneAuthorization(false);
			setShowVaultCreate(false);
			setRetryCreationIntentId("");
		return true;
	};

    const saveVault = async () => {
		if (vaultCreateSubmissionOwner.current?.session === vaultCreateSession.current) return;
        const location = vaultLocation(vaultForm, true);
        if (!vaultForm.name.trim() || !location || !vaultForm.password) {
            toast("error", "Name, destination, and password are required.");
            return;
        }
		if (!validVaultPassword(vaultForm.password)) {
			toast("error", "Enter a vault password without leading or trailing whitespace, line breaks, or NUL characters.");
			return;
		}
		if (!validVaultName(vaultForm.name)) {
			toast("error", `Vault names must be ${MAX_VAULT_NAME_CODE_POINTS} characters or fewer.`);
			return;
		}
		if (createMissingRequiredOptions.length > 0) {
			toast("error", `Enter required connector fields: ${createMissingRequiredOptions.map((option) => option.label).join(", ")}.`);
			return;
		}
        if (vaultForm.password !== vaultForm.passwordConfirmation) {
            toast("error", "The passwords do not match.");
            return;
        }
			const payload = {
				engine: vaultForm.engine,
			connector: vaultForm.connector,
			coldStorage: vaultForm.coldStorage,
			archiveWriteClass: vaultForm.coldStorage ? vaultForm.archiveWriteClass : undefined,
			name: vaultForm.name.trim(),
			location,
			description: vaultForm.description.trim(),
			password: vaultForm.password,
			passwordConfirmation: vaultForm.passwordConfirmation,
				options: connectorOptionsForSubmission(vaultForm, integration),
				rcloneAuthSessionId: createRcloneAuth?.sessionId,
				checkSchedule: vaultForm.checkSchedule,
				maintenanceSchedule: vaultForm.maintenanceSchedule,
				concurrencyMode: vaultForm.concurrencyMode,
				objectLock: vaultForm.objectLock,
				...(retryCreationIntentId ? { creationIntentId: retryCreationIntentId } : {}),
			};
		const owner = { session: vaultCreateSession.current, generation: ++vaultCreateSubmissionGeneration.current };
		vaultCreateSubmissionOwner.current = owner;
		const ownsSubmission = () => vaultCreateSession.current === owner.session &&
			vaultCreateSubmissionGeneration.current === owner.generation && vaultCreateSubmissionOwner.current === owner;
		setVaultSaving(true);
		setVaultCreateError("");
		try {
			setVaultProgress([]);
			const result = await createRepository(payload, (record) => { if (ownsSubmission()) appendVaultProgress(record); });
			const success = result.created ? `Encrypted vault "${payload.name}" created` : `Vault "${payload.name}" added`;
			toast(result.warning ? "info" : "ok", result.warning ?? success);
            load();
			if (ownsSubmission()) {
				if (createRcloneAuth?.sessionId) void closeRcloneAuthorization(createRcloneAuth.sessionId);
				setCreateRcloneAuth(null);
				setShowCreateRcloneAuthorization(false);
				setShowVaultCreate(false);
				setVaultAdvanced(false);
					setVaultFormState(emptyVault(integrations.find((item) => item.id === "fs"), defaultEngine));
					setRetryCreationIntentId("");
			}
		} catch (error) {
			if (ownsSubmission() && error instanceof APIError && error.code === "creation_pending") {
				const readySessionID = createRcloneAuth?.status === "ready" ? createRcloneAuth.sessionId : "";
				if (readySessionID) {
					try { await closeRcloneAuthorization(readySessionID); } catch { /* auth-store retry remains authoritative */ }
				}
				if (!ownsSubmission()) return;
				setCreateRcloneAuth(null);
				setShowCreateRcloneAuthorization(false);
				setShowVaultCreate(false);
				setVaultAdvanced(false);
				setVaultCreateError("");
				setRetryCreationIntentId("");
				setVaultFormState(emptyVault(integrations.find((item) => item.id === "fs"), defaultEngine));
				load();
				toast("info", "Use the pending vault card to retry, cancel, or forget this creation.");
				return;
			}
			const lifecycle = rcloneLifecycleFailure(error);
			const message = lifecycle.message;
			if (ownsSubmission()) {
				if (lifecycle.consumed) {
					setCreateRcloneAuth(null);
					setShowCreateRcloneAuthorization(false);
				}
				if (lifecycle.completed) {
					setShowVaultCreate(false);
					setRetryCreationIntentId("");
				}
				if (retryCreationIntentId && createUsesRcloneLogin && message.includes("reauthorization")) {
					setShowCreateRcloneAuthorization(true);
				}
				setVaultCreateError(message);
				toast("error", message);
				// A lost response can follow a committed attachment, so refresh both
				// durable pending state and attached repositories after client errors.
				load();
			}
		} finally {
			if (ownsSubmission()) {
				vaultCreateSubmissionOwner.current = null;
				setVaultSaving(false);
			}
        }
    };

	const checkExistingVault = async (selectedProfileUUID = "", chooseJoin = false, formOverride?: VaultForm) => {
		if (connectSaving) return false;
		// Canceling a selected prepared intent unlocks its destination. Submit
		// that same editable snapshot, not the prior render's pending URL.
		const submittedForm = formOverride ?? connectForm;
		const location = vaultLocation(submittedForm);
		if (!location || !submittedForm.password || !connectIntegration) {
			toast("error", "Storage type, location, and vault password are required.");
			return false;
		}
		if (!validVaultPassword(submittedForm.password)) {
			toast("error", "Enter a vault password without leading or trailing whitespace, line breaks, or NUL characters.");
			return false;
		}
		// The repository validates its existing password directly. Requiring a
		// second entry here would only duplicate a secret that is not being set.
		if (connectMissingRequiredOptions.length > 0) {
			toast("error", `Enter required connector fields: ${connectMissingRequiredOptions.map((option) => option.label).join(", ")}.`);
			return false;
		}
		const enteredOptions = connectorOptionsWithoutUnchangedDefaults(connectIntegration, connectorOptionsForSubmission(submittedForm, connectIntegration));
		// Cloud Connect takes native identity from its fresh rclone session.
		// Submit only the ordinary visible rclone wrapper options with it.
		const options = connectUsesRcloneLogin
			? Object.fromEntries(Object.entries(enteredOptions).filter(([key]) => key.startsWith("rclone_")))
			: enteredOptions;
		const storage: ExistingVaultStorageInput = {
			connector: submittedForm.connector,
			coldStorage: submittedForm.coldStorage,
			location,
			password: submittedForm.password,
			options,
			...(connectRcloneAuth?.sessionId ? { rcloneAuthSessionId: connectRcloneAuth.sessionId } : {}),
			...(selectedProfileUUID ? { profile_uuid: selectedProfileUUID } : {}),
		};
		const refiningProfile = Boolean(connectPreview && (selectedProfileUUID || chooseJoin));
		const baseFields = {
			name: submittedForm.name,
			description: submittedForm.description,
			checkSchedule: submittedForm.checkSchedule,
			maintenanceSchedule: submittedForm.maintenanceSchedule,
			concurrencyMode: submittedForm.concurrencyMode,
		};
		if (refiningProfile) {
			// Refinement reads only the selected protected profile. Keep the first
			// Check baseline intact so later selection cannot silently accept a
			// changed root or profile before final locked Add admission.
			setConnectPendingProfileChoice({ profileUUID: selectedProfileUUID, join: chooseJoin });
			setConnectProfileUUID("");
			setConnectProfileAction("join");
			setConnectProfileChoiceMade(false);
			setConnectOwnerAction("keep");
			setConnectOwnerChoiceMade(false);
			setConnectNameConflictNotice("");
			setConnectRcloneNameConflict(false);
			const initial = connectPreviewBaseFields.current;
			if (initial) setConnectForm((current) => ({ ...current, ...initial }));
			connectPreviewSession.current++;
			connectSubmissionGeneration.current++;
			connectPreviewController.current?.abort();
			connectPreviewController.current = null;
		} else {
			invalidateConnectPreview();
		}
		const session = connectPreviewSession.current;
		const controller = new AbortController();
		connectPreviewController.current = controller;
		const ownsRequest = () => connectPreviewSession.current === session && connectPreviewController.current === controller && !controller.signal.aborted;
		setConnectChecking(true);
		try {
			setVaultProgress([]);
			const response = refiningProfile
				? await selectExistingVaultProfile(storage, connectPreview?.baseline, controller.signal, (record) => { if (ownsRequest()) appendVaultProgress(record); })
				: await previewExistingVault(storage, controller.signal, (record) => { if (ownsRequest()) appendVaultProgress(record); });
			if (!ownsRequest()) return false;
			const preview = refiningProfile ? { ...response, profiles: connectPreview?.profiles } : response;
			if ((preview.profiles ?? []).filter((choice) => choice.localAttachment).length > 1) {
				throw new Error("Multiple vault profiles are attached to this computer. Connection is blocked until the identity conflict is resolved.");
			}
			setConnectDetectedEngine(preview.engine);
			const protectedStorageClass = (preview as ExistingVaultPreview & { storageClass?: string }).storageClass;
			const reviewedBaseFields = {
				...baseFields,
				checkSchedule: preview.rootIntegritySchedule ?? baseFields.checkSchedule,
				maintenanceSchedule: preview.rootMaintenanceSchedule ?? baseFields.maintenanceSchedule,
			};
			setConnectForm((current) => ({
				...current,
				engine: preview.engine,
				// Connection never asks the user to choose a storage class. Managed
				// Cold vaults carry their protected class forward; native Cold imports
				// always use GLACIER and must not inherit transient Create state.
				archiveWriteClass: preview.coldStorage ? preview.archiveWriteClass || "GLACIER" : current.archiveWriteClass,
				options: protectedStorageClass === undefined ? current.options : { ...current.options, storage_class: protectedStorageClass },
				checkSchedule: current.coldStorage ? "manual" : preview.rootIntegritySchedule ?? current.checkSchedule,
				maintenanceSchedule: preview.rootMaintenanceSchedule ?? current.maintenanceSchedule,
				objectLock: preview.mode === "profile" ? preview.objectLock ?? emptyObjectLock() : current.objectLock,
			}));
			connectReviewedStorage.current = storage;
			setConnectReviewedLocation(preview.existingVault?.candidateLocation ?? storage.location);
			if (!refiningProfile) connectPreviewBaseFields.current = reviewedBaseFields;
			setConnectPreview(preview);
			if (preview.existingVault) {
				// A same-UUID copy is an update of the one registered row. Keep the
				// current local preferences and attachment selected; an older copied
				// profile is evidence for the vault, never authority to restore jobs or
				// roll local settings backward.
				setConnectForm((current) => ({
					...current,
					name: preview.existingVault!.name,
					description: preview.existingVault!.description,
					checkSchedule: preview.existingVault!.checkSchedule,
					maintenanceSchedule: preview.existingVault!.maintenanceSchedule,
					concurrencyMode: preview.existingVault!.concurrencyMode,
					objectLock: preview.objectLock ?? current.objectLock,
				}));
				setConnectProfileUUID(preview.profile?.profile_uuid ?? "");
				setConnectProfileAction("reconnect");
				setConnectProfileChoiceMade(true);
				setConnectOwnerAction("keep");
				setConnectOwnerChoiceMade(true);
				setConnectPendingProfileChoice(null);
				setConnectNameConflictNotice("");
				setConnectRcloneNameConflict(false);
				setConnectUpdateConfirmedDigest("");
				return true;
			}
			const selectedChoice = preview.profile
				? preview.profiles?.find((choice) => choice.profile_uuid === preview.profile?.profile_uuid)
				: undefined;
			const localAttachmentBecameAuthoritative = Boolean(selectedChoice?.localAttachment);
			const decisionChoice = chooseJoin && !localAttachmentBecameAuthoritative ? undefined : selectedChoice;
			const profileChoiceIsAutomatic = preview.mode === "fallback" || Boolean(decisionChoice?.localAttachment) || usesRcloneNativeLogin(storage.connector);
			const profileChoiceWasMade = profileChoiceIsAutomatic || Boolean(selectedProfileUUID) || chooseJoin;
			setConnectProfileUUID(profileChoiceWasMade ? decisionChoice?.profile_uuid ?? "" : "");
			setConnectProfileAction(profileChoiceWasMade && decisionChoice ? (decisionChoice.localAttachment ? "reconnect" : "takeover") : "join");
			setConnectProfileChoiceMade(profileChoiceWasMade);
			setConnectOwnerAction("keep");
			setConnectOwnerChoiceMade(preview.mode === "fallback" || Boolean(decisionChoice?.vaultOwner));
			setConnectPendingProfileChoice(null);
			const rcloneName = usesRcloneNativeLogin(storage.connector)
				? normalizedRcloneFolderName(submittedForm.location)
				: "";
			if (preview.profile && decisionChoice && profileChoiceWasMade) {
				const importedName = rcloneName || preview.profile.vaultPreferences.name.trim();
				const hasRcloneConflict = Boolean(rcloneName) && (repos ?? []).some((repository) =>
					repository.id !== preview.profile!.vault_uuid && repository.name.trim().toLowerCase() === rcloneName.toLowerCase());
				const availableName = rcloneName || availableImportedVaultName(importedName, preview.profile.vault_uuid, repos);
				setConnectRcloneNameConflict(hasRcloneConflict);
				setConnectNameConflictNotice(hasRcloneConflict
					? `A vault named "${rcloneName}" already exists. Remove the existing local vault before connecting this ${connectIntegration?.label ?? "cloud"} vault.`
					: availableName === importedName ? "" :
						`A vault named "${importedName}" already exists. The imported name was changed to "${availableName}". You can edit it before connecting.`);
				setConnectForm((current) => ({ ...current, name: availableName, description: preview.profile!.vaultPreferences.description, checkSchedule: current.coldStorage ? "manual" : preview.rootIntegritySchedule ?? "manual", maintenanceSchedule: preview.rootMaintenanceSchedule ?? "manual", concurrencyMode: preview.profile!.vaultPreferences.concurrencyMode }));
			} else if (chooseJoin && !localAttachmentBecameAuthoritative) {
				const initial = connectPreviewBaseFields.current ?? reviewedBaseFields;
				const refreshedInitial = {
					...initial,
					checkSchedule: submittedForm.coldStorage ? "manual" : reviewedBaseFields.checkSchedule,
					maintenanceSchedule: reviewedBaseFields.maintenanceSchedule,
				};
				connectPreviewBaseFields.current = refreshedInitial;
				setConnectForm((current) => ({ ...current, ...refreshedInitial }));
			} else if (rcloneName) {
				const hasRcloneConflict = (repos ?? []).some((repository) => repository.name.trim().toLowerCase() === rcloneName.toLowerCase());
				setConnectRcloneNameConflict(hasRcloneConflict);
				setConnectNameConflictNotice(hasRcloneConflict
					? `A vault named "${rcloneName}" already exists. Remove the existing local vault before connecting this ${connectIntegration?.label ?? "cloud"} vault.`
					: "");
				setConnectForm((current) => ({ ...current, name: rcloneName }));
			}
			return true;
		} catch (error) {
			if (!ownsRequest() || (error as { name?: string }).name === "AbortError") return false;
			if (!refiningProfile) {
				setConnectPreview(null);
			} else {
				// A failed refinement leaves no reviewed choice. Clearing the profile and
				// its dependent owner decision avoids presenting stale imported settings
				// as though the newly requested profile had been accepted.
				setConnectProfileUUID("");
				setConnectProfileAction("join");
				setConnectProfileChoiceMade(false);
				setConnectOwnerAction("keep");
				setConnectOwnerChoiceMade(false);
				setConnectNameConflictNotice("");
				setConnectRcloneNameConflict(false);
				const initial = connectPreviewBaseFields.current;
				if (initial) setConnectForm((current) => ({ ...current, ...initial }));
			}
			setConnectPendingProfileChoice(null);
			toast("error", (error as Error).message);
			return false;
		} finally {
			if (ownsRequest()) {
				connectPreviewController.current = null;
				setConnectChecking(false);
			}
		}
	};

	const retryPendingConnection = async (selectedIntentId = retryConnectionIntentId) => {
		if (connectSaving) return;
		const location = vaultLocation(connectForm);
		if (!selectedIntentId || !location || !connectForm.password) {
			toast("error", "Enter the pending destination credentials and vault password.");
			return;
		}
		if (!validVaultPassword(connectForm.password)) {
			toast("error", "Enter a vault password without leading or trailing whitespace, line breaks, or NUL characters.");
			return;
		}
		if (connectMissingRequiredOptions.length > 0) {
			toast("error", `Enter required connector fields: ${connectMissingRequiredOptions.map((option) => option.label).join(", ")}.`);
			return;
		}
		const payload = {
			password: connectForm.password,
			// Native rclone identity and credentials come from the reviewed intent
			// and saved config (or the new auth session), not form option defaults.
			options: Object.fromEntries((connectIntegration?.options ?? [])
				.filter((option) => (option.credential || option.secret) && !connectUsesRcloneLogin)
				.map((option) => [option.key, connectForm.options[option.key] ?? ""])),
			rcloneAuthSessionId: connectRcloneAuth?.sessionId,
			intentId: selectedIntentId,
		};
		const session = connectPreviewSession.current;
		const submission = ++connectSubmissionGeneration.current;
		const ownsSubmission = () => connectSubmissionGeneration.current === submission && connectPreviewSession.current === session;
		setConnectSaving(true);
		setRetryConnectionIntentId(selectedIntentId);
		setRetryConnectionError("");
		try {
			setVaultProgress([]);
			const result = await retryExistingVaultConnection(payload, (record) => { if (ownsSubmission()) appendVaultProgress(record); });
			clearReconnectObservation(result.id);
			toast(result.profilePending ? "info" : "ok", result.warning ?? (result.name ? `Vault "${result.name}" connected.` : "Vault connected."));
			load();
			if (ownsSubmission()) {
				setConnectSaving(false);
				resetConnectWorkflow();
				setShowVaultConnect(false);
			}
		} catch (error) {
			if (ownsSubmission()) {
				setRetryConnectionError((error as Error).message);
				refreshConnectionIntents();
				const lifecycle = rcloneLifecycleFailure(error, true);
				if (lifecycle.consumed) {
					setConnectRcloneAuth(null);
					setConnectRcloneAuthorizationAction(null);
				}
				if (lifecycle.completed) {
					resetConnectWorkflow();
					setShowVaultConnect(false);
					load();
				}
				const savedConfigRetryFailed = connectUsesRcloneLogin && !payload.rcloneAuthSessionId &&
					error instanceof APIError && error.status === 400;
				const savedConfigMissing = savedConfigRetryFailed &&
					error.message === "the pending vault requires native rclone reauthorization before retry";
				const nativeAdmissionFailed = savedConfigRetryFailed &&
					error.message === "the detected restic vault rejected the password or could not be validated";
				// A locally safe config can still fail native admission because of
				// expired authorization, a wrong vault password, or a network failure.
				// Offer a deliberate login fallback without claiming which occurred.
				if (savedConfigMissing || nativeAdmissionFailed) {
					setConnectRcloneAuthorizationAction("retry");
					toast("error", savedConfigMissing
						? "The saved rclone configuration is unavailable. Authorize rclone to retry this pending connection."
						: `${error.message} Check the vault password and provider connection. If the saved authorization has expired, authorize rclone and retry.`);
				} else {
					toast("error", lifecycle.message);
				}
			}
		} finally {
			if (ownsSubmission()) setConnectSaving(false);
		}
	};

	const saveExistingVault = async () => {
		if (connectSaving) return;
		const preview = connectPreview;
		const storage = connectReviewedStorage.current;
		if (!preview || !storage || !validVaultName(connectForm.name)) { toast("error", `Review the vault name and keep it to ${MAX_VAULT_NAME_CODE_POINTS} characters or fewer before connecting.`); return; }
		if (preview.existingVault && connectUpdateConfirmedDigest !== connectUpdateReviewDigest) {
			toast("error", "Review and confirm the exact Update existing vault changes before continuing.");
			return;
		}
		// The profile and owner controls state the transfer consequences, and the
		// final Connect vault action is their confirmation boundary. A browser
		// confirmation here would duplicate those deliberate choices without adding
		// authority; the backend still revalidates the exact attachment transition.
		const vaultName = connectForm.name.trim();
		const reviewedOptions = { ...storage.options };
		if (!connectForm.coldStorage && connectForm.connector === "s3" && connectDetectedEngine === "restic") {
			// Ordinary S3 storage class is a creation-time advanced choice, not a
			// connection decision. Carry a managed vault's reviewed protected value
			// without exposing it as an editable or disabled reconnect field.
			const storageClass = connectForm.options.storage_class ?? "";
			if (storageClass) reviewedOptions.storage_class = storageClass;
			else delete reviewedOptions.storage_class;
		}
		const maintenanceScheduleForSubmission = connectPreview?.mode === "profile" && !selectedConnectProfile?.vaultOwner &&
			connectOwnerAction !== "takeover"
			? preview.rootMaintenanceSchedule ?? connectPreviewBaseFields.current?.maintenanceSchedule ?? connectForm.maintenanceSchedule
			: connectForm.maintenanceSchedule;
		const checkScheduleForSubmission = connectPreview?.mode === "profile" && !selectedConnectProfile?.vaultOwner &&
			connectOwnerAction !== "takeover"
			? preview.rootIntegritySchedule ?? connectPreviewBaseFields.current?.checkSchedule ?? connectForm.checkSchedule
			: connectForm.checkSchedule;
		const payload = { ...storage,
			reviewedVaultUUID: preview.vaultUUID,
			options: reviewedOptions,
			archiveWriteClass: connectForm.coldStorage ? connectForm.archiveWriteClass : undefined,
			profile_uuid: connectProfileUUID || undefined,
			profileAction: connectProfileAction,
			ownerAction: connectOwnerAction,
			updateExistingVault: Boolean(preview.existingVault),
			updateExistingVaultReview: preview.existingVault ? {
				vaultUUID: preview.existingVault.id,
				previewDigest: preview.digest,
				previousLocation: preview.existingVault.location,
				location: connectReviewedLocation,
				name: vaultName,
				description: connectForm.description.trim(),
				checkSchedule: checkScheduleForSubmission,
				maintenanceSchedule: maintenanceScheduleForSubmission,
				concurrencyMode: connectForm.concurrencyMode,
			} : undefined,
				rcloneAuthSessionId: connectRcloneAuth?.sessionId,
				mode: preview.mode, digest: preview.digest,
			// Profile choice is the import boundary. The backend derives the full
			// job and snapshot set so a client cannot submit a partial recovery.
			name: vaultName, description: connectForm.description.trim(), checkSchedule: checkScheduleForSubmission, maintenanceSchedule: maintenanceScheduleForSubmission,
			concurrencyMode: connectForm.concurrencyMode,
			objectLock: connectForm.objectLock,
		};
		const session = connectPreviewSession.current;
		const submission = ++connectSubmissionGeneration.current;
		const ownsSubmission = () => connectSubmissionGeneration.current === submission && connectPreviewSession.current === session;
		setConnectSaving(true);
		try {
			setVaultProgress([]);
			const result = await connectExistingVault(payload, (record) => { if (ownsSubmission()) appendVaultProgress(record); });
			clearReconnectObservation(result.id);
			toast(result.profilePending ? "info" : "ok", result.warning ?? `Vault "${result.name ?? vaultName}" connected.`);
			load();
			if (ownsSubmission()) {
				setConnectSaving(false);
				resetConnectWorkflow();
				setShowVaultConnect(false);
			}
		} catch (error) {
			if (ownsSubmission()) {
				const lifecycle = rcloneLifecycleFailure(error, true);
				if (lifecycle.consumed) {
					setConnectRcloneAuth(null);
					setConnectRcloneAuthorizationAction(null);
				}
				if (lifecycle.completed) {
					resetConnectWorkflow();
					setShowVaultConnect(false);
					load();
				}
				if (error instanceof APIError && error.code === "vault_review_changed") invalidateConnectPreview(true);
				toast("error", lifecycle.message);
			}
		} finally {
			if (ownsSubmission()) setConnectSaving(false);
		}
	};

	const clearVaultSettingsTransient = () => {
		toolRequestController.current = null;
		setDormantJobs([]);
		setDormantBusy("");
		setVaultOwnership(null);
		setVaultOwnershipPresentation("unverified");
		setVaultOwnershipBusy(false);
		setVaultPasswordForm({ password: "", confirmation: "" });
		setVaultPasswordRecovery(null);
		setVaultPasswordChangeBusy(false);
		setCareSaving(false);
		setToolBusy("");
		setToolOutput("");
	};

	const vaultSettingsSessionIsCurrent = (session: number, repositoryId: string) =>
		vaultSettingsSession.current === session && vaultSettingsOwner.current === repositoryId;
	const applyAuthoritativeVaultCare = (repositoryId: string, status: VaultOwnershipStatus) => {
		// Ownership reads come from the protected root. Replace only root-owned
		// care fields; concurrency and auto-unlock remain this profile's settings.
		setCheckSchedule(status.integritySchedule);
		setMaintenanceSchedule(status.maintenanceSchedule);
		setObjectLock(status.objectLock);
		setVaultInitialSchedules((current) => current && ({
			...current,
			checkSchedule: status.integritySchedule,
			maintenanceSchedule: status.maintenanceSchedule,
			objectLock: status.objectLock,
		}));
		setVaultSettings((current) => current && ({
			...current,
			checkSchedule: status.integritySchedule,
			maintenanceSchedule: status.maintenanceSchedule,
			objectLock: status.objectLock,
		}));
		setRepos((current) => current?.map((repo) => repo.id === repositoryId ? ({
			...repo,
			checkSchedule: status.integritySchedule,
			maintenanceSchedule: status.maintenanceSchedule,
			objectLock: status.objectLock,
		}) : repo) ?? current);
	};

	const detectVaultOwnership = (repo: Repository, session: number) => {
		const request = ++vaultOwnershipRequest.current;
		setVaultOwnership(null);
		// Owner controls stay absent until this open's authoritative read finishes.
		setVaultOwnershipPresentation("checking");
		void getVaultOwnership(repo.id)
			.then((status) => {
				if (!vaultSettingsSessionIsCurrent(session, repo.id) || vaultOwnershipRequest.current !== request) return;
				applyAuthoritativeVaultCare(repo.id, status);
				setVaultOwnership(status);
				setVaultOwnershipPresentation(status.isOwner ? "owner" : "nonowner");
			})
			.catch(() => {
				if (!vaultSettingsSessionIsCurrent(session, repo.id) || vaultOwnershipRequest.current !== request) return;
				setVaultOwnership(null);
				setVaultOwnershipPresentation("unverified");
			});
	};

	const dismissVaultSettings = () => {
		vaultOwnershipRequest.current++;
		vaultSettingsSession.current++;
		vaultSettingsOwner.current = "";
		setVaultReconnectAfterClose(null);
		setVaultSettings(null);
		setVaultWorkState("idle");
		setVaultSettingsAdvanced(false);
		setVaultInitialSchedules(null);
		clearVaultSettingsTransient();
	};

    const openVaultSettings = (repo: Repository) => {
		const session = ++vaultSettingsSession.current;
		vaultSettingsOwner.current = repo.id;
		clearVaultSettingsTransient();
		setVaultWorkState("checking");
		setClosePrompt(null);
        setVaultSettings(repo);
        const schedules = {
            checkSchedule: repo.checkSchedule || "manual",
            maintenanceSchedule: repo.maintenanceSchedule || "weekly",
			concurrencyMode: repo.concurrencyMode || "native",
			autoUnlock: repo.autoUnlock !== false,
			objectLock: repo.objectLock ?? emptyObjectLock(),
        };
        setCheckSchedule(schedules.checkSchedule);
        setMaintenanceSchedule(schedules.maintenanceSchedule);
		setConcurrencyMode(schedules.concurrencyMode);
		setAutoUnlock(schedules.autoUnlock);
		setObjectLock(schedules.objectLock);
        setVaultInitialSchedules(schedules);
		void getDormantRecoveryJobs(repo.id)
			.then((jobs) => { if (vaultSettingsSessionIsCurrent(session, repo.id)) setDormantJobs(ownedDormantJobs(repo.id, jobs)); })
			.catch((error: Error) => { if (vaultSettingsSessionIsCurrent(session, repo.id)) toast("error", error.message); });
		detectVaultOwnership(repo, session);
		void getVaultPasswordChangeStatus(repo.id)
			.then((status) => { if (vaultSettingsSessionIsCurrent(session, repo.id)) setVaultPasswordRecovery(status); })
			.catch((error: Error) => {
				if (vaultSettingsSessionIsCurrent(session, repo.id) && (!(error instanceof APIError) || error.status !== 404)) toast("error", error.message);
			});
    };

	const openSavedVaultReconnect = async (repository: Repository) => {
		if (connectSaving) return;
		const repositoryID = repository.id;
		resetConnectWorkflow();
		const initialization = ++connectInitializationGeneration.current;
		try {
			// Reconnect only prefills the ordinary Connect existing vault form from
			// local saved fields. Remote access starts when the user submits Check.
			const fields = await getRepositoryReconnectFields(repositoryID);
			if (connectInitializationGeneration.current !== initialization) return;
			const integration = connectionIntegrations.find((item) => item.id === fields.connector);
			if (connectInitializationGeneration.current !== initialization) return;
			if (!integration) throw new Error("The saved vault's storage type is unavailable for Connect existing vault.");
			const restored = restoreVaultDestinationFields({
				...emptyVault(integration, repository.engine),
				connector: fields.connector,
				coldStorage: fields.coldStorage,
				archiveWriteClass: fields.archiveWriteClass ?? "GLACIER",
				location: fields.location,
				name: fields.name,
				description: fields.description,
				// Reconnect prefills the visible password and connector secret inputs.
				// Preserve this prefill; do not require re-entry. Confirmation belongs
				// to creation, where the user chooses a new vault password.
				password: fields.password,
				passwordConfirmation: "",
				options: { ...integrationDefaults(integration), ...fields.options },
				checkSchedule: fields.checkSchedule || "manual",
				maintenanceSchedule: fields.maintenanceSchedule || "weekly",
				concurrencyMode: fields.concurrencyMode || "native",
				objectLock: fields.objectLock ?? emptyObjectLock(),
			});
			// Only values represented by ordinary Connect existing vault inputs are
			// prefilled. pendingLocation and hidden options would change what Check
			// submits even when the user sees the same form values.
			const options = usesRcloneNativeLogin(fields.connector)
				? {}
				: connectorOptionsForSubmission(restored, integration);
			delete options.root;
			if (fields.connector === "s3") {
				delete options.storage_class;
				try {
					const savedURL = s3LocationURL(fields.location);
					if (!fields.options.endpoint?.trim() && integration.options.some((option) => option.key === "endpoint")) {
						// An S3 address can supply the endpoint without a saved option.
						// Show that effective endpoint in the ordinary editable input.
						const hasBucketPath = Boolean(savedURL.pathname.replace(/^\/+|\/+$/g, ""));
						const host = hasBucketPath ? savedURL.host.toLowerCase() : "s3.amazonaws.com";
						const scheme = options.use_tls?.trim().toLowerCase() === "false" ? "http" : "https";
						options.endpoint = `${scheme}://${host}`;
						if (hasBucketPath && savedURL.port && integration.options.some((option) => option.key === "port")) options.port = savedURL.port;
						}
				} catch { /* Check validates the visible S3 fields. */ }
			}
			if (fields.connector === "sftp") {
				try {
					const savedURL = new URL(fields.location);
					if (savedURL.username && integration.options.some((option) => option.key === "username")) options.username = decodeURIComponent(savedURL.username);
					if (savedURL.port && integration.options.some((option) => option.key === "port")) options.port = savedURL.port;
				} catch { /* Check validates the visible SFTP fields. */ }
			}
			setConnectForm({ ...restored, pendingLocation: "", options });
			if (vaultSettingsOwner.current === repositoryID) dismissVaultSettings();
			setShowVaultConnect(true);
			// Saved local profiles use this same Connect screen. Refresh the
			// credential-free intents so an unfinished attempt at this destination
			// appears in context here as well as in Add a vault.
			refreshConnectionIntents();
		} catch (error) {
			if (connectInitializationGeneration.current === initialization) toast("error", (error as Error).message);
		}
	};
	const vaultCareHasUnsavedChanges = () => Boolean(vaultSettings && vaultInitialSchedules && (
		checkSchedule !== vaultInitialSchedules.checkSchedule ||
		maintenanceSchedule !== vaultInitialSchedules.maintenanceSchedule ||
		concurrencyMode !== vaultInitialSchedules.concurrencyMode ||
		autoUnlock !== vaultInitialSchedules.autoUnlock ||
		JSON.stringify(objectLock) !== JSON.stringify(vaultInitialSchedules.objectLock)
	));
	const requestSavedVaultReconnect = (repository: Repository) => {
		if (vaultCareHasUnsavedChanges()) {
			setVaultReconnectAfterClose(repository);
			setClosePrompt("vault");
			return;
		}
		void openSavedVaultReconnect(repository);
	};

	const takeOverVaultOwnership = async () => {
		if (!vaultSettings || !vaultOwnership || vaultOwnership.isOwner || vaultOwnershipBusy) return;
		// The takeover button follows the full ownership explanation and is the
		// confirmation boundary. Do not add a second browser
		// prompt; server-side ownership and stale-review checks remain authoritative.
		const repositoryId = vaultSettings.id;
		const session = vaultSettingsSession.current;
		const reviewedOwnerProfileUUID = vaultOwnership.ownerProfileUUID;
		setVaultOwnershipBusy(true);
		try {
			const next = await forceVaultOwnershipTakeover(repositoryId, reviewedOwnerProfileUUID);
			if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
			if (next.isOwner) applyAuthoritativeVaultCare(repositoryId, next);
			setVaultOwnership(next);
			setVaultOwnershipPresentation(next.isOwner ? "owner" : "nonowner");
			toast("ok", "Vault ownership transferred to this computer");
		} catch (error) {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) toast("error", (error as Error).message);
		} finally {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) setVaultOwnershipBusy(false);
		}
	};

	const submitVaultPasswordChange = async () => {
		if (!vaultSettings || !vaultOwnership?.isOwner || vaultPasswordChangeBusy || vaultPasswordRecovery) return;
		if (!validVaultPassword(vaultPasswordForm.password)) {
			toast("error", "Enter a vault password without leading or trailing whitespace, line breaks, or NUL characters.");
			return;
		}
		if (vaultPasswordForm.password !== vaultPasswordForm.confirmation) {
			toast("error", "The vault passwords do not match.");
			return;
		}
		const repositoryId = vaultSettings.id;
		const session = vaultSettingsSession.current;
		const submitted = { ...vaultPasswordForm };
		setVaultPasswordChangeBusy(true);
		try {
			const result = await changeVaultPassword(repositoryId, submitted.password, submitted.confirmation);
			if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
			if (result.repositoryId !== repositoryId) throw new Error("Password-change response did not match the open vault.");
			setVaultPasswordForm({ password: "", confirmation: "" });
			setVaultPasswordRecovery(result.phase === "completed" ? null : result);
			toast(vaultPasswordChangeNoticeKind(result), vaultPasswordChangeNotice(result));
		} catch (error) {
			if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
			if (error instanceof APIError && error.passwordChangeResult?.repositoryId === repositoryId) setVaultPasswordRecovery(error.passwordChangeResult);
			toast("error", (error as Error).message);
		} finally {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) setVaultPasswordChangeBusy(false);
		}
	};

	const retryPendingVaultPasswordChange = async () => {
		if (!vaultSettings || !vaultPasswordRecovery || vaultPasswordChangeBusy) return;
		const repositoryId = vaultSettings.id;
		const session = vaultSettingsSession.current;
		setVaultPasswordChangeBusy(true);
		try {
			const result = await retryVaultPasswordChange(repositoryId);
			if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
			if (result.repositoryId !== repositoryId) throw new Error("Password-change response did not match the open vault.");
			setVaultPasswordRecovery(result.phase === "completed" ? null : result);
			toast(vaultPasswordChangeNoticeKind(result), vaultPasswordChangeNotice(result));
		} catch (error) {
			if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
			if (error instanceof APIError && error.passwordChangeResult?.repositoryId === repositoryId) setVaultPasswordRecovery(error.passwordChangeResult);
			toast("error", (error as Error).message);
		} finally {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) setVaultPasswordChangeBusy(false);
		}
	};

	const closeVault = () => {
		if (vaultCareHasUnsavedChanges()) {
			setVaultReconnectAfterClose(null);
			setClosePrompt("vault");
			return;
        }
		dismissVaultSettings();
    };

	const discardChanges = () => {
		if (closePrompt === "job") {
			dismissJob();
		} else if (closePrompt === "vault") {
			const reconnect = vaultReconnectAfterClose;
			setVaultReconnectAfterClose(null);
			dismissVaultSettings();
			if (reconnect) void openSavedVaultReconnect(reconnect);
		}
		setClosePrompt(null);
	};

	const saveChangesBeforeClose = () => {
		const reconnect = vaultReconnectAfterClose;
		setVaultReconnectAfterClose(null);
		setClosePrompt(null);
		if (closePrompt === "job") {
			void saveJob();
		} else if (closePrompt === "vault") {
			void saveCare(reconnect);
		}
	};

	const saveCare = async (reconnectAfterSave: Repository | null = null) => {
        if (!vaultSettings || vaultWorkState !== "idle") return;
		const repositoryId = vaultSettings.id;
		const repositoryName = vaultSettings.name;
		const session = vaultSettingsSession.current;
        setCareSaving(true);
        try {
			const result = await updateRepositorySchedules({
				repositoryId,
                checkSchedule,
				maintenanceSchedule,
				concurrencyMode,
				autoUnlock,
				objectLock,
				...(vaultOwnershipPresentation !== "owner" ? { profilePreferencesOnly: true } : {}),
            });
			if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
			toast(result?.warning ? "info" : "ok", result?.warning ?? `Vault "${repositoryName}" settings saved`);
			dismissVaultSettings();
			load();
			if (reconnectAfterSave) void openSavedVaultReconnect(reconnectAfterSave);
        } catch (error) {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) toast("error", (error as Error).message);
        } finally {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) setCareSaving(false);
        }
    };

    const runTool = async (kind: "check" | "maintenance") => {
        if (!vaultSettings) return;
		const repositoryId = vaultSettings.id;
		const session = vaultSettingsSession.current;
		const controller = kind === "maintenance" && vaultSettings.coldStorage ? new AbortController() : null;
		toolRequestController.current = controller;
        setToolBusy(kind);
        setToolOutput("");
        try {
			const result = kind === "check"
				? await checkRepository(repositoryId)
				: controller
					? await runMaintenance(repositoryId, controller.signal)
					: await runMaintenance(repositoryId);
			if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
            setToolOutput(result.output || "Completed with no output.");
            toast("ok", kind === "check" ? "Integrity check passed" : "Maintenance completed");
			try {
				const refreshed = await getRepository(repositoryId);
				if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
				setVaultSettings(refreshed);
				setRepos((current) => current?.some((repo) => repo.id === repositoryId)
					? current.map((repo) => repo.id === repositoryId ? refreshed : repo)
					: current);
			} catch (error) {
				if (vaultSettingsSessionIsCurrent(session, repositoryId)) {
					toast("error", `Care completed, but vault status could not be refreshed: ${(error as Error).message}`);
				}
			}
        } catch (error) {
			if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
			if ((error as Error).name === "AbortError") {
				const message = "Cold storage reclamation was interrupted locally. Provider restore requests may continue.";
				setToolOutput(message);
				toast("info", message);
			} else {
				setToolOutput((error as Error).message);
				toast("error", (error as Error).message);
			}
        } finally {
			if (toolRequestController.current === controller) toolRequestController.current = null;
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) setToolBusy("");
        }
    };

	const cancelColdMaintenance = () => toolRequestController.current?.abort();

    const openVaultDelete = (repository: Repository) => {
		if (vaultRemovalInFlight.current.has(repository.id)) return;
		setVaultDelete(repository);
	};

	const dismissVaultDelete = () => {
		setVaultDelete(null);
		return true;
	};

	const removeVault = async (repository: Repository, discardRecoveryProfile = false) => {
		if (vaultRemovalInFlight.current.has(repository.id)) return;
		vaultRemovalInFlight.current.add(repository.id);
		// The request remains foreground on the server so its profile publication,
		// lock, and deletion safeguards are unchanged. Only the dialog gives way
		// to card-local progress while other UI work can continue.
		setVaultDelete(null);
		setVaultRemoval((current) => ({ ...current, [repository.id]: { phase: "removing" } }));
        try {
			const result = discardRecoveryProfile
				? await deleteRepository(repository.id, true)
				: await deleteRepository(repository.id);
			toast(result?.warning ? "info" : "ok", result?.warning ?? `Vault "${repository.name}" removed from Replicaro`);
			setVaultRemoval((current) => { const next = { ...current }; delete next[repository.id]; return next; });
            load();
        } catch (error) {
			if (error instanceof APIError && error.code === "vault_removal_cleanup_required") {
				setVaultRemoval((current) => { const next = { ...current }; delete next[repository.id]; return next; });
				toast("error", `Vault "${repository.name}" was removed from Replicaro, but local cleanup needs attention: ${error.message}`);
				load();
				return;
			}
			const phase = !discardRecoveryProfile && error instanceof APIError && error.code === "vault_profile_sync_required"
				? "profile_error" : "error";
			setVaultRemoval((current) => ({ ...current, [repository.id]: { phase, message: (error as Error).message } }));
			// A lost response may follow a committed delete. Reload the authoritative
			// inventory before leaving an error overlay on a possibly absent card.
			load();
        } finally {
			vaultRemovalInFlight.current.delete(repository.id);
        }
    };

	const refreshConnectionIntents = () => void getRepositoryConnectionIntents().then(setConnectionIntents).catch((error: Error) => toast("error", error.message));
	const refreshCreationIntents = async () => {
		const generation = ++refreshGenerations.current.creations;
		try {
			const intents = await getRepositoryCreationIntents();
			if (refreshGenerations.current.creations !== generation) return null;
			setCreationIntents(intents);
			return intents;
		} catch (error) {
			if (refreshGenerations.current.creations === generation) toast("error", (error as Error).message);
			return null;
		}
	};
	const retryCreation = (intent: RepositoryCreationIntent) => {
		const selected = supportedIntegrations(intent.engine, engineCatalog, integrations)
			.find((item) => item.id === intent.connector);
		const form = restoreVaultDestinationFields({
			...emptyVault(selected, intent.engine),
			name: intent.name,
			connector: intent.connector,
			coldStorage: intent.coldStorage,
			archiveWriteClass: intent.archiveWriteClass ?? "GLACIER",
			location: intent.location,
			description: intent.description,
			options: { ...integrationDefaults(selected), ...(intent.reviewedOptions ?? {}) },
			checkSchedule: intent.checkSchedule,
			maintenanceSchedule: intent.maintenanceSchedule,
			concurrencyMode: intent.concurrencyMode || "native",
			objectLock: intent.objectLock ?? emptyObjectLock(),
		});
		openVaultCreate(form);
		setRetryCreationIntentId(intent.id);
	};
	const removePendingCreation = async (intent: RepositoryCreationIntent) => {
		if (creationRemovalBusy) return;
		setCreationRemovalBusy(intent.id);
		try {
			const result = await deleteRepositoryCreationIntent(intent.id);
			toast(result?.warning ? "info" : "ok", result?.warning ?? (intent.phase === "prepared" ? "Pending creation cancelled." : "Pending creation forgotten. Remote repository content was not deleted."));
			refreshCreationIntents();
		} catch (error) {
			toast("error", (error as Error).message);
		} finally {
			setCreationRemovalBusy("");
		}
	};
	const startNewConnectionCheck = async (intent: RepositoryConnectionIntent) => {
		if (intent.state !== "prepared" || connectChecking || connectSaving) return;
		const session = connectPreviewSession.current;
		setConnectChecking(true);
		try {
			// Only a prepared intent can be cancelled. Once publication starts,
			// retry-forward recovery remains the sole safe attachment path.
			await cancelRepositoryConnectionIntent(intent.id);
			// Closing, changing the destination, or reopening Connect invalidates
			// this action. A late cancellation must not launch its old Check into
			// the new form or clear the new session's busy/retry state.
			if (connectPreviewSession.current !== session) return;
			setConnectionIntents((current) => current.filter((item) => item.id !== intent.id));
			setRetryConnectionIntentId("");
			setRetryConnectionError("");
			// The saved review locked its destination inputs. Once the prepared
			// attempt is canceled, the fresh Check must leave those inputs editable
			// even if native validation fails and the preview never appears.
			const editableForm = { ...connectForm, pendingLocation: "" };
			setConnectForm(editableForm);
			if (connectUsesRcloneLogin) setConnectRcloneAuthorizationAction("check");
			else await checkExistingVault("", false, editableForm);
		} catch (error) {
			if (connectPreviewSession.current === session) toast("error", (error as Error).message);
		} finally {
			if (connectPreviewSession.current === session) setConnectChecking(false);
		}
	};
	const selectConnectionIntent = (intent: RepositoryConnectionIntent) => {
		const selected = storageIntegrations.find((item) => item.id === intent.connector);
		const reviewedOptions = Object.fromEntries(Object.entries(intent.reviewedOptions ?? {}).filter(([key]) =>
			!(selected?.options ?? []).some((option) => option.key === key && (option.credential || option.secret))));
		const sftpURL = (() => { try { return intent.connector === "sftp" ? new URL(intent.location) : null; } catch { return null; } })();
		const sftpAddressOptions = sftpURL ? {
			...(sftpURL.username ? { username: decodeURIComponent(sftpURL.username) } : {}),
			...(sftpURL.port ? { port: sftpURL.port } : {}),
		} : {};
		const s3EndpointOption = ((): Record<string, string> => {
			if (intent.connector !== "s3" || reviewedOptions.endpoint) return {};
			try {
				const url = new URL(intent.location);
				return { endpoint: `${reviewedOptions.use_tls === "false" ? "http" : "https"}://${url.pathname.replace(/\/+$/, "") ? url.host : "s3.amazonaws.com"}` };
			} catch { return {}; }
		})();
		invalidateConnectPreview();
		setRetryConnectionError("");
		setRetryConnectionIntentId(intent.id);
		// Restore the saved non-secret review before credential entry. The
		// destination and option controls then remain fixed to that intent;
		// native/root/attachment validation still happens on the server.
		setConnectForm((current) => restoreVaultDestinationFields({
			...current,
			connector: intent.connector,
			coldStorage: Boolean(intent.coldStorage),
			archiveWriteClass: intent.archiveWriteClass ?? "GLACIER",
			location: intent.location,
			options: { ...integrationDefaults(selected), ...reviewedOptions, ...s3EndpointOption, ...sftpAddressOptions,
				...Object.fromEntries((selected?.options ?? [])
					.filter((option) => (option.credential || option.secret) && current.options[option.key])
					.map((option) => [option.key, current.options[option.key]])) },
		}));
	};
	const changeDormant = async (repositoryId: string, jobId: string, action: "restore" | "discard") => {
		const session = vaultSettingsSession.current;
		const operation = `${action}:${jobId}`;
		setDormantBusy(operation);
		try {
			const result = action === "restore" ? await restoreDormantRecoveryJob(repositoryId, jobId) : await discardDormantRecoveryJob(repositoryId, jobId);
			if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
			toast(result.warning ? "info" : "ok", result.warning ?? `Dormant job ${action === "restore" ? "restored disabled" : "discarded"}`);
			const jobs = await getDormantRecoveryJobs(repositoryId);
			if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
			setDormantJobs(ownedDormantJobs(repositoryId, jobs));
			load();
		} catch (error) {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) toast("error", (error as Error).message);
		} finally {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) setDormantBusy("");
		}
	};
    const changeJobPageSize = (value: number) => {
        if (!JOB_PAGE_SIZE_OPTIONS.includes(value as (typeof JOB_PAGE_SIZE_OPTIONS)[number])) return;
        setJobPageSize(value as (typeof JOB_PAGE_SIZE_OPTIONS)[number]);
        setJobPage(0);
        writeStoredPreference(JOB_PAGE_SIZE_STORAGE_KEY, value);
    };

    const changeVaultPageSize = (value: number) => {
        if (!VAULT_PAGE_SIZE_OPTIONS.includes(value as (typeof VAULT_PAGE_SIZE_OPTIONS)[number])) return;
        setVaultPageSize(value as (typeof VAULT_PAGE_SIZE_OPTIONS)[number]);
        setVaultPage(0);
        writeStoredPreference(VAULT_PAGE_SIZE_STORAGE_KEY, value);
    };

    const changeJobSort = (value: JobSort) => {
        setJobSort(value);
        setJobPage(0);
        writeStoredPreference(JOB_SORT_STORAGE_KEY, value);
    };

    const changeVaultSort = (value: VaultSort) => {
        setVaultSort(value);
        setVaultPage(0);
        writeStoredPreference(VAULT_SORT_STORAGE_KEY, value);
    };

    const jobCountFor = (repoId: string) => (jobs ?? []).filter((job) => job.targets.some((target) => target.repositoryId === repoId)).length;
    const sortedJobs = sortJobs(jobs ?? [], jobSort);
    const sortedVaults = sortVaults((repos ?? []).map((repository) => {
		const statistics = vaultStats[repository.id];
		return {
			...repository,
			vaultSizeBytes: statistics ? statistics.vaultSizeBytes : repository.vaultSizeBytes,
			vaultSizeMeasuredAt: statistics ? statistics.vaultSizeMeasuredAt : repository.vaultSizeMeasuredAt,
		};
	}), vaultSort);
    const safeJobPage = Math.min(jobPage, Math.max(0, Math.ceil(sortedJobs.length / jobPageSize) - 1));
    const safeVaultPage = Math.min(vaultPage, Math.max(0, Math.ceil(sortedVaults.length / vaultPageSize) - 1));
    const pageJobs = sortedJobs.slice(safeJobPage * jobPageSize, (safeJobPage + 1) * jobPageSize);
    const pageVaults = sortedVaults.slice(safeVaultPage * vaultPageSize, (safeVaultPage + 1) * vaultPageSize);
	const pageVaultIDs = pageVaults.map((repository) => repository.id).join("\u0000");

	useEffect(() => {
		const prepare = (repositoryId: string) => {
			void prepareVaultSize(repositoryId).then((status) =>
				setVaultStats((current) => ({ ...current, [repositoryId]: status }))).catch(() => undefined);
		};
		const nodes = Array.from(document.querySelectorAll<HTMLElement>("[data-vault-id]"));
		if (typeof IntersectionObserver === "undefined") {
			for (const repositoryId of pageVaultIDs.split("\u0000").filter(Boolean)) prepare(repositoryId);
			return;
		}
		const observer = new IntersectionObserver((entries) => {
			for (const entry of entries) if (entry.isIntersecting) {
				const repositoryId = (entry.target as HTMLElement).dataset.vaultId;
				if (repositoryId) { prepare(repositoryId); observer.unobserve(entry.target); }
			}
		});
		for (const node of nodes) observer.observe(node);
		return () => observer.disconnect();
	}, [pageVaultIDs]);

	useEffect(() => {
		const active = Object.entries(vaultStats).filter(([, status]) => status.running || status.pending).map(([id]) => id);
		if (active.length === 0) return;
		const timer = window.setInterval(() => {
			for (const repositoryId of active) void getVaultSizeStatus(repositoryId).then((status) =>
				setVaultStats((current) => ({ ...current, [repositoryId]: status }))).catch(() => undefined);
		}, 2000);
		return () => window.clearInterval(timer);
	}, [vaultStats]);

	const refreshVaultSize = (repositoryId: string) => {
		void prepareVaultSize(repositoryId, true).then((status) =>
			setVaultStats((current) => ({ ...current, [repositoryId]: status })))
			.catch((error: Error) => toast("error", error.message));
	};
    const selectedRunJob = (jobs ?? []).find((job) => job.id === runJobID) ?? null;
	const selectedConnectProfile = connectPreview?.profiles?.find((profile) => profile.profile_uuid === connectProfileUUID);
	const currentConnectOwner = connectPreview?.profiles?.find((profile) => profile.profile_uuid === connectPreview.vault_owner_profile_uuid);
	const connectProfileSelectionVisible = connectPreview?.mode === "profile" && !connectPreview.existingVault && !usesRcloneNativeLogin(connectForm.connector) &&
		!(connectPreview.profiles ?? []).some((profile) => profile.localAttachment);
	const connectProfileReady = !connectPreview || Boolean(connectPreview.existingVault) || connectPreview.mode === "fallback" || connectProfileChoiceMade;
	const connectOwnerReady = !connectPreview || Boolean(connectPreview.existingVault) || connectPreview.mode === "fallback" || !connectProfileChoiceMade || connectOwnerChoiceMade;
	const selectedProfileIsOwner = Boolean(selectedConnectProfile?.vaultOwner);
	const connectMaintenanceLocked = connectPreview?.existingVault
		? !connectPreview.existingVault.isVaultOwner
		: connectPreview?.mode === "profile" && !selectedProfileIsOwner && !(connectOwnerChoiceMade && connectOwnerAction === "takeover");
	const connectIntegrityLocked = connectMaintenanceLocked;
	// Updating an existing registration deliberately preserves ownership and has
	// no takeover control; ordinary profile connection keeps its actionable help.
	const connectIntegrityLockedHelp = connectPreview?.existingVault
		? "Only the current vault owner can change or run integrity checks. Updating this existing vault does not take over ownership."
		: "Only the current vault owner can change or run integrity checks. Take over vault ownership above to enable it on this computer.";
	const connectMaintenanceLockedHelp = connectPreview?.existingVault
		? "Only the current vault owner can change or run space reclamation. Updating this existing vault does not take over ownership."
		: "Only the current vault owner can change or run space reclamation. Take over vault ownership above to enable it on this computer.";
	const existingVaultUpdateChanges = connectPreview?.existingVault ? [
		...[connectPreview.existingVault.location !== connectReviewedLocation
			? `Location: ${connectPreview.existingVault.location} → ${connectReviewedLocation}` : "Location: unchanged"],
		...(connectPreview.existingVault.name !== connectForm.name.trim() ? [`Name: ${connectPreview.existingVault.name} → ${connectForm.name.trim()}`] : []),
		...(connectPreview.existingVault.description !== connectForm.description.trim() ?
			[`Description: ${connectPreview.existingVault.description || "(empty)"} → ${connectForm.description.trim() || "(empty)"}`] : []),
		...(connectPreview.existingVault.checkSchedule !== connectForm.checkSchedule ? [`Integrity checks: ${connectPreview.existingVault.checkSchedule} → ${connectForm.checkSchedule}`] : []),
		...(connectPreview.existingVault.maintenanceSchedule !== connectForm.maintenanceSchedule ? [`Space reclamation: ${connectPreview.existingVault.maintenanceSchedule} → ${connectForm.maintenanceSchedule}`] : []),
		...(connectPreview.existingVault.concurrencyMode !== connectForm.concurrencyMode ? [`Vault performance: ${connectPreview.existingVault.concurrencyMode} → ${connectForm.concurrencyMode}`] : []),
		"Credentials: replace with the successfully validated connection",
	] : [];
	const connectUpdateReviewDigest = connectPreview?.existingVault ? JSON.stringify({
		vaultUUID: connectPreview.existingVault.id,
		previewDigest: connectPreview.digest,
		location: connectReviewedLocation,
		name: connectForm.name.trim(), description: connectForm.description.trim(),
		checkSchedule: connectForm.checkSchedule, maintenanceSchedule: connectForm.maintenanceSchedule,
		concurrencyMode: connectForm.concurrencyMode,
	}) : "";
	const connectReviewedIntegritySchedule = connectPreview?.rootIntegritySchedule ?? connectForm.checkSchedule;
	const connectReviewedMaintenanceSchedule = connectPreview?.rootMaintenanceSchedule ?? connectForm.maintenanceSchedule;
	const currentOwnerName = currentConnectOwner?.attachment.display.computerName && currentConnectOwner.attachment.display.operatingSystem
		? `${currentConnectOwner.attachment.display.computerName}@${currentConnectOwner.attachment.display.operatingSystem}`
		: connectPreview?.vault_owner_profile_uuid ? `Profile ${connectPreview.vault_owner_profile_uuid}` : "The current profile";
	// Pending intent recovery appears only after the user enters its exact
	// destination. Stale intents no longer occupy the whole reconnect screen;
	// the server still proves physical identity and the saved review on retry.
	const enteredConnectLocation = vaultLocation(connectForm);
	const matchingConnectionIntent = enteredConnectLocation ? connectionIntents.find((intent) =>
		connectionIntentMatchesDestination(intent, connectForm, enteredConnectLocation)) : undefined;
	// A cloud folder name alone cannot identify the native account. Keep the
	// ordinary authorization Check reachable beside any saved-account retry.
	const canCheckAnotherRcloneAccount = connectUsesRcloneLogin;
    return (
        <div className="page protect-page">
            <header className="page-header simple">
                <h1 className="page-title">Protect</h1>
                <p className="page-desc">Backup your data into encrypted vaults.</p>
            </header>

	            <section id="jobs">
	                <div className="section-heading">
	                    <h2>Backup jobs</h2>
	                    <div className="job-heading-actions">
						{(jobs?.length ?? 0) > 1 && selectedJobs.length > 0 && <span className="selection-count">{selectedJobs.length} selected</span>}
						<button className="btn primary" disabled={!repos?.length} onClick={() => openJob()}>
							<Icon name="plus" size={14} /> Backup job
						</button>
					</div>
	                </div>
	                <p className="section-copy">Create snapshots of your data.</p>
				{jobs !== null && jobs.length > 1 && selectedJobs.length > 0 && <div className="job-bulk-toolbar">
					<label className="check"><input type="checkbox" aria-label="Select all jobs" checked={selectedJobs.length === jobs.length} disabled={bulkSaving} onChange={() => selectedJobs.length === jobs.length ? setSelectedJobIDs([]) : selectAllJobs()} />Select all jobs</label>
					{selectedJobs.length > 1 && <div className="tool-buttons">
						<button className="btn sm" disabled={bulkSaving} onClick={openBulkEdit}>Edit</button>
						<button className="btn sm" disabled={bulkSaving} onClick={() => void applyBulkEnabled(true)}>Enable</button>
						<button className="btn sm" disabled={bulkSaving} onClick={() => void applyBulkEnabled(false)}>Disable</button>
						<button className="btn sm" disabled={bulkSaving} onClick={() => setSelectedJobIDs([])}>Clear selection</button>
					</div>}
				</div>}

                {jobs === null && <Loading />}
                {jobs !== null && jobs.length === 0 && (
                    <EmptyState icon="jobs" title={repos?.length ? "No backup jobs" : "Add a vault first"}>
                        <p>{repos?.length ? "Create a job to protect a source directory." : "Jobs need a vault for their backup data."}</p>
                    </EmptyState>
                )}

                <div className="job-flow-list">
                    {pageJobs.map((job) => {
                        const activeTargets = job.targets.filter(backupTargetIsActive);
                        const failedTargets = job.targets.filter((target) => target.lastStatus === "failed" || target.lastStatus === "interrupted" || target.lastStatus === "reconnect_required");
                        const issueTargets = job.targets.filter((target) => target.lastStatus === "completed_with_issues");
                        const isRunning = Boolean(running[job.id]) || activeTargets.length > 0;
                        const sourceUnavailable = job.targets.some((target) => target.sourceAvailability === "unavailable");
                        const vaultUnavailable = job.targets.some((target) => target.targetAvailability === "unavailable");
                        const successfulTargets = job.targets.filter((target) => target.lastStatus === "success");
                        const fullyProtected = job.targets.length > 0 && successfulTargets.length === job.targets.length;
                        const partiallyProtected = successfulTargets.length > 0 && !fullyProtected;
                        const tone = !job.enabled ? "idle" : isRunning ? "accent" : sourceUnavailable || vaultUnavailable || failedTargets.length > 0 ? "danger" : issueTargets.length > 0 ? "warn" : fullyProtected ? "ok" : "warn";
                        const status = !job.enabled ? "Paused" : isRunning ? `${activeTargets.length} queued/running` : sourceUnavailable ? "Source unavailable" : vaultUnavailable ? "Vault unavailable" : failedTargets.length > 0 ? `${failedTargets.length} failed` : issueTargets.length > 0 ? "Completed with issues" : fullyProtected ? "Protected" : partiallyProtected ? "Partially protected" : "Not run yet";
                        const destinationStatus = activeTargets.length > 0
                            ? "running"
                            : failedTargets.length > 0
                                ? "failure"
                                : issueTargets.length > 0
                                    ? "protected · completed with issues"
                                    : fullyProtected
                                        ? "successful"
                                        : partiallyProtected
                                            ? `${successfulTargets.length} of ${job.targets.length} successful`
                                            : "not run";
                        const targetDetails = job.targets.map((target) =>
                            `${target.repositoryName}: ${target.lastStatus === "completed_with_issues" ? "completed with issues" : target.lastStatus === "reconnect_required" ? "reconnect required" : target.lastStatus || "not run"}`
                        ).join(" · ");
                        const pendingCatchUpTargets = job.targets.filter((target) => target.pendingCatchUp);
                        return (
	                            <article key={job.id} className={`job-flow ${tone}${selectedJobIDs.includes(job.id) ? " selected" : ""}`}>
								<div className="job-leading-controls">
									{(jobs?.length ?? 0) > 1 && <input type="checkbox" aria-label={`Select ${job.name}`} checked={selectedJobIDs.includes(job.id)} disabled={bulkSaving} onChange={() => toggleJobSelection(job.id)} />}
									<button className={`toggle${job.enabled ? " on" : ""}`} disabled={Boolean(jobToggleBusy) || bulkSaving} onClick={() => void toggle(job)} aria-label={`${job.enabled ? "Disable" : "Enable"} ${job.name}`}><span /></button>
								</div>
                                <div className="flow-source">
                                    <strong>{job.name}</strong>
									<Tooltip content={displayPath(job.source)}>
										<span className="flow-source-path"><span className="flow-source-path-text"><b>Source</b> {displayPath(job.source)}</span></span>
									</Tooltip>
									{job.resolvedSourcePath && <span className="flow-source-path"><span className="flow-source-path-text"><b>Last verified at</b> {displayPath(job.resolvedSourcePath)}</span></span>}
									<span><b>Size</b> {job.sizeBytes == null ? "Not measured yet" : readableSize(job.sizeBytes)}</span>
                                </div>
                                <div className="flow-glyph">
                                    <span className="flow-status">{status === "Partially protected" ? <>Partially <br />protected</> : status}</span>
                                    <svg className="flow-arrow-horizontal" viewBox="0 0 120 10" aria-hidden="true">
                                        <line x1="0" y1="5" x2="112" y2="5" />
                                        <path d="M112 1.5L119 5l-7 3.5z" />
                                    </svg>
                                    <svg className="flow-arrow-vertical" viewBox="0 0 10 36" aria-hidden="true">
                                        <line x1="5" y1="0" x2="5" y2="28" />
                                        <path d="M1.5 28L5 35l3.5-7z" />
                                    </svg>
									<Tooltip content={nextSnapshotTooltip(job)}><span>{scheduleLabel(job.schedule).toLowerCase()}</span></Tooltip>
                                </div>
                                <div className="flow-vault">
                                    <strong>{job.targets.map((target) => target.repositoryName).join(", ")}</strong>
                                    <Tooltip content={targetDetails}>
                                        <span className="flow-vault-status">
                                            <b>Vault{job.targets.length === 1 ? "" : "s"}</b> {job.targets.length} vault{job.targets.length === 1 ? "" : "s"} · {destinationStatus}
                                        </span>
                                    </Tooltip>
                                    {pendingCatchUpTargets.length > 0 && <span className="recovery-warning"><b>Scheduled catch-up pending</b> for {pendingCatchUpTargets.map((target) => `${target.repositoryName} (${target.coalescedMissedCount} coalesced miss${target.coalescedMissedCount === 1 ? "" : "es"})`).join(", ")}</span>}
                                </div>
                                <div className="row-actions">
                                    <Tooltip content="Run job">
                                        <button className="btn ghost-icon job-action-run" aria-label={`Run job ${job.name}`} disabled={Boolean(runActiveJobID) || job.targets.every(backupTargetIsActive)} onClick={() => job.targets.length > 1 ? openRunReview(job.id) : void startRun(job)}>{(isRunning || runActiveJobID === job.id) ? <span className="spinner" /> : <Icon name="play" size={14} />}</button>
                                    </Tooltip>
									<Tooltip content="Copy job">
                                        <button className="btn ghost-icon" aria-label={`Copy job ${job.name}`} onClick={() => copyJob(job)}><Icon name="copy" size={15} /></button>
                                    </Tooltip>
                                    <Tooltip content="Job settings">
                                        <button className="btn ghost-icon" disabled={isRunning} aria-label={`Edit ${job.name}`} onClick={() => openJob(job)}><Icon name="edit" size={14} /></button>
                                    </Tooltip>
                                    <Tooltip content="Delete job">
                                        <button className="btn ghost-icon danger-hover" aria-label={`Delete ${job.name}`} onClick={() => openJobDelete(job)}><Icon name="trash" size={14} /></button>
                                    </Tooltip>
                                </div>
                            </article>
                        );
                    })}
                </div>
                {jobs !== null && jobs.length > 0 && (
                    <ListControls
                        label="jobs"
                        total={jobs.length}
                        defaultPageSize={JOB_PAGE_SIZE}
                        page={safeJobPage}
                        pageSize={jobPageSize}
                        pageSizeOptions={JOB_PAGE_SIZE_OPTIONS}
                        sort={jobSort}
                        sortOptions={JOB_SORT_OPTIONS}
                        onPage={setJobPage}
                        onPageSize={changeJobPageSize}
                        onSort={changeJobSort}
                    />
                )}
            </section>

            <section className="vault-section" id="vaults">
                <div className="section-heading">
                    <h2>Vaults</h2>
                    <button className="btn" onClick={() => setShowVaultChoice(true)}><Icon name="plus" size={14} /> Vault</button>
                </div>
                <p className="section-copy">Where snapshots live.</p>

                {repos === null && <Loading />}
                {repos !== null && repos.length === 0 && creationIntents.length === 0 && (
                    <EmptyState icon="vault" title="No vaults yet"><p>Add a local or remote vault to get started.</p></EmptyState>
                )}

                <div className="vault-list">
					{creationIntents.map((intent) => (
						<article key={intent.id} className="vault-row failed-vault-card">
							<div className="vault-identity">
								<div className="vault-card-head">
									<span className="vault-glyph"><Icon name="shield" size={18} /></span>
									<span className="vault-chips">
										<span className="connector-chip">Creation pending</span>
										<span className="connector-chip">{intent.engine}</span>
									</span>
								</div>
								<strong>{intent.name}</strong>
								<Tooltip content={intent.location}>
									<span className="vault-location"><span className="vault-location-text">{intent.location}</span></span>
								</Tooltip>
							</div>
							<div className="vault-stat"><strong>Not measured yet</strong><span>Vault Size</span></div>
							<div className="row-actions vault-actions failed-vault-actions">
									<button className="btn sm" onClick={() => retryCreation(intent)}>Retry</button>
								<button className="btn sm" onClick={() => setCreationErrorView(intent)}>View error</button>
								<button
									className="btn sm danger-outline"
									disabled={Boolean(creationRemovalBusy)}
										onClick={() => void removePendingCreation(intent)}
								>
									{creationRemovalBusy === intent.id && <span className="spinner" />}
										{intent.phase === "prepared" ? "Cancel" : "Forget"}
								</button>
							</div>
						</article>
					))}
                    {pageVaults.map((repo) => {
						const removal = vaultRemoval[repo.id];
						const stats = vaultStats[repo.id] ?? { vaultSizeBytes: repo.vaultSizeBytes, vaultSizeMeasuredAt: repo.vaultSizeMeasuredAt, vaultSizeDirty: repo.vaultSizeDirty, fresh: false, running: false, pending: false, paused: false };
						const sizePresentation = stats.vaultSizeBytes == null ? null : vaultSizePresentation(stats.vaultSizeBytes);
						const pendingProfile = profileSync[repo.id];
						const statsActive = stats.running || stats.pending;
						const statsStatus = stats.paused ? "Refresh paused due to active jobs…" : "Refreshing stats…";
						const reconnectRequired = Boolean(reconnectRequiredVaults[repo.id]);
                        return (
							<article key={repo.id} className="vault-row" data-vault-id={repo.id}>
									{/* Covered controls must leave keyboard navigation while removal owns this card. */}
									<div className="vault-identity" inert={Boolean(removal)}>
										<div className="vault-card-head">
										  <span className="vault-glyph"><Icon name="shield" size={18} /></span>
									  <span className="vault-chips">
										<span className="connector-chip">{repo.engine}</span>
										<span className="connector-chip">{repositoryConnectorLabel(repo)}</span>
									  </span>
										</div>
										<strong>{repo.name}</strong>
										<Tooltip content={repo.location}>
											<span className="vault-location"><span className="vault-location-text">{repositoryLocationLabel(repo)}</span></span>
										</Tooltip>
									{pendingProfile?.lastError && (
									<span className="recovery-warning profile-sync-warning">Vault profile update pending: {pendingProfile.lastError} {pendingProfile.nextAttemptAt ? `· retry ${timeAgo(pendingProfile.nextAttemptAt)}` : ""} <button className="btn sm" disabled={Boolean(removal)} onClick={() => void retryProfile(repo.id)}>Retry now</button></span>
									)}
									{reconnectRequired && <span className="recovery-warning">Reconnect is required before this vault can run backups.</span>}
                                </div>
								<div className={`vault-stat vault-size-stat${statsActive ? " is-refreshing" : ""}`} inert={Boolean(removal)}>
									<div className="vault-size-summary">{sizePresentation ? <Tooltip content={sizePresentation.tooltip}><span className="vault-size-value" aria-label={sizePresentation.display}><strong>{sizePresentation.display}</strong></span></Tooltip> : <strong>Not measured yet</strong>}<span>Vault Size</span></div>
									<div className="vault-size-meta"><small>Size Stats Updated: {stats.vaultSizeMeasuredAt ? timeAgo(stats.vaultSizeMeasuredAt) : "Not measured yet"}.</small><Tooltip content="Replicaro periodically refreshes vault size stats. You can force a refresh immediately. Your backed up files are not changed. Forced refreshes read from the vault's destination and may take time to complete."><button className="vault-stats-refresh" aria-label="Refresh stats now" disabled={Boolean(removal)} onClick={() => refreshVaultSize(repo.id)}>Refresh stats now</button></Tooltip></div>
									{statsActive && <span className="vault-stats-refreshing">{!stats.paused && <span className="spinner" />}<span><strong>{statsStatus}</strong><span>You can safely close this page if you need to. Refreshing will resume in the background.</span></span></span>}
									{stats.failure && <small className="recovery-warning">{stats.failure}</small>}
								</div>
								<div className="row-actions vault-actions" inert={Boolean(removal)}>
									{reconnectRequired && <button className="btn sm danger" disabled={Boolean(removal)} onClick={() => void openSavedVaultReconnect(repo)}>Reconnect</button>}
									<Link className="btn sm" to={`/restore/${repo.id}`} tabIndex={removal ? -1 : undefined} aria-disabled={Boolean(removal)} onClick={(event) => { if (removal) event.preventDefault(); }}>Browse</Link>
                                    <Tooltip content="Vault settings">
										<button className="btn ghost-icon" aria-label={`Edit ${repo.name}`} disabled={Boolean(removal)} onClick={() => openVaultSettings(repo)}><Icon name="edit" size={14} /></button>
                                    </Tooltip>
                                    <Tooltip content="Delete vault">
										<button className="btn ghost-icon danger-hover" aria-label={`Delete ${repo.name}`} disabled={Boolean(removal)} onClick={() => openVaultDelete(repo)}><Icon name="trash" size={14} /></button>
                                    </Tooltip>
                                </div>
								{removal && <div className="vault-removal-overlay" role="status" aria-live="polite">
									{removal.phase === "removing" ? <><span className="spinner" aria-hidden="true" /><strong>Vault is being removed...</strong><span>You can continue using Replicaro while this finishes.</span></> : <>
										<strong>{removal.phase === "profile_error" ? "The recovery profile could not be updated." : "The vault could not be removed."}</strong>
										<span>{removal.message}</span>
										{removal.phase === "profile_error" && <span>Removing it anyway leaves the remote recovery profile unchanged. The vault's backed up data will remain untouched.</span>}
										<div className="vault-removal-actions"><button className="btn sm" onClick={() => setVaultRemoval((current) => { const next = { ...current }; delete next[repo.id]; return next; })}>Keep vault</button><button className="btn sm danger-outline" onClick={() => void removeVault(repo, removal.phase === "profile_error")}>{removal.phase === "profile_error" ? "Remove anyway" : "Retry removal"}</button></div>
									</>}
								</div>}
                            </article>
                        );
                    })}
                </div>
                {repos !== null && repos.length > 0 && (
                    <ListControls
                        label="vaults"
                        total={repos.length}
                        defaultPageSize={VAULT_PAGE_SIZE}
                        page={safeVaultPage}
                        pageSize={vaultPageSize}
                        pageSizeOptions={VAULT_PAGE_SIZE_OPTIONS}
                        sort={vaultSort}
                        sortOptions={VAULT_SORT_OPTIONS}
                        onPage={setVaultPage}
                        onPageSize={changeVaultPageSize}
                        onSort={changeVaultSort}
                    />
                )}
            </section>

	            {jobModal && (
	                <Modal title={jobModal === "new" ? "New backup job" : "Edit job"} wide onClose={closeJob}>
					<fieldset className="modal-workflow-fields" disabled={jobSaving}>
	                    <div className="modal-form">
							{jobModal === "new" && !jobCopyDraft ? <>
								<p className="muted modal-intro">You can add one or more source data to back up. Each source data will get its own job, and each job will get the same job settings you set here.</p>
								<label className="field"><span>Name</span><input autoFocus value={jobForm.name} placeholder="example: Documents nightly" onChange={(event) => setJobForm({ ...jobForm, name: event.target.value })} /></label>
								<label className="field"><span>Source data</span><DirectoryField ariaLabel="Source data" value={jobForm.source} onChange={(source) => setJobForm({ ...jobForm, source })} /><small>Must be a local folder, mounted share, mapped drive, or network path. External, portable, and flash drives are supported. Source cannot be changed after job is created.</small></label>
								{jobAdditionalSources.map((source, index) => <div className="field job-additional-source" key={index}>
									<span>Source data</span>
									<div className="job-additional-source-controls">
										<DirectoryField ariaLabel={`Source data ${index + 2}`} value={source} onChange={(nextSource) => setJobAdditionalSources((current) => current.map((item, itemIndex) => itemIndex === index ? nextSource : item))} />
										<button type="button" className="btn sm danger-outline remove-source" aria-label={`Remove source ${index + 2}`} onClick={() => setJobAdditionalSources((current) => current.filter((_, itemIndex) => itemIndex !== index))}>Remove source</button>
									</div>
									<small>Must be a local folder, mounted share, mapped drive, or network path. External, portable, and flash drives are supported. Source cannot be changed after job is created.</small>
								</div>)}
								<button type="button" className="btn" onClick={() => setJobAdditionalSources((current) => [...current, ""])}><Icon name="plus" size={14} /> Add source</button>
							</> : <>
								<label className="field"><span>Name</span><input autoFocus={!jobCopyDraft} value={jobForm.name} placeholder="example: Documents nightly" onChange={(event) => setJobForm({ ...jobForm, name: event.target.value })} /></label>
								{/* Keep existing sources read-only, including unbound imports. The backend
								    permits source updates before local binding, but exposing that here would
								    let users point the same job ID at different data and alter native retention
								    scope. Backend mutability does not imply an editable source field. */}
								<label className="field"><span>Source data</span><DirectoryField ariaLabel="Source data" value={jobForm.source} readOnly={jobModal !== "new"} autoFocus={jobCopyDraft} onChange={(source) => setJobForm({ ...jobForm, source })} /><small>Must be a local folder, mounted share, mapped drive, or network path. External, portable, and flash drives are supported. Source cannot be changed after job is created.</small></label>
							</>}
                        <div className="field"><span>Destination vaults</span><DestinationVaultPicker repositories={repos ?? []} selectedIds={jobForm.repositoryIds} onChange={(repositoryIds) => setJobForm({ ...jobForm, repositoryIds })} /><small>Select one or more vaults. Backups for all vaults are managed by this job, but the backup for each vault runs and reports independently.</small></div>
                        <label className="field"><span>Schedule</span><select aria-label="Schedule" value={jobForm.schedule} onChange={(event) => setJobForm({ ...jobForm, schedule: event.target.value })}>{schedulePresets.map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select><small>{scheduleHelp(jobForm.schedule)}</small></label>
                        {customScheduleUnits[jobForm.schedule] && <label className="field compact-field"><span>{customScheduleUnits[jobForm.schedule]!.label}</span><input type="number" min={1} max={customScheduleUnits[jobForm.schedule]!.max} value={jobForm.customInterval} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, customInterval: event.target.value })} /></label>}
						{jobForm.schedule === "cron" && <label className="field"><span>Cron expression</span><input className="mono" value={jobForm.cronExpression} placeholder="0 2 * * *" maxLength={MAX_CRON_EXPRESSION_LENGTH} onChange={(event) => setJobForm({ ...jobForm, cronExpression: event.target.value })} /><small>{cronScheduleHelp}</small></label>}
						<label className="field"><span>Retention policy</span><select aria-label="Retention policy" value={retentionPreset(jobForm.retention)} onChange={(event) => setJobForm({ ...jobForm, retention: event.target.value === "custom" ? (retentionPreset(jobForm.retention) === "custom" ? jobForm.retention : "") : event.target.value })}>{retentionPresets.map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select>
							{/* Deliberate simplification: 100 snapshots do not guarantee 100 distinct versions for every file. Do not change this help text. */}
							<small>How many versions of your files do you want to keep in the backup? 100 snapshots = 100 file versions.</small></label>
						{retentionPreset(jobForm.retention) === "custom" && <label className="field compact-field"><span>Number of latest snapshots to keep</span><input aria-label="Number of latest snapshots to keep" type="number" min={1} max={MAX_MAIN_RETENTION_COUNT} value={jobForm.retention} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retention: event.target.value })} /></label>}
                        <button className="btn advanced-toggle" onClick={() => setJobAdvanced((value) => !value)}><Icon name="settings" size={14} />{jobAdvanced ? "Hide advanced settings" : "Advanced settings"}</button>
                        {jobAdvanced && (
                            <div className="advanced-panel">
								<div className="engine-settings">
									<div className="engine-settings-heading"><strong>More retention options (optional)</strong><small>Advanced retention policies are evaluated together with the main retention policy. A snapshot is deleted when it falls out of range of all the enabled retention rules.<br /><br />For example, if the main retention policy keeps the latest 100 snapshots and the daily policy keeps 10 daily snapshots, Replicaro keeps the latest 100 plus any older snapshots that qualify as being one of the 10 latest daily snapshots.<br /><br />Leave blank any retention tier that you do not want to use.</small></div>
									<label className="field"><span>Keep N latest hourly snapshots</span><input aria-label="Keep N latest hourly snapshots" type="number" min={1} max={MAX_RETENTION_COUNT} value={jobForm.retentionHourly} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retentionHourly: event.target.value })} /></label>
									<label className="field"><span>Keep N latest daily snapshots</span><input aria-label="Keep N latest daily snapshots" type="number" min={1} max={MAX_RETENTION_COUNT} value={jobForm.retentionDaily} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retentionDaily: event.target.value })} /></label>
									<label className="field"><span>Keep N latest weekly snapshots</span><input aria-label="Keep N latest weekly snapshots" type="number" min={1} max={MAX_RETENTION_COUNT} value={jobForm.retentionWeekly} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retentionWeekly: event.target.value })} /></label>
									<label className="field"><span>Keep N latest monthly snapshots</span><input aria-label="Keep N latest monthly snapshots" type="number" min={1} max={MAX_RETENTION_COUNT} value={jobForm.retentionMonthly} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retentionMonthly: event.target.value })} /></label>
									<label className="field"><span>Keep N latest yearly snapshots</span><input aria-label="Keep N latest yearly snapshots" type="number" min={1} max={MAX_RETENTION_COUNT} value={jobForm.retentionYearly} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retentionYearly: event.target.value })} /></label>
								</div>
	                                <label className="field"><span>Exclude patterns (optional)</span><textarea className="mono" rows={3} value={jobForm.excludes} placeholder={"example: *.tmp\nexample: node_modules"} onChange={(event) => setJobForm({ ...jobForm, excludes: event.target.value })} /><small>One pattern per line for files/folders this job should skip.</small></label>
								<label className="field"><span>Tag (optional)</span><input value={jobForm.tag} onChange={(event) => setJobForm({ ...jobForm, tag: event.target.value })} /><small>Optional label applied to snapshots created by this job.</small></label>
                                <div className="engine-settings">
									<div className="engine-settings-heading"><strong>Engine-specific settings (optional)</strong><small>These settings apply only to the selected vault engines.</small></div>
                                    {selectedEngines.length === 0 && <p className="engine-settings-empty">Select a destination vault to configure its engine.</p>}
                                    {selectedEngines.map((engine) => (
                                        <section className="engine-settings-group" key={engine}>
                                            <h3>{engine[0].toUpperCase() + engine.slice(1)}</h3>
											<label className="field"><span>Advanced CLI options</span><textarea className="mono" rows={3} value={jobForm.engineOptions[engine] ?? ""} placeholder={engine === "kopia" ? "example: --fail-fast" : "example: --verbose"} onChange={(event) => setJobForm({ ...jobForm, engineOptions: { ...jobForm.engineOptions, [engine]: event.target.value } })} /><small>One option per line. Each line is tokenized as CLI options, so a flag and its value may share a line. Replicaro rejects positional arguments, shell syntax, secrets, and protected repository settings.</small></label>
                                        </section>
                                    ))}
								</div>
								<JobScriptFields value={jobForm} onChange={(scripts) => setJobForm({ ...jobForm, ...scripts })} />
                            </div>
                        )}
						{jobModal === "new" && <label className="check"><input type="checkbox" checked={jobForm.enabled} onChange={(event) => setJobForm({ ...jobForm, enabled: event.target.checked })} />Enabled — the scheduler will run this job</label>}
                    </div>
						<div className="modal-footer"><button className="btn" disabled={jobSaving} onClick={closeJob}>Cancel</button><button className="btn primary" disabled={jobSaving} onClick={() => void saveJob()}>{jobSaving && <span className="spinner" />}{jobModal === "new" ? "Create job" : "Save changes"}</button></div>
					</fieldset>
	                </Modal>
	            )}

			{jobCreationResults && <Modal title="Backup job creation results" onClose={dismissJobCreationResults}>
				<p className="muted modal-intro">Each source got its own job. These are independent jobs that you can edit individually or together using bulk edit. Any failed job can be retried here or created again manually.</p>
				<div className="mutation-results">{jobCreationResults.map((item, index) => <div className={`mutation-result ${item.status}`} key={`${item.name}:${index}`}>
					<strong>{item.name}</strong>
					<span>{item.status[0].toUpperCase() + item.status.slice(1)}</span>
					{item.source && <small className="mono mutation-result-source">Source: {item.source}</small>}
					<small>{item.message}</small>
				</div>)}</div>
				<div className="modal-footer">
					{jobCreationResults.some((item) => item.status === "failed") && <button className="btn" disabled={jobCreationRetrying} onClick={() => void retryFailedJobCreations()}>{jobCreationRetrying && <span className="spinner" />}Retry failed jobs</button>}
					<button className="btn primary" disabled={jobCreationRetrying} onClick={dismissJobCreationResults}>Close</button>
				</div>
			</Modal>}

			{bulkEditOpen && <Modal title={bulkEditStep === "edit" ? "Bulk edit jobs" : "Review bulk changes"} wide onClose={dismissBulkEdit}>
				{bulkEditStep === "edit" ? <fieldset className="modal-workflow-fields" disabled={bulkSaving}>
					<p className="muted modal-intro">{selectedJobs.length} selected job{selectedJobs.length === 1 ? "" : "s"}. Names and source directories will remain unchanged.</p>
					<div className="modal-form bulk-edit-form">
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.changeSchedule} onChange={(event) => setBulkForm({ ...bulkForm, changeSchedule: event.target.checked })} />Change schedule</label>
							{bulkForm.changeSchedule && <div className="bulk-setting-fields"><label className="field"><span>Schedule</span><select aria-label="Schedule" value={bulkForm.schedule} onChange={(event) => setBulkForm({ ...bulkForm, schedule: event.target.value })}>{schedulePresets.map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select><small>{scheduleHelp(bulkForm.schedule)}</small></label>{customScheduleUnits[bulkForm.schedule] && <label className="field compact-field"><span>{customScheduleUnits[bulkForm.schedule]!.label}</span><input type="number" min={1} max={customScheduleUnits[bulkForm.schedule]!.max} value={bulkForm.customInterval} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, customInterval: event.target.value })} /></label>}{bulkForm.schedule === "cron" && <label className="field"><span>Cron expression</span><input className="mono" value={bulkForm.cronExpression} placeholder="0 2 * * *" maxLength={MAX_CRON_EXPRESSION_LENGTH} onChange={(event) => setBulkForm({ ...bulkForm, cronExpression: event.target.value })} /><small>{cronScheduleHelp}</small></label>}</div>}
						</section>
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.changeRetention} onChange={(event) => setBulkForm({ ...bulkForm, changeRetention: event.target.checked })} />Change retention</label>
							{bulkForm.changeRetention && <div className="bulk-setting-fields"><label className="field"><span>Retention policy</span><select aria-label="Bulk retention policy" value={retentionPreset(bulkForm.retention)} onChange={(event) => setBulkForm({ ...bulkForm, retention: event.target.value === "custom" ? (retentionPreset(bulkForm.retention) === "custom" ? bulkForm.retention : "") : event.target.value })}>{retentionPresets.map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select></label>{retentionPreset(bulkForm.retention) === "custom" && <label className="field compact-field"><span>Number of latest snapshots to keep</span><input type="number" min={1} max={MAX_MAIN_RETENTION_COUNT} value={bulkForm.retention} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retention: event.target.value })} /></label>}<div className="form-grid two"><label className="field"><span>Keep N latest hourly snapshots</span><input type="number" min={1} max={MAX_RETENTION_COUNT} value={bulkForm.retentionHourly} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retentionHourly: event.target.value })} /></label><label className="field"><span>Keep N latest daily snapshots</span><input type="number" min={1} max={MAX_RETENTION_COUNT} value={bulkForm.retentionDaily} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retentionDaily: event.target.value })} /></label><label className="field"><span>Keep N latest weekly snapshots</span><input type="number" min={1} max={MAX_RETENTION_COUNT} value={bulkForm.retentionWeekly} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retentionWeekly: event.target.value })} /></label><label className="field"><span>Keep N latest monthly snapshots</span><input type="number" min={1} max={MAX_RETENTION_COUNT} value={bulkForm.retentionMonthly} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retentionMonthly: event.target.value })} /></label><label className="field"><span>Keep N latest yearly snapshots</span><input type="number" min={1} max={MAX_RETENTION_COUNT} value={bulkForm.retentionYearly} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retentionYearly: event.target.value })} /></label></div></div>}
						</section>
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.changeExcludes} onChange={(event) => setBulkForm({ ...bulkForm, changeExcludes: event.target.checked })} />Change exclude patterns</label>
							{bulkForm.changeExcludes && <div className="bulk-setting-fields"><label className="field"><span>Exclude patterns (optional)</span><textarea className="mono" rows={3} value={bulkForm.excludes} placeholder={"example: *.tmp\nexample: node_modules"} onChange={(event) => setBulkForm({ ...bulkForm, excludes: event.target.value })} /><small>One pattern per line for files/folders these jobs should skip.</small></label></div>}
						</section>
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.changeTag} onChange={(event) => setBulkForm({ ...bulkForm, changeTag: event.target.checked })} />Change tag</label>
							{bulkForm.changeTag && <div className="bulk-setting-fields"><label className="field"><span>Tag (optional)</span><input value={bulkForm.tag} onChange={(event) => setBulkForm({ ...bulkForm, tag: event.target.value })} /><small>Optional label applied to snapshots created by these jobs.</small></label></div>}
						</section>
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.changeScripts} onChange={(event) => setBulkForm({ ...bulkForm, changeScripts: event.target.checked })} />Change scripts</label>
							{bulkForm.changeScripts && <div className="bulk-setting-fields"><JobScriptFields value={bulkForm} onChange={(scripts) => setBulkForm({ ...bulkForm, ...scripts })} /></div>}
						</section>
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.replaceDestinations} onChange={(event) => setBulkForm({ ...bulkForm, replaceDestinations: event.target.checked, repositoryIds: event.target.checked ? bulkForm.repositoryIds : [], engineOptions: event.target.checked ? bulkForm.engineOptions : {} })} />Replace destination vaults</label>
							{bulkForm.replaceDestinations && <div className="bulk-setting-fields"><div className="field"><span>Destination vaults</span><DestinationVaultPicker repositories={repos ?? []} selectedIds={bulkForm.repositoryIds} onChange={(repositoryIds) => setBulkForm({ ...bulkForm, repositoryIds })} /><small>Selected vaults will replace the complete destination list for every selected job.</small></div><div className="engine-settings"><div className="engine-settings-heading"><strong>Engine-specific settings (optional)</strong><small>These options replace the active engine options for every selected job. Leave blank to use no advanced options.</small></div>{bulkSelectedEngines.map((engine) => <section className="engine-settings-group" key={engine}><h3>{engine[0].toUpperCase() + engine.slice(1)}</h3><label className="field"><span>Advanced CLI options</span><textarea className="mono" rows={3} value={bulkForm.engineOptions[engine] ?? ""} placeholder={engine === "kopia" ? "example: --fail-fast" : "example: --verbose"} onChange={(event) => setBulkForm({ ...bulkForm, engineOptions: { ...bulkForm.engineOptions, [engine]: event.target.value } })} /><small>One option per line. Each line is tokenized as CLI options, so a flag and its value may share a line. Replicaro rejects positional arguments, shell syntax, secrets, and protected repository settings.</small></label></section>)}</div></div>}
						</section>
					</div>
					<div className="modal-footer"><button className="btn" onClick={dismissBulkEdit}>Cancel</button><button className="btn primary" onClick={reviewBulkEdit}>Review changes</button></div>
				</fieldset> : <fieldset className="modal-workflow-fields" disabled={bulkSaving}>
					<p className="muted modal-intro">Apply these changes to the {selectedJobs.length} selected job{selectedJobs.length === 1 ? "" : "s"}.</p>
					<div className="bulk-review">
						<div><strong>Jobs</strong><span>{selectedJobs.map((job) => job.name).join(", ")}</span></div>
						<div><strong>Settings</strong><span>{[
							bulkForm.changeSchedule && "Schedule",
							bulkForm.changeRetention && "Retention",
							bulkForm.changeExcludes && "Exclude patterns",
							bulkForm.changeTag && "Tag",
							bulkForm.changeScripts && "Scripts",
							bulkForm.replaceDestinations && "Destination vaults and active engine options",
						].filter(Boolean).join(", ")}</span></div>
						{bulkForm.changeSchedule && <div><strong>Schedule</strong><span>{scheduleLabel(scheduleValue(bulkForm) ?? "")}</span></div>}
						{bulkForm.changeRetention && <div><strong>Retention</strong><span>{retentionReviewSummary(bulkForm)}</span></div>}
						{bulkForm.changeExcludes && <div><strong>Exclude patterns</strong><span className="mono review-verbatim">{bulkForm.excludes || "None"}</span></div>}
						{bulkForm.changeTag && <div><strong>Tag</strong><span>{bulkForm.tag.trim() || "None"}</span></div>}
						{bulkForm.changeScripts && <div><strong>Scripts</strong><span>Before: {bulkForm.beforeScriptPath || "None"}{bulkForm.beforeScriptMustSucceed ? " (must succeed)" : ""}; after: {bulkForm.afterScriptPath || "None"}{bulkForm.afterScriptMustSucceed ? " (must succeed)" : ""}.</span></div>}
						{bulkForm.replaceDestinations && <div><strong>Destination vaults</strong><span>{bulkForm.repositoryIds.map((id) => repos?.find((repo) => repo.id === id)?.name ?? id).join(", ")} · {selectedJobs.filter((job) => sameStringSet(job.targets.map((target) => target.repositoryId), bulkForm.repositoryIds)).length} already identical</span></div>}
						{bulkForm.replaceDestinations && <div><strong>Advanced engine options</strong><span className="mono review-verbatim">{bulkSelectedEngines.map((engine) => `${engine}:\n${parsedEngineOptions(bulkForm.engineOptions[engine] ?? "").join("\n") || "None"}`).join("\n\n")}</span></div>}
						{bulkForm.changeScripts && <div><strong>Script executions</strong><span>Scripts remain configured to run once per independent job/vault target ({selectedJobs.reduce((count, job) => count + (bulkForm.replaceDestinations ? bulkForm.repositoryIds.length : job.targets.length), 0)} targets after this change).</span></div>}
					</div>
					<div className="modal-footer"><button className="btn" disabled={bulkSaving} onClick={() => setBulkEditStep("edit")}>Back</button><button className="btn primary" disabled={bulkSaving} onClick={() => void applyBulkEdit()}>{bulkSaving && <span className="spinner" />}Apply changes</button></div>
				</fieldset>}
			</Modal>}

			{bulkResults && <Modal title="Bulk edit results" onClose={dismissBulkResults}>
				<p className="muted modal-intro">Every selected job is listed separately.</p>
				<div className="mutation-results">{bulkResults.items.map((item) => <div className={`mutation-result ${item.status}`} key={item.jobId ?? item.name}>
					<strong>{item.name}</strong>
					<span>{item.status[0].toUpperCase() + item.status.slice(1)}</span>
					<small>{item.message}</small>
				</div>)}</div>
				<div className="modal-footer">
					{bulkResults.items.some((item) => item.status === "failed") && <button className="btn" disabled={bulkRetrying} onClick={() => void retryFailedBulkJobs()}>{bulkRetrying && <span className="spinner" />}Retry failed jobs</button>}
					<button className="btn primary" disabled={bulkRetrying} onClick={dismissBulkResults}>Close</button>
				</div>
			</Modal>}

				{jobDelete && <ConfirmDialog title={`Delete "${jobDelete.name}"?`} message="Its run history is kept, but no further backups will run. Existing backup data is untouched." confirmLabel="Delete job" busy={jobDeleting} onConfirm={() => void removeJob()} onCancel={dismissJobDelete} />}
            {selectedRunJob && (
                <Modal title={`Run "${selectedRunJob.name}"`} onClose={dismissRunReview}>
					{runResults ? <article aria-label="Manual backup admission results">
						<p className="muted modal-intro">Every requested destination is listed separately.</p>
						<div className="unowned-snapshots">{runResults.map((result) => {
							const target = selectedRunJob.targets.find((candidate) => candidate.repositoryId === result.repositoryId);
							return <div className="unowned-snapshot" key={result.repositoryId}>
								<strong>{target?.repositoryName || result.repositoryId}</strong>
								<small>{manualRunResultText(result)}</small>
							</div>;
						})}</div>
					</article> : <>
						<p className="muted modal-intro">Choose which destination vault to back up to.</p>
						<BackupRunPicker job={selectedRunJob} busy={runBusy} onRun={(repositoryId) => void startRun(selectedRunJob, repositoryId)} />
					</>}
                </Modal>
            )}

            {showVaultChoice && (
				<Modal title="Add a vault" onClose={() => setShowVaultChoice(false)}>
					<div className="vault-choice-grid">
						<button className="vault-choice" onClick={() => openVaultCreate()}><Icon name="plus" size={22} /><span><strong>Create new vault</strong><small>Create a new encrypted backup destination.</small></span></button>
						<button className="vault-choice" onClick={() => { resetConnectWorkflow(); setShowVaultChoice(false); setShowVaultConnect(true); refreshConnectionIntents(); }}><Icon name="restore" size={22} /><span><strong>Connect existing vault</strong><small>Reconnect an existing Replicaro vault or import an existing Restic or Kopia vault.<br /><br />Imported Restic/Kopia vaults are converted to Replicaro vaults without deleting or modifying your backed up data.</small></span></button>
					</div>
				</Modal>
			)}

			{showVaultConnect && !connectRcloneAuthorizationAction && (
				<Modal title="Connect existing vault" wide onClose={() => { if (!connectSaving) { resetConnectWorkflow(); setShowVaultConnect(false); } }}>
					<fieldset className="modal-form modal-workflow-fields" disabled={connectSaving}>
						{/* Discovery inputs are intentionally unmounted after a successful
						    preview so credentials and already-reviewed destination details do
						    not compete with the attachment decisions. Check another vault
						    restores a clean discovery form. */}
						{!connectPreview && <>
							<p className="section-copy">Enter the existing vault&apos;s details. Replicaro detects the vault&apos;s engine automatically.</p>
						<label className="field"><span>Storage type</span><select value={connectForm.coldStorage ? "cold_s3" : connectForm.connector} disabled={Boolean(connectPreview || retryConnectionIntentId)} onChange={(event) => { if (connectRcloneAuth?.sessionId) void closeRcloneAuthorization(connectRcloneAuth.sessionId); setConnectRcloneAuth(null); const coldStorage = event.target.value === "cold_s3"; const connector = coldStorage ? "s3" : event.target.value; const selected = connectionIntegrations.find((item) => item.id === connector); invalidateConnectPreview(); setConnectForm((current) => ({ ...current, engine: coldStorage ? "restic" : current.engine, connector, coldStorage, archiveWriteClass: "GLACIER", checkSchedule: coldStorage ? "manual" : current.checkSchedule, location: "", pendingLocation: "", options: integrationDefaults(selected), objectLock: emptyObjectLock() })); }}>{connectionIntegrations.flatMap((item) => [<option key={item.id} value={item.id}>{item.label}</option>, ...(item.id === "s3" ? [<option key="cold_s3" value="cold_s3">Cold Storage [Must Be S3 Compatible]</option>] : [])])}</select>{connectIntegration && <small>{connectForm.coldStorage ? "Supports all S3-compatible cold object storage that accept GLACIER or DEEP_ARCHIVE storage class." : connectIntegrationDescription}</small>}</label>
						{(!connectPreview || !connectUsesRcloneLogin) && (connectIntegration?.localBrowser ? <label className="field"><span>Location</span><DirectoryField value={connectForm.location} disabled={Boolean(retryConnectionIntentId)} placeholder={examplePlaceholder(connectIntegration.placeholder)} onChange={(location) => { invalidateConnectPreview(); setConnectForm((current) => ({ ...current, location, pendingLocation: "" })); }} /></label> : connectIntegration ? <RemoteVaultFields form={connectForm} integration={connectIntegration} onChange={(next) => { if (!retryConnectionIntentId) invalidateConnectPreview(); setConnectForm(next); }} /> : null)}
						{!connectUsesRcloneLogin && connectOrderedOptions.filter((option) => !option.advanced && !connectionOptionIsCustom(connectForm.connector, option.key)).map((option) => <IntegrationField key={option.key} option={option} value={connectForm.options[option.key] ?? ""} disabled={Boolean(retryConnectionIntentId && !option.credential && !option.secret)} onChange={(value) => { if (!retryConnectionIntentId) invalidateConnectPreview(option.key === "storage_class" && connectDetectedEngine === "restic"); setConnectForm((current) => updateConnectorOption(current, option.key, value)); }} />)}
							{(!connectPreview || !connectUsesRcloneLogin) && <label className="field"><span>Vault encryption password</span><input type="password" value={connectForm.password} onChange={(event) => { if (!retryConnectionIntentId) invalidateConnectPreview(); setConnectForm((current) => ({ ...current, password: event.target.value })); }} /><small>The password is used to decrypt and validate the vault. The password is never stored in the vault, and the password never leaves your computer.</small></label>}
						{matchingConnectionIntent && <section className="recovery-warning recovery-fallback-message" role="status">
							<p><strong>{retryConnectionError ? "The previous connection could not be completed." : matchingConnectionIntent.state === "prepared" ? "An earlier connection was saved for this destination." : "An unfinished connection was found for this destination."}</strong></p>
							<p>{retryConnectionError ? "The saved attempt is still available. Check the destination and password, then try again." : matchingConnectionIntent.state === "prepared" ? "It stopped before Replicaro published changes to the vault. Continue it, or cancel it and check these details again. Replicaro will verify the vault identity before continuing." : "Replicaro may have updated the vault's recovery profile. Continue the saved attempt; Replicaro will verify the vault identity and credentials before attaching it here."}</p>
							{retryConnectionError && <p>{retryConnectionError}</p>}
							<div className="vault-removal-actions">{!retryConnectionIntentId && <button className="btn primary" disabled={connectChecking || connectSaving} onClick={() => selectConnectionIntent(matchingConnectionIntent)}>Continue previous connection</button>}{matchingConnectionIntent.state === "prepared" && <button className="btn" disabled={connectChecking || connectSaving || !validVaultPassword(connectForm.password) || connectMissingRequiredOptions.length > 0} onClick={() => void startNewConnectionCheck(matchingConnectionIntent)}>Start a new check</button>}</div>
						</section>}
						{connectForm.coldStorage && <div className="recovery-warning">{coldStorageProviderGuidance.map((paragraph) => <p key={paragraph}>{paragraph}</p>)}</div>}
						{!connectUsesRcloneLogin && connectOrderedOptions.some((option) => option.advanced) && <button className="btn advanced-toggle" onClick={() => setConnectAdvanced((value) => !value)}><Icon name="settings" size={14} />{connectAdvanced ? "Hide advanced settings" : "Advanced settings"}</button>}
						{!connectUsesRcloneLogin && connectAdvanced && <div className="advanced-panel">{connectOrderedOptions.filter((option) => option.advanced && !connectionOptionIsCustom(connectForm.connector, option.key)).map((option) => <IntegrationField key={option.key} option={option} value={connectForm.options[option.key] ?? ""} disabled={Boolean(retryConnectionIntentId && !option.credential && !option.secret)} onChange={(value) => { if (!retryConnectionIntentId) invalidateConnectPreview(option.key === "storage_class" && connectDetectedEngine === "restic"); setConnectForm((current) => updateConnectorOption(current, option.key, value)); }} />)}</div>}
						<div className="modal-footer"><button className="btn" disabled={connectSaving} onClick={() => { if (!connectSaving) { resetConnectWorkflow(); setShowVaultConnect(false); } }}>Cancel</button>
										{matchingConnectionIntent && retryConnectionIntentId ? <button className="btn primary" disabled={connectChecking || connectSaving || !validVaultPassword(connectForm.password) || connectMissingRequiredOptions.length > 0} onClick={() => void retryPendingConnection()}>{connectSaving && <span className="spinner" />}{retryConnectionError ? "Retry connection" : "Continue previous connection"}</button> : (!matchingConnectionIntent || canCheckAnotherRcloneAccount) && <button className="btn primary" disabled={connectChecking || !vaultLocation(connectForm) || !validVaultPassword(connectForm.password) || connectMissingRequiredOptions.length > 0} onClick={() => { if (connectUsesRcloneLogin) setConnectRcloneAuthorizationAction("check"); else void checkExistingVault(); }}>{connectChecking && <span className="spinner" />}Check existing vault</button>}
						</div>
						</>}
						{connectPreview && <>
							{connectPreview.existingVault ? <div className="recovery-warning recovery-fallback-message">
									<p><strong>Registered vault found: {connectPreview.existingVault.name}</strong></p>
									<p>This is the same protected vault UUID as the existing registration. Replicaro will update that one vault; it will not add a duplicate or import jobs from this copy.</p>
									<p>All current local backup jobs, job UUIDs, sources, target memberships, and source bindings will remain unchanged.</p>
								</div> : connectPreview.mode === "fallback" ? <div className="recovery-warning recovery-fallback-message">
									<p>Existing {connectPreview.engine[0].toUpperCase() + connectPreview.engine.slice(1)} vault found. Replicaro will import this vault and convert it into a Replicaro vault. Your backed up data will not be modified.</p>
									<p>Replicaro allows you to browse, restore, and delete snapshots that already exist in this vault, but Replicaro will not apply any retention policies to those snapshots. Retention policies will apply to all new snapshots going forward.</p>
									<p>Replicaro automatically creates backup jobs of all the backed up sources in your {connectPreview.engine[0].toUpperCase() + connectPreview.engine.slice(1)} snapshots. The jobs will be disabled, and you will need to manually enable them if you want to run them; or, you can delete the jobs if you do not want them.</p>
									<p>Remember to disable {connectPreview.engine[0].toUpperCase() + connectPreview.engine.slice(1)} (and any other software aside from Replicaro) from accessing this vault. Only Replicaro should now be using this vault.</p>
								</div> : <div className="recovery-warning recovery-fallback-message">
									<p>Existing Replicaro vault found. The vault uses {connectPreview.engine[0].toUpperCase() + connectPreview.engine.slice(1)} as the engine.</p>
									{!connectProfileUUID && <p>{connectProfileChoiceMade ? "Replicaro will join this vault as a new profile." : "Choose how this computer will join the vault."}</p>}
									{connectProfileUUID && <p>Replicaro will import all vault settings, backup jobs, and snapshots from the selected profile. All imported jobs are initially disabled. {connectAccessReminder}</p>}
									{!connectProfileUUID && <p>{connectAccessReminder}</p>}
								</div>}
							{connectPreview.profileDegraded && <p className="recovery-warning">The canonical recovery profile is damaged or missing. A verified previous generation was used and will be repaired during connection.</p>}
							{connectProfileSelectionVisible && <fieldset className="connection-decision field multi-computer-field" aria-busy={Boolean(connectPendingProfileChoice)}>
								<legend>Do you want to join this vault as a new profile or use an existing profile? Using an existing profile allows you to import that profile&apos;s jobs and take control of the profile&apos;s backed up snapshots. Using an existing profile takes it over; Replicaro on the other computer can no longer use that profile unless it reconnects or joins with another profile.</legend>
								<div className="radio-options">
									<label><input type="radio" name="connect-profile" disabled={connectChecking} checked={connectPendingProfileChoice ? connectPendingProfileChoice.join : connectProfileChoiceMade && connectProfileAction === "join"} onChange={() => void checkExistingVault("", true)} />Join vault as new profile</label>
									{(connectPreview.profiles ?? []).map((profile) => <label key={profile.profile_uuid}><input type="radio" name="connect-profile" disabled={connectChecking} checked={connectPendingProfileChoice ? !connectPendingProfileChoice.join && connectPendingProfileChoice.profileUUID === profile.profile_uuid : connectProfileUUID === profile.profile_uuid} onChange={() => void checkExistingVault(profile.profile_uuid)} />Use profile from {profile.attachment.display.computerName || "Unknown computer"}@{profile.attachment.display.operatingSystem || "unknown"} — Created {profileDateLabel(profile.createdAt)}{profile.vaultOwner ? " — Current vault owner" : ""}</label>)}
								</div>
								{connectPendingProfileChoice && <p className="connection-profile-loading" role="status" aria-live="polite"><span className="spinner" aria-hidden="true" />{connectPendingProfileChoice.join ? "Hang on, Replicaro is creating a new profile for you..." : "Hang on, Replicaro is grabbing the profile you selected..."}</p>}
								{/* Vault ownership follows the selected owner profile. Asking for a
								    second owner decision would suggest a separate root transfer that
								    this attachment transition neither needs nor performs. */}
								{selectedProfileIsOwner && <p className="connection-decision-note">{currentOwnerName} is the current vault owner. Vault owners are responsible for running integrity checks and space reclamation on a vault, and only vault owners can change the vault&apos;s encryption password. By selecting the {currentOwnerName} profile, you will also take over as vault owner.</p>}
							</fieldset>}
							{connectPreview.mode === "profile" && !connectPreview.existingVault && selectedConnectProfile?.localAttachment && <p className="connection-decision-note">This computer already has a profile in this vault, so Replicaro will reconnect it automatically.</p>}
							{connectPreview.mode === "profile" && !connectPreview.existingVault && connectProfileChoiceMade && !selectedProfileIsOwner && <fieldset className="connection-decision field multi-computer-field">
								<legend>{currentOwnerName} is the current vault owner. Vault owners are responsible for running integrity checks and space reclamation on a vault, and only vault owners can change the vault&apos;s encryption password. Do you want to take over as vault owner?</legend>
								<div className="radio-options">
									<label><input type="radio" name="connect-owner" checked={!selectedProfileIsOwner && connectOwnerChoiceMade && connectOwnerAction === "keep"} disabled={connectChecking || selectedProfileIsOwner} onChange={() => {
										// Relocking an owner-only field must restore the reviewed root value;
										// disabled controls still remain part of the connection payload.
										setConnectForm((current) => ({ ...current, checkSchedule: current.coldStorage ? "manual" : connectReviewedIntegritySchedule, maintenanceSchedule: connectReviewedMaintenanceSchedule }));
										setConnectOwnerAction("keep");
										setConnectOwnerChoiceMade(true);
									}} />No, leave {currentOwnerName} as the vault owner</label>
									<label><input type="radio" name="connect-owner" checked={selectedProfileIsOwner || connectOwnerChoiceMade && connectOwnerAction === "takeover"} disabled={connectChecking || selectedProfileIsOwner} onChange={() => { setConnectOwnerAction("takeover"); setConnectOwnerChoiceMade(true); }} />Yes, make me vault owner</label>
								</div>
							</fieldset>}
							{connectNameConflictNotice && <div className="recovery-warning recovery-fallback-message"><p>{connectNameConflictNotice}</p></div>}
							{connectPreview.mode === "fallback" && connectPreview.objectLockEnrollmentAvailable && <ObjectLockFields form={connectForm} creation onChange={setConnectForm} />}
							<div className="form-grid two"><label className="field"><span>Vault name</span><input aria-label="Vault name" disabled={connectUsesRcloneLogin} value={connectForm.name} onChange={(event) => setConnectForm({ ...connectForm, name: event.target.value })} /><small>{connectUsesRcloneLogin ? `This name cannot be changed from the existing vault name on ${connectIntegration?.label}.` : connectPreview.existingVault ? "Review the saved name for this existing vault." : "Vault names cannot be changed after initial connection. Choose wisely."}</small></label><label className="field"><span>Description (optional)</span><input value={connectForm.description} onChange={(event) => setConnectForm({ ...connectForm, description: event.target.value })} /></label></div>
							<VaultCareFields form={connectForm} integrityDisabled={connectIntegrityLocked} maintenanceDisabled={connectMaintenanceLocked} integrityLockedHelp={connectIntegrityLockedHelp} maintenanceLockedHelp={connectMaintenanceLockedHelp} onChange={setConnectForm} />
							{connectPreview.existingVault && <fieldset className="connection-decision field">
								<legend>Exact Update existing vault changes</legend>
								<ul>{existingVaultUpdateChanges.map((change) => <li key={change}>{change}</li>)}</ul>
								<label><input type="checkbox" checked={connectUpdateConfirmedDigest === connectUpdateReviewDigest} onChange={(event) => setConnectUpdateConfirmedDigest(event.target.checked ? connectUpdateReviewDigest : "")} />Update this existing vault registration with exactly these reviewed changes</label>
							</fieldset>}
						</>}
					</fieldset>
					{connectPreview && <div className="modal-footer"><button className="btn" disabled={connectSaving} onClick={() => { if (!connectSaving) { resetConnectWorkflow(); setShowVaultConnect(false); } }}>Cancel</button><button className="btn" disabled={connectSaving} onClick={resetConnectForNewAttempt}>Check another vault</button><button className="btn primary" disabled={connectSaving || connectChecking || connectRcloneNameConflict || !validVaultName(connectForm.name) || !connectProfileReady || !connectOwnerReady || Boolean(connectPreview.existingVault && connectUpdateConfirmedDigest !== connectUpdateReviewDigest) || !validObjectLockSettings(connectForm.objectLock, connectForm.maintenanceSchedule, connectPreview.mode === "profile")} onClick={() => void saveExistingVault()}>{connectSaving && <span className="spinner" />}{connectPreview.existingVault ? "Update existing vault" : "Connect vault"}</button></div>}
				</Modal>
			)}

			{showVaultConnect && connectRcloneAuthorizationAction && connectIntegration && (
				<Modal title={`Connect ${connectIntegration.label}`} onClose={closeConnectRcloneAuthorization}>
					<div className="modal-form">
						<p>Replicaro uses open source <a href="https://github.com/rclone/rclone" target="_blank" rel="noreferrer">rclone</a> to connect to {connectIntegration.label}. You need to authorize rclone with your {connectIntegration.label} account in order to create backups. You can revoke access at any time, and only Replicaro will be able to use this rclone access. Click the connect button below to get started.</p>
						<RcloneAuthorization provider={connectForm.connector} label={connectIntegration.label} readyAction="check existing vault" value={connectRcloneAuth} disabled={connectChecking || connectSaving} closeOnUnmount={false} showDescription={false} onChange={setConnectRcloneAuth} onError={(message) => toast("error", message)} />
					</div>
					<div className="modal-footer">
						<button className="btn" disabled={connectChecking || connectSaving} onClick={closeConnectRcloneAuthorization}>Back</button>
						{connectRcloneAuth?.status === "ready" && <button className="btn primary" disabled={connectChecking || connectSaving} onClick={() => {
							if (connectRcloneAuthorizationAction === "retry") void retryPendingConnection();
							else void checkExistingVault().then((succeeded) => { if (succeeded) setConnectRcloneAuthorizationAction(null); });
						}}>{(connectChecking || connectSaving) && <span className="spinner" />}{connectRcloneAuthorizationAction === "retry" ? "Retry connection" : "Check existing vault"}</button>}
					</div>
				</Modal>
			)}

            {showVaultCreate && !showCreateRcloneAuthorization && (
				<Modal title="Add a vault" wide onClose={closeVaultCreate}>
					<fieldset className="modal-workflow-fields" disabled={vaultSaving}>
					<div className="modal-form">
						<p className="section-copy">Encryption, compression, and deduplication are automatically applied to all vaults. All fields are required except ones marked as optional.</p>
						<label className="field"><span>Storage type</span><select autoFocus value={vaultForm.coldStorage ? "cold_s3" : vaultForm.connector} disabled={Boolean(vaultForm.pendingLocation)} onChange={(event) => { if (createRcloneAuth?.sessionId) void closeRcloneAuthorization(createRcloneAuth.sessionId); setCreateRcloneAuth(null); setShowCreateRcloneAuthorization(false); const coldStorage = event.target.value === "cold_s3"; const connector = coldStorage ? "s3" : event.target.value; const selected = vaultStorageIntegrations.find((item) => item.id === connector); const eligible = engineCatalog.filter((descriptor) => descriptor.installed && descriptor.providers.some((provider) => provider.id === connector && provider.supported)); const engine = coldStorage ? "restic" : eligible.some((descriptor) => descriptor.id === vaultForm.engine) ? vaultForm.engine : (eligible.find((descriptor) => descriptor.id === "restic")?.id ?? eligible[0]?.id ?? vaultForm.engine) as VaultForm["engine"]; setVaultForm({ ...vaultForm, engine, connector, coldStorage, archiveWriteClass: "GLACIER", checkSchedule: coldStorage ? "manual" : vaultForm.checkSchedule, location: "", pendingLocation: "", bucket: "", container: "", prefix: "", host: "", options: integrationOptionsForEngine(selected, engine, engineCatalog), objectLock: objectLockForSelection(vaultForm.objectLock, engine, connector) }); }}>{vaultStorageIntegrations.flatMap((item) => [<option key={item.id} value={item.id}>{item.label}</option>, ...(item.id === "s3" ? [<option key="cold_s3" value="cold_s3">Cold Storage [Must Be S3 Compatible]</option>] : [])])}</select>{integration && <small>{vaultForm.coldStorage ? "Supports all S3-compatible cold object storage that accept GLACIER or DEEP_ARCHIVE storage class." : createIntegrationDescription}</small>}</label>
						<label className="field"><span>Vault name</span><input aria-label="Vault name" disabled={Boolean(vaultForm.pendingLocation)} value={vaultForm.name} placeholder="example: Work archive" onChange={(event) => setVaultForm({ ...vaultForm, name: event.target.value })} /><small>Vault names cannot be changed after creation and are limited to {MAX_VAULT_NAME_CODE_POINTS} characters. Choose wisely.</small></label>
							{createUsesRcloneLogin && <><label className="field"><span>Description (optional)</span><input disabled={Boolean(vaultForm.pendingLocation)} value={vaultForm.description} onChange={(event) => setVaultForm({ ...vaultForm, description: event.target.value })} /></label><div className="form-grid two"><label className="field"><span>Encryption password</span><input type="password" value={vaultForm.password} onChange={(event) => setVaultForm({ ...vaultForm, password: event.target.value })} /><small>{vaultPasswordHelp}</small></label><label className="field"><span>Confirm encryption password</span><input type="password" value={vaultForm.passwordConfirmation} onChange={(event) => setVaultForm({ ...vaultForm, passwordConfirmation: event.target.value })} /></label></div>{vaultForm.password && vaultForm.passwordConfirmation && vaultForm.password !== vaultForm.passwordConfirmation && <small className="inline-error" role="alert">The passwords do not match.</small>}</>}
						{integration?.localBrowser ? <label className="field"><span>Location</span><DirectoryField value={vaultForm.location} disabled={Boolean(vaultForm.pendingLocation)} placeholder={examplePlaceholder(integration.placeholder)} onChange={(location) => setVaultForm({ ...vaultForm, location })} /></label> : integration && !createUsesRcloneLogin ? <RemoteVaultFields form={vaultForm} integration={integration} onChange={setVaultForm} /> : null}
						{/* Connection has no archive-class choice: managed Cold vaults recover the
						    protected class, and native Cold imports start from the GLACIER default. */}
						{vaultCreateError.includes("connect existing vault") && <div className="recovery-warning"><p>{vaultCreateError}</p><button className="btn sm" onClick={() => { if (!closeVaultCreate()) return; invalidateConnectPreview(); setRetryConnectionIntentId(""); setConnectForm((current) => restoreVaultDestinationFields({ ...current, connector: vaultForm.connector, coldStorage: vaultForm.coldStorage, archiveWriteClass: "GLACIER", checkSchedule: vaultForm.coldStorage ? "manual" : current.checkSchedule, location: vaultLocation(vaultForm, true), options: { ...vaultForm.options }, password: vaultForm.password })); setShowVaultConnect(true); refreshConnectionIntents(); }}>Connect to existing vault</button></div>}
						{vaultCreateError.includes("selected location is not empty") && <div className="recovery-warning"><p>{vaultCreateError}</p><div className="tool-buttons"><button className="btn sm" onClick={() => { setVaultCreateError(""); setVaultForm({ ...vaultForm, location: "", pendingLocation: "", bucket: "", container: "", prefix: "", host: "" }); }}>Select empty subfolder</button><button className="btn sm" onClick={() => { setVaultCreateError(""); setVaultForm({ ...vaultForm, location: "", pendingLocation: "", bucket: "", container: "", prefix: "", host: "" }); }}>Select different destination</button></div></div>}
						{!createUsesRcloneLogin && createOrderedOptions.filter((option) => !option.advanced && !creationOptionIsCustom(vaultForm.connector, option.key)).map((option) => <IntegrationField key={option.key} option={option} value={vaultForm.options[option.key] ?? ""} disabled={creationOptionLockedForPending(vaultForm, option)} onChange={(value) => setVaultForm(updateConnectorOption(vaultForm, option.key, value))} />)}
							{!createUsesRcloneLogin && <><label className="field"><span>Description (optional)</span><input disabled={Boolean(vaultForm.pendingLocation)} value={vaultForm.description} onChange={(event) => setVaultForm({ ...vaultForm, description: event.target.value })} /></label><div className="form-grid two"><label className="field"><span>Encryption password</span><input type="password" value={vaultForm.password} onChange={(event) => setVaultForm({ ...vaultForm, password: event.target.value })} /><small>{vaultPasswordHelp}</small></label><label className="field"><span>Confirm encryption password</span><input type="password" value={vaultForm.passwordConfirmation} onChange={(event) => setVaultForm({ ...vaultForm, passwordConfirmation: event.target.value })} /></label></div>{vaultForm.password && vaultForm.passwordConfirmation && vaultForm.password !== vaultForm.passwordConfirmation && <small className="inline-error" role="alert">The passwords do not match.</small>}</>}
						{vaultForm.coldStorage && <div className="recovery-warning">{coldStorageProviderGuidance.map((paragraph) => <p key={paragraph}>{paragraph}</p>)}</div>}
                        <button className="btn advanced-toggle" onClick={() => setVaultAdvanced((value) => !value)}><Icon name="settings" size={14} />{vaultAdvanced ? "Hide advanced settings" : "Advanced settings"}</button>
						{vaultAdvanced && (
							<div className="advanced-panel">
								<ObjectLockFields form={vaultForm} creation creationAvailable={createObjectLockAvailable} disabled={Boolean(vaultForm.pendingLocation)} onChange={updateCreateObjectLock} />
								<label className="field"><span>Vault engine</span><select value={vaultForm.engine} disabled={vaultForm.objectLock.enrolled || vaultForm.coldStorage || createUsesRcloneLogin || Boolean(vaultForm.pendingLocation)} onChange={(event) => { const engine = event.target.value as VaultForm["engine"]; const selected = vaultStorageIntegrations.find((item) => item.id === vaultForm.connector); setVaultForm({ ...vaultForm, engine, options: integrationOptionsForEngine(selected, engine, engineCatalog, vaultForm.options), objectLock: objectLockForSelection(vaultForm.objectLock, engine, vaultForm.connector) }); }}>{engineCatalog.filter((item) => vaultForm.coldStorage ? item.id === "restic" : !createUsesRcloneLogin || item.id === "restic").map((item) => <option key={item.id} value={item.id} disabled={!item.installed || !item.providers.some((provider) => provider.id === vaultForm.connector && provider.supported)}>{item.name}</option>)}</select><small>{vaultForm.objectLock.enrolled ? "Kopia is the required engine when object locking is enabled." : vaultEngineHelp(vaultForm.connector, vaultForm.coldStorage)}</small></label>
								{vaultForm.coldStorage && <label className="field"><span>Storage class</span><select value={vaultForm.archiveWriteClass} disabled={Boolean(vaultForm.pendingLocation)} onChange={(event) => setVaultForm({ ...vaultForm, archiveWriteClass: event.target.value as VaultForm["archiveWriteClass"] })}><option value="GLACIER">GLACIER (default)</option><option value="DEEP_ARCHIVE">DEEP_ARCHIVE</option></select><small>{coldStorageArchiveClassHelp}</small></label>}
							{!createUsesRcloneLogin && createOrderedOptions.filter((option) => option.advanced && !creationOptionIsCustom(vaultForm.connector, option.key)).map((option) => <IntegrationField key={option.key} option={option} value={vaultForm.options[option.key] ?? ""} disabled={creationOptionLockedForPending(vaultForm, option)} explanation={advancedCreationOptionExplanation(vaultForm.connector, option)} onChange={(value) => setVaultForm(updateConnectorOption(vaultForm, option.key, value))} />)}
								<VaultCareFields form={vaultForm} disabled={Boolean(vaultForm.pendingLocation)} onChange={setVaultForm} />
                            </div>
                        )}
                    </div>
					<div className="modal-footer"><button className="btn" disabled={vaultSaving} onClick={closeVaultCreate}>Cancel</button><button className="btn primary" disabled={vaultSaving || !integration || !validVaultName(vaultForm.name) || !vaultLocation(vaultForm, true) || !validVaultPassword(vaultForm.password) || vaultForm.password !== vaultForm.passwordConfirmation || createMissingRequiredOptions.length > 0 || !validObjectLockSettings(vaultForm.objectLock, vaultForm.maintenanceSchedule)} onClick={() => { if (createUsesRcloneLogin && !createRcloneAuth && !retryCreationIntentId) setShowCreateRcloneAuthorization(true); else void saveVault(); }}>{vaultSaving && <span className="spinner" />}{vaultSaving ? "Creating…" : retryCreationIntentId ? "Retry creation" : "Create vault"}</button></div>
					</fieldset>
                </Modal>
            )}

			{showVaultCreate && showCreateRcloneAuthorization && integration && (
				<Modal title={`Connect ${integration.label}`} onClose={closeCreateRcloneAuthorization}>
					<div className="modal-form">
						<p>Replicaro uses open source <a href="https://github.com/rclone/rclone" target="_blank" rel="noreferrer">rclone</a> to connect to {integration.label}. You need to authorize rclone with your {integration.label} account in order to create backups. You can revoke access at any time, and only Replicaro will be able to use this rclone access. Click the connect button below to get started.</p>
						{vaultCreateError && <div className="inline-error" role="alert">{vaultCreateError}</div>}
						<RcloneAuthorization provider={vaultForm.connector} label={integration.label} readyAction="create vault" value={createRcloneAuth} disabled={vaultSaving} closeOnUnmount={false} showDescription={false} onChange={setCreateRcloneAuth} onError={(message) => toast("error", message)} />
					</div>
					<div className="modal-footer">
						<button className="btn" disabled={vaultSaving} onClick={closeCreateRcloneAuthorization}>Back</button>
						{createRcloneAuth?.status === "ready" && <button className="btn primary" disabled={vaultSaving} onClick={() => void saveVault()}>{vaultSaving && <span className="spinner" />}{vaultSaving ? "Creating…" : "Create vault"}</button>}
					</div>
				</Modal>
			)}

			{creationErrorView && (
					<Modal title={`Pending creation: ${creationErrorView.name}`} onClose={() => setCreationErrorView(null)}>
					<div className="modal-form">
						<p className="section-copy">Replicaro kept the recovery record so this destination cannot be created over accidentally.</p>
						<div className="inline-error" role="alert">{creationErrorView.lastError || "Vault creation did not finish."}</div>
					</div>
					<div className="modal-footer">
						<button className="btn" onClick={() => setCreationErrorView(null)}>Close</button>
					</div>
				</Modal>
			)}

			{vaultSettings && (
				<Modal title={vaultSettings.name} onClose={closeVault}>
					<div className="vault-settings-content" aria-busy={vaultWorkState !== "idle"}>
						{vaultWorkState !== "idle" && <div className="vault-work-overlay" role="status" aria-live="polite">
							<p><span>{vaultWorkState === "running"
								? "Vault work is currently running. Vault settings cannot be changed while vault work is running. Please wait..."
								: vaultWorkState === "unavailable"
									? "Vault work status could not be checked. Vault settings cannot be changed right now. Please wait..."
									: "Checking whether vault work is running. Vault settings cannot be changed until this check finishes."}</span><span className="spinner" aria-hidden="true" /></p>
							{toolBusy === "maintenance" && vaultSettings.coldStorage && <button className="btn" onClick={cancelColdMaintenance}>Cancel reclamation</button>}
						</div>}
						<div inert={vaultWorkState !== "idle" ? true : undefined}>
					<div className="modal-location mono">{vaultSettings.engine} · {vaultSettings.location}</div>
					<div className="modal-section-label">Vault care</div>
					{vaultOwnershipPresentation === "checking" && <section className="recovery-warning vault-owner-status checking" role="status"><span className="spinner" aria-hidden="true" />Checking vault ownership status…</section>}
					{vaultOwnershipPresentation === "unverified" && <section className="recovery-warning vault-owner-status" role="alert"><p>Vault ownership status could not be verified. Vault-owner actions are disabled.</p><button className="btn" onClick={() => detectVaultOwnership(vaultSettings, vaultSettingsSession.current)}>Detect vault owner status</button></section>}
					{(vaultOwnershipPresentation === "owner" || vaultOwnershipPresentation === "nonowner") && vaultOwnership && <section className="recovery-warning vault-owner-status"><p>{vaultOwnership.message}</p>{vaultOwnershipPresentation === "nonowner" && <><p>{vaultOwnership.takeoverExplanation}</p><button className="btn danger" disabled={vaultOwnershipBusy} onClick={() => void takeOverVaultOwnership()}>{vaultOwnershipBusy && <span className="spinner" />}Click here to take over vault ownership</button></>}</section>}
					<div className="form-grid two vault-care-fields">
						{(vaultOwnershipPresentation === "owner" || vaultOwnershipPresentation === "nonowner") && <label className="field"><span>Integrity check</span><select value={vaultSettings.coldStorage ? "manual" : checkSchedule} disabled={vaultSettings.coldStorage || vaultOwnershipPresentation !== "owner"} onChange={(event) => setCheckSchedule(event.target.value)}>{(vaultSettings.coldStorage ? [["manual", "Disabled"]] as const : careSchedules).map(([value, label]) => <option key={value} value={value}>{label}</option>)}</select><small>{vaultSettings.coldStorage ? coldStorageIntegrityHelp : integrityCheckHelp}</small>{vaultOwnershipPresentation === "nonowner" && <small>Only the current vault owner can change or run integrity checks. Take over vault ownership above to enable it on this computer.</small>}<small>Last: {vaultSettings.lastCheck ? `${timeAgo(vaultSettings.lastCheck)} (${vaultSettings.lastCheckStatus})` : "never"}{vaultSettings.nextCheck ? ` · next ${timeAgo(vaultSettings.nextCheck)}` : ""}</small></label>}
						{(vaultOwnershipPresentation === "owner" || vaultOwnershipPresentation === "nonowner") && <label className="field"><span>Space reclamation</span><select value={maintenanceSchedule} disabled={vaultOwnershipPresentation !== "owner"} onChange={(event) => setMaintenanceSchedule(event.target.value)}>{careSchedules.map(([value, label]) => <option key={value} value={value} disabled={!objectLockScheduleEligible(objectLock, value)}>{label}</option>)}</select><small>{objectLock.enrolled ? (objectLock.paused ? pausedObjectLockMaintenanceHelp : objectLockMaintenanceHelp) : maintenanceHelp}</small>{vaultOwnershipPresentation === "nonowner" && <small>Only the current vault owner can change or run space reclamation. Take over vault ownership above to enable it on this computer.</small>}<small>Last: {vaultSettings.lastMaintenance ? `${timeAgo(vaultSettings.lastMaintenance)} (${vaultSettings.lastMaintenanceStatus})` : "never"}{vaultSettings.nextMaintenance ? ` · next ${timeAgo(vaultSettings.nextMaintenance)}` : ""}</small></label>}
						{(vaultOwnershipPresentation === "owner" || vaultOwnershipPresentation === "nonowner") && <div className="vault-care-actions"><div className="vault-care-action"><button className="btn" disabled={vaultSettings.coldStorage || vaultOwnershipPresentation !== "owner" || Boolean(toolBusy)} onClick={() => void runTool("check")}>{toolBusy === "check" && <span className="spinner" />}Run check now</button></div><div className="vault-care-action"><button className="btn" disabled={vaultOwnershipPresentation !== "owner" || Boolean(toolBusy)} onClick={() => void runTool("maintenance")}>{toolBusy === "maintenance" && <span className="spinner" />}Run reclamation now</button>{vaultSettings.coldStorage && toolBusy === "maintenance" && <button className="btn" onClick={cancelColdMaintenance}>Cancel reclamation</button>}</div></div>}
						<label className="field vault-job-speed-field"><span>Job speed</span><select value={concurrencyMode} onChange={(event) => setConcurrencyMode(event.target.value as VaultForm["concurrencyMode"])}><option value="reduced">Slower</option><option value="native">Normal</option><option value="increased">Faster</option><option value="maximum">Maximum</option></select><small>Controls how quickly Replicaro finishes backup, restore, integrity check, and space reclamation jobs. Higher speed means higher CPU, memory, disk, and network use. Set higher speeds with caution.</small></label>
					</div>
					<ObjectLockFields
						form={{ ...emptyVault(vaultSettingsIntegration, vaultSettings.engine), engine: vaultSettings.engine, connector: vaultSettings.connector, maintenanceSchedule, objectLock }}
						disabled={vaultOwnershipPresentation !== "owner"}
						originalObjectLock={vaultSettings.objectLock}
						onChange={(next) => { setObjectLock(next.objectLock); setMaintenanceSchedule(next.maintenanceSchedule); }}
					/>
					{vaultSettings.coldStorage && <p><strong>Archive write class:</strong> {vaultSettings.archiveWriteClass} (read-only)</p>}
					{vaultSettings.coldStorage && <div className="recovery-warning">{coldStorageProviderGuidance.map((paragraph) => <p key={paragraph}>{paragraph}</p>)}</div>}
					{vaultSettings.coldStorage && toolBusy === "maintenance" && <p className="recovery-warning">Cold storage retrieval can take hours or days. Replicaro will continue to wait for native Restic. {coldStorageCancellationGuidance}</p>}
                    {toolOutput && <>
                        <button type="button" className="text-button" aria-pressed={showRawToolLog} onClick={() => setShowRawToolLog((raw) => !raw)}>{showRawToolLog ? "Show readable log" : "Show raw log"}</button>
                        <pre className="output tool-output">{showRawToolLog ? toolOutput : formatNativeLogText(vaultSettings.engine, "care", toolOutput)}</pre>
                    </>}
					{vaultSettings.engine === "restic" && <>
						<button className="btn advanced-toggle vault-settings-advanced-toggle" onClick={() => setVaultSettingsAdvanced((value) => !value)}><Icon name="settings" size={14} />{vaultSettingsAdvanced ? "Hide advanced settings" : "Advanced settings"}</button>
						{vaultSettingsAdvanced && <div className="advanced-panel"><div className="advanced-setting"><label className="check"><input type="checkbox" checked={autoUnlock} onChange={(event) => setAutoUnlock(event.target.checked)} />Auto Unlock for Stuck Vaults</label><small>Restic will sometimes block use of a vault when running an operation. The vault remains blocked if the operation crashes. Auto unlock safely removes that block. Keep this option enabled unless you specifically understand what disabling this option means.</small></div></div>}
					</>}
					<section className="advanced-panel vault-password-panel">
						<div className="modal-section-label">Change vault password</div>
						{vaultPasswordRecovery && <div className="recovery-warning" role="status">
							<p><strong>Password-change recovery: {vaultPasswordRecovery.phase.replaceAll("_", " ")}</strong></p>
							{vaultPasswordRecovery.native.status && <p>Native {vaultPasswordRecovery.native.engine} result: {vaultPasswordRecovery.native.status}{vaultPasswordRecovery.native.output ? ` — ${formatNativeLogText(vaultPasswordRecovery.native.engine, "password_change", vaultPasswordRecovery.native.output)}` : ""}</p>}
							<p>Protected recovery metadata: {vaultPasswordRecovery.sidecarStatus}</p>
							<p>Local cleanup: {vaultPasswordRecovery.cleanupPending ? "pending" : "not pending"}</p>
							<p>{vaultPasswordRecovery.message}</p>
							<button className="btn" disabled={vaultPasswordChangeBusy} onClick={() => void retryPendingVaultPasswordChange()}>{vaultPasswordChangeBusy && <span className="spinner" />}Retry password-change recovery</button>
						</div>}
						{vaultOwnershipPresentation === "owner" && <p className="vault-password-intro">If you are backing up other computers to this vault, you will need to reconnect those computers to the vault after a password change.</p>}
						{vaultOwnershipPresentation === "nonowner" && <p>Only the current vault owner can change this vault password. {currentVaultOwner} is the current vault owner.</p>}
						{(vaultOwnershipPresentation === "owner" || vaultOwnershipPresentation === "nonowner") && <>
							<div className="form-grid two vault-password-fields">
								<label className="field"><span>New vault password</span><input type="password" autoComplete="new-password" disabled={vaultOwnershipPresentation !== "owner" || vaultPasswordChangeBusy || Boolean(vaultPasswordRecovery)} value={vaultPasswordForm.password} onChange={(event) => setVaultPasswordForm((current) => ({ ...current, password: event.target.value }))} /></label>
								<label className="field"><span>Confirm new vault password</span><input type="password" autoComplete="new-password" disabled={vaultOwnershipPresentation !== "owner" || vaultPasswordChangeBusy || Boolean(vaultPasswordRecovery)} value={vaultPasswordForm.confirmation} onChange={(event) => setVaultPasswordForm((current) => ({ ...current, confirmation: event.target.value }))} /></label>
							</div>
							{vaultPasswordForm.password && vaultPasswordForm.confirmation && vaultPasswordForm.password !== vaultPasswordForm.confirmation && <small className="inline-error" role="alert">The vault passwords do not match.</small>}
							<button className="btn" disabled={vaultOwnershipPresentation !== "owner" || vaultPasswordChangeBusy || Boolean(vaultPasswordRecovery)} onClick={() => void submitVaultPasswordChange()}>{vaultPasswordChangeBusy && <span className="spinner" />}Change vault password</button>
						</>}
					</section>
					{dormantJobs.length > 0 && <section className="unowned-snapshots"><div className="modal-section-label">Dormant recovery jobs</div><p>These definitions remain protected in vault.replicaro but do not run or own snapshots locally.</p>{dormantJobs.map((item) => <div className="unowned-snapshot" key={item.jobId}><span><strong>{item.definition.name}</strong><small>{displayPath(item.definition.source)} · {scheduleLabel(item.definition.schedule)}</small><small>Before script: {item.definition.beforeScriptPath ? `${item.definition.beforeScriptPath} (${item.definition.beforeScriptMustSucceed ? "required" : "optional"})` : "none"} · After script: {item.definition.afterScriptPath ? `${item.definition.afterScriptPath} (${item.definition.afterScriptMustSucceed ? "required" : "optional"})` : "none"}</small></span><div className="tool-buttons"><button className="btn sm" disabled={Boolean(dormantBusy)} onClick={() => void changeDormant(item.repositoryId, item.jobId, "restore")}>{dormantBusy === `restore:${item.jobId}` && <span className="spinner" />}Restore disabled</button><button className="btn sm danger-outline" disabled={Boolean(dormantBusy)} onClick={() => void changeDormant(item.repositoryId, item.jobId, "discard")}>{dormantBusy === `discard:${item.jobId}` && <span className="spinner" />}Discard definition</button></div></div>)}</section>}
					{vaultSettingsIntegration && (!usesRcloneNativeLogin(vaultSettings.connector) || (vaultSettings.engine === "restic" && vaultSettingsRcloneSupported)) && <section className="vault-connection-panel"><div className="modal-section-label">Vault connection</div><p>Use this to reconnect to the vault after disconnection for any reason.</p><button className="btn" onClick={() => requestSavedVaultReconnect(vaultSettings)}>Reconnect vault</button></section>}
					<div className="danger-zone"><div className="modal-section-label">Danger zone</div><p>Removing this vault also removes it as a destination from matching backup jobs. Jobs with no other destination are deleted. Data saved in the vault is untouched, and you can easily add the vault again later.</p><button className="btn danger-outline" onClick={() => { const repository = vaultSettings; dismissVaultSettings(); openVaultDelete(repository); }}>Remove vault from Replicaro</button></div>
						</div>
						<div className="modal-footer"><button className="btn" onClick={closeVault}>Cancel</button><button className="btn primary" disabled={vaultWorkState !== "idle" || careSaving || !validObjectLockSettings(objectLock, maintenanceSchedule, true, vaultSettings.objectLock)} onClick={() => void saveCare()}>{careSaving && <span className="spinner" />}Save</button></div>
					</div>
                </Modal>
            )}

            {vaultDelete && (
                <ConfirmDialog
					title={`Remove "${vaultDelete.name}"?`}
					message={<><span>Removing this vault also removes this vault as a destination for <strong>{jobCountFor(vaultDelete.id)} backup job{jobCountFor(vaultDelete.id) === 1 ? "" : "s"}</strong>.</span><br /><br /><span>Jobs that have other vaults as a destination will not be deleted. Jobs with just this vault as a destination will be deleted.</span><br /><br /><span>Data on disk is untouched — you can re-add the vault later.</span></>}
					confirmLabel="Remove vault"
					onConfirm={() => void removeVault(vaultDelete)}
                    onCancel={dismissVaultDelete}
                />
            )}

			{(vaultSaving || connectChecking || connectSaving) && vaultProgress.length > 0 && <aside className="vault-work-log" role="log" aria-label="Vault activity">
				<strong>Vault activity</strong>
				<div ref={vaultProgressView} className="vault-work-log-lines">{vaultProgress.map((record, index) => <div key={index} className={record.type === "stage" ? "vault-work-stage" : "vault-work-native"}>{record.text}</div>)}</div>
			</aside>}

            {closePrompt && (
                <SaveChangesDialog
                    onSave={saveChangesBeforeClose}
                    onDiscard={discardChanges}
					onCancel={() => { setVaultReconnectAfterClose(null); setClosePrompt(null); }}
                    busy={jobSaving || careSaving}
                />
            )}
        </div>
    );
}
