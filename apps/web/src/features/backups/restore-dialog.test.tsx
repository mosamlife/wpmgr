import { describe, it, expect, vi, beforeEach } from "vitest";
import { fireEvent, screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult, mockQueryResult } from "@/test/query-mocks";

import { RestoreDialog } from "./restore-dialog";
import { useCreateRestore, type CreateRestoreResult } from "./use-backups";
import { useSqlInspection, type SqlInspectionState } from "./use-sql-inspection";
import type { BackupSnapshot, RestoreCreate } from "@wpmgr/api";

// P0 outcome test — GH #170 Wave 4.
//
// Restore is the most data-loss-adjacent flow in the web app: it overwrites
// a live site's files and/or database with no undo. Before this file,
// nothing rendered RestoreDialog at all — every existing test covered only
// extracted pure helpers, so a regression that fired the restore mutation
// before the typed confirmation gate, or against the wrong snapshot
// generation, would pass every existing test. These tests fail against both
// of those mistakes — see the "non-vacuous" notes inline and the verification
// performed (and reverted) alongside this file.
//
// Only `useCreateRestore` and `useSqlInspection` are mocked:
//   - `useCreateRestore` is the mutation the dialog must call with the
//     correct snapshot id + body, and must NOT call before confirmation.
//   - `useSqlInspection` backs the "Snapshot contents" preview
//     (`SqlInspectionCard`) that renders inside modal A by default; without
//     a mock it would fire a real network request on every render.

vi.mock("./use-backups", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-backups")>();
  return {
    ...actual,
    useCreateRestore: vi.fn(),
  };
});

vi.mock("./use-sql-inspection", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-sql-inspection")>();
  return {
    ...actual,
    useSqlInspection: vi.fn(),
  };
});

const mockedUseCreateRestore = vi.mocked(useCreateRestore);
const mockedUseSqlInspection = vi.mocked(useSqlInspection);

function buildSnapshot(overrides: Partial<BackupSnapshot>): BackupSnapshot {
  return {
    id: "snap-default",
    tenant_id: "tenant-1",
    site_id: "site-1",
    kind: "full",
    status: "completed",
    created_at: "2026-07-01T00:00:00Z",
    updated_at: "2026-07-01T00:05:00Z",
    ...overrides,
  };
}

beforeEach(() => {
  vi.clearAllMocks();
  // The "Snapshot contents" preview card is irrelevant to these tests; keep
  // it in a quiet, settled state so it never interferes with queries below.
  mockedUseSqlInspection.mockReturnValue(
    mockQueryResult<{ state: SqlInspectionState; analyzingSince: number | null }>(
      { data: { state: { phase: "unwired" }, analyzingSince: null } },
    ),
  );
});

describe("RestoreDialog — destructive-confirm gate", () => {
  it("never calls the restore mutation before the confirm phrase is typed, and calls it with the correct body once satisfied", async () => {
    let latestHookSnapshotId = "";
    const capturedMutateIds: string[] = [];
    const mutateAsyncMock = vi.fn(
      (_body: RestoreCreate): Promise<CreateRestoreResult> => {
        capturedMutateIds.push(latestHookSnapshotId);
        return Promise.resolve({
          snapshot: buildSnapshot({ id: latestHookSnapshotId }),
          restore_run_id: "run-123",
        });
      },
    );
    mockedUseCreateRestore.mockImplementation((id: string) => {
      latestHookSnapshotId = id;
      return mockMutationResult<CreateRestoreResult, RestoreCreate>({
        mutateAsync: mutateAsyncMock,
      });
    });

    const onClose = vi.fn();
    renderWithProviders(
      <RestoreDialog
        open
        onClose={onClose}
        snapshotId="snap-abc123"
        entries={[]}
        siteHost="example.com"
      />,
    );

    // Modal A is open; the destructive-confirm control (modal B) is NOT yet
    // mounted — the operator hasn't asked to restore anything yet.
    expect(
      screen.getByRole("heading", { name: "Restore from snapshot" }),
    ).toBeInTheDocument();
    expect(screen.queryByLabelText(/type/i)).not.toBeInTheDocument();
    expect(mutateAsyncMock).not.toHaveBeenCalled();

    // Step 1 -> Step 2: open the destructive-confirm gate.
    fireEvent.click(screen.getByRole("button", { name: "Apply restore" }));

    const confirmInput = await screen.findByLabelText(/type/i);
    expect(confirmInput).toBeInTheDocument();

    // The gate's own confirm button. Radix marks modal A `aria-hidden` while
    // the stacked confirm dialog is open (correct modal semantics — only the
    // topmost dialog is exposed to the accessibility tree), so this uniquely
    // resolves to modal B's button without further disambiguation.
    const confirmButton = screen.getByRole("button", { name: "Apply restore" });
    expect(confirmButton).toBeDisabled();

    // Clicking a disabled button is a no-op in the DOM — nothing should fire.
    fireEvent.click(confirmButton);
    expect(mutateAsyncMock).not.toHaveBeenCalled();

    // Typing the WRONG value must not enable the button either.
    fireEvent.change(confirmInput, { target: { value: "not-the-host" } });
    expect(confirmButton).toBeDisabled();
    fireEvent.click(confirmButton);
    expect(mutateAsyncMock).not.toHaveBeenCalled();

    // Typing the CORRECT resource name (siteHost) satisfies the gate.
    fireEvent.change(confirmInput, { target: { value: "example.com" } });
    expect(confirmButton).toBeEnabled();

    fireEvent.click(confirmButton);

    expect(mutateAsyncMock).toHaveBeenCalledTimes(1);
    // Mode defaults to "everything" / narrow "full" -> the body carries
    // `full: true` and no `components` restriction.
    expect(mutateAsyncMock).toHaveBeenCalledWith(
      expect.objectContaining({ full: true }),
    );
    // The mutation hook must have been constructed against the SAME snapshot
    // id the dialog was opened for — never a stale/default/wrong id.
    expect(capturedMutateIds).toEqual(["snap-abc123"]);
  });
});

describe("RestoreDialog — incremental-chain restore point selection (issue #177)", () => {
  it("submits the SELECTED generation's id, not the chain tip, when the operator rolls back", async () => {
    const tipSnapshot = buildSnapshot({
      id: "chain-tip-id",
      generation: 1,
      created_at: "2026-07-05T00:00:00Z",
    });
    const baseSnapshot = buildSnapshot({
      id: "chain-base-id",
      generation: 0,
      created_at: "2026-07-01T00:00:00Z",
    });

    let latestHookSnapshotId = "";
    const capturedMutateIds: string[] = [];
    const mutateAsyncMock = vi.fn(
      (_body: RestoreCreate): Promise<CreateRestoreResult> => {
        capturedMutateIds.push(latestHookSnapshotId);
        return Promise.resolve({
          snapshot: buildSnapshot({ id: latestHookSnapshotId }),
          restore_run_id: "run-456",
        });
      },
    );
    mockedUseCreateRestore.mockImplementation((id: string) => {
      latestHookSnapshotId = id;
      return mockMutationResult<CreateRestoreResult, RestoreCreate>({
        mutateAsync: mutateAsyncMock,
      });
    });

    renderWithProviders(
      <RestoreDialog
        open
        onClose={vi.fn()}
        snapshotId="chain-tip-id"
        entries={[]}
        siteHost="example.com"
        chainSnapshots={[tipSnapshot, baseSnapshot]}
      />,
    );

    // Default selection is the tip (newest generation).
    expect(mockedUseCreateRestore).toHaveBeenLastCalledWith("chain-tip-id");

    // Operator explicitly rolls back to the earlier (base) generation.
    // Filter by the radio group's `name` attribute (not the accessible
    // name) to disambiguate from the "What to restore" mode radios, which
    // use a different group ("restore-mode") but the same `role="radio"`.
    const radios = screen
      .getAllByRole("radio")
      .filter((r) => r.getAttribute("name") === "restore-version");
    const baseRadio = radios.find(
      (r) => (r as HTMLInputElement).value === "chain-base-id",
    );
    expect(baseRadio).toBeDefined();
    fireEvent.click(baseRadio!);

    // The hook must now be constructed against the BASE id.
    expect(mockedUseCreateRestore).toHaveBeenLastCalledWith("chain-base-id");

    fireEvent.click(screen.getByRole("button", { name: "Apply restore" }));
    const confirmInput = await screen.findByLabelText(/type/i);
    fireEvent.change(confirmInput, { target: { value: "example.com" } });
    // Modal A is now `aria-hidden` (Radix stacked-dialog semantics), so this
    // uniquely resolves to modal B's confirm button.
    fireEvent.click(screen.getByRole("button", { name: "Apply restore" }));

    expect(mutateAsyncMock).toHaveBeenCalledTimes(1);
    // The single most important invariant of the version-picker: the id
    // submitted to the CP is the one the operator selected, not the tip.
    expect(capturedMutateIds).toEqual(["chain-base-id"]);
  });
});
