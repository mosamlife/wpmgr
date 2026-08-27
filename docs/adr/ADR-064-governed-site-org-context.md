# ADR-064 — Governed per-site and organisation context

**Status:** Proposed · **Date:** 2026-08-27
**Supersedes/relates:** ADR-060 (argued, under an interpretation of its
undefined term "surface" rather than a settled reading, as not gated by its
freeze clause — see Relationship, below, including the flag that this
interpretation may belong in a superseding ADR-060 amendment), ADR-061
(extends the facts-vs-instructions boundary drawn in its Decision 3 into a
stored, versioned, editable surface; reopens none of its seven decisions)

This ADR records the decision to build governed, persistent, per-site and
per-organisation context: human-authored information about a site or an
organisation that every future model-facing surface — the assistant of
ADR-061, and whatever reads or writes content after it — is handed as input
alongside what it observes for itself. It fixes the vocabulary for four
related but distinct kinds of context so they stop being blurred together,
and it fixes the precedence order across all seven layers of information a
model can end up holding, two of which ADR-061 already specified as part of
its accepted design and five of which this ADR is the first record of. (As
Decision 7 and Decision 11 below note, "ADR-061 specified it" and "it is
running today" are different claims — the two inherited layers are decided,
not deployed.)

---

## Context

Per-site context was previously deferred with no product reason attached to
the deferral — it was simply not scheduled. That deferral is reversed here:
governed per-site context ships with the **first usable assistant release**,
not after it. Until this document, there was no ADR recording that context
exists as a concept, what it contains, who may change it, or what happens
when two sources of it disagree. That is the largest single gap in the
assistant record — larger than any of the seven decisions ADR-061 made,
because every one of those decisions assumes there is a body of site facts
and rules for the model to read, and none of them says where that body
lives, who is answerable for what is in it, or what stops a lower-privileged
edit from quietly widening what a higher one intended to hold the line on.

This document is that record.

## Vocabulary

Four terms get fixed here, for the whole project, not only for this
feature. Blurring them is the default failure mode, and it hides a real
security difference each time it happens.

- **Persistent site context.** Human-managed information, entered by an
  operator and kept until an operator changes it: brand voice, audience,
  terminology, design rules, restrictions. It exists at two scopes —
  organisation and site — described in Decision 1.
- **Session context.** Information scoped to one conversation or one run.
  Never persisted, never versioned, discarded when the run ends. It is layer
  6 of the model below, and nothing in this ADR's storage ever sees it.
- **Detected site facts.** What the control plane or the agent observes
  about a site — builder, plugins, versions, theme, capabilities — reported,
  not authored. Facts, never instructions: a plugin name is a data field the
  model reads, and it does not get to change what the model is permitted to
  do. ADR-061 Decision 3 establishes that same non-authority posture for the
  approval screen specifically — control-plane-derived facts only, with
  model-authored text confined to one quarantined slot (ADR-061:174-178) —
  and this ADR generalises it from that one screen to detected facts
  wherever they reach the model, rather than drawing a second, unrelated
  boundary for context. ADR-061 does not itself make a general claim about
  tool output; that generalisation is this ADR's, not an inherited one.
- **Learned memory.** Information automatically inferred and saved from
  previous work, with no human review of the specific claim before it starts
  shaping a later run. **This is not being built.** See the dedicated
  section below.

The distinction that matters most: **static, human-authored instructions are
not learned memory**, even though both end up as text a later run reads. The
difference is not stylistic — it is who is answerable for the content. A
line of persistent site context exists because a named person holding a
write credential typed it; it is dated, versioned, and attributable, and the
organisation that owns it can inspect and revoke it. A line of learned
memory would exist because a model inferred it from something that
happened, with no equivalent moment of human authorship behind it. Filing
the second thing under the same name as the first is how a project ends up
trusting inferred, unreviewed text with the same weight as an operator's own
instruction — and the fact that a learned-memory system typically starts out
seeded with human-written material is exactly what makes the two feel alike
right up until they diverge. This ADR keeps them separate by construction:
one is built, the other is not, and the line between them is the line drawn
in this paragraph.

---

## Decision 1 — Seven layers, highest first; a lower layer can never widen a higher one

1. **WPMgr security policy** — immutable; not editable by any customer, any
   skill, or any model.
2. **Organisation defaults** — set once by the organisation, applies across
   every site in it.
3. **Site overrides** — may narrow or specialise layer 2; may never grant
   what layer 1 or layer 2 withheld.
4. **Detected site facts** — delivered as data the model reads, never as
   directions it follows.
5. **Approved skill instructions** — fenced as untrusted; a skill is data
   and can never grant permission, whatever its approval status says about
   its provenance.
6. **Session context** — this run only, discarded after.
7. **Learned memory** — **not built.** Deferred to a named later release
   behind an exit gate; see the dedicated section below.

**A lower layer can never widen a higher one.** This is the one property in
this document that outranks every other detail in it, and it is worth being
precise about why.

**This property is mechanically enforced for restrictions and advisory only
for guidance, and that split is itself part of what is being decided here,
not a downstream cost of deciding it.** Decision 3 splits every field on the
two context tables into two kinds: restrictions, a closed structured set
where "does this edit widen what a higher layer set" is a well-defined
comparison, and guidance, free text where "wider" and "narrower" are not
defined relations. Decision 4's write-time rejection can only ever apply to
the first kind — a machine can refuse a widening restriction because it can
name the comparison it is making; it cannot refuse a widening *tone*,
because no such comparison exists to run. A site's guidance can still read
as pulling against an organisation's intent in prose, and nothing here
catches that mechanically. Calling this a limitation discovered later, in
the trade-offs, would misstate it: it is a boundary of what "never widen"
can mean for free text, fixed by Decision 3's split, at the same moment the
guarantee itself is stated.

Every other decision below — the write-time check that rejects a widening
edit, the fail-closed audit, the effective-context preview, the
injection-safe fencing, the refuse-on-load-failure rule — exists only to
protect this property. If it did not hold, an attacker who controls nothing
but layer 4 (a crafted plugin name reported by a compromised site) could
attempt to restore a permission layer 1 revoked; a careless or compromised
site-level edit could silently undo an org-wide restriction the organisation
never saw change; a quarantined skill, or injected content riding inside any
layer, could try to turn "read-only" into "read-write" simply by asserting
it in prose. None of those attempts have to be *detected* to be defeated —
they have to be *structurally incapable of succeeding*, because the layer
that would have to grant them ranks below the layer that withheld them.

This is the same shape of guarantee ADR-061 Decision 2 already relies on for
approval — "the proposer's credential cannot approve" is a statement that a
lower-privileged actor cannot manufacture what only a higher one may grant.
This ADR generalises that guarantee from two credential classes to seven
context layers. It is what makes an organisation's policy, or a site
operator's override, mean what it says, rather than mean "unless something
further down the chain contradicts it" — which is not a policy at all.

---

## Decision 2 — The control plane is the source of truth; the agent caches, it does not decide

Layers 1 through 3 are authored and stored on the control plane. The agent's
role is layer 4: it reports what it observes about a site, and it may cache
locally only what safe execution of its own work requires — enough to avoid
re-probing a site's filesystem or database on every step of a single run.
The agent never holds an opinion about layers 1 through 3, never merges
them locally, and is never asked to resolve a conflict between them; that
resolution happens once, on the control plane, per Decision 4.

**What happens to a cache when context changes.** Every context row — an
organisation's layer-2 row, a site's layer-3 row — carries a version number
that increments on every accepted write. The control plane may hold a
materialised "effective context" for a site to avoid re-running the
seven-layer resolution on every request; that materialised copy is
versioned against the same numbers, and any write to any layer beneath it —
an org-policy edit, a site-override edit, or a fresh facts report from the
agent — invalidates it immediately. The next read recomputes; nothing is
ever served that predates a write to a layer it depends on. Symmetrically,
the agent's own local fact cache is never patched or merged: each new
inbound facts report supersedes the previous one wholesale at the version
level, because a fact is a snapshot of what was observed, not an
accumulating record the agent itself is trusted to reconcile.

**This materialised cache never holds layer 6.** Only layers this ADR's
storage actually sees — the versioned rows of layers 2 and 3, and the
agent's own versioned fact reports (layer 4) — ever participate in it;
session context (layer 6) is, by the Vocabulary section's own definition,
never persisted, so it is structurally excluded from any cache keyed on
stored version numbers, not merely excluded by convention. The
model-facing assembly path takes the materialised (or freshly resolved)
layers 1 through 5 and 7 as one input and the calling run's own live
session state as a second, separate input, and unions the two at call
time; no cached artifact is ever shared between two runs' session content,
because no cached artifact ever contains session content in the first
place.

---

## Decision 3 — Storage: tenant-scoped, append-only versions, and two kinds of field

Context lives in two tenant-scoped tables, shaped alike: one keyed to an
organisation (layer 2), one keyed to a site (layer 3). Neither table is
ever updated or deleted in place. A write inserts a new version row
carrying the full resulting snapshot, the version number, the author's
principal id, a provenance tag (manual edit, restore, or a later
machine-assisted import if one is ever built), and a timestamp. "The
current context" is defined as "the latest version row for that
organisation or site" — a read, not a separate mutable row — so restoring
or diffing history never has two representations of the present to keep in
sync.

**The site table's tenant scope is stamped per version row, not derived
live from the site's current owner.** Every other site-owned row in this
codebase is scoped by a live foreign key to `site.organisation_id`, correct
because those rows have no history to protect: the row means "as of now,"
and "now" tracks whoever currently owns the site. A context version row
means "as of when this was written," so it stores the organisation id that
owned the site *at the time of that write*, set once and never rewritten by
a later transfer. Decision 12 depends on this: it is what lets a transfer
reset the site's active context without also retroactively reassigning
authorship of everything written before it.

Fields on both tables split into two kinds, and the split is load-bearing
for Decision 4:

- **Restrictions** — a closed, structured set: allow/deny lists, named
  boundaries a policy can set ("never do X"), the kind of rule where "does
  this edit widen what a higher layer set" is a well-defined comparison a
  machine can make.
- **Guidance** — free text: brand voice, audience, terminology notes, style
  preferences. "Wider" and "narrower" are not defined relations over
  arbitrary prose, and this ADR does not pretend otherwise.

Both kinds are versioned identically. Only restrictions get the mechanical
never-widen check in Decision 4, because that check needs a defined
comparison to mean anything, and claiming to enforce one over free text
would be a check that always passes without ever having tested the thing it
claims to test.

---

## Decision 4 — Conflict resolution: fixed layer order at read time, a rejection at write time

Reading resolves deterministically: walk the seven layers in the order of
Decision 1, and at each layer apply only what that layer is permitted to
apply — a restriction may only be added to or left alone by every layer
below the one that set it; guidance from a lower layer is always taken as
additional context, never as a retraction of a higher layer's guidance.
There is no merge heuristic, no "most specific wins" beyond the ordering
itself — site overrides already exist expressly to be more specific than
organisation defaults, and that is the only kind of specificity this design
recognises.

The more important half of this decision is where the check actually lives.
**A widening attempt on a restriction field is rejected at the write path,
not silently dropped at read time — at every writable layer, against every
layer above it, not only against the nearest one.** A site-level edit is
checked against both its organisation's layer-2 policy and WPMgr's layer-1
policy; an organisation-level edit is checked against WPMgr's layer-1
policy. WPMgr's layer-1 policy is not a row in either context table
(Decision 3 names exactly two, for layers 2 and 3) — it is the same fixed,
structured restriction set this Decision's read-time walk already applies
first, and the write path loads and checks the proposed snapshot against
that same set rather than against a database row, so the absence of a
layer-1 table row is not the absence of a layer-1 check. Any one of these
checks failing fails the write outright, with a reason naming the
restriction and the layer that set it, and no new version is created. The
alternative — accept the edit, and simply have the read-time resolver
ignore the part that overreached — is the shape this project's own working
agreements name as its signature defect: a failure quietly coerced into a
value that looks like success. An editor who typed a rejected restriction
and got a green save screen would have no way to know their edit did
nothing where it mattered most.

**Restrictions must reach enforcement, not merely storage.** Decision 11
quarantines the full resolved context, restrictions included, inside a
fence where "nothing... can grant a tool call, alter a capability, or
change what the model is permitted to do" — which means a restriction that
only ever reaches the model as fenced prose has no mechanical force of its
own; a model that disregarded it would not be tripping anything the
control plane could detect. Restrictions therefore reach enforcement
through a second, independent path from the write-time check described
above: the same tool-dispatch chokepoint that already computes the
capability and tool registry a request may use (ADR-061 Decisions 5 and 6)
consults this Decision's resolved restriction set before invocation, as an
additional deny input alongside the capability grant it already checks —
not as something the model is trusted to honour after reading it. A tool
call or write that a resolved restriction forbids is refused at dispatch,
with a typed reason naming the restriction and the layer that set it, the
same shape Decision 13's `409` already uses for a blocked context write;
it is never permitted and merely left unreported. This is what makes
Decision 3's restrictions/guidance split load-bearing rather than
cosmetic: restrictions are the subset of context a chokepoint outside the
model's own compliance actually consults before the model can act, and
guidance is not, and is never claimed to be.

---

## Decision 5 — Version history, diff and restore are first-class

Every accepted write is a new version row (Decision 3); nothing is ever
edited in place, so history is exact by construction rather than by a
separate audit trail bolted alongside it. The version list for an
organisation or a site shows, per entry: author, timestamp, provenance, and
a diff against the immediately prior version — field-level (added/removed
list entries) for restriction fields, line-level for guidance fields, since
those are the natural units of change for each kind.

**A diff is only ever computed between two versions stamped to the same
organisation.** A version whose immediately prior row carries a different
organisation stamp has no eligible predecessor for diff purposes and is
rendered as a baseline instead: nothing computed, nothing to compare, never
a diff against a version the requester may not be entitled to see. This
covers two cases the same way: the first version a site has after a
transfer to a new organisation (Decision 12), where the immediately prior
row is the last one the previous organisation wrote, and, at the other end
of history, the very first version an organisation or site ever has, which
has no prior row of any kind. Both render identically — a baseline entry,
not a computed diff — because both are the same fact: nothing behind this
version belongs to the story the requester is entitled to read. This is a
property of what "diff" means over this schema, not a permission check
layered onto it afterward: the operation is defined not to reach across an
organisation-stamp boundary, the same way a direct read of a row on the
far side of one already can't (Decision 12).

Restore never mutates history. Restoring version N creates a new version
whose snapshot equals version N's, attributed to the principal who asked
for the restore, with provenance recorded as `restore` and a pointer to the
version restored. It is, in every respect the schema cares about, a write
like any other, which means it goes through the same widen-check and the
same audit transaction as Decision 4 and Decision 7 — a restore is not a
back door around either.

---

## Decision 6 — Permission model: explicit capabilities, and the assistant never holds write

Reading and editing context are entries in the same explicit capability
registry ADR-061 Decision 5 introduced for API keys, not a new role tier —
`context.org.read` / `context.org.write` at organisation scope,
`context.site.read` / `context.site.write` at site scope. Read access
follows existing fleet-read access: anyone who can already see a site's
inventory can see the context that explains the rules governing it, at the
organisation and the site scope that cover that site. Write access is
narrower by default: organisation-scope write is held by organisation
administrators; site-scope write additionally requires access to that
specific site, mirroring how site-scoped API keys already work.

**No assistant-kind principal is ever issued `context.org.write` or
`context.site.write`, under any grant, at any tier.** This is not a
configuration default that an operator could accidentally loosen; per
ADR-061 Decision 5, a capability an assistant must never hold is handled by
absence from the registry available to that principal type, not by a switch
inside it. The reason is the same property Decision 1 exists to protect,
applied to write access rather than to content: a model that could edit the
context constraining it could widen its own leash, and the fact that layer
7 (learned memory) is not built yet is precisely what keeps that path from
opening by default the moment it is.

---

## Decision 7 — Audit is fail-closed here too, because the ledger entry is the product claim

Every context write — org, site, or restore — must produce one row in the
existing hash-chained audit log, in the same transaction as the version
row. **If the audit append fails, the version write fails with it; nothing
commits.** This is the same posture ADR-061 Decision 2 adopted, on paper,
for approvals — and in both places it is a decision, not yet a mechanism.
`apps/api/internal/audit/audit.go:498-500` documents the recorder as
best-effort today: "callers should log but not fail the request if Record
errors, except where the audit trail is itself the point." No fail-closed
variant of that recorder exists in the codebase, for approvals or for
anything else. This ADR and ADR-061's approval path both need the identical
new primitive — a transactional, fail-closed audit append — and neither one
already has it; building it is required new work, not a reuse of something
ADR-061 already shipped. This departs from this codebase's best-effort audit
convention for a specific reason: on most paths, a lost audit row degrades
an investigation after the fact, and refusing the underlying action over it
would be a worse outcome than a gap in the log. That trade is wrong here.

The differentiator this feature exists to support is that an operator, or
an organisation's auditor, can later prove exactly what instruction set a
model was given at a point in time. A context edit that committed without a
verifiable ledger entry would be a governance-relevant change to a fleet's
persistent context with no attributable record behind it — indistinguishable
after the fact from a change nobody can attest to, which is the exact
failure ADR-061 built the whole approval design to make impossible on the
dispatch side. The same reasoning applies unchanged on the context side: the
ledger entry is not a record of the edit, it is the claim that the edit is
what it says it is.

---

## Decision 8 — Effective-context preview is a correctness requirement, not a nicety

An operator can request the exact resolved text a given site's model-facing
surface will be handed — every layer's surviving contribution, in the order
of Decision 1, with what each layer added and what got narrowed or blocked
by a higher one and why, plus the byte accounting from Decision 9.

This is argued as correctness, not convenience, because of what it makes
verifiable. Decisions 1, 4, and 7 are all claims about a resolution process
an operator otherwise cannot see happen. Without a preview, "a lower layer
can never widen a higher one" is an internal implementation assertion this
document makes about code the operator has no way to check — exactly the
gap ADR-061 Decision 2 closed for approvals by putting the human decision
inside a hash-chained, independently verifiable ledger rather than asking
the operator to trust that a confirmation step occurred. The preview is
this feature's equivalent instrument: the thing an operator can point to
instead of taking the design's word for it.

For that equivalence to be real rather than cosmetic, **the preview must
call the same resolution function the model-facing assembly path calls, not
a second implementation of the same idea.** A preview built by re-deriving
the seven-layer walk independently would drift from the real path the first
time either one changed, and an operator reading a preview that no longer
matches what the model actually receives is worse off than an operator with
no preview at all, because they would believe they had checked something
they had not.

**The preview never carries live session content, because none exists at
preview time.** A preview request is operator-initiated from the
dashboard, independent of any running assistant session, so layer 6 has
nothing to contribute when it is called — the preview resolves and
displays layers 1 through 5 and 7, with layer 6 shown as not applicable
outside a live run. This is the same resolution function the model-facing
path calls (above), called with an empty layer-6 input; it is not a
different function or a second code path for the no-session case, and it
is the reason Decision 2's materialised cache can be keyed on layers 1
through 5 and 7 alone (see Decision 2) without ever needing to account for
which run is asking.

---

## Decision 9 — Budgets are counted in bytes, not tokens

Each layer's contribution to the resolved context, and the resolved context
as a whole, is capped in bytes. This matches ADR-061 Decision 6's reasoning
exactly and for the same reason: there is no tokenizer on this side of the
boundary for whatever model ends up reading the result, and the
byte-to-token ratio moves with the model. Bytes are the one unit this
codebase can measure honestly on its own side of that boundary.

Truncation happens at a field or record boundary, never mid-field, and is
marked explicitly rather than silently dropped — the same discipline
ADR-061 already applies to tool output. When the total exceeds budget,
truncation starts at the **lowest** surviving layer and works upward:
session context first, then skill instructions, then any facts overflow,
then site overrides, then organisation defaults; layer 1 is never
truncated. This is the same ordering that decides "widen" in Decision 4,
applied to scarcity instead of permission — a higher layer was written with
more deliberate, organisation-wide intent than a lower one, and a
resource-constrained resolution should starve the layer with the least
standing to complain, not the one with the most.

---

## Decision 10 — Secret detection on write, without echoing what it found

Every write to a context field is scanned before it is accepted. A value
shaped like a credential — an access key, a private key block, a
connection string, a bearer token, a password-shaped string with the right
entropy — is refused. **The refusal names the category of what was found
and never echoes the matched text back to the caller, into a response body,
or into a log line at a level anything downstream persists.** Echoing the
match would relocate the secret into an error message or a log rather than
keep it out of the system, which defeats the point of refusing the save in
the first place.

This check lives at the same single write chokepoint as the widen-check in
Decision 4 and the audit transaction in Decision 7 — one place a write
passes through, not a validation layer that a second entry point could miss.

---

## Decision 11 — Prompt-injection defence assumes injection succeeds

The full resolved context — every layer that survived Decision 4's
resolution — must reach the model inside a single quarantined block, marked
untrusted, using the per-response-nonce fencing scheme ADR-061 specifies for
site-origin text. **That fencing does not exist yet.** ADR-061 lists the two
sanitizers that implement it — one for text on its way to the model, one for
text on its way to a human — under "What has to exist before v1 ships"
(ADR-061:544,548-550), and its own verification note records the surface
they belong to as unshipped (ADR-061:570-572). ADR-061 also never mentions
skill content anywhere in its text, so nothing about how skill instructions
would be fenced can be read off it. This ADR reuses the *design*, not an
existing working mechanism — the fencing this decision depends on is
required new work, shared with ADR-061 and with ADR-062's content-operations
design, not a dependency this ADR can treat as already satisfied.

**Every layer goes inside the fence, including the human-authored ones.**
Layers 2 and 3 are written by an operator with a real write credential, and
that is exactly why they are trusted as *content* — but they are not
trusted as *instructions with authority over what the model may do*, for
two reasons. First, once assembled into prose alongside facts and skill
text, the model has no reliable way to tell which sentence came from which
layer, so a single fencing discipline applied uniformly is simpler than one
that depends on the model doing provenance-sensitive parsing correctly.
Second, a human-authored field is not immune to becoming a laundering path
for injected content — a compromised dashboard session, or a careless
paste of text copied from an untrusted source into a "brand voice" field,
puts attacker-authored text behind a legitimate credential. Treating even
trusted-provenance layers as data the model reads, never authority the
model obeys, costs nothing when the content is exactly what it claims to be,
and it closes the one path where "authored by a human" would otherwise have
been read as "therefore safe to follow."

The system-level instruction the model is given states plainly that nothing
inside the fence can grant a tool call, alter a capability, or change what
the model is permitted to do — only the resolved capability set and tool
registry from ADR-061 Decisions 5 and 6 determine that, and those are never
assembled from anything inside the fenced block.

---

## Decision 12 — Export, deletion, retention, site transfer, and multisite

**Export.** Context is small, textual, and already served whole by the
read endpoints in Decision 13; a broader account-data export tool, if one
is ever built, includes context by calling that same read path rather than
touching the tables directly, so there remains exactly one way to read
context regardless of caller.

**Deletion.** Context tables are ordinary tenant-scoped relational rows —
no object-storage blobs — so they are freed by the same tenant-purge path
every other tenant-scoped table rides: a soft delete, a grace window, and
the async sweep that runs the privileged cascade
(`apps/api/internal/org/purge_worker.go`). That worker's own history is the
reason this is called out rather than assumed: its file header records that
an earlier build forgot five of seven object-storage roots and silently
orphaned client data in them until an adversarial review caught it. Context
storage does not add an eighth root — it has no object-storage component at
all — and this ADR states that explicitly so the next person reading the
purge worker's list does not have to wonder whether context belongs on it.

**Retention.** Version history has no independent expiry in v1. It is
retained for the life of the organisation or site, the same posture the
audit log itself takes — append-only, no TTL — and is removed only when the
organisation or site is removed, via the path above. An independent
retention window for context history, if ever wanted, is a separate
decision, not one this ADR invents speculatively.

**Site transfer clears the site layer, and seals — never deletes — the
history written before it.** When a site moves to a different
organisation, its layer-3 row is reset to empty; the site inherits only
layers 1 and 2 of the organisation it now belongs to. Site-level context was
authored under the old organisation's brand and policy assumptions, and
carrying it into a new tenant would apply narrowing rules nobody in the new
organisation wrote or reviewed — this is an authorship-integrity concern,
not tidiness. The cleared version is itself a version (author: the transfer
operation, provenance: `transfer`), so the reset is auditable and the prior
organisation's history remains attributed to it rather than disappearing.

That attribution is a mechanism here, not only a record. Because every
version row's organisation id is stamped at write time and never rewritten
(Decision 3), the transfer's own cleared version is the first row stamped
with the destination organisation, and every row before it stays stamped
with the source organisation permanently. Decision 13's history routes
authorize list, item, and restore against a version row's *stamped*
organisation, not against the site's current one, so a
destination-organisation principal — however much fleet visibility it has
over the site going forward — can list or view only versions stamped with
its own organisation id, starting at the transfer.

**The transfer's own cleared version is a baseline, not a diff target,
by Decision 5's general rule.** A diff is always computed against the
immediately prior version, and that cleared version's immediately prior
row is, by construction, the last one the source organisation wrote — a
different stamp. Decision 5 already defines what that means: no eligible
predecessor, rendered as a baseline, not a computed comparison. Nothing
adjacent has to special-case the transfer for this to hold — it falls out
of "diff never crosses an organisation-stamp boundary" being a property of
the operation itself, applied here rather than invented here. Getting this
right matters specifically because the alternative failure is quiet: a
comparison that rendered the sealed predecessor's full content as
"everything removed" would disclose exactly what the sealing above exists
to withhold, through a route — the diff view embedded in an ordinary
list — nobody would think to check separately from the read access it
looks like it's respecting. Pre-transfer versions are **retained, never
deleted**: the same append-only, no-TTL
posture the Retention paragraph above already takes for everything else in
this history, and for the same kind of reason Decision 7 already gives the
audit log — a durable record kept for accountability is not the same claim
as a live self-service read anyone can still reach, and this ADR does not
conflate the two. Once the transfer completes, **nobody reaches those rows
through the ordinary site-scoped routes** — not the destination, sealed by
the stamped-organisation check above, and not the source organisation
either, whose access to those rows ends the same way its access to the rest
of the site does on transfer, through the ordinary `context.site.read`
capability check in Decision 6, which already requires access to the
specific site and which transfer already revokes. Retention here means
what it means for the audit log elsewhere in this document: nothing is
destroyed, so a legitimate future need — a dispute, an investigation — is
never met with "it's gone," but meeting that need runs through whatever
privileged path this codebase already uses to read data no ordinary
capability reaches, not through a route this ADR adds.

**If the source organisation wants its own durable copy of its
pre-transfer history, the concrete mechanism is the read endpoints in
Decision 13 themselves** — `GET .../context/versions` and its item and diff
routes — called directly, before the transfer completes, while
`context.site.read` still holds; this is not the broader account-data
export tool the Export paragraph above names as conditional on ever being
built, because pointing at that tool specifically would overclaim a
mechanism this codebase does not confirm exists. Decision 13's routes are
at least a real, specified part of this ADR's own build — they do not exist
yet because nothing in this Proposed ADR does, but "what has to exist
before this ships" below already requires them for reasons independent of
transfer.

**The harder truth checked directly rather than assumed: no mechanism for
moving a site to a different organisation exists anywhere in this
codebase today, and this paragraph's own earlier claim that "the transfer
operation itself predates this ADR" was wrong.** No query in
`apps/api/db/query/sites.sql` updates a site's organisation — its full
query list is `CreateSite`, `GetSite`, `ListSites`, `DeleteSite`, and a set
of narrow setters (tags, age recipient, metadata, app health, connection
state), never a reassignment of ownership; no `apps/api/internal/site/*.go`
or `apps/api/internal/org/*.go` file names transfer, reassignment, or an
organisation-id update on a site; `site_shares.sql` is a distinct
mechanism (granting another tenant read access without changing
ownership); and no other ADR, and no `CHANGELOG.md` entry, describes one
either. **So this section is not a hook into a live workflow — it is a
contract with nothing yet to attach to**, the same shape Decision 1 already
uses honestly for layer 7 (learned memory): a rule this ADR commits to
now, for a capability that does not exist, so that whoever eventually
builds site-to-organisation transfer inherits a decided answer rather than
having to invent one under deadline. Until that mechanism exists, calls it,
and is the thing that actually creates the `transfer`-provenance version
above, none of this section's rules have anything to run against — which
also means **nothing in this ADR, or anywhere else in this codebase today,
guarantees a pre-transfer copy is taken before access would be revoked**,
because the event that would need to trigger that guarantee does not yet
happen at all. Whether a future transfer mechanism should require, prompt
for, or simply document this loss is recorded as a named open question
below, owned by whoever eventually builds it — not assumed answered by a
workflow that, checked directly, turns out not to exist.

Pre-transfer versions are also **sealed against restore**: `restore` on a
pre-transfer version id is refused outright and unconditionally, for every
caller, including a principal in the original authoring organisation,
because this ADR has already decided the destination's active context
starts empty, and a restore that reintroduced pre-transfer text would
silently reopen the exact authorship-integrity gap clearing the layer was
meant to close.

**Multisite.** A managed WordPress installation is already one WPMgr site
record regardless of whether that installation is itself a WordPress
multisite network — `Multisite` is a boolean field on the existing site
model, not a fan-out into per-subsite records:

```sh
grep -n "Multisite" apps/api/internal/site/model.go
```

Site-level context therefore attaches to that same site id and needs no
separate design for multisite. Per-subsite context granularity, if ever
wanted for a managed multisite network, is a future extension keyed to
whatever subsite identifier the product introduces for that purpose, and is
out of scope here.

---

## Decision 13 — API contracts

All of the following are routes on the existing, already-authenticated
dashboard API — not the externally-reachable assistant surface from
ADR-061. See Relationship, below, for why that placement matters.

```http
GET    /api/v1/orgs/{orgId}/context                              current org context (layer 2)
PATCH  /api/v1/orgs/{orgId}/context                               partial field write -> new version
GET    /api/v1/orgs/{orgId}/context/versions                     paginated history
GET    /api/v1/orgs/{orgId}/context/versions/{versionId}         one version's full snapshot
GET    /api/v1/orgs/{orgId}/context/versions/{versionId}/diff    diff against the prior version
POST   /api/v1/orgs/{orgId}/context/versions/{versionId}/restore new version = that version's content

GET    /api/v1/sites/{siteId}/context                             current site context (layer 3)
PATCH  /api/v1/sites/{siteId}/context                              partial field write -> new version
GET    /api/v1/sites/{siteId}/context/versions                     paginated history
GET    /api/v1/sites/{siteId}/context/versions/{versionId}         one version's full snapshot
GET    /api/v1/sites/{siteId}/context/versions/{versionId}/diff    diff against the prior version
POST   /api/v1/sites/{siteId}/context/versions/{versionId}/restore new version = that version's content

GET    /api/v1/sites/{siteId}/context/effective                    Decision 8's preview: all seven
                                                                     layers resolved, per-layer byte
                                                                     accounting against Decision 9
```

`PATCH` accepts a partial set of fields; the server applies them onto the
latest version's full snapshot to build the new version's snapshot, so
history and diff always operate on complete snapshots rather than deltas
of deltas.

For a site, the list and item history routes are additionally scoped to
versions stamped with the site's current organisation once a transfer has
occurred (Decision 12): a pre-transfer version is retained but excluded
from both. `diff` follows Decision 5's general rule rather than a
transfer-specific carve-out: a version whose immediately prior row carries
a different stamp has no eligible predecessor and renders as a baseline,
never a computed comparison against a version on the other side of the
boundary. `restore` against a pre-transfer version id
is refused unconditionally, for any caller.

Error contracts, all with a machine-readable reason code:

- `409` — a write would widen a restriction a higher layer set (Decision 4);
  the reason names the field and the layer that blocked it.
- `422` — the write contains something shaped like a credential
  (Decision 10); the reason names the category found, never the match.
- a distinct `context_unavailable` domain error — effective-context
  resolution could not complete; every caller of Decision 8's resolution
  function, including the model-facing assembly path, treats this as a hard
  failure per Decision 14, never as an empty result.

Capabilities `context.org.read` / `context.org.write` /
`context.site.read` / `context.site.write` gate these routes per Decision 6.
No route in this list is reachable from an assistant-kind credential, both
because that principal type is never granted the write capabilities and
because these routes are not registered on the tool surface ADR-061
Decision 6 defines — an assistant only ever receives the *output* of
Decision 8's resolution, inside the fence of Decision 11, never a path to
call these endpoints itself.

---

## Decision 14 — If context cannot be loaded, the call is refused

If Decision 8's resolution cannot complete — a database error, a cache
invalidated with no fresh copy yet computed, a corrupted version row — the
caller that needed that context is refused outright. It is never given an
empty, partial, or stale-but-unmarked result as a stand-in.

This is stated explicitly because the tempting alternative is quieter and
wrong in the way this project has been wrong before: falling back to "just
layer 1," or to an empty layer 2 and 3, and letting the call proceed looks
like resilience but is actually a different assistant than the one the
operator configured, running under the operator's name, with the operator
never told it happened. A refusal is loud, attributable, and correct; a
silent substitution is exactly the failure mode this codebase's own working
agreements name as its signature defect, applied to the surface this ADR
governs.

---

## Learned memory is deferred, not "later"

Learned memory — automatic inference and persistence from previous work,
with no human review of the specific claim before it shapes a later run —
is not scheduled inside this ADR's scope. It is deferred to the first
release after the Skill Store ships, owned by `backend-architect` with
`security-reviewer` review, and it does not start without an explicit exit
gate:

- a written threat model for agent-authored text re-entering the prompt on
  a later run;
- the same fencing mechanism Decision 11 depends on and requires as new
  work (it is specified by ADR-061 but not yet built there either, and
  ADR-061 does not mention skill content at all) — that mechanism, once
  built, must be the one that carries learned memory when it exists, not a
  second bespoke one;
- opt-in per organisation, never on by default for an existing tenant;
- reviewable by a human before it takes effect on a later run, not after;
- removable, at the granularity of a single inferred item, not only as an
  all-or-nothing switch;
- off by default.

**What WPMgr offers instead, in the meantime, is what most of the real need
turns out to be.** Human-curated persistent site context, as designed in
this ADR, already covers it: "this client dislikes serif faces" is a
context edit an operator makes once, dated and attributable, not an
inferred memory a model decided to keep. The gap between that and learned
memory is real but narrow, and it is exactly the gap the exit gate above is
designed to close carefully rather than skip.

---

## Relationship to ADR-060 and ADR-061

Nothing here reorders ADR-060's phase precedence or touches its freeze
clause's applicability, and nothing here reopens any of ADR-061's seven
decisions.

**This work is argued here as not gated by ADR-060's freeze clause, even
during a phase where an auth-boundary item may be open — but this is an
interpretation of an undefined term, stated as one, not a settled fact.**
The freeze clause reads: "No new externally-reachable surface ships while an
auth-boundary item is open" (ADR-060:44). Its test is **surface**, and
ADR-060 never defines that word; it also never uses "boundary" to name the
test itself (the compound "auth-boundary" names which *category* of open
item triggers the clause, per ADR-060's own five-item precedence list, not
what the clause is testing for), and it never draws a caller-class
distinction anywhere in its text. An earlier draft of this section argued
the exemption on both of those absent grounds — "a new resource on an
existing, already-audited boundary, not a new boundary" and a caller-class
split from ADR-061's assistant route — and that argument is withdrawn here
because it restated the clause in words ADR-060 does not use and then
exempted this document from the restatement.

The argument actually available is narrower, and rests on the word ADR-060
does use. ADR-060 states its own scope explicitly: "This is deliberately
narrow. It does not freeze feature work in general — internal work, and
work that adds no new externally-reachable surface, is unaffected"
(ADR-060:46-48), a boundary restated at :88-90 as "a standing check on any
change that adds a new externally-reachable surface." Every route in
Decision 13 is a new resource on the existing, already-authenticated
dashboard API — the same perimeter every other organisation and site
settings endpoint already answers on, using the same authentication this
ADR adds no new mechanism for. Read against "does not freeze feature work in
general," a new resource on an already-externally-reachable, already-audited
perimeter is a plausible reading of *not* adding a new externally-reachable
surface. ADR-061's assistant route is a new surface on any reading, because
nothing answering on it today is externally reachable at all.

**That plausible reading is still this document's interpretation of a term
ADR-060 leaves undefined, not ADR-060 saying so.** Deciding what "surface"
means for the freeze clause is a question about ADR-060, and answering it
inside a section of ADR-064 sets a precedent that any later ADR could
similarly interpret its way past the one absolute prohibition ADR-060
states. If this reading is going to govern future cases beyond this one
document, it belongs in a superseding ADR-060 amendment that defines
"surface" once, reviewed as a change to ADR-060 itself — not settled
ad hoc, document by document, by whichever ADR needs the exemption next.
This document proceeds on the reading above for its own purposes, but does
not treat that reading as binding on any future ADR; a later document
claiming the same exemption should point at an ADR-060 amendment, not at
this paragraph. **That amendment does not exist yet, and until it does this
document can be merged and read as Proposed but cannot move to Accepted**
— see Open questions, below.

This ADR's vocabulary leans on ADR-061 Decision 3, correctly attributed:
that decision draws the line between control-plane-derived facts, which
alone may appear on the approval surface, and model-authored text, which is
confined to a single quarantined slot after the facts and before the
controls (ADR-061:174-178). This ADR reuses that same discipline — a fixed,
narrow rendering path for anything that is not control-plane-derived —
rather than inventing a second one for context. It also reuses ADR-061
Decision 5's capability-registry pattern and ADR-061 Decision 6's
byte-budget reasoning rather than inventing parallel mechanisms for either.
It does not touch ADR-061's site-scoping posture (Decision 4): context rows
are scoped and resolved per site through the application-layer chokepoint
that decision already established as this codebase's honest current state,
not a database-level guarantee this ADR is claiming to add.

**The two tables Decision 3 introduces are in scope for that same deferred
migration, and are named here for that purpose.** ADR-061 Decision 4
pre-commits the migration's scope to "adding site-scope restrictive policies
to the tables above and to the proposal tables" (ADR-061:370-371); it does
not and could not name this ADR's organisation-context and site-context
tables, since they did not exist when it was written. Naming them here, in
the ADR that creates them, is how that scope stays accurate without editing
ADR-061 itself to add a forward reference to a document it predates. When
the deferred migration lands, it is expected to add a restrictive site-scope
policy — not merely tenant isolation — to both tables named in Decision 3,
alongside everything ADR-061 already named.

**Where resolved context reaches a human matters, and it is not the
approval screen.** ADR-061:200-204 states that the proposal record's single
quarantined note is "the only proposer-controlled free text anywhere in the
schema" — a claim scoped to the proposal schema specifically. This ADR
introduces a second body of operator-authored free text, the guidance
fields of Decision 3 above, and that text is deliberately never rendered on
ADR-061's approval screen: the human-facing surface for resolved context is
Decision 8's effective-context preview, a separate, dashboard-only,
operator-requested read, not a field on the proposal or approval record. A
future change that let guidance text appear inline on the approval screen
itself would put a second source of free text into the one schema ADR-061
built around holding exactly one, and would need to revisit ADR-061:200-204
explicitly rather than drift into it unannounced.

---

## Consequences

**What this forecloses.**

- No assistant-kind or other automation principal is ever issued
  `context.org.write` or `context.site.write`, under any grant, without
  superseding this ADR.
- No merge-based conflict resolution. Precedence is positional and fixed by
  Decision 1; a future "smart merge" needs its own ADR, not a patch to this
  one.
- No best-effort audit for a context write. Decision 7's posture cannot be
  loosened to an asynchronous or queued append without superseding this ADR,
  for the same reason ADR-061 Decision 2 already forecloses it for
  approvals.
- No serving of a resolved context that skipped the widen-check (Decision 4)
  or the secret scan (Decision 10), regardless of caller or code path.
- No promotion of session context into persistent context without a human
  explicitly making that edit; there is no automatic "remember this for next
  time" absent the learned-memory exit gate.

**What it costs.**

- Guidance fields have no mechanical widen-check — this is a property of
  Decision 1 itself, not a gap found afterward. The practical consequence
  operators feel: a site-level edit to free text can still read as pulling
  against organisation intent even though it cannot touch a structured
  restriction, and a general-purpose text-widening detector does not exist
  to catch it. Claiming to enforce one would be a check that always passes
  without ever testing anything.
- The effective-context preview (Decision 8) is only as trustworthy as its
  identity with the real resolution path. Keeping the two from drifting
  apart is an ongoing engineering discipline, not a one-time build cost.
- A verbose organisation policy can crowd out a site's own guidance under
  the byte budget in Decision 9. The truncation order favours higher layers
  deliberately, which means the cost of scarcity is always paid by the
  layer with the least standing to complain about it — an accepted
  trade-off, not an oversight.

**What has to exist before this ships.**

- One resolution function that both the model-facing context assembly and
  the effective-context preview call — not two implementations of the same
  seven-layer walk.
- Version, author, and provenance columns on both context tables, with
  `UPDATE`/`DELETE` revoked from the application role at the privilege
  level, the same way the audit log already enforces append-only.
- The `context.*` capabilities added to the existing registry, with a
  registry test asserting no assistant-kind principal can ever be granted a
  context-write capability — the same test shape ADR-061 Decision 5 already
  uses for capabilities an assistant must never reach.
- Planted-failure proofs, each pasted with its command and its before/after
  output: a save shaped like a credential is rejected, and the rejection is
  asserted not to contain the matched text; a site-level edit that would
  remove an organisation-set restriction is rejected, and the prior version
  is asserted unchanged; a forced audit-append failure is asserted to leave
  no new context version committed; a tool call that a resolved restriction
  forbids is refused at dispatch even when the fenced context the model was
  handed contained no hint of the restriction's wording (Decision 4); after
  a simulated site transfer, a principal in the destination organisation can
  list only post-transfer versions, the first post-transfer version's
  `diff` renders as a baseline rather than a computed comparison against
  its sealed, source-stamped predecessor, and any restore of a
  pre-transfer version id is refused for every caller, source organisation
  included (Decision 12); two concurrent runs against the same site,
  supplied with different layer-6 session inputs, resolve to different
  effective context and neither run's session content appears in the
  other's result or in the cached layers either one reads (Decision 2 and
  Decision 8).
- `make test-integration` coverage of tenant scoping on both new tables,
  run before merge — context is exactly the shape of tenant-scoped data
  ADR-061 Decision 4 already found this codebase gets wrong by default when
  it is not checked.
- **A restrictive site-scope policy on both context tables, not only tenant
  isolation**, delivered as part of the deferred migration ADR-061 Decision
  4 named and this ADR extends to include them (see Relationship, above).
  Tenant isolation alone would let any principal scoped to the tenant read
  or resolve another site's context; the restrictive policy is what confines
  a read to the sites a principal can actually see.
- **The injection-fencing mechanism itself**, per Decision 11 — the two
  sanitizers ADR-061 lists under its own "What has to exist before v1 ships"
  (ADR-061:544,548-550). This ADR's quarantine in Decision 11 has nothing to
  run against until that mechanism is built.
- **A fail-closed audit-append path**, per Decision 7 — `Record()` in
  `apps/api/internal/audit/audit.go` is best-effort today
  (`apps/api/internal/audit/audit.go:498-500`), and no fail-closed variant
  exists for this ADR's transaction or for ADR-061's approval path either.
- **A site-to-organisation transfer mechanism, which this ADR depends on
  but does not build.** Verified this pass: no query in
  `apps/api/db/query/sites.sql` reassigns a site's organisation, no file
  under `apps/api/internal/site/` or `apps/api/internal/org/` implements
  one, and no other ADR or `CHANGELOG.md` entry describes one. Decision
  12's transfer rules (clear the site layer, stamp and seal history, treat
  a stamp-boundary version as a diff baseline per Decision 5, refuse a
  cross-boundary `restore`) are a contract for whatever eventually calls
  them, not a hook into something already running — they have nothing to
  run against, and are untestable end to end, until a real transfer
  mechanism exists and invokes them.

---

## Open questions

Named and owned, rather than answered with an invented mechanism this
document has not earned the right to assert. Neither blocks merging this
ADR as Proposed; both block it moving to **Accepted**.

1. **What "surface" means for ADR-060's freeze clause.** Relationship,
   above, argues Decision 13's routes are not gated by ADR-060's freeze
   clause, but states plainly that this is this document's interpretation
   of a term ADR-060 leaves undefined, not a settled reading, and that a
   reading meant to bind later ADRs belongs in a superseding ADR-060
   amendment, not in this one. That amendment does not exist yet.
   **Owner:** whoever holds ADR-060 next (its author or a designated
   successor). **Resolution:** a change to ADR-060, not to this document —
   this ADR does not re-argue the point further and is not the place to
   settle it.
2. **Concurrency control on `PATCH` (Decision 13).** `PATCH` applies a
   partial field set onto the latest version's snapshot. Two concurrent
   `PATCH` calls touching disjoint fields can both read version N and both
   succeed, and the later write's snapshot will not carry whichever fields
   the earlier write changed unless the later caller happened to resend
   them too. Whether to require a client-supplied base version, an `ETag`,
   or an equivalent conditional-write mechanism, and what the reason code
   and response shape for a stale write look like, is an HTTP-contract
   choice this ADR does not carry — recording that the choice is
   unmade is this ADR's job; the wire format is not. **Owner:**
   `backend-architect`, to decide and land before Decision 13's `PATCH`
   routes are built, not discovered after a concurrent-write incident.
3. **Should a future site-transfer mechanism require or prompt a
   pre-transfer copy of the site's context history — and, first, who
   builds site-to-organisation transfer at all?** Checked directly this
   turn, not assumed: no query in `apps/api/db/query/sites.sql` reassigns
   a site's organisation, no file under `apps/api/internal/site/` or
   `apps/api/internal/org/` implements one, and no other ADR or
   `CHANGELOG.md` entry describes one either — the capability Decision 12's
   transfer rules attach to does not exist anywhere in this codebase
   today, and has no owner. Decision 12 seals pre-transfer context from
   the destination and closes the source organisation's own ordinary
   access to it at the moment a transfer would complete; the one way to
   keep an accessible copy is calling Decision 13's read endpoints before
   that moment. Nothing today makes that happen, because nothing today
   makes a transfer happen at all. This document does not build
   site-to-organisation transfer and does not appoint its owner — it can
   only record that whoever eventually does inherits this question:
   whether that mechanism should block on, prompt for, or simply document
   the loss of ordinary access to pre-transfer history. **Owner:**
   whoever eventually builds site-to-organisation transfer, unassigned
   today; that build is itself a prerequisite this ADR depends on and
   does not provide (see "What has to exist before this ships").
