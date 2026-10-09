<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * wpmgr/page-edit operations applied to a stored Elementor classic tree.
 *
 * apply() takes the decoded stored tree and the operations as
 * PageEditValidator::parse() normalised them, and applies them in order,
 * each to the tree the earlier ones left:
 *
 * - Before any operation, every id on the page, inside locked elements too,
 *   is reserved in the request's IdSeed, so no new id equals one on the page.
 * - A ref names a node of the page's projection before the call: never a
 *   node inside a locked element, never one an operation made. A ref that is
 *   not on the page is node_not_found; one that an earlier operation removed
 *   or replaced, or that sat inside such a node, is ops_invalid. A locked
 *   node is never a target (node_not_editable); it may be an anchor.
 * - set_text: ElementorClassicMapper::setText() on a node that offers the
 *   field (node_not_editable otherwise).
 * - insert and replace: the outline maps through
 *   ElementorClassicMapper::mapFragment() for the place it goes. At the top
 *   of the page the new nodes take the site's layout (containers, or
 *   sections and columns); inside a top-level element that holds widgets they
 *   are leaves or an inner row; inside an inner element, leaves only; inside
 *   an element they take that element's layout. A section or a row of
 *   columns holds only its columns, so no new node goes straight into one
 *   (layout_invalid). An insert into a node without a position goes last.
 * - remove: the node and everything inside it.
 * - move: the node, unchanged, beside its anchor. A node moves only among
 *   the nodes of its own place: the top of the page, the columns of a row or
 *   section, or what a group or column holds; a layout element moves only
 *   where it may stand in the same layout, and an inner row only where a new
 *   one may go (layout_invalid). An anchor inside the moved node is
 *   node_not_editable.
 * - A node no operation changes keeps every key, value and place; only the
 *   element list of a node above a change is rebuilt, its other keys as they
 *   were.
 * - Counted on the tree: at most BuilderContract::MAX_NEW_NODES elements
 *   made (wrappers and columns included), at most MAX_NODES_AFTER elements
 *   on the page after the call (the insides of locked elements included), and
 *   json_encode() of the new tree at most MAX_DOCUMENT_BYTES bytes; else
 *   page_too_large, detail new_nodes, nodes_after or document_bytes.
 * - A stored tree that is not a list of element objects at every level, or
 *   that cannot be projected, is data_unreadable.
 *
 * The answer: tree; changes, one per operation in order; touched, the refs
 * whose text changed and the refs made, in operation order; new_count, the
 * elements made. A refusal: code, detail (WPMgr's words and positions, never
 * site text) and op_index (null for a rule over the whole call).
 *
 * changes:
 *   set_text  {op, ref, kind, level?, before: {field: text}, after: {field: text}}
 *   insert    {op, new_refs, anchor: {ref, how, position?, kind, level?, label?}}
 *   replace   {op, ref, kind, level?, before?, new_refs}
 *   remove    {op, ref, kind, level?, before?}
 *   move      {op, ref, kind, level?, before?, anchor: {ref, how, kind, level?, label?}}
 * before is the node's text as the projection's from_the_site gives it: site
 * text. after is the operation's text. how is after, before or into; position
 * (first or last) is given for into only. label is WPMgr's words for a locked
 * anchor. new_refs lists every element made, parent first.
 *
 * Pure: no WordPress write. The only WordPress function reached is the HTML
 * sanitiser, through LeafPolicy.
 */
final class LayoutOps
{
    /** A stored tree that cannot be edited as a tree. */
    public const CODE_UNREADABLE = 'data_unreadable';

    /** New or moved nodes where the page's layout does not take them. */
    public const CODE_PLACEMENT = 'layout_invalid';

    /** Element types that hold other elements. */
    private const LAYOUT_TYPES = ['container', 'section', 'column'];

    /** Projection kinds that hold only columns. */
    private const HOLDS_COLUMNS = ['columns', 'section'];

    /** Projection kinds an insert may put new nodes into. */
    private const TAKES_CHILDREN = ['section', 'column', 'group', 'columns'];

    /** Where a node stands: the top of the page, among columns, or in content. */
    private const SLOT_TOP = 'top';

    private const SLOT_COLUMN = 'column';

    private const SLOT_CONTENT = 'content';

    /**
     * Apply the operations to the stored tree.
     *
     * @param array<mixed>                $tree       Decoded stored tree.
     * @param list<array<string, mixed>>  $ops        Operations as PageEditValidator::parse() normalised them.
     * @param IdSeed                      $ids        Node ids for this request.
     * @param array<mixed>                $mediaById  Media facts by attachment id, for new images.
     * @param bool                        $containers Whether the site has Elementor containers on.
     * @return array{tree?: list<array<string, mixed>>, changes?: list<array<string, mixed>>, touched?: list<string>, new_count?: int, code?: string, detail?: string, op_index?: int|null}
     */
    public static function apply(array $tree, array $ops, IdSeed $ids, array $mediaById, bool $containers): array
    {
        $pageIds = [];
        if (!self::walkIds($tree, 1, $pageIds)) {
            return self::refuse(self::CODE_UNREADABLE, 'the stored page is not a list of elements at every level', null);
        }
        $ids->reserve($pageIds);
        try {
            $projected = ElementorClassicMapper::project($tree)->nodes();
        } catch (\InvalidArgumentException $e) {
            return self::refuse(self::CODE_UNREADABLE, 'an element of the stored page has no usable id', null);
        }
        $facts = [];
        foreach ($projected as $node) {
            $facts[(string) $node['ref']] = $node;
        }

        $changes = [];
        $touched = [];
        $made    = 0;
        foreach (array_values($ops) as $i => $op) {
            $r = is_array($op)
                ? self::one($tree, $op, $i, $facts, $ids, $mediaById, $containers)
                : self::refuse('bad_input', 'operations[' . $i . ']: not an operation', $i);
            if (!isset($r['tree'])) {
                return $r;
            }
            $tree      = $r['tree'];
            $changes[] = $r['change'];
            array_push($touched, ...$r['touched']);
            $made += $r['made'];
            if ($made > BuilderContract::MAX_NEW_NODES) {
                return self::refuse('page_too_large', 'new_nodes', $i);
            }
        }
        if (self::countElements($tree) > BuilderContract::MAX_NODES_AFTER) {
            return self::refuse('page_too_large', 'nodes_after', null);
        }
        $bytes = json_encode($tree);
        if (!is_string($bytes)) {
            return self::refuse(self::CODE_UNREADABLE, 'the edited page cannot be encoded', null);
        }
        if (strlen($bytes) > BuilderContract::MAX_DOCUMENT_BYTES) {
            return self::refuse('page_too_large', 'document_bytes', null);
        }

        return ['tree' => $tree, 'changes' => $changes, 'touched' => $touched, 'new_count' => $made];
    }

    /**
     * One operation on the current tree.
     *
     * @param list<array<string, mixed>>         $tree       Current tree.
     * @param array<mixed>                       $op         Normalised operation.
     * @param int                                $i          Its index.
     * @param array<string, array<string, mixed>> $facts      Projection nodes of the page before the call, by ref.
     * @param IdSeed                             $ids        Node ids.
     * @param array<mixed>                       $media      Media facts.
     * @param bool                               $containers The site's layout for top-level nodes.
     * @return array<string, mixed> tree, change, touched and made; or a refusal.
     */
    private static function one(array $tree, array $op, int $i, array $facts, IdSeed $ids, array $media, bool $containers): array
    {
        $at   = 'operations[' . $i . ']';
        $name = $op['op'] ?? null;
        if ($name === 'insert') {
            return self::insert($tree, $op, $i, $facts, $ids, $media, $containers);
        }
        if (!in_array($name, ['set_text', 'replace', 'remove', 'move'], true)) {
            return self::refuse('bad_input', $at . '.op: not an operation', $i);
        }
        $ref   = is_string($op['ref'] ?? null) ? $op['ref'] : '';
        $found = self::locate($tree, $ref, $facts, $i);
        if (!isset($found['path'])) {
            return $found;
        }
        $path = $found['path'];
        $fact = $facts[$ref];
        if ($fact['kind'] === 'locked') {
            return self::refuse('node_not_editable', 'locked', $i);
        }
        $parentPath = array_slice($path, 0, -1);
        $index      = $path[count($path) - 1];
        $change     = self::described($name, $fact);
        $siteText   = self::siteText($fact);

        switch ($name) {
            case 'set_text':
                $field = is_string($op['field'] ?? null) ? $op['field'] : '';
                $text  = $op['text'] ?? null;
                if (!is_string($text)) {
                    return self::refuse('bad_input', $at . '.text: not a string', $i);
                }
                if (!in_array($field, (array) ($fact['editable'] ?? []), true)) {
                    return self::refuse('node_not_editable', 'field_not_offered', $i);
                }
                $set = ElementorClassicMapper::setText(self::nodeAt($tree, $path), (string) $fact['kind'], $field, $text, $at);
                if (!isset($set['node'])) {
                    return self::refuse($set['code'] ?? 'node_not_editable', $set['detail'] ?? $at, $i);
                }
                $change['before'] = [$field => $siteText[$field] ?? ''];
                $change['after']  = [$field => $text];

                return self::done(self::splice($tree, $parentPath, $index, 1, [$set['node']]), $change, [$ref], []);
            case 'replace':
                $place = self::place($tree, $parentPath, $facts, $containers, $at, $i);
                if (!isset($place['context'])) {
                    return $place;
                }
                $mapped = self::mapped($op, $ids, $media, $place, $at, $i);
                if (!isset($mapped['tree'])) {
                    return $mapped;
                }
                if ($siteText !== []) {
                    $change['before'] = $siteText;
                }
                $made               = self::idsOf($mapped['tree']);
                $change['new_refs'] = $made;

                return self::done(self::splice($tree, $parentPath, $index, 1, $mapped['tree']), $change, $made, $made);
            case 'remove':
                if ($siteText !== []) {
                    $change['before'] = $siteText;
                }

                return self::done(self::splice($tree, $parentPath, $index, 1, []), $change, [], []);
        }

        return self::move($tree, $op, $i, $facts, $containers, $path, $change, $siteText);
    }

    /**
     * An insert after, before or into its anchor.
     *
     * @param list<array<string, mixed>>         $tree       Current tree.
     * @param array<mixed>                       $op         Normalised operation.
     * @param int                                $i          Its index.
     * @param array<string, array<string, mixed>> $facts      Projection nodes by ref.
     * @param IdSeed                             $ids        Node ids.
     * @param array<mixed>                       $media      Media facts.
     * @param bool                               $containers The site's layout for top-level nodes.
     * @return array<string, mixed>
     */
    private static function insert(array $tree, array $op, int $i, array $facts, IdSeed $ids, array $media, bool $containers): array
    {
        $at  = 'operations[' . $i . ']';
        $how = null;
        foreach (['after', 'before', 'into'] as $field) {
            if (isset($op[$field])) {
                $how = $field;
                break;
            }
        }
        if ($how === null || !is_string($op[$how])) {
            return self::refuse('bad_input', $at . ': an insert names one anchor', $i);
        }
        $anchorRef = $op[$how];
        $found     = self::locate($tree, $anchorRef, $facts, $i);
        if (!isset($found['path'])) {
            return $found;
        }
        $anchorPath = $found['path'];
        $anchor     = $facts[$anchorRef];
        $position   = null;
        if ($how === 'into') {
            if (!in_array($anchor['kind'], self::TAKES_CHILDREN, true)) {
                return self::refuse('node_not_editable', 'takes_no_children', $i);
            }
            $position   = ($op['position'] ?? null) === 'first' ? 'first' : 'last';
            $parentPath = $anchorPath;
            $children   = self::nodeAt($tree, $anchorPath)['elements'] ?? [];
            $index      = $position === 'first' ? 0 : (is_array($children) ? count($children) : 0);
        } else {
            $parentPath = array_slice($anchorPath, 0, -1);
            $index      = $anchorPath[count($anchorPath) - 1] + ($how === 'after' ? 1 : 0);
        }
        $place = self::place($tree, $parentPath, $facts, $containers, $at, $i);
        if (!isset($place['context'])) {
            return $place;
        }
        $mapped = self::mapped($op, $ids, $media, $place, $at, $i);
        if (!isset($mapped['tree'])) {
            return $mapped;
        }
        $made   = self::idsOf($mapped['tree']);
        $change = ['op' => 'insert', 'new_refs' => $made, 'anchor' => self::anchor($anchor, $how, $position)];

        return self::done(self::splice($tree, $parentPath, $index, 0, $mapped['tree']), $change, $made, $made);
    }

    /**
     * A move of a node, unchanged, beside its anchor.
     *
     * @param list<array<string, mixed>>         $tree       Current tree.
     * @param array<mixed>                       $op         Normalised operation.
     * @param int                                $i          Its index.
     * @param array<string, array<string, mixed>> $facts      Projection nodes by ref.
     * @param bool                               $containers The site's layout for top-level nodes.
     * @param list<int>                          $path       Where the moved node is.
     * @param array<string, mixed>               $change     The change so far.
     * @param array<string, string>              $siteText   The moved node's site text.
     * @return array<string, mixed>
     */
    private static function move(array $tree, array $op, int $i, array $facts, bool $containers, array $path, array $change, array $siteText): array
    {
        $at  = 'operations[' . $i . ']';
        $how = isset($op['after']) ? 'after' : 'before';
        if (!is_string($op[$how] ?? null)) {
            return self::refuse('bad_input', $at . ': a move names one anchor', $i);
        }
        $anchorRef = $op[$how];
        $found     = self::locate($tree, $anchorRef, $facts, $i);
        if (!isset($found['path'])) {
            return $found;
        }
        $anchorPath = $found['path'];
        if (array_slice($anchorPath, 0, count($path)) === $path) {
            return self::refuse('node_not_editable', 'into_own_subtree', $i);
        }
        $moved = self::nodeAt($tree, $path);
        $from  = array_slice($path, 0, -1);
        $to    = array_slice($anchorPath, 0, -1);
        if (self::slot($tree, $from, $facts) !== self::slot($tree, $to, $facts) || !self::fits($tree, $to, $moved, $facts, $containers)) {
            return self::refuse(self::CODE_PLACEMENT, $at . ': a node moves only among the nodes of its own place (the top of the page, the columns of one row or section, or what a group or column holds) and only where its layout may stand', $i);
        }

        $tree     = self::splice($tree, $from, $path[count($path) - 1], 1, []);
        $newPlace = self::find($tree, $anchorRef, $facts);
        if ($newPlace === null) {
            return self::refuse('ops_invalid', 'ref_gone', $i);
        }
        $index = $newPlace[count($newPlace) - 1] + ($how === 'after' ? 1 : 0);
        if ($siteText !== []) {
            $change['before'] = $siteText;
        }
        $change['anchor'] = self::anchor($facts[$anchorRef], $how, null);

        return self::done(self::splice($tree, array_slice($newPlace, 0, -1), $index, 0, [$moved]), $change, [], []);
    }

    /**
     * The new nodes of an insert or replace, mapped for their place.
     *
     * @param array<mixed>                                 $op    Normalised operation.
     * @param IdSeed                                       $ids   Node ids.
     * @param array<mixed>                                 $media Media facts.
     * @param array{context: string, containers: bool}     $place Where they go.
     * @param string                                       $at    The operation's position.
     * @param int                                          $i     Its index.
     * @return array<string, mixed>
     */
    private static function mapped(array $op, IdSeed $ids, array $media, array $place, string $at, int $i): array
    {
        $outline = is_array($op['outline'] ?? null) ? $op['outline'] : [];
        $mapped  = ElementorClassicMapper::mapFragment($outline, $ids, $media, $place['containers'], $place['context'], $at . '.');
        if (!isset($mapped['tree'])) {
            return self::refuse($mapped['code'] ?? 'bad_input', $mapped['detail'] ?? $at . '.outline', $i);
        }

        return $mapped;
    }

    /**
     * Where new nodes may go as children of the element at $parentPath: the
     * mapping context and the layout they take.
     *
     * @param list<array<string, mixed>>         $tree       Current tree.
     * @param list<int>                          $parentPath The parent element, or [] for the top of the page.
     * @param array<string, array<string, mixed>> $facts      Projection nodes by ref.
     * @param bool                               $containers The site's layout for top-level nodes.
     * @param string                             $at         The operation's position.
     * @param int                                $i          Its index.
     * @return array<string, mixed> context and containers; or a refusal.
     */
    private static function place(array $tree, array $parentPath, array $facts, bool $containers, string $at, int $i): array
    {
        if ($parentPath === []) {
            return ['context' => ElementorClassicMapper::CONTEXT_TOP, 'containers' => $containers];
        }
        $parent = self::nodeAt($tree, $parentPath);
        $elType = $parent['elType'] ?? null;
        $kind   = self::kindOf($parent, $facts);
        if (in_array($kind, self::HOLDS_COLUMNS, true)) {
            return self::refuse(self::CODE_PLACEMENT, $at . ': a ' . ($kind === 'section' ? 'section' : 'row of columns') . ' holds only its columns; put new nodes into one of its columns, or beside it', $i);
        }
        if ($elType === 'container' && in_array($kind, ['group', 'column'], true)) {
            $top = count($parentPath) === 1 && $kind === 'group';

            return ['context' => $top ? ElementorClassicMapper::CONTEXT_GROUP : ElementorClassicMapper::CONTEXT_INNER, 'containers' => true];
        }
        if ($elType === 'column' && $kind === 'column') {
            $top = count($parentPath) === 2 && (self::nodeAt($tree, [$parentPath[0]])['elType'] ?? null) === 'section';

            return ['context' => $top ? ElementorClassicMapper::CONTEXT_GROUP : ElementorClassicMapper::CONTEXT_INNER, 'containers' => false];
        }

        return self::refuse(self::CODE_PLACEMENT, $at . ': new nodes cannot go there', $i);
    }

    /**
     * Where the children of the element at $parentPath stand.
     *
     * @param list<array<string, mixed>>         $tree       Current tree.
     * @param list<int>                          $parentPath The parent element, or [] for the top of the page.
     * @param array<string, array<string, mixed>> $facts      Projection nodes by ref.
     * @return string One of the SLOT_ values.
     */
    private static function slot(array $tree, array $parentPath, array $facts): string
    {
        if ($parentPath === []) {
            return self::SLOT_TOP;
        }

        return in_array(self::kindOf(self::nodeAt($tree, $parentPath), $facts), self::HOLDS_COLUMNS, true) ? self::SLOT_COLUMN : self::SLOT_CONTENT;
    }

    /**
     * Whether $moved may stand as a child of the element at $to: an element
     * at the top is a container or a section; a column goes into a section
     * and a column container into a row container; a widget into anything
     * that holds content; an inner row only where a new one may go, in the
     * same layout.
     *
     * @param list<array<string, mixed>>         $tree       Current tree.
     * @param list<int>                          $to         The new parent, or [] for the top of the page.
     * @param array<mixed>                       $moved      The moved node.
     * @param array<string, array<string, mixed>> $facts      Projection nodes by ref.
     * @param bool                               $containers The site's layout for top-level nodes.
     * @return bool
     */
    private static function fits(array $tree, array $to, array $moved, array $facts, bool $containers): bool
    {
        $elType = $moved['elType'] ?? null;
        if ($to === []) {
            return in_array($elType, ['container', 'section'], true);
        }
        $parent     = self::nodeAt($tree, $to);
        $parentType = $parent['elType'] ?? null;
        if (in_array(self::kindOf($parent, $facts), self::HOLDS_COLUMNS, true)) {
            return $parentType === 'section' ? $elType === 'column' : $elType === 'container';
        }
        if (!in_array($elType, self::LAYOUT_TYPES, true)) {
            return true;
        }
        $place = self::place($tree, $to, $facts, $containers, '', 0);

        return ($place['context'] ?? null) === ElementorClassicMapper::CONTEXT_GROUP
            && $elType === (($place['containers'] ?? null) === true ? 'container' : 'section');
    }

    /**
     * Where the node with $ref is now, or the refusal.
     *
     * @param list<array<string, mixed>>         $tree  Current tree.
     * @param string                             $ref   Ref.
     * @param array<string, array<string, mixed>> $facts Projection nodes by ref.
     * @param int                                $i     The operation's index.
     * @return array<string, mixed> path; or a refusal.
     */
    private static function locate(array $tree, string $ref, array $facts, int $i): array
    {
        if ($ref === '' || !isset($facts[$ref])) {
            return self::refuse('node_not_found', 'ref_not_on_page', $i);
        }
        $path = self::find($tree, $ref, $facts);
        if ($path === null) {
            return self::refuse('ops_invalid', 'ref_gone', $i);
        }

        return ['path' => $path];
    }

    /**
     * The index path of the element with $ref, looked for only where a ref
     * can be: among projected nodes, never inside a locked element or one an
     * operation made.
     *
     * @param array<mixed>                       $list  Elements.
     * @param string                             $ref   Ref.
     * @param array<string, array<string, mixed>> $facts Projection nodes by ref.
     * @return list<int>|null
     */
    private static function find(array $list, string $ref, array $facts): ?array
    {
        foreach ($list as $k => $node) {
            $id = is_array($node) ? ($node['id'] ?? null) : null;
            if (!is_string($id) || !isset($facts[$id])) {
                continue;
            }
            if ($id === $ref) {
                return [(int) $k];
            }
            if ($facts[$id]['kind'] !== 'locked' && in_array($node['elType'] ?? null, self::LAYOUT_TYPES, true) && is_array($node['elements'] ?? null)) {
                $below = self::find($node['elements'], $ref, $facts);
                if ($below !== null) {
                    return array_merge([(int) $k], $below);
                }
            }
        }

        return null;
    }

    /**
     * The element at $path.
     *
     * @param array<mixed> $list Top-level elements.
     * @param list<int>    $path Index path.
     * @return array<mixed>
     */
    private static function nodeAt(array $list, array $path): array
    {
        $node = $list[$path[0]];
        foreach (array_slice($path, 1) as $k) {
            $node = $node['elements'][$k];
        }

        return is_array($node) ? $node : [];
    }

    /**
     * $list with $length elements at $index of the element list at
     * $parentPath replaced by $nodes. Only the element lists on the way down
     * are rebuilt; every other key of every node keeps its value and place.
     *
     * @param array<mixed> $list       Elements.
     * @param list<int>    $parentPath The parent element, or [] for $list itself.
     * @param int          $index      Position in the parent's list.
     * @param int          $length     Elements taken out.
     * @param array<mixed> $nodes      Elements put in.
     * @return list<array<string, mixed>>
     */
    private static function splice(array $list, array $parentPath, int $index, int $length, array $nodes): array
    {
        if ($parentPath === []) {
            return array_merge(array_slice($list, 0, $index), array_values($nodes), array_slice($list, $index + $length));
        }
        $k                = $parentPath[0];
        $node             = $list[$k];
        $node['elements'] = self::splice(is_array($node['elements'] ?? null) ? $node['elements'] : [], array_slice($parentPath, 1), $index, $length, $nodes);
        $list[$k]         = $node;

        return $list;
    }

    /**
     * Every id on the page, the insides of locked elements included. False
     * when the tree is not a list of element objects at every level.
     *
     * @param array<mixed>          $list  Elements.
     * @param int                   $depth Nesting depth of the list.
     * @param list<string|int>      $ids   Ids found so far.
     * @return bool
     */
    private static function walkIds(array $list, int $depth, array &$ids): bool
    {
        if ($depth > ElementorDocument::MAX_DEPTH || !ArrayShape::isList($list)) {
            return false;
        }
        foreach ($list as $node) {
            if (!is_array($node) || $node === [] || ArrayShape::isList($node)) {
                return false;
            }
            $id = $node['id'] ?? null;
            if (is_string($id) || is_int($id)) {
                $ids[] = $id;
            }
            if (array_key_exists('elements', $node) && (!is_array($node['elements']) || !self::walkIds($node['elements'], $depth + 1, $ids))) {
                return false;
            }
        }

        return true;
    }

    /**
     * Elements in a tree, at every level.
     *
     * @param array<mixed> $list Elements.
     * @return int
     */
    private static function countElements(array $list): int
    {
        $count = 0;
        foreach ($list as $node) {
            $children = is_array($node) ? ($node['elements'] ?? null) : null;
            $count   += 1 + (is_array($children) ? self::countElements($children) : 0);
        }

        return $count;
    }

    /**
     * The ids of a tree's elements, parent first.
     *
     * @param array<mixed> $list Elements.
     * @return list<string>
     */
    private static function idsOf(array $list): array
    {
        $ids = [];
        foreach ($list as $node) {
            if (!is_array($node)) {
                continue;
            }
            if (is_string($node['id'] ?? null)) {
                $ids[] = $node['id'];
            }
            if (is_array($node['elements'] ?? null)) {
                array_push($ids, ...self::idsOf($node['elements']));
            }
        }

        return $ids;
    }

    /**
     * The projection kind of a stored element of the page before the call.
     *
     * @param array<mixed>                       $node  Stored element.
     * @param array<string, array<string, mixed>> $facts Projection nodes by ref.
     * @return string|null
     */
    private static function kindOf(array $node, array $facts): ?string
    {
        $id = $node['id'] ?? null;
        if (!is_string($id) || !isset($facts[$id])) {
            return null;
        }
        $kind = $facts[$id]['kind'] ?? null;

        return is_string($kind) ? $kind : null;
    }

    /**
     * The start of a change: the operation and its target.
     *
     * @param string               $op   Operation.
     * @param array<string, mixed> $fact The target's projection node.
     * @return array<string, mixed>
     */
    private static function described(string $op, array $fact): array
    {
        $change = ['op' => $op, 'ref' => (string) $fact['ref'], 'kind' => (string) $fact['kind']];
        if (isset($fact['level'])) {
            $change['level'] = $fact['level'];
        }

        return $change;
    }

    /**
     * An anchor as a change names it.
     *
     * @param array<string, mixed> $fact     The anchor's projection node.
     * @param string               $how      after, before or into.
     * @param string|null          $position first or last, for into.
     * @return array<string, mixed>
     */
    private static function anchor(array $fact, string $how, ?string $position): array
    {
        $anchor = ['ref' => (string) $fact['ref'], 'how' => $how];
        if ($position !== null) {
            $anchor['position'] = $position;
        }
        $anchor['kind'] = (string) $fact['kind'];
        if (isset($fact['level'])) {
            $anchor['level'] = $fact['level'];
        }
        if (isset($fact['label'])) {
            $anchor['label'] = $fact['label'];
        }

        return $anchor;
    }

    /**
     * A projection node's site text, field => text.
     *
     * @param array<string, mixed> $fact Projection node.
     * @return array<string, string>
     */
    private static function siteText(array $fact): array
    {
        $text = [];
        foreach ((array) ($fact['from_the_site'] ?? []) as $field => $value) {
            if (is_string($value)) {
                $text[(string) $field] = $value;
            }
        }

        return $text;
    }

    /**
     * @param list<array<string, mixed>> $tree    The tree after the operation.
     * @param array<string, mixed>       $change  The change.
     * @param list<string>               $touched Refs whose text changed or that were made.
     * @param list<string>               $made    Refs made.
     * @return array<string, mixed>
     */
    private static function done(array $tree, array $change, array $touched, array $made): array
    {
        return ['tree' => $tree, 'change' => $change, 'touched' => $touched, 'made' => count($made)];
    }

    /**
     * @param string   $code    Code.
     * @param string   $detail  Detail.
     * @param int|null $opIndex The operation, or null for the whole call.
     * @return array{code: string, detail: string, op_index: int|null}
     */
    private static function refuse(string $code, string $detail, ?int $opIndex): array
    {
        return ['code' => $code, 'detail' => $detail, 'op_index' => $opIndex];
    }
}
