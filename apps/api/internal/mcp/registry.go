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
// The fix is not a second check bolted onto tools/call. It is that BOTH paths
// resolve through the SAME function over the SAME closed list:
//
//	tools/list  -> VisibleTools(auth)  -> visible(entry, auth)
//	tools/call  -> AuthorizeTool(name, auth) -> visible(entry, auth)
//
// so a tool cannot be callable and invisible. There is no path from a tool name
// to an invocation that does not pass visible(); the dispatch function itself
// hangs off the registry entry, so tools/call has no switch and therefore no
// `default` arm to fall through.
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
	// It does NOT hide the tool from tools/list. See the asymmetry note on
	// visible() for why visibility follows the capability axis alone.
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
	return []ToolPolicy{{
		Name: ToolListSites,
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
}

// ---------------------------------------------------------------------------
// The one predicate both paths use
// ---------------------------------------------------------------------------

// refusalReason is the OPERATOR-FACING classification of why a tool was not
// reachable. It is precise, it is logged, and it is NEVER sent to the caller.
// See AuthorizeTool.
type refusalReason string

const (
	// reasonUnregistered: no entry of that name exists. The model guessed.
	reasonUnregistered refusalReason = "unregistered"
	// reasonCapabilityNotHeld: the entry exists and this connection's
	// capability set does not hold what it requires.
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

// visible reports whether auth may SEE AND CALL entry. tools/list and
// tools/call both go through it, which is what makes "callable" and "visible"
// the same predicate rather than two that agree by convention.
//
// IT TESTS THE CAPABILITY AXIS AND NOT THE SITE AXIS, and the asymmetry is
// deliberate. The two axes answer different questions and are disclosable to
// different degrees:
//
//   - CAPABILITY answers "may this connection reach this tool at all". A
//     connection that does not hold the capability is not entitled to know the
//     tool exists, so the tool is absent from tools/list and its name gets the
//     uniform refusal.
//
//   - SITE SCOPE answers "is there any data for it to read". A connection whose
//     org enabled the tool and whose capability set covers it IS entitled to
//     know the tool exists -- it would have been listed but for an empty site
//     scope, which is a fact about its own grant. Gating VISIBILITY on it would
//     turn the most common benign misconfiguration in this surface into an
//     unexplainable dead end, and would buy nothing: the only names it hides
//     are ones the caller's own capability set already entitles it to see.
//
// So site scope is enforced at INVOCATION, by AuthorizeTool, with the named
// mcp_scope_empty refusal that says exactly what is wrong. That keeps the
// invariant that matters -- nothing is callable that was not visible -- while
// leaving the actionable half actionable.
func visible(entry ToolPolicy, auth AuthorizedRequest) (bool, refusalReason) {
	if !auth.Capabilities.Allows(entry.Capability) {
		return false, reasonCapabilityNotHeld
	}
	return true, ""
}

// VisibleTools is the tools/list answer: the descriptors for exactly the tools
// this connection may call. A tool it may not call is ABSENT, not present and
// marked unavailable -- a disabled entry in a list is still a disclosure that
// the tool exists.
//
// It returns a fresh, possibly empty slice. An empty tools/list is a truthful
// answer for a connection that reaches nothing, and it is not an error: the
// client is entitled to know its own surface is empty. The refusal with a
// reason happens when it CALLS something.
func VisibleTools(auth AuthorizedRequest) []ToolDescriptor {
	entries := registryTools()
	out := make([]ToolDescriptor, 0, len(entries))
	for _, e := range entries {
		if ok, _ := visible(e, auth); !ok {
			continue
		}
		out = append(out, ToolDescriptor{
			Name:        e.Name,
			Description: e.Description,
			InputSchema: e.InputSchema,
		})
	}
	return out
}

// visibleToolNames lists what this connection was actually shown. Safe to
// return in an error body: it is exactly the tools/list answer this connection
// is already entitled to, so it discloses nothing new, and it lets a model that
// mistyped a name it legitimately holds correct in one round trip.
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
// a callable ToolPolicy, and it applies the same visible() predicate tools/list
// applies, so a name that was never shown is not callable.
//
// ------------------------------------------------------------------------
// THE DISCLOSURE DECISION, AND THE TENSION IT RESOLVES.
//
// The brief for this slice asks for "typed refusals that name the reason", and
// non-disclosure pulls directly against that. "Your organisation has not
// enabled sites.restart" is a precise, actionable, well-typed refusal -- and it
// confirms that sites.restart EXISTS to a caller who was never shown it. Repeat
// that against a wordlist and the refusal messages enumerate the entire tool
// surface of a product the caller has no entitlement to see, including tools
// that are unreleased, gated, or being trialled by one organisation.
//
// The line drawn here is: A NAME THE CONNECTION WAS NOT SHOWN GETS ONE
// INDISTINGUISHABLE ANSWER. Unregistered and registered-but-capability-not-held
// return the same code, the same message and the same data. There is no
// existence oracle, because there is no observable difference between "no such
// tool" and "not yours".
//
// This is the same call authz.RequireSiteAccess makes one layer down, where a
// site the caller may not reach answers 404 and not 403 SPECIFICALLY so that
// probing ids cannot enumerate the fleet. A different answer here would undo
// that at the MCP boundary for anyone holding any valid connection token.
//
// THE REFUSAL IS STILL TYPED -- just not on the caller's axis. The operator
// gets the precise reason, on the operator's own surface, where the audience is
// entitled to it: refusalReason is returned to the transport, which logs it
// with the tenant, the grant and the attempted name. An operator debugging
// "why can't my client call this" reads one log line and gets the exact answer;
// an attacker probing names gets one sentence, identical every time.
//
// WHAT REMAINS DISCLOSABLE, AND WHY IT IS NOT A LEAK. A tool the connection CAN
// see fails with its real reason. Two such reasons exist and both are facts
// about the caller's own grant, for a tool tools/list already showed it:
//
//   - the named mcp_scope_empty refusal, when a site-keyed tool has an empty
//     resolved site scope. This is the actionable case an operator actually
//     hits, and blurring it would send them hunting a capability problem they
//     do not have. See the asymmetry note on visible().
//   - invalid arguments, which carries the schema so a model corrects in one
//     round trip.
//
// The error body also carries the connection's OWN visible tool list, which is
// exactly the tools/list answer it is already entitled to.
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

	reason := reasonUnregistered
	if found {
		var ok bool
		if ok, reason = visible(entry, auth); ok {
			if entry.invoke == nil {
				// A registry entry with no implementation is a build defect,
				// and it is refused as an INTERNAL failure rather than as a
				// permission one. Reporting it as "not available to this
				// connection" would tell an operator their grant is wrong when
				// the truth is that the server is broken, and they would go and
				// widen a scope that was never the problem.
				return ToolPolicy{}, "", fmt.Errorf("registry entry %q has no implementation", name)
			}

			// THE SITE AXIS, ENFORCED AT THE GATE AND NOT ONLY IN THE SERVICE.
			//
			// ListSitesForModel makes this same check, and the redundancy is
			// deliberate in the same way the Go mirror of a schema CHECK is:
			// the service check protects the one tool that has it, this one
			// protects every tool that ever declares RequiresSiteScope,
			// including the next one whose author forgets. A site-keyed read
			// reached with an empty resolved scope must REFUSE and say so, not
			// return an empty result that reads as a healthy org owning
			// nothing.
			//
			// Only a caller who passed visible() reaches here, so naming the
			// reason discloses nothing beyond that caller's own tools/list.
			if entry.RequiresSiteScope && auth.Sites.IsEmpty() {
				// The reason is returned, NOT "". An empty reason here made the
				// refusal invisible to the operator log, which is half of what
				// justifies blurring the other two reasons on the wire.
				return ToolPolicy{}, reasonSiteScopeEmpty, domain.Forbidden(ErrCodeScopeEmpty,
					"this connection's site scope resolves to no sites, so there is nothing it may read. "+
						"This is a refusal, not an empty fleet: check the grant's site scope.")
			}
			return entry, "", nil
		}
	}

	// ONE answer for all three reasons. The reason is returned alongside for
	// the operator log and never reaches the wire.
	return ToolPolicy{}, reason, domain.Forbidden(ErrCodeToolNotAvailable,
		fmt.Sprintf("tool %q is not available to this connection. This answer is the same "+
			"whether no such tool exists or whether this connection's capabilities do "+
			"not cover it -- the server does not distinguish those to a caller. Call "+
			"tools/list for the tools this connection may use; a tool absent from that "+
			"list is not reachable by naming it here.", name))
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
