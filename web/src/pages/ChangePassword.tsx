import { useState } from "react";
import type { FormEvent } from "react";

import { useSession } from "../hooks/useSession";
import { AuthLayout, Field, FormError } from "./SignIn";

/**
 * The forced password change.
 *
 * Shown when an account carries `mustChangePassword` -- a credential somebody
 * else chose, whether generated at account creation or set from the console.
 * Until it is replaced, the server refuses every route except reading the
 * session, changing the password, and signing out, so this page is not a
 * suggestion the operator can navigate away from.
 *
 * # The current password is required
 *
 * Even though the request is already authenticated. A session is possession; the
 * current password is knowledge. Requiring both stops a stolen session being
 * upgraded into permanent account control.
 *
 * # The session rotates
 *
 * Every session on the account ends, including this one, and a new cookie
 * replaces it. The provider installs the new CSRF token from the response; a
 * client that kept the old one would find its next write refused.
 */
export function ChangePassword() {
  const session = useSession();
  const [current, setCurrent] = useState("");
  const [next, setNext] = useState("");
  const [confirm, setConfirm] = useState("");
  const [mismatch, setMismatch] = useState(false);

  async function onSubmit(event: FormEvent) {
    event.preventDefault();

    if (next !== confirm) {
      setMismatch(true);
      return;
    }
    setMismatch(false);

    try {
      await session.changePassword({ currentPassword: current, newPassword: next });
    } catch {
      setCurrent("");
      setNext("");
      setConfirm("");
    }
  }

  return (
    <AuthLayout
      title="Choose a new password"
      subtitle={`Signed in as ${session.user?.username ?? "an operator"}. This password was set by somebody else and must be replaced before you can continue.`}
    >
      <form onSubmit={onSubmit} className="flex flex-col gap-4">
        <Field
          id="current"
          label="Current password"
          type="password"
          value={current}
          onChange={setCurrent}
          autoComplete="current-password"
          autoFocus
        />
        <Field
          id="next"
          label="New password"
          type="password"
          value={next}
          onChange={setNext}
          autoComplete="new-password"
          hint="At least 12 characters, and not your username."
        />
        <Field
          id="confirm"
          label="Repeat new password"
          type="password"
          value={confirm}
          onChange={setConfirm}
          autoComplete="new-password"
        />

        {mismatch ? <FormError message="The two passwords do not match." /> : null}
        {session.error ? <FormError message={session.error} /> : null}

        <button
          type="submit"
          disabled={session.busy || current === "" || next === ""}
          className="rounded-lg bg-accent px-4 py-2 text-sm font-semibold text-white disabled:opacity-50"
        >
          {session.busy ? "Saving…" : "Set password"}
        </button>

        <button
          type="button"
          onClick={() => void session.signOut()}
          className="text-xs text-content-muted underline"
        >
          Sign out instead
        </button>

        <p className="text-xs text-content-muted">
          Every other session on this account will end when the password changes.
        </p>
      </form>
    </AuthLayout>
  );
}
