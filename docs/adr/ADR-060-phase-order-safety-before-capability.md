# ADR-060 — Phase order: safety and truth before capability

**Status:** Accepted · **Date:** 2026-08-18
**Supersedes/relates:** ADR-037 (superseded — see below), ADR-057 (phases 4-7 inherited into the differentiation phase below).

This ADR fixes the order in which the next stretch of work is taken, and
records why that order is a decision rather than a scheduling preference.

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

The next phase of work is taken in this order. A later category does not
start ahead of an earlier one:

1. **Gate** — close any open trust or safety boundary.
2. **Recovery assurance** — prove that what is claimed to be recoverable
   actually recovers.
3. **Operator workflow** — make the operator-facing surface correct and
   legible for what already exists.
4. **Constrained automation** — let the system act on an operator's behalf,
   within tight, explicit limits.
5. **Differentiation** — capability that competes on features.

Capability work does not overtake safety work.

### Freeze clause

> No new externally-reachable surface ships while an auth-boundary item is open.

This is deliberately narrow. It does not freeze feature work in general —
internal work, and work that adds no new externally-reachable surface, is
unaffected. A broad freeze gets suspended informally and the lift is never
recorded, which leaves the project worse off than no freeze at all; a narrow
freeze is cheap enough to actually hold.

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

- A phase does not open until the phase before it is closed.
- The freeze clause is a standing check on any change that adds a new
  externally-reachable surface: confirm no auth-boundary item is open
  before it ships.
- Reordering this sequence again requires a superseding ADR — the same
  mechanism that reorders it here.
