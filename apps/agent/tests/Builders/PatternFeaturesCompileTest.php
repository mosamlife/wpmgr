<?php
/**
 * The features pattern of a designed page: icon-box widgets from a closed list
 * of icons, as a two-column list (split) or as cards in rows (bento, grid).
 * The icon box is compiled in the key order of the frozen round-trip probe; the
 * row and its columns are the probes' columns row.
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
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler
 */
final class PatternFeaturesCompileTest extends TestCase
{
    private const STOCK_BREAKPOINTS = ['mobile', 'tablet'];

    private const REQUEST = 'b7e2a4c1-0000-4000-8000-000000000011';

    private const AT = 'outline[1].children[0]';

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
            'fonts'   => ['heading' => 'Manrope', 'body' => 'system'],
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

    /** @return list<array<string,string>> */
    private static function items(int $count): array
    {
        $all = [
            ['icon' => 'bolt', 'title' => 'Fast setup', 'text' => 'Live in an afternoon.'],
            ['icon' => 'shield', 'title' => 'Safe by default', 'text' => 'Every change can be undone.'],
            ['icon' => 'users', 'title' => 'Built for teams', 'text' => 'Invite people and share the work.'],
            ['icon' => 'chart', 'title' => 'Clear reports', 'text' => 'See what changed and when.'],
            ['icon' => 'globe', 'title' => 'Works anywhere', 'text' => 'Any host and any theme.'],
            ['icon' => 'lock', 'title' => 'Private', 'text' => 'Your data stays on your site.'],
            ['icon' => 'rocket', 'title' => 'Quick', 'text' => 'Pages load fast.'],
            ['icon' => 'heart', 'title' => 'Loved', 'text' => 'People stay for years.'],
        ];

        return array_slice($all, 0, $count);
    }

    /** @param list<array<string,string>> $items Items. */
    private static function node(string $variant, array $items): array
    {
        return ['type' => 'pattern', 'pattern' => 'features', 'variant' => $variant, 'features' => $items];
    }

    private static function features(string $variant, int $count, ?array $breakpoints = self::STOCK_BREAKPOINTS, string $tone = 'ground'): array
    {
        return self::patterns($breakpoints)->features(new IdSeed(self::REQUEST), self::AT, self::node($variant, self::items($count)), $tone);
    }

    /** @param array<mixed> $value A value. */
    private static function bytes(array $value): string
    {
        return json_encode($value, JSON_THROW_ON_ERROR);
    }

    private function assertAllowed(array $tree): void
    {
        $keys = ElementorClassicMapper::ALLOWED_KEYS;
        foreach (ElementorLeafStyle::KEYS as $type => $paths) {
            $keys[$type] = $type === 'container' ? $keys[$type] + $paths : array_merge($keys[$type], $paths);
        }
        $answer = LeafPolicy::checkTree($tree, ElementorPatternCompiler::allowedKeys($keys), ElementorClassicMapper::LEAF_RULES);
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

    // -------------------------------------------------------------------------
    // pattern-features-split
    // -------------------------------------------------------------------------

    public function test_pattern_features_split(): void
    {
        $row      = self::features('split', 3);
        $rowProbe = RoundTripProbes::tree('layout-columns-row')[0]['elements'][0];
        $boxProbe = RoundTripProbes::tree('pattern-icon-box')[0]['elements'][0];

        // The row is the columns row probe's, labelled after its last key.
        $this->assertSame(array_keys($rowProbe['settings']), array_slice(array_keys($row['settings']), 0, count($rowProbe['settings'])));
        $this->assertSame('_title', array_key_last($row['settings']));
        $this->assertSame("WPMgr \u{00B7} Features split", $row['settings']['_title']);
        $this->assertSame(['id', 'elType', 'settings', 'elements', 'isInner'], array_keys($row));
        $this->assertTrue($row['isInner']);

        // Two equal columns that are the probe's columns; the list is split in order, the left one longer by one.
        $this->assertCount(2, $row['elements']);
        foreach ($row['elements'] as $column) {
            $this->assertSame(array_keys($rowProbe['elements'][0]['settings']), array_keys($column['settings']));
            $this->assertSame(['unit' => '%', 'size' => '50'], $column['settings']['width']);
            $this->assertSame(['unit' => '%', 'size' => '100'], $column['settings']['width_mobile']);
        }
        $this->assertSame([2, 1], array_map(static fn (array $c): int => count($c['elements']), $row['elements']));
        $this->assertSame(
            ['Fast setup', 'Safe by default', 'Built for teams'],
            array_map(static fn (array $b): string => $b['settings']['title_text'], array_merge($row['elements'][0]['elements'], $row['elements'][1]['elements']))
        );

        // An icon box is the probe's widget: its keys come in the probe's order, in the probe's shape.
        $box = $row['elements'][0]['elements'][0];
        $this->assertSame(array_keys($boxProbe), array_keys($box));
        $this->assertSame('widget', $box['elType']);
        $this->assertSame('icon-box', $box['widgetType']);
        $this->assertSame([], $box['elements']);
        $mine = array_keys($box['settings']);
        $this->assertSame(array_keys($boxProbe['settings']), array_values(array_intersect($mine, array_keys($boxProbe['settings']))));
        $this->assertSame(array_keys($boxProbe['settings']['selected_icon']), array_keys($box['settings']['selected_icon']));
        $this->assertSame($boxProbe['settings']['selected_icon']['library'], $box['settings']['selected_icon']['library']);
        $this->assertSame('fas fa-bolt', $box['settings']['selected_icon']['value']);
        $this->assertSame('Fast setup', $box['settings']['title_text']);
        $this->assertSame('Live in an afternoon.', $box['settings']['description_text']);
        $this->assertSame('left', $box['settings']['position']);
        $this->assertSame($boxProbe['settings']['title_size'], $box['settings']['title_size']);

        // The tone's colours and the h3 style of the ramp, as numeric strings.
        $colours = self::tokens()->tone('ground');
        $this->assertSame($colours['accent'], $box['settings']['primary_color']);
        $this->assertSame($colours['heading'], $box['settings']['title_color']);
        $this->assertSame($colours['body'], $box['settings']['description_color']);
        $h3 = self::tokens()->typography('h3');
        $this->assertSame('custom', $box['settings']['title_typography_typography']);
        $this->assertSame('Manrope', $box['settings']['title_typography_font_family']);
        $this->assertSame(['unit' => 'px', 'size' => (string) $h3['size']], $box['settings']['title_typography_font_size']);
        $this->assertSame($boxProbe['settings']['title_typography_font_weight'], $box['settings']['title_typography_font_weight']);
        $this->assertSame(['unit' => 'em', 'size' => '1.15'], $box['settings']['title_typography_line_height']);

        $this->assertAllowed([$row]);
    }

    // -------------------------------------------------------------------------
    // The icon list
    // -------------------------------------------------------------------------

    public function test_the_icon_list_is_the_grammars_list_and_each_icon_is_a_solid_icon(): void
    {
        $this->assertSame(OutlineV3Schema::ICONS, array_keys(ElementorPatternCards::ICONS));
        $this->assertSame('fa-solid', ElementorPatternCards::ICON_LIBRARY);
        foreach (ElementorPatternCards::ICONS as $name => $value) {
            $this->assertMatchesRegularExpression('/^fas fa-[a-z]+(?:-[a-z]+)*\z/', $value, $name);
        }
        $this->assertCount(count(ElementorPatternCards::ICONS), array_unique(array_values(ElementorPatternCards::ICONS)), 'no two names share an icon');

        // The names whose icon in the library is named otherwise; each of these is an icon in Elementor's solid library.
        $this->assertSame('fas fa-chart-line', ElementorPatternCards::ICONS['chart']);
        $this->assertSame('fas fa-layer-group', ElementorPatternCards::ICONS['layers']);
        $this->assertSame('fas fa-map-marker-alt', ElementorPatternCards::ICONS['map-pin']);
        $this->assertSame('fas fa-shield-alt', ElementorPatternCards::ICONS['shield']);
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function iconNames(): array
    {
        $out = [];
        foreach (OutlineV3Schema::ICONS as $name) {
            $out[$name] = [$name];
        }

        return $out;
    }

    /** @dataProvider iconNames */
    public function test_every_icon_of_the_list_compiles_to_its_elementor_icon(string $name): void
    {
        $items    = self::items(3);
        $items[0] = ['icon' => $name, 'title' => 'T', 'text' => 'X'];
        $row      = self::patterns()->features(new IdSeed(self::REQUEST), self::AT, self::node('split', $items), 'ground');
        $icon     = $row['elements'][0]['elements'][0]['settings']['selected_icon'];

        $this->assertSame(['value' => ElementorPatternCards::ICONS[$name], 'library' => 'fa-solid'], $icon);
        $this->assertAllowed([$row]);
    }

    public function test_an_icon_outside_the_list_is_not_compiled_and_not_allowed(): void
    {
        $items    = self::items(3);
        $items[1] = ['icon' => 'mug-hot', 'title' => 'T', 'text' => 'X'];
        try {
            self::patterns()->features(new IdSeed(self::REQUEST), self::AT, self::node('split', $items), 'ground');
            $this->fail('an icon outside the list was compiled');
        } catch (\InvalidArgumentException $e) {
            $this->assertStringContainsString('icon', $e->getMessage());
        }

        // The pin of the icon value holds the same list: the probe's own icon is not on it.
        $row = self::features('split', 3);
        $row['elements'][0]['elements'][0]['settings']['selected_icon']['value'] = 'fas fa-mug-hot';
        $answer = LeafPolicy::checkTree([$row], ElementorPatternCompiler::allowedKeys(ElementorClassicMapper::ALLOWED_KEYS), ElementorClassicMapper::LEAF_RULES);
        $this->assertNotNull($answer);
        $this->assertStringContainsString('selected_icon.value', (string) $answer['detail']);
    }

    // -------------------------------------------------------------------------
    // bento and grid
    // -------------------------------------------------------------------------

    /**
     * Widths of each row's cards; null is a card alone in its row, at the full width.
     *
     * @return array<string,array{0:string,1:int,2:list<list<int|null>>}>
     */
    public static function cardRows(): array
    {
        return [
            'grid 3'  => ['grid', 3, [[33, 33, 33]]],
            'grid 4'  => ['grid', 4, [[50, 50], [50, 50]]],
            'grid 5'  => ['grid', 5, [[33, 33, 33], [50, 50]]],
            'grid 6'  => ['grid', 6, [[33, 33, 33], [33, 33, 33]]],
            'grid 7'  => ['grid', 7, [[33, 33, 33], [50, 50], [50, 50]]],
            'grid 8'  => ['grid', 8, [[33, 33, 33], [33, 33, 33], [50, 50]]],
            'bento 3' => ['bento', 3, [[60, 40], [null]]],
            'bento 4' => ['bento', 4, [[60, 40], [40, 60]]],
            'bento 5' => ['bento', 5, [[60, 40], [40, 60], [null]]],
            'bento 8' => ['bento', 8, [[60, 40], [40, 60], [60, 40], [40, 60]]],
        ];
    }

    /**
     * @param list<list<int|null>> $rows The expected widths.
     * @dataProvider cardRows
     */
    public function test_cards_stand_in_rows_of_the_widths_the_variant_plans(string $variant, int $count, array $rows): void
    {
        $group = self::features($variant, $count);

        $this->assertSame("WPMgr \u{00B7} Features " . $variant, $group['settings']['_title']);
        $this->assertSame(['content_width' => 'full', 'flex_direction' => 'column'], array_slice($group['settings'], 0, 2));
        $this->assertSame(['flex_gap', 'flex_gap_tablet', 'flex_gap_mobile', '_title'], array_slice(array_keys($group['settings']), 2));
        $this->assertSame(['column' => '32', 'row' => '32', 'isLinked' => true, 'unit' => 'px'], $group['settings']['flex_gap']);

        $planned = [];
        $titles  = [];
        foreach ($group['elements'] as $row) {
            $cards     = $row['settings']['flex_direction'] === 'row' ? $row['elements'] : [$row];
            $planned[] = array_map(static fn (array $c): ?int => isset($c['settings']['width']) ? (int) $c['settings']['width']['size'] : null, $cards);
            foreach ($cards as $card) {
                $this->assertCount(1, $card['elements']);
                $this->assertSame('top', $card['elements'][0]['settings']['position']);
                $titles[] = $card['elements'][0]['settings']['title_text'];
            }
        }
        $this->assertSame($rows, $planned);
        $this->assertSame(array_column(self::items($count), 'title'), $titles, 'every item stands in one card, in order');
        $this->assertAllowed([$group]);
    }

    public function test_a_card_is_a_padded_bordered_rounded_container_with_no_shadow(): void
    {
        $card = self::features('grid', 3)['elements'][0]['elements'][0];
        $edge = self::tokens()->hairline();

        $this->assertSame(
            ['content_width', 'width', 'width_mobile', 'padding', 'padding_tablet', 'padding_mobile', 'border_border', 'border_width', 'border_color', 'border_radius'],
            array_keys($card['settings'])
        );
        $this->assertSame(['unit' => 'px', 'top' => '32', 'right' => '32', 'bottom' => '32', 'left' => '32', 'isLinked' => true], $card['settings']['padding']);
        $this->assertSame('28', $card['settings']['padding_tablet']['top']);
        $this->assertSame('24', $card['settings']['padding_mobile']['top']);
        $this->assertSame('solid', $card['settings']['border_border']);
        $this->assertSame(['unit' => 'px', 'top' => '1', 'right' => '1', 'bottom' => '1', 'left' => '1', 'isLinked' => true], $card['settings']['border_width']);
        $this->assertSame($edge['color'], $card['settings']['border_color']);
        $this->assertSame('12', $card['settings']['border_radius']['top']);
        $this->assertTrue($card['isInner']);
    }

    public function test_on_a_site_without_breakpoints_a_card_writes_the_desktop_steps_only(): void
    {
        $group = self::features('bento', 4, null);
        $card  = $group['elements'][0]['elements'][0];

        $this->assertSame(
            ['content_width', 'width', 'padding', 'border_border', 'border_width', 'border_color', 'border_radius'],
            array_keys($card['settings'])
        );
        $this->assertSame(['flex_gap', '_title'], array_slice(array_keys($group['settings']), 2));
        $this->assertSame('flex_direction', array_keys($group['elements'][0]['settings'])[0]);
        $this->assertArrayNotHasKey('flex_direction_mobile', $group['elements'][0]['settings']);
        $this->assertAllowed([$group]);
    }

    public function test_a_split_list_on_a_site_without_breakpoints_writes_the_desktop_row_only(): void
    {
        $row = self::features('split', 4, null);

        $this->assertSame(['flex_direction', 'flex_gap', '_title'], array_keys($row['settings']));
        $this->assertSame(['content_width', 'width'], array_keys($row['elements'][0]['settings']));
        $this->assertAllowed([$row]);
    }

    // -------------------------------------------------------------------------
    // Text, tones, ids, refusals
    // -------------------------------------------------------------------------

    /**
     * @return array<string,array{0:string}>
     */
    public static function tones(): array
    {
        return ['ground' => ['ground'], 'surface' => ['surface'], 'inverse' => ['inverse'], 'accent' => ['accent']];
    }

    /** @dataProvider tones */
    public function test_an_icon_box_is_drawn_in_the_colours_of_its_tone(string $tone): void
    {
        $colours = self::tokens()->tone($tone);
        $row     = self::features('split', 3, self::STOCK_BREAKPOINTS, $tone);

        foreach (self::walk([$row]) as $node) {
            if (($node['widgetType'] ?? null) !== 'icon-box') {
                continue;
            }
            $this->assertSame($colours['accent'], $node['settings']['primary_color']);
            $this->assertSame($colours['heading'], $node['settings']['title_color']);
            $this->assertSame($colours['body'], $node['settings']['description_color']);
        }
        $this->assertAllowed([$row]);
    }

    public function test_the_texts_of_an_icon_box_are_escaped_the_way_a_heading_is(): void
    {
        $items    = self::items(3);
        $items[0] = ['icon' => 'star', 'title' => 'R&D <b>', 'text' => 'Five [12] stars & more'];
        $row      = self::patterns()->features(new IdSeed(self::REQUEST), self::AT, self::node('split', $items), 'ground');
        $box      = $row['elements'][0]['elements'][0]['settings'];

        $this->assertSame('R&amp;D &lt;b&gt;', $box['title_text']);
        $this->assertSame('Five &#091;12&#093; stars &amp; more', $box['description_text']);
        $this->assertAllowed([$row]);
    }

    public function test_a_system_body_and_heading_font_writes_no_family(): void
    {
        $design = DesignTokens::fromDesign([
            'colors'  => ['ground' => '#FFFFFF', 'surface' => '#F6F5F2', 'ink' => '#15171C', 'muted' => '#5A606C', 'accent' => '#0D7A63', 'on_accent' => '#FAFAF7'],
            'fonts'   => ['heading' => 'system', 'body' => 'system'],
            'scale'   => ['base_px' => 17, 'ratio' => 'major_third'],
            'radius'  => 'md',
            'density' => 'normal',
        ]);
        $compiler = new ElementorPatternCompiler($design, new ElementorSectionCompiler($design, self::STOCK_BREAKPOINTS));
        $row      = $compiler->features(new IdSeed(self::REQUEST), self::AT, self::node('split', self::items(3)), 'ground');

        $this->assertArrayNotHasKey('title_typography_font_family', $row['elements'][0]['elements'][0]['settings']);
        $this->assertAllowed([$row]);
    }

    public function test_every_node_has_an_id_of_its_own(): void
    {
        foreach (['split', 'bento', 'grid'] as $variant) {
            $ids = array_column(self::walk([self::features($variant, 8)]), 'id');

            $this->assertSame(count($ids), count(array_unique($ids)), $variant);
            foreach ($ids as $id) {
                $this->assertMatchesRegularExpression('/^[0-9a-f]{7}\z/', $id);
            }
        }
    }

    public function test_a_features_pattern_outside_three_to_eight_items_is_not_compiled(): void
    {
        foreach ([0, 2, 9] as $count) {
            $items = $count === 9 ? array_merge(self::items(8), [self::items(1)[0]]) : self::items($count);
            try {
                self::patterns()->features(new IdSeed(self::REQUEST), self::AT, self::node('split', $items), 'ground');
                $this->fail($count . ' items were compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
    }

    public function test_an_item_or_a_pattern_the_grammar_would_have_refused_is_not_compiled(): void
    {
        $bad = [
            'no title'     => ['icon' => 'bolt', 'text' => 'X'],
            'empty title'  => ['icon' => 'bolt', 'title' => '', 'text' => 'X'],
            'no text'      => ['icon' => 'bolt', 'title' => 'T'],
            'list text'    => ['icon' => 'bolt', 'title' => 'T', 'text' => ['X']],
            'no icon'      => ['title' => 'T', 'text' => 'X'],
            'number icon'  => ['icon' => 4, 'title' => 'T', 'text' => 'X'],
        ];
        foreach ($bad as $name => $item) {
            $items    = self::items(3);
            $items[2] = $item;
            try {
                self::patterns()->features(new IdSeed(self::REQUEST), self::AT, self::node('split', $items), 'ground');
                $this->fail($name . ' was compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage(), $name);
            }
        }
        foreach ([['variant' => 'masonry'], ['variant' => 'row'], ['pattern' => 'pricing']] as $change) {
            try {
                self::patterns()->features(new IdSeed(self::REQUEST), self::AT, $change + self::node('split', self::items(3)), 'ground');
                $this->fail('a node of another pattern or variant was compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
    }

    public function test_the_compile_entry_builds_what_features_builds(): void
    {
        foreach (['split', 'bento', 'grid'] as $variant) {
            $node = self::node($variant, self::items(5));

            $this->assertSame(
                self::bytes(self::patterns()->features(new IdSeed(self::REQUEST), self::AT, $node, 'surface')),
                self::bytes(self::patterns()->compile(new IdSeed(self::REQUEST), self::AT, $node, 'surface')),
                $variant
            );
        }
    }

    public function test_the_keys_the_cards_list_are_handed_on_with_the_pattern_keys(): void
    {
        $listed = ElementorPatternCards::keys();
        foreach (['icon-box', 'container'] as $type) {
            $this->assertArrayHasKey($type, $listed);
        }
        $this->assertSame('colour', $listed['icon-box']['primary_color']);
        $this->assertSame('enum:left|top', $listed['icon-box']['position']);
        $this->assertSame('enum:h3', $listed['icon-box']['title_size']);
        $this->assertSame('enum:' . implode('|', array_values(ElementorPatternCards::ICONS)), $listed['icon-box']['selected_icon.value']);

        // The pattern compiler hands these on with the rest.
        $allowed = ElementorPatternCompiler::allowedKeys(ElementorClassicMapper::ALLOWED_KEYS);
        $this->assertSame($listed['icon-box'], $allowed['icon-box']);
        $this->assertSame('enum:solid', $allowed['container']['border_border']);
        $this->assertSame('enum:center', $allowed['container']['flex_justify_content'], 'a pin the mapper already lists is kept');
    }

    public function test_a_raised_card_casts_the_wide_layer_of_the_card_shadow_alone(): void
    {
        $sections = new ElementorSectionCompiler(self::tokens(), self::STOCK_BREAKPOINTS);
        $edge     = self::tokens()->hairline();
        $plain    = $sections->cardSettings($edge['color'], 1, false);
        $raised   = $sections->cardSettings($edge['color'], 2, true);
        $layers   = self::tokens()->cardShadow();

        $this->assertArrayNotHasKey('box_shadow_box_shadow_type', $plain);
        $this->assertSame(['box_shadow_box_shadow_type', 'box_shadow_box_shadow'], array_slice(array_keys($raised), -2));
        $this->assertSame('yes', $raised['box_shadow_box_shadow_type']);
        $this->assertSame(['horizontal' => '0', 'vertical' => '8', 'blur' => '24', 'spread' => '0', 'color' => $layers[1]['color']], $raised['box_shadow_box_shadow']);
        $this->assertSame('2', $raised['border_width']['top']);
        $this->assertSame(['rgba(21,23,28,0.08)'], [$raised['box_shadow_box_shadow']['color']], 'ink at 8 percent, never black');
        try {
            $sections->cardSettings($edge['color'], 0, false);
            $this->fail('a border of no width was written');
        } catch (\InvalidArgumentException $e) {
            $this->assertNotSame('', $e->getMessage());
        }
    }
}
