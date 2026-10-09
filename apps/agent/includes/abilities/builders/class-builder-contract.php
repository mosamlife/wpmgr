<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\PageCreateBuilder;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The builder-neutral contract every builder adapter shares: the limits, the
 * operation and node vocabularies, and the input schemas of wpmgr/page-edit
 * and wpmgr/page-structure.
 *
 * wpmgr/page-edit input (exact JSON text, an object):
 *   post_id           integer, at least 1
 *   base_fingerprint  the page's builder_document_v1, 64 lowercase hex
 *   operations        1..25 of:
 *     {"op":"set_text","ref":REF,"field":FIELD,"text":TEXT}
 *     {"op":"insert","after":REF,"outline":OUTLINE}
 *     {"op":"insert","before":REF,"outline":OUTLINE}
 *     {"op":"insert","into":REF,"position"?:"first"|"last","outline":OUTLINE}
 *     {"op":"replace","ref":REF,"outline":OUTLINE}
 *     {"op":"remove","ref":REF}
 *     {"op":"move","ref":REF,"after":REF}
 *     {"op":"move","ref":REF,"before":REF}
 *   REF      a node reference read from wpmgr/page-structure, matching RE_REF
 *   FIELD    one of FIELDS; the node's own editable list decides
 *   OUTLINE  1..50 wpmgr/page-create outline nodes, the same grammar
 *
 * An insert names exactly one anchor, and only an insert into a node takes a
 * position. Anything outside the grammar is refused, never stripped. Rules
 * that depend on the page (a reference that is not on it, a field the node
 * does not offer, a locked node, an operation the page's builder does not
 * declare) belong to the agent's precheck, not to the schema.
 *
 * wpmgr/page-structure input: post_id, an optional node REF for one subtree,
 * and an optional max_nodes of 1..500.
 *
 * Pure: no WordPress call.
 */
final class BuilderContract
{
    /** A node reference, as a JSON-schema pattern. */
    public const REF_PATTERN = '^[A-Za-z0-9_-]{1,32}$';

    /** A node reference, as a PCRE (the $ matches only at the very end). */
    public const RE_REF = '/' . self::REF_PATTERN . '/D';

    /** A builder_document_v1 fingerprint, as a JSON-schema pattern. */
    public const FINGERPRINT_PATTERN = '^[0-9a-f]{64}$';

    /** Operations in one page-edit call. */
    public const MAX_OPS = 25;

    /** Outline nodes in one insert or replace. */
    public const MAX_OUTLINE_PER_OP = 50;

    /** Nodes created by one page-edit call. */
    public const MAX_NEW_NODES = 400;

    /** Nodes on the page after one page-edit call. */
    public const MAX_NODES_AFTER = 800;

    /** Bytes of the page-edit input text. */
    public const MAX_INPUT_BYTES = 65536;

    /** Bytes of the stored builder document after a write. */
    public const MAX_DOCUMENT_BYTES = 1048576;

    /** Bytes of the snapshot taken before a write. */
    public const MAX_SNAPSHOT_BYTES = 4194304;

    /** Nodes in one page-structure answer. */
    public const MAX_STRUCTURE_NODES = 500;

    /** Bytes of one page-structure answer. */
    public const MAX_STRUCTURE_BYTES = 65536;

    /** The page-edit operations. Each adapter declares which of these it supports. */
    public const OPS = ['set_text', 'insert', 'replace', 'remove', 'move'];

    /**
     * The node kinds of a page projection: the page-create outline kinds, then
     * the builder kinds. "locked" is every node an adapter does not classify.
     */
    public const KINDS = [
        'heading',
        'paragraph',
        'list',
        'image',
        'buttons',
        'quote',
        'separator',
        'spacer',
        'table',
        'group',
        'columns',
        'section',
        'column',
        'locked',
    ];

    /** The fields set_text may name. A node's editable list is a subset. */
    public const FIELDS = ['text', 'url', 'alt', 'caption'];

    /** Where an insert into a node puts the new nodes. */
    public const POSITIONS = ['first', 'last'];

    /**
     * The JSON schema of the wpmgr/page-edit input. Every operation variant
     * is written out in full (no $ref), and the outline items are the
     * page-create outline items, unchanged.
     *
     * @return array<string,mixed>
     */
    public static function pageEditInputSchema(): array
    {
        $ref     = self::refSchema();
        $outline = [
            'type'     => 'array',
            'minItems' => 1,
            'maxItems' => self::MAX_OUTLINE_PER_OP,
            'items'    => PageCreateBuilder::inputSchema()['properties']['outline']['items'],
        ];
        $text    = ['type' => 'string', 'maxLength' => PageCreateBuilder::MAX_TEXT_CHARS];

        $operations = [
            self::opSchema('set_text', [
                'ref'   => $ref,
                'field' => ['type' => 'string', 'enum' => self::FIELDS],
                'text'  => $text,
            ], ['ref', 'field', 'text']),
            self::opSchema('insert', ['after' => $ref, 'outline' => $outline], ['after', 'outline']),
            self::opSchema('insert', ['before' => $ref, 'outline' => $outline], ['before', 'outline']),
            self::opSchema('insert', [
                'into'     => $ref,
                'position' => ['type' => 'string', 'enum' => self::POSITIONS],
                'outline'  => $outline,
            ], ['into', 'outline']),
            self::opSchema('replace', ['ref' => $ref, 'outline' => $outline], ['ref', 'outline']),
            self::opSchema('remove', ['ref' => $ref], ['ref']),
            self::opSchema('move', ['ref' => $ref, 'after' => $ref], ['ref', 'after']),
            self::opSchema('move', ['ref' => $ref, 'before' => $ref], ['ref', 'before']),
        ];

        return [
            'type'                 => 'object',
            'properties'           => [
                'post_id'          => ['type' => 'integer', 'minimum' => 1],
                'base_fingerprint' => ['type' => 'string', 'pattern' => self::FINGERPRINT_PATTERN],
                'operations'       => [
                    'type'     => 'array',
                    'minItems' => 1,
                    'maxItems' => self::MAX_OPS,
                    'items'    => ['oneOf' => $operations],
                ],
            ],
            'required'             => ['post_id', 'base_fingerprint', 'operations'],
            'additionalProperties' => false,
        ];
    }

    /**
     * The JSON schema of the wpmgr/page-structure input.
     *
     * @return array<string,mixed>
     */
    public static function pageStructureInputSchema(): array
    {
        return [
            'type'                 => 'object',
            'properties'           => [
                'post_id'   => ['type' => 'integer', 'minimum' => 1],
                'node'      => self::refSchema(),
                'max_nodes' => ['type' => 'integer', 'minimum' => 1, 'maximum' => self::MAX_STRUCTURE_NODES],
            ],
            'required'             => ['post_id'],
            'additionalProperties' => false,
        ];
    }

    /**
     * @return array<string,string>
     */
    private static function refSchema(): array
    {
        return ['type' => 'string', 'pattern' => self::REF_PATTERN];
    }

    /**
     * One operation variant.
     *
     * @param string              $op       Operation name, one of OPS.
     * @param array<string,mixed> $props    Properties other than op.
     * @param list<string>        $required Required properties other than op.
     * @return array<string,mixed>
     */
    private static function opSchema(string $op, array $props, array $required): array
    {
        return [
            'type'                 => 'object',
            'properties'           => ['op' => ['const' => $op]] + $props,
            'required'             => array_merge(['op'], $required),
            'additionalProperties' => false,
        ];
    }
}
