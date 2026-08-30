// Which paths the dev server forwards to the API.
//
// EXTRACTED FROM vite.config.ts SO IT CAN BE TESTED. A proxy entry nobody tests
// is exactly what let this ship three times: the Go route is mounted, no proxy
// rule exists, and the request is answered by something else with a 200. It
// cost self-hosted RUM ingest silently, then POST /mcp in production, then the
// OAuth discovery documents. The test beside this file reads the Go source and
// fails when a root-mounted path is not covered here, so the next one is caught
// by CI rather than by a developer wondering why their AI client gets HTML.
//
// The client uses same-origin relative paths (baseUrl ""), so every real API
// surface must be forwarded or it falls through to the SPA's index.html.

/**
 * Proxy keys, in Vite's own format.
 *
 * A key beginning with `^` is a regular expression; anything else is a prefix.
 * Both forms are deliberate below:
 *
 *   - `^/mcp$` is EXACT. A bare `/mcp` prefix would also capture any future
 *     `/mcpsomething` SPA route, and the transport is one exact path.
 *   - `/.well-known` is a PREFIX, covering all three discovery documents and
 *     any added later. The SPA serves nothing under it, so breadth here is
 *     safety rather than sloppiness.
 */
export const DEV_PROXY_PATHS: readonly string[] = [
  "/api",
  "/auth",
  "/enroll",
  "/agent",
  "/healthz",
  "/readyz",
  // Root-mounted on the Gin engine, not under /api/v1 — which is precisely why
  // they were missed. internal/mcp/transport.go mounts POST /mcp;
  // internal/mcp/discovery.go mounts the three /.well-known/oauth-* documents.
  "^/mcp$",
  "/.well-known",
];

/**
 * Whether `path` would be forwarded to the API by the dev server.
 *
 * Implements Vite's matching rule so the test asks the same question the dev
 * server does, rather than a restatement of the array above.
 */
export function isProxied(path: string): boolean {
  return DEV_PROXY_PATHS.some((key) =>
    key.startsWith("^") ? new RegExp(key).test(path) : path.startsWith(key),
  );
}
