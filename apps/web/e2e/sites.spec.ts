import { test, expect, type Page } from "@playwright/test";

// Hermetic smoke tests for the real session-based auth flow. We intercept the
// auth + sites endpoints with route mocks so the suite runs without a live
// backend. A tiny in-page flag flips `/auth/me` from 401 (logged out) to 200
// (logged in) once `/auth/login` succeeds.

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

const SITES = {
  items: [
    {
      id: "11111111-1111-1111-1111-111111111111",
      tenant_id: "22222222-2222-2222-2222-222222222222",
      url: "https://example.com",
      name: "Example Site",
      status: "active",
      wp_version: "6.7.1",
      php_version: "8.3",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-02T00:00:00Z",
    },
  ],
};

/** Wire mocks. `authed` controls whether /auth/me starts authenticated. */
async function mockApi(page: Page, opts: { authed: boolean }) {
  let authed = opts.authed;

  await page.route("**/api/auth/me", async (route) => {
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

  await page.route("**/api/auth/login", async (route) => {
    authed = true;
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(ME),
    });
  });

  await page.route("**/api/v1/sites*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(SITES),
    });
  });
}

test("logging in lands on the sites list", async ({ page }) => {
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
  await expect(
    page.getByRole("link", { name: "Example Site" }),
  ).toBeVisible();
  await expect(page.getByText("https://example.com")).toBeVisible();
});

test("unauthenticated visit to /sites redirects to /login", async ({ page }) => {
  await mockApi(page, { authed: false });

  await page.goto("/sites");

  await expect(
    page.getByRole("heading", { name: "Sign in to WPMgr" }),
  ).toBeVisible();
  await expect(page).toHaveURL(/\/login/);
});
