import { NotYetAvailableState } from "../components/States";
import { PageIntro } from "../components/PageIntro";

export function Events() {
  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Events"
        description="HarborMaster's append-only audit log: what it observed, when, and with what outcome."
      />
      <NotYetAvailableState feature="Event listings" />
    </div>
  );
}
