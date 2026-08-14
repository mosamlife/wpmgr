# The agent harness

What is in `.claude/`, what actually enforces what, and what is only written
down. The last of those is now most of it, and this file exists to say so
without dressing it up.

## What enforces what

| Rule | Mechanism | Strength |
|---|---|---|
| Do not hand-edit a generated tree | `permissions.deny` in `.claude/settings.json` | blocked for `Edit`, `Write` and `NotebookEdit`; no shell write is covered |
| Do not edit `apps/landing` | the same deny rules | the same |
| A push must not land on `main` | `.githooks/pre-push`, installed by `make hooks` | blocked, client-side, and only in a checkout where it is installed |
| No competitor names in `docs/**` or root markdown; no dashes or competitor names in shipped copy | `ci.yml`'s Security audit job | CI-gated |
| Every version surface moves together | `scripts/check-version-surfaces.sh` in `ci.yml`, with its `_test.sh` self-test run first; `make check-versions` locally | CI-gated |
| Routing a write to a specialist; not editing an applied migration; commit before you stop; the rules on counts, claims and reporting | `CLAUDE.md`, `review.md`, `.claude/agents/*.md` | **advisory** — read by every session and every subagent, enforced by review |

The last row is the bulk of the working agreements, and it is honest about what
it is. `CLAUDE.md` is context, not configuration: a session reads it and is
expected to follow it. Nothing stops a session that does not.

`permissions.deny` is written as `Edit(<path>)`. That is not a narrowing to the
`Edit` tool — a path denied there is refused for `Write` and `NotebookEdit` too,
for both rule shapes, a `**` prefix and a single named file. It covers no shell
write at all: a heredoc, `sed -i`, `tee`, `cp` or a `python -c` one-liner reaches
every denied path with no prompt and no record.

## The guards that were removed

Until 2026-08-14 a set of shell guards ran as Claude Code hooks and tried to
decide, by parsing the text of a command, what that command would write. That is
undecidable rather than merely hard — `eval`, `bash -c`, a path built at run
time, a variable holding a glob — and four rounds of fixes each closed real
bypasses and each following round found more. Over the same period the defects
that actually mattered, a privilege escalation, a check-then-act race that could
leave an organisation with no owner, an unclamped API-key role and several stale
hard-coded counts, were all found by an agent or a review bot attacking the work,
and none by the guards. They were deleted in `88ecb3d`, where the code and the
reasoning both remain readable.

What replaced them is the row above that says advisory, plus the review process.
That is a deliberate trade, not an oversight: a reader who finds the deletion in
git history is looking at a decision, not at rot.

## `.githooks/pre-push`

This one is kept, because it is different in kind. Git hands a pre-push hook
refs that are already resolved, so `eval`, a `cd`, quoting, `@` and exotic
refspecs are gone before it runs; it is reading a decided fact, not guessing at
one. It refuses a push that lands on `main`.

**It is repo-local config, and config is never committed, so every clone and
every fresh worktree starts unprotected.** `make hooks` installs it and
`make hooks-status` reports whether the installed copy is byte-identical to the
tracked one — run both after cloning and after editing `.githooks/pre-push`.
Nothing announces its absence for you any more, so check rather than assume.

It stops an accident, which is the thing that actually happened here. It is not
a barrier: `git push --no-verify`, `git send-pack` and `git -c core.hooksPath=…`
each skip it, and branch protection on `main` sets `enforce_admins` to `false`
by standing decision, so nothing server-side catches what gets past it.

## Files

```
CLAUDE.md                the working agreements, loaded every session and by every subagent
AGENTS.md                a symlink to CLAUDE.md (see below)
review.md                the review process, cited by every review brief
.claude/agents/*.md      one file per specialist
.claude/rules/*.md       path-scoped detail, loaded when you touch matching files
.claude/settings.json    deny rules, timeouts, worktree base ref. No hooks.
.worktreeinclude         gitignored files copied into every agent worktree (.env)
.githooks/pre-push       refuses a push that lands on main
```

`.claude/skills/` is gitignored (`.gitignore:70`, confirmed with
`git check-ignore -v .claude/skills/`), so a project skill is neither shared nor
committed. That is why procedure lives in `.claude/rules/` here rather than in a
skill.

## AGENTS.md

`AGENTS.md` is a **symlink to `CLAUDE.md`**. One file on disk, one rule, two
readers: Claude Code reads `CLAUDE.md`, and the other agents a drive-by
contributor to a public repository might use read `AGENTS.md`.

It is a symlink rather than a second document, or a `@AGENTS.md` import, because
every duplicated fact in this harness had drifted before it was rewritten. A
symlink cannot drift. On a Windows clone without symlink support, `AGENTS.md`
checks out as a one-line text file containing `CLAUDE.md`, which degrades to a
pointer rather than to wrong content.

## Settings that are load-bearing

`worktree.baseRef: "head"`. A subagent worktree branches from the repository's
**default branch** by default, not from the parent session's HEAD. Without this,
a specialist sent to fix in-progress work silently starts from `main` and reports
success on files that never had the work in them.

`cleanupPeriodDays: 14`. Claude Code's own sweep only covers paths under
`~/.claude`, and it skips any worktree holding changed files, untracked files or
unpushed commits — which is exactly the worktrees an agent did work in. It will
never reclaim a Go build cache, a Docker volume or a testcontainer. **Nothing in
this repository reclaims them either.** Reap merged worktrees and stale
`worktree-*` branches by hand with `git worktree list`, `git worktree remove` and
`git branch -d`, and watch the disk: exhaustion has killed a build here mid-link.

The `env` block raises the Bash tool's default and maximum command timeouts and
the background-subagent stall timeout, so a nine-minute integration suite or an
eighteen-minute CI lane is not cut off mid-run.
