import { PageError } from "@/components/feedback";
import { Badge } from "@/components/ui/badge";
import { Skeleton } from "@/components/ui/skeleton";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";
import { DefinitionList } from "@/components/shared/definition-list";
import type { GovContextEffective, GovContextLayerContribution } from "@wpmgr/api";

import { restrictionRows, guidanceRows } from "./context-rows";
import { ContextUnavailableError, useEffectiveSiteContext } from "./use-context";

// ADR-064 Decision 8 — the effective-context preview (Screen 1 of the S5
// "context management screens" slice). This is the operator's ONLY window
// into what Decision 1's seven-layer resolution actually produced for a
// site, and Decision 8 is explicit that the preview must call the same
// resolution function the model-facing path calls and render its real
// output — never a second, client-assembled merge of the layers into prose.
// See use-context.ts's module doc for why there is deliberately no
// "concatenated context" string anywhere in this file.

export const CONTEXT_UNAVAILABLE_WHAT =
  "This site's context could not be loaded, so the preview can't be shown.";

export const CONTEXT_UNAVAILABLE_WHY =
  "WPMgr refuses to show a resolved context when any layer failed to load, rather than fill the gap with an empty or guessed result — the same rule that governs what a live run is handed (ADR-064 Decision 14).";

export function EffectiveContextPreview({ siteId }: { siteId: string }) {
  const { data, isPending, isError, error, refetch, isRefetching } =
    useEffectiveSiteContext(siteId);

  if (isPending) {
    return <EffectiveContextSkeleton />;
  }

  if (isError) {
    if (error instanceof ContextUnavailableError) {
      // Decision 8 / Decision 14: a refused resolution is a DISTINCT state
      // from "this site has no layers to show" — never the same component
      // tree, so an operator can't mistake "we couldn't check" for "there's
      // nothing to check."
      return (
        <PageError
          what={CONTEXT_UNAVAILABLE_WHAT}
          why={CONTEXT_UNAVAILABLE_WHY}
          onRetry={() => void refetch()}
          retryLabel="Reload context"
          isRetrying={isRefetching}
        />
      );
    }
    return (
      <PageError
        what="Could not load this site's context."
        why={error instanceof Error ? error.message : "Unknown error."}
        onRetry={() => void refetch()}
        retryLabel="Reload context"
        isRetrying={isRefetching}
      />
    );
  }

  return <EffectiveContextBody data={data} />;
}

// ── Data state ───────────────────────────────────────────────────────────

function EffectiveContextBody({ data }: { data: GovContextEffective }) {
  return (
    <div className="space-y-6">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="space-y-0.5">
          <h2 className="text-sm font-medium text-foreground">
            Resolved context
          </h2>
          <p className="text-xs text-muted-foreground tabular-nums">
            {data.total_bytes.toLocaleString()} /{" "}
            {data.budget_bytes.toLocaleString()} bytes
          </p>
        </div>
        {data.truncated ? (
          <Badge variant="muted">Truncated</Badge>
        ) : null}
      </div>

      <LayerOverviewTable layers={data.layers} />

      <div className="space-y-1">
        <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Restrictions enforced at dispatch (union of layers 1-3)
        </h3>
        <p className="text-xs text-muted-foreground">
          This set is what actually blocks a tool call — it is never shortened
          by the byte budget below, even when a layer&apos;s own copy further
          down this page is.
        </p>
        <div className="rounded-lg border border-border bg-card p-4">
          <DefinitionList rows={restrictionRows(data.restrictions)} />
        </div>
      </div>

      <div className="space-y-3">
        <h3 className="text-xs font-medium uppercase tracking-wide text-muted-foreground">
          Layer detail, in precedence order
        </h3>
        {data.layers.map((layer) => (
          <LayerCard key={layer.layer} layer={layer} />
        ))}
      </div>
    </div>
  );
}

function LayerOverviewTable({
  layers,
}: {
  layers: GovContextLayerContribution[];
}) {
  return (
    <div className="overflow-hidden rounded-lg border border-border bg-card">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead className="w-12">#</TableHead>
            <TableHead>Layer</TableHead>
            <TableHead className="text-right">Bytes</TableHead>
            <TableHead className="text-right">Truncated</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {layers.map((layer) => (
            <TableRow key={layer.layer}>
              <TableCell className="text-muted-foreground tabular-nums">
                {layer.layer}
              </TableCell>
              <TableCell className="font-medium text-foreground">
                {layer.name}
              </TableCell>
              <TableCell className="text-right tabular-nums">
                {layer.bytes.toLocaleString()}
              </TableCell>
              <TableCell className="text-right">
                {layer.truncated ? (
                  <Badge variant="muted">Truncated</Badge>
                ) : (
                  <span className="text-muted-foreground">–</span>
                )}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}

function LayerCard({ layer }: { layer: GovContextLayerContribution }) {
  const isSessionLayer = layer.layer === 6;
  // Only layers 2-3 ever carry restrictions (layer 1's is a fixed constant
  // Decision 9 never truncates; layers 4-6 don't set restrictions at all —
  // apps/api/internal/govcontext/resolver.go's Resolve on the S4 branch).
  // For those two, `truncated` can mean this layer's OWN restriction list
  // was shortened to fit the byte budget — a real, expected state (Decision
  // 9), but one that must never read as though this card were the complete
  // enforced set. The enforced union above is the authoritative number;
  // this callout is what stops a short list here from being mistaken for it.
  const restrictionsMayBeIncomplete =
    layer.truncated && (layer.layer === 2 || layer.layer === 3);

  return (
    <div className="space-y-3 rounded-lg border border-border bg-card p-4">
      <div className="flex flex-wrap items-center justify-between gap-2">
        <h4 className="text-sm font-medium text-foreground">
          Layer {layer.layer} — {layer.name}
        </h4>
        <div className="flex items-center gap-2 text-xs text-muted-foreground tabular-nums">
          <span>{layer.bytes.toLocaleString()} bytes</span>
          {layer.truncated ? <Badge variant="muted">Truncated</Badge> : null}
        </div>
      </div>

      <div className="space-y-1">
        <h5 className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
          Restrictions
        </h5>
        {restrictionsMayBeIncomplete ? (
          <p className="text-xs text-warning-subtle-fg">
            This layer&apos;s own list may be shorter than what&apos;s enforced
            — truncated to fit the byte budget. See the enforced union above
            for the complete, untruncated set.
          </p>
        ) : null}
        <DefinitionList rows={restrictionRows(layer.restrictions)} />
      </div>

      <div className="space-y-1">
        <h5 className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
          Guidance
        </h5>
        <DefinitionList rows={guidanceRows(layer.guidance)} />
      </div>

      {/* ADR-064 S4: `facts_unavailable` MUST be checked before `facts` is
          rendered as data. A failed or unwired facts load still carries a
          non-null (all-empty) `facts` object on the wire — checking `facts`
          truthiness alone would render that as "this site genuinely has
          nothing to report," which is a different, false claim from "we do
          not know." This is the same distinction as "inventory age unknown"
          vs. "inventory unavailable" on the updates card, and the same one
          that produced the earlier "Never" bug on this project — an unknown
          state must never be presented as a verified empty one. */}
      {layer.facts_unavailable ? (
        <div className="space-y-1">
          <h5 className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
            Facts
          </h5>
          <p className="text-xs text-warning-subtle-fg">
            Could not be loaded for this site. This is not the same as
            "nothing to report" — treat this layer as unknown, not empty.
          </p>
        </div>
      ) : layer.facts ? (
        <div className="space-y-1">
          <h5 className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
            Facts
          </h5>
          <DefinitionList
            rows={[
              { label: "WordPress version", value: layer.facts.wp_version, mono: true },
              { label: "PHP version", value: layer.facts.php_version, mono: true },
              {
                label: "Multisite",
                value:
                  layer.facts.multisite === undefined
                    ? undefined
                    : layer.facts.multisite
                      ? "Yes"
                      : "No",
              },
              { label: "Active theme", value: layer.facts.active_theme },
            ]}
          />
        </div>
      ) : null}

      {isSessionLayer ? (
        <div className="space-y-1">
          <h5 className="text-[11px] font-medium uppercase tracking-wide text-muted-foreground">
            Session
          </h5>
          <DefinitionList
            rows={[
              {
                label: "Session",
                // ADR-064 Decision 8: session context is always empty on this
                // endpoint — a preview request has no live run behind it.
                // That is "not applicable here", never "nothing was set" —
                // rendering the same empty-value dash as every other absent
                // field would misread as the latter.
                value:
                  layer.session && layer.session.length > 0
                    ? layer.session
                    : "Not applicable outside a live run.",
              },
            ]}
          />
        </div>
      ) : null}
    </div>
  );
}

// ── Loading state ────────────────────────────────────────────────────────
//
// Deliberately makes NO claim about content — no "Never", no placeholder
// layer names, nothing that could be mistaken for a real (or absent) result
// while the fetch is still in flight.

function EffectiveContextSkeleton() {
  return (
    <div
      role="status"
      aria-busy="true"
      aria-label="Loading this site's resolved context"
      className="space-y-6"
    >
      <div className="space-y-2">
        <Skeleton className="h-3.5 w-32" />
        <Skeleton className="h-3 w-40" />
      </div>
      <div className="overflow-hidden rounded-lg border border-border">
        {Array.from({ length: 6 }).map((_, i) => (
          <div
            key={i}
            className="flex items-center gap-4 border-b border-border px-3 py-3 last:border-0"
          >
            <Skeleton className="h-3 w-4" />
            <Skeleton className="h-3 flex-1" />
            <Skeleton className="h-3 w-12" />
          </div>
        ))}
      </div>
    </div>
  );
}
