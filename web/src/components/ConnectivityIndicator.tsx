import type { ResourceState } from "../hooks/useApiResource";
import type { HealthReport } from "../api/types";
import { StatusBadge, type BadgeTone } from "./StatusBadge";

interface Indicator {
  tone: BadgeTone;
  label: string;
  title: string;
}

/**
 * Derives the backend and Docker indicators from a health resource.
 *
 * Backend reachability is a property of the request itself, so it is read from
 * the resource status rather than from the payload: if the response arrived,
 * the backend is up by definition.
 */
export function deriveIndicators(health: ResourceState<HealthReport>): {
  backend: Indicator;
  docker: Indicator;
} {
  if (health.status === "loading") {
    return {
      backend: { tone: "neutral", label: "Backend: checking", title: "Contacting the HarborMaster API" },
      docker: { tone: "neutral", label: "Docker: checking", title: "Waiting for the health report" },
    };
  }

  if (health.status === "disconnected" || health.status === "error" || !health.data) {
    const title =
      health.status === "disconnected"
        ? "The HarborMaster API did not respond"
        : (health.error?.message ?? "The health report could not be read");
    return {
      backend: { tone: "danger", label: "Backend: unreachable", title },
      docker: {
        tone: "neutral",
        label: "Docker: unknown",
        title: "Docker status is reported by the backend, which is unreachable",
      },
    };
  }

  const report = health.data;
  const dockerUp = report.docker.status === "up";

  return {
    backend: {
      tone: "ok",
      label: "Backend: connected",
      title: `Database ${report.database.status}, checked ${report.checkedAt}`,
    },
    docker: {
      tone: dockerUp ? "ok" : "warn",
      label: dockerUp ? "Docker: connected" : "Docker: disconnected",
      title: dockerUp
        ? `Docker Engine API ${report.docker.version ?? "version unknown"}`
        : (report.docker.detail ?? "The Docker socket is unreachable"),
    },
  };
}

/** Live backend and Docker connectivity, rendered in the shell header. */
export function ConnectivityIndicator({
  health,
}: {
  health: ResourceState<HealthReport>;
}) {
  const { backend, docker } = deriveIndicators(health);

  return (
    <div
      className="flex flex-wrap items-center gap-2"
      role="status"
      aria-live="polite"
      aria-label="Connectivity status"
    >
      <StatusBadge tone={backend.tone} label={backend.label} title={backend.title} />
      <StatusBadge tone={docker.tone} label={docker.label} title={docker.title} />
    </div>
  );
}
