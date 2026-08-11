import { useCallback, useState } from "react";

import type {
  CreatedUser,
  PublicUser,
  Role,
  UserStatus,
} from "../api/authTypes";
import {
  ApiError,
  createUser,
  listUsers,
  resetUserPassword,
  updateUser,
} from "../api/client";
import { PageIntro } from "../components/PageIntro";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { useApiResource } from "../hooks/useApiResource";
import { useSession } from "../hooks/useSession";

/**
 * Account administration.
 *
 * # There is no delete
 *
 * Disabling preserves the history an audit record depends on. An account that
 * never existed and one that was turned off are different facts, and an audit
 * log that cannot tell them apart is worth less.
 *
 * # An administrator cannot change their own account here
 *
 * The server refuses it, and the UI does not offer it. The one legitimate case
 * -- stepping down -- is better done by another administrator, and every other
 * case is a mistake that would need console recovery to undo.
 *
 * # A generated password is shown exactly once
 *
 * It is never stored in plaintext and never retrievable again, so it is
 * rendered in a panel the operator has to dismiss rather than a toast that
 * disappears.
 */
export function Users() {
  const session = useSession();
  const [reload, setReload] = useState(0);
  const [created, setCreated] = useState<CreatedUser | null>(null);
  const [reset, setReset] = useState<{ username: string; password: string } | null>(
    null,
  );
  const [failure, setFailure] = useState<string | null>(null);

  const users = useApiResource<Awaited<ReturnType<typeof listUsers>>>(
    useCallback(({ signal }) => listUsers(1, 100, { signal }), []),
    { key: String(reload) },
  );

  const refresh = useCallback(() => setReload((token) => token + 1), []);

  const onCreate = useCallback(
    async (username: string, role: Role) => {
      setFailure(null);
      try {
        setCreated(await createUser({ username, role }));
        refresh();
      } catch (error) {
        setFailure(describe(error, "The account could not be created."));
      }
    },
    [refresh],
  );

  const onChangeRole = useCallback(
    async (user: PublicUser, role: Role) => {
      setFailure(null);
      try {
        await updateUser(user.userId, { role });
        refresh();
      } catch (error) {
        setFailure(describe(error, "The role could not be changed."));
      }
    },
    [refresh],
  );

  const onChangeStatus = useCallback(
    async (user: PublicUser, status: UserStatus) => {
      setFailure(null);
      try {
        await updateUser(user.userId, { status });
        refresh();
      } catch (error) {
        setFailure(describe(error, "The account status could not be changed."));
      }
    },
    [refresh],
  );

  const onReset = useCallback(async (user: PublicUser) => {
    setFailure(null);
    try {
      const outcome = await resetUserPassword(user.userId);
      setReset({ username: user.username, password: outcome.temporaryPassword });
    } catch (error) {
      setFailure(describe(error, "The password could not be reset."));
    }
  }, []);

  if (!session.can("user:manage")) {
    return <NotPermitted />;
  }

  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Accounts"
        description="Who can sign in to HarborMaster, and what each of them may do. Disabling an account ends its sessions immediately and preserves its history; there is no delete."
      />

      {created ? (
        <OneTimeSecret
          title={`Account “${created.user.username}” created`}
          secret={created.temporaryPassword ?? ""}
          onDismiss={() => setCreated(null)}
        />
      ) : null}

      {reset ? (
        <OneTimeSecret
          title={`Password reset for “${reset.username}”`}
          secret={reset.password}
          onDismiss={() => setReset(null)}
        />
      ) : null}

      {failure ? (
        <p
          role="alert"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          {failure}
        </p>
      ) : null}

      <CreateAccount onCreate={onCreate} />

      {users.status === "loading" ? <LoadingState label="Loading accounts" /> : null}
      {users.status === "disconnected" ? <DisconnectedState /> : null}
      {users.status === "error" && users.error ? (
        <ErrorState error={users.error} onRetry={users.refresh} />
      ) : null}

      {users.status === "ready" && users.data ? (
        users.data.items.length === 0 ? (
          <EmptyState title="No accounts" description="This should not happen: an installation always has at least its first administrator." />
        ) : (
          <AccountTable
            users={users.data.items}
            currentUserId={session.user?.userId ?? ""}
            onChangeRole={onChangeRole}
            onChangeStatus={onChangeStatus}
            onReset={onReset}
          />
        )
      ) : null}
    </div>
  );
}

/** The account list. */
function AccountTable({
  users,
  currentUserId,
  onChangeRole,
  onChangeStatus,
  onReset,
}: {
  users: PublicUser[];
  currentUserId: string;
  onChangeRole: (user: PublicUser, role: Role) => void;
  onChangeStatus: (user: PublicUser, status: UserStatus) => void;
  onReset: (user: PublicUser) => void;
}) {
  return (
    <div
      className="overflow-x-auto rounded-xl border border-border-subtle bg-surface-raised"
      tabIndex={0}
    >
      <table className="w-full text-left text-sm">
        <thead className="border-b border-border-subtle text-xs uppercase tracking-wide text-content-muted">
          <tr>
            <th scope="col" className="px-4 py-3">Username</th>
            <th scope="col" className="px-4 py-3">Role</th>
            <th scope="col" className="px-4 py-3">Status</th>
            <th scope="col" className="px-4 py-3">Last sign-in</th>
            <th scope="col" className="px-4 py-3">Actions</th>
          </tr>
        </thead>
        <tbody>
          {users.map((user) => {
            const isSelf = user.userId === currentUserId;
            return (
              <tr key={user.userId} className="border-b border-border-subtle last:border-0">
                <th scope="row" className="px-4 py-3 font-medium">
                  {user.username}
                  {isSelf ? (
                    <span className="ml-2 rounded bg-accent-soft px-1.5 py-0.5 text-xs text-accent">
                      you
                    </span>
                  ) : null}
                  {user.mustChangePassword ? (
                    <span className="ml-2 rounded bg-warn-soft px-1.5 py-0.5 text-xs text-warn">
                      must change password
                    </span>
                  ) : null}
                </th>

                <td className="px-4 py-3">
                  {isSelf ? (
                    <span className="text-content-muted">{user.role}</span>
                  ) : (
                    <select
                      aria-label={`Role for ${user.username}`}
                      value={user.role}
                      onChange={(event) =>
                        onChangeRole(user, event.target.value as Role)
                      }
                      className="rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
                    >
                      <option value="viewer">viewer</option>
                      <option value="operator">operator</option>
                      <option value="administrator">administrator</option>
                    </select>
                  )}
                </td>

                <td className="px-4 py-3">
                  <span
                    className={
                      user.status === "active"
                        ? "rounded bg-ok-soft px-1.5 py-0.5 text-xs text-ok"
                        : "rounded bg-surface-sunken px-1.5 py-0.5 text-xs text-content-muted"
                    }
                  >
                    {user.status}
                  </span>
                </td>

                <td className="px-4 py-3 text-content-muted">
                  {user.lastLoginAt ? formatWhen(user.lastLoginAt) : "never"}
                </td>

                <td className="px-4 py-3">
                  {isSelf ? (
                    <span className="text-xs text-content-muted">
                      change your own account from Settings
                    </span>
                  ) : (
                    <div className="flex flex-wrap gap-2">
                      <button
                        type="button"
                        onClick={() =>
                          onChangeStatus(
                            user,
                            user.status === "active" ? "disabled" : "active",
                          )
                        }
                        className="rounded-lg border border-border-subtle px-2 py-1 text-xs font-medium"
                      >
                        {user.status === "active" ? "Disable" : "Enable"}
                      </button>
                      <button
                        type="button"
                        onClick={() => onReset(user)}
                        className="rounded-lg border border-border-subtle px-2 py-1 text-xs font-medium"
                      >
                        Reset password
                      </button>
                    </div>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

/** The create-account form. */
function CreateAccount({
  onCreate,
}: {
  onCreate: (username: string, role: Role) => void;
}) {
  const [username, setUsername] = useState("");
  const [role, setRole] = useState<Role>("viewer");

  return (
    <form
      onSubmit={(event) => {
        event.preventDefault();
        onCreate(username.trim(), role);
        setUsername("");
      }}
      className="flex flex-wrap items-end gap-3 rounded-xl border border-border-subtle bg-surface-raised p-4"
    >
      {/*
        `min-w-0` on the field wrappers, `w-full` on the controls.

        A flex item defaults to `min-width: auto`, so it refuses to shrink below
        its content. The role options are whole sentences, which made the select
        as wide as its longest one and pushed the page sideways on a narrow
        screen. With the floor removed the controls fill whatever the row can
        give them and the page never scrolls horizontally.
      */}
      <div className="flex min-w-0 flex-col gap-1">
        <label htmlFor="new-username" className="text-sm font-medium">
          New account
        </label>
        <input
          id="new-username"
          value={username}
          spellCheck={false}
          onChange={(event) => setUsername(event.target.value)}
          placeholder="username"
          className="w-full rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm"
        />
      </div>

      <div className="flex min-w-0 flex-col gap-1">
        <label htmlFor="new-role" className="text-sm font-medium">
          Role
        </label>
        <select
          id="new-role"
          value={role}
          onChange={(event) => setRole(event.target.value as Role)}
          className="w-full rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm"
        >
          <option value="viewer">viewer — read only</option>
          <option value="operator">operator — can act on the estate</option>
          <option value="administrator">administrator — can also manage accounts and policies</option>
        </select>
      </div>

      <button
        type="submit"
        disabled={username.trim() === ""}
        className="rounded-lg bg-accent px-4 py-2 text-sm font-semibold text-white disabled:opacity-50"
      >
        Create
      </button>

      <p className="w-full text-xs text-content-muted">
        HarborMaster generates the password and shows it once. The account must
        replace it at first sign-in.
      </p>
    </form>
  );
}

/**
 * A secret shown exactly once.
 *
 * Not a toast. The value is unrecoverable, so it stays until the operator
 * dismisses it deliberately.
 */
function OneTimeSecret({
  title,
  secret,
  onDismiss,
}: {
  title: string;
  secret: string;
  onDismiss: () => void;
}) {
  return (
    <div
      role="alert"
      className="flex flex-col gap-2 rounded-xl border border-warn/40 bg-warn-soft p-4"
    >
      <h3 className="text-sm font-semibold">{title}</h3>
      <p className="text-sm text-content-muted">
        This password is shown once and stored nowhere. Copy it now.
      </p>
      <code className="rounded-lg bg-surface px-3 py-2 font-mono text-sm break-all">
        {secret}
      </code>
      <button
        type="button"
        onClick={onDismiss}
        className="self-start rounded-lg border border-border-subtle px-3 py-1.5 text-sm font-medium"
      >
        I have copied it
      </button>
    </div>
  );
}

/** Shown when the role does not hold `user:manage`. */
export function NotPermitted() {
  return (
    <EmptyState
      title="Not permitted"
      description="Your role does not include this. Ask an administrator if you need it — HarborMaster enforces this on the server, so nothing here would work anyway."
    />
  );
}

/** Renders an API failure without repeating a server detail. */
function describe(error: unknown, fallback: string): string {
  if (error instanceof ApiError) {
    if (error.isConnectivity) return "Cannot reach HarborMaster.";
    if (error.status === 403) return "Your role does not permit this.";
    // A 400 or 409 message is written for an operator: the last-administrator
    // guard, the username allowlist, the password policy.
    if (error.status >= 400 && error.status < 500) return error.message;
  }
  return fallback;
}

/** Formats an ISO timestamp for display, tolerating an unparseable one. */
function formatWhen(iso: string): string {
  const when = new Date(iso);
  return Number.isNaN(when.getTime()) ? iso : when.toLocaleString();
}
