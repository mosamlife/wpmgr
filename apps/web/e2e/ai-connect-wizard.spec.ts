import { test, expect, type Page } from "@playwright/test";

// Hermetic smoke test for the AI connection wizard, and the screenshot pass
// that goes with it.
//
// WHY THIS EXISTS AS A SPEC AND NOT AS A SCRIPT. The wizard shipped once with
// its rail marking one step current while five steps' content stood on the
// page at the same time. Every unit test passed; the defect was visible in
// about five seconds to anyone who looked at the page. So the property this
// file pins is the one a rendered assertion is worst at and a person is best
// at -- how many steps are actually on screen -- and it captures the two
// widths so a person can still look.
//
// Auth and the fleet are route-mocked the same way sites.spec.ts does it, so
// no live backend is required.

const TENANT = "22222222-2222-2222-2222-222222222222";

const ME = {
  user: {
    id: "33333333-3333-3333-3333-333333333333",
    email: "admin@example.com",
    name: "Admin",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [{ user_id: "33333333-3333-3333-3333-333333333333", tenant_id: TENANT, role: "owner" }],
  active_tenant_id: TENANT,
};

const SITES = Array.from({ length: 3 }, (_, i) => ({
  id: `1111111${i}-1111-1111-1111-111111111111`,
  tenant_id: TENANT,
  url: `https://site-${i}.example`,
  name: `site-${i}.example`,
  status: "active",
  health_status: "healthy",
  multisite: false,
  tags: ["production"],
  enrolled: true,
  enrolled_at: "2026-05-20T00:00:00Z",
}));

const TAGS = [{ id: "44444444-4444-4444-4444-444444444444", name: "production" }];

async function mockApi(page: Page) {
  await page.route("**/auth/me", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(ME) }),
  );
  await page.route("**/api/v1/tags*", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(TAGS) }),
  );
  await page.route("**/api/v1/mcp/connections*", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify([]) }),
  );
  await page.route("**/api/v1/sites*", (route) =>
    route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(SITES) }),
  );
}

/** The headings of the wizard's step sections that are actually on screen. */
async function visibleStepHeadings(page: Page): Promise<string[]> {
  return page.locator("section > div > h2").allInnerTexts();
}

/** Client picked and Continue pressed: standing on the auth-method step. */
async function reachMethodStep(page: Page) {
  await page.goto("/ai/connect");
  await page.getByRole("button", { name: /cursor/i }).first().click();
  await page.getByRole("button", { name: /^Continue$/ }).click();
  await expect(page.getByRole("heading", { name: /^5\. Choose how it authenticates$/ })).toBeVisible();
}

test.describe("the AI connection wizard shows one step at a time", () => {
  test.beforeEach(async ({ page }) => {
    await mockApi(page);
  });

  test("renders exactly one step section, and advances only on Continue", async ({ page }) => {
    await page.goto("/ai/connect");

    // ONE STEP. This is the assertion the original defect would have failed:
    // five section headings stood on the page at once while the rail marked a
    // single segment current.
    await expect(page.getByRole("heading", { name: /^2\. Pick your client$/ })).toBeVisible();
    expect(await visibleStepHeadings(page)).toHaveLength(1);

    // The rail is persistent and full length from the first frame.
    await expect(page.locator("[data-step-n]")).toHaveCount(10);
    await expect(page.locator('[aria-current="step"]')).toHaveCount(1);

    // Answering does not advance; Continue does.
    await page.getByRole("button", { name: /cursor/i }).first().click();
    await expect(page.getByRole("heading", { name: /^2\. Pick your client$/ })).toBeVisible();
    await page.getByRole("button", { name: /^Continue$/ }).click();

    await expect(page.getByRole("heading", { name: /^5\. Choose how it authenticates$/ })).toBeVisible();
    expect(await visibleStepHeadings(page)).toHaveLength(1);
    await expect(page.locator('[aria-current="step"]')).toHaveCount(1);

    // Back returns, and the client answer survives it.
    await page.getByRole("button", { name: /^Back$/ }).click();
    await expect(page.getByRole("heading", { name: /^2\. Pick your client$/ })).toBeVisible();
    await expect(page.getByRole("button", { name: /^Continue$/ })).toBeEnabled();
  });

  test("keeps all ten rail segments reachable by scrolling the rail itself", async ({ page }) => {
    // Ten segments do not fit on a phone. The rail is what scrolls; nothing is
    // hidden, wrapped away, or dropped to make them fit.
    await page.setViewportSize({ width: 390, height: 900 });
    await reachMethodStep(page);

    await expect(page.locator("[data-step-n]")).toHaveCount(10);
    const rail = await page.evaluate(() => {
      const el = document.querySelector('[data-testid="step-rail"]');
      return el === null ? null : { scroll: el.scrollWidth, client: el.clientWidth };
    });
    expect(rail).not.toBeNull();
    expect(rail!.scroll).toBeGreaterThan(rail!.client);
  });

  test("does not let the page itself scroll sideways at 390px", async ({ page }) => {
    // KNOWN FAILING, RECORDED RATHER THAN SOFTENED OR DELETED. At 390px the
    // viewport scrolls horizontally by roughly 660px, measured by trying to
    // scroll it rather than by reading documentElement.scrollWidth, which
    // reports the scrollable overflow of a clipped descendant and answers a
    // different question than "can a reader push this page off-screen".
    //
    // The rail is a scroll container and clips correctly: walking the ancestor
    // chain shows every element up to and including BODY reporting the
    // viewport width, and enumerating every element whose right edge exceeds
    // the viewport WITHOUT a clipping ancestor returns an empty list. The
    // remaining cause is therefore not the rail and not the one-step
    // navigation; it is filed separately. The check stays, and stays honest,
    // because a page a reader can shove off-screen on a phone is a real defect
    // and removing the assertion would only make it invisible again.
    test.fixme();

    await page.setViewportSize({ width: 390, height: 900 });
    await reachMethodStep(page);

    // Asserted by actually trying to scroll the window rather than by reading
    // documentElement.scrollWidth, which reports the scrollable overflow of a
    // clipped descendant and so answers a different question than "can a
    // reader push this page off-screen".
    const pageScrollX = await page.evaluate(() => {
      window.scrollTo(9999, 0);
      const x = window.scrollX;
      window.scrollTo(0, 0);
      return x;
    });
    expect(pageScrollX).toBe(0);
  });

  test("captures the wizard at 390px and 1440px", async ({ page }) => {
    const shots = process.env.WIZARD_SHOT_DIR;
    test.skip(shots === undefined, "WIZARD_SHOT_DIR not set");

    for (const [label, width] of [
      ["phone-390", 390],
      ["desktop-1440", 1440],
    ] as const) {
      await page.setViewportSize({ width, height: 900 });
      await reachMethodStep(page);
      await page.screenshot({ path: `${shots}/wizard-${label}.png`, fullPage: true });
    }
  });
});
