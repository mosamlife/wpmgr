import { describe, it, expect, vi } from "vitest";
import { fireEvent, screen, within } from "@testing-library/react";
import type { UpdateTask } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import {
  CLOCK_403_DETAIL,
  CLOCK_403_RAW_ERROR,
  HEALTH_CHECK_FAILED_REASON,
  CORE_LEFT_AS_IS_DETAIL,
  CORE_NO_CHANGE_UNHEALTHY_DETAIL,
  ROLLBACK_RAW_ERROR,
  CORE_ROLLBACK_UNDELIVERABLE_DETAIL,
  FIREWALL_403_DETAIL,
  FIREWALL_403_RAW_ERROR,
  PLUGIN_SITE_DOWN_DETAIL,
} from "@/test/update-task-details";
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

// GH #755 round 2 (T4, N4): an agent self-update redirect comes back
// `skipped`, not `failed`/`rolled_back`. Before isRedirectFailure had its own
// status set this fell through to the generic truncated-detail cell instead
// of the non-truncating alert treatment every other redirect-failure variant
// gets.
describe("UpdateTasksTable: GH #755 round 2 redirect-failure condition on a skipped agent self-update task", () => {
  it("renders the full server detail in a non-truncated role=alert element for a skipped agent self-update task", () => {
    const longDetail =
      "Agent self-update not started. https://example.com redirects to https://www.example.com, so no command was sent. If WordPress on the site reports https://www.example.com as its address, the saved address updates to https://www.example.com automatically at a later check-in from the site. That update does not happen while another site in this workspace uses https://www.example.com: if one does, remove or change the duplicate site.";
    const task = buildTask({
      id: "task-agent-redirect",
      target_type: "agent",
      target_slug: "agent",
      status: "skipped",
      detail: longDetail,
      error:
        "agent_self_update command: https://example.com redirects to https://www.example.com/wp-json/wpmgr/v1/command/agent_self_update (HTTP 301); commands are sent only to the site's saved address, so the redirect was not followed",
    });

    renderWithProviders(<UpdateTasksTable tasks={[task]} />);

    const row = screen.getByTestId("update-task-row");
    const callout = within(row).getByRole("alert");
    expect(callout).toHaveTextContent(longDetail);
    expect(callout.className).not.toContain("truncate");
  });
});

// ---------------------------------------------------------------------------
// GH #679: an update outcome reads in full.
//
// The control plane writes the sentence an operator acts on into a task's
// detail and keeps the raw reply in its error. The Detail cell used to clip
// every ordinary detail to one line, so a firewall or clock explanation was
// readable only as a tooltip. jsdom has no layout, so "clipped" is checked the
// way the markup states it: no element from the text up to its table cell may
// carry a class that cuts text off.
// ---------------------------------------------------------------------------

const CLIPPING_CLASS =
  /(^|\s)(truncate|text-ellipsis|whitespace-nowrap|line-clamp-\d+|overflow-hidden)(\s|$)/;

function expectUnclipped(el: HTMLElement) {
  const cell = el.closest("td");
  expect(cell, "the text is inside a table cell").not.toBeNull();
  for (
    let node: HTMLElement | null = el;
    node && node !== cell?.parentElement;
    node = node.parentElement
  ) {
    expect(node.className, `<${node.tagName.toLowerCase()}> clips its text`).not.toMatch(
      CLIPPING_CLASS,
    );
  }
}

describe("UpdateTasksTable: a refused update reads in full (GH #679)", () => {
  it.each([
    ["a firewall block", FIREWALL_403_DETAIL, FIREWALL_403_RAW_ERROR],
    ["a clock difference", CLOCK_403_DETAIL, CLOCK_403_RAW_ERROR],
  ])(
    "shows the sentence for %s unclipped, and keeps the raw reply behind the log",
    (_label, detail, rawError) => {
      renderWithProviders(
        <UpdateTasksTable
          tasks={[buildTask({ status: "failed", detail, error: rawError })]}
        />,
      );
      const row = screen.getByTestId("update-task-row");

      expectUnclipped(within(row).getByText(detail));

      // The raw reply is not what the row says; it is one click away.
      expect(screen.queryByText(rawError)).not.toBeInTheDocument();
      fireEvent.click(within(row).getByRole("button", { name: /view log/i }));
      expect(screen.getByText(rawError)).toBeInTheDocument();
    },
  );

  it("gives any long detail the same room, so no message needs its own rule", () => {
    // worker.go: the detail a succeeded apply carries when the public probe
    // could not confirm the front end.
    const detail =
      "updated; signed agent check confirmed the backend healthy, but the public probe was inconclusive after 3 attempt(s): status=404 Not Found, so the front end could not be separately confirmed";
    renderWithProviders(
      <UpdateTasksTable tasks={[buildTask({ status: "succeeded", detail })]} />,
    );
    expectUnclipped(
      within(screen.getByTestId("update-task-row")).getByText(detail),
    );
  });

  it("still points a failed task that has no detail at its log", () => {
    renderWithProviders(
      <UpdateTasksTable
        tasks={[
          buildTask({
            status: "failed",
            detail: undefined,
            error: FIREWALL_403_RAW_ERROR,
          }),
        ]}
      />,
    );
    const row = screen.getByTestId("update-task-row");
    expect(within(row).getByText("See log for details")).toBeInTheDocument();
    expect(
      within(row).getByRole("button", { name: /view log/i }),
    ).toBeInTheDocument();
  });
});

// ---------------------------------------------------------------------------
// GH #415: WordPress core has no automatic recovery.
//
// The chip that says "recovery attempted" describes the agent's update
// watchdog, which exists for plugins and themes only. A core task whose
// rollback could not be delivered is a site that needs a person, and the chip
// must say so; a core task the control plane left in place is an ordinary
// failure.
// ---------------------------------------------------------------------------

function coreTask(overrides: Partial<UpdateTask>): UpdateTask {
  return buildTask({
    target_type: "core",
    target_slug: "core",
    from_version: "6.6.2",
    to_version: "6.7.1",
    ...overrides,
  });
}

describe("UpdateTasksTable: WordPress core outcomes (GH #415)", () => {
  it("says manual recovery is needed, not that recovery was attempted, when core's rollback could not be delivered", () => {
    renderWithProviders(
      <UpdateTasksTable
        tasks={[
          coreTask({
            status: "failed",
            detail: CORE_ROLLBACK_UNDELIVERABLE_DETAIL,
            error: ROLLBACK_RAW_ERROR,
          }),
        ]}
      />,
    );
    const row = screen.getByTestId("update-task-row");

    expect(
      within(row).getByText("Site down, manual recovery needed"),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Site down, recovery attempted"),
    ).not.toBeInTheDocument();
    // The whole sentence is in the alert, unclipped, and it is the same
    // sentence the chip summarises.
    const alert = within(row).getByRole("alert");
    expect(alert).toHaveTextContent(CORE_ROLLBACK_UNDELIVERABLE_DETAIL);
    expectUnclipped(alert);
  });

  it.each([
    ["left as is after a failed health check", CORE_LEFT_AS_IS_DETAIL],
    [
      "reported no change but failed the health check",
      CORE_NO_CHANGE_UNHEALTHY_DETAIL,
    ],
  ])(
    "keeps a plain Failed chip for core that %s, with its full sentence",
    (_label, detail) => {
      renderWithProviders(
        <UpdateTasksTable
          tasks={[
            coreTask({
              status: "failed",
              detail,
              error: HEALTH_CHECK_FAILED_REASON,
            }),
          ]}
        />,
      );
      const row = screen.getByTestId("update-task-row");

      expect(within(row).getByText("Failed")).toBeInTheDocument();
      expect(
        screen.queryByText(/^Site down,/),
      ).not.toBeInTheDocument();
      expectUnclipped(within(row).getByText(detail));
    },
  );

  it("does not call a core task that was rolled back a site that is down", () => {
    renderWithProviders(
      <UpdateTasksTable
        tasks={[
          coreTask({
            status: "rolled_back",
            detail: `rolled back: ${HEALTH_CHECK_FAILED_REASON}`,
          }),
        ]}
      />,
    );
    const row = screen.getByTestId("update-task-row");
    expect(within(row).getByText("Rolled back")).toBeInTheDocument();
    expect(screen.queryByText(/^Site down,/)).not.toBeInTheDocument();
  });

  it("keeps the recovery-attempted chip for a plugin, where the agent's watchdog does run", () => {
    renderWithProviders(
      <UpdateTasksTable
        tasks={[
          buildTask({
            status: "failed",
            detail: PLUGIN_SITE_DOWN_DETAIL,
            error: ROLLBACK_RAW_ERROR,
          }),
        ]}
      />,
    );
    const row = screen.getByTestId("update-task-row");
    expect(
      within(row).getByText("Site down, recovery attempted"),
    ).toBeInTheDocument();
    expect(
      screen.queryByText("Site down, manual recovery needed"),
    ).not.toBeInTheDocument();
    expect(within(row).getByRole("alert")).toHaveTextContent(
      PLUGIN_SITE_DOWN_DETAIL,
    );
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
