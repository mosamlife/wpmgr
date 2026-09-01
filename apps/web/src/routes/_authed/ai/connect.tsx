import { useMemo } from "react";
import { createFileRoute } from "@tanstack/react-router";

import { PageHeader } from "@/components/shared/page-header";
import { canManage, useMe } from "@/features/auth/use-auth";
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
  // THE GATE IS ON THE DESTINATION, NOT ONLY ON THE LINK. /ai hides the "New
  // connection" button from a principal who cannot mint, and hiding a link is
  // not a guard: this route has no beforeLoad, so anyone authenticated who
  // typed the URL, followed a bookmark, or was sent one reached the whole
  // wizard. They would pick a client, choose an auth method, scope the sites,
  // press the last button, and collect a 403 from
  // apps/api/internal/mcp/handler.go:172 -- after doing all of the work. The
  // refusal has to arrive before the work, which means here.
  //
  // THE SAME PREDICATE, NOT A SECOND ONE. canManage is what /ai uses for the
  // button, and it mirrors authz.minRoleFor's RoleAdmin for PermAPIKeyManage
  // (apps/api/internal/authz/role.go:241). A second predicate written to match
  // would drift from the first, and the drift would be silent until one of the
  // two was the wrong one.
  const { data: me } = useMe();
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

  // AFTER EVERY HOOK ABOVE, NEVER BEFORE ONE. An early return placed higher
  // would make the hooks below it conditional, which React forbids.
  if (!canManage(me)) {
    return (
      <div className="space-y-6">
        <PageHeader
          title="Add an AI connection"
          subline="Creating a connection needs an organisation owner or admin."
          backTo={{ to: "/ai", label: "AI connections" }}
        />
        <p
          role="status"
          data-testid="connect-role-refused"
          className="rounded-lg border border-[var(--color-border)] p-6 text-sm text-[var(--color-foreground)]"
        >
          Nothing has been created and nothing was attempted. Saying so here rather than at the
          last button is the point: the wizard would have taken a client, an authentication method
          and a list of sites from you before the server refused it.
        </p>
      </div>
    );
  }

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
