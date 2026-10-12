<?php
/**
 * The look of a designed page's buttons: fill, outline and text, with their
 * hover, border, radius, size and Button typography, compiled byte for byte
 * as the frozen round-trip probes store them.
 *
 * @package WPMgr\Agent\Tests\Builders
 */

declare(strict_types=1);

namespace WPMgr\Agent\Tests\Builders;

use Brain\Monkey;
use Brain\Monkey\Functions;
use WPMgr\Agent\Abilities\Builders\DesignTokens;
use WPMgr\Agent\Abilities\Builders\ElementorClassicMapper;
use WPMgr\Agent\Abilities\Builders\ElementorLeafStyle;
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorLeafStyle
 */
final class ButtonStyleCompileTest extends TestCase
{
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
     * The design the button probes were measured under.
     *
     * @param array<string,mixed> $override Top-level keys to replace.
     * @return array<string,mixed>
     */
    private static function design(array $override = []): array
    {
        return array_replace([
            'colors'  => [
                'ground'    => '#F6F5F2',
                'surface'   => '#EDEBE6',
                'ink'       => '#15171C',
                'muted'     => '#5B616B',
                'accent'    => '#0D7A63',
                'on_accent' => '#FFFFFF',
            ],
            'fonts'   => ['heading' => 'Fraunces', 'body' => 'Work Sans'],
            'scale'   => ['base_px' => 16, 'ratio' => 'major_third'],
            'radius'  => 'md',
            'density' => 'normal',
        ], $override);
    }

    /**
     * @param array<string,mixed> $override Top-level design keys to replace.
     * @return ElementorLeafStyle
     */
    private static function style(array $override = []): ElementorLeafStyle
    {
        return new ElementorLeafStyle(DesignTokens::fromDesign(self::design($override)));
    }

    /**
     * @param array<string,mixed> $settings Settings.
     * @return string The bytes the settings encode to.
     */
    private static function bytes(array $settings): string
    {
        return json_encode($settings, JSON_THROW_ON_ERROR);
    }

    /**
     * A button's content keys followed by its style keys.
     *
     * @param string              $text  Button text.
     * @param string              $url   Link address.
     * @param array<string,mixed> $style The style keys.
     * @return array<string,mixed>
     */
    private static function button(string $text, string $url, array $style): array
    {
        return array_merge(['text' => $text, 'link' => ['url' => $url, 'is_external' => '', 'nofollow' => '']], $style);
    }

    /**
     * @param string $probe Probe name.
     * @return array<string,mixed> The settings of the probe's one button.
     */
    private static function probeButton(string $probe): array
    {
        $settings = RoundTripProbes::tree($probe)[0]['elements'][0]['settings'] ?? null;
        if (!is_array($settings)) {
            throw new \RuntimeException('probe ' . $probe . ' has no button');
        }

        return $settings;
    }

    // -------------------------------------------------------------------------
    // buttons-three-styles: the frozen probes
    // -------------------------------------------------------------------------

    public function test_buttons_three_styles_are_the_style_button_probes(): void
    {
        $style = self::style();

        $this->assertSame(
            self::bytes(self::probeButton('style-button-fill')),
            self::bytes(self::button('Start free', '#signup', $style->buttonStyle('fill', 'ground')))
        );
        $this->assertSame(
            self::bytes(self::probeButton('style-button-outline')),
            self::bytes(self::button('See pricing', '#pricing', $style->buttonStyle('outline', 'ground')))
        );
        $this->assertSame(
            self::bytes(self::probeButton('style-button-text')),
            self::bytes(self::button('Learn more', '#features', $style->buttonStyle('text', 'ground')))
        );
    }

    // -------------------------------------------------------------------------
    // What each style is
    // -------------------------------------------------------------------------

    public function test_the_fill_hover_is_a_literal_eight_points_darker_and_nothing_is_bound(): void
    {
        $fill = self::style()->buttonStyle('fill', 'ground');

        $this->assertSame(DesignTokens::hover('#0D7A63'), $fill['button_background_hover_color']);
        $this->assertSame('#095545', $fill['button_background_hover_color']);
        $this->assertArrayNotHasKey('__globals__', $fill);
        $this->assertSame('md', $fill['size']);
        $this->assertSame('', $fill['button_type']);
    }

    public function test_the_outline_draws_its_border_only_with_the_border_activator(): void
    {
        $outline = self::style()->buttonStyle('outline', 'ground');

        $this->assertSame('solid', $outline['border_border']);
        $this->assertSame(['unit' => 'px', 'top' => '1.5', 'right' => '1.5', 'bottom' => '1.5', 'left' => '1.5', 'isLinked' => true], $outline['border_width']);
        $this->assertSame('transparent', $outline['background_color']);
        $keys = array_keys($outline);
        $this->assertLessThan(array_search('border_width', $keys, true), array_search('border_border', $keys, true), 'the activator comes before the sub-keys');
    }

    public function test_the_text_button_is_underlined_at_rest_and_has_no_border_or_fill(): void
    {
        $text = self::style()->buttonStyle('text', 'ground');

        $this->assertSame('underline', $text['typography_text_decoration']);
        $this->assertSame('transparent', $text['background_color']);
        $this->assertSame('transparent', $text['button_background_hover_color']);
        $this->assertArrayNotHasKey('border_border', $text);
        $this->assertArrayNotHasKey('border_width', $text);
        $this->assertSame('#095545', $text['hover_color']);
        $this->assertArrayNotHasKey('typography_text_decoration', self::style()->buttonStyle('fill', 'ground'));
        $this->assertArrayNotHasKey('typography_text_decoration', self::style()->buttonStyle('outline', 'ground'));
    }

    public function test_on_the_accent_tone_a_button_is_drawn_in_on_accent(): void
    {
        $style   = self::style();
        $fill    = $style->buttonStyle('fill', 'accent');
        $outline = $style->buttonStyle('outline', 'accent');
        $text    = $style->buttonStyle('text', 'accent');

        $this->assertSame('#FFFFFF', $fill['background_color']);
        $this->assertSame('#0D7A63', $fill['button_text_color']);
        $this->assertSame(DesignTokens::hover('#FFFFFF'), $fill['button_background_hover_color']);
        $this->assertSame('#0D7A63', $fill['hover_color']);

        $this->assertSame('#FFFFFF', $outline['button_text_color']);
        $this->assertSame('#FFFFFF', $outline['border_color']);
        $this->assertSame('#FFFFFF', $outline['button_background_hover_color']);
        $this->assertSame('#0D7A63', $outline['hover_color']);

        $this->assertSame('#FFFFFF', $text['button_text_color']);
    }

    public function test_the_other_tones_draw_a_button_in_the_accent(): void
    {
        foreach (['ground', 'surface', 'inverse'] as $tone) {
            $fill = self::style()->buttonStyle('fill', $tone);
            $this->assertSame('#0D7A63', $fill['background_color'], $tone);
            $this->assertSame('#FFFFFF', $fill['button_text_color'], $tone);
        }
    }

    public function test_the_radius_and_the_button_typography_follow_the_design(): void
    {
        $this->assertSame('12', self::style()->buttonStyle('fill', 'ground')['border_radius']['top']);
        $this->assertSame('0', self::style(['radius' => 'none'])->buttonStyle('fill', 'ground')['border_radius']['left']);
        $this->assertSame('20', self::style(['radius' => 'lg'])->buttonStyle('outline', 'ground')['border_radius']['bottom']);
        $this->assertTrue(self::style(['radius' => 'sm'])->buttonStyle('text', 'ground')['border_radius']['isLinked']);

        $button = self::style(['scale' => ['base_px' => 20, 'ratio' => 'major_second']])->buttonStyle('fill', 'ground');
        $this->assertSame('19', $button['typography_font_size']['size']);
        $this->assertSame('600', $button['typography_font_weight']);
        $this->assertSame('Work Sans', $button['typography_font_family']);

        $system = self::style(['fonts' => ['heading' => 'system', 'body' => 'system']])->buttonStyle('fill', 'ground');
        $this->assertArrayNotHasKey('typography_font_family', $system);
        $this->assertSame('custom', $system['typography_typography']);
    }

    // -------------------------------------------------------------------------
    // The row
    // -------------------------------------------------------------------------

    public function test_a_row_holds_its_buttons_side_by_side_and_wraps(): void
    {
        $button = ['id' => '1a70002', 'elType' => 'widget', 'settings' => ['text' => 'Go'], 'elements' => [], 'widgetType' => 'button'];
        $row    = self::style()->buttonRow('1a70001', [$button], 'left');

        $this->assertSame(['id', 'elType', 'settings', 'elements', 'isInner'], array_keys($row));
        $this->assertSame('container', $row['elType']);
        $this->assertTrue($row['isInner']);
        $this->assertSame([$button], $row['elements']);
        $this->assertSame(
            ['content_width' => 'full', 'flex_direction' => 'row', 'flex_wrap' => 'wrap', 'flex_gap' => ['column' => '16', 'row' => '16', 'isLinked' => true, 'unit' => 'px']],
            $row['settings']
        );
        $this->assertSame('center', self::style()->buttonRow('1a70001', [$button], 'center')['settings']['flex_justify_content']);
    }

    // -------------------------------------------------------------------------
    // What it refuses to compile
    // -------------------------------------------------------------------------

    /**
     * @dataProvider provide_programming_errors
     *
     * @param callable(ElementorLeafStyle):mixed $call The call that must throw.
     */
    public function test_a_value_the_grammar_does_not_accept_is_a_programming_error(callable $call): void
    {
        $this->expectException(\InvalidArgumentException::class);

        $call(self::style());
    }

    /**
     * @return array<string,array{callable}>
     */
    public static function provide_programming_errors(): array
    {
        return [
            'style'      => [static fn (ElementorLeafStyle $s) => $s->buttonStyle('ghost', 'ground')],
            'tone'       => [static fn (ElementorLeafStyle $s) => $s->buttonStyle('fill', 'night')],
            'row align'  => [static fn (ElementorLeafStyle $s) => $s->buttonRow('1a70001', [], 'right')],
        ];
    }

    // -------------------------------------------------------------------------
    // The leaf policy
    // -------------------------------------------------------------------------

    public function test_every_key_the_buttons_and_the_row_write_passes_the_leaf_policy(): void
    {
        $style   = self::style();
        $widgets = [];
        foreach (['fill', 'outline', 'text'] as $i => $kind) {
            $widgets[] = [
                'id'         => '1a7001' . $i,
                'elType'     => 'widget',
                'settings'   => self::button('Go', '#signup', $style->buttonStyle($kind, 'accent')),
                'elements'   => [],
                'widgetType' => 'button',
            ];
        }
        $tree = [[
            'id'       => '1a70000',
            'elType'   => 'container',
            'settings' => [],
            'elements' => [$style->buttonRow('1a70009', $widgets, 'center')],
            'isInner'  => false,
        ]];
        $allowed = ElementorClassicMapper::ALLOWED_KEYS;
        foreach (['button', 'container'] as $type) {
            $allowed[$type] = array_merge($allowed[$type], ElementorLeafStyle::KEYS[$type]);
        }

        $this->assertNull(LeafPolicy::checkTree($tree, $allowed, ElementorClassicMapper::LEAF_RULES));
    }
}
