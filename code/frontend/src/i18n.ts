import english from "../public/locales/en.json";
import german from "../public/locales/de.json";
import french from "../public/locales/fr.json";
import arabic from "../public/locales/ar.json";
import urdu from "../public/locales/ur.json";
import hindi from "../public/locales/hi.json";
import spanish from "../public/locales/es.json";
import italian from "../public/locales/it.json";
import mandarin from "../public/locales/zh-Hans.json";
import cantonese from "../public/locales/yue-Hant.json";
import japanese from "../public/locales/ja.json";
import irish from "../public/locales/ga.json";
import korean from "../public/locales/ko.json";
import malay from "../public/locales/ms.json";
import indonesian from "../public/locales/id.json";
import turkish from "../public/locales/tr.json";
import hebrew from "../public/locales/he.json";
import portuguese from "../public/locales/pt-PT.json";
import brazilianPortuguese from "../public/locales/pt-BR.json";
import russian from "../public/locales/ru.json";
import polish from "../public/locales/pl.json";
import dutch from "../public/locales/nl.json";
import bengali from "../public/locales/bn.json";
import greek from "../public/locales/el.json";
import persian from "../public/locales/fa.json";
import vietnamese from "../public/locales/vi.json";
import nigerianPidgin from "../public/locales/pcm.json";
import type { ReactNode } from "react";

export type Message = string | (Partial<Record<Intl.LDMLPluralRule, string>> & { other: string });
export type Catalog = { locale: string; direction: "ltr" | "rtl"; messages: Record<string, Message> };

// Catalogs are application data, never markup or executable templates. Adding a
// locale requires an explicit bundled import so a settings/API value cannot turn
// into an arbitrary network fetch or an unreviewed translation source.
// English is the source of truth for wording, including the strength of claims
// and warnings. Report source issues separately; translations must preserve the
// same meaning and keep Replicaro, Restic, Kopia, and rclone unchanged.
const catalogs = {
    en: english as Catalog,
    de: german as Catalog,
    fr: french as Catalog,
    ar: arabic as Catalog,
    ur: urdu as Catalog,
    hi: hindi as Catalog,
    es: spanish as Catalog,
    it: italian as Catalog,
    "zh-Hans": mandarin as Catalog,
    "yue-Hant": cantonese as Catalog,
    ja: japanese as Catalog,
    ga: irish as Catalog,
    ko: korean as Catalog,
    ms: malay as Catalog,
    id: indonesian as Catalog,
    tr: turkish as Catalog,
    he: hebrew as Catalog,
    "pt-PT": portuguese as Catalog,
    "pt-BR": brazilianPortuguese as Catalog,
    ru: russian as Catalog,
    pl: polish as Catalog,
    nl: dutch as Catalog,
    bn: bengali as Catalog,
    el: greek as Catalog,
    fa: persian as Catalog,
    vi: vietnamese as Catalog,
    pcm: nigerianPidgin as Catalog,
};
export type LanguagePreference = "system" | keyof typeof catalogs;

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

let effectiveLocale: keyof typeof catalogs = "en";
const listeners = new Set<() => void>();

export function getEffectiveLocale(): string { return effectiveLocale; }
export function subscribeLocale(listener: () => void): () => void {
    listeners.add(listener);
    return () => listeners.delete(listener);
}

export function activateLocale(requested: string | undefined): void {
    // The backend resolves OS language variants to a registered catalog. Use
    // that catalog's locale for text and formatting, not the browser's locale.
    const next = requested && Object.hasOwn(catalogs, requested) ? requested as keyof typeof catalogs : "en";
    effectiveLocale = next;
    if (typeof document !== "undefined") {
        document.documentElement.lang = catalogs[next].locale;
        document.documentElement.dir = catalogs[next].direction;
    }
    listeners.forEach((listener) => listener());
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

export function t(key: string, values: Record<string, string | number> = {}): string {
    const message = catalogs[effectiveLocale].messages[key] ?? catalogs.en.messages[key];
    return message === undefined ? key : formatMessage(message, values);
}

export function knownMessage(key: string, fallback: string): string {
    return Object.hasOwn(catalogs.en.messages, key) ? t(key) : fallback;
}

export function renderMessage(key: string, values: Record<string, ReactNode>): ReactNode[] {
    const message = catalogs[effectiveLocale].messages[key] ?? catalogs.en.messages[key];
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
