import { useMutation, useQuery, type UseMutationResult, type UseQueryResult } from "@tanstack/react-query";

import { parseConsentContext, type ConsentContext } from "./consent-context";
import type { SiteScopeMode } from "./site-scope";

// Data loading for the consent screen.
//
// WHY RAW fetch AND NOT THE GENERATED CLIENT. The OAuth endpoints are not in
// packages/openapi/openapi.yaml. They are RFC 6749 / RFC 7591 shaped -- snake
// case, an `error` / `error_description` envelope a client library parses, and
// form-encoded on the token endpoint -- which apps/api/internal/mcp/dto.go
// hand-shapes for exactly that reason ("hand-shaped rather than reusing a house
// DTO convention"). The same house pattern is already used for other endpoints
// outside the generated contract: features/backups/use-snapshot-environment.ts,
// features/sharing/use-shares.ts and routes/accept.tsx all fetch a same-origin
// /api/v1 path with credentials included. Whether these endpoints should be
// added to the OpenAPI document is a contract question for the API owner and is
// flagged rather than decided here.
//
// The zod parse in parseConsentContext is not a substitute for generated types;
// it is the thing generated types would not give us. A generated type asserts a
// shape at compile time and believes the server at runtime. On this screen the
// runtime check IS the security control: a payload that does not carry a
// verified redirect host, or that claims an identity is verified, must produce a
// failed load rather than a rendered screen with a plausible hole in it.

export const CONSENT_AUTHORIZE_PATH = "/api/v1/oauth/mcp/authorize";
export const CONSENT_APPROVE_PATH = "/api/v1/oauth/mcp/consent";

export const consentKeys = {
  all: ["mcp-consent"] as const,
  authorize: (params: AuthorizeParams) =>
    [
      ...consentKeys.all,
      "authorize",
      params.client_id,
      params.redirect_uri,
      params.scope,
      params.state ?? "",
      params.code_challenge ?? "",
      params.code_challenge_method ?? "",
    ] as const,
};

export interface AuthorizeParams {
  readonly response_type: string;
  readonly client_id: string;
  readonly redirect_uri: string;
  readonly scope: string;
  readonly state?: string;
  readonly code_challenge?: string;
  readonly code_challenge_method?: string;
}

/**
 * An RFC 6749 section 5.2 error the OAuth endpoints answer with.
 *
 * Kept as a named class with the machine-readable `code` intact rather than
 * flattened into a generic Error, so the screen can tell "this client is not
 * registered" from "we are broken" and say the right thing. The house rule is
 * to branch on the code the server actually returns; every code below is one
 * oauthError() in apps/api/internal/mcp/dto.go emits.
 */
export class OAuthRequestError extends Error {
  readonly code: string;
  readonly status: number;
  constructor(code: string, description: string, status: number) {
    super(description || code);
    this.name = "OAuthRequestError";
    this.code = code;
    this.status = status;
  }
}

async function readOAuthError(res: Response): Promise<OAuthRequestError> {
  // A NON-JSON OR UNPARSEABLE BODY IS STILL A FAILURE. The catch below returns
  // an error, never a success with empty fields -- an infra failure must not be
  // reported to the user as a consent screen it can approve.
  let code = "server_error";
  let description = "";
  try {
    const body: unknown = await res.json();
    if (body && typeof body === "object") {
      const rec = body as Record<string, unknown>;
      if (typeof rec.error === "string" && rec.error.length > 0) code = rec.error;
      if (typeof rec.error_description === "string") description = rec.error_description;
    }
  } catch {
    // Leave the defaults. `code` is already the pessimistic value.
  }
  return new OAuthRequestError(code, description, res.status);
}

/**
 * Load the consent context for an authorization request.
 *
 * Every failure path throws. There is no path that resolves to a partial or
 * empty ConsentContext, because the screen renders its approve control on
 * success and a query that resolves with holes is a screen a user can approve
 * without having been told what they are approving.
 */
export function useConsentContext(
  params: AuthorizeParams | null,
): UseQueryResult<ConsentContext, Error> {
  return useQuery({
    queryKey: params ? consentKeys.authorize(params) : [...consentKeys.all, "authorize", "none"],
    enabled: params !== null,
    // An authorization request is single-use and its context is not worth
    // re-reading behind the user's back while they read the screen.
    retry: false,
    refetchOnWindowFocus: false,
    staleTime: Infinity,
    queryFn: async (): Promise<ConsentContext> => {
      if (params === null) {
        // Unreachable while `enabled` is false, and a throw rather than a
        // fabricated empty context if that ever stops being true.
        throw new Error("No authorization request to load.");
      }
      const qs = new URLSearchParams();
      qs.set("response_type", params.response_type);
      qs.set("client_id", params.client_id);
      qs.set("redirect_uri", params.redirect_uri);
      qs.set("scope", params.scope);
      if (params.state) qs.set("state", params.state);
      if (params.code_challenge) qs.set("code_challenge", params.code_challenge);
      if (params.code_challenge_method) {
        qs.set("code_challenge_method", params.code_challenge_method);
      }

      const res = await fetch(`${CONSENT_AUTHORIZE_PATH}?${qs.toString()}`, {
        method: "GET",
        credentials: "include",
        headers: { Accept: "application/json" },
      });
      if (!res.ok) throw await readOAuthError(res);
      // A 200 with a body that is not the promised shape throws out of
      // parseConsentContext. That is deliberate: see consent-context.ts.
      return parseConsentContext((await res.json()) as unknown);
    },
  });
}

export interface ApproveInput {
  readonly consent: ConsentContext;
  readonly name: string;
  readonly siteScopeMode: SiteScopeMode;
  readonly scopeTagIds: readonly string[];
  readonly scopeSiteIds: readonly string[];
}

export interface ApproveResult {
  readonly grantId: string;
  readonly code: string;
  readonly redirectUri: string;
  readonly state: string | null;
}

/**
 * Record the operator's approval and receive the authorization code to hand
 * back to the client.
 *
 * The response's `redirect_uri` is the one the SERVER exact-matched against the
 * client's registered array, and it is what the caller must navigate to -- not
 * the value that arrived in the browser's query string. Using the echoed
 * request value would make this screen the open redirector that the exact match
 * exists to prevent.
 */
export function useApproveConsent(): UseMutationResult<ApproveResult, Error, ApproveInput> {
  return useMutation({
    mutationFn: async (input: ApproveInput): Promise<ApproveResult> => {
      const res = await fetch(CONSENT_APPROVE_PATH, {
        method: "POST",
        credentials: "include",
        headers: { "Content-Type": "application/json", Accept: "application/json" },
        body: JSON.stringify({
          client_id: input.consent.clientId,
          redirect_uri: input.consent.redirectUri,
          scopes: input.consent.scopes,
          state: input.consent.state ?? "",
          code_challenge: input.consent.codeChallenge ?? "",
          code_challenge_method: input.consent.codeChallengeMethod ?? "",
          name: input.name,
          site_scope_mode: input.siteScopeMode,
          scope_tag_ids: input.scopeTagIds,
          scope_site_ids: input.scopeSiteIds,
        }),
      });
      if (!res.ok) throw await readOAuthError(res);
      const body = (await res.json()) as Record<string, unknown>;
      const grantId = typeof body.grant_id === "string" ? body.grant_id : "";
      const code = typeof body.code === "string" ? body.code : "";
      const redirectUri = typeof body.redirect_uri === "string" ? body.redirect_uri : "";
      // A 200 that did not carry a code or a destination is a failure, not an
      // approval. Handing the browser an empty destination would strand the
      // user on a blank page having already created a live grant.
      if (code === "" || redirectUri === "") {
        throw new OAuthRequestError(
          "server_error",
          "The connection was approved but the server did not return a destination to send the client back to.",
          res.status,
        );
      }
      return {
        grantId,
        code,
        redirectUri,
        state: typeof body.state === "string" && body.state.length > 0 ? body.state : null,
      };
    },
  });
}

/**
 * Build the destination to hand control back to the client.
 *
 * Uses the server-returned redirect_uri only. `state` is echoed when the server
 * returned one and omitted when it did not -- never invented, because a client
 * that sent no state and receives one has been handed a value it will not
 * recognise.
 */
export function buildRedirectTarget(result: ApproveResult): string {
  const url = new URL(result.redirectUri);
  url.searchParams.set("code", result.code);
  if (result.state !== null) url.searchParams.set("state", result.state);
  return url.toString();
}
