<?php
/**
 * UpdateGuard: per-item shutdown-time backstop that restores a plugin/theme
 * directory from its pre-update snapshot when an agent-driven update apply is
 * interrupted by a hard resource kill.
 *
 * Background (GitHub issue #131): UpdateCommand applies plugin/theme updates
 * via WordPress core's Plugin_Upgrader / Theme_Upgrader INLINE in the REST
 * request — no WP-CLI subprocess, no cron handoff. Their underlying
 * install_package() -> copy_dir() is a NON-atomic recursive copy that clears
 * the destination directory FIRST, then copies the new files in. When
 * something kills the PHP process mid-copy — max_execution_time or a
 * PHP-FPM request_terminate_timeout are the most likely causes — the old
 * directory is already gone and a half-written tree is left behind (e.g. a
 * new vendor/vendor-prefixed/autoload.php present but composer/autoload_real.php
 * missing), which fatal-errors the site the next time WordPress loads it.
 *
 * A hard resource kill of this kind does not throw a catchable \Throwable and
 * does not unwind any try/catch/finally — it tears the whole request down.
 * The ONLY code that still runs afterwards is a PHP shutdown function. This
 * class is that shutdown callback, structured as a small stateful object
 * (rather than a bare closure) so:
 *   - UpdateCommand can synchronously call fire() itself from its own
 *     try/catch when it detects a failure by normal means (an ok:false
 *     return, a thrown exception, or a failed completeness check) — the
 *     restore happens immediately instead of waiting on the request to tear
 *     down, and fire() is idempotent so a later real shutdown invocation is
 *     always safe to also occur.
 *   - A test can construct one directly and call fire()/markClean() without
 *     needing to trigger an actual PHP process shutdown.
 *
 * Mirrors WPMgr\Agent\Support\Maintenance::armShutdownGuard's "arm once, heal
 * unconditionally unless told otherwise" shape, but restores a directory
 * instead of clearing a drop-file, and uses an explicit markClean() rather
 * than an age-based staleness heuristic — an update apply either was, or was
 * not, verified good by the time processItem() finishes, so there is nothing
 * to infer from elapsed time.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Restores a single plugin/theme directory from its pre-update snapshot
 * unless the apply that owns this guard was confirmed good.
 */
final class UpdateGuard
{
    private SnapshotManager $snapshots;

    private string $type;

    private string $slug;

    private string $snapshotId;

    /**
     * Set once the apply this guard covers has been verified good. While
     * true, fire() is permanently a no-op — a completed, healthy update must
     * never be rolled back retroactively by a later, unrelated shutdown.
     */
    private bool $clean = false;

    /**
     * Set the first time fire() actually performs (or attempts) a restore.
     * Makes fire() idempotent: a synchronous call from UpdateCommand's own
     * failure handling and a subsequent real-shutdown invocation of the same
     * guard must never restore twice.
     */
    private bool $fired = false;

    /**
     * @param SnapshotManager $snapshots  Snapshot store used to perform the restore.
     * @param string          $type       'plugin'|'theme' (never 'core' — see class doc / D3 in issue #131).
     * @param string          $slug       Sanitized slug (already validated by UpdateCommand).
     * @param string          $snapshotId Snapshot identifier captured before the apply.
     */
    public function __construct(SnapshotManager $snapshots, string $type, string $slug, string $snapshotId)
    {
        $this->snapshots  = $snapshots;
        $this->type       = $type;
        $this->slug       = $slug;
        $this->snapshotId = $snapshotId;
    }

    /**
     * Register this guard's fire() as a PHP shutdown callback. Safe to call
     * more than once — PHP tolerates duplicate shutdown callbacks and a
     * redundant fire() is a cheap, idempotent no-op.
     *
     * @return void
     */
    public function arm(): void
    {
        register_shutdown_function([$this, 'fire']);
    }

    /**
     * Mark the apply this guard covers as verified-good. MUST be called
     * before returning a 'succeeded' (or 'up_to_date') result — an armed
     * guard that never learns the apply was good would otherwise undo it if
     * the request is torn down by something unrelated later in the same
     * shutdown chain.
     *
     * F2 (issue #131 final-hardening review) — ALSO clears this item's
     * UpdateInFlight out-of-band reconcile marker immediately, rather than
     * leaving that to UpdateCommand::processItem()'s `finally` alone. A
     * marker written before apply() previously stayed on disk all the way
     * to that `finally` even for a SUCCESSFUL apply; a hard kill landing in
     * that narrow window (e.g. the exact PHP-FPM `request_terminate_timeout`
     * case UpdateInFlight exists to cover) left a marker pointing at a now
     * genuinely stale pre-update snapshot for an update that had already
     * finished cleanly — a stale-but-healthy marker healStaleIfPresent()'s
     * own F2 verify-before-restore fix already tolerates safely, but closing
     * the window at the source here means it's never even created for the
     * common case.
     *
     * @return void
     */
    public function markClean(): void
    {
        $this->clean = true;
        UpdateInFlight::clear($this->type, $this->slug);
    }

    /**
     * GitHub issue #328 - disarm this guard because THERE IS NOTHING TO ROLL
     * BACK, which is a different fact from markClean()'s "the apply was
     * verified good".
     *
     * The one caller is UpdateCommand's restore-skip decision: an update that
     * failed BEFORE WordPress ever touched the destination (UpdateOutcome said
     * so from the error code) AND whose destination was then positively
     * re-read as unchanged (DestinationVerifier said so from the disk). The
     * shutdown backstop must not fire for such an item, because firing it would
     * perform a non-atomic restore over a directory nobody modified.
     *
     * Deliberately a DISTINCT NAME rather than an extra call to markClean():
     * an operator reading a debug log must be able to tell "we kept a good
     * update" apart from "we declined to undo a failure that changed nothing".
     * The side effects are identical on purpose (clear the in-flight marker so
     * the out-of-band reconcile does not later restore what this decision just
     * declined to restore), so any future change to one must consider the other.
     *
     * @param string $reason Short, non-secret explanation for the debug log.
     * @return void
     */
    public function standDown(string $reason): void
    {
        $this->clean = true;
        UpdateInFlight::clear($this->type, $this->slug);
        DebugLog::write(
            'WPMgr Agent: update guard stood down for ' . $this->type . ':' . $this->slug
            . ' (no restore performed): ' . $reason
        );
    }

    /**
     * Restore the snapshot unless the apply was already confirmed clean, or
     * this guard has already fired once. Safe to call from a real PHP
     * shutdown handler (return value is simply unused there) or
     * synchronously from UpdateCommand's own failure handling.
     *
     * Never lets a \Throwable escape: a shutdown-function context has
     * nowhere useful to propagate one to, and a synchronous caller is
     * already inside its own failure path.
     *
     * @return array{fired:bool,ok:bool,log:string} fired=false means this
     *     call was a no-op (already clean or already fired); ok/log only
     *     mean something when fired=true.
     */
    public function fire(): array
    {
        if ($this->clean || $this->fired) {
            return ['fired' => false, 'ok' => true, 'log' => ''];
        }
        $this->fired = true;

        try {
            $result = $this->snapshots->restore($this->type, $this->slug, $this->snapshotId);
        } catch (\Throwable $e) {
            DebugLog::write(
                'WPMgr Agent: update auto-restore threw for ' . $this->type . ':' . $this->slug
                . ': ' . $e->getMessage()
            );

            return ['fired' => true, 'ok' => false, 'log' => 'Auto-restore error.'];
        }

        return ['fired' => true, 'ok' => (bool) $result['ok'], 'log' => (string) $result['log']];
    }
}
