import { useEffect, useId, useLayoutEffect, useRef, useState } from "react";
import type { KeyboardEvent, ReactNode } from "react";
import { createPortal } from "react-dom";

export type MultilineSelectOption = {
	value: string;
	// The first line is the entry itself; any further lines are details shown
	// indented under it (on the start side, so on the right in a right-to-left
	// language).
	lines: ReactNode[];
};

// A single-choice dropdown whose entries can span several lines, used where a
// browser <select> can't show what an entry needs: an <option> is one line of
// plain text that can't wrap or indent.
//
// What it does like a <select>: the closed control is a button
// (aria-haspopup="listbox", aria-expanded) named by its label followed by the
// chosen entry, and a click on the label focuses it without opening it. A
// click, Enter, Space, Up or Down opens the list, a role="listbox" with
// role="option" entries and aria-selected, which takes focus and tracks the
// highlighted entry with aria-activedescendant. In the list, Up/Down move by
// one, PageUp/PageDown by ten and Home/End to the ends; Enter, Space or a
// click chooses. Escape or a click on the button closes it without choosing,
// with focus back on the button; Tab and Shift+Tab close it without choosing
// and move on to the control after or before the button; a press anywhere
// else closes it without choosing.
//
// What it doesn't do: type-ahead (typing letters to jump to an entry), Up or
// Down changing the choice while the list is closed (they open it instead),
// taking part in a <form> (it has no name or value of its own), and showing
// the operating system's own picker on a touch screen.
//
// The list is rendered into document.body with a fixed position taken from the
// button's box, like FloatingTooltip, because the File History restore panel
// scrolls and clips its content: a list positioned inside it would be cut off
// at the panel's edge. It opens below the button, or above it when there is
// more room there, and scrolls when it is taller than the room it has.
export function MultilineSelect({
	value,
	options,
	onChange,
	placeholder,
	label,
	disabled = false,
	className = "",
}: {
	value: string;
	options: MultilineSelectOption[];
	onChange: (value: string) => void;
	// Shown in the closed control while value matches no option.
	placeholder: ReactNode;
	// The visible label; it names both the button and the list.
	label: ReactNode;
	disabled?: boolean;
	className?: string;
}) {
	const id = useId();
	const labelId = `${id}-label`;
	const buttonId = `${id}-button`;
	const listId = `${id}-list`;
	const optionId = (index: number) => `${id}-option-${index}`;
	const buttonRef = useRef<HTMLButtonElement>(null);
	const listRef = useRef<HTMLDivElement>(null);
	const [expanded, setExpanded] = useState(false);
	const [active, setActive] = useState(0);
	// The list can't be open while the control is disabled or has nothing to
	// offer. expanded is cleared at once rather than only hidden: left set, the
	// list would reopen by itself (and take focus) as soon as the options came
	// back or the control was enabled again, for example when the selected
	// items' common backups change while the list is open.
	const canOpen = !disabled && options.length > 0;
	if (expanded && !canOpen) setExpanded(false);
	const open = expanded && canOpen;
	const selectedIndex = options.findIndex((option) => option.value === value);
	const selected = selectedIndex >= 0 ? options[selectedIndex] : undefined;
	const activeIndex = Math.min(active, options.length - 1);

	const openList = () => {
		setActive(Math.max(selectedIndex, 0));
		setExpanded(true);
	};
	const close = (returnFocus: boolean) => {
		setExpanded(false);
		if (returnFocus) buttonRef.current?.focus();
	};
	const choose = (index: number) => {
		const option = options[index];
		close(true);
		if (option && option.value !== value) onChange(option.value);
	};

	// Place the list before it is painted, then move focus into it. Focus has
	// to wait for the placement: the list is fixed in <body>, and it is
	// positioned here rather than through state so it is never shown (or
	// focused) at a stale spot.
	useLayoutEffect(() => {
		if (!open) return;
		const place = () => {
			const button = buttonRef.current;
			const list = listRef.current;
			if (!button || !list) return;
			const box = button.getBoundingClientRect();
			// clientHeight leaves out a classic horizontal scrollbar, which
			// innerHeight includes.
			const viewportHeight = document.documentElement.clientHeight;
			const gap = 4;
			const margin = 16;
			const below = viewportHeight - box.bottom - gap - margin;
			const above = box.top - gap - margin;
			const wanted = Math.min(list.scrollHeight, 320);
			const openBelow = wanted <= below || below >= above;
			list.style.left = `${box.left}px`;
			list.style.width = `${box.width}px`;
			list.style.maxHeight = `${Math.max(Math.min(320, openBelow ? below : above), 80)}px`;
			list.style.top = openBelow ? `${box.bottom + gap}px` : "";
			list.style.bottom = openBelow ? "" : `${viewportHeight - box.top + gap}px`;
		};
		place();
		listRef.current?.focus({ preventScroll: true });
		// The list is fixed, so it follows the button when the panel or the
		// page scrolls and when the window is resized.
		window.addEventListener("resize", place);
		document.addEventListener("scroll", place, true);
		return () => {
			window.removeEventListener("resize", place);
			document.removeEventListener("scroll", place, true);
		};
	}, [open]);

	useEffect(() => {
		if (!open) return;
		const closeOutside = (event: PointerEvent) => {
			const target = event.target as Node | null;
			if (target && (buttonRef.current?.contains(target) || listRef.current?.contains(target))) return;
			setExpanded(false);
		};
		document.addEventListener("pointerdown", closeOutside, true);
		return () => document.removeEventListener("pointerdown", closeOutside, true);
	}, [open]);

	useEffect(() => {
		if (!open) return;
		// Keep the highlighted entry in view as the keys move through a long
		// list. jsdom has no scrollIntoView.
		document.getElementById(`${id}-option-${activeIndex}`)?.scrollIntoView?.({ block: "nearest" });
	}, [id, open, activeIndex]);

	const buttonKeyDown = (event: KeyboardEvent<HTMLButtonElement>) => {
		if (event.key !== "ArrowDown" && event.key !== "ArrowUp") return;
		event.preventDefault();
		openList();
	};

	const listKeyDown = (event: KeyboardEvent<HTMLDivElement>) => {
		const last = options.length - 1;
		const move = (index: number) => { event.preventDefault(); setActive(Math.max(0, Math.min(index, last))); };
		switch (event.key) {
		case "ArrowDown": move(activeIndex + 1); break;
		case "ArrowUp": move(activeIndex - 1); break;
		case "PageDown": move(activeIndex + 10); break;
		case "PageUp": move(activeIndex - 10); break;
		case "Home": move(0); break;
		case "End": move(last); break;
		case "Enter":
		case " ":
			event.preventDefault();
			choose(activeIndex);
			break;
		case "Escape":
			event.preventDefault();
			event.stopPropagation();
			close(true);
			break;
		case "Tab":
			// The list sits at the end of <body>. Focus goes back to the button
			// here, before the browser handles the key, so the browser's own
			// Tab (or Shift+Tab) then moves on from the button to its
			// neighbour, not from the end of the page. Don't prevent the
			// default: that would take two presses to leave the control.
			close(true);
			break;
		}
	};

	const lines = (option: MultilineSelectOption) => option.lines.map((line, index) =>
		<span key={index} className={`multiline-select-line${index > 0 ? " detail" : ""}`}>{line}</span>);

	return (
		<div className={`field multiline-select-field ${className}`.trim()}>
			{/* A click on the label focuses the control without opening it,
			    as a <label> does for a <select>. */}
			<span id={labelId} className="multiline-select-label" onClick={() => buttonRef.current?.focus()}>{label}</span>
			<button
				ref={buttonRef}
				id={buttonId}
				type="button"
				className="multiline-select"
				aria-haspopup="listbox"
				aria-expanded={open}
				aria-controls={open ? listId : undefined}
				aria-labelledby={`${labelId} ${buttonId}`}
				disabled={disabled}
				// Safari, and Firefox on macOS, don't focus a button that is
				// clicked. Without this, pressing the button of an open list
				// would blur the list, which closes it, and the click would
				// then open it again. Keeping focus in the list on the press
				// lets the click close it in every browser.
				onMouseDown={(event) => { if (open) event.preventDefault(); }}
				onClick={() => open ? close(true) : openList()}
				onKeyDown={buttonKeyDown}
			>
				<span className="multiline-select-value">{selected ? lines(selected) : <span className="multiline-select-line placeholder">{placeholder}</span>}</span>
			</button>
			{open && createPortal(
				<div
					ref={listRef}
					id={listId}
					className="multiline-select-list"
					role="listbox"
					tabIndex={-1}
					aria-labelledby={labelId}
					aria-activedescendant={optionId(activeIndex)}
					onKeyDown={listKeyDown}
					onBlur={(event) => {
						const next = event.relatedTarget as Node | null;
						if (next && (buttonRef.current?.contains(next) || listRef.current?.contains(next))) return;
						setExpanded(false);
					}}
				>
					{options.map((option, index) => <div
						key={option.value}
						id={optionId(index)}
						className={`multiline-select-option${index === activeIndex ? " active" : ""}`}
						role="option"
						aria-selected={index === selectedIndex}
						onPointerMove={() => { if (index !== activeIndex) setActive(index); }}
						onClick={() => choose(index)}
					>{lines(option)}</div>)}
				</div>,
				document.body,
			)}
		</div>
	);
}
