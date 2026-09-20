import {
    cloneElement,
    createContext,
    useCallback,
    useContext,
    useEffect,
    useId,
    useRef,
    useState,
    type ReactElement,
    type ReactNode,
    type MouseEvent,
    type FocusEvent,
} from "react";
import { createPortal } from "react-dom";

/* ------------------------------------------------------------------ */
/* Icons (inline, stroke-based)                                        */
/* ------------------------------------------------------------------ */

const paths: Record<string, ReactNode> = {
    dashboard: (
        <>
            <rect x="3" y="3" width="7" height="9" rx="1.5" />
            <rect x="14" y="3" width="7" height="5" rx="1.5" />
            <rect x="14" y="12" width="7" height="9" rx="1.5" />
            <rect x="3" y="16" width="7" height="5" rx="1.5" />
        </>
    ),
    vault: (
        <>
            <rect x="3" y="4" width="18" height="16" rx="2" />
            <circle cx="12" cy="12" r="3.5" />
            <path d="M12 8.5v-2M12 17.5v-2M8.5 12h-2M17.5 12h-2" />
        </>
    ),
    camera: (
        <>
            <circle cx="12" cy="12" r="8.5" />
            <path d="M12 7.5V12l3 2" />
        </>
    ),
    calendar: (
        <>
            <rect x="3" y="5" width="18" height="16" rx="2" />
            <path d="M8 3v4M16 3v4M3 10h18" />
        </>
    ),
    jobs: (
        <>
            <path d="M8 6h13M8 12h13M8 18h13" />
            <path d="M3.5 6l1 1 2-2M3.5 12l1 1 2-2M3.5 18l1 1 2-2" />
        </>
    ),
    history: (
        <>
            <path d="M3.5 12a8.5 8.5 0 1 0 2.5-6L3.5 8.5" />
            <path d="M3.5 4v4.5H8" />
            <path d="M12 8v4.5l3 1.5" />
        </>
    ),
    logs: (
        <>
            <path d="M6 3h9l4 4v14H6z" />
            <path d="M15 3v4h4" />
            <path d="M9 12h6M9 16h6" />
        </>
    ),
    settings: (
        <>
            <circle cx="12" cy="12" r="3" />
            <path d="M19.4 15a1.6 1.6 0 0 0 .32 1.77l.06.06a2 2 0 1 1-2.83 2.83l-.06-.06a1.6 1.6 0 0 0-1.77-.32 1.6 1.6 0 0 0-1 1.47V21a2 2 0 1 1-4 0v-.09a1.6 1.6 0 0 0-1-1.47 1.6 1.6 0 0 0-1.77.32l-.06.06a2 2 0 1 1-2.83-2.83l.06-.06a1.6 1.6 0 0 0 .32-1.77 1.6 1.6 0 0 0-1.47-1H3a2 2 0 1 1 0-4h.09a1.6 1.6 0 0 0 1.47-1 1.6 1.6 0 0 0-.32-1.77l-.06-.06a2 2 0 1 1 2.83-2.83l.06.06a1.6 1.6 0 0 0 1.77.32h.09a1.6 1.6 0 0 0 1-1.47V3a2 2 0 1 1 4 0v.09a1.6 1.6 0 0 0 1 1.47 1.6 1.6 0 0 0 1.77-.32l.06-.06a2 2 0 1 1 2.83 2.83l-.06.06a1.6 1.6 0 0 0-.32 1.77V9a1.6 1.6 0 0 0 1.47 1H21a2 2 0 1 1 0 4h-.09a1.6 1.6 0 0 0-1.47 1z" />
        </>
    ),
    engine: (
        <>
            <rect x="4" y="7" width="16" height="12" rx="2" />
            <path d="M8 7V5a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" />
            <path d="M9 13h.01M12 13h.01M15 13h.01" />
        </>
    ),
    play: <path d="M7 5l12 7-12 7z" />,
    copy: (
        <>
            <rect x="8" y="8" width="11" height="11" rx="2" />
            <path d="M16 8V6a2 2 0 0 0-2-2H6a2 2 0 0 0-2 2v8a2 2 0 0 0 2 2h2" />
        </>
    ),
    arrowUp: <path d="M12 19V5M6.5 10.5L12 5l5.5 5.5" />,
    plus: <path d="M12 5v14M5 12h14" />,
    trash: (
        <>
            <path d="M4 7h16M9 7V4h6v3M6.5 7l1 13h9l1-13" />
            <path d="M10 11v5M14 11v5" />
        </>
    ),
    folder: (
        <path d="M3 6a2 2 0 0 1 2-2h4l2 2.5h8a2 2 0 0 1 2 2V18a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z" />
    ),
    file: (
        <>
            <path d="M6 3h9l4 4v14H6z" />
            <path d="M15 3v4h4" />
        </>
    ),
    restore: (
        <>
            <path d="M3.5 12a8.5 8.5 0 1 0 2.5-6L3.5 8.5" />
            <path d="M3.5 4v4.5H8" />
        </>
    ),
    shield: (
        <>
            <path d="M12 3l7.5 3v6c0 4.5-3 8-7.5 9.5C7.5 20 4.5 16.5 4.5 12V6z" />
            <path d="M9 12l2 2 4-4.5" />
        </>
    ),
    broom: (
        <>
            <path d="M14 4l6 6" />
            <path d="M16.5 6.5L9 14l-5 6 6-5 7.5-7.5" />
        </>
    ),
    edit: (
        <path d="M4 20h4l11-11a2.1 2.1 0 0 0-3-3L5 17zM13.5 7.5l3 3" />
    ),
    chevron: <path d="M9 6l6 6-6 6" />,
    x: <path d="M6 6l12 12M18 6L6 18" />,
	checkCircle: (
		<>
			<circle cx="12" cy="12" r="8.5" />
			<path d="M8.5 12l2.3 2.3 4.8-5" />
		</>
	),
	xCircle: (
		<>
			<circle cx="12" cy="12" r="8.5" />
			<path d="M9 9l6 6M15 9l-6 6" />
		</>
	),
    info: (
        <>
            <circle cx="12" cy="12" r="8.5" />
            <path d="M12 11v5M12 8h.01" />
        </>
    ),
    lock: (
        <>
            <rect x="5" y="11" width="14" height="9" rx="2" />
            <path d="M8 11V8a4 4 0 0 1 8 0v3" />
        </>
    ),
};

export function Icon({
    name,
    size = 17,
}: {
    name: string;
    size?: number;
}) {
    return (
        <svg
            width={size}
            height={size}
            viewBox="0 0 24 24"
            fill="none"
            stroke="currentColor"
            strokeWidth="1.7"
            strokeLinecap="round"
            strokeLinejoin="round"
            aria-hidden="true"
        >
            {paths[name] ?? paths.info}
        </svg>
    );
}

export function Tooltip({
    content,
    children,
}: {
    content: string;
    children: ReactElement;
}) {
    const id = useId();
    const props = children.props as {
        "aria-describedby"?: string;
        "aria-label"?: string;
        children?: ReactNode;
        className?: string | ((value: unknown) => string);
        tabIndex?: number;
        onMouseEnter?: (event: MouseEvent<HTMLElement>) => void;
        onFocus?: (event: FocusEvent<HTMLElement>) => void;
    };
    const describedBy = [props["aria-describedby"], id].filter(Boolean).join(" ");
    const originalClassName = props.className;
    const className = typeof originalClassName === "function"
        ? (value: unknown) => `${originalClassName(value)} tooltip-trigger`.trim()
        : `${originalClassName ?? ""} tooltip-trigger`.trim();

    const fitTooltip = (trigger: HTMLElement) => {
        const popup = trigger.querySelector<HTMLElement>(":scope > .tooltip-popup");
        if (!popup) return;
        popup.style.left = "0px";
        popup.style.right = "auto";
        const bounds = popup.getBoundingClientRect();
        const rightLimit = document.documentElement.clientWidth - 16;
        const shift = Math.max(16 - bounds.left, Math.min(0, rightLimit - bounds.right));
        popup.style.left = `${shift}px`;
    };

    return cloneElement(children, {
        onMouseEnter: (event: MouseEvent<HTMLElement>) => {
            props.onMouseEnter?.(event);
            fitTooltip(event.currentTarget);
        },
        onFocus: (event: FocusEvent<HTMLElement>) => {
            props.onFocus?.(event);
            fitTooltip(event.currentTarget);
        },
        "aria-describedby": describedBy,
        "aria-label": props["aria-label"] ?? content,
        children: (
            <>
                {props.children}
                <span id={id} className="tooltip-popup" role="tooltip">{content}</span>
            </>
        ),
        className,
        tabIndex: props.tabIndex ?? 0,
    } as never);
}

/* ------------------------------------------------------------------ */
/* Badges & status                                                     */
/* ------------------------------------------------------------------ */

export function StatusBadge({ status }: { status: string }) {
    switch (status) {
        case "running":
            return <span className="badge info running">Running</span>;
        case "success":
            return <span className="badge ok">Healthy</span>;
        case "failed":
            return <span className="badge danger">Failed</span>;
        default:
            return <span className="badge muted">Never run</span>;
    }
}

/* ------------------------------------------------------------------ */
/* Modal                                                               */
/* ------------------------------------------------------------------ */

export function Modal({
    title,
    onClose,
    children,
    wide,
	topAction,
}: {
    title: string;
    onClose: () => void;
    children: ReactNode;
    wide?: boolean;
	topAction?: ReactNode;
}) {
	const dialogRef = useRef<HTMLDivElement>(null);
	const onCloseRef = useRef(onClose);
	useEffect(() => {
		onCloseRef.current = onClose;
	}, [onClose]);

    useEffect(() => {
		const previousFocus = document.activeElement instanceof HTMLElement ? document.activeElement : null;
		const dialog = dialogRef.current;
		const focusable = () => Array.from(dialog?.querySelectorAll<HTMLElement>(
			'button:not([disabled]), input:not([disabled]), select:not([disabled]), textarea:not([disabled]), [tabindex]:not([tabindex="-1"])'
		) ?? []).filter((element) => !element.hidden);
		const preferred = dialog?.querySelector<HTMLElement>("[autofocus]") ?? focusable()[0] ?? dialog;
		preferred?.focus();
        const onKey = (e: KeyboardEvent) => {
			if (e.key === "Escape") onCloseRef.current();
			if (e.key !== "Tab") return;
			const items = focusable();
			if (items.length === 0) {
				e.preventDefault();
				dialog?.focus();
				return;
			}
			const first = items[0];
			const last = items[items.length - 1];
			if (e.shiftKey && document.activeElement === first) {
				e.preventDefault();
				last.focus();
			} else if (!e.shiftKey && document.activeElement === last) {
				e.preventDefault();
				first.focus();
			}
        };
        window.addEventListener("keydown", onKey);
		return () => {
			window.removeEventListener("keydown", onKey);
			previousFocus?.focus();
		};
	}, []);

    return createPortal(
        <div
            className="modal-overlay"
            onMouseDown={(e) => {
                if (e.target === e.currentTarget) onClose();
            }}
        >
            <div
				ref={dialogRef}
                className={"modal" + (wide ? " wide" : "")}
                role="dialog"
				aria-modal="true"
				aria-label={title}
				tabIndex={-1}
            >
				{/* Optional actions precede the title and body so callers can keep a
				    critical control outside content that grows or scrolls. */}
				{topAction && <div className="modal-top-action">{topAction}</div>}
                <div className="row spread" style={{ marginBottom: 4 }}>
                    <h3 style={{ margin: 0 }}>{title}</h3>
                    <button className="modal-close" onClick={onClose} aria-label="Close">
                        Close <Icon name="x" size={13} />
                    </button>
                </div>
                <div style={{ marginTop: 12 }}>{children}</div>
            </div>
        </div>,
        document.body
    );
}

export function ConfirmDialog({
    title,
    message,
    confirmLabel,
    onConfirm,
    onCancel,
    busy,
}: {
    title: string;
    message: ReactNode;
    confirmLabel: string;
    onConfirm: () => void;
    onCancel: () => void;
    busy?: boolean;
}) {
    const cancel = () => {
        if (!busy) onCancel();
    };
    return (
        <Modal title={title} onClose={cancel}>
            <p className="muted" style={{ marginTop: 0 }}>
                {message}
            </p>
            <div className="modal-footer">
                <button className="btn" onClick={cancel} disabled={busy}>
                    Cancel
                </button>
                <button
                    className="btn danger"
                    onClick={onConfirm}
                    disabled={busy}
                >
                    {busy && <span className="spinner" />}
                    {confirmLabel}
                </button>
            </div>
        </Modal>
    );
}

export function SaveChangesDialog({
    onSave,
    onDiscard,
    onCancel,
    busy,
}: {
    onSave: () => void;
    onDiscard: () => void;
    onCancel: () => void;
    busy?: boolean;
}) {
    return (
        <Modal title="Save changes?" onClose={onCancel}>
            <p className="muted" style={{ marginTop: 0 }}>
                You have unsaved changes. Would you like to save them before closing?
            </p>
            <div className="modal-footer">
                <button className="btn" onClick={onCancel} disabled={busy}>
                    Cancel
                </button>
                <button className="btn" onClick={onDiscard} disabled={busy}>
                    Discard changes
                </button>
                <button className="btn primary" onClick={onSave} disabled={busy}>
                    {busy && <span className="spinner" />}
                    Save changes
                </button>
            </div>
        </Modal>
    );
}

/* ------------------------------------------------------------------ */
/* Toasts                                                              */
/* ------------------------------------------------------------------ */

interface Toast {
    id: number;
    kind: "ok" | "error" | "info";
    message: string;
}

const ToastContext = createContext<(kind: Toast["kind"], message: string) => void>(
    () => undefined
);

export function useToast() {
    return useContext(ToastContext);
}

let toastId = 0;

export function ToastProvider({ children }: { children: ReactNode }) {
    const [toasts, setToasts] = useState<Toast[]>([]);

    const push = useCallback((kind: Toast["kind"], message: string) => {
        const id = ++toastId;
        setToasts((current) => [...current, { id, kind, message }]);
        window.setTimeout(() => {
            setToasts((current) => current.filter((t) => t.id !== id));
        }, kind === "error" ? 30_000 : 5_200);
    }, []);

    // Brief toast overlap with controls is accepted behavior to avoid reserving
    // space on small screens.
    // The dismiss button lets users clear them immediately; automatic dismissal
    // timers still apply.
    return (
        <ToastContext.Provider value={push}>
            {children}
            <div className="toasts">
                {toasts.map((toast) => (
					<div key={toast.id} className={`toast ${toast.kind}`} role={toast.kind === "error" ? "alert" : "status"}>
						<span className="toast-icon"><Icon name={toast.kind === "error" ? "xCircle" : "checkCircle"} size={18} /></span>
						<span className="toast-message">{toast.message}</span>
						<button type="button" className="toast-dismiss" aria-label="Dismiss notification" onClick={() => setToasts((current) => current.filter((item) => item.id !== toast.id))}><Icon name="x" size={16} /></button>
                    </div>
                ))}
            </div>
        </ToastContext.Provider>
    );
}

/* ------------------------------------------------------------------ */
/* Empty state, spinner                                                */
/* ------------------------------------------------------------------ */

export function EmptyState({
    icon,
    title,
    children,
}: {
    icon: string;
    title: string;
    children?: ReactNode;
}) {
    return (
        <div className="empty">
            <div className="icon">
                <Icon name={icon} size={34} />
            </div>
            <h4>{title}</h4>
            {children}
        </div>
    );
}

export function Loading({ label = "Loading" }: { label?: string }) {
    return (
        <div className="row muted loading" style={{ padding: "18px 4px" }} aria-live="polite">
            <span className="spinner" />
            <span className="loading-label">{label}<span className="loading-dots" aria-hidden="true">...</span></span>
        </div>
    );
}

/* ------------------------------------------------------------------ */
/* Time helpers                                                        */
/* ------------------------------------------------------------------ */

export function parseTime(value: string): Date | null {
    if (!value) return null;

    // SQLite datetime('now') produces "YYYY-MM-DD HH:MM:SS" in UTC.
    const normalized =
        value.includes("T") || value.includes("Z")
            ? value
            : value.replace(" ", "T") + "Z";

    const date = new Date(normalized);

    return isNaN(date.getTime()) ? null : date;
}

export function timeAgo(value: string): string {
    const date = parseTime(value);

    if (!date) return "—";

    const seconds = Math.round((Date.now() - date.getTime()) / 1000);
    const future = seconds < 0;
    const s = Math.abs(seconds);

    let text: string;

    if (s < 60) text = "just now";
    else if (s < 3600) text = `${Math.floor(s / 60)}m`;
    else if (s < 86400) text = `${Math.floor(s / 3600)}h`;
    else text = `${Math.floor(s / 86400)}d`;

    if (text === "just now") return future ? "in under a minute" : text;

    return future ? `in ${text}` : `${text} ago`;
}

export function formatTime(value: string): string {
    const date = parseTime(value);

    if (!date) return "—";

    return date.toLocaleString(undefined, {
        year: "numeric",
        month: "short",
        day: "numeric",
        hour: "2-digit",
        minute: "2-digit",
    });
}

export function duration(start: string, end: string): string {
    const a = parseTime(start);
    const b = parseTime(end);

    if (!a || !b) return "—";

    const seconds = Math.max(0, Math.round((b.getTime() - a.getTime()) / 1000));

    if (seconds < 60) return `${seconds}s`;
    if (seconds < 3600)
        return `${Math.floor(seconds / 60)}m ${seconds % 60}s`;

    return `${Math.floor(seconds / 3600)}h ${Math.floor((seconds % 3600) / 60)}m`;
}

export function scheduleLabel(schedule: string): string {
    switch (schedule) {
        case "manual":
        case "":
            return "Manual";
        case "hourly":
            return "Every hour";
        case "daily":
            return "Daily";
        case "weekly":
            return "Weekly";
        case "monthly":
            return "Monthly";
    }

	if (schedule.startsWith("every-months:")) {
		return `Every ${schedule.slice(13)} months`;
	}

    if (schedule.startsWith("every:")) {
        const minutes = schedule.slice(6);
        return `Every ${minutes} min`;
    }
	if (schedule.startsWith("cron:")) {
		return `Cron: ${schedule.slice(5)}`;
	}

    return schedule;
}
