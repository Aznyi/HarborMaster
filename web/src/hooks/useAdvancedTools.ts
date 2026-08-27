import { useSyncExternalStore } from "react";

/**
 * Whether the sidebar lists HarborMaster's specialised tools.
 *
 * # Why this is a browser preference and not a server setting
 *
 * It changes nothing about what an account may DO. Every specialised page is
 * still routable by URL and still guarded by the same permission; hiding a link
 * has never been the access control here, and the comment on `NavItem` says so.
 * So there is nothing to persist server-side, and persisting it there would
 * mean a migration and an API for a checkbox.
 *
 * # Why localStorage, given the repository avoids it elsewhere
 *
 * `api/client.ts` deliberately keeps the CSRF token in a module variable
 * because storage "is exactly where a successful XSS looks first". That reasoning
 * is about CREDENTIALS. This value is a boolean about menu density: an attacker
 * who could read or write it has gained the ability to show somebody their own
 * navigation. It is the one thing storage is appropriate for.
 *
 * Every access is wrapped, because a private window, cleared site data, or a
 * browser configured to refuse storage must degrade to the default rather than
 * throw on render.
 */
const STORAGE_KEY = "harbormaster.advancedTools";

/** Subscribers, so every mounted component agrees without a provider. */
const listeners = new Set<() => void>();

function read(): boolean {
  try {
    return window.localStorage.getItem(STORAGE_KEY) === "true";
  } catch {
    // Storage refused. The default -- a simple sidebar -- is the safe answer.
    return false;
  }
}

/**
 * The snapshot React compares between renders.
 *
 * Cached because `useSyncExternalStore` requires a stable value: returning a
 * freshly-read boolean is fine (booleans compare by value), but reading
 * localStorage on every render of every subscriber is not, and this is called
 * on each one.
 */
let snapshot = read();

function emit(): void {
  for (const listener of listeners) listener();
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);

  // Another tab changing the preference should not leave this one disagreeing.
  const onStorage = (event: StorageEvent) => {
    if (event.key !== null && event.key !== STORAGE_KEY) return;
    snapshot = read();
    emit();
  };
  window.addEventListener("storage", onStorage);

  return () => {
    listeners.delete(listener);
    window.removeEventListener("storage", onStorage);
  };
}

function getSnapshot(): boolean {
  return snapshot;
}

/**
 * Server rendering has no browser storage, and guessing would mean the markup
 * disagreeing with the first client render.
 */
function getServerSnapshot(): boolean {
  return false;
}

/** Sets the preference and tells every subscriber. */
export function setAdvancedTools(enabled: boolean): void {
  snapshot = enabled;
  try {
    window.localStorage.setItem(STORAGE_KEY, String(enabled));
  } catch {
    // The preference still applies to this page; it just will not survive a
    // reload. Failing the click would be worse than forgetting the choice.
  }
  emit();
}

/** Reads the preference, re-rendering when it changes anywhere. */
export function useAdvancedTools(): boolean {
  return useSyncExternalStore(subscribe, getSnapshot, getServerSnapshot);
}

/** Test seam: forget the cached snapshot so a case can start from storage. */
export function resetAdvancedToolsForTest(): void {
  snapshot = read();
  emit();
}
