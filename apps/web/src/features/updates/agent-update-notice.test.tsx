import { describe, expect, it } from "vitest";
import { screen, within } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { AGENT_FLEET_NOTE_BODY } from "@/features/sites/agent-column-header";

import { AgentUpdateNotice } from "./agent-update-notice";
import type { SiteAgentUpdate } from "./use-site-agent-update";

// GH #314 (reported against 0.61.102, hosted): the site Updates tab said
// "All up to date" with a green check while the same site's wp-admin was
// offering a WPMgr agent update. These tests pin the line that now states the
// agent separately, and above all pin that it never becomes actionable from
// this tab.

function build(overrides: Partial<SiteAgentUpdate> = {}): SiteAgentUpdate {
  return {
    status: "outdated",
    version: "0.61.100",
    latestVersion: "0.61.121",
    referenceSource: "published",
    channelAvailable: true,
    ...overrides,
  };
}

describe("AgentUpdateNotice", () => {
  // ── The safety property. Do not weaken these two. ────────────────────────
  //
  // The agent is excluded from managed updates because applying it through
  // the ordinary plugin path is the plugin overwriting its own running files
  // inside the request that has to report the outcome, with no armed
  // rollback. A checkbox here, or any control that could enqueue a run, would
  // put that path back.

  it("is never selectable: no checkbox, no selection control of any kind", async () => {
    const { container } = renderWithProviders(
      <AgentUpdateNotice agent={build()} />,
      { withRouter: true },
    );
    const notice = await screen.findByTestId("agent-update-notice");

    expect(within(notice).queryByRole("checkbox")).not.toBeInTheDocument();
    expect(within(notice).queryByRole("radio")).not.toBeInTheDocument();
    expect(container.querySelector("input")).toBeNull();
  });

  it("offers no button that could start an update run, only a link", async () => {
    renderWithProviders(<AgentUpdateNotice agent={build()} />, {
      withRouter: true,
    });
    const notice = await screen.findByTestId("agent-update-notice");

    expect(within(notice).queryByRole("button")).not.toBeInTheDocument();
    expect(notice.querySelector("form")).toBeNull();
  });

  // ── Where the operator is sent (GH #314 requirement 3) ───────────────────

  it("links to the dedicated channel when it is available to this operator", async () => {
    renderWithProviders(<AgentUpdateNotice agent={build()} />, {
      withRouter: true,
    });

    const link = await screen.findByRole("link", {
      name: "Open agent updates",
    });
    const href = link.getAttribute("href") ?? "";
    expect(href).toContain("/sites");
    // Pre-filtered to the agent-status facet, so the operator lands on the
    // sites the channel can actually act on.
    expect(href).toContain("Outdated");
    expect(
      screen.getByText(/Select this site on the Sites page/),
    ).toBeInTheDocument();
    // Never both: the channel is the better route, so wp-admin is not offered
    // alongside it.
    expect(screen.queryByText(/Plugins screen/)).not.toBeInTheDocument();
  });

  it("says what to do instead when the channel is off or the operator cannot use it", () => {
    renderWithProviders(
      <AgentUpdateNotice agent={build({ channelAvailable: false })} />,
    );

    expect(
      screen.queryByRole("link", { name: "Open agent updates" }),
    ).not.toBeInTheDocument();
    expect(
      screen.getByText(/Plugins screen in WordPress admin/),
    ).toBeInTheDocument();
  });

  it("shows the installed and available versions for an outdated agent", () => {
    renderWithProviders(
      <AgentUpdateNotice agent={build({ channelAvailable: false })} />,
    );

    expect(screen.getByText("0.61.100")).toBeInTheDocument();
    expect(screen.getByText("0.61.121")).toBeInTheDocument();
  });

  it("does not claim a published release when the reference came from this fleet", () => {
    renderWithProviders(
      <AgentUpdateNotice
        agent={build({ referenceSource: "fleet", channelAvailable: false })}
      />,
    );

    expect(
      screen.getByText(/Another site in this fleet already reports/),
    ).toBeInTheDocument();
    expect(screen.queryByText(/newer published agent version/)).not.toBeInTheDocument();
  });

  // ── The honesty rule ─────────────────────────────────────────────────────

  it("carries the shipped fleet-derived caveat verbatim for a fleet-derived 'current'", () => {
    renderWithProviders(
      <AgentUpdateNotice
        agent={build({
          status: "current",
          version: "0.61.121",
          referenceSource: "fleet",
        })}
      />,
    );

    // The exact wording already shipped on the Sites table's Agent column, so
    // the same caveat has one phrasing across the app rather than two.
    expect(screen.getByText(AGENT_FLEET_NOTE_BODY)).toBeInTheDocument();
    expect(
      screen.getByText(/This site runs the newest agent version/),
    ).toBeInTheDocument();
  });

  it("states plainly that it cannot tell, rather than guessing, when there is no reference version", () => {
    renderWithProviders(
      <AgentUpdateNotice
        agent={build({
          status: "unknown",
          version: "0.61.100",
          latestVersion: "unknown",
          referenceSource: "none",
          channelAvailable: true,
        })}
      />,
    );

    expect(
      screen.getByText(/no reference agent version for this install/),
    ).toBeInTheDocument();
    // No false "behind", and no channel link for a comparison that never ran.
    expect(
      screen.queryByText(/newer published agent version/),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "Open agent updates" }),
    ).not.toBeInTheDocument();
    // The placeholder must never be rendered as though it were a version.
    expect(screen.queryByText("unknown")).not.toBeInTheDocument();
  });

  it("distinguishes a site that never reported a readable version from a missing reference", () => {
    renderWithProviders(
      <AgentUpdateNotice
        agent={build({
          status: "unknown",
          version: "",
          referenceSource: "published",
          channelAvailable: true,
        })}
      />,
    );

    expect(
      screen.getByText(/has not reported a readable agent version/),
    ).toBeInTheDocument();
  });

  it("never points an ineligible build at a channel it cannot consume", () => {
    renderWithProviders(
      <AgentUpdateNotice
        agent={build({
          status: "ineligible",
          version: "0.61.100",
          channelAvailable: true,
        })}
      />,
    );

    expect(
      screen.getByText(/WordPress plugin directory build of the agent/),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("link", { name: "Open agent updates" }),
    ).not.toBeInTheDocument();
  });

  it("always states that the agent is outside this tab's update runs, even when current", () => {
    renderWithProviders(
      <AgentUpdateNotice
        agent={build({ status: "current", version: "0.61.121" })}
      />,
    );

    expect(
      screen.getByText(/never part of an update run started here/),
    ).toBeInTheDocument();
  });

  it("renders nothing when the classification is unavailable (loading, 403, or no row for this site)", () => {
    const { container } = renderWithProviders(<AgentUpdateNotice agent={null} />);
    expect(container).toBeEmptyDOMElement();
  });
});
