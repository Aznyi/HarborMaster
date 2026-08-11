import { useCallback, useState } from "react";
import { Link, useParams } from "react-router";

import type { AutomationDecision } from "../api/automationTypes";
import { AUTOMATION_TRIGGER_LABELS } from "../api/automationTypes";
import {
  AutomationReasonText,
  AutomationRunStateBadge,
  AutomationVerdictBadge,
} from "../components/AutomationBadges";
import { PageIntro } from "../components/PageIntro";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import {
  useApproveAutomationDecision,
  useAutomationRun,
} from "../hooks/useAutomation";
import { useSession } from "../hooks/useSession";
import { formatMoment } from "./Automation";

/**
 * One scheduler pass, and every decision it made.
 *
 * # The order is the point
 *
 * Decisions are returned in the pass's own execution ORDER, not alphabetically
 * and not by outcome. That is what makes a dry run's output "what would happen,
 * in what sequence" rather than an unordered set — and on a real pass it is how
 * an operator reconstructs which container was touched first.
 *
 * # Approval happens here
 *
 * A decision a policy held for a person is released from this page, because
 * this is where its reasoning is. Approving is a privileged action: it submits
 * the acquisition that leads to a container being stopped and replaced, and the
 * control says so before it is pressed.
 */
export function AutomationRun() {
  const params = useParams<{ id: string }>();
  const runId = params.id ?? "";

  const run = useAutomationRun(runId);

  if (run.status === "loading") return <LoadingState label="Loading pass" />;
  if (run.status === "disconnected") return <DisconnectedState onRetry={run.refresh} />;
  if (run.error) return <ErrorState error={run.error} onRetry={run.refresh} />;
  if (!run.data) return <LoadingState label="Loading pass" />;

  const record = run.data.run;
  const decisions = run.data.decisions;

  return (
    <div className="space-y-6">
      <PageIntro
        title="Automation pass"
        description={
          record.dryRun
            ? "A dry run. Every decision below was made against the real estate, " +
              "and nothing was changed."
            : "What the engine decided, in the order it decided it."
        }
      />

      <p className="text-sm">
        <Link className="underline" to="/automation">
          ← Back to automation
        </Link>
      </p>

      <section className="grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
        <Fact label="Started" value={formatMoment(record.startedAt)} />
        <Fact
          label="Trigger"
          value={`${AUTOMATION_TRIGGER_LABELS[record.trigger] ?? record.trigger}${
            record.dryRun ? " (dry run)" : ""
          }`}
        />
        <Fact
          label="Outcome"
          value=""
          badge={<AutomationRunStateBadge state={record.state} />}
        />
        <Fact
          label="Counters"
          value={
            `${record.considered ?? 0} considered · ` +
            `${record.eligible ?? 0} eligible · ` +
            `${record.submitted ?? 0} submitted · ` +
            `${record.failed ?? 0} failed`
          }
        />
      </section>

      {record.requestedBy?.username ? (
        <p className="text-sm text-content-muted">
          Started by <strong>{record.requestedBy.username}</strong>.
        </p>
      ) : null}

      {record.message ? (
        <p
          role="status"
          className="rounded-lg border border-warn/40 bg-warn-soft px-3 py-2 text-sm text-warn"
        >
          {record.message}
        </p>
      ) : null}

      {decisions.length === 0 ? (
        <EmptyState
          title="No decisions recorded"
          description="This pass examined no containers."
        />
      ) : (
        <DecisionList runId={record.runId} decisions={decisions} onChanged={run.refresh} />
      )}
    </div>
  );
}

function Fact({
  label,
  value,
  badge,
}: {
  label: string;
  value: string;
  badge?: React.ReactNode;
}) {
  return (
    <div className="rounded-xl border border-border-subtle bg-surface-raised px-4 py-3">
      <p className="text-xs uppercase tracking-wide text-content-muted">{label}</p>
      {badge ? <div className="mt-1">{badge}</div> : null}
      {value ? <p className="mt-1 text-sm">{value}</p> : null}
    </div>
  );
}

function DecisionList({
  runId,
  decisions,
  onChanged,
}: {
  runId: string;
  decisions: AutomationDecision[];
  onChanged: () => void;
}) {
  return (
    <section className="space-y-3">
      <h3 className="text-sm font-semibold">Decisions, in execution order</h3>
      <div className="overflow-x-auto rounded-xl border border-border-subtle">
        <table className="min-w-full text-sm">
          <thead className="bg-surface-sunken text-left text-xs uppercase tracking-wide text-content-muted">
            <tr>
              <th className="px-3 py-2">#</th>
              <th className="px-3 py-2">Container</th>
              <th className="px-3 py-2">Verdict</th>
              <th className="px-3 py-2">Reason</th>
              <th className="px-3 py-2">Change</th>
              <th className="px-3 py-2">Records</th>
              <th className="px-3 py-2" />
            </tr>
          </thead>
          <tbody className="divide-y divide-border-subtle">
            {decisions.map((decision) => (
              <tr key={`${decision.position}-${decision.containerName}`}>
                <td className="px-3 py-2 text-content-muted">{decision.position + 1}</td>
                <td className="px-3 py-2 font-medium">{decision.containerName}</td>
                <td className="px-3 py-2">
                  <AutomationVerdictBadge verdict={decision.verdict} />
                </td>
                <td className="px-3 py-2">
                  <AutomationReasonText
                    reason={decision.reason}
                    detail={decision.detail}
                  />
                </td>
                <td className="px-3 py-2 text-content-muted">
                  {decision.proposedImage ? (
                    <span title={decision.proposedDigest}>
                      {decision.currentImage} → {decision.proposedImage}
                    </span>
                  ) : (
                    "—"
                  )}
                </td>
                <td className="px-3 py-2 text-xs text-content-muted">
                  <RecordLinks decision={decision} />
                </td>
                <td className="px-3 py-2">
                  {decision.verdict === "awaitingApproval" ? (
                    <ApproveButton
                      runId={runId}
                      containerName={decision.containerName}
                      onApproved={onChanged}
                    />
                  ) : null}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </section>
  );
}

/**
 * Links to the records a decision created.
 *
 * The whole pipeline in one cell: which download, which recreation, which
 * rollback. Each is a record HarborMaster wrote, with its own page and its own
 * audit trail.
 */
function RecordLinks({ decision }: { decision: AutomationDecision }) {
  const links: React.ReactNode[] = [];
  if (decision.acquisitionId) {
    links.push(
      <Link key="acq" className="underline" to={`/acquisitions/${decision.acquisitionId}`}>
        download
      </Link>,
    );
  }
  if (decision.executionId) {
    links.push(
      <Link key="exec" className="underline" to={`/executions/${decision.executionId}`}>
        recreation
      </Link>,
    );
  }
  if (decision.rollbackId) {
    links.push(
      <Link key="rbk" className="underline" to={`/rollbacks/${decision.rollbackId}`}>
        rollback
      </Link>,
    );
  }
  if (links.length === 0) return <span>—</span>;

  return (
    <span className="flex flex-wrap gap-2">
      {links.map((link, index) => (
        <span key={index}>{link}</span>
      ))}
    </span>
  );
}

/**
 * Releases one held decision.
 *
 * Two-step: the first press asks for confirmation, and the confirmation states
 * what will actually happen. Approving is not a preference — it stops a
 * container and starts a different one in its place.
 */
export function ApproveButton({
  runId,
  containerName,
  onApproved,
}: {
  runId: string;
  containerName: string;
  onApproved: () => void;
}) {
  const session = useSession();
  const approve = useApproveAutomationDecision();

  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  const mayApprove = Boolean(
    session.user?.permissions.includes("automation:approve"),
  );

  const release = useCallback(async () => {
    setBusy(true);
    setFailure(null);
    try {
      await approve(runId, containerName);
      setConfirming(false);
      onApproved();
    } catch (error) {
      setFailure(
        error instanceof Error ? error.message : "The decision could not be released",
      );
    } finally {
      setBusy(false);
    }
  }, [approve, containerName, onApproved, runId]);

  if (!mayApprove) return null;

  if (!confirming) {
    return (
      <button
        type="button"
        className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs font-medium"
        onClick={() => setConfirming(true)}
      >
        Approve
      </button>
    );
  }

  return (
    <div className="space-y-2">
      <p className="text-xs text-warn">
        This stops <strong>{containerName}</strong> and starts a replacement.
      </p>
      <div className="flex gap-2">
        <button
          type="button"
          className="rounded-lg border border-warn/40 bg-warn-soft px-2.5 py-1 text-xs font-medium text-warn disabled:opacity-50"
          disabled={busy}
          onClick={() => void release()}
        >
          {busy ? "Approving…" : "Yes, update it"}
        </button>
        <button
          type="button"
          className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs"
          disabled={busy}
          onClick={() => setConfirming(false)}
        >
          Cancel
        </button>
      </div>
      {failure ? (
        <p role="alert" className="text-xs text-danger">
          {failure}
        </p>
      ) : null}
    </div>
  );
}
