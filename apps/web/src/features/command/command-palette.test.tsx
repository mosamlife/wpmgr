import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen } from "@testing-library/react";

import { renderWithProviders } from "@/test/render";

import { CommandPalette } from "./command-palette";

// GH #322: "Update agent on all sites" in the palette's "Run on all" group.
//
// The group already held two fleet-wide actions available to a regular owner,
// and the agent rollout was the only one of the three that still required
// selecting sites and opening a dropdown to reach.
//
// The whole risk of a palette entry is offering something the viewer cannot
// actually do: a fuzzy match puts it one keystroke away, and arriving at a
// page with no such action is worse than never seeing it. So the gate is the
// thing under test, both halves of it.

// cmdk renders inside a Radix Dialog, which observes its content box. jsdom
// has no ResizeObserver, and src/test/setup.ts deliberately holds no global
// stubs ("mock each feature's data hook at the point of use"), so it is
// stubbed here rather than repo-wide.
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
vi.stubGlobal("ResizeObserver", ResizeObserverStub);

// cmdk scrolls its selected item into view on every list change. jsdom has no
// layout, so the method does not exist.
Element.prototype.scrollIntoView = function scrollIntoView() {};

// Typed so the mocked hooks do not return `any` into the component tree.
const useMeMock = vi.fn<() => { data: unknown }>();
const useFleetAgentVersionsMock = vi.fn<() => { data: unknown }>();

vi.mock("@/features/auth/use-auth", async () => {
  const actual =
    await vi.importActual<typeof import("@/features/auth/use-auth")>(
      "@/features/auth/use-auth",
    );
  return {
    ...actual,
    useMe: () => useMeMock(),
    useLogout: () => ({ mutate: vi.fn() }),
  };
});

vi.mock("@/features/fleet/use-fleet-agents", () => ({
  useFleetAgentVersions: () => useFleetAgentVersionsMock(),
}));

vi.mock("@/features/sites/use-sites", () => ({
  useSites: () => ({ data: [] }),
}));

vi.mock("@/features/backups/use-bulk-backup", () => ({
  useBulkBackup: () => vi.fn(),
}));

const AGENT_ITEM = "Update agent on all sites";
const TENANT_ID = "22222222-2222-2222-2222-222222222222";

function setup({
  role,
  selfUpdateEnabled,
}: {
  role: string;
  selfUpdateEnabled: boolean | undefined;
}) {
  // The role is resolved from the membership matching active_tenant_id
  // (use-auth.ts:445-453), and a site-scoped collaborator with no matching
  // membership is never org-scoped, so a bare { role } would not exercise
  // canManage at all.
  useMeMock.mockReturnValue({
    data: {
      id: "u1",
      active_tenant_id: TENANT_ID,
      memberships: [{ tenant_id: TENANT_ID, role }],
    },
  });
  useFleetAgentVersionsMock.mockReturnValue({
    data: { self_update_enabled: selfUpdateEnabled },
  });
  renderWithProviders(<CommandPalette open onClose={vi.fn()} />);
}

describe("CommandPalette - the fleet agent rollout entry", () => {
  beforeEach(() => {
    vi.clearAllMocks();
  });

  it("offers it to an owner when the control plane's self-update switch is on", () => {
    setup({ role: "owner", selfUpdateEnabled: true });
    expect(screen.getByText(AGENT_ITEM)).toBeInTheDocument();
  });

  it("hides it when the control plane's self-update switch is off", () => {
    setup({ role: "owner", selfUpdateEnabled: false });
    expect(screen.queryByText(AGENT_ITEM)).not.toBeInTheDocument();
  });

  it("hides it when the switch is absent, rather than assuming it is on", () => {
    // An absent value means the rollup did not say. Reading that as enabled
    // would offer the action on every install that has not answered yet.
    setup({ role: "owner", selfUpdateEnabled: undefined });
    expect(screen.queryByText(AGENT_ITEM)).not.toBeInTheDocument();
  });

  it("hides it from a viewer who cannot run it, even with the switch on", () => {
    // The channel is infrastructure. Pointing a non-owner at the Sites page
    // would land them somewhere the action is not rendered at all.
    setup({ role: "viewer", selfUpdateEnabled: true });
    expect(screen.queryByText(AGENT_ITEM)).not.toBeInTheDocument();
  });

  it("keeps the other two fleet-wide actions regardless of the agent gate", () => {
    // The gate is specific to the agent entry and must not suppress the group.
    setup({ role: "viewer", selfUpdateEnabled: false });
    expect(screen.getByText("Run backup on all sites")).toBeInTheDocument();
    expect(screen.getByText("Sync metadata on all sites")).toBeInTheDocument();
  });
});
