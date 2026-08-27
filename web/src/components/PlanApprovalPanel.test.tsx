import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { ChangePlan } from "../api/planTypes";
import { TestSessionProvider, testSession, testUser } from "../test/session";
import { PlanApprovalPanel } from "./PlanApprovalPanel";

/**
 * The manual-review approval panel.
 *
 * # The properties that matter
 *
 *   - it appears ONLY for a plan that asks for review. Offering the control
 *     anywhere else would imply the other plans need one;
 *   - the wording never suggests an override. "Force", "Ignore warnings" and
 *     "Run update" would all describe something this button does not do;
 *   - approving records a review. It does not apply the update, so nothing here
 *     asks for an image, a digest, or a container.
 */

const originalFetch = globalThis.fetch;

let requests: { url: string; method: string; body: string }[] = [];
let respond: (url: string, method: string) => Response;

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function plan(overrides: Partial<ChangePlan> = {}): ChangePlan {
  return {
    planId: "plan_00112233445566778899",
    containerId: "c1",
    containerName: "web",
    currentImage: "nginx:latest",
    proposedImage: "nginx:latest",
    currentDigest: "sha256:" + "a".repeat(64),
    proposedDigest: "sha256:" + "b".repeat(64),
    updateType: "digest",
    snapshotAvailable: true,
    restoreReadiness: "ready",
    driftOpen: 0,
    policyOpen: 0,
    registryStatus: "ok",
    planVersion: 1,
    generatedAt: "2026-03-01T03:00:00Z",
    risk: {
      score: 54,
      band: "high",
      recommendation: "manualReview",
      summary: "Worth a look first",
      factors: [],
    },
    ...overrides,
  } as ChangePlan;
}

const approval = {
  approval: {
    planId: "plan_00112233445566778899",
    state: "active",
    approvedBy: { userId: "usr_1", username: "colby" },
    approvedAt: "2026-03-01T03:05:00Z",
  },
  valid: true,
};

function renderPanel(role: "viewer" | "operator" = "operator", p = plan()) {
  return render(
    <TestSessionProvider session={testSession({ user: testUser(role) })}>
      <PlanApprovalPanel plan={p} />
    </TestSessionProvider>,
  );
}

beforeEach(() => {
  requests = [];
  respond = () => jsonResponse({ error: { code: "notFound" } }, 404);

  globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    const method = init?.method ?? "GET";
    requests.push({
      url,
      method,
      body: typeof init?.body === "string" ? init.body : "",
    });
    return respond(url, method);
  }) as typeof fetch;
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

describe("the plan approval panel", () => {
  it("appears only for a plan that asks for review", () => {
    for (const recommendation of ["proceed", "proceedWithCaution", "notRecommended", "unknown"] as const) {
      const { unmount } = renderPanel(
        "operator",
        plan({ risk: { ...plan().risk, recommendation } }),
      );
      expect(screen.queryByText("Manual review required")).not.toBeInTheDocument();
      unmount();
    }
  });

  it("asks for a review, and never for an override", async () => {
    renderPanel();

    expect(await screen.findByText("Manual review required")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Approve this exact update" }),
    ).toBeInTheDocument();

    // The words this control must never use.
    expect(document.body.textContent).not.toMatch(/force|override|ignore warnings|run update/i);
  });

  it("records a review without applying anything", async () => {
    respond = (_url, method) =>
      method === "POST"
        ? jsonResponse(approval, 201)
        : jsonResponse({ error: { code: "notFound" } }, 404);

    renderPanel();
    await userEvent.click(
      await screen.findByRole("button", { name: "Approve this exact update" }),
    );

    await screen.findByTestId("approval-granted");
    expect(screen.getByText(/Approved by colby/)).toBeInTheDocument();
    expect(
      screen.getByText(/still run every normal safety check/i),
    ).toBeInTheDocument();

    const post = requests.find((r) => r.method === "POST");
    expect(post).toBeTruthy();
    expect(post!.url).toContain("/plan-approvals/plan_00112233445566778899");
    // NO body: every fact about the change is read from the plan the URL names.
    expect(post!.body).toBe("");

    // Nothing was acquired or executed.
    expect(requests.some((r) => r.url.includes("/acquisitions"))).toBe(false);
    expect(requests.some((r) => r.url.includes("/executions"))).toBe(false);
  });

  it("says when a standing approval no longer applies", async () => {
    respond = () =>
      jsonResponse({
        ...approval,
        valid: false,
        refusal: "superseded",
        explanation: "a newer change plan has replaced the one that was approved",
      });

    renderPanel();
    const stale = await screen.findByTestId("approval-stale");
    expect(stale.textContent).toContain("A newer plan replaced this one");
    expect(stale.textContent).toContain("a newer change plan has replaced");
    // The raw enum never reaches the DOM.
    expect(stale.textContent).not.toContain("superseded");
  });

  it("does not render a failed check as unapproved", async () => {
    respond = () => jsonResponse({ error: { code: "internal" } }, 500);

    renderPanel();
    await waitFor(() =>
      expect(
        screen.getByText(/could not check whether this plan has been reviewed/i),
      ).toBeInTheDocument(),
    );
  });

  it("offers a viewer no control", async () => {
    renderPanel("viewer");

    expect(await screen.findByText("Manual review required")).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: "Approve this exact update" }),
    ).not.toBeInTheDocument();
    expect(screen.getByText(/needs the plan approval permission/i)).toBeInTheDocument();
  });

  it("has no serious or critical axe findings", async () => {
    respond = () => jsonResponse(approval);
    renderPanel();
    await screen.findByTestId("approval-granted");

    const results = await axe.run(document.body, {
      resultTypes: ["violations"],
      runOnly: { type: "tag", values: ["wcag2a", "wcag2aa"] },
    });
    const serious = results.violations.filter(
      (violation) => violation.impact === "serious" || violation.impact === "critical",
    );
    expect(serious.map((violation) => `${violation.id}: ${violation.help}`)).toEqual([]);
  });
});
