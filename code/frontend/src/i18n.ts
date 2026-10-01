import english from "../public/locales/en.json";
import type { ReactNode } from "react";

export type Message = string | (Partial<Record<Intl.LDMLPluralRule, string>> & { other: string });
export type Catalog = { locale: string; direction: "ltr" | "rtl"; messages: Record<string, Message> };

// Catalogs are application data, never markup or executable templates. Adding a
// locale requires an explicit entry here so a settings/API value cannot turn
// into an arbitrary network fetch or an unreviewed translation source: the
// loader below only ever requests /locales/<id>.json for an id in this list.
// English is the source of truth for wording, including the strength of claims
// and warnings. Report source issues separately; translations must preserve the
// same meaning and keep Replicaro, Restic, Kopia, and rclone unchanged.
const supportedLocales = [
    "en", "de", "fr", "ar", "ur", "hi", "es", "it", "zh-Hans", "yue-Hant", "ja", "ga", "ko", "ms", "id",
    "tr", "he", "pt-PT", "pt-BR", "ru", "pl", "nl", "bn", "el", "fa", "vi", "pcm",
] as const;
type SupportedLocale = typeof supportedLocales[number];
export type LanguagePreference = "system" | SupportedLocale;

// English is the only catalog compiled into the main bundle. t() falls back to
// English for any key a translation lacks, and English is what the UI renders
// when another catalog can't be downloaded, so it has to be available without
// a request. Bundling all 27 catalogs made the main script roughly 3.5 MB, most
// of it translations nobody on that install would read.
//
// Every other catalog is fetched on demand from the plain copy Vite already
// copies to dist/locales/. Those files have to stay in the build because the
// backend reads them from the embedded UI for its own translated notifications
// (locale.LoadFS), so fetching them keeps one copy of each catalog in the
// binary. Switching to import() would give each catalog a hashed chunk *as
// well as* the plain copy, i.e. every catalog embedded twice. Because the
// plain names don't change between releases, the server sends /locales/ with
// Cache-Control: no-cache (see serveWebUIFile) so an upgrade can't leave a
// browser on last version's catalog.
const englishCatalog = english as Catalog;
const loadedCatalogs: Partial<Record<SupportedLocale, Catalog>> = { en: englishCatalog };
const pendingCatalogs = new Map<SupportedLocale, Promise<Catalog>>();

// Endonyms let users find a language even when the current UI is unfamiliar.
// English and the system option keep their existing translated catalog labels.
export const languageNames = {
    de: "Deutsch",
    fr: "Français",
    ar: "العربية",
    ur: "اردو",
    hi: "हिन्दी",
    es: "Español",
    it: "Italiano",
    "zh-Hans": "普通话（简体中文）",
    "yue-Hant": "粵語（繁體中文）",
    ja: "日本語",
    ga: "Gaeilge",
    ko: "한국어",
    ms: "Bahasa Melayu (Malaysia)",
    id: "Bahasa Indonesia",
    tr: "Türkçe",
    he: "עברית",
    "pt-PT": "Português (Portugal)",
    "pt-BR": "Português (Brasil)",
    ru: "Русский",
    pl: "Polski",
    nl: "Nederlands",
    bn: "বাংলা",
    el: "Ελληνικά",
    fa: "فارسی",
    vi: "Tiếng Việt",
    pcm: "Nigerian Pidgin",
} satisfies Record<Exclude<LanguagePreference, "system" | "en">, string>;

let effectiveLocale: SupportedLocale = "en";
const listeners = new Set<() => void>();

export function getEffectiveLocale(): string { return effectiveLocale; }
export function isRightToLeft(): boolean { return loadedCatalogs[effectiveLocale]?.direction === "rtl"; }
export function subscribeLocale(listener: () => void): () => void {
    listeners.add(listener);
    return () => listeners.delete(listener);
}

function supportedLocale(requested: string | undefined): SupportedLocale {
    // The backend resolves OS language variants to a registered catalog. Use
    // that catalog's locale for text and formatting, not the browser's locale.
    return requested && (supportedLocales as readonly string[]).includes(requested) ? requested as SupportedLocale : "en";
}

function isCatalog(value: unknown, locale: string): value is Catalog {
    if (!value || typeof value !== "object") return false;
    const candidate = value as Partial<Catalog>;
    return candidate.locale === locale && (candidate.direction === "ltr" || candidate.direction === "rtl") &&
        typeof candidate.messages === "object" && candidate.messages !== null && !Array.isArray(candidate.messages);
}

// Adds a catalog that is already in memory. The loader uses it for downloaded
// catalogs, and tests use it to make a language available without a network
// round trip. Only locales from the supported list are accepted.
export function registerCatalog(catalog: Catalog): void {
    const locale = catalog.locale;
    if (!(supportedLocales as readonly string[]).includes(locale) || !isCatalog(catalog, locale)) {
        throw new Error(`not a supported locale catalog: ${locale}`);
    }
    loadedCatalogs[locale as SupportedLocale] = catalog;
}

// A catalog request that hangs would otherwise hold the first render forever
// (main.tsx waits for it). After this long the request is aborted and treated
// like any other failed load, so the UI starts in English.
const catalogFetchTimeoutMs = 15_000;

async function fetchCatalog(locale: SupportedLocale): Promise<Catalog> {
    const response = await fetch(`/locales/${locale}.json`, { credentials: "same-origin", signal: AbortSignal.timeout(catalogFetchTimeoutMs) });
    if (!response.ok) throw new Error(`catalog ${locale} returned ${response.status}`);
    const catalog: unknown = await response.json();
    // A catalog for another language (or something that isn't a catalog at
    // all) is a failed load, not a reason to show the wrong language.
    if (!isCatalog(catalog, locale)) throw new Error(`catalog ${locale} is not the requested catalog`);
    return catalog;
}

function loadCatalog(locale: SupportedLocale): Promise<Catalog> {
    const loaded = loadedCatalogs[locale];
    if (loaded) return Promise.resolve(loaded);
    // Share one request between callers asking for the same catalog. A failed
    // request is forgotten so the next attempt downloads it again.
    let pending = pendingCatalogs.get(locale);
    if (!pending) {
        pending = fetchCatalog(locale)
            .then((catalog) => { registerCatalog(catalog); return catalog; })
            .finally(() => pendingCatalogs.delete(locale));
        pendingCatalogs.set(locale, pending);
    }
    return pending;
}

function applyLocale(locale: SupportedLocale, catalog: Catalog): void {
    effectiveLocale = locale;
    if (typeof document !== "undefined") {
        document.documentElement.lang = catalog.locale;
        document.documentElement.dir = catalog.direction;
    }
    listeners.forEach((listener) => listener());
}

// "failed" means the catalog couldn't be downloaded or wasn't valid; the
// current language stays active.
export type LocaleActivation = "activated" | "failed";

// Switches the UI to a locale, downloading its catalog first when this session
// hasn't loaded it yet. In the app only main.tsx calls this, before the first
// render: once for the saved language and, if that catalog fails, a second
// time for English. Changing the language on the System page saves the
// settings and reloads the page, so a catalog is never swapped while the UI is
// showing.
// A catalog that is already loaded (always the case for English) is applied
// before this function first awaits, so tests can switch languages
// synchronously. Nothing here throws: a failed download leaves the current
// language in place so the UI never stalls on a missing translation.
export async function activateLocale(requested: string | undefined): Promise<LocaleActivation> {
    const next = supportedLocale(requested);
    const loaded = loadedCatalogs[next];
    if (loaded) {
        applyLocale(next, loaded);
        return "activated";
    }
    let catalog: Catalog;
    try {
        catalog = await loadCatalog(next);
    } catch {
        return "failed";
    }
    applyLocale(next, catalog);
    return "activated";
}

// Intl data varies by browser. If a catalog's locale is unsupported for a
// formatter, Intl uses its locale fallback; translated messages stay selected.
export function formatDisplayNumber(value: number, options?: Intl.NumberFormatOptions, locale: string = effectiveLocale): string {
    return new Intl.NumberFormat(locale, options).format(value);
}

export function formatDisplayDate(value: Date, options: Intl.DateTimeFormatOptions, locale: string = effectiveLocale): string {
    // All UI dates use the Gregorian calendar. Locale still controls month
    // names, digits, and field order; e.g. Persian must not switch the year or
    // month to the Persian calendar. Keep this aligned with date-picker keys.
    return value.toLocaleDateString(locale, { ...options, calendar: "gregory" });
}

export function formatDisplayDateTime(value: Date, options: Intl.DateTimeFormatOptions, locale: string = effectiveLocale): string {
    // Apply the same calendar rule to timestamps without changing their instant
    // or time zone. Native output and serialized timestamps stay untouched.
    return value.toLocaleString(locale, { ...options, calendar: "gregory" });
}

function interpolate(template: string, values: Record<string, string | number>, locale: string): string {
    // Replace only named placeholders. Values remain plain React text when the
    // caller renders the result; strings (including names and native values)
    // stay exact. Only explicitly numeric UI values receive locale formatting.
    return template.replace(/\{([a-zA-Z][a-zA-Z0-9]*)\}/g, (match, name: string) =>
        Object.hasOwn(values, name) ? typeof values[name] === "number" ? formatDisplayNumber(values[name] as number, undefined, locale) : String(values[name]) : match);
}

export function formatMessage(message: Message, values: Record<string, string | number> = {}, locale: string = effectiveLocale): string {
    const template = typeof message === "string" ? message :
        (message[new Intl.PluralRules(locale).select(Number(values.count))] ?? message.other);
    return interpolate(template, values, locale);
}

// t(), renderMessage(), and the formatters stay synchronous. They only read
// catalogs that are already loaded; activateLocale does any downloading first.
const englishMessages = englishCatalog.messages;
function currentMessages(): Record<string, Message> {
    return (loadedCatalogs[effectiveLocale] ?? englishCatalog).messages;
}

export function t(key: string, values: Record<string, string | number> = {}): string {
    const message = currentMessages()[key] ?? englishMessages[key];
    return message === undefined ? key : formatMessage(message, values);
}

// English text regardless of the active language, for the few places that must
// stay in English: the Reset to English dialog, and the notice that the saved
// language couldn't be loaded. English picked in the selector is confirmed in
// the current language and doesn't use this.
export function englishText(key: string, values: Record<string, string | number> = {}): string {
    const message = englishMessages[key];
    return message === undefined ? key : formatMessage(message, values, "en");
}

export function knownMessage(key: string, fallback: string): string {
    return Object.hasOwn(englishMessages, key) ? t(key) : fallback;
}

export function renderMessage(key: string, values: Record<string, ReactNode>): ReactNode[] {
    const message = currentMessages()[key] ?? englishMessages[key];
    if (message === undefined) return [key];
    const template = typeof message === "string" ? message :
        (message[new Intl.PluralRules(effectiveLocale).select(Number(values.count))] ?? message.other);
    // A named placeholder may be a real React link or emphasis node. The
    // catalog only supplies surrounding text and cannot inject HTML or a URL.
    const parts: ReactNode[] = [];
    let cursor = 0;
    for (const match of template.matchAll(/\{([a-zA-Z][a-zA-Z0-9]*)\}/g)) {
        parts.push(template.slice(cursor, match.index));
        const value = values[match[1]];
        parts.push(Object.hasOwn(values, match[1]) ? typeof value === "number" ? formatDisplayNumber(value) : value : match[0]);
        cursor = match.index + match[0].length;
    }
    parts.push(template.slice(cursor));
    return parts;
}
