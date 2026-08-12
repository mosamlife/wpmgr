# The agent harness

What is in `.claude/`, what enforces what, and what is only written down.

This file replaces `docs/harness-install.md`, whose opening table was headed
"What is already active (committed, no action needed)" and listed `CLAUDE.md`
and `review.md`, neither of which existed anywhere in the repository. An
install doc that misreports install state is this project's signature defect
wearing a different hat.

## What enforces what

| Rule | Mechanism | Strength |
|---|---|---|
| Do not hand-edit a generated tree | `permissions.deny` + a `deny` arm in `route-guard.sh` + arm 3 of `bash-guard.sh` | blocked |
| Do not edit an already-applied migration | `route-guard.sh` and `bash-guard.sh`, both computed from `git cat-file -e HEAD:<path>` | blocked |
| Do not edit `apps/landing` | `permissions.deny` + both guards | blocked |
| Route a write to a specialist | `route-guard.sh` | prompts once per destination per session, on the main thread only |
| Do not write an unbounded wait loop | `bash-guard.sh` | blocked |
| The rules for publishing prose outside the repo | `bash-guard.sh` | restated at the moment it matters |
| Commit before you stop | `commit-gate.sh` (SubagentStop) | blocks once, in agent worktrees, only for paths that agent wrote |
| Know the machine's disk and guard health | `session-brief.sh` (SessionStart) | reported, never blocks |
| Agent definitions stay true | `agent-lint.sh`, in `ci.yml` via `make harness-check` | CI-gated |
| The guards keep working | `guards_test.sh`, same job | CI-gated |
| Everything else in `CLAUDE.md` | prose | **advisory** |

The last row is the honest one. `CLAUDE.md` is context, not configuration. If a
rule there matters enough that it must hold every time, it has to move up this
table.

## Two deliberate fail-open choices, both announced

`route-guard.sh` and `bash-guard.sh` need `jq` and **exit 0 without it**. The
alternative, blocking every edit on a fresh clone of a public repo, is a
guard that gets switched off within the hour, and then it guards nothing.

The compensating control is `session-brief.sh`, which prints
`route-guard and bash-guard are INACTIVE: jq is not installed` at the top of
every session. Silence is the thing this project bans; a stated, visible
degradation every session is not silence.

Verify the guards are actually loaded with `/context` and, if you want the
detail, an `InstructionsLoaded` hook. Do not assume.

## What the deny rules actually cover

`permissions.deny` is written as `Edit(<path>)`. That is not a narrowing to the
`Edit` tool. A path denied there is refused for `Write` and `NotebookEdit` as
well, for both rule shapes: a `**` prefix and a single named file. Checked on
2026-08-12 by attempting a `Write` to `apps/api/internal/db/sqlc/` and to
`apps/web/src/routeTree.gen.ts`; both came back
`File is in a directory that is denied by your permission settings`.

It covers no shell write at all. `route-guard.sh` is a `PreToolUse` hook on
`Edit|Write|NotebookEdit`, so a heredoc, `sed -i`, `tee`, `cp` or a `python -c`
one-liner reached every denied path with no prompt and no record. Arm 3 of
`bash-guard.sh` closes that for the generated trees, the dead app and any
migration already in `HEAD`, and only when the protected path is the **target**
of the write, so reading, grepping and copying out of those paths stay free.

What it cannot close, stated plainly because pretending otherwise is worse than
the gap: it matches one command string, so a path built from a variable, one
that arrives through `xargs` or base64, a write done inside a script the command
merely invokes, an interactive editor, or `git apply` of a diff whose target
paths never appear on the command line, all still get through. The bar moves
from frictionless to deliberate and visible. That is the whole claim.

## Why the route guard asks once per area, not once per file

The first version asked on 843 of the 926 files touched in the preceding 30 days
(91%), and on 2531 of 2670 tracked files. That is a prompt on essentially every
main-thread edit, and a guard that cries wolf gets switched off, which is the
failure this harness exists to prevent.

Two changes, both measurable with `scripts/claude/route-guard-coverage.sh`:

- A test whose suite `ci.yml` actually runs no longer routes. `apps/api/tests`
  is the deliberate exception and still does, because that package is excluded
  from CI by name.
- The decision is remembered per session, per destination, for
  `WPMGR_ROUTE_GUARD_TTL_MIN` minutes (90 by default). Routing changes the
  outcome when a piece of work starts, not on its fortieth file. A sensitive
  path carries its own key, so the first write to auth, tenancy, crypto or a
  deletion path still prompts.

Replaying the same 30 days with each commit day as one session, prompts fall
from 65.6 per session to 4.7 (`route-guard-coverage.sh --sessions`). If the
payload carries no session identity the guard does not fail open: it asks every
time, which is the old behaviour.

## Files

```
CLAUDE.md                     the working agreements, loaded every session and by every subagent
AGENTS.md                     a symlink to CLAUDE.md (see below)
review.md                     the review process, cited by every review brief
.claude/agents/*.md           seven specialists
.claude/rules/*.md            path-scoped detail, loaded when you touch matching files
.claude/settings.json         hooks, deny rules, timeouts, worktree base ref
.worktreeinclude              gitignored files copied into every agent worktree (.env)
scripts/claude/route-guard.sh    PreToolUse: Edit|Write|NotebookEdit
scripts/claude/bash-guard.sh     PreToolUse: Bash
scripts/claude/agent-writes.sh   PostToolUse: records which files each agent wrote
scripts/claude/commit-gate.sh    SubagentStop, scoped to what THAT agent wrote
scripts/claude/session-brief.sh  SessionStart
scripts/claude/agent-lint.sh     lints the agent definitions; --self-test proves each check
scripts/claude/guards_test.sh    the regression suite over both PreToolUse guards
scripts/claude/harness-reap.sh   reclaims worktrees, branches, build cache, volumes
scripts/claude/route-guard-coverage.sh  what fraction of real work the route guard interrupts
scripts/claude/fact-census.sh    how many harness files restate the same fact
```

`guards_test.sh` prints its own assertion count. An earlier version of this line
hard-coded it, which is the thing `CLAUDE.md` bans two sections further down.

`.claude/skills/` is gitignored (`.gitignore:70`, confirmed with
`git check-ignore -v .claude/skills`), so a project skill is neither shared nor
committed. That is why procedure lives in `.claude/rules/` here rather than in a
skill.

## AGENTS.md

`AGENTS.md` is a **symlink to `CLAUDE.md`**. One file on disk, one rule, two
readers: Claude Code reads `CLAUDE.md`, and the other agents a drive-by
contributor to a public repository might use read `AGENTS.md`.

It is a symlink rather than a second document, or a `@AGENTS.md` import,
because every duplicated fact in this harness had drifted before it was
rewritten. A symlink cannot drift. On a Windows clone without symlink support,
`AGENTS.md` checks out as a one-line text file containing `CLAUDE.md`, which
degrades to a pointer rather than to wrong content.

Everything under `.claude/` is Claude-specific and buys another tool nothing;
it stays where it is.

## Settings that are load-bearing

`worktree.baseRef: "head"`. A subagent worktree branches from the repository's
**default branch** by default, not from the parent session's HEAD. Without this,
a specialist sent to fix in-progress work silently starts from `main` and
reports success on files that never had the work in them.

`cleanupPeriodDays: 14`. Claude Code's own sweep only covers paths under
`~/.claude`, and it skips any worktree holding changed files, untracked files or
unpushed commits, which is exactly the worktrees an agent did work in. It will
never reclaim a Go build cache, a Docker volume or a testcontainer. That is what
`make harness-reap` is for.

The `env` block raises the Bash tool's default and maximum command timeouts and
the background-subagent stall timeout, so a nine-minute integration suite or an
eighteen-minute CI lane is not cut off mid-run. These are the documented
mechanism; the guard against runaway waits does not depend on them.

## Running it

```
make harness-check       # lint + both self-tests; this is what ci.yml runs
make harness-reap        # report what can be reclaimed
make harness-reap-apply  # reclaim it
```

The hooks execute shell from `scripts/claude/`. They are committed precisely so
they can be read before they are trusted. Project hooks require accepting the
workspace trust dialog for this folder; until then they do not run.
