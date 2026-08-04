import { describe, it, expect, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import type { UpdateTask } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { parseWireTask, serverRetryFields } from "@/test/update-task-fixtures";

import { RetryActionBar } from "./retry-action-bar";

// GH #336 - the action bar carries the EFFECTIVE count, in tasks, labelled as
// updates, on both the button and the toolbar's accessible name, and says what
// the selection is made of underneath. A 20 site by 5 plugin run has 100 failed
// tasks across 20 sites, so counting sites here would be a lie.

const RUN_ID = "9c3f1f9e-1f6e-4a5e-9a2f-0f0f0f0f0f0f";
const TENANT_ID = "22222222-2222-2222-2222-222222222222";

function task(overrides: Partial<UpdateTask>): UpdateTask {
  const status = overrides.status ?? "failed";
  return parseWireTask({
    id: "task-1",
    run_id: RUN_ID,
    tenant_id: TENANT_ID,
    site_id: "site-1",
    site_name: "one.example.com",
    target_type: "agent",
    target_slug: "wpmgr",
    status: "failed",
    created_at: "2026-08-04T09:00:00Z",
    updated_at: "2026-08-04T09:10:00Z",
    ...serverRetryFields(status),
    ...overrides,
  });
}

function incidentSelection(): UpdateTask[] {
  return [
    task({ id: "t-canary", status: "failed" }),
    ...Array.from({ length: 20 }, (_, i) =>
      task({ id: `t-withheld-${i}`, site_id: `site-${i}`, status: "cancelled" }),
    ),
  ];
}

describe("RetryActionBar", () => {
  it("stays out of the way until something is selected", () => {
    renderWithProviders(
      <RetryActionBar
        selectedTasks={[]}
        target="agent"
        dryRun={false}
        onClear={vi.fn()}
        onRetry={vi.fn()}
      />,
    );
    expect(screen.queryByRole("toolbar")).not.toBeInTheDocument();
  });

  it("counts tasks as updates on the button and in the toolbar's name, and breaks the selection down by server class", () => {
    renderWithProviders(
      <RetryActionBar
        selectedTasks={incidentSelection()}
        target="agent"
        dryRun={false}
        onClear={vi.fn()}
        onRetry={vi.fn()}
      />,
    );

    expect(
      screen.getByRole("toolbar", { name: "21 updates selected" }),
    ).toBeInTheDocument();
    expect(screen.getByText("21 selected")).toBeInTheDocument();
    expect(screen.getByText("20 not attempted, 1 failed")).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Retry 21 agent updates" }),
    ).toBeInTheDocument();
  });

  it("carries the dry run into the button label", () => {
    renderWithProviders(
      <RetryActionBar
        selectedTasks={incidentSelection()}
        target="agent"
        dryRun
        onClear={vi.fn()}
        onRetry={vi.fn()}
      />,
    );
    expect(
      screen.getByRole("button", { name: "Retry 21 agent updates (dry run)" }),
    ).toBeInTheDocument();
  });

  it("wires clear and retry", () => {
    const onClear = vi.fn();
    const onRetry = vi.fn();
    renderWithProviders(
      <RetryActionBar
        selectedTasks={[task({ id: "t-1", status: "failed" })]}
        target="agent"
        dryRun={false}
        onClear={onClear}
        onRetry={onRetry}
      />,
    );

    fireEvent.click(screen.getByRole("button", { name: "Clear selection" }));
    expect(onClear).toHaveBeenCalledTimes(1);

    fireEvent.click(screen.getByRole("button", { name: "Retry 1 agent update" }));
    expect(onRetry).toHaveBeenCalledTimes(1);
  });
});
