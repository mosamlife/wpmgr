import { createFileRoute, Link } from "@tanstack/react-router";
import { useMemo, useState } from "react";
import { toast } from "sonner";

import { PageHeader } from "@/components/shared/page-header";
import { Button } from "@/components/ui/button";
import { CopyableMono } from "@/components/shared/copyable-mono";
import { DestructiveConfirm } from "@/components/dialogs/destructive-confirm";
import { ConnectionsList } from "@/features/ai-connections/connections-list";
import { ConnectionContract } from "@/features/ai-connections/connection-contract";
import { mcpEndpointUrl } from "@/features/ai-connections/endpoint";
import { canManage, useMe } from "@/features/auth/use-auth";
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

  // THE ROLE-REFUSED STATE. Minting a connection is gated server-side by
  // authz.PermAPIKeyManage (apps/api/internal/mcp/handler.go:172, and the same
  // permission on revoke at :175), and authz.minRoleFor maps that permission to
  // RoleAdmin (apps/api/internal/authz/role.go:241). `canManage` is owner-or-
  // admin plus an org-scope check, which is the same principal set stated on
  // this side. A principal without it gets a button that can only ever 403, so
  // it is not rendered.
  //
  // WHAT THIS STATE IS NOT, and the reason is worth writing down: the design
  // deck describes a role that can LIST connections but not create them. No
  // such role exists here. PermAPIKeyRead is also RoleAdmin
  // (role.go:241, the line above PermAPIKeyManage), so the two permissions
  // resolve to exactly the same set of principals and anyone who cannot mint
  // also cannot list. What a Member actually sees on this page is the contract
  // below -- which is static and true for everyone -- and the list's own 403
  // explanation, which ConnectionsList already renders as a refusal rather than
  // as an empty organisation. The button is hidden on the honest gate, not on a
  // read/manage split this product does not have.
  const { data: me } = useMe();
  const mayCreate = canManage(me);

  const newConnectionButton = mayCreate ? (
    <Button asChild>
      <Link to="/ai/connect">New connection</Link>
    </Button>
  ) : null;

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
        subline="One endpoint, one client, the sites you scope it to."
        actions={newConnectionButton}
      />

      {/* THE CONTRACT SITS ABOVE EVERYTHING, INCLUDING THE LIST. It is not an
          empty-state decoration: an operator with six connections already is
          the reader most likely to be adding a seventh without re-reading what
          one is allowed to do. It renders for every role, because it is a
          statement about the system rather than about the reader. */}
      <ConnectionContract />

      {mayCreate ? null : (
        <p className="text-sm text-[var(--color-muted-foreground)]">
          Creating a connection needs an organisation owner or admin. What is written above is
          what every connection in this organisation is held to, whoever created it.
        </p>
      )}

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
            mayCreate ? (
              <Button asChild variant="outline" size="sm">
                <Link to="/ai/connect">New connection</Link>
              </Button>
            ) : null
          }
        />
      </section>

      <DestructiveConfirm
        open={pending !== null}
        // RESET THE MUTATION, NOT JUST THE SUBJECT. Clearing `pending` alone
        // left revoke.error set, so opening the dialog for connection B
        // rendered connection A's failure -- telling the operator a revoke
        // failed for something nobody tried to revoke. Same family as
        // everything else here: state belonging to one subject presented as a
        // fact about another.
        onClose={() => {
          setPending(null);
          revoke.reset();
        }}
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
