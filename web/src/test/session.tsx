import type { ReactNode } from "react";

import type { Permission, PublicUser, Role } from "../api/authTypes";
import { SessionContext } from "../hooks/useSession";
import type { SessionState } from "../hooks/useSession";

/**
 * A fixed identity for tests.
 *
 * # Why this exists rather than driving the real provider
 *
 * Most of the suite is about a page's behaviour, not about signing in. Standing
 * up the real provider for each would mean every test also asserted the session
 * lifecycle, and a change to that lifecycle would break tests that have nothing
 * to do with it.
 *
 * The sign-in, bootstrap, and expiry behaviour IS tested against the real
 * provider, in Session.test.tsx.
 */

/** What each role holds. Mirrors the server's matrix in internal/domain. */
const READS: Permission[] = [
  "inventory:read",
  "event:read",
  "snapshot:read",
  "drift:read",
  "policy:read",
  "plan:read",
  "acquisition:read",
  "execution:read",
  "rollback:read",
  "automation:read",
  "notification:read",
];

const OPERATOR_WRITES: Permission[] = [
  "inventory:refresh",
  "snapshot:create",
  "drift:annotate",
  "policy:evaluate",
  "policy:annotate",
  "image:refresh",
  "plan:generate",
  "acquisition:create",
  "acquisition:cancel",
  "execution:create",
  "execution:cancel",
  "rollback:create",
  "rollback:cancel",
  "automation:run",
  "automation:approve",
  "automation:pause",
];

const ADMIN_ONLY: Permission[] = [
  "automation:manage",
  "notification:manage",
  "policy:manage",
  "user:manage",
  "audit:read",
];

export function permissionsFor(role: Role): Permission[] {
  switch (role) {
    case "viewer":
      return [...READS];
    case "operator":
      return [...READS, ...OPERATOR_WRITES];
    case "administrator":
      return [...READS, ...OPERATOR_WRITES, ...ADMIN_ONLY];
  }
}

export function testUser(
  role: Role = "administrator",
  overrides: Partial<PublicUser> = {},
): PublicUser {
  return {
    userId: "usr_test0000000000000000",
    username: "tester",
    role,
    status: "active",
    permissions: permissionsFor(role),
    mustChangePassword: false,
    createdAt: "2026-08-01T09:00:00Z",
    ...overrides,
  };
}

/** Builds a session state with sensible no-op actions. */
export function testSession(overrides: Partial<SessionState> = {}): SessionState {
  const user = overrides.user ?? testUser();
  return {
    status: "authenticated",
    user,
    error: null,
    busy: false,
    can: (permission) => user?.permissions.includes(permission) ?? false,
    canAny: (...permissions) =>
      permissions.some((one) => user?.permissions.includes(one) ?? false),
    signIn: async () => {},
    claim: async () => {},
    signOut: async () => {},
    changePassword: async () => {},
    refresh: () => {},
    ...overrides,
  };
}

/** Wraps a tree in a fixed identity. */
export function TestSessionProvider({
  children,
  session = testSession(),
}: {
  children: ReactNode;
  session?: SessionState;
}) {
  return (
    <SessionContext.Provider value={session}>{children}</SessionContext.Provider>
  );
}
