<?php
/**
 * The leaves of a designed section built through the mapper: headings and
 * text in the design's look, the lead after the title, and a plain outline
 * left exactly as it was.
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
use WPMgr\Agent\Abilities\Builders\IdSeed;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorClassicMapper
 */
final class DesignedLeavesTest extends TestCase
{
    private const REQUEST = '11111111-2222-4333-8444-777777777777';

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
     * @return ElementorLeafStyle The look of the design the typography probes were measured under.
     */
    private static function look(): ElementorLeafStyle
    {
        return new ElementorLeafStyle(DesignTokens::fromDesign([
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
        ]));
    }

    /**
     * @param list<array<string,mixed>> $leaves Leaves of the section.
     * @param string                    $tone   Tone of the section.
     * @param string                    $align  Alignment of the section.
     * @return array<string,mixed>
     */
    private static function build(array $leaves, string $tone = 'ground', string $align = 'start'): array
    {
        return ElementorClassicMapper::mapDesignedLeaves($leaves, new IdSeed(self::REQUEST), [], self::look(), $tone, $align, 'outline[0].children');
    }

    /**
     * @param array<string,mixed> $built A successful build.
     * @return list<array<string,mixed>>
     */
    private static function widgets(array $built): array
    {
        self::assertArrayHasKey('tree', $built, (string) ($built['detail'] ?? 'no tree'));

        return $built['tree'];
    }

    public function test_a_hero_title_and_its_lead_are_built_in_the_ramp(): void
    {
        $widgets = self::widgets(self::build([
            ['type' => 'heading', 'level' => 1, 'text' => 'Designed pages start with a type ramp'],
            ['type' => 'paragraph', 'text' => 'The first paragraph after the title is the lead.'],
            ['type' => 'paragraph', 'text' => 'The next one is body text.'],
        ], 'ground', 'start'));

        $this->assertSame(['heading', 'text-editor', 'text-editor'], array_column($widgets, 'widgetType'));
        $this->assertSame('h1', $widgets[0]['settings']['header_size']);
        $this->assertSame('49', $widgets[0]['settings']['typography_font_size']['size']);
        $this->assertSame('20', $widgets[1]['settings']['typography_font_size']['size'], 'the lead');
        $this->assertSame('16', $widgets[2]['settings']['typography_font_size']['size'], 'the body');
        $this->assertSame('<p>The first paragraph after the title is the lead.</p>', $widgets[1]['settings']['editor']);
    }

    public function test_a_paragraph_with_no_title_before_it_is_body_text(): void
    {
        $widgets = self::widgets(self::build([
            ['type' => 'heading', 'level' => 2, 'text' => 'Why teams switch'],
            ['type' => 'paragraph', 'text' => 'Plain body text.'],
        ]));

        $this->assertSame('h2', $widgets[0]['settings']['header_size']);
        $this->assertSame('31', $widgets[0]['settings']['typography_font_size']['size']);
        $this->assertSame('16', $widgets[1]['settings']['typography_font_size']['size']);
    }

    public function test_the_section_tone_and_alignment_reach_every_leaf(): void
    {
        $widgets = self::widgets(self::build([
            ['type' => 'heading', 'level' => 2, 'text' => 'On the ink'],
            ['type' => 'paragraph', 'text' => 'Light text on dark.'],
        ], 'inverse', 'center'));

        $this->assertSame('#F6F5F2', $widgets[0]['settings']['title_color']);
        $this->assertSame('center', $widgets[0]['settings']['align']);
        $this->assertSame('rgba(246,245,242,0.8)', $widgets[1]['settings']['text_color']);
        $this->assertSame('center', $widgets[1]['settings']['align']);
    }

    public function test_a_list_a_quote_and_a_table_are_body_text(): void
    {
        $widgets = self::widgets(self::build([
            ['type' => 'list', 'ordered' => false, 'items' => ['One', 'Two']],
            ['type' => 'quote', 'paragraphs' => ['Said once.'], 'citation' => 'Someone'],
            ['type' => 'table', 'header' => ['Plan', 'Price'], 'rows' => [['Free', '0']]],
        ], 'inverse', 'start'));

        $this->assertCount(3, $widgets);
        foreach ($widgets as $widget) {
            $this->assertSame('text-editor', $widget['widgetType']);
            $this->assertSame('rgba(246,245,242,0.8)', $widget['settings']['text_color']);
            $this->assertSame('16', $widget['settings']['typography_font_size']['size']);
        }
    }

    public function test_the_ids_are_the_same_on_every_run_and_each_leaf_has_its_own(): void
    {
        $leaves = [
            ['type' => 'heading', 'level' => 1, 'text' => 'Title'],
            ['type' => 'paragraph', 'text' => 'Lead.'],
        ];
        $first  = self::widgets(self::build($leaves));
        $second = self::widgets(self::build($leaves));

        $this->assertSame(array_column($first, 'id'), array_column($second, 'id'));
        $this->assertCount(2, array_unique(array_column($first, 'id')));
        $this->assertMatchesRegularExpression('/^[0-9a-f]{7}$/D', $first[0]['id']);
    }

    public function test_a_plain_outline_still_refuses_a_level_1_heading_and_keeps_its_bytes(): void
    {
        $spec = ['outline' => [
            ['type' => 'heading', 'level' => 1, 'text' => 'Title'],
        ]];
        $refused = ElementorClassicMapper::map($spec, new IdSeed(self::REQUEST), [], true);

        $this->assertSame('bad_input', $refused['code'] ?? null);
        $this->assertSame('outline[0]: a heading needs a level of 2, 3 or 4 and a text', $refused['detail'] ?? null);

        $plain = ElementorClassicMapper::map(['outline' => [
            ['type' => 'heading', 'level' => 2, 'text' => 'Title'],
            ['type' => 'paragraph', 'text' => 'Body.'],
        ]], new IdSeed(self::REQUEST), [], true);

        $this->assertArrayHasKey('tree', $plain);
        $leaves = $plain['tree'][0]['elements'];
        $this->assertSame(['title', 'header_size'], array_keys($leaves[0]['settings']));
        $this->assertSame(['editor'], array_keys($leaves[1]['settings']));
    }

    public function test_the_designed_build_keeps_the_page_create_text_rules(): void
    {
        $built = self::build([
            ['type' => 'paragraph', 'text' => 'https://example.com/watch'],
        ]);

        $this->assertSame('create_content_invalid', $built['code'] ?? null);
        $this->assertStringStartsWith('outline[0].children[0].text:', (string) ($built['detail'] ?? ''));
    }

    public function test_a_tone_or_alignment_the_grammar_does_not_know_is_refused(): void
    {
        $leaves = [['type' => 'heading', 'level' => 2, 'text' => 'Title']];

        $this->assertSame('bad_input', self::build($leaves, 'night')['code'] ?? null);
        $this->assertSame('bad_input', self::build($leaves, 'ground', 'right')['code'] ?? null);
        $this->assertSame('bad_input', self::build([])['code'] ?? null);
    }

    public function test_a_group_or_columns_cannot_be_built_as_a_leaf(): void
    {
        $built = self::build([
            ['type' => 'group', 'children' => [['type' => 'paragraph', 'text' => 'Nested.']]],
        ]);

        $this->assertSame('layout_invalid', $built['code'] ?? null);
    }

    // -------------------------------------------------------------------------
    // Buttons
    // -------------------------------------------------------------------------

    /**
     * @param string $align Alignment of the buttons node.
     * @return array<string,mixed>
     */
    private static function threeButtons(string $align = 'left'): array
    {
        return ['type' => 'buttons', 'align' => $align, 'buttons' => [
            ['text' => 'Start free', 'url' => '#signup', 'style' => 'fill'],
            ['text' => 'See pricing', 'url' => '#pricing', 'style' => 'outline'],
            ['text' => 'Learn more', 'url' => '#features', 'style' => 'text'],
        ]];
    }

    public function test_buttons_of_every_style_are_built_in_one_row(): void
    {
        $built = self::widgets(self::build([self::threeButtons()]));

        $this->assertCount(1, $built);
        $row = $built[0];
        $this->assertSame('container', $row['elType']);
        $this->assertTrue($row['isInner']);
        $this->assertSame('row', $row['settings']['flex_direction']);
        $this->assertArrayNotHasKey('flex_justify_content', $row['settings']);
        $this->assertSame(['button', 'button', 'button'], array_column($row['elements'], 'widgetType'));
        $this->assertSame(['Start free', 'See pricing', 'Learn more'], array_column(array_column($row['elements'], 'settings'), 'text'));
        $this->assertSame('classic', $row['elements'][0]['settings']['background_background']);
        $this->assertSame('solid', $row['elements'][1]['settings']['border_border']);
        $this->assertSame('underline', $row['elements'][2]['settings']['typography_text_decoration']);
        $this->assertArrayNotHasKey('align', $row['elements'][0]['settings']);
        $this->assertSame(['#signup', '#pricing', '#features'], array_column(array_column(array_column($row['elements'], 'settings'), 'link'), 'url'));
    }

    public function test_centred_buttons_are_centred_in_their_row_and_the_row_comes_first_in_the_ids(): void
    {
        $built = self::widgets(self::build([self::threeButtons('center')]));
        $ids   = array_merge([$built[0]['id']], array_column($built[0]['elements'], 'id'));

        $this->assertSame('center', $built[0]['settings']['flex_justify_content']);
        $this->assertCount(4, array_unique($ids));
        foreach ($ids as $id) {
            $this->assertMatchesRegularExpression('/^[0-9a-f]{7}$/D', $id);
        }
        $again = self::widgets(self::build([self::threeButtons('center')]));
        $this->assertSame($ids, array_merge([$again[0]['id']], array_column($again[0]['elements'], 'id')));
    }

    public function test_a_button_takes_the_tone_of_its_section(): void
    {
        $built = self::widgets(self::build([self::threeButtons()], 'accent'));

        $this->assertSame('#FFFFFF', $built[0]['elements'][0]['settings']['background_color']);
        $this->assertSame('#0D7A63', $built[0]['elements'][0]['settings']['button_text_color']);
    }

    public function test_the_outline_refusal_is_lifted_for_a_designed_section_and_kept_for_a_plain_outline(): void
    {
        $designed = self::build([self::threeButtons()]);
        $this->assertArrayHasKey('tree', $designed, (string) ($designed['detail'] ?? ''));

        $plain = ElementorClassicMapper::map(['outline' => [self::threeButtons()]], new IdSeed(self::REQUEST), [], true);

        $this->assertSame('node_not_supported_by_builder', $plain['code'] ?? null);
        $this->assertSame(
            'outline[0].buttons[1].style: an outline button is not available in Elementor; the button styles it builds are: fill',
            $plain['detail'] ?? null
        );
    }

    public function test_a_plain_outline_button_keeps_its_bytes(): void
    {
        $plain = ElementorClassicMapper::map(['outline' => [
            ['type' => 'buttons', 'align' => 'center', 'buttons' => [['text' => 'Go', 'url' => 'https://example.com/go', 'style' => 'fill']]],
        ]], new IdSeed(self::REQUEST), [], true);

        $this->assertArrayHasKey('tree', $plain);
        $button = $plain['tree'][0]['elements'][0];
        $this->assertSame('button', $button['widgetType']);
        $this->assertSame(['text', 'link', 'align'], array_keys($button['settings']));
    }

    public function test_a_designed_button_keeps_the_link_rules_and_knows_only_three_styles(): void
    {
        $andLink = self::build([['type' => 'buttons', 'align' => 'left', 'buttons' => [['text' => 'Go', 'url' => 'https://example.com/?a=1&b=2', 'style' => 'fill']]]]);
        $ghost   = self::build([['type' => 'buttons', 'align' => 'left', 'buttons' => [['text' => 'Go', 'url' => '#go', 'style' => 'ghost']]]]);
        $noStyle = self::widgets(self::build([['type' => 'buttons', 'align' => 'left', 'buttons' => [['text' => 'Go', 'url' => '#go']]]]));

        $this->assertSame('link_invalid', $andLink['code'] ?? null);
        $this->assertSame('bad_input', $ghost['code'] ?? null);
        $this->assertSame('outline[0].children[0].buttons[0].style: a button style is fill, outline or text', $ghost['detail'] ?? null);
        $this->assertSame('classic', $noStyle[0]['elements'][0]['settings']['background_background'], 'a button with no style is a fill');
        $this->assertArrayNotHasKey('border_border', $noStyle[0]['elements'][0]['settings']);
    }
}
