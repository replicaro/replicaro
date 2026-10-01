import { formatDisplayDateTime, formatDisplayNumber, getEffectiveLocale, knownMessage, renderMessage, t } from "../i18n";
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Link, useNavigate } from "react-router-dom";

import { DirectoryField } from "../components/DirectoryPicker";
import { BackupRunPicker } from "../components/BackupRunPicker";
import { ListControls } from "../components/ListControls";
import { RcloneAuthorization } from "../components/RcloneAuthorization";
import { JobScriptFields } from "../components/JobScriptFields";
import { UpdateJobSourceDialog } from "../components/UpdateJobSourceDialog";
import { jobCurrentSource, jobSourceNeedsUpdate } from "../jobSourceDisplay";
import {
	activeJobDeletions,
	activeVaultChanges,
	followTrackedOperation,
	JOB_DELETION_KIND,
	newestVaultOperations,
	onTrackedOperationFinished,
	operationFailureReason,
	removalStoppedByRecoveryProfile,
	VAULT_PASSWORD_CHANGE_KIND,
	VAULT_REMOVAL_KIND,
} from "../services/trackedOperations";
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
	getOperations,
	getActiveOperations,
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
import { APIError, listRcloneRemotes } from "../services/api";
import type { RcloneRemoteEntry } from "../services/api";
import { ltrIsolate, parseRcloneRemoteVariables, serializeRcloneRemoteVariables } from "../services/rcloneRemote";
import { limitedSpeedProviders, RCLONE_REMOTE_CONNECTOR, usesRcloneSignIn, usesSingleProfile, usesVaultFolderName } from "../services/storageConnectors";
import type { ExistingVaultStorageInput, ForegroundOutcome, JobInput, ManualTargetAdmissionResult, RcloneAuthStatus, RepositoryConnectionIntent, RepositoryCreationIntent, TrackedOperationStart, VaultOwnershipStatus, VaultPasswordChangeResult, VaultProfileSyncStatus, VaultProgressRecord } from "../services/api";
import { backupTargetIsActive, jobMutationStatusLabel, manualRunNotice, manualRunResultText, nextSnapshotTooltip } from "../services/backupJobs";
import type { JobMutationResultStatus } from "../services/backupJobs";
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
	OperationEntry,
} from "../types";

import { formatNativeLogText } from "./nativeLogFormat";

const vaultPasswordChangeNeedsRecovery = (result: VaultPasswordChangeResult) =>
	["preparing", "native_started", "publishing", "cleanup_pending"].includes(result.phase);

// A change stopped before commit refuses every operation on the vault until it
// is retried; cleanup_pending has already committed the new password and does
// not block anything.
const vaultPasswordChangeBlocksVault = (result: VaultPasswordChangeResult | undefined) =>
	Boolean(result && ["preparing", "native_started", "publishing"].includes(result.phase));

type VaultSettingsPayload = Parameters<typeof updateRepositorySchedules>[0];
type VaultMutationBase = {
	repositoryId: string;
	repositoryName: string;
	generation: number;
	// The background operation doing the work, once the backend has queued it.
	operationId?: string;
};
type VaultPasswordMutation = VaultMutationBase & {
	kind: "password";
	status: "running" | "succeeded" | "recovery" | "error";
	result?: VaultPasswordChangeResult;
	// The operation's own final text and status, for the completion toast.
	message?: string;
	outcome?: OperationEntry["status"];
	error?: string;
};
type VaultSettingsMutation = VaultMutationBase & {
	kind: "settings";
	status: "running" | "succeeded" | "error";
	// Only a save started from this page carries its submitted values, which
	// Retry save resubmits. A save found running after a reload, or started in
	// another browser, has none.
	payload?: Readonly<VaultSettingsPayload>;
	error?: string;
	retainedPasswordRecovery?: VaultPasswordMutation;
};
type VaultMutation = VaultSettingsMutation | VaultPasswordMutation;

// A vault password change, settings save or removal runs as a background
// operation on the server: the dialog closes at once, the vault card shows the
// progress, and closing the browser does not stop the work. The card's state
// lives here, outside the page, so leaving the Protect page inside the app and
// coming back keeps it; and it is rebuilt from the backend's active operations
// (adoptActiveVaultChanges), so a reload or another browser shows the same
// progress. The page only subscribes.
let vaultMutationGeneration = 0;
let vaultMutationSnapshot: Record<string, VaultMutation> = {};
const vaultMutationListeners = new Set<(snapshot: Record<string, VaultMutation>) => void>();
// Vault change operations this browser has seen finish. An active-operations
// read that was sent before one finished can arrive after it; this keeps it
// from bringing the finished operation back as running.
const finishedVaultOperations = new Set<string>();
let vaultPasswordStatusObservationGeneration = 0;
let vaultPasswordStatusLifecycle = 0;
const vaultPasswordStatusObservationOwners: Record<string, number> = {};

type VaultPasswordStatusObservation = {
	repositoryId: string;
	generation: number;
	lifecycle: number;
};

const beginVaultPasswordStatusObservation = (repositoryId: string): VaultPasswordStatusObservation => {
	const generation = ++vaultPasswordStatusObservationGeneration;
	vaultPasswordStatusObservationOwners[repositoryId] = generation;
	return { repositoryId, generation, lifecycle: vaultPasswordStatusLifecycle };
};

const ownsVaultPasswordStatusObservation = (observation: VaultPasswordStatusObservation) =>
	observation.lifecycle === vaultPasswordStatusLifecycle &&
	vaultPasswordStatusObservationOwners[observation.repositoryId] === observation.generation;

const invalidateVaultPasswordStatusObservation = (repositoryId: string) => {
	vaultPasswordStatusObservationOwners[repositoryId] = ++vaultPasswordStatusObservationGeneration;
};

const publishVaultMutationSnapshot = (next: Record<string, VaultMutation>) => {
	vaultMutationSnapshot = next;
	for (const listener of vaultMutationListeners) listener(vaultMutationSnapshot);
};

const setVaultMutation = (mutation: VaultMutation) => publishVaultMutationSnapshot({
	...vaultMutationSnapshot,
	[mutation.repositoryId]: mutation,
});

const setAuthoritativeVaultPasswordMutation = (mutation: VaultPasswordMutation) => {
	// The operation's own result supersedes passive status reads that started
	// before it settled.
	invalidateVaultPasswordStatusObservation(mutation.repositoryId);
	setVaultMutation(mutation);
};

// Every completion must still own this exact vault and generation. That keeps
// a late result from changing a newer session or another vault's card.
const ownsVaultMutation = (repositoryId: string, generation: number) =>
	vaultMutationSnapshot[repositoryId]?.generation === generation;

const subscribeVaultMutations = (listener: (snapshot: Record<string, VaultMutation>) => void) => {
	vaultMutationListeners.add(listener);
	listener(vaultMutationSnapshot);
	return () => { vaultMutationListeners.delete(listener); };
};

type VaultRemovalPresentation = {
	phase: "removing" | "profile_error" | "error";
	operationId?: string;
	message?: string;
	// Remove anyway was chosen here, so its success is an information toast.
	discardRecoveryProfile?: boolean;
};

// Vault removal state is module-owned for the same reason as the settings and
// password changes above. When it lived in component state, a remounted page
// lost the "being removed" overlay and left the card unblocked, and the
// inventory refresh ran on the unmounted page, so the removed vault's card
// stayed visible. Keep the presentation and the settle notification here; the
// page only subscribes.
let vaultRemovalSnapshot: Record<string, VaultRemovalPresentation> = {};
const vaultRemovalListeners = new Set<(snapshot: Record<string, VaultRemovalPresentation>) => void>();
// Mounted pages reload their inventory through this once a removal settles.
// If no page is mounted, the next one loads the inventory when it mounts.
const vaultRemovalSettledListeners = new Set<() => void>();
const publishVaultRemoval = (repositoryId: string, presentation: VaultRemovalPresentation | undefined) => {
	const next = { ...vaultRemovalSnapshot };
	if (presentation) next[repositoryId] = presentation;
	else delete next[repositoryId];
	vaultRemovalSnapshot = next;
	for (const listener of vaultRemovalListeners) listener(vaultRemovalSnapshot);
};

const subscribeVaultRemovals = (listener: (snapshot: Record<string, VaultRemovalPresentation>) => void) => {
	vaultRemovalListeners.add(listener);
	listener(vaultRemovalSnapshot);
	return () => { vaultRemovalListeners.delete(listener); };
};

const subscribeVaultRemovalSettled = (listener: () => void) => {
	vaultRemovalSettledListeners.add(listener);
	return () => { vaultRemovalSettledListeners.delete(listener); };
};

const vaultRemovalIsInFlight = (repositoryId: string) => vaultRemovalSnapshot[repositoryId]?.phase === "removing";

// Operation IDs of failed removals the user dismissed with "Keep vault" in this
// browser. Only the newest few are kept: a dismissal matters only while its
// failed removal is still the vault's newest operation, so old entries are
// dead weight. Missing or blocked storage means the dismissal lasts only until
// the page is opened again; unreadable contents are treated as empty and
// replaced on the next dismissal.
const DISMISSED_VAULT_REMOVALS_KEY = "replicaro.protect.dismissedVaultRemovals";
const DISMISSED_VAULT_REMOVALS_LIMIT = 50;

const readDismissedVaultRemovals = (): string[] => {
	try {
		const stored: unknown = JSON.parse(window.localStorage.getItem(DISMISSED_VAULT_REMOVALS_KEY) ?? "[]");
		return Array.isArray(stored) ? stored.filter((id): id is string => typeof id === "string") : [];
	} catch {
		return [];
	}
};

const rememberDismissedVaultRemoval = (operationId: string) => {
	const dismissed = readDismissedVaultRemovals().filter((id) => id !== operationId);
	dismissed.push(operationId);
	writeStoredPreference(DISMISSED_VAULT_REMOVALS_KEY, JSON.stringify(dismissed.slice(-DISMISSED_VAULT_REMOVALS_LIMIT)));
};

// "Keep vault" only dismisses a settled error; a running removal keeps its
// overlay. It hides the card here and remembers the failed operation in this
// browser, so showStoppedVaultRemovals does not bring the card back after a
// reload. Nothing is saved on the server: another browser still shows the card
// until it is dismissed there too, or until any later work on the kept vault
// ends it everywhere. A new failed removal has a new operation ID and shows.
const dismissVaultRemoval = (repositoryId: string) => {
	const current = vaultRemovalSnapshot[repositoryId];
	if (!current || current.phase === "removing") return;
	if (current.operationId) rememberDismissedVaultRemoval(current.operationId);
	publishVaultRemoval(repositoryId, undefined);
};

// A settled error only means something while the vault is still listed. Once
// the authoritative inventory no longer has the vault (a removal finished
// elsewhere), drop its entry, so a later reconnect of the same vault UUID does
// not come back showing an error with a "Retry removal" nobody asked for. A
// running removal keeps its entry; it publishes its own outcome when it ends.
const forgetSettledVaultRemovalsNotIn = (repositories: readonly Pick<Repository, "id">[]) => {
	const listed = new Set(repositories.map((repository) => repository.id));
	for (const repositoryId of Object.keys(vaultRemovalSnapshot)) {
		if (!listed.has(repositoryId) && !vaultRemovalIsInFlight(repositoryId)) publishVaultRemoval(repositoryId, undefined);
	}
};

type Toast = (kind: "ok" | "error" | "info", message: string) => void;

// followVaultRemoval shows a queued or running removal on the vault card and
// reports its result when the operation ends. A removal that failed because the
// vault's recovery profile could not be updated offers Retry removal and
// Remove anyway; any other failure offers Retry removal.
const followVaultRemoval = (repositoryId: string, repositoryName: string, operationId: string, toast: Toast, discardRecoveryProfile?: boolean) => {
	publishVaultRemoval(repositoryId, {
		phase: "removing", operationId,
		discardRecoveryProfile: discardRecoveryProfile ?? vaultRemovalSnapshot[repositoryId]?.discardRecoveryProfile,
	});
	followTrackedOperation(operationId, async (operation) => {
		finishedVaultOperations.add(operation.id);
		const owns = () => vaultRemovalSnapshot[repositoryId]?.operationId === operation.id;
		if (!owns()) return;
		const message = await operationFailureReason(operation);
		if (!owns()) return;
		const discarded = vaultRemovalSnapshot[repositoryId]?.discardRecoveryProfile;
		if (operation.status === "success") {
			publishVaultRemoval(repositoryId, undefined);
			toast(discarded ? "info" : "ok", message);
		} else if (operation.status === "completed_with_issues") {
			// The vault is removed; only local cleanup is left. The backend error is
			// English text inside a sentence that may be right-to-left, so it goes in
			// a first-strong isolate (U+2068 ... U+2069) to keep its own direction and
			// punctuation. Isolates are invisible in left-to-right languages.
			publishVaultRemoval(repositoryId, undefined);
			toast("error", t("ui.protect.vaultRemovedCleanupNeedsAttention", { name: repositoryName, error: `\u2068${message}\u2069` }));
		} else {
			publishVaultRemoval(repositoryId, {
				phase: removalStoppedByRecoveryProfile(operation) ? "profile_error" : "error", operationId: operation.id, message,
			});
		}
		// Every outcome reloads the authoritative inventory: a removed vault's
		// card goes away, and a kept one shows its current state.
		for (const listener of vaultRemovalSettledListeners) listener();
	});
};

const runVaultRemoval = (
	repository: Pick<Repository, "id" | "name">,
	discardRecoveryProfile: boolean,
	toast: Toast,
) => {
	if (vaultRemovalIsInFlight(repository.id)) return;
	// The card shows the removal at once; the backend queues it and runs it in
	// the background, waiting for anything else on the vault first.
	publishVaultRemoval(repository.id, { phase: "removing", discardRecoveryProfile });
	const request = discardRecoveryProfile ? deleteRepository(repository.id, true) : deleteRepository(repository.id);
	void request.then(({ operationId }) => {
		if (!vaultRemovalIsInFlight(repository.id)) return;
		followVaultRemoval(repository.id, repository.name, operationId, toast, discardRecoveryProfile);
	}).catch((error: Error) => {
		if (!vaultRemovalIsInFlight(repository.id) || vaultRemovalSnapshot[repository.id]?.operationId) return;
		publishVaultRemoval(repository.id, { phase: "error", message: error.message });
		// Usually the request was refused and nothing was queued. If the answer
		// was lost instead, the reload finds the queued removal in the active
		// operations and follows it, or finds the vault already gone.
		for (const listener of vaultRemovalSettledListeners) listener();
	});
};

const clearVaultMutation = (repositoryId: string, generation?: number) => {
	if (generation !== undefined && !ownsVaultMutation(repositoryId, generation)) return;
	// A dismissed or completed presentation supersedes any status GET that was
	// already pending for this vault.
	invalidateVaultPasswordStatusObservation(repositoryId);
	const next = { ...vaultMutationSnapshot };
	delete next[repositoryId];
	publishVaultMutationSnapshot(next);
};

const freezeVaultSettingsPayload = (payload: VaultSettingsPayload): Readonly<VaultSettingsPayload> => Object.freeze({
	...payload,
	objectLock: Object.freeze({ ...payload.objectLock }),
});

const isCleanupPendingRecovery = (mutation: VaultMutation | undefined): mutation is VaultPasswordMutation =>
	mutation?.kind === "password" && mutation.status === "recovery" && mutation.result?.phase === "cleanup_pending";

const retainedPasswordRecoveryOf = (current: VaultMutation | undefined) =>
	isCleanupPendingRecovery(current) ? current : current?.kind === "settings" ? current.retainedPasswordRecovery : undefined;

// followVaultSettingsSave shows a queued or running settings save on the
// vault card and settles it when the operation ends.
const followVaultSettingsSave = (repositoryId: string, repositoryName: string, generation: number, operationId: string) => {
	const current = vaultMutationSnapshot[repositoryId];
	const own = current?.kind === "settings" && current.generation === generation ? current : undefined;
	setVaultMutation({
		kind: "settings", status: "running", repositoryId, repositoryName, generation, operationId,
		payload: own?.payload, retainedPasswordRecovery: own ? own.retainedPasswordRecovery : retainedPasswordRecoveryOf(current),
	});
	followTrackedOperation(operationId, async (operation) => {
		finishedVaultOperations.add(operation.id);
		const owned = () => {
			const mutation = vaultMutationSnapshot[repositoryId];
			return mutation?.kind === "settings" && mutation.operationId === operation.id ? mutation : undefined;
		};
		if (!owned()) return;
		const saved = operation.status === "success" || operation.status === "completed_with_issues";
		const error = saved ? undefined : await operationFailureReason(operation);
		const mutation = owned();
		if (!mutation) return;
		setVaultMutation(saved ? { ...mutation, status: "succeeded" } : { ...mutation, status: "error", error });
	});
};

const runVaultSettingsMutation = (
	repositoryId: string,
	repositoryName: string,
	payload: Readonly<VaultSettingsPayload>,
	replaceGeneration?: number,
) => {
	const current = vaultMutationSnapshot[repositoryId];
	if (replaceGeneration !== undefined) {
		if (!current || current.kind !== "settings" || current.generation !== replaceGeneration) return false;
	} else if (current && !isCleanupPendingRecovery(current)) {
		return false;
	}
	const retainedPasswordRecovery = retainedPasswordRecoveryOf(current);
	invalidateVaultPasswordStatusObservation(repositoryId);
	const generation = ++vaultMutationGeneration;
	setVaultMutation({ kind: "settings", status: "running", repositoryId, repositoryName, generation, payload, retainedPasswordRecovery });
	void updateRepositorySchedules(payload as VaultSettingsPayload).then(({ operationId }) => {
		if (!ownsVaultMutation(repositoryId, generation)) return;
		followVaultSettingsSave(repositoryId, repositoryName, generation, operationId);
	}).catch((error: Error) => {
		if (!ownsVaultMutation(repositoryId, generation)) return;
		setVaultMutation({ kind: "settings", status: "error", repositoryId, repositoryName, generation, payload, error: error.message, retainedPasswordRecovery });
	});
	return true;
};

const startVaultSettingsMutation = (repositoryName: string, payload: VaultSettingsPayload) =>
	runVaultSettingsMutation(payload.repositoryId, repositoryName, freezeVaultSettingsPayload(payload));

const retryVaultSettingsMutation = (repositoryId: string) => {
	const current = vaultMutationSnapshot[repositoryId];
	if (!current || current.kind !== "settings" || current.status !== "error" || !current.payload) return false;
	return runVaultSettingsMutation(repositoryId, current.repositoryName, current.payload, current.generation);
};

const settleVaultSettingsPresentation = (repositoryId: string, generation: number) => {
	const current = vaultMutationSnapshot[repositoryId];
	if (!current || current.kind !== "settings" || current.generation !== generation) return;
	if (current.retainedPasswordRecovery) {
		// cleanup_pending is a separate recovery state for an already committed
		// password. A settings presentation may temporarily cover it, but must not
		// dismiss it.
		setVaultMutation({ ...current.retainedPasswordRecovery, generation: ++vaultMutationGeneration });
		return;
	}
	clearVaultMutation(repositoryId, generation);
};

// passwordMutationFromResult presents the vault's saved password change phase
// record, which stays the recovery authority: an unfinished change offers
// Continue password change (Retry).
const passwordMutationFromResult = (
	repositoryName: string,
	generation: number,
	result: VaultPasswordChangeResult,
	error?: string,
): VaultPasswordMutation => {
	const base = { kind: "password" as const, repositoryId: result.repositoryId, repositoryName, generation, result };
	if (vaultPasswordChangeNeedsRecovery(result)) return { ...base, status: "recovery", error };
	// A completed orchestration means the new credential and sidecars were
	// committed. A failed or interrupted native result is kept for information,
	// but a completed password change is never reported as a failure.
	if (result.phase === "completed" && !result.cleanupPending) return { ...base, status: "succeeded" };
	return { ...base, status: "error", error };
};

// followVaultPasswordChange shows a queued or running password change (or
// retry) on the vault card. The card offers no Retry while it runs. When the
// operation ends, the vault's phase record decides what the card shows: an
// unfinished change stays as a recovery state with Retry; otherwise the
// operation's own result is reported.
const followVaultPasswordChange = (repositoryId: string, repositoryName: string, generation: number, operationId: string) => {
	setAuthoritativeVaultPasswordMutation({ kind: "password", status: "running", repositoryId, repositoryName, generation, operationId });
	followTrackedOperation(operationId, async (operation) => {
		finishedVaultOperations.add(operation.id);
		const owns = () => vaultMutationSnapshot[repositoryId]?.operationId === operation.id;
		if (!owns()) return;
		const message = await operationFailureReason(operation);
		let phase: VaultPasswordChangeResult | undefined;
		try {
			phase = await getVaultPasswordChangeStatus(repositoryId);
		} catch {
			// No phase record (404) means nothing is left to recover.
		}
		if (!owns()) return;
		const current = vaultMutationSnapshot[repositoryId];
		const failed = operation.status !== "success" && operation.status !== "completed_with_issues";
		if (phase?.repositoryId === repositoryId && vaultPasswordChangeNeedsRecovery(phase)) {
			setAuthoritativeVaultPasswordMutation(passwordMutationFromResult(repositoryName, current.generation, phase, failed ? message : undefined));
		} else if (!failed) {
			setAuthoritativeVaultPasswordMutation({
				kind: "password", status: "succeeded", repositoryId, repositoryName, generation: current.generation,
				operationId: operation.id, message, outcome: operation.status,
			});
		} else {
			setAuthoritativeVaultPasswordMutation({
				kind: "password", status: "error", repositoryId, repositoryName, generation: current.generation,
				operationId: operation.id, error: message,
			});
		}
	});
};

const runVaultPasswordMutation = (
	repositoryId: string,
	repositoryName: string,
	request: () => Promise<TrackedOperationStart>,
	replaceGeneration?: number,
) => {
	const current = vaultMutationSnapshot[repositoryId];
	if (current && current.generation !== replaceGeneration) return false;
	const generation = ++vaultMutationGeneration;
	setAuthoritativeVaultPasswordMutation({ kind: "password", status: "running", repositoryId, repositoryName, generation });
	void request().then(({ operationId }) => {
		if (!ownsVaultMutation(repositoryId, generation)) return;
		followVaultPasswordChange(repositoryId, repositoryName, generation, operationId);
	}).catch(async (error: Error) => {
		if (!ownsVaultMutation(repositoryId, generation)) return;
		if (!(error instanceof APIError)) {
			// No answer at all (the connection dropped, or the app went away while
			// answering): the backend may still have queued the change, and it
			// then runs whether or not this page hears about it. Look for it among
			// the active operations before saying it did not finish, and follow
			// it if it is there. A new change waits out a quiet period of at
			// least half a minute, so it is still active when this runs; a quick
			// Retry that already finished is not found here, and its result is
			// still on the dashboard and in the vault's phase record.
			try {
				const queued = activeVaultChanges(await getActiveOperations()).get(repositoryId);
				if (queued?.kind === VAULT_PASSWORD_CHANGE_KIND && ownsVaultMutation(repositoryId, generation)) {
					followVaultPasswordChange(repositoryId, repositoryName, generation, queued.id);
					return;
				}
			} catch {
				// Still unreachable; report the original error.
			}
			if (!ownsVaultMutation(repositoryId, generation)) return;
		}
		// Refused at the request: nothing was queued. The phase record, if any,
		// comes back on the next status read.
		setAuthoritativeVaultPasswordMutation({ kind: "password", status: "error", repositoryId, repositoryName, generation, error: error.message });
	});
	return true;
};

const startVaultPasswordMutation = (repositoryId: string, repositoryName: string, password: string, confirmation: string) =>
	runVaultPasswordMutation(repositoryId, repositoryName, () => changeVaultPassword(repositoryId, password, confirmation));

const continueVaultPasswordMutation = (repositoryId: string) => {
	const current = vaultMutationSnapshot[repositoryId];
	if (!current || current.kind !== "password" || current.status !== "recovery") return false;
	return runVaultPasswordMutation(repositoryId, current.repositoryName, () => retryVaultPasswordChange(repositoryId), current.generation);
};

const recordObservedVaultPasswordMutation = (repositoryId: string, repositoryName: string, result: VaultPasswordChangeResult) => {
	if (result.repositoryId !== repositoryId || !vaultPasswordChangeNeedsRecovery(result)) return;
	const current = vaultMutationSnapshot[repositoryId];
	// A running change or its retry owns the card until its operation ends; the
	// phase record it is working on is not a recovery state yet.
	if (current?.kind === "password" && current.status === "running") return;
	// A settings failure owns its immutable retry too. Its retained cleanup
	// recovery remains visible until settings presentation settles.
	if (current?.kind === "settings") return;
	setVaultMutation(passwordMutationFromResult(repositoryName, ++vaultMutationGeneration, result));
};

const recordMissingVaultPasswordMutation = (repositoryId: string) => {
	const current = vaultMutationSnapshot[repositoryId];
	// No phase record: a recovery state has nothing left to recover. Results of
	// a finished operation stay until they are shown or dismissed.
	if (current?.kind !== "password" || current.status !== "recovery") return;
	clearVaultMutation(repositoryId, current.generation);
};

// adoptActiveVaultChanges puts every queued or running password change,
// settings save or removal on its vault card, whichever page or browser
// started it, and follows it to the end. This is what makes the card's
// progress survive a reload: it comes from the backend's active operations,
// not from this page's memory.
const adoptActiveVaultChanges = (operations: OperationEntry[], repositoryName: (repositoryId: string) => string, toast: Toast) => {
	for (const [repositoryId, operation] of activeVaultChanges(operations)) {
		if (finishedVaultOperations.has(operation.id)) continue;
		const name = repositoryName(repositoryId) || operation.title.slice(operation.title.indexOf(": ") + 2);
		if (operation.kind === VAULT_REMOVAL_KIND) {
			const removal = vaultRemovalSnapshot[repositoryId];
			// This page's own request that has not had its answer yet adopts the
			// operation itself.
			if (removal?.operationId === operation.id || (removal?.phase === "removing" && !removal.operationId)) continue;
			followVaultRemoval(repositoryId, name, operation.id, toast);
			continue;
		}
		const current = vaultMutationSnapshot[repositoryId];
		if (current?.operationId === operation.id || (current?.status === "running" && !current.operationId)) continue;
		const generation = ++vaultMutationGeneration;
		if (operation.kind === VAULT_PASSWORD_CHANGE_KIND) followVaultPasswordChange(repositoryId, name, generation, operation.id);
		else followVaultSettingsSave(repositoryId, name, generation, operation.id);
	}
};

// showStoppedVaultRemovals brings back the "Removal stopped" card of a vault
// whose removal failed because its recovery profile could not be updated, so
// Retry removal and Remove anyway are still offered after a reload or in
// another browser. It is derived from that failed operation; the server keeps
// nothing else for it. It is shown only while that removal is the vault's
// newest operation of any kind and no vault change is active or starting for
// the vault: a backup, restore, settings save or anything else that came later
// means the user kept the vault and went on using it, and offering to remove
// it again from an old failure would be wrong. A removal already dismissed with
// "Keep vault" in this browser stays hidden.
const showStoppedVaultRemovals = (operations: OperationEntry[], repositories: readonly Pick<Repository, "id">[]) => {
	const listed = new Set(repositories.map((repository) => repository.id));
	const activeChanges = activeVaultChanges(operations);
	const dismissed = new Set(readDismissedVaultRemovals());
	for (const [repositoryId, operation] of newestVaultOperations(operations)) {
		if (!listed.has(repositoryId) || vaultRemovalSnapshot[repositoryId] || activeChanges.has(repositoryId) || dismissed.has(operation.id) ||
			vaultMutationSnapshot[repositoryId]?.status === "running" || !removalStoppedByRecoveryProfile(operation)) continue;
		publishVaultRemoval(repositoryId, { phase: "profile_error", operationId: operation.id });
		void operationFailureReason(operation).then((message) => {
			const current = vaultRemovalSnapshot[repositoryId];
			if (current?.phase === "profile_error" && current.operationId === operation.id) publishVaultRemoval(repositoryId, { ...current, message });
		});
	}
};

// Test isolation needs an explicit reset because this presentation state is
// intentionally module-owned so an internal route unmount cannot cancel work.
// eslint-disable-next-line react-refresh/only-export-components
export const resetVaultMutationPresentationForTests = () => {
	vaultPasswordStatusLifecycle++;
	for (const repositoryId of Object.keys(vaultPasswordStatusObservationOwners)) delete vaultPasswordStatusObservationOwners[repositoryId];
	publishVaultMutationSnapshot({});
	finishedVaultOperations.clear();
	vaultRemovalSnapshot = {};
	for (const listener of vaultRemovalListeners) listener(vaultRemovalSnapshot);
};

type RunSubmissionOwner = {
	session: number;
	generation: number;
	jobId: string;
	phase: "run";
};

// transfer_unfinished: this computer's takeover stopped partway and can be
// finished here. transfer_elsewhere: another computer's takeover is
// unfinished; only that computer can finish it.
type VaultOwnershipPresentation = "checking" | "owner" | "nonowner" | "unverified" | "transfer_unfinished" | "transfer_elsewhere";
type VaultWorkState = "checking" | "idle" | "running" | "unavailable";
type ActiveBackupTarget = { jobId: string; repositoryId: string; operationId: string; status: string };
type ObservedBackupOperation = { jobId: string; repositoryId: string };

function VaultActivityLog({ records }: { records: VaultProgressRecord[] }) {
	const container = useRef<HTMLElement | null>(null);
	const lines = useRef<HTMLDivElement | null>(null);
	const dialogWasScrolled = useRef(false);
	// Native records remain in the request capture and result path. This small
	// activity view is a stage-only progress account, not another native log.
	const stages = records.filter((record) => record.type === "stage");
	const hasStages = stages.length > 0;
	useEffect(() => {
		if (!hasStages) return;
		if (lines.current) lines.current.scrollTop = lines.current.scrollHeight;
		if (!dialogWasScrolled.current) {
			const dialog = container.current?.closest<HTMLElement>('[role="dialog"]');
			if (dialog) {
				// Bring the newly appeared request log into view once. Subsequent
				// records move only the ten-line viewport so a long request never
				// keeps dragging the user's dialog position.
				dialog.scrollTop = dialog.scrollHeight;
				dialogWasScrolled.current = true;
			}
		}
	}, [records, hasStages]);
	return <aside ref={container} className="vault-work-log" role="log" aria-label={t("ui.pages.protect.vault.activity")}>
		<strong>{t("ui.pages.protect.vault.activity")}</strong>
		<div ref={lines} className={`vault-work-log-lines${stages.length ? "" : " is-empty"}`}>{stages.map((record, index) => <div key={index} className="vault-work-stage">{record.text}</div>)}</div>
		<span className="spinner vault-work-log-spinner" aria-hidden="true" />
	</aside>;
}

// Vault creation, connection and ownership takeover run inside their request,
// so closing the window stops them partway. The browser's leave-page prompt
// (held by services/api.ts) catches a reload or tab close; this says it up
// front while the request runs. The shorter foreground saves get only the
// prompt.
function KeepWindowOpenNotice() {
	return <p className="vault-keep-open" role="status">{t("ui.protect.keepWindowOpen")}</p>;
}

// A foreground action whose change committed but left a cleanup step
// unfinished. The result is shown as completed with issues, with the
// backend's issue text, instead of as plain success.
function completedWithIssuesMessage(result: ForegroundOutcome | undefined) {
	if (result?.status !== "completed_with_issues") return undefined;
	return `${t("ui.jobStatus.completedWithIssues")}: ${(result.issues ?? []).join(" ")}`;
}

function VaultOverlayIdentity({ name }: { name: string }) {
	return <div className="vault-overlay-identity"><span className="vault-glyph" aria-hidden="true"><Icon name="shield" size={18} /></span><strong>{name}</strong></div>;
}

const vaultMutationBlocksCard = (mutation: VaultMutation) =>
	mutation.kind === "settings" ||
	mutation.status !== "recovery" ||
	mutation.result?.phase !== "cleanup_pending";

// A password change result is only shown while it needs recovery (see
// vaultPasswordChangeNeedsRecovery), so these are the phases that can appear
// here. Any other phase is shown with its underscores turned into spaces.
function vaultPasswordPhaseLabel(phase: string) {
	switch (phase) {
		case "preparing": return t("ui.protect.passwordPhase.preparing");
		case "native_started": return t("ui.protect.passwordPhase.nativeStarted");
		case "publishing": return t("ui.protect.passwordPhase.publishing");
		case "cleanup_pending": return t("ui.protect.passwordPhase.cleanupPending");
		default: return phase.replaceAll("_", " ");
	}
}

// The engine's result (not_started, succeeded, failed, interrupted) and the
// protected recovery metadata status (pending, succeeded, not_changed) of a
// password change. A status this version doesn't know is shown as sent.
function vaultPasswordStatusLabel(status: string) {
	switch (status) {
		case "pending": return t("ui.protect.pending");
		case "succeeded": return t("ui.operation.step.status.succeeded");
		case "failed": return t("ui.operation.failed");
		case "not_started": return t("ui.protect.passwordStatus.notStarted");
		case "interrupted": return t("ui.protect.passwordStatus.interrupted");
		case "not_changed": return t("ui.protect.passwordStatus.notChanged");
		default: return status;
	}
}

function VaultPasswordResult({ result }: { result: VaultPasswordChangeResult }) {
	return <div className="vault-password-result">
		<span><b>{t("ui.pages.protect.phase")}</b> {vaultPasswordPhaseLabel(result.phase)}</span>
		<span><b>{t("ui.protect.nativeResultLabel", { engine: result.native.engine })}</b> {result.native.status ? vaultPasswordStatusLabel(result.native.status) : t("ui.protect.unresolved")}{result.native.output ? ` — ${formatNativeLogText(result.native.engine, "password_change", result.native.output)}` : ""}</span>
		<span><b>{t("ui.pages.protect.protected.recovery.metadata")}</b> {vaultPasswordStatusLabel(result.sidecarStatus)}</span>
		<span><b>{t("ui.pages.protect.local.cleanup")}</b> {result.cleanupPending ? t("ui.protect.pending") : t("ui.protect.notPending")}</span>
		{/* The backend message and Restic key note are shown as written. dir="auto" gives
		    each the direction of its own text, so a right-to-left page keeps the full stop at the end. */}
		<span dir="auto">{result.message}</span>
		{result.resticKeyTruth && <span dir="auto">{result.resticKeyTruth}</span>}
		{result.native.mutationDisposition === "rejected_before_mutation" && !result.resticKeyTruth && <span>{t("ui.pages.protect.the.selected.password.was.not.changed")}</span>}
	</div>;
}

function VaultMutationOverlay({ mutation, onReopenSettings }: { mutation: VaultMutation; onReopenSettings: (repositoryId: string, generation: number) => void }) {
	const active = mutation.status === "running";
	const result = mutation.kind === "password" ? mutation.result : undefined;
	const retainedPasswordRecovery = mutation.kind === "settings" ? mutation.retainedPasswordRecovery : undefined;
	const cleanupNotice = mutation.kind === "password" && mutation.status === "recovery" && result?.phase === "cleanup_pending";
	return <div className={`vault-mutation-overlay${cleanupNotice ? " is-nonblocking" : ""}`} role={mutation.status === "error" ? "alert" : "status"} aria-live="polite">
		<VaultOverlayIdentity name={mutation.repositoryName} />
		<strong className="vault-overlay-status">{mutation.kind === "settings"
			? mutation.status === "running" ? t("ui.pages.protect.saving.vault.settings")
				: mutation.status === "succeeded" ? t("ui.pages.protect.vault.settings.saved")
					: t("ui.pages.protect.vault.settings.could.not.be.saved")
			: mutation.status === "running" ? t("ui.pages.protect.changing.vault.password")
				: mutation.status === "recovery" ? t("ui.pages.protect.password.change.needs.attention")
					: mutation.status === "succeeded" ? t("ui.pages.protect.vault.password.change.completed")
						: t("ui.pages.protect.vault.password.change.did.not.finish")}</strong>
		{/* The work runs on the server whether or not this page stays open, so
		    there is no "keep the window open" text here. */}
		{active && <span className="spinner" aria-hidden="true" />}
		{mutation.kind === "password" && mutation.status === "recovery" && vaultPasswordChangeBlocksVault(result) && <span className="vault-mutation-error">{t("ui.protect.vaultPasswordChangeBlocking")}</span>}
		{mutation.error && <span className="vault-mutation-error">{mutation.error}</span>}
		{result && <VaultPasswordResult result={result} />}
		{!active && <div className="vault-mutation-actions">
			{mutation.status === "error" && <button className="btn sm" onClick={() => mutation.kind === "settings" ? settleVaultSettingsPresentation(mutation.repositoryId, mutation.generation) : clearVaultMutation(mutation.repositoryId, mutation.generation)}>{t("ui.pages.protect.dismiss")}</button>}
			{mutation.kind === "settings" && mutation.status === "error" && mutation.payload && <button className="btn sm" onClick={() => retryVaultSettingsMutation(mutation.repositoryId)}>{t("ui.pages.protect.retry.save")}</button>}
			{mutation.kind === "settings" && mutation.status === "error" && <button className="btn sm" onClick={() => onReopenSettings(mutation.repositoryId, mutation.generation)}>{t("ui.pages.protect.reopen.settings")}</button>}
			{mutation.kind === "password" && mutation.status === "recovery" && <button className="btn sm" onClick={() => continueVaultPasswordMutation(mutation.repositoryId)}>{t("ui.pages.protect.continue.password.change")}</button>}
		</div>}
		{mutation.kind === "settings" && mutation.status !== "running" && retainedPasswordRecovery?.result && <div className="vault-retained-password-recovery">
			<strong>{t("ui.pages.protect.password.change.still.needs.attention")}</strong>
			<VaultPasswordResult result={retainedPasswordRecovery.result} />
			<div className="vault-mutation-actions"><button className="btn sm" onClick={() => runVaultPasswordMutation(
				mutation.repositoryId,
				retainedPasswordRecovery.repositoryName,
				() => retryVaultPasswordChange(mutation.repositoryId),
				mutation.generation,
			)}>{t("ui.pages.protect.continue.password.change")}</button></div>
		</div>}
	</div>;
}

const JOB_PAGE_SIZE = 5;
const VAULT_PAGE_SIZE = 6;
const JOB_PAGE_SIZE_OPTIONS = [JOB_PAGE_SIZE, 10, 20, 50] as const;
const VAULT_PAGE_SIZE_OPTIONS = [VAULT_PAGE_SIZE, 9, 18, 36] as const;
const JOB_PAGE_SIZE_STORAGE_KEY = "replicaro.protect.jobs.pageSize";
const VAULT_PAGE_SIZE_STORAGE_KEY = "replicaro.protect.vaults.pageSize";
const JOB_SORT_STORAGE_KEY = "replicaro.protect.jobs.sort";
const VAULT_SORT_STORAGE_KEY = "replicaro.protect.vaults.sort";
const MAX_VAULT_NAME_CODE_POINTS = 50;

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
		failureStage?: string;
	};
	const disposition = failure.activation?.disposition;
	const failureStage = failure.failureStage ? ` Failure stage: ${failure.failureStage}.` : "";
	if (disposition !== "activated" && disposition !== "indeterminate") {
		return { consumed: false, completed: false, message: `${failure.message}${failureStage}` };
	}
	if (failure.attached === true && failure.usable === true) {
		return {
			consumed: true,
			completed: true,
			message: `${failure.message}${failureStage} The vault is saved and usable; only a local follow-up or temporary authorization cleanup failed. Refresh to review its current state.`,
		};
	}
	return {
		consumed: true,
		completed: false,
		// Activation consumes the login session but keeps the vault-owned config.
		// A pending connection can retry that config before another login.
		message: `${failure.message}${failureStage} Native authorization was ${disposition}; attached=${Boolean(failure.attached)}, usable=${failure.usable === undefined ? "not assessed" : String(failure.usable)}. ${pendingConnection ? "Retry the pending connection. Replicaro will request rclone authorization if the saved configuration is unavailable." : "Reauthorize before retrying."}`,
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
const integrityCheckHelp = () => t("ui.protect.help.integrityCheckHelp");
const coldStorageIntegrityHelp = () => t("ui.protect.help.coldStorageIntegrityHelp");
const connectAccessReminder = () => t("ui.protect.disableOtherSoftwareReminder");
const profileDateLabel = (value: string) => {
	const date = new Date(value);
	return Number.isNaN(date.getTime()) ? t("ui.date.unknownDate") : formatDisplayDateTime(date, {
		month: "short",
		day: "numeric",
		year: "numeric",
		hour: "numeric",
		minute: "2-digit",
	});
};
const maintenanceHelp = () => t("ui.protect.help.maintenanceHelp");
const objectLockMaintenanceHelp = () => t("ui.protect.help.objectLockMaintenanceHelp");
const pausedObjectLockMaintenanceHelp = () => t("ui.protect.help.pausedObjectLockMaintenanceHelp");
const objectLockForwardHelp = () => t("ui.protect.help.objectLockForwardHelp");
const objectLockTransitionHelp = () => t("ui.protect.help.objectLockTransitionHelp");

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
const coldStorageProviderGuidance = () => [
    t("ui.protect.coldGuidance1", { sidecar: "vault.replicaro" }),
    t("ui.protect.coldGuidance2"),
    t("ui.protect.coldGuidance3"),
];
const coldStorageArchiveClassHelp = () => t("ui.protect.help.coldStorageArchiveClassHelp", { glacier: "GLACIER", deepArchive: "DEEP_ARCHIVE" });
const vaultPasswordHelp = () => t("ui.protect.help.vaultPasswordHelp");

function normalizedRcloneFolderName(value: string) {
	return value.trim().normalize("NFC");
}

// Any Rclone Remote is for advanced users, so it comes last.
const vaultStorageOrder = ["fs", "s3", "azblob", "gcs", "sftp", "webdav", "dropbox", "google_drive", "onedrive", RCLONE_REMOTE_CONNECTOR];
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
	const localized = knownMessage(`ui.integration.${connector}.description`, fallback);
	if (!engine) return localized;
	const engineDescription = engineCatalog
		.filter((descriptor) => descriptor.id === engine)
		.flatMap((descriptor) => descriptor.providers)
		.find((provider) => provider.id === connector && provider.supported)
		?.description;
	return engineDescription && engineDescription !== fallback ? engineDescription : localized;
}

// Do not show certification status in the end-user UI. Whether an
// engine/provider is available is decided by the backend's runtime checks.

function unicodeCodePointCount(value: string) {
	return Array.from(value).length;
}

// Every caller passes the bare example value (a path, URL, flag, or pattern),
// so the translated "example:" wording is added exactly once. The value itself
// stays untranslated. It is wrapped in a left-to-right isolate (U+2066 ...
// U+2069) because a placeholder has no markup to carry dir="ltr": without it a
// right-to-left language moves the leading "/" of "/srv/backups/vault" or the
// "--" of "--verbose" to the other end and shows "*.tmp" as "tmp.*".
function examplePlaceholder(value?: string) {
	return value ? t("ui.protect.examplePlaceholder", { value: `\u2066${value}\u2069` }) : value;
}

// A destination's last run status in the job card tooltip. The words match the
// Overview timeline, where an interrupted run is also shown as stopped. A
// status this version doesn't know is shown as the backend sent it.
function targetStatusLabel(status: string) {
	switch (status) {
		case "": return t("ui.destinationStatus.notRun");
		case "queued": return t("ui.operation.queued");
		case "running": return t("ui.operation.running");
		case "success": return t("ui.operation.success");
		case "failed": return t("ui.operation.failed");
		case "partial": return t("ui.operation.partial");
		case "interrupted": return t("ui.operation.stopped");
		case "completed_with_issues": return t("ui.operation.completedWithIssues");
		case "reconnect_required": return t("ui.operation.reconnectRequired");
		default: return status;
	}
}

// The backend reports a failed Vault Size refresh as a stable code rather than
// text. Anything else it sends is shown as it arrives.
function vaultSizeFailureText(failure: string) {
	return failure === "vault_size_refresh_failed" ? t("ui.protect.vaultSizeRefreshFailed") : failure;
}

type JobSort = "newest" | "oldest" | "name-asc" | "name-desc";
type VaultSort = "newest" | "oldest" | "size" | "name-asc" | "name-desc";

const JOB_SORT_OPTIONS: ReadonlyArray<{ value: JobSort; label: () => string }> = [
    { value: "newest", label: () => t("ui.sort.newestLastRun") },
    { value: "oldest", label: () => t("ui.sort.oldestLastRun") },
    { value: "name-asc", label: () => t("ui.sort.nameAscending") },
    { value: "name-desc", label: () => t("ui.sort.nameDescending") },
];

const VAULT_SORT_OPTIONS: ReadonlyArray<{ value: VaultSort; label: () => string }> = [
    { value: "newest", label: () => t("ui.sort.newest") },
    { value: "oldest", label: () => t("ui.sort.oldest") },
    { value: "size", label: () => t("ui.sort.largestSize") },
    { value: "name-asc", label: () => t("ui.sort.nameAscending") },
    { value: "name-desc", label: () => t("ui.sort.nameDescending") },
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

const customScheduleUnits: Record<string, { label: () => string; minutes: number; max: number } | undefined> = {
	custom: { label: () => t("ui.protect.minutesUnit"), minutes: 1, max: MAX_CUSTOM_SCHEDULE_MINUTES },
	"custom-hours": { label: () => t("ui.protect.hoursUnit"), minutes: 60, max: Math.floor(MAX_CUSTOM_SCHEDULE_MINUTES / 60) },
	"custom-days": { label: () => t("ui.protect.daysUnit"), minutes: 1440, max: Math.floor(MAX_CUSTOM_SCHEDULE_MINUTES / 1440) },
	"custom-weeks": { label: () => t("ui.protect.weeksUnit"), minutes: 10080, max: Math.floor(MAX_CUSTOM_SCHEDULE_MINUTES / 10080) },
	"custom-months": { label: () => t("ui.protect.monthsUnit"), minutes: 0, max: Math.floor(MAX_CUSTOM_SCHEDULE_MINUTES / (31 * 1440)) },
};

function customScheduleError(form: { schedule: string; customInterval: string }) {
	const unit = customScheduleUnits[form.schedule];
	if (!unit) return null;
	const count = Number(form.customInterval);
	if (/^\d+$/.test(form.customInterval) && Number.isSafeInteger(count) && count >= 1 && count <= unit.max) return null;
	const locale = getEffectiveLocale();
	// German unit names are nouns and keep their capital letters mid-sentence.
	const unitLabel = locale === "de" ? unit.label() : unit.label().toLocaleLowerCase(locale);
	return t("ui.protect.customScheduleError", { max: unit.max.toLocaleString(locale), unit: unitLabel });
}

const MAX_CRON_EXPRESSION_LENGTH = 256;
const cronScheduleHelp = () => t("ui.protect.help.cronScheduleHelp");

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
		? t("ui.protect.manualScheduleHelp")
		: t("ui.protect.automaticScheduleHelp");
}

const schedulePresets = [
    ["manual", () => t("ui.schedule.manualOnly")],
    ["hourly", () => t("ui.schedule.everyHour")],
    ["daily", () => t("ui.schedule.everyDay")],
    ["weekly", () => t("ui.schedule.everyWeek")],
    ["monthly", () => t("ui.schedule.everyMonth")],
	["custom", () => t("ui.schedule.everyNMinutes")],
	["custom-hours", () => t("ui.schedule.everyNHours")],
	["custom-days", () => t("ui.schedule.everyNDays")],
	["custom-weeks", () => t("ui.schedule.everyNWeeks")],
	["custom-months", () => t("ui.schedule.everyNMonths")],
	["cron", () => t("ui.schedule.cronStyle")],
] as const;

const retentionPresets = [
	["0", () => t("ui.retention.keepAll")],
	["10", () => t("ui.retention.keep10")],
	["50", () => t("ui.retention.keep50")],
	["100", () => t("ui.retention.keep100")],
	["1000", () => t("ui.retention.keep1000")],
	["custom", () => t("ui.retention.keepN")],
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
    ["manual", () => t("ui.schedule.manualOnly")],
    ["daily", () => t("ui.care.daily")],
    ["weekly", () => t("ui.care.weekly")],
    ["monthly", () => t("ui.care.monthly")],
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

interface JobMutationResult {
	jobId?: string;
	name: string;
	source?: string;
	status: JobMutationResultStatus;
	// A backend warning or error, or a translated notice such as the one shown
	// when a failed edit can't be retried while its job is running. A plain
	// success leaves this empty, because the translated status above already
	// says what happened.
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

// The form keeps the immutable source: an edit always saves job.source, never
// the "Update job source" alias. The edit dialog's read-only source field
// shows the alias instead (jobCurrentSource), the same location the job card
// shows; that is display only. Copy job clears the source entirely.
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

const retentionTierLabels: Record<string, () => string> = {
	hourly: () => t("ui.protect.retentionTier.hourly"),
	daily: () => t("ui.protect.retentionTier.daily"),
	weekly: () => t("ui.protect.retentionTier.weekly"),
	monthly: () => t("ui.protect.retentionTier.monthly"),
	yearly: () => t("ui.protect.retentionTier.yearly"),
};

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
			? t("ui.protect.keepAllNoTiers")
			: t("ui.protect.keepAllInactiveTiers", { tiers: tiers.map(([tier, value]) => `${retentionTierLabels[String(tier)]()} ${value}`).join(", ") });
	}
	return t("ui.protect.retentionSummary", { count: form.retention, tiers: ["hourly", "daily", "weekly", "monthly", "yearly"].map((tier, index) => `${retentionTierLabels[tier]()} ${[form.retentionHourly, form.retentionDaily, form.retentionWeekly, form.retentionMonthly, form.retentionYearly][index] || t("ui.protect.off")}`).join(", ") });
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
	// SQLite NOCASE folds only ASCII letters. Fold the same way so suggestions
	// match the database's uniqueness rule regardless of the browser locale.
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

// An rclone config password typed before the answer was changed to No is not
// sent; it only belongs to an encrypted rclone.conf.
function withheldRcloneConfigPassword(form: VaultForm, key: string) {
	return form.connector === RCLONE_REMOTE_CONNECTOR && key === "config_password" && form.options.config_encrypted !== "true";
}

function connectorOptionsForSubmission(form: VaultForm, integration: StorageIntegration | undefined) {
	const presentedKeys = new Set(visibleVaultOptions(integration?.options ?? [], form.connector).map((option) => option.key)
		.filter((key) => !withheldRcloneConfigPassword(form, key)));
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
	return (integration?.options ?? []).filter((option) => option.required
		? !String(options[option.key] ?? "").trim()
		// The rclone config password is needed only for an encrypted rclone.conf.
		: integration?.id === RCLONE_REMOTE_CONNECTOR && option.key === "config_password" &&
			options.config_encrypted === "true" && !options.config_password);
}

// The path in remote the way the backend saves it: trimmed, and without a
// trailing "/" unless the path is just "/".
function normalizedRcloneRemotePath(value = "") {
	return trimGoSpace(value) === "/" ? "/" : trimGoSpace(value).replace(/\/$/, "");
}

// The remote and the full path of an Any Rclone Remote vault, the way rclone
// writes it: <remote>:<path in remote>/Replicaro/<vault name>.
function rcloneRemoteVaultAddress(remote: string, path: string, location: string) {
	return trimGoSpace(remote) ? `${trimGoSpace(remote)}:${normalizedRcloneRemotePath(path).replace(/\/$/, "")}/${location}` : "";
}

function savedRcloneRemoteVaultAddress(repository: Pick<Repository, "connector" | "coldStorage" | "location" | "rcloneRemote">) {
	const settings = repository.rcloneRemote;
	return repository.connector === RCLONE_REMOTE_CONNECTOR && !repository.coldStorage && settings
		? rcloneRemoteVaultAddress(settings.remote, settings.path, repository.location) : "";
}

// The chip label as shown. An Any Rclone Remote address goes in a
// left-to-right <bdi>, so it keeps its order in a right-to-left language and
// copies without any added characters.
function repositoryConnectorLabelContent(repository: Pick<Repository, "connector" | "connectorLabel" | "location" | "isNetwork" | "coldStorage" | "rcloneRemote">) {
	const address = savedRcloneRemoteVaultAddress(repository);
	if (!address) return repositoryConnectorLabel(repository);
	return <>{knownMessage(`ui.integration.${RCLONE_REMOTE_CONNECTOR}.label`, "Any Rclone Remote")}: <bdi dir="ltr" className="rclone-remote-address">{address}</bdi></>;
}

function repositoryConnectorLabel(repository: Pick<Repository, "connector" | "connectorLabel" | "location" | "isNetwork" | "coldStorage" | "rcloneRemote">) {
	if (repository.coldStorage) return t("ui.protect.coldStorageConnectorLabel");
	if (repository.connector === RCLONE_REMOTE_CONNECTOR) {
		// Like "s3: <endpoint>": the storage type, then where the vault is.
		const storageType = knownMessage(`ui.integration.${RCLONE_REMOTE_CONNECTOR}.label`, "Any Rclone Remote");
		const address = savedRcloneRemoteVaultAddress(repository);
		return address ? `${storageType}: ${address}` : storageType;
	}
    if (repository.connector === "s3") return repository.connectorLabel || "s3";
	if (repository.connector === "webdav") {
		// The backend sends "webdav: <host>", like S3, with ":<port>" added for a
		// non-default port; the path is on the location line below. The card
		// data has no connector options, so this fallback only knows the host,
		// written without IPv6 brackets as the backend does when it shows no port.
		const address = canonicalWebDAVAddress(repository.location);
		return repository.connectorLabel || (address ? `webdav: ${address.host.replace(/^\[(.*)\]$/, "$1")}` : "webdav");
	}
    if (repository.connector !== "fs") return repository.connector;
    const location = repository.location.trim().replace(/\//g, "\\");
	return repository.isNetwork || location.startsWith("\\\\") ? t("ui.protect.networkConnectorLabel") : t("ui.protect.localConnectorLabel");
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
	// A WebDAV Location is already the full server URL the user entered (in its
	// saved spelling), so show it as is. The generic handling below would
	// prefix it with "/".
	if (repository.connector === "webdav") return location;
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

// Keep old/imported high-speed values usable while making provider changes
// synchronous with form state so a save cannot race a later correction.
function compatibleConcurrencyMode(connector: string, mode: VaultForm["concurrencyMode"]): VaultForm["concurrencyMode"] {
	return limitedSpeedProviders[connector] && (mode === "increased" || mode === "maximum") ? "native" : mode;
}

function compatibleVaultForm(form: VaultForm): VaultForm {
	const concurrencyMode = compatibleConcurrencyMode(form.connector, form.concurrencyMode);
	return concurrencyMode === form.concurrencyMode ? form : { ...form, concurrencyMode };
}

function IntegrationField({
    connector,
    option,
    value,
    onChange,
    explanation,
    disabled,
}: {
    connector: string;
    option: IntegrationOption;
    value: string;
    onChange: (value: string) => void;
    explanation?: string;
    disabled?: boolean;
}) {
    const label = knownMessage(`ui.integration.${connector}.option.${option.key}.label`, option.label);
    const help = option.help && (connector === "s3" && option.key === "storage_class"
        ? t("ui.integration.s3.option.storage_class.help", { glacier: "GLACIER", deepArchive: "DEEP_ARCHIVE" })
        // Catalogs can't contain URL text, so the schemes are placeholders.
        : connector === "webdav" && option.key === "port"
        ? renderMessage("ui.integration.webdav.option.port.help", ltrValues({ https: "https://", http: "http://" }))
        : knownMessage(`ui.integration.${connector}.option.${option.key}.help`, option.help));
    if (option.kind === "boolean") {
        return (
            <div className="advanced-setting">
                <label className="check">
                    <input type="checkbox" checked={value === "true"} disabled={disabled} onChange={(event) => onChange(String(event.target.checked))} />
                    {label}
                </label>
				{help && <small>{help}</small>}
                {explanation && <small>{explanation}</small>}
            </div>
        );
    }
    // Fields are required unless their label says "(optional)", so a required
    // option gets no marker.
    return (
		<label className="field">
			<span>{label}</span>
			{option.kind === "textarea" ? (
				<textarea rows={3} value={value} disabled={disabled} placeholder={examplePlaceholder(option.placeholder)} onChange={(event) => onChange(event.target.value)} />
			) : (
				<input type={option.secret ? "password" : "text"} value={value} disabled={disabled} placeholder={examplePlaceholder(option.placeholder)} onChange={(event) => onChange(event.target.value)} />
			)}
			{help && <small>{help}</small>}
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

// WebDAV addresses are stored as "<scheme>://<host><path>" with a non-default
// port in the "port" option. The backend (vaultidentity.NormalizeWebDAVAddress)
// saves one canonical spelling, and the helpers below follow its string rules
// so a typed address can be compared with a saved one. They deliberately
// avoid the browser URL parser: it turns IDN hosts into punycode and
// percent-encodes spaces and non-ASCII path characters, so a saved
// "https://bücher.example/Backups/my vault" would never equal what the user
// typed. This is a best-effort match, not a guarantee: the browser and Go can
// ship different Unicode tables, so a rare host or path may compare
// differently here. That only decides whether an unfinished connection is
// offered; the backend validates and canonicalizes every address it receives.
const webdavSchemes: Record<string, "https" | "http"> = {
	https: "https", http: "http",
	webdavs: "https", davs: "https",
	webdav: "http", dav: "http",
};

// Go's unicode.IsSpace set. JavaScript's trim() and \s differ from it (they
// include U+FEFF and leave out U+0085), and the backend trims with Go's set.
const goSpace = "\\t\\n\\v\\f\\r \\u0085\\u00a0\\u1680\\u2000-\\u200a\\u2028\\u2029\\u202f\\u205f\\u3000";
const goLeadingSpace = new RegExp(`^[${goSpace}]+`);
const goTrailingSpace = new RegExp(`[${goSpace}]+$`);

function trimGoSpace(value: string) {
	return value.replace(goLeadingSpace, "").replace(goTrailingSpace, "");
}

// Go's strings.ToLower maps one code point at a time. toLowerCase() on the
// whole string applies context rules (a final capital sigma becomes "ς") and
// turns "İ" into two code points, so lowercase each code point on its own.
function goLowerCase(value: string) {
	return Array.from(value, (character) => character === "\u0130" ? "i" : character.toLowerCase()).join("");
}

// The Server URL field holds only the scheme and host. A trailing slash is
// harmless there, so drop it before the Path is appended.
function webdavServerURL(value: string) {
	return trimGoSpace(value).replace(/\/+$/, "");
}

// The Server URL field takes only a known scheme and the host; the port and
// the folder have their own fields. A path typed here would otherwise be glued
// in front of the Path field and quietly name a different folder, so the form
// stops on a missing scheme, a path, or a port and points the user to the
// right fields. The backend still validates the whole URL.
//
// Returns "misplaced" for a value that needs the message, "incomplete" for one
// the user may still be typing (an empty field, the start of a known scheme,
// a scheme with no host, or an IPv6 address whose "]" hasn't been typed yet),
// and "ok" otherwise. Only "ok" is used to build a Location.
function webdavServerURLCheck(value: string): "ok" | "incomplete" | "misplaced" {
	const trimmed = trimGoSpace(value);
	const lower = trimmed.toLowerCase();
	if (!trimmed || Object.keys(webdavSchemes).some((scheme) => `${scheme}://`.startsWith(lower))) return "incomplete";
	if (!webdavScheme(trimmed)) return "misplaced";
	const host = webdavServerURL(trimmed.slice(trimmed.indexOf("://") + 3));
	if (!host) return "incomplete";
	if (host.includes("/")) return "misplaced";
	// An IPv6 address keeps its colons inside the brackets, so only what comes
	// after the closing bracket can hold a port.
	if (host.startsWith("[") && !host.includes("]")) return "incomplete";
	const outsideBrackets = host.startsWith("[") ? host.slice(host.indexOf("]") + 1) : host;
	// A port adds exactly one colon. More than one means an IPv6 address
	// without brackets, which the backend explains more precisely.
	return outsideBrackets.split(":").length === 2 ? "misplaced" : "ok";
}

// Returns "https", "http", or "" for an unknown or missing scheme. The dav
// aliases are sent as typed; the backend stores the plain scheme.
function webdavScheme(value: string) {
	const trimmed = trimGoSpace(value);
	const separator = trimmed.indexOf("://");
	const scheme = separator < 0 ? "" : trimmed.slice(0, separator).toLowerCase();
	// An own-property check, so "constructor://" or "__proto__://" can't pick
	// up a value from Object.prototype.
	return Object.hasOwn(webdavSchemes, scheme) ? webdavSchemes[scheme] : "";
}

function webdavUsesPlainHTTP(value: string) {
	return webdavScheme(value) === "http";
}

// Splits a stored or composed Location back into the Server URL and Path
// fields. The path is returned exactly as written.
function splitWebDAVLocation(location: string) {
	const trimmed = trimGoSpace(location);
	const separator = trimmed.indexOf("://");
	if (separator < 0) return null;
	const slash = trimmed.indexOf("/", separator + 3);
	return slash < 0 ? { serverURL: trimmed, path: "" } : { serverURL: trimmed.slice(0, slash), path: trimmed.slice(slash) };
}

// The canonical form the backend would store, with the effective port filled
// in: alias schemes mapped, scheme and host lowercased, and the trailing slash
// (with any whitespace it was hiding) dropped from the path until neither is
// left. Everything else in the path, including case, spaces, and Unicode form,
// is kept, because it names a real folder on the server.
function canonicalWebDAVAddress(location: string, port = "") {
	const split = splitWebDAVLocation(location);
	const scheme = split ? webdavScheme(split.serverURL) : "";
	if (!split || !scheme || !split.path) return null;
	const host = goLowerCase(trimGoSpace(split.serverURL.slice(split.serverURL.indexOf("://") + 3)));
	if (!host) return null;
	let path = split.path;
	while (path !== "/") {
		const trimmed = path.replace(/\/$/, "").replace(goTrailingSpace, "");
		if (trimmed === path) break;
		path = trimmed;
	}
	const rawPort = trimGoSpace(port);
	// strconv.Atoi accepts a leading sign and leading zeros.
	if (rawPort && !/^[+-]?\d+$/.test(rawPort)) return null;
	const portNumber = rawPort ? Number(rawPort) : scheme === "https" ? 443 : 80;
	if (!Number.isInteger(portNumber) || portNumber < 1 || portNumber > 65535) return null;
	return { scheme, host, path, port: String(portNumber) };
}

// URLs and paths keep their own left-to-right order inside a translated
// sentence. Without an isolate, a right-to-left language moves the slashes and
// colon of "https://" or "/dav/Backups/vault" to the wrong side of the text
// around them.
function ltrValues(values: Record<string, string>) {
	return Object.fromEntries(Object.entries(values).map(([name, value]) =>
		[name, <bdi key={name} dir="ltr">{value}</bdi>]));
}

function vaultLocation(form: VaultForm, useVaultNameForRclone = false) {
    if (form.pendingLocation) return form.pendingLocation;
	if (usesVaultFolderName(form.connector)) {
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
        case "webdav": {
            // The Location is the Server URL followed by the Path, as typed apart
            // from edge whitespace and the Server URL's trailing slash. The backend
            // validates and canonicalizes it (scheme aliases, host case, default
            // port), so this only has to put the two fields together safely.
            const serverURL = webdavServerURL(form.host);
            const path = trimGoSpace(form.prefix);
            if (webdavServerURLCheck(form.host) !== "ok" || !path.startsWith("/")) return "";
            return `${serverURL}${path}`;
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
	if (form.connector === "webdav") {
		// The saved attempt holds the backend's canonical Location and port, while
		// the form may still hold an alias scheme, an uppercase host, or an empty
		// default port. Compare both in the backend's canonical form. The scheme
		// stays part of the match: a retry reuses the saved address, so an https
		// entry must not resume an attempt saved for http on the same port.
		const saved = canonicalWebDAVAddress(intent.location, savedOptions.port);
		const current = canonicalWebDAVAddress(entered, form.options.port);
		if (!saved || !current) return false;
		return saved.scheme === current.scheme && saved.host === current.host &&
			saved.port === current.port && saved.path === current.path;
	}
	if (form.connector === RCLONE_REMOTE_CONNECTOR) {
		// The same vault folder name can sit on another remote or under another
		// path in remote, so those have to match too. The backend saves the path
		// without its trailing "/".
		if (trimGoSpace(savedOptions.remote ?? "") !== trimGoSpace(form.options.remote ?? "") || normalizedRcloneRemotePath(savedOptions.path) !== normalizedRcloneRemotePath(form.options.path)) return false;
	}
	if (intent.location === entered) return true;
	if (usesVaultFolderName(form.connector)) return intent.location === `Replicaro/${entered}`;
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
	if (usesVaultFolderName(form.connector)) {
		const canonical = form.location.trim();
		const prefix = "Replicaro/";
		if (!canonical.startsWith(prefix)) return form;
		const folder = canonical.slice(prefix.length);
		if (!folder || folder.includes("/") || folder.includes("\\")) return form;
		return { ...form, location: folder, pendingLocation: canonical };
	}
	if (form.connector === "webdav") {
		// Split by hand rather than with URL, which would rewrite an IDN host
		// and encode the path. The port already lives in the options.
		const split = splitWebDAVLocation(form.location);
		return split ? { ...form, host: split.serverURL, prefix: split.path, pendingLocation: form.location } : form;
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
	// Any Rclone Remote lays out all of its settings itself (RcloneRemoteFields).
	if (connector === RCLONE_REMOTE_CONNECTOR) return true;
	if (key === "root") return true;
	if (connector === "s3" && key === "endpoint") return true;
	// WebDAV's Port sits between its Server URL and Path inputs. The WebDAV
	// username and WebDAV account password render as ordinary catalog options.
	if (connector === "webdav") return key === "port";
	return connector === "sftp" && ["path_mode", "port", "username", "identity"].includes(key);
}

function connectionOptionIsCustom(connector: string, key: string) {
	return creationOptionIsCustom(connector, key);
}

function lowercaseEnglishLabel(label: string) {
	// Keep English's existing label style. Whole-label lowercasing changes nouns
	// and abbreviations in translated text, so preserve the catalog's casing.
	return getEffectiveLocale() === "en" ? label.toLowerCase() : label;
}

function advancedCreationOptionExplanation(connector: string, option: IntegrationOption) {
	const helpRemoved = connector === "s3" && ["use_tls", "tls_insecure_no_verify", "port", "storage_class"].includes(option.key) ||
		connector === "sftp" && option.key === "ssh_auth_sock" ||
		["azblob", "gcs"].includes(connector) && option.key === "endpoint";
	return helpRemoved ? undefined : t("ui.protect.optionalConnectorSettingHelp", { option: lowercaseEnglishLabel(knownMessage(`ui.integration.${connector}.option.${option.key}.label`, option.label)) });
}

function vaultEngineHelp(connector: string, coldStorage: boolean) {
	if (coldStorage) return t("ui.protect.coldStorageEngineHelp");
	if (connector === "dropbox") return t("ui.protect.dropboxEngineHelp");
	if (connector === "google_drive") return t("ui.protect.googleDriveEngineHelp");
	if (connector === "onedrive") return t("ui.protect.oneDriveEngineHelp");
	if (connector === RCLONE_REMOTE_CONNECTOR) return t("ui.protect.rcloneRemoteEngineHelp");
	return t("ui.protect.immutableEngineHelp");
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
	vaultFolderField = true,
}: {
    form: VaultForm;
    integration: StorageIntegration;
    onChange: (next: VaultForm) => void;
	// Create uses the vault name as the vault's folder name, so only Connect
	// asks for the folder name itself.
	vaultFolderField?: boolean;
}) {
    const option = (key: string) => integration.options.find((candidate) => candidate.key === key);
    const updateOption = (key: string, value: string) => onChange(updateConnectorOption(form, key, value, Boolean(option("region"))));
    const renderOption = (key: string, displayValue?: string) => {
        const selected = option(key);
		return selected ? <IntegrationField key={key} connector={integration.id} option={selected} value={displayValue ?? form.options[key] ?? ""} disabled={Boolean(form.pendingLocation) && !selected.credential} onChange={(value) => updateOption(key, value)} /> : null;
    };

	// Any Rclone Remote has its own sentences: its storage type name doesn't
	// fit where these sentences put a provider's name in every language.
	const provider = knownMessage(`ui.integration.${integration.id}.label`, integration.label);
	const rcloneRemote = integration.id === RCLONE_REMOTE_CONNECTOR;
	const vaultFolderLabel = rcloneRemote ? t("ui.protect.rcloneRemoteVaultFolder") : t("ui.protect.vaultFolderOnProvider", { provider });
	const vaultFolder = vaultFolderField && <label className="field"><span>{vaultFolderLabel}</span><input aria-label={vaultFolderLabel} value={form.location} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, location: event.target.value })} /><small>{rcloneRemote ? t("ui.protect.rcloneRemoteVaultFolderHelp") : t("ui.protect.vaultFolderExactHelp", { provider })}</small></label>;
	// The vault folder name comes after the remote settings, because it names
	// a folder under the chosen path in remote.
	if (rcloneRemote) return <><RcloneRemoteFields form={form} integration={integration} onChange={onChange} />{vaultFolder}</>;
	if (usesVaultFolderName(integration.id)) return vaultFolder || null;

    switch (integration.id) {
        case "s3":
            return <>
                {renderOption("endpoint")}
                <label className="field"><span>{t("ui.pages.protect.bucket")}</span><input value={form.bucket} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, bucket: event.target.value })} /></label>
				<label className="field"><span>{t("ui.pages.protect.path.prefix.optional")}</span><input value={form.prefix} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, prefix: event.target.value })} /></label>
            </>;
        case "azblob":
            return <>
                <label className="field"><span>{t("ui.pages.protect.container")}</span><input value={form.container} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, container: event.target.value })} /></label>
				<label className="field"><span>{t("ui.pages.protect.path.prefix.optional")}</span><input value={form.prefix} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, prefix: event.target.value })} /></label>
            </>;
        case "gcs":
            return <>
                <label className="field"><span>{t("ui.pages.protect.bucket")}</span><input value={form.bucket} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, bucket: event.target.value })} /></label>
                <label className="field"><span>{t("ui.pages.protect.path.prefix.optional")}</span><input value={form.prefix} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, prefix: event.target.value })} /></label>
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
                <label className="field"><span>{t("ui.pages.protect.host")}</span><input value={form.host} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, host: event.target.value })} /></label>
                {renderOption("port", pendingURL?.port || undefined)}
                {renderOption("username", pendingURL?.username || undefined)}
				{supportsPathMode && <label className="field"><span>{t("ui.pages.protect.vault.location.on.server")}</span><select value={pathMode} disabled={Boolean(form.pendingLocation)} onChange={(event) => updateOption("path_mode", event.target.value)}><option value="home">{t("ui.pages.protect.sftp.home.directory.recommended")}</option><option value="absolute">{t("ui.pages.protect.server.filesystem.root")}</option></select></label>}
				<label className="field"><span>{absolutePath ? t("ui.pages.protect.vault.path.relative.to.server.filesystem.root") : t("ui.pages.protect.vault.path.relative.to.sftp.home")}</span><input aria-label={absolutePath ? t("ui.protect.vaultPathServerRoot") : t("ui.protect.vaultPathSftpHome")} value={form.prefix} placeholder={examplePlaceholder(absolutePath ? "/srv/backups/vault" : "backups/vault")} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, prefix: event.target.value })} /><small>{absolutePath ? t("ui.pages.protect.start.with.one.the.path.begins.at.the.server") : t("ui.pages.protect.do.not.start.with.the.path.begins.in.the")}</small></label>
                {renderOption("identity")}
            </>;
        }
        case "webdav": {
            // Server URL, Port, and Path are separate inputs, like SFTP's host and
            // path, and vaultLocation joins them. WebDAV has no home directory, so
            // unlike SFTP there is no home/root choice: every path starts at the
            // server root, and the examples show where common servers put a
            // user's files. Neither engine creates a missing folder here. Both
            // inputs are laid out left to right even in a right-to-left page;
            // otherwise the Path's leading "/" is drawn at the far end.
            const serverURLMisplaced = webdavServerURLCheck(form.host) === "misplaced";
            return <>
                <div className="webdav-server-url">
                    <label className="field"><span>{t("ui.protect.webdavServerURL")}</span><input aria-label={t("ui.protect.webdavServerURL")} dir="ltr" value={form.host} placeholder={examplePlaceholder("https://cloud.example.com")} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, host: event.target.value })} /><small>{renderMessage("ui.protect.webdavServerURLHelp", ltrValues({ https: "https://", http: "http://", example: "https://cloud.example.com" }))}</small>{serverURLMisplaced && <small className="inline-error" role="alert">{renderMessage("ui.protect.webdavServerURLHasPath", ltrValues({ https: "https://", http: "http://", example: "https://cloud.example.com" }))}</small>}</label>
                    {/* Plain HTTP is allowed for trusted networks, but the WebDAV
                        credentials then cross the network in the clear, so say so
                        whenever the scheme resolves to http (webdav:// and dav://
                        included). The status region stays mounted and only its
                        text comes and goes: several screen readers don't announce
                        a live region that is inserted together with its text. It
                        shares a wrapper with the Server URL so that, while empty,
                        it adds no gap to the form. */}
                    <div className="webdav-plain-http-status" role="status">{webdavUsesPlainHTTP(form.host) && <div className="recovery-warning webdav-plain-http-warning"><p>{renderMessage("ui.protect.webdavPlainHTTPWarning", ltrValues({ https: "https://" }))}</p></div>}</div>
                </div>
                {renderOption("port")}
                <label className="field"><span>{t("ui.protect.webdavPath")}</span><input aria-label={t("ui.protect.webdavPath")} dir="ltr" value={form.prefix} placeholder={examplePlaceholder("/dav/Backups/vault")} disabled={Boolean(form.pendingLocation)} onChange={(event) => onChange({ ...form, prefix: event.target.value })} /><small>{t("ui.protect.webdavPathHelp")}</small><small className="webdav-path-examples">{renderMessage("ui.protect.webdavPathExamples", ltrValues({ nextcloud: "/remote.php/dav/files/<username>/Backups/vault", owncloud: "/remote.php/webdav/Backups/vault", share: "/dav/Backups/vault" }))}</small></label>
            </>;
        }
        default:
            return null;
    }
}

// Shown at the top of the create and connect screens whenever Any Rclone
// Remote is the selected storage type.
function RcloneRemoteWarning() {
	return <div className="recovery-warning recovery-fallback-message rclone-remote-warning">
		<p>{t("ui.protect.rcloneRemoteWarningAdvanced")}</p>
		<p>{t("ui.protect.rcloneRemoteWarningSupport")}</p>
		<p>{t("ui.protect.rcloneRemoteWarningCredentials")}</p>
	</div>;
}

// Listing the remotes runs rclone against the user's rclone.conf and writes a
// short-lived check file next to it, so the list is read only once the typing
// has paused.
const RCLONE_REMOTE_LIST_DELAY_MS = 800;

type RcloneRemoteList =
	| { request: string; status: "loading" }
	| { request: string; status: "loaded"; remotes: RcloneRemoteEntry[] }
	| { request: string; status: "failed"; error: string };

// The Any Rclone Remote settings, in this order: the rclone config file, the
// environment variables, whether rclone.conf is encrypted (and then the rclone
// config password), the remote picked from the file, and the path in remote.
// No sign-in is involved; everything rclone needs is in the user's file or in
// these variables.
function RcloneRemoteFields({
	form,
	integration,
	onChange,
}: {
	form: VaultForm;
	integration: StorageIntegration;
	onChange: (next: VaultForm) => void;
}) {
	const label = (key: string) => knownMessage(`ui.integration.${RCLONE_REMOTE_CONNECTOR}.option.${key}.label`,
		integration.options.find((option) => option.key === key)?.label ?? key);
	const options = form.options;
	const update = (key: string, value: string) => onChange(updateConnectorOption(form, key, value));
	// A pending creation or connection keeps its saved remote settings; only
	// the rclone config password and the environment variables are entered
	// again, as for other storage types' credentials.
	const locked = Boolean(form.pendingLocation);
	const encrypted = options.config_encrypted === "true";
	const variables = parseRcloneRemoteVariables(options.environment);
	const updateVariables = (rows: typeof variables) => update("environment", serializeRcloneRemoteVariables(rows));
	const remote = options.remote ?? "";

	// Only the settings that decide which remotes rclone sees go into the
	// list request.
	const listOptions: Record<string, string> = {
		config_file: options.config_file ?? "",
		config_encrypted: encrypted ? "true" : "false",
		...(encrypted ? { config_password: options.config_password ?? "" } : {}),
		environment: options.environment ?? "",
	};
	const listReady = !locked && Boolean(listOptions.config_file.trim()) && (!encrypted || Boolean(listOptions.config_password));
	const [refreshCount, setRefreshCount] = useState(0);
	const listRequest = listReady ? JSON.stringify([listOptions, refreshCount]) : "";
	const [list, setList] = useState<RcloneRemoteList | null>(null);
	const immediateRefresh = useRef(false);
	useEffect(() => {
		if (!listRequest) return;
		const controller = new AbortController();
		const [requestOptions] = JSON.parse(listRequest) as [Record<string, string>, number];
		const delay = immediateRefresh.current ? 0 : RCLONE_REMOTE_LIST_DELAY_MS;
		immediateRefresh.current = false;
		const timer = window.setTimeout(() => {
			setList({ request: listRequest, status: "loading" });
			listRcloneRemotes(requestOptions, controller.signal)
				.then(({ remotes }) => { if (!controller.signal.aborted) setList({ request: listRequest, status: "loaded", remotes: remotes ?? [] }); })
				.catch((error: Error) => {
					if (!controller.signal.aborted && error.name !== "AbortError") setList({ request: listRequest, status: "failed", error: error.message });
				});
		}, delay);
		return () => {
			window.clearTimeout(timer);
			controller.abort();
		};
	}, [listRequest]);
	// A result for settings that have since changed is not shown.
	const current = list && list.request === listRequest ? list : null;
	const remotes = current?.status === "loaded" ? current.remotes : [];
	const listedNames = remotes.map((entry) => entry.name);
	const placeholder = !listReady ? t("ui.protect.rcloneRemoteListWaiting")
		: !current || current.status === "loading" ? t("ui.protect.rcloneRemoteListLoading")
		: current.status === "loaded" && remotes.length === 0 ? t("ui.protect.rcloneRemoteListEmpty")
		: t("ui.protect.rcloneRemoteChooseRemote");
	const pathExampleRemote = remote || "remote";

	return <>
		<label className="field">
			<span>{label("config_file")}</span>
			<input aria-label={label("config_file")} dir="ltr" value={options.config_file ?? ""} disabled={locked} spellCheck={false} onChange={(event) => update("config_file", event.target.value)} />
			<small>{t("ui.integration.rclone_remote.option.config_file.help")}</small>
		</label>
		<div className="field rclone-remote-variables" role="group" aria-label={label("environment")}>
			<span>{label("environment")}</span>
			{/* Every row has the same visible labels, so each control's
			    accessible name adds the row number to tell the rows apart. */}
			{variables.map((variable, index) => (
				<div className="rclone-remote-variable" key={index}>
					<label className="field">
						<span>{t("ui.protect.rcloneRemoteVariableName")}</span>
						<input aria-label={t("ui.protect.rcloneRemoteVariableNameRow", { number: index + 1 })} dir="ltr" value={variable.name} spellCheck={false} autoComplete="off" onChange={(event) => updateVariables(variables.map((row, rowIndex) => rowIndex === index ? { ...row, name: event.target.value } : row))} />
					</label>
					<label className="field">
						<span>{t("ui.protect.rcloneRemoteVariableValue")}</span>
						{/* Values are often keys or tokens, so they are masked like any
						    other secret setting. */}
						<input aria-label={t("ui.protect.rcloneRemoteVariableValueRow", { number: index + 1 })} type="password" dir="ltr" value={variable.value} autoComplete="new-password" onChange={(event) => updateVariables(variables.map((row, rowIndex) => rowIndex === index ? { ...row, value: event.target.value } : row))} />
					</label>
					<button type="button" className="btn sm" aria-label={t("ui.protect.rcloneRemoteRemoveVariableRow", { number: index + 1 })} onClick={() => updateVariables(variables.filter((_, rowIndex) => rowIndex !== index))}>{t("ui.protect.rcloneRemoteRemoveVariable")}</button>
				</div>
			))}
			<button type="button" className="btn sm rclone-remote-add-variable" onClick={() => updateVariables([...variables, { name: "", value: "" }])}><Icon name="plus" size={14} />{t("ui.protect.rcloneRemoteAddVariable")}</button>
			<small>{renderMessage("ui.integration.rclone_remote.option.environment.help", ltrValues({ example: "AWS_ACCESS_KEY_ID", envAuth: "env_auth" }))}</small>
		</div>
		<label className="field">
			<span>{label("config_encrypted")}</span>
			<select aria-label={label("config_encrypted")} value={encrypted ? "true" : "false"} disabled={locked} onChange={(event) => update("config_encrypted", event.target.value)}>
				<option value="true">{t("ui.protect.answerYes")}</option>
				<option value="false">{t("ui.protect.answerNo")}</option>
			</select>
		</label>
		{encrypted && <label className="field">
			<span>{label("config_password")}</span>
			<input aria-label={label("config_password")} type="password" value={options.config_password ?? ""} autoComplete="off" onChange={(event) => update("config_password", event.target.value)} />
		</label>}
		<div className="field rclone-remote-picker">
			<label className="field">
				<span>{label("remote")}</span>
				{/* The select follows the page direction, so the translated
				    placeholder sentences read correctly. An option can only hold
				    text, so each remote name is isolated left to right instead. */}
				<select aria-label={label("remote")} value={remote} disabled={locked || current?.status !== "loaded"} onChange={(event) => update("remote", event.target.value)}>
					<option value="">{placeholder}</option>
					{/* A saved remote stays selectable while the list loads, and
					    after it, even when rclone no longer lists it. */}
					{remote && !listedNames.includes(remote) && <option value={remote}>{ltrIsolate(remote)}</option>}
					{remotes.map((entry) => <option key={entry.name} value={entry.name}>{ltrIsolate(entry.type ? `${entry.name} (${entry.type})` : entry.name)}</option>)}
				</select>
			</label>
			{current?.status === "failed" && <small className="inline-error rclone-remote-list-error" role="alert">{current.error}</small>}
			{listReady && current && current.status !== "loading" && <button type="button" className="btn sm" onClick={() => { immediateRefresh.current = true; setRefreshCount((count) => count + 1); }}>{t("ui.protect.rcloneRemoteListRefresh")}</button>}
		</div>
		<label className="field">
			<span>{label("path")}</span>
			<input aria-label={label("path")} dir="ltr" value={options.path ?? ""} disabled={locked} spellCheck={false} onChange={(event) => update("path", event.target.value)} />
			<small>{renderMessage("ui.integration.rclone_remote.option.path.help", ltrValues({
				bucketExample: `${pathExampleRemote}:<bucket>/`,
				pathExample: `${pathExampleRemote}:<path>/`,
			}))}</small>
		</label>
	</>;
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
				<span>{t("ui.pages.protect.integrity.check")}</span>
				<select disabled={disabled || integrityDisabled || form.coldStorage} value={form.coldStorage ? "manual" : form.checkSchedule} onChange={(event) => onChange({ ...form, checkSchedule: event.target.value })}>{(form.coldStorage ? [["manual", () => t("ui.care.disabled")]] as const : careSchedules).map(([value, label]) => <option key={value} value={value}>{label()}</option>)}</select>
				<small>{form.coldStorage ? coldStorageIntegrityHelp() : integrityCheckHelp()}</small>
				{integrityDisabled && integrityLockedHelp && <small>{integrityLockedHelp}</small>}
			</label>
			<label className="field">
				<span>{t("ui.pages.protect.space.reclamation")}</span>
				<select disabled={disabled || maintenanceDisabled} value={form.maintenanceSchedule} onChange={(event) => onChange({ ...form, maintenanceSchedule: event.target.value })}>{careSchedules.map(([value, label]) => <option key={value} value={value} disabled={!objectLockScheduleEligible(form.objectLock, value)}>{label()}</option>)}</select>
				<small>{form.objectLock.enrolled ? (form.objectLock.paused ? pausedObjectLockMaintenanceHelp() : objectLockMaintenanceHelp()) : maintenanceHelp()}</small>
				{maintenanceDisabled && maintenanceLockedHelp && <small>{maintenanceLockedHelp}</small>}
			</label>
			<JobSpeedField connector={form.connector} value={form.concurrencyMode} disabled={disabled} onChange={(concurrencyMode) => onChange({ ...form, concurrencyMode })} />
		</div>
	);
}

function JobSpeedField({
	connector,
	value,
	disabled = false,
	onChange,
}: {
	connector: string;
	value: VaultForm["concurrencyMode"];
	disabled?: boolean;
	onChange: (mode: VaultForm["concurrencyMode"]) => void;
}) {
	const provider = limitedSpeedProviders[connector];
	return (
		<label className="field vault-job-speed-field">
			<span>{t("ui.pages.protect.job.speed")}</span>
			<select disabled={disabled} value={compatibleConcurrencyMode(connector, value)} onChange={(event) => onChange(compatibleConcurrencyMode(connector, event.target.value as VaultForm["concurrencyMode"]))}>
				<option value="reduced">{t("ui.pages.protect.slower")}</option>
				<option value="native">{t("ui.pages.protect.normal")}</option>
				<option value="increased" disabled={Boolean(provider)}>{t("ui.pages.protect.faster")}</option>
				<option value="maximum" disabled={Boolean(provider)}>{t("ui.pages.protect.maximum")}</option>
			</select>
			<small>{t("ui.pages.protect.controls.how.quickly.replicaro.finishes.backup.restore.integrity.check")}</small>
			{provider && <small>{connector === RCLONE_REMOTE_CONNECTOR
				? t("ui.protect.rcloneRemoteSpeedLimitHelp")
				: t("ui.protect.providerSpeedLimitHelp", { provider })}</small>}
		</label>
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
			? <label className="check"><input type="checkbox" checked={form.objectLock.enrolled} disabled={enrollmentLocked} onChange={(event) => updateSettings({ ...form.objectLock, enrolled: event.target.checked, paused: false })} />{t("ui.pages.protect.enable.object.lock")}</label>
			: <div className="object-lock-enrollment-status">{t("ui.protect.objectLockStatusLabel")} <strong className={form.objectLock.enrolled ? "enabled" : "disabled"}>{form.objectLock.enrolled ? t("ui.protect.enabled") : t("ui.protect.disabled")}</strong></div>}
		<small className="object-lock-help">{creation && eligible ? t("ui.protect.objectLockProtectionHelp") : t("ui.protect.objectLockImmutableHelp")}</small>
		{form.objectLock.enrolled && <>
			{!creation && <label className="field"><span>{t("ui.pages.protect.object.lock.activity")}</span><select value={form.objectLock.paused ? "paused" : "active"} disabled={disabled} onChange={(event) => updateSettings({ ...form.objectLock, paused: event.target.value === "paused" })}><option value="active">{t("ui.pages.protect.active")}</option><option value="paused">{t("ui.pages.protect.paused")}</option></select></label>}
			<div className="form-grid two object-lock-settings-grid">
				<label className="field"><span>{t("ui.pages.protect.mode")}</span><select value={form.objectLock.mode || "compliance"} disabled={settingsDisabled} onChange={(event) => updateSettings({ ...form.objectLock, mode: event.target.value as ObjectLockSettings["mode"] })}><option value="compliance">{t("ui.pages.protect.compliance")}{form.connector === "s3" ? t("ui.pages.protect.default") : ""}</option>{form.connector === "s3" && (creation || originalObjectLock?.mode === "governance") && <option value="governance">{t("ui.pages.protect.governance")}</option>}</select></label>
				<label className="field"><span>{t("ui.pages.protect.duration")}</span><div className="form-grid two"><input type="number" min={form.objectLock.durationUnit === "days" ? 2 : 1} step={1} value={form.objectLock.durationValue} disabled={settingsDisabled} onWheel={preventNumberInputWheel} onChange={(event) => updateSettings({ ...form.objectLock, durationValue: Math.max(0, Math.trunc(Number(event.target.value))) })} /><select aria-label={t("ui.pages.protect.object.lock.duration.unit")} value={form.objectLock.durationUnit || "days"} disabled={settingsDisabled} onChange={(event) => updateSettings({ ...form.objectLock, durationUnit: event.target.value as ObjectLockSettings["durationUnit"] })}><option value="days">{t("ui.pages.protect.days")}</option><option value="weeks">{t("ui.pages.protect.weeks")}</option><option value="months">{t("ui.pages.protect.months")}</option><option value="years">{t("ui.pages.protect.years")}</option></select></div></label>
			</div>
			{form.objectLock.paused && <small>{t("ui.pages.protect.resume.object.lock.to.change.its.mode.or.duration")}</small>}
			{!creation && <small>{objectLockTransitionHelp()}</small>}
			<small className="object-lock-help">{objectLockForwardHelp()}</small>
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
                    <span>{selected.length ? t("ui.protect.selectedCount", { count: selected.length }) : t("ui.pages.protect.no.vaults.selected")}</span>
                    {selected.length > 0 && <button type="button" className="text-button" onClick={() => onChange([])}>{t("ui.pages.protect.clear.all")}</button>}
                </div>
                {selected.length > 0 && (
                    <div className="destination-chips">
                        {selected.map((repository) => (
                            <Tooltip key={repository.id} content={t("ui.protect.removeNamedVault", { name: repository.name })}>
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
                placeholder={t("ui.pages.protect.search.vaults.by.name.or.location")}
                aria-label={t("ui.pages.protect.search.destination.vaults")}
                onChange={(event) => setQuery(event.target.value)}
            />
            <div className="destination-results" role="listbox" aria-label={t("ui.pages.protect.available.destination.vaults")}>
                {available.map((repository) => (
                    <button
                        type="button"
                        className="destination-result"
                        key={repository.id}
                        onClick={() => onChange([...selectedIds, repository.id])}
                    >
                        <span><strong>{repository.name}</strong><small className="mono">{repository.location}</small></span>
                        <span className="destination-add"><Icon name="plus" size={12} /> {t("ui.pages.protect.add")}</span>
                    </button>
                ))}
                {available.length === 0 && (
                    <div className="destination-results-empty">
                        {normalizedQuery ? t("ui.pages.protect.no.matching.vaults") : t("ui.pages.protect.all.available.vaults.are.selected")}
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
    return `${formatDisplayNumber(value, { minimumFractionDigits: value >= 10 || unit === 0 ? 0 : 1, maximumFractionDigits: value >= 10 || unit === 0 ? 0 : 1 })} ${units[unit]}`;
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
	const display = `${formatDisplayNumber(decimalValue, { minimumFractionDigits: decimalValue >= 10 || unit === 0 ? 0 : 1, maximumFractionDigits: decimalValue >= 10 || unit === 0 ? 0 : 1 })} ${decimalUnits[unit]}`;
	if (unit === 0) {
		return { display, tooltip: t("ui.protect.bytesUnitTooltip", { display }) };
	}
	const binaryValue = bytes / (1024 ** unit);
	const binary = `${formatDisplayNumber(binaryValue, { minimumFractionDigits: binaryValue >= 10 ? 0 : 1, maximumFractionDigits: binaryValue >= 10 ? 0 : 1 })} ${binaryUnits[unit]}`;
	return {
		display,
		tooltip: t("ui.protect.sizeUnitTooltip", { binary, decimalUnit: decimalUnits[unit], binaryUnit: binaryUnits[unit] }),
	};
}

function displayPath(value: string) {
	if (!/^\/?[A-Za-z]:[\\/]/.test(value)) return value;
	return value.replace(/^\/+/, "").replace(/\//g, "\\");
}

export default function Protect() {
    const toast = useToast();
	const navigate = useNavigate();
    const [savedJobs, setJobs] = useState<BackupJob[] | null>(null);
	// Jobs whose deletion is queued or running are hidden everywhere on this
	// page. The set is rebuilt from the backend's active operations, so a
	// reload or another browser hides the same jobs; a job whose deletion this
	// page just requested is added at once and dropped again if the request is
	// refused.
	const [deletingJobIDs, setDeletingJobIDs] = useState<Set<string>>(() => new Set());
	const jobs = useMemo(() => savedJobs === null ? null : savedJobs.filter((job) => !deletingJobIDs.has(job.id)), [savedJobs, deletingJobIDs]);
    const [repos, setRepos] = useState<Repository[] | null>(null);
	const [vaultMutations, setVaultMutations] = useState<Record<string, VaultMutation>>(vaultMutationSnapshot);
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
	const [sourceUpdateJob, setSourceUpdateJob] = useState<BackupJob | null>(null);
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
	const setVaultForm = (next: VaultForm) => setVaultFormState(compatibleVaultForm(next));
	const [vaultCreateError, setVaultCreateError] = useState("");
	const [connectForm, setConnectFormState] = useState<VaultForm>(emptyVault());
	const setConnectForm = (next: VaultForm | ((current: VaultForm) => VaultForm)) => {
		setConnectFormState((current) => compatibleVaultForm(typeof next === "function" ? next(current) : next));
	};
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
	// For Any Rclone Remote, <remote>:<path in remote>/Replicaro/<vault name> as
	// checked, because the location alone doesn't show a changed remote or path.
	const [connectReviewedRcloneAddress, setConnectReviewedRcloneAddress] = useState("");
	const [connectDetectedEngine, setConnectDetectedEngine] = useState<"restic" | "kopia" | "">("");
	const [connectNameConflictNotice, setConnectNameConflictNotice] = useState("");
	const [connectRcloneNameConflict, setConnectRcloneNameConflict] = useState(false);
	const [connectChecking, setConnectChecking] = useState(false);
	const [connectSaving, setConnectSaving] = useState(false);
	const [vaultProgress, setVaultProgress] = useState<VaultProgressRecord[]>([]);
	// A small bounded buffer of stage records lets users see real progress. It
	// is display only: nothing is made up or persisted, and the request's own
	// result decides whether creation/connection worked.
	// Only stage records go here. Native records stay in the API/native result
	// capture; adding them could push every stage out of the buffer during a
	// chatty engine operation.
	const appendVaultProgress = (record: VaultProgressRecord) => {
		if (record.type === "stage") setVaultProgress((current) => [...current.slice(-199), record]);
	};
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
	const [vaultOneAtATimeChoice, setVaultOneAtATimeChoice] = useState<{ kind: "reconnect" | "password"; repository: Repository } | null>(null);
	const rcloneAuthSessionsOnUnmount = useRef<string[]>([]);
	const [dormantJobs, setDormantJobs] = useState<DormantRecoveryJob[]>([]);
	const [dormantBusy, setDormantBusy] = useState("");
	const [vaultInitialSchedules, setVaultInitialSchedules] = useState<{ checkSchedule: string; maintenanceSchedule: string; concurrencyMode: VaultForm["concurrencyMode"]; autoUnlock: boolean; objectLock: ObjectLockSettings } | null>(null);
    const [checkSchedule, setCheckSchedule] = useState("manual");
    const [maintenanceSchedule, setMaintenanceSchedule] = useState("weekly");
	const [concurrencyMode, setConcurrencyMode] = useState<VaultForm["concurrencyMode"]>("native");
	const [autoUnlock, setAutoUnlock] = useState(true);
	const [objectLock, setObjectLock] = useState<ObjectLockSettings>(emptyObjectLock());
    const [toolBusy, setToolBusy] = useState("");
	const [vaultDelete, setVaultDelete] = useState<Repository | null>(null);
	const [vaultRemoval, setVaultRemoval] = useState<Record<string, VaultRemovalPresentation>>(vaultRemovalSnapshot);
	const [closePrompt, setClosePrompt] = useState<"job" | "vault" | null>(null);
	// The vault tool ("check" or "maintenance") waiting on the unsaved-changes
	// prompt, if the prompt was opened by Run check now / Run reclamation now
	// rather than by closing the dialog.
	const pendingVaultTool = useRef<"check" | "maintenance" | null>(null);
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
	const protectPageActive = useRef(false);
	// Backups this page started keep their job shown as running until the
	// status poll lists them. The vault card's Reconnect state is not derived
	// here: it is the vault's saved reconnectRequired flag from the server.
	const observedBackupOperations = useRef<Record<string, ObservedBackupOperation>>({});
	const runReviewSession = useRef(0);
	const runSubmissionGeneration = useRef(0);
	const runSubmissionOwner = useRef<RunSubmissionOwner | null>(null);
	const refreshGenerations = useRef({ jobs: 0, repositories: 0, profileSync: 0, running: 0, creations: 0 });
	const runningState = useRef<Record<string, boolean>>({});
	const handledVaultMutationCompletions = useRef(new Set<string>());
	const handledVaultMutationReloads = useRef(new Set<string>());

	const commitRunning = useCallback((next: Record<string, boolean>) => {
		runningState.current = next;
		setRunning(next);
	}, []);

	useEffect(() => subscribeVaultMutations(setVaultMutations), []);
	useEffect(() => subscribeVaultRemovals(setVaultRemoval), []);

	useEffect(() => {
		protectPageActive.current = true;
		return () => {
			protectPageActive.current = false;
			// Late dialog reads may not mutate module-owned retry presentation after
			// their initiating Protect page is gone.
		};
	}, []);

	useEffect(() => {
		// Status reads are presentation observations, unlike the module-owned
		// vault change presentations that follow their background operations.
		// Crossing a page lifetime invalidates only those reads so an old
		// Protect instance cannot resurrect stale recovery state.
		vaultPasswordStatusLifecycle++;
		return () => { vaultPasswordStatusLifecycle++; };
	}, []);

	const commitActiveTargets = useCallback((activeTargets: ActiveBackupTarget[]) => {
		const activeOperationIDs = new Set(activeTargets.map((target) => target.operationId).filter(Boolean));
		for (const operationId of Object.keys(observedBackupOperations.current)) {
			if (!activeOperationIDs.has(operationId)) delete observedBackupOperations.current[operationId];
		}
		for (const target of activeTargets) {
			if (!target.operationId) continue;
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
		setConnectReviewedRcloneAddress("");
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
		observedBackupOperations.current = {};
		runReviewSession.current++;
		runSubmissionGeneration.current++;
		runSubmissionOwner.current = null;
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
			forgetSettledVaultRemovalsNotIn(nextRepos);
			setVaultStats((current) => Object.fromEntries(nextRepos.map((repository) => {
				const active = current[repository.id];
				return [repository.id, active?.running || active?.pending ? active : {
					vaultSizeBytes: repository.vaultSizeBytes,
					vaultSizeMeasuredAt: repository.vaultSizeMeasuredAt,
					vaultSizeDirty: repository.vaultSizeDirty,
					fresh: false, running: false, pending: false, paused: false,
				}];
			})));
			// Password recovery is already durable in the backend. Rebuild only its
			// unfinished card presentation for this authoritative vault list; an
			// ordinary 404 means there is no operation and is intentionally silent.
			for (const repository of nextRepos) {
				const observation = beginVaultPasswordStatusObservation(repository.id);
				void getVaultPasswordChangeStatus(repository.id).then((status) => {
					if (refreshGenerations.current.repositories !== request.repositories || !ownsVaultPasswordStatusObservation(observation)) return;
					recordObservedVaultPasswordMutation(repository.id, repository.name, status);
				}).catch((error: Error) => {
					if (refreshGenerations.current.repositories !== request.repositories || !ownsVaultPasswordStatusObservation(observation)) return;
					if (error instanceof APIError && error.status === 404) recordMissingVaultPasswordMutation(repository.id);
					else toast("error", error.message);
				});
			}
		}).catch((error: Error) => {
			if (refreshGenerations.current.repositories === request.repositories) toast("error", error.message);
		});
	}, [commitJobs, commitRunning, toast]);

	// A removal started on an earlier mount of this page settles here too.
	useEffect(() => subscribeVaultRemovalSettled(load), [load]);

	// Retry returns as soon as the update is queued. The card keeps showing the
	// pending update from the sync status list until the queue finishes it, so
	// this toast must not claim the profile was synchronized.
	const retryProfile = async (repositoryId: string) => {
		try {
			await retryVaultProfileSync(repositoryId);
			toast("info", t("ui.protect.profileUpdateQueued"));
			load();
		} catch (error) {
			toast("error", (error as Error).message);
		}
	};

	// Jobs this page asked to delete, from the click until the deletion's
	// operation finishes. They stay hidden whatever an active-operations refresh
	// says in between: a refresh that was sent before the backend queued the
	// deletion can land after the 202, and would otherwise show the job again
	// until the next refresh. The entry is dropped when the followed operation
	// finishes (in followJobDeletion), not when the 202 arrives, for that reason.
	const pendingJobDeletions = useRef(new Set<string>());

	// followJobDeletion reports a queued job deletion once it finishes. It is
	// deduplicated by operation, so a job found deleting on a reload is
	// announced once.
	const followJobDeletion = useCallback((operationId: string, jobName: string) => {
		followTrackedOperation(operationId, async (operation) => {
			// Before anything awaits: the finished listeners run right after this
			// and recompute the hidden jobs from the active operations.
			pendingJobDeletions.current.delete(operation.jobId);
			if (operation.status === "success" || operation.status === "completed_with_issues") {
				toast("ok", t("ui.protect.jobDeleted", { name: jobName }));
			} else {
				toast("error", await operationFailureReason(operation));
			}
		});
	}, [toast]);

	// The latest vault list, for naming a vault change found running on the
	// backend.
	const reposForNames = useRef<Repository[] | null>(null);
	useEffect(() => { reposForNames.current = repos; }, [repos]);

	// refreshActiveOperations reads the backend's active operations and derives
	// from them what this page shows for work that runs in the background: the
	// jobs hidden while their deletion is active, and the vault cards showing a
	// password change, settings save or removal in progress.
	const refreshActiveOperations = useCallback(async () => {
		try {
			const operations = await getActiveOperations();
			const active = activeJobDeletions(operations);
			setDeletingJobIDs(new Set([...active.keys(), ...pendingJobDeletions.current]));
			for (const operation of operations) {
				if (operation.kind === JOB_DELETION_KIND && active.get(operation.jobId) === operation.id) {
					followJobDeletion(operation.id, operation.title.replace(/^Delete job: /, ""));
				}
			}
			adoptActiveVaultChanges(operations, (repositoryId) =>
				reposForNames.current?.find((repository) => repository.id === repositoryId)?.name ?? "", toast);
		} catch {
			// Keep what is shown now; the next refresh or finished operation corrects it.
		}
	}, [followJobDeletion, toast]);

	useEffect(() => {
		void refreshActiveOperations();
		return onTrackedOperationFinished((operation) => {
			if (operation.kind !== JOB_DELETION_KIND) return;
			// Drop a deleted job at once so it does not flash back between the
			// active-operation refresh and the job list reload.
			if (operation.status === "success" || operation.status === "completed_with_issues") {
				setJobs((current) => current?.filter((job) => job.id !== operation.jobId) ?? current);
			}
			void refreshActiveOperations();
			load();
		});
	}, [load, refreshActiveOperations]);

	// Every inventory load also picks up vault changes started elsewhere (another
	// browser, or this one before a reload).
	useEffect(() => {
		if (repos) void refreshActiveOperations();
	}, [repos, refreshActiveOperations]);

	// Once per page, bring back the "Removal stopped" card of a vault whose
	// newest removal could not update its recovery profile.
	const stoppedRemovalsShown = useRef(false);
	useEffect(() => {
		if (!repos || stoppedRemovalsShown.current) return;
		stoppedRemovalsShown.current = true;
		void getOperations().then((operations) => {
			if (protectPageActive.current) showStoppedVaultRemovals(operations, reposForNames.current ?? repos);
		}).catch(() => undefined);
	}, [repos]);

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
				setConnectFormState(emptyVault(catalog.integrations.find((item) => item.id === "fs"), settings.defaultEngine));
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
	const createUsesRcloneSignIn = usesRcloneSignIn(vaultForm.connector);
	// Restic through rclone into Replicaro/<vault name>: Restic is the only engine.
	const createUsesVaultFolder = usesVaultFolderName(vaultForm.connector);
	const createUsesRcloneRemote = vaultForm.connector === RCLONE_REMOTE_CONNECTOR && !vaultForm.coldStorage;
	const createObjectLockAvailable = !vaultForm.coldStorage && ["s3", "azblob", "gcs"].includes(vaultForm.connector) &&
		engineCatalog.some((descriptor) => descriptor.id === "kopia" && descriptor.installed &&
			descriptor.providers.some((provider) => provider.id === vaultForm.connector && provider.supported));
	const connectUsesRcloneSignIn = usesRcloneSignIn(connectForm.connector);
	// The vault name is the vault's folder name, so it can't be changed here.
	const connectUsesVaultFolder = usesVaultFolderName(connectForm.connector);
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
	            toast("error", t("ui.protect.jobNameSourceVaultRequired"));
	            return null;
	        }
		const draftNames = sourceDrafts.map((draft) => asciiNoCase(draft.name.trim()));
		if (new Set(draftNames).size !== draftNames.length) {
			toast("error", t("ui.protect.uniqueJobNameRequired"));
			return null;
		}
		const intervalError = customScheduleError(jobForm);
		if (intervalError) {
			toast("error", intervalError);
			return null;
		}
		const schedule = scheduleValue(jobForm);
		if (schedule == null) {
			toast("error", t("ui.protect.invalidCronSchedule"));
			return null;
		}
		const retention = parsedRetention(jobForm.retention);
		const retentionHourly = parsedRetention(jobForm.retentionHourly, true);
		const retentionDaily = parsedRetention(jobForm.retentionDaily, true);
		const retentionWeekly = parsedRetention(jobForm.retentionWeekly, true);
		const retentionMonthly = parsedRetention(jobForm.retentionMonthly, true);
		const retentionYearly = parsedRetention(jobForm.retentionYearly, true);
		if ([retention, retentionHourly, retentionDaily, retentionWeekly, retentionMonthly, retentionYearly].some((value) => value === null)) {
			toast("error", t("ui.protect.retentionValidation", { max: MAX_RETENTION_COUNT.toLocaleString(getEffectiveLocale()) }));
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
				toast(result?.warning ? "info" : "ok", result?.warning ?? t("ui.protect.jobUpdated", { name: payload.name }));
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
				toast(result?.warning ? "info" : "ok", result?.warning ?? t("ui.protect.jobCreated", { name: payload.name }));
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
							message: result?.warning ?? "",
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
						message: result?.warning ?? "",
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
			toast("error", t("ui.protect.selectBackupJob"));
			return;
		}
		const active = selectedJobs.filter(selectedJobIsActive);
		if (active.length > 0) {
			toast("error", t("ui.protect.waitForBulkJobs", { jobs: active.map((job) => job.name).join(", ") }));
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
			toast("error", t("ui.protect.chooseBulkSetting"));
			return;
		}
		const intervalError = bulkForm.changeSchedule ? customScheduleError(bulkForm) : null;
		if (intervalError) {
			toast("error", intervalError);
			return;
		}
		if (bulkForm.changeSchedule && scheduleValue(bulkForm) == null) {
			toast("error", t("ui.protect.invalidCronSchedule"));
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
				toast("error", t("ui.protect.retentionValidation", { max: MAX_RETENTION_COUNT.toLocaleString(getEffectiveLocale()) }));
				return;
			}
		}
		if (bulkForm.replaceDestinations && bulkForm.repositoryIds.length === 0) {
			toast("error", t("ui.protect.selectDestinationVault"));
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
			toast("error", t("ui.protect.bulkJobsNowActive", { jobs: active.map((job) => job.name).join(", ") }));
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
						message: result?.warning ?? "",
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
						message: result?.warning ?? "",
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
							next[index] = { ...item, message: t("ui.protect.waitForJobDefinitionUpdate") };
							setBulkResults({ ...bulkResults, items: [...next] });
							continue;
						}
						const result = await updateJob(item.payload);
						if (result?.job) applySavedJob(result.job);
						if (!ownsRetry()) break;
						next[index] = { jobId: item.jobId, name: item.name, status: result?.warning ? "warning" : "updated", message: result?.warning ?? "" };
					} else {
						const enabled = bulkResults.action === "enable";
						const result = await setJobEnabled(item.jobId, enabled);
						if (!ownsRetry()) break;
						next[index] = { jobId: item.jobId, name: item.name, status: result?.warning ? "warning" : result.changed ? "updated" : "unchanged", message: result?.warning ?? "", enabled };
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
			const showsRunResults = ownsSubmission() && reviewingMultipleTargets;
			if (showsRunResults) setRunResults(result.results);
			// Same per-vault notice as the top-bar "Run jobs now" picker. The one
			// exception: when no vault started and the review dialog is showing
			// the per-vault admission results, the toast points there instead of
			// repeating every reason. Runs started without that screen (a
			// single-vault job run straight from its card) get the full notice.
			const notice = manualRunNotice(jobName, job.targets, result.results);
			if (notice.admittedCount > 0 || !showsRunResults) toast(notice.kind, notice.message);
			else toast("info", t("ui.backup.runResult.noneStartedSeeResults", { jobName }));
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

    const openJobDelete = (job: BackupJob) => setJobDelete(job);

	const dismissJobDelete = () => {
		setJobDelete(null);
		return true;
	};

	// The deletion runs in the background once the backend has queued it: the
	// dialog closes at once, the job is hidden, and the result arrives as a
	// toast when the operation finishes (see followJobDeletion).
    const removeJob = async () => {
        if (!jobDelete) return;
		const jobId = jobDelete.id;
		const jobName = jobDelete.name;
		setJobDelete(null);
		pendingJobDeletions.current.add(jobId);
		setDeletingJobIDs((current) => new Set(current).add(jobId));
		// Hiding the job shrinks the list the same way a reload after deletion
		// did: drop it from the selection, and clear the selection once one job
		// or none is left (selection is only offered for two or more).
		const remaining = (savedJobs ?? []).filter((job) => job.id !== jobId && !deletingJobIDs.has(job.id));
		setSelectedJobIDs((current) => remaining.length <= 1
			? current.length === 0 ? current : []
			: current.includes(jobId) ? current.filter((id) => id !== jobId) : current);
		toast("info", t("ui.protect.jobBeingSentForDeletion", { name: jobName }));
        try {
			const { operationId } = await deleteJob(jobId);
			// The job stays in pendingJobDeletions until this operation finishes.
			followJobDeletion(operationId, jobName);
        } catch (error) {
			pendingJobDeletions.current.delete(jobId);
			toast("error", (error as Error).message);
			// A 409 job_being_deleted here means a deletion of this job is already
			// queued or running (from another tab or a stale page). The job must
			// stay hidden and that deletion's result still has to be reported, so
			// read the active deletions again rather than showing the job.
			if (error instanceof APIError && error.status === 409 && error.code === "job_being_deleted") {
				void refreshActiveOperations();
				return;
			}
			setDeletingJobIDs((current) => {
				const next = new Set(current);
				next.delete(jobId);
				return next;
			});
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
            toast("error", t("ui.protect.createVaultRequiredFields"));
            return;
        }
		if (!validVaultPassword(vaultForm.password)) {
			toast("error", t("ui.protect.invalidVaultPasswordWhitespace"));
			return;
		}
		if (!validVaultName(vaultForm.name)) {
			toast("error", t("ui.protect.vaultNameLengthValidation", { max: MAX_VAULT_NAME_CODE_POINTS }));
			return;
		}
		if (createMissingRequiredOptions.length > 0) {
			toast("error", t("ui.protect.requiredConnectorFields", { fields: createMissingRequiredOptions.map((option) => knownMessage(`ui.integration.${vaultForm.connector}.option.${option.key}.label`, option.label)).join(", ") }));
			return;
		}
        if (vaultForm.password !== vaultForm.passwordConfirmation) {
            toast("error", t("ui.protect.passwordsDoNotMatch"));
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
			const success = result.created ? t("ui.protect.vaultCreatedNamed", { name: payload.name }) : t("ui.protect.vaultAddedNamed", { name: payload.name });
			const issues = completedWithIssuesMessage(result);
			toast(issues ? "info" : "ok", issues ?? success);
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
				toast("info", t("ui.protect.usePendingVaultCard"));
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
				if (retryCreationIntentId && createUsesRcloneSignIn && message.includes("reauthorization")) {
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
			toast("error", t("ui.protect.connectVaultRequiredFields"));
			return false;
		}
		if (!validVaultPassword(submittedForm.password)) {
			toast("error", t("ui.protect.invalidVaultPasswordWhitespace"));
			return false;
		}
		// The repository validates its existing password directly. Requiring a
		// second entry here would only duplicate a secret that is not being set.
		if (connectMissingRequiredOptions.length > 0) {
			toast("error", t("ui.protect.requiredConnectorFields", { fields: connectMissingRequiredOptions.map((option) => knownMessage(`ui.integration.${connectForm.connector}.option.${option.key}.label`, option.label)).join(", ") }));
			return false;
		}
		const enteredOptions = connectorOptionsWithoutUnchangedDefaults(connectIntegration, connectorOptionsForSubmission(submittedForm, connectIntegration));
		// Cloud Connect takes native identity from its fresh rclone session.
		// Submit only the ordinary visible rclone wrapper options with it.
		const options = connectUsesRcloneSignIn
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
				throw new Error(t("ui.protect.multipleProfilesConflict"));
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
			const reviewedLocation = preview.existingVault?.candidateLocation ?? storage.location;
			setConnectReviewedLocation(reviewedLocation);
			setConnectReviewedRcloneAddress(storage.connector === RCLONE_REMOTE_CONNECTOR
				? rcloneRemoteVaultAddress(storage.options?.remote ?? "", storage.options?.path ?? "", reviewedLocation) : "");
			if (!refiningProfile) connectPreviewBaseFields.current = reviewedBaseFields;
			setConnectPreview(preview);
			if (preview.existingVault) {
				// A same-UUID copy updates the one registered row. Keep the current local
				// preferences and attachment selected; an older copied profile identifies
				// the vault but must never restore jobs or roll local settings backward.
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
			const profileChoiceIsAutomatic = preview.mode === "fallback" || Boolean(decisionChoice?.localAttachment) || usesSingleProfile(storage.connector);
			const profileChoiceWasMade = profileChoiceIsAutomatic || Boolean(selectedProfileUUID) || chooseJoin;
			setConnectProfileUUID(profileChoiceWasMade ? decisionChoice?.profile_uuid ?? "" : "");
			setConnectProfileAction(profileChoiceWasMade && decisionChoice ? (decisionChoice.localAttachment ? "reconnect" : "takeover") : "join");
			setConnectProfileChoiceMade(profileChoiceWasMade);
			setConnectOwnerAction("keep");
			setConnectOwnerChoiceMade(preview.mode === "fallback" || Boolean(decisionChoice?.vaultOwner));
			setConnectPendingProfileChoice(null);
			// Folder-named vaults can't take another name, so a name that is
			// already registered blocks the connection.
			const rcloneVaultNameConflict = (connector: string, name: string) => connector === RCLONE_REMOTE_CONNECTOR
				? t("ui.protect.existingRcloneRemoteVaultNameConflict", { name })
				: t("ui.protect.existingRcloneVaultNameConflict", { name, provider: connectIntegration?.label ?? t("ui.protect.cloudProvider") });
			const rcloneName = usesVaultFolderName(storage.connector)
				? normalizedRcloneFolderName(submittedForm.location)
				: "";
			if (preview.profile && decisionChoice && profileChoiceWasMade) {
				const importedName = rcloneName || preview.profile.vaultPreferences.name.trim();
				const hasRcloneConflict = Boolean(rcloneName) && (repos ?? []).some((repository) =>
					repository.id !== preview.profile!.vault_uuid && repository.name.trim().toLowerCase() === rcloneName.toLowerCase());
				const availableName = rcloneName || availableImportedVaultName(importedName, preview.profile.vault_uuid, repos);
				setConnectRcloneNameConflict(hasRcloneConflict);
				setConnectNameConflictNotice(hasRcloneConflict
					? rcloneVaultNameConflict(storage.connector, rcloneName)
					: availableName === importedName ? "" :
						t("ui.protect.importedVaultNameChanged", { before: importedName, after: availableName }));
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
					? rcloneVaultNameConflict(storage.connector, rcloneName)
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
			toast("error", error instanceof APIError && error.code === "kopia_rclone_provider_unsupported"
				? submittedForm.connector === RCLONE_REMOTE_CONNECTOR ? t("ui.protect.kopiaRcloneRemoteUnsupported")
					: t("ui.protect.kopiaRcloneProviderUnsupported", { provider: knownMessage(`ui.integration.${submittedForm.connector}.label`, connectIntegration?.label ?? submittedForm.connector) })
				: (error as Error).message);
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
			toast("error", t("ui.protect.pendingConnectionCredentialsRequired"));
			return;
		}
		if (!validVaultPassword(connectForm.password)) {
			toast("error", t("ui.protect.invalidVaultPasswordWhitespace"));
			return;
		}
		if (connectMissingRequiredOptions.length > 0) {
			toast("error", t("ui.protect.requiredConnectorFields", { fields: connectMissingRequiredOptions.map((option) => knownMessage(`ui.integration.${connectForm.connector}.option.${option.key}.label`, option.label)).join(", ") }));
			return;
		}
		const payload = {
			password: connectForm.password,
			// Native rclone identity and credentials come from the reviewed intent
			// and saved config (or the new auth session), not form option defaults.
			options: Object.fromEntries((connectIntegration?.options ?? [])
				.filter((option) => (option.credential || option.secret) && !connectUsesRcloneSignIn && !withheldRcloneConfigPassword(connectForm, option.key))
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
			const issues = completedWithIssuesMessage(result);
			toast(issues || result.profilePending ? "info" : "ok", issues ?? result.warning ?? (result.name ? t("ui.protect.vaultConnectedNamed", { name: result.name }) : t("ui.protect.vaultConnected")));
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
				const savedConfigRetryFailed = connectUsesRcloneSignIn && !payload.rcloneAuthSessionId &&
					error instanceof APIError && error.status === 400;
				const savedConfigMissing = savedConfigRetryFailed &&
					error.message === "the pending vault requires native rclone reauthorization before retry";
				const nativeAdmissionFailed = savedConfigRetryFailed &&
					error.message === "the detected restic vault rejected the vault encryption password or could not be validated";
				// A locally safe config can still fail native admission because of
				// expired authorization, a wrong vault password, or a network failure.
				// Offer a deliberate login fallback without claiming which occurred.
				if (savedConfigMissing || nativeAdmissionFailed) {
					setConnectRcloneAuthorizationAction("retry");
					toast("error", savedConfigMissing
						? t("ui.protect.savedRcloneConfigUnavailable")
						: t("ui.protect.nativeAdmissionRetryHelp", { error: error.message }));
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
		if (!preview || !storage || !validVaultName(connectForm.name)) { toast("error", t("ui.protect.reviewVaultNameLength", { max: MAX_VAULT_NAME_CODE_POINTS })); return; }
		if (preview.existingVault && connectUpdateConfirmedDigest !== connectUpdateReviewDigest) {
			toast("error", t("ui.protect.reviewExistingVaultUpdate"));
			return;
		}
		// The profile and owner controls explain the transfer, and the final Connect
		// vault click confirms it. Don't add a browser confirmation here; it would
		// only repeat those choices, and the backend still revalidates the
		// attachment change.
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
			const issues = completedWithIssuesMessage(result);
			toast(issues || result.profilePending ? "info" : "ok", issues ?? result.warning ?? t("ui.protect.vaultConnectedNamed", { name: result.name ?? vaultName }));
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
		setDormantJobs([]);
		setDormantBusy("");
		setVaultOwnership(null);
		setVaultOwnershipPresentation("unverified");
		setVaultOwnershipBusy(false);
		setVaultPasswordForm({ password: "", confirmation: "" });
		setToolBusy("");
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
				setVaultOwnershipPresentation(status.ownerTransfer === "unfinished" ? "transfer_unfinished" : status.isOwner ? "owner" : "nonowner");
			})
			.catch((error: unknown) => {
				if (!vaultSettingsSessionIsCurrent(session, repo.id) || vaultOwnershipRequest.current !== request) return;
				setVaultOwnership(null);
				setVaultOwnershipPresentation(error instanceof APIError && error.code === "owner_transfer_elsewhere" ? "transfer_elsewhere" : "unverified");
			});
	};

	const dismissVaultSettings = () => {
		pendingVaultTool.current = null;
		vaultOwnershipRequest.current++;
		vaultSettingsSession.current++;
		vaultSettingsOwner.current = "";
		setVaultOneAtATimeChoice(null);
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
		setConcurrencyMode(compatibleConcurrencyMode(repo.connector, schedules.concurrencyMode));
		setAutoUnlock(schedules.autoUnlock);
		setObjectLock(schedules.objectLock);
        setVaultInitialSchedules(schedules);
		void getDormantRecoveryJobs(repo.id)
			.then((jobs) => { if (vaultSettingsSessionIsCurrent(session, repo.id)) setDormantJobs(ownedDormantJobs(repo.id, jobs)); })
			.catch((error: Error) => { if (vaultSettingsSessionIsCurrent(session, repo.id)) toast("error", error.message); });
		detectVaultOwnership(repo, session);
    };

	const reopenVaultSettings = async (repositoryId: string, generation: number) => {
		// Reserve this page's next dialog intent before the authoritative read. A
		// newer Edit click changes the session and wins without disabling cards.
		const session = ++vaultSettingsSession.current;
		vaultSettingsOwner.current = repositoryId;
		try {
			// The failed response may have been lost after commit. Read the current
			// repository row directly before rebuilding the form; the retry payload
			// remains frozen on the card until this explicit action succeeds.
			const nextRepos = await getRepositories();
			if (!protectPageActive.current || !vaultSettingsSessionIsCurrent(session, repositoryId) || !ownsVaultMutation(repositoryId, generation)) return;
			const repository = nextRepos.find((candidate) => candidate.id === repositoryId);
			if (!repository) throw new Error(t("ui.protect.vaultNoLongerAvailable"));
			++refreshGenerations.current.repositories;
			setRepos(nextRepos);
			settleVaultSettingsPresentation(repositoryId, generation);
			openVaultSettings(repository);
		} catch (error) {
			if (protectPageActive.current && vaultSettingsSessionIsCurrent(session, repositoryId) && ownsVaultMutation(repositoryId, generation)) {
				toast("error", (error as Error).message);
			}
		}
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
			if (!integration) throw new Error(t("ui.protect.savedStorageTypeUnavailable"));
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
			const options = usesRcloneSignIn(fields.connector)
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
			// WebDAV needs nothing here: the backend never keeps a port or user
			// name in a WebDAV Location, so the saved options already hold the
			// port, WebDAV username, and WebDAV account password, and
			// restoreVaultDestinationFields has split the Server URL from the Path.
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
			setVaultOneAtATimeChoice({ kind: "reconnect", repository });
			return;
		}
		void openSavedVaultReconnect(repository);
	};

	// Also Finish takeover: an unfinished takeover is finished by sending the
	// same request again, with the owner the status reported, and the backend
	// resumes it from its saved phase.
	const takeOverVaultOwnership = async () => {
		if (!vaultSettings || !vaultOwnership || vaultOwnershipBusy) return;
		if (vaultOwnership.isOwner && vaultOwnership.ownerTransfer !== "unfinished") return;
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
			setVaultOwnershipPresentation(next.ownerTransfer === "unfinished" ? "transfer_unfinished" : next.isOwner ? "owner" : "nonowner");
			toast("ok", t("ui.protect.vaultOwnershipTransferred"));
		} catch (error) {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) toast("error", (error as Error).message);
		} finally {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) setVaultOwnershipBusy(false);
		}
	};

	const beginVaultPasswordChange = (repository: Repository, submitted: { password: string; confirmation: string }) => {
		if (vaultMutationSnapshot[repository.id]) return;
		setVaultPasswordForm({ password: "", confirmation: "" });
		// The dialog closes at once; the change runs as a background operation
		// and the vault card follows it. Its native and recovery semantics are
		// the phase record's, unchanged.
		dismissVaultSettings();
		startVaultPasswordMutation(repository.id, repository.name, submitted.password, submitted.confirmation);
	};

	const submitVaultPasswordChange = () => {
		if (!vaultSettings || !vaultOwnership?.isOwner || vaultMutationSnapshot[vaultSettings.id]) return;
		if (!validVaultPassword(vaultPasswordForm.password)) {
			toast("error", t("ui.protect.invalidVaultPasswordWhitespace"));
			return;
		}
		if (vaultPasswordForm.password !== vaultPasswordForm.confirmation) {
			toast("error", t("ui.protect.vaultPasswordsDoNotMatch"));
			return;
		}
		const submitted = { ...vaultPasswordForm };
		if (vaultCareHasUnsavedChanges()) {
			setVaultOneAtATimeChoice({ kind: "password", repository: vaultSettings });
			return;
		}
		beginVaultPasswordChange(vaultSettings, submitted);
	};

	const closeVault = () => {
		if (vaultCareHasUnsavedChanges()) {
			setClosePrompt("vault");
			return;
        }
		dismissVaultSettings();
    };

	const cancelClosePrompt = () => {
		pendingVaultTool.current = null;
		setClosePrompt(null);
	};

	const discardChanges = () => {
		const tool = closePrompt === "vault" ? pendingVaultTool.current : null;
		if (tool && vaultSettings && vaultInitialSchedules) {
			// "Run check now" or "Run reclamation now" asked first: drop the edits
			// and run the tool it was waiting for. The dialog closes once the
			// operation is queued; if the request is refused it stays open with the
			// saved values the tool actually ran against.
			pendingVaultTool.current = null;
			setCheckSchedule(vaultInitialSchedules.checkSchedule);
			setMaintenanceSchedule(vaultInitialSchedules.maintenanceSchedule);
			setConcurrencyMode(compatibleConcurrencyMode(vaultSettings.connector, vaultInitialSchedules.concurrencyMode));
			setAutoUnlock(vaultInitialSchedules.autoUnlock);
			setObjectLock(vaultInitialSchedules.objectLock);
			setClosePrompt(null);
			void queueVaultTool(tool);
			return;
		}
		if (closePrompt === "job") {
			dismissJob();
		} else if (closePrompt === "vault") {
			dismissVaultSettings();
		}
		setClosePrompt(null);
	};

	// Save after "Run check now" / "Run reclamation now" saves and closes the
	// dialog, exactly like closing it, and does not run the tool. The settings
	// save and a queued check or reclamation would compete for the vault lock,
	// and a reclamation that won would run on the old settings (object lock,
	// auto unlock). The tool can be run again once the save has finished; a
	// toast says so, because otherwise nothing shows that the tool did not run.
	const saveChangesBeforeClose = () => {
		const tool = closePrompt === "vault" ? pendingVaultTool.current : null;
		pendingVaultTool.current = null;
		setClosePrompt(null);
		if (closePrompt === "job") {
			void saveJob();
		} else if (closePrompt === "vault") {
			if (saveCare() && tool) toast("info", t("ui.protect.settingsSavedRunAgain"));
		}
	};

	// Returns whether the save was started; it is refused while other vault
	// work is running or the object lock settings are invalid.
	const saveCare = () => {
        if (!vaultSettings || vaultWorkState !== "idle") return false;
		const repositoryId = vaultSettings.id;
		const repositoryName = vaultSettings.name;
		if (!validObjectLockSettings(objectLock, maintenanceSchedule, true, vaultSettings.objectLock)) return false;
		const payload: VaultSettingsPayload = {
				repositoryId,
                checkSchedule,
				maintenanceSchedule,
				concurrencyMode,
				autoUnlock,
				objectLock,
				...(vaultOwnershipPresentation !== "owner" ? { profilePreferencesOnly: true } : {}),
		};
		const currentMutation = vaultMutationSnapshot[repositoryId];
		if (currentMutation && !isCleanupPendingRecovery(currentMutation)) return false;
		// Freeze all submitted values before closing so later form/session changes
		// cannot alter this save or its Retry save.
		dismissVaultSettings();
		return startVaultSettingsMutation(repositoryName, payload);
    };

	const chooseSaveVaultSettings = () => {
		if (vaultOneAtATimeChoice?.kind === "password") setVaultPasswordForm({ password: "", confirmation: "" });
		setVaultOneAtATimeChoice(null);
		saveCare();
	};

	const chooseReconnect = () => {
		if (!vaultOneAtATimeChoice || vaultOneAtATimeChoice.kind !== "reconnect") return;
		const repository = vaultOneAtATimeChoice.repository;
		setVaultOneAtATimeChoice(null);
		dismissVaultSettings();
		void openSavedVaultReconnect(repository);
	};

	const choosePasswordChange = () => {
		if (!vaultOneAtATimeChoice || vaultOneAtATimeChoice.kind !== "password") return;
		const repository = vaultOneAtATimeChoice.repository;
		const submitted = { ...vaultPasswordForm };
		setVaultOneAtATimeChoice(null);
		beginVaultPasswordChange(repository, submitted);
	};

	useEffect(() => {
		for (const mutation of Object.values(vaultMutations)) {
			const completionKey = `${mutation.repositoryId}:${mutation.generation}`;
			if (mutation.status === "succeeded" && !handledVaultMutationCompletions.current.has(completionKey)) {
				handledVaultMutationCompletions.current.add(completionKey);
				if (mutation.kind === "settings") {
					toast("ok", t("ui.protect.vaultSettingsSavedNamed", { name: mutation.repositoryName }));
				} else {
					// A change that took effect while the engine reported a failure is
					// completed with issues; its own text says so.
					toast(mutation.outcome === "success" ? "ok" : "info", mutation.message ?? t("ui.pages.protect.vault.password.change.completed"));
				}
				if (mutation.kind === "settings") settleVaultSettingsPresentation(mutation.repositoryId, mutation.generation);
				else clearVaultMutation(mutation.repositoryId, mutation.generation);
				load();
				continue;
			}
			if (mutation.status === "error" && !handledVaultMutationReloads.current.has(completionKey)) {
				handledVaultMutationReloads.current.add(completionKey);
				load();
			}
		}
		// Closing the settings dialog reacts to a module-owned mutation snapshot
		// (possibly started from an earlier mount of this page), so it can only
		// run once that snapshot has arrived here.
		// eslint-disable-next-line react-hooks/set-state-in-effect
		if (vaultSettings && vaultMutations[vaultSettings.id] && vaultMutationBlocksCard(vaultMutations[vaultSettings.id])) dismissVaultSettings();
		// dismissVaultSettings is intentionally not a dependency: this effect is
		// driven by coordinator snapshots, while the dialog cleanup helper changes
		// identity with ordinary form renders.
		// eslint-disable-next-line react-hooks/exhaustive-deps
	}, [load, toast, vaultMutations, vaultSettings]);

	// A manual check or maintenance runs in the background. The request only
	// queues it; the settings dialog closes and the dashboard opens its live
	// log, where it can be followed and cancelled (including a cold storage
	// reclamation that waits for provider retrieval).
	//
	// Queuing the tool closes the settings dialog, which would silently drop
	// unsaved edits, so with edits pending it asks first with the same
	// save/discard prompt as closing the dialog. Nothing is sent until the user
	// picks Discard (see discardChanges).
    const runTool = (kind: "check" | "maintenance") => {
        if (!vaultSettings || toolBusy) return;
		if (vaultCareHasUnsavedChanges()) {
			pendingVaultTool.current = kind;
			setClosePrompt("vault");
			return;
		}
		void queueVaultTool(kind);
	};

    const queueVaultTool = async (kind: "check" | "maintenance") => {
        if (!vaultSettings || toolBusy) return;
		const repositoryId = vaultSettings.id;
		const session = vaultSettingsSession.current;
        setToolBusy(kind);
        try {
			const { operationId } = kind === "check" ? await checkRepository(repositoryId) : await runMaintenance(repositoryId);
			if (!vaultSettingsSessionIsCurrent(session, repositoryId)) return;
			dismissVaultSettings();
			navigate(`/?operation=${encodeURIComponent(operationId)}`);
        } catch (error) {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) toast("error", (error as Error).message);
        } finally {
			if (vaultSettingsSessionIsCurrent(session, repositoryId)) setToolBusy("");
        }
    };

    const openVaultDelete = (repository: Repository) => {
		if (vaultRemovalIsInFlight(repository.id)) return;
		setVaultDelete(repository);
	};

	const dismissVaultDelete = () => {
		setVaultDelete(null);
		return true;
	};

	const removeVault = (repository: Repository, discardRecoveryProfile = false) => {
		if (vaultRemovalIsInFlight(repository.id)) return;
		setVaultDelete(null);
		runVaultRemoval(repository, discardRecoveryProfile, toast);
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
			toast(result?.warning ? "info" : "ok", result?.warning ?? (intent.phase === "prepared" ? t("ui.protect.pendingCreationCancelled") : t("ui.protect.pendingCreationForgotten")));
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
			if (connectUsesRcloneSignIn) setConnectRcloneAuthorizationAction("check");
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
			toast(result.warning ? "info" : "ok", result.warning ?? (action === "restore" ? t("ui.protect.dormantJobRestored") : t("ui.protect.dormantJobDiscarded")));
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
	const connectProfileSelectionVisible = connectPreview?.mode === "profile" && !connectPreview.existingVault && !usesSingleProfile(connectForm.connector) &&
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
		? t("ui.protect.existingVaultIntegrityLockedHelp")
		: t("ui.protect.newVaultIntegrityLockedHelp");
	const connectMaintenanceLockedHelp = connectPreview?.existingVault
		? t("ui.protect.existingVaultMaintenanceLockedHelp")
		: t("ui.protect.newVaultMaintenanceLockedHelp");
	// An Any Rclone Remote vault compares its whole address, so a changed remote
	// or path in remote shows up even when the folder name stays the same.
	const existingVaultRecord = connectPreview?.existingVault && connectReviewedRcloneAddress
		? (repos ?? []).find((repo) => repo.id === connectPreview.existingVault?.id) : undefined;
	const existingVaultSavedRcloneAddress = existingVaultRecord ? savedRcloneRemoteVaultAddress(existingVaultRecord) : "";
	const [existingVaultLocationBefore, existingVaultLocationAfter] = existingVaultSavedRcloneAddress
		? [ltrIsolate(existingVaultSavedRcloneAddress), ltrIsolate(connectReviewedRcloneAddress)]
		: [connectPreview?.existingVault?.location ?? "", connectReviewedLocation];
	const existingVaultUpdateChanges = connectPreview?.existingVault ? [
		...[existingVaultLocationBefore !== existingVaultLocationAfter
			? t("ui.protect.locationChangeReview", { before: existingVaultLocationBefore, after: existingVaultLocationAfter }) : t("ui.protect.locationUnchangedReview")],
		...(connectPreview.existingVault.name !== connectForm.name.trim() ? [t("ui.protect.nameChangeReview", { before: connectPreview.existingVault.name, after: connectForm.name.trim() })] : []),
		...(connectPreview.existingVault.description !== connectForm.description.trim() ?
			[t("ui.protect.descriptionChangeReview", { before: connectPreview.existingVault.description || t("ui.protect.emptyReview"), after: connectForm.description.trim() || t("ui.protect.emptyReview") })] : []),
		...(connectPreview.existingVault.checkSchedule !== connectForm.checkSchedule ? [t("ui.protect.integrityChangeReview", { before: connectPreview.existingVault.checkSchedule, after: connectForm.checkSchedule })] : []),
		...(connectPreview.existingVault.maintenanceSchedule !== connectForm.maintenanceSchedule ? [t("ui.protect.maintenanceChangeReview", { before: connectPreview.existingVault.maintenanceSchedule, after: connectForm.maintenanceSchedule })] : []),
		...(connectPreview.existingVault.concurrencyMode !== connectForm.concurrencyMode ? [t("ui.protect.performanceChangeReview", { before: connectPreview.existingVault.concurrencyMode, after: connectForm.concurrencyMode })] : []),
		t("ui.protect.credentialsChangeReview"),
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
		: connectPreview?.vault_owner_profile_uuid ? t("ui.protect.profileIdLabel", { id: connectPreview.vault_owner_profile_uuid }) : t("ui.protect.currentProfileLabel");
	// Pending intent recovery appears only after the user enters its destination,
	// so stale intents do not take over the reconnect screen. The server still
	// checks physical identity and the saved review on retry.
	const enteredConnectLocation = vaultLocation(connectForm);
	const matchingConnectionIntent = enteredConnectLocation ? connectionIntents.find((intent) =>
		connectionIntentMatchesDestination(intent, connectForm, enteredConnectLocation)) : undefined;
	// A cloud folder name alone cannot identify the native account. Keep the
	// ordinary authorization Check reachable beside any saved-account retry.
	const canCheckAnotherRcloneAccount = connectUsesRcloneSignIn;
	const createVaultNameField = <label className="field"><span>{t("ui.pages.protect.vault.name")}</span><input aria-label={t("ui.pages.protect.vault.name")} disabled={Boolean(vaultForm.pendingLocation)} value={vaultForm.name} placeholder={t("ui.pages.protect.example.work.archive")} onChange={(event) => setVaultForm({ ...vaultForm, name: event.target.value })} /><small>{t("ui.protect.vaultNameLengthHelp", { count: MAX_VAULT_NAME_CODE_POINTS })}</small></label>;
    return (
        <div className="page protect-page">
            <header className="page-header simple">
                <h1 className="page-title">{t("ui.pages.protect.protect")}</h1>
                <p className="page-desc">{t("ui.pages.protect.backup.your.data.into.encrypted.vaults")}</p>
            </header>

	            <section id="jobs">
	                <div className="section-heading">
	                    <h2>{t("ui.pages.protect.backup.jobs")}</h2>
	                    <div className="job-heading-actions">
						{(jobs?.length ?? 0) > 1 && selectedJobs.length > 0 && <span className="selection-count">{t("ui.protect.selectedCount", { count: selectedJobs.length })}</span>}
						{/* Like + Vault below, this is the primary action only while its list
						    is empty; once a job exists it steps back to a plain button. It
						    stays disabled until there is a vault to back up to. */}
						<button className={`btn${jobs !== null && jobs.length === 0 ? " primary" : ""}`} disabled={!repos?.length} onClick={() => openJob()}>
							<Icon name="plus" size={14} /> {t("ui.protect.backupJob")}
						</button>
					</div>
	                </div>
	                <p className="section-copy">{t("ui.pages.protect.create.snapshots.of.your.data")}</p>
				{jobs !== null && jobs.length > 1 && selectedJobs.length > 0 && <div className="job-bulk-toolbar">
					<label className="check"><input type="checkbox" aria-label={t("ui.pages.protect.select.all.jobs")} checked={selectedJobs.length === jobs.length} disabled={bulkSaving} onChange={() => selectedJobs.length === jobs.length ? setSelectedJobIDs([]) : selectAllJobs()} />{t("ui.pages.protect.select.all.jobs")}</label>
					{selectedJobs.length > 1 && <div className="tool-buttons">
						<button className="btn sm" disabled={bulkSaving} onClick={openBulkEdit}>{t("ui.pages.protect.edit")}</button>
						<button className="btn sm" disabled={bulkSaving} onClick={() => void applyBulkEnabled(true)}>{t("ui.pages.protect.enable")}</button>
						<button className="btn sm" disabled={bulkSaving} onClick={() => void applyBulkEnabled(false)}>{t("ui.pages.protect.disable")}</button>
						<button className="btn sm" disabled={bulkSaving} onClick={() => setSelectedJobIDs([])}>{t("ui.pages.protect.clear.selection")}</button>
					</div>}
				</div>}

                {jobs === null && <Loading />}
                {jobs !== null && jobs.length === 0 && (
                    <EmptyState icon="jobs" title={repos?.length ? t("ui.protect.noBackupJobs") : t("ui.protect.addVaultFirst")}>
                        <p>{repos?.length ? t("ui.pages.protect.create.a.job.to.protect.a.source.directory") : t("ui.pages.protect.jobs.need.a.vault.for.their.backup.data")}</p>
                    </EmptyState>
                )}

                <div className="job-flow-list">
                    {pageJobs.map((job) => {
                        const activeTargets = job.targets.filter(backupTargetIsActive);
                        const failedTargets = job.targets.filter((target) => target.lastStatus === "failed" || target.lastStatus === "interrupted" || target.lastStatus === "reconnect_required");
                        const issueTargets = job.targets.filter((target) => target.lastStatus === "completed_with_issues");
                        const isRunning = Boolean(running[job.id]) || activeTargets.length > 0;
                        const sourceUnavailable = job.targets.some((target) => target.sourceAvailability === "unavailable");
						// Offered whenever the source cannot be used (paused or failing);
						// the backend also refuses while any target is queued or running.
						const offerSourceUpdate = jobSourceNeedsUpdate(job) && !isRunning;
						// After "Update job source" the card shows the location in use as
						// the job's source, without calling it an alias. The immutable
						// source is kept in the backend.
						const currentSource = jobCurrentSource(job);
                        const vaultUnavailable = job.targets.some((target) => target.targetAvailability === "unavailable");
                        const successfulTargets = job.targets.filter((target) => target.lastStatus === "success");
                        const fullyProtected = job.targets.length > 0 && successfulTargets.length === job.targets.length;
                        const partiallyProtected = successfulTargets.length > 0 && !fullyProtected;
                        const tone = !job.enabled ? "idle" : isRunning ? "accent" : sourceUnavailable || vaultUnavailable || failedTargets.length > 0 ? "danger" : issueTargets.length > 0 ? "warn" : fullyProtected ? "ok" : "warn";
                        const status = !job.enabled ? t("ui.jobStatus.paused") : isRunning ? t("ui.jobStatus.queuedRunning", { count: activeTargets.length }) : sourceUnavailable ? t("ui.jobStatus.sourceUnavailable") : vaultUnavailable ? t("ui.jobStatus.vaultUnavailable") : failedTargets.length > 0 ? t("ui.jobStatus.failedCount", { count: failedTargets.length }) : issueTargets.length > 0 ? t("ui.jobStatus.completedWithIssues") : fullyProtected ? t("ui.jobStatus.protected") : partiallyProtected ? t("ui.jobStatus.partiallyProtected") : t("ui.jobStatus.notRunYet");
                        const destinationStatus = activeTargets.length > 0
                            ? t("ui.destinationStatus.running")
                            : failedTargets.length > 0
                                ? t("ui.destinationStatus.failure")
                                : issueTargets.length > 0
                                    ? t("ui.destinationStatus.completedWithIssues")
                                    : fullyProtected
                                        ? t("ui.destinationStatus.successful")
                                        : partiallyProtected
                                            ? t("ui.destinationStatus.successfulCount", { successful: successfulTargets.length, total: job.targets.length })
                                            : t("ui.destinationStatus.notRun");
                        const targetDetails = job.targets.map((target) =>
                            `${target.repositoryName}: ${targetStatusLabel(target.lastStatus)}`
                        ).join(" · ");
                        const pendingCatchUpTargets = job.targets.filter((target) => target.pendingCatchUp);
                        return (
	                            <article key={job.id} className={`job-flow ${tone}${selectedJobIDs.includes(job.id) ? " selected" : ""}`}>
								<div className="job-leading-controls">
									{(jobs?.length ?? 0) > 1 && <input type="checkbox" aria-label={t("ui.protect.selectNamedJob", { name: job.name })} checked={selectedJobIDs.includes(job.id)} disabled={bulkSaving} onChange={() => toggleJobSelection(job.id)} />}
									<button className={`toggle${job.enabled ? " on" : ""}`} disabled={Boolean(jobToggleBusy) || bulkSaving} onClick={() => void toggle(job)} aria-label={job.enabled ? t("ui.protect.disableNamedJob", { name: job.name }) : t("ui.protect.enableNamedJob", { name: job.name })}><span /></button>
								</div>
                                <div className="flow-source">
                                    <strong>{job.name}</strong>
									<Tooltip content={displayPath(currentSource)}>
										<span className="flow-source-path"><span className="flow-source-path-text"><b>{t("ui.pages.protect.source")}</b> {displayPath(currentSource)}</span></span>
									</Tooltip>
									<span><b>{t("ui.pages.protect.size")}</b> {job.sizeBytes == null ? t("ui.pages.protect.not.measured.yet") : readableSize(job.sizeBytes)}</span>
                                </div>
                                <div className="flow-glyph">
                                    <span className="flow-status">{status === t("ui.jobStatus.partiallyProtected") ? renderMessage("ui.jobStatus.partiallyProtectedBreak", { break: <br /> }) : status}</span>
									{offerSourceUpdate && <button type="button" className="btn sm flow-status-action" onClick={() => setSourceUpdateJob(job)}>{t("ui.protect.updateJobSource")}</button>}
                                    <svg className="flow-arrow-horizontal" viewBox="0 0 120 10" aria-hidden="true">
                                        <line x1="0" y1="5" x2="112" y2="5" />
                                        <path d="M112 1.5L119 5l-7 3.5z" />
                                    </svg>
                                    <svg className="flow-arrow-vertical" viewBox="0 0 10 36" aria-hidden="true">
                                        <line x1="5" y1="0" x2="5" y2="28" />
                                        <path d="M1.5 28L5 35l3.5-7z" />
                                    </svg>
									<Tooltip content={nextSnapshotTooltip(job)}><span>{lowercaseEnglishLabel(scheduleLabel(job.schedule))}</span></Tooltip>
                                </div>
                                <div className="flow-vault">
                                    <strong>{job.targets.map((target) => target.repositoryName).join(", ")}</strong>
                                    <Tooltip content={targetDetails}>
                                        <span className="flow-vault-status">
                                            <b>{t("ui.protect.vaultLabel", { count: job.targets.length })}</b> {t("ui.protect.vaultCount", { count: job.targets.length })} · {destinationStatus}
                                        </span>
                                    </Tooltip>
                                    {pendingCatchUpTargets.length > 0 && <span className="recovery-warning">{renderMessage("ui.protect.catchUpPendingForTargets", { status: <b>{t("ui.pages.protect.scheduled.catch.up.pending")}</b>, targets: pendingCatchUpTargets.map((target) => t("ui.protect.catchUpTarget", { vault: target.repositoryName, count: target.coalescedMissedCount })).join(", ") })}</span>}
                                </div>
                                <div className="row-actions">
                                    <Tooltip content={t("ui.pages.protect.run.job")}>
                                        <button className="btn ghost-icon job-action-run" aria-label={t("ui.protect.runNamedJobAction", { name: job.name })} disabled={Boolean(runActiveJobID) || job.targets.every(backupTargetIsActive)} onClick={() => job.targets.length > 1 ? openRunReview(job.id) : void startRun(job)}>{(isRunning || runActiveJobID === job.id) ? <span className="spinner" /> : <Icon name="play" size={14} />}</button>
                                    </Tooltip>
									<Tooltip content={t("ui.pages.protect.copy.job")}>
                                        <button className="btn ghost-icon" aria-label={t("ui.protect.copyNamedJob", { name: job.name })} onClick={() => copyJob(job)}><Icon name="copy" size={15} /></button>
                                    </Tooltip>
                                    <Tooltip content={t("ui.pages.protect.job.settings")}>
                                        <button className="btn ghost-icon" disabled={isRunning} aria-label={t("ui.protect.editNamedJob", { name: job.name })} onClick={() => openJob(job)}><Icon name="edit" size={14} /></button>
                                    </Tooltip>
                                    <Tooltip content={t("ui.pages.protect.delete.job")}>
                                        <button className="btn ghost-icon danger-hover" aria-label={t("ui.protect.deleteNamedJob", { name: job.name })} onClick={() => openJobDelete(job)}><Icon name="trash" size={14} /></button>
                                    </Tooltip>
                                </div>
                            </article>
                        );
                    })}
                </div>
                {jobs !== null && jobs.length > 0 && (
                    <ListControls
                        label={t("ui.pages.protect.jobs.2")}
                        total={jobs.length}
                        defaultPageSize={JOB_PAGE_SIZE}
                        page={safeJobPage}
                        pageSize={jobPageSize}
                        pageSizeOptions={JOB_PAGE_SIZE_OPTIONS}
                        sort={jobSort}
                        sortOptions={JOB_SORT_OPTIONS.map(({ value, label }) => ({ value, label: label() }))}
                        onPage={setJobPage}
                        onPageSize={changeJobPageSize}
                        onSort={changeJobSort}
                    />
                )}
            </section>

            <section className="vault-section" id="vaults">
                <div className="section-heading">
                    <h2>{t("ui.pages.protect.vaults")}</h2>
                    <button className={`btn${repos !== null && repos.length === 0 ? " primary" : ""}`} onClick={() => setShowVaultChoice(true)}><Icon name="plus" size={14} /> {t("ui.pages.protect.vault")}</button>
                </div>
                <p className="section-copy">{t("ui.pages.protect.where.snapshots.live")}</p>

                {repos === null && <Loading />}
                {repos !== null && repos.length === 0 && creationIntents.length === 0 && (
                    <EmptyState icon="vault" title={t("ui.pages.protect.no.vaults.yet")}><p>{t("ui.pages.protect.add.a.local.or.remote.vault.to.get.started")}</p></EmptyState>
                )}

                <div className="vault-list">
					{creationIntents.map((intent) => (
						<article key={intent.id} className="vault-row failed-vault-card">
							<div className="vault-identity">
								<div className="vault-card-head">
									<span className="vault-glyph"><Icon name="shield" size={18} /></span>
									<span className="vault-chips">
										<span className="connector-chip">{t("ui.pages.protect.creation.pending")}</span>
										<span className="connector-chip">{intent.engine}</span>
									</span>
								</div>
								<strong>{intent.name}</strong>
								<Tooltip content={intent.location}>
									<span className="vault-location"><span className="vault-location-text">{intent.location}</span></span>
								</Tooltip>
							</div>
							<div className="vault-stat"><strong>{t("ui.pages.protect.not.measured.yet")}</strong><span>{t("ui.pages.protect.vault.size")}</span></div>
							<div className="row-actions vault-actions failed-vault-actions">
									<button className="btn sm" onClick={() => retryCreation(intent)}>{t("ui.pages.protect.retry")}</button>
								<button className="btn sm" onClick={() => setCreationErrorView(intent)}>{t("ui.pages.protect.view.error")}</button>
								<button
									className="btn sm danger-outline"
									disabled={Boolean(creationRemovalBusy)}
										onClick={() => void removePendingCreation(intent)}
								>
									{creationRemovalBusy === intent.id && <span className="spinner" />}
										{intent.phase === "prepared" ? t("ui.pages.protect.cancel") : t("ui.protect.forget")}
								</button>
							</div>
						</article>
					))}
                    {pageVaults.map((repo) => {
						const removal = vaultRemoval[repo.id];
						const mutation = vaultMutations[repo.id];
						const cardBlocked = Boolean(removal || (mutation && vaultMutationBlocksCard(mutation)));
						const stats = vaultStats[repo.id] ?? { vaultSizeBytes: repo.vaultSizeBytes, vaultSizeMeasuredAt: repo.vaultSizeMeasuredAt, vaultSizeDirty: repo.vaultSizeDirty, fresh: false, running: false, pending: false, paused: false };
						const sizePresentation = stats.vaultSizeBytes == null ? null : vaultSizePresentation(stats.vaultSizeBytes);
						const pendingProfile = profileSync[repo.id];
						const statsActive = stats.running || stats.pending;
						const statsStatus = stats.paused ? t("ui.protect.statsRefreshPaused") : t("ui.protect.statsRefreshing");
						const reconnectRequired = Boolean(repo.reconnectRequired);
                        return (
							<article key={repo.id} className="vault-row" data-vault-id={repo.id}>
									{/* Covered controls must leave keyboard navigation while removal owns this card. */}
									<div className="vault-identity" inert={cardBlocked || undefined}>
										<div className="vault-card-head">
										  <span className="vault-glyph"><Icon name="shield" size={18} /></span>
									  <span className="vault-chips">
										<span className="connector-chip">{repo.engine}</span>
										{/* A long remote label (such as an S3 endpoint) stays on one line
										    beside the engine chip; the tooltip always shows it in full,
										    as the location line below does. */}
										<Tooltip content={repositoryConnectorLabelContent(repo)}>
											<span className="connector-chip connector-label-chip" aria-label={repositoryConnectorLabel(repo)}><span className="connector-label-text">{repositoryConnectorLabelContent(repo)}</span></span>
										</Tooltip>
									  </span>
										</div>
										<strong>{repo.name}</strong>
										<Tooltip content={repo.location}>
											<span className="vault-location"><span className="vault-location-text">{repositoryLocationLabel(repo)}</span></span>
										</Tooltip>
									{repo.connector === "webdav" && webdavUsesPlainHTTP(repo.location) && <p className="recovery-warning webdav-plain-http-warning">{t("ui.protect.webdavPlainHTTPCardWarning")}</p>}
									{pendingProfile?.lastError && (
									<span className="recovery-warning profile-sync-warning">{t("ui.protect.profileUpdatePending", { error: pendingProfile.lastError })} {pendingProfile.nextAttemptAt ? t("ui.protect.retryAfter", { time: timeAgo(pendingProfile.nextAttemptAt) }) : ""} <button className="btn sm" disabled={cardBlocked} onClick={() => void retryProfile(repo.id)}>{t("ui.pages.protect.retry.now")}</button></span>
									)}
									{reconnectRequired && <span className="recovery-warning">{t("notifications.message.vaultReconnectRequired")}</span>}
                                </div>
								<div className={`vault-stat vault-size-stat${statsActive ? " is-refreshing" : ""}`} inert={cardBlocked || undefined}>
									<div className="vault-size-summary">{sizePresentation ? <Tooltip content={sizePresentation.tooltip}><span className="vault-size-value" aria-label={sizePresentation.display}><strong>{sizePresentation.display}</strong></span></Tooltip> : <strong>{t("ui.pages.protect.not.measured.yet")}</strong>}<span>{t("ui.pages.protect.vault.size")}</span></div>
									<div className="vault-size-meta"><small>{t("ui.protect.sizeStatsUpdated", { time: stats.vaultSizeMeasuredAt ? timeAgo(stats.vaultSizeMeasuredAt) : t("ui.protect.notMeasuredYet") })}</small><Tooltip content={t("ui.pages.protect.replicaro.periodically.refreshes.vault.size.stats.you.can.force.a.refr")}><button className="vault-stats-refresh" aria-label={t("ui.pages.protect.refresh.stats.now")} disabled={cardBlocked} onClick={() => refreshVaultSize(repo.id)}>{t("ui.pages.protect.refresh.stats.now")}</button></Tooltip></div>
									{statsActive && <span className="vault-stats-refreshing">{!stats.paused && <span className="spinner" />}<span><strong>{statsStatus}</strong><span>{t("ui.pages.protect.you.can.safely.close.this.page.if.you.need.to.refreshing.will.resume.i")}</span></span></span>}
									{stats.failure && <small className="recovery-warning">{vaultSizeFailureText(stats.failure)}</small>}
								</div>
								<div className="row-actions vault-actions" inert={cardBlocked || undefined}>
									{reconnectRequired && <button className="btn sm danger" disabled={cardBlocked} onClick={() => void openSavedVaultReconnect(repo)}>{t("ui.pages.protect.reconnect")}</button>}
									<Link className="btn sm" to={`/restore/${repo.id}`} tabIndex={cardBlocked ? -1 : undefined} aria-disabled={cardBlocked} onClick={(event) => { if (cardBlocked) event.preventDefault(); }}>{t("ui.pages.protect.browse")}</Link>
                                    <Tooltip content={t("ui.pages.protect.vault.settings")}>
											<button className="btn ghost-icon" aria-label={t("ui.protect.editNamedVault", { name: repo.name })} disabled={cardBlocked} onClick={() => openVaultSettings(repo)}><Icon name="edit" size={14} /></button>
                                    </Tooltip>
                                    <Tooltip content={t("ui.pages.protect.delete.vault")}>
											<button className="btn ghost-icon danger-hover" aria-label={t("ui.protect.deleteNamedVault", { name: repo.name })} disabled={cardBlocked} onClick={() => openVaultDelete(repo)}><Icon name="trash" size={14} /></button>
                                    </Tooltip>
                                </div>
								{removal && <div className="vault-removal-overlay" role="status" aria-live="polite">
									<VaultOverlayIdentity name={repo.name} />
									{removal.phase === "removing" ? <><strong className="vault-overlay-status">{t("ui.pages.protect.vault.is.being.removed")}</strong><span className="spinner" aria-hidden="true" /></> : <>
										<strong>{removal.phase === "profile_error" ? t("ui.protect.vaultRemovalProfileStopped") : t("ui.pages.protect.the.vault.could.not.be.removed")}</strong>
										{removal.message && <span>{removal.message}</span>}
										{removal.phase === "profile_error" && <span>{t("ui.pages.protect.removing.it.anyway.leaves.the.remote.recovery.profile.unchanged.the.va")}</span>}
										<div className="vault-removal-actions"><button className="btn sm" onClick={() => dismissVaultRemoval(repo.id)}>{t("ui.pages.protect.keep.vault")}</button><button className="btn sm danger-outline" onClick={() => removeVault(repo)}>{t("ui.pages.protect.retry.removal")}</button>{removal.phase === "profile_error" && <button className="btn sm danger-outline" onClick={() => removeVault(repo, true)}>{t("ui.pages.protect.remove.anyway")}</button>}</div>
									</>}
								</div>}
								{mutation && !removal && <VaultMutationOverlay mutation={mutation} onReopenSettings={(repositoryId, generation) => { void reopenVaultSettings(repositoryId, generation); }} />}
                            </article>
                        );
                    })}
                </div>
                {repos !== null && repos.length > 0 && (
                    <ListControls
                        label={t("ui.pages.protect.vaults.2")}
                        total={repos.length}
                        defaultPageSize={VAULT_PAGE_SIZE}
                        page={safeVaultPage}
                        pageSize={vaultPageSize}
                        pageSizeOptions={VAULT_PAGE_SIZE_OPTIONS}
                        sort={vaultSort}
                        sortOptions={VAULT_SORT_OPTIONS.map(({ value, label }) => ({ value, label: label() }))}
                        onPage={setVaultPage}
                        onPageSize={changeVaultPageSize}
                        onSort={changeVaultSort}
                    />
                )}
            </section>

	            {jobModal && (
	                <Modal title={jobModal === "new" ? t("ui.protect.newBackupJob") : t("ui.protect.editJob")} wide onClose={closeJob}>
					<fieldset className="modal-workflow-fields" disabled={jobSaving}>
	                    <div className="modal-form">
							{jobModal === "new" && !jobCopyDraft ? <>
								<p className="muted modal-intro">{t("ui.pages.protect.you.can.add.one.or.more.source.data.to.back.up.each.source.data.will.g")}</p>
								<label className="field"><span>{t("ui.pages.protect.name")}</span><input autoFocus value={jobForm.name} placeholder={t("ui.pages.protect.example.documents.nightly")} onChange={(event) => setJobForm({ ...jobForm, name: event.target.value })} /></label>
								<label className="field"><span>{t("ui.pages.protect.source.data")}</span><DirectoryField ariaLabel={t("ui.pages.protect.source.data")} value={jobForm.source} onChange={(source) => setJobForm({ ...jobForm, source })} /><small>{t("ui.pages.protect.must.be.a.local.folder.mounted.share.mapped.drive.or.network.path.exte")}</small></label>
								{jobAdditionalSources.map((source, index) => <div className="field job-additional-source" key={index}>
									<span>{t("ui.pages.protect.source.data")}</span>
									<div className="job-additional-source-controls">
										<DirectoryField ariaLabel={t("ui.protect.numberedSourceData", { number: index + 2 })} value={source} onChange={(nextSource) => setJobAdditionalSources((current) => current.map((item, itemIndex) => itemIndex === index ? nextSource : item))} />
										<button type="button" className="btn sm danger-outline remove-source" aria-label={t("ui.protect.removeNumberedSource", { number: index + 2 })} onClick={() => setJobAdditionalSources((current) => current.filter((_, itemIndex) => itemIndex !== index))}>{t("ui.pages.protect.remove.source")}</button>
									</div>
									<small>{t("ui.pages.protect.must.be.a.local.folder.mounted.share.mapped.drive.or.network.path.exte")}</small>
								</div>)}
								<button type="button" className="btn" onClick={() => setJobAdditionalSources((current) => [...current, ""])}><Icon name="plus" size={14} /> {t("ui.pages.protect.add.source")}</button>
							</> : <>
								<label className="field"><span>{t("ui.pages.protect.name")}</span><input autoFocus={!jobCopyDraft} value={jobForm.name} placeholder={t("ui.pages.protect.example.documents.nightly")} onChange={(event) => setJobForm({ ...jobForm, name: event.target.value })} /></label>
								{/* Keep existing sources read-only, including unbound imports. The backend
								    permits source updates before local binding, but exposing that here would
								    let users point the same job ID at different data and alter native retention
								    scope. Backend mutability does not imply an editable source field. */}
								<label className="field"><span>{t("ui.pages.protect.source.data")}</span><DirectoryField ariaLabel={t("ui.pages.protect.source.data")} value={jobModal !== "new" && jobModal ? jobCurrentSource(jobModal) : jobForm.source} readOnly={jobModal !== "new"} autoFocus={jobCopyDraft} onChange={(source) => setJobForm({ ...jobForm, source })} /><small>{jobModal === "new" ? t("ui.pages.protect.must.be.a.local.folder.mounted.share.mapped.drive.or.network.path.exte") : t("ui.protect.editSourceImmutableHelp")}</small></label>
							</>}
                        <div className="field"><span>{t("ui.pages.protect.destination.vaults")}</span><DestinationVaultPicker repositories={repos ?? []} selectedIds={jobForm.repositoryIds} onChange={(repositoryIds) => setJobForm({ ...jobForm, repositoryIds })} /><small>{t("ui.pages.protect.select.one.or.more.vaults.backups.for.all.vaults.are.managed.by.this.j")}</small></div>
                        <label className="field"><span>{t("ui.pages.protect.schedule")}</span><select aria-label={t("ui.pages.protect.schedule")} value={jobForm.schedule} onChange={(event) => setJobForm({ ...jobForm, schedule: event.target.value })}>{schedulePresets.map(([value, label]) => <option key={value} value={value}>{label()}</option>)}</select><small>{scheduleHelp(jobForm.schedule)}</small></label>
                        {customScheduleUnits[jobForm.schedule] && <label className="field compact-field"><span>{customScheduleUnits[jobForm.schedule]!.label()}</span><input type="number" min={1} max={customScheduleUnits[jobForm.schedule]!.max} value={jobForm.customInterval} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, customInterval: event.target.value })} /></label>}
						{jobForm.schedule === "cron" && <label className="field"><span>{t("ui.pages.protect.cron.expression")}</span><input className="mono" value={jobForm.cronExpression} placeholder="0 2 * * *" maxLength={MAX_CRON_EXPRESSION_LENGTH} onChange={(event) => setJobForm({ ...jobForm, cronExpression: event.target.value })} /><small>{cronScheduleHelp()}</small></label>}
						<label className="field"><span>{t("ui.pages.protect.retention.policy")}</span><select aria-label={t("ui.pages.protect.retention.policy")} value={retentionPreset(jobForm.retention)} onChange={(event) => setJobForm({ ...jobForm, retention: event.target.value === "custom" ? (retentionPreset(jobForm.retention) === "custom" ? jobForm.retention : "") : event.target.value })}>{retentionPresets.map(([value, label]) => <option key={value} value={value}>{label()}</option>)}</select>
							{/* Deliberate simplification: 100 snapshots do not guarantee 100 distinct versions for every file. Do not change this help text. */}
							<small>{t("ui.pages.protect.how.many.versions.of.your.files.do.you.want.to.keep.in.the.backup.100")}</small></label>
						{retentionPreset(jobForm.retention) === "custom" && <label className="field compact-field"><span>{t("ui.pages.protect.number.of.latest.snapshots.to.keep")}</span><input aria-label={t("ui.pages.protect.number.of.latest.snapshots.to.keep")} type="number" min={1} max={MAX_MAIN_RETENTION_COUNT} value={jobForm.retention} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retention: event.target.value })} /></label>}
                        <button className="btn advanced-toggle" onClick={() => setJobAdvanced((value) => !value)}><Icon name="settings" size={14} />{jobAdvanced ? t("ui.pages.protect.hide.advanced.settings") : t("ui.pages.protect.advanced.settings")}</button>
                        {jobAdvanced && (
                            <div className="advanced-panel">
								<div className="engine-settings">
									<div className="engine-settings-heading"><strong>{t("ui.pages.protect.more.retention.options.optional")}</strong><small>{t("ui.protect.advancedRetentionHelp")}<br /><br />{t("ui.protect.advancedRetentionExample")}<br /><br />{t("ui.protect.leaveTierBlankHelp")}</small></div>
									<label className="field"><span>{t("ui.pages.protect.keep.n.latest.hourly.snapshots")}</span><input aria-label={t("ui.pages.protect.keep.n.latest.hourly.snapshots")} type="number" min={1} max={MAX_RETENTION_COUNT} value={jobForm.retentionHourly} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retentionHourly: event.target.value })} /></label>
									<label className="field"><span>{t("ui.pages.protect.keep.n.latest.daily.snapshots")}</span><input aria-label={t("ui.pages.protect.keep.n.latest.daily.snapshots")} type="number" min={1} max={MAX_RETENTION_COUNT} value={jobForm.retentionDaily} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retentionDaily: event.target.value })} /></label>
									<label className="field"><span>{t("ui.pages.protect.keep.n.latest.weekly.snapshots")}</span><input aria-label={t("ui.pages.protect.keep.n.latest.weekly.snapshots")} type="number" min={1} max={MAX_RETENTION_COUNT} value={jobForm.retentionWeekly} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retentionWeekly: event.target.value })} /></label>
									<label className="field"><span>{t("ui.pages.protect.keep.n.latest.monthly.snapshots")}</span><input aria-label={t("ui.pages.protect.keep.n.latest.monthly.snapshots")} type="number" min={1} max={MAX_RETENTION_COUNT} value={jobForm.retentionMonthly} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retentionMonthly: event.target.value })} /></label>
									<label className="field"><span>{t("ui.pages.protect.keep.n.latest.yearly.snapshots")}</span><input aria-label={t("ui.pages.protect.keep.n.latest.yearly.snapshots")} type="number" min={1} max={MAX_RETENTION_COUNT} value={jobForm.retentionYearly} onWheel={preventNumberInputWheel} onChange={(event) => setJobForm({ ...jobForm, retentionYearly: event.target.value })} /></label>
								</div>
	                                <label className="field"><span>{t("ui.pages.protect.exclude.patterns.optional")}</span><textarea className="mono" rows={3} value={jobForm.excludes} placeholder={`${examplePlaceholder("*.tmp")}\n${examplePlaceholder("node_modules")}`} onChange={(event) => setJobForm({ ...jobForm, excludes: event.target.value })} /><small>{t("ui.pages.protect.one.pattern.per.line.for.files.folders.this.job.should.skip")}</small></label>
								<label className="field"><span>{t("ui.pages.protect.tag.optional")}</span><input value={jobForm.tag} onChange={(event) => setJobForm({ ...jobForm, tag: event.target.value })} /><small>{t("ui.pages.protect.optional.label.applied.to.snapshots.created.by.this.job")}</small></label>
                                <div className="engine-settings">
									<div className="engine-settings-heading"><strong>{t("ui.pages.protect.engine.specific.settings.optional")}</strong><small>{t("ui.pages.protect.these.settings.apply.only.to.the.selected.vault.engines")}</small></div>
                                    {selectedEngines.length === 0 && <p className="engine-settings-empty">{t("ui.pages.protect.select.a.destination.vault.to.configure.its.engine")}</p>}
                                    {selectedEngines.map((engine) => (
                                        <section className="engine-settings-group" key={engine}>
                                            <h3>{engine[0].toUpperCase() + engine.slice(1)}</h3>
											<label className="field"><span>{t("ui.pages.protect.advanced.cli.options")}</span><textarea className="mono" rows={3} value={jobForm.engineOptions[engine] ?? ""} placeholder={examplePlaceholder(engine === "kopia" ? "--fail-fast" : "--verbose")} onChange={(event) => setJobForm({ ...jobForm, engineOptions: { ...jobForm.engineOptions, [engine]: event.target.value } })} /><small>{t("ui.pages.protect.one.option.per.line.each.line.is.tokenized.as.cli.options.so.a.flag.an")}</small></label>
                                        </section>
                                    ))}
								</div>
								<JobScriptFields value={jobForm} onChange={(scripts) => setJobForm({ ...jobForm, ...scripts })} />
                            </div>
                        )}
						{jobModal === "new" && <label className="check"><input type="checkbox" checked={jobForm.enabled} onChange={(event) => setJobForm({ ...jobForm, enabled: event.target.checked })} />{t("ui.pages.protect.enabled.the.scheduler.will.run.this.job")}</label>}
                    </div>
						<div className="modal-footer"><button className="btn" disabled={jobSaving} onClick={closeJob}>{t("ui.pages.protect.cancel")}</button><button className="btn primary" disabled={jobSaving} onClick={() => void saveJob()}>{jobSaving && <span className="spinner" />}{jobModal === "new" ? t("ui.pages.protect.create.job") : t("ui.pages.protect.save.changes")}</button></div>
					</fieldset>
	                </Modal>
	            )}

			{jobCreationResults && <Modal title={t("ui.pages.protect.backup.job.creation.results")} onClose={dismissJobCreationResults}>
				<p className="muted modal-intro">{t("ui.pages.protect.each.source.got.its.own.job.these.are.independent.jobs.that.you.can.ed")}</p>
				<div className="mutation-results">{jobCreationResults.map((item, index) => <div className={`mutation-result ${item.status}`} key={`${item.name}:${index}`}>
					<strong>{item.name}</strong>
					<span>{jobMutationStatusLabel(item.status)}</span>
					{item.source && <small className="mono mutation-result-source">{t("ui.protect.sourcePath", { path: item.source })}</small>}
					{item.message && <small>{item.message}</small>}
				</div>)}</div>
				<div className="modal-footer">
					{jobCreationResults.some((item) => item.status === "failed") && <button className="btn" disabled={jobCreationRetrying} onClick={() => void retryFailedJobCreations()}>{jobCreationRetrying && <span className="spinner" />}{t("ui.pages.protect.retry.failed.jobs")}</button>}
					<button className="btn primary" disabled={jobCreationRetrying} onClick={dismissJobCreationResults}>{t("ui.pages.protect.close")}</button>
				</div>
			</Modal>}

			{bulkEditOpen && <Modal title={bulkEditStep === "edit" ? t("ui.protect.bulkEditJobs") : t("ui.protect.reviewBulkChanges")} wide onClose={dismissBulkEdit}>
				{bulkEditStep === "edit" ? <fieldset className="modal-workflow-fields" disabled={bulkSaving}>
					<p className="muted modal-intro">{t("ui.protect.bulkSelectedJobsHelp", { count: selectedJobs.length })}</p>
					<div className="modal-form bulk-edit-form">
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.changeSchedule} onChange={(event) => setBulkForm({ ...bulkForm, changeSchedule: event.target.checked })} />{t("ui.pages.protect.change.schedule")}</label>
							{bulkForm.changeSchedule && <div className="bulk-setting-fields"><label className="field"><span>{t("ui.pages.protect.schedule")}</span><select aria-label={t("ui.pages.protect.schedule")} value={bulkForm.schedule} onChange={(event) => setBulkForm({ ...bulkForm, schedule: event.target.value })}>{schedulePresets.map(([value, label]) => <option key={value} value={value}>{label()}</option>)}</select><small>{scheduleHelp(bulkForm.schedule)}</small></label>{customScheduleUnits[bulkForm.schedule] && <label className="field compact-field"><span>{customScheduleUnits[bulkForm.schedule]!.label()}</span><input type="number" min={1} max={customScheduleUnits[bulkForm.schedule]!.max} value={bulkForm.customInterval} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, customInterval: event.target.value })} /></label>}{bulkForm.schedule === "cron" && <label className="field"><span>{t("ui.pages.protect.cron.expression")}</span><input className="mono" value={bulkForm.cronExpression} placeholder="0 2 * * *" maxLength={MAX_CRON_EXPRESSION_LENGTH} onChange={(event) => setBulkForm({ ...bulkForm, cronExpression: event.target.value })} /><small>{cronScheduleHelp()}</small></label>}</div>}
						</section>
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.changeRetention} onChange={(event) => setBulkForm({ ...bulkForm, changeRetention: event.target.checked })} />{t("ui.pages.protect.change.retention")}</label>
							{bulkForm.changeRetention && <div className="bulk-setting-fields"><label className="field"><span>{t("ui.pages.protect.retention.policy")}</span><select aria-label={t("ui.pages.protect.bulk.retention.policy")} value={retentionPreset(bulkForm.retention)} onChange={(event) => setBulkForm({ ...bulkForm, retention: event.target.value === "custom" ? (retentionPreset(bulkForm.retention) === "custom" ? bulkForm.retention : "") : event.target.value })}>{retentionPresets.map(([value, label]) => <option key={value} value={value}>{label()}</option>)}</select></label>{retentionPreset(bulkForm.retention) === "custom" && <label className="field compact-field"><span>{t("ui.pages.protect.number.of.latest.snapshots.to.keep")}</span><input type="number" min={1} max={MAX_MAIN_RETENTION_COUNT} value={bulkForm.retention} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retention: event.target.value })} /></label>}<div className="form-grid two"><label className="field"><span>{t("ui.pages.protect.keep.n.latest.hourly.snapshots")}</span><input type="number" min={1} max={MAX_RETENTION_COUNT} value={bulkForm.retentionHourly} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retentionHourly: event.target.value })} /></label><label className="field"><span>{t("ui.pages.protect.keep.n.latest.daily.snapshots")}</span><input type="number" min={1} max={MAX_RETENTION_COUNT} value={bulkForm.retentionDaily} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retentionDaily: event.target.value })} /></label><label className="field"><span>{t("ui.pages.protect.keep.n.latest.weekly.snapshots")}</span><input type="number" min={1} max={MAX_RETENTION_COUNT} value={bulkForm.retentionWeekly} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retentionWeekly: event.target.value })} /></label><label className="field"><span>{t("ui.pages.protect.keep.n.latest.monthly.snapshots")}</span><input type="number" min={1} max={MAX_RETENTION_COUNT} value={bulkForm.retentionMonthly} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retentionMonthly: event.target.value })} /></label><label className="field"><span>{t("ui.pages.protect.keep.n.latest.yearly.snapshots")}</span><input type="number" min={1} max={MAX_RETENTION_COUNT} value={bulkForm.retentionYearly} onWheel={preventNumberInputWheel} onChange={(event) => setBulkForm({ ...bulkForm, retentionYearly: event.target.value })} /></label></div></div>}
						</section>
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.changeExcludes} onChange={(event) => setBulkForm({ ...bulkForm, changeExcludes: event.target.checked })} />{t("ui.pages.protect.change.exclude.patterns")}</label>
							{bulkForm.changeExcludes && <div className="bulk-setting-fields"><label className="field"><span>{t("ui.pages.protect.exclude.patterns.optional")}</span><textarea className="mono" rows={3} value={bulkForm.excludes} placeholder={`${examplePlaceholder("*.tmp")}\n${examplePlaceholder("node_modules")}`} onChange={(event) => setBulkForm({ ...bulkForm, excludes: event.target.value })} /><small>{t("ui.pages.protect.one.pattern.per.line.for.files.folders.these.jobs.should.skip")}</small></label></div>}
						</section>
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.changeTag} onChange={(event) => setBulkForm({ ...bulkForm, changeTag: event.target.checked })} />{t("ui.pages.protect.change.tag")}</label>
							{bulkForm.changeTag && <div className="bulk-setting-fields"><label className="field"><span>{t("ui.pages.protect.tag.optional")}</span><input value={bulkForm.tag} onChange={(event) => setBulkForm({ ...bulkForm, tag: event.target.value })} /><small>{t("ui.pages.protect.optional.label.applied.to.snapshots.created.by.these.jobs")}</small></label></div>}
						</section>
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.changeScripts} onChange={(event) => setBulkForm({ ...bulkForm, changeScripts: event.target.checked })} />{t("ui.pages.protect.change.scripts")}</label>
							{bulkForm.changeScripts && <div className="bulk-setting-fields"><JobScriptFields value={bulkForm} onChange={(scripts) => setBulkForm({ ...bulkForm, ...scripts })} /></div>}
						</section>
						<section className="bulk-setting">
							<label className="check"><input type="checkbox" checked={bulkForm.replaceDestinations} onChange={(event) => setBulkForm({ ...bulkForm, replaceDestinations: event.target.checked, repositoryIds: event.target.checked ? bulkForm.repositoryIds : [], engineOptions: event.target.checked ? bulkForm.engineOptions : {} })} />{t("ui.pages.protect.replace.destination.vaults")}</label>
							{bulkForm.replaceDestinations && <div className="bulk-setting-fields"><div className="field"><span>{t("ui.pages.protect.destination.vaults")}</span><DestinationVaultPicker repositories={repos ?? []} selectedIds={bulkForm.repositoryIds} onChange={(repositoryIds) => setBulkForm({ ...bulkForm, repositoryIds })} /><small>{t("ui.pages.protect.selected.vaults.will.replace.the.complete.destination.list.for.every.s")}</small></div><div className="engine-settings"><div className="engine-settings-heading"><strong>{t("ui.pages.protect.engine.specific.settings.optional")}</strong><small>{t("ui.pages.protect.these.options.replace.the.active.engine.options.for.every.selected.job")}</small></div>{bulkSelectedEngines.map((engine) => <section className="engine-settings-group" key={engine}><h3>{engine[0].toUpperCase() + engine.slice(1)}</h3><label className="field"><span>{t("ui.pages.protect.advanced.cli.options")}</span><textarea className="mono" rows={3} value={bulkForm.engineOptions[engine] ?? ""} placeholder={examplePlaceholder(engine === "kopia" ? "--fail-fast" : "--verbose")} onChange={(event) => setBulkForm({ ...bulkForm, engineOptions: { ...bulkForm.engineOptions, [engine]: event.target.value } })} /><small>{t("ui.pages.protect.one.option.per.line.each.line.is.tokenized.as.cli.options.so.a.flag.an")}</small></label></section>)}</div></div>}
						</section>
					</div>
					<div className="modal-footer"><button className="btn" onClick={dismissBulkEdit}>{t("ui.pages.protect.cancel")}</button><button className="btn primary" onClick={reviewBulkEdit}>{t("ui.pages.protect.review.changes")}</button></div>
				</fieldset> : <fieldset className="modal-workflow-fields" disabled={bulkSaving}>
					<p className="muted modal-intro">{t("ui.protect.applyBulkChangesQuestion", { count: selectedJobs.length })}</p>
					<div className="bulk-review">
						<div><strong>{t("ui.pages.protect.jobs")}</strong><span>{selectedJobs.map((job) => job.name).join(", ")}</span></div>
						<div><strong>{t("ui.pages.protect.settings")}</strong><span>{[
							bulkForm.changeSchedule && t("ui.pages.protect.schedule"),
							bulkForm.changeRetention && t("ui.pages.protect.retention"),
							bulkForm.changeExcludes && t("ui.pages.protect.exclude.patterns"),
							bulkForm.changeTag && t("ui.pages.protect.tag"),
							bulkForm.changeScripts && t("ui.pages.protect.scripts"),
							bulkForm.replaceDestinations && t("ui.protect.destinationVaultsAndEngineOptions"),
						].filter(Boolean).join(", ")}</span></div>
						{bulkForm.changeSchedule && <div><strong>{t("ui.pages.protect.schedule")}</strong><span>{scheduleLabel(scheduleValue(bulkForm) ?? "")}</span></div>}
						{bulkForm.changeRetention && <div><strong>{t("ui.pages.protect.retention")}</strong><span>{retentionReviewSummary(bulkForm)}</span></div>}
						{bulkForm.changeExcludes && <div><strong>{t("ui.pages.protect.exclude.patterns")}</strong><span className="mono review-verbatim">{bulkForm.excludes || t("ui.protect.none")}</span></div>}
						{bulkForm.changeTag && <div><strong>{t("ui.pages.protect.tag")}</strong><span>{bulkForm.tag.trim() || t("ui.protect.none")}</span></div>}
						{bulkForm.changeScripts && <div><strong>{t("ui.pages.protect.scripts")}</strong><span>{t("ui.protect.bulkScriptSummary", { before: bulkForm.beforeScriptPath || t("ui.protect.none"), beforeRequirement: bulkForm.beforeScriptMustSucceed ? t("ui.protect.mustSucceed") : "", after: bulkForm.afterScriptPath || t("ui.protect.none"), afterRequirement: bulkForm.afterScriptMustSucceed ? t("ui.protect.mustSucceed") : "" })}</span></div>}
						{bulkForm.replaceDestinations && <div><strong>{t("ui.pages.protect.destination.vaults")}</strong><span>{bulkForm.repositoryIds.map((id) => repos?.find((repo) => repo.id === id)?.name ?? id).join(", ")} · {t("ui.protect.alreadyIdentical", { count: selectedJobs.filter((job) => sameStringSet(job.targets.map((target) => target.repositoryId), bulkForm.repositoryIds)).length })}</span></div>}
						{bulkForm.replaceDestinations && <div><strong>{t("ui.pages.protect.advanced.engine.options")}</strong><span className="mono review-verbatim">{bulkSelectedEngines.map((engine) => `${engine}:\n${parsedEngineOptions(bulkForm.engineOptions[engine] ?? "").join("\n") || t("ui.protect.none")}`).join("\n\n")}</span></div>}
						{bulkForm.changeScripts && <div><strong>{t("ui.pages.protect.script.executions")}</strong><span>{t("ui.protect.scriptExecutionsAfterChange", { count: selectedJobs.reduce((count, job) => count + (bulkForm.replaceDestinations ? bulkForm.repositoryIds.length : job.targets.length), 0) })}</span></div>}
					</div>
					<div className="modal-footer"><button className="btn" disabled={bulkSaving} onClick={() => setBulkEditStep("edit")}>{t("ui.pages.protect.back")}</button><button className="btn primary" disabled={bulkSaving} onClick={() => void applyBulkEdit()}>{bulkSaving && <span className="spinner" />}{t("ui.pages.protect.apply.changes")}</button></div>
				</fieldset>}
			</Modal>}

			{bulkResults && <Modal title={t("ui.pages.protect.bulk.edit.results")} onClose={dismissBulkResults}>
				<p className="muted modal-intro">{t("ui.pages.protect.every.selected.job.is.listed.separately")}</p>
				<div className="mutation-results">{bulkResults.items.map((item) => <div className={`mutation-result ${item.status}`} key={item.jobId ?? item.name}>
					<strong>{item.name}</strong>
					<span>{jobMutationStatusLabel(item.status)}</span>
					{item.message && <small>{item.message}</small>}
				</div>)}</div>
				<div className="modal-footer">
					{bulkResults.items.some((item) => item.status === "failed") && <button className="btn" disabled={bulkRetrying} onClick={() => void retryFailedBulkJobs()}>{bulkRetrying && <span className="spinner" />}{t("ui.pages.protect.retry.failed.jobs")}</button>}
					<button className="btn primary" disabled={bulkRetrying} onClick={dismissBulkResults}>{t("ui.pages.protect.close")}</button>
				</div>
			</Modal>}

				{sourceUpdateJob && <UpdateJobSourceDialog
					job={sourceUpdateJob}
					onClose={() => setSourceUpdateJob(null)}
					onError={(message) => toast("error", message)}
					onSaved={(saved) => {
						setSourceUpdateJob(null);
						applySavedJob(saved);
						load();
					}}
				/>}
				{jobDelete && <ConfirmDialog title={t("ui.protect.deleteNamedJobQuestion", { name: jobDelete.name })} message={t("ui.pages.protect.its.run.history.is.kept.but.no.further.backups.will.run.existing.backu")} confirmLabel={t("ui.pages.protect.delete.job")} onConfirm={() => void removeJob()} onCancel={dismissJobDelete} />}
            {selectedRunJob && (
                <Modal title={t("ui.protect.runNamedJob", { name: selectedRunJob.name })} onClose={dismissRunReview}>
					{runResults ? <article aria-label={t("ui.pages.protect.manual.backup.admission.results")}>
						<p className="muted modal-intro">{t("ui.pages.protect.every.requested.destination.is.listed.separately")}</p>
						<div className="unowned-snapshots">{runResults.map((result) => {
							const target = selectedRunJob.targets.find((candidate) => candidate.repositoryId === result.repositoryId);
							return <div className="unowned-snapshot" key={result.repositoryId}>
								<strong>{target?.repositoryName || result.repositoryId}</strong>
								<small>{manualRunResultText(result)}</small>
							</div>;
						})}</div>
					</article> : <>
						<p className="muted modal-intro">{t("ui.pages.protect.choose.which.destination.vault.to.back.up.to")}</p>
						<BackupRunPicker job={selectedRunJob} busy={runBusy} onRun={(repositoryId) => void startRun(selectedRunJob, repositoryId)} />
					</>}
                </Modal>
            )}

            {showVaultChoice && (
				<Modal title={t("ui.pages.protect.add.a.vault")} onClose={() => setShowVaultChoice(false)}>
					<div className="vault-choice-grid">
						<button className="vault-choice" onClick={() => openVaultCreate()}><Icon name="plus" size={22} /><span><strong>{t("ui.pages.protect.create.new.vault")}</strong><small>{t("ui.pages.protect.create.a.new.encrypted.backup.destination")}</small></span></button>
						<button className="vault-choice" onClick={() => { resetConnectWorkflow(); setShowVaultChoice(false); setShowVaultConnect(true); refreshConnectionIntents(); }}><Icon name="restore" size={22} /><span><strong>{t("ui.pages.protect.connect.existing.vault")}</strong><small>{t("ui.protect.connectExistingHelp")}<br /><br />{t("ui.protect.importNativeVaultHelp")}</small></span></button>
					</div>
				</Modal>
			)}

			{showVaultConnect && !connectRcloneAuthorizationAction && (
				<Modal title={t("ui.pages.protect.connect.existing.vault")} wide onClose={() => { if (!connectSaving) { resetConnectWorkflow(); setShowVaultConnect(false); } }}>
					<fieldset className="modal-form modal-workflow-fields" disabled={connectSaving}>
						{connectForm.connector === RCLONE_REMOTE_CONNECTOR && !connectForm.coldStorage && <RcloneRemoteWarning />}
						{/* Discovery inputs are intentionally unmounted after a successful
						    preview so credentials and already-reviewed destination details do
						    not compete with the attachment decisions. Check another vault
						    restores a clean discovery form. */}
						{!connectPreview && <>
							<p className="section-copy">{t("ui.protect.enterExistingDetailsHelp")}</p>
						<label className="field"><span>{t("ui.pages.protect.storage.type")}</span><select value={connectForm.coldStorage ? "cold_s3" : connectForm.connector} disabled={Boolean(connectPreview || retryConnectionIntentId)} onChange={(event) => { if (connectRcloneAuth?.sessionId) void closeRcloneAuthorization(connectRcloneAuth.sessionId); setConnectRcloneAuth(null); const coldStorage = event.target.value === "cold_s3"; const connector = coldStorage ? "s3" : event.target.value; const selected = connectionIntegrations.find((item) => item.id === connector); invalidateConnectPreview(); setConnectForm((current) => ({ ...current, engine: coldStorage ? "restic" : current.engine, connector, coldStorage, archiveWriteClass: "GLACIER", checkSchedule: coldStorage ? "manual" : current.checkSchedule, location: "", pendingLocation: "", options: integrationDefaults(selected), objectLock: emptyObjectLock() })); }}>{connectionIntegrations.flatMap((item) => [<option key={item.id} value={item.id}>{knownMessage(`ui.integration.${item.id}.label`, item.label)}</option>, ...(item.id === "s3" ? [<option key="cold_s3" value="cold_s3">{t("ui.pages.protect.cold.storage.must.be.s3.compatible")}</option>] : [])])}</select>{connectIntegration && <small>{connectForm.coldStorage ? t("ui.protect.coldS3CompatibilityHelp", { glacier: "GLACIER", deepArchive: "DEEP_ARCHIVE" }) : connectIntegrationDescription}</small>}</label>
						{(!connectPreview || !connectUsesRcloneSignIn) && (connectIntegration?.localBrowser ? <label className="field"><span>{t("ui.pages.protect.location")}</span><DirectoryField value={connectForm.location} disabled={Boolean(retryConnectionIntentId)} placeholder={examplePlaceholder(connectIntegration.placeholder)} onChange={(location) => { invalidateConnectPreview(); setConnectForm((current) => ({ ...current, location, pendingLocation: "" })); }} /></label> : connectIntegration ? <RemoteVaultFields form={connectForm} integration={connectIntegration} onChange={(next) => { if (!retryConnectionIntentId) invalidateConnectPreview(); setConnectForm(next); }} /> : null)}
						{!connectUsesRcloneSignIn && connectOrderedOptions.filter((option) => !option.advanced && !connectionOptionIsCustom(connectForm.connector, option.key)).map((option) => <IntegrationField key={option.key} connector={connectForm.connector} option={option} value={connectForm.options[option.key] ?? ""} disabled={Boolean(retryConnectionIntentId && !option.credential && !option.secret)} onChange={(value) => { if (!retryConnectionIntentId) invalidateConnectPreview(option.key === "storage_class" && connectDetectedEngine === "restic"); setConnectForm((current) => updateConnectorOption(current, option.key, value)); }} />)}
							{(!connectPreview || !connectUsesRcloneSignIn) && <label className="field"><span>{t("ui.pages.protect.vault.encryption.password")}</span><input type="password" value={connectForm.password} onChange={(event) => { if (!retryConnectionIntentId) invalidateConnectPreview(); setConnectForm((current) => ({ ...current, password: event.target.value })); }} /><small>{t("ui.pages.protect.the.password.is.used.to.decrypt.and.validate.the.vault.the.password.is")}</small></label>}
						{matchingConnectionIntent && <section className="recovery-warning recovery-fallback-message" role="status">
							<p><strong>{retryConnectionError ? t("ui.protect.previousConnectionIncomplete") : matchingConnectionIntent.state === "prepared" ? t("ui.protect.earlierConnectionSaved") : t("ui.protect.unfinishedConnectionFound")}</strong></p>
							<p>{retryConnectionError ? t("ui.protect.savedAttemptAvailable") : matchingConnectionIntent.state === "prepared" ? t("ui.protect.prePublicationAttemptHelp") : t("ui.protect.profileUpdatedAttemptHelp")}</p>
							{retryConnectionError && <p>{retryConnectionError}</p>}
							<div className="vault-removal-actions">{!retryConnectionIntentId && <button className="btn primary" disabled={connectChecking || connectSaving} onClick={() => selectConnectionIntent(matchingConnectionIntent)}>{t("ui.pages.protect.continue.previous.connection")}</button>}{matchingConnectionIntent.state === "prepared" && <button className="btn" disabled={connectChecking || connectSaving || !validVaultPassword(connectForm.password) || connectMissingRequiredOptions.length > 0} onClick={() => void startNewConnectionCheck(matchingConnectionIntent)}>{t("ui.pages.protect.start.a.new.check")}</button>}</div>
						</section>}
						{connectForm.coldStorage && <div className="recovery-warning">{coldStorageProviderGuidance().map((paragraph) => <p key={paragraph}>{paragraph}</p>)}</div>}
						{!connectUsesRcloneSignIn && connectOrderedOptions.some((option) => option.advanced) && <button className="btn advanced-toggle" onClick={() => setConnectAdvanced((value) => !value)}><Icon name="settings" size={14} />{connectAdvanced ? t("ui.pages.protect.hide.advanced.settings") : t("ui.pages.protect.advanced.settings")}</button>}
						{!connectUsesRcloneSignIn && connectAdvanced && <div className="advanced-panel">{connectOrderedOptions.filter((option) => option.advanced && !connectionOptionIsCustom(connectForm.connector, option.key)).map((option) => <IntegrationField key={option.key} connector={connectForm.connector} option={option} value={connectForm.options[option.key] ?? ""} disabled={Boolean(retryConnectionIntentId && !option.credential && !option.secret)} onChange={(value) => { if (!retryConnectionIntentId) invalidateConnectPreview(option.key === "storage_class" && connectDetectedEngine === "restic"); setConnectForm((current) => updateConnectorOption(current, option.key, value)); }} />)}</div>}
						<div className={`modal-footer${connectForm.connector === RCLONE_REMOTE_CONNECTOR ? " rclone-remote-dialog-footer" : ""}`}><button className="btn" disabled={connectSaving} onClick={() => { if (!connectSaving) { resetConnectWorkflow(); setShowVaultConnect(false); } }}>{t("ui.pages.protect.cancel")}</button>
										{matchingConnectionIntent && retryConnectionIntentId ? <button className="btn primary" disabled={connectChecking || connectSaving || !validVaultPassword(connectForm.password) || connectMissingRequiredOptions.length > 0} onClick={() => void retryPendingConnection()}>{connectSaving && <span className="spinner" />}{retryConnectionError ? t("ui.protect.retryConnection") : t("ui.protect.continuePreviousConnection")}</button> : (!matchingConnectionIntent || canCheckAnotherRcloneAccount) && <button className="btn primary" disabled={connectChecking || !vaultLocation(connectForm) || !validVaultPassword(connectForm.password) || connectMissingRequiredOptions.length > 0} onClick={() => { if (connectUsesRcloneSignIn) setConnectRcloneAuthorizationAction("check"); else void checkExistingVault(); }}>{connectChecking && <span className="spinner" />}{t("ui.pages.protect.check.existing.vault")}</button>}
						</div>
						</>}
						{connectPreview && <>
							{connectPreview.existingVault ? <div className="recovery-warning recovery-fallback-message">
									<p><strong>{t("ui.protect.registeredVaultFound", { vault: connectPreview.existingVault.name })}</strong></p>
									<p>{t("ui.pages.protect.this.is.the.same.protected.vault.uuid.as.the.existing.registration.rep")}</p>
									<p>{t("ui.pages.protect.all.current.local.backup.jobs.job.uuids.sources.target.memberships.and")}</p>
								</div> : connectPreview.mode === "fallback" ? <div className="recovery-warning recovery-fallback-message">
									<p>{t("ui.protect.nativeVaultFoundImportHelp", { engine: connectPreview.engine[0].toUpperCase() + connectPreview.engine.slice(1) })}</p>
									<p>{t("ui.pages.protect.replicaro.allows.you.to.browse.restore.and.delete.snapshots.that.alrea")}</p>
									<p>{t("ui.protect.importedJobsHelp", { engine: connectPreview.engine[0].toUpperCase() + connectPreview.engine.slice(1) })}</p>
									<p>{t("ui.protect.disableOtherWritersHelp", { engine: connectPreview.engine[0].toUpperCase() + connectPreview.engine.slice(1) })}</p>
								</div> : <div className="recovery-warning recovery-fallback-message">
									<p>{t("ui.protect.existingReplicaroEngine", { engine: connectPreview.engine[0].toUpperCase() + connectPreview.engine.slice(1) })}</p>
									{!connectProfileUUID && <p>{connectProfileChoiceMade ? t("ui.protect.joinNewProfileHelp") : t("ui.protect.chooseProfileHelp")}</p>}
									{connectProfileUUID && <p>{t("ui.protect.importSelectedProfileHelp")}</p>}
									{!connectProfileUUID && <p>{connectAccessReminder()}</p>}
								</div>}
							{connectPreview.profileDegraded && <p className="recovery-warning">{t("ui.pages.protect.the.canonical.recovery.profile.is.damaged.or.missing.a.verified.previo")}</p>}
							{connectProfileSelectionVisible && <fieldset className="connection-decision field multi-computer-field" aria-busy={Boolean(connectPendingProfileChoice)}>
								<legend>{t("ui.protect.profileChoiceLegend")}</legend>
								<div className="radio-options">
									<label><input type="radio" name="connect-profile" disabled={connectChecking} checked={connectPendingProfileChoice ? connectPendingProfileChoice.join : connectProfileChoiceMade && connectProfileAction === "join"} onChange={() => void checkExistingVault("", true)} />{t("ui.pages.protect.join.vault.as.new.profile")}</label>
									{(connectPreview.profiles ?? []).map((profile) => <label key={profile.profile_uuid}><input type="radio" name="connect-profile" disabled={connectChecking} checked={connectPendingProfileChoice ? !connectPendingProfileChoice.join && connectPendingProfileChoice.profileUUID === profile.profile_uuid : connectProfileUUID === profile.profile_uuid} onChange={() => void checkExistingVault(profile.profile_uuid)} />{(profile.vaultOwner ? t("ui.protect.ownerProfileChoice", { computer: profile.attachment.display.computerName || t("ui.protect.unknownComputer"), os: profile.attachment.display.operatingSystem || t("ui.protect.unknown"), created: profileDateLabel(profile.createdAt) }) : t("ui.protect.profileChoice", { computer: profile.attachment.display.computerName || t("ui.protect.unknownComputer"), os: profile.attachment.display.operatingSystem || t("ui.protect.unknown"), created: profileDateLabel(profile.createdAt) }))}</label>)}
								</div>
								{connectPendingProfileChoice && <p className="connection-profile-loading" role="status" aria-live="polite"><span className="spinner" aria-hidden="true" />{connectPendingProfileChoice.join ? t("ui.protect.creatingProfileStatus") : t("ui.protect.grabbingProfileStatus")}</p>}
								{/* Vault ownership follows the selected owner profile. Asking for a
								    second owner decision would suggest a separate root transfer that
								    this attachment transition neither needs nor performs. */}
								{selectedProfileIsOwner && <p className="connection-decision-note">{t("ui.protect.ownerProfileTakeoverHelp", { owner: currentOwnerName })}</p>}
							</fieldset>}
							{connectPreview.mode === "profile" && !connectPreview.existingVault && selectedConnectProfile?.localAttachment && <p className="connection-decision-note">{t("ui.pages.protect.this.computer.already.has.a.profile.in.this.vault.so.replicaro.will.re")}</p>}
							{connectPreview.mode === "profile" && !connectPreview.existingVault && connectProfileChoiceMade && !selectedProfileIsOwner && <fieldset className="connection-decision field multi-computer-field">
								<legend>{t("ui.protect.takeoverChoiceLegend", { owner: currentOwnerName })}</legend>
								<div className="radio-options">
									<label><input type="radio" name="connect-owner" checked={!selectedProfileIsOwner && connectOwnerChoiceMade && connectOwnerAction === "keep"} disabled={connectChecking || selectedProfileIsOwner} onChange={() => {
										// Relocking an owner-only field must restore the reviewed root value;
										// disabled controls still remain part of the connection payload.
										setConnectForm((current) => ({ ...current, checkSchedule: current.coldStorage ? "manual" : connectReviewedIntegritySchedule, maintenanceSchedule: connectReviewedMaintenanceSchedule }));
										setConnectOwnerAction("keep");
										setConnectOwnerChoiceMade(true);
									}} />{t("ui.protect.leaveCurrentOwner", { owner: currentOwnerName })}</label>
									<label><input type="radio" name="connect-owner" checked={selectedProfileIsOwner || connectOwnerChoiceMade && connectOwnerAction === "takeover"} disabled={connectChecking || selectedProfileIsOwner} onChange={() => { setConnectOwnerAction("takeover"); setConnectOwnerChoiceMade(true); }} />{t("ui.pages.protect.yes.make.me.vault.owner")}</label>
								</div>
							</fieldset>}
							{connectNameConflictNotice && <div className="recovery-warning recovery-fallback-message"><p>{connectNameConflictNotice}</p></div>}
							{connectPreview.mode === "fallback" && connectPreview.objectLockEnrollmentAvailable && <ObjectLockFields form={connectForm} creation onChange={setConnectForm} />}
							<div className="form-grid two"><label className="field"><span>{t("ui.pages.protect.vault.name")}</span><input aria-label={t("ui.pages.protect.vault.name")} disabled={connectUsesVaultFolder} value={connectForm.name} onChange={(event) => setConnectForm({ ...connectForm, name: event.target.value })} /><small>{connectUsesVaultFolder ? connectForm.connector === RCLONE_REMOTE_CONNECTOR ? t("ui.protect.rcloneRemoteNameImmutableHelp") : t("ui.protect.rcloneNameImmutableHelp", { provider: connectIntegration ? knownMessage(`ui.integration.${connectIntegration.id}.label`, connectIntegration.label) : "" }) : connectPreview.existingVault ? t("ui.protect.reviewSavedName") : t("ui.protect.nameImmutableHelp")}</small></label><label className="field"><span>{t("ui.pages.protect.description.optional")}</span><input value={connectForm.description} onChange={(event) => setConnectForm({ ...connectForm, description: event.target.value })} /></label></div>
							<VaultCareFields form={connectForm} integrityDisabled={connectIntegrityLocked} maintenanceDisabled={connectMaintenanceLocked} integrityLockedHelp={connectIntegrityLockedHelp} maintenanceLockedHelp={connectMaintenanceLockedHelp} onChange={setConnectForm} />
							{connectPreview.existingVault && <fieldset className={`connection-decision field${connectReviewedRcloneAddress ? " rclone-remote-location-review" : ""}`}>
								<legend>{t("ui.pages.protect.exact.update.existing.vault.changes")}</legend>
								<ul>{existingVaultUpdateChanges.map((change) => <li key={change}>{change}</li>)}</ul>
								<label><input type="checkbox" checked={connectUpdateConfirmedDigest === connectUpdateReviewDigest} onChange={(event) => setConnectUpdateConfirmedDigest(event.target.checked ? connectUpdateReviewDigest : "")} />{t("ui.pages.protect.update.this.existing.vault.registration.with.exactly.these.reviewed.ch")}</label>
							</fieldset>}
						</>}
					</fieldset>
					{connectPreview && <div className={`modal-footer${connectForm.connector === RCLONE_REMOTE_CONNECTOR ? " rclone-remote-dialog-footer" : ""}`}><button className="btn" disabled={connectSaving} onClick={() => { if (!connectSaving) { resetConnectWorkflow(); setShowVaultConnect(false); } }}>{t("ui.pages.protect.cancel")}</button><button className="btn" disabled={connectSaving} onClick={resetConnectForNewAttempt}>{t("ui.pages.protect.check.another.vault")}</button><button className="btn primary" disabled={connectSaving || connectChecking || connectRcloneNameConflict || !validVaultName(connectForm.name) || !connectProfileReady || !connectOwnerReady || Boolean(connectPreview.existingVault && connectUpdateConfirmedDigest !== connectUpdateReviewDigest) || !validObjectLockSettings(connectForm.objectLock, connectForm.maintenanceSchedule, connectPreview.mode === "profile")} onClick={() => void saveExistingVault()}>{connectSaving && <span className="spinner" />}{connectPreview.existingVault ? t("ui.protect.updateExistingVault") : t("ui.protect.connectVault")}</button></div>}
					{connectSaving && <KeepWindowOpenNotice />}
					{(connectChecking || connectSaving) && <VaultActivityLog records={vaultProgress} />}
				</Modal>
			)}

			{showVaultConnect && connectRcloneAuthorizationAction && connectIntegration && (
				<Modal title={t("ui.rclone.connectProvider", { provider: knownMessage(`ui.integration.${connectIntegration.id}.label`, connectIntegration.label) })} onClose={closeConnectRcloneAuthorization}>
					<div className="modal-form">
						<p>{renderMessage("ui.rclone.authorizationDescription", { provider: knownMessage(`ui.integration.${connectIntegration.id}.label`, connectIntegration.label), rcloneLink: <a href="https://github.com/rclone/rclone" target="_blank" rel="noreferrer">{t("ui.pages.protect.rclone")}</a> })}</p>
						<RcloneAuthorization provider={connectForm.connector} label={knownMessage(`ui.integration.${connectIntegration.id}.label`, connectIntegration.label)} readyAction="check existing vault" value={connectRcloneAuth} disabled={connectChecking || connectSaving} closeOnUnmount={false} showDescription={false} onChange={setConnectRcloneAuth} onError={(message) => toast("error", message)} />
					</div>
					<div className="modal-footer">
						<button className="btn" disabled={connectChecking || connectSaving} onClick={closeConnectRcloneAuthorization}>{t("ui.pages.protect.back")}</button>
						{connectRcloneAuth?.status === "ready" && <button className="btn primary" disabled={connectChecking || connectSaving} onClick={() => {
							if (connectRcloneAuthorizationAction === "retry") void retryPendingConnection();
							else void checkExistingVault().then((succeeded) => { if (succeeded) setConnectRcloneAuthorizationAction(null); });
						}}>{(connectChecking || connectSaving) && <span className="spinner" />}{connectRcloneAuthorizationAction === "retry" ? t("ui.pages.protect.retry.connection") : t("ui.pages.protect.check.existing.vault")}</button>}
					</div>
					{connectSaving && <KeepWindowOpenNotice />}
					{(connectChecking || connectSaving) && <VaultActivityLog records={vaultProgress} />}
				</Modal>
			)}

            {showVaultCreate && !showCreateRcloneAuthorization && (
				<Modal title={t("ui.pages.protect.add.a.vault")} wide onClose={closeVaultCreate}>
					<fieldset className="modal-workflow-fields" disabled={vaultSaving}>
					<div className="modal-form">
						{createUsesRcloneRemote && <RcloneRemoteWarning />}
						<p className="section-copy">{t("ui.pages.protect.encryption.compression.and.deduplication.are.automatically.applied.to")}</p>
						<label className="field"><span>{t("ui.pages.protect.storage.type")}</span><select autoFocus value={vaultForm.coldStorage ? "cold_s3" : vaultForm.connector} disabled={Boolean(vaultForm.pendingLocation)} onChange={(event) => { if (createRcloneAuth?.sessionId) void closeRcloneAuthorization(createRcloneAuth.sessionId); setCreateRcloneAuth(null); setShowCreateRcloneAuthorization(false); const coldStorage = event.target.value === "cold_s3"; const connector = coldStorage ? "s3" : event.target.value; const selected = vaultStorageIntegrations.find((item) => item.id === connector); const eligible = engineCatalog.filter((descriptor) => descriptor.installed && descriptor.providers.some((provider) => provider.id === connector && provider.supported)); const engine = coldStorage ? "restic" : eligible.some((descriptor) => descriptor.id === vaultForm.engine) ? vaultForm.engine : (eligible.find((descriptor) => descriptor.id === "restic")?.id ?? eligible[0]?.id ?? vaultForm.engine) as VaultForm["engine"]; setVaultForm({ ...vaultForm, engine, connector, coldStorage, archiveWriteClass: "GLACIER", checkSchedule: coldStorage ? "manual" : vaultForm.checkSchedule, location: "", pendingLocation: "", bucket: "", container: "", prefix: "", host: "", options: integrationOptionsForEngine(selected, engine, engineCatalog), objectLock: objectLockForSelection(vaultForm.objectLock, engine, connector) }); }}>{vaultStorageIntegrations.flatMap((item) => [<option key={item.id} value={item.id}>{knownMessage(`ui.integration.${item.id}.label`, item.label)}</option>, ...(item.id === "s3" ? [<option key="cold_s3" value="cold_s3">{t("ui.pages.protect.cold.storage.must.be.s3.compatible")}</option>] : [])])}</select>{integration && <small>{vaultForm.coldStorage ? t("ui.pages.protect.supports.all.s3.compatible.cold.object.storage.that.accept", { glacier: "GLACIER", deepArchive: "DEEP_ARCHIVE" }) : createIntegrationDescription}</small>}</label>
						{!createUsesRcloneRemote && createVaultNameField}
							{createUsesRcloneSignIn && <><label className="field"><span>{t("ui.pages.protect.description.optional")}</span><input disabled={Boolean(vaultForm.pendingLocation)} value={vaultForm.description} onChange={(event) => setVaultForm({ ...vaultForm, description: event.target.value })} /></label><div className="form-grid two"><label className="field"><span>{t("ui.pages.protect.encryption.password")}</span><input type="password" value={vaultForm.password} onChange={(event) => setVaultForm({ ...vaultForm, password: event.target.value })} /><small>{vaultPasswordHelp()}</small></label><label className="field"><span>{t("ui.pages.protect.confirm.encryption.password")}</span><input type="password" value={vaultForm.passwordConfirmation} onChange={(event) => setVaultForm({ ...vaultForm, passwordConfirmation: event.target.value })} /></label></div>{vaultForm.password && vaultForm.passwordConfirmation && vaultForm.password !== vaultForm.passwordConfirmation && <small className="inline-error" role="alert">{t("ui.pages.protect.the.passwords.do.not.match")}</small>}</>}
						{integration?.localBrowser ? <label className="field"><span>{t("ui.pages.protect.location")}</span><DirectoryField value={vaultForm.location} disabled={Boolean(vaultForm.pendingLocation)} placeholder={examplePlaceholder(integration.placeholder)} onChange={(location) => setVaultForm({ ...vaultForm, location })} /></label> : integration && !createUsesRcloneSignIn ? <RemoteVaultFields form={vaultForm} integration={integration} vaultFolderField={false} onChange={setVaultForm} /> : null}
						{/* The vault name is the vault's folder name under the path in
						    remote, so it follows the remote settings. */}
						{createUsesRcloneRemote && createVaultNameField}
						{/* Connection has no archive-class choice: managed Cold vaults recover the
						    protected class, and native Cold imports start from the GLACIER default. */}
						{vaultCreateError.includes("connect existing vault") && <div className="recovery-warning"><p>{vaultCreateError}</p><button className="btn sm" onClick={() => { if (!closeVaultCreate()) return; invalidateConnectPreview(); setRetryConnectionIntentId(""); setConnectForm((current) => restoreVaultDestinationFields({ ...current, connector: vaultForm.connector, coldStorage: vaultForm.coldStorage, archiveWriteClass: "GLACIER", checkSchedule: vaultForm.coldStorage ? "manual" : current.checkSchedule, location: vaultLocation(vaultForm, true), options: { ...vaultForm.options }, password: vaultForm.password })); setShowVaultConnect(true); refreshConnectionIntents(); }}>{t("ui.pages.protect.connect.to.existing.vault")}</button></div>}
						{vaultCreateError.includes("selected location is not empty") && <div className="recovery-warning"><p>{vaultCreateError}</p><div className="tool-buttons"><button className="btn sm" onClick={() => { setVaultCreateError(""); setVaultForm({ ...vaultForm, location: "", pendingLocation: "", bucket: "", container: "", prefix: "", host: "" }); }}>{t("ui.pages.protect.select.empty.subfolder")}</button><button className="btn sm" onClick={() => { setVaultCreateError(""); setVaultForm({ ...vaultForm, location: "", pendingLocation: "", bucket: "", container: "", prefix: "", host: "" }); }}>{t("ui.pages.protect.select.different.destination")}</button></div></div>}
						{!createUsesRcloneSignIn && createOrderedOptions.filter((option) => !option.advanced && !creationOptionIsCustom(vaultForm.connector, option.key)).map((option) => <IntegrationField key={option.key} connector={vaultForm.connector} option={option} value={vaultForm.options[option.key] ?? ""} disabled={creationOptionLockedForPending(vaultForm, option)} onChange={(value) => setVaultForm(updateConnectorOption(vaultForm, option.key, value))} />)}
							{!createUsesRcloneSignIn && <><label className="field"><span>{t("ui.pages.protect.description.optional")}</span><input disabled={Boolean(vaultForm.pendingLocation)} value={vaultForm.description} onChange={(event) => setVaultForm({ ...vaultForm, description: event.target.value })} /></label><div className="form-grid two"><label className="field"><span>{t("ui.pages.protect.encryption.password")}</span><input type="password" value={vaultForm.password} onChange={(event) => setVaultForm({ ...vaultForm, password: event.target.value })} /><small>{vaultPasswordHelp()}</small></label><label className="field"><span>{t("ui.pages.protect.confirm.encryption.password")}</span><input type="password" value={vaultForm.passwordConfirmation} onChange={(event) => setVaultForm({ ...vaultForm, passwordConfirmation: event.target.value })} /></label></div>{vaultForm.password && vaultForm.passwordConfirmation && vaultForm.password !== vaultForm.passwordConfirmation && <small className="inline-error" role="alert">{t("ui.pages.protect.the.passwords.do.not.match")}</small>}</>}
						{vaultForm.coldStorage && <div className="recovery-warning">{coldStorageProviderGuidance().map((paragraph) => <p key={paragraph}>{paragraph}</p>)}</div>}
                        <button className="btn advanced-toggle" onClick={() => setVaultAdvanced((value) => !value)}><Icon name="settings" size={14} />{vaultAdvanced ? t("ui.pages.protect.hide.advanced.settings") : t("ui.pages.protect.advanced.settings")}</button>
						{vaultAdvanced && (
							<div className="advanced-panel">
								<ObjectLockFields form={vaultForm} creation creationAvailable={createObjectLockAvailable} disabled={Boolean(vaultForm.pendingLocation)} onChange={updateCreateObjectLock} />
								<label className="field"><span>{t("ui.pages.protect.vault.engine")}</span><select value={vaultForm.engine} disabled={vaultForm.objectLock.enrolled || vaultForm.coldStorage || createUsesVaultFolder || Boolean(vaultForm.pendingLocation)} onChange={(event) => { const engine = event.target.value as VaultForm["engine"]; const selected = vaultStorageIntegrations.find((item) => item.id === vaultForm.connector); setVaultForm({ ...vaultForm, engine, options: integrationOptionsForEngine(selected, engine, engineCatalog, vaultForm.options), objectLock: objectLockForSelection(vaultForm.objectLock, engine, vaultForm.connector) }); }}>{engineCatalog.filter((item) => vaultForm.coldStorage ? item.id === "restic" : !createUsesVaultFolder || item.id === "restic").map((item) => <option key={item.id} value={item.id} disabled={!item.installed || !item.providers.some((provider) => provider.id === vaultForm.connector && provider.supported)}>{item.name}</option>)}</select><small>{vaultForm.objectLock.enrolled ? t("ui.pages.protect.kopia.is.the.required.engine.when.object.locking.is") : vaultEngineHelp(vaultForm.connector, vaultForm.coldStorage)}</small></label>
								{vaultForm.coldStorage && <label className="field"><span>{t("ui.pages.protect.storage.class")}</span><select value={vaultForm.archiveWriteClass} disabled={Boolean(vaultForm.pendingLocation)} onChange={(event) => setVaultForm({ ...vaultForm, archiveWriteClass: event.target.value as VaultForm["archiveWriteClass"] })}><option value="GLACIER">{t("ui.pages.protect.glacier.default", { glacier: "GLACIER" })}</option><option value="DEEP_ARCHIVE">{t("ui.pages.protect.deep.archive", { deepArchive: "DEEP_ARCHIVE" })}</option></select><small>{coldStorageArchiveClassHelp()}</small></label>}
							{!createUsesRcloneSignIn && createOrderedOptions.filter((option) => option.advanced && !creationOptionIsCustom(vaultForm.connector, option.key)).map((option) => <IntegrationField key={option.key} connector={vaultForm.connector} option={option} value={vaultForm.options[option.key] ?? ""} disabled={creationOptionLockedForPending(vaultForm, option)} explanation={advancedCreationOptionExplanation(vaultForm.connector, option)} onChange={(value) => setVaultForm(updateConnectorOption(vaultForm, option.key, value))} />)}
								<VaultCareFields form={vaultForm} disabled={Boolean(vaultForm.pendingLocation)} onChange={setVaultForm} />
                            </div>
                        )}
                    </div>
					<div className={`modal-footer${createUsesRcloneRemote ? " rclone-remote-dialog-footer" : ""}`}><button className="btn" disabled={vaultSaving} onClick={closeVaultCreate}>{t("ui.pages.protect.cancel")}</button><button className="btn primary" disabled={vaultSaving || !integration || !validVaultName(vaultForm.name) || !vaultLocation(vaultForm, true) || !validVaultPassword(vaultForm.password) || vaultForm.password !== vaultForm.passwordConfirmation || createMissingRequiredOptions.length > 0 || !validObjectLockSettings(vaultForm.objectLock, vaultForm.maintenanceSchedule)} onClick={() => { if (createUsesRcloneSignIn && !createRcloneAuth && !retryCreationIntentId) setShowCreateRcloneAuthorization(true); else void saveVault(); }}>{vaultSaving && <span className="spinner" />}{vaultSaving ? t("ui.pages.protect.creating") : retryCreationIntentId ? t("ui.pages.protect.retry.creation") : t("ui.pages.protect.create.vault")}</button></div>
					</fieldset>
					{vaultSaving && <KeepWindowOpenNotice />}
					{vaultSaving && <VaultActivityLog records={vaultProgress} />}
                </Modal>
            )}

			{showVaultCreate && showCreateRcloneAuthorization && integration && (
				<Modal title={t("ui.rclone.connectProvider", { provider: knownMessage(`ui.integration.${integration.id}.label`, integration.label) })} onClose={closeCreateRcloneAuthorization}>
					<div className="modal-form">
						<p>{renderMessage("ui.rclone.authorizationDescription", { provider: knownMessage(`ui.integration.${integration.id}.label`, integration.label), rcloneLink: <a href="https://github.com/rclone/rclone" target="_blank" rel="noreferrer">{t("ui.pages.protect.rclone")}</a> })}</p>
						{vaultCreateError && <div className="inline-error" role="alert">{vaultCreateError}</div>}
						<RcloneAuthorization provider={vaultForm.connector} label={knownMessage(`ui.integration.${integration.id}.label`, integration.label)} readyAction="create vault" value={createRcloneAuth} disabled={vaultSaving} closeOnUnmount={false} showDescription={false} onChange={setCreateRcloneAuth} onError={(message) => toast("error", message)} />
					</div>
					<div className="modal-footer">
						<button className="btn" disabled={vaultSaving} onClick={closeCreateRcloneAuthorization}>{t("ui.pages.protect.back")}</button>
						{createRcloneAuth?.status === "ready" && <button className="btn primary" disabled={vaultSaving} onClick={() => void saveVault()}>{vaultSaving && <span className="spinner" />}{vaultSaving ? t("ui.pages.protect.creating") : t("ui.pages.protect.create.vault")}</button>}
					</div>
					{vaultSaving && <KeepWindowOpenNotice />}
					{vaultSaving && <VaultActivityLog records={vaultProgress} />}
				</Modal>
			)}

			{creationErrorView && (
					<Modal title={t("ui.protect.pendingCreationTitle", { name: creationErrorView.name })} onClose={() => setCreationErrorView(null)}>
					<div className="modal-form">
						<p className="section-copy">{t("ui.pages.protect.replicaro.kept.the.recovery.record.so.this.destination.cannot.be.creat")}</p>
						<div className="inline-error" role="alert">{creationErrorView.lastError || t("ui.pages.protect.vault.creation.did.not.finish")}</div>
					</div>
					<div className="modal-footer">
						<button className="btn" onClick={() => setCreationErrorView(null)}>{t("ui.pages.protect.close")}</button>
					</div>
				</Modal>
			)}

			{vaultSettings && !vaultOneAtATimeChoice && (
				<Modal title={vaultSettings.name} onClose={closeVault}>
					<div className="vault-settings-content" aria-busy={vaultWorkState !== "idle"}>
						{vaultWorkState !== "idle" && <div className="vault-work-overlay" role="status" aria-live="polite">
							<p><span>{vaultWorkState === "running"
								? t("ui.pages.protect.vault.work.is.currently.running.vault.settings.cannot.be")
								: vaultWorkState === "unavailable"
									? t("ui.pages.protect.vault.work.status.could.not.be.checked.vault.settings")
									: t("ui.pages.protect.checking.whether.vault.work.is.running.vault.settings.cannot")}</span><span className="spinner" aria-hidden="true" /></p>
						</div>}
						<div inert={vaultWorkState !== "idle" ? true : undefined}>
					<div className="modal-location mono">{vaultSettings.engine} · {vaultSettings.location}</div>
					<div className="modal-section-label">{t("ui.pages.protect.vault.care")}</div>
					{vaultOwnershipPresentation === "checking" && <section className="recovery-warning vault-owner-status checking" role="status"><span className="spinner" aria-hidden="true" />{t("ui.pages.protect.checking.vault.ownership.status")}</section>}
					{vaultOwnershipPresentation === "unverified" && <section className="recovery-warning vault-owner-status" role="alert"><p>{t("ui.pages.protect.vault.ownership.status.could.not.be.verified.vault.owner.actions.are.d")}</p><button className="btn" onClick={() => detectVaultOwnership(vaultSettings, vaultSettingsSession.current)}>{t("ui.pages.protect.detect.vault.owner.status")}</button></section>}
					{(vaultOwnershipPresentation === "owner" || vaultOwnershipPresentation === "nonowner") && vaultOwnership && <section className="recovery-warning vault-owner-status"><p>{vaultOwnership.message}</p>{vaultOwnershipPresentation === "nonowner" && <><p>{vaultOwnership.takeoverExplanation}</p><button className="btn danger" disabled={vaultOwnershipBusy} onClick={() => void takeOverVaultOwnership()}>{vaultOwnershipBusy && <span className="spinner" />}{t("ui.pages.protect.click.here.to.take.over.vault.ownership")}</button>{vaultOwnershipBusy && <KeepWindowOpenNotice />}</>}</section>}
					{vaultOwnershipPresentation === "transfer_unfinished" && vaultOwnership && <section className="recovery-warning vault-owner-status" role="alert"><p>{t("ui.protect.ownerTransferUnfinished")}</p><button className="btn danger" disabled={vaultOwnershipBusy} onClick={() => void takeOverVaultOwnership()}>{vaultOwnershipBusy && <span className="spinner" />}{t("ui.protect.finishTakeover")}</button>{vaultOwnershipBusy && <KeepWindowOpenNotice />}</section>}
					{vaultOwnershipPresentation === "transfer_elsewhere" && <section className="recovery-warning vault-owner-status" role="alert"><p>{t("ui.protect.ownerTransferElsewhere")}</p><button className="btn" onClick={() => detectVaultOwnership(vaultSettings, vaultSettingsSession.current)}>{t("ui.pages.protect.detect.vault.owner.status")}</button></section>}
					<div className="form-grid two vault-care-fields">
						{(vaultOwnershipPresentation === "owner" || vaultOwnershipPresentation === "nonowner") && <label className="field"><span>{t("ui.pages.protect.integrity.check")}</span><select value={vaultSettings.coldStorage ? "manual" : checkSchedule} disabled={vaultSettings.coldStorage || vaultOwnershipPresentation !== "owner"} onChange={(event) => setCheckSchedule(event.target.value)}>{(vaultSettings.coldStorage ? [["manual", () => t("ui.care.disabled")]] as const : careSchedules).map(([value, label]) => <option key={value} value={value}>{label()}</option>)}</select><small>{vaultSettings.coldStorage ? coldStorageIntegrityHelp() : integrityCheckHelp()}</small>{vaultOwnershipPresentation === "nonowner" && <small>{t("ui.pages.protect.only.the.current.vault.owner.can.change.or.run.integrity.checks.take.o")}</small>}<small>{t("ui.protect.lastCareRun", { last: vaultSettings.lastCheck ? `${timeAgo(vaultSettings.lastCheck)} (${vaultSettings.lastCheckStatus})` : t("ui.protect.never"), next: vaultSettings.nextCheck ? t("ui.protect.nextCareRun", { time: timeAgo(vaultSettings.nextCheck) }) : "" })}</small></label>}
						{(vaultOwnershipPresentation === "owner" || vaultOwnershipPresentation === "nonowner") && <label className="field"><span>{t("ui.pages.protect.space.reclamation")}</span><select value={maintenanceSchedule} disabled={vaultOwnershipPresentation !== "owner"} onChange={(event) => setMaintenanceSchedule(event.target.value)}>{careSchedules.map(([value, label]) => <option key={value} value={value} disabled={!objectLockScheduleEligible(objectLock, value)}>{label()}</option>)}</select><small>{objectLock.enrolled ? (objectLock.paused ? pausedObjectLockMaintenanceHelp() : objectLockMaintenanceHelp()) : maintenanceHelp()}</small>{vaultOwnershipPresentation === "nonowner" && <small>{t("ui.pages.protect.only.the.current.vault.owner.can.change.or.run.space.reclamation.take")}</small>}<small>{t("ui.protect.lastCareRun", { last: vaultSettings.lastMaintenance ? `${timeAgo(vaultSettings.lastMaintenance)} (${vaultSettings.lastMaintenanceStatus})` : t("ui.protect.never"), next: vaultSettings.nextMaintenance ? t("ui.protect.nextCareRun", { time: timeAgo(vaultSettings.nextMaintenance) }) : "" })}</small></label>}
						{(vaultOwnershipPresentation === "owner" || vaultOwnershipPresentation === "nonowner") && <div className="vault-care-actions"><div className="vault-care-action"><button className="btn" disabled={vaultSettings.coldStorage || vaultOwnershipPresentation !== "owner" || Boolean(toolBusy)} onClick={() => runTool("check")}>{toolBusy === "check" && <span className="spinner" />}{t("ui.pages.protect.run.check.now")}</button></div><div className="vault-care-action"><button className="btn" disabled={vaultOwnershipPresentation !== "owner" || Boolean(toolBusy)} onClick={() => runTool("maintenance")}>{toolBusy === "maintenance" && <span className="spinner" />}{t("ui.pages.protect.run.reclamation.now")}</button></div></div>}
						<JobSpeedField connector={vaultSettings.connector} value={concurrencyMode} onChange={(mode) => setConcurrencyMode(compatibleConcurrencyMode(vaultSettings.connector, mode))} />
					</div>
					<ObjectLockFields
						form={{ ...emptyVault(vaultSettingsIntegration, vaultSettings.engine), engine: vaultSettings.engine, connector: vaultSettings.connector, maintenanceSchedule, objectLock }}
						disabled={vaultOwnershipPresentation !== "owner"}
						originalObjectLock={vaultSettings.objectLock}
						onChange={(next) => { setObjectLock(next.objectLock); setMaintenanceSchedule(next.maintenanceSchedule); }}
					/>
					{vaultSettings.coldStorage && <p><strong>{t("ui.pages.protect.archive.write.class")}</strong> {vaultSettings.archiveWriteClass} {t("ui.protect.readOnly")}</p>}
					{vaultSettings.coldStorage && <div className="recovery-warning">{coldStorageProviderGuidance().map((paragraph) => <p key={paragraph}>{paragraph}</p>)}</div>}
					{vaultSettings.engine === "restic" && <>
						<button className="btn advanced-toggle vault-settings-advanced-toggle" onClick={() => setVaultSettingsAdvanced((value) => !value)}><Icon name="settings" size={14} />{vaultSettingsAdvanced ? t("ui.pages.protect.hide.advanced.settings") : t("ui.pages.protect.advanced.settings")}</button>
						{vaultSettingsAdvanced && <div className="advanced-panel"><div className="advanced-setting"><label className="check"><input type="checkbox" checked={autoUnlock} onChange={(event) => setAutoUnlock(event.target.checked)} />{t("ui.pages.protect.auto.unlock.for.stuck.vaults")}</label><small>{t("ui.pages.protect.restic.will.sometimes.block.use.of.a.vault.when.running.an.operation.t")}</small></div></div>}
					</>}
					<section className="advanced-panel vault-password-panel">
						<div className="modal-section-label">{t("ui.pages.protect.change.vault.password")}</div>
						{vaultOwnershipPresentation === "owner" && <p className="vault-password-intro">{t("ui.pages.protect.if.you.are.backing.up.other.computers.to.this.vault.you.will.need.to.r")}</p>}
						{vaultOwnershipPresentation === "nonowner" && <p>{t("ui.protect.currentOwnerPasswordHelp", { owner: currentVaultOwner })}</p>}
						{(vaultOwnershipPresentation === "owner" || vaultOwnershipPresentation === "nonowner") && <>
							<div className="form-grid two vault-password-fields">
								<label className="field"><span>{t("ui.pages.protect.new.vault.password")}</span><input type="password" autoComplete="new-password" disabled={vaultOwnershipPresentation !== "owner" || Boolean(vaultMutations[vaultSettings.id])} value={vaultPasswordForm.password} onChange={(event) => setVaultPasswordForm((current) => ({ ...current, password: event.target.value }))} /></label>
								<label className="field"><span>{t("ui.pages.protect.confirm.new.vault.password")}</span><input type="password" autoComplete="new-password" disabled={vaultOwnershipPresentation !== "owner" || Boolean(vaultMutations[vaultSettings.id])} value={vaultPasswordForm.confirmation} onChange={(event) => setVaultPasswordForm((current) => ({ ...current, confirmation: event.target.value }))} /></label>
							</div>
							{vaultPasswordForm.password && vaultPasswordForm.confirmation && vaultPasswordForm.password !== vaultPasswordForm.confirmation && <small className="inline-error" role="alert">{t("ui.pages.protect.the.vault.passwords.do.not.match")}</small>}
							<button className="btn" disabled={vaultOwnershipPresentation !== "owner" || Boolean(vaultMutations[vaultSettings.id])} onClick={submitVaultPasswordChange}>{t("ui.pages.protect.change.vault.password")}</button>
						</>}
					</section>
					{dormantJobs.length > 0 && <section className="unowned-snapshots"><div className="modal-section-label">{t("ui.pages.protect.dormant.recovery.jobs")}</div><p>{t("ui.pages.protect.these.definitions.remain.protected.in.vault.replicaro.but.do.not.run.o", { sidecar: "vault.replicaro" })}</p>{dormantJobs.map((item) => <div className="unowned-snapshot" key={item.jobId}><span><strong>{item.definition.name}</strong><small>{displayPath(item.definition.source)} · {scheduleLabel(item.definition.schedule)}</small><small>{t("ui.protect.dormantScriptSummary", { before: item.definition.beforeScriptPath ? `${item.definition.beforeScriptPath} (${item.definition.beforeScriptMustSucceed ? t("ui.protect.required") : t("ui.protect.optional")})` : t("ui.protect.noneLower"), after: item.definition.afterScriptPath ? `${item.definition.afterScriptPath} (${item.definition.afterScriptMustSucceed ? t("ui.protect.required") : t("ui.protect.optional")})` : t("ui.protect.noneLower") })}</small></span><div className="tool-buttons"><button className="btn sm" disabled={Boolean(dormantBusy)} onClick={() => void changeDormant(item.repositoryId, item.jobId, "restore")}>{dormantBusy === `restore:${item.jobId}` && <span className="spinner" />}{t("ui.pages.protect.restore.disabled")}</button><button className="btn sm danger-outline" disabled={Boolean(dormantBusy)} onClick={() => void changeDormant(item.repositoryId, item.jobId, "discard")}>{dormantBusy === `discard:${item.jobId}` && <span className="spinner" />}{t("ui.pages.protect.discard.definition")}</button></div></div>)}</section>}
					{vaultSettingsIntegration && (!usesRcloneSignIn(vaultSettings.connector) || (vaultSettings.engine === "restic" && vaultSettingsRcloneSupported)) && <section className="vault-connection-panel"><div className="modal-section-label">{t("ui.pages.protect.vault.connection")}</div><p>{t("ui.pages.protect.use.this.to.reconnect.to.the.vault.after.disconnection.for.any.reason")}</p><button className="btn" onClick={() => requestSavedVaultReconnect(vaultSettings)}>{t("ui.pages.protect.reconnect.vault")}</button></section>}
					<div className="danger-zone"><div className="modal-section-label">{t("ui.pages.protect.danger.zone")}</div><p>{t("ui.pages.protect.removing.this.vault.also.removes.it.as.a.destination.from.matching.bac")}</p><button className="btn danger-outline" onClick={() => { const repository = vaultSettings; dismissVaultSettings(); openVaultDelete(repository); }}>{t("ui.pages.protect.remove.vault.from.replicaro")}</button></div>
						</div>
						<div className="modal-footer"><button className="btn" onClick={closeVault}>{t("ui.pages.protect.cancel")}</button><button className="btn primary" disabled={vaultWorkState !== "idle" || !validObjectLockSettings(objectLock, maintenanceSchedule, true, vaultSettings.objectLock)} onClick={saveCare}>{t("ui.pages.protect.save")}</button></div>
					</div>
                </Modal>
            )}

            {vaultDelete && (
                <ConfirmDialog
					title={t("ui.protect.removeNamedVaultQuestion", { vault: vaultDelete.name })}
					message={<><span>{renderMessage("ui.protect.removeVaultDestinationHelp", { count: jobCountFor(vaultDelete.id), jobCount: <strong>{t("ui.protect.backupJobCount", { count: jobCountFor(vaultDelete.id) })}</strong> })}</span><br /><br /><span>{t("ui.pages.protect.jobs.that.have.other.vaults.as.a.destination.will.not.be.deleted.jobs")}</span><br /><br /><span>{t("ui.pages.protect.data.on.disk.is.untouched.you.can.re.add.the.vault.later")}</span></>}
					confirmLabel={t("ui.pages.protect.remove.vault")}
					onConfirm={() => removeVault(vaultDelete)}
                    onCancel={dismissVaultDelete}
                />
            )}

			{vaultOneAtATimeChoice && (
				<Modal title={t("ui.pages.protect.pick.one.action")} onClose={() => setVaultOneAtATimeChoice(null)}>
					<p className="muted" style={{ marginTop: 0 }}>{vaultOneAtATimeChoice.kind === "reconnect"
						? t("ui.pages.protect.you.have.changed.vault.settings.and.also.selected.to")
						: t("ui.pages.protect.you.have.changed.vault.settings.and.also.selected.to.2")}</p>
					<div className="modal-footer">
						<button className="btn" onClick={() => setVaultOneAtATimeChoice(null)}>{t("ui.pages.protect.cancel")}</button>
						<button className="btn" onClick={chooseSaveVaultSettings}>{t("ui.pages.protect.save.vault.settings")}</button>
						{vaultOneAtATimeChoice.kind === "reconnect"
							? <button className="btn primary" onClick={chooseReconnect}>{t("ui.pages.protect.go.reconnect")}</button>
							: <button className="btn primary" onClick={choosePasswordChange}>{t("ui.pages.protect.change.vault.password.2")}</button>}
					</div>
				</Modal>
			)}

            {closePrompt && (
                <SaveChangesDialog
                    onSave={saveChangesBeforeClose}
                    onDiscard={discardChanges}
                    onCancel={cancelClosePrompt}
                    busy={jobSaving}
                />
            )}
        </div>
    );
}
