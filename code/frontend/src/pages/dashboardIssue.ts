import { duration } from "../components/ui";
import type { ActivityEntry, DashboardIssue } from "../types";

export interface TimelineEvent {
	id: string;
	timestamp: string;
	kind: "operation" | "log";
	operationKind?: string;
	operationID?: string;
	engine?: "restic" | "kopia";
	tone: "ok" | "accent" | "warn" | "danger" | "faint";
	tag: string;
	title: string;
	outputAvailable: boolean;
	issue: boolean;
	isNew?: boolean;
}

export function logTag(entry: Pick<ActivityEntry, "level" | "message">) {
	const text = entry.message.toLowerCase();
	if (text.includes("maintenance")) return "maint";
	if (text.includes("prune")) return "prune";
	if (text.includes("check")) return "check";
	if (entry.level === "ERROR") return "failed";
	if (entry.level === "WARN") return "warn";
	return "info";
}

export function dashboardIssueEvent(entry: DashboardIssue): TimelineEvent {
	if (entry.kind === "operation") {
		const tag = entry.status === "interrupted" ? "stopped" : entry.status === "partial" ? "partial" :
			entry.status === "completed_with_issues" ? "completed with issues" :
			entry.status === "reconnect_required" ? "Reconnect required" : "failed";
		const elapsed = entry.startedAt && entry.finishedAt ? duration(entry.startedAt, entry.finishedAt) : "—";
		return {
			id: entry.id, timestamp: entry.timestamp, kind: "operation", operationKind: entry.operationKind,
			operationID: entry.operationId, tone: (entry.severity ?? (entry.status === "completed_with_issues" ? "warning" : "error")) === "warning" ? "warn" : "danger", tag,
			title: `${entry.title}${elapsed !== "—" ? ` · ${elapsed}` : ""}`,
			outputAvailable: entry.outputAvailable, issue: true, isNew: entry.isNew,
		};
	}
	const level = entry.level ?? "WARN";
	return {
		id: entry.id, timestamp: entry.timestamp, kind: "log", tone: (entry.severity ?? (level === "ERROR" ? "error" : "warning")) === "error" ? "danger" : "warn",
		tag: logTag({ level, message: entry.title }), title: entry.title, outputAvailable: false, issue: true,
		isNew: entry.isNew,
	};
}
