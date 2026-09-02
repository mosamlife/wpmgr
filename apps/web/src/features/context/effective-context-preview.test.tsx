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
// apps/api/internal/govcontext/resolver.go:110-115 on
// origin/feat/adr064-s4-context-resolver (tip d9f1bde8, post-facts_unavailable):
//
//   {Layer: 1, Name: "WPMgr security policy", ...}
//   {Layer: 2, Name: "organisation default", ...}
//   {Layer: 3, Name: "site override", ...}
//   {Layer: 4, Name: "detected site facts", Facts: &facts, FactsUnavailable: factsUnavailable}
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
    facts_unavailable: false,
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
      screen.getByText(/this layer's own list may be shorter than the full union/i),
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
      screen.queryByText(/this layer's own list may be shorter than the full union/i),
    ).not.toBeInTheDocument();
  });
});

describe("EffectiveContextPreview — the restrictions panel never claims server-side enforcement", () => {
  // Security review finding: `grep -rn "Restrictions\|ForbiddenTools"
  // apps/api/internal/mcp/` returns exactly one hit, in govcontext_test.go —
  // nothing on the tools/call dispatch path consults the restriction set. It
  // is joined into the tool-result text the model reads and nothing more, so
  // a model that disregards it still gets the call served. This screen used
  // to tell the operator the opposite ("enforced at dispatch", "what
  // actually blocks a tool call"), which is a false claim about a security
  // control. This test locks in the honest replacement: the panel names what
  // the set actually is (a deny-list stated to the model) and says plainly
  // that a disobedient model still gets through.
  it("never claims the restriction set is enforced or blocks the call, and says a disregarding model still invokes the tool", () => {
    mockData(buildEffective());
    renderWithProviders(<EffectiveContextPreview siteId="site-1" />);

    // Regression proof: reinstating the old heading/body text turns this
    // whole block red; see the PR description for the paste of that run.
    expect(screen.queryByText(/enforced at dispatch/i)).not.toBeInTheDocument();
    expect(screen.queryByText(/what actually blocks a tool call/i)).not.toBeInTheDocument();

    // The panel still says what it is — a real, useful feature — and is
    // explicit that a model which ignores it still gets the call served.
    expect(
      screen.getByText(/resolved restrictions for this site/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/a model that disregards it can still invoke the tool/i),
    ).toBeInTheDocument();

    // The byte-budget claim is real (see resolver_test.go's
    // TestResolve_RestrictionsUnionIsNeverTruncated) and must survive.
    expect(
      screen.getByText(/never shortened by the byte budget below/i),
    ).toBeInTheDocument();
  });
});

describe("EffectiveContextPreview — layer 4 'unavailable' is never rendered as 'nothing to report'", () => {
  // ADR-064 S4 (apps/api/internal/govcontext/dto.go:245-252,
  // resolver.go:89-98, tip d9f1bde8): a failed or unwired facts load still
  // carries a non-null `facts` object on the wire (Go's `Facts: &facts` is
  // never nil) — only `facts_unavailable: true` distinguishes "we could not
  // load this" from "the load succeeded and found nothing". This is the
  // same "inventory age unknown" vs. "inventory unavailable" distinction
  // this project already drew on the updates card, and the same one that
  // produced the earlier "Never" bug.
  //
  // NOTE for whoever picks this up next: as of S4 tip d9f1bde8,
  // toEffectiveContextDTO (dto.go:267-287) copies every LayerContribution
  // field EXCEPT FactsUnavailable into the wire DTO, so `facts_unavailable`
  // can never actually be `true` in a live response today even though the
  // resolver computes it correctly internally (proven by resolver_test.go's
  // own TestResolve_Layer4FactsUnavailable_DistinctFromKnownEmpty). This
  // fixture exercises the DOCUMENTED contract regardless — the frontend must
  // still be correct once that backend gap is closed, and this test does not
  // depend on the live API to prove it.
  it("shows a distinct 'could not be loaded' message for facts_unavailable, never the empty facts fields", () => {
    const base = buildEffective();
    const layers = base.layers.map((l) =>
      l.layer === 4 ? { ...l, facts: {}, facts_unavailable: true } : l,
    );
    mockData({ ...base, layers });
    renderWithProviders(<EffectiveContextPreview siteId="site-1" />);

    expect(
      screen.getByText(/could not be loaded for this site/i),
    ).toBeInTheDocument();
    // Non-vacuous: none of the facts field labels render at all in this
    // state — a version that fell back to the (empty) facts object would
    // render the label with an absent-value dash instead of this message.
    expect(screen.queryByText("WordPress version")).not.toBeInTheDocument();
    expect(screen.queryByText("PHP version")).not.toBeInTheDocument();
  });

  it("still renders real facts fields when the load succeeded (facts_unavailable false/absent)", () => {
    const base = buildEffective();
    const layers = base.layers.map((l) =>
      l.layer === 4 ? { ...l, facts: { wp_version: "6.7", active_theme: "twentytwentyfive" } } : l,
    );
    mockData({ ...base, layers });
    renderWithProviders(<EffectiveContextPreview siteId="site-1" />);

    expect(screen.getByText("6.7")).toBeInTheDocument();
    expect(screen.getByText("twentytwentyfive")).toBeInTheDocument();
    expect(screen.queryByText(/could not be loaded for this site/i)).not.toBeInTheDocument();
  });
});

describe("EffectiveContextPreview — the union panel never claims this site's own layer 3 reaches the model", () => {
  // Third false claim on this screen. The preview's union (layers 1-3)
  // genuinely includes this site's own layer-3 restrictions — it calls the
  // same Resolve() a live tool call would, just with the site's real id
  // (govcontext/service.go's GetEffectiveContext). A live tool call,
  // though, resolves organisation scope only:
  // apps/api/internal/mcp/govcontext.go's operatorContext calls
  // Resolve(ctx, tenantID, uuid.Nil, nil) — uuid.Nil is org scope, and at
  // org scope resolver.go's LatestSiteSnapshot never matches a site row, so
  // layer 3 never contributes. This screen used to say the full union
  // (including layer 3) was "stated to the model" — true of layers 1-2,
  // false of layer 3. This test locks in the honest split: the union panel
  // never claims full-union delivery, and the site-override layer card says
  // plainly that its own content is not part of what a live call resolves.
  function buildSiteOverrideFixture(): GovContextEffective {
    const base = buildEffective();
    const layers = base.layers.map((l) =>
      l.layer === 3
        ? { ...l, restrictions: { forbidden_tools: ["site_only_tool"] } }
        : l,
    );
    return {
      ...base,
      layers,
      restrictions: { forbidden_tools: ["site_only_tool"] },
    };
  }

  it("never claims the full layers-1-3 union is what the model is told, and says layer 3 does not reach a live call", () => {
    mockData(buildSiteOverrideFixture());
    renderWithProviders(<EffectiveContextPreview siteId="site-1" />);

    // The false claim this finding reports: the union heading/body must
    // never assert that the union (as opposed to org scope alone) is what
    // the model is told.
    expect(
      screen.queryByText(/restrictions stated to the model \(union of layers 1-3\)/i),
    ).not.toBeInTheDocument();
    expect(
      screen.queryByText(/this is a standing deny-list added to what the model is told/i),
    ).not.toBeInTheDocument();

    // What replaces it: the union is named for what it is (resolved, not
    // delivered), and the org-scope-only fact is stated plainly.
    expect(
      screen.getByText(/resolved restrictions for this site \(union of layers 1-3\)/i),
    ).toBeInTheDocument();
    expect(
      screen.getByText(/a live tool call resolves organisation-scope context only/i),
    ).toBeInTheDocument();

    // The site-override layer (3) is still shown with its real content...
    // (it legitimately appears twice: once in the top union list, once in
    // layer 3's own list, hence getAllByText rather than getByText).
    expect(screen.getAllByText("site_only_tool").length).toBeGreaterThan(0);
    // ...but its own card says it is not part of what a live call resolves.
    // Non-vacuous: "organisation-scope" is named twice on screen (the top
    // panel and layer 3's own callout), not once and not zero times.
    const orgScopeMentions = screen.getAllByText(/organisation-scope/i);
    expect(orgScopeMentions.length).toBeGreaterThanOrEqual(2);
  });

  it("says nothing about live-call scope on the organisation-default layer (2), only on the site-override layer (3)", () => {
    mockData(buildSiteOverrideFixture());
    renderWithProviders(<EffectiveContextPreview siteId="site-1" />);

    const layer2Card = screen.getByRole("heading", {
      level: 4,
      name: /Layer 2 — organisation default/,
    }).closest("div.space-y-3");
    const layer3Card = screen.getByRole("heading", {
      level: 4,
      name: /Layer 3 — site override/,
    }).closest("div.space-y-3");
    expect(layer2Card).not.toBeNull();
    expect(layer3Card).not.toBeNull();

    expect(
      layer2Card && Array.from(layer2Card.querySelectorAll("p")).some((p) =>
        /not part of what a live tool call resolves/i.test(p.textContent ?? ""),
      ),
    ).toBe(false);
    expect(
      layer3Card && Array.from(layer3Card.querySelectorAll("p")).some((p) =>
        /not part of what a live tool call resolves/i.test(p.textContent ?? ""),
      ),
    ).toBe(true);
  });
});
