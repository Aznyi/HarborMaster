import { act, render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SessionProvider } from "../hooks/useSession";
import { MemoryRouter, Route, Routes } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { ApiError } from "../api/client";
import { App } from "../App";
import { ContainerDetailPage } from "./ContainerDetail";
import { Events } from "./Events";
import {
  dockerEvent,
  eventEngineStatus,
  eventPage,
  fakeStreamFactory,
  HttpFailure,
  lastEventQuery,
  stubApi,
  type FakeEventStream,
  type RecordedRequest,
} from "../test/fixtures";
import { MAX_LIVE_EVENTS } from "../hooks/useDockerEvents";

function renderEvents(factory?: (url: string) => FakeEventStream) {
  return render(
    <MemoryRouter initialEntries={["/events"]}>
      <Routes>
        <Route
          path="/events"
          element={<Events {...(factory ? { streamFactory: factory } : {})} />}
        />
        <Route path="/containers/:id" element={<div>container page</div>} />
      </Routes>
    </MemoryRouter>,
  );
}

/** Waits until the engine panel has rendered, i.e. status has arrived. */
async function waitForEngine() {
  await waitFor(() =>
    expect(screen.getByRole("region", { name: /event engine/i })).toBeInTheDocument(),
  );
}

beforeEach(() => {
  vi.unstubAllGlobals();
});

// -------------------------------------------------------------- states --

describe("Events page states", () => {
  it("shows a loading state before the engine status arrives", () => {
    stubApi();
    renderEvents();

    expect(
      screen.getByRole("status", { name: /loading event engine status/i }),
    ).toBeInTheDocument();
  });

  it("renders an empty state when nothing has been recorded", async () => {
    stubApi({ events: eventPage([], 0) });
    renderEvents();

    await waitFor(() =>
      expect(screen.getByText(/no events recorded yet/i)).toBeInTheDocument(),
    );
    // The empty state must explain the gap rather than implying nothing happened.
    expect(screen.getByText(/event history begins when the engine first connects/i))
      .toBeInTheDocument();
  });

  it("renders a disconnected state when the backend is unreachable", async () => {
    stubApi({ eventEngine: new ApiError("network_error", "Cannot reach the backend") });
    renderEvents();

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/cannot reach the harbormaster backend/i),
    );
  });

  it("renders an error state when the backend answers with an error", async () => {
    // An HTTP error, not an unreachable backend: the two states have different
    // remedies and must not be collapsed.
    stubApi({ eventEngine: new HttpFailure(500, "internal_error", "internal error") });
    renderEvents();

    await waitFor(() =>
      expect(screen.getByRole("alert")).toHaveTextContent(/something went wrong/i),
    );
  });

  it("renders the disabled state without offering a live stream", async () => {
    stubApi({ eventEngine: eventEngineStatus({ enabled: false, state: "disabled" }) });
    const { factory, streams } = fakeStreamFactory();
    renderEvents(factory);

    await waitForEngine();

    expect(screen.getByText(/switched off by configuration/i)).toBeInTheDocument();
    // No live panel, and crucially no connection attempt.
    expect(screen.queryByRole("region", { name: /^live$/i })).not.toBeInTheDocument();
    expect(streams).toHaveLength(0);
  });

  it("warns when the event stream is disconnected", async () => {
    stubApi({
      eventEngine: eventEngineStatus({
        state: "reconnecting",
        lastError: "docker engine unreachable",
        counters: { ...eventEngineStatus().counters, reconnectCount: 3 },
      }),
    });
    renderEvents();

    await waitForEngine();

    const panel = screen.getByRole("region", { name: /event engine/i });
    // The word appears in both the badge and the explanation, which is the
    // point: the state is legible without reading the prose, and the prose
    // says what it means for the inventory.
    expect(within(panel).getAllByText(/reconnecting/i).length).toBeGreaterThan(0);
    expect(within(panel).getByText(/periodic reconciliation keeps the inventory correct/i))
      .toBeInTheDocument();
    expect(within(panel).getByText("3")).toBeInTheDocument();
  });

  it("warns when the queue has overflowed", async () => {
    stubApi({ eventEngine: eventEngineStatus({ overflowPending: true }) });
    renderEvents();

    await waitForEngine();

    expect(screen.getByRole("alert")).toHaveTextContent(/event queue overflowed/i);
  });
});

// ---------------------------------------------------------------- history --

describe("Events history", () => {
  it("renders rows with both timestamps and a status badge", async () => {
    stubApi({
      events: eventPage([
        dockerEvent({ sequence: 2, action: "die", result: "processed" }),
        dockerEvent({ sequence: 1, fingerprint: "fp-0", action: "start" }),
      ]),
    });
    renderEvents();

    const history = await screen.findByRole("region", { name: /history/i });
    const table = within(history).getByRole("table");

    // Docker time and observed time are separate columns, always both: they
    // diverge after a reconnect, and that gap is what an operator needs.
    expect(within(table).getByRole("columnheader", { name: /docker time/i })).toBeInTheDocument();
    expect(within(table).getByRole("columnheader", { name: /observed/i })).toBeInTheDocument();

    expect(within(table).getByText("die")).toBeInTheDocument();
    expect(within(table).getAllByText("processed").length).toBeGreaterThan(0);
  });

  it("links a container event to its container", async () => {
    stubApi({ events: eventPage([dockerEvent({ actorId: "abcdef0123456789" })]) });
    renderEvents();

    const history = await screen.findByRole("region", { name: /history/i });
    const link = within(history).getByRole("link", { name: "web" });

    expect(link).toHaveAttribute("href", "/containers/abcdef0123456789");
  });

  it("sends filters to the server rather than filtering in the browser", async () => {
    const requests = stubApi();
    renderEvents();

    await screen.findByRole("region", { name: /history/i });

    const user = userEvent.setup();
    await user.selectOptions(screen.getByLabelText(/^type$/i), "image");
    await waitFor(() => expect(lastEventQuery(requests).get("type")).toBe("image"));

    await user.selectOptions(screen.getByLabelText(/^action$/i), "pull");
    await waitFor(() => expect(lastEventQuery(requests).get("action")).toBe("pull"));

    await user.selectOptions(screen.getByLabelText(/compose project/i), "shop");
    await waitFor(() => expect(lastEventQuery(requests).get("project")).toBe("shop"));

    await user.selectOptions(screen.getByLabelText(/processing status/i), "warning");
    await waitFor(() => expect(lastEventQuery(requests).get("result")).toBe("warning"));

    await user.type(screen.getByLabelText(/^search$/i), "nginx");
    await waitFor(() => expect(lastEventQuery(requests).get("search")).toBe("nginx"));

    // Every filter change resets to page 1.
    expect(lastEventQuery(requests).get("page")).toBe("1");
  });

  it("shows a filtered empty state distinct from the unfiltered one", async () => {
    stubApi({ events: eventPage([], 0) });
    renderEvents();

    await screen.findByText(/no events recorded yet/i);

    const user = userEvent.setup();
    await user.selectOptions(screen.getByLabelText(/^type$/i), "volume");

    await waitFor(() =>
      expect(screen.getByText(/no events match these filters/i)).toBeInTheDocument(),
    );
  });

  it("pages through history on the server", async () => {
    const requests = stubApi({
      events: eventPage([dockerEvent()], 60),
    });
    renderEvents();

    await screen.findByRole("region", { name: /history/i });

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /next/i }));

    await waitFor(() => expect(lastEventQuery(requests).get("page")).toBe("2"));
  });

  it("never renders an unmasked sensitive attribute", async () => {
    stubApi({
      events: eventPage([
        dockerEvent({
          attributes: { name: "web", DB_PASSWORD: "********" },
        }),
      ]),
    });
    const { container } = renderEvents();

    await screen.findByRole("region", { name: /history/i });

    // The API masks before storing; this asserts the UI cannot un-mask, and
    // that no test fixture accidentally carries a real-looking secret.
    expect(container.textContent).not.toMatch(/hunter2|s3cret|password=/i);
  });
});

// ------------------------------------------------------------------- live --

describe("Events live stream", () => {
  it("opens a stream and appends delivered events", async () => {
    stubApi();
    const { factory, streams } = fakeStreamFactory();
    renderEvents(factory);

    await waitForEngine();
    await waitFor(() => expect(streams).toHaveLength(1));

    const stream = streams[0]!;
    act(() => {
      stream.open();
      stream.emit("ready", { lastEventId: 0, replayed: 0, redacted: true, notice: "" });
    });

    const live = screen.getByRole("region", { name: /^live$/i });
    await waitFor(() => expect(within(live).getByText("live")).toBeInTheDocument());

    act(() => {
      stream.emit("docker-event", dockerEvent({ sequence: 99, action: "restart" }));
    });

    await waitFor(() =>
      expect(within(screen.getByRole("region", { name: /^live$/i })).getByText("restart"))
        .toBeInTheDocument(),
    );
  });

  // The server ends a stream whose session stopped being valid, and the client
  // must STOP reconnecting. Without this, EventSource retries on its own timer
  // forever and every retry is a 401.
  it("closes the stream and stops reconnecting when the session ends", async () => {
    stubApi();
    const { factory, streams } = fakeStreamFactory();
    renderEvents(factory);

    await waitForEngine();
    await waitFor(() => expect(streams).toHaveLength(1));

    const stream = streams[0]!;
    act(() => {
      stream.open();
      stream.emit("ready", { lastEventId: 0, replayed: 0, redacted: true, notice: "" });
    });

    const live = await screen.findByRole("region", { name: /^live$/i });
    await waitFor(() => expect(within(live).getByText("live")).toBeInTheDocument());

    act(() => {
      stream.emit("closed", { reason: "your session is no longer valid; sign in again" });
    });

    // The underlying connection is closed rather than left to retry.
    await waitFor(() => expect(stream.closed).toBe(true));
    await waitFor(() =>
      expect(
        within(screen.getByRole("region", { name: /^live$/i })).getByText("signed out"),
      ).toBeInTheDocument(),
    );
    // And no replacement stream was opened.
    expect(streams).toHaveLength(1);
  });

  it("reports reconnecting without claiming a failure", async () => {
    stubApi();
    const { factory, streams } = fakeStreamFactory();
    renderEvents(factory);

    await waitForEngine();
    await waitFor(() => expect(streams).toHaveLength(1));

    act(() => streams[0]!.fail());

    const live = await screen.findByRole("region", { name: /^live$/i });
    await waitFor(() =>
      expect(within(live).getByText(/the live connection dropped/i)).toBeInTheDocument(),
    );
    // The browser retries on its own, so this must not read as a dead stream.
    expect(within(live).getByText("reconnecting")).toBeInTheDocument();
  });

  it("pauses and resumes the live list without closing the connection", async () => {
    stubApi();
    const { factory, streams } = fakeStreamFactory();
    renderEvents(factory);

    await waitForEngine();
    await waitFor(() => expect(streams).toHaveLength(1));
    const stream = streams[0]!;

    act(() => stream.emit("docker-event", dockerEvent({ sequence: 10, action: "start" })));
    await waitFor(() =>
      expect(within(screen.getByRole("region", { name: /^live$/i })).getByText("start"))
        .toBeInTheDocument(),
    );

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /pause/i }));

    act(() => stream.emit("docker-event", dockerEvent({ sequence: 11, action: "unpause" })));

    const live = screen.getByRole("region", { name: /^live$/i });
    expect(within(live).getByText(/paused/i)).toBeInTheDocument();
    expect(within(live).queryByText("unpause")).not.toBeInTheDocument();
    // Crucially: the connection stayed open, so resuming does not replay.
    expect(stream.closed).toBe(false);

    await user.click(screen.getByRole("button", { name: /resume/i }));
    act(() => stream.emit("docker-event", dockerEvent({ sequence: 12, action: "kill" })));

    await waitFor(() =>
      expect(within(screen.getByRole("region", { name: /^live$/i })).getByText("kill"))
        .toBeInTheDocument(),
    );
  });

  it("bounds the live list so it cannot grow without limit", async () => {
    stubApi();
    const { factory, streams } = fakeStreamFactory();
    renderEvents(factory);

    await waitForEngine();
    await waitFor(() => expect(streams).toHaveLength(1));
    const stream = streams[0]!;

    const overflow = MAX_LIVE_EVENTS + 25;
    act(() => {
      for (let i = 1; i <= overflow; i++) {
        stream.emit("docker-event", dockerEvent({ sequence: i, fingerprint: `fp-${i}` }));
      }
    });

    await waitFor(() => {
      const live = screen.getByRole("region", { name: /^live$/i });
      const rows = within(live).getAllByRole("row");
      // One header row plus at most the cap.
      expect(rows.length).toBeLessThanOrEqual(MAX_LIVE_EVENTS + 1);
      expect(rows.length).toBeGreaterThan(1);
    });
  });

  it("drops an event already held, so a replay cannot duplicate a row", async () => {
    stubApi();
    const { factory, streams } = fakeStreamFactory();
    renderEvents(factory);

    await waitForEngine();
    await waitFor(() => expect(streams).toHaveLength(1));
    const stream = streams[0]!;

    act(() => {
      stream.emit("docker-event", dockerEvent({ sequence: 5, action: "oom" }));
      stream.emit("docker-event", dockerEvent({ sequence: 5, action: "oom" }));
    });

    await waitFor(() => {
      const live = screen.getByRole("region", { name: /^live$/i });
      expect(within(live).getAllByText("oom")).toHaveLength(1);
    });
  });

  it("reports a truncated replay rather than hiding the gap", async () => {
    stubApi();
    const { factory, streams } = fakeStreamFactory();
    renderEvents(factory);

    await waitForEngine();
    await waitFor(() => expect(streams).toHaveLength(1));

    act(() =>
      streams[0]!.emit("replay-truncated", { skipped: 314, limit: 200, notice: "capped" }),
    );

    await waitFor(() =>
      expect(screen.getByText(/314 events were skipped/i)).toBeInTheDocument(),
    );
  });

  it("closes the stream when live updates are switched off", async () => {
    stubApi();
    const { factory, streams } = fakeStreamFactory();
    renderEvents(factory);

    await waitForEngine();
    await waitFor(() => expect(streams).toHaveLength(1));

    const user = userEvent.setup();
    await user.click(screen.getByRole("button", { name: /disconnect/i }));

    await waitFor(() => expect(streams[0]!.closed).toBe(true));
  });

  it("tolerates a frame that will not parse", async () => {
    stubApi();
    const { factory, streams } = fakeStreamFactory();
    renderEvents(factory);

    await waitForEngine();
    await waitFor(() => expect(streams).toHaveLength(1));

    // A malformed frame must be dropped, not crash the view.
    act(() => streams[0]!.emit("docker-event", "{not json"));

    expect(screen.getByRole("region", { name: /^live$/i })).toBeInTheDocument();
  });
});

// -------------------------------------------------------------- dashboard --

describe("Dashboard event engine panel", () => {
  function renderApp(path = "/") {
    return render(
      <MemoryRouter initialEntries={[path]}>
        <SessionProvider>
          <App />
        </SessionProvider>
      </MemoryRouter>,
    );
  }

  it("shows connection state and counters", async () => {
    stubApi();
    renderApp();

    const panel = await screen.findByRole("region", { name: /docker events/i });

    expect(within(panel).getByText("connected")).toBeInTheDocument();
    expect(within(panel).getByText(/last event/i)).toBeInTheDocument();
    expect(within(panel).getByText(/last reconciliation/i)).toBeInTheDocument();
    expect(within(panel).getByText("0 / 1024")).toBeInTheDocument();
  });

  it("warns that polling is carrying the inventory when events are disconnected", async () => {
    stubApi({ eventEngine: eventEngineStatus({ state: "reconnecting" }) });
    renderApp();

    const panel = await screen.findByRole("region", { name: /docker events/i });

    expect(within(panel).getByRole("alert")).toHaveTextContent(
      /relying on periodic reconciliation/i,
    );
  });

  it("presents a disabled engine as a supported mode, not a fault", async () => {
    stubApi({ eventEngine: eventEngineStatus({ enabled: false, state: "disabled" }) });
    renderApp();

    const panel = await screen.findByRole("region", { name: /docker events/i });

    expect(within(panel).getByText(/supported mode/i)).toBeInTheDocument();
    expect(within(panel).queryByRole("alert")).not.toBeInTheDocument();
  });
});

// -------------------------------------------------- container detail events --

describe("Container detail events tab", () => {
  it("requests only this container's events", async () => {
    const requests: RecordedRequest[] = stubApi();

    render(
      <MemoryRouter initialEntries={["/containers/abcdef0123456789"]}>
        <Routes>
          <Route path="/containers/:id" element={<ContainerDetailPage />} />
        </Routes>
      </MemoryRouter>,
    );

    await screen.findByRole("tab", { name: /events/i });

    const user = userEvent.setup();
    await user.click(screen.getByRole("tab", { name: /events/i }));

    await waitFor(() => {
      const query = lastEventQuery(requests);
      expect(query.get("actorId")).toBe("abcdef0123456789");
      // Bounded: a detail page must not run an unbounded event query.
      expect(Number(query.get("pageSize"))).toBeLessThanOrEqual(50);
    });
  });
});
