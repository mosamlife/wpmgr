# Working agreements

Binding. Nothing is here that a session can derive by reading the code, and
nothing is here that a hook already enforces.

Detail lives in `.claude/rules/*.md`, which load when you touch the files they
cover. `docs/harness.md` says which rules are enforced by a hook and which are
only written down.

## The map

Live: `apps/api` (Go control plane), `apps/web` (React dashboard),
`apps/agent` (the WordPress plugin, published on wordpress.org as
`fleet-agent-site-manager`), `apps/marketing` (the public wpmgr.app Next.js
site), `apps/tracker` (the RUM collector; its build output ships inside the
agent zip as `assets/wpmgr-rum.js`).

Dead, do not edit, do not deploy: `apps/landing` (last commit 2026-06-20,
superseded by `apps/marketing`, absent from `pnpm-workspace.yaml`), `apps/cli`
(untouched since the 2026-06-01 squash).

Generated, never hand-edit: `apps/api/internal/api/gen/**`,
`apps/api/internal/db/sqlc/**`, `apps/web/src/routeTree.gen.ts`. A hand-sync of
the sqlc tree caused a production 500. Regenerate; a `deny` permission rule and
the route guard both refuse the edit.

## Routing: who writes the code

The session you are in is a **router**. Reading, searching, planning, deciding,
replying: yours. Writing code: a specialist's, in its own worktree.

| Path | Agent |
|---|---|
| `apps/api/migrations/**`, `apps/api/db/schema.sql`, `apps/api/db/query/**` | `database-engineer` |
| `apps/api/**/*.go` | `backend-architect` |
| `apps/web/**`, `apps/marketing/**`, `apps/tracker/**`, `packages/**` | `frontend-architect` |
| `apps/agent/**` | `wp-agent-engineer` |
| `.github/workflows/**`, `Makefile`, `scripts/**`, `infra/**` | `devops-engineer` |
| `docs/**`, `README.md`, `CHANGELOG.md` | `docs-writer` |
| auth, RLS, tenancy, crypto, the agent protocol, deletes, GC, reclamation, locks | `security-reviewer` reviews it, always, and never writes the fix |

**"It is only a small edit" is exactly when this gets skipped.** Size is not the
test. The path is the test. The one exception: a change you can describe in one
sentence, in one file, on no path in that table.

A `PreToolUse` hook asks before a main-thread write to a routed path and names
the specialist. It asks rather than denies so you can still approve an inline
edit deliberately; approving it without saying why is the failure it exists to
catch. Two things it denies outright, because neither is a judgement call:
editing a migration that already exists in `HEAD` (see `.claude/rules/db-migrations.md`),
and hand-editing a generated tree.

**A change that spans a migration and Go code is two agents in sequence,
`database-engineer` first**, never one agent doing both.

**This rule outranks a narrower instruction inside one session.** A session
prompt that says "do not call agents unless asked" does not repeal it. Surface
the conflict out loud and get a ruling; never resolve it silently. Taking such
an instruction literally is how a whole OAuth sign-in feature got hand-built
inline and unreviewed here.

**Pass `model: "opus"` when the change is irreversible.** The builders are
pinned `sonnet` in their frontmatter, and frontmatter beats inheriting from this
loop, so a dispatch drops to Sonnet even when you are on Opus. Override for:
deletes, GC, retention sweeps and object-storage reclamation; locks, leader
election, job claiming and anything racing a live process; auth, crypto, key
storage, RLS and tenancy; destructive migrations. Leave UI, copy, docs, additive
endpoints, tests and config on the default. The cheap tier wrote competent code
that would have deleted a running backup's working directory; the review caught
it. Never drop the review either way: builder self-anchoring is structural and
independent of model tier.

**Every reported bug goes through a workflow, not an ad-hoc fix.** Parallel
investigation over each half of the system, then one reconciling design pass
that states the root cause with `file:line` and a failing-to-passing regression
test, then the specialist build, then an adversarial review sized to the risk,
then ship and verify. Right-size the fan-out to severity. Do not trust the
reporter's root cause, or your own first one: on the restore data-loss bug both
the reporter and one investigator had it wrong, and an ad-hoc fix would have
fixed the wrong thing.

**For a latency or performance bug, the first change is measurement.** Add
phase timing, deploy, capture one real slow request, and let the numbers name
the culprit. Reasoning from symptoms shipped four wrong fixes to one endpoint
across three releases; instrumenting found it in one.

## Briefing an agent

The measured failure here is not that long agents die. It is that a blocked
agent gets killed, and a killed agent that has not committed loses everything.
Every brief states, or it is not ready to send:

1. **One job.** A commit range, a file list, named run ids. Never a category
   ("check CI", "audit the tests"). Size it to finish in about ten minutes.
   Splitting a long job into short ones is how work survives; running two agents
   on one job at once is how work gets lost.
2. **Key files, pre-resolved**, one line of purpose each. The agent should spend
   its budget on judgement, not on search.
3. **What counts as done, and what would count as a finding.**
4. **The budget, and what to do at it**: report what you have and stop.
5. **This clause, verbatim:** *Commit and push as soon as the fast checks pass.
   Do not hold the commit while the slow suite runs. Commit first, then run the
   slow suite, then report. If you are interrupted after the commit, the work
   survives.*

A dead or interrupted agent is **resumed**, not re-run from zero. Its transcript
and its worktree both survive. Re-brief only when resuming is impossible, and
then narrower, carrying the previous attempt's partial findings.

**Never wait on a process with an unbounded loop.** `until … sleep` / `while
true` against a build, a CI run or a log file is the single shape that produced
most of this project's killed agents. A hook denies that shape and its denial
prints the three bounded alternatives that work on this machine.

## Isolation

Every writing agent has `isolation: worktree`, and `worktree.baseRef` is `head`
so it branches from your work rather than from `main`. Two agents in one
checkout means one commits the other's uncommitted edits; that has happened
here.

- **Stage by name.** Never `git add -A`, never `git add .`, never `commit -a`.
- Inside a worktree, plain separate commands only: heredocs with unquoted
  delimiters and brace expansion are refused and cannot be re-enabled.
- `.env` is gitignored; `.worktreeinclude` carries it into worktrees.
  Missing-config errors inside a worktree start there.
- Finish by handing the worktree back. `make harness-reap` removes merged ones
  and prunes orphaned `worktree-*` branches. Nothing else reaps them, and disk
  exhaustion has killed a build here mid-link.

## Claims

**Every number is the output of a command you ran in this turn, and the command
goes next to the number.** Every behavioural claim is something you executed.
Recount at the moment of writing, not from memory, not from the PR body, not
from earlier in the session, because this session compacts.

**Never hard-code a count** in a comment, a workflow, a CHANGELOG entry or a
commit message. Make the script print it. Four hard-coded counts in this repo
were wrong within days: `.github/workflows/api-integration.yml` and `ci.yml`
still say "344 tests" in four places, and
`grep -rhoE '^func Test[A-Za-z0-9_]+' apps/api/tests/*.go | wc -l` says 386.

A claim in prose is part of the deliverable. **Wrong prose is a defect.**

## What shipped code may say

**Never name a competitor plugin in code, a code comment, a commit message, or
a committed doc.** That is the clean-room boundary, and `ci.yml` enforces it
over `docs/**.md` and root markdown. Describe the technique neutrally: "standard
minify-and-rewrite technique", never "as used by X".

**Never write a defensive disclaimer.** No "not copied from", no "original
implementation". They imply the question arose, which reads worse than saying
nothing.

**Never name a local reference directory in a tracked ignore file**, not
`.gitignore`, not `.gcloudignore`, not `.dockerignore`. Those files are
committed, so the line publishes the reference. References are used and then
deleted, never ignored.

Two carve-outs, both deliberate. `apps/marketing/**` **may** name competitors:
comparison pages are wanted there, and only the dash rule applies. Integration
targets, the functional conflict-detection map and wordpress.org directory slugs
in `plugin_signatures` are facts about interop, not provenance, and stay.
`apps/agent/readme.txt` is the wordpress.org listing page and stays under the
strict rule.

## Standing permissions

**Agent releases publish without asking.** When a feature needs a new agent
version, ship it as part of shipping and surface the version and channel in the
reply. This covers the agent specifically; every other outward-facing action
still gets a decision.

## Guards and tests

**Prove it fires, then prove it does not over-fire.** Plant the real failure,
watch it go red, restore, watch it go green, paste both outputs with their
commands. A test nobody has seen fail is not known to test anything. Then
construct the honest cases it must not block: a guard that reddens correct work
gets switched off, and then it guards nothing.

**A guard that finds nothing must go red, not green.** This project's signature
defect is announcing success over its own errors. Ask of every check: what does
it print, and what does it exit, when its input is missing, its command fails,
or its pattern matches nothing?

**Reach the thing under test through the same code path production uses**, and
as the same database role. Proofs that opened their own connections left the RLS
policies inert while every test passed; a documented recovery statement worked
as superuser and failed as `wpmgr_app`, which is the role every install runs as.

**Build-gating logic goes in a repo script with a committed test suite**, never
in a YAML block scalar. `scripts/check-version-surfaces.sh` plus its test file
is the pattern.

## Delivery

`ci.yml` is the gate. **It does not run the integration package**, and
`.claude/rules/ci-and-build-logic.md` names that package and says why. That is
where the tenancy and RLS proofs live, so run `make test-integration` locally
before merging anything touching RLS, tenant scoping, the email domain, or
object-storage reclamation. A regression there merges green.

A multi-layer change deploys every layer: api, web, agent, marketing. Agent
before control plane when the agent fix stands alone: the fleet is only
repaired when the plugin updates. `make check-versions` before any release; the
marketing hero badge names the **agent** version, so it must not move on a
control-plane-only release.

A DoD step that cannot find its binary must fail loudly, never be skipped.
`session-brief.sh` prints where each toolchain binary actually is, every session,
because that answer is a property of the machine and not of this file.

## Reporting outside this repository

**Never say "fixed" until it is merged AND deployed AND verified against the
running system.** Order: merge, release, deploy, verify, reply.

**Verify the thing, never the pipeline's report of it.** A green deploy job says
a container started. Query the deployed revision; download the published zip and
grep it; list the objects. State what you verified and how, and say plainly what
is **not** fixed, with the exact remedy, in the same message.

## Commit messages

**No assistant trailers, ever, and no session URL.** No `Co-Authored-By:
Claude`, no `Claude-Session:`, no `claude.ai/code/session` link. This repository
is public and a commit message cannot be recalled. `ci.yml`'s
`commit-hygiene` job fails the PR on either.

Commits from before this rule carry them. They stay: rewriting them changes
every commit id since June 2026 and orphans the release tags.

## Long sessions

Decisions, measured numbers and owner rulings go to `docs/worklog/<issue>.md`
**as they are made**, each with the command that produced it. Compaction
summarises the conversation away, and the next session re-derives it wrong.

This file is re-injected from disk after compaction. Path-scoped
`.claude/rules/` are not, until a matching file is read again. If a rule must
hold for a whole session, it belongs here.

Reviews: `review.md`, read in full before reviewing anything.
