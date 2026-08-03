import type { ContainerState, HealthState } from "../api/inventoryTypes";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

/**
 * Maps a container state onto a badge tone.
 *
 * Colour is never the only signal -- the label always spells out the state --
 * so this is emphasis, not information.
 */
export function stateTone(state: ContainerState): BadgeTone {
  switch (state) {
    case "running":
      return "ok";
    case "paused":
    case "restarting":
    case "created":
    case "removing":
      return "warn";
    case "dead":
      return "danger";
    case "exited":
    case "unknown":
    default:
      return "neutral";
  }
}

export function healthTone(health: HealthState): BadgeTone {
  switch (health) {
    case "healthy":
      return "ok";
    case "starting":
      return "warn";
    case "unhealthy":
      return "danger";
    case "none":
    default:
      return "neutral";
  }
}

export function ContainerStateBadge({ state }: { state: ContainerState }) {
  return <StatusBadge tone={stateTone(state)} label={state} />;
}

/**
 * Health badge. A container with no healthcheck renders nothing rather than a
 * "none" badge: an empty cell reads as "not configured", while a badge would
 * imply a verdict was reached.
 */
export function ContainerHealthBadge({ health }: { health: HealthState }) {
  if (health === "none") {
    return <span className="text-xs text-content-muted">&mdash;</span>;
  }
  return <StatusBadge tone={healthTone(health)} label={health} />;
}
