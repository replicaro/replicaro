import { useEffect, useId, useRef, useState } from "react";
import type { ReactNode } from "react";
import { createPortal } from "react-dom";

// A tooltip trigger whose popup is rendered into document.body with a fixed
// position taken from the trigger's box, instead of inside the trigger like
// the shared Tooltip in ui.tsx.
//
// Use it for labels inside a box that clips its content, such as the File
// History results list and browse tree (overflow: hidden) or the scrolling
// restore-selection panel. A popup positioned inside such a box is cut off at
// the box's edge even when the window has room for it. The lists keep their
// overflow: hidden (it keeps the rounded corners and long rows tidy), so the
// popup has to leave the box instead.
//
// Inside a Modal, pass inModal: the popup's usual z-index sits below the
// modal overlay, so it would open behind the dialog. The usual z-index stays
// below the overlay so that a popup of the page underneath never shows on top
// of a dialog.
export function FloatingTooltip({
	content,
	label,
	className,
	title,
	role,
	inModal = false,
	children,
}: {
	content: string;
	// The trigger's accessible name.
	label: string;
	className: string;
	title?: string;
	// A trigger that wraps a block of controls (rather than a label) is a
	// "group", so screen readers announce its name and description with them.
	role?: string;
	inModal?: boolean;
	children: ReactNode;
}) {
	const id = useId();
	const triggerRef = useRef<HTMLSpanElement>(null);
	const tooltipRef = useRef<HTMLSpanElement>(null);
	// Hover and keyboard focus are tracked separately: the popup stays open
	// while either one holds, so the pointer leaving a keyboard-focused
	// trigger, or a hovered trigger losing focus, doesn't close it.
	const [hovered, setHovered] = useState(false);
	const [focused, setFocused] = useState(false);
	// Focus that comes from a mouse press doesn't count, so clicking a label
	// and moving away closes the popup instead of leaving it pinned over the
	// rows below until something else is clicked. A touch tap still opens it:
	// the browser fires pointerup before the compatibility mousedown that
	// focuses the trigger. :focus-visible would say the same thing, but jsdom
	// never matches it, so the press is tracked by hand.
	const pressed = useRef(false);
	// A mouse press released outside the trigger never sends pointerup here,
	// so pointerleave and blur clear it as well.
	const release = () => { pressed.current = false; };
	const open = hovered || focused;
	const [position, setPosition] = useState<{ left: number; top: number }>();

	useEffect(() => {
		if (!open) return;
		// Start at the trigger's left edge, kept 16px inside the window, and
		// below the trigger unless there is no room there.
		const positionTooltip = () => {
			const trigger = triggerRef.current;
			const tooltip = tooltipRef.current;
			if (!trigger || !tooltip) return;
			const triggerBox = trigger.getBoundingClientRect();
			const tooltipBox = tooltip.getBoundingClientRect();
			// clientWidth and clientHeight leave out classic scrollbars, which
			// innerWidth and innerHeight include.
			const viewport = document.documentElement;
			const left = Math.max(16, Math.min(triggerBox.left, viewport.clientWidth - tooltipBox.width - 16));
			const below = triggerBox.bottom + 8;
			const top = below + tooltipBox.height <= viewport.clientHeight - 16
				? below
				: Math.max(16, triggerBox.top - tooltipBox.height - 8);
			setPosition({ left, top });
		};
		positionTooltip();
		// The popup is fixed, so it has to follow the trigger when any
		// container scrolls or the window is resized.
		window.addEventListener("resize", positionTooltip);
		document.addEventListener("scroll", positionTooltip, true);
		return () => {
			window.removeEventListener("resize", positionTooltip);
			document.removeEventListener("scroll", positionTooltip, true);
		};
	}, [open, content]);

	// The position is cleared when the popup closes, so the next time it
	// opens it stays hidden until it has been measured and placed, rather
	// than flashing at its old spot.
	const leave = () => { setHovered(false); if (!focused) setPosition(undefined); };
	const blur = () => { setFocused(false); if (!hovered) setPosition(undefined); };

	return (
		<span
			ref={triggerRef}
			className={`${className} tooltip-trigger`}
			role={role}
			tabIndex={0}
			aria-label={label}
			aria-describedby={id}
			title={title}
			onMouseEnter={() => setHovered(true)}
			onMouseLeave={leave}
			onPointerDown={() => { pressed.current = true; }}
			onPointerUp={release}
			onPointerCancel={release}
			onPointerLeave={release}
			onFocus={() => { if (!pressed.current) setFocused(true); }}
			onBlur={() => { release(); blur(); }}
		>
			{children}
			{createPortal(
				<span
					ref={tooltipRef}
					id={id}
					className={`tooltip-popup floating-tooltip${inModal ? " in-modal" : ""}${open ? " open" : ""}`}
					role="tooltip"
					style={open && position
						? { left: position.left, top: position.top, visibility: "visible" }
						: { visibility: "hidden" }}
				>{content}</span>,
				document.body,
			)}
		</span>
	);
}
