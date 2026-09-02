import {
  useMutation,
  useQuery,
  useQueryClient,
  type UseMutationResult,
  type UseQueryResult,
} from "@tanstack/react-query";
import { z } from "zod";

import type { AiConnection, ProtocolHeader } from "./connection-model";

// Reading the AI connections list, and revoking one.
//
// WHY RAW fetch AND NOT THE GENERATED CLIENT. These two routes are not in
// packages/openapi/openapi.yaml -- #593 adds them to apps/api only, alongside
// the OAuth endpoints, which internal/mcp/dto.go hand-shapes for its own
// reasons. The same house pattern is already used by features/mcp-consent/
// use-consent.ts, features/sharing/use-shares.ts and routes/accept.tsx: a
// same-origin /api/v1 path with credentials included. Whether this surface
// should join the OpenAPI document is a contract question for the API owner and
// is flagged rather than decided here.
//
// THE ZOD PARSE IS NOT A SUBSTITUTE FOR GENERATED TYPES; IT IS THE THING
// GENERATED TYPES WOULD NOT GIVE US. A generated type asserts a shape at
// compile time and believes the server at runtime. Here the runtime check is
// what keeps a malformed payload from rendering as a confident, wrong list --
// in particular it is what stops an unknown protocol state being silently
// dropped into a plausible one.

export const CONNECTIONS_PATH = "/api/v1/mcp/connections";

export const connectionKeys = {
  all: ["ai-connections"] as const,
  list: () => [...connectionKeys.all, "list"] as const,
};

/**
 * The four protocol states, mirroring ClientProtocolState in
 * apps/api/internal/mcp/model.go.
 *
 * A CLOSED ENUM, DELIBERATELY. An unknown fifth state fails the parse and
 * surfaces as a failed load, rather than being coerced into `absent` -- which
 * would be this codebase's signature defect wearing the exact costume the
 * server went out of its way to prevent by not flattening the state into a
 * nullable string.
 */
const protocolStateSchema = z.enum([
  "never_connected",
  "absent",
  "recognised",
  "unrecognised",
]);

const protocolSchema = z.object({
  state: protocolStateSchema,
  // null for never_connected and absent: those two have no version because the
  // client named none. The server guards on state rather than emptiness.
  version: z.string().nullable(),
});

const connectionSchema = z.object({
  id: z.string(),
  name: z.string(),
  status: z.enum(["active", "revoked"]),
  site_scope_mode: z.string(),
  scopes: z.array(z.string()),
  created_at: z.string(),
  reported_client_name: z.string().nullable(),
  reported_client_version: z.string().nullable(),
  protocol: protocolSchema,
  last_used_at: z.string().nullable(),
  revoked_at: z.string().nullable(),
  // dto.go's connectionDTO.Capabilities: ALWAYS PRESENT, ALWAYS AN ARRAY.
  // capabilityNames builds with make([]string, 0, len(...)), so a grant
  // holding none serialises as `[]`, never `null` and never an absent key.
  // Requiring the key here (not `.optional()`) means a server that ever
  // omitted it fails the parse loudly instead of this list quietly treating
  // "the field is missing" the same as "the grant holds nothing".
  capabilities: z.array(z.string()),
});

// An OBJECT, not a bare array. dto.go says why: an error body and a bare `[]`
// are both valid JSON, so a caller that ignores the status code can decode a
// failure into a zero-length slice. The house error envelope cannot satisfy
// this schema, so the two are distinguishable here even if that ever happened.
const connectionListSchema = z.object({
  connections: z.array(connectionSchema),
});

/**
 * Turn the wire's `{state, version}` pair into the discriminated union.
 *
 * A VERSION IS ONLY READ FOR THE TWO STATES THAT HAVE ONE. If the server ever
 * leaked a version onto `absent`, this drops it rather than rendering it,
 * because `absent` means the client named no version and putting one there
 * would be a claim the client never made.
 */
function toProtocolHeader(p: z.infer<typeof protocolSchema>): ProtocolHeader {
  switch (p.state) {
    case "never_connected":
      return { kind: "never_connected" };
    case "absent":
      return { kind: "absent" };
    // A CONTRADICTION IS NOT AN ABSENCE. Both branches below used to return
    // `{kind: "absent"}` when the version was null, with a comment arguing
    // that absent "truthfully claims no version". That was wrong, and it was
    // the same defect this whole feature was built to avoid, one layer in:
    // `absent` is a confident claim about the CLIENT -- it connected and sent
    // no header -- so a malformed pair rendered as `absent` puts words in the
    // client's mouth on the strength of a response we could not read.
    //
    // dto.go only populates Version for these two states, so a null here means
    // the response disagrees with itself. That is our problem to report, not a
    // fact to assert about anyone.
    case "recognised":
      return p.version === null
        ? { kind: "unreadable" }
        : { kind: "recognised", version: p.version };
    case "unrecognised":
      return p.version === null
        ? { kind: "unreadable" }
        : { kind: "unrecognised", version: p.version };
  }
}

export function toAiConnection(raw: z.infer<typeof connectionSchema>): AiConnection {
  return {
    id: raw.id,
    name: raw.name,
    reportedClientName: raw.reported_client_name,
    reportedClientVersion: raw.reported_client_version,
    protocolHeader: toProtocolHeader(raw.protocol),
    // null is NEVER USED. Not a zero date, not "unknown".
    lastUsed:
      raw.last_used_at === null ? { kind: "never" } : { kind: "at", iso: raw.last_used_at },
    scopes: raw.scopes,
    status: raw.status,
    createdAt: raw.created_at,
    siteScopeMode: raw.site_scope_mode,
    revokedAt: raw.revoked_at,
    // NOT FILTERED AGAINST A KNOWN VOCABULARY HERE. policy.go's
    // capabilitiesFromColumn does not drop an unrecognised name either, and
    // for the same reason: this is what the server actually stored for this
    // grant, and a UI-side allowlist deciding otherwise is the defect #652
    // was filed over, one layer up.
    capabilities: raw.capabilities,
  };
}

/** Parse a list payload, throwing on anything that is not the promised shape. */
export function parseConnectionList(body: unknown): AiConnection[] {
  return connectionListSchema.parse(body).connections.map(toAiConnection);
}

/** An error the house envelope `{code, message}` carries. */
export class ConnectionsRequestError extends Error {
  readonly code: string;
  readonly status: number;
  /**
   * The house envelope's optional `details` object (respond.go), e.g.
   * `retry_after_seconds` on a 429 or `unknown_tag_id` / `unknown_site_id` on
   * the mint endpoint's referential refusals. Undefined, never a fabricated
   * empty object, when the response carried none.
   */
  readonly details?: Readonly<Record<string, unknown>>;
  constructor(
    code: string,
    message: string,
    status: number,
    details?: Readonly<Record<string, unknown>>,
  ) {
    super(message || code);
    this.name = "ConnectionsRequestError";
    this.code = code;
    this.status = status;
    this.details = details;
  }
}

// Exported so the status poll (use-connection-status.ts) reads failures through
// the SAME envelope parser as the list. A second copy would drift, and the 403
// sentence below is the one an operator sees on the one route where a
// site-constrained principal is genuinely refused.
export async function readHouseError(res: Response): Promise<ConnectionsRequestError> {
  // A NON-JSON OR UNPARSEABLE BODY IS STILL A FAILURE. The catch returns an
  // error, never a success with empty fields.
  let code = "server_error";
  let message = "";
  let details: Record<string, unknown> | undefined;
  try {
    const body: unknown = await res.json();
    if (body && typeof body === "object") {
      const rec = body as Record<string, unknown>;
      if (typeof rec.code === "string" && rec.code.length > 0) code = rec.code;
      if (typeof rec.message === "string") message = rec.message;
      if (rec.details && typeof rec.details === "object") {
        details = rec.details as Record<string, unknown>;
      }
    }
  } catch {
    // Defaults stand; `code` is already the pessimistic value.
  }
  if (message === "") {
    // Branching on the status the server actually returns. The route carries
    // RequirePermission(PermAPIKeyRead), which refuses any site-constrained
    // principal outright, so 403 here is a real and expected answer.
    message =
      res.status === 403
        ? "Your role cannot view AI connections. They are organisation-wide credentials, so this needs an admin."
        : "The server did not answer with a list we could read.";
  }
  return new ConnectionsRequestError(code, message, res.status, details);
}

export function useAiConnections(): UseQueryResult<AiConnection[], Error> {
  return useQuery({
    queryKey: connectionKeys.list(),
    queryFn: async (): Promise<AiConnection[]> => {
      const res = await fetch(CONNECTIONS_PATH, {
        method: "GET",
        credentials: "include",
        headers: { Accept: "application/json" },
      });
      if (!res.ok) throw await readHouseError(res);
      // A 200 whose body is not the promised shape throws. There is no path
      // that resolves to a partial list, because a partial list renders as a
      // complete one.
      return parseConnectionList((await res.json()) as unknown);
    },
  });
}

/**
 * A headless mint request: an authenticated operator asking for a connection
 * token directly, without the OAuth dance.
 *
 * `scopeTagIds` and `scopeSiteIds` ARE UUIDs, never names. dto.go's
 * mintConnectionRequestDTO spells the tag field `scope_tag_ids` -- the same
 * wire vocabulary approvalRequestDTO already uses -- because an id survives a
 * tag rename and a name does not; a grant scoped by name would silently
 * change which sites it covers the day somebody renames a tag. The caller
 * resolves names to ids itself (see resolveTagIds in mcp-consent/site-scope)
 * and refuses to call this at all when a chosen name has none.
 */
export interface MintConnectionInput {
  readonly name: string;
  readonly siteScopeMode: string;
  readonly scopeTagIds: readonly string[];
  readonly scopeSiteIds: readonly string[];
  /**
   * The capability list this request confers, wire key `capabilities`
   * (dto.go's mintConnectionRequestDTO, ~line 264). NEVER `[]` ON THE WIRE.
   * dto.go treats an omitted field as the default preset `["mcp.sites.read"]`,
   * but an explicitly empty array is a different thing entirely: it stores a
   * connection that authenticates and can reach no tool at all, because
   * Authenticate refuses by name on every request. A request naming no
   * capabilities and a request naming none-on-purpose are not the same wire
   * value, so this mutation never sends the latter -- the caller
   * (connect-wizard.tsx's mintCapabilitiesRequest) refuses to build a request
   * at all when the operator has deselected every row, rather than letting an
   * empty array reach this function.
   */
  readonly capabilities: readonly string[];
  /**
   * The operator's step 2 client choice, wire key `setup_client`
   * (dto.go:324, a `*string`).
   *
   * THIS IS THE CLIENT TABLE'S `id`, NOT ITS DISPLAY NAME. The server holds it
   * to the shape `^[a-z0-9]+(-[a-z0-9]+)*$` (mint.go:180, mirroring the m128
   * CHECK) and refuses a present-but-malformed value with a 400 naming the
   * field rather than repairing it -- no trimming, no lowercasing. Sending
   * `client.name` would put "Claude Code" on the wire and earn exactly that
   * 400, so every caller sends `client.id`.
   *
   * OMITTED IS A REAL ANSWER AND IS NOT THE SAME AS "". Leaving this undefined
   * omits the key, which the server reads as "the caller never asked" and
   * always accepts. The empty string is a caller that sent the key and put
   * nothing in it, which it refuses; the mint body below therefore omits the
   * key rather than ever writing an empty one.
   *
   * WHAT DEPENDS ON IT, so it is not dropped again as cosmetic: step 9's
   * verification names the client this connection was set up for, and the
   * server can only do that if the choice was sent. No caller sent it until
   * now, while the column, the CHECK and the validator had all shipped.
   */
  readonly setupClient?: string;
}

/**
 * The mint response. `token` is the plaintext bearer credential and it is
 * ONLY EVER HERE: the server holds a token_prefix and a SHA-256 hash, nothing
 * that can reconstruct it, and there is no read-back endpoint. Whatever calls
 * this mutation must render `token` once and let it go -- not stash it in a
 * query cache, not carry it past the component that shows it.
 */
export interface MintedConnection {
  readonly grantId: string;
  readonly token: string;
  readonly tokenPrefix: string;
  readonly expiresAt: string;
  readonly siteScopeMode: string;
  readonly capabilities: readonly string[];
}

const mintResponseSchema = z.object({
  grant_id: z.string(),
  token: z.string(),
  token_prefix: z.string(),
  expires_at: z.string(),
  site_scope_mode: z.string(),
  capabilities: z.array(z.string()),
});

function toMintedConnection(raw: z.infer<typeof mintResponseSchema>): MintedConnection {
  return {
    grantId: raw.grant_id,
    token: raw.token,
    tokenPrefix: raw.token_prefix,
    expiresAt: raw.expires_at,
    siteScopeMode: raw.site_scope_mode,
    capabilities: raw.capabilities,
  };
}

/**
 * Mint a connection token: POST the same CONNECTIONS_PATH the list GETs.
 *
 * INVALIDATES ON SUCCESS so a freshly-minted connection shows up in the list
 * without a manual refresh, same as revoke.
 *
 * NEVER CACHED BY QUERY. This is a mutation, not a query -- its `data` lives
 * only in the calling component's render and in TanStack's in-memory mutation
 * state, never in `queryClient`'s cache, and nothing here calls
 * `setQueryData`. A remount (leaving the step, navigating back) starts over
 * with no token in scope, which is the property the one-time reveal depends
 * on.
 */
export function useMintConnection(): UseMutationResult<
  MintedConnection,
  Error,
  MintConnectionInput
> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (input: MintConnectionInput): Promise<MintedConnection> => {
      const res = await fetch(CONNECTIONS_PATH, {
        method: "POST",
        credentials: "include",
        headers: { Accept: "application/json", "Content-Type": "application/json" },
        body: JSON.stringify({
          name: input.name,
          site_scope_mode: input.siteScopeMode,
          scope_tag_ids: [...input.scopeTagIds],
          scope_site_ids: [...input.scopeSiteIds],
          // NEVER `[]` HERE -- see MintConnectionInput's own doc. This spreads
          // whatever the caller resolved, and the caller's contract is to have
          // already refused to reach this call with zero entries.
          capabilities: [...input.capabilities],
          // THE KEY IS OMITTED, NEVER SENT EMPTY. validateSetupClient
          // (mint.go:195) accepts a missing key and refuses "" with a 400, so
          // a spread is the only correct shape here: writing
          // `setup_client: input.setupClient ?? ""` would turn "the caller did
          // not choose" into a malformed claim the server rejects.
          ...(input.setupClient === undefined ? {} : { setup_client: input.setupClient }),
        }),
      });
      if (!res.ok) throw await readHouseError(res);
      return toMintedConnection(mintResponseSchema.parse((await res.json()) as unknown));
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: connectionKeys.all });
    },
  });
}

export interface RevokeResult {
  readonly status: string;
  readonly grantsRevoked: number;
  readonly tokensRevoked: number;
  readonly alreadyRevoked: boolean;
}

const revokeSchema = z.object({
  status: z.string(),
  grants_revoked: z.number(),
  tokens_revoked: z.number(),
  already_revoked: z.boolean(),
});

/**
 * Revoke a connection.
 *
 * INVALIDATES ON SUCCESS so the list cannot keep showing a revoked connection
 * as active. The counts come back because three different successes are
 * possible and they are not interchangeable to a human: a first revoke that
 * killed live tokens, an idempotent retry, and the repair of a half-revoked
 * grant.
 */
export function useRevokeConnection(): UseMutationResult<RevokeResult, Error, string> {
  const qc = useQueryClient();
  return useMutation({
    mutationFn: async (connectionId: string): Promise<RevokeResult> => {
      const res = await fetch(
        `${CONNECTIONS_PATH}/${encodeURIComponent(connectionId)}/revoke`,
        {
          method: "POST",
          credentials: "include",
          headers: { Accept: "application/json" },
        },
      );
      if (!res.ok) throw await readHouseError(res);
      const parsed = revokeSchema.parse((await res.json()) as unknown);
      return {
        status: parsed.status,
        grantsRevoked: parsed.grants_revoked,
        tokensRevoked: parsed.tokens_revoked,
        alreadyRevoked: parsed.already_revoked,
      };
    },
    onSuccess: () => {
      void qc.invalidateQueries({ queryKey: connectionKeys.all });
    },
  });
}
