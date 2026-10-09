<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\PageCreateBuilder;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The wpmgr/page-edit input rules, in two halves.
 *
 * parse() holds every rule that needs no page, and the control plane applies
 * the same rules before an edit reaches the site:
 *   - the input is the exact JSON text of an object of at most
 *     BuilderContract::MAX_INPUT_BYTES bytes, decoded with objects kept as
 *     objects, so "{}" and "[]" never stand for each other;
 *   - BuilderContract::pageEditInputSchema(), enforced here in code: exactly
 *     the listed fields, 1..25 operations, one anchor per insert or move, a
 *     position only on an insert into a node, refs matching RE_REF;
 *   - set_text text through wpmgr/page-create's own rule for its field:
 *     "text" the paragraph text rule (at most 5000 characters), "caption" the
 *     image caption rule (500), "alt" the image alt rule (300; no square
 *     bracket at all, may be empty), "url" the button link rule;
 *   - each outline through PageCreateBuilder::validate() as the outline of a
 *     page of its own, so its grammar is page-create's, unchanged, and it
 *     comes back in page-create's normalised form;
 *   - a ref is named at most once in the whole call, as a target or as an
 *     anchor (ops_invalid).
 * Every other refusal of parse() is bad_input.
 *
 * againstPage() holds the rules that need the page, checked operation by
 * operation on the page as the earlier operations leave it:
 *   - the adapter declares the operation (op_not_supported_by_builder);
 *   - every ref names a node on the page before the call (node_not_found),
 *     and not one inside a subtree an earlier operation removed or replaced
 *     (ops_invalid);
 *   - a locked node is never the target of an operation, so it is never
 *     changed, replaced, removed or moved; it may be an anchor
 *     (node_not_editable, locked);
 *   - a node that holds a locked node at any depth, on the page as the
 *     earlier operations leave it, is never removed or replaced, so a locked
 *     node never goes with its section or column (node_not_editable,
 *     holds_locked);
 *   - set_text names a field the node offers (node_not_editable), and the
 *     text of a button keeps page-create's button text rule (bad_input);
 *   - an insert into a node needs a node that takes children: a section, a
 *     column, a group or columns (node_not_editable);
 *   - a move never puts a node inside its own subtree (node_not_editable);
 *   - at most MAX_NEW_NODES new nodes, counted the way page-create counts
 *     blocks (every node, every column of a columns node, every button of a
 *     buttons node), and at most MAX_NODES_AFTER nodes on the page after the
 *     call, counted on the projection (page_too_large). The adapter counts
 *     again on the builder's own tree.
 * The detail of an againstPage() refusal is a fixed word, and op_index names
 * the operation (null for the count of the page after the call).
 *
 * Pure: no WordPress call.
 */
final class PageEditValidator
{
    /** Decode depth of the input text, as for every ability input. */
    private const MAX_DEPTH = 32;

    /** The input fields, in schema order. */
    private const INPUT_FIELDS = ['post_id', 'base_fingerprint', 'operations'];

    /** The node kinds an insert may put new nodes into. */
    private const TAKES_CHILDREN = ['section', 'column', 'group', 'columns'];

    /** Title of the page-create input an outline or a text is checked as. */
    private const CHECK_TITLE = 'Edit';

    /** A link that passes the button link rule, beside a button text being checked. */
    private const CHECK_LINK = '/';

    /** Key prefix of the new nodes of one operation; never a ref, as "#" is outside RE_REF. */
    private const NEW_KEY = '#new';

    /**
     * Check the input text against every rule that needs no page.
     *
     * @param string $inputText The exact input text.
     * @return array{input?: array{post_id: int, base_fingerprint: string, operations: list<array<string, mixed>>}, code?: string, detail?: string}
     */
    public static function parse(string $inputText): array
    {
        if (strlen($inputText) > BuilderContract::MAX_INPUT_BYTES) {
            return self::bad('the input is longer than ' . BuilderContract::MAX_INPUT_BYTES . ' bytes');
        }
        $input = json_decode($inputText, false, self::MAX_DEPTH);
        if (!is_object($input)) {
            return self::bad('the input must be the JSON text of an object');
        }
        $vars = get_object_vars($input);
        $bad  = self::keys($vars, self::INPUT_FIELDS, self::INPUT_FIELDS, 'the input');
        if ($bad !== null) {
            return $bad;
        }
        $postId = $vars['post_id'];
        if (!is_int($postId) || $postId < 1) {
            return self::bad('post_id must be a whole number of at least 1');
        }
        $fingerprint = $vars['base_fingerprint'];
        if (!is_string($fingerprint) || preg_match('/' . BuilderContract::FINGERPRINT_PATTERN . '/D', $fingerprint) !== 1) {
            return self::bad('base_fingerprint must be the 64 lowercase hex characters wpmgr/page-structure gave');
        }
        $operations = $vars['operations'];
        if (!is_array($operations) || $operations === [] || count($operations) > BuilderContract::MAX_OPS) {
            return self::bad('operations must be a list of 1 to ' . BuilderContract::MAX_OPS . ' operations');
        }

        $named = [];
        $clean = [];
        foreach (array_values($operations) as $i => $operation) {
            $at = 'operations[' . $i . ']';
            $r  = self::operation($operation, $at);
            if (!isset($r['op'])) {
                return $r;
            }
            foreach (self::refsOf($r['op']) as $ref) {
                if (isset($named[$ref])) {
                    return ['code' => 'ops_invalid', 'detail' => $at . ' names ' . $ref . ' again; a call names each node at most once'];
                }
                $named[$ref] = true;
            }
            $clean[] = $r['op'];
        }

        return ['input' => ['post_id' => $postId, 'base_fingerprint' => $fingerprint, 'operations' => $clean]];
    }

    /**
     * Check parsed operations against the page they edit.
     *
     * @param array{operations: list<array<string, mixed>>} $input           The input parse() returned.
     * @param list<array<string, mixed>>                    $projectionNodes Every node of the page's projection, parent first; never a page-structure answer cut to its caps.
     * @param array{operations?: list<string>}              $capabilities    The adapter's capabilities().
     * @return array{code: string, detail: string, op_index: int|null}|null Null when every rule holds.
     * @throws \InvalidArgumentException When a projection node has no string ref, parent or kind, repeats a ref, or comes before its parent.
     */
    public static function againstPage(array $input, array $projectionNodes, array $capabilities): ?array
    {
        $declared = $capabilities['operations'] ?? [];
        $kind     = [];
        $editable = [];
        $parent   = [];
        $weight   = [];
        foreach ($projectionNodes as $node) {
            $ref = $node['ref'] ?? null;
            $up  = $node['parent'] ?? null;
            $k   = $node['kind'] ?? null;
            if (!is_string($ref) || !is_string($up) || !is_string($k) || $ref === Projection::ROOT || isset($kind[$ref])
                || ($up !== Projection::ROOT && !isset($kind[$up]))) {
                throw new \InvalidArgumentException('a projection node needs a unique ref, a kind and a parent listed before it');
            }
            $fields = $node['editable'] ?? [];
            $kind[$ref]     = $k;
            $editable[$ref] = $k !== 'locked' && is_array($fields) ? $fields : [];
            $parent[$ref]   = $up === Projection::ROOT ? null : $up;
            $weight[$ref]   = 1;
        }

        $removed = [];
        $new     = 0;
        foreach ($input['operations'] as $i => $op) {
            $name = (string) $op['op'];
            if (!in_array($name, $declared, true)) {
                return self::refuse('op_not_supported_by_builder', $name, $i);
            }
            foreach (self::refsOf($op) as $ref) {
                if (!isset($kind[$ref])) {
                    return self::refuse('node_not_found', 'ref_not_on_page', $i);
                }
                if (self::gone($ref, $parent, $removed)) {
                    return self::refuse('ops_invalid', 'ref_gone', $i);
                }
            }
            $target = isset($op['ref']) ? (string) $op['ref'] : null;
            if ($target !== null && $kind[$target] === 'locked') {
                return self::refuse('node_not_editable', 'locked', $i);
            }
            if ($target !== null && ($name === 'remove' || $name === 'replace') && self::holdsLocked($target, $kind, $parent)) {
                return self::refuse('node_not_editable', 'holds_locked', $i);
            }

            switch ($name) {
                case 'set_text':
                    $field = (string) $op['field'];
                    if ($target === null || !in_array($field, $editable[$target], true)) {
                        return self::refuse('node_not_editable', 'field_not_offered', $i);
                    }
                    if ($kind[$target] === 'buttons' && $field === 'text') {
                        $why = self::pageCreateProblem((object) [
                            'type'    => 'buttons',
                            'buttons' => [(object) ['text' => (string) $op['text'], 'url' => self::CHECK_LINK]],
                        ]);
                        if ($why !== null) {
                            return ['code' => 'bad_input', 'detail' => 'operations[' . $i . '].text: ' . $why, 'op_index' => $i];
                        }
                    }
                    break;
                case 'insert':
                case 'replace':
                    if (isset($op['into'])) {
                        $into = (string) $op['into'];
                        if (!in_array($kind[$into], self::TAKES_CHILDREN, true)) {
                            return self::refuse('node_not_editable', 'takes_no_children', $i);
                        }
                        $home = $into;
                    } else {
                        // Beside the anchor, or where the replaced node stood.
                        $beside = (string) ($op['after'] ?? $op['before'] ?? $target);
                        $home   = $parent[$beside] ?? null;
                    }
                    $count = self::countNodes($op['outline']);
                    $new  += $count;
                    if ($new > BuilderContract::MAX_NEW_NODES) {
                        return self::refuse('page_too_large', 'new_nodes', $i);
                    }
                    $parent[self::NEW_KEY . $i] = $home;
                    $weight[self::NEW_KEY . $i] = $count;
                    if ($name === 'replace' && $target !== null) {
                        $removed[$target] = true;
                    }
                    break;
                case 'remove':
                    if ($target !== null) {
                        $removed[$target] = true;
                    }
                    break;
                case 'move':
                    $anchor = (string) ($op['after'] ?? $op['before']);
                    if ($target === null || self::isWithin($anchor, $target, $parent)) {
                        return self::refuse('node_not_editable', 'into_own_subtree', $i);
                    }
                    $parent[$target] = $parent[$anchor];
                    break;
            }
        }

        $after = 0;
        foreach ($weight as $key => $count) {
            if (!self::gone((string) $key, $parent, $removed)) {
                $after += $count;
            }
        }
        if ($after > BuilderContract::MAX_NODES_AFTER) {
            return self::refuse('page_too_large', 'nodes_after', null);
        }

        return null;
    }

    /**
     * New nodes an outline makes, counted the way page-create counts blocks.
     *
     * @param array<mixed> $nodes Outline nodes, in page-create's normalised form.
     * @return int
     */
    public static function countNodes(array $nodes): int
    {
        $count = 0;
        foreach ($nodes as $node) {
            if (!is_array($node)) {
                continue;
            }
            ++$count;
            $type = $node['type'] ?? null;
            if ($type === 'group' && is_array($node['children'] ?? null)) {
                $count += self::countNodes($node['children']);
            } elseif ($type === 'columns' && is_array($node['columns'] ?? null)) {
                foreach ($node['columns'] as $children) {
                    $count += 1 + (is_array($children) ? self::countNodes($children) : 0);
                }
            } elseif ($type === 'buttons' && is_array($node['buttons'] ?? null)) {
                $count += count($node['buttons']);
            }
        }

        return $count;
    }

    // ---------------------------------------------------------------------
    // parse() helpers
    // ---------------------------------------------------------------------

    /**
     * One operation, in its normalised form.
     *
     * @param mixed  $operation Decoded operation.
     * @param string $at        Its path.
     * @return array{op?: array<string, mixed>, code?: string, detail?: string}
     */
    private static function operation($operation, string $at): array
    {
        if (!is_object($operation)) {
            return self::bad($at . ' must be an object');
        }
        $o    = get_object_vars($operation);
        $name = $o['op'] ?? null;
        if (!is_string($name) || !in_array($name, BuilderContract::OPS, true)) {
            return self::bad($at . '.op must be one of ' . implode(', ', BuilderContract::OPS));
        }

        switch ($name) {
            case 'set_text':
                $bad = self::keys($o, ['op', 'ref', 'field', 'text'], ['op', 'ref', 'field', 'text'], $at);
                if ($bad !== null) {
                    return $bad;
                }
                $field = $o['field'];
                if (!is_string($field) || !in_array($field, BuilderContract::FIELDS, true)) {
                    return self::bad($at . '.field must be one of ' . implode(', ', BuilderContract::FIELDS));
                }
                $text = $o['text'];
                if (!is_string($text) || self::chars($text) > PageCreateBuilder::MAX_TEXT_CHARS) {
                    return self::bad($at . '.text must be a string of at most ' . PageCreateBuilder::MAX_TEXT_CHARS . ' characters');
                }
                $why = self::fieldProblem($field, $text);
                if ($why !== null) {
                    return self::bad($at . '.text: ' . $why);
                }
                $ref = self::ref($o['ref'], $at . '.ref');
                if (is_array($ref)) {
                    return $ref;
                }

                return ['op' => ['op' => $name, 'ref' => $ref, 'field' => $field, 'text' => $text]];
            case 'insert':
                $anchor = self::anchor($o, ['after', 'before', 'into'], $at);
                if (is_array($anchor)) {
                    return $anchor;
                }
                if ($anchor !== 'into' && array_key_exists('position', $o)) {
                    return self::bad($at . ': position is only for an insert into a node');
                }
                $allowed = $anchor === 'into' ? ['op', 'into', 'position', 'outline'] : ['op', $anchor, 'outline'];
                $bad     = self::keys($o, $allowed, ['op', $anchor, 'outline'], $at);
                if ($bad !== null) {
                    return $bad;
                }
                $ref = self::ref($o[$anchor], $at . '.' . $anchor);
                if (is_array($ref)) {
                    return $ref;
                }
                $clean = ['op' => $name, $anchor => $ref];
                if (array_key_exists('position', $o)) {
                    if (!is_string($o['position']) || !in_array($o['position'], BuilderContract::POSITIONS, true)) {
                        return self::bad($at . '.position must be first or last');
                    }
                    $clean['position'] = $o['position'];
                }
                $outline = self::outline($o['outline'], $at);
                if (!isset($outline['outline'])) {
                    return $outline;
                }
                $clean['outline'] = $outline['outline'];

                return ['op' => $clean];
            case 'replace':
                $bad = self::keys($o, ['op', 'ref', 'outline'], ['op', 'ref', 'outline'], $at);
                if ($bad !== null) {
                    return $bad;
                }
                $ref = self::ref($o['ref'], $at . '.ref');
                if (is_array($ref)) {
                    return $ref;
                }
                $outline = self::outline($o['outline'], $at);
                if (!isset($outline['outline'])) {
                    return $outline;
                }

                return ['op' => ['op' => $name, 'ref' => $ref, 'outline' => $outline['outline']]];
            case 'remove':
                $bad = self::keys($o, ['op', 'ref'], ['op', 'ref'], $at);
                if ($bad !== null) {
                    return $bad;
                }
                $ref = self::ref($o['ref'], $at . '.ref');
                if (is_array($ref)) {
                    return $ref;
                }

                return ['op' => ['op' => $name, 'ref' => $ref]];
            default:
                $anchor = self::anchor($o, ['after', 'before'], $at);
                if (is_array($anchor)) {
                    return $anchor;
                }
                $bad = self::keys($o, ['op', 'ref', $anchor], ['op', 'ref', $anchor], $at);
                if ($bad !== null) {
                    return $bad;
                }
                $ref = self::ref($o['ref'], $at . '.ref');
                if (is_array($ref)) {
                    return $ref;
                }
                $to = self::ref($o[$anchor], $at . '.' . $anchor);
                if (is_array($to)) {
                    return $to;
                }

                return ['op' => ['op' => $name, 'ref' => $ref, $anchor => $to]];
        }
    }

    /**
     * The one anchor field an insert or a move names.
     *
     * @param array<int|string, mixed> $o       Operation fields.
     * @param list<string>             $anchors The anchor fields this operation may use.
     * @param string                   $at      Its path.
     * @return string|array{code: string, detail: string}
     */
    private static function anchor(array $o, array $anchors, string $at)
    {
        $named = [];
        foreach ($anchors as $anchor) {
            if (array_key_exists($anchor, $o)) {
                $named[] = $anchor;
            }
        }
        if (count($named) !== 1) {
            return self::bad($at . ' must name exactly one of ' . implode(', ', $anchors));
        }

        return $named[0];
    }

    /**
     * An outline through page-create's own validation, in its normalised form.
     *
     * @param mixed  $list Decoded outline.
     * @param string $at   Path of its operation.
     * @return array{outline?: list<array<string, mixed>>, code?: string, detail?: string}
     */
    private static function outline($list, string $at): array
    {
        if (!is_array($list) || $list === [] || count($list) > BuilderContract::MAX_OUTLINE_PER_OP) {
            return self::bad($at . '.outline must be a list of 1 to ' . BuilderContract::MAX_OUTLINE_PER_OP . ' nodes');
        }
        $r = PageCreateBuilder::validate(self::pageCreateInput(array_values($list)));
        if (!isset($r['spec'])) {
            $detail = (string) ($r['detail'] ?? '');

            return self::bad(strncmp($detail, 'outline', 7) === 0 ? $at . '.' . $detail : $at . '.outline: ' . $detail);
        }

        return ['outline' => $r['spec']['outline']];
    }

    /**
     * Why a set_text text breaks page-create's rule for its field, or null.
     *
     * @param string $field One of BuilderContract::FIELDS.
     * @param string $text  The text.
     * @return string|null
     */
    private static function fieldProblem(string $field, string $text): ?string
    {
        switch ($field) {
            case 'url':
                return PageCreateBuilder::linkProblem($text);
            case 'alt':
                return self::pageCreateProblem((object) ['type' => 'image', 'attachment_id' => 1, 'alt' => $text]);
            case 'caption':
                return self::pageCreateProblem((object) ['type' => 'image', 'attachment_id' => 1, 'alt' => '', 'caption' => $text]);
            default:
                return self::pageCreateProblem((object) ['type' => 'paragraph', 'text' => $text]);
        }
    }

    /**
     * Why page-create refuses one outline node, or null; the reason without
     * the node's path.
     *
     * @param object $node Outline node.
     * @return string|null
     */
    private static function pageCreateProblem(object $node): ?string
    {
        $r = PageCreateBuilder::validate(self::pageCreateInput([$node]));
        if (isset($r['spec'])) {
            return null;
        }
        $detail = (string) ($r['detail'] ?? '');
        $colon  = strpos($detail, ': ');

        return $colon === false ? $detail : substr($detail, $colon + 2);
    }

    /**
     * A page-create input whose outline is $outline.
     *
     * @param list<mixed> $outline Outline nodes.
     * @return object
     */
    private static function pageCreateInput(array $outline): object
    {
        return (object) [
            'post_type' => 'page',
            'editor'    => PageCreateBuilder::EDITOR_BLOCKS,
            'title'     => self::CHECK_TITLE,
            'outline'   => $outline,
        ];
    }

    /**
     * Exactly the allowed fields, and every required one present.
     *
     * @param array<int|string, mixed> $fields   Decoded fields.
     * @param list<string>             $allowed  Allowed fields.
     * @param list<string>             $required Required fields.
     * @param string                   $at       Path.
     * @return array{code: string, detail: string}|null
     */
    private static function keys(array $fields, array $allowed, array $required, string $at): ?array
    {
        foreach (array_keys($fields) as $key) {
            if (!in_array((string) $key, $allowed, true)) {
                return self::bad($at . ' has a field outside ' . implode(', ', $allowed));
            }
        }
        foreach ($required as $key) {
            if (!array_key_exists($key, $fields)) {
                return self::bad($at . ' is missing ' . $key);
            }
        }

        return null;
    }

    /**
     * A node reference, or the refusal.
     *
     * @param mixed  $value Decoded value.
     * @param string $at    Path.
     * @return string|array{code: string, detail: string}
     */
    private static function ref($value, string $at)
    {
        if (!is_string($value) || preg_match(BuilderContract::RE_REF, $value) !== 1) {
            return self::bad($at . ' must be a node ref from wpmgr/page-structure: 1 to 32 letters, digits, "_" or "-"');
        }

        return $value;
    }

    /**
     * Characters, counted the way page-create counts them.
     *
     * @param string $text Text.
     * @return int
     */
    private static function chars(string $text): int
    {
        return function_exists('mb_strlen') ? mb_strlen($text, 'UTF-8') : strlen($text);
    }

    // ---------------------------------------------------------------------
    // againstPage() helpers
    // ---------------------------------------------------------------------

    /**
     * Every ref an operation names: its target, then its anchor.
     *
     * @param array<string, mixed> $op Normalised operation.
     * @return list<string>
     */
    private static function refsOf(array $op): array
    {
        $refs = [];
        foreach (['ref', 'after', 'before', 'into'] as $field) {
            if (isset($op[$field]) && is_string($op[$field])) {
                $refs[] = $op[$field];
            }
        }

        return $refs;
    }

    /**
     * Whether $key, or any node above it as the earlier operations left the
     * page, was removed or replaced.
     *
     * @param string                    $key     Node ref or new-node key.
     * @param array<string, string|null> $parent  Current parent of every node; null at the top.
     * @param array<string, true>        $removed Removed and replaced nodes.
     * @return bool
     */
    private static function gone(string $key, array $parent, array $removed): bool
    {
        for ($at = $key; $at !== null; $at = $parent[$at] ?? null) {
            if (isset($removed[$at])) {
                return true;
            }
        }

        return false;
    }

    /**
     * Whether $node is $ancestor or lies inside its subtree, as the earlier
     * operations left the page.
     *
     * @param string                     $node     Node ref.
     * @param string                     $ancestor Node ref.
     * @param array<string, string|null> $parent   Current parent of every node; null at the top.
     * @return bool
     */
    private static function isWithin(string $node, string $ancestor, array $parent): bool
    {
        for ($at = $node; $at !== null; $at = $parent[$at] ?? null) {
            if ($at === $ancestor) {
                return true;
            }
        }

        return false;
    }

    /**
     * Whether a locked node lies inside $ancestor's subtree, at any depth, as
     * the earlier operations left the page.
     *
     * @param string                     $ancestor Node ref.
     * @param array<string, string>      $kind     Kind of every node of the page before the call, by ref.
     * @param array<string, string|null> $parent   Current parent of every node; null at the top.
     * @return bool
     */
    private static function holdsLocked(string $ancestor, array $kind, array $parent): bool
    {
        foreach ($kind as $ref => $k) {
            $ref = (string) $ref;
            if ($k === 'locked' && $ref !== $ancestor && self::isWithin($ref, $ancestor, $parent)) {
                return true;
            }
        }

        return false;
    }

    /**
     * @param string $detail Detail.
     * @return array{code: string, detail: string}
     */
    private static function bad(string $detail): array
    {
        return ['code' => 'bad_input', 'detail' => $detail];
    }

    /**
     * @param string   $code    Code.
     * @param string   $detail  Fixed detail word.
     * @param int|null $opIndex The operation, or null for the whole call.
     * @return array{code: string, detail: string, op_index: int|null}
     */
    private static function refuse(string $code, string $detail, ?int $opIndex): array
    {
        return ['code' => $code, 'detail' => $detail, 'op_index' => $opIndex];
    }
}
