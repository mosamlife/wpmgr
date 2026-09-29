import { describe, it, expect } from "vitest";
import type { AssistantRequest } from "@wpmgr/api";

import {
  cardTitle,
  decidedByLine,
  ifApproveCopy,
  isActionable,
  notSentReasonText,
  requestStatusLine,
  setUpForLine,
  truncateAddress,
  waitingReasonText,
} from "./request-card-model";

// Pure-logic coverage for the card-state mapping (tracka-cache-purge-design-v7
// §2.6's "Card states" table). Each `it` below is aimed at ONE branch a
// mutation could silently flip -- a state, an outcome, a boolean the design
// calls out by name (origin_only_confirmed, decided_by_account_deleted) --
// rather than one broad "renders something" assertion that would survive
// most of those flips.

function baseRequest(overrides: Partial<AssistantRequest> = {}): AssistantRequest {
  return {
    id: "req-1",
    site_id: "site-1",
    scope: "url",
    url: "https://xn--bcher-kva.de/blog/spring-sale/",
    site_label: "Shop",
    site_host: "xn--bcher-kva.de",
    grant_label: "Priya's laptop",
    grant_via: "oauth",
    setup_client: "Claude Code",
    presented_digest: "digest-abc",
    state: "pending",
    created_at: "2026-09-29T09:41:00Z",
    expires_at: "2026-09-30T09:41:00Z",
    decided_at: null,
    decided_by_user_id: null,
    decided_by_name: null,
    decided_by_account_deleted: false,
    withdrawn_at: null,
    claimed_at: null,
    dispatch_attempts: 0,
    last_attempt_at: null,
    last_attempt_code: null,
    outcome: null,
    not_sent_reason: null,
    outcome_at: null,
    hosting_caches_cleared: null,
    hosting_caches_skipped: null,
    origin_only_confirmed: null,
    wpmgr_cdn: null,
    site_reported_text: null,
    ...overrides,
  };
}

describe("truncateAddress", () => {
  it("returns the value untouched at or under the cap", () => {
    const value = "/a".repeat(60); // 120 chars exactly
    expect(truncateAddress(value, 120)).toEqual({ display: value, truncated: false });
  });

  it("caps at exactly the boundary and appends the ellipsis one byte over it", () => {
    const value = "/a".repeat(60) + "x"; // 121 chars
    const { display, truncated } = truncateAddress(value, 120);
    expect(truncated).toBe(true);
    expect(display).toBe(value.slice(0, 120) + "…");
    expect(display.length).toBe(121); // 120 chars + the ellipsis glyph
  });

  it("never decodes percent-encoding — an encoded bidi override stays encoded", () => {
    const hostile = "/blog/%E2%80%AE-evil";
    const { display } = truncateAddress(hostile, 120);
    expect(display).toBe(hostile); // well under the cap, so untouched
    expect(display).not.toContain("‮");
    expect(display).toContain("%E2%80%AE");
  });
});

describe("cardTitle", () => {
  it("names the page for scope url", () => {
    expect(cardTitle(baseRequest({ scope: "url" }))).toBe("Clear one page on Shop");
  });

  it("names the whole cache for scope all", () => {
    expect(cardTitle(baseRequest({ scope: "all", url: null }))).toBe(
      "Clear the whole cache on Shop",
    );
  });
});

describe("setUpForLine", () => {
  it("prefers setup_client when the connection named one", () => {
    expect(setUpForLine(baseRequest({ setup_client: "Claude Code" })).primary).toBe(
      "Claude Code",
    );
  });

  it("falls back to grant_via when setup_client is null", () => {
    expect(setUpForLine(baseRequest({ setup_client: null, grant_via: "api_key" })).primary).toBe(
      "api_key",
    );
  });

  it("renders the dedicated browser-sign-in line and its caption, ignoring setup_client", () => {
    const line = setUpForLine(
      baseRequest({ grant_via: "browser_sign_in", setup_client: "should not show" }),
    );
    expect(line.primary).toBe("Set up by browser sign-in");
    expect(line.caption).toMatch(/identifies the connection, not who is asking/);
  });
});

describe("ifApproveCopy", () => {
  it("names the CDN removal only for a url-scoped clear", () => {
    expect(ifApproveCopy("url")).toMatch(/removes this address from your CDN/);
  });

  it("never mentions a CDN for a whole-site clear", () => {
    expect(ifApproveCopy("all")).not.toMatch(/CDN/);
  });
});

describe("decidedByLine", () => {
  it("says 'you' only when the viewer is the decider by id", () => {
    const req = baseRequest({ decided_by_user_id: "user-1", decided_at: "2026-09-29T09:43:00Z" });
    expect(decidedByLine(req, "Approved", "user-1").text).toMatch(/^Approved by you at/);
  });

  it("names another decider by their recorded name, not 'you'", () => {
    const req = baseRequest({
      decided_by_user_id: "user-2",
      decided_by_name: "Sam",
      decided_at: "2026-09-29T09:50:00Z",
    });
    expect(decidedByLine(req, "Declined", "user-1").text).toMatch(/^Declined by Sam at/);
  });

  it("uses the deleted-account line and drops the name, even for the viewer's own id", () => {
    const req = baseRequest({
      decided_by_user_id: "user-1",
      decided_by_account_deleted: true,
      decided_at: "2026-09-29T09:43:00Z",
    });
    const text = decidedByLine(req, "Approved", "user-1").text;
    expect(text).toContain("a person whose account was later deleted");
    expect(text).not.toContain("you");
  });
});

describe("waitingReasonText / notSentReasonText", () => {
  it("maps every last_attempt_code the wire enum names", () => {
    expect(waitingReasonText("site_unreachable")).toBe("site agent not connected");
    expect(waitingReasonText("site_hourly_cap")).toMatch(/limit of AI cache clears/);
    expect(waitingReasonText(null)).toBeNull();
  });

  it("maps every not_sent_reason the wire enum names", () => {
    expect(notSentReasonText("grant_inactive")).toBe("connection revoked or expired");
    expect(notSentReasonText("dispatch_deadline_passed")).toMatch(/within an hour/);
  });
});

describe("requestStatusLine — one branch per row of the §2.6 table", () => {
  it("pending", () => {
    expect(requestStatusLine(baseRequest({ state: "pending" }), null).kind).toBe("pending");
  });

  it("approved_undispatched with no attempt yet omits the detail line", () => {
    const line = requestStatusLine(
      baseRequest({
        state: "approved_undispatched",
        decided_by_user_id: "user-1",
        decided_at: "2026-09-29T09:43:00Z",
        last_attempt_at: null,
        last_attempt_code: null,
      }),
      "user-1",
    );
    expect(line.kind).toBe("approved_waiting");
    expect(line.text).toContain("Not started yet");
    expect(line.detail).toBeUndefined();
  });

  it("approved_undispatched with a transient attempt names the waiting reason", () => {
    const line = requestStatusLine(
      baseRequest({
        state: "approved_undispatched",
        decided_by_user_id: "user-1",
        decided_at: "2026-09-29T09:43:00Z",
        last_attempt_at: "2026-09-29T09:44:00Z",
        last_attempt_code: "site_busy",
      }),
      "user-1",
    );
    expect(line.detail).toMatch(/another AI cache clear on this site is still running/);
  });

  it("dispatched with no outcome yet is 'running', distinct from every done state", () => {
    const line = requestStatusLine(
      baseRequest({ state: "dispatched", outcome: null }),
      null,
    );
    expect(line.kind).toBe("running");
    expect(line.text).toBe("Clearing now.");
  });

  it("dispatched + purged lists cleared and skipped hosting caches", () => {
    const line = requestStatusLine(
      baseRequest({
        state: "dispatched",
        outcome: "purged",
        outcome_at: "2026-09-29T09:44:00Z",
        hosting_caches_cleared: ["wp_cloud"],
        hosting_caches_skipped: ["kinsta"],
        origin_only_confirmed: true,
      }),
      null,
    );
    expect(line.kind).toBe("done_purged");
    expect(line.text).toContain("Hosting caches cleared: wp_cloud.");
    expect(line.text).toContain("Skipped because WPMgr has not confirmed they clear only this site: kinsta.");
    expect(line.text).not.toMatch(/did not confirm that it skipped/);
  });

  it("dispatched + purged with origin_only_confirmed:false adds the extra sentence", () => {
    const line = requestStatusLine(
      baseRequest({
        state: "dispatched",
        outcome: "purged",
        outcome_at: "2026-09-29T09:44:00Z",
        hosting_caches_cleared: [],
        hosting_caches_skipped: [],
        origin_only_confirmed: false,
      }),
      null,
    );
    expect(line.text).toMatch(/did not confirm that it skipped hosting caches/);
  });

  it("dispatched + not_sent names the reason and says nothing ran", () => {
    const line = requestStatusLine(
      baseRequest({ state: "dispatched", outcome: "not_sent", not_sent_reason: "agent_outdated" }),
      null,
    );
    expect(line.kind).toBe("done_not_sent");
    expect(line.text).toBe("Nothing was sent: site agent too old. Nothing ran.");
  });

  it("dispatched + site_reported_failure / agent_failed / outcome_unknown are three distinct lines", () => {
    const failure = requestStatusLine(
      baseRequest({ state: "dispatched", outcome: "site_reported_failure" }),
      null,
    ).text;
    const agentFailed = requestStatusLine(
      baseRequest({ state: "dispatched", outcome: "agent_failed" }),
      null,
    ).text;
    const unknown = requestStatusLine(
      baseRequest({ state: "dispatched", outcome: "outcome_unknown" }),
      null,
    ).text;
    expect(new Set([failure, agentFailed, unknown]).size).toBe(3);
  });

  it("rejected reads 'Declined by <name>'", () => {
    const line = requestStatusLine(
      baseRequest({
        state: "rejected",
        decided_by_name: "Sam",
        decided_at: "2026-09-29T09:50:00Z",
      }),
      "someone-else",
    );
    expect(line.kind).toBe("declined");
    expect(line.text).toBe("Declined by Sam at " + new Date("2026-09-29T09:50:00Z").toLocaleTimeString([], { hour: "2-digit", minute: "2-digit" }) + ".");
  });

  it("withdrawn names the connection revoke, not a decision", () => {
    const line = requestStatusLine(
      baseRequest({ state: "withdrawn", withdrawn_at: "2026-09-29T10:00:00Z" }),
      null,
    );
    expect(line.kind).toBe("withdrawn");
    expect(line.text).toMatch(/its connection was revoked/);
  });

  it("expired closes unanswered", () => {
    const line = requestStatusLine(baseRequest({ state: "expired" }), null);
    expect(line.kind).toBe("expired");
    expect(line.text).toMatch(/Closed unanswered/);
  });
});

describe("isActionable", () => {
  it("is true only for a pending row", () => {
    expect(isActionable(baseRequest({ state: "pending" }))).toBe(true);
    for (const state of [
      "approved_undispatched",
      "dispatched",
      "rejected",
      "withdrawn",
      "expired",
    ] as const) {
      expect(isActionable(baseRequest({ state }))).toBe(false);
    }
  });
});
