import { test, expect, type Page } from "@playwright/test";

// Hermetic tests for the redesigned Health tab (ADR-037 Impeccable, Batch 1).
// The diagnostics endpoint is route-mocked so the suite runs without a live
// control plane. Covers: the header ribbon (Host / PHP+EOL / WP / Collected /
// as-of + single "Re-run all checks"), the honest empty summary stubs
// (Vulnerabilities "Not scanned yet", Performance "Not measured yet"), the
// titled diagnostics sections, and the 503 "unwired" refresh surfacing as a
// toast rather than inline text.

const SITE_ID = "11111111-1111-1111-1111-111111111111";
const TENANT_ID = "22222222-2222-2222-2222-222222222222";

const ME = {
  user: {
    id: "33333333-3333-3333-3333-333333333333",
    email: "admin@example.com",
    name: "Admin",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [
    { user_id: "33333333-3333-3333-3333-333333333333", tenant_id: TENANT_ID, role: "owner" },
  ],
  active_tenant_id: TENANT_ID,
};

const SITE = {
  id: SITE_ID,
  tenant_id: TENANT_ID,
  url: "https://example.com",
  name: "Example Site",
  status: "active",
  wp_version: "6.6.1",
  php_version: "8.1",
  health_status: "healthy",
  server_info: "nginx/1.25",
  multisite: false,
  active_theme: "twentytwentyfour",
  tags: ["production"],
  enrolled: true,
  enrolled_at: "2026-05-20T00:00:00Z",
  last_seen_at: new Date(Date.now() - 2 * 60_000).toISOString(),
  components: {
    plugins: [],
    themes: [],
    disk: {
      wp_content_bytes: 2_000_000_000,
      uploads_bytes: 1_500_000_000,
      free_bytes: 10_000_000_000,
    },
  },
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
};

const COLLECTED = new Date(Date.now() - 4 * 60_000).toISOString();

const DIAGNOSTICS = {
  items: [
    {
      category: "identity",
      fresh: true,
      collected_at: COLLECTED,
      payload: { wp_version: "6.6.1", site_as_of_hash: "a1b2c3d4e5f6a7b8c9d0" },
    },
    {
      category: "hosting",
      fresh: true,
      collected_at: COLLECTED,
      payload: { is_wpengine: true },
    },
    {
      category: "php",
      fresh: true,
      collected_at: COLLECTED,
      payload: { version: "8.1.27", sapi: "fpm-fcgi", memory_limit: "256M" },
    },
    {
      category: "mysql",
      fresh: true,
      collected_at: COLLECTED,
      payload: { version: "8.0.36", charset: "utf8mb4" },
    },
    {
      category: "plugins",
      fresh: true,
      collected_at: COLLECTED,
      payload: {
        installed_count: 24,
        active_count: 18,
        available_updates: 3,
        licensing: [],
      },
    },
    {
      category: "themes",
      fresh: true,
      collected_at: COLLECTED,
      payload: { active: { name: "Twenty Twenty Four" }, installed: 3 },
    },
    { category: "users", fresh: true, collected_at: COLLECTED, payload: { total: 5, admins: 2 } },
    {
      category: "http",
      fresh: true,
      collected_at: COLLECTED,
      payload: { home_url: "https://example.com", loopback: { ok: true, status: 200 } },
    },
    {
      category: "cron",
      fresh: true,
      collected_at: COLLECTED,
      payload: { disabled: false, event_count: 42 },
    },
    {
      category: "filesystem",
      fresh: true,
      collected_at: COLLECTED,
      payload: { free_bytes: 10_000_000_000, wp_content_writable: true },
    },
    {
      category: "security",
      fresh: true,
      collected_at: COLLECTED,
      payload: { defines: { WP_DEBUG: false }, salts_configured: true },
    },
    {
      category: "wp_native",
      fresh: true,
      collected_at: COLLECTED,
      payload: {
        "wp-paths-sizes": {
          directory_size_status: "ok",
          fields: {
            wordpress_size: { label: "WordPress", value: "60 MB", debug: 62_914_560 },
            themes_size: { label: "Themes", value: "20 MB", debug: 20_971_520 },
            plugins_size: { label: "Plugins", value: "120 MB", debug: 125_829_120 },
            uploads_size: { label: "Uploads", value: "1.4 GB", debug: 1_503_238_553 },
            database_size: { label: "Database", value: "200 MB", debug: 209_715_200 },
            total_size: { label: "Total", value: "1.8 GB", debug: 1_922_668_953 },
          },
        },
        "wp-filesystem": {
          fields: { wordpress: { label: "WordPress", value: "Writable" } },
        },
        "wp-constants": {
          fields: { WP_DEBUG: { label: "WP_DEBUG", value: "false" } },
        },
        "wp-media": {
          fields: { image_editor: { label: "Editor", value: "Imagick" } },
        },
      },
    },
  ],
};

async function mockApi(page: Page) {
  await page.route("**/auth/me", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(ME) }),
  );

  await page.route(`**/api/v1/sites/${SITE_ID}/diagnostics`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(DIAGNOSTICS),
    }),
  );

  await page.route(`**/api/v1/sites/${SITE_ID}`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SITE) }),
  );
  await page.route("**/api/v1/sites*", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: [SITE] }),
    }),
  );

  // Summary-band sources kept empty/quiet so the page renders.
  await page.route(`**/api/v1/sites/${SITE_ID}/backups`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [] }) }),
  );
  await page.route(`**/api/v1/sites/${SITE_ID}/uptime**`, (route) =>
    route.fulfill({ status: 404, contentType: "application/json", body: JSON.stringify({ code: "not_found" }) }),
  );
}

test("health ribbon shows host, php with eol, wp version, collected and as-of", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto(`/sites/${SITE_ID}/health`);

  const ribbon = page.getByRole("region", { name: "Health" });
  await expect(ribbon).toBeVisible();
  await expect(ribbon.getByText("WP Engine")).toBeVisible();
  await expect(ribbon.getByText("8.1.27")).toBeVisible();
  // PHP 8.1 is past EOL on the client calendar -> warning chip present.
  await expect(ribbon.getByText(/EOL/)).toBeVisible();
  await expect(ribbon.getByText("6.6.1")).toBeVisible();
  await expect(ribbon.getByText(/Updated/)).toBeVisible();
  // Exactly one "Re-run all checks" button, not 13 per-card buttons.
  await expect(page.getByRole("button", { name: "Re-run all checks" })).toHaveCount(1);
  await expect(page.getByRole("button", { name: "Re-run check" })).toHaveCount(0);
});

test("summary band renders honest empty stubs, not fabricated zeros", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto(`/sites/${SITE_ID}/health`);

  await expect(page.getByText("Not scanned yet")).toBeVisible();
  await expect(page.getByText("Not measured yet")).toBeVisible();
  // No fabricated "0 findings" claim.
  await expect(page.getByText("No findings")).toHaveCount(0);
});

test("diagnostics render in titled sections with the directory-size bar", async ({
  page,
}) => {
  await mockApi(page);
  await page.goto(`/sites/${SITE_ID}/health`);

  for (const label of ["Runtime", "Storage", "Content", "Delivery", "Configuration"]) {
    await expect(page.getByRole("heading", { name: label })).toBeVisible();
  }

  await expect(page.getByRole("heading", { name: "Directory Sizes" })).toBeVisible();
  // The proportional bar exposes an accessible label naming the segments.
  await expect(page.getByRole("img", { name: /WordPress 60(\.0)? MB/ })).toBeVisible();
  // The plugin-updates warning chip links to the Updates tab.
  await expect(page.getByRole("link", { name: /3 updates/ })).toBeVisible();
});

test("an unwired re-run surfaces as a toast, not inline text", async ({ page }) => {
  await mockApi(page);
  await page.route(`**/api/v1/sites/${SITE_ID}/diagnostics/refresh`, (route) =>
    route.fulfill({
      status: 503,
      contentType: "application/json",
      body: JSON.stringify({ code: "diagnostics_refresh_unwired", message: "not wired" }),
    }),
  );

  await page.goto(`/sites/${SITE_ID}/health`);
  await page.getByRole("button", { name: "Re-run all checks" }).click();

  await expect(page.getByText("Could not queue a re-run")).toBeVisible();
});
