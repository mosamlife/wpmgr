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

  test("changing the current step scrolls the rail and leaves the page where it was", async ({
    page,
  }) => {
    // THE ONLY PLACE THIS IS TESTABLE. jsdom has no layout and no scrolling, so
    // a unit test could assert at most that some scroll function was called --
    // which is what missed this: the page was being dragged to the top while
    // the "right" call was being made. Here the document's own scroll position
    // is measured before and after.
    await mockApi(page);
    await page.setViewportSize({ width: 390, height: 844 });
    await page.goto("/ai/connect");
    await expect(page.getByTestId("step-rail")).toBeVisible();

    // Walk to the auth step and scroll down it, which is where an operator
    // stands when their next action changes the current step.
    await page.getByRole("button", { name: /claude code/i }).click();
    await page.getByRole("button", { name: /^Continue$/ }).click();
    const tokenCard = page.locator('button[data-method="token"]');
    await tokenCard.scrollIntoViewIfNeeded();
    await expect(tokenCard).toBeVisible();

    // MEASURED ON THE APP'S OWN SCROLL CONTAINER, NOT window.scrollY. This app
    // scrolls an inner element rather than the document, so window.scrollY
    // stays 0 no matter how far down the operator is and asserting on it would
    // pass whatever happened.
    //
    // AND NOT ON A SECTION'S BOUNDING BOX EITHER, because one step is on screen
    // at a time now: the click that changes the current step also replaces the
    // section, so there is no element whose y-coordinate means anything across
    // the move. The container's own scrollTop is the thing an operator feels,
    // and it is what `scrollIntoView` would have thrown away.
    const scroller = page.locator("main");
    const scrollTopBefore = await scroller.evaluate((el) => el.scrollTop);
    // The operator has genuinely scrolled: if they were still at the top the
    // assertion below could hold trivially.
    expect(scrollTopBefore).toBeGreaterThan(0);
    const railBefore = await page.getByTestId("step-rail").evaluate((el) => el.scrollLeft);

    // This moves the current step (auth -> site scope), so the rail's scroll
    // effect runs.
    await tokenCard.click();
    await page.getByRole("button", { name: /^Continue$/ }).click();
    await expect(page.locator('[data-step-n="3"]')).toHaveAttribute("aria-current", "step");

    // The smooth scroll settles; poll rather than sleep so this is not timing
    // dependent.
    await expect
      .poll(async () => page.getByTestId("step-rail").evaluate((el) => Math.round(el.scrollLeft)))
      .not.toBe(Math.round(railBefore));

    // THE ASSERTION THIS TEST EXISTS FOR: the rail did not drag the page to the
    // top. `scrollIntoView` moves whatever ancestors it must, and the rail sits
    // at the top of this container, so it would leave scrollTop at exactly 0.
    // Writing the rail's own scrollLeft cannot touch this axis at all.
    //
    // NOT ASSERTED AS AN EXACT EQUALITY, and the reason is a real one rather
    // than a tolerance: the step being moved to is shorter than the one being
    // left, so the container's scrollable range shrinks and the browser clamps
    // scrollTop to fit. That clamp is the page getting shorter, not the rail
    // scrolling it. Zero is the value only `scrollIntoView` produces.
    expect(await scroller.evaluate((el) => el.scrollTop)).toBeGreaterThan(0);
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
