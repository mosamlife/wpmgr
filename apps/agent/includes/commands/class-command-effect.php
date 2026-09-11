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
 *   2. Does the command EXIST IN ORDER TO change the site?  -> Write
 *   3. Otherwise -- it exists in order to report            -> Read
 *
 * Rule 2 is a test of PURPOSE, not of whether the state involved is
 * rebuildable. An earlier revision of this file drew the line at derived state
 * instead, saying that caches and transients "do not count" -- and then
 * labelled cache_purge, cache_preload and objectcache.flush as Writes, which
 * that rule cannot produce. Both could not be right. Purpose is the one that
 * predicts the labels actually on the commands, and the one a consumer can
 * apply without reading an implementation:
 *
 *   cache_purge empties a cache the site rebuilds by itself, and is a Write --
 *   changing what every visitor is served next is the whole reason it exists.
 *
 *   diagnostics memoizes a computed value in a transient and nudges WP-Cron,
 *   and is a Read -- it exists to answer a question, and that bookkeeping is
 *   done in order to answer it.
 *
 * So the question is never "did any byte change anywhere", which is almost
 * always yes. It is "is changing the site what this command is for".
 *
 * Where a command's effect depends on its arguments — the action-dispatch
 * commands such as db_snapshot, db_table_action and media_clean — it declares
 * its WORST case, and says so in the comment on effect(). A caller must be able
 * to trust the label without also parsing the parameters.
 */
enum CommandEffect: string
{
    /**
     * Exists in order to report. Changing the site is not what it is for.
     *
     * A Read may still touch state in the course of answering: memoize a
     * computed value in a transient, nudge WP-Cron, write and immediately
     * remove a probe key or a temp file. That is bookkeeping performed in order
     * to answer, not the point of the call.
     *
     * A Read may also send what it read to the control plane --
     * file_download_prepare streams file bytes to CP-minted presigned URLs and
     * is still a Read, because the SITE is unchanged. Data leaving the site is a
     * confidentiality question on a different axis, which this enum deliberately
     * does not record. If a read-only connection is also meant to mean "no
     * exfiltration", that needs its own axis rather than a re-reading of this
     * one.
     */
    case Read = 'read';

    /**
     * Exists in order to change the site, and nothing is lost by it.
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
