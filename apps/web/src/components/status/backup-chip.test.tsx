import { describe, it, expect } from "vitest";
import type { ReactElement } from "react";
import { render, screen } from "@testing-library/react";

import { BackupChip } from "./backup-chip";

// GH #255 follow-up: in the Sites table the chip sat in a column already
// headed "Backup", so "Backed up 10h ago" said it twice and wrapped onto two
// lines on nearly every row, which is what pushed the neighbouring columns
// off screen. `compact` drops the redundant word from the SCREEN only: it
// still has to be announced, and a failed backup still has to shout.

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

describe("BackupChip", () => {
  it("keeps the full label by default (standalone surfaces)", () => {
    render(<BackupChip status="success" time="10h ago" />);
    expect(screen.getByText("Backed up 10h ago")).toBeInTheDocument();
  });

  it("compact success shows only the relative time on screen", () => {
    renderInCell(<BackupChip compact status="success" time="10h ago" />);
    // The visible fragment is the bare time; the redundant word survives
    // only in visually-hidden text.
    expect(screen.getByText("10h ago").className).not.toContain("sr-only");
    expect(screen.getByText("Backed up 10h ago").className).toContain(
      "sr-only",
    );
  });

  it("compact success still announces what happened", () => {
    renderInCell(<BackupChip compact status="success" time="10h ago" />);
    expect(screen.getByRole("cell")).toHaveAccessibleName("Backed up 10h ago");
  });

  it("compact success with no time keeps the full label rather than rendering empty", () => {
    renderInCell(<BackupChip compact status="success" />);
    expect(screen.getByText("Backed up")).toBeInTheDocument();
  });

  it("compact failure keeps its visible word and its destructive palette", () => {
    renderInCell(<BackupChip compact status="failed" />);
    // The word stays on screen: a failed backup is the one row an operator
    // must not skim past.
    expect(screen.getByText("Failed")).toBeInTheDocument();
    expect(screen.getByRole("cell")).toHaveAccessibleName("Backup failed");
    const chip = screen.getByText("Failed").parentElement;
    expect(chip?.className).toContain("bg-destructive-subtle");
  });

  it("compact running announces the state and keeps the percentage", () => {
    renderInCell(
      <BackupChip compact status="running" progressPercent={38} />,
    );
    expect(screen.getByText("Running")).toBeInTheDocument();
    expect(screen.getByRole("cell")).toHaveAccessibleName("Backup running 38%");
  });
});
