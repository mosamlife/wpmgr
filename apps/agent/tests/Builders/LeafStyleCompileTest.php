<?php
/**
 * The look of a designed page's headings and text: typography literals with
 * their activator and the tone's colours, compiled byte for byte as the
 * frozen round-trip probes store them.
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
final class LeafStyleCompileTest extends TestCase
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
     * The design the typography probes were measured under: 16 px at a
     * major third, Fraunces over Work Sans, and the six colours they paint.
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
     * @param array<string,mixed> $design Design to compile under; the probe design when empty.
     * @return ElementorLeafStyle
     */
    private static function style(array $design = []): ElementorLeafStyle
    {
        return new ElementorLeafStyle(DesignTokens::fromDesign($design === [] ? self::design() : $design));
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
     * The settings of one widget of a probe's containers tree.
     *
     * @param string $probe Probe name.
     * @param int    $index Position of the widget in the probe's first container.
     * @return array<string,mixed>
     */
    private static function probeWidget(string $probe, int $index = 0): array
    {
        $settings = RoundTripProbes::tree($probe)[0]['elements'][$index]['settings'] ?? null;
        if (!is_array($settings)) {
            throw new \RuntimeException('probe ' . $probe . ' has no widget ' . $index);
        }

        return $settings;
    }

    // -------------------------------------------------------------------------
    // typography-ramp: the frozen probes
    // -------------------------------------------------------------------------

    public function test_typography_ramp_display_heading_is_the_style_heading_typography_probe(): void
    {
        $settings = array_merge(
            ['title' => 'Designed pages start with a type ramp', 'header_size' => 'h1'],
            self::style()->headingStyle(1, 'ground', 'start')
        );

        $this->assertSame(self::bytes(self::probeWidget('style-heading-typography')), self::bytes($settings));
    }

    public function test_typography_ramp_body_lead_and_eyebrow_are_the_style_text_editor_probe(): void
    {
        $style = self::style();

        $body = array_merge(['editor' => '<p>Every section reads the same way: one idea and one line of proof.</p>'], $style->textStyle('body', 'ground', 'start'));
        $lead = array_merge(['editor' => '<p>The lead paragraph sits in the inverse tone.</p>'], $style->textStyle('lead', 'inverse', 'start'));
        $eyebrow = array_merge(['editor' => '<p>Eyebrow label</p>'], $style->textStyle('eyebrow', 'ground', 'start'));

        $this->assertSame(self::bytes(self::probeWidget('style-text-editor', 0)), self::bytes($body));
        $this->assertSame(self::bytes(self::probeWidget('style-text-editor', 1)), self::bytes($lead));
        $this->assertSame(self::bytes(self::probeWidget('style-text-editor', 2)), self::bytes($eyebrow));
    }

    // -------------------------------------------------------------------------
    // The ramp
    // -------------------------------------------------------------------------

    public function test_heading_levels_take_display_h2_and_h3_of_the_ramp(): void
    {
        $style = self::style();
        $h2    = $style->headingStyle(2, 'ground', 'start');
        $h3    = $style->headingStyle(3, 'ground', 'start');
        $h4    = $style->headingStyle(4, 'ground', 'start');

        $this->assertSame(['unit' => 'px', 'size' => '31'], $h2['typography_font_size']);
        $this->assertSame('700', $h2['typography_font_weight']);
        $this->assertSame(['unit' => 'em', 'size' => '1.1'], $h2['typography_line_height']);
        $this->assertArrayNotHasKey('typography_letter_spacing', $h2);

        $this->assertSame(['unit' => 'px', 'size' => '25'], $h3['typography_font_size']);
        $this->assertSame('600', $h3['typography_font_weight']);
        $this->assertSame(['unit' => 'em', 'size' => '1.15'], $h3['typography_line_height']);
        $this->assertSame(self::bytes($h3), self::bytes($h4), 'levels 3 and 4 share the H3 style');
    }

    public function test_every_typography_group_comes_with_its_activator_first(): void
    {
        $style = self::style();
        foreach ([$style->headingStyle(2, 'ground', 'start'), $style->textStyle('body', 'ground', 'start'), $style->textStyle('eyebrow', 'ground', 'start')] as $settings) {
            $typography = array_values(array_filter(array_keys($settings), static fn (string $key): bool => strncmp($key, 'typography_', 11) === 0));
            $this->assertSame('typography_typography', $typography[0]);
            $this->assertSame('custom', $settings['typography_typography']);
        }
    }

    public function test_the_system_font_writes_no_family(): void
    {
        $style = self::style(self::design(['fonts' => ['heading' => 'system', 'body' => 'system']]));

        $this->assertArrayNotHasKey('typography_font_family', $style->headingStyle(1, 'ground', 'start'));
        $this->assertArrayNotHasKey('typography_font_family', $style->textStyle('body', 'ground', 'start'));
        $this->assertArrayHasKey('typography_font_size', $style->textStyle('body', 'ground', 'start'));
    }

    public function test_a_family_is_written_as_the_design_names_it(): void
    {
        $style = self::style(self::design(['fonts' => ['heading' => 'Playfair Display', 'body' => 'Work Sans']]));

        $this->assertSame('Playfair Display', $style->headingStyle(2, 'ground', 'start')['typography_font_family']);
        $this->assertSame('Work Sans', $style->textStyle('lead', 'ground', 'start')['typography_font_family']);
    }

    public function test_the_scale_follows_the_design(): void
    {
        $style = self::style(self::design(['scale' => ['base_px' => 18, 'ratio' => 'minor_third']]));

        // 18 x 1.2^5 = 44.79 and 18 x 0.8 = 14.4: whole pixels, a half rounded up.
        $this->assertSame('45', $style->headingStyle(1, 'ground', 'start')['typography_font_size']['size']);
        $this->assertSame('18', $style->textStyle('body', 'ground', 'start')['typography_font_size']['size']);
        $this->assertSame('14', $style->textStyle('eyebrow', 'ground', 'start')['typography_font_size']['size']);
    }

    // -------------------------------------------------------------------------
    // Tone and alignment
    // -------------------------------------------------------------------------

    public function test_the_tone_picks_the_colours(): void
    {
        $style = self::style();

        $this->assertSame('#15171C', $style->headingStyle(2, 'ground', 'start')['title_color']);
        $this->assertSame('#15171C', $style->headingStyle(2, 'surface', 'start')['title_color']);
        $this->assertSame('#F6F5F2', $style->headingStyle(2, 'inverse', 'start')['title_color']);
        $this->assertSame('#FFFFFF', $style->headingStyle(2, 'accent', 'start')['title_color']);

        $this->assertSame('#5B616B', $style->textStyle('body', 'surface', 'start')['text_color']);
        $this->assertSame('rgba(246,245,242,0.8)', $style->textStyle('body', 'inverse', 'start')['text_color']);
        $this->assertSame('#FFFFFF', $style->textStyle('body', 'accent', 'start')['text_color']);
        $this->assertSame('#0D7A63', $style->textStyle('eyebrow', 'surface', 'start')['text_color']);
        $this->assertSame('#FFFFFF', $style->textStyle('eyebrow', 'accent', 'start')['text_color'], 'on the accent tone the accent of the section is on_accent');
    }

    public function test_a_centred_section_aligns_the_heading_and_the_text_and_a_start_section_aligns_the_heading_only(): void
    {
        $style = self::style();

        $this->assertSame('left', $style->headingStyle(2, 'ground', 'start')['align']);
        $this->assertSame('center', $style->headingStyle(2, 'ground', 'center')['align']);
        $this->assertArrayNotHasKey('align', $style->textStyle('body', 'ground', 'start'));
        $this->assertSame('center', $style->textStyle('body', 'ground', 'center')['align']);
        $this->assertSame(['align', 'text_color'], array_slice(array_keys($style->textStyle('lead', 'ground', 'center')), 0, 2));
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
            'level 0'       => [static fn (ElementorLeafStyle $s) => $s->headingStyle(0, 'ground', 'start')],
            'level 5'       => [static fn (ElementorLeafStyle $s) => $s->headingStyle(5, 'ground', 'start')],
            'heading tone'  => [static fn (ElementorLeafStyle $s) => $s->headingStyle(2, 'night', 'start')],
            'heading align' => [static fn (ElementorLeafStyle $s) => $s->headingStyle(2, 'ground', 'right')],
            'text style'    => [static fn (ElementorLeafStyle $s) => $s->textStyle('display', 'ground', 'start')],
            'text tone'     => [static fn (ElementorLeafStyle $s) => $s->textStyle('body', 'night', 'start')],
            'text align'    => [static fn (ElementorLeafStyle $s) => $s->textStyle('body', 'ground', 'left')],
        ];
    }

    // -------------------------------------------------------------------------
    // The leaf policy
    // -------------------------------------------------------------------------

    public function test_every_key_the_styles_write_is_listed_with_a_pin_the_leaf_policy_accepts(): void
    {
        $style = self::style();
        $tree  = [[
            'id'       => '1a70001',
            'elType'   => 'container',
            'settings' => [],
            'elements' => [
                ['id' => '1a70002', 'elType' => 'widget', 'settings' => array_merge(['title' => 'Plain', 'header_size' => 'h1'], $style->headingStyle(1, 'inverse', 'center')), 'elements' => [], 'widgetType' => 'heading'],
                ['id' => '1a70003', 'elType' => 'widget', 'settings' => array_merge(['editor' => '<p>Plain</p>'], $style->textStyle('eyebrow', 'accent', 'center')), 'elements' => [], 'widgetType' => 'text-editor'],
                ['id' => '1a70004', 'elType' => 'widget', 'settings' => array_merge(['editor' => '<p>Plain</p>'], $style->textStyle('lead', 'inverse', 'start')), 'elements' => [], 'widgetType' => 'text-editor'],
            ],
            'isInner'  => false,
        ]];
        $allowed = ElementorClassicMapper::ALLOWED_KEYS;
        foreach (['heading', 'text-editor'] as $type) {
            $allowed[$type] = array_merge($allowed[$type], ElementorLeafStyle::KEYS[$type]);
        }

        $this->assertNull(LeafPolicy::checkTree($tree, $allowed, ElementorClassicMapper::LEAF_RULES));
    }

    public function test_the_leaf_policy_refuses_a_style_whose_value_leaves_its_pin(): void
    {
        $style    = self::style();
        $settings = array_merge(['title' => 'Plain', 'header_size' => 'h1'], $style->headingStyle(1, 'ground', 'start'));
        $settings['title_color'] = 'rgb(21,23,28)';
        $tree = [['id' => '1a70002', 'elType' => 'widget', 'settings' => $settings, 'elements' => [], 'widgetType' => 'heading']];
        $allowed = ['heading' => array_merge(ElementorClassicMapper::ALLOWED_KEYS['heading'], ElementorLeafStyle::KEYS['heading'])];

        $refusal = LeafPolicy::checkTree($tree, $allowed, ElementorClassicMapper::LEAF_RULES);

        $this->assertNotNull($refusal);
        $this->assertStringContainsString('title_color', $refusal['detail']);
    }
}
