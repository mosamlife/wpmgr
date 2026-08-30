import { createFileRoute, Link } from "@tanstack/react-router";

import { PageHeader } from "@/components/shared/page-header";
import { Button } from "@/components/ui/button";
import { CopyableMono } from "@/components/shared/copyable-mono";
import { ConnectionsList } from "@/features/ai-connections/connections-list";
import { mcpEndpointUrl } from "@/features/ai-connections/endpoint";
import {
  PROTOCOL_FLOOR_VERSION,
  PROTOCOL_TARGET_VERSION,
  SELF_HOSTED_PROXY_REQUIREMENT,
} from "@/features/ai-connections/client-table";
import type { ConnectionsState } from "@/features/ai-connections/connection-model";

// /ai — the AI area's front door, and the answer to "what is the route for all
// this AI stuff? it's not in the sidebar at all".
//
// ROUTE PLACEMENT. Under _authed/ because every connection here is
// tenant-scoped; outside it this page would render for logged-out users and
// 403 on every call. It IS in the sidebar, unlike /connect/ai next door, which
// stays out because it is an OAuth redirect target rather than a destination.
//
// WHY THE LIST IS `unavailable` RATHER THAN EMPTY. The control plane exposes
// four MCP OAuth endpoints -- register and token (unauthenticated), authorize
// and consent (operator-authenticated) -- registered at
// apps/api/internal/mcp/handler.go:71 and :89. There is no endpoint that lists
// grants and none that revokes one. The honest render for that is a fifth
// state that names the gap. Passing `{ status: "empty" }` here, or fetching an
// endpoint that does not exist and letting the 404 fall through to an empty
// array, would both put the sentence "no AI clients are connected" on screen as
// a fact about the operator's account when it is a fact about our API surface.

export const Route = createFileRoute("/_authed/ai/")({
  component: AiConnectionsPage,
});

const LIST_UNAVAILABLE: ConnectionsState = {
  status: "unavailable",
  reason:
    "The control plane does not expose an endpoint for listing AI connections yet, so we have nothing to read. Until it does, we will not guess.",
};

function AiConnectionsPage() {
  const endpoint = mcpEndpointUrl();

  return (
    <div className="space-y-6">
      <PageHeader
        title="AI connections"
        subline="Let an AI client read your fleet through one endpoint. It can propose changes; it can never approve them."
        actions={
          <Button asChild>
            <Link to="/ai/connect">Connect an AI client</Link>
          </Button>
        }
      />

      <section aria-label="Endpoint" className="space-y-2">
        <h2 className="text-sm font-semibold text-[var(--color-foreground)]">Your endpoint</h2>
        <p className="text-sm text-[var(--color-muted-foreground)]">
          One URL, for every client. We negotiate protocol {PROTOCOL_TARGET_VERSION} and refuse
          anything below {PROTOCOL_FLOOR_VERSION}.
        </p>
        <CopyableMono value={endpoint} label="Copy the MCP endpoint" />
        {/* Derived from this origin, which does not prove anything forwards it.
            See SELF_HOSTED_PROXY_REQUIREMENT for the two proxies that do not. */}
        <p className="text-xs text-[var(--color-muted-foreground)]">
          {SELF_HOSTED_PROXY_REQUIREMENT}
        </p>
      </section>

      <section aria-label="Connections" className="space-y-3">
        <h2 className="text-sm font-semibold text-[var(--color-foreground)]">
          Connected clients
        </h2>
        <ConnectionsList
          state={LIST_UNAVAILABLE}
          connectAction={
            <Button asChild variant="outline" size="sm">
              <Link to="/ai/connect">Connect an AI client</Link>
            </Button>
          }
        />
      </section>
    </div>
  );
}
