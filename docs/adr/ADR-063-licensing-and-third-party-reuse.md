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
n=$(git ls-files | grep -icE '(^|/)LICENSE')
[ "$n" -eq 2 ] \
  || { echo "FAIL: expected exactly 2 tracked LICENSE files, found $n -- a moved, renamed or newly added licence file all change this ADR's premise" >&2; exit 1; }
echo "$n"
#   2
```

Re-run it rather than trusting the figure; a third licence file appearing is
exactly the event this ADR needs someone to notice, and the assertion above
now fails on that event too, not only on zero. `grep -c` alone already refused
to read a renamed path as "unlicensed" for the standing reason: `grep … | wc
-l` takes its exit status from `wc`, so a renamed path prints `0` and exits
`0`, and "this repository has no licence" is not a claim to arrive at by
accident. It did not, on its own, refuse a *third* file the same way — a count
above 2 still printed and returned success. The `-eq 2` above closes that gap.

**The root `LICENSE` is AGPL-3.0, and it covers `apps/api`.**

```sh
exp1='                    GNU AFFERO GENERAL PUBLIC LICENSE'
exp2='                       Version 3, 19 November 2007'
got1=$(sed -n '1p' LICENSE); got2=$(sed -n '2p' LICENSE)
[ "$got1" = "$exp1" ] && [ "$got2" = "$exp2" ] \
  || { echo "FAIL: LICENSE's opening two lines no longer read as the AGPL-3.0 title block -- got '$got1' / '$got2'" >&2; exit 1; }
echo "$got1"
echo "$got2"
#                     GNU AFFERO GENERAL PUBLIC LICENSE
#                        Version 3, 19 November 2007
```

There is no separate licence file under `apps/api/`, so the root file governs it,
along with `apps/web`, `apps/marketing` and `packages/**` — everything the
carve-out below does not name.

**`LICENSE-AGENT` is MIT, and it covers `apps/agent` and `apps/tracker`.**

```sh
exp1='MIT License'
exp5='Applies to: apps/agent (WordPress plugin) and apps/tracker (JS web-vitals).'
exp6='The rest of this repository is licensed under AGPL-3.0 (see LICENSE).'
got1=$(sed -n '1p' LICENSE-AGENT); got5=$(sed -n '5p' LICENSE-AGENT); got6=$(sed -n '6p' LICENSE-AGENT)
[ "$got1" = "$exp1" ] && [ "$got5" = "$exp5" ] && [ "$got6" = "$exp6" ] \
  || { echo "FAIL: LICENSE-AGENT's title or carve-out lines changed -- got '$got1' / '$got5' / '$got6'" >&2; exit 1; }
printf '%s\n%s\n%s\n' "$got1" "$got5" "$got6"
#   MIT License
#   Applies to: apps/agent (WordPress plugin) and apps/tracker (JS web-vitals).
#   The rest of this repository is licensed under AGPL-3.0 (see LICENSE).
```

`NOTICE.md` states the same split in one sentence:

```sh
exp3="WPMgr's control plane is AGPL-3.0; the WordPress agent is MIT-licensed. See the"
got3=$(sed -n '3p' NOTICE.md)
[ "$got3" = "$exp3" ] \
  || { echo "FAIL: NOTICE.md line 3 no longer states the AGPL/MIT split -- got '$got3'" >&2; exit 1; }
echo "$got3"
#   WPMgr's control plane is AGPL-3.0; the WordPress agent is MIT-licensed. See the
```

**This split is an owner ruling, not an inference from the files above: the
agent is MIT, full stop.** `apps/agent` and `apps/tracker` ship under
`LICENSE-AGENT` (MIT); everything else in this repository ships under the root
`LICENSE` (AGPL-3.0). The consequence is stated in full in §2, and it is the
load-bearing fact for every reuse decision in §3 and §4: **GPL-family code
cannot enter the MIT-licensed agent without relicensing the whole plugin**, and
relicensing a plugin already listed on wordpress.org under MIT is a strategic
change to a public listing, not a dependency bump.

**The plugin's tracked source declares two different licences**, which is the
contradiction that made the ruling above necessary to write down rather than
assume. `apps/agent/readme.txt`, the wordpress.org listing page, says
GPLv2-or-later:

```sh
exp8='License: GPLv2 or later'
exp9='License URI: https://www.gnu.org/licenses/gpl-2.0.html'
got8=$(sed -n '8p' apps/agent/readme.txt); got9=$(sed -n '9p' apps/agent/readme.txt)
[ "$got8" = "$exp8" ] && [ "$got9" = "$exp9" ] \
  || { echo "FAIL: readme.txt:8-9 no longer read as expected -- got '$got8' / '$got9'" >&2; exit 1; }
printf '%s\n%s\n' "$got8" "$got9"
#   License: GPLv2 or later
#   License URI: https://www.gnu.org/licenses/gpl-2.0.html
```

while `apps/agent/wpmgr-agent.php`, the plugin header, says MIT:

```sh
exp10=' * License:           MIT'
exp11=' * License URI:       https://opensource.org/licenses/MIT'
got10=$(sed -n '10p' apps/agent/wpmgr-agent.php); got11=$(sed -n '11p' apps/agent/wpmgr-agent.php)
[ "$got10" = "$exp10" ] && [ "$got11" = "$exp11" ] \
  || { echo "FAIL: wpmgr-agent.php:10-11 no longer read as expected -- got '$got10' / '$got11'" >&2; exit 1; }
printf '%s\n%s\n' "$got10" "$got11"
#    * License:           MIT
#    * License URI:       https://opensource.org/licenses/MIT
```

That is two different strings in two tracked files a user reads side by side.
It was never a licence violation in the tracked source — MIT permits
redistributing under a GPL-compatible licence, so the readme's line is not
false, only misleading next to the header — but **the split is worse on the
distributed artifact than the tracked tree alone shows.** `Makefile`'s
`agent-zip-wporg` target, which builds the zip actually uploaded to
wordpress.org, force-rewrote the staged plugin header's `License` field to
`GPLv2 or later` on every build, independent of what `wpmgr-agent.php` said.
Downloading and grepping the published zip confirms the consequence:

```sh
curl -sL 'https://downloads.wordpress.org/plugin/fleet-agent-site-manager.zip' -o /tmp/fasm.zip
unzip -p /tmp/fasm.zip fleet-agent-site-manager/fleet-agent-site-manager.php | grep -nE '^ \* License'
unzip -p /tmp/fasm.zip fleet-agent-site-manager/readme.txt | grep -nE '^License'
#   10: * License:           GPLv2 or later
#   11: * License URI:       https://www.gnu.org/licenses/gpl-2.0.html
#   8:License: GPLv2 or later
#   9:License URI: https://www.gnu.org/licenses/gpl-2.0.html
```

— version 0.61.146, checked when this revision was written: `GPLv2 or later`
in *both* the header and `readme.txt`, internally consistent as shipped, and
consistent with neither `LICENSE-AGENT` nor the ruling above. This was not a
stale line someone forgot to edit; it was a deliberate build step, so fixing
`readme.txt` alone would not have closed it — the next build would have
silently overwritten a corrected MIT header back to GPLv2.

**The owner has ruled: the agent is MIT.** GH #547 (PR #556, open as of this
revision) fixes both halves: `readme.txt:8` and its restatement at `:51` are
corrected to MIT, and the `agent-zip-wporg` header rewrite is removed so the
build stops overriding whatever the source declares. The same PR adds
`scripts/check-license-surfaces.sh` (`make check-licenses`, self-test `make
check-licenses-test`), which reads every agent licence surface — the plugin
header, both mu-plugin headers, `readme.txt`'s structured header and its
Description prose, `composer.json`, `apps/agent/NOTICE.md`,
`apps/agent/README.md`, `LICENSE-AGENT`, and the Makefile override itself —
and fails if any is missing or any two disagree. That script, not the
illustrative commands above, is the enforced, ongoing check that these
surfaces continue to agree, the same role `scripts/check-version-surfaces.sh`
already plays for version strings; running it against this tree confirms it
catches both halves of the live bug at once (`Makefile rewrites the plugin
header's License field during a build`, and `the agent declares more than one
license`), exit 1. **Until PR #556 merges and a new agent release ships, the
already-published wordpress.org zip stays `GPLv2 or later`, and every copy
already distributed under it keeps that grant — a licence already given is
not revocable by a later commit.** F1 below tracks what remains open.

**The repository is public.** Public is not permissive: publishing source grants
nothing beyond what the licence files grant, and it gives this project no
additional right to anyone else's code either. What being public *does* do is
discharge the AGPL's source-offer obligation conveniently, and make every commit
message, comment and committed document permanently visible.

---

## 2. What follows: the constraint is on the plugin, not on the control plane

**Adding AGPL-3.0-or-later code to `apps/api` creates no new relicensing
conflict.** The network clause (AGPL §13) extends copyleft past distribution
to network interaction — a modified AGPL work must offer users who reach it
over a network the same corresponding-source access a distributed copy would
carry — and for a proprietary hosted service that would ordinarily be fatal,
compelling publication of the whole service's source. `apps/api` is already
AGPL-3.0 and its source is already public, so §13's network-interaction duty
already applies to the service as a whole; a new AGPL-3.0-or-later dependency
does not make that duty apply where it did not apply before. This is the
single most important relicensing fact in this record and it points the
opposite way from the one everybody expects. **It does not waive the ordinary
obligations that travel with any dependency regardless of relicensing risk**:
retain the package's own notice text and add it to `NOTICE.md`, per §3's
ADOPT UPSTREAM disposition below, and where §13's second paragraph combines
in a GPL-3.0-licensed (not AGPL) component, that component keeps its own
GPL-3.0 terms rather than being absorbed into `apps/api`'s AGPL-3.0.

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

**This section supplies one of F2's two independent arguments; it does not
settle F2.** Whether the MCP surface ships at all, and which of F2's three
options builds it, is the owner's ruling below — this section explains why
option (b) is the cheapest licensing outcome, not that (b) has already been
chosen.

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
- Retain the package's own notice text, and add an entry to the root
  `NOTICE.md` in the same PR. If the package is bundled inside the plugin zip
  (added to `apps/agent`), also add it to `apps/agent/NOTICE.md` and to the
  Third-party/Credits section of `apps/agent/readme.txt` — that section is the
  public wordpress.org listing of what the zip actually contains, and it
  already states plainly that nothing else is bundled, so an entry there is a
  claim about the zip, not about what the control plane uses. A control-plane-
  only dependency the agent merely calls into (the RUCSS Go libraries recorded
  only in the root `NOTICE.md` are the standing example) stops at that one
  entry; listing it in `apps/agent/readme.txt` would misdescribe the zip.
  `matthiasmullie/minify` — recorded in all three files because it is actually
  bundled — is the template for a bundled dependency.
- Where an independently-licensed artifact is taken and modified, mark it with
  a sidecar file naming the upstream and its SPDX identifier — that covers
  provenance and, for Apache-2.0, the §4(d) NOTICE-file attribution
  obligation. It does not cover Apache-2.0 §4(b): that section requires the
  modified file itself to carry a prominent notice that it was changed, which
  a sidecar elsewhere cannot do on the file's behalf. Add that notice inside
  the modified file, in addition to the sidecar, not instead of it.
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
understand what it does, then writing Go that behaves the same way, reduces
that risk — it does not eliminate it by itself. Structure, naming and the
sequence of operations can themselves carry protected expression even when no
line is copied, and how close a resulting implementation can safely land is
fact-specific, not a line this document can draw in advance. The difference
between the two ends of this spectrum is whether the source is open next to
yours while you type; the middle of it is a judgement call, and rule 7 in §5
— ask before writing the code — is what to do with an uncertain case, not an
assumption that the practice alone clears it.

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
implementation step, rather than the source, is what meaningfully breaks that
chain: the engineer who builds the feature is working from a description of
*behaviour*, checked against the vendor's own docs, and has not read — and
does not need to have read — the reference implementation at all. It reduces
the risk of unconsciously reproducing someone else's structure; it is not a
guarantee that no output could ever be judged too close, which is why an
uncertain case still goes to rule 7 in §5 rather than being assumed clear.

### The trap that makes this non-optional, not aspirational

GPL-family licences condition reuse on attribution: taking code under
GPL-2.0-or-later or AGPL-3.0-or-later and using it here requires crediting
where it came from. Rule 4 (§5 below) forbids naming a competitor product in
any committed file, and that prohibition is not scoped to `apps/agent` or to
any other destination — it applies repository-wide. The two obligations
collide only when the source being credited *is* a competitor's: attributing
it names it, and naming it breaches rule 4. Attributing an ordinary
GPL-family dependency that is not a competitor — `wordpress/mcp-adapter` in
§2 above is exactly this case — names no one rule 4 forbids naming, and §3's
ADOPT UPSTREAM disposition covers it like any other upstream package, checked
against the destination as §2 already sets out.

**There is no compliant path that copies *competitor* source, under a
GPL-family licence, into this repository — in `apps/api` as much as in
`apps/agent`, because rule 4 is not destination-scoped.** This is the
strongest argument in this document, and it is worth stating plainly rather
than leaving it implied: it is not a policy this project could relax by
deciding to be more careful, because the two obligations contradict each
other the instant actual competitor source text crosses over, before intent
enters into it at all. **This does not reach ordinary third-party GPL-family
or MIT reuse with normal attribution** — this repository already ships
`matthiasmullie/minify` under MIT with proper credit, per §3 — **nor does it
reach `apps/agent`'s separate, narrower problem from §2**: any GPL-family
dependency there, competitor-sourced or not, forces a relicensing choice,
which is a strategic cost to accept or decline, not an attribution
impossibility. The specification-first practice above is not a workaround for
the competitor-source trap — nothing is, on actual source text. It is the
only shape of reuse that avoids the trap entirely, because a behavioural
specification, sourced from the vendor's own documentation, carries no
licence to attribute in the first place.

---

## 5. Standing rules for engineers

1. **Check the licence against the destination before adding any dependency.**
   `apps/agent` is MIT and a copyleft dependency relicenses it. `apps/api` is
   AGPL-3.0 and has no such conflict.
2. **Add every new dependency to the root `NOTICE.md` in the same PR.** If it
   is bundled inside the plugin zip, also add it to `apps/agent/NOTICE.md` and
   to `apps/agent/readme.txt`'s Third-party/Credits section — that page lists
   what ships in the zip, not what the control plane uses. Follow the existing
   entries, per §3.
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
listing said `GPLv2 or later` (`apps/agent/readme.txt:8`, restated at `:51`) —
and the published zip's header agreed with the listing, not with the tracked
source, because `Makefile`'s `agent-zip-wporg` target was rewriting it to
`GPLv2 or later` on every build. This was never a violation — MIT permits
redistribution under a GPL-compatible licence, so what actually shipped was an
accurate, internally consistent statement of what that *distributed package*
was offered under — but it left "can GPL code enter the agent" unanswerable,
because the answer flips depending on which licence is taken as true. **The
owner has ruled: the agent is MIT.** §1 above states the ruling and its
consequence as settled fact. What remains is mechanical, not a decision: GH
#547 (PR #556, open as of this revision) corrects `readme.txt:8` and `:51`,
removes the build-time header rewrite, and adds
`scripts/check-license-surfaces.sh` so the surfaces cannot drift apart again
unnoticed. This item stays open until that PR merges *and* a new agent release
publishes a zip that shows the corrected licence throughout — the currently
live wordpress.org zip stays `GPLv2 or later` until then, and copies already
distributed under it keep that grant regardless of when the fix ships. The
ruling itself is closed.

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
  `LICENSE-AGENT` and `NOTICE.md`; GH #547 (PR #556) brings `apps/agent/readme.txt`
  and the `agent-zip-wporg` build step into agreement with it, and
  `scripts/check-license-surfaces.sh` keeps them from drifting apart again.
- Adding a dependency to `apps/agent` acquires a mandatory licence check against
  MIT. This is a small cost paid on every dependency bump, and it is the only
  mechanism that catches the relicensing case before it ships.
- The reuse dispositions in §3 give a PR description a vocabulary: a PR says
  which of the four it is doing, which makes a review about the right question.
- §4 gives an engineer a three-way test — fact, method, or source — for the
  question that comes up more often than a new dependency: what may be taken
  from reading a competing plugin. It also states, rather than leaves implied,
  that *competitor*-sourced GPL-family material cannot be compliantly copied
  into this repository at all, because attribution and the anti-naming rule
  cannot both be satisfied — a narrower claim than blocking GPL-family reuse
  generally, which §3 already permits in `apps/api`.
- Rule 4 (§5) now states the integration-target exemption explicitly, matching
  what `ci.yml` already enforces and documents in its own preamble. A newly
  accepted rule that its own sibling ADR breaches in the same PR is the kind
  that gets informally suspended and never corrected; writing the exemption down
  is what stops that.
- F1 and F2 stay open until an owner rules or a fix merges. F1's ruling is
  closed and only its mechanical fix (GH #547) remains; F2 gates the start of
  MCP work. A later ADR supersedes this section when F2 closes; it is not
  edited away.
