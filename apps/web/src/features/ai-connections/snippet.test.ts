// The table's test. This is S16's exit gate: every published snippet is
// generated from the tested table, never hand-written.
//
// It has three jobs, and the second and third are the ones that survive
// refactors:
//
//   1. Assert each recorded per-client difference by name, so a row edited to a
//      wrong value fails with the client's name in the message.
//   2. Walk the WHOLE table and assert the generated shape agrees with the row
//      that produced it, so a NEW row is covered on the day it is added rather
//      than on the day someone remembers to write a case for it.
//   3. Scan the shipped feature sources for a hand-written config block, so the
//      exit gate is enforced rather than merely stated.

import { describe, it, expect } from "vitest";
import { existsSync, readFileSync, readdirSync } from "node:fs";
import { join } from "node:path";

import {
  MCP_CLIENTS,
  CLIENT_TABLE_TARGET_NAMED_COUNT,
  availableAuthMethods,
  findClient,
  isAuthAvailable,
  type AuthMethod,
  type McpClientRow,
} from "./client-table";
import {
  buildSnippet,
  serverKey,
  TOKEN_PLACEHOLDER,
  UnsupportedAuthMethodError,
  type Snippet,
} from "./snippet";

const ENDPOINT = "https://app.wpmgr.app/mcp";

/**
 * Narrow away `undefined` by failing the test.
 *
 * `!` would silence the compiler and then produce a confusing runtime error;
 * this fails with the reason, and it never lets an absent value flow into an
 * assertion that would then pass against `undefined`.
 */
function must<T>(value: T | undefined, what: string): T {
  if (value === undefined) throw new Error(`expected ${what}, got undefined`);
  return value;
}

function snippetFor(
  id: string,
  method: AuthMethod,
  token: string | null = null,
): Snippet {
  const client = findClient(id);
  // A missing row must fail the test, never fall through to a default row and
  // assert against something else's shape.
  if (client === undefined) throw new Error(`no client row with id "${id}"`);
  return buildSnippet({
    client,
    endpointUrl: ENDPOINT,
    serverName: "wpmgr",
    authMethod: method,
    token,
  });
}

function jsonDoc(s: Snippet): Record<string, Record<string, Record<string, unknown>>> {
  if (s.kind !== "json") throw new Error(`expected a json snippet, got "${s.kind}"`);
  return JSON.parse(s.text) as Record<string, Record<string, Record<string, unknown>>>;
}

/** The single server object inside a generated JSON block. */
function serverObject(s: Snippet, wrapper: string): Record<string, unknown> {
  const doc = jsonDoc(s);
  const inner = must(doc[wrapper], `a "${wrapper}" wrapper key in the generated block`);
  const keys = Object.keys(inner);
  expect(keys).toHaveLength(1);
  return must(inner[must(keys[0], "a server key")], "a server object");
}

// ---------------------------------------------------------------------------
// 1. The recorded per-client differences, each asserted by name.
// ---------------------------------------------------------------------------

describe("per-client config differences (design §18, verified 2026-08-24)", () => {
  it("wraps every client in mcpServers except VS Code, which uses servers", () => {
    // NOT A VACUOUS LOOP. Without this the `continue` below would let the whole
    // case pass by checking nothing if every row stopped being a json client.
    const jsonClients = MCP_CLIENTS.filter((c) => c.config.kind === "json");
    expect(jsonClients.length).toBeGreaterThanOrEqual(5);

    for (const client of MCP_CLIENTS) {
      if (client.config.kind !== "json") continue;
      const expected = client.id === "vscode" ? "servers" : "mcpServers";
      expect(client.config.wrapperKey, `${client.name} wrapper key`).toBe(expected);
      // Asserted on the OUTPUT too, not only the row, because the row is only
      // half the claim; the generator has to honour it.
      const first = must(availableAuthMethods(client)[0], `${client.name} to have an auth method`);
      const doc = jsonDoc(snippetFor(client.id, first));
      expect(Object.keys(doc), `${client.name} generated wrapper`).toEqual([expected]);
    }
  });

  it("Claude Code emits the required http type", () => {
    const server = serverObject(snippetFor("claude-code", "oauth"), "mcpServers");
    // A URL with no type is read as a local process and skipped with an error.
    expect(server.type).toBe("http");
    expect(server.url).toBe(ENDPOINT);
  });

  it("Cursor emits no type key at all", () => {
    const server = serverObject(snippetFor("cursor", "oauth"), "mcpServers");
    expect(Object.keys(server)).not.toContain("type");
    expect(server.url).toBe(ENDPOINT);
  });

  it("VS Code emits type http under the servers wrapper", () => {
    const server = serverObject(snippetFor("vscode", "token"), "servers");
    expect(server.type).toBe("http");
    expect(server.url).toBe(ENDPOINT);
  });

  it("Gemini CLI uses httpUrl, because the key picks the transport", () => {
    const server = serverObject(snippetFor("gemini-cli", "oauth"), "mcpServers");
    expect(server.httpUrl).toBe(ENDPOINT);
    // `url` would select the older event-stream transport, which we do not
    // serve. Emitting both would be worse than emitting the wrong one.
    expect(Object.keys(server)).not.toContain("url");
  });

  it("Windsurf uses serverUrl", () => {
    const server = serverObject(snippetFor("windsurf", "token"), "mcpServers");
    expect(server.serverUrl).toBe(ENDPOINT);
    expect(Object.keys(server)).not.toContain("url");
  });

  it("Claude Desktop and ChatGPT are OAuth only, with the reason recorded", () => {
    for (const id of ["claude-desktop", "chatgpt"]) {
      // must(), not `expect(...).toBeDefined()` followed by a `continue`: the
      // continue form passes vacuously if the row is ever removed.
      const client = must(findClient(id), `a row for "${id}"`);
      expect(availableAuthMethods(client)).toEqual(["oauth"]);
      const token = client.auth.token;
      expect(token.state).toBe("unavailable");
      // Never a generic "unavailable": the disabled card carries the specific
      // reason, which for both of these is the absent header field.
      if (token.state === "unavailable") {
        expect(token.reason).toMatch(/header field/i);
      }
    }
  });

  it("the generic Streamable HTTP entry exists and is first-class", () => {
    const generic = MCP_CLIENTS.filter((c) => c.generic);
    expect(generic).toHaveLength(1);
    const row = must(generic[0], "the generic row");
    expect(row.id).toBe("generic");
    // Both methods, a spec link, and no "if all else fails" framing.
    expect(availableAuthMethods(row)).toEqual(["oauth", "token"]);
    expect(row.docsUrl).toMatch(/^https:\/\//);
  });
});

// ---------------------------------------------------------------------------
// 2. The whole-table walk. New rows are covered automatically.
// ---------------------------------------------------------------------------

describe("every row in the table", () => {
  it("has rows to walk", () => {
    // A broken import or an emptied table must redden here rather than turning
    // every it.each below into a silent zero-case pass.
    expect(MCP_CLIENTS.length).toBeGreaterThanOrEqual(9);
  });

  it("declares the gap between the shipped rows and the eleven the design calls for", () => {
    // The design specifies eleven named clients plus the generic entry; the
    // source it was verified against enumerates eight. The shortfall is a
    // recorded, visible number rather than three padded-out product names with
    // guessed config shapes.
    const named = MCP_CLIENTS.filter((c) => !c.generic).length;
    expect(named).toBeLessThanOrEqual(CLIENT_TABLE_TARGET_NAMED_COUNT);
    expect(named).toBe(8);
  });

  it.each(MCP_CLIENTS.map((c) => [c.name, c] as const))(
    "%s carries complete, sourced metadata",
    (_name, client: McpClientRow) => {
      expect(client.id).toMatch(/^[a-z0-9-]+$/);
      expect(client.name.length).toBeGreaterThan(0);
      expect(client.blurb.length).toBeGreaterThan(0);
      expect(client.docsUrl).toMatch(/^https:\/\//);
      expect(client.docsLabel.length).toBeGreaterThan(0);
      expect(client.verifiedAt).toMatch(/^\d{4}-\d{2}-\d{2}$/);

      // Every unverified state carries the date we last checked, so staleness
      // is visible on the card instead of becoming permanent.
      for (const method of ["oauth", "token"] as const) {
        const a = client.auth[method];
        if (a.state === "unverified") {
          expect(a.lastCheckedAt, `${client.name}.${method}`).toMatch(/^\d{4}-\d{2}-\d{2}$/);
          expect(a.reason.length).toBeGreaterThan(0);
        }
        if (a.state === "unavailable") {
          // A specific reason, never a generic "unavailable".
          expect(a.reason.length, `${client.name}.${method} reason`).toBeGreaterThan(20);
          expect(a.reason.toLowerCase()).not.toBe("unavailable");
        }
      }
    },
  );

  it.each(MCP_CLIENTS.map((c) => [c.name, c] as const))(
    "%s generates a snippet that agrees with its own row",
    (_name, client: McpClientRow) => {
      const methods = availableAuthMethods(client);
      expect(methods.length, `${client.name} has no usable auth method`).toBeGreaterThan(0);

      for (const method of methods) {
        const snippet = buildSnippet({
          client,
          endpointUrl: ENDPOINT,
          serverName: "Fleet manager",
          authMethod: method,
        });

        if (client.config.kind === "json") {
          const cfg = client.config;
          if (snippet.kind !== "json") throw new Error("expected json");
          const doc = jsonDoc(snippet);
          expect(Object.keys(doc)).toEqual([cfg.wrapperKey]);
          const server = serverObject(snippet, cfg.wrapperKey);

          // The URL lands under the row's key and under no other URL key.
          expect(server[cfg.urlKey]).toBe(ENDPOINT);
          for (const other of ["url", "httpUrl", "serverUrl"] as const) {
            if (other === cfg.urlKey) continue;
            expect(Object.keys(server), `${client.name} emitted a second URL key`).not.toContain(
              other,
            );
          }

          // `type` present exactly when the row says so.
          if (cfg.typeValue === null) {
            expect(Object.keys(server), `${client.name} must not emit type`).not.toContain("type");
          } else {
            expect(server.type, `${client.name} type`).toBe(cfg.typeValue);
          }

          // Token auth produces a real Authorization header, never an empty one.
          if (method === "token") {
            const headers = server.headers as Record<string, string> | undefined;
            expect(headers, `${client.name} token config needs headers`).toBeDefined();
            expect(headers?.Authorization).toBe(`Bearer ${TOKEN_PLACEHOLDER}`);
          } else {
            expect(Object.keys(server)).not.toContain("headers");
          }

          // The server key is slugged from the connection name.
          expect(
            Object.keys(must(doc[cfg.wrapperKey], `the ${cfg.wrapperKey} wrapper`)),
          ).toEqual(["fleet-manager"]);

          // No Windows path may be printed until someone verifies one.
          expect(snippet.text).not.toMatch(/[A-Z]:\\|%APPDATA%/);
        }

        if (client.config.kind === "gui") {
          if (snippet.kind !== "gui") throw new Error("expected gui");
          expect(snippet.url).toBe(ENDPOINT);
          expect(snippet.steps.length).toBeGreaterThanOrEqual(3);
          // The endpoint has to actually appear in the instructions.
          expect(snippet.steps.join(" ")).toContain(ENDPOINT);
        }

        if (client.config.kind === "raw") {
          if (snippet.kind !== "raw") throw new Error("expected raw");
          expect(snippet.url).toBe(ENDPOINT);
          expect(snippet.reason.length).toBeGreaterThan(20);
          expect(snippet.headerLine).toBe(
            method === "token" ? `Authorization: Bearer ${TOKEN_PLACEHOLDER}` : null,
          );
        }
      }
    },
  );

  it("has at least one client that cannot use a method, so the refusal case is real", () => {
    // Without this, the per-client refusal cases below would every one of them
    // pass by having nothing to refuse.
    const refusable = MCP_CLIENTS.filter(
      (c) => availableAuthMethods(c).length < 2,
    );
    expect(refusable.length).toBeGreaterThanOrEqual(3);
  });

  it.each(MCP_CLIENTS.map((c) => [c.name, c] as const))(
    "%s refuses a snippet for a method it cannot use",
    (_name, client: McpClientRow) => {
      for (const method of ["oauth", "token"] as const) {
        if (isAuthAvailable(client, method)) continue;
        // Not a best-effort block: the copy button must have nothing to copy.
        expect(() =>
          buildSnippet({
            client,
            endpointUrl: ENDPOINT,
            serverName: "wpmgr",
            authMethod: method,
          }),
        ).toThrow(UnsupportedAuthMethodError);
      }
    },
  );
});

// ---------------------------------------------------------------------------
// 3. Token handling and slugging.
// ---------------------------------------------------------------------------

describe("token substitution", () => {
  it("emits the placeholder when no token has been minted yet", () => {
    const server = serverObject(snippetFor("cursor", "token", null), "mcpServers");
    const headers = server.headers as Record<string, string>;
    // An empty value would produce a config that parses and fails auth with no
    // clue why.
    expect(headers.Authorization).toBe(`Bearer ${TOKEN_PLACEHOLDER}`);
  });

  it("emits the real token once there is one", () => {
    const server = serverObject(snippetFor("cursor", "token", "wpm_live_abc123"), "mcpServers");
    const headers = server.headers as Record<string, string>;
    expect(headers.Authorization).toBe("Bearer wpm_live_abc123");
  });

  it("never puts a credential in the URL", () => {
    const snippet = snippetFor("claude-code", "token", "wpm_live_abc123");
    if (snippet.kind !== "json") throw new Error("expected json");
    const server = serverObject(snippet, "mcpServers");
    expect(String(server.url)).not.toContain("wpm_live_abc123");
  });
});

describe("serverKey", () => {
  it("slugs a name", () => {
    expect(serverKey("Fleet manager")).toBe("fleet-manager");
    expect(serverKey("  CI / nightly  ")).toBe("ci-nightly");
  });

  it("falls back to wpmgr for a name that slugs to nothing", () => {
    // The one correct default in this feature: the key names OUR server.
    expect(serverKey("///")).toBe("wpmgr");
    expect(serverKey("")).toBe("wpmgr");
  });
});

// ---------------------------------------------------------------------------
// 4. The exit gate itself: no hand-written config block anywhere in the UI.
// ---------------------------------------------------------------------------

const FEATURE_ROOT = join(process.cwd(), "src", "features", "ai-connections");
const ROUTES_ROOT = join(process.cwd(), "src", "routes", "_authed", "ai");

/** Every non-test .ts/.tsx file under `dir`. */
function sourceFiles(dir: string, out: string[] = []): string[] {
  if (!existsSync(dir)) return out;
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) sourceFiles(full, out);
    else if (/\.tsx?$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name)) out.push(full);
  }
  return out;
}

describe("no hand-written config snippet in the UI (S16 exit gate)", () => {
  // client-table.ts and snippet.ts are the table and its generator; they are
  // the only files allowed to contain these literals.
  const GENERATORS = new Set(["client-table.ts", "snippet.ts"]);
  const files = [...sourceFiles(FEATURE_ROOT), ...sourceFiles(ROUTES_ROOT)].filter(
    (f) => !GENERATORS.has(f.split("/").pop() ?? ""),
  );

  it("finds the files it is supposed to be guarding", () => {
    // A moved directory or a broken walk would otherwise make this whole
    // describe pass by checking nothing at all.
    expect(existsSync(FEATURE_ROOT)).toBe(true);
    expect(existsSync(join(FEATURE_ROOT, "client-table.ts"))).toBe(true);
    expect(files.length).toBeGreaterThanOrEqual(3);
  });

  it.each([
    ["mcpServers", /\bmcpServers\b/],
    ["servers wrapper", /"servers"\s*:/],
    ["httpUrl", /\bhttpUrl\b/],
    ["serverUrl", /\bserverUrl\b/],
    ['"type": "http"', /"type"\s*:\s*"http"/],
  ] as const)("no UI file writes %s by hand", (_label, pattern) => {
    const offenders = files.filter((f) => pattern.test(readFileSync(f, "utf8")));
    expect(offenders.map((f) => f.replace(process.cwd(), ""))).toEqual([]);
  });
});
