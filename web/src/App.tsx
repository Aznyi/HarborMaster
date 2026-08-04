import { Navigate, Route, Routes } from "react-router";

import { AppShell } from "./components/AppShell";
import { useHealth } from "./hooks/useHealth";
import { ContainerDetailPage } from "./pages/ContainerDetail";
import { Containers } from "./pages/Containers";
import { Dashboard } from "./pages/Dashboard";
import { Events } from "./pages/Events";
import { Images } from "./pages/Images";
import { Settings } from "./pages/Settings";
import { SnapshotDetailPage } from "./pages/SnapshotDetail";
import { SnapshotReadinessPage } from "./pages/SnapshotReadiness";
import { Snapshots } from "./pages/Snapshots";

/**
 * Health is polled once at the shell level and passed down, so every view
 * agrees on connectivity and the app makes one request per interval rather
 * than one per mounted component.
 */
export function App() {
  const health = useHealth();

  return (
    <AppShell health={health}>
      <Routes>
        <Route path="/" element={<Dashboard health={health} />} />
        <Route path="/containers" element={<Containers />} />
        <Route path="/containers/:id" element={<ContainerDetailPage />} />
        <Route path="/images" element={<Images />} />
        <Route path="/snapshots" element={<Snapshots />} />
        <Route path="/snapshots/:id" element={<SnapshotDetailPage />} />
        <Route path="/snapshots/:id/readiness" element={<SnapshotReadinessPage />} />
        <Route path="/events" element={<Events />} />
        <Route path="/settings" element={<Settings health={health} />} />
        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </AppShell>
  );
}
