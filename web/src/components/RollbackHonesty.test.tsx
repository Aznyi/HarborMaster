import { render, screen } from "@testing-library/react";
import fs from "node:fs";
import path from "node:path";
import { describe, expect, it } from "vitest";

import { RecreationWarningNotice } from "./ExecutionBadges";
import { RollbackWarningNotice } from "./RollbackBadges";

/**
 * Rollback honesty.
 *
 * # The defect this exists to prevent coming back
 *
 * The UI used to say, in as many words, "There is no automatic rollback" and
 * "Rollback is never automatic". Both were true when written and became false
 * when update policies gained `autoRollback`: a failed unattended update stops
 * the replacement, starts the original, and pauses the container — without
 * anybody asking.
 *
 * That is a safety-relevant untruth rather than a cosmetic one. An operator
 * reading "nothing will be undone automatically" during an incident concludes
 * the host is where the failed update left it, when HarborMaster may already
 * have moved it twice.
 *
 * These tests do two things: pin the corrected wording on the two notices, and
 * sweep the whole frontend source for any statement that CATEGORICALLY denies
 * automatic rollback. The sweep is the part that survives refactoring, because
 * the sentence can come back anywhere.
 */

// ------------------------------------------------------------ the notices --

describe("the recreation notice", () => {
  it("scopes the no-rollback promise to operator-requested recreations", () => {
    render(<RecreationWarningNotice />);
    const note = screen.getByRole("note");

    // Accurate: THIS recreation is not undone for you.
    expect(note).toHaveTextContent(/not rolled back automatically/i);
    // And the exception is named rather than omitted.
    expect(note).toHaveTextContent(/update policy/i);
  });
});

describe("the rollback notice", () => {
  it("names both things that can start a rollback", () => {
    render(<RollbackWarningNotice />);
    const note = screen.getByRole("note");

    expect(note).toHaveTextContent(/started either by a person or by the update policy/i);
    // Not guaranteed, and not silent: a policy rollback pauses afterwards.
    expect(note).toHaveTextContent(/pauses the container/i);
    expect(note).toHaveTextContent(/nothing is removed/i);
  });

  it("does not promise a rollback will succeed", () => {
    render(<RollbackWarningNotice />);
    const text = screen.getByRole("note").textContent ?? "";

    // "always works", "guaranteed", "restores service" would each be a promise
    // HarborMaster cannot keep: the original has to start and pass its checks.
    expect(text).not.toMatch(/always (works|succeeds)|guaranteed|will restore service/i);
  });
});

// -------------------------------------------------------------- the sweep --

/** Every frontend source file, excluding tests. */
function sourceFiles(dir: string, acc: string[] = []): string[] {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) sourceFiles(full, acc);
    else if (/\.(ts|tsx)$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name)) acc.push(full);
  }
  return acc;
}

describe("no user-facing text claims rollback is categorically never automatic", () => {
  it("sweeps the frontend source", () => {
    // Phrases that deny automatic rollback OUTRIGHT. Scoped statements are
    // fine and expected — "a recreation you start here is not rolled back
    // automatically" is true — so the patterns below match only the
    // unqualified forms that were actually wrong.
    const forbidden: { pattern: RegExp; why: string }[] = [
      { pattern: /there is no automatic rollback/i, why: "denies the policy-driven path outright" },
      { pattern: /rollback is never automatic/i, why: "denies the policy-driven path outright" },
      { pattern: /never rolls back automatically/i, why: "denies the policy-driven path outright" },
      { pattern: /rolls back only when a person asks/i, why: "excludes the policy-driven path" },
      { pattern: /it is not automatic anywhere else/i, why: "excludes the policy-driven path" },
    ];

    const offenders: string[] = [];
    for (const file of sourceFiles(path.join(__dirname, ".."))) {
      const text = fs.readFileSync(file, "utf8");
      for (const { pattern, why } of forbidden) {
        if (pattern.test(text)) {
          offenders.push(`${path.relative(path.join(__dirname, ".."), file)}: ${pattern} — ${why}`);
        }
      }
    }

    expect(
      offenders,
      "automatic rollback DOES happen when an update policy enables it and " +
        "verification fails; text denying that categorically is untrue and " +
        "misleads an operator during an incident",
    ).toEqual([]);
  });
});
