import { describe, it, expect, vi } from "vitest";
import { fireEvent, screen } from "@testing-library/react";
import type { UseMutationResult } from "@tanstack/react-query";
import type { RumBeaconRotateResult } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockMutationResult } from "@/test/query-mocks";
import { toast } from "@/components/toast";

import { RumSection } from "./RumSection";
import { useRotateBeaconKey } from "../hooks/useRotateBeaconKey";
import type { PerfConfig } from "../types";

// P1 outcome test -- GH #174 (RUM beacon-key recovery UI).
//
// Two behaviors, two invariants:
//
//   1. "Rotate beacon key" is the operator recovery action for a stranded
//      beacon key. It must be visible ONLY when a key is actually stored
//      server-side (`beacon_key_set`) and ONLY for an operator+
//      (`canOperate`, the same site.perf.config gate every other perf-config
//      write uses) -- a viewer must never see, or be able to trigger, this
//      write. Firing it must call the rotate mutation and toast a
//      plain-language success message.
//
//   2. The "beacon key not yet confirmed" banner is the stuck-state
//      visibility fix itself: shown exactly when `rum_enabled &&
//      beacon_key_set && !beacon_key_acked_present`, and ABSENT the moment
//      `beacon_key_acked_present` flips true -- a regression that keeps
//      showing the warning after the agent confirms the key would
//      false-alarm every site on every reload (never double-warn).
//
// Before this file there was no render coverage of RumSection at all --
// FontsSection-style toggle rows were exercised only implicitly via
// OptimizeTab's config plumbing, never their own DOM output.

vi.mock("../hooks/useRotateBeaconKey", async (importOriginal) => {
  const actual =
    await importOriginal<typeof import("../hooks/useRotateBeaconKey")>();
  return { ...actual, useRotateBeaconKey: vi.fn() };
});

vi.mock("@/components/toast", () => ({
  toast: { success: vi.fn(), error: vi.fn() },
}));

const mockedUseRotateBeaconKey = vi.mocked(useRotateBeaconKey);
const mockedToastSuccess = vi.mocked(toast.success);

function noop() {
  /* save() stub -- this section's config-write paths are not under test */
}

function buildConfig(overrides: Partial<PerfConfig> = {}): PerfConfig {
  return {
    cache_enabled: true,
    cache_logged_in: false,
    cache_mobile: true,
    cache_refresh: false,
    cache_refresh_interval: "1hour",
    cache_link_prefetch: false,
    cache_bypass_urls: [],
    cache_bypass_cookies: [],
    cache_include_queries: [],
    cache_include_cookies: [],
    preload_concurrency: 1,
    preload_delay_ms: 500,
    preload_batch_size: 50,
    preload_max_load: 0,
    css_js_minify: false,
    css_rucss: false,
    css_rucss_include_selectors: [],
    css_js_self_host_third_party: false,
    js_delay: false,
    js_delay_method: "defer",
    js_delay_excludes: [],
    js_delay_third_party: false,
    js_delay_third_party_excludes: [],
    fonts_display_swap: false,
    fonts_optimize_google: false,
    fonts_preload: false,
    fonts_transcode_woff2: false,
    lazy_load: false,
    lazy_load_exclusions: [],
    properly_size_images: false,
    youtube_placeholder: false,
    self_host_gravatars: false,
    cdn_enabled: false,
    cdn_url: "",
    cdn_file_types: "all",
    cdn_provider: "",
    cdn_has_credentials: false,
    db_auto_clean: false,
    db_auto_clean_interval: "weekly",
    db_post_revisions: false,
    db_post_auto_drafts: false,
    db_post_trashed: false,
    db_comments_spam: false,
    db_comments_trashed: false,
    db_transients_expired: false,
    db_optimize_tables: false,
    bloat_disable_block_css: false,
    bloat_disable_dashicons: false,
    bloat_disable_emojis: false,
    bloat_disable_jquery_migrate: false,
    bloat_disable_xml_rpc: false,
    bloat_disable_rss_feed: false,
    bloat_disable_oembeds: false,
    bloat_heartbeat_control: false,
    bloat_post_revisions_control: false,
    rum_enabled: false,
    rum_sample_rate: 1,
    min_sample_count: 30,
    beacon_key_set: false,
    beacon_key_acked_present: false,
    woo_cacheable_session: false,
    woo_theme_fragments_supported: null,
    dropin_installed: true,
    wp_cache_constant_set: true,
    htaccess_managed: true,
    config_version: 1,
    ...overrides,
  };
}

function renderSection(config: PerfConfig, canOperate: boolean) {
  return renderWithProviders(
    <RumSection
      siteId="site-1"
      config={config}
      save={noop}
      disabled={!canOperate}
      isSaving={() => false}
      canOperate={canOperate}
    />,
  );
}

describe("RumSection -- Rotate beacon key action (operator-only recovery, GH #174)", () => {
  it("renders no rotate action at all when no key is stored yet", () => {
    mockedUseRotateBeaconKey.mockReturnValue(
      mockMutationResult<RumBeaconRotateResult, void>({}),
    );

    renderSection(buildConfig({ beacon_key_set: false }), true);

    expect(
      screen.queryByRole("button", { name: /rotate beacon key/i }),
    ).not.toBeInTheDocument();
  });

  it("hides the rotate action for a viewer even though a key is stored", () => {
    const mutate = vi.fn();
    mockedUseRotateBeaconKey.mockReturnValue(
      mockMutationResult<RumBeaconRotateResult, void>({ mutate }),
    );

    renderSection(
      buildConfig({ beacon_key_set: true, beacon_key_acked_present: true }),
      false,
    );

    expect(
      screen.queryByRole("button", { name: /rotate beacon key/i }),
    ).not.toBeInTheDocument();
    expect(mutate).not.toHaveBeenCalled();
  });

  it("shows the rotate action for an operator; firing it calls the rotate mutation and toasts a plain-language success message", () => {
    const mutate = vi.fn(
      (
        _vars: void,
        opts?: { onSuccess?: (r: RumBeaconRotateResult) => void },
      ) => {
        opts?.onSuccess?.({ ok: true, beacon_key_set: true });
      },
    );
    mockedUseRotateBeaconKey.mockReturnValue(
      mockMutationResult<RumBeaconRotateResult, void>({
        mutate: mutate as UseMutationResult<
          RumBeaconRotateResult,
          Error,
          void
        >["mutate"],
      }),
    );

    renderSection(
      buildConfig({ beacon_key_set: true, beacon_key_acked_present: true }),
      true,
    );

    fireEvent.click(
      screen.getByRole("button", { name: /rotate beacon key/i }),
    );

    // Non-vacuous: proves the click reaches the real mutation wiring, not
    // just a decorative button.
    expect(mutate).toHaveBeenCalledTimes(1);
    expect(mockedToastSuccess).toHaveBeenCalledTimes(1);
    const toastCall = mockedToastSuccess.mock.calls[0];
    expect(toastCall?.[0]).toBe("Beacon key rotated.");
    expect(toastCall?.[1]?.description).toContain("next sync");
  });
});

describe("RumSection -- beacon-key-not-confirmed warning (GH #174 stuck-state visibility)", () => {
  it("shows the warning (with an inline Rotate action for an operator) when RUM is on, a key is stored, but the agent has never confirmed it", () => {
    mockedUseRotateBeaconKey.mockReturnValue(
      mockMutationResult<RumBeaconRotateResult, void>({}),
    );

    renderSection(
      buildConfig({
        rum_enabled: true,
        beacon_key_set: true,
        beacon_key_acked_present: false,
      }),
      true,
    );

    expect(
      screen.getByText(/hasn't confirmed it received the beacon key/i),
    ).toBeInTheDocument();
    // The recovery action lives inline in the banner (reuses the same
    // component/behavior as the standalone row).
    expect(
      screen.getByRole("button", { name: /rotate beacon key/i }),
    ).toBeInTheDocument();
  });

  it("shows the warning to a viewer too (informational, self-heals) but never the rotate action", () => {
    mockedUseRotateBeaconKey.mockReturnValue(
      mockMutationResult<RumBeaconRotateResult, void>({}),
    );

    renderSection(
      buildConfig({
        rum_enabled: true,
        beacon_key_set: true,
        beacon_key_acked_present: false,
      }),
      false,
    );

    expect(
      screen.getByText(/hasn't confirmed it received the beacon key/i),
    ).toBeInTheDocument();
    expect(
      screen.queryByRole("button", { name: /rotate beacon key/i }),
    ).not.toBeInTheDocument();
  });

  it("is absent once the agent has confirmed the beacon key -- no double-warn", () => {
    mockedUseRotateBeaconKey.mockReturnValue(
      mockMutationResult<RumBeaconRotateResult, void>({}),
    );

    renderSection(
      buildConfig({
        rum_enabled: true,
        beacon_key_set: true,
        beacon_key_acked_present: true,
      }),
      true,
    );

    expect(
      screen.queryByText(/hasn't confirmed it received the beacon key/i),
    ).not.toBeInTheDocument();
    // The plain (non-stuck) rotate row still renders for an operator.
    expect(
      screen.getByRole("button", { name: /rotate beacon key/i }),
    ).toBeInTheDocument();
  });

  it("is absent when RUM is off, even though a stale key is still stored", () => {
    mockedUseRotateBeaconKey.mockReturnValue(
      mockMutationResult<RumBeaconRotateResult, void>({}),
    );

    renderSection(
      buildConfig({
        rum_enabled: false,
        beacon_key_set: true,
        beacon_key_acked_present: false,
      }),
      true,
    );

    expect(
      screen.queryByText(/hasn't confirmed it received the beacon key/i),
    ).not.toBeInTheDocument();
  });
});
