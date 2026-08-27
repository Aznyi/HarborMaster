import { Navigate, Route, Routes } from "react-router";
import type { ReactNode } from "react";

import type { Permission } from "./api/authTypes";
import { AppShell } from "./components/AppShell";
import { DisconnectedState, EmptyState, LoadingState } from "./components/States";
import { useHealth } from "./hooks/useHealth";
import { useSession } from "./hooks/useSession";
import { Account } from "./pages/Account";
import { AcquisitionDetail } from "./pages/AcquisitionDetail";
import { Acquisitions } from "./pages/Acquisitions";
import { Audit } from "./pages/Audit";
import { Automation } from "./pages/Automation";
import { AutomationPaused } from "./pages/AutomationPaused";
import { PendingApprovals } from "./pages/PendingApprovals";
import { AutomationRun } from "./pages/AutomationRun";
import { ChangePassword } from "./pages/ChangePassword";
import { ClaimInstallation } from "./pages/ClaimInstallation";
import { ExecutionDetail } from "./pages/ExecutionDetail";
import { Executions } from "./pages/Executions";
import { RollbackDetail } from "./pages/RollbackDetail";
import { Rollbacks } from "./pages/Rollbacks";
import { ChangePlans } from "./pages/ChangePlans";
import { ContainerDetailPage } from "./pages/ContainerDetail";
import { ContainerPlan } from "./pages/ContainerPlan";
import { ContainerDrift } from "./pages/ContainerDrift";
import { Containers } from "./pages/Containers";
import { ActivitySection } from "./pages/ActivitySection";
import { Dashboard } from "./pages/Dashboard";
import { Dependencies } from "./pages/Dependencies";
import { Drift } from "./pages/Drift";
import { Events } from "./pages/Events";
import { ImageDetailPage } from "./pages/ImageDetail";
import { ImageUpdates } from "./pages/ImageUpdates";
import { Images } from "./pages/Images";
import { ContainerPolicy } from "./pages/ContainerPolicy";
import { Policies } from "./pages/Policies";
import { PolicyViolations } from "./pages/PolicyViolations";
import { Settings } from "./pages/Settings";
import { UpdatesSection } from "./pages/UpdatesSection";
import { SignIn } from "./pages/SignIn";
import { SnapshotDetailPage } from "./pages/SnapshotDetail";
import { SnapshotReadinessPage } from "./pages/SnapshotReadiness";
import { Snapshots } from "./pages/Snapshots";
import { Notifications } from "./pages/Notifications";
import { UpdatePolicies } from "./pages/UpdatePolicies";
import { Users } from "./pages/Users";

/**
 * The application root.
 *
 * # Nothing about the estate renders before a session exists
 *
 * The four pre-session states -- loading, unclaimed, anonymous, and
 * password-change -- return BEFORE the shell is constructed, so no navigation,
 * no connectivity indicator, and no data hook is mounted for a visitor who has
 * not signed in. Hiding those with CSS, or mounting them and letting each
 * request 401, would leak both structure and traffic.
 *
 * This is a usability boundary rather than the security boundary: the API
 * refuses an unauthenticated request whatever the browser renders. The two are
 * checked against each other by TestOnlyTheFourPublicRoutesAnswerWithoutASession
 * on the backend.
 */
export function App() {
  const session = useSession();

  switch (session.status) {
    case "loading":
      return (
        <div className="grid min-h-screen place-items-center p-6">
          <LoadingState label="Checking your session" />
        </div>
      );

    case "disconnected":
      return (
        <div className="grid min-h-screen place-items-center p-6">
          <DisconnectedState />
        </div>
      );

    case "unclaimed":
      return <ClaimInstallation />;

    case "anonymous":
      return <SignIn />;

    case "passwordChange":
      return <ChangePassword />;

    case "authenticated":
      return <AuthenticatedApp />;
  }
}

/** The signed-in application. */
function AuthenticatedApp() {
  // Health is polled once at the shell level and passed down, so every view
  // agrees on connectivity and the app makes one request per interval rather
  // than one per mounted component.
  const health = useHealth();

  return (
    <AppShell health={health}>
      <Routes>
        <Route path="/" element={<Dashboard health={health} />} />

        {/* Section landings. Transitional signposts, not implementations: they
          * link to the pages below and hold no state of their own. Every route
          * they point at still exists and is still reachable directly, so an
          * existing bookmark is unaffected by this grouping.
          */}
        <Route
          path="/updates"
          element={<Guard permission="inventory:read"><UpdatesSection /></Guard>}
        />
        <Route
          path="/activity"
          element={<Guard permission="event:read"><ActivitySection /></Guard>}
        />

        <Route
          path="/containers"
          element={<Guard permission="inventory:read"><Containers /></Guard>}
        />
        <Route
          path="/containers/:id"
          element={<Guard permission="inventory:read"><ContainerDetailPage /></Guard>}
        />
        <Route
          path="/images"
          element={<Guard permission="inventory:read"><Images /></Guard>}
        />
        <Route
          path="/images/updates"
          element={<Guard permission="inventory:read"><ImageUpdates /></Guard>}
        />
        <Route
          path="/images/:id"
          element={<Guard permission="inventory:read"><ImageDetailPage /></Guard>}
        />

        <Route
          path="/snapshots"
          element={<Guard permission="snapshot:read"><Snapshots /></Guard>}
        />
        <Route
          path="/snapshots/:id"
          element={<Guard permission="snapshot:read"><SnapshotDetailPage /></Guard>}
        />
        <Route
          path="/snapshots/:id/readiness"
          element={<Guard permission="snapshot:read"><SnapshotReadinessPage /></Guard>}
        />

        <Route path="/drift" element={<Guard permission="drift:read"><Drift /></Guard>} />
        <Route
          path="/drift/container/:id"
          element={<Guard permission="drift:read"><ContainerDrift /></Guard>}
        />

        <Route
          path="/policies"
          element={<Guard permission="policy:read"><Policies /></Guard>}
        />
        <Route
          path="/compliance"
          element={<Guard permission="policy:read"><PolicyViolations /></Guard>}
        />
        <Route
          path="/policy/container/:id"
          element={<Guard permission="policy:read"><ContainerPolicy /></Guard>}
        />

        <Route path="/plans" element={<Guard permission="plan:read"><ChangePlans /></Guard>} />
        <Route
          path="/plans/container/:id"
          element={<Guard permission="plan:read"><ContainerPlan /></Guard>}
        />

        <Route
          path="/acquisitions"
          element={<Guard permission="acquisition:read"><Acquisitions /></Guard>}
        />
        <Route
          path="/acquisitions/:id"
          element={<Guard permission="acquisition:read"><AcquisitionDetail /></Guard>}
        />

        <Route
          path="/executions"
          element={<Guard permission="execution:read"><Executions /></Guard>}
        />
        <Route
          path="/executions/:id"
          element={<Guard permission="execution:read"><ExecutionDetail /></Guard>}
        />

        <Route
          path="/rollbacks"
          element={<Guard permission="rollback:read"><Rollbacks /></Guard>}
        />
        <Route
          path="/rollbacks/:id"
          element={<Guard permission="rollback:read"><RollbackDetail /></Guard>}
        />

        <Route
          path="/automation"
          element={<Guard permission="automation:read"><Automation /></Guard>}
        />
        <Route
          path="/automation/runs/:id"
          element={<Guard permission="automation:read"><AutomationRun /></Guard>}
        />
        <Route
          path="/automation/approvals"
          element={<Guard permission="automation:read"><PendingApprovals /></Guard>}
        />
        <Route
          path="/automation/paused"
          element={<Guard permission="automation:read"><AutomationPaused /></Guard>}
        />
        <Route
          path="/update-policies"
          element={<Guard permission="automation:read"><UpdatePolicies /></Guard>}
        />
        {/* Guarded on READ, not on manage. Every role may inspect what a
            container is waiting for -- a relationship is frequently the answer
            to "why has that not updated" -- and the page itself withholds the
            create and remove controls from anybody without dependency:manage. */}
        <Route
          path="/dependencies"
          element={<Guard permission="dependency:read"><Dependencies /></Guard>}
        />

        <Route
          path="/notifications"
          element={
            <Guard permission="notification:read">
              <Notifications />
            </Guard>
          }
        />

        <Route path="/events" element={<Guard permission="event:read"><Events /></Guard>} />

        <Route path="/users" element={<Guard permission="user:manage"><Users /></Guard>} />
        <Route path="/audit" element={<Guard permission="audit:read"><Audit /></Guard>} />

        <Route path="/account" element={<Account />} />
        <Route path="/settings" element={<Settings health={health} />} />

        <Route path="*" element={<Navigate to="/" replace />} />
      </Routes>
    </AppShell>
  );
}

/**
 * Renders a page only when the account holds the permission.
 *
 * The refusal is a page rather than a redirect: an operator who followed a link
 * from a colleague should be told their role does not include it, not silently
 * bounced to the dashboard wondering whether the link was wrong.
 */
function Guard({
  permission,
  children,
}: {
  permission: Permission;
  children: ReactNode;
}) {
  const session = useSession();
  if (session.can(permission)) return <>{children}</>;

  return (
    <EmptyState
      title="Not permitted"
      description={`Your role does not include ${permission}. Ask an administrator if you need it — HarborMaster enforces this on the server, so this page would not load anyway.`}
    />
  );
}
