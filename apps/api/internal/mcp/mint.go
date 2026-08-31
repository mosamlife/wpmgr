package mcp

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/mosamlife/wpmgr/apps/api/internal/audit"
	"github.com/mosamlife/wpmgr/apps/api/internal/db/sqlc"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// THE CONNECTION-TOKEN MINT: the documented HEADLESS path, not a fallback.
//
// No MCP client documents a device-code flow and Claude Code cannot run browser
// sign-in non-interactively, so headless, SSH and CI installs have no route
// through /authorize + /consent at all. This endpoint is that route, and the
// wireframes present it as the documented headless path rather than as
// something an operator discovers after browser sign-in fails.
//
// WHAT IT SHARES WITH THE OAUTH PATH, AND WHY THAT IS THE WHOLE DESIGN. The
// credential is built the same way (randomToken(32) -> hashCredential ->
// token_prefix + token_hash), stored in the same table by the same generated
// statement, expires on the same connectionTokenTTL, and hangs off a grant
// created with the same m127 columns and the same mcp.grant.created audit
// event. THE ONLY THING THAT DIFFERS IS HOW THE GRANT IS AUTHORISED INTO
// BEING: consent authorises the OAuth grant, and an authenticated operator's
// own request authorises this one. A parallel insert that reproduced the token
// but skipped the grant path would produce a live credential that no audit row
// explains, which is the state this file is written to make unrepresentable.
// ---------------------------------------------------------------------------

const (
	// MintGlobalPerMin caps connection tokens this process mints per minute
	// across ALL operators. Unspoofable and never evicted; this is the bound.
	//
	// MINTING CREDENTIALS UNTHROTTLED IS HOW ONE STOLEN SESSION BECOMES MANY.
	// A session cookie that is replayed once buys one token; replayed in a loop
	// against an unlimited mint it buys as many long-lived bearer credentials
	// as the attacker cares to store, each surviving the session's own
	// expiry and each needing its own revoke. The bound is what turns that from
	// "unbounded" into "a handful, and the audit log names every one".
	MintGlobalPerMin = 30

	// MintPerUserPerMin caps tokens one operator may mint per minute.
	//
	// Sized to a human: setting up a headless connection is a deliberate act
	// that produces one token, and an operator wiring several CI runners at
	// once is still nowhere near this. It is deliberately far below
	// MintGlobalPerMin so one compromised session cannot consume the process
	// budget and lock every other operator out of the feature -- the same
	// fairness reasoning register_limit.go gives for its per-peer layer.
	MintPerUserPerMin = 5
)

// newMintLimiter builds the mint limiter, REUSING registrationLimiter rather
// than restating its two-layer accounting.
//
// The type is named for its first caller, not for a property that excludes this
// one: it is a global token bucket plus a capacity-bounded per-key bucket, it
// charges nothing on a refused request, and a nil receiver refuses. All four
// are exactly what this endpoint needs, and a second copy would be a second
// place for the "reserve then release" bug register_limit.go documents having
// already fixed once.
//
// THE KEY IS THE OPERATOR'S USER ID, NOT RemoteIP, AND THAT IS THE ONE REAL
// DIFFERENCE. register_limit.go must key on the TCP peer because RFC 7591
// registration is anonymous and every other identity on that request is
// attacker-supplied. This endpoint is behind session auth, so the principal's
// UserID is resolved by the session layer and cannot be set by a header --
// it is a STRICTLY BETTER key than RemoteIP, and it is also the right one:
// the harm being bounded is "one stolen session mints many credentials", and
// that is a per-user quantity. Keying on RemoteIP here would instead collapse
// every operator behind the load balancer into one bucket.
func newMintLimiter() *registrationLimiter {
	return newRegistrationLimiter(MintGlobalPerMin, MintPerUserPerMin)
}

// mintLimiterKey is the per-CALLER bucket key, and the caller is whichever
// credential actually authenticated -- a session user OR an API key.
//
// IT MUST NOT READ UserID DIRECTLY. apikey.PrincipalFor never sets UserID, and
// this route is mounted under Auth.Authenticate() whose Bearer branch returns
// exactly that principal, so uuid.Nil is REACHABLE here and reaching it is the
// ordinary case rather than the exotic one: this is the documented HEADLESS
// path and an API key is its natural caller. Keying on UserID therefore
// collapsed every API-key mint in every tenant, process-wide, into the one
// shared "\x00unattributed" bucket -- one tenant's CI runner could spend the
// 5/min budget belonging to every other tenant's keys. Direction is fail-closed
// so that was availability only, but the per-caller layer is the FAIRNESS
// layer, and it degraded to nothing for exactly the caller class this endpoint
// was built for.
//
// Principal.ActorID() is the only correct source (it returns APIKeyID for an
// API-key principal, UserID otherwise), and the principal TYPE is prefixed so
// the two id spaces cannot alias.
//
// A principal that names neither still gets a single shared reserved bucket
// rather than a fresh one, for the reason registrationLimiter.allow gives the
// empty peer string: "we could not identify who is asking" must be MORE
// restrictive than identifying them, never less.
func mintLimiterKey(p domain.Principal) string {
	id := p.ActorID()
	if id == "" || id == uuid.Nil.String() {
		return "\x00unattributed"
	}
	return string(p.Type) + ":" + id
}

// ErrCodeMintRateLimited is the refusal for an operator who has exhausted
// either mint budget. It is a NAMED code rather than a bare 429 body so the
// wizard can tell "slow down" from "you may not do this at all" -- the two need
// different screens, and a client that cannot tell them apart shows the wrong
// one.
const ErrCodeMintRateLimited = "mcp_mint_rate_limited"

// ErrCodeUnknownScopeTag is the LOUD refusal for a scope tag id that names no
// tag in this organisation.
//
// IT EXISTS BECAUSE scope_tag_ids IS A uuid[] WITH NO FOREIGN KEY. PostgreSQL
// has no referential integrity over array elements, so the column accepts any
// UUID at all -- another organisation's tag, or a value that never named
// anything. Such a grant stores cleanly, passes every CHECK, and then resolves
// to zero sites forever at request time, where it is INDISTINGUISHABLE from a
// deliberately narrow connection. That is scope silently narrowed to nothing
// without anyone being told, which is the failure this endpoint is most likely
// to produce and the one the operator has no way to diagnose.
//
// Refusing at mint time, naming the offending id, is the only moment the caller
// still has the context to fix it.
const ErrCodeUnknownScopeTag = "mcp_unknown_scope_tag"

// ErrCodeInvalidSetupClient refuses a setup_client whose SPELLING could not be
// compared reliably. It is never returned for a slug the server simply has not
// heard of; see setupClientShape.
const ErrCodeInvalidSetupClient = "mcp_invalid_setup_client"

// setupClientShapeMaxLen mirrors the length half of m128's CHECK.
const setupClientShapeMaxLen = 64

// setupClientShape mirrors mcp_grants_setup_client_shape_check EXACTLY --
// `^[a-z0-9]+(-[a-z0-9]+)*$` -- and deliberately encodes nothing else.
//
// WHY THE API VALIDATES SHAPE AND NOT MEMBERSHIP, which is the decision worth
// arguing rather than asserting. The vocabulary is nine ids and it lives in
// apps/web/src/features/ai-connections/client-table.ts, whose header states the
// intended cost of a new client: "when a client changes, one row changes". Go
// cannot import that file, so a closed server-side list would be a SECOND copy
// of the vocabulary in a second language on a second release cadence -- and the
// failure mode of a stale copy is the wizard rejecting, at the end of a ten-step
// flow, a client its own UI is offering. m128 DECISION 3 refused a closed
// database CHECK for precisely that reason, and re-adding the closure one layer
// up in Go would defeat the migration while leaving its rationale in place.
//
// The three alternatives and why each loses: a generated Go constant from the
// TS table couples the control-plane release to a frontend data edit, which is
// the coupling DECISION 3 was written to remove; a config-driven allowlist moves
// the same staleness into an env var nobody updates; validating nothing lets
// 'Windsurf', 'windsurf ' and 'Windsurf (Desktop)' all land in the column, and
// then S31 renders "None of them was set up for Windsurf" with a matching
// connection sitting in the list -- an absence coerced into a confident answer,
// this project's signature defect.
//
// So the server guarantees the ONE property the screens actually need, which is
// that equality is trustworthy, and stays silent on which slugs exist. An
// unrecognised id is a LEGITIMATE STORED STATE that degrades at the render layer
// to the generic panel -- "a complete path, not a placeholder" in the
// wireframe's own words -- not a 400.
//
// It mirrors the CHECK rather than relaxing it so the refusal is a 400 naming
// the field, not a 23514 surfacing as a 500 from the INSERT. The database stays
// the backstop; this is the diagnosis.
var setupClientShape = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// validateSetupClient refuses a present-but-malformed value and accepts nil.
//
// nil is "the caller never asked" and is always valid -- the consent path and
// every non-wizard caller are entitled to it. A PRESENT value is held to the
// shape, INCLUDING the empty string: "" is a caller that sent the key and put
// nothing in it, which is a malformed claim rather than an absent one, and
// silently rewriting it to NULL would report success for a step-2 choice that
// was never stored.
//
// No trimming, no lowercasing, no repair of any kind. Coercing " Windsurf " into
// "windsurf" would accept a caller that is wrong about the vocabulary and hide
// the bug at the only layer positioned to name it; the operator would then see
// a stored value they never chose. Refuse and say which field.
func validateSetupClient(v *string) error {
	if v == nil {
		return nil
	}
	if len(*v) > setupClientShapeMaxLen || !setupClientShape.MatchString(*v) {
		return domain.Validation(ErrCodeInvalidSetupClient,
			"setup_client must be a lowercase slug of letters, digits and single hyphens")
	}
	return nil
}

// ErrCodeUnknownScopeSite is the same refusal on the site axis. Same column
// shape (uuid[], no foreign key), same silent-narrowing outcome, and it is
// detected differently: a site id is verified by resolving it through the
// audited chokepoint under tenant RLS, which drops every foreign id.
const ErrCodeUnknownScopeSite = "mcp_unknown_scope_site"

// MintConnectionRequest is an authenticated operator asking for a headless
// connection token.
//
// Principal is the WHOLE principal for the reason ApprovalRequest's field doc
// gives: its Scope and AllowedSiteIDs are what route the grant insert to a
// site-scoped transaction, so narrowing this to (TenantID, UserID) would
// disarm the RLS layer beneath while still compiling.
type MintConnectionRequest struct {
	Principal domain.Principal

	// Name is the operator's own label for the connection. Required: an
	// unnamed long-lived credential is one nobody can identify later in order
	// to revoke it.
	Name string

	// SiteScope is the site axis, IN TAG AND SITE IDs.
	//
	// IDs AND NOT NAMES, AND THE CHOICE IS DELIBERATE. dto.go's
	// approvalRequestDTO already puts `scope_tag_ids` on the wire as UUIDs, and
	// the consent screen already holds the name->id map it needs to fill them
	// in. Taking names here would create a SECOND wire vocabulary for the same
	// concept and would reopen questions the id form has already closed --
	// case-sensitivity, duplicate names, and what a rename does to a stored
	// grant. A tag id is stable across a rename; a name is not, and a grant
	// scoped by name would silently change meaning the day somebody renames a
	// tag.
	//
	// The cost is that the caller must resolve names to ids, which the consent
	// screen already does and refuses the submit when the registry is
	// unavailable. What this endpoint adds, and what the consent path does not
	// do, is VERIFY the ids server-side -- see ErrCodeUnknownScopeTag.
	SiteScope SiteScopeRequest

	// Capabilities is the tool axis. EMPTY MEANS THE DEFAULT PRESET --
	// DefaultGrantCapabilities(), which is {mcp.sites.read} -- and it is
	// neither "none" nor "the organisation ceiling".
	//
	// BOTH OF THOSE READINGS ARE WRONG AND THEY ARE WRONG IN OPPOSITE
	// DIRECTIONS. "None" would store an empty set, and an empty stored
	// capability set is a connection that authenticates and then reaches no
	// tool, which Authenticate refuses by name -- a credential that can never
	// work. "The organisation default" was this comment's own wording until the
	// vocabulary widened, and it is the name of OrgDefaultCapabilities, the
	// SEVEN-member ceiling; reading it that way now hands the widest available
	// set to a caller who asked for nothing.
	//
	// A non-empty list is NARROWED against that ceiling, never widened past it
	// -- CapabilitySet.NarrowTo refuses rather than intersects. So the ceiling
	// is reachable BY ASKING, and only by asking.
	Capabilities []Capability

	// SetupClient is the operator's step-2 choice, OPTIONAL, and nil means the
	// caller never asked -- which is stored as NULL and NOT as "generic". See
	// Connection.SetupClient for why those two are not interchangeable.
	//
	// Validated for SHAPE ONLY, never for membership: see validateSetupClient.
	SetupClient *string
}

// MintedConnection is the response. Token is present exactly once, here.
type MintedConnection struct {
	GrantID uuid.UUID

	// Token is the PLAINTEXT bearer credential. It exists in this struct, in
	// the response body it is rendered into, and nowhere else: no column holds
	// it, no log line prints it, and it is never put in a URL or a query
	// string. Only TokenPrefix and the SHA-256 hash are persisted.
	Token string

	// TokenPrefix is the PUBLIC handle -- the schema says outright that it
	// "carries no authentication weight". It is safe to log, display and store,
	// and it is what lets an operator match a token in a config file against a
	// row in the connections list without ever handling the secret.
	TokenPrefix string

	ExpiresAt     time.Time
	SiteScopeMode SiteScopeMode
	Capabilities  []Capability
}

// MintConnection creates a grant and its first connection token in ONE
// transaction, and returns the plaintext once.
//
// THE ORDER OF THE REFUSALS IS THE SECURITY PROPERTY, and it mirrors Exchange:
// everything that can refuse runs before anything is generated, the credential
// is generated in memory only after the last refusal, and the write is a single
// transaction that either lands whole or leaves no trace.
func (s *Service) MintConnection(ctx context.Context, req MintConnectionRequest) (MintedConnection, error) {
	// 1. THE ORG-SCOPE GUARD, restated in the service.
	//
	// A SITE-SCOPED COLLABORATOR MINTING AN ORG-WIDE GRANT WAS A LIVE
	// PRODUCTION DEFECT on the sibling path: /register is unauthenticated, so
	// one self-registered client and one POST to the writing route was the
	// whole exploit -- no consent screen and no operator interaction. The fix
	// there was to gate BOTH consent routes; the lesson here is that this path
	// must carry the same gate or better, at every layer the other one does.
	//
	// It has three, outermost first, and requireOrgScopedPrincipal's own doc
	// explains why none of them is redundant:
	//   1. authz.RequirePermission(PermAPIKeyManage) on the route -- an
	//      orgLevelPerms member, so RequirePermission refuses any
	//      site-constrained principal outright.
	//   2. THIS CALL.
	//   3. mcp_grants_site_scope_insert, the RESTRICTIVE policy keyed on the
	//      app.site_scope GUC that Repo's RunTenantTx sets.
	if err := requireOrgScopedPrincipal(req.Principal); err != nil {
		return MintedConnection{}, err
	}

	// 2. THE STRUCTURAL REFUSALS, WHICH ARE FREE, AND DELIBERATELY SO.
	//
	// Everything in this step is a PURE FUNCTION OF THE CALLER'S OWN REQUEST
	// BODY -- a name that is blank after trimming, and the site-scope shape
	// rules. Nothing here reads the database, so nothing here can answer a
	// question about this organisation's data that the caller could not already
	// answer offline.
	//
	// THEY RUN BEFORE THE CHARGE BECAUSE A TYPO MUST NOT SPEND BUDGET. With the
	// charge first, five malformed requests exhausted the per-caller budget and
	// the sixth -- the one that finally carried a real defect to report -- was
	// answered "slow down" instead of "your name is blank". That is precisely
	// the confusion ErrCodeMintRateLimited exists to prevent: the wizard must be
	// able to tell "slow down" from "you may not do this at all", and a limiter
	// charged ahead of validation makes the wrong one of those two the answer to
	// the other's question.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return MintedConnection{}, domain.Validation(ErrCodeInvalidRequest,
			"a connection name is required")
	}

	// ValidateSiteScopeRequest is the Go mirror of
	// mcp_grants_site_scope_payload_check and it is reused verbatim: mode 'all'
	// names nothing, mode 'tags' names at least one tag, mode 'list' names at
	// least one site, and an absent mode is refused rather than read as 'all'.
	// An empty allowlist is never a way to ask for every site.
	if err := ValidateSiteScopeRequest(req.SiteScope); err != nil {
		return MintedConnection{}, err
	}

	// validateSetupClient is the Go mirror of
	// mcp_grants_setup_client_shape_check, and it belongs in THIS step for the
	// reason the step's own comment gives: it is a pure function of the request
	// body, it reads no database, and it discloses nothing. Running it before
	// the charge means a malformed slug is answered "your setup_client is
	// malformed" rather than "slow down".
	//
	// It is in the SERVICE and not only in the handler on purpose. The handler
	// is one caller; the invariant that a stored setup_client is comparable by
	// equality is a property of the column, so it is enforced at the layer
	// every caller passes through. A handler-only check is one new call site
	// away from a row S31's filter cannot match.
	if err := validateSetupClient(req.SetupClient); err != nil {
		return MintedConnection{}, err
	}

	// 3. THE RATE LIMIT, charged on the last line before anything reads tenant
	// data, and keyed on the CALLER (see mintLimiterKey).
	//
	// THIS PLACEMENT IS THE ORACLE BOUND, and it is the reason the charge did
	// not simply move to the end. Step 4 below resolves scope ids against this
	// organisation's tag registry and site table and names the offending id in
	// its refusal, which makes it the only refusal on this endpoint that
	// DISCLOSES ANYTHING -- "this uuid does or does not name a tag here". Every
	// request that reaches it has therefore already paid, so referent probing
	// stays bounded at MintPerUserPerMin per caller and MintGlobalPerMin
	// process-wide, exactly as before this moved.
	//
	// What is now free is only step 2, whose answers the caller computes from
	// its own payload, and which an anonymous attacker cannot reach at all:
	// the route sits behind Auth.Authenticate() and
	// authz.RequirePermission(PermAPIKeyManage).
	//
	// Charged in the service rather than in the handler so that every mount of
	// this service is limited, including a test harness -- a limiter that exists
	// only in the route registration is one refactor away from being absent,
	// which is the same reasoning Handler.Register gives for putting
	// RequireOrgScope on the group.
	if ok, retryAfter := s.mintLimit.allow(mintLimiterKey(req.Principal)); !ok {
		return MintedConnection{}, domain.RateLimited(ErrCodeMintRateLimited,
			"too many connection tokens minted; retry shortly").
			WithDetails(map[string]any{"retry_after_seconds": retryAfterSeconds(retryAfter)})
	}

	// 4. SITE SCOPE: the REFERENTIAL check, which the structural one cannot do.
	//
	// This is where an id that names nothing is refused. See
	// verifyScopeReferents -- and read its comment before adding any
	// "resolves to no sites" refusal here, because that would be a defect
	// rather than a hardening.
	if err := s.verifyScopeReferents(ctx, req.Principal.TenantID, req.SiteScope); err != nil {
		return MintedConnection{}, err
	}

	// 5. CAPABILITIES: narrowed against the organisation's ceiling, never
	// widened past it. NarrowTo REFUSES a capability the ceiling does not hold
	// rather than dropping it, for the reason written on that method: a
	// silently-dropped capability is a grant the operator did not ask for.
	caps, err := s.resolveMintCapabilities(req.Capabilities)
	if err != nil {
		return MintedConnection{}, err
	}

	// 6. THE CREDENTIAL. Generated only now, after the last refusal, and held
	// in memory alone: the insert takes the PREFIX and the HASH, never this.
	secret, err := randomToken(32)
	if err != nil {
		return MintedConnection{}, fmt.Errorf("generate connection token: %w", err)
	}
	now := s.now().UTC()
	tokenExpiry := now.Add(connectionTokenTTL)

	grant, tok, err := s.store.CreateGrantWithToken(ctx, req.Principal,
		sqlc.CreateMCPGrantParams{
			TenantID:      req.Principal.TenantID,
			Name:          name,
			Status:        string(GrantStatusActive),
			SiteScopeMode: string(req.SiteScope.Mode),
			ScopeTagIds:   orEmpty(req.SiteScope.TagIDs),
			ScopeSiteIds:  orEmpty(req.SiteScope.SiteIDs),

			// NO client_id. A connection token has no RFC 7591 client behind
			// it -- nothing registered, nothing to resolve -- and the column is
			// nullable precisely so this path can say so. Inventing a
			// synthetic client id here would put a registration in the row that
			// no client ever performed, and the connections list renders
			// client identity as the CLIENT's own unverified claim; a
			// fabricated one would be rendered with the same weight as a real
			// one. NULL is the honest answer and it is also the one that makes
			// this path identifiable in the data.
			ClientID: nil,

			// NULL FOR AN API-KEY CALLER, AND THAT IS THE HONEST ANSWER, NOT A
			// GAP. uuidToPG maps uuid.Nil to NULL rather than to a zero uuid
			// that looks like a real author, and an API-key mint genuinely has
			// no user behind it. Do not "fix" this by inventing one: the actor
			// is recorded on the audit row, where an api_key actor_type has a
			// name join of its own (see audit.ActorFor), and this column means
			// "the human who created it" -- an answer of "none" beats an answer
			// naming a user that does not exist.
			CreatedByUserID: uuidToPG(req.Principal.UserID),

			// m127's columns, ALL SUPPLIED EXPLICITLY, exactly as Approve
			// supplies them. Two are NOT NULL with no default so a forgotten
			// field is 23502 at the INSERT rather than an unrestricted or
			// never-expiring connection.
			Capabilities: capabilityNames(caps.Sorted()),
			ExpiresAt:    now.Add(grantAbsoluteTTL),

			// NULL means "never idle-expire" (m127 DECISION 4), and NULL is the
			// answer rather than a placeholder. Nothing asks the operator for a
			// window yet, and inventing one nobody chose is the credential-terms
			// defect that column refuses to default.
			IdleExpireAfterDays: nil,

			// m128. THE OPERATOR'S STEP-2 CHOICE, PASSED THROUGH UNCHANGED --
			// nil included. This is the one path in the codebase that has a
			// real answer for this column, because it is the one the wizard
			// calls.
			//
			// nil here is a caller that did not ask, and it stores NULL. It is
			// NOT rewritten to "generic": that would assert the operator saw
			// nine cards and chose "Other MCP client", which is a different
			// fact and the one S29 step 9 exists to distinguish. Already
			// validated for shape in step 2; the database CHECK is the backstop
			// behind that, not the diagnosis.
			SetupClient: req.SetupClient,
		},
		func(grantID uuid.UUID) sqlc.CreateMCPConnectionTokenParams {
			return sqlc.CreateMCPConnectionTokenParams{
				TenantID: req.Principal.TenantID,
				GrantID:  grantID,
				// The PUBLIC handle and the HASH. hashCredential is the one
				// construction this package uses -- lower-case hex SHA-256,
				// matching the '^[0-9a-f]{64}$' CHECK and internal/apikey --
				// and there is deliberately no second one.
				TokenPrefix: secret[:tokenPrefixLen],
				TokenHash:   hashCredential(secret),
				Status:      string(GrantStatusActive),
				ExpiresAt:   timestamptzAt(tokenExpiry),
			}
		},
		// ActionMCPGrantCreated, in the SAME transaction as both inserts. If
		// either insert or this append fails, the whole mint rolls back and
		// there is no audit row for a grant that does not exist -- and,
		// equally, no grant without the row that explains it.
		func(tx pgx.Tx, gr sqlc.McpGrant) error {
			if s.audit == nil {
				return nil
			}
			// THE ACTOR IS WHICHEVER CREDENTIAL AUTHENTICATED, resolved by
			// audit.ActorFor rather than hardcoded. This is the headless path,
			// so an API key is its natural caller and ActorUser over
			// Principal.UserID named a user that does not exist.
			actorType, actorID := audit.ActorFor(req.Principal)
			_, aerr := s.audit.RecordInTx(ctx, tx, audit.Event{
				TenantID:   req.Principal.TenantID,
				ActorType:  actorType,
				ActorID:    actorID,
				Action:     audit.ActionMCPGrantCreated,
				TargetType: "mcp_grant",
				TargetID:   gr.ID.String(),
				Metadata: map[string]any{
					"grant_name":      gr.Name,
					"site_scope_mode": gr.SiteScopeMode,
					// HOW the grant came to exist. The OAuth path and this one
					// produce the same row shape, so without this an auditor
					// cannot tell a browser-consented connection from a
					// headless-minted one -- and they are authorised by
					// different things, which is exactly what an audit log is
					// for.
					"issuance": "connection_token",
					// The PUBLIC handle only. It carries no authentication
					// weight, and it is what lets an operator match this audit
					// row to a token in a config file. The plaintext is not
					// here and must never be.
					"token_prefix": secret[:tokenPrefixLen],
				},
			})
			return aerr
		})
	if err != nil {
		return MintedConnection{}, fmt.Errorf("mint mcp connection: %w", err)
	}

	return MintedConnection{
		GrantID:       grant.ID,
		Token:         secret, // once, here, never again
		TokenPrefix:   tok.TokenPrefix,
		ExpiresAt:     tokenExpiry,
		SiteScopeMode: req.SiteScope.Mode,
		Capabilities:  caps.Sorted(),
	}, nil
}

// resolveMintCapabilities turns the operator's requested capability list into
// the set that will be stored.
//
// AN EMPTY REQUEST MEANS THE DEFAULT PRESET, NOT AN EMPTY SET AND NOT THE
// CEILING, and the difference is the whole function. mcp_grants.capabilities admits '{}' -- the
// shape CHECK passes it, because '{}' is the RESTRICTIVE value -- so an empty
// set is perfectly storable, and Authenticate then refuses the connection by
// name on every request ("this connection holds no capability, so it can reach
// no tool"). Writing one would therefore mint a credential that authenticates
// and can never do anything, which m127 DECISION 3 says this boundary must not
// be able to produce. The operator asking for nothing on the tool axis is an
// ABSENCE, and absence here means "you did not narrow", not "narrow to zero".
//
// Note the asymmetry with the SITE axis and that it is deliberate: an empty
// site scope is a coherent connection (it reads no sites, and may read some
// tomorrow), whereas an empty capability set is a connection that cannot
// function at all. The two axes are not symmetric because their empty values
// mean different things.
//
// THE ABSENT REQUEST TAKES DefaultGrantCapabilities(), NOT THE CEILING, AND THE
// TWO STOPPED BEING THE SAME THING WHEN THE VOCABULARY WIDENED. `return
// ceiling, nil` here was correct and safe for exactly as long as the ceiling
// held one member; against a seven-member ceiling it is the SECOND COPY of the
// widening m131's note warns about, one function over from
// DefaultGrantCapabilities and reached by the mint endpoint rather than by the
// consent screen. An operator who POSTs no capability list has chosen nothing,
// and the answer to "nobody asked" is the preset, never the widest set
// available. An operator who wants more asks for it on the line below, and
// asking is choosing.
func (s *Service) resolveMintCapabilities(requested []Capability) (CapabilitySet, error) {
	ceiling, err := OrgDefaultCapabilities(grantScopes())
	if err != nil {
		return CapabilitySet{}, fmt.Errorf("resolve organisation capabilities: %w", err)
	}
	if len(requested) == 0 {
		// Still routed through NarrowTo rather than returned directly, so the
		// preset is subject to the same ceiling every explicit request is. A
		// preset that named a capability no scope confers would be a refusal
		// here, loudly, rather than a stored set Authenticate refuses later on
		// every request of a credential the operator is already holding.
		return ceiling.NarrowTo(DefaultGrantCapabilities())
	}
	// NarrowTo REFUSES anything the ceiling does not hold rather than dropping
	// it. Dropping would be fail-closed and still wrong: the operator would be
	// told they minted a connection with capabilities it does not have.
	return ceiling.NarrowTo(requested)
}

// verifyScopeReferents refuses a scope payload naming an id that resolves to no
// row in THIS organisation.
//
// WHY IT IS NOT "REFUSE IF THE SCOPE RESOLVES TO NO SITES". Those are two
// different questions and conflating them breaks a rule the owner ruled on
// directly. A grant scoped to a real tag that currently carries zero sites is
// LEGITIMATE: it reads nothing today, it reads whatever gets tagged tomorrow,
// and it must be ACCEPTED AND STORED AS GIVEN -- never refused, and above all
// never widened to mode 'all' on the grounds that empty "must have been a
// mistake". An empty allowlist is never every site, in either direction.
//
// What is NOT legitimate is an id that names nothing at all, because
// scope_tag_ids and scope_site_ids are uuid[] columns with no foreign key and
// therefore accept any UUID. That grant stores cleanly and then resolves to
// zero sites FOREVER, indistinguishable from the legitimate case above. So:
//
//	real tag, zero sites today  -> accepted, stored as given   (empty scope)
//	id naming no tag at all     -> refused, loudly, by id      (typo/foreign id)
//
// Distinguishing them is what makes the empty-scope acceptance safe. Without
// this check, "accept empty" would also silently accept a typo.
//
func (s *Service) verifyScopeReferents(ctx context.Context, tenantID uuid.UUID, req SiteScopeRequest) error {
	switch req.Mode {
	case SiteScopeModeTags:
		// The tenant's tag registry, read under tenant RLS. Tags are a small,
		// unpaginated per-tenant set by construction (see the query's own note),
		// so reading the registry and comparing in Go costs one round trip and
		// needs no new statement.
		known, err := s.store.ListTagIDs(ctx, tenantID)
		if err != nil {
			// A FAILED READ IS NOT AN EMPTY REGISTRY. Proceeding on an error
			// here would refuse every tag as unknown, or -- worse, if the
			// comparison were ever inverted -- accept every tag unchecked.
			// Neither is an answer, so this is an infra error and the mint
			// stops.
			return fmt.Errorf("read tag registry for scope verification: %w", err)
		}
		for _, want := range req.TagIDs {
			if !slices.Contains(known, want) {
				return domain.Validation(ErrCodeUnknownScopeTag,
					fmt.Sprintf("scope tag %s does not exist in this organisation", want)).
					WithDetails(map[string]any{"unknown_tag_id": want.String()})
			}
		}
		return nil

	case SiteScopeModeList:
		// Site ids are verified through the AUDITED CHOKEPOINT rather than a
		// second query: ResolveScopeSites runs InTenantTx and joins through
		// `sites`, so tenant RLS drops every foreign or non-existent id. Under
		// mode 'list' the resolution is exactly the subset of the requested ids
		// that exist here, so anything missing from the result names nothing.
		//
		// This arm has no empty-scope ambiguity to preserve: a site either
		// exists in this tenant or it does not.
		resolved, err := s.store.ResolveScopeSites(ctx, tenantID,
			string(SiteScopeModeList), nil, req.SiteIDs)
		if err != nil {
			return fmt.Errorf("resolve site scope for verification: %w", err)
		}
		for _, want := range req.SiteIDs {
			if !slices.Contains(resolved, want) {
				return domain.Validation(ErrCodeUnknownScopeSite,
					fmt.Sprintf("scope site %s does not exist in this organisation", want)).
					WithDetails(map[string]any{"unknown_site_id": want.String()})
			}
		}
		return nil

	default:
		// Mode 'all' names no ids, so there is nothing to verify. Any other
		// value never reaches here: ValidateSiteScopeRequest refused it above.
		return nil
	}
}

// retryAfterSeconds renders a limiter wait as whole seconds, never below one.
// A "retry after 0 seconds" tells a client to retry immediately into the same
// refusal, which is a busy loop wearing the shape of advice.
func retryAfterSeconds(d time.Duration) int {
	secs := int(d.Seconds())
	if secs < 1 {
		return 1
	}
	return secs
}
