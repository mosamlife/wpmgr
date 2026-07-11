import { describe, it, expect } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import type { UpdateTask } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";

import { UpdateTasksTable } from "./update-tasks-table";

// Outcome test — visibility gap fix: a failed/rolled_back update task carries
// the agent's full diagnostic log in `task.error`, but the run-detail Tasks
// table used to render only the short `task.detail` string, truncated. This
// pins the fix: the full `task.error` text must be reachable in one click via
// a disclosure, alongside a copy affordance, and MUST NOT render for a task
// whose `error` is empty (nothing to disclose).

const RUN_ID = "11111111-1111-1111-1111-111111111111";
const TENANT_ID = "22222222-2222-2222-2222-222222222222";
const SITE_ID = "33333333-3333-3333-3333-333333333333";

const FULL_LOG =
  "Update incomplete; auto-restored the pre-update snapshot. Reason: " +
  "activation check failed after replacing plugin files.\n" +
  "on-disk version: 10.8.0\n" +
  "expected version: 10.8.1\n" +
  "snapshot restored from: pre-update-2026-07-08T00-00-00Z";

function buildTask(overrides: Partial<UpdateTask>): UpdateTask {
  return {
    id: "task-1",
    run_id: RUN_ID,
    tenant_id: TENANT_ID,
    site_id: SITE_ID,
    target_type: "plugin",
    target_slug: "woocommerce/woocommerce.php",
    status: "failed",
    created_at: "2026-07-08T00:00:00Z",
    updated_at: "2026-07-08T00:00:00Z",
    ...overrides,
  };
}

describe("UpdateTasksTable — failed task log disclosure", () => {
  it("reveals the full agent log + copy affordance for a task with a non-empty error, and renders no disclosure for a task with an empty error", () => {
    const failedTask = buildTask({
      id: "task-failed",
      status: "rolled_back",
      detail: "agent reported update failure",
      error: FULL_LOG,
    });
    const cleanFailure = buildTask({
      id: "task-no-log",
      target_slug: "another-plugin/another-plugin.php",
      status: "failed",
      detail: "agent reported update failure",
      error: undefined,
    });

    renderWithProviders(
      <UpdateTasksTable tasks={[failedTask, cleanFailure]} />,
    );

    const rows = screen.getAllByTestId("update-task-row");
    expect(rows).toHaveLength(2);

    // The task with no error has nothing to disclose: no toggle at all.
    expect(
      within(rows[1]!).queryByRole("button", { name: /view log/i }),
    ).not.toBeInTheDocument();

    // The full log text is never rendered until the disclosure is opened.
    expect(screen.queryByText(/activation check failed/)).not.toBeInTheDocument();

    // The failed task's row has exactly one toggle, and it opens the log.
    const toggle = within(rows[0]!).getByRole("button", { name: /view log/i });
    expect(toggle).toHaveAttribute("aria-expanded", "false");

    fireEvent.click(toggle);

    expect(toggle).toHaveAttribute("aria-expanded", "true");

    // Full multi-line log is rendered verbatim (newlines preserved via <pre>),
    // not just the short generic detail string.
    const logPanel = screen.getByText(/activation check failed/).closest("pre");
    expect(logPanel).not.toBeNull();
    expect(logPanel?.textContent).toContain(FULL_LOG);
    expect(logPanel?.textContent).toContain("on-disk version: 10.8.0");

    // Copy affordance is present and scoped to the log panel.
    expect(
      screen.getByRole("button", { name: /copy agent log/i }),
    ).toBeInTheDocument();

    // Still exactly one disclosure toggle in the whole table (the clean
    // failure never grew one).
    expect(screen.getAllByRole("button", { name: /view log|hide log/i })).toHaveLength(1);
  });

  it("renders nothing (empty state) when there are no tasks", () => {
    renderWithProviders(<UpdateTasksTable tasks={[]} />);
    expect(screen.getByText(/no tasks yet/i)).toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });
});

// GH #210 — the worst-case rollback failure: an update causes a site-wide
// PHP fatal, so the rollback command is undeliverable (it rides the same
// WordPress request that's fataling), and an agent-side watchdog attempts
// automatic filesystem recovery. The backend keeps the existing
// failed/rolled_back status and communicates this purely through the
// detail/error text, so this MUST read as its own distinct, actionable
// condition (never the generic "Rolled back"/"Failed" copy).
describe("UpdateTasksTable — GH #210 site-down-recovery condition", () => {
  it("renders the distinct site-down badge + non-truncated alert callout for a rolled_back task whose detail names the condition, instead of the generic 'Rolled back' copy", () => {
    const task = buildTask({
      id: "task-site-down",
      status: "rolled_back",
      detail:
        "The site went down site-wide during this update. The rollback command was undeliverable; an automatic filesystem recovery was attempted.",
      error: "Fatal error: watchdog restore log ...",
    });

    renderWithProviders(<UpdateTasksTable tasks={[task]} />);

    const row = screen.getByTestId("update-task-row");

    // Distinct badge label, not the generic "Rolled back" chip.
    expect(within(row).getByText("Site down, recovery attempted")).toBeInTheDocument();
    expect(within(row).queryByText("Rolled back")).not.toBeInTheDocument();

    // The full detail text is surfaced directly (not truncated behind a
    // title attribute) inside an alert-role callout.
    const callout = within(row).getByRole("alert");
    expect(callout).toHaveTextContent(/site went down site-wide/i);
    expect(callout).toHaveTextContent(/automatic filesystem recovery/i);
  });

  it("renders the distinct treatment for a failed task whose error (not detail) names the condition, falling back to the canned explanation since detail is empty, while the raw error stays reachable via the log disclosure", () => {
    const task = buildTask({
      id: "task-failed-site-down",
      status: "failed",
      detail: undefined,
      error:
        "Site is not responding after the update; agent watchdog attempted automatic recovery of the filesystem.",
    });

    renderWithProviders(<UpdateTasksTable tasks={[task]} />);

    const row = screen.getByTestId("update-task-row");
    expect(within(row).getByText("Site down, recovery attempted")).toBeInTheDocument();
    // No detail was provided, so the callout falls back to the canned
    // explanation rather than rendering nothing.
    expect(within(row).getByRole("alert")).toHaveTextContent(
      /automatic filesystem recovery was attempted/i,
    );
    // The raw agent error is still reachable one click away, unchanged from
    // the existing log-disclosure behavior.
    fireEvent.click(within(row).getByRole("button", { name: /view log/i }));
    expect(screen.getByText(/agent watchdog attempted automatic recovery/i)).toBeInTheDocument();
  });

  it("leaves an ordinary rolled_back / failed task on the generic status copy (no false positive)", () => {
    const rolledBack = buildTask({
      id: "task-ordinary-rollback",
      status: "rolled_back",
      detail: "agent reported update failure",
      error: "activation check failed after replacing plugin files",
    });
    const failed = buildTask({
      id: "task-ordinary-failed",
      target_slug: "another-plugin/another-plugin.php",
      status: "failed",
      detail: "connection timed out",
    });

    renderWithProviders(<UpdateTasksTable tasks={[rolledBack, failed]} />);

    const rows = screen.getAllByTestId("update-task-row");
    expect(within(rows[0]!).getByText("Rolled back")).toBeInTheDocument();
    expect(within(rows[1]!).getByText("Failed")).toBeInTheDocument();
    expect(screen.queryByText("Site down, recovery attempted")).not.toBeInTheDocument();
  });
});
