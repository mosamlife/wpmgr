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
 * The grammar of an outline version 3: a list of sections, the blocks and the
 * patterns they hold, and the caps on all of it. The input schema
 * (OutlineV3Schema) states the shapes; this class judges a decoded outline
 * against them and says, in the refusal envelope, what to change.
 *
 *   outline  1 to MAX_SECTIONS sections, all at the top level
 *   section  {type, anchor, width?, tone?, background?, spacing?, align?,
 *             min_height?, children}: 1 to MAX_SECTION_CHILDREN blocks, groups,
 *             columns and patterns, closed fields, closed option sets
 *   pattern  {type, pattern, variant, ...}: a hero or a cta holds blocks
 *            (a hero at most MAX_HERO_CHILDREN, a cta MAX_CTA_CHILDREN);
 *            every other pattern holds one array of typed items, counted by
 *            ITEM_CAPS, each field judged by its kind
 *
 * The blocks of a section are the blocks of version 2 in the forms version 3
 * gives them (see PageCreateBuilder::v3Counters()), judged by the same code
 * with the same text rules, so one counter covers the whole page: nodes,
 * images (section backgrounds and pattern pictures included), buttons, tables
 * and characters of text.
 *
 * The page as a whole has four more rules:
 *   anchors  a section anchor has the shape of ANCHOR_PATTERN, is not one of
 *            RESERVED_ANCHORS and is the anchor of one section only
 *   hero     a page has at most one hero (layout_invalid, counted before any
 *            hero's place is judged), and it is the first block of the first
 *            section (hero_misplaced)
 *   title    the page has at most one level 1 heading, and it is in the hero
 *   links    a link of the form #anchor leads to a section of the page; it is
 *            judged after the whole outline, so a section further down
 *            resolves it
 *
 * Codes: bad_input (an unknown field, a wrong type, a value outside a closed
 * set), layout_invalid (placement and counts), create_content_invalid (a text
 * rule), link_invalid (a link), pattern_items_out_of_range (too few or too
 * many items of a pattern), icon_unknown, anchor_invalid, anchor_reserved,
 * anchor_duplicate, hero_misplaced, h1_outside_hero and link_anchor_missing.
 * A refusal stops at the first fault, except that every unknown icon of one
 * pattern is listed and so is every link to an anchor the page does not have.
 * A refusal holds WPMgr's own words and the offending keys' names, never text
 * from the input; the one exception is the list of anchors in a refusal of a
 * link, which passed the anchor pattern and so holds only a-z, 0-9 and -.
 *
 * Pure: no WordPress call.
 */
final class OutlineV3
{
    /** The fields of a section, in the order a section is written. */
    private const SECTION_KEYS = ['type', 'anchor', 'width', 'tone', 'background', 'spacing', 'align', 'min_height', 'children'];

    /** The fields of a section background, in the order they are written. */
    private const BACKGROUND_KEYS = ['attachment_id', 'overlay', 'focal'];

    /** The section options that are closed sets. */
    private const SECTION_SETS = [
        'width'      => OutlineV3Schema::WIDTHS,
        'tone'       => OutlineV3Schema::TONES,
        'spacing'    => OutlineV3Schema::SPACINGS,
        'align'      => OutlineV3Schema::ALIGNS,
        'min_height' => OutlineV3Schema::MIN_HEIGHTS,
    ];

    /** The blocks a hero and a cta hold. */
    private const BLOCKS_OF = [
        'hero' => ['heading', 'paragraph', 'buttons', 'image'],
        'cta'  => ['heading', 'paragraph', 'buttons'],
    ];

    /**
     * Every node type of the outline. A known type in the wrong place is a
     * layout fault; a type nobody knows is a content fault of the block grammar.
     */
    private const KNOWN_TYPES = ['heading', 'paragraph', 'list', 'image', 'buttons', 'quote', 'separator', 'spacer', 'table', 'group', 'columns', 'section', 'pattern'];

    /**
     * The fields of an item of each item array, in the order they are written:
     * field => [kind, required]. A kind names the rule a value is judged by.
     */
    private const ITEM_FIELDS = [
        'features'     => [
            'icon'  => ['icon', true],
            'title' => ['text', true],
            'text'  => ['text', true],
        ],
        'testimonials' => [
            'quote'         => ['text', true],
            'name'          => ['text', true],
            'role'          => ['text', false],
            'attachment_id' => ['image', false],
        ],
        'plans'        => [
            'name'        => ['text', true],
            'price'       => ['text', true],
            'period'      => ['text', false],
            'features'    => ['lines', true],
            'cta'         => ['button', true],
            'highlighted' => ['flag', false],
        ],
        'faq'          => [
            'question' => ['text', true],
            'answer'   => ['text', true],
        ],
        'logos'        => [
            'attachment_id' => ['image', true],
            'alt'           => ['alt', true],
        ],
        'stats'        => [
            'value'  => ['count', true],
            'suffix' => ['suffix', false],
            'label'  => ['text', true],
        ],
    ];

    /** What a refusal says first, by code. */
    private const SUMMARY = [
        'bad_input'                  => 'The outline has a field or a value the grammar does not accept.',
        'layout_invalid'             => 'The outline breaks a layout rule.',
        'create_content_invalid'     => 'The outline holds text the page cannot carry.',
        'link_invalid'               => 'The outline holds a link the page cannot carry.',
        'pattern_items_out_of_range' => 'A pattern holds too few or too many items.',
        'icon_unknown'               => 'An icon is not in the list of icons.',
        'anchor_invalid'             => 'A section anchor is not one the page can carry.',
        'anchor_reserved'            => 'A section anchor is an id the theme or WordPress already uses.',
        'anchor_duplicate'           => 'Two sections have the same anchor.',
        'hero_misplaced'             => 'The hero is not at the top of the page.',
        'h1_outside_hero'            => 'A level 1 heading is outside the hero.',
        'link_anchor_missing'        => 'A link leads to an anchor that no section of the page has.',
    ];

    /**
     * Judge the outline of an outline version 3 input.
     *
     * @param mixed $outline    The decoded `outline`: a list of objects, as json_decode() gives with objects.
     * @param int   $titleChars Characters of the page title, which count toward the text cap.
     * @return array{outline?:list<array<string,mixed>>,code?:string,detail?:string,failures?:list<array<string,mixed>>,pattern?:string,item_schema?:array<string,mixed>}
     *         The normalised sections, or a refusal.
     */
    public static function validate($outline, int $titleChars = 0): array
    {
        if (!is_array($outline) || $outline === [] || !ArrayShape::isList($outline)) {
            return self::refuse('bad_input', 'input', 'outline', 'must be a list of 1 to ' . OutlineV3Schema::MAX_SECTIONS . ' sections');
        }
        if (count($outline) > OutlineV3Schema::MAX_SECTIONS) {
            return self::refuse(
                'layout_invalid',
                'input',
                'outline',
                sprintf('a page holds 1 to %d sections; this one has %d', OutlineV3Schema::MAX_SECTIONS, count($outline))
            );
        }

        $second = self::secondHero($outline);
        if ($second !== null) {
            return self::refuse('layout_invalid', $second, 'pattern', 'a page has one hero, the first block of the first section; remove this one, or make it a cta', [], 'hero');
        }

        $ctx      = PageCreateBuilder::v3Counters($titleChars);
        $page     = ['anchors' => [], 'links' => [], 'titles' => 0];
        $sections = [];
        foreach ($outline as $i => $node) {
            $r = self::section($node, 'outline[' . $i . ']', $i === 0, $ctx, $page);
            if (!isset($r['node'])) {
                return $r;
            }
            $sections[] = $r['node'];
        }
        if ($ctx['total'] > PageCreateBuilder::MAX_TOTAL_CHARS) {
            return self::refuse('create_content_invalid', 'input', 'outline', 'the text of the page is longer than ' . PageCreateBuilder::MAX_TOTAL_CHARS . ' characters');
        }
        $missing = self::missingAnchors($page);
        if ($missing !== null) {
            return $missing;
        }

        return ['outline' => $sections];
    }

    /**
     * Judge the outline of one page-edit insert or replace: the nodes that go
     * into a page that exists. A section is read where a section stands, at
     * the top; every other node as a child of a section, so a pattern, a
     * block, a group and columns are read as the grammar reads them there. The
     * rules of validate() hold for what is here, except that a link to an
     * anchor is not resolved (the page's other sections may hold it) and a
     * hero stands only as the first block of the first node when that node is
     * a section. What the page already has, and where the nodes will stand,
     * is the page's to judge: see facts().
     *
     * @param mixed $nodes      The decoded outline: a list of objects.
     * @param int   $titleChars Characters of the page title, which count toward the text cap.
     * @return array{outline?:list<array<string,mixed>>,code?:string,detail?:string,failures?:list<array<string,mixed>>,pattern?:string,item_schema?:array<string,mixed>}
     *         The nodes in the grammar's normalised form, or a refusal.
     */
    public static function fragment($nodes, int $titleChars = 0): array
    {
        if (!is_array($nodes) || $nodes === [] || !ArrayShape::isList($nodes)) {
            return self::refuse('bad_input', 'input', 'outline', 'must be a list of nodes');
        }
        $ctx  = PageCreateBuilder::v3Counters($titleChars);
        $page = ['anchors' => [], 'links' => [], 'titles' => 0];
        $out  = [];
        foreach ($nodes as $i => $node) {
            $at   = 'outline[' . $i . ']';
            $type = is_object($node) ? ($node->type ?? null) : null;
            if ($type === 'section') {
                $r = self::section($node, $at, $i === 0, $ctx, $page);
            } elseif ($type === 'pattern') {
                $r = self::pattern($node, $at, false, $ctx, $page);
            } else {
                $r = PageCreateBuilder::v3Block($node, $at, true, $ctx);
                if (!isset($r['node'])) {
                    return self::fromBlock($r, $at, null);
                }
                $bad = self::read($r['node'], $at, false, null, $page);
                if ($bad !== null) {
                    return $bad;
                }
            }
            if (!isset($r['node'])) {
                return $r;
            }
            $out[] = $r['node'];
        }
        if ($ctx['total'] > PageCreateBuilder::MAX_TOTAL_CHARS) {
            return self::refuse('create_content_invalid', 'input', 'outline', 'the text of the page is longer than ' . PageCreateBuilder::MAX_TOTAL_CHARS . ' characters');
        }

        return ['outline' => $out];
    }

    /**
     * What an outline in the grammar's normalised form says about the page it
     * goes into, for the checks that need the page: the anchors of its
     * sections, the sections' links to an anchor ("#" and the anchor), the
     * heroes it holds and the level 1 headings it holds.
     *
     * @param list<array<string,mixed>> $outline Nodes from validate() or fragment().
     * @return array{anchors:list<string>,links:list<string>,heroes:int,h1s:int,sections:int}
     */
    public static function facts(array $outline): array
    {
        $facts = ['anchors' => [], 'links' => [], 'heroes' => 0, 'h1s' => 0, 'sections' => 0];
        self::collect($outline, $facts);

        return $facts;
    }

    /**
     * @param array<mixed>        $nodes Normalised nodes.
     * @param array<string,mixed> $facts The facts so far.
     */
    private static function collect(array $nodes, array &$facts): void
    {
        foreach ($nodes as $node) {
            if (!is_array($node)) {
                continue;
            }
            switch ($node['type'] ?? null) {
                case 'section':
                    ++$facts['sections'];
                    if (is_string($node['anchor'] ?? null)) {
                        $facts['anchors'][] = $node['anchor'];
                    }
                    break;
                case 'pattern':
                    if (($node['pattern'] ?? null) === 'hero') {
                        ++$facts['heroes'];
                    }
                    break;
                case 'heading':
                    if (($node['level'] ?? null) === 1) {
                        ++$facts['h1s'];
                    }
                    break;
                case 'buttons':
                    foreach (is_array($node['buttons'] ?? null) ? $node['buttons'] : [] as $button) {
                        self::linkOf($button, $facts);
                    }
                    break;
            }
            foreach (['children', 'cta'] as $key) {
                if (is_array($node[$key] ?? null)) {
                    $below = isset($node[$key]['url']) ? [['type' => 'buttons', 'buttons' => [$node[$key]]]] : $node[$key];
                    self::collect($below, $facts);
                }
            }
            foreach (['plans', 'features', 'testimonials', 'faq', 'logos', 'stats'] as $items) {
                if (is_array($node[$items] ?? null)) {
                    foreach ($node[$items] as $item) {
                        if (is_array($item) && is_array($item['cta'] ?? null)) {
                            self::linkOf($item['cta'], $facts);
                        }
                    }
                }
            }
            foreach (is_array($node['columns'] ?? null) ? $node['columns'] : [] as $column) {
                if (is_array($column)) {
                    self::collect($column, $facts);
                }
            }
        }
    }

    /**
     * @param mixed               $button A normalised button or call to action.
     * @param array<string,mixed> $facts  The facts so far.
     */
    private static function linkOf($button, array &$facts): void
    {
        $url = is_array($button) ? ($button['url'] ?? null) : null;
        if (is_string($url) && strncmp($url, '#', 1) === 0) {
            $facts['links'][] = substr($url, 1);
        }
    }

    /**
     * The path of the first hero over the cap, or null when the page has no
     * more than OutlineV3Schema::MAX_HEROES. A hero is a pattern node named
     * hero that is a direct child of a section; the count reads shape alone
     * and judges nothing else, so it comes before any hero's place is judged:
     * a page with two heroes is layout_invalid whether or not the first one
     * stands where the hero goes.
     *
     * @param array<int|string,mixed> $outline The outline: a list.
     * @return string|null Path of the hero that is one too many.
     */
    private static function secondHero(array $outline): ?string
    {
        $heroes = 0;
        foreach ($outline as $i => $section) {
            $children = is_object($section) ? ($section->children ?? null) : null;
            if (!is_array($children)) {
                continue;
            }
            foreach ($children as $j => $child) {
                if (!is_object($child) || ($child->type ?? null) !== 'pattern' || ($child->pattern ?? null) !== 'hero') {
                    continue;
                }
                if (++$heroes > OutlineV3Schema::MAX_HEROES) {
                    return 'outline[' . $i . '].children[' . $j . ']';
                }
            }
        }

        return null;
    }

    // ---------------------------------------------------------------------
    // Sections
    // ---------------------------------------------------------------------

    /**
     * @param mixed               $node  Decoded top-level item.
     * @param string              $at    Its path.
     * @param bool                $first Whether it is the first section of the page.
     * @param array<string,mixed> $ctx   Page-wide counters.
     * @param array<string,mixed> $page  What the page has shown so far: its anchors, its links, its title.
     * @return array<string,mixed> {node} or a refusal.
     */
    private static function section($node, string $at, bool $first, array &$ctx, array &$page): array
    {
        if (!is_object($node)) {
            return self::refuse('bad_input', $at, 'type', 'a top-level item is a section object');
        }
        if (++$ctx['nodes'] > PageCreateBuilder::MAX_NODES) {
            return self::tooMany($at);
        }
        $n = get_object_vars($node);
        if (($n['type'] ?? null) !== 'section') {
            return self::refuse('layout_invalid', $at, 'type', 'every top-level item is a section; put other blocks inside a section', ['section']);
        }
        $bad = self::fields($n, self::SECTION_KEYS, ['type', 'anchor', 'children'], $at, 'a section', null);
        if ($bad !== null) {
            return $bad;
        }
        if (!is_string($n['anchor'])) {
            return self::refuse('bad_input', $at, 'anchor', 'must be a string');
        }
        $bad = self::anchor($n['anchor'], $at, $page);
        if ($bad !== null) {
            return $bad;
        }
        foreach (self::SECTION_SETS as $key => $set) {
            if (array_key_exists($key, $n) && (!is_string($n[$key]) || !in_array($n[$key], $set, true))) {
                return self::refuse('bad_input', $at, $key, 'must be one of the allowed values', $set);
            }
        }
        $background = null;
        if (array_key_exists('background', $n)) {
            $r = self::background($n['background'], $at, $ctx);
            if (!isset($r['node'])) {
                return $r;
            }
            $background = $r['node'];
        }

        $children = $n['children'];
        if (!is_array($children) || !ArrayShape::isList($children)) {
            return self::refuse('bad_input', $at, 'children', 'must be a list of blocks');
        }
        if ($children === [] || count($children) > OutlineV3Schema::MAX_SECTION_CHILDREN) {
            return self::refuse(
                'layout_invalid',
                $at,
                'children',
                sprintf('a section holds 1 to %d blocks; this one has %d', OutlineV3Schema::MAX_SECTION_CHILDREN, count($children))
            );
        }
        $kids = [];
        foreach ($children as $j => $child) {
            $childAt = $at . '.children[' . $j . ']';
            $type    = is_object($child) ? ($child->type ?? null) : null;
            if ($type === 'pattern') {
                $r = self::pattern($child, $childAt, $first && $j === 0, $ctx, $page);
            } elseif ($type === 'section') {
                return self::refuse('layout_invalid', $childAt, 'type', 'a section is a top-level item; it cannot sit inside a section');
            } else {
                $r = PageCreateBuilder::v3Block($child, $childAt, true, $ctx);
                if (!isset($r['node'])) {
                    return self::fromBlock($r, $childAt, null);
                }
                $bad = self::read($r['node'], $childAt, false, null, $page);
                if ($bad !== null) {
                    return $bad;
                }
            }
            if (!isset($r['node'])) {
                return $r;
            }
            $kids[] = $r['node'];
        }

        $section = ['type' => 'section', 'anchor' => $n['anchor']];
        foreach (['width', 'tone'] as $key) {
            if (array_key_exists($key, $n)) {
                $section[$key] = $n[$key];
            }
        }
        if ($background !== null) {
            $section['background'] = $background;
        }
        foreach (['spacing', 'align', 'min_height'] as $key) {
            if (array_key_exists($key, $n)) {
                $section[$key] = $n[$key];
            }
        }
        $section['children'] = $kids;

        return ['node' => $section];
    }

    /**
     * A section background: an image and, if wanted, the strength of its
     * overlay and the focal point. It counts as one image of the page.
     *
     * @param mixed               $background Decoded background.
     * @param string              $at         Path of the section.
     * @param array<string,mixed> $ctx        Page-wide counters.
     * @return array<string,mixed> {node} or a refusal.
     */
    private static function background($background, string $at, array &$ctx): array
    {
        if (!is_object($background)) {
            return self::refuse('bad_input', $at, 'background', 'must be an object with an attachment_id, and an overlay and a focal point if wanted');
        }
        $where = $at . '.background';
        $b     = get_object_vars($background);
        $bad   = self::fields($b, self::BACKGROUND_KEYS, ['attachment_id'], $where, 'a background', null);
        if ($bad !== null) {
            return $bad;
        }
        $id = $b['attachment_id'];
        if (!is_int($id) || $id < 1 || $id > PageCreateBuilder::MAX_ATTACHMENT_ID) {
            return self::refuse('bad_input', $where, 'attachment_id', 'must be a whole number from 1 to ' . PageCreateBuilder::MAX_ATTACHMENT_ID);
        }
        foreach (['overlay' => OutlineV3Schema::OVERLAYS, 'focal' => OutlineV3Schema::FOCALS] as $key => $set) {
            if (array_key_exists($key, $b) && (!is_string($b[$key]) || !in_array($b[$key], $set, true))) {
                return self::refuse('bad_input', $where, $key, 'must be one of the allowed values', $set);
            }
        }
        if (++$ctx['images'] > PageCreateBuilder::MAX_IMAGES) {
            return self::tooManyImages($where, 'attachment_id');
        }
        $out = ['attachment_id' => $id];
        foreach (['overlay', 'focal'] as $key) {
            if (array_key_exists($key, $b)) {
                $out[$key] = $b[$key];
            }
        }

        return ['node' => $out];
    }

    // ---------------------------------------------------------------------
    // Patterns
    // ---------------------------------------------------------------------

    /**
     * One pattern node. It is an object whose type is "pattern".
     *
     * @param object              $node  Decoded node.
     * @param string              $at    Its path.
     * @param bool                $opens Whether it stands where the hero goes: the first block of the first section.
     * @param array<string,mixed> $ctx   Page-wide counters.
     * @param array<string,mixed> $page  What the page has shown so far.
     * @return array<string,mixed> {node} or a refusal.
     */
    private static function pattern(object $node, string $at, bool $opens, array &$ctx, array &$page): array
    {
        if (++$ctx['nodes'] > PageCreateBuilder::MAX_NODES) {
            return self::tooMany($at);
        }
        $n    = get_object_vars($node);
        $name = $n['pattern'] ?? null;
        if (!is_string($name) || !in_array($name, OutlineV3Schema::PATTERNS, true)) {
            return self::refuse('bad_input', $at, 'pattern', 'must be one of the patterns', OutlineV3Schema::PATTERNS);
        }
        if ($name === 'hero' && !$opens) {
            return self::refuse('hero_misplaced', $at, 'pattern', 'the hero is the first block of the first section; move it there, or leave the hero out', [], $name);
        }
        $array = OutlineV3Schema::PATTERN_ARRAY[$name] ?? 'children';
        $bad   = self::fields($n, ['type', 'pattern', 'variant', $array], ['type', 'pattern', 'variant', $array], $at, 'a ' . $name . ' pattern', $name);
        if ($bad !== null) {
            return $bad;
        }
        $variants = OutlineV3Schema::VARIANTS[$name];
        if (!is_string($n['variant']) || !in_array($n['variant'], $variants, true)) {
            return self::refuse('bad_input', $at, 'variant', 'must be one of the variants of the ' . $name . ' pattern', $variants, $name);
        }

        if (!isset(OutlineV3Schema::PATTERN_ARRAY[$name])) {
            return self::composed($name, $n['variant'], $n['children'], $at, $ctx, $page);
        }

        return self::itemsOf($name, $array, $n['variant'], $n[$array], $at, $ctx, $page);
    }

    /**
     * A hero or a cta: blocks in a fixed number, of fixed kinds.
     *
     * @param string              $name    hero or cta.
     * @param string              $variant Its variant.
     * @param mixed               $kids    Decoded children.
     * @param string              $at      Path of the pattern.
     * @param array<string,mixed> $ctx     Page-wide counters.
     * @param array<string,mixed> $page    What the page has shown so far.
     * @return array<string,mixed> {node} or a refusal.
     */
    private static function composed(string $name, string $variant, $kids, string $at, array &$ctx, array &$page): array
    {
        if (!is_array($kids) || !ArrayShape::isList($kids)) {
            return self::refuse('bad_input', $at, 'children', 'must be a list of blocks', [], $name);
        }
        $max = $name === 'hero' ? OutlineV3Schema::MAX_HERO_CHILDREN : OutlineV3Schema::MAX_CTA_CHILDREN;
        if ($kids === [] || count($kids) > $max) {
            return self::refuse('layout_invalid', $at, 'children', sprintf('a %s holds 1 to %d blocks; this one has %d', $name, $max, count($kids)), [], $name);
        }
        $allowed = self::BLOCKS_OF[$name];
        $out     = [];
        foreach ($kids as $k => $kid) {
            $kidAt = $at . '.children[' . $k . ']';
            $type  = is_object($kid) ? ($kid->type ?? null) : null;
            if (is_string($type) && in_array($type, self::KNOWN_TYPES, true) && !in_array($type, $allowed, true)) {
                return self::refuse('layout_invalid', $kidAt, 'type', 'a ' . $name . ' holds only ' . implode(', ', $allowed), $allowed, $name);
            }
            $r = PageCreateBuilder::v3Block($kid, $kidAt, false, $ctx);
            if (!isset($r['node'])) {
                return self::fromBlock($r, $kidAt, $name);
            }
            $bad = self::read($r['node'], $kidAt, $name === 'hero', $name, $page);
            if ($bad !== null) {
                return $bad;
            }
            $out[] = $r['node'];
        }

        return ['node' => ['type' => 'pattern', 'pattern' => $name, 'variant' => $variant, 'children' => $out]];
    }

    /**
     * An item pattern: its array of items, counted and judged item by item.
     *
     * @param string              $name    Pattern name.
     * @param string              $array   The array the items are in.
     * @param string              $variant Its variant.
     * @param mixed               $items   Decoded items.
     * @param string              $at      Path of the pattern.
     * @param array<string,mixed> $ctx     Page-wide counters.
     * @param array<string,mixed> $page    What the page has shown so far.
     * @return array<string,mixed> {node} or a refusal.
     */
    private static function itemsOf(string $name, string $array, string $variant, $items, string $at, array &$ctx, array &$page): array
    {
        if (!is_array($items) || !ArrayShape::isList($items)) {
            return self::refuse('bad_input', $at, $array, 'must be a list of items', [], $name);
        }
        [$min, $max] = OutlineV3Schema::ITEM_CAPS[$array];
        if (count($items) < $min || count($items) > $max) {
            return self::refuse(
                'pattern_items_out_of_range',
                $at,
                $array,
                sprintf('%s take %d to %d items; this one has %d', $array, $min, $max, count($items)),
                [],
                $name
            );
        }
        $out   = [];
        $icons = [];
        foreach ($items as $k => $item) {
            $r = self::item($item, $at, $array, $k, $name, $ctx, $icons);
            if (!isset($r['node'])) {
                return $r;
            }
            $out[] = $r['node'];
        }
        if ($icons !== []) {
            return self::envelope('icon_unknown', $icons, $name);
        }
        if ($array === 'plans') {
            foreach ($out as $k => $plan) {
                self::note($plan['cta']['url'], $at . '.plans[' . $k . '].cta', $page);
            }
        }

        return ['node' => ['type' => 'pattern', 'pattern' => $name, 'variant' => $variant, $array => $out]];
    }

    /**
     * One item: closed fields, then each field by its kind.
     *
     * @param mixed                      $item    Decoded item.
     * @param string                     $base    Path of the pattern.
     * @param string                     $array   The array the item is in.
     * @param int                        $k       Its position.
     * @param string                     $pattern Pattern name.
     * @param array<string,mixed>        $ctx     Page-wide counters.
     * @param list<array<string,mixed>>  $icons   Unknown icons found so far.
     * @return array<string,mixed> {node} or a refusal.
     */
    private static function item($item, string $base, string $array, int $k, string $pattern, array &$ctx, array &$icons): array
    {
        if (!is_object($item)) {
            return self::refuse('bad_input', $base, $array . '[' . $k . ']', 'must be an object', [], $pattern);
        }
        $at   = $base . '.' . $array . '[' . $k . ']';
        $spec = self::ITEM_FIELDS[$array];
        $n    = get_object_vars($item);
        $req  = [];
        foreach ($spec as $field => $rule) {
            if ($rule[1]) {
                $req[] = $field;
            }
        }
        $bad = self::fields($n, array_keys($spec), $req, $at, 'this item', $pattern);
        if ($bad !== null) {
            return $bad;
        }

        $out = [];
        foreach ($spec as $field => $rule) {
            if (!array_key_exists($field, $n)) {
                continue;
            }
            $r = self::value($rule[0], $n[$field], $at, $field, $pattern, $ctx, $icons);
            if (!array_key_exists('value', $r)) {
                return $r;
            }
            $out[$field] = $r['value'];
        }

        return ['node' => $out];
    }

    /**
     * One field of an item, judged by its kind.
     *
     * @param string                     $kind    The rule: icon, text, alt, suffix, image, count, flag, lines or button.
     * @param mixed                      $v       Decoded value.
     * @param string                     $at      Path of the item.
     * @param string                     $field   The field.
     * @param string                     $pattern Pattern name.
     * @param array<string,mixed>        $ctx     Page-wide counters.
     * @param list<array<string,mixed>>  $icons   Unknown icons found so far.
     * @return array<string,mixed> {value} or a refusal.
     */
    private static function value(string $kind, $v, string $at, string $field, string $pattern, array &$ctx, array &$icons): array
    {
        switch ($kind) {
            case 'text':
                return self::words($v, $at, $field, PageCreateBuilder::MAX_TEXT_CHARS, 'text', $pattern, $ctx);
            case 'alt':
                return self::words($v, $at, $field, PageCreateBuilder::MAX_ALT_CHARS, 'alt', $pattern, $ctx);
            case 'suffix':
                return self::words($v, $at, $field, OutlineV3Schema::MAX_SUFFIX_CHARS, 'text', $pattern, $ctx);
            case 'icon':
                if (!is_string($v)) {
                    return self::refuse('bad_input', $at, $field, 'must be the name of an icon', OutlineV3Schema::ICONS, $pattern);
                }
                if (!in_array($v, OutlineV3Schema::ICONS, true)) {
                    $icons[] = ['path' => $at, 'key' => $field, 'reason' => 'is not the name of an icon in the list', 'allowed' => OutlineV3Schema::ICONS];
                }

                return ['value' => $v];
            case 'image':
                if (!is_int($v) || $v < 1 || $v > PageCreateBuilder::MAX_ATTACHMENT_ID) {
                    return self::refuse('bad_input', $at, $field, 'must be a whole number from 1 to ' . PageCreateBuilder::MAX_ATTACHMENT_ID, [], $pattern);
                }
                if (++$ctx['images'] > PageCreateBuilder::MAX_IMAGES) {
                    return self::tooManyImages($at, $field, $pattern);
                }

                return ['value' => $v];
            case 'count':
                if (!is_int($v) || $v < 0 || $v > OutlineV3Schema::MAX_STAT_VALUE) {
                    return self::refuse('bad_input', $at, $field, 'must be a whole number from 0 to ' . OutlineV3Schema::MAX_STAT_VALUE, [], $pattern);
                }

                return ['value' => $v];
            case 'flag':
                if (!is_bool($v)) {
                    return self::refuse('bad_input', $at, $field, 'must be true or false', [], $pattern);
                }

                return ['value' => $v];
            case 'lines':
                return self::lines($v, $at, $field, $pattern, $ctx);
            default:
                return self::button($v, $at, $field, $pattern, $ctx);
        }
    }

    /**
     * A text under the rules of the blocks.
     *
     * @param mixed               $v       Decoded value.
     * @param string              $at      Path of the item.
     * @param string              $field   The field.
     * @param int                 $max     Most characters.
     * @param string              $rule    text or alt.
     * @param string              $pattern Pattern name.
     * @param array<string,mixed> $ctx     Page-wide counters.
     * @return array<string,mixed> {value} or a refusal.
     */
    private static function words($v, string $at, string $field, int $max, string $rule, string $pattern, array &$ctx): array
    {
        $where = $at . '.' . $field;
        $text  = PageCreateBuilder::v3Text($v, $where, $max, $rule, $ctx);
        if (is_array($text)) {
            return self::refuse((string) $text['code'], $at, $field, self::after((string) $text['detail'], $where), [], $pattern);
        }

        return ['value' => $text];
    }

    /**
     * The feature lines of a pricing plan.
     *
     * @param mixed               $v       Decoded value.
     * @param string              $at      Path of the plan.
     * @param string              $field   The field.
     * @param string              $pattern Pattern name.
     * @param array<string,mixed> $ctx     Page-wide counters.
     * @return array<string,mixed> {value} or a refusal.
     */
    private static function lines($v, string $at, string $field, string $pattern, array &$ctx): array
    {
        if (!is_array($v) || !ArrayShape::isList($v)) {
            return self::refuse('bad_input', $at, $field, 'must be a list of short lines', [], $pattern);
        }
        if ($v === [] || count($v) > OutlineV3Schema::MAX_PLAN_FEATURES) {
            return self::refuse(
                'pattern_items_out_of_range',
                $at,
                $field,
                sprintf('a plan lists 1 to %d features; this one has %d', OutlineV3Schema::MAX_PLAN_FEATURES, count($v)),
                [],
                $pattern
            );
        }
        $out = [];
        foreach ($v as $i => $line) {
            $where = $at . '.' . $field . '[' . $i . ']';
            $text  = PageCreateBuilder::v3Text($line, $where, PageCreateBuilder::MAX_TEXT_CHARS, 'text', $ctx);
            if (is_array($text)) {
                return self::refuse((string) $text['code'], $at, $field . '[' . $i . ']', self::after((string) $text['detail'], $where), [], $pattern);
            }
            $out[] = $text;
        }

        return ['value' => $out];
    }

    /**
     * The button of a pricing plan: its text and where it goes.
     *
     * @param mixed               $v       Decoded value.
     * @param string              $at      Path of the plan.
     * @param string              $field   The field.
     * @param string              $pattern Pattern name.
     * @param array<string,mixed> $ctx     Page-wide counters.
     * @return array<string,mixed> {value} or a refusal.
     */
    private static function button($v, string $at, string $field, string $pattern, array &$ctx): array
    {
        if (!is_object($v)) {
            return self::refuse('bad_input', $at, $field, 'must be an object with text and url', [], $pattern);
        }
        $where = $at . '.' . $field;
        $b     = get_object_vars($v);
        $bad   = self::fields($b, ['text', 'url'], ['text', 'url'], $where, 'a plan button', $pattern);
        if ($bad !== null) {
            return $bad;
        }
        $text = self::words($b['text'], $where, 'text', PageCreateBuilder::MAX_BUTTON_TEXT_CHARS, 'text', $pattern, $ctx);
        if (!array_key_exists('value', $text)) {
            return $text;
        }
        if (!is_string($b['url'])) {
            return self::refuse('bad_input', $where, 'url', 'must be a string', [], $pattern);
        }
        $why = PageCreateBuilder::designedLinkProblem($b['url']);
        if ($why !== null) {
            return self::refuse('link_invalid', $where, 'url', $why, [], $pattern);
        }
        $ctx['total'] += strlen($b['url']);

        return ['value' => ['text' => $text['value'], 'url' => $b['url']]];
    }

    // ---------------------------------------------------------------------
    // The page as a whole: anchors, the title, links
    // ---------------------------------------------------------------------

    /**
     * The anchor of a section: it has the shape of an anchor, is not an id the
     * theme owns and is not the anchor of an earlier section. It is kept for the
     * links that lead to it.
     *
     * @param string              $anchor The anchor as given.
     * @param string              $at     Path of the section.
     * @param array<string,mixed> $page   What the page has shown so far.
     * @return array<string,mixed>|null A refusal, or null.
     */
    private static function anchor(string $anchor, string $at, array &$page): ?array
    {
        if (preg_match('/' . OutlineV3Schema::ANCHOR_PATTERN . '/D', $anchor) !== 1) {
            return self::refuse('anchor_invalid', $at, 'anchor', 'must be 2 to 40 characters: a lower-case letter, then lower-case letters, digits or hyphens');
        }
        if (in_array($anchor, OutlineV3Schema::RESERVED_ANCHORS, true)) {
            return self::refuse('anchor_reserved', $at, 'anchor', 'is an id the theme or WordPress already uses (' . implode(', ', OutlineV3Schema::RESERVED_ANCHORS) . '); choose another');
        }
        if (isset($page['anchors'][$anchor])) {
            return self::refuse('anchor_duplicate', $at, 'anchor', 'is the anchor of ' . $page['anchors'][$anchor] . ' already; give each section its own');
        }
        $page['anchors'][$anchor] = $at;

        return null;
    }

    /**
     * What a judged block says about the page as a whole: where its level 1
     * headings stand and where its links lead. A group and columns are read
     * down to their blocks.
     *
     * @param array<string,mixed> $node    The block, as the block grammar returned it.
     * @param string              $at      Its path.
     * @param bool                $inHero  Whether it stands in the hero.
     * @param string|null         $pattern Pattern name, when the block is inside a pattern.
     * @param array<string,mixed> $page    What the page has shown so far.
     * @return array<string,mixed>|null A refusal, or null.
     */
    private static function read(array $node, string $at, bool $inHero, ?string $pattern, array &$page): ?array
    {
        switch ($node['type'] ?? null) {
            case 'heading':
                return $node['level'] === 1 ? self::title($at, $inHero, $pattern, $page) : null;
            case 'buttons':
                foreach ($node['buttons'] as $k => $button) {
                    self::note($button['url'], $at . '.buttons[' . $k . ']', $page);
                }

                return null;
            case 'group':
                return self::readAll($node['children'], $at . '.children', $inHero, $pattern, $page);
            case 'columns':
                foreach ($node['columns'] as $c => $blocks) {
                    $bad = self::readAll($blocks, $at . '.columns[' . $c . '].children', $inHero, $pattern, $page);
                    if ($bad !== null) {
                        return $bad;
                    }
                }

                return null;
            default:
                return null;
        }
    }

    /**
     * @param list<array<string,mixed>> $nodes   The blocks, as the block grammar returned them.
     * @param string                    $at      Path of the list.
     * @param bool                      $inHero  Whether they stand in the hero.
     * @param string|null               $pattern Pattern name, when the blocks are inside a pattern.
     * @param array<string,mixed>       $page    What the page has shown so far.
     * @return array<string,mixed>|null A refusal, or null.
     */
    private static function readAll(array $nodes, string $at, bool $inHero, ?string $pattern, array &$page): ?array
    {
        foreach ($nodes as $k => $node) {
            $bad = self::read($node, $at . '[' . $k . ']', $inHero, $pattern, $page);
            if ($bad !== null) {
                return $bad;
            }
        }

        return null;
    }

    /**
     * A level 1 heading: the one title of the page, which stands in the hero.
     *
     * @param string              $at      Path of the heading.
     * @param bool                $inHero  Whether it stands in the hero.
     * @param string|null         $pattern Pattern name, when the heading is inside a pattern.
     * @param array<string,mixed> $page    What the page has shown so far.
     * @return array<string,mixed>|null A refusal, or null.
     */
    private static function title(string $at, bool $inHero, ?string $pattern, array &$page): ?array
    {
        if (!$inHero) {
            return self::refuse('h1_outside_hero', $at, 'level', 'a level 1 heading is the title of the page and stands in the hero; use level 2 here', [], $pattern);
        }
        if (++$page['titles'] > 1) {
            return self::refuse('layout_invalid', $at, 'level', 'a page has one level 1 heading, in the hero; use level 2 here', [], $pattern);
        }

        return null;
    }

    /**
     * Keep a link to a section of the page until every section is read. Any
     * other link is not the page's to judge here.
     *
     * @param string              $url  The link, which already passed the link rules.
     * @param string              $at   Path of the object that holds it.
     * @param array<string,mixed> $page What the page has shown so far.
     */
    private static function note(string $url, string $at, array &$page): void
    {
        if (strncmp($url, '#', 1) === 0) {
            $page['links'][] = ['slug' => substr($url, 1), 'path' => $at];
        }
    }

    /**
     * Every link of the page that leads to an anchor no section has, in the
     * order of the page. Each one names the anchors the page does have.
     *
     * @param array<string,mixed> $page What the page has shown.
     * @return array<string,mixed>|null A refusal, or null.
     */
    private static function missingAnchors(array $page): ?array
    {
        $have     = array_map('strval', array_keys($page['anchors']));
        $failures = [];
        foreach ($page['links'] as $link) {
            if (!isset($page['anchors'][$link['slug']])) {
                $failures[] = [
                    'path'    => $link['path'],
                    'key'     => 'url',
                    'reason'  => 'leads to a section anchor the page does not have; use # and one of the anchors listed, or add that section',
                    'allowed' => $have,
                ];
            }
        }

        return $failures === [] ? null : self::envelope('link_anchor_missing', $failures, null);
    }

    // ---------------------------------------------------------------------
    // Refusals
    // ---------------------------------------------------------------------

    /**
     * Exactly the allowed fields, and every required one present.
     *
     * @param array<int|string,mixed> $n        Decoded fields.
     * @param list<string>            $allowed  Allowed fields.
     * @param list<string>            $required Required fields.
     * @param string                  $at       Path of the object.
     * @param string                  $what     What the object is, for the sentence ("a section").
     * @param string|null             $pattern  Pattern name, when the object is part of a pattern.
     * @return array<string,mixed>|null A refusal, or null.
     */
    private static function fields(array $n, array $allowed, array $required, string $at, string $what, ?string $pattern): ?array
    {
        if (array_diff(array_map('strval', array_keys($n)), $allowed) !== []) {
            return self::refuse('bad_input', $at, 'fields', 'has a field ' . $what . ' does not take', $allowed, $pattern);
        }
        foreach ($required as $key) {
            if (!array_key_exists($key, $n)) {
                return self::refuse('bad_input', $at, $key, 'is required', [], $pattern);
            }
        }

        return null;
    }

    /**
     * A refusal of one failure.
     *
     * @param string       $code    Refusal code.
     * @param string       $path    Where the node is.
     * @param string       $key     The offending key.
     * @param string       $reason  What to change, in WPMgr's words.
     * @param list<string> $allowed Accepted values, when the set is closed.
     * @param string|null  $pattern Pattern name, when the fault is inside a pattern.
     * @return array<string,mixed>
     */
    private static function refuse(string $code, string $path, string $key, string $reason, array $allowed = [], ?string $pattern = null): array
    {
        $failure = ['path' => $path, 'key' => $key, 'reason' => $reason];
        if ($allowed !== []) {
            $failure['allowed'] = $allowed;
        }

        return self::envelope($code, [$failure], $pattern);
    }

    /**
     * @param string                    $code     Refusal code.
     * @param list<array<string,mixed>> $failures Failures.
     * @param string|null               $pattern  Pattern name, when the fault is inside a pattern.
     * @return array<string,mixed>
     */
    private static function envelope(string $code, array $failures, ?string $pattern): array
    {
        $summary = self::SUMMARY[$code] ?? 'The outline was refused.';

        return $pattern === null
            ? Refusal::make($code, $summary, $failures)
            : Refusal::forPattern($code, $summary, $pattern, $failures);
    }

    /**
     * A refusal of the block grammar, in the envelope. The block grammar
     * writes its detail as the path of the fault and what is wrong with it;
     * the path continues the path of the block it was handed.
     *
     * @param array<string,mixed> $r       A {code, detail} refusal of PageCreateBuilder.
     * @param string              $at      Path of the block.
     * @param string|null         $pattern Pattern name, when the block is inside a pattern.
     * @return array<string,mixed>
     */
    private static function fromBlock(array $r, string $at, ?string $pattern): array
    {
        $detail = (string) ($r['detail'] ?? '');
        $key    = 'node';
        $reason = $detail;
        if (strncmp($detail, $at, strlen($at)) === 0) {
            $rest = substr($detail, strlen($at));
            if (preg_match('/^\.([A-Za-z0-9_.\[\]]{1,40})(?::\s|\s)(.+)$/Ds', $rest, $m) === 1) {
                $key    = $m[1];
                $reason = $m[2];
            } else {
                $reason = ltrim($rest, ': ');
            }
        }

        return self::refuse((string) ($r['code'] ?? 'bad_input'), $at, $key, $reason === '' ? 'is not accepted' : $reason, [], $pattern);
    }

    /**
     * What a {code, detail} refusal says after the path it starts with.
     *
     * @param string $detail Detail.
     * @param string $where  The path the detail starts with.
     */
    private static function after(string $detail, string $where): string
    {
        $reason = strncmp($detail, $where, strlen($where)) === 0 ? ltrim(substr($detail, strlen($where)), ': ') : $detail;

        return $reason === '' ? 'is not accepted' : $reason;
    }

    /**
     * @param string $at Path of the node that is one too many.
     * @return array<string,mixed>
     */
    private static function tooMany(string $at): array
    {
        return self::refuse('layout_invalid', $at, 'type', 'the page has more than ' . PageCreateBuilder::MAX_NODES . ' blocks');
    }

    /**
     * @param string      $at      Path of the node.
     * @param string      $key     Its key.
     * @param string|null $pattern Pattern name, when the picture is in a pattern.
     * @return array<string,mixed>
     */
    private static function tooManyImages(string $at, string $key, ?string $pattern = null): array
    {
        return self::refuse(
            'layout_invalid',
            $at,
            $key,
            'a page shows at most ' . PageCreateBuilder::MAX_IMAGES . ' images, section backgrounds and pattern pictures included',
            [],
            $pattern
        );
    }
}
