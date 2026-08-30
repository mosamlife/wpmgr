import { describe, it, expect } from "vitest";

import {
  ENFORCEMENT_ALL_MODE_SENTENCE,
  ENFORCEMENT_BANNED_WORDS,
  ENFORCEMENT_CHECK_SENTENCE,
  ENFORCEMENT_LIST_MODE_SENTENCE,
  ENFORCEMENT_TAG_DRIFT_SENTENCE,
  allEnforcementScreenStrings,
  bannedWordHits,
  describeRefusals,
} from "./site-enforcement";

// A guard nobody has seen fail is not known to guard anything. This file
// proves the banned-word guard both fires on a real violation and does not
// over-fire on the honest copy this screen actually ships.

describe("bannedWordHits — fires on the wireframe's own banned words", () => {
  it.each(ENFORCEMENT_BANNED_WORDS)("catches %s inside a sentence", (word) => {
    const poisoned = `This connection is completely ${word} from every other request.`;
    expect(bannedWordHits(poisoned)).toContain(word);
  });

  it("catches a multi-word phrase written with different surrounding case", () => {
    expect(bannedWordHits("Nothing outside the Walled Off area can be reached.")).toContain(
      "walled off",
    );
  });

  it("does not fire on a word that merely shares a prefix with a banned word", () => {
    // "safety" must not trip "safe": \bsafe\b requires a boundary right after
    // "safe", and "safety" has no boundary there.
    expect(bannedWordHits("Read our safety guidance before connecting an assistant.")).toEqual(
      [],
    );
  });
});

describe("bannedWordHits — does not over-fire on the shipped copy", () => {
  it("every string this screen renders is clean", () => {
    for (const text of allEnforcementScreenStrings()) {
      expect(bannedWordHits(text)).toEqual([]);
    }
  });

  it.each([
    ["the shared check sentence", ENFORCEMENT_CHECK_SENTENCE],
    ["the named-list sentence", ENFORCEMENT_LIST_MODE_SENTENCE],
    ["the tag-drift sentence", ENFORCEMENT_TAG_DRIFT_SENTENCE],
    ["the every-site sentence", ENFORCEMENT_ALL_MODE_SENTENCE],
  ] as const)("%s is clean", (_label, text) => {
    expect(bannedWordHits(text)).toEqual([]);
  });
});

describe("ENFORCEMENT_TAG_DRIFT_SENTENCE — the sharpest sentence on the screen", () => {
  it("states the drift consequence in the wireframe's own terms", () => {
    expect(ENFORCEMENT_TAG_DRIFT_SENTENCE).toMatch(/included automatically/i);
    expect(ENFORCEMENT_TAG_DRIFT_SENTENCE).toMatch(/without anyone approving it/i);
  });
});

describe("ENFORCEMENT_ALL_MODE_SENTENCE — every-site mode", () => {
  it("says there is no list, in the wireframe's own words", () => {
    expect(ENFORCEMENT_ALL_MODE_SENTENCE).toMatch(
      /Nothing is checked against a list because there is no list\./,
    );
  });
});

describe("describeRefusals — three states, never collapsed", () => {
  it("renders an explicit not-tracked sentence for 'unavailable', never a zero", () => {
    const text = describeRefusals({ kind: "unavailable" });
    expect(text).toMatch(/do not yet track/i);
    expect(text).not.toMatch(/^0/);
    expect(text).not.toBe("0");
  });

  it("renders a real zero as a stated fact, distinct from 'unavailable'", () => {
    expect(describeRefusals({ kind: "zero", windowDays: 7 })).toBe(
      "No requests have been refused.",
    );
  });

  it("renders a count with its window and correct pluralisation", () => {
    expect(describeRefusals({ kind: "count", count: 2, windowDays: 7 })).toBe(
      "Refused 2 times in the last 7 days.",
    );
    expect(describeRefusals({ kind: "count", count: 1, windowDays: 7 })).toBe(
      "Refused 1 time in the last 7 days.",
    );
  });

  it("unavailable and zero never render the same sentence", () => {
    expect(describeRefusals({ kind: "unavailable" })).not.toBe(
      describeRefusals({ kind: "zero", windowDays: 7 }),
    );
  });
});
