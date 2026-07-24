import { describe, it, expect, vi } from "vitest";
import { screen, fireEvent, waitFor } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { UserPickerModal } from "./user-picker-modal";

// GH #286 (web half): UserPickerModal outcome tests.
//
// Covers the two behaviours the locked design calls out by name:
//   1. the "Make this the default" checkbox only appears when the caller
//      supplies `onSaveDefault` (site-detail header + Settings tab); it is
//      absent everywhere else (list surfaces never pass the prop).
//   2. checking it fires the PUT in parallel with the login, never gating
//      the login on the save succeeding. A regression that awaited
//      `onSaveDefault` before calling `onSubmit` (or vice versa) would
//      still pass a "both eventually called" assertion, so the tests below
//      pin the actual call sequence, not just "both fired".

function fillUsernameAndSubmit(username: string, checkDefault: boolean) {
  const input = screen.getByLabelText("WordPress username");
  fireEvent.change(input, { target: { value: username } });

  if (checkDefault) {
    const checkbox = screen.getByRole("checkbox", {
      name: /make this the default for this site/i,
    });
    fireEvent.click(checkbox);
  }

  fireEvent.click(screen.getByRole("button", { name: /open site/i }));
}

describe("UserPickerModal: 'Make this the default' checkbox visibility", () => {
  it("is absent when the caller does not pass onSaveDefault (e.g. list surfaces)", () => {
    renderWithProviders(
      <UserPickerModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        siteName="Example"
      />,
    );

    expect(
      screen.queryByRole("checkbox", { name: /make this the default/i }),
    ).not.toBeInTheDocument();
  });

  it("is present when the caller passes onSaveDefault", () => {
    renderWithProviders(
      <UserPickerModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        onSaveDefault={vi.fn()}
        siteName="Example"
      />,
    );

    expect(
      screen.getByRole("checkbox", { name: /make this the default/i }),
    ).toBeInTheDocument();
  });
});

describe("UserPickerModal: default hint copy", () => {
  it("hints the first-administrator fallback when no default is known", () => {
    renderWithProviders(
      <UserPickerModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        siteName="Example"
      />,
    );

    expect(
      screen.getByText(/leave blank to use the first administrator/i),
    ).toBeInTheDocument();
  });

  it("hints the site default username when known", () => {
    renderWithProviders(
      <UserPickerModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        siteName="Example"
        defaultLoginUser="editor-jane"
      />,
    );

    expect(
      screen.getByText(/leave blank to use the site default/i),
    ).toBeInTheDocument();
    expect(screen.getByText("editor-jane")).toBeInTheDocument();
  });
});

describe("UserPickerModal: checking 'Make this the default' fires the login AND the save, without blocking either", () => {
  it("calls onSubmit and onSaveDefault with the same typed username, both reachable in the same resolved submit", async () => {
    const onSubmit = vi.fn();
    const onSaveDefault = vi.fn();

    renderWithProviders(
      <UserPickerModal
        open
        onClose={vi.fn()}
        onSubmit={onSubmit}
        onSaveDefault={onSaveDefault}
        siteName="Example"
      />,
    );

    fillUsernameAndSubmit("editor-jane", true);

    // The submit handler is async (zod validation resolves via a promise),
    // so wait for the login call, but assert the save call WITHOUT a
    // separate `waitFor`: it is fired on the very next line of the same
    // handler, with no `await` between them. If a future edit ever awaited
    // `onSaveDefault` before calling `onSubmit` (or gated it behind the
    // save), this synchronous check right after the login call would catch
    // it going missing.
    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit).toHaveBeenCalledWith("editor-jane");
    expect(onSaveDefault).toHaveBeenCalledTimes(1);
    expect(onSaveDefault).toHaveBeenCalledWith("editor-jane");
  });

  it("does NOT call onSaveDefault when the checkbox is left unchecked", async () => {
    const onSubmit = vi.fn();
    const onSaveDefault = vi.fn();

    renderWithProviders(
      <UserPickerModal
        open
        onClose={vi.fn()}
        onSubmit={onSubmit}
        onSaveDefault={onSaveDefault}
        siteName="Example"
      />,
    );

    fillUsernameAndSubmit("editor-jane", false);

    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSaveDefault).not.toHaveBeenCalled();
  });

  it("disables the checkbox while the username is blank, so a blank login can never clear the default", async () => {
    const onSubmit = vi.fn();
    const onSaveDefault = vi.fn();

    renderWithProviders(
      <UserPickerModal
        open
        onClose={vi.fn()}
        onSubmit={onSubmit}
        onSaveDefault={onSaveDefault}
        siteName="Example"
      />,
    );

    const checkbox = screen.getByRole("checkbox", {
      name: /make this the default for this site/i,
    });
    expect(checkbox).toBeDisabled();

    // Clicking a disabled checkbox is a no-op, but submit anyway to prove
    // the login proceeds and the save path never fires.
    fireEvent.click(checkbox);
    fireEvent.click(screen.getByRole("button", { name: /open site/i }));

    await waitFor(() => expect(onSubmit).toHaveBeenCalledTimes(1));
    expect(onSubmit).toHaveBeenCalledWith(undefined);
    expect(onSaveDefault).not.toHaveBeenCalled();
  });

  it("re-enables the checkbox once a username is typed, and disables (and unchecks) it again if the field is cleared", () => {
    renderWithProviders(
      <UserPickerModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        onSaveDefault={vi.fn()}
        siteName="Example"
      />,
    );

    const input = screen.getByLabelText("WordPress username");
    const checkbox = screen.getByRole("checkbox", {
      name: /make this the default for this site/i,
    });

    fireEvent.change(input, { target: { value: "editor-jane" } });
    expect(checkbox).toBeEnabled();

    fireEvent.click(checkbox);
    expect(checkbox).toBeChecked();

    fireEvent.change(input, { target: { value: "" } });
    expect(checkbox).toBeDisabled();
    expect(checkbox).not.toBeChecked();
  });
});
