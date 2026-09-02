import { useQuery, type UseQueryResult } from "@tanstack/react-query";
import { z } from "zod";

import { CONNECTIONS_PATH, connectionKeys } from "./use-ai-connections";

// The tools ONE connection can actually see -- the wizard's step 10.
//
// WHY RAW fetch AND NOT THE GENERATED CLIENT. Same reason, same house pattern,
// as use-ai-connections.ts and features/mcp-consent/use-consent.ts, whose
// headers state it at length: the MCP routes are not in
// packages/openapi/openapi.yaml, apps/api/internal/mcp/dto.go hand-shapes them,
// and whether they should join the OpenAPI document is a contract question for
// the API owner rather than one settled here.
//
// WHAT THIS RENDERS AND WHAT IT MUST NOT ADD. The response is a list of
// {name, description} and NOTHING ELSE. There is deliberately no `available`
// flag on it, and this module must not compute one: the availability answer
// already lives inside the description text, and a second, client-derived
// answer could disagree with what a real tool call actually refuses. A badge
// saying "available" over a tool the server would refuse is worse than no
// badge, because it is the screen that exists to prove what works telling the
// operator something it did not check.
//
// THE LIST IS THIS GRANT'S, NOT THE REGISTRY'S. The server resolves it through
// registry.VisibleTools after both narrowing axes -- the capabilities the
// connection holds and the sites it may reach -- so a narrowed grant sees
// fewer tools than a wide one, and a screen that listed the registry would be
// promising an operator tools their own connection cannot call.

/** `GET /api/v1/mcp/connections/:connectionId/tools`. */
export function connectionToolsPath(connectionId: string): string {
  return `${CONNECTIONS_PATH}/${encodeURIComponent(connectionId)}/tools`;
}

export const connectionToolKeys = {
  all: [...connectionKeys.all, "tools"] as const,
  one: (connectionId: string) => [...connectionToolKeys.all, connectionId] as const,
};

/**
 * One tool, exactly as the server names it.
 *
 * `.strict()` is deliberately NOT used: a server that grows a field must not
 * blank this screen. What matters is that the two fields we render are present
 * and are strings, so a malformed entry fails the parse rather than rendering
 * as `undefined` in a list whose whole job is to be trusted.
 */
const toolSchema = z.object({
  name: z.string(),
  description: z.string(),
});

/**
 * The response envelope.
 *
 * `tools` IS REQUIRED AND IS NEVER NULL -- that is the server's own contract,
 * and it is asserted here rather than defended against with `?? []`. The
 * difference matters: `?? []` would turn a malformed response into a confident
 * "this connection can call nothing", which is this project's signature defect
 * and is exactly the claim this screen must never make by accident. A missing
 * or null `tools` fails the parse and surfaces as a failed load.
 */
const toolsResponseSchema = z.object({
  tools: z.array(toolSchema),
});

export interface ConnectionTool {
  readonly name: string;
  readonly description: string;
}

/**
 * The tools this connection can see.
 *
 * AN EMPTY ARRAY IS A REAL ANSWER AND IS NOT AN ERROR. A grant narrowed to
 * capabilities that confer no tool legitimately sees none, and the caller
 * renders that as the empty state it is. It is only distinguishable from a
 * failed read because a failed read rejects here rather than resolving to [].
 */
export function useConnectionTools(
  connectionId: string | null,
): UseQueryResult<readonly ConnectionTool[], Error> {
  return useQuery({
    queryKey: connectionToolKeys.one(connectionId ?? ""),
    // No id means no request. `enabled` rather than a fetch against a path with
    // an empty segment, which would be a 404 rendered as a failed load.
    enabled: connectionId !== null,
    queryFn: async (): Promise<readonly ConnectionTool[]> => {
      const res = await fetch(connectionToolsPath(connectionId ?? ""), {
        method: "GET",
        credentials: "include",
        headers: { Accept: "application/json" },
      });
      if (!res.ok) {
        throw new Error(
          `The tool list could not be read for this connection (HTTP ${String(res.status)}).`,
        );
      }
      return toolsResponseSchema.parse((await res.json()) as unknown).tools;
    },
  });
}
