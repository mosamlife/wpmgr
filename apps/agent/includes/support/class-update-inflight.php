<?php
/**
 * UpdateInFlight: out-of-band reconcile for the ONE update-guard failure mode
 * register_shutdown_function() cannot catch — a PHP-FPM
 * `request_terminate_timeout` SIGTERM (GitHub issue #131 adversarial review, S4).
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/*
 * Background: UpdateGuard's shutdown-time restore only works because PHP runs
 * registered shutdown functions when a request ends — including on a fatal
 * error such as hitting `max_execution_time`. It does NOT run them when
 * PHP-FPM's own `request_terminate_timeout` fires: FPM resets the process's
 * timeout handler to SIG_DFL and SIGTERMs the worker immediately, tearing the
 * whole PHP process down without unwinding anything — no try/catch/finally,
 * no shutdown functions, nothing. If that kill lands mid-copy during a
 * plugin/theme apply, the site is left with a half-written directory AND no
 * in-process code ever runs again to fix it.
 *
 * This class is the out-of-band recovery path for exactly that case, mirroring
 * WPMgr\Agent\Support\Maintenance's "persist a marker before risky work, heal
 * a STALE one before starting new work" shape:
 *   - mark() persists a small JSON marker {type, slug, snapshot_id, ts} to a
 *     RANDOMLY named file (F5, final-hardening review — see keyFor()'s doc)
 *     BEFORE UpdateCommand calls UpdateRunner::apply() — i.e. before the
 *     window a hard kill could land in — and returns an exclusive, held
 *     filesystem lock (B, same review) the caller must keep open for the
 *     apply's duration.
 *   - clear() removes it once the item's fate is resolved SYNCHRONOUSLY
 *     within the same request (success, a synchronous incomplete-restore, or
 *     a caught throw) — called from UpdateCommand::processItem()'s existing
 *     `finally`, so it runs on every path that finishes normally. ALSO called
 *     immediately from UpdateGuard::markClean() (F2, same review) the instant
 *     an apply is confirmed good, closing the narrow window between that
 *     confirmation and the `finally`.
 *   - healStaleIfPresent() is the reconcile: called both at the START of the
 *     next `update` command (mirrors Maintenance::healStaleIfPresent()) and
 *     from a recurring cron sweep (HOOK_GC, mirrors the `wpmgr_restore_oldfiles_gc`
 *     pattern), so recovery does not depend on the control plane ever sending
 *     another `update` request for the SAME site. Two layers gate an actual
 *     restore (F2 + B, final-hardening review):
 *       1. B — the marker's paired lock file must be acquirable right now
 *          (non-blocking). If it is still held, the apply that owns this
 *          marker is GENUINELY still running (set_time_limit() bounds CPU
 *          time, not wall time, so a slow I/O-bound apply can legitimately
 *          outlive the age heuristic below) — skip it entirely.
 *       2. F2 — once the lock is free, verify the live directory is not
 *          ALREADY complete/healthy (a marker can survive a SUCCESSFUL apply
 *          if the kill landed in the old markClean()-to-`finally` window;
 *          that window is now closed at the source, but this stays as
 *          defense in depth) and that the referenced snapshot still exists,
 *          before ever calling restore(). Only a genuinely incomplete live
 *          directory with a still-present snapshot triggers a restore.
 *     A marker only ever survives to be found here when the request that
 *     wrote it never reached its own `finally` — i.e. it really was torn
 *     down mid-flight.
 *
 * Staleness threshold reasoning lives on STALE_AFTER_SECONDS below, not here.
 *
 * Multi-node assumption (doc-only, issue #131 final-review follow-up) — the
 * flock() liveness signal this class's B fix relies on (see acquireLock()'s
 * doc) is a SINGLE-NODE primitive: it is coherent only for processes sharing
 * the same kernel file-lock table. On a multi-node WordPress install where
 * `wp-content/uploads` (and therefore this class's marker/lock store, see
 * PURPOSE below) is mounted over shared network storage (NFS/EFS/etc.),
 * flock() is enforced PER NODE, not across the fleet — a lock a process on
 * node A holds is invisible to a process on node B attempting
 * acquireLock() for the same type/slug, so the "is the owning process still
 * alive" check can spuriously report "free" for a marker whose owning apply
 * is still genuinely running on a different node. This is a KNOWN,
 * ACCEPTED limitation, not an oversight:
 *   - The STALE_AFTER_SECONDS age gate (1200s) is the fallback in that
 *     scenario — healStaleIfPresent() never even attempts the lock probe
 *     for a marker younger than that threshold, so a cross-node false
 *     "free" reading can only ever matter for a marker that has ALSO
 *     already sat for 20 minutes, which is itself far longer than any
 *     normal apply takes on any single node.
 *   - The scenario this whole class exists for — a CONCURRENT apply of the
 *     SAME plugin/theme racing against a reconcile that wrongly believes
 *     the owning process is dead — is independently prevented upstream, at
 *     the control plane, via cross-run dedup for a given site (so the CP
 *     itself never fans the same update out to multiple concurrent runs in
 *     the first place); this class's flock() check is defense-in-depth for
 *     the single-node case, not the sole guard against that race.
 *   - Net effect on shared-storage multi-node installs: reconcile degrades
 *     gracefully to the age-only heuristic this class used BEFORE the B fix
 *     — still correct, just without the tighter single-node liveness
 *     precision flock() adds on top of it.
 */

/**
 * Persists and reconciles a marker for an in-progress plugin/theme apply so a
 * hard-killed request that UpdateGuard's shutdown hook could never see is
 * still recovered — just later, out of band, rather than instantly in-process.
 */
final class UpdateInFlight
{
    /** Recurring cron hook name for the reconcile sweep. */
    public const HOOK_GC = 'wpmgr_update_inflight_gc';

    /** StoragePaths purpose slug — resolves to uploads/wpmgr-update-inflight/. */
    private const PURPOSE = 'update-inflight';

    /**
     * A marker older than this is treated as orphaned (the request that
     * wrote it was torn down without reaching its own cleanup) rather than
     * possibly belonging to a still-running apply. UpdateCommand::execute()
     * bounds an apply to a 900s (15 min) `set_time_limit()` specifically so
     * a hung apply still hits PHP's own recoverable fatal well before this
     * threshold; 1200s (20 min) leaves 5 minutes of margin beyond that bound
     * for the in-process shutdown-guard path to have already fired and
     * cleared the marker, so a legitimately (if unusually slowly) still-
     * running apply is never reconciled out from under itself. In absolute
     * terms this is still a short window: a truly hard-killed site self-heals
     * on its own within about 20 minutes without requiring an operator or a
     * new control-plane request to notice.
     *
     * B (issue #131 final-hardening review) — this is now only a SECONDARY
     * backstop: set_time_limit() bounds CPU time, not wall time, so a slow
     * I/O-bound apply CAN legitimately exceed this age while still running.
     * The flock-based liveness check in healStaleIfPresent() is the PRIMARY
     * signal (see acquireLock()'s doc); this age check is kept as a cheap
     * first filter so the (slightly more expensive) lock probe only runs
     * against markers that are already old enough to be suspicious.
     */
    private const STALE_AFTER_SECONDS = 1200;

    /**
     * Persist an in-flight marker for a plugin/theme apply that is about to
     * start, and acquire the paired liveness lock for it. Must be called
     * AFTER a snapshot has been successfully captured (there is nothing to
     * reconcile TO otherwise) and BEFORE UpdateRunner::apply() is invoked.
     * Best-effort: a failure to write the marker (or acquire the lock) must
     * never block the apply itself — the shutdown-guard in-process path
     * (UpdateGuard) still covers the recoverable-kill case regardless.
     *
     * B (issue #131 final-hardening review) — the CALLER MUST keep the
     * returned lock handle open for the entire duration of the apply this
     * marker covers, and release it (via release()) in the SAME `finally`
     * that already calls clear() once the item's fate is resolved. The OS
     * releases the lock automatically the instant this PHP process exits for
     * ANY reason, including a SIGKILL/SIGTERM that skips every PHP-level
     * cleanup — that is precisely what makes "can healStaleIfPresent()
     * re-acquire this lock" an accurate, real-time liveness signal for
     * "is the process that wrote this marker still alive right now",
     * independent of how long the apply has actually been running.
     *
     * @param string $type       'plugin'|'theme'.
     * @param string $slug       Sanitized slug.
     * @param string $snapshotId Snapshot identifier captured before the apply.
     * @return resource|null An open, held lock handle the caller must pass to
     *     release(), or null when the marker/lock could not be set up.
     */
    public static function mark(string $type, string $slug, string $snapshotId)
    {
        $dir = StoragePaths::ensureHardened(self::PURPOSE);
        if ($dir === '') {
            return null;
        }

        // F5 (issue #131 final-hardening review) — remove any marker(s)
        // already on disk for this exact type/slug before writing a fresh
        // one. This preserves the old deterministic-path implementation's
        // implicit "one marker per slug, latest wins" retry semantics now
        // that the filename itself is randomized (see keyFor()'s doc) rather
        // than derived from type/slug, so there is no longer a single fixed
        // path a repeat mark() call would simply overwrite in place.
        self::forgetMarkersFor($dir, $type, $slug);

        $file = $dir . '/' . bin2hex(random_bytes(16)) . '.json';

        $payload = [
            'type'        => $type,
            'slug'        => $slug,
            'snapshot_id' => $snapshotId,
            'ts'          => time(),
        ];

        // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_file_put_contents -- headless agent; WP_Filesystem never initialized; direct write of a small, server-derived JSON marker
        @file_put_contents($file, (string) wp_json_encode($payload), LOCK_EX);
        // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms (0600); WP_Filesystem would coerce to wider FS_CHMOD_FILE
        @chmod($file, 0600);

        return self::acquireLock($type, $slug);
    }

    /**
     * Clear this item's in-flight marker(s). Safe to call unconditionally —
     * including when mark() was never called for this type/slug (e.g. a
     * snapshot was never captured) — since it is a no-op when no marker
     * matches. Intended to be called from a `finally` block wrapping the
     * same apply lifecycle mark() is called around (every SYNCHRONOUS
     * outcome — success, a synchronous incomplete-restore, or a caught throw
     * — removes the marker), AND from UpdateGuard::markClean() the instant
     * an apply is confirmed good (F2, issue #131 final-hardening review) —
     * see that method's doc for the success-window it closes. A marker
     * survives past both of those call sites ONLY when the request never
     * reached either one at all.
     *
     * @param string $type 'plugin'|'theme'.
     * @param string $slug Sanitized slug.
     * @return void
     */
    public static function clear(string $type, string $slug): void
    {
        $dir = StoragePaths::dataBase(self::PURPOSE);
        if ($dir === '' || !is_dir($dir)) {
            return;
        }
        self::forgetMarkersFor($dir, $type, $slug);
    }

    /**
     * Release a lock handle returned by mark()/acquireLock(). Safe to call
     * with null (a no-op) so callers that never successfully acquired one
     * need no extra branching. Intended to be called from the SAME `finally`
     * that calls clear().
     *
     * @param resource|null $handle Handle returned by mark(), or null.
     * @return void
     */
    public static function release($handle): void
    {
        if ($handle === null || !is_resource($handle)) {
            return;
        }
        flock($handle, LOCK_UN);
        fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- releasing our own advisory lock handle on a small internal coordination file; WP_Filesystem has no flock()-equivalent API and is never initialized in this headless agent context
    }

    /**
     * Reconcile any STALE in-flight marker: restore its recorded snapshot
     * and remove the marker — but ONLY once verified (F2 + B, issue #131
     * final-hardening review; see this class's doc for the two-layer gate).
     * A marker younger than STALE_AFTER_SECONDS is skipped outright (cheap
     * first filter); a marker whose liveness lock cannot be acquired is
     * skipped (its owning apply is still genuinely running); a marker over a
     * live directory that is ALREADY complete/healthy is cleared WITHOUT a
     * restore; a marker whose referenced snapshot no longer exists is
     * cleared WITHOUT attempting a doomed restore. Safe to call from both the
     * start of a fresh `update` command (the "next agent request" path) and
     * a recurring cron sweep (the "cron sweep" path, gcSweep() below) — both
     * share this exact method so a test can exercise it directly with
     * injected SnapshotManager/UpdateRunner doubles.
     *
     * @param SnapshotManager $snapshots Snapshot store used to perform the restore.
     * @param UpdateRunner|null $runner  Used for the F2 live-directory health
     *                                   check; defaults to a real instance.
     * @return void
     */
    public static function healStaleIfPresent(SnapshotManager $snapshots, ?UpdateRunner $runner = null): void
    {
        $dir = StoragePaths::dataBase(self::PURPOSE);
        if ($dir === '' || !is_dir($dir)) {
            return;
        }

        $runner = $runner ?? new UpdateRunner();
        $now    = time();

        foreach (self::markerFiles($dir) as $file) {
            $marker = self::readMarker($file);
            if ($marker === null) {
                // Unreadable/corrupt marker — cannot reconcile it to
                // anything meaningful; remove it rather than leave a dead
                // file behind forever.
                wp_delete_file($file);
                continue;
            }

            $ts = isset($marker['ts']) && is_numeric($marker['ts']) ? (int) $marker['ts'] : 0;
            if ($ts === 0 || ($now - $ts) < self::STALE_AFTER_SECONDS) {
                // Fresh (or unreadable timestamp, treated as fresh to be
                // safe) — may still belong to a legitimately in-flight apply.
                continue;
            }

            $type       = isset($marker['type']) && is_string($marker['type']) ? $marker['type'] : '';
            $slug       = isset($marker['slug']) && is_string($marker['slug']) ? $marker['slug'] : '';
            $snapshotId = isset($marker['snapshot_id']) && is_string($marker['snapshot_id']) ? $marker['snapshot_id'] : '';

            if ($type === '' || $slug === '' || $snapshotId === '') {
                wp_delete_file($file);
                continue;
            }

            // B (issue #131 final-hardening review) — flock is the PRIMARY
            // liveness signal; the age check above is only a cheap secondary
            // filter. If this marker's lock cannot be acquired right now,
            // its owning process is genuinely still alive — skip this marker
            // entirely, without touching the marker file OR the snapshot,
            // rather than reconcile a live apply out from under itself.
            $lock = self::acquireLock($type, $slug);
            if ($lock === null) {
                continue;
            }

            try {
                // F2 (issue #131 final-hardening review) — verify before
                // restoring. Blindly restoring on every stale marker (the
                // original behavior) could revert a perfectly good,
                // already-finished update: a marker can legitimately survive
                // to here if a kill landed in the narrow window between
                // UpdateGuard::markClean() and UpdateCommand's own `finally`
                // (now closed at the source — see markClean()'s own fix —
                // this stays as defense in depth for any other path that
                // could leave a marker behind a healthy live directory).
                $healthy = false;
                try {
                    $healthy = $runner->isComplete($type, $slug);
                } catch (\Throwable $verifyError) {
                    // Inconclusive: unlike UpdateCommand's OWN synchronous S7
                    // case (where $applied['ok'] is already known true), here
                    // NOTHING is known about whether the apply that owned
                    // this marker ever finished — fall through to a restore,
                    // the same conservative default this method used before
                    // F2 existed.
                    DebugLog::write(
                        'WPMgr Agent: stale-marker health verification threw for '
                        . $type . ':' . $slug . ': ' . $verifyError->getMessage()
                        . '; treating as unverified and proceeding to restore.'
                    );
                }

                if ($healthy) {
                    DebugLog::write(
                        'WPMgr Agent: stale update-in-flight marker for ' . $type . ':' . $slug
                        . ' (age ' . ($now - $ts) . 's) found the live directory already complete/healthy — '
                        . 'the apply finished before the kill landed; clearing the marker without restoring.'
                    );
                    wp_delete_file($file);
                    continue;
                }

                if (!$snapshots->snapshotExists($snapshotId)) {
                    DebugLog::write(
                        'WPMgr Agent: stale update-in-flight marker for ' . $type . ':' . $slug
                        . ' references snapshot ' . $snapshotId . ' which no longer exists (already pruned or '
                        . 'consumed) — clearing the marker without attempting a partial restore.'
                    );
                    wp_delete_file($file);
                    continue;
                }

                DebugLog::write(
                    'WPMgr Agent: reconciling stale update-in-flight marker for ' . $type . ':' . $slug
                    . ' (age ' . ($now - $ts) . 's) — the apply that wrote it never reached its own cleanup, '
                    . 'live directory verified incomplete; auto-restoring pre-update snapshot ' . $snapshotId . '.'
                );
                try {
                    $snapshots->restore($type, $slug, $snapshotId);
                } catch (\Throwable $e) {
                    DebugLog::write(
                        'WPMgr Agent: stale-marker auto-restore threw for ' . $type . ':' . $slug
                        . ': ' . $e->getMessage()
                    );
                }

                wp_delete_file($file);
            } finally {
                self::release($lock);
            }
        }
    }

    /**
     * Cron-hook entry point (arg-less, unlike healStaleIfPresent() which
     * takes injectable dependencies for testability). Bound to HOOK_GC.
     *
     * @return void
     */
    public static function gcSweep(): void
    {
        self::healStaleIfPresent(new SnapshotManager());
    }

    /**
     * Schedule the recurring reconcile sweep (gcSweep()). Safe to call on
     * every activation / reschedule pass — a no-op when already scheduled.
     * Mirrors EmailLogger::schedule_prune()'s idempotent-schedule shape.
     *
     * @param int $now Current time.
     * @return void
     */
    public static function scheduleGc(int $now): void
    {
        if (!function_exists('wp_next_scheduled') || !function_exists('wp_schedule_event')) {
            return;
        }
        if (wp_next_scheduled(self::HOOK_GC) !== false) {
            return;
        }
        wp_schedule_event($now + 300, 'hourly', self::HOOK_GC);
    }

    // ---------------------------------------------------------------------
    // B (issue #131 final-hardening review) — flock-based liveness lock
    // ---------------------------------------------------------------------

    /**
     * Acquire, non-blocking, the exclusive lock that marks a type/slug's
     * apply as genuinely "in progress" right now. See mark()'s and this
     * class's doc for the full rationale (flock is immune to the
     * set_time_limit()-bounds-CPU-not-wall-time gap an age heuristic alone
     * cannot close).
     *
     * Single-node assumption — this flock() is only lock-coherent for
     * processes on the SAME node sharing the same kernel file-lock table. On
     * a multi-node install with `wp-content/uploads` on shared NFS/EFS
     * storage, a lock held by a process on one node is invisible to
     * acquireLock() called from another node. This is intentional, accepted
     * best-effort — see this class's top-of-file doc ("Multi-node
     * assumption") for the full reasoning and the STALE_AFTER_SECONDS
     * age-gate fallback that keeps reconcile safe (if less precise) in that
     * topology.
     *
     * The lock filename is DELIBERATELY still derived deterministically from
     * type/slug (keyFor()) — unlike the JSON marker filename (F5) — because
     * it carries no sensitive payload at all (an empty coordination file),
     * and healStaleIfPresent() needs to compute the SAME path mark() used
     * from only the type/slug it just decoded out of a marker's (randomly
     * named) JSON content, without that content needing to also carry a lock
     * file path.
     *
     * @param string $type 'plugin'|'theme'.
     * @param string $slug Sanitized slug.
     * @return resource|null An open, LOCKED file handle the caller must
     *     release via release(), or null when the lock could not be acquired
     *     (already held elsewhere) or the lock file itself could not be
     *     opened (best-effort — never blocks the caller's own work).
     */
    private static function acquireLock(string $type, string $slug)
    {
        $dir = StoragePaths::ensureHardened(self::PURPOSE);
        if ($dir === '') {
            return null;
        }
        $file = $dir . '/' . self::keyFor($type, $slug) . '.lock';

        // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fopen -- headless agent; WP_Filesystem has no flock()-equivalent API and is never initialized in this context; a small, server-derived, empty coordination file
        $handle = @fopen($file, 'c');
        if ($handle === false) {
            return null;
        }
        @chmod($file, 0600); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_chmod -- explicit security perms on our own coordination file; WP_Filesystem would coerce to wider FS_CHMOD_FILE

        if (!flock($handle, LOCK_EX | LOCK_NB)) {
            fclose($handle); // phpcs:ignore WordPress.WP.AlternativeFunctions.file_system_operations_fclose -- releasing our own failed-lock-attempt handle, not a WP_Filesystem-managed resource
            return null;
        }

        return $handle;
    }

    // ---------------------------------------------------------------------
    // Marker storage
    // ---------------------------------------------------------------------

    /**
     * Remove any marker file(s) matching this exact type/slug. Shared by
     * mark() (purging a stale duplicate before writing a fresh one — the
     * retry-overwrite semantics the old deterministic filename gave for
     * free) and clear() (removing this item's marker once its fate is
     * resolved).
     *
     * @param string $dir  Marker store directory.
     * @param string $type 'plugin'|'theme'.
     * @param string $slug Sanitized slug.
     * @return void
     */
    private static function forgetMarkersFor(string $dir, string $type, string $slug): void
    {
        foreach (self::markerFiles($dir) as $file) {
            $marker = self::readMarker($file);
            if ($marker === null) {
                // Leave a corrupt file for healStaleIfPresent() to clean up
                // — it cannot be matched to this type/slug at all.
                continue;
            }
            if (($marker['type'] ?? '') === $type && ($marker['slug'] ?? '') === $slug) {
                wp_delete_file($file);
            }
        }
    }

    /**
     * List every marker (`*.json`) file currently in the store directory.
     *
     * @param string $dir Marker store directory.
     * @return list<string> Absolute file paths.
     */
    private static function markerFiles(string $dir): array
    {
        $items = @scandir($dir);
        if (!is_array($items)) {
            return [];
        }

        $out = [];
        foreach ($items as $item) {
            if (str_ends_with($item, '.json')) {
                $out[] = $dir . '/' . $item;
            }
        }

        return $out;
    }

    /**
     * Read and decode a single marker file.
     *
     * @param string $file Absolute marker file path.
     * @return array<string,mixed>|null Decoded marker, or null when
     *     unreadable/not a file/invalid JSON.
     */
    private static function readMarker(string $file): ?array
    {
        if (!is_file($file)) {
            return null;
        }
        $raw = @file_get_contents($file);
        if (!is_string($raw) || $raw === '') {
            return null;
        }
        $marker = json_decode($raw, true);

        return is_array($marker) ? $marker : null;
    }

    /**
     * Derive a filesystem-safe, collision-resistant key for a type/slug pair,
     * used ONLY for the lock file (never for the JSON marker — see F5 below).
     *
     * F5 (issue #131 final-hardening review) — the JSON marker filename is
     * DELIBERATELY NOT derived from this key (or from type/slug at all)
     * anymore. A deterministic sha256(type:slug) hash has no secret salt, so
     * it is trivially recomputable by anyone who already knows a plugin or
     * theme's PUBLIC slug; if this store directory were ever reachable over
     * HTTP despite its .htaccess/index.php hardening (e.g. an nginx config
     * that does not honor .htaccess at all), that meant an attacker could
     * locate and read a marker's JSON body during the in-flight window and
     * recover its `snapshot_id` — effectively a capability token for the
     * pre-update snapshot payload's own storage path, a source-disclosure
     * risk for a premium plugin/theme. mark() now writes the JSON marker to
     * a RANDOM filename instead (see mark()'s own comment); healStaleIfPresent()
     * already scans the whole directory rather than looking up a computed
     * path, so nothing there needed to change to support this.
     *
     * The lock file is unaffected by this concern (it carries no payload at
     * all — see acquireLock()'s doc for why it stays deterministic).
     *
     * @param string $type 'plugin'|'theme'.
     * @param string $slug Sanitized slug.
     * @return string 32 hex characters.
     */
    private static function keyFor(string $type, string $slug): string
    {
        return substr(hash('sha256', $type . ':' . $slug), 0, 32);
    }
}
