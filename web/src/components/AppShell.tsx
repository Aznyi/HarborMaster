import { useEffect, useState } from "react";
import { NavLink, useLocation } from "react-router";
import type { ReactNode } from "react";

import type { ResourceState } from "../hooks/useApiResource";
import type { HealthReport } from "../api/types";
import type { Permission, PublicUser } from "../api/authTypes";
import { useAdvancedTools } from "../hooks/useAdvancedTools";
import { useSession } from "../hooks/useSession";
import { AccountMenu } from "./AccountMenu";
import { SystemStatus } from "./SystemStatus";

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

/**
 * Primary navigation: the six things a homelab operator actually does.
 *
 * # Why this is six and not twenty-two
 *
 * The sidebar used to list every stage of HarborMaster's update lifecycle --
 * plans, acquisitions, executions, rollbacks, drift, snapshots -- as separate
 * destinations. That is an accurate map of the ENGINE and a poor map of the
 * job. Somebody who wants their containers kept up to date should not have to
 * learn the difference between an acquisition and an execution to find out
 * whether anything happened.
 *
 * Nothing was removed. Every one of those pages is still routable by URL and
 * still listed under ADVANCED_NAV; what changed is which of them the default
 * sidebar puts in front of somebody who has not asked for them.
 */
export const PRIMARY_NAV: readonly NavItem[] = [
  { label: "Dashboard", path: "/" },
  { label: "Containers", path: "/containers", permission: "inventory:read" },
  // The section landings gate on the permission their FIRST destination needs,
  // so the sidebar never offers a section whose contents are all refused.
  { label: "Updates", path: "/updates", permission: "plan:read" },
  { label: "Automation", path: "/automation", permission: "automation:read" },
  { label: "Activity", path: "/activity", permission: "execution:read" },
  { label: "Settings", path: "/settings" },
] as const;

/**
 * The specialised tools, shown only when an operator asks for them.
 *
 * Every entry keeps the permission it has always had. This list is about
 * DENSITY, not access: the routes are unchanged, the guards are unchanged, and
 * a bookmark to any of them still works whether or not this section is shown.
 *
 * # Why some labels moved and the routes did not
 *
 * Phase 6 settled the vocabulary an operator reads. Three entries were named
 * after the record HarborMaster stores rather than the thing it holds --
 * Snapshots, Drift, Update dependencies -- and now read as Restore points,
 * Configuration changes and Update order, matching what the container page has
 * called them since Phase 5.
 *
 * The URLs are unchanged. `/snapshots` and `/drift` are correct names for what
 * those pages contain, they are the terms the API and the documentation use,
 * and changing them would break bookmarks to buy nothing. Two entries gained a
 * qualifier instead of a rename: "Compliance policies" and "Docker events" were
 * ambiguous next to update policies and lifecycle activity.
 *
 * Order is by subject: container diagnostics, then the update lifecycle in the
 * order its records are created, then automation, then administration.
 */
export const ADVANCED_NAV: readonly NavItem[] = [
  // Container diagnostics.
  { label: "Images", path: "/images", permission: "inventory:read" },
  { label: "Available updates", path: "/images/updates", permission: "inventory:read" },
  { label: "Restore points", path: "/snapshots", permission: "snapshot:read" },
  { label: "Configuration changes", path: "/drift", permission: "drift:read" },

  // Update internals: the lifecycle records, in the order they are created.
  { label: "Update reviews", path: "/plans", permission: "plan:read" },
  { label: "Image downloads", path: "/acquisitions", permission: "acquisition:read" },
  { label: "Update history", path: "/executions", permission: "execution:read" },
  { label: "Rollbacks", path: "/rollbacks", permission: "rollback:read" },

  // Automation internals.
  { label: "Update policies", path: "/update-policies", permission: "automation:read" },
  { label: "Update order", path: "/dependencies", permission: "dependency:read" },
  { label: "Paused containers", path: "/automation/paused", permission: "automation:read" },

  // Administration and security.
  { label: "Compliance", path: "/compliance", permission: "policy:read" },
  { label: "Compliance policies", path: "/policies", permission: "policy:read" },
  { label: "Notifications", path: "/notifications", permission: "notification:read" },
  { label: "Docker events", path: "/events", permission: "event:read" },
  { label: "Accounts", path: "/users", permission: "user:manage" },
  { label: "Security audit", path: "/audit", permission: "audit:read" },
] as const;

/**
 * Every destination the sidebar can name, primary or advanced.
 *
 * `Your account` is deliberately absent from both lists: it moved into the
 * header's account menu, which is where somebody looks for it.
 */
export const NAV_ITEMS: readonly NavItem[] = [
  ...PRIMARY_NAV,
  ...ADVANCED_NAV,
  { label: "Your account", path: "/account" },
] as const;

/**
 * The destinations one account may see, from any list.
 *
 * Filtering is a usability property, not the access control: the server refuses
 * the request regardless, and typing the URL gets a 403 rather than a page.
 */
export function visibleNavItems(
  user: PublicUser | null,
  items: readonly NavItem[] = NAV_ITEMS,
): readonly NavItem[] {
  if (!user) return [];
  return items.filter(
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
  const advancedEnabled = useAdvancedTools();

  const primary = visibleNavItems(session.user, PRIMARY_NAV);
  // Computed even when hidden, so the section can be skipped entirely when an
  // account holds none of the permissions behind it.
  const advanced = visibleNavItems(session.user, ADVANCED_NAV);

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
          primary={primary}
          advanced={advancedEnabled ? advanced : []}
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

            <SystemStatus health={health} />
            <AccountMenu />
          </header>

          <main
            id="main-content"
            // Wider than the old max-w-6xl (72rem). HarborMaster's pages are
            // mostly tables of containers, digests and timestamps, and 1152px
            // left a third of a desktop display empty while those tables
            // wrapped. Individual components still constrain their own prose
            // with max-w-prose, so text does not become a 1376px line.
            className="mx-auto w-full max-w-[86rem] flex-1 px-4 py-6 sm:px-6 sm:py-8"
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
  primary,
  advanced,
}: {
  open: boolean;
  onClose: () => void;
  primary: readonly NavItem[];
  advanced: readonly NavItem[];
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
        className={`fixed inset-y-0 left-0 z-40 w-64 overflow-y-auto border-r border-border-subtle bg-surface-raised transition-transform lg:sticky lg:top-0 lg:z-0 lg:h-screen lg:translate-x-0 ${
          open ? "translate-x-0" : "-translate-x-full"
        }`}
      >
        <div className="flex min-h-full flex-col gap-6 p-4">
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

          {/* One landmark for both groups. Two navigation landmarks in a
              sidebar makes a screen reader announce a section boundary that
              is not one; the heading below does that job instead. */}
          <nav aria-label="Primary" className="flex flex-col gap-6">
            <div className="flex flex-col gap-1">
              {primary.map((item) => (
                <SidebarLink key={item.path} item={item} />
              ))}
            </div>

            {advanced.length > 0 ? (
              <div className="flex flex-col gap-1">
                <h2
                  id="advanced-tools-heading"
                  className="px-3 pb-1 text-xs font-semibold uppercase tracking-wide text-content-muted"
                >
                  Advanced
                </h2>
                <div
                  aria-labelledby="advanced-tools-heading"
                  role="group"
                  className="flex flex-col gap-1"
                >
                  {advanced.map((item) => (
                    <SidebarLink key={item.path} item={item} />
                  ))}
                </div>
              </div>
            ) : null}
          </nav>
        </div>
      </aside>
    </>
  );
}

function SidebarLink({ item }: { item: NavItem }) {
  return (
    <NavLink
      to={item.path}
      // `end` on the dashboard alone, as before: every other destination wants
      // its nested detail pages to keep the section highlighted.
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
  );
}
