import { isRightToLeft, t } from "../i18n";

// Any Rclone Remote settings and messages that the page and the API client
// share.

// The name Replicaro gives its own sidecar remote. The backend refuses a
// remote of this name in the user's rclone.conf.
export const RCLONE_SIDECAR_REMOTE_NAME = "replicaro_sidecar";

// One row of the environment variables field. The backend takes the whole
// list as one option holding a JSON array of {name, value}, in the order
// entered, and passes names and values to rclone exactly as typed.
export interface RcloneRemoteVariable {
	name: string;
	value: string;
}

export function parseRcloneRemoteVariables(raw: string | undefined): RcloneRemoteVariable[] {
	if (!raw?.trim()) return [];
	try {
		const parsed: unknown = JSON.parse(raw);
		if (!Array.isArray(parsed)) return [];
		return parsed.map((row) => {
			const entry = (row && typeof row === "object" ? row : {}) as Record<string, unknown>;
			return {
				name: typeof entry.name === "string" ? entry.name : "",
				value: typeof entry.value === "string" ? entry.value : "",
			};
		});
	} catch {
		// A value that isn't a list shows no rows. It is sent unchanged until
		// the rows are edited, and the backend then asks for them again.
		return [];
	}
}

// An empty list is sent as an empty option, which the backend reads as no
// variables.
export function serializeRcloneRemoteVariables(rows: RcloneRemoteVariable[]): string {
	return rows.length ? JSON.stringify(rows.map(({ name, value }) => ({ name, value }))) : "";
}

// Paths, remote names, variable names, and rclone's own output keep their
// left-to-right order inside a translated sentence. These messages end up in
// plain text (toasts, inline errors, and select options), where there is no
// element to carry dir="ltr", so in a right-to-left language the value is
// wrapped in a left-to-right isolate (U+2066 ... U+2069). An isolate ends at a
// line break, so each line gets its own. Left-to-right languages don't need
// one, and leaving it out there keeps the invisible characters out of text
// that people copy into rclone or a file manager.
export function ltrIsolate(value: string) {
	if (!isRightToLeft()) return value;
	return value.split("\n").map((line) => `\u2066${line}\u2069`).join("\n");
}

// The values a coded Any Rclone Remote error carries next to its English text.
export interface RcloneRemoteErrorFields {
	configFile?: string;
	remote?: string;
	names?: string[];
	detail?: string;
}

const names = (fields: RcloneRemoteErrorFields) => (fields.names ?? []).map(ltrIsolate).join(", ");
// rclone's own output stays as rclone printed it; only the advice after it is
// translated.
const listFailure = (first: string) => `${first}\n${t("ui.protect.rcloneRemoteError.checkConfigAndPassword")}`;

const rcloneRemoteErrorMessages: Record<string, (fields: RcloneRemoteErrorFields) => string> = {
	rclone_remote_config_file_required: () => t("ui.protect.rcloneRemoteError.configFileRequired"),
	rclone_remote_config_file_not_absolute: () => t("ui.protect.rcloneRemoteError.configFileNotAbsolute"),
	rclone_remote_config_encrypted_invalid: () => t("ui.protect.rcloneRemoteError.configEncryptedInvalid"),
	rclone_remote_config_password_required: () => t("ui.protect.rcloneRemoteError.configPasswordRequired"),
	rclone_remote_name_required: () => t("ui.protect.rcloneRemoteError.nameRequired"),
	rclone_remote_name_invalid: () => t("ui.protect.rcloneRemoteError.nameInvalid"),
	rclone_remote_name_separator: () => t("ui.protect.rcloneRemoteError.nameSeparator"),
	rclone_remote_name_drive_letter: () => t("ui.protect.rcloneRemoteError.nameDriveLetter"),
	rclone_remote_name_reserved: () => t("ui.protect.rcloneRemoteError.nameReserved", { name: ltrIsolate(RCLONE_SIDECAR_REMOTE_NAME) }),
	rclone_remote_path_required: () => t("ui.protect.rcloneRemoteError.pathRequired"),
	rclone_remote_path_invalid: () => t("ui.protect.rcloneRemoteError.pathInvalid"),
	rclone_remote_path_includes_remote: (fields) => t("ui.protect.rcloneRemoteError.pathIncludesRemote", { prefix: ltrIsolate(`${fields.remote ?? ""}:`) }),
	rclone_remote_environment_unreadable: () => t("ui.protect.rcloneRemoteError.environmentUnreadable"),
	rclone_remote_environment_name_required: () => t("ui.protect.rcloneRemoteError.environmentNameRequired"),
	rclone_remote_environment_name_equals: () => t("ui.protect.rcloneRemoteError.environmentNameEquals"),
	rclone_remote_environment_nul: () => t("ui.protect.rcloneRemoteError.environmentNul"),
	rclone_remote_environment_duplicate: (fields) => t("ui.protect.rcloneRemoteError.environmentDuplicate", { names: names(fields) }),
	rclone_remote_environment_refused: (fields) => t("ui.protect.rcloneRemoteError.environmentRefused", { names: names(fields) }),
	rclone_remote_config_not_found: (fields) => t("ui.protect.rcloneRemoteError.configNotFound", { path: ltrIsolate(fields.configFile ?? "") }),
	rclone_remote_config_access: () => t("ui.protect.rcloneRemoteError.configAccess"),
	rclone_remote_config_folder: () => t("ui.protect.rcloneRemoteError.configFolder"),
	rclone_remote_config_managed_folder: () => t("ui.protect.rcloneRemoteError.configManagedFolder"),
	rclone_remote_config_not_responding: () => t("ui.protect.rcloneRemoteError.configNotResponding"),
	rclone_remote_list_failed: (fields) => listFailure(ltrIsolate(fields.detail ?? "")),
	rclone_remote_list_unreadable: () => listFailure(t("ui.protect.rcloneRemoteError.listUnreadable")),
	rclone_remote_config_reserved_name: () => t("ui.protect.rcloneRemoteError.configReservedName", { name: ltrIsolate(RCLONE_SIDECAR_REMOTE_NAME) }),
	rclone_remote_not_found: (fields) => t("ui.protect.rcloneRemoteError.remoteNotFound", { remote: ltrIsolate(fields.remote ?? "") }),
	rclone_remote_memory: (fields) => t("ui.protect.rcloneRemoteError.remoteMemory", { remote: ltrIsolate(fields.remote ?? "") }),
	// This wording is intentionally simplified. Restic, not Replicaro, does the
	// compression and encryption, and "may cause data loss" is more cautious
	// than the known effect of layering rclone compress or crypt under Restic.
	// It is phrased this way so users don't have to understand the difference
	// between Replicaro and its backup engine. Keep the wording as it is.
	rclone_remote_compress: () => t("ui.protect.rcloneRemoteError.remoteCompress"),
	rclone_remote_crypt: () => t("ui.protect.rcloneRemoteError.remoteCrypt"),
};

export const rcloneRemoteErrorCodes = Object.keys(rcloneRemoteErrorMessages);

// The translated message for a coded Any Rclone Remote refusal, or undefined
// for any other code, which keeps the backend's own text.
export function rcloneRemoteErrorMessage(code: string | undefined, body: unknown): string | undefined {
	if (!code || !Object.hasOwn(rcloneRemoteErrorMessages, code)) return undefined;
	const raw = body && typeof body === "object" ? body as Record<string, unknown> : {};
	const text = (value: unknown) => typeof value === "string" ? value : undefined;
	return rcloneRemoteErrorMessages[code]({
		configFile: text(raw.configFile),
		remote: text(raw.remote),
		names: Array.isArray(raw.names) ? raw.names.filter((name): name is string => typeof name === "string") : undefined,
		detail: text(raw.detail),
	});
}
