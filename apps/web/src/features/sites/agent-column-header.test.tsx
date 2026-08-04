import { describe, it, expect } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import type { AgentMirrorStatus } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import {
  AgentColumnFleetNote,
  AGENT_FLEET_NOTE_LABEL,
  AGENT_FLEET_NOTE_TITLE,
} from "./agent-column-header";

// GH #255 follow-up. The "in fleet" qualifier used to be appended to every
// row's Agent chip, which was honest but wrapped the cell onto two lines and
// pushed the version into the Updates column. It moved here, to the column
// header, where it is stated once. What must NOT be lost is the distinction
// itself: a self-hosted install whose reference version came from its own
// fleet has to keep being told so.
//
// GH #322 extends the same popover with a second, independent signal: how
// fresh the upstream agent-release mirror itself is (see
// agent-reference-check.ts for the copy states, mapped from the REAL
// control-plane contract, FleetAgentVersions.agent_mirror, typed
// AgentMirrorStatus). The two facts can both be true at once (a
// fleet-derived reference AND a stale mirror), so the tests below cover
// them both together and separately.
//
// The manual "Check now" trigger is NOT part of this component: it is a
// superadmin, install-level admin-console action
// (routes/_authed/admin/agent-mirror.tsx), not a Sites-page affordance (a
// superadmin cannot open the tenant-scoped Sites page at all, see
// routes/_authed.tsx's isSuperadminAllowedPath guard), so this popover is
// informational only.

const hoursAgo = (h: number) => new Date(Date.now() - h * 3_600_000).toISOString();

function mirror(overrides: Partial<Record<string, unknown>> = {}): AgentMirrorStatus {
  return {
    enabled: true,
    status: "ok",
    stale_after_seconds: 46_800,
    last_success_at: hoursAgo(3),
    last_success_outcome: "unchanged",
    last_success_version: "0.61.112",
    last_attempt_at: hoursAgo(3),
    last_attempt_outcome: "unchanged",
    last_attempt_detail: null,
    last_attempt_trigger: "periodic",
    last_mirrored_at: null,
    last_mirrored_version: null,
    ...overrides,
  } as unknown as AgentMirrorStatus;
}

describe("AgentColumnFleetNote", () => {
  it("renders nothing when the reference version is a published release and there is no mirror check", () => {
    const { container } = renderWithProviders(
      <AgentColumnFleetNote referenceSource="published" />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing while the rollup has not loaded", () => {
    const { container } = renderWithProviders(<AgentColumnFleetNote />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when there is no reference version at all and there is no mirror check", () => {
    const { container } = renderWithProviders(
      <AgentColumnFleetNote referenceSource="none" />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("offers a named, keyboard-reachable button for a fleet-derived reference", () => {
    renderWithProviders(<AgentColumnFleetNote referenceSource="fleet" />);
    const trigger = screen.getByRole("button", { name: AGENT_FLEET_NOTE_LABEL });
    // A real <button>, not a hover-only tooltip target: focusable, and
    // operable by Enter/Space without a pointer.
    expect(trigger.tagName).toBe("BUTTON");
    expect(trigger).toHaveAttribute("type", "button");
    trigger.focus();
    expect(document.activeElement).toBe(trigger);
  });

  it("explains the fleet-derived comparison when opened", () => {
    renderWithProviders(<AgentColumnFleetNote referenceSource="fleet" />);
    expect(screen.queryByText(AGENT_FLEET_NOTE_TITLE)).not.toBeInTheDocument();

    fireEvent.click(
      screen.getByRole("button", { name: AGENT_FLEET_NOTE_LABEL }),
    );

    expect(screen.getByText(AGENT_FLEET_NOTE_TITLE)).toBeInTheDocument();
    expect(
      screen.getByText(/newest agent version this fleet has reported/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/may still be behind a newer published build/i),
    ).toBeInTheDocument();
  });

  // -------------------------------------------------------------------------
  // GH #322: agent_mirror freshness
  // -------------------------------------------------------------------------

  it("renders the mirror-freshness message even when the reference is a published (non-fleet) release", () => {
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror()}
      />,
    );
    const trigger = screen.getByRole("button", { name: AGENT_FLEET_NOTE_LABEL });
    fireEvent.click(trigger);
    expect(screen.getByText(/^Reference checked /)).toBeInTheDocument();
    // The fleet note itself must NOT appear: the reference is published.
    expect(screen.queryByText(AGENT_FLEET_NOTE_TITLE)).not.toBeInTheDocument();
  });

  it("renders both the fleet note and the mirror-freshness message together, since they are independent facts", () => {
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="fleet"
        referenceCheck={mirror()}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: AGENT_FLEET_NOTE_LABEL }));
    expect(screen.getByText(AGENT_FLEET_NOTE_TITLE)).toBeInTheDocument();
    expect(screen.getByText(/^Reference checked /)).toBeInTheDocument();
  });

  it("renders nothing when disabled and the reference is published, since there is no periodic check to time", () => {
    const { container } = renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ enabled: false, status: "disabled" })}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("uses the unqualified label for a healthy (status ok) reference check", () => {
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ status: "ok" })}
      />,
    );
    // The aria-label stays exactly AGENT_FLEET_NOTE_LABEL (no appended
    // headline) when nothing is warn-tier.
    expect(
      screen.getByRole("button", { name: AGENT_FLEET_NOTE_LABEL }),
    ).toBeInTheDocument();
  });

  it("appends the state headline to the trigger's aria-label when the reference check is stale (warn tier)", () => {
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ status: "stale" })}
      />,
    );
    const trigger = screen.getByRole("button", {
      name: `${AGENT_FLEET_NOTE_LABEL}. Reference checked 3h ago.`,
    });
    expect(trigger).toBeInTheDocument();
  });

  it("never renders a Check now button: the manual trigger lives in the admin console, not on this page", () => {
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ status: "stale" })}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: /Reference checked/i }));
    expect(
      screen.queryByRole("button", { name: /check now/i }),
    ).not.toBeInTheDocument();
  });
});

describe("AgentColumnFleetNote - the warn heading is legible in dark mode", () => {
  // GH #322, reported with a screenshot: the popover heading rendered
  // near-black on the dark popover surface while the body text below it was
  // fine.
  //
  // It was not a missing dark override. The heading used
  // --warning-foreground, which is the text colour for content sitting ON a
  // --warning background, so it is near-black in BOTH themes and its dark
  // value is darker still (L 20% light, L 15% dark). The right token for
  // warning-tinted text on an ordinary surface is --warning-subtle-fg, which
  // inverts properly (L 38% light, L 86% dark).
  //
  // Asserting the token rather than a rendered colour is deliberate: jsdom
  // does not resolve CSS custom properties, so a computed-style assertion
  // here would pass against either token and prove nothing.
  it("uses the surface-text warning token, not the on-warning-background one", () => {
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({
          status: "never_succeeded",
          last_success_at: null,
          last_success_outcome: null,
          last_success_version: null,
        })}
      />,
    );
    // In the warn state the trigger's accessible name appends the title
    // (agent-column-header.tsx:95-96), so match the prefix.
    fireEvent.click(
      screen.getByRole("button", {
        name: new RegExp(`^${AGENT_FLEET_NOTE_LABEL}`),
      }),
    );

    const heading = screen.getByText("No check has ever succeeded");
    expect(heading.className).toContain("--color-warning-subtle-fg");
    // The token that made it invisible must not come back.
    expect(heading.className).not.toContain("--color-warning-foreground");
  });
});
