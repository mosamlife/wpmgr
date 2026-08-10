---

# Standing Process: Every Shipped WPMgr Feature Lands on the Marketing Site AND in a Release Changelog

**Owner:** docs-writer lead
**Status:** STANDING PROCESS (active)
**Applies to:** every feature, capability, or user-visible behavior change shipped in the WPMgr monorepo

This document defines the canonical changelog home, the end-of-feature SOP the docs-writer agent runs, the templates to copy, and how the whole thing is enforced. It is binding: a feature is **not done** until both surfaces below reflect it.

**Sections 1 to 4 were rewritten on 2026-08-10.** They previously described `apps/landing`, a Vite site deployed by syncing `dist/` to a GCS bucket. That app is retired: it is not in `pnpm-workspace.yaml`, nothing builds it, and nothing deploys it, so every command in those sections failed if you ran it. The public site is `apps/marketing`, a Next.js app that ships as a container image to Cloud Run. Reading the old sections with `apps/landing` mentally replaced by `apps/marketing` did not work either, because the deploy mechanism, the file paths and the build steps are all different things now, not renamed ones. What replaced each retired instruction is stated in place below.

---

## 0. The non-negotiable rule

> **Every shipped feature MUST be reflected in TWO places before the feature is considered done:**
> 1. The **marketing site** (`apps/marketing`): the feature content module that covers it, and the `/changelog` page when a release goes out.
> 2. A **release changelog entry** in the root `CHANGELOG.md`.

Two copy rules run alongside it, and they have **different scopes**. Read this before writing anything.

- **The dash rule (no em dashes, no en dashes, use "to" for ranges) applies everywhere.**
- **The competitor-name rule applies everywhere EXCEPT `apps/marketing`.** It exists for code provenance: features here were built clean-room, so crediting a rival product as the source of a technique inside shipped code or in the docs is the thing to prevent. It was never meant to stop the marketing site from writing an honest comparison, and that scoping was made explicit by the owner on 2026-08-06. The site now carries deliberate comparison content under `apps/marketing/lib/content/compare/`. The rule still binds on agent PHP, the plugin header, `apps/agent/readme.txt`, `docs/`, and root markdown, which is where the 2026-07-06 leak actually happened.

Both rules are machine-enforced, in two different places, and neither covers everything. See §4.

---

## 1. Where each surface lives

### 1.1 Canonical source: root `CHANGELOG.md`

- **File:** `CHANGELOG.md` at the repo root, alongside `README.md`.
- **Format:** [Keep a Changelog](https://keepachangelog.com/en/1.1.0/) + [Semantic Versioning](https://semver.org/).
- **Why root:** it is the conventional open-source artifact, it is the single edit point for the docs-writer, and it is the file GitHub Releases and every downstream surface are reconciled against.
- **It is also the ordered list that every version guard measures distance in.** Position in it is the unit of "N releases behind" everywhere in CI. See §5.
- **Versioning anchor:** `info.version` in `packages/openapi/openapi.yaml` must equal the top `CHANGELOG.md` entry exactly. CI fails the build when they disagree. Do not invent a parallel version scheme.

Structure (full template in §3.1):

```
# Changelog

All notable changes to WPMgr are documented here.
Format: Keep a Changelog (keepachangelog.com). Versioning: SemVer (semver.org).
House rules: no em dashes, no en dashes, no competitor names. Use "to" for ranges.

## [Unreleased]

## [0.61.131] - 2026-08-09
### Added
- ...
```

`CHANGELOG.md` sits outside `apps/marketing/scripts/check-copy.mjs`'s walk. The competitor half of the house rules is still machine-enforced on it, by the separate `Docs vocabulary check` step in `ci.yml`, which greps root markdown and `docs/`. **The dash half is not enforced on `CHANGELOG.md` or `README.md` by anything.** Hold it by hand.

### 1.2 The published changelog: `apps/marketing/app/(marketing)/changelog/page.tsx`

The public `/changelog` page is a **hand-curated TSX page**, not a derived view. It carries a typed list of `ChangeEntry` objects, newest first, showing roughly the twenty most recent meaningful releases with a written summary each; the full history lives on GitHub Releases. Grouping several releases into one entry is normal and supported, written oldest first: `version: "0.61.49 - 0.61.53"`.

CI keeps it within **5 releases** of the top `CHANGELOG.md` entry, counted by position in that file, and it reads the newest version in a grouped entry rather than the first one on the line. See §5.1.

**Do not replace it with a build-time generator that reads root `CHANGELOG.md`.** `infra/Dockerfile.marketing` copies the workspace manifests, `packages/` and `apps/marketing/` into the build context and nothing else, so repo-root files are absent inside the image build. This is not hypothetical: `apps/marketing/scripts/sync-openapi.mjs` already tries to read `CHANGELOG.md` for a version stamp and takes its `catch` silently on every production build for exactly that reason. Anything tempted to reach outside `apps/marketing` at build time needs a `COPY` added first, and then needs that widened context to stay wide forever.

### 1.3 Version surfaces

Superseded by §5, which lists every surface that names a version, what it is compared to, and with what tolerance. The old advice here (introduce one version constant, update the hero badge every release) is wrong in one important way: the hero badge names the **agent** version and must not move on a control-plane-only release. Read §5.2 before touching it.

### 1.4 Build, verify, deploy the marketing site

The site has no automatic deploy. It builds locally and ships as a container image to Cloud Run. From the repo root:

```
pnpm install --frozen-lockfile
pnpm -C apps/marketing typecheck
pnpm -C apps/marketing lint
pnpm -C apps/marketing build                  # runs scripts/sync-openapi.mjs, then next build
node apps/marketing/scripts/check-copy.mjs    # repo-wide copy gate, plain node, no install needed
pnpm -C apps/marketing impeccable             # design verification
```

`check-copy.mjs` is deliberately **not** part of the marketing `build` script and must not be added to it. Its second scope reads `apps/agent`, which is absent from that image's build context, so inside the production build it fails on every one of those files. CI is the correct place for a repo-wide copy lint, and that is where it runs.

Deploy is a Cloud Build image plus a Cloud Run revision:

```
gcloud builds submit --config infra/cloudbuild.marketing.yaml \
  --substitutions=_IMAGE=<registry>/marketing,_VERSION=<x.y.z-or-sha> \
  --region <cloud-build-region> .

gcloud run deploy wpmgr-marketing --image <registry>/marketing:<version> --region <run-region>
```

The concrete registry, the regions, and the four public analytics substitutions are documented in the header of `infra/cloudbuild.marketing.yaml`. **That file is the authority for the command, not this one**, which is why the placeholders above are not filled in here: a copy of a deploy command in a process doc is the thing that rots first.

Then confirm the change is live on `wpmgr.app` and that `/changelog` renders the new entry. The canonical app is `https://manage.wpmgr.app`; the apex `wpmgr.app` is this marketing site.

---

## 2. The repeatable SOP: what the docs-writer agent runs at the END of every feature

Run this checklist as the closing act of every feature. Treat each box as blocking.

**A. Marketing content (`apps/marketing/lib/content/`, content-only edits)**

- [ ] Find the feature page the capability belongs to: `apps/marketing/lib/content/features/<slug>.ts`, all of which are registered in `FEATURE_REGISTRY` in `features/index.ts`. Update its `FeaturePageData` (hero, problem, steps, faq, whichever the change touches).
- [ ] If the capability is headline-worthy, also add or refresh a `ClusterFeature` inside `HOME_FEATURES.clusters` in `lib/content/home.ts`. Pages map over these arrays, so no JSX change is needed. (Template in §3.2.)
- [ ] A genuinely new feature page means a new module under `features/`, exported from `features/index.ts`, and added to `FEATURE_REGISTRY` under its slug. A page that is not in the registry does not exist.
- [ ] Every `icon` must be a valid `lucide-react` name.
- [ ] Copy rules: no em dashes, no en dashes, "to" for ranges. Competitor names are allowed here and nowhere else (§0).

**B. Changelog entry, root `CHANGELOG.md`**

- [ ] Add the change under `## [Unreleased]`, in the correct `### Added` / `### Changed` / `### Fixed` bucket (also `Deprecated` / `Removed` / `Security` as needed).
- [ ] On a release: rename `[Unreleased]` to `## [X.Y.Z] - YYYY-MM-DD`, leave a bare `[Unreleased]` heading behind, and bump `info.version` in `packages/openapi/openapi.yaml` to match exactly. (Template in §3.1.)
- [ ] Work the rest of §5.3 in the same commit.

**C. README and `.env.example`, only if needed**

- [ ] Update `README.md` **only when** the feature changes setup, configuration, env vars, the feature list, or the supported surface. Routine internal changes do not touch it.
- [ ] A new `WPMGR_*` env var goes in `.env.example` and the README config section.
- [ ] If a release is being cut, the README install pins move too. They are a required set and CI names the file and the pin when one is missing (§5.1).

**D. Copy gates**

- [ ] Run the copy gate over the whole repo, from the repo root. It needs no install:
  ```
  node apps/marketing/scripts/check-copy.mjs
  ```
- [ ] The competitor check over `docs/` and root markdown is a separate gate with a separate word list, and that list lives in the `Docs vocabulary check` step in `.github/workflows/ci.yml`. Read the list there and grep your own edits against it. It is deliberately not copied into this file: a second copy of a banned-word list is a list that goes stale, and this file is itself one of the files that step scans.
- [ ] **Sanctioned exception, and the only one outside `apps/marketing`:** the GitHub repository **description** field (the "About" blurb on github.com), which is out of the repo tree.

**E. Build, verify, deploy**

- [ ] Everything in §1.4, in that order.
- [ ] Confirm live on `wpmgr.app`, and that `/changelog` renders the new entry.

---

## 3. Templates

### 3.1 Changelog entry (Keep a Changelog + SemVer)

```markdown
## [Unreleased]

## [X.Y.Z] - YYYY-MM-DD
### Added
- One user-facing sentence per new capability. Lead with the benefit, name the
  surface ("Settings > SMTP"), no em or en dashes.

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

SemVer reminder for picking `X.Y.Z`: **MAJOR** for breaking changes, **MINOR** for backward-compatible features, **PATCH** for backward-compatible fixes. Pre-1.0 (we are at `0.x`): breaking changes may ride a MINOR bump, new features and fixes ride MINOR/PATCH. Keep the version equal to `info.version` in `packages/openapi/openapi.yaml`.

### 3.2 Home feature entry (`ClusterFeature` in `HOME_FEATURES.clusters`, `lib/content/home.ts`)

```ts
{
  icon: "ShieldCheck",              // valid lucide-react name
  title: "Short capability name",   // 2 to 4 words, sentence case
  summary:
    "One or two sentences in plain language. Lead with what the user can now do " +
    "and why it matters. No em or en dashes. Use \"to\" for ranges (7 to 90 days).",
  bullets: [
    "A concrete guarantee: what stays safe, what is reversible, what is opt-in.",
    "A second one. Keep them short enough to scan.",
  ],
  link: { href: "#platform-operate" },  // optional, an in-page cluster anchor
}
```

Style notes from the existing entries: benefit first, concrete guarantees ("originals stay archived", "reverted automatically", "opt-in, per site"), and prominence for Media capabilities per house style. A full feature **page** is a different and larger shape (`FeaturePageData` in `lib/content/types.ts`); copy an existing module under `features/` rather than writing one from the type.

---

## 4. How this is enforced

Enforcement is in three layers. Layers 1 and 2 are machine gates that exist and run today; layer 3 is the part that is still human.

**Layer 1: the copy gates.** In the `Security audit` job of `.github/workflows/ci.yml`, on every push and PR:

- `Docs vocabulary check` greps `docs/` and root markdown for competitor names.
- `Shipped copy check` runs `apps/marketing/scripts/check-copy.mjs` over the marketing site (dash rule) and the agent plugin (dash rule on user-facing strings, `readme.txt` and the plugin header, plus the competitor rule over every shipped agent PHP file including comments).

Neither one covers dashes in `CHANGELOG.md` or `README.md`. That gap is deliberate for now and is held by hand.

**Layer 2: the version-surface guards.** Same job, four steps, all described in §5:

- `Docs version drift guard (openapi + marketing changelog)`.
- `Docs version drift guard (marketing badge + install pins)`, which also asserts that every required install pin **exists**, in the file it belongs to, the expected number of times.
- `Agent version triple check (plugin header, constant, Stable tag)`.
- `Agent release asset must not be stamped from the git tag`, in the PHP job.

**Layer 3: the PR checklist.** Everything above is about surfaces that can be compared to each other mechanically. Whether the changelog entry is true, and whether the marketing copy describes the feature that actually shipped, cannot be. Every feature PR carries this block, and the reviewer blocks merge if a box is unchecked:

```
## Docs DoD (required)
- [ ] Marketing content updated in apps/marketing/lib/content/ (feature page, and home cluster if headline)
- [ ] CHANGELOG.md entry added (version, date, Added/Changed/Fixed)
- [ ] README/.env.example updated (or N/A, explain why)
- [ ] Copy gates clean (check-copy.mjs + the docs vocabulary grep)
- [ ] Marketing typechecks, builds, impeccable clean, /changelog renders
- [ ] Marketing deployed (or queued for the release deploy)
```

**The bright line, restated:** the dash rule binds everywhere; the competitor rule binds everywhere except `apps/marketing`, whose comparison content is deliberate. The GitHub repository **description** field is the one sanctioned exception outside the repo tree. When in doubt in code, in `docs/`, or in the agent, leave the competitor out and describe WPMgr on its own terms.

---

## 5. Version surfaces, and which ones CI enforces

Sixteen files in this repo can name a version. Seven of them name one to a reader
who acts on it, and those seven are checked by the `Security audit` job in
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

**The install pins are a REQUIRED SET, not whatever the guard happens to find.**
Six pins are declared, one `expect_pin` line each, in the
`Docs version drift guard (marketing badge + install pins)` step:

| File | Pin |
| --- | --- |
| `README.md` | the release status line under the title |
| `README.md` | the `docker pull` tag for the api image |
| `README.md` | the `docker pull` tag for the web image |
| `README.md` | the `docker pull` tag for the media-encoder image |
| `README.md` | the `WPMGR_VERSION` export in the compose quick start |
| `docs/install.md` | the `WPMGR_VERSION` export in the install guide |

A pin that is not found, in the file it belongs to, the declared number of times,
is an **error naming that file and that pin**. It is not a warning and it is not
a pass. That list in `ci.yml` is the specification: adding a pin to the docs
means adding a line to it, and the count on each line is the only thing that can
tell "the README lost one of its three pull commands" from "the README never had
one". The patterns ignore decoration a formatter may add (quotes, backticks, an
`export` keyword, extra spacing, indentation) and require the part a reader acts
on (the variable name, the image path, the leading `v`), so a reformat is read
rather than reported missing, and its version is still compared.

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
