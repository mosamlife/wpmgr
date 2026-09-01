import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { z } from "zod";

import { CONNECTIONS_PATH, connectionKeys, readHouseError } from "./use-ai-connections";

// GET /api/v1/mcp/connections/:id/status -- the wizard's Step 8 and Step 9.
//
// NO GENERATED CLIENT, AND THAT IS CHECKED RATHER THAN ASSUMED. The MCP surface
// is not in packages/openapi/openapi.yaml; internal/mcp hand-shapes its own
// DTOs (connection_status.go:738, connectionStatusResponse, a gin.H built by
// hand). `grep -rn "mcp" packages/openapi-client/src/generated` returns nothing.
// So this module follows the same house pattern its two siblings already use --
// use-ai-connections.ts and features/mcp-consent/use-consent.ts -- a same-origin
// /api/v1 path with credentials included and a zod parse at the boundary.
//
// EVERY FIELD BELOW IS QUOTED FROM connectionStatusResponse. The keys, the
// nullability and the enum members are that function's, not a guess: the
// nullable fields are the ones it passes through emptyToNil or a *time.Time,
// and the two enums are HandshakeState (connection_status.go:88) and
// FirstCallState (:209) verbatim.

/**
 * The handshake states, mirroring HandshakeState in connection_status.go:88.
 *
 * A CLOSED ENUM. A fifth member fails the parse and the screen says it could
 * not read the answer, which is the honest outcome; coercing an unknown state
 * into `awaiting_client` would report a client that did something as one that
 * did nothing.
 */
const handshakeStateSchema = z.enum([
  "awaiting_client",
  "connected",
  "connected_protocol_assumed",
  "connected_protocol_unrecognised",
]);

/** FirstCallState, connection_status.go:209. Closed for the same reason. */
const firstCallStateSchema = z.enum(["awaiting_call", "succeeded", "indeterminate"]);

const statusProtocolSchema = z.object({
  // ClientProtocolState, the same four the list already parses.
  state: z.enum(["never_connected", "absent", "recognised", "unrecognised"]),
  version: z.string().nullable(),
  // SEPARATE FROM version, and that separation is the point: `assumed` is what
  // WE assumed, `version` is what the CLIENT sent. Folding them would print a
  // header the client never sent.
  assumed: z.string().nullable(),
  // Properties of the server, true on every response including one for a
  // connection nothing has ever touched. Never nullable.
  floor: z.string(),
  target: z.string(),
  supported: z.array(z.string()),
});

const handshakeSchema = z.object({
  state: handshakeStateSchema,
  recorded_at: z.string().nullable(),
  reported_client_name: z.string().nullable(),
  reported_client_version: z.string().nullable(),
  protocol: statusProtocolSchema,
  // ALWAYS null today. connection_status.go:149 states the data gap in the
  // type: a client refused for a below-floor protocol revision writes nothing
  // to mcp_grants, so the server has no refusal to report and returns the key
  // as null "so the frontend can bind it now and light up when the refusal is
  // recorded". Parsed as unknown-but-present so a future non-null body does not
  // fail the parse; nothing reads it, and NOTHING RENDERS A REFUSAL, because
  // there is no refusal to render.
  refusal: z.unknown(),
});

const firstCallSchema = z.object({
  state: firstCallStateSchema,
  called_at: z.string().nullable(),
  tool_name: z.string().nullable(),
  audit_event_id: z.string().nullable(),
  // REPORTED, NEVER USED TO DERIVE THE STATE. connection_status.go:258 spends a
  // paragraph on why: RecordActivity stamps last_used_at from tools/list as
  // well as tools/call, and every client issues tools/list right after
  // initialize without anyone asking. A screen that read "has it done
  // anything?" off this column would show "connected and working" for a client
  // that has read nothing at all.
  last_used_at: z.string().nullable(),
  // Always null; the per-site coverage breakdown is not answerable today
  // (connection_status.go:271). Bound, never rendered.
  partial: z.unknown(),
});

const statusSchema = z.object({
  id: z.string(),
  // A REVOKED GRANT STILL ANSWERS. The server returns 200 rather than 404 on
  // purpose (connection_status.go:305) so the screen can stop polling and say
  // so; a 404 would read as "wrong id".
  status: z.enum(["active", "revoked"]),
  created_at: z.string(),
  expires_at: z.string(),
  handshake: handshakeSchema,
  first_call: firstCallSchema,
  observed_at: z.string(),
  poll_after_ms: z.number(),
});

export type ConnectionStatusWire = z.infer<typeof statusSchema>;

/** Exported for the test that compares this schema against the Go DTO. */
export function parseConnectionStatus(raw: unknown): ConnectionStatusWire {
  return statusSchema.parse(raw);
}

export const connectionStatusKey = (id: string) =>
  [...connectionKeys.all, "status", id] as const;

export function connectionStatusPath(id: string): string {
  return `${CONNECTIONS_PATH}/${encodeURIComponent(id)}/status`;
}

/**
 * Poll one connection's verification status.
 *
 * THE SERVER SETS THE CADENCE. `poll_after_ms` comes back on every response and
 * is used as the next interval, rather than a constant chosen here: the
 * endpoint knows what its own scan costs and this screen does not.
 *
 * `enabled` lets a caller stop entirely. The verify panel passes false once the
 * verdict is terminal (verified, or revoked), so a finished screen left open in
 * a tab is not an endless request loop.
 */
export function useConnectionStatus(
  connectionId: string,
  options: { enabled?: boolean } = {},
): UseQueryResult<ConnectionStatusWire, Error> {
  const enabled = options.enabled ?? true;
  return useQuery({
    queryKey: connectionStatusKey(connectionId),
    enabled,
    // A status snapshot is never fresh: the whole screen exists to notice a
    // change. Anything above zero here shows a stale verdict after a remount.
    staleTime: 0,
    refetchInterval: (query) => {
      if (!enabled) return false;
      const ms = query.state.data?.poll_after_ms;
      // A response that named no cadence, or named a nonsensical one, gets the
      // floor rather than a tight loop. Never `ms || 2000`: a legitimate 0 and
      // a missing field would take the same branch, and one of them is the
      // server asking us to stop.
      if (typeof ms !== "number" || !Number.isFinite(ms) || ms <= 0) return 2000;
      return ms;
    },
    queryFn: async (): Promise<ConnectionStatusWire> => {
      const res = await fetch(connectionStatusPath(connectionId), {
        method: "GET",
        credentials: "include",
        headers: { Accept: "application/json" },
      });
      if (!res.ok) throw await readHouseError(res);
      return parseConnectionStatus((await res.json()) as unknown);
    },
  });
}
