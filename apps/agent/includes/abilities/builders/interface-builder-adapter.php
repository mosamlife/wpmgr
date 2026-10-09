<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * One page builder's storage format, behind the ability engine.
 *
 * The engine keeps approval binding, the ledger, claims, the principal,
 * snapshot and restore. An adapter owns only how its builder stores a page:
 * what it can build, which rows hold the page, how a stored page projects to
 * the builder-neutral structure, and how a built page is saved and read back.
 *
 * Adapters are compiled into the agent and reached only through
 * BuilderRegistry; data never names a class. Every refusal an adapter returns
 * is a code and a short detail from WPMgr's own vocabulary, never text read
 * from the site.
 */
interface BuilderAdapter
{
    /** How a value at an allowed settings path is checked before it is stored. */
    public const PINS = ['html', 'text', 'url', 'int', 'enum'];

    /**
     * The adapter id, one of BuilderRegistry::IDS.
     *
     * @return string
     */
    public function id(): string;

    /**
     * Whether the builder can be used on this site now: active, its version
     * inside the range this code was tested against, and no reason recorded
     * against it. A pure read.
     *
     * @return AdapterStatus
     */
    public function status(): AdapterStatus;

    /**
     * Whether this builder holds the post's layout. Null when that cannot be
     * told; a caller refuses on null.
     *
     * @param int $postId Post id.
     * @return bool|null
     */
    public function owns(int $postId): ?bool;

    /**
     * Exactly which rows make up a page in this builder's format.
     *
     * @return DocumentDescriptor
     */
    public function descriptor(): DocumentDescriptor;

    /**
     * The page-edit operations (a subset of BuilderContract::OPS) and the
     * node kinds (a subset of BuilderContract::KINDS) this adapter supports.
     *
     * @return array{operations: list<string>, node_kinds: list<string>}
     */
    public function capabilities(): array;

    /**
     * The only settings this adapter ever writes: element type, then settings
     * path, then the pin (one of PINS) its value is checked against. A setting
     * not listed here is never written.
     *
     * @return array<string, array<string, string>>
     */
    public function allowedKeys(): array;

    /**
     * Rules every stored string obeys besides its pin: whether a brace
     * anywhere refuses it, and substrings that refuse it.
     *
     * @return array{refuse_braces: bool, forbidden: list<string>}
     */
    public function leafRules(): array;

    /**
     * Map a validated page-create spec to the builder's native document.
     * Pure: no WordPress write. Node ids come from $ids only, so a precheck
     * and its write build the same bytes. A node the builder cannot express
     * is refused with node_not_supported_by_builder, naming the node path.
     *
     * @param array<string, mixed>             $spec      Validated page-create spec.
     * @param IdSeed                           $ids       Deterministic node ids for this request.
     * @param array<int, array<string, mixed>> $mediaById Media facts by attachment id.
     * @return array{doc?: NativeDocument, code?: string, detail?: string}
     */
    public function buildCreate(array $spec, IdSeed $ids, array $mediaById): array;

    /**
     * The builder-neutral projection of a stored tree. Pure.
     *
     * @param array<mixed> $tree Decoded stored tree.
     * @return Projection
     */
    public function project(array $tree): Projection;

    /**
     * Plan a wpmgr/page-edit call: read the post's stored document and apply
     * the operations to it in order, building the new document. No
     * WordPress write. New node ids come from $ids only, so a precheck and
     * its write on the same stored page build the same bytes. Each change
     * describes one operation; touched names the nodes whose text changed and
     * the nodes made. A refusal carries the operation's index, or null for a
     * rule over the whole call.
     *
     * @param int                              $postId    Post id.
     * @param list<array<string, mixed>>       $ops       Operations as PageEditValidator::parse() normalised them.
     * @param IdSeed                           $ids       Deterministic node ids for this request.
     * @param array<int, array<string, mixed>> $mediaById Media facts by attachment id, for new images.
     * @return array{doc?: NativeDocument, changes?: list<array<string, mixed>>, touched?: list<string>, new_count?: int, code?: string, detail?: string, op_index?: int|null}
     */
    public function planEdit(int $postId, array $ops, IdSeed $ids, array $mediaById): array;

    /**
     * Store the document on the post through the builder's own save path.
     *
     * @param int            $postId Post id.
     * @param NativeDocument $doc    What buildCreate() built.
     * @return array<string, mixed> "ok" (bool); "code" and "detail" when not ok.
     */
    public function write(int $postId, NativeDocument $doc): array;

    /**
     * Read the created page back and compare it with what was built.
     *
     * @param int            $postId    Post id.
     * @param NativeDocument $doc       What buildCreate() built.
     * @param int            $principal The WPMgr service user the page was created as.
     * @param string         $requestId The request that created the page.
     * @return string|null Null when the stored page is exactly what was built,
     *                     else a short token naming the first mismatch.
     */
    public function verifyCreated(int $postId, NativeDocument $doc, int $principal, string $requestId): ?string;

    /**
     * Read an edited page back and compare it with what planEdit() built and
     * with the page's snapshot taken before the save: the post is still a
     * draft; its type, author, parent and slug, and the page's document rows
     * the edit does not write, are as the snapshot holds them; the whole
     * stored document, untouched nodes included, is the planned one; and
     * what the edit wrote passes the adapter's allowlist and leaf rules.
     *
     * @param int                  $postId Post id.
     * @param NativeDocument       $doc    What planEdit() built.
     * @param array<string, mixed> $before The page's snapshot, as BuilderDocumentSnapshot::decode() gives it.
     * @return string|null Null when the stored page is exactly what was
     *                     planned, else a short token naming the first
     *                     mismatch.
     */
    public function verifyEdited(int $postId, NativeDocument $doc, array $before): ?string;

    /**
     * Drop what the builder keeps about one post outside the post's rows,
     * after BuilderDocumentRestore has put the rows back and deleted the
     * descriptor's derived keys: generated files, style caches and the like.
     * Only this post's, never a site-wide clear. Never throws.
     *
     * @param int $postId Post id.
     * @return void
     */
    public function afterRestore(int $postId): void;
}
