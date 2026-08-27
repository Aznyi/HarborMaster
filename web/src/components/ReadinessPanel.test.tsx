import { render, screen, waitFor, within } from "@testing-library/react";
import axe from "axe-core";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type { AutomationReadinessRequest } from "../api/automationReadiness";
import { ReadinessPanel } from "./ReadinessPanel";

/**
 * The readiness panel.
 *
 * # What these defend
 *
 * One property matters more than the rest: **a failed request must never render
 * as zero**. "0 eligible" and "HarborMaster could not check" are opposite
 * messages -- the first says the policy governs nothing, the second says
 * nothing is known -- and collapsing them would send an operator to rewrite a
 * selector that was never the problem.
 *
 * The rest is about not lying in the other direction: the panel reports a
 * reading of the present, never a promise about a future pass.
 */

const originalFetch = globalThis.fetch;

let posted: { url: string; body: Record<string, unknown> }[] = [];
let respond: () => Response;

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function report(overrides: Record<string, unknown> = {}) {
  return {
    readiness: {
      evaluatedAt: "2026-03-01T03:00:00Z",
      truncated: false,
      considered: 40,
      governed: 12,
      eligible: 3,
      observing: 0,
      awaitingApproval: 1,
      groups: [
        {
          reason: "noUpdate",
          explanation: "the current change plan proposes no change",
          count: 6,
        },
        {
          reason: "dependencyWaiting",
          explanation: "something it depends on is being updated first",
          count: 2,
        },
      ],
      ...overrides,
    },
    engineEnabled: true,
  };
}

const request: AutomationReadinessRequest = {
  strategy: "digestOnly",
  mode: "automatic",
  scope: "allEligible",
};

beforeEach(() => {
  vi.useFakeTimers({ shouldAdvanceTime: true });
  posted = [];
  respond = () => jsonResponse(report());

  globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    posted.push({
      url: String(input),
      body: init?.body
        ? (JSON.parse(String(init.body)) as Record<string, unknown>)
        : {},
    });
    return respond();
  }) as typeof fetch;
});

afterEach(() => {
  vi.useRealTimers();
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

describe("the readiness panel", () => {
  it("reports the count as a reading rather than a promise", async () => {
    render(<ReadinessPanel request={request} />);

    const headline = await screen.findByTestId("readiness-headline");
    expect(headline).toHaveTextContent("3 containers are currently eligible");

    // The framing, which is the whole point of the wording.
    expect(screen.getByText("Based on current assessment")).toBeInTheDocument();
    expect(
      screen.getByText(/not a prediction/i),
    ).toBeInTheDocument();
    // And never a promise.
    expect(document.body.textContent).not.toMatch(/will update/i);
  });

  it("explains the containers it did not count", async () => {
    render(<ReadinessPanel request={request} />);

    const groups = await screen.findByTestId("readiness-groups");
    const items = within(groups).getAllByRole("listitem");
    expect(items).toHaveLength(2);

    // The LABEL from the closed vocabulary, plus HarborMaster's own sentence.
    // A raw enum must never reach the DOM.
    expect(groups.textContent).toContain("the current change plan proposes no change");
    expect(groups.textContent).not.toContain("noUpdate");
    expect(groups.textContent).not.toContain("dependencyWaiting");
  });

  it("does not render a failed check as zero", async () => {
    respond = () => jsonResponse({ error: { code: "internal" } }, 500);
    render(<ReadinessPanel request={request} />);

    await waitFor(() =>
      expect(screen.getByText(/could not check the estate/i)).toBeInTheDocument(),
    );

    // The distinction that matters: no count at all, rather than a zero.
    expect(screen.queryByTestId("readiness-headline")).not.toBeInTheDocument();
    expect(document.body.textContent).not.toMatch(/0 containers/);
    expect(screen.getByText(/says nothing about how many/i)).toBeInTheDocument();
  });

  it("says when a policy governs nothing, which is different from an error", async () => {
    respond = () => jsonResponse(report({ governed: 0, eligible: 0, groups: [] }));
    render(<ReadinessPanel request={request} />);

    const headline = await screen.findByTestId("readiness-headline");
    expect(headline).toHaveTextContent("does not currently govern any container");
    expect(screen.queryByText(/could not check the estate/i)).not.toBeInTheDocument();
  });

  it("distinguishes eligible-but-engine-off from eligible", async () => {
    respond = () =>
      jsonResponse({ ...report(), engineEnabled: false });
    render(<ReadinessPanel request={request} />);

    const headline = await screen.findByTestId("readiness-headline");
    expect(headline).toHaveTextContent(/update engine is switched off/i);
  });

  it("says when the estate was truncated", async () => {
    respond = () => jsonResponse(report({ truncated: true }));
    render(<ReadinessPanel request={request} />);

    await waitFor(() =>
      expect(
        screen.getByText(/part of the estate rather than all of it/i),
      ).toBeInTheDocument(),
    );
  });

  it("shows a loading state that is not a count", async () => {
    let release: (value: Response) => void = () => {};
    globalThis.fetch = vi.fn(
      () => new Promise<Response>((resolve) => (release = resolve)),
    ) as typeof fetch;

    render(<ReadinessPanel request={request} />);

    await waitFor(() =>
      expect(screen.getByText(/checking the estate/i)).toBeInTheDocument(),
    );
    expect(screen.queryByTestId("readiness-headline")).not.toBeInTheDocument();

    release(jsonResponse(report()));
    await screen.findByTestId("readiness-headline");
  });

  it("asks once for a burst of edits", async () => {
    const { rerender } = render(<ReadinessPanel request={request} />);

    // Five edits in quick succession, as an operator typing would produce.
    for (const priority of [1, 2, 3, 4, 5]) {
      rerender(<ReadinessPanel request={{ ...request, priority }} />);
    }

    await screen.findByTestId("readiness-headline");

    // The debounce collapsed them: the estate is not re-measured per keystroke.
    expect(posted.length).toBeLessThanOrEqual(2);
    expect(posted[posted.length - 1]!.url).toContain("/automation/readiness");
  });

  it("sends the policy configuration and nothing computed", async () => {
    render(<ReadinessPanel request={{ ...request, policyId: "upd_1" }} />);
    await screen.findByTestId("readiness-headline");

    const body = posted[posted.length - 1]!.body;
    expect(body.strategy).toBe("digestOnly");
    expect(body.mode).toBe("automatic");
    expect(body.policyId).toBe("upd_1");

    // The server's facts are absent by construction: the type has nowhere to
    // put them, and this asserts the shape that reaches the wire.
    for (const forbidden of [
      "recommendation",
      "verdict",
      "eligible",
      "updateType",
      "riskScore",
      "dependencyState",
      "snapshotAvailable",
    ]) {
      expect(body).not.toHaveProperty(forbidden);
    }
  });

  it("renders nothing at all when it is not enabled", () => {
    render(<ReadinessPanel request={request} enabled={false} />);
    expect(screen.queryByLabelText("Readiness")).not.toBeInTheDocument();
    expect(posted).toHaveLength(0);
  });

  it("announces politely rather than interrupting", async () => {
    render(<ReadinessPanel request={request} />);
    await screen.findByTestId("readiness-headline");

    const region = screen.getByLabelText("Readiness").querySelector("[aria-live]");
    expect(region).not.toBeNull();
    // Polite, because the panel re-queries as the operator edits. An assertive
    // region would interrupt a screen reader on every change.
    expect(region).toHaveAttribute("aria-live", "polite");
    expect(region).not.toHaveAttribute("role", "alert");
  });

  it("has no serious or critical axe findings", async () => {
    render(<ReadinessPanel request={request} />);
    await screen.findByTestId("readiness-headline");

    const results = await axe.run(document.body, {
      resultTypes: ["violations"],
      runOnly: { type: "tag", values: ["wcag2a", "wcag2aa"] },
    });
    const serious = results.violations.filter(
      (violation) => violation.impact === "serious" || violation.impact === "critical",
    );
    expect(serious.map((violation) => `${violation.id}: ${violation.help}`)).toEqual([]);
  });

  it("keeps the group rows shrinkable and touchable at a narrow width", async () => {
    render(<ReadinessPanel request={request} />);
    const groups = await screen.findByTestId("readiness-groups");

    // Class mechanism rather than measured width: jsdom implements no layout.
    // `min-w-0` is what lets a full explanation sentence wrap instead of
    // forcing the row -- and the page -- sideways at 390px.
    for (const item of within(groups).getAllByRole("listitem")) {
      expect(item.className).toContain("min-h-11");
      expect(item.querySelector("span.min-w-0")).not.toBeNull();
    }
  });
});

describe("the paused group", () => {
  it("offers a way to review paused containers", async () => {
    respond = () =>
      jsonResponse(
        report({
          groups: [
            {
              reason: "automationPaused",
              explanation: "HarborMaster stopped updating this container after a failure",
              count: 1,
            },
          ],
        }),
      );
    render(
      <MemoryRouter>
        <ReadinessPanel request={request} />
      </MemoryRouter>,
    );

    const link = await screen.findByRole("link", { name: "Review paused container" });
    expect(link).toHaveAttribute("href", "/automation/paused");
  });

  it("pluralises the review link", async () => {
    respond = () =>
      jsonResponse(
        report({
          groups: [
            {
              reason: "automationPaused",
              explanation: "HarborMaster stopped updating these containers after failures",
              count: 4,
            },
          ],
        }),
      );
    render(
      <MemoryRouter>
        <ReadinessPanel request={request} />
      </MemoryRouter>,
    );

    expect(
      await screen.findByRole("link", { name: "Review paused containers" }),
    ).toBeInTheDocument();
  });

  it("offers no action for a group that needs none", async () => {
    render(
      <MemoryRouter>
        <ReadinessPanel request={request} />
      </MemoryRouter>,
    );
    await screen.findByTestId("readiness-groups");

    // The default report carries noUpdate and dependencyWaiting. Neither is
    // something a person clears, so neither gets a control: readiness explains,
    // and only the pause needs an operator.
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("never offers the resume itself", async () => {
    respond = () =>
      jsonResponse(
        report({
          groups: [
            {
              reason: "automationPaused",
              explanation: "HarborMaster stopped updating this container after a failure",
              count: 1,
            },
          ],
        }),
      );
    render(
      <MemoryRouter>
        <ReadinessPanel request={request} />
      </MemoryRouter>,
    );
    await screen.findByRole("link", { name: "Review paused container" });

    // Readiness is READ-ONLY. Clearing a safety pause is a deliberate act that
    // belongs on the surface showing why it was paused, never on a preview an
    // operator is reading while editing a policy.
    expect(screen.queryByRole("button", { name: /resume/i })).not.toBeInTheDocument();
  });
});
