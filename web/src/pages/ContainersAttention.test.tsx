import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import { Containers } from "./Containers";
import {
  containerPage,
  containerRow,
  lastContainerQuery,
  stubApi,
  type RecordedRequest,
} from "../test/fixtures";

/**
 * What a container row tells a Docker administrator.
 *
 * # Why this file exists separately from the sort tests
 *
 * The audit finding was not that the table was wrong -- it was that the table
 * was the same seven columns `docker ps` gives, so every HarborMaster-specific
 * fact required opening a row. The properties below are about whether the row
 * now answers the questions an operator actually has, and whether it answers
 * them HONESTLY:
 *
 *   - "not checked" never reads as "up to date"
 *   - "cannot advise" never reads as "nothing to do"
 *   - a container following no tag says so, rather than showing a blank
 *   - the containers HarborMaster parked are out of the default view, are
 *     explained when shown, and are never presented as workloads
 *   - the page says how many rows need somebody, so the answer to "is anything
 *     wrong here" does not require reading every row
 */

function renderTable() {
  return render(
    <MemoryRouter initialEntries={["/containers"]}>
      <Containers />
    </MemoryRouter>,
  );
}

let requests: RecordedRequest[] = [];

beforeEach(() => {
  vi.unstubAllGlobals();
  requests = [];
});

afterEach(() => {
  vi.unstubAllGlobals();
});

/** The row whose name cell contains the given text. */
async function rowFor(name: string): Promise<HTMLElement> {
  const link = await screen.findByRole("link", { name });
  const row = link.closest("tr");
  if (!row) throw new Error(`no <tr> wraps "${name}"`);
  return row;
}

// ------------------------------------------------------- honest verdicts --

describe("the attention column", () => {
  it("says a container has not been checked rather than that it is current", async () => {
    // The property the whole model rests on. These two would be defensible as
    // the same grey badge and mean opposite things to somebody deciding
    // whether to act.
    requests = stubApi({
      containers: containerPage([
        containerRow({ name: "unchecked" }, { state: "notChecked", updateType: undefined }),
        containerRow({ name: "current", id: "b1", shortId: "b1" }, { state: "upToDate" }),
      ]),
    });

    renderTable();

    expect(within(await rowFor("unchecked")).getByText("Not checked")).toBeInTheDocument();
    expect(within(await rowFor("current")).getByText("Up to date")).toBeInTheDocument();
  });

  it("spells out the update column instead of leaving a blank", async () => {
    // An empty cell under "Update" reads as "no update", which is exactly the
    // claim HarborMaster cannot make about a container it never assessed.
    requests = stubApi({
      containers: containerPage([
        containerRow({ name: "unchecked" }, { state: "notChecked" }),
      ]),
    });

    renderTable();

    const row = await rowFor("unchecked");
    expect(within(row).getByText(/not assessed yet/i)).toBeInTheDocument();
  });

  it("says an untracked container will never get an update, not that it has none", async () => {
    requests = stubApi({
      containers: containerPage([
        containerRow(
          { name: "pinned" },
          { state: "notTracked", trackingKnown: true, tracking: undefined },
        ),
      ]),
    });

    renderTable();

    const row = await rowFor("pinned");
    expect(within(row).getByText("Not tracked")).toBeInTheDocument();
    expect(within(row).getByText(/no tag is followed/i)).toBeInTheDocument();
    expect(within(row).getByText(/follows no tag/i)).toBeInTheDocument();
  });

  it("reports the absence of a judgement as an absence", async () => {
    requests = stubApi({
      containers: containerPage([
        containerRow(
          { name: "calendar-tag" },
          {
            state: "cannotAdvise",
            updateType: "unknown",
            proposedImage: "acme/app:2026.08",
          },
        ),
      ]),
    });

    renderTable();

    const row = await rowFor("calendar-tag");
    expect(within(row).getByText("Cannot determine")).toBeInTheDocument();
    expect(within(row).getByText(/cannot judge this change/i)).toBeInTheDocument();
  });

  it("names the change when there is one", async () => {
    requests = stubApi({
      containers: containerPage([
        containerRow(
          { name: "web" },
          {
            state: "updateAvailable",
            updateType: "minor",
            proposedImage: "nginx:1.28.0",
          },
        ),
      ]),
    });

    renderTable();

    const row = await rowFor("web");
    expect(within(row).getByText("Update available")).toBeInTheDocument();
    expect(within(row).getByText("nginx:1.28.0")).toBeInTheDocument();
    expect(within(row).getByText(/minor change/i)).toBeInTheDocument();
  });

  it("never carries the verdict in colour alone", async () => {
    // Every state renders a WORD. The tone is a second signal, never the only
    // one, for anyone who cannot distinguish them.
    requests = stubApi({
      containers: containerPage([
        containerRow({ name: "a" }, { state: "unhealthy" }),
        containerRow({ name: "b", id: "b1", shortId: "b1" }, { state: "approvalRequired" }),
        containerRow({ name: "c", id: "c1", shortId: "c1" }, { state: "paused" }),
      ]),
    });

    renderTable();

    expect(within(await rowFor("a")).getByText("Unhealthy")).toBeInTheDocument();
    expect(within(await rowFor("b")).getByText("Approval required")).toBeInTheDocument();
    expect(within(await rowFor("c")).getByText("Automation paused")).toBeInTheDocument();
  });
});

// ------------------------------------------------------------ the count --

describe("the page summary", () => {
  it("says how many rows need somebody", async () => {
    requests = stubApi({
      containers: containerPage([
        containerRow({ name: "a" }, { state: "unhealthy" }),
        containerRow({ name: "b", id: "b1", shortId: "b1" }, { state: "upToDate" }),
        containerRow({ name: "c", id: "c1", shortId: "c1" }, { state: "approvalRequired" }),
      ]),
    });

    renderTable();

    expect(await screen.findByText(/2 of 3 containers on this page need attention/i))
      .toBeInTheDocument();
  });

  it("stays quiet when nothing does", async () => {
    requests = stubApi({
      containers: containerPage([containerRow({ name: "a" }, { state: "upToDate" })]),
    });

    renderTable();
    await rowFor("a");

    // Scoped to the live region: the column heading is "HarborMaster" and is
    // present on every page, so it is not the claim under test.
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("does not count an evidence gap as a problem", async () => {
    // "Not checked" and "cannot advise" are gaps, not faults. Counting them
    // as work would make a fresh install look like an emergency.
    requests = stubApi({
      containers: containerPage([
        containerRow({ name: "a" }, { state: "notChecked" }),
        containerRow({ name: "b", id: "b1", shortId: "b1" }, { state: "cannotAdvise" }),
        containerRow({ name: "c", id: "c1", shortId: "c1" }, { state: "notTracked" }),
      ]),
    });

    renderTable();
    await rowFor("a");

    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });
});

// -------------------------------------------------------------- preserved --

describe("containers HarborMaster kept", () => {
  it("asks the server for workloads only, by default", async () => {
    requests = stubApi({ containers: containerPage() });

    renderTable();
    await waitFor(() => expect(lastContainerQuery(requests)).toBeTruthy());

    // Absent means excluded: the server's default is the narrow list, so the
    // browser does not have to ask for it.
    expect(lastContainerQuery(requests).get("includePreserved")).toBeNull();
  });

  it("offers them behind one checkbox", async () => {
    const user = userEvent.setup();
    requests = stubApi({ containers: containerPage() });

    renderTable();
    const toggle = await screen.findByRole("checkbox", {
      name: /show containers harbormaster kept/i,
    });
    await user.click(toggle);

    await waitFor(() =>
      expect(lastContainerQuery(requests).get("includePreserved")).toBe("true"),
    );
  });

  it("explains why a preserved container exists rather than listing it as a workload", async () => {
    requests = stubApi({
      containers: containerPage([
        containerRow(
          { name: "web.hm-old-exec_00112233445566778899" },
          { state: "preserved", preserved: "original", preservedFor: "web" },
        ),
      ]),
    });

    renderTable();

    const row = await rowFor("web.hm-old-exec_00112233445566778899");
    expect(within(row).getByText("Kept by HarborMaster")).toBeInTheDocument();
    // Attributed to the workload it belongs to, so an operator can tell what
    // it is evidence OF.
    expect(within(row).getByText(/previous version, kept/i)).toBeInTheDocument();
    expect(within(row).getByText("web", { selector: "span.font-medium" }))
      .toBeInTheDocument();
    // And no update is proposed for something that is not a workload.
    expect(within(row).getByText(/not applicable/i)).toBeInTheDocument();
  });

  it("says when a name only LOOKS like one of HarborMaster's", async () => {
    // A container an operator named this way themselves. Saying HarborMaster
    // parked it would be a false claim about the operator's own host.
    requests = stubApi({
      containers: containerPage([
        containerRow(
          { name: "api.hm-old-exec_ffffffffffffffffffff" },
          { state: "preserved", preserved: "suspected" },
        ),
      ]),
    });

    renderTable();

    const row = await rowFor("api.hm-old-exec_ffffffffffffffffffff");
    expect(within(row).getByText(/looks like one of ours/i)).toBeInTheDocument();
  });
});

// --------------------------------------------------------------- linkage --

describe("the row's links", () => {
  it("points an open finding at the page that explains it", async () => {
    requests = stubApi({
      containers: containerPage([
        containerRow(
          { name: "web", id: "abc123" },
          { state: "upToDate", openViolations: 2, highestSeverity: "critical", openDrift: 1 },
        ),
      ]),
    });

    renderTable();

    const row = await rowFor("web");
    expect(within(row).getByRole("link", { name: /2 policy findings/i })).toHaveAttribute(
      "href",
      "/policy/container/abc123",
    );
    expect(within(row).getByRole("link", { name: /1 drifted setting/i })).toHaveAttribute(
      "href",
      "/drift/container/abc123",
    );
  });

  it("gives the container link a real touch target", async () => {
    // Measured in a real browser, pinned here as the class that produces it:
    // the previous bare inline link was a 19px line box, well under the 24px
    // minimum and the smallest control on the most used page.
    requests = stubApi({ containers: containerPage([containerRow({ name: "web" })]) });

    renderTable();

    const link = await screen.findByRole("link", { name: "web" });
    expect(link.className).toMatch(/min-h-11/);
  });
});
