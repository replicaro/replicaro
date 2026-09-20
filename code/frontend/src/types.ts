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
	resolvedRepositoryPath?: string;
	resolvedRepositoryObservedAt?: string;
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
}

export interface IntegrationOption {
    key: string;
    label: string;
	help?: string;
    kind?: "boolean" | "textarea";
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
}

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

export interface ExistingVaultUpdateConfirmation {
	vaultUUID: string;
	previewDigest: string;
	previousLocation: string;
	location: string;
	name: string;
	description: string;
	checkSchedule: string;
	maintenanceSchedule: string;
	concurrencyMode: "reduced" | "native" | "increased" | "maximum";
}

export interface DormantRecoveryJob {
	repositoryId: string;
	jobId: string;
	definition: ProtectedProfileJob;
}

export interface SnapshotEntry {
    name: string;
    path?: string;
    mode: string;
    size: string;
    isDir: boolean;
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
    size: string;
    source: string;
    isDir: boolean;
    present: boolean;
	nativeRootId: string;
	machineLabel?: string;
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
    tag: string;
	beforeScriptPath: string;
	beforeScriptMustSucceed: boolean;
	afterScriptPath: string;
	afterScriptMustSucceed: boolean;
    engineSettings: Record<string, EngineJobSettings>;
    sizeBytes: number | null;
	sizeMeasuredAt: string;
}

export interface VaultSizeStatus {
	vaultSizeBytes: number | null;
	vaultSizeMeasuredAt: string;
	vaultSizeDirty: boolean;
	fresh: boolean;
	running: boolean;
	pending: boolean;
	paused: boolean;
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

export type StorageAvailability = "available" | "unavailable" | "unknown";

export type StorageAvailabilityReason =
    | ""
    | "not_checked"
    | "storage_missing"
    | "identity_mismatch"
    | "observation_failed"
    | "observation_timeout";

export interface OperationEntry {
    id: string;
    kind: string;
    status: "queued" | "running" | "success" | "failed" | "partial" | "completed_with_issues" | "interrupted" | "reconnect_required";
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
    kind: "operation" | "log";
    isNew?: boolean;
    operationKind?: string;
	operationId?: string;
    severity?: "warning" | "error";
    status?: "failed" | "interrupted" | "partial" | "completed_with_issues" | "reconnect_required";
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

export interface DashboardStats {
    repositoryCount: number;
    jobCount: number;
    enabledJobs: number;
    successfulRuns: number;
    failedRuns: number;
    warningRuns?: number;
    lastBackup: string;
    nextBackup: string;
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
    defaultEngine: "restic" | "kopia";
    autoStart: boolean;
    logRetentionDays: number;
    theme: ThemePreference;
    webhookUrl: string;
    notifyWindowsOnSuccess: boolean;
    notifyWindowsOnFailure: boolean;
    nativeNotificationsOnSuccess?: boolean;
    nativeNotificationsOnFailure?: boolean;
    notifyWebhookOnSuccess: boolean;
    notifyWebhookOnFailure: boolean;
    startWithWindows: boolean;
    startAtLogin?: boolean;
    minimizeToTray: boolean;
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
