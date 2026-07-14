import { describe, it, expect } from "vitest";

import {
  chainDependents,
  terminalChainDependents,
  memberCheckState,
  chainCheckState,
  countAutoIncludedDependents,
  reclaimableBytes,
} from "./backups-chain-selection";
import type { BackupSnapshot } from "@wpmgr/api";

function snap(overrides: Partial<BackupSnapshot> & { id: string }): BackupSnapshot {
  return {
    tenant_id: "t1",
    site_id: "s1",
    kind: "full",
    status: "completed",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
    ...overrides,
  };
}

// A 3-member linear chain: gen 0 (base), gen 1 (child of base), gen 2 (tip,
// child of gen1) — parent_snapshot_id mirrors generation order here so the
// pre-existing tri-state tests below still hold under the parent-id fix.
const base = snap({ id: "base", chain_id: "chain-1", generation: 0 });
const gen1 = snap({
  id: "gen1",
  chain_id: "chain-1",
  generation: 1,
  parent_snapshot_id: "base",
});
const gen2 = snap({
  id: "gen2",
  chain_id: "chain-1",
  generation: 2,
  parent_snapshot_id: "gen1",
});
const chain = [base, gen1, gen2];

describe("chainDependents", () => {
  it("returns every member whose parent_snapshot_id is this member's id", () => {
    expect(chainDependents(chain, base).map((m) => m.id)).toEqual(["gen1"]);
    expect(chainDependents(chain, gen1).map((m) => m.id)).toEqual(["gen2"]);
    expect(chainDependents(chain, gen2)).toEqual([]);
  });

  it("GH #221: a failed same-generation sibling with no real children shows zero dependents", () => {
    // A chain where a failed attempt and a successful retry share a parent
    // and generation: A(gen0) -> S(gen1, parent A) + F(gen1, parent A,
    // failed 0-byte) -> C(gen2, parent S). Only S produced a child; F has
    // none, even though it shares S's generation.
    const chainA = snap({ id: "A", chain_id: "chain-2", generation: 0 });
    const chainS = snap({
      id: "S",
      chain_id: "chain-2",
      generation: 1,
      parent_snapshot_id: "A",
      status: "completed",
    });
    const chainF = snap({
      id: "F",
      chain_id: "chain-2",
      generation: 1,
      parent_snapshot_id: "A",
      status: "failed",
      total_size: 0,
    });
    const chainC = snap({
      id: "C",
      chain_id: "chain-2",
      generation: 2,
      parent_snapshot_id: "S",
    });
    const members = [chainA, chainS, chainF, chainC];

    expect(chainDependents(members, chainF)).toEqual([]);
    expect(chainDependents(members, chainS).map((m) => m.id)).toEqual(["C"]);
    expect(chainDependents(members, chainA).map((m) => m.id)).toEqual([
      "S",
      "F",
    ]);
  });
});

describe("terminalChainDependents", () => {
  it("excludes non-terminal dependents", () => {
    const running = { ...gen2, status: "running" as const };
    const members = [base, gen1, running];
    expect(terminalChainDependents(members, base).map((m) => m.id)).toEqual([
      "gen1",
    ]);
  });
});

describe("memberCheckState", () => {
  it("is unchecked when the id is not in the selection", () => {
    expect(memberCheckState(new Set(), chain, base)).toBe("unchecked");
  });

  it("is checked for a member with no dependents (the tip)", () => {
    expect(memberCheckState(new Set(["gen2"]), chain, gen2)).toBe("checked");
  });

  it("is checked when every dependent is also selected", () => {
    const selected = new Set(["base", "gen1", "gen2"]);
    expect(memberCheckState(selected, chain, base)).toBe("checked");
  });

  it("is indeterminate when selected but a dependent is not", () => {
    // gen1's direct dependent is gen2 (parent_snapshot_id: "gen1"), not
    // selected here — base is unaffected since its only direct dependent
    // (gen1) IS selected.
    const selected = new Set(["base", "gen1"]); // gen2 not selected
    expect(memberCheckState(selected, chain, gen1)).toBe("indeterminate");
    expect(memberCheckState(selected, chain, base)).toBe("checked");
  });

  it("is indeterminate when a dependent is non-terminal and can never be selected", () => {
    const running = { ...gen2, status: "running" as const };
    const members = [base, gen1, running];
    const selected = new Set(["base", "gen1"]);
    expect(memberCheckState(selected, members, gen1)).toBe("indeterminate");
  });
});

describe("chainCheckState", () => {
  it("is unchecked when the chain has zero terminal members", () => {
    const allRunning = chain.map((m) => ({ ...m, status: "running" as const }));
    expect(chainCheckState(new Set(), allRunning)).toBe("unchecked");
  });

  it("is unchecked when nothing is selected", () => {
    expect(chainCheckState(new Set(), chain)).toBe("unchecked");
  });

  it("is checked when every terminal member is selected", () => {
    expect(chainCheckState(new Set(["base", "gen1", "gen2"]), chain)).toBe(
      "checked",
    );
  });

  it("is indeterminate when some but not all terminal members are selected", () => {
    expect(chainCheckState(new Set(["base"]), chain)).toBe("indeterminate");
  });

  it("ignores non-terminal members when computing the eligible denominator", () => {
    const running = { ...gen2, status: "running" as const };
    const members = [base, gen1, running];
    // Both terminal members (base, gen1) selected -> checked, even though
    // the non-terminal gen2 could never be included.
    expect(chainCheckState(new Set(["base", "gen1"]), members)).toBe(
      "checked",
    );
  });
});

describe("countAutoIncludedDependents", () => {
  it("returns 0 for a batch with no chains", () => {
    const singleton = snap({ id: "solo", chain_id: undefined });
    expect(countAutoIncludedDependents([singleton])).toBe(0);
  });

  it("returns 0 when only one member of a chain is in the batch", () => {
    expect(countAutoIncludedDependents([base])).toBe(0);
  });

  it("counts members above the lowest selected generation in a chain", () => {
    // base + gen2 selected (gen1 might have been GC'd or excluded) -> gen2 is
    // "above" the min selected generation (0), so it counts as a dependent.
    expect(countAutoIncludedDependents([base, gen2])).toBe(1);
  });

  it("counts across multiple independent chains in the same batch", () => {
    const chainB0 = snap({ id: "b0", chain_id: "chain-2", generation: 0 });
    const chainB1 = snap({ id: "b1", chain_id: "chain-2", generation: 1 });
    const batch = [base, gen1, gen2, chainB0, chainB1];
    // chain-1: min gen 0 -> gen1, gen2 count (2). chain-2: min gen 0 -> b1 counts (1).
    expect(countAutoIncludedDependents(batch)).toBe(3);
  });
});

describe("reclaimableBytes", () => {
  it("sums total_size across the batch", () => {
    const a = snap({ id: "a", total_size: 100 });
    const b = snap({ id: "b", total_size: 250 });
    expect(reclaimableBytes([a, b])).toBe(350);
  });

  it("excludes locked snapshots from the sum", () => {
    const a = snap({ id: "a", total_size: 100 });
    const locked = snap({ id: "b", total_size: 250, locked: true });
    expect(reclaimableBytes([a, locked])).toBe(100);
  });

  it("treats a missing total_size as 0", () => {
    const a = snap({ id: "a" });
    expect(reclaimableBytes([a])).toBe(0);
  });
});
