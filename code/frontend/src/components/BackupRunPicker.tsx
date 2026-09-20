import { Icon } from "./ui";
import { backupTargetIsActive } from "../services/backupJobs";
import type { BackupJob } from "../types";

export function BackupRunPicker({
    job,
    busy,
    onRun,
}: {
    job: BackupJob;
    busy: string;
    onRun: (repositoryId?: string) => void;
}) {
    return (
        <div className="job-picker">
            {job.targets.length > 1 && (
                <button
                    type="button"
                    className="job-picker-row"
                    disabled={Boolean(busy)}
                    onClick={() => onRun()}
                >
                    <span>
                        <strong>Run all</strong>
                        <span className="faint">Request all {job.targets.length} destinations; every admitted, unavailable, or busy result will be reported</span>
                    </span>
                    {busy === "all" ? <span className="spinner" /> : <Icon name="play" size={14} />}
                </button>
            )}
            {job.targets.map((target) => {
                const active = backupTargetIsActive(target);
                return (
                    <button
                        type="button"
                        key={target.repositoryId}
                        className="job-picker-row"
                        disabled={Boolean(busy) || active}
                        onClick={() => onRun(target.repositoryId)}
                    >
                        <span>
                            <strong>{target.repositoryName} <span className="connector-chip">{target.engine}</span></strong>
                            <span className="faint">{active ? target.lastStatus : "Back up to this destination vault"}</span>
                            {target.sourceAvailability === "unavailable" && <span className="faint">Source unavailable</span>}
                            {target.targetAvailability === "unavailable" && <span className="faint">Vault unavailable</span>}
                            {target.pendingCatchUp && <span className="faint">Scheduled catch-up pending · {target.coalescedMissedCount} coalesced miss{target.coalescedMissedCount === 1 ? "" : "es"}</span>}
                        </span>
                        {busy === target.repositoryId ? <span className="spinner" /> : <Icon name="play" size={14} />}
                    </button>
                );
            })}
        </div>
    );
}
