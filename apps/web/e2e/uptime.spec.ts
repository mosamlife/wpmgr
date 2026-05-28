import { test, expect, type Page } from "@playwright/test";

// Hermetic tests for the M5 uptime-monitoring UX. All backend endpoints are
// route-mocked so the suite runs without a live control plane (no ClickHouse,
// no probe worker). Covers: the site-detail uptime section rendering uptime % +
// status + chart from a mocked /uptime response, the 7d/30d/90d window toggle
// refetching with the new window, and the tenant alert-config form saving via a
// mocked PUT.

const ME = {
  user: {
    id: "33333333-3333-3333-3333-333333333333",
    email: "admin@example.com",
    name: "Admin",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [
    {
      user_id: "33333333-3333-3333-3333-333333333333",
      tenant_id: "22222222-2222-2222-2222-222222222222",
      role: "owner",
    },
  ],
  active_tenant_id: "22222222-2222-2222-2222-222222222222",
};

const SITE = {
  id: "11111111-1111-1111-1111-111111111111",
  tenant_id: "22222222-2222-2222-2222-222222222222",
  url: "https://example.com",
  name: "Example Site",
  status: "active",
  wp_version: "6.7.1",
  php_version: "8.3",
  health_status: "healthy",
  server_info: "nginx/1.25",
  multisite: false,
  active_theme: "twentytwentyfour",
  tags: ["production"],
  enrolled: true,
  enrolled_at: "2026-05-20T00:00:00Z",
  last_seen_at: new Date(Date.now() - 2 * 60_000).toISOString(),
  components: { plugins: [], themes: [] },
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
};

function makeSeries(
  buckets: number,
): { bucket: string; checks: number; up_checks: number; avg_latency_ms: number }[] {
  const out: {
    bucket: string;
    checks: number;
    up_checks: number;
    avg_latency_ms: number;
  }[] = [];
  const base = Date.parse("2026-05-20T00:00:00Z");
  for (let i = 0; i < buckets; i += 1) {
    out.push({
      bucket: new Date(base + i * 3600_000).toISOString(),
      checks: 5,
      // one downtime bucket: only 3/5 of probes up at bucket 3.
      up_checks: i === 3 ? 3 : 5,
      avg_latency_ms: 120 + i * 5,
    });
  }
  return out;
}

function makeUptime(window: string) {
  const pctByWindow: Record<string, number> = {
    "7d": 99.81,
    "30d": 98.42,
    "90d": 97.03,
  };
  const buckets = window === "7d" ? 24 : 12;
  const series = makeSeries(buckets);
  return {
    site_id: SITE.id,
    window,
    uptime_pct: pctByWindow[window] ?? 99.81,
    avg_latency_ms: window === "30d" ? 142 : 137,
    checks: series.reduce((acc, p) => acc + p.checks, 0),
    up: true,
    last_check: new Date(Date.now() - 30_000).toISOString(),
    // TLS expiry inside the 14-day warning window.
    tls_expiry: new Date(Date.now() + 5 * 24 * 3600_000).toISOString(),
    series,
  };
}

async function mockApi(page: Page) {
  await page.route("**/auth/me", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(ME),
    }),
  );

  // Per-tenant current-status summary (sites list health column).
  await page.route("**/api/v1/uptime/summary", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
        items: [
          {
            site_id: SITE.id,
            up: true,
            http_status: 200,
            last_check: new Date(Date.now() - 30_000).toISOString(),
          },
        ],
      }),
    }),
  );

  // Site-specific uptime — registered before the generic site routes so it wins
  // precedence (Playwright resolves most-recently-registered first).
  await page.route(
    `**/api/v1/sites/${SITE.id}/uptime**`,
    async (route) => {
      const url = new URL(route.request().url());
      const window = url.searchParams.get("window") ?? "7d";
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(makeUptime(window)),
      });
    },
  );

  // Generic site routes (least specific precedence).
  await page.route(`**/api/v1/sites/${SITE.id}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(SITE),
    }),
  );
  await page.route("**/api/v1/sites*", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: [SITE] }),
    }),
  );

  // Other site-detail sections fetch these; keep them empty so the page renders.
  await page.route(`**/api/v1/sites/${SITE.id}/backups`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: [] }),
    }),
  );
  await page.route(`**/api/v1/sites/${SITE.id}/backup-schedule`, (route) =>
    route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({ code: "not_found", message: "none" }),
    }),
  );
}

test("site detail shows uptime status, percentage and chart", async ({
  page,
}) => {
  await mockApi(page);

  await page.goto(`/sites/${SITE.id}`);
  await expect(
    page.getByRole("heading", { name: "Example Site", level: 1 }),
  ).toBeVisible();

  // Uptime section (card title + its window toggle).
  await expect(
    page.getByRole("group", { name: "Uptime window" }),
  ).toBeVisible();

  // Uptime % + avg latency + current status from the mocked 7d response.
  await expect(page.getByText("99.81%")).toBeVisible();
  await expect(page.getByText("137 ms")).toBeVisible();
  await expect(
    page.getByRole("group", { name: "Uptime window" }).getByRole("button"),
  ).toHaveCount(3);
  await expect(page.getByLabel("Status: Up")).toBeVisible();

  // TLS expiry warning (cert expires within 14 days).
  await expect(page.getByText(/renew soon/)).toBeVisible();

  // Accessible chart present.
  await expect(page.getByTestId("uptime-chart")).toBeVisible();
});

test("the 7d/30d/90d toggle refetches uptime for the chosen window", async ({
  page,
}) => {
  await mockApi(page);

  await page.goto(`/sites/${SITE.id}`);
  await expect(page.getByText("99.81%")).toBeVisible();

  // Switch to 30d -> a new request with window=30d returns a different value.
  await page
    .getByRole("group", { name: "Uptime window" })
    .getByRole("button", { name: "30d" })
    .click();

  await expect(page.getByText("98.42%")).toBeVisible();
  await expect(page.getByText("142 ms")).toBeVisible();

  // 90d.
  await page
    .getByRole("group", { name: "Uptime window" })
    .getByRole("button", { name: "90d" })
    .click();
  await expect(page.getByText("97.03%")).toBeVisible();
});

test("alert settings form saves recipients and webhook (PUT)", async ({
  page,
}) => {
  await mockApi(page);

  let saved: Record<string, unknown> | null = null;
  await page.route("**/api/v1/alert-config", async (route) => {
    if (route.request().method() === "PUT") {
      saved = route.request().postDataJSON() as Record<string, unknown>;
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          email_recipients: saved.email_recipients,
          webhook_url: saved.webhook_url,
          webhook_configured: Boolean(saved.webhook_url),
          enabled: true,
        }),
      });
      return;
    }
    // Initial GET: none configured yet.
    await route.fulfill({
      status: 404,
      contentType: "application/json",
      body: JSON.stringify({ code: "not_found", message: "none" }),
    });
  });

  await page.goto("/settings/alerts");
  await expect(
    page.getByRole("heading", { name: "Alert settings", level: 1 }),
  ).toBeVisible();
  await expect(page.getByText("Downtime alerts")).toBeVisible();

  await page.getByLabel("Email recipients").fill("ops@example.com");
  await page
    .getByLabel("Webhook URL (optional)")
    .fill("https://hooks.example.com/wpmgr");

  await page.getByRole("button", { name: "Save alert settings" }).click();

  await expect(page.getByText("Alert settings saved.")).toBeVisible();
  const body = saved as Record<string, unknown> | null;
  expect(body).not.toBeNull();
  expect(body?.email_recipients).toEqual(["ops@example.com"]);
  expect(body?.webhook_url).toBe("https://hooks.example.com/wpmgr");
});
