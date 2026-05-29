import { test, expect, type Page } from "@playwright/test";

// Hermetic tests for the M3 bulk update wizard + live run detail. We route-mock
// the auth, sites, and updates endpoints so the suite runs without a backend.
//
// SSE is exercised via the polling FALLBACK path: we replace window.EventSource
// with a stub that immediately fires `onerror`, which makes the run-detail page
// flip to polling and refetch GET /updates/{id}. The detail mock advances task
// statuses across polls so we can assert live progress renders.

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
    ],
    themes: [],
  },
  created_at: "2026-01-01T00:00:00Z",
  updated_at: "2026-01-02T00:00:00Z",
};

const RUN_ID = "99999999-9999-9999-9999-999999999999";
const TASK_ID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";

function makeRun(status: string, taskStatus: string, toVersion?: string) {
  return {
    id: RUN_ID,
    tenant_id: SITE.tenant_id,
    created_by: ME.user.id,
    status,
    dry_run: true,
    created_at: "2026-05-27T12:00:00Z",
    updated_at: "2026-05-27T12:00:00Z",
    tasks: [
      {
        id: TASK_ID,
        run_id: RUN_ID,
        tenant_id: SITE.tenant_id,
        site_id: SITE.id,
        target_type: "core",
        target_slug: "core",
        from_version: "6.7.1",
        to_version: toVersion,
        status: taskStatus,
        created_at: "2026-05-27T12:00:00Z",
        updated_at: "2026-05-27T12:00:00Z",
      },
    ],
  };
}

async function mockApi(page: Page) {
  // Force the SSE -> polling fallback by stubbing EventSource to error.
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

  await page.route("**/api/v1/sites/pairing-codes", (route) =>
    route.fulfill({ status: 201, contentType: "application/json", body: "{}" }),
  );
  await page.route(`**/api/v1/sites/${SITE.id}`, (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SITE) }),
  );
  await page.route("**/api/v1/sites*", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify({ items: [SITE] }) }),
  );

  // POST /updates returns a created run (pending, task pending). Subsequent
  // GET /updates/{id} polls advance the task to succeeded.
  let detailPolls = 0;
  await page.route(`**/api/v1/updates/${RUN_ID}/events`, (route) =>
    route.fulfill({ status: 200, contentType: "text/event-stream", body: "" }),
  );
  await page.route(`**/api/v1/updates/${RUN_ID}`, async (route) => {
    detailPolls += 1;
    const body =
      detailPolls >= 2
        ? makeRun("completed", "succeeded", "6.7.2")
        : makeRun("running", "running");
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(body) });
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
        body: JSON.stringify({ items: [makeRun("completed", "succeeded", "6.7.2")] }),
      });
    }
  });
}

test("select a site, run the dry-run wizard, and see live task statuses", async ({
  page,
}) => {
  await mockApi(page);

  await page.goto("/sites");
  await expect(page.getByRole("heading", { name: "Sites", level: 1 })).toBeVisible();

  // Select the site via its row checkbox, then open the wizard.
  await page.getByRole("checkbox", { name: "Select Example Site" }).check();
  await page.getByRole("button", { name: /Update 1 selected/ }).click();

  // Wizard opens; dry run is on by default. Choose to update core.
  await expect(page.getByRole("heading", { name: "Update sites" })).toBeVisible();
  await page.getByLabel("WordPress core (to latest)").check();
  await expect(page.getByLabel(/Dry run/)).toBeChecked();

  // Submit -> POST /updates -> navigate to run detail.
  await page.getByRole("button", { name: /Preview 1 update/ }).click();

  await expect(page.getByRole("heading", { name: /^Run/ })).toBeVisible();
  await expect(page.getByText("Dry run")).toBeVisible();

  // A task row renders. Polling fallback advances it to succeeded with a
  // from->to version diff.
  await expect(page.getByTestId("update-task-row")).toBeVisible();
  await expect(page.getByText("Succeeded")).toBeVisible();
  await expect(page.getByText("6.7.2")).toBeVisible();
});

test("updates list shows recent runs with a link to detail", async ({ page }) => {
  await mockApi(page);

  await page.goto("/updates");
  await expect(
    page.getByRole("heading", { name: "Update runs", level: 1 }),
  ).toBeVisible();

  await expect(page.getByTestId("update-run-row")).toBeVisible();
  await page.getByRole("link", { name: /9999/ }).click();

  await expect(page.getByRole("heading", { name: /^Run/ })).toBeVisible();
  await expect(page.getByTestId("update-task-row")).toBeVisible();
});
