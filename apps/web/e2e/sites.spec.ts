import { test, expect } from "@playwright/test";

// Smoke test: load /login, sign in with the stub, assert /sites renders.
// The /api/v1/sites response is mocked so the test runs without a backend.
test("sign in with the stub and see the sites list", async ({ page }) => {
  await page.route("**/api/v1/sites*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({
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
      }),
    });
  });

  await page.goto("/login");
  await expect(
    page.getByRole("heading", { name: "Sign in to WPMgr" }),
  ).toBeVisible();

  await page.getByLabel("Email").fill("admin@example.com");
  await page.getByLabel("Password").fill("hunter2");
  await page.getByRole("button", { name: "Sign in" }).click();

  // Router navigates to /sites and the mocked row renders.
  await expect(
    page.getByRole("heading", { name: "Sites", level: 1 }),
  ).toBeVisible();
  await expect(
    page.getByRole("link", { name: "Example Site" }),
  ).toBeVisible();
  await expect(page.getByText("https://example.com")).toBeVisible();
});
