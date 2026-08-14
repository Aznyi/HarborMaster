import { Link } from "react-router";

import type {
  DependencyMember,
  DependencyOperationSummary,
} from "../api/dependencyTypes";
import {
  memberStateLabel,
  operationFailureLabel,
  operationStateLabel,
} from "../api/dependencyTypes";
import { describeRebind, rebindNeedsOperator } from "./DependencyStatus";

/**
 * What HarborMaster did when it replaced a container others depend on.
 *
 * # Why the partial state is the point
 *
 * A coordinated update has two halves, and they can end differently. The
 * provider can be replaced and verified while one of the containers that had to
 * be reattached is not — and HarborMaster does NOT roll a dependency group
 * backward. The provider stays replaced, the successful reattachments stay
 * attached, and one container is left needing a person.
 *
 * That is the situation this view exists for. A summary that reported only
 * "failed" would describe an operation; this describes a HOST, which is what
 * somebody has to go and settle.
 *
 * There are deliberately no controls here. Retrying a reattachment goes through
 * the acquisition and recreation services, each of which re-runs its own
 * preflight — and a "roll the group back" button would be an action HarborMaster
 * has no mechanism for and no intention of acquiring.
 */
export function DependencyOperationList({
  summaries,
  limit,
}: {
  summaries: DependencyOperationSummary[];
  /** The cap the listing was bounded by, so truncation is never silent. */
  limit?: number;
}) {
  if (summaries.length === 0) {
    return (
      <p className="text-sm text-content-muted">
        HarborMaster has not had to reattach anything. A coordinated update is
        recorded here only when a container being replaced has others sharing
        its namespace.
      </p>
    );
  }

  return (
    <div className="space-y-3">
      <ul className="space-y-3" aria-label="Coordinated provider updates">
        {summaries.map((summary) => (
          <li key={summary.operation.operationId}>
            <OperationCard summary={summary} />
          </li>
        ))}
      </ul>
      {limit !== undefined && summaries.length >= limit ? (
        <p className="text-xs text-content-muted">
          Showing the {limit} most recent. Older operations are not listed here.
        </p>
      ) : null}
    </div>
  );
}

/** One operation: the provider, the reattachments, and the overall answer. */
export function OperationCard({
  summary,
}: {
  summary: DependencyOperationSummary;
}) {
  const { operation } = summary;
  const members = operation.members ?? [];
  const failed = members.filter((member) => rebindNeedsOperator(member.state));

  return (
    <article
      className={`space-y-3 rounded-xl border px-4 py-3 ${
        summary.needsAttention
          ? "border-warn/40 bg-warn-soft"
          : "border-border-subtle bg-surface-raised"
      }`}
    >
      <header className="flex flex-wrap items-baseline justify-between gap-2">
        <h3 className="text-sm font-semibold">
          <Link
            to={`/containers?search=${encodeURIComponent(operation.provider)}`}
            className="break-all text-accent underline"
          >
            {operation.provider}
          </Link>{" "}
          update
        </h3>
        <p className="text-xs text-content-muted">
          {operationStateLabel(operation.state)}
        </p>
      </header>

      <dl className="grid gap-x-4 gap-y-1 text-sm sm:grid-cols-[10rem_1fr]">
        <dt className="text-content-muted">Provider update</dt>
        <dd className="font-medium">
          {summary.providerVerified ? "Verified" : "Not verified"}
        </dd>
      </dl>

      {members.length > 0 ? (
        <section aria-label={`Required reattachments for ${operation.provider}`}>
          <h4 className="text-xs uppercase tracking-wide text-content-muted">
            Required reattachments
          </h4>
          <ul className="mt-1 space-y-1">
            {members.map((member) => (
              <MemberRow key={member.dependent} member={member} />
            ))}
          </ul>
        </section>
      ) : null}

      <section aria-label={`Overall outcome for ${operation.provider}`}>
        <h4 className="text-xs uppercase tracking-wide text-content-muted">
          Overall
        </h4>
        <p
          className={`mt-0.5 text-sm font-medium ${
            summary.needsAttention ? "text-warn" : ""
          }`}
        >
          {summary.complete
            ? "Complete"
            : summary.needsAttention
              ? "Needs attention"
              : "In progress"}
        </p>

        {operation.failure ? (
          <p className="mt-1 max-w-prose text-sm text-content-muted">
            {operationFailureLabel(operation.failure)}
          </p>
        ) : null}

        {summary.needsAttention ? (
          <p className="mt-2 max-w-prose text-sm">
            {/* The sentence that stops somebody waiting for a recovery that is
                never coming, and stops them assuming the host was returned to
                how it was. */}
            The provider and the successful reattachments remain in place.
            HarborMaster does not automatically roll the dependency group
            backward, and it does not retry a reattachment by itself.
            {failed.length > 0 ? (
              <>
                {" "}
                No image version was changed by any reattachment — each one
                recreates a container on the digest it was already running.
              </>
            ) : null}
          </p>
        ) : null}
      </section>
    </article>
  );
}

/** One reattachment, as a line in the operation's list. */
function MemberRow({ member }: { member: DependencyMember }) {
  const urgent = rebindNeedsOperator(member.state);
  const { detail } = describeRebind(member.state, member.dependent, member.provider);

  return (
    <li className="flex flex-wrap items-baseline gap-x-3 gap-y-0.5 text-sm">
      <Link
        to={`/containers?search=${encodeURIComponent(member.dependent)}`}
        className="break-all text-accent underline"
      >
        {member.dependent}
      </Link>
      {/* The state in WORDS beside the name. Never colour alone: an operator
          who cannot distinguish the tones must read the same outcome. */}
      <span className={`text-xs ${urgent ? "font-medium text-warn" : "text-content-muted"}`}>
        {memberStateLabel(member.state)}
      </span>
      {urgent ? (
        <span className="w-full break-words text-xs text-content-muted">
          {detail}
        </span>
      ) : null}
    </li>
  );
}
