import { describe, it, expect } from "vitest";
import type { AbilityRequest } from "@wpmgr/api";

import { pageCreateRow, pageEditRow } from "./ability-request-rows";

// The ability-request fixtures refuse a row no site could have, so a screen
// test cannot be written against a state the screen is never shown. This pins
// the two shape rules for the closed kind of an outside change
// (apps/api/internal/abilityrequest/outcome_detail.go outsideChangeFor) and the
// code of a failed undo (m169's undo_code CHECKs, undo.go undoCodeOf): each
// must let the rows the API returns through, and stop the ones it cannot.

const DECIDED = "2026-10-01T09:58:00Z";
const IN_WINDOW = "2026-10-12T09:58:00Z";

function refused(code: string, over: Partial<AbilityRequest> = {}): AbilityRequest {
  return pageEditRow({ state: "failed", outcome: "refused", outcome_code: code, decided_at: DECIDED, ...over });
}

function applied(over: Partial<AbilityRequest> = {}): AbilityRequest {
  return pageEditRow({
    state: "done",
    outcome: "applied",
    decided_at: DECIDED,
    undo_available_until: IN_WINDOW,
    ...over,
  });
}

// Words outside the closed sets, past the types on purpose.
const asKind = (v: string) => v as unknown as AbilityRequest["outside_change"];
const asCode = (v: string) => v as unknown as AbilityRequest["undo_code"];

describe("the fixture's outside_change", () => {
  it.each(["active_kit", "other_posts", "terms", "site_settings", "users"] as const)(
    "lets %s through on a page edit refused with side_effect_detected",
    (kind) => {
      expect(refused("side_effect_detected", { restored: true, outside_change: kind }).outside_change).toBe(kind);
    },
  );

  it("lets a refusal with no kind, or a null one, through", () => {
    expect(() => refused("side_effect_detected", { restored: true })).not.toThrow();
    expect(() => refused("side_effect_detected", { restored: true, outside_change: null })).not.toThrow();
  });

  it("stops a kind outside the closed set", () => {
    expect(() => refused("side_effect_detected", { outside_change: asKind("cache_files") })).toThrow(
      /outside_change value/,
    );
  });

  it("stops a kind on a page edit refused for any other code", () => {
    expect(() => refused("verify_mismatch", { outside_change: "terms" })).toThrow(/outside_change belongs to/);
  });

  it("stops a kind on a request that is not a page edit", () => {
    expect(() =>
      pageCreateRow({
        state: "failed",
        outcome: "refused",
        outcome_code: "side_effect_detected",
        decided_at: DECIDED,
        outside_change: "terms",
      }),
    ).toThrow(/outside_change belongs to/);
  });
});

describe("the fixture's undo_code", () => {
  it.each(["snapshot_tampered", "restore_mismatch"] as const)("lets %s through on an undo that failed", (code) => {
    expect(applied({ undo_state: "failed", undo_code: code }).undo_code).toBe(code);
  });

  it("lets a failed undo with no code, or a null one, through", () => {
    expect(() => applied({ undo_state: "failed" })).not.toThrow();
    expect(() => applied({ undo_state: "failed", undo_code: null })).not.toThrow();
  });

  it("stops a code outside the closed set", () => {
    expect(() => applied({ undo_state: "failed", undo_code: asCode("conflict") })).toThrow(/undo_code value/);
  });

  it("stops a code on an undo that did not fail", () => {
    for (const state of ["available", "in_progress", "undone", "refused_conflict", "refused_published"]) {
      expect(() => applied({ undo_state: state, undo_code: "snapshot_tampered" })).toThrow(/undo_code belongs to/);
    }
  });

  it("stops a code on a request with no undo", () => {
    expect(() => applied({ undo_code: "restore_mismatch" })).toThrow(/undo_code belongs to/);
  });
});
