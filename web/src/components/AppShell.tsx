import { useEffect, useState } from "react";
import { NavLink, useLocation } from "react-router";
import type { ReactNode } from "react";

import type { ResourceState } from "../hooks/useApiResource";
import type { HealthReport } from "../api/types";
import type { Permission, PublicUser } from "../api/authTypes";
import { useSession } from "../hooks/useSession";
import { ConnectivityIndicator } from "./ConnectivityIndicator";

export interface NavItem {
  label: string;
  path: string;
  /**
   * The permission a role must hold for this destination to be listed.
   *
   * Omitted for the pages every signed-in account can reach: the dashboard,
   * their own account, and settings.
   *
   * # Hiding a link is NOT the access control
   *
   * The server refuses the request regardless of what is rendered here, and an
   * operator who types the URL gets a 403 from the API rather than a page. This
   * exists so the navigation reflects what the account can actually do, which
   * is a usability property -- offering a link that always fails is a bad
   * interface, not an insecure one.
   */
  permission?: Permission;
}

/** Primary navigation. Order is the operator's expected workflow. */
export const NAV_ITEMS: readonly NavItem[] = [
  { label: "Dashboard", path: "/" },
  { label: "Containers", path: "/containers", permission: "inventory:read" },
  { label: "Images", path: "/images", permission: "inventory:read" },
  { label: "Updates", path: "/images/updates", permission: "inventory:read" },
  { label: "Snapshots", path: "/snapshots", permission: "snapshot:read" },
  { label: "Drift", path: "/drift", permission: "drift:read" },
  { label: "Change plans", path: "/plans", permission: "plan:read" },
  { label: "Acquisitions", path: "/acquisitions", permission: "acquisition:read" },
  { label: "Recreations", path: "/executions", permission: "execution:read" },
  { label: "Compliance", path: "/compliance", permission: "policy:read" },
  { label: "Policies", path: "/policies", permission: "policy:read" },
  { label: "Events", path: "/events", permission: "event:read" },
  { label: "Accounts", path: "/users", permission: "user:manage" },
  { label: "Security audit", path: "/audit", permission: "audit:read" },
  { label: "Your account", path: "/account" },
  { label: "Settings", path: "/settings" },
] as const;

/** The destinations one account may see. */
export function visibleNavItems(user: PublicUser | null): readonly NavItem[] {
  if (!user) return [];
  return NAV_ITEMS.filter(
    (item) => !item.permission || user.permissions.includes(item.permission),
  );
}

/**
 * The responsive application shell.
 *
 * The sidebar is permanent from the `lg` breakpoint up and collapses to a
 * toggled drawer below it, so the same markup serves phone and desktop.
 */
export function AppShell({
  health,
  children,
}: {
  health: ResourceState<HealthReport>;
  children: ReactNode;
}) {
  const [drawerOpen, setDrawerOpen] = useState(false);
  const location = useLocation();
  const session = useSession();
  const items = visibleNavItems(session.user);

  // Navigating away must close the drawer, or the overlay traps the new page.
  useEffect(() => setDrawerOpen(false), [location.pathname]);

  return (
    <div className="min-h-full bg-surface text-content">
      <a
        href="#main-content"
        className="sr-only focus:not-sr-only focus:absolute focus:left-4 focus:top-4 focus:z-50 focus:rounded-lg focus:bg-surface-raised focus:px-4 focus:py-2"
      >
        Skip to content
      </a>

      <div className="lg:grid lg:grid-cols-[16rem_1fr]">
        <Sidebar
          open={drawerOpen}
          onClose={() => setDrawerOpen(false)}
          items={items}
        />

        <div className="flex min-h-screen flex-col">
          <header className="sticky top-0 z-20 flex flex-wrap items-center gap-3 border-b border-border-subtle bg-surface-raised/95 px-4 py-3 backdrop-blur sm:px-6">
            <button
              type="button"
              onClick={() => setDrawerOpen((open) => !open)}
              aria-expanded={drawerOpen}
              aria-controls="primary-navigation"
              className="rounded-lg border border-border-subtle px-3 py-1.5 text-sm font-medium lg:hidden"
            >
              Menu
            </button>

            <h1 className="mr-auto text-sm font-semibold tracking-tight sm:text-base">
              {pageTitle(location.pathname)}
            </h1>

            <ConnectivityIndicator health={health} />

            <div className="flex items-center gap-2 text-sm">
              <span className="hidden text-content-muted sm:inline">
                {session.user?.username}
              </span>
              {session.user ? (
                <span className="rounded bg-accent-soft px-1.5 py-0.5 text-xs text-accent">
                  {session.user.role}
                </span>
              ) : null}
              <button
                type="button"
                onClick={() => void session.signOut()}
                className="rounded-lg border border-border-subtle px-3 py-1.5 text-sm font-medium"
              >
                Sign out
              </button>
            </div>
          </header>

          <main
            id="main-content"
            className="mx-auto w-full max-w-6xl flex-1 px-4 py-6 sm:px-6 sm:py-8"
          >
            {children}
          </main>

          <footer className="border-t border-border-subtle px-4 py-4 text-xs text-content-muted sm:px-6">
            HarborMaster &mdash; every action is authenticated and recorded
            against the account that performed it.
          </footer>
        </div>
      </div>
    </div>
  );
}

/**
 * Titles the header. Nested routes such as /containers/<id> keep their parent's
 * title rather than falling back to the product name, which would read as
 * though the operator had navigated away from the section.
 */
function pageTitle(pathname: string): string {
  const exact = NAV_ITEMS.find((item) => item.path === pathname);
  if (exact) return exact.label;

  const parent = NAV_ITEMS.find(
    (item) => item.path !== "/" && pathname.startsWith(`${item.path}/`),
  );
  return parent?.label ?? "HarborMaster";
}

function Sidebar({
  open,
  onClose,
  items,
}: {
  open: boolean;
  onClose: () => void;
  items: readonly NavItem[];
}) {
  return (
    <>
      {open ? (
        <button
          type="button"
          aria-label="Close navigation"
          onClick={onClose}
          className="fixed inset-0 z-30 bg-black/40 lg:hidden"
        />
      ) : null}

      <aside
        id="primary-navigation"
        className={`fixed inset-y-0 left-0 z-40 w-64 border-r border-border-subtle bg-surface-raised transition-transform lg:sticky lg:top-0 lg:z-0 lg:h-screen lg:translate-x-0 ${
          open ? "translate-x-0" : "-translate-x-full"
        }`}
      >
        <div className="flex h-full flex-col gap-6 p-4">
          <div className="flex items-center gap-2 px-2 py-1">
            <span
              aria-hidden="true"
              className="grid size-8 place-items-center rounded-lg bg-accent-soft text-sm font-bold text-accent"
            >
              HM
            </span>
            <span className="text-sm font-semibold tracking-tight">
              HarborMaster
            </span>
          </div>

          <nav aria-label="Primary" className="flex flex-col gap-1">
            {items.map((item) => (
              <NavLink
                key={item.path}
                to={item.path}
                end={item.path === "/"}
                className={({ isActive }) =>
                  `rounded-lg px-3 py-2 text-sm font-medium transition-colors ${
                    isActive
                      ? "bg-accent-soft text-accent"
                      : "text-content-muted hover:bg-surface-sunken hover:text-content"
                  }`
                }
              >
                {item.label}
              </NavLink>
            ))}
          </nav>
        </div>
      </aside>
    </>
  );
}
