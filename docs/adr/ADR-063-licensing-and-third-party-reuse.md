# ADR-063 — Repository licensing, and the rules for third-party reuse

**Status:** Accepted · **Date:** 2026-08-24
**Supersedes/relates:** Relates to ADR-061 (its Decision 1 sites the assistant surface on the control plane; §3 below reaches the same siting from licensing alone) and ADR-062 (Phase 2 content operations, which is where new agent-side dependencies would otherwise be proposed).

This ADR records what this repository is actually licensed under, what follows
from that for anyone adding a dependency, and the rules an engineer applies when
they want to reuse something they did not write. It exists because the licence
position was assumed wrong in a planning round, in the direction that costs the
most: the constraint everyone was defending against does not apply here, and the
one that does apply was undocumented.

---

## Context

Two questions kept being answered from memory and being answered differently:
what licence is the control plane under, and what may be added to the WordPress
plugin. Both have exact answers that live in files at the repository root, and
neither answer is where an engineer looks when they are about to run
`composer require`.

The specific failure worth recording: a planning pass reasoned from the premise
that the control plane is closed-source, and therefore that adding copyleft code
to it would be fatal. **The premise is false**, and reasoning from it produces
two errors at once — it refuses adoptions that are in fact permitted, and it
never surfaces the real constraint, which sits on the plugin rather than on the
control plane. A wrong licence premise does not fail loudly. It just quietly
makes every downstream row of the decision table wrong in the same direction.

---

## 1. What this repository is licensed under

Two licence files exist in the tracked tree, and no more. The command, and what
it returned when this ADR was written:

```sh
git ls-files | grep -iE '(^|/)LICENSE' \
  | grep -c . \
  || { echo "FAIL: no tracked LICENSE file matched -- moved or renamed, which is not 'unlicensed'" >&2; exit 1; }
#   2
```

Re-run it rather than trusting the figure; a third licence file appearing is
exactly the event this ADR needs someone to notice. The refusal on zero is
deliberate for the standing reason: `grep … | wc -l` takes its exit status from
`wc`, so a renamed path prints `0` and exits `0`, and "this repository has no
licence" is not a claim to arrive at by accident.

**The root `LICENSE` is AGPL-3.0, and it covers `apps/api`.**

```sh
grep -nE 'GNU AFFERO GENERAL PUBLIC LICENSE|Version 3, 19 November 2007' LICENSE | head -2
#   1:                    GNU AFFERO GENERAL PUBLIC LICENSE
#   2:                       Version 3, 19 November 2007
```

There is no separate licence file under `apps/api/`, so the root file governs it,
along with `apps/web`, `apps/marketing` and `packages/**` — everything the
carve-out below does not name.

**`LICENSE-AGENT` is MIT, and it covers `apps/agent` and `apps/tracker`.**

```sh
grep -nE '^MIT License|^Applies to:|^The rest of this repository' LICENSE-AGENT
#   1:MIT License
#   5:Applies to: apps/agent (WordPress plugin) and apps/tracker (JS web-vitals).
#   6:The rest of this repository is licensed under AGPL-3.0 (see LICENSE).
```

`NOTICE.md` states the same split in one sentence:

```sh
grep -n 'control plane is AGPL' NOTICE.md
#   3:WPMgr's control plane is AGPL-3.0; the WordPress agent is MIT-licensed. See the
```

**This split is an owner ruling, not an inference from the files above: the
agent is MIT, full stop.** `apps/agent` and `apps/tracker` ship under
`LICENSE-AGENT` (MIT); everything else in this repository ships under the root
`LICENSE` (AGPL-3.0). The consequence is stated in full in §2, and it is the
load-bearing fact for every reuse decision in §3 and §4: **GPL-family code
cannot enter the MIT-licensed agent without relicensing the whole plugin**, and
relicensing a plugin already listed on wordpress.org under MIT is a strategic
change to a public listing, not a dependency bump.

**The plugin as distributed currently declares two different licences**, which
is the contradiction that made the ruling above necessary to write down rather
than assume. The wordpress.org listing page says GPLv2-or-later:

```sh
grep -nE '^License' apps/agent/readme.txt
#   8:License: GPLv2 or later
#   9:License URI: https://www.gnu.org/licenses/gpl-2.0.html
```

while the plugin header says MIT:

```sh
grep -nE '^ \* License' apps/agent/wpmgr-agent.php
#   10: * License:           MIT
#   11: * License URI:       https://opensource.org/licenses/MIT
```

That is two different strings in two files a user reads side by side. It was
never a licence violation — MIT permits redistributing under a GPL-compatible
licence, so the readme's line is not false, only misleading next to the header
— but it is exactly the ambiguity that let the wrong premise in the Context
section above go unchecked, and it is why F1 below is no longer left open for
someone to rediscover: `readme.txt:8` and its restatement at `readme.txt:51`
are being corrected to MIT, tracked in GH #547.

**The repository is public.** Public is not permissive: publishing source grants
nothing beyond what the licence files grant, and it gives this project no
additional right to anyone else's code either. What being public *does* do is
discharge the AGPL's source-offer obligation conveniently, and make every commit
message, comment and committed document permanently visible.

---

## 2. What follows: the constraint is on the plugin, not on the control plane

**Adding AGPL-3.0-or-later code to `apps/api` creates no new licence obligation.**
The network clause extends copyleft past distribution to network interaction, and
for a proprietary hosted service that is fatal — it would compel publication of
the whole service. This service is already AGPL-3.0 and its source is already
public, so the obligation is already discharged. This is the single most
important licensing fact in this record and it points the opposite way from the
one everybody expects.

**Adding GPL-2.0-or-later or AGPL code to `apps/agent` relicenses the plugin.**
MIT is permissive; a copyleft dependency makes the combined distributed work
carry the copyleft licence. The result is that `LICENSE-AGENT` and the plugin
header become false, the wordpress.org listing changes what it promises, and a
carve-out that two other files depend on stops being true. That is a strategic
change to a plugin published in a public directory, not an engineering detail
that belongs inside a dependency-bump PR.

**So the direction that is safe for `apps/api` is unsafe for `apps/agent`.** The
asymmetry is the whole content of this section, and it is why the reuse rules
below are stated per destination rather than per package.

### This independently supports siting the MCP surface in the control plane

ADR-061 Decision 1 puts the assistant server on the control plane, and it argues
that from credentials and fan-out: a per-site inbound path would need a
privileged WordPress user and a standing credential on every managed site.

**Licensing reaches the same siting by a different route, and that is worth
recording because two independent arguments for one decision are harder to
overturn than one.** The mature open-source MCP components for WordPress —
`wordpress/mcp-adapter` and `wordpress/php-mcp-schema`, both from the WordPress
project — are GPL-2.0-or-later. Adopting either into `apps/agent` relicenses the
plugin, per §2. Adopting either into `apps/api` is not a licence event at all.
An architecture that keeps the MCP surface in the control plane and lets the agent
keep speaking the existing signed-command protocol is therefore also the cheapest
licensing outcome, and the two arguments do not depend on each other.

The point of writing this down is narrower than it looks: it stops a licence
consideration from being discovered *by* a `composer require` in the agent's
`composer.json`, at which point the change has already been made and the argument
becomes about reverting rather than about deciding.

---

## 3. The reuse rules

Four dispositions, and every reuse decision resolves to exactly one of them.

### ADOPT UPSTREAM

**Take the package from its own upstream, at its own version, as an ordinary
dependency.** This is the right answer whenever the thing wanted is a library
that exists independently of wherever it was first seen.

- Check the licence against the **destination** first, per §2. MIT and BSD are
  fine anywhere. GPL-2.0-or-later and AGPL are fine in `apps/api` and are a
  relicensing event in `apps/agent`.
- Retain the package's own notice text, and add an entry to `NOTICE.md` **and**
  the third-party section of `apps/agent/readme.txt` in the same PR. The existing
  `matthiasmullie/minify` entries in both files are the template.
- Where an independently-licensed artifact is taken and modified, mark it with a
  sidecar file naming the upstream and its SPDX identifier. Apache-2.0 also
  requires stating significant changes in modified files, and a sidecar satisfies
  both obligations in one place.
- Never copy a package out of a local reference tree. Fetch it from its own
  upstream, so what is in the lockfile is what the upstream published.

### INTERFACE-COMPATIBLE ONLY

**Implement the published contract from the contract's own documentation.** MCP,
OAuth 2.1 and its RFCs, the WordPress Abilities API, and a page builder's storage
format are all in this class.

Copyright protects **expression** — the particular source text somebody wrote —
not the interface, not the wire format, not the field names a protocol mandates,
and not the facts of how a third-party product stores its data. Implementing the
same protocol from its own specification is not copying even when the result is
necessarily similar, and no attribution is owed for it. A storage format is a fact
about a third product, discoverable by opening any site that uses it.

Two practical rules follow. Derive from the specification, the vendor's own
documentation, or a real install — never from somebody else's implementation of
the same specification, because at that point what is being read is expression.
And in the PR description, cite the specification: *"implements the published X
specification"*, with a link to X.

### REIMPLEMENT FROM BEHAVIOUR

**Take the design insight; write the implementation.** This is the disposition for
an idea observed working somewhere — a pattern, an architecture, an error-handling
posture — where what is valuable is one or two sentences of understanding and the
code is incidental.

Almost everything worth having lands here, including where a licence would in
principle permit more, and the reason is not licensing at all. Honest copyleft
attribution requires naming the source. The house rule forbids naming a competitor
product in code, a comment, a commit message or a committed document, and this
repository is public so a commit message cannot be recalled. Those two obligations
cannot both be satisfied, so the reuse does not happen and the understanding is
what carries over — §4 below states why that is structural rather than a policy
choice, and gives the working practice that makes it operable. State the design
in your own words, build it against this codebase's own primitives, and route
anything touching auth, tokens, tenancy, capability checks or the command
protocol to `security-reviewer` regardless of how small it looks.

A note on porting, because it is the shape most likely to be mistaken for
reimplementation: **a translation is a derivative work.** Rewriting somebody's PHP
as Go line by line carries the original's licence with it. Reading it to
understand what it does, then writing Go that behaves the same way, does not. The
difference is whether the source is open next to yours while you type.

### DO NOT USE

**Prose.** Consent-screen text, error messages, admin-screen copy, tooltips,
hint strings. Prose is the most copyrightable material in any tree and copied
prose is the easiest infringement to detect and the hardest to explain. Read
somebody else's consent screen for the *checklist* of what a user must be told —
scope, grantee, revocation, duration — and then write every sentence fresh.

---

## 4. The reuse test: fact, method, or source?

The four dispositions in §3 cover *where a package comes from*. This section
covers the question that comes up more often and gets confused with it: looking
at somebody else's plugin to understand how it solves a problem, and asking what
of that may cross into this repository.

Copyright protects expression. It does not protect ideas, methods, or facts.
Three examples, stated at the grain an engineer actually works at:

- **A fact about a third-party plugin is not protected.** "This function returns
  `false` for both failure and a no-op, so a caller has to re-read state to tell
  them apart" is an observation about how a third-party product behaves. Use it
  freely — it is no different from noticing the same thing by reading that
  product's own public documentation or support forum.
- **A method or architecture is not protected.** "Compute each site's capability
  set from one declarative table of plugin, version floor and gate" is a design
  pattern, not a fixed sequence of expression. Reimplement it freely, in this
  codebase's own idiom.
- **Their source code — copied, or lightly edited — is protected**, and it cannot
  enter this repository. Not behind a rename, not reformatted, not "inspired by"
  in a comment. It relicenses `apps/agent` on entry per §2, regardless of how
  small the fragment is, and licence aside, it is simply not this project's to
  redistribute.

Two audited reference implementations were read while evaluating third-party
integration behaviour for this project: an audited reference implementation
(AGPL-3.0-or-later) and a second audited reference implementation
(GPL-2.0-or-later). Both are GPL-family. Both relicense `apps/agent` the moment
their expression — not the facts or methods learned from reading them — crosses
into it. Neither is named here as a competitor; both are named only as the
licence fact that governs what may be taken from them, which is: facts and
methods, never text.

### The working practice that makes this enforceable

> Read the source. Write a behavioural specification in your own words. Build
> from the specification, not from the source.

The specification cites the **third-party plugin vendor's own documentation** —
Elementor's, ACF's, WooCommerce's — never the reference implementation read to
write it. A specification written this way names no competitor, carries no
licence, and can live in this repository under ordinary review.

The middle step is not ceremony. Reading someone else's code and then
immediately writing code that does the same thing risks reproducing its
structure, its naming, and its sequence of operations unconsciously, even when
every line is retyped from memory rather than copied — that is still how a
derivative work gets made. Handing the specification to the actual
implementation step, rather than the source, is what breaks that chain: the
engineer who builds the feature is working from a description of *behaviour*,
checked against the vendor's own docs, and has not read — and does not need to
have read — the reference implementation at all.

### The trap that makes this non-optional, not aspirational

GPL-family licences condition reuse on attribution: taking code under
GPL-2.0-or-later or AGPL-3.0-or-later and using it here requires crediting where
it came from. Rule 4 (§5 below) forbids naming a competitor product in any
committed file. An engineer cannot satisfy both obligations on the same piece of
text at the same time — attributing the source names it, and naming it breaches
rule 4.

**There is no compliant path that copies GPL-family source into this repository,
independent of anyone's intent to follow the licence correctly.** This is the
strongest argument in this document, and it is worth stating plainly rather than
leaving it implied: it is not a policy this project could relax by deciding to
be more careful, because the two obligations contradict each other the instant
actual source text crosses over, before intent enters into it at all. The
specification-first practice above is not a workaround for that trap — nothing
is, on source text. It is the only shape of reuse that avoids the trap entirely,
because a behavioural specification, sourced from the vendor's own
documentation, carries no licence to attribute in the first place.

---

## 5. Standing rules for engineers

1. **Check the licence against the destination before adding any dependency.**
   `apps/agent` is MIT and a copyleft dependency relicenses it. `apps/api` is
   AGPL-3.0 and has no such conflict.
2. **Add every new dependency to `NOTICE.md` and to `apps/agent/readme.txt`'s
   third-party section in the same PR**, following the existing entries.
3. **Take libraries from their own upstream**, never out of a local reference
   copy.
4. **Never name a competitor product as a source of design or code** — in code,
   a comment, a commit message, a committed document, a tracked ignore file or
   a PR title. `apps/marketing/**` is the only carve-out for competitive
   discussion, and nothing else in these rules goes there.
   `apps/agent/readme.txt` is the public wordpress.org listing page and stays
   under the strict rule regardless.

   **Exempt from this rule: naming something as an integration target.**
   `.github/workflows/ci.yml`'s Docs vocabulary check enforces a narrower ban
   than the sentence above and says so in its own preamble — "functional source
   references (conflict-detection slugs, plugin-signature seeds, compat
   handling) are out of scope" — and this rule was stricter than its own gate
   until this revision, which a sibling ADR breached in the same PR it was
   accepted in. Naming a third-party plugin as *the thing being detected,
   integrated with, or made compatible with* is a fact about interoperability,
   not a claim of provenance: conflict-detection slugs, `plugin_signatures` seed
   data, wordpress.org directory slugs used to detect an installed plugin, and
   compat-handling code paths that must name what they are compatible with all
   stay. This exemption is narrow on purpose — it covers naming a target, never
   describing its features, crediting it as an inspiration, or citing it as a
   reference implementation. Those remain forbidden without exception, for the
   structural reason given in §4.
5. **Never write a defensive disclaimer.** No "not derived from", no "original
   implementation". Saying it implies the question arose, which reads worse than
   describing what was built.
6. **Reference material is used and then deleted, never ignored.** A tracked
   ignore file is committed, so a line naming a local reference directory
   publishes the reference. There is no correct entry; there is only deleting the
   directory.
7. **When unsure, ask before writing the code**, not after. An unrouted commit to
   a public repository cannot be recalled.

---

## Open for an owner ruling

**F1 — the agent declared two different licences. Ruled; fix in flight.** The
plugin header said `MIT` (`apps/agent/wpmgr-agent.php:10`) and the wordpress.org
listing said `GPLv2 or later` (`apps/agent/readme.txt:8`, restated at `:51`).
This was never a violation — MIT permits redistribution under a GPL-compatible
licence, so the readme's line was an accurate statement of what the distributed
*package* was offered under — but it left "can GPL code enter the agent"
unanswerable, because the answer flips depending on which licence is taken as
true. **The owner has ruled: the agent is MIT.** §1 above states the ruling and
its consequence as settled fact. The one remaining step is mechanical, not a
decision: correcting `readme.txt:8` and `:51` to match, tracked in GH #547 and
routed to `wp-agent-engineer` for the plugin files and `docs-writer` for the
listing text. This item stays open only until that PR merges and the public
listing shows the corrected licence; the ruling itself is closed.

**F2 — keeping the MCP surface out of the plugin for licence reasons.** §2 sets
out why this is also the architecture already chosen, but the *decision* has three
real options and the owner picks one:

- **(b) Keep the entire MCP surface in `apps/api`** and let the agent keep
  speaking the existing signed-command protocol. Cheapest licensing outcome,
  changes nothing about `apps/agent`, and matches ADR-061 Decision 1.
  **This is the recommendation.**
- **(a) Accept the relicence** and update `LICENSE-AGENT` and the plugin header
  to GPL-2.0-or-later. This also resolves F1 in the other direction, and is
  incompatible with the ruling recorded in F1 above unless the owner revisits it.
  It is a strategic licence change to a plugin listed in a public directory.
- **(c) Implement the MCP wire protocol in the agent from the published
  specification** and bundle neither package. Permitted — the protocol is not
  copyrightable expression — but it is real work for no benefit given (b).

**This blocks the start of MCP work**, and it is a short ruling rather than a
long one. The reason it is written down before anyone needs it is that the
alternative is discovering it from a lockfile diff, after the choice has already
been made by whoever was in a hurry.

---

## Consequences

- The licence position is now citable from one file, with the commands that
  verify it. A future planning pass that reasons from a remembered premise can be
  checked against this in one command rather than one argument.
- The MIT/AGPL split is now ratified in an ADR rather than living only in
  `LICENSE-AGENT` and `NOTICE.md`; GH #547 brings `apps/agent/readme.txt` into
  agreement with it, closing the one file that still contradicted the ruling.
- Adding a dependency to `apps/agent` acquires a mandatory licence check against
  MIT. This is a small cost paid on every dependency bump, and it is the only
  mechanism that catches the relicensing case before it ships.
- The reuse dispositions in §3 give a PR description a vocabulary: a PR says
  which of the four it is doing, which makes a review about the right question.
- §4 gives an engineer a three-way test — fact, method, or source — for the
  question that comes up more often than a new dependency: what may be taken
  from reading a competing plugin. It also states, rather than leaves implied,
  that GPL-family source cannot be compliantly copied into this repository at
  all, because attribution and the anti-naming rule cannot both be satisfied.
- Rule 4 (§5) now states the integration-target exemption explicitly, matching
  what `ci.yml` already enforces and documents in its own preamble. A newly
  accepted rule that its own sibling ADR breaches in the same PR is the kind
  that gets informally suspended and never corrected; writing the exemption down
  is what stops that.
- F1 and F2 stay open until an owner rules or a fix merges. F1's ruling is
  closed and only its mechanical fix (GH #547) remains; F2 gates the start of
  MCP work. A later ADR supersedes this section when F2 closes; it is not
  edited away.
