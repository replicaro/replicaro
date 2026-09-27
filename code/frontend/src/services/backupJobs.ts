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

function manualRunNotStartedReason(status: Exclude<ManualTargetAdmissionResult["status"], "admitted">) {
    switch (status) {
        case "busy":
            return t("ui.backup.runResult.reason.busy");
        case "policy_not_ready":
            return t("ui.backup.runResult.reason.policyNotReady");
        case "storage_unavailable":
            return t("ui.backup.runResult.reason.storageUnavailable");
    }
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
                reason: manualRunNotStartedReason(result.status),
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
