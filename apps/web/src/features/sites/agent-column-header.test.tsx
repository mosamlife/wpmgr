import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import type { AgentMirrorStatus } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

// The popover's "Check now" action calls the SHARED mutation hook, so these
// tests mock the generated SDK operation and the toast surface rather than the
// hook: what must be proven is that this surface produces the SAME outcome
// vocabulary as the admin console page, not that it calls something named
// correctly.
const { checkAgentMirrorNowMock, toastSuccess, toastError, toastInfo } =
  vi.hoisted(() => ({
    checkAgentMirrorNowMock: vi.fn(),
    toastSuccess: vi.fn(),
    toastError: vi.fn(),
    toastInfo: vi.fn(),
  }));

vi.mock("@wpmgr/api", async (importOriginal) => ({
  ...(await importOriginal<Record<string, unknown>>()),
  checkAgentMirrorNow: checkAgentMirrorNowMock,
}));

vi.mock("@/components/toast", () => ({
  toast: {
    success: toastSuccess,
    error: toastError,
    info: toastInfo,
    warning: vi.fn(),
  },
}));

import {
  AgentColumnFleetNote,
  AGENT_FLEET_NOTE_LABEL,
  AGENT_FLEET_NOTE_TITLE,
} from "./agent-column-header";

beforeEach(() => {
  checkAgentMirrorNowMock.mockReset();
  toastSuccess.mockReset();
  toastError.mockReset();
  toastInfo.mockReset();
});

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
// The manual "Check now" trigger IS now part of this component, but only for a
// viewer the control plane says may use it. This file used to assert the
// opposite, on the grounds that the action was superadmin only while a
// superadmin cannot open the Sites page at all; the endpoint has since been
// widened to the owner of the only live organisation on an install, and an
// owner is never redirected off this page. What must not be lost is that the
// visibility is the SERVER's answer (agent_mirror.can_check_now) and never a
// role guessed in the browser, so the button cannot appear for someone the
// endpoint would answer 403.

const hoursAgo = (h: number) => new Date(Date.now() - h * 3_600_000).toISOString();

function mirror(overrides: Partial<Record<string, unknown>> = {}): AgentMirrorStatus {
  return {
    enabled: true,
    status: "ok",
    stale_after_seconds: 46_800,
    can_check_now: false,
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

});

// ---------------------------------------------------------------------------
// GH #322 follow-up: the "Check now" action
// ---------------------------------------------------------------------------
//
// WHICH OF THESE FAIL AGAINST THE PRE-CHANGE CODE:
//
//	"renders a Check now action when can_check_now is true"   FAILS (no button existed)
//	"stale, warn-tier state still offers the action"          FAILS (no button existed)
//	"clicking it queues a check ..."                          FAILS (nothing to click)
//	"409 ... INFO" / "429 ... INFO" / "403 ... error"         FAIL (nothing to click)
//	the three ABSENCE cases                                   pass either way, and are
//	  kept precisely because they are the containment half: they are what would
//	  catch the button being shown to someone the endpoint refuses. The
//	  pre-change file asserted the FIRST of them unconditionally.

const CHECK_NOW = { name: /check now/i } as const;

/** Opens the popover, whatever tier its trigger is currently in. */
function openPopover() {
  fireEvent.click(
    screen.getByRole("button", {
      name: new RegExp(`^${AGENT_FLEET_NOTE_LABEL}`),
    }),
  );
}

describe("AgentColumnFleetNote - the Check now action", () => {
  // --- T6: present only when the server says so -----------------------------

  it("renders a Check now action when can_check_now is true", () => {
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ can_check_now: true })}
      />,
    );
    openPopover();
    expect(screen.getByRole("button", CHECK_NOW)).toBeInTheDocument();
  });

  it("renders no Check now action when can_check_now is false", () => {
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ can_check_now: false })}
      />,
    );
    openPopover();
    expect(screen.queryByRole("button", CHECK_NOW)).not.toBeInTheDocument();
  });

  // An install running an older control plane sends no can_check_now at all.
  // Absent must read as "not permitted", never as a button whose endpoint
  // would refuse it.
  it("renders no Check now action when the field is missing entirely", () => {
    const legacy = mirror();
    delete (legacy as unknown as Record<string, unknown>).can_check_now;
    renderWithProviders(
      <AgentColumnFleetNote referenceSource="published" referenceCheck={legacy} />,
    );
    openPopover();
    expect(screen.queryByRole("button", CHECK_NOW)).not.toBeInTheDocument();
  });

  // The mirror being off is the server's business (it forces can_check_now
  // false), but this pins the client half: a disabled mirror with a published
  // reference renders nothing at all, so there is no popover to put a button
  // in even if the flag were wrong.
  it("renders nothing at all, button included, when the mirror is disabled and the reference is published", () => {
    const { container } = renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ enabled: false, status: "disabled", can_check_now: false })}
      />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  // The reporter's exact scenario: the moment an operator reads "may be stale"
  // is the moment they want to act, so the action must survive the warn tier.
  it("offers the action in the stale, warn-tier state the reporter described", () => {
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ status: "stale", can_check_now: true })}
      />,
    );
    openPopover();
    expect(screen.getByText(/^Reference checked /)).toBeInTheDocument();
    expect(screen.getByRole("button", CHECK_NOW)).toBeInTheDocument();
  });

  // --- T7: the outcome vocabulary is the admin console's, not a second one --

  it("clicking it queues a check and reports the control plane's own message", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: {
        status: "queued",
        queued_at: "2026-08-05T10:00:00Z",
        message: "A mirror run has been queued.",
      },
      error: undefined,
      response: { status: 202 },
    });
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ can_check_now: true })}
      />,
    );
    openPopover();
    fireEvent.click(screen.getByRole("button", CHECK_NOW));

    await waitFor(() =>
      expect(toastSuccess).toHaveBeenCalledWith("A mirror run has been queued."),
    );
    expect(checkAgentMirrorNowMock).toHaveBeenCalledTimes(1);
    expect(toastError).not.toHaveBeenCalled();
  });

  it("409 already running is an INFO toast here too, never an error", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "agent_mirror_check_in_flight",
        message: "a mirror check is already queued or running",
      },
      response: { status: 409 },
    });
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ can_check_now: true })}
      />,
    );
    openPopover();
    fireEvent.click(screen.getByRole("button", CHECK_NOW));

    await waitFor(() =>
      expect(toastInfo).toHaveBeenCalledWith(
        "A check is already running. Its result will appear on the fleet agent view when it finishes.",
      ),
    );
    expect(toastError).not.toHaveBeenCalled();
  });

  it("429 rate limited is an INFO toast here too: being skipped by the 30 minute spacing is the system working", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: {
        code: "agent_mirror_rate_limited",
        message: "the upstream release was last requested 12m ago",
        details: { retry_after_seconds: 480 },
      },
      response: { status: 429 },
    });
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ status: "stale", can_check_now: true })}
      />,
    );
    openPopover();
    fireEvent.click(screen.getByRole("button", CHECK_NOW));

    await waitFor(() =>
      expect(toastInfo).toHaveBeenCalledWith(
        "Not checked. The mirror must wait 8 minutes before its next upstream request. The scheduled check still runs.",
      ),
    );
    expect(toastError).not.toHaveBeenCalled();
  });

  // The one case that IS an error, so the three above are not merely "this
  // surface never shows an error toast".
  it("a 403 is a real error toast, so the INFO cases above are a distinction and not a blanket rule", async () => {
    checkAgentMirrorNowMock.mockResolvedValue({
      data: undefined,
      error: { code: "superadmin_required", message: "superadmin access required" },
      response: { status: 403 },
    });
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ can_check_now: true })}
      />,
    );
    openPopover();
    fireEvent.click(screen.getByRole("button", CHECK_NOW));

    await waitFor(() =>
      expect(toastError).toHaveBeenCalledWith("superadmin access required"),
    );
    expect(toastInfo).not.toHaveBeenCalled();
  });

  // The action must not promise a result it has not got: the endpoint answers
  // 202 queued, and the run has not happened yet.
  it("says the result appears on refresh rather than implying the check already ran", () => {
    renderWithProviders(
      <AgentColumnFleetNote
        referenceSource="published"
        referenceCheck={mirror({ can_check_now: true })}
      />,
    );
    openPopover();
    expect(
      screen.getByText(/result appears here once this view refreshes/i),
    ).toBeInTheDocument();
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
