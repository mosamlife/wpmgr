# Working agreements

Binding, not advisory. Every rule here cost something real, and nothing is here
that a session can derive by reading the code.

This file is loaded by every session **and by every subagent**. Auto memory is
not: a subagent loads the `CLAUDE.md` hierarchy and `.claude/rules/`, and
nothing else. A rule that lives only in memory is invisible to every specialist
that does the building — which is why the routing rule failed for a month.

Detail is deliberately elsewhere. `.claude/rules/*.md` load when you touch the
files they cover. `docs/harness-install.md` says what is enforced and what is
still only written down.

## Routing: who writes the code

The session you are in is a **router**. Reading, searching, planning, deciding,
replying: yours. Writing code: a specialist's, in its own worktree.

| Path | Agent |
|---|---|
| `apps/api/**/*.go`, `apps/api/db/query/**` | `backend-architect` |
| `apps/api/migrations/**`, `apps/api/db/schema.sql` | `db-migration-engineer` |
| `apps/web/**` | `frontend-architect` |
| `apps/agent/**` | `wp-agent-engineer` |
| `apps/marketing/**` (content, TSX) | `frontend-architect` |
| `.github/workflows/**`, `Makefile`, `scripts/**`, `infra/**` | `devops-engineer` |
| `docs/**`, `README.md`, `CHANGELOG.md` | `docs-writer` |
| auth, RLS, tenancy, crypto, agent protocol, deletes, GC, locks | `security-reviewer` reviews it, always |

**"It is only a small edit" is exactly when this gets skipped, and it is the
most common way this rule dies.** Size is not the test. The path is the test.

The one exception: a change you can describe in one sentence, in one file, on
no path in that table. Everything else routes.

A `PreToolUse` hook asks before a write to a routed path from the main thread,
and names the specialist. It asks rather than denies so you can still approve an
inline edit on purpose. Approving it without a reason is the failure it exists
to catch. If a routed change genuinely does not need a specialist, say so out
loud and get a ruling — never resolve that silently.

Routing buys three things: an isolated checkout, a tool set matched to the job,
and a reviewer who did not write the code. It does **not** buy speed. Do not
fan out to go faster.

## Isolation: never two writers in one checkout

Every writing agent has `isolation: worktree`, and `worktree.baseRef` is `head`
so it branches from your work rather than from `main`. Two agents in one
checkout means one commits the other's uncommitted edits. That has happened
here; it was convenient rather than corrupting, which is luck, not a result.

- **Stage by name.** Never `git add -A`, never `git add .`, never `commit -a`.
  Before committing, reconcile `git status --porcelain` against the files you
  personally edited, and stage only those.
- **The primary checkout stays on `main` and stays clean.** Feature work happens
  in a worktree.
- Run agents in parallel only when they own different app directories. Same
  directory means sequential.
- `.env` is gitignored, so a fresh worktree lacks it; `.worktreeinclude` carries
  it. Missing-config errors inside a worktree start there.
- Finish by handing the worktree back: commit or discard, then remove it.

## Finishing: a run that cannot end is a run that failed

Every delegation brief states four things, or it is not ready to send:

1. **The work** — a commit range, a file list, named CI run ids. Never a
   category ("check CI", "audit the tests").
2. **What counts as done**, and what would count as a finding.
3. **A budget** — files to read, commands to run, and what is out of scope.
4. **What to do at the budget** — report what you have and stop. Never go and
   get more.

**Retry at most twice, and never with the same prompt.** A run here spent 4h42m
retrying one task end-to-end six times; 87% of its agent-time was thrown away
because each attempt started from zero. Attempt two receives attempt one's
partial findings and a narrower brief, or it does not happen.

`maxTurns` is set on every agent. It bounds turns, not wall-clock time — a slow
agent doing plausible-looking work stays under it. It is a backstop for a bad
brief, not a substitute for one.

**Never block on a long job.** Start it with `run_in_background`, do other work,
check it with a single non-blocking call. Every wait needs a deadline *and* a
liveness check: an agent here polled `until grep MAKE_EXIT` against a process
that had already exited and had to be killed by hand. A hook denies that shape.

A test that fails a wall-clock assertion under parallel load and passes alone is
a flake. Record it and move on; do not re-run a 20-minute lane to confirm it.

## Claims

**Every number is the output of a command you ran in this turn. Every
behavioural claim is something you executed.** Put the command next to the
number so the next reader can re-run it.

Recount at the moment of writing — not from memory, not from the PR body, not
from earlier in the session. A number that was true 200 messages ago is not
evidence, and this session compacts.

In one week this repo shipped: "42 releases" computed by the exact method the
change outlawed (190 by the method it mandated), a comment saying a guard
skipped one release when it skipped forever, a "262 tests" count that was 344,
and a reclaim prefix missing a path segment that was about to be posted to a
public issue.

**Never hard-code a count in a comment, a workflow, a CHANGELOG entry or a
commit message.** Write the command, or make the script print it. Three
hard-coded counts in this repo were wrong within days of being written.

A claim in prose is part of the deliverable. **Wrong prose is a defect, not a
typo.**

## Guards and tests

**Prove it fires, then prove it does not over-fire.** Plant the real failure,
watch it go red, restore, watch it go green, paste both outputs with their
commands. A test nobody has seen fail is not known to test anything.

Then construct the honest cases it must not block. A guard that reddens correct
work gets switched off, and then it guards nothing.

**A guard that finds nothing must go red, not green.** This project's signature
defect is announcing success over its own errors — six shipped instances in
sixty days, plus one live on `main` in the gate that enforces this file's own
commit rule. Ask of every check: what does it print, and what does it exit, when
its input is missing, its command fails, or its pattern matches nothing?

**Reach the thing under test through the same code path production uses.** The
m112 RLS proofs opened their own transactions, so the policies were inert while
every test passed. A schema guard read the first substring match instead of the
`CREATE TABLE` and passed with both cascading foreign keys in place. Anchor on
the declaration; go through the repository layer.

**Build-gating logic goes in a repo script with a committed test suite**, never
in a YAML block scalar. `scripts/check-version-surfaces.sh` plus its test file
is the pattern. Untested YAML logic took four review rounds here and was wrong
in every one, because each round's proof was thrown away.

## Delivery

**A multi-layer change deploys every layer**: api, web, agent, marketing. Check
what shipped, not what was intended.

**Agent before control plane** when the agent fix stands alone — the fleet is
only repaired when the plugin updates.

**CI does not run `apps/api/tests`.** That is where the tenancy and RLS proofs
live. Run `make test-integration` locally before merging anything touching RLS,
tenant scoping, the email domain, or object-storage reclamation. A regression
there merges green.

`make check-versions` before any release. Four version surfaces move in
lockstep and the marketing hero badge names the **agent** version, so it must
not move on a control-plane-only release.

## Reporting outside this repository

**Never say "fixed" until it is merged AND deployed AND verified against the
running system.** Not at push, not at green CI, not at merge. Order: merge,
release, deploy, verify, reply.

**Verify the thing, never the pipeline's report of it.** A green deploy job says
a container started. Query the deployed revision; download the published zip and
grep it; read the SVN server; list the objects. State what you verified and how,
so the reporter can repeat it, and say plainly what is **not** fixed, with the
exact remedy, in the same message. A hook restates this when you type
`gh issue comment`, because that is when it is needed.

## Commit messages

**No assistant trailers, ever, and no session URL.** No `Co-Authored-By:
Claude`, no `Claude-Session:`, no `claude.ai/code/session` link. This repository
is public; a commit message is published permanently and cannot be recalled. CI
enforces it on the commits a PR adds.

Commits from before this rule carry them. They stay — rewriting them changes
every commit id since June 2026, orphans the release tags and breaks every
clone. Do not reopen that.

## Long sessions

Decisions, measured numbers and owner rulings **go to `docs/worklog/<issue>.md`
as they are made**, with the command that produced each number. Compaction
summarises the conversation away; anything that existed only in the conversation
is gone, and the next session re-derives it wrong. That happened this month —
four compactions in seven days.

This file is re-injected from disk after compaction. Path-scoped
`.claude/rules/` are **not**, until a matching file is read again. If a rule
must hold for a whole session, it belongs here.

## The map

Live: `apps/api` (Go control plane), `apps/web` (React dashboard), `apps/agent`
(the WordPress plugin, published on wordpress.org), `apps/marketing` (the public
wpmgr.app site). Dead, do not edit or deploy: `apps/landing` (superseded by
`apps/marketing`, absent from `pnpm-workspace.yaml`), `apps/cli`. Generated,
never hand-edit: `apps/api/internal/api/gen/**`, `apps/api/internal/db/sqlc/**`,
`apps/web/src/routeTree.gen.ts`.

Reviews: see `review.md`, read in full before reviewing anything.
