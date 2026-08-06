import { StatusBadge, type BadgeTone } from "./StatusBadge";
import type {
  DockerEventType,
  EventConnectionState,
  EventProcessingResult,
  StreamStatus,
} from "../api/eventTypes";

/**
 * Badges for the event views.
 *
 * Tone is never the only signal: every badge carries a label saying the same
 * thing, so the meaning survives for anyone who cannot distinguish the colours.
 */

/**
 * The processing outcome.
 *
 * "warning" is warn rather than danger deliberately: it means HarborMaster
 * could not map the event onto one resource and reconciled instead, which is a
 * conservative success, not a failure.
 */
const resultTones: Record<EventProcessingResult, BadgeTone> = {
  processed: "ok",
  deduplicated: "neutral",
  ignored: "neutral",
  warning: "warn",
  failed: "danger",
};

export function EventResultBadge({ result }: { result: EventProcessingResult }) {
  // A result the client does not know is rendered neutrally rather than
  // crashing the row: the server's vocabulary can gain a member before the
  // bundle is rebuilt.
  const tone = resultTones[result] ?? "neutral";

  return <StatusBadge tone={tone} label={result} title={describeResult(result)} />;
}

function describeResult(result: EventProcessingResult): string {
  switch (result) {
    case "processed":
      return "Recorded, and a refresh was requested where the event called for one.";
    case "deduplicated":
      return "An identical event was already seen; it did no extra work.";
    case "ignored":
      return "Recorded, but this event has no inventory consequence.";
    case "warning":
      return "Recorded, but it could not be mapped to one resource, so a full reconciliation was requested.";
    case "failed":
      return "Processing raised an error. The event is still stored.";
  }
}

/** The kind of object an event describes. Informational, so always neutral. */
export function EventTypeBadge({ type }: { type: DockerEventType }) {
  return <StatusBadge tone="neutral" label={type} />;
}

/** The event engine's connection state, as the server reports it. */
const connectionTones: Record<EventConnectionState, BadgeTone> = {
  connected: "ok",
  // Disabled is neutral, not a fault: running on periodic reconciliation alone
  // is a supported configuration.
  disabled: "neutral",
  stopped: "neutral",
  connecting: "warn",
  reconnecting: "warn",
};

export function ConnectionStateBadge({ state }: { state: EventConnectionState }) {
  const tone = connectionTones[state] ?? "neutral";

  return <StatusBadge tone={tone} label={state} title={describeConnection(state)} />;
}

function describeConnection(state: EventConnectionState): string {
  switch (state) {
    case "connected":
      return "The Docker event stream is live.";
    case "connecting":
      return "Opening the Docker event stream.";
    case "reconnecting":
      return "The stream dropped. Reconnecting; the inventory is being kept current by periodic reconciliation meanwhile.";
    case "disabled":
      return "The event engine is switched off by configuration. The inventory refreshes on its own schedule.";
    case "stopped":
      return "The event engine has shut down.";
  }
}

/** The BROWSER's connection to the SSE endpoint, which is a separate fact. */
const streamTones: Record<StreamStatus, BadgeTone> = {
  open: "ok",
  connecting: "warn",
  reconnecting: "warn",
  idle: "neutral",
  unavailable: "neutral",
  signedOut: "danger",
};

const streamLabels: Record<StreamStatus, string> = {
  open: "live",
  connecting: "connecting",
  reconnecting: "reconnecting",
  idle: "not connected",
  unavailable: "unavailable",
  signedOut: "signed out",
};

export function StreamStatusBadge({ status }: { status: StreamStatus }) {
  const tone = streamTones[status] ?? "neutral";
  const label = streamLabels[status] ?? status;

  return <StatusBadge tone={tone} label={label} title={describeStream(status)} />;
}

function describeStream(status: StreamStatus): string {
  switch (status) {
    case "open":
      return "This browser is receiving events as they happen.";
    case "connecting":
      return "Opening the live stream.";
    case "reconnecting":
      return "The live stream dropped. The browser is retrying on its own; history below is still accurate.";
    case "idle":
      return "The live stream is not connected.";
    case "signedOut":
      return "The live stream ended because this session is no longer valid. Sign in again.";
    case "unavailable":
      return "Live updates are unavailable. The history below is still accurate.";
  }
}
