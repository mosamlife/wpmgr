# ADR-060 — Phase order: safety and truth before capability

**Status:** Accepted (amended 2026-08-27) · **Date:** 2026-08-18
**Supersedes/relates:** ADR-037 (superseded — see below), ADR-057 (phases 4-7 inherited into the differentiation phase below).

This ADR fixes the order in which the next stretch of work is taken, and
records why that order is a decision rather than a scheduling preference.

**Amended 2026-08-27 — read [Amendments](#amendments-2026-08-27) before
deciding whether a change trips the freeze clause.** The status stays
Accepted and no decision text below is rewritten. One amendment, A1, defines
"externally-reachable surface" for the [Freeze clause](#freeze-clause)
below — a term this document used but never defined, which two separate
ADRs had each been left to interpret for themselves, reaching opposite
conclusions by different routes.

---

## Context

Work here sorts into a small number of kinds: closing an open trust or
safety boundary, proving that recovery does what it claims, making an
operator's existing workflow correct and legible, letting the system act on
an operator's behalf within tight limits, and building capability that
competes on features. These kinds compete for the same engineering time, and
left unordered, the visibly sellable kind tends to win by default — every
quarter something in the differentiation column looks more shippable than an
invariant that has no user-facing surface at all.

## Decision

The next phase of work is taken in this order of precedence: when two
phases compete for the same engineering time, the earlier one gets it and
the later one yields.

1. **Gate** — trust and safety boundary work.
2. **Recovery assurance** — prove that what is claimed to be recoverable
   actually recovers.
3. **Operator workflow** — make the operator-facing surface correct and
   legible for what already exists.
4. **Constrained automation** — let the system act on an operator's behalf,
   within tight, explicit limits.
5. **Differentiation** — capability that competes on features.

Capability work does not outbid safety work for the same engineering time.
This precedence order is the whole of the ordering rule; it is not a gate,
and nothing below is a second one. The freeze clause is this ADR's one
absolute prohibition:

### Freeze clause

*Amended 2026-08-27 — "externally-reachable surface" is defined in
[Amendment A1](#amendments-2026-08-27) below. The clause's own text is
unchanged.*

> No new externally-reachable surface ships while an auth-boundary item is open.

This is deliberately narrow. It does not freeze feature work in general —
internal work, and work that adds no new externally-reachable surface, is
unaffected. A broad freeze gets suspended informally and the lift is never
recorded, which leaves the project worse off than no freeze at all; a narrow
freeze is cheap enough to actually hold. Precedence above decides what gets
engineering time; this clause is the only thing that outright does not ship.

## Why an ADR, and not a ticket

Ordering has no artifact that shows it was decided, the way a feature has a
shipped diff. A ticket can be reprioritized by anyone who opens the board,
and a ticket cannot outrank another ticket — nothing stops a later planning
conversation from quietly moving phase 5 ahead of phase 1 because phase 5
has a customer waiting on it, while phase 1 has none. This is the ordering
most likely to be silently reversed for exactly that reason: deferring
safety work by one more sprint costs nothing in the moment, and nothing
about a ticket board stops that from repeating indefinitely. Recording the
order as an accepted ADR gives it standing a ticket does not have: reversing
it takes a new ADR, not a re-drag on a board.

## Disposition of prior roadmap ADRs

**ADR-037** (site-management roadmap, proposed 2026-05-29) is superseded by
this ADR. Its sprint-by-sprint sequencing predates the ordering above and is
no longer the plan. Its content is unchanged and stands as the record of
what was proposed and why.

**ADR-057** (Security Suite Foundation) is not superseded. Its architecture
— control-plane-owned policy, one table and signed command per phase, the
per-site RLS template — is unaffected by this ADR and remains Accepted as
written. Its phases 4 through 7, the capability tiers built on that
architecture (WAF/virtual-patching, cross-fleet IP reputation, and the
phases beyond), are reordered by this ADR: they are inherited into the
differentiation phase above rather than proceeding on ADR-057's original
phase sequence. What they will eventually be does not change; only when
they become eligible to start does.

## Consequences

- When phases compete for the same engineering time, the earlier phase in
  the order above gets it; this is a precedence rule, not a hard gate on
  starting later work.
- The freeze clause is the one hard prohibition, and it is a standing check
  on any change that adds a new externally-reachable surface: confirm no
  auth-boundary item is open before it ships. What counts as a new surface
  is fixed by Amendment A1 below, not decided per feature.
- Reordering this sequence again requires a superseding ADR — the same
  mechanism that reorders it here.

---

## Amendments (2026-08-27)

Two ADRs each had to decide, for their own purposes, whether the work they
proposed added a "new externally-reachable surface" under the freeze clause
above — and each decided it independently, because this document never
defined the term.

ADR-064 (per-site and organisation context, Proposed, PR #548) needs about a
dozen new routes on the existing, already-authenticated dashboard API. It
argued these are not a new surface, said plainly that this was its own
reading of an undefined term and not binding on any other document, and
named the gap as an open question for whoever holds this ADR next to close.
Separately, and earlier, a draft amendment to ADR-061 — its A9, in unmerged
PR #519 and not on `main`, so it does not carry as an accepted decision —
reached the opposite-shaped conclusion, that the assistant route *is* a new
surface, and did so inside an amendment section belonging to a different
ADR rather than this one. The independent derivation below (see the edges,
first bullet) does not depend on that draft landing.

Both documents reasoned carefully, and, on their own facts, both reached the
right answer (see the edges below). That does not make the pattern safe.
If every ADR is free to interpret "surface" for itself, the freeze clause
stops being the one absolute prohibition the Decision section calls it in
the sentence introducing it — a rule that gets interpreted around, document
by document, is indistinguishable in practice from a rule that gets
suspended informally, which the Freeze clause section already names as the
failure a narrow freeze exists to avoid.

**This is an amendment, not a rewrite. The Decision and Freeze clause text
above is unchanged**, in wording and in force. What follows defines a term
that text already uses; it does not loosen, tighten or relocate the rule the
term sits inside.

### A1 — Defining "externally-reachable surface" for the freeze clause

**A surface is fixed by three things: the transport or listener it answers
on, the authentication a caller must satisfy to reach it, and the class of
caller that authentication admits. A change is a *new* surface when it
changes at least one of those three in a way that expands reachability — a
new transport or listener; an
authentication requirement that admits a class of caller that could not
reach that perimeter before; or a route that becomes reachable to a class of
caller that could not reach it before. A change that adds a route, resource
or method inside an unchanged transport, an unchanged authentication
requirement, and an unchanged, already-admitted class of caller is not a new
surface — it is new capability on an existing one, and the freeze clause was
never the rule that governs that.**

Put as the test to run against a diff: before this change, did anything
answer at this transport, to this class of caller, under this
authentication? If no — the transport is new, or the authentication is
weaker than it was, or the caller class is new — this is a new surface and
the freeze clause governs it. If yes — the same transport and the same
authentication already admitted this same class of caller, and the change
only adds a resource inside that perimeter — it is not.

**This is reading (a), a genuinely new reachable entry point, not reading
(b), any new route on an already-authenticated perimeter.** The choice is
forced by this document's own text, not asserted fresh here. The [Freeze
clause](#freeze-clause) section already says, in the paragraph immediately
below the clause itself, that it "does not freeze feature work in general —
internal work, and work that adds no new externally-reachable surface, is
unaffected." Reading (b) makes that sentence false: essentially all feature
work adds a route somewhere, so essentially all feature work would freeze
the moment any auth-boundary item is open — the broad freeze that same
sentence calls itself "deliberately narrow" to avoid being, and the same
paragraph goes on to say why: "a broad freeze gets suspended informally and
the lift is never recorded ... a narrow freeze is cheap enough to actually
hold." A definition that makes this document's own scope statement false is
the wrong definition, whatever else might recommend it.

**The edges, worked through rather than asserted:**

- **A new route on an existing, already-authenticated perimeter, answering
  to a class of caller that could not reach that perimeter at all before.**
  This is a new surface without qualification. ADR-061's assistant route is
  this case: it sits on the existing API host, but nothing answering to the
  agent-kind key ADR-061 Decision 2 defines for the assistant proposer was
  reachable there before it shipped — a new caller class, sufficient on its
  own under the test above. (An unmerged draft amendment to ADR-061, its A6,
  would add a new-transport ground as well; that draft is not on `main`,
  and this amendment does not rely on it.) Nothing answering to a wholly
  new class of caller is "capability added to an existing surface" under
  either reading. Contrast ADR-064: a dozen routes added for a caller class
  already admitted on that perimeter (an existing dashboard session), under
  an authentication requirement the ADR adds no new mechanism for. Same
  transport, same authentication, same already-admitted caller class — only
  the count of routes that class can reach changes. That fails the test the
  other way, and is not a new surface.
- **A route that changes an existing endpoint's authentication
  requirement.** Direction decides it. Weakening — dropping a check,
  accepting a second and weaker credential form, admitting a caller who
  could not authenticate that way before — is a new surface: it is exactly
  "authentication that admits a class of caller that could not reach that
  perimeter before," above. Tightening is never a new surface under this
  clause; it is Gate-phase work, the first item in this document's own
  precedence order, and the freeze clause was never a reason to withhold it.
- **A new transport on an existing host** — a new protocol, a new listener,
  a new port — is a new surface if it is externally reachable at all,
  regardless of whether the authentication behind it matches something else
  on that host. Nothing answered on that transport before, which is the
  test above by itself. A transport that is not externally reachable to
  begin with — loopback-only, a private network — is outside the clause for
  the reason anything non-externally-reachable already is; "externally-
  reachable" is doing that work, and A1 does not reopen it.
- **Reachable only after authentication, versus reachable before it.** A
  route reachable without authentication, where every prior route on that
  host required it, is a new surface on the same test: unauthenticated
  reachability is a class of caller nothing before could satisfy merely by
  existing on the network. A route reachable only to an already-admitted,
  already-authenticated class, added behind an unchanged authentication
  requirement, is not — the ADR-064 shape again, from the other side.

**One edge this definition does not mechanically close, named rather than
forced.** Whether a change "admits a class of caller that could not reach a
perimeter before" is decidable by inspection when the class is a credential
kind (an assistant-kind key versus a dashboard session) or a network
position (unauthenticated versus authenticated). It is not always decidable
by inspection when a change narrows what an *already-admitted* credential
must additionally prove — a scope broadened, a check dropped that the same
credential kind used to have to pass, a weaker proof of the same kind of
secret accepted — because "class of caller" can be sliced at whatever
granularity makes the answer come out either way, and this amendment has no
test finer than the three-part one above. **That case is not settled here.
The security-reviewer decides it**, on the diff in front of them, case by
case — the same reviewer this project's routing rules already require for
any change touching auth, tenancy or the agent protocol, regardless of
whether the freeze clause happens to be live at the time. That is not a gap
papered over with a name; it is the reviewer this kind of change already
goes to, for reasons that have nothing to do with this amendment.

**What this amendment does not change.** The freeze clause's force is
unchanged: it remains this document's one absolute prohibition, not a
second gate standing next to the precedence order. The precedence order —
Gate, Recovery assurance, Operator workflow, Constrained automation,
Differentiation — is unchanged, in content and in strength. Reordering it
still takes a superseding ADR, not a re-reading of a word. This amendment
defines a term the clause already used; nothing above loosens, tightens, or
relocates the rule the term sits inside.

**Consequence.** Once this lands, ADR-064's freeze-clause discussion should
cite Amendment A1 rather than carry its own interpretation of "surface" —
its own text already names this gap as the open question and this
amendment as the resolution, rather than treating its own reading as
binding on any later ADR. ADR-061 carries no amendment section on `main`
today; its unmerged draft A9 (PR #519) reaches the same conclusion this
amendment does for the assistant route, by the same reasoning, but that
reference does not carry until #519 merges. This amendment does not depend
on it: the caller-class ground alone, from ADR-061 Decision 2 as Accepted,
already puts the assistant route on the new-surface side of the test above.
A future document facing this question should still point here rather than
re-argue the term in its own amendment section.
