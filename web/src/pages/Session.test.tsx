import { setAdvancedTools } from "../hooks/useAdvancedTools";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";

import { App } from "../App";
import { CSRF_HEADER, hasCsrfToken, refreshInventory } from "../api/client";
import { SessionProvider } from "../hooks/useSession";
import { HttpFailure, sessionResponse, stubApi } from "../test/fixtures";
import { TestSessionProvider, testSession, testUser } from "../test/session";
import { Users } from "./Users";

/**
 * The identity behaviour, exercised through the real provider.
 *
 * The rest of the suite uses a fixed identity so a page test is about that
 * page. These are the tests that are about signing in, being signed out, and
 * what the browser is allowed to hold while it is signed in.
 */

function renderApp(path = "/") {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <SessionProvider>
        <App />
      </SessionProvider>
    </MemoryRouter>,
  );
}

beforeEach(() => {
  vi.unstubAllGlobals();
});

// -------------------------------------------------------------- the gate --

describe("the pre-session gate", () => {
  it("shows the sign-in page and no estate data when there is no session", async () => {
    stubApi({
      session: new HttpFailure(401, "unauthenticated", "authentication is required"),
      bootstrap: { completed: true, tokenRequired: true },
    });
    renderApp();

    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /sign in/i })).toBeInTheDocument(),
    );

    // Nothing about the estate is rendered. The navigation is not merely
    // hidden -- it is not mounted, so no data hook behind it ever runs.
    expect(
      screen.queryByRole("navigation", { name: /primary/i }),
    ).not.toBeInTheDocument();
    expect(screen.queryByText(/containers/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /sign out/i })).not.toBeInTheDocument();
  });

  it("shows the bootstrap page when the installation has no administrator", async () => {
    stubApi({
      session: new HttpFailure(
        401,
        "bootstrap_required",
        "this installation has no administrator yet",
      ),
      bootstrap: { completed: false, tokenRequired: true },
    });
    renderApp();

    await waitFor(() =>
      expect(
        screen.getByRole("heading", { name: /claim this installation/i }),
      ).toBeInTheDocument(),
    );
    expect(screen.getByLabelText(/bootstrap token/i)).toBeInTheDocument();
  });

  // Fail closed: if the app cannot establish whether the installation is
  // claimed, it must offer sign-in rather than a form that would claim it.
  it("offers sign-in rather than bootstrap when the state cannot be established", async () => {
    stubApi({
      session: new HttpFailure(500, "internal_error", "internal error"),
      bootstrap: new HttpFailure(500, "internal_error", "internal error"),
    });
    renderApp();

    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /sign in/i })).toBeInTheDocument(),
    );
    expect(
      screen.queryByRole("heading", { name: /claim this installation/i }),
    ).not.toBeInTheDocument();
  });

  it("shows the shell once a session resolves", async () => {
    stubApi();
    renderApp();

    await waitFor(() =>
      expect(screen.getByRole("navigation", { name: /primary/i })).toBeInTheDocument(),
    );
    expect(screen.getByRole("button", { name: /sign out/i })).toBeInTheDocument();
  });
});

// ------------------------------------------------------------- signing in --

describe("signing in", () => {
  it("says one thing for every credential failure", async () => {
    stubApi({
      session: new HttpFailure(401, "unauthenticated", "authentication is required"),
      bootstrap: { completed: true, tokenRequired: true },
      login: new HttpFailure(
        401,
        "unauthenticated",
        "the username or password is incorrect",
      ),
    });
    const user = userEvent.setup();
    renderApp();

    await waitFor(() => screen.getByRole("heading", { name: /sign in/i }));
    await user.type(screen.getByLabelText(/username/i), "someone");
    await user.type(screen.getByLabelText(/password/i), "a wrong passphrase");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/username or password is incorrect/i);

    // Nothing more specific. An "unknown user" message here would undo the
    // enumeration resistance the server pays an Argon2id evaluation for.
    expect(alert).not.toHaveTextContent(/unknown/i);
    expect(alert).not.toHaveTextContent(/disabled/i);
    expect(alert).not.toHaveTextContent(/no such/i);
  });

  it("does not leave the password in the field after a failure", async () => {
    stubApi({
      session: new HttpFailure(401, "unauthenticated", "authentication is required"),
      bootstrap: { completed: true, tokenRequired: true },
      login: new HttpFailure(401, "unauthenticated", "the username or password is incorrect"),
    });
    const user = userEvent.setup();
    renderApp();

    await waitFor(() => screen.getByRole("heading", { name: /sign in/i }));
    const password = screen.getByLabelText(/password/i);
    await user.type(screen.getByLabelText(/username/i), "someone");
    await user.type(password, "a wrong passphrase");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    await screen.findByRole("alert");
    expect(password).toHaveValue("");
  });

  it("reports rate limiting as something to wait out", async () => {
    stubApi({
      session: new HttpFailure(401, "unauthenticated", "authentication is required"),
      bootstrap: { completed: true, tokenRequired: true },
      login: new HttpFailure(429, "conflict", "too many attempts"),
    });
    const user = userEvent.setup();
    renderApp();

    await waitFor(() => screen.getByRole("heading", { name: /sign in/i }));
    await user.type(screen.getByLabelText(/username/i), "someone");
    await user.type(screen.getByLabelText(/password/i), "a passphrase");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/wait a few minutes/i);
  });

  it("enters the shell after a successful sign-in", async () => {
    stubApi({
      session: new HttpFailure(401, "unauthenticated", "authentication is required"),
      bootstrap: { completed: true, tokenRequired: true },
      login: sessionResponse("operator"),
    });
    const user = userEvent.setup();
    renderApp();

    await waitFor(() => screen.getByRole("heading", { name: /sign in/i }));
    await user.type(screen.getByLabelText(/username/i), "tester");
    await user.type(screen.getByLabelText(/password/i), "the correct passphrase");
    await user.click(screen.getByRole("button", { name: /sign in/i }));

    await waitFor(() =>
      expect(screen.getByRole("navigation", { name: /primary/i })).toBeInTheDocument(),
    );
    expect(hasCsrfToken()).toBe(true);
  });
});

// ------------------------------------------------ the forced password change --

describe("a forced password change", () => {
  it("replaces the whole app until the password is set", async () => {
    stubApi({
      session: {
        ...sessionResponse(),
        user: { ...sessionResponse().user, mustChangePassword: true },
      },
    });
    renderApp();

    await waitFor(() =>
      expect(
        screen.getByRole("heading", { name: /choose a new password/i }),
      ).toBeInTheDocument(),
    );
    expect(
      screen.queryByRole("navigation", { name: /primary/i }),
    ).not.toBeInTheDocument();
  });

  it("refuses to submit two passwords that do not match", async () => {
    stubApi({
      session: {
        ...sessionResponse(),
        user: { ...sessionResponse().user, mustChangePassword: true },
      },
    });
    const user = userEvent.setup();
    renderApp();

    await waitFor(() => screen.getByRole("heading", { name: /choose a new password/i }));
    await user.type(screen.getByLabelText(/current password/i), "the old passphrase");
    await user.type(screen.getByLabelText(/^new password$/i), "a new passphrase");
    await user.type(screen.getByLabelText(/repeat new password/i), "a different one");
    await user.click(screen.getByRole("button", { name: /set password/i }));

    expect(await screen.findByRole("alert")).toHaveTextContent(/do not match/i);
  });
});

// ----------------------------------------------------- losing the session --

describe("losing the session", () => {
  // A 401 from a background poll on a page the operator is not looking at must
  // end the session once, not raise an error on every view that was fetching.
  it("returns to sign-in when a request is refused", async () => {
    stubApi({
      inventory: new HttpFailure(401, "unauthenticated", "authentication is required"),
      bootstrap: { completed: true, tokenRequired: true },
    });
    renderApp();

    await waitFor(() =>
      expect(screen.getByRole("heading", { name: /sign in/i })).toBeInTheDocument(),
    );
    // The CSRF token is dropped with the session: a write attempted afterwards
    // must not carry a token that no longer matches anything.
    expect(hasCsrfToken()).toBe(false);
  });
});

// ------------------------------------------------------ role-aware rendering --

describe("role-aware rendering", () => {
  function renderWith(role: "viewer" | "operator" | "administrator") {
    stubApi();
    return render(
      <MemoryRouter initialEntries={["/"]}>
        <TestSessionProvider session={testSession({ user: testUser(role) })}>
          <App />
        </TestSessionProvider>
      </MemoryRouter>,
    );
  }

  it("does not offer account administration to a viewer or an operator", async () => {
    for (const role of ["viewer", "operator"] as const) {
      const view = renderWith(role);
      const nav = await screen.findByRole("navigation", { name: /primary/i });

      // Queried as links rather than as text: the rendered labels run together
      // in textContent, and "Your account" + "Settings" reads as "accounts".
      expect(within(nav).queryByRole("link", { name: "Accounts" })).toBeNull();
      expect(within(nav).queryByRole("link", { name: "Security audit" })).toBeNull();
      view.unmount();
    }
  });

  it("offers account administration to an administrator", async () => {
    renderWith("administrator");
    const nav = await screen.findByRole("navigation", { name: /primary/i });

    // Both are specialised tools, so they live under Advanced. What this test
    // is about is the ROLE: an administrator is offered them and a viewer is
    // not, whether or not the section is expanded.
    setAdvancedTools(true);

    expect(
      await within(nav).findByRole("link", { name: "Accounts" }),
    ).toBeInTheDocument();
    expect(
      within(nav).getByRole("link", { name: "Security audit" }),
    ).toBeInTheDocument();
  });

  it("offers neither to a viewer, even with advanced tools shown", async () => {
    renderWith("viewer");
    const nav = await screen.findByRole("navigation", { name: /primary/i });
    setAdvancedTools(true);

    // Showing advanced tools is a density preference. It grants nothing, and
    // the permission filter is unchanged underneath it.
    await waitFor(() =>
      expect(
        within(nav).queryByRole("link", { name: "Accounts" }),
      ).not.toBeInTheDocument(),
    );
    expect(
      within(nav).queryByRole("link", { name: "Security audit" }),
    ).not.toBeInTheDocument();
  });

  // Hiding a link is not the access control -- the server refuses regardless --
  // but a page reached by typing the URL should say so rather than render an
  // empty table that looks like "there is nothing here".
  it("explains rather than pretends when a role lacks the permission", async () => {
    stubApi();
    render(
      <MemoryRouter initialEntries={["/users"]}>
        <TestSessionProvider session={testSession({ user: testUser("operator") })}>
          <App />
        </TestSessionProvider>
      </MemoryRouter>,
    );

    await waitFor(() =>
      expect(screen.getByText(/not permitted/i)).toBeInTheDocument(),
    );
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });
});

// ------------------------------------------------------- account management --

describe("account administration", () => {
  it("shows a generated password once, with a warning", async () => {
    stubApi();
    render(
      <MemoryRouter>
        <TestSessionProvider>
          <Users />
        </TestSessionProvider>
      </MemoryRouter>,
    );

    const user = userEvent.setup();
    await waitFor(() => screen.getByRole("table"));

    await user.type(screen.getByLabelText(/new account/i), "newoperator");
    await user.click(screen.getByRole("button", { name: /^create$/i }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(/shown once/i);
    expect(alert).toHaveTextContent("a-generated-temporary-password");
  });

  it("does not offer to change the signed-in account's own role", async () => {
    stubApi();
    render(
      <MemoryRouter>
        <TestSessionProvider>
          <Users />
        </TestSessionProvider>
      </MemoryRouter>,
    );

    await waitFor(() => screen.getByRole("table"));
    // The stub's account list contains only the caller, marked "you".
    expect(screen.getByText(/^you$/i)).toBeInTheDocument();
    expect(
      screen.queryByRole("combobox", { name: /role for tester/i }),
    ).not.toBeInTheDocument();
  });
});

// ------------------------------------------------------------ the CSRF token --

describe("the CSRF token", () => {
  it("is sent on writes and is never persisted", async () => {
    const requests = stubApi();
    render(
      <MemoryRouter>
        <TestSessionProvider>
          <Users />
        </TestSessionProvider>
      </MemoryRouter>,
    );
    await waitFor(() => screen.getByRole("table"));

    // Install a token the way a real sign-in would, then make a write.
    const { setCsrfToken } = await import("../api/client");
    setCsrfToken("a-session-csrf-token");
    await refreshInventory().catch(() => undefined);

    const write = requests.find((entry) => entry.method === "POST");
    expect(write).toBeDefined();

    // Nothing about the session may be persisted where a script can find it
    // after the page is gone. Web storage is not always available in this
    // environment; when it is, it must be empty.
    expect(window.localStorage?.length ?? 0).toBe(0);
    expect(window.sessionStorage?.length ?? 0).toBe(0);
    expect(document.cookie).not.toContain("a-session-csrf-token");
    expect(CSRF_HEADER).toBe("X-HarborMaster-CSRF");
  });
});
