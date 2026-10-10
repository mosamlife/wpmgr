<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\AbilitySideEffects;
use WPMgr\Agent\Abilities\AbilityWriteScope;
use WPMgr\Agent\Abilities\VersionCompare;
use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * One Elementor page that WPMgr creates or edits: the checks on the target
 * post, the checks on a built or edited element tree before anything is
 * written, the save, and the read-back.
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
 *   with a tripwire on the active kit's rows. Besides the target and the
 *   options saveOptionPatterns() names, the save may write Elementor's
 *   style-cache validity options, write by write, only where the write
 *   changes nothing but the target's own entry (ElementorStyleCache). Anything
 *   the save prints is discarded and the numeric locale is put back whatever
 *   the save does. A throw is builder_crashed, any answer but true is
 *   builder_save_refused, a kit change or a write outside the scope is
 *   side_effect_detected.
 * - verifyCreated() reads the stored rows back with SQL and requires the
 *   whole stored tree to equal the built one.
 * - editTargetProblem() adds to targetProblem() what an edit needs: a page
 *   or a post, no autosave by anyone, no edit lock held by someone else, and
 *   one stored element tree that can be read as a list.
 * - precheckEditTree() refuses an edit Elementor would not store as planned:
 *   content already on the page that the save would strip, an element type
 *   that is not registered, new content the sanitiser would change, or a
 *   node the edit made or changed that Elementor, building it without
 *   saving, would store differently. A node the edit did not touch is never
 *   built.
 * - verifyEdited() reads the edited page back with SQL and requires the
 *   whole stored tree to equal the planned one, and the post and page rows
 *   the edit does not write to be as the snapshot holds them.
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

    /** Content on the page that the save, as a user without unfiltered_html, would strip. */
    public const CODE_ADMIN_ONLY = 'page_has_admin_only_content';

    /** The post types an edited page may be. */
    public const EDIT_POST_TYPES = ['page', 'post'];

    /** editTargetProblem(): someone has an autosave of the page. */
    public const TARGET_AUTOSAVE = 'autosave_pending';

    /** editTargetProblem(): someone else holds the page's edit lock. */
    public const TARGET_LOCKED = 'editor_open';

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
     * An unknown version gets only the patterns of every version. The
     * style-cache validity options are not among them: the save's scope
     * checks each write of those (saveOptionChecks()).
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
     * The value checks of the save's scope, on every Elementor version: an
     * option whose name starts with ElementorStyleCache::PREFIX is the save's
     * own only where ElementorStyleCache::ownWrite() admits the write.
     *
     * @return array<string, callable(mixed, mixed, int): bool>
     */
    public static function saveOptionChecks(): array
    {
        return [ElementorStyleCache::PREFIX => [ElementorStyleCache::class, 'ownWrite']];
    }

    /**
     * Why Elementor would not store $tree exactly as built, or null.
     *
     * @param array<mixed> $tree Top-level elements.
     * @return array{code: string, detail: string}|null
     */
    public function precheckTree(array $tree): ?array
    {
        if ($tree === [] || !ArrayShape::isList($tree)) {
            return ['code' => self::CODE_UNKNOWN, 'detail' => 'the page has no list of elements'];
        }
        $unknown = $this->unknownElement($tree, 'elements', 1);
        if ($unknown !== null) {
            return ['code' => self::CODE_UNKNOWN, 'detail' => $unknown];
        }
        $clean = $this->api->ksesPostDeep($tree);
        if ($clean === null) {
            return ['code' => self::CODE_SANITISER, 'detail' => 'Elementor\'s sanitiser could not be asked'];
        }
        if ($clean !== $tree) {
            return ['code' => self::CODE_SANITISER, 'detail' => 'Elementor\'s sanitiser would change the new content'];
        }

        return $this->dryRun($tree);
    }

    /**
     * Why Elementor would not store an edited page as planned, or null.
     *
     * $currentTree is the page's stored tree before the edit, $newTree the
     * tree after it, and $touched the ids of the nodes the edit made or whose
     * text it changed (LayoutOps' touched). In order:
     *
     * - both trees are lists, and every element of the current page is a
     *   registered type (page_has_unknown_elements);
     * - the whole current page is what Elementor's sanitiser leaves it: the
     *   save sanitises every string of the page for a user without
     *   unfiltered_html, so content that only a user with it may save would
     *   not survive the edit (page_has_admin_only_content);
     * - every touched node, with what it holds, is a registered type
     *   (page_has_unknown_elements, the detail prefixed "after the edit") and
     *   what the sanitiser leaves it (sanitiser_changed_new_content);
     * - Elementor builds each touched node without saving, and would store it
     *   as planned (builder_would_change_layout). A node inside a touched
     *   node is built with it; a node the edit did not touch is never built.
     *   A write while building refuses with side_effect_detected.
     *
     * A sanitiser that cannot be asked refuses with
     * sanitiser_changed_new_content.
     *
     * @param array<mixed> $currentTree The stored tree before the edit.
     * @param array<mixed> $newTree     The tree after the edit.
     * @param array<mixed> $touched     Ids of the nodes made or changed.
     * @return array{code: string, detail: string}|null
     */
    public function precheckEditTree(array $currentTree, array $newTree, array $touched): ?array
    {
        if (!ArrayShape::isList($currentTree) || !ArrayShape::isList($newTree)) {
            return ['code' => self::CODE_UNKNOWN, 'detail' => 'the page has no list of elements'];
        }
        $unknown = $this->unknownElement($currentTree, 'elements', 1);
        if ($unknown !== null) {
            return ['code' => self::CODE_UNKNOWN, 'detail' => $unknown];
        }
        $clean = $this->api->ksesPostDeep($currentTree);
        if ($clean === null) {
            return ['code' => self::CODE_SANITISER, 'detail' => 'Elementor\'s sanitiser could not be asked'];
        }
        if ($clean !== $currentTree) {
            return ['code' => self::CODE_ADMIN_ONLY, 'detail' => 'Elementor\'s sanitiser would change content already on the page'];
        }

        $wanted = [];
        foreach ($touched as $id) {
            if (is_string($id) && $id !== '') {
                $wanted[$id] = true;
            }
        }
        $found = [];
        if (!self::touchedNodes($newTree, $wanted, 'elements', 1, $found)) {
            return ['code' => self::CODE_UNKNOWN, 'detail' => 'after the edit, the page is nested too deep'];
        }
        if ($found === []) {
            return null;
        }
        $nodes = [];
        foreach ($found as $at => [$node, $depth]) {
            $unknown = $this->unknownNode($node, $at, $depth);
            if ($unknown !== null) {
                return ['code' => self::CODE_UNKNOWN, 'detail' => 'after the edit, ' . $unknown];
            }
            $nodes[$at] = $node;
        }
        $clean = $this->api->ksesPostDeep(array_values($nodes));
        if ($clean === null) {
            return ['code' => self::CODE_SANITISER, 'detail' => 'Elementor\'s sanitiser could not be asked'];
        }
        if ($clean !== array_values($nodes)) {
            return ['code' => self::CODE_SANITISER, 'detail' => 'Elementor\'s sanitiser would change the new content'];
        }

        return $this->dryRunNodes($nodes);
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
     * Why the post may not take an edit, as a short token, or null.
     *
     * Every targetProblem() reason first, then, in order: the post is not a
     * page or a post (post_type); anyone has an autosave of it, or
     * Elementor's document reports a newer autosave (TARGET_AUTOSAVE;
     * autosave_unreadable when Elementor fails to answer); someone else holds
     * its edit lock (TARGET_LOCKED; lock_unreadable when that cannot be
     * checked); its _elementor_data is not exactly one row that decodes, at
     * most MAX_DEPTH deep, to a list (LayoutOps::CODE_UNREADABLE). The rows
     * are read with SQL.
     *
     * @param int                  $postId Target post.
     * @param array<string, mixed> $facts  From ElementorFacts::collect().
     * @return string|null
     */
    public function editTargetProblem(int $postId, array $facts): ?string
    {
        $problem = $this->targetProblem($postId, $facts);
        if ($problem !== null) {
            return $problem;
        }
        try {
            $stored = BuilderDocumentFingerprint::read($postId, [self::KEY_DATA]);
        } catch (\Throwable $e) {
            return 'post_unreadable';
        }
        if ($stored === null) {
            return 'post_missing';
        }
        if (!in_array($stored['post']['post_type'], self::EDIT_POST_TYPES, true)) {
            return 'post_type';
        }

        $open = $this->openProblem($postId);
        if ($open !== null) {
            return $open;
        }

        return self::treeOf($stored['rows'][self::KEY_DATA] ?? []) === null ? LayoutOps::CODE_UNREADABLE : null;
    }

    /**
     * Whether someone has the post open or unsaved changes to it, as a short
     * token, or null. In order: anyone has an autosave of it, or Elementor's
     * document reports a newer autosave (TARGET_AUTOSAVE; autosave_unreadable
     * when Elementor fails to answer); someone other than the current user
     * holds its edit lock (TARGET_LOCKED; lock_unreadable when that cannot
     * be checked).
     *
     * @param int $postId Target post.
     * @return string|null
     */
    public function openProblem(int $postId): ?string
    {
        // User id 0 (the int) means an autosave by any user.
        if (wp_get_post_autosave($postId, 0) !== false) {
            return self::TARGET_AUTOSAVE;
        }
        $document = $this->api->document($postId);
        $newer    = $document === null ? null : [$document, 'get_newer_autosave'];
        if ($newer !== null && is_callable($newer)) {
            try {
                $autosave = $newer();
            } catch (\Throwable $e) {
                return 'autosave_unreadable';
            }
            if ($autosave !== false && $autosave !== null) {
                return self::TARGET_AUTOSAVE;
            }
        }

        if (!function_exists('wp_check_post_lock') && defined('ABSPATH') && is_readable(ABSPATH . 'wp-admin/includes/post.php')) {
            require_once ABSPATH . 'wp-admin/includes/post.php';
        }
        if (!function_exists('wp_check_post_lock')) {
            return 'lock_unreadable';
        }

        return wp_check_post_lock($postId) !== false ? self::TARGET_LOCKED : null;
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

        $scope   = new AbilityWriteScope($postId, $this->saveOptionPatterns(), self::saveOptionChecks());
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
     * Why the edited page is not exactly what was planned, as a short token,
     * or null. The rows are read with SQL, never from a cache.
     *
     * $before is the page's snapshot taken before the save, as
     * BuilderDocumentSnapshot::decode() gives it. In order: the snapshot is
     * of this post and holds one stored element tree (snapshot_unreadable);
     * the post is still a draft (not_draft), and its type, author, parent and
     * slug are the snapshot's (post_type, author, parent, post_name); the
     * edit mode is builder (edit_mode); the document type rows are the
     * snapshot's and name a page or a post (template_type); the page settings
     * rows are the snapshot's, byte for byte (page_settings); there is one
     * _elementor_data row (data_missing) that decodes (data_not_json) to the
     * whole planned tree, untouched nodes included, by the rule verifyCreated()
     * uses (tree_differs). Last, what the edit wrote passes the key, pin and
     * stored-string rules (leaf_policy): a node the edit made, whole; a node
     * it changed, the settings it changed, since the rest of the node carries
     * what a person set. A node equal to the node of its id in the snapshot
     * was not written.
     *
     * @param int                  $postId  The edited post.
     * @param array<mixed>         $newTree The tree that was saved.
     * @param array<string, mixed> $before  The page's decoded snapshot.
     * @return string|null
     */
    public function verifyEdited(int $postId, array $newTree, array $before): ?string
    {
        if ($postId < 1) {
            return 'post_missing';
        }
        $snapshot = self::snapshotFacts($before, $postId);
        if ($snapshot === null) {
            return 'snapshot_unreadable';
        }
        try {
            $stored = BuilderDocumentFingerprint::read($postId, self::DESCRIPTOR_KEYS);
            $author = $stored === null ? null : self::storedAuthor($postId);
        } catch (\Throwable $e) {
            return 'post_unreadable';
        }
        if ($stored === null || $author === null) {
            return 'post_missing';
        }
        $post = $stored['post'];
        $rows = $stored['rows'];
        if ($post['post_status'] !== 'draft') {
            return 'not_draft';
        }
        if ($post['post_type'] !== $snapshot['post']['post_type']) {
            return 'post_type';
        }
        if ($author !== $snapshot['post']['post_author']) {
            return 'author';
        }
        if ((string) $post['post_parent'] !== $snapshot['post']['post_parent']) {
            return 'parent';
        }
        if ($post['post_name'] !== $snapshot['post']['post_name']) {
            return 'post_name';
        }
        if (($rows[self::KEY_EDIT_MODE] ?? []) !== [self::EDIT_MODE_BUILDER]) {
            return 'edit_mode';
        }
        $template = $rows[self::KEY_TEMPLATE_TYPE] ?? [];
        if ($template !== $snapshot['rows'][self::KEY_TEMPLATE_TYPE] || !in_array($template, [[self::TEMPLATE_PAGE], [self::TEMPLATE_POST]], true)) {
            return 'template_type';
        }
        if (($rows[self::KEY_PAGE_SETTINGS] ?? []) !== $snapshot['rows'][self::KEY_PAGE_SETTINGS]) {
            return 'page_settings';
        }
        $data = $rows[self::KEY_DATA] ?? [];
        if (count($data) !== 1) {
            return 'data_missing';
        }
        $decoded = json_decode($data[0], true, self::MAX_DEPTH);
        if (!is_array($decoded)) {
            return 'data_not_json';
        }
        if (!$this->storedAsBuilt($decoded, $newTree)) {
            return 'tree_differs';
        }
        $earlier = [];
        if (!self::indexOwn($snapshot['tree'], 1, $earlier)) {
            return 'snapshot_unreadable';
        }
        if (self::writtenProblem($newTree, $earlier, 1) !== null) {
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
            $inner = $this->unknownNode($node, $path . '[' . (is_int($i) ? $i : '?') . ']', $depth);
            if ($inner !== null) {
                return $inner;
            }
        }

        return null;
    }

    /**
     * The path of the first element, $node or one inside it, whose type is
     * not registered, or null.
     *
     * @param mixed  $node  One element.
     * @param string $at    Its path.
     * @param int    $depth Its nesting depth.
     * @return string|null
     */
    private function unknownNode(mixed $node, string $at, int $depth): ?string
    {
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
        if (!is_array($children) || !ArrayShape::isList($children)) {
            return $at . '.elements: not a list of elements';
        }

        return $this->unknownElement($children, $at . '.elements', $depth + 1);
    }

    /**
     * The touched nodes of a tree, outermost only, by path, each with its
     * nesting depth. False when the walk goes deeper than MAX_DEPTH.
     *
     * @param array<mixed>                                 $nodes   Elements.
     * @param array<string, true>                          $touched Ids of the nodes made or changed.
     * @param string                                       $path    Path of $nodes.
     * @param int                                          $depth   Nesting depth of $nodes.
     * @param array<string, array{0: array<mixed>, 1: int}> $found   Gets the nodes found.
     * @return bool
     */
    private static function touchedNodes(array $nodes, array $touched, string $path, int $depth, array &$found): bool
    {
        if ($depth > self::MAX_DEPTH) {
            return false;
        }
        foreach ($nodes as $i => $node) {
            if (!is_array($node)) {
                continue;
            }
            $at = $path . '[' . (is_int($i) ? $i : '?') . ']';
            $id = $node['id'] ?? null;
            if (is_string($id) && isset($touched[$id])) {
                $found[$at] = [$node, $depth];
                continue;
            }
            $children = $node['elements'] ?? [];
            if (is_array($children) && !self::touchedNodes($children, $touched, $at . '.elements', $depth + 1, $found)) {
                return false;
            }
        }

        return true;
    }

    /**
     * The element tree one stored _elementor_data row holds: exactly one
     * row, decoding at most MAX_DEPTH deep to a list; else null.
     *
     * @param array<mixed> $rows The rows of the key, in meta_id order.
     * @return array<mixed>|null
     */
    public static function treeOf(array $rows): ?array
    {
        if (count($rows) !== 1 || !is_string($rows[0] ?? null)) {
            return null;
        }
        $tree = json_decode($rows[0], true, self::MAX_DEPTH);

        return is_array($tree) && ArrayShape::isList($tree) ? $tree : null;
    }

    /**
     * What verifyEdited() needs of a decoded snapshot: the post's type,
     * author, parent and slug as stored, the rows of the descriptor keys in
     * meta_id order, and the stored element tree. Null when the snapshot is
     * not of $postId or is not shaped as decode() gives it.
     *
     * @param array<string, mixed> $before Decoded snapshot.
     * @param int                  $postId The post it must be of.
     * @return array{post: array<string, string>, rows: array<string, list<string|null>>, tree: array<mixed>}|null
     */
    private static function snapshotFacts(array $before, int $postId): ?array
    {
        if (($before['post_id'] ?? null) !== $postId || !is_array($before['post'] ?? null) || !is_array($before['meta'] ?? null)) {
            return null;
        }
        $post = [];
        foreach (['post_type', 'post_author', 'post_parent', 'post_name'] as $column) {
            $value = $before['post'][$column] ?? null;
            if (!is_string($value)) {
                return null;
            }
            $post[$column] = $value;
        }
        $rows = array_fill_keys(self::DESCRIPTOR_KEYS, []);
        foreach ($before['meta'] as $pair) {
            if (!is_array($pair) || !is_string($pair[0] ?? null) || !array_key_exists(1, $pair) || ($pair[1] !== null && !is_string($pair[1]))) {
                return null;
            }
            if (array_key_exists($pair[0], $rows)) {
                $rows[$pair[0]][] = $pair[1];
            }
        }
        $tree = self::treeOf($rows[self::KEY_DATA]);

        return $tree === null ? null : ['post' => $post, 'rows' => $rows, 'tree' => $tree];
    }

    /**
     * The post's stored author as decimal text, read with SQL; null when
     * there is no such post.
     *
     * @param int $postId Post id.
     * @return string|null
     * @throws \RuntimeException When the database cannot answer, or answers with something that is not an author id.
     */
    private static function storedAuthor(int $postId): ?string
    {
        global $wpdb;
        if (!is_object($wpdb)) {
            throw new \RuntimeException('database handle unavailable');
        }
        /** @var \wpdb $wpdb */
        $row = $wpdb->get_row($wpdb->prepare('SELECT post_author FROM %i WHERE ID = %d', $wpdb->posts, $postId), ARRAY_A); // phpcs:ignore WordPress.DB.DirectDatabaseQuery.DirectQuery,WordPress.DB.DirectDatabaseQuery.NoCaching -- the read-back compares the stored row with the snapshot, so it is read uncached; table via the %i identifier placeholder (WP 6.2+)
        if ($wpdb->last_error !== '') {
            throw new \RuntimeException('database read failed');
        }
        if ($row === null) {
            return null;
        }
        $author = is_array($row) ? ($row['post_author'] ?? null) : null;
        if (is_int($author) && $author >= 0) {
            return (string) $author;
        }
        if (is_string($author) && preg_match('/^(?:0|[1-9][0-9]{0,18})$/D', $author) === 1) {
            return $author;
        }
        throw new \RuntimeException('database read failed');
    }

    /**
     * Every node of a tree by id, each without its children, in tree order;
     * a repeated id keeps every version. False when the tree is deeper than
     * MAX_DEPTH.
     *
     * @param array<mixed>                       $nodes Elements.
     * @param int                                $depth Nesting depth of $nodes.
     * @param array<string, list<array<mixed>>> $index Gets the nodes.
     * @return bool
     */
    private static function indexOwn(array $nodes, int $depth, array &$index): bool
    {
        if ($depth > self::MAX_DEPTH) {
            return false;
        }
        foreach ($nodes as $node) {
            if (!is_array($node)) {
                continue;
            }
            $own = $node;
            unset($own['elements']);
            if (is_string($node['id'] ?? null)) {
                $index[$node['id']][] = $own;
            }
            $children = $node['elements'] ?? [];
            if (is_array($children) && !self::indexOwn($children, $depth + 1, $index)) {
                return false;
            }
        }

        return true;
    }

    /**
     * The first refusal of the key, pin and stored-string rules over what an
     * edit wrote into $nodes, or null. A node whose own data (children left
     * out) equals a node of the same id before the edit was not written; any
     * other node was, and is checked through writtenPart().
     *
     * @param array<mixed>                       $nodes   Elements of the planned tree.
     * @param array<string, list<array<mixed>>> $earlier The nodes before the edit, by id.
     * @param int                                $depth   Nesting depth of $nodes.
     * @return array{code: string, detail: string}|null
     */
    private static function writtenProblem(array $nodes, array $earlier, int $depth): ?array
    {
        if ($depth > self::MAX_DEPTH) {
            return ['code' => LeafPolicy::CODE_KEY, 'detail' => 'elements nested too deep'];
        }
        foreach ($nodes as $node) {
            if (!is_array($node)) {
                return ['code' => LeafPolicy::CODE_KEY, 'detail' => 'not an element'];
            }
            $own = $node;
            unset($own['elements']);
            $versions = is_string($own['id'] ?? null) ? ($earlier[$own['id']] ?? []) : [];
            if (!in_array($own, $versions, true)) {
                $problem = LeafPolicy::checkTree([self::writtenPart($own, $versions)], ElementorClassicMapper::ALLOWED_KEYS, ElementorClassicMapper::LEAF_RULES);
                if ($problem !== null) {
                    return $problem;
                }
            }
            $children = $node['elements'] ?? [];
            $inner    = is_array($children) ? self::writtenProblem($children, $earlier, $depth + 1) : null;
            if ($inner !== null) {
                return $inner;
            }
        }

        return null;
    }

    /**
     * The part of a written node the rules are held to, as a node without
     * children. A changed node, one with exactly one earlier version that
     * differs from it in its settings only: its id and types with the
     * settings that differ. Any other node is new: all of it.
     *
     * @param array<mixed>       $own      The node without its children.
     * @param list<array<mixed>> $versions The earlier versions of its id.
     * @return array<mixed>
     */
    private static function writtenPart(array $own, array $versions): array
    {
        $whole = $own + ['elements' => []];
        if (count($versions) !== 1) {
            return $whole;
        }
        $was  = $versions[0];
        $now  = $own['settings'] ?? [];
        $then = $was['settings'] ?? [];
        unset($own['settings'], $was['settings']);
        if ($own !== $was || !is_array($now) || !is_array($then)) {
            return $whole;
        }

        return array_intersect_key($own, ['id' => true, 'elType' => true, 'widgetType' => true])
            + ['settings' => self::settingsWritten($then, $now), 'elements' => []];
    }

    /**
     * The settings of $now that differ from $then: a key that is new or
     * holds another value, walked into where both hold an array. A key
     * $now no longer has, or members only reordered, write nothing.
     *
     * @param array<mixed> $then Settings before.
     * @param array<mixed> $now  Settings after.
     * @return array<mixed>
     */
    private static function settingsWritten(array $then, array $now): array
    {
        $written = [];
        foreach ($now as $key => $value) {
            $had = array_key_exists($key, $then);
            if ($had && $then[$key] === $value) {
                continue;
            }
            if ($had && is_array($value) && $value !== [] && is_array($then[$key])) {
                $inner = self::settingsWritten($then[$key], $value);
                if ($inner !== []) {
                    $written[$key] = $inner;
                }
                continue;
            }
            $written[$key] = $value;
        }

        return $written;
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
        $nodes = [];
        foreach ($tree as $i => $node) {
            $nodes['elements[' . $i . ']'] = $node;
        }

        return $this->dryRunNodes($nodes);
    }

    /**
     * Build each element as Elementor does on save, without saving, and
     * compare what it would store with the element, its whole subtree
     * included. Nothing may be written while it runs.
     *
     * @param array<string, mixed> $nodes Elements by path.
     * @return array{code: string, detail: string}|null
     */
    private function dryRunNodes(array $nodes): ?array
    {
        $recorder = new AbilitySideEffects([], []);
        $problem  = null;
        $level    = ob_get_level();
        ob_start();
        try {
            $recorder->arm();
            foreach ($nodes as $at => $node) {
                $element = is_array($node) ? $this->api->createElementInstance($node) : null;
                $build   = $element === null ? null : [$element, 'get_data_for_save'];
                if ($build === null || !is_callable($build)) {
                    $problem = $at . ': Elementor could not build the element';
                    break;
                }
                if ($build() !== $node) {
                    $problem = $at . ': Elementor would store the element differently';
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
