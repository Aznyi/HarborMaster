import { useCallback, useState } from "react";

import type { PausedContainer } from "../api/automationTypes";
import { isPauseActive } from "../api/automationTypes";
import { PauseReasonBadge } from "../components/AutomationBadges";
import { PageIntro } from "../components/PageIntro";
import {
  DisconnectedState,
  EmptyState,
  ErrorState,
  LoadingState,
} from "../components/States";
import {
  useAutomationPauses,
  usePauseAutomation,
  useResumeAutomation,
} from "../hooks/useAutomation";
import { useSession } from "../hooks/useSession";
import { formatMoment } from "./Automation";

/**
 * Containers automation will not touch.
 *
 * # A pause is HarborMaster refusing to keep trying
 *
 * Three ways one appears: repeated failures reached a policy's threshold, an
 * update was rolled back, or an operator paused it by hand. The first two are
 * the system protecting the estate from a bad image; the page says which,
 * because "why is this here" is the first question.
 *
 * # Resuming is not a formality
 *
 * It records who cleared it and resets the container's failure counters — an
 * operator who investigated and fixed the problem must not be one failure away
 * from the same pause. The confirmation says so.
 */
export function AutomationPaused() {
  const [showCleared, setShowCleared] = useState(false);
  const pauses = useAutomationPauses(showCleared);

  return (
    <div className="space-y-6">
      <PageIntro
        title="Paused containers"
        description={
          "Containers the update engine will not touch, and why. A pause after " +
          "an automatic rollback never expires on its own: the change was wrong " +
          "and the host was moved twice."
        }
      />

      <div className="flex flex-wrap items-center justify-between gap-2">
        <label className="flex items-center gap-2 text-sm text-content-muted">
          <input
            type="checkbox"
            checked={showCleared}
            onChange={(event) => setShowCleared(event.target.checked)}
          />
          Show pauses that have been cleared
        </label>
      </div>

      <PauseByHand onPaused={pauses.refresh} />

      <PauseList state={pauses} onChanged={pauses.refresh} />
    </div>
  );
}

function PauseList({
  state,
  onChanged,
}: {
  state: ReturnType<typeof useAutomationPauses>;
  onChanged: () => void;
}) {
  if (state.status === "loading") return <LoadingState label="Loading pauses" />;
  if (state.status === "disconnected") {
    return <DisconnectedState onRetry={state.refresh} />;
  }
  if (state.error) return <ErrorState error={state.error} onRetry={state.refresh} />;

  const items = state.data?.items ?? [];
  if (items.length === 0) {
    return (
      <EmptyState
        title="Nothing is paused"
        description="Automation is not refusing to touch anything."
      />
    );
  }

  return (
    <div className="space-y-3">
      {items.map((pause) => (
        <PauseCard
          key={`${pause.containerName}-${pause.pausedAt}`}
          pause={pause}
          onChanged={onChanged}
        />
      ))}
    </div>
  );
}

function PauseCard({
  pause,
  onChanged,
}: {
  pause: PausedContainer;
  onChanged: () => void;
}) {
  const session = useSession();
  const resume = useResumeAutomation();

  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  const mayResume = Boolean(session.user?.permissions.includes("automation:pause"));
  const active = isPauseActive(pause, new Date());

  const clear = useCallback(async () => {
    setBusy(true);
    setFailure(null);
    try {
      await resume(pause.containerName);
      setConfirming(false);
      onChanged();
    } catch (error) {
      setFailure(
        error instanceof Error ? error.message : "The pause could not be cleared",
      );
    } finally {
      setBusy(false);
    }
  }, [onChanged, pause.containerName, resume]);

  return (
    <article className="space-y-3 rounded-xl border border-border-subtle bg-surface-raised px-4 py-3">
      <header className="flex flex-wrap items-start justify-between gap-2">
        <div>
          <h3 className="text-sm font-semibold">{pause.containerName}</h3>
          <p className="mt-1 text-sm text-content-muted">
            {pause.detail || "No further detail was recorded."}
          </p>
        </div>
        <PauseReasonBadge reason={pause.reason} />
      </header>

      <dl className="grid gap-2 text-sm sm:grid-cols-3">
        <Detail label="Paused" value={formatMoment(pause.pausedAt)} />
        <Detail
          label="Failures counted"
          value={String(pause.failures ?? 0)}
        />
        <Detail
          label="Clears"
          value={
            pause.acknowledgedAt
              ? `${formatMoment(pause.acknowledgedAt)} by ${
                  pause.acknowledgedBy?.username ?? "an unrecorded account"
                }`
              : pause.resumeAfter
                ? `automatically ${formatMoment(pause.resumeAfter)}`
                : "only when a person clears it"
          }
        />
      </dl>

      {mayResume && active ? (
        <div className="space-y-2">
          {confirming ? (
            <>
              <p className="text-sm text-content-muted">
                Clearing this resets <strong>{pause.containerName}</strong>&rsquo;s
                failure count to zero. The next pass may update it again.
              </p>
              <div className="flex gap-2">
                <button
                  type="button"
                  className="rounded-lg border border-warn/40 bg-warn-soft px-2.5 py-1 text-xs font-medium text-warn disabled:opacity-50"
                  disabled={busy}
                  onClick={() => void clear()}
                >
                  {busy ? "Clearing…" : "Yes, resume automation"}
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
            </>
          ) : (
            <button
              type="button"
              className="rounded-lg border border-border-subtle bg-surface px-2.5 py-1 text-xs font-medium"
              onClick={() => setConfirming(true)}
            >
              Resume
            </button>
          )}
          {failure ? (
            <p role="alert" className="text-xs text-danger">
              {failure}
            </p>
          ) : null}
        </div>
      ) : null}
    </article>
  );
}

function Detail({ label, value }: { label: string; value: string }) {
  return (
    <div>
      <dt className="text-xs uppercase tracking-wide text-content-muted">{label}</dt>
      <dd className="mt-0.5">{value}</dd>
    </div>
  );
}

/**
 * Pausing one container by hand.
 *
 * The container must be one the inventory already knows: a pause on a container
 * that does not exist is a safety control an operator believes in that protects
 * nothing, and the server refuses it.
 */
function PauseByHand({ onPaused }: { onPaused: () => void }) {
  const session = useSession();
  const pause = usePauseAutomation();

  const [name, setName] = useState("");
  const [reason, setReason] = useState("");
  const [busy, setBusy] = useState(false);
  const [failure, setFailure] = useState<string | null>(null);

  const mayPause = Boolean(session.user?.permissions.includes("automation:pause"));

  const submit = useCallback(async () => {
    setBusy(true);
    setFailure(null);
    try {
      await pause(name.trim(), reason.trim());
      setName("");
      setReason("");
      onPaused();
    } catch (error) {
      setFailure(
        error instanceof Error ? error.message : "The container could not be paused",
      );
    } finally {
      setBusy(false);
    }
  }, [name, onPaused, pause, reason]);

  if (!mayPause) return null;

  return (
    <section className="space-y-3 rounded-xl border border-border-subtle bg-surface-raised px-4 py-3">
      <div>
        <h3 className="text-sm font-semibold">Pause a container</h3>
        <p className="mt-1 max-w-prose text-sm text-content-muted">
          Stops automation touching it. Changes nothing on the host, and can be
          cleared at any time.
        </p>
      </div>

      <div className="flex flex-wrap gap-2">
        <input
          className="rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
          value={name}
          onChange={(event) => setName(event.target.value)}
          placeholder="Container name"
          aria-label="Container name"
        />
        <input
          className="min-w-64 flex-1 rounded-lg border border-border-subtle bg-surface px-2 py-1 text-sm"
          value={reason}
          onChange={(event) => setReason(event.target.value)}
          placeholder="Why (optional)"
          aria-label="Reason"
          maxLength={500}
        />
        <button
          type="button"
          className="rounded-lg border border-border-subtle bg-surface px-3 py-1.5 text-sm font-medium disabled:opacity-50"
          disabled={busy || name.trim() === ""}
          onClick={() => void submit()}
        >
          {busy ? "Pausing…" : "Pause"}
        </button>
      </div>

      {failure ? (
        <p role="alert" className="text-sm text-danger">
          {failure}
        </p>
      ) : null}
    </section>
  );
}
