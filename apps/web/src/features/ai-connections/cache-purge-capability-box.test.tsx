import { describe, it, expect, vi } from "vitest";
import { render, screen, fireEvent } from "@testing-library/react";

import { CachePurgeCapabilityBox, NO_UNGATED_WRITE_TIER_NOTE } from "./cache-purge-capability-box";

// Shared between the wizard's step 4 and the consent screen (design v7
// S2.1/S2.2), so its own behaviours are pinned once here instead of twice
// through whichever screen happens to mount it.

describe("CachePurgeCapabilityBox", () => {
  it("renders unticked when the caller says it is unticked, never pre-ticked on its own", () => {
    render(<CachePurgeCapabilityBox checked={false} onChange={() => {}} />);
    expect(screen.getByRole("checkbox")).not.toBeChecked();
  });

  it("reports a tick to the caller rather than flipping its own state silently", () => {
    const onChange = vi.fn();
    render(<CachePurgeCapabilityBox checked={false} onChange={onChange} />);
    fireEvent.click(screen.getByRole("checkbox"));
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith(true);
  });

  it("stays unticked on screen until the caller's own `checked` prop says otherwise", () => {
    // A stray internal state bit here would tick the box on click regardless
    // of what the parent decided, which is exactly the shape that would let
    // a write capability get granted without the parent's own gate agreeing.
    const { rerender } = render(<CachePurgeCapabilityBox checked={false} onChange={() => {}} />);
    fireEvent.click(screen.getByRole("checkbox"));
    expect(screen.getByRole("checkbox")).not.toBeChecked();
    rerender(<CachePurgeCapabilityBox checked={true} onChange={() => {}} />);
    expect(screen.getByRole("checkbox")).toBeChecked();
  });

  it("hides the hosting-cache disclosure until it is asked for", () => {
    render(<CachePurgeCapabilityBox checked={false} onChange={() => {}} />);
    expect(screen.queryByTestId("cache-purge-disclosure")).not.toBeInTheDocument();
    fireEvent.click(screen.getByTestId("cache-purge-disclosure-toggle"));
    expect(screen.getByTestId("cache-purge-disclosure")).toBeInTheDocument();
  });

  it("does not tick the capability when the disclosure toggle is pressed", () => {
    // The toggle sits inside the same <label> as the checkbox; without
    // preventDefault a click on it would bubble into a second, phantom tick.
    const onChange = vi.fn();
    render(<CachePurgeCapabilityBox checked={false} onChange={onChange} />);
    fireEvent.click(screen.getByTestId("cache-purge-disclosure-toggle"));
    expect(onChange).not.toHaveBeenCalled();
  });

  it("restores the closing hint verbatim: no ungated write tier exists", () => {
    render(<CachePurgeCapabilityBox checked={false} onChange={() => {}} />);
    expect(screen.getByText(NO_UNGATED_WRITE_TIER_NOTE)).toBeInTheDocument();
  });

  it("disables the checkbox when the caller disables the row", () => {
    render(<CachePurgeCapabilityBox checked={false} onChange={() => {}} disabled />);
    expect(screen.getByRole("checkbox")).toBeDisabled();
  });
});
