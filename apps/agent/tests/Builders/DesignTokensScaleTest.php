<?php
/**
 * DesignTokens scale: the type ramp (size, weight, line-height as literals),
 * section padding, row gap and card padding by density, and the radius.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\DesignTokens;
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\DesignTokens
 */
final class DesignTokensScaleTest extends TestCase
{
    /** Section padding [desktop, tablet, mobile] by density and spacing. */
    private const SECTION_PADDING = [
        'compact' => ['md' => [48, 42, 30], 'lg' => [72, 54, 42], 'xl' => [96, 72, 54]],
        'normal'  => ['md' => [64, 56, 40], 'lg' => [96, 72, 56], 'xl' => [128, 96, 72]],
        'airy'    => ['md' => [80, 70, 50], 'lg' => [120, 90, 70], 'xl' => [160, 120, 90]],
    ];

    private const ROW_GAP = ['compact' => [24, 18, 15], 'normal' => [32, 24, 20], 'airy' => [40, 30, 25]];

    private const CARD_PADDING = ['compact' => [24, 21, 18], 'normal' => [32, 28, 24], 'airy' => [40, 35, 30]];

    /**
     * A valid design with the values this test varies.
     *
     * @param array<string,mixed> $override Top-level keys to replace.
     * @return array<string,mixed>
     */
    private static function design(array $override = []): array
    {
        return array_replace([
            'colors'  => [
                'ground'    => '#F6F5F2',
                'surface'   => '#FFFFFF',
                'ink'       => '#15171C',
                'muted'     => '#5A606C',
                'accent'    => '#0D7A63',
                'on_accent' => '#FFFFFF',
            ],
            'fonts'   => ['heading' => 'Manrope', 'body' => 'system'],
            'scale'   => ['base_px' => 17, 'ratio' => 'major_third'],
            'radius'  => 'md',
            'density' => 'normal',
        ], $override);
    }

    /**
     * @param mixed $base  Base size in px.
     * @param mixed $ratio Scale ratio: a name of DesignTokens::RATIOS.
     * @return array<string,mixed>
     */
    private static function withScale($base, $ratio): array
    {
        return self::design(['scale' => ['base_px' => $base, 'ratio' => $ratio]]);
    }

    public function test_type_ramp_of_the_shared_design(): void
    {
        $tokens = DesignTokens::fromDesign(self::design());

        $expected = [
            'display' => ['family' => 'Manrope', 'size' => 52, 'weight' => 700, 'line_height' => 1.05, 'letter_spacing' => -0.02, 'transform' => null],
            'h2'      => ['family' => 'Manrope', 'size' => 33, 'weight' => 700, 'line_height' => 1.1, 'letter_spacing' => null, 'transform' => null],
            'h3'      => ['family' => 'Manrope', 'size' => 27, 'weight' => 600, 'line_height' => 1.15, 'letter_spacing' => null, 'transform' => null],
            'lead'    => ['family' => null, 'size' => 21, 'weight' => 400, 'line_height' => 1.5, 'letter_spacing' => null, 'transform' => null],
            'body'    => ['family' => null, 'size' => 17, 'weight' => 400, 'line_height' => 1.6, 'letter_spacing' => null, 'transform' => null],
            'eyebrow' => ['family' => null, 'size' => 14, 'weight' => 600, 'line_height' => null, 'letter_spacing' => 0.08, 'transform' => 'uppercase'],
            'button'  => ['family' => null, 'size' => 16, 'weight' => 600, 'line_height' => 1.0, 'letter_spacing' => null, 'transform' => null],
        ];

        $this->assertSame(array_keys($expected), DesignTokens::STYLES);
        foreach ($expected as $style => $literal) {
            $this->assertSame($literal, $tokens->typography($style), $style);
        }
    }

    public function test_sizes_are_base_times_ratio_to_the_step_rounded_to_whole_pixels(): void
    {
        // Expected values come from exact fraction arithmetic, none of them near a .5 tie.
        $cases = [
            [17, 'major_third', ['display' => 52, 'h2' => 33, 'h3' => 27, 'lead' => 21, 'body' => 17, 'eyebrow' => 14, 'button' => 16]],
            [16, 'minor_third', ['display' => 40, 'h2' => 28, 'h3' => 23, 'lead' => 19, 'body' => 16, 'eyebrow' => 13, 'button' => 15]],
            [18, 'perfect_fourth', ['display' => 76, 'h2' => 43, 'h3' => 32, 'lead' => 24, 'body' => 18, 'eyebrow' => 14, 'button' => 17]],
            [15, 'major_second', ['display' => 27, 'h2' => 21, 'h3' => 19, 'lead' => 17, 'body' => 15, 'eyebrow' => 12, 'button' => 14]],
            [20, 'perfect_fourth', ['display' => 84, 'h2' => 47, 'h3' => 36, 'lead' => 27, 'body' => 20, 'eyebrow' => 16, 'button' => 19]],
            [19, 'minor_third', ['display' => 47, 'h2' => 33, 'h3' => 27, 'lead' => 23, 'body' => 19, 'eyebrow' => 15, 'button' => 18]],
            [17, 'major_second', ['display' => 31, 'h2' => 24, 'h3' => 22, 'lead' => 19, 'body' => 17, 'eyebrow' => 14, 'button' => 16]],
            [16, 'major_third', ['display' => 49, 'h2' => 31, 'h3' => 25, 'lead' => 20, 'body' => 16, 'eyebrow' => 13, 'button' => 15]],
        ];
        foreach ($cases as [$base, $ratio, $sizes]) {
            $tokens = DesignTokens::fromDesign(self::withScale($base, $ratio));
            foreach ($sizes as $style => $size) {
                $this->assertSame($size, $tokens->typography($style)['size'], $base . ' x ' . $ratio . ' ' . $style);
            }
        }
    }

    public function test_a_ratio_name_stands_for_one_number_and_the_schema_offers_those_names(): void
    {
        // The input carries the name only; the number lives here and nowhere in the input.
        $this->assertSame(
            ['major_second' => 1.125, 'minor_third' => 1.2, 'major_third' => 1.25, 'perfect_fourth' => 1.333],
            DesignTokens::RATIOS
        );
        $this->assertSame(array_keys(DesignTokens::RATIOS), OutlineV3Schema::SCALE_RATIOS);

        $ratio = OutlineV3Schema::designSchema()['properties']['scale']['properties']['ratio'];
        $this->assertSame(['type' => 'string', 'enum' => ['major_second', 'minor_third', 'major_third', 'perfect_fourth']], $ratio);

        // Not one number-typed field in the design object: the signed run carries integers only.
        $numbers = [];
        $walk    = static function ($node, string $at) use (&$walk, &$numbers): void {
            if (!is_array($node)) {
                return;
            }
            if (($node['type'] ?? null) === 'number') {
                $numbers[] = $at;
            }
            foreach ($node as $key => $child) {
                $walk($child, $at . '/' . $key);
            }
        };
        $walk(OutlineV3Schema::designSchema(), 'design');
        $this->assertSame([], $numbers, 'the design schema holds no number-typed field');
    }

    public function test_each_ratio_name_is_accepted_and_sets_its_own_ramp(): void
    {
        // Base 20: display = 20 x ratio^5, rounded. The four names give four different ramps.
        $display = [];
        foreach (array_keys(DesignTokens::RATIOS) as $name) {
            $display[$name] = DesignTokens::fromDesign(self::withScale(20, $name))->typography('display')['size'];
        }
        $this->assertSame(['major_second' => 36, 'minor_third' => 50, 'major_third' => 61, 'perfect_fourth' => 84], $display);
    }

    public function test_a_half_pixel_rounds_up_and_a_whole_valued_float_base_is_a_whole_base(): void
    {
        // 18 x 1.25 = 22.5 exactly, so the lead is 23; 17.0 is the whole number 17 as JSON writes it.
        $this->assertSame(23, DesignTokens::fromDesign(self::withScale(18, 'major_third'))->typography('lead')['size']);
        $this->assertSame(17, DesignTokens::fromDesign(self::withScale(17.0, 'major_third'))->typography('body')['size']);
    }

    public function test_family_follows_the_design_and_system_leaves_it_unset(): void
    {
        $named = DesignTokens::fromDesign(self::design(['fonts' => ['heading' => 'Playfair Display', 'body' => 'DM Sans']]));
        foreach (['display', 'h2', 'h3'] as $style) {
            $this->assertSame('Playfair Display', $named->typography($style)['family'], $style);
        }
        foreach (['lead', 'body', 'eyebrow', 'button'] as $style) {
            $this->assertSame('DM Sans', $named->typography($style)['family'], $style);
        }

        $system = DesignTokens::fromDesign(self::design(['fonts' => ['heading' => 'system', 'body' => 'system']]));
        foreach (DesignTokens::STYLES as $style) {
            $this->assertNull($system->typography($style)['family'], $style);
        }
    }

    public function test_unknown_style_is_refused(): void
    {
        $tokens = DesignTokens::fromDesign(self::design());

        foreach (['', 'h1', 'Display', 'caption'] as $bad) {
            try {
                $tokens->typography($bad);
                $this->fail('style "' . $bad . '" must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }
    }

    public function test_section_padding_by_spacing_and_density(): void
    {
        foreach (self::SECTION_PADDING as $density => $bySpacing) {
            $tokens = DesignTokens::fromDesign(self::design(['density' => $density]));
            foreach ($bySpacing as $spacing => [$desktop, $tablet, $mobile]) {
                $this->assertSame(
                    ['desktop' => $desktop, 'tablet' => $tablet, 'mobile' => $mobile],
                    $tokens->sectionPadding($spacing),
                    $density . ' ' . $spacing
                );
            }
        }
    }

    public function test_row_gap_and_card_padding_by_density(): void
    {
        foreach (['compact', 'normal', 'airy'] as $density) {
            $tokens = DesignTokens::fromDesign(self::design(['density' => $density]));

            [$desktop, $tablet, $mobile] = self::ROW_GAP[$density];
            $this->assertSame(['desktop' => $desktop, 'tablet' => $tablet, 'mobile' => $mobile], $tokens->rowGap(), 'row gap ' . $density);

            [$desktop, $tablet, $mobile] = self::CARD_PADDING[$density];
            $this->assertSame(['desktop' => $desktop, 'tablet' => $tablet, 'mobile' => $mobile], $tokens->cardPadding(), 'card padding ' . $density);
        }
    }

    public function test_spacing_steps_down_from_desktop_to_mobile_and_up_with_the_step(): void
    {
        foreach (['compact', 'normal', 'airy'] as $density) {
            $tokens = DesignTokens::fromDesign(self::design(['density' => $density]));
            $last   = 0;
            foreach (OutlineV3Schema::SPACINGS as $spacing) {
                $padding = $tokens->sectionPadding($spacing);
                $this->assertGreaterThan($padding['tablet'], $padding['desktop'], $density . ' ' . $spacing);
                $this->assertGreaterThan($padding['mobile'], $padding['tablet'], $density . ' ' . $spacing);
                $this->assertGreaterThan($last, $padding['desktop'], $density . ' ' . $spacing . ' grows with the step');
                $last = $padding['desktop'];
            }
        }
    }

    public function test_unknown_spacing_is_refused(): void
    {
        $tokens = DesignTokens::fromDesign(self::design());

        foreach (['', 'sm', 'XL', 'xxl'] as $bad) {
            try {
                $tokens->sectionPadding($bad);
                $this->fail('spacing "' . $bad . '" must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }
    }

    public function test_radius_in_pixels(): void
    {
        $expected = ['none' => 0, 'sm' => 6, 'md' => 12, 'lg' => 20];
        foreach ($expected as $name => $px) {
            $this->assertSame($px, DesignTokens::fromDesign(self::design(['radius' => $name]))->radius(), $name);
        }
        $this->assertSame(array_keys($expected), OutlineV3Schema::RADII);
    }

    public function test_every_density_and_radius_the_schema_names_is_accepted(): void
    {
        foreach (OutlineV3Schema::DENSITIES as $density) {
            foreach (OutlineV3Schema::RADII as $radius) {
                $tokens = DesignTokens::fromDesign(self::design(['density' => $density, 'radius' => $radius]));
                $this->assertIsInt($tokens->radius());
                $this->assertIsInt($tokens->rowGap()['desktop']);
            }
        }
    }

    public function test_a_design_with_a_bad_scale_radius_density_or_font_is_refused(): void
    {
        $bad = [
            'no fonts'               => array_diff_key(self::design(), ['fonts' => true]),
            'fonts not a map'        => self::design(['fonts' => 'Manrope']),
            'no heading font'        => self::design(['fonts' => ['body' => 'system']]),
            'no body font'           => self::design(['fonts' => ['heading' => 'Manrope']]),
            'empty font'             => self::design(['fonts' => ['heading' => '', 'body' => 'system']]),
            'font of 61 characters'  => self::design(['fonts' => ['heading' => str_repeat('a', 61), 'body' => 'system']]),
            'font with a semicolon'  => self::design(['fonts' => ['heading' => 'Open;Sans', 'body' => 'system']]),
            'font with a quote'      => self::design(['fonts' => ['heading' => 'Open"Sans', 'body' => 'system']]),
            'font with a paren'      => self::design(['fonts' => ['heading' => 'url(x)', 'body' => 'system']]),
            'font with a newline'    => self::design(['fonts' => ['heading' => "Manrope\n", 'body' => 'system']]),
            'font not a string'      => self::design(['fonts' => ['heading' => 12, 'body' => 'system']]),
            'no scale'               => array_diff_key(self::design(), ['scale' => true]),
            'scale not a map'        => self::design(['scale' => 17]),
            'no base'                => self::design(['scale' => ['ratio' => 'major_third']]),
            'no ratio'               => self::design(['scale' => ['base_px' => 17]]),
            'base 14'                => self::withScale(14, 'major_third'),
            'base 21'                => self::withScale(21, 'major_third'),
            'base 17.5'              => self::withScale(17.5, 'major_third'),
            'base as text'           => self::withScale('17', 'major_third'),
            'ratio as the number'    => self::withScale(17, 1.25),
            'ratio as an integer'    => self::withScale(17, 1),
            'ratio as number text'   => self::withScale(17, '1.25'),
            'ratio unknown name'     => self::withScale(17, 'golden'),
            'ratio in capitals'      => self::withScale(17, 'Major_Third'),
            'ratio with a newline'   => self::withScale(17, "major_third\n"),
            'ratio with a space'     => self::withScale(17, ' major_third'),
            'ratio with a hyphen'    => self::withScale(17, 'major-third'),
            'ratio empty'            => self::withScale(17, ''),
            'ratio null'             => self::withScale(17, null),
            'ratio true'             => self::withScale(17, true),
            'ratio a list'           => self::withScale(17, ['major_third']),
            'ratio not a number'     => self::withScale(17, NAN),
            'ratio infinite'         => self::withScale(17, INF),
            'no radius'              => array_diff_key(self::design(), ['radius' => true]),
            'unknown radius'         => self::design(['radius' => 'xl']),
            'radius in capitals'     => self::design(['radius' => 'MD']),
            'radius not a string'    => self::design(['radius' => 12]),
            'no density'             => array_diff_key(self::design(), ['density' => true]),
            'unknown density'        => self::design(['density' => 'tight']),
        ];
        foreach ($bad as $why => $design) {
            try {
                DesignTokens::fromDesign($design);
                $this->fail('a design with ' . $why . ' must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }
    }

    public function test_the_edges_of_the_allowed_ranges_are_accepted(): void
    {
        foreach ([[15, 'major_second'], [20, 'perfect_fourth'], [15, 'perfect_fourth'], [20, 'major_second'], [15, 'minor_third'], [20, 'major_third']] as [$base, $ratio]) {
            $tokens = DesignTokens::fromDesign(self::withScale($base, $ratio));
            $this->assertSame($base, $tokens->typography('body')['size'], $base . ' x ' . $ratio);
        }
        $edge =DesignTokens::fromDesign(self::design(['fonts' => ['heading' => str_repeat('a', 60), 'body' => 'A1 b2']]));
        $this->assertSame(str_repeat('a', 60), $edge->typography('h2')['family']);
        $this->assertSame('A1 b2', $edge->typography('body')['family']);
    }
}
