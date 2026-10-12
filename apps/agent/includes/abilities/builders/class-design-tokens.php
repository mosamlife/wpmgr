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
 * Sizes are whole pixels, spacing is whole pixels per breakpoint, and the
 * relative values (line height, letter spacing) are in em.
 *
 * Pure: no WordPress call, no I/O, no clock, and no float in a colour or a
 * spacing, so the same design always gives the same bytes whatever the host's
 * float settings.
 */
final class DesignTokens
{
    /** The text styles of a designed page, largest first. */
    public const STYLES = ['display', 'h2', 'h3', 'lead', 'body', 'eyebrow', 'button'];

    /** A design colour, as the input schema states it. */
    private const COLOUR_REGEX = '/' . OutlineV3Schema::COLOUR_PATTERN . '/D';

    /** A font family name, as the input schema states it. */
    private const FONT_REGEX = '/' . OutlineV3Schema::FONT_PATTERN . '/D';

    /** The family name that means: set no family, the theme's own applies. */
    private const SYSTEM_FONT = 'system';

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
     * What each text style is. `face` picks the design font. The size is the
     * base size times the ratio to the power `step`, or, when `percent` is set,
     * that percent of the base size. Line height and letter spacing are in em,
     * null when the style sets none.
     */
    private const TYPOGRAPHY = [
        'display' => ['face' => 'heading', 'weight' => 700, 'step' => 5, 'percent' => null, 'line_height' => 1.05, 'letter_spacing' => -0.02, 'transform' => null],
        'h2'      => ['face' => 'heading', 'weight' => 700, 'step' => 3, 'percent' => null, 'line_height' => 1.1, 'letter_spacing' => null, 'transform' => null],
        'h3'      => ['face' => 'heading', 'weight' => 600, 'step' => 2, 'percent' => null, 'line_height' => 1.15, 'letter_spacing' => null, 'transform' => null],
        'lead'    => ['face' => 'body', 'weight' => 400, 'step' => 1, 'percent' => null, 'line_height' => 1.5, 'letter_spacing' => null, 'transform' => null],
        'body'    => ['face' => 'body', 'weight' => 400, 'step' => 0, 'percent' => null, 'line_height' => 1.6, 'letter_spacing' => null, 'transform' => null],
        'eyebrow' => ['face' => 'body', 'weight' => 600, 'step' => 0, 'percent' => 80, 'line_height' => null, 'letter_spacing' => 0.08, 'transform' => 'uppercase'],
        'button'  => ['face' => 'body', 'weight' => 600, 'step' => 0, 'percent' => 95, 'line_height' => 1.0, 'letter_spacing' => null, 'transform' => null],
    ];

    /**
     * Spacing at the density `normal`, in pixels: [desktop, tablet, mobile].
     * Every value is a multiple of 4, so the 75% and 125% densities give
     * whole pixels.
     */
    private const SECTION_PADDING = [
        'md' => [64, 56, 40],
        'lg' => [96, 72, 56],
        'xl' => [128, 96, 72],
    ];

    private const ROW_GAP = [32, 24, 20];

    private const CARD_PADDING = [32, 28, 24];

    /** The density as a percent of the `normal` spacing. */
    private const DENSITY_PERCENT = ['compact' => 75, 'normal' => 100, 'airy' => 125];

    /** The corner radius in pixels, by name. */
    private const RADIUS_PX = ['none' => 0, 'sm' => 6, 'md' => 12, 'lg' => 20];

    /**
     * The six design colours by role.
     *
     * @var array<string,string>
     */
    private array $colours;

    /**
     * The heading and body family; null for `system`.
     *
     * @var array{heading:?string,body:?string}
     */
    private array $families;

    private int $basePx;

    private float $ratio;

    private string $radius;

    private string $density;

    /**
     * @param array<string,string>                $colours  The six design colours by role, already checked.
     * @param array{heading:?string,body:?string} $families The heading and body family, already checked.
     * @param int                                 $basePx   Body size in px, already checked.
     * @param float                               $ratio    Scale ratio, already checked.
     * @param string                              $radius   A name of RADIUS_PX, already checked.
     * @param string                              $density  A name of DENSITY_PERCENT, already checked.
     */
    private function __construct(array $colours, array $families, int $basePx, float $ratio, string $radius, string $density)
    {
        $this->colours  = $colours;
        $this->families = $families;
        $this->basePx   = $basePx;
        $this->ratio    = $ratio;
        $this->radius   = $radius;
        $this->density  = $density;
    }

    /**
     * The tokens of a design.
     *
     * @param array<mixed> $design The request's `design` object.
     * @return self
     * @throws \InvalidArgumentException When a part of the design is missing or outside what the input schema allows.
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

        $fonts = $design['fonts'] ?? null;
        if (!is_array($fonts)) {
            throw new \InvalidArgumentException('a design has a fonts object');
        }
        $families = ['heading' => self::familyOf($fonts['heading'] ?? null), 'body' => self::familyOf($fonts['body'] ?? null)];

        $scale = $design['scale'] ?? null;
        if (!is_array($scale)) {
            throw new \InvalidArgumentException('a design has a scale object');
        }
        $basePx = self::wholeNumber($scale['base_px'] ?? null);
        if ($basePx === null || $basePx < OutlineV3Schema::SCALE_BASE_PX[0] || $basePx > OutlineV3Schema::SCALE_BASE_PX[1]) {
            throw new \InvalidArgumentException('a design base size is a whole number from 15 to 20');
        }
        $ratio = $scale['ratio'] ?? null;
        if ((!is_int($ratio) && !is_float($ratio)) || !($ratio >= OutlineV3Schema::SCALE_RATIO[0] && $ratio <= OutlineV3Schema::SCALE_RATIO[1])) {
            throw new \InvalidArgumentException('a design ratio is a number from 1.125 to 1.333');
        }

        $radius = $design['radius'] ?? null;
        if (!is_string($radius) || !in_array($radius, OutlineV3Schema::RADII, true)) {
            throw new \InvalidArgumentException('a design radius is none, sm, md or lg');
        }
        $density = $design['density'] ?? null;
        if (!is_string($density) || !in_array($density, OutlineV3Schema::DENSITIES, true)) {
            throw new \InvalidArgumentException('a design density is compact, normal or airy');
        }

        return new self($colours, $families, $basePx, (float) $ratio, $radius, $density);
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
     * One text style as literals.
     *
     *   display  heading font, 700, base x ratio^5, line height 1.05, letter spacing -0.02
     *   h2       heading font, 700, base x ratio^3, line height 1.1
     *   h3       heading font, 600, base x ratio^2, line height 1.15
     *   lead     body font,    400, base x ratio,   line height 1.5
     *   body     body font,    400, base,           line height 1.6
     *   eyebrow  body font,    600, base x 0.8,     letter spacing 0.08, uppercase
     *   button   body font,    600, base x 0.95,    line height 1
     *
     * The size is in whole pixels, a half rounded up. The family is null for
     * the design font `system`, which means set no family. Line height and
     * letter spacing are in em, null when the style sets none.
     *
     * @param string $style One of STYLES.
     * @return array{family:?string,size:int,weight:int,line_height:?float,letter_spacing:?float,transform:?string}
     * @throws \InvalidArgumentException When the style is not one of STYLES.
     */
    public function typography(string $style): array
    {
        $rule = self::TYPOGRAPHY[$style] ?? null;
        if ($rule === null) {
            throw new \InvalidArgumentException('a text style is display, h2, h3, lead, body, eyebrow or button');
        }

        $size = $rule['percent'] !== null
            ? intdiv($this->basePx * $rule['percent'] + 50, 100)
            : self::rung($this->basePx, $this->ratio, $rule['step']);

        return [
            'family'         => $this->families[$rule['face']],
            'size'           => $size,
            'weight'         => $rule['weight'],
            'line_height'    => $rule['line_height'],
            'letter_spacing' => $rule['letter_spacing'],
            'transform'      => $rule['transform'],
        ];
    }

    /**
     * Top and bottom padding of a section, in pixels per breakpoint, for the
     * design's density: md 64/56/40, lg 96/72/56, xl 128/96/72 at `normal`,
     * 75% at `compact`, 125% at `airy`.
     *
     * @param string $spacing One of md, lg, xl.
     * @return array{desktop:int,tablet:int,mobile:int}
     * @throws \InvalidArgumentException When the spacing is not one of the three.
     */
    public function sectionPadding(string $spacing): array
    {
        if (!isset(self::SECTION_PADDING[$spacing])) {
            throw new \InvalidArgumentException('a spacing is md, lg or xl');
        }

        return $this->perBreakpoint(self::SECTION_PADDING[$spacing]);
    }

    /**
     * The gap between the children of a row, in pixels per breakpoint: 32/24/20
     * at `normal`, scaled by the density.
     *
     * @return array{desktop:int,tablet:int,mobile:int}
     */
    public function rowGap(): array
    {
        return $this->perBreakpoint(self::ROW_GAP);
    }

    /**
     * The padding inside a card, in pixels per breakpoint: 32/28/24 at
     * `normal`, scaled by the density.
     *
     * @return array{desktop:int,tablet:int,mobile:int}
     */
    public function cardPadding(): array
    {
        return $this->perBreakpoint(self::CARD_PADDING);
    }

    /**
     * The corner radius in pixels: none 0, sm 6, md 12, lg 20.
     *
     * @return int
     */
    public function radius(): int
    {
        return self::RADIUS_PX[$this->radius];
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
     * A spacing at the design's density, per breakpoint.
     *
     * @param array{int,int,int} $normal The spacing at `normal`: desktop, tablet, mobile.
     * @return array{desktop:int,tablet:int,mobile:int}
     */
    private function perBreakpoint(array $normal): array
    {
        $percent = self::DENSITY_PERCENT[$this->density];

        return [
            'desktop' => intdiv($normal[0] * $percent, 100),
            'tablet'  => intdiv($normal[1] * $percent, 100),
            'mobile'  => intdiv($normal[2] * $percent, 100),
        ];
    }

    /**
     * The base size times the ratio, $steps times, in whole pixels (a half
     * rounds up). Repeated multiplication, so the result does not depend on
     * the maths library of the host.
     *
     * @param int   $base  Base size in px.
     * @param float $ratio Scale ratio.
     * @param int   $steps Steps up the scale, 0 or more.
     * @return int
     */
    private static function rung(int $base, float $ratio, int $steps): int
    {
        $size = (float) $base;
        for ($i = 0; $i < $steps; $i++) {
            $size *= $ratio;
        }

        return (int) floor($size + 0.5);
    }

    /**
     * The family a design font name sets: the name, or null for `system`.
     *
     * @param mixed $name The font name from the design.
     * @return string|null
     * @throws \InvalidArgumentException When the name is not 1 to 60 letters, digits and spaces.
     */
    private static function familyOf(mixed $name): ?string
    {
        if (!is_string($name) || preg_match(self::FONT_REGEX, $name) !== 1) {
            throw new \InvalidArgumentException('a design font is 1 to 60 letters, digits and spaces');
        }

        return $name === self::SYSTEM_FONT ? null : $name;
    }

    /**
     * An integer, or a float that is a whole number (JSON may write 17.0), as
     * an integer; anything else gives null.
     *
     * @param mixed $value The value to read.
     * @return int|null
     */
    private static function wholeNumber(mixed $value): ?int
    {
        if (is_int($value)) {
            return $value;
        }
        if (is_float($value) && is_finite($value) && abs($value) <= 1000000 && floor($value) === $value) {
            return (int) $value;
        }

        return null;
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
