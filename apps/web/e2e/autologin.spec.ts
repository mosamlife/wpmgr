import { test, expect, type Page } from "@playwright/test";

// Hermetic Phase-5.5 (one-click login) smoke tests. The auth + sites mocks
// mirror sites.spec.ts; we additionally mock POST /autologin to:
//   * 200 — happy path: assert window.open is called with the redirect URL
//           (we DON'T follow the cross-origin URL; the WP site is faked).
//   * 403 — rbac_denied: assert the toast text.
//   * 429 — rate_limited: assert the retry-after text in the toast.

const ME_ADMIN = {
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
      role: "admin",
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

const FAKE_REDIRECT_URL =
  "https://example.com/?wpmgr_autologin=eyFAKE.JWT.value";

interface AutoLoginMock {
  status: number;
  body: object;
}

async function mockApi(
  page: Page,
  opts: {
    me?: object;
    authed?: boolean;
    autologin?: AutoLoginMock;
  } = {},
) {
  const me = opts.me ?? ME_ADMIN;
  const authed = opts.authed ?? true;

  await page.route("**/auth/me", async (route) => {
    if (authed) {
      await route.fulfill({
        status: 200,
        contentType: "application/json",
        body: JSON.stringify(me),
      });
    } else {
      await route.fulfill({
        status: 401,
        contentType: "application/json",
        body: JSON.stringify({ code: "unauthenticated", message: "Not authenticated" }),
      });
    }
  });

  await page.route(`**/api/v1/sites/${SITE.id}/autologin`, async (route) => {
    const mock = opts.autologin ?? {
      status: 200,
      body: {
        redirect_url: FAKE_REDIRECT_URL,
        expires_at: new Date(Date.now() + 30_000).toISOString(),
      },
    };
    await route.fulfill({
      status: mock.status,
      contentType: "application/json",
      body: JSON.stringify(mock.body),
    });
  });

  await page.route(`**/api/v1/sites/${SITE.id}`, async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify(SITE),
    });
  });

  await page.route("**/api/v1/sites*", async (route) => {
    await route.fulfill({
      status: 200,
      contentType: "application/json",
      body: JSON.stringify({ items: [SITE] }),
    });
  });
}

// Replace window.open BEFORE the SPA bootstraps so any call gets recorded into
// __wpmgrOpens — including the noopener cross-origin tab the button asks for.
async function shimWindowOpen(page: Page) {
  await page.addInitScript(() => {
    const w = window as unknown as {
      __wpmgrOpens: string[];
      open: typeof window.open;
    };
    w.__wpmgrOpens = [];
    w.open = function open(url?: string | URL) {
      w.__wpmgrOpens.push(typeof url === "string" ? url : String(url ?? ""));
      // Returning null mirrors a popup-blocked window; the app shouldn't care.
      return null;
    };
  });
}

async function getOpens(page: Page): Promise<string[]> {
  return await page.evaluate(
    () => (window as unknown as { __wpmgrOpens: string[] }).__wpmgrOpens,
  );
}

test.describe("Auto-login (Phase 5.5)", () => {
  test("admin sees the button and clicking it opens the redirect URL in a new tab", async ({
    page,
  }) => {
    await shimWindowOpen(page);
    await mockApi(page);

    await page.goto(`/sites/${SITE.id}`);

    const button = page
      .getByRole("button", { name: "Log in to site", exact: true })
      .first();
    await expect(button).toBeVisible();
    await expect(button).toBeEnabled();

    await button.click();

    // We don't navigate to the cross-origin URL; we just assert window.open was
    // called with it.
    await expect
      .poll(async () => (await getOpens(page))[0])
      .toBe(FAKE_REDIRECT_URL);
  });

  test("403 rbac_denied surfaces the right toast text", async ({ page }) => {
    await shimWindowOpen(page);
    await mockApi(page, {
      autologin: {
        status: 403,
        body: {
          code: "rbac_denied",
          message: "RBAC denied",
        },
      },
    });

    await page.goto(`/sites/${SITE.id}`);

    await page
      .getByRole("button", { name: "Log in to site", exact: true })
      .first()
      .click();

    await expect(
      page.getByText("You don't have permission to log into this site."),
    ).toBeVisible();
    // And no tab was opened.
    expect(await getOpens(page)).toEqual([]);
  });

  test("429 rate_limited shows the retry-after text", async ({ page }) => {
    await shimWindowOpen(page);
    await mockApi(page, {
      autologin: {
        status: 429,
        body: {
          code: "rate_limited",
          message: "Slow down",
          retry_after_seconds: 42,
        },
      },
    });

    await page.goto(`/sites/${SITE.id}`);

    await page
      .getByRole("button", { name: "Log in to site", exact: true })
      .first()
      .click();

    await expect(
      page.getByText("Too many attempts. Try again in 42s."),
    ).toBeVisible();
  });

  test("viewers (non-admin) do not see the auto-login button", async ({
    page,
  }) => {
    await shimWindowOpen(page);
    await mockApi(page, {
      me: {
        ...ME_ADMIN,
        memberships: [
          {
            user_id: ME_ADMIN.user.id,
            tenant_id: ME_ADMIN.active_tenant_id,
            role: "viewer",
          },
        ],
      },
    });

    await page.goto(`/sites/${SITE.id}`);
    // Wait for the heading so the page has clearly rendered before asserting
    // the button is absent.
    await expect(
      page.getByRole("heading", { name: "Example Site", level: 1 }),
    ).toBeVisible();
    await expect(
      page.getByRole("button", { name: "Log in to site", exact: true }),
    ).toHaveCount(0);
  });
});
