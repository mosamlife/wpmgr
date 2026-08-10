---

# Standing Process — Every Shipped WPMgr Feature Lands on the Landing Page AND in a Release Changelog

**Owner:** docs-writer lead
**Status:** STANDING PROCESS (active from now on)
**Applies to:** every feature, capability, or user-visible behavior change shipped in the WPMgr monorepo

This document defines the canonical changelog home, the end-of-feature SOP the docs-writer agent runs, the templates to copy, and how this is wired as a definition-of-done gate. It is binding: a feature is **not done** until both surfaces below reflect it.

---

## 0. The non-negotiable rule

> **Every shipped feature MUST be reflected in TWO places before the feature is considered done:**
> 1. The **landing page** (`apps/landing`) — a user-facing feature card and, where applicable, the changelog page.
> 2. A **release changelog entry** in the root `CHANGELOG.md`.

There is exactly **one** hard constraint that overrides convenience everywhere below: **NEVER name a competitor** in the landing site, the README, the CHANGELOG, or any code/copy. The single sanctioned exception is the **GitHub repository *description* field** (the short "About" blurb on github.com), which is out of the repo tree and outside the copy gate. Nowhere else.

---

## 1. The canonical changelog home

WPMgr has **no changelog today** (no root `CHANGELOG.md`, no `/changelog` route on the landing or web app, version strings are hand-typed in two places that drift). We fix that with a two-part home: a canonical source file plus a derived landing page.

### 1.1 Canonical source — root `CHANGELOG.md` (create this)

- **File:** `/Users/mosamgor/Desktop/Terminal/wpmgr/CHANGELOG.md` (repo root, alongside `README.md`, `PLAN.md`, `DECISIONS.md`, `SECURITY.md`, etc.).
- **Format:** [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) + [Semantic Versioning](https://semver.org/).
- **Why root:** it is the conventional open-source artifact (fits the AGPL/open-core stance), it is the **single edit point** for the docs-writer, and it sits **outside** `apps/landing/scripts/check-copy.mjs`'s walk (that script only scans `apps/landing/src/`). Because it is outside the copy gate, **the no-competitor / no-em-dash rules are enforced manually here** — see §2 step C and §4.
- **Versioning anchor:** version headings are keyed to the real app version, which is `spec.info.version` in `packages/openapi/openapi.yaml`. Do not invent a parallel version scheme. When you cut a release entry, the heading version must match (or lead) `spec.info.version`.

Seed it on first creation with an `[Unreleased]` section and the first released version. Structure (full template in §3.1):

```
# Changelog

All notable changes to WPMgr are documented here.
Format: Keep a Changelog (keepachangelog.com). Versioning: SemVer (semver.org).
House rules: no em dashes, no en dashes, no competitor names. Use "to" for ranges.

## [Unreleased]

## [0.12.0] - 2026-06-02
### Added
- ...
```

### 1.2 Derived view — landing `/changelog` page

The landing changelog is a **derived view** of the source, wired the same way `/docs` is wired (a build-time generator feeding a section). Two build-out options, in order of preference:

**Preferred (automated, do this when you create the page):**

1. Add a generator `apps/landing/scripts/sync-changelog.mjs` modeled on the existing `apps/landing/scripts/sync-openapi.mjs`. It parses repo-root `CHANGELOG.md` into JSON under `apps/landing/public/changelog/` (e.g. `public/changelog/index.json`). `dist/` and generated `public/` artifacts are gitignored, matching the openapi pattern.
2. Prepend it to the landing `build` script in `apps/landing/package.json` so the page can never go stale:
   ```
   "build": "node scripts/sync-changelog.mjs && node scripts/sync-openapi.mjs && tsc --noEmit && vite build"
   ```
3. Render it. Either a dedicated HTML entry like `/docs`, or a `Changelog` section component added to `apps/landing/src/sections.tsx` and composed into `apps/landing/src/App.tsx`. Add the route/anchor to `NAV.links` and/or `FOOTER.links` in `apps/landing/src/data/content.ts`.

   **Copy-gate caveat:** if changelog copy is rendered from `src/` (a typed export in `content.ts`), it is subject to `check-copy.mjs`. If it is read at build time from root `CHANGELOG.md` into `public/`, it bypasses that gate — which is exactly why the no-competitor / no-em-dash rules must be held in `CHANGELOG.md` itself by hand (or extend `check-copy.mjs` to also scan the root file). Prefer the `public/`-JSON path so `CHANGELOG.md` stays the one source.

**Minimal (manual, acceptable until the generator exists):**

- Add a typed `CHANGELOG` export to `apps/landing/src/data/content.ts` (same shape the generator would emit), hand-author a `Changelog` section reading from it, and add the `NAV`/`FOOTER` link. This keeps everything inside the existing copy-gate + impeccable + manual-GCS-deploy flow. Note: this **duplicates** the entries you already wrote in root `CHANGELOG.md`, so keep them in lockstep until the generator lands.

### 1.3 Kill the drifting version string

Today the version is hand-typed in two places that drift: `HERO.badge` (`"First public release, v0.12"`) in `content.ts`, and demo `meta: "v0.12.4"` labels in `sections.tsx`. As part of any release that touches the changelog:

- Introduce **one** version constant (e.g. `export const VERSION = "0.12.0"` in `content.ts`) consumed by `HERO.badge`, OR have `sync-changelog.mjs` / `sync-openapi.mjs` inject `spec.info.version` at build time so the badge tracks releases automatically.
- Update `HERO.badge` to the shipped version on every release.

### 1.4 Build + deploy step (both surfaces)

The landing has **no committed CI workflow or deploy script**; it builds locally and deploys by hand to the GCS bucket behind the LB. The repeatable sequence:

```
# 1. Build (runs sync-changelog -> sync-openapi -> typecheck -> vite build)
pnpm -C apps/landing build

# 2. Copy compliance gate (NOT wired into build — run it explicitly)
node apps/landing/scripts/check-copy.mjs

# 3. Design verification (separate manual step)
pnpm -C apps/landing impeccable        # == impeccable detect src/   (or: npx impeccable detect)

# 4. Deploy the built site to the prod bucket behind the LB
gcloud storage rsync apps/landing/dist gs://wpmgr-landing-prod --recursive --delete-unmatched-destination-objects
```

The canonical prod app is `https://manage.wpmgr.app`; the apex `wpmgr.app` (this landing site) sits in `gs://wpmgr-landing-prod` behind the GCP LB (Cloudflare DNS). Steps 2 and 3 are **not** part of `pnpm build` and must be run by hand every time.

**Opportunistic improvement:** add a committed `scripts/deploy-landing.sh` wrapping step 4 (modeled on the existing `scripts/release-agent.sh`) so changelog publishes are repeatable rather than ad hoc.

---

## 2. The repeatable SOP — what the docs-writer agent runs at the END of every feature

Run this checklist as the closing act of every feature. Treat each box as blocking.

**A. Landing feature card — `apps/landing/src/data/content.ts` (content-only edit)**

- [ ] Append (or update) one `{ icon, title, desc }` object in `FEATURES.cards` (currently `content.ts` ~lines 74-111). `sections.tsx`'s `FeatureGrid` `.map()`s over this array, so **no JSX change is needed**.
- [ ] `icon` must be a valid `lucide-react` name resolvable by `apps/landing/src/components/icon.tsx`.
- [ ] If the feature is headline-worthy, also update `FEATURES.heading/subhead/body`, `HERO`, or `STATS`.
- [ ] If the feature deserves nav presence, add to `NAV.links` and/or `FOOTER.links`.
- [ ] Copy rules: **no em dashes, no en dashes, no competitor names**; use "to" for ranges; highlight Media where relevant. (Template in §3.2.)

**B. Changelog entry — root `CHANGELOG.md`**

- [ ] Add the change under `## [Unreleased]` (or under the version section if you are cutting a release now), in the correct `### Added` / `### Changed` / `### Fixed` (also `Deprecated` / `Removed` / `Security` as needed) bucket.
- [ ] On a release: rename `[Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, bump per SemVer, and reconcile the version with `spec.info.version` in `packages/openapi/openapi.yaml`. (Template in §3.1.)
- [ ] Update the landing version surface (`HERO.badge` / version constant) per §1.3.

**C. README — `/Users/mosamgor/Desktop/Terminal/wpmgr/README.md` (only if needed)**

- [ ] Update the README **only when** the feature changes setup, configuration, env vars, the feature list, or the supported surface. Routine internal changes do not touch it.
- [ ] If you added an env var (e.g. a new `WPMGR_*` for SMTP / notifications / an age master key), document it in `.env.example` and the README config section.
- [ ] Same copy rules apply: no competitors, no em/en dashes.

**D. Competitor-name guard (the hard constraint)**

- [ ] Grep your edits before shipping. NEVER name a competitor in landing copy, README, CHANGELOG, or code/comments:
  ```
  node apps/landing/scripts/check-copy.mjs   # gates apps/landing/src for em/en dashes + competitor names
  node apps/landing/scripts/check-copy.mjs --extra README.md CHANGELOG.md
  ```
  Both must come back clean (the root `CHANGELOG.md` and `README.md` are NOT covered by `check-copy.mjs`, so the grep is what protects them).
- [ ] **Sanctioned exception, and the only one:** the GitHub repository **description** field (the "About" blurb on github.com) may reference a competitor for discoverability. Nothing inside the repo tree may.

**E. Build, verify, deploy the landing**

- [ ] `pnpm -C apps/landing typecheck`
- [ ] `pnpm -C apps/landing build`
- [ ] `node apps/landing/scripts/check-copy.mjs`
- [ ] `pnpm -C apps/landing impeccable` (design verification)
- [ ] `gcloud storage rsync apps/landing/dist gs://wpmgr-landing-prod --recursive --delete-unmatched-destination-objects`
- [ ] Confirm the change is live on `wpmgr.app` and the `/changelog` page renders the new entry.

---

## 3. Templates

### 3.1 Changelog entry (Keep a Changelog + SemVer)

```markdown
## [Unreleased]

## [X.Y.Z] - YYYY-MM-DD
### Added
- One user-facing sentence per new capability. Lead with the benefit, name the
  surface ("Settings > SMTP"), no competitor names, no em or en dashes.

### Changed
- What behavior changed and what the user should expect now.

### Fixed
- The bug, stated as the symptom the user no longer sees.

### Security
- Hardening or fixes with a security impact (omit the section if empty).

### Deprecated
- Anything now discouraged and what replaces it (omit if empty).

### Removed
- Anything taken out (omit if empty).
```

SemVer reminder for picking `X.Y.Z`: **MAJOR** for breaking changes, **MINOR** for backward-compatible features, **PATCH** for backward-compatible fixes. Pre-1.0 (we are at `0.x`): breaking changes may ride a MINOR bump, new features and fixes ride MINOR/PATCH. Keep the version aligned with `spec.info.version`.

### 3.2 Landing feature card (`FEATURES.cards` object in `content.ts`)

```ts
{
  icon: "ShieldCheck",            // valid lucide-react name (see components/icon.tsx)
  title: "Short capability name", // 2 to 4 words, sentence case
  desc:
    "One to three sentences in plain language. Lead with what the user can now " +
    "do and why it matters. State guarantees concretely (what stays safe, what " +
    "is reversible, what is opt-in). No competitor names. No em or en dashes. " +
    "Use \"to\" for ranges (7 to 90 days).",
},
```

Style notes pulled from the existing cards: benefit-first, concrete guarantees ("originals stay archived", "reverted automatically", "opt-in, per site"), and never a brand comparison. If it is a Media capability, give it prominence per house style.

---

## 4. Wiring this as a definition-of-done (DoD) gate

The DoD for **any** WPMgr feature includes both documentation surfaces. Enforce it in layers, cheapest first:

**Layer 1 — PR checklist (always).** Every feature PR description carries this block, and the reviewer blocks merge if any box is unchecked:

```
## Docs DoD (required)
- [ ] Landing feature card added/updated in apps/landing/src/data/content.ts
- [ ] CHANGELOG.md entry added (version, date, Added/Changed/Fixed)
- [ ] README/.env.example updated (or N/A — explain why)
- [ ] No competitor names anywhere in repo (check-copy + grep clean)
- [ ] Landing builds, check-copy passes, impeccable clean, /changelog renders
- [ ] Landing deployed to gs://wpmgr-landing-prod (or queued for the release deploy)
```

**Layer 2 — copy gate (automatable now).** `node apps/landing/scripts/check-copy.mjs` already fails on em/en dashes and competitor names in `apps/landing/src/`. Extend its file walk to also scan repo-root `CHANGELOG.md` and `README.md` so the no-competitor / no-em-dash rule is machine-enforced on all four surfaces, then run it in the merge gate.

**Layer 3 — staleness check (when the generator exists).** Once `sync-changelog.mjs` exists, add a check that recompiles the changelog JSON and diffs it against the committed build inputs, and assert `HERO.badge` / the version constant matches `spec.info.version` in `packages/openapi/openapi.yaml`. A drift fails the build, mirroring the recommended "recompile-and-diff" guard for the MJML/checked-in-artifact pattern.

**Layer 4 — release ritual.** Cutting a release is the moment the `[Unreleased]` block becomes a dated `[X.Y.Z]` section, the version surfaces are bumped, and the landing is rebuilt + redeployed via §1.4. No feature is "released" until it appears under a dated version heading in `CHANGELOG.md` and on the live `/changelog`.

**The single bright line that overrides all convenience:** no competitor name may appear in the landing site, README, CHANGELOG, or any code or comment. The lone exception is the GitHub repository **description** field. If in doubt, leave the competitor out and describe WPMgr on its own terms.
---

## 5. Version surfaces, and which ones CI enforces

> **Note on `apps/landing`.** Sections 1 to 4 above were written when the public
> site was `apps/landing`. That app is retired and is no longer built or
> deployed. Read every `apps/landing` reference above as `apps/marketing`. This
> section is written against the current tree.

Sixteen files in this repo can name a version. Six of them name one to a reader
who acts on it, and those six are checked by the `Security audit` job in
`.github/workflows/ci.yml`. The rest are historical statements (a runbook
describing a publish that happened, spec prose naming the agent version that
introduced a field, a workflow comment citing an old version as an example) and
are deliberately left alone: they were true when written and they stay true.

### 5.1 What is checked, against what, with what tolerance

| Surface | Compared to | Tolerance |
| --- | --- | --- |
| `packages/openapi/openapi.yaml` `info.version` | top `CHANGELOG.md` entry | exact |
| `apps/marketing/app/(marketing)/changelog/page.tsx` newest entry | top `CHANGELOG.md` entry | 5 releases |
| GHCR pull tags, `WPMGR_VERSION` export and status line in `README.md`, plus the `WPMGR_VERSION` export in `docs/install.md` | top `CHANGELOG.md` entry, and each other | 1 release, and exact against each other |
| `AGENT_VERSION` in `apps/marketing/lib/content/home.ts` | `WPMGR_AGENT_VERSION` in `apps/agent/wpmgr-agent.php` | exact |
| `apps/agent/wpmgr-agent.php` plugin header, `WPMGR_AGENT_VERSION`, and `apps/agent/readme.txt` `Stable tag` | each other | exact |

**Tolerance is counted in RELEASES, by position in `CHANGELOG.md`'s own ordered
list**, not by subtracting patch numbers. So `0.61.131` is one release behind
`0.62.0`, and a minor bump does not disable the compare. There is no version of
"skip the check because the minor changed".

**The README plus `docs/install.md` tolerance of 1 is the one most likely to
surprise you.** It exists so a release PR can add the CHANGELOG entry before the
tag is pushed. It does not stretch to two. If you skip the install-pin bump on
one release, the next release fails CI, and the failure names the files and the
distance. Bump the pins in the release commit.

### 5.2 The agent version is not the repo version

The agent plugin version moves only when the agent itself changes, so a
control-plane-only release leaves it frozen. `0.61.128` through `0.61.131`
shipped in four days with the agent on `0.61.127`. Three consequences:

- Nothing that names the agent version is compared to `CHANGELOG.md`. The hero
  badge is compared to `apps/agent/wpmgr-agent.php`, and the agent's three
  self-declarations are compared to each other.
- `make agent-zip VERSION=` stamps a staged copy and never the source tree, so
  the checked-in agent version is the last released one, on purpose.
- When the agent version does move, it moves in the release commit, and
  `AGENT_VERSION` in `apps/marketing/lib/content/home.ts` moves in that same
  commit. That is the only thing keeping the badge and the wordpress.org listing
  in agreement.

### 5.3 Release-commit checklist for version surfaces

- [ ] `CHANGELOG.md`: `[Unreleased]` becomes `## [X.Y.Z] - YYYY-MM-DD`, and a bare `[Unreleased]` heading is left behind.
- [ ] `packages/openapi/openapi.yaml`: `info.version` to `X.Y.Z`.
- [ ] `apps/marketing/app/(marketing)/changelog/page.tsx`: add the entry (grouping releases is fine, within 5).
- [ ] `README.md`: three GHCR pull tags, the `WPMGR_VERSION` export, the status line.
- [ ] `docs/install.md`: the `WPMGR_VERSION` export.
- [ ] Only if the agent changed: `apps/agent/wpmgr-agent.php` header and `WPMGR_AGENT_VERSION`, `apps/agent/readme.txt` `Stable tag` plus changelog and Upgrade Notice entries, and `AGENT_VERSION` in `apps/marketing/lib/content/home.ts`.

Anything not on that list does not get bumped. `infra/docker-compose.prod.yml`
in particular names no version: its default is `${WPMGR_VERSION:-latest}` and
its usage comment says `vX.Y.Z`, so there is nothing there to go stale.
