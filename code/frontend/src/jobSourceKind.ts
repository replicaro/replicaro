import { useCallback, useEffect, useState } from "react";

import { lookupSourcePathKind } from "./services/api";
import type { BackupJob, SourcePathKind } from "./types";

// How long the new-job form waits after the last change to a typed source
// before asking the backend what it is.
const SOURCE_KIND_LOOKUP_DELAY_MS = 400;

// A job whose source is a single file. Folder jobs have no sourceKind, and an
// imported job that isn't bound yet has "unknown"; both are folder jobs here.
export function isSingleFileJob(job: Pick<BackupJob, "sourceKind">) {
	return job.sourceKind === "file";
}

// The settings a single file can't use. The new-job and edit forms show and
// send these values for a file source, and the backend refuses to save a file
// job with anything else.
export const singleFileSourceFilters = {
	excludes: "",
	skipNoBackupFolders: false,
	skipCacheDirFolders: false,
	skipCustomMarker: false,
	skipMarkerFileName: "",
};

// What the new-job form knows about each source it shows: whether it is a
// single file. A path returned by the picker comes with its kind (record). A
// typed path is looked up once typing pauses, through the directories
// endpoint's kind lookup.
//
// The answer is only a hint for the form. A path with no answer yet, a lookup
// that fails and one that never answers (a dead share) all count as "not a
// file", so typing and saving a folder job never wait on it, and there is no
// error text for it. The backend decides what the path is when the job is
// saved; a file job saved before its answer arrived, with exclude or marker
// settings, is refused there with the backend's own message.
//
// Answers are kept per path for as long as the form is open (reset clears
// them when a new form opens), so a path that was looked up isn't asked again.
export function useJobSourceKinds(sources: string[], active: boolean) {
	const [kinds, setKinds] = useState<Record<string, SourcePathKind>>({});
	const pending = active ? Array.from(new Set(sources.filter((source) => source && kinds[source] === undefined))) : [];
	// One string, so the effect restarts only when the set of unanswered paths
	// changes, not on every render.
	const pendingKey = JSON.stringify(pending);

	useEffect(() => {
		const paths = JSON.parse(pendingKey) as string[];
		if (paths.length === 0) return;
		const controller = new AbortController();
		const timer = window.setTimeout(() => {
			for (const path of paths) {
				void (async () => {
					try {
						const kind = (await lookupSourcePathKind(path, controller.signal))?.kind;
						if (controller.signal.aborted || (kind !== "file" && kind !== "folder" && kind !== "neither")) return;
						setKinds((current) => ({ ...current, [path]: kind }));
					} catch {
						// No answer counts as "not a file"; see above.
					}
				})();
			}
		}, SOURCE_KIND_LOOKUP_DELAY_MS);
		return () => {
			window.clearTimeout(timer);
			controller.abort();
		};
	}, [pendingKey]);

	const record = useCallback((path: string, kind: SourcePathKind) => {
		setKinds((current) => ({ ...current, [path]: kind }));
	}, []);
	const reset = useCallback(() => setKinds({}), []);
	const isFile = useCallback((path: string) => kinds[path] === "file", [kinds]);
	return { isFile, record, reset };
}
