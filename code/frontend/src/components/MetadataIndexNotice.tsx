import type { MetadataIndexState } from "../types";

type MetadataStage = "" | "queued" | "preparing" | "refreshing_headers" | "indexing_entries";

export function MetadataIndexNotice({
	active,
	paused,
	stage,
	completeHeaderListing,
	state,
	blockSearchAndBrowse = false,
}: {
	active: boolean;
	paused: boolean;
	stage: MetadataStage;
	completeHeaderListing: boolean;
	state: MetadataIndexState | null;
	blockSearchAndBrowse?: boolean;
}) {
	if (!active) return null;
	const known = state?.knownSnapshots ?? 0;
	const processed = Math.min(known, (state?.readySnapshots ?? 0) + (state?.failedSnapshots ?? 0));
	const indexingContents = stage === "indexing_entries" || (state?.headerValid && !completeHeaderListing);
	const message = paused
		? "Indexing paused due to active vault work..."
		: indexingContents
			? `Indexing snapshot contents…${known > 0 ? ` ${processed}/${known}` : ""}`
			: "Indexing snapshots…";
	return <div className="inline-notice metadata-index-notice" role="status">
		{!paused && <span className="spinner" />}
		<span><strong>{message}</strong>
			{blockSearchAndBrowse && <span>Search and browsing is disabled while indexing completes.</span>}
			<span>You can safely close this page if you need to. Indexing will resume in the background.</span>
		</span>
	</div>;
}
