# Working agreements

Binding. Nothing is here that a session can derive by reading the code.

**Almost nothing here is mechanically enforced.** The shell guards that used to
run as hooks were removed on 2026-08-14; they tried to decide what a command
would write by parsing its text, which is undecidable, and across a full day
they caught none of the real defects while the review process caught all of
them. What survives is `permissions.deny` in `.claude/settings.json`, the
`.githooks/pre-push` lock, and `ci.yml`. Everything else below is a standing
instruction to you, and it holds because you follow it.

Detail lives in `.claude/rules/*.md`, which load when you touch the files they
cover. `docs/harness.md` says what still enforces what.

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
`apps/api/internal/db/sqlc/**`, `apps/web/src/routeTree.gen.ts`,
`packages/openapi-client/src/generated/**`. A hand-sync of the sqlc tree caused a
production 500. Regenerate. A `deny` rule refuses `Edit`, `Write` and
`NotebookEdit` on all four; it does not see a shell write, so `sed -i`, `tee`, a
heredoc or `git apply` reaches them unchallenged.

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

**Nothing asks you before a main-thread write to a routed path.** A hook used to,
and it is gone. The table above is a standing instruction, and the moment it
matters is the moment nothing interrupts you: you notice the path, or the change
ships unrouted and unreviewed. Two of those paths were previously refused
outright rather than queried, because neither is a judgement call, and both are
now on you: editing a migration that already exists in `HEAD` (see
`.claude/rules/db-migrations.md`), and hand-editing a generated tree by shell.

**A change that spans a migration and Go code is two agents in sequence,
`database-engineer` first**, never one agent doing both.

**This rule outranks a narrower instruction inside one session.** A session
prompt that says "do not call agents unless asked" does not repeal it. Surface
the conflict out loud and get a ruling; never resolve it silently. Taking such
an instruction literally is how a whole OAuth sign-in feature got hand-built
inline and unreviewed here.

**Pass `model: "opus"` when the change is irreversible.** Frontmatter beats
inheriting from this loop, so a dispatch drops to whatever the agent pins even
when you are on Opus. `database-engineer` and `security-reviewer` already pin
`opus`; every other builder pins `sonnet`, so the override is what raises them.
Check the frontmatter rather than trusting this sentence. Override for:
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
5. **This clause, verbatim:** *Commit at the first increment that compiles,
   before any checks, and push. Open the draft PR on that first commit. Do not
   hold the commit for checks, fast or slow: commit first, then check, then
   report. If you are interrupted after the commit, the work survives.*
   A clause that waits for checks leaves the whole build window unprotected: on
   2026-09-01 an agent ran twenty-one minutes uncommitted and lost every line
   when interrupted, no branch, no remote ref, no PR.

A dead or interrupted agent is **resumed**, not re-run from zero. Its transcript
and its worktree both survive. Re-brief only when resuming is impossible, and
then narrower, carrying the previous attempt's partial findings.

**Never wait on a process with an unbounded loop.** `until … sleep` / `while
true` against a build, a CI run or a log file is the single shape that produced
most of this project's killed agents. Nothing denies that shape any more, so
write the bounded form yourself: a fixed number of polls, or a command run under
an explicit timeout, and report where it had got to when the count runs out.

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
- Finish by handing the worktree back. **Nothing reaps worktrees now** — the
  target that did was removed with the guards. Clear merged ones and orphaned
  `worktree-*` branches by hand (`git worktree list`, `git worktree remove`,
  `git branch -d`), and watch the disk: exhaustion has killed a build here
  mid-link.

## Claims

**Every number is the output of a command you ran in this turn, and the command
goes next to the number.** Every behavioural claim is something you executed.
Recount at the moment of writing, not from memory, not from the PR body, not
from earlier in the session, because this session compacts.

**Never hard-code a count** in a comment, a workflow, a CHANGELOG entry or a
commit message. Make the script print it. `.github/workflows/api-integration.yml`
and `ci.yml` still say "344 tests" in four places; run
`grep -rhoE '^func Test[A-Za-z0-9_]+' apps/api/tests/*.go | wc -l` and compare.

This paragraph carried its own worked example until 2026-08-14, quoting what
that command returned on the day it was written. The number had drifted by six
before anyone re-ran it, inside the rule against hard-coding counts. The command
stays; the answer does not.

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
in a YAML block scalar. `scripts/check-version-surfaces.sh` plus
`scripts/check-version-surfaces_test.sh` is the pattern. `ci.yml` runs the two
scripts directly, the self-test first, so a broken guard cannot pass by failing
open; `make check-versions-test` and `make check-versions` are the local names
for the same two.

## Delivery

**The only route to `main` is a pull request.** Branch, push the branch, open the
PR, let `ci.yml` and review run, merge. Never commit on `main` in the main
checkout and never `git push` while `HEAD` is `main` — not for a one-line fix,
not for a typo, not because CI will pass anyway.

**There is no server-side enforcement.** Branch protection on `main` carries
required contexts, but `enforce_admins` is deliberately `false` so an
owner-token push is accepted and no required check ever runs against it. That is
a standing decision, kept so an incident hotfix stays possible. Everything below
is therefore client-side, and a determined push always gets through:
`git push --no-verify`, `git send-pack`, and `git -c core.hooksPath=…` each skip
it. These layers stop an accident, which is what actually happened.

- `.githooks/pre-push` is the lock, and now the only one. Git hands it the
  already-resolved refs, so `eval`, a `cd`, quoting, `@`, and exotic refspecs
  cannot disguise a push from it. **It is repo-local config and config is never
  committed, so every clone and every fresh worktree starts unprotected.**
  `make hooks` installs it; `make hooks-status` says whether it is live and
  fails if the installed copy has drifted from the tracked one. **Run
  `make hooks-status` yourself at the start of a session that will push** —
  nothing reports its absence for you any more, and a guard that is silently
  absent is worse than none, because you would trust it.

**Approval has to precede the irreversible half.** On 2026-08-12 the #406 fix was
committed to `main`, pushed, and *then* followed by "want me to open a PR for
it?". The code was on `origin/main` before the question was asked, which makes it
an announcement wearing a question mark. Ask, then push.

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
Where a toolchain binary lives is a property of this machine and not of this
file, and nothing prints it for you at session start any more: resolve it with
`command -v` in the turn you need it, and if that comes back empty, say so and
stop rather than skipping the step.

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

Decisions, measured numbers and owner rulings go to `~/.wpmgr/worklog/<issue>.md`
**as they are made**, each with the command that produced it. Compaction
summarises the conversation away, and the next session re-derives it wrong.

**A worklog is private and never enters this repository.** Not in `docs/`, not
under any other path, not committed, not pushed, not attached to an issue or a
PR. This repository is public, and a worklog is the one artefact that routinely
holds what must not be: an unshipped finding, a defect's `file:line` before the
fix exists, the mechanism of a live vulnerability. It is a working note for the
next session, not a deliverable, and it has no audience outside this machine.

Write it to `~/.wpmgr/worklog/`, which is outside every checkout and every
worktree. Never create `docs/worklog/`, and never add a worklog path to
`.gitignore` — an ignore rule is itself committed, so it publishes the thing it
is hiding. The correct location leaves nothing to ignore.

On 2026-08-12 a worklog for GH #406 was written to `docs/worklog/406.md` while
the privilege escalation it described was live, unpatched and shipped, and while
the owner's standing ruling was to disclose nothing until the fix was deployed.
It was caught before any commit. The rule above exists because the instruction to
keep a worklog and the instruction to disclose nothing collided, and the session
followed the first without noticing the second.

This file is re-injected from disk after compaction. Path-scoped
`.claude/rules/` are not, until a matching file is read again. If a rule must
hold for a whole session, it belongs here.

**When a session compacts, write durable state to a file before the turn
ends.** The destination is not a free choice: this file takes guidance,
rules, pointers, working agreements, whatever is true forever and fit to be
public. `~/.wpmgr/worklog/` takes everything else that must survive, under
the rule above. If what you are about to write would not be fine to see
public forever, it belongs in the worklog, never here.

Reviews: `review.md`, read in full before reviewing anything.

## AI connection wizard

The plan for the AI connection wizard, its approved steps, the rulings behind
them, a per-step gap analysis and a build order, lives in
`~/.wpmgr/worklog/WIZARD-SPEC.md`. That path is outside this repository and
stays there, same as every other worklog (see Long sessions, above); this file
only names it, never quotes it. **Read the spec before any wizard work. Never
reconstruct the plan by reading the code.**

Current status belongs on the GitHub issues tracking each step, not here: a
status list here is exactly the kind of fact this file already bans
hard-coding, because it goes stale within days. On 2026-09-01 the session
rebuilt the wizard roadmap from the code after every compaction instead,
got it wrong each time, and the owner had to correct it repeatedly.
