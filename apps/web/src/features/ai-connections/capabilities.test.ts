import { describe, it, expect } from "vitest";

import { capabilityLabel, KNOWN_CAPABILITIES } from "./capabilities";

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
