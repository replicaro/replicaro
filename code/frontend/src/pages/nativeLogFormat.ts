type NativeEngine = "restic" | "kopia";
type JsonRecord = Record<string, unknown>;

const resticSnapshotIDPattern = /^[0-9a-f]{64}$/;
const resticShortSnapshotIDPattern = /^[0-9a-f]{8}$/;
const kopiaSnapshotIDPattern = /^[0-9a-f]{32}$/;
const resticExitCodes = new Set([1, 2, 3, 10, 11, 12, 130]);
const rfc3339Pattern = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):[0-5]\d:[0-5]\d(?:\.\d{1,9})?(?:Z|[+-](\d{2}):[0-5]\d)$/;

function record(value: unknown): JsonRecord | null {
    return value !== null && typeof value === "object" && !Array.isArray(value)
        ? value as JsonRecord
        : null;
}

function hasOnlyKeys(value: JsonRecord, allowed: readonly string[]) {
    return Object.keys(value).every((key) => allowed.includes(key));
}

function hasOwn(value: JsonRecord, key: string) {
    return Object.prototype.hasOwnProperty.call(value, key);
}

function optionalString(value: JsonRecord, key: string): string | null | undefined {
    if (!hasOwn(value, key)) return undefined;
    return typeof value[key] === "string" ? value[key] : null;
}

function optionalNumber(value: JsonRecord, key: string, integer = true): number | null | undefined {
    if (!hasOwn(value, key)) return undefined;
    const candidate = value[key];
    if (typeof candidate !== "number" || !Number.isFinite(candidate) || candidate < 0) return null;
    if (integer && !Number.isSafeInteger(candidate)) return null;
    return candidate;
}

function optionalStrings(value: JsonRecord, key: string): string[] | null | undefined {
    if (!hasOwn(value, key)) return undefined;
    const candidate = value[key];
    if (candidate === null) return [];
    return Array.isArray(candidate) && candidate.every((item) => typeof item === "string")
        ? candidate
        : null;
}

function validOptionalStrings(value: JsonRecord, keys: readonly string[]) {
    return keys.every((key) => optionalString(value, key) !== null);
}

function validOptionalNumbers(value: JsonRecord, keys: readonly string[], integer = true) {
    return keys.every((key) => optionalNumber(value, key, integer) !== null);
}

function validOptionalStringArrays(value: JsonRecord, keys: readonly string[]) {
    return keys.every((key) => optionalStrings(value, key) !== null);
}

function validRFC3339(value: string) {
    const match = rfc3339Pattern.exec(value);
    if (!match) return false;
    const year = Number(match[1]);
    const month = Number(match[2]);
    const day = Number(match[3]);
    const hour = Number(match[4]);
    const offsetHour = match[5] === undefined ? 0 : Number(match[5]);
    if (month < 1 || month > 12 || hour > 23 || offsetHour > 23) return false;
    return day >= 1 && day <= new Date(Date.UTC(year, month, 0)).getUTCDate();
}

function plural(count: number, singular: string, pluralForm = `${singular}s`) {
    return `${count} ${count === 1 ? singular : pluralForm}`;
}

function readableBytes(bytes: number) {
    if (bytes < 1024) return plural(bytes, "byte");
    const units = ["KiB", "MiB", "GiB", "TiB", "PiB"];
    let value = bytes;
    let unit = "bytes";
    for (const candidate of units) {
        value /= 1024;
        unit = candidate;
        if (value < 1024) break;
    }
    const rounded = value >= 100 ? value.toFixed(0) : value >= 10 ? value.toFixed(1) : value.toFixed(2);
    return `${rounded.replace(/\.0+$/, "")} ${unit}`;
}

function readableDuration(seconds: number) {
    if (seconds < 60) return `${Number(seconds.toFixed(3))}s`;
    const wholeSeconds = Math.round(seconds);
    const hours = Math.floor(wholeSeconds / 3600);
    const minutes = Math.floor((wholeSeconds % 3600) / 60);
    const remainder = wholeSeconds % 60;
    return [hours ? `${hours}h` : "", minutes ? `${minutes}m` : "", remainder || (!hours && !minutes) ? `${remainder}s` : ""]
        .filter(Boolean)
        .join(" ");
}

function formatResticStatus(value: JsonRecord) {
    const allowed = ["message_type", "seconds_elapsed", "seconds_remaining", "percent_done", "total_files", "files_done", "total_bytes", "bytes_done", "error_count", "current_files"];
    if (!hasOnlyKeys(value, allowed) ||
        !validOptionalNumbers(value, ["seconds_elapsed", "seconds_remaining", "total_files", "files_done", "total_bytes", "bytes_done", "error_count"]) ||
        !validOptionalNumbers(value, ["percent_done"], false) || optionalStrings(value, "current_files") === null) return null;

    const percent = optionalNumber(value, "percent_done", false);
    const filesDone = optionalNumber(value, "files_done");
    const totalFiles = optionalNumber(value, "total_files");
    const bytesDone = optionalNumber(value, "bytes_done");
    const totalBytes = optionalNumber(value, "total_bytes");
    const errors = optionalNumber(value, "error_count");
    const remaining = optionalNumber(value, "seconds_remaining");
    const currentFiles = optionalStrings(value, "current_files");
    const details: string[] = [];
    if (typeof percent === "number" && percent <= 1) details.push(`${Number((percent * 100).toFixed(1))}%`);
    else if (percent !== undefined) return null;
    if (typeof filesDone === "number" && typeof totalFiles === "number") details.push(`${filesDone}/${totalFiles} files`);
    else if (typeof filesDone === "number" || typeof totalFiles === "number") details.push(plural(filesDone ?? totalFiles ?? 0, "file"));
    if (typeof bytesDone === "number" && typeof totalBytes === "number") details.push(`${readableBytes(bytesDone)} / ${readableBytes(totalBytes)}`);
    else if (typeof bytesDone === "number" || typeof totalBytes === "number") details.push(readableBytes(bytesDone ?? totalBytes ?? 0));
    if (typeof remaining === "number" && remaining > 0) details.push(`${readableDuration(remaining)} remaining`);
    if (typeof errors === "number" && errors > 0) details.push(plural(errors, "error"));
    if (currentFiles?.length) details.push(`current: ${currentFiles.join(", ")}`);
    return details.length ? `Restic backup progress: ${details.join(" · ")}` : null;
}

function validResticSummaryShape(value: JsonRecord) {
    const allowed = [
        "message_type", "dry_run", "files_new", "files_changed", "files_unmodified", "dirs_new", "dirs_changed", "dirs_unmodified",
        "data_blobs", "tree_blobs", "data_added", "data_added_packed", "total_files_processed", "total_bytes_processed",
        "backup_start", "backup_end", "total_duration", "snapshot_id",
    ];
    if (!hasOnlyKeys(value, allowed) ||
        !validOptionalNumbers(value, ["files_new", "files_changed", "files_unmodified", "dirs_new", "dirs_changed", "dirs_unmodified", "data_blobs", "tree_blobs", "data_added", "data_added_packed", "total_files_processed", "total_bytes_processed"]) ||
        !validOptionalNumbers(value, ["total_duration"], false) ||
        !validOptionalStrings(value, ["backup_start", "backup_end", "snapshot_id"]) ||
        (hasOwn(value, "dry_run") && typeof value.dry_run !== "boolean")) return false;
    const backupStart = optionalString(value, "backup_start");
    const backupEnd = optionalString(value, "backup_end");
    const snapshotID = optionalString(value, "snapshot_id");
    if (!backupStart || !validRFC3339(backupStart) || (backupEnd !== undefined && (!backupEnd || !validRFC3339(backupEnd)))) return false;
    if (snapshotID !== undefined && (!snapshotID || !resticSnapshotIDPattern.test(snapshotID))) return false;
    return value.dry_run === true || Boolean(snapshotID);
}

function formatResticSummary(value: JsonRecord) {
    if (!validResticSummaryShape(value)) return null;
    const snapshotID = optionalString(value, "snapshot_id");
    const totalFiles = optionalNumber(value, "total_files_processed");
    const totalBytes = optionalNumber(value, "total_bytes_processed");
    const duration = optionalNumber(value, "total_duration", false);
    const filesNew = optionalNumber(value, "files_new");
    const filesChanged = optionalNumber(value, "files_changed");
    const filesUnmodified = optionalNumber(value, "files_unmodified");
    const details: string[] = [];
    if (snapshotID) details.push(`snapshot ${snapshotID}`);
    if (typeof totalFiles === "number") details.push(plural(totalFiles, "file"));
    if (typeof filesNew === "number" && typeof filesChanged === "number" && typeof filesUnmodified === "number") {
        details.push(`${filesNew} new, ${filesChanged} changed, ${filesUnmodified} unchanged`);
    }
    if (typeof totalBytes === "number") details.push(readableBytes(totalBytes));
    if (typeof duration === "number") details.push(readableDuration(duration));
    // A summary can accompany exit 3. Saving a snapshot does not establish a
    // fully successful source capture or change the native operation's exit.
    const heading = value.dry_run === true ? "Restic backup dry run complete" : "Restic snapshot saved";
    return details.length ? `${heading}: ${details.join(" · ")}` : heading;
}

function formatResticError(value: JsonRecord) {
    if (!hasOnlyKeys(value, ["message_type", "error", "during", "item"]) || !validOptionalStrings(value, ["during", "item"])) return null;
    const nestedError = record(value.error);
    if (!nestedError || !hasOnlyKeys(nestedError, ["message"])) return null;
    const message = optionalString(nestedError, "message");
    const during = optionalString(value, "during");
    const item = optionalString(value, "item");
    if (!message || !during || !item) return null;
    return `Restic error: ${message} · during ${during} · item ${item}`;
}

function formatResticVerbose(value: JsonRecord) {
    const allowed = ["message_type", "action", "item", "duration", "data_size", "data_size_in_repo", "metadata_size", "metadata_size_in_repo", "total_files"];
    if (!hasOnlyKeys(value, allowed) || !validOptionalStrings(value, ["action", "item"]) ||
        !validOptionalNumbers(value, ["data_size", "data_size_in_repo", "metadata_size", "metadata_size_in_repo", "total_files"]) ||
        !validOptionalNumbers(value, ["duration"], false)) return null;
    const action = optionalString(value, "action");
    const item = optionalString(value, "item");
    const size = optionalNumber(value, "data_size");
    const duration = optionalNumber(value, "duration", false);
    if (!action || !["new", "unchanged", "modified", "scan_finished"].includes(action) || (action !== "scan_finished" && !item)) return null;
    const details = [item ?? "", typeof size === "number" ? readableBytes(size) : "", typeof duration === "number" ? readableDuration(duration) : ""].filter(Boolean);
    return `Restic ${action}${details.length ? `: ${details.join(" · ")}` : ""}`;
}

const resticSnapshotSummaryKeys = [
    "backup_start", "backup_end", "files_new", "files_changed", "files_unmodified", "dirs_new", "dirs_changed", "dirs_unmodified",
    "data_blobs", "tree_blobs", "data_added", "data_added_packed", "total_files_processed", "total_bytes_processed",
];

function validResticSnapshotSummary(value: unknown) {
    const summary = record(value);
    if (!summary || !hasOnlyKeys(summary, resticSnapshotSummaryKeys) ||
        !validOptionalStrings(summary, ["backup_start", "backup_end"]) ||
        !validOptionalNumbers(summary, resticSnapshotSummaryKeys.slice(2))) return false;
    const backupStart = optionalString(summary, "backup_start");
    const backupEnd = optionalString(summary, "backup_end");
    return (backupStart === undefined || Boolean(backupStart && validRFC3339(backupStart))) &&
        (backupEnd === undefined || Boolean(backupEnd && validRFC3339(backupEnd)));
}

function resticSnapshotID(value: unknown) {
    const snapshot = record(value);
    const allowed = ["time", "parent", "tree", "paths", "hostname", "username", "uid", "gid", "excludes", "tags", "program_version", "summary", "id", "short_id"];
    if (!snapshot || !hasOnlyKeys(snapshot, allowed) ||
        !validOptionalStrings(snapshot, ["time", "parent", "tree", "hostname", "username", "program_version", "id", "short_id"]) ||
        !validOptionalNumbers(snapshot, ["uid", "gid"]) ||
        !validOptionalStringArrays(snapshot, ["paths", "excludes", "tags"]) ||
        (hasOwn(snapshot, "summary") && !validResticSnapshotSummary(snapshot.summary))) return null;
    const id = optionalString(snapshot, "id");
    const shortID = optionalString(snapshot, "short_id");
    const time = optionalString(snapshot, "time");
    const tree = optionalString(snapshot, "tree");
    const paths = optionalStrings(snapshot, "paths");
    if (!id || !resticSnapshotIDPattern.test(id) || !shortID || !resticShortSnapshotIDPattern.test(shortID) || shortID !== id.slice(0, 8) ||
        !time || !validRFC3339(time) || !tree || !resticSnapshotIDPattern.test(tree) || !paths?.length) return null;
    return id;
}

function formatResticRetention(value: unknown[], summaryOnly = false) {
    if (value.length === 0) return null;
    const lines: string[] = [];
    for (const candidate of value) {
        const group = record(candidate);
        if (!group || !hasOnlyKeys(group, ["tags", "host", "paths", "keep", "remove", "reasons"]) ||
            !["tags", "host", "paths", "keep", "remove", "reasons"].every((key) => hasOwn(group, key))) return null;
        const kept = group.keep === null ? [] : Array.isArray(group.keep) ? group.keep : null;
        const selectedForRemoval = group.remove === null ? [] : Array.isArray(group.remove) ? group.remove : null;
        const reasons = group.reasons === null ? [] : Array.isArray(group.reasons) ? group.reasons : null;
        const paths = optionalStrings(group, "paths");
        const tags = optionalStrings(group, "tags");
        const host = optionalString(group, "host");
        if (!kept || !selectedForRemoval || !reasons || paths === null || paths === undefined || tags === null || host === null || host === undefined) return null;
        const keptIDs = kept.map(resticSnapshotID);
        const selectedIDs = selectedForRemoval.map(resticSnapshotID);
        if (keptIDs.includes(null) || selectedIDs.includes(null)) return null;
        const keptIDSet = new Set(keptIDs as string[]);
        const selectedIDSet = new Set(selectedIDs as string[]);
        if (keptIDSet.size !== keptIDs.length || selectedIDSet.size !== selectedIDs.length ||
            [...selectedIDSet].some((id) => keptIDSet.has(id))) return null;

        const reasonByID = new Map<string, string[]>();
        for (const candidateReason of reasons) {
            const reason = record(candidateReason);
            if (!reason || !hasOnlyKeys(reason, ["snapshot", "matches"]) || !hasOwn(reason, "snapshot") || !hasOwn(reason, "matches")) return null;
            const id = resticSnapshotID(reason.snapshot);
            const matches = optionalStrings(reason, "matches");
            if (!id || !matches?.length || !keptIDSet.has(id) || reasonByID.has(id)) return null;
            reasonByID.set(id, matches);
        }
        if (reasonByID.size !== keptIDSet.size) return null;

        const context = [paths.length ? `source ${paths.join(", ")}` : "", host ? `host ${host}` : "", tags?.length ? `tags ${tags.join(", ")}` : ""].filter(Boolean);
        lines.push(`Restic retention policy: kept ${plural(kept.length, "snapshot")} · selected ${plural(selectedForRemoval.length, "snapshot")} for removal${context.length ? ` · ${context.join(" · ")}` : ""}`);
        if (summaryOnly) continue;
        for (const id of keptIDs as string[]) {
            const matches = reasonByID.get(id);
            lines.push(`Policy kept snapshot ${id}${matches?.length ? `: ${matches.join(", ")}` : ""}`);
        }
        for (const id of selectedIDs as string[]) lines.push(`Selected snapshot ${id} for removal`);
    }
    return lines.join("\n");
}

function formatResticExitError(value: JsonRecord) {
    if (value.message_type !== "exit_error" || !hasOnlyKeys(value, ["message_type", "code", "message"])) return null;
    const code = optionalNumber(value, "code");
    const message = optionalString(value, "message");
    // Keep unknown future exit semantics native instead of assigning current-version meaning to them.
    return typeof code === "number" && resticExitCodes.has(code) && message
        ? `Restic error (exit code ${code}): ${message}`
        : null;
}

function formatResticBackup(value: unknown) {
    const row = record(value);
    if (!row || typeof row.message_type !== "string") return null;
    switch (row.message_type) {
    case "status":
        return formatResticStatus(row);
    case "summary":
        return formatResticSummary(row);
    case "error":
        return formatResticError(row);
    case "exit_error":
        return formatResticExitError(row);
    case "verbose_status":
        return formatResticVerbose(row);
    case "excluded_item": {
        if (!hasOnlyKeys(row, ["message_type", "item"])) return null;
        const item = optionalString(row, "item");
        return item ? `Restic excluded: ${item}` : null;
    }
    default:
        return null;
    }
}

function formatResticRetentionRecord(value: unknown, summaryOnly = false) {
    if (Array.isArray(value)) return formatResticRetention(value, summaryOnly);
    const row = record(value);
    return row ? formatResticExitError(row) : null;
}

function validStringMap(value: unknown) {
    const candidate = record(value);
    return Boolean(candidate && Object.values(candidate).every((item) => typeof item === "string"));
}

function formatKopia(value: unknown) {
    const snapshot = record(value);
    const topLevelKeys = ["id", "source", "description", "startTime", "endTime", "rootEntry", "tags"];
    if (!snapshot || !hasOnlyKeys(snapshot, topLevelKeys) ||
        !validOptionalStrings(snapshot, ["id", "description", "startTime", "endTime"]) ||
        (hasOwn(snapshot, "tags") && !validStringMap(snapshot.tags))) return null;
    const id = optionalString(snapshot, "id");
    const startTime = optionalString(snapshot, "startTime");
    const endTime = optionalString(snapshot, "endTime");
    const source = record(snapshot.source);
    const rootEntry = record(snapshot.rootEntry);
    if (!id || !kopiaSnapshotIDPattern.test(id) || !startTime || !validRFC3339(startTime) || !endTime || !validRFC3339(endTime) || !source || !rootEntry ||
        !hasOnlyKeys(source, ["host", "userName", "path"]) || !validOptionalStrings(source, ["host", "userName", "path"]) ||
        !hasOnlyKeys(rootEntry, ["name", "type", "mode", "mtime", "uid", "gid", "obj", "summ"]) ||
        !validOptionalStrings(rootEntry, ["name", "type", "mode", "mtime", "obj"]) || !validOptionalNumbers(rootEntry, ["uid", "gid"])) return null;
    const path = optionalString(source, "path");
    const host = optionalString(source, "host");
    const user = optionalString(source, "userName");
    const objectID = optionalString(rootEntry, "obj");
    const rootType = optionalString(rootEntry, "type");
    const modifiedTime = optionalString(rootEntry, "mtime");
    const summary = record(rootEntry.summ);
    if (!path || !objectID || (rootType !== undefined && rootType !== "d") ||
        (modifiedTime !== undefined && (!modifiedTime || !validRFC3339(modifiedTime))) || !summary ||
        !hasOnlyKeys(summary, ["size", "files", "symlinks", "dirs", "maxTime", "numFailed", "errors", "numIgnoredErrors"]) ||
        !validOptionalNumbers(summary, ["size", "files", "symlinks", "dirs", "numFailed"]) ||
        !validOptionalStrings(summary, ["maxTime"]) ||
        !["size", "files", "numFailed"].every((key) => hasOwn(summary, key))) return null;
    const size = optionalNumber(summary, "size");
    const files = optionalNumber(summary, "files");
    const failures = optionalNumber(summary, "numFailed");
    const maxTime = optionalString(summary, "maxTime");
    if (typeof size !== "number" || typeof files !== "number" || typeof failures !== "number" ||
        (maxTime !== undefined && (!maxTime || !validRFC3339(maxTime)))) return null;

    const sourceOwner = host && user ? `${user}@${host}` : user || host;
    const sourceIdentity = sourceOwner ? `${sourceOwner}:${path}` : path;
    const details = [
        `snapshot ${id}`,
        plural(files, "file"),
        readableBytes(size),
        failures > 0 ? plural(failures, "failed item") : "",
        `source ${sourceIdentity}`,
    ].filter(Boolean);
    if (hasOwn(summary, "errors") && (!Array.isArray(summary.errors) || !summary.errors.every((item) => {
        const error = record(item);
        return error && hasOnlyKeys(error, ["path", "error"]) && typeof error.path === "string" && typeof error.error === "string";
    }))) return null;
    const ignored = optionalNumber(summary, "numIgnoredErrors");
    if (ignored === null) return null;
    if (typeof ignored === "number" && ignored > 0) details.push(plural(ignored, "ignored error"));
    const errors = Array.isArray(summary.errors) ? summary.errors : [];
    const errorLines = errors.map((item) => {
        const error = record(item);
        return error && typeof error.path === "string" && typeof error.error === "string"
            ? `Kopia item error: ${error.path} · ${error.error}` : readableNativeJSON(item);
    });
    // The manifest can precede a failed final flush; this text reports the
    // record without asserting that publication or the native command succeeded.
    return [`Kopia snapshot reported: ${details.join(" · ")}`, ...errorLines].join("\n");
}

// Keep every key/value in records whose native shape has no specialized
// summary. This is presentation only: unknown fields and diagnostics remain
// visible and cannot supply backup-success or retention evidence.
// Parsed JSON has no cycles. Use an explicit stack when native nesting exceeds
// the JavaScript serializer's call stack, preserving the same round-trip check.
function serializeNativeJSON(value: unknown): string {
    try { return JSON.stringify(value); } catch { /* deeply nested JSON */ }
    const output: string[] = [];
    const pending: Array<{ value: unknown } | { text: string }> = [{ value }];
    while (pending.length) {
        const next = pending.pop()!;
        if ("text" in next) { output.push(next.text); continue; }
        const item = next.value;
        if (item === null || typeof item !== "object") { output.push(JSON.stringify(item)); continue; }
        const array = Array.isArray(item);
        const entries = array ? item.map((value, index) => [String(index), value] as const) : Object.entries(item);
        output.push(array ? "[" : "{");
        pending.push({ text: array ? "]" : "}" });
        for (let index = entries.length - 1; index >= 0; index--) {
            const [key, value] = entries[index];
            pending.push({ value });
            if (!array) pending.push({ text: JSON.stringify(key) + ":" });
            if (index > 0) pending.push({ text: "," });
        }
    }
    return output.join("");
}

function readableNativeJSON(value: unknown, depth = 0): string {
    if (depth > 20) return serializeNativeJSON(value);
    if (value === null) return "null";
    if (typeof value !== "object") return typeof value === "string" ? value : String(value);
    const indent = "  ".repeat(depth);
    if (Array.isArray(value)) {
        if (!value.length) return "(none)";
        return value.map((item, index) => `${indent}Item ${index + 1}: ${readableNativeJSON(item, depth + 1)}`).join("\n\n");
    }
    const entries = Object.entries(value as JsonRecord);
    if (!entries.length) return "(empty record)";
    return entries.map(([key, item]) => {
        const label = key.replace(/_/g, " ").replace(/([a-z])([A-Z])/g, "$1 $2");
        const nested = item !== null && typeof item === "object";
        return `${indent}${label}: ${nested ? "\n" : ""}${readableNativeJSON(item, nested ? depth + 1 : depth)}`;
    }).join("\n");
}

function canonicalNumber(token: string) {
    const [mantissa, exponent = "0"] = token.toLowerCase().split("e");
    const [whole, fraction = ""] = mantissa.split(".");
    let digits = (whole + fraction).replace(/^(-?)0+/, "$1");
    let scale = BigInt(exponent) - BigInt(fraction.length);
    while (digits.endsWith("0")) { digits = digits.slice(0, -1); scale++; }
    return !digits ? "0" : digits === "-" ? "-0" : `${digits}e${scale}`;
}

function compactJSONWhitespace(text: string) {
    // Normalize equivalent string and exact decimal spellings. Comparing both
    // sides still rejects duplicate keys and any numeric precision loss.
    return text.replace(/"(?:[^"\\]|\\[\s\S])*"|-?\d+(?:\.\d+)?(?:[eE][+-]?\d+)?|\s+/g, (token) =>
        token.startsWith('"') ? JSON.stringify(JSON.parse(token)) : token.trim() ? canonicalNumber(token) : "");
}

function plainDiagnostics(text: string, diagnostics?: string[]) {
    if (!diagnostics) return;
    for (const line of text.split(/\r?\n/)) {
        // Inspect actual text records, never keyword-match paths or fields in
        // serialized JSON. Bracketed native severity prefixes are text records.
        const trimmed = line.trim();
        if (/^\s*["{]/.test(line) || /^\s*\[(?!WARN(?:ING)?\]|ERROR\]|FATAL\])/i.test(line)) continue;
        if (/\b(?:error|errors|warning|warnings|warn|fatal)\b/i.test(trimmed)) diagnostics.push(trimmed);
    }
}

function structuredDiagnostics(value: unknown, engine: string, diagnostics: string[]) {
    // Complete reconstructed records may be deeply nested. Walk iteratively
    // so a valid diagnostic is not lost to a display-depth or call-stack limit.
    const pending: Array<{value: unknown; level?: string; path: string}> = [{value, path: ""}];
    while (pending.length) {
        const {value: item, level, path} = pending.pop()!;
        if (item === null || item === false || item === 0 || item === "") continue;
        if (Array.isArray(item)) {
            for (let index = item.length - 1; index >= 0; index--) pending.push({value:item[index], level, path});
        } else if (typeof item === "object") {
            const row = item as JsonRecord;
            const location = typeof row.path === "string" ? row.path : typeof row.item === "string" ? row.item : path;
            const entries = Object.entries(row);
            for (let index = entries.length - 1; index >= 0; index--) {
                const [key, child] = entries[index];
                if (["path", "item"].includes(key)) continue;
                const severity = /^(?:error|errors)$/.test(key) ? "error" : /^(?:warning|warnings)$/.test(key) ? "warning" : level;
                if (severity || (child !== null && typeof child === "object")) pending.push({value:child, level:severity, path:location});
            }
        } else if (level) diagnostics.push(`${engine} ${level}: ${path ? path + " · " : ""}${String(item)}`);
    }
}

export const retentionSummaryUnavailable = "Restic retention summary unavailable for this incomplete or unsupported record. See Raw in the completed log for details.";

export interface LogPresentation { concise?: boolean; maintenanceOnly?: boolean; operationKind?: string }

function formatNativeLogLine(engine: NativeEngine | "replicaro", operationKind: string, line: string, diagnostics?: string[], concise = false, hiddenSection = false) {
    const unparsed = () => {
        if (hiddenSection) diagnostics?.push(`${line}\nSee Raw in the completed log for this unparsed section output.`);
        return line;
    };
    let value: unknown;
    try { value = JSON.parse(line); } catch { return unparsed(); }
    // Round-trip equality rejects duplicate keys and imprecise integers. Native
    // indentation is irrelevant, but ambiguous data must retain its exact text.
    if (compactJSONWhitespace(serializeNativeJSON(value)) !== compactJSONWhitespace(line)) return unparsed();
    if (value === null || typeof value !== "object") return line;
    if (engine === "replicaro") {
        if (diagnostics) structuredDiagnostics(value, "Replicaro", diagnostics);
        return line;
    }
    const retentionContext = operationKind === "retention" || operationKind === "backup" || operationKind === "live";
    const formatted = engine === "restic"
        ? Array.isArray(value) ? formatResticRetentionRecord(value, concise && retentionContext)
            : operationKind === "backup" || operationKind === "live" ? formatResticBackup(value)
            : record(value) ? formatResticExitError(value as JsonRecord) : null
        : formatKopia(value);
    // Summaries intentionally condense native facts, including rounded sizes.
    // Reuse the bounded key/value view for exact fields rather than maintaining
    // a second per-shape list of facts a summary may omit.
    if (diagnostics) {
        const label = engine === "restic" ? "Restic" : "Kopia";
        if (formatted?.startsWith("Restic error")) diagnostics.push(formatted);
        else if (engine === "kopia" && formatted) {
            const stats = record(record(record(value)?.rootEntry)?.summ);
            const itemErrors = formatted.split("\n").filter(line => line.startsWith("Kopia item error:"));
            diagnostics.push(...itemErrors);
            const failed = stats?.numFailed;
            const ignored = stats?.numIgnoredErrors;
            if (typeof failed === "number" && failed > itemErrors.length) diagnostics.push(`Kopia reported ${plural(failed, "failed item")}.`);
            if (typeof ignored === "number" && ignored > 0) diagnostics.push(`Kopia reported ${plural(ignored, "ignored error")}.`);
        } else if (formatted?.startsWith("Restic backup progress:")) {
            const count = record(value)?.error_count;
            if (typeof count === "number" && count > 0) diagnostics.push(`Restic reported ${plural(count, "error")}.`);
        } else if (!formatted) structuredDiagnostics(value, label, diagnostics);
    }
    if (concise && engine === "kopia" && (operationKind === "backup" || operationKind === "live") && formatted) return formatted;
    if (concise && engine === "restic") {
        if (operationKind === "retention" && !Array.isArray(value)) {
            const nativeError = record(value)?.message_type;
            if (nativeError !== "error" && nativeError !== "exit_error") return retentionSummaryUnavailable;
            if (formatted) return formatted;
        }
        if (Array.isArray(value) && retentionContext) return formatted ?? retentionSummaryUnavailable;
        return formatted ?? line;
    }
    const details = `${engine === "restic" ? "Restic" : "Kopia"} details:\n${readableNativeJSON(value)}`;
    return formatted ? `${formatted}\n\n${details}` : details;
}

export function formatNativeLogText(engine: string, operationKind: string, text: string, diagnostics?: string[], concise = false, includeFailureText = false, hiddenSection = false) {
    if (engine !== "restic" && engine !== "kopia" && engine !== "replicaro") return text;
    // Live input stays within its existing buffer. Completed input contains
    // records reconstructed across transport pages. Genuinely malformed
    // records never cause adjoining native diagnostics to vanish.
    const visibleText = engine === "kopia" ? text.replace(/\r\n?/g, "\n") : text;
    const lines = visibleText.split("\n");
    const result: string[] = [];
    const snapshots: string[] = [];
    for (let index = 0; index < lines.length; index++) {
        const line = lines[index];
        if (!/^\s*[[{]/.test(line)) {
            if (includeFailureText && line.trim()) diagnostics?.push(line.trim());
            else plainDiagnostics(line, diagnostics);
            result.push(line); continue;
        }
        let candidate = line;
        let end = index;
        let depth = 0;
        let quoted = false;
        let escaped = false;
        const consume = (part: string) => {
            for (const char of part) {
                if (escaped) { escaped = false; continue; }
                if (quoted && char === "\\") { escaped = true; continue; }
                if (char === '"') { quoted = !quoted; continue; }
                if (!quoted && (char === "{" || char === "[")) depth++;
                if (!quoted && (char === "}" || char === "]")) depth--;
            }
        };
        consume(line);
        while (depth > 0 && !quoted && end + 1 < lines.length) {
            const next = lines[end + 1];
            if (!/^\s*(?:["{}[\]\d\-,]|true\b|false\b|null\b|$)/.test(next)) break;
            candidate += "\n" + next;
            consume(next);
            end++;
        }
        // Legal nested records need no indentation. For a genuinely invalid
        // aggregate, recover diagnostics without promoting nested arrays into
        // independent retention results: the outer record is still incomplete.
        if (end > index && diagnostics) {
            try { JSON.parse(candidate); } catch {
                for (const fragment of lines.slice(index + 1, end + 1)) {
                    formatNativeLogLine(engine, operationKind, fragment, diagnostics, concise);
                }
            }
        }
        const formatted = formatNativeLogLine(engine, operationKind, candidate, diagnostics, concise, hiddenSection);
        const trailingCR = engine === "restic" && candidate.endsWith("\r") ? "\r" : "";
        if (formatted === candidate) { plainDiagnostics(candidate, diagnostics); result.push(concise && engine === "restic" && operationKind === "retention" && /^\s*(?:\{|\[(?!WARN(?:ING)?\]|ERROR\]|FATAL\]))/i.test(candidate) ? retentionSummaryUnavailable : candidate); index = end; continue; }
        if (concise && engine === "kopia" && (operationKind === "backup" || operationKind === "live") && formatted.startsWith("Kopia snapshot reported:")) snapshots.push(formatted);
        else result.push(formatted + trailingCR);
        index = end;
    }
    return [...snapshots, ...result].join("\n");
}

const operationSection = /(^|(?:\r?\n){2})(\[(restic|kopia|replicaro) · ([^\]\r\n]+) · ([^\]\r\n]+)\])(\r?\n)([\s\S]*?)(?=(?:\r?\n){2}\[(?:restic|kopia|replicaro) · [^\]\r\n]+ · [^\]\r\n]+\]|$)/g;

export function completedBackupHeading(header: string, completedWithIssues: boolean) {
    return completedWithIssues
        ? header.replace(/^(\[(?:restic|kopia|replicaro) · (?:backup|backup and retention) · )failed(\]\r?)$/, "$1completed with issues$2")
        : header;
}

const trimSectionLines = (text: string) => text.replace(/^(?:[ \t]*\r?\n)+|(?:\r?\n[ \t]*)+$/g, "");

export function formatOperationLogText(text: string, pageEngine?: NativeEngine, diagnostics?: string[], completedWithIssues = false, presentation: LogPresentation = {}) {
    // Files retain bounded native bodies without rewriting engine content. Interpret only Replicaro's
    // exact section boundary at display time; malformed or future text remains
    // visible verbatim rather than being hidden by the formatter.
	const nativeComponent = /^\[(restic|kopia) · /m.exec(text)?.[1];
    const cleanBody = (body: string) => presentation.concise ? trimSectionLines(body) : body;
	const firstSection = /(^|(?:\r?\n){2})\[(?:restic|kopia|replicaro) · [^\]\r\n]+ · [^\]\r\n]+\]\r?\n/m.exec(text);
	const leadingBodyEnd = firstSection?.index ?? text.length;
	const leadingBody = text.slice(0, leadingBodyEnd);
	const formattedLeadingBody = pageEngine && leadingBodyEnd > 0
		? formatNativeLogText(pageEngine, presentation.operationKind ?? "live", leadingBody, diagnostics, presentation.concise)
		: leadingBody;
	const sectionText = text.slice(leadingBodyEnd);
	const result = (presentation.maintenanceOnly ? "" : cleanBody(formattedLeadingBody)) + sectionText.replace(operationSection, (_section, prefix: string, header: string, component: string,
		sectionKind: string, status: string, newline: string, body: string) => {
        const displayHeader = completedBackupHeading(header, completedWithIssues);
        const failedSection = ["failed", "warning", "interrupted"].includes(status);
        const hiddenSection = presentation.concise && ["metadata_cache", "backup_admission"].includes(sectionKind);
        if (failedSection) {
            if (presentation.maintenanceOnly || hiddenSection) diagnostics?.push(`${component} ${sectionKind}: ${status}`);
            if (!presentation.maintenanceOnly && !/^\s*[[{]/.test(body) && !body.includes("Restic retention details")) {
                body.split(/\r?\n/).map(line => line.trim()).filter(Boolean).forEach(line => diagnostics?.push(line));
            }
        }
        if (hiddenSection) {
            formatNativeLogText(component, sectionKind, body, diagnostics, presentation.concise, failedSection, true);
            return "";
        }
        if (presentation.maintenanceOnly) {
            // Diagnose every original section before selecting the readable body.
            const visible = formatNativeLogText(component, sectionKind, body, diagnostics, presentation.concise, failedSection);
            return (component === "kopia" && sectionKind === "maintenance") ||
                (component === "restic" && (sectionKind === "prune" || sectionKind === "maintenance"))
                ? `${presentation.concise && prefix ? "\n\n" : prefix}${displayHeader}${newline}${cleanBody(visible)}` : "";
        }
		if (component === "replicaro") {
			let visibleBody: string | undefined;
			if (sectionKind === "backup_admission" && status === "succeeded" && nativeComponent) {
				visibleBody = `backup admitted to the engine (${nativeComponent})`;
			} else if (sectionKind === "metadata_cache" && status === "skipped") {
				visibleBody = "metadata cache will generate when user visits restore or file history page";
			} else if (sectionKind === "desktop/webhook notification") {
				if (status === "succeeded") visibleBody = "desktop/webhook notification delivery completed";
				else if (status === "warning") visibleBody = `desktop/webhook notification delivery failed: ${body}`;
				else if (status === "skipped") visibleBody = `desktop/webhook notification settings unavailable: ${body.replace(/^notification settings unavailable: /, "")}`;
				else if (status === "failed") visibleBody = body.toLowerCase().includes("did not complete")
					? body : `desktop/webhook notification delivery did not complete: ${body}`;
			}
			// Only established presentation substitutions replace a body. Unknown
			// Replicaro sections and their diagnostic detail stay visible verbatim.
			return `${presentation.concise && prefix ? "\n\n" : prefix}${displayHeader}${newline}${cleanBody(visibleBody ?? body)}`;
		}
		const kind = component === "kopia" && sectionKind === "backup and retention" ? "backup" : sectionKind;
		return `${presentation.concise && prefix ? "\n\n" : prefix}${displayHeader}${newline}${cleanBody(formatNativeLogText(component as NativeEngine, kind, body, diagnostics, presentation.concise))}`;
	});
    return presentation.concise ? trimSectionLines(result) : result;
}

export interface ReadableLogContext {
    operationStatus?: string;
    operationKind?: string;
    engine?: NativeEngine;
    partial?: boolean;
}

export function formatReadableLog(text: string, context: ReadableLogContext, live = false, formattedBody?: string, pageDiagnostics?: readonly string[]) {
    const diagnostics: string[] = pageDiagnostics ? [...pageDiagnostics] : [];
    const completedWithIssues = context.operationKind === "backup" && context.operationStatus === "completed_with_issues";
    const presentation = { concise: true, operationKind: context.operationKind, maintenanceOnly: context.operationKind === "prune" || context.operationKind === "maintenance" };
    const formatted = pageDiagnostics && formattedBody !== undefined ? formattedBody : live && context.engine
        ? formatNativeLogText(context.engine, context.operationKind ?? "live", text, diagnostics, true)
        : formatOperationLogText(text, context.engine, diagnostics, completedWithIssues, presentation);
    // Paged bodies have already qualified actual source headings before
    // native JSON values were decoded. Never search decoded data for headings.
    const body = live && presentation.maintenanceOnly && context.engine
        ? `[${context.engine} · maintenance]\n${formatted}`
        : formattedBody ?? formatted;
    const unique = [...new Set(diagnostics.map(value => value.trim()).filter(Boolean))];
    // A summary stays short; all omitted text remains in the detailed log.
    const concise = unique.slice(0, 12).map(value => value.length > 500 ? value.slice(0, 500) + "… (see Raw in the completed log)" : value);
    if (unique.length > 12) concise.push("Additional errors or warnings are in Raw in the completed log.");
    const summary = ["[errors / warnings summary]",
        ...(live ? ["Errors and warnings shown below cover the currently loaded log output."] : []),
        ...(concise.length ? concise : ["No errors or warnings identified in the loaded output."]),
    ];
    return `${summary.join("\n")}\n\n${trimSectionLines(body)}`.trimEnd();
}
