import { test, expect, type Page } from "@playwright/test";

// THE LAYOUT HALF OF THE STEPPER'S COVERAGE, AND THE ONLY HALF THAT CAN SEE A
// WRAP. jsdom has no layout: every element measures 0x0 there, so a unit test
// can assert that the rail carries `flex-nowrap` and still not know whether
// ten segments end up on one row at 390px. The orphaned-connector defect (a
// connector travelling with the step after it, so the first step on every
// wrapped row opened with a line coming out of empty space) was invisible to
// every class-level assertion on this component, which is why it is measured
// here instead.

const TENANT = "22222222-2222-2222-2222-222222222222";

const ME = {
  user: {
    id: "33333333-3333-3333-3333-333333333333",
    email: "admin@example.com",
    name: "Admin",
    created_at: "2026-01-01T00:00:00Z",
    updated_at: "2026-01-01T00:00:00Z",
  },
  memberships: [
    { user_id: "33333333-3333-3333-3333-333333333333", tenant_id: TENANT, role: "owner" },
  ],
  active_tenant_id: TENANT,
};

async function mockApi(page: Page) {
  await page.route("**/auth/me", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: JSON.stringify(ME) });
  });
  await page.route("**/api/v1/mcp/connections*", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "[]" });
  });
  await page.route("**/api/v1/tags*", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "[]" });
  });
  await page.route("**/api/v1/sites*", async (route) => {
    await route.fulfill({ status: 200, contentType: "application/json", body: "[]" });
  });
}

/**
 * The measured rows of the rail: every step circle grouped by its vertical
 * position. One entry means the rail did not wrap.
 */
async function stepRows(page: Page): Promise<number[][]> {
  const boxes: { n: number; y: number }[] = [];
  for (let n = 1; n <= 10; n++) {
    const box = await page.getByTestId(`step-circle-${n}`).boundingBox();
    // A circle that cannot be measured must fail the test rather than being
    // quietly dropped out of the grouping, which would report "one row".
    if (box === null) throw new Error(`step circle ${n} has no bounding box`);
    boxes.push({ n, y: Math.round(box.y) });
  }
  const rows = new Map<number, number[]>();
  for (const b of boxes) rows.set(b.y, [...(rows.get(b.y) ?? []), b.n]);
  return [...rows.entries()].sort(([a], [b]) => a - b).map(([, ns]) => ns);
}

test.describe("the connection wizard stepper", () => {
  test("keeps all ten steps on one row at a phone width, so no connector starts a row", async ({
    page,
  }) => {
    await mockApi(page);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/ai/connect");
    await expect(page.getByTestId("step-rail")).toBeVisible();

    // ONE row. Two or more is the defect: the first circle of every row after
    // the first has a connector to its left, drawn from nothing.
    expect(await stepRows(page)).toEqual([[1, 2, 3, 4, 5, 6, 7, 8, 9, 10]]);

    // And the rail is scrollable rather than clipped, so the steps that do not
    // fit are still reachable at this width.
    const rail = page.getByTestId("step-rail");
    const overflow = await rail.evaluate(
      (el) => el.scrollWidth > el.clientWidth && getComputedStyle(el).overflowX,
    );
    expect(overflow).toBe("auto");

    // The deck's own breakpoint, measured rather than asserted by class name:
    // the connector is 16px below 640px.
    const line = await page.getByTestId("step-line-2").boundingBox();
    expect(line?.width).toBeCloseTo(16, 0);

    await page.screenshot({
      path: process.env.STEPPER_SHOT_DIR
        ? `${process.env.STEPPER_SHOT_DIR}/wizard-phone-390.png`
        : "test-results/wizard-phone-390.png",
      fullPage: true,
    });
  });

  test("keeps all ten steps on one row at desktop width, with the wide connector", async ({
    page,
  }) => {
    await mockApi(page);
    await page.setViewportSize({ width: 1440, height: 900 });
    await page.goto("/ai/connect");
    await expect(page.getByTestId("step-rail")).toBeVisible();

    expect(await stepRows(page)).toEqual([[1, 2, 3, 4, 5, 6, 7, 8, 9, 10]]);

    // 44px above the breakpoint, which is the other half of the deck's media
    // query. If `sm` ever moved off 640px this is what would catch it.
    const line = await page.getByTestId("step-line-2").boundingBox();
    expect(line?.width).toBeCloseTo(44, 0);

    await page.screenshot({
      path: process.env.STEPPER_SHOT_DIR
        ? `${process.env.STEPPER_SHOT_DIR}/wizard-desktop-1440.png`
        : "test-results/wizard-desktop-1440.png",
      fullPage: true,
    });
  });
});
