import { describe, it, expect } from "vitest";
import type { AbilityRequest } from "@wpmgr/api";

import { autoApproved, pageCreateRow, pageEditRow, personApproved, waitsBecause } from "./ability-request-rows";

// The ability-request fixtures refuse a row no site could have, so a screen
// test cannot be written against a state the screen is never shown. This pins
// the shape rules for the closed kind of an outside change
// (apps/api/internal/abilityrequest/outcome_detail.go outsideChangeFor), the
// code of a failed undo (m169's undo_code CHECKs, undo.go undoCodeOf) and how a
// request was approved or why it waits (m174's CHECKs, approval_dto.go): each
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

describe("the fixture's approval (m174)", () => {
  it.each(["approved", "dispatched", "done", "failed", "not_sent", "outcome_unknown"] as const)(
    "lets a setting's approval through on a %s request, with its class and no ask_reason",
    (state) => {
      const over: Partial<AbilityRequest> =
        state === "done"
          ? { outcome: "applied", undo_state: "available", undo_available_until: IN_WINDOW }
          : state === "failed"
            ? { outcome: "refused", outcome_code: "conflict" }
            : state === "not_sent"
              ? { outcome: "not_sent", not_sent_reason: "setting_changed" }
              : {};
      const row = pageEditRow({ state, decided_at: DECIDED, ...over, ...autoApproved() });
      expect(row.approval?.source).toBe("policy");
    },
  );

  it("lets a person's approval through, with no setting", () => {
    expect(pageEditRow({ state: "approved", decided_at: DECIDED, ...personApproved() }).approval?.source).toBe("person");
  });

  it("stops an approval on a request that has not been approved", () => {
    for (const state of ["pending", "declined", "withdrawn", "expired"] as const) {
      const decided = state === "declined" ? { decided_at: DECIDED } : {};
      expect(() => pageEditRow({ state, ...decided, ...autoApproved() })).toThrow(/approval belongs to the approved states/);
    }
  });

  it("stops a person's approval that names a setting", () => {
    expect(() =>
      pageEditRow({
        state: "approved",
        decided_at: DECIDED,
        ...personApproved(),
        approval: { source: "person", setting: autoApproved().approval!.setting },
      }),
    ).toThrow(/person's approval names no setting/);
  });

  it("stops a setting's approval on a request that waited, and one with no class", () => {
    expect(() =>
      pageEditRow({ state: "approved", decided_at: DECIDED, ...autoApproved(), ask_reason: "site_mode_ask" }),
    ).toThrow(/never waited/);
    expect(() =>
      pageEditRow({ state: "approved", decided_at: DECIDED, ...autoApproved(), change_class: null }),
    ).toThrow(/names the class WPMgr decided/);
  });
});

describe("the fixture's ask_reason and not_sent_reason (m174)", () => {
  it.each([
    "kind_always_asks",
    "unknown_target_state",
    "site_mode_ask",
    "kind_not_in_mode",
    "setter_lacks_permission",
    "over_change_budget",
    "over_site_cap",
    "connection_never_auto",
    "connection_setter_invalid",
    "not_checked",
  ] as const)("lets %s through on a request that waits", (reason) => {
    expect(pageEditRow({ ...waitsBecause(reason) }).ask_reason).toBe(reason);
  });

  it("lets a request a person approved keep the reason it waited for", () => {
    expect(() =>
      pageEditRow({ state: "approved", decided_at: DECIDED, ...personApproved(), ask_reason: "site_mode_ask" }),
    ).not.toThrow();
  });

  it("stops a reason outside the contract's ten", () => {
    const outside = "session_ended" as unknown as NonNullable<AbilityRequest["ask_reason"]>;
    expect(() => pageEditRow({ ...waitsBecause(outside) })).toThrow(/ask_reason value/);
  });

  it.each(["setting_changed", "class_changed"])("lets %s close an approved request that was never sent", (reason) => {
    expect(() =>
      pageEditRow({ state: "not_sent", decided_at: DECIDED, outcome: "not_sent", not_sent_reason: reason, ...autoApproved() }),
    ).not.toThrow();
  });

  it("stops a not_sent_reason the database does not have", () => {
    expect(() =>
      pageEditRow({ state: "not_sent", decided_at: DECIDED, outcome: "not_sent", not_sent_reason: "setting_moved" }),
    ).toThrow(/not_sent_reason value/);
  });
});
