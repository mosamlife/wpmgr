<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\ServicePrincipal;
use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Elementor behind the builder adapter interface: the classic format, in
 * containers or in sections and columns as the site has them.
 *
 * Facts are read once per adapter, on first use. A page is built by
 * ElementorClassicMapper, an edit is planned by LayoutOps, and both are
 * saved and read back by ElementorDocument.
 * Every page-edit operation and node kind is declared for the edit path; the
 * allowlist and the leaf rules are the mapper's: braces are plain text in
 * Elementor, and its dynamic-tag marker is refused in every stored string.
 */
final class ElementorAdapter implements BuilderAdapter
{
    /** The adapter id. */
    public const ID = 'elementor';

    /** Elementor's action that clears generated styles under a path; afterRestore() fires it by this literal name. */
    public const HOOK_STYLES_CLEAR = 'elementor/atomic-widgets/styles/clear';

    /** The first segment of that path for a post's own (local) styles. */
    public const STYLES_KEY_LOCAL = 'local';

    /** Post type by Elementor document type, for the types page-create makes. */
    private const POST_TYPES = [
        ElementorDocument::TEMPLATE_PAGE => 'page',
        ElementorDocument::TEMPLATE_POST => 'post',
    ];

    private readonly ElementorApi $api;

    private readonly ElementorDocument $document;

    /** @var array<string, mixed>|null */
    private ?array $facts = null;

    /** Native nodes validated by this adapter's most recent edit plan. */
    private array $nativeNodes = [];

    /**
     * @param ElementorApi|null $api         Elementor; the one loaded on this request when null.
     * @param int|null          $principalId The service user's id; read when the facts are first needed when null.
     */
    public function __construct(?ElementorApi $api = null, private readonly ?int $principalId = null)
    {
        $this->api      = $api ?? new ElementorRuntime();
        $this->document = new ElementorDocument($this->api);
    }

    /**
     * {@inheritDoc}
     */
    public function id(): string
    {
        return self::ID;
    }

    /**
     * The site's Elementor facts, read on first use.
     *
     * @return array<string, mixed>
     */
    public function facts(): array
    {
        if ($this->facts === null) {
            $this->facts = ElementorFacts::collect($this->api, $this->principalId ?? ServicePrincipal::userId());
        }

        return $this->facts;
    }

    /** Each precheck and write plan reads the current version and site facts anew. */
    public function refreshEditFacts(): void
    {
        $this->facts = null;
    }

    /**
     * The breakpoints the site has active, which a designed page's tree is
     * compiled for: Elementor's names in its own order, or null when the
     * site could not say, which the compile reads as none active. The
     * precheck answers this same value, so a twin of the compile rebuilds
     * the tree from what the answer carries.
     *
     * @return list<string>|null
     */
    public function activeBreakpoints(): ?array
    {
        $active = $this->facts()['active_breakpoints'] ?? null;

        return is_array($active) ? array_values($active) : null;
    }

    /**
     * The family names in Elementor's font registry on this site, read each
     * time it is asked: only a page whose design names a family needs them,
     * so the facts do not carry them.
     *
     * @return list<string>|null Null when the registry cannot be read.
     */
    public function fontFamilies(): ?array
    {
        return $this->api->fontFamilies();
    }

    /**
     * The document checks, save and read-back for this adapter's Elementor.
     *
     * @return ElementorDocument
     */
    public function document(): ElementorDocument
    {
        return $this->document;
    }

    /**
     * {@inheritDoc}
     */
    public function status(): AdapterStatus
    {
        return ElementorFacts::status($this->facts());
    }

    /**
     * {@inheritDoc}
     */
    public function owns(int $postId): ?bool
    {
        if ($postId < 1) {
            return null;
        }
        try {
            $stored = BuilderDocumentFingerprint::read($postId, [ElementorDocument::KEY_EDIT_MODE]);
        } catch (\Throwable $e) {
            return null;
        }
        if ($stored === null) {
            return null;
        }

        return ($stored['rows'][ElementorDocument::KEY_EDIT_MODE] ?? []) === [ElementorDocument::EDIT_MODE_BUILDER];
    }

    /**
     * {@inheritDoc}
     */
    public function descriptor(): DocumentDescriptor
    {
        return new DocumentDescriptor(
            ElementorDocument::DESCRIPTOR_KEYS,
            [],
            ['_elementor_css', ElementorDocument::KEY_ELEMENT_CACHE, '_elementor_page_assets'],
            [ElementorDocument::KEY_EDIT_MODE]
        );
    }

    /** Template edit fingerprints cover the stored display rules as well as the tree. */
    public function editDescriptor(bool $template): DocumentDescriptor
    {
        $d = $this->descriptor();
        return $template ? new DocumentDescriptor([...$d->exactKeys, ElementorRuntime::KEY_CONDITIONS], $d->prefixes, $d->derivedKeys, $d->flagKeys) : $d;
    }

    /**
     * {@inheritDoc}
     */
    public function capabilities(): array
    {
        return ['operations' => BuilderContract::OPS, 'node_kinds' => BuilderContract::KINDS];
    }

    /** Draft-template validation and the scoped conditions save, using this adapter's runtime. */
    public function templates(): ThemeTemplateCreate
    {
        return new ThemeTemplateCreate($this->api);
    }

    /**
     * {@inheritDoc}
     */
    public function allowedKeys(): array
    {
        return ElementorClassicMapper::ALLOWED_KEYS;
    }

    /**
     * {@inheritDoc}
     */
    public function leafRules(): array
    {
        return ElementorClassicMapper::LEAF_RULES;
    }

    /**
     * {@inheritDoc}
     *
     * Containers when the site has Elementor's container layout on, else
     * sections and columns. The document's meta is the edit mode and the
     * document type the page is inserted with; its content is left to
     * Elementor's save.
     */
    public function buildCreate(array $spec, IdSeed $ids, array $mediaById): array
    {
        $postType = $spec['post_type'] ?? null;
        if (!is_string($postType) || (!in_array($postType, self::POST_TYPES, true) && !($postType === 'elementor_library' && in_array($spec['template'] ?? null, ThemeTemplateCreate::TYPES, true)))) {
            return ['code' => 'bad_input', 'detail' => 'post_type: not a type an Elementor page is created as'];
        }
        $facts      = $this->facts();
        $containers = ($facts['containers'] ?? null) === true;
        $design     = null;
        if (($spec['outline_version'] ?? null) === OutlineV3Schema::VERSION) {
            // A designed page: the design's tokens, and the breakpoints the site has active.
            try {
                $tokens = DesignTokens::fromDesign(is_array($spec['design'] ?? null) ? $spec['design'] : []);
            } catch (\InvalidArgumentException $e) {
                return ['code' => 'bad_input', 'detail' => 'design: the design could not be read'];
            }
            $design = new ElementorSectionCompiler($tokens, $this->activeBreakpoints(), $mediaById);
        }
        $mapped = ElementorClassicMapper::map($spec, $ids, $mediaById, $containers, $design);
        if (!isset($mapped['tree'])) {
            return ['code' => $mapped['code'] ?? 'bad_input', 'detail' => $mapped['detail'] ?? 'outline: could not be mapped'];
        }
        try {
            $doc = new NativeDocument($mapped['tree'], [
                ElementorDocument::KEY_EDIT_MODE     => ElementorDocument::EDIT_MODE_BUILDER,
                ElementorDocument::KEY_TEMPLATE_TYPE => $spec['template'] ?? ElementorDocument::templateType($postType),
            ]);
        } catch (\Throwable $e) {
            return ['code' => 'bad_input', 'detail' => 'outline: the page could not be encoded'];
        }

        return ['doc' => $doc];
    }

    /**
     * {@inheritDoc}
     *
     * The page's one _elementor_data row, decoded at most
     * ElementorDocument::MAX_DEPTH deep to a list of elements.
     */
    public function storedTree(array $rowsByKey): ?array
    {
        $rows = $rowsByKey[ElementorDocument::KEY_DATA] ?? [];

        return is_array($rows) ? ElementorDocument::treeOf($rows) : null;
    }

    /**
     * {@inheritDoc}
     */
    public function project(array $tree, ?string $templateType = null): Projection
    {
        return ElementorClassicMapper::project($tree, new NativeWidgetPolicy($this->api, $templateType));
    }

    /**
     * {@inheritDoc}
     *
     * The stored document is the post's one _elementor_data row, read with
     * SQL and decoded at ElementorDocument::MAX_DEPTH into a list of
     * elements; anything else is data_unreadable. LayoutOps applies the
     * operations: new top-level nodes take the site's container setting,
     * new nodes inside an element take that element's layout. The new
     * document carries no meta: an edit stores only the tree, through
     * Elementor's own save.
     */
    public function planEdit(int $postId, array $ops, IdSeed $ids, array $mediaById, ?array $design = null): array
    {
        $this->nativeNodes = [];
        if ($postId < 1) {
            return ['code' => LayoutOps::CODE_UNREADABLE, 'detail' => 'not a post', 'op_index' => null];
        }
        try {
            $stored = BuilderDocumentFingerprint::read($postId, [ElementorDocument::KEY_DATA, ElementorDocument::KEY_TEMPLATE_TYPE]);
        } catch (\Throwable $e) {
            return ['code' => LayoutOps::CODE_UNREADABLE, 'detail' => 'the page could not be read', 'op_index' => null];
        }
        $rows = $stored === null ? [] : ($stored['rows'][ElementorDocument::KEY_DATA] ?? []);
        if (count($rows) !== 1) {
            return ['code' => LayoutOps::CODE_UNREADABLE, 'detail' => 'the page does not have exactly one Elementor document', 'op_index' => null];
        }
        $tree = json_decode($rows[0], true, ElementorDocument::MAX_DEPTH);
        if (!is_array($tree) || !ArrayShape::isList($tree)) {
            return ['code' => LayoutOps::CODE_UNREADABLE, 'detail' => 'the Elementor document is not a list of elements', 'op_index' => null];
        }

        $identity = LayoutOps::storedProblem($tree);
        if ($identity !== null) {
            return $identity;
        }
        $active = $this->activeBreakpoints();

        // The design the edit carries compiles its new sections, patterns and groups.
        $compiler = null;
        if ($design !== null) {
            try {
                $tokens = DesignTokens::fromDesign($design);
            } catch (\InvalidArgumentException $e) {
                return ['code' => 'bad_input', 'detail' => 'design: the design could not be read', 'op_index' => null];
            }
            $compiler = new ElementorSectionCompiler($tokens, is_array($active) ? array_values($active) : null, $mediaById);
        }
        $template = null;
        if (($stored['post']['post_type'] ?? null) === 'elementor_library') {
            $types = $stored['rows'][ElementorDocument::KEY_TEMPLATE_TYPE] ?? [];
            if (count($types) === 1 && in_array($types[0], ThemeTemplateCreate::TYPES, true)) {
                $template = $types[0];
            }
        }
        $native = new NativeWidgetPolicy($this->api, $template);
        $nativeCall = NativeWidgetPolicy::isNativeCall($ops, ElementorClassicMapper::project($tree, $native)->nodes());
        $planned = LayoutOps::apply($tree, $ops, $ids, $mediaById, ($this->facts()['containers'] ?? null) === true, $compiler, $nativeCall ? $native : null, $active);
        if (!isset($planned['tree'], $planned['changes'], $planned['touched'], $planned['new_count'])) {
            return ['code' => $planned['code'] ?? 'bad_input', 'detail' => $planned['detail'] ?? 'the operations could not be applied', 'op_index' => $planned['op_index'] ?? null]
                + array_intersect_key($planned, ['failures' => true, 'schema' => true]);
        }
        try {
            $doc = new NativeDocument($planned['tree']);
        } catch (\Throwable $e) {
            return ['code' => LayoutOps::CODE_UNREADABLE, 'detail' => 'the edited page could not be encoded', 'op_index' => null];
        }

        $this->nativeNodes = $planned['native_nodes'];
        return ['doc' => $doc, 'changes' => $planned['changes'], 'touched' => $planned['touched'], 'new_count' => $planned['new_count'],
            'results' => $planned['results'], 'schemas' => $planned['schemas'], 'has_native' => $planned['has_native'], 'active_breakpoints' => $active];
    }

    /**
     * {@inheritDoc}
     *
     * @param bool $templateEdit Protect template edits without admitting initial type terms.
     */
    public function write(int $postId, NativeDocument $doc, bool $templateEdit = false): array
    {
        $kitId = $this->facts()['active_kit_id'] ?? 0;

        $type = $doc->meta[ElementorDocument::KEY_TEMPLATE_TYPE] ?? null;
        return $this->document->save($postId, $doc->tree, is_int($kitId) ? $kitId : 0, !$templateEdit && in_array($type, ThemeTemplateCreate::TYPES, true) ? $type : null, $templateEdit);
    }

    /**
     * {@inheritDoc}
     *
     * The post type is the one the document's type stands for, and the page
     * template the one the document's meta carries, if any. Only a designed
     * page carries one, and its tree is held to the designed allowlist for
     * the breakpoints this site has active; a plain page's to ALLOWED_KEYS.
     */
    public function verifyCreated(int $postId, NativeDocument $doc, int $principal, string $requestId, array $showsOn = []): ?string
    {
        $template = $doc->meta[ElementorDocument::KEY_TEMPLATE_TYPE] ?? null;
        $themeType = in_array($template, ThemeTemplateCreate::TYPES, true) ? $template : null;
        $postType = $themeType !== null ? 'elementor_library' : (is_string($template) ? (self::POST_TYPES[$template] ?? null) : null);
        if ($postType === null) {
            return 'template_type';
        }

        $pageTemplate = $doc->meta[ElementorDocument::KEY_PAGE_TEMPLATE] ?? null;
        $pageSettings = $doc->meta[ElementorDocument::KEY_PAGE_SETTINGS] ?? null;

        $active  = $this->facts()['active_breakpoints'] ?? null;
        $allowed = (is_string($pageTemplate) || ($themeType !== null && ElementorClassicMapper::holdsDesignedSection($doc->tree))) ? LeafPolicy::forBreakpoints(ElementorClassicMapper::designedKeys(), is_array($active) ? array_values($active) : []) : null;

        return $this->document->verifyCreated($postId, $doc->tree, $principal, $requestId, $postType, is_string($pageTemplate) ? $pageTemplate : null, is_array($pageSettings) ? $pageSettings : null, $allowed, $themeType, $showsOn);
    }

    /**
     * {@inheritDoc}
     *
     * ElementorDocument::verifyEdited() over the planned tree, and over the
     * page settings and page template the document's meta carries, when it
     * carries them: only a change the edit planned is accepted. What the edit
     * wrote into a designed page, one the edit leaves with a designed section
     * at its top, is held to the designed allowlist for the breakpoints this
     * site has active, as a designed page is when it is created; what it
     * wrote into any other page, to ALLOWED_KEYS.
     */
    public function verifyEdited(int $postId, NativeDocument $doc, array $before): ?string
    {
        $pageSettings = $doc->meta[ElementorDocument::KEY_PAGE_SETTINGS] ?? null;
        $template     = $doc->meta[ElementorDocument::KEY_PAGE_TEMPLATE] ?? null;

        $allowed = null;
        if (ElementorClassicMapper::holdsDesignedSection($doc->tree)) {
            $active  = $this->facts()['active_breakpoints'] ?? null;
            $allowed = LeafPolicy::forBreakpoints(ElementorClassicMapper::designedKeys(), is_array($active) ? array_values($active) : []);
        }

        return $this->document->verifyEdited($postId, $doc->tree, $before, is_array($pageSettings) ? $pageSettings : null, is_string($template) ? $template : null, $allowed, $this->nativeNodes);
    }

    /**
     * The facts the precheck of an edit answers beside its preview, for a
     * designed page: the breakpoints the site has active (activeBreakpoints()),
     * which what an inserted section or pattern compiles to and the allowlist
     * an edited designed page is held to are functions of, so a twin of the
     * edit check rebuilds the tree from what the answer carries. A page is
     * designed when it holds a designed section before the edit or after it
     * (ElementorClassicMapper::holdsDesignedSection()); the answer for any
     * other page carries no fact.
     *
     * @param array<mixed> $before The stored tree.
     * @param array<mixed> $after  The tree the edit leaves.
     * @return array{active_breakpoints: list<string>|null}|null Null for a plain page.
     */
    public function editFacts(array $before, array $after): ?array
    {
        if (!ElementorClassicMapper::holdsDesignedSection($before) && !ElementorClassicMapper::holdsDesignedSection($after)) {
            return null;
        }

        return ['active_breakpoints' => $this->activeBreakpoints()];
    }

    /**
     * Run $restore, which puts the page's own rows back, inside the page's
     * write scope and under the active kit's tripwire
     * (ElementorDocument::restoreScoped()).
     *
     * @param int      $postId  The page.
     * @param callable $restore The restore.
     * @param bool $template Whether the authenticated snapshot belongs to a header or footer.
     * @return array<string, mixed>
     */
    public function restoreInScope(int $postId, callable $restore, bool $template = false): array
    {
        $kitId = $this->facts()['active_kit_id'] ?? 0;

        return $this->document->restoreScoped($postId, is_int($kitId) ? $kitId : 0, $restore, $template);
    }

    /**
     * {@inheritDoc}
     *
     * Elementor's own per-post invalidation: the post's generated CSS file
     * and its CSS meta through Elementor's post CSS object, then the post's
     * local styles in every context through Elementor's style-clear action,
     * on the path Elementor clears when a post is published or deleted. The
     * derived meta rows are already gone. Never Elementor's site-wide clear.
     * Last, the post's CSS is made again once from the rows that are back
     * (ElementorDocument::refreshCaches()); a stylesheet that cannot be
     * confirmed is not reported, since a restore returns no result to carry
     * it, and Elementor makes it again when the page is next shown.
     */
    public function afterRestore(int $postId): void
    {
        if ($postId < 1) {
            return;
        }
        $since = time();
        $this->api->deletePostCss($postId);
        try {
            do_action('elementor/atomic-widgets/styles/clear', [self::STYLES_KEY_LOCAL, $postId]); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- firing Elementor's documented per-post style invalidation; not a custom hook
        } catch (\Throwable $e) {
            // Elementor rebuilds the styles on the post's next save.
            unset($e);
        }
        $this->document->refreshCaches($postId, $since);
    }

    /**
     * {@inheritDoc}
     *
     * ElementorDocument::elementCacheActive(): Elementor Pro is active and
     * the post has a cached element output row.
     */
    public function servesCachedRender(int $postId): bool
    {
        return $this->document->elementCacheActive($postId);
    }
}
