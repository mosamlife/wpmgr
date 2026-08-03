<?php
/**
 * SiteUpdateLock: ONE update writer per site at a time (GitHub issue #328).
 *
 * Backed by WordPress core's OWN cross-request upgrader lock
 * (WP_Upgrader::create_lock / release_lock), the same primitive the agent's
 * self-update channel already uses and the same one WordPress uses to
 * serialise its background auto-updates.
 *
 * THE FULL RATIONALE IS BELOW THE DIRECT-ACCESS GUARD, not above it. Plugin
 * Check's direct-file-access detector only reads the first 50 raw lines of a
 * file when looking for the guard, so a long header comment above it silently
 * turns into a shipping-blocking ERROR. Keep the guard high and the reasoning
 * underneath.
 *
 * @package WPMgr\Agent\Support
 */

declare(strict_types=1);

namespace WPMgr\Agent\Support;

if (!defined('ABSPATH')) {
    exit; // No direct access.
}

/*
 * SiteUpdateLock: ONE update writer per site at a time (GitHub issue #328).
 *
 * WHY THIS EXISTS. The control plane sends exactly one item per `update`
 * command but happily runs several commands against the SAME site at once:
 * its per-target dedup keys on (tenant, site, target_type, target_slug), so a
 * DIFFERENT plugin on the same site is a legal second task, and the per-tenant
 * parallelism default is five. WordPress core is not built for that. The very
 * first thing WP_Upgrader::unpack_package() does is delete EVERY entry under
 * wp-content/upgrade/ (wp-admin/includes/class-wp-upgrader.php:369 to :375,
 * discarding both delete results at :373 and :382). A second upgrader starting
 * while a first is mid-flight therefore deletes the first one's extracted
 * source out from under it, and the first then fails somewhere between
 * "clear the destination" and "move the new tree in", which is how a site ends
 * up with a half-present plugin directory that fatals on the next request.
 *
 * WHAT IT IS. Core's OWN cross-request upgrader lock, taken through
 * WP_Upgrader::create_lock() / release_lock() - the same primitive the agent's
 * self-update channel already uses (class-update-checker.php) and the same one
 * WordPress uses to serialise its background auto-updates. Verified against the
 * bundled WordPress 7.0.2 tree (wp-includes/version.php:19):
 *   - the atomic step is ONE `INSERT IGNORE` statement at
 *     class-wp-upgrader.php:1065; we do not reimplement it and must not claim
 *     to have tested it;
 *   - the get_option() branch at :1068 is reached only after that insert has
 *     already failed;
 *   - validity is judged against the CALLER's release timeout at :1076;
 *   - an expired lock is released and re-taken by recursion at :1080 to :1083,
 *     so mutual exclusion survives two concurrent stealers;
 *   - the winner restamps at :1087;
 *   - release_lock() is an UNCONDITIONAL delete_option() at :1102 to :1103.
 *
 * THREE OUTCOMES, NOT TWO. class-wp-upgrader.php:1067 to :1072 bails false when
 * the INSERT failed AND get_option() is falsy, which conflates "the insert
 * failed for an infrastructure reason" with "somebody holds the lock".
 * `INSERT IGNORE` is MySQL syntax; on a rewritten database backend, a hardened
 * wpdb, or an options table that refuses the write, $wpdb->query() returns
 * false and core's helper would report "locked" forever. A boolean API would
 * then refuse EVERY update on that site permanently with no in-product remedy.
 * acquire() therefore separates HELD_BY_OTHER (refuse) from UNAVAILABLE
 * (proceed without the lock and log loudly). Failing closed on a lock core
 * itself does not even check the return value of would be a worse bug than the
 * one being fixed.
 *
 * THE NAME IS DELIBERATELY NOT core's 'auto_updater'. create_lock() judges
 * staleness against the CALLER's timeout (:1076), so taking core's lock name
 * with our 900s would steal a lock core honours for HOUR_IN_SECONDS (it passes
 * no timeout at class-wp-automatic-updater.php:651, so :1059 to :1061 gives it
 * 3600), and core's own release would then delete OURS, manufacturing exactly
 * the concurrency this class removes.
 *
 * OWNERSHIP IS MANDATORY, NOT DECORATIVE. Because release_lock() is an
 * unconditional delete_option(), a late release() from a holder whose lock had
 * already expired and been stolen would free a LIVE holder. acquire() writes a
 * random token beside the lock; release() and renew() read it back and no-op on
 * a mismatch.
 *
 * DEATH SEMANTICS ARE THE OPPOSITE OF UpdateInFlight'S, ON PURPOSE. No shutdown
 * hook is registered for this lock and none may be added. A PHP fatal, a
 * max_execution_time kill or an FPM SIGTERM mid-swap leaves a half-written
 * directory behind, and admitting a second writer immediately is the worst
 * possible response; the site stays closed until core's own self-expiry at
 * :1076 to :1083 hands the lock to the next caller. UpdateInFlight's per-slug
 * flock needs the exact opposite (it must die with the process, because "can
 * this lock be re-acquired" IS its liveness signal), which is why both exist
 * and neither can do the other's job. LOCK ORDER, fixed and cycle-free: site
 * lock first, then the per-slug flock. It is also a DB row rather than an
 * flock, so it is coherent across nodes on shared storage where flock is not.
 *
 * CONSISTENCY NOTE. TTL (900) is SHORTER than UpdateInFlight::STALE_AFTER_SECONDS
 * (1200). A command arriving at t=901 after a hard kill acquires the lock,
 * reaches the in-flight reconcile and correctly declines to act on a 901s-old
 * marker; at t=1201 it acts. That is today's behaviour and is not a new hole.
 */

/**
 * Serialises every agent-driven writer that can touch wp-content/upgrade/ or a
 * plugin/theme destination on this site.
 */
final class SiteUpdateLock
{
    /** acquire(): this request now owns the lock. */
    public const ACQUIRED = 1;

    /** acquire(): somebody else owns it right now. Refuse, touch nothing. */
    public const HELD_BY_OTHER = 2;

    /**
     * acquire(): the lock could not be evaluated at all (core's helper is
     * absent, or its INSERT failed for an infrastructure reason and the lock
     * row is not readable either). PROCEED WITHOUT THE LOCK and log - see the
     * class doc for why this branch is required rather than defensive padding.
     */
    public const UNAVAILABLE = 3;

    /**
     * Core stores the lock in the wp-option named "<LOCK>.lock". Prefixed, and
     * deliberately NOT core's own 'auto_updater' (class doc).
     */
    private const LOCK = 'wpmgr_site_update';

    /** Non-autoloaded wp-option holding this holder's random owner token. */
    private const OWNER = 'wpmgr_site_update_owner';

    /**
     * Seconds an existing lock is respected, handed to create_lock() as its
     * release timeout.
     *
     * 900 because it is UpdateCommand::APPLY_TIME_LIMIT_SECONDS and
     * LongRunningJob::TIME_LIMIT_SECONDS, the PHP execution budget an apply
     * arms for itself, and the value the self-update channel already passes.
     * The lock must outlive an apply that runs to its own cap and not one
     * second longer, so a worker killed mid-apply blocks the next attempt for
     * exactly as long as it could conceivably still have been alive. Pinned by
     * a test so the three cannot drift apart.
     */
    public const TTL = 900;

    /**
     * The owner token this REQUEST wrote when it took the lock, or '' when this
     * request does not hold it. Also the re-entrancy guard: a second acquire()
     * in the same request must not ask core again, because core would answer
     * "held" (by us) and we would refuse our own work.
     */
    private static string $token = '';

    /**
     * Take the site update lock.
     *
     * @return int One of ACQUIRED, HELD_BY_OTHER, UNAVAILABLE.
     */
    public static function acquire(): int
    {
        if (self::$token !== '') {
            // Already ours in this request. See $token's doc.
            return self::ACQUIRED;
        }

        if (!self::upgraderLockApiAvailable()) {
            DebugLog::write(
                'WPMgr Agent: site update lock unavailable (WP_Upgrader::create_lock is not callable in this '
                . 'runtime); proceeding without per-site serialisation.'
            );

            return self::UNAVAILABLE;
        }

        try {
            $taken = (bool) \WP_Upgrader::create_lock(self::LOCK, self::TTL);
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Agent: site update lock could not be evaluated: ' . $e->getMessage());

            return self::UNAVAILABLE;
        }

        if ($taken) {
            self::$token = bin2hex(random_bytes(16));
            if (function_exists('update_option')) {
                update_option(self::OWNER, self::$token, false);
            }

            return self::ACQUIRED;
        }

        // create_lock() returned false. Distinguish "somebody holds it" from
        // "core could not tell" by reading the lock row ourselves: core's own
        // helper conflates the two (class doc).
        if (self::lockStamp() > 0) {
            return self::HELD_BY_OTHER;
        }

        DebugLog::write(
            'WPMgr Agent: site update lock could not be taken and no lock row is readable; proceeding without '
            . 'per-site serialisation.'
        );

        return self::UNAVAILABLE;
    }

    /**
     * Restamp a lock this request owns so a legitimately long multi-item
     * command cannot expire its own lock between items. Exactly what core does
     * at class-wp-upgrader.php:1087. A no-op when this request does not own the
     * lock any more.
     *
     * @return void
     */
    public static function renew(): void
    {
        if (self::$token === '' || !self::ownsLock()) {
            return;
        }
        if (function_exists('update_option')) {
            update_option(self::LOCK . '.lock', time(), false);
        }
    }

    /**
     * Release a lock this request owns. A no-op when the stored owner token is
     * not ours: release_lock() is an unconditional delete_option()
     * (class-wp-upgrader.php:1102 to :1103), so releasing after our lock
     * expired and was stolen would free a LIVE holder. Do not simplify this
     * check away.
     *
     * @return void
     */
    public static function release(): void
    {
        if (self::$token === '') {
            return;
        }

        if (!self::ownsLock()) {
            DebugLog::write(
                'WPMgr Agent: site update lock was taken over by another request before this one released it; '
                . 'leaving the live holder alone.'
            );
            self::$token = '';

            return;
        }

        try {
            if (self::upgraderLockApiAvailable() && method_exists('\WP_Upgrader', 'release_lock')) {
                \WP_Upgrader::release_lock(self::LOCK);
            }
        } catch (\Throwable $e) {
            DebugLog::write('WPMgr Agent: site update lock release threw: ' . $e->getMessage());
        }

        if (function_exists('delete_option')) {
            delete_option(self::OWNER);
        }

        self::$token = '';
    }

    /**
     * When the currently-held lock self-expires, as a unix timestamp.
     *
     * @return int 0 when no lock row can be read.
     */
    public static function heldUntil(): int
    {
        $stamp = self::lockStamp();

        return $stamp > 0 ? $stamp + self::TTL : 0;
    }

    /**
     * Whether THIS request currently holds the lock. Test/diagnostic seam; no
     * production decision is made on it.
     *
     * @return bool
     */
    public static function heldByThisRequest(): bool
    {
        return self::$token !== '';
    }

    /**
     * Forget any lock state this PROCESS is carrying, without touching the
     * option store. Exists only so a test process (in which statics persist
     * across cases) can start from a known state; production never calls it.
     *
     * @return void
     */
    public static function resetForTests(): void
    {
        self::$token = '';
    }

    /**
     * Is the stored owner token still ours?
     *
     * @return bool
     */
    private static function ownsLock(): bool
    {
        if (self::$token === '' || !function_exists('get_option')) {
            return false;
        }

        $stored = get_option(self::OWNER, '');

        return is_string($stored) && $stored !== '' && hash_equals($stored, self::$token);
    }

    /**
     * The moment the current lock was taken, as core stores it.
     *
     * @return int 0 when absent, unreadable or not a positive integer.
     */
    private static function lockStamp(): int
    {
        if (!function_exists('get_option')) {
            return 0;
        }

        $raw = get_option(self::LOCK . '.lock', 0);
        if (!is_scalar($raw)) {
            return 0;
        }
        $stamp = (int) $raw;

        return $stamp > 0 ? $stamp : 0;
    }

    /**
     * Make sure WP_Upgrader (a wp-admin class) is loaded, since the lock is a
     * static on it. Mirrors the loaders UpdateRunner and UpdateChecker already
     * use; never fatal.
     *
     * @return bool Whether create_lock() can be called.
     */
    private static function upgraderLockApiAvailable(): bool
    {
        if (class_exists('\WP_Upgrader') && method_exists('\WP_Upgrader', 'create_lock')) {
            return true;
        }

        if (!defined('ABSPATH')) {
            return false;
        }

        $base = rtrim((string) ABSPATH, '/\\') . '/wp-admin/includes/';
        foreach (['file.php', 'misc.php', 'plugin.php', 'class-wp-upgrader.php'] as $adminInclude) {
            if (is_file($base . $adminInclude)) {
                require_once $base . $adminInclude;
            }
        }

        return class_exists('\WP_Upgrader') && method_exists('\WP_Upgrader', 'create_lock');
    }
}
