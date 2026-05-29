# Backup Progress UX Patterns — Research & Recommendation

**Context:** The V0 backup runner (ADR-032 / M5.6) emits a closed-set phase
enum (`queued` → `started` → `dumping_db` → `archiving_files` →
`encrypting_uploading` → `submitting_manifest` → `completed` | `failed`)
plus per-phase telemetry (files/parts/chunks/artifacts counters,
`current_file`, `current_artifact`, optional `bytes_done`). Current UI is a
text label + counter + 4 px bar — the product owner has explicitly rejected
this as "text-shaped" and wants a visual progress UI that communicates the
multi-stage nature.

This doc surveys patterns A–F from the brief, then synthesises a concrete
shadcn/Tailwind implementation for the two existing surfaces:
`SnapshotProgressCard` (full-width detail page) and `InlineSnapshotProgress`
(table cell, ~280 px).

---

## A. Stepper / pipeline / journey

The dominant pattern across CI/CD products is a **vertical list of named
steps** (GitHub Actions, GitLab CI, CircleCI) rather than a horizontal
checkout-style stepper. Horizontal steppers (Stripe, Linear inbox, Vercel
header) work for ≤5 short, equal-weight phases; they collapse when one
phase dominates wall-clock time. Our phases are deeply unequal (manifest
submit ~500 ms; upload 5–15 min), so weighting matters.

Eleken's stepper survey and Lollypop's stepper deep-dive converge on the
same state vocabulary: **completed = filled circle + checkmark; active =
solid colour ring + pulsing dot or spinner; pending = hollow circle, muted
text** ([Eleken][1], [Lollypop][2]). HashiCorp Helios and the shadcn
community steppers (reui.io, dice-ui) all encode this same triad
([reui][3], [Dice UI][4]).

Important gap noted in the Eleken survey: **none of the surveyed steppers
target long-running backend jobs.** They model user-driven wizards, where
"active" means "user is filling in this step." For a backend job, "active"
must additionally communicate "work is happening, here's the live
sub-progress." GitHub Actions solves this with a per-step spinner that
flips to a green check on completion; the step row itself is the
container for the streaming log. We will adopt the same shape.

## B. Determinate progress within active phase + total elapsed/ETA

Apple's iCloud Backup screen and Backblaze's client both surface **one
primary determinate bar** (the active phase's %), with elapsed time and a
softly-worded ETA ("About 4 minutes remaining"). They explicitly avoid
showing a global % across all phases because phase durations are
heterogeneous and the resulting global % is a lie. Paragon's support
article ([Paragon][5]) confirms the industry-wide UX trade-off: ETA is
"correct only when ~90% of operation is done" — so it should be **labelled
as an estimate** and ideally hidden until enough samples exist.

The right formula (per the yt-dlp issue and exponential-smoothing
references ([yt-dlp][6], [Wikipedia][7])): maintain an EMA of
bytes-per-second over the last ~10 events with α≈0.3, then
`eta = (total − done) / smoothed_throughput`. Suppress display until the
EMA has at least 3 samples.

## C. Sub-progress nesting

Docker Desktop, Steam, and VSCode all use the same pattern: **a primary
progress bar for the current item, plus a header line "X of Y items"**.
Docker's multi-layer pull stacks one bar per concurrent layer (we don't
need that — our upload loop is sequential per artifact). VSCode extension
install is closer to our shape: "Installing 3 of 7 extensions:
typescript-syntax" with one bar.

For us this maps to:

- **Outer counter:** artifact N of M  *(only meaningful in
  `encrypting_uploading`)*
- **Inner bar:** chunks done / chunks total of the current artifact

Inside `archiving_files`, the equivalent is `parts_done` (outer)
and `files_done/files_total` (inner). Both phases get the same UI shape.

## D. Smooth animation tricks

Discrete SSE events (1 update/sec under load, less when chunks are large)
will produce a jerky bar if width is set step-wise. The CSS-Tricks /
Medium write-ups ([Medium][8], [Bulma][9]) agree on the simplest fix:
**a CSS transition on `width` with a duration slightly longer than the
typical inter-event gap, easing `ease-out`.** For chunk events arriving
~every 1–2 s, `transition: width 800ms ease-out` is sweet-spot; faster
loses smoothness, slower lags the actual data.

The `requestAnimationFrame` interpolation route is overkill for our event
rate. We'll reserve rAF only for the indeterminate shimmer keyframes
(handled by Tailwind's `animate-pulse` or a custom `@keyframes shimmer`).

## E. Indeterminate states

Material Design 3's linear indicator ([m3.material.io][10],
[material-web][11]) gives clear guidance: use **determinate** when % is
known, **indeterminate** otherwise — and the indeterminate variant
animates as a continuously growing/shrinking band. Material 3 also added
a *stop indicator* (a small dot at 100%) and a track-gap, which together
make the bar feel "live" even when paused.

Mapping to our phases:

| Phase | Mode | Why |
|---|---|---|
| `queued` | indeterminate | no work started |
| `started` | indeterminate | runner init, sub-second |
| `dumping_db` | indeterminate | mysqldump streams — no `% done` available |
| `archiving_files` | **determinate** (files_done/total) | we have counters |
| `encrypting_uploading` | **determinate** (chunks_done/total of current artifact) | best signal |
| `submitting_manifest` | indeterminate | sub-second POST |
| `completed` | full bar, success colour |  |
| `failed` | bar frozen at last %, destructive colour |  |

## F. Failure / pause / cancel UX

GitHub Actions and GitLab CI both **freeze the bar at the point of
failure**, recolour the failed step's circle red with an X icon, and leave
prior completed steps green. They do *not* zero the bar — preserving the
"how far did we get" signal is part of the diagnostic. Stripe Connect
onboarding does the same on failed verification.

The active-step spinner is replaced by the X immediately; any text under
the bar switches to the failure message from `phase_detail.message`.
Cancellation (not in our V0 — we don't support cancel yet) would use the
same frozen-bar pattern but with a neutral grey + "Cancelled" badge.

---

## Synthesis — WPMgr recommendation

### Chosen pattern

**A + C + B**, applied differently per surface:

- The **detail-page card** uses the full vertical stepper (A) with the
  active step expanded to show the determinate sub-bar (C) and elapsed +
  optional ETA (B).
- The **inline table cell** collapses to a horizontal "dot strip"
  (compact A), the active step's label, and the determinate sub-bar (C).
  No ETA in the cell — there's no room and the user can click through.

### Layout — snapshot detail page (full-width card)

```
┌─ Live progress ─────────────────────────────────────── ● Live ─┐
│                                                                │
│  ✓  Queued                                          2s         │
│  │                                                             │
│  ✓  Started                                         <1s        │
│  │                                                             │
│  ✓  Dumping database                                14s        │
│  │                                                             │
│  ⊙  Archiving files                                 1m 42s     │
│  │   1,847 / 3,201 files  ·  part 002              57%         │
│  │   ▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓▓░░░░░░░░░░░░░░░░░░░░░░░░░░░             │
│  │   wp-content/uploads/2024/03/header-image.jpg               │
│  │                                                             │
│  ○  Encrypting & uploading                          —          │
│  │                                                             │
│  ○  Submitting manifest                             —          │
│                                                                │
│  Elapsed 2m 14s  ·  ~3 min remaining                           │
└────────────────────────────────────────────────────────────────┘
```

Glyphs: `✓` = completed (green-600 filled circle), `⊙` = active (primary
ring + pulsing inner dot), `○` = pending (muted hollow circle).
Connector `│` is a 1 px muted line between rows; the segment above an
active/completed step is filled in primary, below is muted.

### Layout — site detail table (inline, max 280 px)

```
● ● ● ⊙ ○ ○   Archiving · 1,847/3,201   57%
              ▓▓▓▓▓▓▓▓▓▓▓▓░░░░░░░░░░░░░░
              wp-content/uploads/.../header.jpg
```

Six dots = the six non-terminal phases. Each is 6 px; completed = filled
primary, active = primary with `animate-ping` ring, pending = muted.
Label + counter + % on the right of the dot strip. Sub-bar on row 2 (max
160 px, 4 px tall — same as today). Current-file truncated on row 3
(monospace 10 px, optional). On failure the active dot turns destructive
and a small `Failed` badge replaces the counter.

### ETA calculation

Compute only for `encrypting_uploading` (the long, measurable phase).
Track `(timestamp, bytes_done)` from incoming SSE events in a ref-backed
circular buffer of last 10 events. Compute EMA throughput with α=0.3,
require ≥3 samples before showing ETA. Format with `Intl.RelativeTimeFormat`,
prefix "~", and recompute on each event. On `archiving_files` we lack a
reliable bytes signal across `.partNNN.zip` rotations — fall back to
files-per-second × (files_total − files_done) but label it "approx".

Suppress ETA in the inline cell entirely; show only in the card.

### Color palette

Use existing CSS variables (no new tokens):

- `var(--color-primary)` — active step ring, sub-bar fill, completed connector
- `var(--color-muted)` — pending dot, sub-bar track, connector below active
- `var(--color-muted-foreground)` — pending label, secondary metadata
- `var(--color-foreground)` — active/completed label
- `var(--color-destructive)` — failure dot, failed bar fill, error text
- Green tick (`bg-green-600 / text-white`) for completed dot — slight
  divergence from primary so the eye can scan "what's done" instantly.
  This matches GitHub Actions and matches the dot already used in the
  existing live/polling indicator.

Active step micro-animation: a 2 s `@keyframes` that pulses the ring's
box-shadow from `0 0 0 0 rgba(primary, 0.5)` to `0 0 0 6px transparent`.
Sub-bar uses `transition: width 800ms ease-out`. Indeterminate phases
use a 1.6 s `translateX(-100% → 200%)` band over the track.

### Reduced motion

Wrap all three animations (active-dot pulse, indeterminate band, sub-bar
width transition) in `@media (prefers-reduced-motion: no-preference)`.
Per the MDN / CSS-Tricks guidance ([MDN][12], [CSS-Tricks][13]),
progress indicators may keep a *softened* cue when motion is reduced: we
drop the pulse and indeterminate sweep, but keep a static `ring-2
ring-primary` on the active dot so the state is still visible. Width
transitions become instant (`transition: none`) — the bar still updates,
just step-wise.

### shadcn / Tailwind v4 implementation plan

Already present in `apps/web/src/components/ui/`: `badge`, `button`,
`card`, `checkbox`, `dropdown-menu`, `input`, `label`, `progress`, `table`.

We do **not** need to install a new shadcn block. The stepper is small
enough to build inline as two feature components:

1. **`apps/web/src/features/backups/backup-stepper.tsx`** — new file,
   renders the six-step vertical pipeline. Pure presentational; takes
   `{ currentPhase, phaseStartedAt, elapsedByPhase }`. Internally maps
   the closed phase enum to `{ index, label, status }` rows.
2. **`apps/web/src/components/ui/progress.tsx`** — extend the existing
   `Progress` to accept `indeterminate?: boolean` and a `tone?:
   "primary" | "destructive" | "success"`. Indeterminate mode renders an
   inner span with the shimmer keyframes instead of a width-driven fill.
   Keep the existing API backward-compatible.
3. **`apps/web/src/features/backups/snapshot-progress-card.tsx`** —
   rewrite the body to use `<BackupStepper>` + the determinate sub-bar
   block (already half-built today for the upload phase) + an ETA line
   driven by a new `useBackupEta(snapshotId)` hook that lives in the
   same feature folder.
4. **`apps/web/src/features/backups/inline-snapshot-progress.tsx`** —
   replace today's single-row layout with the dot strip + sub-bar shown
   above. Reuse the same `BackupStepper` in a `compact` variant
   (`orientation="horizontal" density="dots"`).
5. **`apps/web/src/features/backups/use-backup-eta.ts`** — new ~40-line
   hook holding the EMA buffer in a `useRef`, fed by the same
   TanStack-cached `snapshot.progress` updates that already drive the
   existing components. No new SSE subscription.

The `data-build="sse-card-v1"` / `data-build="sse-inline-v1"` markers
must bump to `…-v2` so a stale bundle is visible in DOM-diff tests.

---

## Sources

[1]: https://www.eleken.co/blog-posts/stepper-ui-examples "Eleken — 32 Stepper UI Examples"
[2]: https://lollypop.design/blog/2026/february/beyond-the-progress-bar-the-art-of-stepper-ui-design/ "Lollypop — Beyond the Progress Bar"
[3]: https://reui.io/components/stepper "ReUI — Shadcn Stepper"
[4]: https://www.diceui.com/docs/components/radix/stepper "Dice UI — Radix Stepper"
[5]: https://paragon-software.zendesk.com/hc/en-us/articles/27329544238865 "Paragon — Backup ETA keeps increasing"
[6]: https://github.com/yt-dlp/yt-dlp/issues/4267 "yt-dlp — ETA moving average"
[7]: https://en.wikipedia.org/wiki/Exponential_smoothing "Wikipedia — Exponential smoothing"
[8]: https://medium.com/@dobulbekovach/how-i-simplified-my-progress-bar-with-css-transitions-71fd18ccc234 "Medium — CSS transitions for progress bars"
[9]: https://github.com/jgthms/bulma/issues/491 "Bulma — Smooth progress animations"
[10]: https://m3.material.io/components/progress-indicators/guidelines "Material Design 3 — Progress indicators"
[11]: https://material-web.dev/components/progress/ "Material Web — Linear progress"
[12]: https://developer.mozilla.org/en-US/docs/Web/CSS/Reference/At-rules/@media/prefers-reduced-motion "MDN — prefers-reduced-motion"
[13]: https://css-tricks.com/almanac/rules/m/media/prefers-reduced-motion/ "CSS-Tricks — prefers-reduced-motion"
