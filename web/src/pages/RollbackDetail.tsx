import { useState } from "react";
import { Link, useParams } from "react-router";

import { ApiError } from "../api/client";
import type { PreservationReport } from "../api/executionTypes";
import type { Rollback, RollbackEvent } from "../api/rollbackTypes";
import {
  ROLLBACK_CHECKPOINT_LABELS,
  ROLLBACK_STATE_LABELS,
  isRollbackCancellable,
} from "../api/rollbackTypes";
import { DetailSection } from "../components/DetailSection";
import { VerificationBadge } from "../components/ExecutionBadges";
import { PageIntro } from "../components/PageIntro";
import { RecoveryPlanPanel } from "../components/RecoveryPlanPanel";
import {
  RollbackCheckpointBadge,
  RollbackContainerIdentities,
  RollbackFailureBadge,
  RollbackHostState,
  RollbackImageDetail,
  RollbackStateBadge,
  RollbackWarningNotice,
} from "../components/RollbackBadges";
import {
  DisconnectedState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { stopRollback, useRollback } from "../hooks/useRollbacks";

/**
 * One rollback, with live progress, its verification results, and its
 * checkpoint trail.
 *
 * Polls while the rollback is active and stops once it settles.
 *
 * The page is ordered by what an operator needs FIRST: what is true of the host
 * right now, then the recovery steps if there are any, then the verification
 * detail, then the history. A record that left containers behind puts the
 * answer above the fold; a clean success is quiet.
 */
export function RollbackDetail() {
  const { id = "" } = useParams();
  const [cancelling, setCancelling] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const state = useRollback(id, { poll: true });
  const rollback = state.data?.rollback;
  const events = state.data?.events ?? [];

  const cancel = async () => {
    setActionError(null);
    setCancelling(true);
    try {
      await stopRollback(id);
      state.refresh();
    } catch (caught) {
      setActionError(
        caught instanceof ApiError
          ? caught.message
          : "The rollback could not be cancelled.",
      );
    } finally {
      setCancelling(false);
    }
  };

  if (state.status === "loading") {
    return <LoadingState label="Loading the rollback" />;
  }
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }
  if (!rollback) {
    return <LoadingState label="Loading the rollback" />;
  }

  return (
    <div className="space-y-6">
      <PageIntro
        title={rollback.containerName}
        description={`Rollback ${rollback.rollbackId}`}
      />

      {/* First, always: what is true of this host. */}
      <RollbackHostState rollback={rollback} />

      <div className="flex flex-wrap items-center gap-2">
        <RollbackStateBadge state={rollback.state} />
        {rollback.failure && <RollbackFailureBadge failure={rollback.failure} />}
        <RollbackCheckpointBadge
          checkpoint={rollback.checkpoint}
          mutated={Boolean(rollback.mutatedAt)}
        />
      </div>

      {/* Second: the way out, when there is something to settle. */}
      {rollback.recovery && <RecoveryPlanPanel plan={rollback.recovery} />}

      {actionError && (
        <p
          role="alert"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          {actionError}
        </p>
      )}

      {isRollbackCancellable(rollback.state) && (
        <div className="space-y-2">
          <button
            type="button"
            onClick={cancel}
            disabled={cancelling}
            className="rounded-md border border-border-subtle bg-surface-raised px-3 py-1.5 text-sm text-content disabled:opacity-60"
          >
            {cancelling ? "Cancelling…" : "Cancel this rollback"}
          </button>
          <p className="text-xs text-content-muted">
            Nothing on this host has been changed yet, so cancelling costs
            nothing. Once the replacement is stopped this control disappears: a
            rollback that has begun must reach a recorded conclusion.
          </p>
        </div>
      )}

      <RollbackWarningNotice />

      <DetailSection title="What was backed out">
        <RollbackImageDetail rollback={rollback} />
      </DetailSection>

      <DetailSection title="Verification">
        <VerificationResults rollback={rollback} />
      </DetailSection>

      {rollback.verification.preservationReport && (
        <DetailSection title="Configuration preservation">
          <PreservationDetail report={rollback.verification.preservationReport} />
        </DetailSection>
      )}

      <DetailSection title="Containers on this host">
        <RollbackContainerIdentities rollback={rollback} />
      </DetailSection>

      <DetailSection title="Evidence">
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
          <dt className="text-content-muted">Recreation</dt>
          <dd>
            <Link
              to={`/executions/${encodeURIComponent(rollback.executionId)}`}
              className="font-mono break-all text-content underline underline-offset-2"
            >
              {rollback.executionId}
            </Link>
          </dd>

          <dt className="text-content-muted">Original container</dt>
          <dd>
            <Link
              to={`/containers/${encodeURIComponent(rollback.originalId)}`}
              className="font-mono break-all text-content underline underline-offset-2"
            >
              {rollback.originalId.slice(0, 12)}
            </Link>
          </dd>

          {rollback.requestedBy?.username ? (
            <>
              <dt className="text-content-muted">Requested by</dt>
              <dd className="text-content">{rollback.requestedBy.username}</dd>
            </>
          ) : null}

          <dt className="text-content-muted">Requested</dt>
          <dd className="text-content">
            <time dateTime={rollback.requestedAt}>
              {new Date(rollback.requestedAt).toLocaleString()}
            </time>
          </dd>
        </dl>
      </DetailSection>

      <DetailSection title="What happened, in order">
        <RollbackTimeline events={events} />
      </DetailSection>
    </div>
  );
}

/**
 * The four proofs.
 *
 * All four are shown even when only one failed, because "which checks were
 * never reached" is as informative as "which one failed" — and an `unknown` is
 * rendered as not-checked rather than as a pass.
 *
 * The shared VerificationBadge is reused deliberately: a second implementation
 * of "how a verdict looks" would eventually disagree with the first, and the
 * disagreement that matters is the one where an unknown starts looking green.
 */
function VerificationResults({ rollback }: { rollback: Rollback }) {
  const verification = rollback.verification;

  return (
    <div className="space-y-3">
      <div className="flex flex-wrap gap-2">
        <VerificationBadge
          label="Health"
          result={verification.health}
          detail={
            verification.healthChecked
              ? `The container's own health check reported ${verification.healthState || "nothing"}`
              : `The container declares no health check, so it had to stay running for ${
                  verification.stabilitySeconds ?? 0
                }s`
          }
        />
        <VerificationBadge label="Image" result={verification.image} />
        <VerificationBadge label="Configuration" result={verification.preservation} />
        <VerificationBadge label="Networks" result={verification.network} />
      </div>

      {!verification.healthChecked && (
        <p className="text-xs text-content-muted">
          This container declares no health check, so HarborMaster could only
          establish that it stayed running. That is weaker evidence than a health
          check and is recorded as such.
        </p>
      )}
    </div>
  );
}

/**
 * The field-by-field configuration comparison.
 *
 * For a rollback this compares the restored original against the projection
 * taken BEFORE the rollback moved it — so what it proves is that the rollback
 * did not change the container it restored.
 *
 * No value here is a secret: a sensitive environment variable or log option
 * contributes a keyed digest, so a changed password shows as a changed digest
 * and never as a password.
 */
function PreservationDetail({ report }: { report: PreservationReport }) {
  if (report.unverifiable) {
    return (
      <p className="text-sm text-content-muted">
        {report.reason ||
          "The comparison could not be performed, so nothing about the configuration was established."}
      </p>
    );
  }

  if (report.status === "passed") {
    return (
      <p className="text-sm text-content-muted">
        All {report.checked} configuration fields are as they were before the
        rollback moved this container.
      </p>
    );
  }

  const differences = report.differences ?? [];

  return (
    <div className="space-y-3">
      <p className="text-sm text-content">
        {report.matched} of {report.checked} configuration fields matched.
      </p>

      <ul className="space-y-2">
        {differences.map((difference) => (
          <li
            key={`${difference.field}:${difference.kind}`}
            className="rounded-md border border-border-subtle bg-surface-sunken p-3 text-xs"
          >
            <p className="font-mono font-medium text-content">{difference.field}</p>
            <dl className="mt-1 grid grid-cols-[auto_1fr] gap-x-3 gap-y-0.5">
              <dt className="text-content-muted">Before the rollback</dt>
              <dd className="font-mono break-all text-content">
                {difference.expected || "—"}
              </dd>
              <dt className="text-content-muted">After</dt>
              <dd className="font-mono break-all text-content">
                {difference.actual || "—"}
              </dd>
            </dl>
          </li>
        ))}
      </ul>

      {report.truncated && (
        <p className="text-xs text-content-muted">
          More fields differed than are listed here.
        </p>
      )}
    </div>
  );
}

/** The checkpoint trail, oldest first: this is read as a narrative. */
function RollbackTimeline({ events }: { events: RollbackEvent[] }) {
  if (events.length === 0) {
    return <p className="text-sm text-content-muted">No entries recorded.</p>;
  }

  return (
    <ol className="space-y-2">
      {events.map((event, index) => (
        <li
          key={`${event.at}-${index}`}
          className="flex flex-wrap items-baseline gap-x-3 gap-y-1 text-xs"
        >
          <time dateTime={event.at} className="shrink-0 text-content-muted">
            {new Date(event.at).toLocaleTimeString()}
          </time>
          <span className="font-medium text-content">
            {ROLLBACK_STATE_LABELS[event.state]}
          </span>
          {event.checkpoint ? (
            <span className="rounded border border-border-subtle px-1.5 py-0.5 text-content-muted">
              {ROLLBACK_CHECKPOINT_LABELS[event.checkpoint]}
            </span>
          ) : null}
          {event.detail && <span className="text-content-muted">{event.detail}</span>}
        </li>
      ))}
    </ol>
  );
}
