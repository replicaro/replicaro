import { BrowserRouter, Navigate, Route, Routes, useParams } from "react-router-dom";

import MainLayout from "./layouts/MainLayout";
import { ToastProvider } from "./components/ui";

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

export default function App() {
    return (
        <ToastProvider>
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
