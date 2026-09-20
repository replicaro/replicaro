import type { ThemePreference } from "./types";

export function normalizeThemePreference(value: unknown): ThemePreference {
    return value === "system" || value === "light" || value === "neutral" || value === "dark" ? value : "dark";
}

export function applyThemePreference(value: unknown): ThemePreference {
    const theme = normalizeThemePreference(value);
    document.documentElement.dataset.theme = theme;
    return theme;
}
