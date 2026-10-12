<?php
/**
 * DesignTokens colours: the tone table, the overlay opacity, the hover colour,
 * and the tinted shadow and hairline, all as literals.
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
final class DesignTokensColourTest extends TestCase
{
    /** The design of the shared "v3-minimal-page" case. */
    private const DESIGN = [
        'colors'  => [
            'ground'    => '#F6F5F2',
            'surface'   => '#FFFFFF',
            'ink'       => '#15171C',
            'muted'     => '#5A606C',
            'accent'    => '#0D7A63',
            'on_accent' => '#FFFFFF',
        ],
        'fonts'   => ['heading' => 'Manrope', 'body' => 'system'],
        'scale'   => ['base_px' => 17, 'ratio' => 1.25],
        'radius'  => 'md',
        'density' => 'normal',
    ];

    /**
     * @param array<string,string> $colours Roles to replace.
     * @return array<string,mixed>
     */
    private static function design(array $colours = []): array
    {
        $design           = self::DESIGN;
        $design['colors'] = array_replace($design['colors'], $colours);

        return $design;
    }

    public function test_tone_table_maps_each_tone_to_roles(): void
    {
        $tokens = DesignTokens::fromDesign(self::design());

        $this->assertSame(
            ['background' => '#F6F5F2', 'heading' => '#15171C', 'body' => '#5A606C', 'accent' => '#0D7A63'],
            $tokens->tone('ground')
        );
        $this->assertSame(
            ['background' => '#FFFFFF', 'heading' => '#15171C', 'body' => '#5A606C', 'accent' => '#0D7A63'],
            $tokens->tone('surface')
        );
        $this->assertSame(
            ['background' => '#15171C', 'heading' => '#F6F5F2', 'body' => 'rgba(246,245,242,0.8)', 'accent' => '#0D7A63'],
            $tokens->tone('inverse')
        );
        $this->assertSame(
            ['background' => '#0D7A63', 'heading' => '#FFFFFF', 'body' => '#FFFFFF', 'accent' => '#FFFFFF'],
            $tokens->tone('accent')
        );
    }

    public function test_tone_colours_follow_the_design_not_a_fixed_palette(): void
    {
        $tokens = DesignTokens::fromDesign(self::design([
            'ground'    => '#FAF7F2',
            'ink'       => '#1B2A41',
            'accent'    => '#C2410C',
            'on_accent' => '#FFF8F0',
        ]));

        $inverse = $tokens->tone('inverse');
        $this->assertSame('#1B2A41', $inverse['background']);
        $this->assertSame('#FAF7F2', $inverse['heading']);
        $this->assertSame('rgba(250,247,242,0.8)', $inverse['body']);

        $accent = $tokens->tone('accent');
        $this->assertSame('#C2410C', $accent['background']);
        $this->assertSame('#FFF8F0', $accent['heading']);
        $this->assertSame('#FFF8F0', $accent['body']);
        $this->assertSame('#FFF8F0', $accent['accent']);
    }

    public function test_every_tone_and_overlay_the_schema_names_is_answered(): void
    {
        $tokens = DesignTokens::fromDesign(self::design());

        foreach (OutlineV3Schema::TONES as $tone) {
            $this->assertSame(['background', 'heading', 'body', 'accent'], array_keys($tokens->tone($tone)), $tone);
        }
        foreach (OutlineV3Schema::OVERLAYS as $overlay) {
            $answer = $tokens->overlay($overlay);
            $this->assertTrue($answer === null || array_keys($answer) === ['color', 'opacity'], $overlay);
        }
    }

    public function test_unknown_tone_is_refused(): void
    {
        $tokens = DesignTokens::fromDesign(self::design());

        foreach (['', 'dark', 'Ground', 'accent '] as $bad) {
            try {
                $tokens->tone($bad);
                $this->fail('tone "' . $bad . '" must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }
    }

    public function test_overlay_strength_is_opacity_of_ink(): void
    {
        $tokens = DesignTokens::fromDesign(self::design());

        $this->assertSame(['color' => '#15171C', 'opacity' => 0.45], $tokens->overlay('soft'));
        $this->assertSame(['color' => '#15171C', 'opacity' => 0.65], $tokens->overlay('strong'));
        $this->assertNull($tokens->overlay('none'));

        $other = DesignTokens::fromDesign(self::design(['ink' => '#1B2A41']));
        $this->assertSame(['color' => '#1B2A41', 'opacity' => 0.45], $other->overlay('soft'));

        foreach (['', 'medium', 'Strong'] as $bad) {
            try {
                $tokens->overlay($bad);
                $this->fail('overlay "' . $bad . '" must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }
    }

    public function test_hover_lowers_hsl_lightness_by_eight_points(): void
    {
        // Expected values come from an independent HSL implementation, not from DesignTokens.
        $expected = [
            '#0D7A63' => '#095545',
            '#2563EB' => '#1350D4',
            '#C2410C' => '#9C340A',
            '#808080' => '#6C6C6C',
            '#5A606C' => '#474C56',
            '#F6F5F2' => '#E5E2DA',
            '#F5F5F5' => '#E1E1E1',
            '#FFFFFF' => '#EBEBEB',
        ];
        foreach ($expected as $from => $to) {
            $this->assertSame($to, DesignTokens::hover($from), $from);
        }
    }

    public function test_hover_rounds_half_up_when_a_channel_lands_exactly_between_two_values(): void
    {
        // Each of these has a channel of exactly n + 0.5 before rounding (checked in exact
        // fractions), so a float implementation can land either side; the answer is the upper one.
        $expected = [
            '#007819' => '#004F11',
            '#00F032' => '#00C72A',
            '#05324B' => '#021925',
            '#054B73' => '#03324C',
            '#0596EB' => '#047DC3',
            '#0A196E' => '#071149',
        ];
        foreach ($expected as $from => $to) {
            $this->assertSame($to, DesignTokens::hover($from), $from);
        }
    }

    public function test_hover_never_reaches_pure_black(): void
    {
        // Near-black colours stop at a floor instead of collapsing to #000000.
        $expected = [
            '#111111' => '#0A0A0A',
            '#15171C' => '#090A0C',
            '#0A0A0A' => '#0A0A0A',
            '#050505' => '#050505',
            '#010101' => '#010101',
        ];
        foreach ($expected as $from => $to) {
            $this->assertSame($to, DesignTokens::hover($from), $from);
        }

        // A sweep of the colour cube: never black, never lighter, always upper-case #RRGGBB.
        $steps = [0, 1, 16, 64, 128, 192, 255];
        foreach ($steps as $r) {
            foreach ($steps as $g) {
                foreach ($steps as $b) {
                    $from = sprintf('#%02X%02X%02X', $r, $g, $b);
                    if ($from === '#000000') {
                        continue;
                    }
                    $to = DesignTokens::hover($from);
                    $this->assertMatchesRegularExpression('/^#[0-9A-F]{6}$/D', $to, $from);
                    $this->assertNotSame('#000000', $to, $from);

                    $out = [(int) hexdec(substr($to, 1, 2)), (int) hexdec(substr($to, 3, 2)), (int) hexdec(substr($to, 5, 2))];
                    $this->assertLessThanOrEqual(
                        max($r, $g, $b) + min($r, $g, $b),
                        max($out) + min($out),
                        $from . ' must not get lighter'
                    );
                }
            }
        }
    }

    public function test_card_shadow_is_two_layers_tinted_with_ink(): void
    {
        $this->assertSame(
            [
                ['x' => 0, 'y' => 1, 'blur' => 2, 'spread' => 0, 'color' => 'rgba(21,23,28,0.06)'],
                ['x' => 0, 'y' => 8, 'blur' => 24, 'spread' => 0, 'color' => 'rgba(21,23,28,0.08)'],
            ],
            DesignTokens::fromDesign(self::design())->cardShadow()
        );

        $other = DesignTokens::fromDesign(self::design(['ink' => '#1B2A41']))->cardShadow();
        $this->assertSame('rgba(27,42,65,0.06)', $other[0]['color']);
        $this->assertSame('rgba(27,42,65,0.08)', $other[1]['color']);
        foreach ($other as $layer) {
            $this->assertStringNotContainsString('rgba(0,0,0', $layer['color'], 'a shadow is tinted, never pure black');
        }
    }

    public function test_hairline_is_one_pixel_of_muted_at_twenty_percent(): void
    {
        $this->assertSame(
            ['width' => 1, 'color' => 'rgba(90,96,108,0.2)'],
            DesignTokens::fromDesign(self::design())->hairline()
        );
        $this->assertSame(
            ['width' => 1, 'color' => 'rgba(100,116,139,0.2)'],
            DesignTokens::fromDesign(self::design(['muted' => '#64748B']))->hairline()
        );
    }

    public function test_alpha_text_is_exact_whatever_the_float_ini_says(): void
    {
        $precision          = ini_get('precision');
        $serializePrecision = ini_get('serialize_precision');
        try {
            // A host that raises precision turns (string) 0.8 into 0.80000000000000004.
            ini_set('precision', '17');
            ini_set('serialize_precision', '17');

            $this->assertSame('rgba(246,245,242,0.8)', DesignTokens::alpha('#F6F5F2', 80));
            $this->assertSame('rgba(21,23,28,0.06)', DesignTokens::alpha('#15171C', 6));
            $this->assertSame('rgba(90,96,108,0.2)', DesignTokens::alpha('#5A606C', 20));
        } finally {
            ini_set('precision', (string) $precision);
            ini_set('serialize_precision', (string) $serializePrecision);
        }

        $this->assertSame('rgba(255,255,255,0)', DesignTokens::alpha('#FFFFFF', 0));
        $this->assertSame('rgba(255,255,255,0.05)', DesignTokens::alpha('#FFFFFF', 5));
        $this->assertSame('rgba(255,255,255,0.45)', DesignTokens::alpha('#FFFFFF', 45));
        $this->assertSame('rgba(255,255,255,1)', DesignTokens::alpha('#FFFFFF', 100));
    }

    public function test_alpha_and_hover_refuse_what_is_not_an_allowed_colour(): void
    {
        foreach (['', 'F6F5F2', '#f6f5f2', '#FFF', '#F6F5F2F0', "#F6F5F2\n", '#GGGGGG', 'red', '#000000'] as $bad) {
            try {
                DesignTokens::alpha($bad, 50);
                $this->fail('alpha() must refuse "' . $bad . '"');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
            try {
                DesignTokens::hover($bad);
                $this->fail('hover() must refuse "' . $bad . '"');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }

        foreach ([-1, 101] as $bad) {
            try {
                DesignTokens::alpha('#F6F5F2', $bad);
                $this->fail('alpha percent ' . $bad . ' must be refused');
            } catch (\InvalidArgumentException $e) {
                $this->addToAssertionCount(1);
            }
        }
    }

    public function test_design_colours_are_refused_unless_all_six_are_upper_case_and_not_black(): void
    {
        $bad = [
            'a missing role'   => self::withoutRole('muted'),
            'lower case'       => self::design(['accent' => '#0d7a63']),
            'three digits'     => self::design(['ink' => '#111']),
            'pure black'       => self::design(['ink' => '#000000']),
            'trailing newline' => self::design(['ground' => "#F6F5F2\n"]),
            'not a string'     => ['colors' => ['ground' => 0xF6F5F2] + self::DESIGN['colors']] + self::DESIGN,
            'no colours'       => array_diff_key(self::DESIGN, ['colors' => true]),
            'colours not a map' => ['colors' => '#F6F5F2'] + self::DESIGN,
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

    /**
     * @param string $role The colour role to drop.
     * @return array<string,mixed>
     */
    private static function withoutRole(string $role): array
    {
        $design           = self::DESIGN;
        $design['colors'] = array_diff_key($design['colors'], [$role => true]);

        return $design;
    }
}
