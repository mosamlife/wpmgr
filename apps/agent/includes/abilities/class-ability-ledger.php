<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The site ledger for engine writes: one row per request_id.
 *
 * Each row is one non-autoloaded option named for its request_id, so the
 * options table's unique option_name is the UNIQUE(request_id) constraint.
 * A row holds the ability, the created object ids, the before and after
 * fingerprints, the phase, the undo state and the stored result.
 *
 * In-flight claims (Sec-F3) are options rows too: one per request and one per
 * target post, taken with an atomic INSERT IGNORE and released in a finally
 * block. A claim older than CLAIM_TTL is stale: it is not in flight, and the
 * next caller takes it over with one conditional UPDATE, so exactly one taker
 * wins. A release deletes only the claim this process holds.
 *
 * Object ids used by undo come only from the row for the token-bound
 * request_id. Nothing in the call's input is ever read for them.
 */
final class AbilityLedger
{
    private const ROW_PREFIX      = 'wpmgr_ability_ledger_';
    private const INFLIGHT_PREFIX = 'wpmgr_ability_inflight_';
    private const TARGET_PREFIX   = 'wpmgr_ability_target_';

    /**
     * Seconds after which a claim is stale. A page-create run is one insert
     * and one read-back, bounded by the control plane's per-call timeout and
     * by PHP's max_execution_time; this sits well above both, and well inside
     * the control plane's ledger resolution window.
     */
    public const CLAIM_TTL = 300;

    /** @var array<string,string> Claim values this process holds, by option name. */
    private static array $held = [];

    /**
     * The row for a request, or null.
     *
     * @param string $requestId Request id (validated UUID).
     * @return array<string,mixed>|null
     */
    public static function get(string $requestId): ?array
    {
        $row = get_option(self::ROW_PREFIX . strtolower($requestId), null);

        return is_array($row) ? $row : null;
    }

    /**
     * The row for a request as the options table holds it now, or null.
     * get() can answer this request's cached copy of the row, which misses a
     * write another request made since; this drops that copy first, so the
     * row is read from the table, and get() and update() later in this
     * request start from what it read.
     *
     * @param string $requestId Request id (validated UUID).
     * @return array<string,mixed>|null
     */
    public static function getStored(string $requestId): ?array
    {
        wp_cache_delete(self::ROW_PREFIX . strtolower($requestId), 'options');

        return self::get($requestId);
    }

    /**
     * Create the row. False when a row already exists or the write failed.
     *
     * @param string              $requestId Request id.
     * @param array<string,mixed> $row       Row.
     * @return bool
     */
    public static function create(string $requestId, array $row): bool
    {
        return (bool) add_option(self::ROW_PREFIX . strtolower($requestId), $row, '', false);
    }

    /**
     * Merge fields into an existing row.
     *
     * @param string              $requestId Request id.
     * @param array<string,mixed> $fields    Fields.
     * @return bool
     */
    public static function update(string $requestId, array $fields): bool
    {
        $row = self::get($requestId);
        if ($row === null) {
            return false;
        }
        $next = array_merge($row, $fields);
        if ($next === $row) {
            return true;
        }

        return (bool) update_option(self::ROW_PREFIX . strtolower($requestId), $next, false);
    }

    /**
     * Is a request in flight?
     *
     * @param string $requestId Request id.
     * @return bool
     */
    public static function inflight(string $requestId): bool
    {
        $value = self::claimValue(self::INFLIGHT_PREFIX . strtolower($requestId));

        return $value !== null && !self::isStale($value);
    }

    /**
     * Claim a request. False when another call holds it.
     *
     * @param string $requestId Request id.
     * @return bool
     */
    public static function claimRequest(string $requestId): bool
    {
        return self::claim(self::INFLIGHT_PREFIX . strtolower($requestId));
    }

    /**
     * Atomic claim (Sec-F3). core's add_option() writes with ON DUPLICATE KEY
     * UPDATE, so two racers can both succeed; INSERT IGNORE on the unique
     * option_name lets exactly one of them insert the row. A stale claim is
     * taken over by an UPDATE conditional on the stale value, which only one
     * racer can match.
     *
     * @param string $name Option name.
     * @return bool True only when this call now holds the claim.
     */
    private static function claim(string $name): bool
    {
        global $wpdb;
        $mine = time() . ':' . bin2hex(random_bytes(8));

        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- an atomic claim must be one INSERT IGNORE on the unique key; add_option() is not atomic under a race
        $rows = $wpdb->query($wpdb->prepare("INSERT IGNORE INTO {$wpdb->options} (option_name, option_value, autoload) VALUES (%s, %s, 'no')", $name, $mine));
        if ($rows === 1) {
            self::$held[$name] = $mine;

            return true;
        }

        $old = self::claimValue($name);
        if ($old === null || !self::isStale($old)) {
            return false;
        }
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- the takeover must be one UPDATE conditional on the stale value so only one taker wins; update_option() is not conditional
        $rows = $wpdb->query($wpdb->prepare("UPDATE {$wpdb->options} SET option_value = %s WHERE option_name = %s AND option_value = %s", $mine, $name, $old));
        if ($rows !== 1) {
            return false;
        }
        self::$held[$name] = $mine;

        return true;
    }

    /**
     * The raw claim value, or null when there is no claim.
     *
     * @param string $name Option name.
     * @return string|null
     */
    private static function claimValue(string $name): ?string
    {
        global $wpdb;
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- the claim row is written raw, so it is read raw too; the options cache never holds it
        $value = $wpdb->get_var($wpdb->prepare("SELECT option_value FROM {$wpdb->options} WHERE option_name = %s", $name));

        return is_string($value) ? $value : null;
    }

    /**
     * Is a claim value stale? An unreadable value is stale: it can never be
     * released by its owner's value match, so it must not block forever.
     *
     * @param string $value Claim value ("<unix time>:<nonce>").
     * @return bool
     */
    private static function isStale(string $value): bool
    {
        if (preg_match('/^([0-9]{1,12})(?::|$)/', $value, $m) !== 1) {
            return true;
        }

        return time() - (int) $m[1] >= self::CLAIM_TTL;
    }

    /**
     * Release a claim, only when this process still holds it: a claim taken
     * over after it went stale belongs to its new holder.
     *
     * @param string $name Option name.
     * @return void
     */
    private static function release(string $name): void
    {
        global $wpdb;
        $mine = self::$held[$name] ?? null;
        unset(self::$held[$name]);
        if ($mine === null) {
            return;
        }
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- the release must delete only the value this process wrote; delete_option() would delete a new holder's claim
        $wpdb->query($wpdb->prepare("DELETE FROM {$wpdb->options} WHERE option_name = %s AND option_value = %s", $name, $mine));
    }

    /**
     * @param string $requestId Request id.
     * @return void
     */
    public static function releaseRequest(string $requestId): void
    {
        self::release(self::INFLIGHT_PREFIX . strtolower($requestId));
    }

    /**
     * Claim a target post: one engine write per post at a time.
     *
     * @param int $postId Post id.
     * @return bool
     */
    public static function claimTarget(int $postId): bool
    {
        return self::claim(self::TARGET_PREFIX . $postId);
    }

    /**
     * @param int $postId Post id.
     * @return void
     */
    public static function releaseTarget(int $postId): void
    {
        self::release(self::TARGET_PREFIX . $postId);
    }
}
