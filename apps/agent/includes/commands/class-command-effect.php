<?php
/**
 * CommandEffect: what a command does to the managed WordPress site.
 *
 * This is METADATA. Nothing in the agent reads it to allow or refuse anything;
 * it exists so that something else — an approval screen, a read-only
 * connection, an audit trail — can be built on a declared fact instead of on a
 * guess made from a command's name.
 *
 * The question this answers is narrow and deliberate: after a SUCCESSFUL run,
 * what is different about this site? Not "is this command dangerous", not "does
 * it talk to the control plane", not "does it cost money". Just: what changed
 * here, and can it be put back.
 *
 * @package WPMgr\Agent\Commands
 */

declare(strict_types=1);

namespace WPMgr\Agent\Commands;

/**
 * The three effect classes a command may declare.
 *
 * The string backing values are the wire form. They are stable: the control
 * plane, a future approval prompt, and any log line will compare against these
 * exact strings, so a case may be added but an existing value must never be
 * renamed.
 *
 * How to choose, in order — take the FIRST that applies:
 *
 *   1. Can a successful run leave this site missing or overwriting data that
 *      the command itself does not retain a copy of?   -> Destructive
 *   2. Does it change durable site state at all?        -> Write
 *   3. Otherwise                                        -> Read
 *
 * "Durable site state" means options, database rows, files on disk, installed
 * code, user accounts — anything an operator could be asked about. It does NOT
 * mean derived state the site rebuilds by itself: page caches, object caches,
 * update transients, a cron nudge, or a temp file the command removes before it
 * returns. A command that only touches derived state is still a Read.
 *
 * Where a command's effect depends on its arguments — the action-dispatch
 * commands such as db_snapshot, db_table_action and media_clean — it declares
 * its WORST case, and says so in the comment on effect(). A caller must be able
 * to trust the label without also parsing the parameters.
 */
enum CommandEffect: string
{
    /**
     * Observes and reports. Leaves no durable change to this site.
     *
     * A Read may still: compute and cache a derived value, nudge WP-Cron, write
     * and immediately remove a temp file, or send what it read to the control
     * plane (file_download_prepare streams file content to CP-minted presigned
     * URLs and is still a Read — the SITE is unchanged). Data leaving the site
     * is a confidentiality question, which is a different axis from this one and
     * is not what this enum records.
     */
    case Read = 'read';

    /**
     * Changes durable site state, and nothing is lost by it.
     *
     * Either the command only adds or sets something, or whatever it replaced is
     * retained by the command itself (file_write stages an encrypted copy of the
     * previous bytes before overwriting; update takes a pre-update snapshot), or
     * what it replaced was derived state the site can rebuild.
     *
     * Write is the label for "this will change the site, and an operator should
     * be told, and it can be undone".
     */
    case Write = 'write';

    /**
     * Can permanently remove or overwrite data the command does not keep a copy
     * of. Undoing it needs something from outside the command — a backup taken
     * earlier, a snapshot, or nothing at all.
     *
     * Deletes, DROP, TRUNCATE, and restore-over-live all land here. So does a
     * command whose destructive path is only reachable with certain arguments:
     * the label is the worst case, not the common case.
     */
    case Destructive = 'destructive';
}
