import { render, screen, within } from "@testing-library/react";
import axe from "axe-core";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import type { AutomationDecision } from "../api/automationTypes";
import { AUTOMATION_REASON_LABELS } from "../api/automationTypes";
import type {
  ContainerAttention,
  AttentionState,
} from "../api/inventoryTypes";
import { ATTENTION_LABELS, needsOperator } from "./AttentionBadges";
import { DependencyOperationList } from "./DependencyOperations";
import {
  ApprovalDependencyNote,
  CurrentDependencyStatus,
  DecisionReason,
  RebindNote,
  describeRebind,
  rebindNeedsOperator,
} from "./DependencyStatus";
import { operationSummary, rebindMember } from "../test/fixtures";

/**
 * How dependency conditions are said out loud.
 *
 * # The three claims these tests defend
 *
 *  1. **Waiting is not failure.** It gets no failure styling, no alert, and no
 *     wording implying somebody must act. It is the system working.
 *  2. **A rebind is never an image update.** Every sentence about a
 *     reattachment says the version does not move, because "HarborMaster
 *     recreated my container" and "HarborMaster updated my container" are very
 *     different claims and only one is true.
 *  3. **Approved is authorisation, not a bypass.** An approved container
 *     waiting on a dependency has not been released ahead of it.
 */

function decision(overrides: Partial<AutomationDecision> = {}): AutomationDecision {
  return {
    runId: "run_0123456789abcdef0123",
    containerName: "api",
    verdict: "skip",
    reason: "dependencyWaiting",
    position: 0,
    decidedAt: "2026-08-13T09:00:00Z",
    ...overrides,
  };
}

function attention(overrides: Partial<ContainerAttention> = {}): ContainerAttention {
  return {
    state: "upToDate",
    trackingKnown: true,
    awaitingApproval: false,
    automationPaused: false,
    openViolations: 0,
    openDrift: 0,
    ...overrides,
  };
}

function renderIn(node: React.ReactNode) {
  return render(<MemoryRouter>{node}</MemoryRouter>);
}

async function noSeriousViolations(container: HTMLElement) {
  const results = await axe.run(container);
  return results.violations
    .filter((v) => v.impact === "serious" || v.impact === "critical")
    .map((v) => v.id);
}

// ------------------------------------------------------- automation rows --

describe("a decision's reason", () => {
  it("renders no raw enum for any reason", () => {
    for (const reason of Object.keys(AUTOMATION_REASON_LABELS)) {
      const { unmount } = renderIn(
        <DecisionReason
          decision={decision({ reason: reason as AutomationDecision["reason"] })}
        />,
      );
      expect(screen.queryByText(reason)).toBeNull();
      unmount();
    }
  });

  it("states waiting as a fact, not a failure", () => {
    renderIn(
      <DecisionReason
        decision={decision({ reason: "dependencyWaiting", blockedBy: "postgres" })}
      />,
    );

    const label = screen.getByText("Waiting for dependency");
    // No failure styling. Waiting is the system working.
    expect(label.className).not.toContain("text-danger");
    expect(screen.queryByRole("alert")).toBeNull();
    expect(
      screen.getByText(/Nothing is wrong and nothing is being asked of you/i),
    ).toBeInTheDocument();
    // And it names WHICH container, with a link to it.
    expect(screen.getByRole("link", { name: "postgres" })).toBeInTheDocument();
  });

  it("styles a real block as a failure and names the cause", () => {
    renderIn(
      <DecisionReason
        decision={decision({ reason: "dependencyBlocked", blockedBy: "postgres" })}
      />,
    );

    expect(screen.getByText("Blocked by dependency").className).toContain(
      "text-danger",
    );
    expect(
      screen.getByText(/which could not be updated safely/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "postgres" })).toHaveAttribute(
      "href",
      "/containers?search=postgres",
    );
  });

  it("distinguishes the per-pass budget from a dependency block", () => {
    renderIn(<DecisionReason decision={decision({ reason: "runLimit" })} />);

    expect(screen.getByText("Per-pass limit")).toBeInTheDocument();
    // Nothing is wrong and nothing is blocked: the container is simply next.
    expect(screen.queryByText(/dependenc/i)).toBeNull();
    expect(screen.getByText("Per-pass limit").className).not.toContain(
      "text-danger",
    );
  });

  it("sends a loop to the dependency page rather than to a container", () => {
    renderIn(<DecisionReason decision={decision({ reason: "dependencyCycle" })} />);

    expect(screen.getByRole("link", { name: "See the loop" })).toHaveAttribute(
      "href",
      "/dependencies",
    );
  });

  it("explains a provider held back to protect its dependants", () => {
    renderIn(
      <DecisionReason
        decision={decision({
          containerName: "gluetun",
          reason: "dependentsNotRebindable",
        })}
      />,
    );

    expect(
      screen.getByText("Dependants could not be reattached"),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/could not be established as safely reattachable/i),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("link", { name: "See what depends on it" }),
    ).toHaveAttribute("href", "/dependencies");
  });

  it("has no serious accessibility violations", async () => {
    const { container } = renderIn(
      <ul>
        {(["dependencyWaiting", "dependencyBlocked", "dependencyCycle"] as const).map(
          (reason) => (
            <li key={reason}>
              <DecisionReason decision={decision({ reason, blockedBy: "postgres" })} />
            </li>
          ),
        )}
      </ul>,
    );
    expect(await noSeriousViolations(container)).toEqual([]);
  });
});

// ------------------------------------------------------------- approvals --

describe("approval and dependency", () => {
  it("says approved is not the same as updating now", () => {
    renderIn(
      <ApprovalDependencyNote
        decision={decision({
          verdict: "awaitingApproval",
          reason: "approvalRequired",
          dependencyState: "dependencyWaiting",
          blockedBy: "postgres",
        })}
      />,
    );

    expect(screen.getByText(/Approved — waiting for dependency/)).toBeInTheDocument();
    expect(
      screen.getByText(/does not start it ahead of what this container depends on/i),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "postgres" })).toBeInTheDocument();
  });

  it("says approval does not override a refusal", () => {
    renderIn(
      <ApprovalDependencyNote
        decision={decision({
          verdict: "awaitingApproval",
          dependencyState: "dependencyBlocked",
        })}
      />,
    );

    expect(
      screen.getByText(/HarborMaster still refuses to proceed while this holds/i),
    ).toBeInTheDocument();
  });

  it("says nothing when the dependencies are fine", () => {
    const { container } = renderIn(
      <ApprovalDependencyNote
        decision={decision({ dependencyState: "dependencySatisfied" })}
      />,
    );
    expect(container.textContent).toBe("");
  });

  it("says nothing when the subsystem did not answer", () => {
    const { container } = renderIn(
      <ApprovalDependencyNote decision={decision({ dependencyState: undefined })} />,
    );
    expect(container.textContent).toBe("");
  });
});

// --------------------------------------------------------------- rebinds --

describe("reattachment wording", () => {
  it("never describes a rebind as an image update", () => {
    for (const state of [
      "pending",
      "planCreated",
      "acquired",
      "executing",
      "verified",
      "blocked",
      "failed",
      "interrupted",
    ] as const) {
      const { title, detail } = describeRebind(state, "sonarr", "gluetun");
      const text = `${title} ${detail}`.toLowerCase();
      expect(text, `${state} title`).not.toMatch(/\bupdat(e|ed|ing)\b/);
      expect(text).not.toMatch(/new(er)? (image|version)/);
      expect(text).not.toMatch(/upgrade/);
    }
  });

  it("says the version does not move, in the states where it matters", () => {
    expect(describeRebind("pending", "sonarr", "gluetun").detail).toMatch(
      /version is not changing/i,
    );
    expect(describeRebind("executing", "sonarr", "gluetun").detail).toMatch(
      /already running/i,
    );
    expect(describeRebind("verified", "sonarr", "gluetun").detail).toMatch(
      /same image it was before/i,
    );
    expect(describeRebind("failed", "sonarr", "gluetun").detail).toMatch(
      /No image version was changed/i,
    );
  });

  it("marks only the settled failures as needing a person", () => {
    expect(rebindNeedsOperator("failed")).toBe(true);
    expect(rebindNeedsOperator("blocked")).toBe(true);
    // In flight is work, not a condition.
    expect(rebindNeedsOperator("pending")).toBe(false);
    expect(rebindNeedsOperator("executing")).toBe(false);
    expect(rebindNeedsOperator("verified")).toBe(false);
  });

  it("says HarborMaster does not retry a failed reattachment", () => {
    renderIn(<RebindNote member={rebindMember({ state: "failed" })} />);

    expect(screen.getByText("Rebind failed")).toBeInTheDocument();
    expect(
      screen.getByText(/does not retry a reattachment by itself/i),
    ).toBeInTheDocument();
  });

  it("has no serious accessibility violations", async () => {
    const { container } = renderIn(
      <div>
        <RebindNote member={rebindMember({ state: "failed" })} />
        <RebindNote member={rebindMember({ state: "executing" })} />
      </div>,
    );
    expect(await noSeriousViolations(container)).toEqual([]);
  });
});

// ---------------------------------------------- coordinated update view --

describe("a coordinated update", () => {
  it("shows the provider, each reattachment, and the overall answer", () => {
    renderIn(<DependencyOperationList summaries={[operationSummary()]} />);

    expect(screen.getByRole("heading", { name: /gluetun update/ })).toBeInTheDocument();
    expect(screen.getByText("Verified")).toBeInTheDocument();

    const rebinds = screen.getByRole("region", {
      name: "Required reattachments for gluetun",
    });
    expect(within(rebinds).getByRole("link", { name: "sonarr" })).toBeInTheDocument();
    expect(within(rebinds).getByRole("link", { name: "radarr" })).toBeInTheDocument();
    expect(
      within(rebinds).getByRole("link", { name: "qbittorrent" }),
    ).toBeInTheDocument();

    const overall = screen.getByRole("region", { name: "Overall outcome for gluetun" });
    expect(within(overall).getByText("Needs attention")).toBeInTheDocument();
  });

  it("says the successful half stays and no group rollback happens", () => {
    renderIn(<DependencyOperationList summaries={[operationSummary()]} />);

    expect(
      screen.getByText(/provider and the successful reattachments remain in place/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/does not automatically roll the dependency group backward/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/No image version was changed by any reattachment/i),
    ).toBeInTheDocument();
  });

  it("offers no control that would retry or roll back", () => {
    renderIn(<DependencyOperationList summaries={[operationSummary()]} />);
    // There is deliberately no mechanism for either.
    expect(screen.queryByRole("button")).toBeNull();
  });

  it("says so when nothing has ever had to be reattached", () => {
    renderIn(<DependencyOperationList summaries={[]} />);
    expect(screen.getByText(/has not had to reattach anything/i)).toBeInTheDocument();
  });

  it("says when the listing was truncated", () => {
    const many = Array.from({ length: 3 }, (_, index) =>
      operationSummary({
        operation: {
          ...operationSummary().operation,
          operationId: `depop_${index}`,
        },
      }),
    );
    renderIn(<DependencyOperationList summaries={many} limit={3} />);
    expect(screen.getByText(/Showing the 3 most recent/)).toBeInTheDocument();
  });

  it("has no serious accessibility violations", async () => {
    const { container } = renderIn(
      <DependencyOperationList summaries={[operationSummary()]} />,
    );
    expect(await noSeriousViolations(container)).toEqual([]);
  });
});

// ---------------------------------------- a container's current activity --

describe("a container's current dependency status", () => {
  it("says nothing when the dependencies are satisfied", () => {
    const { container } = renderIn(
      <CurrentDependencyStatus
        attention={attention({
          dependencyKnown: true,
          dependencyState: "dependencySatisfied",
        })}
        container="api"
      />,
    );
    expect(container.textContent).toBe("");
  });

  it("says nothing when the subsystem did not answer", () => {
    const { container } = renderIn(
      <CurrentDependencyStatus attention={attention()} container="api" />,
    );
    expect(container.textContent).toBe("");
  });

  it("names what a waiting container is waiting for", () => {
    renderIn(
      <CurrentDependencyStatus
        attention={attention({
          dependencyKnown: true,
          dependencyState: "dependencyWaiting",
          dependencyBlockedBy: "postgres",
        })}
        container="api"
      />,
    );
    expect(screen.getByText("Waiting for dependency")).toBeInTheDocument();
    expect(
      screen.getByText(/Waiting for postgres to finish updating and verify successfully/),
    ).toBeInTheDocument();
  });

  it("says a failed rebind was not retried and moved no version", () => {
    renderIn(
      <CurrentDependencyStatus
        attention={attention({
          rebindFailed: true,
          rebindProvider: "gluetun",
        })}
        container="sonarr"
      />,
    );
    expect(screen.getByText("Rebind failed")).toBeInTheDocument();
    expect(
      screen.getByText(/did not retry the recreation automatically/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/no image version was changed/i)).toBeInTheDocument();
  });

  it("distinguishes an unresolvable dependency from having none", () => {
    renderIn(
      <CurrentDependencyStatus
        attention={attention({
          dependencyKnown: true,
          dependencyState: "dependencyMissing",
        })}
        container="sonarr"
      />,
    );
    expect(
      screen.getByText(/a refusal, not a finding that there is nothing to wait for/i),
    ).toBeInTheDocument();
  });

  it("has no serious accessibility violations", async () => {
    const { container } = renderIn(
      <CurrentDependencyStatus
        attention={attention({ rebindFailed: true, rebindProvider: "gluetun" })}
        container="sonarr"
      />,
    );
    expect(await noSeriousViolations(container)).toEqual([]);
  });
});

// ------------------------------------------------------ attention states --

describe("the container attention vocabulary", () => {
  it("labels every dependency state in words", () => {
    for (const state of [
      "dependencyFailed",
      "dependencyCycle",
      "dependencyUnresolved",
      "dependencyBlocked",
    ] as const) {
      expect(ATTENTION_LABELS[state]).toBeTruthy();
      expect(ATTENTION_LABELS[state]).not.toBe(state);
    }
  });

  it("asks for a person only where nothing resolves itself", () => {
    expect(needsOperator("dependencyFailed")).toBe(true);
    expect(needsOperator("dependencyCycle")).toBe(true);
    expect(needsOperator("dependencyUnresolved")).toBe(true);
    // A block frequently clears on the next pass. Listing it as work would
    // make the "needs attention" list noisy enough to stop being read.
    expect(needsOperator("dependencyBlocked")).toBe(false);
  });

  it("mirrors the server's own answer for every state", () => {
    // The frontend copy of NeedsOperator must not drift from the server's.
    const expected: Record<AttentionState, boolean> = {
      preserved: false,
      unhealthy: true,
      dependencyFailed: true,
      approvalRequired: true,
      paused: true,
      dependencyCycle: true,
      dependencyUnresolved: true,
      needsReview: true,
      dependencyBlocked: false,
      cannotAdvise: false,
      updateAvailable: false,
      notTracked: false,
      // A permanent absence of evidence, not work for a person. The operator
      // may want to act on it, but nothing is waiting on them.
      notComparable: false,
      notChecked: false,
      upToDate: false,
    };
    for (const [state, want] of Object.entries(expected)) {
      expect(needsOperator(state as AttentionState), state).toBe(want);
    }
  });
});
