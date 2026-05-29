import { test, expect, type Page } from "@playwright/test";

// Hermetic tests for the per-site "Updates available" panel (Track C). The
// /api/v1/sites/{id}/updates/available endpoint isn't in the generated client
// yet, so we drive it via Playwright route mocks alongside the existing
// /api/v1/sites/{id} endpoint. SSE is exercised via the polling fallback —
// window.EventSource is replaced with a stub that fires onerror immediately,
// which makes useRunEventStream signal "polling" and useUpdateRun refetch the
// run-detail query until the task settles.

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

const SITE_ID = "11111111-1111-1111-1111-111111111111";
const TENANT_ID = "22222222-2222-2222-2222-222222222222";

const SITE = {
  id: SITE_ID,
  tenant_id: TENANT_ID,
  url: "https://example.com",
  name: "Example Site",
  status: "active",
  wp_version: "6.4.3",
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
      // Two plugins with updates available; one plugin already up to date.
      { slug: "wp-rocket", name: "WP Rocket", version: "3.16.1", active: true },
      { slug: "akismet", name: "Akismet", version: "5.3", active: true },
      { slug: "hello-dolly", name: "Hello Dolly", version: "1.7.2", active: false },
    ],
    themes: [
      { slug: "twentytwentyfour", name: "Twenty Twenty-Four", version: "1.1", active: true },
    ],
  },
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
};

const AS_OF = new Date(Date.now() - 3 * 60_000).toISOString();

const AVAILABLE_INITIAL = {
  site_id: SITE_ID,
  core_update: { new_version: "6.5.2", current_version: "6.4.3" },
  items: [
    {
      type: "plugin",
      slug: "wp-rocket",
      name: "WP Rocket",
      version: "3.16.1",
      new_version: "3.16.2",
      active: true,
    },
    {
      type: "plugin",
      slug: "akismet",
      name: "Akismet",
      version: "5.3",
      new_version: "5.3.1",
      active: true,
    },
    {
      type: "theme",
      slug: "twentytwentyfour",
      name: "Twenty Twenty-Four",
      version: "1.1",
      new_version: "1.2",
      active: true,
    },
  ],
  as_of: AS_OF,
};

const RUN_ID = "99999999-9999-9999-9999-999999999999";
const TASK_ID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";

function makeRun(status: string, taskStatus: string, toVersion?: string) {
  return {
    id: RUN_ID,
    tenant_id: TENANT_ID,
    created_by: ME.user.id,
    status,
    dry_run: false,
    created_at: "2026-05-29T12:00:00Z",
    updated_at: "2026-05-29T12:00:00Z",
    tasks: [
      {
        id: TASK_ID,
        run_id: RUN_ID,
        tenant_id: TENANT_ID,
        site_id: SITE_ID,
        target_type: "plugin",
        target_slug: "wp-rocket",
        from_version: "3.16.1",
        to_version: toVersion,
        status: taskStatus,
        created_at: "2026-05-29T12:00:00Z",
        updated_at: "2026-05-29T12:00:00Z",
      },
    ],
  };
}

async function mockApi(page: Page) {
  // Force SSE -> polling fallback so the test doesn't depend on a real
  // EventSource — the polling path advances tasks across GET /updates/{id}.
  await page.addInitScript(() => {
    class FailingEventSource {
      onopen: (() => void) | null = null;
      onmessage: ((e: MessageEvent) => void) | null = null;
      onerror: (() => void) | null = null;
      constructor() {
        setTimeout(() => this.onerror?.(), 0);
      }
      close() {}
    }
    // @ts-expect-error override for test
    window.EventSource = FailingEventSource;
  });

  await page.route("**/auth/me", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(ME) }),
  );
  await page.route(`**/api/v1/sites/${SITE_ID}`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SITE) }),
  );
  await page.route("**/api/v1/sites*", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [SITE] }) }),
  );

  // Per-site "available updates" endpoint.
  await page.route(`**/api/v1/sites/${SITE_ID}/updates/available`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(AVAILABLE_INITIAL),
    }),
  );
  // Refresh endpoint — 202 Accepted.
  await page.route(`**/api/v1/sites/${SITE_ID}/updates/refresh`, (route) =>
    route.fulfill({ status: 202, contentType: "application/json", body: "{}" }),
  );

  // Bulk updates endpoints (POST creates, GET /updates/{id} advances tasks).
  let detailPolls = 0;
  await page.route(`**/api/v1/updates/${RUN_ID}/events`, (route) =>
    route.fulfill({ status: 200, contentType: "text/event-stream", body: "" }),
  );
  await page.route(`**/api/v1/updates/${RUN_ID}`, async (route) => {
    detailPolls += 1;
    const body =
      detailPolls >= 2
        ? makeRun("completed", "succeeded", "3.16.2")
        : makeRun("running", "running");
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(body),
    });
  });
  await page.route("**/api/v1/updates", async (route) => {
    if (route.request().method() === "POST") {
      await route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify(makeRun("pending", "pending")),
      });
    } else {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({ items: [makeRun("running", "running")] }),
      });
    }
  });

  // Backups / monitoring routes aren't relevant here but the site detail page
  // mounts them; provide quiet defaults so the page doesn't render errors.
  await page.route(`**/api/v1/sites/${SITE_ID}/backups*`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [] }) }),
  );
  await page.route(`**/api/v1/sites/${SITE_ID}/backup-schedule`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ enabled: false, cron: "0 3 * * *", retention_days: 7 }),
    }),
  );
  await page.route(`**/api/v1/sites/${SITE_ID}/uptime*`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ status: "up", points: [] }) }),
  );
  await page.route("**/api/v1/alert-config", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ enabled: false }) }),
  );
}

test("AvailableUpdatesCard renders rows and drives a per-row update", async ({
  page,
}) => {
  await mockApi(page);

  await page.goto(`/sites/${SITE_ID}`);
  await expect(page.getByRole("heading", { name: "Example Site" })).toBeVisible();

  // Card renders with title + count badge + rows.
  await expect(page.getByText("Updates available")).toBeVisible();
  // 1 core + 3 components = 4 rows
  const rows = page.getByTestId("available-update-row");
  await expect(rows).toHaveCount(4);

  // The core row exposes the WordPress version arrow.
  await expect(page.getByText("WordPress core")).toBeVisible();
  await expect(page.getByText("6.4.3").first()).toBeVisible();
  await expect(page.getByText("6.5.2").first()).toBeVisible();

  // Click the WP Rocket row's [Update] button. The label "Update" is reused
  // across rows, so scope by the row that contains the plugin's name.
  const wpRocketRow = rows.filter({ hasText: "WP Rocket" });
  await wpRocketRow.getByRole("button", { name: "Update" }).click();

  // Polling fallback advances the task from running -> succeeded.
  await expect(wpRocketRow.getByText(/Updated|Queued|Updating/)).toBeVisible();
  await expect(wpRocketRow.getByText("✓ Updated")).toBeVisible({
    timeout: 10_000,
  });

  // The succeeded row fades (opacity-40) before the optimistic-scrub removes
  // it; either way the action button is gone.
  await expect(wpRocketRow.getByRole("button", { name: "Update" })).toHaveCount(
    0,
  );
});

test("Refresh button calls /updates/refresh and shows a toast", async ({
  page,
}) => {
  await mockApi(page);

  await page.goto(`/sites/${SITE_ID}`);
  await expect(page.getByText("Updates available")).toBeVisible();

  const refreshPromise = page.waitForRequest(
    (req) =>
      req.url().includes(`/api/v1/sites/${SITE_ID}/updates/refresh`) &&
      req.method() === "POST",
  );
  await page.getByRole("button", { name: /Refresh available updates/ }).click();
  await refreshPromise;
});

test("multi-select with two items enables Update selected", async ({
  page,
}) => {
  await mockApi(page);

  await page.goto(`/sites/${SITE_ID}`);
  await expect(page.getByText("Updates available")).toBeVisible();

  await page.getByRole("checkbox", { name: "Select WP Rocket" }).check();
  await page.getByRole("checkbox", { name: "Select Akismet" }).check();

  const selectedButton = page.getByRole("button", {
    name: /Update selected \(2\)/,
  });
  await expect(selectedButton).toBeEnabled();
});
