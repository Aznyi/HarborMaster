import { NotYetAvailableState } from "../components/States";
import { PageIntro } from "../components/PageIntro";

export function Containers() {
  return (
    <div className="flex flex-col gap-6">
      <PageIntro
        title="Containers"
        description="A read-only inventory of the containers the Docker Engine reports. HarborMaster observes them; it never starts, stops, recreates, or removes one."
      />
      <NotYetAvailableState feature="Container listings" />
    </div>
  );
}
