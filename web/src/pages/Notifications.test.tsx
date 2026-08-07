import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, expect, it, vi } from "vitest";

import type {
  NotificationDelivery,
  NotificationDestination,
  NotificationRule,
  NotificationStatus,
} from "../api/notificationTypes";
import { Notifications } from "./Notifications";
import { TestSessionProvider, testSession, testUser } from "../test/session";

/**
 * Notification UI tests.
 *
 * The properties under test are the ones an operator's trust depends on:
 *
 *   - A deployment that is not delivering SAYS SO, before any control.
 *   - A broken destination says so on its own card, because a revoked webhook
 *     URL otherwise fails silently.
 *   - `suppressed` and `failed` are visibly different things.
 *   - The credential travels one way: the URL field is never pre-filled, an
 *     edit that leaves it blank sends no url at all, and no response value ever
 *     reaches the page.
 *   - A viewer is offered no control, and an operator is offered no control:
 *     configuring destinations is an administrator's decision.
 *   - Text that came from a destination's name is rendered as TEXT.
 */

const originalFetch = globalThis.fetch;

let requests: { url: string; method: string; body: string }[] = [];

const destinationID = "ndst_00112233445566778899";
const ruleID = "nrul_00112233445566778899";
const deliveryID = "ndlv_00112233445566778899";

function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
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

function sampleStatus(overrides: Partial<NotificationStatus> = {}): NotificationStatus {
  return {
    enabled: true,
    destinations: 1,
    rules: 1,
    failing: 0,
    channels: ["slack", "discord", "teams", "webhook", "email"],
    events: ["execution.failed", "rollback.failed"],
    severities: ["info", "warning", "critical"],
    ...overrides,
  };
}

function sampleDestination(
  overrides: Partial<NotificationDestination> = {},
): NotificationDestination {
  return {
    destinationId: destinationID,
    name: "Operations chat",
    channel: "slack",
    enabled: true,
    endpoint: "https://hooks.example.test",
    consecutiveFailures: 0,
    archived: false,
    createdAt: "2026-08-01T00:00:00Z",
    updatedAt: "2026-08-01T00:00:00Z",
    ...overrides,
  };
}

function sampleRule(overrides: Partial<NotificationRule> = {}): NotificationRule {
  return {
    ruleId: ruleID,
    name: "Things that went wrong",
    enabled: true,
    minimumSeverity: "warning",
    destinations: [destinationID],
    cooldownSeconds: 900,
    archived: false,
    createdAt: "2026-08-01T00:00:00Z",
    updatedAt: "2026-08-01T00:00:00Z",
    ...overrides,
  };
}

function sampleDelivery(
  overrides: Partial<NotificationDelivery> = {},
): NotificationDelivery {
  return {
    deliveryId: deliveryID,
    destinationId: destinationID,
    destinationName: "Operations chat",
    channel: "slack",
    ruleId: ruleID,
    ruleName: "Things that went wrong",
    event: "execution.failed",
    severity: "critical",
    title: "web could not be updated",
    result: "succeeded",
    attempts: 1,
    queuedAt: "2026-08-06T12:00:00Z",
    ...overrides,
  };
}

interface StubOptions {
  status?: NotificationStatus;
  destinations?: NotificationDestination[];
  rules?: NotificationRule[];
  deliveries?: NotificationDelivery[];
}

function stub(options: StubOptions = {}) {
  requests = [];

  vi.stubGlobal(
    "fetch",
    vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      requests.push({
        url,
        method: init?.method ?? "GET",
        body: typeof init?.body === "string" ? init.body : "",
      });

      if (url.includes("/notifications/destinations/") && url.endsWith("/test")) {
        return jsonResponse({
          status: "queued",
          detail: "the test notification was queued",
        });
      }
      if (url.includes("/notifications/destinations")) {
        if ((init?.method ?? "GET") !== "GET") {
          return jsonResponse({ destination: sampleDestination(), warnings: [] }, 201);
        }
        const items = options.destinations ?? [];
        return jsonResponse({ items, pagination: pagination(items.length) });
      }
      if (url.includes("/notifications/rules")) {
        if ((init?.method ?? "GET") !== "GET") {
          return jsonResponse({ rule: sampleRule(), warnings: [] }, 201);
        }
        const items = options.rules ?? [];
        return jsonResponse({ items, pagination: pagination(items.length) });
      }
      if (url.includes("/notifications/deliveries")) {
        const items = options.deliveries ?? [];
        return jsonResponse({ items, pagination: pagination(items.length) });
      }
      if (url.includes("/notifications")) {
        return jsonResponse(options.status ?? sampleStatus());
      }
      return jsonResponse({ items: [], pagination: pagination(0) });
    }),
  );
}

function renderPage(role: "administrator" | "operator" | "viewer" = "administrator") {
  return render(
    <TestSessionProvider session={testSession({ user: testUser(role) })}>
      <MemoryRouter initialEntries={["/notifications"]}>
        <Notifications />
      </MemoryRouter>
    </TestSessionProvider>,
  );
}

beforeEach(() => {
  stub();
});

afterEach(() => {
  vi.unstubAllGlobals();
  globalThis.fetch = originalFetch;
});

// A deployment that is not delivering says so before anything else.
it("says plainly when nothing is being delivered", async () => {
  stub({ status: sampleStatus({ enabled: false }), destinations: [sampleDestination()] });
  renderPage();

  expect(
    await screen.findByText(/Nothing is being delivered/i),
  ).toBeInTheDocument();
  expect(
    screen.getByText(/HARBORMASTER_NOTIFICATIONS_ENABLED/),
  ).toBeInTheDocument();
});

// And a deployment that IS delivering does not show the warning.
it("shows no disabled notice when delivery is on", async () => {
  stub({ destinations: [sampleDestination()] });
  renderPage();

  await screen.findByText("Operations chat");
  expect(screen.queryByText(/Nothing is being delivered/i)).toBeNull();
});

// A revoked webhook URL fails silently. This is where it stops being silent.
it("says on the card when a destination is not working", async () => {
  stub({
    status: sampleStatus({ failing: 1 }),
    destinations: [
      sampleDestination({
        consecutiveFailures: 4,
        lastResult: "failed",
        lastError: "the destination rejected the request",
        lastAttemptAt: "2026-08-06T11:00:00Z",
      }),
    ],
  });
  renderPage();

  expect(
    await screen.findByText(/The last 4 deliveries to this destination failed/i),
  ).toBeInTheDocument();
  expect(
    screen.getByText(/Nothing routed here is reaching anybody/i),
  ).toBeInTheDocument();
  expect(
    screen.getByText(/1 destination is not working|One destination is not working/i),
  ).toBeInTheDocument();
});

// Suppressed is not failed, and the page says which is which.
it("distinguishes a suppressed delivery from a failed one", async () => {
  stub({
    destinations: [sampleDestination()],
    deliveries: [
      sampleDelivery({ deliveryId: "ndlv_1", result: "suppressed" }),
      sampleDelivery({
        deliveryId: "ndlv_2",
        result: "failed",
        error: "the destination could not be reached",
      }),
      sampleDelivery({ deliveryId: "ndlv_3", result: "dropped" }),
    ],
  });
  renderPage();

  expect(await screen.findByText("Suppressed")).toBeInTheDocument();
  expect(screen.getByText("Not delivered")).toBeInTheDocument();
  // The detail column says what suppressed MEANS rather than repeating the
  // badge, which is what stops it being read as a failure.
  expect(
    screen.getByText(/decided not to send a repeat/i),
  ).toBeInTheDocument();
  // "Lost" rather than "failed": HarborMaster losing a notification is a
  // different fact from a destination refusing one.
  expect(screen.getByText("Lost")).toBeInTheDocument();
});

// The credential travels one way.
it("never pre-fills the webhook URL and sends none when the field is blank", async () => {
  stub({ destinations: [sampleDestination()] });
  renderPage();

  await screen.findByText("Operations chat");
  await userEvent.click(screen.getByRole("button", { name: /^Edit$/i }));

  const field = await screen.findByLabelText(/Webhook URL/i);
  expect(field).toHaveValue("");
  expect(field).toHaveAttribute("type", "password");
  expect(field).toHaveAttribute("autocomplete", "off");
  expect(screen.getByText(/Leave blank to keep the stored URL/i)).toBeInTheDocument();

  await userEvent.click(screen.getByRole("button", { name: /Save changes/i }));

  await waitFor(() => {
    const write = requests.find((entry) => entry.method === "PATCH");
    expect(write).toBeDefined();
    // The whole point: no `url` key at all, so the server leaves the stored
    // credential alone.
    expect(write?.body).not.toContain("url");
  });
});

// A supplied URL does reach the server. A form that silently dropped it would
// also pass the leak checks and would be a worse bug.
it("sends the webhook URL when one is entered", async () => {
  stub({ destinations: [] });
  renderPage();

  await screen.findByText(/No destinations/i);
  await userEvent.click(screen.getByRole("button", { name: /New destination/i }));

  await userEvent.type(await screen.findByLabelText(/Name/i), "Chat");
  await userEvent.type(
    screen.getByLabelText(/Webhook URL/i),
    "https://hooks.example.test/services/T/B/SECRET",
  );
  await userEvent.click(screen.getByRole("button", { name: /Create destination/i }));

  await waitFor(() => {
    const write = requests.find((entry) => entry.method === "POST");
    expect(write?.body).toContain("https://hooks.example.test/services/T/B/SECRET");
  });
});

// Configuring where this host sends data is an administrator's decision.
it("offers an operator no way to configure a destination", async () => {
  stub({ destinations: [sampleDestination()], rules: [sampleRule()] });
  renderPage("operator");

  await screen.findAllByText("Operations chat");
  expect(screen.queryByRole("button", { name: /New destination/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /New rule/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /^Edit$/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /Send test/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /Withdraw/i })).toBeNull();
});

// But an operator can still answer "was anybody told about this".
it("shows an operator the delivery history", async () => {
  stub({
    destinations: [sampleDestination()],
    deliveries: [sampleDelivery()],
  });
  renderPage("operator");

  expect(await screen.findByText("web could not be updated")).toBeInTheDocument();
  expect(screen.getAllByText("Delivered").length).toBeGreaterThan(0);
});

// A viewer sees the record and is offered nothing.
it("offers a viewer no control", async () => {
  stub({ destinations: [sampleDestination()], deliveries: [sampleDelivery()] });
  renderPage("viewer");

  await screen.findAllByText("Operations chat");
  expect(screen.queryByRole("button", { name: /New destination/i })).toBeNull();
  expect(screen.queryByRole("button", { name: /Send test/i })).toBeNull();
});

// A test cannot be sent on a deployment that never drains the queue.
it("disables the test control while delivery is off", async () => {
  stub({
    status: sampleStatus({ enabled: false }),
    destinations: [sampleDestination()],
  });
  renderPage();

  const button = await screen.findByRole("button", { name: /Send test/i });
  expect(button).toBeDisabled();
  expect(button).toHaveAttribute(
    "title",
    expect.stringContaining("Delivery is switched off"),
  );
});

// The test send names only an identifier HarborMaster issued.
it("sends a test that carries no target", async () => {
  stub({ destinations: [sampleDestination()] });
  renderPage();

  await screen.findByText("Operations chat");
  await userEvent.click(screen.getByRole("button", { name: /Send test/i }));

  await waitFor(() => {
    const write = requests.find(
      (entry) => entry.method === "POST" && entry.url.endsWith("/test"),
    );
    expect(write).toBeDefined();
    expect(write?.url).toContain(destinationID);
    // Nothing in the body names a URL, a host, or a message.
    expect(write?.body).toBe("{}");
  });
  expect(
    await screen.findByText(/the test notification was queued/i),
  ).toBeInTheDocument();
});

// A rule with no events selected matches every event, and says so.
it("states that an empty event selection means every event", async () => {
  stub({ destinations: [sampleDestination()], rules: [sampleRule({ events: [] })] });
  renderPage();

  expect(await screen.findByText("every event")).toBeInTheDocument();
});

// A zero cooldown is stated as what it means, not as a number.
it("renders a zero cooldown as every occurrence", async () => {
  stub({
    destinations: [sampleDestination()],
    rules: [sampleRule({ cooldownSeconds: 0 })],
  });
  renderPage();

  expect(await screen.findByText("every occurrence is sent")).toBeInTheDocument();
});

// Text that came from a record is rendered as TEXT.
it("renders a hostile destination name as text", async () => {
  stub({
    destinations: [
      sampleDestination({ name: "<img src=x onerror=alert(1)>chat" }),
    ],
  });
  const { container } = renderPage();

  expect(
    await screen.findByText(/<img src=x onerror=alert\(1\)>chat/),
  ).toBeInTheDocument();
  expect(container.querySelector("img")).toBeNull();
});

// An empty deployment says what to do next rather than showing a blank page.
it("tells an administrator what to do first", async () => {
  stub({ destinations: [], rules: [] });
  renderPage();

  expect(await screen.findByText(/No destinations/i)).toBeInTheDocument();
  expect(
    screen.getByText(/Add a destination, send a test to prove it works/i),
  ).toBeInTheDocument();
  // And a rule cannot be written before there is anywhere to route to.
  expect(screen.getByRole("button", { name: /New rule/i })).toBeDisabled();
});
