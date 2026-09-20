import type { EngineDescriptor, Repository } from "./types";

export function restoreCapability(repository: Repository | undefined, engines: EngineDescriptor[]) {
	return engines.find((engine) => engine.id === repository?.engine)?.capabilities.restore;
}

export function defaultConflictMode(repository: Repository | undefined, engines: EngineDescriptor[]) {
	const modes = restoreCapability(repository, engines)?.conflictModes ?? [];
	return modes.find((mode) => mode.default)?.id ?? modes[0]?.id ?? "";
}

export function restoreConflictModeLabel(mode: { id: string; label: string }) {
	if (mode.id === "always" || mode.id === "overwrite") return "Yes, overwrite";
	if (mode.id === "never" || mode.id === "no-overwrite") return "Do not overwrite";
	return mode.label;
}
