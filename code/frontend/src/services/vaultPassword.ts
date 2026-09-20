function isGoSpace(codePoint: number) {
	return (codePoint >= 0x09 && codePoint <= 0x0d) || codePoint === 0x20 || codePoint === 0x85 ||
		codePoint === 0xa0 || codePoint === 0x1680 || (codePoint >= 0x2000 && codePoint <= 0x200a) ||
		codePoint === 0x2028 || codePoint === 0x2029 || codePoint === 0x202f || codePoint === 0x205f ||
		codePoint === 0x3000;
}

export function validVaultPassword(value: string) {
	if (value === "") return false;
	const first = value.codePointAt(0)!;
	const last = value.codePointAt(value.length - 1)!;
	return !isGoSpace(first) && !isGoSpace(last) &&
		!value.includes("\r") && !value.includes("\n") && !value.includes("\0");
}
