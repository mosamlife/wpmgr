import { describe, it, expect } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";

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

describe("AgentColumnFleetNote", () => {
  it("renders nothing when the reference version is a published release", () => {
    const { container } = render(
      <AgentColumnFleetNote referenceSource="published" />,
    );
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing while the rollup has not loaded", () => {
    const { container } = render(<AgentColumnFleetNote />);
    expect(container).toBeEmptyDOMElement();
  });

  it("renders nothing when there is no reference version at all", () => {
    const { container } = render(<AgentColumnFleetNote referenceSource="none" />);
    expect(container).toBeEmptyDOMElement();
  });

  it("offers a named, keyboard-reachable button for a fleet-derived reference", () => {
    render(<AgentColumnFleetNote referenceSource="fleet" />);
    const trigger = screen.getByRole("button", { name: AGENT_FLEET_NOTE_LABEL });
    // A real <button>, not a hover-only tooltip target: focusable, and
    // operable by Enter/Space without a pointer.
    expect(trigger.tagName).toBe("BUTTON");
    expect(trigger).toHaveAttribute("type", "button");
    trigger.focus();
    expect(document.activeElement).toBe(trigger);
  });

  it("explains the fleet-derived comparison when opened", () => {
    render(<AgentColumnFleetNote referenceSource="fleet" />);
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
});
