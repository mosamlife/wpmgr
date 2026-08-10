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
- **It is the ordered list the distance compares are counted in.** "N releases behind" means "this file lists N entries newer than that version", and that is the unit everywhere in CI. Three of the five checks use it: `info.version` must equal its top entry exactly, and the marketing changelog page and the install pins are held within a tolerance measured in it. The two agent-facing checks do not read it at all, on purpose: the hero badge is compared to the agent plugin, and the agent's three self-declarations are compared to each other (§5.2). See §5.
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

The public `/changelog` page is a **hand-curated TSX page**, not a derived view. It carries a typed list of `ChangeEntry` objects, newest first, with a written summary each; the full history lives on GitHub Releases. Counted on 2026-08-10 it holds **72 entries, running from `0.61.131` back to `0.54.0`**, so it is a long curated history rather than a short recent window. Grouping several releases into one entry is normal and supported, written oldest first: `version: "0.61.49 - 0.61.53"`. The widest group it carries names eight versions, `0.61.41 - 0.61.48`.

CI keeps it within **5 releases** of the top `CHANGELOG.md` entry, and it reads the newest version in a grouped entry rather than the first one on the line. That detail is load-bearing: the two ends of `0.61.41 - 0.61.48` sit six releases apart in `CHANGELOG.md`'s list, so reading the first number would have failed an honest page. See §5.1.

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

**Layer 2: the version-surface guard.** One script, `scripts/check-version-surfaces.sh`, run by one step in the `Security audit` job, plus a second step that runs the script's own regression suite first. All five of its checks are described in §5:

1. `packages/openapi/openapi.yaml` `info.version` against the top `CHANGELOG.md` entry.
2. The marketing `/changelog` page against the top entry, within 5 releases.
3. The marketing hero badge against the agent plugin, exactly.
4. The agent version triple against itself, plus `release.yml` not stamping the agent zip from the git tag.
5. The required install pins, plus a repo-wide sweep for any other concrete image tag or `WPMGR_VERSION` value.

**Run it yourself before pushing. It needs nothing but a shell:**

```
make check-versions                          # or: scripts/check-version-surfaces.sh
make check-versions-test                     # the regression suite (75 cases as of this commit)
scripts/check-version-surfaces_test.sh badge # only cases matching "badge"
scripts/check-version-surfaces.sh /some/tree # check a tree other than this one
```

The suite builds throwaway trees under `$TMPDIR`, mutates one thing in each, and asserts the guard's exit code and output. It covers every hole three review rounds found, so reopening one turns a test red rather than shipping. Both files run on bash 3.2 with BSD tools (macOS) and on bash 5 with GNU tools (the CI runner); both were run on both before this landed.

Editing the guard means editing the script, and a behaviour change means a case in the suite. To check the suite is not vacuous, copy the script, put the old bug back, and run the suite against the copy:

```
WPMGR_VERSION_SURFACE_SCRIPT=/tmp/guard-with-the-bug-back.sh \
  scripts/check-version-surfaces_test.sh
```

That check used to be 245 lines of shell inside YAML block scalars in `ci.yml`, 144 of them in one step, with no test at all. Three rounds of review found a real hole each time, because nobody could run it and nobody could see what the last person had proved.

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

Seven files name a version to a reader who acts on it, and all seven are checked
by `scripts/check-version-surfaces.sh`, run by the `Security audit` job in
`.github/workflows/ci.yml`: `packages/openapi/openapi.yaml`,
`apps/marketing/app/(marketing)/changelog/page.tsx`,
`apps/marketing/lib/content/home.ts`, `README.md`, `docs/install.md`,
`apps/agent/wpmgr-agent.php` and `apps/agent/readme.txt`.

Other files mention a version in passing: a runbook describing a publish that
happened, spec prose naming the agent version that introduced a field, a
changelog entry describing what a release fixed. Those are historical statements
and are deliberately left alone; they were true when written and they stay true.
What stops a NEW one of them from quietly becoming an install instruction is the
sweep (§5.1), which reads the whole tree rather than a declared list.

### 5.1 What is checked, against what, with what tolerance

| Surface | Compared to | Tolerance |
| --- | --- | --- |
| `packages/openapi/openapi.yaml` `info.version` | top `CHANGELOG.md` entry | exact |
| `apps/marketing/app/(marketing)/changelog/page.tsx` newest entry | top `CHANGELOG.md` entry | 5 releases |
| GHCR pull tags, `WPMGR_VERSION` export and status line in `README.md`, plus the `WPMGR_VERSION` export in `docs/install.md` | top `CHANGELOG.md` entry, and each other | 1 release, and exact against each other |
| `AGENT_VERSION` in `apps/marketing/lib/content/home.ts` | `WPMGR_AGENT_VERSION` in `apps/agent/wpmgr-agent.php` | exact |
| `apps/agent/wpmgr-agent.php` plugin header, `WPMGR_AGENT_VERSION`, and `apps/agent/readme.txt` `Stable tag` | each other | exact |
| any other concrete `ghcr.io/mosamlife/wpmgr-*` tag or `WPMGR_VERSION` value, anywhere in the tree | top `CHANGELOG.md` entry | 1 release |

**Tolerance is counted in RELEASES**, not by subtracting patch numbers, and "N
releases behind" means "`CHANGELOG.md` lists N entries newer than this one". So
`0.61.131` is one release behind `0.62.0`, a minor bump does not disable the
compare, and a version the changelog has no heading for is still placed: on
2026-08-10, 24 patch versions of `0.61.x` had no heading, and under the old
lookup-by-heading rule a pin left on any of them passed forever.

**The install pins are a REQUIRED SET, and the set is anchored to a marker, not
guessed from a pattern.** A required pin is one that sits between
`<!-- wpmgr-install-pins:start -->` and `<!-- wpmgr-install-pins:end -->`. Six
are declared, in `pin_specs()` in `scripts/check-version-surfaces.sh`:

| File | Pin | Must sit in |
| --- | --- | --- |
| `README.md` | the release status line under the title | prose |
| `README.md` | the `docker pull` tag for the api image | a fenced code block |
| `README.md` | the `docker pull` tag for the web image | a fenced code block |
| `README.md` | the `docker pull` tag for the media-encoder image | a fenced code block |
| `README.md` | the `WPMGR_VERSION` export in the compose quick start | a fenced code block |
| `docs/install.md` | the `WPMGR_VERSION` export in the install guide | a fenced code block |

Four rules follow from that, and each of them exists because its absence was a
real hole:

- **Absence is an error, never a warning.** A declared pin that is not there, or
  a file with no `wpmgr-install-pins` region at all, names the file and the pin
  and fails. The guard has not failed to read something; it has read the file and
  established that a thing the reader needs is not in it.
- **A commented-out pin does not count.** The guard is markdown-aware and shell
  aware: HTML comment spans are removed, and inside a fenced block a line whose
  first character is `#` is a comment, so neither
  `<!-- export WPMGR_VERSION=v0.61.131 -->` nor a commented-out `docker pull` <!-- wpmgr-version-ignore -->
  satisfies presence. A trailing comment beside a live line keeps its line.
- **Inside a region the count is a MINIMUM.** Adding an honest "Upgrading"
  section with a second current pin passes, and prose outside a region may name
  any version it likes. Everything found is still version-compared, so a surplus
  pin cannot go stale in silence.
- **A malformed value is reported as malformed.** `v0.61.131-rc1` is not a
  published tag and `WPMGR_VERSION=0.61.131` without the leading `v` is not one <!-- wpmgr-version-ignore -->
  either; both are errors that say so, rather than reading as current or as
  missing.

Decoration a formatter may add is ignored (quotes, backticks, an `export`
keyword, extra spacing, indentation, a blockquote marker, a tilde fence), and the
part a reader acts on is required (the variable name, the image path, the leading
`v`). A reformatted pin is still read, so its version is still compared: that is
why `WPMGR_VERSION="v0.19.0"` fails as stale rather than passing as absent. <!-- wpmgr-version-ignore --> The
same rule governs the badge and agent patterns, which is why WordPress's own
`define( 'X', 'y' );` spacing no longer switches the agent check off.

**The sweep is the backstop for everything nobody declared.** Any concrete
`ghcr.io/mosamlife/wpmgr-*:vX.Y.Z` or `WPMGR_VERSION=vX.Y.Z` in any tracked file
is held to the same 1 release tolerance, so a new doc with a stale pull command
fails even though no list mentions it. Placeholders are ignored
(`${WPMGR_VERSION:-latest}`, `:latest`, `vX.Y.Z`, an empty assignment).
`CHANGELOG.md` is exempt because it is a historical record, the guard and its
test are exempt because they carry the patterns as data, and any other single
line can opt out by carrying the word `wpmgr-version-ignore` (this file uses that
escape hatch three times, for the examples above).

**The README plus `docs/install.md` tolerance of 1 is the one most likely to
surprise you.** It exists so a release PR can add the CHANGELOG entry before the
tag is pushed. It does not stretch to two. If you skip the install-pin bump on
one release, the next release fails CI, and the failure names the files and the
distance. Bump the pins in the release commit.

### 5.2 The agent version is not the repo version

The agent plugin version moves only when the agent itself changes, so a
control-plane-only release leaves it frozen. `0.61.128`, `0.61.129` and
`0.61.130` shipped between 2026-08-07 and 2026-08-10 with the agent on
`0.61.127`, which moved again in the `0.61.131` release commit. Three
consequences:

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
- [ ] `README.md`: three GHCR pull tags, the `WPMGR_VERSION` export, the status line (all inside the `wpmgr-install-pins` regions).
- [ ] `docs/install.md`: the `WPMGR_VERSION` export.
- [ ] Only if the agent changed: `apps/agent/wpmgr-agent.php` header and `WPMGR_AGENT_VERSION`, `apps/agent/readme.txt` `Stable tag` plus changelog and Upgrade Notice entries, and `AGENT_VERSION` in `apps/marketing/lib/content/home.ts`.
- [ ] Run `scripts/check-version-surfaces.sh` before pushing. It reports every drifted surface in one pass, so you fix them in one commit rather than one CI cycle each.

Anything not on that list does not get bumped. `infra/docker-compose.prod.yml`
in particular names no version: its default is `${WPMGR_VERSION:-latest}` and
its usage comment says `vX.Y.Z`, so there is nothing there to go stale.
