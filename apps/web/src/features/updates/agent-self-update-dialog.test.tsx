import { useState } from "react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { cleanup, fireEvent, screen, waitFor } from "@testing-library/react";
import {
  createMemoryHistory,
  createRootRoute,
  createRoute,
  createRouter,
  RouterProvider,
} from "@tanstack/react-router";
import { client, type Site, type UpdateRun } from "@wpmgr/api";

import type { AgentStatus } from "@/components/status";
import { SitesToolbar } from "@/features/sites/sites-toolbar";
import { renderWithProviders } from "@/test/render";

import { AgentSelfUpdateDialog } from "./agent-self-update-dialog";

const sites: Site[] = ["one", "two"].map((id) => ({
  id,
  tenant_id: "tenant",
  name: id,
  url: `https://${id}.example.test`,
  status: "active",
  health_status: "healthy",
  wp_version: "6.8",
  php_version: "8.3",
  multisite: false,
  tags: [],
  page_cache_enabled: false,
  object_cache_enabled: false,
  created_at: "2026-10-24T00:00:00Z",
  updated_at: "2026-10-24T00:00:00Z",
}));

const run: UpdateRun = {
  id: "run",
  tenant_id: "tenant",
  status: "pending",
  dry_run: false,
  created_at: "2026-10-24T00:00:00Z",
  updated_at: "2026-10-24T00:00:00Z",
};

const noop = () => {};
let submitted: unknown[];
let restoreClient: () => void;

beforeEach(() => {
  submitted = [];
  const previous = client.getConfig();
  client.setConfig({
    ...previous,
    baseUrl: "http://localhost",
    fetch: async (input, init) => {
      const request = input instanceof Request ? input : new Request(input, init);
      submitted.push(await request.json());
      return new Response(JSON.stringify(run), {
        status: 201,
        headers: { "Content-Type": "application/json" },
      });
    },
  });
  restoreClient = () => client.setConfig(previous);
});

afterEach(() => {
  cleanup();
  restoreClient();
  document.body.style.removeProperty("pointer-events");
  document.body.style.removeProperty("overflow");
  document.body.removeAttribute("data-scroll-locked");
});

function Harness({
  status,
  onOutsideClick,
}: {
  status: AgentStatus;
  onOutsideClick: () => void;
}) {
  const [open, setOpen] = useState(false);
  return (
    <main>
      <button type="button" onClick={onOutsideClick}>
        Outside action
      </button>
      <SitesToolbar
        selection={{
          selected: new Set(sites.map((site) => site.id)),
          count: sites.length,
          toggle: noop,
          setMany: noop,
          clear: noop,
          replace: noop,
        }}
        densityState={["comfortable", noop]}
        search=""
        onSearchChange={noop}
        canOperate
        onBulkUpdate={noop}
        onBulkBackup={noop}
        onBulkRestore={noop}
        onBulkOpenWpAdmin={noop}
        onBulkTag={noop}
        onBulkSetClient={noop}
        onBulkPauseMonitoring={noop}
        onBulkResumeMonitoring={noop}
        monitoringMenu={{ pause: null, resume: null }}
        onBulkDelete={noop}
        onUpdateAgent={() => setOpen(true)}
      />
      <AgentSelfUpdateDialog
        open={open}
        onClose={() => setOpen(false)}
        sites={sites}
        agentStatusById={new Map(sites.map((site) => [site.id, status]))}
      />
    </main>
  );
}

async function openFromBulkMenu(status: AgentStatus) {
  const onOutsideClick = vi.fn();
  const root = createRootRoute({
    component: () => <Harness status={status} onOutsideClick={onOutsideClick} />,
  });
  const index = createRoute({ getParentRoute: () => root, path: "/" });
  const detail = createRoute({
    getParentRoute: () => root,
    path: "/updates/$runId",
  });
  const router = createRouter({
    routeTree: root.addChildren([index, detail]),
    history: createMemoryHistory({ initialEntries: ["/"] }),
  });
  renderWithProviders(<RouterProvider router={router} />);
  fireEvent.keyDown(
    await screen.findByRole("button", { name: "More bulk actions" }),
    { key: "Enter" },
  );
  fireEvent.click(
    await screen.findByRole("menuitem", { name: /Update WPMgr agent on/ }),
  );
  await screen.findByRole("dialog", { name: "Update WPMgr agent" });
  return { onOutsideClick, router };
}

async function expectPageUsable(
  onOutsideClick: ReturnType<typeof vi.fn>,
  outcome: () => unknown = () => null,
  expectedOutcome: unknown = null,
) {
  await waitFor(() => {
    const outside = screen.getByRole("button", { name: "Outside action" });
    outside.focus();
    // jsdom dispatches clicks even through CSS locks, so check the lock and
    // the real outside control together.
    onOutsideClick.mockClear();
    fireEvent.click(outside);
    expect({
      bodyPointerEvents: document.body.style.pointerEvents,
      bodyOverflow: document.body.style.overflow,
      scrollLock: document.body.getAttribute("data-scroll-locked"),
      hiddenAncestor: outside.closest('[aria-hidden="true"], [inert]'),
      dialog: screen.queryByRole("dialog"),
      menu: screen.queryByRole("menu"),
      overlay: document.querySelector(".fixed.inset-0.z-50"),
      focused: document.activeElement === outside,
      clicked: onOutsideClick.mock.calls.length > 0,
      outcome: outcome(),
    }).toEqual({
      bodyPointerEvents: "",
      bodyOverflow: "",
      scrollLock: null,
      hiddenAncestor: null,
      dialog: null,
      menu: null,
      overlay: null,
      focused: true,
      clicked: true,
      outcome: expectedOutcome,
    });
  });
}

describe("agent update from the real Sites bulk menu", () => {
  it.each<AgentStatus>(["current", "outdated"])(
    "releases the page after Cancel with %s sites",
    async (status) => {
      const { onOutsideClick } = await openFromBulkMenu(status);
      fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
      await expectPageUsable(onOutsideClick);
    },
  );

  it.each<AgentStatus>(["current", "outdated"])(
    "releases the page after Escape with %s sites",
    async (status) => {
      const { onOutsideClick } = await openFromBulkMenu(status);
      fireEvent.keyDown(screen.getByRole("dialog"), { key: "Escape" });
      await expectPageUsable(onOutsideClick);
    },
  );

  it.each<AgentStatus>(["current", "outdated"])(
    "releases the page after clicking outside with %s sites",
    async (status) => {
      const { onOutsideClick } = await openFromBulkMenu(status);
      // DismissableLayer installs its outside-pointer listener on the next task.
      await new Promise((resolve) => setTimeout(resolve, 0));
      const overlay = screen.getByRole("dialog").parentElement;
      if (!overlay) throw new Error("Dialog overlay is missing");
      fireEvent.pointerDown(overlay, { pointerType: "mouse", button: 0 });
      fireEvent.click(overlay);
      await expectPageUsable(onOutsideClick);
    },
  );

  it("releases the page after confirming an outdated selection", async () => {
    const { onOutsideClick, router } = await openFromBulkMenu("outdated");
    fireEvent.click(screen.getByRole("button", { name: "Update 2 sites" }));
    await expectPageUsable(
      onOutsideClick,
      () => ({ submitted, path: router.state.location.pathname }),
      {
        submitted: [
          {
            site_ids: sites.map((site) => site.id),
            items: [{ type: "agent" }],
            dry_run: false,
          },
        ],
        path: "/updates/run",
      },
    );
  });

  it("can open and cancel again using the keyboard", async () => {
    const { onOutsideClick } = await openFromBulkMenu("current");
    fireEvent.click(screen.getByRole("button", { name: "Cancel" }));
    await expectPageUsable(onOutsideClick);

    fireEvent.keyDown(screen.getByRole("button", { name: "More bulk actions" }), {
      key: "Enter",
    });
    fireEvent.keyDown(
      await screen.findByRole("menuitem", { name: /Update WPMgr agent on/ }),
      { key: "Enter" },
    );
    fireEvent.click(await screen.findByRole("button", { name: "Cancel" }));
    await expectPageUsable(onOutsideClick);
  });
});
