import type {
    ActivityEntry,
    BackupJob,
    DashboardStats,
    DashboardIssues,
    EngineInfo,
    EngineDescriptor,
    OperationEntry,
	OperationLiveResponse,
	OperationLogResponse,
    Repository,
    Settings,
    IntegrationCatalog,
    DirectoryListing,
    FileSearchResult,
	FileBrowseEntry,
    FileVersion,
    MetadataIndexState,
    Snapshot,
    SnapshotEntry,
	ExistingVaultPreview,
	DormantRecoveryJob,
	PlatformInfo,
	AppUpdateStatus,
	SupportReport,
	ObjectLockSettings,
} from "../types";

// Packaged builds are served by the bound backend and must stay same-origin.
// Vite development is the only fixed-port client.
export const apiBaseURL = (development: boolean) =>
    development ? "http://127.0.0.1:9460" : "";

const API_URL = apiBaseURL(import.meta.env.DEV);

export class APIError extends Error {
	readonly status: number;
	readonly code?: string;
	readonly activation?: RcloneConfigActivation;
	readonly attached?: boolean;
	readonly usable?: boolean;
	readonly failureStage?: RcloneApplicationFailureStage;
	readonly passwordChangeResult?: VaultPasswordChangeResult;

	constructor(message: string, status: number, code?: string, lifecycle?: RcloneLifecycleOutcome, passwordChangeResult?: VaultPasswordChangeResult) {
		super(message);
		this.name = "APIError";
		this.status = status;
		this.code = code;
		this.activation = lifecycle?.activation;
		this.attached = lifecycle?.attached;
		this.usable = lifecycle?.usable;
		this.failureStage = lifecycle?.failureStage;
		this.passwordChangeResult = passwordChangeResult;
	}
}

export const isOperationNotFound = (reason: unknown) =>
	reason instanceof APIError && reason.status === 404;

async function request<T>(
    path: string,
    init?: RequestInit
): Promise<T> {
    let response: Response;
	// Public installation identity comes from the backend's HTML shell. Keep it
	// out of user settings/localStorage: a stale or separately configured value
	// could address the wrong installation. This is intentionally not a token.
	const clientUUID = document.querySelector<HTMLMetaElement>('meta[name="replicaro-client-uuid"]')?.content;
	if (!clientUUID) throw new Error("Installation UUID is unavailable. Reopen Replicaro.");

    try {
        const headers = new Headers(init?.headers);
        headers.set("X-Replicaro-Client-UUID", clientUUID);
        response = await fetch(`${API_URL}${path}`, { ...init, headers });
    } catch (reason) {
        if (reason instanceof DOMException && reason.name === "AbortError") throw reason;
        throw new Error(
            "Replicaro app appears closed. Please run it and try again.",
            { cause: reason }
        );
    }

    if (!response.ok) {
        let message = `Request failed (${response.status})`;
		let code: string | undefined;
		let lifecycle: RcloneLifecycleOutcome | undefined;
		let passwordChangeResult: VaultPasswordChangeResult | undefined;

        try {
            const body = await response.json();
            if (body && typeof body.error === "string") {
                message = body.error;
            }
			if (body && typeof body.code === "string") code = body.code;
			if (body && body.activation && typeof body.activation.disposition === "string") {
				lifecycle = {
					activation: body.activation,
					attached: Boolean(body.attached),
					...(typeof body.usable === "boolean" ? { usable: body.usable } : {}),
					...(typeof body.failureStage === "string" ? { failureStage: body.failureStage as RcloneApplicationFailureStage } : {}),
				};
			}
			if (body && body.result && typeof body.result.repositoryId === "string" && typeof body.result.phase === "string") {
				passwordChangeResult = body.result as VaultPasswordChangeResult;
			}
        } catch {
            /* non-JSON error body */
        }

		throw new APIError(message, response.status, code, lifecycle, passwordChangeResult);
    }

    if (response.status === 204) {
        return undefined as T;
    }

    const text = await response.text();

    return (text ? JSON.parse(text) : undefined) as T;
}

function post<T = void>(path: string, body?: unknown, method = "POST"): Promise<T> {
    return request<T>(path, {
        method,
        headers: { "Content-Type": "application/json" },
        body: body === undefined ? undefined : JSON.stringify(body),
    });
}

export interface VaultProgressRecord {
	type: "stage" | "native";
	text: string;
	stream?: "stdout" | "stderr";
}

// The endpoint keeps its normal JSON contract for every other caller. Only
// this opt-in request receives transient stage/native records followed by the
// original result envelope, including its exact coded failure status.
async function postWithVaultProgress<T>(path: string, body: unknown, onProgress?: (record: VaultProgressRecord) => void, signal?: AbortSignal): Promise<T> {
	if (!onProgress) return post<T>(path, body);
	const clientUUID = document.querySelector<HTMLMetaElement>('meta[name="replicaro-client-uuid"]')?.content;
	if (!clientUUID) throw new Error("Installation UUID is unavailable. Reopen Replicaro.");
	let response: Response;
	try {
		response = await fetch(`${API_URL}${path}`, {
			method: "POST", signal, body: JSON.stringify(body),
			headers: { "Content-Type": "application/json", "X-Replicaro-Client-UUID": clientUUID, "X-Replicaro-Vault-Progress": "1" },
		});
	} catch (reason) {
		if (reason instanceof DOMException && reason.name === "AbortError") throw reason;
		throw new Error("Replicaro app appears closed. Please run it and try again.", { cause: reason });
	}
	if (!response.ok || !response.body || !response.headers.get("Content-Type")?.includes("application/x-ndjson")) {
		const payload = await response.json().catch(() => ({}));
		if (!response.ok) throw vaultProgressError(response.status, payload);
		return payload as T;
	}
	const reader = response.body.getReader();
	const decoder = new TextDecoder();
	let pending = "";
	let completed = false;
	let result: T | undefined;
	const processLine = (line: string) => {
		if (!line.trim()) return;
		const record = JSON.parse(line) as { type: "stage" | "native" | "result"; text?: string; stream?: "stdout" | "stderr"; status?: number; body?: unknown };
		if (record.type === "result") {
			completed = true;
			if ((record.status ?? 500) >= 400) throw vaultProgressError(record.status ?? 500, record.body);
			result = record.body as T;
		} else if (!completed && (record.type === "stage" || record.type === "native") && typeof record.text === "string") {
			onProgress({ type: record.type, text: record.text, stream: record.stream });
		}
	};
	while (true) {
		const { value, done } = await reader.read();
		if (done) break;
		pending += decoder.decode(value, { stream: true });
		let newline = pending.indexOf("\n");
		while (newline >= 0) {
			processLine(pending.slice(0, newline));
			// The terminal envelope is the authoritative completed request.
			// A delayed EOF or later transport loss cannot undo that result.
			if (completed) {
				void reader.cancel().catch(() => {});
				return result as T;
			}
			pending = pending.slice(newline + 1);
			newline = pending.indexOf("\n");
		}
	}
	pending += decoder.decode();
	if (pending.trim()) processLine(pending);
	if (!completed) throw new Error("The vault operation ended before Replicaro received its result. Refresh to check its current state.");
	return result as T;
}

function vaultProgressError(status: number, payload: unknown): APIError {
	const body = payload && typeof payload === "object" ? payload as Record<string, unknown> : {};
	const message = typeof body.error === "string" ? body.error : `Request failed (${status})`;
	return new APIError(message, status, typeof body.code === "string" ? body.code : undefined,
		body.activation && typeof body.activation === "object" ? {
			activation: body.activation as RcloneConfigActivation,
			attached: Boolean(body.attached),
			...(typeof body.usable === "boolean" ? { usable: body.usable } : {}),
			...(typeof body.failureStage === "string" ? { failureStage: body.failureStage as RcloneApplicationFailureStage } : {}),
		} : undefined);
}

/* ------------------------------------------------------------------ */
/* Dashboard                                                           */
/* ------------------------------------------------------------------ */

export const getDashboard = () =>
    request<DashboardStats>("/api/dashboard");

export const getPlatform = () => request<PlatformInfo>("/api/platform");

export const markDashboardIssuesReviewed = () =>
    post<DashboardStats>("/api/dashboard/issues/review");

export const getDashboardIssues = (limit = 10, cursor = "") =>
    request<DashboardIssues>(`/api/dashboard/issues?limit=${limit}${cursor ? `&cursor=${encodeURIComponent(cursor)}` : ""}`);

export const getRecentActivity = () =>
    request<ActivityEntry[]>("/api/dashboard/activity");

export const getOperations = (limit = 0) =>
    request<OperationEntry[]>(`/api/operations${limit ? `?limit=${limit}` : ""}`);

export const getOperation = (id: string, signal?: AbortSignal) =>
    request<OperationEntry[]>(`/api/operations?id=${encodeURIComponent(id)}`, { signal });

export function requireExactOperation(operationId: string, operations: OperationEntry[]) {
	const exact = operations.find((operation) => operation.id === operationId);
	if (!exact) throw new Error("Exact operation lookup returned an unexpected response.");
	return exact;
}

const RESTORE_OPERATION_OBSERVATION_ATTEMPTS = 50;
const RESTORE_OPERATION_OBSERVATION_DELAY_MS = 100;
const RESTORE_OPERATION_OBSERVATION_WINDOW_MS = 5000;
const RESTORE_OPERATION_LOOKUP_TIMEOUT_MS = 500;

function abortableDelay(delay: number, signal?: AbortSignal) {
	return new Promise<void>((resolve) => {
		if (signal?.aborted) {
			resolve();
			return;
		}
		const timer = window.setTimeout(done, delay);
		function done() {
			window.clearTimeout(timer);
			signal?.removeEventListener("abort", done);
			resolve();
		}
		signal?.addEventListener("abort", done, { once: true });
	});
}

export async function observeSubmittedRestoreOperation(
	operationId: string,
	isCurrent: () => boolean,
	signal?: AbortSignal,
) {
	const deadline = Date.now() + RESTORE_OPERATION_OBSERVATION_WINDOW_MS;
	for (let attempt = 0; attempt < RESTORE_OPERATION_OBSERVATION_ATTEMPTS && isCurrent() && !signal?.aborted; attempt++) {
		const remaining = deadline - Date.now();
		if (remaining <= 0) break;
		const lookupController = new AbortController();
		const abortLookup = () => lookupController.abort();
		signal?.addEventListener("abort", abortLookup, { once: true });
		const timeout = window.setTimeout(abortLookup, Math.min(remaining, RESTORE_OPERATION_LOOKUP_TIMEOUT_MS));
		try {
			const operations = await getOperation(operationId, lookupController.signal);
			// Handoff is identity-sensitive: never infer the submitted restore from
			// recency, title, vault, or another row returned by a stale response.
			const exact = operations.find((operation) => operation.id === operationId);
			if (exact && isCurrent()) {
				window.clearTimeout(timeout);
				signal?.removeEventListener("abort", abortLookup);
				return exact;
			}
		} catch {
			// The row is inserted during request admission, so an early exact lookup
			// may legitimately observe 404. The bounded run-local loop owns no state.
		}
		window.clearTimeout(timeout);
		signal?.removeEventListener("abort", abortLookup);
		if (attempt + 1 < RESTORE_OPERATION_OBSERVATION_ATTEMPTS && isCurrent() && !signal?.aborted) {
			await abortableDelay(Math.min(RESTORE_OPERATION_OBSERVATION_DELAY_MS, Math.max(0, deadline - Date.now())), signal);
		}
	}
	return null;
}

export const getActiveOperations = () =>
    request<OperationEntry[]>("/api/operations/active");

export const getOperationLive = (id: string, signal?: AbortSignal) =>
	request<OperationLiveResponse>(`/api/operations/live?id=${encodeURIComponent(id)}`, { signal });

type OperationLogWireResponse = Omit<OperationLogResponse, "output" | "rawBytes" | "readableBody" | "readableDiagnostics"> & {
	outputBase64: string;
	rawOutputBase64?: string;
};

function decodeOperationLogOutput(value: string) {
	const binary = window.atob(value);
	const bytes = Uint8Array.from(binary, (character) => character.charCodeAt(0));
	return new TextDecoder("utf-8").decode(bytes);
}

export const getOperationLog = async (id: string, signal?: AbortSignal, offset = 0) => {
	// The response already carries Cache-Control: no-store. A request-side cache
	// mode is unnecessary and can make an otherwise ordinary local fetch fail in
	// browser-specific networking paths.
	const response = await request<OperationLogWireResponse>(`/api/operations/output?id=${encodeURIComponent(id)}${offset > 0 ? `&offset=${offset}` : ""}`, { signal });
	const { outputBase64, rawOutputBase64, ...page } = response;
	const rawBytes = rawOutputBase64 === undefined ? undefined : Uint8Array.from(window.atob(rawOutputBase64), character => character.charCodeAt(0));
	return { ...page, output: decodeOperationLogOutput(outputBase64), ...(rawBytes ? { rawBytes } : {}) };
};

export const cancelOperation = (operationId: string) =>
	post<{ accepted: boolean }>("/api/operations/cancel", { operationId });

/* ------------------------------------------------------------------ */
/* Repositories                                                        */
/* ------------------------------------------------------------------ */

export const getRepositories = (signal?: AbortSignal) =>
    request<Repository[]>("/api/repositories", { signal });

export const getRepository = (id: string) =>
    request<Repository>(`/api/repository?id=${encodeURIComponent(id)}`);

export interface CreateRepositoryInput {
	creationIntentId?: string;
    engine: "restic" | "kopia";
    name: string;
    connector: string;
	coldStorage: boolean;
	archiveWriteClass?: "DEEP_ARCHIVE" | "GLACIER";
    location: string;
    description: string;
    password: string;
    passwordConfirmation: string;
    options: Record<string, string>;
	rcloneAuthSessionId?: string;
    checkSchedule: string;
    maintenanceSchedule: string;
	concurrencyMode: "reduced" | "native" | "increased" | "maximum";
	objectLock: ObjectLockSettings;
}

export interface ProfileMutationResult {
	profilePending?: boolean;
	warning?: string;
}

export interface JobSaveResult extends ProfileMutationResult {
	job: BackupJob;
}

export interface JobEnabledResult extends ProfileMutationResult {
	enabled: boolean;
	changed: boolean;
}

export interface RepositoryMutationResult extends ProfileMutationResult {
	id: string;
	name?: string;
	created?: boolean;
}

export interface VaultProfileSyncStatus {
	repositoryId: string;
	revision: number;
	lastError?: string;
	attemptCount: number;
	nextAttemptAt?: string;
	updatedAt: string;
}

export interface RepositoryConnectionIntent {
	id: string;
	connector: string;
	coldStorage: boolean;
	archiveWriteClass?: "DEEP_ARCHIVE" | "GLACIER";
	location: string;
	reviewedOptions?: Record<string, string>;
	mode: string;
	previewDigest: string;
	state: string;
	error?: string;
	createdAt: string;
	updatedAt: string;
}

export interface RepositoryCreationIntent {
	id: string;
	engine: "restic" | "kopia";
	connector: string;
	coldStorage: boolean;
	archiveWriteClass?: "DEEP_ARCHIVE" | "GLACIER";
	location: string;
	name: string;
	description: string;
	checkSchedule: string;
	maintenanceSchedule: string;
	concurrencyMode: "reduced" | "native" | "increased" | "maximum";
	objectLock?: ObjectLockSettings;
	canonicalIdentity: string;
	reviewedOptions?: Record<string, string>;
	phase: "prepared" | "native_started" | "native_ready";
	lastError?: string;
}

export const getVaultProfileSyncStatuses = () =>
	request<VaultProfileSyncStatus[]>("/api/vault-profile-sync");

export const retryVaultProfileSync = (repositoryId: string) =>
	post<ProfileMutationResult>("/api/vault-profile-sync", { repositoryId });

export const createRepository = (input: CreateRepositoryInput, onProgress?: (record: VaultProgressRecord) => void) =>
	postWithVaultProgress<RepositoryMutationResult>("/api/repositories", input, onProgress);

export interface ExistingVaultStorageInput {
	connector: string;
	coldStorage: boolean;
	archiveWriteClass?: "DEEP_ARCHIVE" | "GLACIER";
	location: string;
	password: string;
	options: Record<string, string>;
	rcloneAuthSessionId?: string;
	profile_uuid?: string;
	expectedVaultUUID?: string;
}

export interface RepositoryReconnectFields extends ExistingVaultStorageInput {
	passwordConfirmation: string;
	name: string;
	description: string;
	checkSchedule: string;
	maintenanceSchedule: string;
	concurrencyMode: "reduced" | "native" | "increased" | "maximum";
	objectLock: ObjectLockSettings;
}

export interface RcloneAuthStatus {
	sessionId: string;
	provider: string;
	status: "waiting_browser" | "question" | "ready";
	authorizationUrl?: string;
	question?: {
		id: string;
		prompt: string;
		defaultValue: string;
		choices: Array<{ value: string; label: string }>;
		notice?: string;
	};
}

export interface RcloneConfigActivation {
	path?: string;
	disposition: "retained" | "activated" | "indeterminate";
}

export interface RcloneLifecycleOutcome {
	activation: RcloneConfigActivation;
	attached: boolean;
	usable?: boolean;
	failureStage?: RcloneApplicationFailureStage;
}

export type RcloneApplicationFailureStage =
	| "admission"
	| "saved_vault_validation"
	| "native_validation"
	| "protected_record_validation"
	| "config_activation"
	| "artifact_work"
	| "database_credential_update";

export const startRcloneAuthorization = (provider: string) =>
	post<RcloneAuthStatus>("/api/rclone/auth/start", { provider });

export const continueRcloneAuthorization = (sessionId: string, answer: string) =>
	post<RcloneAuthStatus>("/api/rclone/auth/continue", { sessionId, answer });

export const statusRcloneAuthorization = (sessionId: string) =>
	post<RcloneAuthStatus>("/api/rclone/auth/status", { sessionId });

export const closeRcloneAuthorization = (sessionId: string) =>
	post<void>(`/api/rclone/auth/session?id=${encodeURIComponent(sessionId)}`, undefined, "DELETE");

export const applyRcloneAuthorization = (sessionId: string, repositoryId: string) =>
	post<RcloneLifecycleOutcome>("/api/rclone/auth/apply", { sessionId, repositoryId });

export const previewExistingVault = (input: ExistingVaultStorageInput, signal?: AbortSignal, onProgress?: (record: VaultProgressRecord) => void) =>
	onProgress ? postWithVaultProgress<ExistingVaultPreview>("/api/vaults/connect/preview", input, onProgress, signal) : request<ExistingVaultPreview>("/api/vaults/connect/preview", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify(input),
		signal,
	});

export const selectExistingVaultProfile = (input: ExistingVaultStorageInput, baseline: ExistingVaultPreview["baseline"], signal?: AbortSignal, onProgress?: (record: VaultProgressRecord) => void) =>
	onProgress ? postWithVaultProgress<ExistingVaultPreview>("/api/vaults/connect/profile", { ...input, baseline }, onProgress, signal) : request<ExistingVaultPreview>("/api/vaults/connect/profile", {
		method: "POST",
		headers: { "Content-Type": "application/json" },
		body: JSON.stringify({ ...input, baseline }),
		signal,
	});

export const connectExistingVault = (input: ExistingVaultStorageInput & Record<string, unknown>, onProgress?: (record: VaultProgressRecord) => void) =>
	postWithVaultProgress<RepositoryMutationResult>("/api/vaults/connect", input, onProgress);

export const retryExistingVaultConnection = (input: {
	intentId: string;
	password: string;
	options: Record<string, string>;
	 rcloneAuthSessionId?: string;
}, onProgress?: (record: VaultProgressRecord) => void) => postWithVaultProgress<RepositoryMutationResult>("/api/vaults/connect/retry", input, onProgress);

export const getRepositoryConnectionIntents = () => request<RepositoryConnectionIntent[]>("/api/repository-connection-intents");
export const getRepositoryReconnectFields = (id: string) =>
	request<RepositoryReconnectFields>(`/api/repository?id=${encodeURIComponent(id)}&reconnect=true`);
export const cancelRepositoryConnectionIntent = (id: string) => post<void>(`/api/repository-connection-intents?id=${encodeURIComponent(id)}`, undefined, "DELETE");
export const getRepositoryCreationIntents = () => request<RepositoryCreationIntent[]>("/api/repository-creation-intents");
export const deleteRepositoryCreationIntent = (id: string) => post<ProfileMutationResult | undefined>(`/api/repository-creation-intents?id=${encodeURIComponent(id)}`, undefined, "DELETE");
export const getDormantRecoveryJobs = (repositoryId: string) => request<DormantRecoveryJob[]>(`/api/dormant-recovery-jobs?repositoryId=${encodeURIComponent(repositoryId)}`);
export const restoreDormantRecoveryJob = (repositoryId: string, jobId: string) => post<ProfileMutationResult>(`/api/dormant-recovery-jobs?repositoryId=${encodeURIComponent(repositoryId)}`, { jobId });
export const discardDormantRecoveryJob = (repositoryId: string, jobId: string) => post<ProfileMutationResult>(`/api/dormant-recovery-jobs?repositoryId=${encodeURIComponent(repositoryId)}&jobId=${encodeURIComponent(jobId)}`, undefined, "DELETE");
export const updateRepositoryCredentials = (repositoryId: string, options: Record<string, string>) => post<void>("/api/repository/credentials", { repositoryId, options }, "PUT");

export const getIntegrations = () =>
    request<IntegrationCatalog>("/api/integrations");

export const getEngines = () =>
    request<{ engines: EngineDescriptor[]; replicaroVersion: string }>("/api/engines");

export const browseDirectories = (path = "", signal?: AbortSignal) =>
    request<DirectoryListing>(
        `/api/filesystem/directories${path ? `?path=${encodeURIComponent(path)}` : ""}`,
        { signal }
    );

export const deleteRepository = (id: string, discardRecoveryProfile = false) =>
	post<ProfileMutationResult | undefined>(`/api/repositories?id=${encodeURIComponent(id)}${discardRecoveryProfile ? "&discardRecoveryProfile=true" : ""}`, undefined, "DELETE");

export const getRepositoryInfo = (id: string, snapshotId = "") =>
    request<{ output: string }>(
        `/api/repository/info?id=${encodeURIComponent(id)}` +
            (snapshotId ? `&snapshotId=${encodeURIComponent(snapshotId)}` : "")
    );

export const getVaultSizeStatus = (repositoryId: string) =>
    request<import("../types").VaultSizeStatus>(`/api/repository/vault-size?id=${encodeURIComponent(repositoryId)}`);

export const prepareVaultSize = (repositoryId: string, force = false) =>
    post<import("../types").VaultSizeStatus>(`/api/repository/vault-size/prepare?id=${encodeURIComponent(repositoryId)}&force=${force}`, {});

export const checkRepository = (id: string, snapshotId = "") =>
	post<{ output: string }>(
        `/api/repository/check?id=${encodeURIComponent(id)}` +
            (snapshotId ? `&snapshotId=${encodeURIComponent(snapshotId)}` : "")
    );

export const runMaintenance = (id: string, signal?: AbortSignal) =>
	request<{ output: string }>(
		`/api/repository/maintenance?id=${encodeURIComponent(id)}`,
		{ method: "POST", headers: { "Content-Type": "application/json" }, signal }
	);

export interface VaultOwnershipStatus {
	isOwner: boolean;
	ownerProfileUUID: string;
	localProfileUUID: string;
	ownerDisplay: { computerName: string; operatingSystem: string };
	localDisplay: { computerName: string; operatingSystem: string };
	integritySchedule: string;
	maintenanceSchedule: string;
	objectLock: ObjectLockSettings;
	message: string;
	takeoverExplanation: string;
}

export const getVaultOwnership = (id: string) =>
	request<VaultOwnershipStatus>(`/api/repository/ownership?id=${encodeURIComponent(id)}`, { cache: "no-store" });

export const forceVaultOwnershipTakeover = (id: string, reviewedOwnerProfileUUID: string) =>
	post<VaultOwnershipStatus>(`/api/repository/ownership?id=${encodeURIComponent(id)}`,
		{ reviewedOwnerProfileUUID, confirmed: true });

export interface VaultPasswordChangeResult {
	repositoryId: string;
	operationUUID: string;
	phase: string;
	native: { engine: string; status: string; processStarted?: boolean; output?: string; mutationDisposition?: "unknown" | "rejected_before_mutation" };
	sidecarStatus: string;
	cleanupPending: boolean;
	message: string;
	resticKeyTruth?: string;
}

export const changeVaultPassword = (repositoryId: string, newPassword: string, passwordConfirmation: string) =>
	post<VaultPasswordChangeResult>("/api/repository/password", {
		repositoryId, newPassword, passwordConfirmation,
	});

export const getVaultPasswordChangeStatus = (repositoryId: string) =>
	request<VaultPasswordChangeResult>(`/api/repository/password?id=${encodeURIComponent(repositoryId)}`);

export const retryVaultPasswordChange = (repositoryId: string) =>
	post<VaultPasswordChangeResult>("/api/repository/password/retry", { repositoryId });

export const updateRepositorySchedules = (input: {
    repositoryId: string;
    checkSchedule: string;
    maintenanceSchedule: string;
	concurrencyMode: "reduced" | "native" | "increased" | "maximum";
	autoUnlock: boolean;
	objectLock: ObjectLockSettings;
	profilePreferencesOnly?: boolean;
}) => post<ProfileMutationResult | undefined>("/api/repository/schedules", input, "PUT");

/* ------------------------------------------------------------------ */
/* Snapshots                                                           */
/* ------------------------------------------------------------------ */

export const listSnapshots = (repositoryId: string, signal?: AbortSignal) =>
    request<Snapshot[]>(
        `/api/repository/snapshots?id=${encodeURIComponent(repositoryId)}`,
        { signal }
    );

export const listCachedSnapshots = (repositoryId: string, signal?: AbortSignal) =>
	request<Snapshot[]>(
		`/api/repository/snapshots?id=${encodeURIComponent(repositoryId)}&mode=cached`,
		{ signal }
	);

export const refreshRestoreSnapshots = (repositoryId: string, signal?: AbortSignal) =>
	request<Snapshot[]>(
		`/api/repository/snapshots?id=${encodeURIComponent(repositoryId)}&mode=cached`,
		{ signal }
	);

export const listSnapshotFiles = (
    repositoryId: string,
    snapshotId: string,
	nativeRootId: string,
    path: string,
    signal?: AbortSignal
) =>
    request<{ entries: SnapshotEntry[]; raw: string }>(
        `/api/snapshot/files?id=${encodeURIComponent(repositoryId)}` +
            `&snapshotId=${encodeURIComponent(snapshotId)}` +
			`&nativeRootId=${encodeURIComponent(nativeRootId)}` +
            `&path=${encodeURIComponent(path)}`,
        { signal }
    );

export const deleteSnapshot = (repositoryId: string, snapshotId: string) =>
    post(
        `/api/snapshot?id=${encodeURIComponent(repositoryId)}` +
            `&snapshotId=${encodeURIComponent(snapshotId)}`,
        undefined,
        "DELETE"
    );

export const restoreSnapshot = (input: {
	operationId?: string;
    repositoryId: string;
    snapshotId: string;
    path: string;
	nativeRootId?: string;
    targetPath: string;
    conflictMode: string;
    originalLocation?: boolean;
}, signal?: AbortSignal) => request("/api/restore", {
	method: "POST",
	headers: { "Content-Type": "application/json" },
	body: JSON.stringify(input),
	signal,
});

export interface IndexedResult<T> {
    items: T[];
    indexing: boolean;
    index: MetadataIndexState;
    offset: number;
    nextOffset: number;
    hasMore: boolean;
	revision: number;
	storageModel?: "snapshot_repository";
}

export const retryFileIndex = (repositoryId: string) =>
    post<{ admitted: boolean }>(`/api/files/index/retry?id=${encodeURIComponent(repositoryId)}`);

export interface MetadataPreparationStatus {
	index: MetadataIndexState;
	readySnapshotIds: string[];
	running: boolean;
	pending: boolean;
	paused: boolean;
	stage: "" | "queued" | "preparing" | "refreshing_headers" | "indexing_entries";
	completeHeaderListing: boolean;
	admitted?: boolean;
}

export const prepareMetadata = (repositoryId: string, action: "access" | "retry" | "force") =>
	post<MetadataPreparationStatus>(`/api/metadata/prepare?id=${encodeURIComponent(repositoryId)}`, { action });

export const getMetadataStatus = (repositoryId: string, signal?: AbortSignal) =>
	request<MetadataPreparationStatus>(`/api/metadata/status?id=${encodeURIComponent(repositoryId)}`, { signal });

export const searchFiles = (repositoryId: string, query: string, offset = 0, signal?: AbortSignal, revision?: number) =>
    request<IndexedResult<FileSearchResult>>(
        `/api/files/search?id=${encodeURIComponent(repositoryId)}` +
			`&q=${encodeURIComponent(query)}&offset=${offset}` +
			(offset > 0 && revision !== undefined ? `&revision=${revision}` : ""),
        { signal }
    );

export const browseFiles = (
	repositoryId: string,
	parent = "",
	source = "",
	offset = 0,
	signal?: AbortSignal,
	revision?: number
) => request<IndexedResult<FileBrowseEntry>>(
	`/api/files/browse?id=${encodeURIComponent(repositoryId)}` +
		(source ? `&source=${encodeURIComponent(source)}` : "") +
		(parent ? `&parent=${encodeURIComponent(parent)}` : "") +
		`&offset=${offset}` +
		(offset > 0 && revision !== undefined ? `&revision=${revision}` : ""),
	{ signal }
);

export const getFileHistory = (
    repositoryId: string,
    path: string,
    source = "",
    offset = 0,
	signal?: AbortSignal,
	revision?: number
) =>
    request<IndexedResult<FileVersion>>(
        `/api/files/history?id=${encodeURIComponent(repositoryId)}` +
            `&path=${encodeURIComponent(path)}` +
            (source ? `&source=${encodeURIComponent(source)}` : "") +
			`&offset=${offset}` +
			(offset > 0 && revision !== undefined ? `&revision=${revision}` : ""),
        { signal }
    );

export const restoreSelection = (input: {
	operationId?: string;
    repositoryId: string;
    targetPath: string;
	items: Array<{
		snapshotId: string;
		path: string;
		nativeRootId: string;
	}>;
    conflictMode: string;
}, signal?: AbortSignal) => request<{ status: "success" | "failed"; attempted: number; restored: number; failed: number; orchestrationFailed?: number; notAttempted: number; items: Array<{ snapshotId: string; path: string; destinationRelative?: string; domain?: "native" | "orchestration"; status: string; output?: string; error?: string; nativeStatus?: string; nativeOutput?: string; nativeError?: string; orchestrationStatus?: string; orchestrationError?: string }> }>("/api/restore-selection", {
	method: "POST",
	headers: { "Content-Type": "application/json" },
	body: JSON.stringify(input),
	signal,
});

export const backupNow = (
    repositoryId: string,
    source: string
) => post("/api/repository/snapshot", { repositoryId, source });

/* ------------------------------------------------------------------ */
/* Backup jobs                                                         */
/* ------------------------------------------------------------------ */

export const getJobs = () => request<BackupJob[]>("/api/jobs");

export interface JobInput {
    id?: string;
    name: string;
    source: string;
    repositoryIds: string[];
    schedule: string;
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
	    engineSettings: Record<string, { additionalOptions?: string[] }>;
	    enabled?: boolean;
}

export const createJob = (input: JobInput) => post<JobSaveResult>("/api/jobs", input);

export const updateJob = (input: JobInput) =>
	post<JobSaveResult>("/api/jobs", input, "PUT");

export const deleteJob = (id: string) =>
	post<ProfileMutationResult | undefined>(`/api/jobs?id=${encodeURIComponent(id)}`, undefined, "DELETE");

export interface ManualTargetAdmissionResult {
    repositoryId: string;
    status: "admitted" | "storage_unavailable" | "busy" | "policy_not_ready";
    reasonCode?: string;
    operationId?: string;
}

export interface ManualJobRunResult {
    status: "admission_complete";
    count: number;
    results: ManualTargetAdmissionResult[];
}

export const runJob = (id: string, repositoryId?: string) =>
    post<ManualJobRunResult>(`/api/jobs/run?id=${encodeURIComponent(id)}${repositoryId ? `&repositoryId=${encodeURIComponent(repositoryId)}` : ""}`);

export const setJobEnabled = (id: string, enabled: boolean) =>
	post<JobEnabledResult>(`/api/jobs/enabled?id=${encodeURIComponent(id)}`, { enabled }, "PUT");

export const getJobStatus = () =>
    request<{ running: Record<string, string>; targets: Array<{ jobId: string; repositoryId: string; repositoryName: string; operationId: string; status: string }> }>("/api/jobs/status");

export const retryKopiaPolicy = (jobId: string, repositoryId: string) =>
	post<{ status: "pending" }>("/api/jobs/policy/retry", { jobId, repositoryId });

/* ------------------------------------------------------------------ */
/* Engine, logs, settings                                              */
/* ------------------------------------------------------------------ */

export const getEngineInfo = () => request<EngineInfo>("/api/engine");

export const getLogs = () => request<ActivityEntry[]>("/api/logs");

export const clearLogs = () => post("/api/logs", undefined, "DELETE");

export const getSettings = () => request<Settings>("/api/settings");

export const getSupportReport = (signal?: AbortSignal) => request<SupportReport>("/api/support/report", { signal, cache: "no-store" });

export const saveSettings = (settings: Settings) =>
    post("/api/settings", settings);

export const getAppUpdateStatus = () => request<AppUpdateStatus>("/api/app-update/status");

export const checkForAppUpdate = () => post<AppUpdateStatus>("/api/app-update/check");

export const skipAppUpdateVersion = (version: string) =>
	post<AppUpdateStatus & { stored: boolean }>("/api/app-update/skip", { version });

export const openAppUpdateDownload = (version: string) =>
	post<void>("/api/app-update/open", { version });
