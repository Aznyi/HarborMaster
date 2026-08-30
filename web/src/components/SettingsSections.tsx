import { Link } from "react-router";

import { useSession } from "../hooks/useSession";
import type { Permission } from "../api/authTypes";

/**
 * Settings as a control centre.
 *
 * # What this is, and what it deliberately is not
 *
 * Settings was the last normal destination that had not been through the
 * consolidation. It reported what the running process can do -- accurately, and
 * worth keeping -- but it named no route to the things an operator goes to
 * Settings looking for: who can sign in, where notifications are configured,
 * where the audit lives.
 *
 * So this adds signposts, not copies. Every destination below is an existing
 * page with its own permission, and none of its content is reproduced here.
 * Duplicating the account editor or the notification form into Settings would
 * create a second place to change one thing.
 *
 * # Permission filtering
 *
 * Each link is filtered by the permission its page already requires, so
 * Settings never offers a destination that would refuse the operator. That is a
 * usability property; the server refuses regardless.
 */

interface Destination {
  label: string;
  description: string;
  to: string;
  permission?: Permission;
}

function DestinationList({
  destinations,
}: {
  destinations: readonly Destination[];
}) {
  const session = useSession();
  const permitted = destinations.filter(
    (entry) => !entry.permission || session.can(entry.permission),
  );

  if (permitted.length === 0) {
    return (
      <p className="mt-3 text-sm text-content-muted">
        Your role does not include access to these.
      </p>
    );
  }

  return (
    <ul className="mt-3 grid gap-3 sm:grid-cols-2">
      {permitted.map((entry) => (
        <li key={entry.to} className="min-w-0">
          <Link
            to={entry.to}
            className="flex h-full min-h-11 flex-col gap-1 rounded-lg border border-border-subtle bg-surface p-3 transition-colors hover:border-accent"
          >
            <span className="text-sm font-medium text-content">{entry.label}</span>
            <span className="text-sm text-content-muted">{entry.description}</span>
          </Link>
        </li>
      ))}
    </ul>
  );
}

/**
 * The shared class for a linkable Settings heading.
 *
 * `scroll-mt-24` clears the sticky application header: without it the browser
 * scrolls the heading to y=0, which is underneath the header, and the section
 * an operator asked for arrives already hidden.
 *
 * `tabIndex={-1}` at each use site makes the heading a focus target so that
 * following an in-page link moves the KEYBOARD to the section, not just the
 * viewport -- otherwise the next Tab continues from wherever focus already was,
 * which for a screen-reader user means the link did nothing at all.
 */
export const SECTION_HEADING =
  "scroll-mt-24 font-semibold focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent focus-visible:ring-offset-2 focus-visible:ring-offset-surface-raised";

function Section({
  id,
  title,
  description,
  children,
}: {
  id: string;
  title: string;
  description?: string;
  children: React.ReactNode;
}) {
  return (
    <section
      aria-labelledby={id}
      data-testid={id}
      className="rounded-xl border border-border-subtle bg-surface-raised p-5"
    >
      <h3 id={id} tabIndex={-1} className={SECTION_HEADING}>
        {title}
      </h3>
      {description ? (
        <p className="mt-1 max-w-prose text-sm text-content-muted">{description}</p>
      ) : null}
      {children}
    </section>
  );
}

/**
 * The way to the notification configuration.
 *
 * Rendered inside the section that already reports whether delivery is on, so
 * one subject has one heading. Settings says where; the notifications page owns
 * the destinations and rules.
 */
export function NotificationDestinations() {
  return (
    <DestinationList
      destinations={[
        {
          label: "Configure notifications",
          description:
            "Where word is sent when an update fails or needs review, and what has been sent.",
          to: "/notifications",
          permission: "notification:read",
        },
      ]}
    />
  );
}

/** Who can sign in, and where their own account lives. */
export function AccessSettings() {
  return (
    <Section
      id="settings-access"
      title="Access"
      description="Every action is recorded against the account that performed it. Roles are fixed permission sets; the smallest one that lets somebody do their job is the right one."
    >
      <DestinationList
        destinations={[
          {
            label: "Manage accounts",
            description: "Create accounts, set roles, and recover access.",
            to: "/users",
            permission: "user:manage",
          },
          {
            label: "Your account",
            description: "Your own password and active sessions.",
            to: "/account",
          },
        ]}
      />
    </Section>
  );
}

/** The record of what was done, and the rules a container must satisfy. */
export function SecuritySettings() {
  return (
    <Section
      id="settings-security"
      title="Security"
      description="What HarborMaster recorded, and the compliance rules it measures containers against."
    >
      <DestinationList
        destinations={[
          {
            label: "Security audit",
            description:
              "Every state-changing action and the account behind it, including the refused ones.",
            to: "/audit",
            permission: "audit:read",
          },
          {
            label: "Compliance policies",
            description: "The rules containers are measured against.",
            to: "/policies",
            permission: "policy:read",
          },
          {
            label: "Compliance findings",
            description: "Containers that do not currently satisfy those rules.",
            to: "/compliance",
            permission: "policy:read",
          },
        ]}
      />
    </Section>
  );
}
