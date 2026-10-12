<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Abilities\PageCreateBuilder;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The input schema of outline version 3 for wpmgr/page-create and
 * wpmgr/page-edit, and the numbers its grammar shares with the schema.
 *
 * Version 3 is a designed Elementor page. The input adds, next to the version 2
 * keys, `outline_version` (3), `design`, and `page_frame`. The outline is a
 * list of sections:
 *
 *   {"type":"section","anchor":ANCHOR,"width"?,"tone"?,"background"?,"spacing"?,
 *    "align"?,"min_height"?,"children":[1..50 of leaf | group | columns | pattern]}
 *
 * A pattern is {"type":"pattern","pattern":NAME,"variant":VARIANT,...}. hero and
 * cta hold children (leaves); the others hold one array of typed items, named
 * in PATTERN_ARRAY and counted by ITEM_CAPS. Per-node style is token references
 * only, so no node carries a raw value.
 *
 * Every object is closed and every variant is written out in full (no $ref),
 * like the version 2 schema. Rules a schema cannot say (a unique anchor, the
 * hero's place, contrast of two colours, an anchor a link points at) belong to
 * the grammar, not to this file.
 *
 * Pure: no WordPress call.
 */
final class OutlineV3Schema
{
    /** The outline version this grammar is. Absent means version 2. */
    public const VERSION = 3;

    /** Where the page sits in the theme. */
    public const PAGE_FRAMES = ['theme', 'full_width', 'canvas'];

    /** The six colour roles of a design. */
    public const COLOUR_ROLES = ['ground', 'surface', 'ink', 'muted', 'accent', 'on_accent'];

    /** A design colour: upper-case hex. */
    public const COLOUR_PATTERN = '^#[0-9A-F]{6}$';

    /** A font family name; the literal "system" means do not set a family. */
    public const FONT_PATTERN = '^[A-Za-z0-9 ]{1,60}$';

    public const SCALE_BASE_PX = [15, 20];

    /**
     * The type-scale ratios of a design, by name, smallest first. The input
     * carries the name and never the number: no field of the input has a
     * fraction. DesignTokens::RATIOS maps each name to its number.
     */
    public const SCALE_RATIOS = ['major_second', 'minor_third', 'major_third', 'perfect_fourth'];

    public const RADII = ['none', 'sm', 'md', 'lg'];

    public const DENSITIES = ['compact', 'normal', 'airy'];

    /** A section anchor: unique on the page. */
    public const ANCHOR_PATTERN = '^[a-z][a-z0-9-]{1,39}$';

    /** Ids a theme already owns; refused as anchors. */
    public const RESERVED_ANCHORS = ['content', 'main', 'primary', 'page', 'masthead', 'colophon', 'site-header', 'site-footer', 'wpadminbar'];

    public const WIDTHS = ['boxed', 'full'];

    public const TONES = ['ground', 'surface', 'inverse', 'accent'];

    public const SPACINGS = ['md', 'lg', 'xl'];

    public const ALIGNS = ['start', 'center'];

    public const MIN_HEIGHTS = ['auto', 'screen'];

    public const OVERLAYS = ['none', 'soft', 'strong'];

    public const FOCALS = ['center', 'top', 'bottom'];

    /** Sections on a page, and heroes on a page. */
    public const MAX_SECTIONS = 20;

    public const MAX_HEROES = 1;

    /** Children of one section. */
    public const MAX_SECTION_CHILDREN = 50;

    /** Children of a hero and of a cta. */
    public const MAX_HERO_CHILDREN = 6;

    public const MAX_CTA_CHILDREN = 4;

    /** Every pattern, composed ones first. */
    public const PATTERNS = ['hero', 'cta', 'features', 'testimonials', 'pricing', 'faq', 'logos', 'stats'];

    /** The variants each pattern offers. */
    public const VARIANTS = [
        'hero'         => ['background', 'split', 'centered'],
        'cta'          => ['band'],
        'features'     => ['split', 'bento', 'grid'],
        'testimonials' => ['featured'],
        'pricing'      => ['three-plans'],
        'faq'          => ['accordion'],
        'logos'        => ['row'],
        'stats'        => ['counters'],
    ];

    /** The array an item pattern keeps its items in; its path prefix in a set_text field. */
    public const PATTERN_ARRAY = [
        'features'     => 'features',
        'testimonials' => 'testimonials',
        'pricing'      => 'plans',
        'faq'          => 'faq',
        'logos'        => 'logos',
        'stats'        => 'stats',
    ];

    /** Items per array: [minimum, maximum]. */
    public const ITEM_CAPS = [
        'features'     => [3, 8],
        'testimonials' => [1, 6],
        'plans'        => [1, 4],
        'faq'          => [1, 12],
        'logos'        => [3, 10],
        'stats'        => [2, 4],
    ];

    /** Plan features in one pricing plan. */
    public const MAX_PLAN_FEATURES = 8;

    /** Characters of a counter suffix such as "+" or "%". */
    public const MAX_SUFFIX_CHARS = 6;

    /** The largest value a counter shows. */
    public const MAX_STAT_VALUE = 999999999;

    /**
     * The closed list of icon names a features item may show. Each maps to a
     * built-in icon of the builder; there is no upload.
     */
    public const ICONS = [
        'arrow-right', 'award', 'bell', 'bolt', 'book', 'briefcase', 'calendar', 'chart', 'check', 'clock',
        'cloud', 'code', 'cog', 'comment', 'compass', 'credit-card', 'cube', 'database', 'envelope', 'eye',
        'flag', 'gift', 'globe', 'heart', 'home', 'image', 'key', 'laptop', 'layers', 'leaf',
        'lightbulb', 'lock', 'map-pin', 'phone', 'rocket', 'search', 'shield', 'star', 'tag', 'users',
    ];

    /**
     * What set_text may name beyond the plain fields: a pattern path, that is
     * the item array, the item's position (from 0) and the item's field, with
     * up to two more steps for a nested field such as a plan's button text.
     */
    public const EDIT_PATH_PATTERN = '^(features|testimonials|plans|faq|logos|stats)\.(0|[1-9][0-9]?)(\.[a-z0-9_]{1,24}){1,3}$';

    /** @return array<string,mixed> */
    public static function versionSchema(): array
    {
        return ['type' => 'integer', 'enum' => [self::VERSION]];
    }

    /** @return array<string,mixed> */
    public static function pageFrameSchema(): array
    {
        return ['type' => 'string', 'enum' => self::PAGE_FRAMES];
    }

    /**
     * The design object: colours, fonts, scale, radius and density, all
     * required. Whether the colours are legible together is the grammar's.
     *
     * @return array<string,mixed>
     */
    public static function designSchema(): array
    {
        $colour = ['type' => 'string', 'pattern' => self::COLOUR_PATTERN];
        $font   = ['type' => 'string', 'pattern' => self::FONT_PATTERN];
        $roles  = [];
        foreach (self::COLOUR_ROLES as $role) {
            $roles[$role] = $colour;
        }

        return self::object([
            'colors'  => self::object($roles, self::COLOUR_ROLES),
            'fonts'   => self::object(['heading' => $font, 'body' => $font], ['heading', 'body']),
            'scale'   => self::object([
                'base_px' => ['type' => 'integer', 'minimum' => self::SCALE_BASE_PX[0], 'maximum' => self::SCALE_BASE_PX[1]],
                'ratio'   => ['type' => 'string', 'enum' => self::SCALE_RATIOS],
            ], ['base_px', 'ratio']),
            'radius'  => ['type' => 'string', 'enum' => self::RADII],
            'density' => ['type' => 'string', 'enum' => self::DENSITIES],
        ], ['colors', 'fonts', 'scale', 'radius', 'density']);
    }

    /**
     * The section node. $leaves, $group and $columns are the version 3 forms
     * of the outline nodes a section may hold.
     *
     * @param list<array<string,mixed>> $leaves  Leaf schemas.
     * @param array<string,mixed>       $group   The group schema.
     * @param array<string,mixed>       $columns The columns schema.
     * @return array<string,mixed>
     */
    public static function section(array $leaves, array $group, array $columns): array
    {
        $children = array_merge($leaves, [$group, $columns], self::patterns($leaves));

        return self::node('section', [
            'anchor'     => ['type' => 'string', 'pattern' => self::ANCHOR_PATTERN],
            'width'      => ['type' => 'string', 'enum' => self::WIDTHS],
            'tone'       => ['type' => 'string', 'enum' => self::TONES],
            'background' => self::object([
                'attachment_id' => ['type' => 'integer', 'minimum' => 1, 'maximum' => PageCreateBuilder::MAX_ATTACHMENT_ID],
                'overlay'       => ['type' => 'string', 'enum' => self::OVERLAYS],
                'focal'         => ['type' => 'string', 'enum' => self::FOCALS],
            ], ['attachment_id']),
            'spacing'    => ['type' => 'string', 'enum' => self::SPACINGS],
            'align'      => ['type' => 'string', 'enum' => self::ALIGNS],
            'min_height' => ['type' => 'string', 'enum' => self::MIN_HEIGHTS],
            'children'   => ['type' => 'array', 'minItems' => 1, 'maxItems' => self::MAX_SECTION_CHILDREN, 'items' => ['oneOf' => $children]],
        ], ['anchor', 'children']);
    }

    /**
     * The pattern nodes, in PATTERNS order.
     *
     * @param list<array<string,mixed>> $leaves Leaf schemas (the version 3 forms).
     * @return list<array<string,mixed>>
     */
    public static function patterns(array $leaves): array
    {
        $text  = ['type' => 'string', 'minLength' => 1, 'maxLength' => PageCreateBuilder::MAX_TEXT_CHARS];
        $short = ['type' => 'string', 'minLength' => 1, 'maxLength' => PageCreateBuilder::MAX_BUTTON_TEXT_CHARS];
        $url   = ['type' => 'string', 'minLength' => 1, 'maxLength' => PageCreateBuilder::MAX_URL_BYTES];
        $image = ['type' => 'integer', 'minimum' => 1, 'maximum' => PageCreateBuilder::MAX_ATTACHMENT_ID];

        $out = [
            self::pattern('hero', [
                'children' => self::kids(self::pick($leaves, ['heading', 'paragraph', 'buttons', 'image']), self::MAX_HERO_CHILDREN),
            ], ['children']),
            self::pattern('cta', [
                'children' => self::kids(self::pick($leaves, ['heading', 'paragraph', 'buttons']), self::MAX_CTA_CHILDREN),
            ], ['children']),
            self::pattern('features', [
                'features' => self::items('features', self::object([
                    'icon'  => ['type' => 'string', 'enum' => self::ICONS],
                    'title' => $text,
                    'text'  => $text,
                ], ['icon', 'title', 'text'])),
            ], ['features']),
            self::pattern('testimonials', [
                'testimonials' => self::items('testimonials', self::object([
                    'quote'         => $text,
                    'name'          => $text,
                    'role'          => $text,
                    'attachment_id' => $image,
                ], ['quote', 'name'])),
            ], ['testimonials']),
            self::pattern('pricing', [
                'plans' => self::items('plans', self::object([
                    'name'        => $text,
                    'price'       => $text,
                    'period'      => $text,
                    'features'    => ['type' => 'array', 'minItems' => 1, 'maxItems' => self::MAX_PLAN_FEATURES, 'items' => $text],
                    'cta'         => self::object(['text' => $short, 'url' => $url], ['text', 'url']),
                    'highlighted' => ['type' => 'boolean'],
                ], ['name', 'price', 'features', 'cta'])),
            ], ['plans']),
            self::pattern('faq', [
                'faq' => self::items('faq', self::object(['question' => $text, 'answer' => $text], ['question', 'answer'])),
            ], ['faq']),
            self::pattern('logos', [
                'logos' => self::items('logos', self::object([
                    'attachment_id' => $image,
                    'alt'           => ['type' => 'string', 'maxLength' => PageCreateBuilder::MAX_ALT_CHARS],
                ], ['attachment_id', 'alt'])),
            ], ['logos']),
            self::pattern('stats', [
                'stats' => self::items('stats', self::object([
                    'value'  => ['type' => 'integer', 'minimum' => 0, 'maximum' => self::MAX_STAT_VALUE],
                    'suffix' => ['type' => 'string', 'minLength' => 1, 'maxLength' => self::MAX_SUFFIX_CHARS],
                    'label'  => $text,
                ], ['value', 'label'])),
            ], ['stats']),
        ];

        return $out;
    }

    /**
     * The schema of one item of an item pattern, as the input schema publishes
     * it; null for a pattern that holds children (hero, cta) and for a name
     * that is no pattern.
     *
     * @param string $pattern Pattern name.
     * @return array<string,mixed>|null
     */
    public static function itemSchema(string $pattern): ?array
    {
        $array = self::PATTERN_ARRAY[$pattern] ?? null;
        if ($array === null) {
            return null;
        }
        foreach (self::patterns([]) as $node) {
            if (($node['properties']['pattern']['enum'][0] ?? null) === $pattern) {
                return $node['properties'][$array]['items'];
            }
        }

        return null;
    }

    /**
     * What set_text may name in `field`: a plain field of the node, or a
     * pattern path.
     *
     * @param list<string> $fields The plain fields.
     * @return array<string,mixed>
     */
    public static function editFieldSchema(array $fields): array
    {
        return [
            'type'  => 'string',
            'anyOf' => [
                ['enum' => $fields],
                ['pattern' => self::EDIT_PATH_PATTERN],
            ],
        ];
    }

    // ---------------------------------------------------------------------
    // Builders
    // ---------------------------------------------------------------------

    /**
     * @param array<string,mixed> $props    Properties.
     * @param list<string>        $required Required properties.
     * @return array<string,mixed>
     */
    private static function object(array $props, array $required): array
    {
        return [
            'type'                 => 'object',
            'properties'           => $props,
            'required'             => $required,
            'additionalProperties' => false,
        ];
    }

    /**
     * @param string              $type     Node type.
     * @param array<string,mixed> $props    Properties other than type.
     * @param list<string>        $required Required properties other than type.
     * @return array<string,mixed>
     */
    private static function node(string $type, array $props, array $required): array
    {
        return self::object(['type' => ['const' => $type]] + $props, array_merge(['type'], $required));
    }

    /**
     * A pattern node: its name and variant are closed enums.
     *
     * @param string              $name     Pattern name.
     * @param array<string,mixed> $props    Properties other than type, pattern and variant.
     * @param list<string>        $required Required properties other than type, pattern and variant.
     * @return array<string,mixed>
     */
    private static function pattern(string $name, array $props, array $required): array
    {
        return self::node('pattern', [
            'pattern' => ['type' => 'string', 'enum' => [$name]],
            'variant' => ['type' => 'string', 'enum' => self::VARIANTS[$name]],
        ] + $props, array_merge(['pattern', 'variant'], $required));
    }

    /**
     * The item list of an item pattern, counted by ITEM_CAPS.
     *
     * @param string              $array Array name.
     * @param array<string,mixed> $item  Item schema.
     * @return array<string,mixed>
     */
    private static function items(string $array, array $item): array
    {
        return ['type' => 'array', 'minItems' => self::ITEM_CAPS[$array][0], 'maxItems' => self::ITEM_CAPS[$array][1], 'items' => $item];
    }

    /**
     * @param list<array<string,mixed>> $leaves Allowed leaf schemas.
     * @param int                       $max    Most children.
     * @return array<string,mixed>
     */
    private static function kids(array $leaves, int $max): array
    {
        return ['type' => 'array', 'minItems' => 1, 'maxItems' => $max, 'items' => ['oneOf' => $leaves]];
    }

    /**
     * The leaf schemas of the named types, in the order named.
     *
     * @param list<array<string,mixed>> $leaves Leaf schemas.
     * @param list<string>              $types  Types.
     * @return list<array<string,mixed>>
     */
    private static function pick(array $leaves, array $types): array
    {
        $out = [];
        foreach ($types as $type) {
            foreach ($leaves as $leaf) {
                if (($leaf['properties']['type']['const'] ?? null) === $type) {
                    $out[] = $leaf;
                }
            }
        }

        return $out;
    }
}
