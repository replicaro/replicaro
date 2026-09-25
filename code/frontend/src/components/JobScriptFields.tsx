import { t } from "../i18n";
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
		<div className="modal-section-label">{t("ui.components.jobscriptfields.before.and.after.scripts")}</div>
		<p className="recovery-warning">{t("ui.components.jobscriptfields.scripts.run.non.interactively.with.your.user.privileges.script.output")}</p>
		<div className="job-script-setting">
			<label className="field"><span>{t("ui.components.jobscriptfields.before.script.path.optional")}</span><input className="mono" value={value.beforeScriptPath} placeholder={t("ui.components.jobscriptfields.exact.absolute.executable.script.path")} onChange={(event) => onChange({ ...value, beforeScriptPath: event.target.value, beforeScriptMustSucceed: event.target.value ? value.beforeScriptMustSucceed : false })} /><small>{t("ui.components.jobscriptfields.runs.before.the.job.one.execution.for.each.destination.vault")}</small></label>
			<div className="advanced-setting"><label className="check"><input type="checkbox" checked={value.beforeScriptMustSucceed} disabled={!value.beforeScriptPath} onChange={(event) => onChange({ ...value, beforeScriptMustSucceed: event.target.checked })} />{t("ui.components.jobscriptfields.before.script.must.succeed")}</label><small className="job-script-success-help">{t("ui.components.jobscriptfields.script.failure.stops.execution.for.that.destination.vault")}</small></div>
		</div>
		<div className="job-script-setting">
			<label className="field"><span>{t("ui.components.jobscriptfields.after.script.path.optional")}</span><input className="mono" value={value.afterScriptPath} placeholder={t("ui.components.jobscriptfields.exact.absolute.executable.script.path")} onChange={(event) => onChange({ ...value, afterScriptPath: event.target.value, afterScriptMustSucceed: event.target.value ? value.afterScriptMustSucceed : false })} /><small>{t("ui.components.jobscriptfields.runs.after.the.job.one.execution.for.each.destination.vault")}</small></label>
			<div className="advanced-setting"><label className="check"><input type="checkbox" checked={value.afterScriptMustSucceed} disabled={!value.afterScriptPath} onChange={(event) => onChange({ ...value, afterScriptMustSucceed: event.target.checked })} />{t("ui.components.jobscriptfields.after.script.must.succeed")}</label><small className="job-script-success-help">{t("ui.jobScripts.afterFailureHelp")}</small></div>
		</div>
	</section>;
}
