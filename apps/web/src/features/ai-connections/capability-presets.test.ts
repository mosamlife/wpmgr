import { describe, it, expect } from "vitest";

import { CONFERRABLE_READS, KNOWN_CAPABILITIES } from "./capabilities";
import {
  DEFAULT_PRESET_ID,
  capabilityPresets,
  conferrableReadsIn,
  defaultCapabilities,
  presetFor,
  withCapability,
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

  it("derives the claim over the whole tick list, so a ticked write makes it Custom", () => {
    // The preset's description says "and nothing else". Over a set that holds
    // the cache-clear request that sentence would be untrue.
    expect(presetFor(["mcp.sites.read", "mcp.cache.purge"], presets)).toBeNull();
    expect(presetFor([...SERVER_READS, "mcp.ability.read"], presets)).toBeNull();
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
