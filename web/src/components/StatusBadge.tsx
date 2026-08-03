import type { ComponentStatus, OverallStatus } from "../api/types";

export type BadgeTone = "ok" | "warn" | "danger" | "neutral";

const toneClasses: Record<BadgeTone, string> = {
  ok: "border-ok/40 bg-ok-soft text-ok",
  warn: "border-warn/40 bg-warn-soft text-warn",
  danger: "border-danger/40 bg-danger-soft text-danger",
  neutral: "border-border-subtle bg-surface-sunken text-content-muted",
};

const dotClasses: Record<BadgeTone, string> = {
  ok: "bg-ok",
  warn: "bg-warn",
  danger: "bg-danger",
  neutral: "bg-content-muted",
};

/**
 * A small status pill.
 *
 * Colour is never the only signal: the label carries the same information for
 * anyone who cannot distinguish the tones.
 */
export function StatusBadge({
  tone,
  label,
  title,
}: {
  tone: BadgeTone;
  label: string;
  title?: string;
}) {
  return (
    <span
      className={`inline-flex items-center gap-1.5 rounded-full border px-2.5 py-1 text-xs font-medium ${toneClasses[tone]}`}
      title={title ?? label}
    >
      <span
        aria-hidden="true"
        className={`size-1.5 rounded-full ${dotClasses[tone]}`}
      />
      {label}
    </span>
  );
}

/** Maps an overall health verdict onto a badge tone. */
export function overallTone(status: OverallStatus): BadgeTone {
  switch (status) {
    case "healthy":
      return "ok";
    case "degraded":
      return "warn";
    case "unhealthy":
      return "danger";
  }
}

/** Maps a single dependency's status onto a badge tone. */
export function componentTone(status: ComponentStatus): BadgeTone {
  return status === "up" ? "ok" : "danger";
}
