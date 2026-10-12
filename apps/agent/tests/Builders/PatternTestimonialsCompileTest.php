<?php
/**
 * The testimonials pattern of a designed page, featured variant: the first
 * testimonial across the full width and the others in rows of two under it.
 * Each testimonial is compiled as the frozen round-trip probe stores it; the
 * group, the rows and their columns are the section compiler's.
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
use WPMgr\Agent\Abilities\Builders\ElementorPatternCompiler;
use WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler;
use WPMgr\Agent\Abilities\Builders\IdSeed;
use WPMgr\Agent\Abilities\Builders\LeafPolicy;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorPatternCompiler
 */
final class PatternTestimonialsCompileTest extends TestCase
{
    private const STOCK_BREAKPOINTS = ['mobile', 'tablet'];

    private const REQUEST = 'b7e2a4c1-0000-4000-8000-000000000013';

    private const PROBE_URL = 'https://example.com/wp-content/uploads/2026/10/van-1024x640.png';

    private const AT = 'outline[2].children[0]';

    /** The keys of a testimonial that names a role and a picture, in the order Elementor returns them. */
    private const FULL_KEYS = [
        'testimonial_content',
        'testimonial_image',
        'testimonial_name',
        'testimonial_job',
        'testimonial_image_position',
        'testimonial_alignment',
        'content_content_color',
        'name_text_color',
        'job_text_color',
    ];

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

    /**
     * Resolved image facts: attachment 5 is the testimonial probe's picture.
     *
     * @return array<int,array<string,mixed>>
     */
    private static function media(): array
    {
        $media = [];
        foreach ([5, 6, 7, 8] as $id) {
            $media[$id] = [
                'id'           => $id,
                'url'          => $id === 5 ? self::PROBE_URL : 'https://example.com/wp-content/uploads/2026/10/face-' . $id . '-150x150.png',
                'filename'     => 'face-' . $id . '.png',
                'mime'         => 'image/png',
                'width'        => 150,
                'height'       => 150,
                'modified_gmt' => '2026-10-01 09:00:00',
                'alt'          => '',
            ];
        }

        return $media;
    }

    /** @param list<string>|null $breakpoints Active breakpoints; null when none is. */
    private static function patterns(?array $breakpoints = self::STOCK_BREAKPOINTS, ?array $media = null): ElementorPatternCompiler
    {
        $media ??= self::media();

        return new ElementorPatternCompiler(self::tokens(), new ElementorSectionCompiler(self::tokens(), $breakpoints, $media), $media);
    }

    /** @return list<array<string,mixed>> The first $count items; the first is the testimonial probe's. */
    private static function items(int $count): array
    {
        $all = [
            ['quote' => 'Best flat white in town.', 'name' => 'Asha Rao', 'role' => 'Regular', 'attachment_id' => 5],
            ['quote' => 'They remembered my order after one visit.', 'name' => 'Tom Baker', 'role' => 'Neighbour', 'attachment_id' => 6],
            ['quote' => 'The only place I trust with a gift box.', 'name' => 'Mei Lin', 'attachment_id' => 7],
            ['quote' => 'Fast, warm and never rushed.', 'name' => 'Joao Silva', 'role' => 'Courier'],
            ['quote' => 'My team lives on their cold brew.', 'name' => 'Priya N.', 'role' => 'Office manager', 'attachment_id' => 8],
            ['quote' => 'A proper local roaster.', 'name' => 'Sam Okoye'],
        ];

        return array_slice($all, 0, $count);
    }

    /** @param list<array<string,mixed>> $items Testimonial items. */
    private static function node(array $items): array
    {
        return ['type' => 'pattern', 'pattern' => 'testimonials', 'variant' => 'featured', 'testimonials' => $items];
    }

    /** @param array<mixed> $value A value. */
    private static function bytes(array $value): string
    {
        return json_encode($value, JSON_THROW_ON_ERROR);
    }

    /**
     * The widgets of one type under an element, in document order.
     *
     * @param array<string,mixed> $element An element.
     * @param string              $type    A widget type.
     * @return list<array<string,mixed>>
     */
    private static function widgets(array $element, string $type): array
    {
        $found = ($element['widgetType'] ?? null) === $type ? [$element] : [];
        foreach ($element['elements'] as $child) {
            $found = array_merge($found, self::widgets($child, $type));
        }

        return $found;
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

    // -------------------------------------------------------------------------
    // pattern-testimonials-featured
    // -------------------------------------------------------------------------

    public function test_pattern_testimonials_featured(): void
    {
        $tree  = self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node(self::items(3)), 'ground');
        $probe = RoundTripProbes::tree('pattern-testimonial')[0]['elements'][0];
        $tone  = self::tokens()->tone('ground');

        // The pattern is a labelled group with the design's row gap between its parts.
        $this->assertSame(['id', 'elType', 'settings', 'elements', 'isInner'], array_keys($tree));
        $this->assertSame(['content_width', 'flex_direction', 'flex_gap', 'flex_gap_tablet', 'flex_gap_mobile', '_title'], array_keys($tree['settings']));
        $this->assertSame('full', $tree['settings']['content_width']);
        $this->assertSame('column', $tree['settings']['flex_direction']);
        $this->assertSame(['column' => '32', 'row' => '32', 'isLinked' => true, 'unit' => 'px'], $tree['settings']['flex_gap']);
        $this->assertSame("WPMgr \u{00B7} Testimonials featured", $tree['settings']['_title']);
        $this->assertTrue($tree['isInner']);
        $this->assertCount(2, $tree['elements']);

        // The featured testimonial is the probe's widget: its key order, its values, the tone's colours.
        $featured = $tree['elements'][0];
        $expected = $probe['settings'];
        $expected['content_content_color'] = $tone['heading'];
        $expected['name_text_color']       = $tone['accent'];
        $expected['job_text_color']        = $tone['body'];
        $this->assertSame(self::bytes($expected), self::bytes($featured['settings']));
        $this->assertSame(array_keys($probe), array_keys($featured));
        $this->assertSame('widget', $featured['elType']);
        $this->assertSame('testimonial', $featured['widgetType']);
        $this->assertSame([], $featured['elements']);

        // The other two stand side by side under it, in equal columns that stack on a phone.
        $row = $tree['elements'][1];
        $this->assertSame(['id', 'elType', 'settings', 'elements', 'isInner'], array_keys($row));
        $this->assertSame(['flex_direction', 'flex_direction_mobile', 'flex_gap', 'flex_gap_tablet', 'flex_gap_mobile'], array_keys($row['settings']));
        $this->assertSame('row', $row['settings']['flex_direction']);
        $this->assertSame('column', $row['settings']['flex_direction_mobile']);
        $this->assertTrue($row['isInner']);
        $this->assertCount(2, $row['elements']);
        foreach ($row['elements'] as $column) {
            $this->assertSame(['content_width' => 'full', 'width' => ['unit' => '%', 'size' => '50'], 'width_mobile' => ['unit' => '%', 'size' => '100']], $column['settings']);
            $this->assertCount(1, $column['elements']);
            $this->assertSame('testimonial', $column['elements'][0]['widgetType']);
        }

        $this->assertAllowed([$tree]);
    }

    /**
     * @return array<string,array{0:string}>
     */
    public static function tones(): array
    {
        return ['ground' => ['ground'], 'surface' => ['surface'], 'inverse' => ['inverse'], 'accent' => ['accent']];
    }

    /** @dataProvider tones */
    public function test_a_testimonial_is_drawn_in_the_colours_of_its_tone(string $tone): void
    {
        $tree    = self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node(self::items(3)), $tone);
        $colours = self::tokens()->tone($tone);

        $widgets = self::widgets($tree, 'testimonial');
        $this->assertCount(3, $widgets);
        foreach ($widgets as $widget) {
            $this->assertSame($colours['heading'], $widget['settings']['content_content_color']);
            $this->assertSame($colours['accent'], $widget['settings']['name_text_color']);
            $this->assertSame($colours['body'], $widget['settings']['job_text_color']);
        }
        $this->assertAllowed([$tree]);
    }

    /**
     * @return array<string,array{0:int,1:list<int>}>
     */
    public static function counts(): array
    {
        return [
            'one'   => [1, []],
            'two'   => [2, [1]],
            'three' => [3, [2]],
            'four'  => [4, [2, 1]],
            'five'  => [5, [2, 2]],
            'six'   => [6, [2, 2, 1]],
        ];
    }

    /**
     * @dataProvider counts
     * @param list<int> $rows The number of columns in each row under the featured testimonial.
     */
    public function test_the_smaller_testimonials_stand_in_rows_of_two_and_a_row_of_one_is_centred(int $count, array $rows): void
    {
        $tree = self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node(self::items($count)), 'surface');

        $this->assertSame('testimonial', $tree['elements'][0]['widgetType']);
        $under = array_slice($tree['elements'], 1);
        $this->assertSame($rows, array_map(static fn (array $row): int => count($row['elements']), $under));
        foreach ($under as $row) {
            $this->assertSame('container', $row['elType']);
            $this->assertSame(count($row['elements']) === 1 ? 'center' : null, $row['settings']['flex_justify_content'] ?? null);
            foreach ($row['elements'] as $column) {
                $this->assertSame('50', $column['settings']['width']['size']);
            }
        }
        $this->assertAllowed([$tree]);
    }

    public function test_the_testimonials_keep_the_order_of_the_outline(): void
    {
        $items = self::items(6);
        $tree  = self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node($items), 'ground');

        $names = array_map(static fn (array $widget): string => $widget['settings']['testimonial_name'], self::widgets($tree, 'testimonial'));
        $this->assertSame(array_map(static fn (array $item): string => LeafPolicy::htmlText($item['name']), $items), $names);
    }

    public function test_a_field_an_item_leaves_out_is_not_written(): void
    {
        $tree = self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node(self::items(6)), 'ground');
        $keys = array_map(static fn (array $widget): array => array_keys($widget['settings']), self::widgets($tree, 'testimonial'));

        $this->assertSame(self::FULL_KEYS, $keys[0]);
        $this->assertSame(self::FULL_KEYS, $keys[1]);
        $this->assertSame(array_values(array_diff(self::FULL_KEYS, ['testimonial_job'])), $keys[2], 'no role');
        $this->assertSame(array_values(array_diff(self::FULL_KEYS, ['testimonial_image'])), $keys[3], 'no picture');
        $this->assertSame(self::FULL_KEYS, $keys[4]);
        $this->assertSame(array_values(array_diff(self::FULL_KEYS, ['testimonial_image', 'testimonial_job'])), $keys[5], 'neither');
        $this->assertAllowed([$tree]);
    }

    public function test_the_texts_of_a_testimonial_are_escaped(): void
    {
        $tree = self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node([
            ['quote' => 'Fish & chips <b>[12]</b>', 'name' => 'A&B', 'role' => 'Owner [2]'],
        ]), 'ground');

        $settings = $tree['elements'][0]['settings'];
        $this->assertSame('Fish &amp; chips &lt;b&gt;&#091;12&#093;&lt;/b&gt;', $settings['testimonial_content']);
        $this->assertSame('A&amp;B', $settings['testimonial_name']);
        $this->assertSame('Owner &#091;2&#093;', $settings['testimonial_job']);
        $this->assertAllowed([$tree]);
    }

    public function test_every_node_of_the_pattern_has_an_id_of_its_own(): void
    {
        $tree = self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node(self::items(6)), 'ground');
        $ids  = [];
        $walk = static function (array $element) use (&$walk, &$ids): void {
            $ids[] = $element['id'];
            foreach ($element['elements'] as $child) {
                $walk($child);
            }
        };
        $walk($tree);

        // The group, six testimonials, three rows and five columns.
        $this->assertCount(15, $ids);
        $this->assertCount(15, array_unique($ids));
        foreach ($ids as $id) {
            $this->assertMatchesRegularExpression('/^[0-9a-f]{7}\z/', $id);
        }
    }

    public function test_a_testimonials_pattern_outside_one_to_six_items_is_not_compiled(): void
    {
        foreach ([0, 7] as $count) {
            $items = $count === 0 ? [] : array_merge(self::items(6), [['quote' => 'one more', 'name' => 'Z']]);
            try {
                self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node($items), 'ground');
                $this->fail($count . ' items were compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
        $this->assertCount(1, self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node(self::items(1)), 'ground')['elements']);
        $this->assertCount(6, self::widgets(self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node(self::items(6)), 'ground'), 'testimonial'));
    }

    public function test_a_testimonial_the_grammar_would_have_refused_is_not_compiled(): void
    {
        $bad = [
            'no quote'      => ['name' => 'x'],
            'empty quote'   => ['quote' => '', 'name' => 'x'],
            'quote list'    => ['quote' => ['x'], 'name' => 'x'],
            'no name'       => ['quote' => 'x'],
            'empty name'    => ['quote' => 'x', 'name' => ''],
            'role list'     => ['quote' => 'x', 'name' => 'y', 'role' => ['r']],
            'empty role'    => ['quote' => 'x', 'name' => 'y', 'role' => ''],
            'picture text'  => ['quote' => 'x', 'name' => 'y', 'attachment_id' => '5'],
            'picture null'  => ['quote' => 'x', 'name' => 'y', 'attachment_id' => null],
            'picture zero'  => ['quote' => 'x', 'name' => 'y', 'attachment_id' => 0],
        ];
        $fine = ['quote' => 'fine', 'name' => 'Fine'];
        foreach ($bad as $name => $item) {
            foreach ([[$item, $fine], [$fine, $item]] as $position => $items) {
                try {
                    self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node($items), 'ground');
                    $this->fail($name . ' was compiled at ' . $position);
                } catch (\InvalidArgumentException $e) {
                    $this->assertNotSame('', $e->getMessage(), $name);
                }
            }
        }
    }

    public function test_a_testimonial_picture_is_an_image_the_site_resolved(): void
    {
        $media = self::media();
        $cases = [
            'unresolved'  => [9, $media],
            'not image'   => [6, array_replace($media, [6 => array_replace($media[6], ['mime' => 'application/pdf'])])],
            'bad address' => [7, array_replace($media, [7 => array_replace($media[7], ['url' => 'javascript:alert(1)'])])],
            'other id'    => [8, array_replace($media, [8 => array_replace($media[8], ['id' => 80])])],
        ];
        foreach ($cases as $name => [$id, $facts]) {
            try {
                self::patterns(self::STOCK_BREAKPOINTS, $facts)->testimonials(new IdSeed(self::REQUEST), self::AT, self::node([
                    ['quote' => 'x', 'name' => 'y', 'attachment_id' => 5],
                    ['quote' => 'x', 'name' => 'y', 'attachment_id' => $id],
                ]), 'ground');
                $this->fail($name . ' was compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage(), $name);
            }
        }
    }

    public function test_testimonials_on_a_site_without_breakpoints_write_the_desktop_values_only(): void
    {
        $tree = self::patterns(null)->testimonials(new IdSeed(self::REQUEST), self::AT, self::node(self::items(3)), 'ground');

        $this->assertSame(['content_width', 'flex_direction', 'flex_gap', '_title'], array_keys($tree['settings']));
        $row = $tree['elements'][1];
        $this->assertSame(['flex_direction', 'flex_gap'], array_keys($row['settings']));
        $this->assertSame(['content_width', 'width'], array_keys($row['elements'][0]['settings']));
        $this->assertAllowed([$tree]);
    }

    public function test_the_compile_entry_builds_testimonials(): void
    {
        $patterns = self::patterns();
        $node     = self::node(self::items(4));

        $this->assertSame(
            self::bytes($patterns->testimonials(new IdSeed(self::REQUEST), self::AT, $node, 'inverse')),
            self::bytes($patterns->compile(new IdSeed(self::REQUEST), self::AT, $node, 'inverse'))
        );
    }

    public function test_every_testimonial_key_this_compiler_lists_is_one_a_testimonial_writes(): void
    {
        $tree    = self::patterns()->testimonials(new IdSeed(self::REQUEST), self::AT, self::node(self::items(6)), 'ground');
        $written = [];
        foreach (self::widgets($tree, 'testimonial') as $widget) {
            foreach (array_keys($widget['settings']) as $key) {
                $written[$key] = true;
            }
        }

        $this->assertArrayHasKey('testimonial', ElementorPatternCompiler::KEYS);
        foreach (array_keys(ElementorPatternCompiler::KEYS['testimonial']) as $key) {
            $this->assertArrayHasKey($key, $written, $key . ' is listed and no testimonial writes it');
        }
        $this->assertSame(self::FULL_KEYS, array_keys(ElementorPatternCompiler::KEYS['testimonial']));
        $this->assertSame('image', ElementorPatternCompiler::allowedKeys([])['testimonial']['testimonial_image']);
    }
}
