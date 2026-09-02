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


/**
 * Walk to the auth step and choose one method, which is the first frame where
 * the rail can say a step will never be asked.
 *
 * BEFORE A METHOD IS CHOSEN NOTHING IS STRUCK THROUGH, deliberately: the answer
 * that decides which of steps 7 to 10 an operator reaches is step 5's. So a
 * screenshot of the opening frame cannot show this state at all, which is why
 * every capture before this one missed it.
 */
async function walkToMethod(page: Page, method: "token" | "oauth") {
  await page.goto("/ai/connect");
  await expect(page.getByTestId("step-rail")).toBeVisible();
  await page.getByRole("button", { name: /^Continue$/ }).click();
  await page.getByRole("button", { name: /claude code/i }).click();
  await page.getByRole("button", { name: /^Continue$/ }).click();
  await page.getByRole("radio", { name: /all sites/i }).click();
  await page.getByRole("button", { name: /^Continue$/ }).click();
  await expect(page.getByRole("heading", { name: /^4\. Choose what it may do$/ })).toBeVisible();
  await page.getByRole("button", { name: /^Continue$/ }).click();
  await expect(
    page.getByRole("heading", { name: /^5\. Choose how it authenticates$/ }),
  ).toBeVisible();
  await page.locator(`button[data-method="${method}"]`).click();
}

function shotPath(name: string): string {
  return process.env.STEPPER_SHOT_DIR
    ? `${process.env.STEPPER_SHOT_DIR}/${name}.png`
    : `test-results/${name}.png`;
}


/** Walk to the capability step, which is where the presets live. */
async function walkToCapabilities(page: Page) {
  await page.goto("/ai/connect");
  await expect(page.getByTestId("step-rail")).toBeVisible();
  await page.getByRole("button", { name: /^Continue$/ }).click();
  await page.getByRole("button", { name: /claude code/i }).click();
  await page.getByRole("button", { name: /^Continue$/ }).click();
  await page.getByRole("radio", { name: /all sites/i }).click();
  await page.getByRole("button", { name: /^Continue$/ }).click();
  await expect(page.getByRole("heading", { name: /^4\. Choose what it may do$/ })).toBeVisible();
}

/** Put the rail and the step in frame before shooting -- see the note below. */
async function frameForShot(page: Page) {
  await page.locator("main").evaluate((el) => {
    el.scrollTop = 0;
  });
  await expect(page.getByTestId("step-rail")).toBeInViewport();
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
    // stands when their next action changes the current step. The walk is the
    // specified order now -- contract, client, sites, capabilities, auth -- so
    // getting here passes through every step before it rather than two.
    await page.getByRole("button", { name: /^Continue$/ }).click();
    await page.getByRole("button", { name: /claude code/i }).click();
    await page.getByRole("button", { name: /^Continue$/ }).click();
    await page.getByRole("radio", { name: /all sites/i }).click();
    await page.getByRole("button", { name: /^Continue$/ }).click();
    await expect(page.getByRole("heading", { name: /^4\. Choose what it may do$/ })).toBeVisible();
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

    // This moves the current step (auth -> setup artefact), so the rail's
    // scroll effect runs.
    await tokenCard.click();
    await page.getByRole("button", { name: /^Continue$/ }).click();
    await expect(page.locator('[data-step-n="6"]')).toHaveAttribute("aria-current", "step");

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

  // ---------------------------------------------------------------------------
  // THE RAIL'S STATES, CAPTURED WHERE THEY ARE ACTUALLY VISIBLE.
  //
  // They were correct in the DOM and asserted by two unit tests before any
  // screenshot contained one: every capture was of the opening frame, where no
  // method has been chosen and therefore nothing is struck through. A state
  // that is right in the markup and never looked at is not known to be legible.
  // ---------------------------------------------------------------------------

  for (const width of [390, 1440]) {
    test(`marks the step this path will never ask, legibly at ${String(width)}px`, async ({
      page,
    }) => {
      await mockApi(page);
      await page.setViewportSize({ width, height: width === 390 ? 844 : 900 });
      await walkToMethod(page, "token");

      // The token path never reaches the approval hand-off, so step 7 says so.
      await expect(page.locator('[data-step-n="7"]')).toHaveAttribute(
        "data-step-state",
        "not-applicable",
      );
      // And a step that IS ahead of them is not marked the same way.
      await expect(page.locator('[data-step-n="8"]')).toHaveAttribute(
        "data-step-state",
        "upcoming",
      );

      // MEASURED, NOT ASSUMED. A struck-through label that renders without the
      // line is the exact "right in the DOM, invisible on screen" failure this
      // capture exists to catch, so the computed style is read rather than the
      // class name.
      const struck = await page
        .getByTestId("step-label-7")
        .evaluate((el) => getComputedStyle(el).textDecorationLine);
      const plain = await page
        .getByTestId("step-label-8")
        .evaluate((el) => getComputedStyle(el).textDecorationLine);
      expect(struck).toContain("line-through");
      expect(plain).not.toContain("line-through");

      // THE RAIL HAS TO BE IN THE FRAME, and `fullPage` does not guarantee it.
      // This app scrolls an inner `main` rather than the document, so a
      // full-page capture records that scroller wherever it happens to be
      // sitting -- and the rail's own centring effect, plus a tall step, had
      // put it off the top at 390px. The first capture of this state showed no
      // rail at all: a screenshot that proves nothing while looking like
      // evidence.
      await page.locator("main").evaluate((el) => {
        el.scrollTop = 0;
      });
      await expect(page.getByTestId("step-rail")).toBeInViewport();
      await page.screenshot({ path: shotPath(`wizard-notasked-${String(width)}`), fullPage: true });
    });
  }

  test("marks the verification steps not-asked on the browser sign-in path", async ({ page }) => {
    // THE OTHER DIRECTION, which is what makes this a general rule rather than
    // a special case for step 7. On this path the client creates the grant
    // through the approval screen, so this wizard never learns its id and steps
    // 8 to 10 are the ones that will never be asked.
    await mockApi(page);
    await page.setViewportSize({ width: 1440, height: 900 });
    await walkToMethod(page, "oauth");

    for (const n of ["8", "9", "10"]) {
      await expect(page.locator(`[data-step-n="${n}"]`)).toHaveAttribute(
        "data-step-state",
        "not-applicable",
      );
    }
    await expect(page.locator('[data-step-n="7"]')).toHaveAttribute("data-step-state", "upcoming");

    await page.locator("main").evaluate((el) => {
      el.scrollTop = 0;
    });
    await expect(page.getByTestId("step-rail")).toBeInViewport();
    await page.screenshot({ path: shotPath("wizard-notasked-oauth-1440"), fullPage: true });
  });

  // ---------------------------------------------------------------------------
  // THE PRESET LABEL, WHERE IT CAN BE SEEN TO AGREE OR DISAGREE WITH THE ROWS.
  //
  // "Custom" being correct in the DOM while looking identical on screen is the
  // failure these captures exist to catch: a control that claims a preset over
  // a diverged set is the defect, and a control that says Custom in a way
  // nobody notices is the same defect wearing a passing test.
  // ---------------------------------------------------------------------------

  for (const width of [390, 1440]) {
    test(`shows the preset, then Custom once a row diverges, at ${String(width)}px`, async ({
      page,
    }) => {
      await mockApi(page);
      await page.setViewportSize({ width, height: width === 390 ? 844 : 900 });
      await walkToCapabilities(page);

      await page.getByTestId("preset-read-everything").click();
      await expect(page.getByTestId("preset-read-everything")).toHaveAttribute(
        "aria-pressed",
        "true",
      );
      await expect(page.getByTestId("preset-custom")).toHaveCount(0);
      // THE GLYPH, NOT ONLY THE ATTRIBUTE. aria-pressed was already correct
      // when this control looked wrong: pressing a preset leaves focus on it,
      // and the focus ring imitated the selected border closely enough that a
      // just-diverged preset went on looking chosen. A tick that only the
      // active branch renders is the thing a focus style cannot fake, so it is
      // what the capture asserts.
      await expect(page.getByTestId("preset-read-everything").locator("svg")).toHaveCount(1);
      await frameForShot(page);
      await page.screenshot({ path: shotPath(`wizard-preset-${String(width)}`), fullPage: true });

      // Untick one row. The claim must drop, and it must be visible that it did.
      await page.getByRole("checkbox", { name: /^Uptime/i }).click();
      await expect(page.getByTestId("preset-custom")).toBeVisible();
      await expect(page.getByTestId("preset-read-everything")).toHaveAttribute(
        "aria-pressed",
        "false",
      );
      // And the tick is GONE, which is the half that was silently wrong.
      await expect(page.getByTestId("preset-read-everything").locator("svg")).toHaveCount(0);
      await frameForShot(page);
      await page.screenshot({ path: shotPath(`wizard-custom-${String(width)}`), fullPage: true });
    });
  }
});
