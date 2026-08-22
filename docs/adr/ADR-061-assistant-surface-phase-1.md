# ADR-061 — Assistant surface, Phase 1: control-plane server and out-of-band approval

**Status:** Accepted · **Date:** 2026-08-22
**Supersedes/relates:** ADR-060 (this work sits in its "constrained automation" phase, ahead of differentiation), ADR-057 (unaffected; the capability model here is a separate axis from per-site security policy).

This ADR records the locked Phase 1 design for the assistant surface: an
endpoint on the control plane that lets an AI assistant read a fleet and
propose work, where nothing that changes a site runs without a human decision
taken in a channel the model cannot reach.

It records the decisions and the reasoning behind them, not the build plan.
**Phase 2 — content operations and page-builder work — is out of scope for
this record and gets its own ADR.** Nothing here should be read as approving
any part of it.

---

## Context

An assistant connector is table stakes and it is commoditising. Connecting a
model to a site-management product is a few weeks of work for anyone who wants
to do it, and the number of products that have done it grows every quarter. A
connector is therefore not a moat, and building one because others have is a
plan to arrive second at a place with no prize.

What is not commoditising is the part every connector defers: proving that a
**person** authorised a change. The market position worth taking is not "our
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
been steered by injected content, its summary is precisely the artefact not to
trust, and putting it where the decision gets made hands the attacker the one
surface that converts a sentence into authority over a fleet.

So the model supplies a structured selection — site ids, component slugs,
target versions — and nothing else. The control plane re-derives every
human-readable line from inventory it holds, and flags downgrades and
major-version bumps **from its own data**, regardless of what the model
claimed. The proposal record carries no free-text column the proposer controls,
on the parent table or on its children, because if such a column existed
something would eventually render it.

The quarantine block is kept rather than removed. An operator with no statement
of intent approves on shape alone, and a short note genuinely helps answer "is
this the thing I asked for". But it is demoted, plain text, capped, escaped,
labelled with what it is and what it is not, and it never appears in the title,
the summary, the notification, or the audit row.

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

**Assistant principals are organisation-scoped at the database. The
connection's site allowlist is an application-layer filter through one audited
chokepoint, and the interface describes it in exactly those terms.**

An earlier draft claimed site-scoped row-level security was an existing, proven
property of this codebase that the feature could simply inherit. That claim is
false, and recording why is the point of this section.

The site-scope policies exist, but they are activated only by the transaction
helper that sets the scope GUC, and almost nothing calls it. Rather than pin a
number that drifts, here are the commands; run them and compare the two
results:

```sh
cd apps/api
grep -rn "InScopedTenantTx(" --include="*.go" internal/ | grep -v _test | wc -l
grep -rn "\.InTenantTx("     --include="*.go" internal/ | grep -v _test | wc -l
```

The first is a handful of call sites; the second is the rest of the
application. On every path in the second group the site-scope policies are
inert, because their predicate short-circuits when the GUC is unset. This
number has already moved once during the drafting of this ADR, which is the
argument against writing it down.

Worse for this feature specifically, the two tables a fleet assistant most
needs carry no site-scope policy at all:

```sh
cd apps/api
grep -hoE "CREATE POLICY [a-z_0-9]+ ON (site_vulnerabilities|update_runs)\b" db/schema.sql | sort -u
```

That returns only the tenant-isolation and cross-tenant worker policies for
each table. There is no site-scope policy to activate.

There are also fail-open shapes for a principal whose scope is the zero value,
in the transaction dispatcher, in the site-access middleware, and in the
principal's own access predicate. An assistant principal that was
site-scoped-in-name-only would land on those paths.

Given that, manufacturing database-level per-site isolation for a non-human
principal inside this release is not available. The honest design is the one
taken: organisation scope at the database, and one Go chokepoint that resolves
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

## Decision 7 — Site generation is conceded

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
The signed-command design protects a **site from a compromised control plane**:
the site verifies that an instruction genuinely came from us before acting on
it. It says nothing whatever about whether the instruction was a good idea, and
it constrains a model not at all, because a model-proposed action that the
control plane dispatches is signed exactly like any other. Presenting signing
as a constraint on the assistant would be a false claim in the one area where
the product's real claim is unusually strong and checkable. The claim to make
is the approval one.

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
- Assistant principals cannot be made site-scoped at the database by
  configuration; that requires the deferred migration and its own decision.
- Content and page-builder operations are outside this design entirely.

**What it costs.**

- Site scoping in v1 is an application-layer property, which is a weaker
  guarantee than the database-level one an earlier draft claimed, and the
  interface must keep saying so until the deferred migration lands. That copy
  becomes false on the day it ships and is written to be changed in one file.
- Fleet-level tools cannot answer per-site operational questions that need a
  live round trip. Some genuinely useful questions are unanswerable in Phase 1.
- The pilot uses a pasted static credential rather than a delegated
  authorisation flow, which skews the cohort toward developer-tool users. We
  will not learn how a non-technical site owner experiences an approval queue
  until that flow exists. This is a real gap, accepted knowingly, and it is why
  that flow is next rather than distant.
- Every tool output crosses the tenant boundary into a third party's inference
  stack. For an agency managing other people's client sites this is the first
  procurement question.

**What has to exist before v1 ships.**

- The one-chokepoint site resolver, with the registry test that asserts every
  tool goes through it.
- The two sanitisers — one for text on its way to the model, one for text on
  its way to a human — with a planted hostile-name test on the approval
  payload.
- The proposal state machine with the conditional approve, plus proofs that the
  self-approval refusal and the wrong-credential-class refusal actually fire:
  plant the failure, watch it go red, restore, watch it go green, both outputs
  pasted with their commands.
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
