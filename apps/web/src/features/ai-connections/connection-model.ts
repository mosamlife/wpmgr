// The shape of a live AI connection, and the three-way facts the list must not
// flatten.
//
// NO FETCHING HERE, AND NO INVENTED ENDPOINT. The control plane exposes four
// MCP OAuth endpoints today -- register, token, authorize and consent
// (apps/api/internal/mcp/handler.go:71 and :89) -- and none of them lists or
// revokes a grant. This module is the model and the vocabulary; wiring it to a
// real query is one hook away once that endpoint exists.

/**
 * What a client told us about the protocol revision it speaks, on connect.
 *
 * THREE CASES, NOT TWO, AND NEVER A STRING THAT COLLAPSES THEM. Design §6 is
 * explicit that the absent header, a recognised version and an unrecognised
 * version have three different correct answers and "treating any two of them
 * the same is a defect". The list inherits that: an absent header rendered as a
 * version is a claim the client never made.
 */
export type ProtocolHeader =
  // FOUR states, matching apps/api/internal/mcp/model.go ClientProtocolState
  // exactly. This type carried three until the endpoint existed; the server
  // distinguishes "has never connected at all" from "connected and sent no
  // header", and folding the first into the second would report a client that
  // has never dialled in as one that dialled in badly.
  | { readonly kind: "never_connected" }
  | { readonly kind: "absent" }
  | { readonly kind: "recognised"; readonly version: string }
  | { readonly kind: "unrecognised"; readonly version: string }
  // A FIFTH KIND THAT IS NOT A FIFTH WIRE STATE. The server never sends this;
  // it is what WE say when the two fields it did send contradict each other --
  // a `recognised` or `unrecognised` state carrying a null version.
  //
  // It exists because the first version of this mapping folded that
  // contradiction into `absent`, and `absent` is a specific, confident claim
  // ABOUT THE CLIENT: "it connected and sent no header". A malformed response
  // is not that. It is us failing to understand the answer, which is a fact
  // about us, and it belongs in the same family as the list's `unavailable`
  // state rather than in the vocabulary of things the client did.
  | { readonly kind: "unreadable" };

/**
 * When a connection was last used.
 *
 * "Never used" and "we could not load this" are different facts, and so is
 * "used at T". The first two are not renderable as a date, so they are not
 * dates.
 */
export type LastUsed =
  | { readonly kind: "never" }
  | { readonly kind: "at"; readonly iso: string };

// Exactly the two values mcp_grants_status_check permits (model.go
// GrantStatus). "paused" was in this type before the endpoint existed and is
// removed: the server cannot return it, there is no endpoint to produce it, and
// a status the API cannot emit is a branch that renders for no reason.
export type ConnectionStatus = "active" | "revoked";

export interface AiConnection {
  readonly id: string;
  /** Operator-chosen name for the connection. */
  readonly name: string;
  /**
   * The client's self-reported name, or null when it reported none.
   * Never defaulted to the operator's chosen client: one is a claim by the
   * client, the other is a claim by the operator, and they can disagree.
   */
  readonly reportedClientName: string | null;
  readonly reportedClientVersion: string | null;
  readonly protocolHeader: ProtocolHeader;
  readonly lastUsed: LastUsed;
  readonly scopes: readonly string[];
  readonly status: ConnectionStatus;
  readonly createdAt: string;
  /** all | tags | sites - which sites the grant covers. */
  readonly siteScopeMode: string;
  /** null means not revoked. Never inferred from status, and never a zero date. */
  readonly revokedAt: string | null;
}

/**
 * Everything the connections list can be showing.
 *
 * FIVE STATES, AND THE FIFTH IS THE HONEST ONE TODAY. `unavailable` is not
 * `error` and it is not `empty`: it means the control plane has no endpoint to
 * ask, which is a fact about us and is fixed by shipping an API, whereas
 * `error` means we asked and it went wrong, and `empty` is a claim that the
 * operator has no connections. Rendering any of these three as another is the
 * defect this codebase keeps producing.
 */
export type ConnectionsState =
  | { readonly status: "loading" }
  | { readonly status: "error"; readonly message: string }
  | { readonly status: "unavailable"; readonly reason: string }
  | { readonly status: "empty" }
  | { readonly status: "ready"; readonly connections: readonly AiConnection[] };

/**
 * Build the list state from a query result.
 *
 * Written as a pure function so the mapping is testable without a render, and
 * so the one line that could turn a failure into an empty list lives in exactly
 * one place instead of at every call site.
 *
 * ORDER MATTERS AND IS THE POINT. Error is checked BEFORE data, and `undefined`
 * data is never coerced to `[]`. `connections ?? []` is precisely the shipped
 * bug this function exists to make impossible.
 */
export function connectionsState(input: {
  readonly isPending: boolean;
  readonly error: Error | null;
  readonly connections: readonly AiConnection[] | undefined;
}): ConnectionsState {
  if (input.error !== null) {
    return {
      status: "error",
      message: input.error.message.length > 0 ? input.error.message : "The request failed.",
    };
  }
  if (input.isPending) return { status: "loading" };
  // Not pending, no error, and still nothing: we did not read the list, so we
  // must not claim it is empty.
  if (input.connections === undefined) {
    return {
      status: "error",
      message: "We did not get a list of connections back, so we cannot tell you what is connected.",
    };
  }
  if (input.connections.length === 0) return { status: "empty" };
  return { status: "ready", connections: input.connections };
}

/** Human label for a protocol header, keeping absence visible as absence. */
export function protocolHeaderLabel(header: ProtocolHeader, floorVersion: string): string {
  switch (header.kind) {
    case "unreadable":
      // Phrased as OUR failure, not the client's behaviour. Nothing here
      // claims anything about what the client sent.
      return "We could not read this client's protocol report";
    case "never_connected":
      // NOT "no header". This client has never opened a session at all, so it
      // has never had the chance to send one.
      return "Has not connected yet";
    case "absent":
      // Not "unknown", not blank, and not the floor version on its own: the
      // client sent nothing, and we treat that as the floor. Both halves are
      // said.
      return `No protocol header sent (treated as ${floorVersion})`;
    case "unrecognised":
      return `${header.version} (not a revision we recognise)`;
    case "recognised":
      return header.version;
  }
}

/** Human label for last use, keeping "never" distinct from a date. */
export function lastUsedLabel(lastUsed: LastUsed, formatIso: (iso: string) => string): string {
  return lastUsed.kind === "never" ? "Never used" : formatIso(lastUsed.iso);
}
