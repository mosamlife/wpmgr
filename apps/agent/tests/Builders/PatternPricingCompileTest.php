<?php
/**
 * The pricing pattern of a designed page: one card per plan, composed of a
 * heading, a price, an icon list and a button, in a columns row; the highlighted
 * plan is drawn apart. Never the price table, which needs the paid plugin.
 * The row and its columns are the frozen round-trip probes' columns row and the
 * icon list is the icon-list probe's widget.
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
use WPMgr\Agent\Abilities\Builders\ElementorPatternCards;
use WPMgr\Agent\Abilities\Builders\ElementorPatternCompiler;
use WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorPatternCards
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorPatternCompiler
 */
final class PatternPricingCompileTest extends TestCase
{
    private const STOCK_BREAKPOINTS = ['mobile', 'tablet'];

    private const REQUEST = 'b7e2a4c1-0000-4000-8000-000000000014';

    private const AT = 'outline[2].children[0]';

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

    private static function tokens(): DesignTokens
    {
        return DesignTokens::fromDesign([
            'colors'  => [
                'ground'    => '#FFFFFF',
                'surface'   => '#F6F5F2',
                'ink'       => '#15171C',
                'muted'     => '#5A606C',
                'accent'    => '#0D7A63',
                'on_accent' => '#FAFAF7',
            ],
            'fonts'   => ['heading' => 'Manrope', 'body' => 'Work Sans'],
            'scale'   => ['base_px' => 17, 'ratio' => 'major_third'],
            'radius'  => 'md',
            'density' => 'normal',
        ]);
    }

    /** @param list<string>|null $breakpoints Active breakpoints; null when none is. */
    private static function patterns(?array $breakpoints = self::STOCK_BREAKPOINTS): ElementorPatternCompiler
    {
        return new ElementorPatternCompiler(self::tokens(), new ElementorSectionCompiler(self::tokens(), $breakpoints));
    }

    /** @return list<array<string,mixed>> */
    private static function plans(int $count): array
    {
        $all = [
            ['name' => 'Starter', 'price' => '$19', 'period' => 'per month', 'features' => ['1 site', 'Email support'], 'cta' => ['text' => 'Start', 'url' => '#signup'], 'highlighted' => false],
            ['name' => 'Growth', 'price' => '$49', 'period' => 'per month', 'features' => ['10 sites', 'Priority support', 'Daily backups'], 'cta' => ['text' => 'Choose Growth', 'url' => '#signup'], 'highlighted' => true],
            ['name' => 'Scale', 'price' => '$99', 'features' => ['Unlimited sites', 'Phone support'], 'cta' => ['text' => 'Talk to us', 'url' => 'https://example.com/contact']],
            ['name' => 'Agency', 'price' => '$199', 'period' => 'per month', 'features' => ['Everything', 'White label', 'Service level', 'Training'], 'cta' => ['text' => 'Contact sales', 'url' => '#contact']],
        ];

        return array_slice($all, 0, $count);
    }

    /** @param list<array<string,mixed>> $plans Plans. */
    private static function node(array $plans): array
    {
        return ['type' => 'pattern', 'pattern' => 'pricing', 'variant' => 'three-plans', 'plans' => $plans];
    }

    private static function pricing(int $count, ?array $breakpoints = self::STOCK_BREAKPOINTS, string $tone = 'ground'): array
    {
        return self::patterns($breakpoints)->pricing(new IdSeed(self::REQUEST), self::AT, self::node(self::plans($count)), $tone);
    }

    /** @param array<mixed> $value A value. */
    private static function bytes(array $value): string
    {
        return json_encode($value, JSON_THROW_ON_ERROR);
    }

    /**
     * The allowlist of a designed page that holds patterns.
     *
     * @return array<string,array<string,string>>
     */
    private static function allowedKeys(): array
    {
        $keys = ElementorClassicMapper::ALLOWED_KEYS;
        foreach (ElementorLeafStyle::KEYS as $type => $paths) {
            $keys[$type] = $type === 'container' ? $keys[$type] + $paths : array_merge($keys[$type], $paths);
        }

        return ElementorPatternCompiler::allowedKeys($keys);
    }

    private function assertAllowed(array $tree): void
    {
        $answer = LeafPolicy::checkTree($tree, self::allowedKeys(), ElementorClassicMapper::LEAF_RULES);
        $this->assertNull($answer, 'the tree is refused: ' . ($answer['detail'] ?? ''));
    }

    /**
     * Every node of a tree, depth first.
     *
     * @param list<array<string,mixed>> $nodes Nodes.
     * @return list<array<string,mixed>>
     */
    private static function walk(array $nodes): array
    {
        $out = [];
        foreach ($nodes as $node) {
            $out[] = $node;
            $out   = array_merge($out, self::walk($node['elements']));
        }

        return $out;
    }

    /**
     * The widget of a card by position among its children.
     *
     * @param array<string,mixed> $card A plan card.
     * @param string              $type The widget type.
     * @return list<array<string,mixed>>
     */
    private static function widgets(array $card, string $type): array
    {
        return array_values(array_filter(self::walk([$card]), static fn (array $n): bool => ($n['widgetType'] ?? null) === $type));
    }

    // -------------------------------------------------------------------------
    // pattern-pricing-composed
    // -------------------------------------------------------------------------

    public function test_pattern_pricing_composed(): void
    {
        $row       = self::pricing(3);
        $rowProbe  = RoundTripProbes::tree('layout-columns-row')[0]['elements'][0];
        $listProbe = RoundTripProbes::tree('pattern-icon-list')[0]['elements'][0];
        $look      = new ElementorLeafStyle(self::tokens());
        $colours   = self::tokens()->tone('ground');

        // The row is the columns row probe's, centred, labelled after its last key; the cards are its columns.
        $this->assertSame(array_keys($rowProbe['settings']), array_slice(array_keys($row['settings']), 0, count($rowProbe['settings'])));
        $this->assertSame(['flex_justify_content', '_title'], array_slice(array_keys($row['settings']), -2));
        $this->assertSame('center', $row['settings']['flex_justify_content']);
        $this->assertSame("WPMgr \u{00B7} Pricing three-plans", $row['settings']['_title']);
        $this->assertSame(['id', 'elType', 'settings', 'elements', 'isInner'], array_keys($row));
        $this->assertTrue($row['isInner']);
        $this->assertCount(3, $row['elements']);
        foreach ($row['elements'] as $card) {
            $this->assertSame(array_slice(array_keys($card['settings']), 0, 3), array_keys($rowProbe['elements'][0]['settings']));
            $this->assertSame(['unit' => '%', 'size' => '33'], $card['settings']['width']);
            $this->assertTrue($card['isInner']);
        }

        // A card, the highlighted one: a plan name, the price, the period, the checklist and the button.
        $card = $row['elements'][1];
        $this->assertSame(['heading', 'heading', 'text-editor', 'icon-list', null], array_map(static fn (array $e): ?string => $e['widgetType'] ?? null, $card['elements']));
        [$name, $price, $period, $list, $buttons] = $card['elements'];
        $this->assertSame(['title' => 'Growth', 'header_size' => 'h3'], array_slice($name['settings'], 0, 2));
        $this->assertSame(array_slice($look->headingStyle(3, 'ground', 'start'), 0), array_slice($name['settings'], 2));
        $this->assertSame(['title' => '$49', 'header_size' => 'div'], array_slice($price['settings'], 0, 2));
        $this->assertSame($look->headingStyle(2, 'ground', 'start'), array_slice($price['settings'], 2));
        $this->assertSame(['editor' => '<p>per month</p>'] + $look->textStyle('body', 'ground', 'start'), $period['settings']);

        // The checklist is the icon-list probe's widget in its key order and shape, a check in front of each line.
        $this->assertSame(array_keys($listProbe), array_keys($list));
        $this->assertSame(array_keys($listProbe['settings']), array_keys($list['settings']));
        $this->assertSame(array_keys($listProbe['settings']['icon_list'][0]), array_keys($list['settings']['icon_list'][0]));
        $this->assertCount(3, $list['settings']['icon_list']);
        foreach ($list['settings']['icon_list'] as $k => $line) {
            $this->assertSame(self::bytes($listProbe['settings']['icon_list'][0]['selected_icon']), self::bytes($line['selected_icon']));
            $this->assertSame(['10 sites', 'Priority support', 'Daily backups'][$k], $line['text']);
            $this->assertMatchesRegularExpression('/^[0-9a-f]{7}\z/', $line['_id']);
        }
        $this->assertSame($colours['accent'], $list['settings']['icon_color']);
        $this->assertSame($colours['body'], $list['settings']['text_color']);
        $this->assertSame(self::bytes($listProbe['settings']['icon_size']), self::bytes($list['settings']['icon_size']));

        // The button stands in the row of buttons the leaves use; the highlighted plan's is a fill.
        $this->assertSame($look->buttonRow('x', [], 'left')['settings'], $buttons['settings']);
        $this->assertCount(1, $buttons['elements']);
        $button = $buttons['elements'][0];
        $this->assertSame('button', $button['widgetType']);
        $this->assertSame(['text' => 'Choose Growth', 'link' => ['url' => '#signup', 'is_external' => '', 'nofollow' => '']], array_slice($button['settings'], 0, 2));
        $this->assertSame($look->buttonStyle('fill', 'ground'), array_slice($button['settings'], 2));

        $this->assertAllowed([$row]);
    }

    public function test_the_price_table_is_never_written(): void
    {
        foreach ([1, 2, 3, 4] as $count) {
            foreach (self::walk([self::pricing($count)]) as $node) {
                $this->assertNotSame('price-table', $node['widgetType'] ?? null);
            }
        }
    }

    public function test_a_plan_that_is_not_highlighted_is_an_outline_button_on_a_hairline_card(): void
    {
        $row   = self::pricing(3);
        $look  = new ElementorLeafStyle(self::tokens());
        $plain = $row['elements'][0];
        $high  = $row['elements'][1];
        $edge  = self::tokens()->hairline();
        $acc   = self::tokens()->tone('ground')['accent'];

        $this->assertSame($look->buttonStyle('outline', 'ground'), array_slice(self::widgets($plain, 'button')[0]['settings'], 2));
        $this->assertSame($look->buttonStyle('outline', 'ground'), array_slice(self::widgets($row['elements'][2], 'button')[0]['settings'], 2), 'a plan that does not say is not highlighted');
        $this->assertSame($look->buttonStyle('fill', 'ground'), array_slice(self::widgets($high, 'button')[0]['settings'], 2));

        $this->assertSame('1', $plain['settings']['border_width']['top']);
        $this->assertSame($edge['color'], $plain['settings']['border_color']);
        $this->assertSame('2', $high['settings']['border_width']['top']);
        $this->assertSame($acc, $high['settings']['border_color']);

        // Every card is padded, rounded and raised; the highlighted one differs by its edge and its button only.
        foreach ($row['elements'] as $card) {
            $this->assertSame(
                ['content_width', 'width', 'width_mobile', 'padding', 'padding_tablet', 'padding_mobile', 'border_border', 'border_width', 'border_color', 'border_radius', 'box_shadow_box_shadow_type', 'box_shadow_box_shadow'],
                array_keys($card['settings'])
            );
            $this->assertSame('yes', $card['settings']['box_shadow_box_shadow_type']);
            $this->assertSame(['horizontal' => '0', 'vertical' => '8', 'blur' => '24', 'spread' => '0', 'color' => 'rgba(21,23,28,0.08)'], $card['settings']['box_shadow_box_shadow']);
            $this->assertSame('12', $card['settings']['border_radius']['top']);
            $this->assertSame('32', $card['settings']['padding']['top']);
        }
    }

    public function test_a_plan_without_a_period_has_no_period_widget(): void
    {
        $card = self::pricing(3)['elements'][2];

        $this->assertSame(['heading', 'heading', 'icon-list', null], array_map(static fn (array $e): ?string => $e['widgetType'] ?? null, $card['elements']));
        $this->assertSame([], self::widgets($card, 'text-editor'));
    }

    /**
     * @return array<string,array{0:int,1:string}>
     */
    public static function counts(): array
    {
        return ['one' => [1, '50'], 'two' => [2, '50'], 'three' => [3, '33'], 'four' => [4, '25']];
    }

    /** @dataProvider counts */
    public function test_the_cards_of_a_row_share_it_equally_and_one_plan_is_centred(int $count, string $width): void
    {
        $row = self::pricing($count);

        $this->assertCount($count, $row['elements']);
        $this->assertSame(array_fill(0, $count, $width), array_map(static fn (array $c): string => $c['settings']['width']['size'], $row['elements']));
        $this->assertSame('center', $row['settings']['flex_justify_content']);
        $this->assertAllowed([$row]);
    }

    public function test_on_a_site_without_breakpoints_the_desktop_steps_only_are_written(): void
    {
        $row  = self::pricing(3, null);
        $card = $row['elements'][1];

        $this->assertSame(['flex_direction', 'flex_gap', 'flex_justify_content', '_title'], array_keys($row['settings']));
        $this->assertSame(
            ['content_width', 'width', 'padding', 'border_border', 'border_width', 'border_color', 'border_radius', 'box_shadow_box_shadow_type', 'box_shadow_box_shadow'],
            array_keys($card['settings'])
        );
        $this->assertAllowed([$row]);
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function tones(): array
    {
        return ['ground' => ['ground'], 'surface' => ['surface'], 'inverse' => ['inverse'], 'accent' => ['accent']];
    }

    /** @dataProvider tones */
    public function test_a_card_is_drawn_in_the_colours_of_its_tone(string $tone): void
    {
        $row     = self::pricing(3, self::STOCK_BREAKPOINTS, $tone);
        $colours = self::tokens()->tone($tone);
        $look    = new ElementorLeafStyle(self::tokens());

        foreach ($row['elements'] as $card) {
            $list = self::widgets($card, 'icon-list')[0]['settings'];
            $this->assertSame($colours['accent'], $list['icon_color']);
            $this->assertSame($colours['body'], $list['text_color']);
            $this->assertSame($colours['heading'], self::widgets($card, 'heading')[0]['settings']['title_color']);
        }
        $this->assertSame($colours['accent'], $row['elements'][1]['settings']['border_color']);
        $this->assertSame($look->buttonStyle('fill', $tone), array_slice(self::widgets($row['elements'][1], 'button')[0]['settings'], 2));
        $this->assertAllowed([$row]);
    }

    public function test_the_texts_of_a_plan_are_escaped_the_way_a_heading_is(): void
    {
        $plans    = self::plans(2);
        $plans[0] = ['name' => 'R&D <b>', 'price' => '$1 & up', 'period' => 'per [12] month', 'features' => ['A & B', 'Five [12] stars'], 'cta' => ['text' => 'Save & start', 'url' => '#signup']];
        $row      = self::patterns()->pricing(new IdSeed(self::REQUEST), self::AT, self::node($plans), 'ground');
        $card     = $row['elements'][0];

        $headings = self::widgets($card, 'heading');
        $this->assertSame('R&amp;D &lt;b&gt;', $headings[0]['settings']['title']);
        $this->assertSame('$1 &amp; up', $headings[1]['settings']['title']);
        $this->assertSame('<p>per &#091;12&#093; month</p>', self::widgets($card, 'text-editor')[0]['settings']['editor']);
        $this->assertSame(['A &amp; B', 'Five &#091;12&#093; stars'], array_column(self::widgets($card, 'icon-list')[0]['settings']['icon_list'], 'text'));
        $this->assertSame('Save &amp; start', self::widgets($card, 'button')[0]['settings']['text']);
        $this->assertAllowed([$row]);
    }

    public function test_every_node_and_repeater_item_has_an_id_of_its_own_and_the_same_request_the_same_ids(): void
    {
        $ids = [];
        foreach (self::walk([self::pricing(4)]) as $node) {
            $ids[] = $node['id'];
            foreach ($node['settings']['icon_list'] ?? [] as $item) {
                $ids[] = $item['_id'];
            }
        }

        $this->assertSame(count($ids), count(array_unique($ids)));
        foreach ($ids as $id) {
            $this->assertMatchesRegularExpression('/^[0-9a-f]{7}\z/', $id);
        }
        $this->assertSame(self::bytes(self::pricing(4)), self::bytes(self::pricing(4)));
    }

    public function test_a_plan_may_list_up_to_eight_features_and_the_policy_holds_every_line(): void
    {
        $plans              = self::plans(1);
        $plans[0]['features'] = ['a', 'b', 'c', 'd', 'e', 'f', 'g', 'h'];
        $row                = self::patterns()->pricing(new IdSeed(self::REQUEST), self::AT, self::node($plans), 'ground');

        $this->assertCount(8, self::widgets($row['elements'][0], 'icon-list')[0]['settings']['icon_list']);
        $this->assertAllowed([$row]);
    }

    public function test_the_checklist_icon_is_pinned_to_the_check(): void
    {
        $row = self::pricing(2);
        $this->assertAllowed([$row]);
        $row['elements'][0]['elements'][3]['settings']['icon_list'][0]['selected_icon']['value'] = 'fas fa-mug-hot';
        $answer = LeafPolicy::checkTree([$row], self::allowedKeys(), ElementorClassicMapper::LEAF_RULES);

        $this->assertNotNull($answer);
        $this->assertStringContainsString('selected_icon.value', (string) $answer['detail']);
    }

    public function test_a_pricing_pattern_outside_one_to_four_plans_is_not_compiled(): void
    {
        foreach ([0, 5] as $count) {
            $plans = $count === 0 ? [] : array_merge(self::plans(4), [self::plans(1)[0]]);
            try {
                self::patterns()->pricing(new IdSeed(self::REQUEST), self::AT, self::node($plans), 'ground');
                $this->fail($count . ' plans were compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
    }

    public function test_a_plan_the_grammar_would_have_refused_is_not_compiled(): void
    {
        $good = self::plans(2)[1];
        $bad  = [
            'no name'            => array_diff_key($good, ['name' => 1]),
            'empty price'        => ['price' => ''] + $good,
            'no price'           => array_diff_key($good, ['price' => 1]),
            'empty period'       => ['period' => ''] + $good,
            'no features'        => ['features' => []] + $good,
            'nine features'      => ['features' => array_fill(0, 9, 'x')] + $good,
            'feature not text'   => ['features' => ['x', 4]] + $good,
            'features not list'  => ['features' => ['a' => 'x']] + $good,
            'no button'          => array_diff_key($good, ['cta' => 1]),
            'button without url' => ['cta' => ['text' => 'Go']] + $good,
            'button with &'      => ['cta' => ['text' => 'Go', 'url' => 'https://example.com/?a=1&b=2']] + $good,
            'highlight a string' => ['highlighted' => 'yes'] + $good,
        ];
        foreach ($bad as $name => $plan) {
            try {
                self::patterns()->pricing(new IdSeed(self::REQUEST), self::AT, self::node([self::plans(1)[0], $plan]), 'ground');
                $this->fail($name . ' was compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage(), $name);
            }
        }
        foreach ([['variant' => 'five-plans'], ['pattern' => 'features']] as $change) {
            try {
                self::patterns()->pricing(new IdSeed(self::REQUEST), self::AT, $change + self::node(self::plans(3)), 'ground');
                $this->fail('a node of another pattern or variant was compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
    }

    public function test_the_compile_entry_builds_what_pricing_builds(): void
    {
        $node = self::node(self::plans(3));

        $this->assertSame(
            self::bytes(self::patterns()->pricing(new IdSeed(self::REQUEST), self::AT, $node, 'surface')),
            self::bytes(self::patterns()->compile(new IdSeed(self::REQUEST), self::AT, $node, 'surface'))
        );
    }

    public function test_the_checklist_keys_are_listed_for_every_line_a_plan_may_hold(): void
    {
        $listed = ElementorPatternCards::keys()['icon-list'];

        $this->assertSame('colour', $listed['icon_color']);
        $this->assertSame('colour', $listed['text_color']);
        $this->assertSame('slider:px', $listed['icon_size']);
        for ($i = 0; $i < OutlineV3Schema::MAX_PLAN_FEATURES; ++$i) {
            $this->assertSame('html', $listed['icon_list.' . $i . '.text']);
            $this->assertSame('enum:' . ElementorPatternCards::ICONS['check'], $listed['icon_list.' . $i . '.selected_icon.value']);
            $this->assertSame('enum:' . ElementorPatternCards::ICON_LIBRARY, $listed['icon_list.' . $i . '.selected_icon.library']);
            $this->assertSame('enum', $listed['icon_list.' . $i . '._id']);
        }
        $this->assertArrayNotHasKey('icon_list.' . OutlineV3Schema::MAX_PLAN_FEATURES . '.text', $listed);
        $this->assertSame($listed, ElementorPatternCompiler::allowedKeys(ElementorClassicMapper::ALLOWED_KEYS)['icon-list']);
    }
}
