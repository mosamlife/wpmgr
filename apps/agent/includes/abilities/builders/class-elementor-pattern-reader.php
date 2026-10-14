<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * Reads a WPMgr pattern back from the container Elementor stores for it.
 *
 * A container whose Navigator label ElementorPatternCompiler::fromLabel()
 * names is a pattern only while its subtree is the one the compiler builds for
 * that pattern, variant and item count. The shape is the nesting of the
 * containers and the widget types in them: a row or a column of containers,
 * the widgets at the ends. Sizes, colours and text are not part of it, so a
 * person's styling and rewording keep the pattern; moving, wrapping, adding or
 * removing an element does not.
 *
 * fields() answers, for a pattern that matches, every piece of text the
 * pattern offers as a path (OutlineV3Schema::EDIT_PATH_PATTERN) in document
 * order: the k-th testimonial widget is item k, the k-th accordion tab is item
 * k, and the cards, columns and icon boxes of a features, pricing or stats
 * pattern are items in the order they stand. Each path says where its text is
 * stored (the element id and the settings path, as ElementorClassicMapper's
 * set_text targets are) and the form it is stored in, so an edit writes what
 * the projection read.
 *
 *   text       the value is the text, with character references encoded
 *   paragraph  the value is one paragraph, <p>text</p>
 *   number     the value is a whole number, as digits
 *   url        the value is a link address
 *
 * A path is offered only while the stored value is a string of its form. A
 * pattern with fewer or more items than the grammar's caps is not a pattern.
 * The hero and the call to action hold ordinary blocks, which the mapper
 * projects as it does any other, so they offer no path.
 *
 * Pure: no WordPress call.
 */
final class ElementorPatternReader
{
    /** Deepest element the shape check follows. */
    private const MAX_DEPTH = 8;

    /** The shapes of one pricing card, with and without its period line. */
    private const CARD_SHAPES = [
        'C[heading,heading,icon-list,R[button]]',
        'C[heading,heading,text-editor,icon-list,R[button]]',
    ];

    /** A number the stats pattern shows, as stored. */
    private const RE_NUMBER = '/^[0-9]{1,9}$/D';

    /**
     * The text a pattern offers, by path.
     *
     * @param array<mixed> $container The stored container.
     * @param string       $pattern   One of OutlineV3Schema::PATTERNS.
     * @param string       $variant   Its variant.
     * @return array<string,array{id:string,at:list<int|string>,form:string,text:string}>|null Null when the subtree is not the pattern's shape.
     */
    public static function fields(array $container, string $pattern, string $variant): ?array
    {
        if (!in_array($variant, OutlineV3Schema::VARIANTS[$pattern] ?? [], true)) {
            return null;
        }

        return match ($pattern) {
            'hero', 'cta'  => self::composed($container, $variant),
            'features'     => self::features($container, $variant),
            'testimonials' => self::testimonials($container),
            'pricing'      => self::pricing($container),
            'faq'          => self::faq($container),
            'stats'        => self::stats($container),
            'logos'        => self::logos($container),
            default        => null,
        };
    }

    /**
     * A hero or a call to action: a full-width column of blocks, or for a split
     * hero a row of two columns.
     *
     * @param array<mixed> $container The stored container.
     * @param string       $variant   The variant.
     * @return array<string,array{id:string,at:list<int|string>,form:string,text:string}>|null
     */
    private static function composed(array $container, string $variant): ?array
    {
        $shape = self::shape($container);
        if (strpos($shape, '?') !== false || $shape === 'C[]' || $shape === 'R[]') {
            return null;
        }
        if ($shape[0] === 'C') {
            return [];
        }
        // A row is a split hero: its copy column and its picture column, each holding blocks.
        $columns = array_values(is_array($container['elements'] ?? null) ? $container['elements'] : []);
        if ($variant !== 'split' || count($columns) !== 2) {
            return null;
        }
        foreach ($columns as $column) {
            $inner = self::shape($column);
            if ($inner[0] !== 'C' || $inner === 'C[]') {
                return null;
            }
        }

        return [];
    }

    /**
     * Features: icon boxes in two columns (split) or in cards in rows (bento, grid).
     *
     * @param array<mixed> $container The stored container.
     * @param string       $variant   split, bento or grid.
     * @return array<string,array{id:string,at:list<int|string>,form:string,text:string}>|null
     */
    private static function features(array $container, string $variant): ?array
    {
        $boxes = self::widgets($container, 'icon-box');
        $n     = count($boxes);
        if (!self::within('features', $n)) {
            return null;
        }
        $box = 'icon-box';
        if ($variant === 'split') {
            $left     = intdiv($n + 1, 2);
            $expected = 'R[C[' . implode(',', array_fill(0, $left, $box)) . '],C[' . implode(',', array_fill(0, $n - $left, $box)) . ']]';
        } else {
            $rows = [];
            foreach (ElementorPatternCards::rowShares($variant, $n) as $shares) {
                $rows[] = $shares === [null] ? 'C[' . $box . ']' : 'R[' . implode(',', array_fill(0, count($shares), 'C[' . $box . ']')) . ']';
            }
            $expected = 'C[' . implode(',', $rows) . ']';
        }
        if (self::shape($container) !== $expected) {
            return null;
        }
        $out = [];
        foreach ($boxes as $k => $widget) {
            self::put($out, 'features.' . $k . '.title', $widget, ['title_text'], 'text');
            self::put($out, 'features.' . $k . '.text', $widget, ['description_text'], 'text');
        }

        return $out;
    }

    /**
     * Testimonials, featured: the first across the width, the others in rows
     * of ElementorPatternCompiler::SMALLER_PER_ROW columns.
     *
     * @param array<mixed> $container The stored container.
     * @return array<string,array{id:string,at:list<int|string>,form:string,text:string}>|null
     */
    private static function testimonials(array $container): ?array
    {
        $widgets = self::widgets($container, 'testimonial');
        $n       = count($widgets);
        if (!self::within('testimonials', $n)) {
            return null;
        }
        $parts = ['testimonial'];
        for ($left = $n - 1; $left > 0; $left -= ElementorPatternCompiler::SMALLER_PER_ROW) {
            $parts[] = 'R[' . implode(',', array_fill(0, min($left, ElementorPatternCompiler::SMALLER_PER_ROW), 'C[testimonial]')) . ']';
        }
        if (self::shape($container) !== 'C[' . implode(',', $parts) . ']') {
            return null;
        }
        $out = [];
        foreach ($widgets as $k => $widget) {
            self::put($out, 'testimonials.' . $k . '.quote', $widget, ['testimonial_content'], 'text');
            self::put($out, 'testimonials.' . $k . '.name', $widget, ['testimonial_name'], 'text');
            self::put($out, 'testimonials.' . $k . '.role', $widget, ['testimonial_job'], 'text');
        }

        return $out;
    }

    /**
     * Pricing: a row with a card for each plan.
     *
     * @param array<mixed> $container The stored container.
     * @return array<string,array{id:string,at:list<int|string>,form:string,text:string}>|null
     */
    private static function pricing(array $container): ?array
    {
        $cards = $container['elements'] ?? null;
        if (!is_array($cards) || !ArrayShape::isList($cards) || !self::within('plans', count($cards)) || self::shape($container)[0] !== 'R') {
            return null;
        }
        foreach ($cards as $card) {
            if (!in_array(self::shape($card), self::CARD_SHAPES, true)) {
                return null;
            }
            // A plan lists 1 to the grammar's most features, in a list of positions.
            $lines = $card['elements'][$card['elements'][2]['widgetType'] === 'text-editor' ? 3 : 2]['settings']['icon_list'] ?? null;
            if (!is_array($lines) || !ArrayShape::isList($lines) || $lines === [] || count($lines) > OutlineV3Schema::MAX_PLAN_FEATURES) {
                return null;
            }
        }
        $out = [];
        foreach ($cards as $k => $card) {
            $parts  = $card['elements'];
            $period = $parts[2]['widgetType'] === 'text-editor';
            $list   = $parts[$period ? 3 : 2];
            $button = end($parts)['elements'][0];
            self::put($out, 'plans.' . $k . '.name', $parts[0], ['title'], 'text');
            self::put($out, 'plans.' . $k . '.price', $parts[1], ['title'], 'text');
            if ($period) {
                self::put($out, 'plans.' . $k . '.period', $parts[2], ['editor'], 'paragraph');
            }
            foreach (array_keys($list['settings']['icon_list']) as $j) {
                self::put($out, 'plans.' . $k . '.features.' . $j, $list, ['icon_list', $j, 'text'], 'text');
            }
            self::put($out, 'plans.' . $k . '.cta.text', $button, ['text'], 'text');
            self::put($out, 'plans.' . $k . '.cta.url', $button, ['link', 'url'], 'url');
        }

        return $out;
    }

    /**
     * Faq: a column holding one accordion, a tab for each question.
     *
     * @param array<mixed> $container The stored container.
     * @return array<string,array{id:string,at:list<int|string>,form:string,text:string}>|null
     */
    private static function faq(array $container): ?array
    {
        if (self::shape($container) !== 'C[accordion]') {
            return null;
        }
        $accordion = $container['elements'][0];
        $tabs      = $accordion['settings']['tabs'] ?? null;
        if (!is_array($tabs) || !ArrayShape::isList($tabs) || !self::within('faq', count($tabs))) {
            return null;
        }
        $out = [];
        foreach (array_keys($tabs) as $k) {
            self::put($out, 'faq.' . $k . '.question', $accordion, ['tabs', $k, 'tab_title'], 'text');
            self::put($out, 'faq.' . $k . '.answer', $accordion, ['tabs', $k, 'tab_content'], 'paragraph');
        }

        return $out;
    }

    /**
     * Stats: a row of equal columns, one counter in each.
     *
     * @param array<mixed> $container The stored container.
     * @return array<string,array{id:string,at:list<int|string>,form:string,text:string}>|null
     */
    private static function stats(array $container): ?array
    {
        $counters = self::widgets($container, 'counter');
        $n        = count($counters);
        if (!self::within('stats', $n) || self::shape($container) !== 'R[' . implode(',', array_fill(0, $n, 'C[counter]')) . ']') {
            return null;
        }
        $out = [];
        foreach ($counters as $k => $widget) {
            self::put($out, 'stats.' . $k . '.value', $widget, ['ending_number'], 'number');
            self::put($out, 'stats.' . $k . '.suffix', $widget, ['suffix'], 'text');
            self::put($out, 'stats.' . $k . '.label', $widget, ['title'], 'text');
        }

        return $out;
    }

    /**
     * Logos: one row of picture widgets. A logo's alt text is the library's, so
     * the pattern offers no path.
     *
     * @param array<mixed> $container The stored container.
     * @return array<string,array{id:string,at:list<int|string>,form:string,text:string}>|null
     */
    private static function logos(array $container): ?array
    {
        $n = count(self::widgets($container, 'image'));

        return self::within('logos', $n) && self::shape($container) === 'R[' . implode(',', array_fill(0, $n, 'image')) . ']' ? [] : null;
    }

    /**
     * Whether an item count is one the grammar allows an item pattern.
     *
     * @param string $array The item array: features, testimonials, plans, faq, logos or stats.
     * @param int    $n     The count.
     * @return bool
     */
    private static function within(string $array, int $n): bool
    {
        [$min, $max] = OutlineV3Schema::ITEM_CAPS[$array];

        return $n >= $min && $n <= $max;
    }

    /**
     * The shape of a stored element: a container is C[...] for a column or R[...]
     * for a row around the shapes of its children; a widget is its type. Anything
     * else, an element with no id or one nested past MAX_DEPTH is "?", which no
     * expected shape holds.
     *
     * @param mixed $node  A stored element.
     * @param int   $depth How deep it stands below the pattern.
     * @return string
     */
    private static function shape(mixed $node, int $depth = 0): string
    {
        if (!is_array($node) || !is_string($node['id'] ?? null) || $depth > self::MAX_DEPTH) {
            return '?';
        }
        $settings = $node['settings'] ?? [];
        $children = $node['elements'] ?? [];
        if (!is_array($settings) || !is_array($children) || !ArrayShape::isList($children)) {
            return '?';
        }
        $elType = $node['elType'] ?? null;
        if ($elType === 'widget') {
            $type = $node['widgetType'] ?? null;

            return is_string($type) && $children === [] && preg_match('/^[a-z][a-z0-9_-]{0,63}$/D', $type) === 1 ? $type : '?';
        }
        $direction = $settings['flex_direction'] ?? 'column';
        if ($elType !== 'container' || ($direction !== 'row' && $direction !== 'column')) {
            return '?';
        }
        $parts = [];
        foreach ($children as $child) {
            $parts[] = self::shape($child, $depth + 1);
        }

        return ($direction === 'row' ? 'R' : 'C') . '[' . implode(',', $parts) . ']';
    }

    /**
     * The widgets of a type below a container, in document order.
     *
     * @param array<mixed> $node  The container.
     * @param string       $type  The widget type.
     * @param int          $depth How deep the node stands below the pattern.
     * @return list<array<mixed>>
     */
    private static function widgets(array $node, string $type, int $depth = 0): array
    {
        $found    = [];
        $children = $node['elements'] ?? [];
        foreach (is_array($children) && $depth <= self::MAX_DEPTH ? $children : [] as $child) {
            if (!is_array($child)) {
                continue;
            }
            if (($child['elType'] ?? null) === 'widget' && ($child['widgetType'] ?? null) === $type) {
                $found[] = $child;
                continue;
            }
            array_push($found, ...self::widgets($child, $type, $depth + 1));
        }

        return $found;
    }

    /**
     * Offer a path when the widget stores a string of the form at the settings path.
     *
     * @param array<string,array{id:string,at:list<int|string>,form:string,text:string}> $out  The fields so far.
     * @param string                                                                       $path The pattern path.
     * @param array<mixed>                                                                 $widget The stored widget.
     * @param list<int|string>                                                             $at   Where its value is, below its settings.
     * @param string                                                                       $form text, paragraph, number or url.
     * @return void
     */
    private static function put(array &$out, string $path, array $widget, array $at, string $form): void
    {
        $value = $widget['settings'] ?? null;
        foreach ($at as $step) {
            if (!is_array($value) || !array_key_exists($step, $value)) {
                return;
            }
            $value = $value[$step];
        }
        $id = $widget['id'] ?? null;
        if (!is_string($value) || !is_string($id)) {
            return;
        }
        $text = match ($form) {
            'paragraph' => ElementorClassicMapper::paragraphText($value),
            'number'    => preg_match(self::RE_NUMBER, $value) === 1 ? $value : null,
            default     => ElementorClassicMapper::decode($value),
        };
        if ($text === null) {
            return;
        }
        $out[$path] = ['id' => $id, 'at' => $at, 'form' => $form, 'text' => $text];
    }
}
