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
 * In-flight claims (Sec-F3) are options too: one per request and one per
 * target post, taken with add_option() and released in a finally block.
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
        return get_option(self::INFLIGHT_PREFIX . strtolower($requestId), null) !== null;
    }

    /**
     * Claim a request. False when another call holds it.
     *
     * @param string $requestId Request id.
     * @return bool
     */
    public static function claimRequest(string $requestId): bool
    {
        return (bool) add_option(self::INFLIGHT_PREFIX . strtolower($requestId), (string) time(), '', false);
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
        return (bool) add_option(self::TARGET_PREFIX . $postId, (string) time(), '', false);
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
