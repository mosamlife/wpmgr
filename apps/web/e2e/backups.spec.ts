import { test, expect, type Page } from "@playwright/test";

// Hermetic tests for the M4 backups / restore / schedule UX. All backend
// endpoints are route-mocked so the suite runs without a live control plane.
//
// Playwright resolves overlapping routes most-recently-registered first, so the
// backup-specific routes (.../backups, .../backup-schedule, /backups/{id}, and
// /backups/{id}/restore) are registered AFTER the generic site routes to win.

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

const SNAPSHOT_ID = "55555555-5555-5555-5555-555555555555";

function makeSnapshot(status: string, extra: Record<string, unknown> = {}) {
  return {
    id: SNAPSHOT_ID,
    tenant_id: SITE.tenant_id,
    site_id: SITE.id,
    kind: "full",
    status,
    total_size: 10_485_760,
    chunk_count: 42,
    created_at: "2026-05-27T12:00:00Z",
    updated_at: "2026-05-27T12:05:00Z",
    ...extra,
  };
}

const SNAPSHOT_DETAIL = {
  snapshot: makeSnapshot("completed", {
    finished_at: "2026-05-27T12:05:00Z",
    age_recipient: "age1examplepublicrecipient",
  }),
  entries: [
    {
      path: "wp-content/uploads/2026/05/logo.png",
      entry_kind: "file",
      size: 204_800,
      chunk_count: 1,
    },
    {
      path: "db/wp_posts",
      entry_kind: "db",
      table_name: "wp_posts",
      size: 1_048_576,
      chunk_count: 3,
    },
  ],
};

const SCHEDULE = {
  id: "66666666-6666-6666-6666-666666666666",
  tenant_id: SITE.tenant_id,
  site_id: SITE.id,
  cadence: "daily",
  kind: "full",
  enabled: true,
  retention_days: 30,
  monthly_archive_keep: 12,
  next_run_at: new Date(Date.now() + 3600_000).toISOString(),
  created_at: "2026-05-20T00:00:00Z",
  updated_at: "2026-05-20T00:00:00Z",
};

async function mockApi(page: Page) {
  await page.route("**/auth/me", (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(ME),
    }),
  );

  // Generic site routes first (least specific precedence).
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

  // --- Backup-specific routes (registered last => highest precedence) -------

  // List grows from empty -> one pending -> completed across calls so a
  // freshly triggered backup appears and advances.
  let listCalls = 0;
  let backupTriggered = false;
  await page.route(`**/api/v1/sites/${SITE.id}/backups`, async (route) => {
    if (route.request().method() === "POST") {
      backupTriggered = true;
      await route.fulfill({
        status: 201,
        contentType: "application/json",
        body: JSON.stringify(makeSnapshot("pending")),
      });
      return;
    }
    listCalls += 1;
    const items = !backupTriggered
      ? []
      : listCalls > 2
        ? [makeSnapshot("completed", { finished_at: "2026-05-27T12:05:00Z" })]
        : [makeSnapshot("running")];
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items }),
    });
  });

  let schedule = { ...SCHEDULE };
  await page.route(
    `**/api/v1/sites/${SITE.id}/backup-schedule`,
    async (route) => {
      if (route.request().method() === "PUT") {
        const body = route.request().postDataJSON() as Record<string, unknown>;
        schedule = { ...schedule, ...body };
        await route.fulfill({
          status: 200,
          contentType: "application/json",
          body: JSON.stringify(schedule),
        });
        return;
      }
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(schedule),
      });
    },
  );

  await page.route(
    `**/api/v1/backups/${SNAPSHOT_ID}/restore`,
    async (route) => {
      await route.fulfill({
        status: 202,
        contentType: "application/json",
        body: JSON.stringify(makeSnapshot("running")),
      });
    },
  );

  // SQL inspection report — now first-class page content (ADR-037 Batch 2),
  // not just inside the restore modal. Registered before the bare snapshot
  // route so its more-specific path wins precedence.
  await page.route(
    `**/api/v1/backups/${SNAPSHOT_ID}/sql-inspection`,
    (route) =>
      route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify({
          schema_version: 1,
          source: "agent",
          is_wordpress: true,
          charset: "utf8mb4",
          collation: "utf8mb4_unicode_ci",
          table_prefix: "wp_",
          siteurl: "https://example.com",
          home: "https://example.com",
          generated_at: "2026-05-27T12:05:00Z",
          tables: [
            { name: "wp_posts", rows_estimate: 128, bytes_estimate: 1_048_576, charset: "utf8mb4" },
            { name: "wp_options", rows_estimate: 340, bytes_estimate: 262_144, charset: "utf8mb4" },
          ],
        }),
      }),
  );

  // Environment fingerprint — older snapshots return 404 (not recorded); we
  // mock that calm path so the provenance card renders its muted note.
  await page.route(
    `**/api/v1/backups/${SNAPSHOT_ID}/environment`,
    (route) =>
      route.fulfill({
        status: 404,
        contentType: "application/json",
        body: JSON.stringify({ message: "not recorded" }),
      }),
  );

  await page.route(`**/api/v1/backups/${SNAPSHOT_ID}`, (route) =>
    route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(SNAPSHOT_DETAIL),
    }),
  );
}

test("trigger a backup on a site and see the snapshot appear with a status", async ({
  page,
}) => {
  await mockApi(page);

  await page.goto(`/sites/${SITE.id}`);
  await expect(
    page.getByRole("heading", { name: "Example Site", level: 1 }),
  ).toBeVisible();

  // Backups section + empty state.
  await expect(page.getByText("Backups", { exact: true })).toBeVisible();
  await expect(page.getByText(/No backups yet/)).toBeVisible();

  // Trigger a backup (kind defaults to full). The control now lives in the
  // card header as a "Run backup" verb-first action (ADR-037 Batch 2 flatten).
  await page.getByRole("button", { name: "Run backup" }).click();

  // The new snapshot row appears with a status badge (list polling advances it).
  const row = page.getByTestId("backup-row");
  await expect(row).toBeVisible();
  await expect(row.getByText("Full")).toBeVisible();
  await expect(row.getByText(/Running|Completed|Pending/)).toBeVisible();
});

test("open a snapshot and run a full restore", async ({ page }) => {
  await mockApi(page);

  await page.goto(`/backups/${SNAPSHOT_ID}`);
  await expect(
    page.getByRole("heading", { name: /^Snapshot/, level: 1 }),
  ).toBeVisible();

  // Contents card (SQL inspection + grouped manifest). The manifest groups are
  // collapsed by default; expand "Files" to reveal individual entries.
  await expect(page.getByText("Contents", { exact: true })).toBeVisible();
  await page
    .getByRole("button", { name: /^Files/ })
    .first()
    .click();
  await expect(page.getByTestId("manifest-entry-row").first()).toBeVisible();

  // The primary "Restore site" CTA in the page header opens the restore dialog.
  await page
    .getByRole("button", { name: "Restore site", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: "Restore from snapshot" }),
  ).toBeVisible();
  await expect(page.getByText(/destructive/i)).toBeVisible();

  // Full restore is selected by default. Clicking "Apply restore" opens the
  // destructive-confirm modal where the operator types the host (or snapshot
  // id prefix fallback) to enable the destructive button.
  await page
    .getByRole("button", { name: "Apply restore", exact: true })
    .click();
  await expect(
    page.getByRole("heading", { name: /Apply restore for .* from backup/ }),
  ).toBeVisible();

  const confirmBtn = page
    .getByRole("button", { name: "Apply restore", exact: true })
    .last();
  await expect(confirmBtn).toBeDisabled();

  // Type the site host (resolved from the snapshot's site_id) to confirm. The
  // page now passes the real host instead of the snapshot id fallback.
  const host = new URL(SITE.url).host;
  await page.getByLabel(/Type .* to confirm/).fill(host);
  await expect(confirmBtn).toBeEnabled();

  await confirmBtn.click();

  // Both dialogs close after the POST resolves.
  await expect(
    page.getByRole("heading", { name: "Restore from snapshot" }),
  ).toBeHidden();
});

test("set a backup schedule", async ({ page }) => {
  await mockApi(page);

  await page.goto(`/sites/${SITE.id}`);
  await expect(page.getByText("Backup schedule")).toBeVisible();

  // Adjust retention then save (PUT).
  const retention = page.getByLabel("Retention (days)");
  await expect(retention).toHaveValue("30");
  await retention.fill("45");

  await page.getByRole("button", { name: "Save schedule" }).click();
  await expect(page.getByText("Schedule saved.")).toBeVisible();
});
