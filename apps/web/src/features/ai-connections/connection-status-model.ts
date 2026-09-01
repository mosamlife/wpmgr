// The verdict the verification screen renders, derived from one status poll.
//
// WHY A PURE MODULE. The whole risk on this screen is a sentence that claims
// more than the response supports, and a sentence is easiest to pin in a test
// when the thing that chose it is a function rather than a render. Every
// branch below is reachable from a fixture in connection-status-model.test.ts.
//
// THE ONE RULE. "Nothing has reached us" and "something reached us and went
// wrong" are different facts with different fixes, and this endpoint can only
// tell us the first. The frontend must not upgrade one into the other. See
// apps/api/internal/mcp/connection_status.go:149 (HandshakeRefusal) for the
// server's own statement of the gap: a client refused for speaking a protocol
// revision below the floor writes NOTHING to mcp_grants, so it is byte
// identical to a client that was never started. The wireframe (S5, X frame)
// draws that refusal as a live diagnosis. WE CANNOT DRAW IT. Where the deck
// asks for a cause we do not have, this model returns the absence and the
// screen says so in words.

import type { ConnectionStatusWire } from "./use-connection-status";

/**
 * How long a connection may sit unused before the screen stops saying "any
 * second now" and starts saying "nothing has arrived".
 *
 * FIVE MINUTES, AND THE NUMBER IS THE WIREFRAME'S. S5's timeout frame is
 * titled "No client has connected in 5 minutes". THE SERVER REFUSES TO OWN
 * THIS THRESHOLD ON PURPOSE -- connection_status.go:224 says a not-yet and a
 * has-not-happened-for-a-while "are the same server-side fact and they differ
 * only in how long the operator has been staring at it, which is a clock the
 * browser owns". So the browser owns it, here, once, named.
 */
export const NOTHING_ARRIVED_AFTER_MS = 5 * 60 * 1000;

/**
 * When an unused connection stops being a note and becomes something to act on.
 *
 * THIRTY DAYS, from S5's E frame: "At 30 days we escalate this from a note to a
 * banner." Escalating the TONE, never the CLAIM: a 40-day-old unused credential
 * is still only an unused credential, and this model does not start calling it
 * broken because it got old.
 */
export const ESCALATE_UNUSED_AFTER_MS = 30 * 24 * 60 * 60 * 1000;

/**
 * What the client said about the protocol revision it speaks.
 *
 * Deliberately NOT reusing ProtocolHeader from ./connection-model. That type
 * answers the list's question ("what did it report"); this one answers the
 * wizard's ("does that leave anything working differently"), and it carries the
 * assumed floor as its own field because the status endpoint sends
 * `assumed` separately from `version` for exactly that reason
 * (connection_status.go:180: writing the floor into Version "would print a
 * header the client never sent").
 */
export type ProtocolNote =
  | { readonly kind: "recognised"; readonly version: string }
  // The client sent no header and the specification says to assume the floor.
  // A SUCCESS STATE. connection_status.go:104 calls this out: "treated as the
  // floor, deliberately not an error".
  | { readonly kind: "assumed"; readonly assumed: string }
  // A revision is stored that this build no longer speaks. Reachable only
  // through history (a revision dropped from the supported window after it was
  // recorded), never through a live refusal.
  | { readonly kind: "unrecognised"; readonly version: string }
  // The response disagreed with itself: a state that must carry a version,
  // carrying none. OUR failure to read the answer, never a claim about the
  // client. Same reasoning as ProtocolHeader's `unreadable` kind.
  | { readonly kind: "unreadable" };

/** Has a client ever opened a session with this credential. */
export type HandshakeVerdict =
  | {
      readonly kind: "never_arrived";
      /**
       * How loudly to say it. NOT three different facts -- one fact, three
       * ages. `fresh` is the wizard's live wait, `silent` is past the
       * five-minute mark, `stale` is past thirty days.
       */
      readonly phase: "fresh" | "silent" | "stale";
      readonly ageMs: number;
      /**
       * The response contradicted itself: no session was recorded, and yet this
       * connection has demonstrably been used.
       *
       * THIS IS GH #636 ARRIVING ON A SCREEN. "tools/call is served without a
       * recorded initialize", so a live, working connection can carry
       * client_identity_recorded_at IS NULL while its tool calls are in the
       * audit log. Without this flag the panel prints "Nothing has reached us
       * from this connection" directly above "It read your fleet", which is
       * both alarming and false, and which sends an operator to debug a
       * connection that is working.
       *
       * The screen resolves it in the direction of the EVIDENCE, not the
       * absence: a recorded call is a thing that happened, a null column is a
       * thing that was not written. So the contradiction is reported as a gap
       * in our own recording, never as a fault in the operator's client.
       */
      readonly contradictedByUse: boolean;
    }
  | {
      readonly kind: "connected";
      readonly recordedAtIso: string;
      readonly protocol: ProtocolNote;
      readonly reportedClientName: string | null;
      readonly reportedClientVersion: string | null;
    };

/** Has the client actually read anything. */
export type FirstCallVerdict =
  // No tool call recorded, and the scan was complete, so this is definitive.
  | { readonly kind: "none_yet" }
  | {
      readonly kind: "succeeded";
      readonly calledAtIso: string;
      /** null when the audit row carried no tool name. Never defaulted. */
      readonly toolName: string | null;
      readonly auditEventId: string | null;
    }
  // The server's third state: the scan hit its bound without finding a row, so
  // it DOES NOT KNOW. Rendered as not knowing. Rounding this down to `none_yet`
  // would send a working connection into troubleshooting advice, which is the
  // exact harm connection_status.go:236 names.
  | { readonly kind: "unknown" };

/**
 * The whole screen's verdict.
 *
 * `revoked` short-circuits both axes: a revoked grant's handshake history is
 * still true but it is no longer the question, and the screen must stop polling
 * rather than wait forever for a call that can never arrive.
 */
export type VerifyVerdict =
  | { readonly kind: "revoked"; readonly everConnected: boolean }
  | {
      readonly kind: "live";
      readonly handshake: HandshakeVerdict;
      readonly firstCall: FirstCallVerdict;
    };

/**
 * Whether this connection is still worth polling.
 *
 * A verified connection and a revoked one are both terminal: nothing further
 * will change on this screen, and a poll that can only ever return the same
 * answer is a request the operator's browser makes for no one.
 */
export function shouldKeepPolling(v: VerifyVerdict): boolean {
  if (v.kind === "revoked") return false;
  return v.firstCall.kind !== "succeeded";
}

function protocolNote(p: ConnectionStatusWire["handshake"]["protocol"]): ProtocolNote {
  switch (p.state) {
    case "absent":
      // `assumed` is the only field that can carry the floor here. When the
      // server sent none we still must not substitute `floor` -- that would be
      // us deciding what the server assumed on its behalf.
      return p.assumed === null ? { kind: "unreadable" } : { kind: "assumed", assumed: p.assumed };
    case "recognised":
      return p.version === null ? { kind: "unreadable" } : { kind: "recognised", version: p.version };
    case "unrecognised":
      return p.version === null
        ? { kind: "unreadable" }
        : { kind: "unrecognised", version: p.version };
    case "never_connected":
      // Reachable only alongside handshake.state === "awaiting_client", where
      // nothing reads this note. Returning `unreadable` rather than inventing a
      // fifth kind keeps the union closed; if it ever DID render, "we could not
      // read it" is the only sentence that is true.
      return { kind: "unreadable" };
  }
}

function firstCallVerdict(f: ConnectionStatusWire["first_call"]): FirstCallVerdict {
  switch (f.state) {
    case "succeeded":
      // called_at is set ONLY for this state (connection_status.go:246). A
      // success without one is a response we cannot read, and reading it as a
      // success anyway would put a blank where the proof goes.
      return f.called_at === null
        ? { kind: "unknown" }
        : {
            kind: "succeeded",
            calledAtIso: f.called_at,
            toolName: f.tool_name,
            auditEventId: f.audit_event_id,
          };
    case "awaiting_call":
      return { kind: "none_yet" };
    case "indeterminate":
      return { kind: "unknown" };
  }
}

/**
 * Build the verdict from one poll.
 *
 * `now` is passed in, never read from the clock here, so the age thresholds are
 * testable and so the caller can hand us the SERVER's instant. The status
 * response carries `observed_at` for that purpose (connection_status.go:317:
 * relative times computed "against the server's clock and not against a browser
 * clock that may be minutes out").
 */
export function verifyVerdict(wire: ConnectionStatusWire, now: Date): VerifyVerdict {
  const everConnected = wire.handshake.state !== "awaiting_client";
  if (wire.status === "revoked") return { kind: "revoked", everConnected };

  const firstCall = firstCallVerdict(wire.first_call);

  if (wire.handshake.state === "awaiting_client") {
    const created = Date.parse(wire.created_at);
    // An unparseable created_at must not become age zero -- that would pin the
    // screen in its "any second now" phase forever. Unknown age gets the
    // middle phase: it says nothing has arrived, which is true, without
    // claiming the connection is fresh or that it is old.
    const ageMs = Number.isNaN(created) ? NOTHING_ARRIVED_AFTER_MS : now.getTime() - created;
    const phase =
      ageMs >= ESCALATE_UNUSED_AFTER_MS
        ? "stale"
        : ageMs >= NOTHING_ARRIVED_AFTER_MS
          ? "silent"
          : "fresh";
    // DEGRADE THE VERDICT, NEVER THE EVIDENCE.
    //
    // This reads the WIRE and not `firstCall`, and the difference is a defect
    // that shipped here for one commit. `firstCallVerdict` correctly degrades a
    // `succeeded` carrying no `called_at` to `unknown` -- a success without its
    // timestamp is a response we cannot read, and rendering it as a success
    // would put a blank where the proof goes. But a flag whose entire job is to
    // notice a contradiction must consult the raw claim, not the conclusion
    // already drawn from it. Reading `firstCall.kind === "succeeded"` meant that
    // a wire saying "a call succeeded", with the timestamp missing and
    // last_used_at null, produced contradictedByUse === false and printed
    // "Nothing has reached us from this connection" -- over a response that
    // records a call.
    //
    // That is the GH #636 shape one field across: the same absence-beats-
    // evidence inversion this branch was added to prevent, reintroduced by
    // sourcing it from a degraded value.
    //
    // Either half is evidence of use. last_used_at is too weak to prove a READ
    // (tools/list stamps it), but it is plenty to disprove "nothing has ever
    // reached us" -- something set it.
    const contradictedByUse =
      wire.first_call.state === "succeeded" || wire.first_call.last_used_at !== null;
    return {
      kind: "live",
      handshake: { kind: "never_arrived", phase, ageMs, contradictedByUse },
      firstCall,
    };
  }

  // Every remaining handshake state means a client identified itself, so
  // recorded_at is non-null (connection_status.go:194). A missing one is a
  // response we cannot read; falling back to created_at would print the moment
  // the credential was minted as the moment a client dialled in.
  return {
    kind: "live",
    handshake: {
      kind: "connected",
      recordedAtIso: wire.handshake.recorded_at ?? "",
      protocol: protocolNote(wire.handshake.protocol),
      reportedClientName: wire.handshake.reported_client_name,
      reportedClientVersion: wire.handshake.reported_client_version,
    },
    firstCall,
  };
}
