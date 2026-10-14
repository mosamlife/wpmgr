<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\PageCreateBuilder;
use WPMgr\Agent\Support\ArrayShape;

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
 *     position only with into, refs matching RE_REF;
 *   - raw duplicate-key detection over the whole input before decoding;
 *     native operation settings keep their parsed collection shapes,
 *     accept string scalars, and use top-level null only to delete a key; move and native placement also
 *     offer into with an optional first/last position;
 *   - set_text text through wpmgr/page-create's own rule for its field:
 *     "text" the paragraph text rule (at most 5000 characters), "caption" the
 *     image caption rule (500), "alt" the image alt rule (300; no square
 *     bracket at all, may be empty), "url" the button link rule;
 *   - each outline through PageCreateBuilder::validate() as the outline of a
 *     page of its own, so its grammar is page-create's, unchanged, and it
 *     comes back in page-create's normalised form;
 *   - native add/duplicate anchors may be reused, including a duplicate
 *     beside its own source; target refs remain once per call; otherwise
 *     a ref is named at most once in the whole call, as a target or as an
 *     anchor (ops_invalid).
 *   - widget operations and text or outline operations use separate calls (ops_invalid).
 * Every other refusal of parse() is bad_input.
 *
 * againstPage() holds the rules that need the page, checked operation by
 * operation on the page as the earlier operations leave it:
 *   - remove and move target kinds complete the mixed-call check (ops_invalid);
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
    /** Plain next step for outline edits covering native widgets. */
    public const NATIVE_SECTION_HINT = 'This section holds Elementor widgets: remove it and add widgets with widget operations, or edit its text with set_text in a separate edit.';

    /** Plain next step for a call that crosses the widget and outline contracts. */
    public const MIXED_HINT = 'Send widget operations and text or outline operations in separate edits.';

    /** Decode depth of the input text, as for every ability input. */
    private const MAX_DEPTH = 32;

    /** The input fields, in schema order; `design` is the optional last one. */
    private const INPUT_FIELDS = ['post_id', 'base_fingerprint', 'operations', 'design'];

    /** The input fields every input has. */
    private const REQUIRED_FIELDS = ['post_id', 'base_fingerprint', 'operations'];

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
     * @return array{input?: array{post_id: int, base_fingerprint: string, operations: list<array<string, mixed>>, design?: array<string, mixed>}, code?: string, detail?: string, failures?: list<array<string, mixed>>}
     */
    public static function parse(string $inputText): array
    {
        if (strlen($inputText) > BuilderContract::MAX_INPUT_BYTES) {
            return self::bad('the input is longer than ' . BuilderContract::MAX_INPUT_BYTES . ' bytes');
        }
        $raw = NativeSettingsJson::validateDocument($inputText);
        if (!$raw['ok']) {
            if (!str_starts_with($raw['failure']['reason'], 'object keys must be unique')) {
                return self::bad('the input must be the JSON text of an object');
            }
            return Refusal::make(NativeSettingsValidator::CODE, 'the raw page-edit JSON is invalid', [$raw['failure']])
                + ['schema' => ['widget' => '', 'atomic' => false, 'controls' => []]];
        }
        $input = json_decode($inputText, false, self::MAX_DEPTH);
        if (!is_object($input)) {
            return self::bad('the input must be the JSON text of an object');
        }
        $vars = get_object_vars($input);
        $bad  = self::keys($vars, self::INPUT_FIELDS, self::REQUIRED_FIELDS, 'the input');
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
        // The design is judged before the operations, as page-create judges it before the outline.
        $design = null;
        if (array_key_exists('design', $vars)) {
            $checked = DesignInput::check($vars['design']);
            if (!isset($checked['design'])) {
                return $checked;
            }
            $design = $checked['design'];
        }
        $operations = $vars['operations'];
        if (!is_array($operations) || $operations === [] || count($operations) > BuilderContract::MAX_OPS) {
            return self::bad('operations must be a list of 1 to ' . BuilderContract::MAX_OPS . ' operations');
        }

        $named = [];
        $clean = [];
        $showsOn = false;
        foreach (array_values($operations) as $i => $operation) {
            $at = 'operations[' . $i . ']';
            $r  = self::operation($operation, $at);
            if (!isset($r['op'])) {
                return $r;
            }
            if ($r['op']['op'] === 'set_shows_on') {
                if ($showsOn) {
                    return ['code' => 'ops_invalid', 'detail' => 'set_shows_on is allowed at most once per edit'];
                }
                $showsOn = true;
            }
            $references = in_array($r['op']['op'], ['add_native', 'duplicate'], true)
                ? (isset($r['op']['ref']) ? [$r['op']['ref']] : []) : self::refsOf($r['op']);
            foreach ($references as $ref) {
                if (isset($named[$ref])) {
                    return ['code' => 'ops_invalid', 'detail' => $at . ' names ' . $ref . ' again; a call names each node at most once'];
                }
                $named[$ref] = true;
            }
            $clean[] = $r['op'];
        }

        if (self::mixedOperations($clean)) {
            return ['code' => 'ops_invalid', 'detail' => self::MIXED_HINT];
        }

        $input = ['post_id' => $postId, 'base_fingerprint' => $fingerprint, 'operations' => $clean];
        if ($design !== null) {
            $input['design'] = $design;
        }

        return ['input' => $input];
    }

    /** Explicit operations are classified at parse time; remove/move use the page's kinds. */
    private static function mixedOperations(array $ops, array $kinds = []): bool
    {
        $native = false;
        $outline = false;
        foreach ($ops as $op) {
            $name = $op['op'];
            $native = $native || in_array($name, ['add_native', 'set_native', 'duplicate'], true);
            $outline = $outline || in_array($name, ['set_text', 'insert', 'replace'], true);
            if (in_array($name, ['remove', 'move'], true)) {
                $kind = $kinds[$op['ref']] ?? null;
                $native = $native || $kind === 'native';
                $outline = $outline || ($kind !== null && !in_array($kind, ['native', 'locked'], true));
            }
        }
        return $native && $outline;
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
        $page     = new \stdClass();
        $page->anchor = [];
        $page->links  = [];
        $page->heroes = [];
        $page->h1s    = [];
        $page->top    = null;
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
            if ($up === Projection::ROOT && $page->top === null) {
                $page->top = $ref;
            }
            if ($k === 'section' && isset($node['anchor']) && is_string($node['anchor'])) {
                $page->anchor[$ref] = $node['anchor'];
            }
            $links = [];
            foreach (is_array($node['links'] ?? null) ? $node['links'] : [] as $link) {
                if (is_string($link) && strncmp($link, '#', 1) === 0) {
                    $links[] = substr($link, 1);
                }
            }
            if ($links !== []) {
                $page->links[$ref] = $links;
            }
            if ($k === 'pattern' && ($node['pattern'] ?? null) === 'hero') {
                $page->heroes[] = $ref;
            }
            if ($k === 'heading' && ($node['level'] ?? null) === 1) {
                $page->h1s[] = $ref;
            }
        }

        $outlineEdit = false;
        foreach ($input['operations'] as $op) {
            $outlineEdit = $outlineEdit || in_array($op['op'], ['set_text', 'insert', 'replace'], true);
        }
        if ($outlineEdit) {
            foreach ($input['operations'] as $i => $op) {
                if (!in_array($op['op'], ['remove', 'replace'], true)) {
                    continue;
                }
                foreach ($kind as $ref => $k) {
                    if ($k !== 'native') {
                        continue;
                    }
                    $up = $op['op'] === 'replace' ? $ref : ($parent[$ref] ?? null);
                    while (is_string($up)) {
                        if ($up === $op['ref']) {
                            return self::refuse('ops_invalid', self::NATIVE_SECTION_HINT, $i);
                        }
                        $up = $parent[$up] ?? null;
                    }
                }
            }
        }

        if (self::mixedOperations($input['operations'], $kind)) {
            return self::refuse('ops_invalid', self::MIXED_HINT, null);
        }

        $removed   = [];
        $removedBy = [];
        $new       = 0;
        $facts     = [];
        $setLinks  = [];
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
                case 'set_shows_on':
                case 'set_native':
                    break;
                case 'add_native':
                case 'duplicate':
                    $anchor = (string) ($op['after'] ?? $op['before'] ?? $op['into']);
                    $home = isset($op['into']) ? $anchor : $parent[$anchor];
                    if ($home === null || !in_array($kind[$home], self::TAKES_CHILDREN, true)) {
                        return self::refuse('layout_invalid', 'native_widget_parent', $i);
                    }
                    break;
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
                    if ($field === 'url' || substr($field, -strlen('.url')) === '.url') {
                        $text               = (string) $op['text'];
                        $setLinks[$target][] = ['slug' => strncmp($text, '#', 1) === 0 ? substr($text, 1) : null, 'op' => $i];
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
                    $facts[$i] = OutlineV3::facts($op['outline']);
                    $placed    = self::placement($facts[$i], $home, $page, $op, $name, $target, $i);
                    if ($placed !== null) {
                        return $placed;
                    }
                    $parent[self::NEW_KEY . $i] = $home;
                    $weight[self::NEW_KEY . $i] = $count;
                    if ($name === 'replace' && $target !== null) {
                        $removed[$target]   = true;
                        $removedBy[$target] = $i;
                    }
                    break;
                case 'remove':
                    if ($target !== null) {
                        $removed[$target]   = true;
                        $removedBy[$target] = $i;
                    }
                    break;
                case 'move':
                    $anchor = (string) ($op['after'] ?? $op['before'] ?? $op['into']);
                    if ($target === null || self::isWithin($anchor, $target, $parent)) {
                        return self::refuse('node_not_editable', 'into_own_subtree', $i);
                    }
                    $parent[$target] = isset($op['into']) ? $anchor : $parent[$anchor];
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

        return self::wholePage($page, $facts, $setLinks, $kind, $parent, $removed, $removedBy);
    }

    /**
     * The rules for where an insert or a replace puts a designed outline,
     * judged on the page as the earlier operations left it: a section stands
     * at the top only, and on a page of sections only sections do; the hero is
     * the page's first block, and the page has one hero and one level 1
     * heading.
     *
     * @param array<string, mixed> $facts  OutlineV3::facts() of the operation's outline.
     * @param string|null          $home   The ref of the node the new nodes go into; null at the top.
     * @param \stdClass            $page   What the projection says: anchor, links, heroes, h1s, top.
     * @param array<string, mixed> $op     The operation.
     * @param string               $name   Its name.
     * @param string|null          $target Its target ref.
     * @param int                  $i      Its index.
     * @return array{code: string, detail: string, op_index: int|null}|null
     */
    private static function placement(array $facts, ?string $home, \stdClass $page, array $op, string $name, ?string $target, int $i): ?array
    {
        if ($facts['sections'] > 0 && $home !== null) {
            return self::refuse('node_not_editable', 'section_at_top_only', $i);
        }
        if ($facts['sections'] === 0 && $home === null && $page->anchor !== []) {
            return self::refuse('node_not_editable', 'only_sections_at_top', $i);
        }
        if ($facts['heroes'] > 0) {
            $first = ($op['before'] ?? null) === $page->top || ($name === 'replace' && $target === $page->top);
            if ($home !== null || !$first) {
                return self::refuse('hero_misplaced', 'hero_not_first', $i);
            }
        }

        return null;
    }

    /**
     * The rules judged on the page as the whole call leaves it, so the order
     * of the operations never changes the answer:
     *
     *   - a page has one hero and one level 1 heading (layout_invalid);
     *   - an anchor is on the page once (anchor_duplicate);
     *   - a section the call removes or replaces whose anchor a node that
     *     stays still links to is anchor_in_use, naming the linking refs;
     *   - a link the call makes that leads to an anchor the page does not have
     *     is link_anchor_missing. A link no operation touched is not judged,
     *     unless the call took its anchor away.
     *
     * @param \stdClass                          $page      What the projection says.
     * @param array<int, array<string, mixed>>   $facts     OutlineV3::facts() of each outline, by operation.
     * @param array<string, list<array<string, mixed>>> $setLinks The links set_text gave, by node ref.
     * @param array<string, string>              $kind      Kind of every node, by ref.
     * @param array<string, string|null>         $parent    Parent of every node, by ref or new-node key.
     * @param array<string, true>                $removed   Removed and replaced nodes.
     * @param array<string, int>                 $removedBy The operation that removed or replaced a node.
     * @return array{code: string, detail: string, op_index: int|null, refs?: list<string>}|null
     */
    private static function wholePage(\stdClass $page, array $facts, array $setLinks, array $kind, array $parent, array $removed, array $removedBy): ?array
    {
        // Heroes and level 1 headings.
        $heroes = 0;
        $h1s    = 0;
        foreach ($page->heroes as $ref) {
            $heroes += self::gone($ref, $parent, $removed) ? 0 : 1;
        }
        foreach ($page->h1s as $ref) {
            $h1s += self::gone($ref, $parent, $removed) ? 0 : 1;
        }
        foreach ($facts as $i => $f) {
            $heroes += $f['heroes'];
            $h1s    += $f['h1s'];
            if ($heroes > 1) {
                return self::refuse('layout_invalid', 'second_hero', $i);
            }
            if ($h1s > 1) {
                return self::refuse('layout_invalid', 'second_h1', $i);
            }
        }

        // Anchors before and after the call.
        $before = [];
        foreach ($page->anchor as $ref => $slug) {
            $before[$slug] = $ref;
        }
        $have = [];
        foreach ($page->anchor as $ref => $slug) {
            if (!self::gone($ref, $parent, $removed)) {
                $have[$slug] = true;
            }
        }
        foreach ($facts as $i => $f) {
            foreach ($f['anchors'] as $slug) {
                if (isset($have[$slug])) {
                    return self::refuse('anchor_duplicate', 'anchor_twice', $i);
                }
                $have[$slug] = true;
            }
        }

        // Links after the call: [slug, node ref or null, operation or null, made by the call].
        $links = [];
        foreach ($page->links as $ref => $slugs) {
            if (self::gone($ref, $parent, $removed)) {
                continue;
            }
            $set = $setLinks[$ref] ?? [];
            // A button keeps one link, which a set_text replaces; a pattern's other links stay.
            if ($set !== [] && ($kind[$ref] ?? '') !== 'pattern') {
                continue;
            }
            foreach ($slugs as $slug) {
                $links[] = [$slug, $ref, null, false];
            }
        }
        foreach ($setLinks as $ref => $set) {
            if (self::gone((string) $ref, $parent, $removed)) {
                continue;
            }
            foreach ($set as $link) {
                if ($link['slug'] !== null) {
                    $links[] = [$link['slug'], (string) $ref, $link['op'], true];
                }
            }
        }
        foreach ($facts as $i => $f) {
            foreach ($f['links'] as $slug) {
                $links[] = [$slug, null, $i, true];
            }
        }

        $refs  = [];
        $inUse = null;
        foreach ($links as [$slug, $ref, $op]) {
            if (isset($before[$slug]) && !isset($have[$slug])) {
                $inUse ??= $removedBy[$before[$slug]] ?? null;
                if ($ref !== null) {
                    $refs[] = $ref;
                }
            }
        }
        if ($refs !== [] || $inUse !== null) {
            return self::refuse('anchor_in_use', 'section_linked', $inUse) + ['refs' => array_values(array_unique($refs))];
        }
        foreach ($links as [$slug, $ref, $op, $made]) {
            if ($made && !isset($have[$slug])) {
                return self::refuse('link_anchor_missing', 'link_leads_nowhere', $op);
            }
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
            case 'set_shows_on':
                $bad = self::keys($o, ['op', 'rules'], ['op', 'rules'], $at);
                if ($bad !== null) {
                    return $bad;
                }
                $problem = ThemeTemplateCreate::inputProblem($o['rules']);
                return $problem === null ? ['op' => ['op' => $name, 'rules' => $o['rules']]] : self::bad($at . ': ' . $problem);
            case 'set_native':
                $bad = self::keys($o, ['op', 'ref', 'settings'], ['op', 'ref', 'settings'], $at);
                if ($bad !== null) {
                    return $bad;
                }
                $ref = self::ref($o['ref'], $at . '.ref');
                if (is_array($ref)) {
                    return $ref;
                }
                $settings = self::nativeSettings($o['settings'], $at);
                return isset($settings['code']) ? $settings : ['op' => ['op' => $name, 'ref' => $ref, 'settings' => $settings['settings']]];
            case 'add_native':
            case 'duplicate':
                $anchor = self::anchor($o, ['after', 'before', 'into'], $at);
                if (is_array($anchor)) {
                    return $anchor;
                }
                $required = $name === 'add_native' ? ['widget', 'settings'] : ['ref'];
                $bad = self::keys($o, array_merge(['op', $anchor], $anchor === 'into' ? ['position'] : [], $required), array_merge(['op', $anchor], $required), $at);
                if ($bad !== null) {
                    return $bad;
                }
                $clean = ['op' => $name];
                foreach ($name === 'duplicate' ? ['ref', $anchor] : [$anchor] as $key) {
                    $ref = self::ref($o[$key], $at . '.' . $key);
                    if (is_array($ref)) {
                        return $ref;
                    }
                    $clean[$key] = $ref;
                }
                if (array_key_exists('position', $o)) {
                    if (!in_array($o['position'], BuilderContract::POSITIONS, true)) {
                        return self::bad($at . '.position must be first or last');
                    }
                    $clean['position'] = $o['position'];
                }
                if ($name === 'add_native') {
                    if (!is_string($o['widget']) || preg_match('/^[a-z][a-z0-9_-]{0,63}$/D', $o['widget']) !== 1) {
                        return self::bad($at . '.widget must be a classic widget name');
                    }
                    $settings = self::nativeSettings($o['settings'], $at);
                    if (isset($settings['code'])) {
                        return $settings;
                    }
                    $clean['widget'] = $o['widget'];
                    $clean['settings'] = $settings['settings'];
                }
                return ['op' => $clean];
            case 'set_text':
                $bad = self::keys($o, ['op', 'ref', 'field', 'text'], ['op', 'ref', 'field', 'text'], $at);
                if ($bad !== null) {
                    return $bad;
                }
                $field = $o['field'];
                if (!is_string($field) || (!in_array($field, BuilderContract::FIELDS, true) && !self::isPatternPath($field))) {
                    return self::bad($at . '.field must be one of ' . implode(', ', BuilderContract::FIELDS) . ', or a pattern path such as plans.1.price');
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
                $anchor = self::anchor($o, ['after', 'before', 'into'], $at);
                if (is_array($anchor)) {
                    return $anchor;
                }
                $bad = self::keys($o, array_merge(['op', 'ref', $anchor], $anchor === 'into' ? ['position'] : []), ['op', 'ref', $anchor], $at);
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

                $clean = ['op' => $name, 'ref' => $ref, $anchor => $to];
                if (array_key_exists('position', $o)) {
                    if (!in_array($o['position'], BuilderContract::POSITIONS, true)) {
                        return self::bad($at . '.position must be first or last');
                    }
                    $clean['position'] = $o['position'];
                }
                return ['op' => $clean];
        }
    }

    /** Native settings keep collection shapes and string scalar values. */
    private static function nativeSettings($value, string $at): array
    {
        if (!is_object($value) && $value !== []) {
            return self::bad($at . '.settings must be an object or empty list');
        }
        try {
            return ['settings' => self::nativeArrays($value, true)];
        } catch (\InvalidArgumentException $e) {
            return Refusal::make(NativeSettingsValidator::CODE, 'native settings must use stored collection shapes and string scalars', [[
                'path' => $at, 'key' => 'settings', 'reason' => 'use JSON objects for control maps and JSON lists for repeater rows; scalars must be strings, with null only for top-level deletion',
            ]]) + ['schema' => ['widget' => '', 'atomic' => false, 'controls' => []]];
        }
    }

    /** @return mixed */
    private static function nativeArrays($value, bool $top = false, bool $delete = false)
    {
        if (is_object($value)) {
            $map = (array) $value;
            if ((!$top && $map === []) || ($map !== [] && ArrayShape::isList($map))) {
                throw new \InvalidArgumentException('a JSON object must retain object encoding');
            }
            foreach ($map as $key => $child) {
                $map[$key] = self::nativeArrays($child, false, $top);
            }
            return $map;
        }
        if (is_array($value)) {
            return array_map(static fn ($child) => self::nativeArrays($child), $value);
        }
        if (is_string($value) || ($delete && $value === null)) {
            return $value;
        }
        throw new \InvalidArgumentException('native settings scalars must be strings');
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
     * The outline of version 2 is judged as the outline of a page of its own.
     * An outline that version 2 does not accept is judged by the grammar of
     * version 3 (OutlineV3::fragment()): a section at the top, or a pattern, a
     * block, a group and columns as the children of a section, with a link to
     * an anchor of the page allowed. Whatever code a grammar gives, the answer
     * is bad_input; which anchors and links the page has is the page's to judge.
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
        $list = array_values($list);
        $r    = self::holdsDesignedNode($list) ? [] : PageCreateBuilder::validate(self::pageCreateInput($list));
        if (isset($r['spec'])) {
            return ['outline' => $r['spec']['outline']];
        }
        $v3 = OutlineV3::fragment($list);
        if (isset($v3['outline'])) {
            return ['outline' => $v3['outline']];
        }
        // A version 2 outline keeps version 2's words; a designed one gets the grammar's.
        $why    = $r === [] || self::holdsDesignedMark($list) ? $v3 : $r;
        $detail = (string) ($why['detail'] ?? '');

        return self::bad(strncmp($detail, 'outline', 7) === 0 ? $at . '.' . $detail : $at . '.outline: ' . $detail);
    }

    /**
     * Whether an outline holds a section or a pattern at its top: nodes only
     * the grammar of version 3 knows.
     *
     * @param list<mixed> $list Decoded outline.
     * @return bool
     */
    private static function holdsDesignedNode(array $list): bool
    {
        foreach ($list as $node) {
            if (is_object($node) && in_array($node->type ?? null, ['section', 'pattern'], true)) {
                return true;
            }
        }

        return false;
    }

    /**
     * Whether an outline holds anything only version 3 gives a meaning to,
     * so that its refusal is the grammar's: a section or a pattern, a link to
     * an anchor of the page, or the text style of a button.
     *
     * @param list<mixed> $list Decoded outline.
     * @return bool
     */
    private static function holdsDesignedMark(array $list): bool
    {
        $text = json_encode($list);

        return self::holdsDesignedNode($list) || (is_string($text) && (strpos($text, '"url":"#') !== false || strpos($text, '"style":"text"') !== false));
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
        if (self::isPatternPath($field)) {
            return self::pathProblem($field, $text);
        }
        switch ($field) {
            case 'url':
                return self::linkProblem($text);
            case 'alt':
                return self::pageCreateProblem((object) ['type' => 'image', 'attachment_id' => 1, 'alt' => $text]);
            case 'caption':
                return self::pageCreateProblem((object) ['type' => 'image', 'attachment_id' => 1, 'alt' => '', 'caption' => $text]);
            default:
                return self::pageCreateProblem((object) ['type' => 'paragraph', 'text' => $text]);
        }
    }

    /**
     * Whether a field names a position in a pattern, such as plans.1.price,
     * matched whole.
     *
     * @param string $field The field.
     * @return bool
     */
    private static function isPatternPath(string $field): bool
    {
        return preg_match('/' . OutlineV3Schema::EDIT_PATH_PATTERN . '/D', $field) === 1;
    }

    /**
     * Why a text breaks the rule of a pattern path, or null: a path takes the
     * rule of its last step. url is the link rule, alt the alt rule, cta.text
     * the button text rule, any other the paragraph text rule.
     *
     * @param string $path A pattern path.
     * @param string $text The text.
     * @return string|null
     */
    private static function pathProblem(string $path, string $text): ?string
    {
        $last = substr($path, (int) strrpos($path, '.') + 1);
        if ($last === 'url') {
            return self::linkProblem($text);
        }
        if ($last === 'alt') {
            return self::pageCreateProblem((object) ['type' => 'image', 'attachment_id' => 1, 'alt' => $text]);
        }
        if (substr($path, -strlen('.cta.text')) === '.cta.text') {
            return self::pageCreateProblem((object) [
                'type'    => 'buttons',
                'buttons' => [(object) ['text' => $text, 'url' => self::CHECK_LINK]],
            ]);
        }

        return self::pageCreateProblem((object) ['type' => 'paragraph', 'text' => $text]);
    }

    /**
     * Why a link breaks the link rule, or null: page-create's, or a link to a
     * section of the page, "#" and an anchor of the anchor's shape. Whether
     * the page has that anchor is the page's to judge.
     *
     * @param string $url The link.
     * @return string|null
     */
    private static function linkProblem(string $url): ?string
    {
        return PageCreateBuilder::linkProblem($url, true);
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
