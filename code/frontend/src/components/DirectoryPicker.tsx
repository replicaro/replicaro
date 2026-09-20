import { useCallback, useEffect, useRef, useState } from "react";

import { browseDirectories } from "../services/api";
import type { DirectoryListing } from "../types";
import { Icon, Loading, Modal } from "./ui";

export function DirectoryField({
    value,
    onChange,
    placeholder,
	buttonLabel = "Browse",
	ariaLabel,
	disabled = false,
	readOnly = false,
	autoFocus = false,
}: {
    value: string;
    onChange: (value: string) => void;
    placeholder?: string;
	buttonLabel?: string;
	ariaLabel?: string;
	disabled?: boolean;
	readOnly?: boolean;
	autoFocus?: boolean;
}) {
    const [open, setOpen] = useState(false);

    return (
        <>
            <div className="directory-input">
                <input
                    type="text"
                    className="mono"
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
                    onCancel={() => setOpen(false)}
                    onSelect={(path) => {
                        onChange(path);
                        setOpen(false);
                    }}
                />
            )}
        </>
    );
}

function DirectoryPicker({
    initialPath,
    onSelect,
    onCancel,
}: {
    initialPath: string;
    onSelect: (path: string) => void;
    onCancel: () => void;
}) {
    const [listing, setListing] = useState<DirectoryListing | null>(null);
    const [path, setPath] = useState(initialPath);
    const [error, setError] = useState("");
	const requestRef = useRef<{ controller: AbortController; generation: number } | null>(null);
	const generationRef = useRef(0);

	const cancelLoad = useCallback(() => {
		generationRef.current += 1;
		requestRef.current?.controller.abort();
		requestRef.current = null;
	}, []);

    const load = useCallback(async (requested: string) => {
		cancelLoad();
		const controller = new AbortController();
		const generation = ++generationRef.current;
		requestRef.current = { controller, generation };
		await Promise.resolve();
		if (generationRef.current !== generation || controller.signal.aborted) return;
        setListing(null);
        setError("");
        try {
			const next = await browseDirectories(requested, controller.signal);
			if (generationRef.current !== generation || controller.signal.aborted) return;
            setListing(next);
            setPath(next.current);
        } catch (reason) {
			if (generationRef.current !== generation || controller.signal.aborted || (reason as { name?: string })?.name === "AbortError") return;
            setError((reason as Error).message);
		} finally {
			if (requestRef.current?.generation === generation) requestRef.current = null;
        }
	}, [cancelLoad]);

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

    return (
        <Modal title="Choose a folder" onClose={close} wide>
            <div className="directory-input" style={{ marginBottom: 12 }}>
                <input
                    className="mono"
                    type="text"
                    value={path}
                    onChange={(event) => setPath(event.target.value)}
                    onKeyDown={(event) => {
                        if (event.key === "Enter") void load(path);
                    }}
                    autoFocus
                />
                <button type="button" className="btn" onClick={() => void load(path)}>
                    Go
                </button>
            </div>

            {error && <div className="inline-error">{error}</div>}
            {!error && listing === null && <Loading />}

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
                                {root.name}
                            </button>
                        ))}
                    </div>
                    <div className="directory-list">
                        {listing.parent && (
                            <button type="button" className="directory-up" onClick={() => void load(listing.parent)}>
                                <Icon name="arrowUp" size={16} /> <span>Up one level</span>
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
                        {listing.directories.length === 0 && !listing.parent && (
                            <p className="muted">No subfolders.</p>
                        )}
                    </div>
                    <div className="modal-footer">
                        <button type="button" className="btn" onClick={close}>
                            Cancel
                        </button>
                        <button
                            type="button"
                            className="btn primary"
							onClick={() => {
								cancelLoad();
								onSelect(listing.current);
							}}
                        >
                            Select this folder
                        </button>
                    </div>
                </>
            )}
        </Modal>
    );
}
