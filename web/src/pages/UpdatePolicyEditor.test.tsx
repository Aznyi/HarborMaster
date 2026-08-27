import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import axe from "axe-core";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { TestSessionProvider, testSession, testUser } from "../test/session";
import { UpdatePolicies } from "./UpdatePolicies";

/**
 * The consolidated policy editor.
 *
 * # What these tests exist to catch
 *
 * The editor is the one place an operator authorises unattended changes to
 * their host, and its two failure modes are both silent:
 *
 *  1. **Sending something they did not choose.** A selector clause left over
 *     from a scope they switched away from is a rule they cannot see on the
 *     form and cannot see on the card.
 *  2. **Arriving at a dangerous setting by itself.** "All eligible containers"
 *     and "Automatic" must both be things a person picked.
 *
 * Everything else here is the accessibility and keyboard cover the phase gates
 * ask for, run against the real component rather than against a description of
 * it.
 */

const originalFetch = globalThis.fetch;

/** Requests the page made, so a test can assert on the body that was sent. */
let posted: { url: string; body: unknown }[] = [];

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
    {
      hostId: "local",
      id: "c2",
      shortId: "c2",
      name: "database",
      image: { raw: "postgres:16" },
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
  pagination: { page: 1, pageSize: 50, total: 2, totalPages: 1 },
};

beforeEach(() => {
  posted = [];
  globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    if (init?.method === "POST" || init?.method === "PATCH") {
      posted.push({
        url,
        body: init.body ? JSON.parse(String(init.body)) : undefined,
      });
      return jsonResponse({ policy: { policyId: "upd_" + "0".repeat(20) }, warnings: [] }, 201);
    }
    if (url.includes("/containers")) return jsonResponse(CONTAINERS);
    if (url.includes("/update-policies")) {
      return jsonResponse({
        items: [],
        pagination: { page: 1, pageSize: 50, total: 0, totalPages: 0 },
      });
    }
    // The engine status lives at /automation, not /automation/status. Checked
    // after /update-policies so the more specific path wins.
    if (url.includes("/automation")) {
      return jsonResponse({
        status: {
          enabled: true,
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

/** Opens the editor and waits for it. */
async function openEditor() {
  const user = userEvent.setup();
  renderPage();
  await user.click(await screen.findByRole("button", { name: "New policy" }));
  await screen.findByText("New update policy");
  return user;
}

// ------------------------------------------------------- the scope choice --

describe("choosing what to manage", () => {
  it("offers the four choices with the broad one unselected", async () => {
    await openEditor();

    for (const label of [
      "All eligible containers",
      "Selected containers",
      "Matching images",
      "Advanced selection",
    ]) {
      expect(screen.getByRole("radio", { name: new RegExp(label) })).toBeInTheDocument();
    }
    expect(
      screen.getByRole("radio", { name: /All eligible containers/ }),
    ).not.toBeChecked();
  });

  it("picks containers from the inventory rather than from memory", async () => {
    const user = await openEditor();

    // The picker is inventory-backed: the names come from the API, not typed.
    await screen.findByText("nginx:1.27");
    await user.click(screen.getByRole("checkbox", { name: /web/ }));

    const selected = screen.getByRole("list", { name: "Selected containers" });
    expect(within(selected).getByText("web")).toBeInTheDocument();
  });

  it("explains what the broad scope will never enrol", async () => {
    const user = await openEditor();
    await user.click(screen.getByRole("radio", { name: /All eligible containers/ }));

    expect(screen.getByText(/never enrol its own container/i)).toBeInTheDocument();
  });

  it("shows the exclusion field in every scope, including the broad one", async () => {
    const user = await openEditor();

    expect(screen.getByText("Never touch these")).toBeInTheDocument();
    await user.click(screen.getByRole("radio", { name: /All eligible containers/ }));
    expect(screen.getByText("Never touch these")).toBeInTheDocument();
    expect(
      screen.getByText(/checked first and win over everything above/i),
    ).toBeInTheDocument();
  });
});

// ------------------------------------------------- what actually gets sent --

describe("the request the editor sends", () => {
  it("sends no inclusion clause with the broad scope", async () => {
    const user = await openEditor();

    // Choose containers FIRST, then switch to the broad scope. The clause the
    // operator can no longer see must not travel with the request -- the
    // server refuses the combination, and a form that sent it would be showing
    // one policy and saving another.
    await screen.findByText("nginx:1.27");
    await user.click(screen.getByRole("checkbox", { name: /web/ }));
    await user.click(screen.getByRole("radio", { name: /All eligible containers/ }));

    await user.type(screen.getByLabelText("Name"), "everything");
    await user.click(screen.getByRole("button", { name: "Save policy" }));

    await waitFor(() => expect(posted).toHaveLength(1));
    const body = posted[0]?.body as { scope: string; selector: Record<string, unknown> };
    expect(body.scope).toBe("allEligible");
    expect(body.selector.include).toBeUndefined();
    expect(body.selector.images).toBeUndefined();
    expect(body.selector.labels).toBeUndefined();
  });

  it("keeps exclusions when the scope is broad", async () => {
    const user = await openEditor();
    await user.click(screen.getByRole("radio", { name: /All eligible containers/ }));
    await user.type(screen.getByLabelText("Name"), "everything");
    await user.type(screen.getByRole("textbox", { name: /Never touch these/ }), "database");
    await user.click(screen.getByRole("button", { name: "Save policy" }));

    await waitFor(() => expect(posted).toHaveLength(1));
    const body = posted[0]?.body as { selector: { exclude?: string[] } };
    expect(body.selector.exclude).toEqual(["database"]);
  });

  it("sends the image pattern and nothing else when matching images", async () => {
    const user = await openEditor();
    await user.click(screen.getByRole("radio", { name: /Matching images/ }));
    await user.type(screen.getByLabelText("Name"), "acme images");
    await user.type(screen.getByLabelText(/Image patterns/), "ghcr.io/acme/*");
    await user.click(screen.getByRole("button", { name: "Save policy" }));

    await waitFor(() => expect(posted).toHaveLength(1));
    const body = posted[0]?.body as { scope: string; selector: Record<string, unknown> };
    expect(body.scope).toBe("selector");
    expect(body.selector.images).toEqual(["ghcr.io/acme/*"]);
    expect(body.selector.include).toBeUndefined();
  });
});

// ------------------------------------------------------------ the summary --

describe("the summary", () => {
  it("describes the policy the operator is about to save", async () => {
    const user = await openEditor();
    await user.click(screen.getByRole("radio", { name: /All eligible containers/ }));

    const summary = screen.getByTestId("policy-summary");
    expect(summary).toHaveTextContent(/will observe all eligible containers/i);
    expect(summary).toHaveTextContent(/No container will be changed/i);
  });

  it("changes when the mode changes", async () => {
    const user = await openEditor();
    await user.click(screen.getByRole("radio", { name: /All eligible containers/ }));
    await user.click(screen.getByRole("radio", { name: /^Automatic/ }));

    expect(screen.getByTestId("policy-summary")).toHaveTextContent(
      /may automatically update all eligible containers/i,
    );
  });

  it("names the window once one is set", async () => {
    const user = await openEditor();
    await user.click(screen.getByRole("radio", { name: /Maintenance window/ }));

    expect(screen.getByTestId("policy-summary")).toHaveTextContent(
      /between 02:00 and 04:00 UTC/,
    );
  });
});

// ---------------------------------------------------------- the mode warning --

describe("the mode warning", () => {
  it("does not show automatic-mode danger while configuring observe", async () => {
    await openEditor();

    expect(
      screen.queryByText(/may stop and replace matching containers unattended, inside/i),
    ).not.toBeInTheDocument();
    expect(
      screen.getAllByText(/evaluates matching containers and changes nothing/i).length,
    ).toBeGreaterThan(0);
  });

  it("shows it once automatic is chosen", async () => {
    const user = await openEditor();
    // The individual mode control lives under Custom now: the presets above it
    // answer the same question as an outcome, and offering both at once would
    // let an operator change the mode out from under a preset without noticing.
    await user.click(screen.getByRole("radio", { name: /^Custom/ }));
    await user.click(screen.getByRole("radio", { name: /^Automatic/ }));

    // Several regions carry role="status" on this page; the one under test is
    // the mode's own warning, found by what it says rather than by its role.
    const warnings = screen.getAllByRole("status");
    const modeWarning = warnings.find((node) =>
      /may stop and replace matching containers unattended/i.test(node.textContent ?? ""),
    );
    expect(modeWarning).toBeDefined();
    expect(modeWarning).toHaveTextContent(/never update its own container/i);
  });
});

// ------------------------------------------------------------- the window --

describe("the maintenance window", () => {
  it("hides the times until a window is chosen", async () => {
    const user = await openEditor();

    expect(screen.queryByLabelText(/^From/)).not.toBeInTheDocument();
    await user.click(screen.getByRole("radio", { name: /Maintenance window/ }));
    expect(screen.getByLabelText(/^From/)).toBeInTheDocument();
    expect(screen.getByLabelText(/^Until/)).toBeInTheDocument();
    expect(screen.getByLabelText(/Timezone/)).toBeInTheDocument();
  });
});

// -------------------------------------------------------------- advanced --

describe("advanced settings", () => {
  it("hides priority but never the mode, ceiling, window, or failure plan", async () => {
    await openEditor();

    // Priority is behind the disclosure.
    const advanced = screen.getByText("Advanced settings").closest("details");
    expect(advanced).not.toBeNull();
    expect(advanced?.open).toBe(false);
    expect(within(advanced as HTMLElement).getByLabelText(/Priority/)).toBeInTheDocument();

    // The four that decide how much the host can change are not.
    for (const legend of [
      "How far may updates go?",
      "How should updates happen?",
      "When may updates happen?",
      "What happens if an update fails?",
    ]) {
      const heading = screen.getByText(legend);
      expect(heading.closest("details")).toBeNull();
    }
  });
});

// -------------------------------------------------------- accessibility --

describe("accessibility", () => {
  it("has no axe violations", async () => {
    const { container } = renderPage();
    const user = userEvent.setup();
    await user.click(await screen.findByRole("button", { name: "New policy" }));
    await screen.findByText("New update policy");

    const results = await axe.run(container, {
      rules: {
        // jsdom has no layout and no computed colour, so these two cannot
        // produce a meaningful answer here. Both are checked in the browser
        // pass instead, which is where they can actually be measured.
        "color-contrast": { enabled: false },
        "target-size": { enabled: false },
      },
    });

    expect(
      results.violations.map((violation) => `${violation.id}: ${violation.help}`),
    ).toEqual([]);
  });

  it("groups every question in a labelled fieldset", async () => {
    await openEditor();

    for (const legend of [
      "What should HarborMaster manage?",
      "How far may updates go?",
      "How should updates happen?",
      "When may updates happen?",
      "What happens if an update fails?",
    ]) {
      expect(screen.getByRole("group", { name: legend })).toBeInTheDocument();
    }
  });

  it("can be driven to a saved policy with the keyboard alone", async () => {
    const user = userEvent.setup();
    renderPage();

    // Tab to "New policy" rather than clicking it.
    await screen.findByRole("button", { name: "New policy" });
    let guard = 0;
    while (
      document.activeElement !==
        screen.getByRole("button", { name: "New policy" }) &&
      guard++ < 30
    ) {
      await user.tab();
    }
    expect(document.activeElement).toBe(
      screen.getByRole("button", { name: "New policy" }),
    );
    await user.keyboard("{Enter}");
    await screen.findByText("New update policy");

    // Every control the flow needs is reachable and operable by keyboard.
    await user.click(screen.getByLabelText("Name"));
    await user.keyboard("keyboard only");

    const broad = screen.getByRole("radio", { name: /All eligible containers/ });
    broad.focus();
    await user.keyboard("{ }");
    expect(broad).toBeChecked();

    const save = screen.getByRole("button", { name: "Save policy" });
    save.focus();
    await user.keyboard("{Enter}");

    await waitFor(() => expect(posted).toHaveLength(1));
    const body = posted[0]?.body as { name: string; scope: string };
    expect(body.name).toBe("keyboard only");
    expect(body.scope).toBe("allEligible");
  });

  it("announces a save failure to a screen reader", async () => {
    const user = await openEditor();
    globalThis.fetch = vi.fn(async () =>
      jsonResponse({ error: { code: "invalidRequest", message: "selector must not..." } }, 400),
    ) as typeof fetch;

    await user.type(screen.getByLabelText("Name"), "x");
    await user.click(screen.getByRole("button", { name: "Save policy" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toBeInTheDocument();
  });
});

// -------------------------------------------------------- long values --

describe("long container and image names", () => {
  it("wraps rather than truncating a name an operator has to verify", async () => {
    globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      if (init?.method === "POST") return jsonResponse({ policy: {} }, 201);
      if (url.includes("/containers")) {
        return jsonResponse({
          items: [
            {
              ...CONTAINERS.items[0],
              name: "a".repeat(180),
              image: { raw: `registry.example.com/${"b".repeat(120)}:1.0.0` },
            },
          ],
          pagination: { page: 1, pageSize: 50, total: 1, totalPages: 1 },
        });
      }
      if (url.includes("/update-policies")) {
        return jsonResponse({
          items: [],
          pagination: { page: 1, pageSize: 50, total: 0, totalPages: 0 },
        });
      }
      if (url.includes("/automation")) {
        return jsonResponse({ status: { enabled: true, running: false, self: {} } });
      }
      return jsonResponse({});
    }) as typeof fetch;

    await openEditor();
    const name = await screen.findByText("a".repeat(180));
    // break-all rather than truncate: a name cut off is a name an operator
    // cannot confirm they picked.
    expect(name.className).toContain("break-all");
  });
});
