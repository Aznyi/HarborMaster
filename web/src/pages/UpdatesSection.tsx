import { SectionLanding, type SectionLink } from "../components/SectionLanding";

/**
 * The Updates section.
 *
 * Transitional: it lists the stages of an update rather than consolidating
 * them, because consolidating them is a later phase. What it buys now is that
 * the sidebar names one thing an operator wants ("Updates") instead of five
 * things HarborMaster does internally.
 */
const LINKS: readonly SectionLink[] = [
  {
    label: "Available updates",
    description: "What each running image could move to, and whether the registry answered.",
    path: "/images/updates",
    permission: "inventory:read",
  },
  {
    label: "Update reviews",
    description: "HarborMaster's verdict on each proposed change, and the approval for one that needs a person.",
    path: "/plans",
    permission: "plan:read",
  },
  {
    label: "Image downloads",
    description: "Images being pulled, and the digest each one was pinned to.",
    path: "/acquisitions",
    permission: "acquisition:read",
  },
  {
    label: "Update history",
    description: "Every container recreation HarborMaster performed, and how it ended.",
    path: "/executions",
    permission: "execution:read",
  },
  {
    label: "Rollbacks",
    description: "Updates that failed verification and the containers restored afterwards.",
    path: "/rollbacks",
    permission: "rollback:read",
  },
];

export function UpdatesSection() {
  return (
    <SectionLanding
      title="Updates"
      description="Everything about moving a container from one image to another: what is available, what HarborMaster thinks of it, and what has already happened."
      links={LINKS}
    />
  );
}
