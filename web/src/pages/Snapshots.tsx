import { NotYetAvailableState } from "../components/States";
import { PageIntro } from "../components/PageIntro";

export function Snapshots() {
  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Snapshots"
        description="Point-in-time captures of container configuration. Snapshots are append-only and are the record a future rollback will restore from."
      />
      <NotYetAvailableState feature="Snapshot listings" />
    </div>
  );
}
