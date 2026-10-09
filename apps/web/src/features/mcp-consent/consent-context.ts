import { z } from "zod";

import { capabilityLabel } from "@/features/ai-connections/capabilities";
import {
  conferrableReadsIn,
  defaultCapabilities,
} from "@/features/ai-connections/capability-presets";

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

  // AN OPAQUE SEALED TOKEN. READ IT, HOLD IT, SEND IT BACK. NOTHING ELSE.
  //
  // The consent flow round-trips through the browser, so every authorize-time
  // fact used to come back as caller input on the approval POST and the server
  // stored what the body said -- including the scope set the operator was
  // actually shown. The ticket is the server sealing those facts at authorize
  // time and signing them, so the approval is recorded against what was on the
  // screen rather than against what the POST claims was on the screen.
  //
  // Typed as a bare string, deliberately, and never parsed. Only the server
  // holds the key. Any structure this file imagined it could see would be
  // structure it had decoded without verifying, which is the same mistake as
  // trusting the body -- one indirection further along.
  //
  // OPTIONAL ON THE WAY IN, AND THAT IS A DEPLOY-ORDERING FACT, NOT A DEFAULT.
  // Today's deployed API does not issue one; the API change that always issues
  // it is still in flight. Required here would mean this dashboard refuses
  // every consent screen the moment it ships and until the API catches up,
  // which is the fail-closed direction pointed at our own users rather than at
  // an attacker. Optional lets this land first, which is the only safe order:
  // the reverse -- API first -- breaks the screen, because the server would
  // demand a field the shipped dashboard cannot send.
  //
  // It is NOT optional in the sense the other optionals here are. There is no
  // branch below that renders differently without it and no sentence on the
  // screen that depends on it. It is cargo.
  consent_ticket: z.string().optional(),

  // Mirrors consentResponseDTO.ConferrableCapabilities (dto.go:105-116): every
  // capability the requested scopes confer, each with its effect ("read" or
  // "request"). OPTIONAL on the way in for the same deploy-ordering reason as
  // consent_ticket above -- an empty array is a real, renderable answer
  // ("this scope set confers nothing"), so `[]` is the default rather than
  // `undefined`, and only a genuinely absent key falls back to it.
  //
  // `effect` is parsed as a bare string, not an enum: design v7 S2.2 requires
  // an UNKNOWN effect to disable Approve, which is a fail-closed *render*
  // decision (allCapabilityEffectsKnown, below), not a parse failure. Refusing
  // to parse here would make an unrecognised effect look identical to a
  // malformed payload, when the honest read is "the server named a capability
  // this dashboard does not yet know how to describe" -- disable Approve, but
  // still show the rest of the screen truthfully.
  conferrable_capabilities: z
    .array(z.object({ name: z.string().min(1), effect: z.string().min(1) }))
    .optional(),

  // Mirrors consentResponseDTO.UnregisteredScopes (dto.go:112-119): the scopes
  // the client asked for that its own registration does not hold, in the order
  // it asked. The server leaves them out of `scopes`, out of the ticket and off
  // the screen's ticks; this list exists only so the screen can say why an app
  // that asked for site tools is being offered none.
  //
  // OPTIONAL ON THE WAY IN, for the same deploy-ordering reason as
  // conferrable_capabilities above: a server that predates the field sends no
  // key, and an absent key means nothing was withheld. When the key IS present
  // it is held to the server's own promise (an array of non-empty strings, `[]`
  // when nothing was withheld, never null), so a value of any other type is a
  // failed load rather than a guess.
  //
  // COPY ONLY, NEVER AUTHORITY. Nothing here reaches the approval POST, the
  // capability list or an Approve gate. The scopes that can be approved are
  // `scopes`, sealed in the ticket; this list can neither add to them nor
  // remove from them.
  unregistered_scopes: z.array(z.string().min(1)).optional(),
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

  /**
   * The server's sealed record of this authorization request, to be handed
   * back on the approval POST unread and unaltered.
   *
   * OPAQUE. Do not parse it, validate it, truncate it for display, or put it
   * anywhere a human will see it. It is signed, so one altered character is a
   * failed signature check, and that failure surfaces as a generic OAuth
   * rejection with nothing in it pointing back at whoever normalised the
   * string. Read the wire schema's note before touching this.
   *
   * `null` means the server sent none, which today is every server. See the
   * wire schema for why that case is tolerated rather than refused.
   */
  readonly consentTicket: string | null;

  /** See the wire schema's note. `[]` for both "confers nothing" and "the
   *  server did not send this key yet" -- there is no sentence on this screen
   *  that needs to tell those two apart. */
  readonly conferrableCapabilities: readonly ConferrableCapability[];

  /**
   * What the client asked for and its own registration does not hold, in the
   * order it asked. `[]` for both "nothing was withheld" and "the server did
   * not send this key yet"; no sentence on this screen needs to tell those two
   * apart.
   *
   * Never on `scopes`, never behind a tick, never in the approval. A scope
   * named here cannot be granted from this screen: the client can ask for it
   * only by registering again. The screen reads this to explain that, and for
   * nothing else.
   */
  readonly unregisteredScopes: readonly string[];
}

export interface ConferrableCapability {
  readonly name: string;
  readonly effect: string;
}

/** The one effect this dashboard can describe as an outright grant. */
export const CAPABILITY_EFFECT_READ = "read";
/** The one effect this dashboard describes as "asks, never runs by itself"
 *  -- mcp.cache.purge's effect, per policy.go's EffectRequest. */
export const CAPABILITY_EFFECT_REQUEST = "request";

const KNOWN_CAPABILITY_EFFECTS: ReadonlySet<string> = new Set([
  CAPABILITY_EFFECT_READ,
  CAPABILITY_EFFECT_REQUEST,
]);

/**
 * False the moment any conferrable capability names an effect this dashboard
 * does not know how to describe truthfully. Design v7 S2.2: "An unknown
 * effect disables Approve" -- an operator must never be asked to approve a
 * capability this screen cannot tell them the honest consequence of.
 */
export function allCapabilityEffectsKnown(caps: readonly ConferrableCapability[]): boolean {
  return caps.every((c) => KNOWN_CAPABILITY_EFFECTS.has(c.effect));
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
    // orNull, not a trim and not a re-encode. It maps an absent key and Go's
    // empty-string zero value onto the same `null` -- both are "the server sent
    // no ticket" -- and returns every other value as the identical string
    // reference it arrived as. A non-empty ticket is not touched here, which is
    // the whole requirement.
    consentTicket: orNull(wire.consent_ticket),
    conferrableCapabilities: wire.conferrable_capabilities ?? [],
    unregisteredScopes: wire.unregistered_scopes ?? [],
  };
}

// ---------------------------------------------------------------------------
// Scope vocabulary
// ---------------------------------------------------------------------------

// recognisedScopes in apps/api/internal/mcp/scope.go holds two entries
// (model.go:35, :46): ScopeRead ("mcp:read"), the fleet-read surface, and
// ScopeCache ("mcp:cache"), seated by m150, which confers CapCachePurge and
// nothing else (policy.go's scopeCapabilities). Granting mcp:read changes
// nothing on a site; granting mcp:cache lets the connection ASK to clear one
// -- every clear still waits on a person approving that one request in WPMgr
// (ADR-061 option B, "no automation may ever approve").
export const SCOPE_READ = "mcp:read";
export const SCOPE_CACHE = "mcp:cache";
export const SCOPE_SITE = "mcp:site";

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
 *
 * SCOPE_CACHE is deliberately absent from this function's non-fallback branch.
 * It is a recognised scope (see allScopesRecognised) but it is never rendered
 * as one of these generic bullets: design v7 S2.2 gives it its own bordered
 * box, shared verbatim with the wizard's step 4 (CachePurgeCapabilityBox), so
 * that the one write permission in this vocabulary is never described twice by
 * two different components that could drift apart. The consent screen filters
 * SCOPE_CACHE out before calling this, the same way PermissionsBlock does.
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

/** True when every requested scope is one this screen can describe truthfully,
 *  whether by describeScope's own bullet (SCOPE_READ) or by its own dedicated
 *  section (SCOPE_CACHE). */
export function allScopesRecognised(scopes: readonly string[]): boolean {
  return scopes.every((s) => s === SCOPE_READ || s === SCOPE_CACHE || s === SCOPE_SITE);
}

/**
 * The reads the server offered that this build can confer: the names the read
 * picker may show as tickable. Only the server decides what is offered, so a
 * name it did not list is never in this set, and a name this build does not
 * know (CONFERRABLE_READS is the reads-only list) is left out even when the
 * server lists it with the read effect. In vocabulary order, once each.
 */
export function offeredReads(conferrable: readonly ConferrableCapability[]): readonly string[] {
  return conferrableReadsIn(
    conferrable.filter((c) => c.effect === CAPABILITY_EFFECT_READ).map((c) => c.name),
  );
}

/**
 * True when the app asked for site tools (mcp:site). This one predicate decides
 * both whether the site-tools box is shown and whether its choices open ticked,
 * so the box can never appear without the opening ticks that go with it, nor the
 * other way round.
 */
export function asksForSiteTools(scopes: readonly string[]): boolean {
  return scopes.includes(SCOPE_SITE);
}

/**
 * True when the app asks for more than reading: site tools (mcp:site) or the
 * cache clear (mcp:cache). This describes the app's request, not what is ticked,
 * so it does not change as the person ticks and clears boxes.
 */
export function asksBeyondReading(scopes: readonly string[]): boolean {
  return asksForSiteTools(scopes) || scopes.includes(SCOPE_CACHE);
}

/**
 * The request capabilities among `names`, in order: the ones the server offered
 * with the request effect. A request capability only ever creates a request that
 * a person approves in WPMgr; it is the only kind of capability that is not a
 * read.
 */
export function requestCapabilitiesIn(
  conferrable: readonly ConferrableCapability[],
  names: readonly string[],
): readonly string[] {
  return names.filter((name) =>
    conferrable.some((c) => c.name === name && c.effect === CAPABILITY_EFFECT_REQUEST),
  );
}

// What each request capability lets the connection ask for, as it reads inside a
// sentence. Matches the labels the boxes show.
const ASK_PHRASES: Readonly<Record<string, string>> = {
  "mcp.cache.purge": "clear the site cache",
  "mcp.ability.request": "make changes through the site's tools",
};

/**
 * The closing sentence of "It cannot change anything.", worded from the request
 * capabilities the approval will carry. It says "read-only" only when there are
 * none, and otherwise names each one as something that only creates a request a
 * person approves. Driven by what will be sent, never by what the app asked for,
 * so a box the person cleared stops being named.
 */
export function describeChangeLimit(asks: readonly string[]): string {
  if (asks.length === 0) return "This connection is read-only.";
  const things = asks.map((cap) => `ask to ${ASK_PHRASES[cap] ?? capabilityLabel(cap)}`);
  const list =
    things.length === 1
      ? things[0]!
      : `${things.slice(0, -1).join(", ")} and ${things[things.length - 1]!}`;
  return things.length === 1
    ? `Beyond reading, the only thing it can do is ${list}. That only creates a request, and nothing runs until a person approves it in WPMgr.`
    : `Beyond reading, the only things it can do are ${list}. Each only creates a request, and nothing runs until a person approves it in WPMgr.`;
}

// The two site-tools choices, each with the effect the server must offer it
// under. A name offered with a different effect is not the capability this
// screen describes, so it is not ticked and is not sent (the same test
// buildApprovalCapabilities applies).
const SITE_TOOLS: readonly (readonly [name: string, effect: string])[] = [
  ["mcp.ability.read", CAPABILITY_EFFECT_READ],
  ["mcp.ability.request", CAPABILITY_EFFECT_REQUEST],
];

/**
 * What the consent screen opens with ticked.
 *
 *   - the reads of the wizard's default preset, limited to the reads the server
 *     offered (Sites alone), exactly as before;
 *   - when the app asked for site tools (mcp:site), the two site-tools choices
 *     as well, each only if the server offers it, and "ask for changes" only
 *     beside "see what the site can do" because it needs it. Asking for the
 *     scope is asking for both, and the person can clear either one before
 *     approving.
 *
 * The cache-clear choice is never in this list, whatever was asked.
 *
 * It is a plain function of the context, called once when the screen mounts
 * (see ConsentScreen), so the ticks come from the context the person is
 * looking at and a later re-render cannot put back a tick they cleared.
 */
export function initialSelection(
  consent: Pick<ConsentContext, "scopes" | "conferrableCapabilities">,
): readonly string[] {
  const reads = defaultCapabilities(offeredReads(consent.conferrableCapabilities));
  if (!asksForSiteTools(consent.scopes)) return reads;
  const offered = SITE_TOOLS.filter(([name, effect]) =>
    consent.conferrableCapabilities.some((c) => c.name === name && c.effect === effect),
  ).map(([name]) => name);
  // The request is never ticked without the read it needs (nextAbilityTicks), so
  // an offer of the request alone opens nothing ticked.
  const siteTools = offered.includes("mcp.ability.read") ? offered : [];
  return [...reads, ...siteTools];
}

/**
 * The capability list an approval sends: EXACTLY what the operator ticked,
 * limited to what the server offered. It never adds a name that is not ticked.
 *
 *   - a read is sent when it is ticked and in offeredReads;
 *   - mcp.cache.purge is sent when it is ticked, offered as a request, and the
 *     scopes ask for mcp:cache;
 *   - mcp.ability.read and mcp.ability.request are sent when they are ticked,
 *     offered with their own effect, and the scopes ask for mcp:site;
 *   - mcp.ability.request is sent only together with mcp.ability.read. A
 *     connection holding the request alone cannot call the tool that carries
 *     one, so a request ticked without a read that is itself sent is dropped.
 *     The site-tools box already keeps the two together; this is the same rule
 *     held again where the request is built.
 *
 * `consent` supplies what was offered and what the scopes ask for, so a name is
 * only ever sent for a box that is on the screen: a tick the screen still holds
 * after a refreshed context narrowed the scopes cannot reach the request.
 *
 * `selected` is the whole tick list of the screen. A name in it that the server
 * did not offer, or offered with a different effect, is dropped here rather
 * than sent, so a tick left over from a different request can never widen what
 * is approved. An empty result is returned as empty; the caller omits the key
 * rather than send `[]`, which the server refuses.
 */
export function buildApprovalCapabilities(
  consent: Pick<ConsentContext, "conferrableCapabilities" | "scopes">,
  selected: readonly string[],
): string[] {
  const conferrable = consent.conferrableCapabilities;
  const ticked: ReadonlySet<string> = new Set(selected);
  const out = offeredReads(conferrable).filter((name) => ticked.has(name));
  const cacheAsked = consent.scopes.includes(SCOPE_CACHE);
  const siteToolsAsked = asksForSiteTools(consent.scopes);
  const askable: readonly (readonly [name: string, effect: string, asked: boolean])[] = [
    ["mcp.cache.purge", CAPABILITY_EFFECT_REQUEST, cacheAsked],
    ["mcp.ability.read", CAPABILITY_EFFECT_READ, siteToolsAsked],
    ["mcp.ability.request", CAPABILITY_EFFECT_REQUEST, siteToolsAsked],
  ];
  for (const [name, effect, asked] of askable) {
    if (!asked || !ticked.has(name)) continue;
    if (!conferrable.some((c) => c.name === name && c.effect === effect)) continue;
    // askable lists the read before the request, so `out` already holds the read
    // here exactly when the read is itself being sent.
    if (name === "mcp.ability.request" && !out.includes("mcp.ability.read")) continue;
    out.push(name);
  }
  return out;
}
