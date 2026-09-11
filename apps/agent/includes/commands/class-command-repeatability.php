<?php
/**
 * CommandRepeatability: whether running a command a second time is safe when
 * the caller never learned the outcome of the first run.
 *
 * This is METADATA, exactly like CommandEffect, and it is a SEPARATE axis from
 * it. Conflating the two loses information the caller needs:
 *
 *   backup   is a Write       and is safe to repeat.
 *   restore  is Destructive   and is NOT safe to repeat.
 *   file_delete is Destructive and IS safe to repeat.
 *
 * A single "danger" score cannot express those three at once, which is why
 * there are two enums.
 *
 * The scenario this answers is the real one: the agent was dispatched, the
 * connection dropped, and nobody knows whether the command ran. Is sending it
 * again the safe move, or does a blind retry risk doing harm the first attempt
 * did not?
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

/**
 * The three repeatability classes a command may declare.
 *
 * The string backing values are the wire form and must never be renamed.
 *
 * How to choose, in order — take the FIRST that applies:
 *
 *   1. Can a second run, launched without knowing the first run's outcome, do
 *      harm the first run did not — destroy newer data, act twice on the
 *      outside world, or act on state the first run itself created?  -> Unsafe
 *   2. Does a second run redo real work or produce another artifact?  -> Repeatable
 *   3. Otherwise (a second run converges on the same end state)       -> Idempotent
 *
 * Note what rule 1 catches that "is it destructive" does not. `db_snapshot`
 * with action=revert is convergent on paper — importing the same dump twice
 * yields the same database — but a blind retry AFTER a successful first run
 * throws away everything written in between. That is harm the first run did
 * not do, so it is Unsafe.
 *
 * As with CommandEffect, an action-dispatch command declares its WORST case
 * across the actions it accepts.
 */
enum CommandRepeatability: string
{
    /**
     * A second run converges on the same end state. Retry freely.
     *
     * Setting an option to a value, deleting a path that is already gone,
     * purging a cache that is already cold: running it twice is
     * indistinguishable from running it once.
     */
    case Idempotent = 'idempotent';

    /**
     * A second run is safe, but it is not free.
     *
     * It redoes the work, or produces an additional artifact: another queued
     * job, another staged archive, another test email. Nothing is lost and
     * nothing gets worse — the cost is duplicated effort or duplicated noise.
     * A caller may retry without asking a human; a caller that cares about
     * cost or noise may prefer to check first.
     */
    case Repeatable = 'repeatable';

    /**
     * A second run can do harm the first run did not. Never retry blind.
     *
     * Three shapes land here:
     *   - the retry destroys data written since the first run succeeded
     *     (revert, TRUNCATE, restore-over-live, rollback);
     *   - the retry acts twice on something outside this site that cannot be
     *     recalled (re-sending a real customer email);
     *   - the retry acts on state the first run itself changed, so it is no
     *     longer doing what was asked (renaming a path the first run already
     *     moved; re-applying an update mid-flight).
     *
     * The correct handling is to determine what actually happened first — read
     * the resulting state, or ask a human — and only then decide.
     */
    case Unsafe = 'unsafe';
}
