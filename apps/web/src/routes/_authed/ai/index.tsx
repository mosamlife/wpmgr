import { createFileRoute, Link } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { toast } from "sonner";

import { PageHeader } from "@/components/shared/page-header";
import { Button } from "@/components/ui/button";
import { CopyableMono } from "@/components/shared/copyable-mono";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { ConnectionsList } from "@/features/ai-connections/connections-list";
import { mcpEndpointUrl } from "@/features/ai-connections/endpoint";
import {
  PROTOCOL_FLOOR_VERSION,
  PROTOCOL_TARGET_VERSION,
  SELF_HOSTED_PROXY_REQUIREMENT,
} from "@/features/ai-connections/client-table";
import {
  useAiConnections,
  useRevokeConnection,
} from "@/features/ai-connections/use-ai-connections";
import {
  connectionsState,
  type AiConnection,
  type ConnectionsState,
} from "@/features/ai-connections/connection-model";

// /ai — the AI area's front door, and the answer to "what is the route for this
// all ai settings? it's not in the sidebar at all".
//
// ROUTE PLACEMENT. Under _authed/ because every connection here is
// tenant-scoped; outside it this page would render for logged-out users and 403
// on every call. It IS in the sidebar, unlike /connect/ai next door, which stays
// out because it is an OAuth redirect target rather than a destination.
//
// THE LIST IS NOW A REAL READ. It was hardcoded to `unavailable` while the
// control plane had no endpoint to ask; #593 adds GET /api/v1/mcp/connections
// and POST /api/v1/mcp/connections/:id/revoke, so the honest render is the data.
// The `unavailable` state stays in the model, unused here, because it is still
// the right answer for any future surface whose API has not landed -- and
// because deleting it would leave `empty` as the only place a gap could land.

export const Route = createFileRoute("/_authed/ai/")({
  component: AiConnectionsPage,
});

function AiConnectionsPage() {
  const endpoint = mcpEndpointUrl();
  const query = useAiConnections();
  const revoke = useRevokeConnection();
  const [pending, setPending] = useState<AiConnection | null>(null);

  // The mapping that decides which sentence is shown, in one tested place.
  // `connections ?? []` is precisely what this function exists to forbid: a
  // failed load must not render as "you have no AI connections", which is a
  // claim about the operator's account made out of a fact about our request.
  const state: ConnectionsState = useMemo(
    () =>
      connectionsState({
        isPending: query.isPending,
        error: query.error,
        connections: query.data,
      }),
    [query.isPending, query.error, query.data],
  );

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
            See SELF_HOSTED_PROXY_REQUIREMENT for the proxies that do not. */}
        <p className="text-xs text-[var(--color-muted-foreground)]">
          {SELF_HOSTED_PROXY_REQUIREMENT}
        </p>
      </section>

      <section aria-label="Connections" className="space-y-3">
        <h2 className="text-sm font-semibold text-[var(--color-foreground)]">
          Connected clients
        </h2>
        <ConnectionsList
          state={state}
          onRetry={() => void query.refetch()}
          isRetrying={query.isFetching}
          revokingId={revoke.isPending ? (pending?.id ?? null) : null}
          onRevoke={(c) => setPending(c)}
          connectAction={
            <Button asChild variant="outline" size="sm">
              <Link to="/ai/connect">Connect an AI client</Link>
            </Button>
          }
        />
      </section>

      <DestructiveConfirm
        open={pending !== null}
        onClose={() => setPending(null)}
        title="Revoke this connection"
        resourceName={pending?.name ?? ""}
        confirmLabel="Revoke connection"
        cancelLabel="Keep it connected"
        isPending={revoke.isPending}
        errorMessage={
          revoke.error instanceof Error ? revoke.error.message : null
        }
        // WHAT IT ACTUALLY DOES, not a generic warning. The revoke cascades to
        // the grant's bearer tokens and the grant is re-checked on every
        // request, so the client stops working on its NEXT request rather than
        // at some token expiry. "Will no longer have access" would let an
        // operator assume a delay that does not exist.
        consequencesBody={
          <>
            <p>
              The client stops working on its <strong>next request</strong>, not at some later
              expiry. Its access tokens are killed at the same time.
            </p>
            <p>
              The connection stays in this list afterwards so you can still see when it was last
              used. Reconnecting means setting the client up again from scratch.
            </p>
          </>
        }
        onConfirm={() => {
          const target = pending;
          if (target === null) return;
          revoke.mutate(target.id, {
            onSuccess: (result) => {
              setPending(null);
              // The counts are reported because three different successes are
              // possible and they are not interchangeable to a human.
              toast.success(
                result.alreadyRevoked
                  ? `"${target.name}" was already revoked. ${result.tokensRevoked} live token(s) cleaned up.`
                  : `"${target.name}" revoked. ${result.tokensRevoked} access token(s) stopped working.`,
              );
            },
            // NO onError THAT CLOSES THE DIALOG. A failed revoke leaves the
            // dialog open with errorMessage showing, and the list untouched, so
            // a failure cannot read as a success.
          });
        }}
      />
    </div>
  );
}
