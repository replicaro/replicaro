import { duration } from "../components/ui";
import { t } from "../i18n";
import type { ActivityEntry, DashboardIssue, OperationEntry } from "../types";

export interface TimelineEvent {
	id: string;
	timestamp: string;
	kind: "operation" | "log";
	operationKind?: string;
	operationID?: string;
	// The operation's status. The tag is translated display text, so code that
	// needs the outcome reads this instead.
	operationStatus?: OperationEntry["status"];
	engine?: "restic" | "kopia";
	tone: "ok" | "accent" | "warn" | "danger" | "faint";
	tag: string;
	title: string;
	outputAvailable: boolean;
	issue: boolean;
	isNew?: boolean;
}

export type LogTag = "maintenance" | "prune" | "check" | "failed" | "warning" | "info";

// Sorts an activity log entry into one of a few fixed tags by its level and a
// few English keywords in the (always English) log message.
export function logTag(entry: Pick<ActivityEntry, "level" | "message">): LogTag {
	const text = entry.message.toLowerCase();
	if (text.includes("maintenance")) return "maintenance";
	if (text.includes("prune")) return "prune";
	if (text.includes("check")) return "check";
	if (entry.level === "ERROR") return "failed";
	if (entry.level === "WARN") return "warning";
	return "info";
}

// The tag shown on the timeline. Only this label is translated; the log
// message next to it stays as written.
export function logTagLabel(tag: LogTag) {
	switch (tag) {
		case "maintenance": return t("ui.operation.kind.maintenance");
		case "prune": return t("ui.overview.logTag.prune");
		case "check": return t("ui.overview.logTag.check");
		case "failed": return t("ui.operation.failed");
		case "warning": return t("ui.operation.step.status.warning");
		case "info": return t("ui.overview.logTag.info");
	}
}

export function dashboardIssueEvent(entry: DashboardIssue): TimelineEvent {
	if (entry.kind === "operation") {
		const tag = entry.status === "interrupted" ? t("ui.operation.stopped") : entry.status === "partial" ? t("ui.operation.partial") :
			entry.status === "completed_with_issues" ? t("ui.operation.completedWithIssues") :
			entry.status === "reconnect_required" ? t("ui.operation.reconnectRequired") : t("ui.operation.failed");
		const elapsed = entry.startedAt && entry.finishedAt ? duration(entry.startedAt, entry.finishedAt) : "—";
		return {
			id: entry.id, timestamp: entry.timestamp, kind: "operation", operationKind: entry.operationKind,
			operationID: entry.operationId, operationStatus: entry.status, tone: (entry.severity ?? (entry.status === "completed_with_issues" ? "warning" : "error")) === "warning" ? "warn" : "danger", tag,
			title: `${entry.title}${elapsed !== "—" ? ` · ${elapsed}` : ""}`,
			outputAvailable: entry.outputAvailable, issue: true, isNew: entry.isNew,
		};
	}
	const level = entry.level ?? "WARN";
	return {
		id: entry.id, timestamp: entry.timestamp, kind: "log", tone: (entry.severity ?? (level === "ERROR" ? "error" : "warning")) === "error" ? "danger" : "warn",
		tag: logTagLabel(logTag({ level, message: entry.title })), title: entry.title, outputAvailable: false, issue: true,
		isNew: entry.isNew,
	};
}
