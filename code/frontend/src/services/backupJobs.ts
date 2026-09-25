import type { BackupJobTarget } from "../types";
import { t } from "../i18n";
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
