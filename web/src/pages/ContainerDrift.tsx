import { useState } from "react";
import { Link, useParams } from "react-router";

import type { DriftEvaluation, DriftRecord, OperatorStatus } from "../api/driftTypes";
import { OPERATOR_STATUSES } from "../api/driftTypes";
import { ApiError } from "../api/client";
import { PageIntro } from "../components/PageIntro";
import {
  CategoryBadge,
  ChangeKindBadge,
  DriftStatusBadge,
  DriftValues,
  SeverityBadge,
} from "../components/DriftBadges";
import { DriftTimeline } from "../components/DriftTimeline";
import { DetailSection } from "../components/DetailSection";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import { setDriftStatus, useContainerDrift } from "../hooks/useDrift";

/**
 * One container's drift.
 *
 * The evaluation banner is the important part of this page: it is what
 * distinguishes "this container matches its baseline" from "this container was
 * never comparable". Rendering an empty list as a clean bill of health for a
 * container with no snapshot would be the worst thing this page could do.
 */
export function ContainerDrift() {
  const { id = "" } = useParams();
  const [showResolved, setShowResolved] = useState(false);
  const state = useContainerDrift(id, { openOnly: !showResolved, pageSize: 200 });

  if (state.status === "loading") return <LoadingState label="Loading container drift" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) {
    return <ErrorState error={state.error} onRetry={state.refresh} />;
  }

  // Tolerate a malformed payload rather than throwing.
  const records: DriftRecord[] = state.data?.records ?? [];
  const evaluation = state.data?.evaluation;

  return (
    <div className="space-y-6">
      <PageIntro
        title="Container drift"
        description={
          "Differences between this container's baseline snapshot and its " +
          "configuration now. Reporting only — nothing here changes the container."
        }
      />

      <p className="text-sm text-content-muted">
        <Link
          to={`/containers/${id}`}
          className="underline underline-offset-2 hover:text-content"
        >
          Back to the container
        </Link>
      </p>

      <EvaluationBanner evaluation={evaluation} />

      <label className="flex items-center gap-2 text-sm text-content">
        <input
          type="checkbox"
          className="size-4 rounded border-border-subtle"
          checked={showResolved}
          onChange={(event) => setShowResolved(event.target.checked)}
        />
        Include resolved
      </label>

      {records.length === 0 ? (
        <EmptyState
          title={
            evaluation
              ? "No drift against the baseline"
              : "This container has not been evaluated"
          }
          description={
            evaluation
              ? "Every field this container was compared on matches its baseline snapshot."
              : "Drift is evaluated in the background. A container with no baseline snapshot cannot be compared at all."
          }
        />
      ) : (
        <>
          <DetailSection title="Timeline">
            <DriftTimeline records={records} />
          </DetailSection>

          <DetailSection title="Differences">
            <ul className="space-y-2">
              {records.map((record) => (
                <DriftCard key={record.id} record={record} onChanged={state.refresh} />
              ))}
            </ul>
          </DetailSection>
        </>
      )}
    </div>
  );
}

/**
 * The evaluation banner.
 *
 * Three distinct states, deliberately not collapsed into two: never evaluated,
 * evaluated but incomplete, and evaluated fully. Only the last one licenses
 * "this container is clean".
 */
function EvaluationBanner({ evaluation }: { evaluation?: DriftEvaluation }) {
  if (!evaluation) {
    return (
      <p
        role="status"
        className="rounded-lg border border-border-subtle bg-surface-sunken px-3 py-2 text-sm text-content-muted"
      >
        This container has never been evaluated for drift, so an empty list here
        means <strong>not checked</strong> rather than <strong>no drift</strong>.
      </p>
    );
  }

  if (!evaluation.complete) {
    return (
      <p
        role="status"
        className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm text-warn"
      >
        The most recent evaluation could not compare everything
        {evaluation.reason ? `: ${evaluation.reason}` : "."} Anything listed
        below is real, but the list may be incomplete.
      </p>
    );
  }

  return (
    <p className="text-xs text-content-muted">
      Last evaluated{" "}
      <time dateTime={evaluation.evaluatedAt}>
        {new Date(evaluation.evaluatedAt).toLocaleString()}
      </time>{" "}
      against snapshot{" "}
      <Link
        to={`/snapshots/${evaluation.snapshotId}`}
        className="underline underline-offset-2 hover:text-content"
      >
        #{evaluation.snapshotId}
      </Link>
      .
    </p>
  );
}

/** One difference, with the operator's status controls. */
function DriftCard({
  record,
  onChanged,
}: {
  record: DriftRecord;
  onChanged: () => void;
}) {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string | null>(null);

  async function apply(status: OperatorStatus) {
    setBusy(true);
    setError(null);
    try {
      await setDriftStatus(record.id, status);
      onChanged();
    } catch (cause) {
      setError(
        cause instanceof ApiError ? cause.message : "The status could not be changed",
      );
    } finally {
      setBusy(false);
    }
  }

  return (
    <li className="rounded-lg border border-border-subtle bg-surface p-4">
      <div className="flex flex-wrap items-center gap-2">
        <SeverityBadge severity={record.severity} />
        <CategoryBadge category={record.category} />
        <ChangeKindBadge kind={record.kind} />
        <DriftStatusBadge status={record.status} />
      </div>

      <p className="mt-2 font-mono text-sm text-content break-all">{record.field}</p>
      {record.reason && (
        <p className="mt-1 text-xs text-content-muted">{record.reason}</p>
      )}

      <div className="mt-3">
        <DriftValues record={record} />
      </div>

      {record.note && (
        <p className="mt-2 text-xs text-content-muted">
          <span className="font-medium">Note:</span> {record.note}
        </p>
      )}

      {/*
        Only the three OPERATOR statuses are offered. There is no "resolve"
        button and there must not be: resolution is something the world does,
        observed by the engine, not something a person asserts. A button that
        let someone mark drift resolved would make this list stop describing
        reality.
      */}
      {record.status !== "resolved" && (
        <div className="mt-3 flex flex-wrap items-center gap-2">
          {OPERATOR_STATUSES.map((status) => (
            <button
              key={status}
              type="button"
              disabled={busy || record.status === status}
              onClick={() => void apply(status)}
              className="rounded-md border border-border-subtle px-2.5 py-1 text-xs text-content transition hover:bg-surface-sunken disabled:cursor-not-allowed disabled:opacity-50"
            >
              {status}
            </button>
          ))}
        </div>
      )}

      {error && (
        <p role="alert" className="mt-2 text-xs text-danger">
          {error}
        </p>
      )}
    </li>
  );
}
