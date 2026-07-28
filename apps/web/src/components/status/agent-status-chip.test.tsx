import { describe, it, expect } from "vitest";
import type { ReactElement } from "react";
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

// GH #255 follow-up: in the Sites table the per-row status WORD is what
// clipped the Agent column into its neighbours, so the compact variant
// renders an icon plus the version. The state must survive that: never by
// colour alone (the icon differs in shape too) and never lost to assistive
// tech (visually-hidden text carries the full announcement). These pin the
// ACCESSIBLE NAME, not the visible digits.

function renderInCell(ui: ReactElement) {
  return render(
    <table>
      <tbody>
        <tr>
          <td>{ui}</td>
        </tr>
      </tbody>
    </table>,
  );
}

describe("AgentStatusChip (compact)", () => {
  it("renders the version without the status word", () => {
    renderInCell(
      <AgentStatusChip compact status="outdated" version="0.61.96" />,
    );
    expect(screen.getByText("0.61.96")).toBeInTheDocument();
    expect(screen.queryByText("Outdated")).not.toBeInTheDocument();
  });

  it.each([
    ["current", "published", "Current, agent 0.61.96"],
    ["current", "fleet", "Current in fleet, agent 0.61.96"],
    ["outdated", "published", "Outdated, agent 0.61.96"],
    ["unknown", "published", "Unknown, agent 0.61.96"],
    ["ineligible", "published", "Not self-updating, agent 0.61.96"],
  ] as const)(
    "announces %s/%s as its accessible name",
    (status, referenceSource, expected) => {
      renderInCell(
        <AgentStatusChip
          compact
          status={status}
          version="0.61.96"
          referenceSource={referenceSource}
        />,
      );
      expect(screen.getByRole("cell")).toHaveAccessibleName(expected);
    },
  );

  it("names the missing-version case rather than announcing a bare dash", () => {
    renderInCell(<AgentStatusChip compact status="unknown" version={null} />);
    expect(screen.getByRole("cell")).toHaveAccessibleName(
      "Unknown, agent version not reported",
    );
  });

  it("distinguishes the four states by icon shape, not colour alone", () => {
    const seen = new Set<string>();
    for (const status of ["current", "outdated", "unknown", "ineligible"] as const) {
      const { container, unmount } = renderInCell(
        <AgentStatusChip compact status={status} version="0.61.96" />,
      );
      const icon = container.querySelector("svg");
      expect(icon, `${status} renders no icon`).not.toBeNull();
      // lucide stamps the icon name onto the svg class list.
      const shape = icon!.getAttribute("class") ?? "";
      expect(seen.has(shape), `${status} reuses another status' icon`).toBe(
        false,
      );
      seen.add(shape);
      unmount();
    }
    expect(seen.size).toBe(4);
  });
});
