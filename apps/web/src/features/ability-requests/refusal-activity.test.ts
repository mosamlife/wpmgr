import { describe, it, expect } from "vitest";

import { refusalActivity } from "./page-edit-model";

// The sentence for a page request WPMgr refused before it became a card (the
// grey rows of section 5.4 of the BF-E design, the owner's acceptance copy),
// read from the metadata of the refusal's audit row. The metadata is what the
// control plane writes: apps/api/internal/mcp/page_structure.go
// builderEditIneligibleRefusal (code, agent_code and, for a word of the form
// [a-z_]{1,40}, detail) and withBuilderEditTarget (ability, and post_id when
// the input holds a positive integer), page_edit_run.go pageEditInputRefusal
// (argument, code) and the precheck refusal (code), and
// internal/mcp/service.go recordToolDeniedWith (grant_name, refusal_reason).
// Every sentence is written out in full here on purpose.

const ELIGIBLE_SUFFIX = ". WPMgr refused.";

/** The metadata of a page edit refused for its input: what pageEditInputRefusal writes, plus the post and ability. */
const inputRefusal = (code: string) => ({
  ability: "wpmgr/page-edit",
  post_id: 418,
  argument: "input",
  code,
  grant_name: "Claude",
  refusal_reason: "invalid_arguments",
});

/** The metadata of a page edit the site's precheck refused: {"code": refusal.Code}. */
const precheckRefusal = (code: string) => ({
  ability: "wpmgr/page-edit",
  post_id: 418,
  code,
  grant_name: "Claude",
  refusal_reason: "ability_precheck_refused",
});

/** The metadata of an ineligible page: builderEditIneligibleRefusal, then withBuilderEditTarget. */
const ineligible = (ability: string, code: string, detail?: string) => ({
  ability,
  post_id: 418,
  code,
  agent_code: code,
  ...(detail === undefined ? {} : { detail }),
  grant_name: "Claude",
  refusal_reason: "ability_agent_refused",
});

describe("a page edit refused before it was shown", () => {
  it.each<[string, Record<string, unknown>, string]>([
    ["a part of the page that is not there", precheckRefusal("node_not_found"), "it named a part of the page that is not there"],
    [
      "something WPMgr does not edit",
      precheckRefusal("node_not_editable"),
      "it asked to change something WPMgr does not edit (a form, a slider, an add-on widget)",
    ],
    [
      "a request Elementor's version does not support",
      precheckRefusal("op_not_supported_by_builder"),
      "Elementor's version of this request is not supported",
    ],
    ["a change that was too large", precheckRefusal("page_too_large"), "the change was too large"],
    ["changes that did not fit together", inputRefusal("ops_invalid"), "its changes did not fit together"],
    [
      "a request that did not follow the input rules (the row names the argument and no code)",
      { ability: "wpmgr/page-edit", post_id: 418, argument: "input", refusal_reason: "invalid_arguments" },
      "the request did not follow the rules for changing a page",
    ],
  ])("names the page and says, for %s, why", (_name, meta, reason) => {
    const got = refusalActivity(meta);
    expect(got).toEqual({
      kind: "refused",
      text: `The AI asked to change "#418" and WPMgr refused the request before showing it to you: ${reason}.`,
    });
  });

  it("says a page, not a number, when the request named no page", () => {
    const { post_id: _dropped, ...meta } = inputRefusal("ops_invalid");
    void _dropped;
    expect(refusalActivity(meta)?.text).toBe(
      "The AI asked to change a page and WPMgr refused the request before showing it to you: its changes did not fit together.",
    );
  });

  it.each<[string, unknown]>([
    ["zero", 0],
    ["negative", -3],
    ["a fraction", 4.5],
    ["text", "418"],
    ["not a number", Number.NaN],
    ["past what a number holds exactly", 2 ** 60],
    ["null", null],
  ])("takes a post id that is %s for no page", (_name, id) => {
    expect(refusalActivity({ ...inputRefusal("ops_invalid"), post_id: id })?.text).toBe(
      "The AI asked to change a page and WPMgr refused the request before showing it to you: its changes did not fit together.",
    );
  });

  it("does not put a code it has no words for on screen", () => {
    expect(refusalActivity(precheckRefusal("something_new"))).toBeNull();
    expect(refusalActivity(precheckRefusal("<b>x</b>"))).toBeNull();
    expect(refusalActivity({ ability: "wpmgr/page-edit", post_id: 418 })).toBeNull();
  });
});

describe("a page edit refused because the page is not a draft WPMgr created", () => {
  it.each<[string, string | undefined, string]>([
    ["a draft a person made", "no_marker", " (a person's draft)"],
    ["a draft with a marker that names no request", "marker_not_a_request", " (a person's draft)"],
    ["a draft whose creation the site has no record of", "ledger_missing", " (a person's draft)"],
    ["a copy of a draft WPMgr made", "ledger_other_post", " (a person's draft)"],
    ["a draft whose creation did not finish", "ledger_not_completed", " (a draft WPMgr has not finished creating)"],
    ["a page that is gone", "missing", " (it no longer exists)"],
    ["a page that is gone, found by the edit's own check", "post_missing", " (it no longer exists)"],
    ["a draft in the trash", "trashed", " (it is in the trash)"],
    ["a page that was published", "not_draft", " (it is no longer a draft, for example because it was published)"],
    ["a page that was not built with Elementor", "not_builder_page", " (it was not built with Elementor)"],
    ["a page the site could not read", "unreadable", " (WPMgr could not read it)"],
    ["a reason the screen has no words for", "not_in_signed_list", ""],
    ["no reason at all", undefined, ""],
  ])("says, for %s, what it was", (_name, detail, suffix) => {
    const got = refusalActivity(ineligible("wpmgr/page-edit", "target_not_eligible", detail));
    expect(got).toEqual({
      kind: "not_eligible",
      text: `The AI asked to change "#418", which is not a draft WPMgr created for it${suffix}${ELIGIBLE_SUFFIX}`,
    });
  });

  it("never shows a reason word it was not given a phrase for", () => {
    for (const detail of ["<b>bold</b>", "constructor", "__proto__", "hasOwnProperty", "toString", "", "NOT_DRAFT"]) {
      expect(refusalActivity(ineligible("wpmgr/page-edit", "target_not_eligible", detail))?.text).toBe(
        'The AI asked to change "#418", which is not a draft WPMgr created for it. WPMgr refused.',
      );
    }
  });

  it("has nothing to say without the page's number", () => {
    const { post_id: _dropped, ...meta } = ineligible("wpmgr/page-edit", "target_not_eligible", "not_draft");
    void _dropped;
    expect(refusalActivity(meta)).toBeNull();
  });
});

describe("a page structure read refused because the page may not be read", () => {
  it.each<[string, string | undefined, string]>([
    ["a password-protected page", "password", " (it is password-protected)"],
    ["a private or scheduled page", "not_published", " (it is private, scheduled or otherwise not published)"],
    ["a page that is gone", "missing", " (it no longer exists)"],
    ["a draft a person made", "no_marker", " (a person's draft)"],
    ["a reason the screen has no words for", "post_type", ""],
    ["no reason at all", undefined, ""],
  ])("says the AI asked to read, not to change, for %s", (_name, detail, suffix) => {
    const got = refusalActivity(ineligible("wpmgr/page-structure", "post_not_readable", detail));
    expect(got).toEqual({
      kind: "not_eligible",
      text: `The AI asked to read "#418", which WPMgr does not let it read${suffix}${ELIGIBLE_SUFFIX}`,
    });
  });
});

describe("a refusal that is not one of these", () => {
  it.each<[string, Record<string, unknown> | null | undefined]>([
    ["no metadata", undefined],
    ["null metadata", null],
    ["empty metadata", {}],
    ["another ability's refusal", { ...ineligible("wpmgr/page-create", "target_not_eligible", "not_draft") }],
    ["a page edit with a read's code", ineligible("wpmgr/page-edit", "post_not_readable", "password")],
    ["a read with an edit's code", ineligible("wpmgr/page-structure", "target_not_eligible", "not_draft")],
    ["a read with an operation code", { ...precheckRefusal("node_not_found"), ability: "wpmgr/page-structure" }],
    ["a refusal with no ability", { post_id: 418, code: "target_not_eligible", detail: "not_draft" }],
    ["a capability the connection lacks", { grant_name: "Claude", refusal_reason: "capability_not_held" }],
  ])("is none: %s", (_name, meta) => {
    expect(refusalActivity(meta)).toBeNull();
  });
});
