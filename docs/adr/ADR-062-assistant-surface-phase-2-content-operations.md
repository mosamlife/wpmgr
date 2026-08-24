# ADR-062 — Assistant surface, Phase 2: governed fleet content operations

**Status:** Proposed · **Date:** 2026-08-23 · **Amended:** 2026-08-24
**Supersedes/relates:** Supersedes ADR-061 Decision 7 (site generation conceded), via ADR-061 amendment A7. Relates to ADR-061 (Phase 1, as amended 2026-08-23 and 2026-08-24 — Decision 1 siting, Decision 2 out-of-band approval, Decision 3 control-plane-derived approval facts, Decision 4 application-layer site scoping, amendment A3's tool registry, amendment A4's concurrency rule, amendment A9's sequencing, amendment A13's fencing rule), ADR-060 (this work sits in the differentiation phase and does not outbid anything above it) and ADR-063 (licensing and third-party reuse).

**This ADR is Proposed and it blocks Phase 2.** No content-write code ships in
`apps/agent` or in the control plane until it is Accepted. The items in
[The nine things this ADR must establish](#the-nine-things-this-adr-must-establish)
are its acceptance checklist.

**What the 2026-08-24 amendment changes.** Item 2 — which WordPress principal a
content write executes as — was named here as *"Open, and a hard blocker"*. It is
now decided, in [The principal question](#the-principal-question). Items 5 and 7
are decided with it, and the raw-content fork in *Other open questions* is decided
too. **The ADR stays Proposed**, and the reasons are named rather than left to
inference: [Why this is still Proposed](#why-this-is-still-proposed).

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

This is the acceptance checklist, revised 2026-08-24. `[x]` means the decision is
recorded in this ADR; `[ ]` means it is not, and every `[ ]` is a reason this ADR
is still Proposed.

The count in this heading is not a figure to trust from a heading. Verify it, and
verify how many are still open, from the list itself:

```sh
f=docs/adr/ADR-062-assistant-surface-phase-2-content-operations.md
[ -f "$f" ] || { echo "FAIL: $f missing -- the checklist moved, which is not an empty checklist" >&2; exit 1; }
total=$(grep -cE '^- \[[ x]\] \*\*[0-9]+\.' "$f")
[ "$total" -gt 0 ] || { echo "FAIL: no checklist items matched -- the list was reformatted, which is not zero items" >&2; exit 1; }
echo "checklist items $total"
echo "still open      $(grep -cE '^- \[ \] \*\*[0-9]+\.' "$f")"
```

The refusal on zero is the point of writing it this way. A reformatted list that
matched nothing would otherwise print `0 open` and read as an ADR ready to accept.

- [x] **1. Where the write executes, and by what channel.** On-site, in-process,
      in `apps/agent`, reached only over the existing outbound signed-command
      channel, as the four separately signed command names above. Recorded in
      [Where the write executes](#where-the-write-executes). **Still needs:**
      `security-reviewer` sign-off on the four-name split and its token scoping.
      That review is a build gate, not an acceptance gate — the decision is made,
      the proof is not.
- [x] **2. Which WordPress principal a content write executes as.** **Decided
      2026-08-24:** a dedicated WordPress service user with a custom role, created
      by the agent, with no password, no login path, no session, and deliberately
      no `unfiltered_html`. See
      [The principal question](#the-principal-question). Its attack-test suite is
      a **ship gate**, not a review item.
- [ ] **3. Own or delegate, per integration — with verify-and-rollback as a
      shared primitive rather than a per-integration one.** The rule is recorded
      in [Own or delegate](#own-or-delegate-and-why-the-guard-is-a-platform-property);
      **the per-integration table that applies it is not written**, and it cannot
      be written for Wave 2 until the Wave 1 machinery has been proved once.
- [ ] **4. Snapshot before every destructive write, mandatory, at the platform
      layer.** The rule is recorded in
      [Snapshot is a platform property](#snapshot-is-a-platform-property-not-an-integration-feature).
      **Open because the file version stack does not cover content**, and
      extending it is named work that has not started. Snapshot-as-a-rule is
      cheap; snapshot-as-a-mechanism for post rows is the build.
- [x] **5. The block-editor fork, taken deliberately.** **Decided 2026-08-24:**
      Branch A for third-party and core *static* blocks, as a stated absence, plus
      a narrow Branch B. See [The block-editor fork](#the-block-editor-fork).
- [ ] **6. Dynamic-data binding as the primary write-reduction strategy.** See
      [Bind once, then write fields](#bind-once-then-write-fields). Recorded as a
      design goal; **the binding operation itself is unscoped** and it lands with
      the one builder in Wave 2, so it cannot close ahead of that wave.
- [x] **7. Scope: which page builders and field plugins are in v1**, with
      everything outside v1 named as absent rather than silently missing.
      **Decided 2026-08-24** in [Scope: the wave plan](#scope-the-wave-plan) —
      and decided on **reasoned** criteria, not on measured fleet data. That
      caveat travels with the plan wherever the plan is rendered.
- [ ] **8. How the Layer B / Layer C tool budget accommodates content.**
      Cross-references ADR-061 amendment A3: content proposal tools are Layer B,
      named by intent, and the measured `tools/list` threshold from A3 — not a
      round number — decides whether Layer C is needed at all. **Open by
      construction:** A3 requires a measurement that cannot be taken until Layer A
      is serving live traffic. This item closes with a number, not with an
      argument.
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

## Scope: the wave plan

**Decided 2026-08-24.** This closes checklist item 7. Read the two honesty
statements that follow it before acting on the ordering; they are part of the
decision, not a caveat attached to it.

### Wave 1 — no page builder, and no block-editor writes

Wave 1 deliberately contains **no page builder at all** and **no Gutenberg
writes**. It is four capabilities:

| # | Capability | Scope | Why it is here |
|---|---|---|---|
| 1a | **Capability detection across every integration target.** Read-only, always registered, never gated off below a version floor. | Zero writes | The cheapest item in the plan and the highest leverage. *"Which of my four hundred sites run a page builder below its floor"* is a question operators have today and nothing answers. One schema across all integrations, with per-integration detail confined to a typed detail object. |
| 1b | **SEO metadata — one canonical document, adapters per plugin.** Post-level and term-level only: title, meta description, canonical, robots, social tags, focus keyword. | **Excluded:** site-wide settings, redirects, structured data | Externally verifiable (below). Documented storage. Reversible by read-then-write. One canonical document with several adapters is the highest value-per-line abstraction in the whole surface. |
| 1c | **Commerce catalog — reads in full, writes narrow.** Writes limited to product and variation stock, stock status and post status. | **Excluded:** every price write, every delete, orders, customers, refunds, coupons, tax, gateways | Everything goes through the commerce plugin's own product API and never through raw post meta, which is the same rule this ADR already states for field values. |
| 1d | **Custom-field values — read and write. No structure.** | **Excluded:** field-group and field creation | A documented plugin write API that maintains the value/definition pair, reversible, and friendly to the signed-command channel. |

**Why SEO leads, in one sentence an operator can repeat:** it is the only
capability in the entire surface whose result can be proved **from outside the
site** — fetch the public URL from the control plane and read the title and
description out of the response.

That is the standing rule about verifying the thing rather than the pipeline's
report of it, applied to a roadmap. Every other Wave 1 candidate — a stock write,
a field write, a theme option write — is verifiable only by asking the same agent
again, which *is* the pipeline's report. We already run outbound uptime probes, so
the verification path exists in shape. **It exists in shape, not in fact:** whether
that prober can be reused for arbitrary URL fetches has not been checked, and the
entire external-verifiability argument rests on it. Confirming that is a
pre-development task, below.

**Price writes carry a typed confirmation, permanently, whenever they land.** Not
a boolean asserted by the caller, and not a consequence of a risk flag: the typed
confirmation string shape the agent already uses for its most destructive database
command. A consent gate keyed on a per-author risk flag is a gate whose meaning
drifts per author. Wave 1 does not include price writes at all.

**Every risk flag in the catalogue gets a written definition and a check that
fails CI**, built as a repo script with a committed self-test, per the standing
rule that build-gating logic never lives in a YAML block scalar. A risk flag
without a definition means four things to four engineers, and a consent gate keyed
on it inherits all four.

### Wave 2 — exactly one page builder

- **Elementor, and only Elementor, of the page builders.** Its document is a
  hand-owned post-meta blob whose real schema lives in editor JavaScript, so it
  requires the semantic verification primitive plus the platform snapshot and the
  ownership precondition to **already exist and already be proved**. This narrows
  the prior planning round's "three to four builders in v1" to one: three builders
  is three reverse-engineered formats, three signature functions and three
  maintenance liabilities tracking three release schedules, all bought before the
  machinery that contains them has been proved once.
- **Theme settings for a small set of themes**, late in the wave, and only because
  of a structural accident that makes them unusually safe: several of them keep
  everything in a single serialized option, so snapshot-and-restore is one option
  read and one option write — the cleanest rollback available anywhere in this
  surface. Any theme whose storage mode varies is detected before it is supported.
  A theme operation that imports content or can install plugins is excluded
  outright.
- **Form reads only.** The canonical-document abstraction earns its keep on the
  read side, where several mappers collapse into one comparable document. It is
  not attempted on the write side, where each mapper would have to be exactly
  right and a shared abstraction becomes a liability.
- **Dynamic-data discovery reads** — enumerate what is bindable, validate it,
  evaluate it. Cheap, idempotent, no writes. This is the read half of
  [Bind once, then write fields](#bind-once-then-write-fields).

### Wave 3 — only if the fleet query justifies it

Gutenberg **static** blocks (subject to the block-editor fork above and its
planted failing test), Bricks, form writes, and content modelling. Each carries a
detection-signature seeding task as an explicit prerequisite, surfaced in the
backlog up front rather than discovered mid-build — several Wave 3 targets are
genuinely absent from our detection corpus today, and Wave 1 depends on none of
them.

### Out of scope, each with its one-line reason

- **Divi** — the document is written into the post body, and a mismatched writer
  destroys the page on first editor open. The failure is silent until someone
  opens the editor.
- **Avada** — the format is owned by hand at the SQL level, with far more raw
  database call sites than any other integration measured. We would be adopting
  someone else's schema, not their API.
- **Oxygen and Breakdance** — one codebase behind a mode constant, so supporting
  either means supporting both and telling them apart correctly every time.
- **WPBakery** — older branches are fatal on PHP 8, so touching a site below the
  floor takes the site down rather than failing the write.
- **Beaver Builder, Etch, Voxel, Mosaic** — no evidence they are on this fleet,
  and nothing in the criteria promotes them ahead of the targets above.
- **Any translation push to a vendor cloud** — permanently. It is the only
  operation in the surveyed surface that mutates state **outside the customer's
  server**, which means we cannot roll it back at all, and this ADR's whole
  premise is that every write has a snapshot and a way back. Translation *reads*
  are fine.

### Two honesty statements that travel with this plan

**1. The ordering is reasoned, not measured — say so wherever the plan is
rendered.** No installed-base numbers were used to produce it, deliberately, since
there is no command that produces them and an installed-base figure is exactly the
kind of number that gets repeated for a year after it stops being true. The
criteria are: can we verify the result from outside the site, can we put it back,
is the storage documented, and does it fit the signed-command channel. Installed
base is deliberately last. **A reasoned ordering must never be read as an
evidenced one**, and it will be, if a wave table is rendered on a screen with no
such line on it.

**2. The fleet-inventory query is a pre-development task, and it has not been
run.** For each integration target: how many of our sites have it, and its version
distribution against that target's vendor floor. The data is already there — the
per-site component inventory, the active theme, the plugin-signature corpus and
the wordpress.org checksum tables. Running it costs an afternoon and turns this
section from an opinion into a plan. **The criteria do not change if the query
surprises us. The ordering does.**

Two further pre-development checks gate Wave 1, and both are about detection
honesty rather than capability:

- **Confirm the outbound prober can fetch arbitrary URLs**, because the whole
  external-verifiability argument for leading with SEO rests on it and it was
  never checked.
- **Audit how the plugin-signature corpus is matched against per-site component
  inventory.** Wave 1 leans on SEO and SEO leans on detecting a handful of
  plugins. At least one corpus key does not match that plugin's real
  wordpress.org directory slug. Either the corpus is keyed on something other
  than the slug and this is noise, or it is keyed on the slug and that plugin has
  never been detected on any site and nobody has noticed. One look at the match
  path settles it, and it is a cheap look with an expensive wrong answer.

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

### The fork, taken — 2026-08-24

**Branch A for third-party and core *static* blocks, recorded as a stated
absence. Plus a narrow Branch B, limited to blocks whose serialization we can make
exact server-side.** Branch B is precisely two things and nothing that resembles
them:

1. **Dynamic blocks we register ourselves in `apps/agent`, with no saved
   markup.** A block that saves nothing serializes to a comment delimiter plus its
   JSON attributes, so PHP serialization is exact rather than approximately right.
   This buys real authoring capability with no browser and no new mechanism, and
   it is the best value available anywhere in the content surface.
2. **A positively enumerated set of core blocks** whose serialization is stable
   and expressible in PHP. Anything outside the set is refused **by name**, with
   the name in the refusal — the same absence-not-a-flag rule that governs
   capability detection above.

**A machine-held browser session is also refused, and it is not a third branch.**
This ADR already forecloses any content operation needing a human-held browser
session per site. Driving a hidden editor from our own cloud through a one-time
login link is the machine-held variant of the same posture change, and it is
worse rather than better, because it means we hold a session for the customer's
admin account. Refused permanently.

**If a later wave revisits static-block authoring, one test is planted first, and
it must fail before anything is built.** An implementation that stops at
constructing blocks and serializing them produces markup that *passes* block
validation and renders broken on the front end, for exactly the blocks that mint a
unique id in an edit-time effect — which is exactly the population of blocks
customers care about. A build that cannot demonstrate that failure first has not
understood the problem it is solving, and the passing-validation part is what
makes it dangerous: the obvious check goes green.

---

## The principal question

**Decided 2026-08-24. This was the blocker; it is now the decision.**

> **A dedicated WordPress service user with a custom role, created by the agent:
> no password, no login path, no session, and deliberately no
> `unfiltered_html`.**

This is candidate (b) below, taken with its cost stated and its cheap alternative
refused. The three candidates and their trade-offs are left in place underneath,
because the next person to reason about this quickly will re-derive candidate (c)
— it has the best-sounding audit story — and needs to find the reason it was
rejected rather than the argument for it.

### What is created, and what it can do

- **One WordPress user per site, created by the agent idempotently at enrolment**,
  and re-created if it is found missing. It has no password, no application
  password, no interactive login path and no session. It exists to be the
  `current_user` for the duration of one signed content command and nothing else.
- **Its role is ours, not `editor` and not `administrator`.** It carries exactly
  the WordPress capabilities the enabled content commands need, and it never
  carries `manage_options`, `edit_users`, `install_plugins`, `edit_files`,
  `edit_themes` or `unfiltered_html`.
- **The command sets the current user for the handler's duration and clears it
  afterwards**, so a `current_user_can()` inside a builder's save path or a
  third-party `save_post` hook resolves against a real, correctly-scoped
  principal instead of against nobody.
- **`post_author` on a new post, and the author on every revision, is that user**,
  so a site owner opening the WordPress editor sees a legible actor rather than a
  blank.
- **A post-meta stamp links the WordPress revision back to the control plane** —
  change-set id, proposal id, and the approving human's identity as recorded in
  the audit chain. The site's own history and ours resolve to the same event.

This satisfies the three required properties stated at the end of this section:
one principal for authorship and authorization, recorded on the proposal row so
the approval screen can render it as a control-plane-derived fact per ADR-061
Decision 3, and stated per site on the capability panel — because a site whose
service user was deleted or whose role was stripped legitimately answers
differently, and an operator must see that before approving rather than afterwards
in a partial-failure report.

### What it costs, stated plainly

**It puts a privileged object on every managed site.** That is the honest
objection and it is the same one this ADR raised against candidate (b) in the
first place: it is adjacent to the shape ADR-061 Decision 1 removed. The answer is
that it has no password, no inbound authentication, no standing session and no
route by which anything outside a command we signed can become it.

**But "not really privileged" is an argument, and an argument is not a property.**
The distinction has to be *proved*, and the proof is a specific attack-test suite
that is a **ship gate** — no content-write code ships without it green, and it is
not a line item on a reviewer's checklist that a busy review can wave through:

- Authenticate as the user by password → must fail, because no password hash
  exists.
- Mint an application password for it → must be refused.
- Reach it through the autologin path → must be refused; the target allowlist
  excludes it.
- Establish a REST or admin session as it → must fail.
- Enumerate its capabilities → must equal the declared role exactly, with
  `unfiltered_html`, `manage_options`, `edit_files` and `install_plugins` all
  absent.

Each of those is planted as a *passing* attack first, watched go red, restored,
and watched go green, with both outputs pasted alongside their commands. A suite
nobody has seen fail is not known to test anything, and this is the suite the
whole "not really privileged" claim rests on.

**It is deletable by the site's own admin, and that breaks content writes.** That
is correct behaviour — the site owner is sovereign over their own users — and the
failure must surface as a typed `principal_missing` refusal with a one-click
recreate remedy. Never a silent fallback to a different principal: a write that
quietly executes as somebody else is worse than a write that refuses.

**It is a new visible row on the customer's Users screen** and it will generate
support questions. That is a documentation cost, and a small one.

### What it forecloses, deliberately

**It forecloses making an AI change look human-authored.** Attribution is legible
by construction, and some operators will want the post author to be their own
editor's account. We are choosing legibility of automation over invisibility of
automation: a fleet where AI-authored changes cannot be told apart from human ones
is a fleet nobody can audit, and that is the property this whole product line
sells. If human attribution is ever genuinely needed, the shape is a per-site
overridable `post_author` while the executing principal stays fixed — attribution
and execution become two fields, and only the display field moves. It never
becomes a second execution principal.

**It forecloses writing arbitrary HTML.** Without `unfiltered_html`,
`wp_kses_post` runs over the content, so a content write cannot introduce a
script tag, an iframe or an arbitrary embed. Two consequences, and both are
features. The assistant cannot place a tracking pixel or a third-party embed into
a page — if that capability is ever wanted it arrives as its own decision, with
its own gate, not as a side effect of a content write. And a prompt-injection
payload that reaches the content path cannot become executable markup on the
site: the last conversion step from text to script is missing. This is worth
saying in the product copy, in the terms ADR-061 Decision 4 permits — describe
what runs on a write, do not claim a boundary.

**It forecloses candidate (c) as the v1 answer**, and candidate (a) entirely for
content. Both reasons are below.

### The candidates, kept for the record

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

**Why (a) is rejected for content specifically.** "No user" is an honest answer
for a filesystem, option or raw-SQL write, and it is the answer every command
shipped so far gives. For a content write it is a bug generator: this ADR's own
cost line for (a) — capability checks that "fail or behave unpredictably" inside
builder save paths and third-party hooks — describes a class of defect that
surfaces on someone else's plugin, on a customer's site, days later. There is no
version of that failure we can debug from the control plane.

**Why (c) is rejected as the v1 answer.** The mapping it depends on does not
exist, would not exist for every operator on every site if it did, and would need
a fallback that is (a) or (b) anyway — so choosing (c) is choosing (b) plus a
mapping. Worse, it makes a control-plane approval mint site-side authorship for a
person who never touched the site, which is a *worse* audit story than the one it
was chosen for. Not deferred, rejected.

**Whatever is chosen, three properties are required of the answer.** It is the
same principal the revision records and every capability check sees — not one
answer for authorship and another for authorization. It is recorded on the
proposal row, so the approval screen can render it as a control-plane-derived
fact before a human approves, per ADR-061 Decision 3. And it is stated on the
capability panel per site, because on a fleet the answer may legitimately differ
between sites and an operator must not have to guess which. The decision above
satisfies all three; a future revision of it must satisfy them too.

**Routing.** `security-reviewer` with `model: "opus"` **before any content code is
written** — this is a new principal, and it touches the agent protocol and
site-side privilege, both of which are on that list. Then the build order is
`database-engineer` (proposal and staging schema) → `backend-architect`
(control-plane half) → `wp-agent-engineer` (`apps/agent`), per the rule that a
change spanning a migration and code is agents in sequence and never one agent
doing both.

## Other open questions

**Does raw content ever cross to the model? Decided 2026-08-24: yes, inside the
fence, with provenance, never as instructions.** ADR-061's threat model assumes
prompt injection succeeds and constrains blast radius, and one of the constraints
is that the highest-density carriers — raw file contents, raw log bodies, raw
command output — are returned by no Phase 1 tool at all. Content operations put
that under direct pressure: the moment a content tool returns post bodies or
builder documents to the model, the densest injection carrier is back, sourced
from exactly the population ADR-061 names as attacker-controlled. The fork was
structured, control-plane-derived summaries only, or raw content with the
sanitizers doing the work.

The first branch is refused because it produces a content assistant that cannot
read content, which is not the product. The second is taken because the threat it
would be defending against is one this architecture **already assumes succeeds**:
ADR-061 puts the real boundary at structural authorization outside the model loop,
precisely so that a successful injection changes nothing about what is permitted.
We cannot make the model safe by starving it; we make it safe by ensuring nothing
it reads can change what it is allowed to do.

The bound is exact. Page content, plugin-supplied error strings, site names and
every other site-originated string reach the model inside a fenced envelope whose
provenance attribute is emitted from **our** database and never from the content
itself, under a standing preamble stating that the enclosed material is reference
text that cannot change what is permitted. **That envelope is an ADR-061 Phase 1
deliverable, not a Phase 2 one** — see ADR-061 amendment A13 — because site names
and plugin error strings reach the model in Phase 1 regardless, and a fence that
arrives after the first untrusted string has already conceded the precedent.

The ship gate travels with the decision: a **planted hostile site name**. A site
whose name is an injection payload must produce an approval screen that still
renders correctly and a set of permitted actions that is unchanged.

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

## Why this is still Proposed

The hard blocker closed on 2026-08-24 and this ADR did **not** move to Accepted.
Recording why, because "the blocker closed" is exactly the moment a status line
gets changed on momentum:

1. **Its own gate is what would lift.** Accepting this ADR releases the sentence
   at the top — *no content-write code ships until it is Accepted*. That sentence
   is the only thing standing between the wave plan above and someone starting
   Wave 1. It should be released when the work is ready to start, not when the
   argument is finished.
2. **ADR-060's precedence puts this behind open rank-1 work.** Content operations
   are differentiation, rank 5. The site-scope chokepoint and the fail-closed
   audit helper are auth-boundary items and both are open (ADR-061 amendments A9
   through A11). Accepting a rank-5 ADR while rank-1 items are open does not
   violate the freeze clause — this ADR adds no externally-reachable surface by
   itself — but it advertises readiness that the precedence order does not
   support, and precedence is reversed by exactly this kind of drift.
3. **Checklist items remain genuinely open, and two of them cannot close on
   argument.** Item 8 needs a measurement that cannot be taken until Layer A is
   serving live traffic. Item 4 needs a snapshot mechanism for post rows that does
   not exist, because the existing version stack covers files and posts do not
   live on the filesystem. Neither closes by anyone thinking harder.
4. **The wave plan is provisional on a query nobody has run.** Item 7 is decided
   on criteria, and the criteria do not move when the numbers arrive — but the
   ordering does, and the query costs an afternoon. Accepting the ADR before it
   runs converts "we reasoned this" into "we decided this" in every downstream
   reading.
5. **`security-reviewer` has not seen the principal or the four-command token
   scoping.** Both are on the standing list, both route with `model: "opus"`, and
   the attack-test suite that the whole "not really privileged" claim rests on has
   not been written, let alone watched go red.

**What closes it.** The five above, in that order. Items 3 and 6 are expected to
close *inside* their waves rather than before them, and this ADR should say so
when it is accepted rather than carry them as permanent open marks.

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
- No content operation that requires a human-held browser session per site, and
  none that requires a machine-held one either.
- **No content write that can produce executable markup.** The execution principal
  has no `unfiltered_html`, so content passes through core's post-content
  sanitizer. Script, iframes and arbitrary embeds are out of reach of this
  surface, and putting them back in reach is a separate decision with its own
  gate — never a side effect of a content feature.
- **No AI-authored change that looks human-authored.** The executing principal is
  fixed and the revision names it. A future per-site `post_author` override moves
  the display field only.
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
- The principal answer adds something to defend: a new site-side object on every
  managed site, deletable by the site's own admin, visible on the Users screen,
  and carrying a support-documentation cost.
- The wave plan buys the narrowest builder capability first and defers the rest.
  That is the intended trade, and the cost is that operators on the deferred
  builders see a named absence for longer.

**What has to exist before any Phase 2 content code is written.**

- Every checklist item closed. Item 2 closed on 2026-08-24; the rest are listed
  with their reason in [Why this is still Proposed](#why-this-is-still-proposed).
- The **pre-development tasks** in
  [Scope: the wave plan](#scope-the-wave-plan): the fleet-inventory query, the
  outbound-prober confirmation, and the signature-corpus match audit.
- The **service-principal attack-test suite** green, with each attack planted as
  passing first and both outputs pasted alongside their commands. This is a ship
  gate; it is not satisfied by a reviewer agreeing that the principal looks safe.
- `security-reviewer` with `model: "opus"` on the principal decision, on the
  four-command token scoping, and on the snapshot and revert paths.
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
