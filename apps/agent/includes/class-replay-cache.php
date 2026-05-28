<?php
/**
 * ReplayCache: DB-backed single-use shield for autologin JWT ids (`jti`).
 *
 * The Connector already has its own per-token replay shield with a 300s
 * window. Autologin tokens, however, must be single-use for a much longer
 * window (the cookie they mint is long-lived) and the bookkeeping table is
 * different — we store the jti hash + an absolute expires_at, NOT a row per
 * token-lifetime. This avoids any coupling to Connector internals and keeps
 * the autologin replay window independent.
 *
 * Table layout (created via dbDelta on activation):
 *
 *   {prefix}wpmgr_autologin_jti (
 *       jti_hash    CHAR(64)         NOT NULL PRIMARY KEY,
 *       expires_at  BIGINT UNSIGNED  NOT NULL,
 *       KEY expires_at (expires_at)
 *   )
 *
 *   - PRIMARY KEY is the SHA-256 of the raw jti (fixed-width, never stores
 *     the raw value).
 *   - expires_at is a Unix timestamp; rows past it are eligible for prune.
 *
 * @package WPMgr\Agent
 */

declare(strict_types=1);

namespace WPMgr\Agent;

/**
 * Persistent single-use cache for autologin token identifiers.
 *
 * Not declared `final` so tests can subclass it to spy on the mark/seen
 * ordering against cookie issuance; production code never inherits from it.
 */
class ReplayCache
{
    /** Unqualified table name (prefixed at runtime via $wpdb->prefix). */
    public const TABLE = 'wpmgr_autologin_jti';

    /** Cron hook used to prune expired rows. */
    public const HOOK_PRUNE = 'wpmgr_agent_autologin_prune';

    /**
     * Whether a jti has already been recorded inside its live window.
     *
     * @param string $jti      Raw token identifier (never stored as-is).
     * @param int|null $now    Override "current time" (testing); default time().
     * @return bool
     */
    public function seen(string $jti, ?int $now = null): bool
    {
        if ($jti === '') {
            // An empty jti is never "seen"; the caller is responsible for
            // rejecting empty values BEFORE consulting the cache.
            return false;
        }

        $wpdb = $this->wpdb();
        if ($wpdb === null) {
            return false;
        }

        $now   = $now ?? time();
        $table = $this->tableName();
        $hash  = $this->hashJti($jti);

        // $table is built from a class constant + the trusted wpdb prefix
        // (no user input), so interpolating it ahead of prepare() is safe.
        // @phpstan-ignore-next-line
        $sql = $wpdb->prepare("SELECT 1 FROM {$table} WHERE jti_hash = %s AND expires_at >= %d LIMIT 1", $hash, $now);
        if (!is_string($sql)) {
            return false;
        }

        return $wpdb->get_var($sql) !== null;
    }

    /**
     * Mark a jti as consumed. Returns true on success.
     *
     * The caller MUST treat a false return as "do not issue the cookie": this
     * is the ordering guarantee that protects against an attacker racing two
     * presentations of the same token through different web workers.
     *
     * @param string $jti        Raw token identifier.
     * @param int    $ttlSeconds Lifetime relative to $now (>= 0).
     * @param int|null $now      Override "current time" (testing); default time().
     * @return bool True when the row was inserted; false otherwise (including
     *              "already present" — which the caller should treat as replay).
     */
    public function mark(string $jti, int $ttlSeconds, ?int $now = null): bool
    {
        if ($jti === '' || $ttlSeconds <= 0) {
            return false;
        }

        $wpdb = $this->wpdb();
        if ($wpdb === null) {
            // No DB available => no durable replay shield => refuse to mark.
            return false;
        }

        $now   = $now ?? time();
        $table = $this->tableName();

        $result = $wpdb->insert(
            $table,
            [
                'jti_hash'   => $this->hashJti($jti),
                'expires_at' => $now + $ttlSeconds,
            ],
            ['%s', '%d']
        );

        // wpdb->insert returns int|false. A duplicate-key violation surfaces
        // as false (the PK uniqueness on jti_hash guarantees this), which we
        // map back to "could not mark" for the caller to treat as replay.
        if (is_int($result) && $result > 0) {
            return true;
        }

        // Diagnostics: surface the actual driver error to debug.log so an
        // operator tailing it can distinguish "table missing" (the M5.5
        // re-upload bug) from a generic insert failure. The HTTP response
        // remains the generic `wpmgr_replay_mark_failed` — no internal
        // detail is leaked to the wire.
        $this->logInsertFailure($wpdb, $table);

        return false;
    }

    /**
     * Emit a single diagnostic line to PHP's error log when an insert fails.
     * Honors WP_DEBUG_LOG via the standard error_log() path.
     *
     * @param object $wpdb  WordPress DB handle (untyped: tests pass doubles).
     * @param string $table Fully-qualified table name.
     * @return void
     */
    private function logInsertFailure(object $wpdb, string $table): void
    {
        $lastError = '';
        if (property_exists($wpdb, 'last_error')) {
            $lastError = is_string($wpdb->last_error) ? $wpdb->last_error : '';
        }

        $missingTable = $lastError !== '' && stripos($lastError, "doesn't exist") !== false;

        $message = sprintf(
            'wpmgr-agent: autologin replay insert failed (table=%s)%s%s',
            $table,
            $lastError === '' ? '' : ' driver_error=' . $lastError,
            $missingTable ? ' hint=run-Schema::ensureCurrent-to-recreate' : ''
        );

        // error_log() is the standard WP debug.log channel when WP_DEBUG_LOG
        // is enabled; outside that it lands in the SAPI's error stream.
        error_log($message);
    }

    /**
     * Delete rows whose expires_at is in the past. Returns the number purged
     * (0 when there's nothing to do or no DB is available).
     *
     * @param int|null $now Override "current time" (testing); default time().
     * @return int
     */
    public function prune(?int $now = null): int
    {
        $wpdb = $this->wpdb();
        if ($wpdb === null) {
            return 0;
        }

        $now   = $now ?? time();
        $table = $this->tableName();

        // @phpstan-ignore-next-line
        $sql = $wpdb->prepare("DELETE FROM {$table} WHERE expires_at < %d", $now);
        if (!is_string($sql)) {
            return 0;
        }

        $result = $wpdb->query($sql);

        return is_int($result) ? $result : 0;
    }

    /**
     * Compute the storage hash of a jti.
     *
     * @param string $jti Raw token identifier.
     * @return string Hex SHA-256.
     */
    private function hashJti(string $jti): string
    {
        return hash('sha256', $jti);
    }

    /**
     * Fully-qualified table name including the WP table prefix.
     *
     * @return string
     */
    private function tableName(): string
    {
        $wpdb   = $this->wpdb();
        $prefix = ($wpdb !== null && $wpdb->prefix !== '') ? $wpdb->prefix : 'wp_';

        return $prefix . self::TABLE;
    }

    /**
     * Resolve the WordPress database handle, if available.
     *
     * @return \wpdb|null
     */
    private function wpdb(): ?object
    {
        if (!isset($GLOBALS['wpdb']) || !is_object($GLOBALS['wpdb'])) {
            return null;
        }

        /** @var \wpdb $wpdb */
        $wpdb = $GLOBALS['wpdb'];

        return $wpdb;
    }
}
