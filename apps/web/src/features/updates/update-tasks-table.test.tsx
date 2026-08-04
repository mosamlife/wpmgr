import { describe, it, expect, vi } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import type { UpdateTask } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import {
  parseWireTask,
  serverRetryFields,
} from "@/test/update-task-fixtures";

import {
  UpdateTasksTable,
  type TaskTableSelection,
} from "./update-tasks-table";

// Outcome test: visibility gap fix: a failed/rolled_back update task carries
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
  // parseWireTask round-trips the literal through JSON and the application's
  // own wire guard, so a fixture that is not a shape the control plane emits
  // fails here rather than silently proving nothing (GH #322).
  const status = overrides.status ?? "failed";
  return parseWireTask({
    id: "task-1",
    run_id: RUN_ID,
    tenant_id: TENANT_ID,
    site_id: SITE_ID,
    target_type: "plugin",
    target_slug: "woocommerce/woocommerce.php",
    status: "failed",
    created_at: "2026-07-08T00:00:00Z",
    updated_at: "2026-07-08T00:00:00Z",
    ...serverRetryFields(status),
    ...overrides,
  });
}

describe("UpdateTasksTable: failed task log disclosure", () => {
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

// GH #210: the worst-case rollback failure: an update causes a site-wide
// PHP fatal, so the rollback command is undeliverable (it rides the same
// WordPress request that's fataling), and an agent-side watchdog attempts
// automatic filesystem recovery. The backend keeps the existing
// failed/rolled_back status and communicates this purely through the
// detail/error text, so this MUST read as its own distinct, actionable
// condition (never the generic "Rolled back"/"Failed" copy).
describe("UpdateTasksTable: GH #210 site-down-recovery condition", () => {
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

// ---------------------------------------------------------------------------
// GH #336 - per-task retry selection
//
// The table is the only place an operator can compose a retry set, so these
// pin the three things that make the selection honest: a control is offered
// only where the SERVER said the task may be retried, a row that has none
// still says why, and adding the column does not misalign the log row.
// ---------------------------------------------------------------------------

function makeSelection(
  overrides: Partial<TaskTableSelection> = {},
): TaskTableSelection {
  return {
    isSelected: () => false,
    toggle: vi.fn(),
    setAllSelectable: vi.fn(),
    allSelectableSelected: false,
    someSelectableSelected: false,
    ...overrides,
  };
}

describe("UpdateTasksTable - retry selection column", () => {
  const failed = buildTask({
    id: "task-failed",
    site_id: "site-a",
    site_name: "shop.example.com",
    target_slug: "akismet/akismet.php",
    status: "failed",
  });
  const succeeded = buildTask({
    id: "task-succeeded",
    site_id: "site-b",
    site_name: "blog.example.com",
    target_slug: "akismet/akismet.php",
    status: "succeeded",
  });
  const running = buildTask({
    id: "task-running",
    site_id: "site-c",
    site_name: "news.example.com",
    target_slug: "akismet/akismet.php",
    status: "running",
  });

  it("offers a checkbox only for a task the server marked retryable, names the target and the site, and states the reason on rows that have none", () => {
    renderWithProviders(
      <UpdateTasksTable
        tasks={[failed, succeeded, running]}
        selection={makeSelection()}
      />,
    );

    // One header checkbox plus exactly one row checkbox: succeeded and
    // running are not selectable, and get no dead control.
    expect(screen.getAllByRole("checkbox")).toHaveLength(2);
    expect(
      screen.getByRole("checkbox", {
        name: "Select akismet/akismet.php update on shop.example.com",
      }),
    ).toBeInTheDocument();

    const rows = screen.getAllByTestId("update-task-row");
    expect(within(rows[1]!).queryByRole("checkbox")).not.toBeInTheDocument();
    expect(
      within(rows[1]!).getByText("Cannot be retried: this update succeeded."),
    ).toBeInTheDocument();
    expect(
      within(rows[2]!).getByText(
        "Cannot be retried: this update has not finished yet.",
      ),
    ).toBeInTheDocument();
  });

  it("toggles by task id and selects every selectable task from the header", () => {
    const selection = makeSelection();
    renderWithProviders(
      <UpdateTasksTable tasks={[failed, succeeded]} selection={selection} />,
    );

    fireEvent.click(
      screen.getByRole("checkbox", {
        name: "Select akismet/akismet.php update on shop.example.com",
      }),
    );
    expect(selection.toggle).toHaveBeenCalledWith("task-failed");

    fireEvent.click(
      screen.getByRole("checkbox", { name: "Select all retryable updates" }),
    );
    expect(selection.setAllSelectable).toHaveBeenCalledWith(true);
  });

  it("marks the header checkbox indeterminate for a partial selection and flips its label when everything is selected", () => {
    const { unmount } = renderWithProviders(
      <UpdateTasksTable
        tasks={[failed, succeeded]}
        selection={makeSelection({ someSelectableSelected: true })}
      />,
    );
    const partial = screen.getByRole("checkbox", {
      name: "Select all retryable updates",
    });
    expect((partial as HTMLInputElement).indeterminate).toBe(true);
    unmount();

    renderWithProviders(
      <UpdateTasksTable
        tasks={[failed, succeeded]}
        selection={makeSelection({
          allSelectableSelected: true,
          isSelected: (id) => id === "task-failed",
        })}
      />,
    );
    const all = screen.getByRole("checkbox", { name: "Clear selection" });
    expect((all as HTMLInputElement).checked).toBe(true);
    expect((all as HTMLInputElement).indeterminate).toBe(false);
  });

  it("keeps the expanded log row spanning the full table once the select column exists", () => {
    const withLog = buildTask({
      id: "task-log",
      site_name: "shop.example.com",
      status: "failed",
      error: FULL_LOG,
    });

    const { unmount } = renderWithProviders(
      <UpdateTasksTable tasks={[withLog]} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /view log/i }));
    expect(
      screen
        .getByTestId("update-task-log-row")
        .querySelector("td")
        ?.getAttribute("colspan"),
    ).toBe("5");
    unmount();

    renderWithProviders(
      <UpdateTasksTable tasks={[withLog]} selection={makeSelection()} />,
    );
    fireEvent.click(screen.getByRole("button", { name: /view log/i }));
    expect(
      screen
        .getByTestId("update-task-log-row")
        .querySelector("td")
        ?.getAttribute("colspan"),
    ).toBe("6");
  });

  it("names the site from the task row, not from the sites cache, which does not contain it on a run wider than one page", () => {
    // The sites list is paginated by the control plane; a 300 task run's
    // sites simply are not all in that cache. The task row always carries
    // the name, so both display and the checkbox label stay correct.
    renderWithProviders(
      <UpdateTasksTable
        tasks={[failed]}
        siteNames={new Map()}
        selection={makeSelection()}
      />,
    );
    const row = screen.getByTestId("update-task-row");
    expect(within(row).getByText("shop.example.com")).toBeInTheDocument();
    expect(within(row).queryByText(/^site-a/)).not.toBeInTheDocument();
  });
});
