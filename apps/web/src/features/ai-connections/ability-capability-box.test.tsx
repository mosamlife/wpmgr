import { useState } from "react";
import { describe, it, expect, vi } from "vitest";
import { act, fireEvent, screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { AbilityCapabilityBox, type AbilityCapabilityBoxProps } from "./ability-capability-box";
import type { AbilityTicks } from "./capabilities";

// The site-tools box, on its own. "Ask for changes" (request) needs "see what
// the site can do" (read): a connection holding the request alone cannot call
// the tool that carries one. So the box keeps the pair together in two
// directions and reports BOTH rows in one onChange:
//
//   ticking the request ticks the read;
//   clearing the read clears the request;
//   clearing the request leaves the read; ticking the read leaves the request.
//
// Each test renders the box inside a small host that keeps the ticks the way the
// wizard and the consent screen do, so a click really moves the state and the
// next click starts from where the last one left it.

const BOTH: AbilityTicks = { read: true, request: true };
const READ_ONLY: AbilityTicks = { read: true, request: false };
const NEITHER: AbilityTicks = { read: false, request: false };

type HostProps = {
  readonly start: AbilityTicks;
  readonly onChange: (next: AbilityTicks) => void;
} & Pick<AbilityCapabilityBoxProps, "disabled" | "readOffered" | "requestOffered">;

function Host({ start, onChange, ...rest }: HostProps) {
  const [ticks, setTicks] = useState(start);
  return (
    <AbilityCapabilityBox
      readChecked={ticks.read}
      requestChecked={ticks.request}
      onChange={(next) => {
        onChange(next);
        setTicks(next);
      }}
      {...rest}
    />
  );
}

function renderBox(start: AbilityTicks, over: Partial<Omit<HostProps, "start" | "onChange">> = {}) {
  const onChange = vi.fn<(next: AbilityTicks) => void>();
  const view = renderWithProviders(<Host start={start} onChange={onChange} {...over} />);
  return { onChange, ...view };
}

/**
 * A click the way a browser delivers it. fireEvent.click dispatches straight to
 * the element, and jsdom then toggles even a DISABLED checkbox, which a browser
 * never does. The element's own click() honours `disabled`, so this is what the
 * tests use whenever the point is that a disabled control reports nothing, and
 * the same call on an enabled control is the positive control.
 */
function press(el: HTMLElement) {
  act(() => {
    el.click();
  });
}

const readBox = () => screen.getByTestId<HTMLInputElement>("ability-box-mcp.ability.read");
const requestBox = () => screen.getByTestId<HTMLInputElement>("ability-box-mcp.ability.request");
const shown = () => ({ read: readBox().checked, request: requestBox().checked });

// Written out in full, never taken from the component, so editing the copy
// there reddens this file.
const READ_LABEL = "See this site's tools and read its published pages";
const NEEDS_READ_HINT = `Needs “${READ_LABEL}”. Ticking this ticks that too, and clearing that clears this.`;
const NEEDS_READ_BLOCKED = `Needs “${READ_LABEL}”, which this app did not request.`;

describe("AbilityCapabilityBox, the request needs the read", () => {
  it("ticking ask for changes while see what the site can do is clear ticks both, in one change", () => {
    const { onChange } = renderBox(NEITHER);
    expect(shown()).toEqual(NEITHER);

    fireEvent.click(requestBox());

    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith(BOTH);
    expect(shown()).toEqual(BOTH);
  });

  it("ticking ask for changes beside a ticked read ticks the request and keeps the read", () => {
    const { onChange } = renderBox(READ_ONLY);
    fireEvent.click(requestBox());
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith(BOTH);
    expect(shown()).toEqual(BOTH);
  });

  it("clearing see what the site can do clears ask for changes too, in one change", () => {
    const { onChange } = renderBox(BOTH);
    fireEvent.click(readBox());
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith(NEITHER);
    expect(shown()).toEqual(NEITHER);
  });

  it("ticking the read again afterwards does not bring the request back", () => {
    const { onChange } = renderBox(BOTH);
    fireEvent.click(readBox());
    fireEvent.click(readBox());
    expect(onChange).toHaveBeenNthCalledWith(1, NEITHER);
    expect(onChange).toHaveBeenNthCalledWith(2, READ_ONLY);
    expect(shown()).toEqual(READ_ONLY);
  });

  it("clearing ask for changes alone leaves see what the site can do ticked", () => {
    const { onChange } = renderBox(BOTH);
    fireEvent.click(requestBox());
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith(READ_ONLY);
    expect(shown()).toEqual(READ_ONLY);
  });

  it("ticking see what the site can do alone leaves ask for changes clear", () => {
    const { onChange } = renderBox(NEITHER);
    fireEvent.click(readBox());
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith(READ_ONLY);
    expect(shown()).toEqual(READ_ONLY);
  });

  it("shows a request the host still holds as clear when its read is clear, and a click ticks both", () => {
    // A state the box never produces itself: the host holds the request ticked
    // and the read clear. What is shown is what an approval would send, so the
    // request is clear, and ticking it brings the read with it.
    const { onChange } = renderBox({ read: false, request: true });
    expect(shown()).toEqual(NEITHER);
    expect(onChange).not.toHaveBeenCalled();

    fireEvent.click(requestBox());
    expect(onChange).toHaveBeenCalledWith(BOTH);
    expect(shown()).toEqual(BOTH);
  });

  it("says, on the request row, what it needs and how the two move together", () => {
    renderBox(NEITHER);
    expect(screen.getByTestId("ability-request-needs-read")).toHaveTextContent(NEEDS_READ_HINT);
    expect(screen.queryByTestId("ability-request-blocked")).toBeNull();
  });
});

describe("AbilityCapabilityBox, what the app did not ask for", () => {
  it("disables ask for changes, with the reason, when the read it needs is not on offer", () => {
    // The positive control comes first: with the read on offer the same click
    // reaches the host. Without it, the request row refuses.
    const control = renderBox(NEITHER);
    press(requestBox());
    expect(control.onChange).toHaveBeenCalledTimes(1);
    control.unmount();

    const { onChange } = renderBox({ read: true, request: true }, { readOffered: false });
    expect(requestBox().disabled).toBe(true);
    expect(requestBox().checked).toBe(false);
    expect(readBox().disabled).toBe(true);
    expect(readBox().checked).toBe(false);
    expect(screen.getByTestId("ability-request-blocked")).toHaveTextContent(NEEDS_READ_BLOCKED);
    expect(screen.queryByTestId("ability-request-needs-read")).toBeNull();
    expect(screen.getByTestId("ability-not-offered-mcp.ability.read")).toHaveTextContent(
      "Not requested by this app",
    );
    expect(screen.queryByTestId("ability-not-offered-mcp.ability.request")).toBeNull();

    press(requestBox());
    expect(onChange).not.toHaveBeenCalled();
    expect(requestBox().checked).toBe(false);
  });

  it("disables ask for changes with the plain note when the app did not request it, and the read moves alone", () => {
    const first = renderBox(NEITHER, { requestOffered: false });
    const { onChange } = first;
    expect(requestBox().disabled).toBe(true);
    expect(requestBox().checked).toBe(false);
    expect(screen.getByTestId("ability-not-offered-mcp.ability.request")).toHaveTextContent(
      "Not requested by this app",
    );
    expect(screen.queryByTestId("ability-request-needs-read")).toBeNull();
    expect(screen.queryByTestId("ability-request-blocked")).toBeNull();
    expect(screen.queryByTestId("ability-not-offered-mcp.ability.read")).toBeNull();

    // The read is a live control, and it reports the request as clear.
    expect(readBox().disabled).toBe(false);
    fireEvent.click(readBox());
    expect(onChange).toHaveBeenCalledTimes(1);
    expect(onChange).toHaveBeenCalledWith(READ_ONLY);
    expect(shown()).toEqual(READ_ONLY);
    fireEvent.click(readBox());
    expect(onChange).toHaveBeenLastCalledWith(NEITHER);

    // A request the host still holds, for an app that did not ask for it, is
    // shown clear and is not carried along when the read is ticked.
    first.unmount();
    const stale = renderBox({ read: false, request: true }, { requestOffered: false });
    expect(requestBox().checked).toBe(false);
    fireEvent.click(readBox());
    expect(stale.onChange).toHaveBeenCalledWith(READ_ONLY);
  });
});

describe("AbilityCapabilityBox, while a request is in flight", () => {
  it("disables both rows and reports nothing, where the same clicks report when it is not", () => {
    const control = renderBox(BOTH);
    press(readBox());
    expect(control.onChange).toHaveBeenCalledTimes(1);
    control.unmount();

    const { onChange } = renderBox(BOTH, { disabled: true });
    expect(readBox().disabled).toBe(true);
    expect(requestBox().disabled).toBe(true);
    press(readBox());
    press(requestBox());
    expect(onChange).not.toHaveBeenCalled();
    expect(shown()).toEqual(BOTH);
  });
});
