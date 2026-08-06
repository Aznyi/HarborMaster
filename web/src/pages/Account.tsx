import { useCallback, useState } from "react";
import type { FormEvent } from "react";

import type { SessionSummary } from "../api/authTypes";
import { ApiError, listOwnSessions, revokeOwnSession } from "../api/client";
import { PageIntro } from "../components/PageIntro";
import {
  DisconnectedState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { useApiResource } from "../hooks/useApiResource";
import { useSession } from "../hooks/useSession";

/**
 * The operator's own account.
 *
 * Their identity, their password, and their live sessions -- nobody else's.
 * Ending somebody else's session is account administration and lives under
 * Accounts, where the permission check is explicit.
 */
export function Account() {
  const session = useSession();
  const [reload, setReload] = useState(0);

  const sessions = useApiResource(
    useCallback(({ signal }: { signal: AbortSignal }) => listOwnSessions({ signal }), []),
    { key: String(reload) },
  );

  const refresh = useCallback(() => setReload((token) => token + 1), []);

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Your account"
        description="Your identity, your password, and the sessions signed in as you. Every action you take in HarborMaster is recorded against this account."
      />

      <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
        <h3 className="font-semibold">Identity</h3>
        <dl className="mt-4 grid gap-3 text-sm sm:grid-cols-2">
          <Row label="Username">{session.user?.username ?? "—"}</Row>
          <Row label="Role">{session.user?.role ?? "—"}</Row>
          <Row label="Last sign-in">
            {session.user?.lastLoginAt ? formatWhen(session.user.lastLoginAt) : "—"}
          </Row>
          <Row label="Permissions">
            {session.user ? `${session.user.permissions.length} granted` : "—"}
          </Row>
        </dl>

        <details className="mt-4">
          <summary className="cursor-pointer text-sm text-content-muted">
            What this role can do
          </summary>
          <ul className="mt-2 flex flex-wrap gap-1.5">
            {(session.user?.permissions ?? []).map((permission) => (
              <li
                key={permission}
                className="rounded bg-surface-sunken px-1.5 py-0.5 font-mono text-xs"
              >
                {permission}
              </li>
            ))}
          </ul>
        </details>
      </section>

      <PasswordSection />

      <section className="flex flex-col gap-3 rounded-xl border border-border-subtle bg-surface-raised p-5">
        <div className="flex items-center justify-between gap-3">
          <h3 className="font-semibold">Signed-in sessions</h3>
          <button
            type="button"
            onClick={refresh}
            className="rounded-lg border border-border-subtle px-3 py-1.5 text-sm font-medium"
          >
            Refresh
          </button>
        </div>

        {sessions.status === "loading" ? <LoadingState label="Loading sessions" /> : null}
        {sessions.status === "disconnected" ? <DisconnectedState /> : null}
        {sessions.status === "error" && sessions.error ? (
          <ErrorState error={sessions.error} onRetry={sessions.refresh} />
        ) : null}

        {sessions.data ? (
          <SessionList
            sessions={sessions.data.items}
            onRevoke={async (sessionId) => {
              try {
                await revokeOwnSession(sessionId);
              } catch {
                // A failed revoke leaves the session live; refreshing shows the
                // truth rather than an optimistic removal that would be a lie.
              }
              refresh();
            }}
          />
        ) : null}
      </section>
    </div>
  );
}

/** The self-service password change. */
function PasswordSection() {
  const session = useSession();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [note, setNote] = useState<string | null>(null);
  const [failure, setFailure] = useState<string | null>(null);

  async function onSubmit(event: FormEvent) {
    event.preventDefault();
    setNote(null);
    setFailure(null);

    if (next !== confirm) {
      setFailure("The two passwords do not match.");
      return;
    }

    try {
      await session.changePassword({ currentPassword: current, newPassword: next });
      setNote(
        "Password changed. Every other session on this account has been signed out.",
      );
    } catch (error) {
      setFailure(
        error instanceof ApiError && error.status >= 400 && error.status < 500
          ? error.message
          : "The password could not be changed.",
      );
    } finally {
      setCurrent("");
      setNext("");
      setConfirm("");
    }
  }

  return (
    <section className="rounded-xl border border-border-subtle bg-surface-raised p-5">
      <h3 className="font-semibold">Change your password</h3>
      <p className="mt-1 text-sm text-content-muted">
        Changing it ends every session on this account, including any somebody
        else is holding. You stay signed in here.
      </p>

      <form onSubmit={onSubmit} className="mt-4 flex max-w-sm flex-col gap-3">
        <Labelled id="account-current" label="Current password">
          <input
            id="account-current"
            type="password"
            value={current}
            autoComplete="current-password"
            onChange={(event) => setCurrent(event.target.value)}
            className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm"
          />
        </Labelled>
        <Labelled id="account-next" label="New password">
          <input
            id="account-next"
            type="password"
            value={next}
            autoComplete="new-password"
            onChange={(event) => setNext(event.target.value)}
            className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm"
          />
        </Labelled>
        <Labelled id="account-confirm" label="Repeat new password">
          <input
            id="account-confirm"
            type="password"
            value={confirm}
            autoComplete="new-password"
            onChange={(event) => setConfirm(event.target.value)}
            className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm"
          />
        </Labelled>

        {failure ? (
          <p role="alert" className="text-sm text-danger">
            {failure}
          </p>
        ) : null}
        {note ? (
          <p role="status" className="text-sm text-ok">
            {note}
          </p>
        ) : null}

        <button
          type="submit"
          disabled={session.busy || current === "" || next === ""}
          className="self-start rounded-lg bg-accent px-4 py-2 text-sm font-semibold text-white disabled:opacity-50"
        >
          {session.busy ? "Saving…" : "Change password"}
        </button>
      </form>
    </section>
  );
}

function SessionList({
  sessions,
  onRevoke,
}: {
  sessions: SessionSummary[];
  onRevoke: (sessionId: string) => void;
}) {
  if (sessions.length === 0) {
    return <p className="text-sm text-content-muted">No live sessions.</p>;
  }

  return (
    <ul className="flex flex-col gap-2">
      {sessions.map((entry) => (
        <li
          key={entry.sessionId}
          className="flex flex-wrap items-center justify-between gap-3 rounded-lg border border-border-subtle px-3 py-2 text-sm"
        >
          <div className="flex flex-col">
            <span className="font-medium">
              {entry.clientAddr ?? "unknown address"}
              {entry.current ? (
                <span className="ml-2 rounded bg-accent-soft px-1.5 py-0.5 text-xs text-accent">
                  this session
                </span>
              ) : null}
            </span>
            <span className="text-xs text-content-muted">
              {entry.userAgent ? `${entry.userAgent} · ` : ""}
              signed in {formatWhen(entry.createdAt)} · last seen{" "}
              {formatWhen(entry.lastSeenAt)}
            </span>
          </div>

          {entry.current ? null : (
            <button
              type="button"
              onClick={() => onRevoke(entry.sessionId)}
              className="rounded-lg border border-border-subtle px-2 py-1 text-xs font-medium"
            >
              Sign out
            </button>
          )}
        </li>
      ))}
    </ul>
  );
}

function Labelled({
  id,
  label,
  children,
}: {
  id: string;
  label: string;
  children: React.ReactNode;
}) {
  return (
    <div className="flex flex-col gap-1">
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      {children}
    </div>
  );
}

function Row({ label, children }: { label: string; children: React.ReactNode }) {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-content-muted">{label}</dt>
      <dd className="mt-0.5">{children}</dd>
    </div>
  );
}

function formatWhen(iso: string): string {
  const when = new Date(iso);
  return Number.isNaN(when.getTime()) ? iso : when.toLocaleString();
}
