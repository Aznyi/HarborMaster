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

/**
 * How much of a digest is enough to recognise it.
 *
 * Twelve hex characters is what `docker images` shows and what an operator
 * pastes when they mean "that one". Long enough to identify a digest among the
 * handful on one host; short enough that a table row stays a row.
 */
const DIGEST_PREFIX = 12;

/** The parts of an image reference a list needs, as the server already sends them. */
export interface ImageReferenceParts {
  raw: string;
  repository?: string;
  tag?: string;
  digest?: string;
}

/**
 * An image reference, compact enough to scan.
 *
 * # The problem this solves
 *
 * A container HarborMaster has updated runs an immutable digest, so its image
 * reads as `docker.io/library/alpine@sha256:` plus sixty-four hex characters.
 * In a table that is one row of noise per successfully-managed container --
 * exactly backwards, since those are the ones working correctly.
 *
 * # What it does NOT do
 *
 * It never shortens a tag. `nginx:1.27.1` is already the shortest true form of
 * itself, and abbreviating a version is how somebody applies the wrong one.
 * Only the digest -- the part that is a content address rather than a name --
 * is abbreviated, and only ever with an ellipsis so it can never be mistaken
 * for a complete value.
 *
 * The full reference is returned alongside, and every caller is expected to
 * carry it: on a `title`, in a detail field, or both. Nothing here is the only
 * copy of anything.
 */
export function formatImageReference(image: ImageReferenceParts): {
  /** The compact form, for the cell. */
  display: string;
  /** The complete reference, exactly as the server sent it. */
  full: string;
  /** True when `display` is an abbreviation of `full`. */
  abbreviated: boolean;
} {
  const full = image.raw ?? "";
  const digest = image.digest ?? "";

  // A digest short enough to read whole is left whole: abbreviating something
  // that already fits adds an ellipsis and removes information.
  const algorithmAndHex = /^([A-Za-z0-9_+.-]+):([A-Fa-f0-9]+)$/.exec(digest);
  const algorithm = algorithmAndHex?.[1] ?? "";
  const hex = algorithmAndHex?.[2] ?? "";
  if (hex.length <= DIGEST_PREFIX) {
    return { display: full || NO_VALUE, full: full || NO_VALUE, abbreviated: false };
  }

  const short = `${algorithm}:${hex.slice(0, DIGEST_PREFIX)}…`;

  // Prefer the repository the server parsed. Falling back to splitting `raw`
  // keeps a malformed row readable rather than blank.
  const repository = image.repository || full.split("@")[0] || "";
  if (!repository) {
    return { display: full || NO_VALUE, full: full || NO_VALUE, abbreviated: false };
  }

  // A reference can carry a tag AND a digest. Both are kept: the tag is what
  // was asked for and the digest is what was resolved, and dropping either
  // changes what the row says.
  const tag = image.tag ? `:${image.tag}` : "";
  return { display: `${repository}${tag}@${short}`, full, abbreviated: true };
}
