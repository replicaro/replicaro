import { t } from "./i18n";
import type { BackupJob, Snapshot } from "./types";

// Managed snapshots are labelled by job, not by a recorded path.
//
// Restore's managed list and File History's job-owned sources always belong
// to a job on this computer, so they show the job name. The tooltip shows the
// job's *current* source (its alias after "Update job source", otherwise the
// immutable source). For an older snapshot that can differ from the folder
// that snapshot actually read; that is intended, because the label describes
// the job as it is now. The native path a snapshot recorded is still what
// restore, browse, and every engine command use; only presentation changes.
// Unmanaged snapshots and File History sources that no single job owns keep
// showing their recorded paths.

export function jobCurrentSource(job: BackupJob): string {
	return job.resolvedSourcePath || job.source;
}

export function managedSnapshotJob(snapshot: Snapshot, jobs: BackupJob[] | null | undefined): BackupJob | undefined {
	if (snapshot.presentation !== "managed" || !snapshot.managedJobId) return undefined;
	return jobs?.find((job) => job.id === snapshot.managedJobId);
}

// File History groups a job's snapshots under the job's immutable source (Kopia
// records it through --override-source, Restic snapshots are regrouped by the
// backend). A source is job-owned only when exactly one job targeting this vault
// has that source; otherwise it is shown as a recorded path.
export function fileHistorySourceJob(source: string, repositoryId: string, jobs: BackupJob[] | null | undefined): BackupJob | undefined {
	const owners = (jobs ?? []).filter((job) => job.source === source &&
		job.targets.some((target) => target.repositoryId === repositoryId));
	return owners.length === 1 ? owners[0] : undefined;
}

// The path of an item inside the job, without drive or root, for example
// 2024\IMG_001.jpg for a Windows source. Archive paths are slash-delimited; a
// literal backslash in a component keeps the archive spelling.
export function pathWithinJob(path: string, source: string): string {
	const relative = path.replace(/^\/+|\/+$/g, "");
	const windowsSource = /^[A-Za-z]:[\\/]/.test(source) || source.startsWith("\\\\");
	return windowsSource && !relative.includes("\\") ? relative.replace(/\//g, "\\") : relative;
}

export function jobSourceTooltip(job: BackupJob): string {
	return t("ui.jobSource.tooltip", { source: jobCurrentSource(job) });
}

// "Update job source" is offered whenever a bound job's source cannot be used:
// paused because the storage is unavailable, or failing because the folder is
// gone from present storage, access is denied, or the storage no longer
// matches. A failed source check is stored per target as "unknown" with the
// reason of that failure; "not_checked" and "observation_timeout" are not
// failures. Imported jobs that were never bound here use first binding instead.
const sourceFailureReasons = new Set(["storage_missing", "identity_mismatch", "observation_failed"]);

export function jobSourceNeedsUpdate(job: BackupJob): boolean {
	if (job.sourceBindingState === "unbound_imported") return false;
	return job.targets.some((target) => target.sourceAvailability === "unavailable" ||
		target.sourceAvailability === "unknown" && sourceFailureReasons.has(target.sourceAvailabilityReason));
}
