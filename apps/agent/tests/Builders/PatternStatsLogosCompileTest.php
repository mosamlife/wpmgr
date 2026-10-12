<?php
/**
 * The stats and logos patterns of a designed page: counters in a row of equal
 * columns, and logos as library images in a wrapping row. The counter and the
 * image are compiled as the frozen round-trip probes store them; the rows are
 * the probes' columns row and the wrapping row's layout keys.
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
use WPMgr\Agent\Abilities\Builders\OutlineV3Schema;
use Yoast\PHPUnitPolyfills\TestCases\TestCase;

/**
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorPatternCompiler
 * @covers \WPMgr\Agent\Abilities\Builders\ElementorSectionCompiler
 */
final class PatternStatsLogosCompileTest extends TestCase
{
    private const STOCK_BREAKPOINTS = ['mobile', 'tablet'];

    private const REQUEST = 'b7e2a4c1-0000-4000-8000-000000000012';

    private const PROBE_URL = 'https://example.com/wp-content/uploads/2026/10/van-1024x640.png';

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

    /**
     * Resolved image facts: attachment 5 is the pattern-image probe's picture.
     *
     * @return array<int,array<string,mixed>>
     */
    private static function media(): array
    {
        $media = [];
        foreach ([5, 6, 7, 8] as $id) {
            $media[$id] = [
                'id'           => $id,
                'url'          => $id === 5 ? self::PROBE_URL : 'https://example.com/wp-content/uploads/2026/10/logo-' . $id . '-300x120.png',
                'filename'     => 'logo-' . $id . '.png',
                'mime'         => 'image/png',
                'width'        => 300,
                'height'       => 120,
                'modified_gmt' => '2026-10-01 09:00:00',
                'alt'          => 'Logo ' . $id,
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

    /** @param list<array<string,mixed>> $stats Items. */
    private static function statsNode(array $stats): array
    {
        return ['type' => 'pattern', 'pattern' => 'stats', 'variant' => 'counters', 'stats' => $stats];
    }

    /** @return list<array<string,mixed>> */
    private static function statItems(int $count): array
    {
        $all = [
            ['value' => 1200, 'suffix' => '+', 'label' => 'Cups served'],
            ['value' => 98, 'suffix' => '%', 'label' => 'would recommend us'],
            ['value' => 24, 'suffix' => 'h', 'label' => 'average reply time'],
            ['value' => 15, 'label' => 'years in business'],
        ];

        return array_slice($all, 0, $count);
    }

    /** @param list<int> $ids Attachment ids. */
    private static function logosNode(array $ids): array
    {
        $logos = [];
        foreach ($ids as $id) {
            $logos[] = ['attachment_id' => $id, 'alt' => 'Logo ' . $id];
        }

        return ['type' => 'pattern', 'pattern' => 'logos', 'variant' => 'row', 'logos' => $logos];
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

    // -------------------------------------------------------------------------
    // pattern-stats
    // -------------------------------------------------------------------------

    public function test_pattern_stats(): void
    {
        $row   = self::patterns()->stats(new IdSeed(self::REQUEST), self::AT, self::statsNode(self::statItems(3)), 'ground');
        $probe = RoundTripProbes::tree('pattern-counter')[0]['elements'][0];
        $rowProbe = RoundTripProbes::tree('layout-columns-row')[0]['elements'][0];

        // The row is the columns row's, labelled; its columns are the columns row's, equal in width.
        $this->assertSame(array_keys($rowProbe['settings']), array_slice(array_keys($row['settings']), 0, count($rowProbe['settings'])));
        $this->assertSame('_title', array_key_last($row['settings']));
        $this->assertSame("WPMgr \u{00B7} Stats counters", $row['settings']['_title']);
        $this->assertSame(['id', 'elType', 'settings', 'elements', 'isInner'], array_keys($row));
        $this->assertTrue($row['isInner']);
        $this->assertCount(3, $row['elements']);
        foreach ($row['elements'] as $column) {
            $this->assertSame(['content_width' => 'full', 'width' => ['unit' => '%', 'size' => '33'], 'width_mobile' => ['unit' => '%', 'size' => '100']], $column['settings']);
            $this->assertCount(1, $column['elements']);
        }

        // The first counter is the counter probe's, in the probe's key order, with the tone's colours.
        $counter = $row['elements'][0]['elements'][0];
        $this->assertSame(array_keys($probe), array_keys($counter));
        $this->assertSame(array_keys($probe['settings']), array_keys($counter['settings']));
        foreach (['starting_number', 'ending_number', 'suffix', 'title'] as $key) {
            $this->assertSame($probe['settings'][$key], $counter['settings'][$key], $key);
        }
        $this->assertSame('counter', $counter['widgetType']);
        $this->assertSame('widget', $counter['elType']);
        $this->assertSame([], $counter['elements']);
        $this->assertSame(self::tokens()->tone('ground')['heading'], $counter['settings']['number_color']);
        $this->assertSame(self::tokens()->tone('ground')['accent'], $counter['settings']['title_color']);

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
    public function test_a_counter_is_drawn_in_the_colours_of_its_tone(string $tone): void
    {
        $row     = self::patterns()->stats(new IdSeed(self::REQUEST), self::AT, self::statsNode(self::statItems(2)), $tone);
        $colours = self::tokens()->tone($tone);

        foreach ($row['elements'] as $column) {
            $settings = $column['elements'][0]['settings'];
            $this->assertSame($colours['heading'], $settings['number_color']);
            $this->assertSame($colours['accent'], $settings['title_color']);
        }
    }

    /**
     * @return array<string,array{0:int,1:string}>
     */
    public static function counts(): array
    {
        return ['two' => [2, '50'], 'three' => [3, '33'], 'four' => [4, '25']];
    }

    /** @dataProvider counts */
    public function test_the_columns_of_a_stats_row_share_it_equally(int $count, string $width): void
    {
        $row = self::patterns()->stats(new IdSeed(self::REQUEST), self::AT, self::statsNode(self::statItems($count)), 'surface');

        $this->assertSame(array_fill(0, $count, $width), array_map(static fn (array $c): string => $c['settings']['width']['size'], $row['elements']));
    }

    public function test_a_counter_without_a_suffix_writes_none_and_its_text_is_escaped(): void
    {
        $row = self::patterns()->stats(new IdSeed(self::REQUEST), self::AT, self::statsNode([
            ['value' => 15, 'label' => 'R&D hours'],
            ['value' => 0, 'suffix' => '&', 'label' => 'bugs [12]'],
        ]), 'ground');

        $first  = $row['elements'][0]['elements'][0]['settings'];
        $second = $row['elements'][1]['elements'][0]['settings'];
        $this->assertSame(['starting_number', 'ending_number', 'title', 'number_color', 'title_color'], array_keys($first));
        $this->assertSame('R&amp;D hours', $first['title']);
        $this->assertSame('0', $second['ending_number']);
        $this->assertSame('&amp;', $second['suffix']);
        $this->assertSame('bugs &#091;12&#093;', $second['title']);
        $this->assertAllowed([$row]);
    }

    public function test_every_counter_and_column_has_an_id_of_its_own(): void
    {
        $row = self::patterns()->stats(new IdSeed(self::REQUEST), self::AT, self::statsNode(self::statItems(4)), 'ground');
        $ids = [$row['id']];
        foreach ($row['elements'] as $column) {
            $ids[] = $column['id'];
            $ids[] = $column['elements'][0]['id'];
        }

        $this->assertCount(9, array_unique($ids));
        foreach ($ids as $id) {
            $this->assertMatchesRegularExpression('/^[0-9a-f]{7}\z/', $id);
        }
    }

    public function test_a_stats_pattern_outside_two_to_four_items_is_not_compiled(): void
    {
        foreach ([0, 1, 5] as $count) {
            $items = $count === 5 ? array_merge(self::statItems(4), [['value' => 1, 'label' => 'one more']]) : self::statItems($count);
            try {
                self::patterns()->stats(new IdSeed(self::REQUEST), self::AT, self::statsNode($items), 'ground');
                $this->fail($count . ' items were compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
    }

    public function test_a_counter_the_grammar_would_have_refused_is_not_compiled(): void
    {
        $bad = [
            'negative'    => ['value' => -1, 'label' => 'x'],
            'a fraction'  => ['value' => 1.5, 'label' => 'x'],
            'too large'   => ['value' => OutlineV3Schema::MAX_STAT_VALUE + 1, 'label' => 'x'],
            'no label'    => ['value' => 1],
            'empty label' => ['value' => 1, 'label' => ''],
            'suffix list' => ['value' => 1, 'label' => 'x', 'suffix' => ['%']],
        ];
        foreach ($bad as $name => $item) {
            try {
                self::patterns()->stats(new IdSeed(self::REQUEST), self::AT, self::statsNode([$item, ['value' => 2, 'label' => 'y']]), 'ground');
                $this->fail($name . ' was compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage(), $name);
            }
        }
    }

    // -------------------------------------------------------------------------
    // pattern-logos
    // -------------------------------------------------------------------------

    public function test_pattern_logos(): void
    {
        $row   = self::patterns()->logos(new IdSeed(self::REQUEST), self::AT, self::logosNode([5, 6, 7, 8]));
        $probe = RoundTripProbes::tree('pattern-image')[0]['elements'][0];

        // A wrapping row, centred, with the design's row gap at each step, never stacked on a phone.
        $this->assertSame(
            ['content_width', 'flex_direction', 'flex_wrap', 'flex_gap', 'flex_gap_tablet', 'flex_gap_mobile', 'flex_justify_content', 'flex_align_items', '_title'],
            array_keys($row['settings'])
        );
        $this->assertSame('full', $row['settings']['content_width']);
        $this->assertSame('row', $row['settings']['flex_direction']);
        $this->assertSame('wrap', $row['settings']['flex_wrap']);
        $this->assertSame(['column' => '32', 'row' => '32', 'isLinked' => true, 'unit' => 'px'], $row['settings']['flex_gap']);
        $this->assertSame(['column' => '24', 'row' => '24', 'isLinked' => true, 'unit' => 'px'], $row['settings']['flex_gap_tablet']);
        $this->assertSame(['column' => '20', 'row' => '20', 'isLinked' => true, 'unit' => 'px'], $row['settings']['flex_gap_mobile']);
        $this->assertSame('center', $row['settings']['flex_justify_content']);
        $this->assertSame('center', $row['settings']['flex_align_items']);
        $this->assertSame("WPMgr \u{00B7} Logos row", $row['settings']['_title']);
        $this->assertSame(['id', 'elType', 'settings', 'elements', 'isInner'], array_keys($row));
        $this->assertTrue($row['isInner']);

        // Each logo is the image probe's widget: the library picture, medium, centred, 140 px wide.
        $this->assertCount(4, $row['elements']);
        $first = $row['elements'][0];
        $this->assertSame(self::bytes($probe['settings']), self::bytes($first['settings']));
        $this->assertSame(array_keys($probe), array_keys($first));
        $this->assertSame('image', $first['widgetType']);
        foreach ($row['elements'] as $k => $logo) {
            $this->assertSame((string) [5, 6, 7, 8][$k], $logo['settings']['image']['id']);
            $this->assertSame('library', $logo['settings']['image']['source']);
        }

        $this->assertAllowed([$row]);
    }

    public function test_a_logos_row_on_a_site_without_breakpoints_writes_the_desktop_gap_only(): void
    {
        $row = self::patterns(null)->logos(new IdSeed(self::REQUEST), self::AT, self::logosNode([5, 6, 7]));

        $this->assertSame(['content_width', 'flex_direction', 'flex_wrap', 'flex_gap', 'flex_justify_content', 'flex_align_items', '_title'], array_keys($row['settings']));
        $this->assertAllowed([$row]);
    }

    public function test_a_logo_is_an_image_the_site_resolved(): void
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
                self::patterns(self::STOCK_BREAKPOINTS, $facts)->logos(new IdSeed(self::REQUEST), self::AT, self::logosNode([5, $id, 5]));
                $this->fail($name . ' was compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage(), $name);
            }
        }
    }

    public function test_a_logos_pattern_outside_three_to_ten_items_is_not_compiled(): void
    {
        foreach ([[], [5], [5, 6], [5, 6, 7, 8, 5, 6, 7, 8, 5, 6, 7]] as $ids) {
            try {
                self::patterns()->logos(new IdSeed(self::REQUEST), self::AT, self::logosNode($ids));
                $this->fail(count($ids) . ' logos were compiled');
            } catch (\InvalidArgumentException $e) {
                $this->assertNotSame('', $e->getMessage());
            }
        }
        $this->assertCount(10, self::patterns()->logos(new IdSeed(self::REQUEST), self::AT, self::logosNode([5, 6, 7, 8, 5, 6, 7, 8, 5, 6]))['elements']);
    }

    // -------------------------------------------------------------------------
    // The alt text of a logo
    // -------------------------------------------------------------------------

    /** @return list<array<string,mixed>> */
    private static function outlineWith(array $logos): array
    {
        return [
            ['type' => 'section', 'anchor' => 'top', 'children' => [['type' => 'paragraph', 'text' => 'x']]],
            ['type' => 'section', 'anchor' => 'clients', 'children' => [['type' => 'pattern', 'pattern' => 'logos', 'variant' => 'row', 'logos' => $logos]]],
        ];
    }

    public function test_a_logo_alt_that_differs_from_the_library_is_refused_with_its_path(): void
    {
        $outline = self::outlineWith([
            ['attachment_id' => 5, 'alt' => 'Logo 5'],
            ['attachment_id' => 6, 'alt' => 'The sixth'],
            ['attachment_id' => 7, 'alt' => 'Logo 7'],
        ]);
        $refusal = ElementorPatternCompiler::logoAltRefusal($outline, self::media());

        $this->assertSame('image_alt_from_library', $refusal['code']);
        $this->assertCount(1, $refusal['failures']);
        $this->assertSame('outline[1].children[0].logos[1]', $refusal['failures'][0]['path']);
        $this->assertSame('alt', $refusal['failures'][0]['key']);
        $this->assertStringNotContainsString('The sixth', json_encode($refusal, JSON_THROW_ON_ERROR), 'the input text is not echoed');
    }

    public function test_logos_whose_alt_is_the_library_alt_are_not_refused(): void
    {
        $this->assertNull(ElementorPatternCompiler::logoAltRefusal(self::outlineWith([['attachment_id' => 5, 'alt' => 'Logo 5']]), self::media()));
        $this->assertNull(ElementorPatternCompiler::logoAltRefusal(self::outlineWith([['attachment_id' => 9, 'alt' => 'unresolved']]), self::media()), 'an unresolved image is the resolution\'s refusal');
        $this->assertNull(ElementorPatternCompiler::logoAltRefusal('not a list', self::media()));
        $this->assertNull(ElementorPatternCompiler::logoAltRefusal([['type' => 'section', 'children' => 7], 'x'], self::media()));
    }

    // -------------------------------------------------------------------------
    // The entry, the keys and the gap
    // -------------------------------------------------------------------------

    public function test_the_compile_entry_builds_stats_and_logos(): void
    {
        $patterns = self::patterns();
        $stats    = self::statsNode(self::statItems(3));
        $logos    = self::logosNode([5, 6, 7]);

        $this->assertSame(
            self::bytes($patterns->stats(new IdSeed(self::REQUEST), self::AT, $stats, 'surface')),
            self::bytes($patterns->compile(new IdSeed(self::REQUEST), self::AT, $stats, 'surface'))
        );
        $this->assertSame(
            self::bytes($patterns->logos(new IdSeed(self::REQUEST), self::AT, $logos)),
            self::bytes($patterns->compile(new IdSeed(self::REQUEST), self::AT, $logos, 'surface'))
        );
    }

    public function test_every_key_this_compiler_lists_is_one_a_pattern_writes(): void
    {
        $patterns = self::patterns();
        $trees    = [
            $patterns->stats(new IdSeed(self::REQUEST), self::AT, self::statsNode(self::statItems(3)), 'ground'),
            $patterns->logos(new IdSeed(self::REQUEST), self::AT, self::logosNode([5, 6, 7])),
        ];
        $written = [];
        $walk    = static function (array $element) use (&$walk, &$written): void {
            $type = $element['elType'] === 'widget' ? $element['widgetType'] : 'container';
            foreach (array_keys($element['settings']) as $key) {
                $written[$type][$key] = true;
            }
            foreach ($element['elements'] as $child) {
                $walk($child);
            }
        };
        foreach ($trees as $tree) {
            $walk($tree);
        }
        foreach (ElementorPatternCompiler::KEYS as $type => $paths) {
            foreach (array_keys($paths) as $key) {
                $this->assertArrayHasKey($key, $written[$type] ?? [], $type . '.' . $key . ' is listed and no pattern writes it');
            }
        }
        $this->assertArrayHasKey('counter', ElementorPatternCompiler::KEYS);
    }

    public function test_the_allowed_keys_widen_a_list_and_never_narrow_a_pin_in_it(): void
    {
        $allowed = ElementorPatternCompiler::allowedKeys(['container' => ['flex_wrap' => 'enum', 'content_width' => 'enum'], 'heading' => ['title' => 'html']]);

        $this->assertSame('enum', $allowed['container']['flex_wrap']);
        $this->assertSame('enum:center', $allowed['container']['flex_align_items']);
        $this->assertSame(['title' => 'html'], $allowed['heading']);
        $this->assertSame('colour', $allowed['counter']['number_color']);
    }

    public function test_the_section_compilers_gap_is_the_gap_of_its_rows(): void
    {
        $sections = new ElementorSectionCompiler(self::tokens(), self::STOCK_BREAKPOINTS);
        $gap      = $sections->gapSettings();
        $row      = $sections->row('abcdef1', [])['settings'];

        $this->assertSame(['flex_gap', 'flex_gap_tablet', 'flex_gap_mobile'], array_keys($gap));
        $this->assertSame($gap, array_intersect_key($row, $gap));
        $this->assertSame(['flex_gap'], array_keys((new ElementorSectionCompiler(self::tokens(), null))->gapSettings()));
    }
}
