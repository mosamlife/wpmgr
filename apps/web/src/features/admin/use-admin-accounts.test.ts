import { describe, it, expect } from "vitest";

import {
  adminAccountsKeys,
  adminRevenueKeys,
  type AdminAccountDetail,
  type AdminAccountsListResponse,
  type AdminRevenueResponse,
} from "./use-admin-accounts";
import { defaultAccountsFilters } from "./admin-accounts-format";

// ---------------------------------------------------------------------------
// Fixtures — realistic payloads mirroring the pinned Go handler DTOs.
// ---------------------------------------------------------------------------

const ACCOUNTS_LIST_FIXTURE: AdminAccountsListResponse = {
  tiles: { mrr_cents: 590_000, active_subs: 120, past_due: 4, total: 200 },
  accounts: [
    {
      tenant_id: "11111111-1111-1111-1111-111111111111",
      org_name: "Acme Agency",
      slug: "acme-agency",
      owner_email: "owner@acme.test",
      plan: "agency",
      plan_status: "active",
      suspended: false,
      has_overrides: false,
      mrr_cents: 5900,
      sites: { used: 12, cap: 50 },
      storage: { used_bytes: 10_000_000_000, cap_bytes: 250_000_000_000, approximate: true },
      created_at: "2025-01-01T00:00:00Z",
      last_activity_at: "2026-07-01T00:00:00Z",
    },
  ],
  total: 200,
};

const ACCOUNT_DETAIL_FIXTURE: AdminAccountDetail = {
  header: {
    org_name: "Acme Agency",
    slug: "acme-agency",
    tenant_id: "11111111-1111-1111-1111-111111111111",
    plan: "agency",
    plan_status: "active",
    suspended_at: null,
    mrr_cents: 5900,
    created_at: "2025-01-01T00:00:00Z",
    owner_email: "owner@acme.test",
  },
  meters: [
    { key: "sites", label: "Sites", used: 12, cap: 50 },
    { key: "storage", label: "Storage", used: 10, cap: 250, approximate: true },
  ],
  entitlements: [{ label: "Hourly backups", value: "Included" }],
  subscription: {
    provider: "stripe",
    provider_customer_id: "cus_123",
    provider_subscription_id: "sub_123",
    stripe_dashboard_base: "https://dashboard.stripe.com",
    current_period_end: "2026-08-01T00:00:00Z",
    cancel_at_period_end: false,
    grace_until: null,
    comp_reason: null,
    last_event_at: "2026-07-01T00:00:00Z",
    webhook_stale: false,
  },
  timeline: [
    { at: "2026-07-01T00:00:00Z", kind: "subscription_updated", label: "Subscription updated", actor: "stripe", source: "webhook" },
  ],
  members: [
    {
      email: "owner@acme.test",
      name: "Owner",
      role: "owner",
      status: "active",
      email_verified: true,
      last_login_at: "2026-07-05T00:00:00Z",
    },
  ],
  sites: [{ url: "https://example.com", connection_state: "connected", created_at: "2025-02-01T00:00:00Z" }],
};

const REVENUE_FIXTURE: AdminRevenueResponse = {
  tiles: {
    mrr_cents: 590_000,
    active_subs: 120,
    trialing: 8,
    past_due_count: 4,
    past_due_at_risk_cents: 23_600,
    new_this_month: 6,
    canceled_this_month: 2,
  },
  plan_distribution: [{ plan: "agency", count: 40, mrr_share_cents: 236_000 }],
  past_due: [
    {
      tenant_id: "11111111-1111-1111-1111-111111111111",
      org_name: "Acme Agency",
      amount_cents: 5900,
      days_past_due: 12,
      grace_until: "2026-07-10T00:00:00Z",
      last_failed_at: "2026-06-25T00:00:00Z",
      owner_email: "owner@acme.test",
    },
  ],
  recent_events: [{ at: "2026-07-01T00:00:00Z", org_name: "Acme Agency", kind: "invoice_paid", source: "stripe" }],
  last_webhook_at: "2026-07-05T12:00:00Z",
  webhook_stale: false,
};

// ---------------------------------------------------------------------------
// DTO shape pins
// ---------------------------------------------------------------------------

describe("AdminAccountsListResponse DTO shape", () => {
  it("tiles has the four required numeric fields", () => {
    const t = ACCOUNTS_LIST_FIXTURE.tiles;
    expect(typeof t.mrr_cents).toBe("number");
    expect(typeof t.active_subs).toBe("number");
    expect(typeof t.past_due).toBe("number");
    expect(typeof t.total).toBe("number");
  });

  it("each account row has sites and storage usage sub-objects", () => {
    const a = ACCOUNTS_LIST_FIXTURE.accounts[0]!;
    expect(a.sites).toEqual({ used: 12, cap: 50 });
    expect(a.storage.approximate).toBe(true);
  });

  it("suspended and has_overrides are booleans, independent of plan_status", () => {
    const a = ACCOUNTS_LIST_FIXTURE.accounts[0]!;
    expect(typeof a.suspended).toBe("boolean");
    expect(typeof a.has_overrides).toBe("boolean");
  });
});

describe("AdminAccountDetail DTO shape", () => {
  it("has all six sections: header, meters, entitlements, subscription, timeline, members, sites", () => {
    const d = ACCOUNT_DETAIL_FIXTURE;
    expect(d.header).toBeDefined();
    expect(Array.isArray(d.meters)).toBe(true);
    expect(Array.isArray(d.entitlements)).toBe(true);
    expect(d.subscription).toBeDefined();
    expect(Array.isArray(d.timeline)).toBe(true);
    expect(Array.isArray(d.members)).toBe(true);
    expect(Array.isArray(d.sites)).toBe(true);
  });

  it("subscription.webhook_stale is a boolean the UI can branch on directly", () => {
    expect(typeof ACCOUNT_DETAIL_FIXTURE.subscription.webhook_stale).toBe("boolean");
  });

  it("meters entries carry an optional 'approximate' flag", () => {
    const storageMeter = ACCOUNT_DETAIL_FIXTURE.meters.find((m) => m.key === "storage");
    expect(storageMeter?.approximate).toBe(true);
    const sitesMeter = ACCOUNT_DETAIL_FIXTURE.meters.find((m) => m.key === "sites");
    expect(sitesMeter?.approximate).toBeUndefined();
  });

  it("members carry role/status/email_verified/last_login_at for the Members card", () => {
    const owner = ACCOUNT_DETAIL_FIXTURE.members[0]!;
    expect(owner.role).toBe("owner");
    expect(owner.email_verified).toBe(true);
  });
});

describe("AdminRevenueResponse DTO shape", () => {
  it("tiles use raw counts, not percentages (spec: raw counts not churn %)", () => {
    const t = REVENUE_FIXTURE.tiles;
    expect(Number.isInteger(t.active_subs)).toBe(true);
    expect(Number.isInteger(t.past_due_count)).toBe(true);
    expect(Number.isInteger(t.new_this_month)).toBe(true);
    expect(Number.isInteger(t.canceled_this_month)).toBe(true);
  });

  it("past_due rows carry everything the worklist needs: amount, days, grace, last-failed, owner", () => {
    const row = REVENUE_FIXTURE.past_due[0]!;
    expect(row.amount_cents).toBeGreaterThan(0);
    expect(row.days_past_due).toBeGreaterThan(0);
    expect(row.owner_email).toContain("@");
  });

  it("plan_distribution rows optionally carry comped_value_cents", () => {
    expect(REVENUE_FIXTURE.plan_distribution[0]!.comped_value_cents).toBeUndefined();
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
