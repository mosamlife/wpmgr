import { describe, it, expect, vi, afterEach } from "vitest";
import { screen, fireEvent, within } from "@testing-library/react";
import type { Site } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import { UpdateWizard, type WizardTarget } from "./update-wizard";

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
