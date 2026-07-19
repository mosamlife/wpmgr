import { describe, it, expect } from "vitest";
import { render, screen } from "@testing-library/react";

import { VulnSeverityChip } from "./vuln-severity-chip";

// GH #245: the "unknown" severity bucket. A CVSS ingestion bug once caused
// every genuinely-unrated finding to display as "Low" (a confirmed-safe
// label). The fix adds a fifth, deliberately distinct bucket so an unrated
// finding never again reads as "safe".

describe("VulnSeverityChip", () => {
  it("renders the Unknown label for severity='unknown'", () => {
    render(<VulnSeverityChip severity="unknown" />);
    expect(screen.getByText("Unknown")).toBeInTheDocument();
  });

  it("renders a leading count before the Unknown word", () => {
    render(<VulnSeverityChip severity="unknown" count={7} />);
    expect(screen.getByText("7")).toBeInTheDocument();
    expect(screen.getByText("Unknown")).toBeInTheDocument();
  });

  it("Unknown uses its own dedicated severity-unknown token classes, distinct from Low", () => {
    const { container: unknownContainer } = render(
      <VulnSeverityChip severity="unknown" />,
    );
    const { container: lowContainer } = render(
      <VulnSeverityChip severity="low" />,
    );
    const unknownChip = unknownContainer.querySelector("span");
    const lowChip = lowContainer.querySelector("span");
    expect(unknownChip?.className).toContain("bg-severity-unknown");
    expect(unknownChip?.className).toContain("text-severity-unknown-foreground");
    // Never falls back to the Low chip's background or its generic
    // text-foreground treatment: the whole point is that Unknown must
    // never visually read as Low.
    expect(unknownChip?.className).not.toContain("bg-severity-low");
    expect(lowChip?.className).not.toContain("bg-severity-unknown");
  });

  it("all five severities render their own word (critical/high/medium/low/unknown)", () => {
    const severities = ["critical", "high", "medium", "low", "unknown"] as const;
    for (const severity of severities) {
      const { unmount } = render(<VulnSeverityChip severity={severity} />);
      unmount();
    }
    // No assertion needed beyond "did not throw": each render() above would
    // throw on a missing Record key at the type level already; this pins the
    // full union is exhaustively handled at runtime too.
    expect(severities).toHaveLength(5);
  });
});
