import { describe, it, expect, vi } from "vitest";
import { screen } from "@testing-library/react";
import type { GovContextEffective, GovContextLayerContribution } from "@wpmgr/api";

import { renderWithProviders } from "@/test/render";
import { mockQueryResult } from "@/test/query-mocks";

import {
  EffectiveContextPreview,
  CONTEXT_UNAVAILABLE_WHAT,
} from "./effective-context-preview";
import { ContextUnavailableError, useEffectiveSiteContext } from "./use-context";

// ADR-064 S5 Stage A — Screen 1 (effective-context preview) outcome tests.
//
// Server state is mocked at the hook boundary (`vi.mock("./use-context")`),
// matching this repo's render-test convention (see
// features/fleet/AgentFleetSummaryCard.test.tsx) rather than a network mock.

vi.mock("./use-context", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./use-context")>();
  return { ...actual, useEffectiveSiteContext: vi.fn() };
});

const mockedUseEffectiveSiteContext = vi.mocked(useEffectiveSiteContext);

// The exact `Name` strings ADR-064 S4's resolver assigns per layer —
// apps/api/internal/govcontext/resolver.go:96-101 on
// origin/feat/adr064-s4-context-resolver:
//
//   {Layer: 1, Name: "WPMgr security policy", ...}
//   {Layer: 2, Name: "organisation default", ...}
//   {Layer: 3, Name: "site override", ...}
//   {Layer: 4, Name: "detected site facts", ...}
//   {Layer: 5, Name: "approved skill instructions"}
//   {Layer: 6, Name: "session context", ...}
const REAL_LAYER_NAMES = [
  "WPMgr security policy",
  "organisation default",
  "site override",
  "detected site facts",
  "approved skill instructions",
  "session context",
];

function buildLayer(
  overrides: Partial<GovContextLayerContribution> = {},
): GovContextLayerContribution {
  return {
    layer: 1,
    name: "WPMgr security policy",
    restrictions: {},
    guidance: {},
    bytes: 12,
    truncated: false,
    ...overrides,
  };
}

function buildEffective(
  overrides: Partial<GovContextEffective> = {},
): GovContextEffective {
  const layers = REAL_LAYER_NAMES.map((name, i) =>
    buildLayer({ layer: i + 1, name, bytes: 20 + i }),
  );
  return {
    site_id: "site-1",
    layers,
    restrictions: {},
    total_bytes: 145,
    budget_bytes: 65536,
    truncated: false,
    ...overrides,
  };
}

function mockData(data: GovContextEffective) {
  mockedUseEffectiveSiteContext.mockReturnValue(
    mockQueryResult<GovContextEffective>({ data }),
  );
}

describe("EffectiveContextPreview — renders every layer, in order, labelled", () => {
  it("shows all six layer names, in Decision 1 precedence order (layer 1 first, layer 6 last)", () => {
    mockData(buildEffective());
    renderWithProviders(<EffectiveContextPreview siteId="site-1" />);

    // Non-vacuous: every real layer name string must be present (each name
    // legitimately appears twice — once in the overview table, once in the
    // per-layer heading — hence getAllByText rather than getByText).
    for (const name of REAL_LAYER_NAMES) {
      expect(screen.getAllByText(new RegExp(name)).length).toBeGreaterThan(0);
    }

    // ...AND in the order the API returned them (already precedence order —
    // this component must never re-sort or drop a layer). A version that
    // rendered only a subset, or reordered them, fails this even though the
    // loop above might still pass for whatever subset it kept.
    const headings = screen
      .getAllByRole("heading", { level: 4 })
      .map((h) => h.textContent);
    expect(headings).toEqual([
      "Layer 1 — WPMgr security policy",
      "Layer 2 — organisation default",
      "Layer 3 — site override",
      "Layer 4 — detected site facts",
      "Layer 5 — approved skill instructions",
      "Layer 6 — session context",
    ]);
  });
});

describe("EffectiveContextPreview — 503 context_unavailable is distinct from an empty result", () => {
  it("renders the could-not-load state for a 503, never the data tree", () => {
    mockedUseEffectiveSiteContext.mockReturnValue(
      mockQueryResult<GovContextEffective>({
        data: undefined,
        isError: true,
        isSuccess: false,
        error: new ContextUnavailableError(
          "effective context could not be resolved: failed to load organisation context",
        ),
      }),
    );
    renderWithProviders(<EffectiveContextPreview siteId="site-1" />);

    expect(screen.getByText(CONTEXT_UNAVAILABLE_WHAT)).toBeInTheDocument();
    // The data tree's own heading must be completely absent — this is a
    // different component tree, not the same tree with different text.
    expect(screen.queryByText("Resolved context")).not.toBeInTheDocument();
  });

  it("renders the data tree (not the could-not-load state) for a genuinely empty layer list", () => {
    mockData(buildEffective({ layers: [] }));
    renderWithProviders(<EffectiveContextPreview siteId="site-1" />);

    expect(screen.getByText("Resolved context")).toBeInTheDocument();
    expect(screen.queryByText(CONTEXT_UNAVAILABLE_WHAT)).not.toBeInTheDocument();
  });
});

describe("EffectiveContextPreview — a truncated layer never reads as the complete enforced set", () => {
  // Security review on ADR-064 S4 (PR #567): Resolve() computes the enforced
  // union from UNTRUNCATED snapshots, but a layer's own display copy can have
  // restriction items dropped by the byte-budget truncation (truncate.go).
  // Only layers 2-3 can carry restrictions at all (resolver.go), so this
  // fixture puts the shorter, truncated copy on layer 3 while the enforced
  // union (top-level `restrictions`, never truncated) carries the full set —
  // the real shape the review flagged, not a hypothetical.
  function buildTruncatedFixture(): GovContextEffective {
    const base = buildEffective();
    const layers = base.layers.map((l) =>
      l.layer === 3
        ? { ...l, truncated: true, restrictions: { forbidden_tools: ["shell_exec"] } }
        : l,
    );
    return {
      ...base,
      layers,
      restrictions: { forbidden_tools: ["shell_exec", "wp_eval", "file_delete"] },
      truncated: true,
    };
  }

  it("flags layer 3's own restriction list as possibly incomplete when truncated, and shows the fuller enforced union separately", () => {
    mockData(buildTruncatedFixture());
    renderWithProviders(<EffectiveContextPreview siteId="site-1" />);

    // The enforced union (never truncated) carries all three items.
    expect(screen.getByText("shell_exec, wp_eval, file_delete")).toBeInTheDocument();
    // Layer 3's own (truncated) copy carries only one — a DIFFERENT string
    // in the DOM, not the same list rendered twice.
    expect(screen.getByText("shell_exec")).toBeInTheDocument();
    // The gap between those two is exactly what the callout must surface.
    expect(
      screen.getByText(/this layer's own list may be shorter than what's enforced/i),
    ).toBeInTheDocument();
  });

  it("does NOT show the incomplete-list callout on a layer that cannot carry restrictions, even if flagged truncated", () => {
    const base = buildEffective();
    const layers = base.layers.map((l) => (l.layer === 6 ? { ...l, truncated: true } : l));
    mockData({ ...base, layers, truncated: true });
    renderWithProviders(<EffectiveContextPreview siteId="site-1" />);

    // Non-vacuous: the callout text must never appear at all here, not just
    // "not next to layer 6" — layers 4-6 never set restrictions, so there is
    // nothing for the callout to be honest about on this fixture.
    expect(
      screen.queryByText(/this layer's own list may be shorter than what's enforced/i),
    ).not.toBeInTheDocument();
  });
});
