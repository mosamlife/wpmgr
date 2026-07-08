import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";

import { MediaCleanerPanel } from "./MediaCleanerPanel";
import {
  useMediaCleanScan,
  useMediaCleanIsolate,
  useMediaCleanRestore,
  useMediaCleanDelete,
  useMediaCleanQuarantineList,
  type DeleteInput,
  type MediaCleanScanResultV2,
  type MediaCleanQuarantineList,
  type MediaCleanQuarantineManifest,
} from "./use-media-clean";
import type {
  MediaCleanDeleteResult,
  MediaCleanIsolateResult,
  MediaCleanRestoreResult,
} from "@wpmgr/api";

// P1 outcome test — GH #170 Wave 5.
//
// `src/features/tools/` had NO render test at all before this file. Permanent
// delete is the single irreversible action in the Media Cleaner (Isolate is
// reversible; Delete is not) and is gated behind typing the literal string
// "DELETE" — this drives the real `DeleteConfirmDialog` and asserts the
// mutation NEVER fires until that exact phrase is typed, then fires with the
// correct quarantine ids once it is.

vi.mock("./use-media-clean", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-media-clean")>();
  return {
    ...actual,
    useMediaCleanScan: vi.fn(),
    useMediaCleanIsolate: vi.fn(),
    useMediaCleanRestore: vi.fn(),
    useMediaCleanDelete: vi.fn(),
    useMediaCleanQuarantineList: vi.fn(),
  };
});

const mockedUseMediaCleanScan = vi.mocked(useMediaCleanScan);
const mockedUseMediaCleanIsolate = vi.mocked(useMediaCleanIsolate);
const mockedUseMediaCleanRestore = vi.mocked(useMediaCleanRestore);
const mockedUseMediaCleanDelete = vi.mocked(useMediaCleanDelete);
const mockedUseMediaCleanQuarantineList = vi.mocked(useMediaCleanQuarantineList);

function buildManifest(
  overrides: Partial<MediaCleanQuarantineManifest> = {},
): MediaCleanQuarantineManifest {
  return {
    manifest_id: "manifest-abc123",
    job_id: "job-1",
    isolated_at: Math.floor(Date.parse("2026-07-01T00:00:00Z") / 1000),
    total_files: 3,
    entries: [{ attachment_id: 42, title: "old-banner.jpg", file_count: 3 }],
    ...overrides,
  };
}

describe("MediaCleanerPanel — permanent delete is gated behind typing DELETE", () => {
  let deleteMutateAsync: ReturnType<typeof vi.fn>;

  beforeEach(() => {
    vi.clearAllMocks();

    mockedUseMediaCleanScan.mockReturnValue(
      mockQueryResult<MediaCleanScanResultV2>({
        data: undefined,
        isFetching: false,
        isFetched: false,
        isSuccess: false,
      }),
    );
    mockedUseMediaCleanIsolate.mockReturnValue(
      mockMutationResult<MediaCleanIsolateResult, { attachmentIds: number[] }>({}),
    );
    mockedUseMediaCleanRestore.mockReturnValue(
      mockMutationResult<MediaCleanRestoreResult, { quarantineIds: string[] }>({}),
    );
    deleteMutateAsync = vi.fn();
    mockedUseMediaCleanDelete.mockReturnValue(
      mockMutationResult<MediaCleanDeleteResult, DeleteInput>({
        mutateAsync: deleteMutateAsync,
      }),
    );
    mockedUseMediaCleanQuarantineList.mockReturnValue(
      mockQueryResult<MediaCleanQuarantineList>({
        data: { ok: true, manifests: [buildManifest()] },
      }),
    );
  });

  it("never calls the delete mutation before 'DELETE' is typed exactly, and fires it with the correct manifest id once satisfied", () => {
    renderWithProviders(<MediaCleanerPanel siteId="site-1" canOperate />);

    // Switch to the Quarantine tab to reach the destructive action.
    fireEvent.click(screen.getByRole("tab", { name: /quarantine/i }));

    expect(screen.getByText(/isolated/i)).toBeInTheDocument();
    const deleteAllButton = screen.getByRole("button", {
      name: /permanently delete all quarantined attachments/i,
    });
    fireEvent.click(deleteAllButton);

    // Confirm dialog is open; the mutation has NOT fired.
    expect(
      screen.getByRole("heading", {
        name: "Permanently delete 1 quarantined batch?",
      }),
    ).toBeInTheDocument();
    expect(deleteMutateAsync).not.toHaveBeenCalled();

    const confirmButton = screen.getByRole("button", {
      name: "Delete permanently",
    });
    expect(confirmButton).toBeDisabled();

    // A disabled button click is a DOM no-op — nothing fires.
    fireEvent.click(confirmButton);
    expect(deleteMutateAsync).not.toHaveBeenCalled();

    const typedInput = screen.getByLabelText(/type/i);

    // Wrong case / wrong text must not enable the button either.
    fireEvent.change(typedInput, { target: { value: "delete" } });
    expect(confirmButton).toBeDisabled();
    fireEvent.click(confirmButton);
    expect(deleteMutateAsync).not.toHaveBeenCalled();

    // The exact literal "DELETE" satisfies the gate.
    fireEvent.change(typedInput, { target: { value: "DELETE" } });
    expect(confirmButton).toBeEnabled();

    fireEvent.click(confirmButton);

    expect(deleteMutateAsync).toHaveBeenCalledTimes(1);
    expect(deleteMutateAsync).toHaveBeenCalledWith({
      quarantineIds: ["manifest-abc123"],
      confirm: "DELETE",
    });
  });
});
