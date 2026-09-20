import type { WheelEvent } from "react";

export function preventNumberInputWheel(event: WheelEvent<HTMLInputElement>) {
	// Number inputs apply the wheel delta only while focused. Blurring before
	// the browser default preserves ordinary page scrolling without changing
	// the field; spinner buttons and keyboard editing remain available.
	event.currentTarget.blur();
}
