import { useId, useState } from "react";
import { t } from "../i18n";
import { markerFileNameError, type MarkerFileValues } from "./markerFileName";

// The four "skip the contents of any folder that contains ..." boxes, shown
// after the exclude patterns. While the job backs up to a Kopia vault
// (usesKopia), the CACHEDIR.TAG box is shown on and locked, and the
// .kopiaignore box is shown, always on: every Kopia job policy sets both, and
// the job's Restic vaults then skip CACHEDIR.TAG folders too. The backend
// applies the same rule whatever the form sends, and the saved CACHEDIR.TAG
// choice is never rewritten, so it comes back once the job's last Kopia vault
// is removed.
//
// The custom name's error appears only once the name has been edited or a
// save was tried (showErrors), not as soon as the box is ticked with the
// field still empty. Saving with an invalid name is refused either way, by
// the form and by the backend.
//
// singleFile greys out every box for a job whose source is a single file:
// marker files only work inside folders, and the backend refuses to save a
// file job with any of them set. The caller passes the cleared values, and the
// boxes show exactly those, so the Kopia ones show unticked too instead of
// locked on. The surrounding block carries the explanation. The two Kopia
// notes are left out for a single file, because they describe the boxes as
// always on with a Kopia vault.
export function MarkerFileFields({ value, usesKopia, showErrors = false, singleFile = false, onChange }: {
	value: MarkerFileValues;
	usesKopia: boolean;
	showErrors?: boolean;
	singleFile?: boolean;
	onChange: (next: MarkerFileValues) => void;
}) {
	const [nameEdited, setNameEdited] = useState(false);
	const helpID = useId();
	const errorID = useId();
	const customError = value.skipCustomMarker && (nameEdited || showErrors) ? markerFileNameError(value.skipMarkerFileName) : "";
	// The help texts are deliberately simplified.
	// "The folder itself stays in the backup as an empty folder" is exact for
	// Kopia, but Restic also keeps the marker file itself (.nobackup, the
	// custom file or CACHEDIR.TAG) in the otherwise empty folder. That detail
	// is left out on purpose, so don't "correct" the wording.
	return <div className="marker-file-fields">
		<div className="advanced-setting">
			<label className="check"><input type="checkbox" checked={value.skipNoBackupFolders} disabled={singleFile} onChange={(event) => onChange({ ...value, skipNoBackupFolders: event.target.checked })} />{t("ui.protect.markerFiles.noBackupLabel")}</label>
			<small>{t("ui.protect.markerFiles.noBackupHelp")}</small>
		</div>
		<div className="advanced-setting">
			<label className="check"><input type="checkbox" checked={(usesKopia && !singleFile) || value.skipCacheDirFolders} disabled={usesKopia || singleFile} onChange={(event) => onChange({ ...value, skipCacheDirFolders: event.target.checked })} />{t("ui.protect.markerFiles.cacheDirLabel")}</label>
			<small>{t("ui.protect.markerFiles.cacheDirHelp")}</small>
			{usesKopia && !singleFile && <small>{t("ui.protect.markerFiles.cacheDirLocked")}</small>}
		</div>
		<div className="advanced-setting">
			<label className="check"><input type="checkbox" checked={value.skipCustomMarker} disabled={singleFile} onChange={(event) => onChange({ ...value, skipCustomMarker: event.target.checked })} />{t("ui.protect.markerFiles.customLabel")}</label>
			<input className="mono" aria-label={t("ui.protect.markerFiles.customLabel")} value={value.skipMarkerFileName} placeholder={t("ui.protect.markerFiles.customPlaceholder")} disabled={singleFile || !value.skipCustomMarker} aria-invalid={customError ? true : undefined} aria-describedby={customError ? `${errorID} ${helpID}` : helpID} onChange={(event) => { setNameEdited(true); onChange({ ...value, skipMarkerFileName: event.target.value }); }} />
			{customError && <small id={errorID} className="inline-error" role="alert">{customError}</small>}
			<small id={helpID}>{t("ui.protect.markerFiles.customHelp")}</small>
		</div>
		{usesKopia && <div className="advanced-setting">
			<label className="check"><input type="checkbox" checked={!singleFile} disabled />{t("ui.protect.markerFiles.kopiaIgnoreLabel")}</label>
			{!singleFile && <small>{t("ui.protect.markerFiles.kopiaIgnoreHelp")}</small>}
		</div>}
	</div>;
}
