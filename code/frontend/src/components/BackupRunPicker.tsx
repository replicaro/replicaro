import { t } from "../i18n";
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
                        <strong>{t("ui.components.backuprunpicker.run.all")}</strong>
                        <span className="faint">{t("ui.backupRunPicker.requestAllHelp", { count: job.targets.length })}</span>
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
                            <span className="faint">{active ? target.lastStatus : t("ui.components.backuprunpicker.back.up.to.this.destination.vault")}</span>
                            {target.sourceAvailability === "unavailable" && <span className="faint">{t("ui.components.backuprunpicker.source.unavailable")}</span>}
                            {target.targetAvailability === "unavailable" && <span className="faint">{t("ui.components.backuprunpicker.vault.unavailable")}</span>}
                            {target.pendingCatchUp && <span className="faint">{t("ui.backupRunPicker.catchUpPending", { count: target.coalescedMissedCount })}</span>}
                        </span>
                        {busy === target.repositoryId ? <span className="spinner" /> : <Icon name="play" size={14} />}
                    </button>
                );
            })}
        </div>
    );
}
