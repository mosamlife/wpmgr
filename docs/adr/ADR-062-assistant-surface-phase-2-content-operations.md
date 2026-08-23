# ADR-062 — Assistant surface, Phase 2: governed fleet content operations

**Status:** Proposed · **Date:** 2026-08-23
**Supersedes/relates:** Supersedes ADR-061 Decision 7 (site generation conceded), via ADR-061 amendment A7. Relates to ADR-061 (Phase 1, as amended 2026-08-23 — Decision 1 siting, Decision 2 out-of-band approval, Decision 3 control-plane-derived approval facts, amendment A3's tool registry, amendment A4's concurrency rule) and ADR-060 (this work sits in the differentiation phase and does not outbid anything above it).

**This ADR is Proposed and it blocks Phase 2.** No content-write code ships in
`apps/agent` or in the control plane until it is Accepted. The nine items in
[The nine things this ADR must establish](#the-nine-things-this-adr-must-establish)
are its acceptance checklist, and item 2 — which WordPress principal a content
write executes as — is currently unanswered and is a hard blocker rather than a
detail to settle during the build.

---

## Context

ADR-061 Decision 7 recorded content writes and site generation as **conceded
rather than deferred**, and gave a reason for the framing: a deferred item gets
re-proposed every planning cycle by whoever has not read the reasoning. That
instinct was right. The reasoning is what did not hold.

**Its premise was a fact about this repository, and that half is still true.**
No post, page, block, taxonomy or menu write path exists in the control plane or
in the agent. Re-verified this pass against the agent's full command list — ADR-061
amendment A2 carries the two commands that produce it — and no signed command
name in that list is a content verb.

**What it drew from that premise was a claim about the market, and that half
fails.** Decision 7 concluded that building a content model to serve an
assistant is "a product bet about entering a different market". Two things
contradict it.

We are already designing that market's product. A published Phase 2 wireframe
set and a published Phase 2 plan both design fleet content operations in detail —
blast radius across a fleet, coverage by page builder, waves, per-site status
vocabulary, undo across sites, partial-failure and halted-run states — against a
locked decision that forbids them. That is the largest governance gap the design
review found, and an ADR is how it closes.

And the technical objection underneath the concession rested on the wrong test.
The experiment that produced it round-tripped page-builder documents and compared
the result to the original **byte for byte**. Byte comparison fails for reasons
that have nothing to do with correctness, so it produced a verdict of
intractability that the evidence does not support. The correct test is stated
below and is one function's worth of work.

## What this is, and what it is not

**The unit of work is the fleet, not the document.** This ADR governs *content
operations across many sites* — apply a change to a set of sites, know which of
them can accept it, stage it, have a human approve it once, dispatch it in waves,
report per-site outcomes honestly including the partial ones, and undo it across
the set. Every concept in that sentence has an N in it.

That is a different product from single-site authoring, which has no N and
therefore needs none of it. Authoring tools optimize the round trip between one
person and one document. Nothing here is aimed at that, and the design should not
drift toward it: a feature that only makes sense with one site open is out of
scope by construction.

The buyer is the same one ADR-060 and ADR-061 are built for — an operator who
answers to someone about sites they do not personally own. What that operator
needs from content operations is not expressive power over a single page. It is
the ability to say what changed, on which of forty sites, who approved it, and
how to put it back.

## Where the write executes

**On the site, in the WordPress process, in `apps/agent`, reached only over the
existing outbound signed-command channel as new command names.**

This **re-affirms** ADR-061 Decision 1; it does not bend it. The site opens
nothing, holds no model-facing credential and gains no inbound AI surface. What
it gains is command classes, on a mechanism that already exists and already
carries a large set of them (ADR-061 amendment A2 measures it, with its
commands).

It has to execute in-process because a content write needs the runtime. A page
builder's document object, a field plugin's write API and the live block registry
are reachable only from inside a WordPress request. A control plane cannot call
any of them, and a content write assembled anywhere else is a guess about what
those objects would have done.

**Four separately signed command names, not one.** `content_read`,
`content_stage`, `content_promote`, `content_revert`. The split is not
organizational. Each command is authorized by its own bearer token bound to that
one command name, so a token minted to stage a change **cannot promote it**, and
the revert path is reachable without holding anything that can write. Collapsing
the four into a single `content_write` would discard that property for nothing.

The WordPress floor stays at **6.2** for content work
(`apps/agent/readme.txt:4`, `Requires at least: 6.2`). A builder write needs the
builder's own classes, which are shipped by the builder, not a newer core API.
Nothing in this ADR raises the floor on a heterogeneous fleet.

## The nine things this ADR must establish

This is the acceptance checklist. Every unchecked item is a reason this ADR is
still Proposed.

- [ ] **1. Where the write executes, and by what channel.** On-site, in-process,
      in `apps/agent`, reached only over the existing outbound signed-command
      channel, as the four separately signed command names above. Drafted in
      [Where the write executes](#where-the-write-executes). **Needs:**
      `security-reviewer` sign-off on the four-name split and its token scoping.
- [ ] **2. Which WordPress principal a content write executes as.** **Open, and
      a hard blocker.** See [The principal question](#the-principal-question).
      No content code is written before this closes.
- [ ] **3. Own or delegate, per integration — with verify-and-rollback as a
      shared primitive rather than a per-integration one.** See
      [Own or delegate](#own-or-delegate-and-why-the-guard-is-a-platform-property).
- [ ] **4. Snapshot before every destructive write, mandatory, at the platform
      layer.** See
      [Snapshot is a platform property](#snapshot-is-a-platform-property-not-an-integration-feature).
- [ ] **5. The block-editor fork, taken deliberately.** Two branches, each with
      its cost, one of which must be chosen before Phase 2 scope is fixed. See
      [The block-editor fork](#the-block-editor-fork).
- [ ] **6. Dynamic-data binding as the primary write-reduction strategy.** See
      [Bind once, then write fields](#bind-once-then-write-fields).
- [ ] **7. Scope: which page builders and field plugins are in v1**, on stated
      evidence about the cost of each, with everything outside v1 named as absent
      rather than silently missing.
- [ ] **8. How the Layer B / Layer C tool budget accommodates content.**
      Cross-references ADR-061 amendment A3: content proposal tools are Layer B,
      named by intent, and the measured `tools/list` threshold from A3 — not a
      round number — decides whether Layer C is needed at all.
- [ ] **9. Formal retirement of ADR-061 Decision 7 and its Consequences bullet**,
      with "conceded rather than deferred" replaced by the scoped commitment this
      ADR records. Done in ADR-061 amendment A7; this item closes when this ADR
      is Accepted.

---

## Verify after write, and why byte comparison is the wrong test

Re-serializing a page-builder document and comparing the bytes to the original
fails on documents that are semantically identical. Encoder profiles differ; key
ordering differs; whitespace, escaping and numeric formatting differ; generated
element ids, timestamps and revision counters change on every save by design. A
byte comparison reports all of that as damage. Used as an acceptance test it does
not measure whether the write was correct — it measures whether our serializer
happens to be byte-compatible with whatever wrote the document last, which is not
a property any writer can hold across a plugin's own version history.

**The correct test is a semantic signature over observed post-write state.**
Read the document back after the write, parse it, **strip the fields known to be
volatile**, canonicalize what remains, and compare the signature of that against
the signature computed from what we intended to write. A mismatch is a real
defect and triggers the rollback in item 4. A match is meaningful in a way a byte
match never was.

Two supporting techniques make the signature stable, and both belong in the
platform layer rather than in each integration:

- **Write the whole document rather than patching part of it.** When the writer
  emits the entire structure, the encoder profile becomes a property of our
  writer instead of a property of the document's history, and an entire class of
  spurious differences stops existing.
- **Guard the encode/decode depth asymmetry by decoding our own output before
  committing it.** Deeply nested structures can encode successfully and then fail
  to decode, because the encoder and decoder do not enforce the same depth limit.
  A structure that cannot be read back is not a document, and the place to
  discover that is before the write commits, not on the next page load.

## Snapshot is a platform property, not an integration feature

**Every destructive content write is preceded by a snapshot, taken by the
platform, before the integration is asked to do anything.**

This is not a new capability and it should not be re-litigated per integration.
The agent already does exactly this for file writes: an existing file is copied
to a per-op staging area before any mutation, and restoring a previous version is
itself a first-class command
(`apps/agent/includes/commands/class-file-write-command.php`). Content extends
that mechanism.

The reason it must sit at the platform layer is a failure mode worth stating
plainly. **A guard that each integration implements for itself is a guard some
integration will not implement.** Where two integrations are built on the same
engine from near-identical code, one can carry the snapshot and its sibling can
carry only a depth check, and nothing in review catches it, because each file
reads as complete on its own terms. The asymmetry is invisible until the day the
unguarded one is asked to roll back. Put the snapshot where an integration cannot
forget it: the promote path takes it, and an integration that wants to write is
handed a target that has already been snapshotted.

The same argument applies to the verify step above. Verify-and-rollback is one
primitive, invoked by the promote path, parameterized by the integration's
signature function — not a thing each integration is trusted to remember.

## Own or delegate, and why the guard is a platform property

For each supported integration this ADR records one of two postures, with its
reason:

- **Delegate** — call the integration's own save API and let it own its storage
  format. Lower risk, bounded capability, and it breaks when the plugin's API
  does not expose what the operation needs.
- **Own** — write the integration's storage format directly. Necessary where the
  save API cannot express the operation or requires a request context we do not
  have, and it makes us responsible for that format across the plugin's version
  history.

Owning a format is viable — that was the objection the byte test manufactured —
but it is only viable **with** the verify-and-rollback primitive above. Own with
verification, or delegate. Owning without a semantic verify is the posture this
ADR forbids.

**Field values are written through the field plugin's own API, never as raw post
meta.** This is not a preference. A custom field's value is stored as a *pair*:
the value under the field's name, and a companion row that ties that name to the
field's definition key. Writing the value row alone leaves the pair inconsistent —
the value is present in the database and the field no longer resolves to its
definition, so it stops appearing in the editor, stops being returned by the
plugin's own read functions, and starts differing between the front end and the
admin. A raw-meta write looks like it worked, and the defect surfaces later, on a
different screen, to someone who did not make the change. The plugin's write API
maintains both rows; use it. Where a target site has the field plugin but not a
version whose API can express the operation, that is a capability refusal (below),
not a reason to reach past it.

## Bind once, then write fields

**Dynamic-data binding is the primary write-reduction strategy, and it is a
design goal rather than an optimization.**

A page-builder element can be bound once to a field, after which every subsequent
content change is a *field* write and the builder document is not touched at all.
That converts the risky operation — a large, structurally unstable rewrite of a
proprietary document, repeated on every update — into a stable, small, verifiable
one, repeated instead.

The consequence for the roadmap is that the expensive builder-document work is
concentrated in a one-time binding operation per element, and the recurring fleet
operation afterward is field writes through the field plugin's API. Design the
binding operation carefully and the ongoing operation is cheap and safe. Invert
that and every routine content update carries the full risk of a document
rewrite.

## Per-site capability detection

**A site reports its own content capability surface over the signed channel, and
the control plane never proposes an operation the target cannot serve.**

The agent reports which page builders and field plugins are installed and active
and at what version; the control plane holds that in inventory alongside
everything else it knows about the site. Two rules follow:

- **A tool a site cannot serve is not offered for that site.** Absence, not a
  flag. This is ADR-061 Decision 5's mechanism applied to content: an
  unregistered capability cannot be reached by a schema-guessing model or by a
  future grant bug.
- **A refusal names the requirement and the site's actual version.** Not "not
  supported". A capability panel row reads, for example, *"Requires Elementor
  3.2 or later — this site has 2.9"*, with the site named. Absent, named and
  explained, never silently missing. The operator planning a fleet change needs
  to know which sites will not accept it **before** approving, not afterwards
  from a partial-failure report.

The same rule covers core-version-gated capability: a site below a version floor
sees a row stating the requirement and its own version, not an empty space.

## The block-editor fork

The core block editor is a genuine fork in the road and this ADR must take one
branch explicitly, because both branches have a cost and drifting between them
produces a feature that half exists.

The constraint: serialization of static block markup is performed client-side by
the editor's own JavaScript. A write path that must produce byte-correct block
markup for arbitrary blocks therefore needs that JavaScript to run, which means a
browser session per site, held open for the duration, driving a hidden editor and
managed by something that can survive a tab closing.

**Branch A — block-editor writes are out of scope for Phase 2.** Content
operations cover page-builder documents and field values; core block content is
read-only. Cost: a real capability gap on sites that use the core editor and no
page builder, which is a large share of any fleet, and it must be surfaced as a
named absence per the capability rule above rather than left to be discovered.

**Branch B — find a route that does not need a browser.** Server-side
construction restricted to a positively enumerated set of blocks whose
serialization is stable and expressible in PHP, with anything outside that set
refused by name. Cost: the enumerated set is small, it is maintenance that tracks
core, and the refusal surface has to be honest about what it will not do.

**A browser-held session per site is not a viable third branch for a fleet.** It
does not survive the tab closing, it cannot run in a wave across forty sites, and
it puts a human in the middle of the mechanical part of the operation while
ADR-061 deliberately puts them in the middle of the *decision*. Whichever branch
is taken, it is taken here and recorded, not discovered during the build.

---

## The principal question

**Open. This is the blocker.**

A signed command arrives at a site with no authenticated WordPress session.
Nothing in the request corresponds to a logged-in user, and that is deliberate —
it is the property ADR-061 Decision 1 exists to preserve. For every command
shipped so far this has not mattered. For a content write it matters immediately,
because WordPress content APIs read the current user:

- Post and revision authorship is attributed to the current user, so revision
  history either names a principal or names nobody.
- Capability checks inside builder save paths and inside third-party save hooks
  consult the current user, and behave unpredictably when there is not one.
- Other plugins observe the write through save hooks and may act on who made it.

So "which principal" is not a cosmetic question about an audit column. It
determines what the site's own revision history says, which save paths succeed,
and what every other plugin on that site sees. Three candidate answers, each with
its cost:

**(a) No user at all.** Truthful about what happened — the control plane wrote
this, not a person on the site — and it needs no new object on the site.
Capability checks inside builder save paths and third-party save hooks will fail
or behave unpredictably, and the revision carries no author, so the site's own
history cannot show where the change came from.

**(b) A dedicated agent-owned WordPress user, created at enrolment.** Revisions
attribute to a named principal an auditor can see on the site, and capability
checks pass predictably. The cost is that a privileged user object now exists on
every managed site — adjacent to the shape Decision 1 removed. It is not the same
shape (no password, no interactive login, no standing session, nothing that
authenticates inbound), but that distinction has to be **designed and proved**,
not asserted, and it is exactly the kind of claim ADR-061 Decision 4's copy rule
forbids stating as a guarantee.

**(c) The approving operator's mapped site user, where a mapping exists.** The
best audit story: the person who approved is the person the site's history names.
The cost is that the mapping does not exist today, will not exist for every
operator on every site, needs a fallback that is one of the two above anyway, and
it makes a control-plane approval mint site-side authorship for someone who never
touched the site.

**Whatever is chosen, three properties are required of the answer.** It is the
same principal the revision records and every capability check sees — not one
answer for authorship and another for authorization. It is recorded on the
proposal row, so the approval screen can render it as a control-plane-derived
fact before a human approves, per ADR-061 Decision 3. And it is stated on the
capability panel per site, because on a fleet the answer may legitimately differ
between sites and an operator must not have to guess which.

This question routes to `security-reviewer` before any content code is written.
It touches the agent protocol and site-side privilege, both of which are on that
list.

## Other open questions

**Does raw content ever cross to the model?** ADR-061's threat model assumes
prompt injection succeeds and constrains blast radius, and one of the constraints
is that the highest-density carriers — raw file contents, raw log bodies, raw
command output — are returned by no Phase 1 tool at all. Content operations put
that under direct pressure: the moment a content tool returns post bodies or
builder documents to the model, the densest injection carrier is back, sourced
from exactly the population ADR-061 names as attacker-controlled. The fork is
structured, control-plane-derived summaries only, or raw content with the
sanitizers doing the work. The first materially limits what a content assistant
can do; the second reopens a threat the design already assumes succeeds. This
ADR must decide it, and it is not decided here.

**Does Phase 2 write content, or also model content?** Editing what exists across
many sites is what the published Phase 2 design work actually describes. Creating
the content model itself — post types, taxonomies, field groups — is a third
product with its own internal representation problem, and folding it in
unannounced is how scope arrives without a decision. Recorded so it arrives, if
it arrives, on purpose.

**One internal content-model representation, adapted at the edges.** Where more
than one integration is supported, the temptation is a bespoke content model per
integration, and the cost lands later, on the first operation that has to span
two of them. Define one internal representation and adapt at each integration's
edge.

---

## Consequences

**What this forecloses.**

- No inbound content path to a site, now or later. Content reaches a site the
  same way everything else does: a signed command the control plane dispatches
  after a human approval. Anything else supersedes ADR-061 Decision 1.
- No destructive content write without a platform-taken snapshot and a semantic
  verify. An integration cannot opt out of either, because neither is the
  integration's to take.
- No raw post meta write for a field-plugin-managed value.
- No content operation that requires a human-held browser session per site.
- ADR-061's approval architecture is unchanged and is not relaxed for content:
  no automation may approve, and a content proposal is a proposal like any other.

**What it costs.**

- Owning a page-builder storage format is a standing maintenance liability that
  tracks someone else's release schedule. The verify primitive bounds the damage;
  it does not remove the work.
- Whichever branch of the block-editor fork is taken leaves a named capability
  gap, and naming it on a capability panel is part of the cost.
- Fleet-wide content operations are constrained by ADR-061 amendment A4's
  per-organization ceiling and per-site exclusive lock, so a large fleet change
  is a wave that takes time rather than a single instant.
- The principal answer, whichever it is, adds something to defend: a new site-side
  object, an unattributed revision, or a mapping.

**What has to exist before any Phase 2 content code is written.**

- All nine checklist items closed, item 2 first.
- `security-reviewer` on the principal decision, on the four-command token
  scoping, and on the snapshot and revert paths.
- `database-engineer` on the proposal and staging schema before any Go or PHP,
  per the routing rule that a change spanning a migration and code is two agents
  in sequence.
- The verify primitive with its planted-failure proof: a write that is corrupted
  on purpose must be caught by the semantic signature and rolled back, with the
  red and green outputs pasted alongside their commands — and then the honest
  cases it must not block, so a correct write with a changed timestamp does not
  trigger a rollback.
- `make test-integration` run locally before merge, since content proposals carry
  site scoping and `ci.yml` does not run that package.
