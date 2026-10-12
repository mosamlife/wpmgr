<?php
/**
 * The section container of an outline version 3 page: anchor, width, tag,
 * direction, tone background, padding per breakpoint and the Navigator label,
 * compiled byte for byte as the frozen round-trip probes store them.
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
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler
 */
final class SectionCompileTest extends TestCase
{
    /** The breakpoints a stock Elementor site has active, smallest first. */
    private const STOCK_BREAKPOINTS = ['mobile', 'tablet'];

    protected function set_up(): void
    {
        parent::set_up();
        Monkey\setUp();
        Functions\when('add_filter')->justReturn(true);
        Functions\when('remove_filter')->justReturn(true);
        // The sanitiser leaves a value with no markup unchanged.
        Functions\when('wp_kses_post')->returnArg();
    }

    protected function tear_down(): void
    {
        Monkey\tearDown();
        parent::tear_down();
    }

    /**
     * A valid design whose surface is the colour the section probe paints.
     *
     * @param array<string,mixed> $override Top-level keys to replace.
     * @return array<string,mixed>
     */
    private static function design(array $override = []): array
    {
        return array_replace([
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
            'density' => 'normal',
        ], $override);
    }

    /**
     * @param array<string,mixed> $design      Design to compile under.
     * @param list<string>|null   $breakpoints Active breakpoints; null when none is.
     * @return ElementorSectionCompiler
     */
    private static function compiler(array $design = [], ?array $breakpoints = self::STOCK_BREAKPOINTS): ElementorSectionCompiler
    {
        return new ElementorSectionCompiler(DesignTokens::fromDesign($design === [] ? self::design() : $design), $breakpoints);
    }

    /**
     * The section the layout-section-container probe compiles from.
     *
     * @param array<string,mixed> $override Keys to replace.
     * @return array<string,mixed>
     */
    private static function section(array $override = []): array
    {
        return array_replace([
            'type'     => 'section',
            'anchor'   => 'features',
            'width'    => 'boxed',
            'tone'     => 'surface',
            'spacing'  => 'lg',
            'children' => [['type' => 'heading', 'level' => 2, 'text' => 'Why teams switch']],
        ], $override);
    }

    /**
     * @param array<string,mixed> $settings Settings.
     * @return string The bytes the settings encode to.
     */
    private static function bytes(array $settings): string
    {
        return json_encode($settings, JSON_THROW_ON_ERROR);
    }

    // -------------------------------------------------------------------------
    // The frozen probes
    // -------------------------------------------------------------------------

    public function test_section_surface_lg_is_the_layout_section_container_probe(): void
    {
        $settings = self::compiler()->settings(self::section());

        $this->assertSame(self::bytes(RoundTripProbes::settings('layout-section-container')), self::bytes($settings));
    }

    public function test_section_surface_xl_steps_the_padding_down_as_the_hero_probe_does(): void
    {
        $hero     = RoundTripProbes::settings('layout-hero-background');
        $expected = RoundTripProbes::settings('layout-section-container');
        foreach (['padding', 'padding_tablet', 'padding_mobile'] as $key) {
            $expected[$key] = $hero[$key];
        }

        $settings = self::compiler()->settings(self::section(['spacing' => 'xl']));

        $this->assertSame(self::bytes($expected), self::bytes($settings));
        $this->assertSame('128', $settings['padding']['top']);
        $this->assertSame('96', $settings['padding_tablet']['bottom']);
        $this->assertSame('72', $settings['padding_mobile']['top']);
    }

    public function test_a_full_width_section_opens_as_the_hero_probe_does(): void
    {
        $hero     = RoundTripProbes::settings('layout-hero-background');
        $settings = self::compiler()->settings(self::section(['anchor' => 'hero', 'width' => 'full', 'spacing' => 'xl']));

        $this->assertSame(
            self::bytes(array_slice($hero, 0, 4, true)),
            self::bytes(array_slice($settings, 0, 4, true)),
            'content_width, html_tag, flex_direction, _element_id'
        );
    }

    public function test_the_container_carries_the_envelope_in_elementors_key_order(): void
    {
        $child     = ['id' => 'abcdef0', 'elType' => 'widget', 'settings' => [], 'elements' => [], 'widgetType' => 'divider'];
        $container = self::compiler()->container('1a70001', self::section(), [$child]);

        $this->assertSame(['id', 'elType', 'settings', 'elements', 'isInner'], array_keys($container));
        $this->assertSame('1a70001', $container['id']);
        $this->assertSame('container', $container['elType']);
        $this->assertFalse($container['isInner']);
        $this->assertSame([$child], $container['elements']);
        $this->assertSame(self::bytes(RoundTripProbes::settings('layout-section-container')), self::bytes($container['settings']));
    }

    // -------------------------------------------------------------------------
    // Tone, defaults, density, breakpoints
    // -------------------------------------------------------------------------

    /**
     * @return array<string,array{0:string,1:string}>
     */
    public static function tones(): array
    {
        return [
            'ground'  => ['ground', '#FFFFFF'],
            'surface' => ['surface', '#F6F5F2'],
            'inverse' => ['inverse', '#15171C'],
            'accent'  => ['accent', '#0D7A63'],
        ];
    }

    /**
     * @dataProvider tones
     */
    public function test_the_background_is_the_tones_colour_with_its_activator(string $tone, string $hex): void
    {
        $settings = self::compiler()->settings(self::section(['tone' => $tone]));

        $this->assertSame('classic', $settings['background_background']);
        $this->assertSame($hex, $settings['background_color']);
        $keys = array_keys($settings);
        $this->assertLessThan(
            (int) array_search('background_color', $keys, true),
            (int) array_search('background_background', $keys, true),
            'the activator is written before the colour it enables'
        );
    }

    public function test_a_section_without_options_is_boxed_ground_and_large(): void
    {
        $settings = self::compiler()->settings(['type' => 'section', 'anchor' => 'story', 'children' => [['type' => 'separator']]]);

        $this->assertSame('boxed', $settings['content_width']);
        $this->assertSame('#FFFFFF', $settings['background_color']);
        $this->assertSame('96', $settings['padding']['top']);
        $this->assertSame('56', $settings['padding_mobile']['bottom']);
    }

    /**
     * @return array<string,array{0:string,1:string,2:array{string,string,string}}>
     */
    public static function densities(): array
    {
        return [
            'compact' => ['compact', 'xl', ['96', '72', '54']],
            'normal'  => ['normal', 'xl', ['128', '96', '72']],
            'airy'    => ['airy', 'xl', ['160', '120', '90']],
            'airy md' => ['airy', 'md', ['80', '70', '50']],
        ];
    }

    /**
     * @dataProvider densities
     * @param array{string,string,string} $tops Top padding per breakpoint.
     */
    public function test_density_scales_the_vertical_padding_and_not_the_side_gutter(string $density, string $spacing, array $tops): void
    {
        $settings = self::compiler(self::design(['density' => $density]))->settings(self::section(['spacing' => $spacing]));

        $this->assertSame($tops[0], $settings['padding']['top']);
        $this->assertSame($tops[0], $settings['padding']['bottom']);
        $this->assertSame($tops[1], $settings['padding_tablet']['top']);
        $this->assertSame($tops[2], $settings['padding_mobile']['top']);
        foreach (['padding', 'padding_tablet', 'padding_mobile'] as $key) {
            $this->assertSame('24', $settings[$key]['left'], $key);
            $this->assertSame('24', $settings[$key]['right'], $key);
            $this->assertSame('px', $settings[$key]['unit'], $key);
            $this->assertFalse($settings[$key]['isLinked'], $key);
        }
    }

    /**
     * @return array<string,array{0:list<string>|null,1:list<string>}>
     */
    public static function breakpoints(): array
    {
        return [
            'null means none active'   => [null, []],
            'none'                     => [[], []],
            'tablet only'              => [['tablet'], ['padding_tablet']],
            'mobile only'              => [['mobile'], ['padding_mobile']],
            'stock'                    => [['mobile', 'tablet'], ['padding_tablet', 'padding_mobile']],
            'extra breakpoints'        => [['mobile', 'mobile_extra', 'tablet', 'tablet_extra', 'laptop', 'widescreen'], ['padding_tablet', 'padding_mobile']],
        ];
    }

    /**
     * @dataProvider breakpoints
     * @param list<string>|null $active   Active breakpoints.
     * @param list<string>      $expected The override keys written, in order.
     */
    public function test_an_override_is_written_only_for_a_breakpoint_the_site_has_active(?array $active, array $expected): void
    {
        $settings = self::compiler([], $active)->settings(self::section());

        $this->assertSame(['padding'], array_values(array_intersect(array_keys($settings), ['padding'])));
        $this->assertSame($expected, array_values(array_filter(
            array_keys($settings),
            static fn (string $key): bool => str_starts_with($key, 'padding_')
        )));
    }

    // -------------------------------------------------------------------------
    // What the compiler will not write
    // -------------------------------------------------------------------------

    /**
     * @return array<string,array{0:array<string,mixed>}>
     */
    public static function refused(): array
    {
        return [
            'tone outside the four'    => [['tone' => 'neon']],
            'spacing outside the three' => [['spacing' => 'xxl']],
            'width outside the two'    => [['width' => 'wide']],
            'tone not a string'        => [['tone' => 3]],
            'no anchor'                => [['anchor' => null]],
            'anchor with a capital'    => [['anchor' => 'Features']],
            'anchor with a quote'      => [['anchor' => 'a"b']],
            'anchor with a newline'    => [['anchor' => "features\n"]],
            'reserved anchor'          => [['anchor' => 'content']],
        ];
    }

    /**
     * @dataProvider refused
     * @param array<string,mixed> $override What the section gets instead.
     */
    public function test_a_section_the_grammar_would_have_refused_is_not_compiled(array $override): void
    {
        $this->expectException(\InvalidArgumentException::class);

        self::compiler()->settings(self::section($override));
    }

    // -------------------------------------------------------------------------
    // The policy that judges the tree
    // -------------------------------------------------------------------------

    public function test_the_mapper_allowlist_names_the_anchor_the_tag_and_the_label(): void
    {
        $allowed = ElementorClassicMapper::ALLOWED_KEYS['container'];

        $this->assertArrayHasKey(LeafPolicy::ANCHOR_KEY, $allowed);
        $this->assertArrayHasKey(LeafPolicy::LABEL_KEY, $allowed);
        $this->assertArrayHasKey('html_tag', $allowed);
    }

    public function test_the_label_and_anchor_rules_take_effect_on_the_compiled_container(): void
    {
        $settings = self::compiler()->settings(self::section());
        // The style keys are judged by the per-key shape pins; the structure is judged here.
        $structure = array_intersect_key($settings, array_flip(['content_width', 'html_tag', 'flex_direction', '_element_id', '_title']));
        $make      = static fn (string $id, array $s): array => ['id' => $id, 'elType' => 'container', 'settings' => $s, 'elements' => [], 'isInner' => false];
        $check     = static fn (array $tree): ?array => LeafPolicy::checkTree($tree, ElementorClassicMapper::ALLOWED_KEYS, ElementorClassicMapper::LEAF_RULES);

        $this->assertNull($check([$make('1a70001', $structure)]));

        $unlabelled = $structure;
        unset($unlabelled['_title']);
        $refusal = $check([$make('1a70001', $unlabelled)]);
        $this->assertNotNull($refusal, 'an anchor on a container without the section label is refused');
        $this->assertSame(LeafPolicy::CODE_KEY, $refusal['code']);

        $this->assertNotNull($check([$make('1a70001', $structure), $make('1a70002', $structure)]), 'two sections cannot share an anchor');

        $outer             = $make('1a70001', ['content_width' => 'boxed']);
        $outer['elements'] = [$make('1a70002', $structure)];
        $this->assertNotNull($check([$outer]), 'an anchor is written on a top level section only');
    }
}
