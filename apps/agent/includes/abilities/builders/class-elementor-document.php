<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\AbilitySideEffects;
use WPMgr\Agent\Abilities\AbilityWriteScope;
use WPMgr\Agent\Abilities\VersionCompare;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * One Elementor page that WPMgr creates: the checks on the target post, the
 * checks on a built element tree before anything is written, the save, and
 * the read-back.
 *
 * The page is stored only through Elementor's own document save, called with
 * the element tree and nothing else (never page settings), so Elementor keeps
 * its CSS, caches and revisions itself. Nothing here writes an Elementor row
 * directly.
 *
 * - precheckTree() refuses a tree Elementor would not store as built: an
 *   element or widget type that is not registered (page_has_unknown_elements),
 *   a string the save-time sanitiser would change (sanitiser_changed_new_content),
 *   or an element Elementor would save differently when it builds it without
 *   saving (builder_would_change_layout). That dry run writes nothing; a write
 *   seen during it refuses with side_effect_detected.
 * - targetProblem() names why a post may not take the save: it is not a
 *   draft, not an Elementor page, the active kit, the front page, the posts
 *   page or the shop page, or Elementor does not let the current user edit it.
 * - save() runs the save inside an AbilityWriteScope bound to the target,
 *   with a tripwire on the active kit's rows. Anything the save prints is
 *   discarded and the numeric locale is put back whatever the save does. A
 *   throw is builder_crashed, any answer but true is builder_save_refused, a
 *   kit change or a write outside the scope is side_effect_detected.
 * - verifyCreated() reads the stored rows back with SQL and requires the
 *   whole stored tree to equal the built one.
 *
 * Every refusal detail is WPMgr's own text; none quotes the site.
 */
final class ElementorDocument
{
    /** The element tree, JSON. */
    public const KEY_DATA = '_elementor_data';

    /** The edit mode; "builder" makes Elementor render the post. */
    public const KEY_EDIT_MODE = '_elementor_edit_mode';

    /** Page settings; a page WPMgr creates has none. */
    public const KEY_PAGE_SETTINGS = '_elementor_page_settings';

    /** Elementor's document type for the post. */
    public const KEY_TEMPLATE_TYPE = '_elementor_template_type';

    /** The post meta rows of an Elementor page, as the fingerprint covers them. */
    public const DESCRIPTOR_KEYS = [self::KEY_DATA, self::KEY_EDIT_MODE, self::KEY_PAGE_SETTINGS, self::KEY_TEMPLATE_TYPE];

    /** The marker on a post WPMgr created, holding the request id. */
    public const MARKER_KEY = '_wpmgr_created_by_request';

    /** The edit mode of a page Elementor renders. */
    public const EDIT_MODE_BUILDER = 'builder';

    /** Elementor's document type for a page. */
    public const TEMPLATE_PAGE = 'wp-page';

    /** Elementor's document type for a post. */
    public const TEMPLATE_POST = 'wp-post';

    /**
     * Options the save may write on every supported version. "{target}" is
     * the target post id, so each names that post's options only.
     */
    public const SAVE_OPTION_PATTERNS = [
        '_transient__elementor_editor_unsaved_{target}',
        '_transient_timeout__elementor_editor_unsaved_{target}',
        '_transient__elementor_mcp_mutation_{target}',
        '_transient_timeout__elementor_mcp_mutation_{target}',
    ];

    /**
     * Options the save may also write on a range of versions: lowest
     * version, highest version, option names. Before 3.35.9 the save may
     * write Elementor's asset cache option.
     */
    public const VERSIONED_OPTION_PATTERNS = [
        ['3.20.0', '3.35.8', ['_elementor_assets_data']],
    ];

    /** The active kit's rows the tripwire covers. */
    public const KIT_KEYS = [self::KEY_DATA, self::KEY_PAGE_SETTINGS];

    /** Deepest a tree is walked or decoded. */
    public const MAX_DEPTH = 64;

    /**
     * The first version known to store booleans and null as given. Before it,
     * the save may store every scalar of the tree as a string ("1" for true,
     * "" for false and null), and the read-back accepts exactly that form.
     */
    public const SCALARS_KEPT_FROM = '3.35.9';

    public const CODE_UNKNOWN = 'page_has_unknown_elements';

    public const CODE_SANITISER = 'sanitiser_changed_new_content';

    public const CODE_LAYOUT = 'builder_would_change_layout';

    public const CODE_REFUSED = 'builder_save_refused';

    public const CODE_CRASHED = 'builder_crashed';

    public const CODE_SIDE_EFFECT = 'side_effect_detected';

    /**
     * @param ElementorApi $api Elementor.
     */
    public function __construct(private readonly ElementorApi $api)
    {
    }

    /**
     * Elementor's document type for a page-create post type: wp-page for a
     * page, wp-post for every other type.
     *
     * @param string $postType Post type.
     * @return string
     */
    public static function templateType(string $postType): string
    {
        return $postType === 'page' ? self::TEMPLATE_PAGE : self::TEMPLATE_POST;
    }

    /**
     * The option patterns the save may write on the running Elementor.
     * An unknown version gets only the patterns of every version.
     *
     * @return list<string>
     */
    public function saveOptionPatterns(): array
    {
        $patterns = self::SAVE_OPTION_PATTERNS;
        $version  = $this->api->version();
        if ($version === null) {
            return $patterns;
        }
        foreach (self::VERSIONED_OPTION_PATTERNS as [$min, $max, $names]) {
            if (VersionCompare::inRange($version, $min, $max)) {
                array_push($patterns, ...$names);
            }
        }

        return $patterns;
    }

    /**
     * Why Elementor would not store $tree exactly as built, or null.
     *
     * @param array<mixed> $tree Top-level elements.
     * @return array{code: string, detail: string}|null
     */
    public function precheckTree(array $tree): ?array
    {
        if ($tree === [] || !array_is_list($tree)) {
            return ['code' => self::CODE_UNKNOWN, 'detail' => 'the page has no list of elements'];
        }
        $unknown = $this->unknownElement($tree, 'elements', 1);
        if ($unknown !== null) {
            return ['code' => self::CODE_UNKNOWN, 'detail' => $unknown];
        }
        $clean = $this->sanitised($tree);
        if ($clean === null) {
            return ['code' => self::CODE_SANITISER, 'detail' => 'Elementor\'s sanitiser could not be asked'];
        }
        if ($clean !== $tree) {
            return ['code' => self::CODE_SANITISER, 'detail' => 'Elementor\'s sanitiser would change the new content'];
        }

        return $this->dryRun($tree);
    }

    /**
     * Why the post may not take the save, as a short token, or null.
     *
     * @param int                  $postId Target post.
     * @param array<string, mixed> $facts  From ElementorFacts::collect().
     * @return string|null
     */
    public function targetProblem(int $postId, array $facts): ?string
    {
        if ($postId < 1) {
            return 'post_missing';
        }
        try {
            $stored = BuilderDocumentFingerprint::read($postId, [self::KEY_EDIT_MODE]);
        } catch (\Throwable $e) {
            return 'post_unreadable';
        }
        if ($stored === null) {
            return 'post_missing';
        }
        if ($stored['post']['post_status'] !== 'draft') {
            return 'not_draft';
        }
        if (($stored['rows'][self::KEY_EDIT_MODE] ?? []) !== [self::EDIT_MODE_BUILDER]) {
            return 'not_builder_page';
        }
        $kits = [self::intOf($facts['active_kit_id'] ?? 0), $this->api->activeKitId()];
        if (in_array($postId, $kits, true)) {
            return 'active_kit';
        }
        if (self::intOf(get_option('page_on_front', 0)) === $postId) {
            return 'front_page';
        }
        if (self::intOf(get_option('page_for_posts', 0)) === $postId) {
            return 'posts_page';
        }
        if (function_exists('wc_get_page_id') && self::intOf(wc_get_page_id('shop')) === $postId) {
            return 'shop_page';
        }

        $document = $this->api->document($postId);
        if ($document === null) {
            return 'document_missing';
        }
        $nameOf     = [$document, 'get_name'];
        $editableBy = [$document, 'is_editable_by_current_user'];
        try {
            $name     = is_callable($nameOf) ? $nameOf() : null;
            $editable = is_callable($editableBy) ? $editableBy() : null;
        } catch (\Throwable $e) {
            return 'not_editable';
        }
        if (!in_array($name, [self::TEMPLATE_PAGE, self::TEMPLATE_POST], true)) {
            return 'document_type';
        }

        return $editable === true ? null : 'not_editable';
    }

    /**
     * Save $tree on the post through Elementor's document save.
     *
     * "scope" is the write scope's outcome, its revision_ids being the
     * revisions this save made; null when the save was not reached.
     *
     * @param int          $postId Target post, checked by targetProblem().
     * @param array<mixed> $tree   Element tree, checked by precheckTree().
     * @param int          $kitId  The active kit's post id; 0 when there is none.
     * @return array{ok: bool, code?: string, detail?: string, scope: array<string, mixed>|null}
     */
    public function save(int $postId, array $tree, int $kitId): array
    {
        $document = $postId > 0 ? $this->api->document($postId) : null;
        $save     = $document === null ? null : [$document, 'save'];
        if ($save === null || !is_callable($save)) {
            return ['ok' => false, 'code' => self::CODE_REFUSED, 'detail' => 'Elementor has no document for the page', 'scope' => null];
        }
        try {
            $kitBefore = self::kitDigest($kitId);
        } catch (\Throwable $e) {
            return ['ok' => false, 'code' => self::CODE_REFUSED, 'detail' => 'the active kit could not be read', 'scope' => null];
        }

        $scope   = new AbilityWriteScope($postId, $this->saveOptionPatterns());
        $locale  = setlocale(LC_NUMERIC, '0');
        $level   = ob_get_level();
        $saved   = null;
        $crashed = false;
        ob_start();
        try {
            $saved = $scope->run(static fn () => $save(['elements' => $tree]));
        } catch (\Throwable $e) {
            $crashed = true;
        } finally {
            self::closeBuffers($level);
            if (is_string($locale)) {
                setlocale(LC_NUMERIC, $locale);
            }
        }
        $outcome = $scope->outcome();

        try {
            $kitChanged = self::kitDigest($kitId) !== $kitBefore;
        } catch (\Throwable $e) {
            $kitChanged = true;
        }
        if ($kitChanged) {
            return ['ok' => false, 'code' => self::CODE_SIDE_EFFECT, 'detail' => 'the active kit changed during the save', 'scope' => $outcome];
        }
        if ($outcome['clean'] !== true) {
            return ['ok' => false, 'code' => self::CODE_SIDE_EFFECT, 'detail' => 'the save wrote outside the page: ' . implode(', ', $outcome['violations']), 'scope' => $outcome];
        }
        if ($crashed) {
            return ['ok' => false, 'code' => self::CODE_CRASHED, 'detail' => 'Elementor failed while saving the page', 'scope' => $outcome];
        }
        if ($saved !== true) {
            return ['ok' => false, 'code' => self::CODE_REFUSED, 'detail' => 'Elementor did not save the page', 'scope' => $outcome];
        }

        return ['ok' => true, 'scope' => $outcome];
    }

    /**
     * Why the created page is not exactly what was built, as a short token,
     * or null. The rows are read with SQL, never from a cache.
     *
     * @param int          $postId    The created post.
     * @param array<mixed> $tree      The tree that was saved.
     * @param int          $principal The WPMgr service user the page was created as.
     * @param string       $requestId The request that created the page.
     * @param string       $postType  The post type that was created.
     * @return string|null
     */
    public function verifyCreated(int $postId, array $tree, int $principal, string $requestId, string $postType): ?string
    {
        if ($postId < 1) {
            return 'post_missing';
        }
        try {
            $stored = BuilderDocumentFingerprint::read($postId, array_merge(self::DESCRIPTOR_KEYS, [self::MARKER_KEY]));
        } catch (\Throwable $e) {
            return 'post_unreadable';
        }
        if ($stored === null) {
            return 'post_missing';
        }
        $rows = $stored['rows'];
        if ($stored['post']['post_status'] !== 'draft') {
            return 'not_draft';
        }
        if ($stored['post']['post_type'] !== $postType) {
            return 'post_type';
        }
        clean_post_cache($postId);
        $post = get_post($postId);
        $vars = is_object($post) ? get_object_vars($post) : [];
        if ($principal < 1 || self::intOf($vars['post_author'] ?? 0) !== $principal) {
            return 'author';
        }
        if (!array_key_exists('post_parent', $vars) || self::intOf($vars['post_parent']) !== 0) {
            return 'parent';
        }
        if (($rows[self::KEY_EDIT_MODE] ?? []) !== [self::EDIT_MODE_BUILDER]) {
            return 'edit_mode';
        }
        if (($rows[self::KEY_TEMPLATE_TYPE] ?? []) !== [self::templateType($postType)]) {
            return 'template_type';
        }
        if (isset($rows[self::KEY_PAGE_SETTINGS])) {
            return 'page_settings';
        }
        if ($requestId === '' || ($rows[self::MARKER_KEY] ?? []) !== [$requestId]) {
            return 'marker';
        }
        $data = $rows[self::KEY_DATA] ?? [];
        if (count($data) !== 1) {
            return 'data_missing';
        }
        $decoded = json_decode($data[0], true, self::MAX_DEPTH);
        if (!is_array($decoded)) {
            return 'data_not_json';
        }
        if (!$this->storedAsBuilt($decoded, $tree)) {
            return 'tree_differs';
        }
        if (LeafPolicy::checkTree($tree, ElementorClassicMapper::ALLOWED_KEYS, ElementorClassicMapper::LEAF_RULES) !== null) {
            return 'leaf_policy';
        }

        return null;
    }

    /**
     * The path of the first element whose type is not registered, or null.
     *
     * @param array<mixed> $nodes Elements.
     * @param string       $path  Their path.
     * @param int          $depth Nesting depth of $nodes.
     * @return string|null
     */
    private function unknownElement(array $nodes, string $path, int $depth): ?string
    {
        if ($depth > self::MAX_DEPTH) {
            return $path . ': nested too deep';
        }
        foreach ($nodes as $i => $node) {
            $at = $path . '[' . (is_int($i) ? $i : '?') . ']';
            if (!is_array($node)) {
                return $at . ': not an element';
            }
            $type = $node['elType'] ?? null;
            if (!is_string($type) || $type === '') {
                return $at . ': no element type';
            }
            if ($type === 'widget') {
                $widget = $node['widgetType'] ?? null;
                if (!is_string($widget) || !$this->api->widgetTypeExists($widget)) {
                    return $at . ': widget type not registered';
                }
            } elseif (!$this->api->elementTypeExists($type)) {
                return $at . ': element type not registered';
            }
            $children = $node['elements'] ?? [];
            if (!is_array($children) || !array_is_list($children)) {
                return $at . '.elements: not a list of elements';
            }
            $inner = $this->unknownElement($children, $at . '.elements', $depth + 1);
            if ($inner !== null) {
                return $inner;
            }
        }

        return null;
    }

    /**
     * $tree after the sanitiser the save applies for a user without
     * unfiltered_html. Asked of Elementor where it offers that sanitiser;
     * where it does not, the save applies WordPress's wp_kses_post() to every
     * string, so that is asked. Null when neither can be asked.
     *
     * @param array<mixed> $tree Element tree.
     * @return array<mixed>|null
     */
    private function sanitised(array $tree): ?array
    {
        $clean = $this->api->ksesPostDeep($tree);
        if ($clean !== null) {
            return $clean;
        }
        if (!$this->api->loaded() || !function_exists('wp_kses_post')) {
            return null;
        }

        return self::ksesStrings($tree, 1);
    }

    /**
     * Every string of $data through wp_kses_post(); other values as they are.
     *
     * @param array<mixed> $data  Data.
     * @param int          $depth Nesting depth of $data.
     * @return array<mixed>|null Null when nested too deep.
     */
    private static function ksesStrings(array $data, int $depth): ?array
    {
        if ($depth > self::MAX_DEPTH) {
            return null;
        }
        $out = [];
        foreach ($data as $key => $value) {
            if (is_array($value)) {
                $value = self::ksesStrings($value, $depth + 1);
                if ($value === null) {
                    return null;
                }
            } elseif (is_string($value)) {
                $value = wp_kses_post($value);
            }
            $out[$key] = $value;
        }

        return $out;
    }

    /**
     * Build every top-level element as Elementor does on save, without
     * saving, and compare what it would store with the element, its whole
     * subtree included. Nothing may be written while it runs.
     *
     * @param list<mixed> $tree Element tree.
     * @return array{code: string, detail: string}|null
     */
    private function dryRun(array $tree): ?array
    {
        $recorder = new AbilitySideEffects([], []);
        $problem  = null;
        $level    = ob_get_level();
        ob_start();
        try {
            $recorder->arm();
            foreach ($tree as $i => $node) {
                $element = is_array($node) ? $this->api->createElementInstance($node) : null;
                $build   = $element === null ? null : [$element, 'get_data_for_save'];
                if ($build === null || !is_callable($build)) {
                    $problem = 'elements[' . $i . ']: Elementor could not build the element';
                    break;
                }
                if ($build() !== $node) {
                    $problem = 'elements[' . $i . ']: Elementor would store the element differently';
                    break;
                }
            }
        } catch (\Throwable $e) {
            $problem = 'Elementor failed while building the elements';
        } finally {
            $recorder->disarm();
            self::closeBuffers($level);
        }
        if ($recorder->detected()) {
            return ['code' => self::CODE_SIDE_EFFECT, 'detail' => 'building the elements without saving wrote to the site'];
        }

        return $problem === null ? null : ['code' => self::CODE_LAYOUT, 'detail' => $problem];
    }

    /**
     * Whether the stored tree is the built tree: equal, key order and types
     * included; or, before SCALARS_KEPT_FROM, equal to the built tree with
     * every scalar in the string form those versions store.
     *
     * @param array<mixed> $stored Decoded stored tree.
     * @param array<mixed> $tree   Built tree.
     * @return bool
     */
    private function storedAsBuilt(array $stored, array $tree): bool
    {
        if ($stored === $tree) {
            return true;
        }
        $version = $this->api->version();
        if ($version === null || VersionCompare::compare($version, self::SCALARS_KEPT_FROM) >= 0) {
            return false;
        }

        return $stored === self::scalarsAsStrings($tree);
    }

    /**
     * $data with every scalar as the string the older sanitiser stores.
     *
     * @param array<mixed> $data Data.
     * @return array<mixed>
     */
    private static function scalarsAsStrings(array $data): array
    {
        $out = [];
        foreach ($data as $key => $value) {
            if (is_array($value)) {
                $value = self::scalarsAsStrings($value);
            } elseif (is_bool($value)) {
                $value = $value ? '1' : '';
            } elseif ($value === null) {
                $value = '';
            } elseif (is_int($value) || is_float($value)) {
                $value = (string) $value;
            }
            $out[$key] = $value;
        }

        return $out;
    }

    /**
     * The fingerprint of the kit's post and KIT_KEYS rows; null without a kit.
     *
     * @param int $kitId Kit post id.
     * @return string|null
     * @throws \RuntimeException When the database cannot answer.
     */
    private static function kitDigest(int $kitId): ?string
    {
        if ($kitId < 1) {
            return null;
        }
        $stored = BuilderDocumentFingerprint::read($kitId, self::KIT_KEYS);
        if ($stored === null) {
            return null;
        }

        return BuilderDocumentFingerprint::compute($stored['post'], $stored['rows'], self::KIT_KEYS);
    }

    /**
     * Discard every output buffer opened above $level.
     *
     * @param int $level Buffer level to return to.
     * @return void
     */
    private static function closeBuffers(int $level): void
    {
        while (ob_get_level() > $level) {
            ob_end_clean();
        }
    }

    /**
     * A non-negative int from an int or a decimal string, else 0.
     *
     * @param mixed $value Value.
     * @return int
     */
    private static function intOf(mixed $value): int
    {
        if (is_int($value)) {
            return max(0, $value);
        }
        if (is_string($value) && preg_match('/^[0-9]{1,18}$/D', $value) === 1) {
            return (int) $value;
        }

        return 0;
    }
}
