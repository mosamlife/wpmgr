<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The builder-neutral structure of a page, as wpmgr/page-structure answers it.
 *
 * Nodes are added parent first, in page order. Each is one of:
 *
 *   {ref, parent, kind, level?, editable: [field…], from_the_site: {field: text…}}
 *   {ref, parent, kind: "locked", label}
 *
 * - ref     the node's id on the page, matching BuilderContract::RE_REF and
 *           unique; parent is ROOT or a node added before it.
 * - kind    one of BuilderContract::KINDS. "locked" is every node the adapter
 *           does not classify, added only through addLocked().
 * - label   WPMgr's words from the adapter's label map, never the site's:
 *           a key the map does not carry gets the map's FALLBACK_LABEL text.
 * - editable, from_the_site  fields from BuilderContract::FIELDS. Site text
 *           appears only under from_the_site, with any invalid UTF-8 replaced
 *           by U+FFFD.
 *
 * toArray() answers at most BuilderContract::MAX_STRUCTURE_NODES nodes and at
 * most a byte cap (default BuilderContract::MAX_STRUCTURE_BYTES, measured as
 * json_encode() with default flags), cut after a whole node so every node it
 * keeps still has its parent. A node that breaks these rules is refused with
 * \InvalidArgumentException and nothing is added.
 */
final class Projection
{
    /** The parent of a top-level node. */
    public const ROOT = 'root';

    /** The label map key every adapter's map carries: any node the map does not name. */
    public const FALLBACK_LABEL = '*';

    /** The kind of a node the adapter does not classify. */
    private const LOCKED = 'locked';

    /**
     * The adapter's label map, key => WPMgr's words.
     *
     * @var array<int|string, string>
     */
    private array $labels;

    /** @var list<array<string, mixed>> */
    private array $nodes = [];

    /** @var array<int|string, true> */
    private array $refs = [];

    /**
     * @param array<mixed> $lockedLabels The adapter's label map, key => WPMgr's words; carries FALLBACK_LABEL.
     * @throws \InvalidArgumentException When a label is empty or not a string, or the fallback is missing.
     */
    public function __construct(array $lockedLabels)
    {
        $labels = [];
        foreach ($lockedLabels as $key => $label) {
            if (!is_string($label) || $label === '') {
                throw new \InvalidArgumentException('a locked label must be a non-empty string');
            }
            $labels[$key] = $label;
        }
        if (!isset($labels[self::FALLBACK_LABEL])) {
            throw new \InvalidArgumentException('a label map must carry the fallback label');
        }
        $this->labels = $labels;
    }

    /**
     * Add a node the adapter classifies.
     *
     * @param string       $ref         The node's id on the page.
     * @param string       $parent      ROOT or the ref of a node added before.
     * @param string       $kind        One of BuilderContract::KINDS except "locked".
     * @param int|null     $level       Heading level 1 to 6, or null.
     * @param array<mixed> $editable    Fields set_text may change on this node.
     * @param array<mixed> $fromTheSite The node's current site text, field => text.
     * @return void
     * @throws \InvalidArgumentException When the node breaks a rule above.
     */
    public function add(string $ref, string $parent, string $kind, ?int $level, array $editable, array $fromTheSite): void
    {
        $this->checkPlace($ref, $parent);
        if ($kind === self::LOCKED || !in_array($kind, BuilderContract::KINDS, true)) {
            throw new \InvalidArgumentException('a node kind must be a projection kind other than locked');
        }
        if ($level !== null && ($level < 1 || $level > 6)) {
            throw new \InvalidArgumentException('a heading level must be 1 to 6');
        }
        $fields = [];
        foreach ($editable as $field) {
            if (!is_string($field) || !in_array($field, BuilderContract::FIELDS, true) || in_array($field, $fields, true)) {
                throw new \InvalidArgumentException('an editable field must be a distinct contract field');
            }
            $fields[] = $field;
        }
        $text = [];
        foreach ($fromTheSite as $field => $value) {
            if (!is_string($field) || !in_array($field, BuilderContract::FIELDS, true) || !is_string($value)) {
                throw new \InvalidArgumentException('site text must be a string under a contract field');
            }
            $text[$field] = self::validUtf8($value);
        }

        $node = ['ref' => $ref, 'parent' => $parent, 'kind' => $kind];
        if ($level !== null) {
            $node['level'] = $level;
        }
        $node['editable']      = $fields;
        $node['from_the_site'] = (object) $text;

        $this->nodes[]    = $node;
        $this->refs[$ref] = true;
    }

    /**
     * Add a node the adapter does not classify. Its label is the adapter's
     * map text for $labelKey, or the fallback text when the map has no such
     * key; $labelKey itself never appears in the answer.
     *
     * @param string $ref      The node's id on the page.
     * @param string $parent   ROOT or the ref of a node added before.
     * @param string $labelKey The key to look up in the label map.
     * @return void
     * @throws \InvalidArgumentException When the ref or parent breaks a rule above.
     */
    public function addLocked(string $ref, string $parent, string $labelKey): void
    {
        $this->checkPlace($ref, $parent);
        $label = $this->labels[$labelKey] ?? $this->labels[self::FALLBACK_LABEL];

        $this->nodes[]    = ['ref' => $ref, 'parent' => $parent, 'kind' => self::LOCKED, 'label' => $label];
        $this->refs[$ref] = true;
    }

    /**
     * The answer: the total node count, whether nodes were left out, and the
     * nodes kept in page order.
     *
     * @param int $maxNodes Most nodes to keep; clamped to 0..MAX_STRUCTURE_NODES.
     * @param int $maxBytes Most bytes of the encoded answer.
     * @return array{node_count: int, truncated: bool, nodes: list<array<string, mixed>>}
     * @throws \JsonException When a node cannot be encoded.
     */
    public function toArray(int $maxNodes, int $maxBytes = BuilderContract::MAX_STRUCTURE_BYTES): array
    {
        $limit = max(0, min($maxNodes, BuilderContract::MAX_STRUCTURE_NODES));
        $total = count($this->nodes);
        // "false" is the longer of the two values, so the envelope is never undercounted.
        $bytes = strlen(json_encode(['node_count' => $total, 'truncated' => false, 'nodes' => []], JSON_THROW_ON_ERROR));
        $kept  = [];
        foreach ($this->nodes as $node) {
            if (count($kept) >= $limit) {
                break;
            }
            $size = strlen(json_encode($node, JSON_THROW_ON_ERROR)) + ($kept === [] ? 0 : 1);
            if ($bytes + $size > $maxBytes) {
                break;
            }
            $bytes += $size;
            $kept[] = $node;
        }

        return ['node_count' => $total, 'truncated' => count($kept) < $total, 'nodes' => $kept];
    }

    /**
     * @param string $ref    Node ref.
     * @param string $parent Parent ref.
     * @return void
     * @throws \InvalidArgumentException When the ref is malformed, the root or taken, or the parent is unknown.
     */
    private function checkPlace(string $ref, string $parent): void
    {
        if ($ref === self::ROOT || preg_match(BuilderContract::RE_REF, $ref) !== 1) {
            throw new \InvalidArgumentException('a node ref must match the reference pattern and not be the root');
        }
        if (isset($this->refs[$ref])) {
            throw new \InvalidArgumentException('a node ref must be unique on the page');
        }
        if ($parent !== self::ROOT && !isset($this->refs[$parent])) {
            throw new \InvalidArgumentException('a node parent must be the root or a node added before it');
        }
    }

    /**
     * $s with every invalid UTF-8 sequence replaced by U+FFFD.
     *
     * @param string $s Site text.
     * @return string
     */
    private static function validUtf8(string $s): string
    {
        if (preg_match('//u', $s) === 1) {
            return $s;
        }
        $clean = json_decode((string) json_encode($s, JSON_INVALID_UTF8_SUBSTITUTE));

        return is_string($clean) ? $clean : '';
    }
}
