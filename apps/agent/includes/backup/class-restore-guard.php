<?php
/**
 * RestoreGuard: GH #146 — per-run shutdown-time backstop that performs the
 * combined files+DB rollback if the PHP process driving a restore is hard-
 * killed somewhere in the window between the destructive swap(s) completing
 * and the post-restore health-check verdict being reached.
 *
 * Structurally a direct mirror of `WPMgr\Agent\Support\UpdateGuard` (see
 * that class's doc for the full "why a shutdown-function object, not a bare
 * closure" rationale) — two bool flags (`$clean`/`$fired`),
 * `register_shutdown_function([$this,'fire'])`, idempotent, never lets a
 * `\Throwable` escape `fire()`. The one structural difference: `fire()`
 * invokes the supplied rollback callable (RestoreRunner's combined
 * files+DB rollback) instead of `SnapshotManager::restore()`.
 *
 * V1 scope — deliberately narrow: a per-request, in-memory object. It
 * protects the crash window of ONE `RestoreRunner::run()` invocation, from
 * wherever `RestoreRunner` arms it (right before the first destructive
 * rename) through `markClean()`. Full design notes (V1 scope limits,
 * markClean() timing across GH #146's two health-check gates, and how a
 * guard-fired rollback stays visitor-safe) live at the bottom of this file.
 *
 * @package WPMgr\Agent\Backup
 */

declare(strict_types=1);

namespace WPMgr\Agent\Backup;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/**
 * Fires a combined files+DB rollback on PHP shutdown unless the restore run
 * this guard covers was confirmed healthy first.
 */
final class RestoreGuard
{
    /** @var callable():array{files:bool,db:bool,reason?:string} */
    private $rollback;

    /**
     * Set once the restore this guard covers has been verified healthy.
     * While true, fire() is permanently a no-op — a confirmed-good restore
     * must never be rolled back retroactively by a later, unrelated
     * shutdown.
     */
    private bool $clean = false;

    /**
     * Set the first time fire() actually performs (or attempts) a
     * rollback. Makes fire() idempotent — a synchronous call from
     * RestoreRunner's own health-check failure handling and a subsequent
     * real-shutdown invocation of the same guard must never roll back
     * twice.
     */
    private bool $fired = false;

    /**
     * @param callable():array{files:bool,db:bool,reason?:string} $rollback
     *        Performs the combined files+DB rollback and reports which legs
     *        completed (an optional `reason` explains a legs-skipped
     *        outcome, e.g. `db_rollback_unavailable`). MUST NEVER throw — a
     *        shutdown-function context has nowhere useful to propagate an
     *        exception to; the caller's implementation catches internally.
     */
    public function __construct(callable $rollback)
    {
        $this->rollback = $rollback;
    }

    /**
     * Register this guard's fire() as a PHP shutdown callback. Safe to
     * call more than once — PHP tolerates duplicate shutdown callbacks and
     * a redundant fire() is a cheap, idempotent no-op.
     */
    public function arm(): void
    {
        register_shutdown_function([$this, 'fire']);
    }

    /**
     * Mark the restore this guard covers as verified-healthy. MUST be
     * called before the LAST health-check gate returns a passing verdict —
     * GH #146's two-gate restructure means that's `health_check_live`
     * (Gate 2, the loopback WSOD probe on the real post-maintenance site),
     * NOT `health_check` (Gate 1, the in-process DB probe) — an armed guard
     * that never learns the restore was healthy would otherwise undo it if
     * the request is torn down by something unrelated later in the same
     * shutdown chain.
     */
    public function markClean(): void
    {
        $this->clean = true;
    }

    /**
     * Perform the rollback unless the restore was already confirmed clean,
     * or this guard has already fired once. Safe to call from a real PHP
     * shutdown handler (return value unused there) or synchronously from
     * RestoreRunner's own health-check failure handling.
     *
     * @return array{fired:bool,files:bool,db:bool,reason:string} fired=false
     *     means this call was a no-op (already clean or already fired);
     *     files/db/reason only mean something when fired=true.
     */
    public function fire(): array
    {
        if ($this->clean || $this->fired) {
            return ['fired' => false, 'files' => false, 'db' => false, 'reason' => ''];
        }
        $this->fired = true;

        try {
            $result = ($this->rollback)();
        } catch (\Throwable $e) {
            \WPMgr\Agent\Support\DebugLog::write(
                'WPMgr Agent: RestoreGuard rollback threw: ' . $e->getMessage()
            );
            return ['fired' => true, 'files' => false, 'db' => false, 'reason' => 'rollback_threw'];
        }

        return [
            'fired'  => true,
            'files'  => (bool) ($result['files'] ?? false),
            'db'     => (bool) ($result['db'] ?? false),
            'reason' => isset($result['reason']) && is_string($result['reason']) ? $result['reason'] : '',
        ];
    }
}

/*
 * Design notes (referenced from the class docblock above; kept down here so
 * the ABSPATH direct-access guard stays within the first ~50 lines of the
 * file, which is what Plugin Check's Direct_File_Access_Check regex
 * fallback scans).
 *
 * V1 scope limits: a hard kill landing BEFORE this guard is armed, or in a
 * re-entry that resumes mid-flight without ever passing through the swap
 * phases again in the SAME request, is NOT covered here — recovery for
 * those falls back to RestoreWatchdog's existing stall-detection + resume
 * path. A full out-of-band RestoreInFlight reconcile sweep (mirroring
 * UpdateInFlight, which closes the equivalent gap for update-apply) would
 * close that remaining window too, but is an explicit FAST-FOLLOW, not
 * built here.
 *
 * markClean() timing across GH #146's two health-check gates: the guard
 * stays armed across BOTH health_check (Gate 1, in-process DB probe, still
 * under maintenance) and health_check_live (Gate 2, the loopback WSOD probe
 * against the real post-maintenance site) — only Gate 2 passing calls
 * markClean(), since Gate 2 is the last checkpoint before completed.
 *
 * Visitor-visibility during a guard-fired rollback: RestoreRunner::
 * performRollback() (the rollback callable this guard invokes) wraps its
 * OWN actual revert in maintenanceOn()/finally maintenanceOff()
 * unconditionally — so a rollback fired by this guard, whether via a
 * synchronous call from routeRollback() during either gate's own failure
 * handling, or asynchronously via a real PHP shutdown, is uniformly
 * protected, with no special-casing needed in this class.
 */
