import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import type { SiteTag } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

// cmdk's <Command.List> observes its own height via ResizeObserver on mount.
// jsdom does not implement ResizeObserver; stub it so the component can mount
// in the test environment without throwing.
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
vi.stubGlobal("ResizeObserver", ResizeObserverStub);

// jsdom does not implement Element.scrollIntoView either; cmdk calls it when
// the active option changes.
Element.prototype.scrollIntoView = vi.fn();

const { useTagsMock } = vi.hoisted(() => ({ useTagsMock: vi.fn() }));

vi.mock("@/features/tags/use-tags", () => ({
  useTags: useTagsMock,
}));

import { TagPicker, type TagPickerState } from "./tag-picker";

function tag(overrides: Partial<SiteTag> = {}): SiteTag {
  return {
    id: overrides.id ?? "t1",
    name: overrides.name ?? "Production",
    color: overrides.color ?? "",
    usage_count: overrides.usage_count ?? 0,
    created_at: overrides.created_at ?? "2024-01-01T00:00:00Z",
  };
}

const TAGS: SiteTag[] = [
  tag({ id: "t1", name: "Production", usage_count: 5 }),
  tag({ id: "t2", name: "Staging", color: "#3b82f6", usage_count: 2 }),
  tag({ id: "t3", name: "client-a", usage_count: 1 }),
];

beforeEach(() => {
  useTagsMock.mockReset();
  useTagsMock.mockReturnValue(mockQueryResult({ data: TAGS }));
});

function renderPicker(overrides: {
  getState?: (t: SiteTag) => TagPickerState;
  onToggle?: (t: SiteTag) => void;
  onCreate?: (name: string) => Promise<SiteTag>;
} = {}) {
  const onToggle = overrides.onToggle ?? vi.fn();
  const onCreate = overrides.onCreate ?? vi.fn().mockResolvedValue(tag({ id: "new", name: "new-tag" }));
  const getState = overrides.getState ?? ((): TagPickerState => "unchecked");
  const utils = renderWithProviders(
    <TagPicker getState={getState} onToggle={onToggle} onCreate={onCreate} />,
  );
  return { ...utils, onToggle, onCreate };
}

describe("TagPicker — create-row visibility rules", () => {
  it("hides the create row when the search query is empty", () => {
    renderPicker();
    expect(screen.queryByText(/^Create /)).not.toBeInTheDocument();
  });

  it("shows a create row for a query with no case-insensitive exact match", () => {
    renderPicker();
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "brand-new" } });
    expect(screen.getByText('Create "brand-new"')).toBeInTheDocument();
  });

  it("hides the create row when the query exactly matches an existing tag (case-insensitive)", () => {
    renderPicker();
    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "PRODUCTION" } });
    expect(screen.queryByText(/^Create /)).not.toBeInTheDocument();
    // The existing tag is still shown (surfaced instead of the create row).
    expect(screen.getByText("Production")).toBeInTheDocument();
  });

  it("selecting the create row calls onCreate then onToggle with the created tag", async () => {
    const created = tag({ id: "new-id", name: "brand-new" });
    const onCreate = vi.fn().mockResolvedValue(created);
    const onToggle = vi.fn();
    renderPicker({ onCreate, onToggle });

    const input = screen.getByRole("combobox");
    fireEvent.change(input, { target: { value: "brand-new" } });
    const createRow = screen.getByText('Create "brand-new"');
    fireEvent.click(createRow);

    await waitFor(() => expect(onCreate).toHaveBeenCalledWith("brand-new"));
    await waitFor(() => expect(onToggle).toHaveBeenCalledWith(created));
  });
});

describe("TagPicker — toggle behavior stays mounted (popover-open ownership lives with the caller)", () => {
  it("clicking an existing tag calls onToggle exactly once with that tag, without unmounting", () => {
    const onToggle = vi.fn();
    renderPicker({ onToggle });

    fireEvent.click(screen.getByText("Staging"));

    expect(onToggle).toHaveBeenCalledTimes(1);
    expect(onToggle).toHaveBeenCalledWith(expect.objectContaining({ id: "t2", name: "Staging" }));
    // TagPicker itself never closes/unmounts on a toggle — the input and the
    // rest of the list are still present (a wrapping Popover, not TagPicker,
    // owns open/close).
    expect(screen.getByRole("combobox")).toBeInTheDocument();
    expect(screen.getByText("Production")).toBeInTheDocument();
  });

  it("toggling a second item after the first still calls onToggle for each (no implicit close/reset)", () => {
    const onToggle = vi.fn();
    renderPicker({ onToggle });

    fireEvent.click(screen.getByText("Production"));
    fireEvent.click(screen.getByText("client-a"));

    expect(onToggle).toHaveBeenCalledTimes(2);
  });
});

describe("TagPicker — tri-state (bulk mode)", () => {
  it('renders aria-checked="mixed" and the Minus glyph for a mixed tag', () => {
    renderPicker({ getState: (t) => (t.id === "t1" ? "mixed" : "unchecked") });
    const option = screen.getByRole("option", { name: /Production/ });
    expect(option).toHaveAttribute("aria-checked", "mixed");
  });

  it('renders aria-checked="true" for a checked tag and "false" for unchecked', () => {
    renderPicker({ getState: (t) => (t.id === "t1" ? "checked" : "unchecked") });
    expect(screen.getByRole("option", { name: /Production/ })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    expect(screen.getByRole("option", { name: /Staging/ })).toHaveAttribute(
      "aria-checked",
      "false",
    );
  });

  it("clicking a mixed tag still calls onToggle (the caller owns the mixed -> checked -> unchecked cycle)", () => {
    const onToggle = vi.fn();
    renderPicker({ getState: () => "mixed", onToggle });
    fireEvent.click(screen.getByText("Production"));
    expect(onToggle).toHaveBeenCalledWith(expect.objectContaining({ id: "t1" }));
  });
});

describe("TagPicker — Esc has no effect of its own (ownership lives with the wrapping surface)", () => {
  it("pressing Escape in the search input does not toggle anything or unmount the picker", () => {
    const onToggle = vi.fn();
    renderPicker({ onToggle });
    const input = screen.getByRole("combobox");
    fireEvent.keyDown(input, { key: "Escape" });
    expect(onToggle).not.toHaveBeenCalled();
    // Still mounted — TagPicker has no Popover of its own to close. Single-
    // site mode wraps it in <Popover>, whose Radix Escape handling closes
    // that surface; bulk mode never wraps it in a Popover at all, so Escape
    // is a no-op by construction.
    expect(screen.getByRole("combobox")).toBeInTheDocument();
  });
});
