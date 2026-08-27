import { Link } from "react-router";

import type { Permission } from "../api/authTypes";
import { EmptyState } from "./States";
import { PageIntro } from "./PageIntro";
import { useSession } from "../hooks/useSession";

export interface SectionLink {
  label: string;
  description: string;
  path: string;
  /** The permission the destination itself requires. */
  permission?: Permission;
}

/**
 * A section landing: a short list of the tools that section covers.
 *
 * # What this is, and what it deliberately is not
 *
 * It is a signpost. It renders links and nothing else -- no counts, no
 * summaries, no data of its own. That matters because the alternative is a
 * second, thinner implementation of each page it points at, which would then
 * disagree with the real one.
 *
 * These are transitional. The intended end state consolidates each section into
 * a real screen; until then a landing is honest about being a menu, which is
 * better than a sidebar that lists every stage of the update lifecycle.
 *
 * Links are filtered by the SAME permission the route guards on, so a section
 * never offers a page that would refuse the operator.
 */
export function SectionLanding({
  title,
  description,
  links,
}: {
  title: string;
  description: string;
  links: readonly SectionLink[];
}) {
  const session = useSession();
  const permitted = links.filter(
    (link) => !link.permission || session.can(link.permission),
  );

  return (
    <div className="flex flex-col gap-6">
      <PageIntro title={title} description={description} />

      {permitted.length === 0 ? (
        <EmptyState
          title="Nothing in this section is available to your role"
          description="These tools exist, but your role does not include the permissions they need. Ask an administrator if you need them."
        />
      ) : (
        <ul className="grid gap-3 sm:grid-cols-2 xl:grid-cols-3">
          {permitted.map((link) => (
            <li key={link.path}>
              <Link
                to={link.path}
                className="flex h-full min-h-11 flex-col gap-1 rounded-xl border border-border-subtle bg-surface-raised p-4 transition-colors hover:border-accent"
              >
                <span className="text-sm font-semibold text-content">
                  {link.label}
                </span>
                <span className="text-sm text-content-muted">
                  {link.description}
                </span>
              </Link>
            </li>
          ))}
        </ul>
      )}
    </div>
  );
}
