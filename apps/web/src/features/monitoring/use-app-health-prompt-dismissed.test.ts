import { describe, it, expect, beforeEach } from "vitest";
import { renderHook, act } from "@testing-library/react";

import { useAppHealthPromptDismissed } from "./use-app-health-prompt-dismissed";

// GH #291 Phase 3 — mirrors the onboarding-state hook's own test intent
// (components/empty/use-onboarding-state.ts has no dedicated test file, so
// this establishes coverage for the shared pattern via this instance):
// per-tenant localStorage persistence, cross-tenant isolation, and the
// "no tenant yet" loading state.

describe("useAppHealthPromptDismissed", () => {
  beforeEach(() => {
    window.localStorage.clear();
  });

  it("is not dismissed by default", () => {
    const { result } = renderHook(() => useAppHealthPromptDismissed("t1"));
    expect(result.current.isDismissed).toBe(false);
  });

  it("persists dismissal for the given tenant", () => {
    const { result } = renderHook(() => useAppHealthPromptDismissed("t1"));

    act(() => {
      result.current.dismiss();
    });

    expect(result.current.isDismissed).toBe(true);
    expect(
      window.localStorage.getItem("wpmgr.app-health-alert-prompt.dismissed.t1"),
    ).toBe("true");
  });

  it("does not leak a dismissal across tenants", () => {
    const { result: t1 } = renderHook(() => useAppHealthPromptDismissed("t1"));
    act(() => {
      t1.current.dismiss();
    });

    const { result: t2 } = renderHook(() => useAppHealthPromptDismissed("t2"));
    expect(t2.current.isDismissed).toBe(false);
  });

  it("treats an absent tenant id as not dismissed and ignores dismiss()", () => {
    const { result } = renderHook(() => useAppHealthPromptDismissed(null));
    expect(result.current.isDismissed).toBe(false);

    act(() => {
      result.current.dismiss();
    });
    expect(result.current.isDismissed).toBe(false);
  });

  it("re-reads dismissal state when the tenant id changes", () => {
    window.localStorage.setItem(
      "wpmgr.app-health-alert-prompt.dismissed.t1",
      "true",
    );

    const { result, rerender } = renderHook(
      ({ tenantId }: { tenantId: string }) =>
        useAppHealthPromptDismissed(tenantId),
      { initialProps: { tenantId: "t2" } },
    );
    expect(result.current.isDismissed).toBe(false);

    rerender({ tenantId: "t1" });
    expect(result.current.isDismissed).toBe(true);
  });
});
