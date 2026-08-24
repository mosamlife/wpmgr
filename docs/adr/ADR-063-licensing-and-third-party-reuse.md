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

**The plugin as distributed is offered under GPLv2-or-later**, which its
wordpress.org listing page declares:

```sh
grep -nE '^License' apps/agent/readme.txt
#   8:License: GPLv2 or later
#   9:License URI: https://www.gnu.org/licenses/gpl-2.0.html
```

while the plugin header declares MIT:

```sh
grep -nE '^ \* License' apps/agent/wpmgr-agent.php
#   10: * License:           MIT
#   11: * License URI:       https://opensource.org/licenses/MIT
```

That is two different strings in two files a user reads side by side. It is not a
violation — see [Open for an owner ruling](#open-for-an-owner-ruling), F1 — but it
is recorded here rather than left for the next reviewer to rediscover.

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
what carries over. State the design in your own words, build it against this
codebase's own primitives, and route anything touching auth, tokens, tenancy,
capability checks or the command protocol to `security-reviewer` regardless of how
small it looks.

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

## 4. Standing rules for engineers

1. **Check the licence against the destination before adding any dependency.**
   `apps/agent` is MIT and a copyleft dependency relicenses it. `apps/api` is
   AGPL-3.0 and has no such conflict.
2. **Add every new dependency to `NOTICE.md` and to `apps/agent/readme.txt`'s
   third-party section in the same PR**, following the existing entries.
3. **Take libraries from their own upstream**, never out of a local reference
   copy.
4. **Never name a competitor product** in code, a comment, a commit message, a
   committed document, a tracked ignore file or a PR title. `apps/marketing/**`
   is the only carve-out and nothing in the rules above goes there.
   `apps/agent/readme.txt` is the public wordpress.org listing page and stays
   under the strict rule.
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

Two items are recorded here as **open**. Neither is resolved by this ADR, and
neither is a session's to resolve.

**F1 — the agent declares two different licences.** The plugin header says `MIT`
(`apps/agent/wpmgr-agent.php:10`) and the wordpress.org listing says
`GPLv2 or later` (`apps/agent/readme.txt:8`). This is **not a violation**: MIT is
GPL-compatible, so an MIT-licensed plugin may legitimately be distributed under
GPLv2-or-later, and the readme is an accurate statement of what the distributed
*package* is offered under. It is still two different strings a user reads side by
side, and it costs one sentence to fix — the plugin's own code is MIT, the package
as distributed is GPLv2-or-later. **Recommendation: fix it now rather than during
a release**, routed to `wp-agent-engineer` since the path is `apps/agent/**`.
Cosmetic, cheap, and it stops recurring in every future review.

**F2 — keeping the MCP surface out of the plugin for licence reasons.** §2 sets
out why this is also the architecture already chosen, but the *decision* has three
real options and the owner picks one:

- **(b) Keep the entire MCP surface in `apps/api`** and let the agent keep
  speaking the existing signed-command protocol. Cheapest licensing outcome,
  changes nothing about `apps/agent`, and matches ADR-061 Decision 1.
  **This is the recommendation.**
- **(a) Accept the relicence** and update `LICENSE-AGENT` and the plugin header
  to GPL-2.0-or-later. This also resolves F1. It is a strategic licence change to
  a plugin listed in a public directory.
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
- Adding a dependency to `apps/agent` acquires a mandatory licence check against
  MIT. This is a small cost paid on every dependency bump, and it is the only
  mechanism that catches the relicensing case before it ships.
- The reuse dispositions give a PR description a vocabulary: a PR says which of
  the four it is doing, which makes a review about the right question.
- ADR-061's Decision 1 gains a second, independent supporting argument. If the
  credential argument were ever revisited, the licensing one would still stand.
- F1 and F2 stay open until an owner rules, and F2 gates the start of MCP work.
  A later ADR supersedes this section when they close; they are not edited away.
