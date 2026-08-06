import { useState } from "react";
import type { FormEvent } from "react";

import { useSession } from "../hooks/useSession";

/**
 * The sign-in page.
 *
 * # It says as little as the server does
 *
 * One message for every credential failure, because the server answers the same
 * way whether the username is unknown, the password is wrong, or the account is
 * disabled. A friendlier "no such user" here would undo the enumeration
 * resistance the backend pays an Argon2id evaluation to provide.
 *
 * # Nothing is remembered
 *
 * No "remember me", no stored username, no autofill hint beyond the browser's
 * own. The session lives in an HttpOnly cookie with an expiry the server sets;
 * there is nothing for this page to persist.
 */
export function SignIn() {
  const session = useSession();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");

  async function onSubmit(event: FormEvent) {
    event.preventDefault();
    try {
      await session.signIn({ username, password });
    } catch {
      // The provider holds the message; clear the password so a retry starts
      // from a blank field rather than a wrong value the operator may not
      // notice is still there.
      setPassword("");
    }
  }

  return (
    <AuthLayout
      title="Sign in"
      subtitle="HarborMaster fronts a privileged Docker socket. Every action is recorded against the account that performed it."
    >
      <form onSubmit={onSubmit} className="flex flex-col gap-4">
        <Field
          id="username"
          label="Username"
          value={username}
          onChange={setUsername}
          autoComplete="username"
          autoFocus
        />
        <Field
          id="password"
          label="Password"
          type="password"
          value={password}
          onChange={setPassword}
          autoComplete="current-password"
        />

        {session.error ? <FormError message={session.error} /> : null}

        <button
          type="submit"
          disabled={session.busy || username === "" || password === ""}
          className="rounded-lg bg-accent px-4 py-2 text-sm font-semibold text-white disabled:opacity-50"
        >
          {session.busy ? "Signing in…" : "Sign in"}
        </button>
      </form>
    </AuthLayout>
  );
}

/**
 * The shared frame for the pre-session pages.
 *
 * Deliberately outside AppShell: the navigation, the connectivity indicator,
 * and the estate summary are all things an unauthenticated visitor must not
 * see, and the surest way to not render them is to not mount them.
 */
export function AuthLayout({
  title,
  subtitle,
  children,
}: {
  title: string;
  subtitle?: string;
  children: React.ReactNode;
}) {
  return (
    <div className="grid min-h-screen place-items-center bg-surface px-4 py-10 text-content">
      <main className="w-full max-w-sm">
        <div className="mb-6 flex items-center gap-2">
          <span
            aria-hidden="true"
            className="grid size-9 place-items-center rounded-lg bg-accent-soft text-sm font-bold text-accent"
          >
            HM
          </span>
          <span className="text-base font-semibold tracking-tight">HarborMaster</span>
        </div>

        <h1 className="text-lg font-semibold tracking-tight">{title}</h1>
        {subtitle ? (
          <p className="mt-1 mb-6 text-sm text-content-muted">{subtitle}</p>
        ) : (
          <div className="mb-6" />
        )}

        <div className="rounded-xl border border-border-subtle bg-surface-raised p-5">
          {children}
        </div>
      </main>
    </div>
  );
}

/**
 * A labelled input.
 *
 * `value`/`onChange` only -- there is no `dangerouslySetInnerHTML` anywhere in
 * this app, and every operator-supplied string reaches the DOM as a text node.
 */
export function Field({
  id,
  label,
  value,
  onChange,
  type = "text",
  autoComplete,
  autoFocus,
  hint,
}: {
  id: string;
  label: string;
  value: string;
  onChange: (value: string) => void;
  type?: "text" | "password";
  autoComplete?: string;
  autoFocus?: boolean;
  hint?: string;
}) {
  return (
    <div className="flex flex-col gap-1">
      <label htmlFor={id} className="text-sm font-medium">
        {label}
      </label>
      <input
        id={id}
        name={id}
        type={type}
        value={value}
        autoComplete={autoComplete}
        autoFocus={autoFocus}
        spellCheck={false}
        onChange={(event) => onChange(event.target.value)}
        className="rounded-lg border border-border-subtle bg-surface px-3 py-2 text-sm"
      />
      {hint ? <p className="text-xs text-content-muted">{hint}</p> : null}
    </div>
  );
}

/** A form-level failure. `role="alert"` so a screen reader announces it. */
export function FormError({ message }: { message: string }) {
  return (
    <p
      role="alert"
      className="rounded-lg border border-border-subtle bg-danger-soft px-3 py-2 text-sm text-danger"
    >
      {message}
    </p>
  );
}
