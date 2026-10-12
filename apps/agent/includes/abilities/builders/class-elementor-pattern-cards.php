<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

use WPMgr\Agent\Support\ArrayShape;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The patterns of a designed page that are built from icon boxes and cards:
 * features, as the icon-box widget over a closed list of icons, and pricing,
 * as one composed card per plan.
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
 * Pricing is a row of one card per plan, centred, each card of equal width
 * (the share of two cards when there is one plan). A plan card holds, top to
 * bottom, the plan's name (an h3 heading), its price (a heading in the h2 style
 * that is a div, so the price is not a heading of the page), its period (body
 * text, when it has one), a checklist (the icon-list widget, a check before each
 * feature) and its button. The highlighted plan has an accent border and a fill
 * button; the others have a hairline border and an outline button. Every plan
 * card casts the card shadow. The paid plugin's price table is never used.
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

    /** The icon in front of each feature of a plan, a name of ICONS. */
    private const CHECK_ICON = 'check';

    /** The size of the checklist's icons, in px as stored. */
    private const CHECK_ICON_PX = '18';

    /** The width of a highlighted plan's border, in px. */
    private const HIGHLIGHT_EDGE_PX = 2;

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
            'icon-list' => self::checklistKeys(),
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
     * The keys of the checklist: its colours and icon size, and, for each line a
     * plan may hold, the line's text, its icon and its repeater id. A repeater
     * item has no key of its own to list, so each position is listed by itself.
     *
     * @return array<string,string>
     */
    private static function checklistKeys(): array
    {
        $keys = ['icon_color' => 'colour', 'text_color' => 'colour', 'icon_size' => 'slider:px'];
        for ($i = 0; $i < OutlineV3Schema::MAX_PLAN_FEATURES; ++$i) {
            $keys['icon_list.' . $i . '.text']                  = 'html';
            $keys['icon_list.' . $i . '.selected_icon.value']   = 'enum:' . self::ICONS[self::CHECK_ICON];
            $keys['icon_list.' . $i . '.selected_icon.library'] = 'enum:' . self::ICON_LIBRARY;
            $keys['icon_list.' . $i . '._id']                   = 'enum';
        }

        return $keys;
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
     * Pricing: a centred row with one card per plan.
     *
     * @param IdSeed      $ids   Node ids for this request.
     * @param string      $path  The pattern's outline path.
     * @param list<mixed> $plans The plans: name, price, optional period, features, cta, optional highlighted.
     * @param string      $tone  The tone of the section it stands in.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When a plan holds a value the grammar does not accept.
     */
    public function pricing(IdSeed $ids, string $path, array $plans, string $tone): array
    {
        $rowId = $ids->next($path);
        // One plan takes the share of two, and the row centres it.
        $share = intdiv(100, max(count($plans), 2));
        $cards = [];
        foreach ($plans as $i => $plan) {
            $cards[] = $this->plan($ids, $path . '.plans[' . $i . ']', $plan, $share, $tone);
        }
        $row                                     = $this->sections->row($rowId, $cards);
        $row['settings']['flex_justify_content'] = 'center';

        return $row;
    }

    /**
     * One plan as a card.
     *
     * @param IdSeed $ids   Node ids for this request.
     * @param string $at    The plan's outline path.
     * @param mixed  $plan  The plan.
     * @param int    $share The card's share of the row in percent.
     * @param string $tone  The tone of the section it stands in.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,isInner:bool}
     * @throws \InvalidArgumentException When the plan holds a value the grammar does not accept.
     */
    private function plan(IdSeed $ids, string $at, mixed $plan, int $share, string $tone): array
    {
        $name        = self::text($plan, 'name');
        $price       = self::text($plan, 'price');
        $period      = is_array($plan) && array_key_exists('period', $plan) ? self::text($plan, 'period') : null;
        $features    = self::lines($plan);
        $cta         = is_array($plan) ? ($plan['cta'] ?? null) : null;
        $highlighted = is_array($plan) ? ($plan['highlighted'] ?? false) : false;
        if (!is_bool($highlighted)) {
            throw new \InvalidArgumentException('a plan is highlighted or not');
        }
        $colours  = $this->tokens->tone($tone);
        $elements = [
            $this->heading($ids->next($at . '.name'), $name, 3, 'h3', $tone),
            $this->heading($ids->next($at . '.price'), $price, 2, 'div', $tone),
        ];
        if ($period !== null) {
            $elements[] = [
                'id'         => $ids->next($at . '.period'),
                'elType'     => 'widget',
                'settings'   => ['editor' => '<p>' . LeafPolicy::htmlText($period) . '</p>'] + $this->look->textStyle('body', $tone, 'start'),
                'elements'   => [],
                'widgetType' => 'text-editor',
            ];
        }
        $elements[] = $this->checklist($ids, $at . '.features', $features, $colours);
        $elements[] = $this->look->buttonRow(
            $ids->next($at . '.cta#row'),
            [$this->button($ids->next($at . '.cta'), $cta, $highlighted ? 'fill' : 'outline', $tone)],
            'left'
        );
        $edge = $highlighted ? ['color' => $colours['accent'], 'width' => self::HIGHLIGHT_EDGE_PX] : $this->tokens->hairline();

        return $this->card($ids->next($at . '#card'), $share, $elements, $this->sections->cardSettings($edge['color'], $edge['width'], true));
    }

    /**
     * A heading of the type ramp in the section's tone, aligned to the start.
     *
     * @param string $id    The widget's id.
     * @param string $text  Its text.
     * @param int    $level The ramp's level: 2 for the h2 style, 3 for the h3 style.
     * @param string $tag   The HTML tag it is written as.
     * @param string $tone  The tone of the section it stands in.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,widgetType:string}
     */
    private function heading(string $id, string $text, int $level, string $tag, string $tone): array
    {
        return [
            'id'         => $id,
            'elType'     => 'widget',
            'settings'   => ['title' => LeafPolicy::htmlText($text), 'header_size' => $tag] + $this->look->headingStyle($level, $tone, 'start'),
            'elements'   => [],
            'widgetType' => 'heading',
        ];
    }

    /**
     * The icon-list widget of a plan's features, a check in front of each.
     *
     * @param IdSeed               $ids      Node ids for this request.
     * @param string               $at       The checklist's outline path, which keys its repeater ids.
     * @param list<string>         $features The feature lines.
     * @param array<string,string> $colours  The tone's colours.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,widgetType:string}
     */
    private function checklist(IdSeed $ids, string $at, array $features, array $colours): array
    {
        $items = [];
        foreach ($features as $k => $line) {
            $items[] = [
                'text'          => LeafPolicy::htmlText($line),
                'selected_icon' => self::icon(self::CHECK_ICON),
                '_id'           => $ids->item($at, 'icon_list', $k),
            ];
        }

        return [
            'id'         => $ids->next($at),
            'elType'     => 'widget',
            'settings'   => [
                'icon_list'  => $items,
                'icon_color' => $colours['accent'],
                'text_color' => $colours['body'],
                'icon_size'  => ['unit' => 'px', 'size' => self::CHECK_ICON_PX],
            ],
            'elements'   => [],
            'widgetType' => 'icon-list',
        ];
    }

    /**
     * A plan's button, as the leaf compiler writes a button.
     *
     * @param string $id    The widget's id.
     * @param mixed  $cta   The plan's button: text and url.
     * @param string $style fill or outline.
     * @param string $tone  The tone of the section it stands in.
     * @return array{id:string,elType:string,settings:array<string,mixed>,elements:list<array<string,mixed>>,widgetType:string}
     * @throws \InvalidArgumentException When the button holds a value the grammar does not accept, or a link with "&" that an Elementor page cannot keep.
     */
    private function button(string $id, mixed $cta, string $style, string $tone): array
    {
        $text = self::text($cta, 'text');
        $url  = self::text($cta, 'url');
        if (strpos($url, '&') !== false) {
            throw new \InvalidArgumentException('a link on an Elementor page cannot hold "&"');
        }

        return [
            'id'         => $id,
            'elType'     => 'widget',
            'settings'   => ['text' => LeafPolicy::htmlText($text), 'link' => ['url' => $url, 'is_external' => '', 'nofollow' => '']] + $this->look->buttonStyle($style, $tone),
            'elements'   => [],
            'widgetType' => 'button',
        ];
    }

    /**
     * A member of an item that is text.
     *
     * @param mixed  $item  The item.
     * @param string $field The member.
     * @return string
     * @throws \InvalidArgumentException When the member is not text that is not empty.
     */
    private static function text(mixed $item, string $field): string
    {
        $value = is_array($item) ? ($item[$field] ?? null) : null;
        if (!is_string($value) || $value === '') {
            throw new \InvalidArgumentException('a plan has its ' . $field . ' as text that is not empty');
        }

        return $value;
    }

    /**
     * The feature lines of a plan.
     *
     * @param mixed $plan The plan.
     * @return list<string>
     * @throws \InvalidArgumentException When they are not 1 to the grammar's most lines of text.
     */
    private static function lines(mixed $plan): array
    {
        $lines = is_array($plan) ? ($plan['features'] ?? null) : null;
        if (!is_array($lines) || !ArrayShape::isList($lines) || $lines === [] || count($lines) > OutlineV3Schema::MAX_PLAN_FEATURES) {
            throw new \InvalidArgumentException('a plan lists as many features as the grammar allows it');
        }
        foreach ($lines as $line) {
            if (!is_string($line) || $line === '') {
                throw new \InvalidArgumentException('a feature of a plan is text that is not empty');
            }
        }

        return $lines;
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
