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
 * block.
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
        global $wpdb;
        $name = self::INFLIGHT_PREFIX . strtolower($requestId);
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- the claim row is written by a raw INSERT IGNORE, so it is read raw too; the options cache never holds it
        $found = $wpdb->get_var($wpdb->prepare("SELECT option_id FROM {$wpdb->options} WHERE option_name = %s", $name));

        return $found !== null;
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
     * option_name lets exactly one of them insert the row.
     *
     * @param string $name Option name.
     * @return bool True only when this call inserted the row.
     */
    private static function claim(string $name): bool
    {
        global $wpdb;
        // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery, WordPress.DB.DirectDatabaseQuery.NoCaching -- an atomic claim must be one INSERT IGNORE on the unique key; add_option() is not atomic under a race
        $rows = $wpdb->query($wpdb->prepare("INSERT IGNORE INTO {$wpdb->options} (option_name, option_value, autoload) VALUES (%s, %s, 'no')", $name, (string) time()));

        return $rows === 1;
    }

    /**
     * @param string $requestId Request id.
     * @return void
     */
    public static function releaseRequest(string $requestId): void
    {
        delete_option(self::INFLIGHT_PREFIX . strtolower($requestId));
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
        delete_option(self::TARGET_PREFIX . $postId);
    }
}
