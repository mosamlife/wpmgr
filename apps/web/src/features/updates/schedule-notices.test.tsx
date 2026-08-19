import { describe, it, expect, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { ScheduledRunNotice } from "./schedule-notices";

// GH #463 — the cancel button on a scheduled run. `handler.go:59` /
// `cancelScheduledUpdateRun` (openapi.yaml:6420) landed while this component
// was being built, so `ScheduledRunNotice` shipped rendering NO button at all
// when `onCancel` is absent (see the component's own doc comment). This file
// pins the two behaviours that make the button "actually work" once the
// facade re-exports the operation and a caller wires it up:
//
//   1. The button appears ONLY when the caller supplies `onCancel` — the
//      route gates that on role, mirroring the server's operator+ gate.
//   2. Clicking it calls `onCancel` DIRECTLY, with no confirmation step.
//      Cancelling a scheduled run is the safe direction (it stops work that
//      has not started and contacts no site), so a confirm dialog here would
//      only train operators to click through confirms — which is exactly
//      what makes the dangerous ones (scheduling a fleet-wide update)
//      ineffective. See the component's doc comment on `onCancel`.

const SCHEDULED_AT = "2026-08-20T02:00:00Z";

describe("ScheduledRunNotice", () => {
  it("renders no Cancel button when onCancel is omitted (e.g. viewer role)", () => {
    renderWithProviders(<ScheduledRunNotice scheduledAt={SCHEDULED_AT} />);
    expect(screen.queryByRole("button", { name: /cancel/i })).toBeNull();
  });

  it("renders a Cancel button when onCancel is supplied, and clicking it calls onCancel immediately — no confirm dialog", () => {
    const onCancel = vi.fn();
    renderWithProviders(
      <ScheduledRunNotice scheduledAt={SCHEDULED_AT} onCancel={onCancel} />,
    );
    const button = screen.getByRole("button", { name: /cancel/i });
    fireEvent.click(button);
    // Called on the FIRST click, not after any intermediate confirm step.
    expect(onCancel).toHaveBeenCalledTimes(1);
    // No dialog role should have appeared anywhere in the document — a
    // confirm step would render one.
    expect(screen.queryByRole("dialog")).toBeNull();
    expect(screen.queryByRole("alertdialog")).toBeNull();
  });

  it("disables the Cancel button while a cancel is in flight", () => {
    const onCancel = vi.fn();
    renderWithProviders(
      <ScheduledRunNotice
        scheduledAt={SCHEDULED_AT}
        onCancel={onCancel}
        cancelPending
      />,
    );
    expect(screen.getByRole("button", { name: /cancel/i })).toBeDisabled();
  });
});
