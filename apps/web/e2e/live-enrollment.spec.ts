import { test, expect, type Page } from "@playwright/test";

// Phase 5 — the live enrollment flow. This is the critical path the phase
// exists to fix: clicking Enroll in the agent must flip the Add-site modal
// from "awaiting" to "connected" over SSE, with NO manual refresh.
//
// We mock:
//   • auth (always authed)
//   • GET  /api/v1/sites          → list (one connected site)
//   • POST /api/v1/sites          → { site_id, enrollment_code, expires_at }
//   • GET  /api/v1/sites/events   → an SSE stream that, after a short delay,
//                                   pushes a `site.state_changed → connected`
//                                   event for the newly-created site.

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

const NEW_SITE_ID = "99999999-9999-9999-9999-999999999999";
const ENROLLMENT_CODE = "WPMGR-ENROLL-WXYZ-9876";

const CONNECTED_SITE = {
  id: NEW_SITE_ID,
  tenant_id: "22222222-2222-2222-2222-222222222222",
  url: "https://newsite.example.com",
  name: "New Site",
  status: "active",
  wp_version: "6.7.1",
  php_version: "8.3",
  health_status: "healthy",
  multisite: false,
  tags: [],
  enrolled: true,
  connection_state: "connected",
  connection_generation: 1,
  last_seen_at: new Date().toISOString(),
  components: {
    plugins: [
      { slug: "akismet", name: "Akismet", version: "5.3", active: true },
    ],
    themes: [],
  },
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
};

const EMPTY_SITES = { items: [] };

/** Build a single SSE frame for a `site.state_changed → connected` event. */
function connectedFrame(): string {
  const envelope = {
    id: "01HZZZZZZZZZZZZZZZZZZZZZZZZ",
    type: "site.state_changed",
    tenant_id: "22222222-2222-2222-2222-222222222222",
    site_id: NEW_SITE_ID,
    ts: new Date().toISOString(),
    data: {
      from: "pending_enrollment",
      to: "connected",
      site: CONNECTED_SITE,
    },
  };
  return `id: ${envelope.id}\nevent: site.state_changed\ndata: ${JSON.stringify(envelope)}\n\n`;
}

async function mockApi(page: Page) {
  await page.route("**/auth/me", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(ME),
    }),
  );

  // The SSE stream. We send an immediate keepalive comment so EventSource
  // reports `onopen`, then push the connected event after ~600ms — long enough
  // for the modal to render its "awaiting" state first.
  await page.route("**/api/v1/sites/events*", async (route) => {
    const body = `:\n\n` + connectedFrame();
    await route.fulfill({
      status: 200,
      headers: {
        "content-type": "text/event-stream",
        "cache-control": "no-cache",
        connection: "keep-alive",
      },
      body,
    });
  });

  // POST /api/v1/sites (site-first create) — must be registered before the
  // generic list route, and matched on method.
  await page.route("**/api/v1/sites", async (route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify({
          site_id: NEW_SITE_ID,
          enrollment_code: ENROLLMENT_CODE,
          expires_at: new Date(Date.now() + 15 * 60_000).toISOString(),
        }),
      });
      return;
    }
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(EMPTY_SITES),
    });
  });

  // Detail fetch for the connected site (success step may refresh).
  await page.route(`**/api/v1/sites/${NEW_SITE_ID}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(CONNECTED_SITE),
    }),
  );

  // Catch-all sites list with query params.
  await page.route("**/api/v1/sites?*", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(EMPTY_SITES),
    }),
  );
}

test("Add site walks URL → awaiting → connected live over SSE", async ({
  page,
}) => {
  await mockApi(page);

  await page.goto("/sites");
  await expect(
    page.getByRole("heading", { name: "Sites", level: 1 }),
  ).toBeVisible();

  // Open the modal at step A (Enter URL).
  await page.getByRole("button", { name: "Add site" }).first().click();
  const dialog = page.getByRole("dialog", { name: "Add site" });
  await expect(dialog).toBeVisible();

  // Step A — enter a URL and continue. Scope to the dialog: the empty-sites
  // onboarding surface also renders a "Site URL" field.
  await dialog.getByLabel("Site URL").fill("https://newsite.example.com");
  await dialog.getByRole("button", { name: "Continue" }).click();

  // Step B — the enrollment code + the "waiting for the agent" affordance.
  await expect(page.getByTestId("enrollment-code")).toHaveText(
    ENROLLMENT_CODE,
  );
  await expect(
    page.getByText("Waiting for the agent to enroll…"),
  ).toBeVisible();

  // Step C — the SSE `site.state_changed → connected` flips us to success with
  // NO manual refresh. This is the core assertion of the whole phase.
  await expect(page.getByText(/is connected/)).toBeVisible({ timeout: 10_000 });
  await expect(
    page.getByRole("button", { name: "Go to site" }),
  ).toBeVisible();
});

test("Add site validates the URL before continuing", async ({ page }) => {
  await mockApi(page);

  await page.goto("/sites");
  await page.getByRole("button", { name: "Add site" }).first().click();
  const dialog = page.getByRole("dialog", { name: "Add site" });
  await expect(dialog).toBeVisible();

  // Invalid URL keeps us on step A with an inline error.
  await dialog.getByLabel("Site URL").fill("not-a-url");
  await dialog.getByRole("button", { name: "Continue" }).click();

  await expect(dialog.getByRole("alert")).toContainText(/valid URL/i);
  // Still on step A — no enrollment code yet.
  await expect(page.getByTestId("enrollment-code")).toHaveCount(0);
});
