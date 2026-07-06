import { describe, it, expect } from "vitest";

import {
  ACCOUNT_STATUS_BADGE_CLASS,
  ACCOUNT_STATUS_FILTER_OPTIONS,
  ACCOUNT_STATUS_LABEL,
  DEFAULT_ACCOUNTS_LIMIT,
  DEFAULT_ACCOUNTS_SORT,
  accountDisplayStatus,
  activeAccountsFilterCount,
  buildAccountsQuery,
  defaultAccountsFilters,
  formatAccountMrr,
  formatCents,
  isIdle90d,
  isOverCap,
  meterBarPercent,
  meterPercent,
  meterTone,
  sortEventsNewestFirst,
  sortPastDueOldestFirst,
  type AdminAccountsFilters,
} from "./admin-accounts-format";

// ---------------------------------------------------------------------------
// Money
// ---------------------------------------------------------------------------

describe("formatCents", () => {
  it("formats whole dollars", () => {
    expect(formatCents(1500)).toBe("$15.00");
  });

  it("formats zero", () => {
    expect(formatCents(0)).toBe("$0.00");
  });

  it("formats fractional cents correctly", () => {
    expect(formatCents(5999)).toBe("$59.99");
  });
});

describe("formatAccountMrr", () => {
  it("shows the real dollar amount for an active paid account", () => {
    expect(
      formatAccountMrr({ mrr_cents: 5900, plan: "agency", plan_status: "active" }),
    ).toBe("$59.00");
  });

  it("shows '$0 comped' for a comped account regardless of stored mrr_cents", () => {
    expect(
      formatAccountMrr({ mrr_cents: 5900, plan: "agency", plan_status: "comped" }),
    ).toBe("$0 comped");
  });

  it("shows an em-dash for the free plan", () => {
    expect(
      formatAccountMrr({ mrr_cents: 0, plan: "free", plan_status: "none" }),
    ).toBe("–");
  });

  it("shows an em-dash for a canceled account even if mrr_cents is stale/nonzero", () => {
    expect(
      formatAccountMrr({ mrr_cents: 1500, plan: "starter", plan_status: "canceled" }),
    ).toBe("–");
  });
});

// ---------------------------------------------------------------------------
// Account display status — suspended overrides everything
// ---------------------------------------------------------------------------

describe("accountDisplayStatus", () => {
  it("maps active plan_status to 'active'", () => {
    expect(accountDisplayStatus({ plan_status: "active", suspended: false })).toBe("active");
  });

  it("maps trialing plan_status to 'trialing'", () => {
    expect(accountDisplayStatus({ plan_status: "trialing", suspended: false })).toBe("trialing");
  });

  it("maps past_due plan_status to 'past_due'", () => {
    expect(accountDisplayStatus({ plan_status: "past_due", suspended: false })).toBe("past_due");
  });

  it("maps canceled/paused/none plan_status to 'canceled' (muted)", () => {
    expect(accountDisplayStatus({ plan_status: "canceled", suspended: false })).toBe("canceled");
    expect(accountDisplayStatus({ plan_status: "paused", suspended: false })).toBe("canceled");
    expect(accountDisplayStatus({ plan_status: "none", suspended: false })).toBe("canceled");
  });

  it("maps comped plan_status to 'comped'", () => {
    expect(accountDisplayStatus({ plan_status: "comped", suspended: false })).toBe("comped");
  });

  it("suspended overrides active plan_status", () => {
    expect(accountDisplayStatus({ plan_status: "active", suspended: true })).toBe("suspended");
  });

  it("suspended overrides comped plan_status (the most surprising combination)", () => {
    expect(accountDisplayStatus({ plan_status: "comped", suspended: true })).toBe("suspended");
  });

  it("suspended overrides past_due plan_status", () => {
    expect(accountDisplayStatus({ plan_status: "past_due", suspended: true })).toBe("suspended");
  });

  it("every status has a label and a badge class (exhaustive UI coverage)", () => {
    for (const status of ACCOUNT_STATUS_FILTER_OPTIONS) {
      expect(ACCOUNT_STATUS_LABEL[status]).toBeTruthy();
      expect(ACCOUNT_STATUS_BADGE_CLASS[status]).toBeTruthy();
    }
  });
});

// ---------------------------------------------------------------------------
// Meters — used/cap coloring + over-cap rendering
// ---------------------------------------------------------------------------

describe("meterPercent", () => {
  it("computes the raw (unclamped) percent", () => {
    expect(meterPercent(5, 10)).toBe(50);
    expect(meterPercent(12, 10)).toBe(120);
  });

  it("returns 0 when cap is zero or negative (avoids divide-by-zero)", () => {
    expect(meterPercent(5, 0)).toBe(0);
    expect(meterPercent(5, -1)).toBe(0);
  });
});

describe("meterBarPercent", () => {
  it("clamps to 100 for a progress-bar-safe value even when over cap", () => {
    expect(meterBarPercent(150)).toBe(100);
  });

  it("clamps negative to 0", () => {
    expect(meterBarPercent(-10)).toBe(0);
  });

  it("rounds fractional percents", () => {
    expect(meterBarPercent(33.6)).toBe(34);
  });
});

describe("meterTone", () => {
  it("is 'ok' under 80%", () => {
    expect(meterTone(0)).toBe("ok");
    expect(meterTone(79.9)).toBe("ok");
  });

  it("is 'warning' at 80% and above, under 100%", () => {
    expect(meterTone(80)).toBe("warning");
    expect(meterTone(99.9)).toBe("warning");
  });

  it("is 'critical' at 100% and above", () => {
    expect(meterTone(100)).toBe("critical");
    expect(meterTone(150)).toBe("critical");
  });
});

describe("isOverCap", () => {
  it("is true only when usage strictly exceeds the cap", () => {
    expect(isOverCap(11, 10)).toBe(true);
    expect(isOverCap(10, 10)).toBe(false);
    expect(isOverCap(9, 10)).toBe(false);
  });

  it("is false when cap is zero or negative (no cap to exceed)", () => {
    expect(isOverCap(5, 0)).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Idle detection
// ---------------------------------------------------------------------------

describe("isIdle90d", () => {
  const NOW = Date.parse("2026-07-06T00:00:00Z");

  it("is true when last_activity_at is null (never active)", () => {
    expect(isIdle90d(null, NOW)).toBe(true);
  });

  it("is false for activity within the last 90 days", () => {
    const recent = new Date(NOW - 10 * 24 * 60 * 60 * 1000).toISOString();
    expect(isIdle90d(recent, NOW)).toBe(false);
  });

  it("is true for activity older than 90 days", () => {
    const old = new Date(NOW - 91 * 24 * 60 * 60 * 1000).toISOString();
    expect(isIdle90d(old, NOW)).toBe(true);
  });

  it("boundary: exactly 90 days is not yet idle (uses strictly-greater-than)", () => {
    const exact = new Date(NOW - 90 * 24 * 60 * 60 * 1000).toISOString();
    expect(isIdle90d(exact, NOW)).toBe(false);
  });
});

// ---------------------------------------------------------------------------
// Filters — URL/wire mapping
// ---------------------------------------------------------------------------

describe("defaultAccountsFilters", () => {
  it("defaults to the needs_attention sort and no active filters", () => {
    const filters = defaultAccountsFilters();
    expect(filters.sort).toBe(DEFAULT_ACCOUNTS_SORT);
    expect(filters.limit).toBe(DEFAULT_ACCOUNTS_LIMIT);
    expect(filters.offset).toBe(0);
    expect(activeAccountsFilterCount(filters)).toBe(0);
  });
});

describe("buildAccountsQuery", () => {
  function filters(overrides: Partial<AdminAccountsFilters>): AdminAccountsFilters {
    return { ...defaultAccountsFilters(), ...overrides };
  }

  it("omits search/status/plan/toggle params entirely when unset", () => {
    const qs = buildAccountsQuery(filters({}));
    const params = new URLSearchParams(qs);
    expect(params.has("search")).toBe(false);
    expect(params.has("status")).toBe(false);
    expect(params.has("plan")).toBe(false);
    expect(params.has("near_limit")).toBe(false);
    expect(params.has("has_overrides")).toBe(false);
    expect(params.has("comped")).toBe(false);
    expect(params.has("idle")).toBe(false);
  });

  it("always includes sort/limit/offset", () => {
    const qs = buildAccountsQuery(filters({}));
    const params = new URLSearchParams(qs);
    expect(params.get("sort")).toBe("needs_attention");
    expect(params.get("limit")).toBe(String(DEFAULT_ACCOUNTS_LIMIT));
    expect(params.get("offset")).toBe("0");
  });

  it("trims and sets search", () => {
    const qs = buildAccountsQuery(filters({ search: "  acme  " }));
    expect(new URLSearchParams(qs).get("search")).toBe("acme");
  });

  it("does not send a blank search after trimming", () => {
    const qs = buildAccountsQuery(filters({ search: "   " }));
    expect(new URLSearchParams(qs).has("search")).toBe(false);
  });

  it("joins multi-select status values with commas", () => {
    const qs = buildAccountsQuery(filters({ status: ["past_due", "suspended"] }));
    expect(new URLSearchParams(qs).get("status")).toBe("past_due,suspended");
  });

  it("joins multi-select plan values with commas", () => {
    const qs = buildAccountsQuery(filters({ plan: ["agency", "scale"] }));
    expect(new URLSearchParams(qs).get("plan")).toBe("agency,scale");
  });

  it("maps nearLimit to near_limit=true", () => {
    const qs = buildAccountsQuery(filters({ nearLimit: true }));
    expect(new URLSearchParams(qs).get("near_limit")).toBe("true");
  });

  it("maps hasOverrides to has_overrides=true", () => {
    const qs = buildAccountsQuery(filters({ hasOverrides: true }));
    expect(new URLSearchParams(qs).get("has_overrides")).toBe("true");
  });

  it("maps comped to comped=true", () => {
    const qs = buildAccountsQuery(filters({ comped: true }));
    expect(new URLSearchParams(qs).get("comped")).toBe("true");
  });

  it("maps idle90d to idle=true (wire param is 'idle', not 'idle90d')", () => {
    const qs = buildAccountsQuery(filters({ idle90d: true }));
    expect(new URLSearchParams(qs).get("idle")).toBe("true");
    expect(new URLSearchParams(qs).has("idle90d")).toBe(false);
  });

  it("carries a custom sort/limit/offset through unchanged", () => {
    const qs = buildAccountsQuery(filters({ sort: "mrr_desc", limit: 50, offset: 100 }));
    const params = new URLSearchParams(qs);
    expect(params.get("sort")).toBe("mrr_desc");
    expect(params.get("limit")).toBe("50");
    expect(params.get("offset")).toBe("100");
  });

  it("combines every axis in one query string", () => {
    const qs = buildAccountsQuery(
      filters({
        search: "acme",
        status: ["past_due"],
        plan: ["agency"],
        nearLimit: true,
        hasOverrides: true,
        comped: true,
        idle90d: true,
        sort: "created_desc",
        limit: 10,
        offset: 20,
      }),
    );
    const params = new URLSearchParams(qs);
    expect(params.get("search")).toBe("acme");
    expect(params.get("status")).toBe("past_due");
    expect(params.get("plan")).toBe("agency");
    expect(params.get("near_limit")).toBe("true");
    expect(params.get("has_overrides")).toBe("true");
    expect(params.get("comped")).toBe("true");
    expect(params.get("idle")).toBe("true");
    expect(params.get("sort")).toBe("created_desc");
    expect(params.get("limit")).toBe("10");
    expect(params.get("offset")).toBe("20");
  });
});

describe("activeAccountsFilterCount", () => {
  it("counts each active axis independently", () => {
    const filters: AdminAccountsFilters = {
      ...defaultAccountsFilters(),
      search: "acme",
      status: ["past_due"],
      plan: ["agency", "scale"],
      nearLimit: true,
    };
    // search(1) + status(1, not per-value) + plan(1, not per-value) + nearLimit(1) = 4
    expect(activeAccountsFilterCount(filters)).toBe(4);
  });

  it("is 0 for the default filters", () => {
    expect(activeAccountsFilterCount(defaultAccountsFilters())).toBe(0);
  });

  it("counts all six axes when everything is active", () => {
    const filters: AdminAccountsFilters = {
      search: "x",
      status: ["active"],
      plan: ["free"],
      nearLimit: true,
      hasOverrides: true,
      comped: true,
      idle90d: true,
      sort: DEFAULT_ACCOUNTS_SORT,
      limit: DEFAULT_ACCOUNTS_LIMIT,
      offset: 0,
    };
    expect(activeAccountsFilterCount(filters)).toBe(7);
  });
});

// ---------------------------------------------------------------------------
// Revenue page defensive ordering
// ---------------------------------------------------------------------------

describe("sortPastDueOldestFirst", () => {
  it("sorts by days_past_due descending (oldest first)", () => {
    const rows = [
      { tenant_id: "a", days_past_due: 3 },
      { tenant_id: "b", days_past_due: 30 },
      { tenant_id: "c", days_past_due: 10 },
    ];
    expect(sortPastDueOldestFirst(rows).map((r) => r.tenant_id)).toEqual(["b", "c", "a"]);
  });

  it("does not mutate the input array", () => {
    const rows = [{ tenant_id: "a", days_past_due: 1 }, { tenant_id: "b", days_past_due: 5 }];
    const original = [...rows];
    sortPastDueOldestFirst(rows);
    expect(rows).toEqual(original);
  });
});

describe("sortEventsNewestFirst", () => {
  it("sorts by 'at' descending", () => {
    const rows = [
      { at: "2026-01-01T00:00:00Z", kind: "old" },
      { at: "2026-06-01T00:00:00Z", kind: "newest" },
      { at: "2026-03-01T00:00:00Z", kind: "middle" },
    ];
    expect(sortEventsNewestFirst(rows).map((r) => r.kind)).toEqual([
      "newest",
      "middle",
      "old",
    ]);
  });
});
