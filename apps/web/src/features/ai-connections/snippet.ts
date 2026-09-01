// Setup-artefact generation for step 6 of the connection wizard.
//
// THE EXIT GATE FOR THIS SLICE IS THAT EVERY PUBLISHED SNIPPET COMES FROM HERE.
// Nothing in the UI may hand-write a config block for a client, because a
// hand-written block is a copy of the table that drifts from it silently. The
// test beside this file walks MCP_CLIENTS and asserts the shape of what every
// row produces, so "one row changes when a client changes" stays true.
//
// No React, no DOM, no clipboard. Pure input to pure output, so the marketing
// site can render the same strings from the same table.

import {
  isAuthAvailable,
  type AuthMethod,
  type McpClientRow,
} from "./client-table";

/**
 * The literal a user replaces with their own token.
 *
 * A PLACEHOLDER, NOT AN EMPTY STRING. An empty value would produce a config
 * that looks complete, parses, and fails authentication with no clue why. It is
 * also never a real token in the OAuth case: for OAuth there is no token at
 * step 6, because the key is not minted until step 7.
 */
export const TOKEN_PLACEHOLDER = "YOUR_CONNECTION_TOKEN";

/** The header every Streamable HTTP client sends a bearer credential in. */
export const AUTH_HEADER_NAME = "Authorization";

export type Snippet =
  | {
      readonly kind: "json";
      readonly language: "json";
      readonly text: string;
      /** Surfaced beside the block so the reason for the type line is on screen. */
      readonly note: string;
    }
  | {
      readonly kind: "gui";
      readonly steps: readonly string[];
      /** The endpoint, so the page can offer it as its own copy target. */
      readonly url: string;
    }
  | {
      readonly kind: "raw";
      readonly url: string;
      readonly reason: string;
      /** The exact header to send, or null when this method needs no header. */
      readonly headerLine: string | null;
    }
  | {
      readonly kind: "shell";
      /**
       * The full script, ready to paste as-is. NEVER CONTAINS THE TOKEN VALUE --
       * see UnsupportedAuthMethodError's sibling guarantee below and
       * snippet.test.ts's assertion on this exact property. The token is read
       * interactively by the script itself, not substituted into it.
       */
      readonly text: string;
      readonly reason: string;
    };

export interface SnippetInput {
  readonly client: McpClientRow;
  /** Absolute endpoint URL, e.g. https://app.wpmgr.app/mcp. Never assembled here. */
  readonly endpointUrl: string;
  /** The connection's name, used as the server key inside the config. */
  readonly serverName: string;
  readonly authMethod: AuthMethod;
  /** A minted token, or null to emit the placeholder. */
  readonly token?: string | null;
}

/**
 * Thrown when a snippet is requested for a combination the table says cannot
 * work.
 *
 * A NAMED THROW RATHER THAN A BEST-EFFORT BLOCK. The failure this guards is
 * precise: a copy button emitting a snippet for the wrong client shape, which
 * the user cannot tell from a right one until it silently does not connect.
 * Returning something plausible here is the whole defect class, so this refuses
 * instead.
 */
export class UnsupportedAuthMethodError extends Error {
  readonly clientId: string;
  readonly method: AuthMethod;
  constructor(client: McpClientRow, method: AuthMethod) {
    super(
      `${client.name} cannot use ${method === "oauth" ? "browser sign-in" : "a connection token"}.`,
    );
    this.name = "UnsupportedAuthMethodError";
    this.clientId = client.id;
    this.method = method;
  }
}

/**
 * Turn a connection name into a config key.
 *
 * Config keys sit in JSON objects and in shell-adjacent contexts, so anything
 * outside `[a-z0-9-]` is folded to a dash. An empty result falls back to
 * "wpmgr" -- the one place a default is correct, because the key names OUR
 * server and is not a user-supplied fact about anything.
 */
export function serverKey(name: string): string {
  const slug = name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-+|-+$/g, "");
  return slug.length > 0 ? slug : "wpmgr";
}

/** The exact `Authorization` header value for a token. */
export function bearerHeaderValue(token: string | null | undefined): string {
  return `Bearer ${token != null && token.length > 0 ? token : TOKEN_PLACEHOLDER}`;
}

export function buildSnippet(input: SnippetInput): Snippet {
  const { client, endpointUrl, serverName, authMethod } = input;

  // Refuse before generating anything. See UnsupportedAuthMethodError.
  if (!isAuthAvailable(client, authMethod)) {
    throw new UnsupportedAuthMethodError(client, authMethod);
  }

  const config = client.config;

  if (config.kind === "gui") {
    // Generated from the row, not written per client, so eight GUI clients
    // would still be one implementation.
    return {
      kind: "gui",
      url: endpointUrl,
      steps: [
        `Open ${client.name} and go to its connectors or MCP settings.`,
        `Add a remote server and paste ${endpointUrl} into the "${config.fieldLabel}" field.`,
        "Save. The client will send you to this dashboard to approve the connection.",
      ],
    };
  }

  if (config.kind === "raw") {
    return {
      kind: "raw",
      url: endpointUrl,
      reason: config.reason,
      headerLine:
        authMethod === "token"
          ? `${AUTH_HEADER_NAME}: ${bearerHeaderValue(input.token)}`
          : null,
    };
  }

  if (config.kind === "shell") {
    // Shell setup exists to protect a token, so it has nothing to say for
    // OAuth, which mints no token at this step at all.
    if (authMethod !== "token") {
      throw new UnsupportedAuthMethodError(client, authMethod);
    }
    const lines = [
      // A LEADING SPACE, ON PURPOSE, NOT A STRAY CHARACTER. Under
      // `HISTCONTROL=ignorespace` (bash) or `setopt HIST_IGNORE_SPACE` (zsh),
      // a line starting with whitespace is never written to shell history at
      // all -- this is what keeps the read itself, not only the token, out of
      // a history file an operator may not think to check.
      ` read -rs -p "Paste your connection token, then press Enter: " WPMGR_CONNECTION_TOKEN`,
      `export WPMGR_CONNECTION_TOKEN`,
      // Referenced from the environment, never interpolated: the token text
      // itself never becomes part of this string, at any point, for any
      // input. That is the property snippet.test.ts asserts directly.
      `curl -sS -H "${AUTH_HEADER_NAME}: Bearer $WPMGR_CONNECTION_TOKEN" ${endpointUrl}`,
    ];
    return {
      kind: "shell",
      text: lines.join("\n"),
      reason: config.reason,
    };
  }

  // JSON. Built as an object and stringified rather than concatenated, so a
  // value containing a quote cannot break out of the block.
  const server: Record<string, unknown> = {};
  // `type` first when present: it is the key that decides how the whole entry
  // is interpreted, and it reads better above the URL it qualifies.
  if (config.typeValue !== null) server.type = config.typeValue;
  server[config.urlKey] = endpointUrl;
  if (authMethod === "token") {
    if (!config.supportsHeaders) {
      // Unreachable while the table keeps token unavailable for header-less
      // clients, and a throw rather than a silently header-less config if that
      // ever stops being true.
      throw new UnsupportedAuthMethodError(client, "token");
    }
    server.headers = { [AUTH_HEADER_NAME]: bearerHeaderValue(input.token) };
  }

  const doc = { [config.wrapperKey]: { [serverKey(serverName)]: server } };

  return {
    kind: "json",
    language: "json",
    text: JSON.stringify(doc, null, 2),
    note: config.typeNote,
  };
}
