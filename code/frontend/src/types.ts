import type { LanguagePreference } from "./i18n";

export interface ObjectLockSettings {
	enrolled: boolean;
	paused: boolean;
	mode: "compliance" | "governance" | "";
	durationValue: number;
	durationUnit: "days" | "weeks" | "months" | "years" | "";
}

export interface Repository {
    id: string;
    name: string;
    engine: "restic" | "kopia";
    connector: string;
    connectorLabel?: string;
	coldStorage: boolean;
	archiveWriteClass?: "DEEP_ARCHIVE" | "GLACIER";
	sftpPathMode?: "home" | "absolute";
    location: string;
    isNetwork?: boolean;
    description: string;
    hasPassword: boolean;
    hasCredentials: boolean;
    vaultSizeBytes: number | null;
    vaultSizeMeasuredAt: string;
    vaultSizeDirty: boolean;
    checkSchedule: string;
    nextCheck: string;
    lastCheck: string;
    lastCheckStatus: string;
    maintenanceSchedule: string;
	concurrencyMode: "reduced" | "native" | "increased" | "maximum";
    nextMaintenance: string;
    lastMaintenance: string;
    lastMaintenanceStatus: string;
	autoUnlock: boolean;
    createdAt: string;
	profile_uuid: string;
	attachment_generation: number;
	native_repository_id: string;
	isVaultOwner: boolean;
	objectLock: ObjectLockSettings;
	// Kopia's error correction overhead as Kopia reported it when this
	// computer created, connected or last reconnected the vault; 0 is off.
	eccOverheadPercent?: number;
	// Saved on the server; set when a job or background worker found that this
	// vault needs a reconnect, cleared by a later success or a reconnect.
	reconnectRequired?: boolean;
	// Set only for an Any Rclone Remote vault. It never holds the rclone
	// config password or a variable value.
	rcloneRemote?: RcloneRemoteSettings;
}

export interface RcloneRemoteSettings {
	configFile: string;
	configEncrypted: boolean;
	remote: string;
	path: string;
	variableNames: string[];
}

export interface IntegrationOption {
    key: string;
    label: string;
	help?: string;
    kind?: "boolean" | "textarea" | "environment";
    placeholder?: string;
    default?: string;
    required?: boolean;
    secret?: boolean;
	credential?: boolean;
    advanced?: boolean;
}

export interface StorageIntegration {
    id: string;
    label: string;
    description: string;
    placeholder: string;
    capabilities: Array<"storage" | "source" | "destination">;
    localBrowser?: boolean;
    options: IntegrationOption[];
}

export interface IntegrationCatalog {
    reviewedAt: string;
    integrations: StorageIntegration[];
}

export interface DirectoryListing {
    current: string;
    parent: string;
    roots: Array<{ name: string; path: string }>;
    directories: Array<{ name: string; path: string }>;
	// Only in the job-source picker's file mode (files=1), and left out when
	// the folder has no files.
	files?: Array<{ name: string; path: string }>;
}

// The new-job form's kind lookup for a typed source path (kind=1). It is a
// hint for the form only; the backend decides when the job is saved.
export type SourcePathKind = "file" | "folder" | "neither";

export interface Snapshot {
    nativeRootType?: "d" | "f";
    id: string;
    timestamp: string;
    size: string;
    duration: string;
    source: string;
	 tags?: string[];
	 ownershipMarker: {
		status: "missing" | "valid" | "malformed" | "duplicate" | "conflicting";
		jobId?: string;
		profileId?: string;
		markers?: string[];
	 };
	 sourceRoots?: Array<{ path: string; user?: string; host?: string; nativeRootId: string }>;
	 // Native roots this snapshot recorded as a single file, from its indexed
	 // entries. Absent for folder snapshots and snapshots not indexed yet.
	 fileRootIds?: string[];
	 presentation?: "managed" | "unmanaged";
	 managedJobId?: string;
	 machineLabel?: string;
}

export interface ProtectedProfileJob {
	job_uuid: string;
	name: string;
	source: string;
	schedule: string;
	retention: number;
	retentionHourly?: number;
	retentionDaily?: number;
	retentionWeekly?: number;
	retentionMonthly?: number;
	retentionYearly?: number;
	exclusions: string;
	tag: string;
	beforeScriptPath: string;
	beforeScriptMustSucceed: boolean;
	afterScriptPath: string;
	afterScriptMustSucceed: boolean;
	engineSettings: Record<string, EngineJobSettings>;
	enabled: boolean;
	target_vault_uuids: string[];
}

export interface ExistingVaultPreview {
	vaultUUID: string;
	baseline?: unknown;
	mode: "profile" | "fallback";
	digest: string;
	engine: "restic" | "kopia";
	engineVersion: string;
	profile?: {
		format: string;
		schemaVersion: number;
		revision: number;
		vault_uuid: string;
		profile_uuid: string;
		attachment: { client_uuid: string; generation: number; attachedAt: string; display: { computerName: string; operatingSystem: string } };
		vaultPreferences: { name: string; description: string; concurrencyMode: "reduced" | "native" | "increased" | "maximum" };
		createdAt: string;
		updatedAt: string;
		jobs: ProtectedProfileJob[];
	};
	profiles?: Array<{
		profile_uuid: string;
		attachment: { client_uuid: string; generation: number; attachedAt: string; display: { computerName: string; operatingSystem: string } };
		vaultPreferences: { name: string; description: string; concurrencyMode: "reduced" | "native" | "increased" | "maximum" };
		jobCount: number;
		createdAt: string;
		vaultOwner: boolean;
		localAttachment: boolean;
	}>;
	vault_owner_profile_uuid?: string;
	rootIntegritySchedule?: string;
	rootMaintenanceSchedule?: string;
	snapshots: Snapshot[];
	importedSources?: string[];
	profileDegraded?: boolean;
	localJobConflicts?: Record<string, "enabled" | "disabled-reviewed">;
	recoveredExclusions?: Record<string, string[]>;
	kopiaExclusionDiscrepancies?: Record<string, string>;
	coldStorage: boolean;
	archiveWriteClass?: "DEEP_ARCHIVE" | "GLACIER";
	objectLock?: ObjectLockSettings;
	objectLockEnrollmentAvailable?: boolean;
	existingVault?: {
		id: string;
		name: string;
		location: string;
		candidateLocation: string;
		description: string;
		checkSchedule: string;
		maintenanceSchedule: string;
		concurrencyMode: "reduced" | "native" | "increased" | "maximum";
		isVaultOwner: boolean;
	};
}

export interface DormantRecoveryJob {
	repositoryId: string;
	jobId: string;
	definition: ProtectedProfileJob;
}

// regularFile (here, on FileVersion and on FileHistoryVersion) is the
// backend's answer to whether the backup holds the item as a regular file, the
// only kind it can download: a folder, a symlink or a device is never one.
// Symlinks are left out because the engines disagree on them: `kopia show`
// prints the link's target text, which the browser would save as if it were
// the file, and `restic dump` refuses a symlink. Download is also never
// offered in a Cold Storage vault, whose data has to be retrieved first; the
// server refuses those vaults.
//
// modified (here and on FileVersion) is the modified time the engine lists for
// the item, RFC 3339 UTC in whole seconds. For a folder the engines differ:
// Restic's is the folder's own time, Kopia's is the newest time within it.
// It is "" only for a File History folder that the backup holds through its
// contents alone; an empty value hides the date. A
// Kopia time of exactly 1970-01-01T00:00:00Z may mean the listing had no time;
// it is shown as that date. size is already written for display by the
// backend.
export interface SnapshotEntry {
    name: string;
    path?: string;
    mode: string;
    size: string;
    modified: string;
    isDir: boolean;
	regularFile: boolean;
	nativeRootId?: string;
	nativeRootUser?: string;
	nativeRootHost?: string;
}

export interface FileSearchResult {
    path: string;
    name: string;
    source: string;
    isDir: boolean;
}

export interface FileBrowseEntry extends FileSearchResult {
    isSource: boolean;
	canExpand?: boolean;
}

export interface FileVersion {
    snapshotId: string;
    timestamp: string;
    modified: string;
    size: string;
    source: string;
    isDir: boolean;
	regularFile: boolean;
    present: boolean;
	nativeRootId: string;
	machineLabel?: string;
	// Set when the backend knows no name for the computer: machineLabel is then
	// its English "Previous computer" (with " #n" when several computers share
	// that label), and the page shows its own translation instead, with
	// unnamedComputerNumber as n (absent without one). Absent for a named
	// computer.
	unnamedComputer?: boolean;
	unnamedComputerNumber?: number;
}

// One version of a file in File History: a run of neighbouring backups of one
// computer and one set of source folders in which the engine reported no
// change to the file. Restore it from snapshotId with nativeRootId (the newest
// backup holding it). modified is RFC 3339 UTC, whole seconds; size is written
// like FileVersion.size; firstBackup and lastBackup are the timestamps of the
// oldest and newest backup in the run.
export interface FileHistoryVersion {
	snapshotId: string;
	nativeRootId: string;
	source: string;
	modified: string;
	size: string;
	// Whether the newest backup holding the version holds it as a regular file.
	// The API sends it, but the page's Download button reads the chosen backup's
	// own entry (FileVersion.regularFile) instead.
	regularFile: boolean;
	firstBackup: string;
	lastBackup: string;
	machineLabel: string;
	// See FileVersion.unnamedComputer.
	unnamedComputer?: boolean;
	unnamedComputerNumber?: number;
}

export interface MetadataIndexState {
    readySnapshots?: number;
    pendingSnapshots?: number;
    failedSnapshots?: number;
    knownSnapshots?: number;
	repositoryFailed?: boolean;
    retryAfter?: string;
	headerRetryAfter?: string;
	entryRetryAfter?: string;
	headerValid?: boolean;
	headerStale?: boolean;
	lastReconciled?: string;
	requiredGeneration?: number;
	appliedGeneration?: number;
	complete: boolean;
	storageModel?: "snapshot_repository";
	stale?: boolean;
	lastError?: string;
	refreshedAt?: string;
	retryAvailable?: boolean;
}

export interface BackupJob {
    id: string;
    name: string;
    source: string;
	sourceBindingState?: "bound" | "unbound_imported";
	// "file" for a job whose source is a single file. Folder jobs have no
	// value, and an imported job that isn't bound yet has "unknown"; treat
	// anything but "file" as a folder job.
	sourceKind?: string;
	// The job's alias, set only by "Update job source" (or kept from an
	// earlier version). Backups read from it; `source` stays immutable.
	resolvedSourcePath?: string;
	resolvedSourceObservedAt?: string;
    targets: BackupJobTarget[];
    schedule: string;
    enabled: boolean;
    nextRun: string;
    lastRun: string;
    retention: number;
	retentionHourly?: number;
	retentionDaily?: number;
	retentionWeekly?: number;
	retentionMonthly?: number;
	retentionYearly?: number;
    excludes: string;
	// The user's own "skip the contents of any folder that contains ..."
	// choices. The CACHEDIR.TAG one is also forced on while the job has a
	// Kopia vault; this saved value doesn't change then.
	skipNoBackupFolders: boolean;
	skipCacheDirFolders: boolean;
	skipMarkerFileName: string;
    tag: string;
	beforeScriptPath: string;
	beforeScriptMustSucceed: boolean;
	afterScriptPath: string;
	afterScriptMustSucceed: boolean;
    engineSettings: Record<string, EngineJobSettings>;
	// True when the job also backs up to a vault that isn't attached on this
	// computer. Its engine isn't known here, so the job's Kopia settings
	// section decides whether the job counts as using Kopia.
	unattachedTargets?: boolean;
    sizeBytes: number | null;
	sizeMeasuredAt: string;
	// Digest of the stored definition. An edit sends it back, and the backend
	// refuses the edit (job_definition_changed) when the job changed since.
	revision: string;
}

export interface VaultSizeStatus {
	vaultSizeBytes: number | null;
	vaultSizeMeasuredAt: string;
	vaultSizeDirty: boolean;
	fresh: boolean;
	running: boolean;
	pending: boolean;
	paused: boolean;
	// A failure code such as "vault_size_refresh_failed", not display text.
	failure?: string;
}

export interface EngineJobSettings {
    additionalOptions?: string[];
}

export interface BackupJobTarget {
    repositoryId: string;
    repositoryName: string;
    engine: "restic" | "kopia";
    lastRun: string;
    lastStatus: string;
    // Set only while lastStatus is "completed_with_issues": the qualifier of
    // the backup run that set it.
    lastIssueKind?: OperationIssueKind;
    operationId?: string;
    pendingCatchUp: boolean;
    coalescedMissedCount: number;
    firstDeferredDueAt: string;
    lastDueAt: string;
    sourceAvailability: StorageAvailability;
    sourceAvailabilityReason: StorageAvailabilityReason;
    sourceAvailabilityCheckedAt: string;
    targetAvailability: StorageAvailability;
    targetAvailabilityReason: StorageAvailabilityReason;
    targetAvailabilityCheckedAt: string;
    policyStatus: "pending" | "ready" | "error";
    policyError?: string;
}

// One folder-check warning from "Update job source". `reason` is the first
// matching check: an empty folder, a different folder name, or fewer than half
// of the latest backup's top-level items present. A job whose source is a
// single file gets the same shape with only "empty" (a zero-byte file) and
// "name".
export interface JobSourceFolderCheck {
	reason: "empty" | "name" | "items";
	chosen: string;
	current: string;
	chosenName: string;
	sourceName: string;
	matched: number;
	total: number;
}

export interface JobSourceUpdateResult {
	saved: boolean;
	check?: JobSourceFolderCheck;
	job?: BackupJob;
}

export type StorageAvailability = "available" | "unavailable" | "unknown";

export type StorageAvailabilityReason =
    | ""
    | "not_checked"
    | "storage_missing"
    | "identity_mismatch"
    | "observation_failed"
    | "observation_timeout"
    | "observation_slots_exhausted";

// Narrows a "completed_with_issues" backup without being a status of its own:
// "locked_files" means every problem was a source file another program had
// locked. Everything that ranks, counts or groups results keeps matching the
// status, so only the label changes. Older backends never send it.
export type OperationIssueKind = "locked_files";

export interface OperationEntry {
    id: string;
    kind: string;
    status: "queued" | "running" | "success" | "failed" | "partial" | "completed_with_issues" | "interrupted" | "reconnect_required";
    issueKind?: OperationIssueKind;
    title: string;
    jobId: string;
    repositoryId: string;
    engine: "restic" | "kopia";
    startedAt: string;
    finishedAt: string;
	steps?: Array<{
		id: string;
		operationId: string;
		domain: "native" | "orchestration" | "application";
		kind: string;
		status: "running" | "succeeded" | "failed" | "skipped" | "warning" | "interrupted";
		startedAt: string;
		finishedAt: string;
	}>;
}

export interface LiveOperationEntry {
	stream: "stdout" | "stderr";
	text: string;
}

export interface OperationLiveSnapshot {
	available: boolean;
	entries: LiveOperationEntry[];
	truncated: boolean;
	cancelable: boolean;
	cancelRequested: boolean;
}

export interface OperationLiveResponse {
	operation: OperationEntry;
	live: OperationLiveSnapshot;
}

export interface OperationLogResponse {
    rawBytes?: Uint8Array;
    readableBody?: string;
    readableDiagnostics?: string[];
	available: boolean;
	output: string;
	offset: number;
	previousOffset: number;
	nextOffset: number;
	size: number;
	eof: boolean;
}

export interface DashboardIssue {
    id: string;
    timestamp: string;
    // "kopia_policy_stuck" is a current condition of one Kopia vault, whose
    // name is the title.
    kind: "operation" | "log" | "kopia_policy_stuck";
    isNew?: boolean;
    operationKind?: string;
	operationId?: string;
    severity?: "warning" | "error";
    status?: "failed" | "interrupted" | "partial" | "completed_with_issues" | "reconnect_required";
    issueKind?: OperationIssueKind;
    level?: "ERROR" | "WARN";
    startedAt?: string;
    finishedAt?: string;
    title: string;
    outputAvailable: boolean;
}

export interface DashboardIssues {
    items: DashboardIssue[];
    hasMore: boolean;
    nextCursor?: string;
}

// The dashboard timeline tabs other than Issues, which has its own feed.
export type DashboardTimelineFilter = "all" | "backups" | "restores" | "checks" | "maintenance" | "other";

export type DashboardTimelineItem =
    | { id: string; kind: "operation"; operation: OperationEntry }
    | { id: string; kind: "log"; log: ActivityEntry };

export interface DashboardTimeline {
    items: DashboardTimelineItem[];
    hasMore: boolean;
    nextCursor?: string;
}

export interface DashboardStats {
    repositoryCount: number;
    jobCount: number;
    enabledJobs: number;
    successfulRuns: number;
    failedRuns: number;
    warningRuns?: number;
    lastBackup: string;
}

export interface ActivityEntry {
    id: number;
    timestamp: string;
    level: string;
    message: string;
}

export interface EngineInfo {
    installed: boolean;
    healthy: boolean;
    path: string;
    version: string;
    replicaroVersion: string;
    engines?: EngineDescriptor[];
}

export interface EngineDescriptor {
    id: "restic" | "kopia";
    name: string;
    compression: string;
    encryption: string;
    installed: boolean;
    path: string;
    version: string;
    error?: string;
    compatibilityWarning?: string;
    providers: Array<{ id: string; label: string; description: string; supported: boolean; fields?: string[] }>;
    jobSettings: string[];
	capabilities: {
		storageModel: "snapshot_repository";
		backup: NativeOperationCapability;
		restore: {
			fullSnapshot: boolean;
			singlePath: boolean;
			multiPath: boolean;
			originalLocation: boolean;
			conflictModes: Array<{ id: string; label: string; description: string; default: boolean }>;
			destinationScope: string;
		};
		verification: NativeOperationCapability;
		integrityCheck: NativeOperationCapability;
		retention: NativeOperationCapability;
		deletion: { supported: boolean; invocation: "single" | "batch"; resultGranularity: "per_id" | "batch_only" };
		maintenance: NativeOperationCapability;
		nativeEncryption: boolean;
		nativeCompression: boolean;
		nativeDeduplication: boolean;
	};
}

export interface NativeOperationCapability {
	supported: boolean;
	label: string;
	scope: string;
}

export interface PlatformCapabilities {
    startAtLogin: boolean;
    nativeNotifications: boolean;
    tray: boolean;
    menuBar: boolean;
}

export interface PlatformInfo {
    platform: string;
    os: string;
    arch: string;
    supported: boolean;
    capabilities: PlatformCapabilities;
}

export type ThemePreference = "system" | "light" | "neutral" | "dark";

export interface Settings {
    language?: LanguagePreference;
    effectiveLocale?: string;
    /** Read-only: the catalog the "system" preference resolves to now. */
    systemLocale?: string;
    defaultEngine: "restic" | "kopia";
    logRetentionDays: number;
    theme: ThemePreference;
    /** Read-only now: the server sets it to the first http(s) entry of notificationUrls and ignores it on save. */
    webhookUrl: string;
    /** http(s) entries go to the JSON webhook, other schemes are shoutrrr-style URLs. notifyWebhookOn* apply to all of them. */
    notificationUrls: string[];
    /**
     * Put in front of the desktop / notification URL titles exactly as typed.
     * The server always sends both; a save that leaves one out keeps the
     * stored value.
     */
    desktopNotificationTitlePrefix?: string;
    notificationUrlTitlePrefix?: string;
    notifyWindowsOnSuccess: boolean;
    notifyWindowsOnFailure: boolean;
    nativeNotificationsOnSuccess?: boolean;
    nativeNotificationsOnFailure?: boolean;
    notifyWebhookOnSuccess: boolean;
    notifyWebhookOnFailure: boolean;
    startWithWindows: boolean;
    startAtLogin?: boolean;
    maxConcurrentJobRuns: number;
    disableAutomaticUpdateChecks: boolean;
}

export interface AppUpdateStatus {
    runningVersion: string;
    result: "" | "up_to_date" | "update_available" | "unavailable";
    availableVersion?: string;
    skippedVersion?: string;
    availableVersionSkipped: boolean;
    automaticChecksDisabled: boolean;
    checking: boolean;
}

export interface SupportReport {
    report: string;
}
