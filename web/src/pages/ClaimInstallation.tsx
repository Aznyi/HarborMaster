import { useState } from "react";
import type { FormEvent } from "react";

import { useSession } from "../hooks/useSession";
import { AuthLayout, Field, FormError } from "./SignIn";

/**
 * The bootstrap page: creating the first administrator.
 *
 * # Why a token is required
 *
 * Without one, claiming a brand-new installation is a race won by whoever
 * reaches the port first, which on an exposed port is not the operator. The
 * token moves the requirement from "be first" to "can read the server's log",
 * which is the same bar the rest of the deployment already assumes.
 *
 * It is printed to the server's output at startup and re-minted on every
 * restart, so an operator who lost it restarts HarborMaster.
 *
 * # This page stops existing once it succeeds
 *
 * The endpoint behind it answers 404 on a claimed installation, and the shell
 * never routes here again. There is no second bootstrap.
 */
export function ClaimInstallation() {
  const session = useSession();
  const [token, setToken] = useState("");
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");
  const [mismatch, setMismatch] = useState(false);

  async function onSubmit(event: FormEvent) {
    event.preventDefault();

    // Checked here as well as on the server: the server never sees the
    // confirmation field, so this is the only place the two can be compared.
    if (password !== confirm) {
      setMismatch(true);
      return;
    }
    setMismatch(false);

    try {
      await session.claim({ token: token.trim(), username, password });
    } catch {
      setPassword("");
      setConfirm("");
    }
  }

  return (
    <AuthLayout
      title="Claim this installation"
      subtitle="HarborMaster has no administrator yet. Create one using the bootstrap token printed in the server log."
    >
      <form onSubmit={onSubmit} className="flex flex-col gap-4">
        <Field
          id="token"
          label="Bootstrap token"
          value={token}
          onChange={setToken}
          autoFocus
          hint="Printed once at startup. Restart HarborMaster to issue a new one."
        />
        <Field
          id="username"
          label="Administrator username"
          value={username}
          onChange={setUsername}
          autoComplete="username"
          hint="Lowercase letters, digits, dot, dash, or underscore."
        />
        <Field
          id="password"
          label="Password"
          type="password"
          value={password}
          onChange={setPassword}
          autoComplete="new-password"
          hint="At least 12 characters. A passphrase is easier to remember and harder to guess."
        />
        <Field
          id="confirm"
          label="Repeat password"
          type="password"
          value={confirm}
          onChange={setConfirm}
          autoComplete="new-password"
        />

        {mismatch ? <FormError message="The two passwords do not match." /> : null}
        {session.error ? <FormError message={session.error} /> : null}

        <button
          type="submit"
          disabled={
            session.busy || token === "" || username === "" || password === ""
          }
          className="rounded-lg bg-accent px-4 py-2 text-sm font-semibold text-white disabled:opacity-50"
        >
          {session.busy ? "Creating…" : "Create administrator"}
        </button>

        <p className="text-xs text-content-muted">
          Lost the token? Restart HarborMaster to print a new one, or run{" "}
          <code className="rounded bg-surface-sunken px-1">
            harbormaster admin bootstrap
          </code>{" "}
          on the host.
        </p>
      </form>
    </AuthLayout>
  );
}
