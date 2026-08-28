import { expect, it } from "vitest";

import { formatImageReference } from "./presentation";

/**
 * Compact image references, with nothing lost.
 *
 * # The defect this exists to prevent coming back
 *
 * A container HarborMaster has successfully updated runs an immutable digest,
 * so its image reads as a repository plus sixty-four hex characters. In the
 * containers table that was one row of noise per correctly-managed container --
 * the wrong ones to bury.
 *
 * # The rule
 *
 * Only the digest is ever shortened, because a digest is a content address and
 * its prefix identifies it. A TAG is a name: `nginx:1.27.1` is already the
 * shortest true form of itself, and abbreviating a version is how somebody
 * applies the wrong one.
 *
 * Nothing here is the only copy of anything. `full` is returned alongside
 * `display`, and `abbreviated` tells the caller whether it needs to offer it.
 */

const SHA = "sha256:2b8d1a4f3c9e7b6a5d4c3b2a1908f7e6d5c4b3a29180f7e6d5c4b3a2918f7e6d5";

it("abbreviates a digest to a recognisable prefix", () => {
  const { display, full, abbreviated } = formatImageReference({
    raw: `docker.io/library/alpine@${SHA}`,
    repository: "docker.io/library/alpine",
    digest: SHA,
  });

  expect(display).toBe("docker.io/library/alpine@sha256:2b8d1a4f3c9e…");
  expect(abbreviated).toBe(true);
  // The complete value comes back untouched, for the title and the detail page.
  expect(full).toBe(`docker.io/library/alpine@${SHA}`);
});

it("keeps the repository readable", () => {
  const { display } = formatImageReference({
    raw: `ghcr.io/home-assistant/home-assistant@${SHA}`,
    repository: "ghcr.io/home-assistant/home-assistant",
    digest: SHA,
  });
  expect(display).toContain("ghcr.io/home-assistant/home-assistant");
});

it("leaves an ordinary tagged image exactly as it is", () => {
  const tagged = { raw: "nginx:1.27.1", repository: "nginx", tag: "1.27.1" };
  const { display, abbreviated } = formatImageReference(tagged);

  expect(display).toBe("nginx:1.27.1");
  expect(abbreviated).toBe(false);
  // Specifically: no ellipsis anywhere near a version number.
  expect(display).not.toContain("…");
});

it("keeps both the tag asked for and the digest resolved", () => {
  // A reference can carry both. The tag is what was requested and the digest is
  // what it resolved to; dropping either changes what the row says.
  const { display } = formatImageReference({
    raw: `nginx:1.27@${SHA}`,
    repository: "nginx",
    tag: "1.27",
    digest: SHA,
  });
  expect(display).toBe("nginx:1.27@sha256:2b8d1a4f3c9e…");
});

it("leaves a digest short enough to read alone", () => {
  // Abbreviating something that already fits adds an ellipsis and removes
  // information.
  const short = { raw: "local/app@sha256:abc123", repository: "local/app", digest: "sha256:abc123" };
  expect(formatImageReference(short)).toMatchObject({
    display: "local/app@sha256:abc123",
    abbreviated: false,
  });
});

it("renders a reference it cannot parse rather than blanking the cell", () => {
  const odd = { raw: "some::weird@thing", digest: "not-a-digest" };
  expect(formatImageReference(odd)).toMatchObject({
    display: "some::weird@thing",
    abbreviated: false,
  });
});

it("falls back to splitting the raw reference when the server sent no repository", () => {
  const { display, abbreviated } = formatImageReference({
    raw: `myregistry.lan/app@${SHA}`,
    digest: SHA,
  });
  expect(display).toBe("myregistry.lan/app@sha256:2b8d1a4f3c9e…");
  expect(abbreviated).toBe(true);
});

it("shows the no-value dash rather than an empty cell", () => {
  expect(formatImageReference({ raw: "" }).display).toBe("—");
});
