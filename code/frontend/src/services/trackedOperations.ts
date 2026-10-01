import { APIError, getOperation, getOperationLog, isOperationNotFound } from "./api";
import type { OperationEntry } from "../types";

// Snapshot and job deletion close their dialog as soon as the backend has
// queued the operation. The page hides the item while that operation is
// active and reports the result from the operation itself, so the outcome
// still arrives if the dialog, or the page, is long gone. This module follows
// such an operation until it finishes and tells whoever is interested.
//
// The hidden state is derived from the backend's active operations, not kept
// here: after a reload or in another browser the page reads the active
// operations again and hides the same items. What lives here is only the
// polling for operations this browser is following, deduplicated by ID so a
// page that mounts twice does not announce a result twice.

export const JOB_DELETION_KIND = "delete_job";
// The vault password change (and its retry), vault settings save and vault
// removal. The vault card shows their progress while one is queued or
// running; like the hidden deletions above, that comes from the backend's
// active operations, so a reload or another browser shows it too.
export const VAULT_PASSWORD_CHANGE_KIND = "vault_password";
export const VAULT_SETTINGS_KIND = "vault_settings";
export const VAULT_REMOVAL_KIND = "remove_vault";
const VAULT_CHANGE_KINDS = new Set([VAULT_PASSWORD_CHANGE_KIND, VAULT_SETTINGS_KIND, VAULT_REMOVAL_KIND]);
// A failed removal records this step when the vault's recovery profile could
// not be updated. It is what offers Remove anyway; no other failure does.
const VAULT_REMOVAL_PROFILE_STEP = "recovery_profile_publication";
// A failed password change records this step when it left the vault blocked
// (stopped before commit, so every operation on the vault is refused until the
// change is retried).
const VAULT_PASSWORD_BLOCKING_STEP = "vault_password_blocked";
const SNAPSHOT_DELETION_KIND = "delete";
// The backend carries the snapshot ID only in the deletion's title. The title
// is fixed English (it is also what notifications and the dashboard show), so
// matching its prefix is exact.
const SNAPSHOT_DELETION_TITLE_PREFIX = "Delete snapshot: ";

export const FOLLOW_INTERVAL_MS = 1500;

export function operationIsActive(operation: Pick<OperationEntry, "status">) {
	return operation.status === "queued" || operation.status === "running";
}

export function activeSnapshotDeletions(operations: OperationEntry[], repositoryId: string) {
	return new Set(operations
		.filter((operation) => operation.kind === SNAPSHOT_DELETION_KIND && operation.repositoryId === repositoryId &&
			operationIsActive(operation) && operation.title.startsWith(SNAPSHOT_DELETION_TITLE_PREFIX))
		.map((operation) => operation.title.slice(SNAPSHOT_DELETION_TITLE_PREFIX.length)));
}

export function activeJobDeletions(operations: OperationEntry[]) {
	return new Map(operations
		.filter((operation) => operation.kind === JOB_DELETION_KIND && operation.jobId && operationIsActive(operation))
		.map((operation) => [operation.jobId, operation.id] as const));
}

// activeVaultChanges returns the queued or running password change, settings
// save or removal of each vault. The backend allows only one per vault.
export function activeVaultChanges(operations: OperationEntry[]) {
	return new Map(operations
		.filter((operation) => VAULT_CHANGE_KINDS.has(operation.kind) && operation.repositoryId && operationIsActive(operation))
		.map((operation) => [operation.repositoryId, operation] as const));
}

// newestVaultOperations returns the newest operation of each vault in
// operations, of any kind or, with kind, of that kind only. It goes by start
// time rather than list order because the dashboard merges operations it kept
// from earlier reads into the backend's newest-first list; for equal times the
// earlier entry in the list wins, as the backend lists newest first.
export function newestVaultOperations(operations: OperationEntry[], kind?: string) {
	const started = (operation: OperationEntry) => {
		const time = Date.parse(operation.startedAt);
		return Number.isNaN(time) ? -Infinity : time;
	};
	const newest = new Map<string, OperationEntry>();
	for (const operation of operations) {
		if ((kind !== undefined && operation.kind !== kind) || !operation.repositoryId) continue;
		const current = newest.get(operation.repositoryId);
		if (!current || started(operation) > started(current)) newest.set(operation.repositoryId, operation);
	}
	return newest;
}

const hasFailedStep = (operation: OperationEntry | undefined, kind: string) =>
	(operation?.steps ?? []).some((step) => step.kind === kind && step.status === "failed");

export function removalStoppedByRecoveryProfile(operation: OperationEntry | undefined) {
	return operation?.kind === VAULT_REMOVAL_KIND && operation.status === "failed" && hasFailedStep(operation, VAULT_REMOVAL_PROFILE_STEP);
}

export function passwordChangeLeftVaultBlocked(operation: OperationEntry | undefined) {
	return operation?.kind === VAULT_PASSWORD_CHANGE_KIND && operation.status === "failed" && hasFailedStep(operation, VAULT_PASSWORD_BLOCKING_STEP);
}

// passwordChangeStillBlocksVault is passwordChangeLeftVaultBlocked for the
// dashboard's issue link: the blocking step is a fact about the moment that
// operation ended, and a later change or Retry of the same vault (whatever its
// result) supersedes it, so the link is shown only while this operation is
// still the vault's newest password change.
export function passwordChangeStillBlocksVault(operation: OperationEntry | undefined, operations: OperationEntry[]) {
	return passwordChangeLeftVaultBlocked(operation) &&
		newestVaultOperations(operations, VAULT_PASSWORD_CHANGE_KIND).get(operation!.repositoryId)?.id === operation!.id;
}

const followed = new Set<string>();
const finishedListeners = new Set<(operation: OperationEntry) => void>();

// onTrackedOperationFinished lets a mounted page refresh its lists when any
// followed operation ends, whichever page started following it.
export function onTrackedOperationFinished(listener: (operation: OperationEntry) => void) {
	finishedListeners.add(listener);
	return () => { finishedListeners.delete(listener); };
}

const delay = (ms: number) => new Promise<void>((resolve) => window.setTimeout(resolve, ms));

// followTrackedOperation polls one operation until it leaves queued/running,
// then calls announce once and notifies the finished listeners. It keeps going
// after the page that started it unmounts, so the toast still appears while
// Replicaro is open. A lookup that fails (for example the app restarting) is
// retried on the next tick. It returns false when the operation is already
// being followed.
export function followTrackedOperation(operationId: string, announce: (operation: OperationEntry) => void | Promise<void>) {
	if (followed.has(operationId)) return false;
	followed.add(operationId);
	void (async () => {
		for (;;) {
			await delay(FOLLOW_INTERVAL_MS);
			let operation: OperationEntry | undefined;
			try {
				operation = (await getOperation(operationId)).find((entry) => entry.id === operationId);
			} catch (reason) {
				// A record that no longer exists (log retention) has nothing left
				// to report; anything else is retried on the next tick.
				if (isOperationNotFound(reason)) return;
				continue;
			}
			if (!operation || operationIsActive(operation)) continue;
			try {
				await announce(operation);
			} finally {
				for (const listener of [...finishedListeners]) listener(operation);
			}
			return;
		}
	})();
	return true;
}

// While the notification step for a finished operation is still running, the
// log endpoint answers 409 operation_active. That step is short (a desktop
// notification or one webhook call), so the reason is worth a few quick
// retries before the toast settles for the title.
export const FAILURE_REASON_RETRY_MS = 500;
const FAILURE_REASON_ATTEMPTS = 6;

const SECTION_HEADER = /^\[([^\]]+) · ([^\]]+) · ([^\]]+)\]$/;

// operationFailureReason reads why a finished operation failed from its log:
// the body of the last section whose header kind is the operation's own kind,
// for example "[replicaro · delete · failed]". That is Replicaro's final
// result. Sections after it, such as "[replicaro · desktop/webhook
// notification · …]", are skipped on purpose; taking plainly the last section
// would put the notification step's text in the toast. It falls back to the
// operation title when the log cannot be read. Vault changes also use it for
// a success, since that section is their final result text either way.
export async function operationFailureReason(operation: OperationEntry) {
	for (let attempt = 1; attempt <= FAILURE_REASON_ATTEMPTS; attempt++) {
		try {
			const page = await getOperationLog(operation.id);
			// The final section is at the end; a log longer than one page (rare for
			// a deletion) falls back to the title.
			if (!page.eof) return operation.title;
			return finalSectionBody(page.output, operation.kind) ?? operation.title;
		} catch (reason) {
			if (!(reason instanceof APIError && reason.status === 409 && reason.code === "operation_active") ||
				attempt === FAILURE_REASON_ATTEMPTS) break;
			await delay(FAILURE_REASON_RETRY_MS);
		}
	}
	// The title still says what failed.
	return operation.title;
}

function finalSectionBody(output: string, kind: string) {
	const lines = output.split(/\r?\n/);
	let end = lines.length;
	for (let index = lines.length - 1; index >= 0; index--) {
		const header = SECTION_HEADER.exec(lines[index].trim());
		if (!header) continue;
		if (header[2] === kind) {
			const body = lines.slice(index + 1, end).join("\n").trim();
			if (!body) return undefined;
			return body.length > 500 ? `${body.slice(0, 500)}…` : body;
		}
		end = index;
	}
	return undefined;
}
