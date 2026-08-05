import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import type {
  PolicyDefinition,
  PolicyRuleCatalogue,
  PolicySummary,
  PolicyViolation,
} from "../api/policyTypes";
import { ContainerPolicy } from "./ContainerPolicy";
import { Policies } from "./Policies";
import { PolicyViolations } from "./PolicyViolations";

/**
 * Policy UI tests.
 *
 * Three properties matter most and are asserted repeatedly:
 *
 *   - "Compliant" and "never evaluated" must READ DIFFERENTLY. Showing an empty
 *     list as a clean bill of health for a container no pass has reached is the
 *     worst thing this feature could do.
 *   - Withdrawing a policy must be described as an ARCHIVE, because that is
 *     what it does. A UI promising a deletion that does not happen is a UI that
 *     lies about the audit trail.
 *   - No environment variable VALUE ever renders. The API cannot send one, and
 *     the UI must not invent one.
 */

const originalFetch = globalThis.fetch;

/** Captures the request URLs the page produced, which is how filter tests work. */
let requests: string[] = [];
/** Captures write requests, so a test can assert method and body. */
let writes: { url: string; method: string; body: string }[] = [];

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(status === 204 ? null : JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

function pagination(totalItems: number) {
  return {
    page: 1,
    pageSize: 25,
    totalItems,
    totalPages: Math.max(1, Math.ceil(totalItems / 25)),
    hasNext: false,
    hasPrevious: false,
  };
}

function policy(overrides: Partial<PolicyDefinition> = {}): PolicyDefinition {
  return {
    policyId: "pol_00112233445566778899",
    name: "Container hardening",
    description: "What production must look like",
    severity: "high",
    enabled: true,
    archived: false,
    rules: [{ type: "privilegedForbidden" }, { type: "userNotRoot" }],
    createdAt: "2026-08-01T09:00:00Z",
    updatedAt: "2026-08-01T09:00:00Z",
    ...overrides,
  };
}

function violation(overrides: Partial<PolicyViolation> = {}): PolicyViolation {
  return {
    id: 1,
    policyId: "pol_00112233445566778899",
    policyName: "Container hardening",
    containerId: "abc123def456",
    containerName: "web",
    ruleType: "privilegedForbidden",
    severity: "critical",
    detectedAt: "2026-08-05T12:00:00Z",
    lastSeenAt: "2026-08-05T12:30:00Z",
    inventoryGeneration: 4,
    observed: "privileged=true",
    expected: "privileged=false",
    reason: "the container runs privileged, which removes the containment boundary",
    status: "active",
    ...overrides,
  };
}

function summary(overrides: Partial<PolicySummary> = {}): PolicySummary {
  return {
    policies: 2,
    policiesTotal: 3,
    total: 5,
    open: 3,
    bySeverity: { critical: 1, high: 2 },
    byStatus: { active: 3, resolved: 2 },
    byRule: { privilegedForbidden: 1 },
    containersEvaluated: 4,
    containersCompliant: 3,
    containersNonCompliant: 1,
    lastEvaluatedAt: "2026-08-05T12:30:00Z",
    incomplete: false,
    ...overrides,
  };
}

function catalogue(): PolicyRuleCatalogue {
  return {
    rules: [
      {
        type: "privilegedForbidden",
        label: "Privileged mode forbidden",
        description: "The container must not run privileged.",
        valueKind: "none",
        requiresValues: false,
      },
      {
        type: "imageAllowlist",
        label: "Image allowlist",
        description: "The image reference must match at least one pattern.",
        valueKind: "imagePattern",
        requiresValues: true,
      },
    ],
    severities: ["critical", "high", "medium", "low"],
    restartPolicyNames: ["no", "always", "on-failure", "unless-stopped"],
    limits: {
      maxRules: 32,
      maxValuesPerRule: 32,
      maxNameBytes: 120,
      maxDescriptionBytes: 1000,
    },
  };
}

/**
 * Installs a fetch double routing by path.
 *
 * Longest fragment first, so "/policy-violations" is not shadowed by a shorter
 * route that happens to be a prefix of it.
 */
function mockApi(routes: Record<string, unknown>, status: Record<string, number> = {}) {
  const ordered = Object.entries(routes).sort((a, b) => b[0].length - a[0].length);

  globalThis.fetch = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === "string" ? input : input.toString();
    const method = init?.method ?? "GET";
    requests.push(url);
    if (method !== "GET") {
      writes.push({ url, method, body: String(init?.body ?? "") });
    }

    for (const [fragment, body] of ordered) {
      if (url.includes(fragment)) return jsonResponse(body, status[fragment] ?? 200);
    }
    return new Response("{}", { status: 404 });
  }) as typeof fetch;
}

beforeEach(() => {
  requests = [];
  writes = [];
});

afterEach(() => {
  globalThis.fetch = originalFetch;
  vi.restoreAllMocks();
});

function renderPolicies() {
  return render(
    <MemoryRouter initialEntries={["/policies"]}>
      <Routes>
        <Route path="/policies" element={<Policies />} />
      </Routes>
    </MemoryRouter>,
  );
}

function renderCompliance() {
  return render(
    <MemoryRouter initialEntries={["/compliance"]}>
      <Routes>
        <Route path="/compliance" element={<PolicyViolations />} />
      </Routes>
    </MemoryRouter>,
  );
}

function renderContainerPolicy(containerId = "abc123def456") {
  return render(
    <MemoryRouter initialEntries={[`/policy/container/${containerId}`]}>
      <Routes>
        <Route path="/policy/container/:id" element={<ContainerPolicy />} />
      </Routes>
    </MemoryRouter>,
  );
}

// -------------------------------------------------------------- policies --

describe("Policies page", () => {
  it("lists policies with their rules", async () => {
    mockApi({
      "/policy-rules": catalogue(),
      "/policies": { items: [policy()], pagination: pagination(1) },
    });

    renderPolicies();

    expect(await screen.findByText("Container hardening")).toBeInTheDocument();
    // Rules render by their human label, not their identifier.
    expect(screen.getByText("Privileged mode forbidden")).toBeInTheDocument();
    expect(screen.getByText("User must not be root")).toBeInTheDocument();
    expect(screen.getByText("enabled")).toBeInTheDocument();
  });

  it("excludes withdrawn policies by default", async () => {
    mockApi({
      "/policy-rules": catalogue(),
      "/policies": { items: [], pagination: pagination(0) },
    });

    renderPolicies();
    await screen.findByText(/No policies defined/);

    const listRequest = requests.find((url) => url.includes("/policies"));
    expect(listRequest).toBeDefined();
    expect(listRequest).toContain("includeArchived=false");
  });

  it("says an estate with no policies has not been asked anything", async () => {
    mockApi({
      "/policy-rules": catalogue(),
      "/policies": { items: [], pagination: pagination(0) },
    });

    renderPolicies();

    // The distinction the whole feature turns on: nothing checked is not the
    // same as everything compliant.
    expect(
      await screen.findByText(/has not been found compliant/),
    ).toBeInTheDocument();
  });

  // DELETE archives. A UI that said "delete" would promise a destruction that
  // does not happen, and the violation history is exactly what survives.
  it("describes withdrawal as keeping the history", async () => {
    mockApi({
      "/policy-rules": catalogue(),
      "/policies": { items: [policy()], pagination: pagination(1) },
    });

    renderPolicies();
    await screen.findByText("Container hardening");

    await userEvent.click(screen.getByRole("button", { name: "Withdraw" }));

    expect(
      await screen.findByText(/definition and the history of what it caught are kept/),
    ).toBeInTheDocument();
    // Nothing has been sent yet: withdrawal takes a confirmation.
    expect(writes).toHaveLength(0);
  });

  it("withdraws through DELETE once confirmed", async () => {
    mockApi(
      {
        "/policy-rules": catalogue(),
        "/policies/pol_00112233445566778899": null,
        "/policies": { items: [policy()], pagination: pagination(1) },
      },
      { "/policies/pol_00112233445566778899": 204 },
    );

    renderPolicies();
    await screen.findByText("Container hardening");

    await userEvent.click(screen.getByRole("button", { name: "Withdraw" }));
    await userEvent.click(await screen.findByRole("button", { name: "Confirm" }));

    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]?.method).toBe("DELETE");
    expect(writes[0]?.url).toContain("/policies/pol_00112233445566778899");
  });

  it("builds the editor from the server's catalogue", async () => {
    mockApi({
      "/policy-rules": catalogue(),
      "/policies": { items: [], pagination: pagination(0) },
    });

    renderPolicies();
    await userEvent.click(await screen.findByRole("button", { name: "New policy" }));

    const picker = await screen.findByRole("combobox", { name: /Add a rule/ });
    // Exactly the two rules the (stubbed) server offered, plus the placeholder.
    // A hardcoded list in the frontend would show sixteen.
    expect(within(picker).getAllByRole("option")).toHaveLength(3);
    expect(
      screen.getByRole("option", { name: "Image allowlist" }),
    ).toBeInTheDocument();
  });

  it("refuses to submit a policy with no rules without contacting the server", async () => {
    mockApi({
      "/policy-rules": catalogue(),
      "/policies": { items: [], pagination: pagination(0) },
    });

    renderPolicies();
    await userEvent.click(await screen.findByRole("button", { name: "New policy" }));

    await userEvent.type(screen.getByRole("textbox", { name: /Name/ }), "My policy");
    await userEvent.click(screen.getByRole("button", { name: "Create policy" }));

    expect(await screen.findByText(/needs at least one rule/)).toBeInTheDocument();
    expect(writes).toHaveLength(0);
  });

  // The server is authoritative on validity, and its message names the field
  // and the constraint. Rendering a generic "something went wrong" instead
  // would leave the operator guessing which of sixteen rules was rejected.
  it("shows the server's rejection message verbatim", async () => {
    mockApi(
      {
        "/policy-rules": catalogue(),
        "/policies": {
          error: {
            code: "invalid_request",
            message: "rules[0].values[0] must be at most 128 bytes",
          },
        },
      },
      { "/policies": 400 },
    );

    renderPolicies();

    expect(
      await screen.findByText(/rules\[0\]\.values\[0\] must be at most 128 bytes/),
    ).toBeInTheDocument();
  });
});

// ------------------------------------------------------------ compliance --

describe("Compliance dashboard", () => {
  it("renders the summary cards and a violation", async () => {
    mockApi({
      "/policy-summary": summary(),
      "/policy-violations": { items: [violation()], pagination: pagination(1) },
    });

    renderCompliance();

    expect(await screen.findByText("Container hardening")).toBeInTheDocument();
    expect(screen.getByText("Open violations")).toBeInTheDocument();
    // The rate is stated over EVALUATED containers, so it cannot be misread as
    // a statement about the whole estate.
    expect(screen.getByText(/3 of 4 evaluated/)).toBeInTheDocument();
    expect(screen.getByText("75%")).toBeInTheDocument();
  });

  it("defaults to open violations only", async () => {
    mockApi({
      "/policy-summary": summary(),
      "/policy-violations": { items: [], pagination: pagination(0) },
    });

    renderCompliance();
    await screen.findByText(/No violations match these filters/);

    const listRequest = requests.find((url) => url.includes("/policy-violations"));
    expect(listRequest).toContain("openOnly=true");
  });

  it("warns when no policies are enabled", async () => {
    mockApi({
      "/policy-summary": summary({ policies: 0 }),
      "/policy-violations": { items: [], pagination: pagination(0) },
    });

    renderCompliance();

    expect(
      await screen.findByText(/has not been found compliant/),
    ).toBeInTheDocument();
  });

  it("reports an incomplete pass rather than averaging it away", async () => {
    mockApi({
      "/policy-summary": summary({ incomplete: true }),
      "/policy-violations": { items: [], pagination: pagination(0) },
    });

    renderCompliance();

    expect(
      await screen.findByText(/a floor rather than a total/),
    ).toBeInTheDocument();
  });

  // The manual pass is asynchronous. The control must say so rather than imply
  // the estate is re-checked by the time the click returns.
  it("requests an evaluation without claiming it finished", async () => {
    mockApi({
      "/policy-summary": summary(),
      "/policy/evaluate": { requested: true, engine: { enabled: true } },
      "/policy-violations": { items: [], pagination: pagination(0) },
    });

    renderCompliance();
    const button = await screen.findByRole("button", { name: "Re-evaluate now" });
    expect(button).toHaveAttribute("title", expect.stringContaining("queued"));

    await userEvent.click(button);

    await waitFor(() => expect(writes.some((w) => w.url.includes("/policy/evaluate"))).toBe(true));
    expect(writes.find((w) => w.url.includes("/policy/evaluate"))?.method).toBe("POST");
  });

  // Acknowledgement does not suppress re-evaluation, and the UI has to say so
  // or an operator will believe they have silenced the check.
  it("offers only the operator statuses, and says they do not stop the checking", async () => {
    mockApi({
      "/policy-summary": summary(),
      "/policy-violations": { items: [violation()], pagination: pagination(1) },
    });

    renderCompliance();
    const acknowledge = await screen.findByRole("button", { name: "Mark acknowledged" });

    expect(acknowledge).toHaveAttribute(
      "title",
      expect.stringContaining("still checked on every pass"),
    );
    expect(screen.getByRole("button", { name: "Mark exempted" })).toBeInTheDocument();
    // There is no resolve control, and there must not be: resolution is
    // something the world does, not something a person asserts.
    expect(screen.queryByRole("button", { name: /resolve/i })).toBeNull();
    // Nor any remediation control, because HarborMaster has no such capability.
    expect(screen.queryByRole("button", { name: /remediate|fix|enforce/i })).toBeNull();
  });

  it("sends the acknowledgement as a PATCH", async () => {
    mockApi({
      "/policy-summary": summary(),
      "/policy-violations/1": violation({ status: "acknowledged" }),
      "/policy-violations": { items: [violation()], pagination: pagination(1) },
    });

    renderCompliance();
    await userEvent.click(
      await screen.findByRole("button", { name: "Mark acknowledged" }),
    );

    await waitFor(() => expect(writes).toHaveLength(1));
    expect(writes[0]?.method).toBe("PATCH");
    expect(writes[0]?.body).toContain('"status":"acknowledged"');
  });
});

// ------------------------------------------------------ container policy --

describe("Container compliance", () => {
  // The distinction the page exists for.
  it("says NOT CHECKED when the container has never been evaluated", async () => {
    mockApi({
      "/policy-violations/container/": {
        containerId: "abc123def456",
        violations: [],
        pagination: pagination(0),
      },
    });

    renderContainerPolicy();

    expect(await screen.findByText(/not checked/)).toBeInTheDocument();
    expect(screen.getByText("This container has not been evaluated")).toBeInTheDocument();
    // It must NOT claim compliance.
    expect(screen.queryByText(/Compliant with every enabled policy/)).toBeNull();
  });

  it("says COMPLIANT only when a complete pass found nothing", async () => {
    mockApi({
      "/policy-violations/container/": {
        containerId: "abc123def456",
        violations: [],
        pagination: pagination(0),
        evaluation: {
          containerId: "abc123def456",
          containerName: "web",
          evaluatedAt: "2026-08-05T12:30:00Z",
          inventoryGeneration: 4,
          policiesEvaluated: 2,
          rulesEvaluated: 5,
          violationCount: 0,
          compliant: true,
          complete: true,
        },
      },
    });

    renderContainerPolicy();

    expect(
      await screen.findByText("Compliant with every enabled policy"),
    ).toBeInTheDocument();
    expect(screen.queryByText(/not checked/)).toBeNull();
  });

  it("warns when the pass could not apply every policy", async () => {
    mockApi({
      "/policy-violations/container/": {
        containerId: "abc123def456",
        violations: [violation()],
        pagination: pagination(1),
        evaluation: {
          containerId: "abc123def456",
          containerName: "web",
          evaluatedAt: "2026-08-05T12:30:00Z",
          inventoryGeneration: 4,
          policiesEvaluated: 2,
          rulesEvaluated: 3,
          violationCount: 1,
          compliant: false,
          complete: false,
          reason: "the container exceeded its violation budget",
        },
      },
    });

    renderContainerPolicy();

    expect(
      await screen.findByText(/could not apply every policy/),
    ).toBeInTheDocument();
    expect(screen.getByText(/the list may be incomplete/)).toBeInTheDocument();
  });

  // An environment rule reports the offending NAMES. A value must never appear,
  // and the API has none to send.
  it("renders environment violations by name and never by value", async () => {
    mockApi({
      "/policy-violations/container/": {
        containerId: "abc123def456",
        violations: [
          violation({
            id: 2,
            ruleType: "forbiddenEnv",
            observed: "AWS_SECRET_ACCESS_KEY",
            expected: "no environment variable matching one of AWS_*",
            reason: "a forbidden environment variable is present: AWS_SECRET_ACCESS_KEY",
          }),
        ],
        pagination: pagination(1),
        evaluation: {
          containerId: "abc123def456",
          containerName: "web",
          evaluatedAt: "2026-08-05T12:30:00Z",
          inventoryGeneration: 4,
          policiesEvaluated: 1,
          rulesEvaluated: 1,
          violationCount: 1,
          compliant: false,
          complete: true,
        },
      },
    });

    const { container } = renderContainerPolicy();

    // The page renders the name in both the timeline and the card, so it is
    // matched as a set rather than as a single node.
    expect(
      (await screen.findAllByText(/AWS_SECRET_ACCESS_KEY/)).length,
    ).toBeGreaterThan(0);
    // Nothing that looks like a value. The fixture deliberately never contains
    // one, so this asserts the component invents nothing either.
    expect(container.textContent).not.toContain("undefined");
    expect(container.textContent).not.toMatch(/AWS_SECRET_ACCESS_KEY=/);
  });
});
