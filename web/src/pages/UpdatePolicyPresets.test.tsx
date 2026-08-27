import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TestSessionProvider, testSession, testUser } from "../test/session";
import { UpdatePolicies } from "./UpdatePolicies";

/**
 * Automation presets, in the editor.
 *
 * # What these assert, and what they refuse to assert
 *
 * Every check here is about the REQUEST BODY that reaches the network, or about
 * a control a person can operate. Nothing snapshots markup: a preset can only
 * be wrong by producing the wrong field, and a snapshot would record the wrong
 * field as happily as the right one.
 *
 * The compiler and the detector have their own unit tests in
 * `src/api/automationPresets.test.ts`. These are about the wiring: that
 * choosing an outcome writes those fields into the form, that the form sends
 * them, and that an operator's targeting survives the trip.
 */

const originalFetch = globalThis.fetch;

let posted: { url: string; method: string; body: Record<string, unknown> }[] = [];
let engineEnabled = true;

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

const CONTAINERS = {
  items: [
    {
      hostId: "local",
      id: "c1",
      shortId: "c1",
      name: "web",
      image: { raw: "nginx:1.27" },
      state: "running",
      health: "healthy",
      createdAt: "2026-01-01T00:00:00Z",
      restartCount: 0,
      restartPolicy: { name: "always" },
      compose: { managed: false, oneOff: false },
      harbormaster: {},
      ports: [],
      present: true,
      firstSeenAt: "2026-01-01T00:00:00Z",
      lastSeenAt: "2026-01-01T00:00:00Z",
      attention: { level: "none", reasons: [] },
    },
  ],
  pagination: { page: 1, pageSize: 50, total: 1, totalPages: 1 },
};

/** A stored policy, so the edit path can be exercised. */
function storedPolicy(overrides: Record<string, unknown> = {}) {
  return {
    policyId: "upd_" + "1".repeat(20),
    name: "existing",
    enabled: true,
    priority: 0,
    scope: "allEligible",
    selector: { exclude: ["database"] },
    strategy: "digestOnly",
    minimumRecommendation: "proceed",
    mode: "automatic",
    window: { alwaysOpen: true },
    failure: { autoRollback: true, pauseAfterFailures: 2 },
    ...overrides,
  };
}

let policyList: unknown[] = [];

beforeEach(() => {
  posted = [];
  policyList = [];
  engineEnabled = true;

  globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (init?.method === "POST" || init?.method === "PATCH") {
      posted.push({
        url,
        method: init.method,
        body: init.body
          ? (JSON.parse(String(init.body)) as Record<string, unknown>)
          : {},
      });
      return jsonResponse(
        { policy: storedPolicy(), warnings: [] },
        init.method === "POST" ? 201 : 200,
      );
    }
    if (url.includes("/containers")) return jsonResponse(CONTAINERS);
    if (url.includes("/update-policies")) {
      return jsonResponse({
        items: policyList,
        pagination: {
          page: 1,
          pageSize: 50,
          total: policyList.length,
          totalPages: 1,
        },
      });
    }
    if (url.includes("/automation")) {
      return jsonResponse({
        status: {
          enabled: engineEnabled,
          running: false,
          policies: 0,
          enabledPolicies: 0,
          pausedContainers: 0,
          awaitingApproval: 0,
          windowOpen: true,
          self: {},
        },
      });
    }
    return jsonResponse({});
  }) as typeof fetch;
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

function renderPage() {
  return render(
    <TestSessionProvider session={testSession({ user: testUser("administrator") })}>
      <MemoryRouter initialEntries={["/automation/policies"]}>
        <UpdatePolicies />
      </MemoryRouter>
    </TestSessionProvider>,
  );
}

async function openNewEditor() {
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "New policy" }));
  await screen.findByText("New update policy");
  return user;
}

async function openEditorFor(policy: Record<string, unknown>) {
  policyList = [policy];
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: /^Edit/ }));
  await screen.findByText(/^Edit /);
  return user;
}

/**
 * The preset radio group, scoped.
 *
 * Scoping matters: the mode control also offers an option headed "Observe
 * only", and the two are told apart by their fieldset rather than by their
 * heading. Their full accessible names differ -- each carries its own
 * description -- but a loose substring query would match both, so every query
 * below is scoped to the group under test.
 */
function presets() {
  const legend = screen.getByText("What do you want HarborMaster to do?");
  return within(legend.closest("fieldset") as HTMLElement);
}

/** Fills the name and saves, returning the body that was sent. */
async function saveAs(user: ReturnType<typeof userEvent.setup>, name: string) {
  await user.type(screen.getByLabelText("Name"), name);
  await user.click(screen.getByRole("button", { name: "Save policy" }));
  await waitFor(() => expect(posted.length).toBeGreaterThan(0));
  return posted[posted.length - 1]!.body;
}

// ------------------------------------------------------------ the choice --

describe("choosing an outcome", () => {
  it("offers the five outcomes as one labelled radio group", async () => {
    await openNewEditor();

    const legend = screen.getByText("What do you want HarborMaster to do?");
    const fieldset = legend.closest("fieldset");
    expect(fieldset).not.toBeNull();

    for (const label of [
      "Observe only",
      "Keep containers safely updated",
      "Follow current tag",
      "Allow minor version updates",
      "Custom",
    ]) {
      expect(presets().getByRole("radio", { name: new RegExp(label) })).toBeInTheDocument();
    }
  });

  it("starts a new policy on Observe only, which cannot change a host", async () => {
    const user = await openNewEditor();

    expect(presets().getByRole("radio", { name: /Observe only/ })).toBeChecked();

    const body = await saveAs(user, "watch first");
    expect(body.mode).toBe("observe");
  });
});

// -------------------------------------------------------- what is posted --

describe("what a preset sends", () => {
  it("Follow current tag posts a digest-only automatic policy", async () => {
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));

    const body = await saveAs(user, "follow tags");

    expect(body.strategy).toBe("digestOnly");
    expect(body.mode).toBe("automatic");
    expect(body.minimumRecommendation).toBe("proceed");
    expect((body.failure as Record<string, unknown>).autoRollback).toBe(true);
  });

  it("Keep containers safely updated posts a patch ceiling", async () => {
    const user = await openNewEditor();
    await user.click(
      presets().getByRole("radio", { name: /Keep containers safely updated/ }),
    );

    const body = await saveAs(user, "safe");
    expect(body.strategy).toBe("patch");
    expect(body.mode).toBe("automatic");
  });

  it("Allow minor version updates posts a minor ceiling", async () => {
    const user = await openNewEditor();
    await user.click(
      presets().getByRole("radio", { name: /Allow minor version updates/ }),
    );

    const body = await saveAs(user, "minor");
    expect(body.strategy).toBe("minor");
    expect(body.mode).toBe("automatic");
  });

  it("never posts a preset identifier", async () => {
    // The backend must not be able to tell a preset from a hand-built policy.
    // A `preset` field would be a second source of truth about what the policy
    // does, and the runtime would eventually read it.
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));

    const body = await saveAs(user, "follow tags");
    expect(body).not.toHaveProperty("preset");
    expect(Object.keys(body).sort()).toEqual([
      "description",
      "failure",
      "minimumRecommendation",
      "mode",
      "name",
      "priority",
      "scope",
      "selector",
      "strategy",
      "window",
    ]);
  });
});

// ----------------------------------------------------- operator ownership --

describe("what a preset leaves alone", () => {
  it("keeps the scope, exclusions and window across a preset change", async () => {
    const user = await openNewEditor();

    // The operator's targeting and scheduling, chosen first.
    await user.click(
      screen.getByRole("radio", { name: /All eligible containers/ }),
    );
    await user.type(screen.getByLabelText(/Never touch these/), "database");
    await user.click(screen.getByRole("radio", { name: /Maintenance window/ }));

    // Then two different outcomes, in sequence.
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));
    await user.click(
      presets().getByRole("radio", { name: /Allow minor version updates/ }),
    );

    const body = await saveAs(user, "estate");

    // The preset moved.
    expect(body.strategy).toBe("minor");
    // The operator's choices did not.
    expect(body.scope).toBe("allEligible");
    expect((body.selector as Record<string, unknown>).exclude).toEqual(["database"]);
    expect((body.window as Record<string, unknown>).alwaysOpen).toBe(false);
  });

  it("keeps selected containers when the outcome changes", async () => {
    const user = await openNewEditor();

    await user.click(screen.getByRole("radio", { name: /Selected containers/ }));
    await user.click(await screen.findByRole("checkbox", { name: /web/ }));
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));

    const body = await saveAs(user, "just web");
    expect((body.selector as Record<string, unknown>).include).toEqual(["web"]);
    expect(body.strategy).toBe("digestOnly");
  });
});

// ------------------------------------------------------------- switching --

describe("switching to Custom", () => {
  it("keeps the values the preset produced", async () => {
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));
    await user.click(presets().getByRole("radio", { name: /^Custom/ }));

    // The individual controls are showing what Follow current tag chose,
    // rather than having been reset.
    expect(screen.getByRole("radio", { name: /^Same tag only/ })).toBeChecked();
    expect(screen.getByRole("radio", { name: /^Automatic/ })).toBeChecked();

    const body = await saveAs(user, "custom from follow");
    expect(body.strategy).toBe("digestOnly");
    expect(body.mode).toBe("automatic");
  });

  it("moves to Custom when a ceiling is widened by hand", async () => {
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));
    expect(presets().getByRole("radio", { name: /Follow current tag/ })).toBeChecked();

    // Widening the ceiling makes this no longer Follow current tag, and the
    // label must say so rather than continuing to claim the preset.
    await user.click(screen.getByRole("radio", { name: /^Up to a major version/ }));

    expect(presets().getByRole("radio", { name: /^Custom/ })).toBeChecked();
    expect(
      presets().getByRole("radio", { name: /Follow current tag/ }),
    ).not.toBeChecked();
  });

  it("returns to a preset when the fields match one again", async () => {
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));
    await user.click(screen.getByRole("radio", { name: /^Up to a major version/ }));
    expect(presets().getByRole("radio", { name: /^Custom/ })).toBeChecked();

    await user.click(screen.getByRole("radio", { name: /^Same tag only/ }));
    expect(
      presets().getByRole("radio", { name: /Follow current tag/ }),
    ).toBeChecked();
  });
});

// ------------------------------------------------------------- detection --

describe("opening an existing policy", () => {
  it("selects the preset a stored policy exactly matches", async () => {
    await openEditorFor(storedPolicy());

    expect(
      presets().getByRole("radio", { name: /Follow current tag/ }),
    ).toBeChecked();
  });

  it("selects Custom for a policy that differs in one safety field", async () => {
    // Digest-only and automatic, but with the looser recommendation floor. It
    // permits strictly more than Follow current tag does, so labelling it
    // Follow current tag would tell the operator something untrue.
    await openEditorFor(
      storedPolicy({ minimumRecommendation: "proceedWithCaution" }),
    );

    expect(presets().getByRole("radio", { name: /^Custom/ })).toBeChecked();
    expect(
      presets().getByRole("radio", { name: /Follow current tag/ }),
    ).not.toBeChecked();
  });

  it("still selects the preset when only targeting differs", async () => {
    await openEditorFor(
      storedPolicy({
        scope: "selector",
        selector: { include: ["web"] },
        priority: 50,
      }),
    );

    expect(
      presets().getByRole("radio", { name: /Follow current tag/ }),
    ).toBeChecked();
  });

  it("does not rewrite an existing policy merely by opening it", async () => {
    const user = await openEditorFor(
      storedPolicy({ minimumRecommendation: "proceedWithCaution" }),
    );

    await user.click(screen.getByRole("button", { name: "Save policy" }));
    await waitFor(() => expect(posted.length).toBeGreaterThan(0));

    // Opening a Custom policy and saving it unchanged must not quietly
    // tighten or loosen it into a preset.
    expect(posted[0]!.body.minimumRecommendation).toBe("proceedWithCaution");
    expect(posted[0]!.body.strategy).toBe("digestOnly");
  });
});

// --------------------------------------------------------------- summary --

describe("the before-save summary", () => {
  it("states what Follow current tag may and may not do", async () => {
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));

    const consequence = screen.getByTestId("preset-consequence");
    expect(consequence).toHaveTextContent(/may replace a container/i);
    expect(consequence).toHaveTextContent(/different digest/i);
    expect(consequence).toHaveTextContent(/will not move to another version tag/i);
  });

  it("says major versions are held under Allow minor version updates", async () => {
    const user = await openNewEditor();
    await user.click(
      presets().getByRole("radio", { name: /Allow minor version updates/ }),
    );

    expect(screen.getByTestId("preset-consequence")).toHaveTextContent(
      /major version updates are still held/i,
    );
  });

  it("promises eligibility rather than an outcome", async () => {
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));

    const text = screen.getByTestId("preset-consequence").textContent ?? "";
    expect(text).toMatch(/\bmay\b/);
    expect(text).not.toMatch(/these containers will be updated/i);
  });

  it("never hides the recommendation floor", async () => {
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));

    expect(
      screen.getByText(/acts only on the ones it rates as straightforward/i),
    ).toBeInTheDocument();
  });
});

// ------------------------------------------------------------ Watchtower --

describe("Watchtower migration help", () => {
  it("names Follow current tag and the checks HarborMaster adds", async () => {
    const user = await openNewEditor();
    await user.click(screen.getByText("Coming from Watchtower?"));

    expect(screen.getByText(/closest match to what Watchtower does/i)).toBeInTheDocument();
    // It must be honest about the difference rather than claiming parity.
    expect(
      screen.getByText(/will sometimes refuse an update Watchtower would have applied/i),
    ).toBeInTheDocument();
  });
});

// --------------------------------------------------------- engine state --

describe("when the automation engine is disabled", () => {
  beforeEach(() => {
    engineEnabled = false;
  });

  it("warns that an automatic policy will not run yet", async () => {
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));

    expect(screen.getByTestId("engine-disabled-warning")).toHaveTextContent(
      /will not run automatically until the automation engine is enabled/i,
    );
  });

  it("names the configuration key without offering to change it", async () => {
    await openNewEditor();

    expect(
      screen.getByText(/HARBORMASTER_AUTOMATION_ENABLED/),
    ).toBeInTheDocument();
    // There must be no control that claims to switch the engine on: it is a
    // deployment setting, and a toggle here would be a lie.
    expect(
      screen.queryByRole("button", { name: /enable the automation engine/i }),
    ).toBeNull();
    expect(
      screen.queryByRole("switch", { name: /automation engine/i }),
    ).toBeNull();
  });

  it("does not warn for an observe policy, which never needed the engine", async () => {
    await openNewEditor();

    expect(screen.queryByTestId("engine-disabled-warning")).toBeNull();
  });

  it("still allows the policy to be saved", async () => {
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));

    const body = await saveAs(user, "ready for later");
    expect(body.mode).toBe("automatic");
  });
});

// ------------------------------------------------------------ a11y --

describe("accessibility", () => {
  it("has no serious or critical axe findings with a preset chosen", async () => {
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));

    const results = await axe.run(document.body, {
      resultTypes: ["violations"],
runOnly: { type: "tag", values: ["wcag2a", "wcag2aa"] },
    });
    const serious = results.violations.filter(
      (violation) => violation.impact === "serious" || violation.impact === "critical",
    );
    expect(
      serious.map((violation) => `${violation.id}: ${violation.help}`),
    ).toEqual([]);
  });

  it("associates every preset with its description", async () => {
    await openNewEditor();

    for (const label of ["Observe only", "Follow current tag", "Custom"]) {
      const radio = presets().getByRole("radio", { name: new RegExp(label) });
      const describedBy = radio.getAttribute("aria-describedby");
      expect(describedBy).toBeTruthy();
      expect(document.getElementById(describedBy!)?.textContent).toBeTruthy();
    }
  });

  it("marks the selected preset in words rather than by colour alone", async () => {
    const user = await openNewEditor();
    await user.click(presets().getByRole("radio", { name: /Follow current tag/ }));

    const radio = presets().getByRole("radio", { name: /Follow current tag/ });
    // The radio's own checked state is the machine-readable signal...
    expect(radio).toBeChecked();
    // ...and the label repeats it in text, so it does not rely on the ring.
    expect(radio.closest("label")).toHaveTextContent(/\(selected\)/);
  });

  it("lets the whole group be reached and changed from the keyboard", async () => {
    const user = await openNewEditor();

    const observe = presets().getByRole("radio", { name: /Observe only/ });
    observe.focus();
    expect(observe).toHaveFocus();

    // Arrow keys move within a radio group, which is why this is a real
    // fieldset of real radios rather than a row of buttons.
    await user.keyboard("{ArrowDown}");
    expect(
      presets().getByRole("radio", { name: /Keep containers safely updated/ }),
    ).toBeChecked();
  });

  it("gives the preset radios unique accessible names", async () => {
    await openNewEditor();

    // Scoped, like every other query in this file. The ceiling group points all
    // four of its radios at ONE shared "this is a ceiling, not an instruction"
    // hint -- correct markup, and deliberate -- so a document-wide uniqueness
    // check would fail on markup that is right.
    const radios = presets().getAllByRole("radio");
    expect(radios).toHaveLength(5);

    // The accessible name is what a screen reader announces: the label's text.
    const names = radios.map((radio) => radio.closest("label")?.textContent ?? "");
    expect(new Set(names).size).toBe(names.length);

    // And each preset points at its OWN description, not a shared one.
    const described = radios.map((radio) => radio.getAttribute("aria-describedby"));
    expect(new Set(described).size).toBe(described.length);
  });
});
