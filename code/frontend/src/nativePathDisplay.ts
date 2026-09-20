// Display only: callers keep the native path for keys, selection, and requests.
// The native source header supplies the association; /C by itself is not proof
// of a Windows drive, and the viewing computer supplies no path grammar.
export function resticArchiveDisplayPath(nativePath: string, source: string): string {
	let archiveRoot: string;
	let displayRoot: string;
	if (/^[A-Za-z]:[\\/]/.test(source)) {
		displayRoot = source.replace(/\//g, "\\").replace(/\\$/, "");
		archiveRoot = `/${source[0]}/${source.slice(3).replace(/\\/g, "/")}`.replace(/\/$/, "");
	} else {
		return nativePath;
	}
	if (nativePath === archiveRoot) return displayRoot;
	if (nativePath.startsWith(`${archiveRoot}/`)) {
		const relative = nativePath.slice(archiveRoot.length + 1);
		// A literal backslash in an archive component must remain recognizable.
		if (relative.includes("\\")) return nativePath;
		return `${displayRoot}\\${relative.replace(/\//g, "\\")}`;
	}
	if (nativePath !== "/" && archiveRoot.startsWith(`${nativePath}/`)) {
		return `${source[0]}:${nativePath.slice(2).replace(/\//g, "\\")}`;
	}
	return nativePath;
}

// This is an address detail derived from the same native source association,
// never an action identifier or a destination on the viewing filesystem.
export function resticNativeAddress(source: string, relative: string): string {
	let root = source;
	if (/^[A-Za-z]:[\\/]/.test(source)) {
		root = `/${source[0]}/${source.slice(3).replace(/\\/g, "/")}`;
	} else if (source.startsWith("\\\\")) {
		const parts = source.slice(2).split(/[\\/]/);
		if (parts.length >= 2 && parts.every((part) => part !== "" && part !== "." && part !== "..")) {
			root = `/\\\\${parts[0]}\\${parts[1]}${parts.length > 2 ? `/${parts.slice(2).join("/")}` : ""}`;
		}
	}
	return relative ? `${root.replace(/\/$/, "")}/${relative}` : root;
}
