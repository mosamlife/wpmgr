import { useMemo } from "react";
import { createFileRoute } from "@tanstack/react-router";

import { PageHeader } from "@/components/shared/page-header";
import { ConnectWizard } from "@/features/ai-connections/connect-wizard";
import { mcpEndpointUrl } from "@/features/ai-connections/endpoint";
import {
  snapshotFromPage,
  type FleetSnapshot,
  type ScopedSite,
} from "@/features/mcp-consent/site-scope";
import { DEFAULT_SITES_LIMIT, useSites } from "@/features/sites/use-sites";
import { useTags } from "@/features/tags/use-tags";

// /ai/connect — the connection wizard (design §18).
//
// NO SIDEBAR ENTRY, DELIBERATELY. It is reached from the primary action on
// /ai, which is the house rule for an authenticated route that hangs off
// another page rather than being a destination of its own.
//
// NOT TO BE CONFUSED WITH /connect/ai, which is the OAuth consent screen an
// external client redirects into. This route is the operator-facing setup
// guide; that one is the approval. They are different halves of step 6 and
// step 7 and neither links into the middle of the other.

export const Route = createFileRoute("/_authed/ai/connect")({
  component: ConnectAiClientPage,
});

function ConnectAiClientPage() {
  const sitesQuery = useSites({ view: "active" });
  const tagsQuery = useTags();

  // NULL, NOT [], WHEN WE CANNOT SEE THE FLEET, and a full page is NOT a whole
  // fleet. Both readings are the consent route's (routes/_authed/connect.ai.tsx)
  // and the reasoning is identical, so the assembly is identical rather than
  // re-invented: an empty snapshot is a fleet with no sites, null is a fleet we
  // did not read, and snapshotFromPage carries the "at least this many" fact
  // that a paged listSites forces on anyone printing a count.
  const fleet: FleetSnapshot | null = useMemo(() => {
    if (sitesQuery.data === undefined) return null;
    const sites: ScopedSite[] = sitesQuery.data.map((s) => ({
      id: s.id,
      name: s.name,
      url: s.url,
    }));
    return snapshotFromPage(sites, DEFAULT_SITES_LIMIT);
  }, [sitesQuery.data]);

  const tagsBySiteId = useMemo(() => {
    const out: Record<string, readonly string[]> = {};
    for (const site of sitesQuery.data ?? []) out[site.id] = site.tags ?? [];
    return out;
  }, [sitesQuery.data]);

  // NULL, NOT [], WHEN THE REGISTRY DID NOT LOAD. `?? []` here would turn a
  // failed request into "this organisation has no tags yet", which is a claim
  // about the organisation made out of a fact about our own request.
  const tags = useMemo(() => {
    if (tagsQuery.data === undefined) return null;
    return tagsQuery.data.map((t) => ({ id: t.id, name: t.name }));
  }, [tagsQuery.data]);

  return (
    <div className="space-y-6">
      <PageHeader
        // ONE NAME PER THING. "AI connections" is the settings screen, "New
        // connection" is the button that arrives here, and this is the
        // wizard's own heading. It used to repeat the button's old wording,
        // which made the button and the page it opened look like two features.
        title="Add an AI connection"
        subline="Nothing is created yet. This is a wizard, not a draft row. Pick your client first: everything after that is computed from it, because the setup differs per client in ways that fail quietly."
        backTo={{ to: "/ai", label: "AI connections" }}
      />
      <ConnectWizard
        endpointUrl={mcpEndpointUrl()}
        fleet={fleet}
        tags={tags}
        tagsBySiteId={tagsBySiteId}
        sitesLoading={sitesQuery.isPending}
      />
    </div>
  );
}
