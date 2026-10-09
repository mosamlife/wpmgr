import { describe, it, expect, vi } from "vitest";
import { act, render, screen, fireEvent } from "@testing-library/react";

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

  // The same `offered` handling as the site-tools box: a row the server did not
  // offer to this app is disabled and shown clear, so what the box shows ticked
  // is what the approval sends. A tick the host still holds for it is not shown.
  describe("when the server did not offer the cache clear to this app", () => {
    it("is disabled and clear, with a note that says what is true, even if the host still holds a tick", () => {
      render(<CachePurgeCapabilityBox checked={true} onChange={() => {}} offered={false} />);
      const box = screen.getByRole("checkbox");
      expect(box).toBeDisabled();
      expect(box).not.toBeChecked();
      // The box only appears when the app did ask for the cache clear, so the
      // old wording "Not requested by this app" was untrue wherever it showed.
      // What did not happen is that WPMgr offered it.
      const note = screen.getByTestId("cache-purge-not-offered");
      expect(note).toHaveTextContent("WPMgr did not offer cache clearing for this connection.");
      expect(note).not.toHaveTextContent(/requested/i);
    });

    it("reports nothing when it is clicked the way a browser delivers a click", () => {
      // fireEvent.click dispatches straight to a disabled checkbox and jsdom
      // toggles it, which a browser never does; the element's own click()
      // honours `disabled`. The same call on an offered box is the positive
      // control.
      const offeredChange = vi.fn();
      const view = render(<CachePurgeCapabilityBox checked={false} onChange={offeredChange} />);
      act(() => {
        screen.getByRole("checkbox").click();
      });
      expect(offeredChange).toHaveBeenCalledWith(true);
      view.unmount();

      const onChange = vi.fn();
      render(<CachePurgeCapabilityBox checked={false} onChange={onChange} offered={false} />);
      act(() => {
        screen.getByRole("checkbox").click();
      });
      expect(onChange).not.toHaveBeenCalled();
    });

    it("says nothing about being unavailable when it is offered, which is the default", () => {
      render(<CachePurgeCapabilityBox checked={true} onChange={() => {}} />);
      expect(screen.getByRole("checkbox")).toBeChecked();
      expect(screen.getByRole("checkbox")).toBeEnabled();
      expect(screen.queryByTestId("cache-purge-not-offered")).toBeNull();
    });
  });
});
