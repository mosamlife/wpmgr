# ADR-061 — Assistant surface, Phase 1: control-plane server and out-of-band approval

**Status:** Accepted (amended 2026-08-23) · **Date:** 2026-08-22
**Supersedes/relates:** ADR-060 (this work sits in its "constrained automation" phase, ahead of differentiation), ADR-057 (unaffected; the capability model here is a separate axis from per-site security policy), ADR-062 (Proposed — supersedes Decision 7 below, per amendment A7).

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
superseded by ADR-062, and four items ADR-061 was silent on are decided.

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
rather than a round number, a three-layer registry is recorded, and the second
of the three arguments against a facade is retired as not binding on this
architecture.*

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

## Decision 7 — Site generation is conceded — SUPERSEDED

*Superseded 2026-08-23 by ADR-062 (Proposed), via amendment A7. The text below
is kept as written and is no longer the standing decision. Its factual premise
survives — Phase 1 writes no content, and no content write path exists in either
process — but the conclusion drawn from that premise does not. Read
[ADR-062](./ADR-062-assistant-surface-phase-2-content-operations.md) instead;
until ADR-062 is Accepted, no Phase 2 content code ships.*

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
(A5). The last bullet is retired (A7); it remains true of Phase 1's scope and is
no longer a statement about the product.*

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

### A3 — Decision 6: a measured threshold, a three-layer registry, and one retired argument

Decision 6's flat fleet-read set is right and stays. Three things change.

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
exists, Layer A ships and instrumenting tool-selection correctness is a Phase 1
deliverable.

**The registry has three layers, and the boundary between them is enforced by
what is registered rather than by a flag.**

- **Layer A — flat fleet-read tools.** Decision 6 as written, unchanged. A small
  set, each wearing its own name, each answered from control-plane inventory with
  its own staleness stamp in the same object as the answer.
- **Layer B — curated per-site *proposal* tools, named by intent rather than by
  command.** Not a mirror of the command surface. Most of that surface is
  operator plumbing — inventory refresh, cache preload queueing, agent
  self-update, object cache and performance config — that no model should ever be
  asked to choose between. A Layer B tool resolves to one or more signed
  commands, and it wears its own name **on our approval surface**, which renders
  the resolved command names from control-plane-derived facts and never a generic
  "execute".
- **Layer C — at most one dispatcher, and only if Layer B exceeds the measured
  cap.** Permitted only over a positively enumerated, per-site-gated registry
  whose argument is constrained by an enum generated from what a given site
  actually has. Never over an open tail.

**The facade objection is retained in one half and retired in the other.**
Decision 6 cut the facade on two grounds.

The first — "a tail defined only by what is absent from it is not a registry" —
is **retained**, and it is exactly why Layer C is confined to a positively
enumerated registry with a generated argument enum. A facade over an enumerated,
per-target-gated, runtime-verified registry is a different object from a facade
over an undefined tail, and only the second is what that sentence rejects.

The second — "every action should wear its own name in the client's approval UI"
— is **retired as not binding on this architecture**, and the reasoning matters
more than the conclusion. That argument protects a property Decision 2 does not
rely on. Decision 2 removed the client's approval UI from our trust model
entirely: approval happens on a control-plane surface the model cannot reach, the
proposing and approving credential classes are disjoint, and the confirmation
phrase is never sent to the model. In an architecture whose *only* gate is the
client's tool-approval prompt, a generic invoke tool is a real and serious cost —
the operator is shown one tool name standing in for every action the surface can
take, and that is the whole of what they get to approve. We are not that
architecture. The name that carries our approval decision is the one **our**
surface renders, and Decision 3 already requires that surface to derive it from
inventory rather than from anything the model sends. The argument is sound; it
binds a different product.

**Unchanged by this amendment:** shell execution and command-runner tools remain
excluded from this product line permanently, and Layer C is not a route back to
them. The byte-capped, record-boundary output rule is also unchanged.

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
- **Target version: 2025-11-25**, with a compatibility window for clients that
  negotiate an earlier version. Version negotiation happens at `initialize`, and
  a version we cannot serve is refused with a named error rather than silently
  downgraded into unspecified behaviour.
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

### A7 — Decision 7 is superseded by ADR-062

Decision 7 conceded content writes and site generation, and recorded the
concession as permanent specifically so it would not be re-proposed each planning
cycle by someone who had not read the reasoning. The reasoning is what failed.

Its premise was a fact about this tree — no content write path exists in either
process — and that half is still true (A2). What Decision 7 drew from it was a
*market* conclusion: that building one is "a product bet about entering a
different market". That conclusion does not survive. Fleet content operations are
already designed in this product's own published Phase 2 design surfaces, against
a locked decision that forbids them; and the technical objection underneath the
concession rested on a test that was the wrong test, which ADR-062 states and
replaces.

**Decision 7 is superseded by
[ADR-062](./ADR-062-assistant-surface-phase-2-content-operations.md).** ADR-062
is **Proposed**, not Accepted, and no Phase 2 content code ships until it is
accepted. "Conceded rather than deferred" is retired and replaced by a scoped
commitment recorded there.

The Consequences bullet "Content and page-builder operations are outside this
design entirely" is retired with it. It remains an accurate statement of *this*
ADR's scope — Phase 1 writes no content — and it is no longer a statement about
the product.

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
claim's author conceding it. The exclusion stands unamended, and A3's Layer C is
not a route back to it.
