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
            ['_elementor_css', '_elementor_element_cache', '_elementor_page_assets'],
            [ElementorDocument::KEY_EDIT_MODE]
        );
    }

    /**
     * {@inheritDoc}
     */
    public function capabilities(): array
    {
        return ['operations' => BuilderContract::OPS, 'node_kinds' => BuilderContract::KINDS];
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
        if (!is_string($postType) || !in_array($postType, self::POST_TYPES, true)) {
            return ['code' => 'bad_input', 'detail' => 'post_type: not a type an Elementor page is created as'];
        }
        $containers = ($this->facts()['containers'] ?? null) === true;
        $mapped     = ElementorClassicMapper::map($spec, $ids, $mediaById, $containers);
        if (!isset($mapped['tree'])) {
            return ['code' => $mapped['code'] ?? 'bad_input', 'detail' => $mapped['detail'] ?? 'outline: could not be mapped'];
        }
        try {
            $doc = new NativeDocument($mapped['tree'], [
                ElementorDocument::KEY_EDIT_MODE     => ElementorDocument::EDIT_MODE_BUILDER,
                ElementorDocument::KEY_TEMPLATE_TYPE => ElementorDocument::templateType($postType),
            ]);
        } catch (\Throwable $e) {
            return ['code' => 'bad_input', 'detail' => 'outline: the page could not be encoded'];
        }

        return ['doc' => $doc];
    }

    /**
     * {@inheritDoc}
     */
    public function project(array $tree): Projection
    {
        return ElementorClassicMapper::project($tree);
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
    public function planEdit(int $postId, array $ops, IdSeed $ids, array $mediaById): array
    {
        if ($postId < 1) {
            return ['code' => LayoutOps::CODE_UNREADABLE, 'detail' => 'not a post', 'op_index' => null];
        }
        try {
            $stored = BuilderDocumentFingerprint::read($postId, [ElementorDocument::KEY_DATA]);
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

        $planned = LayoutOps::apply($tree, $ops, $ids, $mediaById, ($this->facts()['containers'] ?? null) === true);
        if (!isset($planned['tree'], $planned['changes'], $planned['touched'], $planned['new_count'])) {
            return ['code' => $planned['code'] ?? 'bad_input', 'detail' => $planned['detail'] ?? 'the operations could not be applied', 'op_index' => $planned['op_index'] ?? null];
        }
        try {
            $doc = new NativeDocument($planned['tree']);
        } catch (\Throwable $e) {
            return ['code' => LayoutOps::CODE_UNREADABLE, 'detail' => 'the edited page could not be encoded', 'op_index' => null];
        }

        return ['doc' => $doc, 'changes' => $planned['changes'], 'touched' => $planned['touched'], 'new_count' => $planned['new_count']];
    }

    /**
     * {@inheritDoc}
     */
    public function write(int $postId, NativeDocument $doc): array
    {
        $kitId = $this->facts()['active_kit_id'] ?? 0;

        return $this->document->save($postId, $doc->tree, is_int($kitId) ? $kitId : 0);
    }

    /**
     * {@inheritDoc}
     *
     * The post type is the one the document's type stands for.
     */
    public function verifyCreated(int $postId, NativeDocument $doc, int $principal, string $requestId): ?string
    {
        $template = $doc->meta[ElementorDocument::KEY_TEMPLATE_TYPE] ?? null;
        $postType = is_string($template) ? (self::POST_TYPES[$template] ?? null) : null;
        if ($postType === null) {
            return 'template_type';
        }

        return $this->document->verifyCreated($postId, $doc->tree, $principal, $requestId, $postType);
    }

    /**
     * {@inheritDoc}
     *
     * Elementor's own per-post invalidation: the post's generated CSS file
     * and its CSS meta through Elementor's post CSS object, then the post's
     * local styles in every context through Elementor's style-clear action,
     * on the path Elementor clears when a post is published or deleted. The
     * derived meta rows are already gone. Never Elementor's site-wide clear.
     */
    public function afterRestore(int $postId): void
    {
        if ($postId < 1) {
            return;
        }
        $this->api->deletePostCss($postId);
        try {
            do_action('elementor/atomic-widgets/styles/clear', [self::STYLES_KEY_LOCAL, $postId]); // phpcs:ignore WordPress.NamingConventions.PrefixAllGlobals.NonPrefixedHooknameFound -- firing Elementor's documented per-post style invalidation; not a custom hook
        } catch (\Throwable $e) {
            // Elementor rebuilds the styles on the post's next save.
            unset($e);
        }
    }
}
