import { describe, it, expect } from "vitest";

import {
  capabilityKind,
  capabilityLabel,
  CONFERRABLE_READS,
  KNOWN_CAPABILITIES,
  nextAbilityTicks,
  withoutOrphanedRequest,
  type AbilityTicks,
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

// "Ask for changes" (request) needs "see what the site can do" (read): the tool
// that carries a request is declared with the read capability, so a connection
// holding the request alone cannot call it. The rule is written out as a table,
// one row per start state and action, rather than derived from the function.
describe("nextAbilityTicks", () => {
  const both: AbilityTicks = { read: true, request: true };
  const readOnly: AbilityTicks = { read: true, request: false };
  const neither: AbilityTicks = { read: false, request: false };

  const table: readonly {
    readonly from: AbilityTicks;
    readonly row: "read" | "request";
    readonly ticked: boolean;
    readonly to: AbilityTicks;
  }[] = [
    // Ticking the request ticks the read, whatever the read was.
    { from: neither, row: "request", ticked: true, to: both },
    { from: readOnly, row: "request", ticked: true, to: both },
    { from: both, row: "request", ticked: true, to: both },
    // Clearing the read clears the request.
    { from: both, row: "read", ticked: false, to: neither },
    { from: readOnly, row: "read", ticked: false, to: neither },
    { from: neither, row: "read", ticked: false, to: neither },
    // Clearing the request leaves the read as it was.
    { from: both, row: "request", ticked: false, to: readOnly },
    { from: readOnly, row: "request", ticked: false, to: readOnly },
    { from: neither, row: "request", ticked: false, to: neither },
    // Ticking the read leaves the request as it was.
    { from: neither, row: "read", ticked: true, to: readOnly },
    { from: readOnly, row: "read", ticked: true, to: readOnly },
    { from: both, row: "read", ticked: true, to: both },
  ];

  it.each(table)("$row set to $ticked from read=$from.read request=$from.request", (c) => {
    expect(nextAbilityTicks(c.from, c.row, c.ticked)).toEqual(c.to);
  });

  it("never leaves the request ticked without the read, from any start state", () => {
    const starts: readonly AbilityTicks[] = [
      both,
      readOnly,
      neither,
      // A state the box never produces, to show the rule does not depend on it.
      { read: false, request: true },
    ];
    for (const from of starts) {
      for (const row of ["read", "request"] as const) {
        for (const ticked of [true, false]) {
          const next = nextAbilityTicks(from, row, ticked);
          expect(!next.request || next.read).toBe(true);
        }
      }
    }
  });

  it("does not change the state it was given", () => {
    const from = Object.freeze({ read: true, request: true });
    expect(() => nextAbilityTicks(from, "read", false)).not.toThrow();
    expect(from).toEqual({ read: true, request: true });
  });
});

// The one payload rule for the pair, used by the consent approval and by the
// wizard's mint request.
describe("withoutOrphanedRequest", () => {
  it("drops the request when the read is not in the list", () => {
    expect(withoutOrphanedRequest(["mcp.sites.read", "mcp.ability.request"])).toEqual([
      "mcp.sites.read",
    ]);
    expect(withoutOrphanedRequest(["mcp.ability.request"])).toEqual([]);
  });

  it("keeps the request beside the read, in the order given", () => {
    const both = ["mcp.ability.request", "mcp.sites.read", "mcp.ability.read"];
    expect(withoutOrphanedRequest(both)).toEqual(both);
  });

  it("leaves a list without the request alone, whether or not it holds the read", () => {
    expect(withoutOrphanedRequest(["mcp.sites.read"])).toEqual(["mcp.sites.read"]);
    expect(withoutOrphanedRequest(["mcp.ability.read"])).toEqual(["mcp.ability.read"]);
    expect(withoutOrphanedRequest([])).toEqual([]);
  });

  it("does not change the list it was given", () => {
    const before = Object.freeze(["mcp.sites.read", "mcp.ability.request"]);
    expect(() => withoutOrphanedRequest(before)).not.toThrow();
    expect(before).toEqual(["mcp.sites.read", "mcp.ability.request"]);
  });
});
