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
 * The patterns of a designed page, as Elementor containers.
 *
 * A pattern is a child of a section: one labelled container, an inner one, that
 * holds the pattern's content. Its Navigator label (LeafPolicy::LABEL_KEY) is
 * label(): LeafPolicy::LABEL_PREFIX, the pattern's name and its variant, so the
 * label alone says which pattern and variant the subtree is, and fromLabel()
 * reads it back.
 *
 *   hero background  a group (the full-width column) holding the hero's blocks,
 *   hero centered    centred: the heading, the lead, the buttons and a picture
 *   cta band         the same, for a call to action
 *   hero split       a columns row, copy in the 55 percent column on the left
 *                    and the picture in the 45 percent column on the right;
 *                    the row stacks on a phone. A split hero with no picture is
 *                    the stacked group
 *   stats counters   a columns row of equal columns, one counter widget in each
 *   logos row        one row that wraps, centred, holding the logos' image
 *                    widgets; it does not stack on a phone
 *
 * The blocks of a hero or a cta arrive built, by the leaf compiler in the tone
 * of the section and in the alignment align() names for the variant: the
 * pattern decides how its blocks are laid out, so the section's own alignment
 * does not reach them. A centred pattern also centres each row of buttons.
 *
 * The group, the row and its columns are the section compiler's, so they are
 * the settings the round-trip probes store; this class adds the label after
 * the last key, where Elementor returns it.
 *
 * Every argument comes from OutlineV3, which has judged it; one outside what the
 * grammar accepts is a programming error and throws \InvalidArgumentException.
 * Node ids come from the IdSeed, keyed by the pattern's outline path and, for a
 * container the outline never names, by that path and a "#" suffix.
 *
 * Pure: no WordPress call.
 */
final class ElementorPatternCompiler
{
    /** The Navigator name of each pattern, as written after LeafPolicy::LABEL_PREFIX. */
    private const NAMES = [
        'hero'         => 'Hero',
        'cta'          => 'CTA',
        'features'     => 'Features',
        'testimonials' => 'Testimonials',
        'pricing'      => 'Pricing',
        'faq'          => 'FAQ',
        'logos'        => 'Logos',
        'stats'        => 'Stats',
    ];

    /** The patterns that hold blocks the leaf compiler builds. */
    public const COMPOSED = ['hero', 'cta'];

    /** The alignment the blocks of a composed pattern are built in, by pattern and variant. */
    private const ALIGN = [
        'hero' => ['background' => 'center', 'split' => 'start', 'centered' => 'center'],
        'cta'  => ['band' => 'center'],
    ];

    /** The width of the copy column and of the picture column of a split hero, in percent. */
    public const SPLIT_COPY_PERCENT = 55;

    public const SPLIT_MEDIA_PERCENT = 45;

    /** The width of a logo, in px. */
    private const LOGO_WIDTH_PX = 140;

    /** The refusal of a logo whose alt text is not the library's. */
    public const CODE_ALT_FROM_LIBRARY = 'image_alt_from_library';

    /** What the fix for that refusal is, in one line. */
    private const REASON_ALT = 'must be the alt text saved with the image in the media library, exactly; set it to that text, or change it in the media library first';

    /**
     * Every settings key a pattern writes that the mapper's own list does not
     * carry, by element type, with the pin that holds its whole value: a
     * counter, the rows and columns of a pattern and a wrapping row.
     *
     * @var array<string,array<string,string>>
     */
    public const KEYS = [
        'counter'   => [
            'starting_number' => 'enum:0',
            'ending_number'   => 'enum',
            'suffix'          => 'html',
            'title'           => 'html',
            'number_color'    => 'colour',
            'title_color'     => 'colour',
        ],
        'container' => [
            'flex_wrap'        => 'enum:wrap',
            'flex_gap'         => 'gaps',
            'flex_gap_tablet'  => 'gaps',
            'flex_gap_mobile'  => 'gaps',
            'flex_align_items' => 'enum:center',
            'width'            => 'slider:%',
            'width_mobile'     => 'slider:%',
        ],
    ];

    private DesignTokens $tokens;

    private ElementorSectionCompiler $sections;

    /** @var array<mixed> */
    private array $media;

    /**
     * @param DesignTokens             $tokens   The design's tokens.
     * @param ElementorSectionCompiler $sections The page's section compiler: its groups, rows and columns are the patterns' containers.
     * @param array<mixed>             $media    Resolved image facts by attachment id, as the page-create precheck hands them on.
     */
    public function __construct(DesignTokens $tokens, ElementorSectionCompiler $sections, array $media = [])
    {
        $this->tokens   = $tokens;
        $this->sections = $sections;
        $this->media    = $media;
    }

    /**
     * The Navigator label of a pattern's container.
     *
     * @param string $pattern One of OutlineV3Schema::PATTERNS.
     * @param string $variant One of that pattern's OutlineV3Schema::VARIANTS.
     * @return string
     * @throws \InvalidArgumentException When the pattern or the variant is one the grammar does not have.
     */
    public static function label(string $pattern, string $variant): string
    {
        $name = self::NAMES[$pattern] ?? null;
        if ($name === null || !in_array($variant, OutlineV3Schema::VARIANTS[$pattern] ?? [], true)) {
            throw new \InvalidArgumentException('a pattern is one of the patterns, with one of its variants');
        }

        return LeafPolicy::LABEL_PREFIX . $name . ' ' . $variant;
    }

    /**
     * The pattern and variant a label names; null for any other label.
     *
     * @param string $label A container's Navigator label.
     * @return array{pattern:string,variant:string}|null
     */
    public static function fromLabel(string $label): ?array
    {
        foreach (OutlineV3Schema::VARIANTS as $pattern => $variants) {
            foreach ($variants as $variant) {
                if ($label === self::label($pattern, $variant)) {
                    return ['pattern' => $pattern, 'variant' => $variant];
                }
            }
        }

        return null;
    }

    /**
     * The alignment the blocks of a composed pattern are built in: centre for a
     * hero over a picture, a centred hero and a call to action, start for a split
     * hero. Null for a pattern that holds no blocks and for a name the grammar
     * does not have.
     *
     * @param string $pattern A pattern.
     * @param string $variant Its variant.
     * @return string|null One of OutlineV3Schema::ALIGNS.
     */
    public static function align(string $pattern, string $variant): ?string
    {
        return self::ALIGN[$pattern][$variant] ?? null;
    }

    /**
     * The allowed keys of a page that holds patterns: the given allowlist with
     * this compiler's keys added. A key the allowlist already lists keeps its
     * pin, so adding these never narrows one.
     *
     * @param array<string,array<string,string>> $allowed Element type, then settings path, then pin.
     * @return array<string,array<string,string>>
     */
    public static function allowedKeys(array $allowed): array
    {
        foreach (self::KEYS as $type => $paths) {
            $allowed[$type] = ($allowed[$type] ?? []) + $paths;
        }

        return $allowed;
    }

    /**
     * A pattern as its container.
     *
     * @param IdSeed                    $ids     Node ids for this request.
     * @param string                    $path    The pattern's outline path, e.g. "outline[0].children[0]".
     * @param array<string,mixed>       $pattern The pattern node.
     * @param string                    $tone    The tone of the section it stands in.
     * @param list<array<string,mixed>> $blocks  The built blocks of a hero or a cta; none for any other pattern.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the pattern is not one this compiler builds, or holds a value the grammar does not accept.
     */
    public function compile(IdSeed $ids, string $path, array $pattern, string $tone, array $blocks = []): array
    {
        return match ($pattern['pattern'] ?? null) {
            'hero'  => $this->hero($ids, $path, $pattern, $blocks),
            'cta'   => $this->cta($ids, $path, $pattern, $blocks),
            'stats' => $this->stats($ids, $path, $pattern, $tone),
            'logos' => $this->logos($ids, $path, $pattern),
            default => throw new \InvalidArgumentException('this pattern is not one the compiler builds'),
        };
    }

    /**
     * Stats: a row of equal columns, one counter in each.
     *
     * A counter counts from 0 to its value, then shows the suffix, with its
     * label under it. The number is in the tone's heading colour and the label
     * in its accent colour. The row stacks on a phone.
     *
     * @param IdSeed              $ids     Node ids for this request.
     * @param string              $path    The pattern's outline path.
     * @param array<string,mixed> $pattern The stats node.
     * @param string              $tone    The tone of the section it stands in.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the node is not a stats pattern the grammar accepts.
     */
    public function stats(IdSeed $ids, string $path, array $pattern, string $tone): array
    {
        $label   = self::label('stats', self::variantOf($pattern, 'stats'));
        $items   = self::itemsOf($pattern, 'stats');
        $colours = $this->tokens->tone($tone);
        $rowId   = $ids->next($path);
        $width   = intdiv(100, count($items));
        $columns = [];
        foreach ($items as $i => $item) {
            $at        = $path . '.stats[' . $i . ']';
            $columnId  = $ids->next($at . '#column');
            $columns[] = $this->sections->column($columnId, $width, [self::counter($ids->next($at), $item, $colours)]);
        }

        return self::labelled($this->sections->row($rowId, $columns), $label);
    }

    /**
     * Logos: library images in one row that wraps, centred, never stacked.
     *
     * Each logo is an image of medium size, centred, LOGO_WIDTH_PX wide, with
     * the design's row gap between logos at each step. The picture's alt text
     * is the library's (logoAltRefusal() refuses any other before the page is
     * compiled).
     *
     * @param IdSeed              $ids     Node ids for this request.
     * @param string              $path    The pattern's outline path.
     * @param array<string,mixed> $pattern The logos node.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the node is not a logos pattern the grammar accepts, or names an image the site did not resolve.
     */
    public function logos(IdSeed $ids, string $path, array $pattern): array
    {
        $label    = self::label('logos', self::variantOf($pattern, 'logos'));
        $items    = self::itemsOf($pattern, 'logos');
        $id       = $ids->next($path);
        $settings = ['content_width' => 'full', 'flex_direction' => 'row', 'flex_wrap' => 'wrap']
            + $this->sections->gapSettings()
            + ['flex_justify_content' => 'center', 'flex_align_items' => 'center', LeafPolicy::LABEL_KEY => $label];
        $logos    = [];
        foreach ($items as $k => $item) {
            $logos[] = $this->logo($ids->next($path . '.logos[' . $k . ']'), $item);
        }

        return ['id' => $id, 'elType' => 'container', 'settings' => $settings, 'elements' => $logos, 'isInner' => true];
    }

    /**
     * The refusal of the logos whose alt text is not the alt text saved with
     * their image in the media library, each failure naming the logo; null when
     * none is. A logo whose image the site did not resolve is the image
     * resolution's, not this check's.
     *
     * @param mixed        $outline The validated outline: a list of sections.
     * @param array<mixed> $media   Resolved image facts by attachment id.
     * @return array{code:string,detail:string,failures:list<array<string,mixed>>}|null
     */
    public static function logoAltRefusal(mixed $outline, array $media): ?array
    {
        if (!is_array($outline) || !ArrayShape::isList($outline)) {
            return null;
        }
        $failures = [];
        foreach ($outline as $i => $section) {
            $children = is_array($section) ? ($section['children'] ?? null) : null;
            if (!is_array($children)) {
                continue;
            }
            foreach ($children as $j => $child) {
                $logos = is_array($child) && ($child['pattern'] ?? null) === 'logos' ? ($child['logos'] ?? null) : null;
                foreach (is_array($logos) ? $logos : [] as $k => $logo) {
                    $id      = is_array($logo) ? ($logo['attachment_id'] ?? null) : null;
                    $alt     = is_array($logo) ? ($logo['alt'] ?? null) : null;
                    $fact    = is_int($id) ? ($media[$id] ?? null) : null;
                    $library = is_array($fact) ? ($fact['alt'] ?? null) : null;
                    if (is_string($alt) && is_string($library) && $alt !== $library) {
                        $failures[] = ['path' => 'outline[' . $i . '].children[' . $j . '].logos[' . $k . ']', 'key' => 'alt', 'reason' => self::REASON_ALT];
                    }
                }
            }
        }

        return $failures === [] ? null : Refusal::make(self::CODE_ALT_FROM_LIBRARY, 'A logo has alt text that is not the one saved with its image in the media library.', $failures);
    }

    /**
     * A hero around its built blocks.
     *
     * @param IdSeed                    $ids      Node ids for this request.
     * @param string                    $path     The hero's outline path.
     * @param array<string,mixed>       $pattern  The hero node.
     * @param list<array<string,mixed>> $elements Its blocks, built in align()'s alignment.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the node is not a hero with a variant of the grammar, or has no blocks.
     */
    public function hero(IdSeed $ids, string $path, array $pattern, array $elements): array
    {
        $variant = self::variantOf($pattern, 'hero');
        $label   = self::label('hero', $variant);
        if ($elements === []) {
            throw new \InvalidArgumentException('a hero holds at least one block');
        }
        $id = $ids->next($path);
        if ($variant === 'split') {
            return $this->split($ids, $path, $id, $label, $elements);
        }

        return $this->stack($id, $label, self::centred($elements));
    }

    /**
     * A call to action around its built blocks.
     *
     * @param IdSeed                    $ids      Node ids for this request.
     * @param string                    $path     The cta's outline path.
     * @param array<string,mixed>       $pattern  The cta node.
     * @param list<array<string,mixed>> $elements Its blocks, built in align()'s alignment.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the node is not a cta with a variant of the grammar, or has no blocks.
     */
    public function cta(IdSeed $ids, string $path, array $pattern, array $elements): array
    {
        $label = self::label('cta', self::variantOf($pattern, 'cta'));
        if ($elements === []) {
            throw new \InvalidArgumentException('a call to action holds at least one block');
        }

        return $this->stack($ids->next($path), $label, self::centred($elements));
    }

    /**
     * The items of an item pattern, counted by the grammar's caps.
     *
     * @param array<string,mixed> $pattern The pattern node.
     * @param string              $name    stats or logos.
     * @return list<mixed>
     * @throws \InvalidArgumentException When the items are not a list of the grammar's size.
     */
    private static function itemsOf(array $pattern, string $name): array
    {
        $items = $pattern[OutlineV3Schema::PATTERN_ARRAY[$name]] ?? null;
        [$min, $max] = OutlineV3Schema::ITEM_CAPS[OutlineV3Schema::PATTERN_ARRAY[$name]];
        if (!is_array($items) || !ArrayShape::isList($items) || count($items) < $min || count($items) > $max) {
            throw new \InvalidArgumentException('a pattern holds as many items as the grammar allows it');
        }

        return $items;
    }

    /**
     * One counter, in the key order Elementor returns.
     *
     * @param string                 $id      The widget's id.
     * @param mixed                  $item    The stats item: value, optional suffix, label.
     * @param array<string,string>   $colours The tone's colours.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,widgetType:string}
     * @throws \InvalidArgumentException When the item holds a value the grammar does not accept.
     */
    private static function counter(string $id, mixed $item, array $colours): array
    {
        $value  = is_array($item) ? ($item['value'] ?? null) : null;
        $label  = is_array($item) ? ($item['label'] ?? null) : null;
        $suffix = is_array($item) ? ($item['suffix'] ?? null) : null;
        if (!is_int($value) || $value < 0 || $value > OutlineV3Schema::MAX_STAT_VALUE) {
            throw new \InvalidArgumentException('a counter shows a whole number up to the largest the grammar allows');
        }
        if (!is_string($label) || $label === '' || ($suffix !== null && (!is_string($suffix) || $suffix === ''))) {
            throw new \InvalidArgumentException('a counter has a label and, when it has one, a suffix of text');
        }
        $settings = ['starting_number' => '0', 'ending_number' => (string) $value];
        if ($suffix !== null) {
            $settings['suffix'] = LeafPolicy::htmlText($suffix);
        }
        $settings['title']        = LeafPolicy::htmlText($label);
        $settings['number_color'] = $colours['heading'];
        $settings['title_color']  = $colours['accent'];

        return ['id' => $id, 'elType' => 'widget', 'settings' => $settings, 'elements' => [], 'widgetType' => 'counter'];
    }

    /**
     * One logo: the library image, medium size, centred, LOGO_WIDTH_PX wide.
     *
     * @param string $id   The widget's id.
     * @param mixed  $item The logos item: attachment_id and alt.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,widgetType:string}
     * @throws \InvalidArgumentException When the item is not an image the site resolved before the page is compiled.
     */
    private function logo(string $id, mixed $item): array
    {
        $attachment = is_array($item) ? ($item['attachment_id'] ?? null) : null;
        if (!is_int($attachment) || $attachment < 1 || $attachment > PageCreateBuilder::MAX_ATTACHMENT_ID) {
            throw new \InvalidArgumentException('a logo names its image by a whole number');
        }
        $fact = $this->media[$attachment] ?? null;
        $url  = is_array($fact) ? ($fact['url'] ?? null) : null;
        if (!is_array($fact) || ($fact['id'] ?? null) !== $attachment || !is_string($url) || PageCreateBuilder::imageUrlProblem($url) !== null
            || !in_array($fact['mime'] ?? null, PageCreateBuilder::IMAGE_MIMES, true)) {
            throw new \InvalidArgumentException('a logo is an image the site resolved before the page is compiled');
        }
        $settings = [
            'image'      => ['url' => $url, 'id' => (string) $attachment, 'source' => 'library'],
            'image_size' => 'medium',
            'align'      => 'center',
            'width'      => ['unit' => 'px', 'size' => (string) self::LOGO_WIDTH_PX],
        ];

        return ['id' => $id, 'elType' => 'widget', 'settings' => $settings, 'elements' => [], 'widgetType' => 'image'];
    }

    /**
     * The variant of a pattern node of the given name.
     *
     * @param array<string,mixed> $pattern The pattern node.
     * @param string              $name    The pattern it must be.
     * @return string
     * @throws \InvalidArgumentException When it is another pattern or has no variant of the grammar.
     */
    private static function variantOf(array $pattern, string $name): string
    {
        $variant = $pattern['variant'] ?? null;
        if (($pattern['pattern'] ?? null) !== $name || !is_string($variant) || !in_array($variant, OutlineV3Schema::VARIANTS[$name], true)) {
            throw new \InvalidArgumentException('a pattern node names its pattern and one of that pattern\'s variants');
        }

        return $variant;
    }

    /**
     * The blocks in one full-width column, labelled.
     *
     * @param string                    $id       The container's id.
     * @param string                    $label    Its Navigator label.
     * @param list<array<string,mixed>> $elements Its blocks.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     */
    private function stack(string $id, string $label, array $elements): array
    {
        return self::labelled($this->sections->group($id, $elements), $label);
    }

    /**
     * A split hero: the copy on the left and the pictures on the right, in a
     * labelled row. Without a picture, or without copy, it is the stacked group.
     *
     * @param IdSeed                    $ids      Node ids for this request.
     * @param string                    $path     The hero's outline path.
     * @param string                    $id       The container's id.
     * @param string                    $label    Its Navigator label.
     * @param list<array<string,mixed>> $elements Its blocks.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     */
    private function split(IdSeed $ids, string $path, string $id, string $label, array $elements): array
    {
        $copy  = [];
        $media = [];
        foreach ($elements as $element) {
            if (($element['widgetType'] ?? null) === 'image') {
                $media[] = $element;
            } else {
                $copy[] = $element;
            }
        }
        if ($copy === [] || $media === []) {
            return $this->stack($id, $label, $elements);
        }
        $columns = [
            $this->sections->column($ids->next($path . '#copy'), self::SPLIT_COPY_PERCENT, $copy),
            $this->sections->column($ids->next($path . '#media'), self::SPLIT_MEDIA_PERCENT, $media),
        ];

        return self::labelled($this->sections->row($id, $columns), $label);
    }

    /**
     * Each row of buttons among the elements centred: the key follows the row
     * keys the leaf compiler wrote, where it writes it for a centred row.
     *
     * @param list<array<string,mixed>> $elements Built blocks.
     * @return list<array<string,mixed>>
     */
    private static function centred(array $elements): array
    {
        foreach ($elements as $i => $element) {
            $children = $element['elements'] ?? [];
            if (($element['elType'] ?? null) !== 'container' || $children === [] || isset($element['settings']['flex_justify_content'])) {
                continue;
            }
            if (array_column($children, 'widgetType') === array_fill(0, count($children), 'button')) {
                $elements[$i]['settings']['flex_justify_content'] = 'center';
            }
        }

        return $elements;
    }

    /**
     * A container with its Navigator label after the last key.
     *
     * @param array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool} $container A container.
     * @param string                                                                                                       $label     Its label.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     */
    private static function labelled(array $container, string $label): array
    {
        $container['settings'][LeafPolicy::LABEL_KEY] = $label;

        return $container;
    }
}
