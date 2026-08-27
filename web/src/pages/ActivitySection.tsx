import { SectionLanding, type SectionLink } from "../components/SectionLanding";

/**
 * The Activity section.
 *
 * Transitional, and the one whose end state differs most from what it lists
 * today: a single feed of what happened. Until that exists this names the three
 * records HarborMaster already keeps rather than pretending to merge them.
 */
const LINKS: readonly SectionLink[] = [
  {
    label: "Events",
    description: "What the Docker daemon reported: containers starting, stopping and being replaced.",
    path: "/events",
    permission: "event:read",
  },
  {
    label: "Security audit",
    description: "Every state-changing action, and the account that performed it.",
    path: "/audit",
    permission: "audit:read",
  },
  {
    label: "Notifications",
    description: "Where HarborMaster sends word when something needs a person, and what it has sent.",
    path: "/notifications",
    permission: "notification:read",
  },
];

export function ActivitySection() {
  return (
    <SectionLanding
      title="Activity"
      description="What HarborMaster and the Docker daemon have recorded, and who did what."
      links={LINKS}
    />
  );
}
