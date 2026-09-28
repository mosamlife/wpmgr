import { useState } from "react";
import { describe, it, expect, vi, afterEach, beforeEach } from "vitest";
import { screen, fireEvent, within, waitFor } from "@testing-library/react";
import type { Site, UpdateRunCreate } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import { UpdateWizard, type WizardTarget } from "./update-wizard";

// Hoisted mock of the generated create-update-run call. `vi.mock` is hoisted
// above every import by Vitest regardless of where it's written in the file,
// so this applies to the WHOLE file, not only the PR #752 tests below that
// exercise submission — kept next to the imports rather than buried under
// that describe block so that scope is obvious at a glance.
const { createUpdateRunMock } = vi.hoisted(() => ({
  createUpdateRunMock: vi.fn(),
}));

vi.mock("@wpmgr/api", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@wpmgr/api")>();
  return { ...actual, createUpdateRun: createUpdateRunMock };
});

// GH #211 — a WordPress update transient can occasionally report
// `new_version` equal to the already-installed `version` (observed with
// Kadence: "1.5.1 -> 1.5.1"). The bulk-update wizard used to treat any
// non-null `available_update` as a real update, which pre-filtered the
// phantom entry into the "has update" list and counted it in the tab badge.
// This pins the fix: a same-version (post-normalization) entry is treated as
// NOT having an update, everywhere `hasUpdate` drives visibility/labeling.

function buildSite(overrides: Partial<Site> = {}): Site {
  return {
    id: "site-1",
    tenant_id: "tenant-1",
    url: "https://example.com",
    name: "Example",
    status: "active",
    wp_version: "6.8",
    php_version: "8.3",
    health_status: "healthy",
    multisite: false,
    tags: [],
    ...overrides,
  } as unknown as Site;
}

const TARGET: WizardTarget = {
  kind: "sites",
  siteIds: ["site-1"],
  updateKind: "plugins",
};

describe("UpdateWizard — GH #211 same-version phantom guard", () => {
  it("excludes a same-version (and v-prefixed same-version) entry from the default 'has update' list and tab count, while a real update still appears", async () => {
    const site = buildSite({
      components: {
        plugins: [
          // Phantom: exact same-version advisory (the reported Kadence bug).
          {
            slug: "kadence",
            name: "Kadence",
            version: "1.5.1",
            available_update: { new_version: "1.5.1" },
          },
          // Phantom: "v"-prefixed same-version advisory.
          {
            slug: "elementor",
            name: "Elementor",
            version: "2.0.0",
            available_update: { new_version: "v2.0.0" },
          },
          // Real update: genuinely different version.
          {
            slug: "woocommerce",
            name: "WooCommerce",
            version: "8.0.0",
            available_update: { new_version: "8.1.0" },
          },
        ],
        themes: [],
      },
    });

    renderWithProviders(
      <UpdateWizard open target={TARGET} sites={[site]} onClose={() => {}} />,
      { withRouter: true },
    );

    // Default filter is "with updates only" — only the real update shows.
    // The test router's first paint is async (see test/render.tsx), so the
    // first assertion must be a `findBy*`.
    expect(await screen.findByText("WooCommerce")).toBeInTheDocument();
    expect(screen.queryByText("Kadence")).not.toBeInTheDocument();
    expect(screen.queryByText("Elementor")).not.toBeInTheDocument();

    // The Plugins tab count badge reflects only the real update (1), not 3.
    const tab = screen.getByRole("tab", { name: /plugins/i });
    expect(within(tab).getByText("1")).toBeInTheDocument();

    // Switching to "Show all" reveals the phantom rows, but they must be
    // labeled "up to date", never the "update" pill.
    fireEvent.click(screen.getByRole("button", { name: /show all/i }));

    const kadenceRow = screen.getByText("Kadence").closest("li");
    expect(kadenceRow).not.toBeNull();
    expect(within(kadenceRow!).getByText("up to date")).toBeInTheDocument();
    expect(within(kadenceRow!).queryByText("update")).not.toBeInTheDocument();

    const elementorRow = screen.getByText("Elementor").closest("li");
    expect(elementorRow).not.toBeNull();
    expect(within(elementorRow!).getByText("up to date")).toBeInTheDocument();
    expect(within(elementorRow!).queryByText("update")).not.toBeInTheDocument();

    const wooRow = screen.getByText("WooCommerce").closest("li");
    expect(wooRow).not.toBeNull();
    expect(within(wooRow!).getByText("update")).toBeInTheDocument();
  });

  it("still treats a missing installed version as having an update (fails open)", async () => {
    const site = buildSite({
      components: {
        plugins: [
          {
            slug: "mystery-plugin",
            name: "Mystery Plugin",
            available_update: { new_version: "2.0.0" },
          },
        ],
        themes: [],
      },
    });

    renderWithProviders(
      <UpdateWizard open target={TARGET} sites={[site]} onClose={() => {}} />,
      { withRouter: true },
    );

    expect(await screen.findByText("Mystery Plugin")).toBeInTheDocument();
  });
});

// Agent-version visibility (agent-releases): the WPMgr agent's own plugin
// entry must never be offered as a selectable update target, in any of the
// forms WordPress can report it under (directory/file, bare directory,
// single file, a renamed main file, mixed case, either distribution slug).
// The control plane already strips the advisory at the source; this pins
// the client-side belt-and-braces gate so a stale cache or hand-built
// payload still can never surface the agent here.
describe("UpdateWizard: agent plugin exclusion", () => {
  it("never lists the agent's own plugin entry as selectable, in any reported form", async () => {
    const site = buildSite({
      components: {
        plugins: [
          // The self-hosted distribution's real inventory key.
          {
            slug: "wpmgr-agent/wpmgr-agent.php",
            name: "WPMgr Agent",
            version: "0.61.90",
            available_update: { new_version: "0.61.95" },
          },
          // The wordpress.org distribution's real inventory key.
          {
            slug: "fleet-agent-site-manager/fleet-agent-site-manager.php",
            name: "Fleet Agent Site Manager",
            version: "0.61.90",
            available_update: { new_version: "0.61.95" },
          },
          // Bare directory, single file, mixed case, renamed main file.
          {
            slug: "wpmgr-agent",
            name: "WPMgr Agent (bare dir)",
            version: "0.61.90",
            available_update: { new_version: "0.61.95" },
          },
          {
            slug: "WPMGR-Agent/loader.php",
            name: "WPMgr Agent (renamed file, mixed case)",
            version: "0.61.90",
            available_update: { new_version: "0.61.95" },
          },
          // A real, unrelated plugin update must still appear.
          {
            slug: "woocommerce",
            name: "WooCommerce",
            version: "8.0.0",
            available_update: { new_version: "8.1.0" },
          },
        ],
        themes: [],
      },
    });

    renderWithProviders(
      <UpdateWizard open target={TARGET} sites={[site]} onClose={() => {}} />,
      { withRouter: true },
    );

    expect(await screen.findByText("WooCommerce")).toBeInTheDocument();

    // None of the agent's own entries ever render, even after "Show all".
    fireEvent.click(screen.getByRole("button", { name: /show all/i }));
    expect(screen.queryByText("WPMgr Agent")).not.toBeInTheDocument();
    expect(
      screen.queryByText("Fleet Agent Site Manager"),
    ).not.toBeInTheDocument();
    expect(screen.queryByText("WPMgr Agent (bare dir)")).not.toBeInTheDocument();
    expect(
      screen.queryByText("WPMgr Agent (renamed file, mixed case)"),
    ).not.toBeInTheDocument();

    // The Plugins tab count reflects only the one real, unrelated update.
    const tab = screen.getByRole("tab", { name: /plugins/i });
    expect(within(tab).getByText("1")).toBeInTheDocument();
  });
});

// GH #217 — when a tab has components but NONE of them have a pending
// update, the tab-strip badge used to fall back to the unfiltered distinct
// component count (rendered in a muted style), contradicting the "Showing 0
// with available updates" copy and the empty filtered list right below it.
// The badge must now show nothing at all in that case.
describe("UpdateWizard — GH #217 zero-updates tab badge", () => {
  it("shows no numeric badge on the Plugins tab when no plugin has a pending update", async () => {
    const site = buildSite({
      components: {
        plugins: [
          { slug: "akismet", name: "Akismet", version: "5.3" },
          { slug: "jetpack", name: "Jetpack", version: "13.0" },
        ],
        themes: [],
      },
    });

    renderWithProviders(
      <UpdateWizard open target={TARGET} sites={[site]} onClose={() => {}} />,
      { withRouter: true },
    );

    // Wait for first async paint, then assert the empty-filtered state.
    expect(
      await screen.findByText(
        "No plugins with available updates on the selected sites.",
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText("Showing 0 with available updates"),
    ).toBeInTheDocument();

    // The tab badge must render no number at all — not "2" (the distinct
    // component count), muted or otherwise.
    const tab = screen.getByRole("tab", { name: /plugins/i });
    expect(within(tab).queryByText("2")).not.toBeInTheDocument();
    expect(within(tab).queryByText("0")).not.toBeInTheDocument();

    // Toggling "Show all" reveals both components, each labeled up to date.
    fireEvent.click(screen.getByRole("button", { name: /show all/i }));

    const akismetRow = screen.getByText("Akismet").closest("li");
    expect(akismetRow).not.toBeNull();
    expect(within(akismetRow!).getByText("up to date")).toBeInTheDocument();

    const jetpackRow = screen.getByText("Jetpack").closest("li");
    expect(jetpackRow).not.toBeNull();
    expect(within(jetpackRow!).getByText("up to date")).toBeInTheDocument();
  });
});

// GH #463 — the wizard used to `return` out of onSubmit when the schedule was
// invalid, saying nothing. The rendered verdict is computed from the clock at
// RENDER time, so the case that reached that line was exactly the one where
// nothing on screen said no yet: a time that was valid when it was typed and
// went stale while the operator picked plugins. An enabled button that does
// nothing when clicked reads as a broken product, not as a refusal.
describe("UpdateWizard — GH #463 a refused schedule is never silent", () => {
  // Targeting core rather than plugins on purpose: `updateKind: "core"` seeds
  // one item synchronously, so the submit button is enabled on first paint
  // without waiting for the async component list. These tests are about the
  // schedule field, and a disabled button would short-circuit onSubmit before
  // it ever reached the validation under test.
  function openWizard() {
    renderWithProviders(
      <UpdateWizard
        open
        onClose={() => {}}
        target={{ kind: "sites", siteIds: ["site-1"], updateKind: "core" }}
        sites={[buildSite()]}
      />,
    );
  }

  const pad = (n: number) => String(n).padStart(2, "0");
  const localValue = (at: Date) =>
    `${at.getFullYear()}-${pad(at.getMonth() + 1)}-${pad(at.getDate())}` +
    `T${pad(at.getHours())}:${pad(at.getMinutes())}`;

  afterEach(() => {
    vi.useRealTimers();
    vi.unstubAllGlobals();
  });

  // THE case the fix exists for, and the only one that can distinguish it from
  // the pre-existing render-time validation: a schedule that is valid when
  // typed and stale by the time it is submitted. An earlier version of this
  // test typed an obviously-past time and asserted the alert appeared, which
  // passed with the silent `return` still in place, because the alert it saw
  // came from the render-time verdict and not from the submit path at all.
  it("names the reason when the time goes stale between typing and submitting", () => {
    const base = new Date("2026-08-19T06:00:00Z");
    vi.useFakeTimers();
    vi.setSystemTime(base);

    openWizard();

    // 90 minutes out: valid right now, so nothing on screen refuses it and
    // the submit button is enabled.
    const field = screen.getByLabelText(/schedule/i);
    fireEvent.change(field, {
      target: { value: localValue(new Date(base.getTime() + 90 * 60_000)) },
    });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();

    const submit = screen.getByRole("button", { name: /preview|apply/i });
    expect(submit).toBeEnabled();

    // The operator spends three hours choosing plugins. Nothing re-renders,
    // so the stale "valid" verdict is still what is on screen.
    vi.setSystemTime(new Date(base.getTime() + 3 * 60 * 60_000));
    fireEvent.click(submit);

    // Before the fix this discarded the submit and rendered nothing.
    expect(screen.getByRole("alert")).toHaveTextContent(/already passed/i);
    expect(field).toHaveAttribute("aria-invalid", "true");
    expect(field).toHaveAttribute("aria-describedby", "schedule-at-error");
  });

  it("clears the refusal as soon as the operator edits the time", () => {
    const base = new Date("2026-08-19T06:00:00Z");
    vi.useFakeTimers();
    vi.setSystemTime(base);

    openWizard();

    const field = screen.getByLabelText(/schedule/i);
    fireEvent.change(field, {
      target: { value: localValue(new Date(base.getTime() + 90 * 60_000)) },
    });
    vi.setSystemTime(new Date(base.getTime() + 3 * 60 * 60_000));
    fireEvent.click(screen.getByRole("button", { name: /preview|apply/i }));
    expect(screen.getByRole("alert")).toBeInTheDocument();

    // A fresh future time: the refusal must not outlive the value it judged.
    fireEvent.change(field, {
      target: {
        value: localValue(new Date(base.getTime() + 6 * 60 * 60_000)),
      },
    });

    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(field).not.toHaveAttribute("aria-invalid");
  });

  // The honest case the guard must NOT block. A guard that reddens correct
  // work gets switched off, and then it guards nothing.
  //
  // This deliberately stops short of clicking submit. Both refusal paths
  // manifest BEFORE the request: the live verdict disables the button and
  // renders the alert, and the submit-time verdict renders the same alert
  // synchronously. Asserting the button is enabled and no alert is present is
  // therefore the whole of "not refused", and it avoids driving the real
  // generated client at a relative URL under jsdom, which rejects unhandled
  // and fails the suite while every test still reports green.
  it("does not refuse a valid future schedule", () => {
    const base = new Date("2026-08-19T06:00:00Z");
    vi.useFakeTimers();
    vi.setSystemTime(base);

    openWizard();

    fireEvent.change(screen.getByLabelText(/schedule/i), {
      target: {
        value: localValue(new Date(base.getTime() + 8 * 60 * 60_000)),
      },
    });

    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByLabelText(/schedule/i)).not.toHaveAttribute(
      "aria-invalid",
    );
    expect(screen.getByRole("button", { name: /preview|apply/i })).toBeEnabled();
  });
});

// PR #752 (GH #680) — "Select all" / "Deselect all" for the active tab's
// available updates. Selection starts EMPTY (see the header comment at the
// top of update-wizard.tsx); these tests pin what the toggle does to
// `selectedSlugs`, scoped to the ACTIVE tab's `hasUpdate` keys only —
// `activeUpdatableKeys` and `toggleSelectAll` in update-wizard.tsx.

/** The checkbox `<input>` for a labeled row, found via its visible text. */
function checkboxFor(label: string): HTMLInputElement {
  const row = screen.getByText(label).closest("label");
  if (!row) throw new Error(`no <label> ancestor for "${label}"`);
  const input = row.querySelector("input[type=checkbox]");
  if (!input) throw new Error(`no checkbox in the "${label}" row`);
  return input as HTMLInputElement;
}

describe("UpdateWizard — PR #752 Select all / Deselect all (GH #680)", () => {
  const SITE = buildSite({
    id: "site-a",
    components: {
      plugins: [
        {
          slug: "woo",
          name: "Woo",
          version: "8.0",
          available_update: { new_version: "8.1" },
        },
        {
          slug: "yoast",
          name: "Yoast",
          version: "20",
          available_update: { new_version: "21" },
        },
        // Up to date — no available_update at all.
        { slug: "akismet", name: "Akismet", version: "5.3" },
        // GH #211 phantom same-version advisory: hasUpdate is false for
        // this one, so Select all must never tick it.
        {
          slug: "kadence",
          name: "Kadence",
          version: "1.5.1",
          available_update: { new_version: "1.5.1" },
        },
      ],
      themes: [
        {
          slug: "astra",
          name: "Astra",
          version: "4",
          available_update: { new_version: "4.1" },
        },
        { slug: "tt4", name: "Twenty Twenty-Four", version: "1.0" },
      ],
    },
  });

  const TARGET: WizardTarget = {
    kind: "sites",
    siteIds: ["site-a"],
    updateKind: "plugins",
  };

  beforeEach(() => {
    createUpdateRunMock.mockReset();
    createUpdateRunMock.mockResolvedValue({
      data: { id: "run-1" },
      error: undefined,
      response: { status: 201 },
    });
  });

  async function openWizard(
    sites: Site[] = [SITE],
    target: WizardTarget = TARGET,
  ) {
    renderWithProviders(
      <UpdateWizard open target={target} sites={sites} onClose={() => {}} />,
      { withRouter: true },
    );
    // First paint is async under the test router — see test/render.tsx.
    await screen.findByRole("tab", { name: /plugins/i });
  }

  it("1. filter on: Select all ticks exactly the visible rows with a real update, and posts exactly those", async () => {
    await openWizard();
    expect(
      screen.getByText("Select at least one thing to update."),
    ).toBeInTheDocument();
    expect(checkboxFor("Woo").checked).toBe(false);
    expect(checkboxFor("Yoast").checked).toBe(false);

    fireEvent.click(
      screen.getByRole("button", { name: "Select all available updates" }),
    );

    expect(checkboxFor("Woo").checked).toBe(true);
    expect(checkboxFor("Yoast").checked).toBe(true);
    expect(screen.getByText("2 items will be previewed.")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Deselect all available updates" }),
    ).toHaveTextContent("Deselect all");

    // The hidden up-to-date row and the hidden phantom-update row were never
    // ticked, even though they were never visible to click.
    fireEvent.click(screen.getByRole("button", { name: "Show all" }));
    expect(checkboxFor("Akismet").checked).toBe(false);
    expect(checkboxFor("Kadence").checked).toBe(false);

    fireEvent.click(screen.getByRole("button", { name: /preview 2 updates/i }));
    await waitFor(() => expect(createUpdateRunMock).toHaveBeenCalledTimes(1));
    const [{ body }] = createUpdateRunMock.mock.calls[0] as [
      { body: UpdateRunCreate },
    ];
    expect(body.site_ids).toEqual(["site-a"]);
    expect(body.dry_run).toBe(true);
    expect(body.items).toEqual([
      { type: "plugin", slug: "woo", version: "latest" },
      { type: "plugin", slug: "yoast", version: "latest" },
    ]);
  });

  it("1b. hand-ticking a single row (no Select all) posts exactly that row, not every updatable item on the tab", async () => {
    await openWizard();
    fireEvent.click(checkboxFor("Woo"));
    expect(screen.getByText("1 item will be previewed.")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /preview 1 update/i }));
    await waitFor(() => expect(createUpdateRunMock).toHaveBeenCalledTimes(1));
    const [{ body }] = createUpdateRunMock.mock.calls[0] as [
      { body: UpdateRunCreate },
    ];
    // Yoast also has an update and is on the same tab, but was never
    // ticked — it must not ride along in the POST body.
    expect(body.items).toEqual([
      { type: "plugin", slug: "woo", version: "latest" },
    ]);
  });

  it("2. show all: Select all ticks only rows with a real update; up-to-date rows stay unticked", async () => {
    await openWizard();
    fireEvent.click(screen.getByRole("button", { name: "Show all" }));

    fireEvent.click(
      screen.getByRole("button", { name: "Select all available updates" }),
    );

    expect(checkboxFor("Woo").checked).toBe(true);
    expect(checkboxFor("Yoast").checked).toBe(true);
    expect(checkboxFor("Akismet").checked).toBe(false);
    expect(checkboxFor("Kadence").checked).toBe(false);
    expect(screen.getByText("2 items will be previewed.")).toBeInTheDocument();
  });

  it("3. partial selection: Select all fills in the rest and keeps what was already ticked", async () => {
    await openWizard();
    // A row that is NOT one of the active tab's updatable keys, ticked
    // before Select all: an up-to-date row on this tab, revealed via "Show
    // all". If Select all rebuilt the set from scratch instead of adding to
    // it, this tick would be the one thing that could show that, since
    // "Woo" alone (an updatable key) would survive a from-scratch rebuild
    // too.
    fireEvent.click(screen.getByRole("button", { name: "Show all" }));
    fireEvent.click(checkboxFor("Akismet"));
    fireEvent.click(checkboxFor("Woo"));
    expect(
      screen.getByRole("button", { name: "Select all available updates" }),
    ).toHaveTextContent("Select all");

    fireEvent.click(
      screen.getByRole("button", { name: "Select all available updates" }),
    );

    expect(checkboxFor("Woo").checked).toBe(true);
    expect(checkboxFor("Yoast").checked).toBe(true);
    // The pre-ticked up-to-date row, not itself one of the keys Select all
    // touches, must still be ticked afterwards.
    expect(checkboxFor("Akismet").checked).toBe(true);
    expect(screen.getByText("3 items will be previewed.")).toBeInTheDocument();
  });

  it("4. deselect all: removes only this tab's updatable keys; a hand-picked up-to-date row and the other tab's picks survive", async () => {
    await openWizard();
    fireEvent.click(screen.getByRole("button", { name: "Show all" }));
    // Hand-picked up-to-date row on the ACTIVE (plugins) tab.
    fireEvent.click(checkboxFor("Akismet"));

    // A pick on the OTHER tab (themes).
    fireEvent.click(screen.getByRole("tab", { name: /themes/i }));
    fireEvent.click(checkboxFor("Astra"));
    fireEvent.click(screen.getByRole("tab", { name: /plugins/i }));

    fireEvent.click(
      screen.getByRole("button", { name: "Select all available updates" }),
    );
    expect(checkboxFor("Woo").checked).toBe(true);
    expect(checkboxFor("Yoast").checked).toBe(true);
    expect(screen.getByText("4 items will be previewed.")).toBeInTheDocument();

    fireEvent.click(
      screen.getByRole("button", { name: "Deselect all available updates" }),
    );

    // Only woo/yoast (this tab's updatable keys) were removed.
    expect(checkboxFor("Woo").checked).toBe(false);
    expect(checkboxFor("Yoast").checked).toBe(false);
    // The hand-picked up-to-date row on this tab survives.
    expect(checkboxFor("Akismet").checked).toBe(true);
    expect(screen.getByText("2 items will be previewed.")).toBeInTheDocument();

    // The other tab's pick survives too.
    fireEvent.click(screen.getByRole("tab", { name: /themes/i }));
    expect(checkboxFor("Astra").checked).toBe(true);
  });

  it("4b. submits every selected slug across tabs, including an up-to-date row and a theme — not just the active tab's updatable keys", async () => {
    await openWizard();
    fireEvent.click(screen.getByRole("button", { name: "Show all" }));
    // Hand-picked up-to-date row on the ACTIVE (plugins) tab.
    fireEvent.click(checkboxFor("Akismet"));

    // A pick on the OTHER tab (themes).
    fireEvent.click(screen.getByRole("tab", { name: /themes/i }));
    fireEvent.click(checkboxFor("Astra"));
    fireEvent.click(screen.getByRole("tab", { name: /plugins/i }));

    fireEvent.click(
      screen.getByRole("button", { name: "Select all available updates" }),
    );
    expect(screen.getByText("4 items will be previewed.")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /preview 4 updates/i }));
    await waitFor(() => expect(createUpdateRunMock).toHaveBeenCalledTimes(1));
    const [{ body }] = createUpdateRunMock.mock.calls[0] as [
      { body: UpdateRunCreate },
    ];
    // Every selected slug is posted: the two Select-all'd plugin updates,
    // the hand-picked up-to-date plugin (no update, so a filter keyed on
    // "has an update" would drop it), and the theme picked on the other tab
    // (so a filter keyed on "currently visible on this tab" would drop it).
    const slugs = body.items.map((item) => item.slug).sort();
    expect(slugs).toEqual(["akismet", "astra", "woo", "yoast"]);
  });

  it("5. the other tab is untouched by Select all", async () => {
    await openWizard();
    fireEvent.click(screen.getByRole("tab", { name: /themes/i }));
    fireEvent.click(checkboxFor("Astra"));
    fireEvent.click(screen.getByRole("tab", { name: /plugins/i }));

    fireEvent.click(
      screen.getByRole("button", { name: "Select all available updates" }),
    );
    expect(screen.getByText("3 items will be previewed.")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("tab", { name: /themes/i }));
    expect(checkboxFor("Astra").checked).toBe(true);
    expect(screen.getByText("3 items will be previewed.")).toBeInTheDocument();
  });

  it("7. Select all works on the Themes tab itself, not just Plugins", async () => {
    await openWizard();
    fireEvent.click(screen.getByRole("tab", { name: /themes/i }));
    expect(checkboxFor("Astra").checked).toBe(false);

    fireEvent.click(
      screen.getByRole("button", { name: "Select all available updates" }),
    );

    expect(checkboxFor("Astra").checked).toBe(true);
    expect(screen.getByText("1 item will be previewed.")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Deselect all available updates" }),
    ).toHaveTextContent("Deselect all");
  });

  it("6. no Select all button when the active tab has zero available updates", async () => {
    const upToDateOnly = buildSite({
      id: "site-b",
      components: {
        plugins: [{ slug: "akismet", name: "Akismet", version: "5.3" }],
        themes: [],
      },
    });
    await openWizard([upToDateOnly], {
      kind: "sites",
      siteIds: ["site-b"],
      updateKind: "plugins",
    });
    fireEvent.click(screen.getByRole("button", { name: "Show all" }));
    expect(screen.getByText("Akismet")).toBeInTheDocument();
    // "Deselect all available updates" also matches /select all/i as a
    // substring, so this one query rules out both button states.
    expect(
      screen.queryByRole("button", { name: /select all/i }),
    ).not.toBeInTheDocument();
  });
});

// GH #763 — a selection survives after its update disappears and is still
// sent. `WizardForm` is keyed on `targetKey(target)` only (see the top of
// update-wizard.tsx), so a live sites refetch while the wizard is open
// (use-sites-live.ts:59 invalidates the list on cardinality/state-change
// events) swaps the `sites` prop on the SAME mounted form rather than
// remounting it — exactly like `routes/_authed/sites/index.tsx` passing a
// freshly-fetched `selectedSites` array into `sites=` on every render. This
// harness reproduces that prop swap directly: `sites` lives in local state,
// a button swaps it to a new array (the "refetch"), and `target` never
// changes across the swap, so the wizard's remount key doesn't change
// either — the precise condition the bug needs.
function RefetchHarness({
  target,
  initialSites,
  nextSites,
}: {
  target: WizardTarget;
  initialSites: Site[];
  nextSites: Site[];
}) {
  const [sites, setSites] = useState(initialSites);
  return (
    <>
      <button type="button" onClick={() => setSites(nextSites)}>
        Simulate live refetch
      </button>
      <UpdateWizard open target={target} sites={sites} onClose={() => {}} />
    </>
  );
}

describe("UpdateWizard — GH #763 a selection survives after its update disappears", () => {
  const TARGET: WizardTarget = {
    kind: "sites",
    siteIds: ["site-a"],
    updateKind: "plugins",
  };

  beforeEach(() => {
    createUpdateRunMock.mockReset();
    createUpdateRunMock.mockResolvedValue({
      data: { id: "run-1" },
      error: undefined,
      response: { status: 201 },
    });
  });

  it("drops a selected item that loses its update on a live refetch: not counted, not posted, while a still-updatable sibling is", async () => {
    const before = buildSite({
      id: "site-a",
      components: {
        plugins: [
          {
            slug: "woo",
            name: "Woo",
            version: "8.0",
            available_update: { new_version: "8.1" },
          },
          {
            slug: "yoast",
            name: "Yoast",
            version: "20",
            available_update: { new_version: "21" },
          },
        ],
        themes: [],
      },
    });
    // Same site, refetched: Woo's update landed some other way (or the
    // advisory expired) and it no longer reports one. Yoast is untouched.
    const after = buildSite({
      id: "site-a",
      components: {
        plugins: [
          { slug: "woo", name: "Woo", version: "8.1" },
          {
            slug: "yoast",
            name: "Yoast",
            version: "20",
            available_update: { new_version: "21" },
          },
        ],
        themes: [],
      },
    });

    renderWithProviders(
      <RefetchHarness target={TARGET} initialSites={[before]} nextSites={[after]} />,
      { withRouter: true },
    );

    await screen.findByRole("tab", { name: /plugins/i });
    fireEvent.click(checkboxFor("Woo"));
    fireEvent.click(checkboxFor("Yoast"));
    expect(screen.getByText("2 items will be previewed.")).toBeInTheDocument();

    // The refetch trigger lives outside the Dialog's portal, so Radix marks
    // it `aria-hidden` while the dialog is open (correctly, for a real
    // screen reader) — `getByText` rather than `getByRole` reaches it here,
    // same as production reaching it via a query invalidate, not a click.
    fireEvent.click(screen.getByText(/simulate live refetch/i));

    // Woo drops out of the default "with updates" filter (it has none any
    // more) AND out of the count — before the fix it stayed counted while
    // invisible.
    expect(screen.queryByText("Woo")).not.toBeInTheDocument();
    expect(screen.getByText("1 item will be previewed.")).toBeInTheDocument();

    // Confirms it, rather than just its filtered visibility: still unticked
    // even under "Show all".
    fireEvent.click(screen.getByRole("button", { name: "Show all" }));
    expect(checkboxFor("Woo").checked).toBe(false);
    expect(checkboxFor("Yoast").checked).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: /preview 1 update/i }));
    await waitFor(() => expect(createUpdateRunMock).toHaveBeenCalledTimes(1));
    const [{ body }] = createUpdateRunMock.mock.calls[0] as [
      { body: UpdateRunCreate },
    ];
    expect(body.items).toEqual([
      { type: "plugin", slug: "yoast", version: "latest" },
    ]);
  });

  it("keeps a deliberately-ticked up-to-date row selected across the same kind of refetch", async () => {
    const before = buildSite({
      id: "site-a",
      components: {
        plugins: [
          {
            slug: "woo",
            name: "Woo",
            version: "8.0",
            available_update: { new_version: "8.1" },
          },
          // Up to date from the start — only selectable by hand, via "Show
          // all", same as the PR #752 tests.
          { slug: "akismet", name: "Akismet", version: "5.3" },
        ],
        themes: [],
      },
    });
    // Woo's update disappears; Akismet's up-to-date status is unchanged.
    const after = buildSite({
      id: "site-a",
      components: {
        plugins: [
          { slug: "woo", name: "Woo", version: "8.1" },
          { slug: "akismet", name: "Akismet", version: "5.3" },
        ],
        themes: [],
      },
    });

    renderWithProviders(
      <RefetchHarness target={TARGET} initialSites={[before]} nextSites={[after]} />,
      { withRouter: true },
    );

    await screen.findByRole("tab", { name: /plugins/i });
    fireEvent.click(checkboxFor("Woo"));
    fireEvent.click(screen.getByRole("button", { name: "Show all" }));
    fireEvent.click(checkboxFor("Akismet"));
    expect(screen.getByText("2 items will be previewed.")).toBeInTheDocument();

    fireEvent.click(screen.getByText(/simulate live refetch/i));

    // Woo (lost its update) is dropped; Akismet (was already up to date,
    // still is) survives — the count reflects exactly Akismet.
    expect(checkboxFor("Woo").checked).toBe(false);
    expect(checkboxFor("Akismet").checked).toBe(true);
    expect(screen.getByText("1 item will be previewed.")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /preview 1 update/i }));
    await waitFor(() => expect(createUpdateRunMock).toHaveBeenCalledTimes(1));
    const [{ body }] = createUpdateRunMock.mock.calls[0] as [
      { body: UpdateRunCreate },
    ];
    expect(body.items).toEqual([
      { type: "plugin", slug: "akismet", version: "latest" },
    ]);
  });
});

// Adversarial review of PR #766 (GH #763). `RefetchHarness` above swaps
// `sites` exactly once, which is enough to pin "loses its update, stays
// dropped" but not these two: both need a SECOND swap, because the bug only
// shows up on the comparison after the first swap has already happened.
function MultiRefetchHarness({
  target,
  snapshots,
}: {
  target: WizardTarget;
  snapshots: Site[][];
}) {
  const [i, setI] = useState(0);
  return (
    <>
      <button
        type="button"
        onClick={() => setI((n) => Math.min(n + 1, snapshots.length - 1))}
      >
        Simulate live refetch
      </button>
      <UpdateWizard
        open
        target={target}
        sites={snapshots[i] ?? []}
        onClose={() => {}}
      />
    </>
  );
}

describe("UpdateWizard — PR #766 adversarial review of the #763 fix", () => {
  const TARGET: WizardTarget = {
    kind: "sites",
    siteIds: ["site-a"],
    updateKind: "plugins",
  };

  beforeEach(() => {
    createUpdateRunMock.mockReset();
    createUpdateRunMock.mockResolvedValue({
      data: { id: "run-1" },
      error: undefined,
      response: { status: 201 },
    });
  });

  // Pins the `prevOptionsRef.current = options` assignment in update-wizard.tsx.
  // Without it, every later comparison runs against the snapshot from when the
  // wizard first opened, never against the render just before it — so an
  // update that arrives only AFTER the wizard is open, gets ticked, and then
  // disappears again looks (against that stale mount-time snapshot, where
  // this item never had an update to begin with) like it never had an update
  // to lose, and the tick survives.
  it("drops an item whose update arrived after the wizard opened, was ticked, then disappeared again", async () => {
    const openedWithout = buildSite({
      id: "site-a",
      components: {
        plugins: [
          { slug: "woo", name: "Woo", version: "8.1" },
          {
            slug: "yoast",
            name: "Yoast",
            version: "20",
            available_update: { new_version: "21" },
          },
        ],
        themes: [],
      },
    });
    const gainedUpdate = buildSite({
      id: "site-a",
      components: {
        plugins: [
          {
            slug: "woo",
            name: "Woo",
            version: "8.0",
            available_update: { new_version: "8.1" },
          },
          {
            slug: "yoast",
            name: "Yoast",
            version: "20",
            available_update: { new_version: "21" },
          },
        ],
        themes: [],
      },
    });
    const lostItAgain = buildSite({
      id: "site-a",
      components: {
        plugins: [
          { slug: "woo", name: "Woo", version: "8.1" },
          {
            slug: "yoast",
            name: "Yoast",
            version: "20",
            available_update: { new_version: "21" },
          },
        ],
        themes: [],
      },
    });

    renderWithProviders(
      <MultiRefetchHarness
        target={TARGET}
        snapshots={[[openedWithout], [gainedUpdate], [lostItAgain]]}
      />,
      { withRouter: true },
    );

    await screen.findByRole("tab", { name: /plugins/i });
    fireEvent.click(screen.getByText(/simulate live refetch/i)); // Woo's update arrives
    fireEvent.click(checkboxFor("Woo"));
    fireEvent.click(checkboxFor("Yoast"));
    expect(screen.getByText("2 items will be previewed.")).toBeInTheDocument();

    fireEvent.click(screen.getByText(/simulate live refetch/i)); // Woo's update lands elsewhere
    expect(screen.queryByText("Woo")).not.toBeInTheDocument();
    expect(screen.getByText("1 item will be previewed.")).toBeInTheDocument();

    // Confirms it, rather than just its filtered visibility: still unticked
    // even under "Show all".
    fireEvent.click(screen.getByRole("button", { name: "Show all" }));
    expect(checkboxFor("Woo").checked).toBe(false);
    expect(checkboxFor("Yoast").checked).toBe(true);

    fireEvent.click(screen.getByRole("button", { name: /preview 1 update/i }));
    await waitFor(() => expect(createUpdateRunMock).toHaveBeenCalledTimes(1));
    const [{ body }] = createUpdateRunMock.mock.calls[0] as [
      { body: UpdateRunCreate },
    ];
    expect(body.items).toEqual([
      { type: "plugin", slug: "yoast", version: "latest" },
    ]);
  });

  // Pins the "no longer listed" branch (the `if (!nextOpt)` branch in
  // update-wizard.tsx). This is
  // the only branch that can drop a key whose item vanished from `options`
  // entirely WHILE UP TO DATE (hasUpdate already false, not transitioning
  // true -> false) — the hasUpdate-comparison branch below it never fires for
  // that key, on the way out or the way back in, so nothing else in the
  // effect would catch it. Without it, a hand-ticked up-to-date row whose
  // entry disappears entirely and later comes back is still selected once it
  // reappears, with no click of the operator's behind it, and gets posted
  // even though `buildItems` had correctly excluded it (as not currently
  // listed) for every render while it was gone.
  it("does not resurrect a hand-ticked up-to-date row whose entry disappeared entirely and later reappeared", async () => {
    const before = buildSite({
      id: "site-a",
      components: {
        plugins: [
          // Up to date from the start — only selectable by hand, via "Show
          // all", same as the PR #752 tests.
          { slug: "woo", name: "Woo", version: "8.1" },
          {
            slug: "yoast",
            name: "Yoast",
            version: "20",
            available_update: { new_version: "21" },
          },
        ],
        themes: [],
      },
    });
    // Woo's entry drops out of the report entirely — gone from `options`,
    // not merely re-reported as up to date (e.g. deactivated, or the agent
    // stopped reporting it).
    const wooGone = buildSite({
      id: "site-a",
      components: {
        plugins: [
          {
            slug: "yoast",
            name: "Yoast",
            version: "20",
            available_update: { new_version: "21" },
          },
        ],
        themes: [],
      },
    });
    // Woo's entry comes back, still up to date, unchanged from `before`.
    const wooBack = before;

    renderWithProviders(
      <MultiRefetchHarness
        target={TARGET}
        snapshots={[[before], [wooGone], [wooBack]]}
      />,
      { withRouter: true },
    );

    await screen.findByRole("tab", { name: /plugins/i });
    fireEvent.click(screen.getByRole("button", { name: "Show all" }));
    fireEvent.click(checkboxFor("Woo"));
    fireEvent.click(checkboxFor("Yoast"));
    expect(screen.getByText("2 items will be previewed.")).toBeInTheDocument();

    fireEvent.click(screen.getByText(/simulate live refetch/i)); // Woo's entry vanishes entirely
    expect(screen.queryByText("Woo")).not.toBeInTheDocument();
    expect(screen.getByText("1 item will be previewed.")).toBeInTheDocument();

    fireEvent.click(screen.getByText(/simulate live refetch/i)); // Woo's entry comes back
    expect(checkboxFor("Woo").checked).toBe(false);
    expect(screen.getByText("1 item will be previewed.")).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /preview 1 update/i }));
    await waitFor(() => expect(createUpdateRunMock).toHaveBeenCalledTimes(1));
    const [{ body }] = createUpdateRunMock.mock.calls[0] as [
      { body: UpdateRunCreate },
    ];
    expect(body.items).toEqual([
      { type: "plugin", slug: "yoast", version: "latest" },
    ]);
  });
});
