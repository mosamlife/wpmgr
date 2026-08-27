# ADR-061 — Assistant surface, Phase 1: control-plane server and out-of-band approval

**Status:** Accepted (amended 2026-08-23 and 2026-08-24) · **Date:** 2026-08-22
**Supersedes/relates:** ADR-060 (this work sits in its "constrained automation" phase, ahead of differentiation — and its freeze clause fixes the ordering *inside* this phase, per amendment A9, corroborated by ADR-060's own Amendment A1 defining "externally-reachable surface" for that clause), ADR-057 (unaffected; the capability model here is a separate axis from per-site security policy), ADR-062 (Proposed — revisits Decision 7 below and will supersede it once Accepted, per amendment A7), ADR-063 (licensing and third-party reuse — its conclusion independently supports Decision 1's siting).

This ADR records the locked Phase 1 design for the assistant surface: an
endpoint on the control plane that lets an AI assistant read a fleet and
propose work, where nothing that changes a site runs without a human decision
taken in a channel the model cannot reach.

It records the decisions and the reasoning behind them, not the build plan.
**Phase 2 — content operations and page-builder work — is out of scope for
this record and gets its own ADR.** Nothing here should be read as approving
any part of it.

**Amended 2026-08-23 — read [Amendments](#amendments-2026-08-23) before acting
on Decisions 1, 6 or 7.** The status stays Accepted and no decision text below
was rewritten. Eight amendments are recorded at the end of this file: Decision 1
is clarified rather than changed, Decision 6 is amended, Decision 7 is
revisited pending ADR-062's acceptance, and four items ADR-061 was silent on
are decided.

**Amended again 2026-08-24 — read [Amendments](#amendments-2026-08-24) before
planning any Phase 1 work.** Five further amendments, A9 through A13. **A9 is the
one to read first**: ADR-060's freeze clause fixes the ordering *inside* Phase 1,
so the gate work closes before the endpoint ships. The rest make the audit posture
fail-closed for this surface, put site scoping in v1 as named work, settle three
protocol-header cases the code must not conflate, and state the rule that skill
and prompt content is data and never permission. No decision text below is
rewritten by any of them.

---

## Context

An assistant connector is table stakes and it is commoditizing. Connecting a
model to a site-management product is a few weeks of work for anyone who wants
to do it, and the number of products that have done it grows every quarter. A
connector is therefore not a moat, and building one because others have is a
plan to arrive second at a place with no prize.

What is not commoditizing is the part every connector defers: proving that a
**person** authorized a change. The market position worth taking is not "our
assistant can do more things"; it is "when our assistant changes your fleet, a
named human approved it, and you can verify that claim without taking our word
for it". That claim is an architecture, not a feature, and it has to be
designed in from the first release because it cannot be retrofitted onto a
connector that shipped without it.

The second force is that we already hold a hash-chained audit log with an
existing verification endpoint. A decision recorded inside that chain is
externally checkable by a customer's auditor today. That asset only pays out if
the approval decision lands inside it, which constrains the design.

Phase 1 is therefore scoped to reading the fleet and proposing change. The
model applies nothing directly.

---

## Decision 1 — The server lives on the control plane, not on managed sites

*Clarified 2026-08-23 by amendment A1. The text below is unchanged. The
clarification scopes the fan-out argument and activates the second clause of the
first Consequences bullet; it does not weaken the credential argument.*

**The assistant endpoint is a route on the existing API host. It is not a
surface on the WordPress agent, and it does not route through the WordPress
adapter.**

This is the load-bearing architectural choice in the document and everything
else depends on it.

Routing assistant traffic through the WordPress adapter would require a
privileged WordPress user on every managed site and a standing credential held
somewhere that can use it. That undoes the push-based signed-command design the
whole product rests on: today the control plane sends a signed command and the
site verifies it, so there is no standing credential on the site to steal and
no privileged session sitting open. A per-site assistant path reintroduces
exactly the long-lived privileged credential that design removed, multiplied by
the size of the fleet, and it does so for the convenience of a feature rather
than for any property the feature needs.

It is also the wrong place for the work. Fleet questions are answered from
inventory the control plane already holds. Sending them to individual sites
turns one Postgres query into an unbounded fan-out of network round trips, each
of which can be slow, offline, or compromised.

Consequences of siting it centrally: one origin, one TLS boundary, one auth
path, and **zero agent-side change in Phase 1**. The agent plugin does not move
for this feature.

---

## Decision 2 — Out-of-band approval, with disjoint credential classes

**Approval happens on a control-plane surface the model cannot reach, and the
credential class that proposes and the credential class that approves are
disjoint by construction.**

This is the differentiator, and it is worth being precise about why.

The common shape in shipped assistant integrations is to hand the approval
token to the model: the model receives a confirmation value and sends it back
to confirm. That design cannot distinguish a human approving from the model
approving, because the model holds everything needed to do either. A product
built that way can say a confirmation step occurred. It cannot say a person
took it.

Ours can, because of three structural properties:

- **The proposer's credential cannot approve.** The assistant authenticates
  with an agent-kind key. The approve path refuses that principal type
  outright, and there is no approve tool in the registry. Its absence is
  documented so the model stops probing for it.
- **The approver's credential cannot propose.** A dashboard session cannot
  reach the assistant endpoint.
- **The confirmation phrase is issued by the server and never sent to the
  model.** The operator echoes it into a single conditional `UPDATE` that
  cannot race.

The decision lands as metadata inside the existing hash-chained audit log,
inside the fields the canonical hash covers, so "a human approved this" is
verifiable through an endpoint we already ship.

**The approval transition and its ledger entry commit together, or neither
commits.** They are one database transaction. If the ledger append fails, the
approval fails, the operator is told it failed, and nothing is dispatched.

This departs from the best-effort audit convention used elsewhere in this
codebase, where a failed audit write is logged and the operation proceeds. That
convention is defensible where the ledger *records* an action: losing a row
degrades an investigation, and refusing the action would be a worse outcome
than a gap in the log.

It is not defensible here, because on this path the ledger entry **is the
product claim, not a record of it.** The entire differentiator in Decision 2 is
that a customer's auditor can verify a named human approved a specific action.
An approval that committed while its ledger entry did not is an action taken
with no verifiable human behind it — which is precisely the thing this design
exists to make impossible — and best-effort auditing would produce exactly that
outcome, silently, under the ordinary conditions that make a write fail. Worse,
we could not afterward enumerate which approvals were affected, so a single
lost append would put every approval in the account into question rather than
one.

Two consequences follow and are accepted. Approval is unavailable when the
ledger is unwritable, rather than degraded; that is the correct failure
direction for this operation and the operator sees a real error rather than a
false success. And the approve path must not adopt any later "audit
asynchronously" or "queue the append" optimization without superseding this
ADR, because either one reintroduces the gap.

**Dispatch starts only after that transaction commits, and the committed row is
what causes it.** Both simpler orderings are wrong, in different ways.

*Dispatch inside the transaction* is wrong because a database transaction
cannot roll back a command that has already left the building. Once a signed
command is on its way to a site, rolling back the approval row does not recall
it, so the atomicity above would be satisfied while the guarantee it exists to
provide is broken. It is also the shape that holds a transaction open across a
network call to a machine that may be slow, unreachable, or hanging — a shape
this codebase has already been bitten by, and one that turns one unreachable
site into database-wide contention.

*Dispatch immediately after commit, as the next statement*, is wrong because a
crash in the gap between the two leaves an approval a human genuinely granted,
recorded in the ledger as granted, that never ran and that nothing will ever
retry.

So the approval commits into an explicit `approved, not yet dispatched` state,
and a worker claims committed rows in that state and dispatches them. The
commit is the handoff. A crash in the gap is then not a lost action but an
unclaimed row, which is what recovery is for, and the retry is idempotent
against the already-signed command rather than a second approval.

**What the operator sees is part of this decision, not an implementation
detail.** `approved, not yet dispatched` is a state the interface represents
honestly and by name. It is never rendered as "running", because nothing is
running, and never silently folded into "approved" as though the work had
happened. If dispatch keeps failing, the request surfaces as approved but not
started, with the reason and the time of the last attempt, and it stays that
way rather than aging into something that looks finished. The ledger entry
remains correct and unedited throughout: a human did approve, at that time, and
that is what it says. What must never happen is a screen implying the fleet
changed when it did not.

**No automation escape hatch, ever.** No auto-approve tier, no policy engine
that can approve, no "trusted automation may approve" flag, no approve button
in a notification email, no "remember this choice for this session". Every one
of those makes the sentence above false, and the sentence is the product.

---

## Decision 3 — The approval surface renders control-plane-derived facts only

**Every decision-relevant fact on an approval screen is derived by the control
plane from its own inventory. Model-authored text appears in exactly one
quarantined place: after the facts, before the controls.**

An approval screen that shows the model's summary of what it is about to do is
an operator approving the attacker's description of the action. If a model has
been steered by injected content, its summary is precisely the artifact not to
trust, and putting it where the decision gets made hands the attacker the one
surface that converts a sentence into authority over a fleet.

So every fact on the surface comes from a structured selection — site ids,
component slugs, target versions — and nothing else the model sends. The
control plane re-derives every human-readable line from inventory it holds, and
flags downgrades and major-version bumps **from its own data**, regardless of
what the model claimed.

**The one exception is a single quarantined note, and it is specified here
rather than left to the build**, because an under-specified quarantine is how
model text leaks onto a surface that must not carry it.

The quarantine block is kept rather than removed. An operator with no statement
of intent approves on shape alone, and a short note genuinely helps answer "is
this the thing I asked for". Its contract:

- **It is one named column on the proposal record, and it is the only
  proposer-controlled free text anywhere in the schema.** Every other column on
  the parent table and on its children is either control-plane-derived or a
  closed enum. The reason field in particular is a server-side enum that cannot
  carry a sentence.
- **It is excluded from the plan digest**, so editing it cannot invalidate an
  approval and cannot be used to churn one.
- **It never becomes a fact.** It is not an input to any computed value, any
  flag, any ordering, any grouping, or any control-plane-derived line on the
  screen.
- **It renders in exactly one slot**: after the facts, before the controls, in
  a visually demoted surface, as an escaped plain-text node — never markup,
  never a link or link label, never a title attribute — with control
  characters, bidirectional overrides and zero-width characters stripped, hard
  capped, and labeled with both what it is and what it is not.
- **It appears nowhere else, ever**: not in the title, the summary, the queue
  row, the notification or email body or subject, the audit row title, or any
  API response consumed by something that will render it as anything but a
  plain text node.

If any part of that contract cannot be met at build time, the note is dropped
rather than shipped loosely. The facts are what the decision rests on; the note
is a convenience, and a convenience does not get to weaken the surface.

One consequence that was nearly missed and is recorded so it is not missed
again: **site-origin strings are an injection surface for the human, not only
for the model.** Component and theme display names are reported by managed
sites, which makes them attacker-controlled, and they render in fixed slots on
the approval card. They get control-character, bidirectional-override and
zero-width stripping on the way to the human surface, they render as text nodes
and never as markup or link labels, and a test plants a hostile name and
asserts it does not survive.

---

## Decision 4 — Site scoping is enforced in the application, not the database, in v1 — and the UI says so

**Assistant principals are organization-scoped at the database. The
connection's site allowlist is an application-layer filter through one audited
chokepoint, and the interface describes it in exactly those terms.**

An earlier draft claimed site-scoped row-level security was an existing, proven
property of this codebase that the feature could simply inherit. That claim is
false, and recording why is the point of this section.

The site-scope policies exist, but they are activated only by the transaction
helper that sets the scope GUC, and almost nothing calls it. Rather than pin a
number that drifts, here are the commands; run them from `apps/api` and compare
the two results:

```sh
count_calls() {
  pat=$1
  hits=$(grep -rn "$pat" --include="*.go" internal/ | grep -v _test | grep -c .)
  [ "$hits" -gt 0 ] || { echo "FAIL: no match for '$pat' -- renamed or moved, which is not zero call sites" >&2; return 1; }
  echo "$pat -> $hits"
}

# Control. If the helper is gone the two counts below mean nothing, so check first.
grep -q "func (p \*Pool) InScopedTenantTx" internal/db/db.go \
  || { echo "FAIL: InScopedTenantTx is not defined in internal/db/db.go" >&2; exit 1; }

count_calls "\.InScopedTenantTx(" || exit 1
count_calls "\.InTenantTx("      || exit 1
```

**Note the shape, and do not simplify it back.** Two things in it are
load-bearing and both were got wrong in earlier drafts of this ADR.

*Refusing on zero.* The obvious form, `grep … | wc -l`, takes its exit status
from `wc`, so a pattern that matches nothing prints `0` and exits `0` — a
renamed helper would read as "no call sites activate the policies", which is
this ADR's conclusion arrived at by accident rather than by evidence. Counting
with `grep -c .` and refusing on zero makes an absent pattern go red, and the
control grep must match for the same reason: without it the counts measure
nothing.

*The leading `\.` is not decoration.* An unanchored `InScopedTenantTx(` also
matches the function's own definition and its interface declaration, both in
`internal/db/db.go`. An earlier draft counted those, which inflated the figure
by two and — worse — meant the guard could report a healthy non-zero count with
every real call site deleted, since the definition alone would satisfy it.
Anchoring on the method-call form counts calls and nothing else. `InTenantTx`
was already written this way and is unaffected; it was checked rather than
assumed.

The first count is a handful of call sites; the second is the rest of the
application. On every path in the second group the site-scope policies are
inert, because their predicate short-circuits when the GUC is unset. The first
number has already moved once during the drafting of this ADR, which is the
argument against writing either one down.

Worse for this feature specifically, the two tables a fleet assistant most
needs carry no site-scope policy at all — and this check is written so that a
renamed table fails rather than reads as an absence of policy:

```sh
check_table() {
  t=$1
  block=$(awk -v t="$t" '
    $0 ~ ("^CREATE POLICY [a-z_0-9]+ ON " t "$") {inb=1}
    inb {print}
    inb && /;[[:space:]]*$/ {inb=0}
  ' db/schema.sql)
  [ -n "$block" ] || { echo "FAIL: no policy found on $t -- table renamed or dropped, which is not 'no site scoping'" >&2; return 1; }
  echo "$t policies:"
  printf '%s\n' "$block" | grep -oE "^CREATE POLICY [a-z_0-9]+" | sed 's/^/  /'
  if printf '%s\n' "$block" | grep -qE 'site_scope|allowed_site_ids'; then
    echo "FAIL: $t now carries a site-scope predicate; this ADR's premise has changed" >&2
    return 1
  fi
  echo "  OK: no site-scope predicate in any policy on $t"
}

check_table site_vulnerabilities || exit 1
check_table update_runs         || exit 1
```

**Each table is asserted separately, and on its predicate rather than its
name.** Both properties matter and an earlier draft had neither.

Checking the two tables with one combined pattern meant output from either
satisfied the non-empty control, so a dropped or renamed `update_runs` would
have passed on the strength of `site_vulnerabilities` still matching. Per-table
means a table that vanishes fails as a table that vanished, which is a
different fact from "this table has no site scoping" and must not read as one.

Matching on the policy *name* containing `_site_scope` assumed a naming
convention rather than checking behavior. A site-scope predicate introduced
under any other name would have passed silently. The check now reads each
policy's whole definition, up to its terminating semicolon, and looks for the
GUC the scoping actually depends on — so it goes red on the semantics whatever
the policy ends up being called.

What comes back today is the tenant-isolation and cross-tenant worker policy on
each table, and nothing else. There is no site-scope predicate to activate. On
the day the deferred migration lands this goes red, which is the correct moment
for someone to supersede this section rather than discover it is stale.

There are also fail-open shapes for a principal whose scope is the zero value,
in the transaction dispatcher, in the site-access middleware, and in the
principal's own access predicate. An assistant principal that was
site-scoped-in-name-only would land on those paths.

Given that, manufacturing database-level per-site isolation for a non-human
principal inside this release is not available. The honest design is the one
taken: organization scope at the database, and one Go chokepoint that resolves
which sites a request may touch by intersecting the request with the
connection's allowlist, refusing an empty result with a named code. A registry
test asserts every registered tool declares a site parameter derived from that
chokepoint; a tool that cannot be expressed that way is not registered.

**The interface tells the truth about this.** The copy rule for this feature is
to describe what happens on a request and never to describe a boundary.
"Checked before every request", "refused and recorded", "you can see the
refusals" are permitted. Words claiming isolation, sandboxing, impossibility or
guarantee are banned outright in this feature's surfaces, and the ban is
enforced the way the shipped-copy gate is enforced, with the standing guard
treatment: plant the word, watch it go red, restore, watch it go green, then
construct the honest cases it must not block.

The mechanism is stated with its limit and then a stronger lever is handed
over: the check runs in one place, on every request, before anything reaches a
site; it is not a separate boundary inside the database; if a site must never
be touched, pause the assistant or remove the site from every list. The refusal
counter on the connection screen turns that claim into evidence an operator can
click into, and it stays visible reading zero, because an empty log is
information.

**Database-level site scoping is deferred and is its own decision.** It is a
migration owned by `database-engineer`, adding site-scope restrictive policies
to the tables above and to the proposal tables, and it carries one rule worth
recording now: a plan is visible only to a principal who can see **all** of it,
never any part of it, because a principal who sees half a fleet plan could
otherwise approve a plan whose other half is invisible to them.

---

## Decision 5 — Capability sets, not roles (implemented)

*Referenced by amendment A3 (2026-08-23). A per-site dispatcher was considered
and rejected there partly on this decision's own terms: a runtime-generated
argument enum is a gate evaluated inside a registry, which is exactly what the
absence-only rule below exists to avoid. The text below is unchanged.*

**An API key carries an explicit capability set rather than a rank-ordered
role. This shipped in v0.61.144.**

Recorded here as implemented rather than proposed, because it is already in the
tree and the rest of the design depends on it.

The problem it solved: a key previously carried a single rank-ordered role, so
granting an assistant permission to read files transitively granted managing
members, minting further API keys, reading the audit log, and logging into
sites as a user. Rank ordering means any grant is a grant of everything below
it, which is the wrong shape for a principal that should hold a narrow, oddly
shaped slice.

A key now carries an explicit set, and can additionally be restricted to named
sites. Existing keys are unchanged and keep exactly the access they have today.
The site restriction is the application-layer filter of Decision 4, and the
migration says so in place.

Capabilities that are never available to an assistant are handled by absence
from the registry rather than by a gate inside it. Approving, changing the
grant, minting keys, member management, billing, audit mutation, site deletion,
backup deletion and signing a user into a site are not switches an operator can
turn on. An unregistered capability cannot be reached by a schema-guessing
model or by a future grant bug, and a prefix-based blocklist would not have
caught all of them.

---

## Decision 6 — The tool surface is fleet-level questions, not per-site commands

*Amended 2026-08-23 by amendment A3. The text below is unchanged and its flat
fleet-read set stands. What changed: the revisit threshold is now measured
rather than a round number. A curated per-site tool layer is recorded as an
application of this text, not a change to it. A dispatcher considered for the
same problem is recorded as rejected, not amended in, because it is what this
decision's facade clause forbids by name and because its runtime argument gate
does not sit well with Decision 5.*

**A small flat set of fleet-level tools. No meta-tool facade, no per-site
command mirror of the product's action surface.**

Three reasons, in order of weight.

**Tool-selection accuracy degrades past a few dozen tools.** A surface that
mirrors every per-site action the product can perform runs to hundreds of
entries and makes the model worse at picking the right one, which on a write
path is a correctness problem and not merely an annoyance.

**There are hard limits on response time and output size**, and they bind on
the shape of the answer. Fleet questions are answered from one query over
inventory the control plane holds. Per-site commands are an unbounded fan-out
of round trips to machines that may be slow or offline, against a
first-byte deadline.

**A facade does not solve it.** An earlier draft proposed a search-and-invoke
facade over a long tail of tools. It was cut: a tail defined only by what is
absent from it is not a registry, and rejecting a facade on the grounds that
every action should wear its own name in the client's approval UI, then
shipping a generic invoke tool, is a direct contradiction. Revisit a facade if
the flat set approaches roughly 25 entries.

Two rules that fall out and are worth recording. Every answer carries its own
staleness in the same object as the answer — as-of stamp, count of sites with
stale inventory, list of stale site ids — because a connector that reports a
fleet-wide figure from a three-day-old cache without saying so is the failure
mode. And output is capped in **bytes**, not tokens, truncated at a record
boundary with an explicit marker, never mid-record and never silently: there is
no tokenizer for the client's model on our side and the ratio moves per model.

Shell execution and command-runner tools are excluded from this product line
permanently. We have no operating-system boundary to put around either, and
prompt-level safety is not a boundary.

---

## Decision 7 — Site generation is conceded — REVISITED, PENDING ADR-062

*Revisited 2026-08-23 by ADR-062 (Proposed), via amendment A7. The text below
is kept as written and remains the standing decision: ADR-062 is Proposed, not
Accepted, and a Proposed document supersedes nothing until it is accepted. Its
factual premise survives — Phase 1 writes no content, and no content write path
exists in either process — and the conclusion drawn from that premise is what
[ADR-062](./ADR-062-assistant-surface-phase-2-content-operations.md) argues
against, for when it is accepted. Until then, no Phase 2 content code ships and
this decision governs.*

**Content writes and site generation are not scheduled, and this is a
concession rather than a deferral.**

There is no content model in either process to build on: no post, page, block,
taxonomy or menu write path exists in the control plane or in the agent.
Building one to serve an assistant is a product bet about entering a different
market, not a connector feature, and it should be taken on its own merits with
its own ADR if it is ever taken.

Recording it as conceded rather than "later" is deliberate. A deferred item
gets re-proposed every planning cycle by whoever has not read the reasoning.

---

## Two things that were expensive to learn

**Per-command signing is not AI safety, and must never be marketed as such.**
What the signed-command design actually buys is **forgery and tampering
resistance while the signing authority remains trusted**: a site acts only on
an instruction carrying a valid signature, so a network attacker, an
intercepting proxy, or anything else that is not the signer cannot manufacture
or alter a command, and there is no standing privileged session on the site for
an attacker to ride.

It does **not** protect a site against a compromised control plane or a stolen
signing key. An attacker holding the key issues valid signatures by definition,
and the site cannot tell those from ours — that threat is answered by key
custody, revocation and rotation, not by the signature. An earlier draft of
this ADR said signing protects the site from a compromised control plane. That
was wrong, it was caught in review, and the corrected statement is recorded
here rather than quietly replaced, because the wrong version is the intuitive
one and will be re-derived by the next person who reasons about it quickly.

Either way it says nothing about whether an instruction was a good idea, and it
constrains a model not at all, because a model-proposed action that the control
plane dispatches is signed exactly like any other. Presenting signing as a
constraint on the assistant would be a false claim in the one area where the
product's real claim is unusually strong and checkable. The claim to make is
the approval one.

**The threat model's first item is prompt injection, and the design assumes it
succeeds.** Every string that originated on a managed WordPress site is
attacker-controlled by construction: component names, theme names, site names,
log lines. A compromised site is not an edge case here, it is the population we
are hired to manage. So the design does not attempt to prevent injection and
does not claim to. It constrains blast radius: the model cannot write; the plan
is re-derived from our inventory rather than read from the model's prose; the
reason field is a closed server-side enum that cannot carry a sentence;
site-origin text is stripped and fenced with a per-response nonce before it
reaches the model, and stripped again on a separate path before it reaches a
human; the highest-density carriers — raw file contents, raw log bodies, raw
command output — are not returned by any Phase 1 tool at all.

The corollary, and the metric: the gate that catches a poisoned proposal is a
human declining it, and **approval fatigue is what defeats that gate**. Decline
rate is the primary pilot metric, default focus on an approval screen is
Decline rather than Approve, there is no bulk approve and no select-all, and a
sustained 0% decline rate is read as a kill signal rather than as success.

---

## Consequences

**What this forecloses.**

- No per-site assistant path, now or later, without superseding Decision 1.
  Any future per-site capability is dispatched by the control plane through the
  existing signed-command path.
- No automation may ever approve. This closes off an auto-approve tier, a
  policy engine with approval authority, and scheduled or autonomous runs with
  no human in the loop.
- The approve path cannot later adopt asynchronous or queued audit appends, or
  the best-effort audit convention used elsewhere, without superseding this
  ADR. Approval is unavailable when the ledger is unwritable, by design.
- Assistant principals cannot be made site-scoped at the database by
  configuration; that requires the deferred migration and its own decision.
- Content and page-builder operations are outside this design entirely.

*Three items in that list were amended on 2026-08-23 and must be read with the
Amendments section below. The second sentence of the first bullet — per-site
capability dispatched through the existing signed-command path — is activated
rather than hypothetical (A1). The second bullet is bounded against scheduling:
a scheduler that only proposes is permitted, and nothing scheduled may approve
(A5). The last bullet is revisited, pending ADR-062's acceptance (A7); it
remains true of Phase 1's scope, and stays a statement about the product until
ADR-062's own acceptance narrows it.*

**What it costs.**

- Site scoping in v1 is an application-layer property, which is a weaker
  guarantee than the database-level one an earlier draft claimed, and the
  interface must keep saying so until the deferred migration lands. That copy
  becomes false on the day it ships and is written to be changed in one file.
- Fleet-level tools cannot answer per-site operational questions that need a
  live round trip. Some genuinely useful questions are unanswerable in Phase 1.
- The pilot uses a pasted static credential rather than a delegated
  authorization flow, which skews the cohort toward developer-tool users. We
  will not learn how a non-technical site owner experiences an approval queue
  until that flow exists. This is a real gap, accepted knowingly, and it is why
  that flow is next rather than distant.
- Every tool output crosses the tenant boundary into a third party's inference
  stack. For an agency managing other people's client sites this is the first
  procurement question.

**What has to exist before v1 ships.**

- The one-chokepoint site resolver, with the registry test that asserts every
  tool goes through it.
- The two sanitizers — one for text on its way to the model, one for text on
  its way to a human — with a planted hostile-name test on the approval
  payload.
- The proposal state machine with the conditional approve, plus proofs that the
  self-approval refusal and the wrong-credential-class refusal actually fire:
  plant the failure, watch it go red, restore, watch it go green, both outputs
  pasted with their commands.
- The approve path's single transaction, with a proof that a forced ledger-write
  failure leaves the request pending and dispatches nothing. This one needs the
  fault injected rather than reasoned about, since the failure it guards against
  only appears when the append genuinely fails.
- Off by default at the tenant level, and a connection whose site allowlist
  starts empty, so a credential leaked from a half-configured connection reads
  nothing.
- A per-tenant kill switch reachable in one click, and a data-classification
  statement naming exactly which fields can leave.
- The rate limiter and quota counter, and a documented refusal when the pending
  queue is capped.
- Agreed kill criteria for the pilot, written down before it starts.
- `make test-integration` run locally before merge. `ci.yml` does not run that
  package, and it is where the tenancy proofs live.

**Verification note.** A deploy that reports success is not evidence the
surface exists. The assistant route must answer 401 rather than 404 to an
unauthenticated request against the deployed revision, because a missed
constructor leaves every route 404 while the build and the package tests pass.

---

## Amendments (2026-08-23)

A design review over the whole assistant surface — the decisions above, the
published Phase 1 and Phase 2 design surfaces, and the agent's actual command
surface — found that this record contradicts our own published design work in
four places, decides three things it never states, and rests one argument on a
premise that a later decision had already discharged.

**These are amendments, not a rewrite.** Every decision above keeps its original
text. This section records what changed and why, in the pattern `CLAUDE.md`
already uses for its own corrections: the wrong or incomplete version stays
visible, because the next person to reason about it quickly will re-derive the
same version and needs to find the correction rather than the argument.

**What these amendments do not change.** Decisions 2, 3, 4 and 5 are untouched.
So is *Two things that were expensive to learn*, and in particular the corrected
statement at the top of it: per-command signing gives **forgery and tampering
resistance while the signing authority remains trusted**, and does **not**
protect a site from a compromised control plane or a stolen key. Nothing in A2
or A4 below is a claim about the signing key, and nothing below turns signing
into a safety property it does not have.

---

### A1 — Decision 1 stands. The fan-out argument is scoped, and the per-site clause is activated

Decision 1 gave two arguments and they have different reach.

**The credential argument (Decision 1, paragraph 3) is untouched and is the
load-bearing half.** A per-site *inbound* assistant path would still require a
privileged WordPress user and a standing credential on every managed site,
multiplied by the size of the fleet. Nothing here reopens that, and no amendment
below creates an inbound path to a site.

**The fan-out argument is narrower than it reads.** Its subject is *fleet
questions* — "Sending them to individual sites turns one Postgres query into an
unbounded fan-out". The antecedent is fleet-aggregate queries, and the argument
was never applied to a targeted single-site action, which is a different traffic
shape: one round trip to one named site, not N.

Decision 6's "Per-site commands are an unbounded fan-out of round trips" looks
like a counter-example and is not, for a reason neither decision noticed.
"Unbounded" describes a tool whose site parameter is a *set*. And more
decisively: **under Decision 2 a per-site tool call does not execute anything.**
It writes a proposal row. Its cost at tool-call time is one INSERT and zero
network round trips. Dispatch happens afterwards, on a worker, after a human
approves, outside any first-byte deadline. Decision 6's response-time argument
assumes the tool call executes; it does not, and that assumption is the only
thing holding it up.

**What this activates.** The second sentence of the first Consequences bullet —
"Any future per-site capability is dispatched by the control plane through the
existing signed-command path" — was written as a hypothetical. It is hereby the
sanctioned and only route: per-site capability reaches a site as a signed command
dispatched by the control plane after a human approval, and never as an inbound
connection to the site. A future per-site capability therefore does **not**
require superseding Decision 1. Opening an inbound path still does.

### A2 — The agent already has a large action surface, and this record reads as though it does not

Measured in the working tree at the time of writing, from `apps/agent`:

```sh
ls includes/commands/*.php | wc -l
#   68

grep -rhoE 'command/[a-z0-9_]+' includes/commands/*.php \
  | sed 's#command/##' | sort -u | wc -l
#   55
```

Sixty-eight command classes implementing fifty-five distinct signed command
names ship today, over the outbound signed-command channel: the filesystem
surface, database maintenance, search-and-replace, media, cache, backup, restore,
rollback, scan and update. Re-run both commands rather than citing these
figures; they move with the plugin.

This corrects a **reading**, not a decision. "The agent plugin does not move for
this feature" is a statement about Phase 1 and stays true — Phase 1 adds no
command. But the surrounding framing reads as though the agent has no action
surface at all, and every downstream design artifact inherited that reading. The
consequence is the single largest correction in the review: for most per-site
work an assistant might propose, **what is missing is model-facing exposure, not
capability.**

Decision 7's premise — "no post, page, block, taxonomy or menu write path exists
in the control plane or in the agent" — is about *content* write paths
specifically and remains true. It was re-verified against the same command's
output this pass: no name in the set is a content verb.

One property of that channel is worth recording here because A3 and A4 lean on
it. Each command is authorized by its own short-lived Ed25519 bearer token bound
to a single command name and a single site, so a token minted for one command
cannot invoke another (`apps/api/internal/agentcmd/jwt.go`); confirmation for a
dangerous write is a body field the agent enforces server-side rather than a
prose instruction; and a file write that would overwrite an existing file copies
it to a staging area first (`apps/agent/includes/commands/class-file-write-command.php`).
That is a description of the wire contract. It is not a claim about the signing
key, and the paragraph above still governs what signing does and does not buy.

### A3 — Decision 6: a measured threshold, and why the per-site case does not need a facade

Decision 6's flat fleet-read set is right and stays. One thing about it is
corrected because the wording invites a bad measurement. One design considered
against it — a dispatcher — is recorded here as rejected rather than left as a
question a later reader has to re-open.

**The threshold is measured, and it is measured against `tools/list` size
specifically.** "Revisit a facade if the flat set approaches roughly 25 entries"
is replaced by: *revisit at the measured point at which tool-selection accuracy
degrades, measured against the size of the list the client actually receives,
re-measured per release.* Two reasons. Registry size and `tools/list` size are
different quantities and the original conflated them — a registry of hundreds
behind a handful of exposed tools does not test the exposed-list claim at all.
And a round number in an ADR is a guess wearing a decision's clothes; `CLAUDE.md`
already requires that a performance question be settled by measurement before a
fix, and tool-selection accuracy is that kind of question. Until the measurement
exists, the flat set ships as Decision 6 describes it, and instrumenting
tool-selection correctness is a Phase 1 deliverable. ADR-062's own acceptance
checklist (item 10) already defers its tool-surface question to this same
measurement rather than inventing a number of its own; this amendment is what
makes that measurement exist to defer to.

**A small curated set of per-site tools, named by intent, is not an amendment to
the text above — it is that text applied.** Decision 6 forbids two specific
things: a facade, and "a per-site command mirror of the product's action
surface." Neither describes a handful of tools, each wearing its own name, each
resolving to one or more signed commands, added to the same flat list the
fleet-read tools already occupy and counted against the same threshold. Three
things make this a reading of Decision 6 rather than a change to it:

- **The fan-out argument does not bind it.** Amendment A1 above establishes
  that a per-site tool call, under Decision 2, costs one `INSERT` and zero
  network round trips — dispatch happens later, on a worker, after a human
  approves. Decision 6's response-time reason is about a live fan-out to sites
  at call time; a proposal write is not that.
- **A curated handful is not a mirror.** Amendment A2 above measures the actual
  per-site action surface at 55 distinct signed command names, most of it
  operator plumbing — inventory refresh, cache preload queueing, agent
  self-update — that no model should be asked to choose between. Naming a few
  intent-shaped tools that each resolve to specific commands is the opposite of
  mirroring that surface one for one.
- **It is still bounded by the same number.** Whatever a per-site tool adds, it
  adds to the one list `tools/list` returns and the one threshold above
  governs. Naming it a distinct layer does not change which budget it draws
  against or exempt it from the measurement.

No decision text needs to change for this, and none does. It is recorded here
because A1's economics, Decision 6's budget, and ADR-062's Phase 2 content
tools are three different documents leaning on the same assumption, and this is
where a reader who has only one of them open will look.

**A dispatcher was considered for the same problem and is not adopted.** The
shape considered — one tool taking an action argument, constrained by an enum
of what a given site can currently do — was proposed as a release valve for if
the curated tools above ever needed to grow past the measured threshold. It is
rejected here rather than carried forward provisionally, for two independent
reasons, either of which is sufficient on its own.

First, it is exactly what Decision 6 forbids by name: "no meta-tool facade."
Keeping it would mean amending that clause outright, not appending an option
beside it, and this document does not do that. The half of Decision 6's facade
paragraph about "a tail defined only by what is absent from it is not a
registry" would not even apply to what was considered, since it was positively
enumerated rather than an open tail; what would need to be argued down is the
other half — that rejecting a facade on the grounds that every action should
wear its own name in the client's approval UI, then shipping a generic invoke
tool, is a direct contradiction. Decision 2's disjoint-credential architecture
is a real answer to that argument: the client's tool-approval prompt is not
where this product's approval decision lives, so the argument does not bind an
architecture that has a different, stronger gate the way it binds one that has
no other gate at all. That is worth recording for whoever revisits this, but it
is not exercised here, because the second reason does not go away even if the
first is answered.

Second, and this is the reason with no comparably clean answer: **an argument
enum generated at request time from a site's capabilities is a gate evaluated
inside the dispatcher, and Decision 5 draws exactly that line — a capability
that must never be reached is handled by absence from the registry, not by a
gate inside it.** Being positively enumerated does not get a dispatcher past
this: the enumeration fixes which actions exist at all, and the gate is the
separate thing that decides, per call, which of them this site and this
proposal are allowed to reach. Making that gate correct is a second mechanism
doing the safety job Decision 5 already assigns to the registry itself, and a
second mechanism doing a job the first one already owns is exactly the shape
Decision 5 was written to avoid — a schema-guessing model or a future grant bug
now has two places to get past instead of one place with nothing to get past.

Neither reason forecloses a dispatcher forever. Both are enough to not build one
now, for a problem the measurement above has not shown exists yet: the curated
per-site tools are nowhere near the threshold this amendment just defined, and
ADR-062's own item 10 is explicit that how content tools join this surface is
open until Phase 1 is serving traffic and the measurement can be taken. If that
measurement ever does force the question, the amendment that answers it has to
resolve the Decision 5 tension on its own terms — a gate that only appears in an
ADR's prose is not a gate that appears in the code — and it has to say plainly
that it is amending Decision 6's facade clause, not reading around it the way
this section declined to.

**Unchanged by this amendment:** shell execution and command-runner tools
remain excluded from this product line permanently. Nothing here is a route
back to them, considered or otherwise. The byte-capped, record-boundary output
rule is also unchanged.

### A4 — Concurrency: a per-organization ceiling and a per-site exclusive lock

ADR-061 was silent on concurrency, and two published surfaces filled the silence
differently — one rendering several operations running at once, another
disabling an action because another operation holds its sites. Both are correct
under one rule, and nothing said so.

**Two mechanisms, both in force.**

1. **A per-organization fleet-wide ceiling on concurrently dispatching steps,
   default 4.** It is configured as the `MaxWorkers` of the assistant dispatch
   River queue, sharded per tenant the way `apps/api/internal/update` already
   shards its task queue (`const tenantQueueShards = 8`,
   `apps/api/internal/update/worker.go:60`), so one organization's burst fills
   only its own shard and cannot starve another tenant. The default of 4 is a
   starting value, not a measurement: it is the same bounded default the RUCSS
   queue already uses (`apps/api/internal/rucss/worker/worker.go:405`), and it is
   expected to move once real dispatch load exists. It is stated here so that
   there is a number with a decision behind it.
2. **A per-site exclusive advisory lock, held for the duration of that site's
   step.** `pg_advisory_xact_lock(hashtext(<namespace>), hashtext(<site id>))`,
   released by commit or rollback — the same discipline
   `apps/api/internal/update/agent_repo.go` and `apps/api/internal/org` already
   use, and not a new locking mechanism.

The ceiling bounds fleet-wide load. The lock is what makes "another operation
holds these sites" true without taking a global lock, so unrelated sites keep
moving while one site is busy.

**The unit of the ceiling is the per-site step, and "runs" and "operations" are
not synonyms for it.** A step is the smallest unit of work that touches one site;
it is what occupies a worker and makes one outbound request, so it is what
`MaxWorkers` counts and the only unit in which a concurrency limit means
anything here. **A run is not capped.** An organization may have any number of
multi-site runs in flight; what is capped is how many of their steps dispatch at
once. Counting the ceiling in runs would understate fleet load by roughly the
size of the fleet — one run across forty sites is a single run and forty steps,
so a ceiling of four *runs* permits up to forty concurrent site dispatches while
reading on screen as four.

The conversion between the two, since it is meaningful: **a run over N sites
contributes at most `min(N, ceiling)` concurrently dispatching steps, and never
more than one step per site at a time**, because mechanism 2 serializes a site's
own steps whatever the ceiling allows. Every surface that renders this limit
renders it in steps and uses that word: three nouns for one limit is how the
withdrawn figure above survived as long as it did.

Reads are freely concurrent and take no lock. A proposal is an INSERT and is
concurrency-safe, which is A1's point restated: the thing that needs serializing
is dispatch, and dispatch happens after approval.

**Recorded so it is not cited again: the "3 concurrent" figure in the Phase 1
design surfaces is mock copy.** No decision stood behind it; it was invented to
fill a wireframe. It is not the ceiling, and the surfaces render whatever this
decision sets.

### A5 — A scheduler that only proposes is permitted; nothing scheduled may approve

"No automation may ever approve" is unchanged and absolute. It does not forbid
automation that only proposes.

A scheduler may assemble a proposal on a timetable and enqueue it. That proposal
enters the same queue, is rendered on the same approval surface, and waits for
the same human decision as any other. Nothing scheduled may approve; no timeout,
escalation or policy may approve; and a run that waits forever waits forever,
visibly. The operative words in the foreclosure are "with no human in the loop",
and without this sentence the bullet reads as foreclosing scheduled dispatch
entirely, which was not the decision.

### A6 — The protocol is MCP, and this record now says so

This is a decision record for a connector that never named its protocol.
Before this amendment,
`grep -ric 'MCP\|Model Context Protocol' docs/adr/ADR-061-assistant-surface-phase-1.md`
returned `0`, while the published Phase 1 design surfaces already show users an
`/mcp` endpoint on the app host. The UI encoded a protocol decision the ADR did
not record. Closing that gap:

- **Protocol: MCP (Model Context Protocol).**
- **Target version: 2025-11-25.** This is what we implement against and what a
  current client negotiates. It is corroborated against the WordPress project's
  published MCP schema package, whose typed protocol DTOs are described as MCP
  2025-11-25.
- **Floor: 2025-03-26, and it is a floor rather than a preference.** Below it the
  protocol drops fields the approval flow depends on, and the approval flow is
  the product — ADR-061 exists to make "a named human approved this" checkable,
  and a negotiated version that cannot carry the fields that claim rests on
  cannot serve it. A compatibility window is therefore bounded below by
  2025-03-26 and not by whatever the oldest client in the field happens to speak.
- **Below the floor: refuse, and say why.** Version negotiation happens at
  `initialize`; a version below the floor is refused rather than silently
  downgraded into unspecified behaviour, and the refusal reaches the operator as
  the reason rather than as a bare version mismatch — this client speaks an older
  revision of the protocol, that revision cannot carry the approval fields, and
  the remedy is a newer client. A version-mismatch error the operator cannot act
  on is the same defect as a silent downgrade, arriving one screen later.
- **Transport: Streamable HTTP**, on the existing API host, inside the existing
  TLS boundary and auth path. That is Decision 1's consequence restated: one
  origin, one boundary. There is no stdio transport, because there is nothing on
  a customer machine for us to run.
- **Resources: out of scope for Phase 1.** Every fleet answer is a tool result
  that carries its own staleness stamp in the same object as the answer, and a
  resource has nowhere to put one.
- **Prompts: out of scope for Phase 1.**
- **Elicitation and sampling are never on the approval path.** Neither is a
  substitute for the Decision 2 surface, and an elicitation round trip that
  looked like an approval would produce precisely the ambiguity Decision 2 exists
  to remove — a confirmation the model could have satisfied itself.
- **Server instructions are delivered in the first tool result rather than
  relying on the `initialize` handshake, and prepended rather than appended.**
  Client handling of handshake instructions varies, and where an instruction
  budget is capped it is the tail that gets cut.

### A7 — Decision 7 is revisited by ADR-062, and will be superseded when ADR-062 is Accepted

Decision 7 conceded content writes and site generation, and recorded the
concession as permanent specifically so it would not be re-proposed each planning
cycle by someone who had not read the reasoning. The reasoning is what this
amendment challenges — it is not, itself, what retires the decision.

Its premise was a fact about this tree — no content write path exists in either
process — and that half is still true (A2). What Decision 7 drew from it was a
*market* conclusion: that building one is "a product bet about entering a
different market". That conclusion is what does not survive the argument in
ADR-062: fleet content operations are designed there against exactly the locked
decision that forbids them, and the technical objection underneath the
concession rested on a test that was the wrong test, which ADR-062 states and
replaces.

**Decision 7 is not superseded yet. It is revisited by
[ADR-062](./ADR-062-assistant-surface-phase-2-content-operations.md), and will
be superseded when ADR-062 is Accepted — not before.** ADR-062 is **Proposed**
today, its own acceptance checklist carries open items, and a Proposed document
does not retire a standing Accepted decision; only its own acceptance does.
Until then, Decision 7's text above remains the standing decision, "conceded
rather than deferred" included, and no Phase 2 content code ships. What this
amendment records is the argument that will retire it on acceptance, so a
future reader is pointed at ADR-062 rather than left to re-derive why the
concession is being reconsidered at all.

The Consequences bullet "Content and page-builder operations are outside this
design entirely" is under the same condition. It remains an accurate statement
of *this* ADR's scope — Phase 1 writes no content — and it remains a statement
about the product as a whole until ADR-062 is Accepted; ADR-062's own
Consequences section says plainly that its acceptance, not its existence, is
what retires that framing.

### A8 — Added to *Two things that were expensive to learn*

**"Prompt-level safety is not a boundary" now has external corroboration, and
the strongest kind.** The exclusion of shell execution and command-runner tools
was recorded above as reasoning. A shipping product in this category exposes
arbitrary code execution inside the WordPress process to a model, and the entire
safety mechanism around it is a short prose instruction asking the model to
behave. Its own published documentation states that the containment appearing to
surround that feature is not a security boundary, and that code execution passes
straight through it.

A vendor documenting the limit of its own containment, on its own flagship
feature, is better evidence than any argument we could construct: it is the
claim's author conceding it. The exclusion stands unamended, and nothing in
A3 — including the per-site dispatcher considered and rejected there — is a
route back to it.

---

## Amendments (2026-08-24)

A second review pass — over the whole assistant surface, the connection
experience, the tool and capability model, the skill and prompt content model,
the client setup formats and this repository's own licence position — produced
five further amendments. A9 is the most consequential: it fixes an **ordering**
inside Phase 1 that this record left free, and the ordering is not a preference.

The 2026-08-23 amendments above stand unchanged, and so does every decision above
them. As before, the earlier text is not rewritten; the correction is recorded
next to it.

### A9 — Sequencing inside Phase 1 is fixed by ADR-060's freeze clause, and this is the most consequential sequencing fact in the plan

ADR-060 carries one absolute prohibition, and it is the only absolute in that
record:

> No new externally-reachable surface ships while an auth-boundary item is open.

Three facts meet in Phase 1 and the conclusion follows from them without judgement
being involved:

1. **The MCP endpoint is a new externally-reachable surface.** A9 does not need to
   argue this; A6 already records the protocol, the transport and the fact that
   the endpoint answers on the existing API host. Something that a third-party
   client connects to from the public internet is the case the clause is about.
2. **The site-scope chokepoint is an auth-boundary item, and it is open.**
   Decision 4 records that the application-layer chokepoint is the boundary in v1,
   and the chokepoint does not exist yet. A10 and A11 below say what is left.
3. **The audit posture on this surface is an auth-boundary item, and it is open.**
   The audit path is documented as best-effort and fails open by default, and A10
   changes that for this surface. Until the helper exists, the surface's own
   record of who was denied what is not reliable, which is the boundary's evidence.

**Point 1 no longer rests on this document's reasoning alone.** ADR-060's own
Amendment A1 (2026-08-27) has since defined "externally-reachable surface" for
the freeze clause directly, prompted by this same question being answered
independently by two different documents. Worked through as one of that
amendment's own edge cases, the assistant route lands on the new-surface side by
the caller-class ground alone: nothing answering to the agent-kind key Decision 2
defines for the assistant proposer was reachable on the API host before this
endpoint shipped, which is by itself sufficient under that test. A future reader
should cite ADR-060 Amendment A1 for what counts as a new surface in general, and
treat point 1 above as this route's specific instance of it rather than as the
general rule.

**Therefore the Phase-1 gate work closes before the MCP endpoint ships.** Not
"should", not "ideally in the same release" — the clause is absolute and it is
narrow precisely so that it can be held rather than informally suspended. Anything
that renders a Phase 1 plan renders the gate work ahead of the endpoint, and a
plan that shows them shipping together is wrong on its face rather than optimistic.

**Why this needs stating at all.** The connection wizard, the endpoint and the
capability hub are the visible part of Phase 1 and the chokepoint is not visible
at all. That asymmetry is exactly the pressure ADR-060 exists to resist, and it
resolves silently in the visible direction unless the ordering is written where a
planner will hit it.

### A10 — Audit is fail-closed for this surface, and opting in is deliberate

**On the assistant surface the audit trail is the point: every AI-originated
read, proposal, approval, denial and execution writes its record in the same
transaction as its effect, and if the record cannot be written the operation
fails — no AI action is ever performed whose record was lost.**

The existing write helper is documented as **best-effort and fail-open**: callers
are told to log an audit error and continue, *except where the audit trail is
itself the point*. That default is right for the rest of the product, and there is
no existing helper that implements the exception — the fail-closed branch exists as
a sentence in a doc comment and not as a function.

Three things follow, and each is work rather than a caveat:

- **The helper has to be built**, and it becomes the only audit path this surface
  may use. A surface that can reach the fail-open helper will eventually reach it.
- **Reads are included.** *Which sites a connection read, and when* is the record
  a customer needs when they ask what the assistant saw. Reads may be batched or
  sampled for volume, but the batching rule is written down with a stated
  retention. An omission dressed as an optimisation is how a read log becomes
  absent.
- **The chain lock becomes a throughput ceiling, and it must be measured.** Audit
  appends take a per-tenant advisory lock. Fail-closed plus a per-tenant
  serialisation point means audit contention bounds this surface's throughput,
  under A4's per-organization step ceiling. That is the correct trade and it is
  still a number somebody has to take, before production takes it for us.

This is an opt-in to a stricter posture than the codebase's default, chosen for
one surface, and named as such so that nobody later reads the difference as an
inconsistency to be tidied away.

### A11 — Site scoping is in v1, as an application-layer chokepoint, and it must be built rather than assumed

Decision 4 is unchanged and this amendment does not soften it. What it adds is
that site scoping is **in v1** — an organization-wide assistant key is not a
product that can be sold to an operator who answers to someone else for the sites
they touch — together with the specific work that makes it true.

The current state is that the capability-set migration **stores** a site scope and
a site allowlist on an API key and **enforces neither**; its own header says so.
Storage is not a boundary. Six things:

1. **The chokepoint.** One function taking the principal and the requested site
   id, returning either a scoped context or a typed denial. It is the only way
   this surface names a site.
2. **It uses the scoped transaction helper, not the plain tenant one**, so the
   restrictive site-scope policies that *do* exist actually engage. Today that
   helper has a handful of call sites; Decision 4 gives the commands for counting
   them and they are the commands to re-run, not the figures to quote. This
   surface making it the default is also the honest way to prove it works.
3. **A scope of "site" with an empty allowlist resolves to zero sites, not all
   sites.** The scope column exists precisely so that "restricted to nothing" is
   expressible, and a test proves it fails closed. This is the single most likely
   place for a fail-open default to survive review.
4. **A containment test that fails CI.** No handler on this surface may take a
   site id from a request and pass it anywhere but the chokepoint. Plant a bypass,
   watch it go red, restore, watch it go green, paste both outputs with their
   commands.
5. **Every denial is audited**, under the established action / action-denied
   naming convention, through A10's fail-closed path.
6. **The proof runs as the role every install runs as, through the same code path
   production uses.** A proof that opens its own connection leaves the policies
   inert while every test passes, which has already happened in this repository
   once.

**Reaffirmed, and it binds every artifact and not only this one:** database-level
site scoping for API-key principals is **v2**. The v1 boundary is this
application-layer chokepoint plus the restrictive policies that already exist.
**No document, wireframe, marketing page or reviewer-facing summary may imply the
database enforces it.** The presence of a scope column and an allowlist column is
not a boundary, and the most likely way this goes wrong is a reviewer reading the
columns as one.

### A12 — Protocol: three header cases the code must not conflate, and logging from the first request

A6 fixed the target at 2025-11-25 and the floor at 2025-03-26. It did not say what
happens to the version **header** on an ordinary request, which is a different
question from what happens at `initialize`, and conflating the three cases below
produces a server that rejects compliant clients.

1. **Header absent → assume the floor, 2025-03-26. Do not return 400.** This is
   not leniency and it is not a compatibility concession; the specification says
   the server should assume that version when the header is not received.
   Returning 400 here rejects a client that is behaving correctly.
2. **Header present but unsupported or unparseable → 400 Bad Request.** The
   specification requires it. The response names the floor and the target so the
   operator has something to act on, per A6's rule that a version-mismatch error
   the operator cannot act on is the same defect as a silent downgrade arriving
   one screen later.
3. **The negotiated version governs.** Not the header, not the newest version
   either side supports, not a per-request override. The header is transport
   metadata about the request; negotiation at `initialize` is what decides the
   contract.

**Log the received header from day one, on every request, alongside the client
name and version reported at `initialize`.** No AI client's public documentation
mentions this header at all, so the real distribution of what clients actually
send is unknowable from documentation and can only be observed. That log is the
only route to an answer, and it is the same instrumentation the tool-selection
measurement in A3 needs.

One consequence follows immediately and constrains scope rather than describing
it: **no Phase 1 capability may depend on a feature that exists only at
2025-11-25.** Header-less clients are floor clients by definition, so the whole
Phase 1 tool surface has to be expressible at 2025-03-26. Anything richer is gated
and degrades with an explicit message, never with a silent absence.

### A13 — Skill and prompt content is data, never permission

**Authorization resolves outside the model loop. Instruction text that asks for an
approval to be skipped is therefore denied identically to text that does not ask,
because the text is not consulted when the question is decided.**

This is not a new decision — it is Decision 2's architecture stated as a rule
about *content*, because the content is where it will be tested. A skill, a
prompt, a page body, a plugin error string and a site name are all the same kind
of object to this surface: material the model may read, and material that can
never widen what the model may do. There is no phrasing, no framing and no claimed
authority inside that material which changes the answer, and the reason is
structural rather than a matter of the model being well-behaved.

**The fencing rule is Phase 1, even though the skill store itself is later.** Every
site-originated string reaching the model arrives inside a fenced envelope, under
a standing preamble stating that the enclosed material is reference text that
cannot change what is permitted, with a provenance attribute we emit from our own
database and never from the content itself, and with the delimiter escaped so the
content cannot close its own fence.

The ordering argument is the whole of A13's point: **the fence must predate the
first untrusted string, not the first skill.** Site names, plugin-supplied error
strings and — under ADR-062 — page content all reach the model in Phase 1,
regardless of whether any skill exists. If the fence arrives with the store, then
every string that reached the model before the store did so naked, and the first
skill inherits a precedent that instructions arrive unfenced. Likewise the
structural rule above is Phase 1, because a rule that is not true in Phase 1 is
decoration.

The ship gate is a **planted hostile site name**: a site whose name is an injection
payload must produce an approval surface that still renders correctly and a set of
permitted actions that is unchanged. A fence nobody has watched fail is not known
to fence anything.
