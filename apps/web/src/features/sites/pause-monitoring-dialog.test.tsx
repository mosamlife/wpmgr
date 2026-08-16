import { describe, it, expect, vi } from "vitest";
import { screen, fireEvent } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { PauseMonitoringDialog } from "./pause-monitoring-dialog";

// GH #414 phase 4b — the confirmation dialog. The assertion that matters most
// here is the scope: someone pausing before a migration reasonably assumes
// EVERYTHING stops, and backups silently stopping is the one failure people do
// not recover from. If this test is ever deleted, the sentence goes with it.

describe("PauseMonitoringDialog", () => {
  // GH #414 adversarial-review finding 1 — the two constants were swapped once
  // already (backups/update-checks/scans reported as STOPPING, uptime
  // checks/alerts reported as CONTINUING — the exact inversion). The original
  // assertions here used unanchored `screen.getByText(/…/)`, which matches
  // whichever paragraph holds the string regardless of which label ("Stops:"
  // vs "Keeps running:") sits in front of it, so a swap of
  // MONITORING_PAUSE_STOPS/MONITORING_PAUSE_CONTINUES at the call site slid
  // straight past every assertion below, `not.toContain` included, because
  // those ran against whichever paragraph the regex happened to find. Anchor
  // to the labelled `<p>` instead: find the "Stops:"/"Keeps running:" label,
  // then require the DOM to hold that string INSIDE that specific paragraph.
  it("puts the CONTINUES sentence (backups etc.) under the 'Keeps running:' label", () => {
    renderWithProviders(
      <PauseMonitoringDialog
        open
        onClose={() => {}}
        onConfirm={() => {}}
        count={3}
      />,
    );
    const continuesLabel = screen.getByText("Keeps running:");
    const continuesParagraph = continuesLabel.closest("p");
    expect(continuesParagraph).not.toBeNull();
    expect(continuesParagraph).toHaveTextContent(
      /Backups, connection tracking, update checks and scans keep running/,
    );
  });

  it("puts the STOPS sentence (uptime checks/alerts, SCHEDULED screenshots/rescans) under the 'Stops:' label, and it never lists backups, update checks or scans", () => {
    renderWithProviders(
      <PauseMonitoringDialog
        open
        onClose={() => {}}
        onConfirm={() => {}}
        count={3}
      />,
    );
    const stopsLabel = screen.getByText("Stops:");
    const stopsParagraph = stopsLabel.closest("p");
    expect(stopsParagraph).not.toBeNull();
    expect(stopsParagraph).toHaveTextContent(
      /Uptime checks, uptime alerts, scheduled screenshots and scheduled vulnerability rescans stop/,
    );
    const stopsText = stopsParagraph?.textContent ?? "";
    expect(stopsText.toLowerCase()).not.toContain("backup");
    expect(stopsText).not.toContain("update checks");
    expect(stopsText.toLowerCase()).not.toMatch(/\bscans\b/);
  });

  it("counts the sites it will touch in its title", () => {
    renderWithProviders(
      <PauseMonitoringDialog
        open
        onClose={() => {}}
        onConfirm={() => {}}
        count={1}
      />,
    );
    expect(
      screen.getByText("Pause monitoring on 1 site"),
    ).toBeInTheDocument();
  });

  it("passes the typed reason to onConfirm", () => {
    const onConfirm = vi.fn();
    renderWithProviders(
      <PauseMonitoringDialog
        open
        onClose={() => {}}
        onConfirm={onConfirm}
        count={2}
      />,
    );
    fireEvent.change(screen.getByLabelText("Reason (optional)"), {
      target: { value: "  Migrating to the new host  " },
    });
    fireEvent.click(screen.getByRole("button", { name: "Pause monitoring" }));
    expect(onConfirm).toHaveBeenCalledWith("Migrating to the new host");
  });

  it("renders a request error in an alert rather than swallowing it", () => {
    renderWithProviders(
      <PauseMonitoringDialog
        open
        onClose={() => {}}
        onConfirm={() => {}}
        count={2}
        errorMessage="Your session has expired. Sign in again, then retry the change."
      />,
    );
    expect(screen.getByRole("alert")).toHaveTextContent(
      "Your session has expired.",
    );
  });

  it("offers a non-destructive way out", () => {
    const onClose = vi.fn();
    renderWithProviders(
      <PauseMonitoringDialog
        open
        onClose={onClose}
        onConfirm={() => {}}
        count={2}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Keep monitoring" }));
    expect(onClose).toHaveBeenCalled();
  });
});
