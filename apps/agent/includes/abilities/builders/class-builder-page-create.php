<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\OwnAbilities;
use WPMgr\Agent\Abilities\PageCreateBuilder;
use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * wpmgr/page-create for a page builder: the precheck, the write and the undo
 * guard of a builder draft. The command keeps idempotency, the claims, the
 * digest re-check and the ledger row; everything between the ledger row and
 * the answer is here.
 *
 * Elementor is the one builder compiled in, and only its classic format is
 * built: elementor_format "site_default" and "classic" build classic, and
 * "atomic" is refused with builder_not_available / atomic_unavailable.
 *
 * precheck() writes nothing. It refuses what the site's Elementor facts
 * refuse, builds the tree with node ids derived from the request id, checks
 * the tree as Elementor would store it, and answers the preview:
 *
 *   {post_type, editor, format, elementor_version, layout, status: "draft",
 *    title, tree} plus "media" (the resolved image facts) when the outline
 *   has an image
 *
 *   preview_digest   = sha256(json_encode([editor, format, elementor_version,
 *                      post_type, "draft", title, tree_json]))
 *   base_fingerprint = PageCreateBuilder::baseFingerprint(post_type, media facts)
 *
 * json_encode with default flags; tree_json is the tree's canonical bytes.
 * The same request, input and site give the same bytes, so the write's
 * re-check matches the approved precheck.
 *
 * write() inserts the draft, records it on the ledger, saves the tree through
 * the adapter and reads the page back. A draft that fails any step after the
 * insert is moved to the trash and the answer says so ("trashed"). A created
 * draft records its builder_document_v1 fingerprint and the revisions its own
 * save made, which is what revertProblem() checks before an undo.
 */
final class BuilderPageCreate
{
    /** elementor_format: the format the site builds by default. */
    public const FORMAT_SITE_DEFAULT = 'site_default';

    /** The attachment meta holding the media library's alt text. */
    public const ALT_META_KEY = '_wp_attachment_image_alt';

    /** Preview layout of a tree of containers. */
    public const LAYOUT_CONTAINERS = 'containers';

    /** Preview layout of a tree of sections and columns. */
    public const LAYOUT_SECTIONS = 'sections';

    /** Codes a failed save may answer with; anything else is builder_save_refused. */
    private const SAVE_CODES = [
        ElementorDocument::CODE_REFUSED,
        ElementorDocument::CODE_CRASHED,
        ElementorDocument::CODE_SIDE_EFFECT,
    ];

    /** The fingerprinted rows of a draft, by the builder that made it. */
    private const DESCRIPTOR_KEYS = [
        ElementorAdapter::ID => ElementorDocument::DESCRIPTOR_KEYS,
    ];

    /** Most revisions an undo guard reads from the ledger row. */
    private const MAX_OWN_REVISIONS = 50;

    /**
     * Build and check the page, and answer the preview. Writes nothing.
     *
     * On a refusal only "refusal" carries meaning. Otherwise "doc" is the
     * built document and "title" the post_title bytes the write stores.
     *
     * @param array<string, mixed>             $spec       Validated page-create spec with a builder editor.
     * @param BuilderAdapter                   $a          The resolved adapter.
     * @param array<string, mixed>             $facts      The adapter's site facts (ElementorFacts::collect()).
     * @param list<array<string, mixed>>       $mediaFacts Resolved image facts, in outline order.
     * @param string                           $requestId  The request; node ids derive from it.
     * @return array{refusal?: array<string, mixed>, preview: array<string, mixed>, preview_digest: string, base_fingerprint: string, tree: array<mixed>, doc: NativeDocument|null, title: string}
     */
    public static function precheck(array $spec, BuilderAdapter $a, array $facts, array $mediaFacts, string $requestId): array
    {
        if (!$a instanceof ElementorAdapter) {
            return self::refusedPrecheck(AdapterStatus::CODE, 'not_compiled');
        }
        $postType = $spec['post_type'] ?? null;
        $editor   = $spec['editor'] ?? null;
        $title    = $spec['title'] ?? null;
        if (!is_string($postType) || !is_string($title) || $editor !== 'builder:' . $a->id()) {
            return self::refusedPrecheck('bad_input', 'the input does not ask for a page built with this page builder');
        }
        $format = self::format($spec['elementor_format'] ?? null);
        if ($format === null) {
            return self::refusedPrecheck('bad_input', 'elementor_format must be site_default, classic or atomic');
        }
        $refusal = ElementorFacts::createRefusal($facts, $postType, $format);
        if ($refusal !== null) {
            return self::refusedPrecheck($refusal['code'], $refusal['detail']);
        }
        $version = $facts['version'] ?? null;
        if (!is_string($version) || $version === '') {
            return self::refusedPrecheck(AdapterStatus::CODE, 'version_unverified');
        }

        $byId = [];
        foreach ($mediaFacts as $fact) {
            if (!is_array($fact) || PageCreateBuilder::mediaFactProblem($fact) !== null) {
                return self::refusedPrecheck('image_not_available', 'the image facts are incomplete');
            }
            $alt = get_post_meta($fact['id'], self::ALT_META_KEY, true);
            if (is_string($alt)) {
                $fact['alt'] = $alt;
            }
            $byId[$fact['id']] = $fact;
        }

        try {
            $built = $a->buildCreate($spec, new IdSeed($requestId), $byId);
        } catch (\Throwable $e) {
            return self::refusedPrecheck('bad_input', 'the page could not be built');
        }
        $doc = $built['doc'] ?? null;
        if (!$doc instanceof NativeDocument) {
            return self::refusedPrecheck((string) ($built['code'] ?? 'bad_input'), (string) ($built['detail'] ?? 'the page could not be built'));
        }
        $problem = $a->document()->precheckTree($doc->tree);
        if ($problem !== null) {
            return self::refusedPrecheck($problem['code'], $problem['detail']);
        }

        // The stored title is a fixed function of the plain title the digest
        // binds; the save this principal gets must keep it byte for byte.
        $storedTitle = PageCreateBuilder::storedTitle($title);
        $simTitle    = wp_unslash(apply_filters('title_save_pre', wp_slash($storedTitle))); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- core's own save filter, applied to simulate exactly what wp_insert_post will do
        if ($simTitle !== $storedTitle) {
            return self::refusedPrecheck('sanitiser_changed_new_content', 'the site would change the title on save');
        }

        try {
            $digest = self::previewDigest($editor, $format, $version, $postType, $title, $doc->canonicalBytes);
            $base   = PageCreateBuilder::baseFingerprint($postType, $mediaFacts);
        } catch (\JsonException $e) {
            return self::refusedPrecheck('bad_input', 'the page could not be encoded');
        }

        $preview = [
            'post_type'         => $postType,
            'editor'            => $editor,
            'format'            => $format,
            'elementor_version' => $version,
            'layout'            => self::layout($doc->tree),
            'status'            => 'draft',
            'title'             => $title,
            'tree'              => $doc->tree,
        ];
        // Only an outline with an image carries media, as on the block editor.
        if ($mediaFacts !== []) {
            $preview['media'] = array_values($mediaFacts);
        }

        return [
            'preview'          => $preview,
            'preview_digest'   => $digest,
            'base_fingerprint' => $base,
            'tree'             => $doc->tree,
            'doc'              => $doc,
            'title'            => $storedTitle,
        ];
    }

    /**
     * Create the draft, after the command recorded the ledger row for the
     * request and re-checked that $built matches what was approved. Runs as
     * the principal.
     *
     * $ledgerUpdate merges fields into this request's ledger row and answers
     * whether it was recorded.
     *
     * @param int                                $principal    The WPMgr service user.
     * @param array<string, mixed>               $spec         The spec $built was made from.
     * @param BuilderAdapter                     $a            The resolved adapter.
     * @param string                             $requestId    The request.
     * @param array<string, mixed>               $built        What precheck() answered for this write.
     * @param callable(array<string, mixed>): bool $ledgerUpdate Merges fields into the ledger row.
     * @return array<string, mixed>
     */
    public static function write(int $principal, array $spec, BuilderAdapter $a, string $requestId, array $built, callable $ledgerUpdate): array
    {
        $doc      = $built['doc'] ?? null;
        $title    = $built['title'] ?? null;
        $digest   = $built['preview_digest'] ?? null;
        $preview  = $built['preview'] ?? null;
        $postType = $spec['post_type'] ?? null;
        $editor   = $spec['editor'] ?? null;
        $format   = is_array($preview) ? ($preview['format'] ?? null) : null;
        $version  = is_array($preview) ? ($preview['elementor_version'] ?? null) : null;
        if (!$a instanceof ElementorAdapter || !$doc instanceof NativeDocument || !is_string($title) || !is_string($digest)
            || !is_string($postType) || !is_string($editor) || !is_string($format) || !is_string($version) || $principal < 1) {
            $result = self::fail('bad_input', 'the page was not built for this write');
            $ledgerUpdate(['phase' => 'failed', 'result' => $result]);

            return $result;
        }

        // Always a draft, always the principal's, the content left to the
        // builder's save. The meta is the request marker and the rows the
        // builder needs to treat the post as its page.
        $postId = wp_insert_post(wp_slash([
            'post_type'    => $postType,
            'post_status'  => 'draft',
            'post_author'  => $principal,
            'post_title'   => $title,
            'post_content' => '',
            'meta_input'   => [ElementorDocument::MARKER_KEY => $requestId] + $doc->meta,
        ]), true);
        if (!is_int($postId) || $postId < 1) {
            $result = self::fail('refused_by_site', 'WordPress did not create the draft');
            $ledgerUpdate(['phase' => 'failed', 'result' => $result]);

            return $result;
        }
        if (!$ledgerUpdate(['phase' => 'post_created', 'created_post_id' => $postId])) {
            // Undo could never find this draft, so it is not left behind.
            $trashed = self::trashOwn($postId);
            $result  = self::fail('snapshot_failed', 'the ledger could not record the created draft; it was moved to the trash', ['post_id' => $postId, 'trashed' => $trashed]);
            $ledgerUpdate([
                'phase'           => 'failed',
                'created_post_id' => $postId,
                'undo_state'      => $trashed ? 'trashed' : 'none',
                'result'          => $result,
            ]);

            return $result;
        }

        $target = $a->document()->targetProblem($postId, $a->facts());
        if ($target === 'not_editable') {
            return self::failTrashed($postId, AdapterStatus::CODE, 'role_excluded', $ledgerUpdate);
        }
        if ($target !== null) {
            return self::failTrashed($postId, ElementorDocument::CODE_REFUSED, 'the new draft cannot take the page builder\'s save: ' . $target, $ledgerUpdate);
        }

        $saved = $a->write($postId, $doc);
        if (($saved['ok'] ?? null) !== true) {
            $code = $saved['code'] ?? null;
            $code = is_string($code) && in_array($code, self::SAVE_CODES, true) ? $code : ElementorDocument::CODE_REFUSED;
            $why  = $saved['detail'] ?? null;

            return self::failTrashed($postId, $code, is_string($why) && $why !== '' ? $why : 'the page builder did not save the page', $ledgerUpdate);
        }

        $problem = $a->verifyCreated($postId, $doc, $principal, $requestId);
        $afterFp = null;
        if ($problem === null) {
            $keys = $a->descriptor()->exactKeys;
            try {
                $stored = BuilderDocumentFingerprint::read($postId, $keys);
                if ($stored === null) {
                    $problem = 'post_missing';
                } elseif ($stored['post']['post_title'] !== $title) {
                    $problem = 'title';
                } else {
                    $afterFp = BuilderDocumentFingerprint::compute($stored['post'], $stored['rows'], $keys);
                }
            } catch (\Throwable $e) {
                $problem = 'post_unreadable';
            }
        }
        if ($problem !== null || $afterFp === null) {
            return self::failTrashed($postId, 'verify_mismatch', 'the created draft is not what was built: ' . ($problem ?? 'fingerprint'), $ledgerUpdate);
        }

        $result = [
            'ok'                => true,
            'outcome'           => 'created',
            'mode'              => 'write',
            'ability'           => OwnAbilities::NAME_PAGE_CREATE,
            'request_id'        => $requestId,
            'post_id'           => $postId,
            'post_type'         => $postType,
            'editor'            => $editor,
            'format'            => $format,
            'elementor_version' => $version,
            'status'            => 'draft',
            'preview_digest'    => $digest,
            'after_fp'          => $afterFp,
            'verify'            => ['tree_equal' => true, 'status' => 'draft'],
        ];
        // The draft exists and its id is recorded, so the caller is told it
        // was created even when the completed state cannot be recorded.
        $recorded = $ledgerUpdate([
            'phase'            => 'completed',
            'after_fp'         => $afterFp,
            'undo_state'       => 'available',
            'builder'          => $a->id(),
            'builder_version'  => $version,
            'format'           => $format,
            'own_revision_ids' => self::scopeRevisions($saved['scope'] ?? null),
            'result'           => $result,
        ]);
        if (!$recorded) {
            $result['ledger_recorded'] = false;
        }

        return $result;
    }

    /**
     * Why undo may not trash this builder draft, or null.
     *
     * Undo trashes the draft only when its builder_document_v1 fingerprint
     * is still the one recorded when it was created (its content, its
     * builder rows and where it sits, parent and order), it has no revision
     * but the ones its own save made, no autosave, and no one holds its edit
     * lock. Fails closed when any of that cannot be read.
     *
     * @param int                  $postId    The created draft, from the ledger row.
     * @param array<string, mixed> $ledgerRow The request's ledger row.
     * @return string|null
     */
    public static function revertProblem(int $postId, array $ledgerRow): ?string
    {
        $builder = $ledgerRow['builder'] ?? null;
        $keys    = is_string($builder) ? (self::DESCRIPTOR_KEYS[$builder] ?? null) : null;
        if ($keys === null) {
            return 'this draft was built with a page builder this agent cannot check';
        }
        $afterFp = $ledgerRow['after_fp'] ?? null;
        if (!is_string($afterFp) || preg_match('/^[0-9a-f]{64}$/D', $afterFp) !== 1) {
            return 'the created draft has no recorded fingerprint';
        }
        $own = self::ledgerRevisions($ledgerRow['own_revision_ids'] ?? null);
        if ($own === null) {
            return 'the created draft has no record of its own revisions';
        }
        if ($postId < 1) {
            return 'the created draft no longer exists';
        }

        try {
            $current = BuilderDocumentFingerprint::ofPost($postId, $keys);
        } catch (\Throwable $e) {
            return 'the draft could not be read';
        }
        if ($current === null) {
            return 'the created draft no longer exists';
        }
        if (!hash_equals($afterFp, $current)) {
            return 'someone edited this draft after it was created';
        }

        // User id 0 (the int) means an autosave by any user.
        if (wp_get_post_autosave($postId, 0) !== false) {
            return 'someone has unsaved changes to this draft; open it in WordPress';
        }
        $revisions = wp_get_post_revisions($postId, ['check_enabled' => false]);
        if (!is_array($revisions)) {
            return 'the revisions of this draft could not be read';
        }
        foreach ($revisions as $key => $revision) {
            $id = is_object($revision) ? (get_object_vars($revision)['ID'] ?? null) : (is_int($revision) ? $revision : $key);
            if (!is_int($id) || !in_array($id, $own, true)) {
                return 'this draft has revisions its creation did not make; open it in WordPress';
            }
        }

        if (!function_exists('wp_check_post_lock') && defined('ABSPATH') && is_readable(ABSPATH . 'wp-admin/includes/post.php')) {
            require_once ABSPATH . 'wp-admin/includes/post.php';
        }
        if (!function_exists('wp_check_post_lock')) {
            return 'whether someone is editing this draft could not be checked';
        }
        if (wp_check_post_lock($postId) !== false) {
            return 'someone is editing this draft right now';
        }

        return null;
    }

    /**
     * The preview digest.
     *
     * @param string $editor   Editor value.
     * @param string $format   Builder format.
     * @param string $version  Builder version.
     * @param string $postType Post type.
     * @param string $title    The plain title.
     * @param string $treeJson The tree's canonical bytes.
     * @return string
     * @throws \JsonException When a value is not valid UTF-8.
     */
    public static function previewDigest(string $editor, string $format, string $version, string $postType, string $title, string $treeJson): string
    {
        return hash('sha256', json_encode([$editor, $format, $version, $postType, 'draft', $title, $treeJson], JSON_THROW_ON_ERROR));
    }

    /**
     * The format an elementor_format value builds, or null when it is not one.
     *
     * @param mixed $value elementor_format; absent means the site default.
     * @return string|null
     */
    private static function format(mixed $value): ?string
    {
        if ($value === null || $value === self::FORMAT_SITE_DEFAULT || $value === ElementorFacts::FORMAT_CLASSIC) {
            return ElementorFacts::FORMAT_CLASSIC;
        }

        return $value === ElementorFacts::FORMAT_ATOMIC ? ElementorFacts::FORMAT_ATOMIC : null;
    }

    /**
     * containers or sections, from the built tree's first element.
     *
     * @param array<mixed> $tree Built tree.
     * @return string
     */
    private static function layout(array $tree): string
    {
        $first = $tree[0] ?? null;

        return is_array($first) && ($first['elType'] ?? null) === 'container' ? self::LAYOUT_CONTAINERS : self::LAYOUT_SECTIONS;
    }

    /**
     * The revision ids a save's write scope recorded.
     *
     * @param mixed $scope The save's scope outcome.
     * @return list<int>
     */
    private static function scopeRevisions(mixed $scope): array
    {
        $ids = is_array($scope) ? ($scope['revision_ids'] ?? []) : [];
        $out = [];
        foreach (is_array($ids) ? $ids : [] as $id) {
            if (is_int($id) && $id > 0 && !in_array($id, $out, true)) {
                $out[] = $id;
            }
        }

        return $out;
    }

    /**
     * The own revision ids a ledger row holds, or null when they are not a
     * list of post ids.
     *
     * @param mixed $ids Stored ids.
     * @return list<int>|null
     */
    private static function ledgerRevisions(mixed $ids): ?array
    {
        if (!is_array($ids) || !ArrayShape::isList($ids) || count($ids) > self::MAX_OWN_REVISIONS) {
            return null;
        }
        foreach ($ids as $id) {
            if (!is_int($id) || $id < 1) {
                return null;
            }
        }

        return $ids;
    }

    /**
     * Trash the created draft, record the failure, and answer it.
     *
     * @param int                                  $postId       The created draft.
     * @param string                               $code         Refusal code.
     * @param string                               $detail       Reason, WPMgr's own words.
     * @param callable(array<string, mixed>): bool $ledgerUpdate Merges fields into the ledger row.
     * @return array<string, mixed>
     */
    private static function failTrashed(int $postId, string $code, string $detail, callable $ledgerUpdate): array
    {
        $trashed = self::trashOwn($postId);
        $result  = self::fail($code, $detail, ['post_id' => $postId, 'trashed' => $trashed]);
        $ledgerUpdate([
            'phase'      => 'failed',
            'undo_state' => $trashed ? 'trashed' : 'none',
            'result'     => $result,
        ]);

        return $result;
    }

    /**
     * Trash a post this request created (the ledger proves it), then confirm.
     *
     * @param int $postId Post id.
     * @return bool
     */
    private static function trashOwn(int $postId): bool
    {
        wp_trash_post($postId);
        clean_post_cache($postId);
        $after = get_post($postId);

        // With trash disabled core deletes outright; gone counts as undone.
        return !is_object($after) || (get_object_vars($after)['post_status'] ?? null) === 'trash';
    }

    /**
     * A precheck refusal.
     *
     * @param string $code   Refusal code.
     * @param string $detail Reason, WPMgr's own words.
     * @return array{refusal: array<string, mixed>, preview: array<string, mixed>, preview_digest: string, base_fingerprint: string, tree: array<mixed>, doc: null, title: string}
     */
    private static function refusedPrecheck(string $code, string $detail): array
    {
        return [
            'refusal'          => self::fail($code, $detail),
            'preview'          => [],
            'preview_digest'   => '',
            'base_fingerprint' => '',
            'tree'             => [],
            'doc'              => null,
            'title'            => '',
        ];
    }

    /**
     * A refusal in the command's shape.
     *
     * @param string               $code   Refusal code.
     * @param string               $detail Reason.
     * @param array<string, mixed> $extra  Extra fields.
     * @return array<string, mixed>
     */
    private static function fail(string $code, string $detail, array $extra = []): array
    {
        return [
            'ok'        => false,
            'outcome'   => 'refused',
            'code'      => $code,
            'detail'    => $detail,
            'retryable' => false,
        ] + $extra;
    }
}
