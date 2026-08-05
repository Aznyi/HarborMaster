import { useEffect, useState } from "react";
import { NavLink, useLocation } from "react-router";
import type { ReactNode } from "react";

import type { ResourceState } from "../hooks/useApiResource";
import type { HealthReport } from "../api/types";
import { ConnectivityIndicator } from "./ConnectivityIndicator";

export interface NavItem {
  label: string;
  path: string;
}

/** Primary navigation. Order is the operator's expected workflow. */
export const NAV_ITEMS: readonly NavItem[] = [
  { label: "Dashboard", path: "/" },
  { label: "Containers", path: "/containers" },
  { label: "Images", path: "/images" },
  { label: "Updates", path: "/images/updates" },
  { label: "Snapshots", path: "/snapshots" },
  { label: "Drift", path: "/drift" },
  { label: "Compliance", path: "/compliance" },
  { label: "Policies", path: "/policies" },
  { label: "Events", path: "/events" },
  { label: "Settings", path: "/settings" },
] as const;

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
        <Sidebar open={drawerOpen} onClose={() => setDrawerOpen(false)} />

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
          </header>

          <main
            id="main-content"
            className="mx-auto w-full max-w-6xl flex-1 px-4 py-6 sm:px-6 sm:py-8"
          >
            {children}
          </main>

          <footer className="border-t border-border-subtle px-4 py-4 text-xs text-content-muted sm:px-6">
            HarborMaster &mdash; read-only inventory and snapshot foundation. No
            container is created, changed, or removed by this build.
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

function Sidebar({ open, onClose }: { open: boolean; onClose: () => void }) {
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
            {NAV_ITEMS.map((item) => (
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
