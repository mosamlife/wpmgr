import { describe, it, expect } from "vitest";

import { CONFERRABLE_READS, KNOWN_CAPABILITIES } from "./capabilities";
import {
  DEFAULT_PRESET_ID,
  capabilityPresets,
  conferrableReadsIn,
  defaultCapabilities,
  presetFor,
  withAbilityTicks,
  withCapability,
  withPreset,
} from "./capability-presets";

// What the server confers for mcp:read: scopeCapabilities[ScopeRead] in
// apps/api/internal/mcp/policy.go. Written out here, not derived from the
// dashboard's own vocabulary, so a change to that vocabulary cannot quietly
// change what this file says the server sent.
const SERVER_READS = [
  "mcp.activity.read",
  "mcp.backups.read",
  "mcp.diagnostics.read",
  "mcp.performance.read",
  "mcp.security.read",
  "mcp.sites.read",
  "mcp.uptime.read",
];

// Everything in the vocabulary that is NOT a conferrable read: the unreachable
// read, the cache-clear request and the two site-tools names.
const NOT_CONFERRABLE_READS = [
  "mcp.content.read",
  "mcp.cache.purge",
  "mcp.ability.read",
  "mcp.ability.request",
];

describe("the build's conferrable reads match what the server confers for mcp:read", () => {
  it("is exactly the seven reads, and no name outside them", () => {
    expect([...CONFERRABLE_READS].sort()).toEqual([...SERVER_READS].sort());
    for (const name of NOT_CONFERRABLE_READS) {
      expect(KNOWN_CAPABILITIES).toContain(name);
      expect(CONFERRABLE_READS).not.toContain(name);
    }
  });
});

describe("capabilityPresets", () => {
  it("offers Just the basics and Read everything when every read is offered", () => {
    const presets = capabilityPresets(SERVER_READS);
    expect(presets.map((p) => p.id)).toEqual(["basics", "read-everything"]);
    expect(presets.map((p) => p.label)).toEqual(["Just the basics", "Read everything"]);
  });

  it("makes Just the basics Sites alone, the server's own default for an omitted list", () => {
    const basics = capabilityPresets(SERVER_READS).find((p) => p.id === "basics");
    expect(basics?.capabilities).toEqual(["mcp.sites.read"]);
  });

  it("makes Read everything every offered read, and not one name more", () => {
    const everything = capabilityPresets(SERVER_READS).find((p) => p.id === "read-everything");
    expect([...(everything?.capabilities ?? [])].sort()).toEqual([...SERVER_READS].sort());
  });

  it("never lets a write or the unreachable read into a preset, whatever it is handed", () => {
    // The caller is told to pass reads only. This pins what happens when it
    // does not: the presets are built through the conferrable-reads filter, so
    // ruling 33 (no write in any preset) holds without trusting the caller.
    for (const preset of capabilityPresets([...SERVER_READS, ...NOT_CONFERRABLE_READS])) {
      for (const name of NOT_CONFERRABLE_READS) {
        expect(preset.capabilities).not.toContain(name);
      }
    }
  });

  it("builds Read everything from what is offered, so an offer of two reads is two reads", () => {
    const presets = capabilityPresets(["mcp.uptime.read", "mcp.sites.read"]);
    expect(presets.map((p) => p.id)).toEqual(["basics", "read-everything"]);
    const everything = presets.find((p) => p.id === "read-everything");
    expect([...(everything?.capabilities ?? [])].sort()).toEqual([
      "mcp.sites.read",
      "mcp.uptime.read",
    ]);
  });

  it("does not offer a preset that would tick nothing", () => {
    // Offered reads without Sites leave Just the basics empty. An empty preset
    // would match the empty selection in presetFor, so a screen with nothing
    // ticked would claim to be on a named shortcut.
    const presets = capabilityPresets(["mcp.uptime.read"]);
    expect(presets.map((p) => p.id)).toEqual(["read-everything"]);
    expect(presets[0]?.capabilities).toEqual(["mcp.uptime.read"]);
    expect(capabilityPresets([])).toEqual([]);
  });

  it("does not offer a second preset that repeats the first one's set", () => {
    // With Sites the only read on offer, Read everything IS Just the basics.
    // The first match would always win, so the second could never be the
    // active one.
    const presets = capabilityPresets(["mcp.sites.read"]);
    expect(presets.map((p) => p.id)).toEqual(["basics"]);
  });
});

describe("presetFor", () => {
  const presets = capabilityPresets(SERVER_READS);

  it("names the preset the selection is, and null for any other set", () => {
    expect(presetFor(["mcp.sites.read"], presets)).toBe("basics");
    expect(presetFor(SERVER_READS, presets)).toBe("read-everything");
    expect(presetFor(["mcp.sites.read", "mcp.uptime.read"], presets)).toBeNull();
  });

  it("ignores the order the ticks were made in", () => {
    expect(presetFor([...SERVER_READS].reverse(), presets)).toBe("read-everything");
  });

  it("calls the empty selection Custom, never a preset", () => {
    expect(presetFor([], presets)).toBeNull();
  });

  it("judges the read rows only: a site-tools or cache-clear tick never moves it", () => {
    // Owner ruling 2026-10-09: a preset sets the reads and nothing else, so the
    // claim is about the reads. The ticks further down are in no preset and
    // leave the claim exactly where the read rows put it.
    expect(presetFor(["mcp.sites.read", "mcp.cache.purge"], presets)).toBe("basics");
    expect(presetFor(["mcp.sites.read", "mcp.ability.read", "mcp.ability.request"], presets)).toBe(
      "basics",
    );
    expect(presetFor([...SERVER_READS, "mcp.ability.read", "mcp.cache.purge"], presets)).toBe(
      "read-everything",
    );
  });

  it("still calls the set Custom when the read rows are neither shortcut, whatever else is ticked", () => {
    expect(presetFor(["mcp.sites.read", "mcp.uptime.read", "mcp.ability.read"], presets)).toBeNull();
    // No read row at all matches no shortcut, even with other boxes ticked.
    expect(presetFor(["mcp.ability.read", "mcp.cache.purge"], presets)).toBeNull();
  });

  it("claims nothing when the surface offers no preset", () => {
    expect(presetFor(["mcp.sites.read"], [])).toBeNull();
  });
});

describe("defaultCapabilities", () => {
  it("is Sites alone when every read is offered: the wizard's default preset", () => {
    expect(DEFAULT_PRESET_ID).toBe("basics");
    expect(defaultCapabilities(SERVER_READS)).toEqual(["mcp.sites.read"]);
  });

  it("is never wider than the default preset, however many reads are offered", () => {
    expect(defaultCapabilities([...SERVER_READS, ...NOT_CONFERRABLE_READS])).toEqual([
      "mcp.sites.read",
    ]);
  });

  it("is empty when Sites is not offered, rather than a read that is not on offer", () => {
    expect(defaultCapabilities(["mcp.uptime.read", "mcp.backups.read"])).toEqual([]);
    expect(defaultCapabilities([])).toEqual([]);
  });
});

describe("conferrableReadsIn", () => {
  it("keeps only conferrable reads, once each, in vocabulary order", () => {
    const got = conferrableReadsIn([
      "mcp.uptime.read",
      "mcp.cache.purge",
      "mcp.sites.read",
      "mcp.content.read",
      "mcp.uptime.read",
      "mcp.unheard-of.read",
    ]);
    // Sites comes before Uptime in the vocabulary, whatever order they were
    // handed in.
    expect(got).toEqual(["mcp.sites.read", "mcp.uptime.read"]);
  });
});

describe("withCapability", () => {
  it("ticks a name once and unticks it", () => {
    expect(withCapability(["mcp.sites.read"], "mcp.uptime.read", true)).toEqual([
      "mcp.sites.read",
      "mcp.uptime.read",
    ]);
    expect(withCapability(["mcp.sites.read", "mcp.uptime.read"], "mcp.sites.read", false)).toEqual([
      "mcp.uptime.read",
    ]);
  });

  it("never adds a name that is already ticked, and never removes one that is not", () => {
    expect(withCapability(["mcp.sites.read"], "mcp.sites.read", true)).toEqual(["mcp.sites.read"]);
    expect(withCapability(["mcp.sites.read"], "mcp.uptime.read", false)).toEqual(["mcp.sites.read"]);
  });

  it("leaves the list it was given alone", () => {
    const before = Object.freeze(["mcp.sites.read"]);
    withCapability(before, "mcp.uptime.read", true);
    withCapability(before, "mcp.sites.read", false);
    expect(before).toEqual(["mcp.sites.read"]);
  });
});

// Owner ruling 2026-10-09: a preset changes the read rows and nothing else. A
// press leaves the site-tools ticks and the cache-clear tick exactly as they were.
describe("withPreset", () => {
  const presets = capabilityPresets(SERVER_READS);
  const basics = presets.find((p) => p.id === "basics")!;
  const everything = presets.find((p) => p.id === "read-everything")!;

  it("sets the read rows to the preset's reads and leaves a ticked site tool and cache clear exactly as they were", () => {
    const start = ["mcp.sites.read", "mcp.cache.purge", "mcp.ability.read", "mcp.ability.request"];
    const afterEverything = withPreset(start, everything);
    expect([...afterEverything].sort()).toEqual(
      [...SERVER_READS, "mcp.cache.purge", "mcp.ability.read", "mcp.ability.request"].sort(),
    );
    // And back: the reads shrink, the other three ticks are still there.
    const afterBasics = withPreset(afterEverything, basics);
    expect([...afterBasics].sort()).toEqual(
      ["mcp.sites.read", "mcp.cache.purge", "mcp.ability.read", "mcp.ability.request"].sort(),
    );
  });

  it("does not tick a box that was clear: a press never adds a site tool or the cache clear", () => {
    const afterEverything = withPreset(["mcp.sites.read"], everything);
    expect([...afterEverything].sort()).toEqual([...SERVER_READS].sort());
    for (const name of NOT_CONFERRABLE_READS) expect(afterEverything).not.toContain(name);
    expect(withPreset([], basics)).toEqual(["mcp.sites.read"]);
  });

  it("keeps a site tool the person cleared cleared, and the other one ticked", () => {
    // "See what the site can do" ticked and "ask for changes" cleared.
    const start = ["mcp.sites.read", "mcp.ability.read"];
    expect([...withPreset(start, everything)].sort()).toEqual(
      [...SERVER_READS, "mcp.ability.read"].sort(),
    );
  });

  it("replaces read rows it was holding rather than adding to them, and never lists a name twice", () => {
    const afterBasics = withPreset(["mcp.uptime.read", "mcp.backups.read", "mcp.sites.read"], basics);
    expect(afterBasics).toEqual(["mcp.sites.read"]);
    const afterEverything = withPreset([...SERVER_READS, "mcp.ability.read"], everything);
    expect(afterEverything.filter((c) => c === "mcp.sites.read")).toHaveLength(1);
    expect(new Set(afterEverything).size).toBe(afterEverything.length);
  });

  it("leaves the claim consistent: after a press, presetFor names that preset whatever else is ticked", () => {
    const others = ["mcp.cache.purge", "mcp.ability.read", "mcp.ability.request"];
    for (const preset of presets) {
      expect(presetFor(withPreset(others, preset), presets)).toBe(preset.id);
      expect(presetFor(withPreset([...SERVER_READS, ...others], preset), presets)).toBe(preset.id);
    }
  });

  it("does not change the list it was given", () => {
    const before = Object.freeze(["mcp.sites.read", "mcp.ability.read"]);
    expect(() => withPreset(before, everything)).not.toThrow();
    expect(before).toEqual(["mcp.sites.read", "mcp.ability.read"]);
  });
});

// The line under the chip says what the active shortcut is. A shortcut sets the
// read rows, and the site-tools box (which holds a read that returns page text)
// can be ticked alongside it, so each description is about THIS LIST and claims
// nothing about the connection as a whole.
describe("the preset descriptions", () => {
  const presets = capabilityPresets(SERVER_READS);
  const description = (id: string) => presets.find((p) => p.id === id)?.description;

  it("is written about the rows in its own list, in plain words", () => {
    expect(description("basics")).toBe("See which sites are in scope. Nothing else from this list.");
    expect(description("read-everything")).toBe(
      "Every read in this list. None of them can change anything.",
    );
  });

  it("says 'this list' and makes no claim about the connection as a whole", () => {
    // A ticked request, or the site-tools read, would make any such claim false.
    for (const preset of presets) {
      expect(preset.description).toMatch(/\bthis list\b/);
      expect(preset.description).not.toMatch(/\bonly\b/i);
      expect(preset.description).not.toMatch(/no other read/i);
      expect(preset.description).not.toMatch(/this connection/i);
      expect(preset.description).not.toMatch(/\bit (still )?(can|cannot)\b/i);
      expect(preset.description).not.toMatch(new RegExp(`[${String.fromCharCode(0x2013, 0x2014)}]`));
    }
  });
});

describe("withAbilityTicks", () => {
  it("sets both site-tools rows in one step and leaves every other tick alone", () => {
    const start = ["mcp.sites.read", "mcp.cache.purge"];
    expect(withAbilityTicks(start, { read: true, request: true })).toEqual([
      "mcp.sites.read",
      "mcp.cache.purge",
      "mcp.ability.read",
      "mcp.ability.request",
    ]);
    expect(
      withAbilityTicks(
        ["mcp.sites.read", "mcp.ability.read", "mcp.ability.request"],
        { read: false, request: false },
      ),
    ).toEqual(["mcp.sites.read"]);
  });

  it("sets one row without touching the other, and never adds a name twice", () => {
    const both = ["mcp.ability.read", "mcp.ability.request"];
    expect(withAbilityTicks(both, { read: true, request: false })).toEqual(["mcp.ability.read"]);
    expect(withAbilityTicks(both, { read: true, request: true })).toEqual(both);
  });

  it("leaves the list it was given alone", () => {
    const before = Object.freeze(["mcp.sites.read"]);
    withAbilityTicks(before, { read: true, request: true });
    expect(before).toEqual(["mcp.sites.read"]);
  });
});
