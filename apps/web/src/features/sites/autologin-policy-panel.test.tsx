import { describe, it, expect, vi, beforeEach } from "vitest";
import { screen, fireEvent } from "@testing-library/react";
import type { Me, SiteAutologinPolicy } from "@wpmgr/api";
import type { UseMutationResult } from "@tanstack/react-query";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult, mockMutationResult } from "@/test/query-mocks";

import { AutologinPolicyPanel } from "./autologin-policy-panel";
import { useAutologinPolicy, useUpdateAutologinPolicy } from "./use-autologin-policy";
import { useMe } from "@/features/auth/use-auth";
import { toast } from "@/components/toast";

// GH #286 (web half): AutologinPolicyPanel outcome tests. Named coverage per
// the locked design: renders values, save flow, and the two server-mirrored
// validation rules (60-char cap, WordPress username charset).

vi.mock("./use-autologin-policy", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-autologin-policy")>();
  return {
    ...actual,
    useAutologinPolicy: vi.fn(),
    useUpdateAutologinPolicy: vi.fn(),
  };
});

vi.mock("@/features/auth/use-auth", async (importOriginal) => {
  const actual = await importOriginal<typeof import("@/features/auth/use-auth")>();
  return { ...actual, useMe: vi.fn() };
});

vi.mock("@/components/toast", () => ({
  toast: { success: vi.fn(), error: vi.fn(), info: vi.fn(), warning: vi.fn() },
}));

const mockedUseAutologinPolicy = vi.mocked(useAutologinPolicy);
const mockedUseUpdateAutologinPolicy = vi.mocked(useUpdateAutologinPolicy);
const mockedUseMe = vi.mocked(useMe);
const mockedToastSuccess = vi.mocked(toast.success);

function buildMe(overrides: Partial<Me> = {}): Me {
  return {
    user: {
      id: "u1",
      email: "owner@example.com",
      name: "Owner",
      created_at: "2026-01-01T00:00:00Z",
      updated_at: "2026-01-01T00:00:00Z",
    },
    memberships: [{ user_id: "u1", tenant_id: "t1", role: "owner" }],
    active_tenant_id: "t1",
    hosted: true,
    ...overrides,
  };
}

function buildPolicy(overrides: Partial<SiteAutologinPolicy> = {}): SiteAutologinPolicy {
  return {
    enabled: true,
    default_wp_user_login: "editor-jane",
    allowed_wp_roles: ["administrator"],
    ...overrides,
  };
}

function asOwner() {
  mockedUseMe.mockReturnValue({ data: buildMe() } as ReturnType<typeof useMe>);
}

function asOperator() {
  mockedUseMe.mockReturnValue({
    data: buildMe({ memberships: [{ user_id: "u1", tenant_id: "t1", role: "operator" }] }),
  } as ReturnType<typeof useMe>);
}

beforeEach(() => {
  vi.clearAllMocks();
  asOwner();
  mockedUseUpdateAutologinPolicy.mockReturnValue(
    mockMutationResult({ mutate: vi.fn() }) as ReturnType<
      typeof useUpdateAutologinPolicy
    >,
  );
});

describe("AutologinPolicyPanel: role gating", () => {
  it("renders nothing for a role below the site:autologin floor (operator)", () => {
    asOperator();
    mockedUseAutologinPolicy.mockReturnValue(
      mockQueryResult<SiteAutologinPolicy>({ data: buildPolicy() }),
    );

    const { container } = renderWithProviders(
      <AutologinPolicyPanel siteId="site-1" />,
    );

    expect(container).toBeEmptyDOMElement();
    // The GET is gated too: the hook is called with enabled:false so no
    // request fires for a role that would 403 on it.
    expect(mockedUseAutologinPolicy).toHaveBeenCalledWith(
      "site-1",
      expect.objectContaining({ enabled: false }),
    );
  });
});

describe("AutologinPolicyPanel: renders values", () => {
  it("shows the enabled switch state, the default user, and the read-only role hint", () => {
    mockedUseAutologinPolicy.mockReturnValue(
      mockQueryResult<SiteAutologinPolicy>({
        data: buildPolicy({ enabled: true, default_wp_user_login: "editor-jane" }),
      }),
    );

    renderWithProviders(<AutologinPolicyPanel siteId="site-1" />);

    expect(screen.getByRole("switch", { name: /allow one-click login/i })).toHaveAttribute(
      "aria-checked",
      "true",
    );
    expect(screen.getByLabelText(/default login user/i)).toHaveValue("editor-jane");
    expect(screen.getByText(/roles eligible as login targets/i)).toBeInTheDocument();
    expect(screen.getByText("administrator", { selector: "span" })).toBeInTheDocument();
  });
});

describe("AutologinPolicyPanel: save flow", () => {
  it("saves the trimmed username together with the current enabled flag, and toasts success", () => {
    mockedUseAutologinPolicy.mockReturnValue(
      mockQueryResult<SiteAutologinPolicy>({
        data: buildPolicy({ enabled: true, default_wp_user_login: "" }),
      }),
    );
    const mutate = vi.fn((_body, opts?: { onSuccess?: () => void }) => {
      opts?.onSuccess?.();
    });
    mockedUseUpdateAutologinPolicy.mockReturnValue(
      mockMutationResult({
        mutate: mutate as unknown as UseMutationResult<
          SiteAutologinPolicy,
          Error,
          unknown
        >["mutate"],
      }) as ReturnType<typeof useUpdateAutologinPolicy>,
    );

    renderWithProviders(<AutologinPolicyPanel siteId="site-1" />);

    fireEvent.change(screen.getByLabelText(/default login user/i), {
      target: { value: "  new-editor  " },
    });
    fireEvent.click(screen.getByRole("button", { name: /save login settings/i }));

    expect(mutate).toHaveBeenCalledTimes(1);
    expect(mutate.mock.calls[0]?.[0]).toEqual({
      enabled: true,
      default_wp_user_login: "new-editor",
    });
    expect(mockedToastSuccess).toHaveBeenCalledTimes(1);
  });
});

describe("AutologinPolicyPanel: default-user validation mirrors the server", () => {
  it("rejects a username over 60 characters without calling the mutation", () => {
    mockedUseAutologinPolicy.mockReturnValue(
      mockQueryResult<SiteAutologinPolicy>({ data: buildPolicy() }),
    );
    const mutate = vi.fn();
    mockedUseUpdateAutologinPolicy.mockReturnValue(
      mockMutationResult({ mutate }) as ReturnType<typeof useUpdateAutologinPolicy>,
    );

    renderWithProviders(<AutologinPolicyPanel siteId="site-1" />);

    const tooLong = "a".repeat(61);
    fireEvent.change(screen.getByLabelText(/default login user/i), {
      target: { value: tooLong },
    });
    fireEvent.click(screen.getByRole("button", { name: /save login settings/i }));

    expect(screen.getByRole("alert")).toHaveTextContent(/at most 60 characters/i);
    expect(mutate).not.toHaveBeenCalled();
  });

  it("rejects a username with a disallowed character without calling the mutation", () => {
    mockedUseAutologinPolicy.mockReturnValue(
      mockQueryResult<SiteAutologinPolicy>({ data: buildPolicy() }),
    );
    const mutate = vi.fn();
    mockedUseUpdateAutologinPolicy.mockReturnValue(
      mockMutationResult({ mutate }) as ReturnType<typeof useUpdateAutologinPolicy>,
    );

    renderWithProviders(<AutologinPolicyPanel siteId="site-1" />);

    fireEvent.change(screen.getByLabelText(/default login user/i), {
      target: { value: "not a valid login!" },
    });
    fireEvent.click(screen.getByRole("button", { name: /save login settings/i }));

    expect(screen.getByRole("alert")).toHaveTextContent(
      /only letters, digits, and . _ - @ are allowed/i,
    );
    expect(mutate).not.toHaveBeenCalled();
  });

  it("accepts the full WordPress username charset (letters, digits, . _ - @) at exactly 60 characters", () => {
    mockedUseAutologinPolicy.mockReturnValue(
      mockQueryResult<SiteAutologinPolicy>({ data: buildPolicy() }),
    );
    const mutate = vi.fn();
    mockedUseUpdateAutologinPolicy.mockReturnValue(
      mockMutationResult({ mutate }) as ReturnType<typeof useUpdateAutologinPolicy>,
    );

    renderWithProviders(<AutologinPolicyPanel siteId="site-1" />);

    const exactly60 = `${"a".repeat(55)}_.-@1`;
    expect(exactly60).toHaveLength(60);
    fireEvent.change(screen.getByLabelText(/default login user/i), {
      target: { value: exactly60 },
    });
    fireEvent.click(screen.getByRole("button", { name: /save login settings/i }));

    expect(mutate).toHaveBeenCalledTimes(1);
    expect(mutate.mock.calls[0]?.[0]).toEqual({
      enabled: true,
      default_wp_user_login: exactly60,
    });
  });
});
