import { act, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import type { AbilityRequest } from "@wpmgr/api";

import { AbilityRequestCard } from "./ability-request-card";

const NOW = new Date("2026-10-01T10:00:00Z");
const mk = (o: Partial<AbilityRequest>): AbilityRequest => ({
  id: "r1",
  site_id: "s1",
  ability_name: "wpmgr/page-create",
  input_json: "{}",
  effect_copy: "draft",
  snapshot: "x",
  site_label: "Shop",
  site_host: "shop.example",
  grant_label: "grant",
  grant_via: "mcp",
  card_copy_version: 1,
  state: "done",
  outcome: "created",
  created_post_id: 7,
  created_at: "2026-10-01T09:00:00Z",
  expires_at: "2026-10-01T10:30:00Z",
  post_type: "page",
  undo_offered: true,
  undo_available_until: "2026-10-01T10:00:30Z",
  resolve_gave_up: false,
  ...o,
});
const renderCard = (r: AbilityRequest) =>
  render(<AbilityRequestCard request={r} onApprove={vi.fn()} onDecline={vi.fn()} onUndo={vi.fn()} />);

describe("Undo window", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    vi.setSystemTime(NOW);
  });
  afterEach(() => vi.useRealTimers());

  it("disappears at expiry without a refetch", () => {
    renderCard(mk({}));
    expect(screen.queryByRole("button", { name: "Undo" })).not.toBeNull();
    act(() => {
      vi.advanceTimersByTime(29_000);
    });
    expect(screen.queryByRole("button", { name: "Undo" })).not.toBeNull();
    act(() => {
      vi.advanceTimersByTime(1_500);
    });
    expect(screen.queryByRole("button", { name: "Undo" })).toBeNull();
  });
  it("stays hidden when undo_offered is false", () => {
    renderCard(mk({ undo_offered: false }));
    expect(screen.queryByRole("button", { name: "Undo" })).toBeNull();
  });
  it("is hidden when the window already passed", () => {
    renderCard(mk({ undo_available_until: "2026-10-01T09:59:00Z" }));
    expect(screen.queryByRole("button", { name: "Undo" })).toBeNull();
  });
});
