import { Link } from "react-router";

import { useSession } from "../hooks/useSession";

/**
 * The account control in the header.
 *
 * Collects what used to be three separate header elements -- a username, a role
 * chip and a Sign out button -- behind one control, and gives "Your account" a
 * home now that it has left the sidebar.
 *
 * Signing out stays a real button rather than a link: it is a state-changing
 * action, and the session hook revokes the session server-side.
 */
export function AccountMenu() {
  const session = useSession();
  const user = session.user;
  if (!user) return null;

  return (
    <details className="relative" data-testid="account-menu">
      <summary
        className="flex min-h-11 cursor-pointer list-none items-center gap-2 rounded-lg border border-border-subtle px-3 py-1.5 text-sm font-medium"
        aria-label={`Account: ${user.username}`}
      >
        <span
          aria-hidden="true"
          className="grid size-6 shrink-0 place-items-center rounded-full bg-accent-soft text-xs font-bold text-accent"
        >
          {user.username.slice(0, 1).toUpperCase()}
        </span>
        <span className="hidden sm:inline">{user.username}</span>
      </summary>

      <div className="absolute right-0 z-30 mt-1 w-56 rounded-lg border border-border-subtle bg-surface-raised p-2 shadow-lg">
        <div className="px-2 py-1.5">
          <p className="text-sm font-medium">{user.username}</p>
          <p className="text-xs text-content-muted">{user.role}</p>
        </div>

        <Link
          to="/account"
          className="block rounded-lg px-2 py-2 text-sm text-content-muted hover:bg-surface-sunken hover:text-content"
        >
          Your account
        </Link>

        <button
          type="button"
          onClick={() => void session.signOut()}
          className="mt-1 block w-full rounded-lg border border-border-subtle px-2 py-2 text-left text-sm font-medium"
        >
          Sign out
        </button>
      </div>
    </details>
  );
}
