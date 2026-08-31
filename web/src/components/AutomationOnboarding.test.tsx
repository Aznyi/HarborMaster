import { render, screen, within } from "@testing-library/react";
import axe from "axe-core";
import { MemoryRouter } from "react-router";
import { describe, expect, it } from "vitest";

import type { FirstRunFacts } from "../api/firstRun";
import type { Features } from "../api/types";
import { AutomationOnboarding } from "./AutomationOnboarding";

/**
 * The first-run onboarding panel.
 *
 * # What these defend
 *
 * Every case below is a state an operator could be in, and the assertion is as
 * much about what is NOT said as what is. The failures that matter are all the
 * same shape: two different situations rendered with the same sentence, sending
 * somebody to change the wrong thing.
 *
 *     assessment pending   must never read as "0 eligible"
 *     engine disabled      must never read as Observe mode
 *     Observe mode         must never read as engine disabled
 *     readiness unavailable must never read as "no containers eligible"
 *     paused               must never read as the whole engine being off
 */

const allOn: Features = {
  inventory: true,
  events: true,
  snapshots: true,
  drift: true,
  policy: true,
  planner: true,
  imageIntel: true,
  acquisition: true,
  execution: true,
  rollback: true,
  imageCleanup: false,
  automation: true,
  notifications: false,
  notificationsAllowPrivate: false,
};

/** A fully working installation. Each case breaks one thing. */
function working(overrides: Partial<FirstRunFacts> = {}): FirstRunFacts {
  return {
    features: { planner: true, automation: true },
    inventoryEstablished: true,
    assessed: true,
    policies: 1,
    actingPolicies: 1,
    pausedContainers: 0,
    manualReviews: 0,
    eligible: 3,
    readinessKnown: true,
    ...overrides,
  };
}

function renderPanel(
  facts: FirstRunFacts,
  options: { features?: Features | null; required?: string[]; may?: boolean } = {},
) {
  return render(
    <MemoryRouter>
      <AutomationOnboarding
        facts={facts}
        features={options.features === undefined ? allOn : options.features}
        requiredCapabilities={options.required ?? ["acquisition", "execution", "automation"]}
        mayManagePolicies={options.may ?? true}
      />
    </MemoryRouter>,
  );
}

function panel() {
  return screen.getByTestId("automation-onboarding");
}

describe("the first-run onboarding panel", () => {
  it("says HarborMaster is starting before inventory exists", () => {
    renderPanel(working({ inventoryEstablished: false }));

    expect(panel()).toHaveAttribute("data-state", "inventoryPending");
    expect(panel().textContent).toMatch(/still establishing what is running/i);
    // Nothing to do, so nothing is offered.
    expect(screen.queryByRole("link")).not.toBeInTheDocument();
  });

  it("says the assessment is pending, never that nothing is eligible", () => {
    renderPanel(working({ assessed: false, eligible: 0 }));

    expect(panel()).toHaveAttribute("data-state", "assessmentPending");
    expect(panel().textContent).toMatch(/has not finished its first update assessment/i);
    expect(panel().textContent).toMatch(/no action is required/i);

    // THE load-bearing negative.
    expect(panel().textContent).not.toMatch(/0 (containers|eligible)/i);
    expect(panel().textContent).not.toMatch(/no containers are currently eligible/i);
    expect(panel().textContent).not.toMatch(/nothing to do/i);
  });

  it("distinguishes a disabled engine from Observe mode", () => {
    renderPanel(working({ features: { planner: true, automation: false } }));

    expect(panel()).toHaveAttribute("data-state", "engineDisabled");
    expect(panel().textContent).toMatch(/will not automatically recreate containers/i);
    // Not Observe: that is a policy choice, and this is a deployment setting.
    expect(panel().textContent).not.toMatch(/observ/i);
  });

  it("distinguishes Observe mode from a disabled engine", () => {
    renderPanel(working({ actingPolicies: 0 }));

    expect(panel()).toHaveAttribute("data-state", "observeOnly");
    expect(panel().textContent).toMatch(/evaluating updates but will not automatically change/i);
    // The engine may be running perfectly; saying it is off sends the operator
    // to change a deployment setting that is already correct.
    expect(panel().textContent).not.toMatch(/disabled/i);
    expect(
      screen.getByRole("link", { name: "Review policies" }),
    ).toBeInTheDocument();
  });

  it("invites a first policy, using the canonical preset name", () => {
    renderPanel(working({ policies: 0, actingPolicies: 0 }));

    expect(panel()).toHaveAttribute("data-state", "noPolicy");
    expect(panel().textContent).toMatch(/Choose how HarborMaster should handle updates/i);
    // The Watchtower callout, naming the preset from the Stage 17.3 table.
    expect(panel().textContent).toMatch(/Coming from Watchtower\?/i);
    expect(panel().textContent).toMatch(/Follow current tag/);
    // And it does not overstate equivalence.
    expect(panel().textContent).toMatch(/which Watchtower does not/i);
    expect(
      screen.getByRole("link", { name: "Create update policy" }),
    ).toBeInTheDocument();
  });

  it("says nothing is eligible only once the estate is assessed", () => {
    renderPanel(working({ eligible: 0 }));

    expect(panel()).toHaveAttribute("data-state", "nothingEligible");
    expect(panel().textContent).toMatch(/Based on the current assessment/i);
    expect(panel().textContent).toMatch(/no containers are currently eligible/i);
    // Must not read as though the assessment had not happened.
    expect(panel().textContent).not.toMatch(/has not finished/i);
    expect(panel().textContent).not.toMatch(/assessing your containers/i);
  });

  it("reports eligible containers as a reading, not a promise", () => {
    renderPanel(working({ eligible: 6 }));

    expect(panel()).toHaveAttribute("data-state", "active");
    expect(panel().textContent).toMatch(/6 containers are currently eligible/i);
    expect(panel().textContent).toMatch(/still run its normal safety checks/i);
    // Never a promise that six containers will change.
    expect(panel().textContent).not.toMatch(/will update|will be updated|will replace/i);
  });

  it("surfaces attention without claiming the engine is off", () => {
    renderPanel(working({ pausedContainers: 2, manualReviews: 1 }));

    expect(panel()).toHaveAttribute("data-state", "needsAttention");
    const detail = screen.getByTestId("onboarding-attention");
    expect(detail.textContent).toMatch(/2 containers have automatic updates paused/i);
    expect(detail.textContent).toMatch(/1 update requires manual review/i);

    // The engine is fine; only some workloads are held.
    expect(panel().textContent).not.toMatch(/disabled/i);
    // And the actions go to the existing surfaces rather than acting here.
    expect(
      screen.getByRole("link", { name: "Review paused containers" }),
    ).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Review plans" })).toBeInTheDocument();
    // Onboarding never resumes or approves anything itself.
    expect(screen.queryByRole("button")).not.toBeInTheDocument();
  });

  it("says it could not establish readiness, never that nothing is eligible", () => {
    renderPanel(working({ readinessKnown: false, eligible: 0 }));

    expect(panel()).toHaveAttribute("data-state", "unknown");
    expect(panel().textContent).toMatch(/could not establish current automation readiness/i);
    expect(panel().textContent).toMatch(/not the same as having nothing to do/i);

    // THE other load-bearing negative.
    expect(panel().textContent).not.toMatch(/no containers are currently eligible/i);
    expect(panel().textContent).not.toMatch(/0 containers/i);
  });

  it("says when assessment is switched off rather than pending", () => {
    renderPanel(working({ features: { planner: false, automation: true } }));

    expect(panel()).toHaveAttribute("data-state", "assessmentUnavailable");
    expect(panel().textContent).toMatch(/switched off in this deployment/i);
    // Pending resolves by waiting; this one does not.
    expect(panel().textContent).not.toMatch(/no action is required/i);
  });

  it("names the exact settings, and offers no way to write them", () => {
    renderPanel(working({ features: { planner: true, automation: false } }), {
      features: { ...allOn, automation: false },
    });

    const block = screen.getByLabelText(
      "Environment variables to enable the missing capabilities",
    );
    expect(block.textContent).toContain("HARBORMASTER_AUTOMATION_ENABLED=true");
    // Only what is actually missing.
    expect(block.textContent).not.toContain("ACQUISITION");
    expect(panel().textContent).toMatch(/recreate the container/i);

    // No control that could be mistaken for one.
    expect(screen.queryByRole("button", { name: /save|apply|restart|enable/i }))
      .not.toBeInTheDocument();
  });

  it("falls back to a set the process will actually start with", () => {
    // The server names nothing when no policy is set to act -- nothing is
    // required to do nothing -- which is exactly the fresh installation this
    // panel exists for. Live acceptance found the fallback printing
    // acquisition+execution+automation, a combination config validation refuses:
    // an operator applying it exactly would recreate the container and find
    // HarborMaster refusing to boot.
    renderPanel(working({ features: { planner: true, automation: false } }), {
      features: { ...allOn, automation: false, rollback: false },
      required: [],
    });

    const block = screen.getByLabelText(
      "Environment variables to enable the missing capabilities",
    );
    expect(block.textContent).toContain("HARBORMASTER_AUTOMATION_ENABLED=true");
    expect(block.textContent).toContain("HARBORMASTER_ROLLBACK_ENABLED=true");

    const list = screen.getByTestId("capability-checklist");
    expect(within(list).getAllByRole("listitem")).toHaveLength(4);
  });

  it("shows capability state in text, not by mark alone", () => {
    renderPanel(working({ features: { planner: true, automation: false } }), {
      features: { ...allOn, automation: false },
      required: ["acquisition", "execution", "automation"],
    });

    const list = screen.getByTestId("capability-checklist");
    const rows = within(list).getAllByRole("listitem");
    expect(rows).toHaveLength(3);
    // Every row says Enabled or Disabled in words.
    for (const row of rows) {
      expect(row.textContent).toMatch(/Enabled|Disabled/);
      // And stays shrinkable at a narrow width.
      expect(row.className).toContain("min-h-11");
      expect(row.querySelector("span.min-w-0")).not.toBeNull();
    }
  });

  it("lists exactly the capabilities the server named, and no others", () => {
    // The set is the SERVER's. This panel must not add to it or drop from it:
    // adding tells an operator to widen what HarborMaster may do for nothing,
    // and dropping hands them a combination the process will not start with.
    renderPanel(working({ features: { planner: true, automation: false } }), {
      features: { ...allOn, automation: false, rollback: false },
      required: ["acquisition", "execution", "automation", "rollback"],
    });

    const list = screen.getByTestId("capability-checklist");
    expect(within(list).getAllByRole("listitem")).toHaveLength(4);
    expect(list.textContent).toMatch(/Automatic rollback/);
    // Assessment is not in the required set here, so it is not listed.
    expect(list.textContent).not.toMatch(/Update assessment/);

    // And both missing ones reach the variable block.
    const block = screen.getByLabelText(
      "Environment variables to enable the missing capabilities",
    );
    expect(block.textContent).toContain("HARBORMASTER_AUTOMATION_ENABLED=true");
    expect(block.textContent).toContain("HARBORMASTER_ROLLBACK_ENABLED=true");
  });

  it("offers no policy control to somebody who may not manage policies", () => {
    renderPanel(working({ policies: 0, actingPolicies: 0 }), { may: false });

    expect(
      screen.queryByRole("link", { name: "Create update policy" }),
    ).not.toBeInTheDocument();
    expect(panel().textContent).toMatch(/needs the automation management permission/i);
  });

  it("says so when the capability report itself is missing", () => {
    renderPanel(working({ features: null }));

    expect(panel()).toHaveAttribute("data-state", "unknown");
    expect(panel().textContent).not.toMatch(/0 containers/i);
  });

  it("has no serious or critical axe findings in any state", async () => {
    const states: FirstRunFacts[] = [
      working({ inventoryEstablished: false }),
      working({ assessed: false }),
      working({ features: { planner: true, automation: false } }),
      working({ policies: 0, actingPolicies: 0 }),
      working({ actingPolicies: 0 }),
      working({ eligible: 0 }),
      working(),
      working({ pausedContainers: 1, manualReviews: 1 }),
      working({ readinessKnown: false }),
    ];

    for (const facts of states) {
      const { unmount } = renderPanel(facts);
      const results = await axe.run(document.body, {
        resultTypes: ["violations"],
        runOnly: { type: "tag", values: ["wcag2a", "wcag2aa"] },
      });
      const serious = results.violations.filter(
        (violation) =>
          violation.impact === "serious" || violation.impact === "critical",
      );
      expect(
        serious.map((violation) => `${violation.id}: ${violation.help}`),
      ).toEqual([]);
      unmount();
    }
  });

  it("keeps a long variable block scrollable rather than widening the page", () => {
    renderPanel(working({ features: { planner: true, automation: false } }), {
      features: { ...allOn, acquisition: false, execution: false, automation: false },
      required: ["acquisition", "execution", "automation"],
    });

    // Class mechanism, per the repository's convention: jsdom implements no
    // layout, so the cause is pinned rather than the symptom. Without
    // overflow-x the longest HARBORMASTER_* line pushes the page sideways.
    const block = screen.getByLabelText(
      "Environment variables to enable the missing capabilities",
    );
    expect(block.className).toContain("overflow-x-auto");
    // Reachable by keyboard, because a scrollable region must be.
    expect(block).toHaveAttribute("tabindex", "0");
  });
});
