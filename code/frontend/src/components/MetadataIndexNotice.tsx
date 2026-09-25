import { t } from "../i18n";
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
		? t("ui.metadataIndex.paused")
		: indexingContents
			? known > 0 ? t("ui.metadataIndex.contentsProgress", { processed, known }) : t("ui.metadataIndex.contents")
			: t("ui.metadataIndex.snapshots");
	return <div className="inline-notice metadata-index-notice" role="status">
		{!paused && <span className="spinner" />}
		<span><strong>{message}</strong>
			{blockSearchAndBrowse && <span>{t("ui.components.metadataindexnotice.search.and.browsing.is.disabled.while.indexing.completes")}</span>}
			<span>{t("ui.components.metadataindexnotice.you.can.safely.close.this.page.if.you.need.to.indexing.will.resume.in")}</span>
		</span>
	</div>;
}
