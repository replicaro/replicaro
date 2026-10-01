// The browser's leave-page prompt for a request whose work stops when the page
// goes away. Only foreground actions need it: vault creation, connection and
// ownership transfer, and the shorter saves listed in services/api.ts, still
// run inside their HTTP request, so closing or reloading the tab mid-request
// can leave them half done. api.ts holds it around every such request.
// Background jobs (restore, deletions, and the vault password change, settings
// save and removal) run on the server's own lifetime and must not hold this
// prompt.
//
// The prompt only reduces accidental page loss. It cannot keep work alive after
// the user confirms leaving, and in-app React navigation does not fire
// beforeunload at all.

const heldRequests = new Set<string>();

const beforeUnload = (event: BeforeUnloadEvent) => {
	event.preventDefault();
	event.returnValue = "";
};

// holdLeavePagePrompt installs the prompt for one in-flight request and
// returns its release. The key only has to be unique per request, so one
// request's release never drops the prompt another request still holds; the
// listener goes away when the last request releases. Releasing twice is
// harmless.
export function holdLeavePagePrompt(requestKey: string) {
	if (heldRequests.size === 0) window.addEventListener("beforeunload", beforeUnload);
	heldRequests.add(requestKey);
	let released = false;
	return () => {
		if (released) return;
		released = true;
		heldRequests.delete(requestKey);
		if (heldRequests.size === 0) window.removeEventListener("beforeunload", beforeUnload);
	};
}

export function resetLeavePagePromptForTests() {
	heldRequests.clear();
	window.removeEventListener("beforeunload", beforeUnload);
}
