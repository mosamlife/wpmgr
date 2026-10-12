<?php

declare(strict_types=1);

namespace WPMgr\Agent\Abilities\Builders;

// Direct-file-access guard: keep above the docblock.
if (!defined('ABSPATH')) {
    exit;
}

/**
 * The literal values a designed page is built from, worked out from the
 * request's `design` object.
 *
 * Every value is a literal. A page never binds to a kit entry or a global, so
 * a compiler asks this class for the exact string or number to write.
 *
 * Colours are two kinds of string: an upper-case `#RRGGBB` for a design colour
 * or a darkened one, and `rgba(r,g,b,a)` for a colour at an alpha (no spaces,
 * the alpha written without trailing zeros). Pure black is not a colour of
 * this class: it is refused on the way in, so no output is ever `#000000` and
 * no tint is ever `rgba(0,0,0,...)`.
 *
 * Pure: no WordPress call, no I/O, no clock, and no float in a colour, so the
 * same design always gives the same bytes whatever the host's float settings.
 */
final class DesignTokens
{
    /** A design colour, as the input schema states it. */
    private const COLOUR_REGEX = '/' . OutlineV3Schema::COLOUR_PATTERN . '/D';

    /** Opacity of the ink overlay on a background image, by strength. */
    private const OVERLAY_OPACITY = ['soft' => 0.45, 'strong' => 0.65];

    /** The inverse tone writes its body text as the ground colour at this alpha. */
    private const INVERSE_BODY_ALPHA = 80;

    /**
     * Lightness is counted in 2550ths, which keeps the hover maths in whole
     * numbers: the lightness of a colour is 5 * (its brightest channel + its
     * darkest channel) of 2550. A hover colour is 8 points darker (204 of
     * 2550) and does not go below 4 points (102), so a dark colour never turns
     * pure black.
     */
    private const LIGHTNESS_FULL = 2550;

    private const HOVER_LIGHTNESS_STEP = 204;

    private const HOVER_LIGHTNESS_FLOOR = 102;

    /** The tight and the wide layer of a card shadow, each at an alpha of the ink colour. */
    private const CARD_SHADOW = [
        ['x' => 0, 'y' => 1, 'blur' => 2, 'spread' => 0, 'alpha' => 6],
        ['x' => 0, 'y' => 8, 'blur' => 24, 'spread' => 0, 'alpha' => 8],
    ];

    /** A hairline is this many pixels wide, in the muted colour at this alpha. */
    private const HAIRLINE_WIDTH = 1;

    private const HAIRLINE_ALPHA = 20;

    /**
     * The six design colours by role.
     *
     * @var array<string,string>
     */
    private array $colours;

    /**
     * @param array<string,string> $colours The six design colours by role, already checked.
     */
    private function __construct(array $colours)
    {
        $this->colours = $colours;
    }

    /**
     * The tokens of a design.
     *
     * @param array<mixed> $design The request's `design` object.
     * @return self
     * @throws \InvalidArgumentException When a colour role is missing, is not an upper-case `#RRGGBB`, or is `#000000`.
     */
    public static function fromDesign(array $design): self
    {
        $colors = $design['colors'] ?? null;
        if (!is_array($colors)) {
            throw new \InvalidArgumentException('a design has a colors object');
        }

        $colours = [];
        foreach (OutlineV3Schema::COLOUR_ROLES as $role) {
            $value = $colors[$role] ?? null;
            if (!is_string($value)) {
                throw new \InvalidArgumentException('a design has all six colour roles as strings');
            }
            self::channels($value);
            $colours[$role] = $value;
        }

        return new self($colours);
    }

    /**
     * The colours a section of one tone is drawn in.
     *
     *   ground   background ground,  headings ink,       body muted,         accent accent
     *   surface  background surface, headings ink,       body muted,         accent accent
     *   inverse  background ink,     headings ground,    body ground at 80%, accent accent
     *   accent   background accent,  headings on_accent, body on_accent,     accent on_accent
     *
     * "accent" is the colour of what stands out in the section: a link, an
     * eyebrow, the border of an outline button.
     *
     * @param string $tone One of ground, surface, inverse, accent.
     * @return array{background:string,heading:string,body:string,accent:string}
     * @throws \InvalidArgumentException When the tone is not one of the four.
     */
    public function tone(string $tone): array
    {
        $c = $this->colours;

        return match ($tone) {
            'ground'  => ['background' => $c['ground'], 'heading' => $c['ink'], 'body' => $c['muted'], 'accent' => $c['accent']],
            'surface' => ['background' => $c['surface'], 'heading' => $c['ink'], 'body' => $c['muted'], 'accent' => $c['accent']],
            'inverse' => [
                'background' => $c['ink'],
                'heading'    => $c['ground'],
                'body'       => self::alpha($c['ground'], self::INVERSE_BODY_ALPHA),
                'accent'     => $c['accent'],
            ],
            'accent'  => ['background' => $c['accent'], 'heading' => $c['on_accent'], 'body' => $c['on_accent'], 'accent' => $c['on_accent']],
            default   => throw new \InvalidArgumentException('a tone is ground, surface, inverse or accent'),
        };
    }

    /**
     * The overlay on a background image: the ink colour at 0.45 (soft) or 0.65
     * (strong) opacity. None gives null, no overlay.
     *
     * @param string $strength One of none, soft, strong.
     * @return array{color:string,opacity:float}|null
     * @throws \InvalidArgumentException When the strength is not one of the three.
     */
    public function overlay(string $strength): ?array
    {
        if ($strength === 'none') {
            return null;
        }
        if (!isset(self::OVERLAY_OPACITY[$strength])) {
            throw new \InvalidArgumentException('an overlay is none, soft or strong');
        }

        return ['color' => $this->colours['ink'], 'opacity' => self::OVERLAY_OPACITY[$strength]];
    }

    /**
     * The two layers of a card shadow, tight then wide, in the ink colour at 6%
     * and 8%. A shadow is tinted with ink, never pure black.
     *
     * @return list<array{x:int,y:int,blur:int,spread:int,color:string}>
     */
    public function cardShadow(): array
    {
        $layers = [];
        foreach (self::CARD_SHADOW as $layer) {
            $layers[] = [
                'x'      => $layer['x'],
                'y'      => $layer['y'],
                'blur'   => $layer['blur'],
                'spread' => $layer['spread'],
                'color'  => self::alpha($this->colours['ink'], $layer['alpha']),
            ];
        }

        return $layers;
    }

    /**
     * A hairline: one pixel, the muted colour at 20%.
     *
     * @return array{width:int,color:string}
     */
    public function hairline(): array
    {
        return ['width' => self::HAIRLINE_WIDTH, 'color' => self::alpha($this->colours['muted'], self::HAIRLINE_ALPHA)];
    }

    /**
     * The colour of a button on hover: the HSL lightness lowered by 8
     * percentage points, hue and saturation kept, each channel rounded half
     * up. Lightness stops at 4 points (about #0A0A0A), so the result is never
     * `#000000`; a colour already darker than that is returned unchanged.
     *
     * Whole-number maths. With S the sum of the brightest and the darkest
     * channel, the new lightness L (in 2550ths), K = min(L, 2550 - L) and D = S
     * when S is at most 255, else 510 - S, a channel v of the colour becomes
     * L + K * (2v - S) / D in 2550ths, which is v / 10 on the 0 to 255 scale.
     *
     * @param string $hex An upper-case `#RRGGBB` other than `#000000`.
     * @return string Upper-case `#RRGGBB`.
     * @throws \InvalidArgumentException When $hex is not such a colour.
     */
    public static function hover(string $hex): string
    {
        $channels  = self::channels($hex);
        $sum       = max($channels) + min($channels);
        $lightness = 5 * $sum;
        $target    = max(
            $lightness - self::HOVER_LIGHTNESS_STEP,
            min($lightness, self::HOVER_LIGHTNESS_FLOOR)
        );

        if (max($channels) === min($channels)) {
            $grey = intdiv(2 * $target + 10, 20);

            return sprintf('#%02X%02X%02X', $grey, $grey, $grey);
        }

        $span  = $sum <= 255 ? $sum : 510 - $sum;
        $reach = min($target, self::LIGHTNESS_FULL - $target);
        $out   = [];
        foreach ($channels as $value) {
            $scaled = $target * $span + $reach * (2 * $value - $sum);
            $out[]  = intdiv(2 * $scaled + 10 * $span, 20 * $span);
        }

        return sprintf('#%02X%02X%02X', $out[0], $out[1], $out[2]);
    }

    /**
     * A colour at an alpha, as `rgba(r,g,b,a)`. The alpha is written from the
     * integer percent, so the text does not depend on the host's float
     * settings: 80 is `0.8`, 6 is `0.06`, 100 is `1`, 0 is `0`.
     *
     * @param string $hex     An upper-case `#RRGGBB` other than `#000000`.
     * @param int    $percent Alpha from 0 to 100.
     * @return string
     * @throws \InvalidArgumentException When $hex is not such a colour or $percent is out of range.
     */
    public static function alpha(string $hex, int $percent): string
    {
        if ($percent < 0 || $percent > 100) {
            throw new \InvalidArgumentException('an alpha is 0 to 100 percent');
        }
        [$r, $g, $b] = self::channels($hex);

        return 'rgba(' . $r . ',' . $g . ',' . $b . ',' . self::percentText($percent) . ')';
    }

    /**
     * The decimal text of a percent: 80 is "0.8", 6 is "0.06", 100 is "1".
     *
     * @param int $percent 0 to 100.
     * @return string
     */
    private static function percentText(int $percent): string
    {
        if ($percent === 0) {
            return '0';
        }
        if ($percent === 100) {
            return '1';
        }

        return rtrim('0.' . str_pad((string) $percent, 2, '0', STR_PAD_LEFT), '0');
    }

    /**
     * The red, green and blue of a colour.
     *
     * @param string $hex An upper-case `#RRGGBB` other than `#000000`.
     * @return array{int,int,int}
     * @throws \InvalidArgumentException When $hex is not such a colour.
     */
    private static function channels(string $hex): array
    {
        if (preg_match(self::COLOUR_REGEX, $hex) !== 1 || $hex === '#000000') {
            throw new \InvalidArgumentException('a colour is an upper-case #RRGGBB other than #000000');
        }

        return [(int) hexdec(substr($hex, 1, 2)), (int) hexdec(substr($hex, 3, 2)), (int) hexdec(substr($hex, 5, 2))];
    }
}
