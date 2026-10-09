<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\ServicePrincipal;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Elementor behind the builder adapter interface: the classic format, in
 * containers or in sections and columns as the site has them.
 *
 * Facts are read once per adapter, on first use. A page is built by
 * ElementorClassicMapper and saved and read back by ElementorDocument.
 * Every page-edit operation and node kind is declared for the edit path; the
 * allowlist and the leaf rules are the mapper's: braces are plain text in
 * Elementor, and its dynamic-tag marker is refused in every stored string.
 */
final class ElementorAdapter implements BuilderAdapter
{
    /** The adapter id. */
    public const ID = 'elementor';

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
}
