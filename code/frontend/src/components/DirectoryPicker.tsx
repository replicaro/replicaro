import { t } from "../i18n";
import { useCallback, useEffect, useRef, useState } from "react";

import { APIError, browseDirectories } from "../services/api";
import type { DirectoryListing } from "../types";
import { Icon, Loading, Modal } from "./ui";

// What the picker lets the user choose.
//
// - "folder": folders only. Every picker uses this except the job source.
//   It requests the plain folders-only listing, with no files parameter.
// - "fileOrFolder": the job source of a new job. Files are listed too and can
//   be chosen, and so can the open folder.
// - "file": "Update job source" for a job whose source is a single file. Files
//   are listed and one must be chosen; folders are only for moving around,
//   since a file job can't be pointed at a folder.
//
// Keep "folder" the default and keep the other modes opt-in: the vault and
// restore destination pickers must never list or return files.
export type DirectoryPickMode = "folder" | "fileOrFolder" | "file";

export function DirectoryField({
    value,
    onChange,
	onPicked,
    placeholder,
	buttonLabel = t("ui.pages.protect.browse"),
	ariaLabel,
	disabled = false,
	readOnly = false,
	autoFocus = false,
	pick = "folder",
}: {
    value: string;
    onChange: (value: string) => void;
	// Called after onChange when the value comes from the picker, with what
	// was chosen. A typed value never reports a kind; see the new-job form for
	// how it learns one.
	onPicked?: (path: string, kind: "file" | "folder") => void;
    placeholder?: string;
	buttonLabel?: string;
	ariaLabel?: string;
	disabled?: boolean;
	readOnly?: boolean;
	autoFocus?: boolean;
	pick?: DirectoryPickMode;
}) {
    const [open, setOpen] = useState(false);

    return (
        <>
            <div className="directory-input">
                <input
                    type="text"
                    className="mono"
                    dir="ltr"
                    value={value}
                    placeholder={placeholder}
                    onChange={(event) => onChange(event.target.value)}
					aria-label={ariaLabel}
					disabled={disabled}
					readOnly={readOnly}
					autoFocus={autoFocus}
                />
                <button type="button" className="btn" disabled={disabled || readOnly} onClick={() => setOpen(true)}>
                    <Icon name="folder" size={14} /> {buttonLabel}
                </button>
            </div>
            {open && (
                <DirectoryPicker
                    initialPath={value}
					pick={pick}
                    onCancel={() => setOpen(false)}
                    onSelect={(path, kind) => {
                        onChange(path);
						onPicked?.(path, kind);
                        setOpen(false);
                    }}
                />
            )}
        </>
    );
}

function DirectoryPicker({
    initialPath,
	pick,
    onSelect,
    onCancel,
}: {
    initialPath: string;
	pick: DirectoryPickMode;
    onSelect: (path: string, kind: "file" | "folder") => void;
    onCancel: () => void;
}) {
	const listsFiles = pick !== "folder";
    const [listing, setListing] = useState<DirectoryListing | null>(null);
    const [path, setPath] = useState(initialPath);
	// The file chosen in the open folder, by its path; empty when none is.
	// Only the file modes ever set it.
	const [chosenFile, setChosenFile] = useState("");
    const [error, setError] = useState("");
	// The backend ends a listing after 60 seconds when the folder (typically a
	// dead network share or a disconnected mapped drive) does not answer; the
	// OS call itself keeps running on the server. That case gets its own
	// localized message instead of the technical error text.
	const [notResponding, setNotResponding] = useState(false);
	const requestRef = useRef<{ controller: AbortController; generation: number } | null>(null);
	const generationRef = useRef(0);
	// The last folder that loaded. After a failed load it stays on screen under
	// the error, so the roots, Up one level, its subfolders and the footer are
	// still there to move on from.
	const loadedRef = useRef<DirectoryListing | null>(null);

	const cancelLoad = useCallback(() => {
		generationRef.current += 1;
		requestRef.current?.controller.abort();
		requestRef.current = null;
	}, []);

	const load = useCallback(async (requested: string) => {
		// keepError is set only for the fallback below, which must not clear the
		// error that caused it.
		const run = async (target: string, keepError: boolean): Promise<void> => {
			cancelLoad();
			const controller = new AbortController();
			const generation = ++generationRef.current;
			requestRef.current = { controller, generation };
			await Promise.resolve();
			if (generationRef.current !== generation || controller.signal.aborted) return;
			setListing(null);
			if (!keepError) {
				setError("");
				setNotResponding(false);
			}
			let fallback = false;
			try {
				// The folder picker sends exactly the folders-only request; only
				// the file modes add the files parameter.
				const next = listsFiles
					? await browseDirectories(target, controller.signal, true)
					: await browseDirectories(target, controller.signal);
				if (generationRef.current !== generation || controller.signal.aborted) return;
				loadedRef.current = next;
				setListing(next);
				// A path that is a file comes back as the folder that holds it,
				// with the file in its list. It is chosen right away, so the
				// picker opens on a file path typed in the field, and Go on a
				// file path typed in the picker lands on that file.
				const file = listsFiles ? next.files?.find((entry) => entry.path === target) : undefined;
				setChosenFile(file?.path ?? "");
				setPath(file?.path ?? next.current);
			} catch (reason) {
				if (generationRef.current !== generation || controller.signal.aborted || (reason as { name?: string })?.name === "AbortError") return;
				// The fallback's own failure keeps the first error, which names
				// the folder the user asked for.
				if (!keepError) {
					if (reason instanceof APIError && reason.code === "directory_not_responding") setNotResponding(true);
					else setError((reason as Error).message);
				}
				// When the very first folder fails there is nothing to keep, so
				// the default start folder (home, or / in Docker) is loaded once,
				// with the error still shown. Its own failure ends there. A kept
				// folder also goes back into the path box, since that is the
				// folder "Select this folder" returns.
				if (loadedRef.current) {
					setListing(loadedRef.current);
					setPath(loadedRef.current.current);
					setChosenFile("");
				} else fallback = target !== "";
			} finally {
				if (requestRef.current?.generation === generation) requestRef.current = null;
			}
			if (fallback) await run("", true);
		};
		await run(requested, false);
	}, [cancelLoad, listsFiles]);

    useEffect(() => {
		const timer = window.setTimeout(() => void load(initialPath), 0);
        return () => {
			window.clearTimeout(timer);
			cancelLoad();
        };
    }, [cancelLoad, initialPath, load]);

	const close = () => {
		cancelLoad();
		onCancel();
	};

	// In "fileOrFolder" the button returns the chosen file, or the open folder
	// when no file is chosen. In "file" it only ever returns a file.
	const returnsFile = listsFiles && chosenFile !== "";

    return (
        <Modal title={listsFiles ? t("ui.components.directorypicker.chooseFileOrFolder") : t("ui.components.directorypicker.choose.a.folder")} onClose={close} wide>
            <div className="directory-input" style={{ marginBottom: 12 }}>
                <input
                    className="mono"
                    type="text"
                    dir="ltr"
                    value={path}
					onChange={(event) => {
						// Typing a new path drops the chosen file, like moving
						// to another folder does. Go opens what was typed.
						setPath(event.target.value);
						setChosenFile("");
					}}
                    onKeyDown={(event) => {
                        if (event.key === "Enter") void load(path);
                    }}
                    autoFocus
                />
                <button type="button" className="btn" onClick={() => void load(path)}>
                    {t("ui.components.directorypicker.go")}</button>
            </div>

            {error && <div className="inline-error">{error}</div>}
            {notResponding && <div className="inline-error" role="alert">
                <strong>{t("ui.components.directorypicker.notResponding.title")}</strong>
                <div>{t("ui.components.directorypicker.notResponding.body")}</div>
            </div>}
            {!error && !notResponding && listing === null && <Loading />}

            {listing && (
                <>
                    <div className="directory-roots">
                        {listing.roots.map((root) => (
                            <button
                                type="button"
                                className="btn sm"
                                key={root.path}
                                onClick={() => void load(root.path)}
                            >
                                <bdi dir="ltr">{root.name}</bdi>
                            </button>
                        ))}
                    </div>
                    <div className="directory-list">
                        {listing.parent && (
                            <button type="button" className="directory-up" onClick={() => void load(listing.parent)}>
                                <Icon name="arrowUp" size={16} /> <span>{t("ui.components.directorypicker.up.one.level")}</span>
                            </button>
                        )}
                        {listing.directories.map((directory) => (
                            <button
                                type="button"
                                key={directory.path}
                                onClick={() => void load(directory.path)}
                            >
                                <Icon name="folder" size={15} /> <span>{directory.name}</span>
                            </button>
                        ))}
						{listsFiles && listing.files?.map((file) => (
							<button
								type="button"
								key={file.path}
								aria-pressed={chosenFile === file.path}
								onClick={() => {
									// A toggle: pressing the chosen file again clears
									// the choice and puts the open folder back in the
									// path box, which in "fileOrFolder" makes the
									// folder the selection again.
									const next = chosenFile === file.path ? "" : file.path;
									setChosenFile(next);
									setPath(next || listing.current);
								}}
							>
								<Icon name="file" size={15} /> <span>{file.name}</span>
							</button>
						))}
						{/* The folder picker shows its empty text only at a folder
						    without a parent (a drive or filesystem root). The file
						    modes have their own text and show it whenever the
						    folder has nothing to list. Keep the two conditions
						    separate. */}
						{listsFiles
							? listing.directories.length === 0 && !listing.files?.length && (
								<p className="muted">{t("ui.components.directorypicker.noFilesOrSubfolders")}</p>
							)
							: listing.directories.length === 0 && !listing.parent && (
								<p className="muted">{t("ui.components.directorypicker.no.subfolders")}</p>
							)}
                    </div>
                    <div className="modal-footer">
                        <button type="button" className="btn" onClick={close}>
                            {t("ui.components.directorypicker.cancel")}</button>
                        <button
                            type="button"
                            className="btn primary"
							disabled={pick === "file" && !returnsFile}
							onClick={() => {
								cancelLoad();
								if (returnsFile) onSelect(chosenFile, "file");
								else onSelect(listing.current, "folder");
							}}
                        >
							{returnsFile || pick === "file"
								? t("ui.components.directorypicker.selectThisFile")
								: t("ui.components.directorypicker.select.this.folder")}</button>
                    </div>
                </>
            )}
        </Modal>
    );
}
