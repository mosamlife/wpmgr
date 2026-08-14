# Review process

How a change gets from written to merged. Written from what has gone wrong here.

Nothing loads this file automatically. Every review brief cites it by absolute
path, and `security-reviewer` reads it as its first act. The four
non-negotiables below are also inline in `.claude/agents/security-reviewer.md`
so a reviewer that never opens this file still has them; **this file is
authoritative** if the two ever disagree.

## Who reviews what

Route the work, then route the review to someone who did not write it.

| Change touches | Review |
|---|---|
| RLS, tenant scoping, auth, crypto, the agent command protocol | `security-reviewer`, mandatory |
| Deletes, GC, object-storage reclamation, run-locks, advisory locks | `security-reviewer`; its job is to find data that gets deleted and should not |
| A migration that drops, alters or cascades | `security-reviewer`, plus a stated converge path for databases on the earlier version |
| Anything irreversible | Never self-approved |

**Severity is judged by consequence, never by provenance.** "The already-shipped
sibling has the same shape" is a scope note for the owner, not a downgrade. That
reasoning let a reproduced, irreversible data-loss race through here; a review
bot had to catch it, and it was fixed only after the owner overruled the call.

Reviewers cost real time, so scope beats volume: one well-scoped reviewer beats
three vague ones.

**Flag only gaps that affect correctness or the stated requirements.** A
reviewer asked to find gaps will find some whether or not they exist, and
chasing every one produces defensive code and tests for cases that cannot
happen. Say in the brief what would *not* count as a finding.

## Scope the review before launching it

1. **The diff.** A commit range or an explicit file list. Not "the changes".
2. **The question.** What would count as a finding, and what would not.
3. **The budget.** The files to read, the commands to run, what is explicitly
   out of scope ("do not audit CI history").
4. **The stop condition.** What the reviewer returns when it finds nothing, and
   the instruction that if it needs something outside the budget it **reports
   that it needs it and stops** rather than going to get it.
5. **The role to connect as**, for anything touching RLS, tenancy or deletion.

**Review a committed range, never a live working tree.** A reviewer here watched
a concurrent process revert a file mid-review and had to rebuild an isolated
copy from `HEAD`. If the work is not committed, say so and stop. Reviewers run
with `isolation: worktree` for this reason.

**Commit before the slow suite.** `make test-integration` is about
nine minutes locally. A reviewer interrupted mid-suite with uncommitted notes
loses all of them.

**Never wait with an unbounded loop.** `until … sleep` against a build or a CI
run is what got most killed runs here killed. Nothing denies that shape any
more, so this one is on you: poll a bounded number of times, or run the wait
with an explicit timeout, and report where it got to when the budget runs out.

## The four non-negotiables

**1. Does it break something that currently works?** Construct the honest cases
and run them. This matters as much as the bug: three of the four holes in the
version guard were over-firing or silent-skipping, not under-detection.

**2. Do the tests actually test it?** Delete or invert the thing the test
protects, watch it go red, restore it, watch it go green. **Paste both outputs
and the commands that produced them.** A review that says "the tests cover this"
has not happened. This is the check that catches what this repo actually ships -
not tests that fail, but tests that pass for the wrong reason:

- a frontend test asserting three server error codes the server never returns;
- m112's RLS proofs, which opened their own connections and so never went
  through the dispatch that sets the scope, every test green, every policy
  inert;
- a schema guard that read the first substring match instead of the
  `CREATE TABLE`, and passed with both cascading foreign keys planted;
- a negative control recorded with `t.Logf` inside an `if`, so it could not
  fail.

**3. What does this check print, and what does it exit, when its input is
missing, its command fails, or its pattern matches nothing?** Empty command
substitution, a loop over nothing, a grep that matches nothing, a CI guard that
failed open when `git rev-list` errored, each has shipped here as a guard
announcing success over its own errors.

**4. Run it as the real role.** This is the highest-yield technique in this
repository and nothing that only reads code substitutes for it. Connect as the
provisioned `NOSUPERUSER NOBYPASSRLS` application role, go through the
repository layer, and execute the statement you are about to publish. It is how
a documented operator recovery statement was found to be impossible on a
`FORCE ROW LEVEL SECURITY` table for every real install, after passing in every
test that ran as superuser.

## Numbers in the review

Every count the reviewer states (tests, releases, rows, call sites, policies)
is recounted **by the reviewer, with a command, in the review**. Never repeated
from the diff, the PR body, or earlier in the session. Four wrong figures
shipped here in one week, each inherited rather than counted.

## Bot reviews

CodeRabbit, Qodo and Greptile review most PRs. They have caught things
adversarial review missed, including the highest-severity finding of one week,
and they have been confidently wrong.

- **Read every finding and check it.**
- **Never apply a bot patch unread.** A committable suggestion here contained
  `+$conn_entries;` instead of `++$conn_entries;`, a no-op that would have
  closed the finding while fixing nothing. Take the reasoning; write the fix.
- **Close each thread with a comment naming the commit that addressed it, or the
  reason it is wrong.** An unanswered resolve records nothing.
- **Wait for the bots before merging.** Most PRs here merge in under ten
  minutes, which is faster than the bots post. Either the findings are worth
  reading or the bots are worth turning off.

## Before merge

- `ci.yml` green, and green again after the merge commit.
- `make test-integration` locally for any diff touching RLS, tenant scoping, the
  email domain, or reclamation: CI does not run that package.
- `make check-versions` for anything that moves a version surface.
- For anything under `.claude/`, a human read of the whole diff. There is no
  lint and no test suite over the agent definitions or the settings, so a broken
  one fails silently, at dispatch time, in someone else's session.
- The mutation performed by hand, both outputs pasted.
- Every number recounted, every behavioural claim executed.
- Every bot thread read and answered.

## After merge

Merge, release, deploy every affected layer, **verify against the running
system**, and only then reply to the issue. Details in `CLAUDE.md`.

Default merge is a merge commit. Squash only when the branch carries a commit
that must not survive (a scratch commit that deliberately widens an RLS policy
to prove a guard catches it, for example), and verify afterwards that it is
unreachable from `main`.
