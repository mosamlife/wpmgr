import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import type { AbilityRequest } from "@wpmgr/api";

import { AbilityRequestCard } from "./ability-request-card";
import { abilityStatus } from "./ability-card-model";

// Every combination below satisfies the m156 CHECKs: state_matches_outcome
// (failed -> refused | verify_mismatch | failed; outcome_unknown -> NULL |
// outcome_unknown), created_post_id > 0, and ledger refs only on done, failed
// or outcome_unknown rows.
const base: AbilityRequest = {
  id: "r1",
  site_id: "s1",
  ability_name: "wpmgr/page-create",
  input_json: "{}",
  effect_copy: "draft",
  snapshot: "x",
  site_label: "Shop",
  site_host: "shop.example",
  grant_label: "grant",
  grant_via: "mcp",
  card_copy_version: 1,
  state: "failed",
  created_at: "2026-10-01T10:00:00Z",
  expires_at: "2026-10-01T11:00:00Z",
  post_type: "page",
};
const mk = (o: Partial<AbilityRequest>): AbilityRequest => ({ ...base, ...o });

describe("abilityStatus on failed and unknown rows", () => {
  it("failed/interrupted with a post id says a draft may exist", () => {
    const s = abilityStatus(mk({ outcome: "failed", outcome_code: "interrupted", created_post_id: 7 }));
    expect(s.text).toContain("may exist");
    expect(s.text).not.toContain("was not created");
    expect(s.draftMayExist).toBe(true);
  });
  it("failed without a post id keeps the not-created text", () => {
    const s = abilityStatus(mk({ outcome: "failed" }));
    expect(s.text).toContain("was not created");
    expect(s.draftMayExist).toBeUndefined();
  });
  it("verify_mismatch with a post id says a draft may exist", () => {
    const s = abilityStatus(mk({ outcome: "verify_mismatch", created_post_id: 7 }));
    expect(s.text).toContain("did not match");
    expect(s.text).toContain("may exist");
    expect(s.draftMayExist).toBe(true);
  });
  it("verify_mismatch + trashed says the draft was moved to the trash", () => {
    const s = abilityStatus(mk({ outcome: "verify_mismatch", created_post_id: 7, trashed: true }));
    expect(s.text).toContain("moved to the trash");
    expect(s.text).not.toContain("may exist");
    expect(s.draftMayExist).toBeUndefined();
  });
  it("verify_mismatch without a post id keeps the check-drafts text", () => {
    const s = abilityStatus(mk({ outcome: "verify_mismatch" }));
    expect(s.text).toContain("Check the site's drafts");
  });
  it("refused without a post id says nothing was created", () => {
    expect(abilityStatus(mk({ outcome: "refused", outcome_code: "forbidden" })).text).toContain(
      "Nothing was created",
    );
  });
  it("outcome_unknown with a post id says a draft may exist", () => {
    const s = abilityStatus(mk({ state: "outcome_unknown", outcome: "outcome_unknown", created_post_id: 9 }));
    expect(s.kind).toBe("unknown_outcome");
    expect(s.draftMayExist).toBe(true);
  });
  it("outcome_unknown without a post id keeps the checking text", () => {
    const s = abilityStatus(mk({ state: "outcome_unknown" }));
    expect(s.text).toContain("is checking");
    expect(s.draftMayExist).toBeUndefined();
  });
});

describe("AbilityRequestCard on a failed row with a post id", () => {
  const props = { now: new Date("2026-10-01T10:30:00Z"), onApprove: vi.fn(), onDecline: vi.fn(), onUndo: vi.fn() };

  it("shows the edit link when the site address is known and never prints the code", () => {
    render(
      <AbilityRequestCard
        {...props}
        siteUrl="https://shop.example"
        request={mk({ outcome: "failed", outcome_code: "interrupted", created_post_id: 7 })}
      />,
    );
    const a = screen.getByRole("link", { name: /edit the draft/i });
    expect(a.getAttribute("href")).toBe("https://shop.example/wp-admin/post.php?post=7&action=edit");
    expect(screen.queryByText(/interrupted/)).toBeNull();
  });

  it("shows no link when the site address is unknown", () => {
    render(<AbilityRequestCard {...props} request={mk({ outcome: "failed", created_post_id: 7 })} />);
    expect(screen.queryByRole("link")).toBeNull();
    expect(screen.getByText(/may exist/)).toBeTruthy();
  });

  it("shows no link without a post id", () => {
    render(<AbilityRequestCard {...props} siteUrl="https://shop.example" request={mk({ outcome: "failed" })} />);
    expect(screen.queryByRole("link")).toBeNull();
  });
});

describe("abilityStatus plain-English refusals", () => {
  const refused = (code: string) => abilityStatus(mk({ outcome: "refused", outcome_code: code })).text;
  const generic = "The site refused to create the draft page. Nothing was created.";
  const groups: Array<[string[], string]> = [
    [
      ["content_editing_not_enabled", "principal_capabilities_drifted", "principal_missing"],
      "Turn AI page creation on again from this tab.",
    ],
    [["editor_unavailable"], "This site's editor isn't available."],
    [["agent_outdated"], "Update the WPMgr plugin on this site."],
    [["preview_changed", "entry_approval_invalid"], "The site changed since you approved. Ask the AI to try again."],
    [["created_post_touched", "conflict"], "Someone edited the draft, so it was kept."],
  ];
  for (const [codes, advice] of groups) {
    for (const code of codes) {
      it(`${code} shows its advice and never the code`, () => {
        const t = refused(code);
        expect(t).toContain(advice);
        expect(t).not.toContain(code);
        expect(t).toContain("Nothing was created");
      });
    }
  }
  it("a failed row with a refusal code also gets the advice", () => {
    expect(abilityStatus(mk({ outcome: "failed", outcome_code: "editor_unavailable" })).text).toContain(
      "editor isn't available",
    );
  });
  it("an unknown code falls back to the generic text without printing it", () => {
    expect(refused("something_new")).toBe(generic);
  });
  it("a prototype key is not a code", () => {
    expect(refused("constructor")).toBe(generic);
  });
});
