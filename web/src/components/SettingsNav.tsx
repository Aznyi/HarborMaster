import { useEffect } from "react";
import { useLocation } from "react-router";

/**
 * In-page navigation for Settings.
 *
 * # Why anchors and not a router
 *
 * Settings is ONE page and one route. It is long because it reports a lot of
 * true things, not because it contains several subjects that should have been
 * separate destinations — splitting it would put the deployment's capabilities
 * behind a menu, which is exactly what Phase 6 stopped doing. So this is a
 * table of contents, not navigation: plain `<a href="#id">` elements pointing
 * at headings on the same page.
 *
 * That choice does the work for us. The browser scrolls, updates the address
 * bar, and adds a history entry, so Back returns to where the operator was.
 * There is no scroll library, no dependency, and no second router.
 *
 * # Not sticky, deliberately
 *
 * The application header is already sticky. A second bar pinned under it would
 * cost vertical space on every scroll of every Settings visit — on a 390px
 * phone that is a real fraction of the viewport — to save a scroll-to-top that
 * the operator performs once. It sits in the normal flow until there is
 * evidence that it should not.
 */

/** One entry, and the heading it points at. */
interface SettingsSectionLink {
  /** The id of an existing heading on this page. */
  id: string;
  /** Short enough to sit in a row of chips. */
  label: string;
}

/**
 * The sections, in the order the page renders them.
 *
 * Every id here belongs to a heading that already exists; nothing was invented
 * to fill out a category, and no section was added to give an entry something
 * to point at. `settings-host` is intentionally absent: it renders immediately
 * below `settings-capabilities` and reads as the second half of one subject, so
 * a chip for it would be a second name for a place the first chip already
 * reaches.
 *
 * `settings-capabilities` is on the heading of whichever capability section the
 * page rendered — the real one, or the "Features" stand-in shown when the
 * deployment reported nothing — so the chip resolves either way.
 */
export const SETTINGS_SECTIONS: readonly SettingsSectionLink[] = [
  { id: "settings-status", label: "Status" },
  { id: "settings-capabilities", label: "Capabilities" },
  { id: "settings-notifications", label: "Notifications" },
  { id: "settings-build", label: "Build" },
  { id: "settings-configuration", label: "Configuration" },
  { id: "settings-access", label: "Access" },
  { id: "settings-security", label: "Security" },
  { id: "settings-advanced", label: "Advanced" },
] as const;

/**
 * The sections that exist for THIS render.
 *
 * A deployment that did not report its features renders a stand-in in place of
 * the capability sections, and the Notifications section is not rendered at
 * all. A table of contents naming a section that is not on the page is worse
 * than a shorter one: the link does nothing, and the operator is left looking
 * for something that was never there.
 */
export function settingsSectionsFor(hasFeatures: boolean): readonly SettingsSectionLink[] {
  if (hasFeatures) return SETTINGS_SECTIONS;
  return SETTINGS_SECTIONS.filter((section) => section.id !== "settings-notifications");
}

export function SettingsNav({ hasFeatures = true }: { hasFeatures?: boolean }) {
  const sections = settingsSectionsFor(hasFeatures);

  return (
    <nav
      aria-label="Settings sections"
      className="rounded-xl border border-border-subtle bg-surface-raised p-3"
    >
      <ul className="flex flex-wrap gap-2">
        {sections.map((section) => (
          <li key={section.id}>
            <a
              href={`#${section.id}`}
              className="inline-flex min-h-11 items-center rounded-lg border border-border-subtle px-3 py-1.5 text-sm font-medium text-content-muted transition-colors hover:border-accent hover:text-content focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent"
            >
              {section.label}
            </a>
          </li>
        ))}
      </ul>
    </nav>
  );
}

/**
 * Resolves a `#section` in the address bar to the heading it names.
 *
 * # Why this is needed at all, when the browser already does it
 *
 * It does — for a click, where the target already exists. It cannot for a
 * DIRECT visit to `/settings#settings-access`: the browser looks for the
 * element while the document is still the empty SPA shell, finds nothing, and
 * gives up before React has rendered anything. Without this, a shared or
 * bookmarked section link silently lands at the top of the page.
 *
 * So this runs once after the page mounts, for the hash the page was OPENED
 * with. Clicks are still handled natively and are not intercepted.
 *
 * Everything here is defensive on purpose. The hash is attacker-supplied text
 * from the address bar, so it is looked up with `getElementById` — never used
 * to build a selector, which is where a `#` value becomes an injection — and
 * `scrollIntoView` is feature-checked because jsdom does not implement it.
 */
export function useSettingsHashTarget() {
  const { hash } = useLocation();

  useEffect(() => {
    const id = hash.replace(/^#/, "");
    if (!id) return;

    // After paint, so the section exists to scroll to.
    const frame = requestAnimationFrame(() => {
      const target = document.getElementById(id);
      if (!target) return;
      if (typeof target.scrollIntoView === "function") {
        target.scrollIntoView();
      }
      // Moves the keyboard, not only the viewport. The headings carry
      // tabIndex={-1} so this lands somewhere focusable.
      target.focus?.({ preventScroll: true });
    });

    return () => cancelAnimationFrame(frame);
    // Mount and hash changes only. Re-running on every render would yank the
    // page back to the section every time health polling refreshes the page.
  }, [hash]);
}
