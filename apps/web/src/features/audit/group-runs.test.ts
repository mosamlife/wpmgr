/**
 * Tests for the read-burst collapsing algorithm (redesign point 6).
 *
 * Pure-function tests only (project convention, see scan-findings.test.ts).
 * The one rule that matters most here: a denied/sensitive/write entry always
 * breaks the current run and renders standalone — a run of reads never
 * swallows a risk event.
 */
import { describe, it, expect } from "vitest";
import type { AuditEntry } from "@wpmgr/api";

import { commonPathPrefix } from "./metadata";
import { groupRuns } from "./group-runs";

let seq = 0;

function entry(overrides: Partial<AuditEntry> = {}): AuditEntry {
  seq += 1;
  return {
    id: overrides.id ?? `entry-${seq}`,
    tenant_id: "tenant-1",
    actor_type: "user",
    actor_id: "user-1",
    action: "site.files.read",
    target_type: "site",
    target_id: "site-1",
    prev_hash: "prev",
    hash: "hash",
    created_at: `2026-01-01T12:00:${String(seq).padStart(2, "0")}Z`,
    ...overrides,
  };
}

describe("groupRuns", () => {
  it("collapses a consecutive same-actor/action/site read burst into one run", () => {
    const entries = [entry(), entry(), entry()];
    const runs = groupRuns(entries);
    expect(runs).toHaveLength(1);
    expect(runs[0]?.kind).toBe("run");
    expect(runs[0]?.entries).toHaveLength(3);
  });

  it("never collapses a denied, sensitive, or write entry — each renders standalone", () => {
    const entries = [
      entry(), // read
      entry(), // read
      entry({ action: "site.files.write" }), // write — breaks the run
      entry(), // read
      entry(), // read
      entry({ action: "site_security_ban.create" }), // sensitive — breaks the run
      entry({ action: "site.files.delete.denied" }), // denied — breaks the run
    ];
    const runs = groupRuns(entries);

    // Read x2, Write (standalone), Read x2, Sensitive (standalone), Denied (standalone)
    expect(runs.map((r) => r.kind)).toEqual(["run", "single", "run", "single", "single"]);
    expect(runs[0]?.entries).toHaveLength(2);
    expect(runs[2]?.entries).toHaveLength(2);
  });

  it("starts a new run when the actor, action, or target differs", () => {
    const entries = [
      entry({ actor_id: "user-1" }),
      entry({ actor_id: "user-2" }),
      entry({ target_id: "site-2" }),
    ];
    const runs = groupRuns(entries);
    expect(runs).toHaveLength(3);
    expect(runs.every((r) => r.kind === "single")).toBe(true);
  });

  it("preserves input order and never reorders entries", () => {
    const a = entry({ id: "a" });
    const b = entry({ id: "b", action: "site.files.write" });
    const c = entry({ id: "c" });
    const runs = groupRuns([a, b, c]);
    expect(runs.flatMap((r) => r.entries.map((e) => e.id))).toEqual(["a", "b", "c"]);
  });

  it("de-dupes a repeated entry id defensively", () => {
    const a = entry({ id: "dup" });
    const runs = groupRuns([a, a, entry({ id: "unique" })]);
    const ids = runs.flatMap((r) => r.entries.map((e) => e.id));
    expect(ids).toEqual(["dup", "unique"]);
  });

  it("does not merge reads that are far apart in time", () => {
    const a = entry({ created_at: "2026-01-01T12:00:00Z" });
    const b = entry({ created_at: "2026-01-01T13:00:00Z" });
    const runs = groupRuns([a, b]);
    expect(runs).toHaveLength(2);
  });
});

describe("commonPathPrefix", () => {
  it("finds the shared directory prefix across paths, marked truncated", () => {
    const prefix = commonPathPrefix([
      "wp-content/uploads/2026/01/a.jpg",
      "wp-content/uploads/2026/01/b.jpg",
      "wp-content/uploads/2026/02/c.jpg",
    ]);
    expect(prefix).toBe("wp-content/uploads/2026/…");
  });

  it("returns the exact path when every entry shares the same one", () => {
    const prefix = commonPathPrefix(["wp-content/uploads/a.jpg", "wp-content/uploads/a.jpg"]);
    expect(prefix).toBe("wp-content/uploads/a.jpg");
  });

  it("returns null when there is no shared prefix at all", () => {
    expect(commonPathPrefix(["wp-content/a.jpg", "wp-includes/b.php"])).toBeNull();
  });

  it("returns null when no path is present", () => {
    expect(commonPathPrefix([null, null])).toBeNull();
  });
});
