# Working agreements

Rules that are binding, not advisory. Each one is here because breaking it cost
something real.

## Reporting to people outside this repository

**Never reply to a GitHub issue until the fix is merged AND deployed AND
verified in production.** Not when the branch is pushed, not when the PR is
green, not when it is merged. A reporter reading "fixed" goes and checks; if it
is not live yet, the next thing they write is that it is still broken, and they
are right.

The order is: merge, release, deploy, **verify against the running system**,
then reply.

**Verify against the thing itself, never against the pipeline's own report.** A
green deploy job says a container started, not that the fix is live.

- Control plane: query the deployed revision, and check the behaviour changed.
- Agent: download the published zip and grep it for the fix, do not trust the
  build log.
- wordpress.org: read the SVN server and the public download, not the workflow.
- Object storage: list the objects.

State in the reply what you verified and how, so the reporter can check it too.

**Close the loop.** Comment on the issue, close it, and answer any linked PR.
An issue fixed and never answered reads as an issue ignored.

**Be accurate about what was NOT fixed.** If existing damage is not repaired by
the change, say so plainly in the same message, with the exact remedy. Burying
that is how a fix report becomes a second bug report.

## Commit messages

**No assistant trailers, and never a session URL.** No `Co-Authored-By: Claude`,
no `Claude-Session:`, no `claude.ai/code/session` link. This repository is
public, so a commit message is published permanently and cannot be recalled. CI
enforces this (job `Commit message hygiene`) on the commits a PR adds.

149 commits from before this rule carry one. They stay: removing them means
rewriting public history, changing every commit id since June 2026, orphaning
release tags and breaking every clone. Do not reopen that.

## Claims

**Every number gets counted and every behavioural claim gets executed, before it
is written down.** In one week this repo shipped: a "42 releases" figure
computed by the exact method the change outlawed (real answer 190), a comment
claiming a guard skipped one release when it skipped forever, a reclaim prefix
missing a path segment that was about to be posted to a public issue, and a
"262 tests" count that was 344.

A claim in a comment, a CHANGELOG entry, a commit message or an issue reply is
part of the deliverable. Wrong prose is a defect.

## Guards and tests

**Prove it fires and prove it does not over-fire.** Plant the real failure and
watch the test go red; then restore and watch it go green. A test nobody has
seen fail is not known to test anything.

Then construct the *honest* cases it must not block. A guard that fails correct
work gets switched off, and then it guards nothing. A version guard here nearly
shipped that would have reddened main on roughly one commit in three.

**Watch out for tests that pass for the wrong reason.** This repo has shipped: a
frontend test asserting three server error codes that did not exist, RLS proofs
that went around the repo instead of through it so the policies were inert, and
a schema guard that read the first substring match rather than the table.

**Build-gating logic goes in a repo script with a committed test suite**, not in
a YAML block scalar. Untested CI logic took four review rounds and was wrong
each time.

## Delivery

**A multi-layer change deploys every layer.** api, web, agent, marketing.
Check what actually shipped rather than what was intended.

**Agent before control plane** when the agent fix stands alone, because the
fleet is only repaired when the plugin updates.

**`apps/api/tests` is not run by CI** (owner decision: 18 minutes per run). Run
`make test-integration` locally before merging anything touching RLS, the email
domain, tenant scoping, or object-storage reclamation. A regression there merges
green.

## Reviews

See `review.md`. Route security-sensitive and irreversible work to specialists
with independent adversarial review; do not self-approve deletes, GC, locks,
auth, RLS or migrations.
