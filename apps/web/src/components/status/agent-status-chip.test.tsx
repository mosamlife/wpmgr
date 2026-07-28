import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { AgentStatusChip } from "./agent-status-chip";

// Adversarial-review finding: a "current" chip rendered identically whether
// its reference version came from a published release or was merely the
// newest agent this fleet happens to have seen, so a site could sit dozens
// of releases behind the real latest and still show an unqualified green
// "Current". These pin the exact copy for both cases so that overclaim
// cannot silently come back.

describe("AgentStatusChip", () => {
  it("renders a bare Current for a published reference", () => {
    render(
      <AgentStatusChip status="current" version="0.12.0" referenceSource="published" />,
    );
    expect(screen.getByText("Current")).toBeInTheDocument();
    expect(screen.queryByText("Current in fleet")).not.toBeInTheDocument();
  });

  it("relabels a fleet-derived Current instead of asserting it unqualified", () => {
    render(
      <AgentStatusChip status="current" version="0.12.0" referenceSource="fleet" />,
    );
    expect(screen.getByText("Current in fleet")).toBeInTheDocument();
    expect(screen.queryByText(/^Current$/)).not.toBeInTheDocument();
  });

  it("attaches a title explaining the fleet reference on the qualified chip", () => {
    render(
      <AgentStatusChip status="current" version="0.12.0" referenceSource="fleet" />,
    );
    const chip = screen.getByText("Current in fleet").closest("span[title]");
    expect(chip?.getAttribute("title")).toBe(
      "Agent 0.12.0, current in fleet (newest seen in this fleet, not a published release)",
    );
  });

  it("does not qualify outdated/unknown/ineligible statuses regardless of source", () => {
    const statuses = ["outdated", "unknown", "ineligible"] as const;
    for (const status of statuses) {
      const { unmount } = render(
        <AgentStatusChip status={status} version="0.9.0" referenceSource="fleet" />,
      );
      expect(screen.queryByText(/in fleet/)).not.toBeInTheDocument();
      unmount();
    }
  });

  it("treats an absent referenceSource the same as published (no qualifier)", () => {
    render(<AgentStatusChip status="current" version="0.12.0" />);
    expect(screen.getByText("Current")).toBeInTheDocument();
    expect(screen.queryByText("Current in fleet")).not.toBeInTheDocument();
  });
});
