import { describe, it, expect } from "vitest";

import { pruneSelection } from "./use-backups-selection";

describe("pruneSelection", () => {
  it("returns the same reference when the selection is already empty", () => {
    const empty = new Set<string>();
    expect(pruneSelection(empty, ["a", "b"])).toBe(empty);
  });

  it("returns the same reference when nothing needs pruning", () => {
    const selected = new Set(["a", "b"]);
    expect(pruneSelection(selected, ["a", "b", "c"])).toBe(selected);
  });

  it("drops ids that are no longer present in validIds", () => {
    const selected = new Set(["a", "b", "c"]);
    const next = pruneSelection(selected, ["a", "c"]);
    expect(next).not.toBe(selected);
    expect(Array.from(next).sort()).toEqual(["a", "c"]);
  });

  it("returns an empty set when every selected id vanished", () => {
    const selected = new Set(["a", "b"]);
    const next = pruneSelection(selected, ["z"]);
    expect(next.size).toBe(0);
  });
});
