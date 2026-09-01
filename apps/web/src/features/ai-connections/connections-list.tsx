import { Fragment, useState, type ReactNode } from "react";
import { PlugZap } from "lucide-react";

import { Badge } from "@/components/ui/badge";
import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback/page-error";
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table";

import { capabilityLabel } from "./capabilities";
import { PROTOCOL_FLOOR_VERSION } from "./client-table";
import { ConnectionVerify } from "./connection-verify";
import {
  lastUsedLabel,
  protocolHeaderLabel,
  type AiConnection,
  type ConnectionsState,
} from "./connection-model";

// The AI connections list (design §18 step 10).
//
// FIVE STATES, RENDERED AS FIVE DIFFERENT THINGS. The failure this component
// exists to not have is the one this codebase keeps shipping: a failed load
// rendering as a confident empty list. "You have no AI connections" is a claim
// about the operator's account; "we could not load this" is a claim about our
// own request; "the control plane cannot list these yet" is a claim about us
// too, but a different one with a different fix. Three sentences, never one.
//
// Every cell below follows the same rule at field level. An absent protocol
// header is rendered as an absent protocol header, never as a version string.

export interface ConnectionsListProps {
  state: ConnectionsState;
  onRetry?: () => void;
  isRetrying?: boolean;
  /** Rendered in the empty state so the operator has somewhere to go. */
  connectAction?: ReactNode;
  // ROTATE AND PAUSE ARE GONE, NOT DISABLED. #593 adds list and revoke and
  // nothing else, so a rotate button could only ever fail. A control that
  // cannot work is worse than an absent one: it advertises a capability and
  // spends the operator's trust when it turns out not to exist.
  onRevoke?: (connection: AiConnection) => void;
  /** Id currently being revoked, so its row can show the in-flight state. */
  revokingId?: string | null;
}

function formatIso(iso: string): string {
  const d = new Date(iso);
  // An unparseable timestamp is shown as unparseable, not as "Invalid Date"
  // and not as now.
  if (Number.isNaN(d.getTime())) return "Unreadable timestamp";
  return d.toLocaleString();
}

export function ConnectionsList({
  state,
  onRetry,
  isRetrying,
  connectAction,
  onRevoke,
  revokingId,
}: ConnectionsListProps) {
  // Which row has its verification panel open. AT THE TOP, BEFORE THE EARLY
  // RETURNS BELOW, because a hook after a conditional return is a hook that
  // sometimes does not run.
  //
  // ONE AT A TIME, DELIBERATELY. Each open panel polls its own connection, so
  // an "expand all" would put one request per connection per poll interval on
  // a fleet that may hold dozens.
  const [verifyingId, setVerifyingId] = useState<string | null>(null);

  if (state.status === "loading") {
    return (
      <div className="space-y-2" data-testid="connections-loading">
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
        <Skeleton className="h-9 w-full" />
        <span className="sr-only">Loading connections</span>
      </div>
    );
  }

  if (state.status === "error") {
    // NOT AN EMPTY TABLE. This branch returns before any table is constructed,
    // so there is no state in which a reader is looking at a "no connections"
    // row list built out of a request that failed.
    return (
      <PageError
        what="We could not load your AI connections."
        why={state.message}
        onRetry={onRetry}
        isRetrying={isRetrying}
      />
    );
  }

  if (state.status === "unavailable") {
    return (
      <div
        role="status"
        className="rounded-lg border border-[var(--color-border)] p-6 text-sm"
        data-testid="connections-unavailable"
      >
        <p className="font-medium text-[var(--color-foreground)]">
          We cannot list your connections yet
        </p>
        <p className="mt-1 text-[var(--color-muted-foreground)]">{state.reason}</p>
        <p className="mt-2 text-[var(--color-muted-foreground)]">
          This is a gap on our side, not a statement that you have none. Connections you approve do
          work; we just cannot show them here yet.
        </p>
      </div>
    );
  }

  if (state.status === "empty") {
    return (
      <div
        className="flex flex-col items-start gap-3 rounded-lg border border-[var(--color-border)] p-6"
        data-testid="connections-empty"
      >
        <PlugZap
          aria-hidden="true"
          strokeWidth={1.5}
          className="size-5 text-[var(--color-muted-foreground)]"
        />
        <div className="space-y-1">
          <p className="text-sm font-medium text-[var(--color-foreground)]">
            No AI clients are connected
          </p>
          <p className="text-sm text-[var(--color-muted-foreground)]">
            Connect one and it can read your fleet, limited to the sites you scope it to. It cannot
            change anything.
          </p>
        </div>
        {connectAction}
      </div>
    );
  }

  return (
    <div className="overflow-x-auto rounded-lg border border-[var(--color-border)]">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Connection</TableHead>
            <TableHead>Client reported</TableHead>
            <TableHead>Protocol</TableHead>
            <TableHead>Last used</TableHead>
            <TableHead>Scopes</TableHead>
            <TableHead>Capabilities</TableHead>
            <TableHead className="text-right">Actions</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {state.connections.map((c) => (
            <Fragment key={c.id}>
            <TableRow>
              <TableCell>
                <span className="font-medium text-[var(--color-foreground)]">{c.name}</span>
                <span className="ml-2">
                  <Badge
                    variant={c.status === "active" ? "success" : "destructive"}
                  >
                    {c.status}
                  </Badge>
                </span>
              </TableCell>

              <TableCell>
                {/* A CLIENT THAT REPORTED NOTHING SAYS SO. Never backfilled
                    from the client the operator picked in the wizard: one is
                    what the software claimed about itself, the other is what a
                    human selected, and they can disagree. */}
                {c.reportedClientName === null ? (
                  // A NAMELESS CLIENT CAN STILL HAVE REPORTED A VERSION. The
                  // type permits it, and dropping the version would throw away
                  // a fact the client actually sent -- the same defect as
                  // rendering an absent header as a version, pointing the other
                  // way. Both halves are said.
                  <span
                    data-testid="reported-client"
                    className="text-[var(--color-muted-foreground)]"
                  >
                    Reported no client name
                    {c.reportedClientVersion === null ? null : (
                      <span data-testid="reported-version">
                        , version {c.reportedClientVersion}
                      </span>
                    )}
                  </span>
                ) : (
                  <span className="text-[var(--color-foreground)]">
                    {c.reportedClientName}
                    {c.reportedClientVersion === null ? (
                      <span className="text-[var(--color-muted-foreground)]"> (no version)</span>
                    ) : (
                      ` ${c.reportedClientVersion}`
                    )}
                  </span>
                )}
              </TableCell>

              <TableCell>
                <span
                  className={
                    c.protocolHeader.kind === "recognised"
                      ? "text-[var(--color-foreground)]"
                      : "text-[var(--color-muted-foreground)]"
                  }
                >
                  {protocolHeaderLabel(c.protocolHeader, PROTOCOL_FLOOR_VERSION)}
                </span>
              </TableCell>

              <TableCell>
                <span
                  className={
                    c.lastUsed.kind === "never"
                      ? "text-[var(--color-muted-foreground)]"
                      : "text-[var(--color-foreground)]"
                  }
                >
                  {lastUsedLabel(c.lastUsed, formatIso)}
                </span>
              </TableCell>

              <TableCell>
                {c.scopes.length === 0 ? (
                  <span className="text-[var(--color-muted-foreground)]">No scopes granted</span>
                ) : (
                  <span className="font-mono text-xs">{c.scopes.join(" ")}</span>
                )}
              </TableCell>

              <TableCell>
                {/* AN EMPTY SET IS INERT, NOT UNKNOWN. Authenticate refuses a
                    connection holding no capability outright, so this reads as
                    a live, meaningful condition -- and it is given MORE visual
                    weight than a populated set, not a quieter placeholder that
                    could be misread as "we didn't load this". Every entry
                    renders even when this build's label map does not
                    recognise it (capabilityLabel falls back to the raw wire
                    string), so an unmapped capability the server actually
                    stored is still visible here rather than silently dropped
                    -- the exact defect #652 was filed over, one layer up. */}
                {c.capabilities.length === 0 ? (
                  <Badge variant="destructive" data-testid="capabilities-none">
                    No capabilities - inert
                  </Badge>
                ) : (
                  <div className="flex flex-wrap gap-1" data-testid="capabilities-list">
                    {c.capabilities.map((cap) => (
                      <Badge key={cap} variant="secondary">
                        {capabilityLabel(cap)}
                      </Badge>
                    ))}
                  </div>
                )}
              </TableCell>

              <TableCell className="text-right">
                {/* THE ONE QUESTION THIS PAGE COULD NOT ANSWER UNTIL NOW.
                    "Last used" says whether anything has ever touched the
                    credential; it does not say whether the client the operator
                    just configured actually opened a session, and it is
                    stamped by tools/list as well as tools/call, so it says
                    "used" for a client that has read nothing. The panel behind
                    this button asks the endpoint that can tell the two apart.
                    Offered on a REVOKED connection too: "did anything ever use
                    this before I killed it" is exactly the question asked
                    after a leak. */}
                <Button
                  variant="ghost"
                  size="sm"
                  aria-expanded={verifyingId === c.id}
                  onClick={() => setVerifyingId(verifyingId === c.id ? null : c.id)}
                >
                  {verifyingId === c.id ? "Hide check" : "Check connection"}
                </Button>
                {/* An ALREADY-REVOKED grant gets no revoke button. The
                    endpoint is idempotent, so pressing it would succeed and
                    teach the operator nothing. */}
                {c.status === "revoked" ? (
                  <span className="ml-2 text-xs text-[var(--color-muted-foreground)]">
                    Revoked
                  </span>
                ) : (
                  <Button
                    variant="ghost"
                    size="sm"
                    disabled={onRevoke === undefined || revokingId === c.id}
                    onClick={() => onRevoke?.(c)}
                  >
                    {revokingId === c.id ? "Revoking..." : "Revoke"}
                  </Button>
                )}
              </TableCell>
            </TableRow>
            {verifyingId === c.id ? (
              <TableRow>
                {/* colSpan MATCHES THE HEADER above: seven columns. A short
                    span leaves the panel boxed into one cell. */}
                <TableCell colSpan={7} className="bg-[var(--color-muted)]/30">
                  <ConnectionVerify connectionId={c.id} />
                </TableCell>
              </TableRow>
            ) : null}
            </Fragment>
          ))}
        </TableBody>
      </Table>
    </div>
  );
}
