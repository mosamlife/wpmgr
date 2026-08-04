import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import type { Site, SiteTag, UpdateRun, UpdateTask } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult, mockMutationResult } from "@/test/query-mocks";
import { serverRetryFields } from "@/test/update-task-fixtures";

// cmdk's inline TagPicker (rendered by TagEditDrawer) needs the same jsdom
// shims as tag-picker.test.tsx.
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
vi.stubGlobal("ResizeObserver", ResizeObserverStub);
Element.prototype.scrollIntoView = vi.fn();

const {
  useSitesMock,
  useTagsMock,
  useCreateTagMock,
  useBulkApplyTagsMock,
  useUpdateRunMock,
  useRunEventStreamMock,
  toastSuccess,
  toastError,
} = vi.hoisted(() => ({
  useSitesMock: vi.fn(),
  useTagsMock: vi.fn(),
  useCreateTagMock: vi.fn(),
  useBulkApplyTagsMock: vi.fn(),
  useUpdateRunMock: vi.fn(),
  useRunEventStreamMock: vi.fn(),
  toastSuccess: vi.fn(),
  toastError: vi.fn(),
}));

vi.mock("@/features/sites/use-sites", () => ({ useSites: useSitesMock }));
vi.mock("@/features/tags/use-tags", () => ({
  useTags: useTagsMock,
  useCreateTag: useCreateTagMock,
  useBulkApplyTags: useBulkApplyTagsMock,
}));
vi.mock("@/features/updates/use-updates", () => ({
  useUpdateRun: useUpdateRunMock,
  useRunEventStream: useRunEventStreamMock,
}));
vi.mock("@/components/toast", () => ({
  toast: { success: toastSuccess, error: toastError, info: vi.fn(), warning: vi.fn() },
}));

import {
  BulkActionDrawer,
  TagEditDrawer,
  cycleTagState,
  deriveInitialTagState,
  computeTagDiff,
  type TagEditEntry,
} from "./bulk-action-drawer";
import type { TagPickerState } from "./tag-picker";

function makeTag(overrides: Partial<SiteTag> = {}): SiteTag {
  return {
    id: overrides.id ?? "t1",
    name: overrides.name ?? "Production",
    color: overrides.color ?? "",
    usage_count: overrides.usage_count ?? 0,
    created_at: overrides.created_at ?? "2024-01-01T00:00:00Z",
  };
}

function makeSite(overrides: Partial<Site> & { id: string; tags?: string[] }): Site {
  return {
    name: overrides.name ?? overrides.id,
    url: overrides.url ?? `https://${overrides.id}.example.com`,
    tags: overrides.tags ?? [],
    ...overrides,
  } as unknown as Site;
}

beforeEach(() => {
  useSitesMock.mockReset();
  useTagsMock.mockReset();
  useCreateTagMock.mockReset();
  useBulkApplyTagsMock.mockReset();
  useUpdateRunMock.mockReset();
  useRunEventStreamMock.mockReset();
  toastSuccess.mockReset();
  toastError.mockReset();
  useRunEventStreamMock.mockReturnValue(undefined);
  useCreateTagMock.mockReturnValue(
    mockMutationResult({ mutateAsync: vi.fn().mockResolvedValue(makeTag({ id: "new", name: "new-tag" })) }),
  );
});

// ---------------------------------------------------------------------------
// Pure logic: tri-state initial-state derivation
// ---------------------------------------------------------------------------

describe("deriveInitialTagState — tri-state derivation", () => {
  const tags = [
    makeTag({ id: "t1", name: "Production" }),
    makeTag({ id: "t2", name: "Staging" }),
    makeTag({ id: "t3", name: "Legacy" }),
  ];

  it("checked when every selected site carries the tag", () => {
    const sites = [{ tags: ["Production"] }, { tags: ["Production"] }];
    const result = deriveInitialTagState(tags, sites);
    expect(result.get("t1")).toBe("checked");
  });

  it("unchecked when no selected site carries the tag", () => {
    const sites = [{ tags: ["Production"] }, { tags: ["Production"] }];
    const result = deriveInitialTagState(tags, sites);
    expect(result.get("t3")).toBe("unchecked");
  });

  it("mixed when some but not all selected sites carry the tag", () => {
    const sites = [{ tags: ["Production", "Staging"] }, { tags: ["Production"] }];
    const result = deriveInitialTagState(tags, sites);
    expect(result.get("t2")).toBe("mixed");
  });

  it("every tag is unchecked when no sites are selected", () => {
    const result = deriveInitialTagState(tags, []);
    expect(result.get("t1")).toBe("unchecked");
    expect(result.get("t2")).toBe("unchecked");
    expect(result.get("t3")).toBe("unchecked");
  });

  // GH #230 adversarial-verify MEDIUM — `totalSelectedCount` lets a caller
  // tell this function "there were N sites selected, but you only handed me
  // a resolved subset" (e.g. some selected sites are archived / not yet in
  // any loaded cache). The tri-state must never assert a definite "none" or
  // "all" state while any selected site's tags remain unknown.
  describe("with an incomplete resolve (totalSelectedCount > sites.length)", () => {
    it("every tag reads 'mixed' — never 'checked', even when every KNOWN site has it", () => {
      // 3 sites were selected; only 2 resolved, and BOTH known sites carry
      // "Production". Without the safety net this would read "checked" —
      // a lie, since the 3rd (unresolved) site's tags are unknown.
      const sites = [{ tags: ["Production"] }, { tags: ["Production"] }];
      const result = deriveInitialTagState(tags, sites, 3);
      expect(result.get("t1")).toBe("mixed");
    });

    it("every tag reads 'mixed' — never 'unchecked', even when NO known site has it", () => {
      // Same shape, but for a tag no known site carries at all — asserting
      // "unchecked" would equally be a lie about the unresolved site.
      const sites = [{ tags: ["Production"] }, { tags: ["Production"] }];
      const result = deriveInitialTagState(tags, sites, 3);
      expect(result.get("t3")).toBe("mixed");
    });

    it("zero resolved sites out of N selected: still 'mixed', not 'unchecked'", () => {
      const result = deriveInitialTagState(tags, [], 3);
      expect(result.get("t1")).toBe("mixed");
      expect(result.get("t2")).toBe("mixed");
      expect(result.get("t3")).toBe("mixed");
    });

    it("totalSelectedCount === sites.length (fully resolved) behaves exactly like the 2-arg call", () => {
      const sites = [{ tags: ["Production"] }, { tags: ["Production"] }];
      const withCount = deriveInitialTagState(tags, sites, 2);
      const withoutCount = deriveInitialTagState(tags, sites);
      expect([...withCount.entries()]).toEqual([...withoutCount.entries()]);
    });
  });
});

describe("cycleTagState — mixed -> checked -> unchecked -> checked …", () => {
  it("mixed cycles to checked", () => {
    expect(cycleTagState("mixed")).toBe("checked");
  });
  it("checked cycles to unchecked", () => {
    expect(cycleTagState("checked")).toBe("unchecked");
  });
  it("unchecked cycles to checked", () => {
    expect(cycleTagState("unchecked")).toBe("checked");
  });
});

// ---------------------------------------------------------------------------
// Pure logic: diff math
// ---------------------------------------------------------------------------

describe("computeTagDiff — add/remove diff against the frozen initial map", () => {
  const tags = [
    makeTag({ id: "t1", name: "Production" }),
    makeTag({ id: "t2", name: "Staging" }),
    makeTag({ id: "t3", name: "Legacy" }),
  ];

  it("an untouched tag (current === initial) contributes nothing, even if initial is mixed", () => {
    const initial = new Map<string, TagPickerState>([
      ["t1", "checked"],
      ["t2", "mixed"],
      ["t3", "unchecked"],
    ]);
    const diff = computeTagDiff(tags, initial, initial);
    expect(diff.add).toEqual([]);
    expect(diff.remove).toEqual([]);
  });

  it("mixed -> checked lands in add", () => {
    const initial = new Map<string, TagPickerState>([["t2", "mixed"]]);
    const current = new Map<string, TagPickerState>([["t2", "checked"]]);
    const diff = computeTagDiff(tags, initial, current);
    expect(diff.add).toEqual(["Staging"]);
    expect(diff.remove).toEqual([]);
  });

  it("checked -> unchecked lands in remove", () => {
    const initial = new Map<string, TagPickerState>([["t1", "checked"]]);
    const current = new Map<string, TagPickerState>([["t1", "unchecked"]]);
    const diff = computeTagDiff(tags, initial, current);
    expect(diff.remove).toEqual(["Production"]);
    expect(diff.add).toEqual([]);
  });

  it("a fresh tag (not in `initial` at all, e.g. just created) checked lands in add", () => {
    const initial = new Map<string, TagPickerState>();
    const current = new Map<string, TagPickerState>([["t3", "checked"]]);
    const diff = computeTagDiff(tags, initial, current);
    expect(diff.add).toEqual(["Legacy"]);
  });

  it("mixes multiple changes across tags into one diff", () => {
    const initial = new Map<string, TagPickerState>([
      ["t1", "checked"],
      ["t2", "mixed"],
      ["t3", "unchecked"],
    ]);
    const current = new Map<string, TagPickerState>([
      ["t1", "unchecked"], // checked -> unchecked
      ["t2", "checked"], // mixed -> checked
      ["t3", "unchecked"], // untouched
    ]);
    const diff = computeTagDiff(tags, initial, current);
    expect(diff.add).toEqual(["Staging"]);
    expect(diff.remove).toEqual(["Production"]);
  });
});

// ---------------------------------------------------------------------------
// TagEditDrawer — end-to-end diff -> single bulk-apply call shape
// ---------------------------------------------------------------------------

describe("TagEditDrawer — single POST /tags/bulk-apply call shape", () => {
  const TAGS: SiteTag[] = [
    makeTag({ id: "t1", name: "Production", usage_count: 2 }),
    makeTag({ id: "t2", name: "Staging", usage_count: 1 }),
    makeTag({ id: "t3", name: "Legacy", usage_count: 0 }),
  ];
  const SITES: Site[] = [
    makeSite({ id: "site-1", tags: ["Production", "Staging"] }),
    makeSite({ id: "site-2", tags: ["Production"] }),
  ];

  function baseEntry(): TagEditEntry {
    return {
      kind: "tag-edit",
      id: "tag-edit-1",
      title: "Tag 2 sites",
      siteIds: ["site-1", "site-2"],
      status: "editing",
      settled: false,
    };
  }

  beforeEach(() => {
    useSitesMock.mockReturnValue(mockQueryResult({ data: SITES }));
    useTagsMock.mockReturnValue(mockQueryResult({ data: TAGS }));
  });

  it("derives the tri-state correctly on mount (Production checked, Staging mixed, Legacy unchecked)", () => {
    const bulkApply = vi.fn().mockResolvedValue({ results: [] });
    useBulkApplyTagsMock.mockReturnValue(mockMutationResult({ mutateAsync: bulkApply }));

    renderWithProviders(
      <TagEditDrawer entry={baseEntry()} visible onClose={vi.fn()} onUpdate={vi.fn()} />,
    );

    expect(screen.getByRole("option", { name: /Production/ })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("option", { name: /Staging/ })).toHaveAttribute("aria-checked", "mixed");
    expect(screen.getByRole("option", { name: /Legacy/ })).toHaveAttribute("aria-checked", "false");

    // Apply is disabled — nothing has changed yet.
    expect(screen.getByRole("button", { name: /Apply to 2 sites/ })).toBeDisabled();
  });

  it("fires exactly ONE bulk-apply call with the exact {site_ids, add, remove} diff after toggling", async () => {
    const bulkApply = vi.fn().mockResolvedValue({
      results: [
        { site_id: "site-1", ok: true, detail: "applied" },
        { site_id: "site-2", ok: true, detail: "applied" },
      ],
    });
    useBulkApplyTagsMock.mockReturnValue(mockMutationResult({ mutateAsync: bulkApply }));
    const onUpdate = vi.fn();

    renderWithProviders(
      <TagEditDrawer entry={baseEntry()} visible onClose={vi.fn()} onUpdate={onUpdate} />,
    );

    // Staging: mixed -> checked (add).
    fireEvent.click(screen.getByText("Staging"));
    // Legacy: unchecked -> checked (add).
    fireEvent.click(screen.getByText("Legacy"));
    // Production: checked -> unchecked (remove).
    fireEvent.click(screen.getByText("Production"));

    const applyButton = screen.getByRole("button", { name: /Apply to 2 sites/ });
    expect(applyButton).not.toBeDisabled();
    fireEvent.click(applyButton);

    await waitFor(() => expect(bulkApply).toHaveBeenCalledTimes(1));
    expect(bulkApply).toHaveBeenCalledWith({
      site_ids: ["site-1", "site-2"],
      add: ["Staging", "Legacy"],
      remove: ["Production"],
    });

    await waitFor(() =>
      expect(onUpdate).toHaveBeenCalledWith(
        "tag-edit-1",
        expect.objectContaining({ status: "done" }),
      ),
    );
    expect(toastSuccess).toHaveBeenCalledTimes(1);
  });
});

// ---------------------------------------------------------------------------
// TagEditPanel — loading gate for an incomplete resolve (GH #230
// adversarial-verify MEDIUM: selection can include archived-view sites the
// panel's own active-view query would never see on its own).
// ---------------------------------------------------------------------------

describe("TagEditDrawer — blocks seeding until every selected site resolves (active + archived)", () => {
  const TAGS: SiteTag[] = [makeTag({ id: "t1", name: "Production", usage_count: 3 })];
  // site-3 lives ONLY in the archived bucket — a selection made from the
  // "Show archived" view.
  const ACTIVE: Site[] = [
    makeSite({ id: "site-1", tags: ["Production"] }),
    makeSite({ id: "site-2", tags: ["Production"] }),
  ];
  const ARCHIVED: Site[] = [makeSite({ id: "site-3", tags: ["Production"] })];

  function entryWithArchivedSelection(): TagEditEntry {
    return {
      kind: "tag-edit",
      id: "tag-edit-archived",
      title: "Tag 3 sites",
      siteIds: ["site-1", "site-2", "site-3"],
      status: "editing",
      settled: false,
    };
  }

  function mockUseSitesByView(opts: { archivedPending: boolean; includeSite3: boolean }) {
    useSitesMock.mockImplementation((args?: { view?: string }) => {
      if (args?.view === "archived") {
        // `isPending` must be a literal `true`/`false` (not a plain
        // `boolean`) to satisfy `UseQueryResult`'s discriminated union, so
        // branch explicitly rather than passing the flag through directly.
        // A genuinely pending query also has no `data` yet — matches the
        // real runtime shape, and avoids fighting the Partial<union> type.
        return opts.archivedPending
          ? mockQueryResult<Site[]>({ isPending: true })
          : mockQueryResult<Site[]>({
              data: opts.includeSite3 ? ARCHIVED : [],
              isPending: false,
            });
      }
      return mockQueryResult<Site[]>({ data: ACTIVE, isPending: false });
    });
  }

  beforeEach(() => {
    useTagsMock.mockReturnValue(mockQueryResult({ data: TAGS }));
  });

  it("shows a loading state (not a tri-state glyph) while the archived bucket is still pending", () => {
    mockUseSitesByView({ archivedPending: true, includeSite3: false });

    renderWithProviders(
      <TagEditDrawer entry={entryWithArchivedSelection()} visible onClose={vi.fn()} onUpdate={vi.fn()} />,
    );

    expect(screen.getByRole("status")).toHaveTextContent(/loading tags for 3 sites/i);
    expect(screen.queryByRole("option", { name: /Production/ })).not.toBeInTheDocument();
    // The Apply button is always present (footer chrome), but must stay
    // disabled — nothing has been seeded to diff against yet.
    expect(screen.getByRole("button", { name: /Apply to 3 sites/ })).toBeDisabled();
  });

  it("once the archived bucket resolves with site-3 present, seeds the real (fully-resolved) tri-state — not the conservative fallback", () => {
    mockUseSitesByView({ archivedPending: false, includeSite3: true });

    renderWithProviders(
      <TagEditDrawer entry={entryWithArchivedSelection()} visible onClose={vi.fn()} onUpdate={vi.fn()} />,
    );

    // All 3 selected sites (2 active + 1 archived) carry "Production" —
    // fully resolved, so this is a genuine "checked", not a hedge.
    expect(screen.getByRole("option", { name: /Production/ })).toHaveAttribute("aria-checked", "true");
  });

  it("never lies: if a selected site is STILL unresolved after both buckets settle, every tag reads 'mixed', never 'checked' or 'unchecked'", () => {
    // Both queries have settled (isPending: false) but site-3 is in
    // NEITHER bucket (e.g. deleted between selection and drawer-open) — the
    // panel must not silently treat this as "2 of 2 known sites, therefore
    // checked".
    mockUseSitesByView({ archivedPending: false, includeSite3: false });

    renderWithProviders(
      <TagEditDrawer entry={entryWithArchivedSelection()} visible onClose={vi.fn()} onUpdate={vi.fn()} />,
    );

    expect(screen.getByRole("option", { name: /Production/ })).toHaveAttribute("aria-checked", "mixed");
  });
});

// ---------------------------------------------------------------------------
// update-run regression — the pre-existing branch renders unchanged
// ---------------------------------------------------------------------------

describe("BulkActionDrawer — update-run branch renders identically after the GH #230 generalization", () => {
  function makeTask(overrides: Partial<UpdateTask> & Pick<UpdateTask, "id" | "site_id" | "status">): UpdateTask {
    return {
      run_id: "run-1",
      tenant_id: "tenant-1",
      target_type: "plugin",
      target_slug: "akismet",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
      // GH #336: the server always writes the retry pair its own
      // retryClassify would produce for this status.
      ...serverRetryFields(overrides.status),
      ...overrides,
    };
  }

  it("shows the title and per-task done/total counts", async () => {
    const run: Partial<UpdateRun> = {
      id: "run-1",
      status: "running",
      tasks: [
        makeTask({ id: "task-1", site_id: "site-1", target_slug: "akismet", status: "succeeded" }),
        makeTask({ id: "task-2", site_id: "site-2", target_slug: "jetpack", status: "running" }),
      ],
    };
    useUpdateRunMock.mockReturnValue(mockQueryResult({ data: run as UpdateRun }));
    useSitesMock.mockReturnValue(
      mockQueryResult({
        data: [makeSite({ id: "site-1", name: "Site One" }), makeSite({ id: "site-2", name: "Site Two" })],
      }),
    );

    renderWithProviders(
      <BulkActionDrawer
        runId="run-1"
        title="Update plugins on 2 sites"
        visible
        onClose={vi.fn()}
      />,
    );

    expect(await screen.findByText("Update plugins on 2 sites")).toBeInTheDocument();
    // 1 done (succeeded) / 2 total, 1 in progress — the pre-existing footer copy
    // ("<span>1</span> / <span>2</span> done ... <span>1</span> in progress").
    expect(
      screen.getByText((_content, node) => node?.textContent?.trim().startsWith("1 / 2 done") ?? false),
    ).toBeInTheDocument();
    expect(useRunEventStreamMock).toHaveBeenCalledWith("run-1", { enabled: true });
  });
});
