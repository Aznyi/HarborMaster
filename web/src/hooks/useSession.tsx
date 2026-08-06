import {
  createContext,
  useCallback,
  useContext,
  useEffect,
  useMemo,
  useRef,
  useState,
} from "react";
import type { ReactNode } from "react";

import {
  ApiError,
  bootstrapInstallation,
  changePassword as changePasswordRequest,
  clearCsrfToken,
  getBootstrapStatus,
  getSession,
  login as loginRequest,
  logout as logoutRequest,
  onUnauthenticated,
  setCsrfToken,
} from "../api/client";
import type {
  BootstrapRequest,
  ChangePasswordRequest,
  LoginRequest,
  Permission,
  PublicUser,
  SessionResponse,
} from "../api/authTypes";

/**
 * The application's identity state.
 *
 * # The shell renders from this and nothing else
 *
 * There is one source of truth for "who is signed in", and every route decision
 * reads it. Two sources would eventually disagree, and the failure mode of
 * disagreement here is a page that renders estate data to somebody who has been
 * signed out.
 *
 * # Hiding a control is not authorization
 *
 * `can()` exists so the UI does not offer buttons that will fail, which is a
 * usability property. The server refuses regardless of what this returns, and
 * the two are checked against each other by TestEveryPermissionRouteRefusesA
 * RoleThatLacksIt on the backend.
 */

/** What the app knows about the current visitor. */
export type SessionStatus =
  /** The first /auth/session call has not returned yet. */
  | "loading"
  /** The installation has no administrator: show the bootstrap page. */
  | "unclaimed"
  /** No usable session: show the sign-in page. */
  | "anonymous"
  /** Signed in, but the account must choose a new password first. */
  | "passwordChange"
  /** Signed in and able to work. */
  | "authenticated"
  /** The backend could not be reached at all. */
  | "disconnected";

export interface SessionState {
  status: SessionStatus;
  user: PublicUser | null;
  /** The last sign-in failure, for the form to render. Never a server detail. */
  error: string | null;
  /** True while a sign-in, sign-out, or password change is in flight. */
  busy: boolean;

  can: (permission: Permission) => boolean;
  canAny: (...permissions: Permission[]) => boolean;

  signIn: (credentials: LoginRequest) => Promise<void>;
  claim: (request: BootstrapRequest) => Promise<void>;
  signOut: () => Promise<void>;
  changePassword: (request: ChangePasswordRequest) => Promise<void>;
  /** Re-reads the session, after a role change elsewhere for example. */
  refresh: () => void;
}

/**
 * The identity context.
 *
 * Exported so a test can supply a fixed identity without standing up the
 * network stack, and so a future alternative provider is possible. Application
 * code reads it through `useSession` rather than directly.
 */
export const SessionContext = createContext<SessionState | null>(null);

/**
 * Reads the session state.
 *
 * Throws outside a provider rather than returning a permissive default: a
 * component that renders as though nobody were signed in, or as though somebody
 * were, is worse than one that fails loudly in development.
 */
export function useSession(): SessionState {
  const state = useContext(SessionContext);
  if (!state) {
    throw new Error("useSession must be used inside a SessionProvider");
  }
  return state;
}

export function SessionProvider({ children }: { children: ReactNode }) {
  const [status, setStatus] = useState<SessionStatus>("loading");
  const [user, setUser] = useState<PublicUser | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);
  const [reloadToken, setReloadToken] = useState(0);

  // Guards against a resolved promise writing state after unmount, which React
  // reports as an update on an unmounted component and which would also mean a
  // stale identity landing after a sign-out.
  const mounted = useRef(true);
  useEffect(() => {
    mounted.current = true;
    return () => {
      mounted.current = false;
    };
  }, []);

  /** Installs a session from a login, bootstrap, or password-change response. */
  const adopt = useCallback((session: SessionResponse) => {
    setCsrfToken(session.csrfToken);
    setUser(session.user);
    setError(null);
    setStatus(session.user.mustChangePassword ? "passwordChange" : "authenticated");
  }, []);

  /** Drops every trace of the session from this tab. */
  const forget = useCallback((next: SessionStatus) => {
    clearCsrfToken();
    setUser(null);
    setStatus(next);
  }, []);

  // A 401 from ANY request in the app -- including a background poll on a page
  // the operator is not looking at -- ends the session once, here, rather than
  // producing an error on every view that happened to be fetching.
  useEffect(
    () =>
      onUnauthenticated((code) => {
        if (!mounted.current) return;
        forget(code === "bootstrap_required" ? "unclaimed" : "anonymous");
      }),
    [forget],
  );

  // The startup probe: ask who we are, and fall back to asking whether the
  // installation has been claimed at all.
  useEffect(() => {
    const controller = new AbortController();

    void (async () => {
      try {
        const session = await getSession({ signal: controller.signal });
        if (!mounted.current) return;
        adopt(session);
        return;
      } catch (failure) {
        if (!mounted.current) return;

        if (failure instanceof ApiError && failure.isConnectivity) {
          setStatus("disconnected");
          return;
        }
        if (failure instanceof ApiError && failure.code === "bootstrap_required") {
          forget("unclaimed");
          return;
        }
        // Anything else means no usable session. Confirm whether the
        // installation is claimed so the operator lands on the right page --
        // the bootstrap form rather than a sign-in form with no accounts
        // behind it.
      }

      try {
        const bootstrap = await getBootstrapStatus({ signal: controller.signal });
        if (!mounted.current) return;
        forget(bootstrap.completed ? "anonymous" : "unclaimed");
      } catch (failure) {
        if (!mounted.current) return;
        if (failure instanceof ApiError && failure.isConnectivity) {
          setStatus("disconnected");
          return;
        }
        // Fail CLOSED: if the state cannot be established, offer sign-in
        // rather than a bootstrap form that would claim the installation.
        forget("anonymous");
      }
    })();

    return () => controller.abort();
  }, [adopt, forget, reloadToken]);

  const signIn = useCallback(
    async (credentials: LoginRequest) => {
      setBusy(true);
      setError(null);
      try {
        adopt(await loginRequest(credentials));
      } catch (failure) {
        if (!mounted.current) return;
        // The server collapses every credential failure into one message. This
        // renders that message rather than inventing a more specific one.
        setError(signInMessage(failure));
        throw failure;
      } finally {
        if (mounted.current) setBusy(false);
      }
    },
    [adopt],
  );

  const claim = useCallback(
    async (request: BootstrapRequest) => {
      setBusy(true);
      setError(null);
      try {
        adopt(await bootstrapInstallation(request));
      } catch (failure) {
        if (!mounted.current) return;
        setError(bootstrapMessage(failure));
        throw failure;
      } finally {
        if (mounted.current) setBusy(false);
      }
    },
    [adopt],
  );

  const signOut = useCallback(async () => {
    setBusy(true);
    try {
      await logoutRequest();
    } catch {
      // The local session is dropped regardless. A failed sign-out that left
      // the operator apparently signed in would be the worse outcome, and the
      // cookie is gone from the server's point of view or will expire.
    } finally {
      if (mounted.current) {
        forget("anonymous");
        setError(null);
        setBusy(false);
      }
    }
  }, [forget]);

  const changePassword = useCallback(
    async (request: ChangePasswordRequest) => {
      setBusy(true);
      setError(null);
      try {
        // The session ROTATES: the response carries a new CSRF token that must
        // replace the old one, or the next write is refused.
        adopt(await changePasswordRequest(request));
      } catch (failure) {
        if (!mounted.current) return;
        setError(passwordMessage(failure));
        throw failure;
      } finally {
        if (mounted.current) setBusy(false);
      }
    },
    [adopt],
  );

  const refresh = useCallback(() => setReloadToken((token) => token + 1), []);

  const can = useCallback(
    (permission: Permission) => user?.permissions.includes(permission) ?? false,
    [user],
  );
  const canAny = useCallback(
    (...permissions: Permission[]) => permissions.some((one) => can(one)),
    [can],
  );

  const value = useMemo<SessionState>(
    () => ({
      status,
      user,
      error,
      busy,
      can,
      canAny,
      signIn,
      claim,
      signOut,
      changePassword,
      refresh,
    }),
    [status, user, error, busy, can, canAny, signIn, claim, signOut, changePassword, refresh],
  );

  return <SessionContext.Provider value={value}>{children}</SessionContext.Provider>;
}

/**
 * Renders a sign-in failure.
 *
 * Deliberately narrow. The server already collapses unknown-username, wrong
 * password, and disabled-account into one 401, and this must not add detail it
 * does not have.
 */
function signInMessage(failure: unknown): string {
  if (failure instanceof ApiError) {
    if (failure.isConnectivity) return "Cannot reach HarborMaster.";
    if (failure.status === 429) {
      return "Too many attempts. Wait a few minutes and try again.";
    }
    if (failure.status === 401) return "The username or password is incorrect.";
    return failure.message;
  }
  return "Sign-in failed.";
}

function bootstrapMessage(failure: unknown): string {
  if (failure instanceof ApiError) {
    if (failure.isConnectivity) return "Cannot reach HarborMaster.";
    if (failure.status === 404) {
      return "This installation already has an administrator. Sign in instead.";
    }
    if (failure.status === 401 || failure.status === 403) {
      return "That bootstrap token is not valid. Restart HarborMaster to issue a new one.";
    }
    return failure.message;
  }
  return "Could not claim this installation.";
}

function passwordMessage(failure: unknown): string {
  if (failure instanceof ApiError) {
    if (failure.isConnectivity) return "Cannot reach HarborMaster.";
    if (failure.status === 401) return "The current password is incorrect.";
    // A 400 here is the password policy talking, and its message is written for
    // an operator to read.
    return failure.message;
  }
  return "Could not change the password.";
}
