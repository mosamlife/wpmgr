import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { fireEvent, screen, waitFor } from "@testing-library/react";
import { client } from "@wpmgr/api";
import type { UpdateTask } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { parseWireTask, serverRetryFields } from "@/test/update-task-fixtures";

import { RetryRunDialog } from "./retry-dialog";

// GH #336 - the confirmation and, above all, the PARTIAL COMMIT SURFACE.
//
// These tests drive the REAL transport: the dialog calls the generated
// `retryUpdateRun` operation, which builds the real URL and serialises the
// real body, and only `fetch` is replaced. The bodies below are the JSON the
// control plane returns per packages/openapi/openapi.yaml
// (UpdateRunRetryResult), so a fixture the server would never produce cannot
// make these pass. Task fixtures go through the same wire guard the
// application uses.
//
// The requirement that died three design rounds: if `created` is lower than
// `requested`, the operator must SEE it, with numbers and reasons, before
// anything navigates. `navigates silently only on a complete commit` and
// `renders the shortfall` are the two tests that pin it.

const RUN_ID = "9c3f1f9e-1f6e-4a5e-9a2f-0f0f0f0f0f0f";
const NEW_RUN_ID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa";
const TENANT_ID = "22222222-2222-2222-2222-222222222222";
const NEW_TASK_ID = "55555555-5555-5555-5555-555555555555";

const toastWarning = vi.fn();
vi.mock("@/components/toast", () => ({
  toast: {
    warning: (title: string, opts?: unknown) => {
      toastWarning(title, opts);
    },
    success: vi.fn(),
    error: vi.fn(),
    info: vi.fn(),
  },
}));

function task(overrides: Partial<UpdateTask>): UpdateTask {
  const status = overrides.status ?? "cancelled";
  return parseWireTask({
    id: "task-1",
    run_id: RUN_ID,
    tenant_id: TENANT_ID,
    site_id: "site-1",
    site_name: "one.example.com",
    target_type: "agent",
    target_slug: "wpmgr",
    status: "cancelled",
    created_at: "2026-08-04T09:00:00Z",
    updated_at: "2026-08-04T09:10:00Z",
    ...serverRetryFields(status),
    ...overrides,
  });
}

/** The 21 task incident: one failed canary plus 20 withheld sites. */
function incidentTasks(): UpdateTask[] {
  return [
    task({
      id: "task-canary",
      site_id: "site-canary",
      site_name: "shop.example.com",
      status: "failed",
    }),
    ...Array.from({ length: 20 }, (_, i) =>
      task({
        id: `task-withheld-${i}`,
        site_id: `site-${i}`,
        site_name: `site-${i}.example.com`,
        status: "cancelled",
      }),
    ),
  ];
}

let fetchMock: ReturnType<typeof vi.fn>;
let restoreConfig: () => void;

function jsonResponse(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

beforeEach(() => {
  toastWarning.mockReset();
  fetchMock = vi.fn();
  // jsdom cannot build a Request from a relative URL, and the app's client is
  // deliberately configured with an empty baseUrl (same origin). Point it at
  // the jsdom origin for the duration of the test and put the mock transport
  // in place; everything else about the call stays real.
  const previous = client.getConfig();
  client.setConfig({
    ...previous,
    baseUrl: "http://localhost",
    fetch: fetchMock,
  });
  restoreConfig = () => client.setConfig(previous);
});

afterEach(() => {
  restoreConfig();
});

function renderDialog(
  overrides: Partial<Parameters<typeof RetryRunDialog>[0]> = {},
) {
  const onOpenRun = vi.fn();
  const onClose = vi.fn();
  const tasks = incidentTasks();
  renderWithProviders(
    <RetryRunDialog
      open
      onClose={onClose}
      runId={RUN_ID}
      dryRun={false}
      selectedTasks={tasks}
      allTasks={tasks}
      haltReason="the canary site did not confirm the new agent version"
      onOpenRun={onOpenRun}
      {...overrides}
    />,
  );
  return { onOpenRun, onClose };
}

describe("RetryRunDialog - what it says before the click", () => {
  it("counts tasks as updates, names the sites only as a qualifier, and states the wave shape for an agent rollout", () => {
    renderDialog();

    expect(
      screen.getByRole("heading", { name: "Retry 21 agent updates" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        /21 updates on 21 sites will be requested\. This creates a new run and leaves this one exactly as it is\./,
      ),
    ).toBeInTheDocument();
    expect(screen.getByText(/Selected: 20 not attempted, 1 failed\./)).toBeInTheDocument();
    expect(
      screen.getByText(/starts a fresh staged rollout: one site first/),
    ).toBeInTheDocument();
    expect(
      screen.getByText(
        /the canary site did not confirm the new agent version/,
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Retry 21 agent updates" }),
    ).toBeEnabled();
  });

  it("informs about a rolled back update without gating it behind a typed token", () => {
    const reverted = task({
      id: "task-reverted",
      target_type: "plugin",
      target_slug: "akismet/akismet.php",
      status: "rolled_back",
    });
    renderDialog({ selectedTasks: [reverted], allTasks: [reverted] });

    expect(
      screen.getByText(
        /1 update in this selection was rolled back\. The update applied, the site then failed its health check, and the change was reverted automatically\./,
      ),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/can reproduce the same break/),
    ).toBeInTheDocument();

    // No typed gate: the same operator can already retry the same rolled back
    // update with one unconfirmed click from the site's own updates card, so
    // friction here would only push them to the worse informed door.
    expect(screen.queryByRole("textbox")).not.toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Retry 1 plugin update" }),
    ).toBeEnabled();
  });

  it("says plainly when a one task agent retry has no canary ahead of it", () => {
    const only = task({ id: "task-only", status: "failed" });
    renderDialog({ selectedTasks: [only], allTasks: [only] });
    expect(
      screen.getByText(
        /This retry has only one site, so there is no canary ahead of it\./,
      ),
    ).toBeInTheDocument();
  });

  it("carries the dry run into the title and the button, so it cannot be missed", () => {
    renderDialog({ dryRun: true });
    expect(
      screen.getByRole("heading", { name: "Retry 21 agent updates (dry run)" }),
    ).toBeInTheDocument();
    expect(
      screen.getByRole("button", { name: "Retry 21 agent updates (dry run)" }),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/Dry run\. Nothing will be applied to any site\./),
    ).toBeInTheDocument();
  });
});

describe("RetryRunDialog - the request", () => {
  it("posts the selected task ids to the run's retry endpoint, as tasks", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(200, {
        run_id: NEW_RUN_ID,
        requested: 21,
        created: 21,
        excluded: [],
      }),
    );
    const { onOpenRun } = renderDialog();

    fireEvent.click(
      screen.getByRole("button", { name: "Retry 21 agent updates" }),
    );

    await waitFor(() => expect(fetchMock).toHaveBeenCalledTimes(1));
    const request = fetchMock.mock.calls[0]?.[0] as Request;
    expect(request.method).toBe("POST");
    expect(request.url).toBe(
      `http://localhost/api/v1/updates/runs/${RUN_ID}/retry`,
    );
    const body = (await request.clone().json()) as { task_ids: string[] };
    expect(body.task_ids).toHaveLength(21);
    expect(body.task_ids[0]).toBe("task-canary");

    // A complete commit navigates: navigation is the feedback everywhere else
    // in this app, and there is nothing withheld to read.
    await waitFor(() => expect(onOpenRun).toHaveBeenCalledWith(NEW_RUN_ID));
    expect(screen.queryByTestId("retry-partial-result")).not.toBeInTheDocument();
  });
});

describe("RetryRunDialog - the partial commit surface", () => {
  it("holds the operator on a result step, with numbers, reasons and the server's own sentence for every update that did not start", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(200, {
        run_id: NEW_RUN_ID,
        requested: 21,
        created: 18,
        excluded: [
          {
            task_id: "task-withheld-0",
            reason: "site_not_enrolled",
            message: "site-0.example.com is no longer enrolled.",
          },
          {
            task_id: "task-withheld-1",
            reason: "agent_current",
            message: "site-1.example.com already runs agent 0.61.116.",
          },
          {
            task_id: "task-withheld-2",
            reason: "agent_current",
            message: "site-2.example.com already runs agent 0.61.116.",
          },
        ],
      }),
    );
    const { onOpenRun } = renderDialog();

    fireEvent.click(
      screen.getByRole("button", { name: "Retry 21 agent updates" }),
    );

    const panel = await screen.findByTestId("retry-partial-result");

    // Both numbers, in the unit the operator selected in.
    expect(
      screen.getByRole("heading", { name: "Started 18 of 21 updates" }),
    ).toBeInTheDocument();
    expect(panel).toHaveTextContent("3 of 21 updates did not start.");

    // Grouped by the machine reason, counted, with the site each belongs to
    // and the control plane's own explanation rendered as it arrived.
    expect(panel).toHaveTextContent("site no longer connected");
    expect(panel).toHaveTextContent("already on the published agent version");
    expect(panel).toHaveTextContent(
      "site-1.example.com (agent): site-1.example.com already runs agent 0.61.116.",
    );
    expect(panel).toHaveTextContent(
      "site-0.example.com (agent): site-0.example.com is no longer enrolled.",
    );

    // NOTHING navigated on its own.
    expect(onOpenRun).not.toHaveBeenCalled();

    // The numbers also survive the navigation the operator is about to make.
    expect(toastWarning).toHaveBeenCalledWith(
      "Started 18 of 21 updates",
      expect.objectContaining({
        description: "3 did not start. Open the run to see what did.",
      }),
    );

    fireEvent.click(screen.getByRole("button", { name: /^Open run aaaaaaaa$/ }));
    expect(onOpenRun).toHaveBeenCalledWith(NEW_RUN_ID);
  });

  it("reports an all excluded retry as its own outcome, with no run to open", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(200, {
        requested: 21,
        created: 0,
        excluded: incidentTasks().map((t) => ({
          task_id: t.id,
          reason: "agent_current",
          message: `${t.site_name ?? ""} already runs agent 0.61.116.`,
        })),
      }),
    );
    const { onOpenRun } = renderDialog();

    fireEvent.click(
      screen.getByRole("button", { name: "Retry 21 agent updates" }),
    );

    await screen.findByRole("heading", { name: "No updates were started" });
    expect(
      screen.getByText(
        /None of the 21 updates requested could be started\. This run is unchanged\./,
      ),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /^Open run/ }),
    ).not.toBeInTheDocument();
    expect(onOpenRun).not.toHaveBeenCalled();
  });

  it("stops on a complete commit that the control plane flagged, instead of navigating past the warning", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(200, {
        run_id: NEW_RUN_ID,
        requested: 21,
        created: 21,
        excluded: [],
        warning:
          "The run was created but 2 tasks could not be queued; they will stay pending until the reaper clears them.",
      }),
    );
    const { onOpenRun } = renderDialog();

    fireEvent.click(
      screen.getByRole("button", { name: "Retry 21 agent updates" }),
    );

    await screen.findByTestId("retry-partial-result");
    expect(
      screen.getByText(/2 tasks could not be queued/),
    ).toBeInTheDocument();
    expect(onOpenRun).not.toHaveBeenCalled();
    expect(
      screen.getByRole("button", { name: /^Open run aaaaaaaa$/ }),
    ).toBeInTheDocument();
  });
});

describe("RetryRunDialog - refusals", () => {
  it("renders the control plane's own refusal inline and navigates nowhere", async () => {
    fetchMock.mockResolvedValue(
      jsonResponse(409, {
        code: "agent_self_update_disabled",
        message:
          "The agent self-update channel is switched off on this control plane.",
      }),
    );
    const { onOpenRun } = renderDialog();

    fireEvent.click(
      screen.getByRole("button", { name: "Retry 21 agent updates" }),
    );

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent(
      "The agent self-update channel is switched off on this control plane.",
    );
    expect(onOpenRun).not.toHaveBeenCalled();
    expect(screen.queryByTestId("retry-partial-result")).not.toBeInTheDocument();
  });
});

describe("RetryRunDialog - the accounting survives a live selection change", () => {
  // THE DEFECT THIS PINS, which survived three design rounds by never being
  // tested: the dialog body was keyed on the LIVE selection. On a
  // still-running run the default set grows as tasks settle, so the key
  // changed WHILE THE DIALOG WAS OPEN, React remounted the body, and every
  // part of the partial-commit accounting (the shortfall, the per-task
  // reasons, the server warning) was destroyed seconds after arriving. The
  // confirm button then came back live with the same selection, so the next
  // click created a SECOND retry run.
  //
  // Both halves are asserted: the accounting must survive, and the number the
  // operator confirmed must not change under their cursor.
  it("keeps the partial result when the live selection grows underneath it", async () => {
    const tasks = incidentTasks();
    const firstTaskId = tasks[0]?.id ?? "";
    fetchMock.mockResolvedValue(
      jsonResponse(200, {
        run_id: NEW_RUN_ID,
        requested: tasks.length,
        created: tasks.length - 1,
        excluded: [
          {
            task_id: firstTaskId,
            reason: "site_not_enrolled",
            message: "site-0.example.com is no longer enrolled.",
          },
        ],
      }),
    );

    const onOpenRun = vi.fn();
    const props = {
      open: true as const,
      onClose: vi.fn(),
      runId: RUN_ID,
      dryRun: false,
      allTasks: tasks,
      haltReason: null,
      onOpenRun,
    };
    const { rerender } = renderWithProviders(
      <RetryRunDialog {...props} selectedTasks={tasks} />,
    );

    fireEvent.click(
      screen.getByRole("button", { name: `Retry ${tasks.length} agent updates` }),
    );
    await screen.findByTestId("retry-partial-result");

    // A task settles on the still-running run and joins the default set.
    const grown = [...tasks, task({ id: NEW_TASK_ID, status: "failed" })];
    rerender(<RetryRunDialog {...props} selectedTasks={grown} allTasks={grown} />);

    // Still on screen. Before the fix this was gone, replaced by a live
    // confirm step.
    expect(screen.getByTestId("retry-partial-result")).toBeInTheDocument();

    // And the confirm button has NOT returned under a grown selection, which
    // is what allowed a second run to be created on the next click.
    expect(
      screen.queryByRole("button", {
        name: `Retry ${grown.length} agent updates`,
      }),
    ).not.toBeInTheDocument();
  });
});
