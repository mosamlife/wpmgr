---
paths:
  - ".github/workflows/**"
  - "Makefile"
  - "scripts/**"
  - "infra/**"
---

# CI and build-gating logic

## Logic in YAML is untested logic

**Any conditional that gates a build goes in a repo script with a committed
test suite**, and the workflow calls the script. A guard written inline as a
YAML block scalar took four review rounds here and was wrong in every one:
each round found more, because there was nothing to run.

## Prove the guard fires and prove it does not over-fire

Plant the real failure, watch it go red, restore, watch it go green. Paste both
outputs.

Then construct the honest cases it must not block. A version guard here would
have reddened `main` on roughly one commit in three; a guard that fails correct
work gets switched off, and then it guards nothing.

Read the guard's own boundary conditions literally. One comment claimed a guard
skipped a single release when the condition skipped forever.

## Match the structure, not the first substring

A schema guard here read the first substring match instead of the
`CREATE TABLE`. If you are checking for a declaration, anchor on the
declaration.

## What actually exists

Seven workflows: `ci.yml`, `api-integration.yml`, `e2e-agent.yml`,
`plugincheck.yml`, `release.yml`, `security.yml`, `wporg-deploy.yml`.

`ci.yml` is the gate and has seven jobs: `go`, `js`, `commit-hygiene`,
`marketing`, `security`, `nginx-routing`, `php`. Green before and after every
merge. Run the same command CI runs, locally, not an approximation.

`release.yml` is build-only; its image matrix is `api`, `web`, `media-encoder`
to ghcr.io, and it never builds marketing. The marketing image ships only
through `infra/cloudbuild.marketing.yaml`.

`security.yml` is a placeholder: `workflow_dispatch` only, one `echo` step. Do
not cite it as a gate.

`api-integration.yml` is manual-dispatch and `ci.yml` excludes `apps/api/tests`
by name, so the tenancy and RLS proofs never run on a PR.

`plugincheck.yml` runs automatically on any PR touching the agent plugin.
`wporg-deploy.yml` is manual only and irreversible.

A gate that cannot find its binary must fail loudly, never be skipped. Resolve
each one with `command -v`; nothing prints their locations for you.

`govulncheck` reads a live advisory database, so it can redden `main` with zero
code change and a green PR can fail on merge. Fix the dependency. Never tag a
red commit.

## Commit message hygiene is enforced here

Job `Commit message hygiene` rejects `Co-Authored-By: Claude` and
`Claude-Session:` on the commits a PR adds. The repository is public and a
session URL cannot be recalled once pushed.

## Version lockstep

CHANGELOG, the marketing badge and changelog, and the OpenAPI `info.version`
move together in one release. `ci.yml` guards this. New endpoints cannot merge
unspecced; the route-coverage test enforces it.
