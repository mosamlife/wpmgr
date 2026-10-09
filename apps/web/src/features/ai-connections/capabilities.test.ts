import { describe, it, expect } from "vitest";

import {
  capabilityKind,
  capabilityLabel,
  CONFERRABLE_READS,
  KNOWN_CAPABILITIES,
} from "./capabilities";

// KNOWN_CAPABILITIES is DERIVED from CAPABILITY_LABELS's keys (see the file
// header), so there is no longer a second list a mutation could add a name to
// without also adding it here -- that failure mode is structurally impossible
// rather than merely tested for, and a test asserting the two agree would be
// tautological (they are the same object read two ways). What is left to test
// is the behaviour capabilityLabel actually promises.

describe("capabilityLabel", () => {
  it("labels every name in the known vocabulary", () => {
    // Not hardcoding the set: walks whatever KNOWN_CAPABILITIES actually is.
    for (const cap of KNOWN_CAPABILITIES) {
      const label = capabilityLabel(cap);
      expect(label.length).toBeGreaterThan(0);
      // A known capability gets an actual label, not its own wire string back
      // -- that fallback is reserved for names outside the vocabulary.
      expect(label).not.toBe(cap);
    }
  });

  it("falls back to the raw wire string for a capability this build does not know", () => {
    // The property #652 was filed over, preserved: an unrecognised name the
    // server actually stored still renders, as itself, rather than being
    // dropped or replaced with a generic "unknown" placeholder.
    expect(capabilityLabel("mcp.not.a.real.capability")).toBe("mcp.not.a.real.capability");
  });
});

describe("capabilityKind", () => {
  it("marks the one write capability as write, and every other known name as read", () => {
    // Walks the real vocabulary rather than hard-coding the name, so a tenth
    // capability added without an entry in CAPABILITY_KIND fails typecheck
    // (see the file header) before it could fail this test silently.
    for (const cap of KNOWN_CAPABILITIES) {
      const expected =
        cap === "mcp.cache.purge" || cap === "mcp.ability.request" ? "write" : "read";
      expect(capabilityKind(cap)).toBe(expected);
    }
  });

  it("falls back to read for a name outside the vocabulary", () => {
    // The conservative direction: an unrecognised name must not be able to
    // claim the visually distinct, warned, per-call-approval write group.
    expect(capabilityKind("mcp.not.a.real.capability")).toBe("read");
  });

  it("never lets the write capability into the reads-only conferrable set", () => {
    // This is the guard against ruling 33 regressing: CONFERRABLE_READS feeds
    // the "Read everything" preset, and a write capability in that preset
    // would let one click grant something that needs its own per-request
    // approval.
    expect(CONFERRABLE_READS).not.toContain("mcp.cache.purge");
    for (const cap of CONFERRABLE_READS) {
      expect(capabilityKind(cap)).toBe("read");
    }
  });
});
