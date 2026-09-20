import type { BackupJobTarget } from "../types";

export function backupTargetIsActive(target: BackupJobTarget) {
    return target.lastStatus === "queued" || target.lastStatus === "running";
}

export function storageAvailabilityReasonLabel(reason: string) {
    switch (reason) {
        case "not_checked":
            return "not checked";
        case "storage_missing":
            return "storage missing";
        case "identity_mismatch":
            return "storage identity changed";
        case "observation_failed":
            return "availability check failed";
        case "observation_timeout":
            return "availability check timed out";
        default:
            return "";
    }
}
