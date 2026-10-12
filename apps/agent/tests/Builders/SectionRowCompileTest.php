<?php
/**
 * A hero's screen height and the rows and groups of a designed page: the
 * height reset on a phone with the content centred; a columns row with the
 * design's gap, each column's width and a full width on a phone; a group.
 * Compiled byte for byte as the frozen round-trip probes store them, through
 * the section compiler and through the mapper.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\DesignTokens;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorClassicMapper
 */
final class SectionRowCompileTest extends TestCase
{
    private const STOCK_BREAKPOINTS = ['mobile', 'tablet'];

    private const REQUEST = 'b7e2a4c1-0000-4000-8000-000000000006';

    private const IMAGE_URL = 'https://example.com/wp-content/uploads/2026/10/hero-1600x900.jpg';

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('add_filter')->justReturn(true);
        Functions\when('remove_filter')->justReturn(true);
        Functions\when('wp_kses_post')->returnArg();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * @param string $density The design's density.
     * @return array<string,mixed>
     */
    private static function design(string $density = 'normal'): array
    {
        return [
            'colors'  => [
                'ground'    => '#FFFFFF',
                'surface'   => '#F6F5F2',
                'ink'       => '#15171C',
                'muted'     => '#5A606C',
                'accent'    => '#0D7A63',
                'on_accent' => '#FAFAF7',
            ],
            'fonts'   => ['heading' => 'Manrope', 'body' => 'system'],
            'scale'   => ['base_px' => 17, 'ratio' => 'major_third'],
            'radius'  => 'md',
            'density' => $density,
        ];
    }

    /**
     * @param list<string>|null $breakpoints Active breakpoints; null when none is.
     * @param string            $density     The design's density.
     * @return ElementorSectionCompiler
     */
    private static function compiler(?array $breakpoints = self::STOCK_BREAKPOINTS, string $density = 'normal'): ElementorSectionCompiler
    {
        $media = [5 => [
            'id'           => 5,
            'url'          => self::IMAGE_URL,
            'filename'     => 'hero-1600x900.jpg',
            'mime'         => 'image/jpeg',
            'width'        => 1600,
            'height'       => 900,
            'modified_gmt' => '2026-10-01 09:00:00',
            'alt'          => '',
        ]];

        return new ElementorSectionCompiler(DesignTokens::fromDesign(self::design($density)), $breakpoints, $media);
    }

    /**
     * The hero section the layout-hero-background probe compiles from.
     *
     * @param array<string,mixed> $override Keys to replace.
     * @return array<string,mixed>
     */
    private static function hero(array $override = []): array
    {
        return array_replace([
            'type'       => 'section',
            'anchor'     => 'hero',
            'width'      => 'full',
            'spacing'    => 'xl',
            'background' => ['attachment_id' => 5, 'overlay' => 'soft', 'focal' => 'center'],
            'min_height' => 'screen',
            'children'   => [['type' => 'heading', 'level' => 1, 'text' => 'Launch week']],
        ], $override);
    }

    /**
     * @param array<mixed> $value A value.
     * @return string Its bytes.
     */
    private static function bytes(array $value): string
    {
        return json_encode($value, JSON_THROW_ON_ERROR);
    }

    /**
     * A tree with every element id checked for IdSeed's shape and replaced
     * by its place, so two trees compare by everything but their ids.
     *
     * @param list<array<string,mixed>> $tree Elements.
     * @param string                    $at   Place of the list.
     * @return list<array<string,mixed>>
     */
    private function shape(array $tree, string $at = 'e'): array
    {
        $out = [];
        foreach ($tree as $i => $node) {
            $this->assertMatchesRegularExpression('/^[0-9a-f]{7}\z/', $node['id'], $at . $i . ': id shape');
            $node['id']       = $at . $i;
            $node['elements'] = $this->shape($node['elements'], $at . $i . '.');
            $out[]            = $node;
        }

        return $out;
    }

    /**
     * The columns row of the layout-columns-row probe: the container inside its outer one.
     *
     * @return array<string,mixed>
     */
    private static function probeRow(): array
    {
        return RoundTripProbes::tree('layout-columns-row')[0]['elements'][0];
    }

    // -------------------------------------------------------------------------
    // The hero's height
    // -------------------------------------------------------------------------

    public function test_hero_min_height_is_the_hero_probe(): void
    {
        $settings = self::compiler()->settings(self::hero());

        $this->assertSame(self::bytes(RoundTripProbes::settings('layout-hero-background')), self::bytes($settings));
    }

    public function test_a_screen_section_is_80vh_reset_on_a_phone_and_centred(): void
    {
        $section = self::hero(['width' => 'boxed']);
        unset($section['background']);
        $settings = self::compiler()->settings($section);

        $this->assertSame(['unit' => 'vh', 'size' => '80'], $settings['min_height']);
        $this->assertSame(['unit' => 'vh', 'size' => '0'], $settings['min_height_mobile']);
        $this->assertSame('center', $settings['flex_justify_content']);
        $keys = array_keys($settings);
        $this->assertSame(['min_height', 'min_height_mobile', 'flex_justify_content', '_title'], array_slice($keys, -4));
    }

    /**
     * @return array<string,array{0:list<string>|null,1:list<string>}>
     */
    public static function heightBreakpoints(): array
    {
        return [
            'stock'       => [['mobile', 'tablet'], ['min_height', 'min_height_mobile', 'flex_justify_content']],
            'tablet only' => [['tablet'], ['min_height', 'flex_justify_content']],
            'none'        => [null, ['min_height', 'flex_justify_content']],
        ];
    }

    /**
     * @dataProvider heightBreakpoints
     * @param list<string>|null $active   Active breakpoints.
     * @param list<string>      $expected The height keys written.
     */
    public function test_the_phone_reset_is_written_only_when_the_site_has_the_mobile_breakpoint(?array $active, array $expected): void
    {
        $settings = self::compiler($active)->settings(self::hero());

        $this->assertSame($expected, array_values(array_intersect(array_keys($settings), ['min_height', 'min_height_mobile', 'flex_justify_content'])));
    }

    public function test_an_auto_height_section_writes_no_height(): void
    {
        foreach ([self::hero(['min_height' => 'auto']), array_diff_key(self::hero(), ['min_height' => true])] as $section) {
            $settings = self::compiler()->settings($section);
            $this->assertSame([], array_intersect_key($settings, array_flip(['min_height', 'min_height_mobile', 'flex_justify_content'])));
        }
    }

    public function test_a_height_the_grammar_would_have_refused_is_not_compiled(): void
    {
        $this->expectException(\InvalidArgumentException::class);

        self::compiler()->settings(self::hero(['min_height' => '100vh']));
    }

    // -------------------------------------------------------------------------
    // A columns row and a group
    // -------------------------------------------------------------------------

    public function test_columns_55_45_is_the_columns_row_probe(): void
    {
        $probe    = self::probeRow();
        $compiler = self::compiler();
        $columns  = [];
        foreach ([55, 45] as $k => $width) {
            $columns[] = $compiler->column($probe['elements'][$k]['id'], $width, $probe['elements'][$k]['elements']);
        }
        $row = $compiler->row($probe['id'], $columns);

        $this->assertSame(self::bytes($probe), self::bytes($row));
        $this->assertSame(['id', 'elType', 'settings', 'elements', 'isInner'], array_keys($row));
        $this->assertTrue($row['isInner']);
    }

    public function test_the_group_is_the_group_probe(): void
    {
        $probe = RoundTripProbes::tree('layout-group')[0]['elements'][0];
        $group = self::compiler()->group($probe['id'], $probe['elements']);

        $this->assertSame(self::bytes($probe), self::bytes($group));
    }

    /**
     * @return array<string,array{0:string,1:list<string>}>
     */
    public static function densities(): array
    {
        return [
            'compact' => ['compact', ['24', '18', '15']],
            'normal'  => ['normal', ['32', '24', '20']],
            'airy'    => ['airy', ['40', '30', '25']],
        ];
    }

    /**
     * @dataProvider densities
     * @param list<string> $gaps Desktop, tablet and mobile gaps.
     */
    public function test_the_row_gap_follows_the_density(string $density, array $gaps): void
    {
        $settings = self::compiler(self::STOCK_BREAKPOINTS, $density)->row('abcdef1', [])['settings'];

        foreach (['flex_gap', 'flex_gap_tablet', 'flex_gap_mobile'] as $i => $key) {
            $this->assertSame(['column' => $gaps[$i], 'row' => $gaps[$i], 'isLinked' => true, 'unit' => 'px'], $settings[$key], $key);
        }
    }

    public function test_a_site_without_breakpoints_gets_the_desktop_row_only(): void
    {
        $compiler = self::compiler(null);

        $this->assertSame(['flex_direction', 'flex_gap'], array_keys($compiler->row('abcdef1', [])['settings']));
        $this->assertSame(['content_width', 'width'], array_keys($compiler->column('abcdef2', 55, [])['settings']));
        $this->assertSame(
            ['flex_direction', 'flex_direction_mobile', 'flex_gap', 'flex_gap_mobile'],
            array_keys(self::compiler(['mobile'])->row('abcdef1', [])['settings'])
        );
    }

    public function test_a_column_width_outside_10_to_90_is_not_compiled(): void
    {
        foreach ([9, 91, 0, 100] as $width) {
            try {
                self::compiler()->column('abcdef2', $width, []);
                $this->fail('a column of ' . $width . '% was compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
        $this->assertSame('10', self::compiler()->column('abcdef2', 10, [])['settings']['width']['size']);
        $this->assertSame('90', self::compiler()->column('abcdef2', 90, [])['settings']['width']['size']);
    }

    // -------------------------------------------------------------------------
    // Through the mapper
    // -------------------------------------------------------------------------

    /**
     * @param array<mixed>                  $outline Normalised outline.
     * @param ElementorSectionCompiler|null $design  The designed page's compiler.
     * @return array<string,mixed>
     */
    private static function map(array $outline, ?ElementorSectionCompiler $design): array
    {
        return ElementorClassicMapper::map(['outline' => $outline], new IdSeed(self::REQUEST), [], true, $design);
    }

    /** @return array<string,mixed> The probe's row as a normalised columns node. */
    private static function columnsNode(): array
    {
        return [
            'type'    => 'columns',
            'widths'  => [55, 45],
            'columns' => [
                [['type' => 'heading', 'level' => 2, 'text' => 'Copy column']],
                [['type' => 'heading', 'level' => 2, 'text' => 'Image column']],
            ],
        ];
    }

    public function test_the_mapper_builds_a_designed_group_as_the_group_probe(): void
    {
        $r = self::map([['type' => 'group', 'children' => [['type' => 'heading', 'level' => 2, 'text' => 'Grouped heading']]]], self::compiler());

        $this->assertArrayHasKey('tree', $r, (string) ($r['detail'] ?? ''));
        $probe = RoundTripProbes::tree('layout-group')[0]['elements'][0];
        $this->assertSame(self::bytes($this->shape([$probe])), self::bytes($this->shape($r['tree'])));
    }

    public function test_the_mapper_hands_a_designed_row_to_the_compiler(): void
    {
        $outline = [self::columnsNode()];

        $this->assertNotSame(self::bytes(self::map($outline, null)), self::bytes(self::map($outline, self::compiler())));
    }

    public function test_a_plain_outline_keeps_its_rows_and_groups(): void
    {
        $plain = self::map([['type' => 'group', 'children' => [self::columnsNode()]]], null);

        $this->assertArrayHasKey('tree', $plain);
        $this->assertSame(['content_width' => 'boxed', 'flex_direction' => 'column'], $plain['tree'][0]['settings']);
        $this->assertFalse($plain['tree'][0]['isInner']);
        $row = $plain['tree'][0]['elements'][0];
        $this->assertSame(['unit' => 'px', 'size' => '24', 'column' => '24', 'row' => '24'], $row['settings']['flex_gap']);
        $this->assertSame(['unit' => '%', 'size' => '55', 'sizes' => []], $row['elements'][0]['settings']['width']);
    }
}
