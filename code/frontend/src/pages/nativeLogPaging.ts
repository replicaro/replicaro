import type { LogPresentation } from "./nativeLogFormat";
import type { OperationLogResponse } from "../types";
import { formatOperationLogText } from "./nativeLogFormat";

const sourceHeader = /^\[(?:restic|kopia|replicaro) · [^\]\r\n]+ · [^\]\r\n]+\]\r?\n?$/;

export type ReadableLogPage = OperationLogResponse & { readableBody?: string };

// Read the complete readable log only for its open view. Raw keeps the first
// transport page and subsequently fetches pages directly, without formatting.
export class NativeLogPager {
    private generation = 0;

    clear() {
        this.generation++;
    }

    async read(fetchPage: (offset: number) => Promise<OperationLogResponse>,
        current: () => boolean, engine?: "restic" | "kopia", completedWithIssues = false,
        presentation: LogPresentation = {}): Promise<ReadableLogPage> {
        const generation = ++this.generation;
        const check = () => {
            if (!current() || generation !== this.generation) throw new DOMException("Log view changed", "AbortError");
        };
        const page = await fetchPage(0);
        check();
        if (!page.rawBytes || !Number.isSafeInteger(page.offset)) return page;
        const validate = (candidate: OperationLogResponse, expectedOffset: number) => {
            if (!candidate.rawBytes || candidate.offset !== expectedOffset || !Number.isSafeInteger(candidate.nextOffset) ||
                !Number.isSafeInteger(candidate.size) || candidate.size !== page.size ||
                candidate.nextOffset - candidate.offset !== candidate.rawBytes.length || candidate.nextOffset > candidate.size ||
                candidate.eof !== (candidate.nextOffset === candidate.size) || (!candidate.eof && !candidate.rawBytes.length)) {
                throw new Error("Log changed while loading; reopen the log.");
            }
        };
        validate(page, 0);
        if (!page.rawBytes.length) return { ...page, readableBody: "", readableDiagnostics: [] };

        let section = "";
        let sectionParts: string[] = [];
        const bodies: string[] = [];
        const readableDiagnostics: string[] = [];
        const finishSection = () => {
            const source = (section ? section + "\n" : "") + sectionParts.join("");
            const body = formatOperationLogText(source, engine, readableDiagnostics, completedWithIssues, presentation);
            if (body) bodies.push(body);
            sectionParts = [];
        };
        const line = (text: string) => {
            // Only original physical source lines delimit sections. Decoded
            // JSON filenames can never create a section or hide diagnostics.
            if (sourceHeader.test(text)) {
                finishSection();
                section = text.trimEnd();
            } else sectionParts.push(text);
        };

        const decoder = new TextDecoder("utf-8", { ignoreBOM: true });
        let lineParts: string[] = [];
        let pendingCR = false;
        let position = 0;
        while (true) {
            check();
            const chunk = position === page.offset ? page : await fetchPage(position);
            check();
            validate(chunk, position);
            const bytes = chunk.rawBytes!;
            let start = 0;
            if (pendingCR) {
                // A CRLF pair may straddle transport pages. Complete the pair
                // before publishing the record; a lone CR is a native boundary.
                if (bytes[0] === 10) {
                    lineParts.push(decoder.decode(bytes.subarray(0, 1), { stream: true }));
                    start = 1;
                }
                line(lineParts.join(""));
                lineParts = []; pendingCR = false;
            }
            while (start < bytes.length) {
                const lf = bytes.indexOf(10, start);
                const cr = engine === "kopia" || section.startsWith("[kopia · ") ? bytes.indexOf(13, start) : -1;
                const newline = lf < 0 ? cr : cr < 0 ? lf : Math.min(lf, cr);
                let end = newline < 0 ? bytes.length : newline + 1;
                if (newline >= 0 && bytes[newline] === 13 && bytes[end] === 10) end++;
                lineParts.push(decoder.decode(bytes.subarray(start, end), { stream: true }));
                pendingCR = newline >= 0 && bytes[newline] === 13 && end === bytes.length && bytes[end - 1] === 13;
                if (newline >= 0 && !pendingCR) {
                    line(lineParts.join(""));
                    lineParts = [];
                }
                start = end;
            }
            if (chunk.eof) {
                if (lineParts.length) line(lineParts.join("") + decoder.decode());
                break;
            }
            position = chunk.nextOffset;
        }
        check();
        finishSection();
        const readableBody = bodies.join("\n\n");
        check();
        return { ...page, readableBody, readableDiagnostics };
    }
}


// Raw navigation may reuse the open view's readable result only while the
// response still describes the same source. A missing/changed file is a read
// failure, never a new empty log or a mixture of old readable and new Raw text.
export function retainReadableLog(page: OperationLogResponse, previous: OperationLogResponse | null, offset: number): OperationLogResponse {
    if (!previous?.available || !page.available || page.size !== previous.size ||
        page.offset !== offset || !Number.isSafeInteger(page.nextOffset) ||
        page.nextOffset <= offset || page.nextOffset > page.size ||
        page.eof !== (page.nextOffset === page.size) ||
        (page.rawBytes && page.rawBytes.length !== page.nextOffset - offset)) {
        throw new Error("Log changed while loading; reopen the log.");
    }
    return { ...page, readableBody: previous.readableBody, readableDiagnostics: previous.readableDiagnostics };
}
