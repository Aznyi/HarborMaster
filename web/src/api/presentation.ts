/**
 * Presentation helpers shared across the consolidated workspaces.
 *
 * # Why this module exists
 *
 * By the end of Phase 5 four files carried a private timestamp formatter with
 * the same body -- `formatMoment` in Automation and three copies of
 * `formatTimestamp` in Dashboard, Container detail and Events. They did not
 * disagree, which is exactly why the duplication survived five phases: nothing
 * ever broke to point at it.
 *
 * One definition removes the chance that a future change to one of them makes
 * the same instant read differently on two pages.
 *
 * # What this deliberately is not
 *
 * Not a date library, and not a relative-time formatter. HarborMaster's pages
 * report when something happened, and an operator correlating an update with a
 * log line needs the actual time rather than "3 minutes ago".
 */

/** The em dash pages already use for "no value". */
export const NO_VALUE = "—";

/**
 * An absolute local timestamp, or NO_VALUE.
 *
 * Absent and unparseable both render as NO_VALUE: a page must not print
 * "Invalid Date" at somebody, and a missing timestamp is not an error.
 */
export function formatMoment(value: string | undefined | null): string {
  if (!value) return NO_VALUE;
  const parsed = new Date(value);
  if (Number.isNaN(parsed.getTime())) return NO_VALUE;
  return parsed.toLocaleString();
}

/**
 * The same instant, or nothing at all.
 *
 * For layouts whose field component omits a row when it has no value. Absent
 * returns `undefined` so the row disappears; present-but-unparseable returns
 * NO_VALUE, because a field that exists and cannot be read is not the same
 * thing as a field that was never sent.
 */
export function formatMomentOrNothing(
  value: string | undefined | null,
): string | undefined {
  if (!value) return undefined;
  return formatMoment(value);
}
