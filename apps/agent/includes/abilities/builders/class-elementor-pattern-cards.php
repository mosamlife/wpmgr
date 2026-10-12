<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The patterns of a designed page that are built from icon boxes and cards:
 * features, as the icon-box widget over a closed list of icons.
 *
 *   split   the list of icon boxes cut in two columns of equal width, the left
 *           column the longer by one when the count is odd; the icon stands to
 *           the left of its text; the columns stack on a phone
 *   grid    cards of one share in rows of three, a row of two where a row of
 *           one would be left (4 items make 2 and 2, 7 make 3, 2 and 2); the
 *           icon stands above its text
 *   bento   cards of mixed widths in rows of two, 60 and 40 percent and then
 *           40 and 60, a last card alone in its row at the full width
 *
 * A card is a container with the design's card padding, a hairline border and
 * the design's radius (ElementorSectionCompiler::cardSettings()). An icon box
 * is drawn in the colours of the section's tone: the icon in the accent, the
 * title in the heading colour and the text in the body colour, with the title
 * set in the h3 style of the type ramp.
 *
 * The icons are NAMES, a closed list of OutlineV3Schema::ICONS, each one of the
 * icons Elementor ships in its solid Font Awesome library. There is no upload.
 *
 * The arguments come from ElementorPatternCompiler, which has judged the
 * pattern and its variant; an item outside what the grammar accepts is a
 * programming error and throws \InvalidArgumentException.
 *
 * Pure: no WordPress call.
 */
final class ElementorPatternCards
{
    /** The Elementor icon library every icon of the list is in. */
    public const ICON_LIBRARY = 'fa-solid';

    /**
     * The closed list of icons: WPMgr's name, then the icon of Elementor's
     * solid library it stands for. The names are OutlineV3Schema::ICONS, in
     * the same order.
     *
     * @var array<string,string>
     */
    public const ICONS = [
        'arrow-right' => 'fas fa-arrow-right',
        'award'       => 'fas fa-award',
        'bell'        => 'fas fa-bell',
        'bolt'        => 'fas fa-bolt',
        'book'        => 'fas fa-book',
        'briefcase'   => 'fas fa-briefcase',
        'calendar'    => 'fas fa-calendar',
        'chart'       => 'fas fa-chart-line',
        'check'       => 'fas fa-check',
        'clock'       => 'fas fa-clock',
        'cloud'       => 'fas fa-cloud',
        'code'        => 'fas fa-code',
        'cog'         => 'fas fa-cog',
        'comment'     => 'fas fa-comment',
        'compass'     => 'fas fa-compass',
        'credit-card' => 'fas fa-credit-card',
        'cube'        => 'fas fa-cube',
        'database'    => 'fas fa-database',
        'envelope'    => 'fas fa-envelope',
        'eye'         => 'fas fa-eye',
        'flag'        => 'fas fa-flag',
        'gift'        => 'fas fa-gift',
        'globe'       => 'fas fa-globe',
        'heart'       => 'fas fa-heart',
        'home'        => 'fas fa-home',
        'image'       => 'fas fa-image',
        'key'         => 'fas fa-key',
        'laptop'      => 'fas fa-laptop',
        'layers'      => 'fas fa-layer-group',
        'leaf'        => 'fas fa-leaf',
        'lightbulb'   => 'fas fa-lightbulb',
        'lock'        => 'fas fa-lock',
        'map-pin'     => 'fas fa-map-marker-alt',
        'phone'       => 'fas fa-phone',
        'rocket'      => 'fas fa-rocket',
        'search'      => 'fas fa-search',
        'shield'      => 'fas fa-shield-alt',
        'star'        => 'fas fa-star',
        'tag'         => 'fas fa-tag',
        'users'       => 'fas fa-users',
    ];

    /** The two shares of a bento row, in percent: the wide card and the narrow one. */
    private const WIDE_SHARE = 60;

    private const NARROW_SHARE = 40;

    /** Cards in a full row of the grid. */
    private const GRID_PER_ROW = 3;

    /** The font weights the type ramp writes. */
    private const WEIGHTS = '400|600|700';

    private DesignTokens $tokens;

    private ElementorSectionCompiler $sections;

    private ElementorLeafStyle $look;

    /**
     * @param DesignTokens             $tokens   The design's tokens.
     * @param ElementorSectionCompiler $sections The page's section compiler: its groups, rows, columns and card settings build the containers.
     */
    public function __construct(DesignTokens $tokens, ElementorSectionCompiler $sections)
    {
        $this->tokens   = $tokens;
        $this->sections = $sections;
        $this->look     = new ElementorLeafStyle($tokens);
    }

    /**
     * Every settings key these patterns write that the mapper's own list does
     * not carry, by element type, with the pin that holds its whole value. The
     * icon value is held to the closed list.
     *
     * @return array<string,array<string,string>>
     */
    public static function keys(): array
    {
        return [
            'icon-box'  => [
                'selected_icon.value'          => 'enum:' . implode('|', array_values(self::ICONS)),
                'selected_icon.library'        => 'enum:' . self::ICON_LIBRARY,
                'title_text'                   => 'html',
                'description_text'             => 'html',
                'position'                     => 'enum:left|top',
                'title_size'                   => 'enum:h3',
                'primary_color'                => 'colour',
                'title_color'                  => 'colour',
                'description_color'            => 'colour',
                'title_typography_typography'  => 'enum:custom',
                'title_typography_font_family' => 'enum',
                'title_typography_font_size'   => 'slider:px',
                'title_typography_font_weight' => 'enum:' . self::WEIGHTS,
                'title_typography_line_height' => 'slider:em',
            ],
            'container' => [
                'border_border'                      => 'enum:solid',
                'border_width'                       => 'dimensions',
                'border_color'                       => 'colour',
                'border_radius'                      => 'dimensions',
                'box_shadow_box_shadow_type'         => 'enum:yes',
                'box_shadow_box_shadow.horizontal'   => 'enum',
                'box_shadow_box_shadow.vertical'     => 'enum',
                'box_shadow_box_shadow.blur'         => 'enum',
                'box_shadow_box_shadow.spread'       => 'enum',
                'box_shadow_box_shadow.color'        => 'colour',
            ],
        ];
    }

    /**
     * The Elementor icon of a name of the closed list.
     *
     * @param string $name One of OutlineV3Schema::ICONS.
     * @return array{value:string,library:string}
     * @throws \InvalidArgumentException When the name is not on the list.
     */
    public static function icon(string $name): array
    {
        if (!isset(self::ICONS[$name])) {
            throw new \InvalidArgumentException('icon_unknown: an icon is a name from the closed list of icons');
        }

        return ['value' => self::ICONS[$name], 'library' => self::ICON_LIBRARY];
    }

    /**
     * Features: icon boxes in the layout of the variant.
     *
     * @param IdSeed                     $ids     Node ids for this request.
     * @param string                     $path    The pattern's outline path.
     * @param string                     $variant split, bento or grid.
     * @param list<mixed>                $items   The features items: icon, title, text.
     * @param string                     $tone    The tone of the section it stands in.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the variant is none of the three, or an item holds a value the grammar does not accept.
     */
    public function features(IdSeed $ids, string $path, string $variant, array $items, string $tone): array
    {
        $position = match ($variant) {
            'split'         => 'left',
            'bento', 'grid' => 'top',
            default         => throw new \InvalidArgumentException('a features pattern is split, bento or grid'),
        };
        $boxes = [];
        foreach ($items as $i => $item) {
            $boxes[] = $this->iconBox($ids->next($path . '.features[' . $i . ']'), $item, $position, $tone);
        }

        return $variant === 'split' ? $this->halves($ids, $path, $boxes) : $this->grid($ids, $path, $variant, $boxes);
    }

    /**
     * The icon boxes cut in two columns of equal width, the left one the longer
     * by one when the count is odd.
     *
     * @param IdSeed                    $ids   Node ids for this request.
     * @param string                    $path  The pattern's outline path.
     * @param list<array<string,mixed>> $boxes The icon boxes, in order.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     */
    private function halves(IdSeed $ids, string $path, array $boxes): array
    {
        $rowId = $ids->next($path);
        $left  = intdiv(count($boxes) + 1, 2);

        return $this->sections->row($rowId, [
            $this->sections->column($ids->next($path . '#left'), 50, array_slice($boxes, 0, $left)),
            $this->sections->column($ids->next($path . '#right'), 50, array_slice($boxes, $left)),
        ]);
    }

    /**
     * The icon boxes as cards in rows, in a group that spaces its rows with the
     * design's row gap.
     *
     * @param IdSeed                    $ids     Node ids for this request.
     * @param string                    $path    The pattern's outline path.
     * @param string                    $variant bento or grid.
     * @param list<array<string,mixed>> $boxes   The icon boxes, in order.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     */
    private function grid(IdSeed $ids, string $path, string $variant, array $boxes): array
    {
        $groupId = $ids->next($path);
        $edge    = $this->tokens->hairline();
        $chrome  = $this->sections->cardSettings($edge['color'], $edge['width'], false);
        $rows    = [];
        $at      = 0;
        foreach (self::rowShares($variant, count($boxes)) as $r => $shares) {
            $cards = [];
            foreach ($shares as $share) {
                $cards[] = $this->card($ids->next($path . '#card' . $at), $share, [$boxes[$at]], $chrome);
                ++$at;
            }
            // A card alone in its row stands straight in the group, which gives it the full width.
            $rows[] = $shares === [null] ? $cards[0] : $this->sections->row($ids->next($path . '#row' . $r), $cards);
        }
        $group             = $this->sections->group($groupId, $rows);
        $group['settings'] += $this->sections->gapSettings();

        return $group;
    }

    /**
     * The share of each card, in percent, for each row; null is a card alone in
     * its row.
     *
     * @param string $variant bento or grid.
     * @param int    $count   Cards.
     * @return list<list<int|null>>
     */
    private static function rowShares(string $variant, int $count): array
    {
        $rows = [];
        if ($variant === 'bento') {
            for ($wide = true, $left = $count; $left > 0; $left -= 2, $wide = !$wide) {
                if ($left === 1) {
                    $rows[] = [null];
                    break;
                }
                $rows[] = $wide ? [self::WIDE_SHARE, self::NARROW_SHARE] : [self::NARROW_SHARE, self::WIDE_SHARE];
            }

            return $rows;
        }
        $left = $count;
        while ($left > 0) {
            // Four cards make two rows of two; a row of one is never left.
            $take    = $left === 4 ? 2 : min(self::GRID_PER_ROW, $left);
            $rows[]  = array_fill(0, $take, intdiv(100, $take));
            $left   -= $take;
        }

        return $rows;
    }

    /**
     * One card: a column of the row, or a group when it stands alone, holding
     * its elements, with the card settings after the layout keys.
     *
     * @param string                    $id       The card's id.
     * @param int|null                  $share    Its share of the row in percent; null for a card alone in its row.
     * @param list<array<string,mixed>> $elements Its elements.
     * @param array<string,mixed>       $chrome   ElementorSectionCompiler::cardSettings().
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     */
    private function card(string $id, ?int $share, array $elements, array $chrome): array
    {
        $card              = $share === null ? $this->sections->group($id, $elements) : $this->sections->column($id, $share, $elements);
        $card['settings'] += $chrome;

        return $card;
    }

    /**
     * One icon box, in the key order Elementor returns.
     *
     * @param string $id       The widget's id.
     * @param mixed  $item     The features item: icon, title, text.
     * @param string $position left or top.
     * @param string $tone     The tone of the section it stands in.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,widgetType:string}
     * @throws \InvalidArgumentException When the item holds a value the grammar does not accept.
     */
    private function iconBox(string $id, mixed $item, string $position, string $tone): array
    {
        $icon  = is_array($item) ? ($item['icon'] ?? null) : null;
        $title = is_array($item) ? ($item['title'] ?? null) : null;
        $text  = is_array($item) ? ($item['text'] ?? null) : null;
        if (!is_string($icon)) {
            throw new \InvalidArgumentException('icon_unknown: an icon is a name from the closed list of icons');
        }
        if (!is_string($title) || $title === '' || !is_string($text) || $text === '') {
            throw new \InvalidArgumentException('an icon box has a title and a text');
        }
        $colours  = $this->tokens->tone($tone);
        $settings = [
            'selected_icon'     => self::icon($icon),
            'title_text'        => LeafPolicy::htmlText($title),
            'description_text'  => LeafPolicy::htmlText($text),
            'position'          => $position,
            'title_size'        => 'h3',
            'primary_color'     => $colours['accent'],
            'title_color'       => $colours['heading'],
            'description_color' => $colours['body'],
        ];
        // The title is set in the ramp's h3, written whole by the heading style and moved to the title's group.
        foreach ($this->look->headingStyle(3, $tone, 'start') as $key => $value) {
            if (strncmp($key, 'typography_', strlen('typography_')) === 0) {
                $settings['title_' . $key] = $value;
            }
        }

        return ['id' => $id, 'elType' => 'widget', 'settings' => $settings, 'elements' => [], 'widgetType' => 'icon-box'];
    }
}
