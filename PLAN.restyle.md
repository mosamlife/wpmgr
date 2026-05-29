# WPMgr Impeccable Restyle — Plan

> Separate from the broader WPMgr build plan (see `PLAN.md`). This file tracks
> the Impeccable-skill-driven UI restyle of `apps/web`.
>
> Every subagent must run with `model: opus`. STOP at every gate; wait for "go."

## Phase 1 — Install Impeccable + PRODUCT.md
- [x] npx skills add pbakaus/impeccable  (installed to `.agents/skills/impeccable` with symlink at `.claude/skills/impeccable`)
- [x] pnpm --filter @wpmgr/web add -D impeccable@2.1.9
- [x] PRODUCT.md written at repo root (6,538 bytes) using canonical schema from `reference/init.md`
- [ ] User approval ← GATE

## Phase 2 — DESIGN.md
- [x] /impeccable document  (skipped — canonical content supersedes; no scaffold needed)
- [x] DESIGN.md written at repo root (9,247 bytes, 248 lines, YAML front matter + 8 H2 sections)
- [x] DESIGN.json sidecar: not required (Impeccable extensions sidecar lives at `.impeccable/design.json` and is for tonal ramps + motion + HTML/CSS snippets — not a JSON-ified front matter)
- [ ] User approval ← GATE

## Phase 3 — Tokens & fonts
- [x] OKLCH tokens in `apps/web/src/styles/globals.css`  (actual location; `index.css` doesn't exist in this project)
- [x] IBM Plex Sans (400/500/600/700) + IBM Plex Mono (400/500) via Google Fonts in `apps/web/index.html`
- [x] `@theme inline` mappings for every token  (colors + type scale + shadows + durations + easings)
- [x] Global `prefers-reduced-motion` rule
- [x] `components.json`: `baseColor: "neutral"`, `cssVariables: true` (was already correct)
- [x] Pre-existing `@keyframes wpmgr-shimmer` preserved (used by `progress.tsx`; translateX-only, compliant)
- [x] `impeccable detect src/styles/` + `impeccable detect index.html` → 0 findings (Phase 3 territory clean)
- [x] `vite build` green; bundle: 33.24 kB CSS / 7 kB gzip
- [ ] User approval ← GATE

### Findings deferred to Phase 4
- 6× `backdrop:bg-black/50` in dialog backdrops (`restore-dialog.tsx`, `add-site-dialog.tsx`, `user-picker-modal.tsx`, `update-wizard.tsx`, `api-keys.tsx`) — replace with `--scrim` token or `oklch(0% 0 0/0.5)`
- 7× pre-existing zod 4.4 + @hookform/resolvers 5.4 type incompatibility (`Type '4' is not assignable to type '0'`) — unrelated to design system; blocks `pnpm typecheck`. Track as separate fix.
- Puppeteer build-script warning on `pnpm exec impeccable` — call `./node_modules/.bin/impeccable` directly OR `pnpm approve-builds` it.

## Phase 4 — Surface-by-surface (4 sprints, 4 STOP gates)

Batched per user approval — parallel Opus subagents per sprint. Each subagent follows the loop:
read DESIGN.md+PRODUCT.md → static survey → implement against tokens → `pnpm typecheck && npx vite build` → `impeccable detect` → report.

### Sprint 1 — Primitives + Shell (2 parallel subagents)  [STOP]
- [x] **shell-layout-architect**: AppShell rewrite (129) + Sidebar new (491) + TopBar new (316)
      - Grid `grid-rows-[48px_1fr]` with column-template swap on toggle (NO width animation)
      - 5 groups: Sites · Operations · Insights · Security · Settings
      - Collapse persisted to `localStorage["wpmgr.sidebar.collapsed"]`; mobile drawer hand-rolled transform aside
      - `impeccable detect`: 0 findings (one `side-tab` regex hit rewrote with `before:` pseudo-rail)
- [x] **status-primitives-architect**: 5 primitives in `components/status/` (433 lines) + 3-line `--scrim` token + 6 dialog backdrop fixes
      - `StatusDot`, `StatusChip`, `UpdateChip`, `BackupChip`, `VulnSeverityChip` + `_status-preview.tsx`
      - `--scrim` light-mode tinted, dark-mode near-black
      - `impeccable detect` across 7 scopes: 0 findings
- [x] CSS bundle: 39.34 kB raw / 7.97 kB gzip (+0.7 KB raw / +0.15 KB gzip over Phase 3 baseline)
- [ ] User approval ← GATE

### Sprint 1 deviations flagged
- Settings group is DISABLED (no `/settings` index route); current `/settings/api-keys` + `/settings/alerts` temporarily unreachable from sidebar. **User decision needed before Sprint 4 builds the Settings hub.**
- Native `title` tooltips on collapsed rail + disabled items (no Radix Tooltip installed — out of Sprint 1 scope to add)
- Mobile drawer hand-rolled with `transform: translateX()` (no Sheet primitive installed)
- Breadcrumb has a local `TITLES` map (TanStack routes don't declare titles yet); dynamic params like `/sites/$siteId` show raw id — Sprint 2 swaps in resolved labels

### Sprint 2 — Core surface (1 solo subagent)  [STOP]
- [x] **sites-table-architect**: 4.5 Sites table (`sites-table.tsx`, 829 lines)
      - TanStack + `react-virtuoso@^4.18.7` (one new dep)
      - Density modes: Comfortable (56) / Compact (44, default) / Dense (36); `localStorage["wpmgr.sites.density"]`
      - 9 columns: checkbox · status+URL · client tags · WP · PHP · updates chip · backup chip · uptime · actions
      - Selection: `Set<string>` keyed by site_id, persists across filter/sort/pagination (never reconciles against current sites prop)
      - Header checkbox shows checked/indeterminate based on currently-visible rows
      - Hover `bg-muted` 80ms; selected `bg-muted` + `before:` pseudo 2px left primary rail; NO striping
      - Built-in `useSitesSelection()` + `useSitesDensity()` hooks
- [x] tsc/vite/impeccable detect: all clean
- [x] CSS bundle: 40.37 kB / 8.13 kB gz (+1 kB raw from Sprint 1)
- [ ] User approval ← GATE

### Sprint 2 deviations flagged
- **Sticky left columns NOT implemented** (per spec asked for sticky checkbox + URL). TableVirtuoso doesn't support sticky-left cells out of the box with `table-layout: fixed`. Viewport ≥1024px fits without horizontal scroll anyway. Deferred to Sprint 3 (toolbar/filters) if QA shows need.
- **Site API shape lacks updates_count / backup_status / EOL flags / uptime series** — safe defaults rendered, TODO(sprint-4) comments left for wiring CP endpoints
- **Removed standalone UptimeStatusBadge column** — `StatusChip` absorbs uptime inline under URL (calmer, denser)
- **Existing `AutoLoginButton` (dropdown shape) replaced by inline `Zap` icon + three-dot menu** per spec; `AutoLoginButton` preserved (still used by SiteDetail in Sprint 3 scope)

### Sprint 3 — Sites complements + Overlays (5 parallel subagents)  [STOP]
- [x] **toolbar-architect**: `sites-toolbar.tsx` new 560 lines; FLIP transform via `motion@^12.40.0` `<motion.div layout>` 240ms outQuart; verb-first action labels; `useSitesSelection`+`useSitesDensity` lifted to route
- [x] **site-detail-architect**: `routes/_authed/sites/$siteId.tsx` rewrote 211→675 lines; 4-tile health grid in ONE card; anchored sub-nav with IntersectionObserver; reuses AvailableUpdatesCard, BackupsSection, SiteTagsEditor, SiteComponentsTable, AutoLoginButton
- [x] **command-palette-architect**: `features/command/*` 4 new files; `cmdk@^1.1.1`; global ⌘K/Ctrl-K; 4 verb-first groups; recent-sites localStorage; `useSitesSelection` migrated to `useSyncExternalStore` singleton (palette+table share state); 4 cmdk keyframes in globals.css
- [x] **bulk-drawer-architect**: `bulk-action-drawer.tsx` + `use-bulk-action.ts` new; bottom slide-up 300ms outQuart; 5 states (idle/running/some-failed/all-done/dismissed-mid-run); `BulkActionProvider` wraps AppShell in `_authed.tsx`; TopBar Bell shows red dot with in-flight count
- [x] **modals-architect**: shared `components/ui/dialog.tsx` (250) + `destructive-confirm.tsx` (157); Restore is now 2-step (scope picker → hostname-typing confirm); all 5 modals (`restore-dialog`, `add-site-dialog`, `user-picker-modal`, `update-wizard`, `api-keys`) restyled; E2E specs synced
- [x] Vite build: ✓ 2.04s; CSS bundle 40.37 → ~41 kB; main bundle within reason
- [x] impeccable detect across all 5 subagents' scopes: 0 findings
- [x] Pushed to `origin/main` (commit `ea75f15`)
- [ ] User approval ← GATE

### Sprint 4 — Forms + Feedback (5 parallel subagents)  [STOP]
- [x] **forms-architect**: `components/forms/*` (208 lines) + 3 form refactors (backup-schedule-editor, alert-config-form, site-tags-editor); StickySaveBar + FieldError + FormSection; per-section save buttons removed
- [x] **toasts-architect**: `components/toast/*` (243 lines) + 13 call-site migrations; sonner@^2.0.7 already installed; `toast.destructive` REQUIRES action at TS level; lib/toast.ts shim deleted
- [x] **empty-states-architect**: `components/empty/*` (680 lines); NoSitesEmpty / FilterEmpty / NoBackupsEmpty / OnboardingWizard (3-step inline, NOT modal); SitesPageEmpty adapter via useOnboardingState (cross-tab safe)
- [x] **skeleton-architect**: `components/skeleton/*` (451 lines) + globals.css keyframe; SitesTableSkeleton column widths match 1:1; useCrossfade hook with double-RAF mount + 500ms ease-out-expo; row heights track density
- [x] **charts-architect**: `components/charts/*` (423 lines); recharts@^3.8.1; Sparkline + UptimeChart + ChartTooltip + ChartEmpty; CSS vars direct on stroke/fill; SLA reference line; renders NOTHING when <2 points
- [x] Pushed to `origin/main` (commit `32573a3`)
- [ ] User approval ← GATE — full Phase 4 complete

## Phase 5 — Motion pass
- [ ] `pnpm add motion`
- [ ] `apps/web/src/lib/motion-presets.ts` created (ease, dur, fadeUp, fade, scaleIn, drawerUp, stagger, skeletonToContent, statusPulse)
- [ ] Applied ONLY to: toast in/out, dialog enter, sites row initial mount, skeleton→content crossfade, status change one-shot pulse, command palette open, bulk action drawer slide-up, toolbar→action-bar transform
- [ ] `/impeccable animate` + `/impeccable audit motion` clean
- [ ] User approval

## Phase 6 — Harden, clarify, optimize, polish, CI
- [ ] `/impeccable harden apps/web/src/` (long names, DE +30%, 500s, offline, 1000+ rows, empty results)
- [ ] `/impeccable clarify apps/web/src/` (verb-first labels, what/why/how errors, banned-phrase scrub)
- [ ] `/impeccable optimize apps/web/src/` (LCP < 2.5s, INP < 200ms, CLS < 0.1 on sites table)
- [ ] `/impeccable polish apps/web/src/` (alignment, state coverage, copy drift, token alignment)
- [ ] `/impeccable audit apps/web/src/` ≥ 3/4 every dimension; P0 + P1 = zero
- [ ] CI gate: `lint:design` script + GitHub Actions job uploading `impeccable-report.json`
- [ ] `PLAN.restyle.md` boxes all checked

## Constraints (locked)

**Stack:** React 19 + TS strict + Vite + Tailwind v4 (OKLCH-native) + shadcn (new-york) + Radix.
**Add deps allowed:** motion, cmdk, sonner, react-virtuoso, @tanstack/react-table, recharts, lucide-react. Dev: impeccable CLI.
**Register:** product (not brand). Never bolder/delight/overdrive on dashboard surfaces.

**Banned forever:**
- Fonts: Inter, Roboto, Geist, Plus Jakarta Sans, Fraunces, Recoleta, Space Grotesk, Instrument Sans, Mona Sans, Newsreader, Playfair, Cormorant, Tiempos
- Purple-to-blue / cyan-on-dark gradients; cyan-glow shadows
- Gradient text anywhere
- Side-tab accent borders on rounded cards
- Nested cards / wrapping every section in a card
- Pill buttons (6px radius rectangles only)
- Bounce/elastic easing
- Animating width, height, top, left, margin, padding
- Pure `#000` or `#fff`
- Em dashes in any UI string
- "Welcome to", "Introducing", "powerful", "seamless", "blazing fast", "your one-stop", "Manage your"
- "OK" / "Submit" / "Cancel" as button labels — always verb-first
