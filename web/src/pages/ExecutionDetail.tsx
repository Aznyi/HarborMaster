import { useState } from "react";
import { Link, useParams } from "react-router";

import { ApiError } from "../api/client";
import type {
  Execution,
  ExecutionEvent,
  PreservationReport,
} from "../api/executionTypes";
import {
  EXECUTION_CHECKPOINT_LABELS,
  EXECUTION_STATE_LABELS,
  isCancellable,
} from "../api/executionTypes";
import type { Rollback } from "../api/rollbackTypes";
import { DetailSection } from "../components/DetailSection";
import {
  ExecutionCheckpointBadge,
  ExecutionContainerIdentities,
  ExecutionFailureBadge,
  ExecutionHostState,
  ExecutionStateBadge,
  ExecutionTargetDetail,
  RecreationWarningNotice,
  VerificationBadge,
} from "../components/ExecutionBadges";
import { PageIntro } from "../components/PageIntro";
import { RecoveryPlanPanel } from "../components/RecoveryPlanPanel";
import { RollbackStateBadge } from "../components/RollbackBadges";
import { RollbackContainerAction } from "../components/RollbackContainerAction";
import {
  DisconnectedState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { stopExecution, useExecution } from "../hooks/useExecutions";
import { useRollbacks } from "../hooks/useRollbacks";

/**
 * One recreation, with live progress, its verification results, and its audit
 * trail.
 *
 * Polls while the recreation is active and stops once it settles.
 *
 * The page is ordered by what an operator needs FIRST: what is true of the host
 * right now, then the recovery steps if there are any, then the verification
 * detail, then the history. A record that left containers behind puts the
 * answer above the fold; a clean success is quiet.
 */
export function ExecutionDetail() {
  const { id = "" } = useParams();
  const [cancelling, setCancelling] = useState(false);
  const [actionError, setActionError] = useState<string | null>(null);

  const state = useExecution(id, { poll: true });
  const execution = state.data?.execution;
  const events = state.data?.events ?? [];
  const eligibility = state.data?.rollback;

  // The rollbacks already made FROM this recreation. Read separately because
  // it is a history rather than a property of the record, and a page that
  // showed the control without the history would let an operator ask twice for
  // something the server will refuse the second time.
  const rollbacks = useRollbacks({ executionId: id, pageSize: 10 });
  const rollbackHistory = rollbacks.data?.items ?? [];

  const cancel = async () => {
    setActionError(null);
    setCancelling(true);
    try {
      await stopExecution(id);
      state.refresh();
    } catch (caught) {
      setActionError(
        caught instanceof ApiError
          ? caught.message
          : "The recreation could not be cancelled.",
      );
    } finally {
      setCancelling(false);
    }
  };

  if (state.status === "loading") {
    return <LoadingState label="Loading the recreation" />;
  }
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }
  if (!execution) {
    return <LoadingState label="Loading the recreation" />;
  }

  return (
    <div className="space-y-6">
      <PageIntro
        title={execution.containerName}
        description={`Recreation ${execution.executionId}`}
      />

      {/* First, always: what is true of this host. */}
      <ExecutionHostState execution={execution} />

      <div className="flex flex-wrap items-center gap-2">
        <ExecutionStateBadge state={execution.state} />
        {execution.failure && <ExecutionFailureBadge failure={execution.failure} />}
        <ExecutionCheckpointBadge
          checkpoint={execution.checkpoint}
          mutated={Boolean(execution.mutatedAt)}
        />
      </div>

      {/* Second: the way out, when there is something to settle. */}
      {execution.recovery && <RecoveryPlanPanel plan={execution.recovery} />}

      {actionError && (
        <p
          role="alert"
          className="rounded-lg border border-danger/40 bg-danger-soft px-3 py-2 text-sm text-danger"
        >
          {actionError}
        </p>
      )}

      {isCancellable(execution.state) && (
        <div className="space-y-2">
          <button
            type="button"
            onClick={cancel}
            disabled={cancelling}
            className="rounded-md border border-border-subtle bg-surface-raised px-3 py-1.5 text-sm text-content disabled:opacity-60"
          >
            {cancelling ? "Cancelling…" : "Cancel this recreation"}
          </button>
          <p className="text-xs text-content-muted">
            Nothing on this host has been changed yet, so cancelling costs
            nothing. Once the original is stopped this control disappears: a
            recreation that has begun must reach a recorded conclusion.
          </p>
        </div>
      )}

      <RecreationWarningNotice />

      {/* Rollback: offered only once the recreation has settled, because a
          rollback of something still moving is refused anyway and offering it
          would be offering a button that cannot work. */}
      {!isCancellable(execution.state) && (
        <DetailSection title="Undo this recreation">
          <div className="space-y-3">
            <RollbackContainerAction
              executionId={execution.executionId}
              eligibility={eligibility}
              onRequested={() => {
                state.refresh();
                rollbacks.refresh();
              }}
            />
            <RollbackHistory rollbacks={rollbackHistory} />
          </div>
        </DetailSection>
      )}

      <DetailSection title="What was applied">
        <ExecutionTargetDetail execution={execution} />
      </DetailSection>

      <DetailSection title="Verification">
        <VerificationResults execution={execution} />
      </DetailSection>

      {execution.verification.preservationReport && (
        <DetailSection title="Configuration match">
          <PreservationDetail report={execution.verification.preservationReport} />
        </DetailSection>
      )}

      <DetailSection title="Containers on this host">
        <ExecutionContainerIdentities execution={execution} />
        {!execution.mutatedAt && !execution.checkpoint && (
          <p className="text-xs text-content-muted">
            Nothing was changed, so there is nothing extra on this host.
          </p>
        )}
      </DetailSection>

      <DetailSection title="Evidence">
        <dl className="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1 text-xs">
          <dt className="text-content-muted">Acquisition</dt>
          <dd>
            <Link
              to={`/acquisitions/${encodeURIComponent(execution.acquisitionId)}`}
              className="font-mono break-all text-content underline underline-offset-2"
            >
              {execution.acquisitionId}
            </Link>
          </dd>

          <dt className="text-content-muted">Change plan</dt>
          <dd className="font-mono break-all text-content">{execution.planId}</dd>

          <dt className="text-content-muted">Container</dt>
          <dd>
            <Link
              to={`/containers/${encodeURIComponent(execution.containerId)}`}
              className="font-mono break-all text-content underline underline-offset-2"
            >
              {execution.containerId.slice(0, 12)}
            </Link>
          </dd>

          {execution.snapshotId ? (
            <>
              <dt className="text-content-muted">Baseline snapshot</dt>
              <dd>
                <Link
                  to={`/snapshots/${execution.snapshotId}`}
                  className="text-content underline underline-offset-2"
                >
                  #{execution.snapshotId}
                </Link>
              </dd>
            </>
          ) : null}
        </dl>
      </DetailSection>

      <DetailSection title="What happened, in order">
        <ExecutionTimeline events={events} />
      </DetailSection>
    </div>
  );
}

/**
 * The rollbacks already made from this recreation.
 *
 * Shown next to the control rather than on a separate page, because "has
 * somebody already tried this" is the question an operator has immediately
 * before pressing it — and a rollback that failed partway is the single most
 * important thing to have read first.
 */
function RollbackHistory({ rollbacks }: { rollbacks: Rollback[] }) {
  if (rollbacks.length === 0) return null;

  return (
    <div className="space-y-2">
      <p className="text-xs font-medium uppercase tracking-wide text-content-muted">
        Rollbacks of this recreation
      </p>
      <ul className="space-y-1.5">
        {rollbacks.map((rollback) => (
          <li key={rollback.rollbackId} className="flex flex-wrap items-center gap-2 text-xs">
            <RollbackStateBadge state={rollback.state} />
            <Link
              to={`/rollbacks/${encodeURIComponent(rollback.rollbackId)}`}
              className="font-mono break-all text-content underline underline-offset-2"
            >
              {rollback.rollbackId}
            </Link>
            <time dateTime={rollback.requestedAt} className="text-content-muted">
              {new Date(rollback.requestedAt).toLocaleString()}
            </time>
          </li>
        ))}
      </ul>
    </div>
  );
}

/**
 * The four proofs.
 *
 * All four are shown even when only one failed, because "which checks were
 * never reached" is as informative as "which one failed" — and an `unknown` is
 * rendered as not-checked rather than as a pass.
 */
function VerificationResults({ execution }: { execution: Execution }) {
  const verification = execution.verification;

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
        All {report.checked} configuration fields matched the original.
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
              <dt className="text-content-muted">Original</dt>
              <dd className="font-mono break-all text-content">
                {difference.expected || "—"}
              </dd>
              <dt className="text-content-muted">Replacement</dt>
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

/** The audit trail, oldest first: this is read as a narrative. */
function ExecutionTimeline({ events }: { events: ExecutionEvent[] }) {
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
            {EXECUTION_STATE_LABELS[event.state]}
          </span>
          {event.checkpoint ? (
            <span className="rounded border border-border-subtle px-1.5 py-0.5 text-content-muted">
              {EXECUTION_CHECKPOINT_LABELS[event.checkpoint]}
            </span>
          ) : null}
          {event.detail && <span className="text-content-muted">{event.detail}</span>}
        </li>
      ))}
    </ol>
  );
}
