<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\AbilityLedger;
use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Whether a draft is one WPMgr created for the AI, so the AI may read it as
 * editable and edit it.
 *
 * A draft is eligible only when the control plane and the site both say so,
 * checked in this order, the first that fails giving the reason:
 *
 *   1. the post id is in the drafts the control plane named in the signed
 *      parameters (p.allowed_draft_ids)               not_in_signed_list
 *   2. the post exists                                 missing
 *   3. it has exactly one MARKER_KEY row, read with SQL (no_marker for none),
 *      and that row is a request id R                  marker_not_a_request
 *   4. the site ledger has a wpmgr/page-create row for R  ledger_missing
 *      that completed                                  ledger_not_completed
 *      whose created post is this post                 ledger_other_post
 *      and whose undo did not trash it                 trashed
 *   5. the post is a draft                             not_draft
 *
 * A post whose rows or ledger row cannot be read is not eligible
 * (unreadable). A copy of a draft that carries the original's marker names
 * a ledger row whose created post is another post, so it is never eligible.
 *
 * The reason is for the person-visible record only; what the AI is told
 * never depends on it. check() writes nothing.
 */
final class DraftEligibility
{
    /** The post meta naming the request that created a post. */
    public const MARKER_KEY = '_wpmgr_created_by_request';

    /** The member of the signed parameters that names the drafts. */
    public const SIGNED_KEY = 'allowed_draft_ids';

    /** Most drafts the signed parameters name: the one post a call is about. */
    public const MAX_SIGNED_IDS = 1;

    /** Every reason check() answers for a post that is not eligible. */
    public const REASONS = [
        'not_in_signed_list',
        'missing',
        'unreadable',
        'no_marker',
        'marker_not_a_request',
        'ledger_missing',
        'ledger_not_completed',
        'ledger_other_post',
        'trashed',
        'not_draft',
    ];

    /** A request id (the $ matches only at the very end). */
    private const RE_REQUEST_ID = '/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/Di';

    /**
     * Whether the post is an eligible draft, and why not.
     *
     * @param int          $postId     The post.
     * @param array<mixed> $allowedIds The drafts the signed parameters name, as signedIds() gives them.
     * @return array{eligible: bool, reason: string} The reason is one of REASONS, or "" when eligible.
     */
    public static function check(int $postId, array $allowedIds): array
    {
        if ($postId < 1 || !in_array($postId, $allowedIds, true)) {
            return self::no('not_in_signed_list');
        }
        try {
            $stored = BuilderDocumentFingerprint::read($postId, [self::MARKER_KEY]);
        } catch (\Throwable $e) {
            return self::no('unreadable');
        }
        if ($stored === null) {
            return self::no('missing');
        }
        $markers = $stored['rows'][self::MARKER_KEY] ?? [];
        if ($markers === []) {
            return self::no('no_marker');
        }
        if (count($markers) !== 1 || preg_match(self::RE_REQUEST_ID, $markers[0]) !== 1) {
            return self::no('marker_not_a_request');
        }

        try {
            $row = AbilityLedger::get($markers[0]);
        } catch (\Throwable $e) {
            return self::no('unreadable');
        }
        if ($row === null || ($row['ability'] ?? null) !== OwnAbilities::NAME_PAGE_CREATE) {
            return self::no('ledger_missing');
        }
        if (($row['phase'] ?? null) !== 'completed') {
            return self::no('ledger_not_completed');
        }
        if (($row['created_post_id'] ?? null) !== $postId) {
            return self::no('ledger_other_post');
        }
        if (($row['undo_state'] ?? null) === 'trashed') {
            return self::no('trashed');
        }
        if ($stored['post']['post_status'] !== 'draft') {
            return self::no('not_draft');
        }

        return ['eligible' => true, 'reason' => ''];
    }

    /**
     * The drafts the signed parameters name: [] when p has no SIGNED_KEY;
     * else a list of at most MAX_SIGNED_IDS distinct post ids, each a JSON
     * integer of at least 1. Null for anything else, which refuses the call.
     *
     * @param object $p The signed parameters, decoded with objects.
     * @return list<int>|null
     */
    public static function signedIds(object $p): ?array
    {
        $vars = get_object_vars($p);
        if (!array_key_exists(self::SIGNED_KEY, $vars)) {
            return [];
        }
        $ids = $vars[self::SIGNED_KEY];
        if (!is_array($ids) || !ArrayShape::isList($ids) || count($ids) > self::MAX_SIGNED_IDS) {
            return null;
        }
        $out = [];
        foreach ($ids as $id) {
            if (!is_int($id) || $id < 1 || in_array($id, $out, true)) {
                return null;
            }
            $out[] = $id;
        }

        return $out;
    }

    /**
     * Not eligible, for a reason.
     *
     * @param string $reason One of REASONS.
     * @return array{eligible: bool, reason: string}
     */
    private static function no(string $reason): array
    {
        return ['eligible' => false, 'reason' => $reason];
    }
}
