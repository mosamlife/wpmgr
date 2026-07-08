import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import type { SearchReplaceRequest, SearchReplaceResult } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult } from "@/test/query-mocks";

import { SearchReplacePanel } from "./SearchReplacePanel";
import { useSearchReplace } from "./use-search-replace";

// P1 outcome test — GH #170 Wave 5.
//
// `src/features/tools/` had NO render test at all before this file — a
// destructive, whole-database rewrite tool covered by nothing beyond
// TypeScript. This drives the REAL confirm-gated flow: dry-run first, the
// live rewrite fires ONLY after the explicit "Apply now" confirm click, and
// carries the exact search/replace strings the operator entered — never
// fires early, never with the wrong terms. See the non-vacuous notes inline.

vi.mock("./use-search-replace", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-search-replace")>();
  return {
    ...actual,
    useSearchReplace: vi.fn(),
  };
});

const mockedUseSearchReplace = vi.mocked(useSearchReplace);

function buildResult(overrides: Partial<SearchReplaceResult>): SearchReplaceResult {
  return {
    ok: true,
    dry_run: true,
    tables_scanned: 12,
    rows_matched: 5,
    rows_changed: 0,
    ...overrides,
  };
}

describe("SearchReplacePanel — dry-run first, confirm-gated apply", () => {
  let mutateAsyncMock: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.clearAllMocks();
    mutateAsyncMock = vi.fn();
    mockedUseSearchReplace.mockReturnValue(
      mockMutationResult<SearchReplaceResult, SearchReplaceRequest>({
        mutateAsync: mutateAsyncMock,
      }),
    );
  });

  it("never fires the replace before a preview + explicit confirm, and fires it with the exact entered search/replace once confirmed", async () => {
    mutateAsyncMock.mockImplementation((body: SearchReplaceRequest) =>
      Promise.resolve(
        buildResult({
          dry_run: body.dry_run,
          rows_changed: body.dry_run ? 0 : 5,
        }),
      ),
    );

    renderWithProviders(<SearchReplacePanel siteId="site-1" canOperate />);

    // Nothing fires just from mounting the panel.
    expect(mutateAsyncMock).not.toHaveBeenCalled();
    // Apply-adjacent affordances don't even exist yet — there is no preview.
    expect(
      screen.queryByRole("button", { name: /^Apply \(/ }),
    ).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("Search for"), {
      target: { value: "https://old.example.com" },
    });
    fireEvent.change(screen.getByLabelText("Replace with"), {
      target: { value: "https://new.example.com" },
    });

    // Step 1: preview (dry_run=true).
    fireEvent.click(
      screen.getByRole("button", { name: "Preview (dry-run)" }),
    );

    expect(await screen.findByText("Preview result")).toBeInTheDocument();
    expect(mutateAsyncMock).toHaveBeenCalledTimes(1);
    expect(mutateAsyncMock).toHaveBeenNthCalledWith(1, {
      search: "https://old.example.com",
      replace: "https://new.example.com",
      dry_run: true,
    });

    // Step 2: Apply opens the confirm dialog — the live rewrite has NOT
    // fired yet (still only the one dry-run call from the preview).
    fireEvent.click(
      screen.getByRole("button", { name: "Apply (5 rows)" }),
    );
    expect(
      await screen.findByRole("heading", { name: "Apply search-replace?" }),
    ).toBeInTheDocument();
    expect(mutateAsyncMock).toHaveBeenCalledTimes(1);

    // Step 3: the SECOND explicit click ("Apply now") is what fires the
    // live (dry_run=false) rewrite, carrying the SAME search/replace terms.
    fireEvent.click(screen.getByRole("button", { name: "Apply now" }));

    expect(mutateAsyncMock).toHaveBeenCalledTimes(2);
    expect(mutateAsyncMock).toHaveBeenNthCalledWith(2, {
      search: "https://old.example.com",
      replace: "https://new.example.com",
      dry_run: false,
    });

    // The "Done." copy interpolates the row/table counts through a nested
    // <strong>, so the string is split across sibling elements — assert via
    // jest-dom's `toHaveTextContent` (which recurses + normalizes
    // whitespace) on the containing block rather than a single `getByText`
    // regex that would fail to span the element boundary.
    const doneBlock = await screen.findByText("Done.", { exact: false });
    expect(doneBlock).toHaveTextContent("5 rows updated across 12 tables.");
  });

  it("does not enable Apply when the preview matched zero rows", async () => {
    mutateAsyncMock.mockResolvedValue(
      buildResult({ rows_matched: 0, rows_changed: 0 }),
    );

    renderWithProviders(<SearchReplacePanel siteId="site-1" canOperate />);

    fireEvent.change(screen.getByLabelText("Search for"), {
      target: { value: "nomatch-string" },
    });
    fireEvent.click(
      screen.getByRole("button", { name: "Preview (dry-run)" }),
    );

    expect(
      await screen.findByText("No rows match the search string. Nothing will change."),
    ).toBeInTheDocument();
    // Non-vacuous: Apply is keyed on `rows_matched > 0` — a regression that
    // drops that guard would show the button here.
    expect(
      screen.queryByRole("button", { name: /^Apply \(/ }),
    ).not.toBeInTheDocument();
  });
});
