# Review process

How a change gets from written to merged here. Written from what has actually
gone wrong, not from a template.

## Who reviews what

Route the work, then route the review to someone who did not write it.

| Change touches | Review |
|---|---|
| Deletes, GC, object-storage reclamation | `security-reviewer`, and the reviewer's job is to find data that gets deleted and should not |
| RLS, tenant scoping, auth, crypto, agent protocol | `security-reviewer`, mandatory |
| Migrations that drop, alter or cascade | `security-reviewer`, plus a converge path for databases that ran the earlier version |
| Anything irreversible | Do not self-approve. Ever. |

**Nobody approves their own irreversible change.** Four adversarial rounds on
one bug this month each found something the previous round missed, including a
fix that reintroduced the original bug one level up.

## The two questions every review must answer

1. **Does it break something that currently works?** Construct the honest cases
   and run them.
2. **Do the tests actually test it?** Revert the fix, watch them fail, restore,
   watch them pass. Report what you observed, not that you expect it.

A review that says "looks correct" without either answer has not happened.

## Bot reviews

CodeRabbit, Qodo and Greptile run on every PR.

**Read them, and check each one.** Measured over this repo: roughly half are
real, and they have caught things four rounds of adversarial review missed.
They have also been confidently wrong. Verify rather than accept or dismiss.

**Never apply a bot patch unread.** A CodeRabbit committable suggestion here
contained `+$conn_entries;` instead of `++$conn_entries;`, a no-op that would
have closed the finding while fixing nothing. Take the reasoning, write the fix
yourself.

**Do not merge past unresolved bot threads.** That is where the code-debt pile
came from. If a finding is wrong, say why in the thread and resolve it.

## Verification before merge

- CI green, including the bot checks
- `make test-integration` run locally when the change touches RLS, email,
  tenant scoping or reclamation, because CI does not run it
- The mutation performed by hand: break the thing, see the test fail
- Every number in the diff counted; every behavioural claim executed

## After merge

Merge, release, deploy every affected layer, **verify against the running
system**, and only then reply to the issue. See `CLAUDE.md`.

## Squash when history carries something that must not survive

Default merge is a merge commit. Squash when the branch contains commits that
would be dangerous to land on, for example a scratch commit that deliberately
widens an RLS policy to prove a guard catches it. Verify afterwards that the
commit is unreachable from `main`.
