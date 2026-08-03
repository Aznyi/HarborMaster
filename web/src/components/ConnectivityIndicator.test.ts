import { describe, expect, it } from "vitest";

import { ApiError } from "../api/client";
import type { HealthReport } from "../api/types";
import type { ResourceState } from "../hooks/useApiResource";
import { deriveIndicators } from "./ConnectivityIndicator";

function state(overrides: Partial<ResourceState<HealthReport>>): ResourceState<HealthReport> {
  return {
    status: "ready",
    data: null,
    error: null,
    refreshing: false,
    refresh: () => {},
    ...overrides,
  };
}

const report: HealthReport = {
  status: "healthy",
  database: { status: "up" },
  docker: { status: "up", version: "1.51" },
  checkedAt: "2026-08-03T09:20:11.482Z",
  uptimeSeconds: 10,
};

describe("deriveIndicators", () => {
  it("reports both as checking while loading", () => {
    const { backend, docker } = deriveIndicators(state({ status: "loading" }));

    expect(backend.tone).toBe("neutral");
    expect(docker.tone).toBe("neutral");
  });

  it("reports both connected when the report is healthy", () => {
    const { backend, docker } = deriveIndicators(state({ data: report }));

    expect(backend.label).toMatch(/connected/i);
    expect(docker.label).toMatch(/connected/i);
    expect(docker.tone).toBe("ok");
  });

  // Docker down is a warning, not a failure: the backend is still answering.
  it("warns rather than errors when only Docker is down", () => {
    const degraded: HealthReport = {
      ...report,
      status: "degraded",
      docker: { status: "down", detail: "docker engine unreachable" },
    };

    const { backend, docker } = deriveIndicators(state({ data: degraded }));

    expect(backend.tone).toBe("ok");
    expect(docker.tone).toBe("warn");
    expect(docker.title).toBe("docker engine unreachable");
  });

  it("marks Docker unknown when the backend is unreachable", () => {
    const { backend, docker } = deriveIndicators(
      state({
        status: "disconnected",
        data: null,
        error: new ApiError("network_error", "Cannot reach the HarborMaster backend"),
      }),
    );

    expect(backend.tone).toBe("danger");
    expect(backend.label).toMatch(/unreachable/i);
    // Docker status is only knowable through the backend.
    expect(docker.label).toMatch(/unknown/i);
    expect(docker.tone).toBe("neutral");
  });
});
