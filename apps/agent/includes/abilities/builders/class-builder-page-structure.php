<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\OwnAbilities;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * wpmgr/page-structure: the structure of one page built with a page
 * builder, read without changing anything.
 *
 * Two kinds of page can be read:
 *
 *   - a draft DraftEligibility admits (WPMgr created it for the AI and the
 *     control plane names it in the signed parameters): editable true, and
 *     each node offers the fields wpmgr/page-edit may set;
 *   - a published page or post with no password: editable false, and no
 *     node offers any field.
 *
 * Either way the post is a page or a post whose layout the adapter holds.
 * Every other post, a missing one included, is refused with the one code
 * post_not_readable; its detail is a token from DETAILS for the
 * person-visible record and never text from the site.
 *
 * The answer:
 *
 *   {post_id, builder, builder_version, format, status, editable,
 *    base_fingerprint, node_count, truncated, nodes}
 *
 * builder is the adapter id and builder_version the builder version the
 * adapter reports for the site (null when it cannot be told); format is
 * "classic", the one format the adapters read; status is the stored post
 * status. base_fingerprint is the page's builder_document_v1 and the nodes
 * are the projection of the stored tree, both from one SQL read of the post
 * and its descriptor rows; the builder's document API is never asked, so
 * the read writes nothing. With a node ref the nodes are that node's
 * subtree (an unknown ref is node_not_found). The nodes are cut to
 * max_nodes (at most BuilderContract::MAX_STRUCTURE_NODES) and the whole
 * answer to BuilderContract::MAX_STRUCTURE_BYTES as json_encode() gives it;
 * node_count counts the nodes of the page, or of the subtree, before the
 * cut.
 */
final class BuilderPageStructure
{
    /** The refusal for every post outside what the read covers. */
    public const CODE_NOT_READABLE = 'post_not_readable';

    /** The format the answer reads the page in. */
    public const FORMAT_CLASSIC = 'classic';

    /** The post types a structure is read for. */
    public const POST_TYPES = ['page', 'post'];

    /**
     * Every detail of a post_not_readable refusal: DraftEligibility's
     * reasons for a draft, then the read's own.
     */
    public const DETAILS = [
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
        'not_published',
        'password',
        'post_type',
        'not_builder_page',
        'owner_unknown',
    ];

    /**
     * Why a wpmgr/page-structure input breaks its schema
     * (BuilderContract::pageStructureInputSchema()), or null: post_id an
     * integer of at least 1, node (optional) a node ref, max_nodes
     * (optional) an integer of 1 to BuilderContract::MAX_STRUCTURE_NODES.
     * Members outside the schema are the caller's check.
     *
     * @param array<string, mixed> $props The input's members.
     * @return string|null
     */
    public static function inputProblem(array $props): ?string
    {
        if (!isset($props['post_id']) || !is_int($props['post_id']) || $props['post_id'] < 1) {
            return 'post_id must be an integer of at least 1';
        }
        if (array_key_exists('node', $props) && (!is_string($props['node']) || preg_match(BuilderContract::RE_REF, $props['node']) !== 1)) {
            return 'node must be a node ref';
        }
        if (array_key_exists('max_nodes', $props)) {
            $max = $props['max_nodes'];
            if (!is_int($max) || $max < 1 || $max > BuilderContract::MAX_STRUCTURE_NODES) {
                return 'max_nodes out of range';
            }
        }

        return null;
    }

    /**
     * The adapter a structure read uses: the first builder the entry's
     * limits.builders_enabled names that this agent knows and has compiled
     * in. A builders_enabled that is not a list of builder ids is
     * bad_input; one that names no compiled builder answers the registry's
     * refusal for the first known id it names, or builder_not_enabled.
     *
     * @param object                             $entry        The catalogue entry, decoded with objects.
     * @param array<string, BuilderAdapter>|null $compiledSeam Tests only: stands in for the compiled set. Production passes none.
     * @return array{adapter?: BuilderAdapter, code?: string, detail?: string}
     */
    public static function adapterFor(object $entry, ?array $compiledSeam = null): array
    {
        $ids = OwnAbilities::buildersEnabled($entry);
        if ($ids === null) {
            return ['code' => 'bad_input', 'detail' => 'the entry\'s limits.builders_enabled is not a list of page builder ids'];
        }
        $limits  = get_object_vars($entry)['limits'] ?? null;
        $refusal = null;
        foreach ($ids as $id) {
            if (!in_array($id, BuilderRegistry::IDS, true)) {
                continue;
            }
            $resolved = BuilderRegistry::resolve('builder:' . $id, $limits, $compiledSeam);
            if (isset($resolved['adapter'])) {
                return ['adapter' => $resolved['adapter']];
            }
            $refusal ??= $resolved;
        }

        return $refusal ?? ['code' => 'builder_not_enabled', 'detail' => 'the catalogue entry enables no page builder'];
    }

    /**
     * Run the read for a validated wpmgr/page-structure input.
     *
     * @param object         $input      Input that passed OwnAbilities::validate().
     * @param array<mixed>   $allowedIds The drafts the signed parameters name (DraftEligibility::signedIds()).
     * @param BuilderAdapter $a          The adapter from adapterFor().
     * @return array{output?: array<string, mixed>, refusal?: array{code: string, detail: string}}
     */
    public static function run(object $input, array $allowedIds, BuilderAdapter $a): array
    {
        $vars     = get_object_vars($input);
        $postId   = $vars['post_id'] ?? null;
        $node     = $vars['node'] ?? null;
        $maxNodes = $vars['max_nodes'] ?? BuilderContract::MAX_STRUCTURE_NODES;
        if (!is_int($postId) || ($node !== null && !is_string($node)) || !is_int($maxNodes)) {
            return ['refusal' => ['code' => 'bad_input', 'detail' => 'the input is not a page-structure input']];
        }

        return self::read($postId, $node, $maxNodes, $allowedIds, $a);
    }

    /**
     * The structure of one page, or the refusal.
     *
     * @param int            $postId     The post.
     * @param string|null    $node       A node ref for one subtree, or null for the whole page.
     * @param int            $maxNodes   Most nodes to answer; clamped to 0..BuilderContract::MAX_STRUCTURE_NODES.
     * @param array<mixed>   $allowedIds The drafts the signed parameters name (DraftEligibility::signedIds()).
     * @param BuilderAdapter $a          The adapter.
     * @return array{output?: array<string, mixed>, refusal?: array{code: string, detail: string}}
     */
    public static function read(int $postId, ?string $node, int $maxNodes, array $allowedIds, BuilderAdapter $a): array
    {
        if ($postId < 1) {
            return self::notReadable('missing');
        }
        $keys = $a->descriptor()->exactKeys;
        try {
            $stored = BuilderDocumentFingerprint::read($postId, $keys);
        } catch (\Throwable $e) {
            return self::notReadable('unreadable');
        }
        if ($stored === null) {
            return self::notReadable('missing');
        }
        $post   = $stored['post'];
        $status = $post['post_status'];

        if ($status === 'draft') {
            $eligibility = DraftEligibility::check($postId, $allowedIds);
            if (!$eligibility['eligible']) {
                return self::notReadable($eligibility['reason']);
            }
            $editable = true;
        } elseif ($status === 'publish') {
            if ($post['post_password'] !== '') {
                return self::notReadable('password');
            }
            $editable = false;
        } else {
            return self::notReadable('not_published');
        }
        if (!in_array($post['post_type'], self::POST_TYPES, true)) {
            return self::notReadable('post_type');
        }
        $owns = $a->owns($postId);
        if ($owns !== true) {
            return self::notReadable($owns === null ? 'owner_unknown' : 'not_builder_page');
        }

        $tree = $a->storedTree($stored['rows']);
        if ($tree === null) {
            return self::refusal(LayoutOps::CODE_UNREADABLE, 'the page\'s builder document could not be read');
        }
        try {
            $projection = $a->project($tree);
            $baseFp     = BuilderDocumentFingerprint::compute($post, $stored['rows'], $keys);
        } catch (\Throwable $e) {
            return self::refusal(LayoutOps::CODE_UNREADABLE, 'the page\'s builder document could not be read');
        }
        if ($node !== null) {
            $projection = $projection->subtree($node);
            if ($projection === null) {
                return self::refusal('node_not_found', 'no node on the page has that ref');
            }
        }
        if (!$editable) {
            $projection = $projection->readOnly();
        }

        $head = [
            'post_id'          => $postId,
            'builder'          => $a->id(),
            'builder_version'  => self::builderVersion($a),
            'format'           => self::FORMAT_CLASSIC,
            'status'           => $status,
            'editable'         => $editable,
            'base_fingerprint' => $baseFp,
        ];
        try {
            // The head and the nodes envelope join into one object: the
            // head's closing brace and the envelope's opening one become a
            // comma, so the whole answer is the head's bytes less one plus
            // the envelope's.
            $headBytes = strlen(json_encode($head, JSON_THROW_ON_ERROR)) - 1;
            $nodes     = $projection->toArray($maxNodes, BuilderContract::MAX_STRUCTURE_BYTES - $headBytes);
        } catch (\JsonException $e) {
            return self::refusal(LayoutOps::CODE_UNREADABLE, 'the page\'s builder document could not be read');
        }

        return ['output' => $head + $nodes];
    }

    /**
     * The builder version the adapter reports, or null when it cannot be told.
     *
     * @param BuilderAdapter $a The adapter.
     * @return string|null
     */
    private static function builderVersion(BuilderAdapter $a): ?string
    {
        try {
            return $a->status()->version;
        } catch (\Throwable $e) {
            return null;
        }
    }

    /**
     * The refusal for a post outside what the read covers.
     *
     * @param string $detail One of DETAILS.
     * @return array{refusal: array{code: string, detail: string}}
     */
    private static function notReadable(string $detail): array
    {
        return self::refusal(self::CODE_NOT_READABLE, in_array($detail, self::DETAILS, true) ? $detail : 'unreadable');
    }

    /**
     * A refusal.
     *
     * @param string $code   Refusal code.
     * @param string $detail WPMgr's own words.
     * @return array{refusal: array{code: string, detail: string}}
     */
    private static function refusal(string $code, string $detail): array
    {
        return ['refusal' => ['code' => $code, 'detail' => $detail]];
    }
}
