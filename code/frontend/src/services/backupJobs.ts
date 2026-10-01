import type { BackupJobTarget } from "../types";
import { getEffectiveLocale, t } from "../i18n";
import type { ManualTargetAdmissionResult } from "./api";
import { parseTime, timeAgo } from "../components/ui";
import type { BackupJob } from "../types";

export const nextSnapshotTooltip = (job: BackupJob, relativeFormatter = timeAgo) => {
    if (!job.enabled) return t("ui.protect.disabledJobNextSnapshotHelp");
    if (job.schedule === "manual") return t("ui.protect.manualJobNextSnapshotHelp");
    const nextRun = parseTime(job.nextRun);
    if (!nextRun) return t("ui.protect.unavailableNextSnapshotHelp");
    const secondsUntilRun = (nextRun.getTime() - Date.now()) / 1000;
    const relative = relativeFormatter(job.nextRun);
    return secondsUntilRun > 0 ? t("ui.protect.nextSnapshotAt", { relative }) : t("ui.protect.nextSnapshotDueNow");
};

export function backupTargetIsActive(target: BackupJobTarget) {
    return target.lastStatus === "queued" || target.lastStatus === "running";
}

export function storageAvailabilityReasonLabel(reason: string) {
    switch (reason) {
        case "not_checked":
            return t("ui.storageAvailability.notChecked");
        case "storage_missing":
            return t("ui.storageAvailability.storageMissing");
        case "identity_mismatch":
            return t("ui.storageAvailability.identityMismatch");
        case "observation_failed":
            return t("ui.storageAvailability.observationFailed");
        case "observation_timeout":
            return t("ui.storageAvailability.observationTimeout");
        default:
            return "";
    }
}

// The per-job result of creating, bulk editing, enabling, or disabling jobs.
export type JobMutationResultStatus = "created" | "updated" | "unchanged" | "warning" | "failed";

export function jobMutationStatusLabel(status: JobMutationResultStatus) {
    switch (status) {
        case "created":
            return t("ui.protect.jobResult.created");
        case "updated":
            return t("ui.protect.jobResult.updated");
        case "unchanged":
            return t("ui.protect.jobResult.unchanged");
        case "warning":
            return t("ui.protect.jobResult.warning");
        case "failed":
            return t("ui.protect.jobResult.failed");
    }
}

function manualRunNotStartedReason(status: Exclude<ManualTargetAdmissionResult["status"], "admitted">, reasonCode?: string) {
    switch (status) {
        case "busy":
            // A vault whose removal is pending is reported busy as well, but
            // nothing is running on it, so it gets its own reason.
            return reasonCode === "vault_being_removed" ? t("ui.backup.runResult.reason.vaultBeingRemoved") : t("ui.backup.runResult.reason.busy");
        case "policy_not_ready":
            return t("ui.backup.runResult.reason.policyNotReady");
        case "storage_unavailable":
            // The server also reports an unavailable job source this way (with
            // the source's reason code), so the wording names both the source
            // and the vault's storage rather than blaming the vault.
            return t("ui.backup.runResult.reason.storageUnavailable");
    }
}

// One vault's row in the manual run results dialog. Every requested vault is
// listed there, so each row says plainly whether its backup started; a listed
// vault must not read as a started one. The reasons are the notice's own
// wording, and operation IDs and raw reason codes stay out of the text.
export function manualRunResultText(result: ManualTargetAdmissionResult) {
    if (result.status === "admitted") return t("ui.backup.runResult.row.started");
    const reason = manualRunNotStartedReason(result.status, result.reasonCode);
    // "Storage missing" only repeats the reason itself, so it gets no detail.
    const detail = result.status === "storage_unavailable" && result.reasonCode && result.reasonCode !== "storage_missing"
        ? storageAvailabilityReasonLabel(result.reasonCode) : "";
    return detail ? t("ui.backup.runResult.row.notStartedWithDetail", { reason, detail }) : t("ui.backup.runResult.row.notStarted", { reason });
}

// One notice for a manual "run now" request, built from the per-vault
// admission results that /api/jobs/run returns. The server admits each
// destination vault separately, so a single request can start on some vaults
// and not on others; the notice has to say which, and why, instead of
// assuming every vault started. The top-bar "Run jobs now" picker and the
// Protect page both use this so their wording stays the same. Vault lists are
// joined with Intl.ListFormat in the active UI locale ("and" for the vaults
// that started or the ones that did not while others did, "or" when none
// started) so each language gets its own separators and conjunctions.
export function manualRunNotice(
    jobName: string,
    targets: readonly Pick<BackupJobTarget, "repositoryId" | "repositoryName">[],
    results: readonly ManualTargetAdmissionResult[],
): { kind: "info" | "error"; message: string; admittedCount: number } {
    const locale = getEffectiveLocale();
    const vaultName = (repositoryId: string) =>
        targets.find((target) => target.repositoryId === repositoryId)?.repositoryName ?? repositoryId;
    const started: string[] = [];
    const notStarted: string[] = [];
    for (const result of results) {
        if (result.status === "admitted") {
            started.push(t("ui.backup.runResult.vault", { vaultName: vaultName(result.repositoryId) }));
        } else {
            notStarted.push(t("ui.backup.runResult.vaultWithReason", {
                vaultName: vaultName(result.repositoryId),
                reason: manualRunNotStartedReason(result.status, result.reasonCode),
            }));
        }
    }
    const and = new Intl.ListFormat(locale, { type: "conjunction" });
    if (notStarted.length === 0) {
        return { kind: "info", admittedCount: started.length, message: t("ui.backup.runResult.started", { jobName, vaults: and.format(started) }) };
    }
    if (started.length > 0) {
        return {
            kind: "error",
            admittedCount: started.length,
            message: t("ui.backup.runResult.partiallyStarted", {
                jobName, startedVaults: and.format(started), notStartedVaults: and.format(notStarted),
            }),
        };
    }
    const or = new Intl.ListFormat(locale, { type: "disjunction" });
    return { kind: "error", admittedCount: 0, message: t("ui.backup.runResult.notStarted", { jobName, notStartedVaults: or.format(notStarted) }) };
}
