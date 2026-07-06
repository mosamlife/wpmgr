import { describe, it, expect } from "vitest";

import type {
  AdminAccountListItem,
  AdminAccountSubscription,
  AdminAccountTimelineEntry,
  AdminAccountUsage,
  AdminPastDueRow,
  AdminRevenueEvent,
} from "./use-admin-accounts";
import {
  ACCOUNT_STATUS_BADGE_CLASS,
  ACCOUNT_STATUS_FILTER_OPTIONS,
  ACCOUNT_STATUS_LABEL,
  DEFAULT_ACCOUNTS_LIMIT,
  DEFAULT_ACCOUNTS_SORT,
  accountDisplayStatus,
  activeAccountsFilterCount,
  buildAccountMeters,
  buildAccountsQuery,
  buildEntitlementRows,
  bytesToWholeGB,
  defaultAccountsFilters,
  formatAccountMrr,
  formatCents,
  formatMeterValue,
  isIdle90d,
  isOverCap,
  isWebhookStale,
  meterBarPercent,
  meterPercent,
  meterTone,
  sortEventsNewestFirst,
  sortPastDueOldestFirst,
  timelineActorLabel,
  timelineEntryLabel,
  timelineReason,
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
// Account display status — suspended_at (a timestamp, not a boolean) wins
// ---------------------------------------------------------------------------

describe("accountDisplayStatus", () => {
  it("maps active plan_status to 'active'", () => {
    expect(accountDisplayStatus({ plan_status: "active", suspended_at: undefined })).toBe(
      "active",
    );
  });

  it("maps trialing plan_status to 'trialing'", () => {
    expect(accountDisplayStatus({ plan_status: "trialing", suspended_at: null })).toBe(
      "trialing",
    );
  });

  it("maps past_due plan_status to 'past_due'", () => {
    expect(accountDisplayStatus({ plan_status: "past_due", suspended_at: null })).toBe(
      "past_due",
    );
  });

  it("maps canceled/paused/none plan_status to 'canceled' (muted)", () => {
    expect(accountDisplayStatus({ plan_status: "canceled", suspended_at: null })).toBe("canceled");
    expect(accountDisplayStatus({ plan_status: "paused", suspended_at: null })).toBe("canceled");
    expect(accountDisplayStatus({ plan_status: "none", suspended_at: null })).toBe("canceled");
  });

  it("maps comped plan_status to 'comped'", () => {
    expect(accountDisplayStatus({ plan_status: "comped", suspended_at: null })).toBe("comped");
  });

  it("a non-null suspended_at overrides active plan_status", () => {
    expect(
      accountDisplayStatus({ plan_status: "active", suspended_at: "2026-07-01T00:00:00Z" }),
    ).toBe("suspended");
  });

  it("a non-null suspended_at overrides comped plan_status (the most surprising combination)", () => {
    expect(
      accountDisplayStatus({ plan_status: "comped", suspended_at: "2026-07-01T00:00:00Z" }),
    ).toBe("suspended");
  });

  it("a non-null suspended_at overrides past_due plan_status", () => {
    expect(
      accountDisplayStatus({ plan_status: "past_due", suspended_at: "2026-07-01T00:00:00Z" }),
    ).toBe("suspended");
  });

  it("an absent (undefined) suspended_at does not suspend — the omitempty case", () => {
    expect(accountDisplayStatus({ plan_status: "active", suspended_at: undefined })).toBe(
      "active",
    );
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

describe("formatMeterValue", () => {
  it("formats a count meter as a plain locale number", () => {
    expect(formatMeterValue(1234, "count")).toBe("1,234");
  });

  it("formats a bytes meter via formatBytes", () => {
    expect(formatMeterValue(1_500_000, "bytes")).toBe("1.4 MB");
  });
});

// ---------------------------------------------------------------------------
// Idle detection
// ---------------------------------------------------------------------------

describe("isIdle90d", () => {
  const NOW = Date.parse("2026-07-06T00:00:00Z");

  it("is true when last_activity is undefined (omitempty — never active)", () => {
    expect(isIdle90d(undefined, NOW)).toBe(true);
  });

  it("is true when last_activity is null", () => {
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
// Account detail — usage meters + entitlement rows (derived from the flat
// `usage` block; there is no `meters`/`entitlements` array on the wire).
// ---------------------------------------------------------------------------

describe("buildAccountMeters", () => {
  it("builds a sites (count) row and a storage (bytes, always approximate) row", () => {
    const meters = buildAccountMeters({
      sites: { used: 12, cap: 50 },
      storage_bytes_approx: { used: 10_000_000_000, cap: 250_000_000_000 },
    });
    expect(meters).toEqual([
      { key: "sites", label: "Sites", used: 12, cap: 50, unit: "count" },
      {
        key: "storage",
        label: "Storage",
        used: 10_000_000_000,
        cap: 250_000_000_000,
        unit: "bytes",
        approximate: true,
      },
    ]);
  });

  it("handles a zero storage cap (free tier: no CP-managed cap, not zero capacity)", () => {
    const meters = buildAccountMeters({
      sites: { used: 0, cap: 3 },
      storage_bytes_approx: { used: 0, cap: 0 },
    });
    const storage = meters.find((m) => m.key === "storage")!;
    expect(storage.cap).toBe(0);
  });
});

describe("bytesToWholeGB", () => {
  it("converts a byte count to whole GB", () => {
    expect(bytesToWholeGB(250_000_000_000)).toBe(233);
  });

  it("converts 0 bytes to 0 GB", () => {
    expect(bytesToWholeGB(0)).toBe(0);
  });
});

describe("buildEntitlementRows", () => {
  it("formats floor seconds as a human duration and includes booleans as Included/empty", () => {
    const rows = buildEntitlementRows({
      probe_interval_floor_sec: 3600,
      backup_cadence_floor_seconds: 60,
      incremental_backups: true,
      client_portal: false,
    });
    expect(rows).toEqual([
      { label: "Uptime probe interval", value: "1h" },
      { label: "Backup cadence floor", value: "1m" },
      { label: "Incremental backups", value: "Included" },
      { label: "Client portal", value: "" },
    ]);
  });

  it("never throws on a zero-valued minimal entitlements block", () => {
    const rows = buildEntitlementRows({
      probe_interval_floor_sec: 0,
      backup_cadence_floor_seconds: 0,
      incremental_backups: false,
      client_portal: false,
    });
    expect(rows.every((r) => typeof r.value === "string")).toBe(true);
  });
});

// ---------------------------------------------------------------------------
// Timeline display derivation
// ---------------------------------------------------------------------------

describe("timelineEntryLabel", () => {
  it("title-cases an underscore-separated billing_events.kind", () => {
    expect(timelineEntryLabel("subscription_updated")).toBe("Subscription Updated");
  });

  it("title-cases a dotted audit_log.action", () => {
    expect(timelineEntryLabel("admin.billing.suspend")).toBe("Admin Billing Suspend");
  });

  it("falls back to the raw kind for an empty string", () => {
    expect(timelineEntryLabel("")).toBe("");
  });
});

describe("timelineActorLabel", () => {
  it("combines actor_type and actor_id when both are present", () => {
    expect(timelineActorLabel({ actor_type: "admin", actor_id: "u_123" })).toBe("admin u_123");
  });

  it("falls back to actor_type alone", () => {
    expect(timelineActorLabel({ actor_type: "stripe" })).toBe("stripe");
  });

  it("falls back to actor_id alone", () => {
    expect(timelineActorLabel({ actor_id: "u_123" })).toBe("u_123");
  });

  it("is null when neither is present (both omitempty and absent)", () => {
    expect(timelineActorLabel({})).toBeNull();
  });
});

describe("timelineReason", () => {
  it("pulls a string 'reason' key out of metadata", () => {
    expect(timelineReason({ metadata: { reason: "manual comp" } })).toBe("manual comp");
  });

  it("is undefined when metadata is absent", () => {
    expect(timelineReason({})).toBeUndefined();
  });

  it("is undefined when metadata has no 'reason' key", () => {
    expect(timelineReason({ metadata: { other: "value" } })).toBeUndefined();
  });

  it("is undefined when 'reason' is present but not a string (never throws)", () => {
    expect(timelineReason({ metadata: { reason: 42 } })).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// Revenue page — webhook staleness (derived client-side; RevenueResponse has
// no `webhook_stale` boolean on the wire, only the raw timestamp).
// ---------------------------------------------------------------------------

describe("isWebhookStale", () => {
  const NOW = Date.parse("2026-07-06T00:00:00Z");

  it("is true when the timestamp is missing (never received)", () => {
    expect(isWebhookStale(undefined, NOW)).toBe(true);
    expect(isWebhookStale(null, NOW)).toBe(true);
  });

  it("is false within the 25h freshness window", () => {
    const recent = new Date(NOW - 2 * 60 * 60 * 1000).toISOString();
    expect(isWebhookStale(recent, NOW)).toBe(false);
  });

  it("is true past the 25h threshold", () => {
    const old = new Date(NOW - 26 * 60 * 60 * 1000).toISOString();
    expect(isWebhookStale(old, NOW)).toBe(true);
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
    expect(params.has("idle_90d")).toBe(false);
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

  it("maps idle90d to the handler's idle_90d wire param", () => {
    const qs = buildAccountsQuery(filters({ idle90d: true }));
    expect(new URLSearchParams(qs).get("idle_90d")).toBe("true");
    expect(new URLSearchParams(qs).has("idle")).toBe(false);
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
    expect(params.get("idle_90d")).toBe("true");
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
  it("sorts by 'occurred_at' descending", () => {
    const rows = [
      { occurred_at: "2026-01-01T00:00:00Z", kind: "old" },
      { occurred_at: "2026-06-01T00:00:00Z", kind: "newest" },
      { occurred_at: "2026-03-01T00:00:00Z", kind: "middle" },
    ];
    expect(sortEventsNewestFirst(rows).map((r) => r.kind)).toEqual([
      "newest",
      "middle",
      "old",
    ]);
  });
});

// ---------------------------------------------------------------------------
// Regression guard for the prod crash class: every derivation helper above
// must run without throwing when fed a MINIMAL backend payload — only
// required fields present, every omitempty field genuinely absent (not even
// `null`, since Go's `json:",omitempty"` drops the key entirely for a zero
// value). This is exactly the shape that crashed /admin/accounts in prod.
// ---------------------------------------------------------------------------

describe("minimal (all-omitempty-absent) payload survival", () => {
  const MINIMAL_LIST_ITEM: AdminAccountListItem = {
    tenant_id: "11111111-1111-1111-1111-111111111111",
    org_name: "Acme",
    org_slug: "acme",
    // owner_email absent
    plan: "free",
    plan_status: "none",
    // suspended_at absent
    has_overrides: false,
    mrr_cents: 0,
    sites_used: 0,
    sites_cap: 1,
    storage_used_bytes_approx: 0,
    storage_cap_bytes: 0,
    near_limit: false,
    created_at: "2026-01-01T00:00:00Z",
    // last_activity absent
  };

  it("accountDisplayStatus + formatAccountMrr + isIdle90d never throw on a minimal list item", () => {
    expect(() => accountDisplayStatus(MINIMAL_LIST_ITEM)).not.toThrow();
    expect(() => formatAccountMrr(MINIMAL_LIST_ITEM)).not.toThrow();
    expect(() => isIdle90d(MINIMAL_LIST_ITEM.last_activity)).not.toThrow();
    expect(accountDisplayStatus(MINIMAL_LIST_ITEM)).toBe("canceled");
  });

  const MINIMAL_USAGE: AdminAccountUsage = {
    sites: { used: 0, cap: 1 },
    storage_bytes_approx: { used: 0, cap: 0 },
    seats_used: 1,
    restore_volume_bytes_approx: 0,
    entitlements: {
      probe_interval_floor_sec: 0,
      backup_cadence_floor_seconds: 0,
      incremental_backups: false,
      client_portal: false,
    },
  };

  it("buildAccountMeters + buildEntitlementRows never throw on a minimal usage block", () => {
    expect(() => buildAccountMeters(MINIMAL_USAGE)).not.toThrow();
    expect(() => buildEntitlementRows(MINIMAL_USAGE.entitlements)).not.toThrow();
  });

  const MINIMAL_SUBSCRIPTION: AdminAccountSubscription = {
    // provider, provider_customer_id, provider_subscription_id, dashboard_url,
    // current_period_end, grace_until, comp_reason, last_billing_event_at all
    // absent — a free/no-subscription account.
    cancel_at_period_end: false,
    stale: false,
  };

  it("a minimal subscription block's optional fields are all safely undefined (no throw when read)", () => {
    expect(() => {
      const renews = MINIMAL_SUBSCRIPTION.current_period_end
        ? new Date(MINIMAL_SUBSCRIPTION.current_period_end).toLocaleDateString()
        : undefined;
      return renews;
    }).not.toThrow();
  });

  const MINIMAL_TIMELINE_ENTRY: AdminAccountTimelineEntry = {
    source: "audit",
    occurred_at: "2026-01-01T00:00:00Z",
    kind: "account_created",
    // actor_type, actor_id, metadata all absent
  };

  it("timelineEntryLabel + timelineActorLabel + timelineReason never throw on a minimal timeline entry", () => {
    expect(() => timelineEntryLabel(MINIMAL_TIMELINE_ENTRY.kind)).not.toThrow();
    expect(() => timelineActorLabel(MINIMAL_TIMELINE_ENTRY)).not.toThrow();
    expect(() => timelineReason(MINIMAL_TIMELINE_ENTRY)).not.toThrow();
    expect(timelineActorLabel(MINIMAL_TIMELINE_ENTRY)).toBeNull();
    expect(timelineReason(MINIMAL_TIMELINE_ENTRY)).toBeUndefined();
  });

  const MINIMAL_PAST_DUE_ROW: AdminPastDueRow = {
    tenant_id: "22222222-2222-2222-2222-222222222222",
    org_name: "Beta",
    org_slug: "beta",
    // owner_email absent
    amount_cents: 1900,
    days_past_due: 5,
    // grace_until, last_payment_failed_at absent
  };

  it("sortPastDueOldestFirst never throws on a minimal past-due row", () => {
    expect(() => sortPastDueOldestFirst([MINIMAL_PAST_DUE_ROW])).not.toThrow();
  });

  const MINIMAL_REVENUE_EVENT: AdminRevenueEvent = {
    id: "33333333-3333-3333-3333-333333333333",
    occurred_at: "2026-01-01T00:00:00Z",
    // org_name, org_slug, tenant_id absent
    kind: "invoice_paid",
    provider: "stripe",
  };

  it("sortEventsNewestFirst never throws on a minimal revenue event (no org_name)", () => {
    expect(() => sortEventsNewestFirst([MINIMAL_REVENUE_EVENT])).not.toThrow();
  });

  it("isWebhookStale never throws when last_webhook_received_at is absent", () => {
    expect(() => isWebhookStale(undefined)).not.toThrow();
    expect(isWebhookStale(undefined)).toBe(true);
  });
});
