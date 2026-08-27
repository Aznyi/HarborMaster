import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach, vi } from "vitest";

import { resetAdvancedToolsForTest } from "../hooks/useAdvancedTools";

afterEach(() => {
  cleanup();
  vi.restoreAllMocks();

  // The advanced-tools preference is module state backed by localStorage, so a
  // case that enables it would otherwise decide what the next case sees. Reset
  // here rather than in each file: the failure it prevents is order-dependent,
  // which is the kind nobody debugs twice.
  try {
    window.localStorage.clear();
  } catch {
    // A jsdom without storage is fine; the reset below still clears the cache.
  }
  resetAdvancedToolsForTest();
});
