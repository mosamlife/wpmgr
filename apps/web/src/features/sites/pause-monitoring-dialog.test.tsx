import { describe, it, expect, vi } from "vitest";
import { screen, fireEvent } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { PauseMonitoringDialog } from "./pause-monitoring-dialog";

// GH #414 phase 4b — the confirmation dialog. The assertion that matters most
// here is the scope: someone pausing before a migration reasonably assumes
// EVERYTHING stops, and backups silently stopping is the one failure people do
// not recover from. If this test is ever deleted, the sentence goes with it.

describe("PauseMonitoringDialog", () => {
  it("names BACKUPS as continuing, in the body, before the button", () => {
    renderWithProviders(
      <PauseMonitoringDialog
        open
        onClose={() => {}}
        onConfirm={() => {}}
        count={3}
      />,
    );
    expect(
      screen.getByText(/Backups, connection tracking, update checks and scans keep running/),
    ).toBeInTheDocument();
  });

  it("names what stops (uptime checks/alerts, SCHEDULED screenshots/rescans), and never lists backups, update checks or scans among them", () => {
    renderWithProviders(
      <PauseMonitoringDialog
        open
        onClose={() => {}}
        onConfirm={() => {}}
        count={3}
      />,
    );
    const stops = screen.getByText(
      /Uptime checks, uptime alerts, scheduled screenshots and scheduled vulnerability rescans stop/,
    );
    expect(stops).toBeInTheDocument();
    expect(stops.textContent?.toLowerCase()).not.toContain("backup");
    expect(stops.textContent).not.toContain("update checks");
    expect(stops.textContent?.toLowerCase()).not.toMatch(/\bscans\b/);
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
