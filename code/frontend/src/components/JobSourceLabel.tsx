import type { BackupJob } from "../types";
import { jobSourceTooltip } from "../jobSourceDisplay";
import { Tooltip } from "./ui";

// See jobSourceDisplay.ts for why managed snapshots are labelled by job. The
// job name stays the accessible name; the "Job source:" text is the tooltip's
// description.
export function JobSourceLabel({ job, className = "" }: { job: BackupJob; className?: string }) {
	return (
		<Tooltip content={jobSourceTooltip(job)}>
			<span className={`job-source-label ${className}`.trim()} aria-label={job.name}><span className="job-source-name">{job.name}</span></span>
		</Tooltip>
	);
}
