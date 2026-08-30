import { describe, it, expect } from "vitest";
import { existsSync, readFileSync } from "node:fs";
import { join } from "node:path";

import { DEV_PROXY_PATHS, isProxied } from "./dev-proxy-paths";

// THE GO SERVER IS THE SOURCE OF TRUTH, NOT THIS ARRAY.
//
// A test that asserted DEV_PROXY_PATHS contains "/mcp" would restate the source
// and catch nothing. This reads the paths the API actually mounts on the ROOT
// engine and asserts each one is forwarded, so the failure it is built for --
// someone adds a root-mounted route and no proxy rule -- reddens here on the
// commit that introduces it.
//
// Root-mounted is the dangerous category. Anything under /api/v1 is already
// covered by the "/api" prefix and always has been; the three incidents were
// all routes hung off the bare engine, where the SPA's catch-all answers them
// with 200 and an HTML body that no client reports as an error.

const API_MCP = join(process.cwd(), "..", "api", "internal", "mcp");
const TRANSPORT_GO = join(API_MCP, "transport.go");
const DISCOVERY_GO = join(API_MCP, "discovery.go");

/**
 * Fail the test rather than silence the compiler.
 *
 * `!` would let a renamed Go constant flow in as undefined and assert against
 * nothing; this names what was missing.
 */
function must(value: string | undefined, what: string): string {
  if (value === undefined) throw new Error(`expected ${what}, got undefined`);
  return value;
}

/** Pull `const Name = "/literal"` values out of Go source. */
function goStringConsts(source: string): Record<string, string> {
  const out: Record<string, string> = {};
  const re = /(\w+)\s*=\s*"([^"]*)"/g;
  let m: RegExpExecArray | null;
  while ((m = re.exec(source)) !== null) out[m[1] ?? ""] = m[2] ?? "";
  return out;
}

describe("the dev proxy forwards every root-mounted API path", () => {
  it("can see the Go source it is checking against", () => {
    // Without this the whole file passes by reading nothing — the exact shape
    // ("a guard that finds nothing must go red") this project keeps producing.
    expect(existsSync(TRANSPORT_GO), `missing ${TRANSPORT_GO}`).toBe(true);
    expect(existsSync(DISCOVERY_GO), `missing ${DISCOVERY_GO}`).toBe(true);
  });

  it("forwards the MCP transport path the API mounts on the root engine", () => {
    const consts = goStringConsts(readFileSync(TRANSPORT_GO, "utf8"));
    // Read, not assumed. If the constant is renamed or moved this fails loudly
    // rather than silently checking undefined.
    const transportPath = must(consts.TransportPath, "TransportPath in transport.go");
    expect(transportPath).toBe("/mcp");
    expect(isProxied(transportPath)).toBe(true);
  });

  it("forwards all three OAuth discovery documents", () => {
    const src = readFileSync(DISCOVERY_GO, "utf8");
    const consts = goStringConsts(src);

    const authServer = must(
      consts.WellKnownAuthorizationServerPath,
      "WellKnownAuthorizationServerPath in discovery.go",
    );
    const protectedResource = must(
      consts.WellKnownProtectedResourcePath,
      "WellKnownProtectedResourcePath in discovery.go",
    );
    expect(authServer).toBe("/.well-known/oauth-authorization-server");
    expect(protectedResource).toBe("/.well-known/oauth-protected-resource");

    // The third is built by concatenation in Go
    // (WellKnownProtectedResourcePath + TransportPath), so it is composed here
    // the same way rather than pasted as a literal.
    const protectedResourceMCP = `${protectedResource}/mcp`;

    for (const p of [authServer, protectedResource, protectedResourceMCP]) {
      expect(isProxied(p), `${p} is not forwarded by the dev proxy`).toBe(true);
    }
  });

  it("finds every /.well-known path the discovery handler declares", () => {
    // Belt and braces on the named constants above: if a FOURTH document is
    // added, this catches it without anyone editing this test.
    const src = readFileSync(DISCOVERY_GO, "utf8");
    const declared = [...src.matchAll(/"(\/\.well-known\/[^"]*)"/g)].map((m) => m[1] ?? "");
    expect(declared.length).toBeGreaterThanOrEqual(2);
    for (const p of declared) {
      expect(isProxied(p), `${p} is declared in discovery.go but not proxied`).toBe(true);
    }
  });
});

describe("isProxied implements Vite's matching rule", () => {
  it("treats a ^-prefixed key as a regular expression", () => {
    expect(isProxied("/mcp")).toBe(true);
    // Exact, so a future SPA route beginning with /mcp is not swallowed.
    expect(isProxied("/mcpanything")).toBe(false);
  });

  it("treats a plain key as a prefix", () => {
    expect(isProxied("/api/v1/sites")).toBe(true);
    expect(isProxied("/.well-known/oauth-protected-resource/mcp")).toBe(true);
  });

  it("does not forward SPA routes", () => {
    // The over-fire half: proxying an app route would break the dashboard in
    // dev, which is a worse failure than the one this guards.
    for (const p of ["/", "/ai", "/ai/connect", "/sites", "/login", "/connect/ai"]) {
      expect(isProxied(p), `${p} must be served by the SPA, not proxied`).toBe(false);
    }
  });

  it("keeps the pre-existing surfaces forwarded", () => {
    for (const p of ["/api", "/auth/callback", "/enroll", "/agent/x", "/healthz", "/readyz"]) {
      expect(isProxied(p)).toBe(true);
    }
    expect(DEV_PROXY_PATHS.length).toBeGreaterThanOrEqual(8);
  });
});
