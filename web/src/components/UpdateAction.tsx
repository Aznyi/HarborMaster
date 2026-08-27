import { useState } from "react";
import { Link } from "react-router";

import type { UpdateRowModel } from "../api/updateWorkspace";
import { AcquireImageAction } from "./AcquireImageAction";
import { PlanApprovalAction } from "./PlanApprovalAction";
import { RecreateContainerAction } from "./RecreateContainerAction";
import { useSession } from "../hooks/useSession";

/**
 * The next step for one update, on the page where it was decided.
 *
 * # The friction this removes, and the boundary it keeps
 *
 * Applying an update is three server operations against three different
 * records: a plan is approved, an ACQUISITION is authorised by that plan, and
 * an EXECUTION is authorised by that acquisition. Each is anchored to the
 * record before it on purpose -- the comments on the two action components say
 * so -- and the anchoring is a safety property, not an accident:
 *
 *   - a download names a plan, so there is nowhere to aim one freely;
 *   - a recreation names an acquisition, so it can only replace a container
 *     with an image already on this host and already verified.
 *
 * What was wrong was never the boundary. It was that each step lived on the
 * page that happened to own its record, so applying one update meant visiting
 * the review list, a container plan page, and an acquisition detail page in
 * sequence -- and nothing on any of them said what the next page was.
 *
 * This composes the SAME three components in the order the server already
 * requires. It adds no endpoint, merges no operation, and skips no
 * confirmation: every component below still renders its own two-step dialog and
 * still refuses when its own precondition is unmet.
 */
export function UpdateAction({
  row,
  onChanged,
}: {
  row: UpdateRowModel;
  /** Re-reads the three lists, so the row advances to its next step. */
  onChanged: () => void;
}) {
  const session = useSession();
  const { plan, acquisition, assessment } = row;

  // Whether a valid approval already stands. Reported by the approval control
  // rather than inferred: approving deliberately leaves the recommendation as
  // the planner wrote it, so `manualReview` is still `manualReview` afterwards
  // and there is nothing on the plan to read.
  const [approved, setApproved] = useState(false);

  const mayAcquire = session.can("acquisition:create");
  const mayExecute = session.can("execution:create");

  // Nothing to move onto. Saying so beats offering a control that the server
  // would refuse, and the row's details carry the reason.
  if (assessment.kind === "unknown") {
    return (
      <p className="text-xs text-content-muted">
        Nothing to apply until this can be assessed.
      </p>
    );
  }

  // STEP 3 -- the image is on this host and verified. RecreateContainerAction
  // enforces that itself; it renders nothing for any other acquisition state.
  if (acquisition && acquisition.state === "succeeded") {
    if (!mayExecute) {
      return (
        <p className="text-xs text-content-muted">
          Downloaded. Recreating the container needs the execution permission.
        </p>
      );
    }
    return (
      <div className="flex flex-col gap-1">
        <p className="text-xs text-content-muted">
          Image downloaded and verified.
        </p>
        <RecreateContainerAction acquisition={acquisition} onRequested={onChanged} />
      </div>
    );
  }

  // A download already under way. The acquisition page owns the detail; this
  // reports honestly and links to it rather than inventing progress.
  //
  // Listed positively rather than as "not failed": a cancelled or expired
  // acquisition is finished, and reporting it as still downloading would leave
  // the row stuck on a step nothing is working on.
  if (
    acquisition &&
    ["queued", "validating", "pulling", "verifying"].includes(acquisition.state)
  ) {
    return (
      <p className="text-xs text-content-muted">
        Downloading…{" "}
        <Link
          to={`/acquisitions/${encodeURIComponent(acquisition.acquisitionId)}`}
          className="text-accent hover:underline"
        >
          view download
        </Link>
      </p>
    );
  }

  // STEP 1 -- the plan asks for a person. PlanApprovalAction renders nothing
  // unless the recommendation is manualReview, and reads the approval that
  // already exists rather than assuming there is none.
  if (assessment.kind === "review") {
    return (
      <div className="flex flex-col items-start gap-2">
        <PlanApprovalAction
          plan={plan}
          onChanged={onChanged}
          onApprovalKnown={setApproved}
        />

        {/* The step that used to be on another page. Once the review is
            recorded the download becomes the obvious next action, in place,
            without the operator having to know that a plan page exists. */}
        {approved ? (
          mayAcquire ? (
            <AcquireImageAction plan={plan} onRequested={onChanged} />
          ) : (
            <p className="text-xs text-content-muted">
              Reviewed. Downloading the image needs the acquisition permission.
            </p>
          )
        ) : null}
      </div>
    );
  }

  // STEP 2 -- ready to download. The plan id is the whole request; the server
  // revalidates the plan before it fetches anything.
  if (!mayAcquire) {
    return (
      <p className="text-xs text-content-muted">
        Applying an update needs the image acquisition permission.
      </p>
    );
  }

  return <AcquireImageAction plan={plan} onRequested={onChanged} />;
}
