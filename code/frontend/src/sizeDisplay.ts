import { formatDisplayNumber } from "./i18n";

// Saved byte counts (a job's source size, a vault's size) are shown in decimal
// units with one decimal below 10 and none from 10 up: "1.8 TB", "47 GB",
// "512 B". The Protect cards and the dashboard's size tiles both use this, so
// the same number is never rounded two ways. Sizes the backend formats
// (Restore browse, File History, the Restore backup list) come from
// engines.FormatBytes, which gives the same text apart from the locale's
// separators (decimal and grouping); keep the two in step (see the shared
// cases in the tests).
export const DECIMAL_SIZE_UNITS = ["B", "KB", "MB", "GB", "TB"] as const;

// The decimal unit a byte count is shown in (its index in DECIMAL_SIZE_UNITS)
// and the value in that unit.
export function decimalSizeUnit(bytes: number): { value: number; unit: number } {
	let value = bytes;
	let unit = 0;
	while (value >= 1000 && unit < DECIMAL_SIZE_UNITS.length - 1) {
		value /= 1000;
		unit += 1;
	}
	return { value, unit };
}

export function readableSize(bytes: number): string {
	const { value, unit } = decimalSizeUnit(bytes);
	const digits = value >= 10 || unit === 0 ? 0 : 1;
	return `${formatDisplayNumber(value, { minimumFractionDigits: digits, maximumFractionDigits: digits })} ${DECIMAL_SIZE_UNITS[unit]}`;
}
