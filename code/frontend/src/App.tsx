import { BrowserRouter, Navigate, Route, Routes, useParams } from "react-router-dom";
import { useId, useState, useSyncExternalStore } from "react";
import { englishText, getEffectiveLocale, subscribeLocale } from "./i18n";
import { reloadPage } from "./reloadPage";

import MainLayout from "./layouts/MainLayout";
import { Modal, ToastProvider } from "./components/ui";

import Overview from "./pages/Overview";
import Protect from "./pages/Protect";
import Restore from "./pages/Restore";
import FindFile from "./pages/FindFile";
import System from "./pages/System";

function LegacyRepositoryRedirect() {
    const { id = "" } = useParams();
    return <Navigate to={`/restore/${encodeURIComponent(id)}`} replace />;
}

function LegacyFileHistoryRedirect() {
    const { repoId } = useParams();
    return <Navigate to={repoId ? `/file-history/${encodeURIComponent(repoId)}` : "/file-history"} replace />;
}

// "Don't remind me for 30 days" is remembered in this browser only, together
// with the language that failed, so a different language failing later is
// still reported. Storage can be unavailable (private windows, blocked site
// data); the dialog then simply shows on every start.
const languageLoadFailureStorageKey = "replicaro.languageLoadFailure";
const languageLoadFailureSnoozeMs = 30 * 24 * 60 * 60 * 1000;

function languageLoadFailureSnoozed(locale: string): boolean {
    try {
        const stored: unknown = JSON.parse(window.localStorage.getItem(languageLoadFailureStorageKey) ?? "null");
        if (!stored || typeof stored !== "object") return false;
        const { locale: snoozedLocale, until } = stored as { locale?: unknown; until?: unknown };
        return snoozedLocale === locale && typeof until === "number" && Date.now() < until;
    } catch {
        return false;
    }
}

function snoozeLanguageLoadFailure(locale: string): void {
    try {
        window.localStorage.setItem(languageLoadFailureStorageKey, JSON.stringify({ locale, until: Date.now() + languageLoadFailureSnoozeMs }));
    } catch {
        // Nothing to remember it in; the dialog shows again next time.
    }
}

// main.tsx starts in English when the saved language's catalog couldn't be
// loaded. Nothing else on screen would explain why, so say it in a dialog. The
// UI is in English at this point anyway; the text is read from the English
// catalog so that stays true whatever else changes.
// The dialog is part of the first render, before the update status has
// arrived. MainLayout holds back its update dialog while another dialog is
// open, so the two never stack: this one comes first, the update notice after.
function LanguageLoadFailedDialog({ locale }: { locale: string }) {
    // Decided once when the app starts. The storage read has no side effects,
    // so StrictMode calling this twice still opens a single dialog.
    const [open, setOpen] = useState(() => !languageLoadFailureSnoozed(locale));
    const [dontRemind, setDontRemind] = useState(false);
    const messageId = useId();
    if (!open) return null;
    // Close and Escape dismiss the notice for now; only "Continue in English"
    // honors the checkbox.
    const close = () => setOpen(false);
    const continueInEnglish = () => {
        if (dontRemind) snoozeLanguageLoadFailure(locale);
        close();
    };
    return <Modal title={englishText("system.language.loadFailedTitle")} onClose={close} describedBy={messageId} closeLabel={englishText("ui.components.ui.close")} lang="en" dir="ltr">
        <p id={messageId} className="muted" style={{ marginTop: 0 }}>{englishText("system.language.loadFailed")}</p>
        <div className="app-update-actions language-load-failed-actions">
            <button type="button" className="btn primary" onClick={() => reloadPage()}>{englishText("system.language.reloadPage")}</button>
            <div className="language-load-failed-continue">
                <button type="button" className="btn" onClick={continueInEnglish}>{englishText("system.language.continueInEnglish")}</button>
                <label className="check"><input type="checkbox" checked={dontRemind} onChange={(event) => setDontRemind(event.target.checked)} />{englishText("system.language.dontRemind30Days")}</label>
            </div>
        </div>
    </Modal>;
}

// failedLocale is the saved language whose catalog main.tsx couldn't load.
export default function App({ failedLocale }: { failedLocale?: string }) {
    // Re-render mounted UI if the active catalog changes. The native
    // operation data rendered by pages stays untouched.
    useSyncExternalStore(subscribeLocale, getEffectiveLocale);
    return (
        <ToastProvider>
            {failedLocale && <LanguageLoadFailedDialog locale={failedLocale} />}
            <BrowserRouter>
                <Routes>
                    <Route path="/" element={<MainLayout />}>
                        <Route index element={<Overview />} />
                        <Route path="protect" element={<Protect />} />
						<Route path="restore" element={<Restore />} />
                        <Route path="restore/:repoId" element={<Restore />} />
                        <Route path="file-history" element={<FindFile />} />
                        <Route path="file-history/:repoId" element={<FindFile />} />
                        <Route path="system" element={<System />} />

                        <Route path="jobs" element={<Navigate to="/protect" replace />} />
                        <Route path="repositories" element={<Navigate to="/protect" replace />} />
                        <Route path="repositories/:id" element={<LegacyRepositoryRedirect />} />
                        <Route path="snapshots" element={<Navigate to="/restore" replace />} />
                        <Route path="history" element={<Navigate to="/" replace />} />
                        <Route path="logs" element={<Navigate to="/" replace />} />
                        <Route path="engine" element={<Navigate to="/system" replace />} />
                        <Route path="settings" element={<Navigate to="/system" replace />} />
                        <Route path="find-file" element={<LegacyFileHistoryRedirect />} />
                        <Route path="find-file/:repoId" element={<LegacyFileHistoryRedirect />} />
                        <Route path="*" element={<Navigate to="/" replace />} />
                    </Route>
                </Routes>
            </BrowserRouter>
        </ToastProvider>
    );
}
