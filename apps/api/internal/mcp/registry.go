package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/mosamlife/wpmgr/apps/api/internal/authz"
	"github.com/mosamlife/wpmgr/apps/api/internal/domain"
)

// ---------------------------------------------------------------------------
// S7: THE TOOL REGISTRY.
//
// S7's exit gate: a capability absent from the registry is unreachable by a
// schema-guessing model, and DISCOVERY NEVER GRANTS.
//
// That second clause is the whole structural claim of this file, and it is a
// claim about wiring rather than about any single check. Before S7, tools/list
// returned the closed list and tools/call switched on the tool name with a
// `default` arm -- so the filter lived on the DISCOVERY path and the INVOCATION
// path trusted it to have run. That arrangement is correct only for a client
// that calls tools/list first and believes the answer. A model that guesses a
// plausible name never takes the discovery path at all, so nothing it guesses
// is ever filtered.
//
// The fix is not a second check bolted onto tools/call. It is that the
// INVOCATION path resolves the name through one function over the closed list,
// and that function decides authority for itself:
//
//	tools/list  -> VisibleTools(auth)        -> every entry, annotated
//	tools/call  -> AuthorizeTool(name, auth) -> capabilityHeld(entry, auth)
//
// so nothing tools/list said can make a tool callable. There is no path from a
// tool name to an invocation that does not pass AuthorizeTool; the dispatch
// function itself hangs off the registry entry, so tools/call has no switch and
// therefore no `default` arm to fall through.
//
// tools/list NO LONGER FILTERS. Under the D1 ruling an unticked capability
// refuses rather than hides, so the listing is the whole registry and the
// capability answer is given at invocation, by name. See the D1 note further
// down for the reasoning and for what that does and does not change.
//
// THE LIST STAYS CLOSED AND STAYS A FUNCTION. registryTools() returns a fresh
// slice built from a literal, exactly as Tools() did before this slice, and for
// the same reason: the read-only claim of this feature is "no write tool is
// exposed", and that claim is only as strong as the list being closed. There is
// deliberately NO package-level registry var and NO Register() entry point, so
// no init function anywhere in the binary can append a tool into the surface at
// runtime.
// ---------------------------------------------------------------------------

// toolInvoker runs one tool. It hangs off the registry entry so that adding a
// tool to the registry without wiring an implementation is a nil that
// TestEveryRegistryEntryIsInvocable catches, rather than a name that resolves
// to a `default` arm.
type toolInvoker func(ctx context.Context, svc *Service, auth AuthorizedRequest, args json.RawMessage) (string, error)

// ToolPolicy is one registry entry: the tool, and everything reaching it
// requires.
type ToolPolicy struct {
	// Name is the wire name. Matching is exact and case-sensitive: a model that
	// guesses "list-sites" or "listSites" has guessed a name that is not in the
	// registry, and near-miss normalisation would be exactly the coercion of an
	// absent value into a plausible one that this codebase is governed by.
	Name        string
	Description string
	InputSchema json.RawMessage

	// Capability is what the connection must hold. Zero-value Capability ("")
	// is not in the vocabulary, so an entry that forgets to declare one is
	// unreachable rather than universally reachable.
	Capability Capability

	// OperatorPermission is the authz permission an OPERATOR would need to do
	// this by hand in the dashboard. It is DECLARATIVE ON THIS PATH AND IT IS
	// IMPORTANT TO SAY SO: an MCP request carries a grant, not a user
	// principal, so there is nothing here for authz.RequirePermission to check
	// against and no such check runs. What it does do is (a) pin every tool to
	// a permission that really exists in the control plane's vocabulary, which
	// TestEveryToolDeclaresAKnownOperatorPermission enforces, and (b) land in
	// the operator-facing log on every call, so the audit trail says which
	// dashboard authority the model just exercised.
	//
	// It is a separate field from Capability and not a substitute for it. If it
	// were ever read as the gate, a connection would be granted whatever its
	// creating operator could do, which is precisely the widening this surface
	// exists to prevent.
	OperatorPermission authz.Permission

	// RequiresSiteScope marks a tool that reads site-keyed data. Such a tool is
	// UNCALLABLE by a connection whose resolved SiteSet is empty -- that is the
	// per-connection narrowing on the site axis, and unlike the capability axis
	// it is stored today (mcp_grants.site_scope_mode and its payload).
	//
	// A connection scoped to sites it cannot see is not a connection that reads
	// every site; SiteSet's zero value allows nothing and this flag is what
	// turns that into a NAMED refusal at the registry gate rather than an empty
	// result deeper in that reads as a healthy org owning nothing.
	//
	// It does NOT hide the tool from tools/list, and neither does the
	// capability axis any more. See the D1 note below.
	RequiresSiteScope bool

	invoke toolInvoker
}

// registryTools is THE closed list. Every tool this server has, with everything
// reaching it requires, in one literal.
//
// Adding an entry here is the only way a tool becomes reachable, and it is a
// reviewed diff on a file whose package comment says the surface is read-only.
// A write tool arrives with its own capability, its own migration for the
// stored narrowing, and its own review -- never by being appended here.
func registryTools() []ToolPolicy {
	entries := []ToolPolicy{{
		Name: ToolFleetSitesList,
		Description: "List the WordPress sites this connection may read, with their " +
			"connection state, health, WordPress/PHP/agent versions, and an explicit " +
			"inventory staleness stamp. Sites whose plugin/theme inventory has never " +
			"been collected are reported as never_collected rather than being given a " +
			"substitute date.",
		InputSchema:        listSitesSchema,
		Capability:         CapSitesRead,
		OperatorPermission: authz.PermSiteRead,
		RequiresSiteScope:  true,
		invoke: func(ctx context.Context, svc *Service, auth AuthorizedRequest, _ json.RawMessage) (string, error) {
			return svc.ListSitesForModel(ctx, auth)
		},
	}}

	// EVERY ENTRY GETS ITS OWN COPY OF ITS SCHEMA BYTES.
	//
	// A fresh slice of entries is not enough. json.RawMessage is a []byte, so
	// assigning listSitesSchema into the literal shares one backing array with
	// every entry this function has ever returned and ever will. The closure
	// property was therefore half-built: nobody could add a tool, and anybody
	// holding a returned descriptor could rewrite what an existing tool claims
	// to accept -- for every later caller, including the tools/list of every
	// other connection on the instance.
	//
	// It is the same failure the SiteSet and CapabilitySet reasoning is built
	// around, one level in: the container is safe and the contents leak. The
	// container being immutable is the thing that makes the sharing invisible,
	// which is why it survived a security review.
	for i := range entries {
		entries[i].InputSchema = cloneSchema(entries[i].InputSchema)
	}
	return entries
}

// cloneSchema copies schema bytes so a caller cannot mutate the package's
// copy. A nil schema stays nil rather than becoming an empty non-nil slice:
// "no schema" and "an empty schema" are different, and
// TestRegistryEntriesAreWellFormed refuses the first.
func cloneSchema(s json.RawMessage) json.RawMessage {
	if s == nil {
		return nil
	}
	return append(json.RawMessage(nil), s...)
}

// ---------------------------------------------------------------------------
// The invocation gate
// ---------------------------------------------------------------------------

// refusalReason is the OPERATOR-FACING classification of why a tool was not
// reachable. It is precise, it is logged, and it is NEVER sent to the caller.
// See AuthorizeTool.
type refusalReason string

const (
	// reasonUnregistered: no entry of that name exists. The model guessed.
	reasonUnregistered refusalReason = "unregistered"
	// reasonCapabilityNotHeld: the entry exists and this connection's
	// capability set does not hold what it requires. Since the D1 ruling this
	// one is ALSO disclosed to the caller, by name, as
	// mcp_capability_not_granted -- it is still recorded here because the
	// operator log is what an operator greps when a customer says "the model
	// says it cannot do X".
	reasonCapabilityNotHeld refusalReason = "capability_not_held"
	// reasonSiteScopeEmpty: the entry exists, the capability IS held, and the
	// connection's resolved site scope is empty, so a site-keyed tool has
	// nothing it may read.
	//
	// Unlike the two above this one is also disclosed to the caller, by name,
	// as mcp_scope_empty -- see the asymmetry note on visible(). It still needs
	// a reason constant because the OPERATOR log must carry it: an operator
	// debugging a scope problem got nothing at all before this existed, on
	// either the wire or the log.
	reasonSiteScopeEmpty refusalReason = "site_scope_empty"
)

// ---------------------------------------------------------------------------
// D1 RULING: AN UNTICKED CAPABILITY REFUSES, IT DOES NOT HIDE.
//
// This file used to delete a tool from tools/list when the connection did not
// hold its capability, on the reasoning that such a caller "is not entitled to
// know the tool exists". That bought non-enumerability and paid for it with the
// one thing this surface exists to give a model: a fact it can act on. An
// absent tool is indistinguishable from a tool that was never built, so the
// model's only available reading is "wpmgr cannot do this" -- and the one
// person who could fix it, the operator who ticks the capability on the
// connection, never hears that anything was refused.
//
// The capability axis now behaves like the site axis already did: the tool is
// LISTED, its descriptor says plainly that this connection does not hold what
// it requires, and CALLING it returns a typed, terminal refusal naming the
// capability (ErrCodeCapabilityNotGranted). Enumerability is the accepted cost
// of the ruling, and it is bounded: registryTools() is a closed literal in a
// reviewed file and every entry in it is a read tool.
//
// WHAT DID NOT CHANGE IS THE PART THAT MATTERS. The gate is still on the
// INVOCATION path and it is still the only thing between a name and its invoke
// function. tools/list never granted anything and still does not; AuthorizeTool
// re-derives the capability answer for itself rather than trusting what was
// listed, so a model that guesses a name is refused exactly as before. The
// invariant kept is the one-directional one -- NOTHING IS CALLABLE THAT WAS NOT
// LISTED -- which is now trivially true because everything is listed. The
// converse, "everything listed is callable", is deliberately given up; that is
// precisely what the ruling asks for, and TestExitGate_ListedIsNotCallable
// pins it so nobody restores the old identity by accident.
//
// SITE-SCOPE VISIBILITY IS UNTOUCHED BY THIS CHANGE. The design's other half --
// that site scope should hide -- was not ruled on and is not implemented here.
// A site-keyed tool with an empty resolved scope is still listed and still
// refuses at invocation with mcp_scope_empty.
// ---------------------------------------------------------------------------

// capabilityHeld reports whether auth's GRANT covers what entry requires.
//
// It is consulted by AuthorizeTool for the refusal decision and by VisibleTools
// only to decide whether to ANNOTATE a descriptor -- never to drop one. A
// future edit that turns its false answer back into a `continue` in
// VisibleTools is the thing the D1 ruling forbids: that is the boundary whose
// whole point is that it refuses visibly. Dropping is withinOrgCeiling's job
// and only withinOrgCeiling's.
func capabilityHeld(entry ToolPolicy, auth AuthorizedRequest) bool {
	return auth.Capabilities.Allows(entry.Capability)
}

// withinOrgCeiling reports whether entry's capability is inside the
// ORGANISATION's ceiling -- the widest set any connection in this org may hold.
//
// THIS IS THE BOUNDARY THAT HIDES, and it is the only one. The distinction from
// capabilityHeld is the entire ruling:
//
//	inside the ceiling, held by the grant     -> listed, callable
//	inside the ceiling, NOT held by the grant -> listed, refuses by name
//	outside the ceiling                       -> not listed, refuses as unknown
//
// The middle row is why unticking a permission is explicable: an operator who
// removes a capability gets a tool that says what happened and who can fix it,
// instead of one that disappears -- and a vanished tool is indistinguishable
// from a tool that was never built, so the model's only reading is "wpmgr
// cannot do this" and nobody ever hears that something was refused.
//
// The last row is why the org boundary is different: a token holder should not
// be able to enumerate capabilities their organisation deliberately switched
// off. Listing them, even annotated, is exactly that enumeration.
//
// A ZERO-VALUE CEILING ADMITS NOTHING, which is fail-closed and deliberate.
// See AuthorizedRequest.OrgCeiling.
func withinOrgCeiling(entry ToolPolicy, auth AuthorizedRequest) bool {
	return auth.OrgCeiling.Allows(entry.Capability)
}

// capabilityNotice is the LISTING half of the ruling. The descriptor for a tool
// this connection cannot currently call says so, in the description, which is
// the one field of a tools/list entry every MCP client already puts in front of
// the model.
//
// IT SAYS THE REFUSAL IS PERMANENT, and that sentence is load-bearing rather
// than polite. A model that reads "unavailable" as transient retries, and a
// retry loop against a permanent refusal is the exact failure this surface has
// already shipped once: the empty-capability path answered 401, and clients
// re-ran an entire OAuth handshake that could not possibly change the outcome.
// The wording here and in AuthorizeTool's message agree on that point on
// purpose.
func capabilityNotice(entry ToolPolicy) string {
	return fmt.Sprintf(
		"\n\nNOT AVAILABLE TO THIS CONNECTION. This tool requires the %q capability and "+
			"this connection's grant does not hold it. Calling it refuses with %s. That "+
			"refusal is permanent for this connection: retrying, refreshing the token or "+
			"re-running the OAuth authorisation will return the same answer. A wpmgr "+
			"operator must grant %[1]q to this connection before it can be used.",
		entry.Capability, ErrCodeCapabilityNotGranted)
}

// VisibleTools is the tools/list answer: every tool INSIDE THIS
// ORGANISATION'S CEILING, with the ones this connection's own grant cannot call
// marked as such in their description.
//
// TWO BOUNDARIES, TWO ANSWERS, and which one excludes a tool decides whether it
// is hidden or explained. withinOrgCeiling drops; capabilityHeld only
// annotates. The reasoning for the asymmetry is on withinOrgCeiling and it is
// the whole ruling -- do not collapse the two predicates into one.
//
// IT IS STILL A PER-CONNECTION VALUE AND IT IS STILL NOT Tools(). Both the
// membership and the descriptions differ by connection, and handing a caller
// the unannotated registry would give the model a tool that looks callable and
// is not -- the silent half of exactly the problem this change fixes.
// transport.go calls this one; the drift tests call Tools().
//
// It returns a fresh slice. An empty result is a truthful answer for a
// connection whose org ceiling admits nothing, not an error -- but a tool the
// GRANT merely lacks never produces one, which is the point: that case is
// listed and annotated.
//
// SITE SCOPE IS NOT CONSULTED HERE. A site-keyed tool with an empty resolved
// scope is still listed and still refuses at invocation with mcp_scope_empty.
// That half was ruled on separately and is deliberately untouched.
func VisibleTools(auth AuthorizedRequest) []ToolDescriptor {
	entries := registryTools()
	out := make([]ToolDescriptor, 0, len(entries))
	for _, e := range entries {
		if !withinOrgCeiling(e, auth) {
			continue
		}
		d := ToolDescriptor{
			Name:        e.Name,
			Description: e.Description,
			InputSchema: e.InputSchema,
		}
		if !capabilityHeld(e, auth) {
			d.Description += capabilityNotice(e)
		}
		out = append(out, d)
	}
	return out
}

// visibleToolNames lists what this connection was actually shown. Safe to
// return in an error body: it is exactly the tools/list answer this connection
// has already been given, so it discloses nothing new, and it lets a model that
// mistyped a real name correct in one round trip.
//
// It carries NAMES ONLY, so it does not repeat the capability notice. A model
// that finds its name in this list and still cannot call the tool is looking at
// the capability refusal, which names the capability itself.
func visibleToolNames(auth AuthorizedRequest) []string {
	ts := VisibleTools(auth)
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name)
	}
	return out
}

// ---------------------------------------------------------------------------
// AuthorizeTool -- the tools/call gate, and the disclosure decision
// ---------------------------------------------------------------------------

// AuthorizeTool resolves a tool name for INVOCATION. It is the only way to get
// a callable ToolPolicy, and it decides the capability question for itself
// rather than trusting that tools/list ran.
//
// ------------------------------------------------------------------------
// THE DISCLOSURE DECISION, AS THE D1 RULING SETTLED IT.
//
// The tension is real and it was previously resolved the other way. "Your
// connection does not hold mcp.sites.restart" is a precise, actionable, typed
// refusal -- and it confirms that a tool named sites.restart exists. This file
// used to answer registered-but-not-held and never-existed identically so that
// no wordlist could tell them apart.
//
// THE OWNER RULED THAT REFUSAL BEATS CONCEALMENT, and the ruling is right on
// the facts of this surface. The blur bought nothing once tools/list stopped
// filtering -- the listing already hands over every name -- and it cost the
// model the only information that could end the exchange usefully. A model told
// "not available, ask tools/list" by a server whose tools/list DOES show the
// tool has been handed a contradiction, and its next move is to try again.
//
// SO THERE ARE NOW TWO ANSWERS, AND THEY ARE DIFFERENT ON PURPOSE:
//
//   - A NAME NOT IN THE REGISTRY gets ErrCodeToolNotAvailable. The model
//     guessed. Nothing exists to disclose.
//   - A REGISTERED NAME WHOSE CAPABILITY IS NOT HELD gets
//     ErrCodeCapabilityNotGranted, naming the capability, naming what this
//     connection does hold, and saying the refusal is PERMANENT.
//
// TERMINALITY IS PART OF THE CONTRACT. The refusal must not read as a transient
// failure, because a model that reads it that way retries, and a retry loop
// against a permanent refusal is a failure this surface has already shipped:
// the empty-capability path answered 401, and clients responded by re-running
// an OAuth handshake that could not change the outcome. The message says so in
// words for the model and the details carry retryable=false for the client.
//
// The other two disclosable refusals are unchanged, and both remain facts about
// the caller's own grant for a tool it was shown:
//
//   - the named mcp_scope_empty refusal, when a site-keyed tool has an empty
//     resolved site scope. SITE-SCOPE VISIBILITY IS NOT PART OF THIS CHANGE.
//   - invalid arguments, which carries the schema so a model corrects in one
//     round trip.
//
// THE OPERATOR LOG STILL GETS THE PRECISE REASON EITHER WAY. refusalReason is
// returned to the transport, which logs it with the tenant, the grant and the
// attempted name -- that was the only typed channel before this change, and it
// is still the one an operator greps.
// ------------------------------------------------------------------------
//
// The returned ToolPolicy is not "found"; it is "found AND permitted AND
// invocable", and there is deliberately no way for a caller to obtain the first
// without the rest.
func AuthorizeTool(name string, auth AuthorizedRequest) (ToolPolicy, refusalReason, error) {
	var found bool
	var entry ToolPolicy
	for _, e := range registryTools() {
		if e.Name == name {
			entry, found = e, true
			break
		}
	}

	if !found {
		// The model guessed a name. This is now the ONLY use of the uniform
		// answer, and there is no longer a second reason hiding behind it.
		return ToolPolicy{}, reasonUnregistered, domain.Forbidden(ErrCodeToolNotAvailable,
			fmt.Sprintf("no tool named %q exists on this server. Call tools/list for the "+
				"tools this server has; every one of them appears there, so a name absent "+
				"from that list will never resolve however it is spelled.", name))
	}

	if !withinOrgCeiling(entry, auth) {
		// OUTSIDE THE ORG CEILING ANSWERS EXACTLY AS AN UNREGISTERED NAME DOES,
		// and the sameness is the point rather than laziness.
		//
		// VisibleTools omitted this tool so the caller could not enumerate what
		// their organisation switched off. A distinguishable refusal here would
		// hand back precisely that enumeration -- guess the name, read the
		// code, learn which capabilities exist and that yours is not one of
		// them. The listing and the gate have to tell the same story or the
		// quieter one is decorative.
		//
		// This is NOT the capability refusal above. That one names the
		// capability on purpose, because the org HAS enabled it and an operator
		// can act. Here no operator in this organisation has anything to tick,
		// so there is nothing actionable to disclose.
		//
		// The operator log still gets the precise reason: refusalReason is
		// returned to the transport and logged with the tenant, the grant and
		// the attempted name.
		return ToolPolicy{}, reasonUnregistered, domain.Forbidden(ErrCodeToolNotAvailable,
			fmt.Sprintf("no tool named %q exists on this server. Call tools/list for the "+
				"tools this server has; every one of them appears there, so a name absent "+
				"from that list will never resolve however it is spelled.", name))
	}

	if !capabilityHeld(entry, auth) {
		// THE TYPED CAPABILITY REFUSAL. Naming the capability is the ruling;
		// naming the held set and marking it non-retryable is what makes it
		// actionable rather than merely precise.
		return ToolPolicy{}, reasonCapabilityNotHeld, domain.Forbidden(ErrCodeCapabilityNotGranted,
			fmt.Sprintf("tool %q requires the %q capability and this connection's grant does "+
				"not hold it. This is a permanent property of the grant and not a transient "+
				"failure: retrying this call, refreshing the connection token or re-running "+
				"the OAuth authorisation will return exactly this answer. A wpmgr operator "+
				"must grant %[2]q to this connection before it can be called.",
				name, entry.Capability)).
			WithDetails(map[string]any{
				"tool":                name,
				"required_capability": string(entry.Capability),
				// Sorted() renders the caller's OWN grant. Returning it
				// discloses nothing new and lets a model tell the user exactly
				// what this connection can and cannot do.
				"held_capabilities":   capabilityNames(auth.Capabilities.Sorted()),
				"retryable":           false,
			})
	}

	if entry.invoke == nil {
		// A registry entry with no implementation is a build defect, and it is
		// refused as an INTERNAL failure rather than as a permission one.
		// Reporting it as "not available to this connection" would tell an
		// operator their grant is wrong when the truth is that the server is
		// broken, and they would go and widen a scope that was never the
		// problem.
		return ToolPolicy{}, "", fmt.Errorf("registry entry %q has no implementation", name)
	}

	// THE SITE AXIS, ENFORCED AT THE GATE AND NOT ONLY IN THE SERVICE.
	//
	// ListSitesForModel makes this same check, and the redundancy is deliberate
	// in the same way the Go mirror of a schema CHECK is: the service check
	// protects the one tool that has it, this one protects every tool that ever
	// declares RequiresSiteScope, including the next one whose author forgets.
	// A site-keyed read reached with an empty resolved scope must REFUSE and
	// say so, not return an empty result that reads as a healthy org owning
	// nothing.
	//
	// THIS IS UNCHANGED BY THE D1 RULING and deliberately so: the ruling covers
	// the capability axis only. The tool is listed, as it always was, and this
	// refusal is by name, as it always was.
	if entry.RequiresSiteScope && auth.Sites.IsEmpty() {
		// The reason is returned, NOT "". An empty reason here made the refusal
		// invisible to the operator log, which is half of what justified
		// blurring the other reasons on the wire.
		return ToolPolicy{}, reasonSiteScopeEmpty, domain.Forbidden(ErrCodeScopeEmpty,
			"this connection's site scope resolves to no sites, so there is nothing it may read. "+
				"This is a refusal, not an empty fleet: check the grant's site scope.")
	}
	return entry, "", nil
}

// Tools returns the FULL registry surface as descriptors, unfiltered by any
// connection.
//
// IT IS NOT THE tools/list ANSWER and no request path may use it -- VisibleTools
// is. It exists for operator-facing surfaces and for the drift tests, which
// need to enumerate what the server has rather than what one grant may reach.
// It returns a fresh slice from the closed literal for the same reason
// registryTools does.
func Tools() []ToolDescriptor {
	entries := registryTools()
	out := make([]ToolDescriptor, 0, len(entries))
	for _, e := range entries {
		out = append(out, ToolDescriptor{
			Name:        e.Name,
			Description: e.Description,
			InputSchema: e.InputSchema,
		})
	}
	return out
}
