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
  constructor(code: string, message: string, status: number) {
    super(message || code);
    this.name = "ConnectionsRequestError";
    this.code = code;
    this.status = status;
  }
}

async function readHouseError(res: Response): Promise<ConnectionsRequestError> {
  // A NON-JSON OR UNPARSEABLE BODY IS STILL A FAILURE. The catch returns an
  // error, never a success with empty fields.
  let code = "server_error";
  let message = "";
  try {
    const body: unknown = await res.json();
    if (body && typeof body === "object") {
      const rec = body as Record<string, unknown>;
      if (typeof rec.code === "string" && rec.code.length > 0) code = rec.code;
      if (typeof rec.message === "string") message = rec.message;
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
  return new ConnectionsRequestError(code, message, res.status);
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
