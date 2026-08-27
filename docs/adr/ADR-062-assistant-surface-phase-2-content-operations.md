# ADR-062 — Assistant surface, Phase 2: governed fleet content operations

**Status:** Proposed · **Date:** 2026-08-27
**Supersedes/relates:** Revisits ADR-061 Decision 7 (site generation conceded) directly, and its acceptance supersedes that decision's concession framing for the content-editing and page-generation capabilities scoped below — see [Consequences](#consequences). Relates to ADR-061 as Accepted (Decision 1's siting and signed-command channel, Decision 2's out-of-band approval state machine, Decision 3's control-plane-derived approval facts and quarantined-note contract, Decision 4's application-layer site scoping with database-level scoping named as its own deferred decision, Decision 5's capability-by-registry-absence model, Decision 6's flat tool set and its roughly-25-entry facade threshold) and ADR-060 (this work sits at position 5, Differentiation, in that ADR's precedence order, and does not outbid open earlier-position work for engineering time).

**This ADR is Proposed and it blocks Phase 2.** No content-write code ships in
`apps/agent` or in the control plane until it is Accepted. The items in
[The acceptance checklist](#the-acceptance-checklist) are what has to close
first.

---

## Context

ADR-061 Decision 7 recorded content writes and site generation as **conceded
rather than deferred**, and gave a reason for the framing: a deferred item
gets re-proposed every planning cycle by whoever has not read the reasoning.
That instinct was right. The reasoning underneath it was not, and revisiting
it is the point of this document.

**Its premise was a fact about this repository, and that half is still
true.** No post, page, block, taxonomy or menu write path exists in the
control plane or in the agent today. Re-verified this pass, directly against
the agent's own command list rather than against a description of it:

```sh
ls apps/agent/includes/commands/ | grep -iE "post|page|content|block|meta"
```

The only hit is `class-metadata-command.php`, and reading it settles the
question: it collects site inventory — WP and PHP version, active theme,
every installed plugin and theme with its version, the multisite flag — for
the control plane's `/agent/v1/metadata` endpoint. It is not a post or page
content path. Nothing in the command list is a content verb.

**What Decision 7 drew from that premise was a claim about the market, and
that half does not hold.** It concluded that building a content model to
serve an assistant is "a product bet about entering a different market." Two
things contradict it. First, a fleet operator's actual need — say what
changed, on which of a fleet's sites, who approved it, and how to put it
back — is the same buyer ADR-060 and ADR-061 are already built for, not a
different one. Second, the technical objection that sat underneath the
concession rested on the wrong test: an experiment that round-tripped
page-builder documents and compared the result to the original **byte for
byte** produced a verdict of intractability the evidence does not support,
for reasons stated in
[Verify after write](#verify-after-write-and-why-byte-comparison-is-the-wrong-test)
below. The correct test is one function's worth of work, not a wall.

## What this is, and what it is not

**The unit of work is the fleet, not the document.** This ADR governs
*content operations across many sites* — apply a change to a set of sites,
know which of them can accept it, stage it, have a human approve it once,
dispatch it, report per-site outcomes honestly including the partial ones,
and undo it across the set. Every concept in that sentence has an N in it.

That is a different product from single-site authoring, which has no N and
therefore needs none of the above. Authoring tools optimize the round trip
between one person and one document. Nothing here is aimed at that, and the
design should not drift toward it: a feature that only makes sense with one
site open in one browser tab is out of scope by construction, independent of
how good an idea it might be on its own terms.

The buyer is the one ADR-060 and ADR-061 are already built for — an operator
who answers to someone else about sites they do not personally own. What
that operator needs from content operations is not expressive power over a
single page. It is the ability to say what changed, on which of forty sites,
who approved it, and how to put it back.

## Where the write executes

**On the site, in the WordPress process, in `apps/agent`, reached only over
the existing outbound signed-command channel, as new command names.**

This **re-affirms** ADR-061 Decision 1; it does not bend it. The site opens
nothing, holds no model-facing credential, and gains no inbound AI surface.
What it gains is new command classes, on a mechanism that already exists and
already carries a large, heterogeneous set of them:

```sh
ls apps/agent/includes/commands/*.php | grep -c .
```

Sixty-eight files today, covering backups, file operations, caching,
database maintenance, media, performance configuration and security sync —
none of them a content verb, per the grep above. Content commands are new
entries in an existing registry, not a new channel.

It has to execute in-process because a content write needs the WordPress
runtime. A page builder's document object, a field plugin's write API and
the live block registry are reachable only from inside a WordPress request.
The control plane cannot call any of them, and a content write assembled
anywhere else is a guess about what those objects would have done.

**Four separately signed command names, not one.** `content_read`,
`content_stage`, `content_promote`, `content_revert`. The split is not
organizational. Each command is authorized by its own bearer token bound to
that one command name, so a token minted to stage a change **cannot promote
it**, and the revert path is reachable without holding anything that can
write. Collapsing the four into a single `content_write` would discard that
property for nothing.

The WordPress floor stays at **6.2** for content work:

```sh
grep -n "Requires at least" apps/agent/readme.txt
```

`apps/agent/readme.txt:4` — `Requires at least: 6.2`. A builder write needs
the builder's own classes, which are shipped by the builder, not a newer core
API. Nothing in this ADR raises the floor on a heterogeneous fleet.

## The Builder Adapter Framework

**Builders are not a deferred category behind one or two hand-built
integrations. Every integration this ADR supports implements one contract,**
and a builder that cannot implement all of it does not ship write support
for the parts it cannot.

- **`detect(version)`** — returns a capability set for the installed
  version, not a boolean. Detection and capability are the same call.
- **`read()`** — returns a normalised document model, never the builder's
  raw storage shape, so every downstream step (diff, snapshot, verify) works
  against one representation regardless of which builder produced it.
- **`propose(ops)`** — accepts normalised change operations and nothing
  else. **The model never authors raw post-meta, a raw shortcode string, or
  a raw block comment.** It selects and parameterizes operations from a
  closed vocabulary; the adapter is what turns an operation into the
  builder's actual storage shape.
- **`diff()`** — computed twice, structural and content, separately. A row
  inserted and a sentence edited are different kinds of risk, and an
  approval screen that folds them into one number has thrown away the
  distinction an operator needs to decide.
- **`snapshot()`** — **fail-closed.** No snapshot, no write, no exceptions.
  See [Snapshot is a platform property](#snapshot-is-a-platform-property-and-the-existing-precedent-is-fail-open),
  which is the one section of this ADR that exists because an earlier
  reasoning step got this wrong.
- **`stage()`** — produces a preview from the staged copy the snapshot
  already took. Never the live page. An operator approving a change is
  looking at what the change will produce, not at the site as it currently
  stands with an assertion layered on top.
- **`validate()`** — semantic, not structural-only: does this document, as
  proposed, mean what the operation said it should mean, checked against the
  target builder's own rules (for Gutenberg, this is the block registry —
  see [Gutenberg validates server-side](#gutenberg-validates-server-side-and-never-through-a-browser)).
- **`apply()`** — idempotent and keyed to the proposal, so a replayed
  `content_promote` against an already-applied proposal is a no-op rather
  than a second write. A retry after a crash must never double-apply.
- **`verify()`** — checked **from outside the site** wherever the operation
  produces something externally observable (a public page's rendered
  title, for instance); where nothing is externally observable, verify()
  falls back to a second signed read, and the fallback is labeled as weaker
  in the record, because a second read from the same agent is the pipeline's
  own report of itself, not independent confirmation.
- **`rollback()`** — proven per builder, not assumed from the existence of a
  snapshot. **A builder without a proven rollback does not get write
  support.** This is deliberately stricter than "we took a backup, so we can
  presumably restore it."
- **`round_trip()`** — the original builder must still open the result
  correctly. Tested against fixtures per supported version, because a
  document that WordPress accepts on write and a builder's own editor
  chokes on is a defect this ADR exists to catch before a customer does.

**An unsupported version yields a typed refusal naming the requirement and
the actual installed version** — never a bare "not supported," and never a
silent no-op. This is ADR-061 Decision 5's absence-by-registry mechanism
applied at the version granularity: a schema-guessing model cannot coax a
write out of a version this adapter has not proven, and an operator sees
exactly what to upgrade.

Two of these method contracts exist specifically because of concrete failure
modes an audit of a competing product's source turned up, and both are
recorded where the affected method lives rather than here in the abstract:
`snapshot()`'s and `rollback()`'s stricter requirements in
[Scope: builders in the first wave](#scope-builders-in-the-first-wave), under
Elementor.

## The acceptance checklist

`[x]` means the decision is recorded in this ADR; `[ ]` means it is not, and
every `[ ]` is a reason this ADR is still Proposed. The count below is never
asserted in prose — verify it from the list itself, and refuse to trust a
heading that claims one:

```sh
f=docs/adr/ADR-062-assistant-surface-phase-2-content-operations.md
[ -f "$f" ] || { echo "FAIL: $f missing -- the checklist moved, which is not an empty checklist" >&2; exit 1; }
total=$(grep -cE '^- \[[ x]\] \*\*[0-9]+\.' "$f")
[ "$total" -gt 0 ] || { echo "FAIL: no checklist items matched -- the list was reformatted, which is not zero items" >&2; exit 1; }
echo "checklist items $total"
echo "still open      $(grep -cE '^- \[ \] \*\*[0-9]+\.' "$f")"
```

The refusal on zero is the point of writing it this way, not decoration: a
reformatted list that matched nothing would otherwise print `0 open` and
read as an ADR ready to accept.

- [x] **1. Where the write executes, and by what channel.** On-site,
      in-process, in `apps/agent`, reached only over the existing outbound
      signed-command channel, as the four separately signed command names
      above. Recorded in [Where the write executes](#where-the-write-executes).
      **Still needs:** `security-reviewer` sign-off on the four-name split
      and its token scoping — a build gate, not an acceptance gate; the
      decision is made, the proof is not.
- [x] **2. Which WordPress principal a content write executes as.**
      **Decided:** a dedicated WordPress service user with a custom role,
      created by the agent, with no password, no login path, no session,
      and deliberately no `unfiltered_html`. See
      [The principal question](#the-principal-question). Its attack-test
      suite is a **ship gate**, not a review item.
- [ ] **3. Own or delegate, per integration, with verify-and-rollback as a
      shared platform primitive rather than a per-integration one.** The
      rule is recorded in
      [Own or delegate](#own-or-delegate-and-why-the-guard-is-a-platform-property);
      the per-integration table that applies it is not fully written, and it
      cannot be for the builders behind Elementor and Gutenberg until the
      first-wave machinery is proved once.
- [ ] **4. Snapshot before every destructive write, mandatory, fail-closed,
      at the platform layer.** The rule is recorded in
      [Snapshot is a platform property](#snapshot-is-a-platform-property-and-the-existing-precedent-is-fail-open).
      **Open because no fail-closed snapshot mechanism for content rows
      exists.** The file-write staging mechanism this ADR once leaned on for
      precedent is fail-open by its own documentation, so it cannot be
      extended unchanged — the mechanism has to be built new, not borrowed.
- [x] **5. The Builder Adapter Framework: one contract for every
      integration.** Recorded in
      [The Builder Adapter Framework](#the-builder-adapter-framework). The
      contract is decided; individual builders' proofs against it (rollback,
      round-trip fixtures) are build work tracked per builder in
      [Scope: builders in the first wave](#scope-builders-in-the-first-wave).
- [x] **6. Gutenberg block validation runs server-side, never through a
      browser.** Recorded in
      [Gutenberg validates server-side](#gutenberg-validates-server-side-and-never-through-a-browser).
- [x] **7. The four content-generation capabilities are separated, and one
      of them is rejected outright.** Recorded in
      [The four content-generation capabilities](#the-four-content-generation-capabilities).
- [x] **8. Builder support levels: a closed vocabulary, and a first-wave
      assignment against it.** Recorded in
      [Builder support levels](#builder-support-levels) and
      [Scope: builders in the first wave](#scope-builders-in-the-first-wave).
- [ ] **9. Dynamic-data binding as the primary write-reduction strategy.**
      See [Bind once, then write fields](#bind-once-then-write-fields).
      Recorded as a design goal; the binding operation itself is unscoped
      and lands with Elementor, so it cannot close ahead of that work.
- [ ] **10. How the tool surface accommodates content**, bounded by ADR-061
      Decision 6's roughly-25-entry facade-revisit threshold rather than a
      number invented here. **Open by construction:** that threshold is
      measured against a live `tools/list`, which does not exist until the
      Phase 1 surface is serving traffic.
- [x] **11. The fleet-inventory query is an owned gate on builder order,
      never on the framework.** Recorded in
      [The fleet-inventory query gates order](#the-fleet-inventory-query-gates-order-never-the-framework).
      The rule is decided even though the query itself has not been run —
      that is the point of the rule.

**Formal retirement of ADR-061 Decision 7's concession framing is not on
this list.** An earlier draft of this checklist put it here as an item that
"closes when this ADR is Accepted," which cannot ever be true while every
open item on the same list blocks acceptance — a gate that only opens after
the door it is standing on already opened. Retirement of the concession is a
**consequence** of this ADR being Accepted, not a precondition on it. It is
recorded that way in [Consequences](#consequences) instead.

---

## Verify after write, and why byte comparison is the wrong test

Re-serializing a page-builder document and comparing the bytes to the
original fails on documents that are semantically identical. Encoder
profiles differ; key ordering differs; whitespace, escaping and numeric
formatting differ; generated element ids, timestamps and revision counters
change on every save by design. A byte comparison reports all of that as
damage. Used as an acceptance test it does not measure whether the write was
correct — it measures whether our serializer happens to be byte-compatible
with whatever wrote the document last, which is not a property any writer
can hold across a plugin's own version history.

**The correct test is a semantic signature over observed post-write state.**
Read the document back after the write, parse it, **strip the fields known
to be volatile**, canonicalize what remains, and compare the signature of
that against the signature computed from what was intended. A mismatch is a
real defect and triggers `rollback()`. A match is meaningful in a way a byte
match never was.

Two supporting techniques make the signature stable, and both belong in the
platform layer rather than in each integration:

- **Write the whole document rather than patching part of it.** When the
  writer emits the entire structure, the encoder profile becomes a property
  of our writer instead of a property of the document's history, and an
  entire class of spurious differences stops existing. This is also the
  storage-layer shape Elementor's `apply()` takes, below, because its own
  document has no smaller unit that is safe to patch in place.
- **Guard the encode/decode depth asymmetry by decoding our own output
  before committing it.** Deeply nested structures can encode successfully
  and then fail to decode, because the encoder and decoder do not enforce
  the same depth limit. A structure that cannot be read back is not a
  document, and the place to discover that is before the write commits, not
  on the next page load.

## Snapshot is a platform property, and the existing precedent is fail-open

**Every destructive content write is preceded by a snapshot, taken by the
platform, before the integration is asked to do anything, and the snapshot
step is fail-closed: no snapshot, no write, no exceptions.**

An earlier draft of this ADR justified this as "not a new capability,"
pointing at the agent's existing file-write staging as precedent and
treating content snapshots as a straightforward extension of it. That
reasoning does not hold, and it matters enough to say plainly rather than
quietly fix: **the existing mechanism is not fail-closed, and its own
documentation says so.**

```sh
sed -n '255,260p' apps/agent/includes/commands/class-file-write-command.php
```

```
 * Errors are silenced — failure here does NOT block the write; the backup is
 * best-effort so that a later "restore previous version" is possible.
```

`stageBackup()` returns silently — without raising, without setting any flag
the caller checks — on an unwritable staging directory, an unreadable source
file, and a failed encryption step; the method's return type is `void`, so
there is nothing for the caller to check even if it wanted to; and the
caller (`class-file-write-command.php:160`) invokes it and proceeds to the
write unconditionally regardless of what happened inside it. A file write
today succeeds whether or not its backup existed. That is a defensible
choice for a filesystem write, where the operator can usually still recover
the previous state by other means, and it is documented as deliberate, not
accidental.

**It is not a defensible choice for content, and borrowing it unchanged
would be new risk wearing an old name.** A content write this ADR governs is
the one being approved by a named human specifically because it is
reversible; a write whose snapshot silently failed and then proceeded is a
write that broke the one promise the approval was granted on. So this is
**new** in the only respect that matters: the fail-open mechanism does not
become fail-closed by being reused for a different payload, and this ADR
does not let the checklist close by pointing at it unchanged. Item 4 above
stays open until a fail-closed snapshot primitive for content exists —
one whose write path is provably unreachable when the snapshot step did not
complete, not one that merely tries harder and hopes.

The same argument applies to `verify()` and `rollback()` above.
Verify-and-rollback is one primitive, invoked by `apply()`, parameterized by
the integration's signature function — not a thing each integration is
trusted to remember, for the identical reason a per-integration snapshot is
a guard some integration will not implement: where two integrations are
built on the same engine from near-identical code, one can carry the
snapshot and its sibling can carry only a depth check, and nothing in review
catches it, because each file reads as complete on its own terms.

## Own or delegate, and why the guard is a platform property

For each supported integration this ADR records one of two postures, with
its reason:

- **Delegate** — call the integration's own save API and let it own its
  storage format. Lower risk, bounded capability, and it breaks when the
  plugin's API does not expose what the operation needs.
- **Own** — write the integration's storage format directly. Necessary where
  the save API cannot express the operation or requires a request context
  this ADR does not have, and it makes us responsible for that format across
  the plugin's version history.

Owning a format is viable — that was the objection the byte test
manufactured — but only **with** `verify()` and `rollback()` from the
Builder Adapter Framework. Own with verification, or delegate. Owning
without a proven semantic verify and a proven rollback is the posture this
ADR forbids, per the framework's own rule that a builder without a proven
rollback does not get write support.

**Field values are written through the field plugin's own API, never as raw
post meta.** This is not a preference; it is what `propose(ops)` means by
"normalised operations only." A custom field's value is stored as a *pair*:
the value under the field's name, and a companion row that ties that name to
the field's definition key. Writing the value row alone leaves the pair
inconsistent — the value is present in the database and the field no longer
resolves to its definition, so it stops appearing in the editor, stops being
returned by the plugin's own read functions, and starts differing between
the front end and the admin. A raw-meta write looks like it worked, and the
defect surfaces later, on a different screen, to someone who did not make
the change. The plugin's write API maintains both rows; use it. Where a
target site has the field plugin but not a version whose API can express the
operation, that is a capability refusal per the framework's `detect()`
contract, not a reason to reach past it.

## Bind once, then write fields

**Dynamic-data binding is the primary write-reduction strategy, and it is a
design goal rather than an optimization.**

A page-builder element can be bound once to a field, after which every
subsequent content change is a *field* write and the builder document is not
touched at all. That converts the risky operation — a large, structurally
unstable rewrite of a proprietary document, repeated on every update — into
a stable, small, verifiable one, repeated instead.

The consequence for the roadmap is that the expensive builder-document work
concentrates in a one-time binding operation per element, and the recurring
fleet operation afterward is field writes through the field plugin's API.
Design the binding operation carefully and the ongoing operation is cheap
and safe. Invert that and every routine content update carries the full risk
of a document rewrite.

## Per-site capability detection

**A site reports its own content capability surface over the signed
channel, and the control plane never proposes an operation the target
cannot serve.**

The agent reports which page builders and field plugins are installed and
active and at what version; the control plane holds that in inventory
alongside everything else it knows about the site. Two rules follow, both
already stated in the Builder Adapter Framework's `detect()` contract and
restated here because they are the operator-facing consequence of it:

- **A tool a site cannot serve is not offered for that site.** Absence, not
  a flag — ADR-061 Decision 5's mechanism applied to content: an
  unregistered capability cannot be reached by a schema-guessing model or by
  a future grant bug.
- **A refusal names the requirement and the site's actual version.** Not
  "not supported." A capability panel row reads, for example, *"Requires
  Elementor 3.2 or later — this site has 2.9,"* with the site named. Absent,
  named and explained, never silently missing. The operator planning a
  fleet change needs to know which sites will not accept it **before**
  approving, not afterwards from a partial-failure report.

The same rule covers core-version-gated capability: a site below a version
floor sees a row stating the requirement and its own version, not an empty
space.

## Builder support levels

**One closed vocabulary, stated once, and nothing outside it is a support
level:**

`detect-only` · `read` · `narrow edit` · `structured edit` ·
`full replacement` · `new-page generation`.

- **`detect-only`** — presence and version are known and shown on the
  capability panel. No content is read back.
- **`read`** — the normalised document model can be produced for an
  existing page. No writes.
- **`narrow edit`** — a named, bounded slice of fields or attributes can be
  changed inside an existing structure. Nothing structural moves.
- **`structured edit`** — normalised operations against the document model
  can add, remove or change elements, evaluated through the full Builder
  Adapter Framework lifecycle.
- **`full replacement`** — the write replaces the whole stored document,
  because the integration's own storage format has no smaller unit that is
  safe to patch in place.
- **`new-page generation`** — a new document can be authored from a
  proposal rather than edited from an existing one.

A builder's assignment to a level is not a promise about every feature that
builder has; it is a promise about what this ADR has proved for it. The
assignments for the first wave are in
[Scope: builders in the first wave](#scope-builders-in-the-first-wave).

## The four content-generation capabilities

**These are four different products, and this ADR does not let them sit
under one heading again — that collapse is exactly how the original
concession in ADR-061 Decision 7 reasoned about "content" as a single
undifferentiated thing and got the market conclusion wrong.**

- **(a) Edit an existing page in its current builder.** Read the page, stage
  a change, verify, allow rollback. This is what the rest of this ADR is
  built to do, and it is **this ADR's first wave.**
- **(b) Create a new page in a selected builder.** A `new-page generation`
  write against an operator-chosen builder rather than an edit against an
  existing document. It uses the same adapter contract's `propose()` and
  `apply()`, aimed at an empty target instead of an existing one. **It
  follows (a)**, not alongside it: a builder does not earn `new-page
  generation` until its `structured edit` or `full replacement` level is
  proved.
- **(c) Generate multiple pages for an existing site.** The same operation
  as (b), fanned out across N pages on one site. **Gated on (a) proving
  verify-and-rollback in production** — not in a test suite, in the field,
  against a real fleet — because fanning an unproven single-page operation
  out to N pages multiplies whatever it gets wrong by N, and a partial
  failure across many pages on one site is a worse incident than a partial
  failure across many sites for one page.
- **(d) Generate an entire new site or theme. Rejected.** This is a
  published non-goal, not a deferral, and reversing it takes a public change
  of mind — a new ADR, not a roadmap edit. The reason is the one piece of
  ADR-061 Decision 7's original reasoning that turns out to be correctly
  applied to exactly this capability and nothing else: there is no existing
  document to snapshot before the write, no existing site to verify against
  from outside, and no incremental change for an operator to approve — it is
  authoring a document graph from nothing, which really is a different
  product with its own internal representation problem, on its own market
  bet, and it should be taken on its own merits with its own ADR if it is
  ever taken. (a) through (c) are not that: every one of them has an
  existing document or an existing site to anchor snapshot, verify and
  rollback to, which is precisely what (d) lacks.

## Gutenberg validates server-side, and never through a browser

The core block editor is a genuine fork in the road, and this ADR takes one
branch explicitly rather than drifting between them, because drifting
produces a feature that half exists.

**The constraint.** Serialization of static block markup is normally
performed client-side by the editor's own JavaScript. A write path that must
produce byte-correct block markup for arbitrary blocks therefore needs that
JavaScript to run, which means a browser session per site, held open for the
duration, driving a hidden editor and managed by something that can survive
a tab closing.

**A browser-held session, human- or machine-held, is not a viable branch for
a fleet, and this ADR designs no path that needs one.** It does not survive
the tab closing, it cannot run across forty sites in a wave, and a
machine-held variant driven from our own cloud through a one-time login link
is worse than a human-held one, not better, because it means holding a
session against the customer's own admin account. ADR-061 already puts a
human in the middle of the *decision*; a browser-driven write path puts one
in the middle of the mechanical part instead, which is a regression this ADR
refuses.

**The branch taken: block parsing and validation run entirely server-side,
in the agent's own WordPress process, against WordPress's own block
registry** — `parse_blocks()` and the registered block-type definitions in
`WP_Block_Type_Registry`, the same registry core itself consults, not a
description of it and not a browser's rendering of it. Write capability is
scoped to exactly two things, and nothing that resembles a third:

1. **Dynamic blocks registered by `apps/agent` itself, with no saved
   markup.** A block that saves nothing serializes to a comment delimiter
   plus its JSON attributes, so PHP serialization is exact rather than
   approximately right. This buys real authoring capability with no browser
   and no new mechanism, and it is the best value available anywhere in the
   content surface.
2. **A positively enumerated set of core static blocks** whose serialization
   is stable and expressible in PHP. Anything outside the set is refused
   **by name** — the same absence-not-a-flag rule that governs capability
   detection.

Arbitrary third-party and community block markup is a **stated absence**,
not a silent gap: reachable through `read()` for display, never through
`propose()` for a write, and named as such on the capability panel. This is
why Gutenberg reaches **first-wave, full-lifecycle status on a scoped
catalog** — its dynamic blocks and its enumerated static-block set get every
method in the Builder Adapter Framework, proved — while arbitrary blocks
outside that catalog stay at `read`. "First wave, end-to-end" describes the
lifecycle depth on the supported catalog, not coverage of every block a
third-party plugin might register; the same distinction applies to Elementor
below, whose full lifecycle does not cover every widget a third-party
Elementor add-on might ship either.

**If a later wave revisits static-block authoring beyond this catalog, one
test is planted first, and it must fail before anything is built.** An
implementation that stops at constructing blocks and serializing them
produces markup that *passes* block validation and renders broken on the
front end, for exactly the blocks that mint a unique id in an edit-time
effect — which is exactly the population of blocks customers care about most.
A build that cannot demonstrate that failure first has not understood the
problem it is solving, and the passing-validation part is what makes it
dangerous: the obvious check goes green.

## The fleet-inventory query gates order, never the framework

**Recorded once, plainly, because it is easy to let a missing number
quietly defer the wrong thing.** The fleet-inventory query — for each
integration target, how many sites have it and what its version distribution
looks like against that target's vendor floor — decides which builder is
worked on *first* within a support level this ADR has already granted it.
It never decides *whether* the Builder Adapter Framework applies, and it
never decides whether a builder is eligible for detect-only reporting: that
baseline is cheap enough to ship for every integration target regardless of
what the query says.

Two further pre-development checks travel with this rule, both about
detection honesty rather than capability, and neither has been run:

- **Confirm the outbound prober can fetch arbitrary URLs**, because
  `verify()`'s external-verifiability path for anything with a public
  rendering rests on it.
- **Audit how the plugin-signature corpus is matched against per-site
  component inventory**, since detection accuracy for the whole capability
  panel depends on that match being correct.

**The ordering above is reasoned, not measured, and this document says so
wherever the ordering is rendered.** No installed-base numbers were used to
produce it, because no command produces them yet and an installed-base
figure repeated for a year after it stops being true is worse than no figure
at all. **The criteria do not change if the query surprises us. The builder
order does.**

## Scope: builders in the first wave

**Every integration target gets `detect-only`, always registered, never
gated behind a wave.** It is the cheapest capability in this entire surface
and the highest leverage: *"which of my four hundred sites run a page
builder below its floor"* is a question operators have today and nothing
answers. On top of that universal baseline, the six named builders below get
what this ADR has actually proved for them, and no other targets get more
than the baseline in the first wave.

| Builder | First-wave level | Why |
|---|---|---|
| **Gutenberg** | `structured edit`, full lifecycle, on the scoped catalog | See [Gutenberg validates server-side](#gutenberg-validates-server-side-and-never-through-a-browser). No browser session, ever. |
| **Elementor** | `structured edit` from the proposal model, implemented as `full replacement` in storage | See below — no stable public write API, so raw storage ownership is unavoidable, and it carries this ADR's strictest requirements as a result. |
| **WPBakery** | `detect-only` + `read` now; `narrow edit` once proven | Its document lives in shortcodes inside `post_content`, which makes a narrow, structure-preserving edit conceptually straightforward once the platform snapshot and verify primitives exist. Not scheduled ahead of item 3 and item 4 closing. |
| **Bricks** | `detect-only` + `read` | No write path is scheduled. Reachable for `narrow edit` or above only once a revertible path is proven for it specifically — this ADR commits to nothing beyond read on a date. |
| **Divi** | `detect-only` + `read` | Its document is written into the post body, and a mismatched writer destroys the page on first editor open — a failure that stays silent until someone opens the editor. Reading carries none of that risk; writing does not get a path here. |
| **Beaver Builder** | `detect-only` + `read` | Same posture as Bricks: no write path scheduled, no revertible-path proof exists yet. |

**Elementor's requirements, named because an audited reference
implementation's own failures show exactly why they matter.** Elementor
stores its document as a hand-owned post-meta blob whose real schema lives
in editor JavaScript, so raw storage access is unavoidable — there is no
stable public write API to delegate to. That is exactly why it carries the
full contract rather than a shortcut:

- A **documented compatibility contract** per supported version, not an
  assumption that the format is stable across releases.
- **Fixture tests per version**, exercising `round_trip()` against real
  documents saved by that version.
- **Version gates** from `detect()`, refusing by name below the floor a
  fixture has proved.
- **A forced revision on every write.** Because the document lives in post
  meta rather than post content, a meta-only write does not by itself cause
  WordPress to create a revision the way an ordinary content edit does. An
  audited reference implementation's writes are exactly this: writes that
  create no revision and therefore have nothing built by core to roll back
  to. Elementor's `snapshot()` forces a revision explicitly, every time,
  rather than relying on core's implicit revisioning of `post_content`,
  which this document does not use.
- **Round-trip proof**, per the framework: the Elementor editor itself must
  still open what we wrote.
- **Safe failure and a proven `rollback()`** — proven, not assumed from the
  existence of a snapshot. The same audit found builder revert paths that
  restore only a subset of what they changed; the standing discipline this
  ADR applies against that finding is the one already proved out for the
  service principal below — plant a multi-field change, roll it back, and
  assert every field that changed is restored, not only the fields the
  builder's own "restore" affordance is known to touch.

**Non-builder capabilities, alongside the builder work, not gated by it.**
Three capabilities need no page builder at all and are part of the first
wave regardless of builder order:

- **SEO metadata — one canonical document, adapters per plugin.** Post-level
  and term-level only: title, meta description, canonical, robots, social
  tags, focus keyword. Excludes site-wide settings, redirects and
  structured data. **This is the capability that leads, for one reason an
  operator can repeat: it is the only one in the entire surface whose result
  can be proved from outside the site** — fetch the public URL from the
  control plane and read the title and description out of the response,
  which is exactly `verify()`'s preferred path in the framework above. Every
  other candidate here is verifiable only by asking the same agent again.
- **Commerce catalog — reads in full, writes narrow.** Writes limited to
  product and variation stock, stock status and post status. Excludes every
  price write, every delete, orders, customers, refunds, coupons, tax and
  gateways. Everything goes through the commerce plugin's own product API,
  never raw post meta — the same rule this ADR states for field values.
  **Price writes, whenever they land, carry a typed confirmation
  permanently** — the same typed confirmation-string shape the agent already
  uses for its most destructive database command, not a boolean asserted by
  the caller and not a consequence of a per-author risk flag. The first
  wave does not include price writes at all.
- **Custom-field values — read and write, no structure.** Excludes field-group
  and field creation. A documented plugin write API maintains the
  value/definition pair, per [Own or delegate](#own-or-delegate-and-why-the-guard-is-a-platform-property).

**Every risk flag in the catalogue above gets a written definition and a
check that fails CI**, built as a repo script with a committed self-test,
per the standing rule that build-gating logic never lives in a YAML block
scalar. A risk flag without a definition means four things to four
engineers, and a consent gate keyed on it inherits all four.

**Out of scope entirely in the first wave, each with its own reason, and
none of them promoted by the fleet query above them:**

- **Avada** — the format is owned by hand at the SQL level, with far more
  raw database call sites than any other integration surveyed. Supporting
  it means adopting someone else's schema, not their API.
- **Oxygen and Breakdance** — one codebase behind a mode constant, so
  supporting either means supporting both and telling them apart correctly
  every time.
- **Etch, Voxel, Mosaic** — no evidence they are present on this fleet, and
  nothing in the criteria above promotes them ahead of the six named
  builders regardless of what the fleet query eventually shows.
- **Any translation push to a vendor cloud — permanently, at any support
  level, for any builder.** It is the only operation in the surveyed surface
  that mutates state **outside the customer's server**, which means it
  cannot be rolled back at all, and this ADR's whole premise is that every
  write has a snapshot and a way back. Translation *reads* are fine.

---

## The principal question

A signed command arrives at a site with no authenticated WordPress session.
Nothing in the request corresponds to a logged-in user, and that is
deliberate — it is the property ADR-061 Decision 1 exists to preserve. For
every command shipped so far this has not mattered. For a content write it
matters immediately, because WordPress content APIs read the current user:

- Post and revision authorship is attributed to the current user, so
  revision history either names a principal or names nobody.
- Capability checks inside builder save paths and inside third-party save
  hooks consult the current user, and behave unpredictably when there is
  not one.
- Other plugins observe the write through save hooks and may act on who
  made it.

So "which principal" is not a cosmetic question about an audit column. It
determines what the site's own revision history says, which save paths
succeed, and what every other plugin on that site sees.

**Decided:**

> **A dedicated WordPress service user with a custom role, created by the
> agent: no password, no login path, no session, and deliberately no
> `unfiltered_html`.**

The three candidates and their trade-offs are kept below rather than
trimmed to the winner, because the next person to reason about this quickly
will re-derive the rejected candidate with the best-sounding audit story and
needs to find the reason it was rejected, not just the argument for the one
that won.

### What is created, and what it can do

- **One WordPress user per site, created by the agent idempotently at
  enrolment**, and re-created if it is found missing. It has no password, no
  application password, no interactive login path and no session. It exists
  to be the `current_user` for the duration of one signed content command
  and nothing else.
- **Its role is ours, not `editor` and not `administrator`.** It carries
  exactly the WordPress capabilities the enabled content commands need, and
  it never carries `manage_options`, `edit_users`, `install_plugins`,
  `edit_files`, `edit_themes` or `unfiltered_html`.
- **The command sets the current user for the handler's duration and clears
  it afterwards**, so a `current_user_can()` inside a builder's save path or
  a third-party `save_post` hook resolves against a real, correctly-scoped
  principal instead of against nobody.
- **`post_author` on a new post, and the author on every revision, is that
  user**, so a site owner opening the WordPress editor sees a legible actor
  rather than a blank.
- **A post-meta stamp links the WordPress revision back to the control
  plane** — change-set id, proposal id, and the approving human's identity
  as recorded in the audit chain. The site's own history and ours resolve to
  the same event.

This satisfies the three required properties stated at the end of this
section: one principal for authorship and authorization, recorded on the
proposal row so the approval screen can render it as a control-plane-derived
fact per ADR-061 Decision 3, and stated per site on the capability panel —
because a site whose service user was deleted or whose role was stripped
legitimately answers differently, and an operator must see that before
approving rather than afterwards in a partial-failure report.

### What it costs, stated plainly

**It puts a privileged object on every managed site.** That is the honest
objection, and it is adjacent to the shape ADR-061 Decision 1 removed. The
answer is that it has no password, no inbound authentication, no standing
session and no route by which anything outside a command this ADR's channel
signed can become it.

**But "not really privileged" is an argument, and an argument is not a
property.** The distinction has to be *proved*, and the proof is a specific
attack-test suite that is a **ship gate** — no content-write code ships
without it green, and it is not a line item on a reviewer's checklist that a
busy review can wave through:

- Authenticate as the user by password → must fail, because no password
  hash exists.
- Mint an application password for it → must be refused.
- Reach it through the autologin path → must be refused; the target
  allowlist excludes it.
- Establish a REST or admin session as it → must fail.
- Enumerate its capabilities → must equal the declared role exactly, with
  `unfiltered_html`, `manage_options`, `edit_files` and `install_plugins`
  all absent.
- **Widen the role after enrolment** — edit the custom role in place, or
  clone it under a new name and reassign the service user to the clone, to
  restore `unfiltered_html` or any other withheld capability, the way a site
  admin might "fix" a permissions complaint or a third-party plugin might
  rewrite roles on activation → must be caught before the next write, not
  merely logged after it.

Each of those is planted as a *passing* attack first, watched go red,
restored, and watched go green, with both outputs pasted alongside their
commands. A suite nobody has seen fail is not known to test anything, and
this is the suite the whole "not really privileged" claim rests on.

**The sixth attack is different in kind from the first five, and it is the
one the original five-attack gate never tested.** The first five ask whether
the principal can be reached or impersonated; the sixth asks whether the
principal, reached exactly as designed, still means what it meant when it
was created. A site admin's own dashboard, or any third-party plugin running
on that site, can edit or clone a WordPress role at any time — that is
ordinary, unprivileged WordPress behaviour, not an exploit — and nothing
about creating the role safely at enrolment stops it from being widened
five minutes, or five months, later. A foreclosure checked once, at
creation, and never again is not a foreclosure; it is a snapshot with a
foreclosure's name on it.

**So the runtime posture is: verify at use, not only at creation.** Every
content command that sets the service user as `current_user` first reads
that user's live capability set and compares it to the declared set this
ADR names above. A match proceeds. Any drift — a capability present that
should be absent, most importantly `unfiltered_html` — refuses the command
outright with a typed `principal_capabilities_drifted` error naming the
site, the extra capability found, and a remediation path (recreate the role
from the declared set), before any content operation runs. **Never a silent
continuation on the capabilities the role happens to have now**: a write
that executes because the drift was not checked is the exact failure this
ADR's whole "not really privileged" claim was built to prevent, arriving by
a path the original five attacks did not cover. This check is cheap — one
capability-set comparison already available from the WordPress role object,
on the same request that is about to set `current_user` — so there is no
performance argument for checking it only at creation.

**It is deletable by the site's own admin, and that breaks content writes.**
That is correct behaviour — the site owner is sovereign over their own
users — and the failure must surface as a typed `principal_missing` refusal
with a one-click recreate remedy. **Never a silent fallback to a different
principal**: a write that quietly executes as somebody else is worse than a
write that refuses.

**It is a new visible row on the customer's Users screen**, and it will
generate support questions. That is a documentation cost, and a small one.

### What it forecloses, deliberately

**It forecloses making an AI change look human-authored.** Attribution is
legible by construction, and some operators will want the post author to be
their own editor's account. This ADR chooses legibility of automation over
invisibility of automation: a fleet where AI-authored changes cannot be told
apart from human ones is a fleet nobody can audit, and that is the property
this whole product line sells. If human attribution is ever genuinely
needed, the shape is a per-site overridable `post_author` while the
executing principal stays fixed — attribution and execution become two
fields, and only the display field moves. It never becomes a second
execution principal.

**It forecloses writing arbitrary HTML.** Without `unfiltered_html`,
`wp_kses_post` runs over the content, so a content write cannot introduce a
script tag, an iframe or an arbitrary embed. Two consequences, and both are
features. The assistant cannot place a tracking pixel or a third-party embed
into a page — if that capability is ever wanted it arrives as its own
decision, with its own gate, not as a side effect of a content write. And a
prompt-injection payload that reaches the content path cannot become
executable markup on the site: the last conversion step from text to script
is missing. This is worth saying in product copy in the terms ADR-061
Decision 4 permits — describe what runs on a write, never claim a boundary.

**It forecloses the mapped-operator-identity candidate as the v1 answer**,
and the no-user-at-all candidate entirely for content. Both reasons are
below.

### The candidates, kept for the record

**(a) No user at all.** Truthful about what happened — the control plane
wrote this, not a person on the site — and it needs no new object on the
site. Capability checks inside builder save paths and third-party save
hooks will fail or behave unpredictably, and the revision carries no
author, so the site's own history cannot show where the change came from.

**(b) A dedicated agent-owned WordPress user, created at enrolment.**
Revisions attribute to a named principal an auditor can see on the site, and
capability checks pass predictably. The cost is that a privileged user
object now exists on every managed site — adjacent to the shape Decision 1
removed. It is not the same shape (no password, no interactive login, no
standing session, nothing that authenticates inbound), but that distinction
has to be **designed and proved**, not asserted, and it is exactly the kind
of claim ADR-061 Decision 4's copy rule forbids stating as a guarantee.

**(c) The approving operator's mapped site user, where a mapping exists.**
The best-sounding audit story: the person who approved is the person the
site's history names. The cost is that the mapping does not exist today,
will not exist for every operator on every site, needs a fallback that is
one of the two above anyway, and it makes a control-plane approval mint
site-side authorship for someone who never touched the site.

**Why (a) is rejected for content specifically.** "No user" is an honest
answer for a filesystem, option or raw-SQL write, and it is the answer
every command shipped so far gives. For a content write it is a bug
generator: this ADR's own cost line for (a) — capability checks that "fail
or behave unpredictably" inside builder save paths and third-party hooks —
describes a class of defect that surfaces on someone else's plugin, on a
customer's site, days later. There is no version of that failure debuggable
from the control plane.

**Why (c) is rejected as the v1 answer.** The mapping it depends on does not
exist, would not exist for every operator on every site if it did, and
would need a fallback that is (a) or (b) anyway — so choosing (c) is
choosing (b) plus a mapping. Worse, it makes a control-plane approval mint
site-side authorship for a person who never touched the site, which is a
*worse* audit story than the one it was chosen for. Not deferred, rejected.

**Whatever is chosen, three properties are required of the answer.** It is
the same principal the revision records and every capability check sees —
not one answer for authorship and another for authorization. It is
recorded on the proposal row, so the approval screen can render it as a
control-plane-derived fact before a human approves, per ADR-061 Decision 3.
And it is stated on the capability panel per site, because on a fleet the
answer may legitimately differ between sites and an operator must not have
to guess which. The decision above satisfies all three; a future revision of
it must satisfy them too.

**Routing.** `security-reviewer` with `model: "opus"` **before any content
code is written** — this is a new principal, and it touches the agent
protocol and site-side privilege, both of which are on that list. Then the
build order is `database-engineer` (proposal and staging schema) →
`backend-architect` (control-plane half) → `wp-agent-engineer`
(`apps/agent`), per the rule that a change spanning a migration and code is
agents in sequence and never one agent doing both.

## Other open questions

**Does raw content ever cross to the model? Decided: yes, inside the fence,
with provenance, never as instructions.** ADR-061's threat model assumes
prompt injection succeeds and constrains blast radius, and one of its
constraints is that the highest-density carriers — raw file contents, raw
log bodies, raw command output — are returned by no Phase 1 tool at all.
Content operations put that under direct pressure: the moment a content
tool returns post bodies or builder documents to the model, the densest
injection carrier is back, sourced from exactly the population ADR-061
names as attacker-controlled. The fork was structured, control-plane-derived
summaries only, or raw content with the sanitizers doing the work.

The first branch is refused because it produces a content assistant that
cannot read content, which is not the product. The second is taken because
the threat it would be defending against is one this architecture **already
assumes succeeds**: ADR-061 puts the real boundary at structural
authorization outside the model loop, precisely so that a successful
injection changes nothing about what is permitted. The model is not made
safe by starving it; it is made safe by ensuring nothing it reads can change
what it is allowed to do.

**This ADR depends on a fencing mechanism that ADR-061 has decided but not
yet built, and treats it as required new work rather than an inherited
given.** Under "Two things that were expensive to learn," ADR-061 states the
design: *"site-origin text is stripped and fenced with a per-response nonce
before it reaches the model, and stripped again on a separate path before it
reaches a human."* That design is Accepted, but the two sanitizers that
implement it are listed in ADR-061 itself under "What has to exist before v1
ships" (ADR-061:544,548-550), and ADR-061's own verification note records
that the surface they belong to is unshipped (ADR-061:570-572). ADR-061 never
mentions skill content at all — the word does not appear in that document —
so nothing about how skill instructions will be fenced can be read off it
either. Page content, builder documents and every other site-originated
string content operations touch are intended to fall under that same
mechanism once it exists, under a standing preamble stating that the
enclosed material is reference text that cannot change what is permitted.
Nothing about content operations widens or narrows that design; it extends
the population of strings the fence is meant to cover. But "the design
accounts for this" and "the mechanism exists" are different claims, and this
ADR's ship gate below requires the second one, not just the first.

The ship gate travels with the decision: a **planted hostile site name** — or,
for content specifically, a planted hostile post title or field value — must
produce an approval screen that still renders correctly and a set of
permitted actions that is unchanged.

**Does this ADR write content, or also model content?** Editing what exists
across many sites is what this ADR covers. Creating the content model
itself — post types, taxonomies, field groups — is a third product with its
own internal representation problem, and folding it in unannounced is how
scope arrives without a decision. Recorded so that, if it ever arrives, it
arrives on purpose, with its own ADR.

**One internal content-model representation, adapted at the edges.** Where
more than one integration is supported, the temptation is a bespoke content
model per integration, and the cost lands later, on the first operation that
has to span two of them. Define one internal representation — the
`read()`/`propose()` normalised document model in the Builder Adapter
Framework — and adapt at each integration's edge. No integration gets its
own internal shape.

---

## Why this is still Proposed

1. **Its own gate is what would lift.** Accepting this ADR releases the
   sentence at the top — *no content-write code ships until it is
   Accepted*. That sentence should be released when the work is ready to
   start, not when the argument is finished.
2. **ADR-060's precedence puts this behind open earlier-position work.**
   Content operations sit at position 5, Differentiation, in ADR-060's
   ordered list. ADR-061 Decision 4, as Accepted, names database-level site
   scoping as explicitly deferred and "its own decision" — a Gate item
   (position 1) that is still open on the day this document is written.
   ADR-060 is explicit that this ordering is a precedence rule, not a
   sequence of gates each blocking the next ("it is not a gate, and nothing
   below is a second one" — ADR-060:38), so accepting a position-5 ADR while
   a position-1 item sits open does not violate the freeze clause — this ADR
   adds no externally-reachable surface by itself — but it advertises
   readiness the precedence order does not support, and precedence gets
   reversed by exactly this kind of drift.
3. **Checklist items remain genuinely open, and none of them closes on
   argument alone.** Item 3's per-integration own/delegate table needs the
   first-wave machinery proved once before it can be written for the
   builders behind it. Item 4 needs a fail-closed snapshot mechanism for
   content rows that does not exist yet — the existing file-write staging
   is fail-open by design and cannot be reused unchanged, per
   [Snapshot is a platform property](#snapshot-is-a-platform-property-and-the-existing-precedent-is-fail-open).
   Item 9's binding operation is unscoped. Item 10 needs a measurement —
   ADR-061 Decision 6's roughly-25-entry threshold against a live
   `tools/list` — that cannot be taken until Phase 1 is serving traffic.
4. **The builder order is reasoned, not measured, and the query that would
   measure it has not run.** Per
   [The fleet-inventory query gates order](#the-fleet-inventory-query-gates-order-never-the-framework),
   the criteria do not change if the query surprises us, but the order does,
   and accepting this ADR before running it converts "we reasoned this"
   into "we decided this" in every downstream reading.
5. **`security-reviewer` has not seen the principal, the four-command token
   scoping, or the fail-closed snapshot mechanism**, and the attack-test
   suite the whole "not really privileged" claim rests on has not been
   written, let alone watched go red.

**What closes it.** The five above, in that order. Items 3 and 9 are
expected to close *inside* the first wave rather than before it, and this
ADR should say so explicitly when it is accepted rather than carry them as
permanent open marks.

---

## Consequences

**What this forecloses.**

- No inbound content path to a site, now or later. Content reaches a site
  the same way everything else does: a signed command the control plane
  dispatches after a human approval. Anything else supersedes ADR-061
  Decision 1.
- No destructive content write without a platform-taken, fail-closed
  snapshot and a proven semantic verify and rollback. An integration cannot
  opt out of any of the three, because none of the three is the
  integration's to take.
- No raw post-meta write for a field-plugin-managed value, and no raw
  operation of any kind reaching a builder outside the normalised
  `propose(ops)` vocabulary.
- No content operation that requires a human-held browser session per site,
  and none that requires a machine-held one either.
- **No content write that can produce executable markup.** The execution
  principal has no `unfiltered_html`, so content passes through core's
  post-content sanitizer. Script, iframes and arbitrary embeds are out of
  reach of this surface, and putting them back in reach is a separate
  decision with its own gate — never a side effect of a content feature.
- **No AI-authored change that looks human-authored.** The executing
  principal is fixed and the revision names it. A future per-site
  `post_author` override moves the display field only.
- **No whole-site or whole-theme generation.** Capability (d) is rejected,
  not deferred; reversing it takes a superseding ADR, the same mechanism
  that would reverse anything else recorded here.
- ADR-061's approval architecture is unchanged and is not relaxed for
  content: no automation may approve, and a content proposal is a proposal
  like any other.
- **Formal retirement of ADR-061 Decision 7's concession framing** — this
  ADR's acceptance is what retires it, for the content-editing and
  page-generation capabilities scoped above. This is recorded here rather
  than on the acceptance checklist because it is a consequence of
  acceptance, not a gate on it: the retirement cannot be a precondition on
  the event that causes it.

**What it costs.**

- Owning a page-builder storage format is a standing maintenance liability
  that tracks someone else's release schedule. The verify-and-rollback
  primitive bounds the damage; it does not remove the work.
- The Gutenberg branch taken here leaves a named capability gap on
  third-party and community blocks outside the enumerated catalog, and
  naming it on a capability panel is part of the cost.
- The principal answer adds something to defend: a new site-side object on
  every managed site, deletable by the site's own admin, visible on the
  Users screen, and carrying a support-documentation cost.
- The builder-order roadmap buys the narrowest proven capability first and
  defers the rest. That is the intended trade, and the cost is that
  operators on the deferred builders see a named absence for longer.

**What has to exist before any Phase 2 content code is written.**

- Every checklist item closed, or explicitly carried into the first wave
  per [Why this is still Proposed](#why-this-is-still-proposed).
- The pre-development tasks in
  [The fleet-inventory query gates order](#the-fleet-inventory-query-gates-order-never-the-framework):
  the fleet-inventory query itself, the outbound-prober confirmation, and
  the signature-corpus match audit.
- The **service-principal attack-test suite** green, with each attack
  planted as passing first and both outputs pasted alongside their
  commands. This is a ship gate; it is not satisfied by a reviewer agreeing
  that the principal looks safe.
- A **fail-closed snapshot mechanism for content rows**, built new rather
  than borrowed from the file-write staging precedent, with a planted
  failure proof: force the snapshot step to fail and assert the write never
  happens, red then green, both pasted with their commands.
- **The injection-fencing mechanism itself** — the two sanitizers ADR-061
  lists under its own "What has to exist before v1 ships"
  (ADR-061:544,548-550), one for text on its way to the model and one for
  text on its way to a human. Content operations widen who feeds that fence
  (page content, builder documents), not what the fence is, but the fence
  does not exist yet and this ADR cannot ship ahead of it: a planted hostile
  post title or field value must produce an approval screen that still
  renders correctly, per the ship gate in
  [Other open questions](#other-open-questions), and that proof needs a real
  sanitizer to run against.
- `security-reviewer` with `model: "opus"` on the principal decision, on the
  four-command token scoping, and on the snapshot and revert paths.
- `database-engineer` on the proposal and staging schema before any Go or
  PHP, per the routing rule that a change spanning a migration and code is
  two agents in sequence.
- The `verify()` primitive with its planted-failure proof: a write that is
  corrupted on purpose must be caught by the semantic signature and rolled
  back, with the red and green outputs pasted alongside their commands —
  and then the honest cases it must not block, so a correct write with a
  changed timestamp does not trigger a rollback.
- `make test-integration` run locally before merge, since content proposals
  carry site scoping and `ci.yml` does not run that package.
