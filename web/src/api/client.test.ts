import { beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError, API_BASE, getHealth, getVersion } from "./client";
import type { HealthReport } from "./types";

function jsonResponse(body: unknown, init: ResponseInit = {}): Response {
  return new Response(JSON.stringify(body), {
    status: 200,
    headers: { "Content-Type": "application/json" },
    ...init,
  });
}

const healthyReport: HealthReport = {
  status: "healthy",
  database: { status: "up", latencyMs: 1 },
  docker: { status: "up", latencyMs: 4, version: "1.51" },
  checkedAt: "2026-08-03T09:20:11.482Z",
  uptimeSeconds: 42,
};

describe("api client", () => {
  beforeEach(() => {
    vi.stubGlobal("fetch", vi.fn());
  });

  it("requests the versioned API path", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(healthyReport));

    await getHealth();

    expect(fetch).toHaveBeenCalledWith(
      `${API_BASE}/health`,
      expect.objectContaining({ method: "GET" }),
    );
  });

  it("decodes a health report", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(healthyReport));

    const report = await getHealth();

    expect(report.status).toBe("healthy");
    expect(report.docker.version).toBe("1.51");
  });

  it("decodes version information", async () => {
    vi.mocked(fetch).mockResolvedValue(
      jsonResponse({
        version: "v0.1.0",
        commit: "9f2c1ab",
        buildDate: "2026-08-03T08:00:00Z",
        goVersion: "go1.26.5",
        platform: "linux/amd64",
      }),
    );

    const info = await getVersion();

    expect(info.version).toBe("v0.1.0");
    expect(info.platform).toBe("linux/amd64");
  });

  // A failed fetch means the backend is unreachable, which the UI renders
  // differently from a server-side error.
  it("reports an unreachable backend as a connectivity error", async () => {
    vi.mocked(fetch).mockRejectedValue(new TypeError("Failed to fetch"));

    const error = await getHealth().catch((caught: unknown) => caught);

    expect(error).toBeInstanceOf(ApiError);
    expect((error as ApiError).code).toBe("network_error");
    expect((error as ApiError).isConnectivity).toBe(true);
  });

  it("surfaces the server's error code and request id", async () => {
    vi.mocked(fetch).mockResolvedValue(
      new Response(
        JSON.stringify({
          error: { code: "not_found", message: "endpoint not found" },
          requestId: "abc123",
        }),
        { status: 404, headers: { "Content-Type": "application/json" } },
      ),
    );

    const error = (await getHealth().catch((caught: unknown) => caught)) as ApiError;

    expect(error).toBeInstanceOf(ApiError);
    expect(error.code).toBe("not_found");
    expect(error.status).toBe(404);
    expect(error.requestId).toBe("abc123");
    expect(error.isConnectivity).toBe(false);
  });

  // A reverse proxy can return an HTML error page; the client must not crash.
  it("handles a non-JSON error body", async () => {
    vi.mocked(fetch).mockResolvedValue(
      new Response("<html>502 Bad Gateway</html>", { status: 502 }),
    );

    const error = (await getHealth().catch((caught: unknown) => caught)) as ApiError;

    expect(error).toBeInstanceOf(ApiError);
    expect(error.code).toBe("internal_error");
    expect(error.status).toBe(502);
  });

  it("handles a malformed success body", async () => {
    vi.mocked(fetch).mockResolvedValue(
      new Response("not json", {
        status: 200,
        headers: { "Content-Type": "application/json" },
      }),
    );

    const error = (await getHealth().catch((caught: unknown) => caught)) as ApiError;

    expect(error.code).toBe("invalid_response");
  });

  // The API is unauthenticated; attaching cookies to a dev proxy would be a
  // silent way to start sending credentials.
  // The session cookie must travel with every request, and only to this
  // origin. "include" would attach it to a cross-origin dev proxy; "omit"
  // would mean nothing could ever be authenticated.
  it("sends the session cookie to this origin only", async () => {
    vi.mocked(fetch).mockResolvedValue(jsonResponse(healthyReport));

    await getHealth();

    expect(fetch).toHaveBeenCalledWith(
      expect.any(String),
      expect.objectContaining({ credentials: "same-origin" }),
    );
  });

  it("aborts when the caller's signal fires", async () => {
    const controller = new AbortController();
    vi.mocked(fetch).mockImplementation(() => {
      controller.abort();
      return Promise.reject(new DOMException("Aborted", "AbortError"));
    });

    const error = (await getHealth({ signal: controller.signal }).catch(
      (caught: unknown) => caught,
    )) as ApiError;

    expect(error.code).toBe("network_error");
    expect(error.message).toBe("Request cancelled");
  });
});
