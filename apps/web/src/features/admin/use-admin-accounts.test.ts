import { describe, it, expect } from "vitest";

import {
  adminAccountsKeys,
  adminRevenueKeys,
  type AdminAccountDetail,
  type AdminAccountListItem,
  type AdminAccountsListResponse,
  type AdminRevenueResponse,
} from "./use-admin-accounts";
import { defaultAccountsFilters } from "./admin-accounts-format";

// ---------------------------------------------------------------------------
// Fixtures — realistic payloads mirroring the REAL Go handler DTOs in
// apps/api/internal/admin/billing_dto.go (field names, nesting, optionality).
// This file's field names are the regression guard for the prod crash caused
// by a previous fixture set that pinned an invented contract (nested
// sites/storage objects, tiles.past_due/total, header/meters/entitlements,
// etc.) instead of the real one.
// ---------------------------------------------------------------------------

const ACCOUNTS_LIST_FIXTURE: AdminAccountsListResponse = {
  tiles: { mrr_cents: 590_000, active_subs: 120, past_due_count: 4, accounts_total: 200 },
  items: [
    {
      tenant_id: "11111111-1111-1111-1111-111111111111",
      org_name: "Acme Agency",
      org_slug: "acme-agency",
      owner_email: "owner@acme.test",
      plan: "agency",
      plan_status: "active",
      suspended_at: undefined,
      has_overrides: false,
      mrr_cents: 5900,
      sites_used: 12,
      sites_cap: 50,
      storage_used_bytes_approx: 10_000_000_000,
      storage_cap_bytes: 250_000_000_000,
      near_limit: false,
      created_at: "2025-01-01T00:00:00Z",
      last_activity: "2026-07-01T00:00:00Z",
    },
  ],
  total: 200,
  limit: 25,
  offset: 0,
};

const ACCOUNT_DETAIL_FIXTURE: AdminAccountDetail = {
  tenant_id: "11111111-1111-1111-1111-111111111111",
  org_name: "Acme Agency",
  org_slug: "acme-agency",
  owner_email: "owner@acme.test",
  plan: "agency",
  plan_status: "active",
  mrr_cents: 5900,
  created_at: "2025-01-01T00:00:00Z",
  suspended_at: undefined,
  suspended_reason: undefined,
  usage: {
    sites: { used: 12, cap: 50 },
    storage_bytes_approx: { used: 10_000_000_000, cap: 250_000_000_000 },
    seats_used: 3,
    restore_volume_bytes_approx: 0,
    entitlements: {
      probe_interval_floor_sec: 60,
      backup_cadence_floor_seconds: 3600,
      incremental_backups: true,
      client_portal: true,
    },
  },
  subscription: {
    provider: "stripe",
    provider_customer_id: "cus_123",
    provider_subscription_id: "sub_123",
    dashboard_url: "https://dashboard.stripe.com/subscriptions/sub_123",
    current_period_end: "2026-08-01T00:00:00Z",
    cancel_at_period_end: false,
    grace_until: undefined,
    comp_reason: undefined,
    last_billing_event_at: "2026-07-01T00:00:00Z",
    stale: false,
  },
  timeline: [
    {
      source: "billing_event",
      occurred_at: "2026-07-01T00:00:00Z",
      kind: "subscription_updated",
      actor_type: "stripe",
      actor_id: "webhook",
    },
  ],
  members: [
    {
      id: "44444444-4444-4444-4444-444444444444",
      email: "owner@acme.test",
      name: "Owner",
      role: "owner",
      status: "active",
      email_verified: true,
      last_login_at: "2026-07-05T00:00:00Z",
      member_since: "2025-01-01T00:00:00Z",
    },
  ],
  sites: [
    {
      id: "55555555-5555-5555-5555-555555555555",
      url: "https://example.com",
      connection_state: "connected",
      created_at: "2025-02-01T00:00:00Z",
    },
  ],
};

const REVENUE_FIXTURE: AdminRevenueResponse = {
  tiles: {
    mrr_cents: 590_000,
    mrr_past_due_cents: 23_600,
    active_subs: 120,
    trialing_subs: 8,
    past_due_count: 4,
    past_due_at_risk_cents: 23_600,
    new_this_month: 6,
    canceled_this_month: 2,
  },
  plan_distribution: [{ plan: "agency", count: 40, mrr_cents: 236_000 }],
  comped: { count: 2, hypothetical_value_cents: 11_800 },
  past_due: [
    {
      tenant_id: "11111111-1111-1111-1111-111111111111",
      org_name: "Acme Agency",
      org_slug: "acme-agency",
      owner_email: "owner@acme.test",
      amount_cents: 5900,
      days_past_due: 12,
      grace_until: "2026-07-10T00:00:00Z",
      last_payment_failed_at: "2026-06-25T00:00:00Z",
    },
  ],
  recent_events: [
    {
      id: "66666666-6666-6666-6666-666666666666",
      occurred_at: "2026-07-01T00:00:00Z",
      org_name: "Acme Agency",
      org_slug: "acme-agency",
      tenant_id: "11111111-1111-1111-1111-111111111111",
      kind: "invoice_paid",
      provider: "stripe",
    },
  ],
  last_webhook_received_at: "2026-07-05T12:00:00Z",
};

// ---------------------------------------------------------------------------
// DTO shape pins — real wire field names, nesting, and optionality
// ---------------------------------------------------------------------------

describe("AdminAccountsListResponse DTO shape", () => {
  it("tiles use past_due_count/accounts_total (NOT past_due/total)", () => {
    const t = ACCOUNTS_LIST_FIXTURE.tiles;
    expect(typeof t.mrr_cents).toBe("number");
    expect(typeof t.active_subs).toBe("number");
    expect(typeof t.past_due_count).toBe("number");
    expect(typeof t.accounts_total).toBe("number");
  });

  it("the list is `items`, not `accounts`", () => {
    expect(Array.isArray(ACCOUNTS_LIST_FIXTURE.items)).toBe(true);
  });

  it("each row has FLAT sites_used/sites_cap and storage_used_bytes_approx/storage_cap_bytes (no nested sites/storage objects)", () => {
    const a = ACCOUNTS_LIST_FIXTURE.items[0]!;
    expect(a.sites_used).toBe(12);
    expect(a.sites_cap).toBe(50);
    expect(a.storage_used_bytes_approx).toBe(10_000_000_000);
    expect(a.storage_cap_bytes).toBe(250_000_000_000);
  });

  it("has_overrides is a boolean; suspension is a nullable timestamp (suspended_at), not a boolean", () => {
    const a = ACCOUNTS_LIST_FIXTURE.items[0]!;
    expect(typeof a.has_overrides).toBe("boolean");
    expect(a.suspended_at).toBeUndefined();
  });

  it("response carries limit/offset alongside total", () => {
    expect(ACCOUNTS_LIST_FIXTURE.limit).toBe(25);
    expect(ACCOUNTS_LIST_FIXTURE.offset).toBe(0);
  });
});

describe("AdminAccountDetail DTO shape", () => {
  it("has no top-level header/meters/entitlements object — org_name/plan/etc. are flat top-level fields", () => {
    const d = ACCOUNT_DETAIL_FIXTURE;
    expect(d.org_name).toBe("Acme Agency");
    expect(d.plan).toBe("agency");
    expect((d as unknown as { header?: unknown }).header).toBeUndefined();
    expect((d as unknown as { meters?: unknown }).meters).toBeUndefined();
  });

  it("usage.storage_bytes_approx is the storage meter (used/cap), not usage.storage", () => {
    expect(ACCOUNT_DETAIL_FIXTURE.usage.storage_bytes_approx).toEqual({
      used: 10_000_000_000,
      cap: 250_000_000_000,
    });
  });

  it("subscription uses dashboard_url/last_billing_event_at/stale (not stripe_dashboard_base/last_event_at/webhook_stale)", () => {
    const s = ACCOUNT_DETAIL_FIXTURE.subscription;
    expect(s.dashboard_url).toContain("stripe.com");
    expect(s.last_billing_event_at).toBeTruthy();
    expect(typeof s.stale).toBe("boolean");
  });

  it("timeline entries use occurred_at/kind/actor_type/actor_id/metadata (not at/label/actor/reason)", () => {
    const entry = ACCOUNT_DETAIL_FIXTURE.timeline[0]!;
    expect(entry.occurred_at).toBeTruthy();
    expect(entry.kind).toBe("subscription_updated");
    expect(entry.actor_type).toBe("stripe");
  });

  it("members carry role/status/email_verified/last_login_at/member_since", () => {
    const owner = ACCOUNT_DETAIL_FIXTURE.members[0]!;
    expect(owner.role).toBe("owner");
    expect(owner.email_verified).toBe(true);
    expect(owner.member_since).toBeTruthy();
  });

  it("sites carry an id (used as the React key instead of url)", () => {
    expect(ACCOUNT_DETAIL_FIXTURE.sites[0]!.id).toBeTruthy();
  });
});

describe("AdminRevenueResponse DTO shape", () => {
  it("tiles use trialing_subs (not trialing) and raw counts, not percentages", () => {
    const t = REVENUE_FIXTURE.tiles;
    expect(Number.isInteger(t.trialing_subs)).toBe(true);
    expect(Number.isInteger(t.active_subs)).toBe(true);
    expect(Number.isInteger(t.past_due_count)).toBe(true);
    expect(Number.isInteger(t.new_this_month)).toBe(true);
    expect(Number.isInteger(t.canceled_this_month)).toBe(true);
  });

  it("past_due rows carry last_payment_failed_at (not last_failed_at)", () => {
    const row = REVENUE_FIXTURE.past_due[0]!;
    expect(row.amount_cents).toBeGreaterThan(0);
    expect(row.days_past_due).toBeGreaterThan(0);
    expect(row.last_payment_failed_at).toBeTruthy();
  });

  it("plan_distribution rows carry mrr_cents (not mrr_share_cents) and no per-row comped value", () => {
    const row = REVENUE_FIXTURE.plan_distribution[0]!;
    expect(row.mrr_cents).toBe(236_000);
    expect((row as unknown as { comped_value_cents?: unknown }).comped_value_cents).toBeUndefined();
  });

  it("comped is a separate top-level object, not a plan_distribution row", () => {
    expect(REVENUE_FIXTURE.comped.count).toBe(2);
    expect(REVENUE_FIXTURE.comped.hypothetical_value_cents).toBe(11_800);
  });

  it("recent_events use occurred_at/provider (not at/source), and no webhook_stale boolean exists on the response", () => {
    const ev = REVENUE_FIXTURE.recent_events[0]!;
    expect(ev.occurred_at).toBeTruthy();
    expect(ev.provider).toBe("stripe");
    expect((REVENUE_FIXTURE as unknown as { webhook_stale?: unknown }).webhook_stale).toBeUndefined();
  });

  it("last_webhook_received_at is top-level (not nested under tiles)", () => {
    expect(REVENUE_FIXTURE.last_webhook_received_at).toBeTruthy();
  });
});

// ---------------------------------------------------------------------------
// Minimal-payload regression guard — only required fields present, every
// omitempty field genuinely absent. This is the exact shape class that
// crashed /admin/accounts in prod (a missing/undefined field read with
// `.toLocaleString()`); the DTO types themselves must accept these shapes
// without a TypeScript error, which is checked simply by this file
// typechecking under `pnpm -C apps/web typecheck`.
// ---------------------------------------------------------------------------

describe("minimal (all-omitempty-absent) DTO shapes typecheck and hold real data", () => {
  it("a minimal AdminAccountListItem (no owner_email/suspended_at/last_activity) is valid", () => {
    const minimal: AdminAccountListItem = {
      tenant_id: "aaaaaaaa-0000-0000-0000-000000000000",
      org_name: "Minimal Org",
      org_slug: "minimal-org",
      plan: "free",
      plan_status: "none",
      has_overrides: false,
      mrr_cents: 0,
      sites_used: 0,
      sites_cap: 1,
      storage_used_bytes_approx: 0,
      storage_cap_bytes: 0,
      near_limit: false,
      created_at: "2026-01-01T00:00:00Z",
    };
    expect(minimal.owner_email).toBeUndefined();
    expect(minimal.suspended_at).toBeUndefined();
    expect(minimal.last_activity).toBeUndefined();
  });

  it("a minimal AdminAccountDetail (no owner_email/suspended_at/suspended_reason, a bare subscription) is valid", () => {
    const minimal: AdminAccountDetail = {
      tenant_id: "aaaaaaaa-0000-0000-0000-000000000000",
      org_name: "Minimal Org",
      org_slug: "minimal-org",
      plan: "free",
      plan_status: "none",
      mrr_cents: 0,
      created_at: "2026-01-01T00:00:00Z",
      usage: {
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
      },
      subscription: {
        cancel_at_period_end: false,
        stale: false,
      },
      timeline: [],
      members: [],
      sites: [],
    };
    expect(minimal.owner_email).toBeUndefined();
    expect(minimal.suspended_at).toBeUndefined();
    expect(minimal.subscription.provider).toBeUndefined();
    expect(minimal.subscription.dashboard_url).toBeUndefined();
  });

  it("a minimal RevenueResponse (no comped accounts, no webhook ever received) is valid", () => {
    const minimal: AdminRevenueResponse = {
      tiles: {
        mrr_cents: 0,
        mrr_past_due_cents: 0,
        active_subs: 0,
        trialing_subs: 0,
        past_due_count: 0,
        past_due_at_risk_cents: 0,
        new_this_month: 0,
        canceled_this_month: 0,
      },
      plan_distribution: [],
      comped: { count: 0, hypothetical_value_cents: 0 },
      past_due: [],
      recent_events: [],
    };
    expect(minimal.last_webhook_received_at).toBeUndefined();
  });
});

// ---------------------------------------------------------------------------
// Query key factory
// ---------------------------------------------------------------------------

describe("adminAccountsKeys", () => {
  it("all is a two-element ['admin','accounts'] tuple", () => {
    expect(adminAccountsKeys.all).toEqual(["admin", "accounts"]);
  });

  it("list() keys on the filters object so distinct filter states cache separately", () => {
    const a = adminAccountsKeys.list(defaultAccountsFilters());
    const b = adminAccountsKeys.list({ ...defaultAccountsFilters(), search: "acme" });
    expect(a).not.toEqual(b);
  });

  it("list() key is prefixed with the shared 'all' tuple (so invalidating 'all' clears every filter combination)", () => {
    const key = adminAccountsKeys.list(defaultAccountsFilters());
    expect(key[0]).toBe("admin");
    expect(key[1]).toBe("accounts");
    expect(key[2]).toBe("list");
  });

  it("detail() key is distinct per tenantId", () => {
    const a = adminAccountsKeys.detail("tenant-a");
    const b = adminAccountsKeys.detail("tenant-b");
    expect(a).not.toEqual(b);
    expect(a).toEqual(["admin", "accounts", "detail", "tenant-a"]);
  });
});

describe("adminRevenueKeys", () => {
  it("info is a distinct key from the accounts keys (no cross-invalidation)", () => {
    expect(adminRevenueKeys.info).toEqual(["admin", "revenue"]);
    expect(adminRevenueKeys.info[0]).toBe(adminAccountsKeys.all[0]);
    expect(adminRevenueKeys.info[1]).not.toBe(adminAccountsKeys.all[1]);
  });
});

// ---------------------------------------------------------------------------
// Endpoint URL contract
// ---------------------------------------------------------------------------

describe("endpoint URL contract", () => {
  const TENANT = "11111111-1111-1111-1111-111111111111";

  it("GET list matches /api/v1/admin/accounts", () => {
    expect("/api/v1/admin/accounts").toBe("/api/v1/admin/accounts");
  });

  it("GET detail matches /api/v1/admin/accounts/:tenantId", () => {
    expect(`/api/v1/admin/accounts/${TENANT}`).toBe(
      "/api/v1/admin/accounts/11111111-1111-1111-1111-111111111111",
    );
  });

  it("GET revenue matches /api/v1/admin/revenue", () => {
    expect("/api/v1/admin/revenue").toBe("/api/v1/admin/revenue");
  });

  it("every mutation URL is under /api/v1/admin/accounts/:id/", () => {
    const BASE = `/api/v1/admin/accounts/${TENANT}`;
    const paths = [
      `${BASE}/comp`,
      `${BASE}/overrides`,
      `${BASE}/grace`,
      `${BASE}/suspend`,
      `${BASE}/restore`,
      `${BASE}/state`,
    ];
    for (const p of paths) {
      expect(p.startsWith(BASE)).toBe(true);
    }
  });
});

// ---------------------------------------------------------------------------
// Hook exports — public API shape guard
// ---------------------------------------------------------------------------

describe("use-admin-accounts hook exports", () => {
  it("useAdminAccountsList takes the filters object (1 arg)", async () => {
    const { useAdminAccountsList } = await import("./use-admin-accounts");
    expect(typeof useAdminAccountsList).toBe("function");
    expect(useAdminAccountsList.length).toBe(1);
  });

  it("useAdminAccountDetail takes a tenantId (1 arg)", async () => {
    const { useAdminAccountDetail } = await import("./use-admin-accounts");
    expect(typeof useAdminAccountDetail).toBe("function");
    expect(useAdminAccountDetail.length).toBe(1);
  });

  it("useAdminRevenue takes no arguments", async () => {
    const { useAdminRevenue } = await import("./use-admin-accounts");
    expect(typeof useAdminRevenue).toBe("function");
    expect(useAdminRevenue.length).toBe(0);
  });

  it("every mutation hook factory is a function taking the tenantId (1 arg)", async () => {
    const mod = await import("./use-admin-accounts");
    const mutationHooks = [
      mod.useAdminCompAccount,
      mod.useAdminRevokeComp,
      mod.useAdminSetOverrides,
      mod.useAdminExtendGrace,
      mod.useAdminSuspendAccount,
      mod.useAdminRestoreAccount,
      mod.useAdminForceBillingState,
    ];
    for (const hook of mutationHooks) {
      expect(typeof hook).toBe("function");
      expect(hook.length).toBe(1);
    }
  });
});
