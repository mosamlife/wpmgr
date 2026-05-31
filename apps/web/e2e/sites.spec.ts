import { test, expect, type Page } from "@playwright/test";

// Hermetic smoke tests for the real session-based auth flow plus M2 site
// enrollment + metadata UX. We intercept the auth + sites endpoints with route
// mocks so the suite runs without a live backend. A tiny in-page flag flips
// `/auth/me` from 401 (logged out) to 200 (logged in) once `/auth/login`
// succeeds.

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
  components: {
    plugins: [
      { slug: "akismet", name: "Akismet", version: "5.3", active: true },
      { slug: "woocommerce", name: "WooCommerce", version: "9.1", active: false },
    ],
    themes: [
      {
        slug: "twentytwentyfour",
        name: "Twenty Twenty-Four",
        version: "1.1",
        active: true,
      },
    ],
  },
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
};

const SITES = { items: [SITE] };

const PAIRING_CODE = {
  id: "44444444-4444-4444-4444-444444444444",
  tenant_id: "22222222-2222-2222-2222-222222222222",
  code: "WPMGR-PAIR-ABCD-1234",
  site_name: "New Site",
  tags: [],
  expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
  created_at: new Date().toISOString(),
};

/** Wire mocks. `authed` controls whether /auth/me starts authenticated. */
async function mockApi(page: Page, opts: { authed: boolean }) {
  let authed = opts.authed;

  await page.route("**/auth/me", async (route) => {
    if (authed) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(ME),
      });
    } else {
      await route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({ code: "unauthenticated", message: "Not authenticated" }),
      });
    }
  });

  await page.route("**/auth/login", async (route) => {
    authed = true;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(ME),
    });
  });

  // Pairing code (POST) is matched before the generic sites list route.
  await page.route("**/api/v1/sites/pairing-codes", async (route) => {
    await route.fulfill({
      status: 201,
      contentType: "application/json",
      body: JSON.stringify(PAIRING_CODE),
    });
  });

  // Single-site detail.
  await page.route(`**/api/v1/sites/${SITE.id}`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(SITE),
    });
  });

  // Sites list (and any other /sites* path) — registered last so the more
  // specific routes above take precedence.
  await page.route("**/api/v1/sites*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(SITES),
    });
  });
}

test("logging in lands on the sites list with health badges", async ({ page }) => {
  await mockApi(page, { authed: false });

  await page.goto("/login");
  await expect(
    page.getByRole("heading", { name: "Sign in to WPMgr" }),
  ).toBeVisible();

  await page.getByLabel("Email").fill("admin@example.com");
  await page.getByLabel("Password").fill("supersecret123");
  await page.getByRole("button", { name: "Sign in", exact: true }).click();

  await expect(
    page.getByRole("heading", { name: "Sites", level: 1 }),
  ).toBeVisible();
  await expect(page.getByRole("link", { name: "Example Site" })).toBeVisible();

  // Phase 5 — the connection-state badge renders in the row. A site that is
  // enrolled + healthy reads as "Connected".
  await expect(page.getByText("Connected").first()).toBeVisible();
});

test("Add site opens the live enrollment modal at the URL step", async ({
  page,
}) => {
  await mockApi(page, { authed: true });

  await page.goto("/sites");
  await expect(
    page.getByRole("heading", { name: "Sites", level: 1 }),
  ).toBeVisible();

  // Open the Add site modal (header action). Phase 5 replaced the
  // pairing-code modal with the live, site-first enrollment flow — step A
  // collects the URL.
  await page.getByRole("button", { name: "Add site" }).first().click();
  await expect(
    page.getByRole("heading", { name: "Add site", exact: true }),
  ).toBeVisible();
  await expect(page.getByLabel("Site URL")).toBeVisible();
  await expect(page.getByRole("button", { name: "Continue" })).toBeVisible();
});

test("site detail renders metadata and components", async ({ page }) => {
  await mockApi(page, { authed: true });

  await page.goto(`/sites/${SITE.id}`);

  await expect(
    page.getByRole("heading", { name: "Example Site", level: 1 }),
  ).toBeVisible();

  // Metadata section.
  await expect(page.getByText("6.7.1")).toBeVisible();
  await expect(page.getByText("8.3")).toBeVisible();
  await expect(page.getByText("twentytwentyfour")).toBeVisible();
  await expect(page.getByText("nginx/1.25")).toBeVisible();

  // Components table.
  await expect(page.getByText("Installed components")).toBeVisible();
  await expect(page.getByText("Akismet")).toBeVisible();
  await expect(page.getByText("WooCommerce")).toBeVisible();
  await expect(page.getByText("Twenty Twenty-Four")).toBeVisible();
});

test("unauthenticated visit to /sites redirects to /login", async ({ page }) => {
  await mockApi(page, { authed: false });

  await page.goto("/sites");

  await expect(
    page.getByRole("heading", { name: "Sign in to WPMgr" }),
  ).toBeVisible();
  await expect(page).toHaveURL(/\/login/);
});
