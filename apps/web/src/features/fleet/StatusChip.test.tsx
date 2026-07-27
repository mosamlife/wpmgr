import { describe, it, expect } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { StatusChip } from "./StatusChip";

// GH #291 — a page-cached site whose PHP backend is completely dead used to
// render a bare green "Up" chip. The control plane now derives "degraded"
// AND sends a `status_reason` code; the fix is that the chip explains
// itself instead of leaving the operator to guess. These assertions are
// non-vacuous: a regression that drops the reason lookup (or renders the
// note for every status, or renders it for an unrecognised code) fails one
// of these.

describe("StatusChip", () => {
  it("renders the plain-language explanation for a degraded item with a known reason", () => {
    renderWithProviders(
      <StatusChip status="degraded" reason="agent_unreachable" />,
    );

    expect(screen.getByText("Degraded")).toBeInTheDocument();
    expect(
      screen.getByText(
        "Serving visitors, but the site agent is not responding. Cached pages may be masking a broken backend.",
      ),
    ).toBeInTheDocument();
  });

  it("renders the agent_degraded explanation for a degraded item", () => {
    renderWithProviders(<StatusChip status="degraded" reason="agent_degraded" />);

    expect(
      screen.getByText(
        "Serving visitors, but the site agent is late checking in.",
      ),
    ).toBeInTheDocument();
  });

  it("renders the slow_response explanation for a degraded item", () => {
    renderWithProviders(<StatusChip status="degraded" reason="slow_response" />);

    expect(screen.getByText("Responding slowly.")).toBeInTheDocument();
  });

  it("renders no explanation for a degraded item with no reason", () => {
    renderWithProviders(<StatusChip status="degraded" />);

    expect(screen.getByText("Degraded")).toBeInTheDocument();
    expect(
      screen.queryByText(
        "Serving visitors, but the site agent is not responding. Cached pages may be masking a broken backend.",
      ),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText(/Serving visitors|Responding slowly/),
    ).not.toBeInTheDocument();
  });

  it("renders no explanation for an unrecognised reason code (future-proofing)", () => {
    renderWithProviders(
      <StatusChip status="degraded" reason="some_future_reason_code" />,
    );

    expect(screen.getByText("Degraded")).toBeInTheDocument();
    expect(
      screen.queryByText(/Serving visitors|Responding slowly/),
    ).not.toBeInTheDocument();
  });

  it("never renders an explanation for a non-degraded status, even if a reason is present", () => {
    renderWithProviders(<StatusChip status="up" reason="agent_unreachable" />);

    expect(screen.getByText("Up")).toBeInTheDocument();
    expect(
      screen.queryByText(/Serving visitors|Responding slowly/),
    ).not.toBeInTheDocument();
  });
});
