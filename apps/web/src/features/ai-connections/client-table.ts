// The AI client table (design §18, "Each client needs different configuration
// output").
//
// THIS FILE IS DATA, NOT COMPONENTS. The design's constraint is verbatim:
// "These rows are a data table in the codebase with a test, not eight
// hand-written components. Each row carries the URL key, whether to emit a
// type, the available auth methods, the config path [...] and a verified_at
// date. The page renders from the table; when a client changes, one row
// changes."
//
// It imports nothing from React, the router or the design system on purpose, so
// moving it into a shared package for the marketing site to render the same
// truth is a file move rather than an untangling.
//
// EVERY FACT BELOW IS SOURCED, AND A FACT WE DO NOT HAVE IS MODELLED AS ABSENT
// RATHER THAN GUESSED. That is not fastidiousness: the governing defect in this
// codebase is a failure or an absence quietly coerced into a plausible value,
// and on this surface the plausible value is a config block that a user pastes,
// that parses, and that silently never connects. A row we cannot source emits
// no snippet at all (kind: "raw") and says why.

/** ISO date the config facts in this table were checked against client docs. */
export const CLIENT_TABLE_VERIFIED_AT = "2026-08-24";

/**
 * The protocol revision we negotiate for, and the floor below which a client is
 * refused. Both are rendered to the operator (design §6: "The connect screens
 * render the same two numbers").
 */
export const PROTOCOL_TARGET_VERSION = "2025-11-25";
export const PROTOCOL_FLOOR_VERSION = "2025-03-26";

/** Path of the single Streamable HTTP endpoint (apps/api/internal/mcp/transport.go: TransportPath). */
export const MCP_TRANSPORT_PATH = "/mcp";

/**
 * The two auth methods a user chooses between in step 5.
 *
 * There is no third. A device-code flow is deferred past Phase 2 (design §18),
 * so a client that can do neither of these cannot connect and the wizard says
 * so rather than offering a method that cannot work.
 */
export type AuthMethod = "oauth" | "token";

/**
 * Whether an auth method is usable with a client, and WHY when it is not.
 *
 * Three states, not two, and the third is the point. "We know this cannot work"
 * and "we have not checked" are different facts about different things -- the
 * first is about the client, the second is about us -- and collapsing them is
 * how a hardcoded "not verified" list becomes permanent instead of visibly
 * stale. `unverified` carries the date we last looked so the staleness is on
 * screen.
 */
export type AuthAvailability =
  | { readonly state: "available"; readonly detail: string }
  | { readonly state: "unavailable"; readonly reason: string }
  | {
      readonly state: "unverified";
      readonly reason: string;
      readonly lastCheckedAt: string;
    };

/**
 * A client whose remote-server config is a JSON object we can generate.
 *
 * Every field here exists because at least one client differs on it, and each
 * difference breaks a copied config silently rather than loudly.
 */
export interface JsonConfigShape {
  readonly kind: "json";
  /**
   * The top-level wrapper key. `mcpServers` for every client in the survey
   * except VS Code, which uses `servers`.
   */
  readonly wrapperKey: "mcpServers" | "servers";
  /**
   * The key that carries the endpoint URL. Three distinct values in eight
   * clients, and for Gemini CLI the key IS the transport selector: `httpUrl`
   * means Streamable HTTP, `url` means the older event-stream transport, so
   * emitting the wrong one connects the client to a transport we do not serve.
   */
  readonly urlKey: "url" | "httpUrl" | "serverUrl";
  /**
   * The value of the `type` key, or null when no `type` is emitted.
   *
   * Null covers two different reasons and `typeNote` carries which: Cursor MUST
   * NOT receive one, while Windsurf and Gemini CLI simply have none.
   */
  readonly typeValue: string | null;
  /** Rendered to the operator as the reason for the line above. */
  readonly typeNote: string;
  /** True when the client documents a headers map, which is what token auth needs. */
  readonly supportsHeaders: boolean;
}

/**
 * A client configured through its own UI, with no file to write.
 *
 * `fieldLabel` is the only per-client difference; the steps themselves are
 * generated in snippet.ts so they stay one implementation rather than two
 * hand-written paragraphs.
 */
export interface GuiConfigShape {
  readonly kind: "gui";
  readonly fieldLabel: string;
}

/**
 * A client we render the raw URL for, with no config block.
 *
 * Two very different rows use this shape and both are deliberate:
 *
 *  - The generic entry, which is first-class by design ("the twelfth entry is
 *    what stops the list becoming a maintenance trap -- and it is a first-class
 *    option in the same visual weight, not a consolation").
 *  - A named client whose file format we have not fully sourced. Codex CLI is
 *    the live case: the design records that its config is TOML and that the URL
 *    key is `url`, but not the table header a remote server sits under. A
 *    generated `[mcp_servers.name]` would be a guess that parses, and a config
 *    that parses and does not work is worse than no config at all.
 */
export interface RawConfigShape {
  readonly kind: "raw";
  /** Why there is no generated block. Rendered; never a code comment only. */
  readonly reason: string;
}

/**
 * A client set up by running a command in a shell, not by editing a file or a
 * GUI field.
 *
 * TOKEN AUTH HERE NEVER PUTS THE TOKEN IN THE COMMAND TEXT. A bearer
 * credential typed or pasted as a shell argument is written to the shell
 * history file in plain text and stays there, readable by anything that can
 * read that file, for as long as the credential is valid. The generator
 * (snippet.ts) reads the token interactively with `read -rs` -- never an
 * argument, never echoed -- into `$WPMGR_CONNECTION_TOKEN` and references
 * that variable rather than the value, so the value itself never appears in
 * generated text at all.
 */
export interface ShellConfigShape {
  readonly kind: "shell";
  /** Rendered beside the block: why this client is set up by command rather than by file. */
  readonly reason: string;
}

export type ConfigShape = JsonConfigShape | GuiConfigShape | RawConfigShape | ShellConfigShape;

/**
 * A POSIX config file location.
 *
 * WINDOWS IS ABSENT BY DECISION, not by omission. The design: "Windows paths
 * are unverified for every command-line client. Every path in the sources was
 * documented in POSIX form only. Show the POSIX path and link to the client's
 * own docs; do not print a Windows path for any of them until someone checks."
 * There is deliberately no `windows` field to fill in carelessly.
 */
export interface PosixConfigPath {
  readonly posix: string;
  /** ISO date this specific path was checked. */
  readonly verifiedAt: string;
}

export interface McpClientRow {
  readonly id: string;
  readonly name: string;
  /** One line, operator-facing, rendered on the picker card. */
  readonly blurb: string;
  readonly docsUrl: string;
  readonly docsLabel: string;
  /** ISO date this row's config facts were checked. */
  readonly verifiedAt: string;
  /**
   * Null across the board in this slice, and that is a recorded gap rather than
   * an oversight -- see CONFIG_PATH_GAP below. The field is in the schema so
   * that filling it is a one-row change, which is the whole point of the table.
   */
  readonly configPath: PosixConfigPath | null;
  readonly config: ConfigShape;
  readonly auth: {
    readonly oauth: AuthAvailability;
    readonly token: AuthAvailability;
  };
  /**
   * True only for the generic Streamable HTTP entry. It changes the copy, never
   * the visual weight: the design is explicit that it is not a consolation
   * prize, so nothing may render it smaller, later or greyer than the rest.
   */
  readonly generic: boolean;
}

/**
 * Why every `configPath` below is null.
 *
 * Stated as data so the page can render it, rather than buried in a comment
 * where it would read on screen as "this client has no config file". The
 * design wants the POSIX path shown; the honest position today is that nobody
 * on this change verified one, and a path recalled from memory and stamped with
 * someone else's verification date is a false provenance claim on exactly the
 * surface where a wrong path wastes an hour.
 */
/**
 * What a self-hosted operator has to do for the endpoint we print to work.
 *
 * WE CANNOT VERIFY THIS FROM THE BROWSER, SO WE SAY SO RATHER THAN IMPLYING IT
 * IS FINE. The endpoint is derived from the running origin, which is right, but
 * derivable is not routed:
 *
 *   - Hosted IS routed. infra/urlmap.yaml:81 lists `/mcp`.
 *   - The self-hosted reverse proxy is NOT. infra/nginx/nginx.conf carries
 *     locations for /api/, /auth/, /enroll, /agent/, /rum/, /webhooks/,
 *     /healthz and /readyz, and none for /mcp -- so `location /` serves the
 *     SPA and the copied URL returns a web page.
 *   - Neither is the dev server. apps/web/vite.config.ts proxies /api, /auth,
 *     /enroll, /agent, /healthz and /readyz only.
 *
 * That exact shape -- a Go route mounted, no proxy rule, silently answered by
 * something else -- is what nginx.conf's own header comment records happening
 * to self-hosted RUM ingest, and what made POST /mcp unreachable in production
 * once already. A wizard that prints the URL and says nothing hands a
 * self-hosted operator a web page and lets them debug their AI client.
 *
 * Phrased as a deployment requirement rather than an alarm: for a hosted
 * operator it is already satisfied, and a warning there would be crying wolf.
 */
export const SELF_HOSTED_PROXY_REQUIREMENT =
  "Self-hosted: your reverse proxy must forward /mcp to the API, alongside the /api and /auth rules it already has. We cannot check that from your browser, so we say it rather than assume it.";

export const CONFIG_PATH_GAP =
  "We have not verified config file locations ourselves, so we link to each client's own docs instead of printing a path that may have moved.";

/**
 * The design calls for eleven named clients plus the generic entry. The source
 * table it was verified from enumerates eight. The shortfall is declared here
 * and asserted in the test rather than being padded with three plausible
 * product names and guessed config shapes.
 */
export const CLIENT_TABLE_TARGET_NAMED_COUNT = 11;

/** Shared wording for the token method on any client that documents a headers map. */
const HEADER_TOKEN_DETAIL =
  "Paste the connection token into the client's headers map as an Authorization header.";

export const MCP_CLIENTS: readonly McpClientRow[] = [
  {
    id: "claude-code",
    name: "Claude Code",
    blurb: "Anthropic's terminal client.",
    docsUrl: "https://docs.claude.com/en/docs/claude-code/mcp",
    docsLabel: "Claude Code MCP docs",
    verifiedAt: CLIENT_TABLE_VERIFIED_AT,
    configPath: null,
    config: {
      kind: "json",
      wrapperKey: "mcpServers",
      urlKey: "url",
      typeValue: "http",
      // The loudest silent failure in the table: the client does not error on
      // the URL, it reclassifies the whole entry.
      typeNote:
        'Required. A URL with no "type" is read as a local process to launch, and the server is skipped with an error.',
      supportsHeaders: true,
    },
    auth: {
      oauth: {
        state: "available",
        detail: "Sign in through the browser; the client stores the key itself.",
      },
      token: { state: "available", detail: HEADER_TOKEN_DETAIL },
    },
    generic: false,
  },
  {
    id: "claude-desktop",
    name: "Claude Desktop",
    blurb: "The desktop app's connector directory.",
    docsUrl: "https://support.claude.com/en/articles/11175166-about-custom-connectors-via-remote-mcp-servers",
    docsLabel: "Claude Desktop connector docs",
    verifiedAt: CLIENT_TABLE_VERIFIED_AT,
    configPath: null,
    config: { kind: "gui", fieldLabel: "Remote MCP server URL" },
    auth: {
      oauth: {
        state: "available",
        detail: "The only way this client can connect. Sign-in happens in the browser.",
      },
      token: {
        state: "unavailable",
        // Not a preference and not a limitation of ours.
        reason:
          "The add-connector dialog accepts a URL and nothing else. There is no header field, so a token cannot be entered at all.",
      },
    },
    generic: false,
  },
  {
    id: "chatgpt",
    name: "ChatGPT",
    blurb: "Connectors in the ChatGPT apps.",
    docsUrl: "https://platform.openai.com/docs/mcp",
    docsLabel: "ChatGPT connector docs",
    verifiedAt: CLIENT_TABLE_VERIFIED_AT,
    configPath: null,
    config: { kind: "gui", fieldLabel: "MCP server URL" },
    auth: {
      oauth: {
        state: "available",
        detail: "Registers itself with us automatically, then sends you here to approve.",
      },
      token: {
        state: "unavailable",
        reason: "The connector dialog has no header field, so a token cannot be entered at all.",
      },
    },
    generic: false,
  },
  {
    id: "codex-cli",
    name: "Codex CLI",
    blurb: "OpenAI's terminal client.",
    docsUrl: "https://developers.openai.com/codex/mcp",
    docsLabel: "Codex CLI MCP docs",
    verifiedAt: CLIENT_TABLE_VERIFIED_AT,
    configPath: null,
    config: {
      kind: "raw",
      reason:
        "This client's config is TOML rather than JSON. We have the URL key but not the table header a remote server sits under, so we show the endpoint and link to the docs rather than generate a block that would parse and never connect.",
    },
    auth: {
      oauth: {
        state: "available",
        detail: "Through the client's own login command, which opens this approval screen.",
      },
      token: {
        state: "unverified",
        reason:
          "This client reads the token from a named environment variable, but we have not verified the setting name that points at it.",
        lastCheckedAt: CLIENT_TABLE_VERIFIED_AT,
      },
    },
    generic: false,
  },
  {
    id: "cursor",
    name: "Cursor",
    blurb: "The Cursor editor.",
    docsUrl: "https://cursor.com/docs/context/mcp",
    docsLabel: "Cursor MCP docs",
    verifiedAt: CLIENT_TABLE_VERIFIED_AT,
    configPath: null,
    config: {
      kind: "json",
      wrapperKey: "mcpServers",
      urlKey: "url",
      typeValue: null,
      typeNote:
        'Must not be emitted. No remote server example in this client\'s docs carries a "type" key.',
      supportsHeaders: true,
    },
    auth: {
      oauth: {
        state: "available",
        detail: "Sign in through the browser when the editor first reaches the server.",
      },
      token: { state: "available", detail: HEADER_TOKEN_DETAIL },
    },
    generic: false,
  },
  {
    id: "vscode",
    name: "VS Code / Copilot",
    blurb: "Visual Studio Code's agent mode.",
    docsUrl: "https://code.visualstudio.com/docs/copilot/customization/mcp-servers",
    docsLabel: "VS Code MCP docs",
    verifiedAt: CLIENT_TABLE_VERIFIED_AT,
    configPath: null,
    config: {
      kind: "json",
      // The one client in the survey that does not use `mcpServers`.
      wrapperKey: "servers",
      urlKey: "url",
      typeValue: "http",
      typeNote: 'This client expects "type": "http" on a remote server.',
      supportsHeaders: true,
    },
    auth: {
      oauth: {
        state: "unverified",
        // The design is explicit: "Undocumented on the page we fetched -- do
        // not claim OAuth". Rendering this as "available" would send an
        // operator down a path we have never seen work.
        reason:
          "Browser sign-in was not documented on the page we checked. It may work; we have not confirmed it, so we do not claim it.",
        lastCheckedAt: CLIENT_TABLE_VERIFIED_AT,
      },
      token: { state: "available", detail: HEADER_TOKEN_DETAIL },
    },
    generic: false,
  },
  {
    id: "windsurf",
    name: "Windsurf / Devin",
    blurb: "The Windsurf editor and Devin.",
    docsUrl: "https://docs.windsurf.com/windsurf/cascade/mcp",
    docsLabel: "Windsurf MCP docs",
    verifiedAt: CLIENT_TABLE_VERIFIED_AT,
    configPath: null,
    config: {
      kind: "json",
      wrapperKey: "mcpServers",
      urlKey: "serverUrl",
      typeValue: null,
      typeNote: "This client has no type key; the URL key alone selects a remote server.",
      supportsHeaders: true,
    },
    auth: {
      oauth: {
        state: "unverified",
        reason:
          "This client's docs describe headers for remote servers and do not document a browser sign-in. We have not confirmed one, so we do not claim it.",
        lastCheckedAt: CLIENT_TABLE_VERIFIED_AT,
      },
      token: { state: "available", detail: HEADER_TOKEN_DETAIL },
    },
    generic: false,
  },
  {
    id: "gemini-cli",
    name: "Gemini CLI",
    blurb: "Google's terminal client.",
    docsUrl: "https://google-gemini.github.io/gemini-cli/docs/tools/mcp-server.html",
    docsLabel: "Gemini CLI MCP docs",
    verifiedAt: CLIENT_TABLE_VERIFIED_AT,
    configPath: null,
    config: {
      kind: "json",
      wrapperKey: "mcpServers",
      // THE KEY IS THE TRANSPORT. Not a naming quirk.
      urlKey: "httpUrl",
      typeValue: null,
      typeNote:
        'No type key. The URL key picks the transport instead: "httpUrl" is Streamable HTTP, and "url" would select the older event-stream transport we do not serve.',
      supportsHeaders: true,
    },
    auth: {
      oauth: {
        state: "available",
        detail: "Through the client's own OAuth command, which opens this approval screen.",
      },
      token: { state: "available", detail: HEADER_TOKEN_DETAIL },
    },
    generic: false,
  },
  {
    id: "generic",
    name: "Other / generic Streamable HTTP",
    blurb: "Any client that speaks Streamable HTTP. Same endpoint, same auth.",
    docsUrl: "https://modelcontextprotocol.io/specification/2025-06-18/basic/transports",
    docsLabel: "Streamable HTTP specification",
    verifiedAt: CLIENT_TABLE_VERIFIED_AT,
    configPath: null,
    config: {
      kind: "raw",
      reason:
        "Every client stores this differently. Here is the endpoint and how to authenticate; put them wherever your client keeps remote servers.",
    },
    auth: {
      oauth: {
        state: "available",
        detail: "Standard authorization code flow with PKCE and dynamic client registration.",
      },
      token: {
        state: "available",
        detail: "Send the connection token as an Authorization: Bearer header on every request.",
      },
    },
    // First-class, same visual weight. Nothing may treat this as a fallback.
    generic: true,
  },
];

/** Look a row up by id. Returns undefined for an unknown id; never a default row. */
export function findClient(id: string): McpClientRow | undefined {
  return MCP_CLIENTS.find((c) => c.id === id);
}

/** True when the client can use this auth method today. `unverified` is not `available`. */
export function isAuthAvailable(client: McpClientRow, method: AuthMethod): boolean {
  return client.auth[method].state === "available";
}

/** Every method this client can use. Empty is a real answer and the wizard must handle it. */
export function availableAuthMethods(client: McpClientRow): readonly AuthMethod[] {
  return (["oauth", "token"] as const).filter((m) => isAuthAvailable(client, m));
}
