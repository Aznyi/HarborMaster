import type { ReactNode } from "react";

import type { ApiError } from "../api/client";

/**
 * The shared loading, empty, disconnected, and error presentations.
 *
 * Every API-backed view renders one of these rather than inventing its own, so
 * the four states look and read the same everywhere in the app.
 */

interface PanelProps {
  title: string;
  description?: ReactNode;
  action?: ReactNode;
  tone?: "neutral" | "warn" | "danger";
  icon?: ReactNode;
}

const toneClasses: Record<NonNullable<PanelProps["tone"]>, string> = {
  neutral: "border-border-subtle bg-surface-raised",
  warn: "border-warn/40 bg-warn-soft",
  danger: "border-danger/40 bg-danger-soft",
};

function Panel({ title, description, action, tone = "neutral", icon }: PanelProps) {
  return (
    <div
      className={`flex flex-col items-center justify-center gap-3 rounded-xl border px-6 py-12 text-center ${toneClasses[tone]}`}
    >
      {icon ? <div aria-hidden="true">{icon}</div> : null}
      <h3 className="text-base font-semibold text-content">{title}</h3>
      {description ? (
        <p className="max-w-prose text-sm text-content-muted">{description}</p>
      ) : null}
      {action}
    </div>
  );
}

/** Skeleton shown while a resource loads for the first time. */
export function LoadingState({ label = "Loading" }: { label?: string }) {
  return (
    <div
      role="status"
      aria-live="polite"
      aria-busy="true"
      // role="status" does not take its name from content, so the label has to
      // be explicit for it to reach assistive technology.
      aria-label={label}
      className="flex flex-col gap-3 rounded-xl border border-border-subtle bg-surface-raised p-6"
    >
      <span className="sr-only">{label}</span>
      <div className="h-4 w-1/3 animate-pulse rounded bg-surface-sunken" />
      <div className="h-4 w-2/3 animate-pulse rounded bg-surface-sunken" />
      <div className="h-4 w-1/2 animate-pulse rounded bg-surface-sunken" />
    </div>
  );
}

/** Shown when a request succeeded but there is nothing to display. */
export function EmptyState({
  title,
  description,
  action,
}: {
  title: string;
  description?: ReactNode;
  action?: ReactNode;
}) {
  const props: PanelProps = { title, tone: "neutral" };
  if (description !== undefined) props.description = description;
  if (action !== undefined) props.action = action;
  return <Panel {...props} />;
}

/**
 * Shown when the backend could not be reached at all.
 *
 * Distinct from ErrorState because the remedy is different: check that the
 * server is running, not that the request was valid.
 */
export function DisconnectedState({ onRetry }: { onRetry?: () => void }) {
  return (
    <div role="alert">
      <Panel
        tone="warn"
        title="Cannot reach the HarborMaster backend"
        description="The API did not respond. Check that the harbormaster server is running and reachable at this address."
        action={onRetry ? <RetryButton onRetry={onRetry} /> : undefined}
      />
    </div>
  );
}

/** Shown when the backend answered with an error. */
export function ErrorState({
  error,
  onRetry,
}: {
  error: ApiError;
  onRetry?: () => void;
}) {
  return (
    <div role="alert">
      <Panel
        tone="danger"
        title="Something went wrong"
        description={
          <>
            {error.message}
            {error.requestId ? (
              <span className="mt-2 block font-mono text-xs text-content-muted">
                Request ID: {error.requestId}
              </span>
            ) : null}
          </>
        }
        action={onRetry ? <RetryButton onRetry={onRetry} /> : undefined}
      />
    </div>
  );
}

function RetryButton({ onRetry }: { onRetry: () => void }) {
  return (
    <button
      type="button"
      onClick={onRetry}
      className="rounded-lg border border-border-subtle bg-surface-raised px-4 py-2 text-sm font-medium text-content transition-colors hover:bg-surface-sunken"
    >
      Try again
    </button>
  );
}

/**
 * The empty state for pages whose endpoints do not exist yet.
 *
 * These pages deliberately render nothing rather than sample rows: placeholder
 * data in an operations tool is indistinguishable from real data at a glance,
 * and that is a dangerous thing to show next to a container list.
 */
export function NotYetAvailableState({ feature }: { feature: string }) {
  return (
    <EmptyState
      title={`${feature} are not available yet`}
      description={
        <>
          HarborMaster currently provides the read-only foundation: Docker
          connectivity, the snapshot store, and this shell. {feature} will
          appear here as soon as the API serves them. Nothing is shown until
          then, because sample data would be indistinguishable from the real
          thing.
        </>
      }
    />
  );
}
