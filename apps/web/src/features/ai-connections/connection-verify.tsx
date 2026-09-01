import { Button } from "@/components/ui/button";
import { Skeleton } from "@/components/ui/skeleton";
import { PageError } from "@/components/feedback/page-error";
import { DefinitionList, KvRow } from "@/components/shared/definition-list";
import { LiveIndicator } from "@/components/shared/live-indicator";
import { cn } from "@/lib/utils";

import {
  shouldKeepPolling,
  verifyVerdict,
  type FirstCallVerdict,
  type HandshakeVerdict,
  type ProtocolNote,
  type VerifyVerdict,
} from "./connection-status-model";
import { useConnectionStatus, type ConnectionStatusWire } from "./use-connection-status";

// Connection verification -- the wireframe's S5, and Steps 8 and 9 of S29.
//
// WHICH WIZARD DRAWING THIS FOLLOWS. The deck contains two and retracts
// neither: S29 is titled "ADD MCP CONNECTION: THE TEN STEPS", and the shipped
// wizard in connect-wizard.tsx has four (STEPS, line 100). THIS COMPONENT IS
// BUILT TO THE TEN-STEP DRAWING'S STEPS 8 AND 9, because they are the only
// drawing of this screen that exists -- the short drawing does not draw a
// verification frame at all, so following it would mean building nothing.
// Nothing here renumbers the shipped wizard: this is one panel, and it is
// rendered after the setup step rather than replacing it, so a four-step rail
// and a ten-step design document can both stay true.
//
// WHAT THE DECK ASKS FOR THAT THE SERVER CANNOT ANSWER, AND SO IS NOT DRAWN:
//
//   1. S5's X frame, "Auth failed -- something arrived and was rejected", with
//      an attempt count and a source IP. NOT RENDERABLE. The status response's
//      `refusal` is hard-coded null (connection_status.go:751) because a
//      refused client writes nothing to mcp_grants at all. There is no attempt
//      count anywhere in the response, so a screen showing one would be
//      showing a number it made up.
//   2. S5's X frame, "Version unsupported -- it connected and we ended the
//      session". Same gap, same reason.
//   3. S29-9's P frame, partial multi-site coverage ("17 answered, 3 stale, 2
//      unreachable"). `first_call.partial` is always null and the server says
//      why (connection_status.go:271): internal/mcp has no typed per-site
//      partial to report.
//   4. S5's S frame lines "Sites reachable 3 of 61" and "Approvals it can
//      grant". Neither number is in this response.
//
// In place of all four, the silent case says what is known and states plainly
// what is not. A confident wrong cause is worse than an admitted gap: it sends
// the operator to check a firewall over a client that was never restarted.

const NOT_KNOWN_CLASS = "text-[var(--color-muted-foreground)]";

function formatIso(iso: string): string {
  if (iso === "") return "Unknown time";
  const d = new Date(iso);
  // Never "Invalid Date", and never silently now.
  if (Number.isNaN(d.getTime())) return "Unreadable timestamp";
  return d.toLocaleString();
}

/** Whole days, floored, for the unused-credential sentence. */
function daysFrom(ms: number): number {
  return Math.floor(ms / (24 * 60 * 60 * 1000));
}

function protocolLine(note: ProtocolNote): string {
  switch (note.kind) {
    case "recognised":
      return note.version;
    case "assumed":
      // BOTH HALVES SAID. The client sent nothing and we assumed the floor;
      // printing only the floor would be printing a header it never sent.
      return `None sent, treated as ${note.assumed}`;
    case "unrecognised":
      return `${note.version}, which this build does not currently speak`;
    case "unreadable":
      // Phrased as our failure. Nothing here claims anything about the client.
      return "We could not read what it reported";
  }
}

function HandshakeSection({ h }: { h: HandshakeVerdict }) {
  if (h.kind === "never_arrived") {
    // THE CONTRADICTION IS RESOLVED TOWARDS THE EVIDENCE. GH #636: a call can be
    // served without a recorded initialize, so this connection has no session on
    // record and has still plainly been used. Saying "nothing has reached us"
    // here would be false, and would sit directly above the first-call section
    // saying it read the fleet.
    if (h.contradictedByUse) {
      return (
        <div data-testid="handshake-contradicted">
          <p className="text-sm font-medium text-[var(--color-foreground)]">
            This connection has been used, but we recorded no session for it
          </p>
          <p className={cn("mt-2 text-sm", NOT_KNOWN_CLASS)}>
            Something has presented this credential, so it is reaching us. We did not
            record the client identifying itself, so we cannot tell you what it is or
            which protocol revision it speaks. That is a gap in our recording, not a
            fault in your client.
          </p>
        </div>
      );
    }
    if (h.phase === "fresh") {
      return (
        <div data-testid="handshake-waiting">
          <div className="flex items-center gap-2">
            <LiveIndicator state="connecting" label="Waiting for the client" />
          </div>
          <p className={cn("mt-2 text-sm", NOT_KNOWN_CLASS)}>
            Nothing is wrong yet. The client opens a session the next time you start it.
          </p>
        </div>
      );
    }
    const days = daysFrom(h.ageMs);
    return (
      <div data-testid="handshake-silent">
        <p className="text-sm font-medium text-[var(--color-foreground)]">
          Nothing has reached us from this connection
        </p>
        {/* THE AGE IS SAID AS SOON AS THERE IS AN AGE TO SAY. S5's E frame
            pairs "Created 11 days ago" with "Never used", and eleven days is
            well inside the thirty-day escalation, so the count belongs to the
            whole silent range and not only to its far end. Below a day there is
            no whole number to print, so that case says the cause instead. */}
        {days >= 1 ? (
          <p className={cn("mt-2 text-sm", NOT_KNOWN_CLASS)}>
            This connection was created {days} days ago and no client has ever opened a
            session with it. It still holds a valid key.
          </p>
        ) : (
          <p className={cn("mt-2 text-sm", NOT_KNOWN_CLASS)}>
            The connection exists and the key is valid, and no client has opened a session
            with it. The usual cause is a client that was not restarted after its config
            file changed.
          </p>
        )}
        {/* THIRTY DAYS ESCALATES THE TONE, NEVER THE CLAIM. An old unused
            credential is still only an unused credential. */}
        {h.phase === "stale" ? (
          <p className="mt-2 text-sm font-medium text-[var(--color-foreground)]">
            An unused credential is still a credential. Revoke it if nothing is going to
            use it.
          </p>
        ) : null}
        {/* THE HONEST GAP, IN THE COPY, NOT ONLY IN A COMMENT. The operator is
            told what this screen cannot distinguish, because the alternative is
            letting them read "nothing arrived" as "nothing was tried". */}
        <p className={cn("mt-2 text-sm", NOT_KNOWN_CLASS)} data-testid="refusal-gap">
          We cannot tell from here whether a client tried and was turned away. A refused
          connection is not recorded, so it looks exactly like one that was never started.
        </p>
      </div>
    );
  }

  return (
    <div data-testid="handshake-connected">
      <p className="text-sm font-medium text-[var(--color-foreground)]">
        A client opened a session
      </p>
      <DefinitionList className="mt-2">
        <KvRow label="Session opened" value={formatIso(h.recordedAtIso)} />
        <KvRow
          label="Reported itself as"
          value={
            h.reportedClientName === null ? (
              <span className={NOT_KNOWN_CLASS} data-testid="verify-client-unnamed">
                Reported no name
                {h.reportedClientVersion === null ? null : `, version ${h.reportedClientVersion}`}
              </span>
            ) : (
              <span data-testid="verify-client-name">
                {h.reportedClientName}
                {h.reportedClientVersion === null
                  ? " (no version)"
                  : ` ${h.reportedClientVersion}`}
              </span>
            )
          }
        />
        <KvRow
          label="Protocol revision"
          value={<span data-testid="verify-protocol">{protocolLine(h.protocol)}</span>}
        />
      </DefinitionList>
    </div>
  );
}

function FirstCallSection({ f }: { f: FirstCallVerdict }) {
  switch (f.kind) {
    case "none_yet":
      return (
        <div data-testid="firstcall-none">
          <LiveIndicator state="connecting" label="Waiting for its first tool call" />
          <p className={cn("mt-2 text-sm", NOT_KNOWN_CLASS)}>
            Ask it something read only to finish this, for example: which of my sites are
            behind on plugin updates?
          </p>
        </div>
      );
    case "succeeded":
      return (
        <div data-testid="firstcall-succeeded">
          <p className="text-sm font-medium text-[var(--color-foreground)]">
            It read your fleet
          </p>
          <DefinitionList className="mt-2">
            <KvRow label="First call" value={formatIso(f.calledAtIso)} />
            <KvRow
              label="Tool it called"
              value={
                f.toolName === null ? (
                  <span className={NOT_KNOWN_CLASS}>The audit row named no tool</span>
                ) : (
                  <span data-testid="verify-tool-name">{f.toolName}</span>
                )
              }
            />
            {f.auditEventId === null ? null : (
              <KvRow label="Audit row" value={f.auditEventId} mono copyable={f.auditEventId} />
            )}
          </DefinitionList>
          {/* NOT "it read all 22 of your sites". The response cannot
              characterise the call's coverage, and claiming completeness is the
              exact wrongness connection_status.go:271 refuses to put on screen. */}
          <p className={cn("mt-2 text-sm", NOT_KNOWN_CLASS)} data-testid="coverage-gap">
            We do not record how many sites that call reached, so this does not tell you it
            covered all of them.
          </p>
        </div>
      );
    case "unknown":
      return (
        <div data-testid="firstcall-unknown">
          <p className="text-sm font-medium text-[var(--color-foreground)]">
            We cannot tell whether it has made a call yet
          </p>
          {/* NOT ROUNDED DOWN TO "no calls yet". A busy organisation pushes this
              connection's call past the scan's bound, and telling that operator
              to start troubleshooting a working connection is worse than
              admitting the limit. */}
          <p className={cn("mt-2 text-sm", NOT_KNOWN_CLASS)}>
            There is too much recent activity in this organisation for us to find this
            connection&rsquo;s first call. This is a limit of the check, not a fault in the
            connection.
          </p>
        </div>
      );
  }
}

function VerdictBody({ v }: { v: VerifyVerdict }) {
  if (v.kind === "revoked") {
    return (
      <div data-testid="verify-revoked">
        <p className="text-sm font-medium text-[var(--color-foreground)]">
          This connection is revoked
        </p>
        <p className={cn("mt-2 text-sm", NOT_KNOWN_CLASS)}>
          {v.everConnected
            ? "A client did connect with it before it was revoked. Nothing can use it now."
            : "Nothing ever connected with it, and nothing can now."}
        </p>
      </div>
    );
  }
  return (
    <div className="space-y-4">
      {/* THE DECK'S TWO STEP TITLES, WITHOUT THEIR NUMBERS, AND THE OMISSION IS
          DELIBERATE. The owner's ruling is that the ten-step flow is final, so
          these two panels ARE steps 8 and 9 and the deck's wording is used
          verbatim. The NUMBERS are left off because the shipped rail
          (connect-wizard.tsx:100) still shows four steps whose fourth,
          "Set it up", is the deck's step 6. Printing "Step 8" beside a rail
          that ends at 4 puts two contradictory numbers on one screen, and the
          operator has no way to tell which is wrong. Numbering the deck down to
          match the code is the wrong direction -- the deck is the contract --
          so these carry no number until the rail carries all ten. */}
      <section aria-labelledby="verify-handshake-heading">
        <h3
          id="verify-handshake-heading"
          className="text-xs font-semibold uppercase tracking-wide text-[var(--color-muted-foreground)]"
        >
          Is the connection live
        </h3>
        <div className="mt-2">
          <HandshakeSection h={v.handshake} />
        </div>
      </section>
      <section aria-labelledby="verify-firstcall-heading">
        <h3
          id="verify-firstcall-heading"
          className="text-xs font-semibold uppercase tracking-wide text-[var(--color-muted-foreground)]"
        >
          Verify with a first read
        </h3>
        <div className="mt-2">
          <FirstCallSection f={v.firstCall} />
        </div>
      </section>
    </div>
  );
}

/**
 * The pure render, given a status snapshot. Exported so a test can pin the
 * exact sentence for a wire fixture without standing up a fetch.
 */
export function ConnectionVerifyBody({
  wire,
  now,
}: {
  wire: ConnectionStatusWire;
  now?: Date;
}) {
  // The SERVER's instant by default, not the browser's: observed_at exists so
  // "4 seconds ago" is not computed against a clock that may be minutes out.
  const observed = now ?? new Date(wire.observed_at);
  const instant = Number.isNaN(observed.getTime()) ? new Date() : observed;
  return (
    <div data-testid="verify-root">
      <VerdictBody v={verifyVerdict(wire, instant)} />
    </div>
  );
}

export interface ConnectionVerifyProps {
  connectionId: string;
  className?: string;
}

/**
 * Poll one connection and render whether its client actually connected.
 *
 * FOUR STATES, ALL RENDERED: skeleton while the first poll is in flight,
 * PageError when it failed, and then the verdict, which itself covers the
 * never-connected, connected and cannot-tell cases. There is no branch that
 * turns a failed poll into "nothing has connected yet" -- that would report our
 * own broken request as a fact about the operator's client.
 */
export function ConnectionVerify({ connectionId, className }: ConnectionVerifyProps) {
  // Two-pass polling: read once to learn the verdict, and use the verdict to
  // decide whether to keep asking. `enabled` cannot depend on data that does
  // not exist yet, so the first pass always runs.
  const first = useConnectionStatus(connectionId);
  const wire = first.data;
  const keepPolling =
    wire === undefined
      ? true
      : shouldKeepPolling(verifyVerdict(wire, new Date(wire.observed_at)));
  // Re-registering with the same key does not open a second request; it swaps
  // the interval on the one cache entry.
  const q = useConnectionStatus(connectionId, { enabled: keepPolling });

  if (q.error !== null) {
    return (
      <div className={className} data-testid="verify-error">
        {/* THE FAILED CHECK IS OUR FAILURE, SAID AS OURS. It must never render
            as "nothing has connected yet": that sentence is a claim about the
            operator's client, made out of a fact about our own request. */}
        <PageError
          what="We could not check this connection"
          why={q.error.message}
          onRetry={() => void q.refetch()}
          isRetrying={q.isFetching}
        />
      </div>
    );
  }
  if (wire === undefined) {
    return (
      <div className={cn("space-y-2", className)} data-testid="verify-loading">
        <Skeleton className="h-4 w-48" />
        <Skeleton className="h-4 w-72" />
      </div>
    );
  }

  return (
    <div className={cn("space-y-4", className)} data-testid="verify-panel">
      <ConnectionVerifyBody wire={wire} />
      <Button
        variant="outline"
        size="sm"
        onClick={() => void q.refetch()}
        disabled={q.isFetching}
      >
        {q.isFetching ? "Checking..." : "Check again"}
      </Button>
    </div>
  );
}
