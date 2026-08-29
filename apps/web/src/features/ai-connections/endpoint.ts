import { MCP_TRANSPORT_PATH } from "./client-table";

/**
 * The MCP endpoint for THIS deployment.
 *
 * DERIVED FROM THE ORIGIN, NOT HARDCODED. The dashboard is served same-origin
 * with the API (packages/openapi-client base URL is ""), so the transport this
 * browser is talking to is this browser's origin plus the transport path. A
 * literal https://app.wpmgr.app/mcp would print the wrong endpoint to every
 * self-hosted install, and print it as confidently as the right one.
 *
 * `fallbackOrigin` is used only where there is no window (unit tests, and any
 * future prerender). It is an explicit argument rather than a silent default so
 * a caller that reaches it has said so.
 */
export function mcpEndpointUrl(fallbackOrigin = ""): string {
  const origin =
    typeof window !== "undefined" && window.location?.origin
      ? window.location.origin
      : fallbackOrigin;
  // A relative path is still truthful when we have no origin; a made-up host
  // would not be.
  return `${origin}${MCP_TRANSPORT_PATH}`;
}
