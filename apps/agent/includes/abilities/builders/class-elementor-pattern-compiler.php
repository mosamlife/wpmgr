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
 *   testimonials featured
 *                    a group of the design's row gap: the first testimonial
 *                    widget, the featured one, across the full width, then the
 *                    others in rows of SMALLER_PER_ROW equal columns, one
 *                    testimonial widget in each; a row of one column is
 *                    centred, and the rows stack on a phone. The widgets stand
 *                    in the outline's order, so the k-th testimonial widget in
 *                    document order is item k
 *   faq accordion    a group holding one accordion widget, FAQ schema on, its
 *                    k-th tab the k-th question and answer; each tab has a
 *                    repeater id of its own
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

    /** The testimonials, after the featured one, that stand in one row. */
    public const SMALLER_PER_ROW = 2;

    /** The refusal of a logo whose alt text is not the library's. */
    public const CODE_ALT_FROM_LIBRARY = 'image_alt_from_library';

    /** What the fix for that refusal is, in one line. */
    private const REASON_ALT = 'must be the alt text saved with the image in the media library, exactly; set it to that text, or change it in the media library first';

    /**
     * Every settings key a pattern writes that the mapper's own list does not
     * carry, by element type, with the pin that holds its whole value: a
     * counter, a testimonial, an accordion, the rows and columns of a pattern
     * and a wrapping row. The members of a list control are LISTS'.
     *
     * @var array<string,array<string,string>>
     */
    public const KEYS = [
        'accordion'   => [
            'faq_schema'       => 'enum:yes',
            'title_color'      => 'colour',
            'tab_active_color' => 'colour',
            'content_color'    => 'colour',
        ],
        'testimonial' => [
            'testimonial_content'        => 'html',
            'testimonial_image'          => 'image',
            'testimonial_name'           => 'html',
            'testimonial_job'            => 'html',
            'testimonial_image_position' => 'enum:aside',
            'testimonial_alignment'      => 'enum:left',
            'content_content_color'      => 'colour',
            'name_text_color'            => 'colour',
            'job_text_color'             => 'colour',
        ],
        'counter'   => [
            'starting_number' => 'enum:0',
            'ending_number'   => 'counter-number',
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

    /**
     * The list controls a pattern writes, by element type: the control, the
     * members of one item with their pins, and the most items it holds.
     * LeafPolicy lists a path for each position of a list, so allowedKeys()
     * lists "<control>.<position>.<member>" for every position below that most
     * and for no other.
     *
     * @var array<string,array<string,array{0:array<string,string>,1:int}>>
     */
    public const LISTS = [
        'accordion' => [
            'tabs' => [['tab_title' => 'html', 'tab_content' => 'html', '_id' => 'repeater-id'], OutlineV3Schema::ITEM_CAPS['faq'][1]],
        ],
    ];

    private DesignTokens $tokens;

    private ElementorSectionCompiler $sections;

    private ElementorPatternCards $cards;

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
        $this->cards    = new ElementorPatternCards($tokens, $sections);
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
        foreach ([self::KEYS, self::listKeys(), ElementorPatternCards::keys()] as $keys) {
            foreach ($keys as $type => $paths) {
                $allowed[$type] = ($allowed[$type] ?? []) + $paths;
            }
        }

        return $allowed;
    }

    /**
     * The paths of LISTS' controls, a path for each position of each list.
     *
     * @return array<string,array<string,string>> Element type, then settings path, then pin.
     */
    private static function listKeys(): array
    {
        $keys = [];
        foreach (self::LISTS as $type => $lists) {
            foreach ($lists as $control => [$members, $most]) {
                for ($position = 0; $position < $most; $position++) {
                    foreach ($members as $member => $pin) {
                        $keys[$type][$control . '.' . $position . '.' . $member] = $pin;
                    }
                }
            }
        }

        return $keys;
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
            'hero'         => $this->hero($ids, $path, $pattern, $blocks),
            'cta'          => $this->cta($ids, $path, $pattern, $blocks),
            'stats'        => $this->stats($ids, $path, $pattern, $tone),
            'logos'        => $this->logos($ids, $path, $pattern),
            'features'     => $this->features($ids, $path, $pattern, $tone),
            'pricing'      => $this->pricing($ids, $path, $pattern, $tone),
            'testimonials' => $this->testimonials($ids, $path, $pattern, $tone),
            'faq'          => $this->faq($ids, $path, $pattern, $tone),
            default        => throw new \InvalidArgumentException('this pattern is not one the compiler builds'),
        };
    }

    /**
     * Features: icon boxes from the closed list of icons, as a two-column list
     * (split) or as cards in rows (bento, grid). See ElementorPatternCards.
     *
     * @param IdSeed              $ids     Node ids for this request.
     * @param string              $path    The pattern's outline path.
     * @param array<string,mixed> $pattern The features node.
     * @param string              $tone    The tone of the section it stands in.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the node is not a features pattern the grammar accepts.
     */
    public function features(IdSeed $ids, string $path, array $pattern, string $tone): array
    {
        $variant = self::variantOf($pattern, 'features');
        $label   = self::label('features', $variant);

        return self::labelled($this->cards->features($ids, $path, $variant, self::itemsOf($pattern, 'features'), $tone), $label);
    }

    /**
     * Pricing: one composed card per plan, never the paid plugin's price table.
     * See ElementorPatternCards.
     *
     * @param IdSeed              $ids     Node ids for this request.
     * @param string              $path    The pattern's outline path.
     * @param array<string,mixed> $pattern The pricing node.
     * @param string              $tone    The tone of the section it stands in.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the node is not a pricing pattern the grammar accepts.
     */
    public function pricing(IdSeed $ids, string $path, array $pattern, string $tone): array
    {
        $label = self::label('pricing', self::variantOf($pattern, 'pricing'));

        return self::labelled($this->cards->pricing($ids, $path, self::itemsOf($pattern, 'pricing'), $tone), $label);
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
     * @param string       $at      The outline's path in the input.
     * @return array{code:string,detail:string,failures:list<array<string,mixed>>}|null
     */
    public static function logoAltRefusal(mixed $outline, array $media, string $at = 'outline'): ?array
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
                        $failures[] = ['path' => $at . '[' . $i . '].children[' . $j . '].logos[' . $k . ']', 'key' => 'alt', 'reason' => self::REASON_ALT];
                    }
                }
            }
        }

        return $failures === [] ? null : Refusal::make(self::CODE_ALT_FROM_LIBRARY, 'A logo has alt text that is not the one saved with its image in the media library.', $failures);
    }

    /**
     * Testimonials, featured: the first across the full width, the others
     * under it in rows of SMALLER_PER_ROW equal columns.
     *
     * Each testimonial is one testimonial widget, a picture beside the text
     * when the item names one. The quote is in the tone's heading colour, the
     * name in its accent colour and the role in its body colour. A row left
     * with one column is centred.
     *
     * @param IdSeed              $ids     Node ids for this request.
     * @param string              $path    The pattern's outline path.
     * @param array<string,mixed> $pattern The testimonials node.
     * @param string              $tone    The tone of the section it stands in.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the node is not a testimonials pattern the grammar accepts, or names a picture the site did not resolve.
     */
    public function testimonials(IdSeed $ids, string $path, array $pattern, string $tone): array
    {
        $label   = self::label('testimonials', self::variantOf($pattern, 'testimonials'));
        $items   = self::itemsOf($pattern, 'testimonials');
        $colours = $this->tokens->tone($tone);
        $id      = $ids->next($path);
        $at      = static fn (int $i): string => $path . '.testimonials[' . $i . ']';
        $parts   = [$this->testimonial($ids->next($at(0)), $items[0], $colours)];
        $width   = intdiv(100, self::SMALLER_PER_ROW);
        foreach (array_chunk(array_slice($items, 1, null, true), self::SMALLER_PER_ROW, true) as $r => $chunk) {
            $columns = [];
            foreach ($chunk as $i => $item) {
                $columnId  = $ids->next($at($i) . '#column');
                $columns[] = $this->sections->column($columnId, $width, [$this->testimonial($ids->next($at($i)), $item, $colours)]);
            }
            $row = $this->sections->row($ids->next($path . '#row' . $r), $columns);
            if (count($columns) === 1) {
                $row['settings']['flex_justify_content'] = 'center';
            }
            $parts[] = $row;
        }
        $group = $this->sections->group($id, $parts);
        $group['settings'] += $this->sections->gapSettings();

        return self::labelled($group, $label);
    }

    /**
     * Faq: one accordion with FAQ schema on, a tab for each question.
     *
     * The accordion is the classic one, which every Elementor release the
     * round trip runs registers. A tab is the question as its title and the
     * answer as one paragraph; each has a repeater id from the IdSeed. The
     * titles are in the tone's heading colour, the open one in its accent
     * colour and the answers in its body colour.
     *
     * @param IdSeed              $ids     Node ids for this request.
     * @param string              $path    The pattern's outline path.
     * @param array<string,mixed> $pattern The faq node.
     * @param string              $tone    The tone of the section it stands in.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the node is not a faq pattern the grammar accepts.
     */
    public function faq(IdSeed $ids, string $path, array $pattern, string $tone): array
    {
        $label   = self::label('faq', self::variantOf($pattern, 'faq'));
        $items   = self::itemsOf($pattern, 'faq');
        $colours = $this->tokens->tone($tone);
        $id      = $ids->next($path);
        $at      = $path . '.faq';
        $tabs    = [];
        foreach ($items as $i => $item) {
            $tabs[] = self::tab($ids->item($at, 'tabs', $i), $item);
        }
        $settings  = [
            'tabs'             => $tabs,
            'faq_schema'       => 'yes',
            'title_color'      => $colours['heading'],
            'tab_active_color' => $colours['accent'],
            'content_color'    => $colours['body'],
        ];
        $accordion = ['id' => $ids->next($at), 'elType' => 'widget', 'settings' => $settings, 'elements' => [], 'widgetType' => 'accordion'];

        return $this->stack($id, $label, [$accordion]);
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
        $settings = [
            'image'      => $this->libraryImage(is_array($item) ? ($item['attachment_id'] ?? null) : null),
            'image_size' => 'medium',
            'align'      => 'center',
            'width'      => ['unit' => 'px', 'size' => (string) self::LOGO_WIDTH_PX],
        ];

        return ['id' => $id, 'elType' => 'widget', 'settings' => $settings, 'elements' => [], 'widgetType' => 'image'];
    }

    /**
     * An image of the media library as a media control stores it.
     *
     * @param mixed $attachment The attachment id the outline names.
     * @return array{url:string,id:string,source:string}
     * @throws \InvalidArgumentException When it is not a whole number the grammar accepts, or names an image the site did not resolve before the page is compiled.
     */
    private function libraryImage(mixed $attachment): array
    {
        if (!is_int($attachment) || $attachment < 1 || $attachment > PageCreateBuilder::MAX_ATTACHMENT_ID) {
            throw new \InvalidArgumentException('an image is named by a whole number');
        }
        $fact = $this->media[$attachment] ?? null;
        $url  = is_array($fact) ? ($fact['url'] ?? null) : null;
        if (!is_array($fact) || ($fact['id'] ?? null) !== $attachment || !is_string($url) || PageCreateBuilder::imageUrlProblem($url) !== null
            || !in_array($fact['mime'] ?? null, PageCreateBuilder::IMAGE_MIMES, true)) {
            throw new \InvalidArgumentException('an image is one the site resolved before the page is compiled');
        }

        return ['url' => $url, 'id' => (string) $attachment, 'source' => 'library'];
    }

    /**
     * One tab of an accordion, in the key order Elementor returns.
     *
     * @param string $id   The tab's repeater id.
     * @param mixed  $item The faq item: question and answer.
     * @return array{tab_title:string,tab_content:string,_id:string}
     * @throws \InvalidArgumentException When the item holds a value the grammar does not accept.
     */
    private static function tab(string $id, mixed $item): array
    {
        $question = is_array($item) ? ($item['question'] ?? null) : null;
        $answer   = is_array($item) ? ($item['answer'] ?? null) : null;
        if (!is_string($question) || $question === '' || !is_string($answer) || $answer === '') {
            throw new \InvalidArgumentException('a question has a title and an answer of text');
        }

        return ['tab_title' => LeafPolicy::htmlText($question), 'tab_content' => '<p>' . LeafPolicy::htmlText($answer) . '</p>', '_id' => $id];
    }

    /**
     * One testimonial, in the key order Elementor returns. The picture and the
     * role are written only when the item has them.
     *
     * @param string               $id      The widget's id.
     * @param mixed                $item    The testimonials item: quote, name, optional role and optional attachment_id.
     * @param array<string,string> $colours The tone's colours.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,widgetType:string}
     * @throws \InvalidArgumentException When the item holds a value the grammar does not accept, or names a picture the site did not resolve.
     */
    private function testimonial(string $id, mixed $item, array $colours): array
    {
        $quote = is_array($item) ? ($item['quote'] ?? null) : null;
        $name  = is_array($item) ? ($item['name'] ?? null) : null;
        $role  = is_array($item) ? ($item['role'] ?? null) : null;
        if (!is_array($item) || !is_string($quote) || $quote === '' || !is_string($name) || $name === '') {
            throw new \InvalidArgumentException('a testimonial has a quote and a name of text');
        }
        if (array_key_exists('role', $item) && (!is_string($role) || $role === '')) {
            throw new \InvalidArgumentException('a testimonial role, when it has one, is text');
        }
        $settings = ['testimonial_content' => LeafPolicy::htmlText($quote)];
        if (array_key_exists('attachment_id', $item)) {
            $settings['testimonial_image'] = $this->libraryImage($item['attachment_id']);
        }
        $settings['testimonial_name'] = LeafPolicy::htmlText($name);
        if ($role !== null) {
            $settings['testimonial_job'] = LeafPolicy::htmlText($role);
        }
        $settings['testimonial_image_position'] = 'aside';
        $settings['testimonial_alignment']      = 'left';
        $settings['content_content_color']      = $colours['heading'];
        $settings['name_text_color']            = $colours['accent'];
        $settings['job_text_color']             = $colours['body'];

        return ['id' => $id, 'elType' => 'widget', 'settings' => $settings, 'elements' => [], 'widgetType' => 'testimonial'];
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
