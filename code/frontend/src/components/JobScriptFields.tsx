export interface JobScriptValues {
	beforeScriptPath: string;
	beforeScriptMustSucceed: boolean;
	afterScriptPath: string;
	afterScriptMustSucceed: boolean;
}

export function JobScriptFields({ value, onChange }: {
	value: JobScriptValues;
	onChange: (next: JobScriptValues) => void;
}) {
	return <section className="job-script-fields">
		<div className="modal-section-label">Before and after scripts</div>
		<p className="recovery-warning">Scripts run non-interactively with your user privileges. Script output is discarded. Replicaro waits until a script has finished before proceeding to the next step. If a script hangs, your backup job may hang and other jobs may not be able to use your vault until you manually cancel the script run.</p>
		<div className="job-script-setting">
			<label className="field"><span>Before script path (optional)</span><input className="mono" value={value.beforeScriptPath} placeholder="Exact absolute executable script path" onChange={(event) => onChange({ ...value, beforeScriptPath: event.target.value, beforeScriptMustSucceed: event.target.value ? value.beforeScriptMustSucceed : false })} /><small>Runs before the job (one execution for each destination vault).</small></label>
			<div className="advanced-setting"><label className="check"><input type="checkbox" checked={value.beforeScriptMustSucceed} disabled={!value.beforeScriptPath} onChange={(event) => onChange({ ...value, beforeScriptMustSucceed: event.target.checked })} />Before script must succeed</label><small className="job-script-success-help">Script failure stops execution for that destination vault.</small></div>
		</div>
		<div className="job-script-setting">
			<label className="field"><span>After script path (optional)</span><input className="mono" value={value.afterScriptPath} placeholder="Exact absolute executable script path" onChange={(event) => onChange({ ...value, afterScriptPath: event.target.value, afterScriptMustSucceed: event.target.value ? value.afterScriptMustSucceed : false })} /><small>Runs after the job (one execution for each destination vault).</small></label>
			<div className="advanced-setting"><label className="check"><input type="checkbox" checked={value.afterScriptMustSucceed} disabled={!value.afterScriptPath} onChange={(event) => onChange({ ...value, afterScriptMustSucceed: event.target.checked })} />After script must succeed</label><small className="job-script-success-help">Script failure marks that destination&apos;s job run as failed.</small></div>
		</div>
	</section>;
}
