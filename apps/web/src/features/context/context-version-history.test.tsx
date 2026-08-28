import { describe, it, expect, vi } from "vitest";
import { screen, fireEvent, within } from "@testing-library/react";
import type { GovContextDiff, GovContextVersionSummary } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import { ContextVersionHistory, RestoreVersionDialog } from "./context-version-history";
import { ContextWidenForbiddenError } from "./use-context";

// ADR-064 S5 Stage B, Screen 4 — version history / diff / restore outcome
// tests. Tests the presentational components directly against real prop
// shapes, same approach as gov-context-editor.test.tsx.

function buildVersion(overrides: Partial<GovContextVersionSummary> = {}): GovContextVersionSummary {
  return {
    id: "v-1",
    version: 1,
    author_type: "user",
    author_id: "11111111-1111-1111-1111-111111111111",
    provenance: "manual",
    created_at: "2026-08-20T00:00:00Z",
    ...overrides,
  };
}

function noop() {
  /* test stub */
}

describe("ContextVersionHistory — author, provenance and timestamp per version, current flagged correctly", () => {
  it("marks the FIRST (newest) item as current, not any other", () => {
    const items = [
      buildVersion({ id: "v-3", version: 3, provenance: "restore" }),
      buildVersion({ id: "v-2", version: 2, author_type: "api_key", author_id: undefined }),
      buildVersion({ id: "v-1", version: 1 }),
    ];

    renderWithProviders(
      <ContextVersionHistory
        scopeLabel="site"
        items={items}
        isPending={false}
        isError={false}
        error={null}
        onRetry={noop}
        hasNextPage={false}
        isFetchingNextPage={false}
        onLoadMore={noop}
        expandedId={null}
        onToggleExpand={noop}
        canWrite
        onRequestRestore={noop}
      />,
    );

    // Non-vacuous, scoped to the SPECIFIC row: exactly one "Current" badge
    // total is not enough to prove which version it's on (a version that
    // flagged the wrong row as current would still pass a bare count check)
    // — assert it sits inside v3's own row, and nowhere else.
    const v3Row = screen.getByText("v3").closest("li");
    const v2Row = screen.getByText("v2").closest("li");
    const v1Row = screen.getByText("v1").closest("li");
    if (!v3Row || !v2Row || !v1Row) throw new Error("row not found");

    expect(within(v3Row).getByText("Current")).toBeInTheDocument();
    expect(
      within(v3Row).queryByRole("button", { name: "Restore this version" }),
    ).not.toBeInTheDocument();
    expect(within(v2Row).getByRole("button", { name: "Restore this version" })).toBeInTheDocument();
    expect(within(v1Row).getByRole("button", { name: "Restore this version" })).toBeInTheDocument();
    expect(within(v2Row).queryByText("Current")).not.toBeInTheDocument();
    expect(within(v1Row).queryByText("Current")).not.toBeInTheDocument();

    // Author/provenance render for real, differing values (v1 and v2 are
    // both "manual" in this fixture, hence AllBy for that one).
    expect(within(v2Row).getByText("API key")).toBeInTheDocument();
    expect(within(v3Row).getByText("restore")).toBeInTheDocument();
    expect(screen.getAllByText("manual")).toHaveLength(2);
  });

  it("shows the empty state, not a loading or error tree, for zero versions", () => {
    renderWithProviders(
      <ContextVersionHistory
        scopeLabel="site"
        items={[]}
        isPending={false}
        isError={false}
        error={null}
        onRetry={noop}
        hasNextPage={false}
        isFetchingNextPage={false}
        onLoadMore={noop}
        expandedId={null}
        onToggleExpand={noop}
        canWrite
        onRequestRestore={noop}
      />,
    );
    expect(screen.getByText("No edits yet")).toBeInTheDocument();
  });
});

describe("ContextVersionHistory — diff never implies it compares enforced state", () => {
  function buildDiff(overrides: Partial<GovContextDiff> = {}): GovContextDiff {
    return {
      version: { ...buildVersion({ id: "v-2", version: 2 }), restrictions: {}, guidance: {} },
      baseline: false,
      prior: { ...buildVersion({ id: "v-1", version: 1 }), restrictions: {}, guidance: {} },
      diff: {
        forbidden_tools: { added: ["shell_exec"], removed: ["wp_eval"] },
        brand_voice: { old: "Friendly", new: "Formal" },
      },
      ...overrides,
    };
  }

  it("renders added/removed list items and old/new text fields, plus the authored-not-enforced caveat", () => {
    renderWithProviders(
      <ContextVersionHistory
        scopeLabel="site"
        items={[buildVersion({ id: "v-2", version: 2 })]}
        isPending={false}
        isError={false}
        error={null}
        onRetry={noop}
        hasNextPage={false}
        isFetchingNextPage={false}
        onLoadMore={noop}
        expandedId="v-2"
        onToggleExpand={noop}
        diff={mockQueryResult<GovContextDiff>({ data: buildDiff() })}
        canWrite
        onRequestRestore={noop}
      />,
    );

    expect(screen.getByText("+ shell_exec")).toBeInTheDocument();
    expect(screen.getByText("− wp_eval")).toBeInTheDocument();
    expect(screen.getByText("Friendly")).toBeInTheDocument();
    expect(screen.getByText("Formal")).toBeInTheDocument();
    // Non-vacuous: this line is the whole point of the coordinator's ruling
    // on Screen 4 — a diff of stored rows is never a diff of enforced state.
    expect(
      screen.getByText(/not what either one enforced at the time/i),
    ).toBeInTheDocument();
  });

  it("renders the baseline message, never a computed diff table, for a version with no eligible predecessor", () => {
    renderWithProviders(
      <ContextVersionHistory
        scopeLabel="organisation"
        items={[buildVersion({ id: "v-1", version: 1 })]}
        isPending={false}
        isError={false}
        error={null}
        onRetry={noop}
        hasNextPage={false}
        isFetchingNextPage={false}
        onLoadMore={noop}
        expandedId="v-1"
        onToggleExpand={noop}
        diff={mockQueryResult<GovContextDiff>({
          data: { version: buildDiff().version, baseline: true },
        })}
        canWrite
        onRequestRestore={noop}
      />,
    );

    expect(screen.getByText(/no prior version to compare/i)).toBeInTheDocument();
    expect(screen.queryByText("+ shell_exec")).not.toBeInTheDocument();
  });
});

describe("RestoreVersionDialog — a widen refusal here reads as correct behaviour, not a generic edit error", () => {
  it("uses restore-specific wording ('since been tightened'), never the edit form's banner text", () => {
    const message =
      "this write would remove [shell_exec] from forbidden_tools, which was set by organisation default (layer 2) — a lower layer may narrow or add to a restriction but never remove what a higher layer set";
    renderWithProviders(
      <RestoreVersionDialog
        open
        onClose={noop}
        version={buildVersion({ version: 2 })}
        scopeLabel="site"
        onConfirm={noop}
        isPending={false}
        error={
          new ContextWidenForbiddenError(message, {
            field: "forbidden_tools",
            layer: 2,
            layerName: "organisation default",
            removedItems: ["shell_exec"],
          })
        }
      />,
    );

    expect(screen.getByText(/since been tightened/i)).toBeInTheDocument();
    // The server's own message is appended verbatim (not just paraphrased
    // away) — it's rendered inside the same node as the "since been
    // tightened" lead-in, so match on a substring, not the whole node.
    expect(
      screen.getByText((content) => content.includes(message)),
    ).toBeInTheDocument();
    // Never the edit-path's generic pointer — this is a materially different
    // situation (the refusal is correct here, not a mistaken edit).
    expect(
      screen.queryByText("Could not save — see the highlighted restriction below."),
    ).not.toBeInTheDocument();
  });

  it("calls onConfirm when the operator clicks Restore", () => {
    const onConfirm = vi.fn();
    renderWithProviders(
      <RestoreVersionDialog
        open
        onClose={noop}
        version={buildVersion({ version: 2 })}
        scopeLabel="site"
        onConfirm={onConfirm}
        isPending={false}
        error={null}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "Restore this version" }));
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });
});
