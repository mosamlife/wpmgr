import { z } from "zod";

// The consent screen's data model (ADR-064 S6b, design Step 7).
//
// WHY THIS FILE EXISTS SEPARATELY FROM THE SCREEN
//
// m124 obligation 7 (apps/api/migrations/20260826000000_m124_mcp_connection_surface.sql
// lines 548-575) is a presentational obligation that no schema constraint can
// discharge. RFC 7591 dynamic client registration is UNAUTHENTICATED, so
// `client_name` and `client_uri` are attacker-controlled strings. The unique
// index on mcp_oauth_clients is on client_id alone, so two registrations may
// both call themselves "Claude Desktop" with identical redirect URIs and store
// cleanly -- the review proved it. The database cannot tell them apart and a
// uniqueness constraint on client_name would break the legitimate second
// registration while an attacker simply picks a different string.
//
// So the defence is: (a) present the self-declared identity as self-declared,
// (b) show the redirect destination, which IS verified server-side by exact
// match against the stored array, with at least equal prominence, and (c) fail
// closed when the payload is anything other than the shape the server promises.
//
// The types below make (a) and (c) hard to get wrong by accident rather than by
// discipline. A raw `string` for the client name would let any template render
// it as a verified identity; `SelfAsserted` forces the caller to handle the
// "did not state one" branch, and there is no code path that produces a
// verified identity because none exists.

// ---------------------------------------------------------------------------
// Wire shape
// ---------------------------------------------------------------------------

// Mirrors consentResponseDTO in apps/api/internal/mcp/dto.go exactly. The
// `_unverified` suffixes are the API's own marking, carried through rather than
// renamed, so a reader of this file and a reader of that one see the same word.
//
// STRICTNESS IS THE POINT. Every field the screen needs to make a true
// statement is required here. A payload missing `redirect_host` cannot be
// rendered honestly -- the host is the one part of the request a human can
// judge -- so the parse fails and the screen refuses to offer approval, rather
// than rendering a consent screen with a hole in it that a user then approves.
// That is the house defect class (a failure or absence coerced into a plausible
// value) applied to the one screen where the coerced value is authorization.
export const consentWireSchema = z.object({
  client_id: z.string().min(1),

  // Optional on the wire: Go's `json:"client_name_unverified"` emits "" when
  // the registration omitted a name, and an absent key is equally possible.
  // Both are an ABSENCE and are normalised to one below -- never to a blank
  // that renders as an unnamed but apparently legitimate client.
  client_name_unverified: z.string().optional(),
  client_uri_unverified: z.string().optional(),

  // A LITERAL false, not a boolean.
  //
  // dto.go sets this field from a hard-coded `false` with the comment "there is
  // no code path that can set it true. Registration is unauthenticated; nothing
  // about this identity is verified, ever." Typing it as z.boolean() would let
  // a payload asserting `true` parse, and then the only thing standing between
  // that assertion and a verified-looking consent screen would be a template
  // author remembering not to trust it. z.literal(false) makes the assertion
  // unparseable: a payload claiming verification is a failed load, and a failed
  // load is not approvable.
  identity_verified: z.literal(false),

  // Verified server-side by EXACT match against mcp_oauth_clients.redirect_uris
  // -- never a prefix, suffix or host-only comparison, each of which is an open
  // redirector (m124 obligation 7). This is the only identity claim on the
  // screen that we actually stand behind, so it is required, not optional.
  redirect_uri: z.string().min(1),
  redirect_host: z.string().min(1),

  // At least one. ParseRequestedScopes (apps/api/internal/mcp/scope.go) refuses
  // a request naming no recognised scope -- absence is refusal, never
  // "everything we have" -- so an empty array here is an incoherent payload,
  // not a grant of nothing. Failing the parse is the fail-closed direction; the
  // alternative is a screen that says the client may read nothing, over a grant
  // whose real scope we could not read.
  scopes: z.array(z.string().min(1)).min(1),

  state: z.string().optional(),
  code_challenge: z.string().optional(),
  code_challenge_method: z.string().optional(),

  // REQUIRED, for the same reason redirect_host is.
  //
  // The screen has to tell the user how long they are authorising this for.
  // The term is a server constant (grantAbsoluteTTL in
  // apps/api/internal/mcp/service.go), stamped onto mcp_grants.expires_at at
  // approval and enforced on every later request by the `g.expires_at > now()`
  // arm of the authentication lookup (apps/api/db/query/mcp_connections.sql:561).
  // The dashboard cannot derive it and must not assume it: hard-coding 90 here
  // would restate a number only the server owns, and it would go quietly wrong
  // the day the server's value changes.
  //
  // So an absent term is an unrenderable screen, not a screen with a default.
  // A positive integer of days is the only parseable value: 0 would describe a
  // grant that expires the moment it is created, which the schema refuses
  // outright (mcp_grants_expires_at_after_created_check).
  grant_lifetime_days: z.number().int().positive(),
});

export type ConsentWire = z.infer<typeof consentWireSchema>;

// ---------------------------------------------------------------------------
// Self-asserted identity
// ---------------------------------------------------------------------------

/**
 * A string the client supplied about itself during unauthenticated
 * registration. It is attacker-controlled and we vouch for none of it.
 *
 * Modelled as a discriminated union rather than `string | null` so that
 * rendering it requires deciding what an absence says. `{ stated: false }` is
 * not a blank to be interpolated; it is a fact to be reported -- "this client
 * did not give a name" -- which is a different and more useful sentence than an
 * empty space where a name would be.
 */
export type SelfAsserted =
  | { readonly stated: true; readonly value: string }
  | { readonly stated: false };

/**
 * Normalise a registration-supplied string into an explicit presence or
 * absence.
 *
 * Whitespace-only counts as absent: a client_name of " " is not a name, and
 * treating it as one produces a consent screen with an invisible identity,
 * which reads to a user as a rendering glitch rather than as a warning. An
 * attacker choosing that string is choosing it precisely for that effect.
 */
export function asSelfAsserted(raw: string | undefined | null): SelfAsserted {
  if (raw === undefined || raw === null) return { stated: false };
  const trimmed = raw.trim();
  if (trimmed.length === 0) return { stated: false };
  return { stated: true, value: trimmed };
}

// ---------------------------------------------------------------------------
// Domain shape
// ---------------------------------------------------------------------------

export interface ConsentContext {
  readonly clientId: string;

  /**
   * SELF-DECLARED AND UNVERIFIED. Never render this as the answer to "who is
   * asking" without saying, adjacent to it, that we did not verify it. The
   * redirect destination is what we actually checked.
   */
  readonly clientNameUnverified: SelfAsserted;

  /** SELF-DECLARED AND UNVERIFIED, as above. Never linked, only shown. */
  readonly clientUriUnverified: SelfAsserted;

  /**
   * Always false, and typed as the literal so no branch can be written that
   * depends on it being true. Kept on the domain object rather than dropped
   * because its presence is what makes the absence of a verified path legible
   * to the next reader.
   */
  readonly identityVerified: false;

  /** Exact-matched server-side against the client's registered array. */
  readonly redirectUri: string;
  /** The host of the above. What a human can actually judge. */
  readonly redirectHost: string;

  readonly scopes: readonly string[];

  readonly state: string | null;
  readonly codeChallenge: string | null;
  readonly codeChallengeMethod: string | null;

  /**
   * Whole days from approval to automatic expiry, supplied by the server.
   *
   * Not nullable and not optional: every grant this screen can create expires,
   * so there is no "no expiry" case for a caller to render. See the wire
   * schema's note for why the dashboard is told this rather than computing it.
   */
  readonly grantLifetimeDays: number;
}

function orNull(raw: string | undefined): string | null {
  if (raw === undefined) return null;
  return raw.length === 0 ? null : raw;
}

/**
 * Parse an authorize response into the consent screen's model.
 *
 * Throws on any payload that is not exactly what the server promises. The
 * caller is a TanStack Query queryFn, so a throw becomes an error state and the
 * screen renders PageError instead of an approvable form.
 */
export function parseConsentContext(raw: unknown): ConsentContext {
  const wire = consentWireSchema.parse(raw);
  return {
    clientId: wire.client_id,
    clientNameUnverified: asSelfAsserted(wire.client_name_unverified),
    clientUriUnverified: asSelfAsserted(wire.client_uri_unverified),
    identityVerified: false,
    redirectUri: wire.redirect_uri,
    redirectHost: wire.redirect_host,
    scopes: wire.scopes,
    state: orNull(wire.state),
    codeChallenge: orNull(wire.code_challenge),
    codeChallengeMethod: orNull(wire.code_challenge_method),
    grantLifetimeDays: wire.grant_lifetime_days,
  };
}

// ---------------------------------------------------------------------------
// Scope vocabulary
// ---------------------------------------------------------------------------

// recognisedScopes in apps/api/internal/mcp/scope.go holds exactly one entry.
// The read-only surface is the entire security claim of the feature (m124
// obligation 5): the surface is read-only because no write tool is exposed, not
// because a column says so.
export const SCOPE_READ = "mcp:read";

export interface ScopeCopy {
  readonly token: string;
  readonly title: string;
  readonly detail: string;
}

/**
 * Blunt, specific copy for a granted scope -- the design's "Consent-screen
 * candour", and its instruction that blunt beats euphemistic.
 *
 * An UNRECOGNISED scope is described as unrecognised rather than prettified or
 * dropped. Dropping is the tempting behaviour and it is the same mistake
 * ParseRequestedScopes refuses on the request side: it would let the operator
 * consent to a scope set that is not the one the client asked for, and neither
 * party would learn they disagreed.
 */
export function describeScope(token: string): ScopeCopy {
  if (token === SCOPE_READ) {
    return {
      token,
      title: "Read your fleet's data",
      detail:
        "Site names and URLs, WordPress and PHP versions, installed plugins and themes and their versions, update and vulnerability status, uptime and performance history, and backup history.",
    };
  }
  return {
    token,
    title: "An unrecognised permission",
    detail:
      "This dashboard does not recognise this permission and cannot tell you what it allows. Do not approve this connection.",
  };
}

/** True when every requested scope is one this screen can describe truthfully. */
export function allScopesRecognised(scopes: readonly string[]): boolean {
  return scopes.every((s) => s === SCOPE_READ);
}
