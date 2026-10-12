<?php
/**
 * DesignTokens golden: every value the token class answers for a spread of
 * designs, recorded in one shared fixture and asserted equal here. A twin of
 * the class in another language replays the same file.
 *
 * Fixture (regenerate with WPMGR_WRITE_FIXTURES=1, never hand-edit):
 *   design-tokens-cases.json  designs, and what DesignTokens answers for each
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use WPMgr\Agent\Abilities\Builders\DesignTokens;
use WPMgr\Agent\Abilities\Builders\ElementorLeafStyle;
use WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler;
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\DesignTokens
 */
final class DesignTokensGoldenTest extends TestCase
{
    private const FIXTURE = __DIR__ . '/../fixtures/ability-run/design-tokens-cases.json';

    /**
     * Single calls of DesignTokens::hover(): greys (the grey branch, the
     * floor and below it), full-chroma colours around the sum of 255, the
     * boundary where the 8 point step meets the 4 point floor (sums of 60, 61
     * and 62), near black with chroma, light colours and mid colours.
     */
    private const HOVER_INPUTS = [
        '#FFFFFF', '#FEFEFE', '#EEEEEE', '#808080', '#404040', '#1A1A1A', '#111111', '#0A0A0A', '#050505', '#010101',
        '#FF0000', '#00FF00', '#0000FF', '#FFFF00', '#00FFFF', '#FF00FF', '#FE0000', '#FF0101',
        '#271515', '#281515', '#291515',
        '#0B0F1A', '#000001', '#010000', '#00010A',
        '#FDE68A', '#F6F5F2', '#E0F2FE', '#FFFAFA', '#FDFDFD',
        '#0D7A63', '#E4002B', '#3B82F6', '#7C3AED', '#B4532A', '#22D3EE',
    ];

    /** Single calls of DesignTokens::alpha(): the colours and the percents they are taken at. */
    private const ALPHA_COLOURS = ['#0D7A63', '#FFFFFF', '#0A0A0A'];

    private const ALPHA_PERCENTS = [0, 1, 5, 6, 8, 9, 10, 20, 45, 50, 65, 80, 99, 100];

    /** Calls the class refuses with an InvalidArgumentException. */
    private const REFUSED = [
        ['call' => 'hover', 'hex' => '#000000'],
        ['call' => 'hover', 'hex' => '#0d7a63'],
        ['call' => 'hover', 'hex' => '#FFF'],
        ['call' => 'hover', 'hex' => '0D7A63'],
        ['call' => 'hover', 'hex' => "#0D7A63\n"],
        ['call' => 'hover', 'hex' => ''],
        ['call' => 'alpha', 'hex' => '#000000', 'percent' => 50],
        ['call' => 'alpha', 'hex' => "#0D7A63\n", 'percent' => 50],
        ['call' => 'alpha', 'hex' => '#0D7A63', 'percent' => -1],
        ['call' => 'alpha', 'hex' => '#0D7A63', 'percent' => 101],
    ];

    // -------------------------------------------------------------------------
    // The fixture
    // -------------------------------------------------------------------------

    public function test_fixture_is_what_the_code_produces(): void
    {
        $want = self::fixtureText();
        if (getenv('WPMGR_WRITE_FIXTURES') === '1') {
            $this->assertNotFalse(file_put_contents(self::FIXTURE, $want), 'could not write design-tokens-cases.json');
        }
        $this->assertFileExists(self::FIXTURE, 'design-tokens-cases.json is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $got = file_get_contents(self::FIXTURE);
        $this->assertIsString($got);
        $this->assertSame($want, $got, 'design-tokens-cases.json differs from what DesignTokens answers now; regenerate with WPMGR_WRITE_FIXTURES=1');
    }

    public function test_every_case_replays_through_the_code(): void
    {
        $doc = $this->fixture();
        $this->assertNotEmpty($doc['cases'], 'the golden holds no case');
        foreach ($doc['cases'] as $case) {
            $this->assertSame($case['tokens'], self::tokensOf($case['design']), $case['name']);
        }
        $this->assertSame($doc['ratios'], self::ratios(), 'ratios');
        $this->assertSame($doc['type_scale'], self::typeScale(), 'type_scale');
        $this->assertSame($doc['hover'], self::hoverRows(), 'hover');
        $this->assertSame($doc['alpha'], self::alphaRows(), 'alpha');
    }

    public function test_refused_calls_are_refused(): void
    {
        $doc = $this->fixture();
        $this->assertNotEmpty($doc['refused'], 'the golden holds no refused call');
        foreach ($doc['refused'] as $row) {
            try {
                match ($row['call']) {
                    'hover' => DesignTokens::hover($row['hex']),
                    'alpha' => DesignTokens::alpha($row['hex'], $row['percent']),
                };
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
                continue;
            }
            $this->fail('the class accepted a call the golden says it refuses: ' . json_encode($row));
        }
    }

    // -------------------------------------------------------------------------
    // What the golden has to hold
    // -------------------------------------------------------------------------

    public function test_the_golden_covers_every_option(): void
    {
        $doc = $this->fixture();

        $ratios    = [];
        $densities = [];
        $radii     = [];
        $bases     = [];
        foreach ($doc['cases'] as $case) {
            $ratios[$case['design']['scale']['ratio']]    = true;
            $densities[$case['design']['density']]        = true;
            $radii[$case['design']['radius']]             = true;
            $bases[$case['design']['scale']['base_px']]   = true;

            $tokens = $case['tokens'];
            $this->assertSame(OutlineV3Schema::TONES, array_keys($tokens['tones']), $case['name'] . ' tones');
            $this->assertSame(OutlineV3Schema::OVERLAYS, array_keys($tokens['overlay']), $case['name'] . ' overlay');
            $this->assertSame(DesignTokens::STYLES, array_keys($tokens['typography']), $case['name'] . ' typography');
            $this->assertSame(OutlineV3Schema::SPACINGS, array_keys($tokens['section_padding']), $case['name'] . ' section padding');
            $this->assertSame(OutlineV3Schema::COLOUR_ROLES, array_keys($tokens['hover']), $case['name'] . ' hover');
        }

        $this->assertSame(self::sorted(array_keys(DesignTokens::RATIOS)), self::sorted(array_keys($ratios)), 'every ratio name has a design');
        $this->assertSame(self::sorted(OutlineV3Schema::DENSITIES), self::sorted(array_keys($densities)), 'every density has a design');
        $this->assertSame(self::sorted(OutlineV3Schema::RADII), self::sorted(array_keys($radii)), 'every radius has a design');
        $this->assertSame(range(OutlineV3Schema::SCALE_BASE_PX[0], OutlineV3Schema::SCALE_BASE_PX[1]), self::sorted(array_keys($bases)), 'every base size has a design');

        $names = array_column($doc['cases'], 'name');
        foreach (['near-black-accent', 'below-floor-accent', 'light-yellow-accent', 'pale-accent-on-dark', 'dark-ground', 'all-grey'] as $name) {
            $this->assertContains($name, $names, 'the palette spread keeps ' . $name);
        }

        $scaleKeys = [];
        foreach ($doc['type_scale'] as $row) {
            $scaleKeys[] = $row['ratio'] . '/' . $row['base_px'];
        }
        $this->assertCount(count(DesignTokens::RATIOS) * 6, array_unique($scaleKeys), 'type_scale holds every ratio at every base size');
    }

    public function test_the_golden_holds_no_float_and_no_pure_black(): void
    {
        $doc = $this->fixture();
        $this->assertSame([], self::floatPaths($doc), 'a number written as a float depends on the host float settings');

        // Pure black is refused on the way in, so no answer holds it. The refused
        // calls name it as the input they are refused for, and are left out.
        $answers = $doc;
        unset($answers['refused']);
        $text = json_encode($answers, JSON_THROW_ON_ERROR);
        $this->assertStringNotContainsString('#000000', $text);
        $this->assertStringNotContainsString('rgba(0,0,0', $text);
        foreach ($doc['cases'] as $case) {
            $this->assertSame([], self::floatPaths($case['tokens']), $case['name']);
        }
    }

    /**
     * Values worked out by hand from the documented rule, so a regeneration
     * cannot turn a fault of the class into the expected answer.
     */
    public function test_anchors_worked_out_from_the_documented_rule(): void
    {
        // Lightness 100% less 8 points is 92%: 0.92 * 255 = 234.6, a half up 235.
        $this->assertSame('#EBEBEB', DesignTokens::hover('#FFFFFF'));
        // Full red at lightness 50% becomes 42%: the red channel is 0.84 * 255 = 214.2.
        $this->assertSame('#D60000', DesignTokens::hover('#FF0000'));
        // The lightness stops at 4 points, 0.04 * 255 = 10.2, so a grey at 6.7% goes to 0x0A
        // and a grey darker than the floor stays as it is.
        $this->assertSame('#0A0A0A', DesignTokens::hover('#111111'));
        $this->assertSame('#050505', DesignTokens::hover('#050505'));
        // Near black with chroma: lightness 7.25% goes to 4%, so each channel is scaled by 4 / 7.25.
        $this->assertSame('#06080E', DesignTokens::hover('#0B0F1A'));

        $this->assertSame('rgba(13,122,99,0)', DesignTokens::alpha('#0D7A63', 0));
        $this->assertSame('rgba(13,122,99,0.05)', DesignTokens::alpha('#0D7A63', 5));
        $this->assertSame('rgba(13,122,99,0.06)', DesignTokens::alpha('#0D7A63', 6));
        $this->assertSame('rgba(13,122,99,0.1)', DesignTokens::alpha('#0D7A63', 10));
        $this->assertSame('rgba(13,122,99,0.45)', DesignTokens::alpha('#0D7A63', 45));
        $this->assertSame('rgba(13,122,99,0.8)', DesignTokens::alpha('#0D7A63', 80));
        $this->assertSame('rgba(13,122,99,1)', DesignTokens::alpha('#0D7A63', 100));

        $doc = $this->fixture();
        $byName = [];
        foreach ($doc['cases'] as $case) {
            $byName[$case['name']] = $case['tokens'];
        }
        // The shared design: the overlay opacities are fractions written as text.
        $this->assertSame(['color' => '#15171C', 'opacity' => '0.45'], $byName['studio-light']['overlay']['soft']);
        $this->assertSame(['color' => '#15171C', 'opacity' => '0.65'], $byName['studio-light']['overlay']['strong']);
        $this->assertNull($byName['studio-light']['overlay']['none']);
        // The inverse tone: ink ground, ground headings, ground body text at 80%.
        $this->assertSame(
            ['background' => '#15171C', 'heading' => '#F6F5F2', 'body' => 'rgba(246,245,242,0.8)', 'accent' => '#0D7A63'],
            $byName['studio-light']['tones']['inverse']
        );
        // Density compact is 75% of normal: 64/56/40 becomes 48/42/30.
        $this->assertSame(['desktop' => 48, 'tablet' => 42, 'mobile' => 30], $byName['compact-slate']['section_padding']['md']);
        $this->assertSame(['desktop' => 80, 'tablet' => 70, 'mobile' => 50], $byName['airy-warm']['section_padding']['md']);
    }

    // -------------------------------------------------------------------------
    // The generator
    // -------------------------------------------------------------------------

    /**
     * The designs of the golden.
     *
     * @return list<array{name:string,design:array<string,mixed>}>
     */
    private static function designs(): array
    {
        return [
            self::row('studio-light', ['#F6F5F2', '#FFFFFF', '#15171C', '#5A606C', '#0D7A63', '#FFFFFF'], 'Manrope', 'system', 17, 'major_third', 'md', 'normal'),
            self::row('compact-slate', ['#F8FAFC', '#EEF2F7', '#0F172A', '#475569', '#2563EB', '#FFFFFF'], 'system', 'system', 15, 'minor_third', 'none', 'compact'),
            self::row('airy-warm', ['#FBF7F0', '#F1E8DA', '#2B1D14', '#6B5646', '#B4532A', '#FFFFFF'], 'Playfair Display', 'Inter', 20, 'perfect_fourth', 'lg', 'airy'),
            self::row('near-black-accent', ['#FFFFFF', '#F4F4F5', '#18181B', '#52525B', '#0B0F1A', '#FFFFFF'], 'Inter', 'Inter', 16, 'major_second', 'sm', 'normal'),
            self::row('below-floor-accent', ['#FAFAFA', '#FFFFFF', '#0A0A0A', '#737373', '#050505', '#FAFAFA'], 'IBM Plex Sans', 'IBM Plex Sans', 18, 'major_third', 'md', 'compact'),
            self::row('light-yellow-accent', ['#FFFFFF', '#FFF9E5', '#1F2937', '#6B7280', '#FFD60A', '#1F2937'], 'Poppins', 'Source Sans 3', 19, 'minor_third', 'lg', 'airy'),
            self::row('dark-ground', ['#0F1115', '#171A21', '#F3F4F6', '#9CA3AF', '#22D3EE', '#0F1115'], 'Space Grotesk', 'system', 17, 'major_second', 'none', 'normal'),
            self::row('saturated-red', ['#FFFFFF', '#FFF1F2', '#2A0A10', '#7F1D1D', '#E4002B', '#FFFFFF'], 'Oswald', 'Open Sans', 16, 'perfect_fourth', 'sm', 'compact'),
            self::row('all-grey', ['#F5F5F5', '#E5E5E5', '#262626', '#808080', '#404040', '#FFFFFF'], 'system', 'system', 17, 'major_third', 'md', 'airy'),
            self::row('pale-accent-on-dark', ['#111827', '#1F2937', '#F9FAFB', '#9CA3AF', '#FDE68A', '#1F2937'], 'Lora', 'Lato', 20, 'major_second', 'lg', 'normal'),
        ];
    }

    /**
     * One design of the golden.
     *
     * @param string       $name    The case name.
     * @param list<string> $palette The colours in the order of OutlineV3Schema::COLOUR_ROLES.
     * @param string       $heading The heading font.
     * @param string       $body    The body font.
     * @param int          $base    The base size in px.
     * @param string       $ratio   A name of DesignTokens::RATIOS.
     * @param string       $radius  A radius name.
     * @param string       $density A density name.
     * @return array{name:string,design:array<string,mixed>}
     */
    private static function row(string $name, array $palette, string $heading, string $body, int $base, string $ratio, string $radius, string $density): array
    {
        return [
            'name'   => $name,
            'design' => [
                'colors'  => array_combine(OutlineV3Schema::COLOUR_ROLES, $palette),
                'fonts'   => ['heading' => $heading, 'body' => $body],
                'scale'   => ['base_px' => $base, 'ratio' => $ratio],
                'radius'  => $radius,
                'density' => $density,
            ],
        ];
    }

    /**
     * Every value DesignTokens answers for a design. A float is written as
     * the text the compile path writes, so the file holds no float.
     *
     * @param array<string,mixed> $design The design.
     * @return array<string,mixed>
     */
    private static function tokensOf(array $design): array
    {
        $tokens = DesignTokens::fromDesign($design);

        $tones = [];
        foreach (OutlineV3Schema::TONES as $tone) {
            $tones[$tone] = $tokens->tone($tone);
        }

        $overlay = [];
        foreach (OutlineV3Schema::OVERLAYS as $strength) {
            $layer              = $tokens->overlay($strength);
            $overlay[$strength] = $layer === null ? null : ['color' => $layer['color'], 'opacity' => self::fraction($layer['opacity'])];
        }

        $typography = [];
        foreach (DesignTokens::STYLES as $style) {
            $type               = $tokens->typography($style);
            $typography[$style] = [
                'family'         => $type['family'],
                'size'           => $type['size'],
                'weight'         => $type['weight'],
                'line_height'    => $type['line_height'] === null ? null : self::decimal($type['line_height']),
                'letter_spacing' => $type['letter_spacing'] === null ? null : self::decimal($type['letter_spacing']),
                'transform'      => $type['transform'],
            ];
        }

        $sectionPadding = [];
        foreach (OutlineV3Schema::SPACINGS as $spacing) {
            $sectionPadding[$spacing] = $tokens->sectionPadding($spacing);
        }

        $hover = [];
        foreach (OutlineV3Schema::COLOUR_ROLES as $role) {
            $hover[$role] = DesignTokens::hover($design['colors'][$role]);
        }

        return [
            'radius'          => $tokens->radius(),
            'row_gap'         => $tokens->rowGap(),
            'card_padding'    => $tokens->cardPadding(),
            'section_padding' => $sectionPadding,
            'card_shadow'     => $tokens->cardShadow(),
            'hairline'        => $tokens->hairline(),
            'overlay'         => $overlay,
            'tones'           => $tones,
            'typography'      => $typography,
            'hover'           => $hover,
        ];
    }

    /**
     * The number each ratio name stands for, as text.
     *
     * @return array<string,string>
     */
    private static function ratios(): array
    {
        $out = [];
        foreach (DesignTokens::RATIOS as $name => $ratio) {
            $out[$name] = self::decimal($ratio);
        }

        return $out;
    }

    /**
     * The pixel size of each text style at every ratio name and every base size.
     *
     * @return list<array{ratio:string,base_px:int,sizes:array<string,int>}>
     */
    private static function typeScale(): array
    {
        $design = self::designs()[0]['design'];
        $rows   = [];
        foreach (array_keys(DesignTokens::RATIOS) as $ratio) {
            for ($px = OutlineV3Schema::SCALE_BASE_PX[0]; $px <= OutlineV3Schema::SCALE_BASE_PX[1]; $px++) {
                $design['scale'] = ['base_px' => $px, 'ratio' => $ratio];
                $tokens          = DesignTokens::fromDesign($design);
                $sizes           = [];
                foreach (DesignTokens::STYLES as $style) {
                    $sizes[$style] = $tokens->typography($style)['size'];
                }
                $rows[] = ['ratio' => $ratio, 'base_px' => $px, 'sizes' => $sizes];
            }
        }

        return $rows;
    }

    /**
     * @return list<array{hex:string,result:string}>
     */
    private static function hoverRows(): array
    {
        $rows = [];
        foreach (self::HOVER_INPUTS as $hex) {
            $rows[] = ['hex' => $hex, 'result' => DesignTokens::hover($hex)];
        }

        return $rows;
    }

    /**
     * @return list<array{hex:string,percent:int,result:string}>
     */
    private static function alphaRows(): array
    {
        $rows = [];
        foreach (self::ALPHA_COLOURS as $hex) {
            foreach (self::ALPHA_PERCENTS as $percent) {
                $rows[] = ['hex' => $hex, 'percent' => $percent, 'result' => DesignTokens::alpha($hex, $percent)];
            }
        }

        return $rows;
    }

    /**
     * The whole fixture.
     *
     * @return array<string,mixed>
     */
    private static function document(): array
    {
        $cases = [];
        foreach (self::designs() as $row) {
            $cases[] = ['name' => $row['name'], 'design' => $row['design'], 'tokens' => self::tokensOf($row['design'])];
        }

        return [
            'note'       => 'Golden outputs of DesignTokens, regenerated by DesignTokensGoldenTest with WPMGR_WRITE_FIXTURES=1 and never edited by hand. '
                . 'Each case holds a design (the input) and every value the class answers for it. '
                . 'A number the compile path writes as text is recorded as that text: line_height, letter_spacing and the overlay opacity are strings '
                . '(a point for the decimal mark, at most four decimals, no trailing zero; the opacity at most two), and every other number is a JSON integer, '
                . 'so the file holds no float and reads the same under any float setting. '
                . 'A colour is an upper-case #RRGGBB, or rgba(r,g,b,a) with no spaces and the alpha without trailing zeros. '
                . 'A null is a value the class answers as none: a family for the design font system, a line height or letter spacing a style does not set, the overlay none. '
                . '"ratios" is the number each ratio name stands for, as text. '
                . '"type_scale" is the pixel size of each text style for every ratio name and every base size from 15 to 20. '
                . '"hover" and "alpha" are single calls of the two static functions (hover takes an upper-case #RRGGBB, alpha takes that and a percent from 0 to 100). '
                . '"refused" is calls the class refuses with an InvalidArgumentException; a twin refuses them too.',
            'ratios'     => self::ratios(),
            'cases'      => $cases,
            'type_scale' => self::typeScale(),
            'hover'      => self::hoverRows(),
            'alpha'      => self::alphaRows(),
            'refused'    => self::REFUSED,
        ];
    }

    private static function fixtureText(): string
    {
        return json_encode(self::document(), JSON_PRETTY_PRINT | JSON_UNESCAPED_SLASHES | JSON_UNESCAPED_UNICODE | JSON_THROW_ON_ERROR) . "\n";
    }

    /**
     * The fixture, read.
     *
     * @return array<string,mixed>
     */
    private function fixture(): array
    {
        $this->assertFileExists(self::FIXTURE, 'design-tokens-cases.json is missing; regenerate with WPMGR_WRITE_FIXTURES=1');
        $raw = file_get_contents(self::FIXTURE);
        $this->assertIsString($raw);

        return json_decode($raw, true, 512, JSON_THROW_ON_ERROR);
    }

    // -------------------------------------------------------------------------
    // The number formats of the compile path
    // -------------------------------------------------------------------------

    /**
     * A line height or letter spacing as the compile path writes it.
     *
     * @param float $value The number.
     * @return string
     */
    private static function decimal(float $value): string
    {
        return (string) (new \ReflectionMethod(ElementorLeafStyle::class, 'decimal'))->invoke(null, $value);
    }

    /**
     * An overlay opacity as the compile path writes it.
     *
     * @param float $value The opacity.
     * @return string
     */
    private static function fraction(float $value): string
    {
        return (string) (new \ReflectionMethod(ElementorSectionCompiler::class, 'fraction'))->invoke(null, $value);
    }

    /**
     * @param array<int,mixed> $values The values.
     * @return array<int,mixed>
     */
    private static function sorted(array $values): array
    {
        sort($values);

        return $values;
    }

    /**
     * The paths of every float in a value.
     *
     * @param mixed  $value The value.
     * @param string $path  The path so far.
     * @return list<string>
     */
    private static function floatPaths(mixed $value, string $path = ''): array
    {
        if (is_float($value)) {
            return [$path];
        }
        $found = [];
        if (is_array($value)) {
            foreach ($value as $key => $item) {
                $found = array_merge($found, self::floatPaths($item, $path . '/' . $key));
            }
        }

        return $found;
    }
}
