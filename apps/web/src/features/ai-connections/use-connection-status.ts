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

/** Default cadence when the server named none we can use. */
export const POLL_FLOOR_MS = 2000;

/**
 * How long to wait before asking again, or `false` to stop asking.
 *
 * EXTRACTED SO IT CAN BE TESTED. Inlined in `refetchInterval` this was a
 * decision no test could reach without standing up a live query and watching a
 * clock, and "the polling never stops" is precisely the defect that shipped
 * here once already.
 */
export function nextPollInterval(
  data: ConnectionStatusWire | undefined,
  opts: {
    enabled: boolean;
    stopWhen?: (data: ConnectionStatusWire) => boolean;
  },
): number | false {
  if (!opts.enabled) return false;
  // Nothing read yet: keep the floor so the first poll happens.
  if (data === undefined) return POLL_FLOOR_MS;
  if (opts.stopWhen?.(data) === true) return false;
  const ms = data.poll_after_ms;
  // ZERO MEANS STOP, and it is a different answer from "the field was missing".
  // The server naming a cadence of zero is the server asking us to stop asking;
  // a missing or nonsensical one is us having no instruction, which gets the
  // floor rather than a tight loop. Never `ms || POLL_FLOOR_MS`: that folds the
  // two together, which is the flattening this whole feature exists to avoid,
  // one layer down.
  if (ms === 0) return false;
  if (!Number.isFinite(ms) || ms < 0) return POLL_FLOOR_MS;
  return ms;
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
  options: {
    /**
     * Stop asking, whatever the last response said. The caller uses this for
     * its own polling budget: a browser tab left open is a loop, and the
     * repository's own rule against waiting on an unbounded one applies to it
     * exactly as it does to a shell.
     */
    enabled?: boolean;
    /**
     * Terminal-state predicate, consulted against the LATEST response.
     *
     * ONE OBSERVER, AND THAT IS THE POINT. This used to be two `useQuery` calls
     * on the same key -- an unconditional one to learn the verdict and a second
     * whose `enabled` carried the decision. It did not work: disabling the
     * second observer leaves the first one registered, and a single enabled
     * observer is all TanStack needs to keep the interval running. A revoked
     * connection therefore polled forever behind a panel that had already
     * stopped changing. The decision belongs INSIDE the interval callback,
     * where the data it depends on already is.
     */
    stopWhen?: (data: ConnectionStatusWire) => boolean;
  } = {},
): UseQueryResult<ConnectionStatusWire, Error> {
  const enabled = options.enabled ?? true;
  const stopWhen = options.stopWhen;
  return useQuery({
    queryKey: connectionStatusKey(connectionId),
    // NOT `enabled`. Disabling the query would also block the first read and
    // strand the panel on a skeleton; the budget stops the POLLING, not the
    // one-shot fetch that tells the operator where they stand.
    staleTime: 0,
    refetchInterval: (query) => nextPollInterval(query.state.data, { enabled, stopWhen }),
    queryFn: async (): Promise<ConnectionStatusWire> => {
      const res = await fetch(connectionStatusPath(connectionId), {
        method: "GET",
        credentials: "include",
        // NO HTTP CACHE FOR A CREDENTIALED STATUS READ. `staleTime: 0` governs
        // TanStack's cache and says nothing about the browser's: without this,
        // a back-navigation can re-serve a stored response and show a stale
        // verdict, and the body carries the audit event id of a tool call.
        // The server half of this (a `Cache-Control: no-store` on the status
        // response) is in apps/api and is NOT changed here -- that path belongs
        // to another agent, and it is flagged on the PR instead.
        cache: "no-store",
        headers: { Accept: "application/json" },
      });
      if (!res.ok) throw await readHouseError(res);
      return parseConnectionStatus((await res.json()) as unknown);
    },
  });
}
